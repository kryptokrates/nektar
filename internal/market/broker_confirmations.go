package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/store"
)

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
			return // wait for more confirmations before releasing reveal key
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
	b.mu.Lock()
	sale.Settled = true
	delete(b.pendingConfirmations, txid)
	b.mu.Unlock()
	_ = b.mem.DB().MarkPendingSaleSettled(sale.OfferID)
	_ = b.mem.DB().SaveDeal(store.DealRecord{
		DealID:          "sell-" + sale.OfferID,
		Role:            "seller",
		Status:          "settled",
		Shard:           sale.Shard,
		PaymentTxID:     txid,
		ClaimKeyHex:     b.sealKeyAtRest(ctx, hex.EncodeToString(sale.RevealKey[:])),
		ChirpURL:        sale.ChirpURL,
		CounterpartyPub: entry.BuyerPub,
		PriceSats:       sale.SaleOffer.Config.Price,
		IsHubSeller:     b.cfg.IsHub,
		ConfirmHeight:   int64(blockHeight),
	})
	b.notify("deal_settled", map[string]any{"role": "seller", "offerId": sale.OfferID, "txid": txid, "sats": sale.SaleOffer.Config.Price, "confirmHeight": blockHeight})

	if rc := b.RestateClient(); rc != nil {
		go func(oid, tid, mpHex string, sats uint64, bh uint32) {
			sCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = rc.ConfirmSellerPayment(sCtx, oid, PaymentSignal{
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
		}(sale.OfferID, txid, mp.Hex(), entry.AmountSats, blockHeight)
	}

	// Push encrypted reveal_key via Hub bbox so BuyerDeal wakes immediately upon confirmation!
	if b.disc != nil && entry.BuyerPub != "" {
		respPayload := &PurchaseResponse{
			OK:            true,
			Status:        "settled",
			OfferID:       sale.OfferID,
			SellerPub:     b.PubHex(),
			ContentKeyHex: hex.EncodeToString(sale.RevealKey[:]),
			CiphertextHex: sale.SaleOffer.CiphertextHex,
			ChirpURL:      sale.ChirpURL,
			ProofBundle:   sale.ProofBundle,
		}
		bodyRaw, _ := json.Marshal(respPayload)
		office := entry.BboxOffice
		if office == "" {
			office = "default"
		}
		replyAAD := bstack.BboxAAD(office, b.PubHex(), entry.BuyerPub, entry.WantID, "reveal_key")
		if sealedReply, encErr := bstack.SealBboxBody(entry.BuyerPub, replyAAD, bodyRaw); encErr == nil {
			for _, h := range b.disc.ActiveHosts() {
				replyMsg := hub.BboxMessage{
					Office:       office,
					RecipientPub: entry.BuyerPub,
					SenderPub:    b.PubHex(),
					DealID:       entry.WantID,
					Kind:         "reveal_key",
					TS:           time.Now().Unix(),
					Nonce:        randNonceHex(),
					Aud:          hubAudience(h),
					Body:         sealedReply,
				}
				if sig, sErr := bstack.SignArtifact(b.engine.saleKey, replyMsg.Signable()); sErr == nil {
					replyMsg.Sig = sig
				}
				env, _ := json.Marshal(replyMsg)
				if resp, dErr := b.disc.PostJSONWithHub402(ctx, strings.TrimRight(h, "/")+"/bbox/send", "bbox:send", env); dErr == nil {
					resp.Body.Close()
					if resp.StatusCode < 300 {
						break
					}
				}
			}
		}
	}
}

// HandleReorg rolls back deals confirmed at or above forkHeight in SQLite and notifies Restate Tx VOs.
func (b *Broker) HandleReorg(ctx context.Context, forkHeight uint32) {
	if b == nil || forkHeight == 0 || b.mem == nil || b.mem.DB() == nil {
		return
	}
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
				go func(tid, did string) {
					sCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					_, _ = rc.EmitTxEvent(sCtx, tid, TxEventInput{
						Kind:        "reorged",
						BlockHeight: forkHeight,
						Detail:      "deal-reorg:" + did,
					})
				}(d.PaymentTxID, d.DealID)
			}
		}
	}
}
