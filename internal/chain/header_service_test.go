package chain

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/lightwebinc/bcommon/headers"
)

type stubHeaderWitness struct {
	tip uint32
	hdr map[uint32]headers.Header
}

func (s *stubHeaderWitness) CurrentHeight(_ context.Context) (uint32, error) { return s.tip, nil }
func (s *stubHeaderWitness) HeaderAt(_ context.Context, h uint32) (*headers.Header, error) {
	v, ok := s.hdr[h]
	if !ok {
		return nil, headers.ErrUnknownHeight
	}
	cp := v
	return &cp, nil
}
func (s *stubHeaderWitness) IsValidRootForHeight(_ context.Context, root *chainhash.Hash, h uint32) (bool, error) {
	v, ok := s.hdr[h]
	if !ok || root == nil {
		return false, nil
	}
	return v.MerkleRoot == *root, nil
}

func TestSovereignHeaderStoreAndAnchorClient(t *testing.T) {
	ctx := context.Background()
	var root100, hash100 chainhash.Hash
	root100[0] = 0xAA
	root100[31] = 0x55
	hash100[0] = 0x11
	hash100[31] = 0x99

	stub := &stubHeaderWitness{
		tip: 100,
		hdr: map[uint32]headers.Header{
			100: {
				Height:     100,
				Hash:       hash100,
				MerkleRoot: root100,
				Version:    1,
				Time:       1700000000,
				Bits:       0x1d00ffff,
				Nonce:      42,
			},
		},
	}
	wt, err := NewWitnessedTracker("regtest", []NamedWitness{{
		Name:   "stub",
		Origin: "http://stub.local",
		Kind:   headers.BlockHeadersService,
		Source: stub,
	}})
	if err != nil {
		t.Fatalf("NewWitnessedTracker: %v", err)
	}
	// Seed WitnessedTracker cache directly to avoid external PoW difficulty requirement on synthetic header.
	now := time.Now()
	wt.mu.Lock()
	wt.cache[100] = cachedHeader{hdr: stub.hdr[100], fetchedAt: now}
	wt.cachedTip = 100
	wt.cachedTipAt = now
	wt.mu.Unlock()

	port := &Port{witnessed: wt, tracker: wt}
	ts := httptest.NewServer(port.HeaderStoreHandler())
	defer ts.Close()

	anchor := NewHeaderAnchorClient(ts.URL, ts.Client())
	tip, err := anchor.CurrentHeight(ctx)
	if err != nil || tip != 100 {
		t.Fatalf("CurrentHeight: got (%d, %v), want (100, nil)", tip, err)
	}
	ok, err := anchor.IsValidRootForHeight(ctx, &root100, 100)
	if err != nil || !ok {
		t.Fatalf("IsValidRootForHeight(valid): got (%v, %v), want (true, nil)", ok, err)
	}
	var wrongRoot chainhash.Hash
	wrongRoot[0] = 0xFF
	ok, err = anchor.IsValidRootForHeight(ctx, &wrongRoot, 100)
	if err != nil || ok {
		t.Fatalf("IsValidRootForHeight(wrong): got (%v, %v), want (false, nil)", ok, err)
	}
	if _, err := anchor.HeaderAt(ctx, 999); err != headers.ErrUnknownHeight {
		t.Fatalf("HeaderAt(999): got %v, want ErrUnknownHeight", err)
	}
}

func TestValidateHeaderIndependenceAnchor(t *testing.T) {
	if err := ValidateHeaderIndependence("testnet", "anchor:https://nektar.run/hub", "woc:test", "broadcaster:test"); err != nil {
		t.Fatalf("testnet anchor validation failed: %v", err)
	}
	if err := ValidateHeaderIndependence("main", "anchor:https://nektar.run/hub,woc:main", "woc:main", "broadcaster:main"); err != nil {
		t.Fatalf("mainnet anchor+woc validation failed: %v", err)
	}
	if err := ValidateHeaderIndependence("main", "anchor:https://nektar.run/hub,anchor:https://nektar.run/hub", "woc:main", "broadcaster:main"); err == nil {
		t.Fatalf("expected duplicate anchor origin error on mainnet")
	}
}
