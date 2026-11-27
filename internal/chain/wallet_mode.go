package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

const walletModeMarkerFile = "wallet-mode.json"

type walletModeMarker struct {
	Mode    string `json:"mode"`              // "client" | "embedded"
	WireURL string `json:"wireUrl,omitempty"` // recorded when Mode == "client"
	Network string `json:"network,omitempty"`
}

// resolveWalletMode turns requestedMode ("", "auto", "client", "embedded") into a concrete
// custody mode ("client" or "embedded") with sticky per-data-dir guarantees so a node never
// silently flips identity keys across restarts.
//
// Rules:
//   - "memory" / "regtest": always "embedded" (refuses explicit "client").
//   - Explicit "client" or "embedded": honored strictly.
//   - Empty (""): defaults to "embedded" so Go library callers & unit tests stay hermetic.
//   - "auto" (CLI default on testnet/main):
//     1. If <dir>/wallet-mode.json already exists, stick to its recorded mode (fail closed if
//     pinned to "client" and BSV Desktop is currently closed — never silently create a local wallet).
//     2. If legacy embedded wallet files already exist in <dir>/wallet/, stick to "embedded".
//     3. On a fresh data dir, fast-probe BSV Desktop on wireURL (<= 250ms when offline); if running,
//     unlocked, and on the matching network, choose "client" (zero local wallet); else "embedded".
func resolveWalletMode(ctx context.Context, dir, netName, requestedMode, wireURL string) (mode string, stickyFromDisk bool, err error) {
	req := strings.ToLower(strings.TrimSpace(requestedMode))
	if req == "" {
		req = "embedded"
	}
	if netName == "memory" || netName == "regtest" {
		if req == "client" {
			return "", false, errors.New("chain: client wallet custody is for testnet/main only")
		}
		return "embedded", false, nil
	}
	switch req {
	case "embedded":
		return "embedded", false, nil
	case "client":
		return "client", false, nil
	case "auto":
		// 1. Honor sticky marker from previous initialization.
		if m, ok := readWalletModeMarker(dir); ok {
			if m.Mode == "client" || m.Mode == "embedded" {
				return m.Mode, true, nil
			}
		}
		// 2. Preserve legacy embedded data directories (never switch an existing local wallet).
		if hasLegacyEmbeddedWallet(dir) {
			return "embedded", true, nil
		}
		// 3. Fresh data dir: auto-prefer BSV Desktop when listening on matching network.
		wantNet := string(sdkwallet.NetworkTestnet)
		if netName == "main" {
			wantNet = string(sdkwallet.NetworkMainnet)
		}
		probe := nwallet.ProbeDesktopWallet(ctx, wireURL)
		if probe.Available && probe.Network == wantNet && probe.IdentityPub != "" {
			return "client", false, nil
		}
		return "embedded", false, nil
	default:
		return "", false, fmt.Errorf("chain: unknown wallet mode %q (want auto, client, or embedded)", requestedMode)
	}
}

func readWalletModeMarker(dir string) (walletModeMarker, bool) {
	if strings.TrimSpace(dir) == "" {
		return walletModeMarker{}, false
	}
	raw, err := os.ReadFile(filepath.Join(dir, walletModeMarkerFile))
	if err != nil {
		return walletModeMarker{}, false
	}
	var m walletModeMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		return walletModeMarker{}, false
	}
	m.Mode = strings.ToLower(strings.TrimSpace(m.Mode))
	if m.Mode == "" {
		return walletModeMarker{}, false
	}
	return m, true
}

func persistWalletModeMarker(dir, mode, wireURL, netName string) {
	if strings.TrimSpace(dir) == "" || netName == "memory" {
		return
	}
	m := walletModeMarker{
		Mode:    mode,
		Network: netName,
	}
	if mode == "client" {
		m.WireURL = wireURL
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	raw = append(raw, '\n')
	_ = os.MkdirAll(dir, 0o700)
	target := filepath.Join(dir, walletModeMarkerFile)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, target)
}

func hasLegacyEmbeddedWallet(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	wDir := filepath.Join(dir, "wallet")
	for _, name := range []string{"wallet.sqlite", "identity.json"} {
		if st, err := os.Stat(filepath.Join(wDir, name)); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}
