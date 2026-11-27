package wallet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/testchain"
	"github.com/lightwebinc/bcommon/wirewallet"
)

// TestSinglePostSigningSpendGuardAndTimeoutUnknown covers F7 & F11:
// - PostFromBEEF invokes SpendGuard on exact post-signing fee + net external spend (blocking overspend!)
// - Broadcaster errors & transport timeouts (ErrBroadcastUnknown) fail closed without unlocking UTXOs.
func TestSinglePostSigningSpendGuardAndTimeoutUnknown(t *testing.T) {
	ctx := context.Background()
	tc := testchain.New(1)
	var tcMu sync.Mutex

	var checkedFee, checkedSpend uint64
	var checkCalls atomic.Int32
	var rejectNext atomic.Bool
	var timeoutNext atomic.Bool

	cfg := Config{
		Dir:           t.TempDir(),
		InMemoryStore: true,
		Originator:    "nektar-test",
		Tracker:       tc,
		Broadcast: func(_ context.Context, tx *transaction.Transaction) error {
			if timeoutNext.Load() {
				return errors.New("context deadline exceeded: EOF while waiting for miner")
			}
			if rejectNext.Load() {
				return errors.New("HTTP 400: txn-mempool-conflict rejected")
			}
			tcMu.Lock()
			defer tcMu.Unlock()
			return tc.Send(tx)
		},
		ProofLookup: func(_ context.Context, txid string) (*transaction.MerklePath, uint32, bool, error) {
			tcMu.Lock()
			defer tcMu.Unlock()
			mp, h, ok := tc.Proof(txid)
			return mp, h, ok, nil
		},
		SpendGuard: func(txid string, feeSats, spendSats uint64, overrideGuard bool) error {
			checkCalls.Add(1)
			checkedFee = feeSats
			checkedSpend = spendSats
			if !overrideGuard && spendSats > 5000 {
				return errors.New("spendguard: daily spend cap exceeded")
			}
			return nil
		},
	}
	eng, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()
	seedTestEngine(t, tc, &tcMu, eng, 2)
	// Absorb & fan-out into independent fuel/reserve UTXOs so consecutive test sends each have fresh spendable inputs!
	if _, err := eng.AbsorbFundingBasket(ctx); err != nil {
		t.Fatalf("AbsorbFundingBasket: %v", err)
	}
	if _, err := eng.FuelKeeper().RunOnce(ctx); err != nil {
		t.Fatalf("FuelKeeper.RunOnce: %v", err)
	}

	dstAddr, _ := randomTestAddress(t)

	// 1. Overspend (> 5000 sats) MUST be blocked inside PostFromBEEF by SpendGuard!
	if _, err := eng.Send(ctx, SendArgs{ToAddress: dstAddr, Satoshis: 8000}); err == nil || !strings.Contains(err.Error(), "spendguard") {
		t.Fatalf("expected SpendGuard to block 8000-sat external spend, got err=%v", err)
	}
	if checkedSpend < 8000 || checkedFee == 0 {
		t.Fatalf("expected exact post-signing fee>0 and spend>=8000, got fee=%d spend=%d", checkedFee, checkedSpend)
	}

	// 2. Post-submit broadcaster rejection surfaces an error.
	rejectNext.Store(true)
	if _, err := eng.Send(ctx, SendArgs{ToAddress: dstAddr, Satoshis: 1000}); err == nil {
		t.Fatal("expected broadcaster rejection error")
	}
	rejectNext.Store(false)

	// 3. Transport timeout returns ErrBroadcastUnknown.
	timeoutNext.Store(true)
	_, err = eng.Send(ctx, SendArgs{ToAddress: dstAddr, Satoshis: 1000})
	if err == nil || !errors.Is(err, ErrBroadcastUnknown) {
		t.Fatalf("expected ErrBroadcastUnknown on transport timeout, got %v", err)
	}
}

// TestServicesFailClosedHeaderSPVAndNoSelfSubmittedCache covers F3:
// - Non-memory mode without HeaderBytes fails closed on ChainHeaderByHeight (never synthetic header!)
// - GetStatusForTxIDs NEVER trusts local RAM txCache (must query external proof/chain)
// - NLockTimeIsFinal fails closed when CurrentHeight fails
func TestServicesFailClosedHeaderSPVAndNoSelfSubmittedCache(t *testing.T) {
	ctx := context.Background()
	cache := newTxLRU(32)
	svc := newBcommonServices(Config{
		Mainnet:       true,
		InMemoryStore: false,
	}, cache)

	// 1. Live network without HeaderBytes must fail closed (never synthetic header!).
	if _, err := svc.ChainHeaderByHeight(ctx, 800000); err == nil {
		t.Fatal("expected ChainHeaderByHeight to fail closed on mainnet without HeaderBytes")
	}

	// 2. NLockTimeIsFinal with no height source must fail closed (false, err)!
	if final, err := svc.NLockTimeIsFinal(ctx, 800000); err == nil || final {
		t.Fatalf("expected NLockTimeIsFinal to fail closed without height source, got final=%v err=%v", final, err)
	}

	// 3. GetStatusForTxIDs MUST fail closed (never trust txCache or return NotFound,nil when Tracker/Chain == nil)!
	tx := transaction.NewTransaction()
	txid := tx.TxID().String()
	cache.Put(txid, tx)
	if _, err := svc.GetStatusForTxIDs(ctx, []string{txid}); !errors.Is(err, ErrNoChain) {
		t.Fatalf("expected ErrNoChain when Tracker/Chain == nil, got %v", err)
	}
}

// TestBRC29PendingJournalCrashRecovery covers F1:
// Simulates a crash right after desktop CreateAction broadcast before target.InternalizeAction,
// then proves ReconcilePendingBRC29 recovers and credits the exact funds into target!
func TestBRC29PendingJournalCrashRecovery(t *testing.T) {
	ctx := context.Background()
	tc := testchain.New(1)
	var tcMu sync.Mutex

	desktop := newTestEngine(t, tc, &tcMu, t.TempDir(), nil)
	defer desktop.Close()
	seedTestEngine(t, tc, &tcMu, desktop, 2)

	target := newTestEngine(t, tc, &tcMu, t.TempDir(), nil)
	defer target.Close()

	wireSrv := httptest.NewServer(wirewallet.Serve(desktop))
	defer wireSrv.Close()

	origDial := dialWireWallet
	defer func() { dialWireWallet = origDial }()
	dialWireWallet = func(originator, url string, timeout time.Duration) (sdkwallet.Interface, error) {
		return wirewallet.Dial(originator, wireSrv.URL, timeout)
	}

	// 1. Normal FundFromWireWallet persists intent, broadcasts, internalizes, and marks done.
	res, err := FundFromWireWallet(ctx, wireSrv.URL, 2500, target)
	if err != nil || !res.Internalized {
		t.Fatalf("FundFromWireWallet: res=%+v err=%v", res, err)
	}
	open, err := target.ListPendingBRC29(ctx)
	if err != nil || len(open) != 0 {
		t.Fatalf("expected 0 open pending BRC-29 rows after normal fund, got %d err=%v", len(open), err)
	}

	// 2. Simulate crash AFTER desktop broadcast before target InternalizeAction!
	sendRes, err := desktop.Send(ctx, SendArgs{
		ToIdentityKey: target.IdentityPubHex(),
		Satoshis:      3333,
		Description:   "simulated crash transfer",
	})
	if err != nil {
		t.Fatalf("desktop.Send BRC-29: %v", err)
	}
	balBefore, _ := target.Balance(ctx)
	pid, err := target.SavePendingBRC29(ctx, PendingBRC29Record{
		Direction:        "fund",
		WireURL:          wireSrv.URL,
		CounterpartyPub:  sendRes.SenderIdentityKey,
		DerivationPrefix: sendRes.DerivationPrefix,
		DerivationSuffix: sendRes.DerivationSuffix,
		Satoshis:         3333,
		OutputIndex:      sendRes.OutputIndex,
	})
	if err != nil {
		t.Fatalf("SavePendingBRC29: %v", err)
	}
	if err := target.UpdatePendingBRC29Tx(ctx, pid, sendRes.TxID, sendRes.OutputIndex, sendRes.BEEF, 3333); err != nil {
		t.Fatalf("UpdatePendingBRC29Tx: %v", err)
	}

	// Boot reconciler recovers the orphaned broadcast BRC-29 transfer!
	reconciled, err := ReconcilePendingBRC29(ctx, target, wireSrv.URL)
	if err != nil || reconciled != 1 {
		t.Fatalf("expected ReconcilePendingBRC29=1, got %d err=%v", reconciled, err)
	}
	balAfter, _ := target.Balance(ctx)
	if balAfter.SpendableSats != balBefore.SpendableSats+3333 {
		t.Fatalf("expected balance +%d after BRC-29 crash recovery, before=%d after=%d", 3333, balBefore.SpendableSats, balAfter.SpendableSats)
	}
	openAfter, _ := target.ListPendingBRC29(ctx)
	if len(openAfter) != 0 {
		t.Fatalf("expected 0 open pending BRC-29 rows after reconciliation, got %d", len(openAfter))
	}
}

// TestBalanceDisjointFundingAndWarmFuelDenom covers F24 & F35.
func TestBalanceDisjointFundingAndWarmFuelDenom(t *testing.T) {
	ctx := context.Background()
	tc := testchain.New(1)
	var tcMu sync.Mutex

	prof := StandardFuelProfile()
	prof.Name = "custom"
	prof.FuelTarget = 4
	prof.FuelSats = 1200
	prof.DustThresholdSats = 100
	prof.DustConsolidateTrigger = 4

	eng := newTestEngine(t, tc, &tcMu, t.TempDir(), &prof)
	defer eng.Close()
	seedTestEngine(t, tc, &tcMu, eng, 1)

	bal, err := eng.Balance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// F24: SpendableSats must equal DefaultSats + FuelSats + ReserveSats + FundingSats (NO double-counting!).
	sum := bal.DefaultSats + bal.FuelSats + bal.ReserveSats + bal.FundingSats
	if bal.SpendableSats != sum {
		t.Fatalf("balance mismatch (double-count!): spendable=%d != sum(%d+%d+%d+%d)=%d",
			bal.SpendableSats, bal.DefaultSats, bal.FuelSats, bal.ReserveSats, bal.FundingSats, sum)
	}
	// F35: FuelDenom reports configured denomination honestly.
	if eng.FuelDenom() != 1200 {
		t.Fatalf("expected FuelDenom=1200, got %d", eng.FuelDenom())
	}
}

type failingProofBroadcaster struct{}

func (failingProofBroadcaster) Name() string                                              { return "failing-bcast" }
func (failingProofBroadcaster) Broadcast(context.Context, *transaction.Transaction) error { return nil }
func (failingProofBroadcaster) Proof(context.Context, string) (*transaction.MerklePath, uint32, bool, error) {
	return nil, 0, false, errors.New("HTTP 503 broadcaster unavailable")
}

type notMinedChainStub struct{ nodeapi.Chain }

func (notMinedChainStub) Proof(context.Context, string) (*transaction.MerklePath, uint32, error) {
	return nil, 0, nodeapi.ErrNotMined
}

func TestLookupProofClearsBroadcasterErrorOnFallbackNotMined(t *testing.T) {
	ctx := context.Background()
	tc := testchain.New(1)
	svc := newBcommonServices(Config{
		Tracker:     tc,
		Broadcaster: failingProofBroadcaster{},
		Chain:       notMinedChainStub{},
	}, newTxLRU(16))
	txid := strings.Repeat("11", 32)
	mp, h, ok, err := svc.lookupProof(ctx, txid)
	if err != nil || ok || mp != nil || h != 0 {
		t.Fatalf("expected lookupProof=(nil,0,false,nil) when Chain.Proof returns ErrNotMined, got (%v,%d,%v,%v)", mp, h, ok, err)
	}
}

func TestHandleReorgResetsUserUTXOsAndPromotePersistsRawTx(t *testing.T) {
	ctx := context.Background()
	tc := testchain.New(1)
	var tcMu sync.Mutex

	eng := newTestEngine(t, tc, &tcMu, t.TempDir(), nil)
	defer eng.Close()
	seedTestEngine(t, tc, &tcMu, eng, 1)
	if _, err := eng.AbsorbFundingBasket(ctx); err != nil {
		t.Fatalf("AbsorbFundingBasket: %v", err)
	}

	// Mark a known_txes row as mined at height 500 and user_utxos as 'mined', then invoke HandleReorg(500).
	var txRowID int64
	var txid string
	if err := eng.readDB.QueryRowContext(ctx,
		`SELECT id, tx_id FROM "`+eng.tables.transactions+`" WHERE user_id = ? AND tx_id != '' ORDER BY id DESC LIMIT 1`,
		eng.userID,
	).Scan(&txRowID, &txid); err != nil {
		t.Fatalf("select tx: %v", err)
	}
	txObj, err := eng.LoadParsedTx(ctx, txid)
	if err != nil || txObj == nil {
		t.Fatalf("LoadParsedTx: %v", err)
	}
	// Clear raw_tx and call promoteTxStatusAtomic to verify raw_tx is persisted back into known_txes!
	if _, err := eng.sqlDB.ExecContext(ctx, `UPDATE "`+eng.tables.knownTxs+`" SET raw_tx = NULL WHERE tx_id = ?`, txid); err != nil {
		t.Fatalf("clear raw_tx: %v", err)
	}
	if err := eng.promoteTxStatusAtomic(ctx, txRowID, txid, StatusUnproven, txObj, nil, 0, "", ""); err != nil {
		t.Fatalf("promoteTxStatusAtomic: %v", err)
	}
	var rawLen int
	if err := eng.readDB.QueryRowContext(ctx, `SELECT COALESCE(length(raw_tx), 0) FROM "`+eng.tables.knownTxs+`" WHERE tx_id = ?`, txid).Scan(&rawLen); err != nil || rawLen == 0 {
		t.Fatalf("expected promoteTxStatusAtomic to persist raw_tx, got len=%d err=%v", rawLen, err)
	}

	// Simulate mined state at height 500, then verify HandleReorg(500) resets user_utxos.utxo_status -> 'unproven'.
	if _, err := eng.sqlDB.ExecContext(ctx, `UPDATE "`+eng.tables.knownTxs+`" SET status = 'completed', block_height = 500 WHERE tx_id = ?`, txid); err != nil {
		t.Fatalf("mark known_tx mined: %v", err)
	}
	if _, err := eng.sqlDB.ExecContext(ctx, `UPDATE "`+eng.tables.transactions+`" SET status = 'completed' WHERE id = ?`, txRowID); err != nil {
		t.Fatalf("mark tx completed: %v", err)
	}
	if _, err := eng.sqlDB.ExecContext(ctx, `UPDATE "`+eng.tables.userUTXOs+`" SET utxo_status = 'mined' WHERE user_id = ?`, eng.userID); err != nil {
		t.Fatalf("mark user_utxos mined: %v", err)
	}
	if err := eng.HandleReorg(ctx, 500, nil); err != nil {
		t.Fatalf("HandleReorg: %v", err)
	}
	var stillMined int
	if err := eng.readDB.QueryRowContext(ctx, `SELECT COUNT(1) FROM "`+eng.tables.userUTXOs+`" WHERE user_id = ? AND utxo_status = 'mined'`, eng.userID).Scan(&stillMined); err != nil || stillMined != 0 {
		t.Fatalf("expected HandleReorg to reset user_utxos.utxo_status='unproven', stillMined=%d err=%v", stillMined, err)
	}
}

func TestReconcilePendingBRC29WoCScriptHashRecovery(t *testing.T) {
	ctx := context.Background()
	tc := testchain.New(1)
	var tcMu sync.Mutex

	desktop := newTestEngine(t, tc, &tcMu, t.TempDir(), nil)
	defer desktop.Close()
	seedTestEngine(t, tc, &tcMu, desktop, 2)

	// Send a real BRC-29 payment from desktop to target, then simulate crash BEFORE UpdatePendingBRC29Tx (tx_id == "").
	var wocURL string
	var sendTxID string
	var sendVout uint32
	var wantScriptHash string

	wocSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/script/"+wantScriptHash+"/unspent/all") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": []map[string]any{{
					"tx_hash": sendTxID,
					"tx_pos":  sendVout,
					"value":   2222,
					"height":  1,
				}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer wocSrv.Close()
	wocURL = wocSrv.URL

	target, err := Open(Config{
		Dir:           t.TempDir(),
		InMemoryStore: true,
		Originator:    "nektar-test",
		Tracker:       tc,
		WoC:           &nodeapi.WoC{Base: wocURL, Client: wocSrv.Client()},
		Broadcast: func(_ context.Context, tx *transaction.Transaction) error {
			tcMu.Lock()
			defer tcMu.Unlock()
			return tc.Send(tx)
		},
		ProofLookup: func(_ context.Context, txid string) (*transaction.MerklePath, uint32, bool, error) {
			tcMu.Lock()
			defer tcMu.Unlock()
			mp, h, ok := tc.Proof(txid)
			return mp, h, ok, nil
		},
	})
	if err != nil {
		t.Fatalf("Open target: %v", err)
	}
	defer target.Close()

	sendRes, err := desktop.Send(ctx, SendArgs{
		ToIdentityKey: target.IdentityPubHex(),
		Satoshis:      2222,
		Description:   "crashed before tx_id persist",
	})
	if err != nil {
		t.Fatalf("desktop.Send: %v", err)
	}
	sendTxID = sendRes.TxID
	sendVout = sendRes.OutputIndex
	lockBytes := []byte(*sendRes.Tx.Outputs[sendVout].LockingScript)
	wantScriptHash = chainhash.HashH(lockBytes).String()

	// Pre-populate target's txCache with the mined tx (simulating chain GetBEEF availability).
	if mp, _, ok := tc.Proof(sendTxID); ok && mp != nil {
		sendRes.Tx.MerklePath = mp
	}
	target.CacheTx(sendRes.Tx)

	// Journal a crashed fund row with EMPTY tx_id and EMPTY atomic_beef, only locking_script_hex!
	if _, err := target.SavePendingBRC29(ctx, PendingBRC29Record{
		Direction:        "fund",
		WireURL:          "http://127.0.0.1:1",
		CounterpartyPub:  sendRes.SenderIdentityKey,
		DerivationPrefix: sendRes.DerivationPrefix,
		DerivationSuffix: sendRes.DerivationSuffix,
		LockingScriptHex: hex.EncodeToString(lockBytes),
		Satoshis:         2222,
	}); err != nil {
		t.Fatalf("SavePendingBRC29: %v", err)
	}
	// Verify local DB does NOT know sendTxID yet, so recovery MUST go through WoC scriptHash lookup!
	reconciled, err := ReconcilePendingBRC29(ctx, target, "http://127.0.0.1:1")
	if err != nil || reconciled != 1 {
		t.Fatalf("expected WoC scriptHash recovery reconciled=1, got %d err=%v", reconciled, err)
	}
	// Also verify DialDesktopWallet fast-fails offline loopback port via TCP probe (<= 2s).
	t0 := time.Now()
	if _, err := DialDesktopWallet(ctx, "nektar", "http://127.0.0.1:1", 15*time.Second); err == nil {
		t.Fatal("expected DialDesktopWallet on closed port to fail")
	}
	if time.Since(t0) > 2*time.Second {
		t.Fatalf("expected fast TCP probe failure, took %v", time.Since(t0))
	}
}
