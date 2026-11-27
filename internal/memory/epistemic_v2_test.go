package memory

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
)

func TestCLKAPrivateByDefaultAndZeroPlaintextAtRest(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestMemoryService(t)

	secret := "ultra-private-level0-design-note-xyz987"
	res, err := svc.Remember(ctx, RememberInput{
		Shard: "vault",
		Text:  secret,
		Class: "finance",
		Level: 0,
	})
	if err != nil {
		t.Fatalf("Remember(default CLKA): %v", err)
	}
	if !res.Encrypted || res.Public || res.LatticeEpoch != 1 {
		t.Fatalf("expected private-by-default CLKA epoch 1, got %+v", res)
	}
	if res.CommitHash == "" || res.TransitionHash == "" || res.StateQ == "" {
		t.Fatalf("expected unified CommitHash+TransitionHash+StateQ, got %+v", res)
	}

	// Verify ZERO plaintext in SQLite fragments.data, FTS5, and mem_objects!
	rec, err := svc.DB().GetFragment("vault", res.Seq)
	if err != nil {
		t.Fatalf("GetFragment: %v", err)
	}
	if len(rec.Data) != 0 || len(rec.Ciphertext) == 0 || len(rec.KeyedMeta) == 0 {
		t.Fatalf("expected nil data and non-empty CLKA ciphertext/keyedMeta, got data=%d ct=%d km=%d", len(rec.Data), len(rec.Ciphertext), len(rec.KeyedMeta))
	}
	if hits, _ := svc.DB().SearchFTS("vault", "xyz987", 10); len(hits) != 0 {
		t.Fatalf("CLKA private fragment leaked into FTS5: %+v", hits)
	}
	cRow, err := svc.DB().GetMemCommit(res.CommitHash)
	if err != nil {
		t.Fatalf("GetMemCommit: %v", err)
	}
	if strings.Contains(cRow.Message, "xyz987") {
		t.Fatalf("CLKA plaintext leaked into commit message: %q", cRow.Message)
	}
	_, trBody, _ := svc.DB().GetMemObject(cRow.TreeHash)
	if bytes.Contains(trBody, []byte("xyz987")) {
		t.Fatal("CLKA plaintext leaked into tree object")
	}

	// Owner recall decrypts seamlessly and verifies HDP.
	items, err := svc.Recall(ctx, RecallInput{Shard: "vault"})
	if err != nil || len(items) != 1 || items[0].Text != secret {
		t.Fatalf("owner Recall failed: err=%v items=%+v", err, items)
	}

	// Store Level 1 finance, Level 2 finance, and Level 1 medical fragments.
	if _, err := svc.Remember(ctx, RememberInput{Shard: "vault", Text: "Q4 treasury forecast", Class: "finance", Level: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Remember(ctx, RememberInput{Shard: "vault", Text: "Board M&A target", Class: "finance", Level: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Remember(ctx, RememberInput{Shard: "vault", Text: "Patient chart 42", Class: "medical", Level: 1}); err != nil {
		t.Fatal(err)
	}

	// Provision Level 1 finance clearance and verify strict cryptographic lattice enforcement.
	clr, _, err := svc.Lattice().ProvisionClearance(ctx, "vault", 1, []string{"finance"})
	if err != nil {
		t.Fatalf("ProvisionClearance: %v", err)
	}
	clrItems, err := svc.Recall(ctx, RecallInput{
		Shard:     "vault",
		Principal: policy.Principal{Type: "Agent", ID: "analyst-1", Level: 1},
		Clearance: clr,
	})
	if err != nil {
		t.Fatalf("Recall with clearance: %v", err)
	}
	if len(clrItems) != 2 {
		t.Fatalf("expected exactly 2 finance fragments (level 0 & 1), got %d: %+v", len(clrItems), clrItems)
	}
	for _, it := range clrItems {
		if strings.Contains(it.Text, "M&A") || strings.Contains(it.Text, "Patient") {
			t.Fatalf("clearance leaked unauthorized fragment: %+v", it)
		}
	}

	// Epoch rotation provides forward secrecy: old epoch 1 clearance cannot open epoch 2 fragments.
	newEp, err := svc.Lattice().RotateEpoch("vault")
	if err != nil || newEp != 2 {
		t.Fatalf("RotateEpoch: ep=%d err=%v", newEp, err)
	}
	rEp2, err := svc.Remember(ctx, RememberInput{Shard: "vault", Text: "Post-rotation secret", Class: "finance", Level: 1})
	if err != nil || rEp2.LatticeEpoch != 2 {
		t.Fatalf("Remember post-rotation: res=%+v err=%v", rEp2, err)
	}
	clrItems2, err := svc.Recall(ctx, RecallInput{
		Shard:     "vault",
		Principal: policy.Principal{Type: "Agent", ID: "analyst-1", Level: 1},
		Clearance: clr,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range clrItems2 {
		if strings.Contains(it.Text, "Post-rotation") {
			t.Fatalf("epoch-1 clearance decrypted epoch-2 fragment! forward secrecy broken")
		}
	}

	// Explicit Public: true opt-out stores plaintext and indexes FTS5.
	pubRes, err := svc.Remember(ctx, RememberInput{
		Shard:  "vault",
		Text:   "public-searchable-announcement-abc123",
		Public: true,
	})
	if err != nil || pubRes.Encrypted || !pubRes.Public {
		t.Fatalf("Remember(Public:true): res=%+v err=%v", pubRes, err)
	}
	if hits, _ := svc.DB().SearchFTS("vault", "abc123", 10); len(hits) != 1 {
		t.Fatalf("expected 1 FTS hit for Public fragment, got %d", len(hits))
	}
}

func TestCryptographicKnowledgeDAGAndSignedSuperseding(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestMemoryService(t)

	r1, err := svc.Remember(ctx, RememberInput{
		Shard:    "science",
		Text:     "Initial estimate: latency is 120ms.",
		Modality: &core.Modality{Kind: "epistemic", Qualifier: "confidence:0.70"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Forged TargetHDP MUST fail closed!
	if _, err := svc.Remember(ctx, RememberInput{
		Shard: "science",
		Text:  "Forged link",
		Refines: []core.RefinementEdge{{
			TargetShard: "science",
			TargetSeq:   r1.Seq,
			TargetHDP:   strings.Repeat("ff", 32),
			Kind:        core.RefinementQualifies,
		}},
	}); err == nil {
		t.Fatal("expected forged targetHdp refinement edge to be rejected")
	}

	// Valid qualifying edge + valid signed superseding edge.
	r2, err := svc.Remember(ctx, RememberInput{
		Shard: "science",
		Text:  "Measured under cold cache conditions.",
		Refines: []core.RefinementEdge{{
			TargetSeq: r1.Seq,
			Kind:      core.RefinementQualifies,
			Reason:    "context",
		}},
	})
	if err != nil {
		t.Fatalf("Remember(r2): %v", err)
	}
	r3, err := svc.Remember(ctx, RememberInput{
		Shard:    "science",
		Text:     "Corrected measurement: latency is 42ms.",
		Modality: &core.Modality{Kind: "epistemic", Qualifier: "confidence:0.99"},
		Refines: []core.RefinementEdge{{
			TargetSeq: r1.Seq,
			Kind:      core.RefinementSupersedes,
			Reason:    "re-measured with warm cache",
		}},
	})
	if err != nil {
		t.Fatalf("Remember(r3): %v", err)
	}

	// Default Recall MUST exclude cryptographically superseded r1!
	active, err := svc.Recall(ctx, RecallInput{Shard: "science"})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 {
		t.Fatalf("expected 2 active fragments (r1 superseded), got %d", len(active))
	}
	for _, it := range active {
		if it.Seq == r1.Seq {
			t.Fatalf("cryptographically superseded seq %d leaked into default Recall", r1.Seq)
		}
	}

	// GroundedAnswer MUST refuse to ground on superseded r1!
	gaBad, err := svc.GroundedAnswer(ctx, GroundedAnswerInput{
		Claims: []reasoning.Claim{{
			Text:  "Initial estimate: latency is 120ms.",
			Cites: []reasoning.FragmentRef{{Shard: "science", Seq: r1.Seq}},
		}},
	})
	if err != nil || !gaBad.Ungrounded {
		t.Fatalf("expected GroundedAnswer to reject superseded r1, got err=%v ungrounded=%v", err, gaBad.Ungrounded)
	}

	// Lineage from r1 shows both r2 and r3 as verified descendants, and from r3 shows r1 as verified ancestor.
	lin1, err := svc.Lineage(ctx, "science", r1.Seq)
	if err != nil {
		t.Fatalf("Lineage(r1): %v", err)
	}
	if lin1.Root.SupersededBy != r3.Seq || len(lin1.Descendants) != 2 {
		t.Fatalf("unexpected r1 lineage: supersededBy=%d descendants=%d", lin1.Root.SupersededBy, len(lin1.Descendants))
	}
	lin3, err := svc.Lineage(ctx, "science", r3.Seq)
	if err != nil {
		t.Fatalf("Lineage(r3): %v", err)
	}
	if len(lin3.Ancestors) != 1 || lin3.Ancestors[0].Seq != r1.Seq {
		t.Fatalf("unexpected r3 ancestors: %+v", lin3.Ancestors)
	}
	for _, e := range lin3.Edges {
		if !e.Verified {
			t.Fatalf("expected verified refinement edge, got %+v", e)
		}
	}
	_ = r2
}

func TestMemoryAsLawOntologyProofCarryingReceiptAndStrictSPV(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestMemoryService(t)

	// 1. Commit DL-Lite TBox axioms into __ontology__ and anchor both __ontology__ and __policies__.
	ontRes, err := svc.CommitOntologyAxioms(ctx, []reasoning.OntologyAxiom{
		{Kind: reasoning.AxiomSubClassOf, Subject: "SeniorEngineer", Object: "Engineer"},
		{Kind: reasoning.AxiomSubClassOf, Subject: "Engineer", Object: "Employee"},
	}, true)
	if err != nil {
		t.Fatalf("CommitOntologyAxioms: %v", err)
	}
	if ontRes.OntologyHead == "" || ontRes.AnchorTxID == "" || len(ontRes.RuleRefs) != 2 {
		t.Fatalf("unexpected OntologyCommitResult: %+v", ontRes)
	}
	if _, err := svc.AnchorShard(ctx, PoliciesShard); err != nil {
		t.Fatalf("AnchorShard(__policies__): %v", err)
	}

	// 2. Remember structured RDF triple (unanchored first).
	f1, err := svc.Remember(ctx, RememberInput{
		Shard: "hr",
		Triples: []reasoning.Triple{{
			Subject:   "Alice",
			Predicate: "rdf:type",
			Object:    "SeniorEngineer",
		}},
	})
	if err != nil {
		t.Fatalf("Remember(triple): %v", err)
	}

	// 3. RequireSettledSPV MUST reject unanchored fragment!
	gaUnsettled, err := svc.GroundedAnswer(ctx, GroundedAnswerInput{
		RequireSettledSPV: true,
		Claims: []reasoning.Claim{{
			Text:    "Alice is an Employee",
			Literal: `rdf_type("Alice", "Employee")`,
			Cites:   []reasoning.FragmentRef{{Shard: "hr", Seq: f1.Seq}},
		}},
	})
	if err != nil || !gaUnsettled.Ungrounded {
		t.Fatalf("expected RequireSettledSPV to reject unanchored fragment, got err=%v ungrounded=%v", err, gaUnsettled.Ungrounded)
	}

	// 4. Anchor shard -> RequireSettledSPV + proof-carrying 2-hop DL-Lite derivation SUCCEEDS!
	if _, err := svc.AnchorShard(ctx, "hr"); err != nil {
		t.Fatalf("AnchorShard(hr): %v", err)
	}
	ga, err := svc.GroundedAnswer(ctx, GroundedAnswerInput{
		RequireSettledSPV: true,
		AutoAnchor:        true,
		Claims: []reasoning.Claim{{
			Text:    "Alice is an Employee",
			Literal: `rdf_type("Alice", "Employee")`,
			Cites:   []reasoning.FragmentRef{{Shard: "hr", Seq: f1.Seq}},
		}},
	})
	if err != nil || ga.Ungrounded || ga.Receipt == nil {
		t.Fatalf("GroundedAnswer(2-hop DL-Lite + SPV) failed: err=%v res=%+v", err, ga)
	}
	if ga.Receipt.PolicyCommitHash == "" || ga.Receipt.PolicyRef == nil || ga.Receipt.OntologyCommitHash == "" {
		t.Fatalf("expected receipt to commit policy & ontology refs, got %+v", ga.Receipt)
	}
	if len(ga.Receipt.Derivations) == 0 || len(ga.Receipt.RuleRefs) == 0 {
		t.Fatalf("expected proof-carrying derivation tree & rule refs, got derivations=%d rules=%d", len(ga.Receipt.Derivations), len(ga.Receipt.RuleRefs))
	}

	// 5. VerifyReceipt verifies signature, __policies__, __ontology__, derivations, and mined SPV!
	ver, err := svc.VerifyReceipt(ctx, ga.ReceiptToken)
	if err != nil || !ver.OK || ver.Verdict != "green" {
		t.Fatalf("VerifyReceipt failed: err=%v ver=%+v", err, ver)
	}
	if !ver.PolicyAnchored || !ver.OntologyAnchored || !ver.ReceiptAnchored || ver.DerivationsVerified == 0 {
		t.Fatalf("expected fully anchored law+receipt+derivations, got %+v", ver)
	}
}

func TestCompactMMRProofAndConsistencyProofAcrossMultiBatch(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestMemoryService(t)

	var firstSeq int64
	var mmrRoots [][]byte
	for i := 0; i < 4; i++ {
		r, err := svc.Remember(ctx, RememberInput{
			Shard:      "mmr-log",
			Text:       "batch entry " + string(rune('A'+i)),
			AutoAnchor: true,
		})
		if err != nil {
			t.Fatalf("Remember+AutoAnchor batch %d: %v", i, err)
		}
		if i == 0 {
			firstSeq = r.Seq
		}
		anchors, _ := svc.DB().ListAnchors("mmr-log")
		last := anchors[len(anchors)-1]
		mb, err := hex.DecodeString(last.MmrRoot)
		if err != nil || len(mb) != 32 {
			t.Fatalf("expected 32-byte MmrRoot on batch %d, got %q", i, last.MmrRoot)
		}
		mmrRoots = append(mmrRoots, mb)
	}

	// Full shard integrity check including transitions + StateHash Q_n + MMR root + UTXO lineage!
	rep, err := svc.VerifyShardChain("mmr-log")
	if err != nil || !rep.OK || rep.TransitionCount != 4 || rep.AnchorCount != 4 || rep.HeadStateQ == "" || rep.HeadMmrRoot == "" {
		t.Fatalf("VerifyShardChain failed: err=%v rep=%+v", err, rep)
	}

	// O(log N) CompactProofBundle proving batch 0 fragment directly against head batch 3 anchor!
	cpb, vr, err := svc.ProveCompact(ctx, "mmr-log", firstSeq)
	if err != nil || cpb == nil || vr == nil || !vr.OK || !vr.Settled {
		t.Fatalf("ProveCompact failed: err=%v vr=%+v", err, vr)
	}
	if cpb.BatchN != 0 || cpb.HeadAnchor.N != 3 {
		t.Fatalf("expected BatchN=0 HeadN=3, got BatchN=%d HeadN=%d", cpb.BatchN, cpb.HeadAnchor.N)
	}

	// O(log N) ConsistencyProof proving batch 1 (2 leaves) is an unmodified prefix of batch 3 (4 leaves)!
	cp, err := svc.ConsistencyProof("mmr-log", 1, 3)
	if err != nil {
		t.Fatalf("ConsistencyProof: %v", err)
	}
	if err := core.VerifyMMRConsistency(mmrRoots[1], mmrRoots[3], cp); err != nil {
		t.Fatalf("VerifyMMRConsistency rejected valid consistency proof: %v", err)
	}
	// Forged old root MUST fail!
	forgedOld := append([]byte(nil), mmrRoots[1]...)
	forgedOld[0] ^= 0xff
	if err := core.VerifyMMRConsistency(forgedOld, mmrRoots[3], cp); err == nil {
		t.Fatal("VerifyMMRConsistency accepted forged old root")
	}
}
