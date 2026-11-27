package contracts

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"
)

const maxAnchorRotateHops = 16

// VerifyAnchorUTXOLineageWithBEEF verifies strict singleton UTXO spend lineage from prevTx:0 to nextTx:0,
// executing the full post-Chronicle Bitcoin Script VM on every hop.
//
// In the common Append case, nextTx directly spends prevTx:0 (1 hop). When one or more on-chain
// NektarAnchor.Rotate() spends occurred at batch n (e.g. tx_{n-1} -> tx_n -> rot1 -> ... -> rot_k),
// nextBeef carries the intermediate same-batch anchor transactions; every intermediate hop is
// independently verified through VerifyAnchorUTXOLineage + the post-Chronicle Bitcoin Script VM.
func VerifyAnchorUTXOLineageWithBEEF(prevTx, nextTx *transaction.Transaction, nextBeef *transaction.Beef) error {
	if prevTx == nil || nextTx == nil {
		return errors.New("contracts: VerifyAnchorUTXOLineageWithBEEF requires non-nil prevTx and nextTx")
	}
	if len(prevTx.Outputs) == 0 || prevTx.Outputs[0] == nil || prevTx.Outputs[0].LockingScript == nil {
		return errors.New("contracts: prevTx missing output 0")
	}
	if len(nextTx.Outputs) == 0 || nextTx.Outputs[0] == nil || nextTx.Outputs[0].LockingScript == nil {
		return errors.New("contracts: nextTx missing output 0")
	}
	prevSt, err := DecodeNektarAnchor(*prevTx.Outputs[0].LockingScript)
	if err != nil {
		return fmt.Errorf("contracts: decode prev anchor: %w", err)
	}
	nextSt, err := DecodeNektarAnchor(*nextTx.Outputs[0].LockingScript)
	if err != nil {
		return fmt.Errorf("contracts: decode next anchor: %w", err)
	}
	if nextSt.N != prevSt.N+1 {
		return fmt.Errorf("contracts: anchor batch lineage must advance n by 1 (prev=%d next=%d)", prevSt.N, nextSt.N)
	}
	if len(nextTx.Inputs) == 0 || nextTx.Inputs[0] == nil || nextTx.Inputs[0].SourceTXID == nil {
		return errors.New("contracts: nextTx missing input 0")
	}
	prevID := prevTx.TxID()
	if nextTx.Inputs[0].SourceTXID.IsEqual(prevID) && nextTx.Inputs[0].SourceTxOutIndex == 0 {
		return VerifyAnchorUTXOLineage(prevTx, nextTx)
	}

	// Walk backward across intermediate NektarAnchor.Rotate() hops carried inside nextBeef.
	cur := nextTx
	visited := make(map[string]bool, maxAnchorRotateHops)
	for hop := 0; hop < maxAnchorRotateHops; hop++ {
		curID := cur.TxID().String()
		if visited[curID] {
			return errors.New("contracts: cycle detected in anchor UTXO lineage")
		}
		visited[curID] = true

		if len(cur.Inputs) == 0 || cur.Inputs[0] == nil || cur.Inputs[0].SourceTXID == nil {
			return fmt.Errorf("contracts: anchor tx %s missing input 0", curID)
		}
		in0 := cur.Inputs[0]
		if in0.SourceTxOutIndex != 0 {
			return fmt.Errorf("contracts: anchor lineage broken: tx %s input 0 spends vout %d (must spend singleton vout 0)", curID, in0.SourceTxOutIndex)
		}
		var parentTx *transaction.Transaction
		if in0.SourceTXID.IsEqual(prevID) {
			parentTx = prevTx
		} else if in0.SourceTransaction != nil && in0.SourceTransaction.TxID().IsEqual(in0.SourceTXID) {
			parentTx = in0.SourceTransaction
		} else if nextBeef != nil {
			parentTx = nextBeef.FindTransaction(in0.SourceTXID.String())
		}
		if parentTx == nil {
			return fmt.Errorf("contracts: anchor lineage broken: tx %s input 0 spends %s:0, expected %s:0 (missing ancestor in BEEF)",
				curID, in0.SourceTXID.String(), prevID.String())
		}
		if err := VerifyAnchorUTXOLineage(parentTx, cur); err != nil {
			return err
		}
		if parentTx.TxID().IsEqual(prevID) {
			return nil
		}
		cur = parentTx
	}
	return fmt.Errorf("contracts: anchor lineage broken: exceeded max %d rotate hops without reaching %s", maxAnchorRotateHops, prevID.String())
}

// MergeAnchorLineageBEEF embeds the transactions and BUMPs from prevBeef into headBeef as a valid
// BRC-95 AtomicBEEF (subject = headTxID) WITHOUT pruning mined same-batch ancestor transactions.
// Used on NektarAnchor.Rotate() and BUMP refreshes so offline verifiers always have every intermediate
// rotation transaction needed to run the post-Chronicle Bitcoin Script VM back to batch n-1.
func MergeAnchorLineageBEEF(headBeef, prevBeef []byte, headTxID *chainhash.Hash) ([]byte, error) {
	if len(headBeef) == 0 || headTxID == nil {
		return headBeef, errors.New("contracts: MergeAnchorLineageBEEF requires non-empty headBeef and headTxID")
	}
	b, _, _, err := guard.ParseBEEF(headBeef, guard.DefaultBound)
	if err != nil || b == nil {
		return headBeef, fmt.Errorf("contracts: parse headBeef: %w", err)
	}
	if len(prevBeef) > 0 {
		if prevB, _, _, pErr := guard.ParseBEEF(prevBeef, guard.DefaultBound); pErr == nil && prevB != nil {
			// Ensure headBeef (newBeef) always takes precedence over prevBeef (existingBeef) on reorg:
			// only copy ancestor transactions from prevB that are not already present in headBeef.
			for _, btx := range prevB.Transactions {
				if btx == nil || btx.Transaction == nil {
					continue
				}
				txidStr := btx.Transaction.TxID().String()
				if b.FindTransaction(txidStr) != nil {
					continue
				}
				if _, mErr := b.MergeTransaction(btx.Transaction); mErr != nil {
					return headBeef, fmt.Errorf("contracts: merge prev anchor lineage tx %s: %w", txidStr, mErr)
				}
			}
		}
	}
	if b.FindTransaction(headTxID.String()) == nil {
		return headBeef, fmt.Errorf("contracts: merged BEEF missing subject tx %s", headTxID.String())
	}
	body, err := b.Bytes()
	if err != nil {
		return headBeef, fmt.Errorf("contracts: encode merged lineage BEEF: %w", err)
	}
	out := make([]byte, 4+chainhash.HashSize+len(body))
	binary.LittleEndian.PutUint32(out[0:4], transaction.ATOMIC_BEEF)
	copy(out[4:4+chainhash.HashSize], headTxID[:])
	copy(out[4+chainhash.HashSize:], body)
	return out, nil
}
