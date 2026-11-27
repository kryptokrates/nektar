package market

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kryptokrates/nektar/internal/autonomy"
	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/memory"
)

const (
	maxPendingSalesTotal    = 128
	maxPendingSalesPerBuyer = 16
	maxSeenBboxNonces       = 8192
	maxBboxExpected         = 512
	bboxExpectedTTLSeconds  = 1800
	maxConcurrentQuotes     = 16
	maxMarketTapeEvents     = 64
)

// Broker drives real autonomous agent-to-agent and agent-to-Hub knowledge trading over live HTTP/SSE + NAT bbox.
type Broker struct {
	mem        *memory.Service
	engine     *Engine
	disc       *discovery.Service
	cfg        BrokerConfig
	httpClient *http.Client
	sseClient  *http.Client
	quoteSem   chan struct{}
	bgSem      chan struct{}

	mu                   sync.RWMutex
	restate              *RestateClient
	matcher              Matcher
	watchdog             *autonomy.Watchdog
	subSup               *SubscriptionSupervisor
	pendingSales         map[string]*pendingSale
	pendingConfirmations map[string]*pendingConfirmEntry // txid -> over-cap payment waiting for settleDepth
	offerInbox           map[string]chan bstack.OfferAd
	bboxReplies          map[string]chan PurchaseResponse
	bboxExpected         map[string]string // offerID -> expected sellerPub
	bboxWantByOffer      map[string]string // offerID -> wantID (for Restate BuyerDeal/deliverKey wakeup)
	bboxExpectedAt       map[string]int64  // offerID -> registered unix timestamp (bounded TTL eviction)
	activeLoops          map[string]context.CancelFunc
	seenWants            map[string]int64 // wantID -> seenUnix (bounded)
	seenBboxNonces       map[string]int64 // sender:nonce -> seenUnix (bounded replay guard)
	announceWantSeen     map[string]int64 // orderID:sellerPub -> lastWantUnix (5m rate limit)
	packs                []bstack.PackRef
	bountyDoms           []string
	marketTape           []MarketTapeEvent
	tapeListeners        []func(event string, ev MarketTapeEvent)

	cursorMu sync.Mutex

	startMu    sync.Mutex
	started    bool
	runCtx     context.Context
	stopCancel context.CancelFunc
	wg         sync.WaitGroup

	sseConnected atomic.Int64
	sseWakes     atomic.Int64
	pollWakes    atomic.Int64
	bboxWakes    atomic.Int64
}

// NewBroker constructs a live Broker attached to mem, engine, and disc, syncing persisted operator policies.
func NewBroker(mem *memory.Service, engine *Engine, disc *discovery.Service, cfg BrokerConfig) *Broker {
	if cfg.BboxOffice == "" {
		cfg.BboxOffice = "default"
	}
	if cfg.CollectTimeout <= 0 {
		if mem != nil && mem.Chain() != nil && (mem.Chain().Network() == "main" || mem.Chain().Network() == "testnet") {
			// Live BSV cross-hub Spine-and-Leaf round-trips involve 2-4 real Arcade broadcasts
			// (Want -> Leaf -> Spine -> Seller -> Offer -> Leaf -> Spine -> Buyer); first offer
			// still short-circuits via GraceTimeout (200ms) so fast local quotes add zero delay.
			cfg.CollectTimeout = 45 * time.Second
		} else {
			cfg.CollectTimeout = 4 * time.Second
		}
	}
	if cfg.GraceTimeout <= 0 {
		if cfg.Matcher != nil && cfg.Matcher.DefaultMode() != AIModeOff {
			cfg.GraceTimeout = 500 * time.Millisecond
		} else {
			cfg.GraceTimeout = 200 * time.Millisecond
		}
	}
	netName := ""
	if mem != nil && mem.Chain() != nil {
		netName = mem.Chain().Network()
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = core.SafeHTTPClientForNetwork(netName, 15*time.Second)
	}
	sseHC := core.SafeHTTPClientForNetwork(netName, 0)
	_ = SyncPolicyFromDB(mem.DB(), engine.Policy())
	b := &Broker{
		mem:                  mem,
		engine:               engine,
		disc:                 disc,
		cfg:                  cfg,
		httpClient:           hc,
		sseClient:            sseHC,
		quoteSem:             make(chan struct{}, maxConcurrentQuotes),
		bgSem:                make(chan struct{}, 32),
		restate:              cfg.RestateClient,
		matcher:              cfg.Matcher,
		watchdog:             cfg.Watchdog,
		pendingSales:         make(map[string]*pendingSale),
		pendingConfirmations: make(map[string]*pendingConfirmEntry),
		offerInbox:           make(map[string]chan bstack.OfferAd),
		bboxReplies:          make(map[string]chan PurchaseResponse),
		bboxExpected:         make(map[string]string),
		bboxWantByOffer:      make(map[string]string),
		bboxExpectedAt:       make(map[string]int64),
		activeLoops:          make(map[string]context.CancelFunc),
		seenWants:            make(map[string]int64),
		seenBboxNonces:       make(map[string]int64),
	}
	if engine != nil {
		engine.SetBroker(b)
		if cfg.Matcher != nil {
			engine.SetMatcher(cfg.Matcher)
		}
		if disc != nil && engine.Policy() != nil {
			disc.SetHubFeePolicy(engine.Policy().CanPayHubFee, engine.Policy().RecordHubFeeSpend)
		}
	}
	b.subSup = NewSubscriptionSupervisor(mem, engine, SubscriptionSupervisorConfig{
		Watchdog:   cfg.Watchdog,
		HTTPClient: sseHC,
		OnUpdate: func(ev, subID string) {
			if cfg.OnStateChange != nil {
				cfg.OnStateChange(ev, map[string]string{"subId": subID})
			}
		},
	})
	b.restorePersistedState()
	return b
}

// WaitConnected blocks until at least minHosts SSE hub streams have completed HTTP 200 handshake
// (or timeout/ctx expires), eliminating brittle sleep races in tests and rapid startups.
func (b *Broker) WaitConnected(ctx context.Context, minHosts int64, timeout time.Duration) bool {
	if minHosts <= 0 {
		return true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b.sseConnected.Load() >= minHosts {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(15 * time.Millisecond):
		}
	}
	return b.sseConnected.Load() >= minHosts
}

func (b *Broker) Engine() *Engine               { return b.engine }
func (b *Broker) Discovery() *discovery.Service { return b.disc }
func (b *Broker) Subscriptions() *SubscriptionSupervisor {
	if b == nil {
		return nil
	}
	return b.subSup
}
func (b *Broker) Watchdog() *autonomy.Watchdog {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	w := b.watchdog
	b.mu.RUnlock()
	return w
}
func (b *Broker) SetWatchdog(w *autonomy.Watchdog) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.watchdog = w
	b.mu.Unlock()
	if b.subSup != nil {
		b.subSup.SetWatchdog(w)
	}
}
func (b *Broker) PubHex() string                { return hex.EncodeToString(b.engine.saleKey.PubKey().Compressed()) }
func (b *Broker) PublicURL() string             { return strings.TrimRight(b.cfg.PublicURL, "/") }
func (b *Broker) SetRestateClient(rc *RestateClient) {
	b.mu.Lock()
	b.restate = rc
	b.mu.Unlock()
}
func (b *Broker) RestateClient() *RestateClient {
	b.mu.RLock()
	rc := b.restate
	b.mu.RUnlock()
	return rc
}
func (b *Broker) SetMatcher(m Matcher) {
	b.mu.Lock()
	b.matcher = m
	b.mu.Unlock()
	if b.engine != nil {
		b.engine.SetMatcher(m)
	}
}
func (b *Broker) Matcher() Matcher {
	b.mu.RLock()
	m := b.matcher
	b.mu.RUnlock()
	if m == nil && b.engine != nil {
		return b.engine.Matcher()
	}
	return m
}
func (b *Broker) MatcherMetrics() MatcherMetrics {
	if m := b.Matcher(); m != nil {
		return m.MetricsSnapshot()
	}
	return MatcherMetrics{}
}

// ActiveNetworkTopics returns deduplicated live user topics currently advertised by
// active peers and local listings (excluding synthetic cluster:* tags) for dynamic
// non-hierarchical Jev topic correlation.
func (b *Broker) ActiveNetworkTopics() []string {
	if b == nil {
		return nil
	}
	var raw []string
	if b.disc != nil {
		for _, p := range b.disc.Peers() {
			raw = append(raw, p.SellDomains...)
			raw = append(raw, p.BountyDomains...)
		}
	}
	if b.mem != nil && b.mem.DB() != nil {
		if listings, err := b.mem.DB().ListSellListings(); err == nil {
			for _, l := range listings {
				if l.Active {
					raw = append(raw, l.Domains...)
				}
			}
		}
	}
	return normalizePeerTopicList("", raw)
}
func (b *Broker) SetPublicURL(u string) {
	b.mu.Lock()
	b.cfg.PublicURL = strings.TrimRight(u, "/")
	b.mu.Unlock()
}
func (b *Broker) SetAdvertisedPacks(packs []bstack.PackRef, bountyDomains []string) {
	b.mu.Lock()
	b.packs = append([]bstack.PackRef(nil), packs...)
	b.bountyDoms = append([]string(nil), bountyDomains...)
	b.mu.Unlock()
}

// AdvertisedPacks returns a copy of explicitly published starter/public packs.
func (b *Broker) AdvertisedPacks() []bstack.PackRef {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]bstack.PackRef(nil), b.packs...)
}

// Start launches per-host SSE market subscribers (with persisted Last-Event-ID cursors),
// a jittered poll-floor backup, and the NAT-safe bbox inbox consumer. Idempotent across calls.
func (b *Broker) Start(ctx context.Context) {
	b.startMu.Lock()
	if b.started {
		b.startMu.Unlock()
		return
	}
	b.started = true
	runCtx, cancel := context.WithCancel(ctx)
	b.runCtx = runCtx
	b.stopCancel = cancel
	b.startMu.Unlock()

	b.ensureHostLoops(runCtx)
	if b.subSup != nil {
		_ = b.subSup.Start(runCtx)
	}
	b.wg.Add(4)
	go func() {
		defer b.wg.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				b.ensureHostLoops(runCtx)
			}
		}
	}()
	// Jittered low-frequency poll floor catching any events dropped across SSE reconnects (event-arch skill).
	go func() {
		defer b.wg.Done()
		for {
			wait := time.Duration(float64(8*time.Second) * (0.6 + 0.8*rand.Float64()))
			select {
			case <-runCtx.Done():
				return
			case <-time.After(wait):
				for _, h := range b.disc.ActiveHosts() {
					b.pollHostOnce(runCtx, h)
				}
				b.PollPendingConfirmations(runCtx)
			}
		}
	}()
	// NAT-safe bbox inbox loop (delivers purchases to NAT-ed sellers & key reveals to buyers).
	go func() {
		defer b.wg.Done()
		b.bboxLoop(runCtx)
	}()

	// Immediate boot peer crawl + periodic directory crawl (every 15m) & 24h announce keep-alive.
	go func() {
		defer b.wg.Done()
		if hosts := b.disc.ActiveHosts(); len(hosts) > 0 {
			_, _ = b.disc.DiscoverPeers(runCtx, hosts)
		}
		crawlTicker := time.NewTicker(15 * time.Minute)
		announceTicker := time.NewTicker(24 * time.Hour)
		defer crawlTicker.Stop()
		defer announceTicker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-crawlTicker.C:
				if hosts := b.disc.ActiveHosts(); len(hosts) > 0 {
					_, _ = b.disc.DiscoverPeers(runCtx, hosts)
				}
			case <-announceTicker.C:
				if hosts := b.disc.ActiveHosts(); len(hosts) > 0 {
					_, _ = b.AnnounceSelf(runCtx)
				}
			}
		}
	}()
}

// Stop cancels all background Broker SSE, poll, bbox, and crawl loops and waits for them to exit cleanly.
func (b *Broker) Stop() {
	b.startMu.Lock()
	cancel := b.stopCancel
	b.started = false
	b.runCtx = nil
	b.stopCancel = nil
	b.startMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if b.subSup != nil {
		b.subSup.Stop()
	}
	b.mu.Lock()
	for h, c := range b.activeLoops {
		c()
		delete(b.activeLoops, h)
	}
	b.mu.Unlock()
	b.wg.Wait()
}

func (b *Broker) backgroundCtx() context.Context {
	b.startMu.Lock()
	ctx := b.runCtx
	b.startMu.Unlock()
	if ctx != nil {
		return ctx
	}
	return context.Background()
}

func (b *Broker) spawnTracked(timeout time.Duration, fn func(context.Context)) {
	if b == nil || fn == nil {
		return
	}
	base := b.backgroundCtx()
	sem := b.bgSem
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		if sem != nil {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-base.Done():
				return
			}
		}
		ctx, cancel := context.WithTimeout(base, timeout)
		defer cancel()
		fn(ctx)
	}()
}

func lanesForDomains(net string, domains []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, d := range domains {
		l := bstack.LaneOf(net, d)
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		out = append(out, bstack.LaneOf(net, "general"))
	}
	return out
}

func shortPub(s string) string {
	if len(s) <= 10 {
		return s
	}
	return s[:10]
}

func randIDHex() string {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic("market: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
