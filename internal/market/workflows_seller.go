package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	restate "github.com/restatedev/sdk-go"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

type sellerPrepResult struct {
	Offer       contracts.KeyedSaleOffer `json:"offer"`
	ChirpURL    string                   `json:"chirpUrl"`
	ZeroConfMax uint64                   `json:"zeroConfMax"`
	NowUnix     int64                    `json:"nowUnix"`
	FailedErr   string                   `json:"failedErr,omitempty"`
}

func (e *Engine) restateSellerRun(ctx restate.WorkflowContext, in SellerDealInput) (DealState, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return DealState{}, err
	}
	offerID := restate.Key(ctx)
	dealID := "sell-" + offerID

	// Step 1: Prepare BRC-369 keyed offer + persist pending_sales (with reveal key) + snapshot ZeroConfMaxSats inside restate.Run.
	prep, err := restate.Run(ctx, func(rctx restate.RunContext) (sellerPrepResult, error) {
		now := time.Now().Unix()
		if e.mem != nil && e.mem.DB() != nil {
			_ = SyncPolicyFromDB(e.mem.DB(), e.policy)
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
			offer, rk, chirpURL, pErr = e.PrepareDirectSale(rctx, in.Shard, in.Seq, in.PriceSats, in.BuyerPub)
			if pErr != nil {
				e.persistDealRecord(store.DealRecord{
					DealID: dealID,
					Role:   "seller",
					Status: "failed",
					Shard:  in.Shard,
					Error:  pErr.Error(),
				})
				// Deterministic policy / not-found error -> TerminalError (no ~70x retry poison!).
				return sellerPrepResult{}, restate.ToTerminalError(pErr)
			}
			revealKeyHex = hex.EncodeToString(rk[:])
			if pBundle, _, _ := e.mem.Prove(rctx, in.Shard, in.Seq); pBundle != nil {
				pb = pBundle
			}
		}

		// Persist durable pending_sales + deals row so revealKey survives seller restarts (R11)!
		// Preserve settled flag if fast HandlePurchase already settled before SellerDeal Step 1 ran.
		sealSellerKey := func() (string, error) {
			if br := e.getBroker(); br != nil {
				return br.sealKeyAtRest(rctx, revealKeyHex)
			}
			return e.sealKeyAtRest(rctx, revealKeyHex)
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
			_ = e.mem.DB().SavePendingSale(store.PendingSaleRecord{
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
			})
		}
		wantJSON, _ := json.Marshal(in.Want)
		e.persistDealRecord(store.DealRecord{
			DealID:          dealID,
			Role:            "seller",
			Status:          "offered",
			Shard:           in.Shard,
			WantJSON:        wantJSON,
			ClaimKeyHex:     sealedReveal,
			ChirpURL:        chirpURL,
			CounterpartyPub: in.BuyerPub,
			PriceSats:       in.PriceSats,
		})
		return sellerPrepResult{
			Offer:       *offer,
			ChirpURL:    chirpURL,
			ZeroConfMax: zeroConfMax,
			NowUnix:     now,
		}, nil
	}, restate.WithName("seller_prepare_sale"))
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
	restate.Set(ctx, "state", st)

	// Step 2: Bounded wait for payment promise (eliminates unbounded workflow leak — R13).
	ttl := in.OfferTTL
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	payFut := restate.Promise[PaymentSignal](ctx, "payment")
	expFut := restate.After(ctx, ttl)
	winner, waitErr := restate.WaitFirst(ctx, payFut, expFut)
	if waitErr != nil {
		return st, waitErr
	}
	if winner == expFut {
		_ = restate.Promise[PaymentSignal](ctx, "payment").Reject(restate.TerminalErrorf("offer %s expired", offerID))
		expNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
			now := time.Now().Unix()
			e.persistDealRecord(store.DealRecord{
				DealID: dealID,
				Role:   "seller",
				Status: "expired",
				Error:  "offer expired waiting for buyer payment",
			})
			return now, nil
		}, restate.WithName("seller_offer_expired"))
		st.Status = "expired"
		st.Error = "offer expired waiting for buyer payment"
		st.UpdatedAt = expNow
		restate.Set(ctx, "state", st)
		return st, nil
	}
	paySig, pErr := payFut.Result()
	if pErr != nil {
		return st, pErr
	}

	// Step 3: If unconfirmed payment exceeds journaled ZeroConfMax (0 = fail-closed, zero-conf disabled!),
	// wait on confirmation promise (R13 — no dead end!).
	if !paySig.Confirmed && (prep.ZeroConfMax == 0 || paySig.AmountSats > prep.ZeroConfMax) {
		pendNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
			now := time.Now().Unix()
			e.persistDealRecord(store.DealRecord{
				DealID:      dealID,
				Role:        "seller",
				Status:      "payment_pending",
				PaymentTxID: paySig.TxID,
			})
			return now, nil
		}, restate.WithName("seller_payment_pending"))
		st.Status = "payment_pending"
		st.PaymentTxID = paySig.TxID
		st.UpdatedAt = pendNow
		restate.Set(ctx, "state", st)

		confTimeout := in.ConfirmTimeout
		if confTimeout <= 0 {
			confTimeout = 2 * time.Hour
		}
		confFut := restate.Promise[PaymentSignal](ctx, "payment_confirmed")
		confTimer := restate.After(ctx, confTimeout)
		cWin, cwErr := restate.WaitFirst(ctx, confFut, confTimer)
		if cwErr != nil {
			return st, cwErr
		}
		if cWin == confTimer {
			_ = restate.Promise[PaymentSignal](ctx, "payment_confirmed").Reject(restate.TerminalErrorf("payment confirmation timed out for %s", offerID))
			expNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
				now := time.Now().Unix()
				e.persistDealRecord(store.DealRecord{
					DealID:      dealID,
					Role:        "seller",
					Status:      "expired",
					PaymentTxID: paySig.TxID,
					Error:       "timed out waiting for payment confirmation",
				})
				return now, nil
			}, restate.WithName("seller_confirm_expired"))
			st.Status = "expired"
			st.Error = "timed out waiting for payment confirmation"
			st.UpdatedAt = expNow
			restate.Set(ctx, "state", st)
			return st, nil
		}
		confSig, cfErr := confFut.Result()
		if cfErr != nil {
			return st, cfErr
		}
		paySig = confSig
	}

	// Step 4: Settle seller deal durably in SQLite inside restate.Run.
	settleNow, sErr := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
		now := time.Now().Unix()
		if e.mem != nil && e.mem.DB() != nil {
			_ = e.mem.DB().MarkPendingSaleSettled(offerID)
		}
		e.persistDealRecord(store.DealRecord{
			DealID:      dealID,
			Role:        "seller",
			Status:      "settled",
			PaymentTxID: paySig.TxID,
			Error:       "",
		})
		return now, nil
	}, restate.WithName("seller_settle"))
	if sErr != nil {
		return st, sErr
	}
	st.PaymentTxID = paySig.TxID
	st.Status = "settled"
	st.UpdatedAt = settleNow
	restate.Set(ctx, "state", st)
	return st, nil
}

func (e *Engine) restateSellerDeliverPayment(ctx restate.WorkflowSharedContext, sig PaymentSignal) (bool, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return false, err
	}
	// Never trust caller-asserted sig.Confirmed on live networks without verifying a header-checked MerklePath.
	if sig.Confirmed && e.mem != nil && e.mem.Chain() != nil && e.mem.Chain().Network() != "memory" {
		if sig.MerklePathHex == "" {
			sig.Confirmed = false
		} else {
			bh, vErr := restate.Run(ctx, func(rctx restate.RunContext) (uint32, error) {
				return e.verifyMerklePathProof(rctx, sig.TxID, sig.MerklePathHex, true)
			}, restate.WithName("seller_deliver_payment_verify_bump"))
			if vErr != nil {
				sig.Confirmed = false
			} else {
				sig.BlockHeight = bh
			}
		}
	}
	if err := restate.Promise[PaymentSignal](ctx, "payment").Resolve(sig); err != nil {
		return false, err
	}
	return true, nil
}

func (e *Engine) restateSellerConfirmPayment(ctx restate.WorkflowSharedContext, sig PaymentSignal) (bool, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return false, err
	}
	if e.mem != nil && e.mem.Chain() != nil && e.mem.Chain().Network() != "memory" {
		bh, vErr := restate.Run(ctx, func(rctx restate.RunContext) (uint32, error) {
			return e.verifyMerklePathProof(rctx, sig.TxID, sig.MerklePathHex, true)
		}, restate.WithName("seller_confirm_payment_verify_bump"))
		if vErr != nil {
			return false, vErr
		}
		sig.BlockHeight = bh
	}
	sig.Confirmed = true
	if err := restate.Promise[PaymentSignal](ctx, "payment_confirmed").Resolve(sig); err != nil {
		return false, err
	}
	return true, nil
}

// PrepareDirectSale creates a signed BRC-369 keyed DisclosureSale offer for (shard, seq),
// stores the encrypted ciphertext in CHIRP (BRC-167), and returns the public offer + private reveal key k.
func (e *Engine) PrepareDirectSale(ctx context.Context, shard string, seq int64, priceSats uint64, buyerPub string) (*contracts.KeyedSaleOffer, [32]byte, string, error) {
	// Never disclose high-clearance or Borg-encrypted fragments: Level 0 + DisallowBorg + Cedar disclose.
	sellerPrincipal := policy.Principal{Type: "Agent", ID: "market-seller", Level: 0}
	recalled, err := e.mem.Recall(ctx, memory.RecallInput{
		Shard:        shard,
		Principal:    sellerPrincipal,
		Action:       "disclose",
		DisallowBorg: true,
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
	if dec, cErr := e.mem.AuthorizeAction(sellerPrincipal, "disclose", policy.Resource{
		Type:  "Fragment",
		ID:    fmt.Sprintf("%s#%d", target.Shard, target.Seq),
		Level: int64(target.Level),
		Shard: target.Shard,
		Class: target.Class,
	}); cErr != nil || !dec.Allow {
		return nil, [32]byte{}, "", fmt.Errorf("market: cedar policy denied disclose of %s#%d", shard, seq)
	}
	expiresAt := uint64(time.Now().Add(time.Hour).Unix())
	offer, err := contracts.CreateKeyedSaleOffer(
		e.saleKey,
		[]byte(target.Text),
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
	ctBytes, _ := hex.DecodeString(offer.CiphertextHex)
	chirpURL, _, hashes, err := bstack.StoreChirpWithHashes(e.mem.Objects(), ctBytes)
	if err != nil {
		return nil, [32]byte{}, "", err
	}
	e.mem.AllowPublicChirpHashes(hashes...)
	return offer, offer.ContentKey(), chirpURL, nil
}

// ExecuteAtomicKnowledgeTrade runs a complete end-to-end buyer <-> seller BRC-369 paid disclosure
// trade on the chain: seller seals fragment with BRC-369 keyed commitment & stores ciphertext in CHIRP,
// buyer verifies seller's signed DisclosureSale offer & pays seller on-chain, seller reveals content
// scalar k, buyer verifies both hWrapped & BRC-369 commitment, decrypts CHIRP ciphertext, and commits
// the purchased fact into its own anchored memory shard!
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

	// 1. Seller prepares signed BRC-369 keyed sale offer + stores ciphertext in CHIRP.
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

	// 2. Buyer cryptographically verifies the seller's signed DisclosureSale offer & covenant artifact hash!
	if err := contracts.VerifySaleOffer(saleOffer, buyerPub, sellerLeaf); err != nil {
		return nil, fmt.Errorf("market: buyer rejected invalid offer: %w", err)
	}

	// 3. Buyer pays seller on-chain (SpendGuard enforced!).
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
	buyer.policy.RecordSpend(priceSats)

	// 4. Buyer fetches ciphertext from seller's CHIRP store (verified by BRC-167 closure),
	//    verifies revealed key k against DisclosureSale hWrapped + BRC-369 KeyCommitment, and decrypts plaintext!
	verifiedCT, err := bstack.ResolveChirp(seller.mem.Objects(), chirpURL)
	if err != nil {
		return nil, fmt.Errorf("market: resolve CHIRP ciphertext: %w", err)
	}
	if hex.EncodeToString(verifiedCT) != saleOffer.CiphertextHex {
		return nil, errors.New("market: CHIRP ciphertext mismatch vs sale offer")
	}
	plaintext, err := contracts.ClaimKeyedSale(saleOffer, revealKey[:])
	if err != nil {
		return nil, fmt.Errorf("market: claim BRC-369 sale: %w", err)
	}

	// 5. Buyer commits the purchased, verified knowledge into its own memory shard (tainted: true)!
	if _, err := buyer.mem.Remember(ctx, memory.RememberInput{
		Shard:      buyerShard,
		Text:       string(plaintext),
		Source:     "market:" + seller.mem.Chain().IdentityPubHex()[:12],
		Tainted:    true,
		AutoAnchor: true,
	}); err != nil {
		return nil, fmt.Errorf("market: store purchased fact: %w", err)
	}

	dealID := "deal-" + payTxID[:12]
	deal := DealState{
		DealID:       dealID,
		Role:         "buyer",
		Status:       "settled",
		ChirpURL:     chirpURL,
		SaleOffer:    saleOffer,
		PaymentTxID:  payTxID,
		Shard:        buyerShard,
		DeliveredTxt: string(plaintext),
		UpdatedAt:    time.Now().Unix(),
	}
	buyer.persistDealRecord(store.DealRecord{
		DealID:        dealID,
		Role:          "buyer",
		Status:        "settled",
		Shard:         buyerShard,
		PaymentTxID:   payTxID,
		ChirpURL:      chirpURL,
		DeliveredText: string(plaintext),
		PriceSats:     priceSats,
	})
	seller.persistDealRecord(store.DealRecord{
		DealID:        "sell-" + dealID,
		Role:          "seller",
		Status:        "settled",
		Shard:         sellerShard,
		PaymentTxID:   payTxID,
		ChirpURL:      chirpURL,
		DeliveredText: string(plaintext),
		PriceSats:     priceSats,
	})
	return &deal, nil
}
