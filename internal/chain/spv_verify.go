package chain

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/lightwebinc/bcommon/guard"
)

const (
	// DefaultMaxUnconfirmedAncestors bounds counterparty 0-conf unmined ancestor chains.
	DefaultMaxUnconfirmedAncestors = 15
)

var (
	// ErrTrackerUnavailable indicates a header tracker transport/quorum outage (retryable, never 0-conf fallback).
	ErrTrackerUnavailable = errors.New("chain: header tracker unavailable")
	// ErrRefusedBUMP indicates a carried MerklePath failed verification against witnessed block headers.
	ErrRefusedBUMP = errors.New("chain: REFUSED-BUMP merkle path invalid")
	// ErrZeroConfCapExceeded indicates a 0-conf payment exceeded per-tx, aggregate, or ancestor policy caps.
	ErrZeroConfCapExceeded = errors.New("chain: zero-conf policy cap exceeded")
	// ErrInsufficientDepth indicates a mined transaction has not reached the required confirmation depth.
	ErrInsufficientDepth = errors.New("chain: mined transaction below required settleDepth")
	// ErrConflictDetected indicates a competing spend was observed for a watched 0-conf input outpoint.
	ErrConflictDetected = errors.New("chain: conflicting double-spend detected on watched input")
)

type watchedZeroConf struct {
	txid       string
	amountSats uint64
	inputs     []string
	addedAt    time.Time
	conflict   bool
}

// ZeroConfWatch tracks aggregate unconfirmed counterparty payment exposure and detects
// competing input spends (conflict watch) until each tx settles at settleDepth.
type ZeroConfWatch struct {
	mu          sync.Mutex
	byTxID      map[string]*watchedZeroConf
	byOutpoint  map[string]string // "txid:vout" -> watching txid
	conflicts   map[string]string // conflicted txid -> competing txid
	exposure    uint64
	defaultTTL  time.Duration
}

// NewZeroConfWatch creates a thread-safe 0-conf exposure and conflict tracker.
func NewZeroConfWatch() *ZeroConfWatch {
	return &ZeroConfWatch{
		byTxID:     make(map[string]*watchedZeroConf),
		byOutpoint: make(map[string]string),
		conflicts:  make(map[string]string),
		defaultTTL: 2 * time.Hour,
	}
}

// CheckAndTrack enforces conflict-freedom and aggregate unconfirmed exposure cap before accepting a 0-conf payment.
// If maxAggregateSats > 0 and adding amountSats would exceed it, returns ErrZeroConfCapExceeded without tracking.
func (w *ZeroConfWatch) CheckAndTrack(txid string, amountSats uint64, inputs []string, maxAggregateSats uint64) error {
	if w == nil || txid == "" {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneExpiredLocked(time.Now())

	if comp, conflicted := w.conflicts[txid]; conflicted {
		return fmt.Errorf("%w: tx %s conflicts with %s", ErrConflictDetected, txid, comp)
	}
	if _, already := w.byTxID[txid]; already {
		return nil
	}
	for _, op := range inputs {
		if op == "" {
			continue
		}
		if other, exists := w.byOutpoint[op]; exists && other != txid {
			w.conflicts[txid] = other
			w.conflicts[other] = txid
			if rec := w.byTxID[other]; rec != nil {
				rec.conflict = true
			}
			return fmt.Errorf("%w: outpoint %s already spent by %s", ErrConflictDetected, op, other)
		}
	}
	if maxAggregateSats > 0 && w.exposure+amountSats > maxAggregateSats {
		return fmt.Errorf("%w: aggregate unconfirmed exposure %d + %d exceeds cap %d sats",
			ErrZeroConfCapExceeded, w.exposure, amountSats, maxAggregateSats)
	}
	rec := &watchedZeroConf{
		txid:       txid,
		amountSats: amountSats,
		inputs:     append([]string(nil), inputs...),
		addedAt:    time.Now(),
	}
	w.byTxID[txid] = rec
	w.exposure += amountSats
	for _, op := range inputs {
		if op != "" {
			w.byOutpoint[op] = txid
		}
	}
	return nil
}

// MarkSettled removes txid from active 0-conf exposure once it reaches settleDepth.
func (w *ZeroConfWatch) MarkSettled(txid string) {
	if w == nil || txid == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.removeLocked(txid)
}

// RecordConflict flags txid (or any tx spending outpoint) as conflicted.
func (w *ZeroConfWatch) RecordConflict(outpoint, competingTxID string) {
	if w == nil || outpoint == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if other, ok := w.byOutpoint[outpoint]; ok && other != competingTxID {
		w.conflicts[other] = competingTxID
		w.conflicts[competingTxID] = other
		if rec := w.byTxID[other]; rec != nil {
			rec.conflict = true
		}
	}
}

// HasConflict reports whether txid has any observed competing spend.
func (w *ZeroConfWatch) HasConflict(txid string) bool {
	if w == nil || txid == "" {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.conflicts[txid]
	return ok
}

// Exposure returns current aggregate unconfirmed sats and active watched tx count.
func (w *ZeroConfWatch) Exposure() (sats uint64, count int) {
	if w == nil {
		return 0, 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneExpiredLocked(time.Now())
	return w.exposure, len(w.byTxID)
}

func (w *ZeroConfWatch) removeLocked(txid string) {
	rec, ok := w.byTxID[txid]
	if !ok {
		return
	}
	if w.exposure >= rec.amountSats {
		w.exposure -= rec.amountSats
	} else {
		w.exposure = 0
	}
	for _, op := range rec.inputs {
		if w.byOutpoint[op] == txid {
			delete(w.byOutpoint, op)
		}
	}
	delete(w.byTxID, txid)
}

func (w *ZeroConfWatch) pruneExpiredLocked(now time.Time) {
	for txid, rec := range w.byTxID {
		if now.Sub(rec.addedAt) > w.defaultTTL {
			w.removeLocked(txid)
		}
	}
}

// PaymentVerifyOpts configures strict SPV verification of an incoming counterparty BEEF payment.
type PaymentVerifyOpts struct {
	ExpectedAmountSats       uint64
	MaxUnconfirmedAncestors  int
	ZeroConfMaxSats          uint64
	ZeroConfMaxAggregateSats uint64
	SettleDepth              int
	AllowZeroConf            bool
	ConflictWatch            *ZeroConfWatch
	// TrackOnZeroConf registers the tx in ConflictWatch when ZeroConfEligible is true.
	TrackOnZeroConf bool
}

// PaymentVerifyResult summarizes the cryptographic SPV & confirmation policy verdict for a BEEF payment.
type PaymentVerifyResult struct {
	Tx                   *transaction.Transaction
	Beef                 *transaction.Beef
	TxID                 string
	Mined                bool
	BlockHeight          uint32
	Depth                int
	SettledAtDepth       bool
	UnconfirmedAncestors int
	ZeroConfEligible     bool
	NeedsConfirmation    bool
	EscalationReason     string
}

// VerifyCounterpartyPaymentBEEF runs full consensus SPV verification (go-sdk/spv.Verify:
// Merkle path roots against witnessed headers, input/output amounts & overspend, duplicate inputs,
// nLockTime/nSequence finality with 11-block MTP, 100-block coinbase maturity, 1000-block confiscation
// maturity, era-correct Bitcoin Script execution), enforces fail-closed REFUSED-BUMP and tracker
// outage separation, checks settleDepth when mined, and evaluates bounded 0-conf caps + conflict watch.
func VerifyCounterpartyPaymentBEEF(
	ctx context.Context,
	beefBytes []byte,
	tracker chaintracker.ChainTracker,
	opts PaymentVerifyOpts,
) (*PaymentVerifyResult, error) {
	if len(beefBytes) == 0 {
		return nil, errors.New("chain: empty payment BEEF")
	}
	if tracker == nil {
		return nil, fmt.Errorf("%w: nil chain tracker", ErrTrackerUnavailable)
	}
	beef, tx, _, err := guard.ParseBEEF(beefBytes, guard.DefaultBound)
	if err != nil || tx == nil {
		return nil, fmt.Errorf("chain: parse payment BEEF: %v", err)
	}
	if opts.MaxUnconfirmedAncestors <= 0 {
		opts.MaxUnconfirmedAncestors = DefaultMaxUnconfirmedAncestors
	}
	if opts.SettleDepth <= 0 {
		opts.SettleDepth = 1
	}

	txid := tx.TxID().String()
	res := &PaymentVerifyResult{
		Tx:   tx,
		Beef: beef,
		TxID: txid,
	}

	// Step 1: If the payment tx itself claims a mined BUMP, verify it strictly against witnessed headers
	// BEFORE anything else. Never swallow a bad BUMP or tracker error into a 0-conf fallback!
	if tx.MerklePath != nil {
		root, rErr := tx.MerklePath.ComputeRoot(tx.TxID())
		if rErr != nil || root == nil {
			return nil, fmt.Errorf("%w: compute root for %s: %v", ErrRefusedBUMP, txid, rErr)
		}
		valid, vErr := tracker.IsValidRootForHeight(ctx, root, tx.MerklePath.BlockHeight)
		if vErr != nil {
			return nil, fmt.Errorf("%w: verify root for %s at height %d: %w", ErrTrackerUnavailable, txid, tx.MerklePath.BlockHeight, vErr)
		}
		if !valid {
			return nil, fmt.Errorf("%w: merkle root %s invalid at height %d", ErrRefusedBUMP, root.String(), tx.MerklePath.BlockHeight)
		}
		tip, tErr := tracker.CurrentHeight(ctx)
		if tErr != nil {
			return nil, fmt.Errorf("%w: current height: %w", ErrTrackerUnavailable, tErr)
		}
		res.Mined = true
		res.BlockHeight = tx.MerklePath.BlockHeight
		if tip >= res.BlockHeight {
			res.Depth = int(tip-res.BlockHeight) + 1
		}
		res.SettledAtDepth = res.Depth >= opts.SettleDepth
	} else {
		// Ensure all unmined inputs have their SourceTransaction hydrated from the carried BEEF
		// (fail closed on thin/partial ancestry).
		if err := hydrateStrictFromBeef(tx, beef, make(map[string]bool)); err != nil {
			return nil, err
		}
	}

	// Step 2: Run full go-sdk/spv.Verify against the witnessed ChainTracker.
	// Note: spv.Verify checks mined BUMPs, unmined ancestry, finality (with MTP), coinbase/confiscation
	// maturity, duplicate inputs, out<=in overspend, and era-correct script execution!
	ok, sErr := spv.Verify(ctx, tx, tracker, nil)
	if sErr != nil {
		if isTrackerError(sErr) {
			return nil, fmt.Errorf("%w: %w", ErrTrackerUnavailable, sErr)
		}
		if errors.Is(sErr, spv.ErrInvalidMerklePath) {
			return nil, fmt.Errorf("%w: %w", ErrRefusedBUMP, sErr)
		}
		return nil, fmt.Errorf("chain: SPV verify %s failed: %w", txid, sErr)
	}
	if !ok {
		return nil, fmt.Errorf("chain: SPV verify %s returned false", txid)
	}

	if res.Mined {
		if !res.SettledAtDepth {
			res.NeedsConfirmation = true
			res.EscalationReason = fmt.Sprintf("mined at depth %d/%d (awaiting settleDepth=%d)", res.Depth, opts.SettleDepth, opts.SettleDepth)
		}
		if res.SettledAtDepth && opts.ConflictWatch != nil {
			opts.ConflictWatch.MarkSettled(txid)
		}
		return res, nil
	}

	// Step 3: Evaluate bounded 0-conf policy (ancestor depth, conflict watch, per-tx & aggregate caps).
	res.UnconfirmedAncestors = countUnconfirmedAncestors(tx, opts.MaxUnconfirmedAncestors+2)
	inputs := collectInputOutpoints(tx)
	if opts.ConflictWatch != nil && opts.ConflictWatch.HasConflict(txid) {
		return nil, fmt.Errorf("%w: tx %s", ErrConflictDetected, txid)
	}

	if !opts.AllowZeroConf || opts.ZeroConfMaxSats == 0 {
		res.NeedsConfirmation = true
		res.EscalationReason = "zero-conf disabled by policy (settleDepth confirmation required)"
		return res, nil
	}
	if opts.ExpectedAmountSats > opts.ZeroConfMaxSats {
		res.NeedsConfirmation = true
		res.EscalationReason = fmt.Sprintf("amount %d sats exceeds zeroConfMax %d sats", opts.ExpectedAmountSats, opts.ZeroConfMaxSats)
		return res, nil
	}
	if res.UnconfirmedAncestors > opts.MaxUnconfirmedAncestors {
		res.NeedsConfirmation = true
		res.EscalationReason = fmt.Sprintf("unconfirmed ancestor depth %d exceeds cap %d", res.UnconfirmedAncestors, opts.MaxUnconfirmedAncestors)
		return res, nil
	}
	maxAgg := opts.ZeroConfMaxAggregateSats
	if maxAgg == 0 {
		maxAgg = opts.ZeroConfMaxSats * 4
	}
	if opts.ConflictWatch != nil {
		if opts.TrackOnZeroConf {
			if cErr := opts.ConflictWatch.CheckAndTrack(txid, opts.ExpectedAmountSats, inputs, maxAgg); cErr != nil {
				if errors.Is(cErr, ErrConflictDetected) {
					return nil, cErr
				}
				res.NeedsConfirmation = true
				res.EscalationReason = cErr.Error()
				return res, nil
			}
		} else {
			curExp, _ := opts.ConflictWatch.Exposure()
			if curExp+opts.ExpectedAmountSats > maxAgg {
				res.NeedsConfirmation = true
				res.EscalationReason = fmt.Sprintf("aggregate unconfirmed exposure %d + %d exceeds cap %d sats", curExp, opts.ExpectedAmountSats, maxAgg)
				return res, nil
			}
		}
	}

	res.ZeroConfEligible = true
	return res, nil
}

// VerifyTransactionSPV runs full go-sdk/spv.Verify on a parsed transaction (used for Hub admission,
// bounty/faucet receipt verification, and shard pack import ancestry checks).
func VerifyTransactionSPV(ctx context.Context, tx *transaction.Transaction, beef *transaction.Beef, tracker chaintracker.ChainTracker) error {
	if tx == nil {
		return errors.New("chain: nil transaction")
	}
	if tracker == nil {
		return fmt.Errorf("%w: nil chain tracker", ErrTrackerUnavailable)
	}
	if tx.MerklePath == nil && beef != nil {
		if err := hydrateStrictFromBeef(tx, beef, make(map[string]bool)); err != nil {
			return err
		}
	}
	ok, err := spv.Verify(ctx, tx, tracker, nil)
	if err != nil {
		if isTrackerError(err) {
			return fmt.Errorf("%w: %w", ErrTrackerUnavailable, err)
		}
		if errors.Is(err, spv.ErrInvalidMerklePath) {
			return fmt.Errorf("%w: %w", ErrRefusedBUMP, err)
		}
		return err
	}
	if !ok {
		return errors.New("chain: SPV verify returned false")
	}
	return nil
}

func hydrateStrictFromBeef(tx *transaction.Transaction, beef *transaction.Beef, visiting map[string]bool) error {
	if tx == nil || tx.MerklePath != nil {
		return nil
	}
	id := tx.TxID().String()
	if visiting[id] {
		return errors.New("chain: cycle detected in transaction ancestry")
	}
	visiting[id] = true
	defer delete(visiting, id)

	for i, in := range tx.Inputs {
		if in == nil || in.SourceTXID == nil {
			return fmt.Errorf("chain: input %d missing source txid", i)
		}
		if in.SourceTransaction == nil && beef != nil {
			in.SourceTransaction = beef.FindTransaction(in.SourceTXID.String())
		}
		if in.SourceTransaction == nil {
			return fmt.Errorf("chain: input %d missing source transaction %s in BEEF", i, in.SourceTXID.String())
		}
		if in.SourceTransaction.MerklePath == nil {
			if err := hydrateStrictFromBeef(in.SourceTransaction, beef, visiting); err != nil {
				return err
			}
		}
	}
	return nil
}

func countUnconfirmedAncestors(tx *transaction.Transaction, capLimit int) int {
	if tx == nil || tx.MerklePath != nil {
		return 0
	}
	seen := make(map[string]bool)
	var walk func(cur *transaction.Transaction) int
	walk = func(cur *transaction.Transaction) int {
		if cur == nil || cur.MerklePath != nil {
			return 0
		}
		id := cur.TxID().String()
		if seen[id] {
			return 0
		}
		seen[id] = true
		if len(seen) >= capLimit {
			return len(seen)
		}
		for _, in := range cur.Inputs {
			if in != nil && in.SourceTransaction != nil && in.SourceTransaction.MerklePath == nil {
				walk(in.SourceTransaction)
				if len(seen) >= capLimit {
					return len(seen)
				}
			}
		}
		return len(seen)
	}
	return walk(tx)
}

func collectInputOutpoints(tx *transaction.Transaction) []string {
	if tx == nil {
		return nil
	}
	out := make([]string, 0, len(tx.Inputs))
	for _, in := range tx.Inputs {
		if in != nil && in.SourceTXID != nil {
			out = append(out, fmt.Sprintf("%s:%d", in.SourceTXID.String(), in.SourceTxOutIndex))
		}
	}
	return out
}

func isTrackerError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTrackerUnavailable) ||
		errors.Is(err, ErrHeaderQuorumUnavailable) ||
		errors.Is(err, ErrHeaderDisagreement) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) {
		return true
	}
	return false
}
