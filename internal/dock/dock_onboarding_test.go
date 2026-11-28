package dock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockAntigravityAndOpenAISDKIdempotentAndUninstall(t *testing.T) {
	home := t.TempDir()
	agyDir := filepath.Join(home, ".gemini", "antigravity")
	openaiDir := filepath.Join(home, ".openai")
	if err := os.MkdirAll(agyDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(openaiDir, 0o750); err != nil {
		t.Fatal(err)
	}
	agyCfg := filepath.Join(agyDir, "mcp_config.json")
	if err := os.WriteFile(agyCfg, []byte(`{"mcpServers":{"existing":{"command":"other-mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	opts := Options{
		Home:    home,
		ExePath: "/usr/local/bin/nektar",
		LiveURL: "http://127.0.0.1:18420",
		Token:   "tok-xyz",
	}

	// 1. Auto-detect both Antigravity and OpenAI SDK before docking.
	before := DetectAll(home)
	byID := make(map[string]ClientStatus)
	for _, c := range before {
		byID[c.ID] = c
	}
	for _, id := range []string{"antigravity", "openai"} {
		if !byID[id].Detected || byID[id].Docked {
			t.Fatalf("before dock: %s expected detected=true docked=false, got %+v", id, byID[id])
		}
	}

	// 2. Dock explicitly and verify non-clobbering config + SKILL.md + OpenAI env/MCP files.
	res := Apply(opts, []string{"antigravity", "openai"})
	if len(res) != 2 {
		t.Fatalf("expected 2 results, got %d: %+v", len(res), res)
	}
	for _, r := range res {
		if r.Error != "" || !r.Changed {
			t.Fatalf("unexpected dock result for %s: %+v", r.Client, r)
		}
	}

	agyRaw, err := os.ReadFile(agyCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agyRaw), `"nektar"`) || !strings.Contains(string(agyRaw), `"existing"`) {
		t.Fatalf("unexpected Antigravity mcp_config.json:\n%s", string(agyRaw))
	}
	agySkill := filepath.Join(agyDir, "skills", "nektar-memory", "SKILL.md")
	skillRaw, err := os.ReadFile(agySkill)
	if err != nil {
		t.Fatalf("missing Antigravity SKILL.md: %v", err)
	}
	for _, verb := range []string{"buyCapsule", "sellCapsule", "importCapsule"} {
		if !strings.Contains(string(skillRaw), verb) {
			t.Fatalf("embedded SKILL.md missing %q", verb)
		}
	}

	openaiEnvRaw, err := os.ReadFile(filepath.Join(home, ".nektar", "openai.env"))
	if err != nil {
		t.Fatalf("missing openai.env: %v", err)
	}
	if !strings.Contains(string(openaiEnvRaw), "http://127.0.0.1:18420/v1/proxy/openai/v1") {
		t.Fatalf("unexpected openai.env:\n%s", string(openaiEnvRaw))
	}
	openaiMCPRaw, err := os.ReadFile(filepath.Join(home, ".nektar", "openai-mcp.json"))
	if err != nil {
		t.Fatalf("missing openai-mcp.json: %v", err)
	}
	if !strings.Contains(string(openaiMCPRaw), "http://127.0.0.1:18420/mcp") {
		t.Fatalf("unexpected openai-mcp.json:\n%s", string(openaiMCPRaw))
	}

	// Verify Hermes & OpenClaw embedded bundles also include 1-call capsule tools.
	foundHermes := false
	for _, f := range BundleFiles("hermes") {
		if f.Path == "hermes_memory_nektar/provider.py" {
			foundHermes = strings.Contains(f.Content, "nektar_buy_capsule") &&
				strings.Contains(f.Content, "nektar_sell_capsule") &&
				strings.Contains(f.Content, "nektar_import_capsule")
		}
	}
	if !foundHermes {
		t.Fatal("embedded Hermes provider.py missing 1-call capsule tools")
	}
	foundOC := false
	for _, f := range BundleFiles("openclaw") {
		if f.Path == "index.mjs" {
			foundOC = strings.Contains(f.Content, "nektar_buy_capsule") &&
				strings.Contains(f.Content, "nektar_sell_capsule") &&
				strings.Contains(f.Content, "nektar_import_capsule")
		}
	}
	if !foundOC {
		t.Fatal("embedded OpenClaw index.mjs missing 1-call capsule tools")
	}

	// 3. Idempotent second run reports Changed=false.
	res2 := Apply(opts, []string{"antigravity", "openai"})
	for _, r := range res2 {
		if r.Changed {
			t.Fatalf("expected idempotent 2nd run for %s to report Changed=false", r.Client)
		}
	}

	// 4. Uninstall removes Nektar entries while keeping Antigravity sibling MCP servers intact.
	opts.Uninstall = true
	_ = Apply(opts, []string{"antigravity", "openai"})
	afterAgy, _ := os.ReadFile(agyCfg)
	if strings.Contains(string(afterAgy), `"nektar"`) || !strings.Contains(string(afterAgy), `"existing"`) {
		t.Fatalf("uninstall corrupted Antigravity mcp_config.json:\n%s", string(afterAgy))
	}
	if fileExists(filepath.Join(home, ".nektar", "openai.env")) {
		t.Fatal("expected openai.env to be removed on uninstall")
	}
}
