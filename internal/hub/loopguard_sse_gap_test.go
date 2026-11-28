package hub

import (
	"bufio"
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

func buildTestWantBEEF(t *testing.T, priv *ec.PrivateKey, wantID string) []byte {
	t.Helper()
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())
	w := bstack.WantAd{
		WantID:   wantID,
		Pub:      pubHex,
		Net:      "memory",
		Domains:  []string{"ops"},
		Query:    "rfc-test-" + wantID,
		MaxSats:  100,
		Revision: 1,
		Exp:      time.Now().Add(time.Hour).UnixMilli(),
	}
	sig, err := bstack.SignArtifact(priv, w)
	if err != nil {
		t.Fatal(err)
	}
	p, err := bstack.NewWantPayload(bstack.SignedWant{Want: w, Sig: sig})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := bstack.EncodePayload(p)
	if err != nil {
		t.Fatal(err)
	}
	s := &script.Script{}
	_ = s.AppendOpcodes(script.OpFALSE, script.OpRETURN)
	_ = s.AppendPushData(wire)
	tx := transaction.NewTransaction()
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 0, LockingScript: s})
	beef, err := tx.BEEF()
	if err != nil {
		t.Fatal(err)
	}
	return beef
}

func TestHubManifestCapabilitiesAndUnknownTopicRFC6709(t *testing.T) {
	node, err := NewNode(Config{Network: "memory", DBPath: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	// 1. /manifest.json exposes RFC 9170 / RFC 6709 capabilities under metanet.nektar.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18420/manifest.json", nil)
	node.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest HTTP %d", rec.Code)
	}
	var doc struct {
		Metanet struct {
			Nektar struct {
				WireVersion       string   `json:"wireVersion"`
				SupportedKinds    []int    `json:"supportedKinds"`
				CBORExtensionRule string   `json:"cborExtensionRule"`
				PlaneMediaTypes   []string `json:"planeMediaTypes"`
			} `json:"nektar"`
		} `json:"metanet"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if doc.Metanet.Nektar.WireVersion != "nkt1" || doc.Metanet.Nektar.CBORExtensionRule != "even-critical-odd-optional" {
		t.Fatalf("unexpected manifest nektar capabilities: %+v", doc.Metanet.Nektar)
	}

	// 2. Submitting to [unknownFutureTopic, knownLaneTopic] admits known lane cleanly without crashing on unknown topic.
	priv, _ := ec.NewPrivateKey()
	beef := buildTestWantBEEF(t, priv, "w-rfc6709-1")
	knownTopic := bstack.LaneOf("memory", "ops")
	steak, err := node.Submit(context.Background(), beef, []string{"tm_future_unknown_v9", knownTopic})
	if err != nil {
		t.Fatalf("Submit with unknown co-located topic failed: %v", err)
	}
	if steak[knownTopic] == nil || len(steak[knownTopic].OutputsToAdmit) != 1 {
		t.Fatalf("expected known topic %s to admit output, got %+v", knownTopic, steak)
	}

	// 3. LoopGuard MUST be keyed on rawBEEF (matching plane_brc149_client), not txid[:].
	if !node.loopGuard.Seen(beef, knownTopic) {
		t.Fatalf("expected LoopGuard.Seen(rawBEEF, topic) == true after Submit")
	}
}

func TestHubSSEEmitsGapOnSlowConsumer(t *testing.T) {
	node, err := NewNode(Config{Network: "memory", DBPath: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	node.sseBufSize = 2

	ts := httptest.NewServer(node.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events?format=slim", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Wait for subscriber to register.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		node.mu.RLock()
		nSubs := len(node.subs)
		node.mu.RUnlock()
		if nSubs > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Burst > 2 events directly via broadcastEvent before client drains resp.Body, forcing overflow.
	for i := int64(1); i <= 10; i++ {
		node.broadcastEvent(bstack.MarketEvent{
			Seq:     i,
			Topic:   "tm_nektar_memory_0",
			TxID:    "tx",
			Payload: bstack.MarketPayload{Kind: bstack.KindWant},
		}, []string{"ops"})
	}

	sc := bufio.NewScanner(resp.Body)
	sawGap := false
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "event: gap" {
			sawGap = true
			break
		}
	}
	if !sawGap {
		t.Fatalf("expected slow-consumer SSE stream to emit 'event: gap'")
	}
}
