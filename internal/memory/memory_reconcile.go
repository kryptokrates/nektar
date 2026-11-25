package memory

import (
	"context"
	"strings"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/chain"
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
	return newBeef, true, int64(height)
}

func (s *Service) reconcilePendingAnchorLocked(ctx context.Context, p store.PendingAnchor, shardID string) (recorded *store.BatchAnchor, cleaned bool, wStatus string, err error) {
	if p.TxID == "" {
		_ = s.db.DeletePendingAnchor(p.Shard, p.N)
		return nil, true, "", nil
	}
	mined, walletStatus, mp, height := s.port.TxBroadcastStatus(ctx, p.TxID)
	wStatus = walletStatus
	tx := s.port.Tx(p.TxID)
	beef := p.Beef
	if tx != nil && len(beef) == 0 {
		if b, bErr := chain.SafeAtomicBEEF(tx); bErr == nil && len(b) > 0 {
			beef = b
		}
	}

	// Local embedded wallet confirms tx was never broadcast or definitely aborted/failed: clean up cleanly!
	if !mined && (walletStatus == "nosend" || walletStatus == "failed" || walletStatus == "aborted" || walletStatus == "unsigned" || walletStatus == "unprocessed") {
		if walletStatus == "nosend" && s.port.Engine() != nil {
			_ = s.port.Engine().AbortNoSendByTxID(ctx, p.TxID)
		}
		_ = s.db.DeletePendingAnchor(p.Shard, p.N)
		return nil, true, walletStatus, nil
	}

	// First-party confirmed broadcast states (never 'sending' or 'submission_unknown') promote immediately
	// to anchors(settled=0 or 1) so the shard does not stall waiting for a block!
	confirmedBroadcast := mined || walletStatus == "completed" || walletStatus == "unproven" || walletStatus == "unmined"
	if !confirmedBroadcast && len(beef) > 0 && s.port.Network() != "memory" {
		if _, parsedTx, _, pErr := guard.ParseBEEF(beef, guard.DefaultBound); pErr == nil && parsedTx != nil {
			if rErr := s.port.RelayExternalTx(ctx, parsedTx); rErr == nil {
				confirmedBroadcast = true
			} else {
				lower := strings.ToLower(rErr.Error())
				if strings.Contains(lower, "already in mempool") || strings.Contains(lower, "already known") || strings.Contains(lower, "txn-already-known") {
					confirmedBroadcast = true
				}
				// Never delete pending_anchors from broadcaster error strings ("utxo_spent", "missing inputs", etc.):
				// keep pending until Monitor.UnFail proves or settles the underlying tx.
			}
		}
	}

	if len(beef) > 0 && confirmedBroadcast {
		var settled bool
		var blockHeight int64
		if mined && mp != nil {
			beef, settled, blockHeight = s.embedVerifiedAnchorBUMP(ctx, p.TxID, beef, tx, mp, height)
		}
		batch := store.BatchAnchor{
			Shard:       p.Shard,
			ShardID:     shardID,
			N:           p.N,
			Size:        len(p.Seqs),
			RootHex:     p.RootHex,
			ChainedHex:  p.ChainedHex,
			TxID:        p.TxID,
			Beef:        beef,
			Settled:     settled,
			BlockHeight: blockHeight,
			CreatedAt:   p.CreatedAt,
		}
		if rErr := s.db.RecordBatchAnchor(batch, p.Seqs); rErr != nil {
			return nil, false, walletStatus, rErr
		}
		if p.Shard == ReceiptsShard {
			for _, seq := range p.Seqs {
				_ = s.db.UpdateReceiptAnchor(seq, p.TxID)
			}
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
	pubBytes := s.port.Signer().IdentityKey().Compressed()
	shardID := core.ShardIDFromPubkey(pubBytes)
	for _, p := range pending {
		unlock := s.lockShard(p.Shard)
		recBatch, _, _, rErr := s.reconcilePendingAnchorLocked(ctx, p, shardID)
		unlock()
		if rErr == nil && recBatch != nil {
			recovered++
		}
	}
	return recovered, nil
}

// HandleReorg clears settled anchor proofs at or above forkHeight so RefreshAnchorProofs
// re-acquires post-reorg BUMPs instead of serving orphaned proofs.
func (s *Service) HandleReorg(forkHeight uint32) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	return s.db.ClearAnchorProofsFrom(int64(forkHeight))
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
