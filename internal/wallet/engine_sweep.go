package wallet

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/brc29"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/entity"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk/primitives"
	"github.com/lightwebinc/bcommon/guard"
)

// -----------------------------------------------------------------------------
// High-Level Transfer Operators: Sweep
// -----------------------------------------------------------------------------

// Sweep sweeps 100% of spendable funds across ALL spending baskets (default, reserve, fuel, and
// nektar funding) to a P2PKH address or BRC-100 identity public key (BRC-29) via go-wallet-toolbox.
func (e *Engine) Sweep(ctx context.Context, args SweepArgs) (*SweepResult, error) {
	if (args.ToAddress == "" && args.ToIdentityKey == "") || (args.ToAddress != "" && args.ToIdentityKey != "") {
		return nil, fmt.Errorf("%w: specify exactly one of toAddress or toIdentityKey", ErrInvalidArgs)
	}
	maxPerTx := args.MaxPerTx
	if maxPerTx <= 0 {
		maxPerTx = DefaultSweepChunkInputs
	}

	// Collect all unspent spendable outputs across default, reserve, fuel, and funding baskets.
	allOuts, err := e.listUnreservedSpendableOutputs(ctx)
	if err != nil {
		return nil, err
	}

	var defaultCount int
	var explicit []*entity.Output
	var totalAvail uint64
	for _, o := range allOuts {
		bName := ""
		if o.BasketName != nil {
			bName = *o.BasketName
		}
		if !IsSpendingBasket(bName) {
			continue
		}
		totalAvail += uint64(o.Satoshis)
		if bName == BasketDefault {
			defaultCount++
		} else {
			explicit = append(explicit, o)
		}
	}
	if defaultCount == 0 && len(explicit) == 0 {
		return nil, ErrInsufficient
	}

	if e.cfg.SpendGuardCheck != nil && !args.OverrideGuard {
		estFee := e.estimateFeeSats(ctx, defaultCount+len(explicit), 1)
		if err := e.cfg.SpendGuardCheck(estFee, totalAvail); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSpendRejected, err)
		}
	}
	if args.OverrideGuard {
		ctx = WithOverrideGuard(ctx)
	} else {
		ctx = withSweepAllExternalSpend(ctx)
	}

	var recipPub *ec.PublicKey
	var staticLock *script.Script
	if args.ToAddress != "" {
		addr, aErr := script.NewAddressFromString(strings.TrimSpace(args.ToAddress))
		if aErr != nil {
			return nil, fmt.Errorf("%w: invalid address %q: %v", ErrInvalidArgs, args.ToAddress, aErr)
		}
		staticLock, err = p2pkh.Lock(addr)
		if err != nil {
			return nil, err
		}
	} else {
		recipPub, err = guard.ParsePubKeyHex(strings.TrimSpace(args.ToIdentityKey))
		if err != nil {
			return nil, fmt.Errorf("%w: invalid toIdentityKey: %v", ErrInvalidArgs, err)
		}
	}

	makeDestLock := func() (*script.Script, string, string, error) {
		if staticLock != nil {
			return staticLock, "", "", nil
		}
		var preRaw, sufRaw [16]byte
		if _, err := rand.Read(preRaw[:]); err != nil {
			return nil, "", "", fmt.Errorf("wallet: crypto/rand failed: %w", err)
		}
		if _, err := rand.Read(sufRaw[:]); err != nil {
			return nil, "", "", fmt.Errorf("wallet: crypto/rand failed: %w", err)
		}
		preB64 := base64.StdEncoding.EncodeToString(preRaw[:])
		sufB64 := base64.StdEncoding.EncodeToString(sufRaw[:])
		var ls *script.Script
		if !e.cfg.Mainnet {
			ls, err = brc29.LockForCounterparty(e.rootKey, brc29.KeyID{
				DerivationPrefix: preB64,
				DerivationSuffix: sufB64,
			}, recipPub, brc29.WithTestNet())
		} else {
			ls, err = brc29.LockForCounterparty(e.rootKey, brc29.KeyID{
				DerivationPrefix: preB64,
				DerivationSuffix: sufB64,
			}, recipPub)
		}
		return ls, preB64, sufB64, err
	}

	desc := ensureDesc(args.Description, "nektar wallet sweep")
	out := &SweepResult{}
	if recipPub != nil {
		out.SenderIdentityKey = e.IdentityPubHex()
	}

	// Step 1: Sweep all explicit non-default outputs (fuel, reserve, nektar funding) in chunks!
	// On the final chunk, if defaultCount > 0 and chunk + defaultCount <= maxPerTx, we include
	// SweepMaxSatoshis so go-wallet-toolbox sweeps "default" into the exact same single tx!
	defaultSwept := false
	for start := 0; start < len(explicit); start += maxPerTx {
		if start > 0 && !args.OverrideGuard && e.fuel != nil {
			if err := e.fuel.waitSpendGuardInterval(ctx); err != nil {
				return out, err
			}
		}
		end := min(start+maxPerTx, len(explicit))
		chunk := explicit[start:end]
		isLast := end == len(explicit)
		includeDefault := isLast && defaultCount > 0 && len(chunk)+defaultCount <= maxPerTx

		lockScript, preB64, sufB64, lErr := makeDestLock()
		if lErr != nil {
			return out, lErr
		}
		var pendingID int64
		if preB64 != "" {
			var pErr error
			pendingID, pErr = e.SavePendingBRC29(ctx, PendingBRC29Record{
				Direction:        "sweep",
				WireURL:          args.WireURL,
				CounterpartyPub:  strings.TrimSpace(args.ToIdentityKey),
				DerivationPrefix: preB64,
				DerivationSuffix: sufB64,
				LockingScriptHex: hex.EncodeToString(*lockScript),
			})
			if pErr != nil {
				return out, fmt.Errorf("wallet: journal pending BRC-29 sweep: %w", pErr)
			}
		}
		txid, beef, parsed, sweptSats, feeSats, inputsUsed, sErr := e.sweepExplicitChunk(ctx, chunk, includeDefault, lockScript, desc)
		if sErr != nil && len(chunk) > 1 {
			if healthy, pruned := e.filterVerifiedUnspentOutputs(ctx, chunk); pruned > 0 && len(healthy) > 0 {
				txid, beef, parsed, sweptSats, feeSats, inputsUsed, sErr = e.sweepExplicitChunk(ctx, healthy, includeDefault, lockScript, desc)
			}
		}
		if sErr != nil {
			if pendingID > 0 && !IsUncertainBroadcastError(sErr) {
				_ = e.DeletePendingBRC29(ctx, pendingID)
			}
			return out, sErr
		}
		if includeDefault {
			defaultSwept = true
		}
		e.appendSweepChunkResult(out, txid, beef, parsed, sweptSats, feeSats, inputsUsed, lockScript, preB64, sufB64)
		if pendingID > 0 {
			outIdx := out.OutputIndices[len(out.OutputIndices)-1]
			if uErr := e.UpdatePendingBRC29Tx(ctx, pendingID, txid, outIdx, beef, sweptSats); uErr != nil {
				e.logf("wallet: sweep update pending BRC-29 %d (%s): %v", pendingID, txid, uErr)
			}
		}
	}

	// Step 2: Sweep remaining "default" basket via go-wallet-toolbox's native SweepMaxSatoshis!
	if defaultCount > 0 && !defaultSwept {
		if len(out.TxIDs) > 0 && !args.OverrideGuard && e.fuel != nil {
			if err := e.fuel.waitSpendGuardInterval(ctx); err != nil {
				return out, err
			}
		}
		lockScript, preB64, sufB64, lErr := makeDestLock()
		if lErr != nil {
			return out, lErr
		}
		var pendingID int64
		if preB64 != "" {
			var pErr error
			pendingID, pErr = e.SavePendingBRC29(ctx, PendingBRC29Record{
				Direction:        "sweep",
				WireURL:          args.WireURL,
				CounterpartyPub:  strings.TrimSpace(args.ToIdentityKey),
				DerivationPrefix: preB64,
				DerivationSuffix: sufB64,
				LockingScriptHex: hex.EncodeToString(*lockScript),
			})
			if pErr != nil {
				return out, fmt.Errorf("wallet: journal pending BRC-29 sweep: %w", pErr)
			}
		}
		acceptDelayed := false
		randomize := false
		caRes, cErr := e.callTBCreateAction(ctx, sdkwallet.CreateActionArgs{
			Description: desc,
			Labels:      []string{"nektar", "sweep"},
			Outputs: []sdkwallet.CreateActionOutput{{
				LockingScript:     *lockScript,
				Satoshis:          primitives.SweepMaxSatoshis,
				OutputDescription: desc,
			}},
			Options: &sdkwallet.CreateActionOptions{
				AcceptDelayedBroadcast: &acceptDelayed,
				RandomizeOutputs:       &randomize,
			},
		}, e.cfg.Originator)
		if cErr != nil {
			if pendingID > 0 && !IsUncertainBroadcastError(cErr) {
				_ = e.DeletePendingBRC29(ctx, pendingID)
			}
			return out, fmt.Errorf("wallet: sweep default basket: %w", cErr)
		}
		txid := caRes.Txid.String()
		var parsed *transaction.Transaction
		if len(caRes.Tx) > 0 {
			_, parsed, _, _ = guard.ParseBEEF(caRes.Tx, guard.DefaultBound)
		}
		if parsed == nil {
			parsed, _ = e.LoadParsedTx(ctx, txid)
		}
		var sweptSats, feeSats uint64
		var inputsUsed int
		if parsed != nil {
			inputsUsed = len(parsed.Inputs)
			feeSats = ComputeTxFee(parsed)
			for _, o := range parsed.Outputs {
				if o.LockingScript != nil && string(*o.LockingScript) == string(*lockScript) {
					sweptSats += o.Satoshis
				}
			}
			e.txCache.Put(txid, parsed)
		}
		e.appendSweepChunkResult(out, txid, caRes.Tx, parsed, sweptSats, feeSats, inputsUsed, lockScript, preB64, sufB64)
		if pendingID > 0 {
			outIdx := out.OutputIndices[len(out.OutputIndices)-1]
			if uErr := e.UpdatePendingBRC29Tx(ctx, pendingID, txid, outIdx, caRes.Tx, sweptSats); uErr != nil {
				e.logf("wallet: sweep update pending BRC-29 %d (%s): %v", pendingID, txid, uErr)
			}
		}
	}

	return out, nil
}

func (e *Engine) sweepExplicitChunk(ctx context.Context, chunk []*entity.Output, includeDefault bool, destLock *script.Script, desc string) (
	txid string, beef []byte, parsed *transaction.Transaction, sweptSats, feeSats uint64, inputsUsed int, err error,
) {
	combinedBeef := transaction.NewBeef()
	inputs := make([]sdkwallet.CreateActionInput, 0, len(chunk))
	seenTx := make(map[string]bool)
	var chunkSum uint64

	for _, o := range chunk {
		id := *o.TxID
		if !seenTx[id] {
			if mErr := e.mergeTxIntoBeef(ctx, combinedBeef, id); mErr != nil {
				return "", nil, nil, 0, 0, 0, fmt.Errorf("wallet: sweep load parent %s: %w", id, mErr)
			}
			seenTx[id] = true
		}
		h, hErr := chainhash.NewHashFromHex(id)
		if hErr != nil {
			return "", nil, nil, 0, 0, 0, hErr
		}
		chunkSum += uint64(o.Satoshis)
		seqFinal := uint32(0xffffffff)
		inputs = append(inputs, sdkwallet.CreateActionInput{
			Outpoint:              transaction.Outpoint{Txid: *h, Index: o.Vout},
			InputDescription:      "nektar sweep input",
			UnlockingScriptLength: 107,
			SequenceNumber:        &seqFinal,
		})
	}
	inputBeefBytes, err := combinedBeef.Bytes()
	if err != nil {
		return "", nil, nil, 0, 0, 0, err
	}

	outSats := uint64(primitives.SweepMaxSatoshis)
	if !includeDefault {
		// Compute exact fee for this pure explicit-input chunk so zero funds are drawn from "default".
		estFee := e.estimateFeeSats(ctx, len(inputs), 1)
		if chunkSum <= estFee {
			return "", nil, nil, 0, 0, 0, ErrInsufficient
		}
		outSats = chunkSum - estFee
	}

	signAndProc := false
	acceptDelayed := false
	randomize := false
	signed, err := e.runExplicitSignAction(ctx, sdkwallet.CreateActionArgs{
		Description: desc,
		Labels:      []string{"nektar", "sweep"},
		InputBEEF:   inputBeefBytes,
		Inputs:      inputs,
		Outputs: []sdkwallet.CreateActionOutput{{
			LockingScript:     *destLock,
			Satoshis:          outSats,
			OutputDescription: desc,
		}},
		Options: &sdkwallet.CreateActionOptions{
			SignAndProcess:         &signAndProc,
			AcceptDelayedBroadcast: &acceptDelayed,
			RandomizeOutputs:       &randomize,
		},
	}, ctx, func(tx *transaction.Transaction) (map[uint32]sdkwallet.SignActionSpend, error) {
		spends := make(map[uint32]sdkwallet.SignActionSpend, len(chunk))
		for idx, o := range chunk {
			vin := uint32(idx)
			unlocker, uErr := e.unlockerForEntity(o)
			if uErr != nil {
				return nil, fmt.Errorf("wallet: sweep unlocker input %d: %w", vin, uErr)
			}
			us, sErr := unlocker.Sign(tx, vin)
			if sErr != nil {
				return nil, fmt.Errorf("wallet: sweep sign input %d: %w", vin, sErr)
			}
			seqFinal := uint32(0xffffffff)
			spends[vin] = sdkwallet.SignActionSpend{
				UnlockingScript: *us,
				SequenceNumber:  &seqFinal,
			}
		}
		return spends, nil
	}, acceptDelayed, true)
	if err != nil {
		return "", nil, nil, 0, 0, 0, fmt.Errorf("wallet: sweep explicit chunk: %w", err)
	}

	txid = signed.Txid.String()
	beef = signed.Tx
	if len(beef) > 0 {
		_, parsed, _, _ = guard.ParseBEEF(beef, guard.DefaultBound)
	}
	if parsed == nil {
		parsed, _ = e.LoadParsedTx(ctx, txid)
	}
	if parsed != nil {
		inputsUsed = len(parsed.Inputs)
		feeSats = ComputeTxFee(parsed)
		for _, o := range parsed.Outputs {
			if o.LockingScript != nil && string(*o.LockingScript) == string(*destLock) {
				sweptSats += o.Satoshis
			}
		}
		e.txCache.Put(txid, parsed)
	}
	return txid, beef, parsed, sweptSats, feeSats, inputsUsed, nil
}

func (e *Engine) appendSweepChunkResult(out *SweepResult, txid string, beef []byte, parsed *transaction.Transaction, sweptSats, feeSats uint64, inputsUsed int, destLock *script.Script, preB64, sufB64 string) {
	outIdx := uint32(0)
	if parsed != nil {
		for i, o := range parsed.Outputs {
			if o.LockingScript != nil && string(*o.LockingScript) == string(*destLock) {
				outIdx = uint32(i)
				break
			}
		}
	}
	out.TxIDs = append(out.TxIDs, txid)
	out.SweptSats += sweptSats
	out.TotalFeeSats += feeSats
	out.InputsSwept += inputsUsed
	out.BEEFs = append(out.BEEFs, beef)
	out.Txs = append(out.Txs, parsed)
	out.OutputIndices = append(out.OutputIndices, outIdx)
	if preB64 != "" {
		if out.DerivationPrefix == "" {
			out.DerivationPrefix = preB64
			out.DerivationSuffix = sufB64
		}
		out.Remittances = append(out.Remittances, PaymentRemittance{
			TxID:              txid,
			OutputIndex:       outIdx,
			DerivationPrefix:  preB64,
			DerivationSuffix:  sufB64,
			SenderIdentityKey: e.IdentityPubHex(),
		})
	}
}
