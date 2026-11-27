package core

import (
	"errors"
	"fmt"

	"github.com/lightwebinc/bcommon/cbor"
)

// EncodeCBOR encodes v into RFC 8949 §4.2.1 core deterministic canonical CBOR via bcommon/cbor.
// Every map key is strictly ordered and non-canonical types (floats, tags, indefinite lengths) are rejected.
func EncodeCBOR(v cbor.Value) ([]byte, error) {
	return cbor.Encode(v)
}

// DecodeCBOR strictly decodes RFC 8949 canonical CBOR, refusing any non-canonical encoding.
func DecodeCBOR(b []byte) (cbor.Value, error) {
	return cbor.DecodeValue(b)
}

// HashCBOR computes domain-separated DSHA256(tag || CanonicalCBOR(v)).
func HashCBOR(tag []byte, v cbor.Value) ([]byte, error) {
	enc, err := EncodeCBOR(v)
	if err != nil {
		return nil, err
	}
	return DSHA256(Concat(tag, enc)), nil
}

// CBORMapToUintMap indexes a decoded cbor.Map by uint64 keys, rejecting non-uint or duplicate keys.
func CBORMapToUintMap(v cbor.Value) (map[uint64]cbor.Value, error) {
	m, ok := v.(cbor.Map)
	if !ok {
		return nil, errors.New("cbor: expected map")
	}
	out := make(map[uint64]cbor.Value, len(m))
	for _, p := range m {
		k, ok := p.Key.(uint64)
		if !ok {
			return nil, fmt.Errorf("cbor: expected uint64 map key, got %T", p.Key)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("cbor: duplicate key %d", k)
		}
		out[k] = p.Val
	}
	return out, nil
}

func cborText(m map[uint64]cbor.Value, k uint64) (string, error) {
	v, ok := m[k]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("cbor: key %d expected text, got %T", k, v)
	}
	return s, nil
}

func cborBytes(m map[uint64]cbor.Value, k uint64) ([]byte, error) {
	v, ok := m[k]
	if !ok {
		return nil, nil
	}
	b, ok := v.([]byte)
	if !ok {
		return nil, fmt.Errorf("cbor: key %d expected bytes, got %T", k, v)
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp, nil
}

func cborUint(m map[uint64]cbor.Value, k uint64) (uint64, error) {
	v, ok := m[k]
	if !ok {
		return 0, nil
	}
	u, ok := v.(uint64)
	if !ok {
		return 0, fmt.Errorf("cbor: key %d expected uint64, got %T", k, v)
	}
	return u, nil
}

func cborInt(m map[uint64]cbor.Value, k uint64) (int64, error) {
	v, ok := m[k]
	if !ok {
		return 0, nil
	}
	switch n := v.(type) {
	case uint64:
		if n > uint64(1<<63-1) {
			return 0, fmt.Errorf("cbor: key %d uint64 overflows int64", k)
		}
		return int64(n), nil
	case int64:
		return n, nil
	default:
		return 0, fmt.Errorf("cbor: key %d expected int, got %T", k, v)
	}
}
