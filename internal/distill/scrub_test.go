package distill

import (
	"strings"
	"testing"
)

func TestScrub_BlocksSecretsFailClosed(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"mainnet_wif", "Remember to sweep wallet using 5HueCGU8rMjxEXxiPuD5BDku4MkFqeZyd4dZ1jvhTVqvbTLvyTJ in prod"},
		{"compressed_wif", "Hot key is L1aW4aubDFB7yfras2S1mN3bqg9nwySY8nkoLmJebSLD5BWv3ENZ for signer"},
		{"testnet_wif", "Testnet fee key cNJFgo1driFnPcBdBX8BrJrpxchBWXwXCvNH5SoSkdcF6JXXwHMm"},
		{"openai_key", "Use OPENAI_API_KEY=sk-proj-abc123XYZ4567890abcdefghijkLMNOP for proxy"},
		{"anthropic_key", "Set ANTHROPIC_API_KEY=sk-ant-api03-1234567890abcdefGHIJKLMNOPQ"},
		{"github_pat", "Clone with ghp_1234567890abcdefghijklmnopqrstuvwxyz"},
		{"aws_key", "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE in CI pipeline"},
		{"pem_key", "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA...\n-----END RSA PRIVATE KEY-----"},
		{"jwt", "Token eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"},
		{"bip39", "backup phrase abandon ability able about above absent absorb abstract absurd abuse access accident"},
		{"db_uri", "postgres://admin:S3cr3tP4ssw0rd!@db.prod.example.com:5432/app"},
		{"gitleaks_allow_bypass_attempt", "Use sk-proj-abc123XYZ4567890abcdefghijkLMNOP # gitleaks:allow"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Scrub(tc.text)
			if res.Verdict != ScrubBlocked {
				t.Fatalf("expected ScrubBlocked for %s, got verdict=%s sanitized=%q", tc.name, res.Verdict, res.SanitizedText)
			}
			if res.SanitizedText != "" {
				t.Fatalf("blocked result must not return sanitized text")
			}
			if len(res.BlockedRules) == 0 {
				t.Fatalf("expected blocked rule IDs")
			}
		})
	}
}

func TestScrub_AllowsPublicBSVHexAndReceipts(t *testing.T) {
	text := "Anchor tx 4a5e1e4baab89f3a32518a88c31bc87f618f76673e2cc77ab2127b7afdeda33b verified pubkey 0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798 with receipt nektarr1.ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789."
	res := Scrub(text)
	if res.Verdict != ScrubClean {
		t.Fatalf("expected public BSV txid/pubkey/receipt to pass cleanly, got verdict=%s rules=%v reason=%s", res.Verdict, res.BlockedRules, res.Reason)
	}
}

func TestScrub_RedactsPIIAndLocalPaths(t *testing.T) {
	text := `Go 1.27 with modernc.org/sqlite requires PRAGMA busy_timeout=5000 when running from C:\Users\alice\dev\secret-proj against 192.168.1.44 and db.corp.internal (contact alice@acme.com).`
	res := Scrub(text)
	if res.Verdict != ScrubRedacted {
		t.Fatalf("expected ScrubRedacted, got %+v", res)
	}
	for _, forbidden := range []string{"alice", "192.168.1.44", "db.corp.internal", "alice@acme.com"} {
		if strings.Contains(res.SanitizedText, forbidden) {
			t.Fatalf("sanitized text leaked %q: %s", forbidden, res.SanitizedText)
		}
	}
	if !strings.Contains(res.SanitizedText, "PRAGMA busy_timeout=5000") {
		t.Fatalf("generalized technical fact was lost: %s", res.SanitizedText)
	}
}

func TestScrub_DropsEphemeralChatter(t *testing.T) {
	for _, raw := range []string{"ran tests", "failed on line 42", "fixed typo", "ok"} {
		res := Scrub(raw)
		if res.Verdict != ScrubDroppedLowSignal {
			t.Fatalf("expected ScrubDroppedLowSignal for %q, got %+v", raw, res)
		}
	}
}
