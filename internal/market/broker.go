package market

import (
	"bufio"
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

// BrokerConfig configures the autonomous live A2A & Hub Market Broker.
type BrokerConfig struct {
	PublicURL      string // reachable HTTP base URL for direct purchase (empty if behind NAT!)
	BboxOffice     string // NAT store-and-forward office name (default "default")
	Handle         string // optional bfinger handle (e.g. "alice@nektar.run")
	IsHub          bool   // true when running as a Hub liquidity maker
	CollectTimeout time.Duration
	OnStateChange  func(event string, payload any)
	HTTPClient     *http.Client
	RestateClient  *RestateClient // active Restate durable executor client (BuyerDeal, SellerDeal, Custody, Tx, AnchorCadence)
}

// WantRequest describes a buyer's live knowledge purchase request across active hubs.
type WantRequest struct {
	Domains            []string      `json:"domains"`
	Question           string        `json:"question"`
	MaxSats            uint64        `json:"maxSats"`
	TargetShard        string        `json:"targetShard,omitempty"`
	CollectTimeout     time.Duration `json:"collectTimeout,omitempty"`
	ApprovedOverrideID string        `json:"approvedOverrideId,omitempty"`
}

// SellInput arms a local memory shard for autonomous quoting & selling on the market.
type SellInput struct {
	Shard       string   `json:"shard"`
	Domains     []string `json:"domains"`
	PriceSats   uint64   `json:"priceSats"`
	Description string   `json:"description,omitempty"`
}

// PurchaseRequest is sent from Buyer to Seller (via direct HTTP POST /v1/market/purchase OR Hub NAT /bbox/send).
type PurchaseRequest struct {
	OfferID           string `json:"offerId"`
	WantID            string `json:"wantId"`
	BuyerPub          string `json:"buyerPub"`
	TxID              string `json:"txid"`
	PaymentBEEFHex    string `json:"paymentBeefHex"`
	OutputIndex       uint32 `json:"outputIndex,omitempty"`
	DerivationPrefix  string `json:"derivationPrefix,omitempty"`
	DerivationSuffix  string `json:"derivationSuffix,omitempty"`
	SenderIdentityKey string `json:"senderIdentityKey,omitempty"`
	TS                int64  `json:"ts,omitempty"`
	BuyerSig          string `json:"buyerSig,omitempty"`
}

// Signable returns the canonical payload signed by BuyerPub to prove buyer ownership on purchase & key re-reveal.
func (r PurchaseRequest) Signable() map[string]any {
	return map[string]any{
		"action":   "market_purchase",
		"offerId":  r.OfferID,
		"wantId":   r.WantID,
		"buyerPub": r.BuyerPub,
		"txid":     r.TxID,
		"ts":       r.TS,
	}
}

// PurchaseResponse is returned by Seller after validating Buyer's AtomicBEEF payment & BRC-105 replay claim.
type PurchaseResponse struct {
	OK            bool                   `json:"ok"`
	OfferID       string                 `json:"offerId"`
	SellerPub     string                 `json:"sellerPub"`
	ContentKeyHex string                 `json:"contentKeyHex,omitempty"`
	CiphertextHex string                 `json:"ciphertextHex,omitempty"`
	ChirpURL      string                 `json:"chirpUrl,omitempty"`
	ProofBundle   *contracts.ProofBundle `json:"proofBundle,omitempty"`
	Error         string                 `json:"error,omitempty"`
}

type pendingSale struct {
	OfferID     string
	WantID      string
	BuyerPub    string
	Shard       string
	Seq         int64
	SaleOffer   *contracts.KeyedSaleOffer
	RevealKey   [32]byte
	ChirpURL    string
	ProofBundle *contracts.ProofBundle
	ExpiresAt   int64
	Settled     bool
}

const (
	maxPendingSalesTotal    = 128
	maxPendingSalesPerBuyer = 16
	maxSeenBboxNonces       = 8192
)

// Broker drives real autonomous agent-to-agent and agent-to-Hub knowledge trading over live HTTP/SSE + NAT bbox.
type Broker struct {
	mem        *memory.Service
	engine     *Engine
	disc       *discovery.Service
	cfg        BrokerConfig
	httpClient *http.Client
	sseClient  *http.Client

	mu              sync.RWMutex
	restate         *RestateClient
	pendingSales    map[string]*pendingSale
	offerInbox      map[string]chan bstack.OfferAd
	bboxReplies     map[string]chan PurchaseResponse
	bboxExpected    map[string]string // offerID -> expected sellerPub
	bboxWantByOffer map[string]string // offerID -> wantID (for Restate BuyerDeal/deliverKey wakeup)
	activeLoops     map[string]context.CancelFunc
	seenWants       map[string]int64 // wantID -> seenUnix (bounded)
	seenBboxNonces  map[string]int64 // sender:nonce -> seenUnix (bounded replay guard)
	packs           []bstack.PackRef
	bountyDoms      []string

	cursorMu sync.Mutex

	startMu    sync.Mutex
	started    bool
	stopCancel context.CancelFunc
	wg         sync.WaitGroup

	sseWakes  atomic.Int64
	pollWakes atomic.Int64
	bboxWakes atomic.Int64
}

// NewBroker constructs a live Broker attached to mem, engine, and disc, syncing persisted operator policies.
func NewBroker(mem *memory.Service, engine *Engine, disc *discovery.Service, cfg BrokerConfig) *Broker {
	if cfg.BboxOffice == "" {
		cfg.BboxOffice = "default"
	}
	if cfg.CollectTimeout <= 0 {
		cfg.CollectTimeout = 4 * time.Second
	}
	netName := ""
	if mem != nil && mem.Chain() != nil {
		netName = mem.Chain().Network()
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = core.SafeHTTPClientForNetwork(netName, 15*time.Second)
	}
	sseHC := core.SafeHTTPClientForNetwork(netName, 0)
	_ = SyncPolicyFromDB(mem.DB(), engine.Policy())
	b := &Broker{
		mem:             mem,
		engine:          engine,
		disc:            disc,
		cfg:             cfg,
		httpClient:      hc,
		sseClient:       sseHC,
		restate:         cfg.RestateClient,
		pendingSales:    make(map[string]*pendingSale),
		offerInbox:      make(map[string]chan bstack.OfferAd),
		bboxReplies:     make(map[string]chan PurchaseResponse),
		bboxExpected:    make(map[string]string),
		bboxWantByOffer: make(map[string]string),
		activeLoops:     make(map[string]context.CancelFunc),
		seenWants:       make(map[string]int64),
		seenBboxNonces:  make(map[string]int64),
	}
	if engine != nil {
		engine.SetBroker(b)
	}
	b.restorePersistedState()
	return b
}

func (b *Broker) restorePersistedState() {
	if b.mem == nil || b.mem.DB() == nil {
		return
	}
	now := time.Now().Unix()
	_, _ = b.mem.DB().PruneExpiredPendingSales(now)
	if sales, err := b.mem.DB().ListPendingSales(now); err == nil {
		for _, r := range sales {
			if ps := b.pendingSaleFromRecord(context.Background(), r); ps != nil {
				b.pendingSales[r.OfferID] = ps
				// Re-allowlist CHIRP DAG hashes so buyers never hit HTTP 401 on /v1/chirp/{hash} after seller restart!
				if ps.ChirpURL != "" && b.mem.Objects() != nil {
					if hashes, hErr := bstack.CollectChirpHashes(b.mem.Objects(), ps.ChirpURL); hErr == nil && len(hashes) > 0 {
						b.mem.AllowPublicChirpHashes(hashes...)
					}
				}
			}
		}
	}
}

const marketKeyEncPrefix = "enc:"

// sealKeyAtRest encrypts a hex-encoded BRC-369 content key via BRC-43 wallet EncryptAtRest before SQLite persistence (F-28).
func (b *Broker) sealKeyAtRest(ctx context.Context, keyHex string) string {
	if keyHex == "" || strings.HasPrefix(keyHex, marketKeyEncPrefix) || b == nil || b.mem == nil || b.mem.Chain() == nil {
		return keyHex
	}
	if ct, err := b.mem.Chain().EncryptAtRest(ctx, "market-reveal-key", []byte(keyHex)); err == nil && len(ct) > 0 {
		return marketKeyEncPrefix + hex.EncodeToString(ct)
	}
	return keyHex
}

// openKeyAtRest unwraps a BRC-43 encrypted key ("enc:" prefix) or returns legacy plaintext hex.
func (b *Broker) openKeyAtRest(ctx context.Context, stored string) string {
	if !strings.HasPrefix(stored, marketKeyEncPrefix) || b == nil || b.mem == nil || b.mem.Chain() == nil {
		return stored
	}
	ct, err := hex.DecodeString(strings.TrimPrefix(stored, marketKeyEncPrefix))
	if err != nil {
		return ""
	}
	pt, err := b.mem.Chain().DecryptAtRest(ctx, "market-reveal-key", ct)
	if err != nil {
		return ""
	}
	res := string(pt)
	core.ZeroBytes(pt)
	return res
}

func (b *Broker) pendingSaleFromRecord(ctx context.Context, r store.PendingSaleRecord) *pendingSale {
	var so contracts.KeyedSaleOffer
	if err := json.Unmarshal(r.SaleOfferJSON, &so); err != nil {
		return nil
	}
	plainHex := b.openKeyAtRest(ctx, r.RevealKeyHex)
	rkBytes, err := hex.DecodeString(plainHex)
	if err != nil || len(rkBytes) != 32 {
		return nil
	}
	var rk [32]byte
	copy(rk[:], rkBytes)
	core.ZeroBytes(rkBytes)
	var pb *contracts.ProofBundle
	if len(r.ProofBundleJSON) > 0 {
		var tmp contracts.ProofBundle
		if json.Unmarshal(r.ProofBundleJSON, &tmp) == nil {
			pb = &tmp
		}
	}
	return &pendingSale{
		OfferID:     r.OfferID,
		WantID:      r.WantID,
		BuyerPub:    r.BuyerPub,
		Shard:       r.Shard,
		Seq:         r.Seq,
		SaleOffer:   &so,
		RevealKey:   rk,
		ChirpURL:    r.ChirpURL,
		ProofBundle: pb,
		ExpiresAt:   r.ExpiresAt,
		Settled:     r.Settled,
	}
}

func (b *Broker) Engine() *Engine               { return b.engine }
func (b *Broker) Discovery() *discovery.Service { return b.disc }
func (b *Broker) PubHex() string                { return hex.EncodeToString(b.engine.saleKey.PubKey().Compressed()) }
func (b *Broker) PublicURL() string             { return strings.TrimRight(b.cfg.PublicURL, "/") }
func (b *Broker) SetRestateClient(rc *RestateClient) {
	b.mu.Lock()
	b.restate = rc
	b.mu.Unlock()
}
func (b *Broker) RestateClient() *RestateClient {
	b.mu.RLock()
	rc := b.restate
	b.mu.RUnlock()
	return rc
}
func (b *Broker) SetPublicURL(u string) {
	b.mu.Lock()
	b.cfg.PublicURL = strings.TrimRight(u, "/")
	b.mu.Unlock()
}
func (b *Broker) SetAdvertisedPacks(packs []bstack.PackRef, bountyDomains []string) {
	b.mu.Lock()
	b.packs = append([]bstack.PackRef(nil), packs...)
	b.bountyDoms = append([]string(nil), bountyDomains...)
	b.mu.Unlock()
}

// AdvertisedPacks returns a copy of explicitly published starter/public packs.
func (b *Broker) AdvertisedPacks() []bstack.PackRef {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]bstack.PackRef(nil), b.packs...)
}

func (b *Broker) signPurchaseRequest(req *PurchaseRequest) {
	if req == nil || b.engine == nil || b.engine.saleKey == nil {
		return
	}
	if req.BuyerPub == "" {
		req.BuyerPub = b.PubHex()
	}
	req.TS = time.Now().Unix()
	if sig, err := bstack.SignArtifact(b.engine.saleKey, req.Signable()); err == nil {
		req.BuyerSig = sig
	}
}

func hubAudience(hubURL string) string {
	u, err := url.Parse(strings.TrimSpace(hubURL))
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimSpace(hubURL))
	}
	return strings.ToLower(u.Host)
}

func randNonceHex() string {
	var b [16]byte
	_, _ = crand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Start launches per-host SSE market subscribers (with persisted Last-Event-ID cursors),
// a jittered poll-floor backup, and the NAT-safe bbox inbox consumer. Idempotent across calls.
func (b *Broker) Start(ctx context.Context) {
	b.startMu.Lock()
	if b.started {
		b.startMu.Unlock()
		return
	}
	b.started = true
	runCtx, cancel := context.WithCancel(ctx)
	b.stopCancel = cancel
	b.startMu.Unlock()

	b.ensureHostLoops(runCtx)
	b.wg.Add(4)
	go func() {
		defer b.wg.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				b.ensureHostLoops(runCtx)
			}
		}
	}()
	// Jittered low-frequency poll floor catching any events dropped across SSE reconnects (event-arch skill).
	go func() {
		defer b.wg.Done()
		for {
			wait := time.Duration(float64(8*time.Second) * (0.6 + 0.8*rand.Float64()))
			select {
			case <-runCtx.Done():
				return
			case <-time.After(wait):
				for _, h := range b.disc.ActiveHosts() {
					b.pollHostOnce(runCtx, h)
				}
			}
		}
	}()
	// NAT-safe bbox inbox loop (delivers purchases to NAT-ed sellers & key reveals to buyers).
	go func() {
		defer b.wg.Done()
		b.bboxLoop(runCtx)
	}()

	// Immediate boot peer crawl + periodic directory crawl (every 15m) & 24h announce keep-alive.
	go func() {
		defer b.wg.Done()
		if hosts := b.disc.ActiveHosts(); len(hosts) > 0 {
			_, _ = b.disc.DiscoverPeers(runCtx, hosts)
		}
		crawlTicker := time.NewTicker(15 * time.Minute)
		announceTicker := time.NewTicker(24 * time.Hour)
		defer crawlTicker.Stop()
		defer announceTicker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-crawlTicker.C:
				if hosts := b.disc.ActiveHosts(); len(hosts) > 0 {
					_, _ = b.disc.DiscoverPeers(runCtx, hosts)
				}
			case <-announceTicker.C:
				if hosts := b.disc.ActiveHosts(); len(hosts) > 0 {
					_, _ = b.AnnounceSelf(runCtx)
				}
			}
		}
	}()
}

// Stop cancels all background Broker SSE, poll, bbox, and crawl loops and waits for them to exit cleanly.
func (b *Broker) Stop() {
	b.startMu.Lock()
	cancel := b.stopCancel
	b.started = false
	b.stopCancel = nil
	b.startMu.Unlock()
	if cancel != nil {
		cancel()
	}
	b.mu.Lock()
	for h, c := range b.activeLoops {
		c()
		delete(b.activeLoops, h)
	}
	b.mu.Unlock()
	b.wg.Wait()
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
		// Streaming SSRF-hardened client with no total timeout (context cancels cleanly on shutdown).
		resp, err := b.sseClient.Do(req)
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
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			raw := strings.TrimPrefix(line, "data: ")
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
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/sync?cursor=%d", strings.TrimRight(hubURL, "/"), cursor), nil)
		if err != nil {
			return
		}
		resp, err := b.httpClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
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

func (b *Broker) handleMarketEvent(ctx context.Context, ev bstack.MarketEvent) {
	nowMs := time.Now().UnixMilli()
	maxExpMs := nowMs + bstack.MaxLifetime.Milliseconds() + 60_000
	myNet := b.mem.Chain().Network()
	switch ev.Payload.Kind {
	case bstack.KindWant:
		var sw bstack.SignedWant
		if err := json.Unmarshal(ev.Payload.Body, &sw); err != nil {
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
		if err := json.Unmarshal(ev.Payload.Body, &so); err != nil {
			return
		}
		if so.Offer.Net != myNet || so.Offer.Exp <= nowMs || so.Offer.Exp > maxExpMs {
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

// ArmSell persists a SellListing, anchors the shard if spendable coins exist, and re-announces on `dir`.
func (b *Broker) ArmSell(ctx context.Context, in SellInput) (store.SellListing, error) {
	if strings.TrimSpace(in.Shard) == "" {
		return store.SellListing{}, errors.New("market: shard is required")
	}
	if len(in.Domains) == 0 {
		in.Domains = []string{"general"}
	}
	if in.PriceSats == 0 {
		in.PriceSats = 100
	}
	norm := make([]string, 0, len(in.Domains))
	for _, d := range in.Domains {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			norm = append(norm, d)
		}
	}
	listing := store.SellListing{
		Shard:       strings.TrimSpace(in.Shard),
		Domains:     norm,
		PriceSats:   in.PriceSats,
		Description: in.Description,
		Active:      true,
		CreatedAt:   time.Now().Unix(),
	}
	if err := b.mem.DB().SaveSellListing(listing); err != nil {
		return store.SellListing{}, err
	}
	if b.mem.Chain().BalanceSats() > 1000 {
		_, _ = b.mem.AnchorShard(ctx, listing.Shard)
	}
	_, _ = b.AnnounceSelf(ctx)
	b.notify("sell_armed", listing)
	return listing, nil
}

// AnnounceSelf publishes this node's signed AnnounceAd to the enumerable `dir` lane across active hubs.
func (b *Broker) AnnounceSelf(ctx context.Context) (int, error) {
	listings, _ := b.mem.DB().ListSellListings()
	domSeen := make(map[string]bool)
	var sellDoms []string
	for _, l := range listings {
		if !l.Active {
			continue
		}
		for _, d := range l.Domains {
			if !domSeen[d] {
				domSeen[d] = true
				sellDoms = append(sellDoms, d)
			}
		}
	}
	b.mu.RLock()
	pubURL := b.cfg.PublicURL
	office := b.cfg.BboxOffice
	handle := b.cfg.Handle
	isHub := b.cfg.IsHub
	packs := append([]bstack.PackRef(nil), b.packs...)
	bountyDoms := append([]string(nil), b.bountyDoms...)
	b.mu.RUnlock()

	return b.disc.PublishAnnounce(ctx, bstack.AnnounceAd{
		Handle:        handle,
		PublicURL:     pubURL,
		BboxOffice:    office,
		Domains:       sellDoms,
		SellDomains:   sellDoms,
		BountyDomains: bountyDoms,
		Packs:         packs,
		IsHub:         isHub,
	})
}

// autoQuoteWant inspects an incoming verified WantAd and automatically quotes our best matching anchored fragment.
func (b *Broker) autoQuoteWant(ctx context.Context, want bstack.WantAd) {
	_ = SyncPolicyFromDB(b.mem.DB(), b.engine.Policy())
	kill, _, _, _, _, _, sellerOK := b.engine.Policy().Snapshot()
	if kill {
		return
	}
	// Hub makers or explicitly approved sellers quote automatically.
	if !b.cfg.IsHub && !sellerOK {
		return
	}
	listings, err := b.mem.DB().ListSellListings()
	if err != nil || len(listings) == 0 {
		return
	}
	wantDoms := make(map[string]bool, len(want.Domains))
	for _, d := range want.Domains {
		wantDoms[strings.ToLower(strings.TrimSpace(d))] = true
	}

	var matched *store.SellListing
	for i := range listings {
		l := listings[i]
		if !l.Active {
			continue
		}
		if want.MaxSats > 0 && l.PriceSats > want.MaxSats {
			continue
		}
		for _, ld := range l.Domains {
			if ld == "*" || wantDoms[strings.ToLower(ld)] {
				cp := l
				matched = &cp
				break
			}
		}
		if matched != nil {
			break
		}
	}
	if matched == nil {
		return
	}

	// Cap active pendingSales (total + per-buyer) to prevent WantAd flood RAM/CHIRP/workflow exhaustion.
	nowCheck := time.Now().Unix()
	b.mu.Lock()
	buyerActive := 0
	for id, old := range b.pendingSales {
		if old.ExpiresAt > 0 && old.ExpiresAt < nowCheck {
			delete(b.pendingSales, id)
			continue
		}
		if old.BuyerPub == want.Pub {
			buyerActive++
		}
	}
	overCap := len(b.pendingSales) >= maxPendingSalesTotal || buyerActive >= maxPendingSalesPerBuyer
	b.mu.Unlock()
	if overCap {
		return
	}

	// Only ever consider Level 0, non-Borg, Cedar-"disclose" fragments for automated market quoting.
	frags, err := b.mem.Recall(ctx, memory.RecallInput{
		Shard:        matched.Shard,
		Principal:    policy.Principal{Type: "Agent", ID: "market-seller", Level: 0},
		Action:       "disclose",
		DisallowBorg: true,
	})
	if err != nil || len(frags) == 0 {
		return
	}
	best := pickBestFragment(frags, want.Query)
	if best == nil {
		return
	}
	// Ensure fragment has an anchor & SPV ProofBundle before quoting!
	if best.AnchorTxID == "" && b.mem.Chain().BalanceSats() > 1000 {
		_, _ = b.mem.AnchorShard(ctx, matched.Shard)
	}
	pb, _, _ := b.mem.Prove(ctx, matched.Shard, best.Seq)

	saleOffer, revealKey, chirpURL, err := b.engine.PrepareDirectSale(ctx, matched.Shard, best.Seq, matched.PriceSats, want.Pub)
	if err != nil {
		return
	}
	// Keep OfferAd wire payload compact under MaxPayload (16KB): strip ciphertext if > 4KB (buyer fetches via CHIRP).
	wireSale := *saleOffer
	if len(wireSale.CiphertextHex) > 8192 {
		wireSale.CiphertextHex = ""
	}
	saleJSON, _ := json.Marshal(wireSale)
	payAddr, _ := b.mem.Chain().FundAddress()
	offerID := fmt.Sprintf("off-%s-%s", shortPub(b.PubHex()), randIDHex())

	// Build and verify OfferAd fits within post-CBOR bstack.MaxPayload BEFORE persisting pendingSale!
	b.mu.RLock()
	pubURL := b.cfg.PublicURL
	office := b.cfg.BboxOffice
	isHub := b.cfg.IsHub
	b.mu.RUnlock()

	offer := bstack.OfferAd{
		OfferID:        offerID,
		WantID:         want.WantID,
		Pub:            b.PubHex(),
		BuyerPub:       want.Pub,
		Net:            b.mem.Chain().Network(),
		Domains:        want.Domains,
		PriceSats:      matched.PriceSats,
		Shard:          matched.Shard,
		Leaf:           best.Leaf,
		ChirpURL:       chirpURL,
		KeyCommitHex:   saleOffer.KeyCommitment,
		PayAddress:     payAddr,
		PayIdentityKey: b.mem.Chain().IdentityPubHex(),
		SellerURL:      pubURL,
		BboxOffice:     office,
		SaleOffer:      saleJSON,
		IsHub:          isHub,
		Direct:         pubURL != "",
		Revision:       1,
		Exp:            time.Now().Add(30 * time.Minute).UnixMilli(),
	}
	var beef []byte
	for attempt := 0; attempt < 2; attempt++ {
		sig, sErr := bstack.SignArtifact(b.engine.saleKey, offer)
		if sErr != nil {
			return
		}
		body, mErr := json.Marshal(bstack.SignedOffer{Offer: offer, Sig: sig})
		if mErr != nil {
			return
		}
		payload := bstack.MarketPayload{Kind: bstack.KindOffer, Body: body}
		if encoded, eErr := bstack.EncodePayload(payload); eErr == nil && len(encoded) <= bstack.MaxPayload {
			beef, err = discovery.BuildMarketBEEF(payload)
			break
		}
		// Strip inline ciphertext on retry so buyer fetches via CHIRP and post-CBOR stays <= MaxPayload!
		if attempt == 0 && wireSale.CiphertextHex != "" {
			wireSale.CiphertextHex = ""
			saleJSON, _ = json.Marshal(wireSale)
			offer.SaleOffer = saleJSON
			continue
		}
		return
	}
	if err != nil || len(beef) == 0 {
		return
	}

	ps := &pendingSale{
		OfferID:     offerID,
		WantID:      want.WantID,
		BuyerPub:    want.Pub,
		Shard:       matched.Shard,
		Seq:         best.Seq,
		SaleOffer:   saleOffer,
		RevealKey:   revealKey,
		ChirpURL:    chirpURL,
		ProofBundle: pb,
		ExpiresAt:   int64(saleOffer.ExpiresAt),
	}
	b.mu.Lock()
	b.pendingSales[offerID] = ps
	nowUnix := time.Now().Unix()
	for id, old := range b.pendingSales {
		if old.ExpiresAt > 0 && old.ExpiresAt < nowUnix {
			core.Zero32Array(&old.RevealKey)
			if old.SaleOffer != nil {
				old.SaleOffer.ClearContentKey()
			}
			delete(b.pendingSales, id)
		}
	}
	b.mu.Unlock()
	fullSaleJSON, _ := json.Marshal(saleOffer)
	var pbJSON []byte
	if pb != nil {
		pbJSON, _ = json.Marshal(pb)
	}
	_ = b.mem.DB().SavePendingSale(store.PendingSaleRecord{
		OfferID:         offerID,
		WantID:          want.WantID,
		BuyerPub:        want.Pub,
		Shard:           matched.Shard,
		Seq:             best.Seq,
		SaleOfferJSON:   fullSaleJSON,
		RevealKeyHex:    b.sealKeyAtRest(ctx, hex.EncodeToString(revealKey[:])),
		ChirpURL:        chirpURL,
		ProofBundleJSON: pbJSON,
		ExpiresAt:       int64(saleOffer.ExpiresAt),
	})

	topics := lanesForDomains(offer.Net, want.Domains)
	for _, h := range b.disc.ActiveHosts() {
		_ = b.disc.SubmitBEEF(ctx, h, beef, topics)
	}
	// Start durable Restate SellerDeal workflow asynchronously (/send — never blocks SSE consumer!).
	if rc := b.RestateClient(); rc != nil {
		go func(oid string, w bstack.WantAd, sh string, sq int64, price uint64, bpub string, so *contracts.KeyedSaleOffer, rk [32]byte, cu string, bundle *contracts.ProofBundle) {
			sCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = rc.StartSellerDealAsync(sCtx, oid, SellerDealInput{
				Want:            w,
				Shard:           sh,
				Seq:             sq,
				PriceSats:       price,
				BuyerPub:        bpub,
				PreCreatedOffer: so,
				RevealKeyHex:    hex.EncodeToString(rk[:]),
				ChirpURL:        cu,
				ProofBundle:     bundle,
			})
		}(offerID, want, matched.Shard, best.Seq, matched.PriceSats, want.Pub, saleOffer, revealKey, chirpURL, pb)
	}
}

// HandlePurchase validates a buyer's AtomicBEEF payment + BRC-105 replay claim and reveals the BRC-369 content key k + SPV ProofBundle.
// Called by BOTH direct HTTP POST /v1/market/purchase AND NAT bbox inbox delivery!
func (b *Broker) HandlePurchase(ctx context.Context, req PurchaseRequest) (*PurchaseResponse, error) {
	if req.OfferID == "" || req.PaymentBEEFHex == "" {
		return nil, errors.New("market: offerId and paymentBeefHex are required")
	}
	nowUnix := time.Now().Unix()
	b.mu.Lock()
	sale, ok := b.pendingSales[req.OfferID]
	if ok && sale.ExpiresAt > 0 && sale.ExpiresAt < nowUnix {
		delete(b.pendingSales, req.OfferID)
		ok = false
		sale = nil
	}
	b.mu.Unlock()
	if !ok {
		if rec, dErr := b.mem.DB().GetPendingSale(req.OfferID); dErr == nil {
			if restored := b.pendingSaleFromRecord(ctx, rec); restored != nil && (restored.ExpiresAt == 0 || restored.ExpiresAt >= nowUnix) {
				sale = restored
				ok = true
				b.mu.Lock()
				b.pendingSales[req.OfferID] = sale
				b.mu.Unlock()
			}
		}
	}
	if !ok {
		return nil, fmt.Errorf("market: unknown or expired offerId %q", req.OfferID)
	}
	if sale.BuyerPub != "" {
		if strings.TrimSpace(req.BuyerPub) == "" || sale.BuyerPub != req.BuyerPub {
			return nil, errors.New("market: buyerPub does not match offer recipient")
		}
	}
	if strings.TrimSpace(req.BuyerPub) == "" || strings.TrimSpace(req.BuyerSig) == "" {
		return nil, errors.New("market: signed buyer purchase request (buyerPub + buyerSig) is required")
	}
	if req.TS < nowUnix-300 || req.TS > nowUnix+300 {
		return nil, errors.New("market: expired or missing purchase request timestamp")
	}
	if err := bstack.VerifyArtifact(req.BuyerPub, req.BuyerSig, req.Signable()); err != nil {
		return nil, fmt.Errorf("market: invalid buyer purchase signature: %w", err)
	}
	beefBytes, err := hex.DecodeString(strings.TrimSpace(req.PaymentBEEFHex))
	if err != nil {
		return nil, fmt.Errorf("market: decode paymentBeefHex: %w", err)
	}
	_, tx, txHash, err := guard.ParseBEEF(beefBytes, guard.DefaultBound)
	if err != nil || tx == nil || txHash == nil {
		if err == nil {
			err = errors.New("empty payment transaction")
		}
		return nil, fmt.Errorf("market: parse payment AtomicBEEF: %w", err)
	}
	txid := txHash.String()
	if req.TxID != "" && req.TxID != txid {
		return nil, errors.New("market: payment txid mismatch vs AtomicBEEF")
	}

	// Idempotent key re-reveal on buyer retry for the EXACT same payment txid!
	if existing, gErr := b.mem.DB().GetDeal("sell-" + sale.OfferID); gErr == nil && existing.Status == "settled" {
		if existing.PaymentTxID == txid {
			keyHex := b.openKeyAtRest(ctx, existing.ClaimKeyHex)
			if keyHex == "" {
				keyHex = hex.EncodeToString(sale.RevealKey[:])
			}
			return &PurchaseResponse{
				OK:            true,
				OfferID:       sale.OfferID,
				SellerPub:     b.PubHex(),
				ContentKeyHex: keyHex,
				CiphertextHex: sale.SaleOffer.CiphertextHex,
				ChirpURL:      sale.ChirpURL,
				ProofBundle:   sale.ProofBundle,
			}, nil
		}
		return nil, errors.New("market: offer already settled with a different payment txid")
	}

	// Verify payment transaction pays >= Offer.Price via BRC-29 derived key (strictly required on live networks
	// to prevent replay of publicly visible historical mined deposits to static FundAddress()).
	isBRC29 := req.DerivationPrefix != "" && req.DerivationSuffix != "" && req.SenderIdentityKey != ""
	if !isBRC29 && b.mem.Chain().Network() != "memory" {
		return nil, errors.New("market: BRC-29 payment remittance (derivationPrefix, derivationSuffix, senderIdentityKey) is required on live networks")
	}
	var paidSats uint64
	if isBRC29 {
		d := &bwallet.Derivation{
			SecurityLevel:   int(bwallet.PaymentProtocol.SecurityLevel),
			Protocol:        bwallet.PaymentProtocol.Protocol,
			KeyID:           bwallet.PaymentKeyID(req.DerivationPrefix, req.DerivationSuffix),
			CounterpartyHex: req.SenderIdentityKey,
			OwnerHex:        b.mem.Chain().IdentityPubHex(),
		}
		wantScript, dErr := b.mem.Chain().Signer().DerivedScript(ctx, d)
		if dErr != nil {
			return nil, fmt.Errorf("market: derive BRC-29 script: %w", dErr)
		}
		if uint64(req.OutputIndex) >= uint64(len(tx.Outputs)) {
			return nil, fmt.Errorf("market: BRC-29 outputIndex %d out of range", req.OutputIndex)
		}
		out := tx.Outputs[req.OutputIndex]
		if out.LockingScript == nil || !bytes.Equal(*out.LockingScript, *wantScript) {
			return nil, errors.New("market: payment output does not match BRC-29 derivation")
		}
		paidSats = out.Satoshis
	} else {
		fundScript, err := b.mem.Chain().Signer().FundScript()
		if err != nil {
			return nil, err
		}
		fundHex := fundScript.String()
		for _, out := range tx.Outputs {
			if out.LockingScript != nil && out.LockingScript.String() == fundHex {
				paidSats += out.Satoshis
			}
		}
	}
	if paidSats < sale.SaleOffer.Config.Price {
		return nil, fmt.Errorf("market: insufficient payment (%d < %d sats)", paidSats, sale.SaleOffer.Config.Price)
	}

	// Verify MerklePath against header chain before treating payment as confirmed!
	confirmed := false
	if tx.MerklePath != nil {
		if b.mem.Chain().Network() == "memory" {
			confirmed = true
		} else if tracker := b.mem.Chain().Tracker(); tracker != nil {
			if root, rErr := tx.MerklePath.ComputeRoot(txHash); rErr == nil && root != nil {
				if valid, vErr := tracker.IsValidRootForHeight(ctx, root, tx.MerklePath.BlockHeight); vErr == nil && valid {
					confirmed = true
				}
			}
		}
	}

	// Enforce ZeroConfMaxSats policy fail-closed (0 = zero-conf disabled) when tx lacks verified confirmation!
	_, _, _, zeroConfMax, _, _, _ := b.engine.Policy().Snapshot()
	if !confirmed && (zeroConfMax == 0 || sale.SaleOffer.Config.Price > zeroConfMax) {
		return nil, fmt.Errorf("market: payment %d sats exceeds zeroConfMaxSats (%d) without verified confirmation proof", sale.SaleOffer.Config.Price, zeroConfMax)
	}

	// Atomic cross-interface SQLite payment claim registry (shared with HTTP 402 paywall)!
	inputs := make([]string, 0, len(tx.Inputs))
	for _, in := range tx.Inputs {
		if in.SourceTXID != nil {
			inputs = append(inputs, fmt.Sprintf("%s:%d", in.SourceTXID.String(), in.SourceTxOutIndex))
		}
	}
	if err := b.mem.DB().ClaimPaymentAtomic(txid, inputs); err != nil {
		return nil, fmt.Errorf("market: payment claim rejected (%w)", err)
	}

	// On live networks, broadcast unmined zero-conf buyer payment before releasing ContentKeyHex,
	// and credit Seller's wallet immediately & verify SPV against shared headers!
	if b.mem.Chain().Network() != "memory" {
		if !confirmed {
			if rErr := b.mem.Chain().RelayExternalTx(ctx, tx); rErr != nil {
				_ = b.mem.DB().ReleasePaymentClaim(txid)
				return nil, fmt.Errorf("market: broadcast zero-conf buyer payment failed: %w", rErr)
			}
		}
		if isBRC29 {
			if _, iErr := b.mem.Chain().InternalizeBRC29(
				ctx, beefBytes, req.OutputIndex,
				req.DerivationPrefix, req.DerivationSuffix, req.SenderIdentityKey,
				"market sale "+sale.OfferID, []string{"market", "sale"},
			); iErr != nil {
				_ = b.mem.DB().ReleasePaymentClaim(txid)
				return nil, fmt.Errorf("market: internalize BRC-29 payment: %w", iErr)
			}
		} else {
			if _, aErr := b.mem.Chain().AdoptReceived(ctx, tx); aErr != nil {
				_ = b.mem.DB().ReleasePaymentClaim(txid)
				return nil, fmt.Errorf("market: adopt received payment: %w", aErr)
			}
		}
	}

	// Mark settled & persist durable Seller DealRecord!
	b.mu.Lock()
	sale.Settled = true
	b.mu.Unlock()
	_ = b.mem.DB().MarkPendingSaleSettled(sale.OfferID)
	_ = b.mem.DB().SaveDeal(store.DealRecord{
		DealID:          "sell-" + sale.OfferID,
		Role:            "seller",
		Status:          "settled",
		Shard:           sale.Shard,
		PaymentTxID:     txid,
		PaymentBEEF:     beefBytes,
		ClaimKeyHex:     b.sealKeyAtRest(ctx, hex.EncodeToString(sale.RevealKey[:])),
		ChirpURL:        sale.ChirpURL,
		CounterpartyPub: req.BuyerPub,
		PriceSats:       sale.SaleOffer.Config.Price,
		IsHubSeller:     b.cfg.IsHub,
	})
	b.notify("deal_settled", map[string]any{"role": "seller", "offerId": sale.OfferID, "txid": txid, "sats": sale.SaleOffer.Config.Price})

	// Notify active Restate SellerDeal workflow + Tx lifecycle journal asynchronously!
	if rc := b.RestateClient(); rc != nil {
		go func(oid, tid, sh string, sats uint64, conf bool) {
			sCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = rc.DeliverSellerPayment(sCtx, oid, PaymentSignal{TxID: tid, AmountSats: sats, Confirmed: conf})
			_, _ = rc.EmitTxEvent(sCtx, tid, TxEventInput{Kind: "broadcast", Shard: sh, Detail: "seller-sale:" + oid})
		}(sale.OfferID, txid, sale.Shard, paidSats, confirmed)
	}

	return &PurchaseResponse{
		OK:            true,
		OfferID:       sale.OfferID,
		SellerPub:     b.PubHex(),
		ContentKeyHex: hex.EncodeToString(sale.RevealKey[:]),
		CiphertextHex: sale.SaleOffer.CiphertextHex,
		ChirpURL:      sale.ChirpURL,
		ProofBundle:   sale.ProofBundle,
	}, nil
}

// CollectOffersForWant publishes a signed WantAd across active hubs and collects cryptographically verified OfferAds.
// Called by both the Restate BuyerDeal workflow (inside restate.Run) and direct local unit tests.
func (b *Broker) CollectOffersForWant(ctx context.Context, want bstack.WantAd, collectTimeout time.Duration) ([]bstack.OfferAd, int64, error) {
	hosts := b.disc.ActiveHosts()
	if len(hosts) == 0 {
		return nil, 0, errors.New("market: no active hub hosts configured (join a hub first)")
	}
	wantID := want.WantID
	inbox := make(chan bstack.OfferAd, 32)
	b.mu.Lock()
	b.offerInbox[wantID] = inbox
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.offerInbox, wantID)
		b.mu.Unlock()
	}()

	sig, err := bstack.SignArtifact(b.engine.saleKey, want)
	if err != nil {
		return nil, 0, err
	}
	body, err := json.Marshal(bstack.SignedWant{Want: want, Sig: sig})
	if err != nil {
		return nil, 0, err
	}
	beef, err := discovery.BuildMarketBEEF(bstack.MarketPayload{Kind: bstack.KindWant, Body: body})
	if err != nil {
		return nil, 0, err
	}
	startTime := time.Now()
	topics := lanesForDomains(want.Net, want.Domains)
	submitted := 0
	for _, h := range hosts {
		if err := b.disc.SubmitBEEF(ctx, h, beef, topics); err == nil {
			submitted++
		}
	}
	if submitted == 0 {
		return nil, 0, errors.New("failed to submit WantAd to any active hub")
	}

	timeout := collectTimeout
	if timeout <= 0 {
		timeout = b.cfg.CollectTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	lookupTick := time.NewTimer(300 * time.Millisecond)
	defer lookupTick.Stop()

	seenOffers := make(map[string]bstack.OfferAd)
	var firstOfferMs int64
	collectOffers := func(o bstack.OfferAd) {
		if o.WantID != wantID {
			return
		}
		if _, dup := seenOffers[o.OfferID]; dup {
			return
		}
		var so contracts.KeyedSaleOffer
		if err := json.Unmarshal(o.SaleOffer, &so); err != nil {
			return
		}
		if err := contracts.VerifySaleOffer(&so, b.PubHex(), o.Leaf); err != nil {
			return
		}
		if !strings.EqualFold(so.SellerPub, o.Pub) || so.Config.Price != o.PriceSats || !strings.EqualFold(so.KeyCommitment, o.KeyCommitHex) {
			return
		}
		if firstOfferMs == 0 {
			firstOfferMs = time.Since(startTime).Milliseconds()
			if firstOfferMs == 0 {
				firstOfferMs = 1
			}
		}
		seenOffers[o.OfferID] = o
	}

	var graceTimer *time.Timer
	var graceCh <-chan time.Time
collectLoop:
	for {
		select {
		case <-ctx.Done():
			break collectLoop
		case o := <-inbox:
			collectOffers(o)
			if len(seenOffers) > 0 && graceTimer == nil {
				graceTimer = time.NewTimer(200 * time.Millisecond)
				graceCh = graceTimer.C
			}
		case <-lookupTick.C:
			for _, o := range b.lookupOffers(ctx, hosts, wantID) {
				collectOffers(o)
			}
			if len(seenOffers) > 0 && graceTimer == nil {
				graceTimer = time.NewTimer(150 * time.Millisecond)
				graceCh = graceTimer.C
			}
		case <-graceCh:
			break collectLoop
		case <-timer.C:
			for _, o := range b.lookupOffers(ctx, hosts, wantID) {
				collectOffers(o)
			}
			break collectLoop
		}
	}
	if graceTimer != nil {
		graceTimer.Stop()
	}

	offers := make([]bstack.OfferAd, 0, len(seenOffers))
	for _, o := range seenOffers {
		offers = append(offers, o)
	}
	return offers, firstOfferMs, nil
}

// DeliverPurchaseAsync delivers a buyer's AtomicBEEF payment to Seller.
// Direct HTTP returns the seller's immediate PurchaseResponse; NAT bbox sends purchase_beef
// and returns (nil, nil) immediately so Restate BuyerDeal suspends durably on Promise("claim")!
func (b *Broker) DeliverPurchaseAsync(ctx context.Context, chosen bstack.OfferAd, dealID string, pReq PurchaseRequest) (*PurchaseResponse, error) {
	var directErr error
	if chosen.SellerURL != "" {
		pResp, err := b.sendDirectPurchase(ctx, chosen.SellerURL, pReq)
		if err == nil && pResp != nil && pResp.OK {
			return pResp, nil
		}
		directErr = err
		if pResp != nil && !pResp.OK && pResp.Error != "" && chosen.BboxOffice == "" {
			return pResp, errors.New(pResp.Error)
		}
	}
	if chosen.BboxOffice != "" {
		hosts := b.disc.ActiveHosts()
		b.mu.Lock()
		b.bboxExpected[pReq.OfferID] = chosen.Pub
		b.bboxWantByOffer[pReq.OfferID] = dealID
		b.mu.Unlock()
		bodyRaw, _ := json.Marshal(pReq)
		aad := bstack.BboxAAD(chosen.BboxOffice, b.PubHex(), chosen.Pub, dealID, "purchase_beef")
		sealedBody, encErr := bstack.SealBboxBody(chosen.Pub, aad, bodyRaw)
		if encErr != nil {
			return nil, encErr
		}
		sent := false
		for _, h := range hosts {
			msg := hub.BboxMessage{
				Office:       chosen.BboxOffice,
				RecipientPub: chosen.Pub,
				SenderPub:    b.PubHex(),
				DealID:       dealID,
				Kind:         "purchase_beef",
				TS:           time.Now().Unix(),
				Nonce:        randNonceHex(),
				Aud:          hubAudience(h),
				Body:         sealedBody,
			}
			sig, sErr := bstack.SignArtifact(b.engine.saleKey, msg.Signable())
			if sErr != nil {
				return nil, sErr
			}
			msg.Sig = sig
			env, _ := json.Marshal(msg)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(h, "/")+"/bbox/send", bytes.NewReader(env))
			if err != nil {
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := b.httpClient.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode < 300 {
					sent = true
					break
				}
			}
		}
		if sent {
			return nil, nil
		}
		if directErr != nil {
			return nil, directErr
		}
		return nil, errors.New("failed to deliver purchase via direct HTTP or hub bbox")
	}
	if directErr != nil {
		return nil, directErr
	}
	return nil, errors.New("seller advertised neither SellerURL nor BboxOffice")
}

// Want publishes a signed WantAd across active hubs, collects & ranks verified offers,
// executes AtomicBEEF payment (direct HTTP or NAT bbox fallback), verifies the revealed
// BRC-369 content key + remote CHIRP closure + SPV Merkle ProofBundle, and commits the knowledge!
// When an active RestateClient is attached (default in `nektar serve`), execution runs durably
// through the Restate BuyerDeal workflow + Custody VO + Tx VO (zero local-broker bypass!).
func (b *Broker) Want(ctx context.Context, req WantRequest) (*store.DealRecord, error) {
	if len(req.Domains) == 0 {
		req.Domains = []string{"ops"}
	}
	if strings.TrimSpace(req.Question) == "" {
		return nil, errors.New("market: question is required")
	}
	if req.MaxSats == 0 {
		req.MaxSats = 500
	}
	hosts := b.disc.ActiveHosts()
	if len(hosts) == 0 {
		return nil, errors.New("market: no active hub hosts configured (join a hub first)")
	}

	wantID := fmt.Sprintf("want-%s-%s", shortPub(b.PubHex()), randIDHex())
	want := bstack.WantAd{
		WantID:   wantID,
		Pub:      b.PubHex(),
		Net:      b.mem.Chain().Network(),
		Domains:  req.Domains,
		Query:    req.Question,
		MaxSats:  req.MaxSats,
		Revision: 1,
		Exp:      time.Now().Add(30 * time.Minute).UnixMilli(),
	}

	// Active Restate Durable Executor path (SOLE production path on live networks — NO local-broker bypass!).
	rc := b.RestateClient()
	if rc == nil && b.mem.Chain().Network() != "memory" {
		return nil, errors.New("market: Restate durable executor is required for live trades (run `nektar restate-up` or configure NEKTAR_RESTATE_ADMIN)")
	}
	if rc != nil {
		if _, err := rc.StartBuyerDealAsync(ctx, wantID, BuyerDealInput{
			Want:               want,
			TargetShard:        req.TargetShard,
			CollectTimeout:     req.CollectTimeout,
			WaitTimeout:        5 * time.Minute,
			ApprovedOverrideID: req.ApprovedOverrideID,
		}); err != nil {
			return nil, err
		}
		// Race streaming AttachBuyerDeal (Timeout: 0) against approval_pending wake (eliminates 80ms spin-poll!).
		attachCtx, attachCancel := context.WithCancel(ctx)
		defer attachCancel()
		type attachRes struct {
			st  *DealState
			err error
		}
		attachCh := make(chan attachRes, 1)
		go func() {
			st, aErr := rc.AttachBuyerDeal(attachCtx, wantID)
			attachCh <- attachRes{st: st, err: aErr}
		}()
		checkTicker := time.NewTicker(250 * time.Millisecond)
		defer checkTicker.Stop()
		for {
			select {
			case ar := <-attachCh:
				if rec, gErr := b.mem.DB().GetDeal(wantID); gErr == nil {
					return &rec, nil
				}
				if ar.err != nil {
					return nil, ar.err
				}
				return &store.DealRecord{
					DealID:        wantID,
					Role:          "buyer",
					Status:        ar.st.Status,
					Shard:         ar.st.Shard,
					PaymentTxID:   ar.st.PaymentTxID,
					DeliveredText: ar.st.DeliveredTxt,
					Error:         ar.st.Error,
				}, nil
			case <-checkTicker.C:
				if rec, gErr := b.mem.DB().GetDeal(wantID); gErr == nil && rec.Status == "approval_pending" {
					// Workflow is durably suspended on its Restate Awakeable; return immediately to caller!
					return &rec, nil
				}
			case <-ctx.Done():
				if rec, gErr := b.mem.DB().GetDeal(wantID); gErr == nil {
					return &rec, ctx.Err()
				}
				return nil, ctx.Err()
			}
		}
	}

	_ = SyncPolicyFromDB(b.mem.DB(), b.engine.Policy())
	kill, maxDeal, maxDay, _, spent, buyerOK, _ := b.engine.Policy().Snapshot()
	if kill {
		return nil, errors.New("market: operator kill switch is engaged")
	}
	if req.ApprovedOverrideID != "" {
		if err := b.mem.DB().ConsumeApprovalOverride(req.ApprovedOverrideID); err != nil {
			return nil, fmt.Errorf("market: invalid or consumed approval override %q: %w", req.ApprovedOverrideID, err)
		}
	}
	wantJSON, _ := json.Marshal(want)

	// Load-bearing operator consent gate: require active buyer policy, allowed domain, & caps unless explicitly approved!
	domainErr := b.engine.Policy().CanSpendForDomains("", req.Domains, 0)
	if req.ApprovedOverrideID == "" && (!buyerOK || domainErr != nil || (maxDeal > 0 && req.MaxSats > maxDeal) || (maxDay > 0 && spent+req.MaxSats > maxDay)) {
		reason := "no active buyer policy approved (run `nektar policy approve --preset buyer` or approve in Dashboard)"
		if buyerOK && domainErr != nil {
			reason = domainErr.Error()
		} else if buyerOK && maxDeal > 0 && req.MaxSats > maxDeal {
			reason = fmt.Sprintf("requested maxSats (%d) exceeds buyer policy maxPerDeal (%d)", req.MaxSats, maxDeal)
		} else if buyerOK && maxDay > 0 && spent+req.MaxSats > maxDay {
			var rem uint64
			if maxDay > spent {
				rem = maxDay - spent
			}
			reason = fmt.Sprintf("requested maxSats (%d) exceeds remaining daily cap (%d)", req.MaxSats, rem)
		}
		appID := "appr-" + randIDHex()
		_ = b.mem.DB().SaveApproval(store.ApprovalRecord{
			ID:           appID,
			DealID:       wantID,
			Reason:       reason,
			MaxExtraSats: req.MaxSats,
			Status:       "pending",
			ExpiresAt:    time.Now().Add(15 * time.Minute).Unix(),
		})
		rec := store.DealRecord{
			DealID:    wantID,
			Role:      "buyer",
			Status:    "approval_pending",
			WantJSON:  wantJSON,
			PriceSats: req.MaxSats,
			Error:     reason + " [approvalId=" + appID + "]",
		}
		_ = b.mem.DB().SaveDeal(rec)
		b.notify("approval_pending", map[string]any{"approvalId": appID, "dealId": wantID, "reason": reason})
		return &rec, nil
	}

	// Register offer inbox BEFORE publishing WantAd so zero fast quotes are missed!
	inbox := make(chan bstack.OfferAd, 32)
	b.mu.Lock()
	b.offerInbox[wantID] = inbox
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.offerInbox, wantID)
		b.mu.Unlock()
	}()

	rec := store.DealRecord{
		DealID:   wantID,
		Role:     "buyer",
		Status:   "collecting",
		WantJSON: wantJSON,
	}
	_ = b.mem.DB().SaveDeal(rec)

	sig, err := bstack.SignArtifact(b.engine.saleKey, want)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(bstack.SignedWant{Want: want, Sig: sig})
	if err != nil {
		return nil, err
	}
	beef, err := discovery.BuildMarketBEEF(bstack.MarketPayload{Kind: bstack.KindWant, Body: body})
	if err != nil {
		return nil, err
	}
	startTime := time.Now()
	topics := lanesForDomains(want.Net, want.Domains)
	submitted := 0
	for _, h := range hosts {
		if err := b.disc.SubmitBEEF(ctx, h, beef, topics); err == nil {
			submitted++
		}
	}
	if submitted == 0 {
		rec.Status = "failed"
		rec.Error = "failed to submit WantAd to any active hub"
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, errors.New(rec.Error)
	}

	// Collect live SSE offers + one fallback /lookup poll after 300ms.
	timeout := req.CollectTimeout
	if timeout <= 0 {
		timeout = b.cfg.CollectTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	lookupTick := time.NewTimer(300 * time.Millisecond)
	defer lookupTick.Stop()

	seenOffers := make(map[string]bstack.OfferAd)
	var firstOfferMs int64
	collectOffers := func(o bstack.OfferAd) {
		if o.WantID != wantID {
			return
		}
		if _, dup := seenOffers[o.OfferID]; dup {
			return
		}
		// Cryptographically verify embedded DisclosureSale offer BEFORE ranking!
		var so contracts.KeyedSaleOffer
		if err := json.Unmarshal(o.SaleOffer, &so); err != nil {
			return
		}
		if err := contracts.VerifySaleOffer(&so, b.PubHex(), o.Leaf); err != nil {
			return
		}
		if !strings.EqualFold(so.SellerPub, o.Pub) || so.Config.Price != o.PriceSats || !strings.EqualFold(so.KeyCommitment, o.KeyCommitHex) {
			return
		}
		if firstOfferMs == 0 {
			firstOfferMs = time.Since(startTime).Milliseconds()
			if firstOfferMs == 0 {
				firstOfferMs = 1
			}
		}
		seenOffers[o.OfferID] = o
	}

	// Wait up to timeout; if we receive an organic offer AND wait at least 200ms for competition, proceed swiftly!
	var graceTimer *time.Timer
	var graceCh <-chan time.Time
collectLoop:
	for {
		select {
		case <-ctx.Done():
			break collectLoop
		case o := <-inbox:
			collectOffers(o)
			if len(seenOffers) > 0 && graceTimer == nil {
				graceTimer = time.NewTimer(200 * time.Millisecond)
				graceCh = graceTimer.C
			}
		case <-lookupTick.C:
			for _, o := range b.lookupOffers(ctx, hosts, wantID) {
				collectOffers(o)
			}
			if len(seenOffers) > 0 && graceTimer == nil {
				graceTimer = time.NewTimer(150 * time.Millisecond)
				graceCh = graceTimer.C
			}
		case <-graceCh:
			break collectLoop
		case <-timer.C:
			for _, o := range b.lookupOffers(ctx, hosts, wantID) {
				collectOffers(o)
			}
			break collectLoop
		}
	}
	if graceTimer != nil {
		graceTimer.Stop()
	}

	if len(seenOffers) == 0 {
		_ = b.mem.DB().RecordUnmatchedWant(store.UnmatchedWantRecord{
			WantID:  wantID,
			Domain:  req.Domains[0],
			Query:   req.Question,
			MaxSats: req.MaxSats,
		})
		rec.Status = "expired"
		rec.Error = fmt.Sprintf("no valid offers received within %s for domain %q — try `nektar peers` to see active sellers/bounties or widen domains/maxSats", b.cfg.CollectTimeout.Round(time.Millisecond), req.Domains[0])
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, nil
	}

	offers := make([]bstack.OfferAd, 0, len(seenOffers))
	for _, o := range seenOffers {
		offers = append(offers, o)
	}
	// Load verified local seller reputations & rank deterministically in integer bps!
	repList, _ := b.mem.DB().ListReputations()
	reps := make(map[string]store.SellerReputation, len(repList))
	for _, r := range repList {
		reps[r.SellerPub] = r
	}
	rankPol := b.engine.Policy()
	if req.ApprovedOverrideID != "" {
		rankPol = nil // explicitly approved override bypasses default cap check
	}
	ranked := RankOffersWithRep(want, offers, rankPol, reps)
	if len(ranked) == 0 {
		rec.Status = "failed"
		rec.Error = "all received offers exceeded policy caps"
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, nil
	}

	chosen := ranked[0].Offer
	chosenJSON, _ := json.Marshal(chosen)
	rec.ChosenOfferJSON = chosenJSON
	rec.CounterpartyPub = chosen.Pub
	rec.PriceSats = chosen.PriceSats
	rec.ChirpURL = chosen.ChirpURL
	rec.IsHubSeller = chosen.IsHub
	rec.FirstOfferMs = firstOfferMs
	rec.Status = "offered"
	_ = b.mem.DB().SaveDeal(rec)

	// Build, SpendGuard-check, and broadcast BSV payment (preferring BRC-29 derived key when Seller advertises PayIdentityKey).
	sendArgs := nwallet.SendArgs{
		Satoshis:    chosen.PriceSats,
		Description: "market purchase " + chosen.OfferID,
		Labels:      []string{"market", "purchase"},
	}
	if chosen.PayIdentityKey != "" {
		sendArgs.ToIdentityKey = chosen.PayIdentityKey
	} else {
		sendArgs.ToAddress = chosen.PayAddress
	}
	sendRes, err := b.mem.Chain().Send(ctx, sendArgs)
	if err != nil {
		rec.Status = "failed"
		rec.Error = "payment failed: " + err.Error()
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, err
	}
	payTxID := sendRes.TxID
	payBEEF := sendRes.BEEF
	if len(payBEEF) == 0 && sendRes.Tx != nil {
		payBEEF, err = chain.SafeAtomicBEEF(sendRes.Tx)
		if err != nil {
			rec.Status = "failed"
			rec.Error = "encode payment AtomicBEEF: " + err.Error()
			_ = b.mem.DB().SaveDeal(rec)
			return &rec, err
		}
	}
	b.engine.Policy().RecordSpend(chosen.PriceSats)
	rec.PaymentTxID = payTxID
	rec.PaymentBEEF = payBEEF
	rec.Status = "paid"
	_ = b.mem.DB().SaveDeal(rec)

	// Deliver AtomicBEEF (+ BRC-29 remittance when used) to Seller via Direct HTTP OR NAT bbox fallback!
	pReq := PurchaseRequest{
		OfferID:           chosen.OfferID,
		WantID:            wantID,
		BuyerPub:          b.PubHex(),
		TxID:              payTxID,
		PaymentBEEFHex:    hex.EncodeToString(payBEEF),
		OutputIndex:       sendRes.OutputIndex,
		DerivationPrefix:  sendRes.DerivationPrefix,
		DerivationSuffix:  sendRes.DerivationSuffix,
		SenderIdentityKey: sendRes.SenderIdentityKey,
	}
	b.signPurchaseRequest(&pReq)
	var pResp *PurchaseResponse
	if chosen.SellerURL != "" {
		pResp, err = b.sendDirectPurchase(ctx, chosen.SellerURL, pReq)
	}
	if pResp == nil && chosen.BboxOffice != "" {
		for _, h := range hosts {
			pResp, err = b.sendBboxPurchase(ctx, h, chosen.Pub, chosen.BboxOffice, wantID, pReq)
			if err == nil && pResp != nil && pResp.OK {
				break
			}
		}
	}
	if err != nil || pResp == nil || !pResp.OK {
		_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
		rec.Status = "failed"
		if err != nil {
			rec.Error = "purchase delivery failed: " + err.Error()
		} else if pResp != nil {
			rec.Error = "seller rejected purchase: " + pResp.Error
		}
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, errors.New(rec.Error)
	}

	// Zero-trust Buyer Verification:
	// 1. Decode signed SaleOffer
	var saleOffer contracts.KeyedSaleOffer
	if err := json.Unmarshal(chosen.SaleOffer, &saleOffer); err != nil {
		return nil, err
	}
	// 2. Fetch & cryptographically verify remote BRC-167 CHIRP ciphertext when SellerURL is reachable!
	if chosen.SellerURL != "" && chosen.ChirpURL != "" {
		ct, cErr := b.disc.FetchRemoteChirp(ctx, chosen.SellerURL, chosen.ChirpURL)
		if cErr != nil {
			_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
			rec.Status = "failed"
			rec.Error = "remote CHIRP verification failed: " + cErr.Error()
			_ = b.mem.DB().SaveDeal(rec)
			return &rec, cErr
		}
		ctHex := hex.EncodeToString(ct)
		if saleOffer.CiphertextHex != "" && ctHex != saleOffer.CiphertextHex {
			_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
			rec.Status = "failed"
			rec.Error = "CHIRP ciphertext mismatch vs signed offer"
			_ = b.mem.DB().SaveDeal(rec)
			return &rec, errors.New(rec.Error)
		}
		saleOffer.CiphertextHex = ctHex
	} else if saleOffer.CiphertextHex == "" && pResp.CiphertextHex != "" {
		saleOffer.CiphertextHex = pResp.CiphertextHex
	}

	// 3. Verify revealed BRC-369 content key k against DisclosureSale hWrapped AND KeyCommitment, then decrypt!
	keyBytes, err := hex.DecodeString(strings.TrimSpace(pResp.ContentKeyHex))
	if err != nil || len(keyBytes) != 32 {
		_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
		rec.Status = "failed"
		rec.Error = "seller returned invalid 32-byte content key"
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, errors.New(rec.Error)
	}
	plaintext, err := contracts.ClaimKeyedSale(&saleOffer, keyBytes)
	if err != nil {
		_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
		rec.Status = "failed"
		rec.Error = "BRC-369 key verification failed: " + err.Error()
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, err
	}

	// 4. Full offline SPV verification of seller's ProofBundle (AnchorBEEF + NektarAnchor + ChainTail + BUMP/headers + leaf match)!
	if pResp.ProofBundle == nil && chosen.Leaf != "" && b.mem.Chain().Network() != "memory" {
		_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
		rec.Status = "failed"
		rec.Error = "missing SPV ProofBundle from seller"
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, errors.New(rec.Error)
	}
	if pResp.ProofBundle != nil {
		if !strings.EqualFold(pResp.ProofBundle.Leaf, chosen.Leaf) {
			_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
			rec.Status = "failed"
			rec.Error = "seller SPV ProofBundle leaf mismatches signed offer leaf"
			_ = b.mem.DB().SaveDeal(rec)
			return &rec, errors.New(rec.Error)
		}
		vr := contracts.VerifyFragmentCtx(ctx, *pResp.ProofBundle, b.mem.Chain().Tracker())
		if !vr.OK && b.mem.Chain().Network() == "memory" && vr.Reason == "BEEFInvalid" && strings.Contains(vr.Detail, "BUMP root not valid in header chain") {
			vr = contracts.VerifyFragmentStructural(*pResp.ProofBundle)
		}
		if !vr.OK {
			_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
			rec.Status = "failed"
			rec.Error = fmt.Sprintf("seller SPV ProofBundle invalid (%s: %s)", vr.Reason, vr.Detail)
			_ = b.mem.DB().SaveDeal(rec)
			return &rec, errors.New(rec.Error)
		}
		if b.mem.Chain().Network() != "memory" && !vr.Settled {
			_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
			rec.Status = "failed"
			rec.Error = "seller SPV ProofBundle is not settled in a mined block"
			_ = b.mem.DB().SaveDeal(rec)
			return &rec, errors.New(rec.Error)
		}
	}

	// 5. Commit verified knowledge into Buyer's own memory shard & record verified Seller reputation!
	// Anchor synchronously in memory mode; on live testnet/main commit locally immediately so Want returns fast.
	targetShard := strings.TrimSpace(req.TargetShard)
	if targetShard == "" {
		targetShard = "market/" + req.Domains[0]
	}
	if _, err := b.mem.Remember(ctx, memory.RememberInput{
		Shard:      targetShard,
		Text:       string(plaintext),
		Source:     "market:" + shortPub(chosen.Pub),
		Class:      "market:" + req.Domains[0],
		Tainted:    true,
		AutoAnchor: b.mem.Chain().Network() == "memory" && b.mem.Chain().BalanceSats() > 1000,
	}); err != nil {
		rec.Status = "failed"
		rec.Error = "commit purchased fact: " + err.Error()
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, err
	}

	_ = b.mem.DB().RecordReputation(chosen.Pub, true, chosen.PriceSats)
	rec.Shard = targetShard
	rec.ClaimKeyHex = pResp.ContentKeyHex
	rec.DeliveredText = string(plaintext)
	rec.Status = "settled"
	rec.Error = ""
	_ = b.mem.DB().SaveDeal(rec)
	b.notify("deal_settled", map[string]any{"role": "buyer", "dealId": rec.DealID, "txid": payTxID, "sats": chosen.PriceSats, "shard": targetShard})
	return &rec, nil
}

// ApproveAndExecute settles a pending operator approval with atomic SQLite CAS.
// When the paused BuyerDeal is suspended on a Restate Awakeable (active Restate mode),
// it resolves that EXACT awakeable first, then atomically marks the approval consumed/denied
// so transient Restate errors remain retryable and approved awakeable IDs cannot be replayed.
func (b *Broker) ApproveAndExecute(ctx context.Context, approvalID string, approve bool) (store.ApprovalRecord, *store.DealRecord, error) {
	existing, err := b.mem.DB().GetApproval(approvalID)
	if err != nil {
		return store.ApprovalRecord{}, nil, err
	}
	// Active Restate Awakeable resume path (resumes the exact suspended BuyerDeal workflow!).
	if existing.AwakeableID != "" {
		if existing.Status != "pending" && existing.Status != "approved" {
			return existing, nil, store.ErrApprovalNotPending
		}
		if existing.ExpiresAt <= time.Now().Unix() {
			return existing, nil, store.ErrApprovalNotPending
		}
		rc := b.RestateClient()
		if rc == nil {
			return existing, nil, errors.New("market: Restate runtime unavailable to resolve awakeable approval")
		}
		dec := ApprovalDecision{Approved: approve, DecidedBy: "operator"}
		if !approve {
			dec.Reason = "denied by operator"
		}
		if rErr := rc.ResolveAwakeable(ctx, existing.AwakeableID, dec); rErr != nil {
			return existing, nil, rErr
		}
		appr, mErr := b.mem.DB().MarkApprovalResolvedAwakeable(approvalID, approve)
		if mErr != nil {
			// Awakeable was already delivered to Restate; tolerate concurrent state update.
			appr = existing
			if approve {
				appr.Status = "consumed"
			} else {
				appr.Status = "denied"
			}
		}
		aCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if st, aErr := rc.AttachBuyerDeal(aCtx, appr.DealID); aErr == nil && st != nil {
			if deal, dErr := b.mem.DB().GetDeal(appr.DealID); dErr == nil {
				return appr, &deal, nil
			}
			return appr, &store.DealRecord{
				DealID:        appr.DealID,
				Role:          "buyer",
				Status:        st.Status,
				Shard:         st.Shard,
				PaymentTxID:   st.PaymentTxID,
				DeliveredText: st.DeliveredTxt,
				Error:         st.Error,
			}, nil
		}
		if deal, dErr := b.mem.DB().GetDeal(appr.DealID); dErr == nil {
			return appr, &deal, nil
		}
		return appr, nil, nil
	}

	appr, err := b.mem.DB().DecideApproval(approvalID, approve)
	if err != nil {
		return store.ApprovalRecord{}, nil, err
	}
	if !approve {
		if deal, dErr := b.mem.DB().GetDeal(appr.DealID); dErr == nil {
			deal.Status = "failed"
			deal.Error = "denied by operator"
			_ = b.mem.DB().SaveDeal(deal)
			return appr, &deal, nil
		}
		return appr, nil, nil
	}
	if b.mem.Chain().Network() != "memory" {
		return appr, nil, errors.New("market: legacy non-Restate approval execution is disabled on live networks")
	}
	deal, err := b.mem.DB().GetDeal(appr.DealID)
	if err != nil {
		return appr, nil, nil
	}
	var want bstack.WantAd
	if json.Unmarshal(deal.WantJSON, &want) != nil {
		return appr, &deal, nil
	}
	settled, err := b.Want(ctx, WantRequest{
		Domains:            want.Domains,
		Question:           want.Query,
		MaxSats:            want.MaxSats,
		ApprovedOverrideID: approvalID,
	})
	return appr, settled, err
}

// SellToHub submits an SPV-anchored local fragment directly to a Hub's knowledge bounty pool and receives a real BSV payout!
func (b *Broker) SellToHub(ctx context.Context, hubURL, bountyID, shard string, seq int64) (*hub.BountyPayoutResult, error) {
	if b.engine != nil && b.engine.Policy() != nil {
		kill, _, _, _, _, _, sellerOK := b.engine.Policy().Snapshot()
		if kill {
			return nil, errors.New("market: operator kill switch is engaged")
		}
		if !sellerOK && b.mem.Chain().Network() != "memory" {
			return nil, errors.New("market: seller policy not approved (approve a seller policy before selling to Hub bounties)")
		}
	}
	if hubURL == "" {
		hosts := b.disc.ActiveHosts()
		if len(hosts) == 0 {
			return nil, errors.New("market: no active hub host configured")
		}
		hubURL = hosts[0]
	}
	netName := b.mem.Chain().Network()
	if _, err := core.ValidateOutboundURL(hubURL, core.SafeHTTPOpts{
		AllowPrivate: netName == "memory" || netName == "regtest" || netName == "",
		RequireHTTPS: netName == "main",
	}); err != nil {
		return nil, fmt.Errorf("market: blocked bounty hubURL: %w", err)
	}
	// Strictly Level 0 + Cedar "disclose" + non-Borg fragments may be sold externally.
	frags, err := b.mem.Recall(ctx, memory.RecallInput{
		Shard:        shard,
		Principal:    policy.Principal{Type: "Agent", ID: "market-seller", Level: 0},
		Action:       "disclose",
		DisallowBorg: true,
	})
	if err != nil {
		return nil, err
	}
	var target *memory.RecalledFragment
	for i := range frags {
		if seq <= 0 || frags[i].Seq == seq {
			cp := frags[i]
			target = &cp
			if seq > 0 {
				break
			}
		}
	}
	if target == nil {
		return nil, fmt.Errorf("market: fragment %s#%d not found or unauthorized for external disclosure", shard, seq)
	}
	if target.AnchorTxID == "" && b.mem.Chain().BalanceSats() > 1000 {
		_, _ = b.mem.AnchorShard(ctx, shard)
	}
	pb, _, _ := b.mem.Prove(ctx, shard, target.Seq)
	var pbRaw json.RawMessage
	if pb != nil {
		pbRaw, _ = json.Marshal(pb)
	}
	addr, err := b.mem.Chain().FundAddress()
	if err != nil {
		return nil, err
	}
	var provHex string
	if rec, gErr := b.mem.DB().GetFragment(shard, target.Seq); gErr == nil && len(rec.Provenance) > 0 {
		provHex = hex.EncodeToString(rec.Provenance)
	}
	provPub, err := b.mem.Chain().SigningPubKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("market: provenance signing pubkey: %w", err)
	}
	canonicalHDP := strings.ToLower(strings.TrimSpace(target.HDP))
	sub := hub.BountySubmission{
		BountyID:      bountyID,
		SellerPub:     b.PubHex(),
		ProvenancePub: hex.EncodeToString(provPub.Compressed()),
		SellerAddress: addr,
		Shard:         shard,
		Seq:           target.Seq,
		HDP:           canonicalHDP,
		Text:          target.Text,
		ProvenanceHex: provHex,
		ProofBundle:   pbRaw,
		TS:            time.Now().Unix(),
	}
	signable := sub.Signable(canonicalHDP)
	if sig, sErr := bstack.SignArtifact(b.engine.saleKey, signable); sErr == nil {
		sub.Sig = sig
	}
	if canon, cErr := core.CanonicalJSON(signable); cErr == nil {
		if ownerSig, oErr := b.mem.Chain().SignDigest(ctx, core.SHA256(canon)); oErr == nil {
			sub.OwnerSig = hex.EncodeToString(ownerSig)
		}
	}
	body, _ := json.Marshal(sub)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(hubURL, "/")+"/bounties/submit", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("hub bounty submit rejected (HTTP %d)", resp.StatusCode)
	}
	var out hub.BountyPayoutResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if out.Accepted {
		_ = b.mem.DB().SaveDeal(store.DealRecord{
			DealID:          "bounty-" + bountyID + "-" + shortPub(out.TxID),
			Role:            "seller",
			Status:          "settled",
			PaymentTxID:     out.TxID,
			DeliveredText:   target.Text,
			CounterpartyPub: "hub:" + hubURL,
			PriceSats:       out.RewardSats,
			IsHubSeller:     true,
		})
	}
	return &out, nil
}

// LiquidityMetrics computes the platform-strategy scorecard across persisted deals & active peers!
func (b *Broker) LiquidityMetrics() map[string]any {
	deals, _ := b.mem.DB().ListDeals(200)
	var totalBuyer, settledBuyer, zeroOffers, hubFills int
	var offerTimes []int64
	for _, d := range deals {
		if d.Role != "buyer" {
			continue
		}
		totalBuyer++
		if d.Status == "settled" {
			settledBuyer++
			if d.IsHubSeller {
				hubFills++
			}
		}
		if d.Status == "expired" && d.FirstOfferMs == 0 {
			zeroOffers++
		}
		if d.FirstOfferMs > 0 {
			offerTimes = append(offerTimes, d.FirstOfferMs)
		}
	}
	var fillRate, zeroRate, hubShare float64
	if totalBuyer > 0 {
		fillRate = float64(settledBuyer) / float64(totalBuyer)
		zeroRate = float64(zeroOffers) / float64(totalBuyer)
	}
	if settledBuyer > 0 {
		hubShare = float64(hubFills) / float64(settledBuyer)
	}
	var medianMs int64
	if len(offerTimes) > 0 {
		sort.Slice(offerTimes, func(i, j int) bool { return offerTimes[i] < offerTimes[j] })
		medianMs = offerTimes[len(offerTimes)/2]
	}
	peers := b.disc.Peers()
	organicSellers := 0
	for _, p := range peers {
		if !p.IsHub && len(p.SellDomains) > 0 {
			organicSellers++
		}
	}
	return map[string]any{
		"fillRate":            fillRate,
		"medianTimeToOfferMs": medianMs,
		"zeroRate":            zeroRate,
		"hubSupplyShare":      hubShare,
		"organicSellers":      organicSellers,
		"activePeers":         len(peers),
		"sseWakes":            b.sseWakes.Load(),
		"pollWakes":           b.pollWakes.Load(),
		"bboxWakes":           b.bboxWakes.Load(),
	}
}

func (b *Broker) sendDirectPurchase(ctx context.Context, sellerURL string, req PurchaseRequest) (*PurchaseResponse, error) {
	raw, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(sellerURL, "/")+"/v1/market/purchase", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out PurchaseResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 || !out.OK {
		if out.Error == "" {
			out.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return &out, errors.New(out.Error)
	}
	return &out, nil
}

func (b *Broker) sendBboxPurchase(ctx context.Context, hubURL, sellerPub, office, dealID string, pReq PurchaseRequest) (*PurchaseResponse, error) {
	ch := make(chan PurchaseResponse, 1)
	b.mu.Lock()
	b.bboxReplies[pReq.OfferID] = ch
	b.bboxExpected[pReq.OfferID] = sellerPub
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.bboxReplies, pReq.OfferID)
		delete(b.bboxExpected, pReq.OfferID)
		b.mu.Unlock()
	}()

	bodyRaw, _ := json.Marshal(pReq)
	aad := bstack.BboxAAD(office, b.PubHex(), sellerPub, dealID, "purchase_beef")
	sealedBody, encErr := bstack.SealBboxBody(sellerPub, aad, bodyRaw)
	if encErr != nil {
		return nil, encErr
	}
	msg := hub.BboxMessage{
		Office:       office,
		RecipientPub: sellerPub,
		SenderPub:    b.PubHex(),
		DealID:       dealID,
		Kind:         "purchase_beef",
		TS:           time.Now().Unix(),
		Nonce:        randNonceHex(),
		Aud:          hubAudience(hubURL),
		Body:         sealedBody,
	}
	sig, sErr := bstack.SignArtifact(b.engine.saleKey, msg.Signable())
	if sErr != nil {
		return nil, sErr
	}
	msg.Sig = sig
	env, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(hubURL, "/")+"/bbox/send", bytes.NewReader(env))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bbox send HTTP %d", resp.StatusCode)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case reply := <-ch:
		return &reply, nil
	case <-time.After(10 * time.Second):
		return nil, errors.New("bbox: timed out waiting for NAT seller key reveal")
	}
}

func (b *Broker) loadBboxCursor(hubURL string) int64 {
	var v int64
	if ok, _ := b.mem.DB().GetJSON("bbox_cursor:"+hubURL, &v); ok {
		return v
	}
	return 0
}

func (b *Broker) saveBboxCursor(hubURL string, seq int64) {
	if seq <= 0 {
		return
	}
	_ = b.mem.DB().SetJSON("bbox_cursor:"+hubURL, seq)
}

func (b *Broker) bboxLoop(ctx context.Context) {
	cursors := make(map[string]int64)
	for {
		b.mu.RLock()
		waitingReply := len(b.bboxReplies) > 0 || len(b.bboxWantByOffer) > 0
		b.mu.RUnlock()
		interval := 3 * time.Second
		if waitingReply {
			interval = 500 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			gotAny := false
			for _, h := range b.disc.ActiveHosts() {
				since, loaded := cursors[h]
				if !loaded {
					since = b.loadBboxCursor(h)
					cursors[h] = since
				}
				u := fmt.Sprintf("%s/bbox/inbox?pub=%s&since=%d", strings.TrimRight(h, "/"), b.PubHex(), since)
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
				if err != nil {
					continue
				}
				tsNow := time.Now().Unix()
				nonce := randNonceHex()
				aud := hubAudience(h)
				authPayload := map[string]any{
					"action": "bbox_inbox",
					"pub":    b.PubHex(),
					"ts":     tsNow,
					"nonce":  nonce,
					"aud":    aud,
					"since":  since,
				}
				if inSig, sErr := bstack.SignArtifact(b.engine.saleKey, authPayload); sErr == nil {
					req.Header.Set("X-Nektar-Pub", b.PubHex())
					req.Header.Set("X-Nektar-Ts", strconv.FormatInt(tsNow, 10))
					req.Header.Set("X-Nektar-Nonce", nonce)
					req.Header.Set("X-Nektar-Sig", inSig)
				}
				resp, err := b.httpClient.Do(req)
				if err != nil || resp.StatusCode != http.StatusOK {
					if resp != nil {
						resp.Body.Close()
					}
					continue
				}
				var parsed struct {
					Messages []hub.BboxMessage `json:"messages"`
				}
				_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&parsed)
				resp.Body.Close()
				for _, m := range parsed.Messages {
					gotAny = true
					b.bboxWakes.Add(1)
					// Only advance bbox_cursor when delivery succeeds; retry transient HTTP/Restate failures cleanly!
					if hErr := b.handleBboxMessage(ctx, h, m); hErr != nil {
						break
					}
					if m.Seq > cursors[h] {
						cursors[h] = m.Seq
						b.saveBboxCursor(h, m.Seq)
					}
				}
			}
			if gotAny {
				// Immediate quick follow-up when a trade message just arrived.
				select {
				case <-ctx.Done():
					return
				case <-time.After(250 * time.Millisecond):
				}
			}
		}
	}
}

func (b *Broker) claimSeenBboxNonce(sender, nonce string, now int64) bool {
	if strings.TrimSpace(nonce) == "" {
		return false
	}
	key := sender + ":" + nonce
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.seenBboxNonces) >= maxSeenBboxNonces {
		cutoff := now - 600
		for k, ts := range b.seenBboxNonces {
			if ts < cutoff {
				delete(b.seenBboxNonces, k)
			}
		}
	}
	if _, seen := b.seenBboxNonces[key]; seen {
		return false
	}
	b.seenBboxNonces[key] = now
	return true
}

func (b *Broker) handleBboxMessage(ctx context.Context, hubURL string, m hub.BboxMessage) error {
	// Verify recipient, timestamp, nonce uniqueness, and sender signature before decrypting!
	if m.RecipientPub != b.PubHex() {
		return nil
	}
	now := time.Now().Unix()
	if m.TS < now-300 || m.TS > now+300 || strings.TrimSpace(m.Nonce) == "" {
		return nil
	}
	if m.Sig == "" || bstack.VerifyArtifact(m.SenderPub, m.Sig, m.Signable()) != nil {
		return nil
	}
	aad := bstack.BboxAAD(m.Office, m.SenderPub, m.RecipientPub, m.DealID, m.Kind)
	plainBody, decErr := bstack.OpenBboxBody(b.engine.saleKey, aad, m.Body)
	if decErr != nil {
		return nil
	}
	switch m.Kind {
	case "purchase_beef":
		var pReq PurchaseRequest
		if json.Unmarshal(plainBody, &pReq) != nil {
			return nil
		}
		pResp, err := b.HandlePurchase(ctx, pReq)
		if err != nil {
			pResp = &PurchaseResponse{OK: false, OfferID: pReq.OfferID, SellerPub: b.PubHex(), Error: err.Error()}
		}
		bodyRaw, _ := json.Marshal(pResp)
		replyAAD := bstack.BboxAAD(m.Office, b.PubHex(), m.SenderPub, m.DealID, "reveal_key")
		sealedReply, encErr := bstack.SealBboxBody(m.SenderPub, replyAAD, bodyRaw)
		if encErr != nil {
			return encErr
		}
		replyMsg := hub.BboxMessage{
			Office:       m.Office,
			RecipientPub: m.SenderPub,
			SenderPub:    b.PubHex(),
			DealID:       m.DealID,
			Kind:         "reveal_key",
			TS:           time.Now().Unix(),
			Nonce:        randNonceHex(),
			Aud:          hubAudience(hubURL),
			Body:         sealedReply,
		}
		if sig, sErr := bstack.SignArtifact(b.engine.saleKey, replyMsg.Signable()); sErr == nil {
			replyMsg.Sig = sig
		}
		env, _ := json.Marshal(replyMsg)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(hubURL, "/")+"/bbox/send", bytes.NewReader(env))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, dErr := b.httpClient.Do(req)
		if dErr != nil {
			return dErr
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("bbox reveal_key send HTTP %d", resp.StatusCode)
		}
		_ = b.claimSeenBboxNonce(m.SenderPub, m.Nonce, now)
	case "reveal_key":
		var pResp PurchaseResponse
		if json.Unmarshal(plainBody, &pResp) != nil {
			return nil
		}
		b.mu.RLock()
		ch := b.bboxReplies[pResp.OfferID]
		expSeller := b.bboxExpected[pResp.OfferID]
		wantID := b.bboxWantByOffer[pResp.OfferID]
		rc := b.restate
		b.mu.RUnlock()
		if wantID == "" {
			wantID = m.DealID
		}
		// Durable anti-spoofing: if RAM bboxExpected is missing (after restart or spoofed OfferID),
		// verify m.SenderPub against the persisted SQLite buyer DealRecord.CounterpartyPub (fail CLOSED!).
		if expSeller == "" && wantID != "" && b.mem != nil && b.mem.DB() != nil {
			if deal, dErr := b.mem.DB().GetDeal(wantID); dErr == nil && deal.Role == "buyer" {
				expSeller = deal.CounterpartyPub
			}
		}
		if expSeller == "" || !strings.EqualFold(m.SenderPub, expSeller) {
			return nil // reject spoofed reveal_key from unknown or non-seller pubkey!
		}
		// Wake suspended Restate BuyerDeal workflow synchronously BEFORE advancing bbox_cursor!
		if rc != nil && wantID != "" && pResp.OK {
			dCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, kErr := rc.DeliverBuyerKey(dCtx, wantID, ClaimSignal{
				ClaimTxID:     pResp.OfferID,
				ContentKeyHex: pResp.ContentKeyHex,
				CiphertextHex: pResp.CiphertextHex,
				ChirpURL:      pResp.ChirpURL,
				ProofBundle:   pResp.ProofBundle,
			})
			cancel()
			if kErr != nil {
				return kErr
			}
			// Clean up RAM correlation entries after successful delivery so maps never leak unboundedly.
			b.mu.Lock()
			delete(b.bboxExpected, pResp.OfferID)
			delete(b.bboxWantByOffer, pResp.OfferID)
			b.mu.Unlock()
		}
		_ = b.claimSeenBboxNonce(m.SenderPub, m.Nonce, now)
		if ch != nil {
			select {
			case ch <- pResp:
			default:
			}
		}
	}
	return nil
}

func (b *Broker) lookupOffers(ctx context.Context, hosts []string, wantID string) []bstack.OfferAd {
	qBytes, _ := json.Marshal(map[string]any{
		"service": bstack.LookupService,
		"query":   bstack.MarketQuery{Kind: bstack.KindOffer, WantID: wantID, Limit: 50},
	})
	var out []bstack.OfferAd
	for _, h := range hosts {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(h, "/")+"/lookup", bytes.NewReader(qBytes))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := b.httpClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}
		var ans struct {
			Result []bstack.MarketEvent `json:"result"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&ans)
		resp.Body.Close()
		nowMs := time.Now().UnixMilli()
		maxExpMs := nowMs + bstack.MaxLifetime.Milliseconds() + 60_000
		myNet := b.mem.Chain().Network()
		for _, ev := range ans.Result {
			if ev.Payload.Kind != bstack.KindOffer {
				continue
			}
			var so bstack.SignedOffer
			if json.Unmarshal(ev.Payload.Body, &so) != nil {
				continue
			}
			if so.Offer.Net != myNet || so.Offer.Exp <= nowMs || so.Offer.Exp > maxExpMs {
				continue
			}
			if so.Offer.BuyerPub != "" && so.Offer.BuyerPub != b.PubHex() {
				continue
			}
			if bstack.VerifyArtifact(so.Offer.Pub, so.Sig, so.Offer) == nil {
				out = append(out, so.Offer)
			}
		}
	}
	return out
}

func (b *Broker) notify(event string, payload any) {
	if b.cfg.OnStateChange != nil {
		b.cfg.OnStateChange(event, payload)
	}
}

func pickBestFragment(frags []memory.RecalledFragment, query string) *memory.RecalledFragment {
	if len(frags) == 0 {
		return nil
	}
	var tokens []string
	for _, raw := range strings.Fields(strings.ToLower(query)) {
		tok := strings.Trim(raw, ".,?!:;\"'()[]{}")
		if len(tok) >= 3 {
			tokens = append(tokens, tok)
		}
	}
	if len(tokens) == 0 {
		cp := frags[len(frags)-1]
		return &cp
	}
	bestIdx := -1
	bestScore := 0
	for i, f := range frags {
		lower := strings.ToLower(f.Text)
		score := 0
		for _, tok := range tokens {
			if strings.Contains(lower, tok) {
				score++
			}
		}
		if score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return nil
	}
	cp := frags[bestIdx]
	return &cp
}

func lanesForDomains(net string, domains []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, d := range domains {
		l := bstack.LaneOf(net, d)
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		out = append(out, bstack.LaneOf(net, "general"))
	}
	return out
}

func shortPub(s string) string {
	if len(s) <= 10 {
		return s
	}
	return s[:10]
}

func randIDHex() string {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
