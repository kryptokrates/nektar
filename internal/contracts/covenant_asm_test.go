package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/keyed"
)

type fullArtifact struct {
	Version         string `json:"version"`
	CompilerVersion string `json:"compilerVersion"`
	ContractName    string `json:"contractName"`
	Script          string `json:"script"`
	ASM             string `json:"asm"`
}

func loadFullArtifact(t *testing.T, name string) fullArtifact {
	t.Helper()
	raw, err := artifactsFS.ReadFile("artifacts/" + name + ".json")
	if err != nil {
		t.Fatalf("read embedded artifact %s: %v", name, err)
	}
	var a fullArtifact
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("unmarshal artifact %s: %v", name, err)
	}
	return a
}

// TestCovenantASMInvariants enforces Wave 2 Bitcoin Script security invariants on every
// embedded production Rúnar artifact so compiler regressions (F-01) or dropped consensus
// checks (F-02, F-05, F-06, F-21) can never ship silently.
func TestCovenantASMInvariants(t *testing.T) {
	// 1. Parity between contracts/artifacts/*.json and internal/contracts/artifacts/*.json.
	for _, name := range ListArtifacts() {
		embRaw, err := artifactsFS.ReadFile("artifacts/" + name + ".json")
		if err != nil {
			t.Fatalf("missing embedded artifact %s: %v", name, err)
		}
		repoPath := filepath.Join("..", "..", "contracts", "artifacts", name+".json")
		repoRaw, err := os.ReadFile(repoPath)
		if err != nil {
			t.Fatalf("missing repo artifact %s: %v", repoPath, err)
		}
		h1 := sha256.Sum256(embRaw)
		h2 := sha256.Sum256(repoRaw)
		if h1 != h2 {
			t.Fatalf("artifact parity mismatch for %s: embedded=%x repo=%x", name, h1[:8], h2[:8])
		}
	}

	// 2. F-01: Zero OP_BIN2NUM ... OP_CAT miscompiled integer addition in NektarZkClaim.
	zk := loadFullArtifact(t, "NektarZkClaim")
	if strings.Contains(zk.ASM, "OP_BIN2NUM OP_8 OP_PICK OP_CAT") || strings.Contains(zk.Script, "8158797e") {
		t.Fatalf("F-01 REGRESSION: NektarZkClaim contains miscompiled OP_CAT deadline arithmetic!")
	}

	// 3. F-02: Every locktime-gated covenant MUST check nSequence < 0xffffffff (00ffffff... / feffffff / ffffffff00)
	// and the 500_000_000 (0065cd1d) locktime domain constant.
	const seqConstLE = "ffffffff00"  // 4294967295n in signed LE script number
	const lockDomainLE = "0065cd1d"  // 500000000n in signed LE script number
	for _, name := range []string{"NektarZkClaim", "DisclosureSale", "NektarCheckpointBond", "NektarFeeVault", "NektarAgentVault"} {
		a := loadFullArtifact(t, name)
		if !strings.Contains(strings.ToLower(a.Script), seqConstLE) {
			t.Errorf("F-02 REGRESSION: %s missing nSequence < 0xffffffff (%s) check in compiled Script", name, seqConstLE)
		}
		if !strings.Contains(strings.ToLower(a.Script), lockDomainLE) {
			t.Errorf("F-02 REGRESSION: %s missing 500_000_000 (%s) locktime domain check in compiled Script", name, lockDomainLE)
		}
	}

	// 4. F-05: DisclosureSale MUST embed the exact BRC-369 keyed.DomainCommitment hex prefix
	// ("metanet keyed content commitment v1") in its compiled Script.
	ds := loadFullArtifact(t, "DisclosureSale")
	wantDomainHex := hex.EncodeToString([]byte(keyed.DomainCommitment))
	if !strings.Contains(strings.ToLower(ds.Script), wantDomainHex) {
		t.Errorf("F-05 REGRESSION: DisclosureSale missing BRC-369 DomainCommitment (%s) in compiled Script", wantDomainHex)
	}

	// 5. F-21: ClaimVerdict.authorize MUST include OP_CHECKSIG so third parties cannot destroy it for 1 sat.
	cv := loadFullArtifact(t, "ClaimVerdict")
	if !strings.Contains(cv.ASM, "OP_CHECKSIG") {
		t.Errorf("F-21 REGRESSION: ClaimVerdict missing OP_CHECKSIG authorization")
	}
}

func TestVerifyGrothClaimSettlement(t *testing.T) {
	journalRoot := sha256.Sum256([]byte("certified-statement-root"))
	vkCode := []byte{0x51, 0x52, 0x93, 0x53, 0x87} // mock Groth16 verifier code
	vkHash := sha256.Sum256(vkCode)
	vkHashHex := hex.EncodeToString(vkHash[:])

	// Build verifier locking script: <32B journalRoot> OP_1 OP_2DROP <vkCode>
	verScriptBytes := make([]byte, 0, 1+32+1+1+len(vkCode))
	verScriptBytes = append(verScriptBytes, 32)
	verScriptBytes = append(verScriptBytes, journalRoot[:]...)
	verScriptBytes = append(verScriptBytes, 0x51, 0x6d) // OP_1 OP_2DROP
	verScriptBytes = append(verScriptBytes, vkCode...)
	verScript := script.Script(verScriptBytes)

	verifierTx := transaction.NewTransaction()
	verifierTx.AddOutput(&transaction.TransactionOutput{
		Satoshis:      1000,
		LockingScript: &verScript,
	})

	settleTx := transaction.NewTransaction()
	dummyTxID := &chainhash.Hash{1, 2, 3}
	settleTx.AddInput(&transaction.TransactionInput{SourceTXID: dummyTxID, SourceTxOutIndex: 0})
	settleTx.AddInput(&transaction.TransactionInput{SourceTXID: verifierTx.TxID(), SourceTxOutIndex: 0})

	res, err := VerifyGrothClaimSettlement(settleTx, verifierTx, 0, vkHashHex, journalRoot[:])
	if err != nil || !res.StructuralBound || res.Verified {
		t.Fatalf("VerifyGrothClaimSettlement structural binding failed (must not set Verified=true without ChainTracker): res=%+v err=%v", res, err)
	}
	// Wrong VK hash must fail closed (blocks dummy OP_TRUE verifier).
	if _, err := VerifyGrothClaimSettlement(settleTx, verifierTx, 0, strings.Repeat("aa", 32), journalRoot[:]); err == nil {
		t.Fatal("expected VK script hash mismatch rejection")
	}
	// Wrong journalRoot must fail closed (blocks statement substitution).
	badRoot := sha256.Sum256([]byte("evil-statement"))
	if _, err := VerifyGrothClaimSettlement(settleTx, verifierTx, 0, vkHashHex, badRoot[:]); err == nil {
		t.Fatal("expected journalRoot mismatch rejection")
	}
}
