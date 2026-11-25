package memory

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

// GroundedAnswerInput requests a grounded, receipt-backed answer from claims citing memory fragments.
type GroundedAnswerInput struct {
	Claims       []reasoning.Claim         `json:"claims"`
	Mode         string                    `json:"mode,omitempty"` // remove (default) | flag
	Principal    policy.Principal          `json:"principal,omitempty"`
	CallerPub    string                    `json:"callerPub,omitempty"`
	DisallowBorg bool                      `json:"-"`
	Delegations  []reasoning.Delegation    `json:"delegations,omitempty"`
	Payment      *reasoning.PaymentSummary `json:"payment,omitempty"`
	AutoAnchor   bool                      `json:"autoAnchor,omitempty"`
}

// GroundedAnswerResult carries the grounded answer text and auditable receipt footer.
type GroundedAnswerResult struct {
	Answer       string                     `json:"answer"`
	Ungrounded   bool                       `json:"ungrounded"`
	ReceiptHash  string                     `json:"receiptHash,omitempty"`
	ReceiptToken string                     `json:"receiptToken,omitempty"`
	VerifyURL    string                     `json:"verifyUrl,omitempty"`
	Footer       string                     `json:"footer,omitempty"`
	Report       *reasoning.GroundingReport `json:"report"`
	Receipt      *reasoning.Receipt         `json:"receipt,omitempty"`
	AnchorTxID   string                     `json:"anchorTxid,omitempty"`
}

// GroundedAnswer verifies claims against authorized fragments, strips ungrounded claims,
// commits the receipt to __receipts__, and returns the nektarr1.* receipt footer.
func (s *Service) GroundedAnswer(ctx context.Context, in GroundedAnswerInput) (*GroundedAnswerResult, error) {
	principal := in.Principal
	if principal.ID == "" {
		principal = policy.Principal{Type: "Agent", ID: "owner", Level: 100}
	}
	callerPub := in.CallerPub
	if callerPub == "" && !in.DisallowBorg {
		callerPub = s.port.IdentityPubHex()
	}

	s.mu.RLock()
	polText := s.policyText
	rules := append([]string(nil), s.rules...)
	s.mu.RUnlock()

	fetch := func(ref reasoning.FragmentRef) (bool, string) {
		r, err := s.db.GetFragment(ref.Shard, ref.Seq)
		if err != nil || r.Forgotten {
			return false, ""
		}
		// Never ground new answers on superseded facts!
		if newerSeq, sErr := s.db.SupersededBy(ref.Shard, ref.Seq); sErr == nil && newerSeq > 0 {
			return false, ""
		}
		dec, err := policy.Authorize(polText, principal, "read", policy.Resource{
			Type:  "Fragment",
			ID:    fmt.Sprintf("%s#%d", r.Shard, r.Seq),
			Level: int64(r.Level),
			Shard: r.Shard,
			Class: r.Class,
		})
		if err != nil || !dec.Allow {
			return false, ""
		}
		if r.BorgGroup != "" {
			if in.DisallowBorg || callerPub == "" {
				return false, ""
			}
			var env bstack.SealedEnvelope
			if err := json.Unmarshal(r.KeyedMeta, &env); err != nil {
				return false, ""
			}
			epochSym, err := s.groups.EpochKey(r.BorgGroup, callerPub, r.BorgEpoch)
			if err != nil {
				return false, ""
			}
			pt, err := bstack.OpenWithEpoch(&env, epochSym)
			if err != nil {
				return false, ""
			}
			return true, string(pt)
		}
		return true, string(r.Data)
	}

	report, err := reasoning.Ground(in.Claims, fetch, in.Mode, rules)
	if err != nil {
		return nil, err
	}

	// Build grounded answer strictly from kept grounded claims.
	var parts []string
	seenRefs := make(map[reasoning.FragmentRef]bool)
	var refs []reasoning.FragmentRef
	for _, gc := range report.Claims {
		if !gc.Grounded {
			continue
		}
		parts = append(parts, gc.Text)
		for _, r := range gc.Cites {
			if !seenRefs[r] {
				seenRefs[r] = true
				refs = append(refs, r)
			}
		}
		for _, q := range gc.Quotes {
			r := reasoning.FragmentRef{Shard: q.Shard, Seq: q.Seq}
			if !seenRefs[r] {
				seenRefs[r] = true
				refs = append(refs, r)
			}
		}
	}

	// Honesty rule: if nothing grounded, never emit a receipt footer!
	if len(parts) == 0 {
		return &GroundedAnswerResult{
			Answer:     "",
			Ungrounded: true,
			Report:     report,
		}, nil
	}

	answer := strings.Join(parts, " ")
	rcp := reasoning.Receipt{
		Answer:              answer,
		FragmentRefs:        refs,
		CheckResult:         "verified",
		ModelID:             s.modelID,
		Timestamp:           time.Now().Unix(),
		GroundingReportHash: report.ReportHash,
		GroundingTier:       "mangle+quote",
		TruthNote:           reasoning.TruthNote,
		Delegations:         in.Delegations,
		Payment:             in.Payment,
	}
	if err := reasoning.VerifyDelegations(rcp); err != nil {
		return nil, err
	}
	// F-32: sign the receipt under core.TagReceiptSig using the node's BRC-43 signing key (matching SignDigest).
	if signPub, pErr := s.port.SigningPubKey(ctx); pErr == nil && signPub != nil {
		rcp.IssuerPub = hex.EncodeToString(signPub.Compressed())
		if signDigest, dErr := reasoning.ReceiptSignDigest(rcp); dErr == nil {
			if issuerSig, sErr := s.port.SignDigest(ctx, signDigest); sErr == nil && len(issuerSig) > 0 {
				rcp.IssuerSig = hex.EncodeToString(issuerSig)
			} else {
				rcp.IssuerPub = ""
			}
		} else {
			rcp.IssuerPub = ""
		}
	}
	rcpHash, err := reasoning.ReceiptHash(rcp)
	if err != nil {
		return nil, err
	}
	tok, err := reasoning.ReceiptToken(rcpHash)
	if err != nil {
		return nil, err
	}
	bodyJSON, err := core.CanonicalJSON(rcp)
	if err != nil {
		return nil, err
	}

	// Atomically commit receipt fragment (__receipts__), receipts index, and citation edges in ONE SQLite tx!
	now := rcp.Timestamp
	prov := core.Provenance{
		Source:  "receipt",
		Time:    now,
		ModelID: s.modelID.Provider + "/" + s.modelID.Release,
	}
	sigBytes, err := s.port.SignDigest(ctx, core.ProvenanceDigest(bodyJSON, prov))
	if err != nil {
		return nil, fmt.Errorf("memory: sign receipt provenance: %w", err)
	}
	prov.Signature = sigBytes
	provBytes := core.SerializeProvenance(prov)
	hdp := core.ComputeHDPV2(bodyJSON, prov)
	leaf := core.ComputeLeaf(core.Fragment{Data: bodyJSON, Provenance: prov, HDP: hdp})
	fragRec := store.FragmentRecord{
		Shard:      ReceiptsShard,
		HDP:        core.ToHex(hdp),
		Leaf:       core.ToHex(leaf),
		Data:       bodyJSON,
		Provenance: provBytes,
		Class:      "receipt",
		BatchN:     -1,
		LeafIndex:  -1,
		CreatedAt:  now,
	}
	cites := make([]store.CitationEdge, 0, len(refs))
	for _, r := range refs {
		cites = append(cites, store.CitationEdge{Shard: r.Shard, Seq: r.Seq, ReceiptHash: rcpHash})
	}
	storedFrag, err := s.db.CommitReceiptAtomic(fragRec, store.ReceiptRecord{
		ReceiptHash: rcpHash,
		BodyJSON:    bodyJSON,
		CreatedAt:   now,
	}, cites)
	if err != nil {
		return nil, err
	}
	var anchorTxID string
	if in.AutoAnchor {
		if a, aErr := s.AnchorShard(ctx, ReceiptsShard); aErr != nil {
			return nil, aErr
		} else if a != nil {
			anchorTxID = a.TxID
			_ = s.db.UpdateReceiptAnchor(storedFrag.Seq, anchorTxID)
		}
	}

	verifyURL := s.verifyBaseURL + "/" + tok
	footer := fmt.Sprintf("receipt: %s\nverify: %s", tok, verifyURL)

	return &GroundedAnswerResult{
		Answer:       answer,
		Ungrounded:   false,
		ReceiptHash:  rcpHash,
		ReceiptToken: tok,
		VerifyURL:    verifyURL,
		Footer:       footer,
		Report:       report,
		Receipt:      &rcp,
		AnchorTxID:   anchorTxID,
	}, nil
}

// ReceiptVerification is the traffic-light verdict returned by VerifyReceipt and GET /v1/verify/{token}.
type ReceiptVerification struct {
	OK                   bool               `json:"ok"`
	Verdict              string             `json:"verdict"` // green | amber | red
	Reason               string             `json:"reason"`
	ReceiptHash          string             `json:"receiptHash"`
	ReceiptToken         string             `json:"receiptToken"`
	Receipt              *reasoning.Receipt `json:"receipt,omitempty"`
	ReceiptAnchored      bool               `json:"receiptAnchored"`
	CitationsChecked     int                `json:"citationsChecked"`
	CitationsMined       int                `json:"citationsMined"`
	CitationsMissing     int                `json:"citationsMissing"`
	CitationsInvalid     int                `json:"citationsInvalid,omitempty"`
	CitationsUnavailable int                `json:"citationsUnavailable,omitempty"`
	CitationsSuperseded  int                `json:"citationsSuperseded,omitempty"`
	SupersededRefs       []string           `json:"supersededRefs,omitempty"`
	TruthNote            string             `json:"truthNote"`
}

// VerifyReceipt validates a nektarr1.* token or 64-hex hash, its §16 delegations, and its cited SPV proofs.
func (s *Service) VerifyReceipt(ctx context.Context, tokenOrHash string) (*ReceiptVerification, error) {
	h, err := reasoning.ParseReceiptToken(tokenOrHash)
	if err != nil {
		return &ReceiptVerification{Verdict: "red", Reason: err.Error(), TruthNote: reasoning.TruthNote}, nil
	}
	tok, _ := reasoning.ReceiptToken(h)
	rec, err := s.db.GetReceipt(h)
	if err != nil {
		return &ReceiptVerification{
			Verdict:      "red",
			Reason:       "receipt not found",
			ReceiptHash:  h,
			ReceiptToken: tok,
			TruthNote:    reasoning.TruthNote,
		}, nil
	}
	var rcp reasoning.Receipt
	if err := json.Unmarshal(rec.BodyJSON, &rcp); err != nil {
		return &ReceiptVerification{Verdict: "red", Reason: "corrupt receipt JSON", ReceiptHash: h, ReceiptToken: tok, TruthNote: reasoning.TruthNote}, nil
	}
	recomputed, err := reasoning.ReceiptHash(rcp)
	if err != nil || recomputed != h {
		return &ReceiptVerification{Verdict: "red", Reason: "receipt hash mismatch", ReceiptHash: h, ReceiptToken: tok, TruthNote: reasoning.TruthNote}, nil
	}
	if err := reasoning.VerifyDelegations(rcp); err != nil {
		return &ReceiptVerification{Verdict: "red", Reason: err.Error(), ReceiptHash: h, ReceiptToken: tok, Receipt: &rcp, TruthNote: reasoning.TruthNote}, nil
	}
	if err := reasoning.VerifyReceiptSignature(rcp); err != nil {
		return &ReceiptVerification{Verdict: "red", Reason: err.Error(), ReceiptHash: h, ReceiptToken: tok, Receipt: &rcp, TruthNote: reasoning.TruthNote}, nil
	}

	// Derive ReceiptAnchored strictly from settled=1 anchors (with embedded BUMP), never bare anchor_txid != "".
	anchorTxID := rec.AnchorTxID
	if anchorTxID == "" && rec.ShardSeq > 0 {
		if rf, rfErr := s.db.GetFragment(ReceiptsShard, rec.ShardSeq); rfErr == nil && rf.AnchorTxID != "" {
			anchorTxID = rf.AnchorTxID
		}
	}
	receiptAnchored := false
	if anchorTxID != "" {
		if settled, sErr := s.db.IsAnchorSettled(anchorTxID); sErr == nil && settled {
			receiptAnchored = true
		}
	}
	out := &ReceiptVerification{
		ReceiptHash:     h,
		ReceiptToken:    tok,
		Receipt:         &rcp,
		ReceiptAnchored: receiptAnchored,
		TruthNote:       reasoning.TruthNote,
	}
	type batchKey struct {
		shard string
		n     int64
	}
	spvMemos := make(map[batchKey]*contracts.AnchorBatchMemo)
	for _, ref := range rcp.FragmentRefs {
		out.CitationsChecked++
		f, fErr := s.db.GetFragment(ref.Shard, ref.Seq)
		if fErr != nil || f.Forgotten {
			out.CitationsMissing++
			continue
		}
		if newerSeq, sErr := s.db.SupersededBy(ref.Shard, ref.Seq); sErr == nil && newerSeq > 0 {
			out.CitationsSuperseded++
			out.SupersededRefs = append(out.SupersededRefs, fmt.Sprintf("%s#%d->#%d", ref.Shard, ref.Seq, newerSeq))
		}
		if f.BatchN >= 0 {
			bk := batchKey{shard: ref.Shard, n: f.BatchN}
			_, vr, nextMemo, pErr := s.proveWithMemo(ctx, ref.Shard, ref.Seq, policy.Principal{Type: "Agent", ID: "owner", Level: 100}, spvMemos[bk])
			if nextMemo != nil {
				spvMemos[bk] = nextMemo
			}
			if pErr != nil || vr == nil {
				out.CitationsInvalid++
			} else if vr.OK && vr.Settled {
				out.CitationsMined++
			} else if !vr.OK {
				switch vr.Reason {
				case "Unsettled", "InsufficientDepth":
					// Valid structurally, awaiting mining/confirmation depth -> amber.
				case "TrackerUnavailable":
					// Header witness transport/quorum outage -> amber (never red forgery!).
					out.CitationsUnavailable++
				default:
					out.CitationsInvalid++
				}
			}
		}
	}

	switch {
	case out.CitationsInvalid > 0:
		out.Verdict = "red"
		out.OK = false
		out.Reason = fmt.Sprintf("%d cited fragment(s) failed cryptographic SPV proof verification", out.CitationsInvalid)
	case out.CitationsMissing > 0:
		out.Verdict = "red"
		out.OK = false
		out.Reason = fmt.Sprintf("%d cited fragment(s) missing or forgotten", out.CitationsMissing)
	case out.CitationsUnavailable > 0:
		out.Verdict = "amber"
		out.OK = true
		out.Reason = fmt.Sprintf("header tracker unavailable while verifying %d cited fragment(s); retryable", out.CitationsUnavailable)
	case out.CitationsSuperseded > 0:
		out.Verdict = "amber"
		out.OK = true
		out.Reason = fmt.Sprintf("%d cited fragment(s) were superseded by a newer revision; re-grounding recommended", out.CitationsSuperseded)
	case out.CitationsChecked > 0 && out.CitationsMined == out.CitationsChecked:
		out.Verdict = "green"
		out.OK = true
		out.Reason = "all cited fragments SPV-verified against mined anchors"
	default:
		out.Verdict = "amber"
		out.OK = true
		out.Reason = "receipt valid locally; one or more cited fragments are not yet mined"
	}
	return out, nil
}

// OnRevision returns receipt hashes citing (shard, seq) so downstream consumers can re-ground on edits.
func (s *Service) OnRevision(_ context.Context, shard string, seq int64) ([]string, error) {
	return s.db.CitingReceipts(shard, seq)
}
