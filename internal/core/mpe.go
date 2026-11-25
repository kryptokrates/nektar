package core

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
)

const (
	// MaxMPEFields caps field count in a single Merkle Proof Entity to bound CPU/memory on untrusted inputs.
	MaxMPEFields = 1024
)

var (
	tagMPEV1 = []byte("nektar:mpe:v1")

	ErrEmptyMPE       = errors.New("mpe: at least one field is required")
	ErrMPEFieldMissing = errors.New("mpe: requested field not present")
	ErrMPEVerify      = errors.New("mpe: selective disclosure proof verification failed")
)

// MPEFieldLeaf computes the domain-separated double-SHA-256 leaf hash for a named field (key, val).
func MPEFieldLeaf(key string, val []byte) []byte {
	return DSHA256(Concat(tagMPEV1, Field([]byte(key)), Field(val)))
}

// MPE (Merkle Proof Entity) commits a set of named fields into a canonical binary Merkle tree
// so individual fields can be selectively disclosed and verified offline without exposing other fields.
type MPE struct {
	Root    []byte
	RootHex string
	Keys    []string
	Values  map[string][]byte
	Leaves  [][]byte
	Tree    *MerkleTree
}

// DisclosedField carries one selectively revealed (key, value) pair and its Merkle inclusion path.
type DisclosedField struct {
	Key      string   `json:"key"`
	Value    []byte   `json:"value"`
	Index    int      `json:"index"`
	Siblings []string `json:"siblings"` // 32-byte lowercase hex sibling hashes
}

// SelectiveDisclosureProof proves a subset of named fields against an anchored MPE root.
type SelectiveDisclosureProof struct {
	RootHex   string           `json:"rootHex"`
	LeafCount int              `json:"leafCount"`
	Fields    []DisclosedField `json:"fields"`
}

// BuildMPE builds a deterministic Merkle Proof Entity from named fields (sorted lexicographically by key).
func BuildMPE(fields map[string][]byte) (*MPE, error) {
	if len(fields) == 0 {
		return nil, ErrEmptyMPE
	}
	if len(fields) > MaxMPEFields {
		return nil, fmt.Errorf("mpe: field count %d exceeds max %d", len(fields), MaxMPEFields)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		if k == "" {
			return nil, errors.New("mpe: empty field key is forbidden")
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	vals := make(map[string][]byte, len(keys))
	leaves := make([][]byte, len(keys))
	for i, k := range keys {
		cp := make([]byte, len(fields[k]))
		copy(cp, fields[k])
		vals[k] = cp
		leaves[i] = MPEFieldLeaf(k, cp)
	}
	tree, err := BuildTree(leaves)
	if err != nil {
		return nil, err
	}
	return &MPE{
		Root:    tree.Root,
		RootHex: hex.EncodeToString(tree.Root),
		Keys:    keys,
		Values:  vals,
		Leaves:  leaves,
		Tree:    tree,
	}, nil
}

// ProveFields generates a selective-disclosure Merkle proof covering only the requested keys.
func (m *MPE) ProveFields(keys []string) (*SelectiveDisclosureProof, error) {
	if m == nil || len(m.Leaves) == 0 {
		return nil, ErrEmptyMPE
	}
	if len(keys) == 0 {
		return nil, errors.New("mpe: at least one key must be requested")
	}
	indexByKey := make(map[string]int, len(m.Keys))
	for i, k := range m.Keys {
		indexByKey[k] = i
	}
	seen := make(map[string]bool, len(keys))
	reqKeys := make([]string, 0, len(keys))
	for _, k := range keys {
		if seen[k] {
			continue
		}
		if _, ok := indexByKey[k]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrMPEFieldMissing, k)
		}
		seen[k] = true
		reqKeys = append(reqKeys, k)
	}
	sort.Strings(reqKeys)

	outFields := make([]DisclosedField, 0, len(reqKeys))
	for _, k := range reqKeys {
		idx := indexByKey[k]
		mp, err := ComputeMerkleProof(m.Leaves, idx)
		if err != nil {
			return nil, err
		}
		sibs := make([]string, len(mp.Siblings))
		for j, s := range mp.Siblings {
			sibs[j] = hex.EncodeToString(s)
		}
		cp := make([]byte, len(m.Values[k]))
		copy(cp, m.Values[k])
		outFields = append(outFields, DisclosedField{
			Key:      k,
			Value:    cp,
			Index:    idx,
			Siblings: sibs,
		})
	}
	return &SelectiveDisclosureProof{
		RootHex:   m.RootHex,
		LeafCount: len(m.Leaves),
		Fields:    outFields,
	}, nil
}

// VerifySelectiveDisclosure verifies every disclosed field in proof against expectedRootHex.
// Enforces exact tree height == HeightForLeafCount(proof.LeafCount) and depth <= MaxMerkleProofDepth
// to defeat CVE-2012-2459 odd-leaf duplication and oversized proof DoS.
func VerifySelectiveDisclosure(expectedRootHex string, proof *SelectiveDisclosureProof) error {
	if proof == nil {
		return fmt.Errorf("%w: nil proof", ErrMPEVerify)
	}
	if proof.LeafCount <= 0 || proof.LeafCount > MaxMPEFields {
		return fmt.Errorf("%w: leafCount %d out of bounds [1, %d]", ErrMPEVerify, proof.LeafCount, MaxMPEFields)
	}
	if len(proof.Fields) == 0 || len(proof.Fields) > proof.LeafCount {
		return fmt.Errorf("%w: invalid disclosed field count %d", ErrMPEVerify, len(proof.Fields))
	}
	rootBytes, err := hex.DecodeString(expectedRootHex)
	if err != nil || len(rootBytes) != 32 {
		return fmt.Errorf("%w: invalid expected root hex", ErrMPEVerify)
	}
	if proof.RootHex != "" && proof.RootHex != expectedRootHex {
		return fmt.Errorf("%w: proof root %s != expected %s", ErrMPEVerify, proof.RootHex, expectedRootHex)
	}
	expectedHeight := HeightForLeafCount(proof.LeafCount)
	if expectedHeight > MaxMerkleProofDepth {
		return fmt.Errorf("%w: tree height exceeds max depth", ErrMPEVerify)
	}
	seenKeys := make(map[string]bool, len(proof.Fields))
	seenIdx := make(map[int]bool, len(proof.Fields))
	for _, f := range proof.Fields {
		if f.Key == "" {
			return fmt.Errorf("%w: empty field key", ErrMPEVerify)
		}
		if seenKeys[f.Key] {
			return fmt.Errorf("%w: duplicate field key %q", ErrMPEVerify, f.Key)
		}
		seenKeys[f.Key] = true
		if f.Index < 0 || f.Index >= proof.LeafCount {
			return fmt.Errorf("%w: field %q index %d outside [0, %d)", ErrMPEVerify, f.Key, f.Index, proof.LeafCount)
		}
		if seenIdx[f.Index] {
			return fmt.Errorf("%w: duplicate leaf index %d", ErrMPEVerify, f.Index)
		}
		seenIdx[f.Index] = true
		if len(f.Siblings) != expectedHeight {
			return fmt.Errorf("%w: field %q sibling count %d != expected height %d", ErrMPEVerify, f.Key, len(f.Siblings), expectedHeight)
		}
		sibs := make([][]byte, len(f.Siblings))
		for i, sh := range f.Siblings {
			b, dErr := hex.DecodeString(sh)
			if dErr != nil || len(b) != 32 {
				return fmt.Errorf("%w: field %q invalid sibling hex at level %d", ErrMPEVerify, f.Key, i)
			}
			sibs[i] = b
		}
		leaf := MPEFieldLeaf(f.Key, f.Value)
		if !VerifyProof(leaf, &MerkleProof{Index: f.Index, Siblings: sibs}, rootBytes) {
			return fmt.Errorf("%w: field %q inclusion mismatch", ErrMPEVerify, f.Key)
		}
	}
	return nil
}
