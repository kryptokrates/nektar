//go:build windows

package main

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

func isPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const processQueryLimitedInfo = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInfo, false, uint32(pid))
	if err != nil {
		return false
	}
	_ = syscall.CloseHandle(h)
	return true
}

func lockFileExclusive(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&ol,
	)
}

func unlockFileExclusive(f *os.File) error {
	var ol windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}

func terminatePIDGracefully(pid int) {
	if pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
