package server

import (
	"net/http"
	"strings"

	"github.com/kryptokrates/nektar/internal/dock"
)

func (a *App) registerAdapterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, buildOpenAPISpec(requestBaseURL(r)))
	})
	mux.HandleFunc("GET /v1/adapters/manifest", func(w http.ResponseWriter, _ *http.Request) {
		hermes := dock.BundleFiles("hermes")
		openclaw := dock.BundleFiles("openclaw")
		hPaths := make([]string, 0, len(hermes))
		for _, f := range hermes {
			hPaths = append(hPaths, f.Path)
		}
		oPaths := make([]string, 0, len(openclaw))
		for _, f := range openclaw {
			oPaths = append(oPaths, f.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"v": 1,
			"adapters": map[string]any{
				"hermes":   map[string]any{"available": len(hermes) > 0, "files": hPaths},
				"openclaw": map[string]any{"available": len(openclaw) > 0, "files": oPaths},
				"skill":    map[string]any{"available": true, "path": "/v1/adapters/skill.md"},
			},
		})
	})
	mux.HandleFunc("GET /v1/adapters/skill.md", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(dock.SkillMarkdown())
	})
	mux.HandleFunc("GET /v1/adapters/bundle/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := strings.ToLower(strings.TrimSpace(r.PathValue("name")))
		if name != "hermes" && name != "openclaw" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown adapter bundle"})
			return
		}
		files := dock.BundleFiles(name)
		if len(files) == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "adapter bundle not found in this binary"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "files": files})
	})
}

func buildOpenAPISpec(baseURL string) map[string]any {
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Nektar v0.1 Verifiable Agent Memory & Knowledge Market API",
			"version":     "0.1.0",
			"description": "Single-binary pure-Go agent memory anchored on BSV with SPV verification, Cedar authorization, Google Mangle grounding, MCP Streamable HTTP + OAuth 2.1, A2A JSON-RPC, and L0 LLM Memory Proxy.",
		},
		"servers": []map[string]string{{"url": baseURL}},
		"paths": map[string]any{
			"/v1/health":                            map[string]any{"get": map[string]any{"summary": "Liveness & network probe"}},
			"/v1/coldstart/health":                  map[string]any{"get": map[string]any{"summary": "Cold-start per-domain & cohort liquidity scorecard"}},
			"/v1/metrics":                           map[string]any{"get": map[string]any{"summary": "Prometheus telemetry & domain liquidity metrics"}},
			"/v1/tools":                             map[string]any{"get": map[string]any{"summary": "List active profile tools"}},
			"/v1/tools/{name}":                      map[string]any{"post": map[string]any{"summary": "Execute any Nektar tool by name"}},
			"/v1/remember":                          map[string]any{"post": map[string]any{"summary": "Commit a provenance-signed fragment"}},
			"/v1/recall":                            map[string]any{"post": map[string]any{"summary": "Recall FTS5-ranked, Cedar-authorized fragments"}},
			"/v1/answer":                            map[string]any{"post": map[string]any{"summary": "Generate a Mangle-grounded answer with nektarr1.* receipt"}},
			"/v1/verify/{token}":                    map[string]any{"get": map[string]any{"summary": "Verify a nektarr1.* receipt (JSON or HTML)"}},
			"/v1/badge/{token}":                     map[string]any{"get": map[string]any{"summary": "Embeddable SVG verification badge"}},
			"/v1/repos":                             map[string]any{"get": map[string]any{"summary": "List all Git-native Memory Repos"}},
			"/v1/repos/{repo}":                      map[string]any{"get": map[string]any{"summary": "Inspect repo summary, log, and active tree"}},
			"/v1/repos/{repo}/log":                  map[string]any{"get": map[string]any{"summary": "Commit history for a Memory Repo"}},
			"/v1/repos/{repo}/tree":                 map[string]any{"get": map[string]any{"summary": "Cedar-authorized tree and blobs at a branch or commit"}},
			"/v1/repos/{repo}/diff":                 map[string]any{"get": map[string]any{"summary": "Diff and unified patch between two commits"}},
			"/v1/repos/{repo}/blame":                map[string]any{"get": map[string]any{"summary": "Line-level Git blame with ModelID & BSV anchor lineage"}},
			"/v1/repos/{repo}/commit":               map[string]any{"post": map[string]any{"summary": "Commit a file or fact to a Memory Repo"}},
			"/v1/repos/{repo}/file":                 map[string]any{"delete": map[string]any{"summary": "Non-destructive git rm removing a file from branch HEAD"}},
			"/v1/repos/{repo}/branches":             map[string]any{"post": map[string]any{"summary": "Create a new branch (operator)"}},
			"/v1/repos/{repo}/merge":                map[string]any{"post": map[string]any{"summary": "Line-level 3-way LCA merge (operator)"}},
			"/v1/repos/{repo}/revert":               map[string]any{"post": map[string]any{"summary": "Non-destructive revert commit (operator)"}},
			"/v1/repos/{repo}/cherry-pick":          map[string]any{"post": map[string]any{"summary": "Cherry-pick a commit onto a target branch (operator)"}},
			"/v1/repos/{repo}/fork":                 map[string]any{"post": map[string]any{"summary": "Fork a Memory Repo with a new BRC-43 GenesisOwner (operator)"}},
			"/v1/repos/{repo}/tags":                 map[string]any{"get": map[string]any{"summary": "List checkpoint tags"}, "post": map[string]any{"summary": "Create a signed immutable tag (operator)"}},
			"/v1/repos/{repo}/prove-fields":         map[string]any{"post": map[string]any{"summary": "Generate an L1-bound MPE selective-disclosure proof"}},
			"/v1/repos/{repo}/rotate":               map[string]any{"post": map[string]any{"summary": "Rotate on-chain NektarAnchor owner key (operator)"}},
			"/v1/repos/{repo}/capsule":              map[string]any{"get": map[string]any{"summary": "Export a self-verifying .nkt Memory Capsule (operator)"}},
			"/v1/repos/inspect-capsule":             map[string]any{"post": map[string]any{"summary": "Read-only offline verification & diff preview of a .nkt Capsule"}},
			"/v1/repos/import":                      map[string]any{"post": map[string]any{"summary": "Import and optionally merge a verified .nkt Capsule (operator)"}},
			"/v1/repos/{repo}/checkout":             map[string]any{"post": map[string]any{"summary": "Export a working .git workspace on disk (operator)"}},
			"/v1/market/standing":                   map[string]any{"get": map[string]any{"summary": "List durable Restate StandingOrders"}, "post": map[string]any{"summary": "Create a durable StandingOrder (operator)"}},
			"/v1/market/standing/{id}/steer":        map[string]any{"post": map[string]any{"summary": "Live-steer a running StandingOrder (operator)"}},
			"/v1/market/standing/{id}":              map[string]any{"delete": map[string]any{"summary": "Cancel a running StandingOrder (operator)"}},
			"/v1/runtime/invocations":               map[string]any{"get": map[string]any{"summary": "List Restate durable workflow invocations (operator)"}},
			"/v1/runtime/invocations/{id}/resume":   map[string]any{"post": map[string]any{"summary": "Resume a paused Restate invocation (operator)"}},
			"/v1/runtime/invocations/{id}/restart":  map[string]any{"post": map[string]any{"summary": "Restart a Restate Service/VO invocation as new (operator)"}},
			"/v1/runtime/invocations/{id}/kill":     map[string]any{"post": map[string]any{"summary": "Kill a stuck Restate invocation (operator)"}},
			"/v1/wallet/send":                       map[string]any{"post": map[string]any{"summary": "Send BSV (embedded wallet only; disabled in client mode)"}},
			"/v1/hosts":                             map[string]any{"post": map[string]any{"summary": "Join an overlay hub (operator)"}, "delete": map[string]any{"summary": "Leave an overlay hub (operator)"}},
			"/v1/policies":                          map[string]any{"get": map[string]any{"summary": "List active operator market spending policies (operator)"}},
			"/v1/proxy/openai/v1/chat/completions":  map[string]any{"post": map[string]any{"summary": "L0 Transparent OpenAI Memory Proxy"}},
			"/v1/proxy/anthropic/v1/messages":       map[string]any{"post": map[string]any{"summary": "L0 Transparent Anthropic Memory Proxy"}},
			"/mcp":                                  map[string]any{"post": map[string]any{"summary": "MCP Streamable HTTP transport (OAuth 2.1 / Bearer)"}},
			"/a2a":                                  map[string]any{"post": map[string]any{"summary": "A2A JSON-RPC endpoint"}},
			"/.well-known/agent.json":               map[string]any{"get": map[string]any{"summary": "A2A Agent Card"}},
			"/.well-known/nektar":                   map[string]any{"get": map[string]any{"summary": "Nektar discovery manifest"}},
			"/.well-known/oauth-protected-resource": map[string]any{"get": map[string]any{"summary": "MCP OAuth 2.1 Protected Resource Metadata"}},
		},
	}
}
