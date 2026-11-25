package market

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	restate "github.com/restatedev/sdk-go"
)

// restateCustodyReserve serializes ALL spend reservations on key "global" with a sub-ms SQLite transaction
// inside restate.Run (zero network I/O, idempotent on (OperationID, Attempt) — eliminates rt/key-collision & rt/double-effect).
func (e *Engine) restateCustodyReserve(ctx restate.ObjectContext, in CustodyReserveInput) (CustodyReserveResult, error) {
	attempt := in.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	res, err := restate.Run(ctx, func(rctx restate.RunContext) (CustodyReserveResult, error) {
		if e.mem != nil && e.mem.DB() != nil {
			_ = SyncPolicyFromDB(e.mem.DB(), e.policy)
		}
		kill, maxDeal, maxDay, _, _, _, _ := e.policy.Snapshot()
		if kill {
			return CustodyReserveResult{Approved: false, Reason: "market: operator kill switch is engaged"}, nil
		}
		if !in.BypassPolicy {
			if err := e.policy.CanSpendForDomains(in.SellerPub, in.Domains, 0); err != nil {
				return CustodyReserveResult{Approved: false, Reason: err.Error()}, nil
			}
		} else {
			// Even on operator override, explicitly blocked sellers stay blocked!
			if err := e.policy.CanSpend(in.SellerPub, 0); err != nil && strings.Contains(err.Error(), "blocked") {
				return CustodyReserveResult{Approved: false, Reason: err.Error()}, nil
			}
			// Explicit operator override bypasses automated deal/day caps while keeping 0 fail-closed otherwise.
			maxDeal = ^uint64(0)
			maxDay = ^uint64(0)
		}
		bal, balErr := e.mem.Chain().BalanceChecked(rctx)
		if balErr != nil {
			// Propagate wallet DB/RPC errors retryably rather than masking them as "insufficient balance"!
			return CustodyReserveResult{}, fmt.Errorf("custody balance check failed: %w", balErr)
		}
		if bal < in.AmountSats {
			return CustodyReserveResult{Approved: false, BalanceSats: bal, Reason: "insufficient balance"}, nil
		}
		already, rErr := e.mem.DB().ReserveSpendAtomic(in.OperationID, attempt, in.AmountSats, maxDeal, maxDay)
		if rErr != nil {
			return CustodyReserveResult{Approved: false, BalanceSats: bal, Reason: rErr.Error()}, nil
		}
		return CustodyReserveResult{
			Approved:        true,
			AlreadyReserved: already,
			BalanceSats:     bal - in.AmountSats,
		}, nil
	}, restate.WithName("custody_reserve_atomic"))
	if err != nil {
		return CustodyReserveResult{}, err
	}
	if res.Approved {
		restate.Set(ctx, "last_op", fmt.Sprintf("%s#%d", in.OperationID, attempt))
	}
	return res, nil
}

func (e *Engine) restateCustodyRelease(ctx restate.ObjectContext, in CustodyReleaseInput) (bool, error) {
	attempt := in.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	return restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
		if e.mem == nil || e.mem.DB() == nil {
			return true, nil
		}
		if err := e.mem.DB().ReleaseSpendReservation(in.OperationID, attempt); err != nil {
			return false, err
		}
		return true, nil
	}, restate.WithName("custody_release_atomic"))
}

func (e *Engine) restateCustodyBalance(ctx restate.ObjectSharedContext, _ restate.Void) (uint64, error) {
	return restate.Run(ctx, func(_ restate.RunContext) (uint64, error) {
		return e.mem.Chain().BalanceSats(), nil
	}, restate.WithName("custody_balance"))
}

func (e *Engine) verifyMerklePathProof(ctx context.Context, txid, mpHex string, enforceDepth bool) (uint32, error) {
	if e.mem == nil || e.mem.Chain() == nil || e.mem.Chain().Network() == "memory" {
		return 0, nil
	}
	if strings.TrimSpace(mpHex) == "" {
		return 0, restate.TerminalErrorf("nektar: merklePathHex proof is required to confirm/mine tx %s on live networks", txid)
	}
	mp, err := transaction.NewMerklePathFromHex(strings.TrimSpace(mpHex))
	if err != nil || mp == nil {
		return 0, restate.TerminalErrorf("nektar: invalid merklePathHex for tx %s: %v", txid, err)
	}
	txHash, err := chainhash.NewHashFromHex(strings.TrimSpace(txid))
	if err != nil || txHash == nil {
		return 0, restate.TerminalErrorf("nektar: invalid txid %q: %v", txid, err)
	}
	root, err := mp.ComputeRoot(txHash)
	if err != nil || root == nil {
		return 0, restate.TerminalErrorf("nektar: BUMP ComputeRoot failed for %s: %v", txid, err)
	}
	tracker := e.mem.Chain().Tracker()
	if tracker == nil {
		return 0, fmt.Errorf("nektar: header tracker unavailable while verifying tx %s", txid)
	}
	valid, vErr := tracker.IsValidRootForHeight(ctx, root, mp.BlockHeight)
	if vErr != nil {
		return 0, fmt.Errorf("nektar: header tracker error verifying tx %s: %w", txid, vErr)
	}
	if !valid {
		return 0, restate.TerminalErrorf("nektar: REFUSED-BUMP merkle root invalid at height %d for tx %s", mp.BlockHeight, txid)
	}
	if enforceDepth {
		tip, tErr := tracker.CurrentHeight(ctx)
		if tErr != nil {
			return 0, fmt.Errorf("nektar: header tracker height error for tx %s: %w", txid, tErr)
		}
		if tip < mp.BlockHeight || int(tip-mp.BlockHeight)+1 < e.mem.Chain().SettleDepth() {
			return 0, fmt.Errorf("nektar: tx %s at height %d has not reached settleDepth %d (tip=%d)", txid, mp.BlockHeight, e.mem.Chain().SettleDepth(), tip)
		}
	}
	return mp.BlockHeight, nil
}

func txStatusRank(status string) int {
	switch status {
	case "prepared":
		return 1
	case "submission_unknown":
		return 2
	case "broadcast":
		return 3
	case "mempool":
		return 4
	case "mined":
		return 5
	default:
		return 0
	}
}

func shouldAdvanceTxStatus(cur, next string) bool {
	if next == "" {
		return false
	}
	if cur == "" {
		return true
	}
	// Revival edge: a previously failed/aborted/reorged tx that actually mined on-chain ALWAYS advances to mined!
	if next == "mined" {
		return true
	}
	// Reorg edge: a mined tx orphaned by a verified chain reorg rolls back to "reorged" (never stuck permanently mined!).
	if cur == "mined" && next == "reorged" {
		return true
	}
	// Never regress a mined tx to pre-mined broadcast/mempool!
	if cur == "mined" {
		return false
	}
	// Terminal failure/abort can overwrite any pre-mined status.
	if next == "failed" || next == "aborted" {
		return cur != "failed" && cur != "aborted"
	}
	if cur == "failed" || cur == "aborted" {
		return false
	}
	if cur == "reorged" {
		return next == "broadcast" || next == "mempool" || next == "mined" || next == "failed" || next == "aborted"
	}
	return txStatusRank(next) > txStatusRank(cur)
}

func txEventExists(events []TxEventInput, ev TxEventInput) bool {
	for _, prev := range events {
		if prev.Kind == ev.Kind && prev.Shard == ev.Shard && prev.BlockHeight == ev.BlockHeight && prev.Detail == ev.Detail {
			return true
		}
	}
	return false
}

// restateTxEmit enforces content deduplication, verified BUMP proofs on live "mined" transitions,
// reorg rollback edges, monotonic lifecycle progression, revival-on-mine, and deterministic timestamping.
func (e *Engine) restateTxEmit(ctx restate.ObjectContext, ev TxEventInput) (TxState, error) {
	txid := restate.Key(ctx)
	st, err := restate.Get[TxState](ctx, "tx")
	if err != nil {
		return TxState{}, err
	}
	st.TxID = txid
	if txEventExists(st.Events, ev) {
		return st, nil
	}
	// On live networks, refuse unverified bare "mined" status strings without a header-verified BUMP!
	if ev.Kind == "mined" && e.mem != nil && e.mem.Chain() != nil && e.mem.Chain().Network() != "memory" {
		verifiedHeight, vErr := restate.Run(ctx, func(rctx restate.RunContext) (uint32, error) {
			return e.verifyMerklePathProof(rctx, txid, ev.MerklePathHex, false)
		}, restate.WithName("tx_emit_verify_bump"))
		if vErr != nil {
			return st, vErr
		}
		if verifiedHeight > 0 {
			ev.BlockHeight = verifiedHeight
		}
	}
	obsAt := ev.ObservedAt
	if obsAt == 0 {
		obsAt, err = restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
			return time.Now().Unix(), nil
		}, restate.WithName("tx_emit_timestamp"))
		if err != nil {
			return TxState{}, err
		}
		ev.ObservedAt = obsAt
	}
	if shouldAdvanceTxStatus(st.Status, ev.Kind) {
		st.Status = ev.Kind
		if ev.Kind == "reorged" {
			st.BlockHeight = 0
		}
	}
	if ev.Kind != "reorged" && ev.BlockHeight > st.BlockHeight {
		st.BlockHeight = ev.BlockHeight
	}
	st.Events = append(st.Events, ev)
	// Bound event list length to prevent unbounded Restate KV growth on noisy retries.
	if len(st.Events) > 128 {
		st.Events = st.Events[len(st.Events)-128:]
	}
	st.UpdatedAt = obsAt
	restate.Set(ctx, "tx", st)
	return st, nil
}

func (e *Engine) restateTxState(ctx restate.ObjectSharedContext, _ restate.Void) (TxState, error) {
	return restate.Get[TxState](ctx, "tx")
}

type cadenceStepResult struct {
	NowUnix        int64  `json:"nowUnix"`
	TxID           string `json:"txid,omitempty"`
	HasMore        bool   `json:"hasMore"`
	NextSlot       int64  `json:"nextSlot,omitempty"`
	DelaySec       int64  `json:"delaySec,omitempty"`
	DeferredReason string `json:"deferredReason,omitempty"`
}

// restateAnchorCadenceTrigger checks SQLite unanchored fragments inside restate.Run, anchors at most
// one batch, emits a content-idempotent Tx/emit event, and self-arms ONLY when work remains using a
// quantized slot idempotency key (eliminates rt/arm-multiplication & rt/poll-as-mechanism!).
func (e *Engine) restateAnchorCadenceTrigger(ctx restate.ObjectContext, in AnchorCadenceInput) (AnchorCadenceState, error) {
	shard := restate.Key(ctx)
	st, err := restate.Get[AnchorCadenceState](ctx, "cadence")
	if err != nil {
		return AnchorCadenceState{}, err
	}
	st.Shard = shard

	step, runErr := restate.Run(ctx, func(rctx restate.RunContext) (cadenceStepResult, error) {
		now := time.Now().Unix()
		unanchored, uErr := e.mem.DB().ListUnanchored(shard)
		if uErr != nil {
			return cadenceStepResult{}, uErr
		}
		if len(unanchored) == 0 {
			return cadenceStepResult{NowUnix: now, HasMore: false}, nil
		}
		a, aErr := e.mem.AnchorShard(rctx, shard)
		if aErr != nil {
			msg := aErr.Error()
			// Classify deterministic / wallet-empty errors so we don't burn retries on a dry wallet;
			// transient broadcast/network errors bubble out to Restate's 5-attempt PauseOnMaxAttempts policy.
			if strings.Contains(msg, "insufficient") || strings.Contains(msg, "spendguard") || strings.Contains(msg, "no spendable") {
				out := cadenceStepResult{NowUnix: now, HasMore: true, DeferredReason: msg}
				if in.IntervalSeconds > 0 {
					out.NextSlot = (now/in.IntervalSeconds + 1) * in.IntervalSeconds
					out.DelaySec = out.NextSlot - now
					if out.DelaySec < 1 {
						out.DelaySec = in.IntervalSeconds
					}
				}
				return out, nil
			}
			return cadenceStepResult{}, aErr
		}
		txid := ""
		if a != nil {
			txid = a.TxID
		}
		rem, _ := e.mem.DB().ListUnanchored(shard)
		out := cadenceStepResult{
			NowUnix: now,
			TxID:    txid,
			HasMore: len(rem) > 0,
		}
		if in.IntervalSeconds > 0 && out.HasMore {
			out.NextSlot = (now/in.IntervalSeconds + 1) * in.IntervalSeconds
			out.DelaySec = out.NextSlot - now
			if out.DelaySec < 1 {
				out.DelaySec = in.IntervalSeconds
			}
		}
		return out, nil
	}, restate.WithName("anchor_cadence_step"))
	if runErr != nil {
		return st, runErr
	}

	if step.TxID != "" {
		st.LastTxID = step.TxID
		st.AnchorRuns++
		restate.ObjectSend(ctx, "Tx", step.TxID, "emit").Send(
			TxEventInput{Kind: "broadcast", Shard: shard, ObservedAt: step.NowUnix},
			restate.WithIdempotencyKey("tx:"+step.TxID+":broadcast:0"),
		)
	}
	st.LastRunAt = step.NowUnix
	st.NextSlot = step.NextSlot
	st.DeferredReason = step.DeferredReason
	restate.Set(ctx, "cadence", st)

	if in.IntervalSeconds > 0 && step.HasMore && step.NextSlot > 0 {
		restate.ObjectSend(ctx, "AnchorCadence", shard, "trigger").Send(
			in,
			restate.WithDelay(time.Duration(step.DelaySec)*time.Second),
			restate.WithIdempotencyKey(fmt.Sprintf("anchor:%s:%d", shard, step.NextSlot)),
		)
	}
	return st, nil
}

func (e *Engine) restateAnchorCadenceStatus(ctx restate.ObjectSharedContext, _ restate.Void) (AnchorCadenceState, error) {
	return restate.Get[AnchorCadenceState](ctx, "cadence")
}

// --- NektarRuntime authenticated ingress gateway to private Tx & AnchorCadence objects ---

func txEventIdemKey(txid string, ev TxEventInput) string {
	h := sha256.Sum256([]byte(ev.Shard + "|" + ev.Detail))
	return fmt.Sprintf("tx:%s:%s:%d:%x", txid, ev.Kind, ev.BlockHeight, h[:6])
}

func (e *Engine) restateRuntimeEmitTx(ctx restate.Context, in RuntimeTxEmitInput) (bool, error) {
	if err := e.checkInternalAuth(ctx); err != nil {
		return false, err
	}
	if strings.TrimSpace(in.TxID) == "" || strings.TrimSpace(in.Event.Kind) == "" {
		return false, restate.TerminalErrorf("txid and event.kind are required")
	}
	restate.ObjectSend(ctx, "Tx", in.TxID, "emit").Send(
		in.Event,
		restate.WithIdempotencyKey(txEventIdemKey(in.TxID, in.Event)),
	)
	return true, nil
}

func (e *Engine) restateRuntimeTxState(ctx restate.Context, txid string) (TxState, error) {
	if err := e.checkInternalAuth(ctx); err != nil {
		return TxState{}, err
	}
	return restate.Object[TxState](ctx, "Tx", txid, "state").Request(restate.Void{})
}

func (e *Engine) restateRuntimeTriggerAnchor(ctx restate.Context, in RuntimeAnchorTriggerInput) (bool, error) {
	if err := e.checkInternalAuth(ctx); err != nil {
		return false, err
	}
	if strings.TrimSpace(in.Shard) == "" {
		return false, restate.TerminalErrorf("shard is required")
	}
	var opts []restate.SendOption
	if in.Slot > 0 {
		opts = append(opts, restate.WithIdempotencyKey(fmt.Sprintf("anchor:%s:%d", in.Shard, in.Slot)))
	}
	restate.ObjectSend(ctx, "AnchorCadence", in.Shard, "trigger").Send(
		AnchorCadenceInput{IntervalSeconds: in.IntervalSeconds},
		opts...,
	)
	return true, nil
}

func (e *Engine) restateRuntimeAnchorStatus(ctx restate.Context, shard string) (AnchorCadenceState, error) {
	if err := e.checkInternalAuth(ctx); err != nil {
		return AnchorCadenceState{}, err
	}
	return restate.Object[AnchorCadenceState](ctx, "AnchorCadence", shard, "status").Request(restate.Void{})
}
