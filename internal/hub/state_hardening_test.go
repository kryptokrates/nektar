package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func buildSignedWantBEEF(t *testing.T, priv *ec.PrivateKey, wantID string, expMs int64) []byte {
	t.Helper()
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())
	want := bstack.WantAd{
		WantID:   wantID,
		Pub:      pubHex,
		Net:      "regtest",
		Domains:  []string{"ops"},
		Query:    "query " + wantID,
		MaxSats:  100,
		Revision: 1,
		Exp:      expMs,
	}
	sig, err := bstack.SignArtifact(priv, want)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(bstack.SignedWant{Want: want, Sig: sig})
	recBytes, err := bstack.EncodePayload(bstack.MarketPayload{Kind: bstack.KindWant, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	ls := &script.Script{}
	_ = ls.AppendOpcodes(script.OpFALSE, script.OpRETURN)
	_ = ls.AppendPushData(recBytes)
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 0, LockingScript: ls})
	raw, err := tx.AtomicBEEF(false)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestBountyAtomicMaxFillsAndRejectsForgedProof covers F2:
// - Empty / forged ProofBundle rejected (HTTP 400)
// - 10 concurrent valid submissions against MaxFills=1 result in STRICTLY 1 payout!
func TestBountyAtomicMaxFillsAndRejectsForgedProof(t *testing.T) {
	ctx := context.Background()
	db, _ := store.Open(":memory:")
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

	node, err := NewNode(Config{
		Network:      "memory",
		DBPath:       ":memory:",
		ChainTracker: port.Tracker(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	// Arm real verified bounty handler!
	if _, err := ArmHubLiquidity(ctx, node, mem, LiquidityConfig{
		SeedStarterShards: false,
		ArmBounties:       true,
		BountyRewardSats:  100,
	}); err != nil {
		t.Fatal(err)
	}
	_ = node.Store().UpsertBounty(Bounty{
		ID:         "b-one-slot",
		Domain:     "ops",
		Question:   "single fill test",
		RewardSats: 100,
		MaxFills:   1,
		Active:     true,
	})
	ts := httptest.NewServer(node.Handler())
	defer ts.Close()

	// 1. Forged / empty ProofBundle must be rejected (HTTP 400/401) and NOT consume the slot!
	forgedKey, _ := ec.NewPrivateKey()
	forgedHDP := fmt.Sprintf("%064x", 0xdeadbeef)
	forgedSub := BountySubmission{
		BountyID:      "b-one-slot",
		SellerPub:     hex.EncodeToString(forgedKey.PubKey().Compressed()),
		SellerAddress: "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa",
		HDP:           forgedHDP,
		Text:          "unanchored spoof text",
		ProofBundle:   json.RawMessage(`{}`),
		TS:            time.Now().Unix(),
	}
	forgedSub.Sig, _ = bstack.SignArtifact(forgedKey, forgedSub.Signable(forgedHDP))
	forged, _ := json.Marshal(forgedSub)
	resp, err := http.Post(ts.URL+"/bounties/submit", "application/json", bytes.NewReader(forged))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 on forged ProofBundle, got %d", resp.StatusCode)
	}

	// 2. 10 concurrent valid signed submissions against MaxFills=1 -> strictly 1 payout!
	var payouts atomic.Int32
	node.SetBountyHandler(func(_ context.Context, b Bounty, sub BountySubmission) (*BountyPayoutResult, error) {
		payouts.Add(1)
		return &BountyPayoutResult{Accepted: true, BountyID: b.ID, RewardSats: b.RewardSats, TxID: "tx-" + sub.HDP}, nil
	})
	var wins atomic.Int32
	var wg sync.WaitGroup
	wg.Add(10)
	for i := 0; i < 10; i++ {
		i := i
		go func() {
			defer wg.Done()
			sk, _ := ec.NewPrivateKey()
			hdp := fmt.Sprintf("%064x", i+1)
			sub := BountySubmission{
				BountyID:      "b-one-slot",
				SellerPub:     hex.EncodeToString(sk.PubKey().Compressed()),
				SellerAddress: "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa",
				HDP:           hdp,
				Text:          fmt.Sprintf("fact %d", i),
				TS:            time.Now().Unix(),
			}
			sub.Sig, _ = bstack.SignArtifact(sk, sub.Signable(hdp))
			body, _ := json.Marshal(sub)
			r, rErr := http.Post(ts.URL+"/bounties/submit", "application/json", bytes.NewReader(body))
			if rErr == nil {
				if r.StatusCode == http.StatusOK {
					wins.Add(1)
				}
				r.Body.Close()
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || payouts.Load() != 1 {
		t.Fatalf("expected strictly 1 bounty win & 1 payout for MaxFills=1, got wins=%d payouts=%d", wins.Load(), payouts.Load())
	}
}

// TestSSEBacklogDrainExceeding200Events covers F12: paginated backlog drain delivers ALL 240 events without gap!
func TestSSEBacklogDrainExceeding200Events(t *testing.T) {
	ctx := context.Background()
	node, err := NewNode(Config{Network: "regtest", DBPath: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	node.maxPerMin = 5000

	priv, _ := ec.NewPrivateKey()
	lane := bstack.LaneOf("regtest", "ops")
	exp := time.Now().Add(time.Hour).UnixMilli()
	const total = 235
	for i := 0; i < total; i++ {
		beef := buildSignedWantBEEF(t, priv, fmt.Sprintf("w-backlog-%03d", i), exp)
		if _, err := node.Submit(ctx, beef, []string{lane}); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}

	ts := httptest.NewServer(node.Handler())
	defer ts.Close()

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, ts.URL+"/events?cursor=0", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events?cursor=0 failed: err=%v resp=%+v", err, resp)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	got := 0
	var lastSeq int64
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev bstack.MarketEvent
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) == nil {
			if ev.Seq != lastSeq+1 {
				t.Fatalf("SSE sequence gap: prev=%d got=%d", lastSeq, ev.Seq)
			}
			lastSeq = ev.Seq
			got++
			if got == total {
				break
			}
		}
	}
	if got != total {
		t.Fatalf("expected all %d SSE backlog events delivered without gap, got %d", total, got)
	}
}

// TestRestoreOver1000EventsAndPruneExpired covers F17.
func TestRestoreOver1000EventsAndPruneExpired(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "hub.sqlite")
	node1, err := NewNode(Config{Network: "regtest", DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	node1.maxPerMin = 5000
	priv, _ := ec.NewPrivateKey()
	lane := bstack.LaneOf("regtest", "ops")
	exp := time.Now().Add(time.Hour).UnixMilli()
	const total = 1050
	for i := 0; i < total; i++ {
		beef := buildSignedWantBEEF(t, priv, fmt.Sprintf("w-restore-%04d", i), exp)
		if _, err := node1.Submit(ctx, beef, []string{lane}); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}
	_ = node1.Close()

	// Reopen Hub on same SQLite DB -> paginated restoreFromStore must recover ALL 1050 events!
	node2, err := NewNode(Config{Network: "regtest", DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer node2.Close()

	// Verify the 1050th event (beyond the old 1000-row restore limit!) is present in ls_nektar AND
	// PruneExpired confirms ALL 1050 restored entries were loaded into RAM lookup!
	qBytes, _ := json.Marshal(bstack.MarketQuery{Kind: bstack.KindWant, WantID: "w-restore-1049", Limit: 10})
	ans, err := node2.Lookup(ctx, &lookup.LookupQuestion{Service: bstack.LookupService, Query: qBytes})
	if err != nil {
		t.Fatalf("Lookup after restore: %v", err)
	}
	events, ok := ans.Result.([]bstack.MarketEvent)
	if !ok || len(events) != 1 {
		t.Fatalf("expected 1050th event w-restore-1049 restored in ls_nektar, got %d", len(events))
	}

	// PruneExpired with future timestamp clears ALL 1050 restored entries from RAM lookup!
	pruned := node2.marketLS.PruneExpired(exp + 1000)
	if pruned != total {
		t.Fatalf("expected PruneExpired to remove all %d restored entries, removed %d", total, pruned)
	}
}
