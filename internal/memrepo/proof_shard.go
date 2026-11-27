package memrepo

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/kryptokrates/nektar/internal/core"
)

const (
	// ProofShardLevel splits batch Merkle trees at level 6 (64-leaf proof shards) per US 12,730,827 B2,
	// letting verifiers check arbitrary subsets with <=6-step leaf subpaths + ONE deduplicated upper multiproof.
	ProofShardLevel = 6
)

// ShardedLeafProof proves one disclosed leaf up to its level-6 (64-leaf) shard root.
type ShardedLeafProof struct {
	Index    int      `json:"index"`
	Leaf     string   `json:"leaf"`
	ShardIdx int      `json:"shardIdx"`
	SubPath  []string `json:"subPath"` // <= 6 sibling hashes up to shard root
}

// SubsetDisclosureProof is a deduplicated proof-sharded multiproof (L_ZKDis v2 / US 12,730,827 B2)
// proving an arbitrary subset of leaves against an anchored double-SHA-256 batch root R_n.
type SubsetDisclosureProof struct {
	Version       int                `json:"version"`
	BatchSize     int                `json:"batchSize"`
	RootHex       string             `json:"rootHex"`
	ShardLevel    int                `json:"shardLevel"`
	Leaves        []ShardedLeafProof `json:"leaves"`
	UpperSiblings map[string]string  `json:"upperSiblings,omitempty"` // "level:pos" -> 32B hex sibling
}

// BuildSubsetProof constructs a proof-sharded subset disclosure proof for indices in a batch tree.
func BuildSubsetProof(leaves [][]byte, indices []int) (*SubsetDisclosureProof, error) {
	if len(leaves) == 0 {
		return nil, core.ErrEmptyLeaves
	}
	if len(indices) == 0 {
		return nil, errors.New("memrepo: subset proof requires at least one leaf index")
	}
	tree, err := core.BuildTree(leaves)
	if err != nil {
		return nil, err
	}
	totalHeight := len(tree.Levels) - 1
	shardLevel := ProofShardLevel
	if totalHeight < shardLevel {
		shardLevel = totalHeight
	}

	seen := make(map[int]bool, len(indices))
	uniq := make([]int, 0, len(indices))
	for _, idx := range indices {
		if idx < 0 || idx >= len(leaves) {
			return nil, fmt.Errorf("%w: %d", core.ErrIndexOutRange, idx)
		}
		if !seen[idx] {
			seen[idx] = true
			uniq = append(uniq, idx)
		}
	}
	sort.Ints(uniq)

	disclosed := make([]ShardedLeafProof, 0, len(uniq))
	activeShardIdx := make(map[int]bool)
	for _, idx := range uniq {
		sub := make([]string, 0, shardLevel)
		pos := idx
		for lvl := 0; lvl < shardLevel; lvl++ {
			nodes := tree.Levels[lvl]
			var sib []byte
			if pos%2 == 0 {
				if pos+1 < len(nodes) {
					sib = nodes[pos+1]
				} else {
					sib = nodes[pos]
				}
			} else {
				sib = nodes[pos-1]
			}
			sub = append(sub, hex.EncodeToString(sib))
			pos /= 2
		}
		activeShardIdx[pos] = true
		disclosed = append(disclosed, ShardedLeafProof{
			Index:    idx,
			Leaf:     hex.EncodeToString(leaves[idx]),
			ShardIdx: pos,
			SubPath:  sub,
		})
	}

	// Build ONE deduplicated upper multiproof from shardLevel -> totalHeight.
	upperSiblings := make(map[string]string)
	curPresent := activeShardIdx
	for lvl := shardLevel; lvl < totalHeight; lvl++ {
		nodes := tree.Levels[lvl]
		nextPresent := make(map[int]bool)
		for pos := range curPresent {
			var sibPos int
			if pos%2 == 0 {
				if pos+1 < len(nodes) {
					sibPos = pos + 1
				} else {
					sibPos = pos
				}
			} else {
				sibPos = pos - 1
			}
			// Only emit sibling if it is not already derivable from another disclosed shard!
			if !curPresent[sibPos] && sibPos != pos {
				upperSiblings[fmt.Sprintf("%d:%d", lvl, sibPos)] = hex.EncodeToString(nodes[sibPos])
			}
			nextPresent[pos/2] = true
		}
		curPresent = nextPresent
	}

	return &SubsetDisclosureProof{
		Version:       2,
		BatchSize:     len(leaves),
		RootHex:       hex.EncodeToString(tree.Root),
		ShardLevel:    shardLevel,
		Leaves:        disclosed,
		UpperSiblings: upperSiblings,
	}, nil
}

// VerifySubsetProof verifies a proof-sharded SubsetDisclosureProof offline against expectedRootHex
// and expectedBatchSize (which MUST be derived from the verified on-chain NektarAnchor.Size,
// never trusted from an untrusted proof header).
func VerifySubsetProof(expectedRootHex string, expectedBatchSize int, proof *SubsetDisclosureProof) error {
	if proof == nil {
		return errors.New("memrepo: nil subset proof")
	}
	if proof.Version != 2 {
		return fmt.Errorf("memrepo: unsupported subset proof version %d", proof.Version)
	}
	if expectedBatchSize <= 0 {
		return errors.New("memrepo: expectedBatchSize must be > 0")
	}
	if proof.BatchSize != expectedBatchSize || len(proof.Leaves) == 0 || len(proof.Leaves) > expectedBatchSize {
		return errors.New("memrepo: invalid subset batchSize or leaf count")
	}
	expectedRoot, err := hex.DecodeString(expectedRootHex)
	if err != nil || len(expectedRoot) != 32 {
		return errors.New("memrepo: expectedRootHex must be 32-byte hex")
	}
	totalHeight := core.HeightForLeafCount(expectedBatchSize)
	if totalHeight > core.MaxMerkleProofDepth {
		return core.ErrProofDepthExceed
	}
	wantShardLevel := ProofShardLevel
	if totalHeight < wantShardLevel {
		wantShardLevel = totalHeight
	}
	if proof.ShardLevel != wantShardLevel {
		return fmt.Errorf("memrepo: shardLevel %d != expected %d", proof.ShardLevel, wantShardLevel)
	}

	// 1. Fold each disclosed leaf up to its level-6 shard root, enforcing Bitcoin odd-self-pairing
	//    at right boundaries (CVE-2012-2459) and shard-root consistency.
	shardRoots := make(map[int][]byte)
	prevIdx := -1
	for _, lp := range proof.Leaves {
		if lp.Index <= prevIdx || lp.Index >= expectedBatchSize {
			return errors.New("memrepo: subset leaves must be strictly increasing within [0, batchSize)")
		}
		prevIdx = lp.Index
		if len(lp.SubPath) != wantShardLevel {
			return fmt.Errorf("memrepo: leaf %d subPath len %d != %d", lp.Index, len(lp.SubPath), wantShardLevel)
		}
		cur, err := hex.DecodeString(lp.Leaf)
		if err != nil || len(cur) != 32 {
			return fmt.Errorf("memrepo: invalid leaf hex at index %d", lp.Index)
		}
		pos := lp.Index
		lvlWidth := expectedBatchSize
		for _, sh := range lp.SubPath {
			sib, err := hex.DecodeString(sh)
			if err != nil || len(sib) != 32 {
				return fmt.Errorf("memrepo: invalid subpath sibling at index %d", lp.Index)
			}
			if pos&1 == 0 {
				if pos+1 >= lvlWidth && !core.EqualBytes(sib, cur) {
					return fmt.Errorf("memrepo: odd boundary self-pairing violation at index %d", lp.Index)
				}
				cur = core.HashNode(cur, sib)
			} else {
				cur = core.HashNode(sib, cur)
			}
			pos >>= 1
			lvlWidth = (lvlWidth + 1) / 2
		}
		if pos != lp.ShardIdx {
			return fmt.Errorf("memrepo: leaf %d shardIdx %d != computed %d", lp.Index, lp.ShardIdx, pos)
		}
		if existing, ok := shardRoots[pos]; ok {
			if !core.EqualBytes(existing, cur) {
				return fmt.Errorf("memrepo: inconsistent shard root for shard %d", pos)
			}
		} else {
			shardRoots[pos] = cur
		}
	}

	// 2. Fold deduplicated shard roots up to totalHeight using UpperSiblings (handling Bitcoin odd-self-pairing!).
	curNodes := shardRoots
	levelWidth := (expectedBatchSize + (1 << wantShardLevel) - 1) >> wantShardLevel
	for lvl := wantShardLevel; lvl < totalHeight; lvl++ {
		nextNodes := make(map[int][]byte)
		// Process positions in deterministic order.
		positions := make([]int, 0, len(curNodes))
		for p := range curNodes {
			positions = append(positions, p)
		}
		sort.Ints(positions)
		for _, pos := range positions {
			parentPos := pos >> 1
			if _, done := nextNodes[parentPos]; done {
				continue
			}
			leftPos := parentPos << 1
			rightPos := leftPos + 1
			if rightPos >= levelWidth {
				rightPos = leftPos // Bitcoin odd-self-pairing at right boundary
			}
			leftHash, err := resolveUpperNode(curNodes, proof.UpperSiblings, lvl, leftPos)
			if err != nil {
				return err
			}
			var rightHash []byte
			if rightPos == leftPos {
				rightHash = leftHash
			} else {
				rightHash, err = resolveUpperNode(curNodes, proof.UpperSiblings, lvl, rightPos)
				if err != nil {
					return err
				}
			}
			nextNodes[parentPos] = core.HashNode(leftHash, rightHash)
		}
		curNodes = nextNodes
		levelWidth = (levelWidth + 1) / 2
	}

	gotRoot, ok := curNodes[0]
	if !ok || !core.EqualBytes(gotRoot, expectedRoot) {
		return errors.New("memrepo: subset multiproof root mismatch")
	}
	return nil
}

func resolveUpperNode(computed map[int][]byte, provided map[string]string, lvl, pos int) ([]byte, error) {
	if h, ok := computed[pos]; ok {
		return h, nil
	}
	key := fmt.Sprintf("%d:%d", lvl, pos)
	sh, ok := provided[key]
	if !ok {
		return nil, fmt.Errorf("memrepo: missing upper sibling %s", key)
	}
	b, err := hex.DecodeString(sh)
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("memrepo: invalid upper sibling hex at %s", key)
	}
	return b, nil
}
