package jev

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/kryptokrates/nektar/internal/core"
)

var (
	ErrAPIKeyRequired      = errors.New("jev: API key is required")
	ErrHostNotAllowed      = errors.New("jev: endpoint host is not in operator policy allowedHosts")
	ErrCircuitOpen         = errors.New("jev: circuit breaker is open")
	ErrTokenBudgetExceeded = errors.New("jev: daily input token cap exceeded")
)

const maxResponseBytes = 1 << 20 // 1 MiB cap on upstream System One JSON responses

// Config configures the pure-Go Jev System One HTTP client.
type Config struct {
	Provider           string        // "typesafe" (default) | "openrouter"
	BaseURL            string        // optional override (full endpoint or gateway base URL)
	APIKey             string        // Bearer token (from NEKTAR_JEV_API_KEY / _FILE)
	Model              string        // default "jev-latest" (typesafe) or "typesafe/jev-1.14" (openrouter)
	Network            string        // BSV network name calibrating SSRF/HTTPS policy
	AllowedHosts       []string      // optional operator-pinned host allowlist (empty = default provider host)
	Timeout            time.Duration // default 2500ms
	MaxConcurrent      int           // default 4
	RatePerMinute      int           // default 60
	DailyInputTokenCap int64         // default 2_000_000 (0 = unlimited)
	CacheTTL           time.Duration // default 5m
	CacheSize          int           // default 512
	CircuitThreshold   int           // default 5 consecutive failures
	CircuitCooldown    time.Duration // default 30s
	HTTPClient         *http.Client  // optional test override
	Now                func() time.Time
}

type cacheEntry struct {
	resp      *Response
	expiresAt time.Time
	lastUsed  time.Time
}

// Client is a concurrency-bounded, rate-limited, cached, SSRF-hardened Jev System One client.
type Client struct {
	provider      string
	endpointURL   string
	endpointHost  string
	apiKey        string
	model         string
	network       string
	httpClient    *http.Client
	sem           chan struct{}
	limiter       *rate.Limiter
	dailyTokenCap int64
	cacheTTL      time.Duration
	cacheSize     int
	cbThreshold   int
	cbCooldown    time.Duration
	nowFn         func() time.Time

	mu           sync.Mutex
	allowedHosts map[string]bool
	consecFails  int
	circuitUntil time.Time
	dayKey       string
	dailyTokens  int64
	cache        map[string]cacheEntry

	cacheHits atomic.Int64
}

// ResolveEndpoint normalizes provider and optional baseURL into a concrete System One POST endpoint.
func ResolveEndpoint(provider, baseURL string) (normProvider, endpoint string, err error) {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		p = ProviderTypeSafe
	}
	if p != ProviderTypeSafe && p != ProviderOpenRouter {
		return "", "", fmt.Errorf("jev: unsupported provider %q (use %q or %q)", p, ProviderTypeSafe, ProviderOpenRouter)
	}
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		if p == ProviderOpenRouter {
			return p, DefaultOpenRouterURL, nil
		}
		return p, DefaultTypeSafeURL, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", fmt.Errorf("jev: invalid base URL %q", raw)
	}
	path := strings.TrimRight(u.Path, "/")
	if path == "" {
		if p == ProviderOpenRouter {
			u.Path = "/api/alpha/decisions"
		} else {
			u.Path = "/v1/systemone"
		}
	} else if path == "/v1" && p == ProviderTypeSafe {
		u.Path = "/v1/systemone"
	} else if path == "/api/v1" && p == ProviderOpenRouter {
		u.Path = "/api/alpha/decisions"
	}
	return p, u.String(), nil
}

// NewClient constructs a hardened Jev System One client.
func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, ErrAPIKeyRequired
	}
	provider, endpoint, err := ResolveEndpoint(cfg.Provider, cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	netName := strings.ToLower(strings.TrimSpace(cfg.Network))
	safeOpts := core.SafeHTTPOpts{
		Timeout:      cfg.Timeout,
		AllowPrivate: netName == "memory" || netName == "regtest" || netName == "",
		RequireHTTPS: netName == "main",
	}
	if safeOpts.Timeout <= 0 {
		safeOpts.Timeout = 2500 * time.Millisecond
	}
	u, err := core.ValidateOutboundURL(endpoint, safeOpts)
	if err != nil {
		return nil, fmt.Errorf("jev: invalid endpoint: %w", err)
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		if provider == ProviderOpenRouter {
			model = DefaultOpenRouterModel
		} else {
			model = DefaultTypeSafeModel
		}
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = core.SafeHTTPClient(safeOpts)
	}
	maxConc := cfg.MaxConcurrent
	if maxConc <= 0 {
		maxConc = 4
	}
	rpm := cfg.RatePerMinute
	if rpm <= 0 {
		rpm = 60
	}
	burst := max(maxConc, rpm/6)
	if burst < 2 {
		burst = 2
	}
	tokenCap := cfg.DailyInputTokenCap
	if tokenCap == 0 {
		tokenCap = 2_000_000
	}
	cacheTTL := cfg.CacheTTL
	if cacheTTL <= 0 {
		cacheTTL = 5 * time.Minute
	}
	cacheSize := cfg.CacheSize
	if cacheSize <= 0 {
		cacheSize = 512
	}
	cbThresh := cfg.CircuitThreshold
	if cbThresh <= 0 {
		cbThresh = 5
	}
	cbCooldown := cfg.CircuitCooldown
	if cbCooldown <= 0 {
		cbCooldown = 30 * time.Second
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	c := &Client{
		provider:      provider,
		endpointURL:   endpoint,
		endpointHost:  strings.ToLower(u.Hostname()),
		apiKey:        strings.TrimSpace(cfg.APIKey),
		model:         model,
		network:       netName,
		httpClient:    hc,
		sem:           make(chan struct{}, maxConc),
		limiter:       rate.NewLimiter(rate.Every(time.Minute/time.Duration(rpm)), burst),
		dailyTokenCap: tokenCap,
		cacheTTL:      cacheTTL,
		cacheSize:     cacheSize,
		cbThreshold:   cbThresh,
		cbCooldown:    cbCooldown,
		nowFn:         nowFn,
		dayKey:        nowFn().UTC().Format("2006-01-02"),
		cache:         make(map[string]cacheEntry),
	}
	if len(cfg.AllowedHosts) > 0 {
		c.SetAllowedHosts(cfg.AllowedHosts)
	}
	return c, nil
}

// EndpointHost returns the lowercase hostname of the configured Jev endpoint.
func (c *Client) EndpointHost() string { return c.endpointHost }

// Model returns the default configured model identifier.
func (c *Client) Model() string { return c.model }

// Provider returns the configured provider ("typesafe" | "openrouter").
func (c *Client) Provider() string { return c.provider }

// SetAllowedHosts updates the operator-policy host allowlist at runtime.
// Passing an empty slice clears explicit host restrictions (leaving SSRF rules active).
func (c *Client) SetAllowedHosts(hosts []string) {
	m := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if strings.Contains(h, "://") {
			if u, err := url.Parse(h); err == nil && u.Hostname() != "" {
				h = strings.ToLower(u.Hostname())
			}
		}
		m[h] = true
	}
	c.mu.Lock()
	if len(m) == 0 {
		c.allowedHosts = nil
	} else {
		c.allowedHosts = m
	}
	c.mu.Unlock()
}

// Health returns a point-in-time snapshot of the client's breaker, token usage, and cache state.
func (c *Client) Health() HealthSnapshot {
	now := c.nowFn()
	c.mu.Lock()
	c.rollDayLocked(now)
	open := !c.circuitUntil.IsZero() && now.Before(c.circuitUntil)
	snap := HealthSnapshot{
		Provider:            c.provider,
		Endpoint:            c.endpointURL,
		Model:               c.model,
		CircuitOpen:         open,
		ConsecutiveFailures: c.consecFails,
		DailyInputTokens:    c.dailyTokens,
		DailyTokenCap:       c.dailyTokenCap,
		CacheEntries:        len(c.cache),
		CacheHits:           c.cacheHits.Load(),
	}
	c.mu.Unlock()
	return snap
}

func (c *Client) rollDayLocked(now time.Time) {
	today := now.UTC().Format("2006-01-02")
	if today != c.dayKey {
		c.dayKey = today
		c.dailyTokens = 0
	}
}

// Evaluate sends a single parallel System One request to Jev AI, enforcing host allowlist,
// TTL cache, circuit breaker, daily token budget, concurrency semaphore, and rate limiting.
func (c *Client) Evaluate(ctx context.Context, req Request) (*Response, error) {
	if len(req.Questions) == 0 {
		return nil, errors.New("jev: at least one question is required")
	}
	if strings.TrimSpace(req.Model) == "" {
		req.Model = c.model
	}

	// Enforce operator-pinned host allowlist (fail closed on policy drift).
	c.mu.Lock()
	if len(c.allowedHosts) > 0 && !c.allowedHosts["*"] && !c.allowedHosts[c.endpointHost] {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", ErrHostNotAllowed, c.endpointHost)
	}
	c.mu.Unlock()

	cacheKey, err := requestCacheKey(req)
	if err != nil {
		return nil, err
	}
	now := c.nowFn()

	// Fast-path content-addressed TTL cache lookup.
	c.mu.Lock()
	c.rollDayLocked(now)
	if ent, ok := c.cache[cacheKey]; ok {
		if now.Before(ent.expiresAt) {
			ent.lastUsed = now
			c.cache[cacheKey] = ent
			c.mu.Unlock()
			c.cacheHits.Add(1)
			return ent.resp.Clone(), nil
		}
		delete(c.cache, cacheKey)
	}
	if !c.circuitUntil.IsZero() && now.Before(c.circuitUntil) {
		c.mu.Unlock()
		return nil, ErrCircuitOpen
	}
	if c.dailyTokenCap > 0 && c.dailyTokens >= c.dailyTokenCap {
		c.mu.Unlock()
		return nil, ErrTokenBudgetExceeded
	}
	c.mu.Unlock()

	// Acquire bounded concurrency slot + rate limiter token.
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("jev: rate limit wait: %w", err)
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("jev: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("jev: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		c.recordFailure()
		return nil, fmt.Errorf("jev: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if readErr != nil {
		c.recordFailure()
		return nil, fmt.Errorf("jev: read response: %w", readErr)
	}
	if len(body) > maxResponseBytes {
		c.recordFailure()
		return nil, fmt.Errorf("jev: response exceeds %d byte cap", maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			c.recordFailure()
		}
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 240 {
			snippet = snippet[:240] + "..."
		}
		return nil, fmt.Errorf("jev: upstream HTTP %d: %s", resp.StatusCode, snippet)
	}

	var parsed Response
	if err := json.Unmarshal(body, &parsed); err != nil {
		c.recordFailure()
		return nil, fmt.Errorf("jev: decode response: %w", err)
	}
	if err := ValidateResponse(req, &parsed); err != nil {
		return nil, err
	}

	c.recordSuccess(cacheKey, &parsed)
	return parsed.Clone(), nil
}

func (c *Client) recordFailure() {
	now := c.nowFn()
	c.mu.Lock()
	c.consecFails++
	if c.consecFails >= c.cbThreshold {
		c.circuitUntil = now.Add(c.cbCooldown)
	}
	c.mu.Unlock()
}

func (c *Client) recordSuccess(cacheKey string, resp *Response) {
	now := c.nowFn()
	c.mu.Lock()
	c.consecFails = 0
	c.circuitUntil = time.Time{}
	c.rollDayLocked(now)
	if resp.Usage.InputTokens > 0 {
		c.dailyTokens += int64(resp.Usage.InputTokens)
	}
	if c.cacheTTL > 0 && c.cacheSize > 0 {
		if len(c.cache) >= c.cacheSize {
			// Evict expired entries first, then oldest-used entry.
			for k, ent := range c.cache {
				if !now.Before(ent.expiresAt) {
					delete(c.cache, k)
				}
			}
			if len(c.cache) >= c.cacheSize {
				var victim string
				var oldest time.Time
				for k, ent := range c.cache {
					if victim == "" || ent.lastUsed.Before(oldest) {
						victim = k
						oldest = ent.lastUsed
					}
				}
				if victim != "" {
					delete(c.cache, victim)
				}
			}
		}
		c.cache[cacheKey] = cacheEntry{
			resp:      resp.Clone(),
			expiresAt: now.Add(c.cacheTTL),
			lastUsed:  now,
		}
	}
	c.mu.Unlock()
}

func requestCacheKey(req Request) (string, error) {
	canon, err := core.CanonicalJSON(req)
	if err != nil {
		return "", fmt.Errorf("jev: canonicalize cache key: %w", err)
	}
	return hex.EncodeToString(core.SHA256(canon)), nil
}
