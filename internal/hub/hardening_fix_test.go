package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
)

func TestHandleSyncDomainFilterPaginatesPast200NonMatchingRows(t *testing.T) {
	node, err := NewNode(Config{Network: "regtest"})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	exp := time.Now().Add(time.Hour).UnixMilli()
	for i := 1; i <= 210; i++ {
		if _, _, err := node.store.AppendEvent(StoredEvent{
			Outpoint: fmt.Sprintf("aa%062d.0", i),
			Topic:    bstack.LaneOf("regtest", "noise"),
			TxID:     fmt.Sprintf("aa%062d", i),
			Vout:     0,
			Kind:     bstack.KindWant,
			Pub:      "02" + fmt.Sprintf("%064d", i),
			Domains:  []string{"noise"},
			Payload:  bstack.MarketPayload{Kind: bstack.KindWant, Body: json.RawMessage(`{}`)},
			BEEF:     []byte{0x01, 0x02},
			Exp:      exp,
		}); err != nil {
			t.Fatalf("AppendEvent noise %d: %v", i, err)
		}
	}
	for i := 1; i <= 2; i++ {
		if _, _, err := node.store.AppendEvent(StoredEvent{
			Outpoint: fmt.Sprintf("bb%062d.0", i),
			Topic:    bstack.LaneOf("regtest", "target"),
			TxID:     fmt.Sprintf("bb%062d", i),
			Vout:     0,
			Kind:     bstack.KindWant,
			Pub:      "02" + fmt.Sprintf("%064d", i),
			Domains:  []string{"target"},
			Payload:  bstack.MarketPayload{Kind: bstack.KindWant, Body: json.RawMessage(`{}`)},
			BEEF:     []byte{0x03, 0x04},
			Exp:      exp,
		}); err != nil {
			t.Fatalf("AppendEvent target %d: %v", i, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/sync?cursor=0&domains=target", nil)
	rec := httptest.NewRecorder()
	node.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var items []StoredEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode sync response: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 target events past 210 noise rows, got %d", len(items))
	}
	nextCur, _ := strconv.ParseInt(rec.Header().Get("X-Nektar-Next-Cursor"), 10, 64)
	if nextCur < 212 {
		t.Fatalf("expected X-Nektar-Next-Cursor >= 212, got %d", nextCur)
	}
}

func TestHandlePlaneStreamPaginatesPast500Frames(t *testing.T) {
	node, err := NewNode(Config{Network: "regtest"})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	exp := time.Now().Add(time.Hour).UnixMilli()
	topic := bstack.LaneOf("regtest", "ops")
	const total = 520
	for i := 1; i <= total; i++ {
		if _, _, err := node.store.AppendEvent(StoredEvent{
			Outpoint: fmt.Sprintf("cc%062d.0", i),
			Topic:    topic,
			TxID:     fmt.Sprintf("cc%062d", i),
			Vout:     0,
			Kind:     bstack.KindWant,
			Pub:      "02" + fmt.Sprintf("%064d", i),
			Domains:  []string{"ops"},
			Payload:  bstack.MarketPayload{Kind: bstack.KindWant, Body: json.RawMessage(`{}`)},
			BEEF:     []byte{0xde, 0xad, byte(i >> 8), byte(i)},
			Exp:      exp,
		}); err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
	}

	srv := httptest.NewServer(node.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/plane/stream?from=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /plane/stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	for wantSeq := uint64(1); wantSeq <= total; wantSeq++ {
		f, dErr := DecodePlaneFrame(resp.Body, MaxPlaneObjectLen)
		if dErr != nil {
			t.Fatalf("DecodePlaneFrame seq=%d failed: %v", wantSeq, dErr)
		}
		if f.FlowSeq != wantSeq {
			t.Fatalf("expected FlowSeq=%d, got %d", wantSeq, f.FlowSeq)
		}
	}
	cancel()
}

func TestAllowPubRateHardCapAndPrefixRejectsPipe(t *testing.T) {
	node, err := NewNode(Config{Network: "regtest"})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	for i := 0; i < maxRateBuckets+200; i++ {
		node.allowPubRate(fmt.Sprintf("pub-%d", i))
	}
	node.mu.RLock()
	rateCount := len(node.rates)
	node.mu.RUnlock()
	if rateCount > maxRateBuckets {
		t.Fatalf("expected n.rates len <= %d, got %d", maxRateBuckets, rateCount)
	}

	prices := ParseLookupPrices("bad|class=10,good=5")
	if _, hasBad := prices["bad|class"]; hasBad {
		t.Fatal("ParseLookupPrices should reject class containing '|'")
	}
	if prices["good"] != 5 {
		t.Fatalf("expected good=5, got %d", prices["good"])
	}
	if tok := node.IssuePrefix("lookup:bad|class", 5, time.Now()); tok != "" {
		t.Fatalf("expected empty prefix for action containing '|', got %q", tok)
	}
	if err := node.VerifyPrefix("dGVzdA==", "lookup:bad|class", 5, time.Now()); err == nil {
		t.Fatal("expected VerifyPrefix to reject action containing '|'")
	}
}
