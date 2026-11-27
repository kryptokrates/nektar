package memory

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memrepo"
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

	// F-11: Borg-encrypted fragment TagLeafV2 MUST commit to H(E) (non-empty ciphertext) + MPERoot, not SHA256("").
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
	mpeBytes, _ := hex.DecodeString(rec.MPERoot)
	contentBytes, _ := hex.DecodeString(rec.ContentSHA256)
	wantLeaf := core.ToHex(core.ComputeLeafV2(core.Fragment{
		HDP:           hdpBytes,
		Ciphertext:    rec.Ciphertext,
		MPERoot:       mpeBytes,
		ContentSHA256: contentBytes,
	}))
	emptyCTLeaf := core.ToHex(core.ComputeLeafV2(core.Fragment{
		HDP:           hdpBytes,
		Ciphertext:    nil,
		MPERoot:       mpeBytes,
		ContentSHA256: contentBytes,
	}))
	if rec.Leaf != wantLeaf || rec.Leaf == emptyCTLeaf {
		t.Fatalf("F-11: TagLeafV2 did not commit to ciphertext: got=%s want=%s emptyCT=%s", rec.Leaf, wantLeaf, emptyCTLeaf)
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
// .nkt capsule fragment MUST fail closed on import (never accept unverified plaintext!).
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
	capObj, err := memrepo.DecodeCapsule(raw)
	if err != nil {
		t.Fatalf("DecodeCapsule: %v", err)
	}
	if len(capObj.Fragments) != 1 || capObj.Fragments[0].BatchN < 0 {
		t.Fatalf("expected 1 anchored fragment in exported capsule, got %+v", capObj.Fragments)
	}
	capObj.Fragments[0].Data = []byte("forged plaintext without provenance")
	capObj.Fragments[0].Provenance = []byte{}
	tamperedRaw, err := memrepo.EncodeCapsule(capObj)
	if err != nil {
		t.Fatalf("EncodeCapsule: %v", err)
	}

	dst, _ := newTestMemoryService(t)
	if _, _, err := dst.ImportShardPackBytesCtx(ctx, tamperedRaw, "imported-legit"); err == nil {
		t.Fatal("F-09 REGRESSION: expected tampered empty-provenance capsule import to fail closed")
	}
	frags, err := dst.DB().ListFragments("imported-legit")
	if err != nil {
		t.Fatalf("ListFragments: err=%v", err)
	}
	if len(frags) != 0 {
		t.Fatalf("F-09 REGRESSION: forged capsule left %d fragments persisted locally", len(frags))
	}
}
