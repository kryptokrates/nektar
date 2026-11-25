package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestBuildRestateEndpointURI(t *testing.T) {
	cases := []struct {
		listen   string
		override string
		want     string
	}{
		{":9080", "", "http://127.0.0.1:9080"},
		{"127.0.0.1:9080", "", "http://127.0.0.1:9080"},
		{"0.0.0.0:9085", "", "http://127.0.0.1:9085"},
		{":9080", "http://agent:9080/", "http://agent:9080"},
	}
	for _, tc := range cases {
		got, err := buildRestateEndpointURI(tc.listen, tc.override)
		if err != nil {
			t.Fatalf("buildRestateEndpointURI(%q, %q) err: %v", tc.listen, tc.override, err)
		}
		if got != tc.want {
			t.Fatalf("buildRestateEndpointURI(%q, %q) = %q, want %q", tc.listen, tc.override, got, tc.want)
		}
	}
}

func TestStartRestatePlaneFailClosedNonLoopbackWithoutIdentityKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	eng := market.NewEngine(mem, market.DefaultPolicy())

	// R1 fail-closed: non-loopback :9080 without IdentityPublicKey or AllowInsecureEndpoint must error!
	_, stop, err := StartRestatePlane(ctx, eng, RestateRuntimeConfig{
		ListenAddr:            ":9080",
		DataDir:               t.TempDir(),
		IdentityPublicKey:     "",
		AllowInsecureEndpoint: false,
	})
	if stop != nil {
		stop()
	}
	if err == nil || !strings.Contains(err.Error(), "NEKTAR_RESTATE_IDENTITY_KEY") {
		t.Fatalf("expected fail-closed error for non-loopback Restate endpoint without identity key, got %v", err)
	}
}
