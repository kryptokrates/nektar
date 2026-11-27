package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// restatePinnedSHA256 pins the official SHA-256 checksums for restate-server v1.8.1
// so `nektar restate-up` never relies solely on a self-referential remote .sha256 file.
var restatePinnedSHA256 = map[string]string{
	"x86_64-unknown-linux-musl":  "56f12ccc54ee227657d4d8b070622849977444425184ee242a33e3ccc28bf745",
	"aarch64-unknown-linux-musl": "7d30d4c73a6978f7b4e4a2c08163fbf4e5689c4534af013bb8fd37e003474857",
	"aarch64-apple-darwin":       "e2ea3dce01140981fe4d15a09e53718681574d3a290f5566b1025b4a2696a3eb",
	"x86_64-apple-darwin":        "c6c079291b6171ff34358dd94b9315d5c374fc1e2ca96b62573caf8ac047e6c0",
}

func findRestateBinary() string {
	if envBin := strings.TrimSpace(os.Getenv("NEKTAR_RESTATE_BIN")); envBin != "" {
		if info, err := os.Stat(envBin); err == nil && !info.IsDir() {
			return envBin
		}
	}
	exeSuffix := ""
	if runtime.GOOS == "windows" {
		exeSuffix = ".exe"
	}
	home, _ := os.UserHomeDir()
	cands := []string{
		"/usr/local/bin/restate-server" + exeSuffix,
		filepath.Join(home, ".nektar", "bin", "restate-server"+exeSuffix),
	}
	for _, c := range cands {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	if p, err := exec.LookPath("restate-server"); err == nil {
		return p
	}
	// On Windows, transparently detect pinned Linux restate-server installed inside WSL2
	// (WSL2 shares 127.0.0.1 loopback with Windows host).
	if runtime.GOOS == "windows" && os.Getenv("NEKTAR_RESTATE_NO_WSL") != "1" {
		if _, err := exec.LookPath("wsl"); err == nil {
			pCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			out, wErr := exec.CommandContext(pCtx, "wsl", "-e", "sh", "-c", `test -x "$HOME/.nektar/bin/restate-server" && printf "%s/.nektar/bin/restate-server" "$HOME"`).Output()
			cancel()
			if wErr == nil {
				if p := strings.TrimSpace(string(out)); strings.HasPrefix(p, "/") {
					return "wsl:" + p
				}
			}
		}
	}
	return ""
}

// ensureRestateInstalled returns an existing restate-server path or installs pinned v1.8.1
// (native on macOS/Linux, or inside WSL2 on Windows). Safe to call non-fatally during `nektar up`.
func ensureRestateInstalled(verbose bool) (string, error) {
	if bin := findRestateBinary(); bin != "" {
		return bin, nil
	}
	return installRestatePinned(RestatePinnedVersion, "", "", verbose)
}

func cmdRestateUp(args []string) error {
	fs := flag.NewFlagSet("restate-up", flag.ExitOnError)
	ver := fs.String("version", RestatePinnedVersion, "Pinned Restate server version")
	shaOverride := fs.String("sha256", "", "Expected SHA-256 hex digest (required when overriding -version)")
	destDir := fs.String("dir", "", "Install directory (default ~/.nektar/bin)")
	_ = fs.Parse(args)

	installed, err := installRestatePinned(*ver, *shaOverride, *destDir, true)
	if err != nil {
		return err
	}
	fmt.Printf("Installed pinned SHA-256 verified restate-server v%s -> %s\n", strings.TrimPrefix(strings.TrimSpace(*ver), "v"), installed)
	fmt.Println("Next: run `nektar up` (or `nektar serve`) — it will automatically supervise restate-server on loopback!")
	return nil
}

func installRestatePinned(ver, shaOverride, destDir string, verbose bool) (string, error) {
	cleanVer := strings.TrimPrefix(strings.TrimSpace(ver), "v")
	for _, c := range cleanVer {
		if !((c >= '0' && c <= '9') || c == '.' || c == '-' || (c >= 'a' && c <= 'z')) {
			return "", fmt.Errorf("invalid -version %q", ver)
		}
	}

	useWSL := false
	target := ""
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		target = "x86_64-unknown-linux-musl"
	case "linux/arm64":
		target = "aarch64-unknown-linux-musl"
	case "darwin/arm64":
		target = "aarch64-apple-darwin"
	case "darwin/amd64":
		target = "x86_64-apple-darwin"
	case "windows/amd64", "windows/arm64":
		if os.Getenv("NEKTAR_RESTATE_NO_WSL") != "1" {
			if _, err := exec.LookPath("wsl"); err == nil {
				pCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				archOut, wErr := exec.CommandContext(pCtx, "wsl", "-e", "uname", "-m").Output()
				cancel()
				if wErr == nil {
					switch strings.TrimSpace(string(archOut)) {
					case "x86_64", "amd64":
						target = "x86_64-unknown-linux-musl"
						useWSL = true
					case "aarch64", "arm64":
						target = "aarch64-unknown-linux-musl"
						useWSL = true
					}
				}
			}
		}
		if !useWSL {
			return "", fmt.Errorf("restate-server has no native Windows binary and WSL2 was not detected — install WSL2 (`wsl --install`) or use Docker (`docker compose -f deploy/agent/compose.yaml up -d`) for live BSV trades (local memory, recall & receipts work without Restate)")
		}
	default:
		return "", fmt.Errorf("automatic restate-server download is not published for %s/%s — use Docker (`docker compose -f deploy/agent/compose.yaml up -d`) or set NEKTAR_RESTATE_BIN", runtime.GOOS, runtime.GOARCH)
	}

	wantDigest := strings.ToLower(strings.TrimSpace(shaOverride))
	if wantDigest == "" {
		if cleanVer != RestatePinnedVersion {
			return "", fmt.Errorf("refusing unpinned restate-server version %q without explicit -sha256 (pinned version is %s)", cleanVer, RestatePinnedVersion)
		}
		wantDigest = restatePinnedSHA256[target]
	}
	if len(wantDigest) != 64 {
		return "", fmt.Errorf("invalid expected sha256 digest %q", wantDigest)
	}

	dir := strings.TrimSpace(destDir)
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".nektar", "bin")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	archiveName := fmt.Sprintf("restate-server-%s.tar.xz", target)
	baseURL := fmt.Sprintf("https://github.com/restatedev/restate/releases/download/v%s/", cleanVer)
	hc := &http.Client{Timeout: 3 * time.Minute}

	if verbose {
		fmt.Printf("Downloading restate-server v%s (%s)...\n", cleanVer, target)
	}
	resp, err := hc.Get(baseURL + archiveName)
	if err != nil {
		return "", fmt.Errorf("download archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download archive HTTP %d", resp.StatusCode)
	}
	tmpArchive, err := os.CreateTemp(dir, "restate-*.tar.xz")
	if err != nil {
		return "", err
	}
	tmpPath := tmpArchive.Name()
	defer os.Remove(tmpPath)

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmpArchive, h), resp.Body); err != nil {
		_ = tmpArchive.Close()
		return "", err
	}
	_ = tmpArchive.Close()

	gotSum := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(gotSum, wantDigest) {
		return "", fmt.Errorf("SHA-256 mismatch for %s: got %s, want pinned %s", archiveName, gotSum, wantDigest)
	}

	if useWSL {
		f, err := os.Open(tmpPath)
		if err != nil {
			return "", err
		}
		defer f.Close()
		extractCmd := exec.Command("wsl", "-e", "sh", "-c",
			`set -eu; mkdir -p "$HOME/.nektar/bin"; tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT; tar --no-same-owner --no-same-permissions -xJf - -C "$tmp"; bin="$(find "$tmp" -type f -name restate-server | head -n 1)"; test -n "$bin"; chmod 755 "$bin"; mv "$bin" "$HOME/.nektar/bin/restate-server"; printf "%s/.nektar/bin/restate-server" "$HOME"`)
		extractCmd.Stdin = f
		out, err := extractCmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("extract restate-server into WSL2: %w: %s", err, strings.TrimSpace(string(out)))
		}
		wslBin := strings.TrimSpace(string(out))
		if !strings.HasPrefix(wslBin, "/") {
			return "", fmt.Errorf("unexpected WSL2 install output: %q", wslBin)
		}
		return "wsl:" + wslBin, nil
	}

	// Extract restate-server binary from verified .tar.xz archive into a temp dir, then verify regular file before rename.
	extractDir, err := os.MkdirTemp(dir, "restate-extract-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(extractDir)
	tarArgs := []string{"-xJf", tmpPath, "-C", extractDir}
	if runtime.GOOS == "linux" {
		tarArgs = []string{"--no-same-owner", "--no-same-permissions", "-xJf", tmpPath, "-C", extractDir}
	}
	tarCmd := exec.Command("tar", tarArgs...)
	if out, err := tarCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("extract %s (requires tar/xz): %w: %s", archiveName, err, strings.TrimSpace(string(out)))
	}
	var foundBin string
	_ = filepath.WalkDir(extractDir, func(p string, d os.DirEntry, wErr error) error {
		if wErr == nil && d.Type().IsRegular() && d.Name() == "restate-server" {
			foundBin = p
		}
		return nil
	})
	if foundBin == "" {
		return "", errors.New("restate-server regular binary not found inside release archive")
	}
	fi, err := os.Lstat(foundBin)
	if err != nil || !fi.Mode().IsRegular() {
		return "", errors.New("refusing non-regular restate-server file (symlink blocked)")
	}
	dstPath := filepath.Join(dir, "restate-server")
	if err := os.Chmod(foundBin, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(foundBin, dstPath); err != nil {
		return "", err
	}
	return dstPath, nil
}

// detectWSLHostIP queries WSL2's default IPv4 gateway IP, which in WSL2 NAT mode is the Windows host's
// private vEthernet (WSL) adapter address (used so WSL2 restate-server can dial Nektar's h2c endpoint).
func detectWSLHostIP() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "wsl", "-e", "sh", "-c", `ip route show default 2>/dev/null | awk '{print $3; exit}'`).Output()
	if err != nil {
		return ""
	}
	ip := net.ParseIP(strings.TrimSpace(string(out)))
	if ip == nil || ip.To4() == nil || ip.IsLoopback() {
		return ""
	}
	return ip.String()
}

// isLocalHostIP reports whether ipStr is explicitly assigned to a local network interface on this host.
// In WSL2 NAT mode, the gateway IP belongs to the local Hyper-V vEthernet (WSL) interface (returns true).
// In WSL2 mirrored mode, the default gateway is the physical LAN router (returns false, keeping 127.0.0.1).
func isLocalHostIP(ipStr string) bool {
	target := net.ParseIP(strings.TrimSpace(ipStr))
	if target == nil {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip != nil && ip.Equal(target) {
			return true
		}
	}
	return false
}
