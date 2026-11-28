package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/overlay/lookup"

	"github.com/kryptokrates/nektar/internal/bstack"
)

// enforceSubmitFee enforces Valve 1 submission fees on both POST /submit and POST /plane/publish
// (validate-before-charge: malformed payloads fail with 400 before any fee is taken).
// Authenticated bilateral Spine peers (signed X-Nektar-* headers matching IsSpinePeer) are exempt.
// Returns (collectedTxID, ok). When ok is false, the HTTP response has already been written.
func (n *Node) enforceSubmitFee(w http.ResponseWriter, r *http.Request, authAction string, rawBEEF []byte, topics []string) (string, bool) {
	if n.fees.SubmitWantSats == 0 && n.fees.SubmitOfferSats == 0 && n.fees.SubmitAnnounceSats == 0 {
		return "", true
	}
	if strings.TrimSpace(r.Header.Get("X-Nektar-Pub")) != "" {
		if _, isSpinePeer, authErr := n.authenticateEgressCaller(r, authAction, 0, strings.Join(topics, ",")); authErr == nil && isSpinePeer {
			return "", true
		}
	}
	kind, cErr := n.classifySubmitPayload(rawBEEF, topics)
	if cErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": cErr.Error()})
		return "", false
	}
	if price, action := n.fees.FeeForKind(kind); price > 0 {
		out := n.verifyAndCollectFee(r.Context(), action, string(kind), price, r.Header.Get("X-BSV-Payment"))
		if out.HTTPStatus != http.StatusOK {
			n.writeFeeOutcome(w, action, price, out)
			return "", false
		}
		return out.TxID, true
	}
	return "", true
}

// writeFeeOutcome writes standard Lightweb/BRC-105 challenge headers & JSON body for non-200 fee outcomes.
func (n *Node) writeFeeOutcome(w http.ResponseWriter, action string, priceSats uint64, out feeCollectOutcome) {
	if out.HTTPStatus == http.StatusPaymentRequired {
		prefix := n.IssuePrefix(action, priceSats, time.Now())
		payeePub := n.PayeeIdentityKey()
		w.Header().Set("X-BSV-Payment-Version", "1.0")
		w.Header().Set("X-BSV-Payment-Protocol", "BRC-105")
		w.Header().Set("X-BSV-Payment-Satoshis-Required", strconv.FormatUint(priceSats, 10))
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
	authPayload := bstack.HubEgressAuth{
		Action:  action,
		Pub:     hdrPub,
		TS:      ts,
		Nonce:   hdrNonce,
		Aud:     aud,
		Cursor:  cursor,
		Domains: strings.ToLower(strings.TrimSpace(domainsCSV)),
	}
	if vErr := bstack.VerifyArtifact(hdrPub, hdrSig, authPayload); vErr != nil {
		return "", false, errors.New("invalid hub egress signature")
	}
	if !n.claimBboxNonce("egress:"+action+":"+hdrPub, hdrNonce, now) {
		return "", false, errors.New("replayed hub egress nonce")
	}
	isSelf := n.PayeeIdentityKey() != "" && strings.EqualFold(hdrPub, n.PayeeIdentityKey())
	return hdrPub, isSelf || n.IsSpinePeer(hdrPub), nil
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
		if cls, ok := raw["class"].(string); ok {
			cls = strings.ToLower(strings.TrimSpace(cls))
			if cls != "" && !strings.Contains(cls, "|") {
				return cls
			}
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
