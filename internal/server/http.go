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
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
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
	Hub               *hub.Node // optional embedded Go overlay hub mounted under /hub/
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

// App bundles all Nektar v0.4 HTTP interfaces into one net/http Handler.
type App struct {
	mem        *memory.Service
	mkt        *market.Engine
	broker     *market.Broker
	disc       *discovery.Service
	toolbox    *Toolbox
	mcpSrv     *mcp.Server
	cfg            Config
	startedAt      time.Time
	httpClient     *http.Client
	proxyTransport *http.Transport
	handler        http.Handler

	evMu   sync.Mutex
	evSubs map[chan string]struct{}

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
		evSubs:       make(map[chan string]struct{}),
		pairCodes:    make(map[string]pairTicket),
		faucetIPs:    make(map[string][]int64),
		oauthRegIPs:  make(map[string][]int64),
		oauthClients: make(map[string]oauthClient),
		oauthCodes:   make(map[string]oauthCode),
		oauthTokens:  make(map[string]oauthAccessToken),
		stopCh:       make(chan struct{}),
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

// CreatePairCode mints a single-use 5-minute operator pairing code for `GET /pair?code=...`.
// Must only be called from startup banner or operator-authenticated routes.
func (a *App) CreatePairCode() string {
	return a.CreateScopedPairCode(true)
}

// CreateScopedPairCode mints a single-use 5-minute pairing code explicitly scoped to operator or API privileges.
func (a *App) CreateScopedPairCode(forOperator bool) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("server: crypto/rand failed: " + err.Error())
	}
	code := hex.EncodeToString(b[:])
	clear(b[:])
	tok := a.cfg.APIToken
	if forOperator && a.cfg.OperatorToken != "" {
		tok = a.cfg.OperatorToken
	}
	now := time.Now()
	key := oauthSecretKey(code)
	a.pairMu.Lock()
	for k, v := range a.pairCodes {
		if now.After(v.expires) {
			delete(a.pairCodes, k)
		}
	}
	for len(a.pairCodes) >= 256 {
		for k := range a.pairCodes {
			delete(a.pairCodes, k)
			break
		}
	}
	a.pairCodes[key] = pairTicket{token: tok, expires: now.Add(5 * time.Minute)}
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
	balBreakdown, _ := a.mem.Chain().Balance(ctx)
	bal := balBreakdown.SpendableSats
	utxos := balBreakdown.SpendableCount
	feeRate := a.mem.Chain().FeeRateSatPerKB(ctx)
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

	// Market shards summary (real settled works & sats per shard)
	marketRows := make([]map[string]any, 0, len(listings))
	for _, l := range listings {
		frags, _ := a.mem.DB().ListFragments(l.Shard)
		works, shardEarned, _ := a.mem.DB().SellerShardStats(l.Shard)
		marketRows = append(marketRows, map[string]any{
			"shard":   l.Shard,
			"domains": l.Domains,
			"records": len(frags),
			"works":   works,
			"earned":  shardEarned,
			"active":  l.Active,
			"offers":  fmt.Sprintf("%d sats", l.PriceSats),
		})
	}
	unmatchedWants, _ := a.mem.DB().ListUnmatchedWants(20)
	if unmatchedWants == nil {
		unmatchedWants = []store.UnmatchedWantRecord{}
	}
	withholdings := a.disc.Withholdings()
	if withholdings == nil {
		withholdings = []discovery.WithholdingWarning{}
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
		policyBody := map[string]any{
			"roles":   p.Roles,
			"domains": p.Domains,
			"limits": map[string]any{
				"maxPerDeal":  p.MaxPerDeal,
				"maxPerDay":   p.MaxPerDay,
				"zeroConfMax": p.ZeroConfMax,
			},
		}
		if strings.TrimSpace(p.AIPolicyJSON) != "" {
			var aiPol market.AIMatchingPolicy
			if json.Unmarshal([]byte(p.AIPolicyJSON), &aiPol) == nil {
				policyBody["aiMatching"] = aiPol
			}
		}
		row := map[string]any{
			"policyHash": p.PolicyHash,
			"policyId":   p.PolicyID,
			"approvedAt": maxI64(0, time.Now().Unix()-p.ApprovedAt),
			"approvedBy": p.ApprovedBy,
			"active":     p.Active,
			"policy":     policyBody,
		}
		if p.RevokedAt > 0 {
			row["revokedAt"] = maxI64(0, time.Now().Unix()-p.RevokedAt)
		}
		policyRows = append(policyRows, row)
	}

	// Automation panel (Approvals + Broker ops + REAL Restate durable execution telemetry — F26).
	pendingRows := make([]map[string]any, 0, len(pendingApprs))
	for _, p := range pendingApprs {
		pendingRows = append(pendingRows, map[string]any{
			"id":           p.ID,
			"reason":       p.Reason,
			"maxExtraSats": p.MaxExtraSats,
			"operationId":  p.DealID,
			"awakeableId":  p.AwakeableID,
			"expiresAt":    time.Unix(p.ExpiresAt, 0).UTC().Format(time.RFC3339),
		})
	}
	dealFeed := make([]map[string]any, 0, len(deals))
	for i, d := range deals {
		if i >= 12 {
			break
		}
		cause := d.Error
		if cause == "" {
			cause = fmt.Sprintf("%s · %d sats", d.Role, d.PriceSats)
		}
		entry := map[string]any{
			"op":    d.DealID,
			"at":    d.UpdatedAt * 1000,
			"to":    d.Status,
			"cause": cause,
		}
		if len(d.MatchAuditJSON) > 0 {
			var ma market.MatchAudit
			if json.Unmarshal(d.MatchAuditJSON, &ma) == nil {
				entry["matchAudit"] = ma
			}
		}
		dealFeed = append(dealFeed, entry)
	}
	rtExecutor := "unattached"
	rtPending := make([]map[string]any, 0)
	rtJournal := make([]map[string]any, 0)
	var rtNextAt any = nil
	if a.cfg.RestateClient != nil {
		if invs, qErr := a.cfg.RestateClient.QueryInvocations(ctx, 25); qErr == nil {
			rtExecutor = "restate"
			var earliestNext int64
			for _, inv := range invs {
				stLower := strings.ToLower(inv.Status)
				isActive := stLower == "pending" || stLower == "scheduled" || stLower == "running" || stLower == "suspended" || stLower == "backing-off" || stLower == "paused" || stLower == "ready"
				if isActive {
					nextAt := inv.NextRetryAt
					if nextAt == 0 {
						nextAt = inv.ModifiedAt
					}
					if nextAt > 0 && (earliestNext == 0 || nextAt < earliestNext) {
						earliestNext = nextAt
					}
					rtPending = append(rtPending, map[string]any{
						"kind":        inv.Target,
						"operationId": inv.ID,
						"status":      inv.Status,
						"attempts":    inv.RetryCount,
						"nextAt":      nextAt,
						"lastError":   inv.LastFailureErr,
					})
				}
				cause := inv.LastFailureErr
				if cause == "" {
					cause = inv.ID
				}
				rtJournal = append(rtJournal, map[string]any{
					"op":    inv.Target,
					"at":    inv.ModifiedAt,
					"to":    inv.Status,
					"cause": cause,
				})
			}
			if earliestNext > 0 {
				rtNextAt = earliestNext
			}
		} else {
			rtExecutor = "restate-unavailable"
		}
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
			"address":         addr,
			"identityPub":     a.mem.Chain().IdentityPubHex(),
			"sats":            bal,
			"confirmedSats":   balBreakdown.ConfirmedSats,
			"unprovenSats":    balBreakdown.UnprovenSats,
			"reservedSats":    balBreakdown.ReservedSats,
			"utxos":           utxos,
			"fuelUtxos":       balBreakdown.FuelCount,
			"reserveUtxos":    balBreakdown.ReserveCount,
			"defaultUtxos":    balBreakdown.DefaultCount,
			"walletMode":      a.mem.Chain().WalletMode(),
			"feeRateSatPerKB": feeRate,
			"funded":          bal > 0,
			"payments":        "BRC-100 + BRC-29 + AtomicBEEF",
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
			"peers":          peers,
			"bounties":       bounties,
			"liquidity":      liquidity,
			"unmatchedWants": unmatchedWants,
			"withholdings":   withholdings,
		},
		"automation": map[string]any{
			"pending": pendingRows,
			"ops":     ops,
			"deals":   dealFeed,
			"rt": map[string]any{
				"executor": rtExecutor,
				"adminUrl": adminURL,
				"pending":  rtPending,
				"nextAt":   rtNextAt,
				"journal":  rtJournal,
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

	// 3. Health, discovery, public card & A2A Agent Card.
	healthHandler := func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"version": "0.4.0",
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
			"version":     "0.4.0",
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

	// 7. L0 Transparent LLM Memory Proxy, MCP OAuth 2.1, OpenAPI, Bundled Adapters & Prometheus Metrics.
	a.registerProxyRoutes(mux)
	a.registerOAuthRoutes(mux)
	a.registerAdapterRoutes(mux)
	a.registerMetricsRoutes(mux)

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
		case <-a.stopCh:
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
	return a.matchOAuthToken(got, requireOperator)
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.APIToken == "" && a.cfg.OperatorToken == "" {
			// Unauthenticated bare-metal loopback: local owner context.
			ctx := WithCallerAuth(r.Context(), CallerAuth{Authenticated: true, IsOperator: true})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		tok := a.extractToken(r)
		if !a.tokenMatches(tok, false) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		isOp := a.cfg.OperatorToken != "" && a.tokenMatches(tok, true)
		ctx := WithCallerAuth(r.Context(), CallerAuth{Authenticated: true, IsOperator: isOp})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *App) withOperatorAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.OperatorToken == "" && a.cfg.APIToken == "" {
			ctx := WithCallerAuth(r.Context(), CallerAuth{Authenticated: true, IsOperator: true})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		tok := a.extractToken(r)
		// MCP OAuth tokens (nkt_at_*) are strictly scoped to mcp:tools and NEVER grant operator privileges,
		// even when OperatorToken is unset and APIToken is configured!
		if strings.HasPrefix(tok, "nkt_at_") {
			if a.matchOAuthToken(tok, false) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "operator token required"})
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "operator token required"})
			return
		}
		needOp := a.cfg.OperatorToken != ""
		if !a.tokenMatches(tok, needOp) {
			code := http.StatusUnauthorized
			if needOp && a.tokenMatches(tok, false) {
				code = http.StatusForbidden
			}
			writeJSON(w, code, map[string]string{"error": "operator token required"})
			return
		}
		ctx := WithCallerAuth(r.Context(), CallerAuth{Authenticated: true, IsOperator: true})
		next.ServeHTTP(w, r.WithContext(ctx))
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
		Secure:   isHTTPSRequest(r, a.cfg.PublicURL),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 7,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handlePair(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	codeKey := oauthSecretKey(code)
	a.pairMu.Lock()
	tk, ok := a.pairCodes[codeKey]
	if ok {
		delete(a.pairCodes, codeKey)
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
			Secure:   isHTTPSRequest(r, a.cfg.PublicURL),
			SameSite: http.SameSiteLaxMode,
			MaxAge:   86400 * 7,
		})
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *App) handleToolCall(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	exec := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		if name == "recall" || name == "groundedAnswer" {
			a.recordTTFV()
		}
		if name == "remember" || name == "groundedAnswer" || name == "want" || name == "sell" || name == "anchor" {
			a.BroadcastDashboardEvent("tool:" + name)
		}
		writeJSON(w, http.StatusOK, res)
	})
	a.withPaywall(name, exec).ServeHTTP(w, r)
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

func (a *App) allowFaucetIP(r *http.Request) bool {
	ip := extractClientIP(r)
	now := time.Now().Unix()
	a.faucetMu.Lock()
	defer a.faucetMu.Unlock()
	if len(a.faucetIPs) > 4096 {
		for k, times := range a.faucetIPs {
			active := false
			for _, ts := range times {
				if now-ts < 3600 {
					active = true
					break
				}
			}
			if !active {
				delete(a.faucetIPs, k)
			}
		}
	}
	prev := a.faucetIPs[ip]
	kept := prev[:0]
	for _, ts := range prev {
		if now-ts < 3600 {
			kept = append(kept, ts)
		}
	}
	if len(kept) >= 5 {
		a.faucetIPs[ip] = kept
		return false
	}
	a.faucetIPs[ip] = append(kept, now)
	return true
}

func (a *App) handlePublicFaucet(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Hub == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "faucet is only served on Hub nodes"})
		return
	}
	if !a.allowFaucetIP(r) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "faucet rate limit exceeded for this IP — try again later"})
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
		Preset     string                   `json:"preset"`
		Domains    []string                 `json:"domains,omitempty"`
		Confirm    string                   `json:"confirm,omitempty"`
		AIMatching *market.AIMatchingPolicy `json:"aiMatching,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if a.mem.Chain().Mainnet() && in.Confirm != "I_UNDERSTAND" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mainnet policy approval requires confirm=\"I_UNDERSTAND\""})
		return
	}
	rec, err := market.ApprovePolicyPresetWithAI(a.mem.DB(), a.mkt.Policy(), in.Preset, in.Domains, "dashboard", in.AIMatching)
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
	cliCmd := fmt.Sprintf("nektar hub up --auto-ip --network %s", net)
	vpsCmd := fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh | NEKTAR_NETWORK=%s NEKTAR_AUTO_IP=1 bash", net)
	if mode == "domain" {
		cliCmd = fmt.Sprintf("nektar hub up --domain %s --network %s", domain, net)
		vpsCmd = fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh | NEKTAR_NETWORK=%s NEKTAR_DOMAIN=%s bash", net, domain)
	} else if mode == "tunnel" {
		cliCmd = fmt.Sprintf("nektar hub up --tunnel --network %s", net)
		vpsCmd = fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh | NEKTAR_NETWORK=%s NEKTAR_TUNNEL=1 bash", net)
	}
	cloudInit := fmt.Sprintf(`#cloud-config
package_update: true
packages: [curl, ca-certificates, openssl]
runcmd:
  - |
    export NEKTAR_HUB_DIR=/opt/nektar-hub
    export NEKTAR_NETWORK=%s
    export NEKTAR_AUTO_IP=1
    curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh | bash
`, net)
	writeJSON(w, http.StatusOK, map[string]any{
		"cliCommand":         cliCmd,
		"vpsOneLiner":        vpsCmd,
		"cloudInitYaml":      cloudInit,
		"coolifyTemplateUrl": "https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/templates/coolify.compose.yaml",
		"helmCommand":        "helm install nektar-hub deploy/hub/helm --set network=" + net + " --set publicUrl=https://" + domain,
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

func (a *App) buildPublicCardSummary() map[string]any {
	receipts, _ := a.mem.DB().ListReceipts(1000)
	deals, _ := a.mem.DB().ListDeals(500)
	var earned uint64
	for _, d := range deals {
		if d.Role == "seller" && d.Status == "settled" {
			earned += d.PriceSats
		}
	}
	return map[string]any{
		"version":      "0.4.0",
		"identityPub":  a.mem.Chain().IdentityPubHex(),
		"network":      a.mem.Chain().Network(),
		"receiptCount": len(receipts),
		"earnedSats":   earned,
		"truthNote":    reasoning.TruthNote,
	}
}

func (a *App) handlePublicCard(w http.ResponseWriter, r *http.Request) {
	card := a.buildPublicCardSummary()
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, http.StatusOK, card)
		return
	}
	idPub := a.mem.Chain().IdentityPubHex()
	netName := a.mem.Chain().Network()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Nektar Agent Card</title>
<meta property="og:title" content="Nektar v0.4 Verifiable AI Agent">
<meta property="og:description" content="%v checkable receipts · %v sats earned on BSV %s">
<meta name="twitter:card" content="summary">
<style>body{font-family:system-ui,sans-serif;max-width:680px;margin:2rem auto;padding:1rem;background:#120e0c;color:#ece7e4}
.card{background:#211b17;border:1px solid #3a2f28;border-radius:14px;padding:1.5rem;margin-bottom:1rem}
h1{color:#f7991b;margin-top:0}code{color:#f7991b;word-break:break-all}a{color:#f7991b}
pre{background:#120e0c;border:1px solid #3a2f28;border-radius:8px;padding:.75rem;overflow-x:auto;font-size:.82rem}</style></head><body>
<div class="card"><h1>Nektar v0.4 Verifiable Agent</h1>
<p><strong>Identity:</strong> <code>%s</code></p>
<p><strong>Network:</strong> %s · <a href="/.well-known/nektar">Discovery JSON ↗</a> · <a href="/.well-known/agent.json">A2A Card ↗</a></p>
<p><strong>Checkable Receipts:</strong> %v · <strong>Earned:</strong> %v sats</p>
<p><small>%s</small></p></div>
<div class="card"><h3>Run your own verifiable AI agent</h3>
<pre>curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh &amp;&amp; nektar serve</pre></div></body></html>`,
		card["receiptCount"], card["earnedSats"], html.EscapeString(netName),
		html.EscapeString(idPub),
		html.EscapeString(netName),
		card["receiptCount"], card["earnedSats"], html.EscapeString(reasoning.TruthNote),
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

// BRC105PaymentHeader is the JSON payload accepted in `X-BSV-Payment` for HTTP 402 paid routes.
// Supports both verified AtomicBEEF + BRC-29 remittance (internalized directly into the server wallet)
// and lightweight `{txid, amountSats, inputs}` claims validated via `lightwebinc/bcommon/payee.Claims`.
type BRC105PaymentHeader struct {
	TxID              string   `json:"txid"`
	AmountSats        uint64   `json:"amountSats"`
	Inputs            []string `json:"inputs"`
	BEEFHex           string   `json:"beefHex,omitempty"`
	OutputIndex       uint32   `json:"outputIndex,omitempty"`
	DerivationPrefix  string   `json:"derivationPrefix,omitempty"`
	DerivationSuffix  string   `json:"derivationSuffix,omitempty"`
	SenderIdentityKey string   `json:"senderIdentityKey,omitempty"`
}

func (a *App) withPaywall(toolName string, next http.Handler) http.Handler {
	price := a.cfg.ToolPrices[toolName]
	if price == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := strings.TrimSpace(r.Header.Get("X-BSV-Payment"))
		addr, _ := a.mem.Chain().FundAddress()
		idPub := a.mem.Chain().IdentityPubHex()
		if hdr == "" {
			w.Header().Set("X-BSV-Payment-Amount", strconv.FormatUint(price, 10))
			w.Header().Set("X-BSV-Payment-Destination", addr)
			w.Header().Set("X-BSV-Payment-Identity", idPub)
			w.Header().Set("X-BSV-Payment-Protocol", "BRC-105")
			writeJSON(w, http.StatusPaymentRequired, map[string]any{
				"error":       "payment_required",
				"tool":        toolName,
				"amountSats":  price,
				"destination": addr,
				"identityKey": idPub,
			})
			return
		}
		var pay BRC105PaymentHeader
		if err := json.Unmarshal([]byte(hdr), &pay); err != nil {
			writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "invalid X-BSV-Payment header"})
			return
		}

		// Verified AtomicBEEF path (BRC-105 / BRC-29 or direct P2PKH funding output):
		if pay.BEEFHex != "" {
			rawBeef, err := hex.DecodeString(strings.TrimSpace(pay.BEEFHex))
			if err != nil {
				writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "invalid X-BSV-Payment beefHex"})
				return
			}
			_, tx, txHash, err := guard.ParseBEEF(rawBeef, guard.DefaultBound)
			if err != nil || tx == nil || txHash == nil {
				writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "unparseable X-BSV-Payment AtomicBEEF"})
				return
			}
			txid := txHash.String()
			inKeys := make([]string, 0, len(tx.Inputs))
			for _, in := range tx.Inputs {
				if in.SourceTXID != nil {
					inKeys = append(inKeys, fmt.Sprintf("%s:%d", in.SourceTXID.String(), in.SourceTxOutIndex))
				}
			}
			// On live networks (regtest/testnet/main), strictly require BRC-29 derived invoice remittance
			// (blocks replay of publicly visible historical mined deposits to static FundAddress()!),
			// and validate BRC-29 locking script + paidSats >= price BEFORE ClaimPaymentAtomic or RelayExternalTx!
			isBRC29 := pay.DerivationPrefix != "" && pay.DerivationSuffix != "" && pay.SenderIdentityKey != ""
			if a.mem.Chain().Network() != "memory" && !isBRC29 {
				writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "BRC-29 payment remittance (derivationPrefix, derivationSuffix, senderIdentityKey) is required on live networks"})
				return
			}
			var expectedPaid uint64
			if isBRC29 {
				d := &bwallet.Derivation{
					SecurityLevel:   int(bwallet.PaymentProtocol.SecurityLevel),
					Protocol:        bwallet.PaymentProtocol.Protocol,
					KeyID:           bwallet.PaymentKeyID(pay.DerivationPrefix, pay.DerivationSuffix),
					CounterpartyHex: pay.SenderIdentityKey,
					OwnerHex:        a.mem.Chain().IdentityPubHex(),
				}
				wantScript, dErr := a.mem.Chain().Signer().DerivedScript(r.Context(), d)
				if dErr != nil {
					writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "BRC-29 script derivation failed: " + dErr.Error()})
					return
				}
				if uint64(pay.OutputIndex) >= uint64(len(tx.Outputs)) {
					writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "BRC-29 outputIndex out of range"})
					return
				}
				out := tx.Outputs[pay.OutputIndex]
				if out.LockingScript == nil || !bytes.Equal(*out.LockingScript, *wantScript) {
					writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "BRC-29 payment output does not match derived locking script"})
					return
				}
				expectedPaid = out.Satoshis
			} else {
				fundScript, fErr := a.mem.Chain().Signer().FundScript()
				if fErr != nil {
					writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "fund script unavailable"})
					return
				}
				for _, out := range tx.Outputs {
					if out.LockingScript != nil && bytes.Equal(*out.LockingScript, *fundScript) {
						expectedPaid += out.Satoshis
					}
				}
			}
			if expectedPaid < price {
				writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": fmt.Sprintf("insufficient payment (%d < %d sats)", expectedPaid, price)})
				return
			}

			// Atomic cross-interface SQLite payment claim registry (shared with /v1/market/purchase).
			if err := a.mem.DB().ClaimPaymentAtomic(txid, inKeys); err != nil {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "payment_rejected", "reason": err.Error()})
				return
			}

			// On live networks, verify mined BUMP confirmation against our PoW header tracker,
			// or require synchronous network broadcast (RelayExternalTx) of unconfirmed transactions
			// BEFORE serving paid endpoints (prevents double-spend acceptance via swallowed InternalizeAction errors).
			if a.mem.Chain().Network() != "memory" {
				confirmed := false
				if tx.MerklePath != nil {
					if tracker := a.mem.Chain().Tracker(); tracker != nil {
						if root, rErr := tx.MerklePath.ComputeRoot(txHash); rErr == nil && root != nil {
							if valid, vErr := tracker.IsValidRootForHeight(r.Context(), root, tx.MerklePath.BlockHeight); vErr == nil && valid {
								confirmed = true
							}
						}
					}
				}
				if !confirmed {
					if rErr := a.mem.Chain().RelayExternalTx(r.Context(), tx); rErr != nil {
						_ = a.mem.DB().ReleasePaymentClaim(txid)
						writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "unconfirmed payment broadcast failed: " + rErr.Error()})
						return
					}
				}
			}

			var paid uint64
			if isBRC29 {
				paid, err = a.mem.Chain().InternalizeBRC29(
					r.Context(), rawBeef, pay.OutputIndex,
					pay.DerivationPrefix, pay.DerivationSuffix, pay.SenderIdentityKey,
					"BRC-105 payment for "+toolName, []string{"brc105", toolName},
				)
				if err != nil {
					writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "BRC-29 payment verification failed: " + err.Error()})
					return
				}
			} else {
				paid, err = a.mem.Chain().AdoptReceived(r.Context(), tx)
				if err != nil {
					writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "payment verification failed: " + err.Error()})
					return
				}
			}
			if paid < price {
				writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": fmt.Sprintf("insufficient payment (%d < %d sats)", paid, price)})
				return
			}
			a.BroadcastDashboardEvent("wallet_payment")
			next.ServeHTTP(w, r)
			return
		}

		// Legacy unverified JSON header is strictly refused on live networks (regtest/testnet/main);
		// only accepted in local in-memory tests where no real BSV payment BEEF is required.
		if a.mem.Chain().Network() != "memory" {
			writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "beefHex payment transaction is required on live networks"})
			return
		}
		if pay.TxID == "" || pay.AmountSats < price {
			writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "invalid or insufficient X-BSV-Payment"})
			return
		}
		if err := a.mem.DB().ClaimPaymentAtomic(pay.TxID, pay.Inputs); err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "payment_rejected",
				"reason": err.Error(),
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
	if p, disallowBorg, overridden := PrincipalForContext(r.Context(), policy.Principal{}); overridden {
		in.Principal = &p
		in.CallerPub = ""
		if disallowBorg && in.BorgGroup != "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "operator token required for Borg group writes"})
			return
		}
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
	if p, disallowBorg, overridden := PrincipalForContext(r.Context(), in.Principal); overridden {
		in.Principal = p
		in.CallerPub = ""
		in.DisallowBorg = disallowBorg
	}
	res, err := a.toolbox.ExecRecall(r.Context(), in)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.recordTTFV()
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
	if p, disallowBorg, overridden := PrincipalForContext(r.Context(), in.Principal); overridden {
		in.Principal = p
		in.CallerPub = ""
		in.DisallowBorg = disallowBorg
	}
	res, err := a.mem.GroundedAnswer(r.Context(), in)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.recordTTFV()
	a.BroadcastDashboardEvent("receipt")
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleVerifyReceipt(w http.ResponseWriter, r *http.Request) {
	a.verifyViews.Add(1)
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
		supWarn := ""
		if ver.CitationsSuperseded > 0 {
			supWarn = fmt.Sprintf(`<div class="warn"><strong>Superseded citation notice:</strong> %d cited fragment(s) have newer revisions (%s).</div>`,
				ver.CitationsSuperseded, html.EscapeString(strings.Join(ver.SupersededRefs, ", ")))
		}
		base := requestBaseURL(r)
		badgeURL := base + "/v1/badge/" + html.EscapeString(ver.ReceiptToken)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Nektar Receipt Verification — %s</title>
<meta property="og:title" content="Nektar Receipt: %s">
<meta property="og:description" content="%s">
<meta name="twitter:card" content="summary">
<style>body{font-family:system-ui,sans-serif;max-width:740px;margin:2rem auto;padding:1rem;background:#120e0c;color:#ece7e4}
.badge{display:inline-block;padding:.35rem .8rem;border-radius:999px;font-weight:700;text-transform:uppercase;background:%s;color:#fff}
.card{background:#211b17;border:1px solid #3a2f28;border-radius:12px;padding:1.25rem;margin-top:1rem}
.warn{background:rgba(217,119,6,.16);border:1px solid #d97706;border-radius:8px;padding:.75rem;margin-top:.75rem;color:#fcd34d}
code{color:#f7991b;word-break:break-all}a{color:#f7991b}
pre{background:#120e0c;border:1px solid #3a2f28;border-radius:8px;padding:.65rem;overflow-x:auto;font-size:.8rem}</style></head><body>
<h1>Nektar Receipt Verifier</h1><span class="badge">%s</span>
%s
<div class="card"><p><strong>Status:</strong> %s</p><p><strong>Token:</strong> <code>%s</code></p>
<p><strong>Answer:</strong> %s</p><p><strong>Citations:</strong> %d checked / %d mined</p>
<p><strong>Agent:</strong> <a href="/card">View Agent Card ↗</a> · <a href="/v1/badge/%s">SVG Badge ↗</a></p>
<p><small>%s</small></p></div>
<div class="card"><h3>Verify your own AI agent — Install Nektar</h3>
<p>Give Claude Code, Cursor, Hermes, OpenClaw, or any OpenAI/Anthropic agent verifiable BSV-anchored memory in 10 seconds:</p>
<pre>curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh &amp;&amp; nektar serve</pre>
<p><small>Embeddable badge: <code>![nektar receipt](%s)</code></small></p></div></body></html>`,
			html.EscapeString(ver.Verdict),
			html.EscapeString(ver.Verdict),
			html.EscapeString(ver.Reason),
			color, html.EscapeString(ver.Verdict), supWarn,
			html.EscapeString(ver.Reason),
			html.EscapeString(ver.ReceiptToken), html.EscapeString(answer),
			ver.CitationsChecked, ver.CitationsMined,
			html.EscapeString(ver.ReceiptToken),
			html.EscapeString(ver.TruthNote),
			html.EscapeString(badgeURL),
		)
		return
	}
	writeJSON(w, http.StatusOK, ver)
}

func (a *App) handleShardPack(w http.ResponseWriter, r *http.Request) {
	shard := r.PathValue("shard")
	res, err := a.mem.ExportShardPackAuthorized(r.Context(), shard, policy.Principal{Type: "Agent", ID: "owner", Level: 100}, "disclose", false)
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
	// Unauthenticated callers may ONLY fetch CHIRP objects explicitly published to the public allowlist
	// (e.g. starter packs or active BRC-369 sale ciphertexts). All other objects require authentication.
	if !a.mem.IsPublicChirp(h) {
		if a.cfg.APIToken != "" || a.cfg.OperatorToken != "" {
			if !a.tokenMatches(a.extractToken(r), false) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
		}
	}
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
		"memory/anchor":   "anchor",
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
	exec := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})
	a.withPaywall(toolName, exec).ServeHTTP(w, r)
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
