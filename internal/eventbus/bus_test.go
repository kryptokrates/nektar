package eventbus

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBusPublishSubscribeAndGap(t *testing.T) {
	bus := New()
	sub, err := bus.SubscribeWithCap([]string{"shard:alpha", "deal:*"}, 2, 4)
	if err != nil {
		t.Fatalf("SubscribeWithCap: %v", err)
	}
	defer sub.Close()

	// Unrelated topic is ignored.
	bus.Publish("shard:beta", "remember", 1)

	// Matching exact + prefix topics arrive in monotonic sequence order.
	e1 := bus.Publish("shard:alpha", "remember", "a1")
	e2 := bus.Publish("deal:123", "settled", "d1")
	// Overflow buffer (size 2) -> sets gap without blocking publisher!
	e3 := bus.Publish("shard:alpha", "remember", "a2")
	if e1.Seq >= e2.Seq || e2.Seq >= e3.Seq {
		t.Fatalf("non-monotonic seqs: %d, %d, %d", e1.Seq, e2.Seq, e3.Seq)
	}
	if !sub.TakeGap() {
		t.Fatal("expected slow-consumer gap flag on overflow")
	}
	if sub.TakeGap() {
		t.Fatal("expected TakeGap to clear flag")
	}
	got1 := <-sub.Events()
	got2 := <-sub.Events()
	if got1.Payload != "a1" || got2.Payload != "d1" {
		t.Fatalf("unexpected events: %+v, %+v", got1, got2)
	}
}

func TestBusCapAndWaitFor(t *testing.T) {
	bus := New()
	s1, err := bus.SubscribeWithCap([]string{"*"}, 4, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.SubscribeWithCap([]string{"*"}, 4, 1); !errors.Is(err, ErrSubscriberCapReached) {
		t.Fatalf("expected ErrSubscriberCapReached, got %v", err)
	}
	s1.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		time.Sleep(20 * time.Millisecond)
		bus.Publish("peer:ready", "connected", "ok")
	}()
	ev, err := bus.WaitFor(ctx, "peer:ready", func(e Event) bool { return e.Payload == "ok" })
	if err != nil || ev.Kind != "connected" {
		t.Fatalf("WaitFor failed: ev=%+v err=%v", ev, err)
	}
}
