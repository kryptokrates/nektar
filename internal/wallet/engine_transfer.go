package wallet

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
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
// High-Level Transfer Operators: Send & Sweep
// -----------------------------------------------------------------------------

// Send sends BSV to a P2PKH address OR a BRC-100 identity public key (BRC-29).
func (e *Engine) Send(ctx context.Context, args SendArgs) (*SendResult, error) {
	if args.Satoshis == 0 {
		return nil, fmt.Errorf("%w: send satoshis must be > 0", ErrInvalidArgs)
	}
	if (args.ToAddress == "" && args.ToIdentityKey == "") || (args.ToAddress != "" && args.ToIdentityKey != "") {
		return nil, fmt.Errorf("%w: specify exactly one of toAddress or toIdentityKey", ErrInvalidArgs)
	}

	var lockScript *script.Script
	var prefixB64, suffixB64, senderHex string
	var err error
	if args.ToAddress != "" {
		addr, aErr := script.NewAddressFromString(strings.TrimSpace(args.ToAddress))
		if aErr != nil {
			return nil, fmt.Errorf("%w: invalid address %q: %v", ErrInvalidArgs, args.ToAddress, aErr)
		}
		lockScript, err = p2pkh.Lock(addr)
		if err != nil {
			return nil, err
		}
	} else {
		prefixB64 = strings.TrimSpace(args.DerivationPrefix)
		suffixB64 = strings.TrimSpace(args.DerivationSuffix)
		if prefixB64 == "" {
			var preRaw [16]byte
			if _, err := rand.Read(preRaw[:]); err != nil {
				return nil, fmt.Errorf("wallet: crypto/rand failed: %w", err)
			}
			prefixB64 = base64.StdEncoding.EncodeToString(preRaw[:])
		}
		if suffixB64 == "" {
			var sufRaw [16]byte
			if _, err := rand.Read(sufRaw[:]); err != nil {
				return nil, fmt.Errorf("wallet: crypto/rand failed: %w", err)
			}
			suffixB64 = base64.StdEncoding.EncodeToString(sufRaw[:])
		}
		senderHex = e.IdentityPubHex()

		recipPub, pErr := guard.ParsePubKeyHex(strings.TrimSpace(args.ToIdentityKey))
		if pErr != nil {
			return nil, fmt.Errorf("%w: invalid toIdentityKey: %v", ErrInvalidArgs, pErr)
		}
		if !e.cfg.Mainnet {
			lockScript, err = brc29.LockForCounterparty(e.rootKey, brc29.KeyID{
				DerivationPrefix: prefixB64,
				DerivationSuffix: suffixB64,
			}, recipPub, brc29.WithTestNet())
		} else {
			lockScript, err = brc29.LockForCounterparty(e.rootKey, brc29.KeyID{
				DerivationPrefix: prefixB64,
				DerivationSuffix: suffixB64,
			}, recipPub)
		}
		if err != nil {
			return nil, fmt.Errorf("wallet: derive BRC-29 payment script: %w", err)
		}
	}

	desc := ensureDesc(args.Description, "nektar wallet send")
	noSend := args.NoSend
	acceptDelayed := args.AcceptDelayed
	randomize := false
	caRes, err := e.createActionGuarded(ctx, sdkwallet.CreateActionArgs{
		Description: desc,
		Labels:      args.Labels,
		Outputs: []sdkwallet.CreateActionOutput{{
			LockingScript:     *lockScript,
			Satoshis:          args.Satoshis,
			OutputDescription: desc,
		}},
		Options: &sdkwallet.CreateActionOptions{
			NoSend:                 &noSend,
			AcceptDelayedBroadcast: &acceptDelayed,
			RandomizeOutputs:       &randomize,
		},
	}, e.cfg.Originator, args.OverrideGuard, false)
	if err != nil {
		return nil, err
	}

	txid := caRes.Txid.String()
	var parsed *transaction.Transaction
	if len(caRes.Tx) > 0 {
		_, parsed, _, _ = guard.ParseBEEF(caRes.Tx, guard.DefaultBound)
	}
	if parsed == nil {
		parsed, _ = e.LoadParsedTx(ctx, txid)
	}
	outIdx := uint32(0)
	inputCount := 0
	var feeSats uint64
	if parsed != nil {
		inputCount = len(parsed.Inputs)
		feeSats = computeTxFee(parsed)
		for idx, o := range parsed.Outputs {
			if o.LockingScript != nil && string(*o.LockingScript) == string(*lockScript) && o.Satoshis == args.Satoshis {
				outIdx = uint32(idx)
				break
			}
		}
	}
	return &SendResult{
		TxID:              txid,
		Satoshis:          args.Satoshis,
		FeeSats:           feeSats,
		InputCount:        inputCount,
		BEEF:              caRes.Tx,
		BEEFHex:           hex.EncodeToString(caRes.Tx),
		Tx:                parsed,
		DerivationPrefix:  prefixB64,
		DerivationSuffix:  suffixB64,
		SenderIdentityKey: senderHex,
		OutputIndex:       outIdx,
	}, nil
}

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
	allOuts, err := e.provider.OutputsEntity().Read().
		UserID().Equals(e.userID).
		Spendable().Equals(true).
		Find(ctx)
	if err != nil {
		return nil, err
	}
	utxos, _ := e.provider.UserUTXOEntity().Read().UserID(e.userID).Find(ctx)
	reservedByOut := make(map[uint]bool, len(utxos))
	for _, u := range utxos {
		if u.ReservedByID != nil {
			reservedByOut[u.OutputID] = true
		}
	}

	var defaultCount int
	var explicit []*entity.Output
	var totalAvail uint64
	for _, o := range allOuts {
		if o.SpentBy != nil || o.TxID == nil || *o.TxID == "" || o.Satoshis <= 0 || reservedByOut[o.ID] {
			continue
		}
		if !isSpendableParentStatus(o.TxStatus) {
			continue
		}
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
			pendingID, _ = e.SavePendingBRC29(ctx, PendingBRC29Record{
				Direction:        "sweep",
				WireURL:          args.WireURL,
				CounterpartyPub:  strings.TrimSpace(args.ToIdentityKey),
				DerivationPrefix: preB64,
				DerivationSuffix: sufB64,
				LockingScriptHex: hex.EncodeToString(*lockScript),
			})
		}
		txid, beef, parsed, sweptSats, feeSats, inputsUsed, sErr := e.sweepExplicitChunk(ctx, chunk, includeDefault, lockScript, desc)
		if sErr != nil && len(chunk) > 1 {
			if healthy, pruned := e.filterVerifiedUnspentOutputs(ctx, chunk); pruned > 0 && len(healthy) > 0 {
				txid, beef, parsed, sweptSats, feeSats, inputsUsed, sErr = e.sweepExplicitChunk(ctx, healthy, includeDefault, lockScript, desc)
			}
		}
		if sErr != nil {
			return out, sErr
		}
		if includeDefault {
			defaultSwept = true
		}
		e.appendSweepChunkResult(out, txid, beef, parsed, sweptSats, feeSats, inputsUsed, lockScript, preB64, sufB64)
		if pendingID > 0 {
			outIdx := out.OutputIndices[len(out.OutputIndices)-1]
			_ = e.UpdatePendingBRC29Tx(ctx, pendingID, txid, outIdx, beef, sweptSats)
		}
	}

	// Step 2: Sweep remaining "default" basket via go-wallet-toolbox's native SweepMaxSatoshis!
	if defaultCount > 0 && !defaultSwept {
		lockScript, preB64, sufB64, lErr := makeDestLock()
		if lErr != nil {
			return out, lErr
		}
		var pendingID int64
		if preB64 != "" {
			pendingID, _ = e.SavePendingBRC29(ctx, PendingBRC29Record{
				Direction:        "sweep",
				WireURL:          args.WireURL,
				CounterpartyPub:  strings.TrimSpace(args.ToIdentityKey),
				DerivationPrefix: preB64,
				DerivationSuffix: sufB64,
				LockingScriptHex: hex.EncodeToString(*lockScript),
			})
		}
		acceptDelayed := false
		randomize := false
		caRes, cErr := e.tb.CreateAction(ctx, sdkwallet.CreateActionArgs{
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
			feeSats = computeTxFee(parsed)
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
			_ = e.UpdatePendingBRC29Tx(ctx, pendingID, txid, outIdx, caRes.Tx, sweptSats)
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
	created, err := e.tb.CreateAction(ctx, sdkwallet.CreateActionArgs{
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
	}, e.cfg.Originator)
	if err != nil {
		return "", nil, nil, 0, 0, 0, fmt.Errorf("wallet: sweep CreateAction: %w", err)
	}
	if created.SignableTransaction == nil {
		return "", nil, nil, 0, 0, 0, errors.New("wallet: sweep did not return SignableTransaction")
	}

	tx, err := transaction.NewTransactionFromBEEF(created.SignableTransaction.Tx)
	if err != nil {
		_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
		return "", nil, nil, 0, 0, 0, err
	}

	spends := make(map[uint32]sdkwallet.SignActionSpend, len(chunk))
	for idx, o := range chunk {
		vin := uint32(idx)
		unlocker, uErr := e.unlockerForEntity(o)
		if uErr != nil {
			_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
			return "", nil, nil, 0, 0, 0, fmt.Errorf("wallet: sweep unlocker input %d: %w", vin, uErr)
		}
		us, sErr := unlocker.Sign(tx, vin)
		if sErr != nil {
			_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
			return "", nil, nil, 0, 0, 0, fmt.Errorf("wallet: sweep sign input %d: %w", vin, sErr)
		}
		seqFinal := uint32(0xffffffff)
		spends[vin] = sdkwallet.SignActionSpend{
			UnlockingScript: *us,
			SequenceNumber:  &seqFinal,
		}
	}

	signed, err := e.tb.SignAction(ctx, sdkwallet.SignActionArgs{
		Reference: created.SignableTransaction.Reference,
		Spends:    spends,
		Options: &sdkwallet.SignActionOptions{
			AcceptDelayedBroadcast: &acceptDelayed,
		},
	}, e.cfg.Originator)
	if err != nil {
		return "", nil, nil, 0, 0, 0, fmt.Errorf("wallet: sweep SignAction: %w", err)
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
		feeSats = computeTxFee(parsed)
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
