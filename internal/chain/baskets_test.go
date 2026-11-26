package chain

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func TestBuildFundedTxWithBasketAndListAssetBaskets(t *testing.T) {
	ctx := WithAgentID(context.Background(), "Claude Code")
	port, err := Open(ctx, Config{
		Network: "memory",
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	if port.BalanceSats() == 0 {
		t.Fatal("expected seeded memory balance")
	}

	ls := &script.Script{}
	_ = ls.AppendOpcodes(script.OpTRUE)
	meta := nwallet.ActionAnchorShard("demo-shard", 0, 1)
	spec := &nwallet.OutputTagSpec{
		Basket:             nwallet.BasketAnchors,
		Tags:               []string{"anchor", "shard:demo-shard", "batch:0"},
		CustomInstructions: `{"v":1,"kind":"anchor","shard":"demo-shard","n":0,"size":1}`,
	}
	tx, _, fee, spend, err := port.BuildFundedTxWithBasket(ctx, []*transaction.TransactionOutput{
		{Satoshis: 1, LockingScript: ls},
	}, meta.Description, meta.Labels, spec)
	if err != nil {
		t.Fatalf("BuildFundedTxWithBasket: %v", err)
	}
	if _, err := port.Broadcast(ctx, tx, fee, spend); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}

	baskets := port.ListAssetBaskets(ctx)
	if len(baskets) != len(nwallet.AllAssetBaskets) {
		t.Fatalf("expected %d asset baskets, got %d", len(nwallet.AllAssetBaskets), len(baskets))
	}
	var anchorBasket *AssetBasketSummary
	for i := range baskets {
		if baskets[i].Basket == nwallet.BasketAnchors {
			anchorBasket = &baskets[i]
			break
		}
	}
	if anchorBasket == nil || anchorBasket.Count != 1 || anchorBasket.TotalSats != 1 {
		t.Fatalf("expected 1 spendable 1-sat output in nektar-anchors, got %+v", anchorBasket)
	}
	if gotShard, _ := anchorBasket.Items[0].Meta["shard"].(string); gotShard != "demo-shard" {
		t.Fatalf("expected parsed customInstructions shard=demo-shard, got %+v", anchorBasket.Items[0].Meta)
	}
}
