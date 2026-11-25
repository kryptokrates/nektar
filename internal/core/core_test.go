package core

import (
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"
)

func TestFragmentRoundtripAndMerkle(t *testing.T) {
	p := Provenance{
		Source:    "agent-1",
		Time:      1730000000,
		Signature: []byte{0x30, 0x06, 0x02, 0x01, 0x01, 0x02, 0x01, 0x01},
		ModelID:   "claude-sonnet",
		Modality:  &Modality{Kind: "epistemic", Qualifier: "observed at block 900000"},
	}
	f := Fragment{
		Data:       []byte("BSV enables verifiable AI memory"),
		Provenance: p,
		Ciphertext: []byte{0xaa, 0xbb, 0xcc},
	}
	f.HDP = ComputeHDP(f.Data, f.Provenance)

	enc := SerializeFragment(f)
	dec, err := ParseFragment(enc)
	if err != nil {
		t.Fatalf("ParseFragment failed: %v", err)
	}
	if !EqualBytes(dec.Data, f.Data) || !EqualBytes(dec.HDP, f.HDP) || !EqualBytes(dec.Ciphertext, f.Ciphertext) {
		t.Fatalf("fragment roundtrip mismatch")
	}
	if dec.Provenance.Modality == nil || dec.Provenance.Modality.Qualifier != p.Modality.Qualifier {
		t.Fatalf("modality roundtrip mismatch: %+v", dec.Provenance.Modality)
	}

	// Verify Merkle proofs across all odd and even leaf counts 1..17.
	for count := 1; count <= 17; count++ {
		leaves := make([][]byte, count)
		for i := range count {
			lf := ComputeLeaf(Fragment{
				Data:       []byte{byte(i), byte(count)},
				Provenance: Provenance{Source: "s", Time: int64(i), ModelID: "m"},
			})
			leaves[i] = lf
		}
		root, err := MerkleRoot(leaves)
		if err != nil {
			t.Fatalf("count %d: MerkleRoot: %v", count, err)
		}
		for idx := range count {
			proof, err := ComputeMerkleProof(leaves, idx)
			if err != nil {
				t.Fatalf("count %d idx %d: proof: %v", count, idx, err)
			}
			if len(proof.Siblings) != HeightForLeafCount(count) {
				t.Fatalf("count %d idx %d: unexpected path len %d != %d", count, idx, len(proof.Siblings), HeightForLeafCount(count))
			}
			if !VerifyProof(leaves[idx], proof, root) {
				t.Fatalf("count %d idx %d: VerifyProof failed", count, idx)
			}
		}
	}
}

func TestCanonicalJSONAndChainRp(t *testing.T) {
	a, err := CanonicalJSON(map[string]any{"b": 2, "a": map[string]any{"z": 1, "y": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != `{"a":{"y":"hi","z":1},"b":2}` {
		t.Fatalf("unexpected canonical json: %s", string(a))
	}
	rp0 := ChainRp(Zero32, SHA256([]byte("r0")), 3)
	rp1 := ChainRp(rp0, SHA256([]byte("r1")), 5)
	if len(rp0) != 32 || len(rp1) != 32 || EqualBytes(rp0, rp1) {
		t.Fatalf("bad chained roots: %s / %s", hex.EncodeToString(rp0), hex.EncodeToString(rp1))
	}
}

func TestSpendGuard(t *testing.T) {
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	g := NewSpendGuard(SpendGuardOpts{
		MaxFeeSatsPerTx: 500,
		MaxSpendSatsDay: 1000,
		StatePath:       filepath.Join(dir, "spend.json"),
		Now:             func() time.Time { return now },
	})
	if err := g.CheckAndRecord("tx1", 600, 600); err == nil {
		t.Fatal("expected fee cap refusal")
	}
	if err := g.CheckAndRecord("tx1", 100, 600); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if err := g.CheckAndRecord("tx2", 100, 500); err == nil {
		t.Fatal("expected daily cap refusal")
	}
	// Reload persistence check.
	g2 := NewSpendGuard(SpendGuardOpts{
		MaxFeeSatsPerTx: 500,
		MaxSpendSatsDay: 1000,
		StatePath:       filepath.Join(dir, "spend.json"),
		Now:             func() time.Time { return now },
	})
	spent, _, _, count := g2.Stats()
	if spent != 600 || count != 1 {
		t.Fatalf("persisted stats mismatch: spent=%d count=%d", spent, count)
	}
	// Roll UTC day.
	now = now.Add(24 * time.Hour)
	if err := g2.CheckAndRecord("tx3", 100, 500); err != nil {
		t.Fatalf("expected fresh UTC day allowance: %v", err)
	}
}
