package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// DefaultMaxFeeSatsPerTx is the canonical per-transaction miner fee ceiling shared by CLI and library callers.
	DefaultMaxFeeSatsPerTx uint64 = 10_000
	// DefaultMaxSpendSatsDay is the canonical UTC daily spend ceiling shared by CLI and library callers.
	DefaultMaxSpendSatsDay uint64 = 100_000
)

// SpendGuardOpts bounds every funded transaction an agent may broadcast.
type SpendGuardOpts struct {
	MaxFeeSatsPerTx uint64        // per-tx miner fee ceiling (0 = DefaultMaxFeeSatsPerTx)
	MaxSpendSatsDay uint64        // UTC daily spend ceiling (0 = DefaultMaxSpendSatsDay)
	MinInterval     time.Duration // minimum spacing between broadcasts
	StatePath       string        // optional persisted daily spend ledger JSON
	Now             func() time.Time
}

// SpendRecord logs one admitted spend for operational auditing.
type SpendRecord struct {
	TxID      string `json:"txid"`
	FeeSats   uint64 `json:"feeSats"`
	SpendSats uint64 `json:"spendSats"`
	Timestamp int64  `json:"timestamp"`
}

// SpendGuard is the mandatory fail-closed spend circuit breaker sitting in
// front of every live transaction broadcaster.
type SpendGuard struct {
	mu            sync.Mutex
	maxFeePerTx   uint64
	maxSpendDay   uint64
	minInterval   time.Duration
	statePath     string
	now           func() time.Time
	dayKey        string
	spentToday    uint64
	lastBroadcast time.Time
	records       []SpendRecord
}

type spendState struct {
	DayKey     string        `json:"dayKey"`
	SpentToday uint64        `json:"spentToday"`
	Records    []SpendRecord `json:"records,omitempty"`
}

func NewSpendGuard(opts SpendGuardOpts) *SpendGuard {
	maxFee := opts.MaxFeeSatsPerTx
	if maxFee == 0 {
		maxFee = DefaultMaxFeeSatsPerTx
	}
	maxDay := opts.MaxSpendSatsDay
	if maxDay == 0 {
		maxDay = DefaultMaxSpendSatsDay
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	g := &SpendGuard{
		maxFeePerTx: maxFee,
		maxSpendDay: maxDay,
		minInterval: opts.MinInterval,
		statePath:   opts.StatePath,
		now:         nowFn,
		dayKey:      nowFn().UTC().Format("2006-01-02"),
	}
	g.load()
	return g
}

func (g *SpendGuard) load() {
	if g.statePath == "" {
		return
	}
	b, err := os.ReadFile(g.statePath)
	if err != nil {
		return
	}
	var st spendState
	if json.Unmarshal(b, &st) == nil && st.DayKey == g.dayKey {
		g.spentToday = st.SpentToday
		g.records = st.Records
		if n := len(st.Records); n > 0 && st.Records[n-1].Timestamp > 0 {
			g.lastBroadcast = time.Unix(st.Records[n-1].Timestamp, 0)
		}
	}
}

func (g *SpendGuard) persistLocked() error {
	if g.statePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(g.statePath), 0o750); err != nil {
		return fmt.Errorf("spendguard: mkdir state dir: %w", err)
	}
	b, err := json.MarshalIndent(spendState{
		DayKey:     g.dayKey,
		SpentToday: g.spentToday,
		Records:    g.records,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("spendguard: marshal state: %w", err)
	}
	tmp := g.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("spendguard: write state: %w", err)
	}
	if err := os.Rename(tmp, g.statePath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("spendguard: commit state: %w", err)
	}
	return nil
}

func (g *SpendGuard) rollDayLocked(now time.Time) {
	today := now.UTC().Format("2006-01-02")
	if today != g.dayKey {
		g.dayKey = today
		g.spentToday = 0
		g.records = nil
	}
}

func (g *SpendGuard) checkLocked(now time.Time, feeSats, spendSats uint64) (uint64, error) {
	if feeSats > g.maxFeePerTx {
		return 0, fmt.Errorf("spendguard: tx fee %d sats exceeds per-tx cap %d sats", feeSats, g.maxFeePerTx)
	}
	totalSpend := spendSats
	if totalSpend < feeSats {
		totalSpend = feeSats
	}
	if g.spentToday+totalSpend > g.maxSpendDay {
		return 0, fmt.Errorf("spendguard: daily cap exceeded (%d + %d > %d sats)", g.spentToday, totalSpend, g.maxSpendDay)
	}
	if g.minInterval > 0 && !g.lastBroadcast.IsZero() && now.Sub(g.lastBroadcast) < g.minInterval {
		return 0, errors.New("spendguard: minimum broadcast interval not elapsed")
	}
	return totalSpend, nil
}

// Check validates caps without mutating spentToday (safe for pre-build fast-fail checks).
func (g *SpendGuard) Check(feeSats, spendSats uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.rollDayLocked(now)
	_, err := g.checkLocked(now, feeSats, spendSats)
	return err
}

// CheckAndRecord enforces fee cap, daily cap, and rate limit before recording a spend.
// Fails closed if persisting spend-guard.json fails, rolling back the in-memory delta.
func (g *SpendGuard) CheckAndRecord(txid string, feeSats, spendSats uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.rollDayLocked(now)

	// Idempotent on identical non-empty txid so a single tx is never double-charged across retries.
	if txid != "" && txid != "pending" && txid != "client-pending" && txid != "sweep-pending" {
		for _, r := range g.records {
			if r.TxID == txid {
				return nil
			}
		}
	}

	totalSpend, err := g.checkLocked(now, feeSats, spendSats)
	if err != nil {
		return err
	}

	prevSpent := g.spentToday
	prevLast := g.lastBroadcast
	prevLen := len(g.records)

	g.spentToday += totalSpend
	g.lastBroadcast = now
	g.records = append(g.records, SpendRecord{
		TxID:      txid,
		FeeSats:   feeSats,
		SpendSats: totalSpend,
		Timestamp: now.Unix(),
	})
	if err := g.persistLocked(); err != nil {
		g.spentToday = prevSpent
		g.lastBroadcast = prevLast
		g.records = g.records[:prevLen]
		return err
	}
	return nil
}

// Refund reverses a recorded spend ONLY on definitive pre-broadcast rejection (never on unknown timeout).
func (g *SpendGuard) Refund(txid string) {
	if txid == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollDayLocked(g.now())
	for i := len(g.records) - 1; i >= 0; i-- {
		if g.records[i].TxID == txid {
			if g.spentToday >= g.records[i].SpendSats {
				g.spentToday -= g.records[i].SpendSats
			} else {
				g.spentToday = 0
			}
			g.records = append(g.records[:i], g.records[i+1:]...)
			_ = g.persistLocked()
			return
		}
	}
}

// Stats returns current UTC day spend and caps.
func (g *SpendGuard) Stats() (spentToday, maxDay, maxFee uint64, count int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollDayLocked(g.now())
	return g.spentToday, g.maxSpendDay, g.maxFeePerTx, len(g.records)
}
