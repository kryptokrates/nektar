package memory

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
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
	prevRp := core.Zero32Bytes()
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
	genesisPubHex, shardID, err := s.resolveRepoGenesis(ctx, shard, anchors)
	if err != nil {
		return nil, err
	}

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

	pubBytes := s.port.Signer().IdentityKey().Compressed()
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

	// STRICT UTXO LINEAGE ONLY:
	// - Genesis (n == 0): mint a spendable META NektarAnchor covenant UTXO under the repo's unique BRC-43 GenesisOwner.
	// - Successor (n >= 1): MUST spend last.TxID:0 via NektarAnchor.Append() (Input 0 = prior anchor UTXO,
	//   Input 1+ = wallet fuel, Output 0 = successor anchor UTXO, Output 1 = change), verified by the
	//   post-Chronicle Bitcoin Script VM before broadcast. No disconnected fallback UTXO is permitted.
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
		if prevTx == nil || len(prevTx.Outputs) == 0 || prevTx.Outputs[0] == nil || prevTx.Outputs[0].LockingScript == nil {
			_ = s.db.DeletePendingAnchor(shard, nextN)
			return nil, fmt.Errorf("memory: cannot append anchor (%s, n=%d): prior NektarAnchor UTXO %s:0 unavailable", shard, nextN, last.TxID)
		}
		if !contracts.IsSpendableNektarAnchorScript(*prevTx.Outputs[0].LockingScript) {
			_ = s.db.DeletePendingAnchor(shard, nextN)
			return nil, fmt.Errorf("memory: cannot append anchor (%s, n=%d): prior output %s:0 is not a spendable NektarAnchor UTXO", shard, nextN, last.TxID)
		}
		prevSt, dErr := contracts.DecodeNektarAnchor(*prevTx.Outputs[0].LockingScript)
		if dErr != nil {
			_ = s.db.DeletePendingAnchor(shard, nextN)
			return nil, fmt.Errorf("memory: decode prior anchor (%s, n=%d): %w", shard, nextN, dErr)
		}
		if prevSt.Shard != shardID || prevSt.N != uint64(last.N) {
			_ = s.db.DeletePendingAnchor(shard, nextN)
			return nil, fmt.Errorf("memory: prior anchor state mismatch (%s, got shard=%s n=%d want shard=%s n=%d)", shard, prevSt.Shard, prevSt.N, shardID, last.N)
		}
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
		anchorSpec := &nwallet.OutputTagSpec{
			Basket: nwallet.BasketAnchors,
			Tags:   []string{"anchor", "shard:" + shard, fmt.Sprintf("batch:%d", nextN)},
			CustomInstructions: fmt.Sprintf(`{"v":1,"kind":"anchor","shard":%q,"shardId":%q,"n":%d,"size":%d,"R":%q,"Rp":%q}`,
				shard, shardID, nextN, len(leaves), hex.EncodeToString(root), hex.EncodeToString(rp)),
		}
		meta := nwallet.ActionAnchorAppend(shard, nextN, len(leaves))
		cRes, cErr := s.port.BuildCovenantSpendTx(ctx, chain.CovenantSpendRequest{
			InputTx:               prevTx,
			InputBEEF:             last.Beef,
			InputVout:             0,
			Outputs:               []*transaction.TransactionOutput{chain.AnchorOutput(lockScript)},
			UnlockingScriptLength: 5200,
			FuelFromWallet:        true,
			Description:           meta.Description,
			Labels:                meta.Labels,
			OutputSpec:            anchorSpec,
			BuildUnlock: func(spendTx *transaction.Transaction, changePKH []byte, changeSats uint64) (*script.Script, error) {
				return contracts.BuildAnchorAppendUnlock(
					ctx, spendTx, 0, prevTx.Outputs[0], root, uint64(len(leaves)),
					changePKH, changeSats, s.port.SignCovenantSighash,
				)
			},
		})
		if cErr != nil {
			// Never delete pending_anchors on ErrBroadcastUnknown (client-mode SignAction may have broadcast).
			if !errors.Is(cErr, nwallet.ErrBroadcastUnknown) {
				_ = s.db.DeletePendingAnchor(shard, nextN)
			}
			return nil, fmt.Errorf("memory: NektarAnchor.Append (%s, n=%d): %w", shard, nextN, cErr)
		}
		tx, beef, fee, spend = cRes.Tx, cRes.BEEF, cRes.FeeSats, cRes.SpendSats
	} else {
		state := contracts.AnchorState{
			Shard:    shardID,
			OwnerPub: genesisPubHex,
			OwnerPKH: ownerPKH,
			N:        0,
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
		anchorSpec := &nwallet.OutputTagSpec{
			Basket: nwallet.BasketAnchors,
			Tags:   []string{"anchor", "shard:" + shard, "batch:0"},
			CustomInstructions: fmt.Sprintf(`{"v":1,"kind":"anchor","shard":%q,"shardId":%q,"n":0,"size":%d,"R":%q,"Rp":%q}`,
				shard, shardID, len(leaves), hex.EncodeToString(root), hex.EncodeToString(rp)),
		}
		meta := nwallet.ActionAnchorShard(shard, 0, len(leaves))
		tx, beef, fee, spend, err = s.port.BuildFundedTxWithBasket(ctx, []*transaction.TransactionOutput{chain.AnchorOutput(lockScript)}, meta.Description, meta.Labels, anchorSpec)
		if err != nil {
			// Never delete pending_anchors on ErrBroadcastUnknown (client-mode CreateAction may have broadcast).
			if !errors.Is(err, nwallet.ErrBroadcastUnknown) {
				_ = s.db.DeletePendingAnchor(shard, nextN)
			}
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
	return &batch, nil
}

// resolveRepoGenesis returns the immutable 33-byte GenesisOwner pubkey hex and its 20-byte
// ShardID (hash160) for shard. Existing anchored shards preserve their established GenesisOwner;
// fresh Memory Repos derive a unique repo-scoped BRC-43 public key via Port.DeriveRepoGenesisPubKey
// so separate repositories never collide on-chain under one shared ShardID. Fails closed on derivation errors.
func (s *Service) resolveRepoGenesis(ctx context.Context, shard string, existingAnchors []store.BatchAnchor) (string, string, error) {
	// 1. Preserve established on-chain GenesisOwner if this shard already has anchors.
	for _, a := range existingAnchors {
		if len(a.Beef) > 0 {
			if _, parsed, _, err := guard.ParseBEEF(a.Beef, guard.DefaultBound); err == nil && parsed != nil &&
				len(parsed.Outputs) > 0 && parsed.Outputs[0] != nil && parsed.Outputs[0].LockingScript != nil {
				if st, dErr := contracts.DecodeNektarAnchor(*parsed.Outputs[0].LockingScript); dErr == nil && st.OwnerPub != "" {
					return st.OwnerPub, st.Shard, nil
				}
			}
		}
		if a.ShardID != "" {
			return s.port.IdentityPubHex(), a.ShardID, nil
		}
	}
	// 2. Reuse persisted branch genesis identity if already derived for this repo.
	if br, err := s.db.GetMemBranch(shard, "main"); err == nil && br.GenesisPub != "" && br.GenesisShardID != "" {
		return br.GenesisPub, br.GenesisShardID, nil
	}
	// 3. Derive a unique per-repo BRC-43 genesis public key (fail closed on signer/derivation error!).
	pub, err := s.port.DeriveRepoGenesisPubKey(ctx, shard, "main")
	if err != nil {
		return "", "", fmt.Errorf("memory: derive repo genesis pubkey for %q: %w", shard, err)
	}
	if pub == nil {
		return "", "", fmt.Errorf("memory: empty repo genesis pubkey for %q", shard)
	}
	pubBytes := pub.Compressed()
	pubHex := hex.EncodeToString(pubBytes)
	sid := core.ShardIDFromPubkey(pubBytes)
	_ = s.db.UpsertMemBranch(store.MemBranchRow{
		Repo:           shard,
		Branch:         "main",
		GenesisPub:     pubHex,
		GenesisShardID: sid,
		UpdatedAt:      time.Now().Unix(),
	})
	return pubHex, sid, nil
}
