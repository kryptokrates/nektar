package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/market"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func resolvePaywallZeroConfLimits(chainMax, chainAgg uint64, chainAnc int, pol *market.Policy) (uint64, uint64, int) {
	zeroConfMax := chainMax
	zeroConfAgg := chainAgg
	maxAnc := chainAnc
	if pol != nil {
		pMax, pAgg, pAnc := pol.ZeroConfLimits()
		if pol.ActivePolicyHash() != "" {
			zeroConfMax = pMax
			zeroConfAgg = pAgg
		} else {
			if pMax > 0 || zeroConfMax == 0 {
				zeroConfMax = pMax
			}
			if pAgg > 0 || zeroConfAgg == 0 {
				zeroConfAgg = pAgg
			}
		}
		if pAnc > 0 {
			maxAnc = pAnc
		}
	}
	return zeroConfMax, zeroConfAgg, maxAnc
}

// CreatePairCode mints a single-use 5-minute operator pairing code for `GET /pair?code=...`.
// Must only be called from startup banner or operator-authenticated routes.
func (a *App) CreatePairCode() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("server: crypto/rand failed: " + err.Error())
	}
	code := hex.EncodeToString(b[:])
	clear(b[:])
	tok := a.cfg.APIToken
	if a.cfg.OperatorToken != "" {
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

func attachAgentContext(r *http.Request, ctx context.Context) context.Context {
	if agent := strings.TrimSpace(r.Header.Get("X-Nektar-Agent")); agent != "" {
		return chain.WithAgentID(ctx, agent)
	}
	return ctx
}

func (a *App) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.APIToken == "" && a.cfg.OperatorToken == "" {
			// Unauthenticated bare-metal loopback: local owner context.
			ctx := attachAgentContext(r, WithCallerAuth(r.Context(), CallerAuth{Authenticated: true, IsOperator: true}))
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		tok := a.extractToken(r)
		if !a.tokenMatches(tok, false) {
			if r.URL.Path == "/mcp" || strings.HasPrefix(r.URL.Path, "/mcp/") {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, a.oauthBaseURL(r)))
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "unauthorized",
				"hint":  "Pass Authorization: Bearer <token> (see ~/.nektar/http-token or startup banner), or run 'nektar open' for a browser session.",
			})
			return
		}
		isOp := !strings.HasPrefix(tok, "nkt_at_") && (a.cfg.OperatorToken == "" || a.tokenMatches(tok, true))
		ctx := attachAgentContext(r, WithCallerAuth(r.Context(), CallerAuth{Authenticated: true, IsOperator: isOp}))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *App) withOperatorAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.OperatorToken == "" && a.cfg.APIToken == "" {
			ctx := attachAgentContext(r, WithCallerAuth(r.Context(), CallerAuth{Authenticated: true, IsOperator: true}))
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		tok := a.extractToken(r)
		// MCP OAuth tokens (nkt_at_*) are strictly scoped to mcp:tools and NEVER grant operator privileges,
		// even when OperatorToken is unset and APIToken is configured!
		if strings.HasPrefix(tok, "nkt_at_") {
			if a.matchOAuthToken(tok, false) {
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error": "operator token required",
					"hint":  "This action requires the operator token (OAuth mcp:tools tokens cannot perform operator actions). See ~/.nektar/operator-token or run 'nektar open'.",
				})
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "operator token required",
				"hint":  "Pass Authorization: Bearer <operator-token> (see ~/.nektar/operator-token), or run 'nektar open' for a browser session.",
			})
			return
		}
		needOp := a.cfg.OperatorToken != ""
		if !a.tokenMatches(tok, needOp) {
			code := http.StatusUnauthorized
			hint := "Pass Authorization: Bearer <operator-token> (see ~/.nektar/operator-token), or run 'nektar open' for a browser session."
			if needOp && a.tokenMatches(tok, false) {
				code = http.StatusForbidden
				hint = "This action requires the operator token (not the agent HTTP token). See ~/.nektar/operator-token or run 'nektar open'."
			}
			writeJSON(w, code, map[string]string{
				"error": "operator token required",
				"hint":  hint,
			})
			return
		}
		ctx := attachAgentContext(r, WithCallerAuth(r.Context(), CallerAuth{Authenticated: true, IsOperator: true}))
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
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "invalid token",
			"hint":  "Paste the token from ~/.nektar/operator-token (or ~/.nektar/http-token), or run 'nektar open' for a fresh pairing link.",
		})
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
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Pairing Link Expired — nektar</title>
<style>body{background:#120e0c;color:#ece7e4;font:15px/1.5 system-ui,sans-serif;display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0;padding:16px}
.box{background:#211b17;border:1px solid #3a2f28;border-radius:14px;padding:24px;max-width:440px;width:100%}
h1{font-size:18px;margin:0 0 8px;color:#f7991b}p{color:#a89a8e;font-size:13.5px;margin:8px 0}
pre{background:#120e0c;border:1px solid #3a2f28;border-radius:8px;padding:10px 12px;color:#f7991b;font-family:ui-monospace,monospace;font-size:13px;overflow-x:auto}
a{color:#f7991b}</style></head><body><div class="box">
<h1>Pairing link expired or already used</h1>
<p>This single-use pairing code is invalid or expired pairing code (links expire after 5 minutes or one click).</p>
<p>Generate a fresh dashboard link in your terminal:</p>
<pre>nektar open</pre>
<p><small>Or open <a href="/">the dashboard</a> and sign in with your operator token.</small></p>
</div></body></html>`)
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
		if _, _, _, ok := a.verifyBRC105Payment(w, r, toolName, price, map[string]any{"tool": toolName}); !ok {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// verifyBRC105Payment enforces HTTP 402 BRC-105 + BRC-29 payment verification with atomic
// replay protection (`ClaimPaymentAtomic`) and consensus SPV checks on live networks.
// On failure or missing header, it writes the appropriate HTTP 402/409/503 JSON response and returns ok=false.
func (a *App) verifyBRC105Payment(
	w http.ResponseWriter,
	r *http.Request,
	label string,
	price uint64,
	extraChallenge map[string]any,
) (txid string, paid uint64, senderPub string, ok bool) {
	hdr := strings.TrimSpace(r.Header.Get("X-BSV-Payment"))
	addr, _ := a.mem.Chain().FundAddress()
	idPub := a.mem.Chain().IdentityPubHex()
	if hdr == "" {
		w.Header().Set("X-BSV-Payment-Satoshis-Required", strconv.FormatUint(price, 10))
		w.Header().Set("X-BSV-Payment-Destination", addr)
		w.Header().Set("X-BSV-Payment-Identity", idPub)
		w.Header().Set("X-BSV-Payment-Protocol", "BRC-105")
		body := map[string]any{
			"error":       "payment_required",
			"amountSats":  price,
			"destination": addr,
			"identityKey": idPub,
		}
		for k, v := range extraChallenge {
			body[k] = v
		}
		writeJSON(w, http.StatusPaymentRequired, body)
		return "", 0, "", false
	}
	if len(hdr) > 64<<10 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "X-BSV-Payment header exceeds 64 KiB cap"})
		return "", 0, "", false
	}
	var pay BRC105PaymentHeader
	if err := json.Unmarshal([]byte(hdr), &pay); err != nil {
		writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "invalid X-BSV-Payment header"})
		return "", 0, "", false
	}

	// Canonical verified AtomicBEEF + BRC-29 payment path on all live networks.
	// Lightweight `{txid, amountSats, inputs}` claims are accepted ONLY on the in-memory test network.
	if pay.BEEFHex == "" {
		if a.mem.Chain().Network() != "memory" {
			writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "beefHex payment transaction is required on live networks"})
			return "", 0, "", false
		}
		if pay.TxID == "" || pay.AmountSats < price {
			writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "invalid or insufficient X-BSV-Payment"})
			return "", 0, "", false
		}
		if err := a.mem.DB().ClaimPaymentAtomic(pay.TxID, pay.Inputs); err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "payment_rejected",
				"reason": err.Error(),
			})
			return "", 0, "", false
		}
		return strings.ToLower(strings.TrimSpace(pay.TxID)), pay.AmountSats, strings.ToLower(strings.TrimSpace(pay.SenderIdentityKey)), true
	}
	rawBeef, err := hex.DecodeString(strings.TrimSpace(pay.BEEFHex))
	if err != nil {
		writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "invalid X-BSV-Payment beefHex"})
		return "", 0, "", false
	}
	_, tx, txHash, err := guard.ParseBEEF(rawBeef, guard.DefaultBound)
	if err != nil || tx == nil || txHash == nil {
		writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "unparseable X-BSV-Payment AtomicBEEF"})
		return "", 0, "", false
	}
	txid = txHash.String()
	inKeys := make([]string, 0, len(tx.Inputs))
	for _, in := range tx.Inputs {
		if in.SourceTXID != nil {
			inKeys = append(inKeys, fmt.Sprintf("%s:%d", in.SourceTXID.String(), in.SourceTxOutIndex))
		}
	}
	isBRC29 := pay.DerivationPrefix != "" && pay.DerivationSuffix != "" && pay.SenderIdentityKey != ""
	if !isBRC29 {
		writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "BRC-29 payment remittance (derivationPrefix, derivationSuffix, senderIdentityKey) is required"})
		return "", 0, "", false
	}
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
		return "", 0, "", false
	}
	if uint64(pay.OutputIndex) >= uint64(len(tx.Outputs)) {
		writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "BRC-29 outputIndex out of range"})
		return "", 0, "", false
	}
	out := tx.Outputs[pay.OutputIndex]
	if out.LockingScript == nil || !bytes.Equal(*out.LockingScript, *wantScript) {
		writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "BRC-29 payment output does not match derived locking script"})
		return "", 0, "", false
	}
	expectedPaid := out.Satoshis
	if expectedPaid < price {
		writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": fmt.Sprintf("insufficient payment (%d < %d sats)", expectedPaid, price)})
		return "", 0, "", false
	}

	var settledAtDepth bool
	if a.mem.Chain().Network() != "memory" {
		var pol *market.Policy
		if a.mkt != nil {
			pol = a.mkt.Policy()
		}
		zeroConfMax, zeroConfAgg, maxAnc := resolvePaywallZeroConfLimits(
			a.mem.Chain().ZeroConfMaxSats(),
			a.mem.Chain().ZeroConfMaxAggregateSats(),
			a.mem.Chain().MaxUnconfirmedAncestors(),
			pol,
		)
		ver, vErr := chain.VerifyCounterpartyPaymentBEEF(r.Context(), rawBeef, a.mem.Chain().Tracker(), chain.PaymentVerifyOpts{
			ExpectedAmountSats:       price,
			MaxUnconfirmedAncestors:  maxAnc,
			ZeroConfMaxSats:          zeroConfMax,
			ZeroConfMaxAggregateSats: zeroConfAgg,
			SettleDepth:              a.mem.Chain().SettleDepth(),
			AllowZeroConf:            zeroConfMax > 0,
			ConflictWatch:            a.mem.Chain().ZeroConfWatch(),
			TrackOnZeroConf:          true,
		})
		if vErr != nil {
			if errors.Is(vErr, chain.ErrTrackerUnavailable) {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "header tracker unavailable: " + vErr.Error()})
				return "", 0, "", false
			}
			if errors.Is(vErr, chain.ErrConflictDetected) {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "payment_conflict", "reason": vErr.Error()})
				return "", 0, "", false
			}
			writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "payment SPV verification failed: " + vErr.Error()})
			return "", 0, "", false
		}
		if ver.NeedsConfirmation {
			writeJSON(w, http.StatusPaymentRequired, map[string]any{
				"error":         "confirmation_required",
				"reason":        ver.EscalationReason,
				"requiredDepth": a.mem.Chain().SettleDepth(),
				"txid":          txid,
			})
			return "", 0, "", false
		}
		settledAtDepth = ver.SettledAtDepth
	}

	if err := a.mem.DB().ClaimPaymentAtomic(txid, inKeys); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "payment_rejected", "reason": err.Error()})
		return "", 0, "", false
	}

	if a.mem.Chain().Network() != "memory" && !settledAtDepth && a.mem.Chain().WalletMode() != "embedded" {
		if rErr := a.mem.Chain().RelayExternalTx(r.Context(), tx); rErr != nil {
			_ = a.mem.DB().ReleasePaymentClaim(txid)
			writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "unconfirmed payment broadcast failed: " + rErr.Error()})
			return "", 0, "", false
		}
	}

	intCtx := r.Context()
	if tx != nil {
		intCtx = nwallet.WithPreverifiedTx(intCtx, tx)
	}
	paid, err = a.mem.Chain().InternalizeBRC29(
		intCtx, rawBeef, pay.OutputIndex,
		pay.DerivationPrefix, pay.DerivationSuffix, pay.SenderIdentityKey,
		"BRC-105 payment for "+label, []string{"brc105", label},
	)
	if err != nil {
		_ = a.mem.DB().ReleasePaymentClaim(txid)
		writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "BRC-29 payment verification failed: " + err.Error()})
		return "", 0, "", false
	}
	if paid < price {
		_ = a.mem.DB().ReleasePaymentClaim(txid)
		writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": fmt.Sprintf("insufficient payment (%d < %d sats)", paid, price)})
		return "", 0, "", false
	}
	a.BroadcastDashboardEvent("wallet_payment")
	return txid, paid, strings.ToLower(strings.TrimSpace(pay.SenderIdentityKey)), true
}
