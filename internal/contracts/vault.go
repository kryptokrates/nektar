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

// AgentVaultState holds the full on-chain consensus state of a NektarAgentVault UTXO.
type AgentVaultState struct {
	OperatorPKH   []byte `json:"operatorPkh"`
	AgentPKH      []byte `json:"agentPkh"`
	PerTxCap      uint64 `json:"perTxCap"`
	WindowCap     uint64 `json:"windowCap"`
	WindowBlocks  uint64 `json:"windowBlocks"`
	MaxFee        uint64 `json:"maxFee"`
	SpentInWindow uint64 `json:"spentInWindow"`
	WindowStart   uint64 `json:"windowStart"`
	Paused        bool   `json:"paused"`
}

// Validate checks NektarAgentVault state field invariants fail-closed.
func (s AgentVaultState) Validate() error {
	if len(s.OperatorPKH) != 20 {
		return fmt.Errorf("contracts: vault operatorPkh must be 20 bytes, got %d", len(s.OperatorPKH))
	}
	if len(s.AgentPKH) != 20 {
		return fmt.Errorf("contracts: vault agentPkh must be 20 bytes, got %d", len(s.AgentPKH))
	}
	if s.PerTxCap == 0 || s.WindowCap < s.PerTxCap || s.WindowBlocks == 0 || s.MaxFee == 0 {
		return errors.New("contracts: vault caps/window/maxFee must be positive with windowCap >= perTxCap")
	}
	return nil
}

func (s AgentVaultState) toStateMap() map[string]any {
	paused := int64(0)
	if s.Paused {
		paused = 1
	}
	return map[string]any{
		"operatorPkh":   s.OperatorPKH,
		"agentPkh":      s.AgentPKH,
		"perTxCap":      int64(s.PerTxCap),
		"windowCap":     int64(s.WindowCap),
		"windowBlocks":  int64(s.WindowBlocks),
		"maxFee":        int64(s.MaxFee),
		"spentInWindow": int64(s.SpentInWindow),
		"windowStart":   int64(s.WindowStart),
		"paused":        paused,
	}
}

// EncodeAgentVaultLockingScript builds the L1-spendable NektarAgentVault locking script
// (<patched NektarAgentVault code> OP_RETURN <serialized state>).
func EncodeAgentVaultLockingScript(st AgentVaultState) (*script.Script, error) {
	if err := st.Validate(); err != nil {
		return nil, err
	}
	art, err := LoadFullArtifact("NektarAgentVault")
	if err != nil {
		return nil, err
	}
	return BuildStatefulLockingScript(art, nil, st.toStateMap())
}

// DecodeAgentVault verifies that scriptBytes matches the embedded NektarAgentVault code prefix
// and decodes its OP_RETURN state payload into AgentVaultState.
func DecodeAgentVault(scriptBytes []byte) (*AgentVaultState, error) {
	art, err := LoadFullArtifact("NektarAgentVault")
	if err != nil {
		return nil, err
	}
	codePart, err := BuildCodePart(art, nil)
	if err != nil {
		return nil, err
	}
	if len(scriptBytes) <= len(codePart) || !bytes.Equal(scriptBytes[:len(codePart)], codePart) || scriptBytes[len(codePart)] != 0x6a {
		return nil, errors.New("contracts: locking script code does not match NektarAgentVault artifact")
	}
	fields, err := DeserializeStateFields(art.StateFields, scriptBytes[len(codePart)+1:])
	if err != nil {
		return nil, err
	}
	st := &AgentVaultState{
		OperatorPKH:   fields["operatorPkh"].([]byte),
		AgentPKH:      fields["agentPkh"].([]byte),
		PerTxCap:      uint64(fields["perTxCap"].(int64)),
		WindowCap:     uint64(fields["windowCap"].(int64)),
		WindowBlocks:  uint64(fields["windowBlocks"].(int64)),
		MaxFee:        uint64(fields["maxFee"].(int64)),
		SpentInWindow: uint64(fields["spentInWindow"].(int64)),
		WindowStart:   uint64(fields["windowStart"].(int64)),
		Paused:        fields["paused"].(int64) != 0,
	}
	if err := st.Validate(); err != nil {
		return nil, err
	}
	return st, nil
}

// BuildAgentVaultSpendTx constructs, unlocks, and Chronicle-VM-verifies an autonomous hot-agent
// NektarAgentVault.AgentSpend() transaction spending vaultTx:vaultVout into:
//   - Output 0: continued NektarAgentVault with updated SpentInWindow / WindowStart
//   - Output 1: payAmount sats locked to destScript (e.g. a DisclosureSale escrow OR P2PKH!)
// Enforces F-02 consensus locktime (nSequence=0xfffffffe, nLockTime=blockHeight < 500_000_000).
func BuildAgentVaultSpendTx(
	ctx context.Context,
	vaultTx *transaction.Transaction,
	vaultVout uint32,
	destScript *script.Script,
	payAmount uint64,
	blockHeight uint32,
	minerFeeSats uint64,
	agentSigner CovenantSighashSigner,
) (*transaction.Transaction, *AgentVaultState, error) {
	if vaultTx == nil || int(vaultVout) >= len(vaultTx.Outputs) || vaultTx.Outputs[vaultVout] == nil || vaultTx.Outputs[vaultVout].LockingScript == nil {
		return nil, nil, errors.New("contracts: invalid vaultTx or vaultVout")
	}
	if destScript == nil || len(*destScript) == 0 {
		return nil, nil, errors.New("contracts: AgentSpend requires non-empty destScript")
	}
	if agentSigner == nil {
		return nil, nil, errors.New("contracts: AgentSpend requires agentSigner")
	}
	if blockHeight >= 500_000_000 {
		return nil, nil, fmt.Errorf("contracts: AgentSpend blockHeight %d must be < 500_000_000", blockHeight)
	}
	prevOut := vaultTx.Outputs[vaultVout]
	prevScript := []byte(*prevOut.LockingScript)
	prevState, err := DecodeAgentVault(prevScript)
	if err != nil {
		return nil, nil, err
	}
	if prevState.Paused {
		return nil, nil, errors.New("contracts: AgentSpend refused: vault is paused by operator")
	}
	if payAmount == 0 || payAmount > prevState.PerTxCap {
		return nil, nil, fmt.Errorf("contracts: AgentSpend payAmount %d outside [1, perTxCap=%d]", payAmount, prevState.PerTxCap)
	}
	if uint64(blockHeight) < prevState.WindowStart {
		return nil, nil, fmt.Errorf("contracts: AgentSpend blockHeight %d < windowStart %d", blockHeight, prevState.WindowStart)
	}
	if minerFeeSats == 0 {
		minerFeeSats = min(prevState.MaxFee, uint64(450))
	}
	if minerFeeSats > prevState.MaxFee {
		return nil, nil, fmt.Errorf("contracts: AgentSpend minerFeeSats %d exceeds maxFee %d", minerFeeSats, prevState.MaxFee)
	}
	if prevOut.Satoshis <= payAmount+minerFeeSats {
		return nil, nil, fmt.Errorf("contracts: vault balance %d insufficient for payAmount=%d + fee=%d + minCont=1",
			prevOut.Satoshis, payAmount, minerFeeSats)
	}
	contSats := prevOut.Satoshis - payAmount - minerFeeSats

	var newSpent, newStart uint64
	if uint64(blockHeight) >= prevState.WindowStart+prevState.WindowBlocks {
		newSpent = payAmount
		newStart = uint64(blockHeight)
	} else {
		newSpent = prevState.SpentInWindow + payAmount
		newStart = prevState.WindowStart
	}
	if newSpent > prevState.WindowCap {
		return nil, nil, fmt.Errorf("contracts: AgentSpend rolling window exceeded (%d > windowCap=%d)", newSpent, prevState.WindowCap)
	}

	nextState := *prevState
	nextState.SpentInWindow = newSpent
	nextState.WindowStart = newStart
	nextLock, err := EncodeAgentVaultLockingScript(nextState)
	if err != nil {
		return nil, nil, err
	}

	tx := transaction.NewTransaction()
	tx.LockTime = blockHeight
	tx.AddInput(&transaction.TransactionInput{
		SourceTXID:        vaultTx.TxID(),
		SourceTxOutIndex:  vaultVout,
		SourceTransaction: vaultTx,
		SequenceNumber:    0xfffffffe, // F-02: non-final so consensus enforces nLockTime!
	})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: contSats, LockingScript: nextLock})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: payAmount, LockingScript: destScript})

	art, err := LoadFullArtifact("NektarAgentVault")
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
		return nil, nil, fmt.Errorf("contracts: AgentSpend preimage: %w", err)
	}
	sigHash := core.DSHA256(preimage)
	sigWithFlag, pubCompressed, err := agentSigner(ctx, prevState.AgentPKH, sigHash)
	if err != nil {
		return nil, nil, fmt.Errorf("contracts: AgentSpend sign: %w", err)
	}
	if len(pubCompressed) != 33 || !bytes.Equal(core.Hash160(pubCompressed), prevState.AgentPKH) {
		return nil, nil, errors.New("contracts: AgentSpend signer pubkey does not match vault agentPkh")
	}

	zeroPKH := make([]byte, 20)
	unlock, err := BuildMethodUnlockingScript(art, "agentSpend", codePart, []any{
		sigWithFlag,
		pubCompressed,
		[]byte(*destScript),
		payAmount,
		newSpent,
		newStart,
		contSats,
		zeroPKH,
		uint64(0),
		preimage,
	})
	if err != nil {
		return nil, nil, err
	}
	tx.Inputs[0].UnlockingScript = unlock

	if err := VerifyInputScriptChronicleCtx(ctx, tx, 0, prevOut); err != nil {
		return nil, nil, fmt.Errorf("contracts: NektarAgentVault.AgentSpend failed Chronicle VM gate: %w", err)
	}
	return tx, &nextState, nil
}

// OperatorControlUpdate specifies the new vault parameters applied by the cold operator.
type OperatorControlUpdate struct {
	NewAgentPKH     []byte // nil => keep existing AgentPKH
	NewPerTxCap     uint64 // 0 => keep existing
	NewWindowCap    uint64 // 0 => keep existing
	NewWindowBlocks uint64 // 0 => keep existing
	NewMaxFee       uint64 // 0 => keep existing
	NewPaused       *bool  // nil => keep existing
	MinerFeeSats    uint64 // 0 => default 400
}

// BuildAgentVaultOperatorControlTx builds, signs (with the COLD operator key), and Chronicle-VM-verifies
// a NektarAgentVault.OperatorControl() tx to pause/resume, rotate AgentPKH, or adjust caps.
func BuildAgentVaultOperatorControlTx(
	ctx context.Context,
	vaultTx *transaction.Transaction,
	vaultVout uint32,
	upd OperatorControlUpdate,
	operatorSigner CovenantSighashSigner,
) (*transaction.Transaction, *AgentVaultState, error) {
	if vaultTx == nil || int(vaultVout) >= len(vaultTx.Outputs) || vaultTx.Outputs[vaultVout] == nil || vaultTx.Outputs[vaultVout].LockingScript == nil {
		return nil, nil, errors.New("contracts: invalid vaultTx or vaultVout")
	}
	if operatorSigner == nil {
		return nil, nil, errors.New("contracts: OperatorControl requires operatorSigner")
	}
	prevOut := vaultTx.Outputs[vaultVout]
	prevScript := []byte(*prevOut.LockingScript)
	prevState, err := DecodeAgentVault(prevScript)
	if err != nil {
		return nil, nil, err
	}
	nextState := *prevState
	nextState.OperatorPKH = append([]byte(nil), prevState.OperatorPKH...)
	nextState.AgentPKH = append([]byte(nil), prevState.AgentPKH...)
	if len(upd.NewAgentPKH) > 0 {
		nextState.AgentPKH = append([]byte(nil), upd.NewAgentPKH...)
	}
	if upd.NewPerTxCap > 0 {
		nextState.PerTxCap = upd.NewPerTxCap
	}
	if upd.NewWindowCap > 0 {
		nextState.WindowCap = upd.NewWindowCap
	}
	if upd.NewWindowBlocks > 0 {
		nextState.WindowBlocks = upd.NewWindowBlocks
	}
	if upd.NewMaxFee > 0 {
		nextState.MaxFee = upd.NewMaxFee
	}
	if upd.NewPaused != nil {
		nextState.Paused = *upd.NewPaused
	}
	if err := nextState.Validate(); err != nil {
		return nil, nil, err
	}
	fee := upd.MinerFeeSats
	if fee == 0 {
		fee = min(prevState.MaxFee, uint64(400))
	}
	if prevOut.Satoshis <= fee {
		return nil, nil, fmt.Errorf("contracts: vault balance %d insufficient for OperatorControl fee %d", prevOut.Satoshis, fee)
	}
	contSats := prevOut.Satoshis - fee

	nextLock, err := EncodeAgentVaultLockingScript(nextState)
	if err != nil {
		return nil, nil, err
	}

	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{
		SourceTXID:        vaultTx.TxID(),
		SourceTxOutIndex:  vaultVout,
		SourceTransaction: vaultTx,
		SequenceNumber:    0xffffffff,
	})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: contSats, LockingScript: nextLock})

	art, err := LoadFullArtifact("NektarAgentVault")
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
		return nil, nil, fmt.Errorf("contracts: OperatorControl preimage: %w", err)
	}
	sigHash := core.DSHA256(preimage)
	sigWithFlag, pubCompressed, err := operatorSigner(ctx, prevState.OperatorPKH, sigHash)
	if err != nil {
		return nil, nil, fmt.Errorf("contracts: OperatorControl sign: %w", err)
	}
	if len(pubCompressed) != 33 || !bytes.Equal(core.Hash160(pubCompressed), prevState.OperatorPKH) {
		return nil, nil, errors.New("contracts: OperatorControl signer pubkey does not match vault operatorPkh")
	}

	newPausedInt := int64(0)
	if nextState.Paused {
		newPausedInt = 1
	}
	zeroPKH := make([]byte, 20)
	unlock, err := BuildMethodUnlockingScript(art, "operatorControl", codePart, []any{
		sigWithFlag,
		pubCompressed,
		nextState.AgentPKH,
		nextState.PerTxCap,
		nextState.WindowCap,
		nextState.WindowBlocks,
		nextState.MaxFee,
		newPausedInt,
		contSats,
		zeroPKH,
		uint64(0),
		preimage,
	})
	if err != nil {
		return nil, nil, err
	}
	tx.Inputs[0].UnlockingScript = unlock

	if err := VerifyInputScriptChronicleCtx(ctx, tx, 0, prevOut); err != nil {
		return nil, nil, fmt.Errorf("contracts: NektarAgentVault.OperatorControl failed Chronicle VM gate: %w", err)
	}
	return tx, &nextState, nil
}

// BuildAgentVaultSweepTx builds, signs (with the COLD operator key), and Chronicle-VM-verifies
// a terminal NektarAgentVault.OperatorSweep() tx closing the vault and sweeping all remaining sats to destPKH.
func BuildAgentVaultSweepTx(
	ctx context.Context,
	vaultTx *transaction.Transaction,
	vaultVout uint32,
	destPKH []byte,
	minerFeeSats uint64,
	operatorSigner CovenantSighashSigner,
) (*transaction.Transaction, error) {
	if vaultTx == nil || int(vaultVout) >= len(vaultTx.Outputs) || vaultTx.Outputs[vaultVout] == nil || vaultTx.Outputs[vaultVout].LockingScript == nil {
		return nil, errors.New("contracts: invalid vaultTx or vaultVout")
	}
	if len(destPKH) != 20 {
		return nil, fmt.Errorf("contracts: OperatorSweep destPKH must be 20 bytes, got %d", len(destPKH))
	}
	if operatorSigner == nil {
		return nil, errors.New("contracts: OperatorSweep requires operatorSigner")
	}
	prevOut := vaultTx.Outputs[vaultVout]
	prevScript := []byte(*prevOut.LockingScript)
	prevState, err := DecodeAgentVault(prevScript)
	if err != nil {
		return nil, err
	}
	if minerFeeSats == 0 {
		minerFeeSats = min(prevState.MaxFee, uint64(350))
	}
	if prevOut.Satoshis <= minerFeeSats {
		return nil, fmt.Errorf("contracts: vault balance %d insufficient for OperatorSweep fee %d", prevOut.Satoshis, minerFeeSats)
	}
	sweepSats := prevOut.Satoshis - minerFeeSats
	destLock, err := P2PKHLockingScript(destPKH)
	if err != nil {
		return nil, err
	}

	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{
		SourceTXID:        vaultTx.TxID(),
		SourceTxOutIndex:  vaultVout,
		SourceTransaction: vaultTx,
		SequenceNumber:    0xffffffff,
	})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: sweepSats, LockingScript: destLock})

	art, err := LoadFullArtifact("NektarAgentVault")
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
		return nil, fmt.Errorf("contracts: OperatorSweep preimage: %w", err)
	}
	sigHash := core.DSHA256(preimage)
	sigWithFlag, pubCompressed, err := operatorSigner(ctx, prevState.OperatorPKH, sigHash)
	if err != nil {
		return nil, fmt.Errorf("contracts: OperatorSweep sign: %w", err)
	}
	if len(pubCompressed) != 33 || !bytes.Equal(core.Hash160(pubCompressed), prevState.OperatorPKH) {
		return nil, errors.New("contracts: OperatorSweep signer pubkey does not match vault operatorPkh")
	}

	zeroPKH := make([]byte, 20)
	unlock, err := BuildMethodUnlockingScript(art, "operatorSweep", codePart, []any{
		sigWithFlag,
		pubCompressed,
		destPKH,
		sweepSats,
		zeroPKH,
		uint64(0),
		preimage,
	})
	if err != nil {
		return nil, err
	}
	tx.Inputs[0].UnlockingScript = unlock

	if err := VerifyInputScriptChronicleCtx(ctx, tx, 0, prevOut); err != nil {
		return nil, fmt.Errorf("contracts: NektarAgentVault.OperatorSweep failed Chronicle VM gate: %w", err)
	}
	return tx, nil
}
