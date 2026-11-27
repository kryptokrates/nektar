package contracts

import (
	"context"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/script/interpreter/scriptflag"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// ChronicleConsensusFlags matches post-Chronicle BSV L1 mined-block consensus rules
// (post-Genesis BSV consensus permits non-cleanstack in mined blocks, so lineage verification
// of already-mined anchor chains uses this flag set).
const ChronicleConsensusFlags = scriptflag.EnableSighashForkID |
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

// ChronicleRelayFlags matches post-Chronicle BSV L1 consensus + strict relay standardness policy:
// SIGHASH_FORKID, post-Genesis, post-Chronicle, strict low-S DER, MINIMALDATA,
// push-only unlocking scripts, NULLFAIL, and CLEANSTACK (go-sdk requires Bip16 alongside
// VerifyCleanStack; UTXOAfterGenesis disables P2SH evaluation for post-Genesis outputs).
const ChronicleRelayFlags = ChronicleConsensusFlags | scriptflag.Bip16 | scriptflag.VerifyCleanStack

// VerifyInputScriptChronicle executes the full post-Chronicle Bitcoin Script interpreter
// (go-sdk/script/interpreter) with strict relay flags (including CLEANSTACK) on tx.Inputs[inputIdx].
// Fail-closed pre-broadcast gate: every Rúnar covenant spending tx must pass this before broadcast.
func VerifyInputScriptChronicle(tx *transaction.Transaction, inputIdx int, prevOut *transaction.TransactionOutput) error {
	return VerifyInputScriptChronicleCtx(context.Background(), tx, inputIdx, prevOut)
}

// VerifyInputScriptChronicleCtx is the context-aware form of VerifyInputScriptChronicle.
func VerifyInputScriptChronicleCtx(ctx context.Context, tx *transaction.Transaction, inputIdx int, prevOut *transaction.TransactionOutput) error {
	return verifyInputScriptWithFlags(ctx, tx, inputIdx, prevOut, ChronicleRelayFlags)
}

// VerifyInputScriptChronicleConsensus executes post-Chronicle BSV mined-block consensus validation
// (without relay-only CLEANSTACK) for verifying already-mined covenant UTXO spend lineages.
func VerifyInputScriptChronicleConsensus(tx *transaction.Transaction, inputIdx int, prevOut *transaction.TransactionOutput) error {
	return verifyInputScriptWithFlags(context.Background(), tx, inputIdx, prevOut, ChronicleConsensusFlags)
}

func verifyInputScriptWithFlags(ctx context.Context, tx *transaction.Transaction, inputIdx int, prevOut *transaction.TransactionOutput, flags scriptflag.Flag) error {
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
		interpreter.WithFlags(flags),
		interpreter.WithAfterChronicle(),
		interpreter.WithContext(ctx),
	)
	if err != nil {
		return fmt.Errorf("contracts: script VM rejected input %d: %w", inputIdx, err)
	}
	return nil
}
