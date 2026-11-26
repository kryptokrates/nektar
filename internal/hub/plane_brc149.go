package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BRC-148 / BRC-149 Application-Layer Object Plane constants (1BSV Paper 006 §4.2 & Paper 010 §2, §8.2).
const (
	BRC148PlaneBase   uint16 = 0x1000
	BRC149HeaderBytes        = 36 // 32B TopicID + 4B uint32 BE ObjectLen
	PlaneFrameHdrBytes       = 44 // 8B uint64 BE FlowSeq + 32B TopicID + 4B uint32 BE ObjectLen
	MaxPlaneObjectLen uint32 = 4 << 20
	maxPlaneSubs             = 256
)

// PlaneFrame is one numbered BRC-149 delivery record on Nektar's Application-Layer Object Plane.
// The BEEF bytes travel verbatim — never parsed, translated, or re-encoded by the relay.
type PlaneFrame struct {
	FlowSeq uint64
	TopicID [32]byte
	Topic   string // optional local label; mapped back from TopicID via subscriber election
	BEEF    []byte
}

// TopicIDOf computes the BRC-148 32-byte TopicID = SHA-256(UTF-8 topic name).
func TopicIDOf(topic string) [32]byte {
	return sha256.Sum256([]byte(topic))
}

// BandOf computes the BRC-148 multicast shard group index for topicID at widthBits (0..15):
//
//	IDX = 0x1000 + ((TopicID[0:4] as uint32 BE) >> (32 - widthBits))
//
// At widthBits == 0, the entire plane occupies 0x1000 (FF0E::B:1000).
func BandOf(topicID [32]byte, widthBits uint8) uint16 {
	if widthBits == 0 {
		return BRC148PlaneBase
	}
	if widthBits > 15 {
		widthBits = 15
	}
	word := binary.BigEndian.Uint32(topicID[0:4])
	shardIdx := uint16(word >> (32 - widthBits))
	return BRC148PlaneBase + shardIdx
}

// EncodeBRC149Record encodes a raw BRC-149 delivery record: [32B TopicID][4B uint32 BE len][BEEF].
func EncodeBRC149Record(topic string, beef []byte) ([]byte, error) {
	if len(beef) == 0 || uint32(len(beef)) > MaxPlaneObjectLen {
		return nil, fmt.Errorf("plane: object size %d out of bounds", len(beef))
	}
	out := make([]byte, BRC149HeaderBytes+len(beef))
	tid := TopicIDOf(topic)
	copy(out[0:32], tid[:])
	binary.BigEndian.PutUint32(out[32:36], uint32(len(beef)))
	copy(out[36:], beef)
	return out, nil
}

// DecodeBRC149Record reads one length-delimited BRC-149 delivery record with strict pre-allocation bounds.
func DecodeBRC149Record(r io.Reader, maxLen uint32) ([32]byte, []byte, error) {
	var tid [32]byte
	if maxLen == 0 || maxLen > MaxPlaneObjectLen {
		maxLen = MaxPlaneObjectLen
	}
	var hdr [BRC149HeaderBytes]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return tid, nil, err
	}
	copy(tid[:], hdr[0:32])
	objLen := binary.BigEndian.Uint32(hdr[32:36])
	if objLen == 0 || objLen > maxLen {
		return tid, nil, fmt.Errorf("plane: BRC-149 object length %d out of bounds (max %d)", objLen, maxLen)
	}
	beef := make([]byte, objLen)
	if _, err := io.ReadFull(r, beef); err != nil {
		return tid, nil, err
	}
	return tid, beef, nil
}

// EncodePlaneFrame encodes a numbered BRC-149 relay frame: [8B FlowSeq][32B TopicID][4B len][BEEF].
func EncodePlaneFrame(f PlaneFrame) ([]byte, error) {
	if len(f.BEEF) == 0 || uint32(len(f.BEEF)) > MaxPlaneObjectLen {
		return nil, fmt.Errorf("plane: object size %d out of bounds", len(f.BEEF))
	}
	tid := f.TopicID
	if tid == ([32]byte{}) && f.Topic != "" {
		tid = TopicIDOf(f.Topic)
	}
	out := make([]byte, PlaneFrameHdrBytes+len(f.BEEF))
	binary.BigEndian.PutUint64(out[0:8], f.FlowSeq)
	copy(out[8:40], tid[:])
	binary.BigEndian.PutUint32(out[40:44], uint32(len(f.BEEF)))
	copy(out[44:], f.BEEF)
	return out, nil
}

// DecodePlaneFrame decodes one numbered BRC-149 relay frame with strict bounds checking before allocating.
func DecodePlaneFrame(r io.Reader, maxLen uint32) (PlaneFrame, error) {
	if maxLen == 0 || maxLen > MaxPlaneObjectLen {
		maxLen = MaxPlaneObjectLen
	}
	var hdr [PlaneFrameHdrBytes]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return PlaneFrame{}, err
	}
	seq := binary.BigEndian.Uint64(hdr[0:8])
	var tid [32]byte
	copy(tid[:], hdr[8:40])
	objLen := binary.BigEndian.Uint32(hdr[40:44])
	if objLen == 0 || objLen > maxLen {
		return PlaneFrame{}, fmt.Errorf("plane: frame length %d out of bounds (max %d)", objLen, maxLen)
	}
	beef := make([]byte, objLen)
	if _, err := io.ReadFull(r, beef); err != nil {
		return PlaneFrame{}, err
	}
	return PlaneFrame{FlowSeq: seq, TopicID: tid, BEEF: beef}, nil
}

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
	// Suppress upstream re-forwarding on Spine relay publish; emit locally once via broadcastPlaneFrame.
	steak, err := n.submitInternal(r.Context(), beef, topics, false)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
			cred, _ := n.store.GetEgressCredit(callerPub)
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

	chargeOne := func() bool {
		if !metered || isSpinePeer {
			return true
		}
		_, ok, cErr := n.store.ChargeEgressEvents(callerPub, 1, msatsPerEvent)
		return cErr == nil && ok
	}

	sentSeq := uint64(0)
	if fromSeq > latest {
		fromSeq = 0
	}
	if fromSeq < latest {
		backlog, _ := n.store.EventsInRange(fromSeq+1, latest, topicFilter, 500)
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
func (n *Node) handlePlaneNACK(w http.ResponseWriter, r *http.Request) {
	fromSeq, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	toSeq, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
	if fromSeq <= 0 || toSeq < fromSeq {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "valid from and to sequence numbers required"})
		return
	}
	if toSeq-fromSeq > 500 {
		toSeq = fromSeq + 500
	}
	var topics []string
	if rawT := strings.TrimSpace(r.URL.Query().Get("topics")); rawT != "" {
		for _, t := range strings.Split(rawT, ",") {
			if t = strings.TrimSpace(t); t != "" {
				topics = append(topics, t)
			}
		}
	}
	events, err := n.store.EventsInRange(fromSeq, toSeq, topics, 500)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/vnd.nektar.brc149-stream")
	w.WriteHeader(http.StatusOK)
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
		_, _ = w.Write(b)
	}
}

// StartPlaneRelay connects a Leaf Hub to a Spine's Application-Layer BRC-149 Object Plane stream
// (Paper 010 §8.2), mapping received TopicIDs back to local elected topic names, automatically
// issuing GET /plane/nack on any sequence gap, and admitting via LoopGuard with propagation OFF.
func (n *Node) StartPlaneRelay(ctx context.Context, spineURLs []string) {
	for _, raw := range spineURLs {
		spine := strings.TrimRight(strings.TrimSpace(raw), "/")
		if spine == "" {
			continue
		}
		key := "plane:" + spine
		n.mu.Lock()
		if n.fedStarted[key] {
			n.mu.Unlock()
			continue
		}
		n.fedStarted[key] = true
		n.mu.Unlock()
		n.wg.Add(1)
		go func(sp string) {
			defer n.wg.Done()
			n.planeRelayLoop(ctx, sp)
		}(spine)
	}
}

func (n *Node) planeRelayLoop(ctx context.Context, spine string) {
	relCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-n.stopCh:
			cancel()
		case <-relCtx.Done():
		}
	}()
	cursorKey := "plane:" + spine
	cursor := uint64(n.store.GetFedCursor(cursorKey))
	for {
		if relCtx.Err() != nil {
			return
		}
		next := n.consumePlaneOnce(relCtx, spine, cursor)
		if next > cursor {
			cursor = next
			_ = n.store.SetFedCursor(cursorKey, int64(cursor))
		}
		backoff := time.Duration(float64(2*time.Second) * (0.5 + rand.Float64()))
		select {
		case <-relCtx.Done():
			return
		case <-n.stopCh:
			return
		case <-time.After(backoff):
		}
	}
}

func (n *Node) signPlaneEgressReq(req *http.Request, action, spineURL string, cursor int64, topicsCSV string) {
	n.mu.RLock()
	signer := n.egressAuthSigner
	n.mu.RUnlock()
	if signer == nil {
		return
	}
	aud := planeHostAud(spineURL)
	if pub, ts, nonce, sig, err := signer(action, aud, cursor, topicsCSV); err == nil && pub != "" && sig != "" {
		req.Header.Set("X-Nektar-Pub", pub)
		req.Header.Set("X-Nektar-Ts", ts)
		req.Header.Set("X-Nektar-Nonce", nonce)
		req.Header.Set("X-Nektar-Sig", sig)
	}
}

func planeHostAud(rawURL string) string {
	if u, err := url.Parse(strings.TrimSpace(rawURL)); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return strings.ToLower(strings.TrimSpace(rawURL))
}

func (n *Node) tryPlaneTopup(ctx context.Context, spine string, minTopup uint64) bool {
	n.mu.RLock()
	topupFn := n.onEgressTopup
	n.mu.RUnlock()
	if topupFn == nil {
		return false
	}
	tCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return topupFn(tCtx, spine, minTopup) == nil
}

func (n *Node) consumePlaneOnce(ctx context.Context, spine string, cursor uint64) uint64 {
	elected := n.Topics()
	topicByID := make(map[[32]byte]string, len(elected))
	for _, t := range elected {
		topicByID[TopicIDOf(t)] = t
	}
	topicsCSV := strings.Join(elected, ",")
	u := fmt.Sprintf("%s/plane/stream?from=%d", spine, cursor)
	if topicsCSV != "" {
		u += "&topics=" + url.QueryEscape(topicsCSV)
	}
	var resp *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return cursor
		}
		n.signPlaneEgressReq(req, "hub_plane", spine, int64(cursor), topicsCSV)
		r, err := n.sseClient.Do(req)
		if err != nil {
			return cursor
		}
		if r.StatusCode == http.StatusPaymentRequired && attempt == 0 {
			minTopup, _ := strconv.ParseUint(strings.TrimSpace(r.Header.Get("X-BSV-Payment-Satoshis-Required")), 10, 64)
			r.Body.Close()
			if n.tryPlaneTopup(ctx, spine, minTopup) {
				continue
			}
			return cursor
		}
		if r.StatusCode != http.StatusOK {
			r.Body.Close()
			return cursor
		}
		resp = r
		break
	}
	if resp == nil {
		return cursor
	}
	defer resp.Body.Close()
	if lsStr := strings.TrimSpace(resp.Header.Get("X-Nektar-Latest-Seq")); lsStr != "" {
		if latest, pErr := strconv.ParseUint(lsStr, 10, 64); pErr == nil && latest < cursor {
			cursor = 0
		}
	}
	cursorKey := "plane:" + spine
	for {
		frame, err := DecodePlaneFrame(resp.Body, MaxPlaneObjectLen)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return cursor
			}
			return cursor
		}
		// Automatic NACK repair on sequence gap!
		if cursor > 0 && frame.FlowSeq > cursor+1 {
			n.repairPlaneGap(ctx, spine, cursor+1, frame.FlowSeq-1, elected, topicByID)
		}
		if topic, ok := topicByID[frame.TopicID]; ok {
			if n.loopGuard.MarkIfUnseen(frame.BEEF, topic) {
				_, _ = n.submitInternal(ctx, frame.BEEF, []string{topic}, true)
			}
		}
		if frame.FlowSeq > cursor {
			cursor = frame.FlowSeq
			_ = n.store.SetFedCursor(cursorKey, int64(cursor))
		}
	}
}

func (n *Node) repairPlaneGap(ctx context.Context, spine string, fromSeq, toSeq uint64, elected []string, topicByID map[[32]byte]string) {
	u := fmt.Sprintf("%s/plane/nack?from=%d&to=%d", spine, fromSeq, toSeq)
	if len(elected) > 0 {
		u += "&topics=" + url.QueryEscape(strings.Join(elected, ","))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return
	}
	resp, err := n.httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	for {
		frame, err := DecodePlaneFrame(resp.Body, MaxPlaneObjectLen)
		if err != nil {
			return
		}
		if topic, ok := topicByID[frame.TopicID]; ok {
			if n.loopGuard.MarkIfUnseen(frame.BEEF, topic) {
				_, _ = n.submitInternal(ctx, frame.BEEF, []string{topic}, true)
			}
		}
	}
}
