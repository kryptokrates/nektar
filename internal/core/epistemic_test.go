package core

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/lightwebinc/bcommon/cbor"
)

func TestCanonicalCBORAndTransitionRoundtrip(t *testing.T) {
	m := cbor.Map{
		{Key: uint64(0), Val: "hello"},
		{Key: uint64(1), Val: uint64(42)},
		{Key: uint64(2), Val: []byte{0xde, 0xad, 0xbe, 0xef}},
	}
	raw, err := EncodeCBOR(m)
	if err != nil {
		t.Fatalf("EncodeCBOR: %v", err)
	}
	v, err := DecodeCBOR(raw)
	if err != nil {
		t.Fatalf("DecodeCBOR: %v", err)
	}
	got, err := CBORMapToUintMap(v)
	if err != nil {
		t.Fatalf("CBORMapToUintMap: %v", err)
	}
	if s, _ := cborText(got, 0); s != "hello" {
		t.Fatalf("cborText = %q, want hello", s)
	}
	// Non-canonical float (CBOR major 7 half-float 0xf9 0x00 0x00) MUST be rejected.
	if _, err := DecodeCBOR([]byte{0xf9, 0x00, 0x00}); err == nil {
		t.Fatal("expected non-canonical CBOR float rejection")
	}

	prevQ := Zero32Bytes()
	inDig := SHA256([]byte("input-sigma"))
	commitH := SHA256([]byte("commit-1"))
	treeH := SHA256([]byte("tree-1"))
	polH := SHA256([]byte("policy-1"))
	ontH := SHA256([]byte("ontology-1"))
	nextQ, err := NextStepStateQ(prevQ, inDig, commitH, treeH, polH, ontH, 1730000100)
	if err != nil {
		t.Fatalf("NextStepStateQ: %v", err)
	}
	tr := TransitionRecord{
		Version:      1,
		Shard:        "alpha",
		Seq:          1,
		PrevStateQ:   prevQ,
		InputDigest:  inDig,
		NextStateQ:   nextQ,
		CommitHash:   commitH,
		TreeHash:     treeH,
		PolicyHash:   polH,
		OntologyHash: ontH,
		Timestamp:    1730000100,
	}
	h, err := ComputeTransitionHash(tr)
	if err != nil {
		t.Fatalf("ComputeTransitionHash: %v", err)
	}
	tr.Hash = h
	enc, err := EncodeTransitionCBOR(tr)
	if err != nil {
		t.Fatalf("EncodeTransitionCBOR: %v", err)
	}
	dec, err := DecodeTransitionCBOR(enc)
	if err != nil {
		t.Fatalf("DecodeTransitionCBOR: %v", err)
	}
	if dec.Shard != tr.Shard || dec.Seq != tr.Seq || !EqualBytes(dec.NextStateQ, tr.NextStateQ) || !EqualBytes(dec.Hash, tr.Hash) {
		t.Fatalf("transition roundtrip mismatch: %+v", dec)
	}
	// Tampering any byte of the encoded CBOR must fail decoding or hash check.
	tampered := append([]byte(nil), enc...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := DecodeTransitionCBOR(tampered); err == nil {
		t.Fatal("expected tampered transition CBOR to fail verification")
	}
}

func TestProvenanceRefinesAndLeafCommitsDAG(t *testing.T) {
	targetHDP := hex.EncodeToString(SHA256([]byte("hdp-0")))
	targetLeaf := hex.EncodeToString(SHA256([]byte("leaf-0")))
	edges := []RefinementEdge{
		{
			TargetShard: "ops",
			TargetSeq:   1,
			TargetHDP:   targetHDP,
			TargetLeaf:  targetLeaf,
			Kind:        RefinementSupersedes,
			Reason:      "updated threshold",
		},
	}
	if err := edges[0].Validate(); err != nil {
		t.Fatalf("edge validate: %v", err)
	}
	// Test both nil-modality + refines and present-modality + refines roundtrip.
	for _, mod := range []*Modality{nil, {Kind: "deontic", Qualifier: "obligation:audit"}} {
		p := Provenance{
			Source:    "agent-1",
			Time:      1730000200,
			Signature: []byte{0x01, 0x02, 0x03},
			ModelID:   "m1",
			Modality:  mod,
			Refines:   edges,
		}
		raw := SerializeProvenance(p)
		dec, err := ParseProvenance(raw)
		if err != nil {
			t.Fatalf("ParseProvenance (mod=%v): %v", mod, err)
		}
		if len(dec.Refines) != 1 || dec.Refines[0].TargetHDP != targetHDP || dec.Refines[0].Kind != RefinementSupersedes {
			t.Fatalf("refines roundtrip mismatch: %+v", dec.Refines)
		}
		if (mod == nil) != (dec.Modality == nil) {
			t.Fatalf("modality nilness mismatch: got %+v want %+v", dec.Modality, mod)
		}
		// Mutating refinement edge MUST change ProvenanceDigest & ComputeHDPV2!
		p2 := p
		p2.Refines = []RefinementEdge{{
			TargetShard: "ops",
			TargetSeq:   1,
			TargetHDP:   targetHDP,
			TargetLeaf:  targetLeaf,
			Kind:        RefinementContradicts,
			Reason:      "updated threshold",
		}}
		if bytes.Equal(ProvenanceDigest([]byte("fact"), p), ProvenanceDigest([]byte("fact"), p2)) {
			t.Fatal("ProvenanceDigest failed to commit to RefinementEdge.Kind")
		}
		if bytes.Equal(ComputeHDPV2([]byte("fact"), p), ComputeHDPV2([]byte("fact"), p2)) {
			t.Fatal("ComputeHDPV2 failed to commit to RefinementEdge.Kind")
		}
	}

	// ComputeLeaf MUST commit to both CommitHash and TransitionHash.
	base := Fragment{
		Data:           []byte("x"),
		Provenance:     Provenance{Source: "s", Time: 1, ModelID: "m"},
		MPERoot:        SHA256([]byte("mpe")),
		CommitHash:     SHA256([]byte("commit-A")),
		TransitionHash: SHA256([]byte("trans-A")),
	}
	lA := ComputeLeaf(base)
	altCommit := base
	altCommit.CommitHash = SHA256([]byte("commit-B"))
	if EqualBytes(lA, ComputeLeaf(altCommit)) {
		t.Fatal("ComputeLeaf failed to commit to CommitHash")
	}
	altTrans := base
	altTrans.TransitionHash = SHA256([]byte("trans-B"))
	if EqualBytes(lA, ComputeLeaf(altTrans)) {
		t.Fatal("ComputeLeaf failed to commit to TransitionHash")
	}
}

func TestMMRInclusionAndConsistency(t *testing.T) {
	mmr, err := NewMMR(nil)
	if err != nil {
		t.Fatal(err)
	}
	roots := make([][]byte, 0, 129)
	roots = append(roots, Zero32Bytes()) // size 0 unused
	for i := 0; i < 128; i++ {
		leaf := DSHA256([]byte{byte(i), byte(i >> 8), 0x42})
		idx, err := mmr.Append(leaf)
		if err != nil || idx != uint64(i) {
			t.Fatalf("Append(%d): idx=%d err=%v", i, idx, err)
		}
		r := mmr.Root()
		roots = append(roots, r)

		// Check inclusion proof for first, middle, and latest leaf at every size up to 33, and powers/primes beyond.
		if i < 33 || i == 63 || i == 64 || i == 127 {
			checkIdxs := []uint64{0, uint64(i / 2), uint64(i)}
			for _, ci := range checkIdxs {
				p, err := mmr.ProveInclusion(ci)
				if err != nil {
					t.Fatalf("size=%d ProveInclusion(%d): %v", i+1, ci, err)
				}
				ciLeaf := DSHA256([]byte{byte(ci), byte(ci >> 8), 0x42})
				if err := VerifyMMRInclusion(ciLeaf, p, r); err != nil {
					t.Fatalf("size=%d VerifyMMRInclusion(%d): %v", i+1, ci, err)
				}
				// Forged leaf MUST fail.
				badLeaf := DSHA256([]byte("forged"))
				if err := VerifyMMRInclusion(badLeaf, p, r); err == nil {
					t.Fatalf("size=%d VerifyMMRInclusion accepted forged leaf", i+1)
				}
			}
		}
	}

	// Exhaustive consistency proofs across tricky binary-carry boundaries (1..65 plus 128).
	oldSizes := []uint64{1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 17, 31, 32, 33, 63, 64, 65}
	newSizes := []uint64{1, 2, 3, 5, 7, 8, 12, 16, 25, 32, 47, 64, 99, 128}
	for _, oldN := range oldSizes {
		for _, newN := range newSizes {
			if oldN > newN {
				continue
			}
			cp, err := mmr.ProveConsistency(oldN, newN)
			if err != nil {
				t.Fatalf("ProveConsistency(%d->%d): %v", oldN, newN, err)
			}
			if err := VerifyMMRConsistency(roots[oldN], roots[newN], cp); err != nil {
				t.Fatalf("VerifyMMRConsistency(%d->%d): %v", oldN, newN, err)
			}
			// Forged oldRoot or newRoot MUST fail.
			if oldN < newN {
				if err := VerifyMMRConsistency(roots[oldN], roots[oldN], cp); err == nil {
					t.Fatalf("VerifyMMRConsistency(%d->%d) accepted wrong newRoot", oldN, newN)
				}
				if err := VerifyMMRConsistency(roots[newN], roots[newN], cp); err == nil {
					t.Fatalf("VerifyMMRConsistency(%d->%d) accepted wrong oldRoot", oldN, newN)
				}
			}
		}
	}

	// Scale test: N=1024 leaves -> O(log N) proof size <= 20 hashes.
	for i := 128; i < 1024; i++ {
		_, err := mmr.Append(DSHA256(U64LE(uint64(i))))
		if err != nil {
			t.Fatal(err)
		}
	}
	r1024 := mmr.Root()
	cpBig, err := mmr.ProveConsistency(37, 1024)
	if err != nil {
		t.Fatalf("ProveConsistency(37->1024): %v", err)
	}
	if len(cpBig.OldPeaks)+len(cpBig.RightChunks) > 22 {
		t.Fatalf("consistency proof not O(log N): oldPeaks=%d rightChunks=%d", len(cpBig.OldPeaks), len(cpBig.RightChunks))
	}
	if err := VerifyMMRConsistency(roots[37], r1024, cpBig); err != nil {
		t.Fatalf("VerifyMMRConsistency(37->1024): %v", err)
	}
}
