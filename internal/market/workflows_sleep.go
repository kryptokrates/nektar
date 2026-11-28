package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/distill"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/store"
)

const (
	DefaultSleepIdleSeconds    int64 = 120
	DefaultSleepIntervalSec    int64 = 900
	SleepCycleObjectKeyDefault       = "default"
)

// SleepCycleInput configures a Restate SleepCycle distillation + dynamic-price decay pass.
type SleepCycleInput struct {
	IdleThresholdSeconds int64    `json:"idleThresholdSeconds,omitempty"`
	MaxBatch             int      `json:"maxBatch,omitempty"`
	Shards               []string `json:"shards,omitempty"`
	AutoAnchor           bool     `json:"autoAnchor,omitempty"`
	AutoList             bool     `json:"autoList,omitempty"`
	Force                bool     `json:"force,omitempty"` // bypass idle check (manual trigger)
}

// SleepCycleState persists durable SleepCycle telemetry in the Restate SleepCycle Virtual Object.
type SleepCycleState struct {
	Status           string                `json:"status"` // idle_deferred | completed | error
	LastRunAt        int64                 `json:"lastRunAt"`
	TotalRuns        int64                 `json:"totalRuns"`
	TotalDistilled   int64                 `json:"totalDistilled"`
	TotalQuarantined int64                 `json:"totalQuarantined"`
	TotalDropped     int64                 `json:"totalDropped"`
	TotalSuperseded         int64                 `json:"totalSuperseded"`
	PriceDecayed            int                   `json:"priceDecayed,omitempty"`
	DemandPrioritizedShards []string              `json:"demandPrioritizedShards,omitempty"`
	AutoWantsPosted         int                   `json:"autoWantsPosted,omitempty"`
	AutoWantIDs             []string              `json:"autoWantIds,omitempty"`
	LastSummary             *distill.CycleSummary `json:"lastSummary,omitempty"`
	LastError               string                `json:"lastError,omitempty"`
}

// RunSleepCycle executes one bounded distillation + dynamic-pricing decay pass directly (used by
// both Restate SleepCycle VO inside restate.Run and manual POST /v1/distill/run).
func (e *Engine) RunSleepCycle(ctx context.Context, in SleepCycleInput) (*SleepCycleState, error) {
	now := time.Now().Unix()
	st := &SleepCycleState{LastRunAt: now}
	if e == nil || e.mem == nil {
		st.Status = "error"
		st.LastError = "memory service unavailable"
		return st, nil
	}
	idleThresh := in.IdleThresholdSeconds
	if idleThresh <= 0 {
		idleThresh = DefaultSleepIdleSeconds
	}
	if !in.Force {
		idle := e.mem.IdleSeconds()
		if e.mem.LastActivityUnix() > 0 && idle < idleThresh {
			st.Status = "idle_deferred"
			return st, nil
		}
	}

	autoList := in.AutoList
	if !autoList && e.policy != nil {
		kill, _, _, _, _, _, sellerOK := e.policy.Snapshot()
		if !kill && sellerOK {
			autoList = true
		}
	}
	d, err := distill.NewDistiller(e.mem, distill.Config{
		DefaultDomain: "ops",
		MaxBatch:      in.MaxBatch,
		AutoAnchor:    in.AutoAnchor,
		AutoList:      autoList,
		JevEval:       e.mem.JevEvaluator(),
	})
	if err != nil {
		st.Status = "error"
		st.LastError = err.Error()
		return st, err
	}
	targetShards := in.Shards
	var prioritized []string
	if len(targetShards) == 0 && e.mem.DB() != nil {
		targetShards, prioritized = prioritizeWorkingShardsByDemand(e.mem.DB(), now)
	}
	sum, err := d.RunCycle(ctx, distill.RunOpts{
		Shards:   targetShards,
		MaxBatch: in.MaxBatch,
	})
	if err != nil {
		st.Status = "error"
		st.LastError = err.Error()
		return st, err
	}

	// If newly distilled market-* shards were auto-listed, re-announce to Hubs so waiting buyers discover supply immediately.
	if autoList && len(sum.MarketShards) > 0 {
		if br := e.getBroker(); br != nil && br.Discovery() != nil && len(br.Discovery().ActiveHosts()) > 0 {
			br.spawnTracked(15*time.Second, func(aCtx context.Context) {
				_, _ = br.AnnounceSelf(aCtx)
			})
		}
	}

	// Apply dynamic-pricing idle decay across active listings during sleep cycle.
	decayed := e.applyDynamicPriceIdleDecay(now)

	// Closed-loop Demand Trigger (strictly opt-in via AutoWantOnMiss + Watchdog + SpendGuard caps):
	// promote repeated local recall misses into bounded StandingOrders / WantAds.
	autoWantIDs := e.processRecallMissAutoWants(ctx, now)

	st.Status = "completed"
	st.TotalRuns = 1
	st.TotalDistilled = int64(sum.Distilled)
	st.TotalQuarantined = int64(sum.Quarantined)
	st.TotalDropped = int64(sum.DroppedLowSignal)
	st.TotalSuperseded = int64(sum.Superseded)
	st.PriceDecayed = decayed
	st.DemandPrioritizedShards = prioritized
	st.AutoWantsPosted = len(autoWantIDs)
	st.AutoWantIDs = autoWantIDs
	st.LastSummary = sum
	if e.mem.DB() != nil {
		_ = e.mem.DB().SetJSON("sleep_cycle_last_state_v1", st)
	}
	return st, nil
}

// prioritizeWorkingShardsByDemand scores local working shards against recent overlay unmatched_wants
// so SleepCycle distills & lists shards with active buyer demand first.
func prioritizeWorkingShardsByDemand(db *store.DB, nowUnix int64) (ordered []string, demandMatched []string) {
	allShards, err := db.ListShards()
	if err != nil {
		return nil, nil
	}
	var working []string
	for _, sh := range allShards {
		if distill.IsWorkingShard(sh) {
			working = append(working, sh)
		}
	}
	if len(working) <= 1 {
		return working, nil
	}
	wants, err := db.ListUnmatchedWants(50)
	if err != nil || len(wants) == 0 {
		sort.Strings(working)
		return working, nil
	}
	cutoff := nowUnix - 86400
	scores := make(map[string]int64, len(working))
	for _, sh := range working {
		low := strings.ToLower(strings.TrimSpace(sh))
		tokens := strings.FieldsFunc(low, func(r rune) bool {
			return r == '-' || r == '_' || r == '/' || r == '.' || r == ' '
		})
		for _, w := range wants {
			if w.SeenAt > 0 && w.SeenAt < cutoff {
				continue
			}
			wDom := strings.ToLower(strings.TrimSpace(w.Domain))
			wQuery := strings.ToLower(strings.TrimSpace(w.Query))
			weight := int64(10) + int64(min(w.MaxSats, 5000)/100)
			if wDom != "" && (wDom == low || strings.Contains(low, wDom) || strings.Contains(wDom, low)) {
				scores[sh] += weight * 3
				continue
			}
			for _, tok := range tokens {
				if len(tok) >= 3 && (tok == wDom || strings.Contains(wQuery, tok)) {
					scores[sh] += weight
					break
				}
			}
		}
	}
	sort.Slice(working, func(i, j int) bool {
		si, sj := scores[working[i]], scores[working[j]]
		if si != sj {
			return si > sj
		}
		return working[i] < working[j]
	})
	for _, sh := range working {
		if scores[sh] > 0 {
			demandMatched = append(demandMatched, sh)
		}
	}
	return working, demandMatched
}

// processRecallMissAutoWants turns repeated local recall misses into bounded StandingOrders / WantAds
// ONLY when explicitly enabled in operator policy (AutoWantOnMiss=true), Watchdog permits spend, and caps hold.
func (e *Engine) processRecallMissAutoWants(ctx context.Context, nowUnix int64) []string {
	if e == nil || e.mem == nil || e.mem.DB() == nil || e.policy == nil {
		return nil
	}
	autoCfg := e.policy.AutonomyConfig()
	if !autoCfg.AutoWantOnMiss {
		return nil
	}
	kill, maxDeal, maxDay, _, spent, _, _ := e.policy.Snapshot()
	if kill || maxDeal == 0 || maxDay == 0 || spent >= maxDay {
		return nil
	}
	br := e.getBroker()
	if br != nil && br.Watchdog() != nil {
		if err := br.Watchdog().AllowAutonomousSpend(); err != nil {
			return nil
		}
	}
	postedToday, err := e.mem.DB().CountAutoWantsSince(nowUnix - 86400)
	if err != nil || postedToday >= autoCfg.AutoWantMaxPerDay {
		return nil
	}
	remaining := autoCfg.AutoWantMaxPerDay - postedToday
	misses, err := e.mem.DB().ListEligibleRecallMisses(autoCfg.AutoWantMissThreshold, nowUnix-86400, nowUnix-86400, remaining)
	if err != nil || len(misses) == 0 {
		return nil
	}
	budget := autoCfg.AutoWantBudgetSats
	if budget > maxDeal {
		budget = maxDeal
	}
	if remDay := maxDay - spent; budget > remDay {
		budget = remDay
	}
	if budget == 0 {
		return nil
	}

	var posted []string
	for _, m := range misses {
		if ctx.Err() != nil {
			break
		}
		dom := strings.ToLower(strings.TrimSpace(m.Shard))
		dom = strings.TrimPrefix(dom, "market-")
		dom = strings.TrimPrefix(dom, "ont-")
		if dom == "" || dom == "default" {
			dom = "general"
		}
		if err := e.policy.CanSpendForDomains("", []string{dom}, budget); err != nil {
			continue
		}
		// RT-7 fix: deterministic wantID derived from (topicKey, UTC dayBucket) so retries on the same day are idempotent!
		dayBucket := nowUnix / 86400
		topicHash := hex.EncodeToString(core.SHA256([]byte(m.TopicKey)))[:12]
		wantID := fmt.Sprintf("autowant-%s-%d", topicHash, dayBucket)
		if br != nil {
			if wc := br.WorkflowClient(); wc != nil && wc.IsRunning() {
				st, sErr := br.StartStandingOrder(ctx, StandingOrderInput{
					OrderID:     wantID,
					Domains:     []string{dom},
					Query:       m.Query,
					MaxSats:     budget,
					MinGrade:    string(nmu.GradeB),
					PricingMode: string(nmu.ModeSpot),
					TTLSeconds:  86400,
				})
				if sErr == nil && st != nil && st.OrderID != "" {
					wantID = st.OrderID
				}
			} else {
				_ = br.PublishWantForStandingOrderNMU(ctx, StandingOrderState{
					OrderID:       wantID,
					Domains:       []string{dom},
					Query:         m.Query,
					MaxSats:       budget,
					MinGrade:      string(nmu.GradeB),
					PricingMode:   string(nmu.ModeSpot),
					ExpiresAtUnix: nowUnix + 86400,
				})
			}
		}
		if err := e.mem.DB().MarkRecallMissWanted(m.TopicKey, wantID, nowUnix); err == nil {
			posted = append(posted, wantID)
		}
	}
	return posted
}

func (e *Engine) applyDynamicPriceIdleDecay(nowUnix int64) int {
	if e == nil || e.mem == nil || e.mem.DB() == nil {
		return 0
	}
	listings, err := e.mem.DB().ListSellListings()
	if err != nil {
		return 0
	}
	decayed := 0
	for _, l := range listings {
		if !l.Active || len(l.DynamicPricingJSON) == 0 {
			continue
		}
		dp := ParseDynamicPriceState(l.DynamicPricingJSON, l.PriceSats)
		if dp.ApplyIdleDecay(l.PriceSats, nowUnix) {
			if raw, mErr := json.Marshal(dp); mErr == nil {
				if uErr := e.mem.DB().UpdateSellListingPricing(l.Shard, dp.CurrentPriceSats, raw); uErr == nil {
					decayed++
				}
			}
		}
	}
	return decayed
}

// LoadSleepCycleState reads the latest persisted SleepCycleState from SQLite KV.
func LoadSleepCycleState(mem *memory.Service) SleepCycleState {
	var st SleepCycleState
	if mem != nil && mem.DB() != nil {
		_, _ = mem.DB().GetJSON("sleep_cycle_last_state_v1", &st)
	}
	if st.Status == "" {
		st.Status = "idle"
	}
	return st
}

