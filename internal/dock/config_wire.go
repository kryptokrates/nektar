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
	"sort"
	"strconv"
	"strings"

	nektar "github.com/kryptokrates/nektar"
)

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

var (
	reGooseMCPBlock      = regexp.MustCompile(`(?ms)^[ \t]*# BEGIN NEKTAR MCP.*?^[ \t]*# END NEKTAR MCP[ \t]*\r?\n?`)
	reGooseExtensionsKey = regexp.MustCompile(`(?m)^extensions:\s*(\{\s*\})?\s*(?:#.*)?$`)
)

func wireGooseYaml(cfgPath, exePath string, dry, uninstall bool) (bool, error) {
	raw, _ := os.ReadFile(cfgPath)
	content := string(raw)
	cleaned := reGooseMCPBlock.ReplaceAllString(content, "")
	var next string
	if uninstall {
		next = cleaned
	} else {
		quotedExe := strconv.Quote(exePath)
		if loc := reGooseExtensionsKey.FindStringSubmatchIndex(cleaned); loc != nil {
			// Merge indented nektar sub-block directly under existing top-level `extensions:`
			// without creating a duplicate `extensions:` key.
			subBlock := fmt.Sprintf("  # BEGIN NEKTAR MCP\n  nektar:\n    name: nektar\n    type: stdio\n    cmd: %s\n    args: [\"mcp\"]\n    enabled: true\n  # END NEKTAR MCP\n", quotedExe)
			headerEnd := loc[1]
			prefix := cleaned[:loc[0]] + "extensions:"
			rest := cleaned[headerEnd:]
			if strings.HasPrefix(rest, "\r\n") {
				rest = rest[2:]
			} else if strings.HasPrefix(rest, "\n") {
				rest = rest[1:]
			}
			next = prefix + "\n" + subBlock + rest
		} else {
			block := fmt.Sprintf("# BEGIN NEKTAR MCP\nextensions:\n  nektar:\n    name: nektar\n    type: stdio\n    cmd: %s\n    args: [\"mcp\"]\n    enabled: true\n# END NEKTAR MCP\n", quotedExe)
			if strings.TrimSpace(cleaned) == "" {
				next = block
			} else {
				next = strings.TrimRight(cleaned, "\n") + "\n\n" + block
			}
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
