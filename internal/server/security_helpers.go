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

// PrincipalForContext derives a Cedar Principal strictly from server-side authentication context.
// When called over HTTP/MCP/A2A (CallerAuth present), untrusted JSON principal/level/callerPub
// fields are ignored so low-privilege tokens cannot escalate clearance or spoof Borg members.
func PrincipalForContext(ctx context.Context, fallback policy.Principal) (principal policy.Principal, disallowBorg bool, overridden bool) {
	auth, ok := CallerAuthFromContext(ctx)
	if !ok {
		return fallback, false, false
	}
	if auth.IsOperator {
		return policy.Principal{Type: "Agent", ID: "owner", Level: 100}, false, true
	}
	return policy.Principal{Type: "Agent", ID: "agent", Level: 0}, true, true
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

func (a *App) allowOAuthRegisterIP(r *http.Request) bool {
	ip := extractClientIP(r)
	now := time.Now().Unix()
	a.oauthRegMu.Lock()
	defer a.oauthRegMu.Unlock()
	if len(a.oauthRegIPs) > 4096 {
		for k, times := range a.oauthRegIPs {
			active := false
			for _, ts := range times {
				if now-ts < 3600 {
					active = true
					break
				}
			}
			if !active {
				delete(a.oauthRegIPs, k)
			}
		}
	}
	prev := a.oauthRegIPs[ip]
	kept := prev[:0]
	for _, ts := range prev {
		if now-ts < 3600 {
			kept = append(kept, ts)
		}
	}
	if len(kept) >= 20 {
		a.oauthRegIPs[ip] = kept
		return false
	}
	a.oauthRegIPs[ip] = append(kept, now)
	return true
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
					p == "/v1/remember" || p == "/v1/recall" || p == "/v1/anchor" || p == "/v1/answer" || p == "/a2a"
				if isJSONMutation && r.ContentLength != 0 {
					ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
					if ct != "" && !strings.HasPrefix(ct, "application/json") {
						writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
						return
					}
				}
			}
		}

		next.ServeHTTP(w, r)
	})
}

// withMCPPaywall inspects Streamable HTTP /mcp JSON-RPC tool calls and enforces configured
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
			} `json:"params"`
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) > 0 && trimmed[0] == '[' {
			// Inspect JSON-RPC batch arrays so paid tools cannot bypass the MCP paywall via batching.
			var batch []mcpRPC
			if json.Unmarshal(trimmed, &batch) != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed JSON-RPC batch"})
				return
			}
			for _, item := range batch {
				if item.Method == "tools/call" && item.Params.Name != "" && a.cfg.ToolPrices[item.Params.Name] > 0 {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "batched paid MCP tool calls are not permitted; submit one request per paid tool call"})
					return
				}
			}
			next.ServeHTTP(w, r)
			return
		}
		var rpc mcpRPC
		if json.Unmarshal(trimmed, &rpc) == nil && rpc.Method == "tools/call" && rpc.Params.Name != "" {
			if a.cfg.ToolPrices[rpc.Params.Name] > 0 {
				a.withPaywall(rpc.Params.Name, next).ServeHTTP(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
