package backup

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestEncryptedBackupRoundTripTamperAndRetention(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	backupDir := t.TempDir()

	db, err := store.Open(filepath.Join(dataDir, "nektar.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: dataDir})
	if err != nil {
		t.Fatalf("chain.Open: %v", err)
	}
	defer port.Close()

	mem, err := memory.New(db, port, nil, nil, memory.Config{})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	if _, err := mem.Remember(ctx, memory.RememberInput{
		Shard: "ops",
		Text:  "Critical mainnet canary runbook: verify NTP skew < 15s before arming autonomous spend.",
	}); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	sink, err := NewLocalDirSink(backupDir)
	if err != nil {
		t.Fatalf("NewLocalDirSink: %v", err)
	}
	mgr := NewManager(Config{
		DB:      db,
		Port:    port,
		DataDir: dataDir,
		Sink:    sink,
		Keep:    2,
	})

	// Run concurrent writes while snapshotting to prove WAL-safe VACUUM INTO consistency.
	doneWrite := make(chan struct{})
	go func() {
		defer close(doneWrite)
		for i := 0; i < 5; i++ {
			_, _ = mem.Remember(ctx, memory.RememberInput{
				Shard: "ops",
				Text:  fmt.Sprintf("Concurrent live note #%d during WAL-safe backup", i),
			})
		}
	}()

	res, err := mgr.BackupNow(ctx)
	<-doneWrite
	if err != nil {
		t.Fatalf("BackupNow: %v", err)
	}
	if res.SizeBytes == 0 || res.Manifest.Files["nektar.sqlite"].Size == 0 || res.Manifest.Files["wallet/wallet.sqlite"].Size == 0 {
		t.Fatalf("expected both nektar.sqlite and wallet/wallet.sqlite in archive manifest, got %+v", res.Manifest)
	}

	archiveBytes, err := os.ReadFile(res.Location)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}

	// 1. Genuine archive passes VerifyArchive (BRC-42 unwrap + AES-GCM + zstd + SHA-256 + SQLite integrity_check).
	man, err := VerifyArchive(ctx, port.WalletInterface(), archiveBytes)
	if err != nil {
		t.Fatalf("VerifyArchive genuine: %v", err)
	}
	if man.SchemaVersion < 10 {
		t.Fatalf("expected schemaVersion >= 10, got %d", man.SchemaVersion)
	}

	// 2. Flipping 1 ciphertext byte MUST fail GCM authentication.
	tampered := append([]byte(nil), archiveBytes...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := VerifyArchive(ctx, port.WalletInterface(), tampered); err == nil {
		t.Fatal("expected VerifyArchive to reject 1-byte tampered ciphertext")
	}

	// 3. Different wallet identity MUST fail BRC-42 DEK unwrap.
	otherPort, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("other chain.Open: %v", err)
	}
	defer otherPort.Close()
	if _, err := VerifyArchive(ctx, otherPort.WalletInterface(), archiveBytes); err == nil {
		t.Fatal("expected VerifyArchive to fail with a different wallet identity")
	}

	// 4. Restore into a fresh directory and verify SQLite fragments + schema version survived intact.
	restoreDir := t.TempDir()
	if _, err := RestoreArchive(ctx, port.WalletInterface(), archiveBytes, restoreDir); err != nil {
		t.Fatalf("RestoreArchive: %v", err)
	}
	restoredDB, err := store.Open(filepath.Join(restoreDir, "nektar.sqlite"))
	if err != nil {
		t.Fatalf("open restored nektar.sqlite: %v", err)
	}
	defer restoredDB.Close()
	totalFrags, _, err := restoredDB.CountFragments()
	if err != nil || totalFrags < 1 {
		t.Fatalf("expected >= 1 restored fragment, got %d err=%v", totalFrags, err)
	}
	if ver, _ := restoredDB.UserVersion(); ver != 10 {
		t.Fatalf("expected restored user_version=10, got %d", ver)
	}

	// 5. Retention pruning: create 3 more distinct timestamped files and verify Keep=2 retains only 2 newest.
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("nektar-memory-test-20261127T12000%dZ.nktbak", i)
		if _, err := sink.Put(ctx, name, archiveBytes); err != nil {
			t.Fatalf("sink.Put %s: %v", name, err)
		}
	}
	deleted, err := sink.Prune(ctx, 2)
	if err != nil || deleted < 2 {
		t.Fatalf("expected >=2 pruned archives, got deleted=%d err=%v", deleted, err)
	}
	left, _ := sink.List(ctx)
	if len(left) != 2 {
		t.Fatalf("expected exactly 2 archives left after Prune(2), got %d", len(left))
	}
}

func TestS3SigV4SinkPutGetListAndPrune(t *testing.T) {
	var mu sync.Mutex
	objects := make(map[string][]byte)
	var sawSigV4 int

	s3Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIA_TEST/") || r.Header.Get("X-Amz-Content-Sha256") == "" {
			http.Error(w, "missing SigV4 headers", http.StatusForbidden)
			return
		}
		mu.Lock()
		sawSigV4++
		mu.Unlock()

		path := strings.TrimPrefix(r.URL.Path, "/mybucket")
		key := strings.TrimPrefix(path, "/")

		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			objects[key] = body
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if r.URL.Query().Get("list-type") == "2" {
				pref := r.URL.Query().Get("prefix")
				type item struct {
					Key          string `xml:"Key"`
					Size         int64  `xml:"Size"`
					LastModified string `xml:"LastModified"`
				}
				var resp struct {
					XMLName  xml.Name `xml:"ListBucketResult"`
					Contents []item   `xml:"Contents"`
				}
				mu.Lock()
				for k, v := range objects {
					if pref == "" || strings.HasPrefix(k, pref) {
						resp.Contents = append(resp.Contents, item{
							Key:          k,
							Size:         int64(len(v)),
							LastModified: time.Now().UTC().Format(time.RFC3339),
						})
					}
				}
				mu.Unlock()
				w.Header().Set("Content-Type", "application/xml")
				_ = xml.NewEncoder(w).Encode(resp)
				return
			}
			mu.Lock()
			data, ok := objects[key]
			mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(data)
		case http.MethodDelete:
			mu.Lock()
			delete(objects, key)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer s3Srv.Close()

	sink, err := NewS3Sink(S3Config{
		Endpoint:   s3Srv.URL,
		Bucket:     "mybucket",
		Region:     "us-east-1",
		Prefix:     "node1",
		AccessKey:  "AKIA_TEST",
		SecretKey:  "SECRET_TEST_KEY",
		HTTPClient: s3Srv.Client(),
	}, "memory")
	if err != nil {
		t.Fatalf("NewS3Sink: %v", err)
	}
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		loc, err := sink.Put(ctx, fmt.Sprintf("nektar-main-20261127T12000%dZ.nktbak", i), []byte(fmt.Sprintf("blob-%d", i)))
		if err != nil {
			t.Fatalf("S3 Put %d: %v", i, err)
		}
		if !strings.HasPrefix(loc, "s3://mybucket/node1/") {
			t.Fatalf("unexpected S3 location %q", loc)
		}
	}
	got, err := sink.Get(ctx, "nektar-main-20261127T120003Z.nktbak")
	if err != nil || string(got) != "blob-3" {
		t.Fatalf("S3 Get mismatch: got=%q err=%v", string(got), err)
	}
	deleted, err := sink.Prune(ctx, 2)
	if err != nil || deleted != 1 {
		t.Fatalf("expected S3 Prune(2) to delete 1 oldest archive, got deleted=%d err=%v", deleted, err)
	}
	list, err := sink.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("expected 2 remaining S3 objects, got %d err=%v", len(list), err)
	}
	if sawSigV4 < 6 {
		t.Fatalf("expected SigV4 auth on all S3 requests, got %d", sawSigV4)
	}
}
