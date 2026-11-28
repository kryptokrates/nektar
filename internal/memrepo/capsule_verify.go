package memrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/jevgraph"
	"github.com/kryptokrates/nektar/internal/store"
)

func verifyCapsuleObjects(cap *MemoryCapsule, opts CapsuleVerifyOpts, rep *CapsuleVerifyReport) bool {
	objects := opts.Objects
	// 1. Verify every BlobObject hash & payload commitment (fail-closed on missing CHIRP unless AllowMissingExternal).
	for k, b := range cap.Blobs {
		wantH, err := ComputeBlobHash(b)
		if err != nil || !strings.EqualFold(wantH, b.BlobHash) || !strings.EqualFold(k, b.BlobHash) {
			rep.OK = false
			rep.Reason = fmt.Sprintf("blob hash mismatch for %q", b.Path)
			return false
		}
		if b.Redacted {
			// Hash-preserving erased/Cedar-redacted stub: verified by ComputeBlobHash above, tracked separately.
			rep.BlobsRedacted++
			continue
		}
		if !b.Encrypted {
			if len(b.Inline) > 0 {
				sum := sha256.Sum256(b.Inline)
				if !strings.EqualFold(hex.EncodeToString(sum[:]), b.ContentSHA256) {
					rep.OK = false
					rep.Reason = fmt.Sprintf("inline blob content sha256 mismatch for %q", b.Path)
					return false
				}
			} else if b.ChirpURL != "" {
				if objects == nil {
					if !opts.AllowMissingExternal {
						rep.OK = false
						rep.Reason = fmt.Sprintf("external CHIRP blob %q (%s) missing (no ObjectStore)", b.Path, b.ChirpURL)
						return false
					}
					rep.BlobsMissingExternal++
					continue
				}
				resolved, err := bstack.ResolveChirp(objects, b.ChirpURL)
				if err != nil {
					if opts.AllowMissingExternal && (errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "not available") || strings.Contains(err.Error(), "does not exist")) {
						rep.BlobsMissingExternal++
						continue
					}
					rep.OK = false
					rep.Reason = fmt.Sprintf("CHIRP blob %q missing or corrupt: %v", b.Path, err)
					return false
				}
				sum := sha256.Sum256(resolved)
				if !strings.EqualFold(hex.EncodeToString(sum[:]), b.ContentSHA256) {
					rep.OK = false
					rep.Reason = fmt.Sprintf("CHIRP blob content sha256 mismatch for %q", b.Path)
					return false
				}
			}
		}
		rep.BlobsVerified++
	}

	// 2. Verify every TreeObject hash (both v1 leaf/flat trees and v2 sharded root trees),
	//    blob references, and any JevGraph edge sidecars offline.
	verifiedSidecars := make(map[string]bool)
	for k, tr := range cap.Trees {
		if !strings.EqualFold(k, tr.TreeHash) {
			rep.OK = false
			rep.Reason = fmt.Sprintf("tree hash key mismatch for %s", k)
			return false
		}
		if err := VerifyTreeObject(tr); err != nil {
			rep.OK = false
			rep.Reason = fmt.Sprintf("tree hash mismatch for %s: %v", k, err)
			return false
		}
		for bk, subHash := range tr.Subtrees {
			sub, ok := cap.Trees[subHash]
			if !ok {
				rep.OK = false
				rep.Reason = fmt.Sprintf("sharded tree %s missing bucket %s (%s)", k, bk, subHash)
				return false
			}
			if len(sub.Subtrees) > 0 {
				rep.OK = false
				rep.Reason = fmt.Sprintf("sharded tree %s bucket %s has nested subtrees", k, bk)
				return false
			}
			for _, se := range sub.Entries {
				if TreeBucketKey(se.Path) != bk {
					rep.OK = false
					rep.Reason = fmt.Sprintf("sharded tree %s entry %q placed in wrong bucket %s", k, se.Path, bk)
					return false
				}
			}
		}
		rep.TreesVerified++
	}
	// Verify blob ContentSHA256 bindings and JevGraph sidecars against each commit's flattened root tree.
	checkedRoots := make(map[string]bool, len(cap.Commits))
	rootHashes := make([]string, 0, len(cap.Commits)+len(cap.Trees))
	for _, c := range cap.Commits {
		if c.TreeHash != "" && !checkedRoots[c.TreeHash] {
			checkedRoots[c.TreeHash] = true
			rootHashes = append(rootHashes, c.TreeHash)
		}
	}
	for k, tr := range cap.Trees {
		if len(tr.Subtrees) == 0 && !checkedRoots[k] {
			// Also check standalone v1 trees if not part of a sharded root.
			rootHashes = append(rootHashes, k)
		}
	}
	for _, rootH := range rootHashes {
		entries, fErr := FlattenTreeFromMap(rootH, cap.Trees)
		if fErr != nil {
			rep.OK = false
			rep.Reason = fmt.Sprintf("flatten tree %s failed: %v", rootH, fErr)
			return false
		}
		entryByPath := make(map[string]TreeEntry, len(entries))
		for _, e := range entries {
			entryByPath[e.Path] = e
			if b, ok := cap.Blobs[e.BlobHash]; ok {
				if !strings.EqualFold(b.ContentSHA256, e.ContentSHA256) {
					rep.OK = false
					rep.Reason = fmt.Sprintf("tree entry %q contentSha256 mismatches blob", e.Path)
					return false
				}
			}
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Path, "graph/") || !strings.HasSuffix(e.Path, ".edges.json") {
				continue
			}
			if verifiedSidecars[e.BlobHash] {
				continue
			}
			sb, ok := cap.Blobs[e.BlobHash]
			if !ok || sb.Encrypted || sb.Forgotten || sb.Redacted {
				continue
			}
			rawSidecar := ResolveBlobBytes(sb, objects)
			if len(rawSidecar) == 0 {
				continue
			}
			var sidecar jevgraph.EdgeSidecar
			if err := json.Unmarshal(rawSidecar, &sidecar); err != nil {
				rep.OK = false
				rep.Reason = fmt.Sprintf("graph sidecar %q invalid JSON: %v", e.Path, err)
				return false
			}
			srcEntry, hasSrc := entryByPath[sidecar.SourcePath]
			if !hasSrc {
				// Standalone bucket tree might not hold source if source is in another bucket; only fail on commit roots!
				if checkedRoots[rootH] {
					rep.OK = false
					rep.Reason = fmt.Sprintf("graph sidecar %q references missing source %q", e.Path, sidecar.SourcePath)
					return false
				}
				continue
			}
			srcBlob, hasBlob := cap.Blobs[srcEntry.BlobHash]
			if !hasBlob || srcBlob.Encrypted || srcBlob.Forgotten || srcBlob.Redacted {
				verifiedSidecars[e.BlobHash] = true
				continue
			}
			srcBytes := ResolveBlobBytes(srcBlob, objects)
			if len(srcBytes) == 0 && srcBlob.ChirpURL != "" && objects == nil {
				verifiedSidecars[e.BlobHash] = true
				continue
			}
			nEdges, vErr := jevgraph.VerifySidecar(srcBytes, sidecar)
			if vErr != nil {
				rep.OK = false
				rep.Reason = fmt.Sprintf("graph sidecar %q verification failed: %v", e.Path, vErr)
				return false
			}
			rep.EdgesVerified += nEdges
			verifiedSidecars[e.BlobHash] = true
		}
	}

	// 3. Verify every CommitObject hash, MPE root, signature, tree presence, and intra-capsule parent DAG.
	seenCommits := make(map[string]bool, len(cap.Commits))
	if cap.BaseCommit != "" {
		seenCommits[cap.BaseCommit] = true
	}
	for _, c := range cap.Commits {
		if err := VerifyCommitObject(c); err != nil {
			rep.OK = false
			rep.Reason = fmt.Sprintf("commit %s invalid: %v", c.CommitHash, err)
			return false
		}
		if _, ok := cap.Trees[c.TreeHash]; !ok {
			rep.OK = false
			rep.Reason = fmt.Sprintf("commit %s references missing tree %s", c.CommitHash, c.TreeHash)
			return false
		}
		for _, p := range c.Parents {
			// Primary parent inside a full capsule must exist earlier in topological order (cross-repo
			// market parents carry RepoGenesis and may live in the seller's external repo).
			if p.RepoGenesis == "" && cap.BaseCommit == "" && !seenCommits[p.CommitHash] {
				rep.OK = false
				rep.Reason = fmt.Sprintf("commit %s references unknown parent %s", c.CommitHash, p.CommitHash)
				return false
			}
		}
		seenCommits[c.CommitHash] = true
		rep.CommitsVerified++
	}
	if cap.HeadCommit != "" && len(cap.Commits) > 0 && !seenCommits[cap.HeadCommit] {
		rep.OK = false
		rep.Reason = fmt.Sprintf("headCommit %s missing from capsule commits", cap.HeadCommit)
		return false
	}
	return true
}

func verifyCapsuleTransitions(cap *MemoryCapsule, rep *CapsuleVerifyReport) bool {
	if len(cap.Transitions) == 0 {
		return true
	}
	baseQ := cap.BaseStateQ
	if len(baseQ) == 0 {
		baseQ = core.Zero32Bytes()
	}
	if _, err := core.VerifyTransitionChain(baseQ, cap.Transitions); err != nil {
		rep.OK = false
		rep.Reason = fmt.Sprintf("merkle automaton transition replay failed: %v", err)
		return false
	}
	rep.TransitionsVerified = len(cap.Transitions)
	return true
}

func verifyCapsuleAnchors(
	ctx context.Context,
	cap *MemoryCapsule,
	opts CapsuleVerifyOpts,
	rep *CapsuleVerifyReport,
) error {
	tracker := opts.Tracker
	memoryMode := opts.MemoryMode

	// 4. Verify contiguous BSV NektarAnchor AtomicBEEF + Rúnar state + STRICT UTXO SPEND LINEAGE
	//    (every successor spends prevTx:0 via NektarAnchor.Append() & passes post-Chronicle Script VM)
	//    + ChainRp + BUMP/SPV header proofs.
	//    For incremental delta capsules (BaseAnchorN >= 0), anchors start at BaseAnchorN+1 and anchor
	//    against either opts.LocalBaseAnchor (UTXO spend lineage + ChainRp) or cap.AnchorConsistency (MMR).
	prevRp := core.Zero32Bytes()
	var prevAnchorTx *transaction.Transaction
	var expectedN int64
	isIncremental := cap.BaseAnchorN >= 0 && len(cap.Anchors) > 0 && cap.Anchors[0].N == cap.BaseAnchorN+1
	if isIncremental {
		expectedN = cap.BaseAnchorN + 1
		if opts.LocalBaseAnchor != nil && opts.LocalBaseAnchor.N == cap.BaseAnchorN {
			if rpBytes, err := hex.DecodeString(strings.TrimSpace(opts.LocalBaseAnchor.ChainedHex)); err == nil && len(rpBytes) == 32 {
				prevRp = rpBytes
			}
			if len(opts.LocalBaseAnchor.Beef) > 0 {
				if _, baseTx, _, err := guard.ParseBEEF(opts.LocalBaseAnchor.Beef, guard.DefaultBound); err == nil && baseTx != nil {
					prevAnchorTx = baseTx
				}
			}
		} else if cap.AnchorConsistency == nil || len(cap.BaseMmrRoot) != 32 {
			rep.OK = false
			rep.Reason = fmt.Sprintf("incremental capsule starting at anchor n=%d requires LocalBaseAnchor(%d) or MMRConsistencyProof", expectedN, cap.BaseAnchorN)
			return nil
		}
	}

	for idx, a := range cap.Anchors {
		if len(a.Beef) == 0 {
			rep.OK = false
			rep.Reason = fmt.Sprintf("anchor n=%d missing BEEF", a.N)
			return nil
		}
		if a.N != expectedN {
			rep.OK = false
			rep.Reason = fmt.Sprintf("anchor batch gap: expected n=%d got %d", expectedN, a.N)
			return nil
		}
		// Fast path: when the caller just verified the exact same single anchor BEEF bytes via
		// contracts.VerifyFragmentWithMemo in the same deal, match by SHA-256(a.Beef) + TxID
		// and still verify every NektarAnchor field + R' chain commitment without re-parsing BEEF/BUMP.
		if len(cap.Anchors) == 1 && !isIncremental && opts.PreverifiedAnchor != nil &&
			opts.PreverifiedAnchor.State != nil && a.TxID == opts.PreverifiedAnchor.TxID &&
			sha256.Sum256(a.Beef) == opts.PreverifiedAnchor.BeefSHA256 {
			st := *opts.PreverifiedAnchor.State
			if rep.GenesisShardID == "" {
				rep.GenesisShardID = st.Shard
			} else if !strings.EqualFold(rep.GenesisShardID, st.Shard) {
				rep.OK = false
				rep.Reason = fmt.Sprintf("anchor n=%d shardId %s != genesis %s", a.N, st.Shard, rep.GenesisShardID)
				return nil
			}
			if st.N != uint64(a.N) || st.Size != uint64(a.Size) ||
				!strings.EqualFold(hex.EncodeToString(st.R), a.RootHex) ||
				!strings.EqualFold(hex.EncodeToString(st.Rp), a.ChainedHex) ||
				(a.StateHash != "" && !strings.EqualFold(hex.EncodeToString(st.StateHash), a.StateHash)) ||
				(a.MmrRoot != "" && !strings.EqualFold(hex.EncodeToString(st.MmrRoot), a.MmrRoot)) {
				rep.OK = false
				rep.Reason = fmt.Sprintf("anchor n=%d state mismatch vs NektarAnchor output", a.N)
				return nil
			}
			wantRp := core.ChainRpWithTag(st.Tag, prevRp, st.R, st.Size)
			if !core.EqualBytes(wantRp, st.Rp) {
				rep.OK = false
				rep.Reason = fmt.Sprintf("anchor n=%d R' chain link broken", a.N)
				return nil
			}
			if opts.PreverifiedAnchor.Settled {
				rep.SettledAnchors++
				rep.SettledByN[a.N] = true
				if rep.BlockHeightByN == nil {
					rep.BlockHeightByN = make(map[int64]int64)
				}
				rep.BlockHeightByN[a.N] = int64(opts.PreverifiedAnchor.BlockHeight)
			}
			prevRp = st.Rp
			expectedN++
			rep.AnchorsVerified++
			continue
		}
		parsedBeef, tx, txHash, err := guard.ParseBEEF(a.Beef, guard.DefaultBound)
		if err != nil || tx == nil || txHash == nil || len(tx.Outputs) == 0 || tx.Outputs[0].LockingScript == nil {
			rep.OK = false
			rep.Reason = fmt.Sprintf("anchor n=%d invalid BEEF", a.N)
			return nil
		}
		st, err := contracts.DecodeNektarAnchor(*tx.Outputs[0].LockingScript)
		if err != nil {
			rep.OK = false
			rep.Reason = fmt.Sprintf("anchor n=%d invalid NektarAnchor script: %v", a.N, err)
			return nil
		}
		if rep.GenesisShardID == "" {
			rep.GenesisShardID = st.Shard
		} else if !strings.EqualFold(rep.GenesisShardID, st.Shard) {
			rep.OK = false
			rep.Reason = fmt.Sprintf("anchor n=%d shardId %s != genesis %s", a.N, st.Shard, rep.GenesisShardID)
			return nil
		}
		if st.N != uint64(a.N) || st.Size != uint64(a.Size) ||
			!strings.EqualFold(hex.EncodeToString(st.R), a.RootHex) ||
			!strings.EqualFold(hex.EncodeToString(st.Rp), a.ChainedHex) ||
			(a.StateHash != "" && !strings.EqualFold(hex.EncodeToString(st.StateHash), a.StateHash)) ||
			(a.MmrRoot != "" && !strings.EqualFold(hex.EncodeToString(st.MmrRoot), a.MmrRoot)) ||
			txHash.String() != a.TxID {
			rep.OK = false
			rep.Reason = fmt.Sprintf("anchor n=%d state mismatch vs NektarAnchor output", a.N)
			return nil
		}
		if idx == 0 && isIncremental && prevAnchorTx == nil {
			// Standalone incremental checkpoint: verify MMR append-only consistency from BaseMmrRoot -> st.MmrRoot.
			if err := core.VerifyMMRConsistency(cap.BaseMmrRoot, st.MmrRoot, cap.AnchorConsistency); err != nil {
				rep.OK = false
				rep.Reason = fmt.Sprintf("anchor n=%d MMR consistency proof invalid: %v", a.N, err)
				return nil
			}
		} else {
			wantRp := core.ChainRpWithTag(st.Tag, prevRp, st.R, st.Size)
			if !core.EqualBytes(wantRp, st.Rp) {
				rep.OK = false
				rep.Reason = fmt.Sprintf("anchor n=%d R' chain link broken", a.N)
				return nil
			}
			if prevAnchorTx != nil {
				if lErr := contracts.VerifyAnchorUTXOLineageWithBEEF(prevAnchorTx, tx, parsedBeef); lErr != nil {
					rep.OK = false
					rep.Reason = fmt.Sprintf("anchor n=%d UTXO lineage broken: %v", a.N, lErr)
					return nil
				}
			}
		}
		if tx.MerklePath != nil {
			root, rErr := tx.MerklePath.ComputeRoot(txHash)
			if rErr != nil || root == nil {
				rep.OK = false
				rep.Reason = fmt.Sprintf("anchor n=%d invalid BUMP", a.N)
				return nil
			}
			if !memoryMode {
				if tracker == nil {
					return fmt.Errorf("memrepo: tracker required verifying capsule anchor %d: %w", a.N, chain.ErrTrackerUnavailable)
				}
				valid, vErr := tracker.IsValidRootForHeight(ctx, root, tx.MerklePath.BlockHeight)
				if vErr != nil {
					return fmt.Errorf("memrepo: header tracker error on anchor %d: %w", a.N, vErr)
				}
				if !valid {
					rep.OK = false
					rep.Reason = fmt.Sprintf("anchor n=%d BUMP root rejected by headers", a.N)
					return nil
				}
			}
			rep.SettledAnchors++
			rep.SettledByN[a.N] = true
			if rep.BlockHeightByN == nil {
				rep.BlockHeightByN = make(map[int64]int64)
			}
			rep.BlockHeightByN[a.N] = int64(tx.MerklePath.BlockHeight)
		} else if !memoryMode {
			if tracker == nil {
				return fmt.Errorf("memrepo: tracker required verifying unmined capsule anchor %d: %w", a.N, chain.ErrTrackerUnavailable)
			}
			if sErr := chain.VerifyTransactionSPV(ctx, tx, parsedBeef, tracker); sErr != nil {
				if errors.Is(sErr, chain.ErrTrackerUnavailable) {
					return sErr
				}
				rep.OK = false
				rep.Reason = fmt.Sprintf("unmined anchor n=%d failed carried-ancestry SPV: %v", a.N, sErr)
				return nil
			}
		}
		prevAnchorTx = tx
		prevRp = st.Rp
		expectedN++
		rep.AnchorsVerified++
	}
	return nil
}

func verifyCapsuleFragmentsAndBatches(cap *MemoryCapsule, rep *CapsuleVerifyReport) {
	// 5. Verify every fragment record in cap.Fragments: framed v2 HDP, mandatory provenance on plaintext,
	//    L1-bound TagLeaf commitment, AND cryptographic Merkle inclusion against verified NektarAnchor R_n / Size!
	anchorByN := make(map[int64]store.BatchAnchor, len(cap.Anchors))
	for _, a := range cap.Anchors {
		anchorByN[a.N] = a
	}
	fragsByBatch := make(map[int64]map[int]string)
	for _, f := range cap.Fragments {
		if f.Forgotten {
			continue
		}
		hdpBytes, hErr := hex.DecodeString(strings.TrimSpace(f.HDP))
		if hErr != nil || len(hdpBytes) != 32 {
			rep.OK = false
			rep.Reason = fmt.Sprintf("fragment seq=%d invalid 32-byte HDP", f.Seq)
			return
		}
		if f.BorgGroup == "" && f.LatticeEpoch == 0 {
			if len(f.Provenance) == 0 {
				rep.OK = false
				rep.Reason = fmt.Sprintf("fragment seq=%d missing provenance bytes", f.Seq)
				return
			}
			if !core.VerifyHDP(f.Data, f.Provenance, hdpBytes) {
				rep.OK = false
				rep.Reason = fmt.Sprintf("fragment seq=%d plaintext/provenance HDP tamper detected", f.Seq)
				return
			}
		} else if len(f.Data) > 0 {
			rep.OK = false
			rep.Reason = fmt.Sprintf("fragment seq=%d plaintext injection on encrypted fragment", f.Seq)
			return
		}
		mpeBytes, mErr := hex.DecodeString(strings.TrimSpace(f.MPERoot))
		if mErr != nil || len(mpeBytes) != 32 {
			rep.OK = false
			rep.Reason = fmt.Sprintf("fragment seq=%d missing 32-byte L1 MPE root", f.Seq)
			return
		}
		var contentBytes, commitBytes, transBytes []byte
		if strings.TrimSpace(f.ContentSHA256) != "" {
			var cErr error
			contentBytes, cErr = hex.DecodeString(strings.TrimSpace(f.ContentSHA256))
			if cErr != nil || len(contentBytes) != 32 {
				rep.OK = false
				rep.Reason = fmt.Sprintf("fragment seq=%d invalid 32-byte contentSha256", f.Seq)
				return
			}
		}
		if strings.TrimSpace(f.CommitHash) != "" {
			if cb, cErr := hex.DecodeString(strings.TrimSpace(f.CommitHash)); cErr == nil && len(cb) == 32 {
				commitBytes = cb
			}
		}
		if strings.TrimSpace(f.TransitionHash) != "" {
			if tb, tErr := hex.DecodeString(strings.TrimSpace(f.TransitionHash)); tErr == nil && len(tb) == 32 {
				transBytes = tb
			}
		}
		wantLeaf := core.ToHex(core.ComputeLeaf(core.Fragment{
			HDP:            hdpBytes,
			Ciphertext:     f.Ciphertext,
			MPERoot:        mpeBytes,
			ContentSHA256:  contentBytes,
			CommitHash:     commitBytes,
			TransitionHash: transBytes,
		}))
		if !strings.EqualFold(f.Leaf, wantLeaf) {
			rep.OK = false
			rep.Reason = fmt.Sprintf("fragment seq=%d leaf commitment mismatch", f.Seq)
			return
		}
		if f.BatchN >= 0 {
			anc, ok := anchorByN[f.BatchN]
			if !ok {
				rep.OK = false
				rep.Reason = fmt.Sprintf("fragment seq=%d claims anchor batch n=%d missing from capsule", f.Seq, f.BatchN)
				return
			}
			if f.AnchorTxID != "" && !strings.EqualFold(f.AnchorTxID, anc.TxID) {
				rep.OK = false
				rep.Reason = fmt.Sprintf("fragment seq=%d anchorTxid %s mismatches anchor n=%d (%s)", f.Seq, f.AnchorTxID, anc.N, anc.TxID)
				return
			}
			if f.LeafIndex < 0 || f.LeafIndex >= anc.Size {
				rep.OK = false
				rep.Reason = fmt.Sprintf("fragment seq=%d leafIndex %d outside anchor n=%d size %d", f.Seq, f.LeafIndex, anc.N, anc.Size)
				return
			}
			if fragsByBatch[f.BatchN] == nil {
				fragsByBatch[f.BatchN] = make(map[int]string)
			}
			if prevLeaf, dup := fragsByBatch[f.BatchN][f.LeafIndex]; dup && !strings.EqualFold(prevLeaf, wantLeaf) {
				rep.OK = false
				rep.Reason = fmt.Sprintf("duplicate conflicting fragment at batch n=%d leafIndex=%d", f.BatchN, f.LeafIndex)
				return
			}
			fragsByBatch[f.BatchN][f.LeafIndex] = strings.ToLower(wantLeaf)
		}
	}

	// 5b. Cryptographically verify Merkle inclusion of every anchored batch's disclosed leaves
	//     against the BEEF-verified NektarAnchor (R_n, Size).
	verifiedLeavesByBatch := make(map[int64]map[string]bool)
	batchesToCheck := make(map[int64]bool)
	for n := range fragsByBatch {
		batchesToCheck[n] = true
	}
	for n := range cap.BatchProofs {
		batchesToCheck[n] = true
	}
	for n := range batchesToCheck {
		anc, ok := anchorByN[n]
		if !ok {
			rep.OK = false
			rep.Reason = fmt.Sprintf("batch proof or fragment references missing anchor n=%d", n)
			return
		}
		if verifiedLeavesByBatch[n] == nil {
			verifiedLeavesByBatch[n] = make(map[string]bool)
		}
		if sp := cap.BatchProofs[n]; sp != nil {
			if err := VerifySubsetProof(anc.RootHex, anc.Size, sp); err != nil {
				rep.OK = false
				rep.Reason = fmt.Sprintf("batch n=%d subset Merkle proof invalid: %v", n, err)
				return
			}
			proofByIdx := make(map[int]string, len(sp.Leaves))
			for _, lp := range sp.Leaves {
				lHex := strings.ToLower(lp.Leaf)
				proofByIdx[lp.Index] = lHex
				verifiedLeavesByBatch[n][lHex] = true
			}
			for idx, leafHex := range fragsByBatch[n] {
				if pLeaf, exists := proofByIdx[idx]; !exists || pLeaf != leafHex {
					rep.OK = false
					rep.Reason = fmt.Sprintf("fragment batch n=%d leafIndex=%d not proven in subset proof", n, idx)
					return
				}
			}
		} else {
			// Without an explicit SubsetDisclosureProof, all anc.Size leaves [0..Size-1] must be present
			// and reconstruct anc.RootHex exactly.
			batchMap := fragsByBatch[n]
			if len(batchMap) != anc.Size {
				rep.OK = false
				rep.Reason = fmt.Sprintf("batch n=%d has %d/%d leaves and no subset proof to verify Merkle root R", n, len(batchMap), anc.Size)
				return
			}
			ordered := make([][]byte, anc.Size)
			for idx := 0; idx < anc.Size; idx++ {
				lHex, ok := batchMap[idx]
				if !ok {
					rep.OK = false
					rep.Reason = fmt.Sprintf("batch n=%d missing leafIndex %d for Merkle root R verification", n, idx)
					return
				}
				lb, err := hex.DecodeString(lHex)
				if err != nil || len(lb) != 32 {
					rep.OK = false
					rep.Reason = fmt.Sprintf("batch n=%d invalid leaf hex at index %d", n, idx)
					return
				}
				ordered[idx] = lb
				verifiedLeavesByBatch[n][lHex] = true
			}
			rebuiltRoot, err := core.MerkleRoot(ordered)
			if err != nil || !strings.EqualFold(hex.EncodeToString(rebuiltRoot), anc.RootHex) {
				rep.OK = false
				rep.Reason = fmt.Sprintf("batch n=%d fragment leaves Merkle root mismatches anchor R", n)
				return
			}
		}
	}

	// 5c. Bind every anchored CommitObject to a verified anchor txid and Merkle-verified batch leaf.
	for _, c := range cap.Commits {
		if c.AnchorN < 0 && strings.TrimSpace(c.AnchorTxID) == "" {
			continue
		}
		anc, ok := anchorByN[c.AnchorN]
		if !ok {
			rep.OK = false
			rep.Reason = fmt.Sprintf("commit %s claims missing anchor batch n=%d", c.CommitHash, c.AnchorN)
			return
		}
		if c.AnchorTxID != "" && !strings.EqualFold(c.AnchorTxID, anc.TxID) {
			rep.OK = false
			rep.Reason = fmt.Sprintf("commit %s anchorTxid %s mismatches anchor n=%d (%s)", c.CommitHash, c.AnchorTxID, anc.N, anc.TxID)
			return
		}
		if strings.TrimSpace(c.FragmentLeaf) == "" || !verifiedLeavesByBatch[c.AnchorN][strings.ToLower(c.FragmentLeaf)] {
			rep.OK = false
			rep.Reason = fmt.Sprintf("commit %s fragmentLeaf is not Merkle-proven under anchor batch n=%d", c.CommitHash, c.AnchorN)
			return
		}
	}
}
