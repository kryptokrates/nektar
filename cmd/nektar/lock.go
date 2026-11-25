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

// acquireInstanceLock acquires a kernel-managed non-blocking OS advisory lock (flock / LockFileEx)
// on <dataDir>/instance.lock so concurrent processes cannot race on SQLite/wallet/Restate data,
// and the OS automatically releases the lock on SIGKILL/crash with zero stale-file TOCTOU race.
func acquireInstanceLock(dataDir, listenAddr string) (func(), error) {
	if dataDir == "" || dataDir == ":memory:" {
		return func() {}, nil
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("instance.lock mkdir: %w", err)
	}
	lockPath := filepath.Join(dataDir, "instance.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("nektar: open %s: %w", lockPath, err)
	}
	if err := lockFileExclusive(f); err != nil {
		var prev lockFileRecord
		if raw, rErr := os.ReadFile(lockPath); rErr == nil {
			_ = json.Unmarshal(raw, &prev)
		}
		_ = f.Close()
		if prev.PID > 0 {
			return nil, fmt.Errorf("nektar: another daemon (pid=%d, listen=%s) is already running for %s — connect with `nektar mcp` or `nektar open`, or stop pid %d first",
				prev.PID, prev.Listen, dataDir, prev.PID)
		}
		return nil, fmt.Errorf("nektar: another instance already holds %s (%w)", lockPath, err)
	}

	rec := lockFileRecord{
		PID:       os.Getpid(),
		StartedAt: time.Now().Unix(),
		Listen:    listenAddr,
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = f.Write(b)
	_ = f.Sync()

	return func() {
		_ = unlockFileExclusive(f)
		_ = f.Close()
	}, nil
}
