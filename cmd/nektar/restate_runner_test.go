package main

import (
	"context"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestStartRestatePlaneSingleContainerDefaultAndFailClosedGuard(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	eng := market.NewEngine(mem, market.DefaultPolicy())

	// 1. Inside a single-container deployment (NEKTAR_CONTAINER=1) with empty ListenAddr,
	// StartRestatePlane must default to 127.0.0.1:9080 (never crash on R1 non-loopback guard!)
	// and auto-generate a non-empty InternalToken.
	t.Setenv("NEKTAR_CONTAINER", "1")
	_, cleanup, err := StartRestatePlane(ctx, eng, RestateRuntimeConfig{
		ListenAddr:       "",
		DataDir:          t.TempDir(),
		DisableAutoStart: true,
	})
	if err != nil {
		t.Fatalf("single-container default StartRestatePlane failed unexpectedly: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}

	// 2. Explicitly binding non-loopback (:9080) without NEKTAR_RESTATE_IDENTITY_KEY or
	// AllowInsecureEndpoint MUST fail closed!
	if _, _, err := StartRestatePlane(ctx, eng, RestateRuntimeConfig{
		ListenAddr:       ":9080",
		DataDir:          t.TempDir(),
		DisableAutoStart: true,
	}); err == nil {
		t.Fatal("expected non-loopback :9080 without identity key to fail closed")
	}

	// 3. Pinned Restate SHA-256 table must cover all 4 supported release targets with valid 64-hex digests.
	expectedTargets := []string{
		"x86_64-unknown-linux-musl",
		"aarch64-unknown-linux-musl",
		"aarch64-apple-darwin",
		"x86_64-apple-darwin",
	}
	for _, target := range expectedTargets {
		sum := restatePinnedSHA256[target]
		if len(sum) != 64 {
			t.Errorf("restatePinnedSHA256[%q] missing or invalid length %d", target, len(sum))
		}
	}
}
