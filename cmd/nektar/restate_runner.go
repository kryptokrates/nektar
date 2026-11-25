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
	// Fail-closed internal Restate token: auto-generate a 32-byte random token if none was provided
	// so NektarRuntime / BuyerDeal / SellerDeal ingress handlers are NEVER unauthenticated.
	internalTok := strings.TrimSpace(cfg.InternalToken)
	if internalTok == "" {
		var tokRaw [32]byte
		if _, err := rand.Read(tokRaw[:]); err != nil {
			return nil, noop, fmt.Errorf("restate: generate internal token: %w", err)
		}
		internalTok = hex.EncodeToString(tokRaw[:])
		clear(tokRaw[:])
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

	epURI, err := buildRestateEndpointURI(listenAddr, cfg.EndpointURI)
	if err != nil {
		return nil, noop, err
	}

	var idKeys []string
	if k := strings.TrimSpace(cfg.IdentityPublicKey); k != "" {
		idKeys = append(idKeys, k)
	}
	rSrv := eng.NewRestateServer(idKeys...)

	// Start dedicated h2c Restate Go SDK endpoint.
	epErrCh := make(chan error, 1)
	go func() {
		log.Printf("nektar restate Go SDK h2c endpoint listening on %s (BuyerDeal, SellerDeal, Custody, Tx, AnchorCadence, NektarRuntime; auth=%v)",
			listenAddr, len(idKeys) > 0)
		if rErr := rSrv.Start(ctx, listenAddr); rErr != nil && ctx.Err() == nil {
			epErrCh <- rErr
			log.Printf("restate SDK endpoint stopped: %v", rErr)
		}
	}()

	// Wait briefly until the local h2c listener is accepting TCP connections.
	_, dialPort, _ := net.SplitHostPort(listenAddr)
	if dialPort == "" {
		dialPort = "9080"
	}
	readyDeadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(readyDeadline) {
		select {
		case rErr := <-epErrCh:
			return nil, noop, fmt.Errorf("restate SDK endpoint failed to start on %s: %w", listenAddr, rErr)
		default:
		}
		if c, dErr := net.DialTimeout("tcp", "127.0.0.1:"+dialPort, 200*time.Millisecond); dErr == nil {
			_ = c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
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
	binPath := findRestateBinary()
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

	cmdCtx, cmdCancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cmdCtx, binPath, "--base-dir", restateDir)
	cmd.Env = append(os.Environ(),
		"RESTATE_NODE_NAME=nektar-local",
		fmt.Sprintf("RESTATE_BIND_ADDRESS=127.0.0.1:%d", nodePort),
		fmt.Sprintf("RESTATE_INGRESS__BIND_ADDRESS=127.0.0.1:%d", ingPort),
		fmt.Sprintf("RESTATE_ADMIN__BIND_ADDRESS=127.0.0.1:%d", admPort),
		"RUST_LOG=warn",
	)
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
	stopFn := func() {
		cmdCancel()
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
		if logFile != nil {
			_ = logFile.Close()
		}
	}

	localIngress := fmt.Sprintf("http://127.0.0.1:%d", ingPort)
	localAdmin := fmt.Sprintf("http://127.0.0.1:%d", admPort)
	rc := market.NewRestateClientWithToken(localIngress, localAdmin, cfg.InternalToken)
	if err := waitAndRegisterRestate(ctx, rc, epURI, 25*time.Second); err != nil {
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

// cmdRestateUp downloads & SHA-256 verifies the pinned restate-server v1.8.1 binary into ~/.nektar/bin/
// so native bare-metal users get active Restate durable execution in one command without Docker.
func cmdRestateUp(args []string) error {
	fs := flag.NewFlagSet("restate-up", flag.ExitOnError)
	ver := fs.String("version", RestatePinnedVersion, "Pinned Restate server version")
	shaOverride := fs.String("sha256", "", "Expected SHA-256 hex digest (required when overriding -version)")
	destDir := fs.String("dir", "", "Install directory (default ~/.nektar/bin)")
	_ = fs.Parse(args)

	cleanVer := strings.TrimPrefix(strings.TrimSpace(*ver), "v")
	for _, c := range cleanVer {
		if !((c >= '0' && c <= '9') || c == '.' || c == '-' || (c >= 'a' && c <= 'z')) {
			return fmt.Errorf("invalid -version %q", *ver)
		}
	}

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
	default:
		return fmt.Errorf("automatic restate-server download is not published for %s/%s — use Docker (`docker compose -f deploy/agent/compose.yaml up -d`) or install restate-server manually and set NEKTAR_RESTATE_BIN", runtime.GOOS, runtime.GOARCH)
	}

	wantDigest := strings.ToLower(strings.TrimSpace(*shaOverride))
	if wantDigest == "" {
		if cleanVer != RestatePinnedVersion {
			return fmt.Errorf("refusing unpinned restate-server version %q without explicit -sha256 (pinned version is %s)", cleanVer, RestatePinnedVersion)
		}
		wantDigest = restatePinnedSHA256[target]
	}
	if len(wantDigest) != 64 {
		return fmt.Errorf("invalid expected sha256 digest %q", wantDigest)
	}

	dir := strings.TrimSpace(*destDir)
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".nektar", "bin")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	archiveName := fmt.Sprintf("restate-server-%s.tar.xz", target)
	baseURL := fmt.Sprintf("https://github.com/restatedev/restate/releases/download/v%s/", cleanVer)
	hc := &http.Client{Timeout: 3 * time.Minute}

	fmt.Printf("Downloading restate-server v%s (%s)...\n", cleanVer, target)
	resp, err := hc.Get(baseURL + archiveName)
	if err != nil {
		return fmt.Errorf("download archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download archive HTTP %d", resp.StatusCode)
	}
	tmpArchive, err := os.CreateTemp(dir, "restate-*.tar.xz")
	if err != nil {
		return err
	}
	tmpPath := tmpArchive.Name()
	defer os.Remove(tmpPath)

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmpArchive, h), resp.Body); err != nil {
		_ = tmpArchive.Close()
		return err
	}
	_ = tmpArchive.Close()

	gotSum := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(gotSum, wantDigest) {
		return fmt.Errorf("SHA-256 mismatch for %s: got %s, want pinned %s", archiveName, gotSum, wantDigest)
	}

	// Extract restate-server binary from verified .tar.xz archive into a temp dir, then verify regular file before rename.
	extractDir, err := os.MkdirTemp(dir, "restate-extract-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(extractDir)
	tarArgs := []string{"-xJf", tmpPath, "-C", extractDir}
	if runtime.GOOS == "linux" {
		tarArgs = []string{"--no-same-owner", "--no-same-permissions", "-xJf", tmpPath, "-C", extractDir}
	}
	tarCmd := exec.Command("tar", tarArgs...)
	if out, err := tarCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("extract %s (requires tar/xz): %w: %s", archiveName, err, strings.TrimSpace(string(out)))
	}
	var foundBin string
	_ = filepath.WalkDir(extractDir, func(p string, d os.DirEntry, wErr error) error {
		if wErr == nil && d.Type().IsRegular() && d.Name() == "restate-server" {
			foundBin = p
		}
		return nil
	})
	if foundBin == "" {
		return errors.New("restate-server regular binary not found inside release archive")
	}
	fi, err := os.Lstat(foundBin)
	if err != nil || !fi.Mode().IsRegular() {
		return errors.New("refusing non-regular restate-server file (symlink blocked)")
	}
	dstPath := filepath.Join(dir, "restate-server")
	if err := os.Chmod(foundBin, 0o755); err != nil {
		return err
	}
	if err := os.Rename(foundBin, dstPath); err != nil {
		return err
	}
	fmt.Printf("Installed pinned SHA-256 verified restate-server v%s -> %s\n", cleanVer, dstPath)
	fmt.Println("Next: run `nektar serve` — it will automatically supervise restate-server on loopback!")
	return nil
}

func envInt64(key string, fallback int64) int64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}
