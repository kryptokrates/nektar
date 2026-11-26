package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
)

func (n *Node) broadcastEvent(ev bstack.MarketEvent, domains []string) {
	n.mu.RLock()
	for ch, filter := range n.subs {
		if len(filter) > 0 && !matchesAnyDomain(domains, filter) {
			continue
		}
		select {
		case ch <- ev:
		default:
		}
	}
	n.mu.RUnlock()
}

// Subscribe registers a channel receiving live admitted market events (optional domain filter).
func (n *Node) Subscribe(domains ...string) (<-chan bstack.MarketEvent, func()) {
	ch, unsub, _ := n.TrySubscribe(domains...)
	return ch, unsub
}

// TrySubscribe registers an SSE subscriber if below maxHubSSESubs.
func (n *Node) TrySubscribe(domains ...string) (<-chan bstack.MarketEvent, func(), bool) {
	ch := make(chan bstack.MarketEvent, 128)
	filter := make(map[string]bool)
	for _, d := range domains {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			filter[d] = true
		}
	}
	n.mu.Lock()
	if len(n.subs) >= maxHubSSESubs {
		n.mu.Unlock()
		return nil, func() {}, false
	}
	n.subs[ch] = filter
	n.mu.Unlock()
	return ch, func() {
		n.mu.Lock()
		delete(n.subs, ch)
		close(ch)
		n.mu.Unlock()
	}, true
}

func (n *Node) handleSync(w http.ResponseWriter, r *http.Request) {
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	rawDoms := strings.TrimSpace(r.URL.Query().Get("domains"))
	var doms []string
	if rawDoms != "" {
		doms = strings.Split(rawDoms, ",")
	}
	msatsPerEvent := n.fees.EgressMilliSatsPerEvent()
	metered := msatsPerEvent > 0 && !domainsOnlyDir(doms)
	var callerPub string
	var isSpinePeer bool
	if metered {
		var authErr error
		callerPub, isSpinePeer, authErr = n.authenticateEgressCaller(r, "hub_sync", cursor, rawDoms)
		if authErr != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": authErr.Error()})
			return
		}
		// Optional inline BRC-105 credit top-up right on the /sync request.
		if !isSpinePeer && strings.TrimSpace(r.Header.Get("X-BSV-Payment")) != "" {
			out := n.verifyAndCollectFee(r.Context(), ActionEgressTopup, "egress", n.fees.EgressMinTopupSats, r.Header.Get("X-BSV-Payment"))
			if out.HTTPStatus != http.StatusOK {
				n.writeFeeOutcome(w, ActionEgressTopup, n.fees.EgressMinTopupSats, out)
				return
			}
			_, _ = n.store.CreditEgressMilliSats(callerPub, int64(out.PaidSats)*1000, out.PaidSats)
		}
		// Require at least 1 event worth of credit upfront so 0-credit free-riders cannot poll /sync endlessly.
		if !isSpinePeer {
			cred, _ := n.store.GetEgressCredit(callerPub)
			if cred.BalanceMilliSats < msatsPerEvent {
				n.writeFeeOutcome(w, ActionEgressTopup, n.fees.EgressMinTopupSats, feeCollectOutcome{
					HTTPStatus: http.StatusPaymentRequired,
					Code:       "ERR_PAYMENT_REQUIRED",
					Error:      fmt.Sprintf("egress credit balance %d msats below 1-event cost %d msats (top up >= %d sats at /v1/credits/topup)", cred.BalanceMilliSats, msatsPerEvent, n.fees.EgressMinTopupSats),
				})
				return
			}
		}
	}
	latestSeq := n.store.LatestSeq()
	w.Header().Set("X-Nektar-Latest-Seq", strconv.FormatInt(latestSeq, 10))
	if cursor > latestSeq {
		// Hub DB was reset/migrated so client cursor is ahead of the entire log; resync cleanly from 0.
		cursor = 0
	}
	items, err := n.store.EventsSince(cursor, doms, 200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if metered && !isSpinePeer {
		var billable int64
		for _, it := range items {
			if it.Kind != bstack.KindAnnounce {
				billable++
			}
		}
		if billable > 0 {
			if _, ok, cErr := n.store.ChargeEgressEvents(callerPub, billable, msatsPerEvent); cErr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": cErr.Error()})
				return
			} else if !ok {
				n.writeFeeOutcome(w, ActionEgressTopup, n.fees.EgressMinTopupSats, feeCollectOutcome{
					HTTPStatus: http.StatusPaymentRequired,
					Code:       "ERR_PAYMENT_REQUIRED",
					Error:      fmt.Sprintf("insufficient egress credits for %d market events (top up >= %d sats at /v1/credits/topup)", billable, n.fees.EgressMinTopupSats),
				})
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, items)
}

func (n *Node) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	var cursor int64
	if c := r.URL.Query().Get("cursor"); c != "" {
		cursor, _ = strconv.ParseInt(c, 10, 64)
	} else if lei := r.Header.Get("Last-Event-ID"); lei != "" {
		cursor, _ = strconv.ParseInt(strings.TrimSpace(lei), 10, 64)
	}
	rawDoms := strings.TrimSpace(r.URL.Query().Get("domains"))
	var doms []string
	if rawDoms != "" {
		doms = strings.Split(rawDoms, ",")
	}
	msatsPerEvent := n.fees.EgressMilliSatsPerEvent()
	metered := msatsPerEvent > 0 && !domainsOnlyDir(doms)
	var callerPub string
	var isSpinePeer bool
	if metered {
		var authErr error
		callerPub, isSpinePeer, authErr = n.authenticateEgressCaller(r, "hub_events", cursor, rawDoms)
		if authErr != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": authErr.Error()})
			return
		}
		if !isSpinePeer && strings.TrimSpace(r.Header.Get("X-BSV-Payment")) != "" {
			out := n.verifyAndCollectFee(r.Context(), ActionEgressTopup, "egress", n.fees.EgressMinTopupSats, r.Header.Get("X-BSV-Payment"))
			if out.HTTPStatus != http.StatusOK {
				n.writeFeeOutcome(w, ActionEgressTopup, n.fees.EgressMinTopupSats, out)
				return
			}
			_, _ = n.store.CreditEgressMilliSats(callerPub, int64(out.PaidSats)*1000, out.PaidSats)
		}
		if !isSpinePeer {
			cred, _ := n.store.GetEgressCredit(callerPub)
			if cred.BalanceMilliSats < msatsPerEvent {
				n.writeFeeOutcome(w, ActionEgressTopup, n.fees.EgressMinTopupSats, feeCollectOutcome{
					HTTPStatus: http.StatusPaymentRequired,
					Code:       "ERR_PAYMENT_REQUIRED",
					Error:      fmt.Sprintf("egress credit balance %d msats below 1-event cost %d msats (top up >= %d sats at /v1/credits/topup)", cred.BalanceMilliSats, msatsPerEvent, n.fees.EgressMinTopupSats),
				})
				return
			}
		}
	}

	// Subscribe BEFORE replaying SQLite so zero events slip through the gap!
	ch, unsub, subOK := n.TrySubscribe(doms...)
	if !subOK {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "hub SSE subscriber limit reached"})
		return
	}
	defer unsub()

	latestSeq := n.store.LatestSeq()
	cursorReset := cursor > latestSeq
	if cursorReset {
		cursor = 0
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Nektar-Latest-Seq", strconv.FormatInt(latestSeq, 10))
	w.WriteHeader(http.StatusOK)
	if cursorReset {
		_, _ = fmt.Fprintf(w, "event: cursor_reset\ndata: {\"latestSeq\":%d}\n\n", latestSeq)
	}
	flusher.Flush()

	emitPaymentRequired := func(rem int64) {
		msg, _ := json.Marshal(map[string]any{
			"code":               "ERR_PAYMENT_REQUIRED",
			"remainingMilliSats": rem,
			"topupPath":          "/v1/credits/topup",
			"minTopupSats":       n.fees.EgressMinTopupSats,
			"satsPer100Events":   n.fees.EgressSatsPer100Events,
			"identityKey":        n.PayeeIdentityKey(),
		})
		_, _ = fmt.Fprintf(w, "event: payment_required\ndata: %s\n\n", msg)
		flusher.Flush()
	}

	var lastSent int64 = cursor
	if cursor >= 0 && (r.URL.Query().Has("cursor") || r.Header.Get("Last-Event-ID") != "") {
		// Drain full backlog in paginated 200-row chunks using raw scanned cursor so domain filtering never truncates replay!
		scanCursor := cursor
		for {
			backlog, nextCursor, rawCount, bErr := n.store.EventsSinceScanned(scanCursor, doms, 200)
			if bErr != nil || rawCount == 0 {
				break
			}
			scanCursor = nextCursor
			for _, st := range backlog {
				if metered && !isSpinePeer && st.Kind != bstack.KindAnnounce {
					rem, ok, _ := n.store.ChargeEgressEvents(callerPub, 1, msatsPerEvent)
					if !ok {
						emitPaymentRequired(rem)
						return
					}
				}
				ev := bstack.MarketEvent{
					Seq:        st.Seq,
					Topic:      st.Topic,
					TxID:       st.TxID,
					Vout:       st.Vout,
					AdmittedAt: st.AdmittedAt,
					Payload:    st.Payload,
					BEEF:       st.BEEF,
				}
				b, _ := json.Marshal(ev)
				_, _ = fmt.Fprintf(w, "id: %d\nevent: market\ndata: %s\n\n", ev.Seq, b)
				if ev.Seq > lastSent {
					lastSent = ev.Seq
				}
			}
			if len(backlog) > 0 {
				flusher.Flush()
			}
			if rawCount < 200 {
				break
			}
		}
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-n.sseStopCh:
			return
		case <-n.stopCh:
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Seq > 0 && ev.Seq <= lastSent {
				continue
			}
			if metered && !isSpinePeer && ev.Payload.Kind != bstack.KindAnnounce {
				rem, ok, _ := n.store.ChargeEgressEvents(callerPub, 1, msatsPerEvent)
				if !ok {
					emitPaymentRequired(rem)
					return
				}
			}
			b, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "id: %d\nevent: market\ndata: %s\n\n", ev.Seq, b)
			if ev.Seq > lastSent {
				lastSent = ev.Seq
			}
			flusher.Flush()
		case <-keepalive.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
