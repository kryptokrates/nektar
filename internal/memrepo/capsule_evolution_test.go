package memrepo

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/lightwebinc/bcommon/cbor"

	"github.com/kryptokrates/nektar/internal/core"
)

func buildRawCapsuleFromItems(t *testing.T, items []cbor.Map) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString(CapsuleMagicHeader)
	buf.WriteByte(CapsuleModePlainZstd)
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	counts := make(map[uint64]uint64)
	for _, item := range items {
		raw, err := core.WriteCBORSequenceItem(zw, item)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = h.Write(raw)
		for _, p := range item {
			if k, ok := p.Key.(uint64); ok && k == 0 {
				counts[p.Val.(uint64)]++
			}
		}
	}
	tr := CapsuleTrailer{
		StreamDigest: core.SHA256(h.Sum(nil)),
		RecordCounts: counts,
	}
	trMap := append(cbor.Map{{Key: uint64(0), Val: capRecTrailer}}, trailerToCBOR(tr)...)
	if _, err := core.WriteCBORSequenceItem(zw, trMap); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCapsuleSplitCBORMapCriticalAndOptionalEvolution(t *testing.T) {
	baseHeader := cbor.Map{
		{Key: uint64(0), Val: capRecHeader},
		{Key: uint64(1), Val: CapsuleFormatVersion},
		{Key: uint64(2), Val: "ops"},
		{Key: uint64(3), Val: DefaultBranch},
		{Key: uint64(6), Val: "02" + "0000000000000000000000000000000000000000000000000000000000000001"},
		{Key: uint64(9), Val: int64(1700000000)},
		{Key: uint64(10), Val: int64(-1)},
	}

	// 1. Unknown EVEN critical key (100) inside Header MUST fail closed with ErrUnknownCriticalKey.
	badHdr := append(cbor.Map(nil), baseHeader...)
	badHdr = append(badHdr, cbor.Pair{Key: uint64(100), Val: "breaking-header-ext"})
	rawBadHdr := buildRawCapsuleFromItems(t, []cbor.Map{badHdr})
	if _, err := DecodeCapsule(rawBadHdr); !errors.Is(err, core.ErrUnknownCriticalKey) {
		t.Fatalf("expected ErrUnknownCriticalKey for Header key 100, got %v", err)
	}

	// 2. Unknown EVEN critical key (100) inside FragmentMeta MUST fail closed with ErrUnknownCriticalKey.
	badFrag := cbor.Map{
		{Key: uint64(0), Val: capRecFragmentMeta},
		{Key: uint64(1), Val: "ops"},
		{Key: uint64(2), Val: int64(1)},
		{Key: uint64(100), Val: true},
	}
	rawBadFrag := buildRawCapsuleFromItems(t, []cbor.Map{baseHeader, badFrag})
	if _, err := DecodeCapsule(rawBadFrag); !errors.Is(err, core.ErrUnknownCriticalKey) {
		t.Fatalf("expected ErrUnknownCriticalKey for FragmentMeta key 100, got %v", err)
	}

	// 3. Unknown ODD key (101) inside Header + unknown ODD record type (11) MUST decode cleanly & preserve HeaderExtra on round-trip!
	goodHdr := append(cbor.Map(nil), baseHeader...)
	goodHdr = append(goodHdr, cbor.Pair{Key: uint64(101), Val: "future-header-hint"})
	oddRec := cbor.Map{
		{Key: uint64(0), Val: uint64(11)}, // unknown odd record type (non-critical)
		{Key: uint64(1), Val: "optional-future-record"},
	}
	rawGood := buildRawCapsuleFromItems(t, []cbor.Map{goodHdr, oddRec})
	decCap, err := DecodeCapsule(rawGood)
	if err != nil {
		t.Fatalf("DecodeCapsule with odd header key + odd record type 11 failed: %v", err)
	}
	if len(decCap.HeaderExtra) != 1 || decCap.HeaderExtra[0].Key.(uint64) != 101 {
		t.Fatalf("expected HeaderExtra key 101 preserved, got %+v", decCap.HeaderExtra)
	}
	reEnc, err := EncodeCapsule(decCap)
	if err != nil {
		t.Fatalf("EncodeCapsule round-trip failed: %v", err)
	}
	decCap2, err := DecodeCapsule(reEnc)
	if err != nil {
		t.Fatalf("DecodeCapsule second pass failed: %v", err)
	}
	if len(decCap2.HeaderExtra) != 1 || decCap2.HeaderExtra[0].Key.(uint64) != 101 {
		t.Fatalf("HeaderExtra lost after re-encode: %+v", decCap2.HeaderExtra)
	}

	// 4. BlobObject, TreeObject, and CommitObject preserve unknown odd keys across CBOR round-trip.
	blob := BlobObject{
		BlobHash:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Path:          "facts/a.md",
		MediaType:     "text/markdown",
		Size:          4,
		ContentSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Extra:         []cbor.Pair{{Key: uint64(101), Val: "blob-ext"}},
	}
	bRaw, err := EncodeBlobObjectCBOR(blob)
	if err != nil {
		t.Fatal(err)
	}
	bDec, err := DecodeBlobObjectCBOR(bRaw)
	if err != nil || len(bDec.Extra) != 1 || bDec.Extra[0].Key.(uint64) != 101 {
		t.Fatalf("BlobObject odd extra not preserved: %+v err=%v", bDec.Extra, err)
	}

	tree := TreeObject{
		TreeHash: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Entries: []TreeEntry{{
			Mode:          "100644",
			Path:          "facts/a.md",
			BlobHash:      blob.BlobHash,
			MediaType:     "text/markdown",
			Size:          4,
			ContentSHA256: blob.ContentSHA256,
			Extra:         []cbor.Pair{{Key: uint64(103), Val: "entry-ext"}},
		}},
		Extra: []cbor.Pair{{Key: uint64(105), Val: "tree-ext"}},
	}
	tRaw, err := EncodeTreeObjectCBOR(tree)
	if err != nil {
		t.Fatal(err)
	}
	tDec, err := DecodeTreeObjectCBOR(tRaw)
	if err != nil || len(tDec.Extra) != 1 || tDec.Extra[0].Key.(uint64) != 105 || len(tDec.Entries) != 1 || len(tDec.Entries[0].Extra) != 1 {
		t.Fatalf("TreeObject/TreeEntry odd extra not preserved: %+v err=%v", tDec, err)
	}
}
