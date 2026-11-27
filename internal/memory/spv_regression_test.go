package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/store"
)

type failingTracker struct{ err error }

func (f failingTracker) IsValidRootForHeight(_ context.Context, _ *chainhash.Hash, _ uint32) (bool, error) {
	return false, f.err
}
func (f failingTracker) CurrentHeight(_ context.Context) (uint32, error) {
	return 0, f.err
}

func TestAnchorShardEmbedsVerifiedBUMPAndHandleReorgClearsProof(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	svc, err := New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), Config{})
	if err != nil {
		t.Fatal(err)
	}

	rem, err := svc.Remember(ctx, RememberInput{Shard: "spv-test", Text: "Embedded BUMP regression fact"})
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := svc.AnchorShard(ctx, "spv-test")
	if err != nil {
		t.Fatalf("AnchorShard: %v", err)
	}
	if !anchor.Settled || len(anchor.Beef) == 0 {
		t.Fatalf("expected settled anchor with non-empty BEEF, got settled=%v len=%d", anchor.Settled, len(anchor.Beef))
	}
	// Verify stored anchors.beef actually carries the embedded MerklePath (Finding #4)!
	tx, err := transaction.NewTransactionFromBEEF(anchor.Beef)
	if err != nil || tx == nil || tx.MerklePath == nil {
		t.Fatalf("expected persisted anchors.beef to contain embedded MerklePath, got tx=%v err=%v", tx, err)
	}
	pb, vr, err := svc.Prove(ctx, "spv-test", rem.Seq)
	if err != nil || pb == nil || !vr.OK || !vr.Settled {
		t.Fatalf("expected Prove() to return OK=true Settled=true, got vr=%+v err=%v", vr, err)
	}

	// Verify HandleReorg clears settled=0 and strips orphaned MerklePath from anchors.beef!
	cleared, err := svc.HandleReorg(uint32(anchor.BlockHeight))
	if err != nil || cleared != 1 {
		t.Fatalf("expected HandleReorg to clear 1 anchor at height %d, got cleared=%d err=%v", anchor.BlockHeight, cleared, err)
	}
	if settled, _ := db.IsAnchorSettled(anchor.TxID); settled {
		t.Fatal("expected anchor to be unsettled after HandleReorg")
	}
	reorgedAnchors, err := db.ListAnchors("spv-test")
	if err != nil || len(reorgedAnchors) != 1 {
		t.Fatalf("ListAnchors after HandleReorg: len=%d err=%v", len(reorgedAnchors), err)
	}
	if reorgedTx, pErr := transaction.NewTransactionFromBEEF(reorgedAnchors[0].Beef); pErr != nil || reorgedTx == nil || reorgedTx.MerklePath != nil {
		t.Fatalf("expected HandleReorg to strip orphaned MerklePath from anchors.beef, got mp=%v err=%v", reorgedTx.MerklePath, pErr)
	}

	// Verify Stage 3 fix_ok:false invariant (#28): unmined VerifyFragmentCtx keeps OK=false Reason="Unsettled"
	// while still validating carried ancestry and ChainTail BEFORE Unsettled!
	txNoBump, err := transaction.NewTransactionFromBEEF(anchor.Beef)
	if err != nil {
		t.Fatal(err)
	}
	txNoBump.MerklePath = nil
	for _, inp := range txNoBump.Inputs {
		if inp.SourceTransaction == nil && inp.SourceTXID != nil {
			if src := port.Tx(inp.SourceTXID.String()); src != nil {
				if srcMP, _, ok := port.Proof(ctx, inp.SourceTXID.String()); ok && srcMP != nil {
					srcCopy := *src
					srcCopy.MerklePath = srcMP
					inp.SourceTransaction = &srcCopy
				} else {
					inp.SourceTransaction = src
				}
			}
		}
	}
	unminedBEEF, err := txNoBump.AtomicBEEF(false)
	if err != nil {
		t.Fatal(err)
	}
	unminedPB := *pb
	unminedPB.AnchorBEEF = unminedBEEF
	vrUnmined := contracts.VerifyFragmentCtx(ctx, unminedPB, port.Tracker())
	if vrUnmined.OK || vrUnmined.Reason != "Unsettled" || !vrUnmined.AncestryVerified {
		t.Fatalf("expected unmined VerifyFragmentCtx to keep OK=false Reason=Unsettled AncestryVerified=true, got %+v", vrUnmined)
	}

	// Verify tracker outage produces Reason="TrackerUnavailable" (not "BEEFInvalid") (#18)!
	vrOutage := contracts.VerifyFragmentCtx(ctx, *pb, failingTracker{err: errors.New("upstream timeout")})
	if vrOutage.OK || vrOutage.Reason != "TrackerUnavailable" {
		t.Fatalf("expected TrackerUnavailable on tracker outage, got %+v", vrOutage)
	}
}
