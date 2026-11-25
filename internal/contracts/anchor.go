package contracts

import (
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
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

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
	artifactMu    sync.RWMutex
	artifactCache = make(map[string]*Artifact)
	codeCache     = make(map[string][]byte)
)

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

// EncodeAnchorLockingScript builds the NektarAnchor locking script:
// <compiled NektarAnchor code> OP_RETURN [0x04 "META"] <genesisOwner33> <n_le8> <size_le8> <R32> <Rp32> <len(tag)><tag> <len(pkh)><pkh>
func EncodeAnchorLockingScript(st AnchorState) (*script.Script, error) {
	codeBytes, err := loadArtifactCode("NektarAnchor")
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
		tag = core.TagAnchorTag
	}
	state := make([]byte, 0, 4+33+8+8+32+32+1+len(tag)+1+20)
	if st.Metanet {
		state = append(state, 0x04)
		state = append(state, core.MetaFlag...)
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
// and verifies the code prefix matches the embedded NektarAnchor or NektarAnchorV3 artifact.
func DecodeNektarAnchor(scriptBytes []byte) (*AnchorState, error) {
	codeV4, err := loadArtifactCode("NektarAnchor")
	if err != nil {
		return nil, err
	}
	codeV3, _ := loadArtifactCode("NektarAnchorV3")

	var suffix []byte
	switch {
	case len(scriptBytes) > len(codeV4) && core.EqualBytes(scriptBytes[:len(codeV4)], codeV4) && scriptBytes[len(codeV4)] == 0x6a:
		suffix = scriptBytes[len(codeV4)+1:]
	case len(codeV3) > 0 && len(scriptBytes) > len(codeV3) && core.EqualBytes(scriptBytes[:len(codeV3)], codeV3) && scriptBytes[len(codeV3)] == 0x6a:
		suffix = scriptBytes[len(codeV3)+1:]
	default:
		return nil, errors.New("contracts: locking script code does not match NektarAnchor artifact")
	}

	metanet := false
	if len(suffix) >= 5 && suffix[0] == 0x04 && core.EqualBytes(suffix[1:5], core.MetaFlag) {
		metanet = true
		suffix = suffix[5:]
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
	OK          bool   `json:"ok"`
	Settled     bool   `json:"settled"`
	AnchorState string `json:"anchorState,omitempty"` // broadcast | mined
	Reason      string `json:"reason,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// VerifyFragment performs full offline SPV verification of a ProofBundle using only block headers.
func VerifyFragment(bundle ProofBundle, tracker chaintracker.ChainTracker) VerifyResult {
	if bundle.LeafIndex < 0 || uint64(bundle.LeafIndex) >= bundle.Size {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "leafIndex outside batch size"}
	}
	if len(bundle.Path) != core.HeightForLeafCount(int(bundle.Size)) {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "path height does not match committed batch size"}
	}
	leaf, err := hex.DecodeString(bundle.Leaf)
	if err != nil {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "invalid leaf hex"}
	}
	sibs := make([][]byte, len(bundle.Path))
	for i, s := range bundle.Path {
		b, err := hex.DecodeString(s)
		if err != nil {
			return VerifyResult{OK: false, Reason: "BadBundle", Detail: "invalid path sibling hex"}
		}
		sibs[i] = b
	}
	recomputedR := core.ReconstructRoot(leaf, &core.MerkleProof{Index: bundle.LeafIndex, Siblings: sibs})

	anchorTx, err := transaction.NewTransactionFromBEEF(bundle.AnchorBEEF)
	if err != nil {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "BEEF parse: " + err.Error()}
	}
	if len(anchorTx.Outputs) == 0 || anchorTx.Outputs[0].LockingScript == nil {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: "anchor tx has no output 0"}
	}
	state, err := DecodeNektarAnchor(*anchorTx.Outputs[0].LockingScript)
	if err != nil {
		return VerifyResult{OK: false, Reason: "BadBundle", Detail: err.Error()}
	}
	if state.Shard != bundle.Shard || state.N != bundle.N || state.Size != bundle.Size {
		return VerifyResult{OK: false, Reason: "AnchorFieldMismatch", Detail: "shard/n/size mismatch"}
	}
	if !core.EqualBytes(state.R, recomputedR) {
		return VerifyResult{OK: false, Reason: "LeafPathMismatch", Detail: "recomputed R_n != anchor R_n"}
	}

	settled := false
	anchorState := "broadcast"
	if anchorTx.MerklePath != nil {
		if tracker == nil {
			return VerifyResult{OK: false, Reason: "BEEFInvalid", Detail: "no chain tracker configured to verify BUMP"}
		}
		txidHash := anchorTx.TxID()
		root, err := anchorTx.MerklePath.ComputeRoot(txidHash)
		if err != nil {
			return VerifyResult{OK: false, Reason: "BEEFInvalid", Detail: "merkle path compute root: " + err.Error()}
		}
		valid, err := tracker.IsValidRootForHeight(nil, root, anchorTx.MerklePath.BlockHeight)
		if err != nil || !valid {
			return VerifyResult{OK: false, Reason: "BEEFInvalid", Detail: "BUMP root not valid in header chain"}
		}
		settled = true
		anchorState = "mined"
	}

	if len(bundle.ChainTail) == 0 {
		return VerifyResult{OK: false, Reason: "ChainInconsistent", Detail: "empty chainTail"}
	}
	last := bundle.ChainTail[len(bundle.ChainTail)-1]
	txidStr := anchorTx.TxID().String()
	if last.TxID != bundle.Anchor.TxID || last.TxID != txidStr || last.N != state.N || last.Size != state.Size ||
		!strings.EqualFold(last.R, hex.EncodeToString(state.R)) || !strings.EqualFold(last.Rp, hex.EncodeToString(state.Rp)) {
		return VerifyResult{OK: false, Reason: "ChainInconsistent", Detail: "chainTail head mismatches on-chain anchor"}
	}
	prevRp := core.Zero32
	for i, link := range bundle.ChainTail {
		if link.N != uint64(i) {
			return VerifyResult{OK: false, Reason: "ChainInconsistent", Detail: fmt.Sprintf("gap at n=%d", i)}
		}
		rBytes, err1 := hex.DecodeString(link.R)
		rpBytes, err2 := hex.DecodeString(link.Rp)
		if err1 != nil || err2 != nil {
			return VerifyResult{OK: false, Reason: "ChainInconsistent", Detail: "invalid R/Rp hex in chainTail"}
		}
		if !core.EqualBytes(rpBytes, core.ChainRp(prevRp, rBytes, link.Size)) {
			return VerifyResult{OK: false, Reason: "ChainInconsistent", Detail: fmt.Sprintf("R' link broken at n=%d", i)}
		}
		prevRp = rpBytes
	}

	_ = chainhash.Hash{}
	return VerifyResult{OK: true, Settled: settled, AnchorState: anchorState}
}
