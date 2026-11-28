package eventbus

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrSubscriberCapReached is returned when SubscribeWithCap exceeds the configured subscriber ceiling.
var ErrSubscriberCapReached = errors.New("eventbus: subscriber limit reached")

// ErrEventGap is returned by WaitFor when a burst overflows the subscription buffer and events may have been missed.
var ErrEventGap = errors.New("eventbus: slow consumer missed events (gap detected)")

// Event is a monotonically sequenced, topic-keyed in-process notification.
type Event struct {
	Seq       int64  `json:"seq"`
	Topic     string `json:"topic"`
	Kind      string `json:"kind"`
	Payload   any    `json:"payload,omitempty"`
	Timestamp int64  `json:"timestamp"`
}

// Subscription receives events matching its topic filter without blocking publishers.
// If the subscriber falls behind its bounded ring/channel buffer, TakeGap() returns true
// and Wake() fires so the consumer can catch up from durable SQLite state (e.g. ListFragmentsSince).
type Subscription struct {
	bus      *Bus
	id       uint64
	exact    map[string]struct{}
	prefixes []string
	wildcard bool
	ch       chan Event
	wake     chan struct{}
	gap      atomic.Bool
	closed   atomic.Bool
	once     sync.Once
}

// Events returns the buffered channel of matching events.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Wake signals when a slow-consumer buffer overflow occurred so the reader can run SQLite catch-up.
func (s *Subscription) Wake() <-chan struct{} { return s.wake }

// TakeGap atomically reads and clears the slow-consumer overflow flag.
func (s *Subscription) TakeGap() bool { return s.gap.Swap(false) }

// Close unsubscribes from the bus and closes the subscription channels idempotently.
func (s *Subscription) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.closed.Store(true)
		if s.bus != nil {
			s.bus.remove(s.id)
		}
		close(s.ch)
		close(s.wake)
	})
}

func (s *Subscription) matches(topic string) bool {
	if s.wildcard {
		return true
	}
	if _, ok := s.exact[topic]; ok {
		return true
	}
	for _, p := range s.prefixes {
		if strings.HasPrefix(topic, p) {
			return true
		}
	}
	return false
}

func (s *Subscription) deliver(ev Event) {
	if s.closed.Load() {
		return
	}
	select {
	case s.ch <- ev:
	default:
		s.gap.Store(true)
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

// Bus is Nektar's single cursor-numbered, zero-polling in-process pub/sub backbone.
type Bus struct {
	seq   atomic.Int64
	subID atomic.Uint64

	mu   sync.RWMutex
	subs map[uint64]*Subscription
}

// New constructs a new in-process EventBus.
func New() *Bus {
	return &Bus{
		subs: make(map[uint64]*Subscription),
	}
}

// Seq returns the highest published monotonic event sequence number (0 if none).
func (b *Bus) Seq() int64 {
	if b == nil {
		return 0
	}
	return b.seq.Load()
}

// SubscriberCount returns the number of active subscriptions currently attached to b.
func (b *Bus) SubscriberCount() int {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	n := len(b.subs)
	b.mu.RUnlock()
	return n
}

// Subscribe creates a non-blocking subscription for topics (supports exact topics, "*" wildcard,
// and trailing-asterisk prefixes like "shard:*").
func (b *Bus) Subscribe(topics []string, bufSize int) *Subscription {
	sub, _ := b.SubscribeWithCap(topics, bufSize, 0)
	return sub
}

// SubscribeWithCap creates a subscription while enforcing maxSubs > 0 across the bus (or topic prefix).
func (b *Bus) SubscribeWithCap(topics []string, bufSize, maxSubs int) (*Subscription, error) {
	if bufSize <= 0 {
		bufSize = 64
	}
	sub := &Subscription{
		bus:   b,
		id:    b.subID.Add(1),
		exact: make(map[string]struct{}, len(topics)),
		ch:    make(chan Event, bufSize),
		wake:  make(chan struct{}, 1),
	}
	if len(topics) == 0 {
		sub.wildcard = true
	}
	for _, raw := range topics {
		t := strings.TrimSpace(raw)
		switch {
		case t == "" || t == "*":
			sub.wildcard = true
		case strings.HasSuffix(t, "*"):
			sub.prefixes = append(sub.prefixes, strings.TrimSuffix(t, "*"))
		default:
			sub.exact[t] = struct{}{}
		}
	}

	b.mu.Lock()
	if maxSubs > 0 && len(b.subs) >= maxSubs {
		b.mu.Unlock()
		return nil, ErrSubscriberCapReached
	}
	b.subs[sub.id] = sub
	b.mu.Unlock()
	return sub, nil
}

func (b *Bus) remove(id uint64) {
	b.mu.Lock()
	delete(b.subs, id)
	b.mu.Unlock()
}

// Publish assigns the next monotonic sequence number and fans ev out to all matching subscribers
// without ever blocking on a slow consumer.
func (b *Bus) Publish(topic, kind string, payload any) Event {
	if b == nil {
		return Event{Topic: topic, Kind: kind, Payload: payload, Timestamp: time.Now().Unix()}
	}
	ev := Event{
		Seq:       b.seq.Add(1),
		Topic:     topic,
		Kind:      kind,
		Payload:   payload,
		Timestamp: time.Now().Unix(),
	}
	b.mu.RLock()
	targets := make([]*Subscription, 0, len(b.subs))
	for _, s := range b.subs {
		if s.matches(topic) {
			targets = append(targets, s)
		}
	}
	b.mu.RUnlock()

	for _, s := range targets {
		s.deliver(ev)
	}
	return ev
}

// WaitFor blocks without polling until an event on topic satisfies pred, a slow-consumer buffer
// overflow occurs (returning ErrEventGap so callers can reconcile durable state), or ctx is canceled.
func (b *Bus) WaitFor(ctx context.Context, topic string, pred func(Event) bool) (Event, error) {
	if b == nil {
		return Event{}, errors.New("eventbus: nil bus")
	}
	sub := b.Subscribe([]string{topic}, 64)
	defer sub.Close()
	for {
		if sub.TakeGap() {
			return Event{}, ErrEventGap
		}
		select {
		case <-ctx.Done():
			return Event{}, ctx.Err()
		case <-sub.Wake():
			if sub.TakeGap() {
				return Event{}, ErrEventGap
			}
		case ev, ok := <-sub.Events():
			if !ok {
				return Event{}, context.Canceled
			}
			if pred == nil || pred(ev) {
				return ev, nil
			}
		}
	}
}
