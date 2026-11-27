package contracts

import (
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/bsv-blockchain/go-sdk/script"

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
	artifactMu.Lock()
	artifactCache[name] = &a
	artifactMu.Unlock()
	return &a, nil
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
		"DisclosureSale",
		"NektarAgentVault",
		"NektarCheckpointBond",
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

// DecodeNektarAnchor decodes and validates a canonical L1-spendable META NektarAnchor locking script.
// Non-spendable legacy artifacts (NektarAnchorV3, unpatched v4), non-Metanet scripts, and legacy
// TagAnchorTag payloads are strictly rejected so UTXO spend lineage is the sole anchor format.
func DecodeNektarAnchor(scriptBytes []byte) (*AnchorState, error) {
	codeV4Patched, err := loadPatchedAnchorCode()
	if err != nil {
		return nil, err
	}
	if len(scriptBytes) <= len(codeV4Patched) || !core.EqualBytes(scriptBytes[:len(codeV4Patched)], codeV4Patched) || scriptBytes[len(codeV4Patched)] != 0x6a {
		return nil, errors.New("contracts: locking script is not a spendable META NektarAnchor covenant")
	}
	suffix := scriptBytes[len(codeV4Patched)+1:]

	if len(suffix) < 5 || suffix[0] != 0x04 || !core.EqualBytes(suffix[1:5], core.MetaFlag) {
		return nil, errors.New("contracts: NektarAnchor requires canonical META flag prefix")
	}
	suffix = suffix[5:]
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
	if pkhLen != 20 || pkhOff+1+pkhLen != len(suffix) {
		return nil, errors.New("contracts: invalid ownerPkh length (must be 20 bytes)")
	}
	pkh := append([]byte(nil), suffix[pkhOff+1:]...)
	if !core.EqualBytes(tag, core.TagRPrime) {
		return nil, errors.New("contracts: unexpected anchor domain tag (TagRPrime required)")
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
		Metanet:  true,
	}, nil
}

// AnchorLink summarizes one anchor UTXO in a repo's consensus-linearized singleton chain.
type AnchorLink struct {
	TxID        string `json:"txid"`
	OutputIndex uint32 `json:"outputIndex"`
	N           uint64 `json:"n"`
	Size        uint64 `json:"size"`
	R           string `json:"R"`
	Rp          string `json:"Rp"`
	AnchorBEEF  []byte `json:"anchorBeef,omitempty"`
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
