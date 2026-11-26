package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script/interpreter/scriptflag"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/lightwebinc/bcommon/headers"
)

// Default settle depths (confirmations required before irreversible value-final settlement).
const (
	DefaultMainnetSettleDepth = 6
	DefaultTestnetSettleDepth = 2
	DefaultRegtestSettleDepth = 1
	DefaultMemorySettleDepth  = 1

	// TestnetMinDifficulty enforces Bitcoin's consensus minimum difficulty (1.0, bits 0x1d00ffff)
	// on testnet headers so a lying source cannot forge trivial regtest-like targets.
	TestnetMinDifficulty = 1.0

	defaultLinkWindow    = 6
	defaultReorgWindow   = 100
	defaultNearTipTTL    = 30 * time.Second
	defaultDeepHeaderTTL = 24 * time.Hour
	defaultTipCacheTTL   = 5 * time.Second
	defaultMaxTipSkew    = 6
	maxHeaderCacheSize   = 20_000
)

var (
	// ErrHeaderDisagreement indicates independent header witnesses returned conflicting valid headers.
	ErrHeaderDisagreement = errors.New("chain: independent header witnesses disagree")
	// ErrHeaderQuorumUnavailable indicates fewer than the required quorum of header witnesses responded.
	ErrHeaderQuorumUnavailable = errors.New("chain: header witness quorum unavailable")
	// ErrCircularHeaderAnchor indicates header witnesses are not independent from proof/settler origins.
	ErrCircularHeaderAnchor = errors.New("chain: header anchor is circular or insufficiently independent")
)

// HeaderWitnessSource is the minimal interface satisfied by *headers.Client and mock test witnesses.
type HeaderWitnessSource interface {
	HeaderAt(ctx context.Context, height uint32) (*headers.Header, error)
	CurrentHeight(ctx context.Context) (uint32, error)
	IsValidRootForHeight(ctx context.Context, root *chainhash.Hash, height uint32) (bool, error)
}

// NamedWitness binds an independent HeaderWitnessSource to its canonical origin and label.
type NamedWitness struct {
	Name   string
	Origin string
	Kind   headers.Kind
	Source HeaderWitnessSource
}

// ReorgHandler receives fork notifications when a cached block header changes on the witnessed chain.
type ReorgHandler func(forkHeight uint32, orphanedHashes []string)

type cachedHeader struct {
	hdr       headers.Header
	raw       [80]byte
	work      *big.Int
	fetchedAt time.Time
}

// WitnessedTracker is a multi-witness, PoW-checked, Prev-hash-linked, cumulative-work-checked,
// reorg-aware ChainTracker that also implements spv.MedianTimePastProvider and spv.ActivationHeightsProvider.
type WitnessedTracker struct {
	network       string
	witnesses     []NamedWitness
	quorum        int
	minDifficulty float64
	linkWindow    uint32
	reorgWindow   uint32
	nearTipTTL    time.Duration
	tipCacheTTL   time.Duration

	mu          sync.RWMutex
	cache       map[uint32]cachedHeader
	cachedTip   uint32
	cachedTipAt time.Time

	reorgMu    sync.RWMutex
	reorgHooks []ReorgHandler
}

var (
	_ chaintracker.ChainTracker     = (*WitnessedTracker)(nil)
	_ spv.MedianTimePastProvider    = (*WitnessedTracker)(nil)
	_ spv.ActivationHeightsProvider = (*WitnessedTracker)(nil)
)

// NewWitnessedTrackerFromSpec parses comma-separated header sources, enforces live-network witness
// independence, and builds a WitnessedTracker with local PoW, Prev-linkage, cumulative work, and reorg cache.
func NewWitnessedTrackerFromSpec(network, headerSpec, chainSpec, settleSpec string, httpClient *http.Client) (*WitnessedTracker, error) {
	if err := ValidateHeaderIndependence(network, headerSpec, chainSpec, settleSpec); err != nil {
		return nil, err
	}
	netNorm := normalizeNetworkName(network)
	parts := splitSpecs(headerSpec)
	witnesses := make([]NamedWitness, 0, len(parts))
	for _, p := range parts {
		hc, origin, err := ParseHeaderClient(netNorm, p, httpClient)
		if err != nil {
			return nil, err
		}
		witnesses = append(witnesses, NamedWitness{
			Name:   p,
			Origin: origin,
			Kind:   hc.Kind,
			Source: hc,
		})
	}
	return NewWitnessedTracker(netNorm, witnesses)
}

// NewWitnessedTracker constructs a WitnessedTracker over explicit NamedWitness instances.
// Live networks ("main", "testnet") require >=2 distinct-origin non-Native witnesses.
func NewWitnessedTracker(network string, witnesses []NamedWitness) (*WitnessedTracker, error) {
	netNorm := normalizeNetworkName(network)
	if len(witnesses) == 0 {
		return nil, fmt.Errorf("%w: at least one header witness is required", ErrCircularHeaderAnchor)
	}
	seenOrigins := make(map[string]bool, len(witnesses))
	for _, w := range witnesses {
		if w.Source == nil {
			return nil, fmt.Errorf("chain: nil header witness source %q", w.Name)
		}
		if (netNorm == "main" || netNorm == "testnet") && w.Kind == headers.Native {
			return nil, fmt.Errorf("%w: Native header witness %q forbidden on %s", ErrCircularHeaderAnchor, w.Name, netNorm)
		}
		orig := strings.ToLower(strings.TrimSpace(w.Origin))
		if orig == "" {
			orig = strings.ToLower(strings.TrimSpace(w.Name))
		}
		if seenOrigins[orig] {
			return nil, fmt.Errorf("%w: duplicate header witness origin %q", ErrCircularHeaderAnchor, orig)
		}
		seenOrigins[orig] = true
	}
	quorum := 1
	minDiff := 0.0
	switch netNorm {
	case "main":
		if len(witnesses) < 2 {
			return nil, fmt.Errorf("%w: mainnet requires >=2 distinct-origin header witnesses", ErrCircularHeaderAnchor)
		}
		quorum = len(witnesses)/2 + 1
		if quorum < 2 {
			quorum = 2
		}
		minDiff = headers.MainnetMinDifficulty
	case "testnet":
		if len(witnesses) >= 2 {
			quorum = len(witnesses)/2 + 1
			if quorum < 2 {
				quorum = 2
			}
		} else {
			quorum = 1
		}
		minDiff = TestnetMinDifficulty
	default:
		if len(witnesses) >= 2 {
			quorum = len(witnesses)/2 + 1
		}
	}

	return &WitnessedTracker{
		network:       netNorm,
		witnesses:     witnesses,
		quorum:        quorum,
		minDifficulty: minDiff,
		linkWindow:    defaultLinkWindow,
		reorgWindow:   defaultReorgWindow,
		nearTipTTL:    defaultNearTipTTL,
		tipCacheTTL:   defaultTipCacheTTL,
		cache:         make(map[uint32]cachedHeader, 256),
	}, nil
}

// Network returns the normalized network name ("main", "testnet", "regtest", "memory").
func (t *WitnessedTracker) Network() string { return t.network }

// WitnessCount returns the number of configured independent witnesses.
func (t *WitnessedTracker) WitnessCount() int { return len(t.witnesses) }

// Quorum returns the minimum number of agreeing distinct-origin witnesses required.
func (t *WitnessedTracker) Quorum() int { return t.quorum }

// OnReorg registers a callback invoked whenever a header fork/reorg is detected.
func (t *WitnessedTracker) OnReorg(fn ReorgHandler) {
	if fn == nil {
		return
	}
	t.reorgMu.Lock()
	t.reorgHooks = append(t.reorgHooks, fn)
	t.reorgMu.Unlock()
}

// ActivationHeights implements spv.ActivationHeightsProvider so go-sdk/spv.Verify applies
// exact network-specific Genesis/Chronicle script validation flags.
func (t *WitnessedTracker) ActivationHeights() scriptflag.ActivationHeights {
	switch t.network {
	case "main":
		return scriptflag.MainNetActivationHeights
	case "testnet":
		return scriptflag.TestNetActivationHeights
	default:
		return scriptflag.ActivationHeights{}
	}
}

// HeaderAt returns a verified, PoW-checked, Prev-hash-linked, multi-witness-agreed header at height.
func (t *WitnessedTracker) HeaderAt(ctx context.Context, height uint32) (*headers.Header, error) {
	if t.isNativeOnly() {
		return nil, errors.New("chain: regtest Native header source does not provide full 80-byte headers")
	}
	if hit, ok := t.getCachedHeader(height); ok {
		hdr := hit.hdr
		return &hdr, nil
	}
	entry, _, err := t.fetchAndLinkHeader(ctx, height, true)
	if err != nil {
		return nil, err
	}
	hdr := entry.hdr
	return &hdr, nil
}

// HeaderBytes returns the verified 80-byte raw block header at height (using HeaderCache).
func (t *WitnessedTracker) HeaderBytes(ctx context.Context, height uint32) ([]byte, error) {
	if hit, ok := t.getCachedHeader(height); ok {
		out := make([]byte, 80)
		copy(out, hit.raw[:])
		return out, nil
	}
	entry, _, err := t.fetchAndLinkHeader(ctx, height, true)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 80)
	copy(out, entry.raw[:])
	return out, nil
}

// IsValidRootForHeight verifies that root matches the multi-witness, PoW-checked, Prev-linked
// block header at height, automatically re-checking and firing OnReorg on near-tip cache mismatch.
func (t *WitnessedTracker) IsValidRootForHeight(ctx context.Context, root *chainhash.Hash, height uint32) (bool, error) {
	if root == nil {
		return false, nil
	}
	if t.isNativeOnly() {
		// Regtest-only fallback for native overlay bridge.
		return t.witnesses[0].Source.IsValidRootForHeight(ctx, root, height)
	}

	if hit, ok := t.getCachedHeader(height); ok {
		if hit.hdr.MerkleRoot == *root {
			return true, nil
		}
		// Never trust a cached mismatch across a possible reorg: re-fetch directly from witnesses
		// if the cached entry is older than 2s or within reorgWindow of tip.
		if time.Since(hit.fetchedAt) >= 2*time.Second || t.isNearTip(height) {
			fresh, reorgHandled, err := t.fetchAndLinkHeader(ctx, height, false)
			if err != nil {
				if errors.Is(err, headers.ErrUnknownHeight) {
					return false, nil
				}
				return false, err
			}
			if !reorgHandled && fresh.hdr.Hash != hit.hdr.Hash {
				t.handleReorg(ctx, height, fresh)
			}
			return fresh.hdr.MerkleRoot == *root, nil
		}
		return false, nil
	}

	entry, _, err := t.fetchAndLinkHeader(ctx, height, true)
	if err != nil {
		if errors.Is(err, headers.ErrUnknownHeight) {
			return false, nil
		}
		return false, err
	}
	return entry.hdr.MerkleRoot == *root, nil
}

// CurrentHeight queries all witnesses for their tip, requires quorum agreement within maxTipSkew,
// verifies the conservative tip's 80-byte PoW header (never trusting bare JSON heights), and triggers
// reorg detection when the tip regresses or changes hash.
func (t *WitnessedTracker) CurrentHeight(ctx context.Context) (uint32, error) {
	if t.isNativeOnly() {
		h, err := t.witnesses[0].Source.CurrentHeight(ctx)
		if err != nil {
			return 0, err
		}
		if h == 0 {
			return 0, fmt.Errorf("%w: native witness reported height 0", ErrHeaderQuorumUnavailable)
		}
		return h, nil
	}

	t.mu.RLock()
	if t.cachedTip > 0 && time.Since(t.cachedTipAt) < t.tipCacheTTL {
		tip := t.cachedTip
		t.mu.RUnlock()
		return tip, nil
	}
	prevTip := t.cachedTip
	t.mu.RUnlock()

	type res struct {
		h   uint32
		err error
	}
	results := make([]res, len(t.witnesses))
	var wg sync.WaitGroup
	for i, w := range t.witnesses {
		wg.Add(1)
		go func(idx int, witness NamedWitness) {
			defer wg.Done()
			h, err := witness.Source.CurrentHeight(ctx)
			results[idx] = res{h: h, err: err}
		}(i, w)
	}
	wg.Wait()

	var heights []uint32
	var errs []error
	for _, r := range results {
		if r.err != nil {
			errs = append(errs, r.err)
			continue
		}
		if r.h == 0 {
			errs = append(errs, errors.New("witness reported height 0"))
			continue
		}
		heights = append(heights, r.h)
	}
	if len(heights) < t.quorum {
		return 0, fmt.Errorf("%w: got %d/%d tip heights (%w)", ErrHeaderQuorumUnavailable, len(heights), t.quorum, errors.Join(errs...))
	}
	sort.Slice(heights, func(i, j int) bool { return heights[i] < heights[j] })

	// Find largest cluster within defaultMaxTipSkew of size >= quorum and take its conservative min.
	var tip uint32
	foundCluster := false
	for i := len(heights) - 1; i >= t.quorum-1; i-- {
		low := heights[i-t.quorum+1]
		high := heights[i]
		if high-low <= defaultMaxTipSkew {
			tip = low
			foundCluster = true
			break
		}
	}
	if !foundCluster {
		return 0, fmt.Errorf("%w: witness tip heights diverge excessively: %v", ErrHeaderDisagreement, heights)
	}

	// Verify the tip header's PoW and Prev-linkage rather than trusting a bare JSON integer.
	entry, reorgHandled, err := t.fetchAndLinkHeader(ctx, tip, false)
	if err != nil {
		return 0, fmt.Errorf("chain: verify tip header %d: %w", tip, err)
	}

	// Detect reorg if previous cached tip height now has a different hash (when outside linkWindow).
	if !reorgHandled {
		checkH := prevTip
		if checkH > tip {
			checkH = tip
		}
		if checkH > 0 {
			if prevEntry, ok := t.getCachedHeaderAnyAge(checkH); ok {
				if checkH == tip {
					if prevEntry.hdr.Hash != entry.hdr.Hash {
						t.handleReorg(ctx, tip, entry)
					}
				} else if tip-checkH <= t.reorgWindow {
					if freshPrev, fErr := t.fetchConsensusHeader(ctx, checkH); fErr == nil && freshPrev.hdr.Hash != prevEntry.hdr.Hash {
						t.handleReorg(ctx, checkH, freshPrev)
					}
				}
			}
		}
	}

	t.mu.Lock()
	t.cachedTip = tip
	t.cachedTipAt = time.Now()
	t.mu.Unlock()
	return tip, nil
}

// VerifyDepth checks that root is valid at height AND has at least minDepth confirmations
// against the witnessed chain tip. Returns (depth, meetsDepth, err).
func (t *WitnessedTracker) VerifyDepth(ctx context.Context, root *chainhash.Hash, height uint32, minDepth int) (int, bool, error) {
	if minDepth < 1 {
		minDepth = 1
	}
	valid, err := t.IsValidRootForHeight(ctx, root, height)
	if err != nil {
		return 0, false, err
	}
	if !valid {
		return 0, false, nil
	}
	tip, err := t.CurrentHeight(ctx)
	if err != nil {
		return 0, false, err
	}
	if tip < height {
		return 0, false, nil
	}
	depth := int(tip-height) + 1
	return depth, depth >= minDepth, nil
}

// MedianTimePast implements spv.MedianTimePastProvider by computing Bitcoin's consensus
// 11-block median timestamp over verified, linked headers ending at the current witnessed tip.
func (t *WitnessedTracker) MedianTimePast(ctx context.Context) (uint32, error) {
	tip, err := t.CurrentHeight(ctx)
	if err != nil {
		return 0, err
	}
	return t.MedianTimePastAt(ctx, tip)
}

// MedianTimePastAt computes the 11-block median timestamp ending at height (inclusive).
func (t *WitnessedTracker) MedianTimePastAt(ctx context.Context, height uint32) (uint32, error) {
	if t.isNativeOnly() {
		return 0, errors.New("chain: regtest Native header source cannot compute 11-block MTP")
	}
	start := uint32(0)
	if height >= 10 {
		start = height - 10
	}
	times := make([]uint32, 0, int(height-start)+1)
	for h := start; h <= height; h++ {
		hdr, err := t.HeaderAt(ctx, h)
		if err != nil {
			return 0, fmt.Errorf("chain: MTP header at height %d: %w", h, err)
		}
		times = append(times, hdr.Time)
	}
	if len(times) == 0 {
		return 0, errors.New("chain: empty MTP header window")
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	return times[len(times)/2], nil
}
