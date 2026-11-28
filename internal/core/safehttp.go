package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type cachedDNSResult struct {
	ips       []netip.Addr
	expiresAt time.Time
}

var (
	safeDNSMu    sync.RWMutex
	safeDNSCache = make(map[string]cachedDNSResult, 256)
)

const (
	safeDNSCacheTTL = 30 * time.Second
	safeDNSCacheCap = 4096
)

func lookupSafeHostIPs(ctx context.Context, host string) ([]netip.Addr, error) {
	now := time.Now()
	safeDNSMu.RLock()
	if hit, ok := safeDNSCache[host]; ok && now.Before(hit.expiresAt) && len(hit.ips) > 0 {
		ips := hit.ips
		safeDNSMu.RUnlock()
		return ips, nil
	}
	safeDNSMu.RUnlock()

	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("safehttp: resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if IsBlockedOutboundIP(ip) {
			return nil, fmt.Errorf("safehttp: resolved blocked IP %s for host %q", ip.String(), host)
		}
	}
	copied := append([]netip.Addr(nil), ips...)
	safeDNSMu.Lock()
	if len(safeDNSCache) >= safeDNSCacheCap {
		for k, v := range safeDNSCache {
			if now.After(v.expiresAt) {
				delete(safeDNSCache, k)
			}
		}
		if len(safeDNSCache) >= safeDNSCacheCap {
			clear(safeDNSCache)
		}
	}
	safeDNSCache[host] = cachedDNSResult{ips: copied, expiresAt: now.Add(safeDNSCacheTTL)}
	safeDNSMu.Unlock()
	return copied, nil
}

// SafeHTTPOpts configures SSRF-hardened outbound HTTP clients for peer, hub, CHIRP, and federation calls.
type SafeHTTPOpts struct {
	Timeout      time.Duration
	AllowPrivate bool // strictly true ONLY for local memory/regtest networks
	RequireHTTPS bool // true on mainnet
	MaxRedirects int  // 0 disables cross-origin redirects (up to 3 same-origin hops); < 0 disables all redirects
}

// IsBlockedOutboundIP reports whether addr is loopback, private (RFC1918/ULA), link-local/metadata,
// CGNAT (100.64/10), multicast, or unspecified.
func IsBlockedOutboundIP(addr netip.Addr) bool {
	a := addr.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() {
		return true
	}
	// Block CGNAT 100.64.0.0/10 and documentation/benchmarking/reserved ranges.
	if a.Is4() {
		b := a.As4()
		if b[0] == 100 && b[1] >= 64 && b[1] <= 127 {
			return true
		}
		if b[0] == 0 || b[0] >= 240 {
			return true
		}
	}
	return false
}

// ValidateOutboundURL validates scheme, userinfo, and host before dialing an untrusted peer/hub URL.
func ValidateOutboundURL(rawURL string, opts SafeHTTPOpts) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("safehttp: invalid url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if opts.RequireHTTPS {
		if scheme != "https" {
			return nil, fmt.Errorf("safehttp: https required (got %q)", u.Scheme)
		}
	} else if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("safehttp: unsupported scheme %q", u.Scheme)
	}
	if u.User != nil {
		return nil, errors.New("safehttp: URL userinfo is forbidden")
	}
	host := u.Hostname()
	if host == "" {
		return nil, errors.New("safehttp: missing host")
	}
	if !opts.AllowPrivate {
		low := strings.ToLower(host)
		if low == "localhost" || strings.HasSuffix(low, ".localhost") || strings.HasSuffix(low, ".local") || strings.HasSuffix(low, ".internal") {
			return nil, fmt.Errorf("safehttp: private/local hostname %q blocked", host)
		}
		if addr, err := netip.ParseAddr(host); err == nil && IsBlockedOutboundIP(addr) {
			return nil, fmt.Errorf("safehttp: private/loopback IP %q blocked", host)
		}
	}
	return u, nil
}

type safeRoundTripper struct {
	base http.RoundTripper
	opts SafeHTTPOpts
}

func (s *safeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("safehttp: nil request or url")
	}
	if _, err := ValidateOutboundURL(req.URL.String(), s.opts); err != nil {
		return nil, err
	}
	return s.base.RoundTrip(req)
}

// SafeHTTPClient returns an *http.Client whose RoundTripper validates every request URL
// (including RequireHTTPS, userinfo, and local suffixes), whose DialContext resolves DNS and
// blocks private/loopback/metadata IPs (pinning the validated IP to prevent DNS-rebinding TOCTOU),
// and whose CheckRedirect re-validates every redirect hop.
func SafeHTTPClient(opts SafeHTTPOpts) *http.Client {
	return SafeHTTPClientWithWrapper(opts, nil)
}

// SafeHTTPClientWithWrapper returns an SSRF-hardened *http.Client and optionally wraps the validated
// RoundTripper (e.g. with rate-limit pacing) without bypassing DNS-rebinding or URL validation.
func SafeHTTPClientWithWrapper(opts SafeHTTPOpts, wrap func(http.RoundTripper) http.RoundTripper) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	tr := &http.Transport{
		Proxy:                 nil, // never inherit untrusted HTTP_PROXY for peer dials
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("safehttp: invalid dial address %q: %w", addr, err)
			}
			if opts.AllowPrivate {
				return dialer.DialContext(ctx, network, addr)
			}
			low := strings.ToLower(strings.Trim(host, "[]"))
			if low == "localhost" || strings.HasSuffix(low, ".localhost") {
				return nil, fmt.Errorf("safehttp: loopback host %q blocked", host)
			}
			var ips []netip.Addr
			if parsedIP, pErr := netip.ParseAddr(low); pErr == nil {
				if IsBlockedOutboundIP(parsedIP) {
					return nil, fmt.Errorf("safehttp: resolved blocked IP %s for host %q", parsedIP.String(), host)
				}
				ips = []netip.Addr{parsedIP}
			} else {
				ips, err = lookupSafeHostIPs(ctx, low)
				if err != nil {
					return nil, err
				}
			}
			// Dial validated IPs directly (trying each on failure for dual-stack resilience)
			// while eliminating DNS rebinding TOCTOU.
			var lastErr error
			for _, ip := range ips {
				conn, dErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
				if dErr == nil {
					return conn, nil
				}
				lastErr = dErr
				if ctx.Err() != nil {
					break
				}
			}
			return nil, lastErr
		},
	}
	maxRedir := opts.MaxRedirects
	if maxRedir == 0 {
		maxRedir = 3
	}
	var rt http.RoundTripper = &safeRoundTripper{base: tr, opts: opts}
	if wrap != nil {
		rt = wrap(rt)
	}
	return &http.Client{
		Timeout:   opts.Timeout,
		Transport: rt,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if opts.MaxRedirects < 0 {
				return errors.New("safehttp: redirects disabled")
			}
			if len(via) >= maxRedir {
				return errors.New("safehttp: too many redirects")
			}
			if _, err := ValidateOutboundURL(req.URL.String(), opts); err != nil {
				return err
			}
			isCrossOrigin := len(via) > 0 && !strings.EqualFold(via[0].URL.Host, req.URL.Host)
			if isCrossOrigin {
				if opts.MaxRedirects == 0 {
					return errors.New("safehttp: cross-origin redirect blocked")
				}
				// Strip Authorization / custom Nektar auth headers on cross-origin redirects.
				req.Header.Del("Authorization")
				req.Header.Del("X-Nektar-Sig")
				req.Header.Del("X-Nektar-Token")
			}
			return nil
		},
	}
}

// AllowPrivateOutboundForNetwork reports whether private/loopback outbound peer URLs are
// permitted for network. Live networks (main/testnet) and empty/unknown network names block
// private/loopback peers by default; testnet/main permit them only when NEKTAR_ALLOW_LOCAL_PEERS=1
// is explicitly set by the operator (e.g. co-located Docker Compose hubs or local multi-node clusters).
func AllowPrivateOutboundForNetwork(network string) bool {
	n := strings.ToLower(strings.TrimSpace(network))
	return n == "memory" || n == "regtest" || ((n == "testnet" || n == "main") && os.Getenv("NEKTAR_ALLOW_LOCAL_PEERS") == "1")
}

// SafeHTTPClientForNetwork returns an SSRF-hardened *http.Client calibrated to the BSV network:
// memory/regtest allow loopback test servers; main strictly blocks private/loopback/metadata IPs
// and enforces HTTPS by default; testnet/main only permit local/private peers when
// NEKTAR_ALLOW_LOCAL_PEERS=1 is explicitly opted in by the operator.
func SafeHTTPClientForNetwork(network string, timeout time.Duration) *http.Client {
	return SafeHTTPClientForNetworkWithWrapper(network, timeout, nil)
}

// SafeHTTPClientForNetworkWithWrapper returns a network-calibrated SSRF-hardened *http.Client with an optional RoundTripper wrapper.
func SafeHTTPClientForNetworkWithWrapper(network string, timeout time.Duration, wrap func(http.RoundTripper) http.RoundTripper) *http.Client {
	n := strings.ToLower(strings.TrimSpace(network))
	allowPriv := AllowPrivateOutboundForNetwork(n)
	return SafeHTTPClientWithWrapper(SafeHTTPOpts{
		Timeout:      timeout,
		AllowPrivate: allowPriv,
		RequireHTTPS: n == "main" && !allowPriv,
	}, wrap)
}
