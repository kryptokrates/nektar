package memrepo

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
)

func TestBlobTreeCommitAndVerify(t *testing.T) {
	store := bstack.NewMemoryObjectStore()
	priv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())
	signFn := func(digest []byte) ([]byte, error) {
		sig, err := priv.Sign(digest)
		if err != nil {
			return nil, err
		}
		return sig.Serialize(), nil
	}

	// 1. Small inline blob + large volumetric CHIRP blob (> 64 KiB).
	small, _, err := BuildBlob(store, "facts/chronicle.md", "text/markdown", []byte("Chronicle enables OP_PUSH_TX."), false)
	if err != nil {
		t.Fatalf("BuildBlob small: %v", err)
	}
	if small.ChirpURL != "" || len(small.Inline) == 0 {
		t.Fatalf("expected inline small blob, got chirp=%q inline=%d", small.ChirpURL, len(small.Inline))
	}

	bigPayload := bytes.Repeat([]byte("%PDF-1.7 nektar volumetric memory test\n"), 2200)
	big, _, err := BuildBlob(store, "files/whitepaper.pdf", "application/pdf", bigPayload, false)
	if err != nil {
		t.Fatalf("BuildBlob big: %v", err)
	}
	if big.ChirpURL == "" || len(big.Inline) != 0 {
		t.Fatalf("expected CHIRP volumetric blob, got chirp=%q inline=%d", big.ChirpURL, len(big.Inline))
	}
	if resolved, err := bstack.ResolveChirp(store, big.ChirpURL); err != nil || !bytes.Equal(resolved, bigPayload) {
		t.Fatalf("ResolveChirp mismatch: err=%v len=%d", err, len(resolved))
	}

	// Reject path traversal & reserved .git paths.
	if _, _, err := BuildBlob(store, "../escape.txt", "", []byte("x"), false); err == nil {
		t.Fatal("expected path traversal rejection")
	}
	if _, _, err := BuildBlob(store, ".git/HEAD", "", []byte("x"), false); err == nil {
		t.Fatal("expected .git path rejection")
	}

	// 2. Build two trees and verify Git-style diff.
	t1, err := BuildTree([]TreeEntry{{
		Path:          small.Path,
		BlobHash:      small.BlobHash,
		MediaType:     small.MediaType,
		Size:          small.Size,
		ContentSHA256: small.ContentSHA256,
	}})
	if err != nil {
		t.Fatalf("BuildTree t1: %v", err)
	}
	t2, err := BuildTree([]TreeEntry{
		{Path: small.Path, BlobHash: small.BlobHash, MediaType: small.MediaType, Size: small.Size, ContentSHA256: small.ContentSHA256},
		{Path: big.Path, BlobHash: big.BlobHash, MediaType: big.MediaType, Size: big.Size, ContentSHA256: big.ContentSHA256, ChirpURL: big.ChirpURL},
	})
	if err != nil {
		t.Fatalf("BuildTree t2: %v", err)
	}
	diff := DiffTrees(t1, t2)
	if len(diff) != 1 || diff[0].Status != "added" || diff[0].Path != "files/whitepaper.pdf" {
		t.Fatalf("unexpected diff: %+v", diff)
	}

	// 3. Finalize & verify signed commits + tamper rejection.
	now := time.Now().Unix()
	c1, err := FinalizeCommit(CommitObject{
		Version:   1,
		Repo:      "bsv-research",
		Branch:    "main",
		TreeHash:  t1.TreeHash,
		AuthorPub: pubHex,
		ModelID:   "nektar/test",
		Message:   "Initial Chronicle fact",
		Timestamp: now,
		AnchorN:   -1,
	}, signFn)
	if err != nil {
		t.Fatalf("FinalizeCommit c1: %v", err)
	}
	if err := VerifyCommitObject(c1); err != nil {
		t.Fatalf("VerifyCommitObject c1: %v", err)
	}
	tampered := c1
	tampered.Message = "forged message"
	if err := VerifyCommitObject(tampered); err == nil {
		t.Fatal("expected VerifyCommitObject to reject tampered commit")
	}
	// Unsigned commits MUST fail closed even with matching CommitHash (F2).
	unsigned, err := FinalizeCommit(CommitObject{
		Version:   1,
		Repo:      "bsv-research",
		Branch:    "main",
		TreeHash:  t1.TreeHash,
		AuthorPub: pubHex,
		ModelID:   "nektar/test",
		Message:   "Unsigned forged commit",
		Timestamp: now,
		AnchorN:   -1,
	}, nil)
	if err != nil {
		t.Fatalf("FinalizeCommit unsigned: %v", err)
	}
	if err := VerifyCommitObject(unsigned); err == nil {
		t.Fatal("F2: expected VerifyCommitObject to reject unsigned commit")
	}
	// Trailing junk bytes on DER signature MUST fail strict low-S DER verification (F5).
	malleated := c1
	malleated.Signature = c1.Signature + "00"
	malleated.CommitHash, _ = ComputeCommitHash(malleated)
	if err := VerifyCommitObject(malleated); err == nil {
		t.Fatal("F5: expected VerifyCommitObject to reject trailing-byte malleable DER signature")
	}

	c2, err := FinalizeCommit(CommitObject{
		Version:   1,
		Repo:      "bsv-research",
		Branch:    "main",
		Parents:   []ParentRef{{CommitHash: c1.CommitHash}},
		TreeHash:  t2.TreeHash,
		AuthorPub: pubHex,
		ModelID:   "nektar/test",
		Message:   "Attach volumetric whitepaper PDF",
		Timestamp: now + 1,
		AnchorN:   -1,
	}, signFn)
	if err != nil {
		t.Fatalf("FinalizeCommit c2: %v", err)
	}

	// 4. Encode, decode, verify MemoryCapsule + export real .git workspace!
	cap := &MemoryCapsule{
		Format:     CapsuleFormatVersion,
		Repo:       "bsv-research",
		Branch:     "main",
		OwnerPub:   pubHex,
		HeadCommit: c2.CommitHash,
		ExportedAt: now + 2,
		Commits:    []CommitObject{c1, c2},
		Trees:      map[string]TreeObject{t1.TreeHash: *t1, t2.TreeHash: *t2},
		Blobs:      map[string]BlobObject{small.BlobHash: *small, big.BlobHash: *big},
	}
	raw, err := EncodeCapsule(cap)
	if err != nil {
		t.Fatalf("EncodeCapsule: %v", err)
	}
	if !IsCapsuleBytes(raw) {
		t.Fatal("IsCapsuleBytes returned false for encoded capsule")
	}
	decoded, err := DecodeCapsule(raw)
	if err != nil {
		t.Fatalf("DecodeCapsule: %v", err)
	}
	rep, err := VerifyCapsule(context.Background(), decoded, store, nil, true)
	if err != nil || !rep.OK || rep.CommitsVerified != 2 || rep.BlobsVerified != 2 {
		t.Fatalf("VerifyCapsule failed: err=%v rep=%+v", err, rep)
	}

	dir := filepath.Join(t.TempDir(), "bsv-research")
	exp, err := ExportGitWorkspace(dir, decoded, store)
	if err != nil {
		t.Fatalf("ExportGitWorkspace: %v", err)
	}
	if exp.GitHeadOID == "" || exp.CommitCount != 2 || exp.FileCount < 4 {
		t.Fatalf("unexpected git export result: %+v", exp)
	}
	for _, mustExist := range []string{
		filepath.Join(dir, ".git", "HEAD"),
		filepath.Join(dir, ".git", "refs", "heads", "main"),
		filepath.Join(dir, "MEMORY.md"),
		filepath.Join(dir, "facts", "chronicle.md"),
		filepath.Join(dir, "files", "whitepaper.pdf"),
		filepath.Join(dir, ".nektar", "lineage.json"),
	} {
		if _, err := os.Stat(mustExist); err != nil {
			t.Fatalf("expected exported file %s: %v", mustExist, err)
		}
	}
}

func TestProofShardingSubsetDisclosure(t *testing.T) {
	for _, count := range []int{1, 5, 64, 67, 200} {
		leaves := make([][]byte, count)
		for i := 0; i < count; i++ {
			leaves[i] = core.DSHA256(core.U64LE(uint64(i + 1)))
		}
		root, err := core.MerkleRoot(leaves)
		if err != nil {
			t.Fatalf("MerkleRoot(%d): %v", count, err)
		}
		rootHex := hex.EncodeToString(root)

		var indices []int
		switch count {
		case 1:
			indices = []int{0}
		case 5:
			indices = []int{0, 2, 4}
		case 64:
			indices = []int{0, 31, 63}
		case 67:
			indices = []int{0, 1, 63, 64, 66}
		case 200:
			indices = []int{0, 3, 63, 64, 129, 199}
		}

		proof, err := BuildSubsetProof(leaves, indices)
		if err != nil {
			t.Fatalf("BuildSubsetProof(%d): %v", count, err)
		}
		if err := VerifySubsetProof(rootHex, count, proof); err != nil {
			t.Fatalf("VerifySubsetProof(%d): %v", count, err)
		}
		// Tamper one leaf and verify rejection!
		bad := *proof
		badLeaves := append([]ShardedLeafProof(nil), proof.Leaves...)
		badLeaves[0].Leaf = hex.EncodeToString(core.DSHA256([]byte("tampered")))
		bad.Leaves = badLeaves
		if err := VerifySubsetProof(rootHex, count, &bad); err == nil {
			t.Fatalf("VerifySubsetProof(%d) accepted tampered leaf!", count)
		}
	}
}
