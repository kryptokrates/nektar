package contracts

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/core"
)

// VerifyFragment performs full offline SPV verification of a ProofBundle using only block headers.
func VerifyFragment(bundle ProofBundle, tracker chaintracker.ChainTracker) VerifyResult {
	return VerifyFragmentCtx(context.Background(), bundle, tracker)
}

// VerifyFragmentCtx performs full offline SPV verification of a ProofBundle using ctx for header verification.
func VerifyFragmentCtx(ctx context.Context, bundle ProofBundle, tracker chaintracker.ChainTracker) VerifyResult {
	return VerifyFragmentWithDepth(ctx, bundle, tracker, 1)
}

// VerifyFragmentWithDepth performs full offline SPV verification of a ProofBundle requiring at least
// minDepth block confirmations against the witnessed header tip before returning OK:true, Settled:true.
func VerifyFragmentWithDepth(ctx context.Context, bundle ProofBundle, tracker chaintracker.ChainTracker, minDepth int) VerifyResult {
	if minDepth <= 0 {
		minDepth = 1
	}
	return verifyFragment(ctx, bundle, tracker, true, minDepth, nil)
}

// VerifyFragmentWithMemo verifies bundle using an optional per-batch memo cache (keyed by caller per batch)
// while still verifying this fragment's individual leaf -> R_n Merkle proof independently.
func VerifyFragmentWithMemo(ctx context.Context, bundle ProofBundle, tracker chaintracker.ChainTracker, minDepth int, memo *AnchorBatchMemo) (VerifyResult, *AnchorBatchMemo) {
	if minDepth <= 0 {
		minDepth = 1
	}
	if memo != nil && memo.State != nil &&
		memo.AnchorTxID != "" && memo.AnchorTxID == bundle.Anchor.TxID &&
		(memo.Result.Depth >= minDepth || (!memo.Result.OK && memo.Result.Reason != "InsufficientDepth")) {
		if vr := verifyFragmentLeafOnly(bundle, memo.State); !vr.OK {
			return vr, memo
		}
		return memo.Result, memo
	}
	var outMemo AnchorBatchMemo
	vr := verifyFragment(ctx, bundle, tracker, true, minDepth, &outMemo)
	if outMemo.State != nil {
		return vr, &outMemo
	}
	return vr, nil
}

// VerifyFragmentStructural verifies all cryptographic commitments in a ProofBundle (AnchorBEEF,
// Rúnar NektarAnchor locking script & state, batch Merkle path, ChainTail R' hash chain, and BUMP
// root computation) WITHOUT checking the BUMP root against a ChainTracker.
// MUST ONLY be used for cross-node tests on isolated ephemeral "memory" testchains; live networks
// MUST always call VerifyFragmentCtx/VerifyFragmentWithDepth with a real witnessed PoW ChainTracker.
func VerifyFragmentStructural(bundle ProofBundle) VerifyResult {
	return verifyFragment(context.Background(), bundle, nil, false, 1, nil)
}

// MaxMerkleBatchSize caps untrusted ProofBundle.Size to prevent integer-overflow or DoS.
const MaxMerkleBatchSize uint64 = 1 << 24

func verifyFragmentLeafOnly(bundle ProofBundle, state *AnchorState) VerifyResult {
	if bundle.Size == 0 || bundle.Size > MaxMerkleBatchSize {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "batch size out of bounds"}
	}
	if len(bundle.Path) > core.MaxMerkleProofDepth {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "merkle proof depth exceeds cap"}
	}
	if bundle.LeafIndex < 0 || uint64(bundle.LeafIndex) >= bundle.Size {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "leafIndex outside batch size"}
	}
	if len(bundle.Path) != core.HeightForLeafCount(int(bundle.Size)) {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "path height does not match committed batch size"}
	}
	leaf, err := hex.DecodeString(bundle.Leaf)
	if err != nil || len(leaf) != 32 {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "leaf must be 32-byte hex"}
	}
	sibs := make([][]byte, len(bundle.Path))
	for i, s := range bundle.Path {
		b, err := hex.DecodeString(s)
		if err != nil || len(b) != 32 {
			return VerifyResult{OK: false, Reason: "BadBundle", Detail: "path sibling must be 32-byte hex"}
		}
		sibs[i] = b
	}
	recomputedR, err := core.ReconstructRootChecked(leaf, &core.MerkleProof{Index: bundle.LeafIndex, Siblings: sibs})
	if err != nil {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: err.Error()}
	}
	if state.Shard != bundle.Shard || state.N != bundle.N || state.Size != bundle.Size {
		return VerifyResult{OK: false, Reason: "AnchorFieldMismatch", Detail: "shard/n/size mismatch"}
	}
	if !core.EqualBytes(state.R, recomputedR) {
		return VerifyResult{OK: false, Reason: "LeafPathMismatch", Detail: "recomputed R_n != anchor R_n"}
	}
	return VerifyResult{OK: true}
}

func hydrateAnchorInputs(tx *transaction.Transaction, beef *transaction.Beef, visiting map[string]bool) bool {
	if tx == nil || tx.MerklePath != nil {
		return true
	}
	id := tx.TxID().String()
	if visiting[id] {
		return false
	}
	visiting[id] = true
	defer delete(visiting, id)
	for _, in := range tx.Inputs {
		if in == nil || in.SourceTXID == nil {
			return false
		}
		if in.SourceTransaction == nil && beef != nil {
			in.SourceTransaction = beef.FindTransaction(in.SourceTXID.String())
		}
		if in.SourceTransaction == nil {
			return false
		}
		if in.SourceTransaction.MerklePath == nil && !hydrateAnchorInputs(in.SourceTransaction, beef, visiting) {
			return false
		}
	}
	return true
}

func verifyFragment(ctx context.Context, bundle ProofBundle, tracker chaintracker.ChainTracker, checkHeaders bool, minDepth int, memoOut *AnchorBatchMemo) VerifyResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if bundle.Size == 0 || bundle.Size > MaxMerkleBatchSize {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "batch size out of bounds"}
	}
	if len(bundle.Path) > core.MaxMerkleProofDepth {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "merkle proof depth exceeds cap"}
	}
	if bundle.LeafIndex < 0 || uint64(bundle.LeafIndex) >= bundle.Size {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "leafIndex outside batch size"}
	}
	if len(bundle.Path) != core.HeightForLeafCount(int(bundle.Size)) {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "path height does not match committed batch size"}
	}
	leaf, err := hex.DecodeString(bundle.Leaf)
	if err != nil || len(leaf) != 32 {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "leaf must be 32-byte hex"}
	}
	sibs := make([][]byte, len(bundle.Path))
	for i, s := range bundle.Path {
		b, err := hex.DecodeString(s)
		if err != nil || len(b) != 32 {
			return VerifyResult{OK: false, Reason: "BadBundle", Detail: "path sibling must be 32-byte hex"}
		}
		sibs[i] = b
	}
	recomputedR, err := core.ReconstructRootChecked(leaf, &core.MerkleProof{Index: bundle.LeafIndex, Siblings: sibs})
	if err != nil {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: err.Error()}
	}

	parsedBeef, anchorTx, _, err := guard.ParseBEEF(bundle.AnchorBEEF, guard.DefaultBound)
	if err != nil || anchorTx == nil {
		msg := "empty transaction"
		if err != nil {
			msg = err.Error()
		}
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "BEEF parse: " + msg}
	}
	outIdx := bundle.Anchor.OutputIndex
	if int(outIdx) >= len(anchorTx.Outputs) || anchorTx.Outputs[outIdx].LockingScript == nil {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: fmt.Sprintf("anchor tx missing output %d", outIdx)}
	}
	state, err := DecodeNektarAnchor(*anchorTx.Outputs[outIdx].LockingScript)
	if err != nil {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: err.Error()}
	}
	if state.Shard != bundle.Shard || state.N != bundle.N || state.Size != bundle.Size {
		return VerifyResult{OK: false, Reason: "AnchorFieldMismatch", Detail: "shard/n/size mismatch"}
	}
	if !core.EqualBytes(state.R, recomputedR) {
		return VerifyResult{OK: false, Reason: "LeafPathMismatch", Detail: "recomputed R_n != anchor R_n"}
	}

	// Validate ChainTail continuity BEFORE checking mined/unmined status so a forged unmined
	// ChainTail ALWAYS fails closed as ChainInconsistent (RED) rather than rendering Unsettled (AMBER).
	if len(bundle.ChainTail) == 0 {
		return VerifyResult{OK: false, Reason: "ChainInconsistent", Detail: "empty chainTail"}
	}
	last := bundle.ChainTail[len(bundle.ChainTail)-1]
	txidStr := anchorTx.TxID().String()
	if last.TxID != bundle.Anchor.TxID || last.TxID != txidStr || last.N != state.N || last.Size != state.Size ||
		!strings.EqualFold(last.R, hex.EncodeToString(state.R)) || !strings.EqualFold(last.Rp, hex.EncodeToString(state.Rp)) {
		return VerifyResult{OK: false, Reason: "ChainInconsistent", Detail: "chainTail head mismatches on-chain anchor"}
	}
	prevRp := core.Zero32Bytes()
	var prevLinkTx *transaction.Transaction
	for i, link := range bundle.ChainTail {
		if link.N != uint64(i) {
			return VerifyResult{OK: false, Reason: "ChainInconsistent", Detail: fmt.Sprintf("gap at n=%d", i)}
		}
		rBytes, err1 := hex.DecodeString(link.R)
		rpBytes, err2 := hex.DecodeString(link.Rp)
		if err1 != nil || err2 != nil || len(rBytes) != 32 || len(rpBytes) != 32 {
			return VerifyResult{OK: false, Reason: "ChainInconsistent", Detail: "invalid R/Rp hex in chainTail"}
		}
		if !core.EqualBytes(rpBytes, core.ChainRpWithTag(state.Tag, prevRp, rBytes, link.Size)) {
			return VerifyResult{OK: false, Reason: "ChainInconsistent", Detail: fmt.Sprintf("R' link broken at n=%d", i)}
		}
		var curLinkTx *transaction.Transaction
		var curLinkBeef *transaction.Beef
		if i == len(bundle.ChainTail)-1 {
			curLinkTx = anchorTx
			curLinkBeef = parsedBeef
		} else if len(link.AnchorBEEF) > 0 {
			if bObj, parsedLinkTx, linkHash, lErr := guard.ParseBEEF(link.AnchorBEEF, guard.DefaultBound); lErr == nil && parsedLinkTx != nil && linkHash != nil && linkHash.String() == link.TxID {
				curLinkTx = parsedLinkTx
				curLinkBeef = bObj
			} else {
				return VerifyResult{OK: false, Reason: "UTXOLineageBroken", Detail: fmt.Sprintf("invalid chainTail anchorBEEF at n=%d", i)}
			}
		} else if parsedBeef != nil {
			curLinkTx = parsedBeef.FindTransaction(link.TxID)
			curLinkBeef = parsedBeef
		}
		if curLinkTx == nil {
			if len(bundle.ChainTail) > 1 {
				return VerifyResult{OK: false, Reason: "UTXOLineageBroken", Detail: fmt.Sprintf("missing anchor transaction at n=%d to verify intermediate state and UTXO lineage", i)}
			}
		} else {
			if len(curLinkTx.Outputs) == 0 || curLinkTx.Outputs[0] == nil || curLinkTx.Outputs[0].LockingScript == nil {
				return VerifyResult{OK: false, Reason: "ChainInconsistent", Detail: fmt.Sprintf("chainTail tx at n=%d missing output 0", i)}
			}
			curSt, dErr := DecodeNektarAnchor(*curLinkTx.Outputs[0].LockingScript)
			if dErr != nil {
				return VerifyResult{OK: false, Reason: "ChainInconsistent", Detail: fmt.Sprintf("chainTail tx at n=%d invalid NektarAnchor: %v", i, dErr)}
			}
			if curSt.Shard != state.Shard || curSt.OwnerPub != state.OwnerPub ||
				curSt.N != link.N || curSt.Size != link.Size ||
				!core.EqualBytes(curSt.R, rBytes) || !core.EqualBytes(curSt.Rp, rpBytes) ||
				(link.StateHash != "" && !strings.EqualFold(hex.EncodeToString(curSt.StateHash), link.StateHash)) ||
				(link.MmrRoot != "" && !strings.EqualFold(hex.EncodeToString(curSt.MmrRoot), link.MmrRoot)) {
				return VerifyResult{OK: false, Reason: "ChainInconsistent", Detail: fmt.Sprintf("chainTail tx at n=%d locking script state mismatches link fields", i)}
			}
		}
		if i > 0 {
			if prevLinkTx == nil {
				return VerifyResult{OK: false, Reason: "UTXOLineageBroken", Detail: fmt.Sprintf("missing predecessor anchor transaction at n=%d to verify UTXO spend lineage", i-1)}
			}
			if lErr := VerifyAnchorUTXOLineageWithBEEF(prevLinkTx, curLinkTx, curLinkBeef); lErr != nil {
				return VerifyResult{OK: false, Reason: "UTXOLineageBroken", Detail: fmt.Sprintf("anchor n=%d failed UTXO spend lineage: %v", i, lErr)}
			}
		}
		prevLinkTx = curLinkTx
		prevRp = rpBytes
	}

	saveMemo := func(vr VerifyResult) VerifyResult {
		if memoOut != nil {
			memoOut.State = state
			memoOut.AnchorTxID = txidStr
			memoOut.Result = vr
		}
		return vr
	}

	settled := false
	anchorState := "broadcast"
	var blockHeight uint32
	var depth int
	if anchorTx.MerklePath != nil {
		if len(anchorTx.MerklePath.Path) > core.MaxMerkleProofDepth {
			return saveMemo(VerifyResult{OK: false, Reason: "BEEFInvalid", Detail: "BUMP merkle path depth exceeds cap"})
		}
		txidHash := anchorTx.TxID()
		root, err := anchorTx.MerklePath.ComputeRoot(txidHash)
		if err != nil {
			return saveMemo(VerifyResult{OK: false, Reason: "BEEFInvalid", Detail: "merkle path compute root: " + err.Error()})
		}
		blockHeight = anchorTx.MerklePath.BlockHeight
		if checkHeaders {
			if tracker == nil {
				return saveMemo(VerifyResult{OK: false, Reason: "TrackerUnavailable", Detail: "no chain tracker configured to verify BUMP"})
			}
			valid, err := tracker.IsValidRootForHeight(ctx, root, blockHeight)
			if err != nil {
				// Distinctly report tracker transport/quorum outage (AMBER) vs root mismatch forgery (RED).
				return saveMemo(VerifyResult{OK: false, Settled: false, AnchorState: "broadcast", BlockHeight: blockHeight, Reason: "TrackerUnavailable", Detail: "header tracker error: " + err.Error()})
			}
			if !valid {
				return saveMemo(VerifyResult{OK: false, Settled: false, AnchorState: "broadcast", BlockHeight: blockHeight, Reason: "BEEFInvalid", Detail: "BUMP root not valid in header chain"})
			}
			if minDepth > 1 {
				tip, tErr := tracker.CurrentHeight(ctx)
				if tErr != nil {
					return saveMemo(VerifyResult{OK: false, Settled: false, AnchorState: "mined", BlockHeight: blockHeight, Reason: "TrackerUnavailable", Detail: "header tip height error: " + tErr.Error()})
				}
				if tip >= blockHeight {
					depth = int(tip-blockHeight) + 1
				}
				if depth < minDepth {
					return saveMemo(VerifyResult{
						OK:          false,
						Settled:     false,
						AnchorState: "mined",
						BlockHeight: blockHeight,
						Depth:       depth,
						Reason:      "InsufficientDepth",
						Detail:      fmt.Sprintf("anchor confirmation depth %d < required settleDepth %d", depth, minDepth),
					})
				}
			} else {
				depth = 1
			}
			settled = true
			anchorState = "mined"
		}
	} else if checkHeaders {
		// Safety invariant: NEVER return OK:true for unmined anchors. Check carried funding ancestry
		// via go-sdk/spv.Verify and report it separately in AncestryVerified while returning Reason:"Unsettled".
		ancestryOK := false
		if tracker != nil && hydrateAnchorInputs(anchorTx, parsedBeef, make(map[string]bool)) {
			if ok, sErr := spv.Verify(ctx, anchorTx, tracker, nil); sErr == nil && ok {
				ancestryOK = true
			}
		}
		return saveMemo(VerifyResult{
			OK:               false,
			Settled:          false,
			AncestryVerified: ancestryOK,
			AnchorState:      "broadcast",
			Reason:           "Unsettled",
			Detail:           "anchor tx has no mined MerklePath (BUMP)",
		})
	}

	_ = chainhash.Hash{}
	return saveMemo(VerifyResult{
		OK:               true,
		Settled:          settled,
		AncestryVerified: settled,
		AnchorState:      anchorState,
		BlockHeight:      blockHeight,
		Depth:            depth,
	})
}

// VerifyAnchoredMPEProof verifies a complete L1-bound selective-disclosure proof offline:
//  1. Verifies every disclosed field inside proof against mpeRootHex (WO 2022/100946 MPE).
//  2. Recomputes ComputeLeaf(HDP, CiphertextHash, MPERoot, ContentSHA256, CommitHash, TransitionHash)
//     and confirms it matches bundle.Leaf.
//  3. Runs full SPV VerifyFragmentCtx(bundle, tracker) proving bundle.Leaf -> R_n -> NektarAnchor UTXO -> BUMP -> headers.
func VerifyAnchoredMPEProof(
	ctx context.Context,
	hdpHex string,
	ciphertextHashHex string,
	contentSHA256Hex string,
	mpeRootHex string,
	commitHashHex string,
	transitionHashHex string,
	proof *core.SelectiveDisclosureProof,
	bundle ProofBundle,
	tracker chaintracker.ChainTracker,
) VerifyResult {
	if err := core.VerifySelectiveDisclosure(mpeRootHex, proof); err != nil {
		return VerifyResult{OK: false, Reason: "MPEInvalid", Detail: err.Error()}
	}
	hdp, err := hex.DecodeString(strings.TrimSpace(hdpHex))
	if err != nil || len(hdp) != 32 {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "hdpHex must be 32-byte hex"}
	}
	mpeRoot, err := hex.DecodeString(strings.TrimSpace(mpeRootHex))
	if err != nil || len(mpeRoot) != 32 {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "mpeRootHex must be 32-byte hex"}
	}
	eh := core.ComputeEH(nil)
	if s := strings.TrimSpace(ciphertextHashHex); s != "" {
		b, dErr := hex.DecodeString(s)
		if dErr != nil || len(b) != 32 {
			return VerifyResult{OK: false, Reason: "BadBundle", Detail: "ciphertextHashHex must be 32-byte hex"}
		}
		eh = b
	}
	contentHash := core.Zero32Bytes()
	if s := strings.TrimSpace(contentSHA256Hex); s != "" {
		b, dErr := hex.DecodeString(s)
		if dErr != nil || len(b) != 32 {
			return VerifyResult{OK: false, Reason: "BadBundle", Detail: "contentSHA256Hex must be 32-byte hex"}
		}
		contentHash = b
	}
	commitHash := core.Zero32Bytes()
	if s := strings.TrimSpace(commitHashHex); s != "" {
		b, dErr := hex.DecodeString(s)
		if dErr != nil || len(b) != 32 {
			return VerifyResult{OK: false, Reason: "BadBundle", Detail: "commitHashHex must be 32-byte hex"}
		}
		commitHash = b
	}
	transHash := core.Zero32Bytes()
	if s := strings.TrimSpace(transitionHashHex); s != "" {
		b, dErr := hex.DecodeString(s)
		if dErr != nil || len(b) != 32 {
			return VerifyResult{OK: false, Reason: "BadBundle", Detail: "transitionHashHex must be 32-byte hex"}
		}
		transHash = b
	}
	expectedLeaf := core.SHA256(core.Concat(core.TagLeaf, hdp, eh, mpeRoot, contentHash, commitHash, transHash))
	if !strings.EqualFold(bundle.Leaf, hex.EncodeToString(expectedLeaf)) {
		return VerifyResult{OK: false, Reason: "MPELeafMismatch", Detail: "MPE/commit/transition root is not committed inside anchored leaf"}
	}
	return VerifyFragmentCtx(ctx, bundle, tracker)
}

// VerifyFragmentCompact verifies any historical fragment against the shard's HEAD NektarAnchor
// in O(log N) bytes via: Leaf -> BatchR -> MMRInclusionProof -> HeadAnchor.MmrRoot -> BUMP -> BlockHeaders.
func VerifyFragmentCompact(ctx context.Context, bundle CompactProofBundle, tracker chaintracker.ChainTracker, minDepth int) VerifyResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if minDepth <= 0 {
		minDepth = 1
	}
	if bundle.BatchSize == 0 || bundle.BatchSize > MaxMerkleBatchSize {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "batch size out of bounds"}
	}
	if bundle.LeafIndex < 0 || uint64(bundle.LeafIndex) >= bundle.BatchSize || len(bundle.BatchPath) != core.HeightForLeafCount(int(bundle.BatchSize)) {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "invalid leafIndex or batchPath height"}
	}
	leaf, err := hex.DecodeString(bundle.Leaf)
	if err != nil || len(leaf) != 32 {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "leaf must be 32-byte hex"}
	}
	sibs := make([][]byte, len(bundle.BatchPath))
	for i, s := range bundle.BatchPath {
		b, dErr := hex.DecodeString(s)
		if dErr != nil || len(b) != 32 {
			return VerifyResult{OK: false, Reason: "BadBundle", Detail: "invalid batchPath sibling hex"}
		}
		sibs[i] = b
	}
	recomputedR, err := core.ReconstructRootChecked(leaf, &core.MerkleProof{Index: bundle.LeafIndex, Siblings: sibs})
	if err != nil {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: err.Error()}
	}
	if bundle.BatchRoot != "" && !strings.EqualFold(bundle.BatchRoot, hex.EncodeToString(recomputedR)) {
		return VerifyResult{OK: false, Reason: "LeafPathMismatch", Detail: "recomputed batch R mismatches batchRoot"}
	}

	parsedBeef, headTx, _, err := guard.ParseBEEF(bundle.HeadAnchorBEEF, guard.DefaultBound)
	if err != nil || headTx == nil {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "invalid headAnchorBEEF"}
	}
	outIdx := bundle.HeadAnchor.OutputIndex
	if int(outIdx) >= len(headTx.Outputs) || headTx.Outputs[outIdx] == nil || headTx.Outputs[outIdx].LockingScript == nil {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "headAnchor missing output"}
	}
	headState, err := DecodeNektarAnchor(*headTx.Outputs[outIdx].LockingScript)
	if err != nil {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: err.Error()}
	}
	if headState.Shard != bundle.Shard || headState.N != bundle.HeadAnchor.N || headTx.TxID().String() != bundle.HeadAnchor.TxID {
		return VerifyResult{OK: false, Reason: "AnchorFieldMismatch", Detail: "headAnchor shard/n/txid mismatch"}
	}
	if bundle.MMRProof == nil || bundle.MMRProof.LeafIndex != bundle.BatchN || bundle.MMRProof.LeafCount != headState.N+1 {
		return VerifyResult{OK: false, Reason: "MMRInvalid", Detail: "MMR proof index/count does not match headAnchor N+1"}
	}
	if err := core.VerifyMMRInclusion(recomputedR, bundle.MMRProof, headState.MmrRoot); err != nil {
		return VerifyResult{OK: false, Reason: "MMRInvalid", Detail: err.Error()}
	}
	if headTx.MerklePath == nil {
		ancestryOK := false
		if tracker != nil && hydrateAnchorInputs(headTx, parsedBeef, make(map[string]bool)) {
			if ok, sErr := spv.Verify(ctx, headTx, tracker, nil); sErr == nil && ok {
				ancestryOK = true
			}
		}
		return VerifyResult{OK: false, Settled: false, AncestryVerified: ancestryOK, AnchorState: "broadcast", Reason: "Unsettled", Detail: "head anchor tx has no mined MerklePath"}
	}
	if tracker == nil {
		return VerifyResult{OK: false, Reason: "TrackerUnavailable", Detail: "no chain tracker configured"}
	}
	root, err := headTx.MerklePath.ComputeRoot(headTx.TxID())
	if err != nil {
		return VerifyResult{OK: false, Reason: "BEEFInvalid", Detail: err.Error()}
	}
	valid, err := tracker.IsValidRootForHeight(ctx, root, headTx.MerklePath.BlockHeight)
	if err != nil {
		return VerifyResult{OK: false, AnchorState: "broadcast", BlockHeight: headTx.MerklePath.BlockHeight, Reason: "TrackerUnavailable", Detail: err.Error()}
	}
	if !valid {
		return VerifyResult{OK: false, AnchorState: "broadcast", BlockHeight: headTx.MerklePath.BlockHeight, Reason: "BEEFInvalid", Detail: "BUMP root not valid in header chain"}
	}
	depth := 1
	if minDepth > 1 {
		tip, tErr := tracker.CurrentHeight(ctx)
		if tErr != nil {
			return VerifyResult{OK: false, AnchorState: "mined", BlockHeight: headTx.MerklePath.BlockHeight, Reason: "TrackerUnavailable", Detail: tErr.Error()}
		}
		if tip >= headTx.MerklePath.BlockHeight {
			depth = int(tip-headTx.MerklePath.BlockHeight) + 1
		}
		if depth < minDepth {
			return VerifyResult{OK: false, AnchorState: "mined", BlockHeight: headTx.MerklePath.BlockHeight, Depth: depth, Reason: "InsufficientDepth"}
		}
	}
	return VerifyResult{OK: true, Settled: true, AncestryVerified: true, AnchorState: "mined", BlockHeight: headTx.MerklePath.BlockHeight, Depth: depth}
}
