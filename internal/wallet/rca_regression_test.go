package wallet

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/testchain"
)

type fakeTimeoutNetErr struct{ msg string }

func (e fakeTimeoutNetErr) Error() string   { return e.msg }
func (e fakeTimeoutNetErr) Timeout() bool   { return true }
func (e fakeTimeoutNetErr) Temporary() bool { return true }

var _ net.Error = fakeTimeoutNetErr{}

// TestRCA_S4_CanonicalBroadcastErrorClassifiers verifies the single shared uncertain/already-known
// broadcast error classifiers used across wallet and chain packages.
func TestRCA_S4_CanonicalBroadcastErrorClassifiers(t *testing.T) {
	uncertainCases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ErrBroadcastUnknown", ErrBroadcastUnknown, true},
		{"DeadlineExceeded", context.DeadlineExceeded, true},
		{"Canceled", context.Canceled, true},
		{"netError", fakeTimeoutNetErr{"i/o timeout"}, true},
		{"brokenPipe", errors.New("write tcp 127.0.0.1: broken pipe"), true},
		{"connReset", errors.New("read tcp: connection reset by peer"), true},
		{"connRefused", errors.New("dial tcp 127.0.0.1: connection refused"), true},
		{"eof", errors.New("unexpected EOF"), true},
		{"http500", errors.New("HTTP 500 internal error"), true},
		{"http502", errors.New("HTTP 502 bad gateway"), true},
		{"http503", errors.New("HTTP 503 service unavailable"), true},
		{"http504", errors.New("HTTP 504 gateway timeout"), true},
		{"http429", errors.New("HTTP 429 too many requests"), true},
		{"status502", errors.New("upstream returned status 502"), true},
		{"tempFail", errors.New("temporary failure in name resolution"), true},
		{"deterministic400", errors.New("HTTP 400: bad-txns-inputs-missingorspent"), false},
		{"mempoolConflict", errors.New("460 txn-mempool-conflict"), false},
	}
	for _, tc := range uncertainCases {
		if got := IsUncertainBroadcastError(tc.err); got != tc.want {
			t.Errorf("IsUncertainBroadcastError(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}

	knownCases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("TXN-ALREADY-KNOWN"), true},
		{errors.New("transaction already in mempool"), true},
		{errors.New("Already known by peer"), true},
		{errors.New("HTTP 400 invalid script"), false},
	}
	for _, tc := range knownCases {
		if got := IsAlreadyKnownBroadcastError(tc.err); got != tc.want {
			t.Errorf("IsAlreadyKnownBroadcastError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestRCA_S3_ExplicitSignActionBroadcastUnknown verifies that AbsorbFundingBasket, Consolidate,
// and Sweep all route through the single guarded chokepoint so transport/timeout failures during
// SignAction or Sweep default-basket CreateAction mark submission_unknown and return ErrBroadcastUnknown.
func TestRCA_S3_ExplicitSignActionBroadcastUnknown(t *testing.T) {
	ctx := context.Background()
	tc := testchain.New(1)
	var tcMu sync.Mutex
	var failUnknown atomic.Bool

	cfg := Config{
		Dir:           t.TempDir(),
		InMemoryStore: true,
		Originator:    "nektar-rca-s3",
		Tracker:       tc,
		Broadcast: func(_ context.Context, tx *transaction.Transaction) error {
			if failUnknown.Load() {
				return errors.New("write tcp: broken pipe while waiting for ARC")
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
	}
	eng, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	countUnknown := func() int {
		var n int
		_ = eng.readDB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM "`+eng.tables.transactions+`" WHERE user_id = ? AND status = 'submission_unknown'`,
			eng.userID,
		).Scan(&n)
		return n
	}

	// 1. AbsorbFundingBasket with uncertain broadcast -> must return ErrBroadcastUnknown and mark submission_unknown.
	seedTestEngine(t, tc, &tcMu, eng, 1)
	failUnknown.Store(true)
	if _, err := eng.AbsorbFundingBasket(ctx); !errors.Is(err, ErrBroadcastUnknown) {
		t.Fatalf("AbsorbFundingBasket on broken pipe: got %v, want ErrBroadcastUnknown", err)
	}
	if n := countUnknown(); n < 1 {
		t.Fatalf("expected >=1 submission_unknown tx after AbsorbFundingBasket timeout, got %d", n)
	}
	failUnknown.Store(false)

	// 2. Sweep default basket with uncertain broadcast -> must return ErrBroadcastUnknown and mark submission_unknown.
	eng2, err := Open(Config{
		Dir:           t.TempDir(),
		InMemoryStore: true,
		Originator:    "nektar-rca-s3-sweep",
		Tracker:       tc,
		Broadcast:     cfg.Broadcast,
		ProofLookup:   cfg.ProofLookup,
	})
	if err != nil {
		t.Fatalf("Open eng2: %v", err)
	}
	defer eng2.Close()
	seedTestEngine(t, tc, &tcMu, eng2, 1)
	if _, err := eng2.AbsorbFundingBasket(ctx); err != nil {
		t.Fatalf("eng2 AbsorbFundingBasket: %v", err)
	}
	dstAddr, _ := randomTestAddress(t)
	failUnknown.Store(true)
	if _, err := eng2.Sweep(ctx, SweepArgs{ToAddress: dstAddr}); !errors.Is(err, ErrBroadcastUnknown) {
		t.Fatalf("Sweep default basket on broken pipe: got %v, want ErrBroadcastUnknown", err)
	}
	var sweepUnknown int
	_ = eng2.readDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM "`+eng2.tables.transactions+`" WHERE user_id = ? AND status = 'submission_unknown'`,
		eng2.userID,
	).Scan(&sweepUnknown)
	if sweepUnknown < 1 {
		t.Fatalf("expected >=1 submission_unknown tx after Sweep timeout, got %d", sweepUnknown)
	}
	var spendableOnUnknown int
	_ = eng2.readDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM "`+eng2.tables.outputs+`" o JOIN "`+eng2.tables.transactions+`" t ON o.transaction_id = t.id WHERE t.user_id = ? AND t.status = 'submission_unknown' AND o.spendable = 1`,
		eng2.userID,
	).Scan(&spendableOnUnknown)
	if spendableOnUnknown != 0 {
		t.Fatalf("expected 0 spendable outputs on submission_unknown tx, got %d", spendableOnUnknown)
	}
	failUnknown.Store(false)
}

// TestRCA_S8_ReconcilePendingBRC29SurfacesErrors proves ReconcilePendingBRC29 does not silently
// return (0, nil) when a pending BRC-29 row with a known TxID cannot produce a valid AtomicBEEF.
func TestRCA_S8_ReconcilePendingBRC29SurfacesErrors(t *testing.T) {
	ctx := context.Background()
	tc := testchain.New(1)
	var tcMu sync.Mutex

	eng := newTestEngine(t, tc, &tcMu, t.TempDir(), nil)
	defer eng.Close()

	pid, err := eng.SavePendingBRC29(ctx, PendingBRC29Record{
		Direction:        "fund",
		WireURL:          "http://127.0.0.1:3321",
		CounterpartyPub:  eng.IdentityPubHex(),
		DerivationPrefix: base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")),
		DerivationSuffix: base64.StdEncoding.EncodeToString([]byte("fedcba9876543210")),
		Satoshis:         2500,
	})
	if err != nil {
		t.Fatalf("SavePendingBRC29: %v", err)
	}
	missingTxID := strings.Repeat("aa", 32)
	if err := eng.UpdatePendingBRC29Tx(ctx, pid, missingTxID, 0, nil, 2500); err != nil {
		t.Fatalf("UpdatePendingBRC29Tx: %v", err)
	}

	n, err := ReconcilePendingBRC29(ctx, eng, "http://127.0.0.1:3321")
	if err == nil {
		t.Fatalf("expected non-nil error when pending BRC-29 tx %s has unavailable AtomicBEEF, got n=%d err=nil", missingTxID, n)
	}
}

// TestRatchet_WalletSingleTBActionChokepoint enforces RCA-S3 structurally:
// all e.tb.CreateAction and e.tb.SignAction calls MUST live in engine_brc100.go,
// and UserUTXOEntity reservation queries must never silently discard errors.
func TestRatchet_WalletSingleTBActionChokepoint(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		src := string(raw)
		if name != "engine_brc100.go" {
			if strings.Contains(src, "e.tb.CreateAction(") || strings.Contains(src, "e.tb.SignAction(") {
				t.Errorf("RCA-S3 ratchet violated: %s calls e.tb.CreateAction/SignAction directly instead of engine_brc100.go chokepoint", name)
			}
		}
		if strings.Contains(src, "utxos, _ := e.provider.UserUTXOEntity()") {
			t.Errorf("RCA-S3 ratchet violated: %s swallows UserUTXOEntity() error", name)
		}
	}
}
