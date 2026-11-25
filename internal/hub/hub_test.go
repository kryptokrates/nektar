package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
