package hub

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

type noopTracker struct{}

func (noopTracker) IsValidRootForHeight(_ context.Context, _ *chainhash.Hash, _ uint32) (bool, error) {
	return true, nil
}
func (noopTracker) CurrentHeight(_ context.Context) (uint32, error) { return 100, nil }

func TestHubLiveNodeRefusesNilTrackerUnfundedOpReturnAndEmptyLeaf(t *testing.T) {
	// 1. Live network Hub MUST refuse nil ChainTracker (#1, #15).
	if _, err := NewNode(Config{Network: "testnet", ChainTracker: nil}); err == nil {
		t.Fatal("expected NewNode(testnet, nil tracker) to fail closed")
	}
	if _, err := NewNode(Config{Network: "main", ChainTracker: nil}); err == nil {
		t.Fatal("expected NewNode(main, nil tracker) to fail closed")
	}

	// 2. Live network Hub MUST reject 0-input unfunded OP_RETURN carriers (#15).
	liveNode, err := NewNode(Config{Network: "testnet", ChainTracker: noopTracker{}})
	if err != nil {
		t.Fatalf("NewNode testnet with tracker: %v", err)
	}
	defer liveNode.Close()

	key, _ := ec.NewPrivateKey()
	want := bstack.WantAd{
		WantID:   "live-unfunded-1",
		Pub:      hex.EncodeToString(key.PubKey().Compressed()),
		Net:      "testnet",
		Domains:  []string{"ops"},
		Query:    "test",
		MaxSats:  100,
		Revision: 1,
		Exp:      time.Now().Add(time.Hour).UnixMilli(),
	}
	sig, _ := bstack.SignArtifact(key, want)
	body, _ := json.Marshal(bstack.SignedWant{Want: want, Sig: sig})
	cborBytes, _ := bstack.EncodePayload(bstack.MarketPayload{Kind: bstack.KindWant, Body: body})
	ls := &script.Script{}
	_ = ls.AppendOpcodes(script.OpFALSE, script.OpRETURN)
	_ = ls.AppendPushData(cborBytes)
	tx := transaction.NewTransaction()
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 0, LockingScript: ls})
	beef, _ := tx.AtomicBEEF(false)
	if _, err := liveNode.Submit(context.Background(), beef, []string{bstack.LaneOf("testnet", "ops")}); err == nil {
		t.Fatal("expected live testnet Hub to refuse 0-input unfunded OP_RETURN carrier")
	}

	// 3. ValidateMarketPayload MUST reject OfferAd with empty Leaf (#3).
	badOffer := bstack.OfferAd{
		OfferID:   "off-empty-leaf",
		WantID:    "w-1",
		Pub:       hex.EncodeToString(key.PubKey().Compressed()),
		Net:       "main",
		Domains:   []string{"ops"},
		PriceSats: 100,
		Leaf:      "", // bypass attempt!
		Exp:       time.Now().Add(time.Hour).UnixMilli(),
	}
	oSig, _ := bstack.SignArtifact(key, badOffer)
	oBody, _ := json.Marshal(bstack.SignedOffer{Offer: badOffer, Sig: oSig})
	if err := bstack.ValidateMarketPayload(bstack.MarketPayload{Kind: bstack.KindOffer, Body: oBody}, "main", bstack.LaneOf("main", "ops"), time.Now()); err == nil {
		t.Fatal("expected ValidateMarketPayload to reject OfferAd with empty Leaf")
	}
}

func TestHubMarketEventsCarryBEEFBountyPending202AndDripFaucetBEEF(t *testing.T) {
	ctx := context.Background()
	node, err := NewNode(Config{Network: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	ts := httptest.NewServer(node.Handler())
	defer ts.Close()

	// 1. Admitted WantAd returned by /lookup carries non-empty BEEF (#19).
	key, _ := ec.NewPrivateKey()
	want := bstack.WantAd{
		WantID:   "beef-carry-1",
		Pub:      hex.EncodeToString(key.PubKey().Compressed()),
		Net:      "memory",
		Domains:  []string{"ops"},
		Query:    "carry beef check",
		MaxSats:  100,
		Revision: 1,
		Exp:      time.Now().Add(time.Hour).UnixMilli(),
	}
	sig, _ := bstack.SignArtifact(key, want)
	body, _ := json.Marshal(bstack.SignedWant{Want: want, Sig: sig})
	cborBytes, _ := bstack.EncodePayload(bstack.MarketPayload{Kind: bstack.KindWant, Body: body})
	ls := &script.Script{}
	_ = ls.AppendOpcodes(script.OpFALSE, script.OpRETURN)
	_ = ls.AppendPushData(cborBytes)
	tx := transaction.NewTransaction()
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 0, LockingScript: ls})
	beef, _ := tx.AtomicBEEF(false)
	if _, err := node.Submit(ctx, beef, []string{bstack.LaneOf("memory", "ops")}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	qBody, _ := json.Marshal(map[string]any{
		"service": bstack.LookupService,
		"query":   bstack.MarketQuery{WantID: "beef-carry-1"},
	})
	resp, err := http.Post(ts.URL+"/lookup", "application/json", bytes.NewReader(qBody))
	if err != nil {
		t.Fatal(err)
	}
	var ans struct {
		Result []bstack.MarketEvent `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ans)
	resp.Body.Close()
	if len(ans.Result) != 1 || len(ans.Result[0].BEEF) == 0 {
		t.Fatalf("expected /lookup MarketEvent to carry non-empty BEEF, got %+v", ans.Result)
	}

	// 2. Retrying a 'broadcast' or 'pending' bounty claim returns HTTP 202 Accepted (not 409) (#14, #42).
	_ = node.Store().UpsertBounty(Bounty{ID: "b-202", Domain: "ops", Question: "q", RewardSats: 100, MaxFills: 5, Active: true})
	canonHDP := strings.Repeat("cd", 32)
	if err := node.Store().ReserveBountyClaim("b-202", canonHDP, hex.EncodeToString(key.PubKey().Compressed()), 100); err != nil {
		t.Fatal(err)
	}
	if err := node.Store().MarkBountyClaimBroadcast(canonHDP, "tx-broadcast-1"); err != nil {
		t.Fatal(err)
	}
	node.SetBountyHandler(func(_ context.Context, b Bounty, _ BountySubmission) (*BountyPayoutResult, error) {
		return &BountyPayoutResult{Accepted: true, BountyID: b.ID}, nil
	})
	sub := BountySubmission{
		BountyID:      "b-202",
		SellerPub:     hex.EncodeToString(key.PubKey().Compressed()),
		SellerAddress: "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa",
		HDP:           canonHDP,
		Text:          "pending broadcast fact",
		TS:            time.Now().Unix(),
	}
	sub.Sig, _ = bstack.SignArtifact(key, sub.Signable(canonHDP))
	subRaw, _ := json.Marshal(sub)
	r202, err := http.Post(ts.URL+"/bounties/submit", "application/json", bytes.NewReader(subRaw))
	if err != nil {
		t.Fatal(err)
	}
	defer r202.Body.Close()
	if r202.StatusCode != http.StatusAccepted {
		t.Fatalf("expected HTTP 202 Accepted for in-flight broadcast bounty retry, got %d", r202.StatusCode)
	}

	// 3. DripFaucetBEEF returns valid parseable AtomicBEEF hex (#12, #20).
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
	addr, _ := port.FundAddress()
	fTxID, fBEEFHex, sent, err := DripFaucetBEEF(ctx, mem, addr, 500, 1000)
	if err != nil || fTxID == "" || fBEEFHex == "" || sent != 500 {
		t.Fatalf("DripFaucetBEEF: txid=%q beefLen=%d sent=%d err=%v", fTxID, len(fBEEFHex), sent, err)
	}
	fBeef, err := hex.DecodeString(fBEEFHex)
	if err != nil {
		t.Fatal(err)
	}
	if _, ptx, pHash, pErr := guard.ParseBEEF(fBeef, guard.DefaultBound); pErr != nil || ptx == nil || pHash.String() != fTxID {
		t.Fatalf("DripFaucetBEEF AtomicBEEF parse/txid mismatch: err=%v", pErr)
	}
}
