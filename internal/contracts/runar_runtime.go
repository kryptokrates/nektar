package contracts

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/bsv-blockchain/go-sdk/script"
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
