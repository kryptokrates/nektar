package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kardianos/service"
)

type serviceInstallOpts struct {
	Name       string
	Network    string
	Confirm    string
	DataDir    string
	ListenAddr string
	WalletMode string
	PublicURL  string
	Hosts      string
	WithHub    bool
	SystemWide bool
	Executable string
}

type nektarServiceProgram struct {
	serveArgs []string
	serveFn   func(context.Context, []string) error

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan error
}

func (p *nektarServiceProgram) Start(_ service.Service) error {
	p.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	p.cancel = cancel
	p.done = done
	fn := p.serveFn
	if fn == nil {
		fn = cmdServeWithContext
	}
	args := append([]string(nil), p.serveArgs...)
	p.mu.Unlock()

	go func() {
		done <- fn(ctx, args)
	}()
	return nil
}

func (p *nektarServiceProgram) Stop(_ service.Service) error {
	p.mu.Lock()
	cancel := p.cancel
	done := p.done
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			return errors.New("service stop timed out waiting for nektar serve shutdown")
		}
	}
	return nil
}

func buildServiceConfig(opts serviceInstallOpts) (*service.Config, error) {
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = "nektar"
	}
	netName := strings.ToLower(strings.TrimSpace(opts.Network))
	if netName == "" {
		netName = strings.ToLower(strings.TrimSpace(envOr("NEKTAR_NETWORK", "testnet")))
	}
	if netName == "mainnet" {
		netName = "main"
	}
	confirm := strings.TrimSpace(opts.Confirm)
	if confirm == "" {
		confirm = strings.TrimSpace(os.Getenv("NEKTAR_MAINNET_CONFIRM"))
	}
	if netName == "main" && confirm != "I_UNDERSTAND" {
		return nil, errors.New("service install on mainnet requires --confirm I_UNDERSTAND (or NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND)")
	}

	execPath := strings.TrimSpace(opts.Executable)
	if execPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve executable path: %w", err)
		}
		if resolved, rErr := filepath.EvalSymlinks(exe); rErr == nil {
			execPath = resolved
		} else {
			execPath = exe
		}
	}

	dataDir := strings.TrimSpace(opts.DataDir)
	if dataDir == "" {
		dataDir = defaultDataDirForNetwork(netName)
	}
	if absDir, err := filepath.Abs(dataDir); err == nil {
		dataDir = absDir
	}

	listen := strings.TrimSpace(opts.ListenAddr)
	if listen == "" {
		listen = "127.0.0.1:" + envOr("NEKTAR_HTTP_PORT", DefaultLocalPort)
	}

	args := []string{
		"service", "run",
		"--network", netName,
		"--data-dir", dataDir,
		"--listen", listen,
	}
	if w := strings.TrimSpace(opts.WalletMode); w != "" {
		args = append(args, "--wallet", w)
	}
	if u := strings.TrimSpace(opts.PublicURL); u != "" {
		args = append(args, "--public-url", u)
	}
	if h := strings.TrimSpace(opts.Hosts); h != "" {
		args = append(args, "--hosts", h)
	}
	if opts.WithHub {
		args = append(args, "--hub")
	}

	envVars := map[string]string{
		"NEKTAR_NETWORK":  netName,
		"NEKTAR_DATA_DIR": dataDir,
	}
	if netName == "main" {
		envVars["NEKTAR_MAINNET_CONFIRM"] = "I_UNDERSTAND"
	}
	for _, passthrough := range []string{
		"NEKTAR_WALLET",
		"NEKTAR_WALLET_WIRE_URL",
		"NEKTAR_BACKUP_DIR",
		"NEKTAR_BACKUP_INTERVAL",
		"NEKTAR_BACKUP_KEEP",
		"NEKTAR_BACKUP_S3_BUCKET",
		"NEKTAR_BACKUP_S3_ENDPOINT",
		"NEKTAR_BACKUP_S3_REGION",
		"NEKTAR_BACKUP_S3_PREFIX",
		"NEKTAR_BACKUP_S3_ACCESS_KEY",
		"NEKTAR_NTP_SERVERS",
		"NEKTAR_AUTO_QUERY_ON_MISS",
		"NEKTAR_AUTO_WANT_ON_MISS",
	} {
		if v := strings.TrimSpace(os.Getenv(passthrough)); v != "" {
			envVars[passthrough] = v
		}
	}
	// Never embed NEKTAR_BACKUP_S3_SECRET_KEY directly in OS service unit files (systemd units are
	// often 0644 world-readable). Instead, reference a dedicated 0600 service.env file in dataDir.
	if strings.TrimSpace(os.Getenv("NEKTAR_BACKUP_S3_SECRET_KEY")) != "" {
		envVars["NEKTAR_SERVICE_ENV_FILE"] = filepath.Join(dataDir, "service.env")
	}

	svcOpts := service.KeyValue{
		"Restart":   "on-failure",
		"KeepAlive": true,
		"RunAtLoad": true,
	}
	if !opts.SystemWide && runtime.GOOS != "windows" {
		svcOpts["UserService"] = true
	}

	return &service.Config{
		Name:        name,
		DisplayName: "Nektar Verifiable AI Memory Daemon",
		Description: "Verifiable, encrypted AI agent memory and autonomous BSV knowledge market node (nektar.run)",
		Executable:  execPath,
		Arguments:   args,
		Option:      svcOpts,
		EnvVars:     envVars,
	}, nil
}

func cmdService(args []string) error {
	if len(args) == 0 {
		printServiceUsage()
		return nil
	}
	action := strings.ToLower(strings.TrimSpace(args[0]))
	subArgs := args[1:]

	if action == "run" {
		if envFile := strings.TrimSpace(os.Getenv("NEKTAR_SERVICE_ENV_FILE")); envFile != "" {
			if err := loadServiceEnvFile(envFile); err != nil {
				return fmt.Errorf("load service env file %s: %w", envFile, err)
			}
		}
		prog := &nektarServiceProgram{serveArgs: subArgs}
		svc, err := service.New(prog, &service.Config{
			Name:        "nektar",
			DisplayName: "Nektar Verifiable AI Memory Daemon",
		})
		if err != nil {
			return err
		}
		return svc.Run()
	}

	fs := flag.NewFlagSet("service "+action, flag.ContinueOnError)
	nameFlag := fs.String("name", "nektar", "OS service name")
	netFlag := fs.String("network", "", "Network (testnet|main)")
	confirmFlag := fs.String("confirm", "", "Mainnet confirmation (I_UNDERSTAND)")
	dataDirFlag := fs.String("data-dir", "", "Data directory override")
	listenFlag := fs.String("listen", "", "HTTP listen address (default 127.0.0.1:18420)")
	walletFlag := fs.String("wallet", os.Getenv("NEKTAR_WALLET"), "Wallet custody mode (auto|embedded|client)")
	publicURLFlag := fs.String("public-url", os.Getenv("NEKTAR_PUBLIC_URL"), "Publicly reachable HTTP base URL")
	hostsFlag := fs.String("hosts", os.Getenv("NEKTAR_MARKET_HOSTS"), "Comma-separated market hub URLs")
	withHub := fs.Bool("hub", os.Getenv("NEKTAR_EMBED_HUB") == "1", "Mount embedded overlay hub")
	systemWide := fs.Bool("system", false, "Install as a system-wide service instead of user-level service")
	if err := fs.Parse(subArgs); err != nil {
		return err
	}

	opts := serviceInstallOpts{
		Name:       *nameFlag,
		Network:    *netFlag,
		Confirm:    *confirmFlag,
		DataDir:    *dataDirFlag,
		ListenAddr: *listenFlag,
		WalletMode: *walletFlag,
		PublicURL:  *publicURLFlag,
		Hosts:      *hostsFlag,
		WithHub:    *withHub,
		SystemWide: *systemWide,
	}

	var cfg *service.Config
	var err error
	if action == "install" {
		cfg, err = buildServiceConfig(opts)
		if err != nil {
			return err
		}
	} else {
		svcOpts := service.KeyValue{}
		if !opts.SystemWide && runtime.GOOS != "windows" {
			svcOpts["UserService"] = true
		}
		cfg = &service.Config{
			Name:        strings.TrimSpace(opts.Name),
			DisplayName: "Nektar Verifiable AI Memory Daemon",
			Option:      svcOpts,
		}
	}

	prog := &nektarServiceProgram{}
	svc, err := service.New(prog, cfg)
	if err != nil {
		return err
	}

	switch action {
	case "install":
		var secretsEnvFile string
		if envPath := strings.TrimSpace(cfg.EnvVars["NEKTAR_SERVICE_ENV_FILE"]); envPath != "" {
			if err := writeServiceSecretsEnvFile(envPath); err != nil {
				return fmt.Errorf("write service secrets env file: %w", err)
			}
			secretsEnvFile = envPath
		}
		if err := svc.Install(); err != nil {
			return err
		}
		resp := map[string]any{
			"ok":         true,
			"action":     "install",
			"platform":   svc.Platform(),
			"name":       cfg.Name,
			"executable": cfg.Executable,
			"arguments":  cfg.Arguments,
			"nextStep":   fmt.Sprintf("Run `nektar service start --name %s` to launch the supervised daemon.", cfg.Name),
		}
		if secretsEnvFile != "" {
			resp["secretsEnvFile"] = secretsEnvFile
			resp["securityNote"] = fmt.Sprintf(
				"NEKTAR_BACKUP_S3_SECRET_KEY was written to %s (mode 0600) instead of the OS service unit file. Keep %s restricted to the service user (chmod 600 on Linux/macOS).",
				secretsEnvFile, secretsEnvFile,
			)
		} else if runtime.GOOS == "linux" {
			resp["securityNote"] = "systemd unit files are often 0644 (world-readable); store backup S3 secrets in a 0600 EnvironmentFile rather than the unit file."
		}
		out, _ := json.MarshalIndent(resp, "", "  ")
		fmt.Println(string(out))
		return nil
	case "uninstall":
		if err := svc.Uninstall(); err != nil {
			return err
		}
		fmt.Printf("uninstalled service %q (%s)\n", cfg.Name, svc.Platform())
		return nil
	case "start", "stop", "restart":
		if err := service.Control(svc, action); err != nil {
			return err
		}
		fmt.Printf("service %q %s ok (%s)\n", cfg.Name, action, svc.Platform())
		return nil
	case "status":
		st, err := svc.Status()
		statusText := "unknown"
		switch st {
		case service.StatusRunning:
			statusText = "running"
		case service.StatusStopped:
			statusText = "stopped"
		}
		if errors.Is(err, service.ErrNotInstalled) {
			statusText = "not_installed"
			err = nil
		}
		out, _ := json.MarshalIndent(map[string]any{
			"ok":       err == nil,
			"name":     cfg.Name,
			"platform": svc.Platform(),
			"status":   statusText,
		}, "", "  ")
		fmt.Println(string(out))
		return err
	default:
		printServiceUsage()
		return fmt.Errorf("unknown service action %q", action)
	}
}

func printServiceUsage() {
	fmt.Println(`nektar service — Cross-platform OS daemon supervision (systemd / launchd / Windows Service)

Usage:
  nektar service install   [--name nektar] [--network testnet|main] [--confirm I_UNDERSTAND] [--data-dir <dir>] [--listen 127.0.0.1:18420] [--wallet embedded|client] [--hub] [--system]
  nektar service start     [--name nektar] [--system]
  nektar service stop      [--name nektar] [--system]
  nektar service restart   [--name nektar] [--system]
  nektar service status    [--name nektar] [--system]
  nektar service uninstall [--name nektar] [--system]`)
}

// writeServiceSecretsEnvFile persists sensitive S3 backup credentials into a dedicated 0600 file
// so secrets are never written into potentially world-readable (0644) systemd unit files.
func writeServiceSecretsEnvFile(path string) error {
	var lines []string
	for _, key := range []string{"NEKTAR_BACKUP_S3_SECRET_KEY"} {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			continue
		}
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("%s contains invalid newline character", key)
		}
		lines = append(lines, key+"="+v)
	}
	if len(lines) == 0 {
		return nil
	}
	payload := []byte(strings.Join(lines, "\n") + "\n")
	return restoreFileAtomic(path, payload, false)
}

// loadServiceEnvFile loads KEY=VALUE pairs from a 0600 secrets env file into the process
// environment without overriding variables already explicitly set by the caller.
func loadServiceEnvFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" {
			continue
		}
		if strings.TrimSpace(os.Getenv(k)) == "" {
			_ = os.Setenv(k, v)
		}
	}
	return nil
}
