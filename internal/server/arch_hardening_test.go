package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

// TestMetricsAlertsParity enforces S2: every nektar_* metric referenced in ops/alerts.yml
// must be emitted verbatim by GET /v1/metrics so alerts can never silently evaluate to empty.
func TestMetricsAlertsParity(t *testing.T) {
	alertsBytes, err := os.ReadFile(filepath.Join("..", "..", "ops", "alerts.yml"))
	if err != nil {
		t.Fatalf("read ops/alerts.yml: %v", err)
	}
	re := regexp.MustCompile(`\bnektar_[a-z0-9_]+\b`)
	matches := re.FindAllString(string(alertsBytes), -1)
	if len(matches) == 0 {
		t.Fatal("ops/alerts.yml referenced zero nektar_* metrics")
	}

	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	app, err := NewApp(mem, mkt, Config{})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/metrics", nil)
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/metrics status=%d", rec.Code)
	}
	body := rec.Body.String()

	seen := make(map[string]bool)
	for _, metric := range matches {
		if seen[metric] {
			continue
		}
		seen[metric] = true
		if !strings.Contains(body, "# TYPE "+metric+" ") {
			t.Errorf("ops/alerts.yml references metric %q which is NOT emitted by GET /v1/metrics", metric)
		}
	}
}

// TestToolAuthBoundaries verifies non-operator callers cannot invoke borgGroup or export Level>0/Borg fragments.
func TestToolAuthBoundaries(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Remember(ctx, memory.RememberInput{Shard: "mixed", Text: "public level 0 fact", Level: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Remember(ctx, memory.RememberInput{Shard: "mixed", Text: "classified level 5 secret", Level: 5}); err != nil {
		t.Fatal(err)
	}
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	app, err := NewApp(mem, mkt, Config{
		APIToken:      "agent-tok-1234567890",
		OperatorToken: "op-tok-1234567890",
		ToolProfile:   "operator",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Non-operator caller MUST be denied borgGroup.
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/borgGroup", strings.NewReader(`{"op":"create","groupId":"g1","name":"test"}`))
	req.Header.Set("Authorization", "Bearer agent-tok-1234567890")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("non-operator was allowed to call borgGroup: %s", rec.Body.String())
	}

	// 2. Non-operator exportPack MUST filter out Level-5 fragments (only Level 0 exported).
	req2 := httptest.NewRequest(http.MethodPost, "/v1/tools/exportPack", strings.NewReader(`{"shard":"mixed"}`))
	req2.Header.Set("Authorization", "Bearer agent-tok-1234567890")
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("exportPack status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var out struct {
		ChirpURL string `json:"chirpUrl"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &out); err != nil || out.ChirpURL == "" {
		t.Fatalf("decode exportPack: err=%v body=%s", err, rec2.Body.String())
	}
	raw, err := bstack.ResolveChirp(mem.Objects(), out.ChirpURL)
	if err != nil {
		t.Fatalf("ResolveChirp: %v", err)
	}
	var pack memory.ShardPack
	if err := json.Unmarshal(raw, &pack); err != nil {
		t.Fatalf("unmarshal ShardPack: %v", err)
	}
	foundPublic := false
	for _, f := range pack.Fragments {
		txt := string(f.Data)
		if strings.Contains(txt, "classified level 5 secret") {
			t.Fatal("non-operator exportPack leaked Level-5 classified fragment!")
		}
		if strings.Contains(txt, "public level 0 fact") {
			foundPublic = true
		}
	}
	if !foundPublic {
		t.Fatalf("non-operator exportPack did not include Level-0 public fragment (got %d fragments)", len(pack.Fragments))
	}
}
