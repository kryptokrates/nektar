package market

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/cschleiden/go-workflows/workflow"
)

const (
	txStateKVPrefix       = "tx_state_v1:"
	anchorCadenceKVPrefix = "anchor_cadence_v1:"
)

// CustodyReserve serializes spend reservations atomically inside SQLite BEGIN IMMEDIATE (`ReserveSpendAtomic`),
// enforcing bounded ApprovedCapSats on operator overrides instead of infinite caps (STATE-7).
func (a *workflowActivities) CustodyReserve(ctx context.Context, in CustodyReserveInput) (CustodyReserveResult, error) {
	e := a.eng
	attempt := in.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	fj := e.FastJournal()
	if e.policy != nil {
		if fj != nil {
			fj.CachedPolicySync(e.mem.DB(), e.policy)
		} else if err := e.policy.Refresh(); err != nil {
			return CustodyReserveResult{}, err
		}
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
		// STATE-7 fix: bind override ceiling strictly to ApprovedCapSats (never ^uint64(0) infinite!).
		capBound := in.ApprovedCapSats
		if capBound == 0 {
			capBound = in.AmountSats
		}
		if capBound > maxDeal {
			maxDeal = capBound
		}
		maxDay += capBound
	}
	var bal uint64
	if fl := e.mem.Chain().FastLane(); fl != nil && fl.AvailableSats() >= in.AmountSats {
		bal = fl.AvailableSats()
	} else {
		var balErr error
		bal, balErr = e.mem.Chain().BalanceChecked(ctx)
		if balErr != nil {
			return CustodyReserveResult{}, fmt.Errorf("custody balance check failed: %w", balErr)
		}
		if bal < in.AmountSats {
			return CustodyReserveResult{Approved: false, BalanceSats: bal, Reason: "insufficient balance"}, nil
		}
	}
	if fj != nil {
		already, rErr := fj.ReserveSpendAtomic(in.OperationID, attempt, in.AmountSats, maxDeal, maxDay)
		if rErr != nil {
			return CustodyReserveResult{Approved: false, BalanceSats: bal, Reason: rErr.Error()}, nil
		}
		return CustodyReserveResult{
			Approved:        true,
			AlreadyReserved: already,
			BalanceSats:     bal - in.AmountSats,
		}, nil
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
}

func (a *workflowActivities) CustodyRelease(_ context.Context, in CustodyReleaseInput) (bool, error) {
	attempt := in.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	if fj := a.eng.FastJournal(); fj != nil {
		fj.ReleaseSpendReservation(in.OperationID, attempt)
		return true, nil
	}
	if a.eng.mem == nil || a.eng.mem.DB() == nil {
		return true, nil
	}
	if err := a.eng.mem.DB().ReleaseSpendReservation(in.OperationID, attempt); err != nil {
		return false, err
	}
	return true, nil
}

func (e *Engine) verifyMerklePathProof(ctx context.Context, txid, mpHex string, enforceDepth bool) (uint32, error) {
	if e.mem == nil || e.mem.Chain() == nil || e.mem.Chain().Network() == "memory" {
		return 0, nil
	}
	if strings.TrimSpace(mpHex) == "" {
		return 0, workflow.NewPermanentError(fmt.Errorf("nektar: merklePathHex proof is required to confirm/mine tx %s on live networks", txid))
	}
	mp, err := transaction.NewMerklePathFromHex(strings.TrimSpace(mpHex))
	if err != nil || mp == nil {
		return 0, workflow.NewPermanentError(fmt.Errorf("nektar: invalid merklePathHex for tx %s: %v", txid, err))
	}
	txHash, err := chainhash.NewHashFromHex(strings.TrimSpace(txid))
	if err != nil || txHash == nil {
		return 0, workflow.NewPermanentError(fmt.Errorf("nektar: invalid txid %q: %v", txid, err))
	}
	root, err := mp.ComputeRoot(txHash)
	if err != nil || root == nil {
		return 0, workflow.NewPermanentError(fmt.Errorf("nektar: BUMP ComputeRoot failed for %s: %v", txid, err))
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
		return 0, workflow.NewPermanentError(fmt.Errorf("nektar: REFUSED-BUMP merkle root invalid at height %d for tx %s", mp.BlockHeight, txid))
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
	if next == "mined" {
		return true
	}
	if cur == "mined" && next == "reorged" {
		return true
	}
	if cur == "mined" {
		return false
	}
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

// EmitTxEventLocal records a deduplicated, BUMP-verified monotonic tx lifecycle transition in SQLite KV.
func (e *Engine) EmitTxEventLocal(ctx context.Context, txid string, ev TxEventInput) (*TxState, error) {
	txid = strings.TrimSpace(txid)
	if txid == "" || strings.TrimSpace(ev.Kind) == "" {
		return nil, errors.New("nektar: txid and event.kind are required")
	}
	if ev.Kind == "mined" && e.mem != nil && e.mem.Chain() != nil && e.mem.Chain().Network() != "memory" {
		verifiedHeight, vErr := e.verifyMerklePathProof(ctx, txid, ev.MerklePathHex, false)
		if vErr != nil {
			return nil, vErr
		}
		if verifiedHeight > 0 {
			ev.BlockHeight = verifiedHeight
		}
	}
	if ev.ObservedAt == 0 {
		ev.ObservedAt = time.Now().Unix()
	}

	e.txStateMu.Lock()
	defer e.txStateMu.Unlock()

	st := TxState{TxID: txid}
	if e.mem != nil && e.mem.DB() != nil {
		_, _ = e.mem.DB().GetJSON(txStateKVPrefix+txid, &st)
		st.TxID = txid
	}
	if txEventExists(st.Events, ev) {
		return &st, nil
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
	if len(st.Events) > 128 {
		st.Events = st.Events[len(st.Events)-128:]
	}
	st.UpdatedAt = ev.ObservedAt
	if e.mem != nil && e.mem.DB() != nil {
		if err := e.mem.DB().SetJSON(txStateKVPrefix+txid, st); err != nil {
			return nil, err
		}
	}
	return &st, nil
}

// GetTxStateLocal reads the persisted TxState for txid from SQLite KV.
func (e *Engine) GetTxStateLocal(txid string) (*TxState, error) {
	txid = strings.TrimSpace(txid)
	st := TxState{TxID: txid}
	if e != nil && e.mem != nil && e.mem.DB() != nil && txid != "" {
		e.txStateMu.Lock()
		_, _ = e.mem.DB().GetJSON(txStateKVPrefix+txid, &st)
		e.txStateMu.Unlock()
	}
	return &st, nil
}

// TriggerAnchorCadenceLocal runs a serialized per-shard unanchored check & batch anchor,
// persisting status in SQLite KV without external HTTP RPCs.
func (e *Engine) TriggerAnchorCadenceLocal(ctx context.Context, shard string, intervalSec int64) (*AnchorCadenceState, error) {
	shard = strings.TrimSpace(shard)
	if shard == "" {
		return nil, errors.New("nektar: shard is required")
	}
	if e.mem == nil || e.mem.DB() == nil {
		return &AnchorCadenceState{Shard: shard}, nil
	}

	e.cadenceMu.Lock()
	var st AnchorCadenceState
	_, _ = e.mem.DB().GetJSON(anchorCadenceKVPrefix+shard, &st)
	st.Shard = shard
	e.cadenceMu.Unlock()

	now := time.Now().Unix()
	unanchored, uErr := e.mem.DB().ListUnanchored(shard)
	if uErr != nil {
		return nil, uErr
	}
	if len(unanchored) == 0 {
		st.LastRunAt = now
		st.DeferredReason = ""
		st.NextSlot = 0
		e.saveAnchorCadenceState(st)
		return &st, nil
	}
	// Enforce real cadence batching after the initial anchor run so high-TPS purchases/writes
	// never trigger synchronous per-fragment AnchorShard broadcasts inside intervalSec!
	if intervalSec > 0 && st.LastRunAt > 0 && now < st.LastRunAt+intervalSec {
		nextSlot := st.LastRunAt + intervalSec
		if st.NextSlot != nextSlot {
			st.NextSlot = nextSlot
			st.DeferredReason = "cadence_window"
			e.saveAnchorCadenceState(st)
		}
		return &st, nil
	}

	a, aErr := e.mem.AnchorShard(ctx, shard)
	if aErr != nil {
		msg := aErr.Error()
		if strings.Contains(msg, "insufficient") || strings.Contains(msg, "spendguard") || strings.Contains(msg, "no spendable") {
			st.LastRunAt = now
			st.DeferredReason = msg
			if intervalSec > 0 {
				st.NextSlot = (now/intervalSec + 1) * intervalSec
			}
			e.saveAnchorCadenceState(st)
			return &st, nil
		}
		return nil, aErr
	}

	e.cadenceMu.Lock()
	_, _ = e.mem.DB().GetJSON(anchorCadenceKVPrefix+shard, &st)
	st.Shard = shard
	st.LastRunAt = now
	st.DeferredReason = ""
	if a != nil && a.TxID != "" {
		st.LastTxID = a.TxID
		st.AnchorRuns++
	}
	if intervalSec > 0 {
		if rem, _ := e.mem.DB().ListUnanchored(shard); len(rem) > 0 {
			st.NextSlot = (now/intervalSec + 1) * intervalSec
		} else {
			st.NextSlot = 0
		}
	}
	_ = e.mem.DB().SetJSON(anchorCadenceKVPrefix+shard, st)
	e.cadenceMu.Unlock()

	if a != nil && a.TxID != "" {
		_, _ = e.EmitTxEventLocal(ctx, a.TxID, TxEventInput{
			Kind:       "broadcast",
			Shard:      shard,
			ObservedAt: now,
		})
	}
	return &st, nil
}

func (e *Engine) saveAnchorCadenceState(st AnchorCadenceState) {
	if e == nil || e.mem == nil || e.mem.DB() == nil || st.Shard == "" {
		return
	}
	e.cadenceMu.Lock()
	_ = e.mem.DB().SetJSON(anchorCadenceKVPrefix+st.Shard, st)
	e.cadenceMu.Unlock()
}

func (e *Engine) GetAnchorCadenceStatusLocal(shard string) (*AnchorCadenceState, error) {
	shard = strings.TrimSpace(shard)
	st := AnchorCadenceState{Shard: shard}
	if e != nil && e.mem != nil && e.mem.DB() != nil && shard != "" {
		e.cadenceMu.Lock()
		_, _ = e.mem.DB().GetJSON(anchorCadenceKVPrefix+shard, &st)
		e.cadenceMu.Unlock()
	}
	return &st, nil
}
