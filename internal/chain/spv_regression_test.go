package chain

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/headers"

	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

type stubWitness struct {
	mu      sync.RWMutex
	headers map[uint32]*headers.Header
	tip     uint32
	err     error
}

func (s *stubWitness) HeaderAt(_ context.Context, height uint32) (*headers.Header, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.err != nil {
		return nil, s.err
	}
	h, ok := s.headers[height]
	if !ok {
		return nil, headers.ErrUnknownHeight
	}
	cp := *h
	return &cp, nil
}

func (s *stubWitness) CurrentHeight(_ context.Context) (uint32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.err != nil {
		return 0, s.err
	}
	return s.tip, nil
}

func (s *stubWitness) IsValidRootForHeight(_ context.Context, root *chainhash.Hash, height uint32) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.err != nil {
		return false, s.err
	}
	h, ok := s.headers[height]
	if !ok {
		return false, nil
	}
	return root != nil && h.MerkleRoot == *root, nil
}

func mineStubHeader(height uint32, prev, merkleRoot chainhash.Hash, ts uint32) *headers.Header {
	// Easy target (bits 0x207fffff -> regtest easy PoW) so mining in unit tests is instantaneous.
	bits := uint32(0x207fffff)
	target := compactBitsToBig(bits)
	var raw [80]byte
	binary.LittleEndian.PutUint32(raw[0:4], 1)
	copy(raw[4:36], prev[:])
	copy(raw[36:68], merkleRoot[:])
	binary.LittleEndian.PutUint32(raw[68:72], ts)
	binary.LittleEndian.PutUint32(raw[72:76], bits)
	for nonce := uint32(0); ; nonce++ {
		binary.LittleEndian.PutUint32(raw[76:80], nonce)
		h := chainhash.DoubleHashH(raw[:])
		if chainHashToBig(h).Cmp(target) <= 0 {
			return &headers.Header{
				Height:     height,
				Version:    1,
				Prev:       prev,
				MerkleRoot: merkleRoot,
				Time:       ts,
				Bits:       bits,
				Nonce:      nonce,
				Hash:       h,
			}
		}
	}
}

func TestWitnessedTrackerHeaderIndependenceAndCircularRejection(t *testing.T) {
	// 0. DefaultSettleSpec and NormalizeSettleSpec must resolve to official BSV Association Broadcaster endpoints (never GorillaPool).
	wantMain := "broadcaster:" + DefaultBroadcasterMainnet + ",broadcaster:" + DefaultBroadcasterMainnetEU
	if got := DefaultSettleSpec("main"); got != wantMain || strings.Contains(strings.ToLower(got), "gorillapool") {
		t.Fatalf("unexpected DefaultSettleSpec(main): %q (want %q)", got, wantMain)
	}
	wantTest := "broadcaster:" + DefaultBroadcasterTestnet
	if got := DefaultSettleSpec("testnet"); got != wantTest || strings.Contains(strings.ToLower(got), "gorillapool") {
		t.Fatalf("unexpected DefaultSettleSpec(testnet): %q (want %q)", got, wantTest)
	}
	for in, want := range map[string]string{
		"arcade:main":         wantMain,
		"broadcaster:main":    wantMain,
		"broadcaster:mainnet": wantMain,
		"arcade:test":         wantTest,
		"arcade:testnet":      wantTest,
		"broadcaster:test":    wantTest,
		"broadcaster:testnet": wantTest,
		"arcade:https://arcade-eu-1.bsvblockchain.tech":      "broadcaster:https://arcade-eu-1.bsvblockchain.tech",
		"broadcaster:https://arcade-eu-1.bsvblockchain.tech": "broadcaster:https://arcade-eu-1.bsvblockchain.tech",
	} {
		if got := NormalizeSettleSpec(in); got != want {
			t.Fatalf("NormalizeSettleSpec(%q) = %q, want %q", in, got, want)
		}
	}

	// 1. Circular single-source WoC on mainnet MUST be rejected (#1, #2).
	if err := ValidateHeaderIndependence("main", "woc:main", "woc:main", DefaultSettleSpec("main")); err == nil {
		t.Fatal("expected single circular woc:main header source to be rejected")
	}
	// 2. Zero-PoW Native mode on mainnet/testnet MUST be rejected (#1).
	if err := ValidateHeaderIndependence("main", "native:http://127.0.0.1:8080,woc:main", "woc:main", DefaultSettleSpec("main")); err == nil {
		t.Fatal("expected Native header source on mainnet to be rejected")
	}
	if err := ValidateHeaderIndependence("testnet", "native:http://127.0.0.1:8080", "woc:test", ""); err == nil {
		t.Fatal("expected Native header source on testnet to be rejected")
	}
	// 3. Duplicate origins MUST be rejected on both mainnet and testnet (#1, #2).
	if err := ValidateHeaderIndependence("main", "woc:main,woc:main", "woc:main", DefaultSettleSpec("main")); err == nil {
		t.Fatal("expected duplicate header witness origin to be rejected")
	}
	if err := ValidateHeaderIndependence("testnet", "woc:test,woc:test", "woc:test", ""); err == nil {
		t.Fatal("expected duplicate testnet header witness origin to be rejected")
	}
	// 4. Default >=2 distinct-origin PoW witnesses pass independence check (#1, #2).
	if err := ValidateHeaderIndependence("main", DefaultHeaderSources("main"), "woc:main", DefaultSettleSpec("main")); err != nil {
		t.Fatalf("expected DefaultHeaderSources(main) to pass: %v", err)
	}
	if err := ValidateHeaderIndependence("testnet", DefaultHeaderSources("testnet"), "woc:test", DefaultSettleSpec("testnet")); err != nil {
		t.Fatalf("expected DefaultHeaderSources(testnet) to pass: %v", err)
	}
	// 5. Regtest native: parses cleanly (#1).
	if err := ValidateHeaderIndependence("regtest", "native:http://127.0.0.1:8080", "rpc:http://127.0.0.1:18332", ""); err != nil {
		t.Fatalf("expected regtest native: header source to parse: %v", err)
	}
}

func TestWitnessedTrackerQuorumPrevLinkageReorgAndMTP(t *testing.T) {
	ctx := context.Background()
	w1 := &stubWitness{headers: make(map[uint32]*headers.Header), tip: 15}
	w2 := &stubWitness{headers: make(map[uint32]*headers.Header), tip: 15}

	var prev chainhash.Hash
	for h := uint32(0); h <= 15; h++ {
		mr := chainhash.DoubleHashH([]byte{byte(h), 0x42})
		hdr := mineStubHeader(h, prev, mr, 1_700_000_000+h*600)
		w1.headers[h] = hdr
		cp := *hdr
		w2.headers[h] = &cp
		prev = hdr.Hash
	}

	wt, err := NewWitnessedTracker("regtest", []NamedWitness{
		{Name: "w1", Origin: "https://w1.example", Kind: headers.WhatsOnChain, Source: w1},
		{Name: "w2", Origin: "https://w2.example", Kind: headers.Arcade, Source: w2},
	})
	if err != nil {
		t.Fatalf("NewWitnessedTracker: %v", err)
	}
	wt.quorum = 2
	wt.minDifficulty = 0

	// 1. Consensus root check passes when both witnesses agree and Prev links cleanly.
	mr15 := w1.headers[15].MerkleRoot
	ok, err := wt.IsValidRootForHeight(ctx, &mr15, 15)
	if err != nil || !ok {
		t.Fatalf("expected valid root at height 15, got ok=%v err=%v", ok, err)
	}

	// 2. Consensus 11-block MedianTimePastAt (heights 5..15 -> median is height 10 = 1700006000) (#35).
	mtp, err := wt.MedianTimePastAt(ctx, 15)
	if err != nil {
		t.Fatalf("MedianTimePastAt: %v", err)
	}
	wantMTP := uint32(1_700_000_000 + 10*600)
	if mtp != wantMTP {
		t.Fatalf("expected 11-block MTP=%d, got %d", wantMTP, mtp)
	}

	// 3. Reorg triggers OnReorg callback and evicts orphaned cached headers (#13).
	var reorgFork uint32
	var reorgOrphans []string
	wt.OnReorg(func(forkHeight uint32, orphaned []string) {
		reorgFork = forkHeight
		reorgOrphans = orphaned
	})
	// Re-mine height 14 & 15 on a competing fork across both witnesses.
	newMR14 := chainhash.DoubleHashH([]byte("fork-14"))
	newH14 := mineStubHeader(14, w1.headers[13].Hash, newMR14, 1_700_000_000+14*600+5)
	newMR15 := chainhash.DoubleHashH([]byte("fork-15"))
	newH15 := mineStubHeader(15, newH14.Hash, newMR15, 1_700_000_000+15*600+5)
	w1.mu.Lock()
	w1.headers[14] = newH14
	w1.headers[15] = newH15
	w1.mu.Unlock()
	w2.mu.Lock()
	cp14 := *newH14
	cp15 := *newH15
	w2.headers[14] = &cp14
	w2.headers[15] = &cp15
	w2.mu.Unlock()

	// Verify old root at 15 is now rejected AND new root succeeds & fires OnReorg at forkHeight=14!
	okNew, err := wt.IsValidRootForHeight(ctx, &newMR15, 15)
	if err != nil || !okNew {
		t.Fatalf("expected new fork root at height 15 to verify, got ok=%v err=%v", okNew, err)
	}
	if reorgFork != 14 || len(reorgOrphans) == 0 {
		t.Fatalf("expected OnReorg(forkHeight=14, orphans>0), got fork=%d orphans=%v", reorgFork, reorgOrphans)
	}

	// 4. Witness disagreement fails closed with ErrHeaderDisagreement (#1).
	altMR12 := chainhash.DoubleHashH([]byte("disagree-12"))
	w2.mu.Lock()
	w2.headers[12] = mineStubHeader(12, w2.headers[11].Hash, altMR12, 1_700_000_000+12*600+9)
	w2.mu.Unlock()
	wt.mu.Lock()
	delete(wt.cache, 12)
	wt.mu.Unlock()
	if _, err := wt.HeaderAt(ctx, 12); !errors.Is(err, ErrHeaderDisagreement) {
		t.Fatalf("expected ErrHeaderDisagreement on conflicting witnesses, got %v", err)
	}
}

func TestVerifyCounterpartyPaymentBEEFStrictSPVCapsAndConflictWatch(t *testing.T) {
	ctx := context.Background()
	port, err := Open(ctx, Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open memory port: %v", err)
	}
	defer port.Close()

	addr, err := port.FundAddress()
	if err != nil {
		t.Fatal(err)
	}
	sendRes, err := port.Send(ctx, nwallet.SendArgs{ToAddress: addr, Satoshis: 1500})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(sendRes.BEEF) == 0 {
		t.Fatal("expected non-empty AtomicBEEF from Send")
	}

	// 0. Unmined 0-conf BEEF enforces AllowZeroConf & per-deal cap cleanly (#7).
	verUnminedDisabled, err := VerifyCounterpartyPaymentBEEF(ctx, sendRes.BEEF, port.Tracker(), PaymentVerifyOpts{
		ExpectedAmountSats: 1500,
		SettleDepth:        1,
		AllowZeroConf:      false,
	})
	if err != nil || !verUnminedDisabled.NeedsConfirmation || verUnminedDisabled.ZeroConfEligible {
		t.Fatalf("expected AllowZeroConf=false to require confirmation, got ver=%+v err=%v", verUnminedDisabled, err)
	}
	verUnminedOK, err := VerifyCounterpartyPaymentBEEF(ctx, sendRes.BEEF, port.Tracker(), PaymentVerifyOpts{
		ExpectedAmountSats:      1500,
		MaxUnconfirmedAncestors: 15,
		ZeroConfMaxSats:         2000,
		AllowZeroConf:           true,
	})
	if err != nil || !verUnminedOK.ZeroConfEligible || verUnminedOK.NeedsConfirmation {
		t.Fatalf("expected valid 0-conf under cap to be ZeroConfEligible, got ver=%+v err=%v", verUnminedOK, err)
	}

	// Attach the verified mined BUMP from testchain to construct a mined AtomicBEEF.
	mp, _, ok := port.Proof(ctx, sendRes.TxID)
	if !ok || mp == nil {
		t.Fatalf("expected testchain Proof for %s", sendRes.TxID)
	}
	minedTx, err := transaction.NewTransactionFromBEEF(sendRes.BEEF)
	if err != nil {
		t.Fatal(err)
	}
	minedTx.MerklePath = mp
	minedBEEF, err := minedTx.AtomicBEEF(false)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Mined payment verified at depth 1 passes when SettleDepth=1, escalates NeedsConfirmation when SettleDepth=6 (#8).
	ver1, err := VerifyCounterpartyPaymentBEEF(ctx, minedBEEF, port.Tracker(), PaymentVerifyOpts{
		ExpectedAmountSats: 1500,
		SettleDepth:        1,
		AllowZeroConf:      false,
	})
	if err != nil || !ver1.SettledAtDepth {
		t.Fatalf("expected mined tx at depth>=1 to settle, got ver=%+v err=%v", ver1, err)
	}
	ver6, err := VerifyCounterpartyPaymentBEEF(ctx, minedBEEF, port.Tracker(), PaymentVerifyOpts{
		ExpectedAmountSats: 1500,
		SettleDepth:        6,
		AllowZeroConf:      false,
	})
	if err != nil || ver6.SettledAtDepth || !ver6.NeedsConfirmation {
		t.Fatalf("expected depth<6 to require confirmation (NeedsConfirmation=true), got ver=%+v err=%v", ver6, err)
	}

	// 2. Tampered BUMP MUST fail closed with ErrRefusedBUMP — NEVER fall through to 0-conf (#6)!
	txTampered, err := transaction.NewTransactionFromBEEF(minedBEEF)
	if err != nil || txTampered.MerklePath == nil {
		t.Fatalf("parse mined BEEF: %v", err)
	}
	txTampered.MerklePath.BlockHeight += 50 // bogus height for this root
	badBEEF, err := txTampered.AtomicBEEF(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyCounterpartyPaymentBEEF(ctx, badBEEF, port.Tracker(), PaymentVerifyOpts{
		ExpectedAmountSats: 1500,
		ZeroConfMaxSats:    10_000,
		AllowZeroConf:      true,
	}); !errors.Is(err, ErrRefusedBUMP) {
		t.Fatalf("expected ErrRefusedBUMP on forged BUMP (no 0-conf fallback), got %v", err)
	}

	// 3. Tracker outage MUST fail closed with ErrTrackerUnavailable (#6).
	downTracker := &stubWitness{err: errors.New("connection refused")}
	if _, err := VerifyCounterpartyPaymentBEEF(ctx, minedBEEF, downTracker, PaymentVerifyOpts{
		ExpectedAmountSats: 1500,
		ZeroConfMaxSats:    10_000,
		AllowZeroConf:      true,
	}); !errors.Is(err, ErrTrackerUnavailable) {
		t.Fatalf("expected ErrTrackerUnavailable on tracker outage, got %v", err)
	}

	// 4. ZeroConfWatch enforces aggregate exposure cap AND outpoint conflict detection (#7).
	watch := NewZeroConfWatch()
	if err := watch.CheckAndTrack("tx-a", 600, []string{"prev1:0"}, 1000); err != nil {
		t.Fatalf("first 0-conf track should pass: %v", err)
	}
	if err := watch.CheckAndTrack("tx-b", 600, []string{"prev2:0"}, 1000); !errors.Is(err, ErrZeroConfCapExceeded) {
		t.Fatalf("expected ErrZeroConfCapExceeded on aggregate cap breach, got %v", err)
	}
	if err := watch.CheckAndTrack("tx-conflict", 200, []string{"prev1:0"}, 1000); !errors.Is(err, ErrConflictDetected) {
		t.Fatalf("expected ErrConflictDetected on competing outpoint spend, got %v", err)
	}
	if !watch.HasConflict("tx-a") {
		t.Fatal("expected tx-a to be flagged as conflicted")
	}
	watch.MarkSettled("tx-a")
	exp, cnt := watch.Exposure()
	if exp != 0 || cnt != 0 {
		t.Fatalf("expected exposure=0 count=0 after MarkSettled, got %d/%d", exp, cnt)
	}
}
