//go:build !windows

package main

import (
	"os"
	"syscall"
)

func isPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func lockFileExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockFileExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func terminatePIDGracefully(pid int) {
	if pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Signal(syscall.SIGTERM)
	}
}
