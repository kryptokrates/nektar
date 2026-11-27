package lattice

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/core"
)

func testRootProvider(seed string) RootProvider {
	return func(_ context.Context, shard string, epoch uint64) ([32]byte, error) {
		var out [32]byte
		h := core.DSHA256(core.Concat([]byte(seed), core.Field([]byte(shard)), core.U64LE(epoch)))
		copy(out[:], h)
		return out, nil
	}
}

// TestCLKANoUpwardOrLateralTraversal proves mathematically and empirically that:
//  1. A principal holding Clearance(MaxLevel=3, Domain="compliance") can decrypt Levels 0..3
//     in "compliance" AND dominated child domain "general" via the public Manifest edge token.
//  2. That SAME principal CANNOT decrypt Level 4 in "compliance" (zero upward traversal — even if
//     it bypasses checks and tries to use its raw LevelKey directly!).
//  3. That SAME principal CANNOT decrypt Level 0 in sibling domain "legal" or parent domain "executive".
//  4. Rotating epoch achieves full forward secrecy against old Clearance bundles.
//  5. ECDH-sealed Clearance provisioning round-trips cleanly and rejects wrong recipient keys.
func TestCLKANoUpwardOrLateralTraversal(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(testRootProvider("wallet-master-seed"))

	// Domain privilege DAG: executive >= compliance >= general, executive >= legal.
	if err := mgr.SetDomainHierarchy("vault", []DomainEdge{
		{Parent: "executive", Child: "compliance"},
		{Parent: "compliance", Child: "general"},
		{Parent: "executive", Child: "legal"},
	}); err != nil {
		t.Fatalf("SetDomainHierarchy: %v", err)
	}

	// Cycle MUST be rejected!
	if err := mgr.SetDomainHierarchy("vault", []DomainEdge{
		{Parent: "a", Child: "b"},
		{Parent: "b", Child: "a"},
	}); !errors.Is(err, ErrDomainCycle) {
		t.Fatalf("expected ErrDomainCycle, got %v", err)
	}

	// Seal fragments across levels 0, 3, 4 and domains general, compliance, legal, executive.
	seal := func(level int, domain, msg string) *SealedEnvelope {
		var cid [32]byte
		copy(cid[:], core.SHA256([]byte(msg)))
		env, k, err := mgr.SealFragment(ctx, "vault", level, domain, cid, []byte(msg))
		if err != nil {
			t.Fatalf("SealFragment(%d,%s): %v", level, domain, err)
		}
		core.Zero32Array(&k)
		return env
	}
	envL0General := seal(0, "general", "level-0-general-secret")
	envL3Compliance := seal(3, "compliance", "level-3-compliance-secret")
	envL4Compliance := seal(4, "compliance", "level-4-top-secret")
	envL0Legal := seal(0, "legal", "level-0-legal-secret")
	envL0Executive := seal(0, "executive", "level-0-executive-secret")

	// Provision clearance for (MaxLevel=3, Domain="compliance").
	clr, manifest, err := mgr.ProvisionClearance(ctx, "vault", 3, []string{"compliance"})
	if err != nil {
		t.Fatalf("ProvisionClearance: %v", err)
	}

	// 1. Downward level (3 -> 0) AND downward domain (compliance -> general) MUST succeed offline!
	pt, err := OpenWithClearance(envL0General, clr, manifest)
	if err != nil || string(pt) != "level-0-general-secret" {
		t.Fatalf("expected downward decrypt of L0/general: pt=%q err=%v", pt, err)
	}
	pt3, err := OpenWithClearance(envL3Compliance, clr, manifest)
	if err != nil || string(pt3) != "level-3-compliance-secret" {
		t.Fatalf("expected exact-tier decrypt of L3/compliance: pt=%q err=%v", pt3, err)
	}

	// 2. Upward level (3 -> 4) MUST fail both at policy boundary AND cryptographically!
	if _, err := OpenWithClearance(envL4Compliance, clr, manifest); !errors.Is(err, ErrUpwardTraversal) {
		t.Fatalf("expected ErrUpwardTraversal on L4, got %v", err)
	}
	// Adversarial bypass: even if attacker lies about clr.MaxLevel=4 while holding K_3,
	// AES/BRC-369 unwrap + KeyCommitment check MUST fail cryptographically!
	forgedClr := *clr
	forgedClr.MaxLevel = 4
	if _, err := OpenWithClearance(envL4Compliance, &forgedClr, manifest); err == nil {
		t.Fatal("CRITICAL: forged MaxLevel=4 with K_3 decrypted Level 4 fragment!")
	}

	// 3. Lateral sibling ("legal") and upward parent ("executive") MUST fail cryptographically!
	if _, err := OpenWithClearance(envL0Legal, clr, manifest); !errors.Is(err, ErrDomainAccess) {
		t.Fatalf("expected ErrDomainAccess for sibling domain legal, got %v", err)
	}
	if _, err := OpenWithClearance(envL0Executive, clr, manifest); !errors.Is(err, ErrDomainAccess) {
		t.Fatalf("expected ErrDomainAccess for parent domain executive, got %v", err)
	}

	// Executive clearance (MaxLevel=10, Domain="executive") reaches BOTH legal AND general transitively!
	execClr, execMan, err := mgr.ProvisionClearance(ctx, "vault", 10, []string{"executive"})
	if err != nil {
		t.Fatalf("ProvisionClearance(executive): %v", err)
	}
	for name, env := range map[string]*SealedEnvelope{
		"L0/general":    envL0General,
		"L3/compliance": envL3Compliance,
		"L4/compliance": envL4Compliance,
		"L0/legal":      envL0Legal,
		"L0/executive":  envL0Executive,
	} {
		if _, err := OpenWithClearance(env, execClr, execMan); err != nil {
			t.Fatalf("executive clearance failed to open %s: %v", name, err)
		}
	}

	// 4. Epoch rotation achieves forward secrecy: old clearance CANNOT open epoch 2 fragments!
	ep2, err := mgr.RotateEpoch("vault")
	if err != nil || ep2 != 2 {
		t.Fatalf("RotateEpoch: ep=%d err=%v", ep2, err)
	}
	envEp2 := seal(0, "general", "post-rotation-secret")
	if _, err := OpenWithClearance(envEp2, clr, manifest); !errors.Is(err, ErrEpochMismatch) {
		t.Fatalf("expected ErrEpochMismatch after rotation, got %v", err)
	}
	// Even forging Epoch=2 on old K_3 MUST fail cryptographically!
	forgedEp := *clr
	forgedEp.Epoch = 2
	manEp2, _ := mgr.BuildManifest(ctx, "vault", 2)
	if _, err := OpenWithClearance(envEp2, &forgedEp, manEp2); err == nil {
		t.Fatal("CRITICAL: epoch-1 key decrypted epoch-2 fragment!")
	}

	// 5. ECDH-sealed clearance provisioning to agent keypair!
	agentKey, _ := ec.NewPrivateKey()
	otherKey, _ := ec.NewPrivateKey()
	sealedClr, err := SealClearanceForRecipient(hex.EncodeToString(agentKey.PubKey().Compressed()), clr)
	if err != nil {
		t.Fatalf("SealClearanceForRecipient: %v", err)
	}
	openedClr, err := OpenSealedClearance(agentKey, "vault", 1, sealedClr)
	if err != nil || openedClr.LevelKeyHex != clr.LevelKeyHex {
		t.Fatalf("OpenSealedClearance mismatch: %+v err=%v", openedClr, err)
	}
	if _, err := OpenSealedClearance(otherKey, "vault", 1, sealedClr); err == nil {
		t.Fatal("wrong recipient key opened ECDH-sealed clearance!")
	}
	ptCheck, err := OpenWithClearance(envL0General, openedClr, manifest)
	if err != nil || !bytes.Equal(ptCheck, []byte("level-0-general-secret")) {
		t.Fatalf("opened clearance decrypt failed: %q err=%v", ptCheck, err)
	}
}
