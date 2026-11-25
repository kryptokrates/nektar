package core

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestSafeHTTPBlocksPrivateMetadataAndLoopback(t *testing.T) {
	blockedIPs := []string{
		"127.0.0.1",
		"10.0.0.1",
		"172.16.0.1",
		"192.168.1.1",
		"169.254.169.254",
		"100.64.0.1",
		"0.0.0.0",
		"::1",
		"fc00::1",
		"fe80::1",
		"::ffff:127.0.0.1",
		"::ffff:169.254.169.254",
	}
	for _, s := range blockedIPs {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", s, err)
		}
		if !IsBlockedOutboundIP(addr) {
			t.Errorf("expected IsBlockedOutboundIP(%q) == true", s)
		}
	}

	if IsBlockedOutboundIP(netip.MustParseAddr("1.1.1.1")) {
		t.Error("expected public IP 1.1.1.1 to be allowed")
	}

	opts := SafeHTTPOpts{Timeout: 2 * time.Second, AllowPrivate: false, RequireHTTPS: false}
	blockedURLs := []string{
		"http://127.0.0.1:8080/secret",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/internal",
		"http://localhost:18420/v1/status",
		"http://sub.localhost/test",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://user:pass@1.1.1.1/test",
		"file:///etc/passwd",
		"http://[::1]:8080/",
	}
	for _, raw := range blockedURLs {
		if _, err := ValidateOutboundURL(raw, opts); err == nil {
			t.Errorf("ValidateOutboundURL(%q) unexpectedly succeeded", raw)
		}
	}

	// Mainnet SafeHTTPClientForNetwork must refuse dialing a local httptest server on 127.0.0.1.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secret"))
	}))
	defer srv.Close()

	client := SafeHTTPClientForNetwork("testnet", 2*time.Second)
	if resp, err := client.Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatalf("expected SafeHTTPClientForNetwork(testnet) to block loopback %s", srv.URL)
	}

	// Memory/regtest client must still reach local httptest server for unit/integration tests.
	memClient := SafeHTTPClientForNetwork("memory", 2*time.Second)
	resp, err := memClient.Get(srv.URL)
	if err != nil {
		t.Fatalf("expected memory SafeHTTPClient to reach local test server: %v", err)
	}
	resp.Body.Close()
}
