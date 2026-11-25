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

// WriteDaemonInfo atomically writes daemon.json (mode 0600) so sibling tools can discover the running daemon.
func WriteDaemonInfo(dataDir string, info DaemonInfo) error {
	if info.AuthToken != "" && info.Token == "" {
		info.Token = info.AuthToken
	} else if info.Token != "" && info.AuthToken == "" {
		info.AuthToken = info.Token
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	p := DaemonInfoPath(dataDir)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// RemoveDaemonInfo deletes daemon.json on clean daemon exit, only if owned by this PID.
func RemoveDaemonInfo(dataDir string) {
	p := DaemonInfoPath(dataDir)
	if b, err := os.ReadFile(p); err == nil {
		var info DaemonInfo
		if json.Unmarshal(b, &info) == nil && info.PID > 0 && info.PID != os.Getpid() {
			return
		}
	}
	_ = os.Remove(p)
}

// DetectLiveDaemon reads <dataDir>/daemon.json and verifies GET /v1/health responds within 500ms.
func DetectLiveDaemon(dataDir string) (*DaemonInfo, bool) {
	b, err := os.ReadFile(DaemonInfoPath(dataDir))
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

// RunStdioBridge proxies newline-delimited JSON-RPC stdio (stdin -> stdout) to a live daemon's
// Streamable HTTP `/mcp` endpoint so Claude Code, Cursor, Codex, and Hermes share one daemon seamlessly.
func RunStdioBridge(ctx context.Context, daemonURL, bearerToken string, in io.Reader, out io.Writer) error {
	endpoint := strings.TrimRight(daemonURL, "/") + "/mcp"
	hc := &http.Client{Timeout: 60 * time.Second}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for sc.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte(line)))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if bearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+bearerToken)
		}
		resp, err := hc.Do(req)
		if err != nil {
			// Return a valid JSON-RPC error frame instead of crashing stdio.
			_, _ = fmt.Fprintf(out, `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":%q}}`+"\n", err.Error())
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		ct := resp.Header.Get("Content-Type")
		resp.Body.Close()

		// Notifications (202 Accepted or empty body) produce no stdout frame.
		if resp.StatusCode == http.StatusAccepted || len(bytes.TrimSpace(body)) == 0 {
			continue
		}
		if strings.Contains(ct, "text/event-stream") {
			for _, ln := range strings.Split(string(body), "\n") {
				if strings.HasPrefix(ln, "data: ") {
					payload := strings.TrimSpace(strings.TrimPrefix(ln, "data: "))
					if payload != "" {
						_, _ = fmt.Fprintln(out, payload)
					}
				}
			}
			continue
		}
		_, _ = fmt.Fprintln(out, strings.TrimSpace(string(body)))
	}
	return sc.Err()
}
