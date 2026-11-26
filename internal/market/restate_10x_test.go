package market

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestRestateManifestIncludesStandingOrderAndEscrowGuardian(t *testing.T) {
	mem := newTestMemory(t)
	eng := NewEngine(mem, DefaultPolicy())

	h, err := eng.NewRestateServer().Handler()
	if err != nil {
		t.Fatalf("NewRestateServer().Handler: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/discover", nil)
	req.Header.Set("Accept", "application/vnd.restate.endpointmanifest.v4+json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/discover HTTP %d: %s", rec.Code, rec.Body.String())
	}

	var manifest struct {
		Services []struct {
			Name                     string `json:"name"`
			Ty                       string `json:"ty"`
			RetryPolicyMaxAttempts   int    `json:"retryPolicyMaxAttempts"`
			RetryPolicyOnMaxAttempts string `json:"retryPolicyOnMaxAttempts"`
			Handlers                 []struct {
				Name string `json:"name"`
			} `json:"handlers"`
		} `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("unmarshal v4 manifest: %v", err)
	}

	got := make(map[string]map[string]bool)
	onMax := make(map[string]string)
	for _, s := range manifest.Services {
		hmap := make(map[string]bool, len(s.Handlers))
		for _, hh := range s.Handlers {
			hmap[hh.Name] = true
		}
		got[s.Name] = hmap
		onMax[s.Name] = strings.ToUpper(s.RetryPolicyOnMaxAttempts)
	}

	for svc, wantHandlers := range expectedManifest {
		hmap, ok := got[svc]
		if !ok {
			t.Fatalf("missing expected Restate service %q in /discover manifest", svc)
		}
		for _, wh := range wantHandlers {
			if !hmap[wh] {
				t.Fatalf("service %q missing handler %q (got %v)", svc, wh, hmap)
			}
		}
		if onMax[svc] != "PAUSE" {
			t.Fatalf("expected service %q to have PauseOnMaxAttempts (PAUSE), got %q", svc, onMax[svc])
		}
	}
}

func TestStandingOrderIndexAndDomainMatching(t *testing.T) {
	mem := newTestMemory(t)
	eng := NewEngine(mem, DefaultPolicy())

	if !standingOrderDomainMatches([]string{"ops", "bsv"}, []string{"BSV"}) {
		t.Fatal("expected case-insensitive domain match")
	}
	if !standingOrderDomainMatches([]string{"*"}, []string{"finance"}) {
		t.Fatal("expected wildcard domain match")
	}
	if standingOrderDomainMatches([]string{"ops"}, []string{"legal"}) {
		t.Fatal("expected domain mismatch")
	}

	now := time.Now().Unix()
	eng.upsertStandingOrderIndex(StandingOrderState{
		OrderID:       "so-test-1",
		Status:        "watching",
		Domains:       []string{"bsv", "ops"},
		Query:         "latest SPV fee rate",
		MaxSats:       250,
		ExpiresAtUnix: now + 3600,
		UpdatedAt:     now,
	})
	eng.upsertStandingOrderIndex(StandingOrderState{
		OrderID:       "so-test-2",
		Status:        "paused",
		Domains:       []string{"legal"},
		MaxSats:       100,
		ExpiresAtUnix: now + 3600,
		UpdatedAt:     now,
	})

	entries := LoadStandingOrderIndex(mem.DB())
	if len(entries) != 2 {
		t.Fatalf("expected 2 standing order index entries, got %d", len(entries))
	}
	// Upsert updates existing entry in place.
	eng.upsertStandingOrderIndex(StandingOrderState{
		OrderID:       "so-test-1",
		Status:        "filled",
		Domains:       []string{"bsv", "ops"},
		MaxSats:       250,
		ExpiresAtUnix: now + 3600,
		UpdatedAt:     now + 10,
	})
	entries = LoadStandingOrderIndex(mem.DB())
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries after upsert, got %d", len(entries))
	}
	var found bool
	for _, e := range entries {
		if e.OrderID == "so-test-1" {
			found = true
			if e.Status != "filled" {
				t.Fatalf("expected so-test-1 status=filled, got %q", e.Status)
			}
		}
	}
	if !found {
		t.Fatal("so-test-1 not found in index")
	}
}

func TestBuildTopKCandidatesForMultiSellerFailover(t *testing.T) {
	o1 := bstack.OfferAd{OfferID: "off-1", Pub: "pub1", PriceSats: 100}
	o2 := bstack.OfferAd{OfferID: "off-2", Pub: "pub2", PriceSats: 120}
	o3 := bstack.OfferAd{OfferID: "off-3", Pub: "pub3", PriceSats: 150}
	o4 := bstack.OfferAd{OfferID: "off-4", Pub: "pub4", PriceSats: 200}

	ranked := []ScoredOffer{
		{Offer: o1, ScoreBP: 9000},
		{Offer: o2, ScoreBP: 8500},
		{Offer: o3, ScoreBP: 8000},
		{Offer: o4, ScoreBP: 7000},
	}

	// If Jev chose o2 over o1, o2 must come first followed by o1 and o3 (deduped, capped at 3).
	cands := buildTopKCandidates(o2, ranked, 3)
	if len(cands) != 3 {
		t.Fatalf("expected 3 failover candidates, got %d", len(cands))
	}
	if cands[0].OfferID != "off-2" || cands[1].OfferID != "off-1" || cands[2].OfferID != "off-3" {
		t.Fatalf("unexpected candidate failover order: %+v", []string{cands[0].OfferID, cands[1].OfferID, cands[2].OfferID})
	}
}

func TestRestateClientAdminInvocationControls(t *testing.T) {
	var calls []string
	adm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/invocations/inv_abc/resume":
			w.WriteHeader(http.StatusAccepted)
		case "/invocations/inv_abc/restart-as-new":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"new_invocation_id":"inv_new_123"}`))
		case "/invocations/inv_abc/kill":
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer adm.Close()

	rc := NewRestateClient(adm.URL, adm.URL)
	ctx := context.Background()

	if err := rc.ResumeInvocation(ctx, "inv_abc"); err != nil {
		t.Fatalf("ResumeInvocation: %v", err)
	}
	newID, err := rc.RestartInvocationAsNew(ctx, "inv_abc")
	if err != nil {
		t.Fatalf("RestartInvocationAsNew: %v", err)
	}
	if newID != "inv_new_123" {
		t.Fatalf("expected new_invocation_id=inv_new_123, got %q", newID)
	}
	if err := rc.KillInvocation(ctx, "inv_abc"); err != nil {
		t.Fatalf("KillInvocation: %v", err)
	}

	want := []string{
		"PATCH /invocations/inv_abc/resume",
		"PATCH /invocations/inv_abc/restart-as-new",
		"PATCH /invocations/inv_abc/kill",
	}
	if len(calls) != len(want) {
		t.Fatalf("expected %d calls, got %v", len(want), calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call[%d] = %q, want %q", i, calls[i], want[i])
		}
	}
}

func TestStandingOrderBrokerDispatchFiltersMatchingOrders(t *testing.T) {
	mem := newTestMemory(t)
	eng := NewEngine(mem, DefaultPolicy())

	var signaledPaths []string
	ingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signaledPaths = append(signaledPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`true`))
	}))
	defer ingSrv.Close()

	now := time.Now().Unix()
	eng.upsertStandingOrderIndex(StandingOrderState{
		OrderID:       "so-match",
		Status:        "watching",
		Domains:       []string{"bsv"},
		MaxSats:       500,
		ExpiresAtUnix: now + 3600,
		UpdatedAt:     now,
	})
	eng.upsertStandingOrderIndex(StandingOrderState{
		OrderID:       "so-too-expensive",
		Status:        "watching",
		Domains:       []string{"bsv"},
		MaxSats:       50,
		ExpiresAtUnix: now + 3600,
		UpdatedAt:     now,
	})
	eng.upsertStandingOrderIndex(StandingOrderState{
		OrderID:       "so-wrong-domain",
		Status:        "watching",
		Domains:       []string{"legal"},
		MaxSats:       500,
		ExpiresAtUnix: now + 3600,
		UpdatedAt:     now,
	})
	eng.upsertStandingOrderIndex(StandingOrderState{
		OrderID:       "so-paused",
		Status:        "paused",
		Domains:       []string{"bsv"},
		MaxSats:       500,
		ExpiresAtUnix: now + 3600,
		UpdatedAt:     now,
	})

	br := &Broker{
		mem:     mem,
		engine:  eng,
		restate: NewRestateClient(ingSrv.URL, ingSrv.URL),
	}
	br.dispatchOfferToStandingOrders(bstack.OfferAd{
		OfferID:   "off-live-1",
		Domains:   []string{"bsv"},
		PriceSats: 200,
	})

	// Wait briefly for async goroutine dispatch.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(signaledPaths) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if len(signaledPaths) != 1 || !strings.Contains(signaledPaths[0], "/StandingOrder/so-match/onOffer") {
		t.Fatalf("expected only so-match to be signaled, got %v", signaledPaths)
	}

	// Ensure deal persistence helper isn't broken by new DealState fields.
	_ = mem.DB().SaveDeal(store.DealRecord{DealID: "d1", Role: "buyer", Status: "settled"})
}
