package chain

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// DefaultMaxUnconfirmedAncestors bounds counterparty 0-conf unmined ancestor chains.
	DefaultMaxUnconfirmedAncestors = 15
)

var (
	// ErrTrackerUnavailable indicates a header tracker transport/quorum outage (retryable, never 0-conf fallback).
	ErrTrackerUnavailable = errors.New("chain: header tracker unavailable")
	// ErrRefusedBUMP indicates a carried MerklePath failed verification against witnessed block headers.
	ErrRefusedBUMP = errors.New("chain: REFUSED-BUMP merkle path invalid")
	// ErrZeroConfCapExceeded indicates a 0-conf payment exceeded per-tx, aggregate, or ancestor policy caps.
	ErrZeroConfCapExceeded = errors.New("chain: zero-conf policy cap exceeded")
	// ErrConflictDetected indicates a competing spend was observed for a watched 0-conf input outpoint.
	ErrConflictDetected = errors.New("chain: conflicting double-spend detected on watched input")
)

type watchedZeroConf struct {
	txid        string
	amountSats  uint64
	inputs      []string
	addedAt     time.Time
	minedHeight uint32 // non-zero once mined below settleDepth; cleared on reorg >= forkHeight
	conflict    bool
}

const maxZeroConfConflicts = 4096

type zeroConfConflict struct {
	competingTxID string
	recordedAt    time.Time
}

// ZeroConfWatch tracks aggregate unconfirmed counterparty payment exposure and detects
// competing input spends (conflict watch) until each tx settles at settleDepth.
type ZeroConfWatch struct {
	mu         sync.Mutex
	byTxID     map[string]*watchedZeroConf
	byOutpoint map[string]string           // "txid:vout" -> watching txid
	conflicts  map[string]zeroConfConflict // conflicted txid -> competing txid + timestamp (TTL-evicted & capped)
	exposure   uint64
	defaultTTL time.Duration
}

// NewZeroConfWatch creates a thread-safe 0-conf exposure and conflict tracker.
func NewZeroConfWatch() *ZeroConfWatch {
	return &ZeroConfWatch{
		byTxID:     make(map[string]*watchedZeroConf),
		byOutpoint: make(map[string]string),
		conflicts:  make(map[string]zeroConfConflict),
		defaultTTL: 2 * time.Hour,
	}
}

// CheckAndTrack enforces conflict-freedom and aggregate unconfirmed exposure cap before accepting a 0-conf payment.
// If maxAggregateSats > 0 and adding amountSats would exceed it, returns ErrZeroConfCapExceeded without tracking.
func (w *ZeroConfWatch) CheckAndTrack(txid string, amountSats uint64, inputs []string, maxAggregateSats uint64) error {
	if w == nil || txid == "" {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	w.pruneExpiredLocked(now)

	if comp, conflicted := w.conflicts[txid]; conflicted {
		return fmt.Errorf("%w: tx %s conflicts with %s", ErrConflictDetected, txid, comp.competingTxID)
	}
	if _, already := w.byTxID[txid]; already {
		return nil
	}
	for _, op := range inputs {
		if op == "" {
			continue
		}
		if other, exists := w.byOutpoint[op]; exists && other != txid {
			w.recordConflictLocked(txid, other, now)
			w.recordConflictLocked(other, txid, now)
			if rec := w.byTxID[other]; rec != nil {
				rec.conflict = true
			}
			return fmt.Errorf("%w: outpoint %s already spent by %s", ErrConflictDetected, op, other)
		}
	}
	if maxAggregateSats > 0 && w.exposure+amountSats > maxAggregateSats {
		return fmt.Errorf("%w: aggregate unconfirmed exposure %d + %d exceeds cap %d sats",
			ErrZeroConfCapExceeded, w.exposure, amountSats, maxAggregateSats)
	}
	rec := &watchedZeroConf{
		txid:       txid,
		amountSats: amountSats,
		inputs:     append([]string(nil), inputs...),
		addedAt:    now,
	}
	w.byTxID[txid] = rec
	w.exposure += amountSats
	for _, op := range inputs {
		if op != "" {
			w.byOutpoint[op] = txid
		}
	}
	return nil
}

// MarkSettled removes txid from active 0-conf exposure once it reaches settleDepth.
func (w *ZeroConfWatch) MarkSettled(txid string) {
	if w == nil || txid == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.removeLocked(txid)
}

// MarkMined records a watched tx's verified block height when it mines below settleDepth so
// SweepSettled releases its 0-conf exposure as soon as witnessed tip reaches settleDepth.
func (w *ZeroConfWatch) MarkMined(txid string, height uint32) {
	if w == nil || txid == "" || height == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if rec := w.byTxID[txid]; rec != nil {
		rec.minedHeight = height
	}
}

// ClearMinedFrom clears minedHeight for watched txs mined at or above forkHeight on chain reorg.
func (w *ZeroConfWatch) ClearMinedFrom(forkHeight uint32) {
	if w == nil || forkHeight == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, rec := range w.byTxID {
		if rec.minedHeight >= forkHeight {
			rec.minedHeight = 0
		}
	}
}

// SweepSettled evicts any watched tx whose recorded minedHeight has reached settleDepth at tip.
func (w *ZeroConfWatch) SweepSettled(tip uint32, settleDepth int) int {
	if w == nil || tip == 0 {
		return 0
	}
	if settleDepth <= 0 {
		settleDepth = 1
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var evicted int
	for txid, rec := range w.byTxID {
		if rec.minedHeight > 0 && tip >= rec.minedHeight && int(tip-rec.minedHeight)+1 >= settleDepth {
			w.removeLocked(txid)
			evicted++
		}
	}
	return evicted
}

// HasConflict reports whether txid has any observed competing spend within TTL.
func (w *ZeroConfWatch) HasConflict(txid string) bool {
	if w == nil || txid == "" {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneExpiredLocked(time.Now())
	_, ok := w.conflicts[txid]
	return ok
}

// Exposure returns current aggregate unconfirmed sats and active watched tx count.
func (w *ZeroConfWatch) Exposure() (sats uint64, count int) {
	if w == nil {
		return 0, 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneExpiredLocked(time.Now())
	return w.exposure, len(w.byTxID)
}

func (w *ZeroConfWatch) recordConflictLocked(txid, competingTxID string, now time.Time) {
	if txid == "" {
		return
	}
	if _, exists := w.conflicts[txid]; !exists && len(w.conflicts) >= maxZeroConfConflicts {
		w.pruneExpiredConflictsLocked(now)
		for len(w.conflicts) >= maxZeroConfConflicts {
			var oldestKey string
			var oldestTime time.Time
			for k, v := range w.conflicts {
				if oldestKey == "" || v.recordedAt.Before(oldestTime) {
					oldestKey = k
					oldestTime = v.recordedAt
				}
			}
			if oldestKey == "" {
				break
			}
			delete(w.conflicts, oldestKey)
		}
	}
	w.conflicts[txid] = zeroConfConflict{competingTxID: competingTxID, recordedAt: now}
}

func (w *ZeroConfWatch) pruneExpiredConflictsLocked(now time.Time) {
	for txid, c := range w.conflicts {
		if now.Sub(c.recordedAt) > w.defaultTTL {
			delete(w.conflicts, txid)
		}
	}
}

func (w *ZeroConfWatch) removeLocked(txid string) {
	rec, ok := w.byTxID[txid]
	if ok {
		if w.exposure >= rec.amountSats {
			w.exposure -= rec.amountSats
		} else {
			w.exposure = 0
		}
		for _, op := range rec.inputs {
			if w.byOutpoint[op] == txid {
				delete(w.byOutpoint, op)
			}
		}
		delete(w.byTxID, txid)
	}
	if c, hasConflict := w.conflicts[txid]; hasConflict {
		delete(w.conflicts, txid)
		if c.competingTxID != "" {
			if _, stillTracked := w.byTxID[c.competingTxID]; !stillTracked {
				delete(w.conflicts, c.competingTxID)
			}
		}
	}
}

func (w *ZeroConfWatch) pruneExpiredLocked(now time.Time) {
	for txid, rec := range w.byTxID {
		if now.Sub(rec.addedAt) > w.defaultTTL {
			w.removeLocked(txid)
		}
	}
	w.pruneExpiredConflictsLocked(now)
}
