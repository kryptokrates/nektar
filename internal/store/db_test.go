package store

import (
	"testing"
	"time"
)

func TestStoreCRUDAndAnchors(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	rec, dedup, err := db.AppendFragment(FragmentRecord{
		Shard:      "alpha",
		HDP:        "hdp1",
		Leaf:       "leaf1",
		Data:       []byte("hello verifiable world"),
		Provenance: []byte("prov1"),
		Level:      1,
		Class:      "fact",
		CreatedAt:  time.Now().Unix(),
	})
	if err != nil || dedup || rec.Seq != 1 {
		t.Fatalf("unexpected append: rec=%+v dedup=%v err=%v", rec, dedup, err)
	}
	// Duplicate HDP deduplicates cleanly.
	rec2, dedup2, err := db.AppendFragment(FragmentRecord{
		Shard:      "alpha",
		HDP:        "hdp1",
		Leaf:       "leaf1",
		Data:       []byte("hello verifiable world"),
		Provenance: []byte("prov1"),
		CreatedAt:  time.Now().Unix(),
	})
	if err != nil || !dedup2 || rec2.Seq != 1 {
		t.Fatalf("expected dedup: rec=%+v dedup=%v err=%v", rec2, dedup2, err)
	}

	unanchored, err := db.ListUnanchored("alpha")
	if err != nil || len(unanchored) != 1 {
		t.Fatalf("expected 1 unanchored, got %d err=%v", len(unanchored), err)
	}
	if err := db.RecordBatchAnchor(BatchAnchor{
		Shard:      "alpha",
		ShardID:    "sid1",
		N:          0,
		Size:       1,
		RootHex:    "root0",
		ChainedHex: "rp0",
		TxID:       "tx0",
		Settled:    true,
		CreatedAt:  time.Now().Unix(),
	}, []int64{1}); err != nil {
		t.Fatalf("RecordBatchAnchor: %v", err)
	}
	unanchored, err = db.ListUnanchored("alpha")
	if err != nil || len(unanchored) != 0 {
		t.Fatalf("expected 0 unanchored after anchor, got %d", len(unanchored))
	}

	if err := db.SaveReceipt(ReceiptRecord{
		ReceiptHash: "rhash1",
		ShardSeq:    1,
		BodyJSON:    []byte(`{"answer":"ok"}`),
		AnchorTxID:  "tx0",
		CreatedAt:   time.Now().Unix(),
	}, []CitationEdge{{Shard: "alpha", Seq: 1, ReceiptHash: "rhash1"}}); err != nil {
		t.Fatalf("SaveReceipt: %v", err)
	}
	cites, err := db.CitingReceipts("alpha", 1)
	if err != nil || len(cites) != 1 || cites[0] != "rhash1" {
		t.Fatalf("unexpected citations: %v err=%v", cites, err)
	}
}
