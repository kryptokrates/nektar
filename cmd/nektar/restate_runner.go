package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/server"
)

const RestatePinnedVersion = "1.8.1"

// RestateRuntimeConfig controls how Nektar starts and connects to its active Restate durable executor.
type RestateRuntimeConfig struct {
	ListenAddr            string // dedicated h2c Restate service endpoint (default 127.0.0.1:9080 or :9080 in container)
	DataDir               string
	ExternalIngressURL    string      // NEKTAR_RESTATE_URL
	ExternalAdminURL      string      // NEKTAR_RESTATE_ADMIN
	EndpointURI           string      // NEKTAR_RESTATE_ENDPOINT_URI
	IdentityPublicKey     string      // NEKTAR_RESTATE_IDENTITY_KEY (publickeyv1_...)
	AllowInsecureEndpoint bool        // NEKTAR_RESTATE_ALLOW_INSECURE_ENDPOINT=1
	InternalToken         string      // NEKTAR_RESTATE_INTERNAL_TOKEN
	DisableAutoStart      bool        // NEKTAR_RESTATE_DISABLE=1
	Required              bool        // NEKTAR_RESTATE_REQUIRED=1
	OnUnexpectedExit      func(error) // optional callback if auto-supervised restate-server exits unexpectedly post-boot
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
	autoPickPort := false
	if listenAddr == "" {
		listenAddr = "127.0.0.1:9080"
		autoPickPort = true
	} else if listenAddr == "127.0.0.1:9080" {
		autoPickPort = true
	}
	// In local auto-supervised mode (no external Restate & default loopback 9080), avoid port collisions
	// when running side-by-side Mainnet + Testnet nodes or when another local tool occupies :9080.
	if strings.TrimSpace(cfg.ExternalAdminURL) == "" && strings.TrimSpace(cfg.EndpointURI) == "" && autoPickPort {
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
				// Probe the actual WSL2 bridge host IP (not 127.0.0.1) so side-by-side Windows daemons
				// never collide on <wslHostIP>:9080 when another Nektar instance already holds it.
				if autoPickPort {
					prefPort, _ := strconv.Atoi(port)
					if freePort, fErr := pickFreePortOnHost(wslHostIP, prefPort); fErr == nil {
						port = strconv.Itoa(freePort)
					}
				}
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

	// Start dedicated h2c Restate Go SDK endpoint decoupled from parent signal ctx so
	// httpSrv.Shutdown can finish draining before the SDK listener stops cleanly.
	epCtx, epCancel := context.WithCancel(context.Background())
	epDone := make(chan struct{})
	epErrCh := make(chan error, 1)
	go func() {
		defer close(epDone)
		log.Printf("nektar restate Go SDK h2c endpoint listening on %s (BuyerDeal, SellerDeal, StandingOrder, EscrowGuardian, Custody, Tx, AnchorCadence, NektarRuntime; auth=%v)",
			listenAddr, len(idKeys) > 0)
		if rErr := rSrv.Start(epCtx, listenAddr); rErr != nil && epCtx.Err() == nil {
			epErrCh <- rErr
			log.Printf("restate SDK endpoint stopped: %v", rErr)
		}
	}()
	var epStopOnce sync.Once
	stopEndpoint := func() {
		epStopOnce.Do(func() {
			epCancel()
			select {
			case <-epDone:
			case <-time.After(3 * time.Second):
			}
		})
	}

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
			stopEndpoint()
			return nil, noop, ctx.Err()
		case rErr := <-epErrCh:
			stopEndpoint()
			return nil, noop, fmt.Errorf("restate SDK endpoint failed to start on %s: %w", listenAddr, rErr)
		default:
		}
		if c, dErr := net.DialTimeout("tcp", dialAddr, 200*time.Millisecond); dErr == nil {
			_ = c.Close()
			// Yield briefly to ensure our own rSrv.Start goroutine did not exit with a bind error
			// masked by another process already listening on dialAddr.
			select {
			case rErr := <-epErrCh:
				stopEndpoint()
				return nil, noop, fmt.Errorf("restate SDK endpoint failed to start on %s: %w", listenAddr, rErr)
			case <-time.After(35 * time.Millisecond):
			}
			select {
			case rErr := <-epErrCh:
				stopEndpoint()
				return nil, noop, fmt.Errorf("restate SDK endpoint failed to start on %s: %w", listenAddr, rErr)
			default:
			}
			epReady = true
			break
		}
		select {
		case <-ctx.Done():
			stopEndpoint()
			return nil, noop, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !epReady {
		stopEndpoint()
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
			stopEndpoint()
			return nil, noop, fmt.Errorf("restate external registration failed (%s -> %s): %w", adminURL, epURI, err)
		}
		return rc, stopEndpoint, nil
	}

	// Mode 2: Auto-supervised local loopback restate-server child process!
	binPath := localBinPath
	if binPath == "" {
		binPath = findRestateBinary()
	}
	if binPath == "" {
		stopEndpoint()
		msg := "restate-server binary not found locally (run `nektar restate-up` to install pinned v" + RestatePinnedVersion + ", or set NEKTAR_RESTATE_ADMIN)"
		if cfg.Required {
			return nil, noop, errors.New(msg)
		}
		log.Printf("warning: %s — running without active Restate server", msg)
		return nil, noop, nil
	}

	ingPort, err := pickFreeLoopbackPort(8111)
	if err != nil {
		stopEndpoint()
		return nil, noop, err
	}
	admPort, err := pickFreeLoopbackPort(9070)
	if err != nil {
		stopEndpoint()
		return nil, noop, err
	}
	nodePort, err := pickFreeLoopbackPort(5122)
	if err != nil {
		stopEndpoint()
		return nil, noop, err
	}
	restateDir := filepath.Join(cfg.DataDir, "restate-data")
	if err := os.MkdirAll(restateDir, 0o750); err != nil {
		stopEndpoint()
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
		stopEndpoint()
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, noop, fmt.Errorf("start local restate-server (%s): %w", binPath, err)
	}
	if cmd.Process != nil {
		_ = os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600)
	}
	var restateStopping atomic.Bool
	childExited := make(chan struct{})
	go func() {
		wErr := cmd.Wait()
		_ = os.Remove(pidPath)
		close(childExited)
		if !restateStopping.Load() {
			log.Printf("warning: supervised restate-server exited unexpectedly: %v", wErr)
			if cfg.OnUnexpectedExit != nil {
				cfg.OnUnexpectedExit(wErr)
			}
		}
	}()
	var stopOnce sync.Once
	stopFn := func() {
		stopOnce.Do(func() {
			// Stop h2c SDK endpoint cleanly before shutting down supervised restate-server.
			stopEndpoint()
			restateStopping.Store(true)
			cmdCancel()
			select {
			case <-childExited:
			case <-time.After(4 * time.Second):
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				<-childExited
			}
			if logFile != nil {
				_ = logFile.Close()
			}
		})
	}

	localIngress := fmt.Sprintf("http://127.0.0.1:%d", ingPort)
	localAdmin := fmt.Sprintf("http://127.0.0.1:%d", admPort)
	rc := market.NewRestateClientWithToken(localIngress, localAdmin, cfg.InternalToken)
	regCtx, regCancel := context.WithCancel(ctx)
	defer regCancel()
	go func() {
		select {
		case <-childExited:
			regCancel()
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
	return strings.Replace(adminURL, ":9070", ":8080", 1)
}

func pickFreeLoopbackPort(preferred int) (int, error) {
	return pickFreePortOnHost("127.0.0.1", preferred)
}

func pickFreePortOnHost(host string, preferred int) (int, error) {
	if host == "" {
		host = "127.0.0.1"
	}
	if preferred > 0 {
		if ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(preferred))); err == nil {
			_ = ln.Close()
			return preferred, nil
		}
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

func envInt64(key string, fallback int64) int64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}
