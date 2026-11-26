package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/memory"
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

func startLiveRestateForTest(t *testing.T, ctx context.Context, eng *Engine, internalTok string) (rc *RestateClient, epURI string, cleanup func()) {
	t.Helper()
	bin := locateRestateServerBinary()
	useWSL := false
	if bin == "" && runtime.GOOS == "windows" {
		if _, err := exec.LookPath("wsl"); err == nil {
			out, wErr := exec.CommandContext(ctx, "wsl", "-e", "sh", "-c", `test -x "$HOME/.nektar/bin/restate-server" && printf "%s/.nektar/bin/restate-server" "$HOME"`).Output()
			if wErr == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "/") {
				bin = strings.TrimSpace(string(out))
				useWSL = true
			}
		}
	}
	if bin == "" {
		t.Skip("restate-server binary not available locally or in WSL2")
	}

	epPort := freeLoopbackPort(t)
	bindHost := "127.0.0.1"
	if useWSL {
		if out, err := exec.CommandContext(ctx, "wsl", "-e", "sh", "-c", `ip route show default 2>/dev/null | awk '{print $3; exit}'`).Output(); err == nil {
			if ip := net.ParseIP(strings.TrimSpace(string(out))); ip != nil && ip.To4() != nil && !ip.IsLoopback() {
				if addrs, aErr := net.InterfaceAddrs(); aErr == nil {
					for _, a := range addrs {
						var lip net.IP
						switch v := a.(type) {
						case *net.IPNet:
							lip = v.IP
						case *net.IPAddr:
							lip = v.IP
						}
						if lip != nil && lip.Equal(ip) {
							bindHost = ip.String()
							break
						}
					}
				}
			}
		}
	}
	epAddr := net.JoinHostPort(bindHost, fmt.Sprintf("%d", epPort))
	epURI = "http://" + epAddr

	epCtx, epCancel := context.WithCancel(ctx)
	go func() {
		_ = eng.NewRestateServer().Start(epCtx, epAddr)
	}()
	ready := false
	for i := 0; i < 60; i++ {
		if c, err := net.DialTimeout("tcp", epAddr, 100*time.Millisecond); err == nil {
			_ = c.Close()
			ready = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		epCancel()
		t.Fatalf("h2c endpoint %s never became ready", epAddr)
	}

	ingPort := freeLoopbackPort(t)
	admPort := freeLoopbackPort(t)
	nodePort := freeLoopbackPort(t)
	var cmd *exec.Cmd
	var wslDir string
	if useWSL {
		wslDir = fmt.Sprintf("/tmp/nektar-test-restate-%d", time.Now().UnixNano())
		shCmd := fmt.Sprintf(
			`mkdir -p %s && exec env RESTATE_NODE_NAME=nektar-10x-e2e RESTATE_BIND_ADDRESS=127.0.0.1:%d RESTATE_INGRESS__BIND_ADDRESS=127.0.0.1:%d RESTATE_ADMIN__BIND_ADDRESS=127.0.0.1:%d RESTATE_WORKER__INVOKER__INACTIVITY_TIMEOUT=2s RUST_LOG=warn %q --base-dir %s`,
			wslDir, nodePort, ingPort, admPort, bin, wslDir,
		)
		cmd = exec.CommandContext(ctx, "wsl", "-e", "sh", "-c", shCmd)
	} else {
		restateData := t.TempDir()
		cmd = exec.CommandContext(ctx, bin, "--base-dir", restateData)
		cmd.Env = append(os.Environ(),
			"RESTATE_NODE_NAME=nektar-10x-e2e",
			fmt.Sprintf("RESTATE_BIND_ADDRESS=127.0.0.1:%d", nodePort),
			fmt.Sprintf("RESTATE_INGRESS__BIND_ADDRESS=127.0.0.1:%d", ingPort),
			fmt.Sprintf("RESTATE_ADMIN__BIND_ADDRESS=127.0.0.1:%d", admPort),
			"RESTATE_WORKER__INVOKER__INACTIVITY_TIMEOUT=2s",
			"RUST_LOG=warn",
		)
	}
	var rsLogs strings.Builder
	cmd.Stdout = &rsLogs
	cmd.Stderr = &rsLogs
	if err := cmd.Start(); err != nil {
		epCancel()
		t.Fatalf("start restate-server: %v", err)
	}

	ingURL := fmt.Sprintf("http://127.0.0.1:%d", ingPort)
	admURL := fmt.Sprintf("http://127.0.0.1:%d", admPort)
	rc = NewRestateClientWithToken(ingURL, admURL, internalTok)

	healthy := false
	for i := 0; i < 100; i++ {
		if rc.HealthCheck(ctx) == nil {
			healthy = true
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !healthy {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		epCancel()
		t.Fatalf("restate-server never became healthy; logs:\n%s", rsLogs.String())
	}
	if _, err := rc.EnsureDeployment(ctx, epURI, false); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		epCancel()
		t.Fatalf("EnsureDeployment(%s): %v\nlogs:\n%s", epURI, err, rsLogs.String())
	}

	return rc, epURI, func() {
		if t.Failed() {
			t.Logf("restate-server logs:\n%s", rsLogs.String())
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		epCancel()
		if useWSL && wslDir != "" {
			_ = exec.Command("wsl", "-e", "rm", "-rf", wslDir).Run()
		}
	}
}

// TestLiveRestate10xFeaturesE2E runs ALL new Restate v1.8.1 features end-to-end against a REAL
// restate-server v1.8.1 process (NO MOCKS):
//  1. StandingOrder: create -> live-steer pause -> ignore offer while paused -> unpause -> auto-buy & fill via BuyerDeal!
//  2. BuyerDeal Top-K Multi-Seller Failover: Seller #1 fails pre-broadcast -> Custody released -> Seller #2 settles!
//  3. EscrowGuardian: watching -> markClaimed disarm + already-settled auto-claim detection!
//  4. Restate v1.8.1 Admin Invocation Controls: KillInvocation + Resume/Restart real HTTP API check!
func TestLiveRestate10xFeaturesE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	buyerMem := newTestMemory(t)
	sellerMem := newTestMemory(t)
	buyerEng := NewEngine(buyerMem, DefaultPolicy())
	sellerEng := NewEngine(sellerMem, DefaultPolicy())
	const internalTok = "e2e-10x-secret"
	buyerEng.SetInternalToken(internalTok)

	// Attach a minimal broker to buyerEng so BuyerDeal Step 5 executes buyerPayAndDeliver (for Failover test!).
	buyerBroker := NewBroker(buyerMem, buyerEng, nil, BrokerConfig{})
	buyerEng.SetBroker(buyerBroker)

	// Approve buyer policy for ops/bsv up to 1000 sats/deal.
	if _, err := ApprovePolicyPreset(buyerMem.DB(), buyerEng.Policy(), "buyer", []string{"ops", "bsv"}, "e2e-test"); err != nil {
		t.Fatal(err)
	}

	rc, _, cleanup := startLiveRestateForTest(t, ctx, buyerEng, internalTok)
	defer cleanup()

	buyerPub := buyerMem.Chain().IdentityPubHex()

	// === 1. LIVE STANDING ORDER E2E (restate.Signal steer + pause + auto-buy -> filled) ===
	remSO, err := sellerMem.Remember(ctx, memory.RememberInput{
		Shard:      "intel-so",
		Text:       "StandingOrder auto-bought knowledge verified on real Restate v1.8.1!",
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	soOffer, soKey, soChirp, err := sellerEng.PrepareDirectSale(ctx, "intel-so", remSO.Seq, 180, buyerPub)
	if err != nil {
		t.Fatal(err)
	}
	soOfferJSON, _ := json.Marshal(soOffer)
	orderID := "so-live-e2e-1"
	if _, err := rc.StartStandingOrderAsync(ctx, orderID, StandingOrderInput{
		OrderID:     orderID,
		Domains:     []string{"bsv"},
		Query:       "live standing order test",
		MaxSats:     150, // initially too low for 180-sat offer; will steer up to 250!
		TargetShard: "acquired-standing",
		TTL:         10 * time.Minute,
		WaitTimeout: 15 * time.Second,
	}); err != nil {
		t.Fatalf("StartStandingOrderAsync: %v", err)
	}

	// Wait for StandingOrder to reach watching (waitingSeq=1).
	var soSt *StandingOrderState
	for i := 0; i < 40; i++ {
		if st, gErr := rc.StandingOrderStatus(ctx, orderID); gErr == nil && st.Status == "watching" && st.WaitingSeq == 1 {
			soSt = st
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if soSt == nil {
		t.Fatalf("StandingOrder never reached watching state")
	}

	// Live-steer: pause + raise maxSats to 250.
	pausedTrue := true
	newMax := uint64(250)
	if ok, err := rc.SteerStandingOrder(ctx, orderID, SteerStandingOrderInput{MaxSats: &newMax, Paused: &pausedTrue}); err != nil || !ok {
		t.Fatalf("SteerStandingOrder pause: ok=%v err=%v", ok, err)
	}
	for i := 0; i < 40; i++ {
		if st, gErr := rc.StandingOrderStatus(ctx, orderID); gErr == nil && st.Status == "paused" && st.MaxSats == 250 && st.WaitingSeq == 2 {
			soSt = st
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if soSt.Status != "paused" || soSt.MaxSats != 250 {
		t.Fatalf("expected paused maxSats=250, got %+v", soSt)
	}

	// Send an offer while paused — MUST be ignored (waitingSeq bumps to 3, status stays paused, attempts=0).
	if ok, err := rc.SignalStandingOrderOffer(ctx, orderID, bstack.OfferAd{
		OfferID: "off-ignored-while-paused", Domains: []string{"bsv"}, PriceSats: 100,
	}); err != nil || !ok {
		t.Fatalf("SignalStandingOrderOffer while paused: ok=%v err=%v", ok, err)
	}
	for i := 0; i < 40; i++ {
		if st, gErr := rc.StandingOrderStatus(ctx, orderID); gErr == nil && st.WaitingSeq == 3 {
			soSt = st
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if soSt.Status != "paused" || soSt.Attempts != 0 {
		t.Fatalf("expected offer ignored while paused, got %+v", soSt)
	}

	// Unpause!
	pausedFalse := false
	if ok, err := rc.SteerStandingOrder(ctx, orderID, SteerStandingOrderInput{Paused: &pausedFalse}); err != nil || !ok {
		t.Fatalf("SteerStandingOrder unpause: ok=%v err=%v", ok, err)
	}
	for i := 0; i < 40; i++ {
		if st, gErr := rc.StandingOrderStatus(ctx, orderID); gErr == nil && st.Status == "watching" && st.WaitingSeq == 4 {
			soSt = st
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if soSt.Status != "watching" {
		t.Fatalf("expected unpaused watching, got %+v", soSt)
	}

	// Now deliver a REAL cryptographically signed OfferAd via Signal!
	liveOfferAd := bstack.OfferAd{
		OfferID:      "off-so-real-1",
		Pub:          soOffer.SellerPub,
		BuyerPub:     buyerPub,
		Net:          "memory",
		Domains:      []string{"bsv"},
		PriceSats:    180,
		Shard:        "intel-so",
		Leaf:         remSO.Leaf,
		ChirpURL:     soChirp,
		KeyCommitHex: soOffer.KeyCommitment,
		SaleOffer:    soOfferJSON,
		Direct:       true,
	}
	if ok, err := rc.SignalStandingOrderOffer(ctx, orderID, liveOfferAd); err != nil || !ok {
		t.Fatalf("SignalStandingOrderOffer live: ok=%v err=%v", ok, err)
	}

	// Wait for StandingOrder to launch BuyerDeal (dealID = "so-<orderID>-1"), then deliver key to BuyerDeal!
	expectedDealID := "so-" + orderID + "-1"
	for i := 0; i < 50; i++ {
		if dst, gErr := rc.BuyerDealStatus(ctx, expectedDealID); gErr == nil && dst.Status == "offered" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ok, err := rc.DeliverBuyerKey(ctx, expectedDealID, ClaimSignal{
		ClaimTxID:     "tx-so-claim-1",
		ContentKeyHex: hex.EncodeToString(soKey[:]),
		ChirpURL:      soChirp,
	}); err != nil || !ok {
		t.Fatalf("DeliverBuyerKey for StandingOrder deal %s: ok=%v err=%v", expectedDealID, ok, err)
	}

	// Verify StandingOrder transitions to "filled" with decrypted text!
	for i := 0; i < 60; i++ {
		if st, gErr := rc.StandingOrderStatus(ctx, orderID); gErr == nil && st.Status == "filled" {
			soSt = st
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if soSt.Status != "filled" || !strings.Contains(soSt.DeliveredText, "StandingOrder auto-bought") {
		t.Fatalf("expected StandingOrder filled with delivered text, got %+v", soSt)
	}

	// === 2. LIVE TOP-K MULTI-SELLER FAILOVER E2E (BuyerDeal) ===
	remF1, err := sellerMem.Remember(ctx, memory.RememberInput{Shard: "fail-1", Text: "Seller 1 unreachable", AutoAnchor: true})
	if err != nil {
		t.Fatal(err)
	}
	remF2, err := sellerMem.Remember(ctx, memory.RememberInput{Shard: "fail-2", Text: "Seller 2 failover succeeded cleanly!", AutoAnchor: true})
	if err != nil {
		t.Fatal(err)
	}
	fOffer1, _, fChirp1, err := sellerEng.PrepareDirectSale(ctx, "fail-1", remF1.Seq, 100, buyerPub) // cheaper -> ranked #1!
	if err != nil {
		t.Fatal(err)
	}
	fOffer2, fKey2, fChirp2, err := sellerEng.PrepareDirectSale(ctx, "fail-2", remF2.Seq, 150, buyerPub) // ranked #2!
	if err != nil {
		t.Fatal(err)
	}
	fJSON1, _ := json.Marshal(fOffer1)
	fJSON2, _ := json.Marshal(fOffer2)
	failoverWantID := "want-failover-e2e-1"
	if _, err := rc.StartBuyerDealAsync(ctx, failoverWantID, BuyerDealInput{
		Want: bstack.WantAd{WantID: failoverWantID, Pub: buyerPub, Net: "memory", Domains: []string{"ops"}, Query: "failover test", MaxSats: 500},
		Offers: []bstack.OfferAd{
			{
				OfferID:      "off-fail-1",
				WantID:       failoverWantID,
				Pub:          fOffer1.SellerPub,
				BuyerPub:     buyerPub,
				Net:          "memory",
				Domains:      []string{"ops"},
				PriceSats:    100,
				PayAddress:   "invalid-bsv-address-triggers-prebroadcast-err",
				Shard:        "fail-1",
				Leaf:         remF1.Leaf,
				ChirpURL:     fChirp1,
				KeyCommitHex: fOffer1.KeyCommitment,
				SaleOffer:    fJSON1,
				Direct:       true,
			},
			{
				OfferID:      "off-fail-2",
				WantID:       failoverWantID,
				Pub:          fOffer2.SellerPub,
				BuyerPub:     buyerPub,
				Net:          "memory",
				Domains:      []string{"ops"},
				PriceSats:    150,
				Shard:        "fail-2",
				Leaf:         remF2.Leaf,
				ChirpURL:     fChirp2,
				KeyCommitHex: fOffer2.KeyCommitment,
				SaleOffer:    fJSON2,
				Direct:       true,
			},
		},
		TargetShard: "acquired-failover",
		WaitTimeout: 15 * time.Second,
	}); err != nil {
		t.Fatalf("StartBuyerDealAsync failover: %v", err)
	}

	// Wait until BuyerDeal fails over from off-fail-1 to off-fail-2!
	var foSt *DealState
	for i := 0; i < 60; i++ {
		if st, gErr := rc.BuyerDealStatus(ctx, failoverWantID); gErr == nil && st.ChosenOffer != nil && st.ChosenOffer.OfferID == "off-fail-2" {
			foSt = st
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if foSt == nil || foSt.ChosenOffer == nil || foSt.ChosenOffer.OfferID != "off-fail-2" {
		st, _ := rc.BuyerDealStatus(ctx, failoverWantID)
		t.Fatalf("expected BuyerDeal to fail over to off-fail-2 automatically, got %+v", st)
	}
	if ok, err := rc.DeliverBuyerKey(ctx, failoverWantID, ClaimSignal{
		ClaimTxID:     "tx-failover-claim-2",
		ContentKeyHex: hex.EncodeToString(fKey2[:]),
		ChirpURL:      fChirp2,
	}); err != nil || !ok {
		t.Fatalf("DeliverBuyerKey failover: ok=%v err=%v", ok, err)
	}
	settledFO, err := rc.AttachBuyerDeal(ctx, failoverWantID)
	if err != nil {
		t.Fatalf("AttachBuyerDeal failover: %v", err)
	}
	if settledFO.Status != "settled" || !strings.Contains(settledFO.DeliveredTxt, "Seller 2 failover succeeded") {
		t.Fatalf("expected failover deal settled with Seller 2 text, got %+v", settledFO)
	}

	// === 3. LIVE ESCROW GUARDIAN E2E (arm -> watching -> markClaimed -> claimed) ===
	escID := "escrow-live-tx-1"
	if _, err := rc.StartEscrowGuardianAsync(ctx, escID, EscrowGuardianInput{
		DealID:       "deal-escrow-open",
		EscrowTxID:   escID,
		RefundAfter:  uint64(time.Now().Add(1 * time.Hour).Unix()),
		CheckEveryMs: 500,
	}); err != nil {
		t.Fatalf("StartEscrowGuardianAsync: %v", err)
	}
	for i := 0; i < 40; i++ {
		if st, gErr := rc.EscrowGuardianStatus(ctx, escID); gErr == nil && st.Status == "watching" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ok, err := rc.MarkEscrowGuardianClaimed(ctx, escID); err != nil || !ok {
		t.Fatalf("MarkEscrowGuardianClaimed: ok=%v err=%v", ok, err)
	}
	var egSt *EscrowGuardianState
	for i := 0; i < 40; i++ {
		if st, gErr := rc.EscrowGuardianStatus(ctx, escID); gErr == nil && st.Status == "claimed" {
			egSt = st
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if egSt == nil || egSt.Status != "claimed" {
		st, _ := rc.EscrowGuardianStatus(ctx, escID)
		t.Fatalf("expected EscrowGuardian status=claimed, got %+v", st)
	}

	// === 4. LIVE RESTATE v1.8.1 ADMIN INVOCATION API VERIFICATION (KillInvocation on real sleeping workflow) ===
	killOrderID := "so-live-kill-test"
	killInvID, err := rc.StartStandingOrderAsync(ctx, killOrderID, StandingOrderInput{
		OrderID: killOrderID,
		Domains: []string{"ops"},
		MaxSats: 100,
		TTL:     1 * time.Hour,
	})
	if err != nil {
		t.Fatalf("StartStandingOrderAsync for kill test: %v", err)
	}
	for i := 0; i < 30; i++ {
		if st, gErr := rc.StandingOrderStatus(ctx, killOrderID); gErr == nil && st.Status == "watching" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := rc.KillInvocation(ctx, killInvID); err != nil {
		t.Fatalf("LIVE Restate v1.8.1 KillInvocation(%s) failed: %v", killInvID, err)
	}
	// Also verify ResumeInvocation and RestartInvocationAsNew hit valid Restate v1.8.1 Admin routes (NOT 404/405!).
	if err := rc.ResumeInvocation(ctx, killInvID); err != nil {
		if strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), "HTTP 405") {
			t.Fatalf("LIVE Restate v1.8.1 ResumeInvocation route/method mismatch: %v", err)
		}
		t.Logf("ResumeInvocation on killed invocation returned expected domain error (valid route!): %v", err)
	}
	if newID, err := rc.RestartInvocationAsNew(ctx, killInvID); err != nil {
		if strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), "HTTP 405") {
			t.Fatalf("LIVE Restate v1.8.1 RestartInvocationAsNew route/method mismatch: %v", err)
		}
		t.Logf("RestartInvocationAsNew returned: %v", err)
	} else {
		t.Logf("LIVE Restate v1.8.1 RestartInvocationAsNew succeeded! newID=%s", newID)
	}
}

// TestLiveAuditFixesE2E verifies all critical audit fixes LIVE against real restate-server v1.8.1:
//  1. EscrowGuardian per-escrow isolation + REAL Chronicle-VM DisclosureSale.Refund() broadcast
//     (Escrow B refunds while Guardian A stays watching, then Guardian A refunds Escrow A!).
//  2. StandingOrder lossless concurrent inbox (2 offers fired simultaneously -> zero dropped offers!).
//  3. BuyerDeal Custody cap failover (Seller #1 denied by Custody/global/reserve daily cap ->
//     BuyerDeal automatically fails over to cheaper Seller #2 on Attempt 2 and settles!).
func TestLiveAuditFixesE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	buyerMem := newTestMemory(t)
	sellerMem := newTestMemory(t)
	buyerEng := NewEngine(buyerMem, DefaultPolicy())
	sellerEng := NewEngine(sellerMem, DefaultPolicy())
	const internalTok = "e2e-audit-fixes-tok"
	buyerEng.SetInternalToken(internalTok)

	buyerBroker := NewBroker(buyerMem, buyerEng, nil, BrokerConfig{})
	buyerEng.SetBroker(buyerBroker)

	// Approve buyer policy with maxPerDeal=500, maxPerDay=500.
	if _, err := ApprovePolicyCustom(buyerMem.DB(), buyerEng.Policy(), "buyer", []string{"ops", "bsv"}, &PolicyLimitsOverride{
		MaxPerDeal: 500,
		MaxPerDay:  500,
	}, "audit-e2e", nil); err != nil {
		t.Fatal(err)
	}

	rc, _, cleanup := startLiveRestateForTest(t, ctx, buyerEng, internalTok)
	defer cleanup()

	buyerPub := buyerMem.Chain().IdentityPubHex()

	// === 1. ESCROW GUARDIAN ISOLATION + REAL DisclosureSale.Refund() BROADCAST ===
	lockRealEscrow := func(dealID string, refundAfter uint64) (string, EscrowObligationPayload) {
		t.Helper()
		depPKH, err := buyerMem.Chain().FundPKH(ctx)
		if err != nil {
			t.Fatalf("FundPKH: %v", err)
		}
		sellerKey, _ := ec.NewPrivateKey()
		sellerPKH := core.Hash160(sellerKey.PubKey().Compressed())
		hWrapped := core.DSHA256([]byte("secret-key-" + dealID))
		params := contracts.DisclosureSaleParams{
			SellerPKH:    sellerPKH,
			Price:        1200,
			HWrapped:     hWrapped,
			RefundAfter:  refundAfter,
			MaxFee:       500,
			DepositorPKH: depPKH,
		}
		lockScript, err := contracts.EncodeDisclosureSaleLockingScript(params)
		if err != nil {
			t.Fatalf("EncodeDisclosureSaleLockingScript: %v", err)
		}
		escSats := params.RequiredEscrowSatoshis() + 100
		tx, beef, fee, spend, err := buyerMem.Chain().BuildFundedTxWithBasket(ctx, []*transaction.TransactionOutput{{
			Satoshis:      escSats,
			LockingScript: lockScript,
		}}, "test escrow "+dealID, []string{"escrow"}, nil)
		if err != nil {
			t.Fatalf("BuildFundedTx: %v", err)
		}
		if _, err := buyerMem.Chain().Broadcast(ctx, tx, fee, spend); err != nil {
			t.Fatalf("Broadcast escrow: %v", err)
		}
		if len(beef) == 0 {
			beef, _ = chain.SafeAtomicBEEF(tx)
		}
		txid := tx.TxID().String()
		payload := EscrowObligationPayload{
			OfferID:      "off-" + dealID,
			EscrowTxID:   txid,
			EscrowVout:   0,
			EscrowBEEF:   hex.EncodeToString(beef),
			SellerPKH:    hex.EncodeToString(sellerPKH),
			DepositorPKH: hex.EncodeToString(depPKH),
			Price:        1200,
			HWrapped:     hex.EncodeToString(hWrapped),
			RefundAfter:  refundAfter,
			MaxFee:       500,
		}
		_ = buyerMem.DB().SaveDeal(store.DealRecord{DealID: dealID, Role: "buyer", Status: "paid_awaiting_reveal", PaymentTxID: txid})
		if err := RecordRevealOwedWithEscrow(buyerMem.DB(), dealID, hex.EncodeToString(sellerKey.PubKey().Compressed()), txid, 1200, int64(refundAfter), &payload); err != nil {
			t.Fatalf("RecordRevealOwedWithEscrow: %v", err)
		}
		return txid, payload
	}

	now := time.Now().Unix()
	escrowATxID, _ := lockRealEscrow("deal-esc-A", uint64(now+3))  // refunds in 3s
	escrowBTxID, _ := lockRealEscrow("deal-esc-B", uint64(now-60)) // already refundable NOW!

	// Arm Guardian A BEFORE Escrow B is reconciled!
	if _, err := rc.StartEscrowGuardianAsync(ctx, escrowATxID, EscrowGuardianInput{
		DealID:       "deal-esc-A",
		EscrowTxID:   escrowATxID,
		RefundAfter:  uint64(now + 3),
		CheckEveryMs: 300,
	}); err != nil {
		t.Fatalf("StartEscrowGuardianAsync A: %v", err)
	}
	// Verify Guardian A is watching and did NOT falsely steal Escrow B's refund!
	time.Sleep(800 * time.Millisecond)
	stA1, err := rc.EscrowGuardianStatus(ctx, escrowATxID)
	if err != nil {
		t.Fatalf("EscrowGuardianStatus A (early): %v", err)
	}
	if stA1.Status != "watching" {
		t.Fatalf("CROSS-ESCROW CONTAMINATION: Guardian A exited early as %q (refundTx=%s) while Escrow B was refundable!", stA1.Status, stA1.RefundTxID)
	}
	// Now reconcile Escrow B separately and confirm Escrow B refunds cleanly!
	repB, err := ReconcileSingleEscrowObligation(ctx, buyerMem.DB(), buyerMem.Chain(), "deal-esc-B", escrowBTxID, time.Now().Unix())
	if err != nil || repB.RefundsBroadcast != 1 || len(repB.RefundTxIDs) != 1 {
		t.Fatalf("expected Escrow B to refund cleanly via ReconcileSingleEscrowObligation, got rep=%+v err=%v", repB, err)
	}
	refundBTxID := repB.RefundTxIDs[0]

	// Wait for Escrow A's RefundAfter (+3s) to elapse and prove Guardian A broadcasts Escrow A's OWN real Refund tx!
	var stAFinal *EscrowGuardianState
	for i := 0; i < 40; i++ {
		if st, gErr := rc.EscrowGuardianStatus(ctx, escrowATxID); gErr == nil && st.Status == "refunded" {
			stAFinal = st
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if stAFinal == nil || stAFinal.Status != "refunded" || stAFinal.RefundTxID == "" {
		st, _ := rc.EscrowGuardianStatus(ctx, escrowATxID)
		t.Fatalf("expected Guardian A to broadcast real DisclosureSale.Refund(), got %+v", st)
	}
	if stAFinal.RefundTxID == refundBTxID {
		t.Fatalf("Guardian A reported Escrow B's refundTxID %s instead of its own!", refundBTxID)
	}

	// === 2. STANDING ORDER LOSSLESS CONCURRENT SIGNAL BURST ===
	remBurst, err := sellerMem.Remember(ctx, memory.RememberInput{
		Shard:      "intel-burst",
		Text:       "Concurrent burst offer #2 survived lossless inbox!",
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	bOffer, bKey, bChirp, err := sellerEng.PrepareDirectSale(ctx, "intel-burst", remBurst.Seq, 120, buyerPub)
	if err != nil {
		t.Fatal(err)
	}
	bOfferJSON, _ := json.Marshal(bOffer)
	burstOrderID := "so-burst-e2e-1"
	if _, err := rc.StartStandingOrderAsync(ctx, burstOrderID, StandingOrderInput{
		OrderID:     burstOrderID,
		Domains:     []string{"bsv"},
		Query:       "burst test",
		MaxSats:     200,
		TargetShard: "acquired-burst",
		TTL:         10 * time.Minute,
		WaitTimeout: 15 * time.Second,
	}); err != nil {
		t.Fatalf("StartStandingOrderAsync burst: %v", err)
	}
	for i := 0; i < 40; i++ {
		if st, gErr := rc.StandingOrderStatus(ctx, burstOrderID); gErr == nil && st.Status == "watching" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Fire 2 offers CONCURRENTLY in parallel goroutines (Offer #1 too expensive, Offer #2 valid)!
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = rc.SignalStandingOrderOffer(ctx, burstOrderID, bstack.OfferAd{
			OfferID: "off-burst-expensive", Domains: []string{"bsv"}, PriceSats: 999,
		})
	}()
	go func() {
		defer wg.Done()
		_, _ = rc.SignalStandingOrderOffer(ctx, burstOrderID, bstack.OfferAd{
			OfferID:      "off-burst-valid",
			Pub:          bOffer.SellerPub,
			BuyerPub:     buyerPub,
			Net:          "memory",
			Domains:      []string{"bsv"},
			PriceSats:    120,
			Shard:        "intel-burst",
			Leaf:         remBurst.Leaf,
			ChirpURL:     bChirp,
			KeyCommitHex: bOffer.KeyCommitment,
			SaleOffer:    bOfferJSON,
			Direct:       true,
		})
	}()
	wg.Wait()

	burstDealID := "so-" + burstOrderID + "-1"
	for i := 0; i < 50; i++ {
		if dst, gErr := rc.BuyerDealStatus(ctx, burstDealID); gErr == nil && dst.Status == "offered" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ok, err := rc.DeliverBuyerKey(ctx, burstDealID, ClaimSignal{
		ClaimTxID:     "tx-burst-claim",
		ContentKeyHex: hex.EncodeToString(bKey[:]),
		ChirpURL:      bChirp,
	}); err != nil || !ok {
		t.Fatalf("DeliverBuyerKey burst: ok=%v err=%v", ok, err)
	}
	var burstFinal *StandingOrderState
	for i := 0; i < 50; i++ {
		if st, gErr := rc.StandingOrderStatus(ctx, burstOrderID); gErr == nil && st.Status == "filled" {
			burstFinal = st
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if burstFinal == nil || burstFinal.Status != "filled" || burstFinal.Fills != 1 || !strings.Contains(burstFinal.DeliveredText, "survived lossless inbox") {
		st, _ := rc.StandingOrderStatus(ctx, burstOrderID)
		t.Fatalf("expected burst StandingOrder filled (zero dropped offers), got %+v", st)
	}

	// === 3. BUYER DEAL CUSTODY DAILY-CAP FAILOVER (Seller #1 denied by Custody -> Seller #2 succeeds!) ===
	// Simulate a real concurrent deal consuming 230 sats of the 500-sat daily cap right after Step 1
	// (inside Step 3b Matcher), leaving 150 sats remaining in Custody (500 - 120 burst - 230 concurrent):
	// Seller #1 (250 sats, ranked #1) is DENIED by Custody/global/reserve Attempt 1 -> BuyerDeal MUST
	// automatically fail over to cheaper Seller #2 (100 sats <= 150 rem) on Attempt 2 and settle!
	var capRaceOnce sync.Once
	buyerEng.SetMatcher(&capRaceTestMatcher{
		onMatch: func() {
			capRaceOnce.Do(func() {
				_, _ = buyerMem.DB().ReserveSpendAtomic("concurrent-deal-spent", 1, 230, 500, 500)
			})
		},
	})
	defer buyerEng.SetMatcher(nil)
	remCap1, _ := sellerMem.Remember(ctx, memory.RememberInput{Shard: "cap-1", Text: "Too expensive for remaining Custody cap", AutoAnchor: true})
	cOffer1, _, cChirp1, err := sellerEng.PrepareDirectSale(ctx, "cap-1", remCap1.Seq, 250, buyerPub)
	if err != nil {
		t.Fatal(err)
	}
	sellerMem2 := newTestMemory(t)
	sellerEng2 := NewEngine(sellerMem2, DefaultPolicy())
	remCap2b, _ := sellerMem2.Remember(ctx, memory.RememberInput{Shard: "cap-2", Text: "Cheaper Seller #2 fits under Custody daily cap!", AutoAnchor: true})
	cOffer2, cKey2, cChirp2, err := sellerEng2.PrepareDirectSale(ctx, "cap-2", remCap2b.Seq, 100, buyerPub)
	if err != nil {
		t.Fatal(err)
	}
	// Boost Seller #1 reputation so deterministic ranking puts Seller #1 (250 sats) at index 0 above Seller #2 (100 sats)!
	for i := 0; i < 10; i++ {
		_ = buyerMem.DB().RecordReputation(cOffer1.SellerPub, true, 50)
	}
	cJSON1, _ := json.Marshal(cOffer1)
	cJSON2, _ := json.Marshal(cOffer2)
	capWantID := "want-custody-cap-failover-1"
	if _, err := rc.StartBuyerDealAsync(ctx, capWantID, BuyerDealInput{
		Want: bstack.WantAd{WantID: capWantID, Pub: buyerPub, Net: "memory", Domains: []string{"ops"}, Query: "cap failover", MaxSats: 250},
		Offers: []bstack.OfferAd{
			{
				OfferID:      "off-cap-1-over",
				WantID:       capWantID,
				Pub:          cOffer1.SellerPub,
				BuyerPub:     buyerPub,
				Net:          "memory",
				Domains:      []string{"ops"},
				PriceSats:    250,
				Shard:        "cap-1",
				Leaf:         remCap1.Leaf,
				ChirpURL:     cChirp1,
				KeyCommitHex: cOffer1.KeyCommitment,
				SaleOffer:    cJSON1,
				Direct:       true,
			},
			{
				OfferID:      "off-cap-2-fits",
				WantID:       capWantID,
				Pub:          cOffer2.SellerPub,
				BuyerPub:     buyerPub,
				Net:          "memory",
				Domains:      []string{"ops"},
				PriceSats:    100,
				Shard:        "cap-2",
				Leaf:         remCap2b.Leaf,
				ChirpURL:     cChirp2,
				KeyCommitHex: cOffer2.KeyCommitment,
				SaleOffer:    cJSON2,
				Direct:       true,
			},
		},
		TargetShard: "acquired-cap-failover",
		WaitTimeout: 15 * time.Second,
	}); err != nil {
		t.Fatalf("StartBuyerDealAsync cap failover: %v", err)
	}
	var capSt *DealState
	for i := 0; i < 50; i++ {
		if dst, gErr := rc.BuyerDealStatus(ctx, capWantID); gErr == nil && dst.ChosenOffer != nil && dst.ChosenOffer.OfferID == "off-cap-2-fits" {
			capSt = dst
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if capSt == nil || capSt.ChosenOffer == nil || capSt.ChosenOffer.OfferID != "off-cap-2-fits" {
		st, _ := rc.BuyerDealStatus(ctx, capWantID)
		t.Fatalf("expected BuyerDeal to fail over from Custody-denied Seller #1 to Seller #2, got %+v", st)
	}
	if ok, err := rc.DeliverBuyerKey(ctx, capWantID, ClaimSignal{
		ClaimTxID:     "tx-cap-failover-claim",
		ContentKeyHex: hex.EncodeToString(cKey2[:]),
		ChirpURL:      cChirp2,
	}); err != nil || !ok {
		t.Fatalf("DeliverBuyerKey cap failover: ok=%v err=%v", ok, err)
	}
	capRes, err := rc.AttachBuyerDeal(ctx, capWantID)
	if err != nil || capRes.Status != "settled" || !strings.Contains(capRes.DeliveredTxt, "Cheaper Seller #2") {
		t.Fatalf("expected cap failover deal settled with Seller #2, got res=%+v err=%v", capRes, err)
	}
}

// TestStandingOrderKindAnnounceTriggersWantAd verifies that a verified KindAnnounce from a newly
// arrived seller with matching SellDomains triggers a rate-limited WantAd broadcast for watching StandingOrders.
func TestStandingOrderKindAnnounceTriggersWantAd(t *testing.T) {
	mem := newTestMemory(t)
	eng := NewEngine(mem, DefaultPolicy())

	var mu sync.Mutex
	var submits int
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/submit") {
			mu.Lock()
			submits++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer hubSrv.Close()

	disc := discovery.New(mem, []string{hubSrv.URL}, hubSrv.Client())
	br := NewBroker(mem, eng, disc, BrokerConfig{HTTPClient: hubSrv.Client()})

	now := time.Now().Unix()
	eng.upsertStandingOrderIndex(StandingOrderState{
		OrderID:       "so-announce-1",
		Status:        "watching",
		Domains:       []string{"bsv"},
		Query:         "need live bsv fees",
		MaxSats:       250,
		ExpiresAtUnix: now + 3600,
		UpdatedAt:     now,
	})

	sellerKey, _ := ec.NewPrivateKey()
	ann := bstack.AnnounceAd{
		Pub:         hex.EncodeToString(sellerKey.PubKey().Compressed()),
		Net:         "memory",
		SellDomains: []string{"bsv", "ops"},
		Hosts:       []string{hubSrv.URL},
		Exp:         time.Now().Add(30 * time.Minute).UnixMilli(),
	}
	// Trigger twice rapidly — first must submit a WantAd, second must be rate-limited (5m)!
	br.dispatchAnnounceToStandingOrders(ann)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := submits
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	br.dispatchAnnounceToStandingOrders(ann)
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	got := submits
	mu.Unlock()
	if got != 1 {
		t.Fatalf("expected exactly 1 WantAd broadcast (rate-limited on duplicate announce), got %d", got)
	}
}

type capRaceTestMatcher struct {
	onMatch func()
}

func (m *capRaceTestMatcher) DefaultMode() string { return "gated" }
func (m *capRaceTestMatcher) MatchOffersForBuyer(_ context.Context, _ bstack.WantAd, ranked []ScoredOffer, _ map[string]store.SellerReputation, _ *Policy) BuyerMatchDecision {
	if m.onMatch != nil {
		m.onMatch()
	}
	if len(ranked) == 0 {
		return BuyerMatchDecision{Verdict: "reject"}
	}
	chosen := ranked[0].Offer
	return BuyerMatchDecision{Verdict: "allow", Ranked: ranked, Chosen: &chosen}
}
func (m *capRaceTestMatcher) MatchWantForSeller(_ context.Context, _ bstack.WantAd, candidates []SellerCandidate, _ *Policy) SellerMatchDecision {
	if len(candidates) == 0 {
		return SellerMatchDecision{}
	}
	cp := candidates[0]
	return SellerMatchDecision{Approved: true, Chosen: &cp}
}
func (m *capRaceTestMatcher) MetricsSnapshot() MatcherMetrics { return MatcherMetrics{} }


