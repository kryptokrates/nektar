package dock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	reHermesManagedBlock = regexp.MustCompile(`(?ms)^([ \t]*)# BEGIN NEKTAR MEMORY\r?\n(.*?)^[ \t]*# END NEKTAR MEMORY[ \t]*\r?\n?`)
	reHermesPrevProvider = regexp.MustCompile(`(?m)^[ \t]*# nektar-prev-provider:\s*(.+?)\s*$`)
	reHermesMemoryKey    = regexp.MustCompile(`(?m)^memory:\s*(?:#.*)?$`)
)

func isHermesProviderNektar(trimmed string) bool {
	if !strings.HasPrefix(trimmed, "provider:") {
		return false
	}
	val := strings.TrimSpace(strings.TrimPrefix(trimmed, "provider:"))
	val = strings.Trim(val, `"'`)
	return val == "nektar"
}

func stripInlineHermesNektarProvider(content string) string {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	inMemory := false
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if inMemory && len(ln) > 0 && ln[0] != ' ' && ln[0] != '\t' && !strings.HasPrefix(trimmed, "#") {
			inMemory = false
		}
		if !inMemory && (trimmed == "memory:" || strings.HasPrefix(trimmed, "memory: #")) {
			inMemory = true
			out = append(out, ln)
			continue
		}
		if inMemory && isHermesProviderNektar(trimmed) {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

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
		// 1. Remove any managed block (top-level or indented), restoring prior provider if recorded.
		next := reHermesManagedBlock.ReplaceAllStringFunc(content, func(match string) string {
			sub := reHermesManagedBlock.FindStringSubmatch(match)
			if len(sub) >= 3 {
				indent := sub[1]
				body := sub[2]
				if prev := reHermesPrevProvider.FindStringSubmatch(body); len(prev) >= 2 && strings.TrimSpace(prev[1]) != "" {
					return indent + "provider: " + strings.TrimSpace(prev[1]) + "\n"
				}
			}
			return ""
		})
		// 2. Fallback for legacy unmarked inline `provider: nektar` inside `memory:`.
		next = stripInlineHermesNektarProvider(next)
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
		// Insert or replace `provider: nektar` inside existing `memory:` section with reversible markers.
		lines := strings.Split(content, "\n")
		out := make([]string, 0, len(lines)+4)
		inMemory := false
		replaced := false
		emitManaged := func(indent, prevVal string) {
			out = append(out, indent+"# BEGIN NEKTAR MEMORY")
			if prevVal != "" {
				out = append(out, indent+"# nektar-prev-provider: "+prevVal)
			}
			out = append(out, indent+"provider: nektar")
			out = append(out, indent+"# END NEKTAR MEMORY")
		}
		for i, ln := range lines {
			trimmed := strings.TrimSpace(ln)
			if inMemory && len(ln) > 0 && ln[0] != ' ' && ln[0] != '\t' && !strings.HasPrefix(trimmed, "#") {
				if !replaced {
					emitManaged("  ", "")
					replaced = true
				}
				inMemory = false
			}
			if !inMemory && (trimmed == "memory:" || strings.HasPrefix(trimmed, "memory: #")) {
				inMemory = true
				out = append(out, ln)
				continue
			}
			if inMemory && strings.HasPrefix(trimmed, "provider:") {
				indent := ln[:len(ln)-len(strings.TrimLeft(ln, " \t"))]
				prevVal := strings.TrimSpace(strings.TrimPrefix(trimmed, "provider:"))
				emitManaged(indent, prevVal)
				replaced = true
				continue
			}
			out = append(out, ln)
			if i == len(lines)-1 && inMemory && !replaced {
				emitManaged("  ", "")
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

func isLoopbackURL(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" {
		return true
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	h := strings.ToLower(strings.TrimSpace(u.Hostname()))
	return h == "" || h == "127.0.0.1" || h == "localhost" || h == "::1"
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
			if len(slots) > 0 {
				plugins["slots"] = slots
			} else {
				delete(plugins, "slots")
			}
		}
		if entries != nil {
			delete(entries, "nektar-memory")
			if len(entries) > 0 {
				plugins["entries"] = entries
			} else {
				delete(plugins, "entries")
			}
		}
		if allow, ok := plugins["allow"].([]any); ok {
			var filtered []any
			for _, v := range allow {
				if s, _ := v.(string); s != "nektar-memory" {
					filtered = append(filtered, v)
				}
			}
			if len(filtered) > 0 {
				plugins["allow"] = filtered
			} else {
				delete(plugins, "allow")
			}
		}
		if loadObj != nil {
			if paths, ok := loadObj["paths"].([]any); ok {
				var filtered []any
				for _, v := range paths {
					if s, _ := v.(string); !strings.EqualFold(filepath.Clean(s), filepath.Clean(pluginDir)) {
						filtered = append(filtered, v)
					}
				}
				if len(filtered) > 0 {
					loadObj["paths"] = filtered
					plugins["load"] = loadObj
				} else {
					delete(loadObj, "paths")
					if len(loadObj) > 0 {
						plugins["load"] = loadObj
					} else {
						delete(plugins, "load")
					}
				}
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
		pluginCfg := map[string]any{
			"autoRecall":     true,
			"verifyReceipts": true,
		}
		// Omit static loopback URL so OpenClaw dynamically discovers active-daemon.json + authToken
		// across daemon restarts, port changes, and token rotations. Only pin remote non-loopback URLs.
		if !isLoopbackURL(liveURL) {
			pluginCfg["url"] = liveURL
		}
		entries["nektar-memory"] = map[string]any{
			"enabled": true,
			"config":  pluginCfg,
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

func dockOpenAI(home, exePath, liveURL, tok string, dry, uninstall bool) ApplyResult {
	envPath := filepath.Join(home, ".nektar", "openai.env")
	mcpCfgPath := filepath.Join(home, ".nektar", "openai-mcp.json")
	res := ApplyResult{
		Client: "openai",
		Label:  "OpenAI SDK / Responses",
		Level:  "L1",
		Files:  []string{envPath, mcpCfgPath},
	}
	if uninstall {
		removedAny := false
		for _, p := range []string{envPath, mcpCfgPath} {
			if fileExists(p) {
				removedAny = true
				if !dry {
					if err := os.Remove(p); err != nil {
						res.Error = err.Error()
						return res
					}
				}
			}
		}
		res.Changed = removedAny
		if removedAny {
			res.Notes = append(res.Notes, "removed "+envPath)
		}
		return res
	}

	proxyURL := strings.TrimRight(liveURL, "/") + "/v1/proxy/openai/v1"
	mcpURL := strings.TrimRight(liveURL, "/") + "/mcp"
	envBody := fmt.Sprintf("# Source in bash/zsh (`source ~/.nektar/openai.env`) or load via dotenv in Python/TS OpenAI SDKs\nOPENAI_BASE_URL=%q\nNEKTAR_MCP_URL=%q\n", proxyURL, mcpURL)
	mcpDoc := map[string]any{
		"mcpServers": map[string]any{
			"nektar": map[string]any{
				"command": exePath,
				"args":    []string{"mcp"},
			},
		},
		"openaiAgentsSDK": map[string]any{
			"stdio": map[string]any{
				"command": exePath,
				"args":    []string{"mcp"},
			},
			"streamableHttp": map[string]any{
				"url": mcpURL,
			},
			"l0ProxyBaseUrl": proxyURL,
		},
	}
	if tok != "" && !isLoopbackURL(liveURL) {
		if sh, ok := mcpDoc["openaiAgentsSDK"].(map[string]any)["streamableHttp"].(map[string]any); ok {
			sh["headers"] = map[string]string{"Authorization": "Bearer " + tok}
		}
	}
	mcpBytes, _ := json.MarshalIndent(mcpDoc, "", "  ")
	mcpBytes = append(mcpBytes, '\n')

	oldEnv, errEnv := os.ReadFile(envPath)
	oldMCP, errMCP := os.ReadFile(mcpCfgPath)
	envChanged := errEnv != nil || !bytes.Equal(oldEnv, []byte(envBody))
	mcpChanged := errMCP != nil || !bytes.Equal(oldMCP, mcpBytes)
	res.Changed = envChanged || mcpChanged
	if !dry && res.Changed {
		if envChanged {
			if err := writeFileAtomic(envPath, []byte(envBody), 0o600); err != nil {
				res.Error = err.Error()
				return res
			}
		}
		if mcpChanged {
			if err := writeFileAtomic(mcpCfgPath, mcpBytes, 0o600); err != nil {
				res.Error = err.Error()
				return res
			}
		}
	}
	res.Notes = append(res.Notes, "proxy env -> "+envPath, "MCP/SDK config -> "+mcpCfgPath)
	return res
}
