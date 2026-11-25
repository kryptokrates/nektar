package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/server"
)

const hubRawBase = "https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub"

const hubUsage = `nektar hub — 1-command Hub Node deployment, graduation & standalone overlay server

  nektar hub up [options]               pull GHCR v0.4 Hub stack, auto-configure TLS & start
  nektar hub status [--dir <path>]      inspect hub containers, health & announced card
  nektar hub logs [--dir <path>]        follow hub logs
  nektar hub down [--dir <path>]        stop hub containers (volumes & keys preserved)
  nektar hub template <kind> [options]  print a 1-click template (coolify|dokploy|cloud-init|railway|render|fly|umbrel|casaos)
  nektar hub run [flags]                run standalone Go overlay hub directly in this process

Options for 'up':
  --domain <fqdn>          Public domain with automatic Let's Encrypt TLS via Caddy
  --auto-ip                Zero-domain HTTPS via <public-ip>.sslip.io + Caddy Auto-TLS
  --tunnel                 Zero-port HTTPS via Cloudflare Quick Tunnel (works behind NAT)
  --network <testnet|main> Network (default: testnet; main requires --confirm I_UNDERSTAND)
  --confirm I_UNDERSTAND   Required acknowledgement for mainnet
  --dir <path>             Hub directory (default: ~/.nektar-hub)
  --dry                    Write .env + compose.yaml without starting Docker
  --no-join                Skip auto-joining local running agent to this new hub
`

func cmdHub(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Print(hubUsage)
			return nil
		case "up":
			return cmdHubUp(args[1:])
		case "status", "logs", "down":
			return cmdHubLifecycle(args[0], args[1:])
		case "template":
			return cmdHubTemplate(args[1:])
		case "run":
			return cmdHubRun(args[1:])
		}
	}
	return cmdHubRun(args)
}

func cmdHubRun(args []string) error {
	fs := flag.NewFlagSet("hub run", flag.ExitOnError)
	addr := fs.String("listen", ":"+envOr("NEKTAR_HUB_PORT", "18421"), "Hub HTTP listen address")
	netName := fs.String("network", envOr("NEKTAR_NETWORK", "testnet"), "Network name (memory|regtest|testnet|main)")
	dataDir := fs.String("data-dir", "", "Data directory for hub.sqlite")
	peersFlag := fs.String("peers", os.Getenv("NEKTAR_PEER_HUBS"), "Comma-separated sibling Hub URLs to federate with")
	withLiquidity := fs.Bool("liquidity", true, "Arm Hub starter liquidity, curated shards & verified bounties")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dir := *dataDir
	if dir == "" {
		dir = defaultDataDir()
	}
	dbPath := filepath.Join(dir, "hub.sqlite")
	if *netName == "memory" && *dataDir == "" && os.Getenv("NEKTAR_DATA_DIR") == "" {
		dbPath = ":memory:"
	}

	var rt *runtimeBundle
	if *withLiquidity {
		var rErr error
		rt, rErr = openRuntime(ctx, *netName, *dataDir, envOr("NEKTAR_HUB_PORT", "18421"))
		if rErr != nil {
			log.Printf("hub liquidity runtime skipped: %v", rErr)
		} else {
			defer rt.closeFn()
			_, _ = rt.mem.ReconcilePendingAnchors(ctx)
			rt.mem.Chain().StartBackground(ctx)
			if rt.mem.Chain().Network() != "memory" {
				go func() {
					ticker := time.NewTicker(45 * time.Second)
					defer ticker.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-ticker.C:
							_, _ = rt.mem.RefreshAnchorProofs(ctx)
						}
					}
				}()
			}
		}
	}

	hCfg := hub.Config{
		Network:       *netName,
		DBPath:        dbPath,
		EnableFinger:  true,
		EnableBbox:    true,
		EnableBlogs:   true,
		EnableBorg:    true,
		EnableBsecret: true,
		EnableBstore:  true,
		Getenv:        os.Getenv,
	}
	if rt != nil {
		hCfg.ChainTracker = rt.mem.Chain().Tracker()
	}
	node, err := hub.NewNode(hCfg)
	if err != nil {
		return err
	}
	defer node.Close()

	if rt != nil {
		if _, lErr := hub.ArmHubLiquidity(ctx, node, rt.mem, hub.LiquidityConfig{
			SeedStarterShards: true,
			ArmBounties:       true,
			StarterPriceSats:  90,
			BountyRewardSats:  100,
		}); lErr != nil {
			log.Printf("hub liquidity warning: %v", lErr)
		}
	}

	if strings.TrimSpace(*peersFlag) != "" {
		var peers []string
		for _, p := range strings.Split(*peersFlag, ",") {
			if p = strings.TrimSpace(p); p != "" {
				peers = append(peers, p)
			}
		}
		node.StartFederation(ctx, peers)
	}

	httpSrv := &http.Server{Addr: *addr, Handler: node.Handler(), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()
	log.Printf("nektar hub v%s listening on %s (network=%s db=%s topics=%d lookups=%d liquidity=%v)",
		Version, *addr, *netName, dbPath, len(node.Topics()), len(node.Lookups()), rt != nil)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func defaultHubDir() string {
	if d := strings.TrimSpace(os.Getenv("NEKTAR_HUB_DIR")); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".nektar-hub")
}

func locateLocalHubDeployDir() string {
	cands := []string{filepath.Join("deploy", "hub")}
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		cands = append(cands, filepath.Join(d, "deploy", "hub"), filepath.Join(filepath.Dir(d), "deploy", "hub"))
	}
	for _, c := range cands {
		if _, err := os.Stat(filepath.Join(c, "compose.yaml")); err == nil {
			return c
		}
	}
	return ""
}

func cmdHubLifecycle(sub string, args []string) error {
	fs := flag.NewFlagSet("hub "+sub, flag.ExitOnError)
	hubDir := fs.String("dir", defaultHubDir(), "Hub directory")
	_ = fs.Parse(args)
	compose := filepath.Join(*hubDir, "compose.yaml")
	if _, err := os.Stat(compose); err != nil {
		return fmt.Errorf("no hub found in %s — start one with `nektar hub up`", *hubDir)
	}
	var cmd *exec.Cmd
	switch sub {
	case "down":
		cmd = exec.Command("docker", "compose", "-f", compose, "down")
	case "logs":
		cmd = exec.Command("docker", "compose", "-f", compose, "logs", "-f", "hub")
	case "status":
		cmd = exec.Command("docker", "compose", "-f", compose, "ps")
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return err
	}
	if sub == "status" {
		fmt.Printf("\nhub dir : %s\n", *hubDir)
	}
	return nil
}

func cmdHubTemplate(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nektar hub template <coolify|dokploy|cloud-init|railway|render|fly|umbrel|casaos>")
	}
	kind := strings.ToLower(strings.TrimSpace(args[0]))
	relMap := map[string]string{
		"coolify":    "coolify.compose.yaml",
		"dokploy":    "dokploy.compose.yaml",
		"cloud-init": "cloud-init.yaml",
		"railway":    "railway.json",
		"render":     "render.yaml",
		"fly":        "fly.toml",
		"umbrel":     filepath.Join("umbrel", "docker-compose.yml"),
		"casaos":     "casaos.yaml",
	}
	rel, ok := relMap[kind]
	if !ok {
		return fmt.Errorf("unknown template %q (use coolify|dokploy|cloud-init|railway|render|fly|umbrel|casaos)", kind)
	}
	if local := locateLocalHubDeployDir(); local != "" {
		if raw, err := os.ReadFile(filepath.Join(local, "templates", rel)); err == nil {
			fmt.Print(string(raw))
			return nil
		}
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(hubRawBase + "/templates/" + filepath.ToSlash(rel))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch template failed (HTTP %d)", resp.StatusCode)
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}

func cmdHubUp(args []string) error {
	fs := flag.NewFlagSet("hub up", flag.ExitOnError)
	domainFlag := fs.String("domain", "", "Public domain with automatic Let's Encrypt TLS via Caddy")
	autoIP := fs.Bool("auto-ip", false, "Zero-domain HTTPS via <public-ip>.sslip.io + Caddy Auto-TLS")
	tunnel := fs.Bool("tunnel", false, "Zero-port HTTPS via Cloudflare Quick Tunnel")
	netFlag := fs.String("network", envOr("NEKTAR_NETWORK", "testnet"), "Network (testnet|main)")
	confirm := fs.String("confirm", os.Getenv("NEKTAR_MAINNET_CONFIRM"), "Required I_UNDERSTAND for mainnet")
	hubDir := fs.String("dir", defaultHubDir(), "Hub directory")
	dry := fs.Bool("dry", false, "Write .env + compose.yaml without running Docker")
	noJoin := fs.Bool("no-join", false, "Skip auto-joining local agent to this new hub")
	_ = fs.Parse(args)

	net := strings.ToLower(strings.TrimSpace(*netFlag))
	if net != "testnet" && net != "main" {
		return fmt.Errorf("--network must be testnet|main, got %q", net)
	}
	if net == "main" && *confirm != "I_UNDERSTAND" {
		return errors.New("refusing mainnet hub without --confirm I_UNDERSTAND")
	}

	domain := strings.TrimSpace(*domainFlag)
	domain = strings.TrimPrefix(strings.TrimPrefix(domain, "https://"), "http://")
	if idx := strings.IndexByte(domain, '/'); idx >= 0 {
		domain = domain[:idx]
	}
	if domain == "" && *autoIP {
		ip, err := detectPublicIPv4()
		if err != nil {
			return fmt.Errorf("--auto-ip could not detect public IPv4 (%w) — pass --domain <fqdn> or --tunnel", err)
		}
		domain = strings.ReplaceAll(ip, ".", "-") + ".sslip.io"
		fmt.Printf("nektar hub: resolved zero-domain TLS host -> https://%s\n", domain)
	}

	if err := os.MkdirAll(*hubDir, 0o750); err != nil {
		return err
	}
	composeDst := filepath.Join(*hubDir, "compose.yaml")
	if local := locateLocalHubDeployDir(); local != "" {
		raw, err := os.ReadFile(filepath.Join(local, "compose.yaml"))
		if err != nil {
			return err
		}
		if err := os.WriteFile(composeDst, raw, 0o644); err != nil {
			return err
		}
	} else {
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Get(hubRawBase + "/compose.yaml")
		if err != nil || resp.StatusCode != http.StatusOK {
			return fmt.Errorf("failed to fetch compose.yaml from %s", hubRawBase)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err := os.WriteFile(composeDst, raw, 0o644); err != nil {
			return err
		}
	}

	envPath := filepath.Join(*hubDir, ".env")
	var envText string
	if raw, err := os.ReadFile(envPath); err == nil {
		envText = string(raw)
	}
	for _, k := range []string{"NEKTAR_HTTP_TOKEN", "NEKTAR_OPERATOR_TOKEN"} {
		if !hasEnvKey(envText, k) {
			envText = upsertEnvKV(envText, k, randomHex32())
		}
	}
	envText = upsertEnvKV(envText, "NEKTAR_NETWORK", net)
	envText = upsertEnvKV(envText, "NEKTAR_HTTP_PORT", DefaultLocalPort)
	if net == "main" {
		envText = upsertEnvKV(envText, "NEKTAR_MAINNET_CONFIRM", "I_UNDERSTAND")
		envText = upsertEnvKV(envText, "NEKTAR_HEADERS_SOURCE", "woc:main")
		envText = upsertEnvKV(envText, "NEKTAR_SETTLE_SPEC", "arcade:https://arcade.gorillapool.io")
	} else {
		envText = upsertEnvKV(envText, "NEKTAR_HEADERS_SOURCE", "woc:test")
	}

	var profiles []string
	if domain != "" {
		pub := "https://" + domain
		envText = upsertEnvKV(envText, "NEKTAR_DOMAIN", domain)
		envText = upsertEnvKV(envText, "NEKTAR_PUBLIC_URL", pub)
		profiles = append(profiles, "tls")
	} else if *tunnel {
		envText = upsertEnvKV(envText, "NEKTAR_DOMAIN", ":80")
		profiles = append(profiles, "tls", "tunnel")
	}

	if err := os.WriteFile(envPath, []byte(envText), 0o600); err != nil {
		return err
	}
	modeDesc := "localhost:" + DefaultLocalPort
	if domain != "" {
		modeDesc = "TLS https://" + domain
	} else if *tunnel {
		modeDesc = "Cloudflare Tunnel"
	}
	fmt.Printf("nektar hub: prepared %s (%s, %s)\n", envPath, net, modeDesc)

	if *dry {
		fmt.Println("nektar hub [dry]: skipping docker compose up")
		return nil
	}

	cArgs := []string{"compose", "-f", composeDst, "--env-file", envPath}
	for _, p := range profiles {
		cArgs = append(cArgs, "--profile", p)
	}
	cArgs = append(cArgs, "up", "-d")
	cmd := exec.Command("docker", cArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	if domain != "" && !*noJoin {
		if info, ok := server.DetectLiveDaemon(defaultDataDir()); ok {
			_, _ = callLiveAPI(info, http.MethodPost, "/v1/hosts", map[string]string{"url": "https://" + domain + "/hub"})
		}
	}

	fmt.Printf("\nnektar hub UP in %s\n", *hubDir)
	fmt.Printf("  status : nektar hub status --dir %q\n", *hubDir)
	fmt.Printf("  logs   : nektar hub logs --dir %q\n", *hubDir)
	if domain != "" {
		fmt.Printf("  public : https://%s/.well-known/nektar\n", domain)
	} else if *tunnel {
		fmt.Printf("  tunnel : docker compose -f %q logs cloudflared\n", composeDst)
	}
	return nil
}

func detectPublicIPv4() (string, error) {
	ipRe := regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)
	hc := &http.Client{Timeout: 4 * time.Second}
	for _, u := range []string{"https://api.ipify.org", "https://ifconfig.me/ip"} {
		resp, err := hc.Get(u)
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
		resp.Body.Close()
		ip := strings.TrimSpace(string(raw))
		if resp.StatusCode == http.StatusOK && ipRe.MatchString(ip) {
			return ip, nil
		}
	}
	return "", errors.New("all IP detection endpoints unreachable")
}

func hasEnvKey(content, key string) bool {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `=.+$`)
	return re.MatchString(content)
}

func upsertEnvKV(content, key, val string) string {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `=.*$`)
	if re.MatchString(content) {
		return re.ReplaceAllString(content, key+"="+val)
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return content + key + "=" + val + "\n"
}

func randomHex32() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Keep unused bytes import satisfied if refactored later.
var _ = bytes.TrimSpace
