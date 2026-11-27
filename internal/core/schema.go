package core

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

// Modality qualifies a fragment's epistemic, deontic, or temporal scope.
type Modality struct {
	Kind      string `json:"kind"` // deontic | epistemic | temporal
	Qualifier string `json:"qualifier"`
}

// Provenance binds source identity, timestamp, originator signature, model ID,
// and optional modality into a fragment's commitment hash.
type Provenance struct {
	Source    string    `json:"source"`
	Time      int64     `json:"time"` // unix seconds
	Signature []byte    `json:"signature"`
	ModelID   string    `json:"modelId"`
	Modality  *Modality `json:"modality,omitempty"`
}

// Fragment is the canonical unit of Nektar verifiable memory: (d, p, H(d|p), E(d)),
// optionally enriched with a tree path, volumetric CHIRP content commitment, and L1-bound MPE root.
type Fragment struct {
	Data          []byte     `json:"d"`
	Provenance    Provenance `json:"p"`
	HDP           []byte     `json:"hdp,omitempty"`
	Ciphertext    []byte     `json:"E,omitempty"`
	Path          string     `json:"path,omitempty"`
	MediaType     string     `json:"mediaType,omitempty"`
	ChirpURL      string     `json:"chirpUrl,omitempty"`
	ContentSHA256 []byte     `json:"contentSha256,omitempty"`
	MPERoot       []byte     `json:"mpeRoot,omitempty"`
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

// SerializeProvenance produces the exact canonical binary encoding of p.
func SerializeProvenance(p Provenance) []byte {
	parts := [][]byte{
		Field([]byte(p.Source)),
		U64LE(uint64(p.Time)),
		Field(p.Signature),
		Field([]byte(p.ModelID)),
	}
	if p.Modality != nil {
		mb, _ := CanonicalJSON(p.Modality)
		parts = append(parts, Field(mb))
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
		mb, end, err := takeField(b, next)
		if err != nil {
			return Provenance{}, err
		}
		if end != len(b) {
			return Provenance{}, errors.New("parse: trailing bytes in provenance")
		}
		var m Modality
		if err := json.Unmarshal(mb, &m); err != nil {
			return Provenance{}, fmt.Errorf("parse: modality: %w", err)
		}
		p.Modality = &m
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

// ComputeLeafV2 computes the sole canonical L1-bound leaf committing to framed H(d|p), H(E),
// the 32-byte MPE root (binding selective-disclosure proofs directly to the anchored BSV Merkle root),
// and the 32-byte volumetric content SHA-256 (0x00*32 when no external CHIRP blob is attached).
func ComputeLeafV2(f Fragment) []byte {
	hdp := f.HDP
	if len(hdp) == 0 {
		hdp = ComputeHDPV2(f.Data, f.Provenance)
	}
	mpeRoot := Zero32Bytes()
	if len(f.MPERoot) == 32 {
		copy(mpeRoot, f.MPERoot)
	}
	contentHash := Zero32Bytes()
	if len(f.ContentSHA256) == 32 {
		copy(contentHash, f.ContentSHA256)
	}
	return SHA256(Concat(TagLeafV2, hdp, ComputeEH(f.Ciphertext), mpeRoot, contentHash))
}

// SerializeFragment encodes (d | prov | hdp | E) with u32le length framing.
func SerializeFragment(f Fragment) []byte {
	hdp := f.HDP
	if len(hdp) == 0 {
		hdp = ComputeHDPV2(f.Data, f.Provenance)
	}
	return Concat(
		Field(f.Data),
		Field(SerializeProvenance(f.Provenance)),
		Field(hdp),
		Field(f.Ciphertext),
	)
}

// ParseFragment decodes SerializeFragment bytes.
func ParseFragment(b []byte) (Fragment, error) {
	d, next, err := takeField(b, 0)
	if err != nil {
		return Fragment{}, err
	}
	pv, next, err := takeField(b, next)
	if err != nil {
		return Fragment{}, err
	}
	hd, next, err := takeField(b, next)
	if err != nil {
		return Fragment{}, err
	}
	ev, end, err := takeField(b, next)
	if err != nil {
		return Fragment{}, err
	}
	if end != len(b) {
		return Fragment{}, errors.New("parse: trailing bytes in fragment")
	}
	p, err := ParseProvenance(pv)
	if err != nil {
		return Fragment{}, err
	}
	f := Fragment{Data: d, Provenance: p}
	if len(hd) > 0 {
		f.HDP = hd
	}
	if len(ev) > 0 {
		f.Ciphertext = ev
	}
	return f, nil
}
