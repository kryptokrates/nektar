package reasoning

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/lightwebinc/bcommon/cbor"

	"github.com/kryptokrates/nektar/internal/core"
)

// Triple represents a canonical committed RDF-style assertion T_i = (s_i, p_i, o_i, H_i)
// optionally qualified by a deontic, epistemic, or temporal Modality (Paper §8).
type Triple struct {
	Subject   string         `json:"s"`
	Predicate string         `json:"p"`
	Object    string         `json:"o"`
	Modality  *core.Modality `json:"modality,omitempty"`
	Hash      string         `json:"h,omitempty"` // 32-byte lowercase hex DSHA256(TagTripleV1 || CBOR(s,p,o,modality))
}

// Validate checks that Subject, Predicate, and Object are non-empty canonical strings.
func (t Triple) Validate() error {
	if strings.TrimSpace(t.Subject) == "" {
		return errors.New("reasoning: triple subject is required")
	}
	if strings.TrimSpace(t.Predicate) == "" {
		return errors.New("reasoning: triple predicate is required")
	}
	if strings.TrimSpace(t.Object) == "" {
		return errors.New("reasoning: triple object is required")
	}
	if t.Modality != nil {
		switch t.Modality.Kind {
		case "deontic", "epistemic", "temporal":
		default:
			return fmt.Errorf("reasoning: invalid modality kind %q", t.Modality.Kind)
		}
	}
	return nil
}

func (t Triple) cborBody() cbor.Map {
	m := cbor.Map{
		{Key: uint64(0), Val: strings.TrimSpace(t.Subject)},
		{Key: uint64(1), Val: strings.TrimSpace(t.Predicate)},
		{Key: uint64(2), Val: strings.TrimSpace(t.Object)},
	}
	if t.Modality != nil {
		m = append(m,
			cbor.Pair{Key: uint64(3), Val: t.Modality.Kind},
			cbor.Pair{Key: uint64(4), Val: t.Modality.Qualifier},
		)
	}
	return m
}

// ComputeTripleHash computes the canonical RFC 8949 CBOR commitment hash H_i of triple t.
func ComputeTripleHash(t Triple) (string, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}
	h, err := core.HashCBOR(core.TagTripleV1, t.cborBody())
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h), nil
}

// FinalizeTriple validates t and populates its canonical Hash field.
func FinalizeTriple(t Triple) (Triple, error) {
	h, err := ComputeTripleHash(t)
	if err != nil {
		return Triple{}, err
	}
	t.Subject = strings.TrimSpace(t.Subject)
	t.Predicate = strings.TrimSpace(t.Predicate)
	t.Object = strings.TrimSpace(t.Object)
	t.Hash = h
	return t, nil
}

// EncodeTripleCBOR encodes t (including its Hash) into canonical RFC 8949 CBOR.
func EncodeTripleCBOR(t Triple) ([]byte, error) {
	fin, err := FinalizeTriple(t)
	if err != nil {
		return nil, err
	}
	m := append(fin.cborBody(), cbor.Pair{Key: uint64(5), Val: fin.Hash})
	return core.EncodeCBOR(m)
}

var tripleKnownKeys = core.KnownKeyRange(0, 5)

// DecodeTripleCBOR decodes and cryptographically verifies a canonical CBOR Triple,
// rejecting unknown critical (even) keys while tolerating non-critical (odd) extensions.
func DecodeTripleCBOR(raw []byte) (Triple, error) {
	v, err := core.DecodeCBOR(raw)
	if err != nil {
		return Triple{}, fmt.Errorf("triple: decode cbor: %w", err)
	}
	m, _, err := core.SplitCBORMap(v, tripleKnownKeys)
	if err != nil {
		return Triple{}, fmt.Errorf("triple: %w", err)
	}
	getStr := func(k uint64) (string, bool, error) {
		val, ok := m[k]
		if !ok {
			return "", false, nil
		}
		s, ok := val.(string)
		if !ok {
			return "", false, fmt.Errorf("triple: field %d expected string, got %T", k, val)
		}
		return s, true, nil
	}
	s, _, err := getStr(0)
	if err != nil {
		return Triple{}, err
	}
	p, _, err := getStr(1)
	if err != nil {
		return Triple{}, err
	}
	o, _, err := getStr(2)
	if err != nil {
		return Triple{}, err
	}
	mk, hasMK, err := getStr(3)
	if err != nil {
		return Triple{}, err
	}
	mq, _, err := getStr(4)
	if err != nil {
		return Triple{}, err
	}
	h, _, err := getStr(5)
	if err != nil {
		return Triple{}, err
	}
	tr := Triple{Subject: s, Predicate: p, Object: o, Hash: h}
	if hasMK && mk != "" {
		tr.Modality = &core.Modality{Kind: mk, Qualifier: mq}
	}
	want, err := ComputeTripleHash(tr)
	if err != nil {
		return Triple{}, err
	}
	if !strings.EqualFold(want, tr.Hash) {
		return Triple{}, errors.New("triple: hash mismatch")
	}
	return tr, nil
}

// ToMangleAtoms translates a canonical Triple into sanitized, injection-proof ground Mangle atoms.
// Automatically emits rdf_type(s, o) when predicate is "rdf:type" / "type" / "isA" so DL-Lite
// subclass and disjointness rules operate over committed triples seamlessly.
func (t Triple) ToMangleAtoms() ([]string, error) {
	fin, err := FinalizeTriple(t)
	if err != nil {
		return nil, err
	}
	atoms := []string{
		fmt.Sprintf("triple(%s, %s, %s)", strconv.Quote(fin.Subject), strconv.Quote(fin.Predicate), strconv.Quote(fin.Object)),
	}
	lp := strings.ToLower(fin.Predicate)
	if lp == "rdf:type" || lp == "type" || lp == "isa" || lp == "http://www.w3.org/1999/02/22-rdf-syntax-ns#type" {
		atoms = append(atoms, fmt.Sprintf("rdf_type(%s, %s)", strconv.Quote(fin.Subject), strconv.Quote(fin.Object)))
	}
	if fin.Modality != nil {
		atoms = append(atoms, fmt.Sprintf("triple_modality(%s, %s, %s)", strconv.Quote(fin.Hash), strconv.Quote(fin.Modality.Kind), strconv.Quote(fin.Modality.Qualifier)))
	}
	// Verify every emitted atom passes ParseGroundFact strictly (zero syntax injection).
	valid := make([]string, 0, len(atoms))
	for _, a := range atoms {
		parsed, ok := ParseGroundFact(a)
		if !ok {
			return nil, fmt.Errorf("reasoning: emitted triple atom failed ground check: %q", a)
		}
		valid = append(valid, parsed.String())
	}
	return valid, nil
}

// ParseNTripleLine parses a simple N-Triples line (`<s> <p> <o> .` or `<s> <p> "lit" .`) into a Triple.
func ParseNTripleLine(line string) (Triple, bool) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "#") {
		return Triple{}, false
	}
	s = strings.TrimSpace(strings.TrimSuffix(s, "."))
	toks := make([]string, 0, 3)
	for len(s) > 0 && len(toks) < 3 {
		s = strings.TrimLeft(s, " \t")
		if len(s) == 0 {
			break
		}
		switch s[0] {
		case '<':
			end := strings.IndexByte(s, '>')
			if end <= 1 {
				return Triple{}, false
			}
			toks = append(toks, s[1:end])
			s = s[end+1:]
		case '"':
			// Parse quoted literal with simple escape handling.
			i := 1
			for i < len(s) {
				if s[i] == '\\' && i+1 < len(s) {
					i += 2
					continue
				}
				if s[i] == '"' {
					break
				}
				i++
			}
			if i >= len(s) {
				return Triple{}, false
			}
			unq, err := strconv.Unquote(s[:i+1])
			if err != nil {
				return Triple{}, false
			}
			toks = append(toks, unq)
			s = s[i+1:]
		default:
			return Triple{}, false
		}
	}
	if len(toks) != 3 || strings.TrimSpace(s) != "" {
		return Triple{}, false
	}
	tr, err := FinalizeTriple(Triple{Subject: toks[0], Predicate: toks[1], Object: toks[2]})
	if err != nil {
		return Triple{}, false
	}
	return tr, true
}
