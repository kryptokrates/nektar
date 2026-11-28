package core

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestKeyBackoffEvictsOldestWithoutMassAmnesia(t *testing.T) {
	kb := NewKeyBackoff[string](3, 10*time.Second, 5*time.Minute)
	base := time.Unix(1_700_000_000, 0)

	kb.RecordFailure("k1", base)
	kb.RecordFailure("k2", base.Add(1*time.Second))
	kb.RecordFailure("k3", base.Add(2*time.Second))
	// Adding k4 should evict ONLY k1 (oldest), preserving k2 and k3 cooldowns!
	kb.RecordFailure("k4", base.Add(3*time.Second))

	if !kb.ShouldAttempt("k1", base.Add(4*time.Second)) {
		t.Fatal("expected oldest key k1 to be evicted at cap")
	}
	for _, hot := range []string{"k2", "k3", "k4"} {
		if kb.ShouldAttempt(hot, base.Add(4*time.Second)) {
			t.Fatalf("expected active key %s cooldown to survive capacity eviction", hot)
		}
	}
}

func TestSleepContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := SleepContext(ctx, 5*time.Second); err == nil {
		t.Fatal("expected canceled error")
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("SleepContext did not return promptly on cancel: %v", d)
	}
	_ = fmt.Sprint(NewExpBackoff(10*time.Millisecond, 100*time.Millisecond).NextBackOff())
}
