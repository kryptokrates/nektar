package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lightwebinc/bcommon/keyed"
)

func loadFullArtifactForTest(t *testing.T, name string) *FullArtifact {
	t.Helper()
	a, err := LoadFullArtifact(name)
	if err != nil {
		t.Fatalf("load embedded artifact %s: %v", name, err)
	}
	return a
}

// TestCovenantASMInvariants enforces Bitcoin Script security invariants on every
// embedded production Rúnar artifact so dropped consensus checks (F-02, F-05) can never ship silently.
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

	// 2. F-02: Every locktime-gated covenant MUST check nSequence < 0xffffffff
	// and the 500_000_000 (0065cd1d) locktime domain constant.
	const seqConstLE = "ffffffff00" // 4294967295n in signed LE script number
	const lockDomainLE = "0065cd1d" // 500000000n in signed LE script number
	for _, name := range []string{"DisclosureSale", "NektarCheckpointBond", "NektarAgentVault"} {
		a := loadFullArtifactForTest(t, name)
		if !strings.Contains(strings.ToLower(a.Script), seqConstLE) {
			t.Errorf("F-02 REGRESSION: %s missing nSequence < 0xffffffff (%s) check in compiled Script", name, seqConstLE)
		}
		if !strings.Contains(strings.ToLower(a.Script), lockDomainLE) {
			t.Errorf("F-02 REGRESSION: %s missing 500_000_000 (%s) locktime domain check in compiled Script", name, lockDomainLE)
		}
	}

	// 3. F-05: DisclosureSale MUST embed the exact BRC-369 keyed.DomainCommitment hex prefix
	// ("metanet keyed content commitment v1") in its compiled Script.
	ds := loadFullArtifactForTest(t, "DisclosureSale")
	wantDomainHex := hex.EncodeToString([]byte(keyed.DomainCommitment))
	if !strings.Contains(strings.ToLower(ds.Script), wantDomainHex) {
		t.Errorf("F-05 REGRESSION: DisclosureSale missing BRC-369 DomainCommitment (%s) in compiled Script", wantDomainHex)
	}
}
