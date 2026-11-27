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
	NowUnix            int64  `json:"nowUnix"`
	Done               bool   `json:"done"`
	Status             string `json:"status"`
	RefundTxID         string `json:"refundTxid,omitempty"`
	ExtractedKeyHex    string `json:"extractedKeyHex,omitempty"`
	ExtractedClaimTxID string `json:"extractedClaimTxid,omitempty"`
	NextWaitSec        int64  `json:"nextWaitSec"`
	Error              string `json:"error,omitempty"`
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
			// Check if deal already reached a terminal or externally-reconciled state (prevents 34h zombie loop).
			if in.DealID != "" {
				if d, dErr := e.mem.DB().GetDeal(in.DealID); dErr == nil {
					switch d.Status {
					case "settled":
						return escrowCheckResult{NowUnix: now, Done: true, Status: "claimed"}, nil
					case "refunded":
						return escrowCheckResult{NowUnix: now, Done: true, Status: "refunded"}, nil
					case "key_extracted_onchain":
						return escrowCheckResult{NowUnix: now, Done: true, Status: "key_extracted", ExtractedKeyHex: d.ClaimKeyHex}, nil
					}
				}
			}
			// Reconcile STRICTLY this escrowTxID (never cross-contaminate with other open escrows!).
			rep, rErr := ReconcileSingleEscrowObligation(rctx, e.mem.DB(), e.mem.Chain(), in.DealID, escTxID, now)
			if rErr != nil {
				return escrowCheckResult{NowUnix: now, NextWaitSec: int64(checkEvery.Seconds()), Error: rErr.Error()}, nil
			}
			if rep.KeysExtractedOnChain > 0 {
				return escrowCheckResult{
					NowUnix:            now,
					Done:               true,
					Status:             "key_extracted",
					ExtractedKeyHex:    rep.ExtractedKeyHex,
					ExtractedClaimTxID: rep.ExtractedClaimTxID,
				}, nil
			}
			if rep.RefundsSettled > 0 {
				refTx := ""
				if len(rep.RefundTxIDs) > 0 {
					refTx = rep.RefundTxIDs[0]
				}
				return escrowCheckResult{NowUnix: now, Done: true, Status: "refunded", RefundTxID: refTx}, nil
			}
			if rep.RefundsBroadcast > 0 || rep.RefundPendingTxID != "" {
				refTx := rep.RefundPendingTxID
				if refTx == "" && len(rep.RefundTxIDs) > 0 {
					refTx = rep.RefundTxIDs[0]
				}
				waitSec := int64(checkEvery.Seconds())
				if waitSec <= 0 {
					waitSec = 1
				}
				return escrowCheckResult{NowUnix: now, Done: false, Status: "refund_broadcast", RefundTxID: refTx, NextWaitSec: waitSec}, nil
			}
			// Compute smart sleep duration for BOTH Bitcoin locktime domains:
			//  - Unix timestamp (>= 500M): sleep directly until RefundAfter
			//  - Block height (< 500M): estimate blocks remaining * 60s clamped to [60s, 600s]
			//    so multi-day/multi-week block-height locktimes never exhaust the loop cap.
			waitSec := int64(checkEvery.Seconds())
			if waitSec <= 0 {
				waitSec = 1
			}
			if in.RefundAfter >= 500_000_000 && int64(in.RefundAfter) > now {
				rem := int64(in.RefundAfter) - now
				if rem > waitSec {
					waitSec = rem
				}
			} else if in.RefundAfter > 0 && in.RefundAfter < 500_000_000 {
				if curHeight, hErr := e.mem.Chain().Height(rctx); hErr == nil && uint64(curHeight) < in.RefundAfter {
					est := int64(in.RefundAfter-uint64(curHeight)) * 60
					if est > 600 {
						est = 600
					}
					if est > waitSec {
						waitSec = est
					}
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
			// If seller's revealed content key k was extracted on-chain, durably wake the waiting
			// BuyerDeal workflow via deliverKey so the buyer completes decryption & settlement!
			if chk.Status == "key_extracted" && chk.ExtractedKeyHex != "" && in.DealID != "" {
				var dOpts []restate.SendOption
				if hdrs := e.internalCallHeaders(); hdrs != nil {
					dOpts = append(dOpts, restate.WithHeaders(hdrs))
				}
				restate.WorkflowSend(ctx, "BuyerDeal", in.DealID, "deliverKey").Send(ClaimSignal{
					ClaimTxID:     chk.ExtractedClaimTxID,
					ContentKeyHex: chk.ExtractedKeyHex,
				}, dOpts...)
			}
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
