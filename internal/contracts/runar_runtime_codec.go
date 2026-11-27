package contracts

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// EncodePushData wraps raw bytes in a BSV MINIMALDATA-compliant Bitcoin Script push opcode.
func EncodePushData(data []byte) []byte {
	n := len(data)
	if n == 0 {
		return []byte{0x00} // OP_0
	}
	if n == 1 {
		b := data[0]
		if b >= 0x01 && b <= 0x10 {
			return []byte{0x50 + b} // OP_1..OP_16
		}
		if b == 0x81 {
			return []byte{0x4f} // OP_1NEGATE
		}
		return []byte{0x01, b}
	}
	if n <= 75 {
		out := make([]byte, 1+n)
		out[0] = byte(n)
		copy(out[1:], data)
		return out
	}
	if n <= 0xff {
		out := make([]byte, 2+n)
		out[0] = 0x4c // OP_PUSHDATA1
		out[1] = byte(n)
		copy(out[2:], data)
		return out
	}
	if n <= 0xffff {
		out := make([]byte, 3+n)
		out[0] = 0x4d // OP_PUSHDATA2
		binary.LittleEndian.PutUint16(out[1:3], uint16(n))
		copy(out[3:], data)
		return out
	}
	out := make([]byte, 5+n)
	out[0] = 0x4e // OP_PUSHDATA4
	binary.LittleEndian.PutUint32(out[1:5], uint32(n))
	copy(out[5:], data)
	return out
}

// EncodeStatePushData frames variable-width ByteString state fields for the OP_RETURN suffix
// WITHOUT the OP_1..OP_16 minimaldata short-circuit, matching Rúnar's emitPushDataEncode.
func EncodeStatePushData(data []byte) []byte {
	n := len(data)
	if n <= 75 {
		out := make([]byte, 1+n)
		out[0] = byte(n)
		copy(out[1:], data)
		return out
	}
	if n <= 0xff {
		out := make([]byte, 2+n)
		out[0] = 0x4c
		out[1] = byte(n)
		copy(out[2:], data)
		return out
	}
	if n <= 0xffff {
		out := make([]byte, 3+n)
		out[0] = 0x4d
		binary.LittleEndian.PutUint16(out[1:3], uint16(n))
		copy(out[3:], data)
		return out
	}
	out := make([]byte, 5+n)
	out[0] = 0x4e
	binary.LittleEndian.PutUint32(out[1:5], uint32(n))
	copy(out[5:], data)
	return out
}

// EncodeScriptInt encodes a signed integer using Bitcoin Script minimal push encoding
// (OP_0, OP_1..OP_16, OP_1NEGATE, or minimal little-endian sign-magnitude push).
func EncodeScriptInt(n int64) []byte {
	if n == 0 {
		return []byte{0x00}
	}
	if n >= 1 && n <= 16 {
		return []byte{byte(0x50 + n)}
	}
	if n == -1 {
		return []byte{0x4f}
	}
	neg := n < 0
	var u uint64
	if neg {
		u = uint64(-(n + 1)) + 1 // overflow-safe for math.MinInt64
	} else {
		u = uint64(n)
	}
	var buf []byte
	for u > 0 {
		buf = append(buf, byte(u&0xff))
		u >>= 8
	}
	if buf[len(buf)-1]&0x80 != 0 {
		if neg {
			buf = append(buf, 0x80)
		} else {
			buf = append(buf, 0x00)
		}
	} else if neg {
		buf[len(buf)-1] |= 0x80
	}
	return EncodePushData(buf)
}

func encodeNum2BinChecked(n int64, width int) ([]byte, error) {
	if width <= 0 {
		return nil, fmt.Errorf("contracts: num2bin width %d must be > 0", width)
	}
	neg := n < 0
	var u uint64
	if neg {
		u = uint64(-(n + 1)) + 1 // overflow-safe for math.MinInt64 (= 1<<63)
	} else {
		u = uint64(n)
	}
	// Sign-magnitude uses the MSB of buf[width-1] for the sign bit, leaving (width*8 - 1) magnitude bits.
	if width <= 8 {
		maxMag := (uint64(1) << (uint(width)*8 - 1)) - 1
		if u > maxMag {
			return nil, fmt.Errorf("contracts: integer %d overflows %d-byte sign-magnitude num2bin", n, width)
		}
	}
	buf := make([]byte, width)
	for i := 0; i < width && u > 0; i++ {
		buf[i] = byte(u & 0xff)
		u >>= 8
	}
	if neg {
		buf[width-1] |= 0x80
	}
	return buf, nil
}

// EncodeNum2Bin encodes n as a fixed-width little-endian sign-magnitude byte slice (matching OP_NUM2BIN).
// Handles math.MinInt64 safely without signed negation overflow and clamps out-of-width values
// rather than silently wrapping to negative zero.
func EncodeNum2Bin(n int64, width int) []byte {
	if width <= 0 {
		return nil
	}
	if buf, err := encodeNum2BinChecked(n, width); err == nil {
		return buf
	}
	// Saturate to the maximum representable signed magnitude for this width instead of truncating into -0.
	buf := make([]byte, width)
	for i := 0; i < width-1; i++ {
		buf[i] = 0xff
	}
	if n < 0 {
		buf[width-1] = 0xff // sign bit (0x80) | max 7-bit magnitude (0x7f)
	} else {
		buf[width-1] = 0x7f
	}
	return buf
}

// DecodeNum2Bin decodes a fixed-width little-endian sign-magnitude byte slice (inverse of EncodeNum2Bin).
func DecodeNum2Bin(b []byte) int64 {
	if len(b) == 0 {
		return 0
	}
	neg := b[len(b)-1]&0x80 != 0
	var u uint64
	for i := 0; i < len(b) && i < 8; i++ {
		v := b[i]
		if i == len(b)-1 {
			v &= 0x7f
		}
		u |= uint64(v) << (8 * i)
	}
	if neg {
		return -int64(u)
	}
	return int64(u)
}

// EncodeArg encodes a Rúnar constructor or method argument into a minimal Bitcoin Script push.
// Supported types: int, int32, int64, uint32, uint64, *big.Int, bool, []byte, string (hex).
func EncodeArg(v any) ([]byte, error) {
	switch x := v.(type) {
	case int:
		return EncodeScriptInt(int64(x)), nil
	case int32:
		return EncodeScriptInt(int64(x)), nil
	case int64:
		return EncodeScriptInt(x), nil
	case uint32:
		return EncodeScriptInt(int64(x)), nil
	case uint64:
		if x > 1<<63-1 {
			return EncodeBigIntScriptNum(new(big.Int).SetUint64(x)), nil
		}
		return EncodeScriptInt(int64(x)), nil
	case *big.Int:
		return EncodeBigIntScriptNum(x), nil
	case bool:
		if x {
			return []byte{0x51}, nil
		}
		return []byte{0x00}, nil
	case []byte:
		return EncodePushData(x), nil
	case string:
		if x == "" {
			return []byte{0x00}, nil
		}
		raw, err := hex.DecodeString(strings.TrimPrefix(x, "0x"))
		if err != nil {
			return nil, fmt.Errorf("contracts: EncodeArg hex string: %w", err)
		}
		return EncodePushData(raw), nil
	default:
		return nil, fmt.Errorf("contracts: unsupported Rúnar arg type %T", v)
	}
}

// EncodeBigIntScriptNum encodes an arbitrary-precision *big.Int as a minimal Script number push.
func EncodeBigIntScriptNum(v *big.Int) []byte {
	if v == nil || v.Sign() == 0 {
		return []byte{0x00}
	}
	if v.IsInt64() {
		return EncodeScriptInt(v.Int64())
	}
	neg := v.Sign() < 0
	abs := new(big.Int).Abs(v)
	be := abs.Bytes()
	// Reverse to little-endian.
	le := make([]byte, len(be))
	for i := range be {
		le[i] = be[len(be)-1-i]
	}
	if le[len(le)-1]&0x80 != 0 {
		if neg {
			le = append(le, 0x80)
		} else {
			le = append(le, 0x00)
		}
	} else if neg {
		le[len(le)-1] |= 0x80
	}
	return EncodePushData(le)
}

// SerializeStateFields encodes stateful fields in declaration order (by Index) matching Rúnar's OP_RETURN layout.
func SerializeStateFields(fields []ArtifactStateField, state map[string]any) ([]byte, error) {
	ordered := append([]ArtifactStateField(nil), fields...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })

	var out []byte
	for _, f := range ordered {
		val, ok := state[f.Name]
		if !ok {
			// Case-insensitive lookup fallback for Go PascalCase callers.
			for k, v := range state {
				if strings.EqualFold(k, f.Name) {
					val = v
					ok = true
					break
				}
			}
		}
		if !ok {
			return nil, fmt.Errorf("contracts: missing state field %q (%s)", f.Name, f.Type)
		}
		b, err := encodeStateFieldValue(f.Name, f.Type, val)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
	}
	return out, nil
}

func encodeStateFieldValue(name, typ string, val any) ([]byte, error) {
	switch typ {
	case "int", "bigint", "RabinSig", "RabinPubKey":
		n, err := toInt64(val)
		if err != nil {
			return nil, fmt.Errorf("contracts: state field %q (%s): %w", name, typ, err)
		}
		enc, err := encodeNum2BinChecked(n, 8)
		if err != nil {
			return nil, fmt.Errorf("contracts: state field %q (%s): %w", name, typ, err)
		}
		return enc, nil
	case "bool", "boolean":
		b, ok := val.(bool)
		if !ok {
			return nil, fmt.Errorf("contracts: state field %q (%s) requires bool, got %T", name, typ, val)
		}
		if b {
			return []byte{0x01}, nil
		}
		return []byte{0x00}, nil
	case "PubKey", "Addr", "Ripemd160", "Sha256", "Point", "P256Point", "P384Point":
		raw, err := toRawBytes(val)
		if err != nil {
			return nil, fmt.Errorf("contracts: state field %q (%s): %w", name, typ, err)
		}
		want := fixedStateFieldWidth(typ)
		if want > 0 && len(raw) != want {
			return nil, fmt.Errorf("contracts: state field %q (%s) must be %d bytes, got %d", name, typ, want, len(raw))
		}
		return raw, nil
	default:
		// ByteString and variable-width byte types: raw <len><data> framing.
		raw, err := toRawBytes(val)
		if err != nil {
			return nil, fmt.Errorf("contracts: state field %q (%s): %w", name, typ, err)
		}
		return EncodeStatePushData(raw), nil
	}
}

func fixedStateFieldWidth(typ string) int {
	switch typ {
	case "bool", "boolean":
		return 1
	case "int", "bigint", "RabinSig", "RabinPubKey":
		return 8
	case "PubKey":
		return 33
	case "Addr", "Ripemd160":
		return 20
	case "Sha256":
		return 32
	case "Point", "P256Point":
		return 64
	case "P384Point":
		return 96
	default:
		return 0
	}
}

func toInt64(val any) (int64, error) {
	switch v := val.(type) {
	case int:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case int64:
		return v, nil
	case uint32:
		return int64(v), nil
	case uint64:
		if v > 1<<63-1 {
			return 0, fmt.Errorf("uint64 %d overflows int64", v)
		}
		return int64(v), nil
	case *big.Int:
		if v == nil || !v.IsInt64() {
			return 0, errors.New("bigint out of int64 range")
		}
		return v.Int64(), nil
	default:
		return 0, fmt.Errorf("expected integer, got %T", val)
	}
}

func toRawBytes(val any) ([]byte, error) {
	switch v := val.(type) {
	case []byte:
		return append([]byte(nil), v...), nil
	case string:
		if v == "" {
			return nil, nil
		}
		return hex.DecodeString(strings.TrimPrefix(v, "0x"))
	default:
		return nil, fmt.Errorf("expected []byte or hex string, got %T", val)
	}
}

// DeserializeStateFields decodes the OP_RETURN state payload of a StatefulSmartContract locking script.
func DeserializeStateFields(fields []ArtifactStateField, stateBytes []byte) (map[string]any, error) {
	ordered := append([]ArtifactStateField(nil), fields...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })

	res := make(map[string]any, len(ordered))
	off := 0
	for _, f := range ordered {
		switch f.Type {
		case "int", "bigint", "RabinSig", "RabinPubKey":
			if off+8 > len(stateBytes) {
				return nil, fmt.Errorf("contracts: truncated bigint state field %q", f.Name)
			}
			res[f.Name] = DecodeNum2Bin(stateBytes[off : off+8])
			off += 8
		case "bool", "boolean":
			if off+1 > len(stateBytes) {
				return nil, fmt.Errorf("contracts: truncated bool state field %q", f.Name)
			}
			res[f.Name] = stateBytes[off] != 0
			off++
		case "PubKey", "Addr", "Ripemd160", "Sha256", "Point", "P256Point", "P384Point":
			w := fixedStateFieldWidth(f.Type)
			if off+w > len(stateBytes) {
				return nil, fmt.Errorf("contracts: truncated %s state field %q", f.Type, f.Name)
			}
			res[f.Name] = append([]byte(nil), stateBytes[off:off+w]...)
			off += w
		default:
			if off >= len(stateBytes) {
				return nil, fmt.Errorf("contracts: truncated ByteString state field %q", f.Name)
			}
			op := stateBytes[off]
			off++
			var n int
			switch {
			case op <= 75:
				n = int(op)
			case op == 0x4c:
				if off+1 > len(stateBytes) {
					return nil, fmt.Errorf("contracts: truncated PUSHDATA1 len for %q", f.Name)
				}
				n = int(stateBytes[off])
				off++
			case op == 0x4d:
				if off+2 > len(stateBytes) {
					return nil, fmt.Errorf("contracts: truncated PUSHDATA2 len for %q", f.Name)
				}
				n = int(binary.LittleEndian.Uint16(stateBytes[off : off+2]))
				off += 2
			case op == 0x4e:
				if off+4 > len(stateBytes) {
					return nil, fmt.Errorf("contracts: truncated PUSHDATA4 len for %q", f.Name)
				}
				n = int(binary.LittleEndian.Uint32(stateBytes[off : off+4]))
				off += 4
			default:
				return nil, fmt.Errorf("contracts: unexpected state push opcode 0x%02x for %q", op, f.Name)
			}
			if off+n > len(stateBytes) {
				return nil, fmt.Errorf("contracts: truncated ByteString payload for %q (need %d, have %d)", f.Name, n, len(stateBytes)-off)
			}
			res[f.Name] = append([]byte(nil), stateBytes[off:off+n]...)
			off += n
		}
	}
	if off != len(stateBytes) {
		return nil, fmt.Errorf("contracts: %d trailing byte(s) in state suffix", len(stateBytes)-off)
	}
	return res, nil
}
