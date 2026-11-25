package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// IsContainerEnv reports whether Nektar is running inside a Docker/OCI container.
func IsContainerEnv() bool {
	if os.Getenv("NEKTAR_CONTAINER") == "1" {
		return true
	}
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// IsNonLoopbackListen reports whether listenAddr exposes Nektar beyond 127.0.0.1 / ::1.
func IsNonLoopbackListen(listenAddr string) bool {
	a := strings.TrimSpace(strings.ToLower(listenAddr))
	if a == "" {
		return false
	}
	if strings.HasPrefix(a, ":") || strings.HasPrefix(a, "0.0.0.0:") || strings.HasPrefix(a, "[::]:") {
		return true
	}
	return !strings.HasPrefix(a, "127.0.0.1:") && !strings.HasPrefix(a, "localhost:") && !strings.HasPrefix(a, "[::1]:")
}

// ResolveOrGenerateTokens guarantees persistent mode-0600 http-token and operator-token
// whenever Nektar runs in a container or binds beyond loopback without explicit env tokens.
// Explicit env tokens always win.
func ResolveOrGenerateTokens(dataDir, apiTok, opTok string, requireAuth bool) (httpToken, operatorToken string, generated bool) {
	httpToken = strings.TrimSpace(apiTok)
	operatorToken = strings.TrimSpace(opTok)
	if !requireAuth || dataDir == "" || dataDir == ":memory:" {
		return httpToken, operatorToken, false
	}
	_ = os.MkdirAll(dataDir, 0o700)
	if httpToken == "" {
		tok, created := readOrCreateTokenFile(filepath.Join(dataDir, "http-token"))
		httpToken = tok
		if created {
			generated = true
		}
	}
	if operatorToken == "" {
		tok, created := readOrCreateTokenFile(filepath.Join(dataDir, "operator-token"))
		operatorToken = tok
		if created {
			generated = true
		}
	}
	return httpToken, operatorToken, generated
}

func readOrCreateTokenFile(path string) (string, bool) {
	if raw, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(raw)); len(s) >= 16 {
			_ = os.Chmod(path, 0o600)
			return s, false
		}
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	tok := hex.EncodeToString(b[:])
	clear(b[:])
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err != nil {
		_ = os.Remove(tmp)
		return tok, true
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
	return tok, true
}

// StartupBannerOpts configures the human-readable Startup Card printed on stderr at boot.
type StartupBannerOpts struct {
	Version         string
	URL             string
	Network         string
	BootDuration    time.Duration
	PairCode        string
	FeeAddress      string
	BalanceSats     uint64
	WithHub         bool
	ContainerMode   bool
	GeneratedTokens bool
}

// FormatStartupBanner renders the crisp zero-jargon Startup Card for `nektar serve`.
func FormatStartupBanner(opts StartupBannerOpts) string {
	base := strings.TrimRight(opts.URL, "/")
	dashURL := base
	if opts.PairCode != "" {
		dashURL = base + "/pair?code=" + opts.PairCode
	}
	sec := fmt.Sprintf("%.1fs", opts.BootDuration.Seconds())
	mode := opts.Network
	if opts.WithHub {
		mode += " · hub on"
	}
	var b strings.Builder
	b.WriteString("\n")
	fmt.Fprintf(&b, "╭─ nektar v%s ready (%s · %s)\n", opts.Version, mode, sec)
	fmt.Fprintf(&b, "│ Dashboard : %s\n", dashURL)
	fmt.Fprintf(&b, "│ MCP (HTTP): %s/mcp\n", base)
	fmt.Fprintf(&b, "│ LLM Proxy : %s/v1/proxy/openai/v1  (any OpenAI/Anthropic agent)\n", base)
	if opts.FeeAddress != "" {
		hint := ""
		if opts.BalanceSats == 0 && opts.Network == "testnet" {
			hint = " — 1-click faucet in Dashboard"
		}
		fmt.Fprintf(&b, "│ Fee Key   : %s (%d sats)%s\n", opts.FeeAddress, opts.BalanceSats, hint)
	}
	if opts.GeneratedTokens {
		b.WriteString("│ Auth      : auto-generated tokens saved in data dir (use pairing link above)\n")
	}
	if opts.ContainerMode {
		fmt.Fprintf(&b, "│ Dock Apps : nektar dock   (or OPENAI_BASE_URL=%s/v1/proxy/openai/v1)\n", base)
	} else {
		b.WriteString("│ Dock Apps : nektar dock   (auto-wires Claude, Cursor, Hermes, OpenClaw, Codex…)\n")
	}
	b.WriteString("╰─\n")
	return b.String()
}
