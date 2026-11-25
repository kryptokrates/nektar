package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func claudeDesktopPath(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	case "windows":
		appData := envOr("APPDATA", filepath.Join(home, "AppData", "Roaming"))
		return filepath.Join(appData, "Claude", "claude_desktop_config.json")
	default:
		return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json")
	}
}

func vscodeMCPPath(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Code", "User", "mcp.json")
	case "windows":
		appData := envOr("APPDATA", filepath.Join(home, "AppData", "Roaming"))
		return filepath.Join(appData, "Code", "User", "mcp.json")
	default:
		return filepath.Join(home, ".config", "Code", "User", "mcp.json")
	}
}

func zedSettingsPath(home string) string {
	if runtime.GOOS == "windows" {
		appData := envOr("APPDATA", filepath.Join(home, "AppData", "Roaming"))
		return filepath.Join(appData, "Zed", "settings.json")
	}
	return filepath.Join(home, ".config", "zed", "settings.json")
}

func clineMCPPath(home string) string {
	var base string
	switch runtime.GOOS {
	case "darwin":
		base = filepath.Join(home, "Library", "Application Support", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings")
	case "windows":
		appData := envOr("APPDATA", filepath.Join(home, "AppData", "Roaming"))
		base = filepath.Join(appData, "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings")
	default:
		base = filepath.Join(home, ".config", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings")
	}
	return filepath.Join(base, "cline_mcp_settings.json")
}

func mergeOrRemoveJSONEntry(cfgPath, rootKey, entryKey string, val any, dry, uninstall bool) (bool, error) {
	root := make(map[string]any)
	raw, err := os.ReadFile(cfgPath)
	if err == nil && len(bytes.TrimSpace(raw)) > 0 {
		if jErr := json.Unmarshal(raw, &root); jErr != nil {
			return false, fmt.Errorf("parse existing JSON: %w", jErr)
		}
	} else if uninstall {
		return false, nil
	}
	section, _ := root[rootKey].(map[string]any)
	if uninstall {
		if section == nil {
			return false, nil
		}
		if _, ok := section[entryKey]; !ok {
			return false, nil
		}
		delete(section, entryKey)
		root[rootKey] = section
	} else {
		if section == nil {
			section = make(map[string]any)
		}
		section[entryKey] = val
		root[rootKey] = section
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
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o750); err != nil {
		return false, err
	}
	tmp := cfgPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, cfgPath)
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
		"SessionStart":     fmt.Sprintf("%q hook session-start", exePath),
		"UserPromptSubmit": fmt.Sprintf("%q hook prefetch", exePath),
		"Stop":             fmt.Sprintf("%q hook verify-receipt", exePath),
	}
	for event, cmd := range desired {
		existing, _ := hooks[event].([]any)
		var filtered []any
		for _, item := range existing {
			b, _ := json.Marshal(item)
			if strings.Contains(string(b), "hook session-start") || strings.Contains(string(b), "hook prefetch") || strings.Contains(string(b), "hook verify-receipt") {
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
	root["hooks"] = hooks
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
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o750); err != nil {
		return false, err
	}
	return true, os.WriteFile(settingsPath, out, 0o600)
}

func wireCodexToml(cfgPath, exePath string, dry, uninstall bool) (bool, error) {
	raw, _ := os.ReadFile(cfgPath)
	content := string(raw)
	const startMark = "# BEGIN NEKTAR MCP"
	const endMark = "# END NEKTAR MCP"
	re := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(startMark) + `.*?` + regexp.QuoteMeta(endMark) + `\n?`)
	cleaned := re.ReplaceAllString(content, "")
	var next string
	if uninstall {
		next = cleaned
	} else {
		block := fmt.Sprintf("%s\n[mcp_servers.nektar]\ncommand = %q\nargs = [\"mcp\"]\n%s\n", startMark, exePath, endMark)
		next = strings.TrimRight(cleaned, "\n") + "\n\n" + block
		if strings.TrimSpace(cleaned) == "" {
			next = block
		}
	}
	if next == content {
		return false, nil
	}
	if dry {
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o750); err != nil {
		return false, err
	}
	return true, os.WriteFile(cfgPath, []byte(next), 0o600)
}

func wireGooseYaml(cfgPath, exePath string, dry, uninstall bool) (bool, error) {
	raw, _ := os.ReadFile(cfgPath)
	content := string(raw)
	const startMark = "# BEGIN NEKTAR MCP"
	const endMark = "# END NEKTAR MCP"
	re := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(startMark) + `.*?` + regexp.QuoteMeta(endMark) + `\n?`)
	cleaned := re.ReplaceAllString(content, "")
	var next string
	if uninstall {
		next = cleaned
	} else {
		block := fmt.Sprintf("%s\nextensions:\n  nektar:\n    name: nektar\n    type: stdio\n    cmd: %q\n    args: [\"mcp\"]\n    enabled: true\n%s\n", startMark, exePath, endMark)
		next = strings.TrimRight(cleaned, "\n") + "\n\n" + block
		if strings.TrimSpace(cleaned) == "" {
			next = block
		}
	}
	if next == content {
		return false, nil
	}
	if dry {
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o750); err != nil {
		return false, err
	}
	return true, os.WriteFile(cfgPath, []byte(next), 0o600)
}

func locateBundledAsset(rel string) string {
	cands := []string{filepath.Join("/usr/local/share/nektar", rel)}
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		cands = append(cands, filepath.Join(d, rel), filepath.Join(filepath.Dir(d), rel))
	}
	if cwd, err := os.Getwd(); err == nil {
		cands = append(cands, filepath.Join(cwd, rel))
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func installSkillFile(dst, liveURL string, dry, uninstall bool) (bool, error) {
	if uninstall {
		if _, err := os.Stat(dst); err != nil {
			return false, nil
		}
		if dry {
			return true, nil
		}
		return true, os.Remove(dst)
	}
	var content []byte
	if src := locateBundledAsset(filepath.Join("skills", "nektar-memory", "SKILL.md")); src != "" {
		content, _ = os.ReadFile(src)
	}
	if len(content) == 0 && liveURL != "" {
		if resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(liveURL + "/v1/adapters/skill.md"); err == nil {
			if resp.StatusCode == http.StatusOK {
				content, _ = io.ReadAll(io.LimitReader(resp.Body, 256*1024))
			}
			resp.Body.Close()
		}
	}
	if len(content) == 0 {
		return false, nil
	}
	if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, content) {
		return false, nil
	}
	if dry {
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return false, err
	}
	return true, os.WriteFile(dst, content, 0o644)
}

func dockNativeBundle(name, label, dstDir, liveURL string, dry, uninstall bool) dockClientResult {
	res := dockClientResult{Client: name, Label: label, Level: "L3", Files: []string{dstDir}}
	if uninstall {
		if !dirExists(dstDir) {
			return res
		}
		res.Changed = true
		if !dry {
			_ = os.RemoveAll(dstDir)
		}
		res.Notes = append(res.Notes, "removed "+dstDir)
		return res
	}
	if src := locateBundledAsset(filepath.Join("integrations", name)); src != "" {
		if dry {
			res.Changed = true
			res.Notes = append(res.Notes, "would copy "+src+" -> "+dstDir)
			return res
		}
		if err := copyTreeClean(src, dstDir); err != nil {
			res.Notes = append(res.Notes, "copy error: "+err.Error())
		} else {
			res.Changed = true
			res.Notes = append(res.Notes, dstDir)
		}
		return res
	}
	// Fallback: fetch bundle from live daemon /v1/adapters/bundle/{name}
	if liveURL != "" {
		if resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(liveURL + "/v1/adapters/bundle/" + name); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var bundle struct {
					Files []struct {
						Path    string `json:"path"`
						Content string `json:"content"`
					} `json:"files"`
				}
				if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&bundle) == nil && len(bundle.Files) > 0 {
					res.Changed = true
					if dry {
						res.Notes = append(res.Notes, fmt.Sprintf("would fetch %d file(s) -> %s", len(bundle.Files), dstDir))
						return res
					}
					for _, f := range bundle.Files {
						clean := filepath.Clean(filepath.FromSlash(f.Path))
						if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
							continue
						}
						full := filepath.Join(dstDir, clean)
						_ = os.MkdirAll(filepath.Dir(full), 0o750)
						_ = os.WriteFile(full, []byte(f.Content), 0o644)
					}
					res.Notes = append(res.Notes, fmt.Sprintf("installed %d file(s) -> %s", len(bundle.Files), dstDir))
					return res
				}
			}
		}
	}
	res.Notes = append(res.Notes, "start `nektar serve` or install plugin from https://github.com/kryptokrates/nektar/tree/master/integrations/"+name)
	return res
}

func copyTreeClean(srcDir, dstDir string) error {
	return filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		base := d.Name()
		if d.IsDir() {
			if base == "__pycache__" || base == "node_modules" || base == ".pytest_cache" || base == ".git" || base == "tests" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(base, ".pyc") || strings.HasSuffix(base, ".pyo") {
			return nil
		}
		rel, _ := filepath.Rel(srcDir, p)
		dst := filepath.Join(dstDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, raw, 0o644)
	})
}
