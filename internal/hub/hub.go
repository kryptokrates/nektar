package hub

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	bboxmod "github.com/lightwebinc/bbox/module"
	"github.com/lightwebinc/bcommon/module"
	bfingermod "github.com/lightwebinc/bfinger/module"
	blogsmod "github.com/lightwebinc/blogs/module"
	borgmod "github.com/lightwebinc/borg/module"
	bsecretmod "github.com/lightwebinc/bsecret/module"
	bstoremod "github.com/lightwebinc/bstore/module"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
)

// BountySubmission is a signed agent submission selling verified knowledge to the Hub bounty pool.
type BountySubmission struct {
	BountyID      string          `json:"bountyId"`
	SellerPub     string          `json:"sellerPub"`
	ProvenancePub string          `json:"provenancePub,omitempty"` // BRC-43 provenance pubkey matching Provenance signature & OwnerSig
	SellerAddress string          `json:"sellerAddress"`
	Shard         string          `json:"shard"`
	Seq           int64           `json:"seq"`
	HDP           string          `json:"hdp"`
	Text          string          `json:"text"`
	ProvenanceHex string          `json:"provenanceHex,omitempty"`
	ProofBundle   json.RawMessage `json:"proofBundle,omitempty"`
	TS            int64           `json:"ts,omitempty"`
	Sig           string          `json:"sig,omitempty"`
	OwnerSig      string          `json:"ownerSig,omitempty"`
}

// Signable returns the canonical signable payload for BountySubmission (using canonical lowercase HDP).
func (s BountySubmission) Signable(canonicalHDP string) map[string]any {
	return map[string]any{
		"action":        "bounty_submit",
		"bountyId":      s.BountyID,
		"hdp":           canonicalHDP,
		"sellerPub":     s.SellerPub,
		"provenancePub": s.ProvenancePub,
		"sellerAddress": s.SellerAddress,
		"ts":            s.TS,
	}
}

// BountyPayoutResult reports the Hub's verified payment to a contributing agent.
// BEEFHex carries the strict AtomicBEEF payout transaction so recipients can SPV-verify and internalize immediately.
type BountyPayoutResult struct {
	Accepted   bool   `json:"accepted"`
	BountyID   string `json:"bountyId"`
	RewardSats uint64 `json:"rewardSats"`
	TxID       string `json:"txid,omitempty"`
	BEEFHex    string `json:"beefHex,omitempty"`
	Status     string `json:"status,omitempty"` // broadcast | paid | pending
	Shard      string `json:"shard,omitempty"`
	Seq        int64  `json:"seq,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// BountyHandler validates and pays an incoming BountySubmission using the Hub's memory + SpendGuard wallet.
type BountyHandler func(ctx context.Context, b Bounty, sub BountySubmission) (*BountyPayoutResult, error)

// TransportMode selects how Hub-to-Hub live propagation operates (aligned with 1BSV Papers 006/008/009/010).
type TransportMode string

const (
	// TransportRelay runs Spine as an Application-Layer BRC-149 Object Plane Relay (Paper 010 §8.2)
	// and Leafs with sibling gossip OFF (O(N) crossings instead of O(N^2), same wire format as 1BSV).
	// This is the default built-in Hub-to-Hub transport (zero mesh gossip fallback).
	TransportRelay TransportMode = "relay"
	// TransportPlane switches internal Hub propagation completely OFF so stock Apache-2.0
	// lightwebinc/overlay-bridge delivers BRC-149 objects & BRC-135 headers (Paper 008).
	TransportPlane TransportMode = "plane"
	// TransportUnicast disables live Hub-to-Hub propagation; publishers fan out via bcommon/unicast
	// and Hubs backfill offline gaps via bcommon/replay (Paper 009 §4.5).
	TransportUnicast TransportMode = "unicast"
)

// Config configures the embedded v0.1 Go Overlay Hub.
type Config struct {
	Network           string
	DBPath            string // durable SQLite path (empty -> ":memory:")
	EnableFinger      bool   // mounts lightwebinc/bfinger/module (tm_finger, ls_finger)
	EnableBbox        bool   // mounts lightwebinc/bbox/module when getenv configures BBOX_OFFICES
	EnableBlogs       bool   // mounts lightwebinc/blogs/module when getenv configures BLOGS_STREAMS
	EnableBorg        bool   // mounts lightwebinc/borg/module when getenv configures BORG_HALLS
	EnableBsecret     bool   // mounts lightwebinc/bsecret/module when getenv configures BSECRET_CRYPTS
	EnableBstore      bool   // mounts lightwebinc/bstore/module when getenv configures BSTORE_BUCKETS
	MaxAdmitsPerMin   int    // per-pubkey admission rate limit (default 120/min)
	OnBountySubmit    BountyHandler
	Getenv            func(string) string
	ChainTracker      chaintracker.ChainTracker
	HeaderHandler     http.Handler // optional sovereign Header Store serving /v1/tip, /v1/root/{height}, /v1/header/{hash}
	TransportMode     TransportMode
	PropagationOff    bool
	Fees              FeeSchedule
	FeeCollector      FeeCollector
	PrefixSecret      []byte
	UpstreamSpines    []string
	UpstreamSubmitter func(ctx context.Context, spineURL string, beef []byte, topics []string) error
	EgressAuthSigner  func(action, aud string, cursor int64, domainsCSV string) (pub, ts, nonce, sig string, err error)
	OnEgressTopup     func(ctx context.Context, spineURL string, minTopupSats uint64) error
	SubscribedDomains func() []string
}

type simpleHost struct {
	mu       sync.Mutex
	counters map[string]float64
	gauges   map[string]func() float64
}

func newSimpleHost() *simpleHost {
	return &simpleHost{
		counters: make(map[string]float64),
		gauges:   make(map[string]func() float64),
	}
}

func (h *simpleHost) Log(_ string, _ map[string]any) {}
func (h *simpleHost) Metrics() module.Metrics        { return h }
func (h *simpleHost) Inc(name string, _ map[string]string, by float64) {
	h.mu.Lock()
	h.counters[name] += by
	h.mu.Unlock()
}
func (h *simpleHost) Preset(name string, _ map[string]string) {
	h.mu.Lock()
	if _, ok := h.counters[name]; !ok {
		h.counters[name] = 0
	}
	h.mu.Unlock()
}
func (h *simpleHost) Gauge(name string, fn func() float64) {
	h.mu.Lock()
	h.gauges[name] = fn
	h.mu.Unlock()
}

type rateBucket struct {
	windowStart time.Time
	count       int
}

const (
	maxHubSSESubs   = 256
	maxBboxNonces   = 16384
	bboxSkewSeconds = 300
)

// Node is a durable, self-contained Go overlay hub serving BRC-22 /submit, BRC-24 /lookup,
// numbered/cursored SSE /events, NAT-safe /bbox relay, /bounties, and Hub-to-Hub federation.
type Node struct {
	mu                sync.RWMutex
	admitMu           sync.Mutex // serializes AppendEvent -> broadcastEvent so SSE seq order is strictly monotonic
	net               string
	host              *simpleHost
	store             *Store
	marketLS          *bstack.MarketLookupService
	topics            map[string]module.TopicManager
	lookups           map[string]module.LookupService
	subs              map[chan bstack.MarketEvent]map[string]bool
	rates             map[string]*rateBucket
	bboxNonces        map[string]int64
	maxPerMin         int
	tracker           chaintracker.ChainTracker
	headerHandler     http.Handler
	transportMode     TransportMode
	propagationOff    bool
	loopGuard         *LoopGuard
	planeSubs         map[chan PlaneFrame]map[[32]byte]bool
	bountyHandler     BountyHandler
	fees              FeeSchedule
	feeCollector      FeeCollector
	prefixSecret      []byte
	spinePeers        map[string]bool
	upstreamSpines    []string
	upstreamSubmitter func(ctx context.Context, spineURL string, beef []byte, topics []string) error
	upstreamSem       chan struct{}
	egressAuthSigner  func(action, aud string, cursor int64, domainsCSV string) (pub, ts, nonce, sig string, err error)
	onEgressTopup     func(ctx context.Context, spineURL string, minTopupSats uint64) error
	subscribedDomains func() []string
	fedStarted        map[string]bool
	httpClient        *http.Client
	sseClient         *http.Client
	stopCh            chan struct{}
	closeOnce         sync.Once
	sseStopCh         chan struct{}
	sseCloseOnce      sync.Once
	wg                sync.WaitGroup
}

// NewNode constructs an embedded durable Hub Node mounting Nektar's 16 market lanes + requested Lightweb modules.
func NewNode(cfg Config) (*Node, error) {
	net := cfg.Network
	if net == "" {
		net = "regtest"
	}
	if (net == "main" || net == "testnet") && cfg.ChainTracker == nil {
		return nil, fmt.Errorf("hub: witnessed ChainTracker is required on live network %q", net)
	}
	getenv := cfg.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	st, err := OpenStore(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	maxPerMin := cfg.MaxAdmitsPerMin
	if maxPerMin <= 0 {
		maxPerMin = 120
	}
	fees := cfg.Fees
	if !fees.HasAnyFees() && fees.Role == "" && fees.Preset == "" && len(fees.SpinePeerPubs) == 0 {
		fees = LoadFeeSchedule(getenv)
	}
	fees.Normalize()
	if fees.HasAnyFees() && cfg.FeeCollector == nil {
		_ = st.Close()
		return nil, errors.New("hub: FeeCollector wallet is required when hub fee schedule has non-zero prices")
	}
	prefixSecret := append([]byte(nil), cfg.PrefixSecret...)
	if len(prefixSecret) < 16 {
		sec, sErr := st.GetOrCreateKV("hub_prefix_secret", func() ([]byte, error) {
			b := make([]byte, 32)
			if _, rErr := crand.Read(b); rErr != nil {
				return nil, rErr
			}
			return b, nil
		})
		if sErr != nil {
			_ = st.Close()
			return nil, fmt.Errorf("hub: init prefix secret: %w", sErr)
		}
		prefixSecret = sec
	}
	spinePeers := make(map[string]bool, len(fees.SpinePeerPubs))
	for _, sp := range fees.SpinePeerPubs {
		if sp = strings.ToLower(strings.TrimSpace(sp)); sp != "" {
			spinePeers[sp] = true
		}
	}
	var upSpines []string
	seenSpine := make(map[string]bool)
	for _, u := range cfg.UpstreamSpines {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" && !seenSpine[u] {
			seenSpine[u] = true
			upSpines = append(upSpines, u)
		}
	}
	tMode := cfg.TransportMode
	if tMode == "" {
		switch strings.ToLower(strings.TrimSpace(getenv("NEKTAR_HUB_TRANSPORT"))) {
		case "plane":
			tMode = TransportPlane
		case "unicast":
			tMode = TransportUnicast
		default:
			tMode = TransportRelay
		}
	}
	propOff := cfg.PropagationOff || strings.EqualFold(strings.TrimSpace(getenv("NEKTAR_HUB_PROPAGATION")), "off") || tMode == TransportPlane || tMode == TransportUnicast

	n := &Node{
		net:               net,
		host:              newSimpleHost(),
		store:             st,
		topics:            make(map[string]module.TopicManager),
		lookups:           make(map[string]module.LookupService),
		subs:              make(map[chan bstack.MarketEvent]map[string]bool),
		planeSubs:         make(map[chan PlaneFrame]map[[32]byte]bool),
		rates:             make(map[string]*rateBucket),
		bboxNonces:        make(map[string]int64),
		maxPerMin:         maxPerMin,
		tracker:           cfg.ChainTracker,
		headerHandler:     cfg.HeaderHandler,
		transportMode:     tMode,
		propagationOff:    propOff,
		loopGuard:         NewLoopGuard(DefaultLoopGuardCap),
		bountyHandler:     cfg.OnBountySubmit,
		fees:              fees,
		feeCollector:      cfg.FeeCollector,
		prefixSecret:      prefixSecret,
		spinePeers:        spinePeers,
		upstreamSpines:    upSpines,
		upstreamSubmitter: cfg.UpstreamSubmitter,
		upstreamSem:       make(chan struct{}, 3),
		egressAuthSigner:  cfg.EgressAuthSigner,
		onEgressTopup:     cfg.OnEgressTopup,
		subscribedDomains: cfg.SubscribedDomains,
		fedStarted:        make(map[string]bool),
		httpClient:        core.SafeHTTPClientForNetwork(net, 20*time.Second),
		sseClient:         core.SafeHTTPClientForNetwork(net, 0),
		stopCh:            make(chan struct{}),
		sseStopCh:         make(chan struct{}),
	}

	// 1. Mount Nektar v0.1 market module (tm_nektar_<net>_0..f + ls_nektar) with durable SSE callback!
	nktMod, err := bstack.NewMarketModule(net, n.onMarketAdmit)(n.host)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("hub: mount nektar market: %w", err)
	}
	n.mountModule(nktMod)
	if ls, ok := nktMod.Lookups[bstack.LookupService].(*bstack.MarketLookupService); ok {
		n.marketLS = ls
	}

	// 2. Mount Lightweb bfinger (tm_finger + ls_finger).
	if cfg.EnableFinger {
		fm, err := bfingermod.Mount(n.host, []string{bfingermod.Topic})
		if err != nil {
			_ = st.Close()
			return nil, fmt.Errorf("hub: mount bfinger: %w", err)
		}
		n.mountModule(fm.Module)
	}

	// 3. Optionally mount bbox / blogs / borg / bsecret / bstore Go modules when configured via env.
	if cfg.EnableBbox && getenv("BBOX_OFFICES") != "" {
		m, err := bboxmod.New(n.host, getenv)
		if err != nil {
			_ = st.Close()
			return nil, fmt.Errorf("hub: mount bbox: %w", err)
		}
		n.mountModule(m.Module)
	}
	if cfg.EnableBlogs && getenv("BLOGS_STREAMS") != "" {
		m, err := blogsmod.New(n.host, getenv)
		if err != nil {
			_ = st.Close()
			return nil, fmt.Errorf("hub: mount blogs: %w", err)
		}
		n.mountModule(m.Module)
	}
	if cfg.EnableBorg && getenv("BORG_HALLS") != "" {
		m, err := borgmod.New(n.host, getenv)
		if err != nil {
			_ = st.Close()
			return nil, fmt.Errorf("hub: mount borg: %w", err)
		}
		n.mountModule(m.Module)
	}
	if cfg.EnableBsecret && getenv("BSECRET_CRYPTS") != "" {
		m, err := bsecretmod.New(n.host, getenv)
		if err != nil {
			_ = st.Close()
			return nil, fmt.Errorf("hub: mount bsecret: %w", err)
		}
		n.mountModule(m.Module)
	}
	if cfg.EnableBstore && getenv("BSTORE_BUCKETS") != "" {
		m, err := bstoremod.New(n.host, getenv)
		if err != nil {
			_ = st.Close()
			return nil, fmt.Errorf("hub: mount bstore: %w", err)
		}
		n.mountModule(m.Module)
	}

	// 4. Restore unexpired market events from SQLite into ls_nektar so restarts never lose state!
	if err := n.restoreFromStore(context.Background()); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("hub: restore store: %w", err)
	}

	// 5. Always-on background maintenance (expiry GC for hub_events + bbox TTL/row cap, even on standalone hubs).
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.maintenanceLoop()
	}()

	return n, nil
}

func (n *Node) maintenanceLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-n.stopCh:
			return
		case <-ticker.C:
			nowMs := time.Now().UnixMilli()
			_, _ = n.store.PruneExpired(nowMs)
			_, _ = n.store.PruneBbox(nowMs-int64(7*24*time.Hour/time.Millisecond), 10_000)
			if n.marketLS != nil {
				_ = n.marketLS.PruneExpired(nowMs)
			}
			// Evict stale per-pubkey rate-limit buckets.
			n.mu.Lock()
			now := time.Now()
			for pub, b := range n.rates {
				if now.Sub(b.windowStart) >= 2*time.Minute {
					delete(n.rates, pub)
				}
			}
			n.mu.Unlock()
		}
	}
}

// CloseSSE signals active /events SSE handlers to exit cleanly so http.Server.Shutdown does not stall.
func (n *Node) CloseSSE() {
	n.sseCloseOnce.Do(func() {
		if n.sseStopCh != nil {
			close(n.sseStopCh)
		}
	})
}

func (n *Node) Close() error {
	n.CloseSSE()
	n.closeOnce.Do(func() { close(n.stopCh) })
	n.wg.Wait()
	return n.store.Close()
}
func (n *Node) Store() *Store { return n.store }
func (n *Node) SetBountyHandler(h BountyHandler) {
	n.mu.Lock()
	n.bountyHandler = h
	n.mu.Unlock()
}

// SetUpstreamSubmitter configures the Leaf -> Spine upstream BEEF push function.
func (n *Node) SetUpstreamSubmitter(fn func(ctx context.Context, spineURL string, beef []byte, topics []string) error) {
	n.mu.Lock()
	n.upstreamSubmitter = fn
	n.mu.Unlock()
}

// SetUpstreamSpines registers Spine Hub URLs that a Leaf Hub forwards local ads UP to.
func (n *Node) SetUpstreamSpines(spines []string) {
	n.mu.Lock()
	seen := make(map[string]bool)
	var clean []string
	for _, s := range spines {
		if s = strings.TrimRight(strings.TrimSpace(s), "/"); s != "" && !seen[s] {
			seen[s] = true
			clean = append(clean, s)
		}
	}
	n.upstreamSpines = clean
	n.mu.Unlock()
}

// SetEgressAuth wires outgoing Spine-and-Leaf egress authentication, credit auto-topup, and interest signaling.
func (n *Node) SetEgressAuth(
	signer func(action, aud string, cursor int64, domainsCSV string) (pub, ts, nonce, sig string, err error),
	topup func(ctx context.Context, spineURL string, minTopupSats uint64) error,
	domainsFn func() []string,
) {
	n.mu.Lock()
	n.egressAuthSigner = signer
	n.onEgressTopup = topup
	n.subscribedDomains = domainsFn
	n.mu.Unlock()
}

// AddSpinePeerPub whitelists a bilateral Spine peer's compressed pubkey for settlement-free peering.
func (n *Node) AddSpinePeerPub(pubHex string) {
	if pubHex = strings.ToLower(strings.TrimSpace(pubHex)); pubHex != "" {
		n.mu.Lock()
		n.spinePeers[pubHex] = true
		n.mu.Unlock()
	}
}

// OnBountyPayoutMined promotes a broadcast bounty claim to 'paid' once its payout tx mines at settleDepth.
func (n *Node) OnBountyPayoutMined(txid string) error {
	return n.store.FinalizeBountyClaimByTxID(txid)
}

func (n *Node) mountModule(m module.Module) {
	for k, tm := range m.Topics {
		n.topics[k] = tm
	}
	for k, ls := range m.Lookups {
		n.lookups[k] = ls
	}
}

func (n *Node) Topics() []string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make([]string, 0, len(n.topics))
	for k := range n.topics {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (n *Node) Lookups() []string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make([]string, 0, len(n.lookups))
	for k := range n.lookups {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (n *Node) allowPubRate(pub string) bool {
	if pub == "" {
		return true
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	if len(n.rates) > 4096 {
		for k, v := range n.rates {
			if now.Sub(v.windowStart) >= time.Minute {
				delete(n.rates, k)
			}
		}
	}
	b, ok := n.rates[pub]
	if !ok || now.Sub(b.windowStart) >= time.Minute {
		n.rates[pub] = &rateBucket{windowStart: now, count: 1}
		return true
	}
	if b.count >= n.maxPerMin {
		return false
	}
	b.count++
	return true
}

func (n *Node) claimBboxNonce(scope, nonce string, now int64) bool {
	if strings.TrimSpace(nonce) == "" {
		return false
	}
	key := scope + ":" + nonce
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.bboxNonces) >= maxBboxNonces {
		cutoff := now - bboxSkewSeconds*2
		for k, ts := range n.bboxNonces {
			if ts < cutoff {
				delete(n.bboxNonces, k)
			}
		}
		if len(n.bboxNonces) >= maxBboxNonces {
			return false
		}
	}
	if _, exists := n.bboxNonces[key]; exists {
		return false
	}
	n.bboxNonces[key] = now
	return true
}

// Handler returns the HTTP handler exposing BRC-22 /submit, BRC-24 /lookup, numbered/cursored SSE /events,
// /sync federation feed, NAT-safe /bbox relay, /bounties, and /v1/health.
func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", n.handleHealth)
	mux.HandleFunc("GET /v1/terms", n.handleTerms)
	mux.HandleFunc("GET /ls_nektar/terms", n.handleTerms)
	// Standard BRC-22 / BRC-24 engine metadata routes (required by lightwebinc/overlay-bridge -topics-from engine).
	mux.HandleFunc("GET /listTopicManagers", n.handleListTopicManagers)
	mux.HandleFunc("GET /listLookupServiceProviders", n.handleListLookupServiceProviders)
	// Optional Sovereign Header Store (1BSV Paper 008 §4: /v1/tip, /v1/root/{height}, /v1/header/{hash}).
	if n.headerHandler != nil {
		mux.Handle("GET /v1/tip", n.headerHandler)
		mux.Handle("GET /v1/root/{height}", n.headerHandler)
		mux.Handle("GET /v1/header/{hash}", n.headerHandler)
	}
	mux.HandleFunc("POST /v1/credits/topup", n.handleCreditsTopup)
	mux.HandleFunc("GET /v1/credits/balance", n.handleCreditsBalance)
	mux.HandleFunc("GET /v1/hub/earnings", n.handleEarnings)
	mux.HandleFunc("POST /submit", n.handleSubmit)
	mux.HandleFunc("POST /lookup", n.handleLookup)
	// Application-Layer BRC-149 Object Plane Relay (1BSV Paper 006 §4.2 & Paper 010 §8.2).
	mux.HandleFunc("GET /plane/stream", n.handlePlaneStream)
	mux.HandleFunc("POST /plane/publish", n.handlePlanePublish)
	mux.HandleFunc("GET /plane/nack", n.handlePlaneNACK)
	// Numbered /sync feed for Hub-to-Hub federation & cursor gap repair (Valve 2: metered egress).
	mux.HandleFunc("GET /sync", n.handleSync)
	// Native numbered/cursored SSE /events?cursor=<seq>&domains=<csv> (Valve 2: metered egress with breaker!).
	mux.HandleFunc("GET /events", n.handleEvents)
	// Authenticated NAT-safe counterparty store-and-forward relay (/bbox/send & /bbox/inbox) (Valve 3: unbypassable toll).
	mux.HandleFunc("POST /bbox/send", n.handleBboxSend)
	mux.HandleFunc("GET /bbox/inbox", n.handleBboxInbox)
	// Hub knowledge bounties — lets organic agents sell verified domain facts directly TO the Hub!
	mux.HandleFunc("GET /bounties", n.handleBountiesList)
	mux.HandleFunc("POST /bounties/submit", n.handleBountiesSubmit)
	return mux
}
