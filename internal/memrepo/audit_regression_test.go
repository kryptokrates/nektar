package memrepo

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/core"
)

func TestNormalizePathBlocksGitAndWindowsHazards(t *testing.T) {
	bad := []string{
		"",
		".",
		"..",
		"../etc/passwd",
		"a/../../b",
		".git/config",
		".GIT/config",
		".Git/hooks/post-checkout",
		".git./config",
		"a/.git /config",
		"GIT~1/config",
		"sub/git~2/HEAD",
		"C:secret.txt",
		"foo\x00bar.md",
		"foo\nbar.md",
	}
	for _, p := range bad {
		if got, err := NormalizePath(p); err == nil {
			t.Errorf("NormalizePath(%q) unexpectedly succeeded with %q", p, got)
		}
	}
	good := map[string]string{
		"docs/spec.md":             "docs/spec.md",
		"./notes/a.txt":            "notes/a.txt",
		".github/workflows/ci.yml": ".github/workflows/ci.yml",
	}
	for in, want := range good {
		got, err := NormalizePath(in)
		if err != nil || got != want {
			t.Errorf("NormalizePath(%q) = (%q, %v), want (%q, nil)", in, got, err, want)
		}
	}
}

func TestVerifyCommitObjectRejectsUnsignedAndMalleableSig(t *testing.T) {
	priv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())

	c := CommitObject{
		Version:   1,
		Repo:      "demo",
		Branch:    "main",
		TreeHash:  hex.EncodeToString(core.DSHA256([]byte("tree"))),
		AuthorPub: pubHex,
		Message:   "initial commit",
		Timestamp: 1700000000,
		AnchorN:   -1,
	}
	h, err := ComputeCommitHash(c)
	if err != nil {
		t.Fatal(err)
	}
	c.CommitHash = h

	// 1. Unsigned commit MUST fail closed.
	if err := VerifyCommitObject(c); err == nil {
		t.Fatal("expected unsigned commit to fail VerifyCommitObject")
	}

	// 2. Properly signed commit passes.
	signed, err := FinalizeCommit(c, func(digest []byte) ([]byte, error) {
		sig, sErr := priv.Sign(digest)
		if sErr != nil {
			return nil, sErr
		}
		return sig.Serialize(), nil
	})
	if err != nil {
		t.Fatalf("FinalizeCommit: %v", err)
	}
	if err := VerifyCommitObject(signed); err != nil {
		t.Fatalf("VerifyCommitObject(valid): %v", err)
	}

	// 3. Trailing byte on DER signature MUST fail strict low-S DER verification.
	badTrail := signed
	badTrail.Signature = signed.Signature + "00"
	if err := VerifyCommitObject(badTrail); err == nil {
		t.Fatal("expected trailing-byte DER signature to fail VerifyCommitObject")
	}

	// 4. High-S malleable signature MUST fail strict low-S check (encode raw DER directly since ec.Signature.Serialize auto-normalizes to low-S).
	sigBytes, _ := hex.DecodeString(signed.Signature)
	parsedSig, err := ec.ParseDERSignature(sigBytes)
	if err != nil {
		t.Fatal(err)
	}
	highS := new(big.Int).Sub(ec.S256().N, parsedSig.S)
	derInt := func(n *big.Int) []byte {
		b := n.Bytes()
		if len(b) == 0 || b[0]&0x80 != 0 {
			b = append([]byte{0x00}, b...)
		}
		return append([]byte{0x02, byte(len(b))}, b...)
	}
	rDER := derInt(parsedSig.R)
	sDER := derInt(highS)
	highSDER := append([]byte{0x30, byte(len(rDER) + len(sDER))}, append(rDER, sDER...)...)
	badHighS := signed
	badHighS.Signature = hex.EncodeToString(highSDER)
	badHighS.CommitHash, _ = ComputeCommitHash(badHighS)
	if err := VerifyCommitObject(badHighS); err == nil {
		t.Fatal("expected high-S malleable signature to fail VerifyCommitObject")
	}
}

func TestVerifySubsetProofEnforcesExpectedBatchSizeAndOddBoundary(t *testing.T) {
	leaves := make([][]byte, 5)
	for i := range leaves {
		leaves[i] = core.DSHA256([]byte{byte(i + 1)})
	}
	proof, err := BuildSubsetProof(leaves, []int{0, 4})
	if err != nil {
		t.Fatalf("BuildSubsetProof: %v", err)
	}
	if err := VerifySubsetProof(proof.RootHex, 5, proof); err != nil {
		t.Fatalf("VerifySubsetProof(valid): %v", err)
	}

	// Forged BatchSize: 1 proof claiming root == arbitrary leaf MUST be rejected when expectedBatchSize == 5.
	forged := &SubsetDisclosureProof{
		Version:    2,
		BatchSize:  1,
		RootHex:    proof.RootHex,
		ShardLevel: 0,
		Leaves: []ShardedLeafProof{
			{Index: 0, Leaf: proof.RootHex, ShardIdx: 0},
		},
	}
	if err := VerifySubsetProof(proof.RootHex, 5, forged); err == nil {
		t.Fatal("expected forged BatchSize=1 subset proof to fail against expectedBatchSize=5")
	}

	// Odd right-boundary leaf (index 4 in 5-leaf tree) MUST self-pair; replacing sibling with a different hash must fail.
	tampered := *proof
	tampered.Leaves = append([]ShardedLeafProof(nil), proof.Leaves...)
	tampered.Leaves[1].SubPath = append([]string(nil), proof.Leaves[1].SubPath...)
	tampered.Leaves[1].SubPath[0] = hex.EncodeToString(leaves[0])
	if err := VerifySubsetProof(proof.RootHex, 5, &tampered); err == nil {
		t.Fatal("expected odd-boundary self-pairing violation to fail VerifySubsetProof")
	}
}

func TestComputeBlobHashIncludesKeyedMeta(t *testing.T) {
	b1 := BlobObject{
		Path:          "secret.md",
		MediaType:     "text/markdown",
		Size:          12,
		ContentSHA256: hex.EncodeToString(core.DSHA256([]byte("ciphertext"))),
		Encrypted:     true,
		BorgGroup:     "team-a",
		BorgEpoch:     1,
		KeyedMeta:     []byte(`{"recipient":"alice"}`),
	}
	h1, err := ComputeBlobHash(b1)
	if err != nil {
		t.Fatal(err)
	}
	b2 := b1
	b2.KeyedMeta = []byte(`{"recipient":"mallory"}`)
	h2, err := ComputeBlobHash(b2)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatalf("ComputeBlobHash ignored KeyedMeta mutation: %s == %s", h1, h2)
	}
}

func TestMergeTextThreeWayLineLevelConflict(t *testing.T) {
	base := "header: v1\nprice: 1000\nfooter: ok\n"
	ours := "header: v1\nprice: 2000\nfooter: ok\n"
	theirs := "header: v1\nprice: 1050\nfooter: ok\n"

	if merged, ok := mergeTextThreeWay(base, ours, theirs); ok {
		t.Fatalf("same-line numeric edits MUST conflict instead of silently splicing, got %q", merged)
	}

	// Disjoint-line edits MUST auto-merge cleanly.
	oursDisjoint := "header: v2\nprice: 1000\nfooter: ok\n"
	theirsDisjoint := "header: v1\nprice: 1000\nfooter: updated\n"
	merged, ok := mergeTextThreeWay(base, oursDisjoint, theirsDisjoint)
	want := "header: v2\nprice: 1000\nfooter: updated\n"
	if !ok || merged != want {
		t.Fatalf("disjoint line merge = (%q, %v), want (%q, true)", merged, ok, want)
	}
}

func TestExportGitWorkspaceWritesValidIndexAndRejectsBadBranch(t *testing.T) {
	priv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())

	blob, _, err := BuildBlob(nil, "notes/hello.md", "text/markdown", []byte("hello world\n"), false)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := BuildTree([]TreeEntry{{
		Mode:          "100644",
		Path:          blob.Path,
		BlobHash:      blob.BlobHash,
		MediaType:     blob.MediaType,
		Size:          blob.Size,
		ContentSHA256: blob.ContentSHA256,
	}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := FinalizeCommit(CommitObject{
		Version:   1,
		Repo:      "demo",
		Branch:    "main",
		TreeHash:  tr.TreeHash,
		AuthorPub: pubHex,
		Message:   "init",
		Timestamp: 1700000000,
		AnchorN:   -1,
	}, func(digest []byte) ([]byte, error) {
		sig, sErr := priv.Sign(digest)
		if sErr != nil {
			return nil, sErr
		}
		return sig.Serialize(), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	capObj := &MemoryCapsule{
		Format:     CapsuleFormatVersion,
		Repo:       "demo",
		Branch:     "main",
		HeadCommit: c.CommitHash,
		Commits:    []CommitObject{c},
		Trees:      map[string]TreeObject{tr.TreeHash: *tr},
		Blobs:      map[string]BlobObject{blob.BlobHash: *blob},
	}

	// 1. Traversal branch name MUST be rejected.
	badCap := *capObj
	badCap.Branch = "../escape"
	if _, err := ExportGitWorkspace(t.TempDir(), &badCap, nil); err == nil {
		t.Fatal("expected ExportGitWorkspace to reject traversal branch name")
	}

	// 2. Clean checkout writes valid DIRC v2 .git/index with valid SHA-1 checksum
	//    covering notes/hello.md plus auto-generated MEMORY.md and .nektar/lineage.json.
	outDir := t.TempDir()
	res, err := ExportGitWorkspace(outDir, capObj, nil)
	if err != nil {
		t.Fatalf("ExportGitWorkspace: %v", err)
	}
	if res.FileCount != 3 {
		t.Fatalf("expected FileCount=3 (notes/hello.md + MEMORY.md + .nektar/lineage.json), got %d", res.FileCount)
	}
	idxBytes, err := os.ReadFile(filepath.Join(outDir, ".git", "index"))
	if err != nil {
		t.Fatalf("read .git/index: %v", err)
	}
	if len(idxBytes) < 32 || string(idxBytes[:4]) != "DIRC" {
		t.Fatalf("invalid .git/index header: len=%d magic=%q", len(idxBytes), string(idxBytes[:min(4, len(idxBytes))]))
	}
	if ver := binary.BigEndian.Uint32(idxBytes[4:8]); ver != 2 {
		t.Fatalf("expected index version 2, got %d", ver)
	}
	if cnt := binary.BigEndian.Uint32(idxBytes[8:12]); cnt != 3 {
		t.Fatalf("expected index entry count 3, got %d", cnt)
	}
	sum := sha1.Sum(idxBytes[:len(idxBytes)-20])
	if !bytes.Equal(sum[:], idxBytes[len(idxBytes)-20:]) {
		t.Fatal(".git/index trailing SHA-1 checksum mismatch")
	}
}
