package chain

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script/interpreter/scriptflag"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/lightwebinc/bcommon/headers"
	"github.com/lightwebinc/bcommon/publish"
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

	defaultLinkWindow   = 6
	defaultReorgWindow  = 100
	defaultNearTipTTL   = 30 * time.Second
	defaultDeepHeaderTTL = 24 * time.Hour
	defaultTipCacheTTL  = 5 * time.Second
	defaultMaxTipSkew   = 6
	maxHeaderCacheSize  = 20_000
)

var (
	// ErrHeaderDisagreement indicates independent header witnesses returned conflicting valid headers.
	ErrHeaderDisagreement = errors.New("chain: independent header witnesses disagree")
	// ErrHeaderQuorumUnavailable indicates fewer than the required quorum of header witnesses responded.
	ErrHeaderQuorumUnavailable = errors.New("chain: header witness quorum unavailable")
	// ErrCircularHeaderAnchor indicates header witnesses are not independent from proof/settler origins.
	ErrCircularHeaderAnchor = errors.New("chain: header anchor is circular or insufficiently independent")
)

// DefaultSettleDepth returns the default confirmation depth for a network.
func DefaultSettleDepth(network string) int {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "main", "mainnet":
		return DefaultMainnetSettleDepth
	case "testnet", "test":
		return DefaultTestnetSettleDepth
	case "regtest":
		return DefaultRegtestSettleDepth
	default:
		return DefaultMemorySettleDepth
	}
}

// DefaultHeaderSources returns the default comma-separated independent header witnesses for network.
// Live networks ALWAYS pair >=2 distinct-origin PoW header services so no single proof/settler
// origin can validate its own BUMPs.
func DefaultHeaderSources(network string) string {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "main", "mainnet":
		return "woc:main,arcade:" + publish.ArcadeMainnet
	case "testnet", "test":
		return "woc:test,arcade:" + publish.ArcadeTestnet
	case "regtest":
		return "native:http://127.0.0.1:8080"
	default:
		return ""
	}
}

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
	_ chaintracker.ChainTracker   = (*WitnessedTracker)(nil)
	_ spv.MedianTimePastProvider  = (*WitnessedTracker)(nil)
	_ spv.ActivationHeightsProvider = (*WitnessedTracker)(nil)
)

// NormalizeSpecOrigin extracts the canonical "scheme://host[:port]" origin from a header, chain, or settle spec.
func NormalizeSpecOrigin(spec string) (string, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return "", errors.New("empty spec")
	}
	// Strip optional nodeapi method qualifier (tx=, proof=, spend=, known=).
	if m, rest, ok := strings.Cut(s, "="); ok && !strings.Contains(m, ":") {
		s = strings.TrimSpace(rest)
	}
	var rawURL string
	switch {
	case strings.HasPrefix(s, "woc:"):
		net := strings.TrimPrefix(s, "woc:")
		if net != "main" && net != "test" {
			return "", fmt.Errorf("invalid woc network %q", s)
		}
		rawURL = "https://api.whatsonchain.com"
	case s == "arcade:main":
		rawURL = publish.ArcadeMainnet
	case s == "arcade:test":
		rawURL = publish.ArcadeTestnet
	case strings.HasPrefix(s, "arcade:"):
		rawURL = strings.TrimPrefix(s, "arcade:")
	case strings.HasPrefix(s, "arc:"):
		rawURL = strings.TrimPrefix(s, "arc:")
	case strings.HasPrefix(s, "chaintracks:"):
		rawURL = strings.TrimPrefix(s, "chaintracks:")
	case strings.HasPrefix(s, "bhs:"):
		rawURL = strings.TrimPrefix(s, "bhs:")
	case strings.HasPrefix(s, "asset:"):
		rawURL = strings.TrimPrefix(s, "asset:")
	case strings.HasPrefix(s, "rpc:"):
		rawURL = strings.TrimPrefix(s, "rpc:")
	case strings.HasPrefix(s, "native:"):
		rawURL = strings.TrimPrefix(s, "native:")
	case strings.HasPrefix(s, "tcp:"):
		addr := strings.TrimPrefix(s, "tcp:")
		if addr == "" {
			return "", fmt.Errorf("invalid tcp spec %q", s)
		}
		return "tcp://" + strings.ToLower(addr), nil
	default:
		rawURL = s
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid origin URL in %q", spec)
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		return scheme + "://" + host + ":" + port, nil
	}
	return scheme + "://" + host, nil
}

// ParseHeaderClient parses one header source spec, fixes bcommon/headers network & regtest native:
// handling, and sets the network's proof-of-work difficulty floor.
func ParseHeaderClient(network, singleSpec string, httpClient *http.Client) (*headers.Client, string, error) {
	netNorm := normalizeNetworkName(network)
	spec := strings.TrimSpace(singleSpec)
	if spec == "" {
		return nil, "", fmt.Errorf("chain: empty header source spec")
	}

	// Expand shorthand arcade:main / arcade:test and strip explicit native: prefix (regtest only).
	isExplicitNative := false
	switch {
	case spec == "arcade:main":
		spec = "arcade:" + publish.ArcadeMainnet
	case spec == "arcade:test":
		spec = "arcade:" + publish.ArcadeTestnet
	case strings.HasPrefix(spec, "native:"):
		isExplicitNative = true
		spec = strings.TrimPrefix(spec, "native:")
	}

	hc, err := headers.NewSource(spec)
	if err != nil {
		return nil, "", fmt.Errorf("chain: header source %q: %w", singleSpec, err)
	}
	if hc.Kind == headers.Native || isExplicitNative {
		if netNorm == "main" || netNorm == "testnet" {
			return nil, "", fmt.Errorf("%w: zero-PoW Native header source %q is forbidden on live network %q", ErrCircularHeaderAnchor, singleSpec, netNorm)
		}
		hc.Kind = headers.Native
		hc.Network = headers.Regtest
		hc.MinDifficulty = 0
	} else {
		// Enforce network consistency for woc: specs and propagate target network onto chaintracks/bhs/arcade.
		if hc.Kind == headers.WhatsOnChain {
			if (netNorm == "main" && hc.Network != headers.Mainnet) || (netNorm == "testnet" && hc.Network != headers.Testnet) {
				return nil, "", fmt.Errorf("chain: header source %q does not match network %q", singleSpec, netNorm)
			}
		}
		switch netNorm {
		case "main":
			hc.Network = headers.Mainnet
			hc.MinDifficulty = headers.MainnetMinDifficulty
		case "testnet":
			hc.Network = headers.Testnet
			hc.MinDifficulty = TestnetMinDifficulty
		default:
			hc.Network = headers.Regtest
			hc.MinDifficulty = 0
		}
	}
	if httpClient != nil {
		hc.HTTP = httpClient
	}
	origin, err := NormalizeSpecOrigin(singleSpec)
	if err != nil {
		return nil, "", err
	}
	return hc, origin, nil
}

// ValidateHeaderIndependence enforces non-circular, multi-witness PoW header independence on live networks.
// It rejects single-source configs, Native zero-PoW sources, duplicate witness origins, and any config
// where a proof or settler origin can self-attest its own BUMPs without a disjoint header witness.
func ValidateHeaderIndependence(network, headerSpec, chainSpec, settleSpec string) error {
	netNorm := normalizeNetworkName(network)
	if netNorm == "memory" {
		return nil
	}
	parts := splitSpecs(headerSpec)
	if len(parts) == 0 {
		return fmt.Errorf("%w: no header sources configured for %s", ErrCircularHeaderAnchor, netNorm)
	}
	if netNorm == "regtest" {
		for _, p := range parts {
			if _, _, err := ParseHeaderClient(netNorm, p, nil); err != nil {
				return err
			}
		}
		return nil
	}

	// Live network (main / testnet): require >=2 distinct-origin PoW sources, zero Native sources.
	headerOrigins := make(map[string]bool, len(parts))
	for _, p := range parts {
		hc, origin, err := ParseHeaderClient(netNorm, p, nil)
		if err != nil {
			return err
		}
		if hc.Kind == headers.Native {
			return fmt.Errorf("%w: Native header source %q forbidden on %s", ErrCircularHeaderAnchor, p, netNorm)
		}
		if headerOrigins[origin] {
			return fmt.Errorf("%w: duplicate header witness origin %q in %q", ErrCircularHeaderAnchor, origin, headerSpec)
		}
		headerOrigins[origin] = true
	}
	if len(headerOrigins) < 2 {
		return fmt.Errorf("%w: live network %q requires >=2 independent distinct-origin PoW header witnesses (got %d from %q)",
			ErrCircularHeaderAnchor, netNorm, len(headerOrigins), headerSpec)
	}

	// Verify every proof/settler origin has at least one strictly disjoint header witness.
	checkDisjoint := func(label, spec string) error {
		for _, item := range splitSpecs(spec) {
			orig, err := NormalizeSpecOrigin(item)
			if err != nil {
				continue
			}
			disjoint := 0
			for hOrig := range headerOrigins {
				if hOrig != orig {
					disjoint++
				}
			}
			if disjoint == 0 {
				return fmt.Errorf("%w: %s origin %q has zero disjoint header witnesses in %q",
					ErrCircularHeaderAnchor, label, orig, headerSpec)
			}
		}
		return nil
	}
	if err := checkDisjoint("chainSpec", chainSpec); err != nil {
		return err
	}
	if err := checkDisjoint("settleSpec", settleSpec); err != nil {
		return err
	}
	return nil
}

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
		if len(witnesses) < 2 {
			return nil, fmt.Errorf("%w: testnet requires >=2 distinct-origin header witnesses", ErrCircularHeaderAnchor)
		}
		quorum = len(witnesses)/2 + 1
		if quorum < 2 {
			quorum = 2
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

func normalizeNetworkName(network string) string {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "main", "mainnet":
		return "main"
	case "test", "testnet":
		return "testnet"
	case "regtest":
		return "regtest"
	default:
		return "memory"
	}
}

func splitSpecs(spec string) []string {
	var out []string
	for _, part := range strings.Split(spec, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
