package core

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/lightwebinc/bcommon/cbor"
)

func TestSpendGuardMonotonicDayFloorOnBackwardNTP(t *testing.T) {
	cur := time.Date(2026, 11, 29, 0, 10, 0, 0, time.UTC)
	sg := NewSpendGuard(SpendGuardOpts{
		MaxFeeSatsPerTx: 1000,
		MaxSpendSatsDay: 100,
		StatePath:       filepath.Join(t.TempDir(), "spend-guard.json"),
		Now:             func() time.Time { return cur },
	})
	defer sg.Close()

	if err := sg.CheckAndRecord("tx-day29-1", 10, 60); err != nil {
		t.Fatalf("first spend failed: %v", err)
	}

	// Simulate NTP stepping wall clock backward across UTC midnight to 2026-11-28 23:55.
	cur = time.Date(2026, 11, 28, 23, 55, 0, 0, time.UTC)
	if err := sg.CheckAndRecord("tx-backward-attempt", 10, 50); !errors.Is(err, ErrDailyCapExceeded) {
		t.Fatalf("expected ErrDailyCapExceeded after backward NTP step across midnight, got %v", err)
	}

	// Stepping forward to 2026-11-30 MUST roll over cleanly.
	cur = time.Date(2026, 11, 30, 0, 1, 0, 0, time.UTC)
	if err := sg.CheckAndRecord("tx-day30-1", 10, 50); err != nil {
		t.Fatalf("expected forward day rollover to allow spend, got %v", err)
	}
}

func TestSpendGuardMinIntervalBackwardClockNoDeadlock(t *testing.T) {
	cur := time.Date(2026, 11, 29, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "spend-guard.json")
	sg1 := NewSpendGuard(SpendGuardOpts{
		MaxFeeSatsPerTx: 1000,
		MaxSpendSatsDay: 1000,
		MinInterval:     5 * time.Second,
		StatePath:       path,
		Now:             func() time.Time { return cur },
	})
	if err := sg1.CheckAndRecord("tx-1", 10, 10); err != nil {
		t.Fatalf("initial spend: %v", err)
	}
	_ = sg1.Close()

	// Reopen after NTP steps system clock backward by 30 seconds: must NOT deadlock MinInterval.
	cur = cur.Add(-30 * time.Second)
	sg2 := NewSpendGuard(SpendGuardOpts{
		MaxFeeSatsPerTx: 1000,
		MaxSpendSatsDay: 1000,
		MinInterval:     5 * time.Second,
		StatePath:       path,
		Now:             func() time.Time { return cur },
	})
	defer sg2.Close()
	if err := sg2.CheckAndRecord("tx-2", 10, 10); err != nil {
		t.Fatalf("expected backward clock step not to deadlock MinInterval, got %v", err)
	}
}

func TestCBORMaxKeysGreaseAndSchemaEvolution(t *testing.T) {
	// 1. MaxCBORMapKeys cap enforced fail-closed.
	big := make(cbor.Map, 0, MaxCBORMapKeys+1)
	for i := uint64(0); i <= MaxCBORMapKeys; i++ {
		big = append(big, cbor.Pair{Key: i, Val: uint64(1)})
	}
	if _, _, err := SplitCBORMap(big, KnownKeyRange(0, MaxCBORMapKeys)); !errors.Is(err, ErrCBORTooManyKeys) {
		t.Fatalf("expected ErrCBORTooManyKeys, got %v", err)
	}

	// 2. MaybeAppendGrease + AppendExtraPairs round-trip.
	m := MaybeAppendGrease(cbor.Map{{Key: uint64(1), Val: "ok"}}, true)
	raw, err := EncodeCBOR(m)
	if err != nil {
		t.Fatal(err)
	}
	v, err := DecodeCBOR(raw)
	if err != nil {
		t.Fatal(err)
	}
	vals, extra, err := SplitCBORMap(v, KnownKeyRange(1, 1))
	if err != nil {
		t.Fatalf("SplitCBORMap with grease failed: %v", err)
	}
	if len(vals) != 1 || len(extra) != 1 || extra[0].Key.(uint64) != GreaseKey {
		t.Fatalf("unexpected grease split: vals=%v extra=%v", vals, extra)
	}

	// 3. RefinementEdge rejects unknown even critical keys and preserves odd keys.
	badEdge, err := EncodeCBOR([]cbor.Value{cbor.Map{
		{Key: uint64(0), Val: "s"},
		{Key: uint64(4), Val: string(RefinementRefines)},
		{Key: uint64(100), Val: true}, // unknown critical even key
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRefinesCBOR(badEdge); !errors.Is(err, ErrUnknownCriticalKey) {
		t.Fatalf("expected ErrUnknownCriticalKey on RefinementEdge key 100, got %v", err)
	}

	goodEdge := RefinementEdge{
		TargetShard: "ops",
		TargetSeq:   1,
		TargetHDP:   "0000000000000000000000000000000000000000000000000000000000000001",
		TargetLeaf:  "0000000000000000000000000000000000000000000000000000000000000002",
		Kind:        RefinementRefines,
		Extra:       []cbor.Pair{{Key: uint64(101), Val: "future-hint"}},
	}
	encEdges, err := encodeRefinesCBOR([]RefinementEdge{goodEdge})
	if err != nil {
		t.Fatal(err)
	}
	decEdges, err := decodeRefinesCBOR(encEdges)
	if err != nil || len(decEdges) != 1 || len(decEdges[0].Extra) != 1 || decEdges[0].Extra[0].Key.(uint64) != 101 {
		t.Fatalf("expected odd key 101 preserved on RefinementEdge, got edges=%+v err=%v", decEdges, err)
	}

	// 4. TransitionRecord preserves odd extension keys across hash + decode + re-encode.
	tr := TransitionRecord{
		Version:     1,
		Shard:       "ops",
		Seq:         1,
		PrevStateQ:  Zero32Bytes(),
		InputDigest: SHA256([]byte("in")),
		NextStateQ:  SHA256([]byte("next")),
		Timestamp:   1700000000,
		Extra:       []cbor.Pair{{Key: uint64(101), Val: "ext-transition"}},
	}
	trRaw, err := EncodeTransitionCBOR(tr)
	if err != nil {
		t.Fatalf("EncodeTransitionCBOR: %v", err)
	}
	decTr, err := DecodeTransitionCBOR(trRaw)
	if err != nil {
		t.Fatalf("DecodeTransitionCBOR with odd extra failed: %v", err)
	}
	if len(decTr.Extra) != 1 || decTr.Extra[0].Key.(uint64) != 101 {
		t.Fatalf("expected odd key 101 preserved on TransitionRecord, got %+v", decTr.Extra)
	}
}
