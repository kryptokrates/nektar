package server

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
	APIToken          string            // optional HTTP/agent bearer token
	OperatorToken     string            // optional higher-privilege operator token (approvals/policies/hosts)
	ToolProfile       string            // agent | operator (default operator)
	ToolPrices        map[string]uint64 // optional BRC-105/402 satoshi price per route/tool name
	OpenAIUpstream    string            // L0 proxy upstream (default https://api.openai.com)
	AnthropicUpstream string            // L0 proxy upstream (default https://api.anthropic.com)
	Hub               *hub.Node         // optional embedded Go overlay hub mounted under /hub/
	Discovery         *discovery.Service
	Broker            *market.Broker
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
	toolbox        *Toolbox
	mcpSrv         *mcp.Server
	cfg            Config
	startedAt      time.Time
	httpClient     *http.Client
	proxyTransport *http.Transport
	handler        http.Handler

	evMu   sync.Mutex
	evSubs map[chan []byte]struct{}

	pairMu    sync.Mutex
	pairCodes map[string]pairTicket

	faucetMu  sync.Mutex
	faucetIPs map[string][]int64

	oauthRegMu  sync.Mutex
	oauthRegIPs map[string][]int64

	oauthMu      sync.Mutex
	oauthClients map[string]oauthClient
	oauthCodes   map[string]oauthCode
	oauthTokens  map[string]oauthAccessToken

	ttfvMs      atomic.Int64
	verifyViews atomic.Int64

	stopCh    chan struct{}
	closeOnce sync.Once
	restateMu sync.RWMutex
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
	tb := NewToolbox(mem, mkt, cfg.Broker, disc, cfg.Hub, cfg.ToolProfile)
	app := &App{
		mem:        mem,
		mkt:        mkt,
		broker:     cfg.Broker,
		disc:       disc,
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
		pairCodes:    make(map[string]pairTicket),
		faucetIPs:    make(map[string][]int64),
		oauthRegIPs:  make(map[string][]int64),
		oauthClients: make(map[string]oauthClient),
		oauthCodes:   make(map[string]oauthCode),
		oauthTokens:  make(map[string]oauthAccessToken),
		stopCh:       make(chan struct{}),
	}
	if cfg.Broker != nil {
		cfg.Broker.OnMarketTape(func(event string, ev market.MarketTapeEvent) {
			evCopy := ev
			app.BroadcastDashboardPayload(event, &evCopy)
		})
	}
	app.handler = app.buildMux()
	return app, nil
}

func (a *App) Handler() http.Handler { return a.handler }
func (a *App) MCP() *mcp.Server      { return a.mcpSrv }
func (a *App) Toolbox() *Toolbox     { return a.toolbox }

// Close wakes and disconnects active Dashboard SSE streams so http.Server.Shutdown drains immediately.
func (a *App) Close() {
	a.closeOnce.Do(func() {
		if a.stopCh != nil {
			close(a.stopCh)
		}
	})
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

func (a *App) restateClient() *market.RestateClient {
	a.restateMu.RLock()
	defer a.restateMu.RUnlock()
	return a.cfg.RestateClient
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

func (a *App) buildMux() http.Handler {
	mux := http.NewServeMux()

	// 1. Official MCP Streamable HTTP transport (/mcp) with localhost protection & BRC-105 paywall.
	mcpHandler := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
		return a.mcpSrv
	}, &mcp.StreamableHTTPOptions{
		Stateless: true,
	})
	mux.Handle("/mcp", a.withAuth(a.withMCPPaywall(mcpHandler)))

	// 2. Auth session & 1-click pairing (/v1/auth/session, /pair).
	mux.HandleFunc("POST /v1/auth/session", a.handleAuthSession)
	mux.HandleFunc("GET /pair", a.handlePair)

	// 3. Health, discovery, app manifest/icons (for BSV Desktop Connected Apps), public card & A2A Agent Card.
	a.registerAppManifestRoutes(mux)
	healthHandler := func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"version": "0.1.0",
			"network": a.mem.Chain().Network(),
		})
	}
	mux.HandleFunc("GET /v1/health", healthHandler)
	mux.HandleFunc("GET /health", healthHandler)
	mux.HandleFunc("GET /healthz", healthHandler)
	mux.Handle("GET /v1/status", a.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, BuildStatus(r.Context(), a.mem, a.mkt))
	})))
	mux.HandleFunc("GET /.well-known/nektar", func(w http.ResponseWriter, r *http.Request) {
		var publicPacks []string
		if a.broker != nil {
			for _, p := range a.broker.AdvertisedPacks() {
				if p.Shard != "" {
					publicPacks = append(publicPacks, p.Shard)
				}
			}
		}
		resp := map[string]any{
			"version":     "0.1.0",
			"identityPub": a.mem.Chain().IdentityPubHex(),
			"network":     a.mem.Chain().Network(),
			"packBase":    "/v1/pack/",
			"verifyBase":  "/v1/verify/",
			"mcp":         "/mcp",
			"a2a":         "/a2a",
			"publicPacks": publicPacks,
			"hosts":       a.disc.ActiveHosts(),
		}
		if a.cfg.Hub != nil {
			resp["hub"] = "/hub"
		}
		writeJSON(w, http.StatusOK, resp)
	})
	mux.HandleFunc("GET /.well-known/agent.json", a.handleAgentCard)
	mux.Handle("POST /a2a", a.withAuth(http.HandlerFunc(a.handleA2A)))
	mux.HandleFunc("GET /card", a.handlePublicCard)

	// 4. Unified Tools API (used by OnboardingCard, Hermes, CLI, and external SDKs!).
	mux.Handle("GET /v1/tools", a.withAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"profile": a.toolbox.Profile(), "tools": a.toolbox.ListTools()})
	})))
	mux.Handle("POST /v1/tools/{name}", a.withAuth(http.HandlerFunc(a.handleToolCall)))

	// 5. Memory, Anchoring, Grounding & Receipts REST API.
	mux.Handle("POST /v1/remember", a.withAuth(a.withPaywall("remember", http.HandlerFunc(a.handleRemember))))
	mux.Handle("POST /v1/recall", a.withAuth(a.withPaywall("recall", http.HandlerFunc(a.handleRecall))))
	mux.Handle("POST /v1/anchor", a.withAuth(a.withPaywall("anchor", http.HandlerFunc(a.handleAnchor))))
	mux.Handle("POST /v1/answer", a.withAuth(a.withPaywall("groundedAnswer", http.HandlerFunc(a.handleAnswer))))
	mux.HandleFunc("GET /v1/verify", a.handleVerifyReceipt)
	mux.HandleFunc("GET /v1/verify/{token}", a.handleVerifyReceipt)
	mux.Handle("GET /v1/pack/{shard}", a.withOperatorAuth(http.HandlerFunc(a.handleShardPack)))
	mux.HandleFunc("GET /v1/chirp/{hash}", a.handleChirpObject)
	a.registerWalletRoutes(mux)

	// 6. Live A2A Market Purchase, Host Discovery, Onboarding, Faucet & Approvals!
	mux.HandleFunc("POST /v1/market/purchase", a.handleMarketPurchase)
	mux.Handle("GET /v1/topics/cluster", a.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		topic := strings.TrimSpace(r.URL.Query().Get("topic"))
		hint := strings.TrimSpace(r.URL.Query().Get("hint"))
		var matcher market.Matcher
		var peerTopics []string
		if a.broker != nil {
			matcher = a.broker.Matcher()
			peerTopics = a.broker.ActiveNetworkTopics()
		}
		res := market.ClassifyTopicWithPeers(r.Context(), matcher, topic, hint, peerTopics)
		writeJSON(w, http.StatusOK, res)
	})))
	mux.Handle("POST /v1/hosts", a.withOperatorAuth(http.HandlerFunc(a.handleJoinHost)))
	mux.Handle("DELETE /v1/hosts", a.withOperatorAuth(http.HandlerFunc(a.handleLeaveHost)))
	mux.Handle("POST /v1/onboard/faucet", a.withAuth(http.HandlerFunc(a.handleOnboardFaucet)))
	mux.HandleFunc("POST /v1/faucet", a.handlePublicFaucet)
	mux.Handle("POST /v1/onboard/policy-preset", a.withOperatorAuth(http.HandlerFunc(a.handlePolicyPreset)))
	mux.Handle("GET /v1/policies", a.withOperatorAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		list, err := a.mem.DB().ListOperatorPolicies()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"policies": list})
	})))
	mux.Handle("DELETE /v1/policies/{id}", a.withOperatorAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.PathValue("id"))
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "policy id required"})
			return
		}
		if err := a.mem.DB().RevokeOperatorPolicy(id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		_ = market.SyncPolicyFromDB(a.mem.DB(), a.mkt.Policy())
		a.BroadcastDashboardEvent("policy_revoked")
		writeJSON(w, http.StatusOK, map[string]any{"revoked": id})
	})))
	mux.Handle("POST /v1/auth/pair-code", a.withOperatorAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"code": a.CreateScopedPairCode(true)})
	})))
	mux.Handle("POST /v1/automation/kill", a.withOperatorAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		a.mkt.Policy().SetKillSwitch(in.Enabled)
		a.BroadcastDashboardEvent("kill_switch")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "killSwitch": in.Enabled})
	})))
	mux.Handle("DELETE /v1/market/listings/{shard}", a.withOperatorAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shard := strings.TrimSpace(r.PathValue("shard"))
		if shard == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "shard required"})
			return
		}
		if err := a.mem.DB().DeleteSellListing(shard); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		a.BroadcastDashboardEvent("listing_deleted")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": shard})
	})))
	mux.Handle("PATCH /v1/market/listings/{shard}", a.withOperatorAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shard := strings.TrimSpace(r.PathValue("shard"))
		var in struct {
			Active bool `json:"active"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := a.mem.DB().SetSellListingActive(shard, in.Active); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		a.BroadcastDashboardEvent("listing_updated")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "shard": shard, "active": in.Active})
	})))
	mux.Handle("GET /v1/onboard/hub-bundle", a.withAuth(http.HandlerFunc(a.handleHubBundle)))
	mux.Handle("POST /v1/automation/approvals/{id}", a.withOperatorAuth(http.HandlerFunc(a.handleApprovalDecision)))
	mux.Handle("POST /v1/market/standing", a.withAuth(http.HandlerFunc(a.handleCreateStandingOrder)))
	mux.Handle("GET /v1/market/standing", a.withAuth(http.HandlerFunc(a.handleListStandingOrders)))
	mux.Handle("POST /v1/market/standing/{id}/steer", a.withAuth(http.HandlerFunc(a.handleSteerStandingOrder)))
	mux.Handle("DELETE /v1/market/standing/{id}", a.withAuth(http.HandlerFunc(a.handleCancelStandingOrder)))
	mux.Handle("POST /v1/runtime/invocations/{id}/resume", a.withOperatorAuth(http.HandlerFunc(a.handleResumeInvocation)))
	mux.Handle("POST /v1/runtime/invocations/{id}/restart", a.withOperatorAuth(http.HandlerFunc(a.handleRestartInvocation)))
	mux.Handle("DELETE /v1/runtime/invocations/{id}", a.withOperatorAuth(http.HandlerFunc(a.handleKillInvocation)))

	// 7. L0 Transparent LLM Memory Proxy, MCP OAuth 2.1, OpenAPI, Bundled Adapters & Prometheus Metrics.
	a.registerProxyRoutes(mux)
	a.registerOAuthRoutes(mux)
	a.registerAdapterRoutes(mux)
	a.registerMetricsRoutes(mux)
	a.registerDockAndLaunchRoutes(mux)
	a.registerColdStartRoutes(mux)

	// 8. Embedded Overlay Hub (Restate services are NEVER exposed on the public :18420 mux — dedicated internal h2c port only!).
	if a.cfg.Hub != nil {
		mux.Handle("/hub/", http.StripPrefix("/hub", a.cfg.Hub.Handler()))
	}

	// 9. Svelte 5 Dashboard exact routes (/state, /events, /dashboard/state, /dashboard/events, /).
	mux.Handle("GET /state", a.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.BuildDashboardState(r.Context(), requestBaseURL(r)))
	})))
	mux.Handle("GET /dashboard/state", a.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.BuildDashboardState(r.Context(), requestBaseURL(r)))
	})))
	mux.Handle("GET /events", a.withAuth(http.HandlerFunc(a.handleDashboardSSE)))
	mux.Handle("GET /dashboard/events", a.withAuth(http.HandlerFunc(a.handleDashboardSSE)))

	uiSub, err := fs.Sub(uiDistFS, "ui_dist")
	if err == nil {
		fileSrv := http.FileServer(http.FS(uiSub))
		mux.Handle("GET /assets/", fileSrv)
		mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, _ *http.Request) {
			serveEmbeddedIndex(w, uiSub)
		})
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if r.URL.Path != "/" {
				fileSrv.ServeHTTP(w, r)
				return
			}
			serveEmbeddedIndex(w, uiSub)
		})
	}

	return a.withBrowserOriginGuard(mux)
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
