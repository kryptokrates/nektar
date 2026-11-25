package hub

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
)

type feeCollectOutcome struct {
	HTTPStatus int
	Code       string
	Error      string
	TxID       string
	PayerPub   string
	PaidSats   uint64
	Decision   string
}

// verifyAndCollectFee enforces BRC-105 payment verification, Lightweb acceptance limits,
// replay/conflict protection, and wallet internalization for any priced Hub action.
func (n *Node) verifyAndCollectFee(ctx context.Context, action, kind string, priceSats uint64, payHeader string) feeCollectOutcome {
	if priceSats == 0 {
		return feeCollectOutcome{HTTPStatus: http.StatusOK}
	}
	if n.feeCollector == nil {
		return feeCollectOutcome{
			HTTPStatus: http.StatusServiceUnavailable,
			Code:       "ERR_HUB_FEE_UNCONFIGURED",
			Error:      "hub priced action requires a configured FeeCollector wallet",
		}
	}
	payHeader = strings.TrimSpace(payHeader)
	if payHeader == "" {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_REQUIRED",
			Error:      fmt.Sprintf("payment of %d sats required for %s", priceSats, action),
		}
	}
	var pay BRC105PaymentHeader
	if err := json.Unmarshal([]byte(payHeader), &pay); err != nil {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_MALFORMED",
			Error:      "invalid X-BSV-Payment JSON header",
		}
	}
	if pay.BEEFHex == "" || pay.DerivationPrefix == "" || pay.DerivationSuffix == "" || pay.SenderIdentityKey == "" {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_INCOMPLETE",
			Error:      "X-BSV-Payment requires beefHex, derivationPrefix, derivationSuffix, and senderIdentityKey",
		}
	}
	if err := n.VerifyPrefix(pay.DerivationPrefix, action, priceSats, time.Now()); err != nil {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_PREFIX",
			Error:      err.Error(),
		}
	}
	rawBeef, err := hex.DecodeString(strings.TrimSpace(pay.BEEFHex))
	if err != nil || len(rawBeef) == 0 {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_MALFORMED",
			Error:      "invalid X-BSV-Payment beefHex",
		}
	}
	_, tx, txHash, err := guard.ParseBEEF(rawBeef, guard.DefaultBound)
	if err != nil || tx == nil || txHash == nil {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_MALFORMED",
			Error:      "unparseable X-BSV-Payment AtomicBEEF",
		}
	}
	txid := txHash.String()
	if pay.TxID != "" && !strings.EqualFold(strings.TrimSpace(pay.TxID), txid) {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_MALFORMED",
			Error:      "X-BSV-Payment txid does not match AtomicBEEF",
		}
	}
	if uint64(pay.OutputIndex) >= uint64(len(tx.Outputs)) {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_WRONG_SCRIPT",
			Error:      "X-BSV-Payment outputIndex out of range",
		}
	}

	// Derive expected BRC-29 locking script and assert output pays >= priceSats.
	wantScript, dErr := n.feeCollector.DeriveBRC29Script(ctx, pay.SenderIdentityKey, pay.DerivationPrefix, pay.DerivationSuffix)
	if dErr != nil || wantScript == nil {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_WRONG_SCRIPT",
			Error:      fmt.Sprintf("BRC-29 script derivation failed: %v", dErr),
		}
	}
	out := tx.Outputs[pay.OutputIndex]
	if out == nil || out.LockingScript == nil || !bytes.Equal(*out.LockingScript, *wantScript) {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_WRONG_SCRIPT",
			Error:      "payment output does not match derived BRC-29 locking script",
		}
	}
	paidSats := out.Satoshis
	if paidSats < priceSats {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_INSUFFICIENT",
			Error:      fmt.Sprintf("insufficient payment (%d < %d sats)", paidSats, priceSats),
		}
	}

	inKeys := make([]string, 0, len(tx.Inputs))
	for _, in := range tx.Inputs {
		if in != nil && in.SourceTXID != nil {
			inKeys = append(inKeys, fmt.Sprintf("%s:%d", in.SourceTXID.String(), in.SourceTxOutIndex))
		}
	}
	payerPub := strings.ToLower(strings.TrimSpace(pay.SenderIdentityKey))

	// Check Lightweb acceptance policy: flagged payer OR over fast threshold/payer/total exposure -> require mined.
	flagged, flagReason, _ := n.store.IsPayerFlagged(payerPub)
	payerExp, totalExp, _ := n.store.UnminedFastExposure(payerPub, int64(n.fees.AcceptWindow.Seconds()))
	holdReason := ""
	if flagged {
		holdReason = "payer flagged for prior unsettleable payment: " + flagReason
	} else if paidSats > n.fees.AcceptThresholdSats {
		holdReason = fmt.Sprintf("payment %d sats exceeds fast accept threshold %d sats", paidSats, n.fees.AcceptThresholdSats)
	} else if payerExp+paidSats > n.fees.AcceptPayerLimitSats {
		holdReason = fmt.Sprintf("payer unmined exposure %d+%d exceeds limit %d sats", payerExp, paidSats, n.fees.AcceptPayerLimitSats)
	} else if totalExp+paidSats > n.fees.AcceptTotalLimitSats {
		holdReason = fmt.Sprintf("hub total unmined exposure %d+%d exceeds limit %d sats", totalExp, paidSats, n.fees.AcceptTotalLimitSats)
	}

	// Run full local consensus SPV verification BEFORE replay claim or wallet internalization.
	settled := false
	var blockHeight uint32
	if n.feeCollector.Network() != "memory" {
		allowZeroConf := holdReason == ""
		ver, vErr := n.feeCollector.VerifyPaymentBEEF(ctx, rawBeef, chain.PaymentVerifyOpts{
			ExpectedAmountSats:       priceSats,
			ZeroConfMaxSats:          n.fees.AcceptThresholdSats,
			ZeroConfMaxAggregateSats: n.fees.AcceptTotalLimitSats,
			SettleDepth:              n.feeCollector.SettleDepth(),
			AllowZeroConf:            allowZeroConf,
			ConflictWatch:            n.feeCollector.ZeroConfWatch(),
			TrackOnZeroConf:          allowZeroConf,
		})
		if vErr != nil {
			if errors.Is(vErr, chain.ErrTrackerUnavailable) {
				return feeCollectOutcome{
					HTTPStatus: http.StatusServiceUnavailable,
					Code:       "ERR_TRACKER_UNAVAILABLE",
					Error:      vErr.Error(),
				}
			}
			if errors.Is(vErr, chain.ErrConflictDetected) {
				_ = n.store.FlagPayer(payerPub, txid, vErr.Error())
				return feeCollectOutcome{
					HTTPStatus: http.StatusConflict,
					Code:       "ERR_PAYMENT_CONFLICT",
					Error:      vErr.Error(),
					TxID:       txid,
				}
			}
			return feeCollectOutcome{
				HTTPStatus: http.StatusPaymentRequired,
				Code:       "ERR_PAYMENT_SPV_FAILED",
				Error:      vErr.Error(),
			}
		}
		if ver.NeedsConfirmation {
			// Record held claim so the payer can retry the exact same txid once mined without paying twice!
			if cErr := n.store.ClaimFeePaymentAtomic(txid, inKeys, action, payerPub, paidSats, "held"); cErr != nil {
				return mapClaimErr(cErr, txid)
			}
			_ = n.store.RecordFeeLedger(FeeLedgerEntry{
				TxID:     txid,
				Action:   action,
				Kind:     kind,
				PayerPub: payerPub,
				Sats:     paidSats,
				Decision: "hold",
				Status:   "held",
			})
			reason := holdReason
			if reason == "" {
				reason = ver.EscalationReason
			}
			return feeCollectOutcome{
				HTTPStatus: http.StatusAccepted,
				Code:       "ERR_PAYMENT_HELD",
				Error:      reason,
				TxID:       txid,
				PayerPub:   payerPub,
				PaidSats:   paidSats,
				Decision:   "hold",
			}
		}
		settled = ver.SettledAtDepth
		blockHeight = ver.BlockHeight
	} else {
		// In memory mode, still honor flagged-payer & explicit hold threshold when unmined so tests can verify 202 hold!
		if tx.MerklePath != nil {
			settled = true
			blockHeight = tx.MerklePath.BlockHeight
		} else if holdReason != "" {
			if cErr := n.store.ClaimFeePaymentAtomic(txid, inKeys, action, payerPub, paidSats, "held"); cErr != nil {
				return mapClaimErr(cErr, txid)
			}
			_ = n.store.RecordFeeLedger(FeeLedgerEntry{
				TxID:     txid,
				Action:   action,
				Kind:     kind,
				PayerPub: payerPub,
				Sats:     paidSats,
				Decision: "hold",
				Status:   "held",
			})
			return feeCollectOutcome{
				HTTPStatus: http.StatusAccepted,
				Code:       "ERR_PAYMENT_HELD",
				Error:      holdReason,
				TxID:       txid,
				PayerPub:   payerPub,
				PaidSats:   paidSats,
				Decision:   "hold",
			}
		}
	}

	// Atomic anti-replay & anti-conflict claim BEFORE internalizing or admitting.
	if cErr := n.store.ClaimFeePaymentAtomic(txid, inKeys, action, payerPub, paidSats, "claimed"); cErr != nil {
		return mapClaimErr(cErr, txid)
	}

	// Internalize directly into the Hub's wallet (exempt from Hub outgoing SpendGuard!).
	if _, iErr := n.feeCollector.InternalizeBRC29(ctx, rawBeef, pay.OutputIndex, pay.DerivationPrefix, pay.DerivationSuffix, pay.SenderIdentityKey, "hub fee "+action, []string{"nektar", "hub-fee", action}); iErr != nil {
		_ = n.store.ReleaseFeeClaim(txid)
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_INTERNALIZE",
			Error:      fmt.Sprintf("internalize hub fee failed: %v", iErr),
		}
	}

	decision := "fast"
	status := "unmined"
	if settled {
		decision = "mined"
		status = "mined"
	}
	_ = n.store.RecordFeeLedger(FeeLedgerEntry{
		TxID:        txid,
		Action:      action,
		Kind:        kind,
		PayerPub:    payerPub,
		Sats:        paidSats,
		Decision:    decision,
		Status:      status,
		BlockHeight: blockHeight,
	})
	return feeCollectOutcome{
		HTTPStatus: http.StatusOK,
		TxID:       txid,
		PayerPub:   payerPub,
		PaidSats:   paidSats,
		Decision:   decision,
	}
}

func mapClaimErr(err error, txid string) feeCollectOutcome {
	if errors.Is(err, ErrFeePaymentConflict) {
		return feeCollectOutcome{
			HTTPStatus: http.StatusConflict,
			Code:       "ERR_PAYMENT_CONFLICT",
			Error:      err.Error(),
			TxID:       txid,
		}
	}
	return feeCollectOutcome{
		HTTPStatus: http.StatusConflict,
		Code:       "ERR_PAYMENT_REPLAYED",
		Error:      err.Error(),
		TxID:       txid,
	}
}

// writeFeeOutcome writes standard Lightweb/BRC-105 challenge headers & JSON body for non-200 fee outcomes.
func (n *Node) writeFeeOutcome(w http.ResponseWriter, action string, priceSats uint64, out feeCollectOutcome) {
	if out.HTTPStatus == http.StatusPaymentRequired {
		prefix := n.IssuePrefix(action, priceSats, time.Now())
		payeePub := n.PayeeIdentityKey()
		w.Header().Set("X-BSV-Payment-Version", "1.0")
		w.Header().Set("X-BSV-Payment-Protocol", "BRC-105")
		w.Header().Set("X-BSV-Payment-Satoshis-Required", strconv.FormatUint(priceSats, 10))
		w.Header().Set("X-BSV-Payment-Amount", strconv.FormatUint(priceSats, 10))
		w.Header().Set("X-BSV-Payment-Derivation-Prefix", prefix)
		w.Header().Set("X-BSV-Payment-Identity", payeePub)
		writeJSON(w, http.StatusPaymentRequired, map[string]any{
			"status":           "error",
			"code":             out.Code,
			"error":            out.Error,
			"action":           action,
			"amountSats":       priceSats,
			"identityKey":      payeePub,
			"derivationPrefix": prefix,
			"termsPath":        "/v1/terms",
			"topupPath":        "/v1/credits/topup",
		})
		return
	}
	body := map[string]any{
		"status": "error",
		"code":   out.Code,
		"error":  out.Error,
		"action": action,
	}
	if out.TxID != "" {
		body["txid"] = out.TxID
	}
	if out.HTTPStatus == http.StatusAccepted {
		body["held"] = true
	}
	writeJSON(w, out.HTTPStatus, body)
}

// authenticateEgressCaller verifies signed X-Nektar-Pub / X-Nektar-Ts / X-Nektar-Nonce / X-Nektar-Sig headers
// on metered GET /events and GET /sync requests (same replay-safe contract as GET /bbox/inbox).
func (n *Node) authenticateEgressCaller(r *http.Request, action string, cursor int64, domainsCSV string) (pub string, isSpinePeer bool, err error) {
	hdrPub := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Nektar-Pub")))
	hdrTs := strings.TrimSpace(r.Header.Get("X-Nektar-Ts"))
	hdrNonce := strings.TrimSpace(r.Header.Get("X-Nektar-Nonce"))
	hdrSig := strings.TrimSpace(r.Header.Get("X-Nektar-Sig"))
	if hdrPub == "" || hdrTs == "" || hdrNonce == "" || hdrSig == "" {
		return "", false, errors.New("signed X-Nektar-Pub / X-Nektar-Ts / X-Nektar-Nonce / X-Nektar-Sig headers required for metered hub egress")
	}
	ts, tsErr := strconv.ParseInt(hdrTs, 10, 64)
	now := time.Now().Unix()
	if tsErr != nil || ts < now-bboxSkewSeconds || ts > now+bboxSkewSeconds {
		return "", false, errors.New("expired or invalid X-Nektar-Ts")
	}
	aud := strings.ToLower(strings.TrimSpace(r.Host))
	authPayload := map[string]any{
		"action":  action,
		"pub":     hdrPub,
		"ts":      ts,
		"nonce":   hdrNonce,
		"aud":     aud,
		"cursor":  cursor,
		"domains": strings.ToLower(strings.TrimSpace(domainsCSV)),
	}
	if vErr := bstack.VerifyArtifact(hdrPub, hdrSig, authPayload); vErr != nil {
		return "", false, errors.New("invalid hub egress signature")
	}
	if !n.claimBboxNonce("egress:"+action+":"+hdrPub, hdrNonce, now) {
		return "", false, errors.New("replayed hub egress nonce")
	}
	return hdrPub, n.IsSpinePeer(hdrPub), nil
}

// domainsOnlyDir reports whether the caller explicitly requested ONLY the free `dir` discovery lane.
func domainsOnlyDir(doms []string) bool {
	if len(doms) == 0 {
		return false
	}
	for _, d := range doms {
		if !strings.EqualFold(strings.TrimSpace(d), bstack.DirDomain) {
			return false
		}
	}
	return true
}

// classifyLookupClass determines the pricing class for a BRC-24 ls_nektar lookup question.
// Standard live queries are "live" (free); historical cursor walks are "history"; large unscoped dumps are "export".
func classifyLookupClass(q *lookup.LookupQuestion) string {
	if q == nil || q.Service != bstack.LookupService || len(q.Query) == 0 {
		return "live"
	}
	var raw map[string]any
	if json.Unmarshal(q.Query, &raw) == nil {
		if cls, ok := raw["class"].(string); ok && strings.TrimSpace(cls) != "" {
			return strings.ToLower(strings.TrimSpace(cls))
		}
	}
	var mq bstack.MarketQuery
	if json.Unmarshal(q.Query, &mq) != nil {
		return "live"
	}
	if mq.Kind == bstack.KindAnnounce && strings.EqualFold(strings.TrimSpace(mq.Domain), bstack.DirDomain) {
		return "live"
	}
	if mq.SinceMs > 0 || mq.SinceSeq > 0 {
		return "history"
	}
	if mq.Limit > 100 && mq.WantID == "" && mq.OfferID == "" && mq.Domain == "" && mq.Pub == "" {
		return "export"
	}
	return "live"
}

func (n *Node) handleCreditsTopup(w http.ResponseWriter, r *http.Request) {
	minTopup := n.fees.EgressMinTopupSats
	if minTopup == 0 {
		minTopup = defaultMinTopupSats
	}
	// Optional JSON body may name a beneficiary pubkey (defaults to BRC-29 senderIdentityKey).
	var body struct {
		PayerPub string `json:"payerPub,omitempty"`
	}
	if r.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 16<<10))
		if len(bytes.TrimSpace(raw)) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
	}
	out := n.verifyAndCollectFee(r.Context(), ActionEgressTopup, "egress", minTopup, r.Header.Get("X-BSV-Payment"))
	if out.HTTPStatus != http.StatusOK {
		n.writeFeeOutcome(w, ActionEgressTopup, minTopup, out)
		return
	}
	creditPub := strings.ToLower(strings.TrimSpace(body.PayerPub))
	if creditPub == "" {
		creditPub = out.PayerPub
	}
	cred, err := n.store.CreditEgressMilliSats(creditPub, int64(out.PaidSats)*1000, out.PaidSats)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var eventsRemaining int64
	if perEv := n.fees.EgressMilliSatsPerEvent(); perEv > 0 {
		eventsRemaining = cred.BalanceMilliSats / perEv
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"txid":             out.TxID,
		"payerPub":         cred.PayerPub,
		"creditedSats":     out.PaidSats,
		"balanceMilliSats": cred.BalanceMilliSats,
		"eventsRemaining":  eventsRemaining,
		"decision":         out.Decision,
	})
}

func (n *Node) handleCreditsBalance(w http.ResponseWriter, r *http.Request) {
	pub := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("pub")))
	if pub == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "pub query parameter required"})
		return
	}
	cred, err := n.store.GetEgressCredit(pub)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var eventsRemaining int64
	if perEv := n.fees.EgressMilliSatsPerEvent(); perEv > 0 {
		eventsRemaining = cred.BalanceMilliSats / perEv
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"payerPub":             cred.PayerPub,
		"balanceMilliSats":     cred.BalanceMilliSats,
		"totalFundedSats":      cred.TotalFundedSats,
		"totalEventsDelivered": cred.TotalEventsDelivered,
		"eventsRemaining":      eventsRemaining,
		"satsPer100Events":     n.fees.EgressSatsPer100Events,
		"minTopupSats":         n.fees.EgressMinTopupSats,
	})
}
