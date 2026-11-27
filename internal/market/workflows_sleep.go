package market

import (
	"context"
	"encoding/json"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/kryptokrates/nektar/internal/distill"
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
	TotalSuperseded  int64                 `json:"totalSuperseded"`
	PriceDecayed     int                   `json:"priceDecayed,omitempty"`
	LastSummary      *distill.CycleSummary `json:"lastSummary,omitempty"`
	LastError        string                `json:"lastError,omitempty"`
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
	sum, err := d.RunCycle(ctx, distill.RunOpts{
		Shards:   in.Shards,
		MaxBatch: in.MaxBatch,
	})
	if err != nil {
		st.Status = "error"
		st.LastError = err.Error()
		return st, err
	}

	// Apply dynamic-pricing idle decay across active listings during sleep cycle.
	decayed := e.applyDynamicPriceIdleDecay(now)

	st.Status = "completed"
	st.TotalRuns = 1
	st.TotalDistilled = int64(sum.Distilled)
	st.TotalQuarantined = int64(sum.Quarantined)
	st.TotalDropped = int64(sum.DroppedLowSignal)
	st.TotalSuperseded = int64(sum.Superseded)
	st.PriceDecayed = decayed
	st.LastSummary = sum
	if e.mem.DB() != nil {
		_ = e.mem.DB().SetJSON("sleep_cycle_last_state_v1", st)
	}
	return st, nil
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

func (e *Engine) restateSleepCycleRun(ctx restate.ObjectContext, in SleepCycleInput) (SleepCycleState, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return SleepCycleState{}, err
	}
	prev, _ := restate.Get[SleepCycleState](ctx, "state")
	res, err := restate.Run(ctx, func(rctx restate.RunContext) (SleepCycleState, error) {
		out, rErr := e.RunSleepCycle(rctx, in)
		if out == nil {
			return SleepCycleState{Status: "error"}, rErr
		}
		return *out, rErr
	}, restate.WithName("sleep_cycle_distill"))
	if err != nil {
		prev.Status = "error"
		prev.LastError = err.Error()
		restate.Set(ctx, "state", prev)
		return prev, err
	}
	if res.Status == "completed" {
		prev.TotalRuns++
		prev.TotalDistilled += res.TotalDistilled
		prev.TotalQuarantined += res.TotalQuarantined
		prev.TotalDropped += res.TotalDropped
		prev.TotalSuperseded += res.TotalSuperseded
		prev.PriceDecayed = res.PriceDecayed
		prev.LastSummary = res.LastSummary
		prev.LastError = ""
	}
	prev.Status = res.Status
	prev.LastRunAt = res.LastRunAt
	restate.Set(ctx, "state", prev)
	return prev, nil
}

func (e *Engine) restateSleepCycleTriggerNow(ctx restate.ObjectContext, in SleepCycleInput) (SleepCycleState, error) {
	in.Force = true
	return e.restateSleepCycleRun(ctx, in)
}

func (e *Engine) restateSleepCycleStatus(ctx restate.ObjectSharedContext, _ restate.Void) (SleepCycleState, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return SleepCycleState{}, err
	}
	return restate.Get[SleepCycleState](ctx, "state")
}
