package core

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/brc29"
	dsecp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	decdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

func TestFastSecp256k1_CrossCompatibilityWithGoSDKAndBRC29(t *testing.T) {
	for i := 0; i < 32; i++ {
		senderSDK, err := ec.NewPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		recipSDK, err := ec.NewPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		var senderRaw [32]byte
		sb := senderSDK.Serialize()
		copy(senderRaw[32-len(sb):], sb)
		var recipRaw [32]byte
		rb := recipSDK.Serialize()
		copy(recipRaw[32-len(rb):], rb)

		senderFast := dsecp.PrivKeyFromBytes(senderRaw[:])
		recipFast := dsecp.PrivKeyFromBytes(recipRaw[:])
		senderPubHex := hex.EncodeToString(senderSDK.PubKey().Compressed())
		recipPubHex := hex.EncodeToString(recipSDK.PubKey().Compressed())

		// 1. Cross-verify Sign & Verify in both directions.
		digest := DSHA256([]byte("nektar-fast-crypto-test"))
		fastDER, err := FastSignWithPriv(senderFast, digest)
		if err != nil {
			t.Fatalf("FastSignWithPriv: %v", err)
		}
		sdkSig, err := ec.ParseDERSignature(fastDER)
		if err != nil {
			t.Fatalf("go-sdk ParseDERSignature on fast sig: %v", err)
		}
		if !sdkSig.Verify(digest, senderSDK.PubKey()) {
			t.Fatalf("go-sdk failed to verify FastSignWithPriv signature")
		}
		if err := FastVerifyStrictHex(senderPubHex, hex.EncodeToString(fastDER), digest); err != nil {
			t.Fatalf("FastVerifyStrictHex rejected own sig: %v", err)
		}
		// Sign with go-sdk, verify with FastVerifyStrictHex.
		sig2, err := senderSDK.Sign(digest)
		if err != nil {
			t.Fatal(err)
		}
		if err := FastVerifyStrictHex(senderPubHex, hex.EncodeToString(sig2.Serialize()), digest); err != nil {
			t.Fatalf("FastVerifyStrictHex rejected go-sdk sig: %v", err)
		}
		// Reject trailing DER bytes.
		if err := FastVerifyStrictHex(senderPubHex, hex.EncodeToString(append(fastDER, 0x00)), digest); err == nil {
			t.Fatalf("FastVerifyStrictHex accepted trailing DER byte")
		}

		// 2. Cross-verify 33-byte compressed ECDH shared secret.
		sdkShared, err := senderSDK.DeriveSharedSecret(recipSDK.PubKey())
		if err != nil {
			t.Fatal(err)
		}
		fastShared, err := FastECDHSharedCompressed(senderFast, recipSDK.PubKey().Compressed())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(sdkShared.Compressed(), fastShared) {
			t.Fatalf("ECDH mismatch:\n sdk=%x\nfast=%x", sdkShared.Compressed(), fastShared)
		}

		// 3. Cross-verify BRC-29 locking script & derived recipient private key!
		var pre, suf [16]byte
		_, _ = rand.Read(pre[:])
		_, _ = rand.Read(suf[:])
		preB64 := base64.StdEncoding.EncodeToString(pre[:])
		sufB64 := base64.StdEncoding.EncodeToString(suf[:])

		sdkLock, err := brc29.LockForCounterparty(senderSDK, brc29.KeyID{
			DerivationPrefix: preB64,
			DerivationSuffix: sufB64,
		}, recipSDK.PubKey(), brc29.WithTestNet())
		if err != nil {
			t.Fatalf("brc29.LockForCounterparty: %v", err)
		}
		fastLock, err := FastDeriveBRC29LockingScript(senderFast, recipPubHex, preB64, sufB64)
		if err != nil {
			t.Fatalf("FastDeriveBRC29LockingScript: %v", err)
		}
		if !bytes.Equal(*sdkLock, fastLock) {
			t.Fatalf("BRC-29 lock script mismatch:\n sdk=%x\nfast=%x", *sdkLock, fastLock)
		}

		derivedPriv, err := FastDeriveBRC29PrivKey(recipFast, senderPubHex, preB64, sufB64)
		if err != nil {
			t.Fatalf("FastDeriveBRC29PrivKey: %v", err)
		}
		derivedLock := P2PKHLockScript(Hash160(derivedPriv.PubKey().SerializeCompressed()))
		if !bytes.Equal(derivedLock, fastLock) {
			t.Fatalf("BRC-29 derived recipient privkey lock script mismatch:\nexpect=%x\n   got=%x", fastLock, derivedLock)
		}
	}
}

func BenchmarkFastCrypto(b *testing.B) {
	priv, _ := dsecp.GeneratePrivateKey()
	peer, _ := dsecp.GeneratePrivateKey()
	pubHex := hex.EncodeToString(priv.PubKey().SerializeCompressed())
	peerHex := hex.EncodeToString(peer.PubKey().SerializeCompressed())
	digest := DSHA256([]byte("bench-digest"))
	sigBytes, _ := FastSignWithPriv(priv, digest)
	sigHex := hex.EncodeToString(sigBytes)
	sigParsed, _ := decdsa.ParseDERSignature(sigBytes)
	// Warm up fixed table
	_ = FastVerifyStrictHex(pubHex, sigHex, digest)
	_ = FastVerifyStrictHex(pubHex, sigHex, digest)

	b.Run("dcrd_Sign", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = decdsa.Sign(priv, digest).Serialize()
		}
	})
	b.Run("FastSignWithPriv", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _ = FastSignWithPriv(priv, digest)
		}
	})
	b.Run("dcrd_Verify", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = sigParsed.Verify(digest, priv.PubKey())
		}
	})
	b.Run("FastVerifyStrictHex_Hot", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = FastVerifyStrictHex(pubHex, sigHex, digest)
		}
	})
	b.Run("FastDeriveBRC29", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _ = FastDeriveBRC29LockingScript(priv, peerHex, "pre", "suf")
		}
	})
}
