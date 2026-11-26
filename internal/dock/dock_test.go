package dock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedAdaptersPresent(t *testing.T) {
	skill := SkillMarkdown()
	if len(skill) == 0 || !strings.Contains(string(skill), "nektar-memory") {
		t.Fatalf("expected embedded SKILL.md, got %q", string(skill))
	}
	hermes := BundleFiles("hermes")
	if len(hermes) < 5 {
		t.Fatalf("expected >=5 embedded hermes files, got %d", len(hermes))
	}
	hasClient := false
	for _, f := range hermes {
		if f.Path == "hermes_memory_nektar/client.py" {
			hasClient = true
			if !strings.Contains(f.Content, "active-daemon.json") {
				t.Fatalf("expected embedded hermes client.py to include active-daemon.json discovery")
			}
		}
	}
	if !hasClient {
		t.Fatal("missing hermes_memory_nektar/client.py in embedded bundle")
	}

	oc := BundleFiles("openclaw")
	if len(oc) < 3 {
		t.Fatalf("expected >=3 embedded openclaw files, got %d", len(oc))
	}
	hasIndex := false
	for _, f := range oc {
		if f.Path == "index.mjs" {
			hasIndex = true
			if !strings.Contains(f.Content, "active-daemon.json") {
				t.Fatalf("expected embedded openclaw index.mjs to include active-daemon.json discovery")
			}
		}
	}
	if !hasIndex {
		t.Fatal("missing index.mjs in embedded openclaw bundle")
	}
}

func TestDockAllFlagshipClientsInstallIdempotentAndUninstall(t *testing.T) {
	home := t.TempDir()
	// Pre-populate realistic existing configs for Claude Code, Codex, Hermes, OpenClaw, and Cursor.
	mustMkdir := func(rel ...string) {
		if err := os.MkdirAll(filepath.Join(append([]string{home}, rel...)...), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir(".claude")
	mustMkdir(".codex")
	mustMkdir(".hermes")
	mustMkdir(".openclaw")
	mustMkdir(".cursor")

	_ = os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"sibling":{"command":"other"}}}`), 0o600)
	_ = os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("model = \"o4-mini\"\n\n[mcp_servers.other]\ncommand = \"foo\"\n"), 0o600)
	_ = os.WriteFile(filepath.Join(home, ".hermes", "config.yaml"), []byte("model:\n  name: claude\n"), 0o600)
	_ = os.WriteFile(filepath.Join(home, ".openclaw", "openclaw.json"), []byte(`{"plugins":{"allow":["existing"],"entries":{"existing":{"enabled":true}}}}`), 0o600)
	_ = os.WriteFile(filepath.Join(home, ".cursor", "mcp.json"), []byte(`{"mcpServers":{"existing":{"command":"bar"}}}`), 0o600)

	// Simulate Windows-style path with backslashes to verify JSON + TOML escaping!
	fakeExe := `C:\Users\alice\AppData\Local\nektar\nektar.exe`
	opts := Options{
		Home:    home,
		ExePath: fakeExe,
		LiveURL: "http://127.0.0.1:18420",
		Token:   "tok-123",
	}

	// 1. DetectAll before docking: all 5 detected, none docked, all OneClick=true!
	before := DetectAll(home)
	byID := make(map[string]ClientStatus)
	for _, c := range before {
		byID[c.ID] = c
		if !c.OneClick {
			t.Fatalf("expected client %s to support OneClick=true", c.ID)
		}
	}
	for _, id := range []string{"claude", "codex", "hermes", "openclaw", "cursor"} {
		if !byID[id].Detected || byID[id].Docked {
			t.Fatalf("before dock: client %s expected detected=true docked=false, got %+v", id, byID[id])
		}
	}

	// 2. Auto-dock all detected clients!
	res := Apply(opts, []string{"auto"})
	if len(res) < 5 {
		t.Fatalf("expected >=5 docked clients, got %d (%+v)", len(res), res)
	}
	for _, r := range res {
		if r.Error != "" {
			t.Fatalf("unexpected error docking %s: %s", r.Client, r.Error)
		}
		if !r.Changed {
			t.Fatalf("expected first dock of %s to report Changed=true", r.Client)
		}
	}

	// Verify Claude Code L3: ~/.claude.json + ~/.claude/settings.json + SKILL.md
	claudeHooksRaw, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("missing claude settings.json: %v", err)
	}
	var hookDoc map[string]any
	if err := json.Unmarshal(claudeHooksRaw, &hookDoc); err != nil {
		t.Fatalf("invalid claude settings.json: %v", err)
	}
	if !strings.Contains(string(claudeHooksRaw), "hook verify-receipt") || !strings.Contains(string(claudeHooksRaw), "hook prefetch") {
		t.Fatalf("missing expected Claude hooks: %s", string(claudeHooksRaw))
	}
	if !fileExists(filepath.Join(home, ".claude", "skills", "nektar-memory", "SKILL.md")) {
		t.Fatal("missing Claude SKILL.md")
	}

	// Verify Codex CLI L2: top-level `notify` appears BEFORE [mcp_servers.*] AND backslashes are TOML-escaped!
	codexRaw, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("missing codex config.toml: %v", err)
	}
	codexStr := string(codexRaw)
	notifyIdx := strings.Index(codexStr, "notify = ")
	firstTableIdx := strings.Index(codexStr, "[mcp_servers.")
	if notifyIdx < 0 || firstTableIdx < 0 || notifyIdx > firstTableIdx {
		t.Fatalf("TOML error: top-level notify must appear BEFORE first [table]:\n%s", codexStr)
	}
	if !strings.Contains(codexStr, `C:\\Users\\alice\\AppData\\Local\\nektar\\nektar.exe`) {
		t.Fatalf("expected properly TOML-escaped Windows path in config.toml:\n%s", codexStr)
	}
	if !fileExists(filepath.Join(home, ".codex", "skills", "nektar-memory", "SKILL.md")) ||
		!fileExists(filepath.Join(home, ".agents", "skills", "nektar-memory", "SKILL.md")) {
		t.Fatal("missing Codex/universal SKILL.md")
	}

	// Verify Hermes L3: embedded plugin + SKILL.md + config.yaml memory.provider: nektar + sibling preserved + backup!
	if !fileExists(filepath.Join(home, ".hermes", "plugins", "hermes-memory-nektar", "plugin.yaml")) {
		t.Fatal("missing Hermes plugin.yaml")
	}
	if !fileExists(filepath.Join(home, ".hermes", "skills", "nektar-memory", "SKILL.md")) {
		t.Fatal("missing Hermes SKILL.md")
	}
	hermesCfg, _ := os.ReadFile(filepath.Join(home, ".hermes", "config.yaml"))
	if !strings.Contains(string(hermesCfg), "provider: nektar") || !strings.Contains(string(hermesCfg), "name: claude") {
		t.Fatalf("unexpected Hermes config.yaml:\n%s", string(hermesCfg))
	}
	if !fileExists(filepath.Join(home, ".hermes", "config.yaml.nektar.bak")) {
		t.Fatal("expected Hermes config.yaml.nektar.bak backup")
	}

	// Verify OpenClaw L3: embedded plugin + openclaw.json slot/allow/entries/load merged without clobbering existing!
	if !fileExists(filepath.Join(home, ".openclaw", "plugins", "nektar", "index.mjs")) {
		t.Fatal("missing OpenClaw index.mjs")
	}
	ocRaw, _ := os.ReadFile(filepath.Join(home, ".openclaw", "openclaw.json"))
	if !strings.Contains(string(ocRaw), `"memory": "nektar-memory"`) || !strings.Contains(string(ocRaw), `"existing"`) {
		t.Fatalf("unexpected openclaw.json:\n%s", string(ocRaw))
	}

	// 3. DetectAll after docking: all 5 report Docked=true!
	after := DetectAll(home)
	for _, c := range after {
		byID[c.ID] = c
	}
	for _, id := range []string{"claude", "codex", "hermes", "openclaw", "cursor"} {
		if !byID[id].Docked {
			t.Fatalf("after dock: expected client %s Docked=true, got %+v", id, byID[id])
		}
	}

	// 4. Idempotent second run reports Changed=false for every client!
	res2 := Apply(opts, []string{"auto"})
	for _, r := range res2 {
		if r.Changed {
			t.Fatalf("expected idempotent 2nd dock of %s to report Changed=false, got notes=%v", r.Client, r.Notes)
		}
	}

	// 5. Uninstall cleanly removes Nektar while preserving every sibling entry!
	opts.Uninstall = true
	_ = Apply(opts, []string{"auto"})
	afterUn := DetectAll(home)
	for _, c := range afterUn {
		byID[c.ID] = c
	}
	for _, id := range []string{"claude", "codex", "hermes", "openclaw", "cursor"} {
		if byID[id].Docked {
			t.Fatalf("after uninstall: expected client %s Docked=false, got %+v", id, byID[id])
		}
	}
	claudeAfter, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	if !strings.Contains(string(claudeAfter), `"sibling"`) {
		t.Fatalf("uninstall clobbered sibling in .claude.json: %s", string(claudeAfter))
	}
	ocAfter, _ := os.ReadFile(filepath.Join(home, ".openclaw", "openclaw.json"))
	if !strings.Contains(string(ocAfter), `"existing"`) {
		t.Fatalf("uninstall clobbered existing plugin in openclaw.json: %s", string(ocAfter))
	}
}
