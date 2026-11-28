package memory

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

func newLifecycleTestNode(t *testing.T) (*Service, *store.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = port.Close() })
	objStore := bstack.NewMemoryObjectStore()
	svc, err := New(db, port, bstack.NewGroupRegistry(), objStore, Config{})
	if err != nil {
		t.Fatal(err)
	}
	return svc, db
}

// TestForgetTombstonePreservesCapsuleVerify verifies strict append-only cryptographic erasure:
// historical commits stay immutable, a signed Tombstone commit removes the path from HEAD,
// VerifyShardChain passes, and ExportCapsule + VerifyCapsule passes with BlobsRedacted >= 1.
func TestForgetTombstonePreservesCapsuleVerify(t *testing.T) {
	ctx := context.Background()
	svc, db := newLifecycleTestNode(t)

	r1, err := svc.Remember(ctx, RememberInput{
		Shard:  "erasure",
		Path:   "keep.md",
		Text:   "Keep this public fact forever.",
		Public: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := svc.Remember(ctx, RememberInput{
		Shard:  "erasure",
		Path:   "secret-to-forget.md",
		Text:   "SENSITIVE_SECRET_TOKEN_XYZ_987",
		Public: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AnchorShard(ctx, "erasure"); err != nil {
		t.Fatalf("AnchorShard: %v", err)
	}
	origCommitBefore, err := db.GetMemCommit(r2.CommitHash)
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.Forget(ctx, "erasure", r2.Seq); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	// Historical commit MUST NOT be mutated!
	origCommitAfter, err := db.GetMemCommit(r2.CommitHash)
	if err != nil {
		t.Fatal(err)
	}
	if origCommitAfter.Message != origCommitBefore.Message || origCommitAfter.Signature != origCommitBefore.Signature {
		t.Fatalf("Forget mutated historical commit! before=%q after=%q", origCommitBefore.Message, origCommitAfter.Message)
	}

	// HEAD commit must be a signed Tombstone commit removing secret-to-forget.md.
	br, err := db.GetMemBranch("erasure", "main")
	if err != nil {
		t.Fatal(err)
	}
	headRow, err := db.GetMemCommit(br.HeadCommit)
	if err != nil {
		t.Fatal(err)
	}
	if headRow.CommitKind != memrepo.CommitKindTombstone {
		t.Fatalf("expected HEAD commit to have CommitKindTombstone (%d), got %d", memrepo.CommitKindTombstone, headRow.CommitKind)
	}

	// VerifyShardChain must pass including the tombstone transition.
	rep, err := svc.VerifyShardChain("erasure")
	if err != nil || !rep.OK || !rep.AnchorsOK {
		t.Fatalf("VerifyShardChain failed after Forget tombstone: rep=%+v err=%v", rep, err)
	}

	// ExportCapsule -> VerifyCapsule MUST pass with BlobsRedacted >= 1 and ZERO secret leakage!
	_, capObj, raw, err := svc.ExportCapsule(ctx, "erasure", "main", "")
	if err != nil {
		t.Fatalf("ExportCapsule: %v", err)
	}
	if bytes.Contains(raw, []byte("SENSITIVE_SECRET_TOKEN_XYZ_987")) {
		t.Fatal("forgotten plaintext leaked into raw exported .nkt capsule")
	}
	ver, err := memrepo.VerifyCapsuleWithOpts(ctx, capObj, memrepo.CapsuleVerifyOpts{
		Objects:              svc.Objects(),
		Tracker:              svc.Chain().Tracker(),
		MemoryMode:           true,
		RequireSignedTrailer: true,
	})
	if err != nil || !ver.OK {
		t.Fatalf("VerifyCapsule failed after Forget tombstone: ver=%+v err=%v", ver, err)
	}
	if ver.BlobsRedacted < 1 {
		t.Fatalf("expected BlobsRedacted >= 1 for forgotten blob stub, got %+v", ver)
	}
	if ver.TransitionsVerified < 3 {
		t.Fatalf("expected >= 3 verified transitions (2 writes + 1 tombstone), got %d", ver.TransitionsVerified)
	}
	_ = r1
}

// TestCedarFilteredExportVerifies verifies that Cedar-filtered capsule export preserves exact
// historical TreeHashes via hash-preserving BlobStubs so offline VerifyCapsule passes 100%.
func TestCedarFilteredExportVerifies(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLifecycleTestNode(t)

	if _, err := svc.Remember(ctx, RememberInput{
		Shard:  "cedar-cap",
		Path:   "open.md",
		Text:   "Level 0 open fact",
		Level:  0,
		Public: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Remember(ctx, RememberInput{
		Shard:  "cedar-cap",
		Path:   "classified.md",
		Text:   "LEVEL_50_CLASSIFIED_DATA_ABC",
		Level:  50,
		Public: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AnchorShard(ctx, "cedar-cap"); err != nil {
		t.Fatalf("AnchorShard: %v", err)
	}

	lowPrincipal := policy.Principal{Type: "Agent", ID: "guest", Level: 0}
	_, capObj, raw, err := svc.ExportCapsuleAuthorized(ctx, "cedar-cap", "main", "", lowPrincipal, true)
	if err != nil {
		t.Fatalf("ExportCapsuleAuthorized: %v", err)
	}
	if bytes.Contains(raw, []byte("LEVEL_50_CLASSIFIED_DATA_ABC")) {
		t.Fatal("Level 50 plaintext leaked into Level 0 Cedar-filtered capsule")
	}
	decoded, ver, err := memrepo.VerifyCapsuleStream(ctx, bytes.NewReader(raw), memrepo.CapsuleVerifyOpts{
		Objects:              svc.Objects(),
		Tracker:              svc.Chain().Tracker(),
		MemoryMode:           true,
		RequireSignedTrailer: true,
	})
	if err != nil || !ver.OK {
		t.Fatalf("Cedar-filtered capsule failed VerifyCapsuleStream: ver=%+v err=%v", ver, err)
	}
	if ver.BlobsRedacted < 1 {
		t.Fatalf("expected BlobsRedacted >= 1 for Cedar-denied blob stub, got %+v", ver)
	}
	for _, b := range decoded.Blobs {
		if b.Path == "classified.md" && (!b.Redacted || len(b.Inline) != 0) {
			t.Fatalf("expected classified.md to be redacted stub, got %+v", b)
		}
	}
	_ = capObj
}

// TestImportMergeAtomicSingleTx verifies single-tx import+merge, OriginSeq preservation, and 2-parent merge commit.
func TestImportMergeAtomicSingleTx(t *testing.T) {
	ctx := context.Background()
	seller, _ := newLifecycleTestNode(t)
	buyer, buyerDB := newLifecycleTestNode(t)

	if _, err := seller.Remember(ctx, RememberInput{
		Shard:  "seller-kb",
		Path:   "insight.md",
		Text:   "Seller verified market insight #1",
		Public: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := seller.AnchorShard(ctx, "seller-kb"); err != nil {
		t.Fatal(err)
	}
	_, _, raw, err := seller.ExportCapsule(ctx, "seller-kb", "main", "")
	if err != nil {
		t.Fatal(err)
	}

	// Buyer already has local commits in target repo so local seq != seller seq.
	for i := 0; i < 2; i++ {
		if _, err := buyer.Remember(ctx, RememberInput{
			Shard:  "buyer-kb",
			Path:   "local.md",
			Text:   "Buyer local fact " + strings.Repeat("x", i+1),
			Public: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	imp, err := buyer.ImportCapsule(ctx, raw, "", "buyer-kb", "main", true)
	if err != nil {
		t.Fatalf("ImportCapsule: %v", err)
	}
	if imp.MergeCommitHash == "" || imp.ImportedCommits != 1 || imp.ImportedFrags != 1 {
		t.Fatalf("unexpected import result: %+v", imp)
	}
	mc, err := buyerDB.GetMemCommit(imp.MergeCommitHash)
	if err != nil {
		t.Fatal(err)
	}
	mcObj := commitFromRow(mc)
	if len(mcObj.Parents) != 2 {
		t.Fatalf("expected 2-parent merge commit, got %d parents: %+v", len(mcObj.Parents), mcObj.Parents)
	}
	// Verify imported commit preserves OriginSeq=1 while local Seq=3!
	commits, err := buyerDB.ListAllMemCommits("buyer-kb")
	if err != nil {
		t.Fatal(err)
	}
	foundOrigin := false
	for _, c := range commits {
		if c.OriginRepo == "seller-kb" {
			foundOrigin = true
			if c.OriginSeq != 1 || c.Seq != 3 {
				t.Fatalf("expected imported commit OriginSeq=1 and local Seq=3, got OriginSeq=%d Seq=%d", c.OriginSeq, c.Seq)
			}
		}
	}
	if !foundOrigin {
		t.Fatal("missing imported seller commit row with OriginRepo=seller-kb")
	}
}

// TestIncrementalDeltaCapsuleMMR verifies incremental sinceCommit export slices anchors to n > baseAnchorN,
// emits an MMRConsistencyProof, and verifies cleanly against receiver's LocalBaseAnchor.
func TestIncrementalDeltaCapsuleMMR(t *testing.T) {
	ctx := context.Background()
	src, _ := newLifecycleTestNode(t)
	dst, _ := newLifecycleTestNode(t)

	c1, err := src.Remember(ctx, RememberInput{
		Shard:      "sync-repo",
		Path:       "v1.md",
		Text:       "Batch zero baseline fact",
		Public:     true,
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, fullRaw, err := src.ExportCapsule(ctx, "sync-repo", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.ImportCapsule(ctx, fullRaw, "", "sync-repo", "main", false); err != nil {
		t.Fatalf("initial full import: %v", err)
	}

	// Append a second anchored batch on src, then export incremental delta since c1.
	if _, err := src.Remember(ctx, RememberInput{
		Shard:      "sync-repo",
		Path:       "v2.md",
		Text:       "Batch one incremental delta fact",
		Public:     true,
		AutoAnchor: true,
	}); err != nil {
		t.Fatal(err)
	}
	_, deltaCap, deltaRaw, err := src.ExportCapsule(ctx, "sync-repo", "main", c1.CommitHash)
	if err != nil {
		t.Fatalf("incremental ExportCapsule: %v", err)
	}
	if deltaCap.BaseAnchorN != 0 || len(deltaCap.Anchors) != 1 || deltaCap.Anchors[0].N != 1 {
		t.Fatalf("expected incremental delta BaseAnchorN=0 with 1 anchor (n=1), got BaseAnchorN=%d anchors=%d", deltaCap.BaseAnchorN, len(deltaCap.Anchors))
	}
	if deltaCap.AnchorConsistency == nil {
		t.Fatal("expected incremental delta capsule to include MMRConsistencyProof")
	}
	imp, err := dst.ImportCapsule(ctx, deltaRaw, "", "sync-repo", "main", false)
	if err != nil || !imp.Verification.OK {
		t.Fatalf("incremental delta ImportCapsule failed: imp=%+v err=%v", imp, err)
	}
	if imp.Verification.AnchorsVerified != 1 || imp.Verification.TransitionsVerified != 1 {
		t.Fatalf("expected 1 delta anchor and 1 transition verified, got %+v", imp.Verification)
	}
}

// TestSingleSourceBlobStorage verifies non-system fragments store payload once in mem_objects
// (NULL data/keyed_meta in fragments table) while hydrating transparently on read.
func TestSingleSourceBlobStorage(t *testing.T) {
	ctx := context.Background()
	svc, db := newLifecycleTestNode(t)

	rPub, err := svc.Remember(ctx, RememberInput{
		Shard:  "dedup-store",
		Path:   "pub.md",
		Text:   "Single source public payload text",
		Public: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	rEnc, err := svc.Remember(ctx, RememberInput{
		Shard: "dedup-store",
		Path:  "priv.md",
		Text:  "Single source encrypted CLKA payload text",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Raw SQLite check: fragments.data and fragments.keyed_meta MUST be NULL while blob_hash is populated!
	var pubDataLen int
	var pubBlobHash string
	if err := db.Conn().QueryRow(`SELECT COALESCE(length(data), 0), blob_hash FROM fragments WHERE shard = ? AND seq = ?`, "dedup-store", rPub.Seq).Scan(&pubDataLen, &pubBlobHash); err != nil {
		t.Fatal(err)
	}
	if pubDataLen != 0 || pubBlobHash == "" {
		t.Fatalf("expected fragments.data to be NULL (single-source in mem_objects) with non-empty blob_hash, got len=%d blob_hash=%q", pubDataLen, pubBlobHash)
	}
	var encKMLen, encCTLen int
	var encBlobHash string
	if err := db.Conn().QueryRow(`SELECT COALESCE(length(keyed_meta), 0), COALESCE(length(ciphertext), 0), blob_hash FROM fragments WHERE shard = ? AND seq = ?`, "dedup-store", rEnc.Seq).Scan(&encKMLen, &encCTLen, &encBlobHash); err != nil {
		t.Fatal(err)
	}
	if encKMLen != 0 || encCTLen != 0 || encBlobHash == "" {
		t.Fatalf("expected fragments.keyed_meta & ciphertext NULL with non-empty blob_hash, got km=%d ct=%d hash=%q", encKMLen, encCTLen, encBlobHash)
	}

	// Transparent hydration via GetFragment & Recall & VerifyShardChain must work seamlessly!
	fPub, err := db.GetFragment("dedup-store", rPub.Seq)
	if err != nil || string(fPub.Data) != "Single source public payload text" {
		t.Fatalf("hydrated public fragment mismatch: data=%q err=%v", string(fPub.Data), err)
	}
	fEnc, err := db.GetFragment("dedup-store", rEnc.Seq)
	if err != nil || len(fEnc.KeyedMeta) == 0 || len(fEnc.Ciphertext) == 0 {
		t.Fatalf("hydrated encrypted fragment missing KeyedMeta/Ciphertext: km=%d ct=%d err=%v", len(fEnc.KeyedMeta), len(fEnc.Ciphertext), err)
	}
	rep, err := svc.VerifyShardChain("dedup-store")
	if err != nil || !rep.OK {
		t.Fatalf("VerifyShardChain failed on single-source storage: rep=%+v err=%v", rep, err)
	}
}
