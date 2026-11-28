package hub

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

func (n *Node) broadcastPlaneFrame(f PlaneFrame) {
	if len(f.BEEF) == 0 {
		return
	}
	n.mu.RLock()
	for ch, filter := range n.planeSubs {
		if len(filter) > 0 && !filter[f.TopicID] {
			continue
		}
		select {
		case ch <- f:
		default:
			// Slow consumer detects gap via FlowSeq and repairs cleanly via GET /plane/nack.
		}
	}
	n.mu.RUnlock()
}

// handlePlanePublish accepts a single BRC-149 publication ([32B TopicID][4B len][BEEF]) or
// X-Topics + raw BEEF, admits locally via the Hub engine (returning real STEAK), and emits
// ONCE onto the Spine's numbered BRC-149 stream to all subscribed Leaf Hubs.
func (n *Node) handlePlanePublish(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, int64(MaxPlaneObjectLen)+BRC149HeaderBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var topics []string
	var beef []byte
	if r.Header.Get("Content-Type") == "application/vnd.nektar.brc149" {
		tid, obj, dErr := DecodeBRC149Record(bytes.NewReader(raw), MaxPlaneObjectLen)
		if dErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": dErr.Error()})
			return
		}
		topicName := n.topicNameForID(tid)
		if topicName == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "plane: unknown elected TopicID"})
			return
		}
		topics = []string{topicName}
		beef = obj
	} else {
		beef = raw
		if hdr := r.Header.Get("X-Topics"); hdr != "" {
			for _, t := range strings.Split(strings.Trim(hdr, "[]\" "), ",") {
				t = strings.Trim(strings.TrimSpace(t), `"`)
				if t != "" {
					topics = append(topics, t)
				}
			}
		}
		if len(topics) == 0 {
			topics = n.Topics()
		}
	}
	// Valve 1: enforce shared submission fee gate (bilateral Spine peers exempt).
	collectedTxID, ok := n.enforceSubmitFee(w, r, "hub_plane_publish", beef, topics)
	if !ok {
		return
	}
	// Suppress upstream re-forwarding on Spine relay publish; emit locally once via broadcastPlaneFrame.
	steak, err := n.submitInternal(r.Context(), beef, topics, false)
	if err != nil {
		if collectedTxID != "" {
			_ = n.store.ReleaseFeeClaim(collectedTxID)
		}
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "rate limit") {
			code = http.StatusTooManyRequests
		} else if errors.Is(err, ErrHubTransient) || strings.Contains(err.Error(), "tracker unavailable") {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, steak)
}

func (n *Node) topicNameForID(tid [32]byte) string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	for name := range n.topics {
		if TopicIDOf(name) == tid {
			return name
		}
	}
	return ""
}

// handlePlaneStream serves the numbered Application-Layer BRC-149 Object Plane stream
// (GET /plane/stream?from=<seq>&topics=<csv>). Backfills missed frames > from, then pushes live.
// When the Hub meters egress (EgressSatsPer100Events > 0), callers must authenticate via signed
// X-Nektar-* headers and maintain positive egress credits (bilateral Spine peers are exempt).
func (n *Node) handlePlaneStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	fromSeq, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	rawT := strings.TrimSpace(r.URL.Query().Get("topics"))
	var topicFilter []string
	tidFilter := make(map[[32]byte]bool)
	if rawT != "" {
		for _, t := range strings.Split(rawT, ",") {
			if t = strings.TrimSpace(t); t != "" {
				topicFilter = append(topicFilter, t)
				tidFilter[TopicIDOf(t)] = true
			}
		}
	}

	msatsPerEvent := n.fees.EgressMilliSatsPerEvent()
	metered := msatsPerEvent > 0
	var callerPub string
	var isSpinePeer bool
	if metered {
		var authErr error
		callerPub, isSpinePeer, authErr = n.authenticateEgressCaller(r, "hub_plane", fromSeq, rawT)
		if authErr != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": authErr.Error()})
			return
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
					Error:      fmt.Sprintf("egress credit balance %d msats below 1-frame cost %d msats (top up >= %d sats at /v1/credits/topup)", cred.BalanceMilliSats, msatsPerEvent, n.fees.EgressMinTopupSats),
				})
				return
			}
		}
	}

	ch := make(chan PlaneFrame, 128)
	n.mu.Lock()
	if len(n.planeSubs) >= maxPlaneSubs {
		n.mu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "too many active plane subscribers"})
		return
	}
	n.planeSubs[ch] = tidFilter
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		delete(n.planeSubs, ch)
		n.mu.Unlock()
	}()

	latest := n.store.LatestSeq()
	w.Header().Set("Content-Type", "application/vnd.nektar.brc149-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Nektar-Latest-Seq", strconv.FormatInt(latest, 10))
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var prepaid int64
	if metered && !isSpinePeer {
		defer func() {
			if prepaid > 0 {
				n.store.RefundUnusedEgress(callerPub, prepaid, msatsPerEvent)
			}
		}()
	}
	chargeOne := func() bool {
		if !metered || isSpinePeer {
			return true
		}
		if prepaid > 0 {
			prepaid--
			return true
		}
		granted, _, cErr := n.store.ReserveEgressBatch(callerPub, 20, msatsPerEvent)
		if cErr != nil || granted <= 0 {
			return false
		}
		prepaid = granted - 1
		return true
	}

	sentSeq := uint64(0)
	if fromSeq > latest {
		fromSeq = 0
	}
	if fromSeq < latest {
		for curFrom := fromSeq + 1; curFrom <= latest; {
			backlog, nextSeq, rawCount, bErr := n.store.EventsInRangeScanned(curFrom, latest, topicFilter, 500)
			if bErr != nil || rawCount == 0 {
				break
			}
			for _, ev := range backlog {
				if len(ev.BEEF) == 0 {
					continue
				}
				if !chargeOne() {
					flusher.Flush()
					return
				}
				frameBytes, err := EncodePlaneFrame(PlaneFrame{
					FlowSeq: uint64(ev.Seq),
					TopicID: TopicIDOf(ev.Topic),
					Topic:   ev.Topic,
					BEEF:    ev.BEEF,
				})
				if err != nil {
					continue
				}
				if _, err := w.Write(frameBytes); err != nil {
					return
				}
				sentSeq = uint64(ev.Seq)
			}
			flusher.Flush()
			if nextSeq < curFrom || rawCount < 500 {
				break
			}
			curFrom = nextSeq + 1
		}
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.sseStopCh:
			return
		case <-n.stopCh:
			return
		case f := <-ch:
			if f.FlowSeq <= sentSeq {
				continue
			}
			if !chargeOne() {
				flusher.Flush()
				return
			}
			frameBytes, err := EncodePlaneFrame(f)
			if err != nil {
				continue
			}
			if _, err := w.Write(frameBytes); err != nil {
				return
			}
			sentSeq = f.FlowSeq
			flusher.Flush()
		}
	}
}

// handlePlaneNACK serves exact missing numbered BRC-149 frames in [from, to] for lossless gap repair
// (GET /plane/nack?from=<seq>&to=<seq>&topics=<csv>).
// When egress metering is active, callers must authenticate via signed X-Nektar-* headers and pay Valve 2
// egress credits per delivered frame (bilateral Spine peers are exempt).
func (n *Node) handlePlaneNACK(w http.ResponseWriter, r *http.Request) {
	fromSeq, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	toSeq, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
	if fromSeq <= 0 || toSeq < fromSeq {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "valid from and to sequence numbers required"})
		return
	}
	if toSeq-fromSeq >= 500 {
		toSeq = fromSeq + 499
	}
	rawT := strings.TrimSpace(r.URL.Query().Get("topics"))
	var topics []string
	if rawT != "" {
		for _, t := range strings.Split(rawT, ",") {
			if t = strings.TrimSpace(t); t != "" {
				topics = append(topics, t)
			}
		}
	}
	msatsPerEvent := n.fees.EgressMilliSatsPerEvent()
	metered := msatsPerEvent > 0
	var callerPub string
	var isSpinePeer bool
	if metered {
		var authErr error
		callerPub, isSpinePeer, authErr = n.authenticateEgressCaller(r, "hub_plane_nack", fromSeq, rawT)
		if authErr != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": authErr.Error()})
			return
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
					Error:      fmt.Sprintf("egress credit balance %d msats below 1-frame cost %d msats (top up >= %d sats at /v1/credits/topup)", cred.BalanceMilliSats, msatsPerEvent, n.fees.EgressMinTopupSats),
				})
				return
			}
		}
	}
	events, err := n.store.EventsInRange(fromSeq, toSeq, topics, 500)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	frames := make([][]byte, 0, len(events))
	for _, ev := range events {
		if len(ev.BEEF) == 0 {
			continue
		}
		b, err := EncodePlaneFrame(PlaneFrame{
			FlowSeq: uint64(ev.Seq),
			TopicID: TopicIDOf(ev.Topic),
			Topic:   ev.Topic,
			BEEF:    ev.BEEF,
		})
		if err != nil {
			continue
		}
		frames = append(frames, b)
	}
	if metered && !isSpinePeer && len(frames) > 0 {
		if _, ok, cErr := n.store.ChargeEgressEvents(callerPub, int64(len(frames)), msatsPerEvent); cErr != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "egress charge failed: " + cErr.Error()})
			return
		} else if !ok {
			n.writeFeeOutcome(w, ActionEgressTopup, n.fees.EgressMinTopupSats, feeCollectOutcome{
				HTTPStatus: http.StatusPaymentRequired,
				Code:       "ERR_PAYMENT_REQUIRED",
				Error:      fmt.Sprintf("insufficient egress credits for %d nack frames (top up >= %d sats at /v1/credits/topup)", len(frames), n.fees.EgressMinTopupSats),
			})
			return
		}
	}
	w.Header().Set("Content-Type", "application/vnd.nektar.brc149-stream")
	w.WriteHeader(http.StatusOK)
	for _, b := range frames {
		if _, wErr := w.Write(b); wErr != nil {
			return
		}
	}
}
