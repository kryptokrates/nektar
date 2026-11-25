package chain

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/lightwebinc/bcommon/headers"
)

// CheckReorg actively checks cached near-tip headers against witnesses, evicting any orphaned
// entries and invoking registered OnReorg callbacks if a chain fork occurred.
func (t *WitnessedTracker) CheckReorg(ctx context.Context) (bool, uint32, error) {
	if t.isNativeOnly() {
		return false, 0, nil
	}
	t.mu.RLock()
	var cachedTip uint32
	for h := range t.cache {
		if h > cachedTip {
			cachedTip = h
		}
	}
	prevEntry, hasPrev := t.cache[cachedTip]
	t.mu.RUnlock()
	if !hasPrev || cachedTip == 0 {
		return false, 0, nil
	}
	fresh, err := t.fetchConsensusHeader(ctx, cachedTip)
	if err != nil {
		if errors.Is(err, headers.ErrUnknownHeight) {
			// Chain shortened below cachedTip: walk back until witnesses hold a height.
			for h := cachedTip - 1; h > 0 && cachedTip-h <= t.reorgWindow; h-- {
				if f, fErr := t.fetchConsensusHeader(ctx, h); fErr == nil {
					forkH := t.handleReorg(ctx, h, f)
					return true, forkH, nil
				}
			}
			return false, 0, nil
		}
		return false, 0, err
	}
	if fresh.hdr.Hash == prevEntry.hdr.Hash {
		return false, 0, nil
	}
	forkH := t.handleReorg(ctx, cachedTip, fresh)
	return true, forkH, nil
}

func (t *WitnessedTracker) isNativeOnly() bool {
	return len(t.witnesses) == 1 && t.witnesses[0].Kind == headers.Native
}

func (t *WitnessedTracker) isNearTip(height uint32) bool {
	t.mu.RLock()
	tip := t.cachedTip
	t.mu.RUnlock()
	return tip == 0 || height+t.reorgWindow >= tip
}

func (t *WitnessedTracker) getCachedHeader(height uint32) (cachedHeader, bool) {
	t.mu.RLock()
	hit, ok := t.cache[height]
	tip := t.cachedTip
	t.mu.RUnlock()
	if !ok {
		return cachedHeader{}, false
	}
	ttl := t.nearTipTTL
	if tip > 0 && height+t.reorgWindow < tip {
		ttl = defaultDeepHeaderTTL
	}
	if time.Since(hit.fetchedAt) >= ttl {
		return cachedHeader{}, false
	}
	return hit, true
}

func (t *WitnessedTracker) getCachedHeaderAnyAge(height uint32) (cachedHeader, bool) {
	t.mu.RLock()
	hit, ok := t.cache[height]
	t.mu.RUnlock()
	return hit, ok
}

func (t *WitnessedTracker) putCachedHeaders(entries []cachedHeader) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.cache)+len(entries) > maxHeaderCacheSize {
		// Evict oldest deep entries while preserving the recent window.
		cutoff := uint32(0)
		if t.cachedTip > t.reorgWindow*2 {
			cutoff = t.cachedTip - t.reorgWindow*2
		}
		for h := range t.cache {
			if h < cutoff {
				delete(t.cache, h)
				if len(t.cache)+len(entries) <= maxHeaderCacheSize/2 {
					break
				}
			}
		}
	}
	for _, e := range entries {
		t.cache[e.hdr.Height] = e
	}
}

// fetchAndLinkHeader fetches height from quorum witnesses AND verifies contiguous Prev-hash linkage
// plus cumulative PoW work across [max(0, height-linkWindow+1) .. height].
// Returns (header, reorgHandled, error). Any cache divergence against the freshly verified window
// triggers handleReorg BEFORE putCachedHeaders so multi-block reorgs find the earliest forkHeight.
func (t *WitnessedTracker) fetchAndLinkHeader(ctx context.Context, height uint32, useCache bool) (cachedHeader, bool, error) {
	start := uint32(0)
	if t.linkWindow > 1 && height >= t.linkWindow-1 {
		start = height - (t.linkWindow - 1)
	}

	window := make([]cachedHeader, 0, int(height-start)+1)
	var toStore []cachedHeader
	cumWork := new(big.Int)

	for h := start; h <= height; h++ {
		var entry cachedHeader
		var found bool
		if useCache && h < height {
			entry, found = t.getCachedHeader(h)
		}
		if !found {
			var err error
			entry, err = t.fetchConsensusHeader(ctx, h)
			if err != nil {
				return cachedHeader{}, false, err
			}
			toStore = append(toStore, entry)
		}
		if len(window) > 0 {
			prev := window[len(window)-1]
			if entry.hdr.Prev != prev.hdr.Hash {
				// If prev came from cache, re-fetch the entire window uncached so multi-block
				// reorgs are verified end-to-end and reported at the earliest forkHeight.
				if useCache {
					return t.fetchAndLinkHeader(ctx, height, false)
				}
				return cachedHeader{}, false, fmt.Errorf("%w: broken Prev hash link at height %d (prev=%s, header[%d]=%s)",
					headers.ErrProofOfWork, h, entry.hdr.Prev, h-1, prev.hdr.Hash)
			}
		}
		cumWork.Add(cumWork, entry.work)
		window = append(window, entry)
	}

	// Enforce cumulative work floor across the verified linked window.
	if t.minDifficulty > 0 && len(window) > 0 {
		minOne := workForTarget(floorTargetForDiff(t.minDifficulty))
		minTotal := new(big.Int).Mul(minOne, big.NewInt(int64(len(window))))
		if cumWork.Cmp(minTotal) < 0 {
			return cachedHeader{}, false, fmt.Errorf("%w: linked window [%d..%d] cumulative work below network floor",
				headers.ErrProofOfWork, start, height)
		}
	}

	// Detect any divergence between freshly fetched headers and existing cache BEFORE overwriting
	// t.cache, so handleReorg walks backward cleanly from the lowest diverged height and collects
	// every orphaned block hash.
	targetEntry := window[len(window)-1]
	reorgHandled := false
	for _, e := range toStore {
		if old, ok := t.getCachedHeaderAnyAge(e.hdr.Height); ok && old.hdr.Hash != e.hdr.Hash {
			t.handleReorg(ctx, e.hdr.Height, e)
			reorgHandled = true
			break
		}
	}
	if !reorgHandled {
		if nextEntry, ok := t.getCachedHeaderAnyAge(height + 1); ok && nextEntry.hdr.Prev != targetEntry.hdr.Hash {
			t.handleReorg(ctx, height, targetEntry)
			reorgHandled = true
		}
	}
	t.putCachedHeaders(toStore)
	return targetEntry, reorgHandled, nil
}

func (t *WitnessedTracker) fetchConsensusHeader(ctx context.Context, height uint32) (cachedHeader, error) {
	type wRes struct {
		entry cachedHeader
		err   error
	}
	results := make([]wRes, len(t.witnesses))
	var wg sync.WaitGroup
	for i, w := range t.witnesses {
		wg.Add(1)
		go func(idx int, witness NamedWitness) {
			defer wg.Done()
			hdr, err := witness.Source.HeaderAt(ctx, height)
			if err != nil {
				results[idx] = wRes{err: fmt.Errorf("%s: %w", witness.Name, err)}
				return
			}
			if hdr == nil {
				results[idx] = wRes{err: fmt.Errorf("%s: %w", witness.Name, headers.ErrUnknownHeight)}
				return
			}
			raw, work, pErr := verifyHeaderLocalPoW(hdr, height, t.minDifficulty)
			if pErr != nil {
				results[idx] = wRes{err: fmt.Errorf("%s: %w", witness.Name, pErr)}
				return
			}
			results[idx] = wRes{
				entry: cachedHeader{
					hdr:       *hdr,
					raw:       raw,
					work:      work,
					fetchedAt: time.Now(),
				},
			}
		}(i, w)
	}
	wg.Wait()

	type bucket struct {
		entry cachedHeader
		count int
	}
	byRaw := make(map[[80]byte]*bucket, len(t.witnesses))
	unknownCount := 0
	var errs []error
	for _, r := range results {
		if r.err != nil {
			if errors.Is(r.err, headers.ErrUnknownHeight) {
				unknownCount++
			}
			errs = append(errs, r.err)
			continue
		}
		b := byRaw[r.entry.raw]
		if b == nil {
			b = &bucket{entry: r.entry}
			byRaw[r.entry.raw] = b
		}
		b.count++
	}

	var best *bucket
	for _, b := range byRaw {
		if best == nil || b.count > best.count {
			best = b
		}
	}
	if best != nil && best.count >= t.quorum {
		return best.entry, nil
	}
	if len(byRaw) > 1 {
		return cachedHeader{}, fmt.Errorf("%w: height %d had %d conflicting headers across witnesses", ErrHeaderDisagreement, height, len(byRaw))
	}
	if unknownCount >= t.quorum {
		return cachedHeader{}, fmt.Errorf("%w: height %d", headers.ErrUnknownHeight, height)
	}
	return cachedHeader{}, fmt.Errorf("%w: height %d (%w)", ErrHeaderQuorumUnavailable, height, errors.Join(errs...))
}

func (t *WitnessedTracker) handleReorg(ctx context.Context, observedHeight uint32, fresh cachedHeader) uint32 {
	forkHeight := observedHeight
	// Walk backward across cached headers to find the earliest height where cached hash diverges.
	for h := observedHeight; h > 0 && observedHeight-h < t.reorgWindow; h-- {
		prevCached, ok := t.getCachedHeaderAnyAge(h - 1)
		if !ok {
			break
		}
		if prevCached.hdr.Hash == fresh.hdr.Prev {
			break
		}
		fPrev, err := t.fetchConsensusHeader(ctx, h-1)
		if err != nil {
			break
		}
		if fPrev.hdr.Hash == prevCached.hdr.Hash {
			break
		}
		forkHeight = h - 1
		fresh = fPrev
	}

	var orphaned []string
	t.mu.Lock()
	for h, entry := range t.cache {
		if h >= forkHeight {
			orphaned = append(orphaned, entry.hdr.Hash.String())
			delete(t.cache, h)
		}
	}
	t.cache[fresh.hdr.Height] = fresh
	t.cachedTipAt = time.Time{} // force fresh tip on next CurrentHeight
	t.mu.Unlock()

	sort.Strings(orphaned)
	t.reorgMu.RLock()
	hooks := append([]ReorgHandler(nil), t.reorgHooks...)
	t.reorgMu.RUnlock()
	for _, fn := range hooks {
		fn(forkHeight, orphaned)
	}
	return forkHeight
}

// verifyHeaderLocalPoW independently re-verifies 80-byte double-SHA-256, target bounds,
// header self-PoW, and network difficulty floor, returning raw 80 bytes and exact block work.
func verifyHeaderLocalPoW(h *headers.Header, wantHeight uint32, minDifficulty float64) ([80]byte, *big.Int, error) {
	var raw [80]byte
	if h == nil {
		return raw, nil, fmt.Errorf("%w: nil header", headers.ErrProofOfWork)
	}
	if h.Height != wantHeight {
		return raw, nil, fmt.Errorf("%w: wanted height %d, got %d", headers.ErrProofOfWork, wantHeight, h.Height)
	}
	binary.LittleEndian.PutUint32(raw[0:4], h.Version)
	copy(raw[4:36], h.Prev[:])
	copy(raw[36:68], h.MerkleRoot[:])
	binary.LittleEndian.PutUint32(raw[68:72], h.Time)
	binary.LittleEndian.PutUint32(raw[72:76], h.Bits)
	binary.LittleEndian.PutUint32(raw[76:80], h.Nonce)

	gotHash := chainhash.DoubleHashH(raw[:])
	if gotHash != h.Hash {
		return raw, nil, fmt.Errorf("%w: height %d hash mismatch (computed %s, claimed %s)", headers.ErrProofOfWork, wantHeight, gotHash, h.Hash)
	}
	target := compactBitsToBig(h.Bits)
	if target.Sign() <= 0 {
		return raw, nil, fmt.Errorf("%w: height %d invalid compact bits %08x", headers.ErrProofOfWork, wantHeight, h.Bits)
	}
	if chainHashToBig(gotHash).Cmp(target) > 0 {
		return raw, nil, fmt.Errorf("%w: height %d hash exceeds target", headers.ErrProofOfWork, wantHeight)
	}
	if minDifficulty > 0 {
		if target.Cmp(floorTargetForDiff(minDifficulty)) > 0 {
			return raw, nil, fmt.Errorf("%w: height %d bits %08x below network difficulty floor %.1f", headers.ErrProofOfWork, wantHeight, h.Bits, minDifficulty)
		}
	}
	return raw, workForTarget(target), nil
}

var (
	diff1Target = compactBitsToBig(0x1d00ffff)
	twoPow256   = new(big.Int).Lsh(big.NewInt(1), 256)
)

func compactBitsToBig(bits uint32) *big.Int {
	mantissa := int64(bits & 0x007fffff)
	exp := uint(bits >> 24)
	n := big.NewInt(mantissa)
	if exp <= 3 {
		n.Rsh(n, 8*(3-exp))
	} else {
		n.Lsh(n, 8*(exp-3))
	}
	if bits&0x00800000 != 0 {
		n.Neg(n)
	}
	return n
}

func chainHashToBig(h chainhash.Hash) *big.Int {
	r := make([]byte, len(h))
	for i := range h {
		r[len(h)-1-i] = h[i]
	}
	return new(big.Int).SetBytes(r)
}

func floorTargetForDiff(minDifficulty float64) *big.Int {
	f := new(big.Float).SetInt(diff1Target)
	f.Quo(f, big.NewFloat(minDifficulty))
	t, _ := f.Int(nil)
	return t
}

func workForTarget(target *big.Int) *big.Int {
	if target == nil || target.Sign() <= 0 {
		return big.NewInt(0)
	}
	denom := new(big.Int).Add(target, big.NewInt(1))
	return new(big.Int).Div(twoPow256, denom)
}
