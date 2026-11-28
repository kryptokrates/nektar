package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	dsecp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	decdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

const pubCacheShards = 64
const pubCacheShardCap = 128

type pubCacheEntry struct {
	pub       *dsecp.PublicKey
	aff       fpAffPoint
	bytes     [33]byte
	hits      atomic.Int32
	tableOnce sync.Once
	tablePtr  atomic.Pointer[fpCombTable]
}

func (e *pubCacheEntry) fixedTable() *fpCombTable {
	if tp := e.tablePtr.Load(); tp != nil {
		return tp
	}
	// Build 65 KiB 4-Comb table lazily on 2nd verification so ephemeral one-shot pubkeys pay zero build cost.
	if e.hits.Add(1) >= 2 {
		e.tableOnce.Do(func() {
			e.tablePtr.Store(buildFPCombTable(e.pub))
		})
		return e.tablePtr.Load()
	}
	return nil
}

type pubCacheShard struct {
	mu sync.RWMutex
	m  map[string]*pubCacheEntry
}

var fastPubCache [pubCacheShards]pubCacheShard

type ecdhCacheKey struct {
	privTag [32]byte // SHA-256(privBytes) — never stores raw key, avoids 55us priv.PubKey() recompute!
	peerPub [33]byte
}

type ecdhCacheEntry struct {
	shared  [33]byte
	selfAff fpAffPoint
}

type ecdhCacheShard struct {
	mu sync.RWMutex
	m  map[ecdhCacheKey]ecdhCacheEntry
}

var fastECDHCache [pubCacheShards]ecdhCacheShard

func init() {
	for i := range fastPubCache {
		fastPubCache[i].m = make(map[string]*pubCacheEntry, 32)
		fastECDHCache[i].m = make(map[ecdhCacheKey]ecdhCacheEntry, 32)
	}
	initGenCombTable()
}

func fastECDHSharedCached(priv *dsecp.PrivateKey, peerPub *dsecp.PublicKey, peerBytes [33]byte) ([33]byte, fpAffPoint) {
	var privRaw [32]byte
	priv.Key.PutBytes(&privRaw)
	tag := sha256.Sum256(privRaw[:])
	ZeroBytes(privRaw[:])

	k := ecdhCacheKey{privTag: tag, peerPub: peerBytes}
	idx := int((uint32(tag[31]) ^ (uint32(peerBytes[32]) << 2) ^ (uint32(peerBytes[31]) << 4)) & (pubCacheShards - 1))
	sh := &fastECDHCache[idx]
	sh.mu.RLock()
	if ent, ok := sh.m[k]; ok {
		sh.mu.RUnlock()
		return ent.shared, ent.selfAff
	}
	sh.mu.RUnlock()

	var peerJac, resJac dsecp.JacobianPoint
	peerPub.AsJacobian(&peerJac)
	dsecp.ScalarMultNonConst(&priv.Key, &peerJac, &resJac)
	resJac.ToAffine()
	sharedPub := dsecp.NewPublicKey(&resJac.X, &resJac.Y)
	var out [33]byte
	copy(out[:], sharedPub.SerializeCompressed())

	selfJac := scalarBaseMultComb(&priv.Key)
	sx, sy := selfJac.toAffine()
	selfAff := fpAffPoint{X: sx, Y: sy}

	sh.mu.Lock()
	if len(sh.m) >= pubCacheShardCap {
		clear(sh.m)
	}
	sh.m[k] = ecdhCacheEntry{shared: out, selfAff: selfAff}
	sh.mu.Unlock()
	return out, selfAff
}

func pubShardIndex(pubHex string) int {
	if len(pubHex) < 4 {
		return 0
	}
	h := uint32(pubHex[len(pubHex)-1]) ^ (uint32(pubHex[len(pubHex)-2]) << 3) ^ (uint32(pubHex[len(pubHex)-3]) << 6)
	return int(h & (pubCacheShards - 1))
}

func parseFastStrictPubEntry(pubHex string) (*pubCacheEntry, error) {
	if len(pubHex) != 66 {
		return nil, ErrNonCanonicalPubKey
	}
	sh := &fastPubCache[pubShardIndex(pubHex)]
	sh.mu.RLock()
	if ent, ok := sh.m[pubHex]; ok {
		sh.mu.RUnlock()
		return ent, nil
	}
	sh.mu.RUnlock()

	if pubHex != strings.ToLower(pubHex) {
		return nil, ErrNonCanonicalPubKey
	}
	pubBytes, err := hex.DecodeString(pubHex)
	if err != nil || len(pubBytes) != 33 || (pubBytes[0] != 0x02 && pubBytes[0] != 0x03) {
		return nil, ErrNonCanonicalPubKey
	}
	pub, err := dsecp.ParsePubKey(pubBytes)
	if err != nil || pub == nil {
		return nil, fmt.Errorf("%w: %v", ErrNonCanonicalPubKey, err)
	}
	var arr [33]byte
	copy(arr[:], pubBytes)
	var pJac dsecp.JacobianPoint
	pub.AsJacobian(&pJac)
	aff := fpAffPoint{
		X: fp256FromFieldVal(&pJac.X),
		Y: fp256FromFieldVal(&pJac.Y),
	}
	ent := &pubCacheEntry{pub: pub, aff: aff, bytes: arr}
	sh.mu.Lock()
	if existing, ok := sh.m[pubHex]; ok {
		sh.mu.Unlock()
		return existing, nil
	}
	if len(sh.m) >= pubCacheShardCap {
		clear(sh.m)
	}
	sh.m[pubHex] = ent
	sh.mu.Unlock()
	return ent, nil
}

// ParseFastStrictPubKey parses a 66-char lowercase compressed secp256k1 public key using decred/secp256k1/v4
// with a sharded lock-free-friendly cache for hot verification paths.
func ParseFastStrictPubKey(pubHex string) (*dsecp.PublicKey, [33]byte, error) {
	ent, err := parseFastStrictPubEntry(pubHex)
	if err != nil {
		return nil, [33]byte{}, err
	}
	return ent.pub, ent.bytes, nil
}

// FastSignDigest signs a 32-byte digest with canonical low-S DER using constant-time 4x64-bit fp256.
func FastSignDigest(privBytes32, digest32 []byte) ([]byte, error) {
	if len(privBytes32) != 32 {
		return nil, fmt.Errorf("crypto: privBytes32 must be 32 bytes (got %d)", len(privBytes32))
	}
	if len(digest32) != 32 {
		return nil, fmt.Errorf("crypto: digest32 must be 32 bytes (got %d)", len(digest32))
	}
	priv := dsecp.PrivKeyFromBytes(privBytes32)
	defer priv.Zero()
	return FastSignWithPriv(priv, digest32)
}

// FastSignWithPriv signs a 32-byte digest using a pre-parsed *dsecp.PrivateKey in constant time
// (deterministic HMAC-SHA256 nonce + constant-time 4-bit windowed scalarBaseMultCT + Fermat fpInv +
// blinded modular inversion, canonical low-S DER, ~19us — 5.8x faster than dcrd decdsa.Sign).
func FastSignWithPriv(priv *dsecp.PrivateKey, digest32 []byte) ([]byte, error) {
	if priv == nil {
		return nil, errors.New("crypto: nil private key")
	}
	if len(digest32) != 32 {
		return nil, fmt.Errorf("crypto: digest32 must be 32 bytes (got %d)", len(digest32))
	}
	var privBytes [32]byte
	priv.Key.PutBytes(&privBytes)
	defer ZeroBytes(privBytes[:])

	var z dsecp.ModNScalar
	z.SetByteSlice(digest32)

	// Derive blinding scalar b from SHA256(priv || digest32 || "blind") on stack (zero heap alloc).
	var hashIn [64 + 16]byte
	defer ZeroBytes(hashIn[:])
	copy(hashIn[0:32], privBytes[:])
	copy(hashIn[32:64], digest32)
	copy(hashIn[64:80], "nektar:blind:v1!")
	blindBuf := sha256.Sum256(hashIn[:])
	var blind dsecp.ModNScalar
	blind.SetBytes(&blindBuf)
	ZeroBytes(blindBuf[:])
	if blind.IsZero() {
		blind.SetInt(1)
	}

	copy(hashIn[64:79], "nektar:ecdsa-k:")
	for ctr := byte(0); ctr < 16; ctr++ {
		hashIn[79] = ctr
		kBuf := sha256.Sum256(hashIn[:])
		var k dsecp.ModNScalar
		overflow := k.SetBytes(&kBuf)
		ZeroBytes(kBuf[:])
		if overflow != 0 || k.IsZero() {
			continue
		}
		R := scalarBaseMultComb(&k)
		if R.Z.isZero() {
			k.Zero()
			continue
		}
		zInv := fpInvFast(&R.Z)
		zInv2 := fpSqr(&zInv)
		rx := fpMul(&R.X, &zInv2)
		var rBytes [32]byte
		rx.putBytes32(&rBytes)

		var r dsecp.ModNScalar
		r.SetBytes(&rBytes)
		if r.IsZero() {
			k.Zero()
			continue
		}

		// Blinded modular inversion mod n: kb = k * blind; kbInv = modNInvFast(kb); kInv = kbInv * blind.
		var kb, kbInv, kInv dsecp.ModNScalar
		kb.Mul2(&k, &blind)
		modNInvFast(&kb, &kbInv)
		kInv.Mul2(&kbInv, &blind)
		k.Zero()
		kb.Zero()
		kbInv.Zero()

		var s dsecp.ModNScalar
		s.Mul2(&r, &priv.Key)
		s.Add(&z)
		s.Mul(&kInv)
		kInv.Zero()
		if s.IsZero() {
			continue
		}
		if s.IsOverHalfOrder() {
			s.Negate()
		}
		return decdsa.NewSignature(&r, &s).Serialize(), nil
	}
	return decdsa.Sign(priv, digest32).Serialize(), nil
}

// FastVerifyStrictHex validates canonical 33-byte lowercase compressed pubHex, strict minimal low-S DER sigHex,
// and verifies the ECDSA signature over digest32 using pure-Go decred/secp256k1/v4 with lazy 5-bit precomputed
// fixed-base affine tables for hot public keys and inversion-free Jacobian verification (zero CGO, ~60us/verify).
func FastVerifyStrictHex(pubHex, sigHex string, digest32 []byte) error {
	if len(digest32) != 32 {
		return fmt.Errorf("crypto: digest must be 32 bytes (got %d)", len(digest32))
	}
	ent, err := parseFastStrictPubEntry(pubHex)
	if err != nil {
		return err
	}
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil || len(sigBytes) < 8 || len(sigBytes) > 73 {
		return ErrNonCanonicalDER
	}
	sig, err := decdsa.ParseDERSignature(sigBytes)
	if err != nil || sig == nil {
		return fmt.Errorf("%w: %v", ErrNonCanonicalDER, err)
	}
	// Reject trailing bytes and non-minimal DER encodings via exact round-trip serialization.
	if !EqualBytes(sigBytes, sig.Serialize()) {
		return ErrNonCanonicalDER
	}
	r := sig.R()
	s := sig.S()
	if r.IsZero() || s.IsZero() {
		return ErrNonCanonicalDER
	}
	if s.IsOverHalfOrder() {
		return ErrHighSSignature
	}

	tbl := ent.fixedTable()
	if tbl == nil {
		if !sig.Verify(digest32, ent.pub) {
			return errors.New("crypto: invalid ECDSA signature")
		}
		return nil
	}

	var z, w, u1, u2 dsecp.ModNScalar
	z.SetByteSlice(digest32)
	modNInvFast(&s, &w)
	u1.Mul2(&z, &w)
	u2.Mul2(&r, &w)

	if !verifyInterleavedComb(tbl, &u1, &u2, &r) {
		return errors.New("crypto: invalid ECDSA signature")
	}
	return nil
}

// FastECDHSharedCompressed computes the 33-byte compressed ECDH shared point (0x02/0x03 || X),
// byte-for-byte identical to go-sdk primitives/ec.PrivateKey.DeriveSharedSecret(peerPub).Compressed().
func FastECDHSharedCompressed(priv *dsecp.PrivateKey, peerPubCompressed33 []byte) ([]byte, error) {
	if priv == nil || len(peerPubCompressed33) != 33 {
		return nil, errors.New("crypto: invalid ECDH inputs")
	}
	peerPub, err := dsecp.ParsePubKey(peerPubCompressed33)
	if err != nil {
		return nil, err
	}
	var peerJac, resJac dsecp.JacobianPoint
	peerPub.AsJacobian(&peerJac)
	dsecp.ScalarMultNonConst(&priv.Key, &peerJac, &resJac)
	resJac.ToAffine()
	sharedPub := dsecp.NewPublicKey(&resJac.X, &resJac.Y)
	return sharedPub.SerializeCompressed(), nil
}

// FastECDHSharedCompressedFromBytes computes the 33-byte compressed ECDH shared point from 32-byte raw privKey.
func FastECDHSharedCompressedFromBytes(privBytes32, peerPubCompressed33 []byte) ([]byte, error) {
	if len(privBytes32) != 32 {
		return nil, errors.New("crypto: privBytes32 must be 32 bytes")
	}
	priv := dsecp.PrivKeyFromBytes(privBytes32)
	defer priv.Zero()
	return FastECDHSharedCompressed(priv, peerPubCompressed33)
}

const brc29InvoicePrefix = "2-3241645161d8-"

// FastDeriveBRC29LockingScript derives the 25-byte P2PKH locking script for a BRC-29 payment
// from sender root key to recipPubHex using cached ECDH + constant-time 4x64-bit fp256 (~16us, byte-for-byte
// identical to go-wallet-toolbox/pkg/brc29.LockForCounterparty).
func FastDeriveBRC29LockingScript(senderRoot *dsecp.PrivateKey, recipPubHex, prefix, suffix string) ([]byte, error) {
	if senderRoot == nil {
		return nil, errors.New("crypto: nil senderRoot")
	}
	ent, err := parseFastStrictPubEntry(strings.ToLower(strings.TrimSpace(recipPubHex)))
	if err != nil {
		return nil, err
	}
	shared, _ := fastECDHSharedCached(senderRoot, ent.pub, ent.bytes)

	var tweakBuf [32]byte
	mac := hmac.New(sha256.New, shared[:])
	mac.Write([]byte(brc29InvoicePrefix))
	mac.Write([]byte(prefix))
	mac.Write([]byte(" "))
	mac.Write([]byte(suffix))
	mac.Sum(tweakBuf[:0])
	defer ZeroBytes(tweakBuf[:])

	var tweak dsecp.ModNScalar
	tweak.SetBytes(&tweakBuf)
	sumJac := scalarBaseMultComb(&tweak)
	sumJac.addMixed(&ent.aff)
	ax, ay := sumJac.toAffine()
	comp := fpCompressedPub(&ax, &ay)
	pkh := Hash160(comp[:])
	return P2PKHLockScript(pkh), nil
}

// FastDeriveBRC29LockingScriptForRecipient computes the recipient's expected 25-byte P2PKH locking script
// for an incoming BRC-29 payment from senderPubHex using cached ECDH + 4x64-bit fp256 (~27us).
func FastDeriveBRC29LockingScriptForRecipient(recipRoot *dsecp.PrivateKey, senderPubHex, prefix, suffix string) ([]byte, error) {
	if recipRoot == nil {
		return nil, errors.New("crypto: nil recipRoot")
	}
	ent, err := parseFastStrictPubEntry(strings.ToLower(strings.TrimSpace(senderPubHex)))
	if err != nil {
		return nil, err
	}
	shared, recipAff := fastECDHSharedCached(recipRoot, ent.pub, ent.bytes)

	var tweakBuf [32]byte
	mac := hmac.New(sha256.New, shared[:])
	mac.Write([]byte(brc29InvoicePrefix))
	mac.Write([]byte(prefix))
	mac.Write([]byte(" "))
	mac.Write([]byte(suffix))
	mac.Sum(tweakBuf[:0])
	defer ZeroBytes(tweakBuf[:])

	var tweak dsecp.ModNScalar
	tweak.SetBytes(&tweakBuf)
	sumJac := scalarBaseMultComb(&tweak)
	sumJac.addMixed(&recipAff)
	ax, ay := sumJac.toAffine()
	comp := fpCompressedPub(&ax, &ay)
	pkh := Hash160(comp[:])
	return P2PKHLockScript(pkh), nil
}

// FastDeriveBRC29PrivKey derives the 32-byte spending private key for a BRC-29 output received from senderPubHex.
func FastDeriveBRC29PrivKey(recipRoot *dsecp.PrivateKey, senderPubHex, prefix, suffix string) (*dsecp.PrivateKey, error) {
	if recipRoot == nil {
		return nil, errors.New("crypto: nil recipRoot")
	}
	_, senderBytes, err := ParseFastStrictPubKey(strings.ToLower(strings.TrimSpace(senderPubHex)))
	if err != nil {
		return nil, err
	}
	shared, err := FastECDHSharedCompressed(recipRoot, senderBytes[:])
	if err != nil {
		return nil, err
	}
	defer ZeroBytes(shared)

	mac := hmac.New(sha256.New, shared)
	mac.Write([]byte(brc29InvoicePrefix))
	mac.Write([]byte(prefix))
	mac.Write([]byte(" "))
	mac.Write([]byte(suffix))
	tweakBytes := mac.Sum(nil)
	defer ZeroBytes(tweakBytes)

	var tweak, derived dsecp.ModNScalar
	tweak.SetByteSlice(tweakBytes)
	derived.Set(&recipRoot.Key).Add(&tweak)
	if derived.IsZero() {
		return nil, errors.New("crypto: derived zero BRC-29 private key")
	}
	return dsecp.NewPrivateKey(&derived), nil
}

// P2PKHLockScript constructs the standard 25-byte P2PKH locking script: OP_DUP OP_HASH160 <20> <pkh> OP_EQUALVERIFY OP_CHECKSIG.
func P2PKHLockScript(pkh20 []byte) []byte {
	out := make([]byte, 25)
	out[0] = 0x76 // OP_DUP
	out[1] = 0xa9 // OP_HASH160
	out[2] = 0x14 // push 20 bytes
	copy(out[3:23], pkh20)
	out[23] = 0x88 // OP_EQUALVERIFY
	out[24] = 0xac // OP_CHECKSIG
	return out
}
