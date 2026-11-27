package chain

import (
	"context"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/lightwebinc/bcommon/guard"

	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

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
	res.UnconfirmedAncestors = nwallet.CountUnminedAncestors(tx, opts.MaxUnconfirmedAncestors+2)
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
		if opts.SettleDepth > 1 && tracker != nil {
			if tip, tErr := tracker.CurrentHeight(ctx); tErr == nil && tip > 0 {
				opts.ConflictWatch.SweepSettled(tip, opts.SettleDepth)
			}
		}
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
