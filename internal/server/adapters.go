package server

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const fallbackSkillMD = `---
name: nektar-memory
description: >
  Use nektar verifiable, encrypted agent memory anchored on BSV. Recall saved
  knowledge before answering factual questions, persist durable facts with
  provenance, route verifiable answers through groundedAnswer with receipt
  footers, and optionally buy/sell knowledge under approved policies.
---

# nektar — Verifiable Agent Memory (v0.4)

1. **Recall first:** Call ` + "`recall`" + ` before answering factual questions (omit ` + "`shard`" + ` to discover all shards including ` + "`welcome`" + `).
2. **Ground & cite:** Route verifiable answers through ` + "`groundedAnswer`" + ` so every claim cites a verified fragment and receives a checkable receipt.
3. **Keep the footer:** End grounded answers with the returned ` + "`receipt:`" + ` and ` + "`verify:`" + ` lines verbatim.
4. **Remember with provenance:** Save durable facts via ` + "`remember`" + ` with ` + "`shard`" + `, ` + "`text`" + `, and ` + "`source`" + `.
5. **Honest boundary:** Receipts prove content was *recorded, unaltered, and derivable* — never that it is *true*.
`

type adapterBundleFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func resolveAdaptersRoot() string {
	candidates := []string{
		"/usr/local/share/nektar",
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, dir, filepath.Dir(dir), filepath.Join(dir, "share", "nektar"))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, cwd, filepath.Dir(cwd), filepath.Dir(filepath.Dir(cwd)))
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "integrations", "hermes")); err == nil {
			return c
		}
		if _, err := os.Stat(filepath.Join(c, "integrations", "openclaw")); err == nil {
			return c
		}
	}
	return ""
}

func loadAdapterBundleFiles(root, name string) []adapterBundleFile {
	if root == "" {
		return nil
	}
	dir := filepath.Join(root, "integrations", name)
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	var out []adapterBundleFile
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		base := d.Name()
		if d.IsDir() {
			if base == "__pycache__" || base == "node_modules" || base == ".pytest_cache" || base == ".git" || base == "tests" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(base, ".pyc") || strings.HasSuffix(base, ".pyo") {
			return nil
		}
		raw, rErr := os.ReadFile(p)
		if rErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, adapterBundleFile{Path: filepath.ToSlash(rel), Content: string(raw)})
		return nil
	})
	return out
}

func loadSkillMarkdown(root string) string {
	if root != "" {
		if raw, err := os.ReadFile(filepath.Join(root, "skills", "nektar-memory", "SKILL.md")); err == nil && len(raw) > 0 {
			return string(raw)
		}
	}
	return fallbackSkillMD
}

func (a *App) registerAdapterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, buildOpenAPISpec(requestBaseURL(r)))
	})
	mux.HandleFunc("GET /v1/adapters/manifest", func(w http.ResponseWriter, _ *http.Request) {
		root := resolveAdaptersRoot()
		hermes := loadAdapterBundleFiles(root, "hermes")
		openclaw := loadAdapterBundleFiles(root, "openclaw")
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
		_, _ = w.Write([]byte(loadSkillMarkdown(resolveAdaptersRoot())))
	})
	mux.HandleFunc("GET /v1/adapters/bundle/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := strings.ToLower(strings.TrimSpace(r.PathValue("name")))
		if name != "hermes" && name != "openclaw" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown adapter bundle"})
			return
		}
		files := loadAdapterBundleFiles(resolveAdaptersRoot(), name)
		if len(files) == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "adapter bundle not found in this binary/image"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "files": files})
	})
}

func buildOpenAPISpec(baseURL string) map[string]any {
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Nektar v0.4 Verifiable Agent Memory & Knowledge Market API",
			"version":     "0.4.0",
			"description": "Single-binary pure-Go agent memory anchored on BSV with SPV verification, Cedar authorization, Google Mangle grounding, MCP Streamable HTTP + OAuth 2.1, A2A JSON-RPC, and L0 LLM Memory Proxy.",
		},
		"servers": []map[string]string{{"url": baseURL}},
		"paths": map[string]any{
			"/v1/health":                            map[string]any{"get": map[string]any{"summary": "Liveness & network probe"}},
			"/v1/metrics":                           map[string]any{"get": map[string]any{"summary": "Prometheus telemetry & domain liquidity metrics"}},
			"/v1/tools":                             map[string]any{"get": map[string]any{"summary": "List active profile tools"}},
			"/v1/tools/{name}":                      map[string]any{"post": map[string]any{"summary": "Execute any Nektar tool by name"}},
			"/v1/remember":                          map[string]any{"post": map[string]any{"summary": "Commit a provenance-signed fragment"}},
			"/v1/recall":                            map[string]any{"post": map[string]any{"summary": "Recall FTS5-ranked, Cedar-authorized fragments"}},
			"/v1/answer":                            map[string]any{"post": map[string]any{"summary": "Generate a Mangle-grounded answer with nektarr1.* receipt"}},
			"/v1/verify/{token}":                    map[string]any{"get": map[string]any{"summary": "Verify a nektarr1.* receipt (JSON or HTML)"}},
			"/v1/badge/{token}":                     map[string]any{"get": map[string]any{"summary": "Embeddable SVG verification badge"}},
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
