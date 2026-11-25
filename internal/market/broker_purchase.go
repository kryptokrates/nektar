package market

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lightwebinc/bcommon/bwallet"
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

	// Refresh ProofBundle from memory store so buyer receives the freshest mined BUMP (not a stale quote snapshot).
	refreshProof := func(s *pendingSale) *contracts.ProofBundle {
		if s == nil {
			return nil
		}
		if s.Shard != "" && s.Seq > 0 {
			if freshPB, vr, pErr := b.mem.Prove(ctx, s.Shard, s.Seq); pErr == nil && freshPB != nil && vr.OK {
				s.ProofBundle = freshPB
			}
		}
		return s.ProofBundle
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
				ProofBundle:   refreshProof(sale),
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

	// Verify payment transaction pays >= Offer.Price via DisclosureSale on-chain escrow OR BRC-29 derived key
	// (strictly required on live networks to prevent replay of publicly visible historical deposits to static FundAddress()).
	isEscrow := req.EscrowMode
	isBRC29 := !isEscrow && req.DerivationPrefix != "" && req.DerivationSuffix != "" && req.SenderIdentityKey != ""
	if !isEscrow && !isBRC29 && b.mem.Chain().Network() != "memory" {
		return nil, errors.New("market: DisclosureSale escrow or BRC-29 payment remittance is required on live networks")
	}
	var paidSats uint64
	var escrowParams contracts.DisclosureSaleParams
	if isEscrow {
		if uint64(req.OutputIndex) >= uint64(len(tx.Outputs)) {
			return nil, fmt.Errorf("market: escrow outputIndex %d out of range", req.OutputIndex)
		}
		depPKH, dErr := hex.DecodeString(strings.TrimSpace(req.DepositorPKH))
		if dErr != nil || len(depPKH) != 20 {
			return nil, errors.New("market: escrow requires 20-byte hex depositorPkh")
		}
		sellerPKH, sErr := hex.DecodeString(strings.TrimSpace(req.EscrowSellerPKH))
		if sErr != nil || len(sellerPKH) != 20 {
			return nil, errors.New("market: escrow requires 20-byte hex escrowSellerPkh")
		}
		// Verify EscrowSellerPKH belongs to THIS seller (either our wallet FundPKH or our signed offer SellerPKH)!
		sellerPKHMatches := false
		if myFundPKH, fErr := b.mem.Chain().FundPKH(ctx); fErr == nil && bytes.Equal(myFundPKH, sellerPKH) {
			sellerPKHMatches = true
		}
		if offerPKH, oErr := hex.DecodeString(sale.SaleOffer.Config.SellerPKH); oErr == nil && bytes.Equal(offerPKH, sellerPKH) {
			sellerPKHMatches = true
		}
		if !sellerPKHMatches {
			return nil, errors.New("market: escrowSellerPkh does not belong to this seller")
		}
		hWrappedBytes, hwErr := hex.DecodeString(sale.SaleOffer.Config.HWrapped)
		if hwErr != nil || len(hWrappedBytes) != 32 {
			return nil, errors.New("market: seller offer has invalid hWrapped")
		}
		maxFee := req.EscrowMaxFee
		if maxFee < 100 || maxFee > 5000 {
			return nil, fmt.Errorf("market: escrow maxFee %d outside safe range [100, 5000]", maxFee)
		}
		refundAfter := req.EscrowRefundAfter
		if refundAfter == 0 {
			refundAfter = sale.SaleOffer.Config.RefundAfter
		}
		// Ensure RefundAfter has safe headroom so buyer cannot front-run our Buy() with an immediate Refund()!
		if refundAfter < 500_000_000 {
			if curH, hErr := b.mem.Chain().Height(ctx); hErr == nil && refundAfter <= uint64(curH)+1 {
				return nil, fmt.Errorf("market: escrow refundAfter height %d too close to tip %d", refundAfter, curH)
			}
		} else if int64(refundAfter) <= nowUnix+60 {
			return nil, fmt.Errorf("market: escrow refundAfter timestamp %d too close to now %d", refundAfter, nowUnix)
		}
		escrowParams = contracts.DisclosureSaleParams{
			SellerPKH:    sellerPKH,
			Price:        sale.SaleOffer.Config.Price,
			HWrapped:     hWrappedBytes,
			RefundAfter:  refundAfter,
			MaxFee:       maxFee,
			DepositorPKH: depPKH,
		}
		expectedEscrowLock, eErr := contracts.EncodeDisclosureSaleLockingScript(escrowParams)
		if eErr != nil {
			return nil, fmt.Errorf("market: encode DisclosureSale escrow: %w", eErr)
		}
		out := tx.Outputs[req.OutputIndex]
		if out == nil || out.LockingScript == nil || !bytes.Equal(*out.LockingScript, *expectedEscrowLock) {
			return nil, errors.New("market: escrow output locking script does not match committed DisclosureSale covenant")
		}
		if out.Satoshis < escrowParams.RequiredEscrowSatoshis() {
			return nil, fmt.Errorf("market: escrow balance %d < required %d (price + maxFee + 1)",
				out.Satoshis, escrowParams.RequiredEscrowSatoshis())
		}
		paidSats = sale.SaleOffer.Config.Price
	} else if isBRC29 {
		d := &bwallet.Derivation{
			SecurityLevel:   int(bwallet.PaymentProtocol.SecurityLevel),
			Protocol:        bwallet.PaymentProtocol.Protocol,
			KeyID:           bwallet.PaymentKeyID(req.DerivationPrefix, req.DerivationSuffix),
			CounterpartyHex: req.SenderIdentityKey,
			OwnerHex:        b.mem.Chain().IdentityPubHex(),
		}
		wantScript, dErr := b.mem.Chain().Signer().DerivedScript(ctx, d)
		if dErr != nil {
			return nil, fmt.Errorf("market: derive BRC-29 script: %w", dErr)
		}
		if uint64(req.OutputIndex) >= uint64(len(tx.Outputs)) {
			return nil, fmt.Errorf("market: BRC-29 outputIndex %d out of range", req.OutputIndex)
		}
		out := tx.Outputs[req.OutputIndex]
		if out.LockingScript == nil || !bytes.Equal(*out.LockingScript, *wantScript) {
			return nil, errors.New("market: payment output does not match BRC-29 derivation")
		}
		paidSats = out.Satoshis
	} else {
		fundScript, err := b.mem.Chain().Signer().FundScript()
		if err != nil {
			return nil, err
		}
		fundHex := fundScript.String()
		for _, out := range tx.Outputs {
			if out.LockingScript != nil && out.LockingScript.String() == fundHex {
				paidSats += out.Satoshis
			}
		}
	}
	if paidSats < sale.SaleOffer.Config.Price {
		return nil, fmt.Errorf("market: insufficient payment (%d < %d sats)", paidSats, sale.SaleOffer.Config.Price)
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
		} else if !isEscrow {
			if _, aErr := b.mem.Chain().AdoptReceived(ctx, tx); aErr != nil {
				_ = b.mem.DB().ReleasePaymentClaim(txid)
				return nil, fmt.Errorf("market: adopt received payment: %w", aErr)
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
		b.mu.Lock()
		b.pendingConfirmations[txid] = entry
		b.mu.Unlock()
		_ = b.mem.DB().SetJSON("pending_confirm:"+txid, entry)
		_ = b.mem.DB().SaveDeal(store.DealRecord{
			DealID:          "sell-" + sale.OfferID,
			Role:            "seller",
			Status:          "payment_pending",
			Shard:           sale.Shard,
			PaymentTxID:     txid,
			PaymentBEEF:     beefBytes,
			ClaimKeyHex:     b.sealKeyAtRest(ctx, hex.EncodeToString(sale.RevealKey[:])),
			ChirpURL:        sale.ChirpURL,
			CounterpartyPub: req.BuyerPub,
			PriceSats:       sale.SaleOffer.Config.Price,
			IsHubSeller:     b.cfg.IsHub,
		})
		if rc := b.RestateClient(); rc != nil {
			go func(oid, tid, sh string, sats uint64) {
				sCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, _ = rc.DeliverSellerPayment(sCtx, oid, PaymentSignal{TxID: tid, AmountSats: sats, Confirmed: false})
				_, _ = rc.EmitTxEvent(sCtx, tid, TxEventInput{Kind: "broadcast", Shard: sh, Detail: "seller-sale-pending:" + oid})
			}(sale.OfferID, txid, sale.Shard, paidSats)
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

	freshPB := refreshProof(sale)
	// Mark settled & persist durable Seller DealRecord!
	b.mu.Lock()
	sale.Settled = true
	b.mu.Unlock()
	// Build seller-signed third-entry DealReceipt (bilateral countersignature completed by buyer).
	var sellerReceipt *DealReceipt
	var sellerReceiptJSON []byte
	if sPub, pErr := b.mem.Chain().SigningPubKey(ctx); pErr == nil && sPub != nil {
		sellerSignPub := hex.EncodeToString(sPub.Compressed())
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
			SettledAt:          time.Now().Unix(),
		}
		if freshPB != nil {
			dr.FragmentLeaf = freshPB.Leaf
		}
		if sErr := SignDealReceiptSeller(ctx, sellerSignPub, b.mem.Chain().SignDigest, &dr); sErr == nil {
			sellerReceipt = &dr
			sellerReceiptJSON, _ = json.Marshal(dr)
		}
	}

	_ = b.mem.DB().MarkPendingSaleSettled(sale.OfferID)
	_ = b.mem.DB().SaveDeal(store.DealRecord{
		DealID:          "sell-" + sale.OfferID,
		Role:            "seller",
		Status:          "settled",
		Shard:           sale.Shard,
		DealReceiptJSON: sellerReceiptJSON,
		PaymentTxID:     txid,
		PaymentBEEF:     beefBytes,
		ClaimKeyHex:     b.sealKeyAtRest(ctx, hex.EncodeToString(sale.RevealKey[:])),
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
		go func(oid, tid, sh, mp string, sats uint64, conf bool, bh uint32) {
			sCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = rc.DeliverSellerPayment(sCtx, oid, PaymentSignal{TxID: tid, AmountSats: sats, Confirmed: conf, BlockHeight: bh, MerklePathHex: mp})
			_, _ = rc.EmitTxEvent(sCtx, tid, TxEventInput{Kind: "broadcast", Shard: sh, Detail: "seller-sale:" + oid})
		}(sale.OfferID, txid, sale.Shard, mpHex, paidSats, confirmed, confirmHeight)
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

// AcceptDealReceiptCountersig verifies and persists a buyer-countersigned bilateral DealReceipt on Seller's book,
// fulfilling Seller's receipt_countersig_owed obligation.
func (b *Broker) AcceptDealReceiptCountersig(rcp DealReceipt) error {
	if vr := VerifyDealReceipt(rcp, true); !vr.OK || !vr.Bilateral {
		return fmt.Errorf("market: invalid bilateral deal receipt: %s", vr.Error)
	}
	matchesLocal := strings.EqualFold(rcp.SellerPub, b.PubHex())
	if !matchesLocal {
		if sPub, err := b.mem.Chain().SigningPubKey(context.Background()); err == nil && sPub != nil {
			matchesLocal = strings.EqualFold(rcp.SellerPub, hex.EncodeToString(sPub.Compressed()))
		}
	}
	if !matchesLocal {
		return errors.New("market: deal receipt sellerPub does not match local node")
	}
	raw, err := json.Marshal(rcp)
	if err != nil {
		return err
	}
	dealID := "sell-" + rcp.OfferID
	b.engine.persistDealRecord(store.DealRecord{
		DealID:          dealID,
		Role:            "seller",
		Status:          "settled",
		DealReceiptJSON: raw,
	})
	_ = FulfillCountersigOwed(b.mem.DB(), dealID)
	return nil
}
