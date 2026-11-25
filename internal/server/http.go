package server

import (
	"bytes"
	"context"
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
	APIToken          string            // optional operator bearer token protecting write endpoints
	ToolPrices        map[string]uint64 // optional BRC-105/402 satoshi price per route/tool name
	OpenAIUpstream    string            // L0 proxy upstream (default https://api.openai.com)
	AnthropicUpstream string            // L0 proxy upstream (default https://api.anthropic.com)
	Hub               *hub.Node         // optional embedded Go overlay hub mounted under /hub/
	MountRestate      bool              // mounts Restate Go SDK service handler under /restate/
	HTTPClient        *http.Client
}

// App bundles all Nektar v0.4 HTTP interfaces into one net/http Handler.
type App struct {
	mem        *memory.Service
	mkt        *market.Engine
	mcpSrv     *mcp.Server
	cfg        Config
	claimsMu   sync.Mutex
	payClaims  payee.Claims
	httpClient *http.Client
	handler    http.Handler
}

// NewApp wires MCP Streamable HTTP, REST API, A2A Agent Card + JSON-RPC, L0 LLM Proxy,
// BRC-105/402 payment claims (`bcommon/payee`), embedded Svelte dashboard, Hub, and Restate.
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
	app := &App{
		mem:        mem,
		mkt:        mkt,
		mcpSrv:     NewMCPServer(mem, mkt),
		cfg:        cfg,
		httpClient: hc,
	}
	app.handler = app.buildMux()
	return app, nil
}

func (a *App) Handler() http.Handler { return a.handler }
func (a *App) MCP() *mcp.Server      { return a.mcpSrv }

// BuildStatus computes the live node status payload shared by MCP, REST, and Dashboard.
func BuildStatus(ctx context.Context, mem *memory.Service, mkt *market.Engine) map[string]any {
	shards, _ := mem.DB().ListShards()
	addr, _ := mem.Chain().FundAddress()
	h, _ := mem.Chain().Height(ctx)
	var deals []market.DealState
	if mkt != nil {
		deals = mkt.ListDeals()
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
		"truthNote":  reasoning.TruthNote,
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

	// 2. Health, discovery & A2A Agent Card (Agent-to-Agent protocol).
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
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
		shards, _ := a.mem.DB().ListShards()
		writeJSON(w, http.StatusOK, map[string]any{
			"version":     "0.4.0",
			"identityPub": a.mem.Chain().IdentityPubHex(),
			"network":     a.mem.Chain().Network(),
			"packBase":    "/v1/pack/",
			"verifyBase":  "/v1/verify/",
			"mcp":         "/mcp",
			"a2a":         "/a2a",
			"shards":      shards,
		})
	})
	mux.HandleFunc("GET /.well-known/agent.json", a.handleAgentCard)
	mux.HandleFunc("POST /a2a", a.handleA2A)

	// 3. Memory, Anchoring, Grounding & Receipts REST API (with optional 402 paywall + bearer auth).
	mux.Handle("POST /v1/remember", a.withAuth(a.withPaywall("remember", http.HandlerFunc(a.handleRemember))))
	mux.Handle("POST /v1/recall", a.withAuth(a.withPaywall("recall", http.HandlerFunc(a.handleRecall))))
	mux.Handle("POST /v1/anchor", a.withAuth(a.withPaywall("anchor", http.HandlerFunc(a.handleAnchor))))
	mux.Handle("POST /v1/answer", a.withAuth(a.withPaywall("groundedAnswer", http.HandlerFunc(a.handleAnswer))))
	mux.HandleFunc("GET /v1/verify/{token}", a.handleVerifyReceipt)
	mux.HandleFunc("GET /v1/pack/{shard}", a.handleShardPack)
	mux.HandleFunc("GET /v1/chirp/{hash}", a.handleChirpObject)
	mux.HandleFunc("GET /v1/wallet", func(w http.ResponseWriter, r *http.Request) {
		addr, _ := a.mem.Chain().FundAddress()
		spentToday, maxDay, maxFee, txCount := a.mem.Chain().SpendGuard().Stats()
		writeJSON(w, http.StatusOK, map[string]any{
			"network":     a.mem.Chain().Network(),
			"identityPub": a.mem.Chain().IdentityPubHex(),
			"fundAddress": addr,
			"balanceSats": a.mem.Chain().BalanceSats(),
			"spendGuard": map[string]any{
				"spentToday": spentToday,
				"maxPerDay":  maxDay,
				"maxFee":     maxFee,
				"txCount":    txCount,
			},
		})
	})

	// 4. L0 Transparent LLM Memory Proxy (OpenAI & Anthropic drop-in!).
	mux.Handle("POST /v1/proxy/openai/v1/chat/completions", a.withAuth(http.HandlerFunc(a.handleOpenAIProxy)))
	mux.Handle("POST /v1/proxy/anthropic/v1/messages", a.withAuth(http.HandlerFunc(a.handleAnthropicProxy)))

	// 5. Embedded Overlay Hub & Restate Services (optional single-binary all-in-one mode).
	if a.cfg.Hub != nil {
		mux.Handle("/hub/", http.StripPrefix("/hub", a.cfg.Hub.Handler()))
	}
	if a.cfg.MountRestate && a.mkt != nil {
		if rh, err := a.mkt.BindRestate(restateserver.NewRestate()).Handler(); err == nil {
			mux.Handle("/restate/", http.StripPrefix("/restate", rh))
		}
	}

	// 6. Embedded Svelte 5 Dashboard (/ + /dashboard + /dashboard/state + /dashboard/events).
	mux.HandleFunc("GET /dashboard/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, BuildStatus(r.Context(), a.mem, a.mkt))
	})
	mux.HandleFunc("GET /dashboard/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		b, _ := json.Marshal(BuildStatus(r.Context(), a.mem, a.mkt))
		_, _ = fmt.Fprintf(w, "event: state\ndata: %s\n\n", b)
		flusher.Flush()
	})
	uiSub, err := fs.Sub(uiDistFS, "ui_dist")
	if err == nil {
		fileSrv := http.FileServer(http.FS(uiSub))
		mux.Handle("GET /assets/", fileSrv)
		mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
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

func (a *App) withAuth(next http.Handler) http.Handler {
	if a.cfg.APIToken == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(a.cfg.APIToken)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
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
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleRecall(w http.ResponseWriter, r *http.Request) {
	var in memory.RecallInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	res, err := a.mem.Recall(r.Context(), in)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fragments": res, "count": len(res)})
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
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleVerifyReceipt(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
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
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        "Nektar Verifiable Memory Agent",
		"description": "BSV-anchored, BRC-369 encrypted, Cedar-governed, Google Mangle grounded agent memory node.",
		"version":     "0.4.0",
		"url":         base + "/a2a",
		"identityPub": a.mem.Chain().IdentityPubHex(),
		"capabilities": map[string]bool{
			"streaming":              true,
			"verifiableReceipts":     true,
			"paidKnowledgeDisclosure": true,
		},
		"skills": []map[string]string{
			{"id": "memory.recall", "name": "Verified Recall", "description": "Recall SPV-verified, Cedar-authorized fragments."},
			{"id": "memory.answer", "name": "Grounded Answer", "description": "Produce Mangle-grounded answers with nektarr1.* receipts."},
			{"id": "memory.verify", "name": "Verify Receipt", "description": "Verify a nektarr1.* receipt offline."},
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
	var result any
	var callErr error
	switch rpc.Method {
	case "memory/recall":
		var in memory.RecallInput
		callErr = json.Unmarshal(rpc.Params, &in)
		if callErr == nil {
			result, callErr = a.mem.Recall(r.Context(), in)
		}
	case "memory/answer":
		var in memory.GroundedAnswerInput
		callErr = json.Unmarshal(rpc.Params, &in)
		if callErr == nil {
			result, callErr = a.mem.GroundedAnswer(r.Context(), in)
		}
	case "memory/verify":
		var in struct {
			Token string `json:"token"`
		}
		callErr = json.Unmarshal(rpc.Params, &in)
		if callErr == nil {
			result, callErr = a.mem.VerifyReceipt(r.Context(), in.Token)
		}
	default:
		callErr = fmt.Errorf("unknown A2A method %q", rpc.Method)
	}
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

// handleOpenAIProxy implements Nektar's L0 zero-code transparent memory proxy for OpenAI-compatible clients.
// It recalls active fragments from X-Nektar-Shard, injects them as a system memory block, and forwards upstream.
func (a *App) handleOpenAIProxy(w http.ResponseWriter, r *http.Request) {
	shard := r.Header.Get("X-Nektar-Shard")
	if shard == "" {
		shard = "default"
	}
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

// handleAnthropicProxy implements Nektar's L0 transparent memory proxy for Anthropic Messages API clients.
func (a *App) handleAnthropicProxy(w http.ResponseWriter, r *http.Request) {
	shard := r.Header.Get("X-Nektar-Shard")
	if shard == "" {
		shard = "default"
	}
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
	frags, err := a.mem.Recall(ctx, memory.RecallInput{Shard: shard, Limit: 20})
	if err != nil || len(frags) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[Nektar Verified Memory — shard: " + shard + "]\n")
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
