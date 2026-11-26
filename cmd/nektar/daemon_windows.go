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

// installUserAutostart writes a silent Windows Startup VBS launcher in the user's Startup folder
// (no admin privileges required) so Nektar starts automatically on sign-in.
func installUserAutostart(exePath string, serveArgs []string, _ string) (string, error) {
	appData := envOr("APPDATA", "")
	if appData == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		appData = filepath.Join(home, "AppData", "Roaming")
	}
	startupDir := filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup")
	if err := os.MkdirAll(startupDir, 0o755); err != nil {
		return "", err
	}
	vbsPath := filepath.Join(startupDir, "nektar-daemon.vbs")
	var parts []string
	parts = append(parts, `""`+strings.ReplaceAll(exePath, `"`, `""`)+`""`)
	for _, a := range serveArgs {
		if strings.ContainsAny(a, " \t\"") {
			parts = append(parts, `""`+strings.ReplaceAll(a, `"`, `""`)+`""`)
		} else {
			parts = append(parts, a)
		}
	}
	cmdLine := strings.Join(parts, " ")
	vbs := fmt.Sprintf("Set WshShell = CreateObject(\"WScript.Shell\")\r\nWshShell.Run \"%s\", 0, False\r\n", cmdLine)
	if err := os.WriteFile(vbsPath, []byte(vbs), 0o644); err != nil {
		return "", err
	}
	return vbsPath, nil
}
