package memrepo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
)

func buildSignedTestCapsule(t *testing.T, objects bstack.ObjectStore, includeVolumetric bool) (*MemoryCapsule, *ec.PrivateKey) {
	t.Helper()
	priv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())

	b1, _, err := BuildBlobWithMeta(objects, "facts/0001.md", "text/markdown", []byte("Immutable memory fact #1"), false, 10, "ops")
	if err != nil {
		t.Fatal(err)
	}
	entries := []TreeEntry{{
		Mode: "100644", Path: b1.Path, BlobHash: b1.BlobHash, MediaType: b1.MediaType,
		Size: b1.Size, ContentSHA256: b1.ContentSHA256, Level: b1.Level, Class: b1.Class,
	}}
	blobs := map[string]BlobObject{b1.BlobHash: *b1}

	if includeVolumetric {
		big := bytes.Repeat([]byte("volumetric-data-block-"), 4000) // ~88 KiB > 64 KiB
		b2, _, err := BuildBlobWithMeta(objects, "docs/large.bin", "application/octet-stream", big, true, 0, "docs")
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, TreeEntry{
			Mode: "100644", Path: b2.Path, BlobHash: b2.BlobHash, MediaType: b2.MediaType,
			Size: b2.Size, ContentSHA256: b2.ContentSHA256, ChirpURL: b2.ChirpURL, Level: b2.Level, Class: b2.Class,
		})
		blobs[b2.BlobHash] = *b2
	}

	tree, err := BuildTree(entries)
	if err != nil {
		t.Fatal(err)
	}
	c, err := FinalizeCommit(CommitObject{
		Version:   1,
		Repo:      "ops",
		Branch:    "main",
		Seq:       1,
		TreeHash:  tree.TreeHash,
		AuthorPub: pubHex,
		ModelID:   "test/v1",
		Message:   "initial commit",
		Timestamp: time.Now().Unix(),
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

	// Build a valid Merkle Automaton TransitionRecord (q0 -> q1).
	q0 := core.Zero32Bytes()
	inDig := core.DSHA256([]byte("input-1"))
	chBytes, _ := hex.DecodeString(c.CommitHash)
	thBytes, _ := hex.DecodeString(c.TreeHash)
	q1, err := core.NextStepStateQ(q0, inDig, chBytes, thBytes, nil, nil, c.Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	tr := core.TransitionRecord{
		Version:     1,
		Shard:       "ops",
		Seq:         1,
		PrevStateQ:  q0,
		InputDigest: inDig,
		NextStateQ:  q1,
		CommitHash:  chBytes,
		TreeHash:    thBytes,
		Timestamp:   c.Timestamp,
	}
	tr.Hash, err = core.ComputeTransitionHash(tr)
	if err != nil {
		t.Fatal(err)
	}

	return &MemoryCapsule{
		Format:      CapsuleFormatVersion,
		Repo:        "ops",
		Branch:      "main",
		OwnerPub:    pubHex,
		HeadCommit:  c.CommitHash,
		BaseAnchorN: -1,
		ExportedAt:  time.Now().Unix(),
		Commits:     []CommitObject{c},
		Trees:       map[string]TreeObject{tree.TreeHash: *tree},
		Blobs:       blobs,
		Transitions: []core.TransitionRecord{tr},
	}, priv
}

func TestCapsuleStreamSignedTrailerAndTransitionReplay(t *testing.T) {
	ctx := context.Background()
	srcObj := bstack.NewMemoryObjectStore()
	cap, priv := buildSignedTestCapsule(t, srcObj, true)
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())

	var buf bytes.Buffer
	err := WriteCapsuleStream(&buf, cap, CapsuleEncodeOpts{
		Objects:       srcObj,
		SelfContained: true,
		SignerPub:     pubHex,
		SignDigest: func(digest []byte) ([]byte, error) {
			sig, sErr := priv.Sign(digest)
			if sErr != nil {
				return nil, sErr
			}
			return sig.Serialize(), nil
		},
	})
	if err != nil {
		t.Fatalf("WriteCapsuleStream: %v", err)
	}
	raw := buf.Bytes()
	if !IsCapsuleBytes(raw) {
		t.Fatal("expected NKTCAP1 magic prefix")
	}

	// Verify on a FRESH empty ObjectStore (self-contained volumetric chunks must pass offline!).
	freshObj := bstack.NewMemoryObjectStore()
	dec, rep, err := VerifyCapsuleStream(ctx, bytes.NewReader(raw), CapsuleVerifyOpts{
		Objects:              freshObj,
		MemoryMode:           true,
		RequireSignedTrailer: true,
	})
	if err != nil {
		t.Fatalf("VerifyCapsuleStream: %v", err)
	}
	if !rep.OK {
		t.Fatalf("expected OK report, got reason=%q", rep.Reason)
	}
	if !rep.TrailerVerified || rep.TrailerSignerPub != pubHex {
		t.Fatalf("expected verified signed trailer, got %+v", rep)
	}
	if rep.BlobsVerified != 2 || rep.TransitionsVerified != 1 || rep.CommitsVerified != 1 {
		t.Fatalf("unexpected counts: %+v", rep)
	}
	if len(dec.Transitions) != 1 {
		t.Fatalf("expected 1 decoded transition, got %d", len(dec.Transitions))
	}

	// Truncating the capsule MUST fail trailer completeness check.
	if _, _, err := VerifyCapsuleStream(ctx, bytes.NewReader(raw[:len(raw)-12]), CapsuleVerifyOpts{MemoryMode: true}); err == nil {
		t.Fatal("expected truncated capsule to fail")
	}
}

func TestCapsuleAEADOuterEnvelope(t *testing.T) {
	ctx := context.Background()
	srcObj := bstack.NewMemoryObjectStore()
	cap, priv := buildSignedTestCapsule(t, srcObj, false)
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())

	// Simulate BRC-42 DEK wrap/unwrap with a 32-byte key XOR mask for unit test.
	kek := sha256.Sum256([]byte("test-brc42-kek"))
	xorWrap := func(in []byte) []byte {
		out := make([]byte, len(in))
		for i := range in {
			out[i] = in[i] ^ kek[i%len(kek)]
		}
		return out
	}

	raw, err := EncodeCapsuleWithOpts(cap, CapsuleEncodeOpts{
		SelfContained: true,
		SignerPub:     pubHex,
		WrapDEK: func(dek []byte) ([]byte, string, error) {
			return xorWrap(dek), pubHex, nil
		},
	})
	if err != nil {
		t.Fatalf("EncodeCapsuleWithOpts AEAD: %v", err)
	}
	if bytes.Contains(raw, []byte("Immutable memory fact")) || bytes.Contains(raw, []byte("facts/0001.md")) {
		t.Fatal("encrypted capsule leaked plaintext payload or path!")
	}

	// Decoding without UnwrapDEK MUST fail closed.
	if _, err := DecodeCapsule(raw); err == nil {
		t.Fatal("expected encrypted capsule without UnwrapDEK to fail")
	}

	_, rep, err := VerifyCapsuleStream(ctx, bytes.NewReader(raw), CapsuleVerifyOpts{
		MemoryMode: true,
		UnwrapDEK: func(hdr CapsuleEnvelopeHeader) ([]byte, error) {
			if hdr.Cipher != "AES-256-GCM" || hdr.Compression != "zstd" {
				return nil, errors.New("unexpected header metadata")
			}
			return xorWrap(hdr.WrappedDEK), nil
		},
	})
	if err != nil || !rep.OK || !rep.EncryptedEnvelope {
		t.Fatalf("expected encrypted capsule verify OK, got rep=%+v err=%v", rep, err)
	}
}

func TestCapsuleRedactedStubAndMissingChirpFailClosed(t *testing.T) {
	ctx := context.Background()
	srcObj := bstack.NewMemoryObjectStore()
	cap, _ := buildSignedTestCapsule(t, srcObj, true)

	// 1. Thin external-CHIRP export (SelfContained=false): verifying with empty ObjectStore MUST fail closed by default.
	rawThin, err := EncodeCapsuleWithOpts(cap, CapsuleEncodeOpts{SelfContained: false})
	if err != nil {
		t.Fatal(err)
	}
	emptyObj := bstack.NewMemoryObjectStore()
	_, repFail, err := VerifyCapsuleStream(ctx, bytes.NewReader(rawThin), CapsuleVerifyOpts{
		Objects:    emptyObj,
		MemoryMode: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if repFail.OK {
		t.Fatalf("expected fail-closed on missing external CHIRP blob, got OK: %+v", repFail)
	}

	// With explicit AllowMissingExternal=true, report tracks BlobsMissingExternal separately (NEVER BlobsVerified!).
	_, repAllow, err := VerifyCapsuleStream(ctx, bytes.NewReader(rawThin), CapsuleVerifyOpts{
		Objects:              emptyObj,
		MemoryMode:           true,
		AllowMissingExternal: true,
	})
	if err != nil || !repAllow.OK || repAllow.BlobsMissingExternal != 1 || repAllow.BlobsVerified != 1 {
		t.Fatalf("expected AllowMissingExternal to report 1 missing & 1 verified, got rep=%+v err=%v", repAllow, err)
	}

	// 2. Redacted/erased BlobStub preserves BlobHash, TreeHash, and CommitHash while stripping plaintext!
	for k, b := range cap.Blobs {
		if b.Path == "facts/0001.md" {
			cap.Blobs[k] = b.ToRedactedStub()
		}
	}
	rawRedacted, err := EncodeCapsuleWithOpts(cap, CapsuleEncodeOpts{Objects: srcObj, SelfContained: true})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawRedacted, []byte("Immutable memory fact #1")) {
		t.Fatal("redacted capsule leaked erased plaintext")
	}
	_, repRed, err := VerifyCapsuleStream(ctx, bytes.NewReader(rawRedacted), CapsuleVerifyOpts{
		Objects:    emptyObj,
		MemoryMode: true,
	})
	if err != nil || !repRed.OK || repRed.BlobsRedacted != 1 || repRed.BlobsVerified != 1 {
		t.Fatalf("expected redacted stub to verify cleanly (1 redacted, 1 verified), got rep=%+v err=%v", repRed, err)
	}
}

func TestBlobLevelAndClassCommittedInHash(t *testing.T) {
	b1, _, err := BuildBlobWithMeta(nil, "facts/0001.md", "text/markdown", []byte("classified"), false, 50, "secret")
	if err != nil {
		t.Fatal(err)
	}
	tampered := *b1
	tampered.Level = 0
	hTampered, err := ComputeBlobHash(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if hTampered == b1.BlobHash {
		t.Fatal("downgrading BlobObject.Level did not change BlobHash!")
	}
}

func FuzzVerifyCapsuleStream(f *testing.F) {
	obj := bstack.NewMemoryObjectStore()
	cap, _ := buildSignedTestCapsule(&testing.T{}, obj, false)
	if raw, err := EncodeCapsule(cap); err == nil {
		f.Add(raw)
	}
	f.Add([]byte(CapsuleMagicHeader))
	f.Add([]byte(CapsuleMagicHeader + "\x00\x28\xb5\x2f\xfd"))

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = VerifyCapsuleStream(context.Background(), bytes.NewReader(data), CapsuleVerifyOpts{
			MemoryMode:           true,
			AllowMissingExternal: true,
		})
	})
}
