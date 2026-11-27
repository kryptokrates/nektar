package memory

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/jevgraph"
	"github.com/kryptokrates/nektar/internal/lattice"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

// GroundedAnswerInput requests a grounded, receipt-backed answer from claims citing memory fragments.
type GroundedAnswerInput struct {
	Claims            []reasoning.Claim         `json:"claims"`
	Mode              string                    `json:"mode,omitempty"` // remove (default) | flag
	Principal         policy.Principal          `json:"principal,omitempty"`
	Clearance         *lattice.Clearance        `json:"clearance,omitempty"`
	CallerPub         string                    `json:"callerPub,omitempty"`
	DisallowBorg      bool                      `json:"-"`
	DisallowPrivate   bool                      `json:"-"`
	RequireSettledSPV bool                      `json:"requireSettledSpv,omitempty"`
	Delegations       []reasoning.Delegation    `json:"delegations,omitempty"`
	Payment           *reasoning.PaymentSummary `json:"payment,omitempty"`
	AutoAnchor        bool                      `json:"autoAnchor,omitempty"`
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

// verifyStoredFragmentLeaf checks that r's stored Leaf cryptographically matches unified core.ComputeLeaf.
func verifyStoredFragmentLeaf(r store.FragmentRecord) bool {
	hdpBytes, err := hex.DecodeString(strings.TrimSpace(r.HDP))
	if err != nil || len(hdpBytes) != 32 {
		return false
	}
	mpeBytes, err := hex.DecodeString(strings.TrimSpace(r.MPERoot))
	if err != nil || len(mpeBytes) != 32 {
		return false
	}
	var contentBytes, commitBytes, transBytes []byte
	if strings.TrimSpace(r.ContentSHA256) != "" {
		var cErr error
		contentBytes, cErr = hex.DecodeString(strings.TrimSpace(r.ContentSHA256))
		if cErr != nil || len(contentBytes) != 32 {
			return false
		}
	}
	if strings.TrimSpace(r.CommitHash) != "" {
		if cb, cErr := hex.DecodeString(strings.TrimSpace(r.CommitHash)); cErr == nil && len(cb) == 32 {
			commitBytes = cb
		}
	}
	if strings.TrimSpace(r.TransitionHash) != "" {
		if tb, tErr := hex.DecodeString(strings.TrimSpace(r.TransitionHash)); tErr == nil && len(tb) == 32 {
			transBytes = tb
		}
	}
	want := core.ToHex(core.ComputeLeaf(core.Fragment{
		HDP:            hdpBytes,
		Ciphertext:     r.Ciphertext,
		MPERoot:        mpeBytes,
		ContentSHA256:  contentBytes,
		CommitHash:     commitBytes,
		TransitionHash: transBytes,
	}))
	return strings.EqualFold(want, r.Leaf)
}

func collectDerivationRuleRefs(step *reasoning.DerivationStep, seen map[reasoning.FragmentRef]bool, out *[]reasoning.FragmentRef) {
	if step == nil {
		return
	}
	if step.RuleRef != nil && step.RuleRef.Shard != "" && step.RuleRef.Seq > 0 {
		if !seen[*step.RuleRef] {
			seen[*step.RuleRef] = true
			*out = append(*out, *step.RuleRef)
		}
	}
	for _, p := range step.Premises {
		if p.Step != nil {
			collectDerivationRuleRefs(p.Step, seen, out)
		}
	}
}

// GroundedAnswer verifies claims against authorized, cryptographically verified fragments,
// evaluates committed __ontology__ Mangle rules with proof-carrying derivations, strips ungrounded claims,
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
	polCommitHash := ""
	if s.policyCommit != nil {
		polCommitHash = s.policyCommit.Hash
	}
	var polRef *reasoning.FragmentRef
	if s.policyRef != nil {
		cp := *s.policyRef
		polRef = &cp
	}
	ontHead := s.ontologyHead
	committedRules := make([]reasoning.CommittedRule, len(s.committedRules))
	copy(committedRules, s.committedRules)
	s.mu.RUnlock()

	// Collect any committed domain ontologies & Bridge Shards referenced by cited fragments' accepted JevGraph edges.
	citedOntIDs := make(map[string]bool)
	for _, c := range in.Claims {
		for _, ref := range c.Cites {
			if oid, err := s.db.FragmentOntologyID(ref.Shard, ref.Seq); err == nil && oid != "" {
				citedOntIDs[strings.ToLower(oid)] = true
			}
		}
	}
	if len(citedOntIDs) > 0 {
		seenRH := make(map[string]bool, len(committedRules))
		for _, cr := range committedRules {
			seenRH[cr.RuleHash] = true
		}
		for _, dr := range s.domainOntologyCommittedRules(ctx, citedOntIDs) {
			if !seenRH[dr.RuleHash] {
				seenRH[dr.RuleHash] = true
				committedRules = append(committedRules, dr)
			}
		}
	}

	type batchKey struct {
		shard string
		n     int64
	}
	spvMemos := make(map[batchKey]*contracts.AnchorBatchMemo)
	supCache := make(map[string]map[int64]int64)
	isSuperseded := func(shard string, seq int64) bool {
		m, ok := supCache[shard]
		if !ok {
			m = s.CollectSupersededSeqs(shard)
			supCache[shard] = m
		}
		return m[seq] > 0
	}

	detailedFetch := func(ref reasoning.FragmentRef) (bool, string, string) {
		r, err := s.db.GetFragment(ref.Shard, ref.Seq)
		if err != nil || r.Forgotten {
			return false, "", ""
		}
		// Never ground new answers on cryptographically superseded facts!
		if isSuperseded(ref.Shard, ref.Seq) {
			return false, "", ""
		}
		dec, err := policy.Authorize(polText, principal, "read", policy.Resource{
			Type:  "Fragment",
			ID:    fmt.Sprintf("%s#%d", r.Shard, r.Seq),
			Level: int64(r.Level),
			Shard: r.Shard,
			Class: r.Class,
		})
		if err != nil || !dec.Allow {
			return false, "", ""
		}
		// Local cryptographic integrity gate: verify unified leaf commitment AND framed v2 HDP!
		if !verifyStoredFragmentLeaf(r) {
			return false, "", ""
		}
		pt, ok := s.openFragmentPlaintext(ctx, r, principal, callerPub, in.Clearance, in.DisallowBorg, in.DisallowPrivate)
		if !ok {
			return false, "", ""
		}
		// Optional strict SPV mode: reject fragments not yet mined & SPV-verified against PoW headers.
		if in.RequireSettledSPV {
			if r.BatchN < 0 {
				return false, "", ""
			}
			bk := batchKey{shard: r.Shard, n: r.BatchN}
			_, vr, nextMemo, pErr := s.proveWithMemo(ctx, r.Shard, r.Seq, ownerPrincipal(), spvMemos[bk])
			if nextMemo != nil {
				spvMemos[bk] = nextMemo
			}
			if pErr != nil || vr == nil || !vr.OK || !vr.Settled {
				return false, "", ""
			}
		}
		textOut := string(pt)
		// Append strictly sanitized ground Mangle atoms from accepted JevGraph edges whose evidence
		// quote is verbatim-verified inside this fragment's plaintext.
		if edges, eErr := s.db.ListKnowledgeEdges(store.KnowledgeEdgeFilter{
			Shard:  ref.Shard,
			Seq:    ref.Seq,
			Status: "accepted",
		}); eErr == nil && len(edges) > 0 {
			var extraLines []string
			for _, edge := range edges {
				if edge.EvidenceQuote != "" && !strings.Contains(textOut, edge.EvidenceQuote) {
					continue
				}
				extraLines = append(extraLines, knowledgeEdgeToMangleAtoms(edge)...)
			}
			if len(extraLines) > 0 {
				textOut = strings.TrimRight(textOut, "\n") + "\n" + strings.Join(extraLines, "\n")
			}
		}
		return true, textOut, r.HDP
	}

	report, err := reasoning.GroundWithProofs(in.Claims, detailedFetch, in.Mode, committedRules)
	if err != nil {
		return nil, err
	}

	// Build grounded answer strictly from kept grounded claims.
	var parts []string
	seenRefs := make(map[reasoning.FragmentRef]bool)
	var refs []reasoning.FragmentRef
	seenRuleRefs := make(map[reasoning.FragmentRef]bool)
	var ruleRefs []reasoning.FragmentRef
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
		if gc.Derivation != nil {
			collectDerivationRuleRefs(gc.Derivation, seenRuleRefs, &ruleRefs)
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

	tier := "mangle+quote"
	if len(report.Derivations) > 0 {
		tier = "mangle+proof+quote"
	}
	answer := strings.Join(parts, " ")
	rcp := reasoning.Receipt{
		Answer:              answer,
		FragmentRefs:        refs,
		CheckResult:         "verified",
		ModelID:             s.modelID,
		Timestamp:           time.Now().Unix(),
		GroundingReportHash: report.ReportHash,
		GroundingTier:       tier,
		PolicyCommitHash:    polCommitHash,
		PolicyRef:           polRef,
		OntologyCommitHash:  ontHead,
		RuleRefs:            ruleRefs,
		Derivations:         report.Derivations,
		TruthNote:           reasoning.TruthNote,
		Delegations:         in.Delegations,
		Payment:             in.Payment,
	}
	if err := reasoning.VerifyDelegations(rcp); err != nil {
		return nil, err
	}
	// F-32: strictly sign every receipt under core.TagReceiptSig using the node's BRC-43 signing key (fail closed).
	signPub, pErr := s.port.SigningPubKey(ctx)
	if pErr != nil || signPub == nil {
		if pErr == nil {
			pErr = errors.New("nil signing public key")
		}
		return nil, fmt.Errorf("memory: load receipt signing key: %w", pErr)
	}
	rcp.IssuerPub = hex.EncodeToString(signPub.Compressed())
	signDigest, dErr := reasoning.ReceiptSignDigest(rcp)
	if dErr != nil {
		return nil, fmt.Errorf("memory: compute receipt sign digest: %w", dErr)
	}
	issuerSig, sErr := s.port.SignDigest(ctx, signDigest)
	if sErr != nil || len(issuerSig) == 0 {
		if sErr == nil {
			sErr = errors.New("empty signature")
		}
		return nil, fmt.Errorf("memory: sign receipt: %w", sErr)
	}
	rcp.IssuerSig = hex.EncodeToString(issuerSig)
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
	hdpHex := core.ToHex(hdp)
	mpe, mErr := buildCommittedFragmentMPE(ReceiptsShard, hdpHex, prov, 0, "receipt", "", "", "", core.ToHex(core.SHA256(bodyJSON)))
	if mErr != nil || mpe == nil {
		return nil, fmt.Errorf("memory: build receipt L1 MPE: %w", mErr)
	}
	leaf := core.ComputeLeaf(core.Fragment{
		Data:       bodyJSON,
		Provenance: prov,
		HDP:        hdp,
		MPERoot:    mpe.Root,
	})
	fragRec := store.FragmentRecord{
		Shard:      ReceiptsShard,
		HDP:        hdpHex,
		Leaf:       core.ToHex(leaf),
		MPERoot:    mpe.RootHex,
		Data:       bodyJSON,
		Provenance: provBytes,
		Class:      "receipt",
		Public:     true,
		BatchN:     -1,
		LeafIndex:  -1,
		CreatedAt:  now,
	}
	cites := make([]store.CitationEdge, 0, len(refs))
	for _, r := range refs {
		cites = append(cites, store.CitationEdge{Shard: r.Shard, Seq: r.Seq, ReceiptHash: rcpHash})
	}
	if _, err := s.db.CommitReceiptAtomic(fragRec, store.ReceiptRecord{
		ReceiptHash: rcpHash,
		BodyJSON:    bodyJSON,
		CreatedAt:   now,
	}, cites); err != nil {
		return nil, err
	}
	var anchorTxID string
	if in.AutoAnchor {
		if a, aErr := s.AnchorShard(ctx, ReceiptsShard); aErr != nil {
			return nil, aErr
		} else if a != nil {
			anchorTxID = a.TxID
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
	PolicyAnchored       bool               `json:"policyAnchored,omitempty"`
	OntologyAnchored     bool               `json:"ontologyAnchored,omitempty"`
	DerivationsVerified  int                `json:"derivationsVerified,omitempty"`
	CitationsChecked     int                `json:"citationsChecked"`
	CitationsMined       int                `json:"citationsMined"`
	CitationsMissing     int                `json:"citationsMissing"`
	CitationsInvalid     int                `json:"citationsInvalid,omitempty"`
	CitationsUnavailable int                `json:"citationsUnavailable,omitempty"`
	CitationsSuperseded  int                `json:"citationsSuperseded,omitempty"`
	SupersededRefs       []string           `json:"supersededRefs,omitempty"`
	TruthNote            string             `json:"truthNote"`
}

// VerifyReceipt validates a nektarr1.* token or 64-hex hash, its §16 delegations, its issuer signature,
// its committed __policies__ / __ontology__ proof-carrying derivations, and its cited SPV proofs.
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

	receiptAnchored := false
	if rec.ShardSeq > 0 {
		if _, rvr, _, rpErr := s.proveWithMemo(ctx, ReceiptsShard, rec.ShardSeq, ownerPrincipal(), nil); rpErr == nil && rvr != nil && rvr.OK && rvr.Settled {
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

	// Verify __policies__ commitment if referenced by the receipt.
	if rcp.PolicyRef != nil && rcp.PolicyRef.Shard != "" && rcp.PolicyRef.Seq > 0 {
		pf, pErr := s.db.GetFragment(rcp.PolicyRef.Shard, rcp.PolicyRef.Seq)
		if pErr != nil || pf.Forgotten || !verifyStoredFragmentLeaf(pf) {
			out.Verdict = "red"
			out.OK = false
			out.Reason = fmt.Sprintf("committed policy fragment %s#%d missing or tampered", rcp.PolicyRef.Shard, rcp.PolicyRef.Seq)
			return out, nil
		}
		if rcp.PolicyCommitHash != "" {
			var pObj struct {
				Hash string `json:"hash"`
			}
			if json.Unmarshal(pf.Data, &pObj) != nil || !strings.EqualFold(pObj.Hash, rcp.PolicyCommitHash) {
				out.Verdict = "red"
				out.OK = false
				out.Reason = "receipt policyCommitHash mismatches committed __policies__ fragment"
				return out, nil
			}
		}
		if pf.BatchN >= 0 {
			if _, pvr, _, err := s.proveWithMemo(ctx, pf.Shard, pf.Seq, ownerPrincipal(), nil); err == nil && pvr != nil && pvr.OK && pvr.Settled {
				out.PolicyAnchored = true
			}
		}
	}

	// Verify __ontology__ rules & check anchored status.
	ruleHashesByRef := make(map[reasoning.FragmentRef]map[string]bool)
	if len(rcp.RuleRefs) > 0 || len(rcp.Derivations) > 0 {
		seenRR := make(map[reasoning.FragmentRef]bool)
		var allRuleRefs []reasoning.FragmentRef
		for _, rr := range rcp.RuleRefs {
			if !seenRR[rr] {
				seenRR[rr] = true
				allRuleRefs = append(allRuleRefs, rr)
			}
		}
		for i := range rcp.Derivations {
			collectDerivationRuleRefs(&rcp.Derivations[i], seenRR, &allRuleRefs)
		}
		allRulesMined := len(allRuleRefs) > 0
		for _, rr := range allRuleRefs {
			rf, rErr := s.db.GetFragment(rr.Shard, rr.Seq)
			if rErr != nil || rf.Forgotten || !verifyStoredFragmentLeaf(rf) {
				out.Verdict = "red"
				out.OK = false
				out.Reason = fmt.Sprintf("committed ontology rule %s#%d missing or tampered", rr.Shard, rr.Seq)
				return out, nil
			}
			hset := make(map[string]bool)
			switch rf.Class {
			case "ontology:tbox":
				ont, oErr := jevgraph.ParseOntologyAuto(rf.Data)
				if oErr != nil {
					out.Verdict = "red"
					out.OK = false
					out.Reason = fmt.Sprintf("corrupt domain ontology fragment %s#%d: %v", rr.Shard, rr.Seq, oErr)
					return out, nil
				}
				comp, cErr := jevgraph.Compile(ont)
				if cErr != nil {
					out.Verdict = "red"
					out.OK = false
					out.Reason = fmt.Sprintf("invalid domain ontology fragment %s#%d: %v", rr.Shard, rr.Seq, cErr)
					return out, nil
				}
				for _, rt := range comp.MangleRules {
					if strings.TrimSpace(rt) != "" {
						hset[reasoning.ComputeRuleHash(rt)] = true
					}
				}
			case "ontology:bridge":
				var bm jevgraph.BridgeManifest
				if bErr := json.Unmarshal(rf.Data, &bm); bErr != nil {
					out.Verdict = "red"
					out.OK = false
					out.Reason = fmt.Sprintf("corrupt ontology bridge fragment %s#%d", rr.Shard, rr.Seq)
					return out, nil
				}
				_, rules, cErr := jevgraph.CompileBridgeRules(&bm)
				if cErr != nil {
					out.Verdict = "red"
					out.OK = false
					out.Reason = fmt.Sprintf("invalid ontology bridge fragment %s#%d: %v", rr.Shard, rr.Seq, cErr)
					return out, nil
				}
				for _, rt := range rules {
					if strings.TrimSpace(rt) != "" {
						hset[reasoning.ComputeRuleHash(rt)] = true
					}
				}
			default:
				var ax struct {
					CompiledRule  string   `json:"compiledRule"`
					CompiledRules []string `json:"compiledRules"`
					RuleText      string   `json:"ruleText"`
				}
				if json.Unmarshal(rf.Data, &ax) != nil {
					out.Verdict = "red"
					out.OK = false
					out.Reason = fmt.Sprintf("corrupt ontology rule fragment %s#%d", rr.Shard, rr.Seq)
					return out, nil
				}
				for _, rt := range ax.CompiledRules {
					if strings.TrimSpace(rt) != "" {
						hset[reasoning.ComputeRuleHash(rt)] = true
					}
				}
				if strings.TrimSpace(ax.CompiledRule) != "" {
					hset[reasoning.ComputeRuleHash(ax.CompiledRule)] = true
				}
				if strings.TrimSpace(ax.RuleText) != "" {
					hset[reasoning.ComputeRuleHash(ax.RuleText)] = true
				}
			}
			ruleHashesByRef[rr] = hset
			if rf.BatchN < 0 {
				allRulesMined = false
			} else if _, rvr, _, err := s.proveWithMemo(ctx, rf.Shard, rf.Seq, ownerPrincipal(), nil); err != nil || rvr == nil || !rvr.OK || !rvr.Settled {
				allRulesMined = false
			}
		}
		out.OntologyAnchored = allRulesMined
	}

	type batchKey struct {
		shard string
		n     int64
	}
	spvMemos := make(map[batchKey]*contracts.AnchorBatchMemo)
	supCache := make(map[string]map[int64]int64)
	factPlaintextByRef := make(map[reasoning.FragmentRef]struct {
		text string
		hdp  string
	})

	for _, ref := range rcp.FragmentRefs {
		out.CitationsChecked++
		f, fErr := s.db.GetFragment(ref.Shard, ref.Seq)
		if fErr != nil || f.Forgotten {
			out.CitationsMissing++
			continue
		}
		if !verifyStoredFragmentLeaf(f) {
			out.CitationsInvalid++
			continue
		}
		if pt, ok := s.openFragmentPlaintext(ctx, f, ownerPrincipal(), s.port.IdentityPubHex(), nil, false, false); ok {
			factPlaintextByRef[ref] = struct {
				text string
				hdp  string
			}{text: string(pt), hdp: f.HDP}
		}
		m, cached := supCache[ref.Shard]
		if !cached {
			m = s.CollectSupersededSeqs(ref.Shard)
			supCache[ref.Shard] = m
		}
		if newerSeq := m[ref.Seq]; newerSeq > 0 {
			out.CitationsSuperseded++
			out.SupersededRefs = append(out.SupersededRefs, fmt.Sprintf("%s#%d->#%d", ref.Shard, ref.Seq, newerSeq))
		}
		if f.BatchN >= 0 {
			bk := batchKey{shard: ref.Shard, n: f.BatchN}
			_, vr, nextMemo, pErr := s.proveWithMemo(ctx, ref.Shard, ref.Seq, ownerPrincipal(), spvMemos[bk])
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
				case "TrackerUnavailable":
					out.CitationsUnavailable++
				default:
					out.CitationsInvalid++
				}
			}
		}
	}

	// Re-verify every proof-carrying derivation step against committed facts & committed rules.
	factVer := func(ref reasoning.FragmentRef, expectedHDP string, atom string) error {
		info, ok := factPlaintextByRef[ref]
		if !ok {
			return fmt.Errorf("cited fact %s#%d not verified", ref.Shard, ref.Seq)
		}
		if expectedHDP != "" && !strings.EqualFold(info.hdp, expectedHDP) {
			return fmt.Errorf("fact %s#%d HDP mismatch", ref.Shard, ref.Seq)
		}
		wantAtom, okA := reasoning.ParseGroundFact(atom)
		if !okA {
			return fmt.Errorf("invalid ground atom %q", atom)
		}
		for _, line := range strings.Split(info.text, "\n") {
			if parsed, okL := reasoning.ParseGroundFact(line); okL && parsed.String() == wantAtom.String() {
				return nil
			}
		}
		// Also accept verified JevGraph edges anchored to this fragment whose evidence quote exists verbatim in info.text.
		if edges, eErr := s.db.ListKnowledgeEdges(store.KnowledgeEdgeFilter{
			Shard:  ref.Shard,
			Seq:    ref.Seq,
			Status: "accepted",
		}); eErr == nil {
			for _, edge := range edges {
				if edge.EvidenceQuote != "" && !strings.Contains(info.text, edge.EvidenceQuote) {
					continue
				}
				for _, a := range knowledgeEdgeToMangleAtoms(edge) {
					if parsed, okA := reasoning.ParseGroundFact(a); okA && parsed.String() == wantAtom.String() {
						return nil
					}
				}
			}
		}
		return fmt.Errorf("atom %s not found in fragment %s#%d", wantAtom.String(), ref.Shard, ref.Seq)
	}
	ruleVer := func(ref *reasoning.FragmentRef, ruleHash string, _ string) error {
		if ref == nil {
			return errors.New("uncommitted rule (missing ruleRef)")
		}
		hset, ok := ruleHashesByRef[*ref]
		if !ok || !hset[ruleHash] {
			return fmt.Errorf("rule hash %s not committed at %s#%d", ruleHash, ref.Shard, ref.Seq)
		}
		return nil
	}
	for i := range rcp.Derivations {
		if dErr := reasoning.VerifyDerivationProof(&rcp.Derivations[i], factVer, ruleVer); dErr != nil {
			out.Verdict = "red"
			out.OK = false
			out.Reason = fmt.Sprintf("derivation proof %d invalid: %v", i, dErr)
			return out, nil
		}
		out.DerivationsVerified++
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
