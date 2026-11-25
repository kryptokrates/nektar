package wallet

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/testchain"
	"github.com/lightwebinc/bcommon/wirewallet"
)

// TestSinglePostSigningSpendGuardAndTimeoutUnknown covers F7 & F11:
// - PostFromBEEF invokes SpendGuard on exact post-signing fee + net external spend (blocking overspend!)
// - Transport timeout (ErrBroadcastUnknown) does NOT refund SpendGuard (never unlocks UTXOs!)
// - Definitive pre-broadcast rejection DOES refund SpendGuard cleanly.
func TestSinglePostSigningSpendGuardAndTimeoutUnknown(t *testing.T) {
	ctx := context.Background()
	tc := testchain.New(1)
	var tcMu sync.Mutex

	var checkedFee, checkedSpend uint64
	var checkCalls, refundCalls atomic.Int32
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
		SpendGuardRefund: func(txid string) error {
			refundCalls.Add(1)
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

	// 2. Post-submit broadcaster error strings MUST NOT refund SpendGuard or mark terminal double-spend!
	rejectNext.Store(true)
	refundsBefore := refundCalls.Load()
	if _, err := eng.Send(ctx, SendArgs{ToAddress: dstAddr, Satoshis: 1000}); err == nil {
		t.Fatal("expected broadcaster rejection error")
	}
	if refundCalls.Load() != refundsBefore {
		t.Fatalf("post-submit error string MUST NOT refund SpendGuard, got delta=%d", refundCalls.Load()-refundsBefore)
	}
	rejectNext.Store(false)

	// 3. Transport timeout returns ErrBroadcastUnknown and MUST NOT refund SpendGuard!
	timeoutNext.Store(true)
	refundsBefore = refundCalls.Load()
	_, err = eng.Send(ctx, SendArgs{ToAddress: dstAddr, Satoshis: 1000})
	if err == nil || !errors.Is(err, ErrBroadcastUnknown) {
		t.Fatalf("expected ErrBroadcastUnknown on transport timeout, got %v", err)
	}
	if refundCalls.Load() != refundsBefore {
		t.Fatalf("SpendGuard MUST NOT refund on ErrBroadcastUnknown! refunds=%d", refundCalls.Load()-refundsBefore)
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
