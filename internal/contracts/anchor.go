package contracts

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/core"
)

//go:embed artifacts/*.json
var artifactsFS embed.FS

// Artifact represents a compiled Rúnar Bitcoin Script covenant artifact.
type Artifact struct {
	Version         string `json:"version"`
	CompilerVersion string `json:"compilerVersion"`
	ContractName    string `json:"contractName"`
	Script          string `json:"script"`
}

// CovenantID uniquely identifies a compiled covenant artifact on the wire.
type CovenantID struct {
	ArtifactID string `json:"artifactId"`
	ScriptHash string `json:"scriptHash"`
}

var (
	artifactMu        sync.RWMutex
	artifactCache     = make(map[string]*Artifact)
	codeCache         = make(map[string][]byte)
	patchedAnchorCode []byte
)

func loadPatchedAnchorCode() ([]byte, error) {
	artifactMu.RLock()
	if len(patchedAnchorCode) > 0 {
		c := patchedAnchorCode
		artifactMu.RUnlock()
		return c, nil
	}
	artifactMu.RUnlock()

	art, err := LoadFullArtifact("NektarAnchor")
	if err != nil {
		return nil, err
	}
	code, err := BuildCodePart(art, nil)
	if err != nil {
		return nil, fmt.Errorf("contracts: patch NektarAnchor codeSepIndexSlots: %w", err)
	}
	artifactMu.Lock()
	patchedAnchorCode = code
	artifactMu.Unlock()
	return code, nil
}

// IsSpendableNektarAnchorScript reports whether scriptBytes carries the codeSepIndex-patched
// NektarAnchor v4 code prefix that can be spent on-chain via Append() / Rotate().
func IsSpendableNektarAnchorScript(scriptBytes []byte) bool {
	patched, err := loadPatchedAnchorCode()
	if err != nil || len(patched) == 0 {
		return false
	}
	return len(scriptBytes) > len(patched) && core.EqualBytes(scriptBytes[:len(patched)], patched) && scriptBytes[len(patched)] == 0x6a
}

// LoadArtifact reads and caches an embedded Rúnar JSON artifact by contract name (e.g. "NektarAnchor").
func LoadArtifact(name string) (*Artifact, error) {
	artifactMu.RLock()
	if a, ok := artifactCache[name]; ok {
		artifactMu.RUnlock()
		return a, nil
	}
	artifactMu.RUnlock()

	raw, err := artifactsFS.ReadFile("artifacts/" + name + ".json")
	if err != nil {
		return nil, fmt.Errorf("contracts: load %s: %w", name, err)
	}
	var a Artifact
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("contracts: parse %s: %w", name, err)
	}
	code, _ := hex.DecodeString(strings.TrimPrefix(a.Script, "0x"))
	artifactMu.Lock()
	artifactCache[name] = &a
	codeCache[name] = code
	artifactMu.Unlock()
	return &a, nil
}

func loadArtifactCode(name string) ([]byte, error) {
	artifactMu.RLock()
	if c, ok := codeCache[name]; ok {
		artifactMu.RUnlock()
		return c, nil
	}
	artifactMu.RUnlock()
	if _, err := LoadArtifact(name); err != nil {
		return nil, err
	}
	artifactMu.RLock()
	c := codeCache[name]
	artifactMu.RUnlock()
	return c, nil
}

// ID returns the canonical wire CovenantID for an artifact.
func (a *Artifact) ID() CovenantID {
	h := sha256.Sum256([]byte(a.Script))
	return CovenantID{
		ArtifactID: a.ContractName + "@" + a.Version,
		ScriptHash: hex.EncodeToString(h[:]),
	}
}

// ListArtifacts returns all embedded production covenant names.
func ListArtifacts() []string {
	return []string{
		"NektarAnchor",
		"NektarAnchorV3",
		"NektarAutomaton",
		"DisclosureSale",
		"NektarFeeVault",
		"NektarAgentVault",
		"NektarCheckpointBond",
		"NektarZkClaim",
		"NektarGrothClaim",
		"ClaimVerdict",
	}
}

// AnchorState is the on-chain state committed in a NektarAnchor locking script.
type AnchorState struct {
	Shard    string `json:"shard"`
	OwnerPub string `json:"ownerPub"`
	OwnerPKH []byte `json:"ownerPkh"`
	N        uint64 `json:"n"`
	Size     uint64 `json:"size"`
	R        []byte `json:"R"`
	Rp       []byte `json:"Rp"`
	Tag      []byte `json:"tag"`
	Metanet  bool   `json:"metanet"`
}

// EncodeAnchorLockingScript builds the L1-spendable NektarAnchor locking script:
// <patched NektarAnchor code> OP_RETURN [0x04 "META"] <genesisOwner33> <n_le8> <size_le8> <R32> <Rp32> <len(tag)><tag> <len(pkh)><pkh>
func EncodeAnchorLockingScript(st AnchorState) (*script.Script, error) {
	codeBytes, err := loadPatchedAnchorCode()
	if err != nil {
		return nil, err
	}
	ownerPubBytes, err := hex.DecodeString(st.OwnerPub)
	if err != nil || len(ownerPubBytes) != 33 {
		return nil, errors.New("contracts: ownerPub must be 33-byte compressed hex")
	}
	if len(st.R) != 32 || len(st.Rp) != 32 || len(st.OwnerPKH) != 20 {
		return nil, errors.New("contracts: invalid R/Rp/ownerPkh lengths")
	}
	tag := st.Tag
	if len(tag) == 0 {
		tag = core.TagRPrime
	}
	if len(tag) > 75 {
		return nil, errors.New("contracts: anchor tag exceeds 75-byte direct push limit")
	}
	state := make([]byte, 0, 5+33+8+8+32+32+1+len(tag)+1+20)
	if st.Metanet {
		state = append(state, 0x04)
		state = append(state, core.MetaFlag...)
	} else {
		// Explicit empty push so NektarAnchor v4's OP_1 OP_SPLIT metaFlag parser stays aligned.
		state = append(state, 0x00)
	}
	state = append(state, ownerPubBytes...)
	state = append(state, core.U64LE(st.N)...)
	state = append(state, core.U64LE(st.Size)...)
	state = append(state, st.R...)
	state = append(state, st.Rp...)
	state = append(state, byte(len(tag)))
	state = append(state, tag...)
	state = append(state, byte(len(st.OwnerPKH)))
	state = append(state, st.OwnerPKH...)

	full := make([]byte, 0, len(codeBytes)+1+len(state))
	full = append(full, codeBytes...)
	full = append(full, 0x6a) // OP_RETURN state separator
	full = append(full, state...)
	s := script.Script(full)
	return &s, nil
}

// DecodeNektarAnchor decodes and validates a NektarAnchor locking script's OP_RETURN state suffix
// and verifies the code prefix matches the patched L1-spendable NektarAnchor, legacy unpatched
// NektarAnchor, or legacy NektarAnchorV3 artifact.
func DecodeNektarAnchor(scriptBytes []byte) (*AnchorState, error) {
	codeV4Patched, err := loadPatchedAnchorCode()
	if err != nil {
		return nil, err
	}
	codeV4Raw, err := loadArtifactCode("NektarAnchor")
	if err != nil {
		return nil, err
	}
	codeV3, _ := loadArtifactCode("NektarAnchorV3")

	var suffix []byte
	switch {
	case len(scriptBytes) > len(codeV4Patched) && core.EqualBytes(scriptBytes[:len(codeV4Patched)], codeV4Patched) && scriptBytes[len(codeV4Patched)] == 0x6a:
		suffix = scriptBytes[len(codeV4Patched)+1:]
	case len(scriptBytes) > len(codeV4Raw) && core.EqualBytes(scriptBytes[:len(codeV4Raw)], codeV4Raw) && scriptBytes[len(codeV4Raw)] == 0x6a:
		suffix = scriptBytes[len(codeV4Raw)+1:]
	case len(codeV3) > 0 && len(scriptBytes) > len(codeV3) && core.EqualBytes(scriptBytes[:len(codeV3)], codeV3) && scriptBytes[len(codeV3)] == 0x6a:
		suffix = scriptBytes[len(codeV3)+1:]
	default:
		return nil, errors.New("contracts: locking script code does not match NektarAnchor artifact")
	}

	metanet := false
	if len(suffix) >= 5 && suffix[0] == 0x04 && core.EqualBytes(suffix[1:5], core.MetaFlag) {
		metanet = true
		suffix = suffix[5:]
	} else if len(suffix) >= 1 && suffix[0] == 0x00 {
		suffix = suffix[1:]
	}
	// Layout: ownerPub(33) | n(8) | size(8) | R(32) | Rp(32) | tagLen(1) | tag | pkhLen(1) | pkh
	if len(suffix) < 115 {
		return nil, errors.New("contracts: anchor state suffix too short")
	}
	ownerPub := suffix[0:33]
	if ownerPub[0] != 0x02 && ownerPub[0] != 0x03 {
		return nil, errors.New("contracts: invalid compressed ownerPub")
	}
	n := binary.LittleEndian.Uint64(suffix[33:41])
	size := binary.LittleEndian.Uint64(suffix[41:49])
	r := append([]byte(nil), suffix[49:81]...)
	rp := append([]byte(nil), suffix[81:113]...)
	tagLen := int(suffix[113])
	if 114+tagLen+1 > len(suffix) {
		return nil, errors.New("contracts: truncated anchor tag")
	}
	tag := append([]byte(nil), suffix[114:114+tagLen]...)
	pkhOff := 114 + tagLen
	pkhLen := int(suffix[pkhOff])
	if pkhOff+1+pkhLen != len(suffix) {
		return nil, errors.New("contracts: trailing or truncated ownerPkh")
	}
	pkh := append([]byte(nil), suffix[pkhOff+1:]...)
	if !core.EqualBytes(tag, core.TagAnchorTag) && !core.EqualBytes(tag, core.TagRPrime) {
		return nil, errors.New("contracts: unexpected anchor domain tag")
	}

	return &AnchorState{
		Shard:    core.ShardIDFromPubkey(ownerPub),
		OwnerPub: hex.EncodeToString(ownerPub),
		OwnerPKH: pkh,
		N:        n,
		Size:     size,
		R:        r,
		Rp:       rp,
		Tag:      tag,
		Metanet:  metanet,
	}, nil
}

// AnchorLink summarizes one anchor in a shard's chained history.
type AnchorLink struct {
	TxID        string `json:"txid"`
	OutputIndex uint32 `json:"outputIndex"`
	N           uint64 `json:"n"`
	Size        uint64 `json:"size"`
	R           string `json:"R"`
	Rp          string `json:"Rp"`
}

// ProofBundle is the self-contained SPV proof for one memory fragment:
// leaf -> batch Merkle path -> NektarAnchor state -> Atomic BEEF + BUMP -> block headers.
type ProofBundle struct {
	Version    int          `json:"version"`
	Shard      string       `json:"shard"`
	N          uint64       `json:"n"`
	Size       uint64       `json:"size"`
	Leaf       string       `json:"leaf"`
	LeafIndex  int          `json:"leafIndex"`
	Path       []string     `json:"path"`
	Anchor     AnchorLink   `json:"anchor"`
	ChainTail  []AnchorLink `json:"chainTail"`
	AnchorBEEF []byte       `json:"anchorBEEF"`
}

// VerifyResult reports the honest verification outcome for a ProofBundle.
type VerifyResult struct {
	OK               bool   `json:"ok"`
	Settled          bool   `json:"settled"`
	AncestryVerified bool   `json:"ancestryVerified,omitempty"` // true when unmined carried BEEF ancestry passed spv.Verify
	AnchorState      string `json:"anchorState,omitempty"`      // broadcast | mined
	BlockHeight      uint32 `json:"blockHeight,omitempty"`
	Depth            int    `json:"depth,omitempty"`
	Reason           string `json:"reason,omitempty"`
	Detail           string `json:"detail,omitempty"`
}

// AnchorBatchMemo caches the verified anchor-tx, ChainTail, and BUMP header result for one (shard, batchN)
// so Recall(VerifySPV:true) and VerifyReceipt avoid re-parsing AnchorBEEF and re-querying headers per fragment.
type AnchorBatchMemo struct {
	State      *AnchorState
	Result     VerifyResult
	AnchorTxID string
}

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
	if memo != nil && memo.State != nil {
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

// VerifyFirstPartyAnchorAncestry checks structural commitment, ChainTail continuity, AND carried
// input ancestry via go-sdk/spv.Verify for a first-party unmined anchor without weakening VerifyFragmentCtx
// (which always keeps OK:false, Reason:"Unsettled" on unmined anchors).
func VerifyFirstPartyAnchorAncestry(ctx context.Context, bundle ProofBundle, tracker chaintracker.ChainTracker) VerifyResult {
	return verifyFragment(ctx, bundle, tracker, true, 1, nil)
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
	for i, link := range bundle.ChainTail {
		if link.N != uint64(i) {
			return VerifyResult{OK: false, Reason: "ChainInconsistent", Detail: fmt.Sprintf("gap at n=%d", i)}
		}
		rBytes, err1 := hex.DecodeString(link.R)
		rpBytes, err2 := hex.DecodeString(link.Rp)
		if err1 != nil || err2 != nil || len(rBytes) != 32 || len(rpBytes) != 32 {
			return VerifyResult{OK: false, Reason: "ChainInconsistent", Detail: "invalid R/Rp hex in chainTail"}
		}
		matchesTag := core.EqualBytes(rpBytes, core.ChainRpWithTag(state.Tag, prevRp, rBytes, link.Size))
		// Legacy compatibility: anchors minted before TagRPrime unification stored TagAnchorTag on-chain
		// while computing R' with TagRPrime.
		matchesLegacy := core.EqualBytes(state.Tag, core.TagAnchorTag) && core.EqualBytes(rpBytes, core.ChainRp(prevRp, rBytes, link.Size))
		if !matchesTag && !matchesLegacy {
			return VerifyResult{OK: false, Reason: "ChainInconsistent", Detail: fmt.Sprintf("R' link broken at n=%d", i)}
		}
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
