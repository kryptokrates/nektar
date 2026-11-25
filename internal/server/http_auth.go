package server

import (
	"bytes"
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
)

// CreatePairCode mints a single-use 5-minute operator pairing code for `GET /pair?code=...`.
// Must only be called from startup banner or operator-authenticated routes.
func (a *App) CreatePairCode() string {
	return a.CreateScopedPairCode(true)
}

// CreateScopedPairCode mints a single-use 5-minute pairing code explicitly scoped to operator or API privileges.
func (a *App) CreateScopedPairCode(forOperator bool) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("server: crypto/rand failed: " + err.Error())
	}
	code := hex.EncodeToString(b[:])
	clear(b[:])
	tok := a.cfg.APIToken
	if forOperator && a.cfg.OperatorToken != "" {
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

func (a *App) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.APIToken == "" && a.cfg.OperatorToken == "" {
			// Unauthenticated bare-metal loopback: local owner context.
			ctx := WithCallerAuth(r.Context(), CallerAuth{Authenticated: true, IsOperator: true})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		tok := a.extractToken(r)
		if !a.tokenMatches(tok, false) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		isOp := a.cfg.OperatorToken != "" && a.tokenMatches(tok, true)
		ctx := WithCallerAuth(r.Context(), CallerAuth{Authenticated: true, IsOperator: isOp})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *App) withOperatorAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.OperatorToken == "" && a.cfg.APIToken == "" {
			ctx := WithCallerAuth(r.Context(), CallerAuth{Authenticated: true, IsOperator: true})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		tok := a.extractToken(r)
		// MCP OAuth tokens (nkt_at_*) are strictly scoped to mcp:tools and NEVER grant operator privileges,
		// even when OperatorToken is unset and APIToken is configured!
		if strings.HasPrefix(tok, "nkt_at_") {
			if a.matchOAuthToken(tok, false) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "operator token required"})
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "operator token required"})
			return
		}
		needOp := a.cfg.OperatorToken != ""
		if !a.tokenMatches(tok, needOp) {
			code := http.StatusUnauthorized
			if needOp && a.tokenMatches(tok, false) {
				code = http.StatusForbidden
			}
			writeJSON(w, code, map[string]string{"error": "operator token required"})
			return
		}
		ctx := WithCallerAuth(r.Context(), CallerAuth{Authenticated: true, IsOperator: true})
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
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
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
		http.Error(w, "invalid or expired pairing code", http.StatusUnauthorized)
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
		hdr := strings.TrimSpace(r.Header.Get("X-BSV-Payment"))
		addr, _ := a.mem.Chain().FundAddress()
		idPub := a.mem.Chain().IdentityPubHex()
		if hdr == "" {
			w.Header().Set("X-BSV-Payment-Amount", strconv.FormatUint(price, 10))
			w.Header().Set("X-BSV-Payment-Destination", addr)
			w.Header().Set("X-BSV-Payment-Identity", idPub)
			w.Header().Set("X-BSV-Payment-Protocol", "BRC-105")
			writeJSON(w, http.StatusPaymentRequired, map[string]any{
				"error":       "payment_required",
				"tool":        toolName,
				"amountSats":  price,
				"destination": addr,
				"identityKey": idPub,
			})
			return
		}
		var pay BRC105PaymentHeader
		if err := json.Unmarshal([]byte(hdr), &pay); err != nil {
			writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "invalid X-BSV-Payment header"})
			return
		}

		// Verified AtomicBEEF path (BRC-105 / BRC-29 or direct P2PKH funding output):
		if pay.BEEFHex != "" {
			rawBeef, err := hex.DecodeString(strings.TrimSpace(pay.BEEFHex))
			if err != nil {
				writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "invalid X-BSV-Payment beefHex"})
				return
			}
			_, tx, txHash, err := guard.ParseBEEF(rawBeef, guard.DefaultBound)
			if err != nil || tx == nil || txHash == nil {
				writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "unparseable X-BSV-Payment AtomicBEEF"})
				return
			}
			txid := txHash.String()
			inKeys := make([]string, 0, len(tx.Inputs))
			for _, in := range tx.Inputs {
				if in.SourceTXID != nil {
					inKeys = append(inKeys, fmt.Sprintf("%s:%d", in.SourceTXID.String(), in.SourceTxOutIndex))
				}
			}
			// On live networks (regtest/testnet/main), strictly require BRC-29 derived invoice remittance
			// (blocks replay of publicly visible historical mined deposits to static FundAddress()!),
			// and validate BRC-29 locking script + paidSats >= price BEFORE ClaimPaymentAtomic or RelayExternalTx!
			isBRC29 := pay.DerivationPrefix != "" && pay.DerivationSuffix != "" && pay.SenderIdentityKey != ""
			if a.mem.Chain().Network() != "memory" && !isBRC29 {
				writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "BRC-29 payment remittance (derivationPrefix, derivationSuffix, senderIdentityKey) is required on live networks"})
				return
			}
			var expectedPaid uint64
			if isBRC29 {
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
					return
				}
				if uint64(pay.OutputIndex) >= uint64(len(tx.Outputs)) {
					writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "BRC-29 outputIndex out of range"})
					return
				}
				out := tx.Outputs[pay.OutputIndex]
				if out.LockingScript == nil || !bytes.Equal(*out.LockingScript, *wantScript) {
					writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "BRC-29 payment output does not match derived locking script"})
					return
				}
				expectedPaid = out.Satoshis
			} else {
				fundScript, fErr := a.mem.Chain().Signer().FundScript()
				if fErr != nil {
					writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "fund script unavailable"})
					return
				}
				for _, out := range tx.Outputs {
					if out.LockingScript != nil && bytes.Equal(*out.LockingScript, *fundScript) {
						expectedPaid += out.Satoshis
					}
				}
			}
			if expectedPaid < price {
				writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": fmt.Sprintf("insufficient payment (%d < %d sats)", expectedPaid, price)})
				return
			}

			// Full consensus SPV verification (spv.Verify + REFUSED-BUMP + tracker outage separation + settleDepth +
			// per-tx/aggregate 0-conf caps + maxUnconfirmedAncestors + conflict watch) BEFORE replay claim or broadcast!
			var settledAtDepth bool
			if a.mem.Chain().Network() != "memory" {
				zeroConfMax := a.mem.Chain().ZeroConfMaxSats()
				zeroConfAgg := a.mem.Chain().ZeroConfMaxAggregateSats()
				maxAnc := a.mem.Chain().MaxUnconfirmedAncestors()
				if a.mkt != nil && a.mkt.Policy() != nil {
					pMax, pAgg, pAnc := a.mkt.Policy().ZeroConfLimits()
					if pMax > 0 || zeroConfMax == 0 {
						zeroConfMax = pMax
					}
					if pAgg > 0 || zeroConfAgg == 0 {
						zeroConfAgg = pAgg
					}
					if pAnc > 0 {
						maxAnc = pAnc
					}
				}
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
						return
					}
					if errors.Is(vErr, chain.ErrConflictDetected) {
						writeJSON(w, http.StatusConflict, map[string]any{"error": "payment_conflict", "reason": vErr.Error()})
						return
					}
					writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "payment SPV verification failed: " + vErr.Error()})
					return
				}
				if ver.NeedsConfirmation {
					writeJSON(w, http.StatusPaymentRequired, map[string]any{
						"error":         "confirmation_required",
						"reason":        ver.EscalationReason,
						"requiredDepth": a.mem.Chain().SettleDepth(),
						"txid":          txid,
					})
					return
				}
				settledAtDepth = ver.SettledAtDepth
			}

			// Atomic cross-interface SQLite payment claim registry (shared with /v1/market/purchase).
			if err := a.mem.DB().ClaimPaymentAtomic(txid, inKeys); err != nil {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "payment_rejected", "reason": err.Error()})
				return
			}

			// Broadcast unmined payment once without double-broadcasting or poisoning seller SpendGuard:
			// in embedded BRC-29 mode, InternalizeBRC29 performs SPV + single WithIncomingInternalize broadcast.
			if a.mem.Chain().Network() != "memory" && !settledAtDepth && (!isBRC29 || a.mem.Chain().WalletMode() != "embedded") {
				if rErr := a.mem.Chain().RelayExternalTx(r.Context(), tx); rErr != nil {
					_ = a.mem.DB().ReleasePaymentClaim(txid)
					writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "unconfirmed payment broadcast failed: " + rErr.Error()})
					return
				}
			}

			var paid uint64
			if isBRC29 {
				paid, err = a.mem.Chain().InternalizeBRC29(
					r.Context(), rawBeef, pay.OutputIndex,
					pay.DerivationPrefix, pay.DerivationSuffix, pay.SenderIdentityKey,
					"BRC-105 payment for "+toolName, []string{"brc105", toolName},
				)
				if err != nil {
					_ = a.mem.DB().ReleasePaymentClaim(txid)
					writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "BRC-29 payment verification failed: " + err.Error()})
					return
				}
			} else {
				paid, err = a.mem.Chain().AdoptReceived(r.Context(), tx)
				if err != nil {
					_ = a.mem.DB().ReleasePaymentClaim(txid)
					writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "payment verification failed: " + err.Error()})
					return
				}
			}
			if paid < price {
				_ = a.mem.DB().ReleasePaymentClaim(txid)
				writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": fmt.Sprintf("insufficient payment (%d < %d sats)", paid, price)})
				return
			}
			a.BroadcastDashboardEvent("wallet_payment")
			next.ServeHTTP(w, r)
			return
		}

		// Legacy unverified JSON header is strictly refused on live networks (regtest/testnet/main);
		// only accepted in local in-memory tests where no real BSV payment BEEF is required.
		if a.mem.Chain().Network() != "memory" {
			writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "beefHex payment transaction is required on live networks"})
			return
		}
		if pay.TxID == "" || pay.AmountSats < price {
			writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "invalid or insufficient X-BSV-Payment"})
			return
		}
		if err := a.mem.DB().ClaimPaymentAtomic(pay.TxID, pay.Inputs); err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "payment_rejected",
				"reason": err.Error(),
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}
