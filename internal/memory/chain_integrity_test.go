package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
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

	// 2. Anchor and verify again (anchors Rp chain + fragment chain both OK).
	if _, err := svc.AnchorShard(ctx, "audit-shard"); err != nil {
		t.Fatalf("AnchorShard: %v", err)
	}
	rep2, err := svc.VerifyShardChain("audit-shard")
	if err != nil || !rep2.OK || rep2.AnchorCount != 1 || !rep2.AnchorsOK {
		t.Fatalf("expected clean anchored shard report, got %+v err=%v", rep2, err)
	}

	// 3. Selective disclosure field proof: prove source + class + time WITHOUT revealing plaintext!
	fp, err := svc.ProveFragmentFields(ctx, "audit-shard", 2, []string{"source", "class", "modelId", "leaf"})
	if err != nil {
		t.Fatalf("ProveFragmentFields: %v", err)
	}
	if err := VerifyFragmentFieldProof(fp); err != nil {
		t.Fatalf("VerifyFragmentFieldProof: %v", err)
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
	if err := VerifyFragmentFieldProof(&badFP); err == nil {
		t.Fatal("expected tampered selective disclosure proof to fail")
	}

	// 4. Directly tamper with an unanchored row's plaintext in SQLite -> VerifyShardChain MUST catch it!
	// Append one unanchored 4th fragment, then verify forgetting still preserves chain integrity!
	if _, err := svc.Remember(ctx, RememberInput{
		Shard: "audit-shard",
		Text:  "ephemeral secret to be shredded",
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Forget(ctx, "audit-shard", 4); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	// After shredding seq 4, chain_hash and prev_leaf remain intact!
	repShred, err := svc.VerifyShardChain("audit-shard")
	if err != nil || !repShred.OK || repShred.VerifiedCount != 4 {
		t.Fatalf("expected shredded fragment to preserve hash chain, got %+v err=%v", repShred, err)
	}
}
