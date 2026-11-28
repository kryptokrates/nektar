package market

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/cschleiden/go-workflows/workflow"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reputation"
	"github.com/kryptokrates/nektar/internal/store"
)

type sellerPrepResult struct {
	Offer       contracts.KeyedSaleOffer `json:"offer"`
	ChirpURL    string                   `json:"chirpUrl"`
	ZeroConfMax uint64                   `json:"zeroConfMax"`
	NowUnix     int64                    `json:"nowUnix"`
	FailedErr   string                   `json:"failedErr,omitempty"`
}

// SellerDealWorkflow orchestrates a seller knowledge disclosure saga on pure-Go go-workflows:
//  1. Seal BRC-369 keyed offer + persist pending_sales & deals atomically (RCA-2)
//  2. Bounded wait on SignalChannel("payment")
//  3. If unconfirmed & over ZeroConfMax, wait on SignalChannel("payment_confirmed")
//  4. Settle seller deal, economics, dynamic pricing, and bilateral attestation in SQLite
func SellerDealWorkflow(ctx workflow.Context, in SellerDealInput) (DealState, error) {
	var acts *workflowActivities
	instID := workflow.WorkflowInstance(ctx).InstanceID
	offerID := strings.TrimPrefix(instID, sellerWorkflowPrefix)
	dealID := sellerDealID(offerID)

	payCh := workflow.NewSignalChannel[PaymentSignal](ctx, "payment")
	confCh := workflow.NewSignalChannel[PaymentSignal](ctx, "payment_confirmed")

	prep, err := workflow.ExecuteActivity[sellerPrepResult](ctx, dealActivityOpts(), acts.SellerPrepareSale, offerID, dealID, in).Get(ctx)
	if err != nil {
		return DealState{DealID: offerID, Role: "seller", Status: "failed", Error: err.Error()}, err
	}

	st := DealState{
		DealID:    offerID,
		Role:      "seller",
		Status:    "offered",
		Want:      in.Want,
		SaleOffer: &prep.Offer,
		ChirpURL:  prep.ChirpURL,
		Shard:     in.Shard,
		UpdatedAt: prep.NowUnix,
	}

	ttl := in.OfferTTL
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	tCtx, cancelTimer := workflow.WithCancel(ctx)
	expTimer := workflow.ScheduleTimer(tCtx, ttl)
	var paySig PaymentSignal
	gotPay := false
	workflow.Select(ctx,
		workflow.Receive(payCh, func(_ workflow.Context, v PaymentSignal, ok bool) {
			cancelTimer()
			if ok {
				paySig = v
				gotPay = true
			}
		}),
		workflow.Await(expTimer, func(_ workflow.Context, _ workflow.Future[any]) {
		}),
	)
	cancelTimer()

	if !gotPay {
		expNow, _ := workflow.ExecuteActivity[int64](ctx, dealActivityOpts(), acts.PersistDealState, store.DealRecord{
			DealID: dealID,
			Role:   "seller",
			Status: "expired",
			Error:  "offer expired waiting for buyer payment",
		}).Get(ctx)
		st.Status = "expired"
		st.Error = "offer expired waiting for buyer payment"
		st.UpdatedAt = expNow
		return st, nil
	}

	if !paySig.Confirmed && (prep.ZeroConfMax == 0 || paySig.AmountSats > prep.ZeroConfMax) {
		pendNow, _ := workflow.ExecuteActivity[int64](ctx, dealActivityOpts(), acts.PersistDealState, store.DealRecord{
			DealID:      dealID,
			Role:        "seller",
			Status:      "payment_pending",
			PaymentTxID: paySig.TxID,
		}).Get(ctx)
		st.Status = "payment_pending"
		st.PaymentTxID = paySig.TxID
		st.UpdatedAt = pendNow

		confTimeout := in.ConfirmTimeout
		if confTimeout <= 0 {
			confTimeout = 2 * time.Hour
		}
		cCtx, cancelConf := workflow.WithCancel(ctx)
		confTimer := workflow.ScheduleTimer(cCtx, confTimeout)
		var confSig PaymentSignal
		gotConf := false
		workflow.Select(ctx,
			workflow.Receive(confCh, func(_ workflow.Context, v PaymentSignal, ok bool) {
				cancelConf()
				if ok {
					confSig = v
					gotConf = true
				}
			}),
			workflow.Await(confTimer, func(_ workflow.Context, _ workflow.Future[any]) {
			}),
		)
		cancelConf()

		if !gotConf {
			expNow, _ := workflow.ExecuteActivity[int64](ctx, dealActivityOpts(), acts.PersistDealState, store.DealRecord{
				DealID:      dealID,
				Role:        "seller",
				Status:      "expired",
				PaymentTxID: paySig.TxID,
				Error:       "timed out waiting for payment confirmation",
			}).Get(ctx)
			st.Status = "expired"
			st.Error = "timed out waiting for payment confirmation"
			st.UpdatedAt = expNow
			return st, nil
		}
		paySig = confSig
	}

	settleNow, sErr := workflow.ExecuteActivity[int64](ctx, dealActivityOpts(), acts.SellerSettle, offerID, dealID, in, paySig).Get(ctx)
	if sErr != nil {
		return st, sErr
	}
	st.PaymentTxID = paySig.TxID
	st.Status = "settled"
	st.UpdatedAt = settleNow
	return st, nil
}

func (a *workflowActivities) SellerPrepareSale(ctx context.Context, offerID, dealID string, in SellerDealInput) (sellerPrepResult, error) {
	e := a.eng
	now := time.Now().Unix()
	if e.policy != nil {
		if err := e.policy.Refresh(); err != nil {
			return sellerPrepResult{}, err
		}
	}
	_, _, _, zeroConfMax, _, _, _ := e.policy.Snapshot()

	var offer *contracts.KeyedSaleOffer
	var revealKeyHex, chirpURL string
	var pb *contracts.ProofBundle
	if in.PreCreatedOffer != nil && in.RevealKeyHex != "" {
		offer = in.PreCreatedOffer
		revealKeyHex = in.RevealKeyHex
		chirpURL = in.ChirpURL
		pb = in.ProofBundle
	} else {
		var rk [32]byte
		var pErr error
		offer, rk, chirpURL, pErr = e.PrepareDirectSale(ctx, in.Shard, in.Seq, in.PriceSats, in.BuyerPub)
		if pErr != nil {
			_ = e.persistDealRecordChecked(store.DealRecord{
				DealID: dealID,
				Role:   "seller",
				Status: "failed",
				Shard:  in.Shard,
				Error:  pErr.Error(),
			})
			return sellerPrepResult{}, workflow.NewPermanentError(pErr)
		}
		revealKeyHex = hex.EncodeToString(rk[:])
		if pBundle, _, _ := e.mem.Prove(ctx, in.Shard, in.Seq); pBundle != nil {
			pb = pBundle
		}
	}

	sealSellerKey := func() (string, error) {
		if br := e.getBroker(); br != nil {
			return br.sealKeyAtRest(ctx, revealKeyHex)
		}
		return e.sealKeyAtRest(ctx, revealKeyHex)
	}
	sealedReveal, sealErr := sealSellerKey()
	if sealErr != nil {
		return sellerPrepResult{}, sealErr
	}
	alreadySettled := false
	if e.mem != nil && e.mem.DB() != nil {
		if existingDeal, gErr := e.mem.DB().GetDeal(dealID); gErr == nil && existingDeal.Status == "settled" {
			alreadySettled = true
		}
		saleJSON, _ := json.Marshal(offer)
		var pbJSON []byte
		if pb != nil {
			pbJSON, _ = json.Marshal(pb)
		}
		if err := e.mem.DB().SavePendingSale(store.PendingSaleRecord{
			OfferID:         offerID,
			WantID:          in.Want.WantID,
			BuyerPub:        in.BuyerPub,
			Shard:           in.Shard,
			Seq:             in.Seq,
			SaleOfferJSON:   saleJSON,
			RevealKeyHex:    sealedReveal,
			ChirpURL:        chirpURL,
			ProofBundleJSON: pbJSON,
			ExpiresAt:       int64(offer.ExpiresAt),
			Settled:         alreadySettled,
		}); err != nil {
			return sellerPrepResult{}, err
		}
	}
	wantJSON, _ := json.Marshal(in.Want)
	if err := e.persistDealRecordChecked(store.DealRecord{
		DealID:          dealID,
		Role:            "seller",
		Status:          "offered",
		Shard:           in.Shard,
		WantJSON:        wantJSON,
		ClaimKeyHex:     sealedReveal,
		ChirpURL:        chirpURL,
		CounterpartyPub: in.BuyerPub,
		PriceSats:       in.PriceSats,
	}); err != nil {
		return sellerPrepResult{}, err
	}
	return sellerPrepResult{
		Offer:       *offer,
		ChirpURL:    chirpURL,
		ZeroConfMax: zeroConfMax,
		NowUnix:     now,
	}, nil
}

func (a *workflowActivities) SellerSettle(ctx context.Context, offerID, dealID string, in SellerDealInput, paySig PaymentSignal) (int64, error) {
	e := a.eng
	now := time.Now().Unix()
	if e.mem != nil && e.mem.DB() != nil {
		if err := e.mem.DB().MarkPendingSaleSettled(offerID); err != nil {
			return 0, err
		}
		_ = e.mem.DB().RecordEconomicsEvent(store.EconomicsEventRow{
			Kind:       "deal_sell",
			SatsDelta:  int64(in.PriceSats),
			OntologyID: reputation.NormalizeOntologyID(in.Want.OntologyID),
			Shard:      in.Shard,
			DealID:     dealID,
			CreatedAt:  now,
		})
		if listings, lErr := e.mem.DB().ListSellListings(); lErr == nil {
			for _, l := range listings {
				if l.Shard == in.Shard && len(l.DynamicPricingJSON) > 0 {
					dp := ParseDynamicPriceState(l.DynamicPricingJSON, l.PriceSats)
					var sellerComp int64 = 7500
					if r6, rErr := e.mem.DB().GetReputation6D(e.salePubHex(), reputation.NormalizeOntologyID(in.Want.OntologyID)); rErr == nil && r6.CompositeBP > 0 {
						sellerComp = r6.CompositeBP
					}
					nextPrice := dp.OnDemandHit(l.PriceSats, sellerComp, now)
					if rawDyn, mErr := json.Marshal(dp); mErr == nil {
						_ = e.mem.DB().UpdateSellListingPricing(l.Shard, nextPrice, rawDyn)
					}
					break
				}
			}
		}
		if dRec, gErr := e.mem.DB().GetDeal(dealID); gErr == nil && len(dRec.DealReceiptJSON) > 0 {
			var dr DealReceipt
			if json.Unmarshal(dRec.DealReceiptJSON, &dr) == nil && dr.DealID != "" && dr.BuyerPub != "" {
				bScores := reputation.EvaluateBuyerCollaboration(reputation.SellerEvalInput{
					BuyerPub:          dr.BuyerPub,
					OntologyID:        in.Want.OntologyID,
					HasPinnedOntology: in.Want.OntologyID != "",
					CleanFirstPay:     true,
				})
				_, _ = e.EmitPostDealAttestation(ctx, reputation.RoleSellerRatesBuyer, dr, in.Want.OntologyID, bScores, in.Want.Domains)
			}
		}
	}
	if err := e.persistDealRecordChecked(store.DealRecord{
		DealID:      dealID,
		Role:        "seller",
		Status:      "settled",
		PaymentTxID: paySig.TxID,
		Error:       "",
	}); err != nil {
		return 0, err
	}
	return now, nil
}

// PrepareDirectSale creates a signed BRC-369 keyed DisclosureSale offer for (shard, seq),
// enforces the unified fail-closed sell egress gate (RCA-1), stores offloaded >16 KiB ciphertexts in
// `bstore`/CHIRP (while keeping <=16 KiB ciphertexts inline AND CHIRP-backed), and returns the offer + reveal key k.
func (e *Engine) PrepareDirectSale(ctx context.Context, shard string, seq int64, priceSats uint64, buyerPub string) (*contracts.KeyedSaleOffer, [32]byte, string, error) {
	if e.keyInitErr != nil {
		return nil, [32]byte{}, "", e.keyInitErr
	}
	if e.signer == nil {
		return nil, [32]byte{}, "", fmt.Errorf("market: sale signer unavailable")
	}
	sellerPrincipal := policy.Principal{Type: "Agent", ID: "market-seller", Level: 0}
	recalled, err := e.mem.Recall(ctx, memory.RecallInput{
		Shard:        shard,
		Principal:    sellerPrincipal,
		Action:       "disclose",
		DisallowBorg: true,
		VerifySPV:    false,
	})
	if err != nil {
		return nil, [32]byte{}, "", err
	}
	var target *memory.RecalledFragment
	for i := range recalled {
		if recalled[i].Seq == seq {
			cp := recalled[i]
			target = &cp
			break
		}
	}
	if target == nil {
		return nil, [32]byte{}, "", fmt.Errorf("market: fragment %s#%d not found or unauthorized", shard, seq)
	}
	var listing *store.SellListing
	if e.mem.DB() != nil {
		listing, _ = e.mem.DB().GetSellListing(shard)
	}
	return e.PrepareDirectSaleWithFragment(ctx, *target, listing, priceSats, buyerPub)
}

// PrepareDirectSaleWithFragment builds and signs a BRC-369 KeyedSaleOffer directly from an
// already-recalled RecalledFragment, avoiding a redundant full-shard Recall on Seller quote hot paths.
func (e *Engine) PrepareDirectSaleWithFragment(ctx context.Context, target memory.RecalledFragment, listing *store.SellListing, priceSats uint64, buyerPub string) (*contracts.KeyedSaleOffer, [32]byte, string, error) {
	if e.keyInitErr != nil {
		return nil, [32]byte{}, "", e.keyInitErr
	}
	if e.signer == nil {
		return nil, [32]byte{}, "", fmt.Errorf("market: sale signer unavailable")
	}
	shard := target.Shard
	seq := target.Seq
	sellerPrincipal := policy.Principal{Type: "Agent", ID: "market-seller", Level: 0}
	if dec, cErr := e.mem.AuthorizeAction(sellerPrincipal, "disclose", policy.Resource{
		Type:  "Fragment",
		ID:    fmt.Sprintf("%s#%d", target.Shard, target.Seq),
		Level: int64(target.Level),
		Shard: target.Shard,
		Class: target.Class,
	}); cErr != nil || !dec.Allow {
		return nil, [32]byte{}, "", fmt.Errorf("market: cedar policy denied disclose of %s#%d", shard, seq)
	}
	if listing == nil {
		// Direct atomic trade without an armed listing defaults to AllowRaw=true ONLY when Cedar + PII Scrub + Tainted checks pass!
		listing = &store.SellListing{Shard: shard, AllowRaw: true, Active: true}
	}
	if eval := e.mem.EvaluateSellableFragment(ctx, target, listing, false, "", time.Now().Unix()); !eval.Allowed {
		return nil, [32]byte{}, "", fmt.Errorf("market: fragment %s#%d blocked from sale (%s)", shard, seq, eval.Reason)
	}
	fj := e.FastJournal()
	capKey := fmt.Sprintf("cap:%s:%d:%s:%s", shard, seq, target.Leaf, target.AnchorTxID)
	var capBytes []byte
	if fj != nil {
		if cached, _, _, ok := fj.GetCachedCapsule(capKey); ok && len(cached) > 0 {
			capBytes = cached
		}
	}
	if len(capBytes) == 0 {
		var capBuf bytes.Buffer
		if _, err := e.mem.ExportFragmentCapsuleStream(ctx, &capBuf, shard, seq, &memory.CapsuleExportOpts{
			SelfContained: true,
			SignExport:    true,
		}); err != nil {
			return nil, [32]byte{}, "", fmt.Errorf("market: export fragment .nkt capsule %s#%d: %w", shard, seq, err)
		}
		capBytes = capBuf.Bytes()
		if fj != nil {
			fj.PutCachedCapsule(capKey, capBytes, "nkt", nil)
		}
	}
	expiresAt := uint64(time.Now().Add(time.Hour).Unix())
	offer, err := contracts.CreateKeyedSaleOfferWithSigner(
		e.signer.PubHex(),
		e.signer.SignDigest,
		capBytes,
		priceSats,
		expiresAt+3600,
		2000,
		expiresAt,
		buyerPub,
		target.Leaf,
		true,
	)
	if err != nil {
		return nil, [32]byte{}, "", err
	}
	offer.PayloadFormat = "nkt"
	// Skip CHIRP disk file creation + SQLite allowlist write when ciphertext fits inline inside OfferAd
	// in high-throughput fast-journal mode (zero NTFS temp+rename, zero SQLite lock).
	if fj != nil && len(offer.CiphertextHex) > 0 && len(offer.CiphertextHex) <= bstack.InlineOfferMaxHexLen {
		return offer, offer.ContentKey(), "", nil
	}
	ctBytes, _ := hex.DecodeString(offer.CiphertextHex)
	var chirpURL string
	var hashes [][32]byte
	if bstack.ClassifyDeliveryTier(len(ctBytes)) == bstack.DeliveryTierBstore {
		chirpURL, _, _, hashes, err = bstack.BuildBstoreEntryForData(
			e.mem.Objects(),
			"nektar",
			e.signer.PubCompressed(),
			fmt.Sprintf("sales/%s/%d.nkt.enc", shard, seq),
			"application/vnd.nektar.capsule+brc369",
			ctBytes,
			uint64(time.Now().UnixMilli()),
		)
	} else {
		chirpURL, _, hashes, err = bstack.StoreChirpWithHashes(e.mem.Objects(), ctBytes)
	}
	if err != nil {
		return nil, [32]byte{}, "", err
	}
	e.mem.AllowPublicChirpHashes(hashes...)
	return offer, offer.ContentKey(), chirpURL, nil
}

// ExecuteAtomicKnowledgeTrade runs a complete end-to-end buyer <-> seller BRC-369 paid disclosure trade.
func ExecuteAtomicKnowledgeTrade(
	ctx context.Context,
	buyer *Engine,
	seller *Engine,
	sellerShard string,
	sellerSeq int64,
	buyerShard string,
	priceSats uint64,
) (*DealState, error) {
	if err := buyer.policy.CanSpend(seller.mem.Chain().IdentityPubHex(), priceSats); err != nil {
		return nil, err
	}
	buyerPub := buyer.mem.Chain().IdentityPubHex()

	recalledSeller, _ := seller.mem.Recall(ctx, memory.RecallInput{Shard: sellerShard})
	var sellerLeaf string
	for _, rf := range recalledSeller {
		if rf.Seq == sellerSeq {
			sellerLeaf = rf.Leaf
			break
		}
	}
	saleOffer, revealKey, chirpURL, err := seller.PrepareDirectSale(ctx, sellerShard, sellerSeq, priceSats, buyerPub)
	if err != nil {
		return nil, err
	}
	if err := contracts.VerifySaleOffer(saleOffer, buyerPub, sellerLeaf); err != nil {
		return nil, fmt.Errorf("market: buyer rejected invalid offer: %w", err)
	}
	sellerPayScript, err := seller.mem.Chain().Signer().FundScript()
	if err != nil {
		return nil, err
	}
	payTx, fee, spend, err := buyer.mem.Chain().BuildFundedTx(ctx, []*transaction.TransactionOutput{
		{Satoshis: priceSats, LockingScript: sellerPayScript},
	})
	if err != nil {
		return nil, fmt.Errorf("market: buyer fund payment: %w", err)
	}
	payTxID, err := buyer.mem.Chain().Broadcast(ctx, payTx, fee, spend)
	if err != nil {
		return nil, fmt.Errorf("market: buyer broadcast payment: %w", err)
	}
	ctBytes, err := bstack.ResolveChirp(seller.mem.Objects(), chirpURL)
	if err != nil {
		return nil, fmt.Errorf("market: buyer fetch CHIRP ciphertext: %w", err)
	}
	saleOffer.CiphertextHex = hex.EncodeToString(ctBytes)
	plaintext, err := contracts.ClaimKeyedSale(saleOffer, revealKey[:])
	if err != nil {
		return nil, fmt.Errorf("market: buyer BRC-369 claim failed: %w", err)
	}
	deliveredTxt := string(plaintext)
	if memrepo.IsCapsuleBytes(plaintext) {
		decCap, rep, vErr := memrepo.VerifyCapsuleStream(ctx, bytes.NewReader(plaintext), memrepo.CapsuleVerifyOpts{
			Objects:    buyer.mem.Objects(),
			Tracker:    buyer.mem.Chain().Tracker(),
			MemoryMode: buyer.mem.Chain().Network() == "memory",
		})
		if vErr != nil || rep == nil || !rep.OK {
			reason := ""
			if rep != nil {
				reason = rep.Reason
			}
			return nil, fmt.Errorf("market: buyer .nkt capsule verification failed: %v %s", vErr, reason)
		}
		if _, iErr := buyer.mem.ImportCapsuleStream(ctx, bytes.NewReader(plaintext), buyerShard, memrepo.DefaultBranch, true, nil, nil); iErr != nil {
			return nil, fmt.Errorf("market: buyer import .nkt capsule: %w", iErr)
		}
		if len(decCap.Fragments) > 0 {
			deliveredTxt = string(decCap.Fragments[0].Data)
		}
	} else {
		if _, err := buyer.mem.Remember(ctx, memory.RememberInput{
			Shard:   buyerShard,
			Text:    deliveredTxt,
			Source:  "market:" + shortPub(seller.mem.Chain().IdentityPubHex()),
			Tainted: true,
		}); err != nil {
			return nil, err
		}
	}
	if _, err := buyer.mem.AnchorShard(ctx, buyerShard); err != nil {
		return nil, err
	}
	buyer.policy.RecordSpend(priceSats)
	dealID := saleOffer.KeyCommitment
	if len(dealID) > 16 {
		dealID = dealID[:16]
	}
	return &DealState{
		DealID:       "direct-" + dealID,
		Role:         "buyer",
		Status:       "settled",
		SaleOffer:    saleOffer,
		ChirpURL:     chirpURL,
		PaymentTxID:  payTxID,
		Shard:        buyerShard,
		DeliveredTxt: deliveredTxt,
		UpdatedAt:    time.Now().Unix(),
	}, nil
}
