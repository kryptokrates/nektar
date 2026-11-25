package memory

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/store"
)

// ShardChainReport summarizes local hash-chain, leaf-hash, and anchor-chain integrity for one shard.
type ShardChainReport struct {
	Shard         string `json:"shard"`
	OK            bool   `json:"ok"`
	FragmentCount int    `json:"fragmentCount"`
	VerifiedCount int    `json:"verifiedCount"`
	BrokenSeq     int64  `json:"brokenSeq,omitempty"`
	Reason        string `json:"reason,omitempty"`
	HeadChainHash string `json:"headChainHash,omitempty"`
	AnchorCount   int    `json:"anchorCount"`
	AnchorsOK     bool   `json:"anchorsOk"`
}

// VerifyShardChain walks every fragment in shard in ascending seq order and verifies:
//  1. Unbroken seq continuity (catches silent SQLite deletion/insertion/reordering before anchoring),
//  2. PrevLeaf and ChainHash linkage via store.ComputeFragmentChainHash (for chain-tagged records),
//  3. Leaf recomputation (core.ComputeLeaf) and HDP verification on non-forgotten fragments,
//  4. Batch anchor R' hash-chain continuity across persisted anchors.
func (s *Service) VerifyShardChain(shard string) (*ShardChainReport, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("memory: store not initialized")
	}
	frags, err := s.db.ListFragments(shard)
	if err != nil {
		return nil, err
	}
	rep := &ShardChainReport{
		Shard:         shard,
		OK:            true,
		FragmentCount: len(frags),
		AnchorsOK:     true,
	}

	var prevLeaf, prevChain string
	var expectedSeq int64 = 1
	for _, f := range frags {
		if f.Seq != expectedSeq {
			rep.OK = false
			rep.BrokenSeq = expectedSeq
			rep.Reason = fmt.Sprintf("sequence gap or reorder: expected seq %d, found %d", expectedSeq, f.Seq)
			return rep, nil
		}
		if f.ChainHash != "" {
			if f.Seq > 1 && f.PrevLeaf != prevLeaf {
				rep.OK = false
				rep.BrokenSeq = f.Seq
				rep.Reason = fmt.Sprintf("prev_leaf broken at seq %d: got %s want %s", f.Seq, f.PrevLeaf, prevLeaf)
				return rep, nil
			}
			wantChain := store.ComputeFragmentChainHash(prevChain, f.Leaf)
			if f.ChainHash != wantChain {
				rep.OK = false
				rep.BrokenSeq = f.Seq
				rep.Reason = fmt.Sprintf("chain_hash mismatch at seq %d: got %s want %s", f.Seq, f.ChainHash, wantChain)
				return rep, nil
			}
			prevChain = f.ChainHash
		} else {
			// Legacy pre-v5 row: fold deterministically so subsequent rows can verify cleanly.
			prevChain = store.ComputeFragmentChainHash(prevChain, f.Leaf)
		}

		// Verify local plaintext/ciphertext leaf commitment on non-forgotten rows.
		if !f.Forgotten {
			hdpBytes, hErr := hex.DecodeString(f.HDP)
			if hErr != nil || len(hdpBytes) != 32 {
				rep.OK = false
				rep.BrokenSeq = f.Seq
				rep.Reason = fmt.Sprintf("invalid 32-byte HDP hex at seq %d", f.Seq)
				return rep, nil
			}
			if f.BorgGroup == "" && len(f.Data) > 0 && len(f.Provenance) > 0 {
				if !core.VerifyHDP(f.Data, f.Provenance, hdpBytes) {
					rep.OK = false
					rep.BrokenSeq = f.Seq
					rep.Reason = fmt.Sprintf("plaintext/provenance HDP tamper detected at seq %d", f.Seq)
					return rep, nil
				}
			}
			recomputedLeaf := core.ToHex(core.ComputeLeaf(core.Fragment{
				Data:       f.Data,
				HDP:        hdpBytes,
				Ciphertext: f.Ciphertext,
			}))
			if recomputedLeaf != f.Leaf {
				rep.OK = false
				rep.BrokenSeq = f.Seq
				rep.Reason = fmt.Sprintf("leaf hash tamper detected at seq %d", f.Seq)
				return rep, nil
			}
		}

		prevLeaf = f.Leaf
		rep.VerifiedCount++
		expectedSeq++
	}
	rep.HeadChainHash = prevChain

	anchors, err := s.db.ListAnchors(shard)
	if err != nil {
		return nil, err
	}
	rep.AnchorCount = len(anchors)
	prevRp := core.Zero32Bytes()
	for i, a := range anchors {
		if a.N != int64(i) {
			rep.OK = false
			rep.AnchorsOK = false
			rep.Reason = fmt.Sprintf("anchor batch gap at n=%d (found n=%d)", i, a.N)
			return rep, nil
		}
		rBytes, err1 := hex.DecodeString(a.RootHex)
		rpBytes, err2 := hex.DecodeString(a.ChainedHex)
		if err1 != nil || err2 != nil || len(rBytes) != 32 || len(rpBytes) != 32 {
			rep.OK = false
			rep.AnchorsOK = false
			rep.Reason = fmt.Sprintf("invalid anchor R/Rp hex at n=%d", a.N)
			return rep, nil
		}
		wantRp := core.ChainRp(prevRp, rBytes, uint64(a.Size))
		if !core.EqualBytes(rpBytes, wantRp) {
			rep.OK = false
			rep.AnchorsOK = false
			rep.Reason = fmt.Sprintf("anchor Rp chain mismatch at n=%d", a.N)
			return rep, nil
		}
		prevRp = rpBytes
	}

	return rep, nil
}

// VerifyAllShardsIntegrity verifies every non-system shard in the local store.
func (s *Service) VerifyAllShardsIntegrity() ([]ShardChainReport, error) {
	shards, err := s.ListUserShards()
	if err != nil {
		return nil, err
	}
	out := make([]ShardChainReport, 0, len(shards))
	for _, sh := range shards {
		r, err := s.VerifyShardChain(sh)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, nil
}

// FragmentFieldProof is a selective-disclosure Merkle Proof Entity (MPE) proving chosen metadata
// fields of a fragment (e.g. source, time, modelId, class, level, hdp, leaf) without revealing plaintext.
type FragmentFieldProof struct {
	Shard      string                         `json:"shard"`
	Seq        int64                          `json:"seq"`
	Leaf       string                         `json:"leaf"`
	FieldsRoot string                         `json:"fieldsRoot"`
	Proof      *core.SelectiveDisclosureProof `json:"proof"`
}

// ProveFragmentFields builds an MPE over fragment (shard, seq) metadata and discloses ONLY the requested fields.
func (s *Service) ProveFragmentFields(_ context.Context, shard string, seq int64, fields []string) (*FragmentFieldProof, error) {
	f, err := s.db.GetFragment(shard, seq)
	if err != nil {
		return nil, err
	}
	if f.Forgotten {
		return nil, errors.New("memory: cannot prove fields of a forgotten fragment")
	}
	prov, err := core.ParseProvenance(f.Provenance)
	if err != nil {
		return nil, fmt.Errorf("memory: parse provenance: %w", err)
	}
	dataHash := ""
	if len(f.Data) > 0 {
		dataHash = hex.EncodeToString(core.SHA256(f.Data))
	}
	mpe, err := core.BuildMPE(map[string][]byte{
		"shard":      []byte(f.Shard),
		"seq":        []byte(strconv.FormatInt(f.Seq, 10)),
		"hdp":        []byte(f.HDP),
		"leaf":       []byte(f.Leaf),
		"prevLeaf":   []byte(f.PrevLeaf),
		"chainHash":  []byte(f.ChainHash),
		"source":     []byte(prov.Source),
		"time":       []byte(strconv.FormatInt(prov.Time, 10)),
		"modelId":    []byte(prov.ModelID),
		"level":      []byte(strconv.Itoa(f.Level)),
		"class":      []byte(f.Class),
		"signature":  []byte(hex.EncodeToString(prov.Signature)),
		"dataSha256": []byte(dataHash),
	})
	if err != nil {
		return nil, err
	}
	sdp, err := mpe.ProveFields(fields)
	if err != nil {
		return nil, err
	}
	return &FragmentFieldProof{
		Shard:      f.Shard,
		Seq:        f.Seq,
		Leaf:       f.Leaf,
		FieldsRoot: mpe.RootHex,
		Proof:      sdp,
	}, nil
}

// VerifyFragmentFieldProof verifies a selective-disclosure FragmentFieldProof offline.
func VerifyFragmentFieldProof(p *FragmentFieldProof) error {
	if p == nil {
		return errors.New("memory: nil fragment field proof")
	}
	return core.VerifySelectiveDisclosure(p.FieldsRoot, p.Proof)
}
