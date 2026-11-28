package hub

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
)

// TestHubRiskDBErrorsFailClosed verifies RCA-RT-3:
// DB errors in IsPayerFlagged / UnminedFastExposure refuse 0-conf fast acceptance (503),
// and non-constraint DB errors in ClaimFeePaymentAtomic map to 503 instead of 409 conflict/replay.
func TestHubRiskDBErrorsFailClosed(t *testing.T) {
	ctx := context.Background()
	shared := chain.NewSharedMemoryChain()
	hubPort, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir(), SharedMemoryChain: shared})
	if err != nil {
		t.Fatal(err)
	}
	defer hubPort.Close()
	payerPort, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir(), SharedMemoryChain: shared})
	if err != nil {
		t.Fatal(err)
	}
	defer payerPort.Close()

	node, err := NewNode(Config{
		Network:      "memory",
		Fees:         FeeSchedule{Role: RoleLeaf, SubmitWantSats: 5},
		FeeCollector: NewPortFeeCollector(hubPort),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	// Build a valid BRC-29 payment header for submit:want.
	prefix := node.IssuePrefix(ActionSubmitWant, 5, time.Now())
	suffix := "dGVzdHN1ZmZpeA=="
	wantScript, err := node.feeCollector.DeriveBRC29Script(ctx, payerPort.IdentityPubHex(), prefix, suffix)
	if err != nil {
		t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 5, LockingScript: wantScript})
	// Add a dummy input so guard.ParseBEEF accepts it as a funded tx structure.
	_ = tx
	// Use ClaimFeePaymentAtomic directly on a broken table to verify non-constraint errors don't map to ErrFeePaymentConflict/Replayed.
	if _, err := node.store.sql.Exec(`DROP TABLE hub_fee_claim_inputs`); err != nil {
		t.Fatal(err)
	}
	claimErr := node.store.ClaimFeePaymentAtomic("deadbeef01", []string{"in0:0"}, ActionSubmitWant, payerPort.IdentityPubHex(), 5, "claimed")
	if claimErr == nil {
		t.Fatal("expected ClaimFeePaymentAtomic to fail when hub_fee_claim_inputs is missing")
	}
	out := mapClaimErr(claimErr, "deadbeef01")
	if out.HTTPStatus != http.StatusServiceUnavailable || out.Code != "ERR_HUB_STORE_UNAVAILABLE" {
		t.Fatalf("non-constraint SQLite error MUST map to 503 ERR_HUB_STORE_UNAVAILABLE, got %+v (err=%v)", out, claimErr)
	}

	// Verify IsPayerFlagged DB error refuses 0-conf fast acceptance (503) instead of failing open!
	if _, err := node.store.sql.Exec(`DROP TABLE hub_flagged_payers`); err != nil {
		t.Fatal(err)
	}
	_, _, fErr := node.store.IsPayerFlagged(payerPort.IdentityPubHex())
	if fErr == nil {
		t.Fatal("expected IsPayerFlagged error when hub_flagged_payers table is dropped")
	}
	// Construct a valid AtomicBEEF payment paying wantScript using payerPort.
	builtTx, builtBEEF, _, _, err := payerPort.BuildFundedTxWithBasket(ctx, []*transaction.TransactionOutput{{
		Satoshis:      5,
		LockingScript: wantScript,
	}}, "test hub fee", nil, nil)
	if err != nil {
		t.Fatalf("BuildFundedTxWithBasket: %v", err)
	}
	if len(builtBEEF) == 0 {
		builtBEEF, err = chain.SafeAtomicBEEF(builtTx)
		if err != nil {
			t.Fatalf("SafeAtomicBEEF: %v", err)
		}
	}
	payJSON, _ := json.Marshal(BRC105PaymentHeader{
		TxID:              builtTx.TxID().String(),
		BEEFHex:           hex.EncodeToString(builtBEEF),
		OutputIndex:       0,
		DerivationPrefix:  prefix,
		DerivationSuffix:  suffix,
		SenderIdentityKey: payerPort.IdentityPubHex(),
	})
	verOut := node.verifyAndCollectFee(ctx, ActionSubmitWant, string(bstack.KindWant), 5, string(payJSON))
	if verOut.HTTPStatus != http.StatusServiceUnavailable || verOut.Code != "ERR_HUB_STORE_UNAVAILABLE" {
		t.Fatalf("expected 503 ERR_HUB_STORE_UNAVAILABLE when IsPayerFlagged fails, got %+v", verOut)
	}
}

// TestPlaneGapRepairDoesNotAdvanceCursorOnFailure verifies H1:
// When /plane/nack returns HTTP 503 during a sequence gap, consumePlaneOnce MUST NOT advance
// the persisted federation cursor, and MUST recover the missed frame once /plane/nack returns 200.
func TestPlaneGapRepairDoesNotAdvanceCursorOnFailure(t *testing.T) {
	ctx := context.Background()
	leaf, err := NewNode(Config{Network: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()

	key, _ := ec.NewPrivateKey()
	lane := bstack.LaneOf("memory", "ops")
	makeCarrier := func(id string) []byte {
		w := bstack.WantAd{
			WantID:   id,
			Pub:      hex.EncodeToString(key.PubKey().Compressed()),
			Net:      "memory",
			Domains:  []string{"ops"},
			Query:    "q " + id,
			MaxSats:  10,
			Revision: 1,
			Exp:      time.Now().Add(time.Hour).UnixMilli(),
		}
		sig, sErr := bstack.SignArtifact(key, w)
		if sErr != nil {
			t.Fatal(sErr)
		}
		pWant, pErr := bstack.NewWantPayload(bstack.SignedWant{Want: w, Sig: sig})
		if pErr != nil {
			t.Fatal(pErr)
		}
		cborBytes, err := bstack.EncodePayload(pWant)
		if err != nil {
			t.Fatal(err)
		}
		var opRet script.Script
		_ = opRet.AppendOpcodes(script.OpFALSE, script.OpRETURN)
		_ = opRet.AppendPushData(cborBytes)
		tx := transaction.NewTransaction()
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: 0, LockingScript: &opRet})
		beef, err := tx.AtomicBEEF(false)
		if err != nil {
			t.Fatal(err)
		}
		return beef
	}
	b1 := makeCarrier("gap-1")
	b2 := makeCarrier("gap-2")
	b3 := makeCarrier("gap-3")

	var nackFail atomic.Bool
	nackFail.Store(true)
	var sawSignedNack atomic.Bool

	spineSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/plane/stream":
			w.Header().Set("Content-Type", "application/vnd.nektar.brc149-stream")
			w.WriteHeader(http.StatusOK)
			// Emit seq 1, then jump to seq 3 (creating a gap at seq 2).
			f1, _ := EncodePlaneFrame(PlaneFrame{FlowSeq: 1, Topic: lane, BEEF: b1})
			f3, _ := EncodePlaneFrame(PlaneFrame{FlowSeq: 3, Topic: lane, BEEF: b3})
			_, _ = w.Write(f1)
			_, _ = w.Write(f3)
		case "/plane/nack":
			if r.Header.Get("X-Nektar-Pub") != "" && r.Header.Get("X-Nektar-Sig") != "" {
				sawSignedNack.Store(true)
			}
			if nackFail.Load() {
				http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.nektar.brc149-stream")
			w.WriteHeader(http.StatusOK)
			f2, _ := EncodePlaneFrame(PlaneFrame{FlowSeq: 2, Topic: lane, BEEF: b2})
			_, _ = w.Write(f2)
		default:
			http.NotFound(w, r)
		}
	}))
	defer spineSrv.Close()

	leaf.SetEgressAuth(func(action, aud string, cursor int64, domainsCSV string) (string, string, string, string, error) {
		return "02deadbeef", "1700000000", "nonce-1", "sig-1", nil
	}, nil)

	// Attempt 1: /plane/nack returns 503 -> cursor MUST remain at 1 (NOT 3)!
	next1 := leaf.consumePlaneOnce(ctx, spineSrv.URL, 0)
	if !sawSignedNack.Load() {
		t.Fatal("expected repairPlaneGap to send signed X-Nektar-* headers on /plane/nack")
	}
	if next1 != 1 {
		t.Fatalf("H1 REGRESSION: cursor advanced to %d despite /plane/nack 503 failure (want 1)", next1)
	}
	if persisted := leaf.store.GetFedCursor("plane:" + spineSrv.URL); persisted != 1 {
		t.Fatalf("H1 REGRESSION: persisted fed cursor = %d, want 1", persisted)
	}

	// Attempt 2: /plane/nack recovers (200) -> gap event 2 AND event 3 are admitted and cursor reaches 3!
	nackFail.Store(false)
	next2 := leaf.consumePlaneOnce(ctx, spineSrv.URL, next1)
	if next2 != 3 {
		t.Fatalf("expected cursor to advance to 3 after NACK recovery, got %d", next2)
	}
	evs, err := leaf.store.EventsSince(0, nil, 10)
	if err != nil || len(evs) != 3 {
		t.Fatalf("expected all 3 events admitted after gap repair, got %d err=%v", len(evs), err)
	}
}

// TestBountyBroadcastStateFailureSurfacesError verifies H2:
// If MarkBountyClaimBroadcast / FinalizeBountyClaim fails after payout, handleBountiesSubmit
// MUST NOT swallow the error and return 200 status="broadcast"/"paid".
func TestBountyBroadcastStateFailureSurfacesError(t *testing.T) {
	node, err := NewNode(Config{Network: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	if err := node.Store().UpsertBounty(Bounty{
		ID:         "b-h2",
		Domain:     "ops",
		Question:   "H2 test",
		RewardSats: 100,
		MaxFills:   5,
		Active:     true,
	}); err != nil {
		t.Fatal(err)
	}

	// Bounty handler simulates payout success AND injects a DB failure before FinalizeBountyClaim runs.
	node.SetBountyHandler(func(_ context.Context, b Bounty, sub BountySubmission) (*BountyPayoutResult, error) {
		// Install a trigger that rejects UPDATE on hub_bounty_claims to simulate a post-broadcast DB write error.
		_, _ = node.store.sql.Exec(`CREATE TRIGGER fail_bounty_update BEFORE UPDATE ON hub_bounty_claims BEGIN SELECT RAISE(ABORT, 'disk io error'); END`)
		return &BountyPayoutResult{
			Accepted:   true,
			BountyID:   b.ID,
			RewardSats: b.RewardSats,
			TxID:       "tx-h2-payout-001",
		}, nil
	})

	ts := httptest.NewServer(node.Handler())
	defer ts.Close()

	sellerKey, _ := ec.NewPrivateKey()
	sellerPub := hex.EncodeToString(sellerKey.PubKey().Compressed())
	hdp := strings.Repeat("ab", 32)
	sub := BountySubmission{
		BountyID:      "b-h2",
		SellerPub:     sellerPub,
		SellerAddress: "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa",
		HDP:           hdp,
		Text:          "verified fact for H2 test",
		TS:            time.Now().Unix(),
	}
	sub.Sig, err = bstack.SignArtifact(sellerKey, sub.Signable(hdp))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(sub)
	resp, err := http.Post(ts.URL+"/bounties/submit", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("H2 REGRESSION: expected HTTP 500 when FinalizeBountyClaim fails, got %d", resp.StatusCode)
	}
}

// TestHubStoreMigrationIdempotentAndFailClosed verifies RCA-RT-7:
// Hub SQLite migrations check PRAGMA table_info cleanly, upgrade legacy tables idempotently,
// and fail closed when schema migration cannot execute.
func TestHubStoreMigrationIdempotentAndFailClosed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-hub.sqlite")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a legacy pre-migration hub_events & hub_bbox schema lacking newer columns.
	if _, err := rawDB.Exec(`
		CREATE TABLE hub_events (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			outpoint TEXT NOT NULL UNIQUE,
			topic TEXT NOT NULL,
			txid TEXT NOT NULL,
			vout INTEGER NOT NULL,
			kind TEXT NOT NULL,
			pub TEXT NOT NULL DEFAULT '',
			domains_json TEXT NOT NULL DEFAULT '[]',
			payload_json BLOB NOT NULL,
			beef BLOB,
			exp INTEGER NOT NULL DEFAULT 0,
			admitted_at INTEGER NOT NULL
		);
		CREATE TABLE hub_bbox (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			office TEXT NOT NULL DEFAULT 'default',
			recipient_pub TEXT NOT NULL,
			sender_pub TEXT NOT NULL,
			deal_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			body_json BLOB NOT NULL,
			created_at INTEGER NOT NULL
		);
	`); err != nil {
		t.Fatal(err)
	}
	_ = rawDB.Close()

	// Opening via OpenStore MUST migrate missing columns cleanly and reopen idempotently.
	st, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore legacy migrate failed: %v", err)
	}
	_ = st.Close()
	st2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore second open failed: %v", err)
	}
	// Verify migrated columns exist.
	if _, err := st2.sql.Exec(`SELECT spent, block_height FROM hub_events LIMIT 1`); err != nil {
		t.Fatalf("migrated hub_events columns missing: %v", err)
	}
	if _, err := st2.sql.Exec(`SELECT sig, ts, nonce, aud FROM hub_bbox LIMIT 1`); err != nil {
		t.Fatalf("migrated hub_bbox columns missing: %v", err)
	}
	_ = st2.Close()

	// Fail-closed check: corrupt a table name into a VIEW so ALTER TABLE fails -> OpenStore MUST return an error!
	badPath := filepath.Join(t.TempDir(), "bad-hub.sqlite")
	badDB, err := sql.Open("sqlite", badPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := badDB.Exec(`CREATE VIEW hub_bbox AS SELECT 1 AS seq`); err != nil {
		t.Fatal(err)
	}
	_ = badDB.Close()
	if badSt, err := OpenStore(badPath); err == nil {
		_ = badSt.Close()
		t.Fatal("RCA-RT-7 REGRESSION: OpenStore silently ignored failed migration on invalid hub_bbox view")
	}
}
