package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/store"
)

// PollPendingConfirmations actively checks on-chain mining proofs for seller deals awaiting
// confirmation (essential for DisclosureSale escrows, which are not tracked in wallet.sqlite).
func (b *Broker) PollPendingConfirmations(ctx context.Context) {
	if b == nil || b.mem == nil || b.mem.Chain() == nil {
		return
	}
	b.mu.RLock()
	if len(b.pendingConfirmations) == 0 {
		b.mu.RUnlock()
		return
	}
	txids := make([]string, 0, len(b.pendingConfirmations))
	for txid := range b.pendingConfirmations {
		txids = append(txids, txid)
	}
	b.mu.RUnlock()
	for _, txid := range txids {
		if ctx.Err() != nil {
			return
		}
		mp, height, ok, err := b.mem.Chain().ProofChecked(ctx, txid)
		if err != nil || !ok || mp == nil {
			continue
		}
		if height == 0 {
			height = mp.BlockHeight
		}
		b.OnPaymentTxMined(ctx, txid, height, mp)
	}
}

// OnPaymentTxMined is wired to Monitor.OnTxMined (via Port.OnTxMined) so over-cap payments waiting
// in payment_pending automatically verify BUMP + settleDepth, settle SellerDeal, invoke Restate
// ConfirmSellerPayment, and deliver the encrypted reveal_key to Buyer via Hub bbox!
func (b *Broker) OnPaymentTxMined(ctx context.Context, txid string, blockHeight uint32, mp *transaction.MerklePath) {
	if b == nil || txid == "" || mp == nil {
		return
	}
	b.mu.RLock()
	entry := b.pendingConfirmations[txid]
	b.mu.RUnlock()
	if entry == nil && b.mem != nil && b.mem.DB() != nil {
		var restored pendingConfirmEntry
		if ok, err := b.mem.DB().GetJSON("pending_confirm:"+txid, &restored); err == nil && ok && restored.OfferID != "" {
			entry = &restored
		}
	}
	if entry == nil {
		return
	}
	// Verify MerklePath against witnessed headers & enforce RequiredDepth before revealing key!
	tracker := b.mem.Chain().Tracker()
	if b.mem.Chain().Network() != "memory" {
		if tracker == nil {
			return
		}
		txH, hErr := chainhash.NewHashFromHex(txid)
		if hErr != nil || txH == nil {
			return
		}
		root, rErr := mp.ComputeRoot(txH)
		if rErr != nil || root == nil {
			return
		}
		if valid, vErr := tracker.IsValidRootForHeight(ctx, root, mp.BlockHeight); vErr != nil || !valid {
			return
		}
		tip, tErr := tracker.CurrentHeight(ctx)
		if tErr != nil || tip < mp.BlockHeight {
			return
		}
		reqDepth := entry.RequiredDepth
		if reqDepth <= 0 {
			reqDepth = b.mem.Chain().SettleDepth()
		}
		if int(tip-mp.BlockHeight)+1 < reqDepth {
			// Persist verified BUMP + MinedHeight so OnTipAdvanced automatically settles and delivers
			// reveal_key as soon as tip reaches MinedHeight + reqDepth - 1 (no lost multi-block wake!).
			entry.MinedHeight = mp.BlockHeight
			entry.MerklePathHex = mp.Hex()
			b.mu.Lock()
			b.pendingConfirmations[txid] = entry
			b.mu.Unlock()
			if b.mem != nil && b.mem.DB() != nil {
				_ = b.mem.DB().SetJSON("pending_confirm:"+txid, entry)
			}
			return
		}
	}
	b.mem.Chain().ZeroConfWatch().MarkSettled(txid)

	// Load pendingSale (RAM or SQLite) to reveal key & fresh ProofBundle.
	b.mu.RLock()
	sale := b.pendingSales[entry.OfferID]
	b.mu.RUnlock()
	if sale == nil {
		if rec, dErr := b.mem.DB().GetPendingSale(entry.OfferID); dErr == nil {
			sale = b.pendingSaleFromRecord(ctx, rec)
		}
	}
	if sale == nil {
		return
	}
	if sale.Shard != "" && sale.Seq > 0 {
		if freshPB, vr, pErr := b.mem.Prove(ctx, sale.Shard, sale.Seq); pErr == nil && freshPB != nil && vr.OK {
			sale.ProofBundle = freshPB
		}
	}

	// Atomic on-chain DisclosureSale.Buy(k) claim for deferred-confirmation escrows:
	// MUST broadcast Buy(k) BEFORE marking settled or revealing ContentKeyHex off-chain so the
	// buyer cannot learn `k` while keeping the escrow refundable after RefundAfter!
	var claimTxID string
	var claimTxBEEFHex string
	if entry.IsEscrow {
		rawEscrow, dErr := hex.DecodeString(strings.TrimSpace(entry.EscrowBEEFHex))
		if dErr != nil || len(rawEscrow) == 0 {
			return
		}
		_, escrowTx, _, pErr := guard.ParseBEEF(rawEscrow, guard.DefaultBound)
		if pErr != nil || escrowTx == nil {
			return
		}
		if escrowTx.MerklePath == nil {
			escrowTx.MerklePath = mp
		}
		sellerPKH, sErr := hex.DecodeString(strings.TrimSpace(entry.EscrowSellerPKH))
		depPKH, depErr := hex.DecodeString(strings.TrimSpace(entry.DepositorPKH))
		hWrapped, hwErr := hex.DecodeString(strings.TrimSpace(sale.SaleOffer.Config.HWrapped))
		if sErr != nil || len(sellerPKH) != 20 || depErr != nil || len(depPKH) != 20 || hwErr != nil || len(hWrapped) != 32 {
			return
		}
		escrowParams := contracts.DisclosureSaleParams{
			SellerPKH:    sellerPKH,
			Price:        sale.SaleOffer.Config.Price,
			HWrapped:     hWrapped,
			RefundAfter:  entry.EscrowRefundAfter,
			MaxFee:       entry.EscrowMaxFee,
			DepositorPKH: depPKH,
		}
		buyFee := min(escrowParams.MaxFee, uint64(150))
		buyTx, bErr := contracts.BuildDisclosureSaleBuyTx(escrowTx, entry.EscrowVout, escrowParams, sale.RevealKey[:], buyFee)
		if bErr != nil {
			return // Fail closed: never mark settled or reveal ContentKeyHex without a valid Buy(k) claim
		}
		shouldSubmit := b.mem.Chain().Network() != "memory" || b.mem.Chain().Tx(txid) != nil
		if shouldSubmit {
			cTxID, sErr := b.mem.Chain().SubmitCovenantTx(ctx, buyTx)
			if sErr != nil {
				return // Keep pendingConfirmations[txid] intact for retry; never reveal ContentKeyHex before L1 claim!
			}
			claimTxID = cTxID
			_, _ = b.mem.Chain().AdoptReceived(ctx, buyTx)
		} else {
			claimTxID = buyTx.TxID().String()
		}
		if buyBEEF, bErr := chain.SafeAtomicBEEF(buyTx); bErr == nil && len(buyBEEF) > 0 {
			claimTxBEEFHex = hex.EncodeToString(buyBEEF)
		}
	}

	b.mu.Lock()
	sale.Settled = true
	delete(b.pendingConfirmations, txid)
	b.mu.Unlock()
	if b.mem != nil && b.mem.DB() != nil {
		// Clear persisted pending_confirm KV entry so settled txs are never re-loaded on restart.
		_ = b.mem.DB().SetJSON("pending_confirm:"+txid, pendingConfirmEntry{})
	}
	sealedClaimKey, sealErr := b.sealKeyAtRest(ctx, hex.EncodeToString(sale.RevealKey[:]))
	if sealErr != nil {
		log.Printf("market: seal confirmed claim key for offer %s failed: %v", sale.OfferID, sealErr)
		return
	}
	// STATE-4 fix: construct & sign seller's third-entry DealReceipt on confirmed settlement (matching HandlePurchase)!
	var sellerReceipt *DealReceipt
	var sellerReceiptJSON []byte
	if sellerSignPub := b.engine.salePubHex(); sellerSignPub != "" {
		dr := DealReceipt{
			Version:         1,
			DealID:          sale.WantID,
			OfferID:         sale.OfferID,
			BuyerPub:        entry.BuyerPub,
			SellerPub:       sellerSignPub,
			PriceSats:       sale.SaleOffer.Config.Price,
			OfferDigestHash: sha256CanonicalHex(sale.SaleOffer),
			PaymentTxID:     txid,
			KeyCommitment:   sale.SaleOffer.KeyCommitment,
			NMU:             sale.OfferNMU,
			SettledAt:       time.Now().Unix(),
		}
		if sale.ProofBundle != nil {
			dr.FragmentLeaf = sale.ProofBundle.Leaf
		}
		if sErr := SignDealReceiptSeller(ctx, sellerSignPub, b.engine.signSaleDigest, &dr); sErr == nil {
			sellerReceipt = &dr
			sellerReceiptJSON, _ = json.Marshal(dr)
		}
	}

	_ = b.mem.DB().MarkPendingSaleSettled(sale.OfferID)
	b.engine.persistDealRecord(store.DealRecord{
		DealID:          "sell-" + sale.OfferID,
		Role:            "seller",
		Status:          "settled",
		Shard:           sale.Shard,
		DealReceiptJSON: sellerReceiptJSON,
		PaymentTxID:     txid,
		ClaimKeyHex:     sealedClaimKey,
		ChirpURL:        sale.ChirpURL,
		CounterpartyPub: entry.BuyerPub,
		PriceSats:       sale.SaleOffer.Config.Price,
		IsHubSeller:     b.cfg.IsHub,
		ConfirmHeight:   int64(blockHeight),
	})
	if len(sellerReceiptJSON) > 0 {
		_ = RecordCountersigOwed(b.mem.DB(), "sell-"+sale.OfferID, entry.BuyerPub, txid, sale.SaleOffer.Config.Price, sellerReceiptJSON)
		if sellerReceipt != nil {
			_, _, _ = CommitDealAudit(ctx, b.mem, *sellerReceipt, false)
		}
	}
	b.notify("deal_settled", map[string]any{"role": "seller", "offerId": sale.OfferID, "txid": txid, "claimTxid": claimTxID, "sats": sale.SaleOffer.Config.Price, "confirmHeight": blockHeight})

	if rc := b.RestateClient(); rc != nil {
		oid, tid, mpHex, sats, bh := sale.OfferID, txid, mp.Hex(), entry.AmountSats, blockHeight
		b.spawnTracked(10*time.Second, func(sCtx context.Context) {
			_, _ = rc.ConfirmSellerPaymentAsync(sCtx, oid, PaymentSignal{
				TxID:          tid,
				AmountSats:    sats,
				Confirmed:     true,
				BlockHeight:   bh,
				MerklePathHex: mpHex,
			})
			_, _ = rc.EmitTxEvent(sCtx, tid, TxEventInput{
				Kind:          "mined",
				BlockHeight:   bh,
				MerklePathHex: mpHex,
				Detail:        "seller-confirm:" + oid,
			})
		})
	}

	// Push encrypted reveal_key via Hub bbox (office-bound across Leaf-Spine BRC-149 plane) so BuyerDeal wakes immediately upon confirmation!
	if b.disc != nil && entry.BuyerPub != "" {
		wireCT := sale.SaleOffer.CiphertextHex
		if len(wireCT) > bstack.InlineOfferMaxHexLen && sale.ChirpURL != "" {
			wireCT = ""
		}
		respPayload := &PurchaseResponse{
			OK:             true,
			Status:         "settled",
			OfferID:        sale.OfferID,
			SellerPub:      b.PubHex(),
			ContentKeyHex:  hex.EncodeToString(sale.RevealKey[:]),
			ClaimTxID:      claimTxID,
			ClaimTxBEEFHex: claimTxBEEFHex,
			CiphertextHex:  wireCT,
			ChirpURL:       sale.ChirpURL,
			ProofBundle:    sale.ProofBundle,
			DealReceipt:    sellerReceipt,
		}
		replyTS := time.Now().Unix()
		bodyRaw, _ := json.Marshal(respPayload)
		normOffice := bstack.NormalizeBboxOffice(entry.BboxOffice)
		replyAAD := bstack.BboxAAD(normOffice, b.PubHex(), entry.BuyerPub, entry.WantID, "reveal_key")
		if sealedReply, encErr := b.sealBboxPayloadUnderLimit(ctx, normOffice, "reveal_key", b.PubHex(), entry.BuyerPub, replyTS, replyAAD, bodyRaw); encErr == nil {
			hosts := b.peerBboxHosts(ctx, "", b.disc.ActiveHosts())
			for _, ph := range b.peerBboxHosts(ctx, entry.BuyerPub, nil) {
				found := false
				for _, eh := range hosts {
					if eh == ph {
						found = true
						break
					}
				}
				if !found {
					hosts = append(hosts, ph)
				}
			}
			_ = b.postOfficeBboxMessage(ctx, hosts, normOffice, entry.BuyerPub, entry.WantID, "reveal_key", replyTS, sealedReply)
		}
	}
}

// OnTipAdvanced re-checks any mined-but-awaiting-depth pending confirmations once the chain tip advances,
// settling and delivering reveal_key as soon as tip >= MinedHeight + RequiredDepth - 1.
func (b *Broker) OnTipAdvanced(ctx context.Context, tip uint32) {
	if b == nil || tip == 0 {
		return
	}
	b.mu.RLock()
	var ready []*pendingConfirmEntry
	for _, entry := range b.pendingConfirmations {
		if entry == nil || entry.MinedHeight == 0 || entry.MerklePathHex == "" || tip < entry.MinedHeight {
			continue
		}
		reqDepth := entry.RequiredDepth
		if reqDepth <= 0 && b.mem != nil && b.mem.Chain() != nil {
			reqDepth = b.mem.Chain().SettleDepth()
		}
		if reqDepth <= 0 {
			reqDepth = 1
		}
		if int(tip-entry.MinedHeight)+1 >= reqDepth {
			cp := *entry
			ready = append(ready, &cp)
		}
	}
	b.mu.RUnlock()
	for _, entry := range ready {
		if mp, err := transaction.NewMerklePathFromHex(entry.MerklePathHex); err == nil && mp != nil {
			b.OnPaymentTxMined(ctx, entry.TxID, entry.MinedHeight, mp)
		}
	}
}

// HandleReorg rolls back deals confirmed at or above forkHeight in SQLite, invalidates cached
// ProofBundles & pendingConfirmEntry mined heights, and notifies Restate Tx VOs.
func (b *Broker) HandleReorg(ctx context.Context, forkHeight uint32) {
	if b == nil || forkHeight == 0 || b.mem == nil || b.mem.DB() == nil {
		return
	}
	b.mu.Lock()
	for _, s := range b.pendingSales {
		if s != nil {
			s.ProofBundle = nil
		}
	}
	for txid, entry := range b.pendingConfirmations {
		if entry != nil && entry.MinedHeight >= forkHeight {
			entry.MinedHeight = 0
			entry.MerklePathHex = ""
			_ = b.mem.DB().SetJSON("pending_confirm:"+txid, entry)
		}
	}
	b.mu.Unlock()
	var affected []store.DealRecord
	if deals, err := b.mem.DB().ListDeals(500); err == nil {
		for _, d := range deals {
			if d.ConfirmHeight >= int64(forkHeight) && d.ConfirmHeight > 0 {
				affected = append(affected, d)
			}
		}
	}
	rolled, err := b.mem.DB().RollbackDealsFromHeight(int64(forkHeight))
	if err != nil || rolled == 0 {
		return
	}
	if rc := b.RestateClient(); rc != nil {
		for _, d := range affected {
			if d.PaymentTxID != "" {
				tid, did := d.PaymentTxID, d.DealID
				b.spawnTracked(10*time.Second, func(sCtx context.Context) {
					_, _ = rc.EmitTxEvent(sCtx, tid, TxEventInput{
						Kind:        "reorged",
						BlockHeight: forkHeight,
						Detail:      "deal-reorg:" + did,
					})
				})
			}
		}
	}
}
