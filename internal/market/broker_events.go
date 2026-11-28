package market

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
)

func (b *Broker) isPlaneConnected(hubURL string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.planeHosts[hubURL]
}

func (b *Broker) setPlaneConnected(hubURL string, live bool) {
	b.mu.Lock()
	if b.planeHosts == nil {
		b.planeHosts = make(map[string]bool)
	}
	if live {
		b.planeHosts[hubURL] = true
	} else {
		delete(b.planeHosts, hubURL)
	}
	b.mu.Unlock()
}

// electedPlaneTopics computes the exact BRC-149 / BRC-223 topic interest set for this agent:
//   - All 16 market nibble lanes (`tm_nektar_<net>_0..f`) + listed semantic `dom_*` topics
//   - Deterministic NAT settlement topics (`tm_bbox_<office>` + `tm_nektar_bbox_<office>`)
//   - LightBSV identity & capsule storage topics (`tm_finger`, `tm_bstore_<bucket>`)
func (b *Broker) electedPlaneTopics() []string {
	netName := "testnet"
	if b.mem != nil && b.mem.Chain() != nil && b.mem.Chain().Network() != "" {
		netName = b.mem.Chain().Network()
	}
	seen := make(map[string]bool, 24)
	out := make([]string, 0, 24)
	add := func(t string) {
		if t = strings.TrimSpace(t); t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, t := range bstack.MarketTopics(netName) {
		add(t)
	}
	b.mu.RLock()
	office := b.cfg.BboxOffice
	b.mu.RUnlock()
	normOffice := bstack.NormalizeBboxOffice(office)
	add("tm_bbox_" + normOffice)
	add(hub.BboxPlaneTopic(normOffice))
	defaultOffice := bstack.NormalizeBboxOffice("nektar")
	if defaultOffice != normOffice {
		add("tm_bbox_" + defaultOffice)
		add(hub.BboxPlaneTopic(defaultOffice))
	}
	add("tm_finger")
	add("tm_bstore_" + bstack.NormalizeBstoreBucket("nektar"))

	if b.mem != nil && b.mem.DB() != nil {
		var listings []struct{ Domains []string }
		if fj := b.engine.FastJournal(); fj != nil {
			if ls, err := fj.CachedListings(b.mem.DB()); err == nil {
				for _, l := range ls {
					if l.Active {
						listings = append(listings, struct{ Domains []string }{Domains: l.Domains})
					}
				}
			}
		} else if ls, err := b.mem.DB().ListSellListings(); err == nil {
			for _, l := range ls {
				if l.Active {
					listings = append(listings, struct{ Domains []string }{Domains: l.Domains})
				}
			}
		}
		for _, l := range listings {
			for _, d := range l.Domains {
				if dt := bstack.DomainTopic(netName, d); dt != "" {
					add(dt)
				}
			}
		}
	}
	return out
}

func (b *Broker) handlePlaneDelivery(ctx context.Context, hubURL string, seq int64, topic string, rawBEEF []byte) bool {
	if len(rawBEEF) == 0 {
		return false
	}
	if strings.HasPrefix(topic, "tm_nektar_bbox_") {
		var msg hub.BboxMessage
		if json.Unmarshal(rawBEEF, &msg) == nil && strings.EqualFold(msg.RecipientPub, b.PubHex()) {
			b.bboxWakes.Add(1)
			b.enqueueBboxWork(ctx, hubURL, msg, false)
		}
		return false
	}
	if strings.HasPrefix(topic, "tm_bbox_") {
		b.WakeBbox()
		return false
	}
	if !strings.HasPrefix(topic, "tm_nektar_") {
		return false
	}
	myNet := b.mem.Chain().Network()
	isLive := myNet == "main" || myNet == "testnet"
	offchainAds := os.Getenv("NEKTAR_OFFCHAIN_ADS") == "1"
	var tx *transaction.Transaction
	_, parsedTx, _, pErr := guard.ParseBEEF(rawBEEF, guard.DefaultBound)
	if pErr == nil && parsedTx != nil {
		tx = parsedTx
	} else if (!isLive || offchainAds) && pErr != nil && strings.Contains(pErr.Error(), "no inputs") && len(rawBEEF) <= 64<<10 {
		if t0, tErr := transaction.NewTransactionFromBEEF(rawBEEF); tErr == nil && t0 != nil && len(t0.Inputs) == 0 {
			tx = t0
		}
	}
	if tx == nil {
		return false
	}
	txidStr := tx.TxID().String()
	for vout, out := range tx.Outputs {
		if out == nil || out.LockingScript == nil {
			continue
		}
		p, isNkt, _ := bstack.ExtractNkt1Payload(out.LockingScript)
		if !isNkt {
			continue
		}
		b.sseWakes.Add(1)
		if b.handleMarketEvent(ctx, bstack.MarketEvent{
			Seq:     seq,
			Topic:   topic,
			TxID:    txidStr,
			Vout:    uint32(vout),
			Payload: p,
			BEEF:    rawBEEF,
		}) {
			return true
		}
	}
	return false
}

func (b *Broker) repairHubPlaneGap(ctx context.Context, hubURL string, fromSeq, toSeq uint64, topicsCSV string, topicByID map[[32]byte]string) error {
	base := strings.TrimRight(hubURL, "/")
	for chunkFrom := fromSeq; chunkFrom <= toSeq; {
		chunkTo := toSeq
		if chunkTo-chunkFrom >= 500 {
			chunkTo = chunkFrom + 499
		}
		u := fmt.Sprintf("%s/plane/nack?from=%d&to=%d", base, chunkFrom, chunkTo)
		if topicsCSV != "" {
			u += "&topics=" + url.QueryEscape(topicsCSV)
		}
		var resp *http.Response
		for attempt := 0; attempt < 2; attempt++ {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			if err != nil {
				return err
			}
			b.attachHubEgressAuth(req, "hub_plane_nack", hubURL, int64(chunkFrom), topicsCSV)
			r, err := b.httpClient.Do(req)
			if err != nil {
				return err
			}
			if r.StatusCode == http.StatusPaymentRequired && attempt == 0 {
				minTopup, _ := strconv.ParseUint(strings.TrimSpace(r.Header.Get("X-BSV-Payment-Satoshis-Required")), 10, 64)
				r.Body.Close()
				if _, tErr := b.disc.TopUpHubEgressCredits(ctx, hubURL, minTopup, b.PubHex()); tErr == nil {
					continue
				}
				return fmt.Errorf("plane nack 402 at %s", hubURL)
			}
			if r.StatusCode != http.StatusOK {
				r.Body.Close()
				return fmt.Errorf("plane nack HTTP %d at %s", r.StatusCode, hubURL)
			}
			resp = r
			break
		}
		if resp == nil {
			return fmt.Errorf("plane nack empty response at %s", hubURL)
		}
		err := func() error {
			defer resp.Body.Close()
			for {
				frame, dErr := hub.DecodePlaneFrame(resp.Body, hub.MaxPlaneObjectLen)
				if dErr != nil {
					if errors.Is(dErr, io.EOF) {
						return nil
					}
					return dErr
				}
				if topic, ok := topicByID[frame.TopicID]; ok {
					if b.handlePlaneDelivery(ctx, hubURL, int64(frame.FlowSeq), topic, frame.BEEF) {
						return errors.New("transient tracker outage during plane nack")
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

// ReconnectStreams cancels and restarts active Hub /plane/stream (or /events fallback) loops
// and refreshes BRC-223 UDP multicast interest whenever local SellListings change at runtime.
func (b *Broker) ReconnectStreams() {
	if b == nil {
		return
	}
	b.startMu.Lock()
	started := b.started
	runCtx := b.runCtx
	b.startMu.Unlock()

	elected := b.electedPlaneTopics()
	b.mu.Lock()
	fab := b.fabricSub
	for h, cancel := range b.activeLoops {
		cancel()
		delete(b.activeLoops, h)
	}
	b.mu.Unlock()
	if fab != nil {
		fab.SetTopics(elected)
	}
	if started && runCtx != nil && runCtx.Err() == nil {
		b.ensureHostLoops(runCtx)
	}
}

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
	// Fast-journal mode uses targeted O(1) pubkey/domain-filtered slim SSE (?format=slim) with 8-worker
	// parallel dispatch (avoiding 12x full-BEEF broadcast amplification across all nibble lanes);
	// standard mode uses numbered binary BRC-149 /plane/stream with /plane/nack gap repair.
	useSSEFallback := b.engine.FastJournal() != nil
	for {
		if ctx.Err() != nil {
			return
		}
		if !useSSEFallback {
			status, ok := b.consumeHubPlaneOnce(ctx, hubURL)
			if ctx.Err() != nil {
				return
			}
			if status == http.StatusNotFound {
				// Legacy mock test server without /plane/stream: fall back cleanly to /events SSE.
				useSSEFallback = true
				continue
			}
			if ok {
				backoff = 250 * time.Millisecond
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

		cursor := b.loadCursor(hubURL)
		urlStr := fmt.Sprintf("%s/events?cursor=%d&format=slim", strings.TrimRight(hubURL, "/"), cursor)
		domainsCSV := ""
		if fj := b.engine.FastJournal(); fj != nil {
			if listings, lErr := fj.CachedListings(b.mem.DB()); lErr == nil {
				if len(listings) == 0 {
					urlStr += "&kinds=offer"
				} else {
					hasWild := false
					domSeen := make(map[string]bool, 8)
					var listedDoms []string
					for i := range listings {
						if !listings[i].Active {
							continue
						}
						for _, d := range listings[i].Domains {
							dl := strings.ToLower(strings.TrimSpace(d))
							if dl == "*" {
								hasWild = true
								break
							}
							if dl != "" && !domSeen[dl] {
								domSeen[dl] = true
								listedDoms = append(listedDoms, dl)
							}
						}
						if hasWild {
							break
						}
					}
					if !hasWild && len(listedDoms) > 0 {
						domainsCSV = strings.Join(listedDoms, ",")
						urlStr += "&domains=" + domainsCSV
					}
				}
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
		b.attachHubEgressAuth(req, "hub_events", hubURL, cursor, domainsCSV)
		// Streaming SSRF-hardened client with no total timeout (context cancels cleanly on shutdown).
		resp, err := b.sseClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusPaymentRequired {
			minTopup, _ := strconv.ParseUint(strings.TrimSpace(resp.Header.Get("X-BSV-Payment-Satoshis-Required")), 10, 64)
			resp.Body.Close()
			if _, tErr := b.disc.TopUpHubEgressCredits(ctx, hubURL, minTopup, b.PubHex()); tErr == nil {
				backoff = 100 * time.Millisecond
				continue
			}
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
			if pendingEvent == "gap" {
				// Hub slow-consumer overflow signal (RFC-003): flush and reconnect immediately from persisted cursor.
				pendingEvent = ""
				break
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

func (b *Broker) consumeHubPlaneOnce(ctx context.Context, hubURL string) (int, bool) {
	elected := b.electedPlaneTopics()
	topicByID := make(map[[32]byte]string, len(elected))
	for _, t := range elected {
		topicByID[hub.TopicIDOf(t)] = t
	}
	topicsCSV := strings.Join(elected, ",")
	cursor := b.loadCursor(hubURL)
	if cursor < 0 {
		cursor = 0
	}
	u := fmt.Sprintf("%s/plane/stream?from=%d", strings.TrimRight(hubURL, "/"), cursor)
	if topicsCSV != "" {
		u += "&topics=" + url.QueryEscape(topicsCSV)
	}
	var resp *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return 0, false
		}
		if pub := b.PubHex(); pub != "" {
			req.Header.Set("X-Nektar-Pub", pub)
		}
		b.attachHubEgressAuth(req, "hub_plane", hubURL, cursor, topicsCSV)
		r, err := b.sseClient.Do(req)
		if err != nil {
			return 0, false
		}
		if r.StatusCode == http.StatusPaymentRequired && attempt == 0 {
			minTopup, _ := strconv.ParseUint(strings.TrimSpace(r.Header.Get("X-BSV-Payment-Satoshis-Required")), 10, 64)
			r.Body.Close()
			if _, tErr := b.disc.TopUpHubEgressCredits(ctx, hubURL, minTopup, b.PubHex()); tErr == nil {
				continue
			}
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
			return http.StatusPaymentRequired, false
		}
		if r.StatusCode != http.StatusOK {
			code := r.StatusCode
			r.Body.Close()
			return code, false
		}
		resp = r
		break
	}
	if resp == nil {
		return 0, false
	}
	defer resp.Body.Close()
	if lsStr := strings.TrimSpace(resp.Header.Get("X-Nektar-Latest-Seq")); lsStr != "" {
		if latest, pErr := strconv.ParseInt(lsStr, 10, 64); pErr == nil && latest < cursor {
			b.resetCursor(hubURL, 0)
			cursor = 0
		}
	}
	b.setPlaneConnected(hubURL, true)
	b.sseConnected.Add(1)
	b.signalSSEConnected()
	defer func() {
		b.sseConnected.Add(-1)
		b.setPlaneConnected(hubURL, false)
	}()

	type planeJob struct {
		seq   int64
		topic string
		beef  []byte
	}
	jobCh := make(chan planeJob, 512)
	var workerWG sync.WaitGroup
	for i := 0; i < 8; i++ {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			for j := range jobCh {
				if !b.handlePlaneDelivery(ctx, hubURL, j.seq, j.topic, j.beef) {
					b.saveCursor(hubURL, j.seq)
				}
			}
		}()
	}
	defer func() {
		close(jobCh)
		workerWG.Wait()
	}()

	lastSeq := uint64(cursor)
	for {
		frame, err := hub.DecodePlaneFrame(resp.Body, hub.MaxPlaneObjectLen)
		if err != nil {
			return http.StatusOK, true
		}
		if lastSeq > 0 && frame.FlowSeq > lastSeq+1 {
			if gErr := b.repairHubPlaneGap(ctx, hubURL, lastSeq+1, frame.FlowSeq-1, topicsCSV, topicByID); gErr != nil {
				return http.StatusOK, true
			}
		}
		if frame.FlowSeq > lastSeq {
			lastSeq = frame.FlowSeq
		}
		if topic, ok := topicByID[frame.TopicID]; ok {
			select {
			case jobCh <- planeJob{seq: int64(frame.FlowSeq), topic: topic, beef: frame.BEEF}:
			case <-ctx.Done():
				return http.StatusOK, true
			}
		} else {
			b.saveCursor(hubURL, int64(frame.FlowSeq))
		}
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
