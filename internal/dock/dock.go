package dock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	nektar "github.com/kryptokrates/nektar"
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

// SkillMarkdown returns the universal agentskills.io SKILL.md embedded in the binary.
func SkillMarkdown() []byte {
	b, err := fs.ReadFile(nektar.EmbeddedAdaptersFS, "skills/nektar-memory/SKILL.md")
	if err == nil && len(b) > 0 {
		return b
	}
	return []byte("# nektar-memory\nCall recall first, route factual answers through groundedAnswer, and preserve the nektarr1.* receipt footer verbatim.\n")
}

// BundleFiles returns the embedded plugin files for "hermes" or "openclaw".
func BundleFiles(name string) []BundleFile {
	name = strings.ToLower(strings.TrimSpace(name))
	if name != "hermes" && name != "openclaw" {
		return nil
	}
	root := path.Join("integrations", name)
	var out []BundleFile
	_ = fs.WalkDir(nektar.EmbeddedAdaptersFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		raw, rErr := fs.ReadFile(nektar.EmbeddedAdaptersFS, p)
		if rErr != nil {
			return nil
		}
		rel := strings.TrimPrefix(p, root+"/")
		out = append(out, BundleFile{Path: rel, Content: string(raw)})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
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
		{ID: "vscode", Label: "VS Code", Tier: "L1", TierLabel: "Streamable HTTP MCP", Summary: "Connects VS Code Copilot/MCP directly to this daemon over HTTP.", Path: vscodeMCP, RootKey: "servers", Mode: "http"},
		{ID: "windsurf", Label: "Windsurf", Tier: "L1", TierLabel: "MCP Tools", Summary: "Adds Nektar MCP tools to Windsurf.", Path: filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"), RootKey: "mcpServers", Mode: "stdio"},
		{ID: "zed", Label: "Zed", Tier: "L1", TierLabel: "MCP Context Server", Summary: "Adds Nektar as a Zed context server.", Path: zedSettings, RootKey: "context_servers", Mode: "stdio"},
		{ID: "cline", Label: "Cline", Tier: "L1", TierLabel: "MCP Tools", Summary: "Adds Nektar MCP server to Cline.", Path: clineMCP, RootKey: "mcpServers", Mode: "stdio"},
		{ID: "continue", Label: "Continue", Tier: "L1", TierLabel: "MCP Tools", Summary: "Adds Nektar MCP server to Continue.", Path: filepath.Join(home, ".continue", "mcpServers", "nektar.json"), RootKey: "mcpServers", Mode: "stdio"},
		{ID: "opencode", Label: "OpenCode", Tier: "L1", TierLabel: "Remote HTTP MCP", Summary: "Connects OpenCode directly to /mcp.", Path: filepath.Join(home, ".config", "opencode", "opencode.json"), RootKey: "mcp", Mode: "opencode"},
		{ID: "gemini", Label: "Gemini CLI", Tier: "L1", TierLabel: "Streamable HTTP MCP", Summary: "Connects Gemini CLI to /mcp.", Path: filepath.Join(home, ".gemini", "settings.json"), RootKey: "mcpServers", Mode: "gemini"},
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

// IsJSONEntryDocked checks whether entryKey exists inside rootKey of cfgPath.
func IsJSONEntryDocked(cfgPath, rootKey, entryKey string) bool {
	raw, err := os.ReadFile(cfgPath)
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return false
	}
	sec, _ := root[rootKey].(map[string]any)
	if sec == nil {
		return false
	}
	_, ok := sec[entryKey]
	return ok
}

// MergeOrRemoveJSONEntry non-destructively sets or removes root[rootKey][entryKey].
func MergeOrRemoveJSONEntry(cfgPath, rootKey, entryKey string, val any, dry, uninstall bool) (bool, error) {
	root := make(map[string]any)
	raw, err := os.ReadFile(cfgPath)
	if err == nil && len(bytes.TrimSpace(raw)) > 0 {
		if jErr := json.Unmarshal(raw, &root); jErr != nil {
			return false, fmt.Errorf("parse %s: %w", filepath.Base(cfgPath), jErr)
		}
	} else if uninstall {
		return false, nil
	}
	sec, _ := root[rootKey].(map[string]any)
	if uninstall {
		if sec == nil {
			return false, nil
		}
		if _, ok := sec[entryKey]; !ok {
			return false, nil
		}
		delete(sec, entryKey)
		root[rootKey] = sec
	} else {
		if sec == nil {
			sec = make(map[string]any)
		}
		sec[entryKey] = val
		root[rootKey] = sec
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')
	if bytes.Equal(raw, out) {
		return false, nil
	}
	if dry {
		return true, nil
	}
	return true, writeFileAtomic(cfgPath, out, 0o600)
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
	return out
}

func entryPayload(t jsonTarget, exePath, mcpURL, tok string) any {
	switch t.Mode {
	case "http":
		e := map[string]any{"type": "http", "url": mcpURL}
		if tok != "" {
			e["headers"] = map[string]string{"Authorization": "Bearer " + tok}
		}
		return e
	case "opencode":
		e := map[string]any{"type": "remote", "url": mcpURL, "enabled": true}
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

func quoteHookCommand(exePath, subArgs string) string {
	// Wrap exePath in shell double quotes without Go-escaping backslashes (json.Marshal handles JSON escaping).
	return fmt.Sprintf(`"%s" %s`, strings.ReplaceAll(exePath, `"`, `\"`), subArgs)
}

func wireClaudeHooks(settingsPath, exePath string, dry, uninstall bool) (bool, error) {
	root := make(map[string]any)
	raw, err := os.ReadFile(settingsPath)
	if err == nil && len(bytes.TrimSpace(raw)) > 0 {
		if jErr := json.Unmarshal(raw, &root); jErr != nil {
			return false, jErr
		}
	} else if uninstall {
		return false, nil
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = make(map[string]any)
	}
	desired := map[string]string{
		"SessionStart":     quoteHookCommand(exePath, "hook session-start"),
		"UserPromptSubmit": quoteHookCommand(exePath, "hook prefetch"),
		"Stop":             quoteHookCommand(exePath, "hook verify-receipt"),
	}
	for event, cmd := range desired {
		existing, _ := hooks[event].([]any)
		var filtered []any
		for _, item := range existing {
			b, _ := json.Marshal(item)
			s := string(b)
			if strings.Contains(s, "hook session-start") || strings.Contains(s, "hook prefetch") || strings.Contains(s, "hook verify-receipt") {
				continue
			}
			filtered = append(filtered, item)
		}
		if !uninstall {
			filtered = append(filtered, map[string]any{
				"hooks": []map[string]any{{"type": "command", "command": cmd}},
			})
		}
		if len(filtered) > 0 {
			hooks[event] = filtered
		} else {
			delete(hooks, event)
		}
	}
	if len(hooks) > 0 {
		root["hooks"] = hooks
	} else {
		delete(root, "hooks")
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')
	if bytes.Equal(raw, out) {
		return false, nil
	}
	if dry {
		return true, nil
	}
	return true, writeFileAtomic(settingsPath, out, 0o600)
}

var (
	reCodexMCPBlock    = regexp.MustCompile(`(?s)# BEGIN NEKTAR MCP.*?# END NEKTAR MCP\n?`)
	reCodexNotifyBlock = regexp.MustCompile(`(?s)# BEGIN NEKTAR NOTIFY.*?# END NEKTAR NOTIFY\n?`)
	reExistingNotify   = regexp.MustCompile(`(?m)^\s*notify\s*=`)
)

// wireCodexToml merges [mcp_servers.nektar] AND top-level `notify` receipt verification
// into ~/.codex/config.toml (placing top-level `notify` BEFORE any `[table]` header per TOML spec!).
func wireCodexToml(cfgPath, exePath string, dry, uninstall bool) (bool, string, error) {
	raw, _ := os.ReadFile(cfgPath)
	content := string(raw)
	cleaned := reCodexNotifyBlock.ReplaceAllString(content, "")
	cleaned = reCodexMCPBlock.ReplaceAllString(cleaned, "")
	cleaned = strings.TrimLeft(cleaned, "\n")

	var next string
	notifyNote := ""
	if uninstall {
		next = cleaned
	} else {
		quotedExe := strconv.Quote(exePath)
		mcpBlock := fmt.Sprintf("# BEGIN NEKTAR MCP\n[mcp_servers.nektar]\ncommand = %s\nargs = [\"mcp\"]\n# END NEKTAR MCP\n", quotedExe)
		notifyPrefix := ""
		if !reExistingNotify.MatchString(cleaned) {
			notifyPrefix = fmt.Sprintf("# BEGIN NEKTAR NOTIFY\nnotify = [%s, \"hook\", \"verify-receipt\", \"--codex\"]\n# END NEKTAR NOTIFY\n", quotedExe)
			notifyNote = "notify hook -> verify-receipt (--codex)"
		} else {
			notifyNote = "kept existing custom `notify` setting in config.toml"
		}
		trimmed := strings.TrimSpace(cleaned)
		if trimmed == "" {
			next = notifyPrefix + mcpBlock
		} else {
			if notifyPrefix != "" {
				next = notifyPrefix + "\n" + strings.TrimRight(cleaned, "\n") + "\n\n" + mcpBlock
			} else {
				next = strings.TrimRight(cleaned, "\n") + "\n\n" + mcpBlock
			}
		}
	}
	if next == content {
		return false, notifyNote, nil
	}
	if dry {
		return true, notifyNote, nil
	}
	return true, notifyNote, writeFileAtomic(cfgPath, []byte(next), 0o600)
}

var reGooseMCPBlock = regexp.MustCompile(`(?s)# BEGIN NEKTAR MCP.*?# END NEKTAR MCP\n?`)

func wireGooseYaml(cfgPath, exePath string, dry, uninstall bool) (bool, error) {
	raw, _ := os.ReadFile(cfgPath)
	content := string(raw)
	cleaned := reGooseMCPBlock.ReplaceAllString(content, "")
	var next string
	if uninstall {
		next = cleaned
	} else {
		block := fmt.Sprintf("# BEGIN NEKTAR MCP\nextensions:\n  nektar:\n    name: nektar\n    type: stdio\n    cmd: %s\n    args: [\"mcp\"]\n    enabled: true\n# END NEKTAR MCP\n", strconv.Quote(exePath))
		if strings.TrimSpace(cleaned) == "" {
			next = block
		} else {
			next = strings.TrimRight(cleaned, "\n") + "\n\n" + block
		}
	}
	if next == content {
		return false, nil
	}
	if dry {
		return true, nil
	}
	return true, writeFileAtomic(cfgPath, []byte(next), 0o600)
}

func installEmbeddedSkill(dst string, dry, uninstall bool) (bool, error) {
	if uninstall {
		if !fileExists(dst) {
			return false, nil
		}
		if dry {
			return true, nil
		}
		return true, os.Remove(dst)
	}
	content := SkillMarkdown()
	if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, content) {
		return false, nil
	}
	if dry {
		return true, nil
	}
	return true, writeFileAtomic(dst, content, 0o644)
}

func installEmbeddedBundle(name, dstDir string, dry, uninstall bool) (bool, int, error) {
	if uninstall {
		if !dirExists(dstDir) {
			return false, 0, nil
		}
		if dry {
			return true, 0, nil
		}
		return true, 0, os.RemoveAll(dstDir)
	}
	files := BundleFiles(name)
	if len(files) == 0 {
		return false, 0, fmt.Errorf("embedded bundle %q is empty", name)
	}
	changed := false
	for _, f := range files {
		clean := filepath.Clean(filepath.FromSlash(f.Path))
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			continue
		}
		full := filepath.Join(dstDir, clean)
		want := []byte(f.Content)
		if old, err := os.ReadFile(full); err == nil && bytes.Equal(old, want) {
			continue
		}
		changed = true
		if !dry {
			if err := writeFileAtomic(full, want, 0o644); err != nil {
				return false, 0, err
			}
		}
	}
	return changed, len(files), nil
}

var (
	reHermesManagedBlock = regexp.MustCompile(`(?s)# BEGIN NEKTAR MEMORY.*?# END NEKTAR MEMORY\n?`)
	reHermesMemoryKey    = regexp.MustCompile(`(?m)^memory:\s*$`)
	reHermesProviderLine = regexp.MustCompile(`(?m)^(\s+provider:\s*).*$`)
)

func wireHermesConfigYaml(cfgPath string, dry, uninstall bool) (bool, error) {
	raw, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	content := string(raw)
	if uninstall {
		if strings.TrimSpace(content) == "" {
			return false, nil
		}
		next := reHermesManagedBlock.ReplaceAllString(content, "")
		if next == content {
			return false, nil
		}
		if dry {
			return true, nil
		}
		return true, writeFileAtomic(cfgPath, []byte(next), 0o600)
	}
	// If already configured for nektar, idempotent no-op.
	if strings.Contains(content, "provider: nektar") || strings.Contains(content, `provider: "nektar"`) {
		return false, nil
	}
	var next string
	if reHermesMemoryKey.MatchString(content) {
		// Insert or replace `provider: nektar` inside existing `memory:` section without clobbering siblings.
		lines := strings.Split(content, "\n")
		out := make([]string, 0, len(lines)+2)
		inMemory := false
		replaced := false
		for i, ln := range lines {
			trimmed := strings.TrimSpace(ln)
			if inMemory && len(ln) > 0 && ln[0] != ' ' && ln[0] != '\t' && !strings.HasPrefix(trimmed, "#") {
				if !replaced {
					out = append(out, "  provider: nektar")
					replaced = true
				}
				inMemory = false
			}
			if !inMemory && trimmed == "memory:" {
				inMemory = true
				out = append(out, ln)
				continue
			}
			if inMemory && strings.HasPrefix(trimmed, "provider:") {
				indent := ln[:len(ln)-len(strings.TrimLeft(ln, " \t"))]
				out = append(out, indent+"provider: nektar")
				replaced = true
				continue
			}
			out = append(out, ln)
			if i == len(lines)-1 && inMemory && !replaced {
				out = append(out, "  provider: nektar")
				replaced = true
			}
		}
		next = strings.Join(out, "\n")
	} else {
		block := "# BEGIN NEKTAR MEMORY\nmemory:\n  provider: nektar\n# END NEKTAR MEMORY\n"
		if strings.TrimSpace(content) == "" {
			next = block
		} else {
			next = strings.TrimRight(content, "\n") + "\n\n" + block
		}
	}
	if next == content {
		return false, nil
	}
	if dry {
		return true, nil
	}
	// Save one-time backup of existing config before mutating.
	if len(raw) > 0 && !fileExists(cfgPath+".nektar.bak") {
		_ = os.WriteFile(cfgPath+".nektar.bak", raw, 0o600)
	}
	return true, writeFileAtomic(cfgPath, []byte(next), 0o600)
}

func dockHermes(hermesDir string, dry, uninstall bool) ApplyResult {
	plugDir := filepath.Join(hermesDir, "plugins", "hermes-memory-nektar")
	skillDst := filepath.Join(hermesDir, "skills", "nektar-memory", "SKILL.md")
	cfgPath := filepath.Join(hermesDir, "config.yaml")
	res := ApplyResult{
		Client: "hermes",
		Label:  "Hermes Agent",
		Level:  "L3",
		Files:  []string{plugDir, skillDst, cfgPath},
	}
	changed, count, err := installEmbeddedBundle("hermes", plugDir, dry, uninstall)
	if err != nil {
		res.Error = err.Error()
		res.Notes = append(res.Notes, "plugin error: "+err.Error())
		return res
	}
	if changed {
		res.Changed = true
		if uninstall {
			res.Notes = append(res.Notes, "removed "+plugDir)
		} else {
			res.Notes = append(res.Notes, fmt.Sprintf("plugin (%d files) -> %s", count, plugDir))
		}
	} else if !uninstall {
		res.Notes = append(res.Notes, plugDir)
	}
	if sChanged, sErr := installEmbeddedSkill(skillDst, dry, uninstall); sErr == nil && sChanged {
		res.Changed = true
		res.Notes = append(res.Notes, "skill -> "+skillDst)
	}
	if cChanged, cErr := wireHermesConfigYaml(cfgPath, dry, uninstall); cErr != nil {
		res.Notes = append(res.Notes, "config.yaml note: "+cErr.Error())
	} else if cChanged {
		res.Changed = true
		res.Notes = append(res.Notes, "memory.provider=nektar -> "+cfgPath)
	}
	return res
}

func isOpenClawDocked(cfgPath string) bool {
	raw, err := os.ReadFile(cfgPath)
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return false
	}
	plugins, _ := root["plugins"].(map[string]any)
	if plugins == nil {
		return false
	}
	slots, _ := plugins["slots"].(map[string]any)
	if slots != nil && slots["memory"] == "nektar-memory" {
		return true
	}
	entries, _ := plugins["entries"].(map[string]any)
	if entries != nil {
		_, ok := entries["nektar-memory"]
		return ok
	}
	return false
}

func wireOpenClawConfig(cfgPath, pluginDir, liveURL string, dry, uninstall bool) (bool, error) {
	root := make(map[string]any)
	raw, err := os.ReadFile(cfgPath)
	if err == nil && len(bytes.TrimSpace(raw)) > 0 {
		if jErr := json.Unmarshal(raw, &root); jErr != nil {
			return false, fmt.Errorf("parse openclaw.json: %w", jErr)
		}
	} else if uninstall {
		return false, nil
	}

	plugins, _ := root["plugins"].(map[string]any)
	if plugins == nil {
		if uninstall {
			return false, nil
		}
		plugins = make(map[string]any)
	}
	slots, _ := plugins["slots"].(map[string]any)
	entries, _ := plugins["entries"].(map[string]any)
	loadObj, _ := plugins["load"].(map[string]any)

	if uninstall {
		if slots != nil && slots["memory"] == "nektar-memory" {
			delete(slots, "memory")
			plugins["slots"] = slots
		}
		if entries != nil {
			delete(entries, "nektar-memory")
			plugins["entries"] = entries
		}
		if allow, ok := plugins["allow"].([]any); ok {
			var filtered []any
			for _, v := range allow {
				if s, _ := v.(string); s != "nektar-memory" {
					filtered = append(filtered, v)
				}
			}
			plugins["allow"] = filtered
		}
		if loadObj != nil {
			if paths, ok := loadObj["paths"].([]any); ok {
				var filtered []any
				for _, v := range paths {
					if s, _ := v.(string); !strings.EqualFold(filepath.Clean(s), filepath.Clean(pluginDir)) {
						filtered = append(filtered, v)
					}
				}
				loadObj["paths"] = filtered
				plugins["load"] = loadObj
			}
		}
		root["plugins"] = plugins
	} else {
		if slots == nil {
			slots = make(map[string]any)
		}
		slots["memory"] = "nektar-memory"
		plugins["slots"] = slots

		var allow []any
		if existing, ok := plugins["allow"].([]any); ok {
			allow = existing
		}
		hasAllow := false
		for _, v := range allow {
			if s, _ := v.(string); s == "nektar-memory" {
				hasAllow = true
				break
			}
		}
		if !hasAllow {
			allow = append(allow, "nektar-memory")
		}
		plugins["allow"] = allow

		if loadObj == nil {
			loadObj = make(map[string]any)
		}
		var paths []any
		if existing, ok := loadObj["paths"].([]any); ok {
			paths = existing
		}
		hasPath := false
		for _, v := range paths {
			if s, _ := v.(string); strings.EqualFold(filepath.Clean(s), filepath.Clean(pluginDir)) {
				hasPath = true
				break
			}
		}
		if !hasPath {
			paths = append(paths, pluginDir)
		}
		loadObj["paths"] = paths
		plugins["load"] = loadObj

		if entries == nil {
			entries = make(map[string]any)
		}
		entries["nektar-memory"] = map[string]any{
			"enabled": true,
			"config": map[string]any{
				"url":            liveURL,
				"autoRecall":     true,
				"verifyReceipts": true,
			},
			"hooks": map[string]any{
				"allowPromptInjection":    true,
				"allowConversationAccess": true,
			},
		}
		plugins["entries"] = entries
		root["plugins"] = plugins
	}

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')
	if bytes.Equal(raw, out) {
		return false, nil
	}
	if dry {
		return true, nil
	}
	return true, writeFileAtomic(cfgPath, out, 0o600)
}

func dockOpenClaw(ocDir, liveURL string, dry, uninstall bool) ApplyResult {
	plugDir := filepath.Join(ocDir, "plugins", "nektar")
	cfgPath := filepath.Join(ocDir, "openclaw.json")
	res := ApplyResult{
		Client: "openclaw",
		Label:  "OpenClaw",
		Level:  "L3",
		Files:  []string{plugDir, cfgPath},
	}
	changed, count, err := installEmbeddedBundle("openclaw", plugDir, dry, uninstall)
	if err != nil {
		res.Error = err.Error()
		res.Notes = append(res.Notes, "plugin error: "+err.Error())
		return res
	}
	if changed {
		res.Changed = true
		if uninstall {
			res.Notes = append(res.Notes, "removed "+plugDir)
		} else {
			res.Notes = append(res.Notes, fmt.Sprintf("plugin (%d files) -> %s", count, plugDir))
		}
	} else if !uninstall {
		res.Notes = append(res.Notes, plugDir)
	}
	if cChanged, cErr := wireOpenClawConfig(cfgPath, plugDir, liveURL, dry, uninstall); cErr != nil {
		res.Error = cErr.Error()
		res.Notes = append(res.Notes, "openclaw.json error: "+cErr.Error())
	} else if cChanged {
		res.Changed = true
		res.Notes = append(res.Notes, "memory slot -> "+cfgPath)
	}
	return res
}
