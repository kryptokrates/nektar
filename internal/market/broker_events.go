package market

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
)

func (b *Broker) ensureHostLoops(ctx context.Context) {
	active := b.disc.ActiveHosts()
	activeSet := make(map[string]bool, len(active))
	for _, h := range active {
		activeSet[h] = true
	}
	b.mu.Lock()
	// Cancel SSE loops for hosts that were removed or blocked!
	for h, cancel := range b.activeLoops {
		if !activeSet[h] {
			cancel()
			delete(b.activeLoops, h)
		}
	}
	var toStart []struct {
		url string
		ctx context.Context
	}
	for _, h := range active {
		if _, exists := b.activeLoops[h]; !exists {
			hCtx, cancel := context.WithCancel(ctx)
			b.activeLoops[h] = cancel
			toStart = append(toStart, struct {
				url string
				ctx context.Context
			}{url: h, ctx: hCtx})
		}
	}
	b.mu.Unlock()
	for _, s := range toStart {
		b.wg.Add(1)
		go func(hCtx context.Context, hURL string) {
			defer b.wg.Done()
			b.sseHostLoop(hCtx, hURL)
		}(s.ctx, s.url)
	}
}

func (b *Broker) loadCursor(hubURL string) int64 {
	b.cursorMu.Lock()
	defer b.cursorMu.Unlock()
	if b.hubCursors == nil {
		b.hubCursors = make(map[string]int64)
	}
	if v, ok := b.hubCursors[hubURL]; ok {
		return v
	}
	var v int64
	if b.mem != nil && b.mem.DB() != nil {
		ok, _ := b.mem.DB().GetJSON("hub_cursor:"+hubURL, &v)
		if !ok {
			v = 0
		}
	}
	b.hubCursors[hubURL] = v
	return v
}

func (b *Broker) saveCursor(hubURL string, seq int64) {
	if seq <= 0 {
		return
	}
	var flushBatch map[string]int64
	b.cursorMu.Lock()
	if b.hubCursors == nil {
		b.hubCursors = make(map[string]int64)
	}
	if b.dirtyCursors == nil {
		b.dirtyCursors = make(map[string]int64)
	}
	cur, loaded := b.hubCursors[hubURL]
	if !loaded && b.mem != nil && b.mem.DB() != nil {
		_, _ = b.mem.DB().GetJSON("hub_cursor:"+hubURL, &cur)
		b.hubCursors[hubURL] = cur
	}
	if seq > cur {
		b.hubCursors[hubURL] = seq
		b.dirtyCursors[hubURL] = seq
		b.cursorDirtyN++
		if b.cursorDirtyN >= 250 {
			flushBatch = make(map[string]int64, len(b.dirtyCursors))
			for k, v := range b.dirtyCursors {
				flushBatch[k] = v
			}
			clear(b.dirtyCursors)
			b.cursorDirtyN = 0
		}
	}
	b.cursorMu.Unlock()
	if len(flushBatch) > 0 && b.mem != nil && b.mem.DB() != nil {
		for k, v := range flushBatch {
			_ = b.mem.DB().SetJSON("hub_cursor:"+k, v)
		}
	}
}

func (b *Broker) flushDirtyCursors() {
	if b == nil || b.mem == nil || b.mem.DB() == nil {
		return
	}
	b.cursorMu.Lock()
	if len(b.dirtyCursors) == 0 {
		b.cursorMu.Unlock()
		return
	}
	batch := make(map[string]int64, len(b.dirtyCursors))
	for k, v := range b.dirtyCursors {
		batch[k] = v
	}
	clear(b.dirtyCursors)
	b.cursorDirtyN = 0
	b.cursorMu.Unlock()
	for k, v := range batch {
		_ = b.mem.DB().SetJSON("hub_cursor:"+k, v)
	}
}

func (b *Broker) resetCursor(hubURL string, seq int64) {
	b.cursorMu.Lock()
	if b.hubCursors == nil {
		b.hubCursors = make(map[string]int64)
	}
	b.hubCursors[hubURL] = seq
	if b.dirtyCursors != nil {
		delete(b.dirtyCursors, hubURL)
	}
	b.cursorMu.Unlock()
	if b.mem != nil && b.mem.DB() != nil {
		_ = b.mem.DB().SetJSON("hub_cursor:"+hubURL, seq)
	}
}

func (b *Broker) attachHubEgressAuth(req *http.Request, action, hubURL string, cursor int64, domainsCSV string) {
	if b == nil || b.engine == nil || b.engine.signer == nil {
		return
	}
	if pub, ts, nonce, sig, err := discovery.SignEgressAuthHeaders(b.engine.signer, action, hubURL, cursor, domainsCSV); err == nil {
		req.Header.Set("X-Nektar-Pub", pub)
		req.Header.Set("X-Nektar-Ts", ts)
		req.Header.Set("X-Nektar-Nonce", nonce)
		req.Header.Set("X-Nektar-Sig", sig)
	}
}

func (b *Broker) sseHostLoop(ctx context.Context, hubURL string) {
	backoff := 250 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		cursor := b.loadCursor(hubURL)
		urlStr := fmt.Sprintf("%s/events?cursor=%d&format=slim", strings.TrimRight(hubURL, "/"), cursor)
		if fj := b.engine.FastJournal(); fj != nil {
			if listings, lErr := fj.CachedListings(b.mem.DB()); lErr == nil && len(listings) == 0 {
				urlStr += "&kinds=offer"
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
		if err != nil {
			return
		}
		if cursor > 0 {
			req.Header.Set("Last-Event-ID", strconv.FormatInt(cursor, 10))
		}
		if pub := b.PubHex(); pub != "" {
			req.Header.Set("X-Nektar-Pub", pub)
		}
		b.attachHubEgressAuth(req, "hub_events", hubURL, cursor, "")
		// Streaming SSRF-hardened client with no total timeout (context cancels cleanly on shutdown).
		resp, err := b.sseClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusPaymentRequired {
			minTopup, _ := strconv.ParseUint(strings.TrimSpace(resp.Header.Get("X-BSV-Payment-Satoshis-Required")), 10, 64)
			resp.Body.Close()
			if _, tErr := b.disc.TopUpHubEgressCredits(ctx, hubURL, minTopup, b.PubHex()); tErr == nil {
				backoff = 100 * time.Millisecond
				continue
			}
			// Policy denied or wallet dry — back off gently instead of hammering 402.
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			jitter := time.Duration(float64(backoff) * (0.5 + rand.Float64()))
			select {
			case <-ctx.Done():
				return
			case <-time.After(jitter):
			}
			if backoff < 2*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 250 * time.Millisecond
		if lsStr := strings.TrimSpace(resp.Header.Get("X-Nektar-Latest-Seq")); lsStr != "" {
			if latest, pErr := strconv.ParseInt(lsStr, 10, 64); pErr == nil && latest < cursor {
				b.resetCursor(hubURL, 0)
			}
		}
		b.sseConnected.Add(1)
		b.signalSSEConnected()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		fastDispatch := b.engine.FastJournal() != nil
		var (
			dispatchCh chan bstack.MarketEvent
			dispatchWG sync.WaitGroup
		)
		if fastDispatch {
			dispatchCh = make(chan bstack.MarketEvent, 512)
			for i := 0; i < 8; i++ {
				dispatchWG.Add(1)
				go func() {
					defer dispatchWG.Done()
					for ev := range dispatchCh {
						if !b.handleMarketEvent(ctx, ev) {
							b.saveCursor(hubURL, ev.Seq)
						}
					}
				}()
			}
		}
		var pendingEvent string
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: ") {
				pendingEvent = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			raw := strings.TrimPrefix(line, "data: ")
			if pendingEvent == "cursor_reset" {
				pendingEvent = ""
				b.resetCursor(hubURL, 0)
				continue
			}
			if pendingEvent == "bbox_wake" {
				pendingEvent = ""
				var bw struct {
					Pub string `json:"pub"`
				}
				if json.Unmarshal([]byte(raw), &bw) == nil && (bw.Pub == "" || strings.EqualFold(bw.Pub, b.PubHex())) {
					b.WakeBbox()
				}
				continue
			}
			if pendingEvent == "payment_required" {
				pendingEvent = ""
				var pr struct {
					MinTopupSats uint64 `json:"minTopupSats"`
				}
				_ = json.Unmarshal([]byte(raw), &pr)
				_, _ = b.disc.TopUpHubEgressCredits(ctx, hubURL, pr.MinTopupSats, b.PubHex())
				break
			}
			pendingEvent = ""
			var ev bstack.MarketEvent
			if err := json.Unmarshal([]byte(raw), &ev); err != nil {
				continue
			}
			b.sseWakes.Add(1)
			if fastDispatch {
				select {
				case dispatchCh <- ev:
				case <-ctx.Done():
				}
				continue
			}
			if prev := b.loadCursor(hubURL); prev > 0 && ev.Seq > prev+1 {
				b.pollHostOnce(ctx, hubURL)
			}
			if !b.handleMarketEvent(ctx, ev) {
				b.saveCursor(hubURL, ev.Seq)
			}
		}
		if dispatchCh != nil {
			close(dispatchCh)
			dispatchWG.Wait()
		}
		b.sseConnected.Add(-1)
		resp.Body.Close()
	}
}

func (b *Broker) pollHostOnce(ctx context.Context, hubURL string) {
	// Paginate across full /sync backlog (200 events/page) so SSE gaps > 200 never skip events!
	for page := 0; page < 50; page++ {
		cursor := b.loadCursor(hubURL)
		var resp *http.Response
		for attempt := 0; attempt < 2; attempt++ {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/sync?cursor=%d", strings.TrimRight(hubURL, "/"), cursor), nil)
			if err != nil {
				return
			}
			b.attachHubEgressAuth(req, "hub_sync", hubURL, cursor, "")
			r, err := b.httpClient.Do(req)
			if err != nil {
				return
			}
			if r.StatusCode == http.StatusPaymentRequired && attempt == 0 {
				minTopup, _ := strconv.ParseUint(strings.TrimSpace(r.Header.Get("X-BSV-Payment-Satoshis-Required")), 10, 64)
				r.Body.Close()
				if _, tErr := b.disc.TopUpHubEgressCredits(ctx, hubURL, minTopup, b.PubHex()); tErr == nil {
					continue
				}
				return
			}
			if r.StatusCode != http.StatusOK {
				r.Body.Close()
				return
			}
			resp = r
			break
		}
		if resp == nil {
			return
		}
		if lsStr := strings.TrimSpace(resp.Header.Get("X-Nektar-Latest-Seq")); lsStr != "" {
			if latest, pErr := strconv.ParseInt(lsStr, 10, 64); pErr == nil && latest < cursor {
				b.resetCursor(hubURL, 0)
				cursor = 0
			}
		}
		var items []hub.StoredEvent
		dErr := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&items)
		resp.Body.Close()
		if dErr != nil || len(items) == 0 {
			return
		}
		b.pollWakes.Add(1)
		var maxSeq int64
		hitRetryable := false
		for _, it := range items {
			retryable := b.handleMarketEvent(ctx, bstack.MarketEvent{
				Seq:        it.Seq,
				Topic:      it.Topic,
				TxID:       it.TxID,
				Vout:       it.Vout,
				AdmittedAt: it.AdmittedAt,
				Payload:    it.Payload,
				BEEF:       it.BEEF,
			})
			if retryable {
				// Tracker transport outage: stop advancing cursor so this event is re-verified once headers recover.
				hitRetryable = true
				break
			}
			if it.Seq > maxSeq {
				maxSeq = it.Seq
			}
		}
		if maxSeq > cursor {
			b.saveCursor(hubURL, maxSeq)
		} else {
			return
		}
		if hitRetryable || len(items) < 200 {
			return
		}
	}
}
