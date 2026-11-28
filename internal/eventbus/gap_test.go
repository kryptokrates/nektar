package eventbus

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEventBusWaitForReturnsGapOnOverflow(t *testing.T) {
	bus := New()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Subscribe directly with a tiny buffer? WaitFor uses internal bufSize=64, so burst > 128 events
	// faster than a blocked predicate to deterministically overflow the WaitFor subscription buffer.
	predBlocked := make(chan struct{})
	releasePred := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		first := true
		_, err := bus.WaitFor(ctx, "deal:test", func(ev Event) bool {
			if first {
				first = false
				close(predBlocked)
				<-releasePred
			}
			return ev.Kind == "target_never_arrives_before_overflow"
		})
		errCh <- err
	}()

	// Wait until WaitFor is subscribed, then trigger the first event so predicate pauses while we burst.
	deadline := time.Now().Add(time.Second)
	for bus.SubscriberCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	bus.Publish("deal:test", "kick", nil)
	<-predBlocked

	// Overflow WaitFor's 64-slot buffer while its goroutine is busy in the predicate.
	for i := 0; i < 100; i++ {
		bus.Publish("deal:test", "noise", i)
	}
	close(releasePred)

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrEventGap) {
			t.Fatalf("expected ErrEventGap on slow-consumer overflow, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitFor hung after slow-consumer buffer overflow instead of returning ErrEventGap")
	}
}
