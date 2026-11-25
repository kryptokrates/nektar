package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/kryptokrates/nektar/internal/core"
)

// GrothSettlementResult summarizes verified consensus + statement bindings of a NektarGrothClaim settlement tx.
type GrothSettlementResult struct {
	SettleTxID        string `json:"settleTxid"`
	VerifierTxID      string `json:"verifierTxid"`
	VerifierVKHash    string `json:"verifierVkHash"`
	DocPushStatement  string `json:"docPushStatement"`
	JournalRoot       string `json:"journalRoot"`
	Verified          bool   `json:"verified"`
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

// VerifyGrothClaimSettlement performs the mandatory off-chain F-18 cross-check for NektarGrothClaim:
//  1. Verifies settleTx has exactly 2 inputs and input[1] spends verifierOutpoint (verifierTx:verifierVout).
//  2. Parses verifierTx's locking script doc-push prefix `<statement-root> <index> OP_2DROP <vkCode>`.
//  3. Verifies sha256(vkCode) == expectedVKScriptHashHex (rejecting dummy OP_TRUE or dev setups).
//  4. Verifies statement-root == expectedJournalRoot (binding the verified proof to ClaimVerdict.journalRoot).
func VerifyGrothClaimSettlement(
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
		Verified:         true,
	}, nil
}
