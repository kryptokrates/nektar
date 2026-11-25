package memory

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

func newTestMemoryService(t *testing.T) (*Service, *chain.Port) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	port, err := chain.Open(context.Background(), chain.Config{Network: "memory", DataDir: dir})
	if err != nil {
		t.Fatalf("chain.Open: %v", err)
	}
	t.Cleanup(func() { _ = port.Close() })
	svc, err := New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), Config{
		VerifyBaseURL: "http://localhost:8080/v1/verify",
	})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	return svc, port
}

// TestHardeningEncryptedLeafAndSignedReceipt verifies F-10, F-11, and F-32 end-to-end.
func TestHardeningEncryptedLeafAndSignedReceipt(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestMemoryService(t)

	// F-11: Borg-encrypted fragment leaf MUST commit to H(E) (non-empty ciphertext), not SHA256("").
	res, err := svc.Remember(ctx, RememberInput{
		Shard:     "sec",
		Text:      "classified quarterly forecast alpha",
		BorgGroup: "team-sec",
	})
	if err != nil {
		t.Fatalf("Remember(BorgGroup): %v", err)
	}
	rec, err := svc.DB().GetFragment("sec", res.Seq)
	if err != nil {
		t.Fatalf("GetFragment: %v", err)
	}
	if len(rec.Ciphertext) == 0 {
		t.Fatal("expected non-empty BRC-369 ciphertext")
	}
	hdpBytes, _ := hex.DecodeString(rec.HDP)
	wantLeaf := core.ToHex(core.ComputeLeaf(core.Fragment{HDP: hdpBytes, Ciphertext: rec.Ciphertext}))
	emptyCTLeaf := core.ToHex(core.ComputeLeaf(core.Fragment{HDP: hdpBytes, Ciphertext: nil}))
	if rec.Leaf != wantLeaf || rec.Leaf == emptyCTLeaf {
		t.Fatalf("F-11: leaf did not commit to ciphertext: got=%s want=%s emptyCT=%s", rec.Leaf, wantLeaf, emptyCTLeaf)
	}

	// F-32: GroundedAnswer signs Receipt under TagReceiptSig and VerifyReceipt verifies it.
	plain, err := svc.Remember(ctx, RememberInput{
		Shard: "pub",
		Text:  "Nektar uses pure Go cryptographic verification.",
	})
	if err != nil {
		t.Fatalf("Remember(pub): %v", err)
	}
	ga, err := svc.GroundedAnswer(ctx, GroundedAnswerInput{
		Claims: []reasoning.Claim{{
			Text:  "Nektar uses pure Go cryptographic verification.",
			Cites: []reasoning.FragmentRef{{Shard: "pub", Seq: plain.Seq}},
		}},
	})
	if err != nil || ga.Ungrounded || ga.Receipt == nil {
		t.Fatalf("GroundedAnswer failed: err=%v res=%+v", err, ga)
	}
	if ga.Receipt.IssuerPub == "" || ga.Receipt.IssuerSig == "" {
		t.Fatalf("F-32: GroundedAnswer emitted unsigned receipt: %+v", ga.Receipt)
	}
	vr, err := svc.VerifyReceipt(ctx, ga.ReceiptToken)
	if err != nil || !vr.OK {
		t.Fatalf("VerifyReceipt failed: err=%v vr=%+v", err, vr)
	}
}

// TestShardPackRejectsEmptyProvenanceForgery verifies F-09: stripping Provenance from an anchored
// shard pack fragment MUST strip its anchored status on import (never trust unverified plaintext!).
func TestShardPackRejectsEmptyProvenanceForgery(t *testing.T) {
	ctx := context.Background()
	src, _ := newTestMemoryService(t)
	if _, err := src.Remember(ctx, RememberInput{
		Shard:      "legit",
		Text:       "original anchored memory",
		AutoAnchor: true,
	}); err != nil {
		t.Fatalf("Remember+AutoAnchor: %v", err)
	}
	res, err := src.ExportPublicShardPack(ctx, "legit")
	if err != nil {
		t.Fatalf("ExportPublicShardPack: %v", err)
	}
	raw, err := bstack.ResolveChirp(src.Objects(), res.ChirpURL)
	if err != nil {
		t.Fatalf("ResolveChirp: %v", err)
	}
	var pack ShardPack
	if err := json.Unmarshal(raw, &pack); err != nil {
		t.Fatalf("unmarshal pack: %v", err)
	}
	if len(pack.Fragments) != 1 || pack.Fragments[0].BatchN < 0 {
		t.Fatalf("expected 1 anchored fragment in exported pack, got %+v", pack.Fragments)
	}
	pack.Fragments[0].Data = []byte("forged plaintext without provenance")
	pack.Fragments[0].Provenance = []byte{}
	tamperedRaw, _ := json.Marshal(pack)

	dst, _ := newTestMemoryService(t)
	_, importedShard, err := dst.importShardPackVerified(ctx, tamperedRaw, "imported-legit")
	if err != nil {
		t.Fatalf("importShardPackVerified: %v", err)
	}
	frags, err := dst.DB().ListFragments(importedShard)
	if err != nil {
		t.Fatalf("ListFragments: err=%v", err)
	}
	// Either the forged empty-provenance fragment was dropped or its anchored status was stripped.
	for _, f := range frags {
		if f.BatchN != -1 || f.AnchorTxID != "" {
			t.Fatalf("F-09 REGRESSION: empty-provenance forged fragment retained anchored status! BatchN=%d AnchorTxID=%q", f.BatchN, f.AnchorTxID)
		}
	}
}
