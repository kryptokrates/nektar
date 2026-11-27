package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/server"
)

const hubRawBase = "https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub"

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
	if !*nativeMode {
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
Description=Nektar v0.1 Pure-Go Hub (%s · %s)
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
		// Query authenticated GET /v1/wallet first using the token written to .env so Mainnet
		// funding QR always prints even when /.well-known/nektar omits private wallet addresses.
		authTok := extractEnvVal(envText, "NEKTAR_OPERATOR_TOKEN")
		if authTok == "" {
			authTok = extractEnvVal(envText, "NEKTAR_HTTP_TOKEN")
		}
		if req, rErr := http.NewRequest(http.MethodGet, localBase+"/v1/wallet", nil); rErr == nil {
			if authTok != "" {
				req.Header.Set("Authorization", "Bearer "+authTok)
			}
			if resp, wErr := hc.Do(req); wErr == nil {
				if resp.StatusCode == http.StatusOK {
					var wm map[string]any
					if json.NewDecoder(io.LimitReader(resp.Body, 32<<10)).Decode(&wm) == nil {
						fundAddr, _ = wm["fundAddress"].(string)
					}
				}
				_ = resp.Body.Close()
			}
		}
		if fundAddr == "" {
			if resp, err := hc.Get(localBase + "/.well-known/nektar"); err == nil {
				var wk map[string]any
				if json.NewDecoder(io.LimitReader(resp.Body, 32<<10)).Decode(&wk) == nil {
					fundAddr, _ = wk["fundAddress"].(string)
				}
				_ = resp.Body.Close()
			}
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

func extractEnvVal(content, key string) string {
	prefix := key + "="
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(ln, prefix))
		}
	}
	return ""
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
