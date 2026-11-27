package server

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kryptokrates/nektar/internal/market"
)

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
		if pubURL := strings.TrimRight(strings.TrimSpace(a.cfg.PublicURL), "/"); pubURL != "" && !isLoopbackURL(pubURL) {
			resp["publicUrl"] = pubURL
		}
		if a.cfg.Hub != nil {
			resp["hub"] = "/hub"
			if addr, err := a.mem.Chain().FundAddress(); err == nil && addr != "" {
				resp["fundAddress"] = addr
			}
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

	// 5. Memory, Grounding, Ontology, CLKA Lattice & Receipts REST API.
	mux.Handle("POST /v1/remember", a.withAuth(a.withPaywall("remember", http.HandlerFunc(a.handleRemember))))
	mux.Handle("POST /v1/recall", a.withAuth(a.withPaywall("recall", http.HandlerFunc(a.handleRecall))))
	mux.Handle("POST /v1/answer", a.withAuth(a.withPaywall("groundedAnswer", http.HandlerFunc(a.handleAnswer))))
	mux.Handle("POST /v1/ontology/commit", a.withOperatorAuth(http.HandlerFunc(a.handleCommitOntology)))
	mux.Handle("GET /v1/lineage/{shard}/{seq}", a.withAuth(http.HandlerFunc(a.handleLineage)))
	mux.Handle("POST /v1/lattice/clearance", a.withOperatorAuth(http.HandlerFunc(a.handleProvisionClearance)))
	mux.Handle("POST /v1/lattice/rotate", a.withOperatorAuth(http.HandlerFunc(a.handleRotateLattice)))
	mux.Handle("GET /v1/prove/compact/{shard}/{seq}", a.withAuth(http.HandlerFunc(a.handleCompactProve)))
	mux.Handle("GET /v1/prove/consistency/{shard}", a.withAuth(http.HandlerFunc(a.handleConsistencyProof)))
	mux.HandleFunc("GET /v1/verify", a.handleVerifyReceipt)
	mux.HandleFunc("GET /v1/verify/{token}", a.handleVerifyReceipt)
	mux.Handle("GET /v1/pack/{shard}", a.withOperatorAuth(http.HandlerFunc(a.handleShardPack)))
	mux.HandleFunc("GET /v1/chirp/{hash}", a.handleChirpObject)
	a.registerRepoRoutes(mux)
	a.registerWalletRoutes(mux)

	// 6. Live A2A Market Purchase, NMU-1 Metered Recall / Subscriptions / Grade S Feeds / Ticker, Host Discovery, Onboarding, Faucet & Approvals!
	mux.HandleFunc("POST /v1/market/purchase", a.handleMarketPurchase)
	mux.HandleFunc("POST /v1/market/query", a.handleMarketQuery)
	mux.HandleFunc("POST /v1/market/subscriptions", a.handleMarketSubscribe)
	mux.HandleFunc("GET /v1/market/feeds/{shard}", a.handleMarketFeedSSE)
	mux.HandleFunc("GET /v1/market/ticker", a.handleMarketTicker)
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
		writeJSON(w, http.StatusOK, map[string]any{"code": a.CreatePairCode()})
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
	mux.Handle("GET /v1/autonomy/status", a.withAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{"ok": true}
		if a.watchdog != nil {
			resp["watchdog"] = a.watchdog.Status()
		}
		if a.broker != nil && a.broker.Subscriptions() != nil {
			resp["subscriptions"] = a.broker.Subscriptions().Status()
		}
		if a.mkt != nil && a.mkt.Policy() != nil {
			resp["policyAutonomy"] = a.mkt.Policy().AutonomyConfig()
		}
		writeJSON(w, http.StatusOK, resp)
	})))
	mux.Handle("POST /v1/autonomy/config", a.withOperatorAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in market.PolicyAutonomyConfig
		if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if a.mkt == nil || a.mkt.Policy() == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "market policy unavailable"})
			return
		}
		if err := a.mkt.Policy().SetAutonomyConfig(in); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		a.BroadcastDashboardEvent("autonomy_config_updated")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "policyAutonomy": a.mkt.Policy().AutonomyConfig()})
	})))
	mux.Handle("POST /v1/autonomy/reset", a.withOperatorAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if a.watchdog != nil {
			a.watchdog.ManualReset()
		}
		a.BroadcastDashboardEvent("autonomy_reset")
		resp := map[string]any{"ok": true}
		if a.watchdog != nil {
			resp["watchdog"] = a.watchdog.Status()
		}
		writeJSON(w, http.StatusOK, resp)
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
	a.registerAutonomousRoutes(mux)

	// 8. Embedded Overlay Hub (Restate services are NEVER exposed on the public :18420 mux — dedicated internal h2c port only!).
	if a.cfg.Hub != nil {
		mux.Handle("/hub/", http.StripPrefix("/hub", a.cfg.Hub.Handler()))
	}

	// 9. Svelte 5 Dashboard canonical routes (/state, /events, /).
	mux.Handle("GET /state", a.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.BuildDashboardState(r.Context(), requestBaseURL(r)))
	})))
	mux.Handle("GET /events", a.withAuth(http.HandlerFunc(a.handleDashboardSSE)))

	uiSub, err := fs.Sub(uiDistFS, "ui_dist")
	if err == nil {
		fileSrv := http.FileServer(http.FS(uiSub))
		mux.Handle("GET /assets/", fileSrv)
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
