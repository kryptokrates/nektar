package hub

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	bboxmod "github.com/lightwebinc/bbox/module"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/module"
	"github.com/lightwebinc/bcommon/verify"
	bfingermod "github.com/lightwebinc/bfinger/module"
	blogsmod "github.com/lightwebinc/blogs/module"
	borgmod "github.com/lightwebinc/borg/module"
	bsecretmod "github.com/lightwebinc/bsecret/module"
	bstoremod "github.com/lightwebinc/bstore/module"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
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

// Config configures the embedded v0.4 Go Overlay Hub.
type Config struct {
	Network          string
	DBPath           string // durable SQLite path (empty -> ":memory:")
	EnableFinger     bool   // mounts lightwebinc/bfinger/module (tm_finger, ls_finger)
	EnableBbox       bool   // mounts lightwebinc/bbox/module when getenv configures BBOX_OFFICES
	EnableBlogs      bool   // mounts lightwebinc/blogs/module when getenv configures BLOGS_STREAMS
	EnableBorg       bool   // mounts lightwebinc/borg/module when getenv configures BORG_HALLS
	EnableBsecret    bool   // mounts lightwebinc/bsecret/module when getenv configures BSECRET_CRYPTS
	EnableBstore     bool   // mounts lightwebinc/bstore/module when getenv configures BSTORE_BUCKETS
	MaxAdmitsPerMin  int    // per-pubkey admission rate limit (default 120/min)
	PeerHubs         []string
	OnBountySubmit   BountyHandler
	Getenv           func(string) string
	ChainTracker     chaintracker.ChainTracker
	Fees             FeeSchedule
	FeeCollector     FeeCollector
	PrefixSecret     []byte
	UpstreamSpines   []string
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
	bountyHandler     BountyHandler
	fees              FeeSchedule
	feeCollector      FeeCollector
	prefixSecret      []byte
	spinePeers        map[string]bool
	upstreamSpines    []string
	upstreamSubmitter func(ctx context.Context, spineURL string, beef []byte, topics []string) error
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
	n := &Node{
		net:               net,
		host:              newSimpleHost(),
		store:             st,
		topics:            make(map[string]module.TopicManager),
		lookups:           make(map[string]module.LookupService),
		subs:              make(map[chan bstack.MarketEvent]map[string]bool),
		rates:             make(map[string]*rateBucket),
		bboxNonces:        make(map[string]int64),
		maxPerMin:         maxPerMin,
		tracker:           cfg.ChainTracker,
		bountyHandler:     cfg.OnBountySubmit,
		fees:              fees,
		feeCollector:      cfg.FeeCollector,
		prefixSecret:      prefixSecret,
		spinePeers:        spinePeers,
		upstreamSpines:    upSpines,
		upstreamSubmitter: cfg.UpstreamSubmitter,
		egressAuthSigner:  cfg.EgressAuthSigner,
		onEgressTopup:     cfg.OnEgressTopup,
		subscribedDomains: cfg.SubscribedDomains,
		fedStarted:        make(map[string]bool),
		httpClient:        core.SafeHTTPClientForNetwork(net, 20*time.Second),
		sseClient:         core.SafeHTTPClientForNetwork(net, 0),
		stopCh:            make(chan struct{}),
		sseStopCh:         make(chan struct{}),
	}

	// 1. Mount Nektar v0.4 market module (tm_nektar_<net>_0..f + ls_nektar) with durable SSE callback!
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
func (n *Node) Store() *Store          { return n.store }
func (n *Node) SetBountyHandler(h BountyHandler) {
	n.mu.Lock()
	n.bountyHandler = h
	n.mu.Unlock()
}

func (n *Node) restoreFromStore(ctx context.Context) error {
	if n.marketLS == nil {
		return nil
	}
	isLive := n.net == "main" || n.net == "testnet"
	var cursor int64
	for {
		events, err := n.store.EventsSince(cursor, nil, 1000)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			if ev.Seq > cursor {
				cursor = ev.Seq
			}
			if len(ev.BEEF) == 0 {
				continue
			}
			var tx *transaction.Transaction
			if isLive {
				_, parsed, _, pErr := guard.ParseBEEF(ev.BEEF, guard.DefaultBound)
				if pErr != nil || parsed == nil || int(ev.Vout) >= len(parsed.Outputs) {
					continue
				}
				if n.tracker != nil {
					if verdict, _ := verify.Check(ctx, parsed, n.tracker); verdict != verify.Passed {
						continue
					}
				}
				tx = parsed
			} else {
				parsed, err := transaction.NewTransactionFromBEEF(ev.BEEF)
				if err != nil || parsed == nil || int(ev.Vout) >= len(parsed.Outputs) {
					continue
				}
				tx = parsed
			}
			out := tx.Outputs[ev.Vout]
			op := transaction.Outpoint{Txid: *tx.TxID(), Index: ev.Vout}
			_ = n.marketLS.OutputAdmittedByTopic(ctx, &module.OutputAdmittedByTopic{
				Mode:          module.AdmitLockingScript,
				Outpoint:      op,
				Topic:         ev.Topic,
				Satoshis:      out.Satoshis,
				LockingScript: out.LockingScript,
				AtomicBEEF:    ev.BEEF,
			})
			n.marketLS.SetSeq(op.String(), ev.Seq)
		}
		if len(events) < 1000 {
			break
		}
	}
	return nil
}

// HandleReorg evicts persisted Hub events anchored at block_height >= forkHeight and notifies
// every mounted LookupService via OutputEvicted so orphaned block state is rolled back cleanly.
func (n *Node) HandleReorg(ctx context.Context, forkHeight uint32) error {
	if forkHeight == 0 {
		return nil
	}
	evicted, err := n.store.EvictEventsFromHeight(forkHeight)
	if err != nil {
		return err
	}
	for _, ev := range evicted {
		h, hErr := chainhash.NewHashFromHex(ev.TxID)
		if hErr != nil || h == nil {
			continue
		}
		op := transaction.Outpoint{Txid: *h, Index: ev.Vout}
		for _, ls := range n.lookups {
			_ = ls.OutputEvicted(ctx, &op)
		}
	}
	return nil
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

func extractMeta(p bstack.MarketPayload) (pub string, domains []string, exp int64) {
	switch p.Kind {
	case bstack.KindWant:
		var sw bstack.SignedWant
		if json.Unmarshal(p.Body, &sw) == nil {
			return sw.Want.Pub, sw.Want.Domains, sw.Want.Exp
		}
	case bstack.KindOffer:
		var so bstack.SignedOffer
		if json.Unmarshal(p.Body, &so) == nil {
			return so.Offer.Pub, so.Offer.Domains, so.Offer.Exp
		}
	case bstack.KindAnnounce:
		var sa bstack.SignedAnnounce
		if json.Unmarshal(p.Body, &sa) == nil {
			return sa.Announce.Pub, append([]string{bstack.DirDomain}, sa.Announce.Domains...), sa.Announce.Exp
		}
	}
	return "", nil, 0
}

// onMarketAdmit is called by ls_nektar on admission; live fanout is triggered by Submit after durable seq assignment.
func (n *Node) onMarketAdmit(_ bstack.MarketEvent) {}

func (n *Node) broadcastEvent(ev bstack.MarketEvent, domains []string) {
	n.mu.RLock()
	for ch, filter := range n.subs {
		if len(filter) > 0 && !matchesAnyDomain(domains, filter) {
			continue
		}
		select {
		case ch <- ev:
		default:
		}
	}
	n.mu.RUnlock()
}

// Subscribe registers a channel receiving live admitted market events (optional domain filter).
func (n *Node) Subscribe(domains ...string) (<-chan bstack.MarketEvent, func()) {
	ch, unsub, _ := n.TrySubscribe(domains...)
	return ch, unsub
}

// TrySubscribe registers an SSE subscriber if below maxHubSSESubs.
func (n *Node) TrySubscribe(domains ...string) (<-chan bstack.MarketEvent, func(), bool) {
	ch := make(chan bstack.MarketEvent, 128)
	filter := make(map[string]bool)
	for _, d := range domains {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			filter[d] = true
		}
	}
	n.mu.Lock()
	if len(n.subs) >= maxHubSSESubs {
		n.mu.Unlock()
		return nil, func() {}, false
	}
	n.subs[ch] = filter
	n.mu.Unlock()
	return ch, func() {
		n.mu.Lock()
		delete(n.subs, ch)
		close(ch)
		n.mu.Unlock()
	}, true
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

// Submit processes a BRC-22 Atomic BEEF submission against requested topics, persists durably with a monotonic seq,
// notifies lookup services, and pushes numbered SSE events.
func (n *Node) Submit(ctx context.Context, rawBEEF []byte, topics []string) (overlay.Steak, error) {
	return n.submitInternal(ctx, rawBEEF, topics, false)
}

func (n *Node) submitInternal(ctx context.Context, rawBEEF []byte, topics []string, isFederation bool) (overlay.Steak, error) {
	if len(rawBEEF) == 0 {
		return nil, errors.New("hub: empty BEEF body")
	}
	if len(rawBEEF) > 4<<20 {
		return nil, errors.New("hub: BEEF exceeds 4 MiB limit")
	}
	isLive := n.net == "main" || n.net == "testnet"
	if isLive && n.tracker == nil {
		return nil, errors.New("hub: witnessed ChainTracker is required on live networks")
	}
	allMarketTopics := len(topics) > 0
	for _, t := range topics {
		if !strings.HasPrefix(t, "tm_nektar_") {
			allMarketTopics = false
			break
		}
	}
	var tx *transaction.Transaction
	_, parsedTx, _, err := guard.ParseBEEF(rawBEEF, guard.DefaultBound)
	if err == nil && parsedTx != nil {
		tx = parsedTx
	} else if !isLive && allMarketTopics && err != nil && strings.Contains(err.Error(), "no inputs") && len(rawBEEF) <= 64<<10 {
		// Unfunded 0-input OP_FALSE OP_RETURN market carrier transactions allowed ONLY on memory/regtest.
		t0, tErr := transaction.NewTransactionFromBEEF(rawBEEF)
		if tErr != nil || t0 == nil || len(t0.Inputs) != 0 || len(t0.Outputs) == 0 || len(t0.Outputs) > 16 {
			return nil, fmt.Errorf("hub: parse carrier BEEF: %w", err)
		}
		tx = t0
	} else {
		if err == nil {
			err = errors.New("empty BEEF transaction")
		}
		return nil, fmt.Errorf("hub: parse BEEF: %w", err)
	}
	// Run overlay SPV verification against witnessed headers BEFORE admitting to any topic manager
	// (including all 6 mounted Lightweb modules which assume host-engine SPV already ran).
	if len(tx.Inputs) > 0 && n.tracker != nil {
		verdict, vErr := verify.Check(ctx, tx, n.tracker)
		if verdict != verify.Passed {
			if verdict == verify.Transport {
				return nil, fmt.Errorf("hub: tracker unavailable: %w", vErr)
			}
			return nil, fmt.Errorf("hub: SPV verification refused (%v): %w", verdict, vErr)
		}
	}
	txid := tx.TxID()
	beef := transaction.NewBeef()
	if _, err := beef.MergeTransaction(tx); err != nil {
		return nil, fmt.Errorf("hub: merge tx into BEEF: %w", err)
	}

	// Track previously admitted outpoints spent by tx.Inputs so TopicManagers receive accurate
	// previousCoins and LookupServices receive OutputSpent notifications.
	type spentPrev struct {
		outpointKey string
		outpoint    transaction.Outpoint
		topic       string
	}
	spentByTopic := make(map[string][]uint32)
	var spentList []spentPrev
	for idx, inp := range tx.Inputs {
		if inp.SourceTXID == nil {
			continue
		}
		op := transaction.Outpoint{Txid: *inp.SourceTXID, Index: inp.SourceTxOutIndex}
		prevTopic, alreadySpent, found, fErr := n.store.FindEventTopicByOutpoint(op.String())
		if fErr == nil && found && !alreadySpent {
			spentByTopic[prevTopic] = append(spentByTopic[prevTopic], uint32(idx))
			spentList = append(spentList, spentPrev{
				outpointKey: op.String(),
				outpoint:    op,
				topic:       prevTopic,
			})
		}
	}

	var blockHeight int64
	if tx.MerklePath != nil {
		blockHeight = int64(tx.MerklePath.BlockHeight)
	}
	subCtx := module.WithSubmission(ctx, rawBEEF)
	steak := make(overlay.Steak)

	for _, topic := range topics {
		tm, ok := n.topics[topic]
		if !ok {
			continue
		}
		inst, err := tm.IdentifyAdmissibleOutputs(subCtx, beef, txid, spentByTopic[topic])
		if err != nil {
			return nil, fmt.Errorf("hub: topic %s: %w", topic, err)
		}
		var admitted []uint32
		for _, vout := range inst.OutputsToAdmit {
			if int(vout) >= len(tx.Outputs) {
				continue
			}
			out := tx.Outputs[vout]
			p, isNkt, _ := bstack.ExtractNkt1Payload(out.LockingScript)
			var pub string
			var domains []string
			var exp int64
			if isNkt {
				pub, domains, exp = extractMeta(p)
				if !isFederation && !n.allowPubRate(pub) {
					return nil, errors.New("hub: rate limit exceeded for pubkey")
				}
			}
			admitted = append(admitted, vout)
			op := transaction.Outpoint{Txid: *txid, Index: vout}
			var seq int64
			isNew := true
			nowMs := time.Now().UnixMilli()
			if isNkt {
				n.admitMu.Lock()
				seq, isNew, err = n.store.AppendEvent(StoredEvent{
					Outpoint:    op.String(),
					Topic:       topic,
					TxID:        txid.String(),
					Vout:        vout,
					Kind:        p.Kind,
					Pub:         pub,
					Domains:     domains,
					Payload:     p,
					BEEF:        rawBEEF,
					BlockHeight: blockHeight,
					Exp:         exp,
					AdmittedAt:  nowMs,
				})
				if err != nil {
					n.admitMu.Unlock()
					return nil, err
				}
				for _, ls := range n.lookups {
					_ = ls.OutputAdmittedByTopic(subCtx, &module.OutputAdmittedByTopic{
						Mode:          ls.AdmissionMode(),
						Outpoint:      op,
						Topic:         topic,
						Satoshis:      out.Satoshis,
						LockingScript: out.LockingScript,
						AtomicBEEF:    rawBEEF,
					})
				}
				if n.marketLS != nil {
					n.marketLS.SetSeq(op.String(), seq)
				}
				if isNew {
					n.broadcastEvent(bstack.MarketEvent{
						Seq:        seq,
						Topic:      topic,
						TxID:       txid.String(),
						Vout:       vout,
						AdmittedAt: nowMs,
						Payload:    p,
						BEEF:       append([]byte(nil), rawBEEF...),
					}, domains)
				}
				n.admitMu.Unlock()
			} else {
				for _, ls := range n.lookups {
					_ = ls.OutputAdmittedByTopic(subCtx, &module.OutputAdmittedByTopic{
						Mode:          ls.AdmissionMode(),
						Outpoint:      op,
						Topic:         topic,
						Satoshis:      out.Satoshis,
						LockingScript: out.LockingScript,
						AtomicBEEF:    rawBEEF,
					})
				}
			}
		}
		inst.OutputsToAdmit = admitted
		steak[topic] = &inst
	}

	// Mark any previously admitted outpoints spent by this verified transaction and notify SpendListeners.
	for _, sp := range spentList {
		if _, found, mErr := n.store.MarkOutpointSpent(sp.outpointKey); mErr == nil && found {
			for _, ls := range n.lookups {
				if sl, ok := ls.(module.SpendListener); ok {
					_ = sl.OutputSpent(subCtx, &module.OutputSpent{
						Mode:         module.SpendTxid,
						Outpoint:     sp.outpoint,
						Topic:        sp.topic,
						SpendingTxid: txid,
					})
				}
			}
		}
	}
	return steak, nil
}

// Lookup executes a BRC-24 lookup query against a mounted lookup service.
func (n *Node) Lookup(ctx context.Context, q *lookup.LookupQuestion) (*lookup.LookupAnswer, error) {
	if q == nil || q.Service == "" {
		return nil, errors.New("hub: lookup service name required")
	}
	ls, ok := n.lookups[q.Service]
	if !ok {
		return nil, fmt.Errorf("hub: unknown lookup service %q", q.Service)
	}
	return ls.Lookup(ctx, q)
}

// StartFederation subscribes to sibling peer hubs' /events streams with persisted cursors and jittered reconnects,
// re-admitting every verified BEEF idempotently so agents on different hubs find and trade with each other.
func (n *Node) StartFederation(ctx context.Context, peerHubURLs []string) {
	for _, u := range peerHubURLs {
		peer := strings.TrimRight(strings.TrimSpace(u), "/")
		if peer == "" {
			continue
		}
		n.wg.Add(1)
		go func(p string) {
			defer n.wg.Done()
			n.federatePeerLoop(ctx, p)
		}(peer)
	}
}

func (n *Node) federatePeerLoop(ctx context.Context, peer string) {
	fedCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-n.stopCh:
			cancel()
		case <-fedCtx.Done():
		}
	}()
	cursor := n.store.GetFedCursor(peer)
	for {
		if fedCtx.Err() != nil {
			return
		}
		next := n.pullPeerOnce(fedCtx, peer, cursor)
		if next > cursor {
			cursor = next
			_ = n.store.SetFedCursor(peer, cursor)
		}
		backoff := time.Duration(float64(3*time.Second) * (0.5 + rand.Float64()))
		select {
		case <-fedCtx.Done():
			return
		case <-n.stopCh:
			return
		case <-time.After(backoff):
		}
	}
}

func (n *Node) pullPeerOnce(ctx context.Context, peer string, cursor int64) int64 {
	// Drain missed raw BEEFs via /sync?cursor= in paginated batches until caught up.
	cursor = n.syncRange(ctx, peer, cursor)
	if cursor > 0 {
		_ = n.store.SetFedCursor(peer, cursor)
	}

	// Hold SSE /events?cursor= until EOF or error, syncing inline without tearing down the stream!
	sseReq, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/events?cursor=%d", peer, cursor), nil)
	if err != nil {
		return cursor
	}
	sseResp, err := n.sseClient.Do(sseReq)
	if err != nil || sseResp.StatusCode != http.StatusOK {
		if sseResp != nil {
			sseResp.Body.Close()
		}
		return cursor
	}
	defer sseResp.Body.Close()
	sc := bufio.NewScanner(sseResp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var pendingID int64
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			if id, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "id: ")), 10, 64); err == nil {
				pendingID = id
			}
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			raw := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			var ev bstack.MarketEvent
			if err := json.Unmarshal([]byte(raw), &ev); err == nil {
				seq := ev.Seq
				if seq == 0 {
					seq = pendingID
				}
				if seq <= cursor {
					continue
				}
				// Fast path: admit carried BEEF inline when contiguous with our cursor — zero /sync refetch!
				if len(ev.BEEF) > 0 && ev.Topic != "" && (cursor == 0 || seq == cursor+1) {
					if _, sErr := n.submitInternal(ctx, ev.BEEF, []string{ev.Topic}, true); sErr != nil {
						lower := strings.ToLower(sErr.Error())
						if strings.Contains(lower, "rate limit") || strings.Contains(lower, "busy") || strings.Contains(lower, "locked") || strings.Contains(lower, "tracker unavailable") {
							continue
						}
					}
					cursor = seq
					_ = n.store.SetFedCursor(peer, cursor)
					continue
				}
			}
			// Fallback on cursor gap or legacy SSE peer omitting BEEF: repair gap via /sync.
			if pendingID > cursor {
				next := n.syncRange(ctx, peer, cursor)
				if next > cursor {
					cursor = next
					_ = n.store.SetFedCursor(peer, cursor)
				}
			}
		}
	}
	return cursor
}

func (n *Node) syncRange(ctx context.Context, peer string, cursor int64) int64 {
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/sync?cursor=%d", peer, cursor), nil)
		if err != nil {
			return cursor
		}
		resp, err := n.httpClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			return cursor
		}
		var items []StoredEvent
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&items)
		resp.Body.Close()
		if len(items) == 0 {
			return cursor
		}
		advanced := false
		for _, it := range items {
			if len(it.BEEF) > 0 && it.Topic != "" {
				if _, sErr := n.submitInternal(ctx, it.BEEF, []string{it.Topic}, true); sErr != nil {
					// Stop advancing federation cursor on transient storage/rate/tracker errors so failed events retry cleanly.
					lower := strings.ToLower(sErr.Error())
					if strings.Contains(lower, "rate limit") || strings.Contains(lower, "busy") || strings.Contains(lower, "locked") || strings.Contains(lower, "tracker unavailable") {
						return cursor
					}
				}
			}
			if it.Seq > cursor {
				cursor = it.Seq
				advanced = true
			}
		}
		if len(items) < 200 || !advanced {
			return cursor
		}
	}
}

// Handler returns the HTTP handler exposing BRC-22 /submit, BRC-24 /lookup, numbered/cursored SSE /events,
// /sync federation feed, NAT-safe /bbox relay, /bounties, and /v1/health.
func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		bounties, _ := n.store.ListBounties(true)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"network":       n.net,
			"topics":        n.Topics(),
			"lookups":       n.Lookups(),
			"activeBounties": len(bounties),
		})
	})
	mux.HandleFunc("POST /submit", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		var topics []string
		if hdr := r.Header.Get("X-Topics"); hdr != "" {
			if err := json.Unmarshal([]byte(hdr), &topics); err != nil {
				for _, t := range strings.Split(hdr, ",") {
					if t = strings.TrimSpace(t); t != "" {
						topics = append(topics, t)
					}
				}
			}
		}
		if len(topics) == 0 {
			topics = n.Topics()
		}
		steak, err := n.Submit(r.Context(), raw, topics)
		if err != nil {
			code := http.StatusBadRequest
			if strings.Contains(err.Error(), "rate limit") {
				code = http.StatusTooManyRequests
			} else if strings.Contains(err.Error(), "tracker unavailable") {
				code = http.StatusServiceUnavailable
			}
			writeJSON(w, code, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, steak)
	})
	mux.HandleFunc("POST /lookup", func(w http.ResponseWriter, r *http.Request) {
		var q lookup.LookupQuestion
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&q); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		ans, err := n.Lookup(r.Context(), &q)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, ans)
	})
	// Numbered /sync feed for Hub-to-Hub federation & cursor gap repair.
	mux.HandleFunc("GET /sync", func(w http.ResponseWriter, r *http.Request) {
		cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
		var doms []string
		if d := strings.TrimSpace(r.URL.Query().Get("domains")); d != "" {
			doms = strings.Split(d, ",")
		}
		items, err := n.store.EventsSince(cursor, doms, 200)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, items)
	})
	// Native numbered/cursored SSE /events?cursor=<seq>&domains=<csv> (supports Last-Event-ID resume!).
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		var cursor int64
		if c := r.URL.Query().Get("cursor"); c != "" {
			cursor, _ = strconv.ParseInt(c, 10, 64)
		} else if lei := r.Header.Get("Last-Event-ID"); lei != "" {
			cursor, _ = strconv.ParseInt(strings.TrimSpace(lei), 10, 64)
		}
		var doms []string
		if d := strings.TrimSpace(r.URL.Query().Get("domains")); d != "" {
			doms = strings.Split(d, ",")
		}

		// Subscribe BEFORE replaying SQLite so zero events slip through the gap!
		ch, unsub, subOK := n.TrySubscribe(doms...)
		if !subOK {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "hub SSE subscriber limit reached"})
			return
		}
		defer unsub()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		var lastSent int64 = cursor
		if cursor >= 0 && (r.URL.Query().Has("cursor") || r.Header.Get("Last-Event-ID") != "") {
			// Drain full backlog in paginated 200-row chunks using raw scanned cursor so domain filtering never truncates replay!
			scanCursor := cursor
			for {
				backlog, nextCursor, rawCount, bErr := n.store.EventsSinceScanned(scanCursor, doms, 200)
				if bErr != nil || rawCount == 0 {
					break
				}
				scanCursor = nextCursor
				for _, st := range backlog {
					ev := bstack.MarketEvent{
						Seq:        st.Seq,
						Topic:      st.Topic,
						TxID:       st.TxID,
						Vout:       st.Vout,
						AdmittedAt: st.AdmittedAt,
						Payload:    st.Payload,
						BEEF:       st.BEEF,
					}
					b, _ := json.Marshal(ev)
					_, _ = fmt.Fprintf(w, "id: %d\nevent: market\ndata: %s\n\n", ev.Seq, b)
					if ev.Seq > lastSent {
						lastSent = ev.Seq
					}
				}
				if len(backlog) > 0 {
					flusher.Flush()
				}
				if rawCount < 200 {
					break
				}
			}
		}

		keepalive := time.NewTicker(15 * time.Second)
		defer keepalive.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-n.sseStopCh:
				return
			case <-n.stopCh:
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				if ev.Seq > 0 && ev.Seq <= lastSent {
					continue
				}
				b, _ := json.Marshal(ev)
				_, _ = fmt.Fprintf(w, "id: %d\nevent: market\ndata: %s\n\n", ev.Seq, b)
				if ev.Seq > lastSent {
					lastSent = ev.Seq
				}
				flusher.Flush()
			case <-keepalive.C:
				_, _ = fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			}
		}
	})

	// Authenticated NAT-safe counterparty store-and-forward relay (/bbox/send & /bbox/inbox).
	mux.HandleFunc("POST /bbox/send", func(w http.ResponseWriter, r *http.Request) {
		var msg BboxMessage
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&msg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if msg.RecipientPub == "" || msg.SenderPub == "" || msg.DealID == "" || msg.Kind == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "recipientPub, senderPub, dealId, and kind are required"})
			return
		}
		now := time.Now().Unix()
		if msg.TS < now-bboxSkewSeconds || msg.TS > now+bboxSkewSeconds {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "expired or missing bbox ts"})
			return
		}
		if strings.TrimSpace(msg.Nonce) == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bbox nonce required"})
			return
		}
		if msg.Aud == "" || (r.Host != "" && !strings.EqualFold(msg.Aud, r.Host)) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bbox aud does not match hub host"})
			return
		}
		if msg.Sig == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bbox message signature required"})
			return
		}
		if err := bstack.VerifyArtifact(msg.SenderPub, msg.Sig, msg.Signable()); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bbox sender signature"})
			return
		}
		// Rate-limit BEFORE claiming global bboxNonce slot so 1 client cannot DoS Hub-wide bbox (F6).
		if !n.allowPubRate(msg.SenderPub) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}
		if !n.claimBboxNonce("send:"+msg.SenderPub, msg.Nonce, now) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "replayed bbox message nonce"})
			return
		}
		seq, err := n.store.SaveBboxMessage(msg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "seq": seq})
	})
	mux.HandleFunc("GET /bbox/inbox", func(w http.ResponseWriter, r *http.Request) {
		pub := strings.TrimSpace(r.URL.Query().Get("pub"))
		if pub == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "pub query parameter required"})
			return
		}
		hdrPub := strings.TrimSpace(r.Header.Get("X-Nektar-Pub"))
		hdrTs := strings.TrimSpace(r.Header.Get("X-Nektar-Ts"))
		hdrNonce := strings.TrimSpace(r.Header.Get("X-Nektar-Nonce"))
		hdrSig := strings.TrimSpace(r.Header.Get("X-Nektar-Sig"))
		if hdrPub != pub || hdrTs == "" || hdrNonce == "" || hdrSig == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "signed X-Nektar-Pub / X-Nektar-Ts / X-Nektar-Nonce / X-Nektar-Sig headers required for bbox inbox"})
			return
		}
		ts, tsErr := strconv.ParseInt(hdrTs, 10, 64)
		now := time.Now().Unix()
		if tsErr != nil || ts < now-bboxSkewSeconds || ts > now+bboxSkewSeconds {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "expired or invalid X-Nektar-Ts"})
			return
		}
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		aud := strings.ToLower(strings.TrimSpace(r.Host))
		authPayload := map[string]any{
			"action": "bbox_inbox",
			"pub":    pub,
			"ts":     ts,
			"nonce":  hdrNonce,
			"aud":    aud,
			"since":  since,
		}
		if err := bstack.VerifyArtifact(pub, hdrSig, authPayload); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bbox inbox signature"})
			return
		}
		if !n.allowPubRate(pub) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}
		if !n.claimBboxNonce("inbox:"+pub, hdrNonce, now) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "replayed bbox inbox nonce"})
			return
		}
		msgs, err := n.store.ListBboxMessages(pub, since, 50)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
	})

	// Hub knowledge bounties — lets organic agents sell verified domain facts directly TO the Hub!
	mux.HandleFunc("GET /bounties", func(w http.ResponseWriter, _ *http.Request) {
		bounties, err := n.store.ListBounties(true)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"bounties": bounties})
	})
	mux.HandleFunc("POST /bounties/submit", func(w http.ResponseWriter, r *http.Request) {
		var sub BountySubmission
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&sub); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if sub.HDP == "" || sub.Text == "" || sub.SellerPub == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "hdp, text, and sellerPub are required"})
			return
		}
		// Canonicalize HDP to 64 lowercase hex chars BEFORE deduplicating in SQLite!
		hdpRaw, hErr := hex.DecodeString(strings.TrimSpace(sub.HDP))
		if hErr != nil || len(hdpRaw) != 32 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid 32-byte hdp hex"})
			return
		}
		sub.HDP = hex.EncodeToString(hdpRaw)

		// Require fresh signed submission from SellerPub to prevent unauthenticated ProofBundle theft.
		now := time.Now().Unix()
		if sub.TS < now-bboxSkewSeconds || sub.TS > now+bboxSkewSeconds || sub.Sig == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "fresh signed bounty submission (ts + sig) required"})
			return
		}
		if err := bstack.VerifyArtifact(sub.SellerPub, sub.Sig, sub.Signable(sub.HDP)); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bounty submission signature"})
			return
		}

		if !n.allowPubRate(sub.SellerPub) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}
		b, err := n.store.GetBounty(sub.BountyID)
		if err != nil || !b.Active || b.Filled >= b.MaxFills {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "bounty not found or already filled"})
			return
		}
		n.mu.RLock()
		bh := n.bountyHandler
		n.mu.RUnlock()
		if bh == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "hub bounty buyer is not armed"})
			return
		}
		// Atomically reserve bounty slot + unique canonical HDP claim BEFORE executing payout!
		if err := n.store.ReserveBountyClaim(b.ID, sub.HDP, sub.SellerPub, b.RewardSats); err != nil {
			if errors.Is(err, ErrBountyDuplicate) {
				if claim, cErr := n.store.GetBountyClaim(sub.HDP); cErr == nil && (claim.Status == "pending" || claim.Status == "broadcast") {
					writeJSON(w, http.StatusAccepted, BountyPayoutResult{
						Accepted:   false,
						BountyID:   claim.BountyID,
						RewardSats: claim.RewardSats,
						TxID:       claim.TxID,
						Status:     claim.Status,
						Reason:     "bounty payout already " + claim.Status + " (awaiting confirmation)",
					})
					return
				}
				writeJSON(w, http.StatusConflict, map[string]string{"error": "duplicate fragment HDP already claimed"})
				return
			}
			if errors.Is(err, ErrBountyFilled) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "bounty not found or already filled"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		res, err := bh(r.Context(), b, sub)
		if err != nil {
			// Only release reservation if payout definitely did NOT broadcast (never on ErrBroadcastUnknown!).
			if !errors.Is(err, nwallet.ErrBroadcastUnknown) {
				_ = n.store.CancelBountyClaim(b.ID, sub.HDP)
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if res != nil && res.Accepted {
			if n.net == "main" || n.net == "testnet" {
				_ = n.store.MarkBountyClaimBroadcast(sub.HDP, res.TxID)
				res.Status = "broadcast"
			} else {
				_ = n.store.FinalizeBountyClaim(sub.HDP, res.TxID)
				res.Status = "paid"
			}
		} else {
			_ = n.store.CancelBountyClaim(b.ID, sub.HDP)
		}
		writeJSON(w, http.StatusOK, res)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
