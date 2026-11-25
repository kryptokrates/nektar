package market

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/verify"

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
	var v int64
	ok, _ := b.mem.DB().GetJSON("hub_cursor:"+hubURL, &v)
	if !ok {
		return 0
	}
	return v
}

func (b *Broker) saveCursor(hubURL string, seq int64) {
	if seq <= 0 {
		return
	}
	b.cursorMu.Lock()
	defer b.cursorMu.Unlock()
	var cur int64
	_, _ = b.mem.DB().GetJSON("hub_cursor:"+hubURL, &cur)
	if seq > cur {
		_ = b.mem.DB().SetJSON("hub_cursor:"+hubURL, seq)
	}
}

func (b *Broker) attachHubEgressAuth(req *http.Request, action, hubURL string, cursor int64, domainsCSV string) {
	if b == nil || b.engine == nil || b.engine.saleKey == nil {
		return
	}
	if pub, ts, nonce, sig, err := discovery.SignEgressAuthHeaders(b.engine.saleKey, action, hubURL, cursor, domainsCSV); err == nil {
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
		urlStr := fmt.Sprintf("%s/events?cursor=%d", strings.TrimRight(hubURL, "/"), cursor)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
		if err != nil {
			return
		}
		if cursor > 0 {
			req.Header.Set("Last-Event-ID", strconv.FormatInt(cursor, 10))
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
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
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
			// Gap detection: if an SSE event jumped ahead of our persisted cursor, repair via /sync first!
			if prev := b.loadCursor(hubURL); prev > 0 && ev.Seq > prev+1 {
				b.pollHostOnce(ctx, hubURL)
			}
			b.handleMarketEvent(ctx, ev)
			b.saveCursor(hubURL, ev.Seq)
		}
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
		var items []hub.StoredEvent
		dErr := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&items)
		resp.Body.Close()
		if dErr != nil || len(items) == 0 {
			return
		}
		b.pollWakes.Add(1)
		var maxSeq int64
		for _, it := range items {
			b.handleMarketEvent(ctx, bstack.MarketEvent{
				Seq:        it.Seq,
				Topic:      it.Topic,
				TxID:       it.TxID,
				Vout:       it.Vout,
				AdmittedAt: it.AdmittedAt,
				Payload:    it.Payload,
				BEEF:       it.BEEF,
			})
			if it.Seq > maxSeq {
				maxSeq = it.Seq
			}
		}
		if maxSeq > cursor {
			b.saveCursor(hubURL, maxSeq)
		} else {
			return
		}
		if len(items) < 200 {
			return
		}
	}
}

// verifyMarketEventBEEF validates carried BEEF bytes on an incoming Hub MarketEvent before trusting its payload.
// On live networks (main/testnet), BEEF is mandatory and verified via bstack.VerifyMarketCarrier + bcommon/verify.Check.
func (b *Broker) verifyMarketEventBEEF(ctx context.Context, ev bstack.MarketEvent) (bstack.MarketPayload, bool) {
	myNet := b.mem.Chain().Network()
	isLive := myNet == "main" || myNet == "testnet"
	if isLive && len(ev.BEEF) == 0 {
		return bstack.MarketPayload{}, false
	}
	if len(ev.BEEF) == 0 {
		return ev.Payload, true
	}
	var tx *transaction.Transaction
	_, parsedTx, _, pErr := guard.ParseBEEF(ev.BEEF, guard.DefaultBound)
	if pErr == nil && parsedTx != nil {
		tx = parsedTx
	} else if !isLive {
		if t0, tErr := transaction.NewTransactionFromBEEF(ev.BEEF); tErr == nil && t0 != nil {
			tx = t0
		}
	}
	if tx == nil || (ev.TxID != "" && !strings.EqualFold(tx.TxID().String(), ev.TxID)) || int(ev.Vout) >= len(tx.Outputs) {
		return bstack.MarketPayload{}, false
	}
	if isLive {
		vout, p, cErr := bstack.VerifyMarketCarrier(tx, myNet, ev.Topic, time.Now())
		if cErr != nil || vout != ev.Vout {
			return bstack.MarketPayload{}, false
		}
		tracker := b.mem.Chain().Tracker()
		if tracker == nil {
			return bstack.MarketPayload{}, false
		}
		if verdict, _ := verify.Check(ctx, tx, tracker); verdict != verify.Passed {
			return bstack.MarketPayload{}, false
		}
		return p, true
	}
	p, ok, _ := bstack.ExtractNkt1Payload(tx.Outputs[ev.Vout].LockingScript)
	if !ok {
		return bstack.MarketPayload{}, false
	}
	return p, true
}

func (b *Broker) handleMarketEvent(ctx context.Context, ev bstack.MarketEvent) {
	payload, ok := b.verifyMarketEventBEEF(ctx, ev)
	if !ok {
		return
	}
	nowMs := time.Now().UnixMilli()
	maxExpMs := nowMs + bstack.MaxLifetime.Milliseconds() + 60_000
	myNet := b.mem.Chain().Network()
	switch payload.Kind {
	case bstack.KindWant:
		var sw bstack.SignedWant
		if err := json.Unmarshal(payload.Body, &sw); err != nil {
			return
		}
		if sw.Want.Net != myNet || sw.Want.Exp <= nowMs || sw.Want.Exp > maxExpMs {
			return
		}
		if err := bstack.VerifyArtifact(sw.Want.Pub, sw.Sig, sw.Want); err != nil {
			return
		}
		if sw.Want.Pub == b.PubHex() {
			return // ignore our own want
		}
		b.mu.Lock()
		if _, dup := b.seenWants[sw.Want.WantID]; dup {
			b.mu.Unlock()
			return
		}
		nowUnix := time.Now().Unix()
		if len(b.seenWants) >= 4096 {
			cutoff := nowUnix - 3600
			for k, ts := range b.seenWants {
				if ts < cutoff || len(b.seenWants) >= 3072 {
					delete(b.seenWants, k)
				}
			}
		}
		b.seenWants[sw.Want.WantID] = nowUnix
		b.mu.Unlock()
		go b.autoQuoteWant(ctx, sw.Want)

	case bstack.KindOffer:
		var so bstack.SignedOffer
		if err := json.Unmarshal(payload.Body, &so); err != nil {
			return
		}
		if so.Offer.Net != myNet || so.Offer.Exp <= nowMs || so.Offer.Exp > maxExpMs {
			return
		}
		if leafRaw, lErr := hex.DecodeString(strings.TrimSpace(so.Offer.Leaf)); lErr != nil || len(leafRaw) != 32 {
			return
		}
		if err := bstack.VerifyArtifact(so.Offer.Pub, so.Sig, so.Offer); err != nil {
			return
		}
		if so.Offer.BuyerPub != "" && so.Offer.BuyerPub != b.PubHex() {
			return
		}
		b.mu.RLock()
		ch := b.offerInbox[so.Offer.WantID]
		b.mu.RUnlock()
		if ch != nil {
			select {
			case ch <- so.Offer:
			default:
			}
		}
	}
}
