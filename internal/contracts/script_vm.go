package contracts

import (
	"context"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/script/interpreter/scriptflag"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// ChronicleRelayFlags matches post-Chronicle BSV L1 consensus + strict relay policy:
// SIGHASH_FORKID, post-Genesis, post-Chronicle, strict low-S DER, MINIMALDATA,
// push-only unlocking scripts, NULLFAIL, and CLEANSTACK.
const ChronicleRelayFlags = scriptflag.EnableSighashForkID |
	scriptflag.UTXOAfterGenesis |
	scriptflag.UTXOAfterChronicle |
	scriptflag.Genesis |
	scriptflag.Chronicle |
	scriptflag.VerifyStrictEncoding |
	scriptflag.VerifyDERSignatures |
	scriptflag.VerifyLowS |
	scriptflag.VerifyMinimalData |
	scriptflag.VerifySigPushOnly |
	scriptflag.VerifyNullFail

// VerifyInputScriptChronicle executes the full post-Chronicle Bitcoin Script interpreter
// (go-sdk/script/interpreter) on tx.Inputs[inputIdx] against prevOut.
// Fail-closed pre-broadcast gate: every Rúnar covenant spending tx must pass this before broadcast.
func VerifyInputScriptChronicle(tx *transaction.Transaction, inputIdx int, prevOut *transaction.TransactionOutput) error {
	return VerifyInputScriptChronicleCtx(context.Background(), tx, inputIdx, prevOut)
}

// VerifyInputScriptChronicleCtx is the context-aware form of VerifyInputScriptChronicle.
func VerifyInputScriptChronicleCtx(ctx context.Context, tx *transaction.Transaction, inputIdx int, prevOut *transaction.TransactionOutput) error {
	if tx == nil {
		return errors.New("contracts: script VM: nil transaction")
	}
	if inputIdx < 0 || inputIdx >= len(tx.Inputs) {
		return fmt.Errorf("contracts: script VM: input index %d out of range (%d inputs)", inputIdx, len(tx.Inputs))
	}
	in := tx.Inputs[inputIdx]
	if in == nil || in.UnlockingScript == nil {
		return fmt.Errorf("contracts: script VM: input %d has nil unlocking script", inputIdx)
	}
	if prevOut == nil {
		prevOut = in.SourceTxOutput()
	}
	if prevOut == nil || prevOut.LockingScript == nil {
		return fmt.Errorf("contracts: script VM: input %d missing source locking script", inputIdx)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	err := interpreter.NewEngine().Execute(
		interpreter.WithTx(tx, inputIdx, prevOut),
		interpreter.WithFlags(ChronicleRelayFlags),
		interpreter.WithAfterChronicle(),
		interpreter.WithContext(ctx),
	)
	if err != nil {
		return fmt.Errorf("contracts: script VM rejected input %d: %w", inputIdx, err)
	}
	return nil
}

// VerifyTxScriptsChronicle verifies every input of tx whose source output is attached.
// Returns an error if tx has zero inputs or any input fails post-Chronicle Script execution.
func VerifyTxScriptsChronicle(tx *transaction.Transaction) error {
	if tx == nil {
		return errors.New("contracts: script VM: nil transaction")
	}
	if len(tx.Inputs) == 0 {
		return errors.New("contracts: script VM: transaction has no inputs")
	}
	for i, in := range tx.Inputs {
		if in == nil {
			return fmt.Errorf("contracts: script VM: nil input %d", i)
		}
		prevOut := in.SourceTxOutput()
		if prevOut == nil {
			return fmt.Errorf("contracts: script VM: input %d missing source output", i)
		}
		if err := VerifyInputScriptChronicle(tx, i, prevOut); err != nil {
			return err
		}
	}
	return nil
}
