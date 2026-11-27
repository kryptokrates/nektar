package hub

import (
	"crypto/sha256"
	"sync"
)

// LoopGuard is a bounded FIFO seen-set keyed on SHA-256(rawBEEF || 0x00 || SHA-256(topic)),
// matching lightwebinc/overlay-bridge/guard and 1BSV Paper 008 §5.
//
// Keying on raw BEEF bytes (rather than subject txid) is deliberate: a byte-identical echo
// arriving back from the object plane or a second facade is suppressed, while a proof-refreshed
// republish of the same txid carrying a newly mined BUMP has distinct bytes and passes cleanly.
type LoopGuard struct {
	mu    sync.Mutex
	cap   int
	seen  map[[32]byte]struct{}
	order [][32]byte
	head  int
}

const DefaultLoopGuardCap = 65536

// NewLoopGuard creates a bounded LoopGuard holding up to capacity distinct (BEEF, topic) pairs.
func NewLoopGuard(capacity int) *LoopGuard {
	if capacity <= 0 {
		capacity = DefaultLoopGuardCap
	}
	return &LoopGuard{
		cap:   capacity,
		seen:  make(map[[32]byte]struct{}, min(capacity, 4096)),
		order: make([][32]byte, 0, min(capacity, 4096)),
	}
}

// ObjectKey computes SHA-256(SHA-256(rawBEEF) || SHA-256(topic)) — the exact composite
// ContentID || TopicID key used by the BRC-148/149 object plane and overlay-bridge.
func ObjectKey(rawBEEF []byte, topic string) [32]byte {
	cid := sha256.Sum256(rawBEEF)
	tid := sha256.Sum256([]byte(topic))
	var buf [64]byte
	copy(buf[:32], cid[:])
	copy(buf[32:], tid[:])
	return sha256.Sum256(buf[:])
}

// Seen reports whether (rawBEEF, topic) is already present in the guard without mutating it.
func (g *LoopGuard) Seen(rawBEEF []byte, topic string) bool {
	if g == nil || len(rawBEEF) == 0 {
		return false
	}
	k := ObjectKey(rawBEEF, topic)
	g.mu.Lock()
	_, ok := g.seen[k]
	g.mu.Unlock()
	return ok
}

// MarkIfUnseen atomically checks whether (rawBEEF, topic) was unseen and marks it.
// Returns true if it was unseen (caller should proceed), false if already seen (duplicate/echo).
func (g *LoopGuard) MarkIfUnseen(rawBEEF []byte, topic string) bool {
	if g == nil || len(rawBEEF) == 0 {
		return true
	}
	k := ObjectKey(rawBEEF, topic)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.seen[k]; ok {
		return false
	}
	g.insertLocked(k)
	return true
}

func (g *LoopGuard) insertLocked(k [32]byte) {
	if len(g.order) < g.cap {
		g.order = append(g.order, k)
		g.seen[k] = struct{}{}
		return
	}
	old := g.order[g.head]
	delete(g.seen, old)
	g.order[g.head] = k
	g.seen[k] = struct{}{}
	g.head = (g.head + 1) % g.cap
}
