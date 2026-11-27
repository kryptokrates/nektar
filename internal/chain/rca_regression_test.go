package chain

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/headers"
	"github.com/lightwebinc/bcommon/nodeapi"

	"github.com/kryptokrates/nektar/internal/core"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

type mockCovenantClientWallet struct {
	sdkwallet.Interface
	mu          sync.Mutex
	createCalls int
	signCalls   int
	abortCalls  int
	signErr     error
	signableTx  *transaction.Transaction
	signedTx    *transaction.Transaction
}

func (m *mockCovenantClientWallet) CreateAction(_ context.Context, _ sdkwallet.CreateActionArgs, _ string) (*sdkwallet.CreateActionResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls++
	beef, err := m.signableTx.AtomicBEEF(true)
	if err != nil {
		return nil, err
	}
	return &sdkwallet.CreateActionResult{
		SignableTransaction: &sdkwallet.SignableTransaction{
			Tx:        beef,
			Reference: []byte("ref-cov-1"),
		},
	}, nil
}

func (m *mockCovenantClientWallet) SignAction(_ context.Context, _ sdkwallet.SignActionArgs, _ string) (*sdkwallet.SignActionResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signCalls++
	if m.signErr != nil {
		return nil, m.signErr
	}
	tx := m.signedTx
	if tx == nil {
		tx = m.signableTx
	}
	beef, err := tx.AtomicBEEF(true)
	if err != nil {
		return nil, err
	}
	return &sdkwallet.SignActionResult{
		Txid: *tx.TxID(),
		Tx:   beef,
	}, nil
}

func (m *mockCovenantClientWallet) AbortAction(_ context.Context, _ sdkwallet.AbortActionArgs, _ string) (*sdkwallet.AbortActionResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.abortCalls++
	return &sdkwallet.AbortActionResult{Aborted: true}, nil
}

func buildTrivialCovenantFixture(t *testing.T, outSats uint64) (*transaction.Transaction, *transaction.Transaction, *script.Script) {
	t.Helper()
	// Locking script OP_DROP OP_TRUE (0x75 0x51) accepts OP_TRUE (0x51) unlocking script
	// leaving a single clean true element on the stack under Chronicle Script VM (CLEANSTACK).
	lockDropTrue := script.Script{0x75, 0x51}
	opTrue := script.Script{0x51}
	coinbaseSig := script.Script{0x04, 0x01, 0x02, 0x03, 0x04}
	parent := transaction.NewTransaction()
	parent.AddInput(&transaction.TransactionInput{
		SourceTXID:       &chainhash.Hash{},
		SourceTxOutIndex: 0xffffffff,
		UnlockingScript:  &coinbaseSig,
		SequenceNumber:   0xffffffff,
	})
	parent.AddOutput(&transaction.TransactionOutput{
		Satoshis:      outSats + 500,
		LockingScript: &lockDropTrue,
	})
	// Attach a synthetic minimal MerklePath so AtomicBEEF(true) succeeds cleanly in mock.
	parent.MerklePath = &transaction.MerklePath{
		BlockHeight: 10,
		Path: [][]*transaction.PathElement{{{
			Offset: 0,
			Hash:   parent.TxID(),
			Txid:   boolPtr(true),
		}}},
	}

	spend := transaction.NewTransaction()
	spend.AddInput(&transaction.TransactionInput{
		SourceTXID:        parent.TxID(),
		SourceTxOutIndex:  0,
		SourceTransaction: parent,
		SequenceNumber:    0xffffffff,
		UnlockingScript:   &opTrue,
	})
	spend.AddOutput(&transaction.TransactionOutput{
		Satoshis:      outSats,
		LockingScript: &lockDropTrue,
	})
	return parent, spend, &opTrue
}

func boolPtr(b bool) *bool { return &b }

// TestRCA_S1_ClientModeCovenantSpendGuardAndUnknown verifies Mode B (client mode) in BuildCovenantSpendTx:
//  1. SpendGuard cap rejects BEFORE p.wallet.CreateAction is ever called.
//  2. Successful SignAction commits SpendGuard reservation so spentToday reflects the spend.
//  3. Uncertain SignAction error (broken pipe) returns ErrBroadcastUnknown, NEVER calls AbortAction,
//     and keeps SpendGuard reservation intact.
//  4. Deterministic SignAction error cancels SpendGuard reservation and calls AbortAction.
func TestRCA_S1_ClientModeCovenantSpendGuardAndUnknown(t *testing.T) {
	ctx := context.Background()

	newPort := func(maxDaily uint64, w *mockCovenantClientWallet) (*Port, *core.SpendGuard) {
		sg := core.NewSpendGuard(core.SpendGuardOpts{
			MaxFeeSatsPerTx: 5000,
			MaxSpendSatsDay: maxDaily,
			StatePath:       filepath.Join(t.TempDir(), "spend-guard.json"),
		})
		return &Port{
			network:     "testnet",
			wallet:      w,
			guard:       sg,
			clientTxDir: filepath.Join(t.TempDir(), "client-txs"),
			clientBuilt: make(map[string]*transaction.Transaction),
		}, sg
	}

	// 1. Over-cap covenant spend must fail BEFORE CreateAction is called.
	parent1, spend1, opTrue := buildTrivialCovenantFixture(t, 2000)
	mock1 := &mockCovenantClientWallet{signableTx: spend1}
	p1, _ := newPort(1000, mock1)
	_, err := p1.BuildCovenantSpendTx(ctx, CovenantSpendRequest{
		InputTx:        parent1,
		InputVout:      0,
		Outputs:        []*transaction.TransactionOutput{{Satoshis: 2000, LockingScript: opTrue}},
		FuelFromWallet: true,
		BuildUnlock:    func(_ *transaction.Transaction, _ []byte, _ uint64) (*script.Script, error) { return opTrue, nil },
		Description:    "over-cap client covenant spend",
	})
	if err == nil {
		t.Fatal("expected SpendGuard cap rejection for over-cap client covenant spend")
	}
	if mock1.createCalls != 0 {
		t.Fatalf("expected 0 CreateAction calls when SpendGuard rejects pre-flight, got %d", mock1.createCalls)
	}

	// 2. Normal client covenant spend commits SpendGuard reservation.
	parent2, spend2, _ := buildTrivialCovenantFixture(t, 1200)
	mock2 := &mockCovenantClientWallet{signableTx: spend2}
	p2, sg2 := newPort(5000, mock2)
	res2, err := p2.BuildCovenantSpendTx(ctx, CovenantSpendRequest{
		InputTx:        parent2,
		InputVout:      0,
		Outputs:        []*transaction.TransactionOutput{{Satoshis: 1200, LockingScript: opTrue}},
		FuelFromWallet: true,
		BuildUnlock:    func(_ *transaction.Transaction, _ []byte, _ uint64) (*script.Script, error) { return opTrue, nil },
		Description:    "valid client covenant spend",
	})
	if err != nil {
		t.Fatalf("valid client covenant spend failed: %v", err)
	}
	if !res2.AlreadyBroadcast {
		t.Fatal("expected AlreadyBroadcast=true in client mode")
	}
	if spentToday, _, _, _ := sg2.Stats(); spentToday < 1200 {
		t.Fatalf("expected SpendGuard SpentToday >= 1200 after client covenant spend, got %d", spentToday)
	}

	// 3. Uncertain SignAction error (broken pipe) returns ErrBroadcastUnknown, does NOT AbortAction,
	//    and keeps SpendGuard reservation locked so a subsequent spend cannot reuse the budget!
	parent3, spend3, _ := buildTrivialCovenantFixture(t, 1500)
	mock3 := &mockCovenantClientWallet{
		signableTx: spend3,
		signErr:    errors.New("write tcp 127.0.0.1:3321: broken pipe"),
	}
	p3, _ := newPort(2000, mock3)
	_, err = p3.BuildCovenantSpendTx(ctx, CovenantSpendRequest{
		InputTx:        parent3,
		InputVout:      0,
		Outputs:        []*transaction.TransactionOutput{{Satoshis: 1500, LockingScript: opTrue}},
		FuelFromWallet: true,
		BuildUnlock:    func(_ *transaction.Transaction, _ []byte, _ uint64) (*script.Script, error) { return opTrue, nil },
		Description:    "uncertain client covenant spend",
	})
	if !errors.Is(err, nwallet.ErrBroadcastUnknown) {
		t.Fatalf("expected ErrBroadcastUnknown on broken pipe SignAction, got %v", err)
	}
	if mock3.abortCalls != 0 {
		t.Fatalf("expected 0 AbortAction calls on uncertain client broadcast, got %d", mock3.abortCalls)
	}
	// Budget of 2000 minus reserved >=1700 leaves <1000, so a second 1000-sat spend MUST be rejected by SpendGuard!
	parent3b, spend3b, _ := buildTrivialCovenantFixture(t, 1000)
	mock3.signableTx = spend3b
	mock3.signErr = nil
	if _, err := p3.BuildCovenantSpendTx(ctx, CovenantSpendRequest{
		InputTx:        parent3b,
		InputVout:      0,
		Outputs:        []*transaction.TransactionOutput{{Satoshis: 1000, LockingScript: opTrue}},
		FuelFromWallet: true,
		BuildUnlock:    func(_ *transaction.Transaction, _ []byte, _ uint64) (*script.Script, error) { return opTrue, nil },
		Description:    "second spend after uncertain reservation",
	}); err == nil {
		t.Fatal("expected SpendGuard to retain reservation from uncertain SignAction and block second spend")
	}

	// 4. Deterministic 400 SignAction refusal cancels SpendGuard reservation AND calls AbortAction.
	parent4, spend4, _ := buildTrivialCovenantFixture(t, 1500)
	mock4 := &mockCovenantClientWallet{
		signableTx: spend4,
		signErr:    errors.New("HTTP 400: deterministic script failure"),
	}
	p4, _ := newPort(2000, mock4)
	_, err = p4.BuildCovenantSpendTx(ctx, CovenantSpendRequest{
		InputTx:        parent4,
		InputVout:      0,
		Outputs:        []*transaction.TransactionOutput{{Satoshis: 1500, LockingScript: opTrue}},
		FuelFromWallet: true,
		BuildUnlock:    func(_ *transaction.Transaction, _ []byte, _ uint64) (*script.Script, error) { return opTrue, nil },
		Description:    "deterministic 400 client covenant spend",
	})
	if err == nil || errors.Is(err, nwallet.ErrBroadcastUnknown) {
		t.Fatalf("expected non-unknown error on deterministic 400, got %v", err)
	}
	if mock4.abortCalls != 1 {
		t.Fatalf("expected 1 AbortAction call on deterministic SignAction refusal, got %d", mock4.abortCalls)
	}
}

type hostRewriteRT struct {
	target *url.URL
	base   http.RoundTripper
}

func (h hostRewriteRT) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = h.target.Scheme
	clone.URL.Host = h.target.Host
	return h.base.RoundTrip(clone)
}

// TestRCA_S2_FindSpendingTxLiveNodeChain proves Port.FindSpendingTx queries nodeChain.Spender +
// TxRaw on live networks, verifies sha256d txid & input linkage, and fails closed on forged raw bytes.
func TestRCA_S2_FindSpendingTxLiveNodeChain(t *testing.T) {
	ctx := context.Background()
	parent, spend, _ := buildTrivialCovenantFixture(t, 1000)
	parentID := parent.TxID().String()
	spendID := spend.TxID().String()
	spendHex := hex.EncodeToString(spend.Bytes())

	var returnForged bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tx/"+parentID) && strings.HasSuffix(r.URL.Path, "/spent"):
			_ = json.NewEncoder(w).Encode(map[string]any{"txid": spendID, "vin": 0})
		case strings.HasSuffix(r.URL.Path, "/tx/"+spendID+"/hex"):
			if returnForged {
				// Return parent bytes under spendID -> sha256d mismatch!
				_, _ = w.Write([]byte(hex.EncodeToString(parent.Bytes())))
				return
			}
			_, _ = w.Write([]byte(spendHex))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	cli := &http.Client{Transport: hostRewriteRT{target: u, base: srv.Client().Transport}}
	nc, err := nodeapi.ParseChain("woc:test", nodeapi.ChainOptions{Client: cli, Headers: &stubHeaderWitness{}})
	if err != nil {
		t.Fatalf("nodeapi.ParseChain: %v", err)
	}
	p := &Port{network: "testnet", nodeChain: nc}

	gotTx, vin, found, err := p.FindSpendingTx(ctx, parentID, 0)
	if err != nil || !found || vin != 0 || gotTx == nil || gotTx.TxID().String() != spendID {
		t.Fatalf("FindSpendingTx valid = (%v, %d, %v, %v), want spendTx=%s vin=0 found=true", gotTx, vin, found, err, spendID)
	}

	returnForged = true
	if _, _, found, err := p.FindSpendingTx(ctx, parentID, 0); err == nil || found {
		t.Fatalf("expected fail-closed mismatch error on forged spender raw tx bytes, got found=%v err=%v", found, err)
	}
}

// TestRCA_S5_HeaderAnchorClientDetectsReorg proves HeaderAnchorClient does not stale-cache headers
// across a Spine reorg at height h (delegating TTL caching to WitnessedTracker).
func TestRCA_S5_HeaderAnchorClientDetectsReorg(t *testing.T) {
	ctx := context.Background()
	var rootA, rootB chainhash.Hash
	rootA[0] = 0x11
	rootB[0] = 0x22
	hdrMap := make(map[uint32]*headers.Header)
	var prev chainhash.Hash
	for h := uint32(494); h <= 499; h++ {
		var mr chainhash.Hash
		mr[0] = byte(h)
		hdr := mineStubHeader(h, prev, mr, 1699990000+h*600)
		hdrMap[h] = hdr
		prev = hdr.Hash
	}
	hdrA := mineStubHeader(500, prev, rootA, 1700300000)
	hdrB := mineStubHeader(500, prev, rootB, 1700300060)

	var mu sync.Mutex
	curHdr := hdrA
	encodeHdr := func(w http.ResponseWriter, h *headers.Header) {
		_ = json.NewEncoder(w).Encode(SovereignHeaderJSON{
			Height:     h.Height,
			Hash:       h.Hash.String(),
			PrevHash:   h.Prev.String(),
			MerkleRoot: h.MerkleRoot.String(),
			Version:    h.Version,
			Time:       h.Time,
			Bits:       h.Bits,
			Nonce:      h.Nonce,
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		h500 := curHdr
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/tip", "/v1/root/500":
			encodeHdr(w, h500)
		default:
			if strings.HasPrefix(r.URL.Path, "/v1/root/") {
				var qh uint32
				if _, err := fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/v1/root/"), "%d", &qh); err == nil {
					if hdr, ok := hdrMap[qh]; ok {
						encodeHdr(w, hdr)
						return
					}
				}
			}
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := NewHeaderAnchorClient(srv.URL, srv.Client())
	h1, err := client.HeaderAt(ctx, 500)
	if err != nil || h1.MerkleRoot != rootA {
		t.Fatalf("initial HeaderAt(500) = (%v, %v), want rootA", h1, err)
	}

	// Warm WitnessedTracker against Spine anchor at height 500 (rootA).
	wt, err := NewWitnessedTracker("regtest", []NamedWitness{{
		Name:   "anchor",
		Origin: srv.URL,
		Kind:   headers.BlockHeadersService,
		Source: client,
	}})
	if err != nil {
		t.Fatalf("NewWitnessedTracker: %v", err)
	}
	var reorgHeight uint32
	wt.OnReorg(func(h uint32, _ []string) { reorgHeight = h })
	if ok, err := wt.IsValidRootForHeight(ctx, &rootA, 500); err != nil || !ok {
		t.Fatalf("initial WitnessedTracker IsValidRootForHeight(rootA) = (%v, %v)", ok, err)
	}

	// Simulate Spine reorg at height 500 -> HeaderAnchorClient sees rootB immediately,
	// and WitnessedTracker re-checks & fires OnReorg(500)!
	mu.Lock()
	curHdr = hdrB
	mu.Unlock()
	h2, err := client.HeaderAt(ctx, 500)
	if err != nil || h2.MerkleRoot != rootB {
		t.Fatalf("post-reorg HeaderAt(500) = (%v, %v), want rootB", h2, err)
	}
	wt.mu.Lock()
	if ce, ok := wt.cache[500]; ok {
		ce.fetchedAt = time.Now().Add(-5 * time.Second)
		wt.cache[500] = ce
	}
	wt.mu.Unlock()
	ok, err := wt.IsValidRootForHeight(ctx, &rootB, 500)
	if err != nil || !ok {
		t.Fatalf("WitnessedTracker post-reorg IsValidRootForHeight(rootB) = (%v, %v)", ok, err)
	}
	if reorgHeight != 500 {
		t.Fatalf("expected WitnessedTracker OnReorg(500) to fire after Spine reorg, got %d", reorgHeight)
	}
}

// TestRatchet_ChainNoDuplicateTransportClassifiers enforces RCA-S4 structurally:
// chain production files must delegate uncertain/already-known broadcast error checks to
// internal/wallet's canonical classifiers instead of duplicating transport token lists.
func TestRatchet_ChainNoDuplicateTransportClassifiers(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	forbidden := []string{`"broken pipe"`, `"connection reset"`, `"already in mempool"`, `"txn-already-known"`}
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
		for _, tok := range forbidden {
			if strings.Contains(src, tok) {
				t.Errorf("RCA-S4 ratchet violated: %s duplicates transport/broadcast token %s (use wallet.IsUncertainBroadcastError / IsAlreadyKnownBroadcastError)", name, tok)
			}
		}
	}
}
