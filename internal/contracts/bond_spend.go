package contracts

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"

	"github.com/kryptokrates/nektar/internal/core"
)

// BuildBondCommitTx constructs, signs, and Chronicle-VM-verifies a NektarCheckpointBond.Commit()
// transaction rolling the SLA deadline forward by EpochLen before Deadline expires.
func BuildBondCommitTx(
	ctx context.Context,
	bondTx *transaction.Transaction,
	bondVout uint32,
	newRoot []byte,
	blockHeight uint32,
	minerFeeSats uint64,
	operatorSigner CovenantSighashSigner,
) (*transaction.Transaction, *CheckpointBondState, error) {
	if bondTx == nil || int(bondVout) >= len(bondTx.Outputs) || bondTx.Outputs[bondVout] == nil || bondTx.Outputs[bondVout].LockingScript == nil {
		return nil, nil, errors.New("contracts: invalid bondTx or bondVout")
	}
	if operatorSigner == nil {
		return nil, nil, errors.New("contracts: Bond Commit requires operatorSigner")
	}
	if blockHeight >= 500_000_000 {
		return nil, nil, fmt.Errorf("contracts: Bond Commit blockHeight %d must be < 500_000_000", blockHeight)
	}
	prevOut := bondTx.Outputs[bondVout]
	prevScript := []byte(*prevOut.LockingScript)
	prevState, err := DecodeCheckpointBond(prevScript)
	if err != nil {
		return nil, nil, err
	}
	if prevState.Seized {
		return nil, nil, errors.New("contracts: Bond Commit refused: bond is already seized")
	}
	if uint64(blockHeight) >= prevState.Deadline {
		return nil, nil, fmt.Errorf("contracts: Bond Commit blockHeight %d >= deadline %d (missed SLA window)", blockHeight, prevState.Deadline)
	}
	newDeadline := uint64(blockHeight) + prevState.EpochLen
	if newDeadline >= 500_000_000 {
		return nil, nil, errors.New("contracts: Bond Commit newDeadline exceeds block-height domain")
	}
	if minerFeeSats == 0 {
		minerFeeSats = min(prevState.MaxFee, uint64(400))
	}
	if minerFeeSats > prevState.MaxFee {
		return nil, nil, fmt.Errorf("contracts: Bond Commit fee %d exceeds maxFee %d", minerFeeSats, prevState.MaxFee)
	}
	if prevOut.Satoshis <= minerFeeSats {
		return nil, nil, fmt.Errorf("contracts: bond balance %d insufficient for Commit fee %d", prevOut.Satoshis, minerFeeSats)
	}
	bondSats := prevOut.Satoshis - minerFeeSats

	nextState := *prevState
	nextState.OperatorPKH = append([]byte(nil), prevState.OperatorPKH...)
	nextState.N = prevState.N + 1
	nextState.LastRoot = append([]byte(nil), newRoot...)
	nextState.Deadline = newDeadline
	nextLock, err := EncodeCheckpointBondLockingScript(nextState)
	if err != nil {
		return nil, nil, err
	}

	tx := transaction.NewTransaction()
	tx.LockTime = blockHeight
	tx.AddInput(&transaction.TransactionInput{
		SourceTXID:        bondTx.TxID(),
		SourceTxOutIndex:  bondVout,
		SourceTransaction: bondTx,
		SequenceNumber:    0xfffffffe, // F-02 non-final
	})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: bondSats, LockingScript: nextLock})

	art, err := LoadFullArtifact("NektarCheckpointBond")
	if err != nil {
		return nil, nil, err
	}
	codePart, err := BuildCodePart(art, nil)
	if err != nil {
		return nil, nil, err
	}
	codeSepIdx := ResolveMethodCodeSepOffset(art, prevScript, 0)
	preimage, err := ComputeOpPushTxPreimage(tx, 0, prevScript, prevOut.Satoshis, codeSepIdx, sighash.AllForkID)
	if err != nil {
		return nil, nil, fmt.Errorf("contracts: Bond Commit preimage: %w", err)
	}
	sigHash := core.DSHA256(preimage)
	sigWithFlag, pubCompressed, err := operatorSigner(ctx, prevState.OperatorPKH, sigHash)
	if err != nil {
		return nil, nil, fmt.Errorf("contracts: Bond Commit sign: %w", err)
	}
	if len(pubCompressed) != 33 || !bytes.Equal(core.Hash160(pubCompressed), prevState.OperatorPKH) {
		return nil, nil, errors.New("contracts: Bond Commit signer pubkey does not match operatorPkh")
	}

	unlock, err := BuildMethodUnlockingScript(art, "commit", codePart, []any{
		sigWithFlag,
		pubCompressed,
		newRoot,
		bondSats,
		newDeadline,
		make([]byte, 20),
		uint64(0),
		preimage,
	})
	if err != nil {
		return nil, nil, err
	}
	tx.Inputs[0].UnlockingScript = unlock

	if err := VerifyInputScriptChronicleCtx(ctx, tx, 0, prevOut); err != nil {
		return nil, nil, fmt.Errorf("contracts: NektarCheckpointBond.Commit failed Chronicle VM gate: %w", err)
	}
	return tx, &nextState, nil
}

// BuildBondSeizeTx constructs and Chronicle-VM-verifies a permissionless NektarCheckpointBond.Seize()
// slashing transaction once blockHeight >= Deadline! Pays payoutSats to payToScript (Output 0) and
// emits a 1-sat tombstoned Seized=1 continuation (Output 1) matching NektarCheckpointBond.runar.go:58-59.
func BuildBondSeizeTx(
	bondTx *transaction.Transaction,
	bondVout uint32,
	payToScript *script.Script,
	blockHeight uint32,
	minerFeeSats uint64,
) (*transaction.Transaction, *CheckpointBondState, error) {
	if bondTx == nil || int(bondVout) >= len(bondTx.Outputs) || bondTx.Outputs[bondVout] == nil || bondTx.Outputs[bondVout].LockingScript == nil {
		return nil, nil, errors.New("contracts: invalid bondTx or bondVout")
	}
	if payToScript == nil || len(*payToScript) == 0 {
		return nil, nil, errors.New("contracts: Bond Seize requires payToScript")
	}
	if blockHeight >= 500_000_000 {
		return nil, nil, fmt.Errorf("contracts: Bond Seize blockHeight %d must be < 500_000_000", blockHeight)
	}
	prevOut := bondTx.Outputs[bondVout]
	prevScript := []byte(*prevOut.LockingScript)
	prevState, err := DecodeCheckpointBond(prevScript)
	if err != nil {
		return nil, nil, err
	}
	if prevState.Seized {
		return nil, nil, errors.New("contracts: Bond Seize refused: bond is already seized")
	}
	if uint64(blockHeight) < prevState.Deadline {
		return nil, nil, fmt.Errorf("contracts: Bond Seize blockHeight %d < deadline %d (not slashable yet)", blockHeight, prevState.Deadline)
	}
	if minerFeeSats == 0 {
		minerFeeSats = min(prevState.MaxFee, uint64(400))
	}
	if minerFeeSats > prevState.MaxFee {
		return nil, nil, fmt.Errorf("contracts: Bond Seize fee %d exceeds maxFee %d", minerFeeSats, prevState.MaxFee)
	}
	if prevOut.Satoshis <= minerFeeSats+1 {
		return nil, nil, fmt.Errorf("contracts: bond balance %d insufficient for Seize fee %d + 1 sat tombstone", prevOut.Satoshis, minerFeeSats)
	}
	// Output 0 = payoutSats to slasher; Output 1 = 1 sat Seized=1 continuation; fee = in - payout - 1 <= MaxFee.
	payoutSats := prevOut.Satoshis - minerFeeSats - 1

	seizedState := *prevState
	seizedState.OperatorPKH = append([]byte(nil), prevState.OperatorPKH...)
	seizedState.LastRoot = append([]byte(nil), prevState.LastRoot...)
	seizedState.Seized = true
	seizedLock, err := EncodeCheckpointBondLockingScript(seizedState)
	if err != nil {
		return nil, nil, err
	}

	tx := transaction.NewTransaction()
	tx.LockTime = blockHeight
	tx.AddInput(&transaction.TransactionInput{
		SourceTXID:        bondTx.TxID(),
		SourceTxOutIndex:  bondVout,
		SourceTransaction: bondTx,
		SequenceNumber:    0xfffffffe, // F-02 non-final
	})
	// Exact source order from NektarCheckpointBond.runar.go:58-59: AddRawOutput FIRST, AddOutput SECOND!
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: payoutSats, LockingScript: payToScript})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: seizedLock})

	art, err := LoadFullArtifact("NektarCheckpointBond")
	if err != nil {
		return nil, nil, err
	}
	codePart, err := BuildCodePart(art, nil)
	if err != nil {
		return nil, nil, err
	}
	codeSepIdx := ResolveMethodCodeSepOffset(art, prevScript, 1)
	preimage, err := ComputeOpPushTxPreimage(tx, 0, prevScript, prevOut.Satoshis, codeSepIdx, sighash.AllForkID)
	if err != nil {
		return nil, nil, fmt.Errorf("contracts: Bond Seize preimage: %w", err)
	}

	unlock, err := BuildMethodUnlockingScript(art, "seize", codePart, []any{
		[]byte(*payToScript),
		payoutSats,
		make([]byte, 20),
		uint64(0),
		preimage,
	})
	if err != nil {
		return nil, nil, err
	}
	tx.Inputs[0].UnlockingScript = unlock

	if err := VerifyInputScriptChronicle(tx, 0, prevOut); err != nil {
		return nil, nil, fmt.Errorf("contracts: NektarCheckpointBond.Seize failed Chronicle VM gate: %w", err)
	}
	return tx, &seizedState, nil
}

// BuildBondCloseTx constructs, signs, and Chronicle-VM-verifies a terminal NektarCheckpointBond.Close()
// transaction returning the bond to P2PKH(OperatorPKH) once blockHeight >= BondReleaseHeight.
func BuildBondCloseTx(
	ctx context.Context,
	bondTx *transaction.Transaction,
	bondVout uint32,
	blockHeight uint32,
	minerFeeSats uint64,
	operatorSigner CovenantSighashSigner,
) (*transaction.Transaction, error) {
	if bondTx == nil || int(bondVout) >= len(bondTx.Outputs) || bondTx.Outputs[bondVout] == nil || bondTx.Outputs[bondVout].LockingScript == nil {
		return nil, errors.New("contracts: invalid bondTx or bondVout")
	}
	if operatorSigner == nil {
		return nil, errors.New("contracts: Bond Close requires operatorSigner")
	}
	if blockHeight >= 500_000_000 {
		return nil, fmt.Errorf("contracts: Bond Close blockHeight %d must be < 500_000_000", blockHeight)
	}
	prevOut := bondTx.Outputs[bondVout]
	prevScript := []byte(*prevOut.LockingScript)
	prevState, err := DecodeCheckpointBond(prevScript)
	if err != nil {
		return nil, err
	}
	if uint64(blockHeight) < prevState.BondReleaseHeight {
		return nil, fmt.Errorf("contracts: Bond Close blockHeight %d < bondReleaseHeight %d", blockHeight, prevState.BondReleaseHeight)
	}
	if minerFeeSats == 0 {
		minerFeeSats = min(prevState.MaxFee, uint64(350))
	}
	if minerFeeSats > prevState.MaxFee {
		return nil, fmt.Errorf("contracts: Bond Close fee %d exceeds maxFee %d", minerFeeSats, prevState.MaxFee)
	}
	if prevOut.Satoshis <= minerFeeSats {
		return nil, fmt.Errorf("contracts: bond balance %d insufficient for Close fee %d", prevOut.Satoshis, minerFeeSats)
	}
	payoutSats := prevOut.Satoshis - minerFeeSats
	opLock, err := P2PKHLockingScript(prevState.OperatorPKH)
	if err != nil {
		return nil, err
	}

	tx := transaction.NewTransaction()
	tx.LockTime = blockHeight
	tx.AddInput(&transaction.TransactionInput{
		SourceTXID:        bondTx.TxID(),
		SourceTxOutIndex:  bondVout,
		SourceTransaction: bondTx,
		SequenceNumber:    0xfffffffe, // F-02 non-final
	})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: payoutSats, LockingScript: opLock})

	art, err := LoadFullArtifact("NektarCheckpointBond")
	if err != nil {
		return nil, err
	}
	codePart, err := BuildCodePart(art, nil)
	if err != nil {
		return nil, err
	}
	codeSepIdx := ResolveMethodCodeSepOffset(art, prevScript, 2)
	preimage, err := ComputeOpPushTxPreimage(tx, 0, prevScript, prevOut.Satoshis, codeSepIdx, sighash.AllForkID)
	if err != nil {
		return nil, fmt.Errorf("contracts: Bond Close preimage: %w", err)
	}
	sigHash := core.DSHA256(preimage)
	sigWithFlag, pubCompressed, err := operatorSigner(ctx, prevState.OperatorPKH, sigHash)
	if err != nil {
		return nil, fmt.Errorf("contracts: Bond Close sign: %w", err)
	}
	if len(pubCompressed) != 33 || !bytes.Equal(core.Hash160(pubCompressed), prevState.OperatorPKH) {
		return nil, errors.New("contracts: Bond Close signer pubkey does not match operatorPkh")
	}

	unlock, err := BuildMethodUnlockingScript(art, "close", codePart, []any{
		sigWithFlag,
		pubCompressed,
		payoutSats,
		make([]byte, 20),
		uint64(0),
		preimage,
	})
	if err != nil {
		return nil, err
	}
	tx.Inputs[0].UnlockingScript = unlock

	if err := VerifyInputScriptChronicleCtx(ctx, tx, 0, prevOut); err != nil {
		return nil, fmt.Errorf("contracts: NektarCheckpointBond.Close failed Chronicle VM gate: %w", err)
	}
	return tx, nil
}
