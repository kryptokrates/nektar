package market

import (
	"context"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

const (
	standingOrdersKVKey      = "standing_orders_v1"
	standingOrderInboxPrefix = "standing_order_inbox_v1:"
)

// StandingOrderInput starts a durable, crash-proof, live-steerable StandingOrder workflow
// ("Limit-Order for Knowledge") keyed by orderID. It sleeps at 0% CPU until a matching
// OfferAd arrives via durable inbox + restate.Signal, or until steered/cancelled/expired.
type StandingOrderInput struct {
	OrderID                string        `json:"orderId,omitempty"`
	Domains                []string      `json:"domains"`
	Query                  string        `json:"query,omitempty"`
	OntologyID             string        `json:"ontologyId,omitempty"`  // optional 64-hex OntologyID pin
	AllowBridge            bool          `json:"allowBridge,omitempty"` // accept foreign OntologyID when a committed Bridge Shard exists
	MaxSats                uint64        `json:"maxSats"`
	MinGrade               string        `json:"minGrade,omitempty"`
	MaxSatsPer1kUnits      uint64        `json:"maxSatsPer1kUnits,omitempty"`
	FreshnessClass         string        `json:"freshnessClass,omitempty"`
	RequiredRights         string        `json:"requiredRights,omitempty"`
	PricingMode            string        `json:"pricingMode,omitempty"`
	RequireTrustedOrBonded bool          `json:"requireTrustedOrBonded,omitempty"` // enforce Mangle trusted_seller/2 gate
	AllowUntrusted         bool          `json:"allowUntrusted,omitempty"`         // opt out of default live-network trust gate
	TargetShard            string        `json:"targetShard,omitempty"`
	AgentID                string        `json:"agentId,omitempty"`
	TTL                    time.Duration `json:"ttl,omitempty"`
	TTLSeconds             int64         `json:"ttlSeconds,omitempty"`
	MaxFills               int           `json:"maxFills,omitempty"` // default 1 (one-shot limit order); >1 for recurring (max 50)
	MaxAttempts            int           `json:"maxAttempts,omitempty"`
	WaitTimeout            time.Duration `json:"waitTimeout,omitempty"`
	ApprovalTimeout        time.Duration `json:"approvalTimeout,omitempty"`
}

// SteerStandingOrderInput live-steers a running StandingOrder without restarting it.
type SteerStandingOrderInput struct {
	MaxSats       *uint64 `json:"maxSats,omitempty"`
	Paused        *bool   `json:"paused,omitempty"`
	ExtendSeconds int64   `json:"extendSeconds,omitempty"`
	Reason        string  `json:"reason,omitempty"`
}

type standingOrderEvent struct {
	Seq    int64                    `json:"seq,omitempty"`
	Kind   string                   `json:"kind"` // offer | steer | cancel
	Offer  *bstack.OfferAd          `json:"offer,omitempty"`
	Steer  *SteerStandingOrderInput `json:"steer,omitempty"`
	Reason string                   `json:"reason,omitempty"`
}

// StandingOrderState is the durable state persisted inside Restate & mirrored in SQLite KV
// so the Broker's SSE loop knows which orders to wake without polling Restate.
type StandingOrderState struct {
	OrderID          string   `json:"orderId"`
	InvocationID     string   `json:"invocationId,omitempty"`
	WaitingSeq       int      `json:"waitingSeq"`
	LastProcessedSeq int64    `json:"lastProcessedSeq,omitempty"`
	Status           string   `json:"status"` // watching | paused | matching | filled | cancelled | expired
	Domains          []string `json:"domains"`
	Query            string   `json:"query,omitempty"`
	OntologyID             string   `json:"ontologyId,omitempty"`
	AllowBridge            bool     `json:"allowBridge,omitempty"`
	RequireTrustedOrBonded bool     `json:"requireTrustedOrBonded,omitempty"`
	MaxSats                uint64   `json:"maxSats"`
	MinGrade               string   `json:"minGrade,omitempty"`
	MaxSatsPer1kUnits      uint64   `json:"maxSatsPer1kUnits,omitempty"`
	FreshnessClass         string   `json:"freshnessClass,omitempty"`
	RequiredRights         string   `json:"requiredRights,omitempty"`
	PricingMode            string   `json:"pricingMode,omitempty"`
	TargetShard            string   `json:"targetShard,omitempty"`
	AgentID          string   `json:"agentId,omitempty"`
	ExpiresAtUnix    int64    `json:"expiresAtUnix"`
	OffersSeen       int      `json:"offersSeen"`
	Attempts         int      `json:"attempts"`
	Fills            int      `json:"fills"`
	MaxFills         int      `json:"maxFills"`
	LastDealID       string   `json:"lastDealId,omitempty"`
	PaymentTxID      string   `json:"paymentTxid,omitempty"`
	DeliveredText    string   `json:"deliveredText,omitempty"`
	Error            string   `json:"error,omitempty"`
	CreatedAt        int64    `json:"createdAt"`
	UpdatedAt        int64    `json:"updatedAt"`
}

// StandingOrderIndexEntry is the lightweight active-order record stored in SQLite KV
// so Hub SSE events match and wake sleeping Restate StandingOrders in O(N_active).
type StandingOrderIndexEntry struct {
	OrderID           string   `json:"orderId"`
	Domains           []string `json:"domains"`
	Query             string   `json:"query,omitempty"`
	OntologyID        string   `json:"ontologyId,omitempty"`
	AllowBridge       bool     `json:"allowBridge,omitempty"`
	MaxSats           uint64   `json:"maxSats"`
	MinGrade          string   `json:"minGrade,omitempty"`
	MaxSatsPer1kUnits uint64   `json:"maxSatsPer1kUnits,omitempty"`
	FreshnessClass    string   `json:"freshnessClass,omitempty"`
	RequiredRights    string   `json:"requiredRights,omitempty"`
	PricingMode       string   `json:"pricingMode,omitempty"`
	TargetShard       string   `json:"targetShard,omitempty"`
	AgentID           string   `json:"agentId,omitempty"`
	Status            string   `json:"status"`
	ExpiresAtUnix     int64    `json:"expiresAtUnix"`
	UpdatedAt         int64    `json:"updatedAt"`
}

func (e *Engine) enqueueStandingOrderEvent(orderID string, ev standingOrderEvent) {
	if e.mem == nil || e.mem.DB() == nil || orderID == "" {
		return
	}
	e.standingInboxMu.Lock()
	defer e.standingInboxMu.Unlock()
	key := standingOrderInboxPrefix + orderID
	var q []standingOrderEvent
	_, _ = e.mem.DB().GetJSON(key, &q)
	if ev.Kind == "offer" && ev.Offer != nil && ev.Offer.OfferID != "" {
		for _, prev := range q {
			if prev.Kind == "offer" && prev.Offer != nil && prev.Offer.OfferID == ev.Offer.OfferID {
				return // idempotent dedup
			}
		}
	}
	if len(q) >= 64 {
		// Preserve cancel/steer control events by evicting the oldest offer event first.
		evicted := false
		for i := range q {
			if q[i].Kind == "offer" {
				q = append(q[:i], q[i+1:]...)
				evicted = true
				break
			}
		}
		if !evicted {
			q = q[len(q)-63:]
		}
	}
	seqKey := standingOrderInboxPrefix + "seq:" + orderID
	var maxSeq int64
	_, _ = e.mem.DB().GetJSON(seqKey, &maxSeq)
	for _, prev := range q {
		if prev.Seq > maxSeq {
			maxSeq = prev.Seq
		}
	}
	ev.Seq = maxSeq + 1
	q = append(q, ev)
	_ = e.mem.DB().SetJSON(seqKey, ev.Seq)
	_ = e.mem.DB().SetJSON(key, q)
}

// peekStandingOrderEvents returns all queued events with Seq > afterSeq WITHOUT deleting them
// from SQLite, so a crash inside restate.Run before Restate commits the journal never drops events.
func (e *Engine) peekStandingOrderEvents(orderID string, afterSeq int64) []standingOrderEvent {
	if e.mem == nil || e.mem.DB() == nil || orderID == "" {
		return nil
	}
	e.standingInboxMu.Lock()
	defer e.standingInboxMu.Unlock()
	key := standingOrderInboxPrefix + orderID
	var q []standingOrderEvent
	ok, err := e.mem.DB().GetJSON(key, &q)
	if err != nil || !ok || len(q) == 0 {
		return nil
	}
	var out []standingOrderEvent
	for _, ev := range q {
		if ev.Seq == 0 || ev.Seq > afterSeq {
			out = append(out, ev)
		}
	}
	return out
}

// ackStandingOrderEvents compacts events with Seq <= upToSeq AFTER Restate has journaled the batch.
func (e *Engine) ackStandingOrderEvents(orderID string, upToSeq int64) {
	if e.mem == nil || e.mem.DB() == nil || orderID == "" || upToSeq <= 0 {
		return
	}
	e.standingInboxMu.Lock()
	defer e.standingInboxMu.Unlock()
	key := standingOrderInboxPrefix + orderID
	var q []standingOrderEvent
	ok, err := e.mem.DB().GetJSON(key, &q)
	if err != nil || !ok || len(q) == 0 {
		return
	}
	var keep []standingOrderEvent
	for _, ev := range q {
		if ev.Seq > upToSeq {
			keep = append(keep, ev)
		}
	}
	if keep == nil {
		keep = []standingOrderEvent{}
	}
	_ = e.mem.DB().SetJSON(key, keep)
}

func (e *Engine) drainStandingOrderEvents(orderID string) []standingOrderEvent {
	return e.peekStandingOrderEvents(orderID, 0)
}

// MarkStandingOrderKilled marks a standing order as killed in SQLite KV when its Restate invocation is killed.
func MarkStandingOrderKilled(db *store.DB, orderID string) {
	if db == nil || strings.TrimSpace(orderID) == "" {
		return
	}
	list := LoadStandingOrderIndex(db)
	now := time.Now().Unix()
	changed := false
	for i := range list {
		if list[i].OrderID == orderID && (list[i].Status == "watching" || list[i].Status == "paused" || list[i].Status == "matching") {
			list[i].Status = "killed"
			list[i].UpdatedAt = now
			changed = true
		}
	}
	if changed {
		_ = db.SetJSON(standingOrdersKVKey, list)
	}
}

func (e *Engine) clearStandingOrderInbox(orderID string) {
	if e.mem == nil || e.mem.DB() == nil || orderID == "" {
		return
	}
	e.standingInboxMu.Lock()
	defer e.standingInboxMu.Unlock()
	_ = e.mem.DB().SetJSON(standingOrderInboxPrefix+orderID, []standingOrderEvent{})
}

func standingOrderDomainMatches(wantDomains, offerDomains []string) bool {
	if len(wantDomains) == 0 || len(offerDomains) == 0 {
		return true
	}
	for _, wd := range wantDomains {
		w := strings.ToLower(strings.TrimSpace(wd))
		if w == "" || w == "*" {
			return true
		}
		for _, od := range offerDomains {
			if strings.EqualFold(w, strings.TrimSpace(od)) {
				return true
			}
		}
	}
	return false
}

func standingOrderOntologyMatches(ctx context.Context, mem *memory.Service, wantOntID string, allowBridge bool, offerOntID string) bool {
	want := strings.ToLower(strings.TrimSpace(wantOntID))
	if want == "" {
		return true
	}
	offer := strings.ToLower(strings.TrimSpace(offerOntID))
	if offer == "" {
		return false
	}
	if want == offer {
		return true
	}
	if allowBridge && mem != nil {
		if _, rules, err := mem.LoadBridgeRulesBetween(ctx, offer, want); err == nil && len(rules) > 0 {
			return true
		}
	}
	return false
}

func (e *Engine) internalCallHeaders() map[string]string {
	e.mu.RLock()
	tok := e.internalToken
	e.mu.RUnlock()
	if tok == "" {
		return nil
	}
	return map[string]string{internalTokenHeader: tok}
}

func (e *Engine) upsertStandingOrderIndex(st StandingOrderState) {
	if e.mem == nil || e.mem.DB() == nil || st.OrderID == "" {
		return
	}
	e.standingIndexMu.Lock()
	defer e.standingIndexMu.Unlock()
	list := LoadStandingOrderIndex(e.mem.DB())
	entry := StandingOrderIndexEntry{
		OrderID:           st.OrderID,
		Domains:           st.Domains,
		Query:             st.Query,
		OntologyID:        st.OntologyID,
		AllowBridge:       st.AllowBridge,
		MaxSats:           st.MaxSats,
		MinGrade:          st.MinGrade,
		MaxSatsPer1kUnits: st.MaxSatsPer1kUnits,
		FreshnessClass:    st.FreshnessClass,
		RequiredRights:    st.RequiredRights,
		PricingMode:       st.PricingMode,
		TargetShard:       st.TargetShard,
		AgentID:           st.AgentID,
		Status:            st.Status,
		ExpiresAtUnix:     st.ExpiresAtUnix,
		UpdatedAt:         st.UpdatedAt,
	}
	found := false
	for i := range list {
		if list[i].OrderID == st.OrderID {
			list[i] = entry
			found = true
			break
		}
	}
	if !found {
		list = append(list, entry)
	}
	// Keep at most 128 most recent standing order entries in KV.
	if len(list) > 128 {
		list = list[len(list)-128:]
	}
	_ = e.mem.DB().SetJSON(standingOrdersKVKey, list)
}

// LoadStandingOrderIndex returns persisted StandingOrder index entries from SQLite KV.
func LoadStandingOrderIndex(db *store.DB) []StandingOrderIndexEntry {
	if db == nil {
		return nil
	}
	var list []StandingOrderIndexEntry
	ok, err := db.GetJSON(standingOrdersKVKey, &list)
	if err != nil || !ok {
		return nil
	}
	return list
}
