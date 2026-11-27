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

// Lock a high sentinel byte offset outside the JSON metadata bytes so sibling processes
// on Windows can still read PID/listen info from instance.lock without hitting ERROR_LOCK_VIOLATION.
const windowsLockSentinelOffset uint32 = 0x7FFF0000

func lockFileExclusive(f *os.File) error {
	ol := windows.Overlapped{Offset: windowsLockSentinelOffset}
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
	ol := windows.Overlapped{Offset: windowsLockSentinelOffset}
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
