package memory

import (
	"context"
	"encoding/hex"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/store"
)

// TestNektarAnchorSingletonAppendAndRotateE2E proves end-to-end on real BSV testchain + pure-Go wallet:
//  1. AnchorShard batch 0 mints a spendable patched NektarAnchor genesis UTXO.
//  2. AnchorShard batch 1 & batch 2 spend the previous anchor UTXO (tx(n-1):0) via NektarAnchor.Append(),
//     passing real post-Chronicle Bitcoin Script VM + testchain validation + full SPV Prove().
//  3. RotateShardOwner transfers ownership on-chain via NektarAnchor.Rotate():
//     - Rotating to another local BRC-43 key (SigningPubKey) allows continued Append().
//     - Rotating to an external key not held locally causes subsequent AnchorShard to fail closed (no silent fork!).
func TestNektarAnchorSingletonAppendAndRotateE2E(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(dir + "/nektar.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: dir})
	if err != nil {
		t.Fatalf("chain.Open: %v", err)
	}
	defer port.Close()

	svc, err := New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), Config{})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}

	// Batch 0 (genesis NektarAnchor).
	f0, err := svc.Remember(ctx, RememberInput{Shard: "lineage", Text: "batch 0 fact"})
	if err != nil {
		t.Fatalf("Remember(0): %v", err)
	}
	a0, err := svc.AnchorShard(ctx, "lineage")
	if err != nil || a0 == nil {
		t.Fatalf("AnchorShard(0): a=%v err=%v", a0, err)
	}
	_, tx0, _, err := guard.ParseBEEF(a0.Beef, guard.DefaultBound)
	if err != nil || tx0 == nil {
		t.Fatalf("parse a0 BEEF: %v", err)
	}

	// Batch 1 (must spend tx0:0 via NektarAnchor.Append!).
	f1, err := svc.Remember(ctx, RememberInput{Shard: "lineage", Text: "batch 1 fact"})
	if err != nil {
		t.Fatalf("Remember(1): %v", err)
	}
	a1, err := svc.AnchorShard(ctx, "lineage")
	if err != nil || a1 == nil {
		t.Fatalf("AnchorShard(1): a=%v err=%v", a1, err)
	}
	_, tx1, _, err := guard.ParseBEEF(a1.Beef, guard.DefaultBound)
	if err != nil || tx1 == nil {
		t.Fatalf("parse a1 BEEF: %v", err)
	}
	if err := contracts.VerifyAnchorUTXOLineage(tx0, tx1); err != nil {
		t.Fatalf("Batch 0->1 singleton UTXO lineage failed: %v", err)
	}

	// Batch 2 (must spend tx1:0 via NektarAnchor.Append!).
	f2, err := svc.Remember(ctx, RememberInput{Shard: "lineage", Text: "batch 2 fact"})
	if err != nil {
		t.Fatalf("Remember(2): %v", err)
	}
	a2, err := svc.AnchorShard(ctx, "lineage")
	if err != nil || a2 == nil {
		t.Fatalf("AnchorShard(2): a=%v err=%v", a2, err)
	}
	_, tx2, _, err := guard.ParseBEEF(a2.Beef, guard.DefaultBound)
	if err != nil || tx2 == nil {
		t.Fatalf("parse a2 BEEF: %v", err)
	}
	if err := contracts.VerifyAnchorUTXOLineage(tx1, tx2); err != nil {
		t.Fatalf("Batch 1->2 singleton UTXO lineage failed: %v", err)
	}

	// SPV Prove must pass for fragments across all 3 batches.
	for _, seq := range []int64{f0.Seq, f1.Seq, f2.Seq} {
		pb, vr, err := svc.Prove(ctx, "lineage", seq)
		if err != nil || pb == nil || !vr.OK || !vr.Settled {
			t.Fatalf("Prove(seq=%d) failed: vr=%+v err=%v", seq, vr, err)
		}
	}

	// Rotate ownership from FundPKH -> SigningPubKey (another local BRC-43 key) via NektarAnchor.Rotate()!
	signPub, err := port.SigningPubKey(ctx)
	if err != nil {
		t.Fatalf("SigningPubKey: %v", err)
	}
	rot1, err := svc.RotateShardOwner(ctx, "lineage", hex.EncodeToString(signPub.Compressed()))
	if err != nil {
		t.Fatalf("RotateShardOwner(local SigningPubKey): %v", err)
	}
	_, rotTx1, _, err := guard.ParseBEEF(rot1.Beef, guard.DefaultBound)
	if err != nil {
		t.Fatalf("parse rot1 BEEF: %v", err)
	}
	if err := contracts.VerifyAnchorUTXOLineage(tx2, rotTx1); err != nil {
		t.Fatalf("Rotate lineage failed: %v", err)
	}
	// Existing batch 2 fragment still SPV-verifies against rotated anchor head!
	if pb, vr, err := svc.Prove(ctx, "lineage", f2.Seq); err != nil || pb == nil || !vr.OK {
		t.Fatalf("Prove after Rotate failed: vr=%+v err=%v", vr, err)
	}

	// Batch 3 after local rotation spends rotTx1:0 signed by SigningPubKey!
	if _, err := svc.Remember(ctx, RememberInput{Shard: "lineage", Text: "batch 3 after local rotate"}); err != nil {
		t.Fatalf("Remember(3): %v", err)
	}
	a3, err := svc.AnchorShard(ctx, "lineage")
	if err != nil || a3 == nil {
		t.Fatalf("AnchorShard(3) after local Rotate failed: %v", err)
	}
	_, tx3, _, err := guard.ParseBEEF(a3.Beef, guard.DefaultBound)
	if err != nil {
		t.Fatalf("parse a3 BEEF: %v", err)
	}
	if err := contracts.VerifyAnchorUTXOLineage(rotTx1, tx3); err != nil {
		t.Fatalf("Batch 3 lineage after Rotate failed: %v", err)
	}

	// Rotate ownership away to an external key NOT held by this wallet -> future AnchorShard MUST fail closed!
	extKey, _ := ec.NewPrivateKey()
	if _, err := svc.RotateShardOwner(ctx, "lineage", hex.EncodeToString(extKey.PubKey().Compressed())); err != nil {
		t.Fatalf("RotateShardOwner(external): %v", err)
	}
	if _, err := svc.Remember(ctx, RememberInput{Shard: "lineage", Text: "unauthorized batch 4"}); err != nil {
		t.Fatalf("Remember(4): %v", err)
	}
	if _, err := svc.AnchorShard(ctx, "lineage"); err == nil {
		t.Fatal("CRITICAL: AnchorShard succeeded after rotating shard ownership to an external key!")
	}
}
