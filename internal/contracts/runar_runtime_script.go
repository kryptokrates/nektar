package contracts

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"

	"github.com/kryptokrates/nektar/internal/core"
)

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

// CanonicalizeLowSDERWithFlag validates strict DER encoding (rejecting trailing bytes and
// non-minimal encodings), enforces BSV low-S via core.ParseStrictLowSDER, and appends the sighash flag byte.
func CanonicalizeLowSDERWithFlag(derSig []byte, flag sighash.Flag) ([]byte, error) {
	if flag == 0 {
		flag = sighash.AllForkID
	}
	if len(derSig) < 8 || len(derSig) > 73 {
		return nil, fmt.Errorf("contracts: parse DER signature: %w", core.ErrNonCanonicalDER)
	}
	if sig, err := core.ParseStrictLowSDER(hex.EncodeToString(derSig)); err == nil {
		return append(sig.Serialize(), byte(flag)), nil
	} else if !errors.Is(err, core.ErrHighSSignature) {
		return nil, fmt.Errorf("contracts: parse DER signature: %w", err)
	}
	// Canonicalize valid high-S DER signature into low-S, then verify strict low-S DER output.
	sig, err := ec.ParseDERSignature(derSig)
	if err != nil || sig == nil || sig.R == nil || sig.S == nil {
		return nil, fmt.Errorf("contracts: parse DER signature: %w", core.ErrNonCanonicalDER)
	}
	if !core.EqualBytes(derSig, sig.Serialize()) || sig.R.Sign() <= 0 || sig.R.Cmp(secp256k1N) >= 0 || sig.S.Sign() <= 0 || sig.S.Cmp(secp256k1N) >= 0 {
		return nil, fmt.Errorf("contracts: parse DER signature: %w", core.ErrNonCanonicalDER)
	}
	if sig.S.Cmp(secp256k1HalfN) > 0 {
		sig.S = new(big.Int).Sub(secp256k1N, sig.S)
	}
	out := sig.Serialize()
	if _, err := core.ParseStrictLowSDER(hex.EncodeToString(out)); err != nil {
		return nil, fmt.Errorf("contracts: canonicalize low-S DER signature: %w", err)
	}
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

// NewOperatorKeySigner constructs a CovenantSighashSigner from a cold operator WIF or 32-byte hex key
// inside the allowed internal/contracts signing boundary so cmd/nektar never touches raw ec.PrivateKey.
func NewOperatorKeySigner(wif, keyHex string) (CovenantSighashSigner, error) {
	var priv *ec.PrivateKey
	switch {
	case strings.TrimSpace(wif) != "":
		var err error
		priv, err = ec.PrivateKeyFromWif(strings.TrimSpace(wif))
		if err != nil {
			return nil, fmt.Errorf("vault: invalid operator WIF: %w", err)
		}
	case strings.TrimSpace(keyHex) != "":
		raw, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(keyHex), "0x"))
		defer core.ZeroBytes(raw)
		if err != nil || len(raw) != 32 {
			return nil, errors.New("vault: operator-key-hex must be 32-byte hex")
		}
		priv, _ = ec.PrivateKeyFromBytes(raw)
		clear(raw)
		if priv == nil {
			return nil, errors.New("vault: invalid operator private key")
		}
	default:
		return nil, errors.New("vault: cold operator key required (-operator-wif / NEKTAR_OPERATOR_WIF or -operator-key-hex)")
	}
	return func(_ context.Context, _ []byte, sighash32 []byte) ([]byte, []byte, error) {
		sig, err := SignDigestLowS(priv, sighash32, sighash.AllForkID)
		if err != nil {
			return nil, nil, err
		}
		return sig, priv.PubKey().Compressed(), nil
	}, nil
}
