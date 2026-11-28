package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/dock"
	"github.com/kryptokrates/nektar/internal/server"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

type backgroundDaemonOpts struct {
	Network       string
	Confirm       string
	WalletMode    string
	WalletWireURL string
	Listen        string
	DataDir       string
	WithHub       bool
	HubRole       string
	FeePreset     string
	NoRestate     bool
	Quiet         bool
}

func buildServeArgs(opts backgroundDaemonOpts) []string {
	args := []string{"serve"}
	if strings.TrimSpace(opts.Network) != "" {
		args = append(args, "--network", strings.TrimSpace(opts.Network))
	}
	if strings.TrimSpace(opts.Confirm) != "" {
		args = append(args, "--confirm", strings.TrimSpace(opts.Confirm))
	}
	if strings.TrimSpace(opts.WalletMode) != "" {
		args = append(args, "--wallet", strings.TrimSpace(opts.WalletMode))
	}
	if strings.TrimSpace(opts.WalletWireURL) != "" {
		args = append(args, "--wallet-wire-url", strings.TrimSpace(opts.WalletWireURL))
	}
	if strings.TrimSpace(opts.Listen) != "" {
		args = append(args, "--listen", strings.TrimSpace(opts.Listen))
	}
	if strings.TrimSpace(opts.DataDir) != "" {
		args = append(args, "--data-dir", strings.TrimSpace(opts.DataDir))
	}
	if opts.WithHub {
		args = append(args, "--hub")
	}
	if strings.TrimSpace(opts.HubRole) != "" {
		args = append(args, "--hub-role", strings.TrimSpace(opts.HubRole))
	}
	if strings.TrimSpace(opts.FeePreset) != "" {
		args = append(args, "--fee-preset", strings.TrimSpace(opts.FeePreset))
	}
	return args
}

// ensureBackgroundDaemon returns an already-running daemon or spawns a detached `nektar serve`
// child process and waits until /v1/health responds (survives terminal close!).
func ensureBackgroundDaemon(ctx context.Context, opts backgroundDaemonOpts) (*server.DaemonInfo, error) {
	effectiveNet := strings.TrimSpace(opts.Network)
	if effectiveNet == "" {
		effectiveNet = envOr("NEKTAR_NETWORK", "testnet")
	}
	dir := strings.TrimSpace(opts.DataDir)
	if dir == "" {
		dir = defaultDataDirForNetwork(effectiveNet)
	}
	if info, ok := server.DetectLiveDaemon(dir); ok {
		return info, nil
	}
	if strings.EqualFold(effectiveNet, "main") || strings.EqualFold(effectiveNet, "mainnet") {
		confirm := strings.TrimSpace(opts.Confirm)
		if confirm == "" {
			confirm = strings.TrimSpace(os.Getenv("NEKTAR_MAINNET_CONFIRM"))
		}
		if confirm != "I_UNDERSTAND" {
			return nil, errors.New("mainnet requires --confirm I_UNDERSTAND (e.g. `nektar up --network main --confirm I_UNDERSTAND`)")
		}
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}
	exePath, err := os.Executable()
	if err != nil || exePath == "" {
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	logPath := filepath.Join(dir, "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open daemon log %s: %w", logPath, err)
	}

	serveArgs := buildServeArgs(opts)
	cmd := exec.Command(exePath, serveArgs...)
	cmd.Env = os.Environ()
	if opts.NoRestate {
		cmd.Env = append(cmd.Env, "NEKTAR_RESTATE_DISABLE=1")
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	detachCmd(cmd)
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start background daemon: %w", err)
	}
	_ = logFile.Close()

	exited := make(chan error, 1)
	go func() {
		exited <- cmd.Wait()
	}()

	const readyTimeout = 40 * time.Second
	deadline := time.Now().Add(readyTimeout)
	progressShown := false
	startWait := time.Now()
	for time.Now().Before(deadline) {
		if info, ok := server.DetectLiveDaemon(dir); ok {
			return info, nil
		}
		if !opts.Quiet && !progressShown && time.Since(startWait) >= 1500*time.Millisecond {
			fmt.Fprintf(os.Stderr, "• Starting nektar daemon (waiting up to %s for /v1/health)…\n", readyTimeout)
			progressShown = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case wErr := <-exited:
			tail := readTailText(logPath, 600)
			if tail != "" {
				return nil, fmt.Errorf("daemon exited early (%v):\n%s", wErr, tail)
			}
			return nil, fmt.Errorf("daemon exited early: %v (see %s)", wErr, logPath)
		case <-time.After(80 * time.Millisecond):
		}
	}
	tail := readTailText(logPath, 600)
	if tail != "" {
		return nil, fmt.Errorf("daemon did not become ready within %s (see %s):\n%s", readyTimeout, logPath, tail)
	}
	return nil, fmt.Errorf("daemon did not become ready within %s (see %s)", readyTimeout, logPath)
}

func readTailText(path string, maxBytes int) string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return ""
	}
	if len(b) > maxBytes {
		b = b[len(b)-maxBytes:]
	}
	return strings.TrimSpace(string(b))
}

// cmdUp is the flagship 1-command zero-config onboarding path (`nektar up`):
//  1. Starts (or attaches to) the detached background pure-Go Nektar daemon
//  2. Auto-funds Testnet faucet (on 0 sats) and auto-arms safe bounded trading policy
//  3. Auto-docks all detected AI apps (Claude, Codex, Hermes, OpenClaw, Antigravity, Cursor, VS Code…)
//  4. Runs a live remember/recall/groundedAnswer/verifyReceipt probe (Magic Moment #1)
//  5. Prints Mainnet funding QR if needed and opens the paired Dashboard
func cmdUp(args []string) error {
	t0 := time.Now()
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	netName := fs.String("network", "", "Network: testnet (default) | main | memory")
	mainnetShort := fs.Bool("mainnet", false, "Shorthand for --network main (requires --confirm I_UNDERSTAND)")
	confirmFlag := fs.String("confirm", os.Getenv("NEKTAR_MAINNET_CONFIRM"), "Required I_UNDERSTAND when using --network main / --mainnet")
	walletMode := fs.String("wallet", os.Getenv("NEKTAR_WALLET"), "Wallet custody mode: auto (default: BSV Desktop if running on :3321, else embedded) | client | embedded")
	walletWireURL := fs.String("wallet-wire-url", os.Getenv("NEKTAR_WALLET_WIRE_URL"), "BSV Desktop / BRC-100 wire URL (default http://127.0.0.1:3321)")
	listenAddr := fs.String("listen", "", "HTTP listen address override (default 127.0.0.1:18420)")
	dataDir := fs.String("data-dir", "", "Data directory override")
	withHub := fs.Bool("hub", false, "Also mount embedded Overlay Hub at /hub/")
	hubRole := fs.String("hub-role", "", "Embedded Hub role (leaf|spine)")
	feePreset := fs.String("fee-preset", "", "Embedded Hub fee preset (free|lightbsv_egress|hybrid)")
	dockTarget := fs.String("dock", "auto", "Clients to dock (auto or comma-separated IDs: claude,codex,hermes,openclaw,antigravity,openai,cursor,vscode)")
	noDock := fs.Bool("no-dock", false, "Skip auto-docking AI clients")
	noOpen := fs.Bool("no-open", false, "Do not auto-open the Dashboard in your browser")
	noRestate := fs.Bool("no-restate", false, "Deprecated no-op (durable workflows are embedded in pure Go)")
	installSvc := fs.Bool("install-service", false, "Install per-user login autostart service (launchd / systemd --user / Startup)")
	asJSON := fs.Bool("json", false, "Machine-readable JSON output")
	_ = fs.Parse(args)
	if *mainnetShort && strings.TrimSpace(*netName) == "" {
		*netName = "main"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 1. Pure-Go embedded go-workflows SQLite runtime (zero external sidecars needed).
	runtimeDesc := "pure-Go single binary (SQLite WAL · zero sidecars)"

	// 2. Start or attach to the detached background daemon (or hand off to OS service manager if --install-service).
	opts := backgroundDaemonOpts{
		Network:       *netName,
		Confirm:       *confirmFlag,
		WalletMode:    *walletMode,
		WalletWireURL: *walletWireURL,
		Listen:        *listenAddr,
		DataDir:       *dataDir,
		WithHub:       *withHub,
		HubRole:       *hubRole,
		FeePreset:     *feePreset,
		NoRestate:     *noRestate,
		Quiet:         *asJSON,
	}
	exePath, _ := os.Executable()
	if exePath == "" {
		exePath = "nektar"
	}
	effectiveNet := strings.TrimSpace(opts.Network)
	if effectiveNet == "" {
		effectiveNet = envOr("NEKTAR_NETWORK", "testnet")
	}
	targetDir := strings.TrimSpace(opts.DataDir)
	if targetDir == "" {
		targetDir = defaultDataDirForNetwork(effectiveNet)
	}
	if strings.EqualFold(effectiveNet, "main") || strings.EqualFold(effectiveNet, "mainnet") {
		confirm := strings.TrimSpace(opts.Confirm)
		if confirm == "" {
			confirm = strings.TrimSpace(os.Getenv("NEKTAR_MAINNET_CONFIRM"))
		}
		if confirm != "I_UNDERSTAND" {
			return errors.New("mainnet requires --confirm I_UNDERSTAND (e.g. `nektar up --network main --confirm I_UNDERSTAND`)")
		}
	}

	svcPath := ""
	svcErr := ""
	var info *server.DaemonInfo
	var err error
	if *installSvc {
		_ = os.MkdirAll(targetDir, 0o750)
		// If a detached daemon is already running and the OS service manager will own the lifecycle,
		// stop the detached process first so launchd/systemd does not collide on instance.lock.
		if existing, ok := server.DetectLiveDaemon(targetDir); ok && existing != nil && existing.PID > 0 {
			terminatePIDGracefully(existing.PID)
			stopDeadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(stopDeadline) && isPIDAlive(existing.PID) {
				time.Sleep(100 * time.Millisecond)
			}
		}
		p, startedBySvc, iErr := installUserAutostart(exePath, buildServeArgs(opts), targetDir)
		if iErr != nil {
			svcErr = iErr.Error()
		} else {
			svcPath = p
			if startedBySvc {
				waitUntil := time.Now().Add(10 * time.Second)
				for time.Now().Before(waitUntil) {
					if dInfo, ok := server.DetectLiveDaemon(targetDir); ok {
						info = dInfo
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
	}
	if info == nil {
		info, err = ensureBackgroundDaemon(ctx, opts)
		if err != nil {
			return err
		}
	}

	// 3. Auto-dock detected AI clients via the unified embedded internal/dock engine.
	home, _ := os.UserHomeDir()
	var dockResults []dock.ApplyResult
	if !*noDock {
		var targets []string
		for _, part := range strings.Split(*dockTarget, ",") {
			if p := strings.TrimSpace(part); p != "" {
				targets = append(targets, p)
			}
		}
		dockResults = dock.Apply(dock.Options{
			Home:    home,
			ExePath: exePath,
			LiveURL: info.URL,
			Token:   info.AuthToken,
		}, targets)
	}

	// 4. Live Magic Moment #1 probe (recall + groundedAnswer + verifyReceipt).
	var probe map[string]any
	if out, pErr := callLiveAPI(info, http.MethodPost, "/v1/dock/probe", map[string]any{}); pErr == nil {
		probe, _ = out.(map[string]any)
	}

	// Fetch wallet summary & live BSV Desktop presence for custody/funding guidance.
	var fundAddr, activeWalletMode string
	var balanceSats uint64
	refreshWallet := func() {
		if wOut, wErr := callLiveWallet(info, http.MethodGet, "/v1/wallet", nil); wErr == nil {
			if wm, ok := wOut.(map[string]any); ok {
				fundAddr, _ = wm["fundAddress"].(string)
				activeWalletMode, _ = wm["walletMode"].(string)
				if b, ok := wm["balanceSats"].(float64); ok {
					balanceSats = uint64(b)
				}
			}
		}
	}
	refreshWallet()

	// Zero-config trade readiness:
	//  - Testnet: auto-drip faucet when balance == 0 + auto-arm bounded `both` (buyer+seller) policy
	//  - Mainnet: auto-arm `seller` policy (0 treasury spend risk) or `both` when funded / BSV Desktop client mode
	var faucetDripSats uint64
	if info.Network == "testnet" && balanceSats == 0 && activeWalletMode != "client" {
		if fOut, fErr := callLiveAPI(info, http.MethodPost, "/v1/onboard/faucet", map[string]any{}); fErr == nil {
			if fm, ok := fOut.(map[string]any); ok {
				if s, ok := fm["sats"].(float64); ok && s > 0 {
					faucetDripSats = uint64(s)
				}
			}
			refreshWallet()
		}
	}
	tradingPosture := ensureZeroConfigTradingPolicy(info, activeWalletMode, balanceSats, *confirmFlag)

	var desktopProbe nwallet.DesktopProbeStatus
	if info.Network != "memory" {
		desktopProbe = nwallet.ProbeDesktopWallet(ctx, *walletWireURL)
	}

	// Generate 1-click paired dashboard URL if auth is enabled.
	dashURL := strings.TrimRight(info.URL, "/")
	if out, pErr := callLiveAPI(info, http.MethodPost, "/v1/auth/pair-code", map[string]any{}); pErr == nil {
		if m, ok := out.(map[string]any); ok {
			if code, _ := m["code"].(string); code != "" {
				dashURL = dashURL + "/pair?code=" + code
			}
		}
	}

	elapsedMs := time.Since(t0).Milliseconds()
	if *asJSON {
		return printJSON(map[string]any{
			"ok":             true,
			"elapsedMs":      elapsedMs,
			"daemon":         map[string]any{"pid": info.PID, "url": info.URL, "network": info.Network, "dataDir": info.DataDir},
			"dashboard":      dashURL,
			"runtime":        runtimeDesc,
			"tradingPosture": tradingPosture,
			"faucetDripSats": faucetDripSats,
			"fundAddress":    fundAddr,
			"balanceSats":    balanceSats,
			"walletMode":     activeWalletMode,
			"desktop":        desktopProbe,
			"dock":           dockResults,
			"probe":          probe,
			"servicePath":    svcPath,
			"serviceErr":     svcErr,
		})
	}

	var card strings.Builder
	fmt.Fprintf(&card, "\n╭─ nektar v%s UP (%s · pid %d · ready in %dms)\n", Version, info.Network, info.PID, elapsedMs)
	fmt.Fprintf(&card, "│ Dashboard : %s\n", dashURL)
	fmt.Fprintf(&card, "│ Daemon    : %s  (background · logs: %s)\n", info.URL, filepath.Join(info.DataDir, "daemon.log"))
	fmt.Fprintf(&card, "│ Runtime   : %s\n", runtimeDesc)
	fmt.Fprintf(&card, "│ Trading   : %s\n", tradingPosture)
	if fundAddr != "" {
		custodyTag := "embedded BRC-100"
		if activeWalletMode == "client" {
			custodyTag = "BSV Desktop BRC-100 (zero local spending keys)"
		}
		fmt.Fprintf(&card, "│ Wallet    : %s (%d sats · %s)\n", fundAddr, balanceSats, custodyTag)
	}
	if activeWalletMode != "client" && desktopProbe.Available {
		fmt.Fprintf(&card, "│ Desktop   : BSV Desktop detected on %s (%s) — 1-click fund: `nektar wallet fund --from-desktop --sats 25000`\n", desktopProbe.WireURL, desktopProbe.Network)
	}
	if svcPath != "" {
		fmt.Fprintf(&card, "│ Autostart : installed -> %s  (remove with `nektar stop --uninstall-service`)\n", svcPath)
	} else if svcErr != "" {
		fmt.Fprintf(&card, "│ Autostart : warning (%s)\n", svcErr)
	}
	card.WriteString("╰─\n")
	fmt.Print(formatConsoleText(card.String()))

	if !*noDock {
		fmt.Println("\nDocked AI Apps:")
		printDockResults(dockResults, home, false, false)
	}
	printDockProbe(probe)

	if info.Network == "main" && activeWalletMode != "client" && balanceSats == 0 && fundAddr != "" {
		if qr, qErr := core.RenderTerminalQR("bitcoin:" + fundAddr); qErr == nil {
			fmt.Println("\nScan to fund your BSV Mainnet agent wallet (recommended starter: 5,000–10,000 sats):")
			fmt.Print(qr)
		}
	}

	fmt.Println("\nNext:")
	fmt.Println("  1. Restart your docked AI app (Claude Code / Codex / Hermes / Antigravity / OpenClaw / Cursor).")
	fmt.Println(`  2. Ask it: "Sell what we just debugged as an #ops capsule for 50 sats" or "Buy a verified #ops capsule."`)
	fmt.Println(formatConsoleText("  3. Discover Hubs with `nektar peers` · Deploy your own Hub with `nektar hub up` · Stop with `nektar stop`"))

	if !*noOpen && os.Getenv("CI") == "" && os.Getenv("NO_BROWSER") == "" && os.Getenv("SSH_CONNECTION") == "" {
		_ = openBrowserURL(dashURL)
	}
	return nil
}

func ensureZeroConfigTradingPolicy(info *server.DaemonInfo, walletMode string, balanceSats uint64, confirmFlag string) string {
	if info == nil {
		return "uninitialized"
	}
	hasActive := false
	hasBuyer := false
	hasSeller := false
	if out, err := callLiveAPI(info, http.MethodGet, "/v1/policies", nil); err == nil {
		if list, ok := out.([]any); ok {
			for _, item := range list {
				pm, _ := item.(map[string]any)
				if active, _ := pm["active"].(bool); active {
					hasActive = true
					role := strings.ToLower(fmt.Sprint(pm["role"]))
					if role == "both" || role == "buyer" {
						hasBuyer = true
					}
					if role == "both" || role == "seller" {
						hasSeller = true
					}
				}
			}
		}
	}
	if hasActive {
		// Upgrade Mainnet seller-only policy to bounded `both` automatically once funded or paired with BSV Desktop.
		if info.Network == "main" && hasSeller && !hasBuyer && (walletMode == "client" || balanceSats > 0) {
			confirm := strings.TrimSpace(confirmFlag)
			if confirm == "" {
				confirm = strings.TrimSpace(os.Getenv("NEKTAR_MAINNET_CONFIRM"))
			}
			if confirm == "I_UNDERSTAND" {
				if _, err := callLiveAPI(info, http.MethodPost, "/v1/onboard/policy-preset", map[string]any{
					"profile": "standard",
					"preset":  "both",
					"confirm": confirm,
				}); err == nil {
					return "ready (Mainnet bounded buyer + seller policy armed)"
				}
			}
		}
		if hasBuyer && hasSeller {
			return "ready (buyer + seller policy active)"
		}
		if hasSeller {
			return "seller ready (0-risk earning active · fund wallet to unlock buying)"
		}
		return "buyer policy active"
	}

	switch info.Network {
	case "main":
		confirm := strings.TrimSpace(confirmFlag)
		if confirm == "" {
			confirm = strings.TrimSpace(os.Getenv("NEKTAR_MAINNET_CONFIRM"))
		}
		if confirm != "I_UNDERSTAND" {
			return "unarmed (pass --confirm I_UNDERSTAND on Mainnet)"
		}
		preset := "seller"
		if walletMode == "client" || balanceSats > 0 {
			preset = "both"
		}
		if _, err := callLiveAPI(info, http.MethodPost, "/v1/onboard/policy-preset", map[string]any{
			"profile": "standard",
			"preset":  preset,
			"confirm": confirm,
		}); err == nil {
			if preset == "both" {
				return "ready (Mainnet bounded buyer + seller policy auto-armed)"
			}
			return "seller ready (Mainnet seller auto-armed · fund QR below to unlock buying)"
		}
		return "manual approval (`nektar policy approve --preset seller --confirm I_UNDERSTAND`)"
	default:
		if _, err := callLiveAPI(info, http.MethodPost, "/v1/onboard/policy-preset", map[string]any{
			"profile": "easy",
			"preset":  "both",
		}); err == nil {
			return "ready (Testnet sandbox buyer + seller policy auto-armed)"
		}
		return "manual approval (`nektar policy approve --preset both`)"
	}
}

// cmdStop gracefully terminates the running background Nektar daemon and optionally removes login autostart.
func cmdStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	uninstallSvc := fs.Bool("uninstall-service", false, "Also disable and remove the per-user login autostart service")
	_ = fs.Parse(args)

	if *uninstallSvc {
		removed, err := uninstallUserAutostart()
		if err != nil {
			return fmt.Errorf("uninstall autostart service: %w", err)
		}
		if removed != "" {
			fmt.Printf("Removed autostart service: %s\n", removed)
		} else {
			fmt.Println("No autostart service installed.")
		}
	} else {
		// Stop OS service manager first so launchd KeepAlive / systemd Restart does not respawn after stop.
		stopUserAutostart()
	}

	info, ok := detectDaemon(*dataDir, *netName)
	if !ok || info == nil {
		if !*uninstallSvc {
			fmt.Println("nektar stop: no running daemon found")
		}
		return nil
	}
	if info.PID <= 0 {
		return fmt.Errorf("daemon at %s has no recorded PID", info.URL)
	}
	terminatePIDGracefully(info.PID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !isPIDAlive(info.PID) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if isPIDAlive(info.PID) {
		if p, err := os.FindProcess(info.PID); err == nil {
			_ = p.Kill()
		}
	}
	_ = os.Remove(server.DaemonInfoPath(info.DataDir))
	if p := server.ActiveDaemonPointerPath(""); p != "" {
		if raw, err := os.ReadFile(p); err == nil && strings.Contains(string(raw), fmt.Sprintf(`"pid": %d`, info.PID)) {
			_ = os.Remove(p)
		}
	}
	fmt.Printf("Stopped nektar daemon (pid=%d, network=%s, url=%s)\n", info.PID, info.Network, info.URL)
	return nil
}
