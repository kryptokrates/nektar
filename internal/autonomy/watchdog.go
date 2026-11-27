package autonomy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/beevik/ntp"
	"github.com/sony/gobreaker/v2"
)

const (
	DefaultPollInterval            = 5 * time.Minute
	DefaultWarnSkew                = 15 * time.Second
	DefaultTripSkew                = 25 * time.Second
	DefaultRecoverSkew             = 10 * time.Second
	DefaultRecoverConsecutive      = 2
	DefaultBreakerWindow           = 10 * time.Minute
	DefaultBreakerCooldown         = 15 * time.Minute
	DefaultConsecutiveFailures     = uint32(5)
	DefaultSlashBurstThreshold     = 3
	DefaultPeerBreakerCooldown     = 10 * time.Minute
	DefaultPeerConsecutiveFailures = uint32(3)
	maxTrackedPeerBreakers         = 256
)

var (
	// DefaultNTPServers lists independent public SNTP pools queried for median clock-skew detection.
	DefaultNTPServers = []string{
		"time.cloudflare.com",
		"pool.ntp.org",
		"time.google.com",
	}

	// ErrClockSkewTripped indicates local system time drifted past the safety guard inside the ±60s SSE replay window.
	ErrClockSkewTripped = errors.New("autonomy: autonomous spending paused due to system clock skew")
	// ErrSpendBreakerOpen indicates the anomaly circuit breaker opened after repeated settlement failures or slashing bursts.
	ErrSpendBreakerOpen = errors.New("autonomy: autonomous spending paused by anomaly circuit breaker")
	// ErrPeerBreakerOpen indicates a specific peer seller is temporarily isolated after repeated query failures.
	ErrPeerBreakerOpen = errors.New("autonomy: peer seller temporarily isolated by circuit breaker")
)

// Config configures the Nektar autonomy safety Watchdog (SNTP clock-skew guard + gobreaker circuit breakers).
type Config struct {
	Network                 string
	NTPServers              []string
	HTTPDateURLs            []string
	PollInterval            time.Duration
	WarnSkew                time.Duration
	TripSkew                time.Duration
	RecoverSkew             time.Duration
	RecoverConsecutive      int
	BreakerWindow           time.Duration
	BreakerCooldown         time.Duration
	ConsecutiveFailures     uint32
	SlashBurstThreshold     int
	PeerBreakerCooldown     time.Duration
	PeerConsecutiveFailures uint32
	// NTPProbe overrides the SNTP query function (defaults to github.com/beevik/ntp).
	NTPProbe   func(ctx context.Context, server string) (time.Duration, error)
	HTTPClient *http.Client
	Now        func() time.Time
	OnTrip     func(reason string)
}

// Status is the JSON-serializable operational snapshot exposed to nektar doctor, /state, and /v1/metrics.
type Status struct {
	OK                   bool     `json:"ok"`
	SpendPaused          bool     `json:"spendPaused"`
	PausedReason         string   `json:"pausedReason,omitempty"`
	ClockStatus          string   `json:"clockStatus"` // ok | warn | tripped | degraded-http-date | disabled | pending
	ClockOffsetMs        int64    `json:"clockOffsetMs"`
	ClockCheckedAt       int64    `json:"clockCheckedAt,omitempty"`
	ClockServer          string   `json:"clockServer,omitempty"`
	ClockError           string   `json:"clockError,omitempty"`
	SpendBreakerState    string   `json:"spendBreakerState"` // closed | half-open | open
	SlashEventsWindow    int      `json:"slashEventsWindow"`
	ConsecutiveSpendErrs int      `json:"consecutiveSpendErrors"`
	IsolatedPeers        []string `json:"isolatedPeers,omitempty"`
	LastTrippedAt        int64    `json:"lastTrippedAt,omitempty"`
	LastTripReason       string   `json:"lastTripReason,omitempty"`
}

type peerBreakerEntry struct {
	cb       *gobreaker.CircuitBreaker[struct{}]
	lastUsed time.Time
}

// Watchdog protects unattended Mainnet nodes from silent ±60s replay failures (clock drift)
// and runaway spend / dead-feed slashing bursts via github.com/beevik/ntp + github.com/sony/gobreaker/v2.
type Watchdog struct {
	cfg Config
	now func() time.Time

	mu                    sync.Mutex
	clockStatus           string
	clockOffset           time.Duration
	clockCheckedAt        time.Time
	clockServer           string
	clockErr              string
	clockTripped          bool
	healthyConsecutive    int
	spendBreaker          *gobreaker.CircuitBreaker[struct{}]
	consecutiveSpendErrs  int
	slashTimestamps       []time.Time
	slashPausedUntil      time.Time
	lastTrippedAt         time.Time
	lastTripReason        string
	peerBreakers          map[string]*peerBreakerEntry

	startMu sync.Mutex
	started bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// ParseNTPServersEnv splits a comma-separated NEKTAR_NTP_SERVERS string into clean hostnames.
func ParseNTPServersEnv(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		s := strings.TrimSpace(part)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// DefaultConfigFromEnv builds a Watchdog Config honoring NEKTAR_NTP_SERVERS and network defaults.
func DefaultConfigFromEnv(network string, httpDateURLs []string, onTrip func(string)) Config {
	servers := ParseNTPServersEnv(os.Getenv("NEKTAR_NTP_SERVERS"))
	return Config{
		Network:      network,
		NTPServers:   servers,
		HTTPDateURLs: httpDateURLs,
		OnTrip:       onTrip,
	}
}

// NewWatchdog constructs a Watchdog with fail-closed defaults.
func NewWatchdog(cfg Config) *Watchdog {
	if len(cfg.NTPServers) == 0 {
		cfg.NTPServers = append([]string(nil), DefaultNTPServers...)
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.WarnSkew <= 0 {
		cfg.WarnSkew = DefaultWarnSkew
	}
	if cfg.TripSkew <= 0 {
		cfg.TripSkew = DefaultTripSkew
	}
	if cfg.RecoverSkew <= 0 {
		cfg.RecoverSkew = DefaultRecoverSkew
	}
	if cfg.RecoverConsecutive <= 0 {
		cfg.RecoverConsecutive = DefaultRecoverConsecutive
	}
	if cfg.BreakerWindow <= 0 {
		cfg.BreakerWindow = DefaultBreakerWindow
	}
	if cfg.BreakerCooldown <= 0 {
		cfg.BreakerCooldown = DefaultBreakerCooldown
	}
	if cfg.ConsecutiveFailures == 0 {
		cfg.ConsecutiveFailures = DefaultConsecutiveFailures
	}
	if cfg.SlashBurstThreshold <= 0 {
		cfg.SlashBurstThreshold = DefaultSlashBurstThreshold
	}
	if cfg.PeerBreakerCooldown <= 0 {
		cfg.PeerBreakerCooldown = DefaultPeerBreakerCooldown
	}
	if cfg.PeerConsecutiveFailures == 0 {
		cfg.PeerConsecutiveFailures = DefaultPeerConsecutiveFailures
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 4 * time.Second}
	}
	if cfg.NTPProbe == nil && strings.ToLower(strings.TrimSpace(cfg.Network)) != "memory" {
		cfg.NTPProbe = defaultNTPProbe
	}

	initClockStatus := "pending"
	if cfg.NTPProbe == nil {
		initClockStatus = "disabled"
	}

	w := &Watchdog{
		cfg:          cfg,
		now:          cfg.Now,
		clockStatus:  initClockStatus,
		peerBreakers: make(map[string]*peerBreakerEntry),
	}
	w.spendBreaker = w.newSpendBreaker()
	return w
}

func defaultNTPProbe(ctx context.Context, server string) (time.Duration, error) {
	type res struct {
		offset time.Duration
		err    error
	}
	ch := make(chan res, 1)
	go func() {
		r, err := ntp.QueryWithOptions(server, ntp.QueryOptions{Timeout: 3 * time.Second})
		if err != nil {
			ch <- res{err: err}
			return
		}
		if err := r.Validate(); err != nil {
			ch <- res{err: err}
			return
		}
		ch <- res{offset: r.ClockOffset}
	}()
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case out := <-ch:
		return out.offset, out.err
	}
}

func (w *Watchdog) newSpendBreaker() *gobreaker.CircuitBreaker[struct{}] {
	return gobreaker.NewCircuitBreaker[struct{}](gobreaker.Settings{
		Name:        "nektar-autonomous-spend",
		MaxRequests: 1,
		Interval:    w.cfg.BreakerWindow,
		Timeout:     w.cfg.BreakerCooldown,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= w.cfg.ConsecutiveFailures
		},
		OnStateChange: func(_ string, _ gobreaker.State, to gobreaker.State) {
			if to == gobreaker.StateOpen {
				reason := fmt.Sprintf("spend circuit breaker opened after %d consecutive autonomous failures", w.cfg.ConsecutiveFailures)
				w.mu.Lock()
				w.lastTrippedAt = w.now()
				w.lastTripReason = reason
				cb := w.cfg.OnTrip
				w.mu.Unlock()
				if cb != nil {
					cb(reason)
				}
			}
		},
	})
}

func (w *Watchdog) newPeerBreaker(sellerPub string) *gobreaker.CircuitBreaker[struct{}] {
	return gobreaker.NewCircuitBreaker[struct{}](gobreaker.Settings{
		Name:        "nektar-peer-" + sellerPub,
		MaxRequests: 1,
		Interval:    w.cfg.BreakerWindow,
		Timeout:     w.cfg.PeerBreakerCooldown,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= w.cfg.PeerConsecutiveFailures
		},
	})
}

// Start launches the periodic SNTP clock-skew monitoring loop (no-op when disabled on memory network).
func (w *Watchdog) Start(ctx context.Context) {
	if w == nil || w.cfg.NTPProbe == nil {
		return
	}
	w.startMu.Lock()
	if w.started {
		w.startMu.Unlock()
		return
	}
	w.started = true
	runCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.startMu.Unlock()

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		// Immediate boot check so clock drift is caught before the first autonomous cycle.
		bootCtx, bootCancel := context.WithTimeout(runCtx, 6*time.Second)
		_, _ = w.CheckClockNow(bootCtx)
		bootCancel()

		ticker := time.NewTicker(w.cfg.PollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				checkCtx, checkCancel := context.WithTimeout(runCtx, 6*time.Second)
				_, _ = w.CheckClockNow(checkCtx)
				checkCancel()
			}
		}
	}()
}

// Stop stops the background SNTP polling loop cleanly.
func (w *Watchdog) Stop() {
	if w == nil {
		return
	}
	w.startMu.Lock()
	cancel := w.cancel
	w.started = false
	w.cancel = nil
	w.startMu.Unlock()
	if cancel != nil {
		cancel()
	}
	w.wg.Wait()
}

// CheckClockNow queries all configured NTP servers in parallel, uses the median offset,
// falls back to HTTP Date headers if UDP/123 is blocked, and updates trip/recovery hysteresis.
func (w *Watchdog) CheckClockNow(ctx context.Context) (time.Duration, error) {
	if w == nil || w.cfg.NTPProbe == nil {
		return 0, nil
	}
	type sample struct {
		server string
		offset time.Duration
		err    error
	}
	servers := w.cfg.NTPServers
	results := make(chan sample, len(servers))
	for _, srv := range servers {
		go func(host string) {
			off, err := w.cfg.NTPProbe(ctx, host)
			results <- sample{server: host, offset: off, err: err}
		}(srv)
	}
	var good []sample
	var lastErr error
	for i := 0; i < len(servers); i++ {
		s := <-results
		if s.err == nil {
			good = append(good, s)
		} else {
			lastErr = s.err
		}
	}

	if len(good) > 0 {
		sort.Slice(good, func(i, j int) bool { return good[i].offset < good[j].offset })
		med := good[len(good)/2]
		w.applyClockSample(med.offset, med.server, false, nil)
		return med.offset, nil
	}

	// Graceful fallback when UDP port 123 is firewalled: cross-check HTTP Date headers on configured endpoints.
	if off, src, ok := w.probeHTTPDate(ctx); ok {
		w.applyClockSample(off, src, true, lastErr)
		return off, nil
	}

	w.mu.Lock()
	w.clockCheckedAt = w.now()
	if lastErr != nil {
		w.clockErr = lastErr.Error()
	}
	if !w.clockTripped {
		w.clockStatus = "warn"
	}
	w.mu.Unlock()
	return 0, lastErr
}

func (w *Watchdog) probeHTTPDate(ctx context.Context) (time.Duration, string, bool) {
	if len(w.cfg.HTTPDateURLs) == 0 || w.cfg.HTTPClient == nil {
		return 0, "", false
	}
	type sample struct {
		src    string
		offset time.Duration
	}
	var samples []sample
	for _, rawURL := range w.cfg.HTTPDateURLs {
		u := strings.TrimSpace(rawURL)
		if u == "" {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
		if err != nil {
			continue
		}
		t0 := w.now()
		resp, err := w.cfg.HTTPClient.Do(req)
		if err != nil {
			continue
		}
		t1 := w.now()
		dateHdr := resp.Header.Get("Date")
		_ = resp.Body.Close()
		if dateHdr == "" {
			continue
		}
		parsed, err := http.ParseTime(dateHdr)
		if err != nil {
			continue
		}
		mid := t0.Add(t1.Sub(t0) / 2)
		samples = append(samples, sample{src: "http:" + u, offset: parsed.Sub(mid)})
	}
	if len(samples) == 0 {
		return 0, "", false
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].offset < samples[j].offset })
	med := samples[len(samples)/2]
	return med.offset, med.src, true
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func (w *Watchdog) applyClockSample(offset time.Duration, source string, degradedHTTP bool, ntpErr error) {
	absOff := absDuration(offset)
	now := w.now()
	var tripReason string
	var onTrip func(string)

	w.mu.Lock()
	w.clockOffset = offset
	w.clockCheckedAt = now
	w.clockServer = source
	if ntpErr != nil {
		w.clockErr = ntpErr.Error()
	} else {
		w.clockErr = ""
	}

	switch {
	case absOff >= w.cfg.TripSkew:
		wasTripped := w.clockTripped
		w.clockTripped = true
		w.healthyConsecutive = 0
		w.clockStatus = "tripped"
		if !wasTripped {
			tripReason = fmt.Sprintf("clock skew %s exceeds %s safety threshold (source=%s)", offset.Round(time.Millisecond), w.cfg.TripSkew, source)
			w.lastTrippedAt = now
			w.lastTripReason = tripReason
			onTrip = w.cfg.OnTrip
		}
	case w.clockTripped:
		if absOff < w.cfg.RecoverSkew {
			w.healthyConsecutive++
			if w.healthyConsecutive >= w.cfg.RecoverConsecutive {
				w.clockTripped = false
				if degradedHTTP {
					w.clockStatus = "degraded-http-date"
				} else {
					w.clockStatus = "ok"
				}
			} else {
				w.clockStatus = "tripped"
			}
		} else {
			w.healthyConsecutive = 0
			w.clockStatus = "tripped"
		}
	case absOff >= w.cfg.WarnSkew:
		w.clockStatus = "warn"
	default:
		if degradedHTTP {
			w.clockStatus = "degraded-http-date"
		} else {
			w.clockStatus = "ok"
		}
	}
	w.mu.Unlock()

	if onTrip != nil && tripReason != "" {
		onTrip(tripReason)
	}
}

func (w *Watchdog) pruneSlashesLocked(now time.Time) {
	cutoff := now.Add(-w.cfg.BreakerWindow)
	kept := w.slashTimestamps[:0]
	for _, ts := range w.slashTimestamps {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	w.slashTimestamps = kept
}

// AllowAutonomousSpend returns nil iff neither the clock-skew guard nor the anomaly circuit breaker is tripped.
// Safe to register directly on core.SpendGuard via SetPreCheck.
func (w *Watchdog) AllowAutonomousSpend() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	clockTripped := w.clockTripped
	clockOff := w.clockOffset
	slashPaused := w.now().Before(w.slashPausedUntil)
	slashUntil := w.slashPausedUntil
	cb := w.spendBreaker
	w.mu.Unlock()

	if clockTripped {
		return fmt.Errorf("%w (offset=%s, limit=%s)", ErrClockSkewTripped, clockOff.Round(time.Millisecond), w.cfg.TripSkew)
	}
	if slashPaused {
		rem := time.Until(slashUntil).Round(time.Second)
		if rem < 0 {
			rem = 0
		}
		return fmt.Errorf("%w: slashing anomaly cooldown (%s remaining)", ErrSpendBreakerOpen, rem)
	}
	if cb != nil && cb.State() == gobreaker.StateOpen {
		return fmt.Errorf("%w: spend circuit breaker open", ErrSpendBreakerOpen)
	}
	return nil
}

// ExecuteAutonomousSpend gates fn behind AllowAutonomousSpend and records outcome in the gobreaker circuit breaker.
func (w *Watchdog) ExecuteAutonomousSpend(fn func() error) error {
	if w == nil {
		return fn()
	}
	if err := w.AllowAutonomousSpend(); err != nil {
		return err
	}
	w.mu.Lock()
	cb := w.spendBreaker
	w.mu.Unlock()

	_, err := cb.Execute(func() (struct{}, error) {
		e := fn()
		w.mu.Lock()
		if e != nil {
			w.consecutiveSpendErrs++
		} else {
			w.consecutiveSpendErrs = 0
		}
		w.mu.Unlock()
		return struct{}{}, e
	})
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		return fmt.Errorf("%w: %v", ErrSpendBreakerOpen, err)
	}
	return err
}

// RecordSpendOutcome feeds an external autonomous spend or settlement result into the spend circuit breaker.
func (w *Watchdog) RecordSpendOutcome(err error) {
	if w == nil {
		return
	}
	w.mu.Lock()
	cb := w.spendBreaker
	if err != nil {
		w.consecutiveSpendErrs++
	} else {
		w.consecutiveSpendErrs = 0
	}
	w.mu.Unlock()
	if cb == nil {
		return
	}
	_, _ = cb.Execute(func() (struct{}, error) {
		return struct{}{}, err
	})
}

// RecordSlash records a dead-feed slashing or settlement dispute event.
// When SlashBurstThreshold events accumulate inside BreakerWindow, autonomous spend pauses automatically.
func (w *Watchdog) RecordSlash(sellerPub, reason string) {
	if w == nil {
		return
	}
	now := w.now()
	var tripReason string
	var onTrip func(string)

	w.mu.Lock()
	w.pruneSlashesLocked(now)
	w.slashTimestamps = append(w.slashTimestamps, now)
	if len(w.slashTimestamps) >= w.cfg.SlashBurstThreshold {
		w.slashPausedUntil = now.Add(w.cfg.BreakerCooldown)
		tripReason = fmt.Sprintf("slashing burst (%d events in %s; latest seller=%s: %s)", len(w.slashTimestamps), w.cfg.BreakerWindow, sellerPub, reason)
		w.lastTrippedAt = now
		w.lastTripReason = tripReason
		onTrip = w.cfg.OnTrip
	}
	w.mu.Unlock()

	if onTrip != nil && tripReason != "" {
		onTrip(tripReason)
	}
}

func (w *Watchdog) getOrCreatePeerBreaker(sellerPub string) *gobreaker.CircuitBreaker[struct{}] {
	key := strings.ToLower(strings.TrimSpace(sellerPub))
	if key == "" {
		key = "unknown"
	}
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if entry, ok := w.peerBreakers[key]; ok {
		entry.lastUsed = now
		return entry.cb
	}
	if len(w.peerBreakers) >= maxTrackedPeerBreakers {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range w.peerBreakers {
			if oldestKey == "" || v.lastUsed.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.lastUsed
			}
		}
		if oldestKey != "" {
			delete(w.peerBreakers, oldestKey)
		}
	}
	cb := w.newPeerBreaker(key)
	w.peerBreakers[key] = &peerBreakerEntry{cb: cb, lastUsed: now}
	return cb
}

// AllowPeerQuery reports whether sellerPub is currently permitted for autonomous per_query 402 calls.
func (w *Watchdog) AllowPeerQuery(sellerPub string) error {
	if w == nil {
		return nil
	}
	key := strings.ToLower(strings.TrimSpace(sellerPub))
	w.mu.Lock()
	entry := w.peerBreakers[key]
	w.mu.Unlock()
	if entry != nil && entry.cb.State() == gobreaker.StateOpen {
		return fmt.Errorf("%w (%s)", ErrPeerBreakerOpen, key)
	}
	return nil
}

// ExecutePeerQuery runs fn inside sellerPub's isolated circuit breaker.
func (w *Watchdog) ExecutePeerQuery(sellerPub string, fn func() error) error {
	if w == nil {
		return fn()
	}
	cb := w.getOrCreatePeerBreaker(sellerPub)
	_, err := cb.Execute(func() (struct{}, error) {
		return struct{}{}, fn()
	})
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		return fmt.Errorf("%w (%s): %v", ErrPeerBreakerOpen, sellerPub, err)
	}
	return err
}

// ManualReset clears tripped anomaly breakers, slash cooldowns, isolated peer breakers, and clock trip latch.
func (w *Watchdog) ManualReset() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.clockTripped = false
	w.healthyConsecutive = 0
	if w.clockStatus == "tripped" {
		w.clockStatus = "pending"
	}
	w.spendBreaker = w.newSpendBreaker()
	w.consecutiveSpendErrs = 0
	w.slashTimestamps = nil
	w.slashPausedUntil = time.Time{}
	clear(w.peerBreakers)
	w.mu.Unlock()
}

// Status returns a consistent read-only snapshot of all watchdog safety indicators.
func (w *Watchdog) Status() Status {
	if w == nil {
		return Status{OK: true, ClockStatus: "disabled", SpendBreakerState: "closed"}
	}
	now := w.now()
	w.mu.Lock()
	w.pruneSlashesLocked(now)
	st := Status{
		ClockStatus:          w.clockStatus,
		ClockOffsetMs:        int64(math.Round(float64(w.clockOffset) / float64(time.Millisecond))),
		ClockServer:          w.clockServer,
		ClockError:           w.clockErr,
		SlashEventsWindow:    len(w.slashTimestamps),
		ConsecutiveSpendErrs: w.consecutiveSpendErrs,
		LastTripReason:       w.lastTripReason,
	}
	if !w.clockCheckedAt.IsZero() {
		st.ClockCheckedAt = w.clockCheckedAt.Unix()
	}
	if !w.lastTrippedAt.IsZero() {
		st.LastTrippedAt = w.lastTrippedAt.Unix()
	}
	if w.spendBreaker != nil {
		st.SpendBreakerState = strings.ToLower(w.spendBreaker.State().String())
	} else {
		st.SpendBreakerState = "closed"
	}
	if w.clockTripped {
		st.SpendPaused = true
		st.PausedReason = fmt.Sprintf("clock skew %dms exceeds %s limit", st.ClockOffsetMs, w.cfg.TripSkew)
	} else if now.Before(w.slashPausedUntil) {
		st.SpendPaused = true
		st.PausedReason = fmt.Sprintf("slashing burst cooldown (%d events)", len(w.slashTimestamps))
	} else if w.spendBreaker != nil && w.spendBreaker.State() == gobreaker.StateOpen {
		st.SpendPaused = true
		st.PausedReason = "spend anomaly circuit breaker open"
	}
	for pub, entry := range w.peerBreakers {
		if entry.cb.State() == gobreaker.StateOpen {
			st.IsolatedPeers = append(st.IsolatedPeers, pub)
		}
	}
	sort.Strings(st.IsolatedPeers)
	st.OK = !st.SpendPaused && st.ClockStatus != "tripped"
	w.mu.Unlock()
	return st
}
