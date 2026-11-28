package wallet

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
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

	// Enforce MaxUnprovenDepth as a real spend cap on live networks when ONLY deep unproven UTXOs exist
	// (fast-path O(1) index probe skips Balance + unproven DFS whenever any confirmed UTXO is spendable!).
	if !e.cfg.InMemoryStore && !isSignLater && !isPureSendWith && e.cfg.MaxUnprovenDepth > 0 && !e.HasConfirmedSpendableOutput(ctx) {
		if bal, bErr := e.Balance(ctx); bErr == nil && bal.ConfirmedSats == 0 && bal.MaxUnprovenDepth >= e.cfg.MaxUnprovenDepth {
			if _, minDepth, dErr := e.computeUnprovenDepthBounds(ctx); dErr == nil && minDepth >= e.cfg.MaxUnprovenDepth {
				return nil, fmt.Errorf("%w: active unproven chain depth %d reached policy cap %d (wait for a block confirmation)",
					ErrDepthExceeded, minDepth, e.cfg.MaxUnprovenDepth)
			}
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

	res, err := e.callTBCreateAction(ctx, args, orig)
	if err != nil && isNotEnoughFunds(err) && !isPureSendWith && e.svc.hasBroadcaster() {
		// 1. If static FundAddress() deposits are waiting in BasketFunding, absorb into native BRC-29 "default".
		if absorbed, aErr := e.AbsorbFundingBasket(ctx); aErr != nil {
			e.logf("wallet: CreateAction funding-basket absorb failed: %v", aErr)
		} else if absorbed > 0 {
			res, err = e.callTBCreateAction(ctx, args, orig)
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
				res, err = e.callTBCreateAction(ctx, args, orig)
			}
		}
	}
	if err != nil {
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

// callTBCreateAction is the single serialized chokepoint around e.tb.CreateAction.
// For live auto-funded sends (SignAndProcess=true, NoSend=false, no explicit inputs/SweepMax),
// it reserves UTXOs under e.actionMu in <1ms with SignAndProcess=false and then runs SignAction
// + live HTTP PostFromBEEF OUTSIDE e.actionMu so all fuel UTXOs broadcast in parallel (PERF-4).
func (e *Engine) callTBCreateAction(ctx context.Context, args sdkwallet.CreateActionArgs, originator string) (*sdkwallet.CreateActionResult, error) {
	orig := e.normalizeOrig(originator)
	isNoSend := args.Options != nil && args.Options.NoSend != nil && *args.Options.NoSend
	isSignLater := args.Options != nil && args.Options.SignAndProcess != nil && !*args.Options.SignAndProcess
	isPureSendWith := len(args.Inputs) == 0 && len(args.Outputs) == 0 && args.Options != nil && len(args.Options.SendWith) > 0
	hasSweepAll := false
	for _, o := range args.Outputs {
		if o.Satoshis == primitives.SweepMaxSatoshis {
			hasSweepAll = true
			break
		}
	}

	if !isNoSend && !isSignLater && !isPureSendWith && !hasSweepAll && len(args.Inputs) == 0 && len(args.Outputs) > 0 {
		phase1Args := args
		optsCopy := sdkwallet.CreateActionOptions{}
		if args.Options != nil {
			optsCopy = *args.Options
		}
		signFalse := false
		optsCopy.SignAndProcess = &signFalse
		phase1Args.Options = &optsCopy

		e.actionMu.Lock()
		created, cErr := e.tb.CreateAction(ctx, phase1Args, orig)
		e.actionMu.Unlock()
		if cErr != nil {
			return nil, cErr
		}
		if created == nil || created.SignableTransaction == nil {
			return nil, errors.New("wallet: two-phase CreateAction did not return SignableTransaction")
		}
		signOpts := &sdkwallet.SignActionOptions{}
		if args.Options != nil {
			signOpts.AcceptDelayedBroadcast = args.Options.AcceptDelayedBroadcast
			signOpts.ReturnTXIDOnly = args.Options.ReturnTXIDOnly
			signOpts.SendWith = args.Options.SendWith
		}
		signed, preBroadcast, sErr := e.signActionGuardedWithOrigin(ctx, sdkwallet.SignActionArgs{
			Reference: created.SignableTransaction.Reference,
			Options:   signOpts,
		}, orig, false)
		if sErr != nil {
			if preBroadcast && !IsUncertainBroadcastError(sErr) {
				_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, orig)
			}
			return nil, sErr
		}
		return &sdkwallet.CreateActionResult{
			Txid:            signed.Txid,
			Tx:              signed.Tx,
			SendWithResults: signed.SendWithResults,
		}, nil
	}

	callCtx, errSink := withPostErrSink(ctx)
	var (
		res *sdkwallet.CreateActionResult
		err error
	)
	if isPureSendWith {
		// Pure SendWith broadcasts an ALREADY-reserved NoSend tx over live HTTP ARC:
		// NEVER hold e.actionMu across network RTT so concurrent buyer broadcasts run in parallel!
		res, err = e.tb.CreateAction(callCtx, args, orig)
	} else {
		e.actionMu.Lock()
		res, err = e.tb.CreateAction(callCtx, args, orig)
		e.actionMu.Unlock()
	}
	if sinkTxID, sinkErr := errSink.get(); sinkErr != nil {
		if errors.Is(sinkErr, ErrBroadcastUnknown) && sinkTxID != "" {
			_ = e.MarkSubmissionUnknown(ctx, sinkTxID)
		}
		if err != nil {
			return nil, fmt.Errorf("%w (%v)", sinkErr, err)
		}
		return nil, sinkErr
	}
	if err != nil {
		if errors.Is(err, ErrBroadcastUnknown) && res != nil && !res.Txid.IsEqual(&chainhash.Hash{}) {
			_ = e.MarkSubmissionUnknown(ctx, res.Txid.String())
		}
		return nil, err
	}
	return res, nil
}

// signActionGuarded is the single chokepoint around e.tb.SignAction.
// It attaches a postErrSink, marks uncertain broadcasts as submission_unknown, and caches signed txs.
func (e *Engine) signActionGuarded(ctx context.Context, args sdkwallet.SignActionArgs, originator string, wakeFuel bool) (*sdkwallet.SignActionResult, error) {
	res, _, err := e.signActionGuardedWithOrigin(ctx, args, originator, wakeFuel)
	return res, err
}

func (e *Engine) signActionGuardedWithOrigin(ctx context.Context, args sdkwallet.SignActionArgs, originator string, wakeFuel bool) (*sdkwallet.SignActionResult, bool, error) {
	if args.Options == nil {
		args.Options = &sdkwallet.SignActionOptions{}
	}
	if args.Options.AcceptDelayedBroadcast == nil {
		d := e.profile.DefaultAcceptDelayed
		args.Options.AcceptDelayedBroadcast = &d
	}
	callCtx, errSink := withPostErrSink(ctx)
	res, err := e.tb.SignAction(callCtx, args, e.normalizeOrig(originator))
	if sinkTxID, sinkErr := errSink.get(); sinkErr != nil {
		if errors.Is(sinkErr, ErrBroadcastUnknown) && sinkTxID != "" {
			_ = e.MarkSubmissionUnknown(ctx, sinkTxID)
		}
		preBroadcast := errSink.isPreBroadcast()
		if err != nil {
			return nil, preBroadcast, fmt.Errorf("%w (%v)", sinkErr, err)
		}
		return nil, preBroadcast, sinkErr
	}
	if err != nil {
		if errors.Is(err, ErrBroadcastUnknown) && res != nil && !res.Txid.IsEqual(&chainhash.Hash{}) {
			_ = e.MarkSubmissionUnknown(ctx, res.Txid.String())
		}
		return nil, !IsUncertainBroadcastError(err), err
	}
	if res != nil && len(res.Tx) > 0 {
		if _, parsed, _, pErr := guard.ParseBEEF(res.Tx, guard.DefaultBound); pErr == nil && parsed != nil {
			e.txCache.Put(parsed.TxID().String(), parsed)
		}
	}
	if wakeFuel {
		e.fuel.Wake()
	}
	return res, false, nil
}

// runExplicitSignAction executes the two-phase CreateAction(SignAndProcess=false) -> custom input signing ->
// SignAction lifecycle through the single guarded chokepoints (actionMu, postErrSink, MarkSubmissionUnknown).
func (e *Engine) runExplicitSignAction(
	ctx context.Context,
	createArgs sdkwallet.CreateActionArgs,
	signCtx context.Context,
	buildSpends func(tx *transaction.Transaction) (map[uint32]sdkwallet.SignActionSpend, error),
	acceptDelayed bool,
	wakeFuel bool,
) (*sdkwallet.SignActionResult, error) {
	created, err := e.callTBCreateAction(ctx, createArgs, e.cfg.Originator)
	if err != nil {
		return nil, err
	}
	if created == nil || created.SignableTransaction == nil {
		return nil, errors.New("wallet: CreateAction did not return SignableTransaction")
	}
	tx, err := transaction.NewTransactionFromBEEF(created.SignableTransaction.Tx)
	if err != nil {
		_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
		return nil, err
	}
	spends, err := buildSpends(tx)
	if err != nil {
		_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
		return nil, err
	}
	res, preBroadcast, err := e.signActionGuardedWithOrigin(signCtx, sdkwallet.SignActionArgs{
		Reference: created.SignableTransaction.Reference,
		Spends:    spends,
		Options: &sdkwallet.SignActionOptions{
			AcceptDelayedBroadcast: &acceptDelayed,
		},
	}, e.cfg.Originator, wakeFuel)
	if err != nil {
		if preBroadcast && !IsUncertainBroadcastError(err) {
			_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
		}
		return nil, err
	}
	return res, nil
}

func (e *Engine) SignAction(ctx context.Context, args sdkwallet.SignActionArgs, originator string) (*sdkwallet.SignActionResult, error) {
	return e.signActionGuarded(ctx, args, originator, true)
}

func (e *Engine) AbortAction(ctx context.Context, args sdkwallet.AbortActionArgs, originator string) (*sdkwallet.AbortActionResult, error) {
	return e.tb.AbortAction(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) ListActions(ctx context.Context, args sdkwallet.ListActionsArgs, originator string) (*sdkwallet.ListActionsResult, error) {
	return e.tb.ListActions(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) InternalizeAction(ctx context.Context, args sdkwallet.InternalizeActionArgs, originator string) (*sdkwallet.InternalizeActionResult, error) {
	args.Description = ensureDesc(args.Description, "nektar internalize action")
	var parsed *transaction.Transaction
	if preTx := PreverifiedTxFromCtx(ctx); preTx != nil {
		// Upstream caller (e.g. HandlePurchase / verifyBRC105Payment) ALREADY ran full consensus
		// VerifyCounterpartyPaymentBEEF against the witnessed ChainTracker — skip duplicate ParseBEEF + spv.Verify!
		parsed = preTx
	} else {
		// Run full consensus go-sdk/spv.Verify (coinbase maturity 100, confiscation maturity 1000,
		// finality with 11-block MTP, out<=in overspend, duplicate inputs, era scripts, BUMP verification)
		// before accepting any incoming BEEF into wallet storage.
		parsedBeef, pTx, _, pErr := guard.ParseBEEF(args.Tx, guard.DefaultBound)
		if pErr != nil || pTx == nil {
			return nil, fmt.Errorf("wallet: InternalizeAction invalid BEEF: %v", pErr)
		}
		parsed = pTx
		if e.cfg.Tracker == nil {
			return nil, ErrNoChain
		}
		if parsed.MerklePath == nil {
			hydrateInputs(parsed, parsedBeef, e.txCache, e.svc.dbLoader(ctx), 0, e.cfg.MaxUnprovenDepth)
			if !e.cfg.InMemoryStore && e.cfg.MaxUnprovenDepth > 0 {
				if depth := CountUnminedAncestors(parsed, e.cfg.MaxUnprovenDepth+2); depth > e.cfg.MaxUnprovenDepth {
					return nil, fmt.Errorf("%w: incoming tx %s has %d unconfirmed ancestors (max %d)", ErrDepthExceeded, parsed.TxID().String(), depth, e.cfg.MaxUnprovenDepth)
				}
			}
		}
		if ok, sErr := spv.Verify(ctx, parsed, e.cfg.Tracker, nil); sErr != nil || !ok {
			return nil, fmt.Errorf("wallet: InternalizeAction SPV verify failed for %s: %v", parsed.TxID().String(), sErr)
		}
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

// CountUnminedAncestors returns the maximum unmined ancestor chain depth rooted at tx, capped at capLimit.
func CountUnminedAncestors(tx *transaction.Transaction, capLimit int) int {
	if tx == nil || tx.MerklePath != nil {
		return 0
	}
	maxNodes := capLimit * 8
	if maxNodes < 128 {
		maxNodes = 128
	}
	memo := make(map[string]int)
	visiting := make(map[string]bool)
	var depthOf func(cur *transaction.Transaction) int
	depthOf = func(cur *transaction.Transaction) int {
		if cur == nil || cur.MerklePath != nil {
			return 0
		}
		id := cur.TxID().String()
		if d, ok := memo[id]; ok {
			return d
		}
		if visiting[id] || len(memo) >= maxNodes {
			return capLimit
		}
		visiting[id] = true
		best := 1
		for _, in := range cur.Inputs {
			if in != nil && in.SourceTransaction != nil && in.SourceTransaction.MerklePath == nil {
				pd := depthOf(in.SourceTransaction)
				if pd+1 > best {
					best = pd + 1
				}
				if best >= capLimit {
					best = capLimit
					break
				}
			}
		}
		delete(visiting, id)
		memo[id] = best
		return best
	}
	return depthOf(tx)
}

func (e *Engine) ListOutputs(ctx context.Context, args sdkwallet.ListOutputsArgs, originator string) (*sdkwallet.ListOutputsResult, error) {
	// Read-only BRC-100 RPC: never trigger synchronous AbsorbFundingBasket broadcasts here.
	return e.tb.ListOutputs(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) RelinquishOutput(ctx context.Context, args sdkwallet.RelinquishOutputArgs, originator string) (*sdkwallet.RelinquishOutputResult, error) {
	return e.tb.RelinquishOutput(ctx, args, e.normalizeOrig(originator))
}

func isFastPathBRC43(p sdkwallet.Protocol, keyID string) bool {
	if p.SecurityLevel < 0 || p.SecurityLevel > 2 {
		return false
	}
	if len(p.Protocol) < 5 || len(p.Protocol) > 400 || strings.TrimSpace(p.Protocol) != p.Protocol || strings.Contains(p.Protocol, "  ") {
		return false
	}
	for i := 0; i < len(p.Protocol); i++ {
		c := p.Protocol[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == ' ') {
			return false
		}
	}
	return len(keyID) >= 1 && len(keyID) <= 800
}

func (e *Engine) GetPublicKey(ctx context.Context, args sdkwallet.GetPublicKeyArgs, originator string) (*sdkwallet.GetPublicKeyResult, error) {
	if e.keyDeriver != nil {
		if args.IdentityKey {
			return &sdkwallet.GetPublicKeyResult{PublicKey: e.rootKey.PubKey()}, nil
		}
		if !args.Privileged && isFastPathBRC43(args.ProtocolID, args.KeyID) {
			cp := args.Counterparty
			if cp.Type == sdkwallet.CounterpartyUninitialized {
				cp = sdkwallet.Counterparty{Type: sdkwallet.CounterpartyTypeSelf}
			}
			forSelf := args.ForSelf != nil && *args.ForSelf
			if pub, err := e.keyDeriver.DerivePublicKey(args.ProtocolID, args.KeyID, cp, forSelf); err == nil {
				return &sdkwallet.GetPublicKeyResult{PublicKey: pub}, nil
			}
		}
	}
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
	if e.keyDeriver != nil && !args.Privileged && isFastPathBRC43(args.ProtocolID, args.KeyID) {
		cp := args.Counterparty
		if cp.Type == sdkwallet.CounterpartyUninitialized {
			cp = sdkwallet.Counterparty{Type: sdkwallet.CounterpartyTypeSelf}
		}
		if sym, err := e.keyDeriver.DeriveSymmetricKey(args.ProtocolID, args.KeyID, cp); err == nil {
			mac := hmac.New(sha256.New, sym.ToBytes())
			mac.Write(args.Data)
			res := &sdkwallet.CreateHMACResult{}
			copy(res.HMAC[:], mac.Sum(nil))
			return res, nil
		}
	}
	return e.tb.CreateHMAC(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) VerifyHMAC(ctx context.Context, args sdkwallet.VerifyHMACArgs, originator string) (*sdkwallet.VerifyHMACResult, error) {
	return e.tb.VerifyHMAC(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) CreateSignature(ctx context.Context, args sdkwallet.CreateSignatureArgs, originator string) (*sdkwallet.CreateSignatureResult, error) {
	if e.keyDeriver != nil && !args.Privileged && isFastPathBRC43(args.ProtocolID, args.KeyID) && (len(args.HashToDirectlySign) == 32 || len(args.Data) > 0) {
		cp := args.Counterparty
		if cp.Type == sdkwallet.CounterpartyUninitialized {
			cp = sdkwallet.Counterparty{Type: sdkwallet.CounterpartyTypeAnyone}
		}
		if priv, err := e.keyDeriver.DerivePrivateKey(args.ProtocolID, args.KeyID, cp); err == nil {
			var digest []byte
			if len(args.HashToDirectlySign) > 0 {
				digest = args.HashToDirectlySign
			} else {
				sum := sha256.Sum256(args.Data)
				digest = sum[:]
			}
			if sig, sErr := priv.Sign(digest); sErr == nil {
				return &sdkwallet.CreateSignatureResult{Signature: sig}, nil
			}
		}
	}
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
