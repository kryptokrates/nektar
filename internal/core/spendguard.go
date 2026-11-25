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

var (
	// ErrFeeCapExceeded indicates a transaction fee exceeds SpendGuard's per-tx ceiling (terminal for that tx).
	ErrFeeCapExceeded = errors.New("spendguard: per-tx fee cap exceeded")
	// ErrDailyCapExceeded indicates SpendGuard's UTC daily cap would be exceeded (deferrable until UTC midnight).
	ErrDailyCapExceeded = errors.New("spendguard: daily spend cap exceeded")
	// ErrMinInterval indicates SpendGuard's minimum broadcast interval has not elapsed (deferrable).
	ErrMinInterval = errors.New("spendguard: minimum broadcast interval not elapsed")
	// ErrSpendStateCorrupt indicates an existing spend-guard.json failed to read or parse (fail-closed).
	ErrSpendStateCorrupt = errors.New("spendguard: persisted state file corrupt or unreadable")
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
	reservedToday uint64
	lastBroadcast time.Time
	records       []SpendRecord
	txIndex       map[string]struct{}
	reservations  map[string]uint64
	resSeq        uint64
	corruptErr    error
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
		maxFeePerTx:  maxFee,
		maxSpendDay:  maxDay,
		minInterval:  opts.MinInterval,
		statePath:    opts.StatePath,
		now:          nowFn,
		dayKey:       nowFn().UTC().Format("2006-01-02"),
		txIndex:      make(map[string]struct{}),
		reservations: make(map[string]uint64),
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
		if os.IsNotExist(err) {
			return
		}
		// Fail closed: an existing unreadable spend-guard.json must block all spends until repaired.
		g.corruptErr = fmt.Errorf("%w: read %s: %v", ErrSpendStateCorrupt, g.statePath, err)
		return
	}
	if len(b) == 0 {
		g.corruptErr = fmt.Errorf("%w: %s is empty", ErrSpendStateCorrupt, g.statePath)
		return
	}
	var st spendState
	if err := json.Unmarshal(b, &st); err != nil {
		g.corruptErr = fmt.Errorf("%w: parse %s: %v", ErrSpendStateCorrupt, g.statePath, err)
		return
	}
	if st.DayKey == g.dayKey {
		g.spentToday = st.SpentToday
		g.records = st.Records
		for _, r := range st.Records {
			if r.TxID != "" {
				g.txIndex[r.TxID] = struct{}{}
			}
		}
		if n := len(st.Records); n > 0 && st.Records[n-1].Timestamp > 0 {
			g.lastBroadcast = time.Unix(st.Records[n-1].Timestamp, 0)
		}
	}
}

func (g *SpendGuard) persistLocked() error {
	if g.statePath == "" {
		return nil
	}
	dir := filepath.Dir(g.statePath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
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
	tmpFile, err := os.CreateTemp(dir, "spend-guard-*.tmp")
	if err != nil {
		return fmt.Errorf("spendguard: create temp state: %w", err)
	}
	tmpPath := tmpFile.Name()
	_, wErr := tmpFile.Write(b)
	sErr := tmpFile.Sync()
	cErr := tmpFile.Close()
	if wErr != nil || sErr != nil || cErr != nil {
		_ = os.Remove(tmpPath)
		if wErr != nil {
			return fmt.Errorf("spendguard: write state: %w", wErr)
		}
		if sErr != nil {
			return fmt.Errorf("spendguard: sync state: %w", sErr)
		}
		return fmt.Errorf("spendguard: close state: %w", cErr)
	}
	_ = os.Chmod(tmpPath, 0o600)
	if err := os.Rename(tmpPath, g.statePath); err != nil {
		_ = os.Remove(tmpPath)
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
		clear(g.txIndex)
	}
}

func (g *SpendGuard) checkLocked(now time.Time, feeSats, spendSats uint64) (uint64, error) {
	if g.corruptErr != nil {
		return 0, g.corruptErr
	}
	if feeSats > g.maxFeePerTx {
		return 0, fmt.Errorf("%w: tx fee %d sats exceeds per-tx cap %d sats", ErrFeeCapExceeded, feeSats, g.maxFeePerTx)
	}
	totalSpend := spendSats
	if totalSpend < feeSats {
		totalSpend = feeSats
	}
	if g.spentToday+g.reservedToday+totalSpend > g.maxSpendDay {
		return 0, fmt.Errorf("%w: (%d + %d + %d > %d sats)", ErrDailyCapExceeded, g.spentToday, g.reservedToday, totalSpend, g.maxSpendDay)
	}
	if g.minInterval > 0 && !g.lastBroadcast.IsZero() && now.Sub(g.lastBroadcast) < g.minInterval {
		return 0, ErrMinInterval
	}
	return totalSpend, nil
}

// Reserve atomically reserves daily spend budget before a remote client-wallet CreateAction RPC,
// eliminating concurrent TOCTOU races across overlapping sends.
func (g *SpendGuard) Reserve(feeSats, spendSats uint64) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.rollDayLocked(now)
	totalSpend, err := g.checkLocked(now, feeSats, spendSats)
	if err != nil {
		return "", err
	}
	g.resSeq++
	id := fmt.Sprintf("res-%d", g.resSeq)
	g.reservations[id] = totalSpend
	g.reservedToday += totalSpend
	return id, nil
}

// CancelReservation releases a pending pre-broadcast reservation on definite RPC failure.
func (g *SpendGuard) CancelReservation(resID string) {
	if resID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if amt, ok := g.reservations[resID]; ok {
		delete(g.reservations, resID)
		if g.reservedToday >= amt {
			g.reservedToday -= amt
		} else {
			g.reservedToday = 0
		}
	}
}

// CommitReservation finalizes an already-broadcast transaction against spentToday unconditionally
// (never dropping actual on-chain spend if actualFee exceeded the pre-check estimate).
func (g *SpendGuard) CommitReservation(resID, txid string, actualFee, actualSpend uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if amt, ok := g.reservations[resID]; ok {
		delete(g.reservations, resID)
		if g.reservedToday >= amt {
			g.reservedToday -= amt
		} else {
			g.reservedToday = 0
		}
	}
	now := g.now()
	g.rollDayLocked(now)
	if txid != "" {
		if _, dup := g.txIndex[txid]; dup {
			return nil
		}
	}
	totalSpend := actualSpend
	if totalSpend < actualFee {
		totalSpend = actualFee
	}
	g.spentToday += totalSpend
	g.lastBroadcast = now
	g.records = append(g.records, SpendRecord{
		TxID:      txid,
		FeeSats:   actualFee,
		SpendSats: totalSpend,
		Timestamp: now.Unix(),
	})
	if txid != "" {
		g.txIndex[txid] = struct{}{}
	}
	return g.persistLocked()
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

	// Idempotent on identical non-empty txid so a single tx is never double-charged across retries (O(1) map check).
	if txid != "" && txid != "pending" && txid != "client-pending" && txid != "sweep-pending" {
		if _, dup := g.txIndex[txid]; dup {
			return nil
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
	if txid != "" {
		g.txIndex[txid] = struct{}{}
	}
	if err := g.persistLocked(); err != nil {
		g.spentToday = prevSpent
		g.lastBroadcast = prevLast
		g.records = g.records[:prevLen]
		if txid != "" {
			delete(g.txIndex, txid)
		}
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
			delete(g.txIndex, txid)
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
