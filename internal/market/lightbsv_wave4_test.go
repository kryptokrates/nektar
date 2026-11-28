package market_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lightwebinc/bstore/storerec"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func newWave4Agent(t *testing.T, ctx context.Context, shared *chain.SharedMemoryChain, hubURLs []string, bboxOffice string) (*memory.Service, *market.Engine, *market.Broker) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir + "/nektar.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	port, err := chain.Open(ctx, chain.Config{
		Network:           "memory",
		DataDir:           dir,
		SharedMemoryChain: shared,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = port.Close() })
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	eng := market.NewEngine(mem, market.DefaultPolicy())
	t.Cleanup(func() { _ = eng.CloseWorkflows() })
	disc := discovery.New(mem, hubURLs, nil)
	br := market.NewBroker(mem, eng, disc, market.BrokerConfig{
		BboxOffice:     bboxOffice,
		CollectTimeout: 2 * time.Second,
		GraceTimeout:   60 * time.Millisecond,
	})
	t.Cleanup(func() { br.Stop() })
	return mem, eng, br
}

func TestNativeBboxBoxrecEnvelopeAndZeroPollWake(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// 1. Unit-verify native Lightweb BRC-169 + BRC-78 `boxrec` envelope sealing & opening via MarketSigner.
	alice, err := nwallet.NewEphemeralMarketSigner()
	if err != nil {
		t.Fatal(err)
	}
	bob, err := nwallet.NewEphemeralMarketSigner()
	if err != nil {
		t.Fatal(err)
	}
	aliceW, err := alice.ProtoWallet()
	if err != nil {
		t.Fatal(err)
	}
	bobW, err := bob.ProtoWallet()
	if err != nil {
		t.Fatal(err)
	}
	nowTS := time.Now().Unix()
	aad := bstack.BboxAAD("nektar_abcdefghij", alice.PubHex(), bob.PubHex(), "deal-native-1", "purchase_beef")
	plain := []byte(`{"offerId":"off-1","txid":"deadbeef"}`)

	sealed, err := bstack.SealBboxMessageBody(ctx, aliceW, "nektar_abcdefghij", "purchase_beef", alice.PubHex(), bob.PubHex(), nowTS, aad, plain)
	if err != nil {
		t.Fatalf("SealBboxMessageBody: %v", err)
	}
	if !bstack.IsNativeBboxContent(sealed) {
		t.Fatalf("expected native BRC-169 metanetHandles bbox envelope, got %s", string(sealed))
	}
	opened, err := bstack.OpenBboxMessageBody(ctx, bobW, bob, "nektar_abcdefghij", "purchase_beef", alice.PubHex(), bob.PubHex(), nowTS, aad, sealed)
	if err != nil {
		t.Fatalf("OpenBboxMessageBody: %v", err)
	}
	if string(opened) != string(plain) {
		t.Fatalf("decrypted payload mismatch: got %q, want %q", string(opened), string(plain))
	}
	// Tampered AAD MUST fail closed!
	badAAD := bstack.BboxAAD("nektar_abcdefghij", alice.PubHex(), bob.PubHex(), "deal-other", "purchase_beef")
	if _, err := bstack.OpenBboxMessageBody(ctx, bobW, bob, "nektar_abcdefghij", "purchase_beef", alice.PubHex(), bob.PubHex(), nowTS, badAAD, sealed); err == nil {
		t.Fatal("expected AAD mismatch to fail closed on OpenBboxMessageBody")
	}

	// 2. Full NAT-to-NAT trade (neither Seller nor Buyer has a PublicURL) over SSE bbox_wake with ZERO spin-polling!
	shared := chain.NewSharedMemoryChain()
	hubNode, err := hub.NewNode(hub.Config{
		Network:      "memory",
		EnableFinger: true,
		EnableBbox:   true,
		EnableBstore: true,
		EnableFabric: true,
	})
	if err != nil {
		t.Fatalf("hub.NewNode: %v", err)
	}
	t.Cleanup(func() { _ = hubNode.Close() })
	ts := httptest.NewServer(hubNode.Handler())
	t.Cleanup(func() {
		hubNode.CloseSSE()
		ts.CloseClientConnections()
		ts.Close()
	})

	office := hubNode.DefaultBboxOffice()
	sellerMem, sellerEng, sellerBroker := newWave4Agent(t, ctx, shared, []string{ts.URL}, office)
	if _, err := market.ApprovePolicyPreset(sellerMem.DB(), sellerEng.Policy(), "both", nil, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := sellerMem.Remember(ctx, memory.RememberInput{
		Shard:      "lightbsv-ops",
		Text:       "LightBSV BRC-223 multicast interest datagrams push topic admissions in sub-10ms without polling.",
		Source:     "architect:jeff",
		Distilled:  true,
		AutoAnchor: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sellerBroker.ArmSell(ctx, market.SellInput{
		Shard:     "lightbsv-ops",
		Domains:   []string{"bsv-overlay"},
		PriceSats: 45,
	}); err != nil {
		t.Fatal(err)
	}
	sellerBroker.Start(ctx)

	buyerMem, buyerEng, buyerBroker := newWave4Agent(t, ctx, shared, []string{ts.URL}, office)
	t.Cleanup(cancel)
	if _, err := market.ApprovePolicyPreset(buyerMem.DB(), buyerEng.Policy(), "buyer", nil, "test"); err != nil {
		t.Fatal(err)
	}
	buyerBroker.Start(ctx)

	if !sellerBroker.WaitConnected(ctx, 1, 2*time.Second) || !buyerBroker.WaitConnected(ctx, 1, 2*time.Second) {
		t.Fatal("expected Seller and Buyer SSE connections to complete handshake")
	}

	t0 := time.Now()
	deal, err := buyerBroker.Want(ctx, market.WantRequest{
		Domains:        []string{"bsv-overlay"},
		Question:       "How fast do BRC-223 multicast interest datagrams push topic admissions?",
		MaxSats:        100,
		TargetShard:    "bought/lightbsv",
		CollectTimeout: 2 * time.Second,
	})
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatalf("NAT Want failed: %v", err)
	}
	if deal.Status != "settled" || !strings.Contains(deal.DeliveredText, "BRC-223") {
		t.Fatalf("expected settled NAT deal with BRC-223 text, got status=%s text=%q err=%s", deal.Status, deal.DeliveredText, deal.Error)
	}
	// Previously bboxLoop slept 3000ms + 500ms = 3500ms; with SSE bbox_wake + native boxrec it settles well under 1.5s!
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("expected event-driven NAT bbox trade to settle in <1.5s without polling, took %v", elapsed)
	}
}

func TestTwoTierBstoreEntryAndRCA1DirectSaleGate(t *testing.T) {
	ctx := context.Background()
	shared := chain.NewSharedMemoryChain()
	sellerMem, sellerEng, _ := newWave4Agent(t, ctx, shared, nil, "nektar_abcdefghij")

	// 1. Verify Tier-2 (>16 KiB) `storerec.Entry` construction & round-trip CBOR decode.
	largeBlob := make([]byte, 24<<10)
	for i := range largeBlob {
		largeBlob[i] = byte('a' + (i % 26))
	}
	if bstack.ClassifyDeliveryTier(len(largeBlob)) != bstack.DeliveryTierBstore {
		t.Fatal("expected 24 KiB payload to classify as DeliveryTierBstore")
	}
	if bstack.ClassifyDeliveryTier(4096) != bstack.DeliveryTierInline {
		t.Fatal("expected 4 KiB payload to classify as DeliveryTierInline")
	}
	chirpURL, entry, entryCBOR, hashes, err := bstack.BuildBstoreEntryForData(
		sellerMem.Objects(),
		"nektar",
		sellerEng.Signer().PubCompressed(),
		"capsules/volumetric.nkt",
		"application/vnd.nektar.capsule",
		largeBlob,
		uint64(time.Now().UnixMilli()),
	)
	if err != nil {
		t.Fatalf("BuildBstoreEntryForData: %v", err)
	}
	if chirpURL == "" || entry == nil || len(hashes) == 0 {
		t.Fatalf("unexpected empty bstore entry outputs: url=%q entry=%+v hashes=%d", chirpURL, entry, len(hashes))
	}
	decEntry, err := storerec.DecodeEntry(entryCBOR)
	if err != nil || decEntry.Size != uint64(len(largeBlob)) {
		t.Fatalf("storerec.DecodeEntry failed: entry=%+v err=%v", decEntry, err)
	}

	// 2. RCA-1 Regression: PrepareDirectSale MUST enforce unified fail-closed privacy & resale gates!
	buyerPub := sellerEng.Signer().PubHex()

	// (a) Undistilled fragment in a shard whose SellListing has AllowRaw=false -> MUST fail closed!
	undist, err := sellerMem.Remember(ctx, memory.RememberInput{
		Shard:      "strict-shard",
		Text:       "Raw undistilled internal architecture note.",
		Distilled:  false,
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sellerMem.DB().SaveSellListing(store.SellListing{
		Shard:     "strict-shard",
		Domains:   []string{"ops"},
		PriceSats: 50,
		AllowRaw:  false,
		Active:    true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := sellerEng.PrepareDirectSale(ctx, "strict-shard", undist.Seq, 50, buyerPub); err == nil {
		t.Fatal("RCA-1: expected PrepareDirectSale to reject undistilled fragment when AllowRaw=false")
	}

	// (b) Secret/PII-bearing fragment -> MUST fail closed even when AllowRaw=true!
	secretFrag, err := sellerMem.Remember(ctx, memory.RememberInput{
		Shard:      "raw-shard",
		Text:       "Production credential " + "-----BEGIN " + "PRIVATE KEY-----",
		Distilled:  true,
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := sellerEng.PrepareDirectSale(ctx, "raw-shard", secretFrag.Seq, 50, buyerPub); err == nil {
		t.Fatal("RCA-1: expected PrepareDirectSale to block secret-bearing fragment via unified sell gate")
	}

	// (c) Tainted inference_only purchased fragment -> MUST fail closed against unauthorized resale!
	taintedFrag, err := sellerMem.Remember(ctx, memory.RememberInput{
		Shard:      "purchased-shard",
		Text:       "Clean third-party market insight licensed strictly for inference.",
		Distilled:  true,
		Tainted:    true,
		NMU:        &nmu.Spec{Version: nmu.SpecVersion, Grade: nmu.GradeB, UsageRights: nmu.RightsInferenceOnly},
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := sellerEng.PrepareDirectSale(ctx, "purchased-shard", taintedFrag.Seq, 50, buyerPub); err == nil {
		t.Fatal("RCA-1: expected PrepareDirectSale to block resale of tainted inference_only fragment")
	}
}
