package server

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kryptokrates/nektar/internal/autonomy"
	"github.com/kryptokrates/nektar/internal/backup"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/reasoning"
)

//go:embed all:ui_dist
var uiDistFS embed.FS

// Config configures the unified v0.1 HTTP / MCP / A2A / L0-Proxy / Dashboard / Hub / Restate server.
type Config struct {
	ListenAddr        string
	PublicURL         string
	DataDir           string            // explicit runtime data directory (injected from CLI/config)
	APIToken          string            // optional HTTP/agent bearer token
	OperatorToken     string            // optional higher-privilege operator token (approvals/policies/hosts)
	ToolProfile       string            // agent | operator (default operator)
	ToolPrices        map[string]uint64 // optional BRC-105/402 satoshi price per route/tool name
	OpenAIUpstream    string            // L0 proxy upstream (default https://api.openai.com)
	AnthropicUpstream string            // L0 proxy upstream (default https://api.anthropic.com)
	Hub               *hub.Node         // optional embedded Go overlay hub mounted under /hub/
	Discovery         *discovery.Service
	Broker            *market.Broker
	Watchdog          *autonomy.Watchdog  // optional SNTP clock-skew & spend anomaly safety watchdog
	Backup            *backup.Manager     // optional scheduled encrypted state backup manager
	RestateClient     *market.RestateClient // active Restate Ingress/Admin client (restate-server)
	RestateAdminURL   string
	ExposeWalletWire  bool // opt-in loopback-only BRC-100 wire at /v1/wallet/wire/
	HTTPClient        *http.Client
}

type pairTicket struct {
	token   string
	expires time.Time
}

// App bundles all Nektar v0.1 HTTP interfaces into one net/http Handler.
type App struct {
	mem            *memory.Service
	mkt            *market.Engine
	broker         *market.Broker
	disc           *discovery.Service
	watchdog       *autonomy.Watchdog
	backupMgr      *backup.Manager
	toolbox        *Toolbox
	mcpSrv         *mcp.Server
	cfg            Config
	startedAt      time.Time
	httpClient     *http.Client
	proxyTransport *http.Transport
	handler        http.Handler

	evMu   sync.Mutex
	evSubs map[chan []byte]struct{}

	feedMu   sync.Mutex
	feedSubs map[string]map[chan shardFeedFrame]struct{}

	pairMu    sync.Mutex
	pairCodes map[string]pairTicket

	faucetMu        sync.Mutex
	faucetIPs       map[string][]int64
	faucetLastPrune int64

	oauthRegMu        sync.Mutex
	oauthRegIPs       map[string][]int64
	oauthRegLastPrune int64

	oauthMu      sync.Mutex
	oauthClients map[string]oauthClient
	oauthCodes   map[string]oauthCode
	oauthTokens  map[string]oauthAccessToken

	ttfvMs      atomic.Int64
	verifyViews atomic.Int64

	stopCh    chan struct{}
	closeOnce sync.Once
	bgMu      sync.Mutex
	bgCtx     context.Context
	bgCancel  context.CancelFunc
	bgWG      sync.WaitGroup
	closed    bool

	restateMu      sync.RWMutex
	restateAliveFn func() bool
}

// NewApp wires MCP Streamable HTTP, Unified Tools API, REST API, A2A Agent Card + JSON-RPC,
// L0 LLM Proxy, BRC-105/402 payment claims, Svelte 5 Dashboard (/state + /events + /pair), Hub, and Restate.
func NewApp(mem *memory.Service, mkt *market.Engine, cfg Config) (*App, error) {
	if cfg.OpenAIUpstream == "" {
		cfg.OpenAIUpstream = "https://api.openai.com"
	}
	if cfg.AnthropicUpstream == "" {
		cfg.AnthropicUpstream = "https://api.anthropic.com"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	disc := cfg.Discovery
	if disc == nil && cfg.Broker != nil {
		disc = cfg.Broker.Discovery()
	}
	if disc == nil {
		disc = discovery.New(mem, nil, nil)
	}
	bgCtx, bgCancel := context.WithCancel(context.Background())
	tb := NewToolbox(mem, mkt, cfg.Broker, disc, cfg.Hub, cfg.ToolProfile)
	wd := cfg.Watchdog
	if wd == nil && cfg.Broker != nil {
		wd = cfg.Broker.Watchdog()
	}
	if wd != nil {
		tb.SetWatchdog(wd)
	}
	if cfg.Backup != nil {
		tb.SetBackup(cfg.Backup)
	}
	app := &App{
		mem:        mem,
		mkt:        mkt,
		broker:     cfg.Broker,
		disc:       disc,
		watchdog:   wd,
		backupMgr:  cfg.Backup,
		toolbox:    tb,
		mcpSrv:     NewMCPServerWithToolbox(tb),
		cfg:        cfg,
		startedAt:  time.Now(),
		httpClient: hc,
		proxyTransport: &http.Transport{
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConns:          64,
			IdleConnTimeout:       90 * time.Second,
		},
		evSubs:       make(map[chan []byte]struct{}),
		feedSubs:     make(map[string]map[chan shardFeedFrame]struct{}),
		pairCodes:    make(map[string]pairTicket),
		faucetIPs:    make(map[string][]int64),
		oauthRegIPs:  make(map[string][]int64),
		oauthClients: make(map[string]oauthClient),
		oauthCodes:   make(map[string]oauthCode),
		oauthTokens:  make(map[string]oauthAccessToken),
		stopCh:       make(chan struct{}),
		bgCtx:        bgCtx,
		bgCancel:     bgCancel,
	}
	if cfg.Broker != nil {
		cfg.Broker.OnMarketTape(func(event string, ev market.MarketTapeEvent) {
			evCopy := ev
			app.BroadcastDashboardPayload(event, &evCopy)
		})
	}
	if mem != nil {
		mem.SetOnSupersede(func(ev memory.SupersessionEvent) {
			app.BroadcastShardFeedEvent(ev.Shard, "fragment_superseded", ev)
		})
	}
	app.handler = app.buildMux()
	return app, nil
}

func (a *App) Handler() http.Handler { return a.handler }
func (a *App) MCP() *mcp.Server      { return a.mcpSrv }
func (a *App) Toolbox() *Toolbox     { return a.toolbox }

// Close wakes and disconnects active Dashboard SSE streams and drains tracked background goroutines.
func (a *App) Close() {
	a.closeOnce.Do(func() {
		a.bgMu.Lock()
		a.closed = true
		a.bgMu.Unlock()
		if a.bgCancel != nil {
			a.bgCancel()
		}
		if a.stopCh != nil {
			close(a.stopCh)
		}
		a.bgWG.Wait()
	})
}

func (a *App) goBackground(fn func(ctx context.Context)) {
	a.bgMu.Lock()
	if a.closed {
		a.bgMu.Unlock()
		return
	}
	ctx := a.bgCtx
	if ctx == nil {
		ctx = context.Background()
	}
	a.bgWG.Add(1)
	a.bgMu.Unlock()
	go func() {
		defer a.bgWG.Done()
		fn(ctx)
	}()
}

// SetRestateClient attaches the active Restate client and Admin URL after Broker/HTTP initialization.
func (a *App) SetRestateClient(rc *market.RestateClient, adminURL string) {
	a.restateMu.Lock()
	a.cfg.RestateClient = rc
	if adminURL != "" {
		a.cfg.RestateAdminURL = adminURL
	}
	a.restateMu.Unlock()
}

// SetRestateLiveness registers a liveness probe for an auto-supervised Restate runtime.
func (a *App) SetRestateLiveness(aliveFn func() bool) {
	a.restateMu.Lock()
	a.restateAliveFn = aliveFn
	a.restateMu.Unlock()
	if a.toolbox != nil {
		a.toolbox.SetRestateLiveness(aliveFn)
	}
}

func (a *App) restateClient() *market.RestateClient {
	a.restateMu.RLock()
	defer a.restateMu.RUnlock()
	return a.cfg.RestateClient
}

func (a *App) restateAdminURL() string {
	a.restateMu.RLock()
	defer a.restateMu.RUnlock()
	return a.cfg.RestateAdminURL
}

func (a *App) isRestateAlive() bool {
	a.restateMu.RLock()
	fn := a.restateAliveFn
	a.restateMu.RUnlock()
	return fn == nil || fn()
}

type dashboardSSEFrame struct {
	Event string                  `json:"event"`
	Ts    int64                   `json:"ts"`
	Tape  *market.MarketTapeEvent `json:"tape,omitempty"`
}

// BroadcastDashboardEvent wakes connected Svelte Dashboard SSE subscribers immediately.
func (a *App) BroadcastDashboardEvent(kind string) {
	a.BroadcastDashboardPayload(kind, nil)
}

// BroadcastDashboardPayload pushes a typed live SSE frame (with optional market tape event) to dashboard subscribers.
func (a *App) BroadcastDashboardPayload(kind string, tape *market.MarketTapeEvent) {
	raw, err := json.Marshal(dashboardSSEFrame{
		Event: kind,
		Ts:    time.Now().UnixMilli(),
		Tape:  tape,
	})
	if err != nil {
		return
	}
	a.evMu.Lock()
	for ch := range a.evSubs {
		select {
		case ch <- raw:
		default:
		}
	}
	a.evMu.Unlock()
}

// BuildStatus computes the live node status payload shared by MCP, REST, and CLI.
func BuildStatus(ctx context.Context, mem *memory.Service, mkt *market.Engine) map[string]any {
	shards, _ := mem.ListUserShards()
	addr, _ := mem.Chain().FundAddress()
	h, _ := mem.Chain().Height(ctx)
	deals, _ := mem.DB().ListDeals(50)
	var engDeals []market.DealState
	if mkt != nil {
		engDeals = mkt.ListDeals()
	}
	spentToday, maxDay, maxFee, txCount := mem.Chain().SpendGuard().Stats()
	return map[string]any{
		"version":     "0.1.0",
		"runtime":     "go-single-binary",
		"network":     mem.Chain().Network(),
		"height":      h,
		"identityPub": mem.Chain().IdentityPubHex(),
		"fundAddress": addr,
		"balanceSats": mem.Chain().BalanceSats(),
		"utxos":       mem.Chain().UTXOCount(),
		"spendGuard": map[string]any{
			"spentToday": spentToday,
			"maxPerDay":  maxDay,
			"maxFee":     maxFee,
			"txCount":    txCount,
		},
		"policyHash": mem.PolicyCommit().Hash,
		"shards":     shards,
		"contracts":  contracts.ListArtifacts(),
		"deals":      deals,
		"liveDeals":  engDeals,
		"truthNote":  reasoning.TruthNote,
	}
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if os.Getenv("NEKTAR_TRUST_PROXY") == "1" {
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
			scheme = "https"
		}
		if xfHost := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); xfHost != "" {
			// Take first hop if comma-separated and validate strictly before trusting.
			if first, _, ok := strings.Cut(xfHost, ","); ok {
				xfHost = strings.TrimSpace(first)
			}
			if isValidHostHeader(xfHost) {
				host = xfHost
			}
		}
	}
	if !isValidHostHeader(host) {
		host = "127.0.0.1:18420"
	}
	return scheme + "://" + host
}

func serveEmbeddedIndex(w http.ResponseWriter, uiSub fs.FS) {
	b, err := fs.ReadFile(uiSub, "index.html")
	if err != nil {
		http.Error(w, "dashboard not built", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func shortStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func maxI64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
