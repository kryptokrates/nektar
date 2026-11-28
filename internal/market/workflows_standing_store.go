package market

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

const (
	standingOrdersKVKey      = "standing_orders_v1"
	standingOrderIdxPrefix   = "standing_order_idx_v1:"
	standingOrderStatePrefix = "standing_order_state_v1:"
	standingOrderInboxPrefix = "standing_order_inbox_v1:"
)

// StandingOrderInput starts a durable, crash-proof, live-steerable StandingOrderWorkflow keyed by orderID.
type StandingOrderInput struct {
	OrderID                string        `json:"orderId,omitempty"`
	Domains                []string      `json:"domains"`
	Query                  string        `json:"query,omitempty"`
	OntologyID             string        `json:"ontologyId,omitempty"`
	AllowBridge            bool          `json:"allowBridge,omitempty"`
	MaxSats                uint64        `json:"maxSats"`
	MinGrade               string        `json:"minGrade,omitempty"`
	MaxSatsPer1kUnits      uint64        `json:"maxSatsPer1kUnits,omitempty"`
	FreshnessClass         string        `json:"freshnessClass,omitempty"`
	RequiredRights         string        `json:"requiredRights,omitempty"`
	PricingMode            string        `json:"pricingMode,omitempty"`
	RequireTrustedOrBonded bool          `json:"requireTrustedOrBonded,omitempty"`
	AllowUntrusted         bool          `json:"allowUntrusted,omitempty"`
	TargetShard            string        `json:"targetShard,omitempty"`
	AgentID                string        `json:"agentId,omitempty"`
	TTL                    time.Duration `json:"ttl,omitempty"`
	TTLSeconds             int64         `json:"ttlSeconds,omitempty"`
	MaxFills               int           `json:"maxFills,omitempty"`
	MaxAttempts            int           `json:"maxAttempts,omitempty"`
	WaitTimeout            time.Duration `json:"waitTimeout,omitempty"`
	ApprovalTimeout        time.Duration `json:"approvalTimeout,omitempty"`
	// ContinueAsNew carry-over fields bounding workflows.sqlite history growth (DDIA Ch. 9 / RFC-003).
	ResumeState                 *StandingOrderState `json:"resumeState,omitempty"`
	SeenOfferIDs                []string            `json:"seenOfferIds,omitempty"`
	MaxIterationsBeforeRollover int                 `json:"maxIterationsBeforeRollover,omitempty"`
}

// SteerStandingOrderInput live-steers a running StandingOrderWorkflow without restarting it.
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

// StandingOrderState is the durable state persisted for a StandingOrderWorkflow.
type StandingOrderState struct {
	OrderID                string   `json:"orderId"`
	InvocationID           string   `json:"invocationId,omitempty"`
	WaitingSeq             int      `json:"waitingSeq"`
	LastProcessedSeq       int64    `json:"lastProcessedSeq,omitempty"`
	Status                 string   `json:"status"` // watching | paused | matching | filled | cancelled | expired | killed
	Domains                []string `json:"domains"`
	Query                  string   `json:"query,omitempty"`
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
	AgentID                string   `json:"agentId,omitempty"`
	ExpiresAtUnix          int64    `json:"expiresAtUnix"`
	OffersSeen             int      `json:"offersSeen"`
	Attempts               int      `json:"attempts"`
	Fills                  int      `json:"fills"`
	MaxFills               int      `json:"maxFills"`
	LastDealID             string   `json:"lastDealId,omitempty"`
	PaymentTxID            string   `json:"paymentTxid,omitempty"`
	DeliveredText          string   `json:"deliveredText,omitempty"`
	Error                  string   `json:"error,omitempty"`
	CreatedAt              int64    `json:"createdAt"`
	UpdatedAt              int64    `json:"updatedAt"`
}

// StandingOrderIndexEntry is the lightweight active-order record stored in SQLite KV.
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

func (e *Engine) saveStandingOrderState(st StandingOrderState) {
	if e == nil || e.mem == nil || e.mem.DB() == nil || st.OrderID == "" {
		return
	}
	e.standingIndexMu.Lock()
	defer e.standingIndexMu.Unlock()
	_ = e.mem.DB().SetJSON(standingOrderStatePrefix+st.OrderID, st)
}

func (e *Engine) GetStandingOrderState(orderID string) (*StandingOrderState, error) {
	if e == nil || e.mem == nil || e.mem.DB() == nil || strings.TrimSpace(orderID) == "" {
		return nil, errors.New("market: standing order state unavailable")
	}
	var st StandingOrderState
	ok, err := e.mem.DB().GetJSON(standingOrderStatePrefix+strings.TrimSpace(orderID), &st)
	if err != nil {
		return nil, err
	}
	if !ok || st.OrderID == "" {
		return nil, errors.New("market: standing order not found")
	}
	return &st, nil
}

// MarkStandingOrderKilled marks a standing order as killed in SQLite KV when its workflow instance is cancelled.
func MarkStandingOrderKilled(db *store.DB, orderID string) {
	if db == nil || strings.TrimSpace(orderID) == "" {
		return
	}
	now := time.Now().Unix()
	var st StandingOrderState
	if ok, err := db.GetJSON(standingOrderStatePrefix+orderID, &st); err == nil && ok {
		st.Status = "killed"
		st.UpdatedAt = now
		_ = db.SetJSON(standingOrderStatePrefix+orderID, st)
	}
	var idx StandingOrderIndexEntry
	if ok, err := db.GetJSON(standingOrderIdxPrefix+orderID, &idx); err == nil && ok {
		if idx.Status == "watching" || idx.Status == "paused" || idx.Status == "matching" {
			idx.Status = "killed"
			idx.UpdatedAt = now
			_ = db.SetJSON(standingOrderIdxPrefix+orderID, idx)
		}
	}
	var list []StandingOrderIndexEntry
	if ok, err := db.GetJSON(standingOrdersKVKey, &list); err == nil && ok {
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
}

func (e *Engine) clearStandingOrderInbox(orderID string) {
	if e == nil || e.mem == nil || e.mem.DB() == nil || orderID == "" {
		return
	}
	e.standingInboxMu.Lock()
	defer e.standingInboxMu.Unlock()
	_ = e.mem.DB().DeleteKV(standingOrderInboxPrefix + orderID)
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

func (e *Engine) upsertStandingOrderIndex(st StandingOrderState) {
	if e == nil || e.mem == nil || e.mem.DB() == nil || st.OrderID == "" {
		return
	}
	e.standingIndexMu.Lock()
	defer e.standingIndexMu.Unlock()
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
	_ = e.mem.DB().SetJSON(standingOrderIdxPrefix+st.OrderID, entry)
}

// LoadStandingOrderIndex returns all persisted StandingOrder index entries without 128-entry slice truncation.
func LoadStandingOrderIndex(db *store.DB) []StandingOrderIndexEntry {
	if db == nil {
		return nil
	}
	seen := make(map[string]int)
	var out []StandingOrderIndexEntry
	if rows, err := db.ListKVPrefixRaw(standingOrderIdxPrefix, 0); err == nil {
		for _, raw := range rows {
			var entry StandingOrderIndexEntry
			if json.Unmarshal(raw, &entry) == nil && entry.OrderID != "" {
				seen[entry.OrderID] = len(out)
				out = append(out, entry)
			}
		}
	}
	var legacy []StandingOrderIndexEntry
	if ok, err := db.GetJSON(standingOrdersKVKey, &legacy); err == nil && ok {
		for _, entry := range legacy {
			if entry.OrderID == "" {
				continue
			}
			if idx, exists := seen[entry.OrderID]; exists {
				if entry.UpdatedAt > out[idx].UpdatedAt {
					out[idx] = entry
				}
				continue
			}
			seen[entry.OrderID] = len(out)
			out = append(out, entry)
		}
	}
	return out
}
