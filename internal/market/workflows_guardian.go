package market

import (
	"fmt"
	"time"

	restate "github.com/restatedev/sdk-go"
)

// EscrowGuardianInput starts a durable Restate EscrowGuardian workflow (keyed by escrowTxID)
// that guarantees DisclosureSale buyer refunds automatically once RefundAfter elapses,
// even across multi-day laptop sleeps or process restarts.
type EscrowGuardianInput struct {
	DealID       string `json:"dealId"`
	EscrowTxID   string `json:"escrowTxid"`
	RefundAfter  uint64 `json:"refundAfter"`  // block height (<500M) or unix timestamp (>=500M)
	CheckEveryMs int64  `json:"checkEveryMs"` // optional override for tests (default 60s)
}

// EscrowGuardianState tracks the durable lifecycle of an EscrowGuardian protection workflow.
type EscrowGuardianState struct {
	DealID      string `json:"dealId"`
	EscrowTxID  string `json:"escrowTxid"`
	RefundAfter uint64 `json:"refundAfter"`
	Status      string `json:"status"` // watching | claimed | key_extracted | refunded | failed
	RefundTxID  string `json:"refundTxid,omitempty"`
	Error       string `json:"error,omitempty"`
	UpdatedAt   int64  `json:"updatedAt"`
}

type escrowCheckResult struct {
	NowUnix       int64  `json:"nowUnix"`
	Done          bool   `json:"done"`
	Status        string `json:"status"`
	RefundTxID    string `json:"refundTxid,omitempty"`
	NextWaitSec   int64  `json:"nextWaitSec"`
	Error         string `json:"error,omitempty"`
}

func (e *Engine) restateEscrowGuardianRun(ctx restate.WorkflowContext, in EscrowGuardianInput) (EscrowGuardianState, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return EscrowGuardianState{}, err
	}
	escTxID := restate.Key(ctx)
	if escTxID == "" {
		escTxID = in.EscrowTxID
	}
	checkEvery := 60 * time.Second
	if in.CheckEveryMs > 0 {
		checkEvery = time.Duration(in.CheckEveryMs) * time.Millisecond
	}

	st := EscrowGuardianState{
		DealID:      in.DealID,
		EscrowTxID:  escTxID,
		RefundAfter: in.RefundAfter,
		Status:      "watching",
	}
	restate.Set(ctx, "state", st)

	claimedFut := restate.Promise[bool](ctx, "claimed")

	// Loop durably across sleeps until either Seller claims (markClaimed) or ReconcileEscrowObligations
	// extracts the on-chain revealed key / broadcasts DisclosureSale.Refund() back to BSV Desktop.
	for attempt := 0; attempt < 2048; attempt++ {
		chk, err := restate.Run(ctx, func(rctx restate.RunContext) (escrowCheckResult, error) {
			now := time.Now().Unix()
			if e.mem == nil || e.mem.DB() == nil || e.mem.Chain() == nil {
				return escrowCheckResult{NowUnix: now, Done: true, Status: "failed", Error: "memory/chain unavailable"}, nil
			}
			// Check if deal already settled cleanly.
			if in.DealID != "" {
				if d, dErr := e.mem.DB().GetDeal(in.DealID); dErr == nil && d.Status == "settled" {
					return escrowCheckResult{NowUnix: now, Done: true, Status: "claimed"}, nil
				}
			}
			rep, rErr := ReconcileEscrowObligations(rctx, e.mem.DB(), e.mem.Chain(), now)
			if rErr != nil {
				return escrowCheckResult{NowUnix: now, NextWaitSec: int64(checkEvery.Seconds()), Error: rErr.Error()}, nil
			}
			if rep.KeysExtractedOnChain > 0 {
				return escrowCheckResult{NowUnix: now, Done: true, Status: "key_extracted"}, nil
			}
			if rep.RefundsBroadcast > 0 {
				refTx := ""
				if len(rep.RefundTxIDs) > 0 {
					refTx = rep.RefundTxIDs[0]
				}
				return escrowCheckResult{NowUnix: now, Done: true, Status: "refunded", RefundTxID: refTx}, nil
			}
			// Compute smart sleep duration: if unix timestamp locktime is still in the future, sleep directly until RefundAfter!
			waitSec := int64(checkEvery.Seconds())
			if waitSec <= 0 {
				waitSec = 1
			}
			if in.RefundAfter >= 500_000_000 && int64(in.RefundAfter) > now {
				rem := int64(in.RefundAfter) - now
				if rem > waitSec {
					waitSec = rem
				}
			}
			return escrowCheckResult{NowUnix: now, Done: false, Status: "watching", NextWaitSec: waitSec}, nil
		}, restate.WithName(fmt.Sprintf("escrow_guardian_check_%d", attempt)))
		if err != nil {
			return st, err
		}

		st.UpdatedAt = chk.NowUnix
		st.Error = chk.Error
		if chk.Done {
			st.Status = chk.Status
			st.RefundTxID = chk.RefundTxID
			restate.Set(ctx, "state", st)
			return st, nil
		}
		restate.Set(ctx, "state", st)

		waitDur := time.Duration(chk.NextWaitSec) * time.Second
		if in.CheckEveryMs > 0 && time.Duration(in.CheckEveryMs)*time.Millisecond < waitDur {
			waitDur = time.Duration(in.CheckEveryMs) * time.Millisecond
		}
		timer := restate.After(ctx, waitDur)
		winner, wErr := restate.WaitFirst(ctx, claimedFut, timer)
		if wErr != nil {
			return st, wErr
		}
		if winner == claimedFut {
			st.Status = "claimed"
			restate.Set(ctx, "state", st)
			return st, nil
		}
	}

	st.Status = "failed"
	st.Error = "escrow guardian max checks exceeded"
	restate.Set(ctx, "state", st)
	return st, nil
}

func (e *Engine) restateEscrowGuardianMarkClaimed(ctx restate.WorkflowSharedContext, _ restate.Void) (bool, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return false, err
	}
	if err := restate.Promise[bool](ctx, "claimed").Resolve(true); err != nil {
		return false, nil
	}
	return true, nil
}

func (e *Engine) restateEscrowGuardianStatus(ctx restate.WorkflowSharedContext, _ restate.Void) (EscrowGuardianState, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return EscrowGuardianState{}, err
	}
	return restate.Get[EscrowGuardianState](ctx, "state")
}
