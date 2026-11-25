package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type lockFileRecord struct {
	PID       int    `json:"pid"`
	StartedAt int64  `json:"startedAt"`
	Listen    string `json:"listen,omitempty"`
}

// acquireInstanceLock prevents two `nektar serve` processes from racing on the same SQLite/wallet dataDir.
// Stale locks from dead processes are automatically reclaimed.
func acquireInstanceLock(dataDir, listenAddr string) (func(), error) {
	if dataDir == "" || dataDir == ":memory:" {
		return func() {}, nil
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("instance.lock mkdir: %w", err)
	}
	lockPath := filepath.Join(dataDir, "instance.lock")
	if raw, err := os.ReadFile(lockPath); err == nil && len(raw) > 0 {
		var prev lockFileRecord
		if json.Unmarshal(raw, &prev) == nil && prev.PID > 0 && prev.PID != os.Getpid() {
			if isPIDAlive(prev.PID) {
				return nil, fmt.Errorf("nektar: another daemon (pid=%d, listen=%s) is already running for %s — connect with `nektar mcp` or `nektar open`, or stop pid %d first",
					prev.PID, prev.Listen, dataDir, prev.PID)
			}
		}
		_ = os.Remove(lockPath)
	}

	rec := lockFileRecord{
		PID:       os.Getpid(),
		StartedAt: time.Now().Unix(),
		Listen:    listenAddr,
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("nektar: acquire %s: %w (another instance may be starting)", lockPath, err)
	}
	_, _ = f.Write(b)
	_ = f.Close()

	return func() {
		if raw, rErr := os.ReadFile(lockPath); rErr == nil {
			var cur lockFileRecord
			if json.Unmarshal(raw, &cur) == nil && cur.PID == os.Getpid() {
				_ = os.Remove(lockPath)
			}
		}
	}, nil
}
