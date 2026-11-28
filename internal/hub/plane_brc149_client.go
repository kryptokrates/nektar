package hub

import (
	"context"
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
		if n.demandDrivenUpstream {
			// BRC-223 / 1BSV Union Rule: idle Leaf Hubs with 0 downstream listeners park at 0% CPU and 0 egress sats.
			if len(n.ActiveDownstreamTopics()) == 0 {
				select {
				case <-relCtx.Done():
					return
				case <-n.stopCh:
					return
				case <-n.downInterestWake:
					continue
				}
			}
		}
		// Run stream in a child context that cancels immediately when downstream topic union changes!
		connCtx, connCancel := context.WithCancel(relCtx)
		initialCSV := strings.Join(n.desiredUpstreamTopics(), ",")
		reconnectedOnInterest := make(chan struct{})
		if n.demandDrivenUpstream && n.downInterestWake != nil {
			go func() {
				for {
					select {
					case <-connCtx.Done():
						return
					case <-n.downInterestWake:
						// Short 50ms debounce coalesces simultaneous agent topic registrations.
						select {
						case <-connCtx.Done():
							return
						case <-time.After(50 * time.Millisecond):
						}
						if nowCSV := strings.Join(n.desiredUpstreamTopics(), ","); nowCSV != initialCSV {
							close(reconnectedOnInterest)
							connCancel()
							return
						}
					}
				}
			}()
		}
		next := n.consumePlaneOnce(connCtx, spine, cursor)
		connCancel()
		if next > cursor {
			cursor = next
			_ = n.store.SetFedCursor(cursorKey, int64(cursor))
		}
		select {
		case <-reconnectedOnInterest:
			// Immediate seamless reconnect on new downstream topic union (no 2s backoff!).
			continue
		default:
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

func (n *Node) desiredUpstreamTopics() []string {
	if !n.demandDrivenUpstream {
		return n.Topics()
	}
	active := n.ActiveDownstreamTopics()
	if len(active) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(active)+4)
	out := make([]string, 0, len(active)+4)
	add := func(t string) {
		if t = strings.TrimSpace(t); t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, t := range active {
		add(t)
	}
	// Always include core control/handshake topics while any local agent is connected.
	add("tm_finger")
	if off := n.DefaultBboxOffice(); off != "" {
		add("tm_bbox_" + off)
		add(BboxPlaneTopic(off))
	}
	if bkt := n.DefaultBstoreBucket(); bkt != "" {
		add("tm_bstore_" + bkt)
	}
	return out
}

func (n *Node) buildUpstreamTopicMap(elected []string) map[[32]byte]string {
	m := make(map[[32]byte]string, len(elected)+len(n.Topics())+2)
	for _, t := range n.Topics() {
		m[TopicIDOf(t)] = t
	}
	if off := n.DefaultBboxOffice(); off != "" {
		bt := BboxPlaneTopic(off)
		m[TopicIDOf(bt)] = bt
	}
	for _, t := range elected {
		m[TopicIDOf(t)] = t
		// Pre-instantiate dynamic market topic managers (`dom_*` / `ont_*`) on the Leaf Hub.
		_, _ = n.resolveTopicManager(t)
	}
	return m
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
	elected := n.desiredUpstreamTopics()
	if len(elected) == 0 && !n.demandDrivenUpstream {
		elected = n.Topics()
	}
	if len(elected) == 0 {
		return cursor
	}
	topicByID := n.buildUpstreamTopicMap(elected)
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
	persistedCursor := cursor
	unflushed := 0
	lastFlush := time.Now()
	flushCursor := func() {
		if cursor > persistedCursor {
			_ = n.store.SetFedCursor(cursorKey, int64(cursor))
			persistedCursor = cursor
			unflushed = 0
			lastFlush = time.Now()
		}
	}
	defer flushCursor()
	for {
		frame, err := DecodePlaneFrame(resp.Body, MaxPlaneObjectLen)
		if err != nil {
			return cursor
		}
		// Automatic NACK repair on sequence gap! Do NOT advance cursor if gap repair or frame admission fails.
		if cursor > 0 && frame.FlowSeq > cursor+1 {
			if err := n.repairPlaneGap(ctx, spine, cursor+1, frame.FlowSeq-1, elected, topicByID); err != nil {
				return cursor
			}
		}
		if topic, ok := topicByID[frame.TopicID]; ok {
			if !n.loopGuard.Seen(frame.BEEF, topic) {
				var sErr error
				if strings.HasPrefix(topic, "tm_nektar_bbox_") {
					sErr = n.admitRelayedBboxPlaneFrame(frame.BEEF)
				} else {
					_, sErr = n.submitInternal(ctx, frame.BEEF, []string{topic}, true)
				}
				if sErr != nil {
					// ARCH-F5: permanently refused poison frames are counted & skipped so one bad frame
					// cannot stall the entire BRC-149 stream; transient errors hold the cursor for retry.
					if errors.Is(sErr, ErrHubRefused) {
						n.poisonSkipped.Add(1)
						n.loopGuard.MarkIfUnseen(frame.BEEF, topic)
					} else {
						return cursor
					}
				}
			}
		}
		if frame.FlowSeq > cursor {
			cursor = frame.FlowSeq
			unflushed++
			if unflushed >= 100 || time.Since(lastFlush) >= 500*time.Millisecond {
				flushCursor()
			}
		}
	}
}

func (n *Node) repairPlaneGap(ctx context.Context, spine string, fromSeq, toSeq uint64, elected []string, topicByID map[[32]byte]string) error {
	topicsCSV := strings.Join(elected, ",")
	for chunkFrom := fromSeq; chunkFrom <= toSeq; {
		chunkTo := toSeq
		if chunkTo-chunkFrom >= 500 {
			chunkTo = chunkFrom + 499
		}
		u := fmt.Sprintf("%s/plane/nack?from=%d&to=%d", spine, chunkFrom, chunkTo)
		if topicsCSV != "" {
			u += "&topics=" + url.QueryEscape(topicsCSV)
		}
		var resp *http.Response
		for attempt := 0; attempt < 2; attempt++ {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			if err != nil {
				return err
			}
			n.signPlaneEgressReq(req, "hub_plane_nack", spine, int64(chunkFrom), topicsCSV)
			r, err := n.httpClient.Do(req)
			if err != nil {
				return err
			}
			if r.StatusCode == http.StatusPaymentRequired && attempt == 0 {
				minTopup, _ := strconv.ParseUint(strings.TrimSpace(r.Header.Get("X-BSV-Payment-Satoshis-Required")), 10, 64)
				r.Body.Close()
				if n.tryPlaneTopup(ctx, spine, minTopup) {
					continue
				}
				return fmt.Errorf("plane nack %s [%d..%d]: payment required (402)", spine, chunkFrom, chunkTo)
			}
			if r.StatusCode != http.StatusOK {
				r.Body.Close()
				return fmt.Errorf("plane nack %s [%d..%d]: HTTP %d", spine, chunkFrom, chunkTo, r.StatusCode)
			}
			resp = r
			break
		}
		if resp == nil {
			return fmt.Errorf("plane nack %s [%d..%d]: no response", spine, chunkFrom, chunkTo)
		}
		err := func() error {
			defer resp.Body.Close()
			for {
				frame, err := DecodePlaneFrame(resp.Body, MaxPlaneObjectLen)
				if err != nil {
					if errors.Is(err, io.EOF) {
						return nil
					}
					return fmt.Errorf("plane nack %s [%d..%d] decode: %w", spine, chunkFrom, chunkTo, err)
				}
				if topic, ok := topicByID[frame.TopicID]; ok {
					if !n.loopGuard.Seen(frame.BEEF, topic) {
						var sErr error
						if strings.HasPrefix(topic, "tm_nektar_bbox_") {
							sErr = n.admitRelayedBboxPlaneFrame(frame.BEEF)
						} else {
							_, sErr = n.submitInternal(ctx, frame.BEEF, []string{topic}, true)
						}
						if sErr != nil {
							if errors.Is(sErr, ErrHubRefused) {
								n.poisonSkipped.Add(1)
								n.loopGuard.MarkIfUnseen(frame.BEEF, topic)
							} else {
								return fmt.Errorf("plane nack admit seq=%d topic=%s: %w", frame.FlowSeq, topic, sErr)
							}
						}
					}
				}
			}
		}()
		if err != nil {
			return err
		}
		chunkFrom = chunkTo + 1
	}
	return nil
}
