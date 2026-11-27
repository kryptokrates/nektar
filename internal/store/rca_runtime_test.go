package store

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRCA_StoreReceiptChainAndAtomicAnchorBinding(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Append two receipts via CommitReceiptAtomic -> prev_leaf & chain_hash MUST be populated!
	f1, err := db.CommitReceiptAtomic(FragmentRecord{
		Shard:      "__receipts__",
		HDP:        "rhdp1",
		Leaf:       "1111111111111111111111111111111111111111111111111111111111111111",
		Data:       []byte(`{"answer":"a1"}`),
		Provenance: []byte("p1"),
		CreatedAt:  time.Now().Unix(),
	}, ReceiptRecord{
		ReceiptHash: "rhash1",
		BodyJSON:    []byte(`{"answer":"a1"}`),
		CreatedAt:   time.Now().Unix(),
	}, nil)
	if err != nil {
		t.Fatalf("CommitReceiptAtomic 1: %v", err)
	}
	if f1.ChainHash == "" {
		t.Fatalf("expected non-empty chain_hash on first receipt fragment")
	}

	f2, err := db.CommitReceiptAtomic(FragmentRecord{
		Shard:      "__receipts__",
		HDP:        "rhdp2",
		Leaf:       "2222222222222222222222222222222222222222222222222222222222222222",
		Data:       []byte(`{"answer":"a2"}`),
		Provenance: []byte("p2"),
		CreatedAt:  time.Now().Unix(),
	}, ReceiptRecord{
		ReceiptHash: "rhash2",
		BodyJSON:    []byte(`{"answer":"a2"}`),
		CreatedAt:   time.Now().Unix(),
	}, nil)
	if err != nil {
		t.Fatalf("CommitReceiptAtomic 2: %v", err)
	}
	if f2.PrevLeaf != f1.Leaf {
		t.Fatalf("expected f2.PrevLeaf=%s, got %s", f1.Leaf, f2.PrevLeaf)
	}
	wantChain2 := ComputeFragmentChainHash(f1.ChainHash, f2.Leaf)
	if f2.ChainHash != wantChain2 {
		t.Fatalf("expected f2.ChainHash=%s, got %s", wantChain2, f2.ChainHash)
	}

	// RecordBatchAnchor on __receipts__ must atomically stamp receipts.anchor_txid inside the same tx!
	if err := db.RecordBatchAnchor(BatchAnchor{
		Shard:      "__receipts__",
		ShardID:    "sid",
		N:          0,
		Size:       2,
		RootHex:    "root0",
		ChainedHex: "rp0",
		TxID:       "tx_rcp_0",
		CreatedAt:  time.Now().Unix(),
	}, []int64{f1.Seq, f2.Seq}); err != nil {
		t.Fatalf("RecordBatchAnchor: %v", err)
	}
	r2, err := db.GetReceipt("rhash2")
	if err != nil {
		t.Fatalf("GetReceipt: %v", err)
	}
	if r2.AnchorTxID != "tx_rcp_0" {
		t.Fatalf("expected receipts.anchor_txid stamped atomically to tx_rcp_0, got %q", r2.AnchorTxID)
	}
}

func TestRCA_CorruptKVCountersFailClosed(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	today := time.Now().UTC().Format("2006-01-02")
	if _, err := db.sql.Exec(`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`, "market_daily_spend:"+today, []byte("{corrupt")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReserveSpendAtomic("op1", 1, 10, 100, 1000); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("expected ReserveSpendAtomic to fail closed on corrupt KV JSON, got err=%v", err)
	}

	if _, err := db.sql.Exec(`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`, "faucet_global_daily:"+today, []byte("not-a-uint64")); err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveFaucetDrip("1AddrTest", 100, 1000, 10000); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("expected ReserveFaucetDrip to fail closed on corrupt KV JSON, got err=%v", err)
	}
}

func TestRCA_ForgetFragmentShredsMemObjects(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	blobHash := "blobhash_secret_1"
	treeHash := "treehash_secret_1"
	commitHash := "commithash_secret_1"

	blobBody, _ := json.Marshal(map[string]any{
		"blobHash":      blobHash,
		"path":          "notes/secret.md",
		"contentSha256": "deadbeef",
		"inline":        []byte("TOP_SECRET_PLAINTEXT"),
		"chirpUrl":      "chirp://abc",
	})
	treeBody, _ := json.Marshal(map[string]any{
		"treeHash": treeHash,
		"entries": []map[string]any{
			{"path": "notes/secret.md", "blobHash": blobHash},
		},
	})
	if err := db.PutMemObject(blobHash, "blob", blobBody, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.PutMemObject(treeHash, "tree", treeBody, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMemCommit(MemCommitRow{
		CommitHash: commitHash, Repo: "priv", Branch: "main", Seq: 1,
		TreeHash: treeHash, MPERoot: "mpe", AuthorPub: "pub", ModelID: "m", Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	f, _, err := db.AppendFragment(FragmentRecord{
		Shard: "priv", HDP: "hdp1", Leaf: "leaf1", Data: []byte("TOP_SECRET_PLAINTEXT"),
		Provenance: []byte("p"), Path: "notes/secret.md", CommitHash: commitHash, CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := db.ForgetFragment("priv", f.Seq); err != nil {
		t.Fatalf("ForgetFragment: %v", err)
	}
	_, rawAfter, err := db.GetMemObject(blobHash)
	if err != nil {
		t.Fatalf("GetMemObject: %v", err)
	}
	if strings.Contains(string(rawAfter), "TOP_SECRET") || strings.Contains(string(rawAfter), "VE9QX1NFQ1JFVA") || strings.Contains(string(rawAfter), "chirp://abc") {
		t.Fatalf("forgotten mem_objects blob leaked payload/chirpUrl: %s", string(rawAfter))
	}
	var obj map[string]any
	if err := json.Unmarshal(rawAfter, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["forgotten"] != true {
		t.Fatalf("expected forgotten=true tombstone in mem_objects, got %+v", obj)
	}
}

func TestRCA_MigrateMarketDoesNotAdvanceVersionOnError(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Reset user_version to 0 and inject malformed inputs_json in payment_claims so v1 backfill MUST fail.
	if _, err := db.sql.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO payment_claims (txid, inputs_json, claimed_at) VALUES ('badtx', '{not-json-array', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.migrateMarket(); err == nil {
		t.Fatal("expected migrateMarket to fail closed on corrupt legacy payment_claims row")
	}
	var ver int
	if err := db.sql.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver != 0 {
		t.Fatalf("expected user_version to stay 0 after failed migration, got %d", ver)
	}
}

func TestLoadOrCreateStableKeySerializedAndFailClosedOnCorrupt(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Concurrent callers must observe one identical generated key (no TOCTOU race).
	const workers = 8
	results := make([][]byte, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		idx := i
		go func() {
			defer wg.Done()
			k, kErr := db.LoadOrCreateStableKey("stable_test_key", func() ([]byte, error) {
				b := make([]byte, 32)
				b[0] = byte(idx + 1)
				return b, nil
			})
			if kErr != nil {
				t.Errorf("LoadOrCreateStableKey(%d): %v", idx, kErr)
				return
			}
			results[idx] = k
		}()
	}
	wg.Wait()
	for i := 1; i < workers; i++ {
		if !bytes.Equal(results[0], results[i]) {
			t.Fatalf("concurrent LoadOrCreateStableKey diverged: [0]=%x [%d]=%x", results[0], i, results[i])
		}
	}

	// Corrupt stored KV entry must fail closed without overwriting the key.
	if _, err := db.sql.Exec(`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`, "corrupt_key", []byte(`"not-64-hex"`)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LoadOrCreateStableKey("corrupt_key", func() ([]byte, error) {
		return make([]byte, 32), nil
	}); err == nil {
		t.Fatal("expected LoadOrCreateStableKey to fail closed on corrupt existing KV entry")
	}
}
