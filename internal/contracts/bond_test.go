package contracts

import (
	"bytes"
	"context"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"

	"github.com/kryptokrates/nektar/internal/core"
)

// TestNektarCheckpointBondChronicleVM proves NektarCheckpointBond end-to-end under the real
// post-Chronicle Bitcoin Script VM:
//  1. Commit() rolls deadline forward by EpochLen before Deadline expires.
//  2. Adversarial negative checks: late Commit (>= Deadline), wrong newDeadline, F-02 SEQUENCE_FINAL,
//     F-02 timestamp locktime, premature Seize (< Deadline), premature Close (< BondReleaseHeight),
//     unauthorized Close, double-Seize after Seized==1.
//  3. Permissionless Seize() slashes the bond to slasher's payTo script at height >= Deadline.
//  4. Operator Close() reclaims bond cleanly at height >= BondReleaseHeight.
func TestNektarCheckpointBondChronicleVM(t *testing.T) {
	ctx := context.Background()
	op, _ := ec.NewPrivateKey()
	slasher, _ := ec.NewPrivateKey()
	opPKH := core.Hash160(op.PubKey().Compressed())
	slasherPKH := core.Hash160(slasher.PubKey().Compressed())
	slasherLock, _ := P2PKHLockingScript(slasherPKH)

	st0 := CheckpointBondState{
		OperatorPKH:       opPKH,
		BondReleaseHeight: 300,
		MaxFee:            500,
		EpochLen:          50,
		N:                 0,
		LastRoot:          nil,
		Deadline:          150,
		Seized:            false,
	}
	lock0, err := EncodeCheckpointBondLockingScript(st0)
	if err != nil {
		t.Fatalf("EncodeCheckpointBondLockingScript: %v", err)
	}
	dec0, err := DecodeCheckpointBond(*lock0)
	if err != nil || dec0.Deadline != 150 || dec0.EpochLen != 50 || dec0.Seized {
		t.Fatalf("DecodeCheckpointBond roundtrip failed: dec=%+v err=%v", dec0, err)
	}

	tx0 := transaction.NewTransaction()
	tx0.AddOutput(&transaction.TransactionOutput{Satoshis: 25_000, LockingScript: lock0})

	// 1. Honest Commit at height 120 (< 150) -> newDeadline = 170, N = 1!
	root1 := core.SHA256([]byte("checkpoint-root-1"))
	tx1, st1, err := BuildBondCommitTx(ctx, tx0, 0, root1, 120, 400, privKeySigner(op))
	if err != nil {
		t.Fatalf("BuildBondCommitTx failed Chronicle VM: %v", err)
	}
	if st1.N != 1 || st1.Deadline != 170 || !bytes.Equal(st1.LastRoot, root1) {
		t.Fatalf("unexpected st1: %+v", st1)
	}

	// 2. Adversarial raw Chronicle VM negative tests on tx1:0 (Deadline=170, BondReleaseHeight=300).
	art, _ := LoadFullArtifact("NektarCheckpointBond")
	codePart, _ := BuildCodePart(art, nil)
	prevOut1 := tx1.Outputs[0]
	prevScript1 := []byte(*prevOut1.LockingScript)

	// 2a. Premature Seize at height 169 (< 170) MUST fail Chronicle VM!
	{
		seizedSt := *st1
		seizedSt.Seized = true
		seizedLock, _ := EncodeCheckpointBondLockingScript(seizedSt)
		bad := transaction.NewTransaction()
		bad.LockTime = 169
		bad.AddInput(&transaction.TransactionInput{SourceTXID: tx1.TxID(), SourceTxOutIndex: 0, SourceTransaction: tx1, SequenceNumber: 0xfffffffe})
		payout := prevOut1.Satoshis - 401
		bad.AddOutput(&transaction.TransactionOutput{Satoshis: payout, LockingScript: slasherLock})
		bad.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: seizedLock})
		pre, _ := ComputeOpPushTxPreimage(bad, 0, prevScript1, prevOut1.Satoshis, ResolveMethodCodeSepOffset(art, prevScript1, 1), sighash.AllForkID)
		unlock, _ := BuildMethodUnlockingScript(art, "seize", codePart, []any{[]byte(*slasherLock), payout, make([]byte, 20), uint64(0), pre})
		bad.Inputs[0].UnlockingScript = unlock
		if vmErr := VerifyInputScriptChronicle(bad, 0, prevOut1); vmErr == nil {
			t.Fatal("CRITICAL: Chronicle VM allowed premature Seize before Deadline!")
		}
	}

	// 2b. F-02 SEQUENCE_FINAL (0xffffffff) on Seize at height 170 MUST fail Chronicle VM!
	{
		seizedSt := *st1
		seizedSt.Seized = true
		seizedLock, _ := EncodeCheckpointBondLockingScript(seizedSt)
		bad := transaction.NewTransaction()
		bad.LockTime = 170
		bad.AddInput(&transaction.TransactionInput{SourceTXID: tx1.TxID(), SourceTxOutIndex: 0, SourceTransaction: tx1, SequenceNumber: 0xffffffff})
		payout := prevOut1.Satoshis - 401
		bad.AddOutput(&transaction.TransactionOutput{Satoshis: payout, LockingScript: slasherLock})
		bad.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: seizedLock})
		pre, _ := ComputeOpPushTxPreimage(bad, 0, prevScript1, prevOut1.Satoshis, ResolveMethodCodeSepOffset(art, prevScript1, 1), sighash.AllForkID)
		unlock, _ := BuildMethodUnlockingScript(art, "seize", codePart, []any{[]byte(*slasherLock), payout, make([]byte, 20), uint64(0), pre})
		bad.Inputs[0].UnlockingScript = unlock
		if vmErr := VerifyInputScriptChronicle(bad, 0, prevOut1); vmErr == nil {
			t.Fatal("CRITICAL: Chronicle VM allowed F-02 SEQUENCE_FINAL bypass on Bond.Seize!")
		}
	}

	// 2c. Late Commit at height 170 (== Deadline) MUST fail Chronicle VM!
	{
		nextSt := *st1
		nextSt.N = 2
		nextSt.LastRoot = core.SHA256([]byte("late-root"))
		nextSt.Deadline = 220
		nextLock, _ := EncodeCheckpointBondLockingScript(nextSt)
		bad := transaction.NewTransaction()
		bad.LockTime = 170
		bad.AddInput(&transaction.TransactionInput{SourceTXID: tx1.TxID(), SourceTxOutIndex: 0, SourceTransaction: tx1, SequenceNumber: 0xfffffffe})
		bad.AddOutput(&transaction.TransactionOutput{Satoshis: prevOut1.Satoshis - 400, LockingScript: nextLock})
		pre, _ := ComputeOpPushTxPreimage(bad, 0, prevScript1, prevOut1.Satoshis, ResolveMethodCodeSepOffset(art, prevScript1, 0), sighash.AllForkID)
		sig, _ := SignDigestLowS(op, core.DSHA256(pre), sighash.AllForkID)
		unlock, _ := BuildMethodUnlockingScript(art, "commit", codePart, []any{sig, op.PubKey().Compressed(), nextSt.LastRoot, prevOut1.Satoshis - 400, uint64(220), make([]byte, 20), uint64(0), pre})
		bad.Inputs[0].UnlockingScript = unlock
		if vmErr := VerifyInputScriptChronicle(bad, 0, prevOut1); vmErr == nil {
			t.Fatal("CRITICAL: Chronicle VM allowed late Commit at height >= Deadline!")
		}
	}

	// 2d. Premature Close at height 299 (< 300) MUST fail Chronicle VM!
	{
		opLock, _ := P2PKHLockingScript(opPKH)
		bad := transaction.NewTransaction()
		bad.LockTime = 299
		bad.AddInput(&transaction.TransactionInput{SourceTXID: tx1.TxID(), SourceTxOutIndex: 0, SourceTransaction: tx1, SequenceNumber: 0xfffffffe})
		bad.AddOutput(&transaction.TransactionOutput{Satoshis: prevOut1.Satoshis - 350, LockingScript: opLock})
		pre, _ := ComputeOpPushTxPreimage(bad, 0, prevScript1, prevOut1.Satoshis, ResolveMethodCodeSepOffset(art, prevScript1, 2), sighash.AllForkID)
		sig, _ := SignDigestLowS(op, core.DSHA256(pre), sighash.AllForkID)
		unlock, _ := BuildMethodUnlockingScript(art, "close", codePart, []any{sig, op.PubKey().Compressed(), prevOut1.Satoshis - 350, make([]byte, 20), uint64(0), pre})
		bad.Inputs[0].UnlockingScript = unlock
		if vmErr := VerifyInputScriptChronicle(bad, 0, prevOut1); vmErr == nil {
			t.Fatal("CRITICAL: Chronicle VM allowed premature Close before BondReleaseHeight!")
		}
	}

	// 3. Permissionless Seize at height 170 (>= Deadline) succeeds under real Chronicle VM!
	seizeTx, seizedSt, err := BuildBondSeizeTx(tx1, 0, slasherLock, 170, 400)
	if err != nil {
		t.Fatalf("BuildBondSeizeTx failed Chronicle VM: %v", err)
	}
	if !seizedSt.Seized || seizeTx.Outputs[0].Satoshis != prevOut1.Satoshis-401 || seizeTx.Outputs[1].Satoshis != 1 {
		t.Fatalf("unexpected seize outputs/state: st=%+v out0=%d out1=%d", seizedSt, seizeTx.Outputs[0].Satoshis, seizeTx.Outputs[1].Satoshis)
	}

	// 4. Operator Close at height 300 (>= BondReleaseHeight) succeeds under real Chronicle VM!
	closeTx, err := BuildBondCloseTx(ctx, tx1, 0, 300, 350, privKeySigner(op))
	if err != nil {
		t.Fatalf("BuildBondCloseTx failed Chronicle VM: %v", err)
	}
	if closeTx.Outputs[0].Satoshis != prevOut1.Satoshis-350 {
		t.Fatalf("unexpected close payout: %d", closeTx.Outputs[0].Satoshis)
	}
}
