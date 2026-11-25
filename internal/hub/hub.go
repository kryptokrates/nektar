package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
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
)

// Config configures the embedded v0.4 Go Overlay Hub.
// Replaces the legacy 12-container Hub stack (2x MySQL/MariaDB, header-shim, watch-a, watch-b, bbox-host)
// with a single Go process mounting Nektar market lanes + Lightweb Go overlay modules directly!
type Config struct {
	Network        string
	EnableFinger   bool // mounts lightwebinc/bfinger/module (tm_finger, ls_finger)
	EnableBbox     bool // mounts lightwebinc/bbox/module when getenv configures BBOX_OFFICES
	EnableBlogs    bool // mounts lightwebinc/blogs/module when getenv configures BLOGS_STREAMS
	EnableBorg     bool // mounts lightwebinc/borg/module when getenv configures BORG_HALLS
	EnableBsecret  bool // mounts lightwebinc/bsecret/module when getenv configures BSECRET_CRYPTS
	EnableBstore   bool // mounts lightwebinc/bstore/module when getenv configures BSTORE_BUCKETS
	Getenv         func(string) string
	ChainTracker   chaintracker.ChainTracker
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

// Node is a self-contained Go overlay hub serving BRC-22 /submit, BRC-24 /lookup,
// mounted module discovery, and native SSE /events push (eliminating watch-shim!).
type Node struct {
	mu      sync.RWMutex
	net     string
	host    *simpleHost
	topics  map[string]module.TopicManager
	lookups map[string]module.LookupService
	subs    map[chan bstack.MarketEvent]struct{}
	history []bstack.MarketEvent
}

// NewNode constructs an embedded Hub Node mounting Nektar's 16 market lanes + requested Lightweb modules.
func NewNode(cfg Config) (*Node, error) {
	net := cfg.Network
	if net == "" {
		net = "regtest"
	}
	getenv := cfg.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	n := &Node{
		net:     net,
		host:    newSimpleHost(),
		topics:  make(map[string]module.TopicManager),
		lookups: make(map[string]module.LookupService),
		subs:    make(map[chan bstack.MarketEvent]struct{}),
	}

	// 1. Mount Nektar v0.4 market module (tm_nektar_<net>_0..f + ls_nektar) with live SSE callback!
	nktMod, err := bstack.NewMarketModule(net, n.publishEvent)(n.host)
	if err != nil {
		return nil, fmt.Errorf("hub: mount nektar market: %w", err)
	}
	n.mountModule(nktMod)

	// 2. Mount Lightweb bfinger (tm_finger + ls_finger — zero external config needed!).
	if cfg.EnableFinger {
		fm, err := bfingermod.Mount(n.host, []string{bfingermod.Topic})
		if err != nil {
			return nil, fmt.Errorf("hub: mount bfinger: %w", err)
		}
		n.mountModule(fm.Module)
	}

	// 3. Optionally mount bbox / blogs / borg / bsecret / bstore Go modules when configured via env.
	if cfg.EnableBbox && getenv("BBOX_OFFICES") != "" {
		m, err := bboxmod.New(n.host, getenv)
		if err != nil {
			return nil, fmt.Errorf("hub: mount bbox: %w", err)
		}
		n.mountModule(m.Module)
	}
	if cfg.EnableBlogs && getenv("BLOGS_STREAMS") != "" {
		m, err := blogsmod.New(n.host, getenv)
		if err != nil {
			return nil, fmt.Errorf("hub: mount blogs: %w", err)
		}
		n.mountModule(m.Module)
	}
	if cfg.EnableBorg && getenv("BORG_HALLS") != "" {
		m, err := borgmod.New(n.host, getenv)
		if err != nil {
			return nil, fmt.Errorf("hub: mount borg: %w", err)
		}
		n.mountModule(m.Module)
	}
	if cfg.EnableBsecret && getenv("BSECRET_CRYPTS") != "" {
		m, err := bsecretmod.New(n.host, getenv)
		if err != nil {
			return nil, fmt.Errorf("hub: mount bsecret: %w", err)
		}
		n.mountModule(m.Module)
	}
	if cfg.EnableBstore && getenv("BSTORE_BUCKETS") != "" {
		m, err := bstoremod.New(n.host, getenv)
		if err != nil {
			return nil, fmt.Errorf("hub: mount bstore: %w", err)
		}
		n.mountModule(m.Module)
	}

	return n, nil
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

func (n *Node) publishEvent(ev bstack.MarketEvent) {
	n.mu.Lock()
	n.history = append(n.history, ev)
	if len(n.history) > 1024 {
		n.history = n.history[len(n.history)-1024:]
	}
	for ch := range n.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	n.mu.Unlock()
}

// Subscribe registers a channel receiving live admitted market events.
func (n *Node) Subscribe() (<-chan bstack.MarketEvent, func()) {
	ch := make(chan bstack.MarketEvent, 64)
	n.mu.Lock()
	n.subs[ch] = struct{}{}
	n.mu.Unlock()
	return ch, func() {
		n.mu.Lock()
		delete(n.subs, ch)
		close(ch)
		n.mu.Unlock()
	}
}

// Submit processes a BRC-22 Atomic BEEF submission against requested topics and notifies lookup services.
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
		steak[topic] = &inst
		for _, vout := range inst.OutputsToAdmit {
			if int(vout) >= len(tx.Outputs) {
				continue
			}
			out := tx.Outputs[vout]
			op := transaction.Outpoint{Txid: *txid, Index: vout}
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

// Handler returns the HTTP handler exposing BRC-22 /submit, BRC-24 /lookup, /events (SSE), and /v1/health.
func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"network": n.net,
			"topics":  n.Topics(),
			"lookups": n.Lookups(),
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
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
	// Native SSE /events stream — permanently retires watch-a / watch-b Node polling shims!
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		ch, unsub := n.Subscribe()
		defer unsub()
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
				b, _ := json.Marshal(ev)
				_, _ = fmt.Fprintf(w, "event: market\ndata: %s\n\n", b)
				flusher.Flush()
			case <-keepalive.C:
				_, _ = fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			}
		}
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
