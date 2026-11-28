package hub

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-overlay-services/pkg/core/engine"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/lightwebinc/bbox/boxrec"
	bboxmod "github.com/lightwebinc/bbox/module"
	"github.com/lightwebinc/bcommon/module"
	bdnsmod "github.com/lightwebinc/bdns/module"
	bfingermod "github.com/lightwebinc/bfinger/module"
	blogsmod "github.com/lightwebinc/blogs/module"
	borgmod "github.com/lightwebinc/borg/module"
	bsecretmod "github.com/lightwebinc/bsecret/module"
	bstoremod "github.com/lightwebinc/bstore/module"
	"github.com/lightwebinc/bstore/storerec"
	oespv "github.com/lightwebinc/overlay-bridge/pkg/spv"
	"github.com/lightwebinc/shard-common/interest"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/eventbus"
	"github.com/kryptokrates/nektar/internal/fabric"
	"github.com/kryptokrates/nektar/internal/faststore"
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
	DBPath            string        // durable SQLite path (empty -> ":memory:")
	FastStore         *faststore.DB // optional Pebble LSM store for lock-free 16-lane event & BEEF storage
	EnableFastStore   bool          // auto-open Pebble LSM store (in-memory or <dir>/hub.pebble) for hot-path events
	Lanes             []string      // optional hex nibble subset (e.g. ["0","1","2","3"]); empty mounts all 0..f
	EnableFinger      bool     // mounts lightwebinc/bfinger/module (tm_finger, ls_finger)
	EnableBbox        bool   // mounts lightwebinc/bbox/module (zero-config default office when BBOX_OFFICES unset)
	EnableBlogs       bool   // mounts lightwebinc/blogs/module when getenv configures BLOGS_STREAMS
	EnableBorg        bool   // mounts lightwebinc/borg/module when getenv configures BORG_HALLS
	EnableBsecret     bool   // mounts lightwebinc/bsecret/module when getenv configures BSECRET_CRYPTS
	EnableBstore      bool   // mounts lightwebinc/bstore/module (zero-config default bucket when BSTORE_BUCKETS unset)
	EnableBdns        bool   // mounts lightwebinc/bdns/module when getenv configures BDNS_ZONES
	EnableFabric      bool   // starts embedded BRC-223 topic interest receiver + BRC-149 TCP distributor
	FabricUDPListen   string // optional UDP address for BRC-223 interest datagrams (default "127.0.0.1:0")
	EventBus          *eventbus.Bus
	HandleResolver    http.HandlerFunc
	MaxAdmitsPerMin   int // per-pubkey admission rate limit (default 120/min)
	OnBountySubmit    BountyHandler
	Getenv            func(string) string
	ChainTracker      chaintracker.ChainTracker
	HeaderHandler     http.Handler // optional sovereign Header Store serving /v1/tip, /v1/root/{height}, /v1/header/{hash}
	TransportMode        TransportMode
	PropagationOff       bool
	DemandDrivenUpstream bool // Leaf subscribes to Spine strictly for the union of active downstream topics
	Fees                 FeeSchedule
	FeeCollector         FeeCollector
	PrefixSecret         []byte
	UpstreamSpines       []string
	UpstreamSubmitter    func(ctx context.Context, spineURL string, beef []byte, topics []string) error
	EgressAuthSigner     func(action, aud string, cursor int64, domainsCSV string) (pub, ts, nonce, sig string, err error)
	OnEgressTopup        func(ctx context.Context, spineURL string, minTopupSats uint64) error
}

// Node is a durable, self-contained Go overlay hub serving BRC-22 /submit, BRC-24 /lookup,
// BRC-180 /manifest.json, BRC-223 topic interest fabric, numbered/cursored SSE /events,
// native Lightweb bbox / bfinger / bstore modules, /bounties, and Hub-to-Hub federation.
type Node struct {
	mu                   sync.RWMutex
	admitMu              sync.Mutex     // legacy fallback lock for non-lane topics
	laneAdmitMu          [16]sync.Mutex // 16 per-lane admit locks so all 16 nibble lanes admit in parallel
	feeCollectMu         sync.Mutex     // serializes UnminedFastExposure checks with ClaimFeePaymentAtomic + RecordFeeLedger
	downInterestMu       sync.Mutex
	downTopicRefs        map[string]int
	downInterestWake     chan struct{}
	demandDrivenUpstream bool
	net                  string
	host                *simpleHost
	store               *Store
	objects             bstack.ObjectStore
	oeStore             *OverlayStore
	oeEngine            *engine.Engine
	spvVerifier         *oespv.Verifier
	fabricSrv           *fabric.HubInterestServer
	eventBus            *eventbus.Bus
	handleResolver      http.HandlerFunc
	defaultBboxOffice   string
	defaultBstoreBucket string
	modClosers          []io.Closer
	modOutputs          map[string]modOutputEntry
	modOutOrder         []string
	poisonSkipped       atomic.Uint64
	marketLS            *bstack.MarketLookupService
	topics              map[string]module.TopicManager
	lookups             map[string]module.LookupService
	subs                map[*sseSubscriber]map[string]bool
	subsByPub           map[string]map[*sseSubscriber]struct{}
	sseBufSize          int
	rates               map[string]*rateBucket
	bboxNonces          map[string]int64
	rateShards          [64]hubRateNonceShard
	maxPerMin           int
	tracker             chaintracker.ChainTracker
	headerHandler       http.Handler
	transportMode       TransportMode
	propagationOff      bool
	loopGuard           *LoopGuard
	planeSubs           map[chan PlaneFrame]map[[32]byte]bool
	bountyHandler       BountyHandler
	workHandler         WorkHandler
	workTasks           map[string]WorkTask
	workPerPubCapSats   uint64
	workGlobalCapSats   uint64
	fees                FeeSchedule
	feeCollector        FeeCollector
	prefixSecret        []byte
	spinePeers          map[string]bool
	upstreamSpines      []string
	upstreamSubmitter   func(ctx context.Context, spineURL string, beef []byte, topics []string) error
	upstreamSem         chan struct{}
	egressAuthSigner    func(action, aud string, cursor int64, domainsCSV string) (pub, ts, nonce, sig string, err error)
	onEgressTopup       func(ctx context.Context, spineURL string, minTopupSats uint64) error
	fedStarted          map[string]bool
	httpClient          *http.Client
	sseClient           *http.Client
	stopCh              chan struct{}
	closeOnce           sync.Once
	sseStopCh           chan struct{}
	sseCloseOnce        sync.Once
	wg                  sync.WaitGroup
}

// NewNode constructs an embedded durable Hub Node mounting Nektar's market lanes + Lightweb Go modules.
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
	if cfg.FastStore != nil {
		st.SetFastStore(cfg.FastStore, false)
	} else if cfg.EnableFastStore || getenv("NEKTAR_FAST_HUB") == "1" {
		var fsDir string
		if cfg.DBPath != "" && cfg.DBPath != ":memory:" {
			fsDir = filepath.Join(filepath.Dir(cfg.DBPath), "hub.pebble")
		}
		if fs, fErr := faststore.Open(faststore.Options{
			Dir:        fsDir,
			InMemory:   fsDir == "",
			SyncWrites: fsDir != "",
		}); fErr == nil {
			st.SetFastStore(fs, true)
		}
	}
	maxPerMin := cfg.MaxAdmitsPerMin
	if maxPerMin <= 0 {
		maxPerMin = 60_000
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

	demandUp := cfg.DemandDrivenUpstream || strings.EqualFold(strings.TrimSpace(getenv("NEKTAR_HUB_DEMAND_UPSTREAM")), "1")

	n := &Node{
		net:                  net,
		host:                 newSimpleHost(),
		store:                st,
		eventBus:             cfg.EventBus,
		handleResolver:       cfg.HandleResolver,
		downTopicRefs:        make(map[string]int),
		downInterestWake:     make(chan struct{}, 1),
		demandDrivenUpstream: demandUp,
		modOutputs:           make(map[string]modOutputEntry),
		topics:               make(map[string]module.TopicManager),
		lookups:              make(map[string]module.LookupService),
		subs:                 make(map[*sseSubscriber]map[string]bool),
		subsByPub:            make(map[string]map[*sseSubscriber]struct{}),
		planeSubs:            make(map[chan PlaneFrame]map[[32]byte]bool),
		rates:                make(map[string]*rateBucket),
		bboxNonces:           make(map[string]int64),
		maxPerMin:            maxPerMin,
		tracker:              cfg.ChainTracker,
		headerHandler:        cfg.HeaderHandler,
		transportMode:        tMode,
		propagationOff:       propOff,
		loopGuard:            NewLoopGuard(DefaultLoopGuardCap),
		bountyHandler:        cfg.OnBountySubmit,
		fees:                 fees,
		feeCollector:         cfg.FeeCollector,
		prefixSecret:         prefixSecret,
		spinePeers:           spinePeers,
		upstreamSpines:       upSpines,
		upstreamSubmitter:    cfg.UpstreamSubmitter,
		upstreamSem:          make(chan struct{}, 64),
		egressAuthSigner:     cfg.EgressAuthSigner,
		onEgressTopup:        cfg.OnEgressTopup,
		fedStarted:           make(map[string]bool),
		httpClient:           core.SafeHTTPClientForNetwork(net, 20*time.Second),
		sseClient:            core.SafeHTTPClientForNetwork(net, 0),
		stopCh:               make(chan struct{}),
		sseStopCh:            make(chan struct{}),
	}

	cleanupFail := func(err error) (*Node, error) {
		for _, c := range n.modClosers {
			_ = c.Close()
		}
		if n.fabricSrv != nil {
			_ = n.fabricSrv.Close()
		}
		if n.oeStore != nil {
			_ = n.oeStore.Close()
		}
		_ = st.Close()
		return nil, err
	}

	if err := n.initOverlayEngine(context.Background(), cfg.DBPath); err != nil {
		return cleanupFail(err)
	}

	isDisk := cfg.DBPath != "" && cfg.DBPath != ":memory:"
	var stateBaseDir string
	if isDisk {
		absDB, aErr := filepath.Abs(cfg.DBPath)
		if aErr != nil {
			return cleanupFail(aErr)
		}
		stateBaseDir = filepath.Join(filepath.Dir(absDB), strings.TrimSuffix(filepath.Base(absDB), filepath.Ext(absDB))+"_modules")
	}

	var objStore bstack.ObjectStore
	if isDisk {
		if fos, oErr := bstack.NewFileObjectStore(stateBaseDir); oErr == nil {
			objStore = fos
		}
	}
	if objStore == nil {
		objStore = bstack.NewMemoryObjectStore()
	}
	n.objects = objStore

	// 1. Mount Nektar v0.1 market module (tm_nektar_<net>_0..f or configured lane subset + ls_nektar) with durable SSE callback!
	lanes := append([]string(nil), cfg.Lanes...)
	if len(lanes) == 0 {
		if rawLanes := strings.TrimSpace(getenv("NEKTAR_HUB_LANES")); rawLanes != "" {
			lanes = []string{rawLanes}
		}
	}
	nktMod, err := bstack.NewMarketModuleWithLanes(net, lanes, n.onMarketAdmit)(n.host)
	if err != nil {
		return cleanupFail(fmt.Errorf("hub: mount nektar market: %w", err))
	}
	n.mountModule(nktMod)
	if ls, ok := nktMod.Lookups[bstack.LookupService].(*bstack.MarketLookupService); ok {
		n.marketLS = ls
	}

	// 2. Mount Lightweb bfinger (tm_finger + ls_finger).
	if cfg.EnableFinger {
		fm, err := bfingermod.Mount(n.host, []string{bfingermod.Topic})
		if err != nil {
			return cleanupFail(fmt.Errorf("hub: mount bfinger: %w", err))
		}
		n.mountModule(fm.Module)
	}

	// 3. Mount Lightweb bbox (explicit BBOX_OFFICES or deterministic network-wide office `nektar_nektarbbox`).
	if cfg.EnableBbox {
		if rawOffices := strings.TrimSpace(getenv("BBOX_OFFICES")); rawOffices != "" {
			m, err := bboxmod.New(n.host, getenv)
			if err != nil {
				return cleanupFail(fmt.Errorf("hub: mount bbox: %w", err))
			}
			n.mountModule(m.Module)
			n.modClosers = append(n.modClosers, m)
			if offices, pErr := bboxmod.ParseOffices(rawOffices); pErr == nil && len(offices) > 0 {
				n.defaultBboxOffice = offices[0]
			}
		} else {
			office := bstack.NormalizeBboxOffice("nektar")
			bCfg := &bboxmod.Config{
				Offices:       []string{office},
				RetentionDays: bboxmod.FloorDays,
				MaxBEEF:       boxrec.DefaultMaxBEEF,
				JournalSync:   bboxmod.DefaultJournalSyncMs * time.Millisecond,
			}
			var jrn bboxmod.Journal
			if isDisk {
				bCfg.StateDir = filepath.Join(stateBaseDir, "bbox")
			} else {
				jrn = &bboxmod.MemoryJournal{}
			}
			m, err := bboxmod.Mount(n.host, bCfg, nil, jrn, n.tracker, nil)
			if err != nil {
				return cleanupFail(fmt.Errorf("hub: mount default bbox: %w", err))
			}
			n.mountModule(m.Module)
			n.modClosers = append(n.modClosers, m)
			n.defaultBboxOffice = office
		}
	}

	// 4. Mount Lightweb blogs / borg / bsecret when configured via env.
	if cfg.EnableBlogs && getenv("BLOGS_STREAMS") != "" {
		m, err := blogsmod.New(n.host, getenv)
		if err != nil {
			return cleanupFail(fmt.Errorf("hub: mount blogs: %w", err))
		}
		n.mountModule(m.Module)
	}
	if cfg.EnableBorg && getenv("BORG_HALLS") != "" {
		m, err := borgmod.New(n.host, getenv)
		if err != nil {
			return cleanupFail(fmt.Errorf("hub: mount borg: %w", err))
		}
		n.mountModule(m.Module)
	}
	if cfg.EnableBsecret && getenv("BSECRET_CRYPTS") != "" {
		m, err := bsecretmod.New(n.host, getenv)
		if err != nil {
			return cleanupFail(fmt.Errorf("hub: mount bsecret: %w", err))
		}
		n.mountModule(m.Module)
	}

	// 5. Mount Lightweb bstore (explicit BSTORE_BUCKETS or deterministic network-wide bucket `nektar_nektarstor`).
	if cfg.EnableBstore {
		if rawBuckets := strings.TrimSpace(getenv("BSTORE_BUCKETS")); rawBuckets != "" {
			m, err := bstoremod.New(n.host, getenv)
			if err != nil {
				return cleanupFail(fmt.Errorf("hub: mount bstore: %w", err))
			}
			n.mountModule(m.Module)
			n.modClosers = append(n.modClosers, m)
			if buckets, pErr := bstoremod.ParseBuckets(rawBuckets); pErr == nil && len(buckets) > 0 {
				n.defaultBstoreBucket = buckets[0]
			}
		} else {
			bucket := bstack.NormalizeBstoreBucket("nektar")
			bsCfg := &bstoremod.Config{
				Buckets:        []string{bucket},
				KeepDays:       bstoremod.FloorKeepDays,
				PendingDays:    bstoremod.FloorPendingDays,
				OtherHours:     bstoremod.FloorOtherHours,
				MaxBEEF:        storerec.DefaultMaxBEEF,
				MaxSweepInputs: storerec.DefaultMaxSweepInputs,
				JournalSync:    bstoremod.DefaultJournalSyncMs * time.Millisecond,
			}
			var bsJrn bstoremod.Journal
			if isDisk {
				bsCfg.StateDir = filepath.Join(stateBaseDir, "bstore")
			} else {
				bsJrn = &bstoremod.MemoryJournal{}
			}
			hdrs := n.tracker
			if hdrs == nil {
				hdrs = noopChainTracker{}
			}
			m, err := bstoremod.Mount(n.host, bsCfg, nil, bsJrn, hdrs, nil)
			if err != nil {
				return cleanupFail(fmt.Errorf("hub: mount default bstore: %w", err))
			}
			n.mountModule(m.Module)
			n.modClosers = append(n.modClosers, m)
			n.defaultBstoreBucket = bucket
		}
	}

	// 6. Mount Lightweb bdns when configured via BDNS_ZONES.
	if cfg.EnableBdns && strings.TrimSpace(getenv("BDNS_ZONES")) != "" {
		m, err := bdnsmod.New(n.host, getenv)
		if err != nil {
			return cleanupFail(fmt.Errorf("hub: mount bdns: %w", err))
		}
		n.mountModule(m.Module)
	}

	// 7. Restore Lightweb module lookup indexes & unexpired market events from SQLite / overlay store!
	if err := n.restoreOverlayModules(context.Background()); err != nil {
		return cleanupFail(err)
	}
	if err := n.restoreFromStore(context.Background()); err != nil {
		return cleanupFail(fmt.Errorf("hub: restore store: %w", err))
	}

	// 8. Optionally start embedded BRC-223 topic interest receiver + BRC-149 TCP distributor.
	udpListen := strings.TrimSpace(cfg.FabricUDPListen)
	if udpListen == "" {
		udpListen = strings.TrimSpace(getenv("NEKTAR_HUB_BRC223_UDP"))
	}
	if cfg.EnableFabric || udpListen != "" {
		var fabMu sync.Mutex
		fabReleases := make(map[[32]byte]func())
		fSrv, fErr := fabric.StartHubInterestServer(context.Background(), fabric.HubConfig{
			UDPListen: udpListen,
			OnTopicInterest: func(topic interest.TopicID, live bool) {
				tid := [32]byte(topic)
				tName := n.topicNameForID(tid)
				if tName == "" {
					return
				}
				fabMu.Lock()
				if live {
					if _, exists := fabReleases[tid]; !exists {
						fabReleases[tid] = n.AcquireDownstreamTopics([]string{tName})
					}
				} else if rel, exists := fabReleases[tid]; exists {
					delete(fabReleases, tid)
					fabMu.Unlock()
					rel()
					return
				}
				fabMu.Unlock()
			},
		})
		if fErr != nil {
			return cleanupFail(fmt.Errorf("hub: start BRC-223 fabric: %w", fErr))
		}
		n.fabricSrv = fSrv
	}

	// 9. Always-on background maintenance (expiry GC + fast-fee unsettleable sweep).
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
	if n.fabricSrv != nil {
		_ = n.fabricSrv.Close()
	}
	for _, c := range n.modClosers {
		_ = c.Close()
	}
	if n.oeStore != nil {
		_ = n.oeStore.Close()
	}
	return n.store.Close()
}

// MarketLookup returns the mounted ls_nektar lookup service (if initialized) for local ticker and index queries.
func (n *Node) MarketLookup() *bstack.MarketLookupService {
	if n == nil {
		return nil
	}
	return n.marketLS
}

// Handler returns the HTTP handler exposing BRC-22 /submit, BRC-24 /lookup, BRC-180 /manifest.json,
// GASP foreign-node sync, numbered/cursored SSE /events, /sync, /bbox, /bounties, and /v1/health.
func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", n.handleHealth)
	mux.HandleFunc("GET /v1/terms", n.handleTerms)
	// Standard BRC-180 + BRC-68 + BRC-169 manifest & handle discovery routes.
	mux.HandleFunc("GET /manifest.json", n.handleManifestJSON)
	mux.HandleFunc("GET /hub/manifest.json", n.handleManifestJSON)
	mux.HandleFunc("GET /.well-known/metanet-handles/resolve", n.handleWellKnownResolve)
	// Standard BRC-22 / BRC-24 / GASP engine routes (compatible with lightwebinc/overlay-bridge).
	mux.HandleFunc("GET /listTopicManagers", n.handleListTopicManagers)
	mux.HandleFunc("GET /listLookupServiceProviders", n.handleListLookupServiceProviders)
	mux.HandleFunc("POST /requestForeignGASPNode", n.handleRequestForeignGASP)
	mux.HandleFunc("POST /provideForeignGASPNode", n.handleProvideForeignGASP)
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
	// Content-addressed BRC-167 CHIRP / bstore object staging for >12 KiB encrypted capsules & NAT Sellers.
	mux.HandleFunc("PUT /chirp/v1/objects/{hash}", n.handleChirpPut)
	mux.HandleFunc("GET /chirp/v1/objects/{hash}", n.handleChirpGet)
	mux.HandleFunc("HEAD /chirp/v1/objects/{hash}", n.handleChirpGet)
	mux.HandleFunc("PUT /v1/chirp/{hash}", n.handleChirpPut)
	mux.HandleFunc("GET /v1/chirp/{hash}", n.handleChirpGet)
	mux.HandleFunc("HEAD /v1/chirp/{hash}", n.handleChirpGet)
	// Hub knowledge bounties — lets organic agents sell verified domain facts directly TO the Hub!
	mux.HandleFunc("GET /bounties", n.handleBountiesList)
	mux.HandleFunc("POST /bounties/submit", n.handleBountiesSubmit)
	// Zero-sat verifiable Hub work bounties — lets fresh 0-sat nodes earn their initial BSV float!
	mux.HandleFunc("GET /work-bounties", n.handleWorkBountiesList)
	mux.HandleFunc("POST /work-bounties/submit", n.handleWorkBountiesSubmit)
	return mux
}
