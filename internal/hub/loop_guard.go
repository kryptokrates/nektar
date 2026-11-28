package hub

import (
	"crypto/sha256"
	"sync"
)

const (
	DefaultLoopGuardCap = 524288
	numLoopGuardShards  = 16
)

type loopGuardShard struct {
	mu    sync.Mutex
	cap   int
	seen  map[[32]byte]struct{}
	order [][32]byte
	head  int
}

// LoopGuard is a 16-shard bounded FIFO seen-set keyed on SHA-256(SHA-256(rawBEEF) || SHA-256(topic)),
// matching lightwebinc/overlay-bridge/guard and 1BSV Paper 008 §5.
//
// Keying on raw BEEF bytes (rather than subject txid) is deliberate: a byte-identical echo
// arriving back from the object plane or a second facade is suppressed, while a proof-refreshed
// republish of the same txid carrying a newly mined BUMP has distinct bytes and passes cleanly.
type LoopGuard struct {
	shards [numLoopGuardShards]loopGuardShard
}

// NewLoopGuard creates a bounded LoopGuard holding up to capacity distinct (BEEF, topic) pairs.
func NewLoopGuard(capacity int) *LoopGuard {
	if capacity <= 0 {
		capacity = DefaultLoopGuardCap
	}
	shardCap := (capacity + numLoopGuardShards - 1) / numLoopGuardShards
	if shardCap < 1 {
		shardCap = 1
	}
	g := &LoopGuard{}
	for i := range g.shards {
		g.shards[i] = loopGuardShard{
			cap:   shardCap,
			seen:  make(map[[32]byte]struct{}, min(shardCap, 512)),
			order: make([][32]byte, 0, min(shardCap, 512)),
		}
	}
	return g
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
	sh := &g.shards[k[0]&(numLoopGuardShards-1)]
	sh.mu.Lock()
	_, ok := sh.seen[k]
	sh.mu.Unlock()
	return ok
}

// MarkIfUnseen atomically checks whether (rawBEEF, topic) was unseen and marks it.
// Returns true if it was unseen (caller should proceed), false if already seen (duplicate/echo).
func (g *LoopGuard) MarkIfUnseen(rawBEEF []byte, topic string) bool {
	if g == nil || len(rawBEEF) == 0 {
		return true
	}
	k := ObjectKey(rawBEEF, topic)
	sh := &g.shards[k[0]&(numLoopGuardShards-1)]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if _, ok := sh.seen[k]; ok {
		return false
	}
	sh.insertLocked(k)
	return true
}

func (sh *loopGuardShard) insertLocked(k [32]byte) {
	if len(sh.order) < sh.cap {
		sh.order = append(sh.order, k)
		sh.seen[k] = struct{}{}
		return
	}
	old := sh.order[sh.head]
	delete(sh.seen, old)
	sh.order[sh.head] = k
	sh.seen[k] = struct{}{}
	sh.head = (sh.head + 1) % sh.cap
}
