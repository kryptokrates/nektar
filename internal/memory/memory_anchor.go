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
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

// AnchorShard commits all unanchored fragments in shard into a real NektarAnchor BSV transaction,
// enforcing Merkle Automaton StateHash (Q_n) chaining and O(log N) MmrRoot accumulation on-chain,
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
	prevStateQ := core.Zero32Bytes()
	var nextN int64
	mmr, _ := core.NewMMR(nil)
	for _, a := range anchors {
		if rb, rErr := hex.DecodeString(a.RootHex); rErr == nil && len(rb) == 32 {
			_, _ = mmr.Append(rb)
		}
	}
	_, _ = mmr.Append(root)
	mmrRoot := mmr.Root()

	if len(anchors) > 0 {
		last := anchors[len(anchors)-1]
		nextN = last.N + 1
		prevRp, err = hex.DecodeString(last.ChainedHex)
		if err != nil {
			return nil, err
		}
		if last.StateHash != "" {
			if sq, sErr := hex.DecodeString(last.StateHash); sErr == nil && len(sq) == 32 {
				prevStateQ = sq
			}
		}
	}
	rp := core.ChainRp(prevRp, root, uint64(len(leaves)))

	// Compute canonical batchMetaHash binding latest treeHash, active Cedar policyCommit, and ontologyHead.
	var treeHashBytes, policyHashBytes, ontologyHashBytes []byte
	if latestCommits, cErr := s.db.ListMemCommits(shard, "", 1); cErr == nil && len(latestCommits) > 0 && latestCommits[0].TreeHash != "" {
		if tb, err := hex.DecodeString(latestCommits[0].TreeHash); err == nil && len(tb) == 32 {
			treeHashBytes = tb
		}
	}
	if pc := s.PolicyCommit(); pc != nil && pc.Hash != "" {
		if pb, err := hex.DecodeString(pc.Hash); err == nil && len(pb) == 32 {
			policyHashBytes = pb
		}
	}
	if oh := s.OntologyHead(); oh != "" {
		if ob, err := hex.DecodeString(oh); err == nil && len(ob) == 32 {
			ontologyHashBytes = ob
		}
	}
	batchMetaHash := core.ComputeBatchMetaHash(treeHashBytes, policyHashBytes, ontologyHashBytes)
	nextStateQ := core.ChainStateQ(prevStateQ, root, batchMetaHash, mmrRoot)

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
					return recBatch, nil
				}
				if cleaned {
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
		StateHash:  core.ToHex(nextStateQ),
		MmrRoot:    core.ToHex(mmrRoot),
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
		// Compute nextStateQ directly from the on-chain prevSt.StateHash so script consensus always matches.
		nextSt, nErr := contracts.NextAnchorState(*prevSt, root, uint64(len(leaves)), batchMetaHash, mmrRoot)
		if nErr != nil {
			_ = s.db.DeletePendingAnchor(shard, nextN)
			return nil, nErr
		}
		nextStateQ = nextSt.StateHash
		lockScript, eErr := contracts.EncodeAnchorLockingScript(nextSt)
		if eErr != nil {
			_ = s.db.DeletePendingAnchor(shard, nextN)
			return nil, eErr
		}
		anchorSpec := &nwallet.OutputTagSpec{
			Basket: nwallet.BasketAnchors,
			Tags:   []string{"anchor", "shard:" + shard, fmt.Sprintf("batch:%d", nextN)},
			CustomInstructions: fmt.Sprintf(`{"v":1,"kind":"anchor","shard":%q,"shardId":%q,"n":%d,"size":%d,"R":%q,"Rp":%q,"Q":%q,"mmr":%q}`,
				shard, shardID, nextN, len(leaves), hex.EncodeToString(root), hex.EncodeToString(rp), hex.EncodeToString(nextStateQ), hex.EncodeToString(mmrRoot)),
		}
		meta := nwallet.ActionAnchorAppend(shard, nextN, len(leaves))
		cRes, cErr := s.port.BuildCovenantSpendTx(ctx, chain.CovenantSpendRequest{
			InputTx:               prevTx,
			InputBEEF:             last.Beef,
			InputVout:             0,
			Outputs:               []*transaction.TransactionOutput{chain.AnchorOutput(lockScript)},
			UnlockingScriptLength: 5600,
			FuelFromWallet:        true,
			Description:           meta.Description,
			Labels:                meta.Labels,
			OutputSpec:            anchorSpec,
			BuildUnlock: func(spendTx *transaction.Transaction, changePKH []byte, changeSats uint64) (*script.Script, error) {
				return contracts.BuildAnchorAppendUnlock(
					ctx, spendTx, 0, prevTx.Outputs[0], root, uint64(len(leaves)),
					batchMetaHash, mmrRoot,
					changePKH, changeSats, s.port.SignCovenantSighash,
				)
			},
		})
		if cErr != nil {
			if !errors.Is(cErr, nwallet.ErrBroadcastUnknown) {
				_ = s.db.DeletePendingAnchor(shard, nextN)
			}
			return nil, fmt.Errorf("memory: NektarAnchor.Append (%s, n=%d): %w", shard, nextN, cErr)
		}
		tx, beef, fee, spend = cRes.Tx, cRes.BEEF, cRes.FeeSats, cRes.SpendSats
	} else {
		state := contracts.AnchorState{
			Shard:     shardID,
			OwnerPub:  genesisPubHex,
			OwnerPKH:  ownerPKH,
			N:         0,
			Size:      uint64(len(leaves)),
			R:         root,
			Rp:        rp,
			StateHash: nextStateQ,
			MmrRoot:   mmrRoot,
			Tag:       core.TagRPrime,
			Metanet:   true,
		}
		lockScript, err := contracts.EncodeAnchorLockingScript(state)
		if err != nil {
			_ = s.db.DeletePendingAnchor(shard, nextN)
			return nil, err
		}
		anchorSpec := &nwallet.OutputTagSpec{
			Basket: nwallet.BasketAnchors,
			Tags:   []string{"anchor", "shard:" + shard, "batch:0"},
			CustomInstructions: fmt.Sprintf(`{"v":1,"kind":"anchor","shard":%q,"shardId":%q,"n":0,"size":%d,"R":%q,"Rp":%q,"Q":%q,"mmr":%q}`,
				shard, shardID, len(leaves), hex.EncodeToString(root), hex.EncodeToString(rp), hex.EncodeToString(nextStateQ), hex.EncodeToString(mmrRoot)),
		}
		meta := nwallet.ActionAnchorShard(shard, 0, len(leaves))
		tx, beef, fee, spend, err = s.port.BuildFundedTxWithBasket(ctx, []*transaction.TransactionOutput{chain.AnchorOutput(lockScript)}, meta.Description, meta.Labels, anchorSpec)
		if err != nil {
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
		if errors.Is(err, nwallet.ErrSpendRejected) {
			_ = s.db.DeletePendingAnchor(shard, nextN)
		}
		return nil, err
	}

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
		StateHash:   core.ToHex(nextStateQ),
		MmrRoot:     core.ToHex(mmrRoot),
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

// ProveCompact constructs an O(log N) MMR CompactProofBundle proving (shard, seq) directly against
// the shard's latest head NektarAnchor UTXO without shipping an O(N) ChainTail.
func (s *Service) ProveCompact(ctx context.Context, shard string, seq int64) (*contracts.CompactProofBundle, *contracts.VerifyResult, error) {
	return s.ProveCompactAuthorized(ctx, shard, seq, ownerPrincipal())
}

// ProveCompactAuthorized enforces Cedar read authorization before constructing an O(log N) CompactProofBundle.
func (s *Service) ProveCompactAuthorized(ctx context.Context, shard string, seq int64, principal policy.Principal) (*contracts.CompactProofBundle, *contracts.VerifyResult, error) {
	if err := ValidateShardName(shard, isOperatorPrincipal(principal)); err != nil {
		return nil, nil, err
	}
	rec, err := s.db.GetFragment(shard, seq)
	if err != nil {
		return nil, nil, err
	}
	if principal.ID == "" {
		principal = ownerPrincipal()
	}
	if dec, aErr := s.AuthorizeAction(principal, "read", policy.Resource{
		Type:  "Fragment",
		ID:    fmt.Sprintf("%s#%d", rec.Shard, rec.Seq),
		Level: int64(rec.Level),
		Shard: rec.Shard,
		Class: rec.Class,
	}); aErr != nil || !dec.Allow {
		if aErr != nil {
			return nil, nil, fmt.Errorf("memory: cedar compact-prove policy error: %w", aErr)
		}
		return nil, nil, fmt.Errorf("memory: cedar policy denied compact-prove on %s#%d", rec.Shard, rec.Seq)
	}
	if rec.BatchN < 0 {
		return nil, &contracts.VerifyResult{OK: false, AnchorState: "unanchored", Reason: "fragment not yet anchored"}, nil
	}
	leafHashes, err := s.db.ListBatchLeafHashes(shard, rec.BatchN)
	if err != nil {
		return nil, nil, err
	}
	leaves := make([][]byte, len(leafHashes))
	for i, lh := range leafHashes {
		lb, dErr := hex.DecodeString(lh)
		if dErr != nil {
			return nil, nil, dErr
		}
		leaves[i] = lb
	}
	mp, err := core.ComputeMerkleProof(leaves, rec.LeafIndex)
	if err != nil {
		return nil, nil, err
	}
	pathHex := make([]string, len(mp.Siblings))
	for i, p := range mp.Siblings {
		pathHex[i] = core.ToHex(p)
	}
	anchors, err := s.db.ListAnchors(shard)
	if err != nil {
		return nil, nil, err
	}
	if len(anchors) == 0 {
		return nil, nil, fmt.Errorf("memory: no anchors for shard %q", shard)
	}
	mmr, _ := core.NewMMR(nil)
	var target *store.BatchAnchor
	for i := range anchors {
		rb, rErr := hex.DecodeString(anchors[i].RootHex)
		if rErr != nil || len(rb) != 32 {
			return nil, nil, fmt.Errorf("memory: invalid anchor root hex at batch %d", anchors[i].N)
		}
		if _, aErr := mmr.Append(rb); aErr != nil {
			return nil, nil, aErr
		}
		if anchors[i].N == rec.BatchN {
			cp := anchors[i]
			target = &cp
		}
	}
	if target == nil {
		return nil, nil, fmt.Errorf("memory: missing anchor batch %d for shard %q", rec.BatchN, shard)
	}
	mmrProof, err := mmr.ProveInclusion(uint64(rec.BatchN))
	if err != nil {
		return nil, nil, err
	}
	head := anchors[len(anchors)-1]
	bundle := &contracts.CompactProofBundle{
		Version:   1,
		Shard:     head.ShardID,
		BatchN:    uint64(target.N),
		BatchSize: uint64(target.Size),
		BatchRoot: target.RootHex,
		Leaf:      rec.Leaf,
		LeafIndex: rec.LeafIndex,
		BatchPath: pathHex,
		MMRProof:  mmrProof,
		HeadAnchor: contracts.AnchorLink{
			TxID:        head.TxID,
			OutputIndex: 0,
			N:           uint64(head.N),
			Size:        uint64(head.Size),
			R:           head.RootHex,
			Rp:          head.ChainedHex,
			StateHash:   head.StateHash,
			MmrRoot:     head.MmrRoot,
		},
		HeadAnchorBEEF: head.Beef,
	}
	minDepth := 1
	if s.port != nil && s.port.SettleDepth() > 0 {
		minDepth = s.port.SettleDepth()
	}
	vr := contracts.VerifyFragmentCompact(ctx, *bundle, s.port.Tracker(), minDepth)
	return bundle, &vr, nil
}

// ConsistencyProof constructs an O(log N) MMR consistency proof proving that shard's history at
// anchor batch fromN (inclusive, 1-based prefix count = fromN+1) is an unmodified prefix of toN.
func (s *Service) ConsistencyProof(shard string, fromN, toN int64) (*core.MMRConsistencyProof, error) {
	if err := ValidateShardName(shard, true); err != nil {
		return nil, err
	}
	anchors, err := s.db.ListAnchorSummaries(shard)
	if err != nil {
		return nil, err
	}
	if len(anchors) == 0 {
		return nil, fmt.Errorf("memory: shard %q has no anchors", shard)
	}
	if toN < 0 {
		toN = anchors[len(anchors)-1].N
	}
	if fromN < 0 || fromN > toN || int(toN) >= len(anchors) {
		return nil, fmt.Errorf("memory: invalid consistency range [%d..%d] (anchor count %d)", fromN, toN, len(anchors))
	}
	mmr, _ := core.NewMMR(nil)
	for i := int64(0); i <= toN; i++ {
		rb, rErr := hex.DecodeString(anchors[i].RootHex)
		if rErr != nil || len(rb) != 32 {
			return nil, fmt.Errorf("memory: invalid anchor root at batch %d", i)
		}
		if _, aErr := mmr.Append(rb); aErr != nil {
			return nil, aErr
		}
	}
	return mmr.ProveConsistency(uint64(fromN+1), uint64(toN+1))
}

// resolveRepoGenesis returns the immutable 33-byte GenesisOwner pubkey hex and its 20-byte
// ShardID (hash160) for shard. Existing anchored shards preserve their established GenesisOwner;
// fresh Memory Repos derive a unique repo-scoped BRC-43 public key via Port.DeriveRepoGenesisPubKey
// so separate repositories never collide on-chain under one shared ShardID. Fails closed on derivation errors.
func (s *Service) resolveRepoGenesis(ctx context.Context, shard string, existingAnchors []store.BatchAnchor) (string, string, error) {
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
	if br, err := s.db.GetMemBranch(shard, "main"); err == nil && br.GenesisPub != "" && br.GenesisShardID != "" {
		return br.GenesisPub, br.GenesisShardID, nil
	}
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
