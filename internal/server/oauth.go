package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type oauthClient struct {
	ClientID     string
	ClientName   string
	RedirectURIs []string
	CreatedAt    time.Time
}

type oauthCode struct {
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	ExpiresAt     time.Time
}

type oauthAccessToken struct {
	ClientID  string
	ExpiresAt time.Time
}

func (a *App) registerOAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", a.handleOAuthProtectedResource)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", a.handleOAuthProtectedResource)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", a.handleOAuthAuthServer)
	mux.HandleFunc("POST /oauth/register", a.handleOAuthRegister)
	mux.HandleFunc("GET /oauth/authorize", a.handleOAuthAuthorize)
	mux.HandleFunc("POST /oauth/authorize", a.handleOAuthAuthorize)
	mux.HandleFunc("POST /oauth/token", a.handleOAuthToken)
}

func (a *App) oauthBaseURL(r *http.Request) string {
	if strings.TrimSpace(a.cfg.PublicURL) != "" {
		return strings.TrimRight(a.cfg.PublicURL, "/")
	}
	return requestBaseURL(r)
}

func (a *App) handleOAuthProtectedResource(w http.ResponseWriter, r *http.Request) {
	base := a.oauthBaseURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{"mcp:tools"},
	})
}

func (a *App) handleOAuthAuthServer(w http.ResponseWriter, r *http.Request) {
	base := a.oauthBaseURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{"mcp:tools"},
	})
}

func allowedOAuthRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" && u.Host != "" {
		return true
	}
	h := strings.ToLower(u.Hostname())
	return u.Scheme == "http" && (h == "localhost" || h == "127.0.0.1" || h == "::1")
}

const (
	maxOAuthClients = 1024
	maxOAuthCodes   = 4096
	maxOAuthTokens  = 4096
)

func (a *App) pruneOAuthLocked(now time.Time) {
	for k, c := range a.oauthCodes {
		if now.After(c.ExpiresAt) {
			delete(a.oauthCodes, k)
		}
	}
	activeClients := make(map[string]bool)
	for k, t := range a.oauthTokens {
		if now.After(t.ExpiresAt) {
			delete(a.oauthTokens, k)
		} else if t.ClientID != "" {
			activeClients[t.ClientID] = true
		}
	}
	if len(a.oauthClients) >= maxOAuthClients {
		// Evict oldest clients without active tokens first so unauthenticated registration floods
		// cannot evict legitimate active MCP clients.
		for len(a.oauthClients) >= maxOAuthClients {
			var oldestID string
			var oldestTime time.Time
			for id, c := range a.oauthClients {
				if activeClients[id] {
					continue
				}
				if oldestID == "" || c.CreatedAt.Before(oldestTime) {
					oldestID = id
					oldestTime = c.CreatedAt
				}
			}
			if oldestID == "" {
				break
			}
			delete(a.oauthClients, oldestID)
		}
	}
	if len(a.oauthCodes) >= maxOAuthCodes {
		for k := range a.oauthCodes {
			delete(a.oauthCodes, k)
			if len(a.oauthCodes) < maxOAuthCodes/2 {
				break
			}
		}
	}
	if len(a.oauthTokens) >= maxOAuthTokens {
		for k := range a.oauthTokens {
			delete(a.oauthTokens, k)
			if len(a.oauthTokens) < maxOAuthTokens/2 {
				break
			}
		}
	}
}

func (a *App) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	if !a.allowOAuthRegisterIP(r) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited", "error_description": "too many client registrations"})
		return
	}
	var in struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata"})
		return
	}
	if len(in.RedirectURIs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_redirect_uri"})
		return
	}
	for _, u := range in.RedirectURIs {
		if !allowedOAuthRedirect(u) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_redirect_uri", "error_description": "redirect_uri must be https or localhost"})
			return
		}
	}
	if strings.TrimSpace(in.ClientName) == "" {
		in.ClientName = "MCP Client"
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	cid := "nkt_c_" + base64.RawURLEncoding.EncodeToString(b[:])

	now := time.Now()
	a.oauthMu.Lock()
	a.pruneOAuthLocked(now)
	if len(a.oauthClients) >= maxOAuthClients {
		a.oauthMu.Unlock()
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "registration_capacity_reached"})
		return
	}
	a.oauthClients[cid] = oauthClient{ClientID: cid, ClientName: in.ClientName, RedirectURIs: in.RedirectURIs, CreatedAt: now}
	a.oauthMu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  cid,
		"client_name":                in.ClientName,
		"redirect_uris":              in.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
	})
}

func readOAuthParams(r *http.Request) map[string]string {
	out := make(map[string]string)
	if r.Method == http.MethodGet {
		for k, vals := range r.URL.Query() {
			if len(vals) > 0 {
				out[k] = vals[0]
			}
		}
		return out
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/x-www-form-urlencoded") {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if vals, err := url.ParseQuery(string(raw)); err == nil {
			for k, v := range vals {
				if len(v) > 0 {
					out[k] = v[0]
				}
			}
		}
		return out
	}
	var j map[string]any
	if json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&j) == nil {
		for k, v := range j {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
	}
	return out
}

func oauthSecretKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (a *App) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	params := readOAuthParams(r)
	clientID := params["client_id"]
	redirectURI := params["redirect_uri"]
	state := params["state"]
	challenge := params["code_challenge"]
	method := params["code_challenge_method"]
	if method == "" {
		method = "S256"
	}

	a.oauthMu.Lock()
	client, ok := a.oauthClients[clientID]
	a.oauthMu.Unlock()
	uriAllowed := false
	if ok {
		for _, u := range client.RedirectURIs {
			if u == redirectURI {
				uriAllowed = true
				break
			}
		}
	}
	if !ok || !uriAllowed || challenge == "" || method != "S256" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "error_description": "unknown client_id, redirect_uri, or missing PKCE S256"})
		return
	}

	authRequired := a.cfg.APIToken != "" || a.cfg.OperatorToken != ""
	sessAuthed := !authRequired || a.tokenMatches(a.extractToken(r), false)

	renderForm := func(status int, errMsg string) {
		var csrfRaw [16]byte
		if _, err := rand.Read(csrfRaw[:]); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}
		csrfTok := base64.RawURLEncoding.EncodeToString(csrfRaw[:])
		http.SetCookie(w, &http.Cookie{
			Name:     "nektar_oauth_csrf",
			Value:    csrfTok,
			Path:     "/oauth/authorize",
			HttpOnly: true,
			Secure:   isHTTPSRequest(r, a.cfg.PublicURL),
			SameSite: http.SameSiteLaxMode,
			MaxAge:   600,
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		errBanner := ""
		if errMsg != "" {
			errBanner = fmt.Sprintf(`<div class="err">%s</div>`, html.EscapeString(errMsg))
		}
		tokenField := `<p style="color:#4cc26b">✓ Authenticated via active Nektar session</p>`
		if !sessAuthed {
			tokenField = `<label style="font-size:12.5px;color:#a89a8e">Nektar access token (NEKTAR_HTTP_TOKEN)</label><input type="password" name="token" required placeholder="paste token from startup banner">`
		}
		_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Authorize %s — nektar</title>
<style>body{background:#120e0c;color:#ece7e4;font:15px/1.5 system-ui,sans-serif;display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0;padding:16px}
.box{background:#211b17;border:1px solid #3a2f28;border-radius:14px;padding:24px;max-width:420px;width:100%%}
h1{font-size:18px;margin:0 0 8px;color:#f7991b}p{color:#a89a8e;font-size:13.5px}
.err{background:rgba(220,38,38,.16);border:1px solid #dc2626;border-radius:8px;padding:10px 12px;color:#fca5a5;font-size:13px;margin:10px 0}
input{width:100%%;box-sizing:border-box;padding:10px 12px;border-radius:8px;border:1px solid #3a2f28;background:#120e0c;color:#ece7e4;margin:10px 0 14px;font-family:ui-monospace,monospace}
button{width:100%%;padding:11px;border:0;border-radius:8px;background:#f7991b;color:#221e1d;font-weight:700;cursor:pointer}</style></head>
<body><form class="box" method="POST" action="/oauth/authorize">
<h1>Connect %s</h1>
<p>Allow this client to access your Nektar MCP memory tools at <code>%s</code>.</p>
%s
<input type="hidden" name="csrf_token" value="%s">
<input type="hidden" name="client_id" value="%s">
<input type="hidden" name="redirect_uri" value="%s">
<input type="hidden" name="state" value="%s">
<input type="hidden" name="code_challenge" value="%s">
<input type="hidden" name="code_challenge_method" value="S256">
%s
<button type="submit">Approve Connection</button></form></body></html>`,
			html.EscapeString(client.ClientName), html.EscapeString(client.ClientName), html.EscapeString(redirectURI),
			errBanner,
			html.EscapeString(csrfTok),
			html.EscapeString(clientID), html.EscapeString(redirectURI), html.EscapeString(state), html.EscapeString(challenge),
			tokenField)
	}

	if r.Method == http.MethodGet {
		renderForm(http.StatusOK, "")
		return
	}

	// Verify per-request CSRF cookie + form nonce on POST /oauth/authorize to block same-site form auto-approval.
	csrfCookie, cErr := r.Cookie("nektar_oauth_csrf")
	csrfForm := strings.TrimSpace(params["csrf_token"])
	if cErr != nil || csrfCookie.Value == "" || csrfForm == "" || subtle.ConstantTimeCompare([]byte(csrfCookie.Value), []byte(csrfForm)) != 1 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid_request", "error_description": "missing or invalid oauth csrf token"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "nektar_oauth_csrf",
		Value:    "",
		Path:     "/oauth/authorize",
		HttpOnly: true,
		Secure:   isHTTPSRequest(r, a.cfg.PublicURL),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})

	presented := strings.TrimSpace(params["token"])
	if !sessAuthed {
		if presented == "" || !a.tokenMatches(presented, false) {
			if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json") &&
				!strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html") {
				writeJSON(w, http.StatusUnauthorized, map[string]string{
					"error":             "access_denied",
					"error_description": "invalid nektar token",
					"hint":              "Paste the token from ~/.nektar/http-token or startup banner, or run 'nektar open' first.",
				})
				return
			}
			renderForm(http.StatusUnauthorized, "Invalid Nektar access token. Check ~/.nektar/http-token (or startup banner) and try again.")
			return
		}
	}

	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	code := base64.RawURLEncoding.EncodeToString(b[:])
	codeKey := oauthSecretKey(code)
	a.oauthMu.Lock()
	now := time.Now()
	a.pruneOAuthLocked(now)
	a.oauthCodes[codeKey] = oauthCode{
		ClientID:      clientID,
		RedirectURI:   redirectURI,
		CodeChallenge: challenge,
		ExpiresAt:     now.Add(5 * time.Minute),
	}
	a.oauthMu.Unlock()

	dest, _ := url.Parse(redirectURI)
	q := dest.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	dest.RawQuery = q.Encode()
	http.Redirect(w, r, dest.String(), http.StatusFound)
}

func (a *App) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	params := readOAuthParams(r)
	if params["grant_type"] != "authorization_code" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	codeKey := oauthSecretKey(params["code"])
	a.oauthMu.Lock()
	entry, ok := a.oauthCodes[codeKey]
	delete(a.oauthCodes, codeKey)
	a.oauthMu.Unlock()
	if !ok || time.Now().After(entry.ExpiresAt) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "expired or unknown code"})
		return
	}
	if entry.ClientID != params["client_id"] || entry.RedirectURI != params["redirect_uri"] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "client or redirect mismatch"})
		return
	}
	verifier := params["code_verifier"]
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	if verifier == "" || subtle.ConstantTimeCompare([]byte(computed), []byte(entry.CodeChallenge)) != 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "PKCE code_verifier mismatch"})
		return
	}
	var tb [32]byte
	if _, err := rand.Read(tb[:]); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	accessTok := "nkt_at_" + base64.RawURLEncoding.EncodeToString(tb[:])
	tokKey := oauthSecretKey(accessTok)
	a.oauthMu.Lock()
	now := time.Now()
	a.pruneOAuthLocked(now)
	// MCP OAuth tokens are strictly scoped to mcp:tools and NEVER grant operator privileges.
	a.oauthTokens[tokKey] = oauthAccessToken{ClientID: entry.ClientID, ExpiresAt: now.Add(24 * time.Hour)}
	a.oauthMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": accessTok,
		"token_type":   "Bearer",
		"expires_in":   86400,
	})
}

func (a *App) matchOAuthToken(tok string, requireOperator bool) bool {
	if requireOperator || !strings.HasPrefix(tok, "nkt_at_") {
		return false
	}
	tokKey := oauthSecretKey(tok)
	a.oauthMu.Lock()
	defer a.oauthMu.Unlock()
	t, ok := a.oauthTokens[tokKey]
	if !ok {
		return false
	}
	if time.Now().After(t.ExpiresAt) {
		delete(a.oauthTokens, tokKey)
		return false
	}
	return true
}
