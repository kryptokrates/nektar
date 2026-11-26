package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
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
  --native                 Zero-Docker mode: write .env + systemd service unit using this static nektar binary
  --network <testnet|main> Network (default: testnet; main requires --confirm I_UNDERSTAND)
  --confirm I_UNDERSTAND   Required acknowledgement for mainnet
  --hub-role <leaf|spine>  Spine-and-Leaf topology role (default: leaf)
  --fee-preset <preset>    Monetization preset (free|lightbsv_egress|hybrid; default: hybrid on main, free on testnet)
  --dir <path>             Hub directory (default: ~/.nektar-hub)
  --dry                    Write config files without starting Docker
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
	upstreamFlag := fs.String("upstream-spines", os.Getenv("NEKTAR_UPSTREAM_SPINES"), "Comma-separated Spine Hub URLs for Leaf upstream submission")
	withLiquidity := fs.Bool("liquidity", true, "Arm Hub starter liquidity, curated shards & verified bounties")
	hubRoleFlag := fs.String("hub-role", os.Getenv("NEKTAR_HUB_ROLE"), "Hub topology role (leaf|spine)")
	feePresetFlag := fs.String("fee-preset", os.Getenv("NEKTAR_HUB_FEE_PRESET"), "Hub fee preset (free|lightbsv_egress|hybrid)")
	wantFeeFlag := fs.Uint64("want-fee-sats", 0, "BRC-105 fee in satoshis for WantAd POST /submit")
	offerFeeFlag := fs.Uint64("offer-fee-sats", 0, "BRC-105 fee in satoshis for OfferAd POST /submit")
	announceFeeFlag := fs.Uint64("announce-fee-sats", 0, "BRC-105 fee in satoshis for AnnounceAd POST /submit")
	bboxFeeFlag := fs.Uint64("bbox-fee-sats", 0, "BRC-105 fee in satoshis for NAT POST /bbox/send")
	egressFeeFlag := fs.Uint64("egress-sats-per-100", 0, "Metered egress fee in satoshis per 100 market events")
	lookupPricesFlag := fs.String("lookup-prices", os.Getenv("NEKTAR_HUB_LOOKUP_PRICES"), "Priced lookup classes (e.g. history=5,export=20)")
	spinePeersFlag := fs.String("spine-peer-pubs", os.Getenv("NEKTAR_SPINE_PEER_PUBS"), "Comma-separated bilateral Spine peer pubkeys (settlement-free peering)")
	_ = fs.Parse(args)

	visited := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })

	feeSched := hub.LoadFeeSchedule(os.Getenv)
	if visited["fee-preset"] {
		feeSched = hub.DefaultFeeSchedule(*feePresetFlag)
		// Re-apply explicit env vars only if not overridden by CLI flags below.
		if envRole := strings.ToLower(strings.TrimSpace(os.Getenv("NEKTAR_HUB_ROLE"))); envRole == string(hub.RoleSpine) || envRole == string(hub.RoleLeaf) {
			feeSched.Role = hub.HubRole(envRole)
		}
	}
	if visited["hub-role"] {
		if r := strings.ToLower(strings.TrimSpace(*hubRoleFlag)); r == string(hub.RoleSpine) || r == string(hub.RoleLeaf) {
			feeSched.Role = hub.HubRole(r)
		}
	}
	if visited["want-fee-sats"] {
		feeSched.SubmitWantSats = *wantFeeFlag
	}
	if visited["offer-fee-sats"] {
		feeSched.SubmitOfferSats = *offerFeeFlag
	}
	if visited["announce-fee-sats"] {
		feeSched.SubmitAnnounceSats = *announceFeeFlag
	}
	if visited["bbox-fee-sats"] {
		feeSched.BboxSendSats = *bboxFeeFlag
	}
	if visited["egress-sats-per-100"] {
		feeSched.EgressSatsPer100Events = *egressFeeFlag
	}
	if visited["lookup-prices"] {
		feeSched.LookupPrices = hub.ParseLookupPrices(*lookupPricesFlag)
	}
	if visited["spine-peer-pubs"] {
		feeSched.SpinePeerPubs = nil
		for _, p := range strings.Split(*spinePeersFlag, ",") {
			if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
				feeSched.SpinePeerPubs = append(feeSched.SpinePeerPubs, p)
			}
		}
	}
	feeSched.Normalize()

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
	if *withLiquidity || feeSched.HasAnyFees() {
		if *netName != "memory" {
			releaseLock, lockErr := acquireInstanceLock(dir, *addr)
			if lockErr != nil {
				return lockErr
			}
			defer releaseLock()
		}
		var rErr error
		rt, rErr = openRuntime(ctx, *netName, *dataDir, envOr("NEKTAR_HUB_PORT", "18421"))
		if rErr != nil {
			if feeSched.HasAnyFees() {
				return fmt.Errorf("hub: wallet runtime required when fees are enabled: %w", rErr)
			}
			log.Printf("hub liquidity runtime skipped: %v", rErr)
		} else {
			defer rt.closeFn()
			_, _ = rt.mem.ReconcilePendingAnchors(ctx)
			rt.mem.Chain().StartBackground(ctx)
			stopRefresher := startAnchorProofRefresher(ctx, rt.mem)
			defer stopRefresher()
		}
	}

	var upstreamSpines []string
	for _, s := range strings.Split(*upstreamFlag, ",") {
		if s = strings.TrimSpace(s); s != "" {
			upstreamSpines = append(upstreamSpines, s)
		}
	}

	hCfg := hub.Config{
		Network:        *netName,
		DBPath:         dbPath,
		EnableFinger:   true,
		EnableBbox:     true,
		EnableBlogs:    true,
		EnableBorg:     true,
		EnableBsecret:  true,
		EnableBstore:   true,
		Getenv:         os.Getenv,
		Fees:           feeSched,
		UpstreamSpines: upstreamSpines,
	}
	if rt != nil {
		hCfg.ChainTracker = rt.mem.Chain().Tracker()
		hCfg.FeeCollector = hub.NewPortFeeCollector(rt.mem.Chain())
	} else if *netName == "main" || *netName == "testnet" {
		headerSpec := strings.TrimSpace(os.Getenv("NEKTAR_HEADERS_SOURCE"))
		if headerSpec == "" {
			headerSpec = chain.DefaultHeaderSources(*netName)
		}
		chainSpec := strings.TrimSpace(os.Getenv("NEKTAR_CHAIN_SPEC"))
		if chainSpec == "" {
			if *netName == "main" {
				chainSpec = "woc:main"
			} else {
				chainSpec = "woc:test"
			}
		}
		settleSpec := strings.TrimSpace(os.Getenv("NEKTAR_SETTLE_SPEC"))
		wt, wErr := chain.NewWitnessedTrackerFromSpec(*netName, headerSpec, chainSpec, settleSpec, core.SafeHTTPClientForNetwork(*netName, 15*time.Second))
		if wErr != nil {
			return fmt.Errorf("hub: live network %q requires independent witnessed ChainTracker: %w", *netName, wErr)
		}
		hCfg.ChainTracker = wt
	}
	node, err := hub.NewNode(hCfg)
	if err != nil {
		return err
	}
	defer node.Close()

	if rt != nil {
		if rt.disc != nil && rt.mkt != nil && rt.mkt.Policy() != nil {
			rt.disc.SetHubFeePolicy(rt.mkt.Policy().CanPayHubFee, rt.mkt.Policy().RecordHubFeeSpend)
		}
		if rt.disc != nil {
			node.SetUpstreamSubmitter(rt.disc.SubmitBEEF)
			node.SetEgressAuth(
				func(action, aud string, cursor int64, domainsCSV string) (string, string, string, string, error) {
					return discovery.SignEgressAuthHeaders(rt.disc.AnnounceKey(), action, aud, cursor, domainsCSV)
				},
				func(tCtx context.Context, spineURL string, minTopupSats uint64) error {
					_, tErr := rt.disc.TopUpHubEgressCredits(tCtx, spineURL, minTopupSats, rt.disc.AnnouncePubHex())
					return tErr
				},
				nil,
			)
		}
		rt.mem.Chain().OnTxMined(func(txid string, height uint32, mp *transaction.MerklePath) {
			mCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if _, aErr := rt.mem.RefreshAnchorProofByTxID(mCtx, txid, height, mp); aErr == nil {
				_, _ = hub.RefreshHubLiquidityStock(mCtx, rt.mem)
			}
			_ = node.OnBountyPayoutMined(txid)
			_ = node.OnFeePaymentMined(txid, height)
		})
		rt.mem.Chain().OnReorg(func(forkHeight uint32, _ []string) {
			rCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			_, _ = rt.mem.HandleReorg(forkHeight)
			_ = node.HandleReorg(rCtx, forkHeight)
		})
		if *withLiquidity {
			if _, lErr := hub.ArmHubLiquidity(ctx, node, rt.mem, hub.LiquidityConfig{
				SeedStarterShards: true,
				ArmBounties:       true,
				StarterPriceSats:  90,
				BountyRewardSats:  100,
			}); lErr != nil {
				log.Printf("hub liquidity warning: %v", lErr)
			}
		}
	}

	if strings.TrimSpace(*peersFlag) != "" {
		var peers []string
		for _, p := range strings.Split(*peersFlag, ",") {
			if p = strings.TrimSpace(p); p != "" {
				peers = append(peers, p)
			}
		}
		node.StartFederationWithTier(ctx, peers, true)
	}
	if len(upstreamSpines) > 0 {
		node.StartFederationWithTier(ctx, upstreamSpines, true)
	}

	httpSrv := &http.Server{Addr: *addr, Handler: node.Handler(), ReadHeaderTimeout: 15 * time.Second}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		node.CloseSSE()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()
	log.Printf("nektar hub v%s listening on %s (network=%s role=%s fees=%s db=%s topics=%d lookups=%d liquidity=%v)",
		Version, *addr, *netName, feeSched.Role, feeSched.Preset, dbPath, len(node.Topics()), len(node.Lookups()), rt != nil && *withLiquidity)
	serveErr := httpSrv.ListenAndServe()
	if serveErr != nil && serveErr != http.ErrServerClosed {
		stop()
		<-shutdownDone
		return serveErr
	}
	<-shutdownDone
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
		port := DefaultLocalPort
		if raw, err := os.ReadFile(filepath.Join(*hubDir, ".env")); err == nil {
			for _, ln := range strings.Split(string(raw), "\n") {
				if k, v, ok := strings.Cut(strings.TrimSpace(ln), "="); ok && k == "NEKTAR_HTTP_PORT" && strings.TrimSpace(v) != "" {
					if _, pErr := strconv.Atoi(strings.TrimSpace(v)); pErr == nil {
						port = strings.TrimSpace(v)
					}
				}
			}
		}
		hc := &http.Client{Timeout: 3 * time.Second}
		for _, path := range []string{"/hub/v1/hub/earnings", "/v1/hub/earnings"} {
			if resp, err := hc.Get("http://127.0.0.1:" + port + path); err == nil {
				if resp.StatusCode == http.StatusOK {
					var res struct {
						Role     string                 `json:"role"`
						Earnings hub.FeeEarningsSummary `json:"earnings"`
					}
					if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&res) == nil {
						fmt.Printf("role    : %s\n", res.Role)
						fmt.Printf("earned  : %d sats total (%d sats 24h, %d mined, %d unmined fast)\n",
							res.Earnings.TotalEarnedSats, res.Earnings.Earned24hSats, res.Earnings.MinedEarnedSats, res.Earnings.UnminedFastSats)
						fmt.Printf("egress  : %d events billed across %d account(s) (flagged payers: %d)\n",
							res.Earnings.TotalEgressDelivered, res.Earnings.ActiveCreditAccounts, res.Earnings.FlaggedPayers)
					}
					resp.Body.Close()
					break
				}
				resp.Body.Close()
			}
		}
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
	nativeMode := fs.Bool("native", false, "Zero-Docker mode: generate .env + systemd unit for static nektar binary")
	netFlag := fs.String("network", envOr("NEKTAR_NETWORK", "testnet"), "Network (testnet|main)")
	confirm := fs.String("confirm", os.Getenv("NEKTAR_MAINNET_CONFIRM"), "Required I_UNDERSTAND for mainnet")
	hubRoleFlag := fs.String("hub-role", envOr("NEKTAR_HUB_ROLE", string(hub.RoleLeaf)), "Spine-and-Leaf topology role (leaf|spine)")
	feePresetFlag := fs.String("fee-preset", os.Getenv("NEKTAR_HUB_FEE_PRESET"), "Monetization preset (free|lightbsv_egress|hybrid)")
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

	if err := os.MkdirAll(*hubDir, 0o700); err != nil {
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
	roleNorm := strings.ToLower(strings.TrimSpace(*hubRoleFlag))
	if roleNorm != string(hub.RoleSpine) {
		roleNorm = string(hub.RoleLeaf)
	}
	presetNorm := strings.ToLower(strings.TrimSpace(*feePresetFlag))
	if presetNorm == "" {
		if net == "main" {
			presetNorm = hub.PresetHybrid
		} else {
			presetNorm = hub.PresetFree
		}
	}
	envText = upsertEnvKV(envText, "NEKTAR_HUB_ROLE", roleNorm)
	envText = upsertEnvKV(envText, "NEKTAR_HUB_FEE_PRESET", presetNorm)
	seedHub := "https://testnet.nektar.run/hub"
	if net == "main" {
		seedHub = "https://nektar.run/hub"
		envText = upsertEnvKV(envText, "NEKTAR_MAINNET_CONFIRM", "I_UNDERSTAND")
		envText = upsertEnvKV(envText, "NEKTAR_HEADERS_SOURCE", chain.DefaultHeaderSources("main"))
		envText = upsertEnvKV(envText, "NEKTAR_SETTLE_SPEC", chain.DefaultSettleSpec("main"))
	} else {
		envText = upsertEnvKV(envText, "NEKTAR_HEADERS_SOURCE", chain.DefaultHeaderSources("testnet"))
		envText = upsertEnvKV(envText, "NEKTAR_SETTLE_SPEC", chain.DefaultSettleSpec("testnet"))
	}
	if roleNorm == string(hub.RoleLeaf) && !hasEnvKey(envText, "NEKTAR_UPSTREAM_SPINES") {
		envText = upsertEnvKV(envText, "NEKTAR_UPSTREAM_SPINES", seedHub)
	}

	var profiles []string
	if domain != "" {
		pub := "https://" + domain
		envText = upsertEnvKV(envText, "NEKTAR_DOMAIN", domain)
		envText = upsertEnvKV(envText, "NEKTAR_PUBLIC_URL", pub)
		envText = upsertEnvKV(envText, "NEKTAR_TRUST_PROXY", "1")
		profiles = append(profiles, "tls")
	} else if *tunnel {
		envText = upsertEnvKV(envText, "NEKTAR_DOMAIN", ":80")
		envText = upsertEnvKV(envText, "NEKTAR_TRUST_PROXY", "1")
		if strings.TrimSpace(os.Getenv("CLOUDFLARE_TUNNEL_TOKEN")) != "" {
			profiles = append(profiles, "tunnel-token")
		} else {
			profiles = append(profiles, "tunnel")
		}
	}

	tmpEnv := envPath + ".tmp"
	if err := os.WriteFile(tmpEnv, []byte(envText), 0o600); err != nil {
		_ = os.Remove(tmpEnv)
		return err
	}
	_ = os.Chmod(tmpEnv, 0o600)
	if err := os.Rename(tmpEnv, envPath); err != nil {
		_ = os.Remove(tmpEnv)
		return err
	}
	modeDesc := "localhost:" + DefaultLocalPort
	if domain != "" {
		modeDesc = "TLS https://" + domain
	} else if *tunnel {
		modeDesc = "Cloudflare Tunnel"
	}
	fmt.Printf("nektar hub: prepared %s (%s · role=%s · fees=%s · %s)\n", envPath, net, roleNorm, presetNorm, modeDesc)

	if *nativeMode {
		exePath, _ := os.Executable()
		if exePath == "" {
			exePath = "/usr/local/bin/nektar"
		}
		unitPath := filepath.Join(*hubDir, "nektar-hub.service")
		unitText := fmt.Sprintf(`[Unit]
Description=Nektar v0.4 Pure-Go Hub (%s · %s)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%s
EnvironmentFile=%s
Environment=NEKTAR_DATA_DIR=%s/data
ExecStart=%s serve -hub -listen 127.0.0.1:%s
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`, net, roleNorm, *hubDir, envPath, *hubDir, exePath, DefaultLocalPort)
		if err := os.WriteFile(unitPath, []byte(unitText), 0o644); err != nil {
			return err
		}
		fmt.Printf("\n✓ Zero-Docker Hub config ready in %s\n", *hubDir)
		fmt.Printf("  1. Run right now   : set -a && . %q && set +a && %s serve -hub -data-dir %q\n", envPath, exePath, filepath.Join(*hubDir, "data"))
		fmt.Printf("  2. Or systemd VPS  : sudo cp %q /etc/systemd/system/ && sudo systemctl enable --now nektar-hub\n", unitPath)
		if domain != "" {
			fmt.Printf("  3. Reverse proxy   : caddy reverse-proxy --from %s --to 127.0.0.1:%s\n", domain, DefaultLocalPort)
		}
		return nil
	}

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

	// Poll local Hub health up to 35s so operators get immediate green confirmation + funding QR!
	localBase := "http://127.0.0.1:" + DefaultLocalPort
	hc := &http.Client{Timeout: 3 * time.Second}
	var healthy bool
	var fundAddr string
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := hc.Get(localBase + "/hub/v1/health"); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				healthy = true
				break
			}
		}
		time.Sleep(1 * time.Second)
	}
	if healthy {
		if resp, err := hc.Get(localBase + "/.well-known/nektar"); err == nil {
			var wk map[string]any
			if json.NewDecoder(io.LimitReader(resp.Body, 32<<10)).Decode(&wk) == nil {
				fundAddr, _ = wk["fundAddress"].(string)
			}
			_ = resp.Body.Close()
		}
	}

	if domain != "" && !*noJoin {
		if info, ok := server.DetectLiveDaemon(defaultDataDirForNetwork(net)); ok {
			_, _ = callLiveAPI(info, http.MethodPost, "/v1/hosts", map[string]string{"url": "https://" + domain + "/hub"})
		}
	}

	if healthy {
		fmt.Printf("\n✓ nektar hub LIVE & HEALTHY in %s (%s · role=%s · fees=%s)\n", *hubDir, net, roleNorm, presetNorm)
	} else {
		fmt.Printf("\nnektar hub STARTED in %s (still booting — check `nektar hub logs`)\n", *hubDir)
	}
	fmt.Printf("  status : nektar hub status --dir %q\n", *hubDir)
	fmt.Printf("  logs   : nektar hub logs --dir %q\n", *hubDir)
	if domain != "" {
		fmt.Printf("  public : https://%s/.well-known/nektar\n", domain)
		fmt.Printf("  terms  : https://%s/hub/v1/terms\n", domain)
	} else if *tunnel {
		fmt.Printf("  tunnel : docker compose -f %q logs cloudflared\n", composeDst)
	}
	if fundAddr != "" && net == "main" {
		fmt.Printf("  fund   : %s (send 5,000–10,000 sats for upstream Spine egress credits)\n", fundAddr)
		if qr, err := core.RenderTerminalQR("bitcoin:" + fundAddr); err == nil {
			fmt.Print(qr)
		}
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
	if _, err := rand.Read(b[:]); err != nil {
		panic("hub: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Keep unused bytes import satisfied if refactored later.
var _ = bytes.TrimSpace
