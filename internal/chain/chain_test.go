package chain

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
)

func TestMemoryPortBroadcastAnchorAndSPVVerify(t *testing.T) {
	ctx := context.Background()
	port, err := Open(ctx, Config{
		Network: "memory",
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Open memory port: %v", err)
	}
	if port.BalanceSats() == 0 {
		t.Fatal("expected seeded coinbase balance in embedded bwallet pool")
	}

	// Build and broadcast a real NektarAnchor tx onto bcommon/testchain, then SPV-verify a fragment!
	pubBytes := port.Signer().IdentityKey().Compressed()
	leaf := core.ComputeLeaf(core.Fragment{
		Data:       []byte("first anchored Go fact"),
		Provenance: core.Provenance{Source: "go-test", Time: 1730000000, ModelID: "test"},
	})
	root, _ := core.MerkleRoot([][]byte{leaf})
	rp := core.ChainRp(core.Zero32, root, 1)
	state := contracts.AnchorState{
		Shard:    core.ShardIDFromPubkey(pubBytes),
		OwnerPub: port.IdentityPubHex(),
		OwnerPKH: core.Hash160(pubBytes),
		N:        0,
		Size:     1,
		R:        root,
		Rp:       rp,
		Tag:      core.TagAnchorTag,
		Metanet:  true,
	}
	lockScript, err := contracts.EncodeAnchorLockingScript(state)
	if err != nil {
		t.Fatalf("EncodeAnchorLockingScript: %v", err)
	}
	tx, fee, spend, err := port.BuildFundedTx(ctx, []*transaction.TransactionOutput{AnchorOutput(lockScript)})
	if err != nil {
		t.Fatalf("BuildFundedTx: %v", err)
	}
	txid, err := port.Broadcast(ctx, tx, fee, spend)
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if err := port.AdoptChange(tx); err != nil {
		t.Fatalf("AdoptChange: %v", err)
	}

	beef, err := tx.AtomicBEEF(false)
	if err != nil {
		t.Fatalf("AtomicBEEF: %v", err)
	}
	link := contracts.AnchorLink{
		TxID:        txid,
		OutputIndex: 0,
		N:           0,
		Size:        1,
		R:           core.ToHex(root),
		Rp:          core.ToHex(rp),
	}
	bundle := contracts.ProofBundle{
		Version:    1,
		Shard:      state.Shard,
		N:          0,
		Size:       1,
		Leaf:       core.ToHex(leaf),
		LeafIndex:  0,
		Path:       nil,
		Anchor:     link,
		ChainTail:  []contracts.AnchorLink{link},
		AnchorBEEF: beef,
	}
	res := contracts.VerifyFragment(bundle, port.Tracker())
	if !res.OK || !res.Settled || res.AnchorState != "mined" {
		t.Fatalf("expected mined SPV verification, got: %+v", res)
	}

	// Reopening the same dataDir in memory mode must preserve identity and reconcile pool cleanly!
	port2, err := Open(ctx, Config{Network: "memory", DataDir: port.dataDir})
	if err != nil {
		t.Fatalf("reopen memory port on same dataDir: %v", err)
	}
	if port2.IdentityPubHex() != port.IdentityPubHex() {
		t.Fatalf("identity pub changed across reopen: %s vs %s", port2.IdentityPubHex(), port.IdentityPubHex())
	}
	if port2.BalanceSats() == 0 {
		t.Fatal("reopened memory port has zero balance")
	}
}
