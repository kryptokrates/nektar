package core

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"runtime"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

var (
	halfSecp256k1N = new(big.Int).Rsh(ec.S256().N, 1)

	ErrNonCanonicalPubKey = errors.New("crypto: public key must be 33-byte lowercase compressed secp256k1 hex")
	ErrNonCanonicalDER    = errors.New("crypto: non-canonical DER signature (trailing bytes or malformed encoding)")
	ErrHighSSignature     = errors.New("crypto: malleable high-S ECDSA signature rejected")
)

// ParseStrictCompressedPubKey parses a canonical 66-character lowercase hex compressed
// secp256k1 public key (0x02/0x03 prefix), rejecting 65-byte uncompressed/hybrid keys and
// case-variant bypasses of string-keyed maps and rate limiters.
func ParseStrictCompressedPubKey(pubHex string) (*ec.PublicKey, []byte, error) {
	pub, arr, err := ParseFastStrictPubKey(pubHex)
	if err != nil {
		return nil, nil, err
	}
	outBytes := make([]byte, 33)
	copy(outBytes, arr[:])
	return &ec.PublicKey{Curve: ec.S256(), X: pub.X(), Y: pub.Y()}, outBytes, nil
}

// ParseStrictLowSDER parses a DER-encoded ECDSA signature while enforcing:
//  1. Exact DER byte length (rejects trailing bytes silently truncated by go-sdk ParseDERSignature),
//  2. Non-zero R and S within [1, N-1], and
//  3. Low-S canonicality (S <= N/2, BIP-62 / BIP-146) to prevent signature malleability.
func ParseStrictLowSDER(sigHex string) (*ec.Signature, error) {
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil || len(sigBytes) < 8 || len(sigBytes) > 73 {
		return nil, ErrNonCanonicalDER
	}
	sig, err := ec.ParseDERSignature(sigBytes)
	if err != nil || sig == nil || sig.R == nil || sig.S == nil {
		return nil, fmt.Errorf("%w: %v", ErrNonCanonicalDER, err)
	}
	// Reject trailing bytes and non-minimal DER encodings by checking round-trip serialization.
	canon := sig.Serialize()
	if !EqualBytes(sigBytes, canon) {
		return nil, ErrNonCanonicalDER
	}
	if sig.R.Sign() <= 0 || sig.R.Cmp(ec.S256().N) >= 0 {
		return nil, ErrNonCanonicalDER
	}
	if sig.S.Sign() <= 0 || sig.S.Cmp(halfSecp256k1N) > 0 {
		return nil, ErrHighSSignature
	}
	return sig, nil
}

// ZeroBytes overwrites b with zeros and prevents dead-store elimination.
func ZeroBytes(b []byte) {
	if len(b) == 0 {
		return
	}
	clear(b)
	runtime.KeepAlive(b)
}

// Zero32Array overwrites a 32-byte secret array with zeros.
func Zero32Array(b *[32]byte) {
	if b == nil {
		return
	}
	clear(b[:])
	runtime.KeepAlive(b)
}
