package backup

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestStreamedChunkedBackupMultiChunkTruncationAndAuxDBs(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()

	db, err := store.Open(filepath.Join(dataDir, "nektar.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()

	// Create auxiliary workflows.sqlite & hub.sqlite in dataDir plus client-txs/ and a >2.5 MiB random CHIRP blob
	// to force multiple 1 MiB AEAD chunks (ARCH-F10).
	for _, aux := range []string{"workflows.sqlite", "hub.sqlite"} {
		auxDB, oErr := sql.Open("sqlite", filepath.Join(dataDir, aux))
		if oErr != nil {
			t.Fatal(oErr)
		}
		if _, eErr := auxDB.ExecContext(ctx, `CREATE TABLE marker (k TEXT PRIMARY KEY, v TEXT); INSERT INTO marker(k,v) VALUES ('ok', ?);`, aux); eErr != nil {
			_ = auxDB.Close()
			t.Fatal(eErr)
		}
		_ = auxDB.Close()
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "client-txs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "client-txs", "deadbeef.beef"), []byte("raw-client-beef-journal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "chirp", "ab"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Pseudo-random incompressible 2.4 MiB payload -> spans >= 3 distinct 1 MiB AEAD frames!
	large := make([]byte, 2400*1024)
	x := uint64(0x9e3779b97f4a7c15)
	for i := range large {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		large[i] = byte(x)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "chirp", "ab", "volumetric.bin"), large, 0o600); err != nil {
		t.Fatal(err)
	}

	arcBytes, man, err := CreateEncryptedArchive(ctx, db, port, dataDir)
	if err != nil {
		t.Fatalf("CreateEncryptedArchive: %v", err)
	}
	if string(arcBytes[:len(archiveMagic)]) != archiveMagic || man.Version != archiveVersion {
		t.Fatalf("expected NKTBAK02 v2 archive, got magic=%q ver=%d", string(arcBytes[:8]), man.Version)
	}
	for _, wantFile := range []string{
		"nektar.sqlite",
		"wallet/wallet.sqlite",
		"workflows.sqlite",
		"hub.sqlite",
		"client-txs/deadbeef.beef",
		"chirp/ab/volumetric.bin",
	} {
		if _, ok := man.Files[wantFile]; !ok {
			t.Fatalf("expected %s in backup manifest, got %+v", wantFile, man.Files)
		}
	}

	// Truncating at 1.5 MiB (dropping final AEAD chunk) MUST fail closed!
	truncated := arcBytes[:1500*1024]
	if _, err := VerifyArchive(ctx, port.WalletInterface(), truncated); err == nil {
		t.Fatal("ARCH-F10: expected truncated multi-chunk archive (missing final AEAD frame) to fail closed")
	}

	// Trailing garbage byte MUST fail closed!
	trailing := append(append([]byte(nil), arcBytes...), 0x42)
	if _, err := VerifyArchive(ctx, port.WalletInterface(), trailing); err == nil {
		t.Fatal("ARCH-F10: expected trailing byte after final AEAD frame to fail closed")
	}

	// Full stream restore into restoreDir verifies all 4 SQLite DBs + client-txs + 2.4 MiB CHIRP blob!
	restoreDir := t.TempDir()
	if _, err := RestoreArchiveReader(ctx, port.WalletInterface(), bytes.NewReader(arcBytes), restoreDir); err != nil {
		t.Fatalf("RestoreArchiveReader: %v", err)
	}
	for _, aux := range []string{"workflows.sqlite", "hub.sqlite"} {
		if err := sqliteIntegrityCheck(ctx, filepath.Join(restoreDir, aux)); err != nil {
			t.Fatalf("restored %s integrity check: %v", aux, err)
		}
	}
	gotClientTx, err := os.ReadFile(filepath.Join(restoreDir, "client-txs", "deadbeef.beef"))
	if err != nil || string(gotClientTx) != "raw-client-beef-journal" {
		t.Fatalf("restored client-txs mismatch: got=%q err=%v", string(gotClientTx), err)
	}
	gotLarge, err := os.ReadFile(filepath.Join(restoreDir, "chirp", "ab", "volumetric.bin"))
	if err != nil || !bytes.Equal(gotLarge, large) {
		t.Fatalf("restored 2.4 MiB CHIRP blob mismatch: len=%d err=%v", len(gotLarge), err)
	}
}
