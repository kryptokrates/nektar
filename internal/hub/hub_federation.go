package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
)

// StartFederation subscribes to explicit bilateral peer hubs' /events streams with persisted cursors.
func (n *Node) StartFederation(ctx context.Context, peerHubURLs []string) {
	n.StartFederationWithTier(ctx, peerHubURLs, true)
}

// StartFederationWithTier starts deduplicated peer federation; when manual==false (auto-discovered
// dir peer) on a RoleSpine Hub, only free `dir` announcements are synced (closing the Sybil
// self-hub free-rider attack: Leaf hubs MUST push ads UP via POST /submit instead).
func (n *Node) StartFederationWithTier(ctx context.Context, peerHubURLs []string, manual bool) {
	for _, u := range peerHubURLs {
		peer := strings.TrimRight(strings.TrimSpace(u), "/")
		if peer == "" {
			continue
		}
		n.mu.Lock()
		if n.fedStarted[peer] {
			n.mu.Unlock()
			continue
		}
		n.fedStarted[peer] = true
		n.mu.Unlock()
		n.wg.Add(1)
		go func(p string, isManual bool) {
			defer n.wg.Done()
			n.federatePeerLoop(ctx, p, isManual)
		}(peer, manual)
	}
}

func (n *Node) federatePeerLoop(ctx context.Context, peer string, manual bool) {
	fedCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-n.stopCh:
			cancel()
		case <-fedCtx.Done():
		}
	}()
	cursor := n.store.GetFedCursor(peer)
	for {
		if fedCtx.Err() != nil {
			return
		}
		next := n.pullPeerOnce(fedCtx, peer, cursor, manual)
		if next > cursor {
			cursor = next
			_ = n.store.SetFedCursor(peer, cursor)
		}
		backoff := time.Duration(float64(3*time.Second) * (0.5 + rand.Float64()))
		select {
		case <-fedCtx.Done():
			return
		case <-n.stopCh:
			return
		case <-time.After(backoff):
		}
	}
}

func (n *Node) peerDomainsCSV(manual bool) string {
	// Spine anti-freerider guard: never blindly pull unpaid want/offer ads from untrusted auto-discovered hubs.
	if n.fees.Role == RoleSpine && !manual {
		return bstack.DirDomain
	}
	// Leaf downstream-signals-interest-up: request only interested domains (+ dir) from upstream Spine.
	if n.fees.Role == RoleLeaf {
		n.mu.RLock()
		domFn := n.subscribedDomains
		n.mu.RUnlock()
		if domFn != nil {
			if doms := domFn(); len(doms) > 0 {
				seen := map[string]bool{bstack.DirDomain: true}
				out := []string{bstack.DirDomain}
				for _, d := range doms {
					if d = strings.ToLower(strings.TrimSpace(d)); d != "" && !seen[d] {
						seen[d] = true
						out = append(out, d)
					}
				}
				return strings.Join(out, ",")
			}
		}
	}
	return ""
}

func (n *Node) signPeerEgressReq(req *http.Request, action, peerURL string, cursor int64, domainsCSV string) {
	n.mu.RLock()
	signer := n.egressAuthSigner
	n.mu.RUnlock()
	if signer == nil {
		return
	}
	aud := peerHostAud(peerURL)
	if pub, ts, nonce, sig, err := signer(action, aud, cursor, domainsCSV); err == nil && pub != "" && sig != "" {
		req.Header.Set("X-Nektar-Pub", pub)
		req.Header.Set("X-Nektar-Ts", ts)
		req.Header.Set("X-Nektar-Nonce", nonce)
		req.Header.Set("X-Nektar-Sig", sig)
	}
}

func peerHostAud(rawURL string) string {
	if u, err := url.Parse(strings.TrimSpace(rawURL)); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return strings.ToLower(strings.TrimSpace(rawURL))
}

func (n *Node) tryPeerTopup(ctx context.Context, peer string, minTopup uint64) bool {
	n.mu.RLock()
	topupFn := n.onEgressTopup
	n.mu.RUnlock()
	if topupFn == nil {
		return false
	}
	tCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return topupFn(tCtx, peer, minTopup) == nil
}

func (n *Node) pullPeerOnce(ctx context.Context, peer string, cursor int64, manual bool) int64 {
	domainsCSV := n.peerDomainsCSV(manual)
	// Drain missed raw BEEFs via /sync?cursor= in paginated batches until caught up.
	cursor = n.syncRange(ctx, peer, cursor, domainsCSV)
	if cursor > 0 {
		_ = n.store.SetFedCursor(peer, cursor)
	}

	// Hold SSE /events?cursor= until EOF or error, syncing inline without tearing down the stream!
	sseURL := fmt.Sprintf("%s/events?cursor=%d", peer, cursor)
	if domainsCSV != "" {
		sseURL += "&domains=" + url.QueryEscape(domainsCSV)
	}
	var sseResp *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		sseReq, err := http.NewRequestWithContext(ctx, http.MethodGet, sseURL, nil)
		if err != nil {
			return cursor
		}
		n.signPeerEgressReq(sseReq, "hub_events", peer, cursor, domainsCSV)
		resp, err := n.sseClient.Do(sseReq)
		if err != nil {
			return cursor
		}
		if resp.StatusCode == http.StatusPaymentRequired && attempt == 0 {
			minTopup, _ := strconv.ParseUint(strings.TrimSpace(resp.Header.Get("X-BSV-Payment-Satoshis-Required")), 10, 64)
			resp.Body.Close()
			if n.tryPeerTopup(ctx, peer, minTopup) {
				continue
			}
			return cursor
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return cursor
		}
		sseResp = resp
		break
	}
	if sseResp == nil {
		return cursor
	}
	defer sseResp.Body.Close()
	if lsStr := strings.TrimSpace(sseResp.Header.Get("X-Nektar-Latest-Seq")); lsStr != "" {
		if latest, pErr := strconv.ParseInt(lsStr, 10, 64); pErr == nil && latest < cursor {
			cursor = 0
			_ = n.store.SetFedCursor(peer, 0)
		}
	}
	sc := bufio.NewScanner(sseResp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var pendingID int64
	var pendingEvent string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			pendingEvent = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			continue
		}
		if strings.HasPrefix(line, "id: ") {
			if id, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "id: ")), 10, 64); err == nil {
				pendingID = id
			}
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			raw := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			if pendingEvent == "cursor_reset" {
				pendingEvent = ""
				cursor = 0
				_ = n.store.SetFedCursor(peer, 0)
				continue
			}
			if pendingEvent == "payment_required" {
				pendingEvent = ""
				var pr struct {
					MinTopupSats uint64 `json:"minTopupSats"`
				}
				_ = json.Unmarshal([]byte(raw), &pr)
				_ = n.tryPeerTopup(ctx, peer, pr.MinTopupSats)
				return cursor
			}
			pendingEvent = ""
			var ev bstack.MarketEvent
			if err := json.Unmarshal([]byte(raw), &ev); err == nil {
				seq := ev.Seq
				if seq == 0 {
					seq = pendingID
				}
				if seq <= cursor {
					continue
				}
				// Spine anti-freerider guard: refuse foreign want/offer if non-manual peer ignored ?domains=dir.
				if n.fees.Role == RoleSpine && !manual && ev.Payload.Kind != bstack.KindAnnounce {
					cursor = seq
					continue
				}
				// Fast path: admit carried BEEF inline when contiguous with our cursor — zero /sync refetch!
				if len(ev.BEEF) > 0 && ev.Topic != "" && (cursor == 0 || seq == cursor+1) {
					if _, sErr := n.submitInternal(ctx, ev.BEEF, []string{ev.Topic}, true); sErr != nil {
						lower := strings.ToLower(sErr.Error())
						if strings.Contains(lower, "rate limit") || strings.Contains(lower, "busy") || strings.Contains(lower, "locked") || strings.Contains(lower, "tracker unavailable") {
							continue
						}
					}
					cursor = seq
					_ = n.store.SetFedCursor(peer, cursor)
					continue
				}
			}
			// Fallback on cursor gap or legacy SSE peer omitting BEEF: repair gap via /sync.
			if pendingID > cursor {
				next := n.syncRange(ctx, peer, cursor, domainsCSV)
				if next > cursor {
					cursor = next
					_ = n.store.SetFedCursor(peer, cursor)
				}
			}
		}
	}
	return cursor
}

func (n *Node) syncRange(ctx context.Context, peer string, cursor int64, domainsCSV string) int64 {
	for {
		syncURL := fmt.Sprintf("%s/sync?cursor=%d", peer, cursor)
		if domainsCSV != "" {
			syncURL += "&domains=" + url.QueryEscape(domainsCSV)
		}
		var resp *http.Response
		for attempt := 0; attempt < 2; attempt++ {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, syncURL, nil)
			if err != nil {
				return cursor
			}
			n.signPeerEgressReq(req, "hub_sync", peer, cursor, domainsCSV)
			r, err := n.httpClient.Do(req)
			if err != nil {
				return cursor
			}
			if r.StatusCode == http.StatusPaymentRequired && attempt == 0 {
				minTopup, _ := strconv.ParseUint(strings.TrimSpace(r.Header.Get("X-BSV-Payment-Satoshis-Required")), 10, 64)
				r.Body.Close()
				if n.tryPeerTopup(ctx, peer, minTopup) {
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
		if lsStr := strings.TrimSpace(resp.Header.Get("X-Nektar-Latest-Seq")); lsStr != "" {
			if latest, pErr := strconv.ParseInt(lsStr, 10, 64); pErr == nil && latest < cursor {
				cursor = 0
				_ = n.store.SetFedCursor(peer, 0)
			}
		}
		var items []StoredEvent
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&items)
		resp.Body.Close()
		if len(items) == 0 {
			return cursor
		}
		advanced := false
		for _, it := range items {
			if n.fees.Role == RoleSpine && domainsCSV == bstack.DirDomain && it.Kind != bstack.KindAnnounce {
				if it.Seq > cursor {
					cursor = it.Seq
					advanced = true
				}
				continue
			}
			if len(it.BEEF) > 0 && it.Topic != "" {
				if _, sErr := n.submitInternal(ctx, it.BEEF, []string{it.Topic}, true); sErr != nil {
					// Stop advancing federation cursor on transient storage/rate/tracker errors so failed events retry cleanly.
					lower := strings.ToLower(sErr.Error())
					if strings.Contains(lower, "rate limit") || strings.Contains(lower, "busy") || strings.Contains(lower, "locked") || strings.Contains(lower, "tracker unavailable") {
						return cursor
					}
				}
			}
			if it.Seq > cursor {
				cursor = it.Seq
				advanced = true
			}
		}
		if len(items) < 200 || !advanced {
			return cursor
		}
	}
}
