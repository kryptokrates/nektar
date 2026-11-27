package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DaemonInfo is written atomically to <dataDir>/daemon.json while `nektar serve` is running,
// enabling Hermes, `nektar open`, `nektar dock`, `nektar hook`, and `nektar mcp` (stdio->HTTP bridge)
// to share one brain across many AI clients with zero SQLite/wallet contention.
type DaemonInfo struct {
	PID       int    `json:"pid"`
	Port      int    `json:"port"`
	URL       string `json:"url"`
	Network   string `json:"network"`
	DataDir   string `json:"dataDir"`
	AuthToken string `json:"authToken,omitempty"`
	Token     string `json:"token,omitempty"`
	StartedAt int64  `json:"startedAt"`
}

func DaemonInfoPath(dataDir string) string {
	return filepath.Join(dataDir, "daemon.json")
}

// ActiveDaemonPointerPath returns the cross-network active-daemon pointer file (~/.nektar/active-daemon.json)
// so zero-flag CLI commands, MCP stdio bridges, hooks, Hermes, and OpenClaw automatically find a running
// Mainnet (~/.nektar/main) or Testnet (~/.nektar) daemon without split-brain DB creation.
func ActiveDaemonPointerPath(home string) string {
	if strings.TrimSpace(home) == "" {
		home, _ = os.UserHomeDir()
	}
	if strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".nektar", "active-daemon.json")
}

func writeDaemonFileAtomic(path string, info DaemonInfo) error {
	if info.AuthToken != "" && info.Token == "" {
		info.Token = info.AuthToken
	} else if info.Token != "" && info.AuthToken == "" {
		info.AuthToken = info.Token
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// WriteDaemonInfo atomically writes <dataDir>/daemon.json (mode 0600) so sibling tools can discover the running daemon.
func WriteDaemonInfo(dataDir string, info DaemonInfo) error {
	return writeDaemonFileAtomic(DaemonInfoPath(dataDir), info)
}

// WriteActiveDaemonPointer writes ~/.nektar/active-daemon.json unless dataDir is under os.TempDir()
// or NEKTAR_ISOLATED_DAEMON=1 (preventing test harnesses from clobbering a live user daemon pointer).
func WriteActiveDaemonPointer(home, dataDir string, info DaemonInfo) error {
	if os.Getenv("NEKTAR_ISOLATED_DAEMON") == "1" {
		return nil
	}
	if home == "" {
		if absDir, err := filepath.Abs(dataDir); err == nil {
			if absTmp, tErr := filepath.Abs(os.TempDir()); tErr == nil {
				if strings.HasPrefix(strings.ToLower(absDir), strings.ToLower(absTmp)) {
					return nil
				}
			}
		}
	}
	p := ActiveDaemonPointerPath(home)
	if p == "" {
		return nil
	}
	return writeDaemonFileAtomic(p, info)
}

// RemoveDaemonInfo deletes <dataDir>/daemon.json (and active-daemon.json if owned by this PID) on clean exit.
func RemoveDaemonInfo(dataDir string) {
	removeDaemonFileIfOwned(DaemonInfoPath(dataDir))
	if p := ActiveDaemonPointerPath(""); p != "" {
		removeDaemonFileIfOwned(p)
	}
}

func removeDaemonFileIfOwned(p string) {
	if b, err := os.ReadFile(p); err == nil {
		var info DaemonInfo
		if json.Unmarshal(b, &info) == nil && info.PID > 0 && info.PID != os.Getpid() {
			return
		}
	}
	_ = os.Remove(p)
}

func detectLiveDaemonAtFile(path string) (*DaemonInfo, bool) {
	if strings.TrimSpace(path) == "" {
		return nil, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var info DaemonInfo
	if err := json.Unmarshal(b, &info); err != nil || info.URL == "" {
		return nil, false
	}
	if info.AuthToken == "" && info.Token != "" {
		info.AuthToken = info.Token
	} else if info.Token == "" && info.AuthToken != "" {
		info.Token = info.AuthToken
	}
	hc := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := hc.Get(strings.TrimRight(info.URL, "/") + "/v1/health")
	if err != nil {
		return nil, false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	return &info, true
}

// DetectLiveDaemon reads <dataDir>/daemon.json and verifies GET /v1/health responds within 500ms.
func DetectLiveDaemon(dataDir string) (*DaemonInfo, bool) {
	return detectLiveDaemonAtFile(DaemonInfoPath(dataDir))
}

// DetectAnyLiveDaemon finds the active Nektar daemon across preferredDir, ~/.nektar/active-daemon.json,
// ~/.nektar/main/daemon.json (BSV Mainnet), and ~/.nektar/daemon.json (BSV Testnet).
func DetectAnyLiveDaemon(preferredDir string) (*DaemonInfo, bool) {
	home, _ := os.UserHomeDir()
	return DetectAnyLiveDaemonInHome(home, preferredDir)
}

// DetectAnyLiveDaemonInHome is the hermetic testable core of DetectAnyLiveDaemon.
func DetectAnyLiveDaemonInHome(home, preferredDir string) (*DaemonInfo, bool) {
	var cands []string
	seen := make(map[string]bool)
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		clean := filepath.Clean(p)
		key := strings.ToLower(clean)
		if !seen[key] {
			seen[key] = true
			cands = append(cands, clean)
		}
	}
	if strings.TrimSpace(preferredDir) != "" {
		add(DaemonInfoPath(preferredDir))
	}
	if strings.TrimSpace(home) != "" {
		add(ActiveDaemonPointerPath(home))
		add(filepath.Join(home, ".nektar", "main", "daemon.json"))
		add(filepath.Join(home, ".nektar", "daemon.json"))
	}
	for _, p := range cands {
		if info, ok := detectLiveDaemonAtFile(p); ok {
			return info, true
		}
	}
	return nil, false
}

var detectLiveDaemonFn = DetectAnyLiveDaemon

func extractJSONRPCID(raw []byte) (json.RawMessage, bool) {
	var env struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(raw, &env) != nil || len(bytes.TrimSpace(env.ID)) == 0 {
		return nil, false
	}
	return bytes.TrimSpace(env.ID), true
}

func isValidJSONRPCFrame(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] == '{' {
		var obj map[string]json.RawMessage
		if json.Unmarshal(trimmed, &obj) != nil {
			return false
		}
		_, hasVer := obj["jsonrpc"]
		return hasVer
	}
	if trimmed[0] == '[' {
		var arr []map[string]json.RawMessage
		if json.Unmarshal(trimmed, &arr) != nil || len(arr) == 0 {
			return false
		}
		_, hasVer := arr[0]["jsonrpc"]
		return hasVer
	}
	return false
}

func writeJSONRPCBridgeError(out io.Writer, id json.RawMessage, code int, message string) {
	var idVal any
	if len(id) > 0 {
		idVal = id
	}
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      idVal,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(out, string(frame))
}

func summarizeBridgeHTTPError(status int, body []byte) string {
	trimmed := bytes.TrimSpace(body)
	var j struct {
		Error string `json:"error"`
		Hint  string `json:"hint"`
	}
	if json.Unmarshal(trimmed, &j) == nil && strings.TrimSpace(j.Error) != "" {
		if strings.TrimSpace(j.Hint) != "" {
			return fmt.Sprintf("HTTP %d: %s (%s)", status, j.Error, j.Hint)
		}
		return fmt.Sprintf("HTTP %d: %s", status, j.Error)
	}
	msg := strings.TrimSpace(string(trimmed))
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	if msg == "" {
		return fmt.Sprintf("HTTP %d from nektar daemon", status)
	}
	return fmt.Sprintf("HTTP %d: %s", status, msg)
}

// RunStdioBridge proxies newline-delimited JSON-RPC stdio (stdin -> stdout) to a live daemon's
// Streamable HTTP `/mcp` endpoint so Claude Code, Cursor, Codex, and Hermes share one daemon seamlessly.
func RunStdioBridge(ctx context.Context, daemonURL, bearerToken string, in io.Reader, out io.Writer) error {
	endpoint := strings.TrimRight(daemonURL, "/") + "/mcp"
	hc := &http.Client{Timeout: 60 * time.Second}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	doPost := func(line string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte(line)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if bearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+bearerToken)
		}
		return hc.Do(req)
	}

	for sc.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		reqID, hasID := extractJSONRPCID([]byte(line))

		resp, err := doPost(line)
		if err != nil {
			// On connection refused / daemon restart, re-discover the live daemon once and retry.
			if info, ok := detectLiveDaemonFn(""); ok && info != nil && strings.TrimSpace(info.URL) != "" {
				endpoint = strings.TrimRight(info.URL, "/") + "/mcp"
				if info.AuthToken != "" {
					bearerToken = info.AuthToken
				} else if info.Token != "" {
					bearerToken = info.Token
				}
				resp, err = doPost(line)
			}
		}
		if err != nil {
			if hasID {
				writeJSONRPCBridgeError(out, reqID, -32000, err.Error())
			}
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		ct := resp.Header.Get("Content-Type")
		status := resp.StatusCode
		resp.Body.Close()

		// Notifications (202 Accepted or empty 2xx body) produce no stdout frame.
		if status == http.StatusAccepted || (status >= 200 && status < 300 && len(bytes.TrimSpace(body)) == 0) {
			continue
		}
		if status < 200 || status >= 300 {
			if hasID {
				writeJSONRPCBridgeError(out, reqID, -32000, summarizeBridgeHTTPError(status, body))
			}
			continue
		}
		if strings.Contains(ct, "text/event-stream") {
			for _, ln := range strings.Split(string(body), "\n") {
				if strings.HasPrefix(ln, "data: ") {
					payload := strings.TrimSpace(strings.TrimPrefix(ln, "data: "))
					if payload != "" && isValidJSONRPCFrame([]byte(payload)) {
						_, _ = fmt.Fprintln(out, payload)
					}
				}
			}
			continue
		}
		trimmed := bytes.TrimSpace(body)
		if !isValidJSONRPCFrame(trimmed) {
			if hasID {
				writeJSONRPCBridgeError(out, reqID, -32000, "non-JSON-RPC response from nektar daemon: "+summarizeBridgeHTTPError(status, trimmed))
			}
			continue
		}
		_, _ = fmt.Fprintln(out, string(trimmed))
	}
	return sc.Err()
}
