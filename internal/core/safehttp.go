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
	"time"
)

// SafeHTTPOpts configures SSRF-hardened outbound HTTP clients for peer, hub, CHIRP, and federation calls.
type SafeHTTPOpts struct {
	Timeout         time.Duration
	AllowPrivate    bool // strictly true ONLY for local memory/regtest networks
	RequireHTTPS    bool // true on mainnet
	MaxRedirects    int  // 0 disables cross-origin redirects
	AllowAnyPort    bool
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

// SafeHTTPClient returns an *http.Client whose DialContext resolves DNS and blocks private/loopback/metadata
// IPs (pinning the resolved IP to prevent DNS-rebinding TOCTOU) and re-validates every redirect hop.
func SafeHTTPClient(opts SafeHTTPOpts) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	tr := &http.Transport{
		Proxy:                 nil, // never inherit untrusted HTTP_PROXY for peer dials
		MaxIdleConns:          64,
		IdleConnTimeout:       60 * time.Second,
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
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", low)
			if err != nil || len(ips) == 0 {
				return nil, fmt.Errorf("safehttp: resolve %q: %w", host, err)
			}
			for _, ip := range ips {
				if IsBlockedOutboundIP(ip) {
					return nil, fmt.Errorf("safehttp: resolved blocked IP %s for host %q", ip.String(), host)
				}
			}
			// Dial the first validated IP directly to eliminate DNS rebinding TOCTOU.
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].Unmap().String(), port))
		},
	}
	maxRedir := opts.MaxRedirects
	if maxRedir <= 0 {
		maxRedir = 3
	}
	return &http.Client{
		Timeout:   opts.Timeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedir {
				return errors.New("safehttp: too many redirects")
			}
			if _, err := ValidateOutboundURL(req.URL.String(), opts); err != nil {
				return err
			}
			// Strip Authorization / custom Nektar auth headers on cross-origin redirects.
			if len(via) > 0 && !strings.EqualFold(via[0].URL.Host, req.URL.Host) {
				req.Header.Del("Authorization")
				req.Header.Del("X-Nektar-Sig")
				req.Header.Del("X-Nektar-Token")
			}
			return nil
		},
	}
}

// AllowPrivateOutboundForNetwork reports whether private/loopback outbound peer URLs are
// permitted for network. Live networks (main/testnet) block private/loopback peers by default
// and only permit them when NEKTAR_ALLOW_LOCAL_PEERS=1 is explicitly set by the operator
// (e.g. co-located Docker Compose hubs or local multi-node clusters).
func AllowPrivateOutboundForNetwork(network string) bool {
	n := strings.ToLower(strings.TrimSpace(network))
	return n == "memory" || n == "regtest" || n == "" || ((n == "testnet" || n == "main") && os.Getenv("NEKTAR_ALLOW_LOCAL_PEERS") == "1")
}

// SafeHTTPClientForNetwork returns an SSRF-hardened *http.Client calibrated to the BSV network:
// memory/regtest allow loopback test servers; main strictly blocks private/loopback/metadata IPs
// and enforces HTTPS by default; testnet/main only permit local/private peers when
// NEKTAR_ALLOW_LOCAL_PEERS=1 is explicitly opted in by the operator.
func SafeHTTPClientForNetwork(network string, timeout time.Duration) *http.Client {
	n := strings.ToLower(strings.TrimSpace(network))
	allowPriv := AllowPrivateOutboundForNetwork(n)
	return SafeHTTPClient(SafeHTTPOpts{
		Timeout:      timeout,
		AllowPrivate: allowPriv,
		RequireHTTPS: n == "main" && !allowPriv,
	})
}
