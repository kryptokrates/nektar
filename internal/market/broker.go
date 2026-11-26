package market

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

// BrokerConfig configures the autonomous live A2A & Hub Market Broker.
type BrokerConfig struct {
	PublicURL      string    // reachable HTTP base URL for direct purchase (empty if behind NAT!)
	BboxOffice     string    // NAT store-and-forward office name (default "default")
	Handle         string    // optional bfinger handle (e.g. "alice@nektar.run")
	IsHub          bool      // true when running as a Hub liquidity maker
	Hub            *hub.Node // optional embedded Hub node for advertising role/terms/fees on dir
	CollectTimeout time.Duration
	GraceTimeout   time.Duration // wait after first offer for competing quotes (default 200ms, or 500ms with AI matcher)
	OnStateChange  func(event string, payload any)
	HTTPClient     *http.Client
	RestateClient  *RestateClient // active Restate durable executor client (BuyerDeal, SellerDeal, Custody, Tx, AnchorCadence)
	Matcher        Matcher        // optional Jev System One semantic matcher
}

// WantRequest describes a buyer's live knowledge purchase request across active hubs.
type WantRequest struct {
	Domains            []string      `json:"domains"`
	Question           string        `json:"question"`
	MaxSats            uint64        `json:"maxSats"`
	TargetShard        string        `json:"targetShard,omitempty"`
	CollectTimeout     time.Duration `json:"collectTimeout,omitempty"`
	ApprovedOverrideID string        `json:"approvedOverrideId,omitempty"`
}

// SellInput arms a local memory shard for autonomous quoting & selling on the market.
type SellInput struct {
	Shard       string   `json:"shard"`
	Domains     []string `json:"domains"`
	PriceSats   uint64   `json:"priceSats"`
	Description string   `json:"description,omitempty"`
}

// PurchaseRequest is sent from Buyer to Seller (via direct HTTP POST /v1/market/purchase OR Hub NAT /bbox/send).
type PurchaseRequest struct {
	OfferID           string `json:"offerId"`
	WantID            string `json:"wantId"`
	BuyerPub          string `json:"buyerPub"`
	TxID              string `json:"txid"`
	PaymentBEEFHex    string `json:"paymentBeefHex"`
	OutputIndex       uint32 `json:"outputIndex,omitempty"`
	DerivationPrefix  string `json:"derivationPrefix,omitempty"`
	DerivationSuffix  string `json:"derivationSuffix,omitempty"`
	SenderIdentityKey string `json:"senderIdentityKey,omitempty"`
	EscrowMode        bool   `json:"escrowMode,omitempty"`
	DepositorPKH      string `json:"depositorPkh,omitempty"`
	EscrowSellerPKH   string `json:"escrowSellerPkh,omitempty"`
	EscrowRefundAfter uint64 `json:"escrowRefundAfter,omitempty"`
	EscrowMaxFee      uint64 `json:"escrowMaxFee,omitempty"`
	TS                int64  `json:"ts,omitempty"`
	BuyerSig          string `json:"buyerSig,omitempty"`
}

// Signable returns the canonical payload signed by BuyerPub to prove buyer ownership on purchase & key re-reveal.
func (r PurchaseRequest) Signable() map[string]any {
	return map[string]any{
		"action":   "market_purchase",
		"offerId":  r.OfferID,
		"wantId":   r.WantID,
		"buyerPub": r.BuyerPub,
		"txid":     r.TxID,
		"ts":       r.TS,
	}
}

// PurchaseResponse is returned by Seller after validating Buyer's AtomicBEEF payment & BRC-105 replay claim.
// When Status == "awaiting_confirmation", payment was SPV-verified & internalized but exceeds 0-conf caps
// (or settleDepth); ContentKeyHex is withheld until the tx mines at RequiredDepth and Seller delivers reveal_key.
type PurchaseResponse struct {
	OK             bool                   `json:"ok"`
	Status         string                 `json:"status,omitempty"` // settled | awaiting_confirmation
	RequiredDepth  int                    `json:"requiredDepth,omitempty"`
	OfferID        string                 `json:"offerId"`
	SellerPub      string                 `json:"sellerPub"`
	ContentKeyHex  string                 `json:"contentKeyHex,omitempty"`
	ClaimTxID      string                 `json:"claimTxid,omitempty"`
	ClaimTxBEEFHex string                 `json:"claimTxBeefHex,omitempty"`
	CiphertextHex  string                 `json:"ciphertextHex,omitempty"`
	ChirpURL       string                 `json:"chirpUrl,omitempty"`
	ProofBundle    *contracts.ProofBundle `json:"proofBundle,omitempty"`
	DealReceipt    *DealReceipt           `json:"dealReceipt,omitempty"`
	Error          string                 `json:"error,omitempty"`
}

type pendingConfirmEntry struct {
	OfferID       string `json:"offerId"`
	WantID        string `json:"wantId"`
	BuyerPub      string `json:"buyerPub"`
	TxID          string `json:"txid"`
	AmountSats    uint64 `json:"amountSats"`
	RequiredDepth int    `json:"requiredDepth"`
	BboxOffice    string `json:"bboxOffice,omitempty"`
	CreatedAt     int64  `json:"createdAt"`
}

type pendingSale struct {
	OfferID     string
	WantID      string
	BuyerPub    string
	Shard       string
	Seq         int64
	SaleOffer   *contracts.KeyedSaleOffer
	RevealKey   [32]byte
	ChirpURL    string
	ProofBundle *contracts.ProofBundle
	ExpiresAt   int64
	Settled     bool
}

// MarketTapeEvent is one verified live market flow event (want, offer, or deal)
// surfaced to the real-time dashboard SSE stream and /state snapshot.
type MarketTapeEvent struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"` // want | offer | deal
	Side    string   `json:"side"` // buy | sell | clear
	Domains []string `json:"domains,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Sats    uint64   `json:"sats"`
	Pub     string   `json:"pub,omitempty"`
	Shard   string   `json:"shard,omitempty"`
	Status  string   `json:"status,omitempty"`
	IsSelf  bool     `json:"isSelf,omitempty"`
	At      int64    `json:"at"` // unix ms
}

const (
	maxPendingSalesTotal    = 128
	maxPendingSalesPerBuyer = 16
	maxSeenBboxNonces       = 8192
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

	mu                   sync.RWMutex
	restate              *RestateClient
	matcher              Matcher
	pendingSales         map[string]*pendingSale
	pendingConfirmations map[string]*pendingConfirmEntry // txid -> over-cap payment waiting for settleDepth
	offerInbox           map[string]chan bstack.OfferAd
	bboxReplies          map[string]chan PurchaseResponse
	bboxExpected         map[string]string // offerID -> expected sellerPub
	bboxWantByOffer      map[string]string // offerID -> wantID (for Restate BuyerDeal/deliverKey wakeup)
	activeLoops          map[string]context.CancelFunc
	seenWants            map[string]int64 // wantID -> seenUnix (bounded)
	seenBboxNonces       map[string]int64 // sender:nonce -> seenUnix (bounded replay guard)
	packs                []bstack.PackRef
	bountyDoms           []string
	marketTape           []MarketTapeEvent
	tapeListeners        []func(event string, ev MarketTapeEvent)

	cursorMu sync.Mutex

	startMu    sync.Mutex
	started    bool
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
		restate:              cfg.RestateClient,
		matcher:              cfg.Matcher,
		pendingSales:         make(map[string]*pendingSale),
		pendingConfirmations: make(map[string]*pendingConfirmEntry),
		offerInbox:           make(map[string]chan bstack.OfferAd),
		bboxReplies:          make(map[string]chan PurchaseResponse),
		bboxExpected:         make(map[string]string),
		bboxWantByOffer:      make(map[string]string),
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

func (b *Broker) restorePersistedState() {
	if b.mem == nil || b.mem.DB() == nil {
		return
	}
	now := time.Now().Unix()
	_, _ = b.mem.DB().PruneExpiredPendingSales(now)
	if sales, err := b.mem.DB().ListPendingSales(now); err == nil {
		for _, r := range sales {
			if ps := b.pendingSaleFromRecord(context.Background(), r); ps != nil {
				b.pendingSales[r.OfferID] = ps
				// Re-allowlist CHIRP DAG hashes so buyers never hit HTTP 401 on /v1/chirp/{hash} after seller restart!
				if ps.ChirpURL != "" && b.mem.Objects() != nil {
					if hashes, hErr := bstack.CollectChirpHashes(b.mem.Objects(), ps.ChirpURL); hErr == nil && len(hashes) > 0 {
						b.mem.AllowPublicChirpHashes(hashes...)
					}
				}
			}
		}
	}
}

const marketKeyEncPrefix = "enc:"

// sealKeyAtRest encrypts a hex-encoded BRC-369 content key via BRC-43 wallet EncryptAtRest before SQLite persistence (F-28).
func (b *Broker) sealKeyAtRest(ctx context.Context, keyHex string) string {
	if keyHex == "" || strings.HasPrefix(keyHex, marketKeyEncPrefix) || b == nil || b.mem == nil || b.mem.Chain() == nil {
		return keyHex
	}
	if ct, err := b.mem.Chain().EncryptAtRest(ctx, "market-reveal-key", []byte(keyHex)); err == nil && len(ct) > 0 {
		return marketKeyEncPrefix + hex.EncodeToString(ct)
	}
	return keyHex
}

// openKeyAtRest unwraps a BRC-43 encrypted key ("enc:" prefix) or returns legacy plaintext hex.
func (b *Broker) openKeyAtRest(ctx context.Context, stored string) string {
	if !strings.HasPrefix(stored, marketKeyEncPrefix) || b == nil || b.mem == nil || b.mem.Chain() == nil {
		return stored
	}
	ct, err := hex.DecodeString(strings.TrimPrefix(stored, marketKeyEncPrefix))
	if err != nil {
		return ""
	}
	pt, err := b.mem.Chain().DecryptAtRest(ctx, "market-reveal-key", ct)
	if err != nil {
		return ""
	}
	res := string(pt)
	core.ZeroBytes(pt)
	return res
}

func (b *Broker) pendingSaleFromRecord(ctx context.Context, r store.PendingSaleRecord) *pendingSale {
	var so contracts.KeyedSaleOffer
	if err := json.Unmarshal(r.SaleOfferJSON, &so); err != nil {
		return nil
	}
	plainHex := b.openKeyAtRest(ctx, r.RevealKeyHex)
	rkBytes, err := hex.DecodeString(plainHex)
	if err != nil || len(rkBytes) != 32 {
		return nil
	}
	var rk [32]byte
	copy(rk[:], rkBytes)
	core.ZeroBytes(rkBytes)
	var pb *contracts.ProofBundle
	if len(r.ProofBundleJSON) > 0 {
		var tmp contracts.ProofBundle
		if json.Unmarshal(r.ProofBundleJSON, &tmp) == nil {
			pb = &tmp
		}
	}
	return &pendingSale{
		OfferID:     r.OfferID,
		WantID:      r.WantID,
		BuyerPub:    r.BuyerPub,
		Shard:       r.Shard,
		Seq:         r.Seq,
		SaleOffer:   &so,
		RevealKey:   rk,
		ChirpURL:    r.ChirpURL,
		ProofBundle: pb,
		ExpiresAt:   r.ExpiresAt,
		Settled:     r.Settled,
	}
}

func (b *Broker) Engine() *Engine               { return b.engine }
func (b *Broker) Discovery() *discovery.Service { return b.disc }
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
	b.stopCancel = cancel
	b.startMu.Unlock()

	b.ensureHostLoops(runCtx)
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
	b.stopCancel = nil
	b.startMu.Unlock()
	if cancel != nil {
		cancel()
	}
	b.mu.Lock()
	for h, c := range b.activeLoops {
		c()
		delete(b.activeLoops, h)
	}
	b.mu.Unlock()
	b.wg.Wait()
}

func (b *Broker) notify(event string, payload any) {
	if b.cfg.OnStateChange != nil {
		b.cfg.OnStateChange(event, payload)
	}
}

// OnMarketTape registers a live callback invoked whenever a verified want/offer/deal enters the market tape.
func (b *Broker) OnMarketTape(fn func(event string, ev MarketTapeEvent)) {
	if b == nil || fn == nil {
		return
	}
	b.mu.Lock()
	b.tapeListeners = append(b.tapeListeners, fn)
	b.mu.Unlock()
}

// RecordMarketTape appends a verified event to the bounded ring buffer and notifies live listeners.
func (b *Broker) RecordMarketTape(event string, ev MarketTapeEvent) {
	if b == nil {
		return
	}
	if ev.At == 0 {
		ev.At = time.Now().UnixMilli()
	}
	b.mu.Lock()
	b.marketTape = append(b.marketTape, ev)
	if len(b.marketTape) > maxMarketTapeEvents {
		b.marketTape = append([]MarketTapeEvent(nil), b.marketTape[len(b.marketTape)-maxMarketTapeEvents:]...)
	}
	listeners := make([]func(event string, ev MarketTapeEvent), len(b.tapeListeners))
	copy(listeners, b.tapeListeners)
	b.mu.Unlock()
	for _, fn := range listeners {
		fn(event, ev)
	}
}

// RecentMarketEvents returns up to limit newest verified market tape events (newest first).
func (b *Broker) RecentMarketEvents(limit int) []MarketTapeEvent {
	if b == nil {
		return nil
	}
	if limit <= 0 || limit > maxMarketTapeEvents {
		limit = maxMarketTapeEvents
	}
	b.mu.RLock()
	n := len(b.marketTape)
	if n == 0 {
		b.mu.RUnlock()
		return nil
	}
	count := min(n, limit)
	out := make([]MarketTapeEvent, 0, count)
	for i := n - 1; i >= n-count; i-- {
		out = append(out, b.marketTape[i])
	}
	b.mu.RUnlock()
	return out
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
	_, _ = crand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
