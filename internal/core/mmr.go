package core

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/bits"
)

const (
	// MaxMMRHeight caps MMR height (height 48 covers 2^48 batches).
	MaxMMRHeight = 48
)

var (
	ErrMMREmpty       = errors.New("mmr: empty accumulator")
	ErrMMRIndexRange  = errors.New("mmr: index out of range")
	ErrMMRInvalid     = errors.New("mmr: invalid proof")
	ErrMMRConsistency = errors.New("mmr: consistency verification failed")
)

// HashMMRNode computes the domain-separated double-SHA-256 internal MMR node hash.
func HashMMRNode(left, right []byte) []byte {
	return DSHA256(Concat(TagMMRNode, pad32(left), pad32(right)))
}

// BagMMRPeaks folds peak hashes right-to-left into a single 32-byte MMR root commitment.
// Returns Zero32Bytes() when peaks is empty, and peaks[0] when there is a single peak.
func BagMMRPeaks(peaks [][]byte) []byte {
	if len(peaks) == 0 {
		return Zero32Bytes()
	}
	acc := pad32(peaks[len(peaks)-1])
	for i := len(peaks) - 2; i >= 0; i-- {
		acc = DSHA256(Concat(TagMMRBag, pad32(peaks[i]), acc))
	}
	return acc
}

// MMR is an append-only Merkle Mountain Range over 32-byte batch roots (R_0, R_1, ...),
// providing O(log N) inclusion proofs and RFC 6962-style O(log N) append-only consistency proofs.
type MMR struct {
	leaves [][]byte
}

// NewMMR builds an MMR from an ordered slice of 32-byte leaves (e.g. batch roots R_0..R_n).
func NewMMR(leaves [][]byte) (*MMR, error) {
	m := &MMR{leaves: make([][]byte, 0, len(leaves))}
	for i, lf := range leaves {
		if len(lf) != 32 {
			return nil, fmt.Errorf("mmr: leaf %d must be 32 bytes, got %d", i, len(lf))
		}
		cp := make([]byte, 32)
		copy(cp, lf)
		m.leaves = append(m.leaves, cp)
	}
	return m, nil
}

// Append adds a 32-byte leaf and returns its 0-based leaf index.
func (m *MMR) Append(leaf []byte) (uint64, error) {
	if len(leaf) != 32 {
		return 0, fmt.Errorf("mmr: leaf must be 32 bytes, got %d", len(leaf))
	}
	cp := make([]byte, 32)
	copy(cp, leaf)
	idx := uint64(len(m.leaves))
	m.leaves = append(m.leaves, cp)
	return idx, nil
}

// Size returns the number of appended leaves.
func (m *MMR) Size() uint64 { return uint64(len(m.leaves)) }

// mmrRangeHash computes the complete binary subtree hash for leaves[start : start+(1<<height)].
func (m *MMR) rangeHash(start, height uint64) []byte {
	if height == 0 {
		return pad32(m.leaves[start])
	}
	half := uint64(1) << (height - 1)
	l := m.rangeHash(start, height-1)
	r := m.rangeHash(start+half, height-1)
	return HashMMRNode(l, r)
}

type mmrPeakSpan struct {
	start  uint64
	height uint64
}

// peakSpans decomposes count into its canonical descending-height complete binary mountains.
func peakSpans(count uint64) []mmrPeakSpan {
	if count == 0 {
		return nil
	}
	var out []mmrPeakSpan
	var offset uint64
	for h := 63; h >= 0; h-- {
		bit := uint64(1) << uint(h)
		if count&bit != 0 {
			out = append(out, mmrPeakSpan{start: offset, height: uint64(h)})
			offset += bit
		}
	}
	return out
}

// Peaks returns the canonical left-to-right peak hashes of the current MMR.
func (m *MMR) Peaks() [][]byte {
	spans := peakSpans(uint64(len(m.leaves)))
	out := make([][]byte, len(spans))
	for i, sp := range spans {
		out[i] = m.rangeHash(sp.start, sp.height)
	}
	return out
}

// Root returns the 32-byte bagged MMR root over all current peaks.
func (m *MMR) Root() []byte {
	return BagMMRPeaks(m.Peaks())
}

// MMRInclusionProof proves leaf at LeafIndex belongs to an MMR of LeafCount leaves with root RootHex.
type MMRInclusionProof struct {
	LeafIndex  uint64   `json:"leafIndex"`
	LeafCount  uint64   `json:"leafCount"`
	Siblings   []string `json:"siblings"`   // bottom-up Merkle siblings inside leaf's mountain
	LeftPeaks  []string `json:"leftPeaks"`  // peaks strictly left of leaf's mountain
	RightPeaks []string `json:"rightPeaks"` // peaks strictly right of leaf's mountain
}

// ProveInclusion constructs an O(log N) inclusion proof for leaf at leafIndex.
func (m *MMR) ProveInclusion(leafIndex uint64) (*MMRInclusionProof, error) {
	n := uint64(len(m.leaves))
	if n == 0 {
		return nil, ErrMMREmpty
	}
	if leafIndex >= n {
		return nil, ErrMMRIndexRange
	}
	spans := peakSpans(n)
	targetIdx := -1
	for i, sp := range spans {
		end := sp.start + (uint64(1) << sp.height)
		if leafIndex >= sp.start && leafIndex < end {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		return nil, ErrMMRIndexRange
	}
	sp := spans[targetIdx]
	sibs := make([]string, 0, sp.height)
	local := leafIndex - sp.start
	for h := uint64(0); h < sp.height; h++ {
		step := uint64(1) << h
		blockStart := sp.start + (local &^ (step - 1))
		var sibStart uint64
		if (local>>h)&1 == 0 {
			sibStart = blockStart + step
		} else {
			sibStart = blockStart - step
		}
		sibs = append(sibs, hex.EncodeToString(m.rangeHash(sibStart, h)))
	}
	left := make([]string, 0, targetIdx)
	for i := 0; i < targetIdx; i++ {
		left = append(left, hex.EncodeToString(m.rangeHash(spans[i].start, spans[i].height)))
	}
	right := make([]string, 0, len(spans)-targetIdx-1)
	for i := targetIdx + 1; i < len(spans); i++ {
		right = append(right, hex.EncodeToString(m.rangeHash(spans[i].start, spans[i].height)))
	}
	return &MMRInclusionProof{
		LeafIndex:  leafIndex,
		LeafCount:  n,
		Siblings:   sibs,
		LeftPeaks:  left,
		RightPeaks: right,
	}, nil
}

// VerifyMMRInclusion verifies an O(log N) MMR inclusion proof for leaf against expectedRoot.
func VerifyMMRInclusion(leaf []byte, proof *MMRInclusionProof, expectedRoot []byte) error {
	if proof == nil || len(leaf) != 32 || len(expectedRoot) != 32 {
		return ErrMMRInvalid
	}
	if proof.LeafCount == 0 || proof.LeafIndex >= proof.LeafCount {
		return ErrMMRIndexRange
	}
	spans := peakSpans(proof.LeafCount)
	targetIdx := -1
	for i, sp := range spans {
		end := sp.start + (uint64(1) << sp.height)
		if proof.LeafIndex >= sp.start && proof.LeafIndex < end {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		return ErrMMRIndexRange
	}
	sp := spans[targetIdx]
	if sp.height > MaxMMRHeight || uint64(len(proof.Siblings)) != sp.height {
		return fmt.Errorf("%w: sibling count %d != mountain height %d", ErrMMRInvalid, len(proof.Siblings), sp.height)
	}
	if len(proof.LeftPeaks) != targetIdx || len(proof.RightPeaks) != len(spans)-targetIdx-1 {
		return fmt.Errorf("%w: peak count mismatch", ErrMMRInvalid)
	}
	cur := pad32(leaf)
	local := proof.LeafIndex - sp.start
	for h := uint64(0); h < sp.height; h++ {
		sib, err := hex.DecodeString(proof.Siblings[h])
		if err != nil || len(sib) != 32 {
			return fmt.Errorf("%w: invalid sibling hex at height %d", ErrMMRInvalid, h)
		}
		if (local>>h)&1 == 0 {
			cur = HashMMRNode(cur, sib)
		} else {
			cur = HashMMRNode(sib, cur)
		}
	}
	peaks := make([][]byte, 0, len(spans))
	for _, lp := range proof.LeftPeaks {
		b, err := hex.DecodeString(lp)
		if err != nil || len(b) != 32 {
			return fmt.Errorf("%w: invalid left peak hex", ErrMMRInvalid)
		}
		peaks = append(peaks, b)
	}
	peaks = append(peaks, cur)
	for _, rp := range proof.RightPeaks {
		b, err := hex.DecodeString(rp)
		if err != nil || len(b) != 32 {
			return fmt.Errorf("%w: invalid right peak hex", ErrMMRInvalid)
		}
		peaks = append(peaks, b)
	}
	gotRoot := BagMMRPeaks(peaks)
	if !EqualBytes(gotRoot, expectedRoot) {
		return fmt.Errorf("%w: reconstructed root mismatch", ErrMMRInvalid)
	}
	return nil
}

// MMRConsistencyProof proves an older MMR state (OldSize, OldRoot) is an exact append-only
// prefix of a newer MMR state (NewSize, NewRoot) in O(log N) hashes.
type MMRConsistencyProof struct {
	OldSize     uint64   `json:"oldSize"`
	NewSize     uint64   `json:"newSize"`
	OldPeaks    []string `json:"oldPeaks"`    // canonical peaks of OldSize (bagging == OldRoot)
	RightChunks []string `json:"rightChunks"` // minimal aligned binary subtrees covering [OldSize, NewSize)
}

func deltaSubtreeSpans(oldSize, newSize uint64) []mmrPeakSpan {
	var out []mmrPeakSpan
	pos := oldSize
	for pos < newSize {
		rem := newSize - pos
		maxPow := uint(bits.Len64(rem) - 1)
		if pos > 0 {
			align := uint(bits.TrailingZeros64(pos))
			if align < maxPow {
				maxPow = align
			}
		}
		out = append(out, mmrPeakSpan{start: pos, height: uint64(maxPow)})
		pos += uint64(1) << maxPow
	}
	return out
}

// ProveConsistency generates an O(log N) prefix-consistency proof between oldSize and newSize.
func (m *MMR) ProveConsistency(oldSize, newSize uint64) (*MMRConsistencyProof, error) {
	if oldSize == 0 || oldSize > newSize || newSize > uint64(len(m.leaves)) {
		return nil, ErrMMRIndexRange
	}
	oldSpans := peakSpans(oldSize)
	oldPeaks := make([]string, len(oldSpans))
	for i, sp := range oldSpans {
		oldPeaks[i] = hex.EncodeToString(m.rangeHash(sp.start, sp.height))
	}
	deltas := deltaSubtreeSpans(oldSize, newSize)
	chunks := make([]string, len(deltas))
	for i, sp := range deltas {
		chunks[i] = hex.EncodeToString(m.rangeHash(sp.start, sp.height))
	}
	return &MMRConsistencyProof{
		OldSize:     oldSize,
		NewSize:     newSize,
		OldPeaks:    oldPeaks,
		RightChunks: chunks,
	}, nil
}

// VerifyMMRConsistency verifies that (oldRoot, oldSize) is an unaltered append-only prefix of (newRoot, newSize).
func VerifyMMRConsistency(oldRoot, newRoot []byte, proof *MMRConsistencyProof) error {
	if proof == nil || len(oldRoot) != 32 || len(newRoot) != 32 {
		return ErrMMRConsistency
	}
	if proof.OldSize == 0 || proof.OldSize > proof.NewSize {
		return ErrMMRConsistency
	}
	oldSpans := peakSpans(proof.OldSize)
	if len(proof.OldPeaks) != len(oldSpans) {
		return fmt.Errorf("%w: old peak count mismatch", ErrMMRConsistency)
	}
	type stackEntry struct {
		height uint64
		hash   []byte
	}
	stack := make([]stackEntry, 0, len(oldSpans)+8)
	oldPeakBytes := make([][]byte, len(oldSpans))
	for i, sp := range oldSpans {
		b, err := hex.DecodeString(proof.OldPeaks[i])
		if err != nil || len(b) != 32 {
			return fmt.Errorf("%w: invalid old peak hex %d", ErrMMRConsistency, i)
		}
		oldPeakBytes[i] = b
		stack = append(stack, stackEntry{height: sp.height, hash: b})
	}
	if !EqualBytes(BagMMRPeaks(oldPeakBytes), oldRoot) {
		return fmt.Errorf("%w: old peaks do not bag to oldRoot", ErrMMRConsistency)
	}
	deltas := deltaSubtreeSpans(proof.OldSize, proof.NewSize)
	if len(proof.RightChunks) != len(deltas) {
		return fmt.Errorf("%w: delta chunk count %d != expected %d", ErrMMRConsistency, len(proof.RightChunks), len(deltas))
	}
	for i, sp := range deltas {
		b, err := hex.DecodeString(proof.RightChunks[i])
		if err != nil || len(b) != 32 {
			return fmt.Errorf("%w: invalid delta chunk hex %d", ErrMMRConsistency, i)
		}
		cur := stackEntry{height: sp.height, hash: b}
		for len(stack) > 0 && stack[len(stack)-1].height == cur.height {
			left := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			cur = stackEntry{
				height: cur.height + 1,
				hash:   HashMMRNode(left.hash, cur.hash),
			}
		}
		stack = append(stack, cur)
	}
	newSpans := peakSpans(proof.NewSize)
	if len(stack) != len(newSpans) {
		return fmt.Errorf("%w: resulting peak count %d != expected %d", ErrMMRConsistency, len(stack), len(newSpans))
	}
	newPeaks := make([][]byte, len(stack))
	for i, se := range stack {
		if se.height != newSpans[i].height {
			return fmt.Errorf("%w: resulting peak height mismatch at %d", ErrMMRConsistency, i)
		}
		newPeaks[i] = se.hash
	}
	if !EqualBytes(BagMMRPeaks(newPeaks), newRoot) {
		return fmt.Errorf("%w: resulting peaks do not bag to newRoot", ErrMMRConsistency)
	}
	return nil
}
