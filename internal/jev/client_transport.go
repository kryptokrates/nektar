package jev

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
)

func isHostedThirdPartyHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	return h == "typesafe.ai" || strings.HasSuffix(h, ".typesafe.ai") ||
		h == "openrouter.ai" || strings.HasSuffix(h, ".openrouter.ai")
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	if h == "localhost" {
		return true
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		return addr.Unmap().IsLoopback()
	}
	return false
}

// loopbackHTTPClient strictly pins dials to verified loopback IPs (127.0.0.1 / ::1) and blocks
// redirects and proxy inheritance so local open-weight sidecars work safely on all networks including main.
func loopbackHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	tr := &http.Transport{
		Proxy:                 nil,
		MaxIdleConns:          32,
		IdleConnTimeout:       60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("jev: invalid loopback dial address %q: %w", addr, err)
			}
			low := strings.ToLower(strings.Trim(host, "[]"))
			if !isLoopbackHost(low) {
				return nil, fmt.Errorf("jev: non-loopback host %q rejected by loopback client", host)
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", low)
			if err != nil || len(ips) == 0 {
				return nil, fmt.Errorf("jev: resolve loopback host %q: %w", host, err)
			}
			for _, ip := range ips {
				if !ip.Unmap().IsLoopback() {
					return nil, fmt.Errorf("jev: resolved non-loopback IP %s for %q", ip.String(), host)
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].Unmap().String(), port))
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("jev: redirects are disabled for local open-weight endpoints")
		},
	}
}

// ResolveEndpoint normalizes an optional self-hosted baseURL into a concrete System One POST endpoint.
func ResolveEndpoint(baseURL string) (string, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return DefaultBaseURL, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("jev: invalid base URL %q", raw)
	}
	if isHostedThirdPartyHost(u.Hostname()) {
		return "", fmt.Errorf("%w: %q", ErrHostedProviderDisallowed, u.Hostname())
	}
	path := strings.TrimRight(u.Path, "/")
	if path == "" || path == "/v1" {
		u.Path = "/v1/systemone"
	}
	return u.String(), nil
}

func requestCacheKey(req Request) (string, error) {
	canon, err := core.CanonicalJSON(req)
	if err != nil {
		return "", fmt.Errorf("jev: canonicalize cache key: %w", err)
	}
	return hex.EncodeToString(core.SHA256(canon)), nil
}
