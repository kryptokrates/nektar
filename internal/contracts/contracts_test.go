package contracts

import (
	"bytes"
	"context"
	"encoding/hex"
	"math"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"

	"github.com/kryptokrates/nektar/internal/core"
)

func TestAnchorEncodeDecodeAndSaleBRC369(t *testing.T) {
	// 1. Verify all 9 active compiled Rúnar artifacts load cleanly from go:embed (NektarAnchorV3 deleted).
	for _, name := range ListArtifacts() {
		a, err := LoadArtifact(name)
		if err != nil || a.Script == "" {
			t.Fatalf("LoadArtifact(%s) failed: err=%v", name, err)
		}
	}

	// 2. NektarAnchor state encode -> decode roundtrip (strict META + TagRPrime only).
	priv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pubBytes := priv.PubKey().Compressed()
	pkh := core.Hash160(pubBytes)
	r0 := core.SHA256([]byte("batch0"))
	rp0 := core.ChainRp(core.Zero32Bytes(), r0, 4)

	st := AnchorState{
		Shard:    core.ShardIDFromPubkey(pubBytes),
		OwnerPub: hex.EncodeToString(pubBytes),
		OwnerPKH: pkh,
		N:        0,
		Size:     4,
		R:        r0,
		Rp:       rp0,
		Tag:      core.TagRPrime,
		Metanet:  true,
	}
	lockScript, err := EncodeAnchorLockingScript(st)
	if err != nil {
		t.Fatalf("EncodeAnchorLockingScript: %v", err)
	}
	dec, err := DecodeNektarAnchor(*lockScript)
	if err != nil {
		t.Fatalf("DecodeNektarAnchor: %v", err)
	}
	if dec.Shard != st.Shard || dec.N != 0 || dec.Size != 4 || !dec.Metanet || !core.EqualBytes(dec.Rp, rp0) {
		t.Fatalf("decoded state mismatch: %+v", dec)
	}
	// Non-Metanet and legacy TagAnchorTag scripts MUST fail closed.
	legacySt := st
	legacySt.Metanet = false
	if badScript, err := EncodeAnchorLockingScript(legacySt); err == nil {
		if _, dErr := DecodeNektarAnchor(*badScript); dErr == nil {
			t.Fatal("expected DecodeNektarAnchor to reject non-Metanet script")
		}
	}
	legacyTagSt := st
	legacyTagSt.Tag = []byte("nektar-rprime")
	if badScript, err := EncodeAnchorLockingScript(legacyTagSt); err == nil {
		if _, dErr := DecodeNektarAnchor(*badScript); dErr == nil {
			t.Fatal("expected DecodeNektarAnchor to reject legacy nektar-rprime tag")
		}
	}

	// 3. BRC-369 Keyed DisclosureSale offer -> verify -> conditional key claim!
	buyer, _ := ec.NewPrivateKey()
	buyerPub := hex.EncodeToString(buyer.PubKey().Compressed())
	secret := []byte("Verifiable alpha insight worth 500 sats")
	expUnix := uint64(time.Now().Add(time.Hour).Unix())
	leafHex := hex.EncodeToString(core.SHA256([]byte("test-fragment-leaf")))
	offer, err := CreateKeyedSaleOffer(priv, secret, 500, expUnix+3600, 100, expUnix, buyerPub, leafHex, false)
	if err != nil {
		t.Fatalf("CreateKeyedSaleOffer: %v", err)
	}
	if err := VerifySaleOffer(offer, buyerPub, leafHex); err != nil {
		t.Fatalf("VerifySaleOffer: %v", err)
	}
	// Empty leafHex MUST fail closed so offers cannot bypass mandatory buyer SPV verification.
	if err := VerifySaleOffer(offer, buyerPub, ""); err == nil {
		t.Fatal("expected VerifySaleOffer to reject empty leafHex")
	}
	k := offer.ContentKey()
	opened, err := ClaimKeyedSale(offer, k[:])
	if err != nil {
		t.Fatalf("ClaimKeyedSale: %v", err)
	}
	if string(opened) != string(secret) {
		t.Fatalf("claimed plaintext mismatch: got %q want %q", opened, secret)
	}
	// Wrong key must fail closed.
	badKey := make([]byte, 32)
	if _, err := ClaimKeyedSale(offer, badKey); err == nil {
		t.Fatal("expected ClaimKeyedSale to refuse wrong key")
	}

	// Empty CiphertextHex + CiphertextSHA256 MUST fail closed (never hash SHA256(nil)).
	emptyOffer := *offer
	emptyOffer.CiphertextHex = ""
	emptyOffer.CiphertextSHA256 = ""
	if err := VerifySaleOffer(&emptyOffer, buyerPub, leafHex); err == nil {
		t.Fatal("expected VerifySaleOffer to reject offer with empty ciphertextHex and ciphertextSha256")
	}

	// DecodeNektarAnchor MUST enforce pkhLen == 20.
	badAnchorBytes := append([]byte(nil), (*lockScript)...)
	badAnchorBytes = append(badAnchorBytes[:len(badAnchorBytes)-21], 19)
	badAnchorBytes = append(badAnchorBytes, make([]byte, 19)...)
	if _, err := DecodeNektarAnchor(badAnchorBytes); err == nil {
		t.Fatal("expected DecodeNektarAnchor to reject pkhLen != 20")
	}
}

func TestContractsHardeningRegressions(t *testing.T) {
	// 1. AgentVaultState.Validate rejects WindowStart == 0 (single-block window reset guard).
	pkh := make([]byte, 20)
	pkh[0] = 1
	st := AgentVaultState{
		OperatorPKH:  pkh,
		AgentPKH:     pkh,
		PerTxCap:     1000,
		WindowCap:    2000,
		WindowBlocks: 144,
		MaxFee:       500,
		WindowStart:  0,
	}
	if err := st.Validate(); err == nil {
		t.Fatal("expected AgentVaultState.Validate to reject WindowStart == 0")
	}
	st.WindowStart = 100
	if err := st.ValidateForHeight(150); err != nil {
		t.Fatalf("expected valid WindowStart=100 at height 150, got %v", err)
	}

	// 2. EncodeNum2Bin handles math.MinInt64 without overflow to -0, and state serialization fails closed.
	encMin := EncodeNum2Bin(math.MinInt64, 8)
	if bytes.Equal(encMin, []byte{0, 0, 0, 0, 0, 0, 0, 0x80}) {
		t.Fatalf("EncodeNum2Bin(MinInt64, 8) overflowed to -0: %x", encMin)
	}
	if _, err := encodeNum2BinChecked(math.MinInt64, 8); err == nil {
		t.Fatal("expected encodeNum2BinChecked(MinInt64, 8) to reject 8-byte overflow")
	}
	encMin9 := EncodeNum2Bin(math.MinInt64, 9)
	if len(encMin9) != 9 || encMin9[8] != 0x80 || encMin9[7] != 0x80 {
		t.Fatalf("unexpected EncodeNum2Bin(MinInt64, 9): %x", encMin9)
	}

	// 3. CanonicalizeLowSDERWithFlag rejects trailing DER bytes.
	k, _ := ec.NewPrivateKey()
	sig, _ := k.Sign(core.SHA256([]byte("der-check")))
	trailing := append(sig.Serialize(), 0xde, 0xad)
	if _, err := CanonicalizeLowSDERWithFlag(trailing, sighash.AllForkID); err == nil {
		t.Fatal("expected CanonicalizeLowSDERWithFlag to reject trailing DER bytes")
	}

	// 4. VerifyInputScriptChronicle enforces VerifyCleanStack on pre-broadcast relay checks.
	opTrue := script.Script{0x51}
	dirtyUnlock := script.Script{0x51, 0x51} // leaves 2 items on stack -> fails VerifyCleanStack
	prevTx := transaction.NewTransaction()
	prevTx.AddOutput(&transaction.TransactionOutput{Satoshis: 1000, LockingScript: &opTrue})
	spendTx := transaction.NewTransaction()
	spendTx.AddInput(&transaction.TransactionInput{
		SourceTXID:        prevTx.TxID(),
		SourceTxOutIndex:  0,
		SourceTransaction: prevTx,
		UnlockingScript:   &dirtyUnlock,
		SequenceNumber:    0xffffffff,
	})
	spendTx.AddOutput(&transaction.TransactionOutput{Satoshis: 900, LockingScript: &opTrue})
	if err := VerifyInputScriptChronicle(spendTx, 0, prevTx.Outputs[0]); err == nil {
		t.Fatal("expected VerifyInputScriptChronicle (VerifyCleanStack) to reject dirty stack")
	}
	if err := VerifyInputScriptChronicleConsensus(spendTx, 0, prevTx.Outputs[0]); err != nil {
		t.Fatalf("expected mined-block VerifyInputScriptChronicleConsensus to allow post-Genesis non-cleanstack, got %v", err)
	}

	// 5. VerifyFragmentWithMemo rejects mismatched AnchorTxID or lower-depth memo reuse.
	memo := &AnchorBatchMemo{
		State:      &AnchorState{Shard: "s1", N: 0, Size: 1, R: core.Zero32Bytes()},
		AnchorTxID: "aaaa",
		Result:     VerifyResult{OK: true, Settled: true, Depth: 1},
	}
	vr, _ := VerifyFragmentWithMemo(context.Background(), ProofBundle{
		Shard:  "s1",
		N:      0,
		Size:   1,
		Anchor: AnchorLink{TxID: "bbbb"},
	}, nil, 1, memo)
	if vr.OK {
		t.Fatal("expected VerifyFragmentWithMemo to reject mismatched AnchorTxID")
	}
	vrDepth, _ := VerifyFragmentWithMemo(context.Background(), ProofBundle{
		Shard:  "s1",
		N:      0,
		Size:   1,
		Anchor: AnchorLink{TxID: "aaaa"},
	}, nil, 6, memo)
	if vrDepth.OK {
		t.Fatal("expected VerifyFragmentWithMemo to reject memo with Depth 1 < minDepth 6")
	}

	// 6. BuildBondCommitTx and BuildBondCloseTx reject minerFeeSats == MaxFee+1.
	opPKH := core.Hash160(k.PubKey().Compressed())
	bondLock, err := EncodeCheckpointBondLockingScript(CheckpointBondState{
		OperatorPKH:       opPKH,
		BondReleaseHeight: 300,
		MaxFee:            500,
		EpochLen:          50,
		N:                 0,
		Deadline:          150,
	})
	if err != nil {
		t.Fatal(err)
	}
	bondTx := transaction.NewTransaction()
	bondTx.AddOutput(&transaction.TransactionOutput{Satoshis: 10_000, LockingScript: bondLock})
	if _, _, err := BuildBondCommitTx(context.Background(), bondTx, 0, core.SHA256([]byte("r")), 120, 501, privKeySigner(k)); err == nil {
		t.Fatal("expected BuildBondCommitTx to reject minerFeeSats == MaxFee+1")
	}
	if _, err := BuildBondCloseTx(context.Background(), bondTx, 0, 300, 501, privKeySigner(k)); err == nil {
		t.Fatal("expected BuildBondCloseTx to reject minerFeeSats == MaxFee+1")
	}
}
