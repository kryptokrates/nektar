package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
)

var proxyReceiptRe = regexp.MustCompile(`nektarr1\.[a-zA-Z0-9_-]{20,}`)

func (a *App) registerProxyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/proxy/health", func(w http.ResponseWriter, r *http.Request) {
		ok, _, _ := a.resolveProxyAuth(r, "openai")
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":                true,
			"openaiUpstream":    a.cfg.OpenAIUpstream,
			"anthropicUpstream": a.cfg.AnthropicUpstream,
		})
	})
	mux.HandleFunc("/v1/proxy/openai/", func(w http.ResponseWriter, r *http.Request) {
		a.handleLLMProxy(w, r, "openai", strings.TrimPrefix(r.URL.Path, "/v1/proxy/openai"))
	})
	mux.HandleFunc("/v1/proxy/anthropic/", func(w http.ResponseWriter, r *http.Request) {
		a.handleLLMProxy(w, r, "anthropic", strings.TrimPrefix(r.URL.Path, "/v1/proxy/anthropic"))
	})
}

// resolveProxyAuth checks X-Nektar-Token, composite `nkt:<nektarToken>:<providerKey>` in Authorization/x-api-key,
// or standard session/Bearer auth (when APIToken is unset, loopback is open).
// Returns (authorized, upstreamKeyOverride, hasOverride).
func (a *App) resolveProxyAuth(r *http.Request, provider string) (bool, string, bool) {
	authRequired := a.cfg.APIToken != "" || a.cfg.OperatorToken != ""

	var rawKey string
	if provider == "anthropic" {
		rawKey = strings.TrimSpace(r.Header.Get("x-api-key"))
	} else if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		rawKey = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}

	if strings.HasPrefix(rawKey, "nkt:") {
		parts := strings.SplitN(rawKey, ":", 3)
		if len(parts) == 3 {
			nktTok, upstreamKey := parts[1], parts[2]
			if !authRequired || a.tokenMatches(nktTok, false) {
				return true, upstreamKey, true
			}
			return false, "", true
		}
	}

	if xTok := strings.TrimSpace(r.Header.Get("X-Nektar-Token")); xTok != "" {
		if a.tokenMatches(xTok, false) {
			return true, "", false
		}
		if authRequired {
			return false, "", false
		}
	}

	if !authRequired {
		return true, "", false
	}
	if a.tokenMatches(a.extractToken(r), false) {
		return true, "", false
	}
	return false, "", false
}

func (a *App) isNektarCredential(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "Bearer ") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "Bearer "))
	}
	if strings.HasPrefix(s, "nkt:") || strings.HasPrefix(s, "nkt_at_") {
		return true
	}
	return a.tokenMatches(s, false)
}

func (a *App) handleLLMProxy(w http.ResponseWriter, r *http.Request, provider, subPath string) {
	if strings.Contains(subPath, "..") || strings.Contains(strings.ToLower(r.URL.RawPath), "%2e") || strings.Contains(strings.ToLower(r.URL.RawPath), "%2f..") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid proxy path"})
		return
	}
	authed, upstreamKeyOverride, hasOverride := a.resolveProxyAuth(r, provider)
	if !authed {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized (pass X-Nektar-Token or composite nkt:<nektarToken>:<providerKey>)"})
		return
	}

	upstreamBase := a.cfg.OpenAIUpstream
	if provider == "anthropic" {
		upstreamBase = a.cfg.AnthropicUpstream
	}
	targetURL := strings.TrimRight(upstreamBase, "/") + subPath
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	var rawBody []byte
	var err error
	if r.Body != nil {
		rawBody, err = io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}

	isInferencePost := r.Method == http.MethodPost && (strings.HasSuffix(subPath, "/chat/completions") || strings.HasSuffix(subPath, "/responses") || strings.HasSuffix(subPath, "/messages"))
	shard := strings.TrimSpace(r.Header.Get("X-Nektar-Shard"))
	skipRecall := strings.TrimSpace(r.Header.Get("X-Nektar-Recall")) == "0"
	captureOn := strings.TrimSpace(r.Header.Get("X-Nektar-Capture")) == "1"
	var userPrompt string
	memoryHits := 0

	if isInferencePost && len(rawBody) > 0 {
		var payload map[string]any
		if json.Unmarshal(rawBody, &payload) == nil {
			userPrompt = extractProxyUserPrompt(provider, subPath, payload)
			if !skipRecall {
				memCtx, hits := a.buildPromptMemoryContext(r.Context(), shard, userPrompt)
				memoryHits = hits
				if memCtx != "" {
					injectProxyMemory(provider, subPath, payload, memCtx)
					if updated, mErr := json.Marshal(payload); mErr == nil {
						rawBody = updated
					}
				}
			}
		}
	}

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(rawBody))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	for _, h := range []string{"Content-Type", "Accept", "anthropic-version", "anthropic-beta", "OpenAI-Beta", "OpenAI-Organization"} {
		if v := r.Header.Get(h); v != "" {
			upReq.Header.Set(h, v)
		}
	}
	if provider == "anthropic" {
		if hasOverride {
			if upstreamKeyOverride != "" {
				upReq.Header.Set("x-api-key", upstreamKeyOverride)
			}
		} else if v := r.Header.Get("x-api-key"); v != "" && !a.isNektarCredential(v) {
			upReq.Header.Set("x-api-key", v)
		}
	} else {
		if hasOverride {
			if upstreamKeyOverride != "" {
				upReq.Header.Set("Authorization", "Bearer "+upstreamKeyOverride)
			}
		} else if v := r.Header.Get("Authorization"); v != "" && !a.isNektarCredential(v) {
			upReq.Header.Set("Authorization", v)
		}
	}

	streamClient := &http.Client{
		Transport: a.proxyTransport,
	}
	if a.cfg.HTTPClient != nil && a.cfg.HTTPClient.Transport != nil {
		streamClient.Transport = a.cfg.HTTPClient.Transport
	}

	resp, err := streamClient.Do(upReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	w.Header().Set("X-Nektar-Memory-Hits", strconv.Itoa(memoryHits))
	if shard != "" {
		w.Header().Set("X-Nektar-Shard", shard)
	} else {
		w.Header().Set("X-Nektar-Shard", "*")
	}
	for k, vals := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "content-length" || lk == "transfer-encoding" || lk == "connection" {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	isStream := strings.Contains(ct, "text/event-stream")
	if isStream {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	var teed bytes.Buffer
	const maxTee = 64 * 1024
	buf := make([]byte, 8192)
	for {
		n, rErr := resp.Body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if isStream && flusher != nil {
				flusher.Flush()
			}
			if isInferencePost && teed.Len() < maxTee {
				rem := maxTee - teed.Len()
				if n < rem {
					rem = n
				}
				_, _ = teed.Write(buf[:rem])
			}
		}
		if rErr != nil {
			break
		}
	}

	if isInferencePost && resp.StatusCode >= 200 && resp.StatusCode < 300 && teed.Len() > 0 {
		rawResp := teed.String()
		go a.postProxyTurn(provider, shard, userPrompt, rawResp, isStream, captureOn)
	}
}

func (a *App) buildPromptMemoryContext(ctx context.Context, shard, prompt string) (string, int) {
	rCtx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	// External LLM disclosure: strictly Level 0 + Cedar "disclose" + never Borg-encrypted secrets.
	proxyPrincipal := policy.Principal{Type: "Agent", ID: "llm-proxy", Level: 0}
	_, frags, err := a.mem.RecallAllShards(rCtx, memory.RecallInput{
		Shard:        shard,
		Query:        prompt,
		Principal:    proxyPrincipal,
		Action:       "disclose",
		DisallowBorg: true,
		Limit:        8,
	})
	if err != nil {
		return "", 0
	}
	// If prompt FTS returned 0 hits, fall back to top 4 most-recent fragments so welcome/core rules stay visible.
	if len(frags) == 0 && strings.TrimSpace(prompt) != "" {
		_, frags, _ = a.mem.RecallAllShards(rCtx, memory.RecallInput{
			Shard:        shard,
			Principal:    proxyPrincipal,
			Action:       "disclose",
			DisallowBorg: true,
			Limit:        4,
		})
	}
	if len(frags) == 0 {
		return "", 0
	}
	var sb strings.Builder
	label := shard
	if label == "" {
		label = "all shards"
	}
	sb.WriteString("[Nektar Verified Memory — " + label + "]\n")
	for _, f := range frags {
		taintTag := ""
		if f.Tainted {
			taintTag = " [external-market]"
		}
		sb.WriteString(fmt.Sprintf("- [%s:%d]%s %s\n", f.Shard, f.Seq, taintTag, f.Text))
	}
	return sb.String(), len(frags)
}

func extractProxyUserPrompt(provider, subPath string, payload map[string]any) string {
	if provider == "openai" && strings.HasSuffix(subPath, "/responses") {
		if s, ok := payload["input"].(string); ok {
			return s
		}
		if arr, ok := payload["input"].([]any); ok {
			for i := len(arr) - 1; i >= 0; i-- {
				if m, ok := arr[i].(map[string]any); ok && m["role"] == "user" {
					return extractProxyContentText(m["content"])
				}
			}
		}
		return ""
	}
	msgs, _ := payload["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		if m, ok := msgs[i].(map[string]any); ok && m["role"] == "user" {
			return extractProxyContentText(m["content"])
		}
	}
	return ""
}

func extractProxyContentText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	arr, ok := v.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range arr {
		if s, ok := item.(string); ok {
			parts = append(parts, s)
		} else if m, ok := item.(map[string]any); ok {
			if t, ok := m["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func injectProxyMemory(provider, subPath string, payload map[string]any, block string) {
	if provider == "openai" && strings.HasSuffix(subPath, "/responses") {
		prev, _ := payload["instructions"].(string)
		if prev != "" {
			payload["instructions"] = block + "\n\n" + prev
		} else {
			payload["instructions"] = block
		}
		return
	}
	if provider == "anthropic" {
		switch sys := payload["system"].(type) {
		case string:
			if sys != "" {
				payload["system"] = block + "\n\n" + sys
			} else {
				payload["system"] = block
			}
		case []any:
			payload["system"] = append([]any{map[string]any{"type": "text", "text": block}}, sys...)
		default:
			payload["system"] = block
		}
		return
	}
	msgs, _ := payload["messages"].([]any)
	payload["messages"] = append([]any{map[string]any{
		"role":    "system",
		"content": block,
	}}, msgs...)
}

func (a *App) postProxyTurn(provider, shard, userPrompt, rawResp string, isStream, captureOn bool) {
	assistantText := extractAssistantText(provider, rawResp, isStream)
	if assistantText == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Async receipt verification telemetry for any nektarr1.* tokens emitted by the model.
	for _, tok := range proxyReceiptRe.FindAllString(assistantText, 3) {
		_, _ = a.mem.VerifyReceipt(ctx, tok)
	}

	if captureOn {
		targetShard := shard
		if targetShard == "" {
			targetShard = "proxy-captures"
		}
		summary := assistantText
		if userPrompt != "" {
			summary = "Q: " + shortStr(userPrompt, 300) + "\nA: " + shortStr(assistantText, 1200)
		}
		_, _ = a.mem.Remember(ctx, memory.RememberInput{
			Shard:  targetShard,
			Text:   summary,
			Source: "proxy:" + provider,
			Class:  "capture",
		})
	}
}

func extractAssistantText(provider, raw string, isStream bool) string {
	if isStream {
		var sb strings.Builder
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}
			var ev map[string]any
			if json.Unmarshal([]byte(data), &ev) != nil {
				continue
			}
			if choices, ok := ev["choices"].([]any); ok && len(choices) > 0 {
				if c0, ok := choices[0].(map[string]any); ok {
					if delta, ok := c0["delta"].(map[string]any); ok {
						if txt, ok := delta["content"].(string); ok {
							sb.WriteString(txt)
						}
					}
				}
			}
			if delta, ok := ev["delta"].(map[string]any); ok {
				if txt, ok := delta["text"].(string); ok {
					sb.WriteString(txt)
				}
			}
		}
		return sb.String()
	}
	var obj map[string]any
	if json.Unmarshal([]byte(raw), &obj) != nil {
		return ""
	}
	if provider == "anthropic" {
		return extractProxyContentText(obj["content"])
	}
	if choices, ok := obj["choices"].([]any); ok && len(choices) > 0 {
		if c0, ok := choices[0].(map[string]any); ok {
			if msg, ok := c0["message"].(map[string]any); ok {
				return extractProxyContentText(msg["content"])
			}
		}
	}
	if txt, ok := obj["output_text"].(string); ok {
		return txt
	}
	return ""
}
