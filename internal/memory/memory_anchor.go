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

// AnchorShard commits all unanchored fragments in shard into a real NektarAnchor BSV transaction,
// serialized per-shard with durable pre-broadcast pending_anchors persistence, guarded by SpendGuard.
func (s *Service) AnchorShard(ctx context.Context, shard string) (*store.BatchAnchor, error) {
	unlock := s.lockShard(shard)
	defer unlock()

	unanchored, err := s.db.ListUnanchored(shard)
	if err != nil {
		return nil, err
	}
	if len(unanchored) == 0 {
		return nil, nil
	}
	leaves := make([][]byte, len(unanchored))
	seqs := make([]int64, len(unanchored))
	for i, f := range unanchored {
		lb, err := hex.DecodeString(f.Leaf)
		if err != nil {
			return nil, fmt.Errorf("memory: decode leaf seq=%d: %w", f.Seq, err)
		}
		leaves[i] = lb
		seqs[i] = f.Seq
	}
	root, err := core.MerkleRoot(leaves)
	if err != nil {
		return nil, err
	}

	anchors, err := s.db.ListAnchors(shard)
	if err != nil {
		return nil, err
	}
	prevRp := core.Zero32
	var nextN int64
	if len(anchors) > 0 {
		last := anchors[len(anchors)-1]
		nextN = last.N + 1
		prevRp, err = hex.DecodeString(last.ChainedHex)
		if err != nil {
			return nil, err
		}
	}
	rp := core.ChainRp(prevRp, root, uint64(len(leaves)))
	pubBytes := s.port.Signer().IdentityKey().Compressed()
	shardID := core.ShardIDFromPubkey(pubBytes)

	// Guard against clobbering an existing in-flight pending anchor for (shard, nextN):
	// run per-shard reconciliation directly so confirmed-broadcast first-party anchors promote
	// immediately instead of stalling the shard behind a mined block wait!
	if existingPending, pErr := s.db.ListPendingAnchors(); pErr == nil {
		for _, p := range existingPending {
			if p.Shard == shard && p.N == nextN {
				if p.TxID == "" {
					_ = s.db.DeletePendingAnchor(p.Shard, p.N)
					break
				}
				recBatch, cleaned, wStatus, recErr := s.reconcilePendingAnchorLocked(ctx, p, shardID)
				if recErr != nil {
					return nil, recErr
				}
				if recBatch != nil {
					// Previous batch (shard, nextN) was recorded! Return it cleanly.
					return recBatch, nil
				}
				if cleaned {
					// Confirmed unbroadcast local nosend/aborted/failed row was cleaned up; proceed to build fresh batch.
					break
				}
				return nil, fmt.Errorf("memory: pending anchor (%s, n=%d, txid=%s, walletStatus=%q) still awaiting reconciliation", shard, nextN, p.TxID, wStatus)
			}
		}
	}

	// Persist pending_anchors intent BEFORE building/broadcasting so a crash after broadcast is recoverable!
	now := time.Now().Unix()
	if err := s.db.SavePendingAnchor(store.PendingAnchor{
		Shard:      shard,
		N:          nextN,
		Seqs:       seqs,
		RootHex:    core.ToHex(root),
		ChainedHex: core.ToHex(rp),
		CreatedAt:  now,
	}); err != nil {
		return nil, fmt.Errorf("memory: persist pending anchor: %w", err)
	}

	ownerPKH := core.Hash160(pubBytes)
	if fpkh, fErr := s.port.FundPKH(ctx); fErr == nil && len(fpkh) == 20 {
		ownerPKH = fpkh
	}

	var (
		tx    *transaction.Transaction
		beef  []byte
		fee   uint64
		spend uint64
	)

	// Consensus-linearized NektarAnchor singleton UTXO chain (Wave 1):
	// When a prior spendable NektarAnchor v4 UTXO exists for this shard, spend last.TxID:0 via
	// NektarAnchor.Append() (Input 0 = prior anchor, Input 1+ = wallet fuel, Output 0 = new anchor,
	// Output 1 = change), verified by the post-Chronicle Bitcoin Script VM before broadcast!
	spentSingleton := false
	if len(anchors) > 0 {
		last := anchors[len(anchors)-1]
		var prevTx *transaction.Transaction
		if len(last.Beef) > 0 {
			if _, parsed, _, pErr := guard.ParseBEEF(last.Beef, guard.DefaultBound); pErr == nil && parsed != nil {
				prevTx = parsed
			}
		}
		if prevTx == nil && last.TxID != "" {
			prevTx = s.port.Tx(last.TxID)
		}
		if prevTx != nil && len(prevTx.Outputs) > 0 && prevTx.Outputs[0] != nil && prevTx.Outputs[0].LockingScript != nil &&
			contracts.IsSpendableNektarAnchorScript(*prevTx.Outputs[0].LockingScript) {
			prevSt, dErr := contracts.DecodeNektarAnchor(*prevTx.Outputs[0].LockingScript)
			if dErr == nil && prevSt.Shard == shardID && prevSt.N == uint64(last.N) {
				nextSt, nErr := contracts.NextAnchorState(*prevSt, root, uint64(len(leaves)))
				if nErr != nil {
					_ = s.db.DeletePendingAnchor(shard, nextN)
					return nil, nErr
				}
				lockScript, eErr := contracts.EncodeAnchorLockingScript(nextSt)
				if eErr != nil {
					_ = s.db.DeletePendingAnchor(shard, nextN)
					return nil, eErr
				}
				cRes, cErr := s.port.BuildCovenantSpendTx(ctx, chain.CovenantSpendRequest{
					InputTx:               prevTx,
					InputBEEF:             last.Beef,
					InputVout:             0,
					Outputs:               []*transaction.TransactionOutput{chain.AnchorOutput(lockScript)},
					UnlockingScriptLength: 5200,
					FuelFromWallet:        true,
					Description:           fmt.Sprintf("nektar anchor append %s#%d", shard, nextN),
					Labels:                []string{"anchor", "append"},
					BuildUnlock: func(spendTx *transaction.Transaction, changePKH []byte, changeSats uint64) (*script.Script, error) {
						return contracts.BuildAnchorAppendUnlock(
							ctx, spendTx, 0, prevTx.Outputs[0], root, uint64(len(leaves)),
							changePKH, changeSats, s.port.SignCovenantSighash,
						)
					},
				})
				if cErr != nil {
					_ = s.db.DeletePendingAnchor(shard, nextN)
					return nil, fmt.Errorf("memory: NektarAnchor.Append (%s, n=%d): %w", shard, nextN, cErr)
				}
				tx, beef, fee, spend = cRes.Tx, cRes.BEEF, cRes.FeeSats, cRes.SpendSats
				spentSingleton = true
			}
		}
	}

	if !spentSingleton {
		state := contracts.AnchorState{
			Shard:    shardID,
			OwnerPub: s.port.IdentityPubHex(),
			OwnerPKH: ownerPKH,
			N:        uint64(nextN),
			Size:     uint64(len(leaves)),
			R:        root,
			Rp:       rp,
			Tag:      core.TagRPrime,
			Metanet:  true,
		}
		lockScript, err := contracts.EncodeAnchorLockingScript(state)
		if err != nil {
			_ = s.db.DeletePendingAnchor(shard, nextN)
			return nil, err
		}
		tx, beef, fee, spend, err = s.port.BuildFundedTxWithBEEF(ctx, []*transaction.TransactionOutput{chain.AnchorOutput(lockScript)})
		if err != nil {
			_ = s.db.DeletePendingAnchor(shard, nextN)
			return nil, err
		}
	}
	if uErr := s.db.UpdatePendingAnchorTx(shard, nextN, tx.TxID().String(), beef); uErr != nil {
		return nil, fmt.Errorf("memory: persist pending anchor txid: %w", uErr)
	}
	txid, err := s.port.Broadcast(ctx, tx, fee, spend)
	if err != nil {
		// Only delete pending_anchors on genuine local pre-broadcast refusal (SpendGuard cap).
		// On ANY post-submit broadcaster error or ErrBroadcastUnknown, keep pending_anchors intact for reconciliation!
		if errors.Is(err, nwallet.ErrSpendRejected) {
			_ = s.db.DeletePendingAnchor(shard, nextN)
		}
		return nil, err
	}
	if err := s.port.AdoptChange(tx); err != nil {
		return nil, err
	}

	// Only check instant post-broadcast proof in memory mode (or when Broadcast already attached a BUMP).
	// On live networks, skip the wasted 1ms-after-broadcast proof lookup; RefreshAnchorProofs upgrades once mined.
	var settled bool
	var height int64
	if tx.MerklePath != nil {
		beef, settled, height = s.embedVerifiedAnchorBUMP(ctx, txid, beef, tx, tx.MerklePath, tx.MerklePath.BlockHeight)
	} else if s.port.Network() == "memory" {
		if mp, h, ok := s.port.Proof(ctx, txid); ok && mp != nil {
			beef, settled, height = s.embedVerifiedAnchorBUMP(ctx, txid, beef, tx, mp, h)
		}
	}
	if len(beef) == 0 {
		var bErr error
		beef, bErr = chain.SafeAtomicBEEF(tx)
		if bErr != nil {
			return nil, fmt.Errorf("memory: encode AtomicBEEF: %w", bErr)
		}
	}
	batch := store.BatchAnchor{
		Shard:       shard,
		ShardID:     shardID,
		N:           nextN,
		Size:        len(leaves),
		RootHex:     core.ToHex(root),
		ChainedHex:  core.ToHex(rp),
		TxID:        txid,
		Beef:        beef,
		Settled:     settled,
		BlockHeight: height,
		CreatedAt:   now,
	}
	if err := s.db.RecordBatchAnchor(batch, seqs); err != nil {
		return nil, err
	}
	if shard == ReceiptsShard {
		for _, seq := range seqs {
			_ = s.db.UpdateReceiptAnchor(seq, txid)
		}
	}
	return &batch, nil
}

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
	txid, err := s.port.Broadcast(ctx, cRes.Tx, cRes.FeeSats, cRes.SpendSats)
	if err != nil {
		return nil, err
	}
	if err := s.port.AdoptChange(cRes.Tx); err != nil {
		return nil, err
	}
	beef := cRes.BEEF
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

func shardIdentifier(shard string, pubBytes []byte) string {
	h := core.DSHA256(core.Concat([]byte("nektar/shard/v1"), pubBytes, []byte(shard)))
	return core.ToHex(h)
}
