package core

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lightwebinc/bcommon/cbor"
)

// Modality qualifies a fragment's epistemic, deontic, or temporal scope.
type Modality struct {
	Kind      string `json:"kind"` // deontic | epistemic | temporal
	Qualifier string `json:"qualifier"`
}

// RefinementKind classifies a cryptographically committed refinement edge (K_i -> K_j) in the Knowledge DAG.
type RefinementKind string

const (
	RefinementRefines      RefinementKind = "refines"
	RefinementSupersedes   RefinementKind = "supersedes"
	RefinementQualifies    RefinementKind = "qualifies"
	RefinementContradicts  RefinementKind = "contradicts"
	RefinementCorroborates RefinementKind = "corroborates"
)

// ValidRefinementKind reports whether k is one of the canonical DAG refinement edge kinds.
func ValidRefinementKind(k RefinementKind) bool {
	switch k {
	case RefinementRefines, RefinementSupersedes, RefinementQualifies, RefinementContradicts, RefinementCorroborates:
		return true
	default:
		return false
	}
}

// RefinementEdge cryptographically binds a new fragment K_j to an existing committed fragment K_i
// via K_i's 32-byte HDP and Leaf digests, making Knowledge DAG lineage L1-immutable (Paper §7).
type RefinementEdge struct {
	TargetShard string         `json:"targetShard"`
	TargetSeq   int64          `json:"targetSeq,omitempty"`
	TargetHDP   string         `json:"targetHdp"`  // 32-byte lowercase hex H(d_i || p_i)
	TargetLeaf  string         `json:"targetLeaf"` // 32-byte lowercase hex Leaf_i
	Kind        RefinementKind `json:"kind"`
	Reason      string         `json:"reason,omitempty"`
}

// Validate checks canonical formatting of a RefinementEdge before signing/committing.
func (e RefinementEdge) Validate() error {
	if strings.TrimSpace(e.TargetShard) == "" {
		return errors.New("core: refinement edge missing targetShard")
	}
	if !ValidRefinementKind(e.Kind) {
		return fmt.Errorf("core: invalid refinement kind %q", e.Kind)
	}
	if h, err := hex.DecodeString(strings.TrimSpace(e.TargetHDP)); err != nil || len(h) != 32 {
		return fmt.Errorf("core: refinement edge targetHdp must be 32-byte hex")
	}
	if l, err := hex.DecodeString(strings.TrimSpace(e.TargetLeaf)); err != nil || len(l) != 32 {
		return fmt.Errorf("core: refinement edge targetLeaf must be 32-byte hex")
	}
	return nil
}

// Provenance binds source identity, timestamp, originator signature, model ID,
// optional modality, and cryptographically committed Knowledge DAG refinement edges into H(d|p).
type Provenance struct {
	Source    string           `json:"source"`
	Time      int64            `json:"time"` // unix seconds
	Signature []byte           `json:"signature"`
	ModelID   string           `json:"modelId"`
	Modality  *Modality        `json:"modality,omitempty"`
	Refines   []RefinementEdge `json:"refines,omitempty"`
}

// TransitionRecord encodes one deterministic step (q_i, sigma, q_j, t, h_i) of the extended
// Merkle Automaton A' = (Q, Sigma, delta, q_0, F, T) in canonical RFC 8949 CBOR (Paper §2–§3).
type TransitionRecord struct {
	Version      uint64 `json:"v"`
	Shard        string `json:"shard"`
	Seq          int64  `json:"seq"`
	PrevStateQ   []byte `json:"prevQ"`                  // 32B q_i
	InputDigest  []byte `json:"inputDigest"`            // 32B H(sigma)
	NextStateQ   []byte `json:"nextQ"`                  // 32B q_j
	CommitHash   []byte `json:"commitHash,omitempty"`   // 32B Git CommitObject hash
	TreeHash     []byte `json:"treeHash,omitempty"`     // 32B Git TreeObject hash
	PolicyHash   []byte `json:"policyHash,omitempty"`   // 32B active Cedar PolicyCommitment hash
	OntologyHash []byte `json:"ontologyHash,omitempty"` // 32B active __ontology__ head chain hash
	Timestamp    int64  `json:"t"`
	Hash         []byte `json:"h,omitempty"` // 32B DSHA256(TagTransitionV1 || CBOR(body))
}

func (tr TransitionRecord) cborBody() cbor.Map {
	v := tr.Version
	if v == 0 {
		v = 1
	}
	return cbor.Map{
		{Key: uint64(0), Val: v},
		{Key: uint64(1), Val: tr.Shard},
		{Key: uint64(2), Val: tr.Seq},
		{Key: uint64(3), Val: pad32(tr.PrevStateQ)},
		{Key: uint64(4), Val: pad32(tr.InputDigest)},
		{Key: uint64(5), Val: pad32(tr.NextStateQ)},
		{Key: uint64(6), Val: pad32(tr.CommitHash)},
		{Key: uint64(7), Val: pad32(tr.TreeHash)},
		{Key: uint64(8), Val: pad32(tr.PolicyHash)},
		{Key: uint64(9), Val: pad32(tr.OntologyHash)},
		{Key: uint64(10), Val: tr.Timestamp},
	}
}

// ComputeTransitionHash deterministically hashes tr using canonical RFC 8949 CBOR.
func ComputeTransitionHash(tr TransitionRecord) ([]byte, error) {
	return HashCBOR(TagTransitionV1, tr.cborBody())
}

// NextStepStateQ computes the deterministic per-transition automaton state q_j from q_i and inputs.
func NextStepStateQ(prevQ, inputDigest, commitHash, treeHash, policyHash, ontologyHash []byte, timestamp int64) ([]byte, error) {
	return HashCBOR(TagStateQ, cbor.Map{
		{Key: uint64(0), Val: pad32(prevQ)},
		{Key: uint64(1), Val: pad32(inputDigest)},
		{Key: uint64(2), Val: pad32(commitHash)},
		{Key: uint64(3), Val: pad32(treeHash)},
		{Key: uint64(4), Val: pad32(policyHash)},
		{Key: uint64(5), Val: pad32(ontologyHash)},
		{Key: uint64(6), Val: timestamp},
	})
}

// EncodeTransitionCBOR serializes tr (including its Hash) into canonical RFC 8949 CBOR.
func EncodeTransitionCBOR(tr TransitionRecord) ([]byte, error) {
	h := tr.Hash
	if len(h) != 32 {
		var err error
		h, err = ComputeTransitionHash(tr)
		if err != nil {
			return nil, err
		}
	}
	m := append(tr.cborBody(), cbor.Pair{Key: uint64(11), Val: h})
	return EncodeCBOR(m)
}

var transitionKnownKeys = KnownKeyRange(0, 11)

// DecodeTransitionCBOR strictly decodes and verifies a canonical CBOR TransitionRecord,
// enforcing Nektar's critical (even) / non-critical (odd) unknown-key evolution rule.
func DecodeTransitionCBOR(raw []byte) (TransitionRecord, error) {
	v, err := DecodeCBOR(raw)
	if err != nil {
		return TransitionRecord{}, fmt.Errorf("transition: decode cbor: %w", err)
	}
	m, _, err := SplitCBORMap(v, transitionKnownKeys)
	if err != nil {
		return TransitionRecord{}, fmt.Errorf("transition: %w", err)
	}
	var tr TransitionRecord
	if tr.Version, err = cborUint(m, 0); err != nil {
		return TransitionRecord{}, err
	}
	if tr.Shard, err = cborText(m, 1); err != nil {
		return TransitionRecord{}, err
	}
	if tr.Seq, err = cborInt(m, 2); err != nil {
		return TransitionRecord{}, err
	}
	if tr.PrevStateQ, err = cborBytes(m, 3); err != nil {
		return TransitionRecord{}, err
	}
	if tr.InputDigest, err = cborBytes(m, 4); err != nil {
		return TransitionRecord{}, err
	}
	if tr.NextStateQ, err = cborBytes(m, 5); err != nil {
		return TransitionRecord{}, err
	}
	if tr.CommitHash, err = cborBytes(m, 6); err != nil {
		return TransitionRecord{}, err
	}
	if tr.TreeHash, err = cborBytes(m, 7); err != nil {
		return TransitionRecord{}, err
	}
	if tr.PolicyHash, err = cborBytes(m, 8); err != nil {
		return TransitionRecord{}, err
	}
	if tr.OntologyHash, err = cborBytes(m, 9); err != nil {
		return TransitionRecord{}, err
	}
	if tr.Timestamp, err = cborInt(m, 10); err != nil {
		return TransitionRecord{}, err
	}
	if tr.Hash, err = cborBytes(m, 11); err != nil {
		return TransitionRecord{}, err
	}
	want, err := ComputeTransitionHash(tr)
	if err != nil {
		return TransitionRecord{}, err
	}
	if !EqualBytes(want, tr.Hash) {
		return TransitionRecord{}, errors.New("transition: hash mismatch")
	}
	return tr, nil
}

// VerifyTransitionChain replays an ordered slice of TransitionRecords starting from baseQ
// (Zero32Bytes() when baseQ is empty), verifying each record's CBOR commitment hash, PrevStateQ
// continuity, and deterministic NextStepStateQ transition (Paper §2-§3 Merkle Automaton).
func VerifyTransitionChain(baseQ []byte, records []TransitionRecord) ([]byte, error) {
	curQ := pad32(baseQ)
	for i, tr := range records {
		if len(tr.PrevStateQ) != 32 || !EqualBytes(tr.PrevStateQ, curQ) {
			return nil, fmt.Errorf("transition[%d] seq=%d: prevStateQ link broken", i, tr.Seq)
		}
		wantNextQ, err := NextStepStateQ(tr.PrevStateQ, tr.InputDigest, tr.CommitHash, tr.TreeHash, tr.PolicyHash, tr.OntologyHash, tr.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("transition[%d] seq=%d: compute nextQ: %w", i, tr.Seq, err)
		}
		if len(tr.NextStateQ) != 32 || !EqualBytes(tr.NextStateQ, wantNextQ) {
			return nil, fmt.Errorf("transition[%d] seq=%d: nextStateQ mismatch", i, tr.Seq)
		}
		wantHash, err := ComputeTransitionHash(tr)
		if err != nil {
			return nil, fmt.Errorf("transition[%d] seq=%d: compute hash: %w", i, tr.Seq, err)
		}
		if len(tr.Hash) != 32 || !EqualBytes(tr.Hash, wantHash) {
			return nil, fmt.Errorf("transition[%d] seq=%d: hash mismatch", i, tr.Seq)
		}
		curQ = wantNextQ
	}
	return curQ, nil
}

// Fragment is the canonical unit of Nektar verifiable memory: (d, p, H(d|p), E(d)),
// optionally enriched with a tree path, volumetric CHIRP content commitment, L1-bound MPE root,
// and v3 bidirectional Git CommitHash + Automaton TransitionHash commitments.
type Fragment struct {
	Data           []byte     `json:"d"`
	Provenance     Provenance `json:"p"`
	HDP            []byte     `json:"hdp,omitempty"`
	Ciphertext     []byte     `json:"E,omitempty"`
	Path           string     `json:"path,omitempty"`
	MediaType      string     `json:"mediaType,omitempty"`
	ChirpURL       string     `json:"chirpUrl,omitempty"`
	ContentSHA256  []byte     `json:"contentSha256,omitempty"`
	MPERoot        []byte     `json:"mpeRoot,omitempty"`
	CommitHash     []byte     `json:"commitHash,omitempty"`     // 32B Git CommitObject hash bound into LeafV3
	TransitionHash []byte     `json:"transitionHash,omitempty"` // 32B Automaton TransitionRecord hash bound into LeafV3
}

// Field prefixes b with its uint32 little-endian length.
func Field(b []byte) []byte {
	return Concat(U32LE(uint32(len(b))), b)
}

func takeField(b []byte, off int) ([]byte, int, error) {
	if off < 0 || uint64(off)+4 > uint64(len(b)) {
		return nil, 0, errors.New("parse: truncated field header")
	}
	u := binary.LittleEndian.Uint32(b[off : off+4])
	if uint64(off)+4+uint64(u) > uint64(len(b)) {
		return nil, 0, errors.New("parse: truncated field body")
	}
	n := int(u)
	v := make([]byte, n)
	copy(v, b[off+4:off+4+n])
	return v, off + 4 + n, nil
}

func encodeRefinesCBOR(edges []RefinementEdge) ([]byte, error) {
	arr := make([]cbor.Value, len(edges))
	for i, e := range edges {
		arr[i] = cbor.Map{
			{Key: uint64(0), Val: e.TargetShard},
			{Key: uint64(1), Val: e.TargetSeq},
			{Key: uint64(2), Val: strings.ToLower(strings.TrimSpace(e.TargetHDP))},
			{Key: uint64(3), Val: strings.ToLower(strings.TrimSpace(e.TargetLeaf))},
			{Key: uint64(4), Val: string(e.Kind)},
			{Key: uint64(5), Val: e.Reason},
		}
	}
	return EncodeCBOR(arr)
}

func decodeRefinesCBOR(raw []byte) ([]RefinementEdge, error) {
	v, err := DecodeCBOR(raw)
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]cbor.Value)
	if !ok {
		return nil, errors.New("parse: refines expected CBOR array")
	}
	out := make([]RefinementEdge, len(arr))
	for i, item := range arr {
		m, err := CBORMapToUintMap(item)
		if err != nil {
			return nil, err
		}
		var e RefinementEdge
		if e.TargetShard, err = cborText(m, 0); err != nil {
			return nil, err
		}
		if e.TargetSeq, err = cborInt(m, 1); err != nil {
			return nil, err
		}
		if e.TargetHDP, err = cborText(m, 2); err != nil {
			return nil, err
		}
		if e.TargetLeaf, err = cborText(m, 3); err != nil {
			return nil, err
		}
		kindStr, err := cborText(m, 4)
		if err != nil {
			return nil, err
		}
		e.Kind = RefinementKind(kindStr)
		if e.Reason, err = cborText(m, 5); err != nil {
			return nil, err
		}
		out[i] = e
	}
	return out, nil
}

// SerializeProvenance produces the exact canonical binary encoding of p.
// When Refines is empty, output is byte-identical to v0.1; when Refines is present,
// an explicit modality slot (empty if nil) is followed by canonical CBOR RefinementEdges.
func SerializeProvenance(p Provenance) []byte {
	parts := [][]byte{
		Field([]byte(p.Source)),
		U64LE(uint64(p.Time)),
		Field(p.Signature),
		Field([]byte(p.ModelID)),
	}
	if p.Modality != nil || len(p.Refines) > 0 {
		var mb []byte
		if p.Modality != nil {
			mb, _ = CanonicalJSON(p.Modality)
		}
		parts = append(parts, Field(mb))
	}
	if len(p.Refines) > 0 {
		rb, _ := encodeRefinesCBOR(p.Refines)
		parts = append(parts, Field(rb))
	}
	return Concat(parts...)
}

// ParseProvenance decodes SerializeProvenance bytes.
func ParseProvenance(b []byte) (Provenance, error) {
	src, next, err := takeField(b, 0)
	if err != nil {
		return Provenance{}, err
	}
	if next+8 > len(b) {
		return Provenance{}, errors.New("parse: truncated time")
	}
	t := int64(binary.LittleEndian.Uint64(b[next : next+8]))
	sig, next, err := takeField(b, next+8)
	if err != nil {
		return Provenance{}, err
	}
	mid, next, err := takeField(b, next)
	if err != nil {
		return Provenance{}, err
	}
	p := Provenance{
		Source:    string(src),
		Time:      t,
		Signature: sig,
		ModelID:   string(mid),
	}
	if next < len(b) {
		mb, afterMod, err := takeField(b, next)
		if err != nil {
			return Provenance{}, err
		}
		if len(mb) > 0 {
			var m Modality
			if err := json.Unmarshal(mb, &m); err != nil {
				return Provenance{}, fmt.Errorf("parse: modality: %w", err)
			}
			p.Modality = &m
		}
		next = afterMod
	}
	if next < len(b) {
		rb, end, err := takeField(b, next)
		if err != nil {
			return Provenance{}, err
		}
		if end != len(b) {
			return Provenance{}, errors.New("parse: trailing bytes in provenance")
		}
		edges, err := decodeRefinesCBOR(rb)
		if err != nil {
			return Provenance{}, fmt.Errorf("parse: refines: %w", err)
		}
		p.Refines = edges
	}
	return p, nil
}

// ProvenanceDigest is the COM-003 message signed by the originator:
// DSHA256(TagProvSig || Field(d) || SerializeProvenance(p without sig)).
func ProvenanceDigest(data []byte, p Provenance) []byte {
	unsigned := p
	unsigned.Signature = nil
	return DSHA256(Concat(TagProvSig, Field(data), SerializeProvenance(unsigned)))
}

// ComputeHDPV2 computes framed v2 H(d|p) = SHA256(TagProvV2 || Field(d) || SerializeProvenance(p))
// eliminating boundary ambiguity between variable-length plaintext data and serialized provenance (F-10).
func ComputeHDPV2(data []byte, p Provenance) []byte {
	return SHA256(Concat(TagProvV2, Field(data), SerializeProvenance(p)))
}

// VerifyHDP checks whether hdp matches canonical length-framed v2 H(d|p) over raw provBytes.
func VerifyHDP(data, provBytes, hdp []byte) bool {
	if len(hdp) != 32 {
		return false
	}
	return EqualBytes(SHA256(Concat(TagProvV2, Field(data), provBytes)), hdp)
}

// ComputeEH computes SHA256(E) (SHA256("") if empty).
func ComputeEH(ciphertext []byte) []byte {
	return SHA256(ciphertext)
}

// ComputeLeaf computes the sole canonical L1-bound Merkle Automaton leaf committing to:
// H(d|p) (including Refines & Modality), H(E), MPERoot, ContentSHA256, Git CommitHash, and TransitionHash.
// Collapses the split-brain between linear fragments, Git Memory Repo DAG, and Automaton transitions.
func ComputeLeaf(f Fragment) []byte {
	hdp := f.HDP
	if len(hdp) == 0 {
		hdp = ComputeHDPV2(f.Data, f.Provenance)
	}
	return SHA256(Concat(
		TagLeaf,
		pad32(hdp),
		ComputeEH(f.Ciphertext),
		pad32(f.MPERoot),
		pad32(f.ContentSHA256),
		pad32(f.CommitHash),
		pad32(f.TransitionHash),
	))
}
