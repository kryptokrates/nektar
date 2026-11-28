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

type watchedExpiry struct {
	txid    string
	addedAt time.Time
}

type conflictExpiry struct {
	txid       string
	recordedAt time.Time
}

const maxZeroConfConflicts = 65536

type zeroConfConflict struct {
	competingTxID string
	recordedAt    time.Time
}

// ZeroConfWatch tracks aggregate unconfirmed counterparty payment exposure and detects
// competing input spends (conflict watch) until each tx settles at settleDepth.
// Uses chronological FIFO queues + height buckets so expiry and block-settlement sweeps run in amortized O(1).
type ZeroConfWatch struct {
	mu            sync.Mutex
	byTxID        map[string]*watchedZeroConf
	byOutpoint    map[string]string           // "txid:vout" -> watching txid
	byHeight      map[uint32][]string         // minedHeight -> watched txids
	minMinedH     uint32                      // lowest non-empty key in byHeight (0 if empty)
	order         []watchedExpiry             // chronological FIFO for O(1) TTL pruning
	orderHead     int
	conflicts     map[string]zeroConfConflict // conflicted txid -> competing txid + timestamp
	conflictOrder []conflictExpiry
	conflictHead  int
	exposure      uint64
	defaultTTL    time.Duration
}

// NewZeroConfWatch creates a thread-safe O(1) 0-conf exposure and conflict tracker.
func NewZeroConfWatch() *ZeroConfWatch {
	return &ZeroConfWatch{
		byTxID:     make(map[string]*watchedZeroConf, 256),
		byOutpoint: make(map[string]string, 512),
		byHeight:   make(map[uint32][]string),
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
		if now.Sub(comp.recordedAt) <= w.defaultTTL {
			return fmt.Errorf("%w: tx %s conflicts with %s", ErrConflictDetected, txid, comp.competingTxID)
		}
		delete(w.conflicts, txid)
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
	w.order = append(w.order, watchedExpiry{txid: txid, addedAt: now})
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
		w.byHeight[height] = append(w.byHeight[height], txid)
		if w.minMinedH == 0 || height < w.minMinedH {
			w.minMinedH = height
		}
	}
}

// ClearMinedFrom clears minedHeight for watched txs mined at or above forkHeight on chain reorg.
func (w *ZeroConfWatch) ClearMinedFrom(forkHeight uint32) {
	if w == nil || forkHeight == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for h, txids := range w.byHeight {
		if h >= forkHeight {
			for _, id := range txids {
				if rec := w.byTxID[id]; rec != nil && rec.minedHeight >= forkHeight {
					rec.minedHeight = 0
				}
			}
			delete(w.byHeight, h)
		}
	}
	w.recomputeMinMinedLocked()
}

// SweepSettled evicts any watched tx whose recorded minedHeight has reached settleDepth at tip in amortized O(1).
func (w *ZeroConfWatch) SweepSettled(tip uint32, settleDepth int) int {
	if w == nil || tip == 0 {
		return 0
	}
	if settleDepth <= 0 {
		settleDepth = 1
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.byHeight) == 0 || w.minMinedH == 0 {
		return 0
	}
	if tip < uint32(settleDepth-1) {
		return 0
	}
	maxSettledH := tip - uint32(settleDepth-1)
	if w.minMinedH > maxSettledH {
		return 0
	}
	var evicted int
	for h, txids := range w.byHeight {
		if h <= maxSettledH {
			for _, id := range txids {
				if rec := w.byTxID[id]; rec != nil && rec.minedHeight == h {
					w.removeLocked(id)
					evicted++
				}
			}
			delete(w.byHeight, h)
		}
	}
	w.recomputeMinMinedLocked()
	return evicted
}

func (w *ZeroConfWatch) recomputeMinMinedLocked() {
	w.minMinedH = 0
	for h := range w.byHeight {
		if w.minMinedH == 0 || h < w.minMinedH {
			w.minMinedH = h
		}
	}
}

// HasConflict reports whether txid has any observed competing spend within TTL.
func (w *ZeroConfWatch) HasConflict(txid string) bool {
	if w == nil || txid == "" {
		return false
	}
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneExpiredLocked(now)
	c, ok := w.conflicts[txid]
	if !ok {
		return false
	}
	if now.Sub(c.recordedAt) > w.defaultTTL {
		delete(w.conflicts, txid)
		return false
	}
	return true
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
	w.pruneExpiredConflictsLocked(now)
	for len(w.conflicts) >= maxZeroConfConflicts && w.conflictHead < len(w.conflictOrder) {
		old := w.conflictOrder[w.conflictHead]
		w.conflictOrder[w.conflictHead] = conflictExpiry{}
		w.conflictHead++
		delete(w.conflicts, old.txid)
	}
	w.conflicts[txid] = zeroConfConflict{competingTxID: competingTxID, recordedAt: now}
	w.conflictOrder = append(w.conflictOrder, conflictExpiry{txid: txid, recordedAt: now})
}

func (w *ZeroConfWatch) pruneExpiredConflictsLocked(now time.Time) {
	for w.conflictHead < len(w.conflictOrder) {
		head := w.conflictOrder[w.conflictHead]
		if now.Sub(head.recordedAt) <= w.defaultTTL {
			break
		}
		w.conflictOrder[w.conflictHead] = conflictExpiry{}
		w.conflictHead++
		if c, ok := w.conflicts[head.txid]; ok && now.Sub(c.recordedAt) > w.defaultTTL {
			delete(w.conflicts, head.txid)
		}
	}
	if w.conflictHead > 0 && w.conflictHead*2 >= len(w.conflictOrder) {
		rem := copy(w.conflictOrder, w.conflictOrder[w.conflictHead:])
		w.conflictOrder = w.conflictOrder[:rem]
		w.conflictHead = 0
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
	for w.orderHead < len(w.order) {
		head := w.order[w.orderHead]
		if now.Sub(head.addedAt) <= w.defaultTTL {
			break
		}
		w.order[w.orderHead] = watchedExpiry{}
		w.orderHead++
		if rec, ok := w.byTxID[head.txid]; ok && now.Sub(rec.addedAt) > w.defaultTTL {
			w.removeLocked(head.txid)
		}
	}
	if w.orderHead > 0 && w.orderHead*2 >= len(w.order) {
		rem := copy(w.order, w.order[w.orderHead:])
		w.order = w.order[:rem]
		w.orderHead = 0
	}
	w.pruneExpiredConflictsLocked(now)
}
