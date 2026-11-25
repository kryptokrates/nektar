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

// Fragment is the canonical unit of Nektar verifiable memory: (d, p, H(d|p), E(d)).
type Fragment struct {
	Data       []byte     `json:"d"`
	Provenance Provenance `json:"p"`
	HDP        []byte     `json:"hdp,omitempty"`
	Ciphertext []byte     `json:"E,omitempty"`
}

// Field prefixes b with its uint32 little-endian length.
func Field(b []byte) []byte {
	return Concat(U32LE(uint32(len(b))), b)
}

func takeField(b []byte, off int) ([]byte, int, error) {
	if off+4 > len(b) {
		return nil, 0, errors.New("parse: truncated field header")
	}
	n := int(binary.LittleEndian.Uint32(b[off : off+4]))
	if off+4+n > len(b) {
		return nil, 0, errors.New("parse: truncated field body")
	}
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
		mb, _ := json.Marshal(p.Modality)
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

// ComputeHDP computes H(d|p) = SHA256(TagProv || d || SerializeProvenance(p)).
func ComputeHDP(data []byte, p Provenance) []byte {
	return SHA256(Concat(TagProv, data, SerializeProvenance(p)))
}

// ComputeEH computes SHA256(E) (SHA256("") if empty).
func ComputeEH(ciphertext []byte) []byte {
	return SHA256(ciphertext)
}

// ComputeLeaf computes leaf = SHA256(TagLeaf || H(d|p) || H(E)).
func ComputeLeaf(f Fragment) []byte {
	hdp := f.HDP
	if len(hdp) == 0 {
		hdp = ComputeHDP(f.Data, f.Provenance)
	}
	return SHA256(Concat(TagLeaf, hdp, ComputeEH(f.Ciphertext)))
}

// SerializeFragment encodes (d | prov | hdp | E) with u32le length framing.
func SerializeFragment(f Fragment) []byte {
	hdp := f.HDP
	if len(hdp) == 0 {
		hdp = ComputeHDP(f.Data, f.Provenance)
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
