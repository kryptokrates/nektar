package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lightwebinc/bcommon/payee"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	restateserver "github.com/restatedev/sdk-go/server"

	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/reasoning"
)

//go:embed all:ui_dist
var uiDistFS embed.FS

// Config configures the unified v0.4 HTTP / MCP / A2A / L0-Proxy / Dashboard / Hub / Restate server.
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
	MountRestate      bool // mounts Restate Go SDK service handler under /restate/
	RestateAdminURL   string
	HTTPClient        *http.Client
}

type pairTicket struct {
	token   string
	expires time.Time
}

// App bundles all Nektar v0.4 HTTP interfaces into one net/http Handler.
type App struct {
	mem        *memory.Service
	mkt        *market.Engine
	broker     *market.Broker
	disc       *discovery.Service
	toolbox    *Toolbox
	mcpSrv     *mcp.Server
	cfg        Config
	startedAt  time.Time
	claimsMu   sync.Mutex
	payClaims  payee.Claims
	httpClient *http.Client
	handler    http.Handler

	evMu   sync.Mutex
	evSubs map[chan string]struct{}

	pairMu    sync.Mutex
	pairCodes map[string]pairTicket
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
		evSubs:     make(map[chan string]struct{}),
		pairCodes:  make(map[string]pairTicket),
	}
	app.handler = app.buildMux()
	return app, nil
}

func (a *App) Handler() http.Handler { return a.handler }
func (a *App) MCP() *mcp.Server      { return a.mcpSrv }
func (a *App) Toolbox() *Toolbox     { return a.toolbox }

// BroadcastDashboardEvent wakes connected Svelte Dashboard SSE subscribers immediately.
func (a *App) BroadcastDashboardEvent(kind string) {
	a.evMu.Lock()
	for ch := range a.evSubs {
		select {
		case ch <- kind:
		default:
		}
	}
	a.evMu.Unlock()
}

// CreatePairCode mints a single-use 5-minute pairing code for `GET /pair?code=...`.
func (a *App) CreatePairCode() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	code := hex.EncodeToString(b[:])
	tok := a.cfg.OperatorToken
	if tok == "" {
		tok = a.cfg.APIToken
	}
	a.pairMu.Lock()
	a.pairCodes[code] = pairTicket{token: tok, expires: time.Now().Add(5 * time.Minute)}
	a.pairMu.Unlock()
	return code
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
		"version":     "0.4.0",
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

// BuildDashboardState returns the exact typed `State` JSON expected by `ui/App.svelte`.
func (a *App) BuildDashboardState(ctx context.Context, baseURL string) map[string]any {
	if baseURL == "" {
		baseURL = strings.TrimRight(a.cfg.PublicURL, "/")
	}
	if baseURL == "" {
		baseURL = "http://127.0.0.1:18420"
	}
	addr, _ := a.mem.Chain().FundAddress()
	bal := a.mem.Chain().BalanceSats()
	utxos := a.mem.Chain().UTXOCount()
	shards, _ := a.mem.ListUserShards()
	totalRecs, anchoredRecs, _ := a.mem.DB().CountFragments()
	unproven := totalRecs - anchoredRecs
	coverage := 0.0
	if totalRecs > 0 {
		coverage = float64(anchoredRecs) / float64(totalRecs)
	}

	// Receipts + Seller Earnings feed
	rcps, _ := a.mem.DB().ListReceipts(50)
	deals, _ := a.mem.DB().ListDeals(100)
	listings, _ := a.mem.DB().ListSellListings()
	policies, _ := a.mem.DB().ListOperatorPolicies()
	pendingApprs, _ := a.mem.DB().ListPendingApprovals()

	var earned, earned24h uint64
	cutoff24h := time.Now().Add(-24 * time.Hour).Unix()
	amounts := make([]uint64, 0, len(rcps)+len(deals))
	rcpList := make([]map[string]any, 0, len(rcps))
	for _, r := range rcps {
		tok, _ := reasoning.ReceiptToken(r.ReceiptHash)
		var parsed reasoning.Receipt
		_ = json.Unmarshal(r.BodyJSON, &parsed)
		shSeen := make(map[string]bool)
		var shList []string
		for _, ref := range parsed.FragmentRefs {
			if !shSeen[ref.Shard] {
				shSeen[ref.Shard] = true
				shList = append(shList, ref.Shard)
			}
		}
		var satsVal any = nil
		if parsed.Payment != nil && parsed.Payment.AmountSats > 0 {
			satsVal = parsed.Payment.AmountSats
			earned += parsed.Payment.AmountSats
			if r.CreatedAt >= cutoff24h {
				earned24h += parsed.Payment.AmountSats
			}
			amounts = append(amounts, parsed.Payment.AmountSats)
		}
		rcpList = append(rcpList, map[string]any{
			"seq":    r.ShardSeq,
			"tool":   "groundedAnswer",
			"sats":   satsVal,
			"shards": shList,
			"at":     r.CreatedAt * 1000,
			"url":    "/v1/verify/" + tok,
		})
	}
	for _, d := range deals {
		if d.Role == "seller" && d.Status == "settled" && d.PriceSats > 0 {
			earned += d.PriceSats
			if d.UpdatedAt >= cutoff24h {
				earned24h += d.PriceSats
			}
			amounts = append(amounts, d.PriceSats)
		}
	}

	// Market shards summary
	marketRows := make([]map[string]any, 0, len(listings))
	for _, l := range listings {
		frags, _ := a.mem.DB().ListFragments(l.Shard)
		marketRows = append(marketRows, map[string]any{
			"shard":   l.Shard,
			"domains": l.Domains,
			"records": len(frags),
			"works":   0,
			"earned":  0,
			"offers":  fmt.Sprintf("%d sats", l.PriceSats),
		})
	}

	// Hosts + Peers + Bounties + Liquidity
	envHosts, addedHosts, blockedHosts := a.disc.HostLists()
	if envHosts == nil {
		envHosts = []string{}
	}
	if addedHosts == nil {
		addedHosts = []string{}
	}
	if blockedHosts == nil {
		blockedHosts = []string{}
	}
	addedSet := make(map[string]bool, len(addedHosts))
	for _, u := range addedHosts {
		addedSet[u] = true
	}
	persistedHosts, _ := a.mem.DB().ListHosts()
	hostState := make(map[string]string, len(persistedHosts))
	for _, h := range persistedHosts {
		hostState[h.URL] = h.State
	}
	activeHostURLs := a.disc.ActiveHosts()
	hostRows := make([]map[string]any, 0, len(activeHostURLs))
	for _, u := range activeHostURLs {
		st := hostState[u]
		if st == "" {
			st = "up"
		}
		hostRows = append(hostRows, map[string]any{
			"url":   u,
			"state": st,
			"saved": addedSet[u],
		})
	}
	peers := a.disc.Peers()
	if peers == nil {
		peers = []discovery.PeerInfo{}
	}
	bounties := a.toolbox.fetchHubBounties(ctx, activeHostURLs)
	if bounties == nil {
		bounties = []hub.Bounty{}
	}
	var liquidity map[string]any
	if a.broker != nil {
		liquidity = a.broker.LiquidityMetrics()
	}

	// Policies formatted for Settings -> Policy tab
	policyRows := make([]map[string]any, 0, len(policies))
	hasActivePolicy := false
	for _, p := range policies {
		if p.Active {
			hasActivePolicy = true
		}
		row := map[string]any{
			"policyHash": p.PolicyHash,
			"policyId":   p.PolicyID,
			"approvedAt": maxI64(0, time.Now().Unix()-p.ApprovedAt),
			"approvedBy": p.ApprovedBy,
			"active":     p.Active,
			"policy": map[string]any{
				"roles":   p.Roles,
				"domains": p.Domains,
				"limits": map[string]any{
					"maxPerDeal":  p.MaxPerDeal,
					"maxPerDay":   p.MaxPerDay,
					"zeroConfMax": p.ZeroConfMax,
				},
			},
		}
		if p.RevokedAt > 0 {
			row["revokedAt"] = maxI64(0, time.Now().Unix()-p.RevokedAt)
		}
		policyRows = append(policyRows, row)
	}

	// Automation panel (Approvals + Broker ops + Deal journal)
	pendingRows := make([]map[string]any, 0, len(pendingApprs))
	for _, p := range pendingApprs {
		pendingRows = append(pendingRows, map[string]any{
			"id":           p.ID,
			"reason":       p.Reason,
			"maxExtraSats": p.MaxExtraSats,
			"operationId":  p.DealID,
			"expiresAt":    time.Unix(p.ExpiresAt, 0).UTC().Format(time.RFC3339),
		})
	}
	journal := make([]map[string]any, 0, len(deals))
	for i, d := range deals {
		if i >= 12 {
			break
		}
		cause := d.Error
		if cause == "" {
			cause = fmt.Sprintf("%s · %d sats", d.Role, d.PriceSats)
		}
		journal = append(journal, map[string]any{
			"op":    d.DealID,
			"at":    d.UpdatedAt * 1000,
			"to":    d.Status,
			"cause": cause,
		})
	}
	ops := []map[string]any{}
	if a.broker != nil {
		ops = append(ops,
			map[string]any{"role": "broker", "kind": "sse-market-feed", "state": "running"},
			map[string]any{"role": "broker", "kind": "nat-bbox-relay", "state": "running"},
		)
		if len(listings) > 0 {
			ops = append(ops, map[string]any{"role": "seller", "kind": fmt.Sprintf("auto-quote (%d shard)", len(listings)), "state": "running"})
		}
	}

	steps := []map[string]any{
		{"name": "memory seeded", "done": totalRecs > 0},
		{"name": "receipt issued", "done": len(rcps) > 0},
		{"name": "wallet funded", "done": bal > 0},
		{"name": "policy approved", "done": hasActivePolicy},
		{"name": "network joined", "done": len(activeHostURLs) > 0},
	}
	doneCount := 0
	nextStep := "trade & earn"
	for _, st := range steps {
		if st["done"].(bool) {
			doneCount++
		} else if nextStep == "trade & earn" {
			nextStep = st["name"].(string)
		}
	}

	var adminURL any = nil
	if a.cfg.RestateAdminURL != "" {
		adminURL = a.cfg.RestateAdminURL
	}
	var pushURL any = nil
	if a.broker != nil && a.broker.PublicURL() != "" {
		pushURL = a.broker.PublicURL()
	}

	return map[string]any{
		"network":   a.mem.Chain().Network(),
		"model":     "nektar/v0.4-go",
		"pub":       shortStr(a.mem.Chain().IdentityPubHex(), 20),
		"uptimeSec": int64(time.Since(a.startedAt).Seconds()),
		"loop": map[string]any{
			"steps":    steps,
			"done":     doneCount,
			"frontier": fmt.Sprintf("%d/%d", doneCount, len(steps)),
			"next":     nextStep,
		},
		"treasury": map[string]any{
			"address":  addr,
			"sats":     bal,
			"utxos":    utxos,
			"funded":   bal > 0,
			"payments": "BRC-105 + AtomicBEEF",
		},
		"receipts": map[string]any{
			"count":     len(rcps),
			"earned":    earned,
			"earned24h": earned24h,
			"amounts":   amounts,
			"list":      rcpList,
		},
		"memory": map[string]any{
			"shards":   len(shards),
			"records":  totalRecs,
			"anchored": anchoredRecs,
			"unproven": unproven,
			"coverage": coverage,
			"prover":   "SPV + Rúnar + Mangle",
		},
		"market": marketRows,
		"net": map[string]any{
			"hosts":     hostRows,
			"seeds":     discovery.DefaultSeedHosts(a.mem.Chain().Network()),
			"slots":     map[string]int{"peers": len(peers), "bounties": len(bounties), "listings": len(listings)},
			"boot":      "dir-v1",
			"push":      pushURL,
			"peers":     peers,
			"bounties":  bounties,
			"liquidity": liquidity,
		},
		"automation": map[string]any{
			"pending": pendingRows,
			"ops":     ops,
			"rt": map[string]any{
				"executor": "go-restate-sdk",
				"adminUrl": adminURL,
				"pending":  []any{},
				"nextAt":   nil,
				"journal":  journal,
			},
		},
		"share": map[string]any{
			"enabled": true,
			"url":     baseURL + "/card",
		},
		"settings": map[string]any{
			"policies": policyRows,
			"hosts": map[string]any{
				"env":     envHosts,
				"added":   addedHosts,
				"blocked": blockedHosts,
			},
		},
	}
}

func (a *App) buildMux() http.Handler {
	mux := http.NewServeMux()

	// 1. Official MCP Streamable HTTP transport (/mcp).
	mcpHandler := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
		return a.mcpSrv
	}, &mcp.StreamableHTTPOptions{
		Stateless:                  true,
		DisableLocalhostProtection: true,
	})
	mux.Handle("/mcp", a.withAuth(mcpHandler))

	// 2. Auth session & 1-click pairing (/v1/auth/session, /pair).
	mux.HandleFunc("POST /v1/auth/session", a.handleAuthSession)
	mux.HandleFunc("GET /pair", a.handlePair)

	// 3. Health, discovery, public card & A2A Agent Card.
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"version": "0.4.0",
			"network": a.mem.Chain().Network(),
		})
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, BuildStatus(r.Context(), a.mem, a.mkt))
	})
	mux.HandleFunc("GET /.well-known/nektar", func(w http.ResponseWriter, r *http.Request) {
		shards, _ := a.mem.ListUserShards()
		resp := map[string]any{
			"version":     "0.4.0",
			"identityPub": a.mem.Chain().IdentityPubHex(),
			"network":     a.mem.Chain().Network(),
			"packBase":    "/v1/pack/",
			"verifyBase":  "/v1/verify/",
			"mcp":         "/mcp",
			"a2a":         "/a2a",
			"shards":      shards,
			"hosts":       a.disc.ActiveHosts(),
		}
		if a.cfg.Hub != nil {
			resp["hub"] = "/hub"
		}
		writeJSON(w, http.StatusOK, resp)
	})
	mux.HandleFunc("GET /.well-known/agent.json", a.handleAgentCard)
	mux.HandleFunc("POST /a2a", a.handleA2A)
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
	mux.HandleFunc("GET /v1/pack/{shard}", a.handleShardPack)
	mux.HandleFunc("GET /v1/chirp/{hash}", a.handleChirpObject)
	mux.HandleFunc("GET /v1/wallet", func(w http.ResponseWriter, _ *http.Request) {
		addr, _ := a.mem.Chain().FundAddress()
		spentToday, maxDay, maxFee, txCount := a.mem.Chain().SpendGuard().Stats()
		writeJSON(w, http.StatusOK, map[string]any{
			"network":     a.mem.Chain().Network(),
			"identityPub": a.mem.Chain().IdentityPubHex(),
			"fundAddress": addr,
			"balanceSats": a.mem.Chain().BalanceSats(),
			"utxos":       a.mem.Chain().UTXOCount(),
			"spendGuard": map[string]any{
				"spentToday": spentToday,
				"maxPerDay":  maxDay,
				"maxFee":     maxFee,
				"txCount":    txCount,
			},
		})
	})

	// 6. Live A2A Market Purchase, Host Discovery, Onboarding, Faucet & Approvals!
	mux.HandleFunc("POST /v1/market/purchase", a.handleMarketPurchase)
	mux.Handle("POST /v1/hosts", a.withOperatorAuth(http.HandlerFunc(a.handleJoinHost)))
	mux.Handle("DELETE /v1/hosts", a.withOperatorAuth(http.HandlerFunc(a.handleLeaveHost)))
	mux.Handle("POST /v1/onboard/faucet", a.withAuth(http.HandlerFunc(a.handleOnboardFaucet)))
	mux.HandleFunc("POST /v1/faucet", a.handlePublicFaucet)
	mux.Handle("POST /v1/onboard/policy-preset", a.withOperatorAuth(http.HandlerFunc(a.handlePolicyPreset)))
	mux.Handle("GET /v1/onboard/hub-bundle", a.withAuth(http.HandlerFunc(a.handleHubBundle)))
	mux.Handle("POST /v1/automation/approvals/{id}", a.withOperatorAuth(http.HandlerFunc(a.handleApprovalDecision)))

	// 7. L0 Transparent LLM Memory Proxy (OpenAI & Anthropic drop-in!).
	mux.Handle("POST /v1/proxy/openai/v1/chat/completions", a.withAuth(http.HandlerFunc(a.handleOpenAIProxy)))
	mux.Handle("POST /v1/proxy/anthropic/v1/messages", a.withAuth(http.HandlerFunc(a.handleAnthropicProxy)))

	// 8. Embedded Overlay Hub & Restate Services.
	if a.cfg.Hub != nil {
		mux.Handle("/hub/", http.StripPrefix("/hub", a.cfg.Hub.Handler()))
	}
	if a.cfg.MountRestate && a.mkt != nil {
		if rh, err := a.mkt.BindRestate(restateserver.NewRestate()).Handler(); err == nil {
			mux.Handle("/restate/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r2 := r.Clone(r.Context())
				r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/restate")
				if r2.URL.Path == "" {
					r2.URL.Path = "/"
				}
				r2.RequestURI = r2.URL.RequestURI()
				rh.ServeHTTP(w, r2)
			}))
		}
	}

	// 9. Svelte 5 Dashboard exact routes (/state, /events, /dashboard/state, /dashboard/events, /).
	mux.Handle("GET /state", a.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.BuildDashboardState(r.Context(), "http://"+r.Host))
	})))
	mux.Handle("GET /dashboard/state", a.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.BuildDashboardState(r.Context(), "http://"+r.Host))
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

	return mux
}

func (a *App) handleDashboardSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan string, 16)
	a.evMu.Lock()
	a.evSubs[ch] = struct{}{}
	a.evMu.Unlock()
	defer func() {
		a.evMu.Lock()
		delete(a.evSubs, ch)
		close(ch)
		a.evMu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "data: {\"event\":\"connected\"}\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case kind, ok := <-ch:
			if !ok {
				return
			}
			_, _ = fmt.Fprintf(w, "data: {\"event\":%q}\n\n", kind)
			flusher.Flush()
		case <-keepalive.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (a *App) tokenMatches(got string, requireOperator bool) bool {
	if got == "" {
		return false
	}
	if a.cfg.OperatorToken != "" && subtle.ConstantTimeCompare([]byte(got), []byte(a.cfg.OperatorToken)) == 1 {
		return true
	}
	if !requireOperator && a.cfg.APIToken != "" && subtle.ConstantTimeCompare([]byte(got), []byte(a.cfg.APIToken)) == 1 {
		return true
	}
	return false
}

func (a *App) extractToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	if c, err := r.Cookie("nektar_session"); err == nil && c.Value != "" {
		return strings.TrimSpace(c.Value)
	}
	return ""
}

func (a *App) withAuth(next http.Handler) http.Handler {
	if a.cfg.APIToken == "" && a.cfg.OperatorToken == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.tokenMatches(a.extractToken(r), false) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) withOperatorAuth(next http.Handler) http.Handler {
	if a.cfg.OperatorToken == "" && a.cfg.APIToken == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		needOp := a.cfg.OperatorToken != ""
		if !a.tokenMatches(a.extractToken(r), needOp) {
			code := http.StatusUnauthorized
			if needOp && a.tokenMatches(a.extractToken(r), false) {
				code = http.StatusForbidden
			}
			writeJSON(w, code, map[string]string{"error": "operator token required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if (a.cfg.APIToken != "" || a.cfg.OperatorToken != "") && !a.tokenMatches(in.Token, false) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "nektar_session",
		Value:    in.Token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 7,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handlePair(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	a.pairMu.Lock()
	tk, ok := a.pairCodes[code]
	if ok {
		delete(a.pairCodes, code)
	}
	a.pairMu.Unlock()
	if !ok || time.Now().After(tk.expires) {
		http.Error(w, "invalid or expired pairing code", http.StatusUnauthorized)
		return
	}
	if tk.token != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     "nektar_session",
			Value:    tk.token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   86400 * 7,
		})
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *App) handleToolCall(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	res, err := a.toolbox.Call(r.Context(), name, raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if name == "remember" || name == "groundedAnswer" || name == "want" || name == "sell" || name == "anchor" {
		a.BroadcastDashboardEvent("tool:" + name)
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleMarketPurchase(w http.ResponseWriter, r *http.Request) {
	if a.broker == nil {
		writeJSON(w, http.StatusServiceUnavailable, market.PurchaseResponse{OK: false, Error: "market broker not attached"})
		return
	}
	var req market.PurchaseRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, market.PurchaseResponse{OK: false, Error: err.Error()})
		return
	}
	resp, err := a.broker.HandlePurchase(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, market.PurchaseResponse{OK: false, OfferID: req.OfferID, Error: err.Error()})
		return
	}
	a.BroadcastDashboardEvent("deal_settled")
	writeJSON(w, http.StatusOK, resp)
}

func (a *App) handleJoinHost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	hubURL, discovered, err := a.disc.JoinHost(r.Context(), in.URL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if a.broker != nil {
		_, _ = a.broker.AnnounceSelf(r.Context())
	}
	a.BroadcastDashboardEvent("host_joined")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": hubURL, "discovered": discovered})
}

func (a *App) handleLeaveHost(w http.ResponseWriter, r *http.Request) {
	u := strings.TrimSpace(r.URL.Query().Get("url"))
	if err := a.disc.LeaveHost(u); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("host_left")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": u})
}

func (a *App) handleOnboardFaucet(w http.ResponseWriter, r *http.Request) {
	addr, err := a.mem.Chain().FundAddress()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	txid, sats, err := a.toolbox.requestFaucet(r.Context(), addr, "")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("faucet")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "txid": txid, "sats": sats, "address": addr})
}

func (a *App) handlePublicFaucet(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Hub == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "faucet is only served on Hub nodes"})
		return
	}
	var in struct {
		Address string `json:"address"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	txid, sats, err := hub.DripFaucet(r.Context(), a.mem, in.Address, 2000, 10_000)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "txid": txid, "sats": sats})
}

func (a *App) handlePolicyPreset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Preset  string   `json:"preset"`
		Domains []string `json:"domains,omitempty"`
		Confirm string   `json:"confirm,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if a.mem.Chain().Mainnet() && in.Confirm != "I_UNDERSTAND" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mainnet policy approval requires confirm=\"I_UNDERSTAND\""})
		return
	}
	rec, err := market.ApprovePolicyPreset(a.mem.DB(), a.mkt.Policy(), in.Preset, in.Domains, "dashboard")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("policy_approved")
	writeJSON(w, http.StatusOK, rec)
}

func (a *App) handleHubBundle(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "auto-ip"
	}
	net := r.URL.Query().Get("network")
	if net == "" {
		net = "testnet"
	}
	domain := strings.TrimSpace(r.URL.Query().Get("domain"))
	if domain == "" {
		domain = "hub.example.com"
	}
	cliCmd := fmt.Sprintf("NEKTAR_NETWORK=%s NEKTAR_EMBED_HUB=1 nektar serve --hub --listen :18420", net)
	vpsCmd := fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh && NEKTAR_NETWORK=%s nektar serve --hub --listen :18420", net)
	if mode == "domain" {
		cliCmd = fmt.Sprintf("NEKTAR_NETWORK=%s NEKTAR_PUBLIC_URL=https://%s NEKTAR_EMBED_HUB=1 nektar serve --hub --listen :18420", net, domain)
	} else if mode == "tunnel" {
		cliCmd = fmt.Sprintf("NEKTAR_NETWORK=%s nektar serve --hub --listen :18420 & cloudflared tunnel --url http://127.0.0.1:18420", net)
	}
	cloudInit := fmt.Sprintf(`#cloud-config
runcmd:
  - curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh
  - NEKTAR_NETWORK=%s NEKTAR_EMBED_HUB=1 nohup /usr/local/bin/nektar serve --hub --listen :18420 >/var/log/nektar.log 2>&1 &
`, net)
	writeJSON(w, http.StatusOK, map[string]any{
		"cliCommand":         cliCmd,
		"vpsOneLiner":        vpsCmd,
		"cloudInitYaml":      cloudInit,
		"coolifyTemplateUrl": "https://github.com/kryptokrates/nektar/blob/master/deploy/hub/templates/coolify.compose.yaml",
		"helmCommand":        "helm install nektar-hub https://github.com/kryptokrates/nektar/tree/master/deploy/hub/helm",
	})
}

func (a *App) handleApprovalDecision(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in struct {
		Approve bool `json:"approve"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if a.broker == nil {
		appr, err := a.mem.DB().DecideApproval(id, in.Approve)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		a.BroadcastDashboardEvent("approval_decided")
		writeJSON(w, http.StatusOK, map[string]any{"approval": appr})
		return
	}
	appr, deal, err := a.broker.ApproveAndExecute(r.Context(), id, in.Approve)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("approval_decided")
	writeJSON(w, http.StatusOK, map[string]any{"approval": appr, "deal": deal})
}

func (a *App) handlePublicCard(w http.ResponseWriter, r *http.Request) {
	st := a.BuildDashboardState(r.Context(), "http://"+r.Host)
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, http.StatusOK, st)
		return
	}
	rcps := st["receipts"].(map[string]any)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Nektar Agent Card</title>
<style>body{font-family:system-ui,sans-serif;max-width:680px;margin:2rem auto;padding:1rem;background:#120e0c;color:#ece7e4}
.card{background:#211b17;border:1px solid #3a2f28;border-radius:14px;padding:1.5rem}
h1{color:#f7991b;margin-top:0}code{color:#f7991b}</style></head><body>
<div class="card"><h1>Nektar v0.4 Verifiable Agent</h1>
<p><strong>Identity:</strong> <code>%s</code></p>
<p><strong>Network:</strong> %s</p>
<p><strong>Checkable Receipts:</strong> %v · <strong>Earned:</strong> %v sats</p>
<p><small>%s</small></p></div></body></html>`,
		html.EscapeString(a.mem.Chain().IdentityPubHex()),
		html.EscapeString(a.mem.Chain().Network()),
		rcps["count"], rcps["earned"], html.EscapeString(reasoning.TruthNote),
	)
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

// BRC105PaymentHeader is the JSON payload accepted in `X-BSV-Payment` for HTTP 402 paid routes,
// validated for replay/double-spend conflicts via `lightwebinc/bcommon/payee.Claims`.
type BRC105PaymentHeader struct {
	TxID       string   `json:"txid"`
	AmountSats uint64   `json:"amountSats"`
	Inputs     []string `json:"inputs"`
}

func (a *App) withPaywall(toolName string, next http.Handler) http.Handler {
	price := a.cfg.ToolPrices[toolName]
	if price == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := strings.TrimSpace(r.Header.Get("X-BSV-Payment"))
		addr, _ := a.mem.Chain().FundAddress()
		if hdr == "" {
			w.Header().Set("X-BSV-Payment-Amount", strconv.FormatUint(price, 10))
			w.Header().Set("X-BSV-Payment-Destination", addr)
			w.Header().Set("X-BSV-Payment-Protocol", "BRC-105")
			writeJSON(w, http.StatusPaymentRequired, map[string]any{
				"error":       "payment_required",
				"tool":        toolName,
				"amountSats":  price,
				"destination": addr,
			})
			return
		}
		var pay BRC105PaymentHeader
		if err := json.Unmarshal([]byte(hdr), &pay); err != nil || pay.TxID == "" || pay.AmountSats < price {
			writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "invalid or insufficient X-BSV-Payment"})
			return
		}
		a.claimsMu.Lock()
		claim := a.payClaims.Claim(pay.TxID, pay.Inputs)
		a.claimsMu.Unlock()

		if claim != payee.Accepted {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "payment_rejected",
				"reason": claim.String(),
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) handleRemember(w http.ResponseWriter, r *http.Request) {
	var in memory.RememberInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	res, err := a.mem.Remember(r.Context(), in)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("remember")
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleRecall(w http.ResponseWriter, r *http.Request) {
	var in memory.RecallInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	res, err := a.toolbox.ExecRecall(r.Context(), in)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleAnchor(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Shard string `json:"shard"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if in.Shard == "" {
		in.Shard = "default"
	}
	res, err := a.mem.AnchorShard(r.Context(), in.Shard)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("anchor")
	writeJSON(w, http.StatusOK, map[string]any{"anchor": res})
}

func (a *App) handleAnswer(w http.ResponseWriter, r *http.Request) {
	var in memory.GroundedAnswerInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	res, err := a.mem.GroundedAnswer(r.Context(), in)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("receipt")
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleVerifyReceipt(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimSpace(r.PathValue("token"))
	if tok == "" {
		tok = strings.TrimSpace(r.URL.Query().Get("r"))
	}
	if tok == "" {
		tok = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	ver, err := a.mem.VerifyReceipt(r.Context(), tok)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/html") && !strings.Contains(r.Header.Get("Accept"), "application/json") {
		color := "#dc2626"
		switch ver.Verdict {
		case "green":
			color = "#16a34a"
		case "amber":
			color = "#d97706"
		}
		answer := ""
		if ver.Receipt != nil {
			answer = ver.Receipt.Answer
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Nektar Receipt Verification</title>
<style>body{font-family:system-ui,sans-serif;max-width:720px;margin:2rem auto;padding:1rem;background:#0b0f17;color:#e5e7eb}
.badge{display:inline-block;padding:.35rem .8rem;border-radius:999px;font-weight:700;text-transform:uppercase;background:%s;color:#fff}
.card{background:#111827;border:1px solid #1f2937;border-radius:12px;padding:1.25rem;margin-top:1rem}
code{color:#93c5fd;word-break:break-all}</style></head><body>
<h1>Nektar Receipt Verifier</h1><span class="badge">%s</span>
<div class="card"><p><strong>Status:</strong> %s</p><p><strong>Token:</strong> <code>%s</code></p>
<p><strong>Answer:</strong> %s</p><p><strong>Citations:</strong> %d checked / %d mined</p>
<p><small>%s</small></p></div></body></html>`,
			color, html.EscapeString(ver.Verdict), html.EscapeString(ver.Reason),
			html.EscapeString(ver.ReceiptToken), html.EscapeString(answer),
			ver.CitationsChecked, ver.CitationsMined, html.EscapeString(ver.TruthNote),
		)
		return
	}
	writeJSON(w, http.StatusOK, ver)
}

func (a *App) handleShardPack(w http.ResponseWriter, r *http.Request) {
	shard := r.PathValue("shard")
	res, err := a.mem.ExportShardPack(r.Context(), shard)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleChirpObject(w http.ResponseWriter, r *http.Request) {
	rawHex := r.PathValue("hash")
	b, err := hex.DecodeString(rawHex)
	if err != nil || len(b) != 32 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid 32-byte CHIRP object hash"})
		return
	}
	var h [32]byte
	copy(h[:], b)
	obj, err := a.mem.Objects().GetObject(h)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "object not found"})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(obj)
}

func (a *App) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	base := a.cfg.PublicURL
	if base == "" {
		base = "http://" + r.Host
	}
	// Note: name == "nektar" matches integrations/hermes auto-discovery probe!
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        "nektar",
		"displayName": "Nektar Verifiable Memory Agent",
		"description": "BSV-anchored, BRC-369 encrypted, Cedar-governed, Google Mangle grounded agent memory node.",
		"version":     "0.4.0",
		"url":         base + "/a2a",
		"identityPub": a.mem.Chain().IdentityPubHex(),
		"capabilities": map[string]bool{
			"streaming":               true,
			"verifiableReceipts":      true,
			"paidKnowledgeDisclosure": true,
		},
		"skills": []map[string]string{
			{"id": "memory.recall", "name": "Verified Recall", "description": "Recall SPV-verified, Cedar-authorized fragments."},
			{"id": "memory.answer", "name": "Grounded Answer", "description": "Produce Mangle-grounded answers with nektarr1.* receipts."},
			{"id": "memory.verify", "name": "Verify Receipt", "description": "Verify a nektarr1.* receipt offline."},
			{"id": "market.want", "name": "Buy Verified Knowledge", "description": "Buy BRC-369 disclosed knowledge over AtomicBEEF."},
			{"id": "market.sell", "name": "Sell Verified Knowledge", "description": "Sell anchored shards to peers or Hub bounties."},
		},
	})
}

func (a *App) handleA2A(w http.ResponseWriter, r *http.Request) {
	var rpc struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&rpc); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": err.Error()}})
		return
	}
	methodMap := map[string]string{
		"memory/recall":   "recall",
		"memory/remember": "remember",
		"memory/answer":   "groundedAnswer",
		"memory/verify":   "verifyReceipt",
		"market/want":     "want",
		"market/sell":     "sell",
		"market/deals":    "deals",
		"market/peers":    "peers",
	}
	toolName := methodMap[rpc.Method]
	if toolName == "" {
		toolName = rpc.Method
	}
	result, callErr := a.toolbox.Call(r.Context(), toolName, rpc.Params)
	if callErr != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0",
			"id":      rpc.ID,
			"error":   map[string]any{"code": -32601, "message": callErr.Error()},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
}

func (a *App) handleOpenAIProxy(w http.ResponseWriter, r *http.Request) {
	shard := r.Header.Get("X-Nektar-Shard")
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	memBlock := a.buildMemoryContext(r.Context(), shard)
	if memBlock != "" {
		msgs, _ := payload["messages"].([]any)
		injected := append([]any{map[string]any{
			"role":    "system",
			"content": memBlock,
		}}, msgs...)
		payload["messages"] = injected
		raw, _ = json.Marshal(payload)
	}
	a.forwardProxy(w, r, strings.TrimRight(a.cfg.OpenAIUpstream, "/")+"/v1/chat/completions", raw)
}

func (a *App) handleAnthropicProxy(w http.ResponseWriter, r *http.Request) {
	shard := r.Header.Get("X-Nektar-Shard")
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	memBlock := a.buildMemoryContext(r.Context(), shard)
	if memBlock != "" {
		existing, _ := payload["system"].(string)
		if existing != "" {
			payload["system"] = memBlock + "\n\n" + existing
		} else {
			payload["system"] = memBlock
		}
		raw, _ = json.Marshal(payload)
	}
	a.forwardProxy(w, r, strings.TrimRight(a.cfg.AnthropicUpstream, "/")+"/v1/messages", raw)
}

func (a *App) buildMemoryContext(ctx context.Context, shard string) string {
	_, frags, err := a.mem.RecallAllShards(ctx, memory.RecallInput{Shard: shard, Limit: 20})
	if err != nil || len(frags) == 0 {
		return ""
	}
	var sb strings.Builder
	label := shard
	if label == "" {
		label = "all shards"
	}
	sb.WriteString("[Nektar Verified Memory — " + label + "]\n")
	for _, f := range frags {
		sb.WriteString(fmt.Sprintf("- [%s#%d] %s\n", f.Shard, f.Seq, f.Text))
	}
	return sb.String()
}

func (a *App) forwardProxy(w http.ResponseWriter, orig *http.Request, targetURL string, body []byte) {
	req, err := http.NewRequestWithContext(orig.Context(), http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for _, h := range []string{"Authorization", "x-api-key", "anthropic-version"} {
		if v := orig.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 16<<20))
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
