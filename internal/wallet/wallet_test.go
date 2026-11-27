package wallet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/testchain"
	"github.com/lightwebinc/bcommon/wirewallet"
)

// newTestEngine opens a fresh pure-Go wallet Engine seeded with mature coinbases on a thread-safe testchain.
func newTestEngine(t testing.TB, tc *testchain.Chain, tcMu *sync.Mutex, dir string, profile *FuelProfile) *Engine {
	t.Helper()
	if tc == nil {
		tc = testchain.New(1)
	}
	if tcMu == nil {
		tcMu = &sync.Mutex{}
	}
	cfg := Config{
		Dir:         dir,
		Mainnet:     false,
		Originator:  "nektar-test",
		Tracker:     tc,
		FuelProfile: profile,
		Broadcast: func(_ context.Context, tx *transaction.Transaction) error {
			tcMu.Lock()
			defer tcMu.Unlock()
			if err := tc.Send(tx); err != nil {
				return err
			}
			if mp, _, ok := tc.Proof(tx.TxID().String()); ok {
				tx.MerklePath = mp
			}
			return nil
		},
		ProofLookup: func(_ context.Context, txid string) (*transaction.MerklePath, uint32, bool, error) {
			tcMu.Lock()
			defer tcMu.Unlock()
			mp, h, ok := tc.Proof(txid)
			return mp, h, ok, nil
		},
	}
	eng, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open wallet engine: %v", err)
	}
	return eng
}

func seedTestEngine(t testing.TB, tc *testchain.Chain, tcMu *sync.Mutex, eng *Engine, matureBlocks int) uint64 {
	t.Helper()
	tcMu.Lock()
	defer tcMu.Unlock()
	addr := eng.FundAddress()
	lockHex := eng.FundScript().String()
	if _, err := tc.Generate(testchain.Maturity+matureBlocks, addr); err != nil {
		t.Fatalf("testchain.Generate: %v", err)
	}
	tip := tc.Height()
	var total uint64
	for _, txid := range tc.Txids() {
		mp, h, ok := tc.Proof(txid)
		if !ok || h+testchain.Maturity > tip {
			continue
		}
		cbTx := tc.Tx(txid)
		if cbTx == nil || len(cbTx.Outputs) == 0 {
			continue
		}
		out0 := cbTx.Outputs[0]
		if out0.LockingScript == nil || out0.LockingScript.String() != lockHex {
			continue
		}
		if err := eng.SeedCoinbaseOutput(cbTx, 0, mp, h); err != nil {
			t.Fatalf("SeedCoinbaseOutput: %v", err)
		}
		total += out0.Satoshis
	}
	if total == 0 {
		t.Fatal("seeded zero satoshis")
	}
	return total
}

func randomTestAddress(t testing.TB) (string, *script.Script) {
	t.Helper()
	k, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatalf("NewPrivateKey: %v", err)
	}
	addr, err := script.NewAddressFromPublicKey(k.PubKey(), false)
	if err != nil {
		t.Fatalf("NewAddressFromPublicKey: %v", err)
	}
	ls, err := p2pkh.Lock(addr)
	if err != nil {
		t.Fatalf("p2pkh.Lock: %v", err)
	}
	return addr.AddressString, ls
}

func TestBRC100ConformanceRoundTrip(t *testing.T) {
	ctx := context.Background()
	tc := testchain.New(1)
	var tcMu sync.Mutex

	alice := newTestEngine(t, tc, &tcMu, t.TempDir(), nil)
	defer alice.Close()
	bob := newTestEngine(t, tc, &tcMu, t.TempDir(), nil)
	defer bob.Close()

	seedTestEngine(t, tc, &tcMu, alice, 3)

	// 1. Metadata & auth checks (BRC-100 GetNetwork must return "testnet" / "mainnet").
	netRes, err := alice.GetNetwork(ctx, nil, "test")
	if err != nil || netRes.Network != sdkwallet.NetworkTestnet {
		t.Fatalf("GetNetwork = %+v, err=%v; want testnet", netRes, err)
	}
	authRes, err := alice.IsAuthenticated(ctx, nil, "test")
	if err != nil || !authRes.Authenticated {
		t.Fatalf("IsAuthenticated = %+v, err=%v", authRes, err)
	}
	hRes, err := alice.GetHeight(ctx, nil, "test")
	if err != nil || hRes.Height < testchain.Maturity {
		t.Fatalf("GetHeight = %+v, err=%v", hRes, err)
	}
	vRes, err := alice.GetVersion(ctx, nil, "test")
	if err != nil || vRes.Version == "" {
		t.Fatalf("GetVersion = %+v, err=%v", vRes, err)
	}

	// 2. Crypto round-trip: GetPublicKey, Encrypt/Decrypt, CreateSignature/VerifySignature.
	proto := sdkwallet.Protocol{SecurityLevel: sdkwallet.SecurityLevelEveryApp, Protocol: "nektar test"}
	cpSelf := sdkwallet.Counterparty{Type: sdkwallet.CounterpartyTypeSelf}
	encRes, err := alice.Encrypt(ctx, sdkwallet.EncryptArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{ProtocolID: proto, KeyID: "k1", Counterparty: cpSelf},
		Plaintext:      []byte("hello verifiable memory"),
	}, "test")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	decRes, err := alice.Decrypt(ctx, sdkwallet.DecryptArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{ProtocolID: proto, KeyID: "k1", Counterparty: cpSelf},
		Ciphertext:     encRes.Ciphertext,
	}, "test")
	if err != nil || !bytes.Equal(decRes.Plaintext, []byte("hello verifiable memory")) {
		t.Fatalf("Decrypt mismatch: %q, err=%v", decRes.Plaintext, err)
	}
	sigRes, err := alice.CreateSignature(ctx, sdkwallet.CreateSignatureArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{ProtocolID: proto, KeyID: "k1", Counterparty: cpSelf},
		Data:           []byte("sign me"),
	}, "test")
	if err != nil {
		t.Fatalf("CreateSignature: %v", err)
	}
	forSelf := true
	verRes, err := alice.VerifySignature(ctx, sdkwallet.VerifySignatureArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{ProtocolID: proto, KeyID: "k1", Counterparty: cpSelf},
		ForSelf:        &forSelf,
		Data:           []byte("sign me"),
		Signature:      sigRes.Signature,
	}, "test")
	if err != nil || !verRes.Valid {
		t.Fatalf("VerifySignature: %+v, err=%v", verRes, err)
	}

	// 3. CreateAction with custom basket output + ListOutputs (locking scripts + entire transactions) + RelinquishOutput.
	_, customLock := randomTestAddress(t)
	actRes, err := alice.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: "create custom basket token",
		Labels:      []string{"conformance", "token"},
		Outputs: []sdkwallet.CreateActionOutput{{
			LockingScript:      *customLock,
			Satoshis:           500,
			Basket:             "tokens",
			Tags:               []string{"tag-a", "tag-b"},
			CustomInstructions: `{"kind":"note"}`,
			OutputDescription:  "custom token",
		}},
	}, "test")
	if err != nil || len(actRes.Tx) == 0 {
		t.Fatalf("CreateAction: %v", err)
	}
	incTags, incCI, incLabels := true, true, true
	listRes, err := alice.ListOutputs(ctx, sdkwallet.ListOutputsArgs{
		Basket:                    "tokens",
		Tags:                      []string{"tag-a"},
		TagQueryMode:              sdkwallet.QueryModeAll,
		Include:                   sdkwallet.OutputIncludeEntireTransactions,
		IncludeTags:               &incTags,
		IncludeCustomInstructions: &incCI,
		IncludeLabels:             &incLabels,
	}, "test")
	if err != nil || listRes.TotalOutputs != 1 || len(listRes.Outputs) != 1 || len(listRes.BEEF) == 0 {
		t.Fatalf("ListOutputs(tokens) = %+v, err=%v", listRes, err)
	}
	relRes, err := alice.RelinquishOutput(ctx, sdkwallet.RelinquishOutputArgs{
		Basket: "tokens",
		Output: listRes.Outputs[0].Outpoint,
	}, "test")
	if err != nil || !relRes.Relinquished {
		t.Fatalf("RelinquishOutput = %+v, err=%v", relRes, err)
	}
	listAfter, _ := alice.ListOutputs(ctx, sdkwallet.ListOutputsArgs{Basket: "tokens"}, "test")
	if listAfter.TotalOutputs != 0 {
		t.Fatalf("expected 0 outputs in tokens basket after relinquish, got %d", listAfter.TotalOutputs)
	}

	// 4. Two-phase CreateAction (SignAndProcess=false) -> SignAction AND AbortAction.
	signAndProc := false
	_, destLock := randomTestAddress(t)
	pendingRes, err := alice.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: "two-phase abort test",
		Outputs:     []sdkwallet.CreateActionOutput{{LockingScript: *destLock, Satoshis: 1200}},
		Options:     &sdkwallet.CreateActionOptions{SignAndProcess: &signAndProc},
	}, "test")
	if err != nil || pendingRes.SignableTransaction == nil {
		t.Fatalf("CreateAction two-phase: %+v, err=%v", pendingRes, err)
	}
	abortRes, err := alice.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: pendingRes.SignableTransaction.Reference}, "test")
	if err != nil || !abortRes.Aborted {
		t.Fatalf("AbortAction: %+v, err=%v", abortRes, err)
	}

	pendingRes2, err := alice.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: "two-phase sign test",
		Labels:      []string{"two-phase"},
		Outputs:     []sdkwallet.CreateActionOutput{{LockingScript: *destLock, Satoshis: 1200}},
		Options:     &sdkwallet.CreateActionOptions{SignAndProcess: &signAndProc},
	}, "test")
	if err != nil || pendingRes2.SignableTransaction == nil {
		t.Fatalf("CreateAction two-phase 2: %v", err)
	}
	signedRes, err := alice.SignAction(ctx, sdkwallet.SignActionArgs{
		Reference: pendingRes2.SignableTransaction.Reference,
		Spends:    map[uint32]sdkwallet.SignActionSpend{},
	}, "test")
	if err != nil || len(signedRes.Tx) == 0 {
		t.Fatalf("SignAction: %v", err)
	}

	// 5. BRC-29 Send from Alice to Bob's IdentityKey -> Bob InternalizeAction -> Bob spends that BRC-29 UTXO!
	sendRes, err := alice.Send(ctx, SendArgs{
		ToIdentityKey: bob.IdentityPubHex(),
		Satoshis:      5000,
		Description:   "BRC-29 payment Alice -> Bob",
	})
	if err != nil {
		t.Fatalf("Alice Send BRC-29: %v", err)
	}
	preRaw, _ := base64.StdEncoding.DecodeString(sendRes.DerivationPrefix)
	sufRaw, _ := base64.StdEncoding.DecodeString(sendRes.DerivationSuffix)
	intRes, err := bob.InternalizeAction(ctx, sdkwallet.InternalizeActionArgs{
		Tx:          sendRes.BEEF,
		Description: "receive BRC-29 from Alice",
		Labels:      []string{"incoming-brc29"},
		Outputs: []sdkwallet.InternalizeOutput{{
			OutputIndex: sendRes.OutputIndex,
			Protocol:    sdkwallet.InternalizeProtocolWalletPayment,
			PaymentRemittance: &sdkwallet.Payment{
				DerivationPrefix:  preRaw,
				DerivationSuffix:  sufRaw,
				SenderIdentityKey: alice.IdentityKey(),
			},
		}},
	}, "test")
	if err != nil || !intRes.Accepted {
		t.Fatalf("Bob InternalizeAction: %+v, err=%v", intRes, err)
	}
	bobBal, _ := bob.Balance(ctx)
	if bobBal.SpendableSats != 5000 {
		t.Fatalf("Bob balance = %d, want 5000", bobBal.SpendableSats)
	}
	// Bob immediately spends his BRC-29 derived UTXO!
	charlieAddr, _ := randomTestAddress(t)
	bobSpend, err := bob.Send(ctx, SendArgs{ToAddress: charlieAddr, Satoshis: 3000})
	if err != nil || bobSpend.TxID == "" {
		t.Fatalf("Bob spend BRC-29 UTXO failed: %v", err)
	}

	// 6. ListActions check (with labels, inputs, outputs).
	incIn, incOut := true, true
	laRes, err := alice.ListActions(ctx, sdkwallet.ListActionsArgs{
		Labels:         []string{"two-phase"},
		IncludeLabels:  &incLabels,
		IncludeInputs:  &incIn,
		IncludeOutputs: &incOut,
	}, "test")
	if err != nil || laRes.TotalActions != 1 || len(laRes.Actions[0].Inputs) == 0 {
		t.Fatalf("ListActions(two-phase) = %+v, err=%v", laRes, err)
	}
}

func TestWalletConcurrentLoad(t *testing.T) {
	ctx := context.Background()
	tc := testchain.New(1)
	var tcMu sync.Mutex

	prof := ThroughputFuelProfile()
	prof.FuelTarget = 96
	prof.FuelSats = 1000
	prof.LowWaterPct = 35
	prof.MaxFanOutOutputs = 128

	eng := newTestEngine(t, tc, &tcMu, t.TempDir(), &prof)
	defer eng.Close()

	initialSats := seedTestEngine(t, tc, &tcMu, eng, 6)

	// Pre-warm fuel pool cleanly (just like `nektar wallet fuel` or background FuelKeeper on boot).
	minted, err := eng.FuelKeeper().WarmFuel(ctx, 96, 1000)
	if err != nil || minted < 64 {
		t.Fatalf("WarmFuel minted=%d, err=%v", minted, err)
	}
	balAfterWarm, _ := eng.Balance(ctx)
	// Account for fan-out fee when verifying exact satoshi conservation.
	var warmFees uint64
	txsBefore, _, _ := eng.ListRecentActions(ctx, 500, 0)
	for _, r := range txsBefore {
		if r.IsOutgoing && (r.Status == StatusCompleted || r.Status == StatusUnproven) {
			warmFees += r.FeeSats
		}
	}
	if balAfterWarm.TotalSats+warmFees != initialSats {
		t.Fatalf("conservation mismatch after WarmFuel: total(%d) + warmFees(%d) != initial(%d)",
			balAfterWarm.TotalSats, warmFees, initialSats)
	}

	// Start background FuelKeeper during the concurrent load burst!
	bgCtx, bgCancel := context.WithCancel(ctx)
	eng.StartBackground(bgCtx)

	const workers = 64
	const txsPerWorker = 8 // 512 simultaneous transactions across 64 goroutines!
	const sendEach = 200

	destAddr, _ := randomTestAddress(t)
	var (
		wg        sync.WaitGroup
		succeeded atomic.Int64
		totalSent atomic.Uint64
		errMu     sync.Mutex
		errs      []error
	)
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < txsPerWorker; i++ {
				var lastErr error
				for retry := 0; retry < 4; retry++ {
					res, err := eng.Send(ctx, SendArgs{
						ToAddress:   destAddr,
						Satoshis:    sendEach,
						Description: "concurrent load tx",
					})
					if err == nil {
						succeeded.Add(1)
						totalSent.Add(res.Satoshis)
						lastErr = nil
						break
					}
					lastErr = err
					time.Sleep(10 * time.Millisecond)
				}
				if lastErr != nil {
					errMu.Lock()
					errs = append(errs, lastErr)
					errMu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	bgCancel()
	_ = eng.Close()

	// Reopen DB read-only/cleanly to inspect final durable SQLite state!
	eng2 := newTestEngine(t, tc, &tcMu, eng.cfg.Dir, &prof)
	defer eng2.Close()

	if len(errs) > 0 {
		t.Fatalf("concurrent load had %d/%d errors (first: %v)", len(errs), workers*txsPerWorker, errs[0])
	}
	if succeeded.Load() != workers*txsPerWorker {
		t.Fatalf("succeeded=%d, want %d", succeeded.Load(), workers*txsPerWorker)
	}

	// Verify exact satoshi conservation across ALL outgoing transactions (including background FuelKeeper fan-outs!):
	// initialSats == remainingTotalSats + totalExternalSent + totalMinerFees
	finalBal, err := eng2.Balance(ctx)
	if err != nil {
		t.Fatalf("final Balance: %v", err)
	}
	allTxs, _, err := eng2.ListRecentActions(ctx, 5000, 0)
	if err != nil {
		t.Fatalf("ListRecentTxs: %v", err)
	}
	var allFees uint64
	for _, r := range allTxs {
		// Only committed (completed/unproven) txs have deducted fees from TotalSats;
		// unsigned/unprocessed/sending/aborted txs retain full input value in ReservedSats.
		if r.IsOutgoing && (r.Status == StatusCompleted || r.Status == StatusUnproven) {
			allFees += r.FeeSats
		}
	}
	if finalBal.TotalSats+totalSent.Load()+allFees != initialSats {
		t.Fatalf("EXACT CONSERVATION VIOLATED: remaining(%d) + sent(%d) + fees(%d) = %d != initial(%d)",
			finalBal.TotalSats, totalSent.Load(), allFees, finalBal.TotalSats+totalSent.Load()+allFees, initialSats)
	}
	if finalBal.MaxUnprovenDepth > eng2.cfg.MaxUnprovenDepth {
		t.Fatalf("MaxUnprovenDepth %d exceeded cap %d", finalBal.MaxUnprovenDepth, eng2.cfg.MaxUnprovenDepth)
	}
	tps := float64(succeeded.Load()) / elapsed.Seconds()
	t.Logf("TestWalletConcurrentLoad: %d txs across %d goroutines in %s (%.1f TPS), total fees=%d sats (avg %.1f sat/tx), conservation exact!",
		succeeded.Load(), workers, elapsed.Round(time.Millisecond), tps, allFees, float64(allFees)/float64(succeeded.Load()))
}

func TestMultiInputConsolidateSweepAndDesktopBridge(t *testing.T) {
	ctx := context.Background()
	tc := testchain.New(1)
	var tcMu sync.Mutex

	// Simulate BSV Desktop wallet exposed over standard BRC-100 binary wire (wirewallet.Serve)!
	desktop := newTestEngine(t, tc, &tcMu, t.TempDir(), nil)
	defer desktop.Close()
	seedTestEngine(t, tc, &tcMu, desktop, 2)

	desktopSrv := httptest.NewServer(wirewallet.Serve(desktop))
	defer desktopSrv.Close()

	// Brand-new empty Nektar embedded wallet configured with 500-sat fuel denomination!
	nekProf := StandardFuelProfile()
	nekProf.FuelSats = 500
	nektar := newTestEngine(t, tc, &tcMu, t.TempDir(), &nekProf)
	defer nektar.Close()

	// 1. 1-click BRC-29 funding from BSV Desktop into Nektar!
	fundRes, err := FundFromWireWallet(ctx, desktopSrv.URL, 25_000, nektar)
	if err != nil || !fundRes.Internalized {
		t.Fatalf("FundFromWireWallet: %+v, err=%v", fundRes, err)
	}
	bal, _ := nektar.Balance(ctx)
	if bal.SpendableSats != 25_000 {
		t.Fatalf("nektar balance after desktop fund = %d, want 25000", bal.SpendableSats)
	}

	// 2. Fragment Nektar into 20 small fuel UTXOs of 500 sats + 1 BRC-29 deposit of 3000 sats (mixed key kinds!).
	if _, err := nektar.FuelKeeper().WarmFuel(ctx, 20, 500); err != nil {
		t.Fatalf("WarmFuel(20 x 500): %v", err)
	}
	if _, err := FundFromWireWallet(ctx, desktopSrv.URL, 3_000, nektar); err != nil {
		t.Fatalf("second desktop fund: %v", err)
	}

	// 3. Multi-input send requiring combining several UTXOs!
	extAddr, _ := randomTestAddress(t)
	multiSend, err := nektar.Send(ctx, SendArgs{ToAddress: extAddr, Satoshis: 18_500})
	if err != nil {
		t.Fatalf("multi-input Send(18500): %v", err)
	}
	if multiSend.InputCount < 2 {
		t.Fatalf("expected multi-input spend (>=2 inputs), got %d", multiSend.InputCount)
	}

	// 4. Consolidate remaining small UTXOs (< 1000 sats).
	consRes, err := nektar.Consolidate(ctx, ConsolidateArgs{BelowSats: 1000, MaxInputs: 50, TargetBasket: BasketReserve})
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if consRes.MergedCount < 2 {
		t.Fatalf("expected >=2 UTXOs consolidated, got %+v", consRes)
	}

	// 5. Add one more BRC-29 output so Sweep proves it signs MIXED fund + brc29 inputs in one sweep tx back to BSV Desktop!
	if _, err := FundFromWireWallet(ctx, desktopSrv.URL, 2_500, nektar); err != nil {
		t.Fatalf("third desktop fund: %v", err)
	}
	desktopBalBefore, _ := desktop.Balance(ctx)
	nektarBalBefore, _ := nektar.Balance(ctx)

	sweepRes, err := SweepToWireWallet(ctx, desktopSrv.URL, nektar, true)
	if err != nil {
		t.Fatalf("SweepToWireWallet: %v", err)
	}
	if sweepRes.Internalized != len(sweepRes.TxIDs) || sweepRes.SweptSats == 0 {
		t.Fatalf("unexpected SweepToWireWallet result: %+v", sweepRes)
	}
	if sweepRes.SweptSats+sweepRes.TotalFeeSats != nektarBalBefore.SpendableSats {
		t.Fatalf("sweep accounting mismatch: swept(%d) + fee(%d) != before(%d)",
			sweepRes.SweptSats, sweepRes.TotalFeeSats, nektarBalBefore.SpendableSats)
	}

	nektarBalAfter, _ := nektar.Balance(ctx)
	if nektarBalAfter.SpendableSats != 0 || nektarBalAfter.SpendableCount != 0 {
		t.Fatalf("expected 0 spendable sats after full sweep, got %+v", nektarBalAfter)
	}
	desktopBalAfter, _ := desktop.Balance(ctx)
	if desktopBalAfter.SpendableSats != desktopBalBefore.SpendableSats+sweepRes.SweptSats {
		t.Fatalf("desktop balance after sweep = %d, want %d", desktopBalAfter.SpendableSats, desktopBalBefore.SpendableSats+sweepRes.SweptSats)
	}
}

func TestCrashRecoveryAbortAbandoned(t *testing.T) {
	ctx := context.Background()
	tc := testchain.New(1)
	var tcMu sync.Mutex
	dir := t.TempDir()

	openWithTTL := func() *Engine {
		eng, err := Open(Config{
			Dir:            dir,
			Mainnet:        false,
			Originator:     "nektar-test",
			Tracker:        tc,
			ReservationTTL: 1 * time.Second,
			Broadcast: func(_ context.Context, tx *transaction.Transaction) error {
				tcMu.Lock()
				defer tcMu.Unlock()
				if err := tc.Send(tx); err != nil {
					return err
				}
				if mp, _, ok := tc.Proof(tx.TxID().String()); ok {
					tx.MerklePath = mp
				}
				return nil
			},
			ProofLookup: func(_ context.Context, txid string) (*transaction.MerklePath, uint32, bool, error) {
				tcMu.Lock()
				defer tcMu.Unlock()
				mp, h, ok := tc.Proof(txid)
				return mp, h, ok, nil
			},
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return eng
	}

	eng := openWithTTL()
	defer eng.Close()
	seedTestEngine(t, tc, &tcMu, eng, 2)
	if _, err := eng.AbsorbFundingBasket(ctx); err != nil {
		t.Fatalf("AbsorbFundingBasket: %v", err)
	}
	balInitial, _ := eng.Balance(ctx)
	initial := balInitial.SpendableSats

	// Start a two-phase unsigned action and simulate process crash (Close without SignAction/AbortAction).
	signAndProc := false
	_, lock := randomTestAddress(t)
	res, err := eng.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: "crashed unsigned tx",
		Outputs:     []sdkwallet.CreateActionOutput{{LockingScript: *lock, Satoshis: 1000}},
		Options:     &sdkwallet.CreateActionOptions{SignAndProcess: &signAndProc},
	}, "test")
	if err != nil || res.SignableTransaction == nil {
		t.Fatalf("two-phase CreateAction: %v", err)
	}
	balMid, _ := eng.Balance(ctx)
	if balMid.ReservedSats == 0 {
		t.Fatalf("expected reserved sats > 0 during pending unsigned action, got %+v", balMid)
	}
	_ = eng.Close()

	// Wait past ReservationTTL, reopen SQLite, and run Monitor.AbortAbandoned!
	time.Sleep(1500 * time.Millisecond)
	eng2 := openWithTTL()
	defer eng2.Close()

	aborted, err := eng2.Monitor().AbortAbandoned(ctx)
	if err != nil || aborted < 1 {
		t.Fatalf("AbortAbandoned = %d, err=%v; want >= 1", aborted, err)
	}
	balAfter, _ := eng2.Balance(ctx)
	if balAfter.ReservedSats != 0 || balAfter.SpendableSats != initial {
		t.Fatalf("after AbortAbandoned: got %+v, want ReservedSats=0 SpendableSats=%d", balAfter, initial)
	}
}

func TestMigrateLegacyWalletJSON(t *testing.T) {
	ctx := context.Background()
	tc := testchain.New(1)
	var tcMu sync.Mutex
	dir := t.TempDir()

	prof := bwallet.Profile{
		FundProtocol:   sdkwallet.Protocol{SecurityLevel: sdkwallet.SecurityLevelEveryAppAndCounterparty, Protocol: "nektar fund"},
		FundKeyID:      "nektar-fund",
		FundBasket:     "nektar funding",
		Version:        "0.1.0",
		LegacyPoolFile: "nektar-pool.json",
	}
	// Create an old bcommon/bwallet embedded wallet & seed one mature coinbase in wallet.json!
	oldEmb, err := bwallet.Create(dir, prof)
	if err != nil {
		t.Fatalf("bwallet.Create: %v", err)
	}
	oldIDHex := hex.EncodeToString(oldEmb.IdentityKey().Compressed())
	oldAddr, err := oldEmb.FundAddress(false)
	if err != nil {
		t.Fatalf("old FundAddress: %v", err)
	}
	oldLock, _ := oldEmb.FundScript()
	if _, err := tc.Generate(testchain.Maturity+1, oldAddr); err != nil {
		t.Fatalf("tc.Generate: %v", err)
	}
	var seeded uint64
	for _, txid := range tc.Txids() {
		mp, h, ok := tc.Proof(txid)
		if !ok || h+testchain.Maturity > tc.Height() {
			continue
		}
		cbTx := tc.Tx(txid)
		if _, err := oldEmb.Pool.Add(bwallet.Output{
			TxID:          txid,
			Vout:          0,
			Satoshis:      cbTx.Outputs[0].Satoshis,
			LockingScript: oldLock.String(),
			Height:        h,
			Coinbase:      true,
			Raw:           cbTx.Hex(),
			Bump:          hex.EncodeToString(mp.Bytes()),
		}); err != nil {
			t.Fatalf("oldEmb.Pool.Add: %v", err)
		}
		seeded += cbTx.Outputs[0].Satoshis
	}

	// Open the new SQLite wallet.Engine on the exact same directory!
	eng := newTestEngine(t, tc, &tcMu, dir, nil)
	defer eng.Close()

	if eng.IdentityPubHex() != oldIDHex {
		t.Fatalf("identity key changed across migration: got %s, want %s", eng.IdentityPubHex(), oldIDHex)
	}
	if eng.FundAddress() != oldAddr {
		t.Fatalf("fund address changed across migration: got %s, want %s", eng.FundAddress(), oldAddr)
	}
	if _, err := os.Stat(filepath.Join(dir, "wallet.json.migrated")); err != nil {
		t.Fatalf("expected wallet.json.migrated backup: %v", err)
	}
	bal, _ := eng.Balance(ctx)
	if bal.SpendableSats != seeded {
		t.Fatalf("migrated balance = %d, want %d", bal.SpendableSats, seeded)
	}
	// Prove migrated coin actually signs and spends on chain!
	destAddr, _ := randomTestAddress(t)
	if _, err := eng.Send(ctx, SendArgs{ToAddress: destAddr, Satoshis: 1000}); err != nil {
		t.Fatalf("spend migrated UTXO failed: %v", err)
	}
}

func BenchmarkWalletCreateActionParallel(b *testing.B) {
	ctx := context.Background()
	tc := testchain.New(1)
	var tcMu sync.Mutex

	prof := ThroughputFuelProfile()
	prof.FuelTarget = 256
	prof.FuelSats = 1000
	prof.MaxFanOutOutputs = 256

	eng := newTestEngine(b, tc, &tcMu, b.TempDir(), &prof)
	defer eng.Close()

	seedTestEngine(b, tc, &tcMu, eng, 10)
	if _, err := eng.FuelKeeper().WarmFuel(ctx, 256, 1000); err != nil {
		b.Fatalf("WarmFuel: %v", err)
	}
	_, destLock := randomTestAddress(b)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := eng.CreateAction(ctx, sdkwallet.CreateActionArgs{
				Description: "bench",
				Outputs:     []sdkwallet.CreateActionOutput{{LockingScript: *destLock, Satoshis: 100}},
			}, "bench")
			if err != nil {
				// Top up once if bench exhausted fuel pool during long -benchtime.
				_, _ = eng.FuelKeeper().WarmFuel(ctx, 128, 1000)
			}
		}
	})
}
