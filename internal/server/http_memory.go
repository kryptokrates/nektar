package server

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"

	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
)

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

func (a *App) buildPublicCardSummary() map[string]any {
	receipts, _ := a.mem.DB().ListReceipts(1000)
	deals, _ := a.mem.DB().ListDeals(500)
	var earned uint64
	for _, d := range deals {
		if d.Role == "seller" && d.Status == "settled" && d.PaymentTxID != "" {
			earned += d.PriceSats
		}
	}
	return map[string]any{
		"version":      "0.1.0",
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
<meta property="og:title" content="Nektar v0.1 Verifiable AI Agent">
<meta property="og:description" content="%v checkable receipts · %v sats earned on BSV %s">
<meta name="twitter:card" content="summary">
<style>body{font-family:system-ui,sans-serif;max-width:680px;margin:2rem auto;padding:1rem;background:#120e0c;color:#ece7e4}
.card{background:#211b17;border:1px solid #3a2f28;border-radius:14px;padding:1.5rem;margin-bottom:1rem}
h1{color:#f7991b;margin-top:0}code{color:#f7991b;word-break:break-all}a{color:#f7991b}
pre{background:#120e0c;border:1px solid #3a2f28;border-radius:8px;padding:.75rem;overflow-x:auto;font-size:.82rem}</style></head><body>
<div class="card"><h1>Nektar v0.1 Verifiable Agent</h1>
<p><strong>Identity:</strong> <code>%s</code></p>
<p><strong>Network:</strong> %s · <a href="/.well-known/nektar">Discovery JSON ↗</a> · <a href="/.well-known/agent.json">A2A Card ↗</a></p>
<p><strong>Checkable Receipts:</strong> %v · <strong>Earned:</strong> %v sats</p>
<p><small>%s</small></p></div>
<div class="card"><h3>Run your own verifiable AI agent in 25 seconds</h3>
<pre>curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh -s -- --up</pre></div></body></html>`,
		card["receiptCount"], card["earnedSats"], html.EscapeString(netName),
		html.EscapeString(idPub),
		html.EscapeString(netName),
		card["receiptCount"], card["earnedSats"], html.EscapeString(reasoning.TruthNote),
	)
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
<div class="card"><h3>Verify locally or dock your AI agent in 25 seconds</h3>
<p>Give Claude Code, Codex CLI, Hermes Agent, OpenClaw, Cursor, or VS Code verifiable BSV-anchored memory &amp; receipts in 1 command:</p>
<pre>curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh -s -- --up</pre>
<p><small>Embeddable badge: <code>![nektar receipt](%s)</code> · <a href="https://github.com/kryptokrates/nektar">Docs &amp; Windows installer ↗</a></small></p></div></body></html>`,
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
		"version":     "0.1.0",
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
