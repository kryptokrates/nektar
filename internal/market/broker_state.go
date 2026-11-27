package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/store"
)

// BrokerConfig configures the autonomous live A2A & Hub Market Broker.
type BrokerConfig struct {
	PublicURL      string    // reachable HTTP base URL for direct purchase (empty if behind NAT!)
	BboxOffice     string    // NAT store-and-forward office name (default "default")
	Handle         string    // optional bfinger handle (e.g. "alice@nektar.run")
	IsHub          bool      // true when running as a Hub liquidity maker
	Hub            *hub.Node // optional embedded Hub node for advertising role/terms/fees on dir
	CollectTimeout time.Duration
	GraceTimeout   time.Duration // wait after first offer for competing quotes (default 200ms, or 500ms with AI matcher)
	OnStateChange  func(event string, payload any)
	HTTPClient     *http.Client
	RestateClient  *RestateClient // active Restate durable executor client (BuyerDeal, SellerDeal, Custody, Tx, AnchorCadence)
	Matcher        Matcher        // optional Jev System One semantic matcher
}

// WantRequest describes a buyer's live knowledge purchase request across active hubs.
type WantRequest struct {
	Domains            []string      `json:"domains"`
	Question           string        `json:"question"`
	MaxSats            uint64        `json:"maxSats"`
	EstTokensSaved     uint64        `json:"estTokensSaved,omitempty"` // optional token-arbitrage hint when MaxSats == 0
	TargetShard        string        `json:"targetShard,omitempty"`
	OntologyID         string        `json:"ontologyId,omitempty"` // optional 64-hex OntologyID filter
	CollectTimeout     time.Duration `json:"collectTimeout,omitempty"`
	StandingTTLSeconds int64         `json:"standingTtlSeconds,omitempty"`
	ApprovedOverrideID string        `json:"approvedOverrideId,omitempty"`
}

// SellInput arms a local memory shard for autonomous quoting & selling on the market.
type SellInput struct {
	Shard          string   `json:"shard"`
	Domains        []string `json:"domains"`
	PriceSats      uint64   `json:"priceSats"`
	Description    string   `json:"description,omitempty"`
	OntologyID     string   `json:"ontologyId,omitempty"`
	AllowRaw       bool     `json:"allowRaw,omitempty"` // explicit operator bypass for non-distilled raw shards
	DynamicPricing bool     `json:"dynamicPricing,omitempty"`
	FloorSats      uint64   `json:"floorSats,omitempty"`
	CeilingSats    uint64   `json:"ceilingSats,omitempty"`
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
	EscrowMode        bool   `json:"escrowMode,omitempty"`
	DepositorPKH      string `json:"depositorPkh,omitempty"`
	EscrowSellerPKH   string `json:"escrowSellerPkh,omitempty"`
	EscrowRefundAfter uint64 `json:"escrowRefundAfter,omitempty"`
	EscrowMaxFee      uint64 `json:"escrowMaxFee,omitempty"`
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
// When Status == "awaiting_confirmation", payment was SPV-verified & internalized but exceeds 0-conf caps
// (or settleDepth); ContentKeyHex is withheld until the tx mines at RequiredDepth and Seller delivers reveal_key.
type PurchaseResponse struct {
	OK             bool                   `json:"ok"`
	Status         string                 `json:"status,omitempty"` // settled | awaiting_confirmation
	RequiredDepth  int                    `json:"requiredDepth,omitempty"`
	OfferID        string                 `json:"offerId"`
	SellerPub      string                 `json:"sellerPub"`
	ContentKeyHex  string                 `json:"contentKeyHex,omitempty"`
	ClaimTxID      string                 `json:"claimTxid,omitempty"`
	ClaimTxBEEFHex string                 `json:"claimTxBeefHex,omitempty"`
	CiphertextHex  string                 `json:"ciphertextHex,omitempty"`
	ChirpURL       string                 `json:"chirpUrl,omitempty"`
	ProofBundle    *contracts.ProofBundle `json:"proofBundle,omitempty"`
	DealReceipt    *DealReceipt           `json:"dealReceipt,omitempty"`
	Error          string                 `json:"error,omitempty"`
}

type pendingConfirmEntry struct {
	OfferID           string `json:"offerId"`
	WantID            string `json:"wantId"`
	BuyerPub          string `json:"buyerPub"`
	TxID              string `json:"txid"`
	AmountSats        uint64 `json:"amountSats"`
	RequiredDepth     int    `json:"requiredDepth"`
	MinedHeight       uint32 `json:"minedHeight,omitempty"`
	MerklePathHex     string `json:"merklePathHex,omitempty"`
	BboxOffice        string `json:"bboxOffice,omitempty"`
	CreatedAt         int64  `json:"createdAt"`
	IsEscrow          bool   `json:"isEscrow,omitempty"`
	EscrowVout        uint32 `json:"escrowVout,omitempty"`
	EscrowBEEFHex     string `json:"escrowBeefHex,omitempty"`
	EscrowSellerPKH   string `json:"escrowSellerPkh,omitempty"`
	DepositorPKH      string `json:"depositorPkh,omitempty"`
	EscrowRefundAfter uint64 `json:"escrowRefundAfter,omitempty"`
	EscrowMaxFee      uint64 `json:"escrowMaxFee,omitempty"`
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

// MarketTapeEvent is one verified live market flow event (want, offer, or deal)
// surfaced to the real-time dashboard SSE stream and /state snapshot.
type MarketTapeEvent struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"` // want | offer | deal
	Side    string   `json:"side"` // buy | sell | clear
	Domains []string `json:"domains,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Sats    uint64   `json:"sats"`
	Pub     string   `json:"pub,omitempty"`
	Shard   string   `json:"shard,omitempty"`
	Status  string   `json:"status,omitempty"`
	IsSelf  bool     `json:"isSelf,omitempty"`
	At      int64    `json:"at"` // unix ms
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
	// Restore seller deals awaiting block confirmation so PollPendingConfirmations resumes after restart.
	if deals, err := b.mem.DB().ListDeals(500); err == nil {
		for _, d := range deals {
			if d.Role != "seller" || d.Status != "payment_pending" || d.PaymentTxID == "" {
				continue
			}
			var entry pendingConfirmEntry
			if ok, gErr := b.mem.DB().GetJSON("pending_confirm:"+d.PaymentTxID, &entry); gErr == nil && ok && entry.OfferID != "" {
				cp := entry
				b.pendingConfirmations[d.PaymentTxID] = &cp
			}
		}
	}
}

const marketKeyEncPrefix = "enc:"

// sealKeyAtRest encrypts a hex-encoded BRC-369 content key via BRC-43 wallet EncryptAtRest before SQLite persistence (F-28).
// Fails closed on encryption failure so plaintext reveal keys are never silently persisted in SQLite.
func (b *Broker) sealKeyAtRest(ctx context.Context, keyHex string) (string, error) {
	if keyHex == "" || strings.HasPrefix(keyHex, marketKeyEncPrefix) {
		return keyHex, nil
	}
	if b == nil || b.mem == nil || b.mem.Chain() == nil {
		return "", errors.New("market: wallet chain port required to encrypt reveal key at rest")
	}
	ct, err := b.mem.Chain().EncryptAtRest(ctx, "market-reveal-key", []byte(keyHex))
	if err != nil || len(ct) == 0 {
		if err == nil {
			err = errors.New("empty ciphertext")
		}
		return "", fmt.Errorf("market: encrypt reveal key at rest: %w", err)
	}
	return marketKeyEncPrefix + hex.EncodeToString(ct), nil
}

// openKeyAtRest unwraps a BRC-43 encrypted key ("enc:" prefix); legacy plaintext hex is rejected.
func (b *Broker) openKeyAtRest(ctx context.Context, stored string) string {
	if !strings.HasPrefix(stored, marketKeyEncPrefix) || b == nil || b.mem == nil || b.mem.Chain() == nil {
		return ""
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

func (b *Broker) notify(event string, payload any) {
	if b.cfg.OnStateChange != nil {
		b.cfg.OnStateChange(event, payload)
	}
}

// OnMarketTape registers a live callback invoked whenever a verified want/offer/deal enters the market tape.
func (b *Broker) OnMarketTape(fn func(event string, ev MarketTapeEvent)) {
	if b == nil || fn == nil {
		return
	}
	b.mu.Lock()
	b.tapeListeners = append(b.tapeListeners, fn)
	b.mu.Unlock()
}

// RecordMarketTape appends a verified event to the bounded ring buffer and notifies live listeners.
func (b *Broker) RecordMarketTape(event string, ev MarketTapeEvent) {
	if b == nil {
		return
	}
	if ev.At == 0 {
		ev.At = time.Now().UnixMilli()
	}
	b.mu.Lock()
	b.marketTape = append(b.marketTape, ev)
	if len(b.marketTape) > maxMarketTapeEvents {
		b.marketTape = append([]MarketTapeEvent(nil), b.marketTape[len(b.marketTape)-maxMarketTapeEvents:]...)
	}
	listeners := make([]func(event string, ev MarketTapeEvent), len(b.tapeListeners))
	copy(listeners, b.tapeListeners)
	b.mu.Unlock()
	for _, fn := range listeners {
		fn(event, ev)
	}
}

// RecentMarketEvents returns up to limit newest verified market tape events (newest first).
func (b *Broker) RecentMarketEvents(limit int) []MarketTapeEvent {
	if b == nil {
		return nil
	}
	if limit <= 0 || limit > maxMarketTapeEvents {
		limit = maxMarketTapeEvents
	}
	b.mu.RLock()
	n := len(b.marketTape)
	if n == 0 {
		b.mu.RUnlock()
		return nil
	}
	count := min(n, limit)
	out := make([]MarketTapeEvent, 0, count)
	for i := n - 1; i >= n-count; i-- {
		out = append(out, b.marketTape[i])
	}
	b.mu.RUnlock()
	return out
}
