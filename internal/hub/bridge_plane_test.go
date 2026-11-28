package hub

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/testchain"

	"github.com/kryptokrates/nektar/internal/bstack"
)

func TestBRC149FramingAndBands(t *testing.T) {
	// Verify Paper 010 §2 exact table for "tm_uhrp_files":
	//   width 0  -> 0x1000
	//   width 4  -> 0x1004
	//   width 8  -> 0x104B
	//   width 12 -> 0x14BD
	//   width 15 -> 0x35EA
	tid := TopicIDOf("tm_uhrp_files")
	cases := []struct {
		bits uint8
		want uint16
	}{
		{0, 0x1000},
		{4, 0x1004},
		{8, 0x104B},
		{12, 0x14BD},
		{15, 0x35EA},
	}
	for _, tc := range cases {
		if got := BandOf(tid, tc.bits); got != tc.want {
			t.Fatalf("BandOf(tm_uhrp_files, %d) = 0x%04X, want 0x%04X", tc.bits, got, tc.want)
		}
	}

	raw := []byte{0x01, 0x00, 0xBE, 0xEF, 0x42, 0x99}
	rec, err := EncodeBRC149Record("tm_nektar_testnet_a", raw)
	if err != nil {
		t.Fatalf("EncodeBRC149Record: %v", err)
	}
	if len(rec) != BRC149HeaderBytes+len(raw) {
		t.Fatalf("unexpected BRC-149 record len %d", len(rec))
	}
	gotTID, gotBEEF, err := DecodeBRC149Record(bytes.NewReader(rec), MaxPlaneObjectLen)
	if err != nil {
		t.Fatalf("DecodeBRC149Record: %v", err)
	}
	if gotTID != TopicIDOf("tm_nektar_testnet_a") || !bytes.Equal(gotBEEF, raw) {
		t.Fatalf("BRC-149 roundtrip mismatch")
	}

	frameBytes, err := EncodePlaneFrame(PlaneFrame{FlowSeq: 77, Topic: "tm_nektar_testnet_a", BEEF: raw})
	if err != nil {
		t.Fatalf("EncodePlaneFrame: %v", err)
	}
	gotFrame, err := DecodePlaneFrame(bytes.NewReader(frameBytes), MaxPlaneObjectLen)
	if err != nil {
		t.Fatalf("DecodePlaneFrame: %v", err)
	}
	if gotFrame.FlowSeq != 77 || gotFrame.TopicID != TopicIDOf("tm_nektar_testnet_a") || !bytes.Equal(gotFrame.BEEF, raw) {
		t.Fatalf("PlaneFrame roundtrip mismatch: %+v", gotFrame)
	}
}

func TestOverlayBridgeEngineContractAndLoopGuard(t *testing.T) {
	node, err := NewNode(Config{
		Network:       "memory",
		TransportMode: TransportPlane,
	})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	defer node.Close()
	if !node.propagationOff {
		t.Fatal("expected TransportPlane to switch propagationOff=true automatically")
	}

	ts := httptest.NewServer(node.Handler())
	defer ts.Close()

	// 1. GET /listTopicManagers returns JSON object + ETag; conditional GET returns 304.
	resp, err := http.Get(ts.URL + "/listTopicManagers")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /listTopicManagers: err=%v resp=%v", err, resp)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag on /listTopicManagers")
	}
	var tms map[string]map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&tms); err != nil {
		t.Fatalf("decode /listTopicManagers: %v", err)
	}
	resp.Body.Close()
	if len(tms) < 16 {
		t.Fatalf("expected >=16 market topic managers, got %d", len(tms))
	}

	req304, _ := http.NewRequest(http.MethodGet, ts.URL+"/listTopicManagers", nil)
	req304.Header.Set("If-None-Match", etag)
	resp304, err := http.DefaultClient.Do(req304)
	if err != nil || resp304.StatusCode != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified on matching ETag, got err=%v resp=%v", err, resp304)
	}
	resp304.Body.Close()

	// 2. GET /listLookupServiceProviders returns ls_nektar.
	lsResp, err := http.Get(ts.URL + "/listLookupServiceProviders")
	if err != nil || lsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /listLookupServiceProviders: err=%v resp=%v", err, lsResp)
	}
	var lss map[string]map[string]any
	_ = json.NewDecoder(lsResp.Body).Decode(&lss)
	lsResp.Body.Close()
	if _, ok := lss[bstack.LookupService]; !ok {
		t.Fatalf("expected %s in /listLookupServiceProviders, got %+v", bstack.LookupService, lss)
	}

	// 3. LoopGuard suppresses byte-identical echo, but passes proof-refreshed BEEF (different bytes!).
	lg := NewLoopGuard(4)
	beefV1 := []byte("beef-unmined-bytes")
	beefV2WithBUMP := []byte("beef-with-mined-bump-bytes")
	if !lg.MarkIfUnseen(beefV1, "tm_nektar_testnet_0") {
		t.Fatal("first sight of beefV1 must be unseen")
	}
	if lg.MarkIfUnseen(beefV1, "tm_nektar_testnet_0") {
		t.Fatal("byte-identical echo of beefV1 must be suppressed")
	}
	if !lg.MarkIfUnseen(beefV2WithBUMP, "tm_nektar_testnet_0") {
		t.Fatal("proof-refreshed beefV2WithBUMP must NOT be suppressed by LoopGuard")
	}
}

func TestCarrierOutpointJournalAndKillSwitchSweep(t *testing.T) {
	ctx := context.Background()
	tc := testchain.New(1)
	dbPath := filepath.Join(t.TempDir(), "hub.sqlite")

	node, err := NewNode(Config{
		Network:      "regtest",
		DBPath:       dbPath,
		ChainTracker: tc,
	})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	defer node.Close()

	priv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatalf("NewPrivateKey: %v", err)
	}
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())
	pw, err := sdkwallet.NewCompletedProtoWallet(priv)
	if err != nil {
		t.Fatalf("NewCompletedProtoWallet: %v", err)
	}
	fundLock, err := carrier.FundingLock(ctx, pw, bstack.Originator, bstack.MarketCarrierParams)
	if err != nil {
		t.Fatalf("FundingLock: %v", err)
	}
	// Mine a coinbase on testchain, then spend it into a mined funding tree output F:0.
	addr, err := script.NewAddressFromPublicKey(priv.PubKey(), false)
	if err != nil {
		t.Fatalf("NewAddressFromPublicKey: %v", err)
	}
	if _, err := tc.Generate(1, addr.AddressString); err != nil {
		t.Fatalf("tc.Generate: %v", err)
	}
	txids := tc.Txids()
	if len(txids) == 0 {
		t.Fatal("tc.Txids empty after Generate")
	}
	cbTxID := txids[0]
	// Mature coinbase by 100 blocks so tc.Send accepts spending it.
	if _, err := tc.Generate(100, addr.AddressString); err != nil {
		t.Fatalf("tc.Generate maturity: %v", err)
	}
	cbTx := tc.Tx(cbTxID)
	if mp, _, ok := tc.Proof(cbTxID); ok {
		cbTx.MerklePath = mp
	}
	ut, err := p2pkh.Unlock(priv, nil)
	if err != nil {
		t.Fatalf("p2pkh.Unlock: %v", err)
	}
	fundTx := transaction.NewTransaction()
	fundTx.AddInputFromTx(cbTx, 0, ut)
	fundTx.AddOutput(&transaction.TransactionOutput{Satoshis: 1000, LockingScript: fundLock})
	if err := fundTx.Sign(); err != nil {
		t.Fatalf("fundTx.Sign: %v", err)
	}
	if err := tc.Send(fundTx); err != nil {
		t.Fatalf("tc.Send(fundTx): %v", err)
	}
	if mp, _, ok := tc.Proof(fundTx.TxID().String()); ok {
		fundTx.MerklePath = mp
	}

	mintWantCarrier := func(wantID string) ([]byte, string) {
		w := bstack.WantAd{
			WantID:   wantID,
			Pub:      pubHex,
			Net:      "regtest",
			Domains:  []string{"ops"},
			Query:    "test " + wantID,
			MaxSats:  100,
			Revision: 1,
			Exp:      time.Now().Add(time.Hour).UnixMilli(),
		}
		sig, sErr := bstack.SignArtifact(priv, w)
		if sErr != nil {
			t.Fatalf("SignArtifact: %v", sErr)
		}
		pWant, pErr := bstack.NewWantPayload(bstack.SignedWant{Want: w, Sig: sig})
		if pErr != nil {
			t.Fatalf("NewWantPayload: %v", pErr)
		}
		cborBytes, eErr := bstack.EncodePayload(pWant)
		if eErr != nil {
			t.Fatalf("EncodePayload: %v", eErr)
		}
		cTx, mErr := carrier.Mint(ctx, pw, bstack.Originator, bstack.MarketCarrierParams, cborBytes, fundTx, 0)
		if mErr != nil {
			t.Fatalf("carrier.Mint: %v", mErr)
		}
		cTx.Inputs[0].SourceTransaction = fundTx
		beef, bErr := cTx.AtomicBEEF(false)
		if bErr != nil {
			t.Fatalf("AtomicBEEF: %v", bErr)
		}
		return beef, cTx.TxID().String()
	}

	topic := bstack.LaneOf("regtest", "ops")
	beef1, _ := mintWantCarrier("want-first")
	if _, err := node.Submit(ctx, beef1, []string{topic}); err != nil {
		t.Fatalf("first carrier Submit failed: %v", err)
	}

	// 1. A second distinct carrier spending the SAME funding output F:0 MUST be refused (Paper 011 §6)!
	beef2, _ := mintWantCarrier("want-second-double-spend-carrier")
	if _, err := node.Submit(ctx, beef2, []string{topic}); err == nil {
		t.Fatal("expected second carrier spending same funding outpoint to be refused by outpoint journal")
	}

	// 2. Kill-switch sweep: build a mineable (LockTime=0, Sequence=0xFFFFFFFF) tx spending F:0.
	unlocker := bstack.MarketCarrierParams.Derivation.Unlocker(ctx, pw, bstack.Originator)
	sweepTx := transaction.NewTransaction()
	sweepTx.LockTime = 0
	sweepTx.AddInputFromTx(fundTx, 0, unlocker)
	sweepTx.Inputs[0].SequenceNumber = transaction.MaxTxInSequenceNum
	sweepTx.AddOutput(&transaction.TransactionOutput{Satoshis: 1000, LockingScript: fundLock})
	if err := sweepTx.Sign(); err != nil {
		t.Fatalf("sweep sign: %v", err)
	}
	sweepBEEF, err := sweepTx.AtomicBEEF(false)
	if err != nil {
		t.Fatalf("sweep AtomicBEEF: %v", err)
	}
	if _, err := node.Submit(ctx, sweepBEEF, []string{topic}); err != nil {
		t.Fatalf("kill-switch sweep Submit failed: %v", err)
	}

	// Verify carrier 1 was evicted from active events & ls_nektar!
	evs, _ := node.store.EventsSince(0, nil, 50)
	if len(evs) != 0 {
		t.Fatalf("expected retracted carrier to be excluded from EventsSince, got %d", len(evs))
	}

	// Re-submitting carrier 1 after the kill-switch sweep MUST stay refused permanently!
	if _, err := node.Submit(ctx, beef1, []string{topic}); err == nil {
		t.Fatal("expected retracted carrier re-submission to fail closed after kill-switch sweep")
	}
}
