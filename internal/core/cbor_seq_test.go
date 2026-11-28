package core

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/lightwebinc/bcommon/cbor"
)

func TestCBORSplitMapCriticalVsNonCritical(t *testing.T) {
	known := KnownKeyRange(0, 3)

	// Unknown odd key (5) is non-critical and preserved in extra.
	mOdd := cbor.Map{
		{Key: uint64(0), Val: "hello"},
		{Key: uint64(5), Val: uint64(42)},
	}
	rawOdd, err := EncodeCBOR(mOdd)
	if err != nil {
		t.Fatalf("encode odd: %v", err)
	}
	vOdd, err := DecodeCBOR(rawOdd)
	if err != nil {
		t.Fatalf("decode odd: %v", err)
	}
	vals, extra, err := SplitCBORMap(vOdd, known)
	if err != nil {
		t.Fatalf("split odd failed: %v", err)
	}
	if s, err := CBORText(vals, 0); err != nil || s != "hello" {
		t.Fatalf("key 0 got %q err %v", s, err)
	}
	if len(extra) != 1 || extra[0].Key.(uint64) != 5 || extra[0].Val.(uint64) != 42 {
		t.Fatalf("expected extra key 5=42, got %+v", extra)
	}

	// Unknown even key (4) is critical and MUST fail closed.
	mEven := cbor.Map{
		{Key: uint64(0), Val: "hello"},
		{Key: uint64(4), Val: true},
	}
	rawEven, err := EncodeCBOR(mEven)
	if err != nil {
		t.Fatalf("encode even: %v", err)
	}
	vEven, err := DecodeCBOR(rawEven)
	if err != nil {
		t.Fatalf("decode even: %v", err)
	}
	if _, _, err := SplitCBORMap(vEven, known); !errors.Is(err, ErrUnknownCriticalKey) {
		t.Fatalf("expected ErrUnknownCriticalKey for even key 4, got %v", err)
	}
}

func TestCBORSequenceStreamRoundTripAndLimits(t *testing.T) {
	items := []cbor.Value{
		cbor.Map{{Key: uint64(0), Val: uint64(1)}, {Key: uint64(1), Val: "header"}},
		cbor.Map{{Key: uint64(0), Val: uint64(2)}, {Key: uint64(1), Val: []byte{0xde, 0xad, 0xbe, 0xef}}},
		cbor.Map{{Key: uint64(0), Val: uint64(3)}, {Key: uint64(1), Val: CBORStringsVal([]string{"a", "b"})}, {Key: uint64(2), Val: true}},
	}
	var buf bytes.Buffer
	var rawWritten [][]byte
	for _, it := range items {
		raw, err := WriteCBORSequenceItem(&buf, it)
		if err != nil {
			t.Fatalf("WriteCBORSequenceItem: %v", err)
		}
		rawWritten = append(rawWritten, raw)
	}

	sr := NewCBORSeqReader(&buf, 4096)
	for i := range items {
		raw, val, err := sr.Next()
		if err != nil {
			t.Fatalf("Next[%d]: %v", i, err)
		}
		if !bytes.Equal(raw, rawWritten[i]) {
			t.Fatalf("Next[%d] raw mismatch", i)
		}
		if _, ok := val.(cbor.Map); !ok {
			t.Fatalf("Next[%d] expected map, got %T", i, val)
		}
	}
	if _, _, err := sr.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF, got %v", err)
	}

	// Reject oversized item.
	srTiny := NewCBORSeqReader(bytes.NewReader(rawWritten[1]), 4)
	if _, _, err := srTiny.Next(); !errors.Is(err, ErrCBORSeqItemTooLarge) {
		t.Fatalf("expected ErrCBORSeqItemTooLarge, got %v", err)
	}

	// Reject indefinite-length map (0xbf ... 0xff).
	srIndef := NewCBORSeqReader(bytes.NewReader([]byte{0xbf, 0xff}), 1024)
	if _, _, err := srIndef.Next(); err == nil {
		t.Fatalf("expected indefinite-length rejection")
	}

	// Reject non-canonical integer (0x18 0x05 encodes 5 in 2 bytes instead of 0x05).
	srNonCanon := NewCBORSeqReader(bytes.NewReader([]byte{0x18, 0x05}), 1024)
	if _, _, err := srNonCanon.Next(); err == nil {
		t.Fatalf("expected non-canonical integer rejection")
	}
}

func TestVerifyTransitionChain(t *testing.T) {
	q0 := Zero32Bytes()
	in1 := DSHA256([]byte("input-1"))
	c1 := DSHA256([]byte("commit-1"))
	t1 := DSHA256([]byte("tree-1"))
	q1, err := NextStepStateQ(q0, in1, c1, t1, nil, nil, 1700000001)
	if err != nil {
		t.Fatal(err)
	}
	tr1 := TransitionRecord{
		Version: 1, Shard: "ops", Seq: 1, PrevStateQ: q0, InputDigest: in1,
		NextStateQ: q1, CommitHash: c1, TreeHash: t1, Timestamp: 1700000001,
	}
	tr1.Hash, err = ComputeTransitionHash(tr1)
	if err != nil {
		t.Fatal(err)
	}

	in2 := DSHA256([]byte("input-2"))
	c2 := DSHA256([]byte("commit-2"))
	t2 := DSHA256([]byte("tree-2"))
	q2, err := NextStepStateQ(q1, in2, c2, t2, nil, nil, 1700000002)
	if err != nil {
		t.Fatal(err)
	}
	tr2 := TransitionRecord{
		Version: 1, Shard: "ops", Seq: 2, PrevStateQ: q1, InputDigest: in2,
		NextStateQ: q2, CommitHash: c2, TreeHash: t2, Timestamp: 1700000002,
	}
	tr2.Hash, err = ComputeTransitionHash(tr2)
	if err != nil {
		t.Fatal(err)
	}

	finalQ, err := VerifyTransitionChain(q0, []TransitionRecord{tr1, tr2})
	if err != nil {
		t.Fatalf("VerifyTransitionChain failed: %v", err)
	}
	if !EqualBytes(finalQ, q2) {
		t.Fatalf("finalQ mismatch")
	}

	// Tampering prevQ on step 2 must fail.
	bad := tr2
	bad.PrevStateQ = Zero32Bytes()
	if _, err := VerifyTransitionChain(q0, []TransitionRecord{tr1, bad}); err == nil {
		t.Fatalf("expected tampered chain to fail")
	}
}
