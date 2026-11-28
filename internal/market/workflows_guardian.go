package market

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cschleiden/go-workflows/workflow"
)

const escrowGuardianStatePrefix = "escrow_guardian_state_v1:"

// EscrowGuardianInput starts a durable EscrowGuardianWorkflow (keyed by "escrow-"+escrowTxID)
// that guarantees DisclosureSale buyer key extraction or Refund() automatically once RefundAfter elapses,
// even across multi-week locktimes, laptop sleeps, or process restarts (RT-8: zero artificial loop cap).
type EscrowGuardianInput struct {
	DealID                      string               `json:"dealId"`
	EscrowTxID                  string               `json:"escrowTxid"`
	RefundAfter                 uint64               `json:"refundAfter"`  // block height (<500M) or unix timestamp (>=500M)
	CheckEveryMs                int64                `json:"checkEveryMs"` // optional override for tests (default 60s)
	ResumeState                 *EscrowGuardianState `json:"resumeState,omitempty"`
	MaxIterationsBeforeRollover int                  `json:"maxIterationsBeforeRollover,omitempty"`
}

// EscrowGuardianState tracks the durable lifecycle of an EscrowGuardianWorkflow.
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

func EscrowGuardianWorkflow(ctx workflow.Context, in EscrowGuardianInput) (EscrowGuardianState, error) {
	var acts *workflowActivities
	instID := workflow.WorkflowInstance(ctx).InstanceID
	escTxID := strings.TrimPrefix(instID, escrowWorkflowPrefix)
	if escTxID == "" {
		escTxID = in.EscrowTxID
	}
	checkEvery := 60 * time.Second
	if in.CheckEveryMs > 0 {
		checkEvery = time.Duration(in.CheckEveryMs) * time.Millisecond
	}

	claimedCh := workflow.NewSignalChannel[bool](ctx, "claimed")

	const defaultGuardianRolloverIterations = 256
	maxRollover := in.MaxIterationsBeforeRollover
	if maxRollover <= 0 {
		maxRollover = defaultGuardianRolloverIterations
	}

	var st EscrowGuardianState
	if in.ResumeState != nil {
		st = *in.ResumeState
		st.EscrowTxID = escTxID
	} else {
		st = EscrowGuardianState{
			DealID:      in.DealID,
			EscrowTxID:  escTxID,
			RefundAfter: in.RefundAfter,
			Status:      "watching",
			UpdatedAt:   workflow.Now(ctx).Unix(),
		}
		_, _ = workflow.ExecuteActivity[bool](ctx, singleAttemptActivityOpts(), acts.EscrowGuardianSaveState, st).Get(ctx)
	}

	// Loop durably without any artificial ceiling (RT-8), compacting workflows.sqlite history via ContinueAsNew (RFC-003).
	for loopIter := 0; ; loopIter++ {
		if loopIter >= maxRollover {
			// Drain claimedCh before ContinueAsNew so a queued claimed signal is never lost across rollover.
			if v, ok := claimedCh.ReceiveNonBlocking(); ok && v {
				st.Status = "claimed"
				st.UpdatedAt = workflow.Now(ctx).Unix()
				_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.EscrowGuardianSaveState, st).Get(ctx)
				return st, nil
			}
			stCopy := st
			nextIn := in
			nextIn.ResumeState = &stCopy
			return st, workflow.ContinueAsNew(ctx, nextIn)
		}
		chk, err := workflow.ExecuteActivity[escrowCheckResult](ctx, dealActivityOpts(), acts.EscrowGuardianCheck, in.DealID, escTxID, in.RefundAfter, int64(checkEvery.Seconds())).Get(ctx)
		if err != nil {
			return st, err
		}

		st.UpdatedAt = chk.NowUnix
		st.Error = chk.Error
		if chk.Done {
			if chk.Status == "key_extracted" && chk.ExtractedKeyHex != "" && in.DealID != "" {
				_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.EscrowGuardianDeliverExtractedKey, in.DealID, ClaimSignal{
					ClaimTxID:     chk.ExtractedClaimTxID,
					ContentKeyHex: chk.ExtractedKeyHex,
				}).Get(ctx)
			}
			st.Status = chk.Status
			st.RefundTxID = chk.RefundTxID
			_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.EscrowGuardianSaveState, st).Get(ctx)
			return st, nil
		}
		_, _ = workflow.ExecuteActivity[bool](ctx, singleAttemptActivityOpts(), acts.EscrowGuardianSaveState, st).Get(ctx)

		waitDur := time.Duration(chk.NextWaitSec) * time.Second
		if waitDur <= 0 {
			waitDur = time.Second
		}
		if in.CheckEveryMs > 0 && time.Duration(in.CheckEveryMs)*time.Millisecond < waitDur {
			waitDur = time.Duration(in.CheckEveryMs) * time.Millisecond
		}

		tCtx, cancelTimer := workflow.WithCancel(ctx)
		timerFut := workflow.ScheduleTimer(tCtx, waitDur)
		claimed := false
		workflow.Select(ctx,
			workflow.Receive(claimedCh, func(_ workflow.Context, v bool, ok bool) {
				cancelTimer()
				if ok && v {
					claimed = true
				}
			}),
			workflow.Await(timerFut, func(_ workflow.Context, _ workflow.Future[any]) {
			}),
		)
		cancelTimer()
		if claimed {
			st.Status = "claimed"
			st.UpdatedAt = workflow.Now(ctx).Unix()
			_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.EscrowGuardianSaveState, st).Get(ctx)
			return st, nil
		}
	}
}

func (a *workflowActivities) EscrowGuardianSaveState(_ context.Context, st EscrowGuardianState) (bool, error) {
	a.eng.saveEscrowGuardianState(st)
	return true, nil
}

func (a *workflowActivities) EscrowGuardianDeliverExtractedKey(ctx context.Context, dealID string, sig ClaimSignal) (bool, error) {
	if wc := a.eng.WorkflowClient(); wc != nil {
		return wc.DeliverBuyerKey(ctx, dealID, sig)
	}
	if a.eng.mem != nil && a.eng.mem.DB() != nil {
		if rec, err := a.eng.mem.DB().GetDeal(dealID); err == nil {
			return a.eng.recoverLateBuyerClaim(ctx, rec, sig)
		}
	}
	return false, nil
}

func (a *workflowActivities) EscrowGuardianCheck(ctx context.Context, dealID, escTxID string, refundAfter uint64, defaultWaitSec int64) (escrowCheckResult, error) {
	e := a.eng
	now := time.Now().Unix()
	if e.mem == nil || e.mem.DB() == nil || e.mem.Chain() == nil {
		return escrowCheckResult{NowUnix: now, Done: true, Status: "failed", Error: "memory/chain unavailable"}, nil
	}
	if dealID != "" {
		if d, dErr := e.mem.DB().GetDeal(dealID); dErr == nil {
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
	waitSec := defaultWaitSec
	if waitSec <= 0 {
		waitSec = 60
	}
	rep, rErr := ReconcileSingleEscrowObligation(ctx, e.mem.DB(), e.mem.Chain(), dealID, escTxID, now)
	if rErr != nil {
		return escrowCheckResult{NowUnix: now, NextWaitSec: waitSec, Error: rErr.Error()}, nil
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
		return escrowCheckResult{NowUnix: now, Done: false, Status: "refund_broadcast", RefundTxID: refTx, NextWaitSec: waitSec}, nil
	}
	if refundAfter >= 500_000_000 && int64(refundAfter) > now {
		rem := int64(refundAfter) - now
		if rem > waitSec {
			waitSec = rem
		}
	} else if refundAfter > 0 && refundAfter < 500_000_000 {
		if curHeight, hErr := e.mem.Chain().Height(ctx); hErr == nil && uint64(curHeight) < refundAfter {
			est := int64(refundAfter-uint64(curHeight)) * 60
			if est > 600 {
				est = 600
			}
			if est > waitSec {
				waitSec = est
			}
		}
	}
	return escrowCheckResult{NowUnix: now, Done: false, Status: "watching", NextWaitSec: waitSec}, nil
}

func (e *Engine) saveEscrowGuardianState(st EscrowGuardianState) {
	if e == nil || e.mem == nil || e.mem.DB() == nil || st.EscrowTxID == "" {
		return
	}
	_ = e.mem.DB().SetJSON(escrowGuardianStatePrefix+st.EscrowTxID, st)
}

func (e *Engine) GetEscrowGuardianState(escrowTxID string) (*EscrowGuardianState, error) {
	if e == nil || e.mem == nil || e.mem.DB() == nil || strings.TrimSpace(escrowTxID) == "" {
		return nil, errors.New("market: escrow guardian state unavailable")
	}
	var st EscrowGuardianState
	ok, err := e.mem.DB().GetJSON(escrowGuardianStatePrefix+strings.TrimSpace(escrowTxID), &st)
	if err != nil {
		return nil, err
	}
	if !ok || st.EscrowTxID == "" {
		return nil, errors.New("market: escrow guardian not found")
	}
	return &st, nil
}
