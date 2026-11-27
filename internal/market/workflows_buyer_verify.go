package market

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/store"
)

func (e *Engine) buyerVerifyAndCommit(
	rctx context.Context,
	dealID string,
	want bstack.WantAd,
	targetShardIn string,
	chosen bstack.OfferAd,
	paymentTxID string,
	matchAudit *MatchAudit,
	sig ClaimSignal,
) (buyerVerifyResult, error) {
	now := time.Now().Unix()
	var deliveredText string
	targetShard := strings.TrimSpace(targetShardIn)
	if targetShard == "" {
		dom := "ops"
		if len(want.Domains) > 0 {
			dom = want.Domains[0]
		}
		targetShard = "market/" + dom
	}

	var disc *discovery.Service
	if br := e.getBroker(); br != nil {
		disc = br.disc
	}
	plaintext, resolvedKeyHex, failReason := verifySaleDelivery(
		rctx, e.mem, disc, e.policy, dealID, chosen,
		sig.ContentKeyHex, sig.ClaimTxBEEFHex, sig.CiphertextHex, sig.ProofBundle,
	)
	if failReason != "" {
		return buyerVerifyResult{NowUnix: now, FailedReason: failReason}, nil
	}
	sig.ContentKeyHex = resolvedKeyHex
	deliveredText = string(plaintext)
	{
		dom := "ops"
		if len(want.Domains) > 0 {
			dom = want.Domains[0]
		}
		var extraParents []memrepo.ParentRef
		if sig.ProofBundle != nil {
			extraParents = append(extraParents, memrepo.ParentRef{
				CommitHash:  sig.ProofBundle.Leaf,
				AnchorTxID:  sig.ProofBundle.Anchor.TxID,
				AnchorVout:  sig.ProofBundle.Anchor.OutputIndex,
				RepoGenesis: sig.ProofBundle.Shard,
			})
		}
		if _, rErr := e.mem.Remember(rctx, memory.RememberInput{
			Shard:         targetShard,
			Path:          fmt.Sprintf("market/%s-%s-%d.md", dom, shortPub(chosen.Pub), now),
			MediaType:     "text/markdown",
			Text:          deliveredText,
			CommitMessage: fmt.Sprintf("Market import (%s, %d sats from %s)", dom, chosen.PriceSats, shortPub(chosen.Pub)),
			ExtraParents:  extraParents,
			Source:        "market:" + shortPub(chosen.Pub),
			Class:         "market:" + dom,
			Tainted:       true, // F10: purchased external knowledge is ALWAYS marked tainted!
			AutoAnchor:    e.mem.Chain().Network() == "memory" && e.mem.Chain().BalanceSats() > 1000,
		}); rErr != nil {
			return buyerVerifyResult{}, rErr
		}
	}

	if e.mem != nil && e.mem.DB() != nil && chosen.Pub != "" {
		_ = e.mem.DB().RecordReputation(chosen.Pub, true, chosen.PriceSats)
	}
	var dealReceipt *DealReceipt
	var dealReceiptJSON []byte
	if e.mem != nil && e.mem.Chain() != nil {
		buyerSignPub := e.salePubHex()
		expectedPayTxID := paymentTxID
		if expectedPayTxID == "" {
			expectedPayTxID = sig.ClaimTxID
		}
		var dr DealReceipt
		hasSellerReceipt := sig.DealReceipt != nil
		if hasSellerReceipt {
			dr = *sig.DealReceipt
			if dr.SellerSig == "" {
				return buyerVerifyResult{NowUnix: now, FailedReason: "seller triple-entry deal receipt missing required sellerSig"}, nil
			}
			if vr := VerifyDealReceipt(dr, false); !vr.OK || !vr.SellerVerified {
				return buyerVerifyResult{NowUnix: now, FailedReason: "seller triple-entry deal receipt signature invalid: " + vr.Error}, nil
			}
			if dr.DealID != dealID ||
				dr.OfferID != chosen.OfferID ||
				!strings.EqualFold(dr.SellerPub, chosen.Pub) ||
				dr.PriceSats != chosen.PriceSats ||
				(expectedPayTxID != "" && !strings.EqualFold(dr.PaymentTxID, expectedPayTxID)) ||
				(chosen.KeyCommitHex != "" && !strings.EqualFold(dr.KeyCommitment, chosen.KeyCommitHex)) ||
				(chosen.Leaf != "" && !strings.EqualFold(dr.FragmentLeaf, chosen.Leaf)) {
				return buyerVerifyResult{NowUnix: now, FailedReason: "seller triple-entry deal receipt commercial fields mismatch agreed deal"}, nil
			}
		} else {
			dr = DealReceipt{
				Version:       1,
				DealID:        dealID,
				OfferID:       chosen.OfferID,
				BuyerPub:      buyerSignPub,
				SellerPub:     chosen.Pub,
				Domains:       want.Domains,
				PriceSats:     chosen.PriceSats,
				WantHash:      sha256CanonicalHex(want),
				PaymentTxID:   expectedPayTxID,
				KeyCommitment: chosen.KeyCommitHex,
				FragmentLeaf:  chosen.Leaf,
				SettledAt:     now,
			}
			if e.policy != nil {
				dr.BuyerPolicyHash = e.policy.ActivePolicyHash()
			}
			if matchAudit != nil {
				dr.MatchAuditHash = sha256CanonicalHex(matchAudit)
			}
		}
		if sErr := SignDealReceiptBuyer(rctx, buyerSignPub, e.signSaleDigest, &dr); sErr != nil {
			if hasSellerReceipt {
				return buyerVerifyResult{NowUnix: now, FailedReason: "countersign bilateral deal receipt failed: " + sErr.Error()}, nil
			}
		} else {
			if hasSellerReceipt {
				if bvr := VerifyDealReceipt(dr, true); !bvr.OK || !bvr.Bilateral {
					return buyerVerifyResult{NowUnix: now, FailedReason: "bilateral deal receipt verification failed: " + bvr.Error}, nil
				}
			}
			dealReceipt = &dr
			dealReceiptJSON, _ = json.Marshal(dr)
			_, _, _ = CommitDealAudit(rctx, e.mem, dr, false)
		}
	}
	if e.mem != nil && e.mem.DB() != nil {
		_ = FulfillRevealOwed(e.mem.DB(), dealID)
	}
	e.persistDealRecord(store.DealRecord{
		DealID:          dealID,
		Role:            "buyer",
		Status:          "settled",
		Shard:           targetShard,
		DealReceiptJSON: dealReceiptJSON,
		PaymentTxID:     paymentTxID,
		ClaimKeyHex:     sig.ContentKeyHex,
		DeliveredText:   deliveredText,
		Error:           "",
	})
	if br := e.getBroker(); br != nil {
		br.notify("deal_settled", map[string]any{"role": "buyer", "dealId": dealID, "txid": paymentTxID, "sats": chosen.PriceSats, "shard": targetShard})
	}
	return buyerVerifyResult{NowUnix: now, DeliveredText: deliveredText, TargetShard: targetShard, DealReceipt: dealReceipt}, nil
}
