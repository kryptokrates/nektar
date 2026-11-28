package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/server"
)

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

func resolveExistingHubDir(dirFlag string, explicit bool) string {
	hasHub := func(d string) bool {
		if _, err := os.Stat(filepath.Join(d, "compose.yaml")); err == nil {
			return true
		}
		if _, err := os.Stat(filepath.Join(d, "nektar-hub.service")); err == nil {
			return true
		}
		return false
	}
	if hasHub(dirFlag) || explicit {
		return dirFlag
	}
	home, _ := os.UserHomeDir()
	legacy := filepath.Join(home, "nektar-hub")
	if hasHub(legacy) {
		return legacy
	}
	return dirFlag
}

func cmdHubLifecycle(sub string, args []string) error {
	fs := flag.NewFlagSet("hub "+sub, flag.ExitOnError)
	hubDirFlag := fs.String("dir", defaultHubDir(), "Hub directory")
	_ = fs.Parse(args)
	explicitDir := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "dir" {
			explicitDir = true
		}
	})
	dir := resolveExistingHubDir(*hubDirFlag, explicitDir)
	compose := filepath.Join(dir, "compose.yaml")
	nativeUnit := filepath.Join(dir, "nektar-hub.service")
	_, composeErr := os.Stat(compose)
	_, nativeErr := os.Stat(nativeUnit)
	if composeErr != nil && nativeErr != nil {
		return fmt.Errorf("no hub found in %s — start one with `nektar hub up`", dir)
	}
	isNative := composeErr != nil && nativeErr == nil
	hubDataDir := filepath.Join(dir, "data")
	hubLogPath := filepath.Join(dir, "hub.log")
	if isNative {
		switch sub {
		case "down":
			stopped := false
			if info, ok := server.DetectLiveDaemon(hubDataDir); ok && info != nil && info.PID > 0 {
				terminatePIDGracefully(info.PID)
				stopped = true
			}
			for _, pidFile := range []string{"hub.pid", "tunnel.pid"} {
				pPath := filepath.Join(dir, pidFile)
				if raw, rErr := os.ReadFile(pPath); rErr == nil {
					if pid, pErr := strconv.Atoi(strings.TrimSpace(string(raw))); pErr == nil && pid > 0 {
						terminatePIDGracefully(pid)
						stopped = true
					}
					_ = os.Remove(pPath)
				}
			}
			if _, sErr := exec.LookPath("systemctl"); sErr == nil {
				_ = exec.Command("systemctl", "stop", "nektar-hub").Run()
			}
			if stopped {
				fmt.Printf("Stopped native nektar hub in %s\n", dir)
			} else {
				fmt.Printf("No running native hub process found in %s\n", dir)
			}
			return nil
		case "logs":
			if raw, rErr := os.ReadFile(hubLogPath); rErr == nil && len(raw) > 0 {
				if len(raw) > 16384 {
					raw = raw[len(raw)-16384:]
				}
				fmt.Print(string(raw))
				return nil
			}
			if _, sErr := exec.LookPath("journalctl"); sErr == nil {
				cmd := exec.Command("journalctl", "-u", "nektar-hub", "-f")
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				return cmd.Run()
			}
			return fmt.Errorf("no hub log found at %s", hubLogPath)
		case "status":
			if info, ok := server.DetectLiveDaemon(hubDataDir); ok && info != nil {
				fmt.Printf("native hub : UP (pid=%d · %s · url=%s)\n", info.PID, info.Network, info.URL)
			} else {
				fmt.Printf("native hub : STOPPED (start with `nektar hub up --dir %q`)\n", dir)
			}
		}
	} else {
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
	}
	if sub == "status" {
		fmt.Printf("\nhub dir : %s", dir)
		if isNative {
			fmt.Print(" (native systemd)")
		}
		fmt.Println()
		port := DefaultLocalPort
		if raw, err := os.ReadFile(filepath.Join(dir, ".env")); err == nil {
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
