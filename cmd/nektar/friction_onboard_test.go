package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquireInstanceLockIncludesPIDAndRecoveryBumper(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireInstanceLock(dir, "127.0.0.1:28420")
	if err != nil {
		t.Fatalf("first acquireInstanceLock failed: %v", err)
	}
	defer release()

	_, err2 := acquireInstanceLock(dir, "127.0.0.1:28421")
	if err2 == nil {
		t.Fatal("expected second acquireInstanceLock to fail while held")
	}
	msg := err2.Error()
	for _, want := range []string{"pid=", "127.0.0.1:28420", "nektar mcp", "nektar open", "nektar stop"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("lock collision error missing %q: %s", want, msg)
		}
	}
}

func TestResolveRepoDataDirHonorsNetworkFlagPostParse(t *testing.T) {
	t.Setenv("NEKTAR_DATA_DIR", "")
	t.Setenv("NEKTAR_NETWORK", "testnet")

	mainDir := resolveRepoDataDir("", "main")
	testDir := resolveRepoDataDir("", "testnet")
	if !strings.HasSuffix(filepath.ToSlash(mainDir), "/.nektar/main") {
		t.Fatalf("expected mainnet repo dir under /.nektar/main, got %q", mainDir)
	}
	if strings.HasSuffix(filepath.ToSlash(testDir), "/.nektar/main") {
		t.Fatalf("expected testnet repo dir outside /main, got %q", testDir)
	}
	custom := filepath.Join(t.TempDir(), "custom")
	if got := resolveRepoDataDir(custom, "main"); got != custom {
		t.Fatalf("expected explicit data-dir %q to win, got %q", custom, got)
	}
}

func TestKillActiveHonorsMainnetParentKillSwitch(t *testing.T) {
	t.Setenv("NEKTAR_KILL", "")
	fakeHome := t.TempDir()
	base := filepath.Join(fakeHome, ".nektar")
	mainDir := filepath.Join(base, "main")
	if err := os.MkdirAll(mainDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if killActive(mainDir) {
		t.Fatal("expected killActive false before sentinel exists")
	}
	// Parent ~/.nektar/KILL must act as global emergency stop for ~/.nektar/main.
	if err := os.WriteFile(filepath.Join(base, "KILL"), []byte("stop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !killActive(mainDir) {
		t.Fatal("expected ~/.nektar/KILL to activate kill switch for ~/.nektar/main")
	}
	// Unrelated isolated temp dir must NOT be affected by another directory's KILL file.
	other := t.TempDir()
	if killActive(other) {
		t.Fatal("unrelated dataDir must not inherit another directory's KILL sentinel")
	}
}

func TestCmdOpenRefusesDeadPortWhenDaemonStopped(t *testing.T) {
	t.Setenv("NEKTAR_ISOLATED_DAEMON", "1")
	dir := t.TempDir()
	err := cmdOpen([]string{"-data-dir", dir})
	if err == nil {
		t.Fatal("expected cmdOpen to return actionable error when no daemon is running")
	}
	if !strings.Contains(err.Error(), "nektar up") {
		t.Fatalf("expected 'nektar up' recovery bumper in cmdOpen error, got: %v", err)
	}
}

func TestCmdDoctorAndStatusDoNotCreateDBOnUninitializedDir(t *testing.T) {
	t.Setenv("NEKTAR_ISOLATED_DAEMON", "1")
	dir := filepath.Join(t.TempDir(), "fresh")
	if err := cmdDoctor([]string{"-data-dir", dir, "-json"}); err != nil {
		t.Fatalf("cmdDoctor uninitialized failed: %v", err)
	}
	if err := cmdStatus([]string{"-data-dir", dir, "-json"}); err != nil {
		t.Fatalf("cmdStatus uninitialized failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nektar.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("expected cmdDoctor/cmdStatus not to silently create nektar.sqlite in %s", dir)
	}
}

func TestFormatHookMemoryBulletsCompactFormat(t *testing.T) {
	out := formatHookMemoryBullets("nektar prefetch", map[string]any{
		"items": []map[string]any{
			{"shard": "welcome", "seq": 1, "text": "Nektar stores verifiable memory."},
			{"shard": "ops", "seq": 4, "text": "API runs on port 8080."},
		},
	})
	if !strings.Contains(out, "- [welcome:1] Nektar stores verifiable memory.") {
		t.Fatalf("missing formatted welcome bullet: %q", out)
	}
	if !strings.Contains(out, "- [ops:4] API runs on port 8080.") {
		t.Fatalf("missing formatted ops bullet: %q", out)
	}
	if strings.Contains(out, `{"items"`) {
		t.Fatalf("hook output must not dump raw JSON blob: %q", out)
	}
}

// TestOnboardingInstallAndDocsFrictionRatchets guards against regressions on F-01, F-02, F-07, and F-12.
func TestOnboardingInstallAndDocsFrictionRatchets(t *testing.T) {
	root := filepath.Join("..", "..")

	// F-01: installers must NEVER auto-default I_UNDERSTAND when --mainnet is passed without --confirm.
	for _, rel := range []string{"install/install.sh", "install/install.ps1"} {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		text := string(raw)
		if strings.Contains(text, "${MAINNET_CONFIRM:-I_UNDERSTAND}") || strings.Contains(text, "else { 'I_UNDERSTAND' }") {
			t.Fatalf("%s bypasses NEKTAR_MAINNET_CONFIRM by auto-defaulting I_UNDERSTAND", rel)
		}
	}

	// F-02: Compose/Helm/agent .env must NEVER hardcode woc:test headers source (crashes Mainnet deploys).
	for _, rel := range []string{
		"deploy/hub/compose.yaml",
		"deploy/agent/compose.yaml",
		"deploy/hub/helm/values.yaml",
		"deploy/agent/helm/values.yaml",
		"deploy/agent/.env.example",
	} {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		text := string(raw)
		if strings.Contains(text, "NEKTAR_HEADERS_SOURCE:-woc:test") ||
			strings.Contains(text, "NEKTAR_HEADERS_SOURCE=woc:test") ||
			strings.Contains(text, "headersSource: 'woc:test'") {
			t.Fatalf("%s hardcodes woc:test for NEKTAR_HEADERS_SOURCE (breaks mainnet)", rel)
		}
	}

	// F-12: Pure-Go v0.1 docs must not regress back to deleted TypeScript/pnpm stack references.
	staleMarkers := []string{
		"pnpm -r ",
		"pnpm test:",
		"pnpm stack:",
		"packages/mcp/dist",
		":8080/mcp",
		"NEKTAR_KEY_PASSPHRASE",
		"automationDoctor",
		"deploy/bstack-sandbox",
		"./nektar --port",
	}
	for _, rel := range []string{
		"docs/JOIN-AGENT.md",
		"docs/JOIN-MAINNET.md",
		"docs/ENVIRONMENT.md",
		"docs/OPERATIONS.md",
		"deploy/agent/README.md",
	} {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		text := string(raw)
		for _, bad := range staleMarkers {
			if strings.Contains(text, bad) {
				t.Fatalf("%s contains stale legacy TS marker %q", rel, bad)
			}
		}
	}

	// F-12: MAINNET-CANARY.md must point kill switch to ~/.nektar/main/KILL.
	canary, err := os.ReadFile(filepath.Join(root, "docs/MAINNET-CANARY.md"))
	if err != nil {
		t.Fatalf("read docs/MAINNET-CANARY.md: %v", err)
	}
	if !strings.Contains(string(canary), "~/.nektar/main/KILL") {
		t.Fatal("docs/MAINNET-CANARY.md must document ~/.nektar/main/KILL for mainnet kill-switch drill")
	}

	// F-13: Zero-sidecar ratchet — web, installers, AGENTS.md, SKILL.md, README.md, and docs must NEVER
	// tell users to install or supervise a deleted external `restate-server` sidecar or WSL2 for Restate.
	deadSidecarMarkers := []string{
		"restate-server",
		"restate-up",
		"NEKTAR_RESTATE_",
		"WSL2 for Restate",
		"Restate (WSL2)",
		"restate:1.8.1",
	}
	zeroSidecarDocs := []string{
		"README.md",
		"AGENTS.md",
		"skills/nektar-memory/SKILL.md",
		"install/install.sh",
		"install/install.ps1",
		"site/index.html",
		"site/llm.txt",
		"site/llms.txt",
		"site/docs/index.html",
		"site/docs/buy-and-sell.html",
		"site/docs/run-an-agent.html",
		"site/docs/run-a-hub.html",
		"docs/DOCKING-AND-HUBS.md",
		"docs/MANUAL-CLAUDE-CODE.md",
		"docs/MANUAL-CODEX.md",
		"docs/JOIN-AGENT.md",
		"docs/JOIN-MAINNET.md",
		"docs/MAINNET-CANARY.md",
		"docs/OPERATIONS.md",
		"docs/ENVIRONMENT.md",
		"docs/ADAPTERS.md",
		"docs/INTEGRATION.md",
	}
	for _, rel := range zeroSidecarDocs {
		raw, rErr := os.ReadFile(filepath.Join(root, rel))
		if rErr != nil {
			t.Fatalf("read %s: %v", rel, rErr)
		}
		text := string(raw)
		for _, bad := range deadSidecarMarkers {
			if strings.Contains(text, bad) {
				t.Fatalf("%s still contains stale Restate/WSL2 sidecar marker %q", rel, bad)
			}
		}
	}

	// F-14: Dual human + agent onboarding ratchet on site/index.html & site/llm.txt.
	indexHTML, err := os.ReadFile(filepath.Join(root, "site/index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(indexHTML), "Paste to Agent") || !strings.Contains(string(indexHTML), "https://nektar.run/llm.txt") {
		t.Fatal("site/index.html must expose the 1-click 'Paste to Agent' setup prompt pointing to llm.txt")
	}
	llmTxt, err := os.ReadFile(filepath.Join(root, "site/llm.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"buyCapsule", "sellCapsule", "importCapsule", "nektar hub up", "nektar peers join"} {
		if !strings.Contains(string(llmTxt), want) {
			t.Fatalf("site/llm.txt missing zero-friction onboarding verb %q", want)
		}
	}
}

func TestHubUpDefaultsToZeroDockerNativeMode(t *testing.T) {
	hubDir := t.TempDir()
	if err := cmdHubUp([]string{"--dir", hubDir, "--network", "testnet", "--dry"}); err != nil {
		t.Fatalf("cmdHubUp --dry failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hubDir, "nektar-hub.service")); err != nil {
		t.Fatalf("expected zero-Docker nektar-hub.service in %s: %v", hubDir, err)
	}
	if _, err := os.Stat(filepath.Join(hubDir, "compose.yaml")); !os.IsNotExist(err) {
		t.Fatalf("expected zero-Docker default NOT to write compose.yaml unless --docker is passed")
	}
	envRaw, err := os.ReadFile(filepath.Join(hubDir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envRaw), "NEKTAR_UPSTREAM_SPINES=https://testnet.nektar.run/hub") {
		t.Fatalf("expected automatic upstream Spine peering in .env, got:\n%s", string(envRaw))
	}
}
