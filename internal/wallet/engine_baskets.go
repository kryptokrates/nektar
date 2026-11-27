package wallet

import (
	"context"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/brc29"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/entity"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk"
	"github.com/lightwebinc/bcommon/guard"
)

// -----------------------------------------------------------------------------
// Static FundAddress() -> Native BRC-29 Absorption & Basket Helpers
// -----------------------------------------------------------------------------

// AbsorbFundingBasket sweeps any spendable static-P2PKH outputs in "nektar funding"
// (from FundAddress() deposits, faucets, testchain coinbases, or legacy wallet.json)
// into go-wallet-toolbox's native BRC-29 "default" change basket via a single BRC-100 action.
func (e *Engine) AbsorbFundingBasket(ctx context.Context) (int, error) {
	if !e.svc.hasBroadcaster() {
		return 0, nil
	}
	e.absorbMu.Lock()
	defer e.absorbMu.Unlock()

	outs, err := e.provider.OutputsEntity().Read().
		UserID().Equals(e.userID).
		BasketName().Equals(BasketFunding).
		Spendable().Equals(true).
		Find(ctx)
	if err != nil || len(outs) == 0 {
		return 0, err
	}

	// Filter to unspent outputs with a txid, positive satoshis, and spendable parent tx status.
	var eligible []*entity.Output
	for _, o := range outs {
		if o.TxID == nil || *o.TxID == "" || o.Satoshis <= 0 || o.SpentBy != nil {
			continue
		}
		if !isSpendableParentStatus(o.TxStatus) {
			continue
		}
		eligible = append(eligible, o)
	}
	if len(eligible) == 0 {
		return 0, nil
	}

	absorbed := 0
	const chunkSize = 100
	for start := 0; start < len(eligible); start += chunkSize {
		end := min(start+chunkSize, len(eligible))
		chunk := eligible[start:end]
		if err := e.absorbFundingChunk(ctx, chunk); err != nil {
			// On multi-input failure, isolate any output with verified spend evidence (never absence!)
			// and retry with the remaining unspent inputs so one dead UTXO cannot stall a 100-input chunk.
			if len(chunk) > 1 {
				healthy, pruned := e.filterVerifiedUnspentOutputs(ctx, chunk)
				if pruned > 0 && len(healthy) > 0 {
					if rErr := e.absorbFundingChunk(ctx, healthy); rErr == nil {
						absorbed += len(healthy)
						continue
					}
				}
			}
			return absorbed, err
		}
		absorbed += len(chunk)
	}
	return absorbed, nil
}

// filterVerifiedUnspentOutputs marks outputs Spendable=false ONLY when local spent_by or
// cryptographically verified external spending tx bytes prove the output is already spent.
func (e *Engine) filterVerifiedUnspentOutputs(ctx context.Context, chunk []*entity.Output) (healthy []*entity.Output, pruned int) {
	f := false
	for _, o := range chunk {
		if o.TxID == nil || *o.TxID == "" {
			continue
		}
		if o.SpentBy != nil {
			pruned++
			continue
		}
		if spent, err := e.verifyOutpointSpent(ctx, *o.TxID, o.Vout, uint64(o.Satoshis), o.LockingScript); err == nil && spent {
			_ = e.provider.OutputsEntity().Update(ctx, &entity.OutputUpdateSpecification{
				ID:        o.ID,
				Spendable: &f,
			})
			pruned++
			continue
		}
		healthy = append(healthy, o)
	}
	return healthy, pruned
}

func (e *Engine) absorbFundingChunk(ctx context.Context, chunk []*entity.Output) error {
	combinedBeef := transaction.NewBeef()
	inputs := make([]sdkwallet.CreateActionInput, 0, len(chunk))
	seenTx := make(map[string]bool)

	for _, o := range chunk {
		txid := *o.TxID
		if !seenTx[txid] {
			if err := e.mergeTxIntoBeef(ctx, combinedBeef, txid); err != nil {
				return fmt.Errorf("wallet: absorb load parent %s: %w", txid, err)
			}
			seenTx[txid] = true
		}
		h, err := chainhash.NewHashFromHex(txid)
		if err != nil {
			return err
		}
		seqFinal := uint32(0xffffffff)
		inputs = append(inputs, sdkwallet.CreateActionInput{
			Outpoint:              transaction.Outpoint{Txid: *h, Index: o.Vout},
			InputDescription:      "absorb static fund address deposit",
			UnlockingScriptLength: 107,
			SequenceNumber:        &seqFinal,
		})
	}

	inputBeefBytes, err := combinedBeef.Bytes()
	if err != nil {
		return fmt.Errorf("wallet: encode absorb inputBEEF: %w", err)
	}

	signAndProc := false
	acceptDelayed := false
	// Internal self-move from static FundAddress into default basket: external spend is 0 (fee-only).
	_, err = e.runExplicitSignAction(ctx, sdkwallet.CreateActionArgs{
		Description: "absorb funding deposits into default basket",
		Labels:      []string{"nektar", "absorb"},
		InputBEEF:   inputBeefBytes,
		Inputs:      inputs,
		Outputs:     nil, // 100% net value returns to go-wallet-toolbox's native BRC-29 "default" change basket!
		Options: &sdkwallet.CreateActionOptions{
			SignAndProcess:         &signAndProc,
			AcceptDelayedBroadcast: &acceptDelayed,
		},
	}, withExternalSpend(ctx, 0), func(tx *transaction.Transaction) (map[uint32]sdkwallet.SignActionSpend, error) {
		unlocker, uErr := p2pkh.Unlock(e.fundKey, nil)
		if uErr != nil {
			return nil, uErr
		}
		spends := make(map[uint32]sdkwallet.SignActionSpend, len(inputs))
		for idx := range inputs {
			vin := uint32(idx)
			if int(vin) >= len(tx.Inputs) {
				break
			}
			us, sErr := unlocker.Sign(tx, vin)
			if sErr != nil {
				return nil, fmt.Errorf("wallet: absorb sign input %d: %w", vin, sErr)
			}
			seqFinal := uint32(0xffffffff)
			spends[vin] = sdkwallet.SignActionSpend{
				UnlockingScript: *us,
				SequenceNumber:  &seqFinal,
			}
		}
		return spends, nil
	}, acceptDelayed, false)
	if err != nil {
		return fmt.Errorf("wallet: absorb funding: %w", err)
	}
	return nil
}

// listUnreservedSpendableOutputs returns all spendable, unspent, non-reserved outputs belonging to
// spendable parent transactions. Fails closed if the user_utxos reservation query fails.
func (e *Engine) listUnreservedSpendableOutputs(ctx context.Context) ([]*entity.Output, error) {
	outs, err := e.provider.OutputsEntity().Read().
		UserID().Equals(e.userID).
		Spendable().Equals(true).
		Find(ctx)
	if err != nil {
		return nil, err
	}
	utxos, err := e.provider.UserUTXOEntity().Read().UserID(e.userID).Find(ctx)
	if err != nil {
		return nil, fmt.Errorf("wallet: load reserved UTXOs: %w", err)
	}
	reserved := make(map[uint]bool, len(utxos))
	for _, u := range utxos {
		if u.ReservedByID != nil {
			reserved[u.OutputID] = true
		}
	}
	eligible := make([]*entity.Output, 0, len(outs))
	for _, o := range outs {
		if o.SpentBy != nil || o.TxID == nil || *o.TxID == "" || o.Satoshis <= 0 || reserved[o.ID] {
			continue
		}
		if !isSpendableParentStatus(o.TxStatus) {
			continue
		}
		eligible = append(eligible, o)
	}
	return eligible, nil
}

func (e *Engine) mergeTxIntoBeef(ctx context.Context, dst *transaction.Beef, txid string) error {
	if beef, err := e.provider.GetBeefForTransaction(ctx, txid, wdk.StorageGetBeefOptions{}); err == nil && beef != nil {
		return dst.MergeBeef(beef)
	}
	if tx, err := e.LoadParsedTx(ctx, txid); err == nil && tx != nil {
		_, err = dst.MergeTransaction(tx)
		return err
	}
	return fmt.Errorf("tx %s not available for InputBEEF", txid)
}

func (e *Engine) unlockerForEntity(o *entity.Output) (transaction.UnlockingScriptTemplate, error) {
	bName := ""
	if o.BasketName != nil {
		bName = *o.BasketName
	}
	if bName == BasketFunding {
		return p2pkh.Unlock(e.fundKey, nil)
	}
	if o.DerivationPrefix != nil && *o.DerivationPrefix != "" && o.DerivationSuffix != nil && *o.DerivationSuffix != "" {
		senderPub := e.rootKey.PubKey()
		if o.SenderIdentityKey != nil && *o.SenderIdentityKey != "" {
			parsed, err := guard.ParsePubKeyHex(*o.SenderIdentityKey)
			if err != nil || parsed == nil {
				return nil, fmt.Errorf("wallet: invalid BRC-29 senderIdentityKey %q: %w", *o.SenderIdentityKey, err)
			}
			senderPub = parsed
		}
		return brc29.Unlock(senderPub, brc29.KeyID{
			DerivationPrefix: *o.DerivationPrefix,
			DerivationSuffix: *o.DerivationSuffix,
		}, e.rootKey)
	}
	return nil, fmt.Errorf("wallet: unrecognized output basket %q without BRC-29 derivation metadata", bName)
}
