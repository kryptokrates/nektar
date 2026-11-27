//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func detachCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
		HideWindow:    true,
	}
}

const windowsAutostartTaskName = "NektarDaemon"

func windowsStartupDir() (string, error) {
	appData := envOr("APPDATA", "")
	if appData == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		appData = filepath.Join(home, "AppData", "Roaming")
	}
	return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup"), nil
}

// installUserAutostart registers a per-user Windows logon task via `schtasks` (avoiding deprecated
// .vbs Startup scripts that trigger Windows Defender/ASR), with a `.lnk` Startup shortcut fallback.
func installUserAutostart(exePath string, serveArgs []string, _ string) (string, bool, error) {
	// Clean up any legacy .vbs script from earlier versions.
	if startupDir, err := windowsStartupDir(); err == nil {
		_ = os.Remove(filepath.Join(startupDir, "nektar-daemon.vbs"))
	}
	quoted := make([]string, 0, len(serveArgs)+1)
	quoted = append(quoted, `"`+exePath+`"`)
	for _, a := range serveArgs {
		if strings.ContainsAny(a, " \t\"") {
			quoted = append(quoted, `"`+strings.ReplaceAll(a, `"`, `\"`)+`"`)
		} else {
			quoted = append(quoted, a)
		}
	}
	tr := strings.Join(quoted, " ")
	cmd := exec.Command("schtasks", "/Create", "/F", "/SC", "ONLOGON", "/TN", windowsAutostartTaskName, "/TR", tr)
	if out, err := cmd.CombinedOutput(); err == nil {
		return "TaskScheduler:" + windowsAutostartTaskName, false, nil
	} else {
		// Fallback: create a standard Windows Startup .lnk shortcut via PowerShell WScript.Shell (no .vbs on disk).
		startupDir, sErr := windowsStartupDir()
		if sErr != nil {
			return "", false, fmt.Errorf("schtasks failed (%v: %s) and Startup dir unavailable: %w", err, strings.TrimSpace(string(out)), sErr)
		}
		if mkErr := os.MkdirAll(startupDir, 0o755); mkErr != nil {
			return "", false, mkErr
		}
		lnkPath := filepath.Join(startupDir, "Nektar.lnk")
		psEscape := func(s string) string { return strings.ReplaceAll(s, "'", "''") }
		psScript := fmt.Sprintf(
			"$ws = New-Object -ComObject WScript.Shell; $sc = $ws.CreateShortcut('%s'); $sc.TargetPath = '%s'; $sc.Arguments = '%s'; $sc.WindowStyle = 7; $sc.Save()",
			psEscape(lnkPath),
			psEscape(exePath),
			psEscape(strings.Join(serveArgs, " ")),
		)
		if pOut, pErr := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", psScript).CombinedOutput(); pErr != nil {
			return "", false, fmt.Errorf("autostart install failed: schtasks (%v) and shortcut (%v: %s)", err, pErr, strings.TrimSpace(string(pOut)))
		}
		return lnkPath, false, nil
	}
}

func stopUserAutostart() {
	// Windows ONLOGON task / Startup shortcut does not auto-respawn on stop.
}

// uninstallUserAutostart removes the NektarDaemon scheduled task, Startup .lnk, and any legacy .vbs.
func uninstallUserAutostart() (string, error) {
	var removed []string
	if exec.Command("schtasks", "/Delete", "/F", "/TN", windowsAutostartTaskName).Run() == nil {
		removed = append(removed, "TaskScheduler:"+windowsAutostartTaskName)
	}
	if startupDir, err := windowsStartupDir(); err == nil {
		for _, name := range []string{"Nektar.lnk", "nektar-daemon.vbs"} {
			p := filepath.Join(startupDir, name)
			if _, sErr := os.Stat(p); sErr == nil {
				if rErr := os.Remove(p); rErr == nil {
					removed = append(removed, p)
				}
			}
		}
	}
	return strings.Join(removed, ", "), nil
}

var (
	kernel32DLL         = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleOutCP = kernel32DLL.NewProc("GetConsoleOutputCP")
)

// formatConsoleText falls back to clean ASCII on Windows non-UTF8 consoles (e.g. cp1252 / redirected
// PowerShell 5.1 pipes) so startup banners never render mojibake like `â•­â”€`.
func formatConsoleText(s string) string {
	cp, _, _ := procGetConsoleOutCP.Call()
	if cp == 65001 {
		return s
	}
	r := strings.NewReplacer(
		"╭─", "+-",
		"╰─", "+-",
		"╭", "+",
		"╰", "+",
		"│", "|",
		"─", "-",
		"·", "*",
		"—", "--",
		"…", "...",
		"✓", "OK",
		"•", "*",
	)
	return r.Replace(s)
}
