package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kryptokrates/nektar/internal/policy"
)

type callerAuthKey struct{}

// CallerAuth carries server-verified caller privilege derived strictly from authentication middleware.
type CallerAuth struct {
	Authenticated bool
	IsOperator    bool
}

// WithCallerAuth attaches server-verified caller privilege to ctx.
func WithCallerAuth(ctx context.Context, auth CallerAuth) context.Context {
	return context.WithValue(ctx, callerAuthKey{}, auth)
}

// CallerAuthFromContext returns any server-attached CallerAuth.
func CallerAuthFromContext(ctx context.Context) (CallerAuth, bool) {
	if ctx == nil {
		return CallerAuth{}, false
	}
	v, ok := ctx.Value(callerAuthKey{}).(CallerAuth)
	return v, ok
}

// CallerContext carries typed server-verified authorization context derived strictly
// from authentication middleware (never from untrusted JSON fields).
type CallerContext struct {
	Principal    policy.Principal
	IsOperator   bool
	DisallowBorg bool
	FromHTTP     bool
}

// CallerContextFor derives a typed CallerContext strictly from server-side authentication context.
// When called over HTTP/MCP/A2A (CallerAuth present), FromHTTP is true and untrusted JSON
// principal/level/callerPub fields are ignored so low-privilege tokens cannot escalate clearance
// or spoof Borg members. Unauthenticated bare-metal loopback preserves IsOperator=true (owner Level 100),
// while agent APIToken and OAuth nkt_at_* tokens receive IsOperator=false (agent Level 0, DisallowBorg=true).
func CallerContextFor(ctx context.Context, fallback policy.Principal) CallerContext {
	auth, ok := CallerAuthFromContext(ctx)
	if !ok {
		if fallback.ID == "" {
			fallback = policy.Principal{Type: "Agent", ID: "owner", Level: 100}
		}
		return CallerContext{
			Principal:    fallback,
			IsOperator:   true,
			DisallowBorg: false,
			FromHTTP:     false,
		}
	}
	if auth.IsOperator {
		return CallerContext{
			Principal:    policy.Principal{Type: "Agent", ID: "owner", Level: 100},
			IsOperator:   true,
			DisallowBorg: false,
			FromHTTP:     true,
		}
	}
	return CallerContext{
		Principal:    policy.Principal{Type: "Agent", ID: "agent", Level: 0},
		IsOperator:   false,
		DisallowBorg: true,
		FromHTTP:     true,
	}
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	return isLoopbackHost(u.Host)
}

func isHTTPSRequest(r *http.Request, publicURL string) bool {
	if r != nil {
		if r.TLS != nil {
			return true
		}
		if os.Getenv("NEKTAR_TRUST_PROXY") == "1" && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
			return true
		}
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(publicURL)), "https://")
}

func isLoopbackHost(hostport string) bool {
	h := strings.TrimSpace(strings.ToLower(hostport))
	if h == "" {
		return false
	}
	if parsed, _, err := net.SplitHostPort(h); err == nil {
		h = parsed
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		return true
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		return addr.IsLoopback()
	}
	return false
}

// isValidHostHeader validates RFC host[:port] without HTML-special characters, whitespace, or path separators.
func isValidHostHeader(hostport string) bool {
	h := strings.TrimSpace(hostport)
	if h == "" || len(h) > 255 {
		return false
	}
	for _, c := range h {
		if c <= 0x20 || c >= 0x7f || strings.ContainsRune(`<>'"/\@?#`, c) {
			return false
		}
	}
	host := h
	if parsed, port, err := net.SplitHostPort(h); err == nil {
		host = parsed
		if port == "" {
			return false
		}
		for _, c := range port {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	// DNS hostname characters: letters, digits, hyphen, dot.
	for _, c := range host {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// extractClientIP safely parses the client IP, reading the rightmost untrusted hop from X-Forwarded-For
// only when NEKTAR_TRUST_PROXY=1 and normalizing IPv4/IPv6 with netip.ParseAddr.
func extractClientIP(r *http.Request) string {
	remoteHost := strings.TrimSpace(r.RemoteAddr)
	if h, _, err := net.SplitHostPort(remoteHost); err == nil {
		remoteHost = h
	}
	remoteHost = strings.Trim(remoteHost, "[]")
	if os.Getenv("NEKTAR_TRUST_PROXY") == "1" {
		if xf := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xf != "" {
			parts := strings.Split(xf, ",")
			// Walk from right to left so spoofed leftmost client-injected headers cannot bypass rate limits.
			for i := len(parts) - 1; i >= 0; i-- {
				cand := strings.TrimSpace(parts[i])
				if addr, err := netip.ParseAddr(cand); err == nil {
					addr = addr.Unmap()
					if !addr.IsLoopback() && !addr.IsPrivate() && !addr.IsLinkLocalUnicast() {
						return addr.String()
					}
					remoteHost = addr.String()
				}
			}
		}
	}
	if addr, err := netip.ParseAddr(remoteHost); err == nil {
		return addr.Unmap().String()
	}
	return remoteHost
}

const maxRateLimitKeys = 4096

// normalizeRateLimitIP maps IPv6 addresses onto their /64 prefix so a single residential
// or cloud IPv6 allocation cannot bypass per-IP rate limits or exhaust memory via /128 rotation.
func normalizeRateLimitIP(ip string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	if addr.Is6() {
		return netip.PrefixFrom(addr, 64).Masked().String()
	}
	return addr.String()
}

// allowRateLimitedIP enforces a sliding-window per-IP rate limit with IPv6 /64 normalization,
// amortized expired-key pruning (preventing O(N^2) scans), and a hard map cap (fail-closed 429).
func allowRateLimitedIP(mu *sync.Mutex, buckets map[string][]int64, lastPrune *int64, rawIP string, limit int, windowSec int64, maxKeys int) bool {
	ip := normalizeRateLimitIP(rawIP)
	now := time.Now().Unix()
	mu.Lock()
	defer mu.Unlock()

	if maxKeys <= 0 {
		maxKeys = maxRateLimitKeys
	}
	pruneExpired := func() {
		for k, times := range buckets {
			active := false
			for _, ts := range times {
				if now-ts < windowSec {
					active = true
					break
				}
			}
			if !active {
				delete(buckets, k)
			}
		}
		if lastPrune != nil {
			*lastPrune = now
		}
	}

	prev, exists := buckets[ip]
	if !exists && len(buckets) >= maxKeys {
		if lastPrune == nil || *lastPrune == 0 || now-*lastPrune >= 60 {
			pruneExpired()
		}
		if len(buckets) >= maxKeys {
			return false
		}
	}

	kept := prev[:0]
	for _, ts := range prev {
		if now-ts < windowSec {
			kept = append(kept, ts)
		}
	}
	if len(kept) >= limit {
		buckets[ip] = kept
		return false
	}
	buckets[ip] = append(kept, now)
	return true
}

func (a *App) allowOAuthRegisterIP(r *http.Request) bool {
	return allowRateLimitedIP(&a.oauthRegMu, a.oauthRegIPs, &a.oauthRegLastPrune, extractClientIP(r), 20, 3600, maxRateLimitKeys)
}

func (a *App) isAllowedOrigin(origin string, r *http.Request) bool {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if a.cfg.PublicURL != "" {
		if pu, pErr := url.Parse(a.cfg.PublicURL); pErr == nil && pu.Host != "" && strings.EqualFold(u.Host, pu.Host) {
			return true
		}
	}
	return false
}

// withBrowserOriginGuard blocks DNS-rebinding Host headers on loopback listeners,
// rejects cross-site browser CSRF requests on mutation routes, and requires application/json
// for browser-originated JSON API mutations.
func (a *App) withBrowserOriginGuard(next http.Handler) http.Handler {
	loopbackOnly := a.cfg.ListenAddr != "" && !IsNonLoopbackListen(a.cfg.ListenAddr)
	var publicHost string
	if a.cfg.PublicURL != "" {
		if pu, err := url.Parse(a.cfg.PublicURL); err == nil {
			publicHost = strings.ToLower(pu.Host)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. Block DNS rebinding against loopback listeners (accept loopback hosts, configured PublicURL host,
		//    or test httptest hostnames like example.com when r.RemoteAddr is empty/test).
		if loopbackOnly && r.Host != "" && r.RemoteAddr != "" && isLoopbackRequest(r) {
			hostLower := strings.ToLower(r.Host)
			if !isLoopbackHost(r.Host) && (publicHost == "" || hostLower != publicHost) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden host header"})
				return
			}
		}

		// 2. For state-changing methods, enforce Origin / Sec-Fetch-Site CSRF protection.
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			p := r.URL.Path
			// Public signed peer/hub/faucet endpoints are designed for server-to-server calls.
			isPublicServerRoute := p == "/v1/market/purchase" || p == "/v1/faucet" || strings.HasPrefix(p, "/hub/")
			if !isPublicServerRoute {
				sfs := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
				if sfs == "cross-site" {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request blocked"})
					return
				}
				if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
					if !a.isAllowedOrigin(origin, r) {
						writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request blocked"})
						return
					}
				}
				// Require application/json on JSON mutation endpoints whenever browser headers are present
				// OR when a non-JSON content type like text/plain is sent (blocks CORS-safelisted simple POST CSRF).
				isJSONMutation := strings.HasPrefix(p, "/v1/wallet/") && !strings.HasPrefix(p, "/v1/wallet/wire/") ||
					strings.HasPrefix(p, "/v1/onboard/") ||
					strings.HasPrefix(p, "/v1/policies") ||
					strings.HasPrefix(p, "/v1/hosts") ||
					strings.HasPrefix(p, "/v1/automation/") ||
					strings.HasPrefix(p, "/v1/tools/") ||
					strings.HasPrefix(p, "/v1/auth/") ||
					strings.HasPrefix(p, "/v1/market/listings/") ||
					strings.HasPrefix(p, "/v1/market/standing") ||
					strings.HasPrefix(p, "/v1/repos/") ||
					strings.HasPrefix(p, "/v1/dock/") ||
					strings.HasPrefix(p, "/v1/hubs/") ||
					strings.HasPrefix(p, "/v1/runtime/") ||
					p == "/v1/remember" || p == "/v1/recall" || p == "/v1/anchor" || p == "/v1/answer" || p == "/a2a"
				if isJSONMutation && r.ContentLength != 0 {
					ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
					allowOctet := p == "/v1/repos/import" || p == "/v1/repos/inspect-capsule"
					if ct != "" && !strings.HasPrefix(ct, "application/json") && !(allowOctet && strings.HasPrefix(ct, "application/octet-stream")) {
						writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
						return
					}
				}
			}
		}

		next.ServeHTTP(w, r)
	})
}

func (a *App) mcpPaywallKey(method, name, uri string) string {
	switch method {
	case "tools/call":
		if name != "" && a.cfg.ToolPrices[name] > 0 {
			return name
		}
	case "resources/read":
		if a.cfg.ToolPrices["resources/read"] > 0 {
			return "resources/read"
		}
		if uri == "nektar://welcome" && a.cfg.ToolPrices["recall"] > 0 {
			return "recall"
		}
		if (uri == "nektar://repos" || strings.HasPrefix(uri, "nektar://repo/")) && a.cfg.ToolPrices["repo"] > 0 {
			return "repo"
		}
	}
	return ""
}

// withMCPPaywall inspects Streamable HTTP /mcp JSON-RPC tool and resource calls and enforces configured
// NEKTAR_TOOL_PRICES via the exact same BRC-105 / HTTP 402 paywall used by REST /v1/* routes.
func (a *App) withMCPPaywall(next http.Handler) http.Handler {
	if len(a.cfg.ToolPrices) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		type mcpRPC struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
				URI  string `json:"uri"`
			} `json:"params"`
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) > 0 && trimmed[0] == '[' {
			// Inspect JSON-RPC batch arrays so paid tools/resources cannot bypass the MCP paywall via batching.
			var batch []mcpRPC
			if json.Unmarshal(trimmed, &batch) != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed JSON-RPC batch"})
				return
			}
			for _, item := range batch {
				if a.mcpPaywallKey(item.Method, item.Params.Name, item.Params.URI) != "" {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "batched paid MCP tool or resource calls are not permitted; submit one request per paid call"})
					return
				}
			}
			next.ServeHTTP(w, r)
			return
		}
		var rpc mcpRPC
		if json.Unmarshal(trimmed, &rpc) == nil {
			if pricedKey := a.mcpPaywallKey(rpc.Method, rpc.Params.Name, rpc.Params.URI); pricedKey != "" {
				a.withPaywall(pricedKey, next).ServeHTTP(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
