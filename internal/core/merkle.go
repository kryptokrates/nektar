package core

import (
	"errors"
	"math/bits"
)

var (
	ErrEmptyLeaves  = errors.New("merkle: empty leaves")
	ErrIndexOutRange = errors.New("merkle: index out of range")
	ErrInvalidProof = errors.New("merkle: invalid leaf, sibling length, or negative index")
)

// MerkleProof is an inclusion path for leaf at Index within a batch Merkle tree.
type MerkleProof struct {
	Index    int      `json:"index"`
	Siblings [][]byte `json:"siblings"`
}

// MerkleTree holds the root and every level of a double-SHA-256 Merkle tree.
type MerkleTree struct {
	Root   []byte
	Levels [][][]byte
}

// HashLeaf hashes a raw item as a double-SHA-256 Merkle leaf.
func HashLeaf(item []byte) []byte {
	return DSHA256(item)
}

// HashNode hashes a pair of sibling nodes: DSHA256(left || right).
func HashNode(left, right []byte) []byte {
	return DSHA256(Concat(left, right))
}

func nextLevel(level [][]byte) [][]byte {
	out := make([][]byte, 0, (len(level)+1)/2)
	for i := 0; i < len(level); i += 2 {
		l := level[i]
		r := l
		if i+1 < len(level) {
			r = level[i+1]
		}
		out = append(out, HashNode(l, r))
	}
	return out
}

// BuildTree constructs the odd-self-pairing double-SHA-256 Merkle tree over leaves.
func BuildTree(leaves [][]byte) (*MerkleTree, error) {
	if len(leaves) == 0 {
		return nil, ErrEmptyLeaves
	}
	cur := make([][]byte, len(leaves))
	for i, lf := range leaves {
		cp := make([]byte, len(lf))
		copy(cp, lf)
		cur[i] = cp
	}
	levels := [][][]byte{cur}
	for len(cur) > 1 {
		cur = nextLevel(cur)
		levels = append(levels, cur)
	}
	return &MerkleTree{Root: cur[0], Levels: levels}, nil
}

// MerkleRoot returns the root of the batch Merkle tree.
func MerkleRoot(leaves [][]byte) ([]byte, error) {
	t, err := BuildTree(leaves)
	if err != nil {
		return nil, err
	}
	return t.Root, nil
}

// ComputeMerkleProof builds the inclusion path for leaf at index.
func ComputeMerkleProof(leaves [][]byte, index int) (*MerkleProof, error) {
	if index < 0 || index >= len(leaves) {
		return nil, ErrIndexOutRange
	}
	t, err := BuildTree(leaves)
	if err != nil {
		return nil, err
	}
	siblings := make([][]byte, 0, len(t.Levels)-1)
	pos := index
	for lvl := 0; lvl < len(t.Levels)-1; lvl++ {
		nodes := t.Levels[lvl]
		if pos%2 == 0 {
			if pos+1 < len(nodes) {
				siblings = append(siblings, nodes[pos+1])
			} else {
				siblings = append(siblings, nodes[pos])
			}
		} else {
			siblings = append(siblings, nodes[pos-1])
		}
		pos /= 2
	}
	return &MerkleProof{Index: index, Siblings: siblings}, nil
}

// ReconstructRootChecked strictly validates 32-byte leaf & sibling lengths and non-negative index
// before folding leaf through proof.Siblings back to the Merkle root.
func ReconstructRootChecked(leaf []byte, proof *MerkleProof) ([]byte, error) {
	if proof == nil || proof.Index < 0 || len(leaf) != 32 {
		return nil, ErrInvalidProof
	}
	cur := leaf
	idx := proof.Index
	for _, sib := range proof.Siblings {
		if len(sib) != 32 {
			return nil, ErrInvalidProof
		}
		if idx&1 == 0 {
			cur = HashNode(cur, sib)
		} else {
			cur = HashNode(sib, cur)
		}
		idx >>= 1
	}
	return cur, nil
}

// ReconstructRoot folds leaf through proof.Siblings back to the Merkle root.
// Returns nil on malformed inputs (nil proof, negative index, or non-32-byte leaf/sibling).
func ReconstructRoot(leaf []byte, proof *MerkleProof) []byte {
	root, err := ReconstructRootChecked(leaf, proof)
	if err != nil {
		return nil
	}
	return root
}

// VerifyProof returns true iff leaf + proof reconstructs root with valid 32-byte hashes and index >= 0.
// Callers verifying Bitcoin odd-self-pairing trees MUST also verify tree leafCount / height against
// an authenticated commitment (e.g. NektarAnchor.Size) to prevent CVE-2012-2459 odd-leaf duplication.
func VerifyProof(leaf []byte, proof *MerkleProof, root []byte) bool {
	if len(root) != 32 {
		return false
	}
	got, err := ReconstructRootChecked(leaf, proof)
	if err != nil {
		return false
	}
	return EqualBytes(got, root)
}

// HeightForLeafCount returns ceil(log2(leafCount)) (0 for <= 1) in O(1) without shift overflow.
func HeightForLeafCount(leafCount int) int {
	if leafCount <= 1 {
		return 0
	}
	return bits.Len(uint(leafCount - 1))
}
