package contracts

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"

	"github.com/kryptokrates/nektar/internal/core"
)

// ABIParam describes one parameter of a compiled Rúnar constructor or method.
type ABIParam struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// ABIConstructor describes the compiled Rúnar constructor ABI.
type ABIConstructor struct {
	Params []ABIParam `json:"params"`
}

// ABIMethod describes one method in a compiled Rúnar artifact ABI.
type ABIMethod struct {
	Name         string     `json:"name"`
	Params       []ABIParam `json:"params"`
	IsPublic     bool       `json:"isPublic"`
	UsesCodePart bool       `json:"usesCodePart,omitempty"`
	SigHashType  *int       `json:"sigHashType,omitempty"`
}

// ArtifactABI is the full ABI section of a Rúnar v1.0.0-rc.1 artifact.
type ArtifactABI struct {
	Constructor ABIConstructor `json:"constructor"`
	Methods     []ABIMethod    `json:"methods"`
}

// ArtifactStateField describes one stateful field serialized after OP_RETURN.
type ArtifactStateField struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Index        int    `json:"index"`
	InitialValue any    `json:"initialValue,omitempty"`
}

// ConstructorSlot identifies an OP_0 placeholder replaced by a constructor argument push.
type ConstructorSlot struct {
	ParamIndex int `json:"paramIndex"`
	ByteOffset int `json:"byteOffset"`
}

// CodeSepIndexSlot identifies an OP_0 placeholder replaced by the adjusted OP_CODESEPARATOR byte offset.
type CodeSepIndexSlot struct {
	ByteOffset   int `json:"byteOffset"`
	CodeSepIndex int `json:"codeSepIndex"`
}

// CodePartLenSlot identifies a 4-byte LE code-part length pin filled at deploy time.
type CodePartLenSlot struct {
	ByteOffset int `json:"byteOffset"`
}

// FullArtifact is the complete Rúnar v1.0.0-rc.1 compiled JSON artifact embedded in Nektar.
type FullArtifact struct {
	Version              string               `json:"version"`
	CompilerVersion      string               `json:"compilerVersion"`
	ContractName         string               `json:"contractName"`
	ParentClass          string               `json:"parentClass,omitempty"`
	ABI                  ArtifactABI          `json:"abi"`
	Script               string               `json:"script"`
	ASM                  string               `json:"asm,omitempty"`
	StateFields          []ArtifactStateField `json:"stateFields,omitempty"`
	ConstructorSlots     []ConstructorSlot    `json:"constructorSlots,omitempty"`
	CodeSepIndexSlots    []CodeSepIndexSlot   `json:"codeSepIndexSlots,omitempty"`
	CodePartLenSlots     []CodePartLenSlot    `json:"codePartLenSlots,omitempty"`
	CodeSeparatorIndex   *int                 `json:"codeSeparatorIndex,omitempty"`
	CodeSeparatorIndices []int                `json:"codeSeparatorIndices,omitempty"`
}

var (
	fullArtifactMu    sync.RWMutex
	fullArtifactCache = make(map[string]*FullArtifact)
)

// LoadFullArtifact reads and caches a full embedded Rúnar JSON artifact by contract name.
func LoadFullArtifact(name string) (*FullArtifact, error) {
	fullArtifactMu.RLock()
	if a, ok := fullArtifactCache[name]; ok {
		fullArtifactMu.RUnlock()
		return a, nil
	}
	fullArtifactMu.RUnlock()

	raw, err := artifactsFS.ReadFile("artifacts/" + name + ".json")
	if err != nil {
		return nil, fmt.Errorf("contracts: load full artifact %s: %w", name, err)
	}
	var a FullArtifact
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("contracts: parse full artifact %s: %w", name, err)
	}
	fullArtifactMu.Lock()
	fullArtifactCache[name] = &a
	fullArtifactMu.Unlock()
	return &a, nil
}

// HasCodeSeparator reports whether the compiled artifact emits OP_CODESEPARATOR.
func (a *FullArtifact) HasCodeSeparator() bool {
	return a != nil && (a.CodeSeparatorIndex != nil || len(a.CodeSeparatorIndices) > 0)
}

// PublicMethods returns all public ABI methods in declaration order (which determines the selector index).
func (a *FullArtifact) PublicMethods() []ABIMethod {
	if a == nil {
		return nil
	}
	var out []ABIMethod
	for _, m := range a.ABI.Methods {
		if m.IsPublic {
			out = append(out, m)
		}
	}
	return out
}

// FindPublicMethod looks up a public method and its selector index (case-insensitive fallback for Go PascalCase).
func (a *FullArtifact) FindPublicMethod(name string) (*ABIMethod, int, error) {
	pubs := a.PublicMethods()
	for i := range pubs {
		if pubs[i].Name == name {
			return &pubs[i], i, nil
		}
	}
	for i := range pubs {
		if strings.EqualFold(pubs[i].Name, name) {
			return &pubs[i], i, nil
		}
	}
	return nil, -1, fmt.Errorf("contracts: public method %q not found in %s", name, a.ContractName)
}

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
		u = uint64(-n)
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

// EncodeNum2Bin encodes n as a fixed-width little-endian sign-magnitude byte slice (matching OP_NUM2BIN).
func EncodeNum2Bin(n int64, width int) []byte {
	buf := make([]byte, width)
	neg := n < 0
	var u uint64
	if neg {
		u = uint64(-n)
	} else {
		u = uint64(n)
	}
	for i := 0; i < width && u > 0; i++ {
		buf[i] = byte(u & 0xff)
		u >>= 8
	}
	if neg && width > 0 {
		buf[width-1] |= 0x80
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

type codeSlotSub struct {
	byteOffset int
	encoded    []byte
}

// BuildCodePart builds the deployable code portion of a Rúnar artifact by splicing
// ConstructorSlots, CodeSepIndexSlots, and CodePartLenSlots identically to runar-go's buildCodeScript.
func BuildCodePart(art *FullArtifact, ctorArgs []any) ([]byte, error) {
	if art == nil {
		return nil, errors.New("contracts: nil artifact")
	}
	template, err := hex.DecodeString(strings.TrimPrefix(art.Script, "0x"))
	if err != nil {
		return nil, fmt.Errorf("contracts: decode template script for %s: %w", art.ContractName, err)
	}
	if len(art.ConstructorSlots) == 0 && len(art.CodeSepIndexSlots) == 0 && len(art.CodePartLenSlots) == 0 {
		return template, nil
	}

	encodedCtor := make(map[int][]byte, len(ctorArgs))
	for _, slot := range art.ConstructorSlots {
		if _, ok := encodedCtor[slot.ParamIndex]; ok {
			continue
		}
		if slot.ParamIndex < 0 || slot.ParamIndex >= len(ctorArgs) {
			return nil, fmt.Errorf("contracts: %s constructorSlot paramIndex %d out of range (%d args)",
				art.ContractName, slot.ParamIndex, len(ctorArgs))
		}
		enc, err := EncodeArg(ctorArgs[slot.ParamIndex])
		if err != nil {
			return nil, fmt.Errorf("contracts: %s constructor arg %d: %w", art.ContractName, slot.ParamIndex, err)
		}
		encodedCtor[slot.ParamIndex] = enc
	}

	var subs []codeSlotSub
	for _, slot := range art.ConstructorSlots {
		subs = append(subs, codeSlotSub{
			byteOffset: slot.ByteOffset,
			encoded:    encodedCtor[slot.ParamIndex],
		})
	}

	// Resolve CodeSepIndexSlots left-to-right (ascending template byte offset), accounting for
	// constructor arg expansion AND earlier codeSepIndex slot expansions.
	if len(art.CodeSepIndexSlots) > 0 {
		sorted := append([]CodeSepIndexSlot(nil), art.CodeSepIndexSlots...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].ByteOffset < sorted[j].ByteOffset })
		type resolvedSep struct {
			templateOff int
			enc         []byte
		}
		var resolved []resolvedSep
		for _, slot := range sorted {
			shift := 0
			for _, cs := range art.ConstructorSlots {
				if cs.ByteOffset < slot.CodeSepIndex {
					shift += len(encodedCtor[cs.ParamIndex]) - 1
				}
			}
			for _, prev := range resolved {
				if prev.templateOff < slot.CodeSepIndex {
					shift += len(prev.enc) - 1
				}
			}
			enc := EncodeScriptInt(int64(slot.CodeSepIndex + shift))
			resolved = append(resolved, resolvedSep{templateOff: slot.ByteOffset, enc: enc})
			subs = append(subs, codeSlotSub{byteOffset: slot.ByteOffset, encoded: enc})
		}
	}

	// Apply substitutions in descending template byte-offset order.
	sort.Slice(subs, func(i, j int) bool { return subs[i].byteOffset > subs[j].byteOffset })
	out := append([]byte(nil), template...)
	for _, sub := range subs {
		if sub.byteOffset < 0 || sub.byteOffset >= len(out) {
			return nil, fmt.Errorf("contracts: %s slot byteOffset %d out of range (len=%d)", art.ContractName, sub.byteOffset, len(out))
		}
		next := make([]byte, 0, len(out)-1+len(sub.encoded))
		next = append(next, out[:sub.byteOffset]...)
		next = append(next, sub.encoded...)
		next = append(next, out[sub.byteOffset+1:]...)
		out = next
	}

	// Patch 4-byte LE codePartLenSlots (if any).
	if len(art.CodePartLenSlots) > 0 {
		codeLen := uint32(len(out))
		var le [4]byte
		binary.LittleEndian.PutUint32(le[:], codeLen)
		for _, slot := range art.CodePartLenSlots {
			off := slot.ByteOffset
			if off < 1 || off+7 > len(template) ||
				template[off-1] != 0x04 || template[off+4] != 0x81 || template[off+5] != 0x9c || template[off+6] != 0x69 {
				return nil, fmt.Errorf("contracts: %s invalid codePartLenSlot at %d", art.ContractName, off)
			}
			shift := 0
			for _, sub := range subs {
				if sub.byteOffset < off {
					shift += len(sub.encoded) - 1
				}
			}
			at := off + shift
			if at < 0 || at+4 > len(out) {
				return nil, fmt.Errorf("contracts: %s patched codePartLenSlot %d out of bounds", art.ContractName, at)
			}
			copy(out[at:at+4], le[:])
		}
	}

	return out, nil
}

// BuildStatelessLockingScript builds the locking script for a stateless SmartContract (e.g. DisclosureSale).
func BuildStatelessLockingScript(art *FullArtifact, ctorArgs []any) (*script.Script, error) {
	code, err := BuildCodePart(art, ctorArgs)
	if err != nil {
		return nil, err
	}
	s := script.Script(code)
	return &s, nil
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
		return EncodeNum2Bin(n, 8), nil
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

// BuildStatefulLockingScript builds <patchedCodePart> OP_RETURN <serializedState> for a StatefulSmartContract.
func BuildStatefulLockingScript(art *FullArtifact, ctorArgs []any, state map[string]any) (*script.Script, error) {
	code, err := BuildCodePart(art, ctorArgs)
	if err != nil {
		return nil, err
	}
	stateBytes, err := SerializeStateFields(art.StateFields, state)
	if err != nil {
		return nil, err
	}
	full := make([]byte, 0, len(code)+1+len(stateBytes))
	full = append(full, code...)
	full = append(full, 0x6a) // OP_RETURN
	full = append(full, stateBytes...)
	s := script.Script(full)
	return &s, nil
}

// FindFirstOpReturn walks scriptBytes at real opcode boundaries and returns the byte offset
// of the first OP_RETURN (0x6a), or -1 if none exists.
func FindFirstOpReturn(scriptBytes []byte) int {
	off := 0
	n := len(scriptBytes)
	for off < n {
		op := scriptBytes[off]
		if op == 0x6a {
			return off
		}
		next, ok := advanceScriptOpcode(scriptBytes, off)
		if !ok {
			return -1
		}
		off = next
	}
	return -1
}

// FindCodeSepOffsets walks scriptBytes at real opcode boundaries (stopping at OP_RETURN)
// and returns the byte offsets of every OP_CODESEPARATOR (0xab).
func FindCodeSepOffsets(scriptBytes []byte) []int {
	var offsets []int
	off := 0
	n := len(scriptBytes)
	for off < n {
		op := scriptBytes[off]
		if op == 0x6a {
			break
		}
		if op == 0xab {
			offsets = append(offsets, off)
			off++
			continue
		}
		next, ok := advanceScriptOpcode(scriptBytes, off)
		if !ok {
			break
		}
		off = next
	}
	return offsets
}

func advanceScriptOpcode(b []byte, off int) (int, bool) {
	if off >= len(b) {
		return off, false
	}
	op := b[off]
	switch {
	case op >= 0x01 && op <= 0x4b:
		end := off + 1 + int(op)
		return end, end <= len(b)
	case op == 0x4c:
		if off+2 > len(b) {
			return len(b), false
		}
		end := off + 2 + int(b[off+1])
		return end, end <= len(b)
	case op == 0x4d:
		if off+3 > len(b) {
			return len(b), false
		}
		l := int(binary.LittleEndian.Uint16(b[off+1 : off+3]))
		end := off + 3 + l
		return end, end <= len(b)
	case op == 0x4e:
		if off+5 > len(b) {
			return len(b), false
		}
		l := int(binary.LittleEndian.Uint32(b[off+1 : off+5]))
		end := off + 5 + l
		return end, end <= len(b)
	default:
		return off + 1, true
	}
}

// ResolveMethodCodeSepOffset returns the real on-chain OP_CODESEPARATOR byte offset in lockScript
// for methodIdx, or -1 if the artifact uses no OP_CODESEPARATOR.
func ResolveMethodCodeSepOffset(art *FullArtifact, lockScript []byte, methodIdx int) int {
	if art == nil || !art.HasCodeSeparator() {
		return -1
	}
	seps := FindCodeSepOffsets(lockScript)
	if len(seps) == 0 {
		return -1
	}
	if len(art.CodeSeparatorIndices) > 1 && methodIdx >= 0 && methodIdx < len(seps) {
		return seps[methodIdx]
	}
	return seps[0]
}

// ComputeOpPushTxPreimage computes the exact BIP-143 sighash preimage for tx.Inputs[inputIdx]
// without mutating tx.Inputs[inputIdx]'s existing SourceTxOutput.
// When codeSepOffset >= 0, scriptCode is prevLockScript[codeSepOffset+1:] (post-OP_CODESEPARATOR).
func ComputeOpPushTxPreimage(
	tx *transaction.Transaction,
	inputIdx int,
	prevLockScript []byte,
	prevSats uint64,
	codeSepOffset int,
	sigHashFlag sighash.Flag,
) ([]byte, error) {
	if tx == nil {
		return nil, errors.New("contracts: nil transaction for preimage")
	}
	if inputIdx < 0 || inputIdx >= len(tx.Inputs) {
		return nil, fmt.Errorf("contracts: inputIdx %d out of range (%d)", inputIdx, len(tx.Inputs))
	}
	if len(prevLockScript) == 0 {
		return nil, errors.New("contracts: empty prevLockScript for preimage")
	}
	if sigHashFlag == 0 {
		sigHashFlag = sighash.AllForkID
	}
	scriptCode := prevLockScript
	if codeSepOffset >= 0 {
		if codeSepOffset+1 >= len(prevLockScript) {
			return nil, fmt.Errorf("contracts: codeSepOffset %d out of range (len=%d)", codeSepOffset, len(prevLockScript))
		}
		scriptCode = prevLockScript[codeSepOffset+1:]
	}
	// Clone tx inputs/outputs shallowly via a fresh transaction header so we never clobber caller's SourceTransaction.
	tmp := &transaction.Transaction{
		Version:  tx.Version,
		LockTime: tx.LockTime,
		Inputs:   make([]*transaction.TransactionInput, len(tx.Inputs)),
		Outputs:  tx.Outputs,
	}
	for i, in := range tx.Inputs {
		cp := *in
		tmp.Inputs[i] = &cp
	}
	sc := script.Script(append([]byte(nil), scriptCode...))
	tmp.Inputs[inputIdx].SourceTransaction = nil
	tmp.Inputs[inputIdx].SetSourceTxOutput(&transaction.TransactionOutput{
		Satoshis:      prevSats,
		LockingScript: &sc,
	})
	return tmp.CalcInputPreimage(uint32(inputIdx), sigHashFlag)
}

// ComputeInputSigHash returns DSHA256(BIP-143 preimage) — the exact 32-byte digest signed by ECDSA OP_CHECKSIG.
func ComputeInputSigHash(
	tx *transaction.Transaction,
	inputIdx int,
	prevLockScript []byte,
	prevSats uint64,
	codeSepOffset int,
	sigHashFlag sighash.Flag,
) ([]byte, error) {
	preimage, err := ComputeOpPushTxPreimage(tx, inputIdx, prevLockScript, prevSats, codeSepOffset, sigHashFlag)
	if err != nil {
		return nil, err
	}
	return core.DSHA256(preimage), nil
}

// BuildMethodUnlockingScript constructs the exact push-only unlocking script for invoking methodName on art.
// For stateful methods with usesCodePart=true, pass the deployed codePart bytes (everything before OP_RETURN).
// args must match method.Params in exact ABI order (including trailing _changePKH, _changeAmount, txPreimage).
func BuildMethodUnlockingScript(art *FullArtifact, methodName string, codePart []byte, args []any) (*script.Script, error) {
	if art == nil {
		return nil, errors.New("contracts: nil artifact")
	}
	method, methodIdx, err := art.FindPublicMethod(methodName)
	if err != nil {
		return nil, err
	}
	if len(args) != len(method.Params) {
		return nil, fmt.Errorf("contracts: %s.%s expects %d args, got %d",
			art.ContractName, method.Name, len(method.Params), len(args))
	}

	var out []byte
	// Post-BUG-100 stateful prefix: ONLY _codePart (no OP_PUSH_TX signature slot).
	if method.UsesCodePart && art.HasCodeSeparator() {
		if len(codePart) == 0 {
			return nil, fmt.Errorf("contracts: %s.%s requires non-empty codePart", art.ContractName, method.Name)
		}
		out = append(out, EncodePushData(codePart)...)
	}
	for i, arg := range args {
		enc, err := EncodeArg(arg)
		if err != nil {
			return nil, fmt.Errorf("contracts: %s.%s arg[%d] (%s): %w",
				art.ContractName, method.Name, i, method.Params[i].Name, err)
		}
		out = append(out, enc...)
	}
	if len(art.PublicMethods()) > 1 {
		out = append(out, EncodeScriptInt(int64(methodIdx))...)
	}
	s := script.Script(out)
	return &s, nil
}

// ExtractUnlockPushes parses a push-only Bitcoin Script unlocking script into its raw data items.
func ExtractUnlockPushes(unlockScript []byte) ([][]byte, error) {
	var items [][]byte
	off := 0
	n := len(unlockScript)
	for off < n {
		op := unlockScript[off]
		off++
		switch {
		case op == 0x00:
			items = append(items, []byte{})
		case op >= 0x01 && op <= 0x4b:
			l := int(op)
			if off+l > n {
				return nil, errors.New("contracts: truncated direct push in unlocking script")
			}
			items = append(items, append([]byte(nil), unlockScript[off:off+l]...))
			off += l
		case op == 0x4c:
			if off+1 > n {
				return nil, errors.New("contracts: truncated OP_PUSHDATA1 len")
			}
			l := int(unlockScript[off])
			off++
			if off+l > n {
				return nil, errors.New("contracts: truncated OP_PUSHDATA1 data")
			}
			items = append(items, append([]byte(nil), unlockScript[off:off+l]...))
			off += l
		case op == 0x4d:
			if off+2 > n {
				return nil, errors.New("contracts: truncated OP_PUSHDATA2 len")
			}
			l := int(binary.LittleEndian.Uint16(unlockScript[off : off+2]))
			off += 2
			if off+l > n {
				return nil, errors.New("contracts: truncated OP_PUSHDATA2 data")
			}
			items = append(items, append([]byte(nil), unlockScript[off:off+l]...))
			off += l
		case op == 0x4e:
			if off+4 > n {
				return nil, errors.New("contracts: truncated OP_PUSHDATA4 len")
			}
			l := int(binary.LittleEndian.Uint32(unlockScript[off : off+4]))
			off += 4
			if off+l > n {
				return nil, errors.New("contracts: truncated OP_PUSHDATA4 data")
			}
			items = append(items, append([]byte(nil), unlockScript[off:off+l]...))
			off += l
		case op == 0x4f:
			items = append(items, []byte{0x81})
		case op >= 0x51 && op <= 0x60:
			items = append(items, []byte{op - 0x50})
		default:
			return nil, fmt.Errorf("contracts: non-push opcode 0x%02x in unlocking script", op)
		}
	}
	return items, nil
}

// P2PKHLockingScript returns the canonical 25-byte P2PKH locking script for a 20-byte pubkey hash.
func P2PKHLockingScript(pkh []byte) (*script.Script, error) {
	if len(pkh) != 20 {
		return nil, fmt.Errorf("contracts: P2PKH requires 20-byte hash, got %d", len(pkh))
	}
	b := make([]byte, 25)
	b[0] = 0x76 // OP_DUP
	b[1] = 0xa9 // OP_HASH160
	b[2] = 0x14 // push 20
	copy(b[3:23], pkh)
	b[23] = 0x88 // OP_EQUALVERIFY
	b[24] = 0xac // OP_CHECKSIG
	s := script.Script(b)
	return &s, nil
}

// ExtractP2PKH extracts the 20-byte pubkey hash if lockScript is a canonical 25-byte P2PKH script.
func ExtractP2PKH(lockScript []byte) ([]byte, bool) {
	if len(lockScript) == 25 &&
		lockScript[0] == 0x76 && lockScript[1] == 0xa9 && lockScript[2] == 0x14 &&
		lockScript[23] == 0x88 && lockScript[24] == 0xac {
		return append([]byte(nil), lockScript[3:23]...), true
	}
	return nil, false
}

var secp256k1N, _ = new(big.Int).SetString("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141", 16)
var secp256k1HalfN = new(big.Int).Rsh(secp256k1N, 1)

// CanonicalizeLowSDERWithFlag ensures a DER ECDSA signature satisfies BSV low-S and appends the sighash flag byte.
func CanonicalizeLowSDERWithFlag(derSig []byte, flag sighash.Flag) ([]byte, error) {
	if flag == 0 {
		flag = sighash.AllForkID
	}
	sig, err := ec.ParseDERSignature(derSig)
	if err != nil {
		return nil, fmt.Errorf("contracts: parse DER signature: %w", err)
	}
	if sig.S.Cmp(secp256k1HalfN) > 0 {
		sig.S = new(big.Int).Sub(secp256k1N, sig.S)
	}
	out := sig.Serialize()
	return append(out, byte(flag)), nil
}

// SignDigestLowS signs a 32-byte sighash with priv and returns low-S DER || sighashFlag.
func SignDigestLowS(priv *ec.PrivateKey, digest []byte, flag sighash.Flag) ([]byte, error) {
	if priv == nil || len(digest) != 32 {
		return nil, errors.New("contracts: SignDigestLowS requires non-nil key and 32-byte digest")
	}
	sig, err := priv.Sign(digest)
	if err != nil {
		return nil, err
	}
	return CanonicalizeLowSDERWithFlag(sig.Serialize(), flag)
}
