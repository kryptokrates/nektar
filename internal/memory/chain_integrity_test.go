package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/store"
)

func newTestMemoryForChain(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	port, err := chain.Open(ctx, chain.Config{Network: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = port.Close() })
	objStore, err := bstack.NewFileObjectStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(db, port, bstack.NewGroupRegistry(), objStore, Config{})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestPerFragmentHashChainAndTamperDetection(t *testing.T) {
	ctx := context.Background()
	svc := newTestMemoryForChain(t)

	for _, txt := range []string{
		"BSV Chronicle activated at height 943816.",
		"Nektar anchors batch Merkle roots with R' chaining.",
		"Unanchored rows must also be tamper-evident locally.",
	} {
		if _, err := svc.Remember(ctx, RememberInput{
			Shard:      "audit-shard",
			Text:       txt,
			Class:      "ops",
			AutoAnchor: false,
		}); err != nil {
			t.Fatalf("Remember: %v", err)
		}
	}

	// 1. Clean unanchored shard passes full prev_leaf + chain_hash + HDP + leaf check!
	rep, err := svc.VerifyShardChain("audit-shard")
	if err != nil {
		t.Fatalf("VerifyShardChain: %v", err)
	}
	if !rep.OK || rep.FragmentCount != 3 || rep.VerifiedCount != 3 || rep.HeadChainHash == "" {
		t.Fatalf("expected clean shard chain report, got %+v", rep)
	}

	// 1b. Unanchored selective-disclosure field proof MUST fail closed (L1 UTXO anchor is mandatory).
	if _, err := svc.ProveFragmentFields(ctx, "audit-shard", 2, []string{"source", "class"}); err == nil {
		t.Fatal("expected ProveFragmentFields to fail closed before anchoring")
	}

	// 2. Anchor and verify again (anchors Rp chain + UTXO lineage + fragment chain all OK).
	if _, err := svc.AnchorShard(ctx, "audit-shard"); err != nil {
		t.Fatalf("AnchorShard: %v", err)
	}
	rep2, err := svc.VerifyShardChain("audit-shard")
	if err != nil || !rep2.OK || rep2.AnchorCount != 1 || !rep2.AnchorsOK {
		t.Fatalf("expected clean anchored shard report, got %+v err=%v", rep2, err)
	}

	// 3. L1-bound selective disclosure field proof: prove source + class + modelId + hdp WITHOUT revealing plaintext!
	fp, err := svc.ProveFragmentFields(ctx, "audit-shard", 2, []string{"source", "class", "modelId", "hdp"})
	if err != nil {
		t.Fatalf("ProveFragmentFields: %v", err)
	}
	if vr := contracts.VerifyAnchoredMPEProof(ctx, fp.HDP, fp.CiphertextHash, fp.ContentSHA256, fp.FieldsRoot, fp.CommitHash, fp.TransitionHash, fp.Proof, *fp.ProofBundle, svc.port.Tracker()); !vr.OK {
		t.Fatalf("VerifyAnchoredMPEProof: %+v", vr)
	}
	for _, f := range fp.Proof.Fields {
		if strings.Contains(string(f.Value), "Merkle roots") {
			t.Fatalf("plaintext leaked into selective metadata proof: %+v", f)
		}
	}

	// Tampering with a disclosed metadata field MUST fail offline verification!
	badFP := *fp
	badProof := *fp.Proof
	badProof.Fields = append([]core.DisclosedField(nil), fp.Proof.Fields...)
	badProof.Fields[0].Value = []byte("forged-class")
	badFP.Proof = &badProof
	if vr := contracts.VerifyAnchoredMPEProof(ctx, badFP.HDP, badFP.CiphertextHash, badFP.ContentSHA256, badFP.FieldsRoot, badFP.CommitHash, badFP.TransitionHash, badFP.Proof, *badFP.ProofBundle, svc.port.Tracker()); vr.OK {
		t.Fatal("expected tampered selective disclosure proof to fail")
	}

	// 4. Directly tamper with an unanchored row's plaintext in SQLite -> VerifyShardChain MUST catch it!
	if _, err := svc.Remember(ctx, RememberInput{
		Shard:  "audit-shard",
		Text:   "ephemeral secret to be shredded",
		Public: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DB().Conn().Exec(`UPDATE fragments SET data = ? WHERE shard = ? AND seq = 4`, []byte("TAMPERED PLAINTEXT"), "audit-shard"); err != nil {
		t.Fatalf("tamper SQL UPDATE: %v", err)
	}
	repTampered, err := svc.VerifyShardChain("audit-shard")
	if err != nil {
		t.Fatalf("VerifyShardChain(tampered): %v", err)
	}
	if repTampered.OK {
		t.Fatalf("expected VerifyShardChain to detect direct SQLite plaintext tampering on unanchored seq=4, got OK: %+v", repTampered)
	}
	// Restore original plaintext, then verify forgetting still preserves chain integrity!
	if _, err := svc.DB().Conn().Exec(`UPDATE fragments SET data = ? WHERE shard = ? AND seq = 4`, []byte("ephemeral secret to be shredded"), "audit-shard"); err != nil {
		t.Fatalf("restore SQL UPDATE: %v", err)
	}
	if err := svc.Forget(ctx, "audit-shard", 4); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	// After shredding seq 4, chain_hash and prev_leaf remain intact!
	repShred, err := svc.VerifyShardChain("audit-shard")
	if err != nil || !repShred.OK || repShred.VerifiedCount != 4 {
		t.Fatalf("expected shredded fragment to preserve hash chain, got %+v err=%v", repShred, err)
	}

	// 5. Second batch anchor MUST strictly spend batch 0's UTXO (n=0:0 -> n=1:0) and pass VerifyShardChain!
	if _, err := svc.Remember(ctx, RememberInput{
		Shard: "audit-shard",
		Text:  "second batch anchored strictly via singleton UTXO lineage",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AnchorShard(ctx, "audit-shard"); err != nil {
		t.Fatalf("AnchorShard(n=1): %v", err)
	}
	repLineage, err := svc.VerifyShardChain("audit-shard")
	if err != nil || !repLineage.OK || repLineage.AnchorCount != 2 || !repLineage.AnchorsOK {
		t.Fatalf("expected 2-batch UTXO lineage shard chain OK, got %+v err=%v", repLineage, err)
	}
	// Prove seq=5 verifies full ChainTail UTXO spend lineage (n=0 -> n=1) via VerifyFragment!
	pb5, vr5, err := svc.Prove(ctx, "audit-shard", 5)
	if err != nil || pb5 == nil || vr5 == nil || !vr5.OK {
		t.Fatalf("Prove(seq=5) failed: pb=%v vr=%+v err=%v", pb5 != nil, vr5, err)
	}
	if len(pb5.ChainTail) != 2 || len(pb5.ChainTail[0].AnchorBEEF) == 0 || len(pb5.ChainTail[1].AnchorBEEF) == 0 {
		t.Fatalf("expected ChainTail of length 2 carrying full AnchorBEEF lineage, got len=%d", len(pb5.ChainTail))
	}
}
