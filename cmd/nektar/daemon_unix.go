//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

func detachCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}

// installUserAutostart installs a per-user background service (macOS launchd or Linux systemd --user)
// so Nektar stays alive across reboots for docked AI apps with zero root permissions required.
// Returns (serviceFilePath, startedByServiceManager, err). When startedByServiceManager is true,
// the OS service manager owns the daemon lifecycle so callers must not spawn a duplicate child.
func installUserAutostart(exePath string, serveArgs []string, dataDir string) (string, bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, err
	}
	logPath := filepath.Join(dataDir, "daemon.log")
	if runtime.GOOS == "darwin" {
		plistDir := filepath.Join(home, "Library", "LaunchAgents")
		if err := os.MkdirAll(plistDir, 0o755); err != nil {
			return "", false, err
		}
		plistPath := filepath.Join(plistDir, "run.nektar.agent.plist")
		var argLines strings.Builder
		fmt.Fprintf(&argLines, "    <string>%s</string>\n", xmlEscape(exePath))
		for _, a := range serveArgs {
			fmt.Fprintf(&argLines, "    <string>%s</string>\n", xmlEscape(a))
		}
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>run.nektar.agent</string>
  <key>ProgramArguments</key>
  <array>
%s  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, argLines.String(), xmlEscape(logPath), xmlEscape(logPath))
		if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
			return "", false, err
		}
		_ = exec.Command("launchctl", "unload", plistPath).Run()
		started := exec.Command("launchctl", "load", "-w", plistPath).Run() == nil
		return plistPath, started, nil
	}

	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return "", false, err
	}
	unitPath := filepath.Join(unitDir, "nektar.service")
	quotedArgs := make([]string, 0, len(serveArgs)+1)
	quotedArgs = append(quotedArgs, fmt.Sprintf("%q", exePath))
	for _, a := range serveArgs {
		quotedArgs = append(quotedArgs, fmt.Sprintf("%q", a))
	}
	unit := fmt.Sprintf(`[Unit]
Description=Nektar v0.1 Verifiable AI Agent Memory Daemon
After=network-online.target

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
`, strings.Join(quotedArgs, " "))
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return "", false, err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	started := exec.Command("systemctl", "--user", "enable", "--now", "nektar.service").Run() == nil
	return unitPath, started, nil
}

// stopUserAutostart stops the per-user login service (without removing it) so `nektar stop`
// is not immediately reversed by launchd KeepAlive or systemd Restart.
func stopUserAutostart() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	if runtime.GOOS == "darwin" {
		plistPath := filepath.Join(home, "Library", "LaunchAgents", "run.nektar.agent.plist")
		if _, err := os.Stat(plistPath); err == nil {
			_ = exec.Command("launchctl", "unload", plistPath).Run()
		}
		return
	}
	unitPath := filepath.Join(home, ".config", "systemd", "user", "nektar.service")
	if _, err := os.Stat(unitPath); err == nil {
		_ = exec.Command("systemctl", "--user", "stop", "nektar.service").Run()
	}
}

// uninstallUserAutostart stops, disables, and removes the per-user login autostart service.
func uninstallUserAutostart() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		plistPath := filepath.Join(home, "Library", "LaunchAgents", "run.nektar.agent.plist")
		if _, err := os.Stat(plistPath); os.IsNotExist(err) {
			return "", nil
		}
		_ = exec.Command("launchctl", "unload", "-w", plistPath).Run()
		if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
			return plistPath, err
		}
		return plistPath, nil
	}
	unitPath := filepath.Join(home, ".config", "systemd", "user", "nektar.service")
	if _, err := os.Stat(unitPath); os.IsNotExist(err) {
		return "", nil
	}
	_ = exec.Command("systemctl", "--user", "disable", "--now", "nektar.service").Run()
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return unitPath, err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return unitPath, nil
}

func formatConsoleText(s string) string {
	return s
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
