package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/overlay"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	bboxmod "github.com/lightwebinc/bbox/module"
	"github.com/lightwebinc/bcommon/module"
	bfingermod "github.com/lightwebinc/bfinger/module"
	blogsmod "github.com/lightwebinc/blogs/module"
	borgmod "github.com/lightwebinc/borg/module"
	bsecretmod "github.com/lightwebinc/bsecret/module"
	bstoremod "github.com/lightwebinc/bstore/module"

	"github.com/kryptokrates/nektar/internal/bstack"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

// BountySubmission is an agent's submission selling verified knowledge to the Hub bounty pool.
type BountySubmission struct {
	BountyID      string          `json:"bountyId"`
	SellerPub     string          `json:"sellerPub"`
	SellerAddress string          `json:"sellerAddress"`
	Shard         string          `json:"shard"`
	Seq           int64           `json:"seq"`
	HDP           string          `json:"hdp"`
	Text          string          `json:"text"`
	ProvenanceHex string          `json:"provenanceHex,omitempty"`
	ProofBundle   json.RawMessage `json:"proofBundle,omitempty"`
}

// BountyPayoutResult reports the Hub's verified payment to a contributing agent.
type BountyPayoutResult struct {
	Accepted   bool   `json:"accepted"`
	BountyID   string `json:"bountyId"`
	RewardSats uint64 `json:"rewardSats"`
	TxID       string `json:"txid,omitempty"`
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

// Node is a durable, self-contained Go overlay hub serving BRC-22 /submit, BRC-24 /lookup,
// numbered/cursored SSE /events, NAT-safe /bbox relay, /bounties, and Hub-to-Hub federation.
type Node struct {
	mu            sync.RWMutex
	admitMu       sync.Mutex // serializes AppendEvent -> broadcastEvent so SSE seq order is strictly monotonic
	net           string
	host          *simpleHost
	store         *Store
	marketLS      *bstack.MarketLookupService
	topics        map[string]module.TopicManager
	lookups       map[string]module.LookupService
	subs          map[chan bstack.MarketEvent]map[string]bool
	rates         map[string]*rateBucket
	maxPerMin     int
	tracker       chaintracker.ChainTracker
	bountyHandler BountyHandler
	httpClient    *http.Client
	stopCh        chan struct{}
	closeOnce     sync.Once
}

// NewNode constructs an embedded durable Hub Node mounting Nektar's 16 market lanes + requested Lightweb modules.
func NewNode(cfg Config) (*Node, error) {
	net := cfg.Network
	if net == "" {
		net = "regtest"
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
	n := &Node{
		net:           net,
		host:          newSimpleHost(),
		store:         st,
		topics:        make(map[string]module.TopicManager),
		lookups:       make(map[string]module.LookupService),
		subs:          make(map[chan bstack.MarketEvent]map[string]bool),
		rates:         make(map[string]*rateBucket),
		maxPerMin:     maxPerMin,
		tracker:       cfg.ChainTracker,
		bountyHandler: cfg.OnBountySubmit,
		httpClient:    &http.Client{Timeout: 20 * time.Second},
		stopCh:        make(chan struct{}),
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
	go n.maintenanceLoop()

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

func (n *Node) Close() error {
	n.closeOnce.Do(func() { close(n.stopCh) })
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
			tx, err := transaction.NewTransactionFromBEEF(ev.BEEF)
			if err != nil || int(ev.Vout) >= len(tx.Outputs) {
				continue
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
	ch := make(chan bstack.MarketEvent, 128)
	filter := make(map[string]bool)
	for _, d := range domains {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			filter[d] = true
		}
	}
	n.mu.Lock()
	n.subs[ch] = filter
	n.mu.Unlock()
	return ch, func() {
		n.mu.Lock()
		delete(n.subs, ch)
		close(ch)
		n.mu.Unlock()
	}
}

// Submit processes a BRC-22 Atomic BEEF submission against requested topics, persists durably with a monotonic seq,
// notifies lookup services, and pushes numbered SSE events.
func (n *Node) Submit(ctx context.Context, rawBEEF []byte, topics []string) (overlay.Steak, error) {
	if len(rawBEEF) == 0 {
		return nil, errors.New("hub: empty BEEF body")
	}
	tx, err := transaction.NewTransactionFromBEEF(rawBEEF)
	if err != nil {
		return nil, fmt.Errorf("hub: parse BEEF: %w", err)
	}
	beef := transaction.NewBeef()
	if _, err := beef.MergeTransaction(tx); err != nil {
		return nil, fmt.Errorf("hub: merge tx into BEEF: %w", err)
	}
	txid := tx.TxID()
	subCtx := module.WithSubmission(ctx, rawBEEF)
	steak := make(overlay.Steak)

	for _, topic := range topics {
		tm, ok := n.topics[topic]
		if !ok {
			continue
		}
		inst, err := tm.IdentifyAdmissibleOutputs(subCtx, beef, txid, nil)
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
				if !n.allowPubRate(pub) {
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
					Outpoint:   op.String(),
					Topic:      topic,
					TxID:       txid.String(),
					Vout:       vout,
					Kind:       p.Kind,
					Pub:        pub,
					Domains:    domains,
					Payload:    p,
					BEEF:       rawBEEF,
					Exp:        exp,
					AdmittedAt: nowMs,
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
		go n.federatePeerLoop(ctx, peer)
	}
}

func (n *Node) federatePeerLoop(ctx context.Context, peer string) {
	cursor := n.store.GetFedCursor(peer)
	for {
		if ctx.Err() != nil {
			return
		}
		next := n.pullPeerOnce(ctx, peer, cursor)
		if next > cursor {
			cursor = next
			_ = n.store.SetFedCursor(peer, cursor)
		}
		backoff := time.Duration(float64(3*time.Second) * (0.5 + rand.Float64()))
		select {
		case <-ctx.Done():
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
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil || sseResp.StatusCode != http.StatusOK {
		if sseResp != nil {
			sseResp.Body.Close()
		}
		return cursor
	}
	defer sseResp.Body.Close()
	sc := bufio.NewScanner(sseResp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			if id, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "id: ")), 10, 64); err == nil && id > cursor {
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
				_, _ = n.Submit(ctx, it.BEEF, []string{it.Topic})
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
		ch, unsub := n.Subscribe(doms...)
		defer unsub()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		var lastSent int64 = cursor
		if cursor >= 0 && (r.URL.Query().Has("cursor") || r.Header.Get("Last-Event-ID") != "") {
			// Drain full backlog in paginated 200-event chunks before switching to live push!
			for {
				backlog, bErr := n.store.EventsSince(lastSent, doms, 200)
				if bErr != nil || len(backlog) == 0 {
					break
				}
				for _, st := range backlog {
					ev := bstack.MarketEvent{
						Seq:        st.Seq,
						Topic:      st.Topic,
						TxID:       st.TxID,
						Vout:       st.Vout,
						AdmittedAt: st.AdmittedAt,
						Payload:    st.Payload,
					}
					b, _ := json.Marshal(ev)
					_, _ = fmt.Fprintf(w, "id: %d\nevent: market\ndata: %s\n\n", ev.Seq, b)
					if ev.Seq > lastSent {
						lastSent = ev.Seq
					}
				}
				flusher.Flush()
				if len(backlog) < 200 {
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
		if msg.Sig == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bbox message signature required"})
			return
		}
		if err := bstack.VerifyArtifact(msg.SenderPub, msg.Sig, msg.Signable()); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bbox sender signature"})
			return
		}
		if !n.allowPubRate(msg.SenderPub) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
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
		hdrSig := strings.TrimSpace(r.Header.Get("X-Nektar-Sig"))
		if hdrPub != pub || hdrTs == "" || hdrSig == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "signed X-Nektar-Pub / X-Nektar-Ts / X-Nektar-Sig headers required for bbox inbox"})
			return
		}
		ts, tsErr := strconv.ParseInt(hdrTs, 10, 64)
		now := time.Now().Unix()
		if tsErr != nil || ts < now-300 || ts > now+300 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "expired or invalid X-Nektar-Ts"})
			return
		}
		authPayload := map[string]any{"action": "bbox_inbox", "pub": pub, "ts": ts}
		if err := bstack.VerifyArtifact(pub, hdrSig, authPayload); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bbox inbox signature"})
			return
		}
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
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
		// Atomically reserve bounty slot + unique HDP claim BEFORE executing payout!
		if err := n.store.ReserveBountyClaim(b.ID, sub.HDP, sub.SellerPub, b.RewardSats); err != nil {
			if errors.Is(err, ErrBountyDuplicate) {
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
			_ = n.store.FinalizeBountyClaim(sub.HDP, res.TxID)
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
