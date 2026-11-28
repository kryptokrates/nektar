package core

import (
	"context"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v5"
)

// NewExpBackoff returns a jittered exponential backoff policy backed by cenkalti/backoff/v5.
func NewExpBackoff(initial, maxInterval time.Duration) *backoff.ExponentialBackOff {
	if initial <= 0 {
		initial = 200 * time.Millisecond
	}
	if maxInterval < initial {
		maxInterval = initial * 30
	}
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = initial
	b.MaxInterval = maxInterval
	b.Multiplier = 2.0
	b.RandomizationFactor = 0.25
	b.Reset()
	return b
}

// SleepContext pauses for d or returns ctx.Err() immediately on cancellation without leaking timers.
func SleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type keyBackoffItem[K comparable] struct {
	attempts int
	nextTry  time.Time
	lastSeen time.Time
}

// KeyBackoff tracks per-key exponential retry cooldowns with bounded capacity.
// When capacity is reached, ONLY the oldest entry is evicted — never wiping active cooldowns via clear().
type KeyBackoff[K comparable] struct {
	mu      sync.Mutex
	cap     int
	initial time.Duration
	maxWait time.Duration
	items   map[K]keyBackoffItem[K]
}

// NewKeyBackoff constructs a bounded per-key backoff map.
func NewKeyBackoff[K comparable](capacity int, initial, maxWait time.Duration) *KeyBackoff[K] {
	if capacity <= 0 {
		capacity = 512
	}
	if initial <= 0 {
		initial = 30 * time.Second
	}
	if maxWait < initial {
		maxWait = 30 * time.Minute
	}
	return &KeyBackoff[K]{
		cap:     capacity,
		initial: initial,
		maxWait: maxWait,
		items:   make(map[K]keyBackoffItem[K]),
	}
}

// ShouldAttempt reports whether key is eligible for an attempt at now.
func (kb *KeyBackoff[K]) ShouldAttempt(key K, now time.Time) bool {
	if kb == nil {
		return true
	}
	kb.mu.Lock()
	defer kb.mu.Unlock()
	ent, ok := kb.items[key]
	if !ok {
		return true
	}
	return !now.Before(ent.nextTry)
}

// RecordFailure increments key's failure count, computes exponential cooldown, and evicts at most one
// oldest entry when at capacity (eliminating thundering-herd mass amnesia).
func (kb *KeyBackoff[K]) RecordFailure(key K, now time.Time) (int, time.Time) {
	if kb == nil {
		return 1, now
	}
	kb.mu.Lock()
	defer kb.mu.Unlock()
	ent, exists := kb.items[key]
	if !exists && len(kb.items) >= kb.cap {
		var oldestKey K
		var hasOldest bool
		var oldestTime time.Time
		for k, v := range kb.items {
			if !hasOldest || v.lastSeen.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.lastSeen
				hasOldest = true
			}
		}
		if hasOldest {
			delete(kb.items, oldestKey)
		}
	}
	ent.attempts++
	shift := min(ent.attempts-1, 10)
	wait := kb.initial * time.Duration(1<<shift)
	if wait > kb.maxWait || wait <= 0 {
		wait = kb.maxWait
	}
	ent.nextTry = now.Add(wait)
	ent.lastSeen = now
	kb.items[key] = ent
	return ent.attempts, ent.nextTry
}

// Attempts returns the recorded failure count for key.
func (kb *KeyBackoff[K]) Attempts(key K) int {
	if kb == nil {
		return 0
	}
	kb.mu.Lock()
	defer kb.mu.Unlock()
	return kb.items[key].attempts
}

// Clear removes key's backoff state after a successful operation.
func (kb *KeyBackoff[K]) Clear(key K) {
	if kb == nil {
		return
	}
	kb.mu.Lock()
	delete(kb.items, key)
	kb.mu.Unlock()
}
