package contracts

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
	"github.com/lightwebinc/bcommon/keyed"

	"github.com/kryptokrates/nektar/internal/core"
)

// DisclosureSaleParams holds the exact 7 constructor parameters of DisclosureSale.runar.go.
type DisclosureSaleParams struct {
	SellerPKH       []byte // 20B hash160(seller payout pubkey)
	Price           uint64 // sats paid to SellerPKH on atomic key reveal
	HWrapped        []byte // 32B BRC-369 keyed.Commitment(k)
	RefundAfter     uint64 // block height (<500_000_000) or unix timestamp (>=500_000_000)
	MaxFee          uint64 // max miner fee deducted from escrow change buffer
	DepositorPKH    []byte // 20B hash160(buyer refund/change pubkey)
	VerdictOutpoint []byte // empty for ungated Buy(), or 36B outpoint for BuyGated()
}

// Validate checks all DisclosureSale constructor parameter invariants fail-closed.
func (p DisclosureSaleParams) Validate() error {
	if len(p.SellerPKH) != 20 {
		return fmt.Errorf("contracts: DisclosureSale sellerPkh must be 20 bytes, got %d", len(p.SellerPKH))
	}
	if len(p.DepositorPKH) != 20 {
		return fmt.Errorf("contracts: DisclosureSale depositorPkh must be 20 bytes, got %d", len(p.DepositorPKH))
	}
	if len(p.HWrapped) != 32 {
		return fmt.Errorf("contracts: DisclosureSale hWrapped must be 32 bytes, got %d", len(p.HWrapped))
	}
	if p.Price == 0 {
		return errors.New("contracts: DisclosureSale price must be > 0")
	}
	if p.MaxFee == 0 {
		return errors.New("contracts: DisclosureSale maxFee must be > 0")
	}
	if len(p.VerdictOutpoint) != 0 && len(p.VerdictOutpoint) != 36 {
		return fmt.Errorf("contracts: DisclosureSale verdictOutpoint must be 0 or 36 bytes, got %d", len(p.VerdictOutpoint))
	}
	return nil
}

// RequiredEscrowSatoshis returns the minimum escrow UTXO funding amount so Buy() has room
// for Price + MaxFee + at least 1 sat of depositor change (required by DisclosureSale's changeSats >= 1 check).
func (p DisclosureSaleParams) RequiredEscrowSatoshis() uint64 {
	return p.Price + p.MaxFee + 1
}

// EncodeDisclosureSaleLockingScript splices all 26 constructorSlots of DisclosureSale.json
// to produce the exact L1 locking script for this escrow instance.
func EncodeDisclosureSaleLockingScript(p DisclosureSaleParams) (*script.Script, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	art, err := LoadFullArtifact("DisclosureSale")
	if err != nil {
		return nil, err
	}
	verdict := p.VerdictOutpoint
	if verdict == nil {
		verdict = []byte{}
	}
	return BuildStatelessLockingScript(art, []any{
		p.SellerPKH,
		p.Price,
		p.HWrapped,
		p.RefundAfter,
		p.MaxFee,
		p.DepositorPKH,
		verdict,
	})
}

// BuildDisclosureSaleBuyTx constructs, unlocks, and Chronicle-VM-verifies the self-funded
// atomic Buy(wrapped, changeSats, txPreimage) transaction that pays Price to SellerPKH,
// returns unused fee buffer to DepositorPKH, and reveals the 32-byte BRC-369 content key `wrapped`
// directly on BSV L1 in Input 0's unlocking script!
func BuildDisclosureSaleBuyTx(
	escrowTx *transaction.Transaction,
	escrowVout uint32,
	p DisclosureSaleParams,
	wrappedKey32 []byte,
	minerFeeSats uint64,
) (*transaction.Transaction, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if len(p.VerdictOutpoint) != 0 {
		return nil, errors.New("contracts: Buy() cannot spend verdict-gated DisclosureSale (use BuyGated)")
	}
	if escrowTx == nil || int(escrowVout) >= len(escrowTx.Outputs) || escrowTx.Outputs[escrowVout] == nil {
		return nil, errors.New("contracts: invalid escrowTx or escrowVout")
	}
	if len(wrappedKey32) != 32 {
		return nil, fmt.Errorf("contracts: DisclosureSale Buy requires 32-byte BRC-369 content key, got %d", len(wrappedKey32))
	}
	// Pre-validate BRC-369 keyed commitment locally before even touching Script VM.
	var wantCommit [32]byte
	copy(wantCommit[:], p.HWrapped)
	if _, err := keyed.CheckOpened(wrappedKey32, wantCommit); err != nil {
		return nil, fmt.Errorf("contracts: Buy wrappedKey does not match BRC-369 hWrapped: %w", err)
	}

	prevOut := escrowTx.Outputs[escrowVout]
	if prevOut.LockingScript == nil {
		return nil, errors.New("contracts: escrow output has nil locking script")
	}
	expectedLock, err := EncodeDisclosureSaleLockingScript(p)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(*prevOut.LockingScript, *expectedLock) {
		return nil, errors.New("contracts: escrow locking script does not match DisclosureSaleParams")
	}
	if minerFeeSats == 0 {
		minerFeeSats = min(p.MaxFee, uint64(300))
	}
	if minerFeeSats > p.MaxFee {
		return nil, fmt.Errorf("contracts: Buy minerFeeSats %d exceeds MaxFee %d", minerFeeSats, p.MaxFee)
	}
	if prevOut.Satoshis <= p.Price+minerFeeSats {
		return nil, fmt.Errorf("contracts: escrow balance %d too small for price=%d + fee=%d + minChange=1",
			prevOut.Satoshis, p.Price, minerFeeSats)
	}
	changeSats := prevOut.Satoshis - p.Price - minerFeeSats

	sellerLock, err := P2PKHLockingScript(p.SellerPKH)
	if err != nil {
		return nil, err
	}
	depositorLock, err := P2PKHLockingScript(p.DepositorPKH)
	if err != nil {
		return nil, err
	}

	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{
		SourceTXID:        escrowTx.TxID(),
		SourceTxOutIndex:  escrowVout,
		SourceTransaction: escrowTx,
		SequenceNumber:    0xffffffff,
	})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: p.Price, LockingScript: sellerLock})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: changeSats, LockingScript: depositorLock})

	art, err := LoadFullArtifact("DisclosureSale")
	if err != nil {
		return nil, err
	}
	prevScript := []byte(*prevOut.LockingScript)
	codeSepIdx := ResolveMethodCodeSepOffset(art, prevScript, 0) // method 0 = buy
	preimage, err := ComputeOpPushTxPreimage(tx, 0, prevScript, prevOut.Satoshis, codeSepIdx, sighash.AllForkID)
	if err != nil {
		return nil, fmt.Errorf("contracts: Buy preimage: %w", err)
	}
	unlock, err := BuildMethodUnlockingScript(art, "buy", nil, []any{
		wrappedKey32,
		changeSats,
		preimage,
	})
	if err != nil {
		return nil, err
	}
	tx.Inputs[0].UnlockingScript = unlock

	if err := VerifyInputScriptChronicle(tx, 0, prevOut); err != nil {
		return nil, fmt.Errorf("contracts: DisclosureSale.Buy failed Chronicle VM gate: %w", err)
	}
	return tx, nil
}

// BuildDisclosureSaleRefundTx constructs, signs, unlocks, and Chronicle-VM-verifies the self-funded
// Refund(sig, pubKey, changeSats, txPreimage) transaction reclaiming an unrevealed escrow after RefundAfter.
// Enforces F-02 consensus locktime (nSequence=0xfffffffe, nLockTime=lockTime in matching domain >= RefundAfter).
func BuildDisclosureSaleRefundTx(
	ctx context.Context,
	escrowTx *transaction.Transaction,
	escrowVout uint32,
	p DisclosureSaleParams,
	lockTime uint32,
	minerFeeSats uint64,
	signer CovenantSighashSigner,
) (*transaction.Transaction, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if escrowTx == nil || int(escrowVout) >= len(escrowTx.Outputs) || escrowTx.Outputs[escrowVout] == nil {
		return nil, errors.New("contracts: invalid escrowTx or escrowVout")
	}
	if signer == nil {
		return nil, errors.New("contracts: Refund requires depositor signer")
	}
	if uint64(lockTime) < p.RefundAfter {
		return nil, fmt.Errorf("contracts: Refund lockTime %d < refundAfter %d", lockTime, p.RefundAfter)
	}
	if (p.RefundAfter < 500_000_000) != (lockTime < 500_000_000) {
		return nil, fmt.Errorf("contracts: Refund lockTime %d domain mismatches refundAfter %d", lockTime, p.RefundAfter)
	}
	prevOut := escrowTx.Outputs[escrowVout]
	if prevOut.LockingScript == nil {
		return nil, errors.New("contracts: escrow output has nil locking script")
	}
	expectedLock, err := EncodeDisclosureSaleLockingScript(p)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(*prevOut.LockingScript, *expectedLock) {
		return nil, errors.New("contracts: escrow locking script does not match DisclosureSaleParams")
	}
	if minerFeeSats == 0 {
		minerFeeSats = min(p.MaxFee, uint64(300))
	}
	if minerFeeSats > p.MaxFee {
		return nil, fmt.Errorf("contracts: Refund minerFeeSats %d exceeds MaxFee %d", minerFeeSats, p.MaxFee)
	}
	if prevOut.Satoshis <= minerFeeSats {
		return nil, fmt.Errorf("contracts: escrow balance %d too small for refund fee %d", prevOut.Satoshis, minerFeeSats)
	}
	changeSats := prevOut.Satoshis - minerFeeSats
	depositorLock, err := P2PKHLockingScript(p.DepositorPKH)
	if err != nil {
		return nil, err
	}

	tx := transaction.NewTransaction()
	tx.LockTime = lockTime
	tx.AddInput(&transaction.TransactionInput{
		SourceTXID:        escrowTx.TxID(),
		SourceTxOutIndex:  escrowVout,
		SourceTransaction: escrowTx,
		SequenceNumber:    0xfffffffe, // F-02: non-final so consensus enforces nLockTime!
	})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: changeSats, LockingScript: depositorLock})

	art, err := LoadFullArtifact("DisclosureSale")
	if err != nil {
		return nil, err
	}
	prevScript := []byte(*prevOut.LockingScript)
	codeSepIdx := ResolveMethodCodeSepOffset(art, prevScript, 2) // method 2 = refund
	preimage, err := ComputeOpPushTxPreimage(tx, 0, prevScript, prevOut.Satoshis, codeSepIdx, sighash.AllForkID)
	if err != nil {
		return nil, fmt.Errorf("contracts: Refund preimage: %w", err)
	}
	sigHash := core.DSHA256(preimage)
	sigWithFlag, pubCompressed, err := signer(ctx, p.DepositorPKH, sigHash)
	if err != nil {
		return nil, fmt.Errorf("contracts: Refund sign: %w", err)
	}
	if len(pubCompressed) != 33 || !bytes.Equal(core.Hash160(pubCompressed), p.DepositorPKH) {
		return nil, errors.New("contracts: Refund signer pubkey does not match depositorPkh")
	}

	unlock, err := BuildMethodUnlockingScript(art, "refund", nil, []any{
		sigWithFlag,
		pubCompressed,
		changeSats,
		preimage,
	})
	if err != nil {
		return nil, err
	}
	tx.Inputs[0].UnlockingScript = unlock

	if err := VerifyInputScriptChronicleCtx(ctx, tx, 0, prevOut); err != nil {
		return nil, fmt.Errorf("contracts: DisclosureSale.Refund failed Chronicle VM gate: %w", err)
	}
	return tx, nil
}

// ExtractRevealedKeyFromBuyTx recovers the 32-byte BRC-369 content key directly from a mined or
// mempool DisclosureSale.Buy / BuyGated spending transaction's unlocking script, verifying that
// keyed.Commitment(k) equals expectedHWrappedHex (even if the seller vanished off HTTP/bbox!).
func ExtractRevealedKeyFromBuyTx(spendTx *transaction.Transaction, inputIdx int, expectedHWrappedHex string) ([]byte, error) {
	if spendTx == nil || inputIdx < 0 || inputIdx >= len(spendTx.Inputs) || spendTx.Inputs[inputIdx] == nil || spendTx.Inputs[inputIdx].UnlockingScript == nil {
		return nil, errors.New("contracts: invalid spendTx or input unlocking script")
	}
	wantBytes, err := hex.DecodeString(expectedHWrappedHex)
	if err != nil || len(wantBytes) != 32 {
		return nil, errors.New("contracts: expectedHWrappedHex must be 32-byte hex")
	}
	var wantCommit [32]byte
	copy(wantCommit[:], wantBytes)

	pushes, err := ExtractUnlockPushes(*spendTx.Inputs[inputIdx].UnlockingScript)
	if err != nil {
		return nil, fmt.Errorf("contracts: parse Buy unlocking script: %w", err)
	}
	// Buy: [wrapped32, changeSats, txPreimage, OP_0] (4 pushes)
	// BuyGated: [wrapped32, changeSats, prevouts, txPreimage, OP_1] (5 pushes)
	if len(pushes) != 4 && len(pushes) != 5 {
		return nil, fmt.Errorf("contracts: unexpected DisclosureSale Buy push count %d", len(pushes))
	}
	selector := pushes[len(pushes)-1]
	isBuy := len(pushes) == 4 && len(selector) == 0
	isBuyGated := len(pushes) == 5 && len(selector) == 1 && selector[0] == 1
	if !isBuy && !isBuyGated {
		return nil, errors.New("contracts: unlocking script is not a DisclosureSale Buy/BuyGated call")
	}
	wrapped := pushes[0]
	if len(wrapped) != 32 {
		return nil, fmt.Errorf("contracts: revealed key length %d != 32", len(wrapped))
	}
	if _, err := keyed.CheckOpened(wrapped, wantCommit); err != nil {
		return nil, fmt.Errorf("contracts: revealed key fails BRC-369 commitment check: %w", err)
	}
	return append([]byte(nil), wrapped...), nil
}
