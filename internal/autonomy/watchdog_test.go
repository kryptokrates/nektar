package autonomy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
)

func TestWatchdogClockSkewMedianTripAndRecovery(t *testing.T) {
	offsets := map[string]time.Duration{
		"s1": 2 * time.Second,
		"s2": 3 * time.Second,
		"s3": 90 * time.Second, // single bad outlier ignored by median
	}
	var tripped []string
	w := NewWatchdog(Config{
		Network:            "testnet",
		NTPServers:         []string{"s1", "s2", "s3"},
		WarnSkew:           15 * time.Second,
		TripSkew:           25 * time.Second,
		RecoverSkew:        10 * time.Second,
		RecoverConsecutive: 2,
		NTPProbe: func(_ context.Context, srv string) (time.Duration, error) {
			return offsets[srv], nil
		},
		OnTrip: func(reason string) { tripped = append(tripped, reason) },
	})

	// 1. Median = 3s -> healthy.
	off, err := w.CheckClockNow(context.Background())
	if err != nil {
		t.Fatalf("CheckClockNow: %v", err)
	}
	if off != 3*time.Second {
		t.Fatalf("expected median 3s, got %s", off)
	}
	if st := w.Status(); !st.OK || st.ClockStatus != "ok" || st.SpendPaused {
		t.Fatalf("expected healthy status, got %+v", st)
	}

	// 2. Median = 18s -> warn, NOT paused yet.
	offsets["s1"], offsets["s2"], offsets["s3"] = 17*time.Second, 18*time.Second, 19*time.Second
	_, _ = w.CheckClockNow(context.Background())
	if st := w.Status(); st.ClockStatus != "warn" || st.SpendPaused {
		t.Fatalf("expected warn without spend pause, got %+v", st)
	}

	// 3. Median = -30s -> tripped, SpendGuard.Reserve MUST fail closed!
	offsets["s1"], offsets["s2"], offsets["s3"] = -28*time.Second, -30*time.Second, -32*time.Second
	_, _ = w.CheckClockNow(context.Background())
	if st := w.Status(); st.OK || !st.SpendPaused || st.ClockStatus != "tripped" {
		t.Fatalf("expected tripped status, got %+v", st)
	}
	if len(tripped) != 1 {
		t.Fatalf("expected 1 OnTrip callback, got %d", len(tripped))
	}

	sg := core.NewSpendGuard(core.SpendGuardOpts{MaxFeeSatsPerTx: 1000, MaxSpendSatsDay: 10000})
	sg.SetPreCheck(w.AllowAutonomousSpend)
	if _, err := sg.Reserve(10, 100); !errors.Is(err, ErrClockSkewTripped) {
		t.Fatalf("expected SpendGuard.Reserve to fail with ErrClockSkewTripped, got %v", err)
	}

	// 4. Recovery requires 2 consecutive <10s polls.
	offsets["s1"], offsets["s2"], offsets["s3"] = 2*time.Second, 3*time.Second, 4*time.Second
	_, _ = w.CheckClockNow(context.Background())
	if st := w.Status(); !st.SpendPaused {
		t.Fatalf("expected still paused after 1st healthy poll (hysteresis=2), got %+v", st)
	}
	_, _ = w.CheckClockNow(context.Background())
	if st := w.Status(); !st.OK || st.SpendPaused || st.ClockStatus != "ok" {
		t.Fatalf("expected recovered after 2nd healthy poll, got %+v", st)
	}
	if resID, err := sg.Reserve(10, 100); err != nil {
		t.Fatalf("expected SpendGuard.Reserve to succeed after recovery, got %v", err)
	} else {
		sg.CancelReservation(resID)
	}
}

func TestWatchdogUDPBlockedFallsBackToHTTPDate(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", now.Add(2*time.Second).Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	w := NewWatchdog(Config{
		Network:      "main",
		NTPServers:   []string{"udp-blocked-1", "udp-blocked-2"},
		HTTPDateURLs: []string{ts.URL},
		Now:          func() time.Time { return now },
		NTPProbe: func(_ context.Context, _ string) (time.Duration, error) {
			return 0, errors.New("i/o timeout")
		},
	})
	off, err := w.CheckClockNow(context.Background())
	if err != nil {
		t.Fatalf("expected HTTP Date fallback to succeed, got err=%v", err)
	}
	if absDuration(off-2*time.Second) > time.Second {
		t.Fatalf("expected ~2s offset from HTTP Date, got %s", off)
	}
	if st := w.Status(); st.ClockStatus != "degraded-http-date" || st.SpendPaused {
		t.Fatalf("expected degraded-http-date without pause, got %+v", st)
	}
}

func TestWatchdogSpendBreakerSlashBurstAndPeerIsolation(t *testing.T) {
	w := NewWatchdog(Config{
		Network:                 "memory",
		ConsecutiveFailures:     3,
		SlashBurstThreshold:     2,
		PeerConsecutiveFailures: 2,
		BreakerCooldown:         time.Minute,
		PeerBreakerCooldown:     time.Minute,
	})

	// 1. 3 consecutive autonomous spend failures open the spend circuit breaker.
	for i := 0; i < 3; i++ {
		_ = w.ExecuteAutonomousSpend(func() error { return errors.New("broadcast failed") })
	}
	if err := w.AllowAutonomousSpend(); !errors.Is(err, ErrSpendBreakerOpen) {
		t.Fatalf("expected ErrSpendBreakerOpen after 3 consecutive failures, got %v", err)
	}
	if st := w.Status(); !st.SpendPaused || st.SpendBreakerState != "open" {
		t.Fatalf("expected open spend breaker status, got %+v", st)
	}

	// 2. ManualReset clears breaker cleanly.
	w.ManualReset()
	if err := w.AllowAutonomousSpend(); err != nil {
		t.Fatalf("expected AllowAutonomousSpend nil after ManualReset, got %v", err)
	}

	// 3. Slash burst (2 events) pauses autonomous spending.
	w.RecordSlash("02aaaa", "dead feed 1")
	if err := w.AllowAutonomousSpend(); err != nil {
		t.Fatalf("single slash should not trip threshold=2, got %v", err)
	}
	w.RecordSlash("02bbbb", "dead feed 2")
	if err := w.AllowAutonomousSpend(); !errors.Is(err, ErrSpendBreakerOpen) {
		t.Fatalf("expected ErrSpendBreakerOpen after slash burst, got %v", err)
	}

	// 4. Per-peer isolation isolates bad seller without affecting healthy seller.
	for i := 0; i < 2; i++ {
		_ = w.ExecutePeerQuery("02badpeer", func() error { return errors.New("invalid proof") })
	}
	if err := w.AllowPeerQuery("02badpeer"); !errors.Is(err, ErrPeerBreakerOpen) {
		t.Fatalf("expected bad peer isolated, got %v", err)
	}
	if err := w.AllowPeerQuery("02goodpeer"); err != nil {
		t.Fatalf("expected good peer unaffected, got %v", err)
	}
	if st := w.Status(); len(st.IsolatedPeers) != 1 || st.IsolatedPeers[0] != "02badpeer" {
		t.Fatalf("expected isolatedPeers=[02badpeer], got %+v", st.IsolatedPeers)
	}
}
