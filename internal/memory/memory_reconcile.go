package memory

import (
	"context"
	"encoding/binary"
	"errors"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/store"
)

// embedVerifiedAnchorBUMP verifies mp against witnessed headers, attaches tx.MerklePath = mp,
// and re-encodes strict AtomicBEEF(false). Returns settled=true ONLY if the returned BEEF
// actually embeds the verified BUMP; never persists Settled=true with a BUMP-less BEEF.
func (s *Service) embedVerifiedAnchorBUMP(
	ctx context.Context,
	txid string,
	existingBeef []byte,
	tx *transaction.Transaction,
	mp *transaction.MerklePath,
	height uint32,
) ([]byte, bool, int64) {
	if mp == nil {
		return existingBeef, false, 0
	}
	if height == 0 {
		height = mp.BlockHeight
	}
	if tx == nil && len(existingBeef) > 0 {
		if _, parsed, _, err := guard.ParseBEEF(existingBeef, guard.DefaultBound); err == nil && parsed != nil {
			tx = parsed
		}
	}
	if tx == nil {
		return existingBeef, false, 0
	}
	if tx.TxID().String() != txid {
		return existingBeef, false, 0
	}
	root, rErr := mp.ComputeRoot(tx.TxID())
	if rErr != nil || root == nil {
		return existingBeef, false, 0
	}
	tracker := s.port.Tracker()
	if tracker == nil {
		return existingBeef, false, 0
	}
	valid, vErr := tracker.IsValidRootForHeight(ctx, root, height)
	if vErr != nil || !valid {
		return existingBeef, false, 0
	}
	txCopy := *tx
	txCopy.MerklePath = mp
	newBeef, bErr := txCopy.AtomicBEEF(false)
	if bErr != nil || len(newBeef) == 0 {
		return existingBeef, false, 0
	}
	// Preserve any same-batch Rotate() lineage transactions that were carried in existingBeef.
	if len(existingBeef) > 0 {
		if existingB, _, _, pErr := guard.ParseBEEF(existingBeef, guard.DefaultBound); pErr == nil && existingB != nil && len(existingB.Transactions) > 1 {
			if merged, mErr := contracts.MergeAnchorLineageBEEF(newBeef, existingBeef, tx.TxID()); mErr == nil && len(merged) > 0 {
				newBeef = merged
			}
		}
	}
	return newBeef, true, int64(height)
}

func (s *Service) reconcilePendingAnchorLocked(ctx context.Context, p store.PendingAnchor, shardID string) (recorded *store.BatchAnchor, cleaned bool, wStatus string, err error) {
	if p.TxID == "" {
		_ = s.db.DeletePendingAnchor(p.Shard, p.N)
		return nil, true, "", nil
	}
	mined, walletStatus, mp, height, statusErr := s.port.TxBroadcastStatus(ctx, p.TxID)
	wStatus = walletStatus
	// Local embedded wallet confirms tx is strictly pre-broadcast 'nosend' or terminal 'failed'/'aborted':
	// clean up cleanly without ever re-broadcasting via RelayExternalTx!
	if !mined && (walletStatus == "nosend" || walletStatus == "failed" || walletStatus == "aborted") {
		if walletStatus == "nosend" && s.port.Engine() != nil {
			_ = s.port.Engine().AbortNoSendByTxID(ctx, p.TxID)
		}
		_ = s.db.DeletePendingAnchor(p.Shard, p.N)
		return nil, true, walletStatus, nil
	}
	// Fail closed on tracker or proof-lookup outages: never delete or promote pending_anchors on outage!
	if statusErr != nil && !mined {
		return nil, false, walletStatus, statusErr
	}
	beef := p.Beef
	var tx *transaction.Transaction
	if len(beef) == 0 {
		tx = s.port.Tx(p.TxID)
		if tx != nil {
			if b, bErr := chain.SafeAtomicBEEF(tx); bErr == nil && len(b) > 0 {
				beef = b
			}
		}
	}

	// Promote pending_anchors ONLY via cryptographic evidence:
	// (1) header-verified mined BUMP (mined && mp != nil), OR
	// (2) carried first-party AtomicBEEF whose input ancestry passes VerifyTransactionSPV (on live networks)
	//     and is already confirmed in local wallet state ('completed'/'unproven'/'unmined') or recorded in client-mode journal.
	// Never call RelayExternalTx on first-party anchors or promote 'submission_unknown'/'failed'/'aborted'/'nosend'.
	confirmedBroadcast := mined && mp != nil
	if !confirmedBroadcast && len(beef) > 0 {
		parsedBeef, parsedTx, _, pErr := guard.ParseBEEF(beef, guard.DefaultBound)
		if pErr == nil && parsedTx != nil {
			ancestryOK := s.port.Network() == "memory"
			if !ancestryOK {
				if sErr := chain.VerifyTransactionSPV(ctx, parsedTx, parsedBeef, s.port.Tracker()); sErr == nil {
					ancestryOK = true
				} else if errors.Is(sErr, chain.ErrTrackerUnavailable) {
					return nil, false, walletStatus, sErr
				}
			}
			if ancestryOK {
				if walletStatus == "completed" || walletStatus == "unproven" || walletStatus == "unmined" {
					confirmedBroadcast = true
				} else if s.port.Engine() == nil && walletStatus == "" && s.port.Tx(p.TxID) != nil {
					confirmedBroadcast = true
				}
			}
		}
	}

	if len(beef) > 0 && confirmedBroadcast {
		var settled bool
		var blockHeight int64
		if mined && mp != nil {
			beef, settled, blockHeight = s.embedVerifiedAnchorBUMP(ctx, p.TxID, beef, tx, mp, height)
		}
		if p.Kind == "rotate" {
			if uErr := s.db.UpdateAnchorHead(p.Shard, p.N, p.TxID, beef, settled, blockHeight); uErr != nil {
				return nil, false, walletStatus, uErr
			}
			updated := store.BatchAnchor{
				Shard:       p.Shard,
				ShardID:     shardID,
				N:           p.N,
				RootHex:     p.RootHex,
				ChainedHex:  p.ChainedHex,
				StateHash:   p.StateHash,
				MmrRoot:     p.MmrRoot,
				TxID:        p.TxID,
				Beef:        beef,
				Settled:     settled,
				BlockHeight: blockHeight,
				CreatedAt:   p.CreatedAt,
			}
			return &updated, false, walletStatus, nil
		}
		batch := store.BatchAnchor{
			Shard:       p.Shard,
			ShardID:     shardID,
			N:           p.N,
			Size:        len(p.Seqs),
			RootHex:     p.RootHex,
			ChainedHex:  p.ChainedHex,
			StateHash:   p.StateHash,
			MmrRoot:     p.MmrRoot,
			TxID:        p.TxID,
			Beef:        beef,
			Settled:     settled,
			BlockHeight: blockHeight,
			CreatedAt:   p.CreatedAt,
		}
		if rErr := s.db.RecordBatchAnchor(batch, p.Seqs); rErr != nil {
			return nil, false, walletStatus, rErr
		}
		return &batch, false, walletStatus, nil
	}
	if len(beef) == 0 && !mined && walletStatus == "" {
		_ = s.db.DeletePendingAnchor(p.Shard, p.N)
		return nil, true, walletStatus, nil
	}
	return nil, false, walletStatus, nil
}

// ReconcilePendingAnchors recovers any in-flight pending_anchors persisted before a crash.
// Never promotes an unbroadcast local 'nosend'/'failed'/'aborted' transaction as anchored,
// and never persists Settled=true without embedding a header-verified BUMP into AnchorBEEF.
func (s *Service) ReconcilePendingAnchors(ctx context.Context) (int, error) {
	pending, err := s.db.ListPendingAnchors()
	if err != nil || len(pending) == 0 {
		return 0, err
	}
	recovered := 0
	for _, p := range pending {
		unlock := s.lockShard(p.Shard)
		existingAnchors, _ := s.db.ListAnchors(p.Shard)
		_, shardID, _ := s.resolveRepoGenesis(ctx, p.Shard, existingAnchors)
		if shardID == "" {
			pubBytes := s.port.Signer().IdentityKey().Compressed()
			shardID = core.ShardIDFromPubkey(pubBytes)
		}
		recBatch, _, _, rErr := s.reconcilePendingAnchorLocked(ctx, p, shardID)
		unlock()
		if rErr == nil && recBatch != nil {
			recovered++
		}
	}
	return recovered, nil
}

// HandleReorg clears settled anchor proofs at or above forkHeight and strips orphaned
// MerklePaths from anchors.beef so RefreshAnchorProofs re-acquires valid post-reorg BUMPs
// and offline verifiers see Unsettled instead of BEEFInvalid.
func (s *Service) HandleReorg(forkHeight uint32) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	return s.db.ClearAnchorProofsFromWithStrip(int64(forkHeight), func(rawBeef []byte) []byte {
		return stripAnchorMerklePathFromHeight(rawBeef, forkHeight)
	})
}

func stripAnchorMerklePathFromHeight(rawBeef []byte, forkHeight uint32) []byte {
	if len(rawBeef) == 0 {
		return rawBeef
	}
	parsedBeef, tx, txHash, err := guard.ParseBEEF(rawBeef, guard.DefaultBound)
	if err != nil || parsedBeef == nil || tx == nil || txHash == nil {
		return rawBeef
	}
	changed := false
	var stripTx func(t *transaction.Transaction, seen map[*transaction.Transaction]bool)
	stripTx = func(t *transaction.Transaction, seen map[*transaction.Transaction]bool) {
		if t == nil || seen[t] {
			return
		}
		seen[t] = true
		if t.MerklePath != nil && t.MerklePath.BlockHeight >= forkHeight {
			t.MerklePath = nil
			changed = true
		}
		for _, inp := range t.Inputs {
			if inp != nil && inp.SourceTransaction != nil {
				stripTx(inp.SourceTransaction, seen)
			}
		}
	}
	seen := make(map[*transaction.Transaction]bool)
	stripTx(tx, seen)
	for _, btx := range parsedBeef.Transactions {
		if btx != nil && btx.Transaction != nil {
			stripTx(btx.Transaction, seen)
		}
	}
	if !changed {
		return rawBeef
	}
	cleanBeef := transaction.NewBeefV2()
	for _, btx := range parsedBeef.Transactions {
		if btx == nil || btx.Transaction == nil {
			continue
		}
		if _, mErr := cleanBeef.MergeTransaction(btx.Transaction); mErr != nil {
			return rawBeef
		}
	}
	if _, mErr := cleanBeef.MergeTransaction(tx); mErr != nil {
		return rawBeef
	}
	body, err := cleanBeef.Bytes()
	if err != nil || len(body) == 0 {
		return rawBeef
	}
	out := make([]byte, 4+chainhash.HashSize+len(body))
	binary.LittleEndian.PutUint32(out[0:4], transaction.ATOMIC_BEEF)
	copy(out[4:4+chainhash.HashSize], txHash[:])
	copy(out[4+chainhash.HashSize:], body)
	return out
}

// RefreshAnchorProofs polls the chain for mined MerklePaths on any unsettled broadcast anchors,
// verifies the computed Merkle root against witnessed PoW headers before upgrading settled=true,
// and embeds the mined BUMP into the stored AnchorBEEF. Fails closed when tracker == nil.
func (s *Service) RefreshAnchorProofs(ctx context.Context) (int, error) {
	if s.port.Tracker() == nil {
		return 0, chain.ErrTrackerUnavailable
	}
	unsettled, err := s.db.ListUnsettledAnchors()
	if err != nil || len(unsettled) == 0 {
		return 0, err
	}
	upgraded := 0
	for _, a := range unsettled {
		mp, height, ok, pErr := s.port.ProofChecked(ctx, a.TxID)
		if pErr != nil || !ok || mp == nil {
			continue
		}
		newBeef, settled, blockHeight := s.embedVerifiedAnchorBUMP(ctx, a.TxID, a.Beef, nil, mp, height)
		if !settled {
			continue
		}
		if err := s.db.UpdateAnchorProof(a.Shard, a.N, newBeef, blockHeight); err == nil {
			upgraded++
		}
	}
	return upgraded, nil
}

// RefreshAnchorProofByTxID upgrades a matching unsettled anchor immediately when Monitor.OnTxMined
// fires with a verified MerklePath, avoiding a 45s polling delay.
func (s *Service) RefreshAnchorProofByTxID(ctx context.Context, txid string, height uint32, mp *transaction.MerklePath) (bool, error) {
	if s == nil || s.db == nil || txid == "" || mp == nil {
		return false, nil
	}
	a, found, err := s.db.GetAnchorByTxID(txid)
	if err != nil || !found || a.Settled {
		return false, err
	}
	newBeef, settled, blockHeight := s.embedVerifiedAnchorBUMP(ctx, txid, a.Beef, nil, mp, height)
	if !settled {
		return false, nil
	}
	if err := s.db.UpdateAnchorProof(a.Shard, a.N, newBeef, blockHeight); err != nil {
		return false, err
	}
	return true, nil
}
