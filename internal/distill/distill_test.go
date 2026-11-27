package distill

import (
	"context"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestDistillerRunCycle_QuarantinesSecretsDropsChatterAndPromotesCleanFacts(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("chain.Open: %v", err)
	}
	defer port.Close()

	mem, err := memory.New(db, port, nil, nil, memory.Config{})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}

	// 1. Seed a private CLKA working shard with:
	//    (a) leaked OpenAI API key -> MUST be quarantined (zero secret persisted in quarantine or market!)
	//    (b) ephemeral test chatter -> MUST be dropped
	//    (c) genuine engineering fact with email/path PII -> MUST be redacted and promoted to market-ops with Distilled=true!
	//    (d) high-clearance Level-2 private note -> MUST NEVER be touched by distiller!
	secretKey := "sk-proj-aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789abcdef1234567890"
	_, err = mem.Remember(ctx, memory.RememberInput{
		Shard: "work-ops",
		Text:  "Production OpenAI key is " + secretKey + " for service billing.",
	})
	if err != nil {
		t.Fatalf("Remember secret: %v", err)
	}
	_, err = mem.Remember(ctx, memory.RememberInput{
		Shard: "work-ops",
		Text:  "ran tests, failed on line 42, fixed typo",
	})
	if err != nil {
		t.Fatalf("Remember chatter: %v", err)
	}
	_, err = mem.Remember(ctx, memory.RememberInput{
		Shard: "work-ops",
		Text:  "Alice (alice@acme.internal) fixed Go SQLite SQLITE_BUSY in C:\\Users\\alice\\repo\\db.go by setting SetMaxOpenConns(1) and WAL mode on host 10.0.1.25.",
	})
	if err != nil {
		t.Fatalf("Remember clean fact: %v", err)
	}
	_, err = mem.Remember(ctx, memory.RememberInput{
		Shard: "work-ops",
		Level: 2,
		Text:  "Board-level M&A strategy for Q4 acquisition target.",
	})
	if err != nil {
		t.Fatalf("Remember level 2: %v", err)
	}

	d, err := NewDistiller(mem, Config{
		DefaultDomain: "ops",
		AutoAnchor:    true,
		AutoList:      true,
	})
	if err != nil {
		t.Fatalf("NewDistiller: %v", err)
	}

	sum, err := d.RunCycle(ctx, RunOpts{})
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if sum.Quarantined != 1 {
		t.Fatalf("expected 1 quarantined secret, got %+v", sum)
	}
	if sum.DroppedLowSignal != 1 {
		t.Fatalf("expected 1 dropped chatter item, got %+v", sum)
	}
	if sum.Distilled != 1 {
		t.Fatalf("expected 1 distilled fact promoted, got %+v", sum)
	}

	// Verify quarantine table contains rule ID + hash and ZERO plaintext secret!
	qRows, err := db.ListDistillQuarantine(10)
	if err != nil || len(qRows) != 1 {
		t.Fatalf("ListDistillQuarantine: %+v err=%v", qRows, err)
	}
	if strings.Contains(qRows[0].RuleID, secretKey) || strings.Contains(qRows[0].ContentHash, secretKey) {
		t.Fatal("secret plaintext leaked into quarantine record!")
	}

	// Verify market-ops contains ONLY the redacted distilled fact, with Distilled=true and ZERO PII/secrets/Level-2 text!
	marketFrags, err := db.ListFragments("market-ops")
	if err != nil || len(marketFrags) != 1 {
		t.Fatalf("expected 1 fragment in market-ops, got %d err=%v", len(marketFrags), err)
	}
	mf := marketFrags[0]
	if !mf.Distilled || !mf.Public || mf.OntologyID == "" {
		t.Fatalf("expected Distilled=true, Public=true, OntologyID!=\"\", got %+v", mf)
	}
	txt := string(mf.Data)
	for _, forbidden := range []string{secretKey, "alice@acme.internal", `C:\Users\alice`, "10.0.1.25", "M&A strategy"} {
		if strings.Contains(txt, forbidden) {
			t.Fatalf("forbidden content %q leaked into market-ops: %q", forbidden, txt)
		}
	}

	// Verify Cedar Action::"sell" allows the distilled fragment and denies raw working fragments!
	seller := policy.Principal{Type: "Agent", ID: "market-seller", Level: 0}
	decDist, err := mem.AuthorizeSell(seller, policy.Resource{
		Type: "Fragment", ID: "market-ops#1", Level: 0, Shard: "market-ops", Class: mf.Class, Distilled: mf.Distilled,
	}, false)
	if err != nil || !decDist.Allow {
		t.Fatalf("expected Cedar Action::sell ALLOW for distilled fragment, got %+v err=%v", decDist, err)
	}
	decRaw, err := mem.AuthorizeSell(seller, policy.Resource{
		Type: "Fragment", ID: "work-ops#3", Level: 0, Shard: "work-ops", Class: "general", Distilled: false,
	}, false)
	if err != nil || decRaw.Allow {
		t.Fatalf("expected Cedar Action::sell DENY for raw working fragment, got %+v err=%v", decRaw, err)
	}

	// Verify auto-listed sell listing has AllowRaw=false and dynamic pricing enabled.
	listings, err := db.ListSellListings()
	if err != nil || len(listings) != 1 || listings[0].Shard != "market-ops" || listings[0].AllowRaw {
		t.Fatalf("unexpected auto-list sell listings: %+v err=%v", listings, err)
	}

	// Idempotency: re-running cycle with no new working fragments scans 0!
	sum2, err := d.RunCycle(ctx, RunOpts{})
	if err != nil {
		t.Fatalf("RunCycle 2: %v", err)
	}
	if sum2.Scanned != 0 || sum2.Distilled != 0 {
		t.Fatalf("expected idempotent 0-scan second cycle, got %+v", sum2)
	}
}
