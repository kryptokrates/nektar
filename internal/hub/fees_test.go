package hub_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
)

func openMemoryPort(t *testing.T, shared *chain.SharedMemoryChain) *chain.Port {
	t.Helper()
	p, err := chain.Open(context.Background(), chain.Config{
		Network:           "memory",
		DataDir:           t.TempDir(),
		SharedMemoryChain: shared,
	})
	if err != nil {
		t.Fatalf("chain.Open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func makeWantPayload(t *testing.T, key *ec.PrivateKey, wantID, domain string) bstack.MarketPayload {
	t.Helper()
	w := bstack.WantAd{
		WantID:   wantID,
		Pub:      hex.EncodeToString(key.PubKey().Compressed()),
		Net:      "memory",
		Domains:  []string{domain},
		Query:    "test query " + wantID,
		MaxSats:  100,
		Revision: 1,
		Exp:      time.Now().Add(time.Hour).UnixMilli(),
	}
	sig, err := bstack.SignArtifact(key, w)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(bstack.SignedWant{Want: w, Sig: sig})
	return bstack.MarketPayload{Kind: bstack.KindWant, Body: body}
}

func makeWantBEEF(t *testing.T, key *ec.PrivateKey, wantID, domain string) []byte {
	t.Helper()
	beef, err := discovery.BuildMarketBEEF(makeWantPayload(t, key, wantID, domain))
	if err != nil {
		t.Fatal(err)
	}
	return beef
}

func postSubmitHTTP(t *testing.T, hubURL string, beef []byte, topics []string, payHdr string) *http.Response {
	t.Helper()
	tHdr, _ := json.Marshal(topics)
	req, err := http.NewRequest(http.MethodPost, hubURL+"/submit", bytes.NewReader(beef))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Topics", string(tHdr))
	if payHdr != "" {
		req.Header.Set("X-BSV-Payment", payHdr)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func fetchTermsHTTP(t *testing.T, hubURL, path string) hub.TermsDocument {
	t.Helper()
	resp, err := http.Get(hubURL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status %d", path, resp.StatusCode)
	}
	var doc hub.TermsDocument
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode terms: %v", err)
	}
	return doc
}

func TestHubFeesTermsAndPrefixValidation(t *testing.T) {
	hubPort := openMemoryPort(t, nil)
	dbPath := filepath.Join(t.TempDir(), "hub.sqlite")
	fs := hub.DefaultFeeSchedule(hub.PresetHybrid)
	fs.Role = hub.RoleSpine

	node, err := hub.NewNode(hub.Config{
		Network:      "memory",
		DBPath:       dbPath,
		Fees:         fs,
		FeeCollector: hub.NewPortFeeCollector(hubPort),
	})
	if err != nil {
		t.Fatalf("hub.NewNode: %v", err)
	}
	ts := httptest.NewServer(node.Handler())

	t1 := fetchTermsHTTP(t, ts.URL, "/v1/terms")
	t2 := fetchTermsHTTP(t, ts.URL, "/ls_nektar/terms")

	if t1.HubRole != hub.RoleSpine || t2.HubRole != hub.RoleSpine {
		t.Fatalf("expected spine role in both terms endpoints, got %q / %q", t1.HubRole, t2.HubRole)
	}
	if t1.PayeeIdentityKey != hubPort.IdentityPubHex() || t2.PayeeIdentityKey != hubPort.IdentityPubHex() {
		t.Fatalf("payee identity key mismatch")
	}
	if t1.Fees[hub.ActionSubmitWant] != 5 || t1.Fees[hub.ActionSubmitOffer] != 2 || t1.Fees[hub.ActionBboxSend] != 2 {
		t.Fatalf("unexpected hybrid fees: %+v", t1.Fees)
	}
	if t1.Egress.SatsPer100Events != 10 || t1.Prefixes[hub.ActionSubmitWant] == "" || t1.Prefixes[hub.ActionEgressTopup] == "" {
		t.Fatalf("missing egress or prefixes in terms: %+v", t1)
	}

	prefix := t1.Prefixes[hub.ActionSubmitWant]
	now := time.Now()
	if err := node.VerifyPrefix(prefix, hub.ActionSubmitWant, 5, now); err != nil {
		t.Fatalf("valid prefix rejected: %v", err)
	}
	if err := node.VerifyPrefix(prefix, hub.ActionSubmitOffer, 2, now); err == nil {
		t.Fatal("expected wrong action to be rejected")
	}
	if err := node.VerifyPrefix(prefix, hub.ActionSubmitWant, 6, now); err == nil {
		t.Fatal("expected higher minSats to be rejected")
	}
	if err := node.VerifyPrefix(prefix, hub.ActionSubmitWant, 5, now.Add(15*time.Minute)); err == nil {
		t.Fatal("expected expired prefix to be rejected")
	}
	if err := node.VerifyPrefix("not-valid-base64!!!", hub.ActionSubmitWant, 5, now); err == nil {
		t.Fatal("expected invalid base64 prefix to be rejected")
	}
	tampered := prefix[:len(prefix)-2] + "AA"
	if tampered == prefix {
		tampered = prefix[:len(prefix)-2] + "BB"
	}
	if err := node.VerifyPrefix(tampered, hub.ActionSubmitWant, 5, now); err == nil {
		t.Fatal("expected tampered HMAC prefix to be rejected")
	}

	// Prefix secret MUST persist in hub_kv across restart so in-flight prefixes remain valid!
	ts.Close()
	_ = node.Close()

	node2, err := hub.NewNode(hub.Config{
		Network:      "memory",
		DBPath:       dbPath,
		Fees:         fs,
		FeeCollector: hub.NewPortFeeCollector(hubPort),
	})
	if err != nil {
		t.Fatalf("reopen hub.NewNode: %v", err)
	}
	defer node2.Close()
	if err := node2.VerifyPrefix(prefix, hub.ActionSubmitWant, 5, now); err != nil {
		t.Fatalf("expected prefix issued before restart to verify after restart: %v", err)
	}
}

func TestHubFeeValve1SubmitValidateBeforeChargeReplayConflictAndPiggyback(t *testing.T) {
	ctx := context.Background()
	shared := chain.NewSharedMemoryChain()
	hubPort := openMemoryPort(t, shared)
	payerPort := openMemoryPort(t, shared)
	key, _ := ec.NewPrivateKey()
	lane := bstack.LaneOf("memory", "ops")

	fs := hub.FeeSchedule{
		Role:                 hub.RoleLeaf,
		SubmitWantSats:       5,
		SubmitOfferSats:      2,
		AcceptThresholdSats:  50,
		AcceptPayerLimitSats: 500,
		AcceptTotalLimitSats: 5000,
	}
	node, err := hub.NewNode(hub.Config{
		Network:      "memory",
		Fees:         fs,
		FeeCollector: hub.NewPortFeeCollector(hubPort),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	ts := httptest.NewServer(node.Handler())
	defer ts.Close()

	bal0 := hubPort.BalanceSats()

	// 1. Unpaid WantAd -> HTTP 402 with BRC-105 headers; MUST NOT admit to store.
	w1 := makeWantBEEF(t, key, "want-1", "ops")
	r402 := postSubmitHTTP(t, ts.URL, w1, []string{lane}, "")
	r402.Body.Close()
	if r402.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("expected 402 for unpaid WantAd, got %d", r402.StatusCode)
	}
	if r402.Header.Get("X-BSV-Payment-Satoshis-Required") != "5" ||
		r402.Header.Get("X-BSV-Payment-Identity") != hubPort.IdentityPubHex() ||
		r402.Header.Get("X-BSV-Payment-Derivation-Prefix") == "" {
		t.Fatalf("incomplete 402 headers: %+v", r402.Header)
	}
	if evs, _ := node.Store().EventsSince(0, nil, 10); len(evs) != 0 {
		t.Fatalf("unpaid WantAd must not be admitted, got %d events", len(evs))
	}

	// Build a valid standalone BRC-29 payment for submit:want using the 402 prefix.
	prefix1 := r402.Header.Get("X-BSV-Payment-Derivation-Prefix")
	payHdr1, txid1, err := discovery.BuildStandaloneHubPayment(ctx, payerPort, hubPort.IdentityPubHex(), prefix1, hub.ActionSubmitWant, 5)
	if err != nil {
		t.Fatalf("BuildStandaloneHubPayment: %v", err)
	}

	// 2. Malformed WantAd (corrupted signature) WITH valid payment -> HTTP 400 and ZERO fee claimed (validate-before-charge!).
	badWant := bstack.WantAd{
		WantID:   "bad-sig-want",
		Pub:      hex.EncodeToString(key.PubKey().Compressed()),
		Net:      "memory",
		Domains:  []string{"ops"},
		Query:    "bad",
		MaxSats:  10,
		Revision: 1,
		Exp:      time.Now().Add(time.Hour).UnixMilli(),
	}
	badBody, _ := json.Marshal(bstack.SignedWant{Want: badWant, Sig: "00deadbeef"})
	badBEEF, _ := discovery.BuildMarketBEEF(bstack.MarketPayload{Kind: bstack.KindWant, Body: badBody})
	rBad := postSubmitHTTP(t, ts.URL, badBEEF, []string{lane}, payHdr1)
	rBad.Body.Close()
	if rBad.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed WantAd, got %d", rBad.StatusCode)
	}
	earn0, _ := node.Store().FeeEarningsSummary(0)
	if earn0.TotalPayments != 0 || earn0.TotalEarnedSats != 0 {
		t.Fatalf("malformed WantAd MUST NOT claim or internalize payment: %+v", earn0)
	}

	// 3. Valid WantAd WITH that exact same unclaimed payment -> HTTP 200 & wallet balance +5 sats!
	rOK := postSubmitHTTP(t, ts.URL, w1, []string{lane}, payHdr1)
	okRaw, _ := io.ReadAll(rOK.Body)
	rOK.Body.Close()
	if rOK.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for paid WantAd, got %d: %s", rOK.StatusCode, string(okRaw))
	}
	if hubPort.BalanceSats() != bal0+5 {
		t.Fatalf("expected Hub wallet balance +5 (%d -> %d), got %d", bal0, bal0+5, hubPort.BalanceSats())
	}
	earn1, _ := node.Store().FeeEarningsSummary(0)
	if earn1.TotalEarnedSats != 5 || earn1.ByActionSats[hub.ActionSubmitWant] != 5 {
		t.Fatalf("unexpected earnings after 1st submit: %+v", earn1)
	}

	// 4. Replay exact same X-BSV-Payment txid -> HTTP 409 ERR_PAYMENT_REPLAYED.
	w2 := makeWantBEEF(t, key, "want-2", "ops")
	rReplay := postSubmitHTTP(t, ts.URL, w2, []string{lane}, payHdr1)
	replayRaw, _ := io.ReadAll(rReplay.Body)
	rReplay.Body.Close()
	if rReplay.StatusCode != http.StatusConflict || !strings.Contains(string(replayRaw), "ERR_PAYMENT_REPLAYED") {
		t.Fatalf("expected 409 ERR_PAYMENT_REPLAYED, got %d %s", rReplay.StatusCode, string(replayRaw))
	}

	// 5. Conflicting spent input (different txid spending the EXACT same input outpoint) -> HTTP 409 ERR_PAYMENT_CONFLICT.
	var parsedHdr hub.BRC105PaymentHeader
	_ = json.Unmarshal([]byte(payHdr1), &parsedHdr)
	rawBeef1, _ := hex.DecodeString(parsedHdr.BEEFHex)
	txConflict, err := transaction.NewTransactionFromBEEF(rawBeef1)
	if err != nil {
		t.Fatalf("parse tx1 BEEF: %v", err)
	}
	txConflict.LockTime = 777 // produces a distinct txid while preserving identical Inputs[0] and BRC-29 output!
	if txConflict.TxID().String() == txid1 {
		t.Fatal("expected modified LockTime to yield distinct txid")
	}
	conflictBeef, err := txConflict.AtomicBEEF(false)
	if err != nil {
		t.Fatal(err)
	}
	conflictHdr := parsedHdr
	conflictHdr.TxID = txConflict.TxID().String()
	conflictHdr.BEEFHex = hex.EncodeToString(conflictBeef)
	conflictHdrBytes, _ := json.Marshal(conflictHdr)
	rConf := postSubmitHTTP(t, ts.URL, w2, []string{lane}, string(conflictHdrBytes))
	confRaw, _ := io.ReadAll(rConf.Body)
	rConf.Body.Close()
	if rConf.StatusCode != http.StatusConflict || !strings.Contains(string(confRaw), "ERR_PAYMENT_CONFLICT") {
		t.Fatalf("expected 409 ERR_PAYMENT_CONFLICT, got %d %s", rConf.StatusCode, string(confRaw))
	}

	// 6. Single-tx fundTx piggybacking (Output 0 = carrier lock, Output 1 = BRC-29 Hub toll, 0 extra miner fee!).
	terms := fetchTermsHTTP(t, ts.URL, "/v1/terms")
	piggyPayload := makeWantPayload(t, key, "want-piggy", "ops")
	carrierBEEF, perHubHdrs, piggySats, err := discovery.BuildFundedMarketCarrierWithHubFees(
		ctx, payerPort, key, piggyPayload,
		[]discovery.HubPiggybackTarget{{
			HubURL:           ts.URL,
			PayeeIdentityKey: terms.PayeeIdentityKey,
			DerivationPrefix: terms.Prefixes[hub.ActionSubmitWant],
			Action:           hub.ActionSubmitWant,
			AmountSats:       5,
		}},
	)
	if err != nil {
		t.Fatalf("BuildFundedMarketCarrierWithHubFees: %v", err)
	}
	piggyHdr := perHubHdrs[ts.URL]
	if piggySats != 5 || piggyHdr == "" {
		t.Fatalf("expected piggybacked 5-sat header for %s, got sats=%d hdrs=%+v", ts.URL, piggySats, perHubHdrs)
	}
	var piggyDecoded hub.BRC105PaymentHeader
	_ = json.Unmarshal([]byte(piggyHdr), &piggyDecoded)
	if piggyDecoded.OutputIndex < 1 {
		t.Fatalf("expected piggybacked BRC-29 output at OutputIndex >= 1, got %d", piggyDecoded.OutputIndex)
	}
	rPiggy := postSubmitHTTP(t, ts.URL, carrierBEEF, []string{lane}, piggyHdr)
	piggyRespRaw, _ := io.ReadAll(rPiggy.Body)
	rPiggy.Body.Close()
	if rPiggy.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for piggybacked fundTx WantAd, got %d %s", rPiggy.StatusCode, string(piggyRespRaw))
	}
	if hubPort.BalanceSats() != bal0+10 {
		t.Fatalf("expected Hub wallet balance +10 after piggyback, got %d (start %d)", hubPort.BalanceSats(), bal0)
	}

	// 7. Flagged payer -> unmined 0-conf payment held (202 ERR_PAYMENT_HELD) -> retries same txid once mined -> 200!
	if err := node.OnFeePaymentLost("deadbeat-tx-000", payerPort.IdentityPubHex(), "simulated double-spend"); err != nil {
		t.Fatal(err)
	}
	terms2 := fetchTermsHTTP(t, ts.URL, "/v1/terms")
	payHdrHeld, txidHeld, err := discovery.BuildStandaloneHubPayment(ctx, payerPort, hubPort.IdentityPubHex(), terms2.Prefixes[hub.ActionSubmitWant], hub.ActionSubmitWant, 5)
	if err != nil {
		t.Fatal(err)
	}
	var heldHdr hub.BRC105PaymentHeader
	_ = json.Unmarshal([]byte(payHdrHeld), &heldHdr)
	rawHeldBeef, _ := hex.DecodeString(heldHdr.BEEFHex)
	txHeld, err := transaction.NewTransactionFromBEEF(rawHeldBeef)
	if err != nil {
		t.Fatal(err)
	}
	// Strip MerklePath to represent unmined 0-conf payment from a flagged payer.
	txHeld.MerklePath = nil
	unminedBytes, _ := txHeld.AtomicBEEF(false)
	unminedHdr := heldHdr
	unminedHdr.BEEFHex = hex.EncodeToString(unminedBytes)
	unminedHdrJSON, _ := json.Marshal(unminedHdr)

	wHeld := makeWantBEEF(t, key, "want-held", "ops")
	rHeld := postSubmitHTTP(t, ts.URL, wHeld, []string{lane}, string(unminedHdrJSON))
	heldBody, _ := io.ReadAll(rHeld.Body)
	rHeld.Body.Close()
	if rHeld.StatusCode != http.StatusAccepted || !strings.Contains(string(heldBody), "ERR_PAYMENT_HELD") {
		t.Fatalf("expected 202 ERR_PAYMENT_HELD for flagged payer unmined tx, got %d %s", rHeld.StatusCode, string(heldBody))
	}

	// Now attach the real mined BUMP from SharedMemoryChain for the EXACT SAME txid -> must upgrade held -> claimed and return 200!
	mp, _, ok := payerPort.Proof(ctx, txidHeld)
	if !ok || mp == nil {
		t.Fatalf("expected SharedMemoryChain mined MerklePath for %s", txidHeld)
	}
	txHeld.MerklePath = mp
	minedBytes, err := txHeld.AtomicBEEF(false)
	if err != nil {
		t.Fatal(err)
	}
	minedHdr := heldHdr
	minedHdr.BEEFHex = hex.EncodeToString(minedBytes)
	minedHdrJSON, _ := json.Marshal(minedHdr)
	rUpgraded := postSubmitHTTP(t, ts.URL, wHeld, []string{lane}, string(minedHdrJSON))
	upBody, _ := io.ReadAll(rUpgraded.Body)
	rUpgraded.Body.Close()
	if rUpgraded.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on mined retry of held txid %s, got %d %s", txidHeld, rUpgraded.StatusCode, string(upBody))
	}
	if err := node.OnFeePaymentMined(txid1, 900002); err != nil {
		t.Fatal(err)
	}
	earnFinal, _ := node.Store().FeeEarningsSummary(0)
	if earnFinal.TotalEarnedSats != 15 || earnFinal.MinedEarnedSats < 10 || earnFinal.FlaggedPayers != 1 {
		t.Fatalf("unexpected final earnings summary: %+v", earnFinal)
	}
}

func TestHubFeeValve2MeteredEgressSSEAndSyncAndSpineGuard(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	shared := chain.NewSharedMemoryChain()
	spinePort := openMemoryPort(t, shared)
	payerPort := openMemoryPort(t, shared)

	// 100 sats / 100 events = 1000 msats (1 sat) per event; minTopup = 2 sats (= 2 events).
	spineFS := hub.FeeSchedule{
		Role:                   hub.RoleSpine,
		EgressSatsPer100Events: 100,
		EgressMinTopupSats:     2,
	}
	spineNode, err := hub.NewNode(hub.Config{
		Network:      "memory",
		Fees:         spineFS,
		FeeCollector: hub.NewPortFeeCollector(spinePort),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer spineNode.Close()
	spineTS := httptest.NewServer(spineNode.Handler())
	defer spineTS.Close()
	defer func() {
		cancel()
		spineTS.CloseClientConnections()
	}()

	// 1. Unauthenticated GET /events & GET /sync -> HTTP 401.
	for _, path := range []string{"/events?cursor=0", "/sync?cursor=0"} {
		resp, err := http.Get(spineTS.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 for unauthenticated %s, got %d", path, resp.StatusCode)
		}
	}

	// 2. Free `dir` lane (?domains=dir) stays HTTP 200 even with 0 credits!
	respDir, err := http.Get(spineTS.URL + "/sync?cursor=0&domains=dir")
	if err != nil {
		t.Fatal(err)
	}
	respDir.Body.Close()
	if respDir.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for free dir lane, got %d", respDir.StatusCode)
	}

	// 3. Authenticated 0-credit subscriber -> HTTP 402 on /events and /sync.
	subKey, _ := ec.NewPrivateKey()
	subPub := hex.EncodeToString(subKey.PubKey().Compressed())
	doSignedGET := func(path, action string, cursor int64, doms string, key *ec.PrivateKey) *http.Response {
		pub, tsStr, nonce, sig, sErr := discovery.SignEgressAuthHeaders(key, action, spineTS.URL, cursor, doms)
		if sErr != nil {
			t.Fatal(sErr)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, spineTS.URL+path, nil)
		req.Header.Set("X-Nektar-Pub", pub)
		req.Header.Set("X-Nektar-Ts", tsStr)
		req.Header.Set("X-Nektar-Nonce", nonce)
		req.Header.Set("X-Nektar-Sig", sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	r0 := doSignedGET("/events?cursor=0", "hub_events", 0, "", subKey)
	r0.Body.Close()
	if r0.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("expected 402 for 0-credit /events, got %d", r0.StatusCode)
	}
	r0s := doSignedGET("/sync?cursor=0", "hub_sync", 0, "", subKey)
	r0s.Body.Close()
	if r0s.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("expected 402 for 0-credit /sync, got %d", r0s.StatusCode)
	}

	// Helper to top up subscriber egress credits via POST /v1/credits/topup.
	topupSub := func(sats uint64) {
		terms := fetchTermsHTTP(t, spineTS.URL, "/v1/terms")
		payHdr, _, err := discovery.BuildStandaloneHubPayment(ctx, payerPort, terms.PayeeIdentityKey, terms.Prefixes[hub.ActionEgressTopup], hub.ActionEgressTopup, sats)
		if err != nil {
			t.Fatalf("BuildStandaloneHubPayment topup: %v", err)
		}
		body, _ := json.Marshal(map[string]string{"payerPub": subPub})
		req, _ := http.NewRequest(http.MethodPost, spineTS.URL+"/v1/credits/topup", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-BSV-Payment", payHdr)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("topup failed (%d): %s", resp.StatusCode, string(raw))
		}
	}

	// 4. Submit 3 WantAds to Spine Hub, top up 2 sats (= 2 events), and verify SSE stops at event 2 with payment_required!
	adKey, _ := ec.NewPrivateKey()
	lane := bstack.LaneOf("memory", "ops")
	for i := 1; i <= 3; i++ {
		if _, err := spineNode.Submit(ctx, makeWantBEEF(t, adKey, fmt.Sprintf("egress-w-%d", i), "ops"), []string{lane}); err != nil {
			t.Fatal(err)
		}
	}

	topupSub(2)
	balResp, err := http.Get(spineTS.URL + "/v1/credits/balance?pub=" + subPub)
	if err != nil {
		t.Fatal(err)
	}
	var balDoc struct {
		BalanceMilliSats int64 `json:"balanceMilliSats"`
		EventsRemaining  int64 `json:"eventsRemaining"`
	}
	_ = json.NewDecoder(balResp.Body).Decode(&balDoc)
	balResp.Body.Close()
	if balDoc.BalanceMilliSats != 2000 || balDoc.EventsRemaining != 2 {
		t.Fatalf("unexpected credit balance after 2-sat topup: %+v", balDoc)
	}

	sseResp := doSignedGET("/events?cursor=0", "hub_events", 0, "", subKey)
	if sseResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on funded SSE, got %d", sseResp.StatusCode)
	}
	scanner := bufio.NewScanner(sseResp.Body)
	var marketCount int
	var lastSeq int64
	var gotPayReq bool
	var curEvent string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		} else if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			switch curEvent {
			case "market":
				var ev bstack.MarketEvent
				if json.Unmarshal([]byte(data), &ev) == nil {
					marketCount++
					if ev.Seq > lastSeq {
						lastSeq = ev.Seq
					}
				}
			case "payment_required":
				gotPayReq = true
			}
			curEvent = ""
		}
	}
	sseResp.Body.Close()
	if marketCount != 2 || !gotPayReq {
		t.Fatalf("expected exactly 2 market events then payment_required, got count=%d payReq=%v", marketCount, gotPayReq)
	}

	// 5. Top up 2 more sats and resume from lastSeq -> event #3 delivered with zero dropped events!
	topupSub(2)
	syncResp := doSignedGET(fmt.Sprintf("/sync?cursor=%d", lastSeq), "hub_sync", lastSeq, "", subKey)
	if syncResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on resumed /sync, got %d", syncResp.StatusCode)
	}
	var syncItems []hub.StoredEvent
	if err := json.NewDecoder(syncResp.Body).Decode(&syncItems); err != nil {
		t.Fatalf("decode /sync: %v", err)
	}
	syncResp.Body.Close()
	if len(syncItems) != 1 {
		t.Fatalf("expected exact 3rd remaining event after topup, got %d", len(syncItems))
	}

	// 6. Bilateral whitelisted Spine peer receives ALL 3 events free with 0 credits!
	peerKey, _ := ec.NewPrivateKey()
	peerPub := hex.EncodeToString(peerKey.PubKey().Compressed())
	spineNode.AddSpinePeerPub(peerPub)
	peerResp := doSignedGET("/sync?cursor=0", "hub_sync", 0, "", peerKey)
	if peerResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for bilateral Spine peer, got %d", peerResp.StatusCode)
	}
	var peerItems []hub.StoredEvent
	if err := json.NewDecoder(peerResp.Body).Decode(&peerItems); err != nil {
		t.Fatalf("decode peer /sync: %v", err)
	}
	peerResp.Body.Close()
	if len(peerItems) != 3 {
		t.Fatalf("expected bilateral Spine peer to receive all 3 events free, got %d", len(peerItems))
	}

	// 7. Metered BRC-149 Object Plane stream (/plane/stream) enforces the exact same Spine egress metering!
	if respUnauth, uErr := http.Get(spineTS.URL + "/plane/stream?from=0"); uErr != nil || respUnauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated /plane/stream, got err=%v resp=%+v", uErr, respUnauth)
	} else {
		respUnauth.Body.Close()
	}
	zeroKey, _ := ec.NewPrivateKey()
	rPlaneZero := doSignedGET("/plane/stream?from=0", "hub_plane", 0, "", zeroKey)
	rPlaneZero.Body.Close()
	if rPlaneZero.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("expected 402 for 0-credit /plane/stream, got %d", rPlaneZero.StatusCode)
	}
	_ = cancel
}

func TestHubFeeValve3BboxSendAndValve4Lookup(t *testing.T) {
	ctx := context.Background()
	shared := chain.NewSharedMemoryChain()
	hubPort := openMemoryPort(t, shared)
	payerPort := openMemoryPort(t, shared)

	fs := hub.FeeSchedule{
		Role:         hub.RoleLeaf,
		BboxSendSats: 3,
		LookupPrices: map[string]uint64{"history": 5, "export": 20},
	}
	node, err := hub.NewNode(hub.Config{
		Network:      "memory",
		Fees:         fs,
		FeeCollector: hub.NewPortFeeCollector(hubPort),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	ts := httptest.NewServer(node.Handler())
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	senderKey, _ := ec.NewPrivateKey()
	recipKey, _ := ec.NewPrivateKey()

	// Seed 1 WantAd so /lookup has data.
	if _, err := node.Submit(ctx, makeWantBEEF(t, senderKey, "lookup-w-1", "ops"), []string{bstack.LaneOf("memory", "ops")}); err != nil {
		t.Fatal(err)
	}

	// --- Valve 3: POST /bbox/send ---
	msg := hub.BboxMessage{
		RecipientPub: hex.EncodeToString(recipKey.PubKey().Compressed()),
		SenderPub:    hex.EncodeToString(senderKey.PubKey().Compressed()),
		Office:       "default",
		DealID:       "deal-bbox-1",
		Kind:         "purchase",
		Body:         json.RawMessage(`{"hello":"world"}`),
		TS:           time.Now().Unix(),
		Nonce:        "nonce-bbox-1",
		Aud:          u.Host,
		Sig:          "00badsignature",
	}
	// Invalid sig -> 401 (never charged!).
	badRaw, _ := json.Marshal(msg)
	r401, err := http.Post(ts.URL+"/bbox/send", "application/json", bytes.NewReader(badRaw))
	if err != nil {
		t.Fatal(err)
	}
	r401.Body.Close()
	if r401.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad bbox sig, got %d", r401.StatusCode)
	}

	// Valid sig, unpaid -> 402.
	msg.Sig, _ = bstack.SignArtifact(senderKey, msg.Signable())
	goodRaw, _ := json.Marshal(msg)
	r402, err := http.Post(ts.URL+"/bbox/send", "application/json", bytes.NewReader(goodRaw))
	if err != nil {
		t.Fatal(err)
	}
	prefixBbox := r402.Header.Get("X-BSV-Payment-Derivation-Prefix")
	r402.Body.Close()
	if r402.StatusCode != http.StatusPaymentRequired || prefixBbox == "" {
		t.Fatalf("expected 402 with prefix for unpaid /bbox/send, got %d", r402.StatusCode)
	}

	// Valid sig + BRC-105 payment -> 200 and stored in inbox!
	payBbox, _, err := discovery.BuildStandaloneHubPayment(ctx, payerPort, hubPort.IdentityPubHex(), prefixBbox, hub.ActionBboxSend, 3)
	if err != nil {
		t.Fatal(err)
	}
	reqPaid, _ := http.NewRequest(http.MethodPost, ts.URL+"/bbox/send", bytes.NewReader(goodRaw))
	reqPaid.Header.Set("Content-Type", "application/json")
	reqPaid.Header.Set("X-BSV-Payment", payBbox)
	rBboxOK, err := http.DefaultClient.Do(reqPaid)
	if err != nil {
		t.Fatal(err)
	}
	rBboxRaw, _ := io.ReadAll(rBboxOK.Body)
	rBboxOK.Body.Close()
	if rBboxOK.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for paid /bbox/send, got %d: %s", rBboxOK.StatusCode, string(rBboxRaw))
	}
	inbox, _ := node.Store().ListBboxMessages(msg.RecipientPub, 0, 10)
	if len(inbox) != 1 || inbox[0].DealID != "deal-bbox-1" {
		t.Fatalf("expected paid bbox message in inbox, got %+v", inbox)
	}

	// --- Valve 4: POST /lookup ---
	postLookup := func(query any, payHdr string) *http.Response {
		body, _ := json.Marshal(map[string]any{"service": bstack.LookupService, "query": query})
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/lookup", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if payHdr != "" {
			req.Header.Set("X-BSV-Payment", payHdr)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Live query -> free (200).
	rLive := postLookup(bstack.MarketQuery{Domain: "ops", Limit: 10}, "")
	rLive.Body.Close()
	if rLive.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for free live /lookup, got %d", rLive.StatusCode)
	}

	// History query (SinceSeq > 0) -> 402 -> paid (5 sats) -> 200.
	histQ := bstack.MarketQuery{SinceSeq: 1, Limit: 50}
	rHist402 := postLookup(histQ, "")
	prefHist := rHist402.Header.Get("X-BSV-Payment-Derivation-Prefix")
	rHist402.Body.Close()
	if rHist402.StatusCode != http.StatusPaymentRequired || prefHist == "" {
		t.Fatalf("expected 402 for history /lookup, got %d", rHist402.StatusCode)
	}
	payHist, _, err := discovery.BuildStandaloneHubPayment(ctx, payerPort, hubPort.IdentityPubHex(), prefHist, hub.ActionLookupPrefix+"history", 5)
	if err != nil {
		t.Fatal(err)
	}
	rHistOK := postLookup(histQ, payHist)
	rHistOK.Body.Close()
	if rHistOK.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for paid history /lookup, got %d", rHistOK.StatusCode)
	}

	// Export query (explicit class: "export") -> 402 -> paid (20 sats) -> 200.
	expQ := map[string]any{"class": "export", "limit": 500}
	rExp402 := postLookup(expQ, "")
	prefExp := rExp402.Header.Get("X-BSV-Payment-Derivation-Prefix")
	rExp402.Body.Close()
	if rExp402.StatusCode != http.StatusPaymentRequired || prefExp == "" {
		t.Fatalf("expected 402 for export /lookup, got %d", rExp402.StatusCode)
	}
	payExp, _, err := discovery.BuildStandaloneHubPayment(ctx, payerPort, hubPort.IdentityPubHex(), prefExp, hub.ActionLookupPrefix+"export", 20)
	if err != nil {
		t.Fatal(err)
	}
	rExpOK := postLookup(expQ, payExp)
	rExpOK.Body.Close()
	if rExpOK.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for paid export /lookup, got %d", rExpOK.StatusCode)
	}

	earn, _ := node.Store().FeeEarningsSummary(0)
	if earn.TotalEarnedSats != 28 || earn.ByActionSats[hub.ActionBboxSend] != 3 ||
		earn.ByActionSats[hub.ActionLookupPrefix+"history"] != 5 || earn.ByActionSats[hub.ActionLookupPrefix+"export"] != 20 {
		t.Fatalf("unexpected earnings for Valves 3 & 4: %+v", earn)
	}
}
