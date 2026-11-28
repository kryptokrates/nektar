package dock

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// BundleFile is one embedded adapter file (Hermes or OpenClaw plugin source).
type BundleFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// ClientStatus describes one AI client detected (or dockable) on this machine.
type ClientStatus struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Tier       string `json:"tier"`      // L1 | L2 | L3
	TierLabel  string `json:"tierLabel"` // plain-English description
	Summary    string `json:"summary"`
	ConfigPath string `json:"configPath"`
	Detected   bool   `json:"detected"`
	Docked     bool   `json:"docked"`
	OneClick   bool   `json:"oneClick"`
	Note       string `json:"note,omitempty"`
}

// ApplyResult summarizes what Apply changed (or would change) for one client.
type ApplyResult struct {
	Client  string   `json:"client"`
	Label   string   `json:"label"`
	Level   string   `json:"level"`
	Changed bool     `json:"changed"`
	Files   []string `json:"files,omitempty"`
	Notes   []string `json:"notes,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// Options configures a dock Apply or Uninstall run.
type Options struct {
	Home      string
	ExePath   string
	LiveURL   string
	Token     string
	Dry       bool
	Uninstall bool
}

type jsonTarget struct {
	ID        string
	Label     string
	Tier      string
	TierLabel string
	Summary   string
	Path      string
	RootKey   string // mcpServers | servers | context_servers | mcp
	Mode      string // stdio | http | opencode | gemini
}

func jsonTargets(home string) []jsonTarget {
	appData := filepath.Join(home, "AppData", "Roaming")
	if realHome, err := os.UserHomeDir(); err == nil && strings.EqualFold(filepath.Clean(home), filepath.Clean(realHome)) {
		if envAD := strings.TrimSpace(os.Getenv("APPDATA")); envAD != "" {
			appData = envAD
		}
	}
	claudeDesktop := filepath.Join(home, ".config", "Claude", "claude_desktop_config.json")
	vscodeMCP := filepath.Join(home, ".config", "Code", "User", "mcp.json")
	zedSettings := filepath.Join(home, ".config", "zed", "settings.json")
	clineMCP := filepath.Join(home, ".config", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")
	switch runtime.GOOS {
	case "darwin":
		claudeDesktop = filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
		vscodeMCP = filepath.Join(home, "Library", "Application Support", "Code", "User", "mcp.json")
		clineMCP = filepath.Join(home, "Library", "Application Support", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")
	case "windows":
		claudeDesktop = filepath.Join(appData, "Claude", "claude_desktop_config.json")
		vscodeMCP = filepath.Join(appData, "Code", "User", "mcp.json")
		zedSettings = filepath.Join(appData, "Zed", "settings.json")
		clineMCP = filepath.Join(appData, "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")
	}
	return []jsonTarget{
		{ID: "claude", Label: "Claude Code", Tier: "L3", TierLabel: "MCP + Hooks + Skill", Summary: "Adds Nektar MCP tools, auto-recall hooks, Stop receipt verifier, and SKILL.md.", Path: filepath.Join(home, ".claude.json"), RootKey: "mcpServers", Mode: "stdio"},
		{ID: "claude-desktop", Label: "Claude Desktop", Tier: "L1", TierLabel: "MCP Tools", Summary: "Gives Claude Desktop encrypted memory, recall, and knowledge trading.", Path: claudeDesktop, RootKey: "mcpServers", Mode: "stdio"},
		{ID: "cursor", Label: "Cursor", Tier: "L1", TierLabel: "MCP Tools", Summary: "Wires Nektar MCP server into Cursor for persistent verified memory.", Path: filepath.Join(home, ".cursor", "mcp.json"), RootKey: "mcpServers", Mode: "stdio"},
		{ID: "vscode", Label: "VS Code", Tier: "L1", TierLabel: "MCP Tools", Summary: "Connects VS Code Copilot/MCP via nektar stdio bridge (auto-discovers live daemon).", Path: vscodeMCP, RootKey: "servers", Mode: "vscode"},
		{ID: "windsurf", Label: "Windsurf", Tier: "L1", TierLabel: "MCP Tools", Summary: "Adds Nektar MCP tools to Windsurf.", Path: filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"), RootKey: "mcpServers", Mode: "stdio"},
		{ID: "zed", Label: "Zed", Tier: "L1", TierLabel: "MCP Context Server", Summary: "Adds Nektar as a Zed context server.", Path: zedSettings, RootKey: "context_servers", Mode: "stdio"},
		{ID: "cline", Label: "Cline", Tier: "L1", TierLabel: "MCP Tools", Summary: "Adds Nektar MCP server to Cline.", Path: clineMCP, RootKey: "mcpServers", Mode: "stdio"},
		{ID: "continue", Label: "Continue", Tier: "L1", TierLabel: "MCP Tools", Summary: "Adds Nektar MCP server to Continue.", Path: filepath.Join(home, ".continue", "mcpServers", "nektar.json"), RootKey: "mcpServers", Mode: "stdio"},
		{ID: "opencode", Label: "OpenCode", Tier: "L1", TierLabel: "MCP Tools", Summary: "Connects OpenCode via nektar stdio bridge (auto-discovers live daemon).", Path: filepath.Join(home, ".config", "opencode", "opencode.json"), RootKey: "mcp", Mode: "opencode"},
		{ID: "gemini", Label: "Gemini CLI", Tier: "L1", TierLabel: "MCP Tools", Summary: "Connects Gemini CLI via nektar stdio bridge (auto-discovers live daemon).", Path: filepath.Join(home, ".gemini", "settings.json"), RootKey: "mcpServers", Mode: "stdio"},
		{ID: "antigravity", Label: "Antigravity", Tier: "L2", TierLabel: "MCP + Skill", Summary: "Wires Nektar stdio MCP server and installs nektar-memory SKILL.md into Antigravity.", Path: filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"), RootKey: "mcpServers", Mode: "stdio"},
	}
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func writeFileAtomic(dst string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// DetectAll inspects the user's home directory and reports every supported AI client and its dock status.
func DetectAll(home string) []ClientStatus {
	if strings.TrimSpace(home) == "" {
		home, _ = os.UserHomeDir()
	}
	var out []ClientStatus
	for _, t := range jsonTargets(home) {
		detected := dirExists(filepath.Dir(t.Path))
		if t.ID == "claude" {
			// Home directory always exists; detect Claude Code via ~/.claude dir or existing ~/.claude.json
			detected = dirExists(filepath.Join(home, ".claude")) || fileExists(t.Path)
		} else if t.ID == "antigravity" {
			detected = dirExists(filepath.Join(home, ".gemini", "antigravity")) || fileExists(t.Path)
		}
		docked := IsJSONEntryDocked(t.Path, t.RootKey, "nektar")
		out = append(out, ClientStatus{
			ID:         t.ID,
			Label:      t.Label,
			Tier:       t.Tier,
			TierLabel:  t.TierLabel,
			Summary:    t.Summary,
			ConfigPath: t.Path,
			Detected:   detected,
			Docked:     docked,
			OneClick:   true,
		})
	}

	codexDir := filepath.Join(home, ".codex")
	codexCfg := filepath.Join(codexDir, "config.toml")
	codexRaw, _ := os.ReadFile(codexCfg)
	out = append(out, ClientStatus{
		ID:         "codex",
		Label:      "Codex CLI",
		Tier:       "L2",
		TierLabel:  "MCP + Notify Hook + Skill",
		Summary:    "Adds Nektar MCP server, turn-complete receipt verification hook, and SKILL.md to Codex CLI.",
		ConfigPath: codexCfg,
		Detected:   dirExists(codexDir),
		Docked:     strings.Contains(string(codexRaw), "[mcp_servers.nektar]"),
		OneClick:   true,
	})

	hermesDir := filepath.Join(home, ".hermes")
	hermesPlug := filepath.Join(hermesDir, "plugins", "hermes-memory-nektar", "plugin.yaml")
	out = append(out, ClientStatus{
		ID:         "hermes",
		Label:      "Hermes Agent",
		Tier:       "L3",
		TierLabel:  "Native MemoryProvider + Skill",
		Summary:    "Installs the embedded Python MemoryProvider plugin, SKILL.md, and enables memory.provider=nektar.",
		ConfigPath: filepath.Join(hermesDir, "plugins", "hermes-memory-nektar"),
		Detected:   dirExists(hermesDir),
		Docked:     fileExists(hermesPlug),
		OneClick:   true,
	})

	ocDir := filepath.Join(home, ".openclaw")
	ocPlug := filepath.Join(ocDir, "plugins", "nektar", "index.mjs")
	out = append(out, ClientStatus{
		ID:         "openclaw",
		Label:      "OpenClaw",
		Tier:       "L3",
		TierLabel:  "Native Memory Slot",
		Summary:    "Installs the embedded OpenClaw memory plugin and wires plugins.slots.memory in openclaw.json.",
		ConfigPath: filepath.Join(ocDir, "plugins", "nektar"),
		Detected:   dirExists(ocDir),
		Docked:     fileExists(ocPlug) && isOpenClawDocked(filepath.Join(ocDir, "openclaw.json")),
		OneClick:   true,
	})

	gooseDir := filepath.Join(home, ".config", "goose")
	gooseCfg := filepath.Join(gooseDir, "config.yaml")
	gooseRaw, _ := os.ReadFile(gooseCfg)
	out = append(out, ClientStatus{
		ID:         "goose",
		Label:      "Goose",
		Tier:       "L1",
		TierLabel:  "MCP Extension",
		Summary:    "Adds Nektar as a stdio MCP extension in Goose config.yaml.",
		ConfigPath: gooseCfg,
		Detected:   dirExists(gooseDir),
		Docked:     strings.Contains(string(gooseRaw), "# BEGIN NEKTAR MCP"),
		OneClick:   true,
	})

	openaiEnvPath := filepath.Join(home, ".nektar", "openai.env")
	out = append(out, ClientStatus{
		ID:         "openai",
		Label:      "OpenAI SDK / Responses",
		Tier:       "L1",
		TierLabel:  "L0 Proxy + Agents SDK MCP",
		Summary:    "Writes ready-to-source OPENAI_BASE_URL memory proxy env and OpenAI Agents/Responses MCP config.",
		ConfigPath: openaiEnvPath,
		Detected:   dirExists(filepath.Join(home, ".openai")) || fileExists(openaiEnvPath),
		Docked:     fileExists(openaiEnvPath),
		OneClick:   true,
	})
	return out
}

func entryPayload(t jsonTarget, exePath, mcpURL, tok string) any {
	switch t.Mode {
	case "vscode":
		return map[string]any{"type": "stdio", "command": exePath, "args": []string{"mcp"}}
	case "opencode":
		return map[string]any{"type": "local", "command": []string{exePath, "mcp"}, "enabled": true}
	case "http":
		e := map[string]any{"type": "http", "url": mcpURL}
		if tok != "" {
			e["headers"] = map[string]string{"Authorization": "Bearer " + tok}
		}
		return e
	case "gemini":
		e := map[string]any{"httpUrl": mcpURL}
		if tok != "" {
			e["headers"] = map[string]string{"Authorization": "Bearer " + tok}
		}
		return e
	default:
		return map[string]any{"command": exePath, "args": []string{"mcp"}}
	}
}

// Apply docks or undocks the requested targets ("auto" or explicit client IDs) cleanly and idempotently.
func Apply(opts Options, targets []string) []ApplyResult {
	home := strings.TrimSpace(opts.Home)
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	exePath := strings.TrimSpace(opts.ExePath)
	if exePath == "" {
		if p, err := os.Executable(); err == nil && p != "" {
			exePath = p
		} else {
			exePath = "nektar"
		}
	}
	liveURL := strings.TrimRight(strings.TrimSpace(opts.LiveURL), "/")
	if liveURL == "" {
		liveURL = "http://127.0.0.1:18420"
	}
	mcpURL := liveURL + "/mcp"

	selected := make(map[string]bool)
	explicit := false
	for _, t := range targets {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || t == "auto" {
			continue
		}
		explicit = true
		selected[t] = true
	}

	var results []ApplyResult
	wantUniversalSkill := false

	for _, t := range jsonTargets(home) {
		detected := dirExists(filepath.Dir(t.Path))
		if t.ID == "claude" {
			detected = dirExists(filepath.Join(home, ".claude")) || fileExists(t.Path)
		} else if t.ID == "antigravity" {
			detected = dirExists(filepath.Join(home, ".gemini", "antigravity")) || fileExists(t.Path)
		}
		if explicit && !selected[t.ID] {
			continue
		}
		if !explicit && !detected {
			continue
		}
		res := ApplyResult{Client: t.ID, Label: t.Label, Level: t.Tier, Files: []string{t.Path}}
		entry := entryPayload(t, exePath, mcpURL, opts.Token)
		changed, mErr := MergeOrRemoveJSONEntry(t.Path, t.RootKey, "nektar", entry, opts.Dry, opts.Uninstall)
		if mErr != nil {
			res.Error = mErr.Error()
			res.Notes = append(res.Notes, "error: "+mErr.Error())
		} else {
			res.Changed = changed
			res.Notes = append(res.Notes, t.Path)
		}
		if t.ID == "claude" {
			hPath := filepath.Join(home, ".claude", "settings.json")
			if hChanged, hErr := wireClaudeHooks(hPath, exePath, opts.Dry, opts.Uninstall); hErr != nil {
				res.Notes = append(res.Notes, "hooks error: "+hErr.Error())
			} else if hChanged {
				res.Changed = true
				res.Files = append(res.Files, hPath)
				res.Notes = append(res.Notes, "hooks -> "+hPath)
			}
			skillDst := filepath.Join(home, ".claude", "skills", "nektar-memory", "SKILL.md")
			if sChanged, sErr := installEmbeddedSkill(skillDst, opts.Dry, opts.Uninstall); sErr == nil && sChanged {
				res.Changed = true
				res.Files = append(res.Files, skillDst)
				res.Notes = append(res.Notes, "skill -> "+skillDst)
			}
		} else if t.ID == "antigravity" {
			agySkill := filepath.Join(home, ".gemini", "antigravity", "skills", "nektar-memory", "SKILL.md")
			if sChanged, sErr := installEmbeddedSkill(agySkill, opts.Dry, opts.Uninstall); sErr == nil && sChanged {
				res.Changed = true
				res.Files = append(res.Files, agySkill)
				res.Notes = append(res.Notes, "skill -> "+agySkill)
			}
			wantUniversalSkill = true
		} else if t.ID == "cursor" || t.ID == "windsurf" || t.ID == "gemini" || t.ID == "opencode" || t.ID == "cline" {
			wantUniversalSkill = true
		}
		results = append(results, res)
	}

	// Codex CLI (~/.codex/config.toml + notify hook + ~/.codex/skills + ~/.agents/skills)
	codexDir := filepath.Join(home, ".codex")
	if (explicit && selected["codex"]) || (!explicit && dirExists(codexDir)) {
		cfg := filepath.Join(codexDir, "config.toml")
		r := ApplyResult{Client: "codex", Label: "Codex CLI", Level: "L2", Files: []string{cfg}}
		changed, notifyNote, cErr := wireCodexToml(cfg, exePath, opts.Dry, opts.Uninstall)
		if cErr != nil {
			r.Error = cErr.Error()
			r.Notes = append(r.Notes, "error: "+cErr.Error())
		} else {
			r.Changed = changed
			r.Notes = append(r.Notes, cfg)
			if notifyNote != "" {
				r.Notes = append(r.Notes, notifyNote)
			}
		}
		codexSkill := filepath.Join(codexDir, "skills", "nektar-memory", "SKILL.md")
		if sChanged, sErr := installEmbeddedSkill(codexSkill, opts.Dry, opts.Uninstall); sErr == nil && sChanged {
			r.Changed = true
			r.Files = append(r.Files, codexSkill)
			r.Notes = append(r.Notes, "skill -> "+codexSkill)
		}
		wantUniversalSkill = true
		results = append(results, r)
	}

	// Hermes Agent (~/.hermes/plugins/hermes-memory-nektar + skill + config.yaml)
	hermesDir := filepath.Join(home, ".hermes")
	if (explicit && selected["hermes"]) || (!explicit && dirExists(hermesDir)) {
		results = append(results, dockHermes(hermesDir, opts.Dry, opts.Uninstall))
	}

	// OpenClaw (~/.openclaw/plugins/nektar + openclaw.json slot merge)
	ocDir := filepath.Join(home, ".openclaw")
	if (explicit && selected["openclaw"]) || (!explicit && dirExists(ocDir)) {
		results = append(results, dockOpenClaw(ocDir, liveURL, opts.Dry, opts.Uninstall))
	}

	// Goose (~/.config/goose/config.yaml)
	gooseDir := filepath.Join(home, ".config", "goose")
	if (explicit && selected["goose"]) || (!explicit && dirExists(gooseDir)) {
		cfg := filepath.Join(gooseDir, "config.yaml")
		changed, gErr := wireGooseYaml(cfg, exePath, opts.Dry, opts.Uninstall)
		r := ApplyResult{Client: "goose", Label: "Goose", Level: "L1", Changed: changed, Files: []string{cfg}}
		if gErr != nil {
			r.Error = gErr.Error()
			r.Notes = append(r.Notes, "error: "+gErr.Error())
		} else {
			r.Notes = append(r.Notes, cfg)
		}
		results = append(results, r)
	}

	if wantUniversalSkill {
		agentSkill := filepath.Join(home, ".agents", "skills", "nektar-memory", "SKILL.md")
		_, _ = installEmbeddedSkill(agentSkill, opts.Dry, opts.Uninstall)
	}

	openaiEnvPath := filepath.Join(home, ".nektar", "openai.env")
	if (explicit && selected["openai"]) || (!explicit && (dirExists(filepath.Join(home, ".openai")) || fileExists(openaiEnvPath))) {
		results = append(results, dockOpenAI(home, exePath, liveURL, opts.Token, opts.Dry, opts.Uninstall))
	}

	if selected["proxy"] {
		results = append(results, ApplyResult{
			Client: "proxy",
			Label:  "L0 LLM Memory Proxy",
			Level:  "L0",
			Notes:  []string{liveURL + "/v1/proxy/openai/v1", liveURL + "/v1/proxy/anthropic"},
		})
	}
	return results
}
