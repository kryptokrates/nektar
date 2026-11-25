package main

import (
	"os"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/chain"
)

func TestCmdCheckMainnetEnforcesNonCircularHeaderIndependence(t *testing.T) {
	t.Setenv("NEKTAR_MAINNET_CONFIRM", "I_UNDERSTAND")
	t.Setenv("NEKTAR_SETTLE_SPEC", "arcade:https://arcade.gorillapool.io")

	// 1. Single circular woc:main MUST fail (#1, #2).
	t.Setenv("NEKTAR_HEADERS_SOURCE", "woc:main")
	t.Setenv("NEKTAR_CHAIN_SPEC", "woc:main")
	if err := cmdCheckMainnet(); err == nil {
		t.Fatal("expected cmdCheckMainnet to reject single circular woc:main header source")
	}

	// 2. Native zero-PoW header source MUST fail (#1).
	t.Setenv("NEKTAR_HEADERS_SOURCE", "native:http://127.0.0.1:8080,woc:main")
	if err := cmdCheckMainnet(); err == nil {
		t.Fatal("expected cmdCheckMainnet to reject Native header source on mainnet")
	}

	// 3. Independent >=2 PoW witnesses MUST pass (#1, #2).
	t.Setenv("NEKTAR_HEADERS_SOURCE", chain.DefaultHeaderSources("main"))
	if err := cmdCheckMainnet(); err != nil {
		t.Fatalf("expected cmdCheckMainnet to accept independent witnesses: %v", err)
	}
}

func TestHookVerifyReceiptFailsClosedOnVerificationError(t *testing.T) {
	// Feed a nektarr1.* token with an unwritable/nonexistent data-dir and --codex so errors must fail closed (#38).
	tmpDir := t.TempDir()
	// Make data-dir point to a regular file so openRuntime fails deterministically!
	blockingFile := tmpDir + "/not-a-dir"
	if err := os.WriteFile(blockingFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeTok := "nektarr1." + strings.Repeat("a", 40)
	err := cmdHook([]string{"verify-receipt", "--codex", "-data-dir", blockingFile, fakeTok})
	if err == nil {
		t.Fatal("expected nektar hook verify-receipt to fail closed when runtime/verification fails (not exit 0)")
	}
}
