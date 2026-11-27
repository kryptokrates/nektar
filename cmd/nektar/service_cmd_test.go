package main

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuildServiceConfigMainnetGateAndArgs(t *testing.T) {
	t.Setenv("NEKTAR_MAINNET_CONFIRM", "")
	t.Setenv("NEKTAR_BACKUP_DIR", "/var/backups/nektar")

	// 1. Mainnet without --confirm I_UNDERSTAND MUST fail closed!
	if _, err := buildServiceConfig(serviceInstallOpts{
		Network:    "main",
		Executable: "/usr/local/bin/nektar",
	}); err == nil {
		t.Fatal("expected mainnet service install without --confirm I_UNDERSTAND to fail closed")
	}

	// 2. Mainnet with --confirm I_UNDERSTAND produces valid arguments and env vars!
	cfg, err := buildServiceConfig(serviceInstallOpts{
		Name:       "nektar-main",
		Network:    "main",
		Confirm:    "I_UNDERSTAND",
		DataDir:    t.TempDir(),
		ListenAddr: "127.0.0.1:18420",
		WalletMode: "embedded",
		WithHub:    true,
		Executable: "/usr/local/bin/nektar",
	})
	if err != nil {
		t.Fatalf("buildServiceConfig mainnet confirmed: %v", err)
	}
	if cfg.Name != "nektar-main" || cfg.Executable != "/usr/local/bin/nektar" {
		t.Fatalf("unexpected service name/executable: %+v", cfg)
	}
	if cfg.EnvVars["NEKTAR_MAINNET_CONFIRM"] != "I_UNDERSTAND" {
		t.Fatalf("expected NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND in service EnvVars, got %+v", cfg.EnvVars)
	}
	if cfg.EnvVars["NEKTAR_BACKUP_DIR"] != "/var/backups/nektar" {
		t.Fatalf("expected NEKTAR_BACKUP_DIR passthrough, got %+v", cfg.EnvVars)
	}
	joined := strings.Join(cfg.Arguments, " ")
	for _, want := range []string{"service run", "--network main", "--listen 127.0.0.1:18420", "--wallet embedded", "--hub"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %q in service Arguments %q", want, joined)
		}
	}
}

func TestServiceProgramStartStopLifecycle(t *testing.T) {
	var started atomic.Bool
	var stopped atomic.Bool

	prog := &nektarServiceProgram{
		serveArgs: []string{"--network", "memory"},
		serveFn: func(ctx context.Context, args []string) error {
			started.Store(true)
			<-ctx.Done()
			stopped.Store(true)
			return nil
		},
	}

	if err := prog.Start(nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !started.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !started.Load() {
		t.Fatal("expected serveFn to start cleanly")
	}
	if err := prog.Stop(nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !stopped.Load() {
		t.Fatal("expected serveFn context to cancel and drain on Stop")
	}
}

func TestBuildServiceConfigExcludesS3SecretAndUsesEnvFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NEKTAR_BACKUP_S3_ACCESS_KEY", "AKIA_TEST_KEY")
	t.Setenv("NEKTAR_BACKUP_S3_SECRET_KEY", "super-secret-s3-value")

	cfg, err := buildServiceConfig(serviceInstallOpts{
		Network:    "testnet",
		DataDir:    dir,
		Executable: "/usr/local/bin/nektar",
	})
	if err != nil {
		t.Fatalf("buildServiceConfig: %v", err)
	}
	if _, leaked := cfg.EnvVars["NEKTAR_BACKUP_S3_SECRET_KEY"]; leaked {
		t.Fatalf("SECURITY VIOLATION: NEKTAR_BACKUP_S3_SECRET_KEY must NOT be embedded in OS service EnvVars")
	}
	envPath := cfg.EnvVars["NEKTAR_SERVICE_ENV_FILE"]
	if envPath == "" {
		t.Fatal("expected NEKTAR_SERVICE_ENV_FILE to be set when S3 secret is configured")
	}
	if err := writeServiceSecretsEnvFile(envPath); err != nil {
		t.Fatalf("writeServiceSecretsEnvFile: %v", err)
	}
	// Clear process env and verify loadServiceEnvFile restores the secret cleanly.
	t.Setenv("NEKTAR_BACKUP_S3_SECRET_KEY", "")
	if err := loadServiceEnvFile(envPath); err != nil {
		t.Fatalf("loadServiceEnvFile: %v", err)
	}
	if got := strings.TrimSpace(envOr("NEKTAR_BACKUP_S3_SECRET_KEY", "")); got != "super-secret-s3-value" {
		t.Fatalf("expected loaded S3 secret, got %q", got)
	}
}

func TestRestoreFileAtomicReplacesSQLiteAndClearsWAL(t *testing.T) {
	dir := t.TempDir()
	dst := dir + "/nektar.sqlite"
	if err := restoreFileAtomic(dst, []byte("old-db"), true); err != nil {
		t.Fatalf("initial restoreFileAtomic: %v", err)
	}
	if err := restoreFileAtomic(dst+"-wal", []byte("stale-wal"), false); err != nil {
		t.Fatalf("seed wal: %v", err)
	}
	if err := restoreFileAtomic(dst+"-shm", []byte("stale-shm"), false); err != nil {
		t.Fatalf("seed shm: %v", err)
	}
	if err := restoreFileAtomic(dst, []byte("new-db-bytes"), true); err != nil {
		t.Fatalf("overwrite restoreFileAtomic: %v", err)
	}
	if _, err := os.Stat(dst + "-wal"); !os.IsNotExist(err) {
		t.Fatalf("expected stale -wal to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(dst + "-shm"); !os.IsNotExist(err) {
		t.Fatalf("expected stale -shm to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(dst + ".restore.tmp"); !os.IsNotExist(err) {
		t.Fatalf("expected .restore.tmp to be renamed away, stat err=%v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "new-db-bytes" {
		t.Fatalf("expected restored bytes, got %q err=%v", string(got), err)
	}
}
