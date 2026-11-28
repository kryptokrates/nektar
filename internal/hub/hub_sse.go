package hub

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
)

func matchesSubFilter(ev bstack.MarketEvent, domains []string, filter map[string]bool) bool {
	if len(filter) == 0 {
		return true
	}
	hasKindFilter := false
	kindMatched := false
	hasDomFilter := false
	domMatched := false
	for k := range filter {
		if strings.HasPrefix(k, "__kind:") {
			hasKindFilter = true
			if string(ev.Payload.Kind) == strings.TrimPrefix(k, "__kind:") {
				kindMatched = true
			}
		} else {
			hasDomFilter = true
		}
	}
	if hasKindFilter && !kindMatched {
		return false
	}
	if hasDomFilter {
		domMatched = matchesAnyDomain(domains, filter)
		if !domMatched {
			return false
		}
	}
	return true
}

func (n *Node) broadcastEvent(ev bstack.MarketEvent, domains []string) {
	n.mu.RLock()
	hasSubs := len(n.subs) > 0
	n.mu.RUnlock()
	if !hasSubs {
		return
	}
	if len(ev.RawJSON) == 0 {
		ev.RawJSON, _ = json.Marshal(ev)
	}
	if len(ev.SlimJSON) == 0 {
		slim := ev
		slim.BEEF = nil
		slim.RawJSON = nil
		ev.SlimJSON, _ = json.Marshal(slim)
	}
	n.mu.RLock()
	// Targeted O(1) delivery for Buyer-addressed OfferAds when subscribers identified their pubkey!
	if ev.Payload.Kind == bstack.KindOffer && ev.BuyerPub != "" {
		bPub := strings.ToLower(strings.TrimSpace(ev.BuyerPub))
		if set := n.subsByPub[bPub]; len(set) > 0 {
			for ch := range set {
				if filter := n.subs[ch]; !matchesSubFilter(ev, domains, filter) {
					continue
				}
				select {
				case ch <- ev:
				default:
				}
			}
		}
		if anonSet := n.subsByPub[""]; len(anonSet) > 0 {
			for ch := range anonSet {
				if filter := n.subs[ch]; !matchesSubFilter(ev, domains, filter) {
					continue
				}
				select {
				case ch <- ev:
				default:
				}
			}
		}
		n.mu.RUnlock()
		return
	}
	for ch, filter := range n.subs {
		if !matchesSubFilter(ev, domains, filter) {
			continue
		}
		select {
		case ch <- ev:
		default:
		}
	}
	n.mu.RUnlock()
}

func (n *Node) broadcastBboxWake(recipientPub string) {
	pub := strings.ToLower(strings.TrimSpace(recipientPub))
	if pub == "" {
		return
	}
	wakeEv := bstack.MarketEvent{Seq: -1, Topic: "bbox:" + pub}
	n.mu.RLock()
	if set := n.subsByPub[pub]; len(set) > 0 {
		for ch := range set {
			select {
			case ch <- wakeEv:
			default:
			}
		}
	}
	if anonSet := n.subsByPub[""]; len(anonSet) > 0 {
		for ch := range anonSet {
			select {
			case ch <- wakeEv:
			default:
			}
		}
	}
	n.mu.RUnlock()
}

// TrySubscribe registers an SSE subscriber if below maxHubSSESubs.
func (n *Node) TrySubscribe(domains ...string) (<-chan bstack.MarketEvent, func(), bool) {
	return n.TrySubscribeForPub("", domains...)
}

// TrySubscribeForPub registers an SSE subscriber indexed by callerPub (for targeted O(1) bbox_wake delivery).
func (n *Node) TrySubscribeForPub(callerPub string, domains ...string) (<-chan bstack.MarketEvent, func(), bool) {
	ch := make(chan bstack.MarketEvent, 2048)
	pubKey := strings.ToLower(strings.TrimSpace(callerPub))
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
	if n.subsByPub == nil {
		n.subsByPub = make(map[string]map[chan bstack.MarketEvent]struct{})
	}
	pset := n.subsByPub[pubKey]
	if pset == nil {
		pset = make(map[chan bstack.MarketEvent]struct{}, 2)
		n.subsByPub[pubKey] = pset
	}
	pset[ch] = struct{}{}
	n.mu.Unlock()
	return ch, func() {
		n.mu.Lock()
		delete(n.subs, ch)
		if ps := n.subsByPub[pubKey]; ps != nil {
			delete(ps, ch)
			if len(ps) == 0 {
				delete(n.subsByPub, pubKey)
			}
		}
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
			if _, cErr := n.store.CreditEgressMilliSats(callerPub, int64(out.PaidSats)*1000, out.PaidSats); cErr != nil {
				log.Printf("hub: CreditEgressMilliSats failed on /sync txid=%s payer=%s: %v", out.TxID, callerPub, cErr)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to credit egress balance: " + cErr.Error()})
				return
			}
		}
		// Require at least 1 event worth of credit upfront so 0-credit free-riders cannot poll /sync endlessly.
		if !isSpinePeer {
			cred, gErr := n.store.GetEgressCredit(callerPub)
			if gErr != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "egress credit lookup failed: " + gErr.Error()})
				return
			}
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
	const syncPageSize = 200
	scanCursor := cursor
	var items []StoredEvent
	totalRaw := 0
	for pages := 0; pages < 25; pages++ {
		chunk, nextCursor, rawCount, err := n.store.EventsSinceScanned(scanCursor, doms, syncPageSize)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		totalRaw += rawCount
		if nextCursor > scanCursor {
			scanCursor = nextCursor
		}
		items = append(items, chunk...)
		if rawCount < syncPageSize || len(doms) == 0 || len(items) >= syncPageSize {
			break
		}
	}
	if len(items) > syncPageSize {
		items = items[:syncPageSize]
		scanCursor = items[len(items)-1].Seq
	}
	w.Header().Set("X-Nektar-Next-Cursor", strconv.FormatInt(scanCursor, 10))
	w.Header().Set("X-Nektar-Raw-Count", strconv.Itoa(totalRaw))
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
			if _, cErr := n.store.CreditEgressMilliSats(callerPub, int64(out.PaidSats)*1000, out.PaidSats); cErr != nil {
				log.Printf("hub: CreditEgressMilliSats failed on /events txid=%s payer=%s: %v", out.TxID, callerPub, cErr)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to credit egress balance: " + cErr.Error()})
				return
			}
		}
		if !isSpinePeer {
			cred, gErr := n.store.GetEgressCredit(callerPub)
			if gErr != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "egress credit lookup failed: " + gErr.Error()})
				return
			}
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
	subPub := strings.ToLower(strings.TrimSpace(callerPub))
	if subPub == "" {
		subPub = strings.ToLower(strings.TrimSpace(r.Header.Get("X-Nektar-Pub")))
	}
	subFilters := append([]string(nil), doms...)
	if rawKinds := strings.TrimSpace(r.URL.Query().Get("kinds")); rawKinds != "" {
		for _, k := range strings.Split(rawKinds, ",") {
			if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
				subFilters = append(subFilters, "__kind:"+k)
			}
		}
	}
	ch, unsub, subOK := n.TrySubscribeForPub(subPub, subFilters...)
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
	slimMode := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("format")), "slim")
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
	emitStreamError := func(code, errMsg string) {
		msg, _ := json.Marshal(map[string]any{
			"code":  code,
			"error": errMsg,
		})
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", msg)
		flusher.Flush()
	}

	var prepaid int64
	if metered && !isSpinePeer {
		defer func() {
			if prepaid > 0 {
				n.store.RefundUnusedEgress(callerPub, prepaid, msatsPerEvent)
			}
		}()
	}
	consumeEgressOne := func() (rem int64, ok bool, err error) {
		if !metered || isSpinePeer {
			return 0, true, nil
		}
		if prepaid > 0 {
			prepaid--
			return prepaid * msatsPerEvent, true, nil
		}
		granted, remBal, rErr := n.store.ReserveEgressBatch(callerPub, 200, msatsPerEvent)
		if rErr != nil {
			return 0, false, rErr
		}
		if granted <= 0 {
			return remBal, false, nil
		}
		prepaid = granted - 1
		return remBal, true, nil
	}

	var lastSent int64 = cursor
	if cursor >= 0 && (r.URL.Query().Has("cursor") || r.Header.Get("Last-Event-ID") != "") {
		// Drain full backlog in paginated 200-row chunks using raw scanned cursor so domain filtering never truncates replay!
		scanCursor := cursor
		for {
			select {
			case <-r.Context().Done():
				return
			case <-n.sseStopCh:
				return
			case <-n.stopCh:
				return
			default:
			}
			backlog, nextCursor, rawCount, bErr := n.store.EventsSinceScanned(scanCursor, doms, 200)
			if bErr != nil || rawCount == 0 {
				break
			}
			scanCursor = nextCursor
			for _, st := range backlog {
				if metered && !isSpinePeer && st.Kind != bstack.KindAnnounce {
					rem, ok, cErr := consumeEgressOne()
					if cErr != nil {
						log.Printf("hub: ReserveEgressBatch failed on /events backlog payer=%s: %v", callerPub, cErr)
						emitStreamError("ERR_HUB_STORE_UNAVAILABLE", cErr.Error())
						return
					}
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

	backlogWatermark := lastSent
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
			if ev.Seq == -1 && strings.HasPrefix(ev.Topic, "bbox:") {
				recPub := strings.TrimPrefix(ev.Topic, "bbox:")
				if subPub == "" || strings.EqualFold(subPub, recPub) {
					wData, _ := json.Marshal(map[string]string{"pub": recPub})
					_, _ = fmt.Fprintf(w, "event: bbox_wake\ndata: %s\n\n", wData)
					flusher.Flush()
				}
				continue
			}
			// Deduplicate strictly against backlogWatermark (NOT live lastSent!) so concurrent
			// 16-lane parallel admissions arriving slightly out of seq order are NEVER dropped!
			if ev.Seq > 0 && ev.Seq <= backlogWatermark {
				continue
			}
			if metered && !isSpinePeer && ev.Payload.Kind != bstack.KindAnnounce {
				rem, ok, cErr := consumeEgressOne()
				if cErr != nil {
					log.Printf("hub: ReserveEgressBatch failed on /events live payer=%s: %v", callerPub, cErr)
					emitStreamError("ERR_HUB_STORE_UNAVAILABLE", cErr.Error())
					return
				}
				if !ok {
					emitPaymentRequired(rem)
					return
				}
			}
			var b []byte
			if slimMode && len(ev.SlimJSON) > 0 {
				b = ev.SlimJSON
			} else if len(ev.RawJSON) > 0 {
				b = ev.RawJSON
			}
			if len(b) == 0 {
				if slimMode {
					slim := ev
					slim.BEEF = nil
					slim.RawJSON = nil
					slim.SlimJSON = nil
					b, _ = json.Marshal(slim)
				} else {
					b, _ = json.Marshal(ev)
				}
			}
			_, _ = fmt.Fprintf(w, "id: %d\nevent: market\ndata: %s\n\n", ev.Seq, b)
			if ev.Seq > lastSent {
				lastSent = ev.Seq
			}
			// Coalesce kernel flushes under burst load while preserving 0ms immediate flush when idle.
			if len(ch) == 0 {
				flusher.Flush()
			}
		case <-keepalive.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
