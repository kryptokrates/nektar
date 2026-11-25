package market

import (
	"bufio"
	"bytes"
	"context"
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
	"github.com/lightwebinc/bcommon/payee"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
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
	OfferID        string `json:"offerId"`
	WantID         string `json:"wantId"`
	BuyerPub       string `json:"buyerPub"`
	TxID           string `json:"txid"`
	PaymentBEEFHex string `json:"paymentBeefHex"`
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

	mu           sync.RWMutex
	pendingSales map[string]*pendingSale
	offerInbox   map[string]chan bstack.OfferAd
	bboxReplies  map[string]chan PurchaseResponse
	activeLoops  map[string]bool
	seenWants    map[string]bool
	packs        []bstack.PackRef
	bountyDoms   []string

	claimsMu  sync.Mutex
	payClaims payee.Claims

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
		cfg.CollectTimeout = 1500 * time.Millisecond
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	_ = SyncPolicyFromDB(mem.DB(), engine.Policy())
	return &Broker{
		mem:          mem,
		engine:       engine,
		disc:         disc,
		cfg:          cfg,
		httpClient:   hc,
		pendingSales: make(map[string]*pendingSale),
		offerInbox:   make(map[string]chan bstack.OfferAd),
		bboxReplies:  make(map[string]chan PurchaseResponse),
		activeLoops:  make(map[string]bool),
		seenWants:    make(map[string]bool),
	}
}

func (b *Broker) Engine() *Engine               { return b.engine }
func (b *Broker) Discovery() *discovery.Service { return b.disc }
func (b *Broker) PubHex() string                { return hex.EncodeToString(b.engine.saleKey.PubKey().Compressed()) }
func (b *Broker) PublicURL() string             { return strings.TrimRight(b.cfg.PublicURL, "/") }
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
}

func (b *Broker) ensureHostLoops(ctx context.Context) {
	for _, h := range b.disc.ActiveHosts() {
		b.mu.Lock()
		already := b.activeLoops[h]
		if !already {
			b.activeLoops[h] = true
		}
		b.mu.Unlock()
		if !already {
			go b.sseHostLoop(ctx, h)
		}
	}
}

func (b *Broker) loadCursor(hubURL string) int64 {
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
	if cur := b.loadCursor(hubURL); seq > cur {
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
			if backoff < 10*time.Second {
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
			b.saveCursor(hubURL, ev.Seq)
			b.handleMarketEvent(ctx, ev)
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
		if b.seenWants[sw.Want.WantID] {
			b.mu.Unlock()
			return
		}
		b.seenWants[sw.Want.WantID] = true
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
	offerID := fmt.Sprintf("off-%s-%d", shortPub(b.PubHex()), time.Now().UnixNano()%1_000_000_000)

	b.mu.Lock()
	pubURL := b.cfg.PublicURL
	office := b.cfg.BboxOffice
	isHub := b.cfg.IsHub
	b.pendingSales[offerID] = &pendingSale{
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
	b.mu.Unlock()

	offer := bstack.OfferAd{
		OfferID:      offerID,
		WantID:       want.WantID,
		Pub:          b.PubHex(),
		BuyerPub:     want.Pub,
		Net:          b.mem.Chain().Network(),
		Domains:      want.Domains,
		PriceSats:    matched.PriceSats,
		Shard:        matched.Shard,
		Leaf:         best.Leaf,
		ChirpURL:     chirpURL,
		KeyCommitHex: saleOffer.KeyCommitment,
		PayAddress:   payAddr,
		SellerURL:    pubURL,
		BboxOffice:   office,
		SaleOffer:    saleJSON,
		IsHub:        isHub,
		Direct:       pubURL != "",
		Revision:     1,
		Exp:          time.Now().Add(30 * time.Minute).UnixMilli(),
	}
	sig, err := bstack.SignArtifact(b.engine.saleKey, offer)
	if err != nil {
		return
	}
	body, err := json.Marshal(bstack.SignedOffer{Offer: offer, Sig: sig})
	if err != nil {
		return
	}
	beef, err := discovery.BuildMarketBEEF(bstack.MarketPayload{Kind: bstack.KindOffer, Body: body})
	if err != nil {
		return
	}
	topics := lanesForDomains(offer.Net, want.Domains)
	for _, h := range b.disc.ActiveHosts() {
		_ = b.disc.SubmitBEEF(ctx, h, beef, topics)
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

	// Verify payment transaction pays >= Offer.Price to Seller's own funding script!
	fundScript, err := b.mem.Chain().Signer().FundScript()
	if err != nil {
		return nil, err
	}
	fundHex := fundScript.String()
	var paidSats uint64
	for _, out := range tx.Outputs {
		if out.LockingScript != nil && out.LockingScript.String() == fundHex {
			paidSats += out.Satoshis
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

	// BRC-105 / payee.Claims replay & double-spend conflict guard!
	inputs := make([]string, 0, len(tx.Inputs))
	for _, in := range tx.Inputs {
		if in.SourceTXID != nil {
			inputs = append(inputs, fmt.Sprintf("%s:%d", in.SourceTXID.String(), in.SourceTxOutIndex))
		}
	}
	b.claimsMu.Lock()
	claim := b.payClaims.Claim(txid, inputs)
	b.claimsMu.Unlock()
	if claim != payee.Accepted {
		return nil, fmt.Errorf("market: payment claim rejected (%s)", claim.String())
	}

	// Mark settled & persist durable Seller DealRecord!
	b.mu.Lock()
	sale.Settled = true
	b.mu.Unlock()
	_ = b.mem.DB().SaveDeal(store.DealRecord{
		DealID:          "sell-" + sale.OfferID,
		Role:            "seller",
		Status:          "settled",
		PaymentTxID:     txid,
		PaymentBEEF:     beefBytes,
		ClaimKeyHex:     hex.EncodeToString(sale.RevealKey[:]),
		ChirpURL:        sale.ChirpURL,
		CounterpartyPub: req.BuyerPub,
		PriceSats:       sale.SaleOffer.Config.Price,
		IsHubSeller:     b.cfg.IsHub,
	})
	b.notify("deal_settled", map[string]any{"role": "seller", "offerId": sale.OfferID, "txid": txid, "sats": sale.SaleOffer.Config.Price})

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

// Want publishes a signed WantAd across active hubs, collects & ranks verified offers,
// executes AtomicBEEF payment (direct HTTP or NAT bbox fallback), verifies the revealed
// BRC-369 content key + remote CHIRP closure + SPV Merkle ProofBundle, and commits the knowledge!
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
	_ = SyncPolicyFromDB(b.mem.DB(), b.engine.Policy())
	kill, maxDeal, maxDay, _, spent, buyerOK, _ := b.engine.Policy().Snapshot()
	if kill {
		return nil, errors.New("market: operator kill switch is engaged")
	}

	wantID := fmt.Sprintf("want-%s-%d", shortPub(b.PubHex()), time.Now().UnixNano()%1_000_000_000)
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
	wantJSON, _ := json.Marshal(want)

	// Load-bearing operator consent gate: require active buyer policy & caps unless explicitly approved!
	if req.ApprovedOverrideID == "" && (!buyerOK || (maxDeal > 0 && req.MaxSats > maxDeal) || (maxDay > 0 && spent+req.MaxSats > maxDay)) {
		reason := "no active buyer policy approved (run `nektar policy approve --preset buyer` or approve in Dashboard)"
		if buyerOK && maxDeal > 0 && req.MaxSats > maxDeal {
			reason = fmt.Sprintf("requested maxSats (%d) exceeds buyer policy maxPerDeal (%d)", req.MaxSats, maxDeal)
		} else if buyerOK && maxDay > 0 && spent+req.MaxSats > maxDay {
			reason = fmt.Sprintf("requested maxSats (%d) exceeds remaining daily cap (%d)", req.MaxSats, maxDay-spent)
		}
		appID := fmt.Sprintf("appr-%d", time.Now().UnixNano()%1_000_000_000)
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
		rec.Status = "expired"
		rec.Error = "no valid offers received within collect window"
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

	// Split coins proactively if UTXO count is low so parallel deals never starve.
	_ = b.mem.Chain().EnsureSplitCoins(ctx, 4, maxU64(chosen.PriceSats*2, 1000))

	// Build, SpendGuard-check, and broadcast real BSV payment to Seller's signed PayAddress!
	payTx, payTxID, err := b.mem.Chain().SendToAddress(ctx, chosen.PayAddress, chosen.PriceSats)
	if err != nil {
		rec.Status = "failed"
		rec.Error = "payment failed: " + err.Error()
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, err
	}
	payBEEF, err := payTx.AtomicBEEF(false)
	if err != nil {
		rec.Status = "failed"
		rec.Error = "encode payment AtomicBEEF: " + err.Error()
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, err
	}
	b.engine.Policy().RecordSpend(chosen.PriceSats)
	rec.PaymentTxID = payTxID
	rec.PaymentBEEF = payBEEF
	rec.Status = "paid"
	_ = b.mem.DB().SaveDeal(rec)

	// Deliver AtomicBEEF to Seller via Direct HTTP OR NAT bbox fallback!
	pReq := PurchaseRequest{
		OfferID:        chosen.OfferID,
		WantID:         wantID,
		BuyerPub:       b.PubHex(),
		TxID:           payTxID,
		PaymentBEEFHex: hex.EncodeToString(payBEEF),
	}
	var pResp *PurchaseResponse
	if chosen.SellerURL != "" {
		pResp, err = b.sendDirectPurchase(ctx, chosen.SellerURL, pReq)
	}
	if pResp == nil && chosen.BboxOffice != "" {
		pResp, err = b.sendBboxPurchase(ctx, hosts[0], chosen.Pub, chosen.BboxOffice, wantID, pReq)
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

	// 4. Verify seller's SPV Merkle ProofBundle if attached!
	if pResp.ProofBundle != nil {
		if err := verifyBundleOffline(*pResp.ProofBundle); err != nil {
			_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
			rec.Status = "failed"
			rec.Error = "seller SPV ProofBundle invalid: " + err.Error()
			_ = b.mem.DB().SaveDeal(rec)
			return &rec, err
		}
	}

	// 5. Commit verified knowledge into Buyer's own memory shard & record verified Seller reputation!
	targetShard := strings.TrimSpace(req.TargetShard)
	if targetShard == "" {
		targetShard = "market/" + req.Domains[0]
	}
	if _, err := b.mem.Remember(ctx, memory.RememberInput{
		Shard:      targetShard,
		Text:       string(plaintext),
		Source:     "market:" + shortPub(chosen.Pub),
		Class:      req.Domains[0],
		AutoAnchor: b.mem.Chain().BalanceSats() > 1000,
	}); err != nil {
		rec.Status = "failed"
		rec.Error = "commit purchased fact: " + err.Error()
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, err
	}

	_ = b.mem.DB().RecordReputation(chosen.Pub, true, chosen.PriceSats)
	rec.ClaimKeyHex = pResp.ContentKeyHex
	rec.DeliveredText = string(plaintext)
	rec.Status = "settled"
	rec.Error = ""
	_ = b.mem.DB().SaveDeal(rec)
	b.notify("deal_settled", map[string]any{"role": "buyer", "dealId": rec.DealID, "txid": payTxID, "sats": chosen.PriceSats, "shard": targetShard})
	return &rec, nil
}

// ApproveAndExecute settles a pending operator approval and, if approved, re-runs the paused Want deal!
func (b *Broker) ApproveAndExecute(ctx context.Context, approvalID string, approve bool) (store.ApprovalRecord, *store.DealRecord, error) {
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
	sub := hub.BountySubmission{
		BountyID:      bountyID,
		SellerPub:     b.PubHex(),
		SellerAddress: addr,
		Shard:         shard,
		Seq:           target.Seq,
		HDP:           target.HDP,
		Text:          target.Text,
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
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.bboxReplies, pReq.OfferID)
		b.mu.Unlock()
	}()

	bodyRaw, _ := json.Marshal(pReq)
	env, _ := json.Marshal(hub.BboxMessage{
		Office:       office,
		RecipientPub: sellerPub,
		SenderPub:    b.PubHex(),
		DealID:       dealID,
		Kind:         "purchase_beef",
		Body:         bodyRaw,
	})
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

func (b *Broker) bboxLoop(ctx context.Context) {
	cursors := make(map[string]int64)
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, h := range b.disc.ActiveHosts() {
				since := cursors[h]
				u := fmt.Sprintf("%s/bbox/inbox?pub=%s&since=%d", strings.TrimRight(h, "/"), b.PubHex(), since)
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
				if err != nil {
					continue
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
					if m.Seq > cursors[h] {
						cursors[h] = m.Seq
					}
					b.bboxWakes.Add(1)
					b.handleBboxMessage(ctx, h, m)
				}
			}
		}
	}
}

func (b *Broker) handleBboxMessage(ctx context.Context, hubURL string, m hub.BboxMessage) {
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
		env, _ := json.Marshal(hub.BboxMessage{
			Office:       m.Office,
			RecipientPub: m.SenderPub,
			SenderPub:    b.PubHex(),
			DealID:       m.DealID,
			Kind:         "reveal_key",
			Body:         bodyRaw,
		})
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
		b.mu.RUnlock()
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
	tokens := strings.Fields(strings.ToLower(query))
	bestIdx := len(frags) - 1
	bestScore := -1
	for i, f := range frags {
		lower := strings.ToLower(f.Text)
		score := 0
		for _, tok := range tokens {
			if len(tok) >= 3 && strings.Contains(lower, tok) {
				score++
			}
		}
		if score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	cp := frags[bestIdx]
	return &cp
}

func verifyBundleOffline(pb contracts.ProofBundle) error {
	leafBytes, err := hex.DecodeString(pb.Leaf)
	if err != nil {
		return err
	}
	rootBytes, err := hex.DecodeString(pb.Anchor.R)
	if err != nil {
		return err
	}
	siblings := make([][]byte, len(pb.Path))
	for i, s := range pb.Path {
		b, err := hex.DecodeString(s)
		if err != nil {
			return err
		}
		siblings[i] = b
	}
	if !core.VerifyProof(leafBytes, &core.MerkleProof{Index: pb.LeafIndex, Siblings: siblings}, rootBytes) {
		return errors.New("merkle proof invalid")
	}
	return nil
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

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
