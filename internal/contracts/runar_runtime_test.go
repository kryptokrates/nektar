package contracts

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"

	"github.com/kryptokrates/nektar/internal/core"
)

func privKeySigner(keys ...*ec.PrivateKey) CovenantSighashSigner {
	return func(_ context.Context, targetPKH []byte, sighash32 []byte) ([]byte, []byte, error) {
		for _, k := range keys {
			comp := k.PubKey().Compressed()
			if bytes.Equal(core.Hash160(comp), targetPKH) {
				sig, err := SignDigestLowS(k, sighash32, sighash.AllForkID)
				return sig, comp, err
			}
		}
		// Fallback to first key for adversarial wrong-key tests.
		k := keys[0]
		sig, err := SignDigestLowS(k, sighash32, sighash.AllForkID)
		return sig, k.PubKey().Compressed(), err
	}
}

// TestNektarAnchorChronicleVM executes real post-Chronicle Bitcoin Script VM verification
// across genesis -> Append(batch 1) -> Append(batch 2) -> Rotate(newOwner) -> Append(batch 3),
// plus adversarial negative tests (wrong signer, forged R', skipped n, old owner after Rotate).
func TestNektarAnchorChronicleVM(t *testing.T) {
	ctx := context.Background()
	owner1, _ := ec.NewPrivateKey()
	owner2, _ := ec.NewPrivateKey()
	attacker, _ := ec.NewPrivateKey()
	fuelKey, _ := ec.NewPrivateKey()

	pub1 := owner1.PubKey().Compressed()
	pkh1 := core.Hash160(pub1)
	pkh2 := core.Hash160(owner2.PubKey().Compressed())
	changePKH := core.Hash160(fuelKey.PubKey().Compressed())
	changeLock, _ := P2PKHLockingScript(changePKH)

	// Genesis anchor (n=0) with consensus-enforced StateHash Q_0 and MMR root.
	r0 := core.SHA256([]byte("batch-0-root"))
	rp0 := core.ChainRp(core.Zero32Bytes(), r0, 3)
	mmr, _ := core.NewMMR([][]byte{r0})
	meta0 := core.ComputeBatchMetaHash(core.SHA256([]byte("tree-0")), core.SHA256([]byte("pol-0")), core.SHA256([]byte("ont-0")))
	q0 := core.ChainStateQ(core.Zero32Bytes(), r0, meta0, mmr.Root())
	st0 := AnchorState{
		Shard:     core.ShardIDFromPubkey(pub1),
		OwnerPub:  hex.EncodeToString(pub1),
		OwnerPKH:  pkh1,
		N:         0,
		Size:      3,
		R:         r0,
		Rp:        rp0,
		StateHash: q0,
		MmrRoot:   mmr.Root(),
		Tag:       core.TagRPrime,
		Metanet:   true,
	}
	lock0, err := EncodeAnchorLockingScript(st0)
	if err != nil {
		t.Fatalf("EncodeAnchorLockingScript(0): %v", err)
	}
	if !IsSpendableNektarAnchorScript(*lock0) {
		t.Fatal("expected newly encoded NektarAnchor to be spendable")
	}
	tx0 := transaction.NewTransaction()
	tx0.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: lock0})

	// Helper building an anchor spend tx (Input 0 = prevAnchor:0, Input 1 = dummy fuel UTXO, Output 0 = nextAnchor, Output 1 = change).
	fuelTx := transaction.NewTransaction()
	fuelTx.AddOutput(&transaction.TransactionOutput{Satoshis: 100_000, LockingScript: changeLock})

	buildAppend := func(prevTx *transaction.Transaction, prevSt AnchorState, newR []byte, newSize uint64, batchMeta, newMmrRoot []byte, signerKey *ec.PrivateKey) (*transaction.Transaction, AnchorState, error) {
		nextSt, err := NextAnchorState(prevSt, newR, newSize, batchMeta, newMmrRoot)
		if err != nil {
			return nil, AnchorState{}, err
		}
		nextLock, err := EncodeAnchorLockingScript(nextSt)
		if err != nil {
			return nil, AnchorState{}, err
		}
		tx := transaction.NewTransaction()
		tx.AddInput(&transaction.TransactionInput{
			SourceTXID:        prevTx.TxID(),
			SourceTxOutIndex:  0,
			SourceTransaction: prevTx,
			SequenceNumber:    0xffffffff,
		})
		tx.AddInput(&transaction.TransactionInput{
			SourceTXID:        fuelTx.TxID(),
			SourceTxOutIndex:  0,
			SourceTransaction: fuelTx,
			SequenceNumber:    0xffffffff,
		})
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: nextLock})
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: 99_000, LockingScript: changeLock})
		unlock, err := BuildAnchorAppendUnlock(ctx, tx, 0, prevTx.Outputs[0], newR, newSize, batchMeta, newMmrRoot, changePKH, 99_000, privKeySigner(signerKey))
		if err != nil {
			return nil, AnchorState{}, err
		}
		tx.Inputs[0].UnlockingScript = unlock
		return tx, nextSt, VerifyAnchorUTXOLineage(prevTx, tx)
	}

	// 1. Append batch 0 -> batch 1 under real Chronicle Script VM!
	r1 := core.SHA256([]byte("batch-1-root"))
	_, _ = mmr.Append(r1)
	meta1 := core.ComputeBatchMetaHash(core.SHA256([]byte("tree-1")), core.SHA256([]byte("pol-0")), core.SHA256([]byte("ont-0")))
	tx1, st1, err := buildAppend(tx0, st0, r1, 5, meta1, mmr.Root(), owner1)
	if err != nil {
		t.Fatalf("NektarAnchor.Append (0->1) failed Chronicle VM: %v", err)
	}

	// 2. Append batch 1 -> batch 2!
	r2 := core.SHA256([]byte("batch-2-root"))
	_, _ = mmr.Append(r2)
	meta2 := core.ComputeBatchMetaHash(core.SHA256([]byte("tree-2")), core.SHA256([]byte("pol-1")), core.SHA256([]byte("ont-1")))
	tx2, st2, err := buildAppend(tx1, st1, r2, 7, meta2, mmr.Root(), owner1)
	if err != nil {
		t.Fatalf("NektarAnchor.Append (1->2) failed Chronicle VM: %v", err)
	}

	// 3. Adversarial negative: forged R' in Output 0 MUST be rejected by Chronicle Script VM!
	{
		rBad := core.SHA256([]byte("batch-3-root"))
		metaBad := core.ComputeBatchMetaHash(core.Zero32Bytes(), core.Zero32Bytes(), core.Zero32Bytes())
		badMmr := core.SHA256([]byte("mmr-bad"))
		badSt, _ := NextAnchorState(st2, rBad, 2, metaBad, badMmr)
		badSt.Rp = core.SHA256([]byte("forged-rp"))
		badLock, _ := EncodeAnchorLockingScript(badSt)
		badTx := transaction.NewTransaction()
		badTx.AddInput(&transaction.TransactionInput{SourceTXID: tx2.TxID(), SourceTxOutIndex: 0, SourceTransaction: tx2, SequenceNumber: 0xffffffff})
		badTx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: badLock})
		badTx.AddOutput(&transaction.TransactionOutput{Satoshis: 99_000, LockingScript: changeLock})
		unlock, err := BuildAnchorAppendUnlock(ctx, badTx, 0, tx2.Outputs[0], rBad, 2, metaBad, badMmr, changePKH, 99_000, privKeySigner(owner1))
		if err != nil {
			t.Fatalf("unlock build: %v", err)
		}
		badTx.Inputs[0].UnlockingScript = unlock
		if vmErr := VerifyInputScriptChronicle(badTx, 0, tx2.Outputs[0]); vmErr == nil {
			t.Fatal("CRITICAL: Chronicle Script VM accepted forged R' on NektarAnchor.Append!")
		}
	}

	// 3b. Adversarial negative: forged StateHash Q_n in Output 0 MUST be rejected by Chronicle Script VM!
	{
		rBad := core.SHA256([]byte("batch-3-state-bad"))
		metaBad := core.ComputeBatchMetaHash(core.Zero32Bytes(), core.Zero32Bytes(), core.Zero32Bytes())
		mmrBad := core.SHA256([]byte("mmr-state-bad"))
		badSt, _ := NextAnchorState(st2, rBad, 2, metaBad, mmrBad)
		badSt.StateHash = core.SHA256([]byte("forged-state-q"))
		badLock, _ := EncodeAnchorLockingScript(badSt)
		badTx := transaction.NewTransaction()
		badTx.AddInput(&transaction.TransactionInput{SourceTXID: tx2.TxID(), SourceTxOutIndex: 0, SourceTransaction: tx2, SequenceNumber: 0xffffffff})
		badTx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: badLock})
		unlock, err := BuildAnchorAppendUnlock(ctx, badTx, 0, tx2.Outputs[0], rBad, 2, metaBad, mmrBad, make([]byte, 20), 0, privKeySigner(owner1))
		if err != nil {
			t.Fatalf("unlock build: %v", err)
		}
		badTx.Inputs[0].UnlockingScript = unlock
		if vmErr := VerifyInputScriptChronicle(badTx, 0, tx2.Outputs[0]); vmErr == nil {
			t.Fatal("CRITICAL: Chronicle Script VM accepted forged StateHash Q_n on NektarAnchor.Append!")
		}
	}

	// 4. Adversarial negative: skipped n (n=4 instead of 3) MUST be rejected by Chronicle Script VM!
	{
		rSkip := core.SHA256([]byte("batch-skip"))
		metaSkip := core.ComputeBatchMetaHash(core.Zero32Bytes(), core.Zero32Bytes(), core.Zero32Bytes())
		mmrSkip := core.SHA256([]byte("mmr-skip"))
		badSt, _ := NextAnchorState(st2, rSkip, 2, metaSkip, mmrSkip)
		badSt.N = 4
		badLock, _ := EncodeAnchorLockingScript(badSt)
		badTx := transaction.NewTransaction()
		badTx.AddInput(&transaction.TransactionInput{SourceTXID: tx2.TxID(), SourceTxOutIndex: 0, SourceTransaction: tx2, SequenceNumber: 0xffffffff})
		badTx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: badLock})
		unlock, err := BuildAnchorAppendUnlock(ctx, badTx, 0, tx2.Outputs[0], rSkip, 2, metaSkip, mmrSkip, make([]byte, 20), 0, privKeySigner(owner1))
		if err != nil {
			t.Fatalf("unlock build: %v", err)
		}
		badTx.Inputs[0].UnlockingScript = unlock
		if vmErr := VerifyInputScriptChronicle(badTx, 0, tx2.Outputs[0]); vmErr == nil {
			t.Fatal("CRITICAL: Chronicle Script VM accepted skipped batch number n=4 on NektarAnchor.Append!")
		}
	}

	// 5. Adversarial negative: wrong key signing NektarAnchor.Append MUST be rejected by Chronicle Script VM!
	{
		rAtk := core.SHA256([]byte("attacker-root"))
		metaAtk := core.ComputeBatchMetaHash(core.Zero32Bytes(), core.Zero32Bytes(), core.Zero32Bytes())
		mmrAtk := core.SHA256([]byte("mmr-atk"))
		nextSt, _ := NextAnchorState(st2, rAtk, 1, metaAtk, mmrAtk)
		nextLock, _ := EncodeAnchorLockingScript(nextSt)
		badTx := transaction.NewTransaction()
		badTx.AddInput(&transaction.TransactionInput{SourceTXID: tx2.TxID(), SourceTxOutIndex: 0, SourceTransaction: tx2, SequenceNumber: 0xffffffff})
		badTx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: nextLock})
		art, _ := LoadFullArtifact("NektarAnchor")
		codePart, _ := loadPatchedAnchorCode()
		prevScript := []byte(*tx2.Outputs[0].LockingScript)
		pre, _ := ComputeOpPushTxPreimage(badTx, 0, prevScript, 1, ResolveMethodCodeSepOffset(art, prevScript, 0), sighash.AllForkID)
		badSig, _ := SignDigestLowS(attacker, core.DSHA256(pre), sighash.AllForkID)
		unlock, _ := BuildMethodUnlockingScript(art, "append", codePart, []any{badSig, attacker.PubKey().Compressed(), rAtk, uint64(1), metaAtk, mmrAtk, make([]byte, 20), uint64(0), pre})
		badTx.Inputs[0].UnlockingScript = unlock
		if vmErr := VerifyInputScriptChronicle(badTx, 0, tx2.Outputs[0]); vmErr == nil {
			t.Fatal("CRITICAL: Chronicle Script VM accepted attacker signature on NektarAnchor.Append!")
		}
	}

	// 6. Rotate ownership owner1 -> owner2 under real Chronicle Script VM!
	rotSt, err := RotatedAnchorState(st2, pkh2)
	if err != nil {
		t.Fatalf("RotatedAnchorState: %v", err)
	}
	rotLock, err := EncodeAnchorLockingScript(rotSt)
	if err != nil {
		t.Fatalf("EncodeAnchorLockingScript(rot): %v", err)
	}
	rotTx := transaction.NewTransaction()
	rotTx.AddInput(&transaction.TransactionInput{SourceTXID: tx2.TxID(), SourceTxOutIndex: 0, SourceTransaction: tx2, SequenceNumber: 0xffffffff})
	rotTx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: rotLock})
	rotTx.AddOutput(&transaction.TransactionOutput{Satoshis: 99_000, LockingScript: changeLock})
	rotUnlock, err := BuildAnchorRotateUnlock(ctx, rotTx, 0, tx2.Outputs[0], pkh2, changePKH, 99_000, privKeySigner(owner1))
	if err != nil {
		t.Fatalf("BuildAnchorRotateUnlock: %v", err)
	}
	rotTx.Inputs[0].UnlockingScript = rotUnlock
	if err := VerifyAnchorUTXOLineage(tx2, rotTx); err != nil {
		t.Fatalf("NektarAnchor.Rotate failed Chronicle VM: %v", err)
	}

	// 7. Old owner1 MUST be rejected after Rotate; new owner2 MUST succeed!
	r3 := core.SHA256([]byte("batch-3-after-rotate"))
	_, _ = mmr.Append(r3)
	meta3 := core.ComputeBatchMetaHash(core.SHA256([]byte("tree-3")), core.SHA256([]byte("pol-1")), core.SHA256([]byte("ont-1")))
	if _, _, err := buildAppend(rotTx, rotSt, r3, 9, meta3, mmr.Root(), owner1); err == nil {
		t.Fatal("expected old owner1 to fail after NektarAnchor.Rotate")
	}
	if _, _, err := buildAppend(rotTx, rotSt, r3, 9, meta3, mmr.Root(), owner2); err != nil {
		t.Fatalf("new owner2 Append after Rotate failed Chronicle VM: %v", err)
	}
}

// TestDisclosureSaleEscrowChronicleVM tests real BSV Chronicle Script VM execution of
// DisclosureSale.Buy(k), on-chain key extraction + BRC-369 decryption, DisclosureSale.Refund(),
// and all adversarial attack vectors (wrong key, underpay, premature refund, F-02 nSequence bypass, F-02 domain spoof).
func TestDisclosureSaleEscrowChronicleVM(t *testing.T) {
	ctx := context.Background()
	seller, _ := ec.NewPrivateKey()
	buyer, _ := ec.NewPrivateKey()
	attacker, _ := ec.NewPrivateKey()

	sellerPKH := core.Hash160(seller.PubKey().Compressed())
	buyerPKH := core.Hash160(buyer.PubKey().Compressed())

	plaintext := []byte("Verifiable L1 trustless atomic disclosure payload")
	leafHex := hex.EncodeToString(core.SHA256([]byte("leaf-escrow-1")))
	expUnix := uint64(time.Now().Add(time.Hour).Unix())
	offer, err := CreateKeyedSaleOffer(seller, plaintext, 2500, 120, 500, expUnix, hex.EncodeToString(buyer.PubKey().Compressed()), leafHex, false)
	if err != nil {
		t.Fatalf("CreateKeyedSaleOffer: %v", err)
	}
	hWrapped, _ := hex.DecodeString(offer.Config.HWrapped)
	params := DisclosureSaleParams{
		SellerPKH:    sellerPKH,
		Price:        2500,
		HWrapped:     hWrapped,
		RefundAfter:  120, // block height 120
		MaxFee:       500,
		DepositorPKH: buyerPKH,
	}

	escrowLock, err := EncodeDisclosureSaleLockingScript(params)
	if err != nil {
		t.Fatalf("EncodeDisclosureSaleLockingScript: %v", err)
	}
	escrowTx := transaction.NewTransaction()
	escrowTx.AddOutput(&transaction.TransactionOutput{
		Satoshis:      params.RequiredEscrowSatoshis() + 200, // 2500 + 500 + 1 + 200 = 3201 sats
		LockingScript: escrowLock,
	})

	// 1. Positive: Seller calls Buy(k) -> passes Chronicle VM -> Buyer extracts k directly from Buy tx!
	k := offer.ContentKey()
	buyTx, err := BuildDisclosureSaleBuyTx(escrowTx, 0, params, k[:], 300)
	if err != nil {
		t.Fatalf("BuildDisclosureSaleBuyTx failed Chronicle VM: %v", err)
	}
	extractedKey, err := ExtractRevealedKeyFromBuyTx(buyTx, 0, offer.Config.HWrapped)
	if err != nil {
		t.Fatalf("ExtractRevealedKeyFromBuyTx: %v", err)
	}
	decrypted, err := ClaimKeyedSale(offer, extractedKey)
	if err != nil {
		t.Fatalf("ClaimKeyedSale with on-chain extracted key: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypted mismatch: got %q want %q", decrypted, plaintext)
	}

	// 2. Adversarial negative: wrong BRC-369 content key MUST fail Chronicle Script VM!
	{
		art, _ := LoadFullArtifact("DisclosureSale")
		prevOut := escrowTx.Outputs[0]
		sellerLock, _ := P2PKHLockingScript(sellerPKH)
		buyerLock, _ := P2PKHLockingScript(buyerPKH)
		badTx := transaction.NewTransaction()
		badTx.AddInput(&transaction.TransactionInput{SourceTXID: escrowTx.TxID(), SourceTxOutIndex: 0, SourceTransaction: escrowTx, SequenceNumber: 0xffffffff})
		badTx.AddOutput(&transaction.TransactionOutput{Satoshis: params.Price, LockingScript: sellerLock})
		badTx.AddOutput(&transaction.TransactionOutput{Satoshis: prevOut.Satoshis - params.Price - 300, LockingScript: buyerLock})
		prevScript := []byte(*prevOut.LockingScript)
		pre, _ := ComputeOpPushTxPreimage(badTx, 0, prevScript, prevOut.Satoshis, ResolveMethodCodeSepOffset(art, prevScript, 0), sighash.AllForkID)
		wrongK := core.SHA256([]byte("fake-key"))
		unlock, _ := BuildMethodUnlockingScript(art, "buy", nil, []any{wrongK, prevOut.Satoshis - params.Price - 300, pre})
		badTx.Inputs[0].UnlockingScript = unlock
		if vmErr := VerifyInputScriptChronicle(badTx, 0, prevOut); vmErr == nil {
			t.Fatal("CRITICAL: Chronicle Script VM accepted wrong BRC-369 key on DisclosureSale.Buy!")
		}
	}

	// 3. Positive: Buyer calls Refund() at blockHeight == 120 with nSequence == 0xfffffffe!
	refundTx, err := BuildDisclosureSaleRefundTx(ctx, escrowTx, 0, params, 120, 300, privKeySigner(buyer))
	if err != nil {
		t.Fatalf("BuildDisclosureSaleRefundTx failed Chronicle VM: %v", err)
	}
	if refundTx.Outputs[0].Satoshis != escrowTx.Outputs[0].Satoshis-300 {
		t.Fatalf("unexpected refund sats: %d", refundTx.Outputs[0].Satoshis)
	}

	// 4. Adversarial F-02 negative: premature locktime (119 < 120), SEQUENCE_FINAL (0xffffffff),
	//    timestamp locktime domain spoof (500000001), and wrong depositor key MUST ALL fail Chronicle Script VM!
	art, _ := LoadFullArtifact("DisclosureSale")
	prevOut := escrowTx.Outputs[0]
	prevScript := []byte(*prevOut.LockingScript)
	buyerLock, _ := P2PKHLockingScript(buyerPKH)

	runBadRefund := func(name string, lt uint32, seq uint32, key *ec.PrivateKey) {
		t.Helper()
		badTx := transaction.NewTransaction()
		badTx.LockTime = lt
		badTx.AddInput(&transaction.TransactionInput{SourceTXID: escrowTx.TxID(), SourceTxOutIndex: 0, SourceTransaction: escrowTx, SequenceNumber: seq})
		chg := prevOut.Satoshis - 300
		badTx.AddOutput(&transaction.TransactionOutput{Satoshis: chg, LockingScript: buyerLock})
		pre, _ := ComputeOpPushTxPreimage(badTx, 0, prevScript, prevOut.Satoshis, ResolveMethodCodeSepOffset(art, prevScript, 2), sighash.AllForkID)
		sig, _ := SignDigestLowS(key, core.DSHA256(pre), sighash.AllForkID)
		unlock, _ := BuildMethodUnlockingScript(art, "refund", nil, []any{sig, key.PubKey().Compressed(), chg, pre})
		badTx.Inputs[0].UnlockingScript = unlock
		if vmErr := VerifyInputScriptChronicle(badTx, 0, prevOut); vmErr == nil {
			t.Fatalf("CRITICAL: Chronicle Script VM accepted adversarial Refund (%s)!", name)
		}
	}
	runBadRefund("premature locktime 119 < 120", 119, 0xfffffffe, buyer)
	runBadRefund("F-02 SEQUENCE_FINAL 0xffffffff", 120, 0xffffffff, buyer)
	runBadRefund("F-02 timestamp domain 500000001 on block-height escrow", 500000001, 0xfffffffe, buyer)
	runBadRefund("attacker key", 120, 0xfffffffe, attacker)
}

// TestNektarAgentVaultAndAtomicEscrowCompositionChronicleVM proves end-to-end under the real
// post-Chronicle Bitcoin Script VM:
//  1. Cold operator deploys NektarAgentVault -> hot agent spends DIRECTLY into a DisclosureSale
//     escrow locking script in 1 atomic tx -> Seller spends that DisclosureSale output via Buy(k)
//     -> Buyer extracts k and decrypts!
//  2. Rolling window enforcement & reset across block heights.
//  3. Adversarial tests: PerTxCap breach, WindowCap breach, excess fee siphon, F-02 SEQUENCE_FINAL,
//     F-02 timestamp locktime, hot agent calling OperatorControl/OperatorSweep, hot agent spending while Paused.
//  4. Cold operator pauses, rotates agent key, resumes, and sweeps remaining vault balance.
func TestNektarAgentVaultAndAtomicEscrowCompositionChronicleVM(t *testing.T) {
	ctx := context.Background()
	operator, _ := ec.NewPrivateKey()
	agent1, _ := ec.NewPrivateKey()
	agent2, _ := ec.NewPrivateKey()
	seller, _ := ec.NewPrivateKey()

	opPKH := core.Hash160(operator.PubKey().Compressed())
	ag1PKH := core.Hash160(agent1.PubKey().Compressed())
	ag2PKH := core.Hash160(agent2.PubKey().Compressed())
	sellerPKH := core.Hash160(seller.PubKey().Compressed())

	vault0State := AgentVaultState{
		OperatorPKH:   opPKH,
		AgentPKH:      ag1PKH,
		PerTxCap:      4_000,
		WindowCap:     7_000,
		WindowBlocks:  144,
		MaxFee:        600,
		SpentInWindow: 0,
		WindowStart:   100,
		Paused:        false,
	}
	vault0Lock, err := EncodeAgentVaultLockingScript(vault0State)
	if err != nil {
		t.Fatalf("EncodeAgentVaultLockingScript: %v", err)
	}
	dec0, err := DecodeAgentVault(*vault0Lock)
	if err != nil || dec0.PerTxCap != 4_000 || dec0.WindowCap != 7_000 || dec0.Paused {
		t.Fatalf("DecodeAgentVault roundtrip failed: dec=%+v err=%v", dec0, err)
	}

	vaultTx0 := transaction.NewTransaction()
	vaultTx0.AddOutput(&transaction.TransactionOutput{Satoshis: 50_000, LockingScript: vault0Lock})

	// 1. Atomic 1-tx composition: NektarAgentVault.AgentSpend -> Output 1 IS a DisclosureSale escrow!
	secret := []byte("Vault-funded atomic escrow secret")
	expUnix := uint64(time.Now().Add(time.Hour).Unix())
	leafHex := hex.EncodeToString(core.SHA256([]byte("vault-composed-leaf")))
	offer, err := CreateKeyedSaleOffer(seller, secret, 2_000, 150, 500, expUnix, hex.EncodeToString(agent1.PubKey().Compressed()), leafHex, false)
	if err != nil {
		t.Fatalf("CreateKeyedSaleOffer: %v", err)
	}
	hWrapped, _ := hex.DecodeString(offer.Config.HWrapped)
	saleParams := DisclosureSaleParams{
		SellerPKH:    sellerPKH,
		Price:        2_000,
		HWrapped:     hWrapped,
		RefundAfter:  150,
		MaxFee:       500,
		DepositorPKH: ag1PKH,
	}
	escrowLock, err := EncodeDisclosureSaleLockingScript(saleParams)
	if err != nil {
		t.Fatalf("EncodeDisclosureSaleLockingScript: %v", err)
	}
	escrowSats := saleParams.RequiredEscrowSatoshis() + 100 // 2,601 sats <= PerTxCap (4,000)

	vaultTx1, vault1State, err := BuildAgentVaultSpendTx(ctx, vaultTx0, 0, escrowLock, escrowSats, 105, 450, privKeySigner(agent1))
	if err != nil {
		t.Fatalf("AgentSpend -> DisclosureSale failed Chronicle VM: %v", err)
	}
	if vault1State.SpentInWindow != escrowSats || vault1State.WindowStart != 100 {
		t.Fatalf("unexpected vault1 state: %+v", vault1State)
	}

	// Seller spends Output 1 (the DisclosureSale escrow funded directly by the vault!) via Buy(k).
	k := offer.ContentKey()
	buyTx, err := BuildDisclosureSaleBuyTx(vaultTx1, 1, saleParams, k[:], 300)
	if err != nil {
		t.Fatalf("Seller Buy() on vault-funded DisclosureSale failed Chronicle VM: %v", err)
	}
	gotK, err := ExtractRevealedKeyFromBuyTx(buyTx, 0, offer.Config.HWrapped)
	if err != nil {
		t.Fatalf("ExtractRevealedKeyFromBuyTx: %v", err)
	}
	gotPlain, err := ClaimKeyedSale(offer, gotK)
	if err != nil || !bytes.Equal(gotPlain, secret) {
		t.Fatalf("decrypted mismatch: %q err=%v", gotPlain, err)
	}

	// 2. Second spend within same window (2601 + 3500 = 6101 <= 7000) succeeds!
	p2pkhSeller, _ := P2PKHLockingScript(sellerPKH)
	vaultTx2, vault2State, err := BuildAgentVaultSpendTx(ctx, vaultTx1, 0, p2pkhSeller, 3_500, 110, 450, privKeySigner(agent1))
	if err != nil {
		t.Fatalf("second AgentSpend within window failed: %v", err)
	}
	if vault2State.SpentInWindow != 6_101 {
		t.Fatalf("expected SpentInWindow=6101, got %d", vault2State.SpentInWindow)
	}

	// 3. Adversarial negative raw VM checks directly against vaultTx2:0 (SpentInWindow=6101, WindowCap=7000).
	art, _ := LoadFullArtifact("NektarAgentVault")
	codePart, _ := BuildCodePart(art, nil)
	prevOut2 := vaultTx2.Outputs[0]
	prevScript2 := []byte(*prevOut2.LockingScript)

	tryRawAgentSpend := func(name string, pay, newSpent, newStart, contSats uint64, lt, seq uint32, signer *ec.PrivateKey) {
		t.Helper()
		nextSt := *vault2State
		nextSt.SpentInWindow = newSpent
		nextSt.WindowStart = newStart
		nextLock, encErr := EncodeAgentVaultLockingScript(nextSt)
		if encErr != nil || nextLock == nil {
			nextLock, encErr = BuildStatefulLockingScript(art, nil, nextSt.toStateMap())
			if encErr != nil {
				t.Fatalf("build raw adversarial AgentVault locking script (%s): %v", name, encErr)
			}
		}
		badTx := transaction.NewTransaction()
		badTx.LockTime = lt
		badTx.AddInput(&transaction.TransactionInput{SourceTXID: vaultTx2.TxID(), SourceTxOutIndex: 0, SourceTransaction: vaultTx2, SequenceNumber: seq})
		badTx.AddOutput(&transaction.TransactionOutput{Satoshis: contSats, LockingScript: nextLock})
		badTx.AddOutput(&transaction.TransactionOutput{Satoshis: pay, LockingScript: p2pkhSeller})
		pre, _ := ComputeOpPushTxPreimage(badTx, 0, prevScript2, prevOut2.Satoshis, ResolveMethodCodeSepOffset(art, prevScript2, 0), sighash.AllForkID)
		sig, _ := SignDigestLowS(signer, core.DSHA256(pre), sighash.AllForkID)
		unlock, _ := BuildMethodUnlockingScript(art, "agentSpend", codePart, []any{
			sig, signer.PubKey().Compressed(), []byte(*p2pkhSeller), pay, newSpent, newStart, contSats, make([]byte, 20), uint64(0), pre,
		})
		badTx.Inputs[0].UnlockingScript = unlock
		if vmErr := VerifyInputScriptChronicle(badTx, 0, prevOut2); vmErr == nil {
			t.Fatalf("CRITICAL: Chronicle Script VM accepted adversarial AgentSpend (%s)!", name)
		}
	}
	// Exceeds WindowCap (6101 + 1000 = 7101 > 7000).
	tryRawAgentSpend("over WindowCap", 1_000, 7_101, 100, prevOut2.Satoshis-1_450, 115, 0xfffffffe, agent1)
	// Exceeds PerTxCap (4001 > 4000) even after window expires at height 250.
	tryRawAgentSpend("over PerTxCap", 4_001, 4_001, 250, prevOut2.Satoshis-4_451, 250, 0xfffffffe, agent1)
	// F-02 SEQUENCE_FINAL trying to bypass locktime window reset!
	tryRawAgentSpend("F-02 SEQUENCE_FINAL", 1_000, 1_000, 250, prevOut2.Satoshis-1_450, 250, 0xffffffff, agent1)
	// F-02 timestamp locktime domain spoof!
	tryRawAgentSpend("F-02 timestamp locktime", 1_000, 1_000, 500_000_001, prevOut2.Satoshis-1_450, 500_000_001, 0xfffffffe, agent1)
	// Excess fee siphon (> MaxFee 600)!
	tryRawAgentSpend("excess fee siphon", 500, 6_601, 100, prevOut2.Satoshis-500-900, 115, 0xfffffffe, agent1)

	// 4. Rolling window reset after WindowStart + WindowBlocks (100 + 144 = 244) succeeds cleanly!
	vaultTx3, vault3State, err := BuildAgentVaultSpendTx(ctx, vaultTx2, 0, p2pkhSeller, 3_000, 244, 450, privKeySigner(agent1))
	if err != nil {
		t.Fatalf("AgentSpend after rolling window reset failed Chronicle VM: %v", err)
	}
	if vault3State.SpentInWindow != 3_000 || vault3State.WindowStart != 244 {
		t.Fatalf("expected reset window (spent=3000, start=244), got %+v", vault3State)
	}

	// 5. Cold Operator pauses vault -> hot agent is locked out on L1!
	pausedTrue := true
	vaultTx4, vault4State, err := BuildAgentVaultOperatorControlTx(ctx, vaultTx3, 0, OperatorControlUpdate{
		NewPaused: &pausedTrue,
	}, privKeySigner(operator))
	if err != nil {
		t.Fatalf("OperatorControl(pause) failed Chronicle VM: %v", err)
	}
	if !vault4State.Paused {
		t.Fatal("expected vault4 to be paused")
	}
	// Verify paused lockout directly in Chronicle Script VM (not just Go pre-check!).
	{
		prevOut4 := vaultTx4.Outputs[0]
		prevScript4 := []byte(*prevOut4.LockingScript)
		nextLock, _ := EncodeAgentVaultLockingScript(*vault4State)
		badTx := transaction.NewTransaction()
		badTx.LockTime = 245
		badTx.AddInput(&transaction.TransactionInput{SourceTXID: vaultTx4.TxID(), SourceTxOutIndex: 0, SourceTransaction: vaultTx4, SequenceNumber: 0xfffffffe})
		badTx.AddOutput(&transaction.TransactionOutput{Satoshis: prevOut4.Satoshis - 550, LockingScript: nextLock})
		badTx.AddOutput(&transaction.TransactionOutput{Satoshis: 100, LockingScript: p2pkhSeller})
		pre, _ := ComputeOpPushTxPreimage(badTx, 0, prevScript4, prevOut4.Satoshis, ResolveMethodCodeSepOffset(art, prevScript4, 0), sighash.AllForkID)
		sig, _ := SignDigestLowS(agent1, core.DSHA256(pre), sighash.AllForkID)
		unlock, _ := BuildMethodUnlockingScript(art, "agentSpend", codePart, []any{
			sig, agent1.PubKey().Compressed(), []byte(*p2pkhSeller), uint64(100), uint64(3_100), uint64(244), prevOut4.Satoshis - 550, make([]byte, 20), uint64(0), pre,
		})
		badTx.Inputs[0].UnlockingScript = unlock
		if vmErr := VerifyInputScriptChronicle(badTx, 0, prevOut4); vmErr == nil {
			t.Fatal("CRITICAL: Chronicle Script VM allowed AgentSpend on PAUSED vault!")
		}
	}

	// 6. Hot agent CANNOT call OperatorControl or OperatorSweep (verified in Chronicle Script VM!).
	{
		prevOut4 := vaultTx4.Outputs[0]
		prevScript4 := []byte(*prevOut4.LockingScript)
		badTx := transaction.NewTransaction()
		badTx.AddInput(&transaction.TransactionInput{SourceTXID: vaultTx4.TxID(), SourceTxOutIndex: 0, SourceTransaction: vaultTx4, SequenceNumber: 0xffffffff})
		destLock, _ := P2PKHLockingScript(ag1PKH)
		badTx.AddOutput(&transaction.TransactionOutput{Satoshis: prevOut4.Satoshis - 350, LockingScript: destLock})
		pre, _ := ComputeOpPushTxPreimage(badTx, 0, prevScript4, prevOut4.Satoshis, ResolveMethodCodeSepOffset(art, prevScript4, 2), sighash.AllForkID)
		sig, _ := SignDigestLowS(agent1, core.DSHA256(pre), sighash.AllForkID)
		unlock, _ := BuildMethodUnlockingScript(art, "operatorSweep", codePart, []any{
			sig, agent1.PubKey().Compressed(), ag1PKH, prevOut4.Satoshis - 350, make([]byte, 20), uint64(0), pre,
		})
		badTx.Inputs[0].UnlockingScript = unlock
		if vmErr := VerifyInputScriptChronicle(badTx, 0, prevOut4); vmErr == nil {
			t.Fatal("CRITICAL: Chronicle Script VM allowed hot agent to call OperatorSweep!")
		}
	}

	// 7. Cold Operator unpauses + rotates AgentPKH (agent1 -> agent2), then agent2 spends, then Operator sweeps all funds!
	pausedFalse := false
	vaultTx5, _, err := BuildAgentVaultOperatorControlTx(ctx, vaultTx4, 0, OperatorControlUpdate{
		NewAgentPKH: ag2PKH,
		NewPaused:   &pausedFalse,
	}, privKeySigner(operator))
	if err != nil {
		t.Fatalf("OperatorControl(unpause+rotate) failed Chronicle VM: %v", err)
	}
	vaultTx6, _, err := BuildAgentVaultSpendTx(ctx, vaultTx5, 0, p2pkhSeller, 500, 250, 450, privKeySigner(agent2))
	if err != nil {
		t.Fatalf("rotated agent2 AgentSpend failed Chronicle VM: %v", err)
	}
	sweepTx, err := BuildAgentVaultSweepTx(ctx, vaultTx6, 0, opPKH, 350, privKeySigner(operator))
	if err != nil {
		t.Fatalf("OperatorSweep failed Chronicle VM: %v", err)
	}
	if sweepTx.Outputs[0].Satoshis != vaultTx6.Outputs[0].Satoshis-350 {
		t.Fatalf("unexpected sweep amount: %d", sweepTx.Outputs[0].Satoshis)
	}
}
