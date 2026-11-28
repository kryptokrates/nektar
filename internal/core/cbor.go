package core

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/lightwebinc/bcommon/cbor"
)

const (
	// MaxCBORSeqDepth matches bcommon/cbor's MaxDepth (16) for nested containers.
	MaxCBORSeqDepth = 16
	// DefaultMaxCBORSeqItemBytes caps any single RFC 8742 CBOR sequence record (16 MiB).
	DefaultMaxCBORSeqItemBytes = 16 << 20
)

var (
	// ErrUnknownCriticalKey is returned when a CBOR map carries an unknown even integer key.
	// Following BRC-167 CHIRP's TLV convention: unknown even keys are critical (fail-closed);
	// unknown odd keys are non-critical extensions (preserved verbatim).
	ErrUnknownCriticalKey = errors.New("cbor: unknown critical (even) key")

	// ErrCBORSeqItemTooLarge is returned when a streaming RFC 8742 item exceeds its configured cap.
	ErrCBORSeqItemTooLarge = errors.New("cbor-seq: item exceeds max size")
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

// SplitCBORMap indexes a decoded uint-keyed CBOR map against knownKeys and enforces Nektar's
// forward-compatible schema evolution policy:
//   - Unknown EVEN keys are critical and rejected fail-closed (ErrUnknownCriticalKey).
//   - Unknown ODD keys are non-critical extensions and returned verbatim in extra.
func SplitCBORMap(v cbor.Value, knownKeys map[uint64]struct{}) (map[uint64]cbor.Value, []cbor.Pair, error) {
	m, ok := v.(cbor.Map)
	if !ok {
		return nil, nil, errors.New("cbor: expected map")
	}
	vals := make(map[uint64]cbor.Value, len(m))
	var extra []cbor.Pair
	for _, p := range m {
		k, ok := p.Key.(uint64)
		if !ok {
			return nil, nil, fmt.Errorf("cbor: expected uint64 map key, got %T", p.Key)
		}
		if _, dup := vals[k]; dup {
			return nil, nil, fmt.Errorf("cbor: duplicate key %d", k)
		}
		if knownKeys != nil {
			if _, known := knownKeys[k]; !known {
				if k%2 == 0 {
					return nil, nil, fmt.Errorf("%w: %d", ErrUnknownCriticalKey, k)
				}
				extra = append(extra, p)
				continue
			}
		}
		vals[k] = p.Val
	}
	return vals, extra, nil
}

// KnownKeySet builds a uint64 key lookup set for SplitCBORMap.
func KnownKeySet(keys ...uint64) map[uint64]struct{} {
	s := make(map[uint64]struct{}, len(keys))
	for _, k := range keys {
		s[k] = struct{}{}
	}
	return s
}

// KnownKeyRange builds a contiguous uint64 key lookup set [minKey..maxKey] inclusive.
func KnownKeyRange(minKey, maxKey uint64) map[uint64]struct{} {
	if maxKey < minKey {
		return map[uint64]struct{}{}
	}
	s := make(map[uint64]struct{}, maxKey-minKey+1)
	for k := minKey; k <= maxKey; k++ {
		s[k] = struct{}{}
	}
	return s
}

// CBORText reads an optional text string from a uint-keyed CBOR map.
func CBORText(m map[uint64]cbor.Value, k uint64) (string, error) {
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

// CBORBytes reads an optional byte string from a uint-keyed CBOR map (copied defensively).
func CBORBytes(m map[uint64]cbor.Value, k uint64) ([]byte, error) {
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

// CBORUint reads an optional unsigned integer from a uint-keyed CBOR map.
func CBORUint(m map[uint64]cbor.Value, k uint64) (uint64, error) {
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

// CBORInt reads an optional signed integer from a uint-keyed CBOR map.
func CBORInt(m map[uint64]cbor.Value, k uint64) (int64, error) {
	v, ok := m[k]
	if !ok {
		return 0, nil
	}
	switch n := v.(type) {
	case uint64:
		if n > uint64(math.MaxInt64) {
			return 0, fmt.Errorf("cbor: key %d uint64 overflows int64", k)
		}
		return int64(n), nil
	case int64:
		return n, nil
	default:
		return 0, fmt.Errorf("cbor: key %d expected int, got %T", k, v)
	}
}

// CBORBool reads an optional boolean from a uint-keyed CBOR map.
func CBORBool(m map[uint64]cbor.Value, k uint64) (bool, error) {
	v, ok := m[k]
	if !ok {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("cbor: key %d expected bool, got %T", k, v)
	}
	return b, nil
}

// CBORStrings reads an optional array of UTF-8 strings from a uint-keyed CBOR map.
func CBORStrings(m map[uint64]cbor.Value, k uint64) ([]string, error) {
	v, ok := m[k]
	if !ok {
		return nil, nil
	}
	arr, ok := v.([]cbor.Value)
	if !ok {
		return nil, fmt.Errorf("cbor: key %d expected array, got %T", k, v)
	}
	out := make([]string, len(arr))
	for i, item := range arr {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("cbor: key %d[%d] expected text, got %T", k, i, item)
		}
		out[i] = s
	}
	return out, nil
}

// CBORStringMap reads an optional string->string map from a uint-keyed CBOR map.
func CBORStringMap(m map[uint64]cbor.Value, k uint64) (map[string]string, error) {
	v, ok := m[k]
	if !ok {
		return nil, nil
	}
	cm, ok := v.(cbor.Map)
	if !ok {
		return nil, fmt.Errorf("cbor: key %d expected map, got %T", k, v)
	}
	out := make(map[string]string, len(cm))
	for _, p := range cm {
		ks, ok := p.Key.(string)
		if !ok {
			return nil, fmt.Errorf("cbor: key %d expected text map key, got %T", k, p.Key)
		}
		vs, ok := p.Val.(string)
		if !ok {
			return nil, fmt.Errorf("cbor: key %d[%q] expected text value, got %T", k, ks, p.Val)
		}
		out[ks] = vs
	}
	return out, nil
}

// CBORArray reads an optional array from a uint-keyed CBOR map.
func CBORArray(m map[uint64]cbor.Value, k uint64) ([]cbor.Value, error) {
	v, ok := m[k]
	if !ok {
		return nil, nil
	}
	arr, ok := v.([]cbor.Value)
	if !ok {
		return nil, fmt.Errorf("cbor: key %d expected array, got %T", k, v)
	}
	return arr, nil
}

// CBORStringsVal converts a Go []string into a []cbor.Value array.
func CBORStringsVal(ss []string) []cbor.Value {
	out := make([]cbor.Value, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// CBORStringMapVal converts a Go map[string]string into a cbor.Map (bcommon/cbor sorts keys canonically on encode).
func CBORStringMapVal(sm map[string]string) cbor.Map {
	out := make(cbor.Map, 0, len(sm))
	for k, v := range sm {
		out = append(out, cbor.Pair{Key: k, Val: v})
	}
	return out
}

// Unexported aliases for internal/core callers.
func cborText(m map[uint64]cbor.Value, k uint64) (string, error)   { return CBORText(m, k) }
func cborBytes(m map[uint64]cbor.Value, k uint64) ([]byte, error)  { return CBORBytes(m, k) }
func cborUint(m map[uint64]cbor.Value, k uint64) (uint64, error)   { return CBORUint(m, k) }
func cborInt(m map[uint64]cbor.Value, k uint64) (int64, error)     { return CBORInt(m, k) }

// WriteCBORSequenceItem encodes v as a single canonical RFC 8949 CDE CBOR item and writes it
// directly to w as one self-delimiting element of an RFC 8742 CBOR Sequence.
// Returns the exact encoded bytes so callers can feed them into a running stream digest.
func WriteCBORSequenceItem(w io.Writer, v cbor.Value) ([]byte, error) {
	enc, err := EncodeCBOR(v)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(enc); err != nil {
		return nil, err
	}
	return enc, nil
}

// CBORSeqReader reads a streaming RFC 8742 CBOR Sequence item-by-item in O(1) memory,
// validating every single item against strict RFC 8949 §4.2.1 CDE via bcommon/cbor.
type CBORSeqReader struct {
	br           *bufio.Reader
	maxItemBytes int
}

// NewCBORSeqReader wraps r in a bounded RFC 8742 streaming CBOR sequence reader.
func NewCBORSeqReader(r io.Reader, maxItemBytes int) *CBORSeqReader {
	if maxItemBytes <= 0 {
		maxItemBytes = DefaultMaxCBORSeqItemBytes
	}
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 32<<10)
	}
	return &CBORSeqReader{br: br, maxItemBytes: maxItemBytes}
}

// Next reads the next complete self-delimiting CBOR item from the stream, returning both its
// exact canonical raw bytes and decoded cbor.Value. Returns io.EOF cleanly at sequence end.
func (sr *CBORSeqReader) Next() ([]byte, cbor.Value, error) {
	b0, err := sr.br.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil, io.EOF
		}
		return nil, nil, err
	}
	if err := sr.br.UnreadByte(); err != nil {
		return nil, nil, err
	}
	_ = b0
	buf := make([]byte, 0, 256)
	buf, err = sr.readItemBytes(buf, 0)
	if err != nil {
		return nil, nil, err
	}
	val, err := DecodeCBOR(buf)
	if err != nil {
		return nil, nil, fmt.Errorf("cbor-seq: non-canonical item: %w", err)
	}
	return buf, val, nil
}

func (sr *CBORSeqReader) readItemBytes(buf []byte, depth int) ([]byte, error) {
	if depth > MaxCBORSeqDepth {
		return nil, errors.New("cbor-seq: nesting depth exceeds maximum")
	}
	if len(buf)+1 > sr.maxItemBytes {
		return nil, ErrCBORSeqItemTooLarge
	}
	b0, err := sr.br.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	buf = append(buf, b0)
	mt := b0 >> 5
	ai := b0 & 0x1f

	if mt == 6 {
		return nil, errors.New("cbor-seq: CBOR tags (major type 6) are unsupported")
	}

	var arg uint64
	switch {
	case ai < 24:
		arg = uint64(ai)
	case ai == 24:
		buf, arg, err = sr.readUintArg(buf, 1)
	case ai == 25:
		buf, arg, err = sr.readUintArg(buf, 2)
	case ai == 26:
		buf, arg, err = sr.readUintArg(buf, 4)
	case ai == 27:
		buf, arg, err = sr.readUintArg(buf, 8)
	default:
		// ai == 28..30 reserved, ai == 31 indefinite length (disallowed by RFC 8949 CDE)
		return nil, fmt.Errorf("cbor-seq: unsupported additional info %d", ai)
	}
	if err != nil {
		return nil, err
	}

	switch mt {
	case 0, 1:
		return buf, nil
	case 2, 3:
		rem := uint64(sr.maxItemBytes - len(buf))
		if arg > rem {
			return nil, ErrCBORSeqItemTooLarge
		}
		n := int(arg)
		start := len(buf)
		buf = append(buf, make([]byte, n)...)
		if _, err := io.ReadFull(sr.br, buf[start:]); err != nil {
			return nil, err
		}
		return buf, nil
	case 4:
		rem := uint64(sr.maxItemBytes - len(buf))
		if arg > rem {
			return nil, ErrCBORSeqItemTooLarge
		}
		for i := uint64(0); i < arg; i++ {
			buf, err = sr.readItemBytes(buf, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return buf, nil
	case 5:
		rem := uint64(sr.maxItemBytes - len(buf))
		if arg > rem/2 {
			return nil, ErrCBORSeqItemTooLarge
		}
		for i := uint64(0); i < arg; i++ {
			buf, err = sr.readItemBytes(buf, depth+1)
			if err != nil {
				return nil, err
			}
			buf, err = sr.readItemBytes(buf, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return buf, nil
	case 7:
		if ai != 20 && ai != 21 && ai != 22 {
			return nil, fmt.Errorf("cbor-seq: unsupported simple/float value ai=%d", ai)
		}
		return buf, nil
	default:
		return nil, fmt.Errorf("cbor-seq: unexpected major type %d", mt)
	}
}

func (sr *CBORSeqReader) readUintArg(buf []byte, width int) ([]byte, uint64, error) {
	if len(buf)+width > sr.maxItemBytes {
		return nil, 0, ErrCBORSeqItemTooLarge
	}
	var tmp [8]byte
	if _, err := io.ReadFull(sr.br, tmp[:width]); err != nil {
		return nil, 0, err
	}
	buf = append(buf, tmp[:width]...)
	switch width {
	case 1:
		return buf, uint64(tmp[0]), nil
	case 2:
		return buf, uint64(binary.BigEndian.Uint16(tmp[:2])), nil
	case 4:
		return buf, uint64(binary.BigEndian.Uint32(tmp[:4])), nil
	case 8:
		return buf, binary.BigEndian.Uint64(tmp[:8]), nil
	default:
		return nil, 0, errors.New("cbor-seq: invalid argument width")
	}
}
