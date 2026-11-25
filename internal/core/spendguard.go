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

// SpendGuardOpts bounds every funded transaction an agent may broadcast.
type SpendGuardOpts struct {
	MaxFeeSatsPerTx   uint64        // per-tx miner fee ceiling (0 = default 5000)
	MaxSpendSatsDay   uint64        // UTC daily spend ceiling (0 = default 100000)
	MinInterval       time.Duration // minimum spacing between broadcasts
	StatePath         string        // optional persisted daily spend ledger JSON
	Now               func() time.Time
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
	mu           sync.Mutex
	maxFeePerTx  uint64
	maxSpendDay  uint64
	minInterval  time.Duration
	statePath    string
	now          func() time.Time
	dayKey       string
	spentToday   uint64
	lastBroadcast time.Time
	records      []SpendRecord
}

type spendState struct {
	DayKey     string        `json:"dayKey"`
	SpentToday uint64        `json:"spentToday"`
	Records    []SpendRecord `json:"records,omitempty"`
}

func NewSpendGuard(opts SpendGuardOpts) *SpendGuard {
	maxFee := opts.MaxFeeSatsPerTx
	if maxFee == 0 {
		maxFee = 5_000
	}
	maxDay := opts.MaxSpendSatsDay
	if maxDay == 0 {
		maxDay = 100_000
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
	}
}

func (g *SpendGuard) persistLocked() {
	if g.statePath == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(g.statePath), 0o750)
	b, err := json.MarshalIndent(spendState{
		DayKey:     g.dayKey,
		SpentToday: g.spentToday,
		Records:    g.records,
	}, "", "  ")
	if err != nil {
		return
	}
	tmp := g.statePath + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, g.statePath)
	}
}

func (g *SpendGuard) rollDayLocked(now time.Time) {
	today := now.UTC().Format("2006-01-02")
	if today != g.dayKey {
		g.dayKey = today
		g.spentToday = 0
		g.records = nil
	}
}

// CheckAndRecord enforces fee cap, daily cap, and rate limit before recording a spend.
func (g *SpendGuard) CheckAndRecord(txid string, feeSats, spendSats uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.rollDayLocked(now)

	if feeSats > g.maxFeePerTx {
		return fmt.Errorf("spendguard: tx fee %d sats exceeds per-tx cap %d sats", feeSats, g.maxFeePerTx)
	}
	totalSpend := spendSats
	if totalSpend < feeSats {
		totalSpend = feeSats
	}
	if g.spentToday+totalSpend > g.maxSpendDay {
		return fmt.Errorf("spendguard: daily cap exceeded (%d + %d > %d sats)", g.spentToday, totalSpend, g.maxSpendDay)
	}
	if g.minInterval > 0 && !g.lastBroadcast.IsZero() && now.Sub(g.lastBroadcast) < g.minInterval {
		return errors.New("spendguard: minimum broadcast interval not elapsed")
	}

	g.spentToday += totalSpend
	g.lastBroadcast = now
	g.records = append(g.records, SpendRecord{
		TxID:      txid,
		FeeSats:   feeSats,
		SpendSats: totalSpend,
		Timestamp: now.Unix(),
	})
	g.persistLocked()
	return nil
}

// Stats returns current UTC day spend and caps.
func (g *SpendGuard) Stats() (spentToday, maxDay, maxFee uint64, count int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollDayLocked(g.now())
	return g.spentToday, g.maxSpendDay, g.maxFeePerTx, len(g.records)
}
