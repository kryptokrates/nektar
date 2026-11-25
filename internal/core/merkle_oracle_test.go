package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"testing"
)

// Independent naive double-SHA-256 Merkle oracle (mfspv/04_TEST_PLAN.md differential methodology).
func oracleDSHA256(b []byte) [32]byte {
	h1 := sha256.Sum256(b)
	return sha256.Sum256(h1[:])
}

func oracleRoot(leaves [][]byte) [32]byte {
	cur := make([][32]byte, len(leaves))
	for i, lf := range leaves {
		copy(cur[i][:], lf)
	}
	for len(cur) > 1 {
		next := make([][32]byte, 0, (len(cur)+1)/2)
		for i := 0; i < len(cur); i += 2 {
			var pair [64]byte
			copy(pair[:32], cur[i][:])
			if i+1 < len(cur) {
				copy(pair[32:], cur[i+1][:])
			} else {
				copy(pair[32:], cur[i][:])
			}
			next = append(next, oracleDSHA256(pair[:]))
		}
		cur = next
	}
	return cur[0]
}

func TestDifferentialMerkleOracleAndAdversarialForgeries(t *testing.T) {
	// 1. Differential parity across sizes 1..129 plus boundary powers (255, 256, 257, 511, 512, 513).
	sizes := make([]int, 0, 140)
	for n := 1; n <= 129; n++ {
		sizes = append(sizes, n)
	}
	sizes = append(sizes, 255, 256, 257, 511, 512, 513)

	for _, n := range sizes {
		leaves := make([][]byte, n)
		for i := 0; i < n; i++ {
			var buf [8]byte
			binary.LittleEndian.PutUint64(buf[:], uint64(n*10000+i))
			h := oracleDSHA256(buf[:])
			leaves[i] = append([]byte(nil), h[:]...)
		}
		wantRoot := oracleRoot(leaves)
		gotRoot, err := MerkleRoot(leaves)
		if err != nil {
			t.Fatalf("n=%d MerkleRoot: %v", n, err)
		}
		if !bytes.Equal(gotRoot, wantRoot[:]) {
			t.Fatalf("n=%d differential root mismatch: got %x want %x", n, gotRoot, wantRoot)
		}
		// Spot-check first, middle, and last leaf inclusion proofs + bit-flip rejection.
		indices := []int{0, n / 2, n - 1}
		wantHeight := HeightForLeafCount(n)
		for _, idx := range indices {
			proof, err := ComputeMerkleProof(leaves, idx)
			if err != nil {
				t.Fatalf("n=%d idx=%d ComputeMerkleProof: %v", n, idx, err)
			}
			if len(proof.Siblings) != wantHeight {
				t.Fatalf("n=%d idx=%d height mismatch: got %d want %d", n, idx, len(proof.Siblings), wantHeight)
			}
			if !VerifyProof(leaves[idx], proof, gotRoot) {
				t.Fatalf("n=%d idx=%d valid proof failed verification", n, idx)
			}
			// Flip 1 bit in leaf -> MUST fail.
			badLeaf := append([]byte(nil), leaves[idx]...)
			badLeaf[0] ^= 0x01
			if VerifyProof(badLeaf, proof, gotRoot) {
				t.Fatalf("n=%d idx=%d 1-bit leaf flip accepted!", n, idx)
			}
			// Flip 1 bit in each sibling -> MUST fail.
			for s := range proof.Siblings {
				badSibs := make([][]byte, len(proof.Siblings))
				for j := range proof.Siblings {
					badSibs[j] = append([]byte(nil), proof.Siblings[j]...)
				}
				badSibs[s][31] ^= 0x80
				if VerifyProof(leaves[idx], &MerkleProof{Index: idx, Siblings: badSibs}, gotRoot) {
					t.Fatalf("n=%d idx=%d sibling[%d] flip accepted!", n, idx, s)
				}
			}
		}
	}

	// 2. Depth > MaxMerkleProofDepth MUST be rejected (mfspv sender-held proof cap).
	deepSibs := make([][]byte, MaxMerkleProofDepth+1)
	for i := range deepSibs {
		deepSibs[i] = make([]byte, 32)
	}
	if _, err := ReconstructRootChecked(make([]byte, 32), &MerkleProof{Index: 0, Siblings: deepSibs}); !errors.Is(err, ErrProofDepthExceed) {
		t.Fatalf("expected ErrProofDepthExceed for depth %d, got %v", len(deepSibs), err)
	}
}

func TestMPESelectiveDisclosureAndTamperResistance(t *testing.T) {
	fields := map[string][]byte{
		"dealId":          []byte("deal-42"),
		"priceSats":       []byte("250"),
		"paymentTxid":     []byte("aabbccdd"),
		"buyerPolicyHash": []byte("polhash-123"),
		"secretQuery":     []byte("CONFIDENTIAL_QUERY_NEVER_DISCLOSED"),
		"fragmentText":    []byte("CONFIDENTIAL_PLAINTEXT_NEVER_DISCLOSED"),
	}
	mpe, err := BuildMPE(fields)
	if err != nil {
		t.Fatalf("BuildMPE: %v", err)
	}

	// Disclose only 2 audit fields — zero exposure of confidential fields!
	proof, err := mpe.ProveFields([]string{"priceSats", "paymentTxid"})
	if err != nil {
		t.Fatalf("ProveFields: %v", err)
	}
	if len(proof.Fields) != 2 {
		t.Fatalf("expected 2 disclosed fields, got %d", len(proof.Fields))
	}
	if err := VerifySelectiveDisclosure(mpe.RootHex, proof); err != nil {
		t.Fatalf("VerifySelectiveDisclosure valid: %v", err)
	}

	// 1. Tamper disclosed value -> MUST fail.
	tampered := *proof
	tampered.Fields = append([]DisclosedField(nil), proof.Fields...)
	tampered.Fields[0].Value = []byte("999999")
	if err := VerifySelectiveDisclosure(mpe.RootHex, &tampered); err == nil {
		t.Fatal("expected tampered field value to fail verification")
	}

	// 2. Rename field key while keeping value -> MUST fail (key is domain-bound into leaf!).
	renamed := *proof
	renamed.Fields = append([]DisclosedField(nil), proof.Fields...)
	renamed.Fields[0].Key = "otherKey"
	if err := VerifySelectiveDisclosure(mpe.RootHex, &renamed); err == nil {
		t.Fatal("expected renamed key to fail verification")
	}

	// 3. CVE-2012-2459 odd-leaf duplication forgery attempt (3 fields -> duplicate last leaf as idx 3 with leafCount=4):
	oddMPE, err := BuildMPE(map[string][]byte{"a": []byte("1"), "b": []byte("2"), "c": []byte("3")})
	if err != nil {
		t.Fatal(err)
	}
	oddProof, err := oddMPE.ProveFields([]string{"c"})
	if err != nil {
		t.Fatal(err)
	}
	// Attempting to pass index=3 (duplicate of leaf 2) against leafCount=3 MUST fail index bounds!
	dupIdx := *oddProof
	dupIdx.Fields = append([]DisclosedField(nil), oddProof.Fields...)
	dupIdx.Fields[0].Index = 3
	if err := VerifySelectiveDisclosure(oddMPE.RootHex, &dupIdx); err == nil {
		t.Fatal("expected out-of-range duplicate tail index to fail")
	}
	// Attempting to inflate LeafCount to 5 (height 3 instead of 2) MUST fail height mismatch!
	badCount := *oddProof
	badCount.LeafCount = 5
	if err := VerifySelectiveDisclosure(oddMPE.RootHex, &badCount); err == nil {
		t.Fatal("expected mismatched leafCount/height to fail")
	}
}

func FuzzMerkleProofForgery(f *testing.F) {
	seedLeaf := HashLeaf([]byte("seed-leaf"))
	seedSib := HashLeaf([]byte("seed-sib"))
	seedRoot := HashNode(seedLeaf, seedSib)
	f.Add(seedLeaf, seedSib, seedRoot, 0)
	f.Fuzz(func(t *testing.T, leaf, sib, root []byte, idx int) {
		if len(leaf) != 32 || len(sib) != 32 || len(root) != 32 || idx < 0 {
			if VerifyProof(leaf, &MerkleProof{Index: idx, Siblings: [][]byte{sib}}, root) {
				t.Fatalf("malformed proof accepted: leaf=%d sib=%d root=%d idx=%d", len(leaf), len(sib), len(root), idx)
			}
			return
		}
		ok := VerifyProof(leaf, &MerkleProof{Index: idx, Siblings: [][]byte{sib}}, root)
		want := oracleDSHA256(Concat(leaf, sib))
		if idx&1 == 1 {
			want = oracleDSHA256(Concat(sib, leaf))
		}
		if ok != bytes.Equal(want[:], root) {
			t.Fatalf("VerifyProof mismatch vs oracle: ok=%v got=%s want=%x", ok, hex.EncodeToString(root), want)
		}
	})
}
