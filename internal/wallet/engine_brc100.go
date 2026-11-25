package wallet

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk/primitives"
	"github.com/lightwebinc/bcommon/guard"
)

func (e *Engine) normalizeOrig(orig string) string {
	if strings.TrimSpace(orig) == "" {
		return e.cfg.Originator
	}
	return strings.TrimSpace(orig)
}

func ensureDesc(s, fallback string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 5 {
		return s
	}
	if s == "" {
		return fallback
	}
	return s + " action"
}

// -----------------------------------------------------------------------------
// BRC-100 sdkwallet.Interface (28 methods delegated to go-wallet-toolbox)
// -----------------------------------------------------------------------------

func (e *Engine) CreateAction(ctx context.Context, args sdkwallet.CreateActionArgs, originator string) (*sdkwallet.CreateActionResult, error) {
	return e.createActionGuarded(ctx, args, originator, false, false)
}

func (e *Engine) createActionGuarded(ctx context.Context, args sdkwallet.CreateActionArgs, originator string, overrideGuard, skipGuard bool) (*sdkwallet.CreateActionResult, error) {
	orig := e.normalizeOrig(originator)
	args.Description = ensureDesc(args.Description, "nektar wallet action")
	for i := range args.Outputs {
		args.Outputs[i].OutputDescription = ensureDesc(args.Outputs[i].OutputDescription, "nektar output")
	}
	for i := range args.Inputs {
		args.Inputs[i].InputDescription = ensureDesc(args.Inputs[i].InputDescription, "nektar input")
	}

	if args.Options == nil {
		args.Options = &sdkwallet.CreateActionOptions{}
	}
	if args.Options.AcceptDelayedBroadcast == nil {
		d := e.profile.DefaultAcceptDelayed
		args.Options.AcceptDelayedBroadcast = &d
	}

	isNoSend := args.Options.NoSend != nil && *args.Options.NoSend
	isSignLater := args.Options.SignAndProcess != nil && !*args.Options.SignAndProcess
	isPureSendWith := len(args.Inputs) == 0 && len(args.Outputs) == 0 && len(args.Options.SendWith) > 0

	var outTotal uint64
	hasSweepAll := false
	for _, o := range args.Outputs {
		if o.Satoshis == primitives.SweepMaxSatoshis {
			hasSweepAll = true
		} else {
			outTotal += o.Satoshis
		}
	}

	// Enforce MaxUnprovenDepth as a real spend cap on live networks when only deep unproven UTXOs exist.
	if !e.cfg.InMemoryStore && !isSignLater && !isPureSendWith && e.cfg.MaxUnprovenDepth > 0 {
		if bal, bErr := e.Balance(ctx); bErr == nil && bal.ConfirmedSats == 0 && bal.MaxUnprovenDepth >= e.cfg.MaxUnprovenDepth {
			return nil, fmt.Errorf("%w: active unproven chain depth %d reached policy cap %d (wait for a block confirmation)",
				ErrDepthExceeded, bal.MaxUnprovenDepth, e.cfg.MaxUnprovenDepth)
		}
	}

	// Read-only SpendGuard pre-check for fast fail (zero phantom deduction; real deduction happens
	// once post-signing inside bcommonServices.PostFromBEEF with exact fee + external net spend).
	if !skipGuard && !overrideGuard && !isNoSend && !isSignLater && !isPureSendWith && e.cfg.SpendGuardCheck != nil {
		estFee := e.estimateFeeSats(ctx, max(1, len(args.Inputs)), len(args.Outputs)+1)
		if err := e.cfg.SpendGuardCheck(estFee, outTotal+estFee); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSpendRejected, err)
		}
	}
	if overrideGuard || skipGuard {
		ctx = WithOverrideGuard(ctx)
	} else if !isPureSendWith {
		if hasSweepAll {
			ctx = withSweepAllExternalSpend(ctx)
		} else {
			ctx = withExternalSpend(ctx, outTotal)
		}
	}

	ctx, errSink := withPostErrSink(ctx)
	res, err := e.tb.CreateAction(ctx, args, orig)
	if err != nil && isNotEnoughFunds(err) && !isPureSendWith && e.svc.hasBroadcaster() {
		// 1. If static FundAddress() deposits are waiting in BasketFunding, absorb into native BRC-29 "default".
		if absorbed, aErr := e.AbsorbFundingBasket(ctx); aErr != nil {
			e.logf("wallet: CreateAction funding-basket absorb failed: %v", aErr)
		} else if absorbed > 0 {
			res, err = e.tb.CreateAction(ctx, args, orig)
		}
		// 2. If funds are split across StrategyThroughput baskets (default + reserve + fuel) such that no
		// single basket covers this action alone, reclaim just enough reserve/fuel into "default" and retry!
		if err != nil && isNotEnoughFunds(err) {
			var needed uint64
			for _, o := range args.Outputs {
				if o.Satoshis != primitives.SweepMaxSatoshis {
					needed += o.Satoshis
				}
			}
			if reclaimed, rErr := e.reclaimPoolToDefault(ctx, needed+e.estimateFeeSats(ctx, 4, len(args.Outputs)+1)); rErr != nil {
				e.logf("wallet: CreateAction pool reclaim failed: %v", rErr)
			} else if reclaimed > 0 {
				res, err = e.tb.CreateAction(ctx, args, orig)
			}
		}
	}
	if err != nil {
		if sinkTxID, sinkErr := errSink.get(); sinkErr != nil {
			if errors.Is(sinkErr, ErrBroadcastUnknown) && sinkTxID != "" {
				_ = e.MarkSubmissionUnknown(ctx, sinkTxID)
			}
			return nil, fmt.Errorf("%w (%v)", sinkErr, err)
		}
		if errors.Is(err, ErrBroadcastUnknown) && res != nil && !res.Txid.IsEqual(&chainhash.Hash{}) {
			_ = e.MarkSubmissionUnknown(ctx, res.Txid.String())
		}
		return nil, err
	}

	if res != nil && isNoSend && !res.Txid.IsEqual(&chainhash.Hash{}) {
		e.svc.setPlannedSpend(res.Txid.String(), outTotal)
	}
	if res != nil && len(res.Tx) > 0 {
		if _, parsed, _, pErr := guard.ParseBEEF(res.Tx, guard.DefaultBound); pErr == nil && parsed != nil {
			e.txCache.Put(parsed.TxID().String(), parsed)
		}
	}
	if !isSignLater && !isNoSend {
		e.fuel.Wake()
	}
	return res, nil
}

func (e *Engine) SignAction(ctx context.Context, args sdkwallet.SignActionArgs, originator string) (*sdkwallet.SignActionResult, error) {
	if args.Options == nil {
		args.Options = &sdkwallet.SignActionOptions{}
	}
	if args.Options.AcceptDelayedBroadcast == nil {
		d := e.profile.DefaultAcceptDelayed
		args.Options.AcceptDelayedBroadcast = &d
	}
	ctx, errSink := withPostErrSink(ctx)
	res, err := e.tb.SignAction(ctx, args, e.normalizeOrig(originator))
	if err != nil {
		if sinkTxID, sinkErr := errSink.get(); sinkErr != nil {
			if errors.Is(sinkErr, ErrBroadcastUnknown) && sinkTxID != "" {
				_ = e.MarkSubmissionUnknown(ctx, sinkTxID)
			}
			return nil, fmt.Errorf("%w (%v)", sinkErr, err)
		}
		return nil, err
	}
	if res != nil && len(res.Tx) > 0 {
		if _, parsed, _, pErr := guard.ParseBEEF(res.Tx, guard.DefaultBound); pErr == nil && parsed != nil {
			e.txCache.Put(parsed.TxID().String(), parsed)
		}
	}
	e.fuel.Wake()
	return res, nil
}

func (e *Engine) AbortAction(ctx context.Context, args sdkwallet.AbortActionArgs, originator string) (*sdkwallet.AbortActionResult, error) {
	return e.tb.AbortAction(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) ListActions(ctx context.Context, args sdkwallet.ListActionsArgs, originator string) (*sdkwallet.ListActionsResult, error) {
	return e.tb.ListActions(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) InternalizeAction(ctx context.Context, args sdkwallet.InternalizeActionArgs, originator string) (*sdkwallet.InternalizeActionResult, error) {
	args.Description = ensureDesc(args.Description, "nektar internalize action")
	// Run full consensus go-sdk/spv.Verify (coinbase maturity 100, confiscation maturity 1000,
	// finality with 11-block MTP, out<=in overspend, duplicate inputs, era scripts, BUMP verification)
	// before accepting any incoming BEEF into wallet storage.
	parsedBeef, parsed, _, pErr := guard.ParseBEEF(args.Tx, guard.DefaultBound)
	if pErr != nil || parsed == nil {
		return nil, fmt.Errorf("wallet: InternalizeAction invalid BEEF: %v", pErr)
	}
	if e.cfg.Tracker == nil {
		return nil, ErrNoChain
	}
	if parsed.MerklePath == nil {
		hydrateInputs(parsed, parsedBeef, e.txCache, e.svc.dbLoader(ctx), 0, e.cfg.MaxUnprovenDepth)
		if !e.cfg.InMemoryStore && e.cfg.MaxUnprovenDepth > 0 {
			if depth := countUnminedAncestors(parsed, e.cfg.MaxUnprovenDepth+2); depth > e.cfg.MaxUnprovenDepth {
				return nil, fmt.Errorf("%w: incoming tx %s has %d unconfirmed ancestors (max %d)", ErrDepthExceeded, parsed.TxID().String(), depth, e.cfg.MaxUnprovenDepth)
			}
		}
	}
	if ok, sErr := spv.Verify(ctx, parsed, e.cfg.Tracker, nil); sErr != nil || !ok {
		return nil, fmt.Errorf("wallet: InternalizeAction SPV verify failed for %s: %v", parsed.TxID().String(), sErr)
	}

	// Mark incoming internalization so go-wallet-toolbox BackgroundBroadcast never debits buyer
	// change against the receiver's SpendGuard, and capture any swallowed broadcast errors.
	intCtx, errSink := withPostErrSink(WithIncomingInternalize(ctx))
	res, err := e.tb.InternalizeAction(intCtx, args, e.normalizeOrig(originator))
	if err != nil {
		return nil, err
	}
	if sinkTxID, sinkErr := errSink.get(); sinkErr != nil {
		e.logf("wallet: InternalizeAction background broadcast warning for %s: %v", sinkTxID, sinkErr)
	}
	e.txCache.Put(parsed.TxID().String(), parsed)
	e.fuel.Wake()
	return res, nil
}

func countUnminedAncestors(tx *transaction.Transaction, capLimit int) int {
	if tx == nil || tx.MerklePath != nil {
		return 0
	}
	seen := make(map[string]bool)
	var walk func(cur *transaction.Transaction)
	walk = func(cur *transaction.Transaction) {
		if cur == nil || cur.MerklePath != nil || len(seen) >= capLimit {
			return
		}
		id := cur.TxID().String()
		if seen[id] {
			return
		}
		seen[id] = true
		for _, in := range cur.Inputs {
			if in != nil && in.SourceTransaction != nil && in.SourceTransaction.MerklePath == nil {
				walk(in.SourceTransaction)
				if len(seen) >= capLimit {
					return
				}
			}
		}
	}
	walk(tx)
	return len(seen)
}

func (e *Engine) ListOutputs(ctx context.Context, args sdkwallet.ListOutputsArgs, originator string) (*sdkwallet.ListOutputsResult, error) {
	// Read-only BRC-100 RPC: never trigger synchronous AbsorbFundingBasket broadcasts here.
	return e.tb.ListOutputs(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) RelinquishOutput(ctx context.Context, args sdkwallet.RelinquishOutputArgs, originator string) (*sdkwallet.RelinquishOutputResult, error) {
	return e.tb.RelinquishOutput(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) GetPublicKey(ctx context.Context, args sdkwallet.GetPublicKeyArgs, originator string) (*sdkwallet.GetPublicKeyResult, error) {
	return e.tb.GetPublicKey(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) RevealCounterpartyKeyLinkage(ctx context.Context, args sdkwallet.RevealCounterpartyKeyLinkageArgs, originator string) (*sdkwallet.RevealCounterpartyKeyLinkageResult, error) {
	return e.tb.RevealCounterpartyKeyLinkage(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) RevealSpecificKeyLinkage(ctx context.Context, args sdkwallet.RevealSpecificKeyLinkageArgs, originator string) (*sdkwallet.RevealSpecificKeyLinkageResult, error) {
	return e.tb.RevealSpecificKeyLinkage(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) Encrypt(ctx context.Context, args sdkwallet.EncryptArgs, originator string) (*sdkwallet.EncryptResult, error) {
	return e.tb.Encrypt(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) Decrypt(ctx context.Context, args sdkwallet.DecryptArgs, originator string) (*sdkwallet.DecryptResult, error) {
	return e.tb.Decrypt(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) CreateHMAC(ctx context.Context, args sdkwallet.CreateHMACArgs, originator string) (*sdkwallet.CreateHMACResult, error) {
	return e.tb.CreateHMAC(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) VerifyHMAC(ctx context.Context, args sdkwallet.VerifyHMACArgs, originator string) (*sdkwallet.VerifyHMACResult, error) {
	return e.tb.VerifyHMAC(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) CreateSignature(ctx context.Context, args sdkwallet.CreateSignatureArgs, originator string) (*sdkwallet.CreateSignatureResult, error) {
	return e.tb.CreateSignature(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) VerifySignature(ctx context.Context, args sdkwallet.VerifySignatureArgs, originator string) (*sdkwallet.VerifySignatureResult, error) {
	return e.tb.VerifySignature(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) AcquireCertificate(ctx context.Context, args sdkwallet.AcquireCertificateArgs, originator string) (*sdkwallet.Certificate, error) {
	return e.tb.AcquireCertificate(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) ListCertificates(ctx context.Context, args sdkwallet.ListCertificatesArgs, originator string) (*sdkwallet.ListCertificatesResult, error) {
	return e.tb.ListCertificates(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) ProveCertificate(ctx context.Context, args sdkwallet.ProveCertificateArgs, originator string) (*sdkwallet.ProveCertificateResult, error) {
	return e.tb.ProveCertificate(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) RelinquishCertificate(ctx context.Context, args sdkwallet.RelinquishCertificateArgs, originator string) (*sdkwallet.RelinquishCertificateResult, error) {
	return e.tb.RelinquishCertificate(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) DiscoverByIdentityKey(ctx context.Context, args sdkwallet.DiscoverByIdentityKeyArgs, originator string) (*sdkwallet.DiscoverCertificatesResult, error) {
	return e.tb.DiscoverByIdentityKey(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) DiscoverByAttributes(ctx context.Context, args sdkwallet.DiscoverByAttributesArgs, originator string) (*sdkwallet.DiscoverCertificatesResult, error) {
	return e.tb.DiscoverByAttributes(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) IsAuthenticated(ctx context.Context, args any, originator string) (*sdkwallet.AuthenticatedResult, error) {
	return e.tb.IsAuthenticated(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) WaitForAuthentication(ctx context.Context, args any, originator string) (*sdkwallet.AuthenticatedResult, error) {
	return e.tb.WaitForAuthentication(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) GetHeight(ctx context.Context, args any, originator string) (*sdkwallet.GetHeightResult, error) {
	return e.tb.GetHeight(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) GetHeaderForHeight(ctx context.Context, args sdkwallet.GetHeaderArgs, originator string) (*sdkwallet.GetHeaderResult, error) {
	return e.tb.GetHeaderForHeight(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) GetNetwork(ctx context.Context, args any, originator string) (*sdkwallet.GetNetworkResult, error) {
	return e.tb.GetNetwork(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) GetVersion(_ context.Context, _ any, _ string) (*sdkwallet.GetVersionResult, error) {
	return &sdkwallet.GetVersionResult{Version: WalletVersion}, nil
}
