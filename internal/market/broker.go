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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/memory"
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

// Broker drives real autonomous agent-to-agent and agent-to-Hub knowledge trading over live HTTP/SSE + NAT bbox.
type Broker struct {
	mem        *memory.Service
	engine     *Engine
	disc       *discovery.Service
	cfg        BrokerConfig
	httpClient *http.Client

	mu              sync.RWMutex
	restate         *RestateClient
	pendingSales    map[string]*pendingSale
	offerInbox      map[string]chan bstack.OfferAd
	bboxReplies     map[string]chan PurchaseResponse
	bboxExpected    map[string]string // offerID -> expected sellerPub
	bboxWantByOffer map[string]string // offerID -> wantID (for Restate BuyerDeal/deliverKey wakeup)
	activeLoops     map[string]context.CancelFunc
	seenWants       map[string]int64 // wantID -> seenUnix (bounded)
	packs           []bstack.PackRef
	bountyDoms      []string

	cursorMu sync.Mutex

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
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	_ = SyncPolicyFromDB(mem.DB(), engine.Policy())
	b := &Broker{
		mem:             mem,
		engine:          engine,
		disc:            disc,
		cfg:             cfg,
		httpClient:      hc,
		restate:         cfg.RestateClient,
		pendingSales:    make(map[string]*pendingSale),
		offerInbox:      make(map[string]chan bstack.OfferAd),
		bboxReplies:     make(map[string]chan PurchaseResponse),
		bboxExpected:    make(map[string]string),
		bboxWantByOffer: make(map[string]string),
		activeLoops:     make(map[string]context.CancelFunc),
		seenWants:       make(map[string]int64),
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
			if ps := pendingSaleFromRecord(r); ps != nil {
				b.pendingSales[r.OfferID] = ps
			}
		}
	}
}

func pendingSaleFromRecord(r store.PendingSaleRecord) *pendingSale {
	var so contracts.KeyedSaleOffer
	if err := json.Unmarshal(r.SaleOfferJSON, &so); err != nil {
		return nil
	}
	rkBytes, err := hex.DecodeString(r.RevealKeyHex)
	if err != nil || len(rkBytes) != 32 {
		return nil
	}
	var rk [32]byte
	copy(rk[:], rkBytes)
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

// Start launches per-host SSE market subscribers (with persisted Last-Event-ID cursors),
// a jittered poll-floor backup, and the NAT-safe bbox inbox consumer.
func (b *Broker) Start(ctx context.Context) {
	b.ensureHostLoops(ctx)
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				b.ensureHostLoops(ctx)
			}
		}
	}()
	// Jittered low-frequency poll floor catching any events dropped across SSE reconnects (event-arch skill).
	go func() {
		for {
			wait := time.Duration(float64(8*time.Second) * (0.6 + 0.8*rand.Float64()))
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
				for _, h := range b.disc.ActiveHosts() {
					b.pollHostOnce(ctx, h)
				}
			}
		}
	}()
	// NAT-safe bbox inbox loop (delivers purchases to NAT-ed sellers & key reveals to buyers).
	go b.bboxLoop(ctx)

	// Immediate boot peer crawl + periodic directory crawl (every 15m) & 24h announce keep-alive.
	go func() {
		if hosts := b.disc.ActiveHosts(); len(hosts) > 0 {
			_, _ = b.disc.DiscoverPeers(ctx, hosts)
		}
		crawlTicker := time.NewTicker(15 * time.Minute)
		announceTicker := time.NewTicker(24 * time.Hour)
		defer crawlTicker.Stop()
		defer announceTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-crawlTicker.C:
				if hosts := b.disc.ActiveHosts(); len(hosts) > 0 {
					_, _ = b.disc.DiscoverPeers(ctx, hosts)
				}
			case <-announceTicker.C:
				if hosts := b.disc.ActiveHosts(); len(hosts) > 0 {
					_, _ = b.AnnounceSelf(ctx)
				}
			}
		}
	}()
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
		go b.sseHostLoop(s.ctx, s.url)
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
		// Streaming client with no total timeout (context cancels cleanly on shutdown).
		streamClient := &http.Client{}
		resp, err := streamClient.Do(req)
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
	defer resp.Body.Close()
	var items []hub.StoredEvent
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&items); err != nil {
		return
	}
	if len(items) > 0 {
		b.pollWakes.Add(1)
	}
	for _, it := range items {
		b.saveCursor(hubURL, it.Seq)
		b.handleMarketEvent(ctx, bstack.MarketEvent{
			Seq:        it.Seq,
			Topic:      it.Topic,
			TxID:       it.TxID,
			Vout:       it.Vout,
			AdmittedAt: it.AdmittedAt,
			Payload:    it.Payload,
		})
	}
}

func (b *Broker) handleMarketEvent(ctx context.Context, ev bstack.MarketEvent) {
	switch ev.Payload.Kind {
	case bstack.KindWant:
		var sw bstack.SignedWant
		if err := json.Unmarshal(ev.Payload.Body, &sw); err != nil {
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

	frags, err := b.mem.Recall(ctx, memory.RecallInput{Shard: matched.Shard})
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
		RevealKeyHex:    hex.EncodeToString(revealKey[:]),
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
	b.mu.Lock()
	sale, ok := b.pendingSales[req.OfferID]
	b.mu.Unlock()
	if !ok {
		if rec, dErr := b.mem.DB().GetPendingSale(req.OfferID); dErr == nil {
			if restored := pendingSaleFromRecord(rec); restored != nil && (restored.ExpiresAt == 0 || restored.ExpiresAt >= time.Now().Unix()) {
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
	if sale.BuyerPub != "" && req.BuyerPub != "" && sale.BuyerPub != req.BuyerPub {
		return nil, errors.New("market: buyerPub does not match offer recipient")
	}
	beefBytes, err := hex.DecodeString(strings.TrimSpace(req.PaymentBEEFHex))
	if err != nil {
		return nil, fmt.Errorf("market: decode paymentBeefHex: %w", err)
	}
	tx, err := transaction.NewTransactionFromBEEF(beefBytes)
	if err != nil {
		return nil, fmt.Errorf("market: parse payment AtomicBEEF: %w", err)
	}
	txid := tx.TxID().String()
	if req.TxID != "" && req.TxID != txid {
		return nil, errors.New("market: payment txid mismatch vs AtomicBEEF")
	}

	// Idempotent key re-reveal on buyer retry for the EXACT same payment txid!
	if existing, gErr := b.mem.DB().GetDeal("sell-" + sale.OfferID); gErr == nil && existing.Status == "settled" {
		if existing.PaymentTxID == txid {
			keyHex := existing.ClaimKeyHex
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

	// Verify payment transaction pays >= Offer.Price via BRC-29 derived key OR legacy FundScript!
	isBRC29 := req.DerivationPrefix != "" && req.DerivationSuffix != "" && req.SenderIdentityKey != ""
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

	// Enforce ZeroConfMaxSats policy when tx lacks a block MerklePath!
	_, _, _, zeroConfMax, _, _, _ := b.engine.Policy().Snapshot()
	if tx.MerklePath == nil && zeroConfMax > 0 && sale.SaleOffer.Config.Price > zeroConfMax {
		return nil, fmt.Errorf("market: payment %d sats exceeds zeroConfMaxSats (%d) without confirmation proof", sale.SaleOffer.Config.Price, zeroConfMax)
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

	// Credit Seller's wallet immediately & verify SPV against shared headers on live networks!
	if b.mem.Chain().Network() != "memory" {
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
		ClaimKeyHex:     hex.EncodeToString(sale.RevealKey[:]),
		ChirpURL:        sale.ChirpURL,
		CounterpartyPub: req.BuyerPub,
		PriceSats:       sale.SaleOffer.Config.Price,
		IsHubSeller:     b.cfg.IsHub,
	})
	b.notify("deal_settled", map[string]any{"role": "seller", "offerId": sale.OfferID, "txid": txid, "sats": sale.SaleOffer.Config.Price})

	// Notify active Restate SellerDeal workflow + Tx lifecycle journal asynchronously!
	if rc := b.RestateClient(); rc != nil {
		confirmed := tx.MerklePath != nil
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
		msg := hub.BboxMessage{
			Office:       chosen.BboxOffice,
			RecipientPub: chosen.Pub,
			SenderPub:    b.PubHex(),
			DealID:       dealID,
			Kind:         "purchase_beef",
			Body:         bodyRaw,
		}
		sig, sErr := bstack.SignArtifact(b.engine.saleKey, msg.Signable())
		if sErr != nil {
			return nil, sErr
		}
		msg.Sig = sig
		env, _ := json.Marshal(msg)
		sent := false
		for _, h := range hosts {
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

	// Active Restate Durable Executor path (primary production path — NO local-broker bypass!).
	if rc := b.RestateClient(); rc != nil {
		if _, err := rc.StartBuyerDealAsync(ctx, wantID, BuyerDealInput{
			Want:               want,
			TargetShard:        req.TargetShard,
			CollectTimeout:     req.CollectTimeout,
			WaitTimeout:        5 * time.Minute,
			ApprovedOverrideID: req.ApprovedOverrideID,
		}); err != nil {
			return nil, err
		}
		handle := ingress.WorkflowHandle[DealState](rc.Ingress, "BuyerDeal", wantID)
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		for {
			if rec, gErr := b.mem.DB().GetDeal(wantID); gErr == nil {
				if rec.Status == "approval_pending" {
					// Workflow is durably suspended on its Restate Awakeable; return immediately to caller!
					return &rec, nil
				}
				if rec.Status == "settled" || rec.Status == "expired" || rec.Status == "failed" {
					return &rec, nil
				}
			}
			if st, oErr := handle.Output(ctx); oErr == nil {
				if rec, gErr := b.mem.DB().GetDeal(wantID); gErr == nil {
					return &rec, nil
				}
				return &store.DealRecord{
					DealID:        wantID,
					Role:          "buyer",
					Status:        st.Status,
					Shard:         st.Shard,
					PaymentTxID:   st.PaymentTxID,
					DeliveredText: st.DeliveredTxt,
					Error:         st.Error,
				}, nil
			}
			select {
			case <-ctx.Done():
				if rec, gErr := b.mem.DB().GetDeal(wantID); gErr == nil {
					return &rec, ctx.Err()
				}
				return nil, ctx.Err()
			case <-ticker.C:
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
// it resolves that EXACT awakeable to resume the original wantID (zero duplicate deals!).
func (b *Broker) ApproveAndExecute(ctx context.Context, approvalID string, approve bool) (store.ApprovalRecord, *store.DealRecord, error) {
	appr, err := b.mem.DB().DecideApproval(approvalID, approve)
	if err != nil {
		return store.ApprovalRecord{}, nil, err
	}
	// Active Restate Awakeable resume path (resumes the exact suspended BuyerDeal workflow!).
	if rc := b.RestateClient(); rc != nil && appr.AwakeableID != "" {
		dec := ApprovalDecision{Approved: approve, DecidedBy: "operator"}
		if !approve {
			dec.Reason = "denied by operator"
		}
		if rErr := rc.ResolveAwakeable(ctx, appr.AwakeableID, dec); rErr != nil {
			return appr, nil, rErr
		}
		if st, aErr := rc.AttachBuyerDeal(ctx, appr.DealID); aErr == nil && st != nil {
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

	if !approve {
		if deal, dErr := b.mem.DB().GetDeal(appr.DealID); dErr == nil {
			deal.Status = "failed"
			deal.Error = "denied by operator"
			_ = b.mem.DB().SaveDeal(deal)
			return appr, &deal, nil
		}
		return appr, nil, nil
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
	if hubURL == "" {
		hosts := b.disc.ActiveHosts()
		if len(hosts) == 0 {
			return nil, errors.New("market: no active hub host configured")
		}
		hubURL = hosts[0]
	}
	frags, err := b.mem.Recall(ctx, memory.RecallInput{Shard: shard})
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
		return nil, fmt.Errorf("market: fragment %s#%d not found", shard, seq)
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
	sub := hub.BountySubmission{
		BountyID:      bountyID,
		SellerPub:     b.PubHex(),
		SellerAddress: addr,
		Shard:         shard,
		Seq:           target.Seq,
		HDP:           target.HDP,
		Text:          target.Text,
		ProvenanceHex: provHex,
		ProofBundle:   pbRaw,
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
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("hub bounty submit HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
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
	msg := hub.BboxMessage{
		Office:       office,
		RecipientPub: sellerPub,
		SenderPub:    b.PubHex(),
		DealID:       dealID,
		Kind:         "purchase_beef",
		Body:         bodyRaw,
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
		waitingReply := len(b.bboxReplies) > 0
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
				if inSig, sErr := bstack.SignArtifact(b.engine.saleKey, map[string]any{"action": "bbox_inbox", "pub": b.PubHex(), "ts": tsNow}); sErr == nil {
					req.Header.Set("X-Nektar-Pub", b.PubHex())
					req.Header.Set("X-Nektar-Ts", strconv.FormatInt(tsNow, 10))
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
					// Process message FIRST, then persist bbox_cursor so a mid-handler crash retries cleanly!
					b.handleBboxMessage(ctx, h, m)
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

func (b *Broker) handleBboxMessage(ctx context.Context, hubURL string, m hub.BboxMessage) {
	// Verify sender signature on every relayed bbox message before trusting payload!
	if m.Sig == "" || bstack.VerifyArtifact(m.SenderPub, m.Sig, m.Signable()) != nil {
		return
	}
	switch m.Kind {
	case "purchase_beef":
		var pReq PurchaseRequest
		if json.Unmarshal(m.Body, &pReq) != nil {
			return
		}
		pResp, err := b.HandlePurchase(ctx, pReq)
		if err != nil {
			pResp = &PurchaseResponse{OK: false, OfferID: pReq.OfferID, SellerPub: b.PubHex(), Error: err.Error()}
		}
		bodyRaw, _ := json.Marshal(pResp)
		replyMsg := hub.BboxMessage{
			Office:       m.Office,
			RecipientPub: m.SenderPub,
			SenderPub:    b.PubHex(),
			DealID:       m.DealID,
			Kind:         "reveal_key",
			Body:         bodyRaw,
		}
		if sig, sErr := bstack.SignArtifact(b.engine.saleKey, replyMsg.Signable()); sErr == nil {
			replyMsg.Sig = sig
		}
		env, _ := json.Marshal(replyMsg)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(hubURL, "/")+"/bbox/send", bytes.NewReader(env))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			if resp, dErr := b.httpClient.Do(req); dErr == nil {
				resp.Body.Close()
			}
		}
	case "reveal_key":
		var pResp PurchaseResponse
		if json.Unmarshal(m.Body, &pResp) != nil {
			return
		}
		b.mu.RLock()
		ch := b.bboxReplies[pResp.OfferID]
		expSeller := b.bboxExpected[pResp.OfferID]
		wantID := b.bboxWantByOffer[pResp.OfferID]
		rc := b.restate
		b.mu.RUnlock()
		if expSeller != "" && m.SenderPub != expSeller {
			return // reject spoofed reveal_key from non-seller pubkey!
		}
		if wantID == "" {
			wantID = m.DealID
		}
		// Wake suspended Restate BuyerDeal workflow via Promise("claim")!
		if rc != nil && wantID != "" && pResp.OK {
			go func(wid string, pr PurchaseResponse) {
				dCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, _ = rc.DeliverBuyerKey(dCtx, wid, ClaimSignal{
					ClaimTxID:     pr.OfferID,
					ContentKeyHex: pr.ContentKeyHex,
					CiphertextHex: pr.CiphertextHex,
					ChirpURL:      pr.ChirpURL,
					ProofBundle:   pr.ProofBundle,
				})
			}(wantID, pResp)
		}
		if ch != nil {
			select {
			case ch <- pResp:
			default:
			}
		}
	}
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
		for _, ev := range ans.Result {
			if ev.Payload.Kind != bstack.KindOffer {
				continue
			}
			var so bstack.SignedOffer
			if json.Unmarshal(ev.Payload.Body, &so) == nil && bstack.VerifyArtifact(so.Offer.Pub, so.Sig, so.Offer) == nil {
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
