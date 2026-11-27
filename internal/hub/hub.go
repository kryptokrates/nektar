package hub

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"net/http"
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
}

// Node is a durable, self-contained Go overlay hub serving BRC-22 /submit, BRC-24 /lookup,
// numbered/cursored SSE /events, NAT-safe /bbox relay, /bounties, and Hub-to-Hub federation.
type Node struct {
	mu                sync.RWMutex
	admitMu           sync.Mutex // serializes AppendEvent -> broadcastEvent so SSE seq order is strictly monotonic
	feeCollectMu      sync.Mutex // serializes UnminedFastExposure checks with ClaimFeePaymentAtomic + RecordFeeLedger
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
	workHandler       WorkHandler
	workTasks         map[string]WorkTask
	workPerPubCapSats uint64
	workGlobalCapSats uint64
	fees              FeeSchedule
	feeCollector      FeeCollector
	prefixSecret      []byte
	spinePeers        map[string]bool
	upstreamSpines    []string
	upstreamSubmitter func(ctx context.Context, spineURL string, beef []byte, topics []string) error
	upstreamSem       chan struct{}
	egressAuthSigner  func(action, aud string, cursor int64, domainsCSV string) (pub, ts, nonce, sig string, err error)
	onEgressTopup     func(ctx context.Context, spineURL string, minTopupSats uint64) error
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

// Handler returns the HTTP handler exposing BRC-22 /submit, BRC-24 /lookup, numbered/cursored SSE /events,
// /sync federation feed, NAT-safe /bbox relay, /bounties, and /v1/health.
func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", n.handleHealth)
	mux.HandleFunc("GET /v1/terms", n.handleTerms)
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
	// Zero-sat verifiable Hub work bounties — lets fresh 0-sat nodes earn their initial BSV float!
	mux.HandleFunc("GET /work-bounties", n.handleWorkBountiesList)
	mux.HandleFunc("POST /work-bounties/submit", n.handleWorkBountiesSubmit)
	return mux
}
