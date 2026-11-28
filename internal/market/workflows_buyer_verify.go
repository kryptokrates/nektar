package market

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/reputation"
	"github.com/kryptokrates/nektar/internal/store"
)

func (a *workflowActivities) BuyerVerifyAndCommit(
	ctx context.Context,
	dealID string,
	want bstack.WantAd,
	targetShard string,
	chosen bstack.OfferAd,
	paymentTxID string,
	matchAudit *MatchAudit,
	sig ClaimSignal,
) (buyerVerifyResult, error) {
	return a.eng.buyerVerifyAndCommit(ctx, dealID, want, targetShard, chosen, paymentTxID, matchAudit, sig)
}

// recoverLateBuyerClaim settles an expired-wait buyer deal whose funds are live on-chain (`PaymentTxID != ""`)
// when a late seller `reveal_key` or `EscrowGuardian` on-chain key extraction arrives (STATE-3).
func (e *Engine) recoverLateBuyerClaim(ctx context.Context, existing store.DealRecord, sig ClaimSignal) (bool, error) {
	if existing.Status == "settled" {
		return true, nil
	}
	var want bstack.WantAd
	if len(existing.WantJSON) > 0 {
		_ = json.Unmarshal(existing.WantJSON, &want)
	}
	var chosen bstack.OfferAd
	if len(existing.ChosenOfferJSON) > 0 {
		_ = json.Unmarshal(existing.ChosenOfferJSON, &chosen)
	}
	if chosen.OfferID == "" && len(chosen.SaleOffer) == 0 {
		return false, fmt.Errorf("market: late claim for deal %s missing persisted ChosenOfferJSON", existing.DealID)
	}
	var matchAudit *MatchAudit
	if len(existing.MatchAuditJSON) > 0 {
		var ma MatchAudit
		if json.Unmarshal(existing.MatchAuditJSON, &ma) == nil {
			matchAudit = &ma
		}
	}
	res, err := e.buyerVerifyAndCommit(ctx, existing.DealID, want, existing.Shard, chosen, existing.PaymentTxID, matchAudit, sig)
	if err != nil {
		return false, err
	}
	if res.FailedReason != "" {
		return false, fmt.Errorf("market: late claim verification failed for %s: %s", existing.DealID, res.FailedReason)
	}
	return true, nil
}

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
	plaintext, resolvedKeyHex, anchorMemo, failReason := verifySaleDelivery(
		rctx, e.mem, disc, e.policy, dealID, chosen,
		sig.ContentKeyHex, sig.ClaimTxBEEFHex, sig.CiphertextHex, sig.ProofBundle,
	)
	if failReason != "" {
		return buyerVerifyResult{NowUnix: now, FailedReason: failReason}, nil
	}
	sig.ContentKeyHex = resolvedKeyHex
	isCapsuleDelivery := memrepo.IsCapsuleBytes(plaintext) || strings.EqualFold(strings.TrimSpace(chosen.PayloadFormat), "nkt")
	var verifiedCap *memrepo.MemoryCapsule
	var verifiedCapRes *memrepo.CapsuleVerifyReport
	if isCapsuleDelivery {
		capOpts := memrepo.CapsuleVerifyOpts{
			Objects:    e.mem.Objects(),
			Tracker:    e.mem.Chain().Tracker(),
			MemoryMode: e.mem.Chain().Network() == "memory",
		}
		if anchorMemo != nil && anchorMemo.State != nil && sig.ProofBundle != nil && len(sig.ProofBundle.AnchorBEEF) > 0 {
			capOpts.PreverifiedAnchor = &memrepo.PreverifiedAnchorHint{
				TxID:        anchorMemo.AnchorTxID,
				BeefSHA256:  sha256.Sum256(sig.ProofBundle.AnchorBEEF),
				State:       anchorMemo.State,
				Settled:     anchorMemo.Result.Settled,
				BlockHeight: anchorMemo.Result.BlockHeight,
			}
		}
		decCap, capVer, cErr := memrepo.VerifyCapsuleStream(rctx, bytes.NewReader(plaintext), capOpts)
		if cErr != nil || capVer == nil || !capVer.OK {
			reason := "invalid .nkt capsule payload"
			if cErr != nil {
				reason = cErr.Error()
			} else if capVer != nil && capVer.Reason != "" {
				reason = capVer.Reason
			}
			return buyerVerifyResult{NowUnix: now, FailedReason: "delivered .nkt capsule verification failed: " + reason}, nil
		}
		verifiedCap = decCap
		verifiedCapRes = capVer
		if len(decCap.Fragments) > 0 {
			deliveredText = string(decCap.Fragments[0].Data)
		} else {
			for _, b := range decCap.Blobs {
				if !b.Redacted && !b.Encrypted && len(b.Inline) > 0 {
					deliveredText = string(b.Inline)
					break
				}
			}
		}
	} else {
		deliveredText = string(plaintext)
	}

	claimedNMU := offerNMUSpec(chosen)
	claimedOntID := strings.ToLower(strings.TrimSpace(chosen.OntologyID))
	if claimedOntID == "" {
		claimedOntID = strings.ToLower(strings.TrimSpace(want.OntologyID))
	}
	deliveredTriples := extractTriplesFromDeliveredText(deliveredText)
	var allowedPredicates map[string]struct{}
	if claimedOntID != "" && e.mem != nil {
		allowedPredicates = e.mem.ResolveOntologyPredicateSet(rctx, claimedOntID)
	}
	mangleConsistent := true
	if len(deliveredTriples) > 0 && claimedOntID != "" && e.mem != nil {
		if loaded, lErr := e.mem.ResolveOntology(rctx, claimedOntID); lErr == nil && loaded != nil && len(loaded.Rules) > 0 {
			var atoms []string
			for _, tr := range deliveredTriples {
				if ta, tErr := tr.ToMangleAtoms(); tErr == nil {
					atoms = append(atoms, ta...)
				}
			}
			if rep, cErr := reasoning.CheckOntologyConsistency(atoms, loaded.Rules); cErr != nil || (rep != nil && !rep.Consistent) {
				mangleConsistent = false
			}
		}
	}
	// ARCH-F12 fix: NEVER assume bondVerified=true when BondBEEFHex is missing!
	bondVerified := false
	if chosen.BondOutpoint != "" && chosen.BondBEEFHex != "" && e.mem != nil && e.mem.Chain() != nil {
		bondVerified = VerifyOfferBondSats(rctx, chosen, e.mem.Chain()) > 0
	}
	ontMismatch := want.OntologyID != "" && chosen.OntologyID != "" && !strings.EqualFold(want.OntologyID, chosen.OntologyID)
	gradeVer := nmu.VerifyDeliveredGrade(claimedNMU, nmu.DeliveryObservation{
		HasCleanText:           strings.TrimSpace(deliveredText) != "",
		DistilledOrStructured:  len(deliveredTriples) > 0 || chosen.EdgesRoot != "" || claimedOntID != "",
		OntologyID:             claimedOntID,
		OntologyIDMismatch:     ontMismatch,
		Triples:                deliveredTriples,
		AllowedPredicates:      allowedPredicates,
		MangleConsistent:       mangleConsistent,
		EdgesRoot:              chosen.EdgesRoot,
		SPVVerified:            sig.ProofBundle != nil,
		MPEHasOntologyAndEdges: len(claimedOntID) == 64 && len(chosen.EdgesRoot) == 64,
		ActiveBondVerified:     bondVerified,
		NowUnix:                now,
	})

	fj := e.FastJournal()
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
		var targetOntology string
		if !gradeVer.QuarantineEdges {
			for _, cand := range []string{strings.TrimSpace(chosen.OntologyID), strings.TrimSpace(want.OntologyID)} {
				if cand != "" {
					if _, _, lErr := e.mem.LoadDomainOntology(rctx, cand); lErr == nil {
						targetOntology = cand
						break
					}
				}
			}
		}
		storedNMU := &nmu.Spec{
			Version:        nmu.SpecVersion,
			Grade:          gradeVer.AchievedGrade,
			UnitCount:      gradeVer.ActualUnits,
			FreshnessClass: claimedNMU.FreshnessClass,
			ValidUntilUnix: claimedNMU.ValidUntilUnix,
			SubjectVersion: claimedNMU.SubjectVersion,
			UsageRights:    nmu.EffectiveRights(claimedNMU.UsageRights),
			PricingMode:    nmu.EffectivePricingMode(claimedNMU.PricingMode),
			FeedURL:        claimedNMU.FeedURL,
			AdvisoryNote:   nmu.AdvisoryNMUNote,
		}
		if isCapsuleDelivery {
			var (
				impRes *memory.CapsuleImportResult
				impErr error
			)
			if verifiedCap != nil && verifiedCapRes != nil && verifiedCapRes.OK {
				impRes, impErr = e.mem.ImportVerifiedCapsule(rctx, verifiedCap, verifiedCapRes, targetShard, memrepo.DefaultBranch, true, nil)
			} else {
				impRes, impErr = e.mem.ImportCapsuleStream(rctx, bytes.NewReader(plaintext), targetShard, memrepo.DefaultBranch, true, nil, nil)
			}
			if impErr != nil {
				return buyerVerifyResult{}, fmt.Errorf("market: import delivered .nkt capsule into %s: %w", targetShard, impErr)
			}
			// Persist NMU-1 post-reveal grade/quarantine metadata on every imported fragment row.
			if fj == nil && e.mem.DB() != nil && storedNMU != nil {
				for _, sq := range impRes.ImportedSeqs {
					_ = e.mem.DB().SaveFragmentNMUMeta(store.FragmentNMUMeta{
						Shard:          targetShard,
						Seq:            sq,
						Grade:          string(storedNMU.Grade),
						UnitCount:      storedNMU.UnitCount,
						FreshnessClass: string(storedNMU.FreshnessClass),
						ValidUntilUnix: storedNMU.ValidUntilUnix,
						SubjectVersion: storedNMU.SubjectVersion,
						UsageRights:    string(nmu.EffectiveRights(storedNMU.UsageRights)),
						GradeMismatch:  gradeVer.GradeMismatch,
						UpdatedAt:      now,
					})
				}
			}
			if fj == nil && e.mem.Chain().Network() == "memory" && os.Getenv("NEKTAR_FAST_DEALS") != "1" && e.mem.Chain().BalanceSats() >= 250 && impRes.ImportedFrags > 0 {
				_, _ = e.mem.AnchorShard(rctx, targetShard)
			}
		} else {
			if _, rErr := e.mem.Remember(rctx, memory.RememberInput{
				Shard:           targetShard,
				Path:            fmt.Sprintf("market/%s-%s-%d.md", dom, shortPub(chosen.Pub), now),
				MediaType:       "text/markdown",
				Text:            deliveredText,
				CommitMessage:   fmt.Sprintf("Market import (%s, %d sats from %s, grade %s)", dom, chosen.PriceSats, shortPub(chosen.Pub), gradeVer.AchievedGrade),
				ExtraParents:    extraParents,
				Source:          "market:" + shortPub(chosen.Pub),
				Class:           "market:" + dom,
				Ontology:        targetOntology,
				NMU:             storedNMU,
				GradeMismatch:   gradeVer.GradeMismatch,
				QuarantineEdges: gradeVer.QuarantineEdges,
				Tainted:         true, // F10: purchased external knowledge is ALWAYS marked tainted!
				AutoAnchor:      fj == nil && e.mem.Chain().Network() == "memory" && os.Getenv("NEKTAR_FAST_DEALS") != "1" && e.mem.Chain().BalanceSats() >= 250,
			}); rErr != nil {
				return buyerVerifyResult{}, rErr
			}
		}
	}

	if fj == nil && e.mem != nil && e.mem.DB() != nil && chosen.Pub != "" {
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
			if (dr.DealID != dealID && (chosen.WantID == "" || dr.DealID != chosen.WantID)) ||
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
			if !claimedNMU.IsEmpty() {
				cp := claimedNMU
				dr.NMU = &cp
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
			if hasSellerReceipt && fj == nil {
				if bvr := VerifyDealReceipt(dr, true); !bvr.OK || !bvr.Bilateral {
					return buyerVerifyResult{NowUnix: now, FailedReason: "bilateral deal receipt verification failed: " + bvr.Error}, nil
				}
			}
			dealReceipt = &dr
			dealReceiptJSON, _ = json.Marshal(dr)
			if fj == nil {
				_, _, _ = CommitDealAudit(rctx, e.mem, dr, false)
			}
		}
	}

	settledRec := store.DealRecord{
		DealID:          dealID,
		Role:            "buyer",
		Status:          "settled",
		Shard:           targetShard,
		DealReceiptJSON: dealReceiptJSON,
		PaymentTxID:     paymentTxID,
		ClaimKeyHex:     sig.ContentKeyHex,
		DeliveredText:   deliveredText,
		CounterpartyPub: chosen.Pub,
		PriceSats:       chosen.PriceSats,
		Error:           "",
		UpdatedAt:       now,
	}
	if fj != nil {
		// Synchronous group-committed Pebble WAL commit for verified .nkt capsule + settled deal (<1ms, crash-safe);
		// go-git / SQLite / reputation projection runs on the async background worker.
		if err := fj.CommitBuyerSettledWAL(asyncBuyerCommitJob{
			dealID:      dealID,
			targetShard: targetShard,
			verifiedCap: verifiedCap,
			capReport:   verifiedCapRes,
			plaintext:   plaintext,
			dealRec:     settledRec,
			sellerPub:   chosen.Pub,
			priceSats:   chosen.PriceSats,
		}); err != nil {
			return buyerVerifyResult{}, err
		}
		e.publishDealEvent(dealID, "settled")
	} else if e.mem != nil && e.mem.DB() != nil {
		db := e.mem.DB()
		// RCA-2 fix: atomically commit settled deal + fulfill reveal_owed/refund_claimable obligations and return any DB error!
		if err := db.SettleDealAndFulfillObligations(
			mergeDealOrNew(db, settledRec),
			[]string{ObligationID(dealID, ObligationRevealOwed), ObligationID(dealID, ObligationRefundClaimable)},
			nil,
		); err != nil {
			return buyerVerifyResult{}, err
		}
		e.publishDealEvent(dealID, "settled")

		estSaved := int64(DefaultTokenCounter().CountTokens(deliveredText)) * 8
		if estSaved < 500 {
			estSaved = 500
		}
		_ = db.RecordEconomicsEvent(store.EconomicsEventRow{
			Kind:        "deal_buy",
			TokensSaved: estSaved,
			SatsDelta:   -int64(chosen.PriceSats),
			OntologyID:  reputation.NormalizeOntologyID(chosen.OntologyID),
			Shard:       targetShard,
			DealID:      dealID,
			CreatedAt:   now,
		})
		if dealReceipt != nil && chosen.Pub != "" {
			ontID := chosen.OntologyID
			if ontID == "" {
				ontID = want.OntologyID
			}
			scores := reputation.EvaluatePurchasedFragment(reputation.BuyerEvalInput{
				SellerPub:             chosen.Pub,
				OntologyID:            ontID,
				SPVVerified:           sig.ProofBundle != nil,
				MPEVerified:           true,
				HasEvidenceSpans:      chosen.EdgesRoot != "",
				HasPinnedOntology:     ontID != "",
				SellerBonded:          bondVerified,
				ContradictionDetected: !mangleConsistent,
				GroundedTriples:       int(gradeVer.ActualUnits),
				GradeMismatch:         gradeVer.GradeMismatch,
				SlashAccCapBP:         gradeVer.SlashAccCapBP,
				SlashDispCapBP:        gradeVer.SlashDispCapBP,
				SlashProvCapBP:        gradeVer.SlashProvCapBP,
			})
			_, _ = e.EmitPostDealAttestation(rctx, reputation.RoleBuyerRatesSeller, *dealReceipt, ontID, scores, want.Domains)
		}
	}
	if br := e.getBroker(); br != nil {
		br.notify("deal_settled", map[string]any{"role": "buyer", "dealId": dealID, "txid": paymentTxID, "sats": chosen.PriceSats, "shard": targetShard})
	}
	return buyerVerifyResult{NowUnix: now, DeliveredText: deliveredText, TargetShard: targetShard, DealReceipt: dealReceipt}, nil
}

func offerNMUSpec(o bstack.OfferAd) nmu.Spec {
	return nmu.Spec{
		Version:         nmu.SpecVersion,
		Grade:           nmu.NormalizeGrade(o.Grade),
		UnitCount:       o.UnitCount,
		SatsPer1kUnits:  o.SatsPer1kUnits,
		FreshnessClass:  nmu.NormalizeFreshness(o.FreshnessClass),
		ValidUntilUnix:  o.ValidUntilUnix,
		SubjectVersion:  strings.TrimSpace(o.SubjectVersion),
		UsageRights:     nmu.NormalizeRights(o.UsageRights),
		PricingMode:     nmu.NormalizePricingMode(o.PricingMode),
		SubscriptionSec: o.SubscriptionSec,
		FeedURL:         strings.TrimSpace(o.FeedURL),
	}
}

func extractTriplesFromDeliveredText(text string) []reasoning.Triple {
	var out []reasoning.Triple
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if atom, ok := reasoning.ParseGroundFact(line); ok && atom.Predicate.Symbol == "triple" && len(atom.Args) == 3 {
			unq := func(raw string) string {
				if s, err := strconv.Unquote(raw); err == nil {
					return s
				}
				return strings.TrimSpace(raw)
			}
			if fin, err := reasoning.FinalizeTriple(reasoning.Triple{
				Subject:   unq(atom.Args[0].String()),
				Predicate: unq(atom.Args[1].String()),
				Object:    unq(atom.Args[2].String()),
			}); err == nil {
				out = append(out, fin)
			}
			continue
		}
		if tr, ok := reasoning.ParseNTripleLine(line); ok {
			out = append(out, tr)
		}
	}
	return out
}
