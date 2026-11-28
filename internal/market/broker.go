package market

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kryptokrates/nektar/internal/autonomy"
	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/eventbus"
	"github.com/kryptokrates/nektar/internal/fabric"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/memory"
)

const (
	maxPendingSalesTotal    = 65536
	maxPendingSalesPerBuyer = 4096
	maxSeenBboxNonces       = 65536
	maxBboxExpected         = 16384
	bboxExpectedTTLSeconds  = 1800
	maxConcurrentQuotes     = 128
	maxMarketTapeEvents     = 64
	bboxWorkerPoolSize      = 16
)

type bboxWorkItem struct {
	hubURL string
	msg    hub.BboxMessage
	done   chan error
}

// Broker drives real autonomous agent-to-agent and agent-to-Hub knowledge trading over live BRC-149/BRC-223 + NAT bbox.
type Broker struct {
	mem        *memory.Service
	engine     *Engine
	disc       *discovery.Service
	cfg        BrokerConfig
	httpClient *http.Client
	sseClient  *http.Client
	quoteSem   chan struct{}
	bgSem      chan struct{}
	bboxWorkCh chan bboxWorkItem

	mu                   sync.RWMutex
	restate              *RestateClient
	matcher              Matcher
	watchdog             *autonomy.Watchdog
	subSup               *SubscriptionSupervisor
	fabricSub            *fabric.Subscriber
	planeHosts           map[string]bool
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

	cursorMu     sync.Mutex
	hubCursors   map[string]int64
	dirtyCursors map[string]int64
	cursorDirtyN int

	repMu        sync.Mutex
	dirtyRepOnts map[string]struct{}
	repWakeCh    chan struct{}

	startMu    sync.Mutex
	started    bool
	runCtx     context.Context
	stopCancel context.CancelFunc
	wg         sync.WaitGroup

	sseConnected atomic.Int64
	sseConnCh    chan struct{}
	bboxWakeCh   chan struct{}
	sseWakes     atomic.Int64
	pollWakes    atomic.Int64
	bboxWakes    atomic.Int64
}

// NewBroker constructs a live Broker attached to mem, engine, and disc, syncing persisted operator policies.
func NewBroker(mem *memory.Service, engine *Engine, disc *discovery.Service, cfg BrokerConfig) *Broker {
	if cfg.BboxOffice == "" || strings.EqualFold(cfg.BboxOffice, "default") {
		cfg.BboxOffice = bstack.NormalizeBboxOffice("nektar")
	} else {
		cfg.BboxOffice = bstack.NormalizeBboxOffice(cfg.BboxOffice)
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
			cfg.GraceTimeout = 50 * time.Millisecond
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
		bgSem:                make(chan struct{}, 256),
		bboxWorkCh:           make(chan bboxWorkItem, 256),
		restate:              cfg.RestateClient,
		matcher:              cfg.Matcher,
		watchdog:             cfg.Watchdog,
		planeHosts:           make(map[string]bool),
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
		hubCursors:           make(map[string]int64),
		dirtyCursors:         make(map[string]int64),
		dirtyRepOnts:         make(map[string]struct{}),
		repWakeCh:            make(chan struct{}, 1),
		sseConnCh:            make(chan struct{}, 1),
		bboxWakeCh:           make(chan struct{}, 1),
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

// WakeBbox triggers an immediate non-blocking drain of pending NAT bbox inbox messages (zero polling).
func (b *Broker) WakeBbox() {
	if b == nil || b.bboxWakeCh == nil {
		return
	}
	select {
	case b.bboxWakeCh <- struct{}{}:
	default:
	}
}

func (b *Broker) signalSSEConnected() {
	if b == nil || b.sseConnCh == nil {
		return
	}
	select {
	case b.sseConnCh <- struct{}{}:
	default:
	}
}

// WaitConnected blocks until at least minHosts SSE hub streams have completed HTTP 200 handshake
// (or timeout/ctx expires), waking immediately via sseConnCh without sleep-polling (RCA-5).
func (b *Broker) WaitConnected(ctx context.Context, minHosts int64, timeout time.Duration) bool {
	if minHosts <= 0 {
		return true
	}
	if b.sseConnected.Load() >= minHosts {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if b.sseConnected.Load() >= minHosts {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return b.sseConnected.Load() >= minHosts
		case <-b.sseConnCh:
		}
	}
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
func (b *Broker) PubHex() string    { return b.engine.salePubHex() }
func (b *Broker) PublicURL() string { return strings.TrimRight(b.cfg.PublicURL, "/") }
func (b *Broker) SetWorkflowClient(wc *WorkflowClient) {
	b.mu.Lock()
	b.restate = wc
	b.mu.Unlock()
}
func (b *Broker) WorkflowClient() *WorkflowClient {
	b.mu.RLock()
	wc := b.restate
	b.mu.RUnlock()
	if wc == nil && b.engine != nil {
		wc = b.engine.WorkflowClient()
	}
	return wc
}
func (b *Broker) EnsureWorkflowClient() (*WorkflowClient, error) {
	if wc := b.WorkflowClient(); wc != nil && wc.IsRunning() {
		return wc, nil
	}
	if b.engine == nil {
		return nil, errors.New("market: engine unavailable")
	}
	wc, err := b.engine.EnsureWorkflows()
	if err != nil {
		return nil, err
	}
	b.SetWorkflowClient(wc)
	return wc, nil
}
func (b *Broker) SetRestateClient(rc *RestateClient) { b.SetWorkflowClient(rc) }
func (b *Broker) RestateClient() *RestateClient      { return b.WorkflowClient() }
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
		if fj := b.engine.FastJournal(); fj != nil {
			if listings, err := fj.CachedListings(b.mem.DB()); err == nil {
				for _, l := range listings {
					if l.Active {
						raw = append(raw, l.Domains...)
					}
				}
			}
		} else if listings, err := b.mem.DB().ListSellListings(); err == nil {
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
	if rawFab := strings.TrimSpace(os.Getenv("NEKTAR_FABRIC_UPSTREAM")); rawFab != "" {
		var upstreams []netip.AddrPort
		for _, part := range strings.Split(rawFab, ",") {
			if ap, pErr := netip.ParseAddrPort(strings.TrimSpace(part)); pErr == nil {
				upstreams = append(upstreams, ap)
			}
		}
		if len(upstreams) > 0 {
			var bus *eventbus.Bus
			if b.mem != nil {
				bus = b.mem.EventBus()
			}
			if sub, fErr := fabric.StartSubscriber(runCtx, fabric.SubscriberConfig{
				Label:     "agent-" + shortPub(b.PubHex()),
				Upstreams: upstreams,
				Bus:       bus,
				OnObject: func(oCtx context.Context, ev fabric.DeliveryEvent) {
					_ = b.handlePlaneDelivery(oCtx, "", 0, ev.Topic, ev.BEEF)
				},
			}); fErr == nil {
				sub.SetTopics(b.electedPlaneTopics())
				b.mu.Lock()
				b.fabricSub = sub
				b.mu.Unlock()
			}
		}
	}
	for i := 0; i < bboxWorkerPoolSize; i++ {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.runBboxWorker(runCtx)
		}()
	}
	b.wg.Add(5)
	go func() {
		defer b.wg.Done()
		b.runStateFlusher(runCtx)
	}()
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
	// Jittered low-frequency reconciliation floor catching any events on legacy SSE-only mocks + confirmations.
	go func() {
		defer b.wg.Done()
		for {
			wait := time.Duration(float64(45*time.Second) * (0.7 + 0.6*rand.Float64()))
			select {
			case <-runCtx.Done():
				return
			case <-time.After(wait):
				for _, h := range b.disc.ActiveHosts() {
					if !b.isPlaneConnected(h) {
						b.pollHostOnce(runCtx, h)
					}
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

// Stop cancels all background Broker plane/SSE, poll, bbox, and crawl loops and waits for them to exit cleanly.
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
	fSub := b.fabricSub
	b.fabricSub = nil
	for h, c := range b.activeLoops {
		c()
		delete(b.activeLoops, h)
	}
	b.mu.Unlock()
	if fSub != nil {
		_ = fSub.Close()
	}
	b.wg.Wait()
	b.flushDirtyCursors()
	b.flushDirtyReputation()
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

func topicsForMarketAd(net string, domains []string, ontologyID string) []string {
	base := lanesForDomains(net, domains)
	seen := make(map[string]bool, len(base)+len(domains)+1)
	out := make([]string, 0, len(base)+len(domains)+1)
	for _, t := range base {
		seen[t] = true
		out = append(out, t)
	}
	doms := domains
	if len(doms) == 0 {
		doms = []string{"general"}
	}
	for _, dt := range bstack.TopicsForMarketAd(net, doms, ontologyID) {
		if !seen[dt] {
			seen[dt] = true
			out = append(out, dt)
		}
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
