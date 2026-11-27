package memory

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

// RotateShardOwner executes an on-chain NektarAnchor.Rotate() spend transferring shard anchor
// ownership from the current ownerPkh to newOwnerPubOrPKHHex (33-byte compressed pubkey hex
// or 20-byte PKH hex), preserving n, R, Rp, and immutable GenesisOwner shard identity.
func (s *Service) RotateShardOwner(ctx context.Context, shard string, newOwnerPubOrPKHHex string) (*store.BatchAnchor, error) {
	unlock := s.lockShard(shard)
	defer unlock()

	newOwnerHex := strings.TrimPrefix(strings.TrimSpace(newOwnerPubOrPKHHex), "0x")
	rawNew, err := hex.DecodeString(newOwnerHex)
	if err != nil {
		return nil, fmt.Errorf("memory: invalid newOwner hex: %w", err)
	}
	var newOwnerPKH []byte
	switch len(rawNew) {
	case 20:
		newOwnerPKH = rawNew
	case 33:
		if _, _, pErr := core.ParseStrictCompressedPubKey(newOwnerHex); pErr != nil {
			return nil, fmt.Errorf("memory: invalid newOwner compressed pubkey: %w", pErr)
		}
		newOwnerPKH = core.Hash160(rawNew)
	default:
		return nil, fmt.Errorf("memory: newOwner must be 33-byte compressed pubkey hex or 20-byte PKH hex (got %d bytes)", len(rawNew))
	}

	anchors, err := s.db.ListAnchors(shard)
	if err != nil {
		return nil, err
	}
	if len(anchors) == 0 {
		return nil, fmt.Errorf("memory: shard %q has no existing anchor to rotate", shard)
	}
	last := anchors[len(anchors)-1]
	var prevTx *transaction.Transaction
	if len(last.Beef) > 0 {
		if _, parsed, _, pErr := guard.ParseBEEF(last.Beef, guard.DefaultBound); pErr == nil && parsed != nil {
			prevTx = parsed
		}
	}
	if prevTx == nil {
		prevTx = s.port.Tx(last.TxID)
	}
	if prevTx == nil || len(prevTx.Outputs) == 0 || prevTx.Outputs[0] == nil || prevTx.Outputs[0].LockingScript == nil {
		return nil, fmt.Errorf("memory: prior anchor tx %s unavailable for Rotate", last.TxID)
	}
	if !contracts.IsSpendableNektarAnchorScript(*prevTx.Outputs[0].LockingScript) {
		return nil, fmt.Errorf("memory: prior anchor %s is a legacy non-spendable anchor (re-anchor a fresh shard)", last.TxID)
	}
	prevSt, err := contracts.DecodeNektarAnchor(*prevTx.Outputs[0].LockingScript)
	if err != nil {
		return nil, err
	}
	rotSt, err := contracts.RotatedAnchorState(*prevSt, newOwnerPKH)
	if err != nil {
		return nil, err
	}
	rotLock, err := contracts.EncodeAnchorLockingScript(rotSt)
	if err != nil {
		return nil, err
	}

	cRes, err := s.port.BuildCovenantSpendTx(ctx, chain.CovenantSpendRequest{
		InputTx:               prevTx,
		InputBEEF:             last.Beef,
		InputVout:             0,
		Outputs:               []*transaction.TransactionOutput{chain.AnchorOutput(rotLock)},
		UnlockingScriptLength: 5200,
		FuelFromWallet:        true,
		Description:           fmt.Sprintf("nektar anchor rotate %s#%d", shard, last.N),
		Labels:                []string{"anchor", "rotate"},
		BuildUnlock: func(spendTx *transaction.Transaction, changePKH []byte, changeSats uint64) (*script.Script, error) {
			return contracts.BuildAnchorRotateUnlock(
				ctx, spendTx, 0, prevTx.Outputs[0], newOwnerPKH,
				changePKH, changeSats, s.port.SignCovenantSighash,
			)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("memory: NektarAnchor.Rotate: %w", err)
	}
	rotTxID := cRes.Tx.TxID().String()
	preBroadcastBeef := cRes.BEEF
	if len(preBroadcastBeef) == 0 {
		if b, bErr := chain.SafeAtomicBEEF(cRes.Tx); bErr == nil {
			preBroadcastBeef = b
		}
	}
	if len(last.Beef) > 0 && len(preBroadcastBeef) > 0 {
		if merged, mErr := contracts.MergeAnchorLineageBEEF(preBroadcastBeef, last.Beef, cRes.Tx.TxID()); mErr == nil && len(merged) > 0 {
			preBroadcastBeef = merged
		}
	}
	// Persist pending rotation BEFORE broadcast so a crash between Broadcast and UpdateAnchorHead
	// is cleanly recovered on restart by ReconcilePendingAnchors (never bricking subsequent Appends!).
	if err := s.db.SavePendingAnchor(store.PendingAnchor{
		Shard:      shard,
		N:          last.N,
		Kind:       "rotate",
		RootHex:    last.RootHex,
		ChainedHex: last.ChainedHex,
		TxID:       rotTxID,
		Beef:       preBroadcastBeef,
		CreatedAt:  time.Now().Unix(),
	}); err != nil {
		return nil, fmt.Errorf("memory: persist pending anchor rotation: %w", err)
	}
	txid, err := s.port.Broadcast(ctx, cRes.Tx, cRes.FeeSats, cRes.SpendSats)
	if err != nil {
		if errors.Is(err, nwallet.ErrSpendRejected) {
			_ = s.db.DeletePendingAnchor(shard, last.N)
		}
		return nil, err
	}
	beef := preBroadcastBeef
	var settled bool
	var height int64
	if cRes.Tx.MerklePath != nil {
		beef, settled, height = s.embedVerifiedAnchorBUMP(ctx, txid, beef, cRes.Tx, cRes.Tx.MerklePath, cRes.Tx.MerklePath.BlockHeight)
	} else if s.port.Network() == "memory" {
		if mp, h, ok := s.port.Proof(ctx, txid); ok && mp != nil {
			beef, settled, height = s.embedVerifiedAnchorBUMP(ctx, txid, beef, cRes.Tx, mp, h)
		}
	}
	if len(beef) == 0 {
		var bErr error
		beef, bErr = chain.SafeAtomicBEEF(cRes.Tx)
		if bErr != nil {
			return nil, fmt.Errorf("memory: encode rotated anchor AtomicBEEF: %w", bErr)
		}
	}
	// Preserve same-batch pre-rotation anchor tx(es) inside the rotated AtomicBEEF so offline
	// verifiers can execute the post-Chronicle Script VM across every Rotate() hop back to n-1.
	if len(last.Beef) > 0 {
		if merged, mErr := contracts.MergeAnchorLineageBEEF(beef, last.Beef, cRes.Tx.TxID()); mErr == nil && len(merged) > 0 {
			beef = merged
		}
	}
	if err := s.db.UpdateAnchorHead(shard, last.N, txid, beef, settled, height); err != nil {
		return nil, err
	}
	updated := last
	updated.TxID = txid
	updated.Beef = beef
	updated.Settled = settled
	updated.BlockHeight = height
	return &updated, nil
}
