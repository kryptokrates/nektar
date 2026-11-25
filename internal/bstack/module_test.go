package bstack

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/module"
)

func TestMarketOverlayModuleAdmitAndLookup(t *testing.T) {
	ctx := context.Background()
	var events []MarketEvent
	factory := NewMarketModule("regtest", func(ev MarketEvent) {
		events = append(events, ev)
	})
	mod, err := factory(nil)
	if err != nil {
		t.Fatalf("NewMarketModule: %v", err)
	}
	if len(mod.Topics) != 16 {
		t.Fatalf("expected 16 nibble topics, got %d", len(mod.Topics))
	}

	buyerKey, _ := ec.NewPrivateKey()
	buyerPub := hex.EncodeToString(buyerKey.PubKey().Compressed())
	want := WantAd{
		WantID:   "want-001",
		Pub:      buyerPub,
		Net:      "regtest",
		Domains:  []string{"finance"},
		Query:    "Q3 BSV Teranode throughput",
		MaxSats:  500,
		Revision: 1,
		Exp:      time.Now().Add(time.Hour).UnixMilli(),
	}
	sig, err := SignArtifact(buyerKey, want)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(SignedWant{Want: want, Sig: sig})
	cborBytes, err := EncodePayload(MarketPayload{Kind: KindWant, Body: body})
	if err != nil {
		t.Fatalf("EncodePayload: %v", err)
	}

	// Build a minimal tx with OP_FALSE OP_RETURN <cborBytes> on the matching nibble lane.
	ls := &script.Script{}
	_ = ls.AppendOpcodes(script.OpFALSE, script.OpRETURN)
	_ = ls.AppendPushData(cborBytes)
	tx := transaction.NewTransaction()
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 0, LockingScript: ls})
	beef := transaction.NewBeef()
	if _, err := beef.MergeTransaction(tx); err != nil {
		t.Fatal(err)
	}
	txid := tx.TxID()
	lane := LaneOf("regtest", "finance")
	tm := mod.Topics[lane]
	if tm == nil {
		t.Fatalf("missing topic manager for lane %s", lane)
	}
	inst, err := tm.IdentifyAdmissibleOutputs(ctx, beef, txid, nil)
	if err != nil {
		t.Fatalf("IdentifyAdmissibleOutputs: %v", err)
	}
	if len(inst.OutputsToAdmit) != 1 || inst.OutputsToAdmit[0] != 0 {
		t.Fatalf("expected output 0 admitted on %s, got %+v", lane, inst.OutputsToAdmit)
	}

	// Wrong lane must refuse admission!
	wrongLane := LaneOf("regtest", "dir")
	if wrongLane == lane {
		wrongLane = LaneOf("regtest", "other-domain-xyz")
	}
	wrongInst, _ := mod.Topics[wrongLane].IdentifyAdmissibleOutputs(ctx, beef, txid, nil)
	if len(wrongInst.OutputsToAdmit) != 0 {
		t.Fatalf("expected wrong lane %s to admit 0 outputs, got %+v", wrongLane, wrongInst.OutputsToAdmit)
	}

	// Admit into ls_nektar and verify both Lookup and live SSE callback!
	lsvc := mod.Lookups[LookupService]
	op := transaction.Outpoint{Txid: *txid, Index: 0}
	if err := lsvc.OutputAdmittedByTopic(ctx, &module.OutputAdmittedByTopic{
		Mode:          module.AdmitLockingScript,
		Outpoint:      op,
		Topic:         lane,
		LockingScript: ls,
	}); err != nil {
		t.Fatalf("OutputAdmittedByTopic: %v", err)
	}
	if len(events) != 1 || events[0].Payload.Kind != KindWant {
		t.Fatalf("expected 1 live admission event, got %+v", events)
	}
	qBytes, _ := json.Marshal(MarketQuery{Domain: "finance", Kind: KindWant})
	ans, err := lsvc.Lookup(ctx, &lookup.LookupQuestion{Service: LookupService, Query: qBytes})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	gotEvents, ok := ans.Result.([]MarketEvent)
	if !ok || len(gotEvents) != 1 {
		t.Fatalf("expected 1 lookup result, got %#v", ans.Result)
	}
}
