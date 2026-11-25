//go:build windows

package main

import "syscall"

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
