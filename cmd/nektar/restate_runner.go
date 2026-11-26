package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/server"
)

const RestatePinnedVersion = "1.8.1"

// restatePinnedSHA256 pins the official SHA-256 checksums for restate-server v1.8.1
// so `nektar restate-up` never relies solely on a self-referential remote .sha256 file.
var restatePinnedSHA256 = map[string]string{
	"x86_64-unknown-linux-musl":  "56f12ccc54ee227657d4d8b070622849977444425184ee242a33e3ccc28bf745",
	"aarch64-unknown-linux-musl": "7d30d4c73a6978f7b4e4a2c08163fbf4e5689c4534af013bb8fd37e003474857",
	"aarch64-apple-darwin":       "e2ea3dce01140981fe4d15a09e53718681574d3a290f5566b1025b4a2696a3eb",
	"x86_64-apple-darwin":        "c6c079291b6171ff34358dd94b9315d5c374fc1e2ca96b62573caf8ac047e6c0",
}

// RestateRuntimeConfig controls how Nektar starts and connects to its active Restate durable executor.
type RestateRuntimeConfig struct {
	ListenAddr            string // dedicated h2c Restate service endpoint (default 127.0.0.1:9080 or :9080 in container)
	DataDir               string
	ExternalIngressURL    string // NEKTAR_RESTATE_URL
	ExternalAdminURL      string // NEKTAR_RESTATE_ADMIN
	EndpointURI           string // NEKTAR_RESTATE_ENDPOINT_URI
	IdentityPublicKey     string // NEKTAR_RESTATE_IDENTITY_KEY (publickeyv1_...)
	AllowInsecureEndpoint bool   // NEKTAR_RESTATE_ALLOW_INSECURE_ENDPOINT=1
	InternalToken         string // NEKTAR_RESTATE_INTERNAL_TOKEN
	DisableAutoStart      bool   // NEKTAR_RESTATE_DISABLE=1
	Required              bool   // NEKTAR_RESTATE_REQUIRED=1
}

// buildRestateEndpointURI constructs the HTTP URI Restate Admin uses to call our Go SDK endpoint,
// safely parsing host/port via net.SplitHostPort (fixes invalid string concatenation bugs).
func buildRestateEndpointURI(listenAddr, overrideURI string) (string, error) {
	if s := strings.TrimRight(strings.TrimSpace(overrideURI), "/"); s != "" {
		return s, nil
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil {
		return "", fmt.Errorf("invalid -restate-listen %q: %w", listenAddr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%s", host, port), nil
}

// StartRestatePlane starts Nektar's dedicated h2c Restate Go SDK service endpoint (with optional
// Request Identity v1 JWT verification), connects to an external `restate-server` OR auto-supervises
// a local loopback `restate-server` child process, and registers the deployment with exponential backoff.
// Fails closed if an external Restate server is configured (or Required=true) and registration fails!
func StartRestatePlane(ctx context.Context, eng *market.Engine, cfg RestateRuntimeConfig) (*market.RestateClient, func(), error) {
	noop := func() {}
	if eng == nil {
		return nil, noop, nil
	}
	// Always default to loopback 127.0.0.1:9080 even inside single-container deployments;
	// multi-container Compose stacks pass `-restate-listen :9080` explicitly.
	listenAddr := strings.TrimSpace(cfg.ListenAddr)
	if listenAddr == "" {
		listenAddr = "127.0.0.1:9080"
	}
	// In local auto-supervised mode (no external Restate & default loopback 9080), avoid port collisions
	// when running side-by-side Mainnet + Testnet nodes or when another local tool occupies :9080.
	if strings.TrimSpace(cfg.ExternalAdminURL) == "" && strings.TrimSpace(cfg.EndpointURI) == "" && listenAddr == "127.0.0.1:9080" {
		if p, pErr := pickFreeLoopbackPort(9080); pErr == nil {
			listenAddr = fmt.Sprintf("127.0.0.1:%d", p)
		}
	}
	// Fail-closed persistent internal Restate token: load or atomically create a mode-0600 32-byte
	// token in <dataDir>/restate-internal-token so suspended Restate workflows survive restarts
	// without hitting checkInternalAuthReq mismatches.
	internalTok := strings.TrimSpace(cfg.InternalToken)
	if internalTok == "" {
		if cfg.DataDir != "" && cfg.DataDir != ":memory:" {
			tok, _, tErr := server.ReadOrCreateTokenFile(filepath.Join(cfg.DataDir, "restate-internal-token"))
			if tErr != nil {
				return nil, noop, fmt.Errorf("restate: persist internal token: %w", tErr)
			}
			internalTok = tok
		} else {
			var tokRaw [32]byte
			if _, err := rand.Read(tokRaw[:]); err != nil {
				return nil, noop, fmt.Errorf("restate: generate internal token: %w", err)
			}
			internalTok = hex.EncodeToString(tokRaw[:])
			clear(tokRaw[:])
		}
		cfg.InternalToken = internalTok
	}
	eng.SetInternalToken(internalTok)

	// R1 fail-closed endpoint security guard: refuse non-loopback Restate service endpoints unless
	// Request Identity v1 JWT key is configured OR operator explicitly opted into private network mode.
	if server.IsNonLoopbackListen(listenAddr) && strings.TrimSpace(cfg.IdentityPublicKey) == "" && !cfg.AllowInsecureEndpoint {
		return nil, noop, fmt.Errorf(
			"restate: refusing non-loopback -restate-listen %q without NEKTAR_RESTATE_IDENTITY_KEY (bind 127.0.0.1:9080, set NEKTAR_RESTATE_IDENTITY_KEY=publickeyv1_..., or set NEKTAR_RESTATE_ALLOW_INSECURE_ENDPOINT=1 on a private bridge network)",
			listenAddr,
		)
	}
	// When Restate auto-start is explicitly disabled and no external Restate admin URL is configured,
	// skip starting an unused h2c listener after enforcing the non-loopback security guard above.
	if cfg.DisableAutoStart && strings.TrimSpace(cfg.ExternalAdminURL) == "" {
		return nil, noop, nil
	}

	// Pre-detect local restate-server binary when in auto-supervised mode so on Windows WSL2 NAT mode
	// we can bind the h2c listener directly on the private WSL2 host gateway interface (never LAN/0.0.0.0)
	// and register http://<wslHostIP>:<port> so WSL2 restate-server reaches Nektar without Connection refused!
	var localBinPath string
	if strings.TrimSpace(cfg.ExternalAdminURL) == "" && !cfg.DisableAutoStart {
		localBinPath = findRestateBinary()
	}

	epURI, err := buildRestateEndpointURI(listenAddr, cfg.EndpointURI)
	if err != nil {
		return nil, noop, err
	}

	dialAddr := ""
	if runtime.GOOS == "windows" && strings.HasPrefix(localBinPath, "wsl:") && strings.TrimSpace(cfg.EndpointURI) == "" && !server.IsNonLoopbackListen(listenAddr) {
		if wslHostIP := detectWSLHostIP(); wslHostIP != "" && isLocalHostIP(wslHostIP) {
			_, port, sErr := net.SplitHostPort(listenAddr)
			if sErr == nil && port != "" {
				listenAddr = net.JoinHostPort(wslHostIP, port)
				epURI = fmt.Sprintf("http://%s:%s", wslHostIP, port)
				dialAddr = listenAddr
				log.Printf("restate: detected WSL2 NAT bridge host IP %s — binding h2c endpoint on %s", wslHostIP, listenAddr)
			}
		}
	}

	var idKeys []string
	if k := strings.TrimSpace(cfg.IdentityPublicKey); k != "" {
		idKeys = append(idKeys, k)
	}
	rSrv := eng.NewRestateServer(idKeys...)

	// Start dedicated h2c Restate Go SDK endpoint.
	epErrCh := make(chan error, 1)
	go func() {
		log.Printf("nektar restate Go SDK h2c endpoint listening on %s (BuyerDeal, SellerDeal, StandingOrder, EscrowGuardian, Custody, Tx, AnchorCadence, NektarRuntime; auth=%v)",
			listenAddr, len(idKeys) > 0)
		if rErr := rSrv.Start(ctx, listenAddr); rErr != nil && ctx.Err() == nil {
			epErrCh <- rErr
			log.Printf("restate SDK endpoint stopped: %v", rErr)
		}
	}()

	// Wait until the local h2c listener is accepting TCP connections (ctx-aware, fail-closed on timeout).
	if dialAddr == "" {
		_, dialPort, _ := net.SplitHostPort(listenAddr)
		if dialPort == "" {
			dialPort = "9080"
		}
		dialAddr = "127.0.0.1:" + dialPort
	}
	readyDeadline := time.Now().Add(4 * time.Second)
	epReady := false
	for time.Now().Before(readyDeadline) {
		select {
		case <-ctx.Done():
			return nil, noop, ctx.Err()
		case rErr := <-epErrCh:
			return nil, noop, fmt.Errorf("restate SDK endpoint failed to start on %s: %w", listenAddr, rErr)
		default:
		}
		if c, dErr := net.DialTimeout("tcp", dialAddr, 200*time.Millisecond); dErr == nil {
			_ = c.Close()
			epReady = true
			break
		}
		select {
		case <-ctx.Done():
			return nil, noop, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !epReady {
		return nil, noop, fmt.Errorf("restate SDK endpoint on %s did not become ready within 4s", listenAddr)
	}

	// Mode 1: External restate-server (Docker Compose / Kubernetes sidecar / standalone daemon).
	adminURL := strings.TrimSpace(cfg.ExternalAdminURL)
	ingressURL := strings.TrimSpace(cfg.ExternalIngressURL)
	if adminURL != "" {
		if ingressURL == "" {
			ingressURL = deriveIngressURL(adminURL)
		}
		rc := market.NewRestateClientWithToken(ingressURL, adminURL, cfg.InternalToken)
		if err := waitAndRegisterRestate(ctx, rc, epURI, 30*time.Second); err != nil {
			return nil, noop, fmt.Errorf("restate external registration failed (%s -> %s): %w", adminURL, epURI, err)
		}
		return rc, noop, nil
	}

	// Mode 2: Auto-supervised local loopback restate-server child process!
	if cfg.DisableAutoStart {
		return nil, noop, nil
	}
	binPath := localBinPath
	if binPath == "" {
		binPath = findRestateBinary()
	}
	if binPath == "" {
		msg := "restate-server binary not found locally (run `nektar restate-up` to install pinned v" + RestatePinnedVersion + ", or set NEKTAR_RESTATE_ADMIN)"
		if cfg.Required {
			return nil, noop, errors.New(msg)
		}
		log.Printf("warning: %s — running without active Restate server", msg)
		return nil, noop, nil
	}

	ingPort, err := pickFreeLoopbackPort(8111)
	if err != nil {
		return nil, noop, err
	}
	admPort, err := pickFreeLoopbackPort(9070)
	if err != nil {
		return nil, noop, err
	}
	nodePort, err := pickFreeLoopbackPort(5122)
	if err != nil {
		return nil, noop, err
	}
	restateDir := filepath.Join(cfg.DataDir, "restate-data")
	if err := os.MkdirAll(restateDir, 0o750); err != nil {
		return nil, noop, err
	}
	// Reclaim any orphaned restate-server child from a previous unclean SIGKILL so RocksDB lock never blocks reboot.
	pidPath := filepath.Join(cfg.DataDir, "restate-server.pid")
	if rawPID, pErr := os.ReadFile(pidPath); pErr == nil {
		if oldPID, nErr := strconv.Atoi(strings.TrimSpace(string(rawPID))); nErr == nil && oldPID > 0 && oldPID != os.Getpid() && isPIDAlive(oldPID) {
			log.Printf("reclaiming orphaned restate-server pid=%d before startup", oldPID)
			terminatePIDGracefully(oldPID)
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) && isPIDAlive(oldPID) {
				time.Sleep(50 * time.Millisecond)
			}
			if isPIDAlive(oldPID) {
				if p, fErr := os.FindProcess(oldPID); fErr == nil {
					_ = p.Kill()
				}
			}
		}
		_ = os.Remove(pidPath)
	}

	// Decouple child lifecycle from parent ctx so SIGTERM does not immediately SIGKILL restate-server
	// before httpSrv.Shutdown drains in-flight requests; stopFn performs graceful SIGTERM + bounded WaitDelay.
	cmdCtx, cmdCancel := context.WithCancel(context.Background())
	var cmd *exec.Cmd
	if strings.HasPrefix(binPath, "wsl:") {
		wslBin := strings.TrimPrefix(binPath, "wsl:")
		dirSum := sha256.Sum256([]byte(cfg.DataDir))
		wslDataDir := fmt.Sprintf("$HOME/.nektar/restate-data/%s", hex.EncodeToString(dirSum[:8]))
		shCmd := fmt.Sprintf(
			`mkdir -p %s && exec env RESTATE_NODE_NAME=nektar-local RESTATE_BIND_ADDRESS=127.0.0.1:%d RESTATE_INGRESS__BIND_ADDRESS=127.0.0.1:%d RESTATE_ADMIN__BIND_ADDRESS=127.0.0.1:%d RUST_LOG=warn %q --base-dir %s`,
			wslDataDir, nodePort, ingPort, admPort, wslBin, wslDataDir,
		)
		cmd = exec.CommandContext(cmdCtx, "wsl", "-e", "sh", "-c", shCmd)
	} else {
		cmd = exec.CommandContext(cmdCtx, binPath, "--base-dir", restateDir)
		cmd.Env = append(os.Environ(),
			"RESTATE_NODE_NAME=nektar-local",
			fmt.Sprintf("RESTATE_BIND_ADDRESS=127.0.0.1:%d", nodePort),
			fmt.Sprintf("RESTATE_INGRESS__BIND_ADDRESS=127.0.0.1:%d", ingPort),
			fmt.Sprintf("RESTATE_ADMIN__BIND_ADDRESS=127.0.0.1:%d", admPort),
			"RUST_LOG=warn",
		)
	}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			terminatePIDGracefully(cmd.Process.Pid)
		}
		return nil
	}
	cmd.WaitDelay = 3 * time.Second
	logFile, lErr := os.OpenFile(filepath.Join(cfg.DataDir, "restate-server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if lErr == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	if err := cmd.Start(); err != nil {
		cmdCancel()
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, noop, fmt.Errorf("start local restate-server (%s): %w", binPath, err)
	}
	if cmd.Process != nil {
		_ = os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600)
	}
	childDone := make(chan error, 1)
	go func() {
		wErr := cmd.Wait()
		_ = os.Remove(pidPath)
		childDone <- wErr
		close(childDone)
	}()
	stopFn := func() {
		cmdCancel()
		select {
		case <-childDone:
		case <-time.After(4 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-childDone
		}
		if logFile != nil {
			_ = logFile.Close()
		}
	}

	localIngress := fmt.Sprintf("http://127.0.0.1:%d", ingPort)
	localAdmin := fmt.Sprintf("http://127.0.0.1:%d", admPort)
	rc := market.NewRestateClientWithToken(localIngress, localAdmin, cfg.InternalToken)
	regCtx, regCancel := context.WithCancel(ctx)
	defer regCancel()
	go func() {
		select {
		case wErr, ok := <-childDone:
			if ok && regCtx.Err() == nil {
				log.Printf("warning: supervised restate-server exited unexpectedly: %v", wErr)
				regCancel()
			}
		case <-regCtx.Done():
		}
	}()
	if err := waitAndRegisterRestate(regCtx, rc, epURI, 25*time.Second); err != nil {
		stopFn()
		return nil, noop, fmt.Errorf("local restate-server registration failed (%s -> %s): %w", localAdmin, epURI, err)
	}
	log.Printf("restate-server v%s auto-supervised on loopback (ingress=%s admin=%s endpoint=%s)", RestatePinnedVersion, localIngress, localAdmin, epURI)
	return rc, stopFn, nil
}

func waitAndRegisterRestate(ctx context.Context, rc *market.RestateClient, endpointURI string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	backoff := 200 * time.Millisecond
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if hErr := rc.HealthCheck(ctx); hErr != nil {
			lastErr = hErr
		} else {
			changed, rErr := rc.EnsureDeployment(ctx, endpointURI, false)
			if rErr == nil {
				if changed {
					log.Printf("restate deployment registered (%s -> %s)", rc.AdminURL, endpointURI)
				} else {
					log.Printf("restate deployment verified up-to-date (%s -> %s)", rc.AdminURL, endpointURI)
				}
				return nil
			}
			lastErr = rErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
	if lastErr == nil {
		lastErr = errors.New("timed out waiting for restate-server")
	}
	return lastErr
}

func deriveIngressURL(adminURL string) string {
	// Common topology: http://restate:9070 -> http://restate:8080; http://127.0.0.1:9070 -> http://127.0.0.1:8111
	if strings.Contains(adminURL, "127.0.0.1:9070") || strings.Contains(adminURL, "localhost:9070") {
		return strings.Replace(adminURL, ":9070", ":8080", 1)
	}
	return strings.Replace(adminURL, ":9070", ":8080", 1)
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

func pickFreeLoopbackPort(preferred int) (int, error) {
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", preferred)); err == nil {
		_ = ln.Close()
		return preferred, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
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

func envInt64(key string, fallback int64) int64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
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
