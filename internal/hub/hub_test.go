package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/kryptokrates/nektar/internal/bstack"
)

func TestEmbeddedHubWithLightwebFingerAndSSE(t *testing.T) {
	node, err := NewNode(Config{
		Network:      "regtest",
		EnableFinger: true,
	})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	// Verify both Nektar's 16 market lanes AND Lightweb's tm_finger + ls_finger mounted in ONE Go process!
	topics := node.Topics()
	if len(topics) != 17 {
		t.Fatalf("expected 17 mounted topics (16 nektar + tm_finger), got %d: %v", len(topics), topics)
	}
	lookups := node.Lookups()
	if len(lookups) != 2 {
		t.Fatalf("expected 2 mounted lookups (ls_finger + ls_nektar), got %v", lookups)
	}

	ts := httptest.NewServer(node.Handler())
	defer ts.Close()

	// Start SSE subscriber before submitting!
	sseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, ts.URL+"/events", nil)
	sseResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer sseResp.Body.Close()

	// Build & submit a signed WantAd BEEF to /submit.
	buyerKey, _ := ec.NewPrivateKey()
	want := bstack.WantAd{
		WantID:   "hub-want-1",
		Pub:      hex.EncodeToString(buyerKey.PubKey().Compressed()),
		Net:      "regtest",
		Domains:  []string{"ops"},
		Query:    "BSV Chronicle height",
		MaxSats:  1000,
		Revision: 1,
		Exp:      time.Now().Add(time.Hour).UnixMilli(),
	}
	sig, _ := bstack.SignArtifact(buyerKey, want)
	body, _ := json.Marshal(bstack.SignedWant{Want: want, Sig: sig})
	cborBytes, _ := bstack.EncodePayload(bstack.MarketPayload{Kind: bstack.KindWant, Body: body})

	ls := &script.Script{}
	_ = ls.AppendOpcodes(script.OpFALSE, script.OpRETURN)
	_ = ls.AppendPushData(cborBytes)
	tx := transaction.NewTransaction()
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 0, LockingScript: ls})
	beef, err := tx.AtomicBEEF(false)
	if err != nil {
		t.Fatalf("AtomicBEEF: %v", err)
	}

	lane := bstack.LaneOf("regtest", "ops")
	subReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/submit", bytes.NewReader(beef))
	subReq.Header.Set("X-Topics", `["`+lane+`"]`)
	subResp, err := http.DefaultClient.Do(subReq)
	if err != nil || subResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /submit failed: resp=%+v err=%v", subResp, err)
	}
	subResp.Body.Close()

	// Verify live SSE delivered the event immediately!
	sc := bufio.NewScanner(sseResp.Body)
	gotSSE := false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "hub-want-1") {
			gotSSE = true
			break
		}
	}
	if !gotSSE {
		t.Fatal("expected live SSE event for hub-want-1")
	}

	// Verify BRC-24 /lookup returns the admitted want!
	qBody, _ := json.Marshal(map[string]any{
		"service": bstack.LookupService,
		"query":   map[string]any{"domain": "ops", "wantId": "hub-want-1"},
	})
	lResp, err := http.Post(ts.URL+"/lookup", "application/json", bytes.NewReader(qBody))
	if err != nil || lResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /lookup failed: resp=%+v err=%v", lResp, err)
	}
	defer lResp.Body.Close()
	var parsed map[string]any
	if err := json.NewDecoder(lResp.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	resList, _ := parsed["result"].([]any)
	if len(resList) != 1 {
		t.Fatalf("expected 1 lookup result, got %#v", parsed)
	}
}

func TestHubSQLiteRestartFederationBboxAndBounties(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "hub1.sqlite")
	node1, err := NewNode(Config{Network: "memory", DBPath: dbPath})
	if err != nil {
		t.Fatalf("NewNode 1: %v", err)
	}
	buyerKey, _ := ec.NewPrivateKey()
	want := bstack.WantAd{
		WantID:   "persist-want-1",
		Pub:      hex.EncodeToString(buyerKey.PubKey().Compressed()),
		Net:      "memory",
		Domains:  []string{"ops"},
		Query:    "durable hub check",
		MaxSats:  500,
		Revision: 1,
		Exp:      time.Now().Add(time.Hour).UnixMilli(),
	}
	sig, _ := bstack.SignArtifact(buyerKey, want)
	body, _ := json.Marshal(bstack.SignedWant{Want: want, Sig: sig})
	cborBytes, _ := bstack.EncodePayload(bstack.MarketPayload{Kind: bstack.KindWant, Body: body})
	ls := &script.Script{}
	_ = ls.AppendOpcodes(script.OpFALSE, script.OpRETURN)
	_ = ls.AppendPushData(cborBytes)
	tx := transaction.NewTransaction()
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 0, LockingScript: ls})
	beef, _ := tx.AtomicBEEF(false)

	lane := bstack.LaneOf("memory", "ops")
	if _, err := node1.Submit(context.Background(), beef, []string{lane}); err != nil {
		t.Fatalf("Submit to hub1: %v", err)
	}
	_ = node1.Close()

	// Reopen Hub 1 on the SAME SQLite file and assert event survives restart + replays over cursored SSE!
	node1Reopened, err := NewNode(Config{Network: "memory", DBPath: dbPath})
	if err != nil {
		t.Fatalf("Reopen hub1: %v", err)
	}
	defer node1Reopened.Close()
	ts1 := httptest.NewServer(node1Reopened.Handler())
	defer ts1.Close()

	// Verify /events?cursor=0 replays persisted seq=1 after restart!
	sseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, ts1.URL+"/events?cursor=0", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events?cursor=0: %v", err)
	}
	sc := bufio.NewScanner(resp.Body)
	replayed := false
	for sc.Scan() {
		if strings.Contains(sc.Text(), "persist-want-1") {
			replayed = true
			break
		}
	}
	resp.Body.Close()
	if !replayed {
		t.Fatal("expected SQLite-persisted want to replay over /events?cursor=0 after hub restart")
	}

	// Start Hub 2 federating from Hub 1 and verify it pulls persist-want-1 automatically!
	node2, err := NewNode(Config{Network: "memory"})
	if err != nil {
		t.Fatalf("NewNode 2: %v", err)
	}
	defer node2.Close()
	fedCtx, fedCancel := context.WithCancel(context.Background())
	defer fedCancel()
	node2.StartFederation(fedCtx, []string{ts1.URL})

	deadline := time.Now().Add(4 * time.Second)
	federated := false
	for time.Now().Before(deadline) {
		evs, _ := node2.Store().EventsSince(0, nil, 10)
		if len(evs) > 0 {
			federated = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !federated {
		t.Fatal("expected Hub 2 to federate persist-want-1 from Hub 1")
	}

	// Verify NAT bbox relay (/bbox/send & /bbox/inbox) enforces ECDSA signatures!
	senderKey, _ := ec.NewPrivateKey()
	recipientKey, _ := ec.NewPrivateKey()
	senderPub := hex.EncodeToString(senderKey.PubKey().Compressed())
	recipientPub := hex.EncodeToString(recipientKey.PubKey().Compressed())

	unsignedPayload, _ := json.Marshal(BboxMessage{
		Office:       "default",
		RecipientPub: recipientPub,
		SenderPub:    senderPub,
		DealID:       "deal-nat-unsigned",
		Kind:         "purchase_beef",
		Body:         json.RawMessage(`{"ok":true}`),
	})
	if unResp, err := http.Post(ts1.URL+"/bbox/send", "application/json", bytes.NewReader(unsignedPayload)); err != nil || unResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401 for unsigned /bbox/send, got err=%v resp=%+v", err, unResp)
	} else {
		unResp.Body.Close()
	}

	u1, _ := url.Parse(ts1.URL)
	hubAud := strings.ToLower(u1.Host)

	signedMsg := BboxMessage{
		Office:       "default",
		RecipientPub: recipientPub,
		SenderPub:    senderPub,
		DealID:       "deal-nat-1",
		Kind:         "purchase_beef",
		TS:           time.Now().Unix(),
		Nonce:        "nonce-send-1",
		Aud:          hubAud,
		Body:         json.RawMessage(`{"ok":true}`),
	}
	bboxSig, err := bstack.SignArtifact(senderKey, signedMsg.Signable())
	if err != nil {
		t.Fatalf("SignArtifact bbox send: %v", err)
	}
	signedMsg.Sig = bboxSig
	bboxPayload, _ := json.Marshal(signedMsg)
	bResp, err := http.Post(ts1.URL+"/bbox/send", "application/json", bytes.NewReader(bboxPayload))
	if err != nil || bResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /bbox/send failed: err=%v resp=%+v", err, bResp)
	}
	bResp.Body.Close()

	// Replaying the exact same signed /bbox/send nonce must return 409!
	if repResp, err := http.Post(ts1.URL+"/bbox/send", "application/json", bytes.NewReader(bboxPayload)); err != nil || repResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected HTTP 409 for replayed /bbox/send nonce, got err=%v resp=%+v", err, repResp)
	} else {
		repResp.Body.Close()
	}

	// Unsigned /bbox/inbox must return 401!
	if unInResp, err := http.Get(ts1.URL + "/bbox/inbox?pub=" + recipientPub + "&since=0"); err != nil || unInResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401 for unsigned /bbox/inbox, got err=%v resp=%+v", err, unInResp)
	} else {
		unInResp.Body.Close()
	}

	tsNow := time.Now().Unix()
	inNonce := "nonce-inbox-1"
	inSig, err := bstack.SignArtifact(recipientKey, map[string]any{
		"action": "bbox_inbox",
		"pub":    recipientPub,
		"ts":     tsNow,
		"nonce":  inNonce,
		"aud":    hubAud,
		"since":  int64(0),
	})
	if err != nil {
		t.Fatalf("SignArtifact bbox inbox: %v", err)
	}
	inReq, _ := http.NewRequest(http.MethodGet, ts1.URL+"/bbox/inbox?pub="+recipientPub+"&since=0", nil)
	inReq.Header.Set("X-Nektar-Pub", recipientPub)
	inReq.Header.Set("X-Nektar-Ts", strconv.FormatInt(tsNow, 10))
	inReq.Header.Set("X-Nektar-Nonce", inNonce)
	inReq.Header.Set("X-Nektar-Sig", inSig)
	inResp, err := http.DefaultClient.Do(inReq)
	if err != nil || inResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /bbox/inbox failed: err=%v resp=%+v", err, inResp)
	}
	var inbox struct {
		Messages []BboxMessage `json:"messages"`
	}
	_ = json.NewDecoder(inResp.Body).Decode(&inbox)
	inResp.Body.Close()
	if len(inbox.Messages) != 1 || inbox.Messages[0].DealID != "deal-nat-1" {
		t.Fatalf("expected 1 bbox message, got %+v", inbox.Messages)
	}

	// Verify bounty registration, payout handler, and duplicate-HDP rejection (HTTP 409) including mixed-case hex!
	_ = node1Reopened.Store().UpsertBounty(Bounty{ID: "b-test", Domain: "ops", Question: "test", RewardSats: 100, MaxFills: 5, Active: true})
	node1Reopened.SetBountyHandler(func(_ context.Context, b Bounty, sub BountySubmission) (*BountyPayoutResult, error) {
		return &BountyPayoutResult{Accepted: true, BountyID: b.ID, RewardSats: b.RewardSats, TxID: "deadbeef"}, nil
	})
	sellerKey, _ := ec.NewPrivateKey()
	sellerPub := hex.EncodeToString(sellerKey.PubKey().Compressed())
	canonHDP := strings.Repeat("ab", 32)
	sub := BountySubmission{
		BountyID:      "b-test",
		SellerPub:     sellerPub,
		SellerAddress: "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa",
		HDP:           canonHDP,
		Text:          "verified ops fact for hub bounty",
		TS:            time.Now().Unix(),
	}
	sub.Sig, err = bstack.SignArtifact(sellerKey, sub.Signable(canonHDP))
	if err != nil {
		t.Fatalf("SignArtifact bounty submit: %v", err)
	}
	subBody, _ := json.Marshal(sub)
	r1, err := http.Post(ts1.URL+"/bounties/submit", "application/json", bytes.NewReader(subBody))
	if err != nil || r1.StatusCode != http.StatusOK {
		t.Fatalf("first bounty submit failed: err=%v resp=%+v", err, r1)
	}
	r1.Body.Close()
	// Duplicate with uppercase hex variant must ALSO collide on canonical HDP (HTTP 409).
	subUpper := sub
	subUpper.HDP = strings.ToUpper(canonHDP)
	subBodyUpper, _ := json.Marshal(subUpper)
	r2, err := http.Post(ts1.URL+"/bounties/submit", "application/json", bytes.NewReader(subBodyUpper))
	if err != nil || r2.StatusCode != http.StatusConflict {
		t.Fatalf("expected HTTP 409 on duplicate HDP bounty submit, got err=%v code=%d", err, r2.StatusCode)
	}
	r2.Body.Close()
}

func TestHubCloseDrainsActiveSSEImmediately(t *testing.T) {
	node, err := NewNode(Config{Network: "memory"})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	ts := httptest.NewServer(node.Handler())

	sseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, ts.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events failed: resp=%+v err=%v", resp, err)
	}
	defer resp.Body.Close()

	start := time.Now()
	node.CloseSSE()
	ts.CloseClientConnections()
	ts.Close()
	if err := node.Close(); err != nil {
		t.Fatalf("node.Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("expected active SSE drain & close in <2s, took %v", elapsed)
	}
}
