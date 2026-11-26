package market

import (
	"fmt"
	"strings"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/store"
)

const standingOrdersKVKey = "standing_orders_v1"

// StandingOrderInput starts a durable, crash-proof, live-steerable StandingOrder workflow
// ("Limit-Order for Knowledge") keyed by orderID. It sleeps at 0% CPU until a matching
// OfferAd arrives via restate.Signal, or until steered/cancelled/expired.
type StandingOrderInput struct {
	OrderID         string        `json:"orderId,omitempty"`
	Domains         []string      `json:"domains"`
	Query           string        `json:"query,omitempty"`
	MaxSats         uint64        `json:"maxSats"`
	TargetShard     string        `json:"targetShard,omitempty"`
	AgentID         string        `json:"agentId,omitempty"`
	TTL             time.Duration `json:"ttl,omitempty"`
	TTLSeconds      int64         `json:"ttlSeconds,omitempty"`
	MaxAttempts     int           `json:"maxAttempts,omitempty"`
	WaitTimeout     time.Duration `json:"waitTimeout,omitempty"`
	ApprovalTimeout time.Duration `json:"approvalTimeout,omitempty"`
}

// SteerStandingOrderInput live-steers a running StandingOrder without restarting it.
type SteerStandingOrderInput struct {
	MaxSats       *uint64 `json:"maxSats,omitempty"`
	Paused        *bool   `json:"paused,omitempty"`
	ExtendSeconds int64   `json:"extendSeconds,omitempty"`
	Reason        string  `json:"reason,omitempty"`
}

type standingOrderEvent struct {
	Kind   string                   `json:"kind"` // offer | steer | cancel
	Offer  *bstack.OfferAd          `json:"offer,omitempty"`
	Steer  *SteerStandingOrderInput `json:"steer,omitempty"`
	Reason string                   `json:"reason,omitempty"`
}

// StandingOrderState is the durable state persisted inside Restate & mirrored in SQLite KV
// so the Broker's SSE loop knows which orders to wake without polling Restate.
type StandingOrderState struct {
	OrderID       string   `json:"orderId"`
	InvocationID  string   `json:"invocationId,omitempty"`
	WaitingSeq    int      `json:"waitingSeq"`
	Status        string   `json:"status"` // watching | paused | matching | filled | cancelled | expired
	Domains       []string `json:"domains"`
	Query         string   `json:"query,omitempty"`
	MaxSats       uint64   `json:"maxSats"`
	TargetShard   string   `json:"targetShard,omitempty"`
	AgentID       string   `json:"agentId,omitempty"`
	ExpiresAtUnix int64    `json:"expiresAtUnix"`
	OffersSeen    int      `json:"offersSeen"`
	Attempts      int      `json:"attempts"`
	LastDealID    string   `json:"lastDealId,omitempty"`
	PaymentTxID   string   `json:"paymentTxid,omitempty"`
	DeliveredText string   `json:"deliveredText,omitempty"`
	Error         string   `json:"error,omitempty"`
	CreatedAt     int64    `json:"createdAt"`
	UpdatedAt     int64    `json:"updatedAt"`
}

// StandingOrderIndexEntry is the lightweight active-order record stored in SQLite KV
// so Hub SSE events match and wake sleeping Restate StandingOrders in O(N_active).
type StandingOrderIndexEntry struct {
	OrderID       string   `json:"orderId"`
	Domains       []string `json:"domains"`
	Query         string   `json:"query,omitempty"`
	MaxSats       uint64   `json:"maxSats"`
	TargetShard   string   `json:"targetShard,omitempty"`
	AgentID       string   `json:"agentId,omitempty"`
	Status        string   `json:"status"`
	ExpiresAtUnix int64    `json:"expiresAtUnix"`
	UpdatedAt     int64    `json:"updatedAt"`
}

func (e *Engine) restateStandingOrderRun(ctx restate.WorkflowContext, in StandingOrderInput) (StandingOrderState, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return StandingOrderState{}, err
	}
	orderID := restate.Key(ctx)
	invID := ""
	if req := ctx.Request(); req != nil {
		invID = req.ID
	}

	ttl := in.TTL
	if ttl <= 0 && in.TTLSeconds > 0 {
		ttl = time.Duration(in.TTLSeconds) * time.Second
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if ttl > 30*24*time.Hour {
		ttl = 30 * 24 * time.Hour
	}
	maxAttempts := in.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 16
	}

	initNow, err := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
		return time.Now().Unix(), nil
	}, restate.WithName("standing_order_init_time"))
	if err != nil {
		return StandingOrderState{}, err
	}

	doms := make([]string, 0, len(in.Domains))
	for _, d := range in.Domains {
		if s := strings.ToLower(strings.TrimSpace(d)); s != "" {
			doms = append(doms, s)
		}
	}
	if len(doms) == 0 {
		doms = []string{"ops"}
	}

	st := StandingOrderState{
		OrderID:       orderID,
		InvocationID:  invID,
		WaitingSeq:    1,
		Status:        "watching",
		Domains:       doms,
		Query:         strings.TrimSpace(in.Query),
		MaxSats:       in.MaxSats,
		TargetShard:   strings.TrimSpace(in.TargetShard),
		AgentID:       strings.TrimSpace(in.AgentID),
		ExpiresAtUnix: initNow + int64(ttl.Seconds()),
		CreatedAt:     initNow,
		UpdatedAt:     initNow,
	}
	restate.Set(ctx, "state", st)
	_, _ = restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
		e.upsertStandingOrderIndex(st)
		return true, nil
	}, restate.WithName("standing_order_register_index"))

	seenOffers := make(map[string]bool)

	for st.Status == "watching" || st.Status == "paused" {
		nowUnix, nErr := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
			return time.Now().Unix(), nil
		}, restate.WithName(fmt.Sprintf("standing_clock_%d", st.WaitingSeq)))
		if nErr != nil {
			return st, nErr
		}
		if nowUnix >= st.ExpiresAtUnix {
			st.Status = "expired"
			st.UpdatedAt = nowUnix
			break
		}
		remaining := time.Duration(st.ExpiresAtUnix-nowUnix) * time.Second
		if remaining <= 0 {
			remaining = time.Second
		}

		// Park at 0% CPU / 0 MB RAM waiting for the next named Signal OR TTL expiry.
		sigName := fmt.Sprintf("ev_%d", st.WaitingSeq)
		sigFut := restate.Signal[standingOrderEvent](ctx, sigName)
		expFut := restate.After(ctx, remaining)
		winner, wErr := restate.WaitFirst(ctx, sigFut, expFut)
		if wErr != nil {
			return st, wErr
		}
		if winner == expFut {
			expNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
				return time.Now().Unix(), nil
			}, restate.WithName(fmt.Sprintf("standing_expire_%d", st.WaitingSeq)))
			st.Status = "expired"
			st.UpdatedAt = expNow
			break
		}

		ev, sErr := sigFut.Result()
		if sErr != nil {
			return st, sErr
		}
		st.WaitingSeq++

		switch ev.Kind {
		case "cancel":
			cNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
				return time.Now().Unix(), nil
			}, restate.WithName(fmt.Sprintf("standing_cancel_%d", st.WaitingSeq)))
			st.Status = "cancelled"
			st.Error = strings.TrimSpace(ev.Reason)
			st.UpdatedAt = cNow
			restate.Set(ctx, "state", st)
			_, _ = restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
				e.upsertStandingOrderIndex(st)
				return true, nil
			}, restate.WithName("standing_cancel_index"))
			return st, nil

		case "steer":
			if ev.Steer != nil {
				if ev.Steer.MaxSats != nil && *ev.Steer.MaxSats > 0 {
					st.MaxSats = *ev.Steer.MaxSats
				}
				if ev.Steer.Paused != nil {
					if *ev.Steer.Paused {
						st.Status = "paused"
					} else if st.Status == "paused" {
						st.Status = "watching"
					}
				}
				if ev.Steer.ExtendSeconds > 0 {
					st.ExpiresAtUnix += ev.Steer.ExtendSeconds
				}
			}
			st.UpdatedAt = nowUnix
			restate.Set(ctx, "state", st)
			_, _ = restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
				e.upsertStandingOrderIndex(st)
				return true, nil
			}, restate.WithName(fmt.Sprintf("standing_steer_index_%d", st.WaitingSeq)))
			continue

		case "offer":
			if ev.Offer == nil || st.Status == "paused" {
				restate.Set(ctx, "state", st)
				continue
			}
			off := *ev.Offer
			if off.OfferID != "" && seenOffers[off.OfferID] {
				restate.Set(ctx, "state", st)
				continue
			}
			if off.OfferID != "" {
				seenOffers[off.OfferID] = true
			}
			st.OffersSeen++
			if st.MaxSats > 0 && off.PriceSats > st.MaxSats {
				st.UpdatedAt = nowUnix
				restate.Set(ctx, "state", st)
				continue
			}
			if !standingOrderDomainMatches(st.Domains, off.Domains) {
				st.UpdatedAt = nowUnix
				restate.Set(ctx, "state", st)
				continue
			}
			if st.Attempts >= maxAttempts {
				st.Status = "expired"
				st.Error = fmt.Sprintf("standing order reached max attempts (%d)", maxAttempts)
				st.UpdatedAt = nowUnix
				break
			}

			st.Attempts++
			dealID := fmt.Sprintf("so-%s-%d", orderID, st.Attempts)
			st.Status = "matching"
			st.LastDealID = dealID
			st.Error = ""
			st.UpdatedAt = nowUnix
			restate.Set(ctx, "state", st)

			wantAd := bstack.WantAd{
				WantID:  dealID,
				Domains: st.Domains,
				Query:   st.Query,
				MaxSats: st.MaxSats,
				Pub:     off.BuyerPub,
			}
			if wantAd.Pub == "" && e.mem != nil && e.mem.Chain() != nil {
				wantAd.Pub = e.mem.Chain().IdentityPubHex()
			}

			// Invoke BuyerDeal durably with deterministic internal auth headers.
			var reqOpts []restate.RequestOption
			if hdrs := e.internalCallHeaders(); hdrs != nil {
				reqOpts = append(reqOpts, restate.WithHeaders(hdrs))
			}
			dealRes, dErr := restate.Workflow[DealState](ctx, "BuyerDeal", dealID, "run").
				Request(BuyerDealInput{
					Want:            wantAd,
					Offers:          []bstack.OfferAd{off},
					TargetShard:     st.TargetShard,
					WaitTimeout:     in.WaitTimeout,
					ApprovalTimeout: in.ApprovalTimeout,
				}, reqOpts...)

			doneNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
				return time.Now().Unix(), nil
			}, restate.WithName(fmt.Sprintf("standing_attempt_done_%d", st.Attempts)))
			st.UpdatedAt = doneNow

			if dErr == nil && dealRes.Status == "settled" {
				st.Status = "filled"
				st.PaymentTxID = dealRes.PaymentTxID
				st.DeliveredText = dealRes.DeliveredTxt
				st.TargetShard = dealRes.Shard
				st.Error = ""
				break
			}
			// Seller failed or rejected — keep the StandingOrder alive for the next matching offer!
			st.Status = "watching"
			if dErr != nil {
				st.Error = dErr.Error()
			} else if dealRes.Error != "" {
				st.Error = dealRes.Error
			}
			restate.Set(ctx, "state", st)
		}
	}

	restate.Set(ctx, "state", st)
	_, _ = restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
		e.upsertStandingOrderIndex(st)
		return true, nil
	}, restate.WithName("standing_order_finalize_index"))
	return st, nil
}

func (e *Engine) restateStandingOrderOnOffer(ctx restate.WorkflowSharedContext, off bstack.OfferAd) (bool, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return false, err
	}
	st, err := restate.Get[StandingOrderState](ctx, "state")
	if err != nil || st.InvocationID == "" || (st.Status != "watching" && st.Status != "paused") {
		return false, nil
	}
	restate.ResolveSignal(ctx, st.InvocationID, fmt.Sprintf("ev_%d", st.WaitingSeq), standingOrderEvent{
		Kind:  "offer",
		Offer: &off,
	})
	return true, nil
}

func (e *Engine) restateStandingOrderSteer(ctx restate.WorkflowSharedContext, steer SteerStandingOrderInput) (bool, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return false, err
	}
	st, err := restate.Get[StandingOrderState](ctx, "state")
	if err != nil || st.InvocationID == "" || (st.Status != "watching" && st.Status != "paused") {
		return false, nil
	}
	restate.ResolveSignal(ctx, st.InvocationID, fmt.Sprintf("ev_%d", st.WaitingSeq), standingOrderEvent{
		Kind:  "steer",
		Steer: &steer,
	})
	return true, nil
}

func (e *Engine) restateStandingOrderCancel(ctx restate.WorkflowSharedContext, reason string) (bool, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return false, err
	}
	st, err := restate.Get[StandingOrderState](ctx, "state")
	if err != nil || st.InvocationID == "" || (st.Status != "watching" && st.Status != "paused") {
		return false, nil
	}
	restate.ResolveSignal(ctx, st.InvocationID, fmt.Sprintf("ev_%d", st.WaitingSeq), standingOrderEvent{
		Kind:   "cancel",
		Reason: reason,
	})
	return true, nil
}

func (e *Engine) restateStandingOrderStatus(ctx restate.WorkflowSharedContext, _ restate.Void) (StandingOrderState, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return StandingOrderState{}, err
	}
	return restate.Get[StandingOrderState](ctx, "state")
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
	list := LoadStandingOrderIndex(e.mem.DB())
	entry := StandingOrderIndexEntry{
		OrderID:       st.OrderID,
		Domains:       st.Domains,
		Query:         st.Query,
		MaxSats:       st.MaxSats,
		TargetShard:   st.TargetShard,
		AgentID:       st.AgentID,
		Status:        st.Status,
		ExpiresAtUnix: st.ExpiresAtUnix,
		UpdatedAt:     st.UpdatedAt,
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
