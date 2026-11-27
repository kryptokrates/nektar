package wallet

import (
	"context"
	"fmt"
	"sort"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/entity"
	"github.com/lightwebinc/bcommon/guard"
)

// -----------------------------------------------------------------------------
// Pool Reclaim & UTXO Consolidation
// -----------------------------------------------------------------------------

// reclaimPoolToDefault moves just enough spendable UTXOs from "reserve" (first) and "fuel" (second)
// into "default" so a single large action whose target exceeds both "fuel" and "default" individually
// (while still <= total wallet balance) succeeds cleanly.
func (e *Engine) reclaimPoolToDefault(ctx context.Context, neededTotal uint64) (int, error) {
	if !e.svc.hasBroadcaster() {
		return 0, nil
	}
	e.absorbMu.Lock()
	defer e.absorbMu.Unlock()

	bal, err := e.Balance(ctx)
	if err != nil {
		return 0, err
	}
	if bal.ReserveSats+bal.FuelSats == 0 {
		return 0, nil
	}
	var shortfall uint64
	if neededTotal > bal.DefaultSats {
		shortfall = neededTotal - bal.DefaultSats
	}
	if shortfall == 0 {
		shortfall = 1000
	}
	if bal.ReserveSats+bal.FuelSats < shortfall {
		return 0, nil
	}

	outs, err := e.listUnreservedSpendableOutputs(ctx)
	if err != nil {
		return 0, err
	}

	var reserveOuts, fuelOuts []*entity.Output
	for _, o := range outs {
		if o.BasketName == nil {
			continue
		}
		switch *o.BasketName {
		case BasketReserve:
			reserveOuts = append(reserveOuts, o)
		case BasketFuel:
			fuelOuts = append(fuelOuts, o)
		}
	}

	// Prefer reclaiming larger reserve chunks first, then fuel UTXOs only for the remaining shortfall.
	sort.Slice(reserveOuts, func(i, j int) bool { return reserveOuts[i].Satoshis > reserveOuts[j].Satoshis })
	sort.Slice(fuelOuts, func(i, j int) bool { return fuelOuts[i].Satoshis > fuelOuts[j].Satoshis })
	candidates := append(reserveOuts, fuelOuts...)

	var picked []*entity.Output
	var pickedSum uint64
	target := shortfall + e.estimateFeeSats(ctx, 8, 2) + 500
	for _, o := range candidates {
		picked = append(picked, o)
		pickedSum += uint64(o.Satoshis)
		if pickedSum >= target || len(picked) >= 100 {
			break
		}
	}
	if len(picked) == 0 {
		return 0, nil
	}

	combinedBeef := transaction.NewBeef()
	inputs := make([]sdkwallet.CreateActionInput, 0, len(picked))
	seenTx := make(map[string]bool)
	for _, o := range picked {
		id := *o.TxID
		if !seenTx[id] {
			if mErr := e.mergeTxIntoBeef(ctx, combinedBeef, id); mErr != nil {
				return 0, mErr
			}
			seenTx[id] = true
		}
		h, hErr := chainhash.NewHashFromHex(id)
		if hErr != nil {
			return 0, fmt.Errorf("wallet: reclaim invalid parent txid %q: %w", id, hErr)
		}
		seqFinal := uint32(0xffffffff)
		inputs = append(inputs, sdkwallet.CreateActionInput{
			Outpoint:              transaction.Outpoint{Txid: *h, Index: o.Vout},
			InputDescription:      "reclaim throughput pool UTXO to default",
			UnlockingScriptLength: 107,
			SequenceNumber:        &seqFinal,
		})
	}
	inputBeefBytes, err := combinedBeef.Bytes()
	if err != nil {
		return 0, err
	}

	signAndProc := false
	acceptDelayed := false
	// Internal self-move from reserve/fuel into default basket: external spend is 0 (fee-only).
	if _, err = e.runExplicitSignAction(ctx, sdkwallet.CreateActionArgs{
		Description: "reclaim throughput pool UTXOs into default basket",
		Labels:      []string{"nektar", "reclaim"},
		InputBEEF:   inputBeefBytes,
		Inputs:      inputs,
		Outputs:     nil,
		Options: &sdkwallet.CreateActionOptions{
			SignAndProcess:         &signAndProc,
			AcceptDelayedBroadcast: &acceptDelayed,
		},
	}, withExternalSpend(ctx, 0), func(tx *transaction.Transaction) (map[uint32]sdkwallet.SignActionSpend, error) {
		spends := make(map[uint32]sdkwallet.SignActionSpend, len(picked))
		for idx, o := range picked {
			vin := uint32(idx)
			unlocker, uErr := e.unlockerForEntity(o)
			if uErr != nil {
				return nil, uErr
			}
			us, sErr := unlocker.Sign(tx, vin)
			if sErr != nil {
				return nil, sErr
			}
			seqFinal := uint32(0xffffffff)
			spends[vin] = sdkwallet.SignActionSpend{UnlockingScript: *us, SequenceNumber: &seqFinal}
		}
		return spends, nil
	}, acceptDelayed, false); err != nil {
		return 0, err
	}
	return len(picked), nil
}

// Consolidate merges fragmented small UTXOs (below args.BelowSats) into one clean output in targetBasket.
func (e *Engine) Consolidate(ctx context.Context, args ConsolidateArgs) (*ConsolidateResult, error) {
	_, _ = e.AbsorbFundingBasket(ctx)
	below := args.BelowSats
	if below == 0 {
		below = 10_000
	}
	maxIn := args.MaxInputs
	if maxIn <= 0 {
		maxIn = 100
	}
	targetBasket := args.TargetBasket
	if targetBasket == "" {
		targetBasket = BasketReserve
	}

	outs, err := e.listUnreservedSpendableOutputs(ctx)
	if err != nil {
		return nil, err
	}

	var small []*entity.Output
	for _, o := range outs {
		bName := ""
		if o.BasketName != nil {
			bName = *o.BasketName
		}
		if bName != BasketDefault && bName != BasketFuel {
			continue
		}
		if uint64(o.Satoshis) <= below {
			small = append(small, o)
		}
	}
	if len(small) < 2 {
		return &ConsolidateResult{TargetBasket: targetBasket}, nil
	}
	if len(small) > maxIn {
		small = small[:maxIn]
	}

	var totalIn uint64
	combinedBeef := transaction.NewBeef()
	inputs := make([]sdkwallet.CreateActionInput, 0, len(small))
	seenTx := make(map[string]bool)
	for _, o := range small {
		id := *o.TxID
		if !seenTx[id] {
			if mErr := e.mergeTxIntoBeef(ctx, combinedBeef, id); mErr != nil {
				return nil, mErr
			}
			seenTx[id] = true
		}
		h, hErr := chainhash.NewHashFromHex(id)
		if hErr != nil {
			return nil, fmt.Errorf("wallet: consolidate invalid parent txid %q: %w", id, hErr)
		}
		totalIn += uint64(o.Satoshis)
		seqFinal := uint32(0xffffffff)
		inputs = append(inputs, sdkwallet.CreateActionInput{
			Outpoint:              transaction.Outpoint{Txid: *h, Index: o.Vout},
			InputDescription:      "nektar consolidate small UTXO",
			UnlockingScriptLength: 107,
			SequenceNumber:        &seqFinal,
		})
	}
	estFee := e.estimateFeeSats(ctx, len(inputs), 1)
	if totalIn <= estFee+100 {
		return &ConsolidateResult{TargetBasket: targetBasket}, nil
	}
	inputBeefBytes, err := combinedBeef.Bytes()
	if err != nil {
		return nil, err
	}

	// Consolidate cleanly into native BRC-29 "default" change basket (or shaped reserve via FanOutFuel).
	signAndProc := false
	acceptDelayed := false
	// Internal self-move from reserve/fuel into default basket: external spend is 0 (fee-only).
	signed, err := e.runExplicitSignAction(ctx, sdkwallet.CreateActionArgs{
		Description: "nektar UTXO consolidation",
		Labels:      []string{"nektar", "consolidate"},
		InputBEEF:   inputBeefBytes,
		Inputs:      inputs,
		Outputs:     nil,
		Options: &sdkwallet.CreateActionOptions{
			SignAndProcess:         &signAndProc,
			AcceptDelayedBroadcast: &acceptDelayed,
		},
	}, withExternalSpend(ctx, 0), func(tx *transaction.Transaction) (map[uint32]sdkwallet.SignActionSpend, error) {
		spends := make(map[uint32]sdkwallet.SignActionSpend, len(small))
		for idx, o := range small {
			vin := uint32(idx)
			unlocker, uErr := e.unlockerForEntity(o)
			if uErr != nil {
				return nil, uErr
			}
			us, sErr := unlocker.Sign(tx, vin)
			if sErr != nil {
				return nil, sErr
			}
			seqFinal := uint32(0xffffffff)
			spends[vin] = sdkwallet.SignActionSpend{UnlockingScript: *us, SequenceNumber: &seqFinal}
		}
		return spends, nil
	}, acceptDelayed, false)
	if err != nil {
		return nil, fmt.Errorf("wallet: consolidate: %w", err)
	}
	txid := signed.Txid.String()
	var feeSats, outSats uint64
	if len(signed.Tx) > 0 {
		if _, parsed, _, pErr := guard.ParseBEEF(signed.Tx, guard.DefaultBound); pErr == nil && parsed != nil {
			feeSats = ComputeTxFee(parsed)
			for _, o := range parsed.Outputs {
				outSats += o.Satoshis
			}
			e.txCache.Put(txid, parsed)
		}
	}
	return &ConsolidateResult{
		TxID:         txid,
		MergedCount:  len(small),
		OutputSats:   outSats,
		FeeSats:      feeSats,
		TargetBasket: BasketDefault,
	}, nil
}
