package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/store"
)

// HandlePurchase validates a buyer's AtomicBEEF payment + BRC-105 replay claim and reveals the BRC-369 content key k + SPV ProofBundle.
// Called by BOTH direct HTTP POST /v1/market/purchase AND NAT bbox inbox delivery!
func (b *Broker) HandlePurchase(ctx context.Context, req PurchaseRequest) (*PurchaseResponse, error) {
	if req.OfferID == "" || req.PaymentBEEFHex == "" {
		return nil, errors.New("market: offerId and paymentBeefHex are required")
	}
	nowUnix := time.Now().Unix()
	b.mu.Lock()
	sale, ok := b.pendingSales[req.OfferID]
	if ok && sale.ExpiresAt > 0 && sale.ExpiresAt < nowUnix {
		delete(b.pendingSales, req.OfferID)
		ok = false
		sale = nil
	}
	b.mu.Unlock()
	if !ok {
		if rec, dErr := b.mem.DB().GetPendingSale(req.OfferID); dErr == nil {
			if restored := b.pendingSaleFromRecord(ctx, rec); restored != nil && (restored.ExpiresAt == 0 || restored.ExpiresAt >= nowUnix) {
				sale = restored
				ok = true
				b.mu.Lock()
				b.pendingSales[req.OfferID] = sale
				b.mu.Unlock()
			}
		}
	}
	if !ok {
		return nil, fmt.Errorf("market: unknown or expired offerId %q", req.OfferID)
	}
	if sale.BuyerPub != "" {
		if strings.TrimSpace(req.BuyerPub) == "" || sale.BuyerPub != req.BuyerPub {
			return nil, errors.New("market: buyerPub does not match offer recipient")
		}
	}
	if strings.TrimSpace(req.BuyerPub) == "" || strings.TrimSpace(req.BuyerSig) == "" {
		return nil, errors.New("market: signed buyer purchase request (buyerPub + buyerSig) is required")
	}
	if req.TS < nowUnix-300 || req.TS > nowUnix+300 {
		return nil, errors.New("market: expired or missing purchase request timestamp")
	}
	if err := bstack.VerifyArtifact(req.BuyerPub, req.BuyerSig, req.Signable()); err != nil {
		return nil, fmt.Errorf("market: invalid buyer purchase signature: %w", err)
	}
	beefBytes, err := hex.DecodeString(strings.TrimSpace(req.PaymentBEEFHex))
	if err != nil {
		return nil, fmt.Errorf("market: decode paymentBeefHex: %w", err)
	}
	_, tx, txHash, err := guard.ParseBEEF(beefBytes, guard.DefaultBound)
	if err != nil || tx == nil || txHash == nil {
		if err == nil {
			err = errors.New("empty payment transaction")
		}
		return nil, fmt.Errorf("market: parse payment AtomicBEEF: %w", err)
	}
	txid := txHash.String()
	if req.TxID != "" && req.TxID != txid {
		return nil, errors.New("market: payment txid mismatch vs AtomicBEEF")
	}

	// Idempotent key re-reveal on buyer retry for the EXACT same payment txid!
	if existing, gErr := b.mem.DB().GetDeal("sell-" + sale.OfferID); gErr == nil && existing.Status == "settled" {
		if existing.PaymentTxID == txid {
			keyHex := b.openKeyAtRest(ctx, existing.ClaimKeyHex)
			if keyHex == "" {
				keyHex = hex.EncodeToString(sale.RevealKey[:])
			}
			return &PurchaseResponse{
				OK:            true,
				Status:        "settled",
				OfferID:       sale.OfferID,
				SellerPub:     b.PubHex(),
				ContentKeyHex: keyHex,
				CiphertextHex: sale.SaleOffer.CiphertextHex,
				ChirpURL:      sale.ChirpURL,
				ProofBundle:   b.refreshPendingSaleProof(ctx, sale),
			}, nil
		}
		return nil, errors.New("market: offer already settled with a different payment txid")
	}
	// Idempotent awaiting_confirmation response if buyer retries while payment is pending settleDepth!
	if existing, gErr := b.mem.DB().GetDeal("sell-" + sale.OfferID); gErr == nil && existing.Status == "payment_pending" && existing.PaymentTxID == txid {
		return &PurchaseResponse{
			OK:            true,
			Status:        "awaiting_confirmation",
			RequiredDepth: b.mem.Chain().SettleDepth(),
			OfferID:       sale.OfferID,
			SellerPub:     b.PubHex(),
			ChirpURL:      sale.ChirpURL,
		}, nil
	}

	isEscrow, isBRC29, paidSats, escrowParams, err := b.verifyPurchasePaymentOutput(ctx, req, sale, tx, nowUnix)
	if err != nil {
		return nil, err
	}

	// Full consensus SPV verification (spv.Verify + REFUSED-BUMP + tracker outage separation + settleDepth +
	// per-deal & aggregate 0-conf caps + maxUnconfirmedAncestors + conflict watch) BEFORE replay claim or broadcast!
	zeroConfMax, zeroConfAggMax, maxAncestors := b.engine.Policy().ZeroConfLimits()
	settleDepth := b.mem.Chain().SettleDepth()
	confirmed := false
	needsConfirm := false
	var confirmHeight uint32
	if b.mem.Chain().Network() != "memory" {
		ver, vErr := chain.VerifyCounterpartyPaymentBEEF(ctx, beefBytes, b.mem.Chain().Tracker(), chain.PaymentVerifyOpts{
			ExpectedAmountSats:       sale.SaleOffer.Config.Price,
			MaxUnconfirmedAncestors:  maxAncestors,
			ZeroConfMaxSats:          zeroConfMax,
			ZeroConfMaxAggregateSats: zeroConfAggMax,
			SettleDepth:              settleDepth,
			AllowZeroConf:            zeroConfMax > 0,
			ConflictWatch:            b.mem.Chain().ZeroConfWatch(),
			TrackOnZeroConf:          true,
		})
		if vErr != nil {
			// Fail closed on forged BUMP, tracker outage, invalid script, coinbase maturity, or conflict!
			return nil, fmt.Errorf("market: payment SPV verification failed: %w", vErr)
		}
		confirmed = ver.SettledAtDepth
		needsConfirm = ver.NeedsConfirmation
		confirmHeight = ver.BlockHeight
	} else {
		if tx.MerklePath != nil {
			confirmed = true
			confirmHeight = tx.MerklePath.BlockHeight
		} else if zeroConfMax == 0 || sale.SaleOffer.Config.Price > zeroConfMax {
			needsConfirm = true
		}
	}

	// Atomic cross-interface SQLite payment claim registry (shared with HTTP 402 paywall)!
	inputs := make([]string, 0, len(tx.Inputs))
	for _, in := range tx.Inputs {
		if in.SourceTXID != nil {
			inputs = append(inputs, fmt.Sprintf("%s:%d", in.SourceTXID.String(), in.SourceTxOutIndex))
		}
	}
	if err := b.mem.DB().ClaimPaymentAtomic(txid, inputs); err != nil {
		return nil, fmt.Errorf("market: payment claim rejected (%w)", err)
	}

	// On live networks, credit Seller's wallet immediately without double-broadcasting or poisoning SpendGuard:
	// In embedded BRC-29 mode, InternalizeBRC29 runs verified SPV + WithIncomingInternalize broadcast once;
	// in escrow mode, relay unmined escrow tx before executing atomic DisclosureSale.Buy(k).
	if b.mem.Chain().Network() != "memory" {
		needRelay := !confirmed && (isEscrow || !isBRC29 || b.mem.Chain().WalletMode() != "embedded")
		if needRelay {
			if rErr := b.mem.Chain().RelayExternalTx(ctx, tx); rErr != nil {
				_ = b.mem.DB().ReleasePaymentClaim(txid)
				return nil, fmt.Errorf("market: broadcast buyer payment failed: %w", rErr)
			}
		}
		if isBRC29 {
			if _, iErr := b.mem.Chain().InternalizeBRC29(
				ctx, beefBytes, req.OutputIndex,
				req.DerivationPrefix, req.DerivationSuffix, req.SenderIdentityKey,
				"market sale "+sale.OfferID, []string{"market", "sale"},
			); iErr != nil {
				_ = b.mem.DB().ReleasePaymentClaim(txid)
				return nil, fmt.Errorf("market: internalize BRC-29 payment: %w", iErr)
			}
		}
	}

	// Atomic on-chain DisclosureSale.Buy(k) claim: Seller reveals the 32-byte BRC-369 content key `k`
	// directly in Input 0's unlocking script on BSV L1 to claim `Price` sats!
	var claimTxID string
	var claimTxBEEFHex string
	if isEscrow && !needsConfirm {
		buyFee := min(escrowParams.MaxFee, uint64(300))
		buyTx, bErr := contracts.BuildDisclosureSaleBuyTx(tx, req.OutputIndex, escrowParams, sale.RevealKey[:], buyFee)
		if bErr != nil {
			_ = b.mem.DB().ReleasePaymentClaim(txid)
			return nil, fmt.Errorf("market: build DisclosureSale.Buy tx: %w", bErr)
		}
		// On live networks, ALWAYS broadcast Buy(k) to chain (fail-closed).
		// On memory testchain, submit whenever the escrow parent tx exists on this node's testchain
		// (isolated two-node unit tests keep separate in-memory testchains; Chronicle VM already verified buyTx above).
		shouldSubmit := b.mem.Chain().Network() != "memory" || b.mem.Chain().Tx(txid) != nil
		if shouldSubmit {
			cTxID, sErr := b.mem.Chain().SubmitCovenantTx(ctx, buyTx)
			if sErr != nil {
				_ = b.mem.DB().ReleasePaymentClaim(txid)
				return nil, fmt.Errorf("market: broadcast DisclosureSale.Buy tx: %w", sErr)
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

	// Over-cap / below-settleDepth escalation seam (Finding #24): valid SPV payment is internalized,
	// ContentKeyHex is withheld, SellerDeal enters payment_pending, and OnPaymentTxMined completes reveal at settleDepth!
	if needsConfirm {
		entry := &pendingConfirmEntry{
			OfferID:       sale.OfferID,
			WantID:        sale.WantID,
			BuyerPub:      req.BuyerPub,
			TxID:          txid,
			AmountSats:    paidSats,
			RequiredDepth: settleDepth,
			BboxOffice:    b.cfg.BboxOffice,
			CreatedAt:     time.Now().Unix(),
		}
		if isEscrow {
			entry.IsEscrow = true
			entry.EscrowVout = req.OutputIndex
			entry.EscrowBEEFHex = strings.TrimSpace(req.PaymentBEEFHex)
			entry.EscrowSellerPKH = hex.EncodeToString(escrowParams.SellerPKH)
			entry.DepositorPKH = hex.EncodeToString(escrowParams.DepositorPKH)
			entry.EscrowRefundAfter = escrowParams.RefundAfter
			entry.EscrowMaxFee = escrowParams.MaxFee
		}
		b.mu.Lock()
		b.pendingConfirmations[txid] = entry
		b.mu.Unlock()
		_ = b.mem.DB().SetJSON("pending_confirm:"+txid, entry)
		sealedPendingKey, sealErr := b.sealKeyAtRest(ctx, hex.EncodeToString(sale.RevealKey[:]))
		if sealErr != nil {
			return nil, sealErr
		}
		b.engine.persistDealRecord(store.DealRecord{
			DealID:          "sell-" + sale.OfferID,
			Role:            "seller",
			Status:          "payment_pending",
			Shard:           sale.Shard,
			PaymentTxID:     txid,
			PaymentBEEF:     beefBytes,
			ClaimKeyHex:     sealedPendingKey,
			ChirpURL:        sale.ChirpURL,
			CounterpartyPub: req.BuyerPub,
			PriceSats:       sale.SaleOffer.Config.Price,
			IsHubSeller:     b.cfg.IsHub,
		})
		if rc := b.RestateClient(); rc != nil {
			oid, tid, sh, sats := sale.OfferID, txid, sale.Shard, paidSats
			b.spawnTracked(10*time.Second, func(sCtx context.Context) {
				_, _ = rc.DeliverSellerPaymentAsync(sCtx, oid, PaymentSignal{TxID: tid, AmountSats: sats, Confirmed: false})
				_, _ = rc.EmitTxEvent(sCtx, tid, TxEventInput{Kind: "broadcast", Shard: sh, Detail: "seller-sale-pending:" + oid})
			})
		}
		return &PurchaseResponse{
			OK:            true,
			Status:        "awaiting_confirmation",
			RequiredDepth: settleDepth,
			OfferID:       sale.OfferID,
			SellerPub:     b.PubHex(),
			ChirpURL:      sale.ChirpURL,
		}, nil
	}

	freshPB := b.refreshPendingSaleProof(ctx, sale)
	// Mark settled & persist durable Seller DealRecord!
	b.mu.Lock()
	sale.Settled = true
	b.mu.Unlock()
	// Build seller-signed third-entry DealReceipt using market saleKey (matching OfferAd.Pub and req.BuyerPub)
	// so Buyer can countersign bilaterally without mutating BuyerPub or invalidating SellerSig!
	var sellerReceipt *DealReceipt
	var sellerReceiptJSON []byte
	if sellerSignPub := b.engine.salePubHex(); sellerSignPub != "" {
		dr := DealReceipt{
			Version:            1,
			DealID:             sale.WantID,
			OfferID:            sale.OfferID,
			BuyerPub:           req.BuyerPub,
			SellerPub:          sellerSignPub,
			PriceSats:          sale.SaleOffer.Config.Price,
			OfferDigestHash:    sha256CanonicalHex(sale.SaleOffer),
			PaymentTxID:        txid,
			PaymentOutputIndex: req.OutputIndex,
			KeyCommitment:      sale.SaleOffer.KeyCommitment,
			NMU:                sale.OfferNMU,
			SettledAt:          time.Now().Unix(),
		}
		if freshPB != nil {
			dr.FragmentLeaf = freshPB.Leaf
		}
		if sErr := SignDealReceiptSeller(ctx, sellerSignPub, b.engine.signSaleDigest, &dr); sErr == nil {
			sellerReceipt = &dr
			sellerReceiptJSON, _ = json.Marshal(dr)
		}
	}

	sealedClaimKey, sealErr := b.sealKeyAtRest(ctx, hex.EncodeToString(sale.RevealKey[:]))
	if sealErr != nil {
		return nil, sealErr
	}
	_ = b.mem.DB().MarkPendingSaleSettled(sale.OfferID)
	b.engine.persistDealRecord(store.DealRecord{
		DealID:          "sell-" + sale.OfferID,
		Role:            "seller",
		Status:          "settled",
		Shard:           sale.Shard,
		DealReceiptJSON: sellerReceiptJSON,
		PaymentTxID:     txid,
		PaymentBEEF:     beefBytes,
		ClaimKeyHex:     sealedClaimKey,
		ChirpURL:        sale.ChirpURL,
		CounterpartyPub: req.BuyerPub,
		PriceSats:       sale.SaleOffer.Config.Price,
		IsHubSeller:     b.cfg.IsHub,
		ConfirmHeight:   int64(confirmHeight),
	})
	if len(sellerReceiptJSON) > 0 {
		_ = RecordCountersigOwed(b.mem.DB(), "sell-"+sale.OfferID, req.BuyerPub, txid, sale.SaleOffer.Config.Price, sellerReceiptJSON)
		if sellerReceipt != nil {
			_, _, _ = CommitDealAudit(ctx, b.mem, *sellerReceipt, false)
		}
	}
	if !confirmed && txid != "" {
		_ = RecordProofUpgradeOwed(b.mem.DB(), "sell-"+sale.OfferID, "seller", req.BuyerPub, txid, sale.SaleOffer.Config.Price)
	}
	b.notify("deal_settled", map[string]any{"role": "seller", "offerId": sale.OfferID, "txid": txid, "sats": sale.SaleOffer.Config.Price})

	// Notify active Restate SellerDeal workflow + Tx lifecycle journal asynchronously!
	var mpHex string
	if tx.MerklePath != nil {
		mpHex = tx.MerklePath.Hex()
	}
	if rc := b.RestateClient(); rc != nil {
		oid, tid, sh, mp, sats, conf, bh := sale.OfferID, txid, sale.Shard, mpHex, paidSats, confirmed, confirmHeight
		b.spawnTracked(10*time.Second, func(sCtx context.Context) {
			_, _ = rc.DeliverSellerPaymentAsync(sCtx, oid, PaymentSignal{TxID: tid, AmountSats: sats, Confirmed: conf, BlockHeight: bh, MerklePathHex: mp})
			_, _ = rc.EmitTxEvent(sCtx, tid, TxEventInput{Kind: "broadcast", Shard: sh, Detail: "seller-sale:" + oid})
		})
	}

	return &PurchaseResponse{
		OK:             true,
		Status:         "settled",
		OfferID:        sale.OfferID,
		SellerPub:      b.PubHex(),
		ContentKeyHex:  hex.EncodeToString(sale.RevealKey[:]),
		ClaimTxID:      claimTxID,
		ClaimTxBEEFHex: claimTxBEEFHex,
		CiphertextHex:  sale.SaleOffer.CiphertextHex,
		ChirpURL:       sale.ChirpURL,
		ProofBundle:    freshPB,
		DealReceipt:    sellerReceipt,
	}, nil
}
