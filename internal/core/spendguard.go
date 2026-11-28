package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// ErrDailyCapExceeded indicates SpendGuard's UTC daily spend ceiling would be exceeded (deferrable until UTC midnight).
	ErrDailyCapExceeded = errors.New("spendguard: daily spend cap exceeded")
	// ErrMinInterval indicates SpendGuard's minimum broadcast interval has not elapsed (deferrable).
	ErrMinInterval = errors.New("spendguard: minimum broadcast interval not elapsed")
	// ErrSpendStateCorrupt indicates an existing spend-guard.json failed to read or parse (fail-closed).
	ErrSpendStateCorrupt = errors.New("spendguard: persisted state file corrupt or unreadable")
)

// SpendJournal is an optional high-throughput persistent journal backend (e.g. Pebble WAL)
// for SpendGuard, avoiding per-tx NTFS file open/write/close syscalls while keeping full
// fail-closed crash recovery and daily cap enforcement.
type SpendJournal interface {
	Load() ([][]byte, error)
	Append(line []byte) error
	Compact(lines [][]byte) error
}

// SpendGuardOpts bounds every funded transaction an agent may broadcast.
type SpendGuardOpts struct {
	MaxFeeSatsPerTx uint64        // per-tx miner fee ceiling (0 = DefaultMaxFeeSatsPerTx)
	MaxSpendSatsDay uint64        // UTC daily spend ceiling (0 = NEKTAR_MAX_SPEND_SATS_DAY or DefaultMaxSpendSatsDay)
	MinInterval     time.Duration // minimum spacing between broadcasts
	StatePath       string        // optional persisted daily spend ledger (append-only NDJSON, legacy JSON compatible)
	Journal         SpendJournal  // optional fast WAL journal (takes precedence over StatePath when set)
	Now             func() time.Time
}

// SpendRecord logs one admitted spend for operational auditing.
type SpendRecord struct {
	TxID      string `json:"txid"`
	FeeSats   uint64 `json:"feeSats"`
	SpendSats uint64 `json:"spendSats"`
	Timestamp int64  `json:"timestamp"`
}

type spendReservation struct {
	amount    uint64
	dayKey    string
	createdAt time.Time
}

func addSaturatingUint64(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

type persistedReservation struct {
	Amount    uint64 `json:"amount"`
	DayKey    string `json:"dayKey"`
	CreatedAt int64  `json:"createdAt"`
}

// spendState is retained for reading legacy v0.1 whole-file JSON snapshots seamlessly.
type spendState struct {
	DayKey       string                          `json:"dayKey"`
	SpentToday   uint64                          `json:"spentToday"`
	Records      []SpendRecord                   `json:"records,omitempty"`
	Reservations map[string]persistedReservation `json:"reservations,omitempty"`
	ResSeq       uint64                          `json:"resSeq,omitempty"`
}

// spendLogEntry is an O(1) append-only NDJSON journal record eliminating O(N^2) full-day JSON rewrites.
type spendLogEntry struct {
	Version   int    `json:"v,omitempty"`
	Op        string `json:"op"` // "res" | "cancel" | "commit" | "rec" | "refund" | "roll"
	Day       string `json:"day"`
	ResID     string `json:"resId,omitempty"`
	ResSeq    uint64 `json:"seq,omitempty"`
	TxID      string `json:"txid,omitempty"`
	FeeSats   uint64 `json:"fee,omitempty"`
	SpendSats uint64 `json:"sats,omitempty"`
	TS        int64  `json:"ts,omitempty"`
}

// SpendGuard is the mandatory fail-closed spend circuit breaker sitting in
// front of every live transaction broadcaster.
type SpendGuard struct {
	mu            sync.Mutex
	maxFeePerTx   uint64
	maxSpendDay   uint64
	minInterval   time.Duration
	statePath     string
	journal       SpendJournal
	logFile       *os.File
	now           func() time.Time
	dayKey        string
	spentToday    uint64
	reservedToday uint64
	lastBroadcast time.Time
	txSpend       map[string]uint64
	txCount       int
	reservations  map[string]spendReservation
	resSeq        uint64
	needsCompact  bool
	usesWallClock bool
	corruptErr    error
	preCheck      atomic.Pointer[func() error]
}

// SetPreCheck registers an optional fail-closed external pre-spend gate (e.g. autonomy Watchdog
// clock-skew & circuit-breaker guard) checked before Reserve, Check, and CheckAndRecord.
func (g *SpendGuard) SetPreCheck(fn func() error) {
	if g == nil {
		return
	}
	if fn == nil {
		g.preCheck.Store(nil)
		return
	}
	g.preCheck.Store(&fn)
}

func (g *SpendGuard) runPreCheck() error {
	if g == nil {
		return nil
	}
	if p := g.preCheck.Load(); p != nil && *p != nil {
		return (*p)()
	}
	return nil
}

func NewSpendGuard(opts SpendGuardOpts) *SpendGuard {
	maxFee := opts.MaxFeeSatsPerTx
	if maxFee == 0 {
		maxFee = DefaultMaxFeeSatsPerTx
	}
	maxDay := opts.MaxSpendSatsDay
	if maxDay == 0 {
		if env := strings.TrimSpace(os.Getenv("NEKTAR_MAX_SPEND_SATS_DAY")); env != "" {
			if parsed, err := strconv.ParseUint(env, 10, 64); err == nil && parsed > 0 {
				maxDay = parsed
			}
		}
		if maxDay == 0 {
			maxDay = DefaultMaxSpendSatsDay
		}
	}
	nowFn := opts.Now
	usesWall := false
	if nowFn == nil {
		nowFn = time.Now
		usesWall = true
	}
	g := &SpendGuard{
		maxFeePerTx:   maxFee,
		maxSpendDay:   maxDay,
		minInterval:   opts.MinInterval,
		statePath:     opts.StatePath,
		journal:       opts.Journal,
		now:           nowFn,
		usesWallClock: usesWall,
		dayKey:        nowFn().UTC().Format("2006-01-02"),
		txSpend:       make(map[string]uint64, 256),
		reservations:  make(map[string]spendReservation, 64),
	}
	g.load()
	return g
}

func (g *SpendGuard) load() {
	if g.journal != nil {
		lines, err := g.journal.Load()
		if err != nil {
			g.corruptErr = fmt.Errorf("%w: load journal: %v", ErrSpendStateCorrupt, err)
			return
		}
		if len(lines) == 0 {
			return
		}
		g.loadNDJSON(bytes.Join(lines, []byte{'\n'}))
		return
	}
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
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		g.corruptErr = fmt.Errorf("%w: %s is empty", ErrSpendStateCorrupt, g.statePath)
		return
	}

	// Probe whether this is a legacy multi-line indented spendState JSON object vs NDJSON journal.
	var probe struct {
		Op     string  `json:"op"`
		DayKey *string `json:"dayKey"`
	}
	firstLine := trimmed
	if nl := bytes.IndexByte(trimmed, '\n'); nl >= 0 {
		firstLine = bytes.TrimSpace(trimmed[:nl])
	}
	if json.Unmarshal(firstLine, &probe) == nil && probe.Op != "" {
		g.loadNDJSON(trimmed)
		return
	}

	var st spendState
	if err := json.Unmarshal(trimmed, &st); err != nil || st.DayKey == "" {
		g.corruptErr = fmt.Errorf("%w: parse %s: %v", ErrSpendStateCorrupt, g.statePath, err)
		return
	}
	if st.ResSeq > g.resSeq {
		g.resSeq = st.ResSeq
	}
	if st.DayKey == g.dayKey {
		g.spentToday = st.SpentToday
		g.txCount = len(st.Records)
		for _, r := range st.Records {
			if r.TxID != "" {
				g.txSpend[r.TxID] = r.SpendSats
			}
		}
		if n := len(st.Records); n > 0 && st.Records[n-1].Timestamp > 0 {
			g.lastBroadcast = time.Unix(st.Records[n-1].Timestamp, 0)
		}
		for id, pr := range st.Reservations {
			if pr.DayKey != g.dayKey || pr.Amount == 0 {
				continue
			}
			g.reservations[id] = spendReservation{
				amount:    pr.Amount,
				dayKey:    pr.DayKey,
				createdAt: time.Unix(pr.CreatedAt, 0),
			}
			g.reservedToday = addSaturatingUint64(g.reservedToday, pr.Amount)
		}
	}
	// Migrate legacy snapshot cleanly on next write so we never append NDJSON onto multi-line JSON.
	g.needsCompact = true
}

func (g *SpendGuard) loadNDJSON(trimmed []byte) {
	lines := bytes.Split(trimmed, []byte{'\n'})
	for _, raw := range lines {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var e spendLogEntry
		if err := json.Unmarshal(line, &e); err != nil || e.Op == "" {
			g.corruptErr = fmt.Errorf("%w: parse journal line in %s: %v", ErrSpendStateCorrupt, g.statePath, err)
			return
		}
		if e.ResSeq > g.resSeq {
			g.resSeq = e.ResSeq
		}
		if e.Day != g.dayKey {
			g.needsCompact = true
			continue
		}
		switch e.Op {
		case "roll":
			// Explicit fresh day header.
		case "res":
			if e.ResID != "" && e.SpendSats > 0 {
				g.reservations[e.ResID] = spendReservation{
					amount:    e.SpendSats,
					dayKey:    e.Day,
					createdAt: time.Unix(e.TS, 0),
				}
				g.reservedToday = addSaturatingUint64(g.reservedToday, e.SpendSats)
			}
		case "cancel":
			if res, ok := g.reservations[e.ResID]; ok {
				delete(g.reservations, e.ResID)
				if g.reservedToday >= res.amount {
					g.reservedToday -= res.amount
				} else {
					g.reservedToday = 0
				}
			}
		case "commit":
			if res, ok := g.reservations[e.ResID]; ok {
				delete(g.reservations, e.ResID)
				if g.reservedToday >= res.amount {
					g.reservedToday -= res.amount
				} else {
					g.reservedToday = 0
				}
			}
			if e.TxID != "" {
				if _, dup := g.txSpend[e.TxID]; dup {
					continue
				}
				g.txSpend[e.TxID] = e.SpendSats
			}
			g.spentToday = addSaturatingUint64(g.spentToday, e.SpendSats)
			g.txCount++
			if e.TS > 0 {
				g.lastBroadcast = time.Unix(e.TS, 0)
			}
		case "rec":
			isNamed := e.TxID != "" && e.TxID != "pending" && e.TxID != "client-pending" && e.TxID != "sweep-pending"
			if isNamed {
				if _, dup := g.txSpend[e.TxID]; dup {
					continue
				}
				g.txSpend[e.TxID] = e.SpendSats
			} else if e.TxID != "" {
				g.txSpend[e.TxID] = e.SpendSats
			}
			g.spentToday = addSaturatingUint64(g.spentToday, e.SpendSats)
			g.txCount++
			if e.TS > 0 {
				g.lastBroadcast = time.Unix(e.TS, 0)
			}
		case "refund":
			if e.TxID != "" {
				if amt, ok := g.txSpend[e.TxID]; ok {
					delete(g.txSpend, e.TxID)
					if g.spentToday >= amt {
						g.spentToday -= amt
					} else {
						g.spentToday = 0
					}
					if g.txCount > 0 {
						g.txCount--
					}
				}
			}
		default:
			g.corruptErr = fmt.Errorf("%w: unknown journal op %q in %s", ErrSpendStateCorrupt, e.Op, g.statePath)
			return
		}
	}
}

func (g *SpendGuard) appendEntryLocked(entry spendLogEntry) error {
	if g.journal != nil {
		if g.needsCompact {
			if err := g.compactLocked(entry); err != nil {
				return err
			}
			g.needsCompact = false
			return nil
		}
		line, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("spendguard: marshal journal entry: %w", err)
		}
		if err := g.journal.Append(line); err != nil {
			return fmt.Errorf("spendguard: append journal: %w", err)
		}
		return nil
	}
	if g.statePath == "" {
		return nil
	}
	if g.needsCompact {
		if g.logFile != nil {
			_ = g.logFile.Close()
			g.logFile = nil
		}
		dir := filepath.Dir(g.statePath)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("spendguard: mkdir state dir: %w", err)
		}
		if err := g.compactLocked(entry); err != nil {
			return err
		}
		g.needsCompact = false
		return nil
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("spendguard: marshal journal entry: %w", err)
	}
	line = append(line, '\n')
	if g.logFile == nil {
		dir := filepath.Dir(g.statePath)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("spendguard: mkdir state dir: %w", err)
		}
		f, err := os.OpenFile(g.statePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("spendguard: open journal: %w", err)
		}
		g.logFile = f
	}
	if _, wErr := g.logFile.Write(line); wErr != nil {
		_ = g.logFile.Close()
		g.logFile = nil
		return fmt.Errorf("spendguard: write journal: %w", wErr)
	}
	if os.Getenv("NEKTAR_SPENDGUARD_FSYNC") == "1" {
		if sErr := g.logFile.Sync(); sErr != nil {
			return fmt.Errorf("spendguard: sync journal: %w", sErr)
		}
	}
	if runtime.GOOS == "windows" {
		_ = g.logFile.Close()
		g.logFile = nil
	}
	return nil
}

// Close flushes and closes the persistent append-only spend-guard journal file handle.
func (g *SpendGuard) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.logFile != nil {
		err := g.logFile.Close()
		g.logFile = nil
		return err
	}
	return nil
}

// compactLocked writes a clean NDJSON day checkpoint (only executed once on UTC day rollover or legacy file upgrade).
func (g *SpendGuard) compactLocked(_ spendLogEntry) error {
	if g.logFile != nil {
		_ = g.logFile.Close()
		g.logFile = nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	_ = enc.Encode(spendLogEntry{Op: "roll", Day: g.dayKey, ResSeq: g.resSeq})
	var accounted uint64
	var tsUnix int64
	if !g.lastBroadcast.IsZero() {
		tsUnix = g.lastBroadcast.Unix()
	}
	recorded := 0
	for txid, amt := range g.txSpend {
		_ = enc.Encode(spendLogEntry{Op: "rec", Day: g.dayKey, TxID: txid, SpendSats: amt, TS: tsUnix})
		accounted = addSaturatingUint64(accounted, amt)
		recorded++
	}
	for recorded < g.txCount {
		rem := uint64(0)
		if g.spentToday > accounted && recorded == g.txCount-1 {
			rem = g.spentToday - accounted
		}
		_ = enc.Encode(spendLogEntry{Op: "rec", Day: g.dayKey, SpendSats: rem, TS: tsUnix})
		accounted = addSaturatingUint64(accounted, rem)
		recorded++
	}
	for id, res := range g.reservations {
		if res.dayKey == g.dayKey && res.amount > 0 {
			_ = enc.Encode(spendLogEntry{
				Op:        "res",
				Day:       g.dayKey,
				ResID:     id,
				SpendSats: res.amount,
				TS:        res.createdAt.Unix(),
			})
		}
	}
	if g.journal != nil {
		rawLines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte{'\n'})
		lines := make([][]byte, 0, len(rawLines))
		for _, ln := range rawLines {
			ln = bytes.TrimSpace(ln)
			if len(ln) > 0 {
				lines = append(lines, ln)
			}
		}
		if err := g.journal.Compact(lines); err != nil {
			return fmt.Errorf("spendguard: compact journal: %w", err)
		}
		return nil
	}
	dir := filepath.Dir(g.statePath)
	tmpFile, err := os.CreateTemp(dir, "spend-guard-*.tmp")
	if err != nil {
		return fmt.Errorf("spendguard: create temp journal: %w", err)
	}
	tmpPath := tmpFile.Name()
	_, wErr := tmpFile.Write(buf.Bytes())
	cErr := tmpFile.Close()
	if wErr != nil || cErr != nil {
		_ = os.Remove(tmpPath)
		if wErr != nil {
			return fmt.Errorf("spendguard: write temp journal: %w", wErr)
		}
		return fmt.Errorf("spendguard: close temp journal: %w", cErr)
	}
	_ = os.Chmod(tmpPath, 0o600)
	if err := os.Rename(tmpPath, g.statePath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("spendguard: commit compacted journal: %w", err)
	}
	return nil
}

func (g *SpendGuard) rollDayLocked(now time.Time) {
	today := now.UTC().Format("2006-01-02")
	// DDIA Ch. 8 monotonic UTC day floor: ISO-8601 dates sort lexicographically, so a backward
	// NTP clock step across UTC midnight MUST NOT reset spentToday or reservedToday.
	if today > g.dayKey {
		g.dayKey = today
		g.spentToday = 0
		g.txCount = 0
		clear(g.txSpend)
		var activeReserved uint64
		for id, res := range g.reservations {
			if res.dayKey != today {
				delete(g.reservations, id)
				continue
			}
			activeReserved = addSaturatingUint64(activeReserved, res.amount)
		}
		g.reservedToday = activeReserved
		g.needsCompact = true
	}
}

func (g *SpendGuard) effectiveLastBroadcastLocked() time.Time {
	last := g.lastBroadcast
	if g.minInterval > 0 {
		for _, res := range g.reservations {
			if res.createdAt.After(last) {
				last = res.createdAt
			}
		}
	}
	return last
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
	if g.spentToday > g.maxSpendDay ||
		g.reservedToday > g.maxSpendDay-g.spentToday ||
		totalSpend > g.maxSpendDay-g.spentToday-g.reservedToday {
		return 0, fmt.Errorf("%w: (spent=%d + reserved=%d [%d active] + request=%d > cap=%d sats)", ErrDailyCapExceeded, g.spentToday, g.reservedToday, len(g.reservations), totalSpend, g.maxSpendDay)
	}
	if g.minInterval > 0 {
		lastActivity := g.effectiveLastBroadcastLocked()
		if !lastActivity.IsZero() {
			// Guard against backward wall-clock steps (e.g. NTP adjustment after restoring a disk timestamp):
			// negative elapsed duration MUST NOT deadlock broadcasts indefinitely (DDIA Ch. 8).
			if elapsed := now.Sub(lastActivity); elapsed >= 0 && elapsed < g.minInterval {
				rem := (g.minInterval - elapsed).Round(time.Millisecond)
				return 0, fmt.Errorf("%w (retry after %s)", ErrMinInterval, rem)
			}
		}
	}
	return totalSpend, nil
}

func (g *SpendGuard) paceMinIntervalLocked(now *time.Time) {
	if !g.usesWallClock || g.minInterval <= 0 || g.minInterval > 250*time.Millisecond {
		return
	}
	for attempt := 0; attempt < 4; attempt++ {
		lastActivity := g.effectiveLastBroadcastLocked()
		if lastActivity.IsZero() {
			return
		}
		elapsed := now.Sub(lastActivity)
		if elapsed < 0 || elapsed >= g.minInterval {
			return
		}
		wait := g.minInterval - elapsed
		g.mu.Unlock()
		time.Sleep(wait)
		g.mu.Lock()
		*now = g.now()
		g.rollDayLocked(*now)
	}
}

// Reserve atomically reserves daily spend budget before a remote client-wallet CreateAction RPC,
// eliminating concurrent TOCTOU races across overlapping sends and enforcing MinInterval spacing.
func (g *SpendGuard) Reserve(feeSats, spendSats uint64) (string, error) {
	if err := g.runPreCheck(); err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.rollDayLocked(now)
	g.paceMinIntervalLocked(&now)
	totalSpend, err := g.checkLocked(now, feeSats, spendSats)
	if err != nil {
		return "", err
	}
	g.resSeq++
	id := fmt.Sprintf("res-%d", g.resSeq)
	prevReserved := g.reservedToday
	g.reservations[id] = spendReservation{amount: totalSpend, dayKey: g.dayKey, createdAt: now}
	g.reservedToday = addSaturatingUint64(g.reservedToday, totalSpend)
	if err := g.appendEntryLocked(spendLogEntry{
		Op:        "res",
		Day:       g.dayKey,
		ResID:     id,
		ResSeq:    g.resSeq,
		FeeSats:   feeSats,
		SpendSats: totalSpend,
		TS:        now.Unix(),
	}); err != nil {
		delete(g.reservations, id)
		g.reservedToday = prevReserved
		return "", err
	}
	return id, nil
}

// CancelReservation releases a pending pre-broadcast reservation on definite RPC failure.
func (g *SpendGuard) CancelReservation(resID string) {
	if resID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if res, ok := g.reservations[resID]; ok {
		delete(g.reservations, resID)
		if g.reservedToday >= res.amount {
			g.reservedToday -= res.amount
		} else {
			g.reservedToday = 0
		}
		_ = g.appendEntryLocked(spendLogEntry{
			Op:    "cancel",
			Day:   g.dayKey,
			ResID: resID,
		})
	}
}

// CommitReservation finalizes an already-broadcast transaction against spentToday unconditionally
// (never dropping actual on-chain spend if actualFee exceeded the pre-check estimate).
func (g *SpendGuard) CommitReservation(resID, txid string, actualFee, actualSpend uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if res, ok := g.reservations[resID]; ok {
		delete(g.reservations, resID)
		if g.reservedToday >= res.amount {
			g.reservedToday -= res.amount
		} else {
			g.reservedToday = 0
		}
	}
	now := g.now()
	g.rollDayLocked(now)
	if txid != "" {
		if _, dup := g.txSpend[txid]; dup {
			return nil
		}
	}
	totalSpend := actualSpend
	if totalSpend < actualFee {
		totalSpend = actualFee
	}
	g.spentToday = addSaturatingUint64(g.spentToday, totalSpend)
	g.lastBroadcast = now
	g.txCount++
	if txid != "" {
		g.txSpend[txid] = totalSpend
	}
	return g.appendEntryLocked(spendLogEntry{
		Op:        "commit",
		Day:       g.dayKey,
		ResID:     resID,
		TxID:      txid,
		FeeSats:   actualFee,
		SpendSats: totalSpend,
		TS:        now.Unix(),
	})
}

// Check validates caps without mutating spentToday (safe for pre-build fast-fail checks).
func (g *SpendGuard) Check(feeSats, spendSats uint64) error {
	if err := g.runPreCheck(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.rollDayLocked(now)
	_, err := g.checkLocked(now, feeSats, spendSats)
	return err
}

// CheckAndRecord enforces fee cap, daily cap, and rate limit before recording a spend.
// Fails closed if persisting the journal entry fails, rolling back the in-memory delta.
func (g *SpendGuard) CheckAndRecord(txid string, feeSats, spendSats uint64) error {
	if err := g.runPreCheck(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.rollDayLocked(now)

	isNamed := txid != "" && txid != "pending" && txid != "client-pending" && txid != "sweep-pending"
	if isNamed {
		if _, dup := g.txSpend[txid]; dup {
			return nil
		}
	}

	g.paceMinIntervalLocked(&now)
	totalSpend, err := g.checkLocked(now, feeSats, spendSats)
	if err != nil {
		return err
	}

	prevSpent := g.spentToday
	prevLast := g.lastBroadcast
	prevCount := g.txCount
	prevHadTx := false
	var prevTxAmt uint64
	if txid != "" {
		prevTxAmt, prevHadTx = g.txSpend[txid]
	}

	g.spentToday = addSaturatingUint64(g.spentToday, totalSpend)
	g.lastBroadcast = now
	g.txCount++
	if txid != "" {
		g.txSpend[txid] = totalSpend
	}
	if err := g.appendEntryLocked(spendLogEntry{
		Op:        "rec",
		Day:       g.dayKey,
		TxID:      txid,
		FeeSats:   feeSats,
		SpendSats: totalSpend,
		TS:        now.Unix(),
	}); err != nil {
		g.spentToday = prevSpent
		g.lastBroadcast = prevLast
		g.txCount = prevCount
		if txid != "" {
			if prevHadTx {
				g.txSpend[txid] = prevTxAmt
			} else {
				delete(g.txSpend, txid)
			}
		}
		return err
	}
	return nil
}

// Refund reverses a recorded spend ONLY on definitive pre-broadcast refusal (never on post-submit
// error strings or unknown timeouts) and propagates any persistence failure.
func (g *SpendGuard) Refund(txid string) error {
	if txid == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.rollDayLocked(now)
	amt, ok := g.txSpend[txid]
	if !ok {
		return nil
	}
	prevSpent := g.spentToday
	prevCount := g.txCount
	if g.spentToday >= amt {
		g.spentToday -= amt
	} else {
		g.spentToday = 0
	}
	if g.txCount > 0 {
		g.txCount--
	}
	delete(g.txSpend, txid)
	if err := g.appendEntryLocked(spendLogEntry{
		Op:        "refund",
		Day:       g.dayKey,
		TxID:      txid,
		SpendSats: amt,
		TS:        now.Unix(),
	}); err != nil {
		g.spentToday = prevSpent
		g.txCount = prevCount
		g.txSpend[txid] = amt
		return err
	}
	return nil
}

// MinInterval returns the configured minimum spacing between outgoing broadcasts.
func (g *SpendGuard) MinInterval() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.minInterval
}

// RemainingInterval returns how long remains before the next outgoing broadcast is permitted.
func (g *SpendGuard) RemainingInterval() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	lastActivity := g.effectiveLastBroadcastLocked()
	if g.minInterval <= 0 || lastActivity.IsZero() {
		return 0
	}
	if rem := g.minInterval - g.now().Sub(lastActivity); rem > 0 {
		return rem
	}
	return 0
}

// Stats returns current UTC day spend and caps.
func (g *SpendGuard) Stats() (spentToday, maxDay, maxFee uint64, count int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollDayLocked(g.now())
	return g.spentToday, g.maxSpendDay, g.maxFeePerTx, g.txCount
}
