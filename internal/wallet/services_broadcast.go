package wallet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk"
)

func (s *bcommonServices) PostFromBEEF(ctx context.Context, beef *transaction.Beef, txids []string) (wdk.PostFromBeefResult, error) {
	if !s.hasBroadcaster() {
		return nil, ErrNoSettler
	}
	name := "bcommon"
	if s.cfg.Settler != nil {
		name = s.cfg.Settler.Name()
	} else if s.cfg.Broadcast != nil {
		name = "testchain"
	}

	posted := &wdk.PostedBEEF{
		TxIDResults: make([]wdk.PostedTxID, 0, len(txids)),
	}
	var firstErr error
	loader := s.dbLoader(ctx)
	for _, id := range txids {
		tx := findAndHydrateTx(beef, id, s.txCache, loader, s.maxDepth())
		if tx == nil {
			err := fmt.Errorf("wallet services: tx %s missing from BEEF", id)
			if firstErr == nil {
				firstErr = err
			}
			posted.TxIDResults = append(posted.TxIDResults, wdk.PostedTxID{
				TxID:   id,
				Result: wdk.PostedTxIDResultError,
				Error:  err,
			})
			continue
		}

		// Exempt verified incoming non-outgoing transactions from the receiver's SpendGuard so
		// go-wallet-toolbox BackgroundBroadcast never charges the buyer's change against the seller.
		incoming := isIncomingInternalize(ctx) || (s.isIncomingTx != nil && s.isIncomingTx(id))

		// Single post-signing SpendGuard chokepoint for outgoing spends.
		if !incoming && s.cfg.SpendGuard != nil && !isOverrideGuard(ctx) {
			feeSats := computeTxFee(tx)
			var outSum uint64
			for _, o := range tx.Outputs {
				outSum += o.Satoshis
			}
			var externalOut uint64
			hint := externalSpendFromCtx(ctx)
			switch {
			case hint.set && hint.sweepAll:
				externalOut = outSum
			case hint.set:
				externalOut = hint.sats
			default:
				if ownedSum, ok := func() (uint64, bool) {
					if s.ownedOutputSum != nil {
						return s.ownedOutputSum(id)
					}
					return 0, false
				}(); ok {
					if outSum > ownedSum {
						externalOut = outSum - ownedSum
					}
				} else if planned, ok := s.lookupPlannedSpend(id); ok {
					externalOut = planned
				} else {
					// Fail-closed: unknown direct broadcast path counts full output sum.
					externalOut = outSum
				}
			}
			spendSats := externalOut + feeSats
			if gErr := s.cfg.SpendGuard(id, feeSats, spendSats, false); gErr != nil {
				rejErr := fmt.Errorf("%w: %w", ErrSpendRejected, gErr)
				s.setLastErr(id, rejErr)
				postErrSinkFromCtx(ctx).record(id, rejErr)
				if len(posted.TxIDResults) > 0 {
					if firstErr == nil {
						firstErr = rejErr
					}
					posted.TxIDResults = append(posted.TxIDResults, wdk.PostedTxID{
						TxID:   id,
						Result: wdk.PostedTxIDResultError,
						Error:  rejErr,
					})
					continue
				}
				return nil, rejErr
			}
		}

		var bErr error
		if s.cfg.Broadcast != nil {
			bErr = s.cfg.Broadcast(ctx, tx)
		} else {
			bErr = s.cfg.Settler.Submit(ctx, tx)
		}

		if bErr != nil {
			lower := strings.ToLower(bErr.Error())
			switch {
			case strings.Contains(lower, "already in mempool") || strings.Contains(lower, "already known") || strings.Contains(lower, "txn-already-known"):
				s.txCache.Put(id, tx)
				posted.TxIDResults = append(posted.TxIDResults, wdk.PostedTxID{
					TxID:         id,
					Result:       wdk.PostedTxIDResultAlreadyKnown,
					AlreadyKnown: true,
				})
			case isUncertainBroadcastError(bErr):
				// Transport/timeout/5xx/TCP/Arcade-RECEIVED-timeout: outcome is UNKNOWN.
				// Never refund SpendGuard or release UTXO reservations.
				unkErr := fmt.Errorf("%w: %w", ErrBroadcastUnknown, bErr)
				s.setLastErr(id, unkErr)
				postErrSinkFromCtx(ctx).record(id, unkErr)
				if len(posted.TxIDResults) > 0 {
					if firstErr == nil {
						firstErr = unkErr
					}
					posted.TxIDResults = append(posted.TxIDResults, wdk.PostedTxID{
						TxID:   id,
						Result: wdk.PostedTxIDResultError,
						Error:  unkErr,
					})
					continue
				}
				return nil, unkErr
			default:
				// Treat broadcaster error strings (including "utxo_spent"/"double_spend"/"400")
				// strictly as observational hints: NEVER mark terminal PostedTxIDResultDoubleSpend
				// or refund SpendGuard post-submit without a verified competing BUMP.
				s.setLastErr(id, bErr)
				postErrSinkFromCtx(ctx).record(id, bErr)
				if firstErr == nil {
					firstErr = bErr
				}
				posted.TxIDResults = append(posted.TxIDResults, wdk.PostedTxID{
					TxID:   id,
					Result: wdk.PostedTxIDResultError,
					Error:  bErr,
				})
			}
			continue
		}

		// Broadcast succeeded; cache hydrated tx. Only run instant post-broadcast proof lookup on
		// ephemeral testchain (where Broadcast auto-mines synchronously); skip wasted live roundtrips.
		item := wdk.PostedTxID{
			TxID:   id,
			Result: wdk.PostedTxIDResultSuccess,
		}
		if s.cfg.ProofLookup != nil {
			if mp, h, ok, _ := s.lookupProof(ctx, id); ok && mp != nil {
				tx.MerklePath = mp
				item.MerklePath = mp
				item.BlockHeight = h
				if hdr, hErr := s.ChainHeaderByHeight(ctx, h); hErr == nil && hdr != nil {
					item.BlockHash = hdr.Hash
				}
			}
		}
		s.txCache.Put(id, tx)
		posted.TxIDResults = append(posted.TxIDResults, item)
	}

	return wdk.PostFromBeefResult{
		{
			Name:             name,
			PostedBEEFResult: posted,
			Error:            firstErr,
		},
	}, nil
}

// isNotEnoughFunds reports whether err indicates insufficient wallet funds.
func isNotEnoughFunds(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrInsufficient) || errors.Is(err, wdk.ErrNotEnoughFunds) {
		return true
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "not enough funds") || strings.Contains(low, "insufficient")
}

// isUncertainBroadcastError reports whether err is a timeout, canceled context, network transport
// failure, or 5xx response where the transaction MAY have been accepted by the network.
func isUncertainBroadcastError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrBroadcastUnknown) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	low := strings.ToLower(err.Error())
	for _, tok := range []string{
		"timeout", "deadline exceeded", "eof", "connection reset", "connection refused",
		"broken pipe", "http 500", "http 502", "http 503", "http 504", "status 500", "status 502", "status 503", "status 504",
		"temporary failure", "context canceled",
	} {
		if strings.Contains(low, tok) {
			return true
		}
	}
	return false
}
