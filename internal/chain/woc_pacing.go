package chain

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// wocPacingTransport serializes requests to unkeyed api.whatsonchain.com under its public
// 3 req/sec free-tier rate limit and retries transient HTTP 429 responses with bounded backoff.
// Non-WoC hosts (Arcade, Chaintracks, local Hubs, A2A peers) bypass pacing completely.
type wocPacingTransport struct {
	base       http.RoundTripper
	minSpacing time.Duration
	mu         sync.Mutex
	nextAt     time.Time
}

func newWoCPacingTransport(base http.RoundTripper, minSpacing time.Duration) *wocPacingTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	if minSpacing <= 0 {
		minSpacing = 400 * time.Millisecond
	}
	return &wocPacingTransport{
		base:       base,
		minSpacing: minSpacing,
	}
}

func isWoCHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if idx := strings.IndexByte(h, ':'); idx >= 0 {
		h = h[:idx]
	}
	return h == "api.whatsonchain.com" || strings.HasSuffix(h, ".whatsonchain.com")
}

func (t *wocPacingTransport) waitSlot(req *http.Request) error {
	t.mu.Lock()
	now := time.Now()
	wait := time.Duration(0)
	if now.Before(t.nextAt) {
		wait = t.nextAt.Sub(now)
		t.nextAt = t.nextAt.Add(t.minSpacing)
	} else {
		t.nextAt = now.Add(t.minSpacing)
	}
	t.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-req.Context().Done():
		return req.Context().Err()
	case <-timer.C:
		return nil
	}
}

func (t *wocPacingTransport) pushBack(delay time.Duration) {
	t.mu.Lock()
	target := time.Now().Add(delay)
	if target.After(t.nextAt) {
		t.nextAt = target
	}
	t.mu.Unlock()
}

func (t *wocPacingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || !isWoCHost(req.URL.Host) {
		return t.base.RoundTrip(req)
	}
	// Only retry safe idempotent GET/HEAD requests on 429.
	maxAttempts := 1
	if req.Method == http.MethodGet || req.Method == http.MethodHead {
		maxAttempts = 3
	}
	var resp *http.Response
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if wErr := t.waitSlot(req); wErr != nil {
			return nil, wErr
		}
		resp, err = t.base.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt+1 >= maxAttempts {
			return resp, nil
		}
		backoff := time.Duration(attempt+1) * 1500 * time.Millisecond
		if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
			if secs, pErr := strconv.Atoi(ra); pErr == nil && secs > 0 && secs <= 10 {
				backoff = time.Duration(secs) * time.Second
			}
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		t.pushBack(backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}
	}
	return resp, err
}
