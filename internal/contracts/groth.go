package contracts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/kryptokrates/nektar/internal/core"
)

// GrothSettlementResult summarizes verified consensus + statement bindings of a NektarGrothClaim settlement tx.
type GrothSettlementResult struct {
	SettleTxID        string `json:"settleTxid"`
	VerifierTxID      string `json:"verifierTxid"`
	VerifierVKHash    string `json:"verifierVkHash"`
	DocPushStatement  string `json:"docPushStatement"`
	JournalRoot       string `json:"journalRoot"`
	BlockHeight       uint32 `json:"blockHeight,omitempty"`
	Depth             int    `json:"depth,omitempty"`
	StructuralBound   bool   `json:"structuralBound"`
	Verified          bool   `json:"verified"` // true ONLY after witnessed-header BUMP + settleDepth + spv.Verify!
}

// ExtractGrothDocPush extracts the leading doc-push statement root from a Groth16 verifier locking script
// formatted as `<statement-root> <index> OP_2DROP <groth16-verifier-code>`.
// Returns (statementBytes, remainingVerifierCode, error).
func ExtractGrothDocPush(verifierLockScript []byte) ([]byte, []byte, error) {
	if len(verifierLockScript) < 4 {
		return nil, nil, errors.New("groth: verifier locking script too short")
	}
	// First push: statement root (direct push 1..75 bytes).
	stmtLen := int(verifierLockScript[0])
	if stmtLen == 0 || stmtLen > 75 || 1+stmtLen >= len(verifierLockScript) {
		return nil, nil, errors.New("groth: invalid statement doc-push length")
	}
	stmt := append([]byte(nil), verifierLockScript[1:1+stmtLen]...)
	off := 1 + stmtLen

	// Second push: index (OP_0 / OP_1..OP_16 or 1..75 byte direct push).
	op := verifierLockScript[off]
	switch {
	case op == 0x00 || (op >= 0x51 && op <= 0x60):
		off++
	case op >= 1 && op <= 75 && off+1+int(op) < len(verifierLockScript):
		off += 1 + int(op)
	default:
		return nil, nil, errors.New("groth: invalid index doc-push opcode")
	}
	// Expect OP_2DROP (0x6d).
	if off >= len(verifierLockScript) || verifierLockScript[off] != 0x6d {
		return nil, nil, errors.New("groth: missing OP_2DROP after doc-push prefix")
	}
	vkCode := verifierLockScript[off+1:]
	if len(vkCode) == 0 {
		return nil, nil, errors.New("groth: empty Groth16 verifier code after OP_2DROP")
	}
	return stmt, vkCode, nil
}

// VerifyGrothDocPushBinding performs structural off-chain F-18 doc-push binding checks ONLY:
//   1. Verifies settleTx has 2 inputs and input[1] names verifierOutpoint (verifierTx:verifierVout).
//   2. Parses verifierTx's locking script doc-push prefix `<statement-root> <index> OP_2DROP <vkCode>`.
//   3. Verifies sha256(vkCode) == expectedVKScriptHashHex and statement-root == expectedJournalRoot.
//
// It NEVER sets Verified=true (use VerifyGrothClaimSettlementSPV with a witnessed ChainTracker + minDepth
// to verify mined BUMP settlement and script execution).
func VerifyGrothDocPushBinding(
	settleTx *transaction.Transaction,
	verifierTx *transaction.Transaction,
	verifierVout uint32,
	expectedVKScriptHashHex string,
	expectedJournalRoot []byte,
) (*GrothSettlementResult, error) {
	if settleTx == nil || verifierTx == nil {
		return nil, errors.New("groth: settleTx and verifierTx are required")
	}
	if len(settleTx.Inputs) != 2 {
		return nil, fmt.Errorf("groth: settleTx must have exactly 2 inputs, got %d", len(settleTx.Inputs))
	}
	in1 := settleTx.Inputs[1]
	wantVerifierTxID := verifierTx.TxID().String()
	if in1.SourceTXID == nil || in1.SourceTXID.String() != wantVerifierTxID || in1.SourceTxOutIndex != verifierVout {
		return nil, errors.New("groth: settleTx input[1] does not spend committed verifier outpoint")
	}
	if int(verifierVout) >= len(verifierTx.Outputs) || verifierTx.Outputs[verifierVout].LockingScript == nil {
		return nil, errors.New("groth: verifierTx missing expected output")
	}
	stmt, vkCode, err := ExtractGrothDocPush(*verifierTx.Outputs[verifierVout].LockingScript)
	if err != nil {
		return nil, err
	}
	vkHash := sha256.Sum256(vkCode)
	vkHashHex := hex.EncodeToString(vkHash[:])
	if expectedVKScriptHashHex == "" || vkHashHex != expectedVKScriptHashHex {
		return nil, fmt.Errorf("groth: verifier VK script hash mismatch: got %s want %s", vkHashHex, expectedVKScriptHashHex)
	}
	if len(expectedJournalRoot) != 32 || !core.EqualBytes(stmt, expectedJournalRoot) {
		return nil, errors.New("groth: verifier doc-push statement root does not match journalRoot")
	}
	return &GrothSettlementResult{
		SettleTxID:       settleTx.TxID().String(),
		VerifierTxID:     wantVerifierTxID,
		VerifierVKHash:   vkHashHex,
		DocPushStatement: hex.EncodeToString(stmt),
		JournalRoot:      hex.EncodeToString(expectedJournalRoot),
		StructuralBound:  true,
		Verified:         false,
	}, nil
}

// VerifyGrothClaimSettlement is retained for compatibility and explicitly returns StructuralBound=true,
// Verified=false because it runs without a ChainTracker or script execution. For consensus-verified
// settlement (Verified=true), call VerifyGrothClaimSettlementSPV.
func VerifyGrothClaimSettlement(
	settleTx *transaction.Transaction,
	verifierTx *transaction.Transaction,
	verifierVout uint32,
	expectedVKScriptHashHex string,
	expectedJournalRoot []byte,
) (*GrothSettlementResult, error) {
	return VerifyGrothDocPushBinding(settleTx, verifierTx, verifierVout, expectedVKScriptHashHex, expectedJournalRoot)
}

// VerifyGrothClaimSettlementSPV performs full structural + consensus SPV verification of a
// NektarGrothClaim settlement: doc-push binding, witnessed header BUMP verification at >= minDepth,
// and go-sdk/spv.Verify consensus/script execution before setting Verified=true.
func VerifyGrothClaimSettlementSPV(
	ctx context.Context,
	settleTx *transaction.Transaction,
	verifierTx *transaction.Transaction,
	verifierVout uint32,
	expectedVKScriptHashHex string,
	expectedJournalRoot []byte,
	tracker chaintracker.ChainTracker,
	minDepth int,
) (*GrothSettlementResult, error) {
	if tracker == nil {
		return nil, errors.New("groth: witnessed ChainTracker is required for consensus SPV verification")
	}
	if minDepth <= 0 {
		minDepth = 1
	}
	res, err := VerifyGrothDocPushBinding(settleTx, verifierTx, verifierVout, expectedVKScriptHashHex, expectedJournalRoot)
	if err != nil {
		return nil, err
	}
	if settleTx.MerklePath == nil {
		return nil, errors.New("groth: settleTx lacks a mined MerklePath (BUMP)")
	}
	root, rErr := settleTx.MerklePath.ComputeRoot(settleTx.TxID())
	if rErr != nil || root == nil {
		return nil, fmt.Errorf("groth: compute settleTx merkle root: %v", rErr)
	}
	valid, vErr := tracker.IsValidRootForHeight(ctx, root, settleTx.MerklePath.BlockHeight)
	if vErr != nil {
		return nil, fmt.Errorf("groth: header tracker verify failed: %w", vErr)
	}
	if !valid {
		return nil, errors.New("groth: settleTx BUMP root invalid in header chain")
	}
	tip, tErr := tracker.CurrentHeight(ctx)
	if tErr != nil {
		return nil, fmt.Errorf("groth: header tip height failed: %w", tErr)
	}
	depth := 0
	if tip >= settleTx.MerklePath.BlockHeight {
		depth = int(tip-settleTx.MerklePath.BlockHeight) + 1
	}
	if depth < minDepth {
		return nil, fmt.Errorf("groth: settleTx confirmation depth %d < required minDepth %d", depth, minDepth)
	}
	// Verify settleTx (including script execution when unmined inputs exist) via go-sdk/spv.Verify.
	if ok, sErr := spv.Verify(ctx, settleTx, tracker, nil); sErr != nil || !ok {
		return nil, fmt.Errorf("groth: spv.Verify failed: %v", sErr)
	}
	res.BlockHeight = settleTx.MerklePath.BlockHeight
	res.Depth = depth
	res.Verified = true
	return res, nil
}
