package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestShardValidationAndPackVerificationHardening(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	objStore, err := bstack.NewFileObjectStore(filepath.Join(t.TempDir(), "chirp"))
	if err != nil {
		t.Fatal(err)
	}
	mem, err := New(db, port, bstack.NewGroupRegistry(), objStore, Config{})
	if err != nil {
		t.Fatal(err)
	}

	// 1. ValidateShardName blocks reserved __receipts__ and path traversal.
	for _, bad := range []string{"", ReceiptsShard, "../escape", "/leading", "trailing/", "bad shard!"} {
		if err := ValidateShardName(bad, false); err == nil {
			t.Errorf("expected ValidateShardName(%q, false) to fail", bad)
		}
	}
	if _, err := mem.Remember(ctx, RememberInput{Shard: ReceiptsShard, Text: "spoofed receipt"}); err == nil {
		t.Fatal("Remember into __receipts__ must be rejected")
	}
	if _, err := mem.Remember(ctx, RememberInput{Shard: "../evil", Text: "escape"}); err == nil {
		t.Fatal("Remember with path traversal shard name must be rejected")
	}

	// 2. FileObjectStore rejects hash-mismatched CHIRP puts.
	var wrongHash [32]byte
	wrongHash[0] = 0x42
	if err := objStore.PutObject(wrongHash, []byte("tampered payload")); err == nil {
		t.Fatal("expected FileObjectStore.PutObject to reject hash mismatch")
	}
	goodData := []byte("verified chirp object")
	goodHash := sha256.Sum256(goodData)
	if err := objStore.PutObject(goodHash, goodData); err != nil {
		t.Fatalf("valid PutObject failed: %v", err)
	}

	// 3. ExportShardPack (.nkt Capsule) -> tamper fragment text / strip BEEF / legacy flat JSON -> Import MUST fail closed!
	if _, err := mem.Remember(ctx, RememberInput{
		Shard:      "clean",
		Text:       "original verified memory fragment",
		AutoAnchor: true,
	}); err != nil {
		t.Fatal(err)
	}
	exp, err := mem.ExportShardPack(ctx, "clean")
	if err != nil {
		t.Fatalf("ExportShardPack: %v", err)
	}
	packRaw, err := bstack.ResolveChirp(objStore, exp.ChirpURL)
	if err != nil {
		t.Fatalf("ResolveChirp: %v", err)
	}
	capObj, err := memrepo.DecodeCapsule(packRaw)
	if err != nil {
		t.Fatalf("DecodeCapsule: %v", err)
	}

	// Tamper fragment Data while keeping original HDP/Leaf -> capsule verification MUST fail closed!
	tampered := *capObj
	tampered.Fragments = append([]store.FragmentRecord(nil), capObj.Fragments...)
	tampered.Fragments[0].Data = []byte("FORGED TEXT")
	tamperedRaw, _ := memrepo.EncodeCapsule(&tampered)

	db2, _ := store.Open(":memory:")
	defer db2.Close()
	port2, _ := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	mem2, _ := New(db2, port2, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), Config{})

	if _, _, err := mem2.ImportShardPackBytesCtx(ctx, tamperedRaw, "imported-tampered"); err == nil {
		t.Fatal("expected tampered capsule import to fail closed")
	}
	if tamperedFrags, _ := db2.ListFragments("imported-tampered"); len(tamperedFrags) != 0 {
		t.Fatalf("expected 0 persisted fragments after rejected tampered capsule, got %d", len(tamperedFrags))
	}

	// Legacy flat JSON ShardPack v1 MUST be rejected!
	if _, _, err := mem2.ImportShardPackBytesCtx(ctx, []byte(`{"version":1,"shard":"clean","fragments":[]}`), "legacy-shard"); err == nil {
		t.Fatal("expected legacy flat JSON ShardPack v1 to be rejected")
	}

	// Import into reserved __receipts__ must fail!
	if _, _, err := mem2.ImportShardPackBytesCtx(ctx, packRaw, ReceiptsShard); err == nil {
		t.Fatal("expected ImportShardPackBytesCtx into __receipts__ to fail")
	}

	// Forged Settled=true with stripped BEEF MUST fail closed (anchors without AtomicBEEF cannot be verified)!
	unsettled := *capObj
	unsettled.Anchors = append([]store.BatchAnchor(nil), capObj.Anchors...)
	if len(unsettled.Anchors) > 0 {
		unsettled.Anchors[0].Settled = true
		unsettled.Anchors[0].Beef = nil
	}
	unsettledRaw, _ := memrepo.EncodeCapsule(&unsettled)
	if _, _, err := mem2.ImportShardPackBytesCtx(ctx, unsettledRaw, "imported-unsettled"); err == nil {
		t.Fatal("expected capsule with stripped anchor BEEF to fail closed")
	}

	// 4. Encrypted Borg group epoch key round-trip survives DB reload and is NOT plaintext JSON in SQLite!
	if _, err := mem.Groups().CreateGroup("vault-sec", "Vault Sec", port.IdentityPubHex()); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	groups, err := db.ListBorgGroups()
	if err != nil || len(groups) != 1 {
		t.Fatalf("ListBorgGroups: len=%d err=%v", len(groups), err)
	}
	rawEpochs := string(groups[0].EpochsJSON)
	if rawEpochs == "" || rawEpochs[0] == '[' || !json.Valid([]byte(rawEpochs)) {
		t.Fatalf("expected borg_groups.epochs_json to be encrypted JSON envelope at rest, got: %s", rawEpochs)
	}
	var sealedCheck sealedEpochsBlob
	if err := json.Unmarshal([]byte(rawEpochs), &sealedCheck); err != nil || sealedCheck.EncHex == "" {
		t.Fatalf("expected sealed encHex envelope in borg_groups.epochs_json, got: %s", rawEpochs)
	}
	// Reload into a fresh GroupRegistry on the SAME db + chain port -> must decrypt seamlessly!
	memReload, err := New(db, port, bstack.NewGroupRegistry(), objStore, Config{})
	if err != nil {
		t.Fatalf("reload memory service: %v", err)
	}
	if _, err := memReload.Remember(ctx, RememberInput{
		Shard:     "vault-reloaded",
		Text:      "post-restart encrypted secret",
		BorgGroup: "vault-sec",
	}); err != nil {
		t.Fatalf("expected encrypted Borg group to reload seamlessly after restart: %v", err)
	}
	reloadedRec, err := memReload.Recall(ctx, RecallInput{Shard: "vault-reloaded", CallerPub: port.IdentityPubHex()})
	if err != nil || len(reloadedRec) != 1 || reloadedRec[0].Text != "post-restart encrypted secret" {
		t.Fatalf("expected reloaded Borg group to decrypt seamlessly, got %+v err=%v", reloadedRec, err)
	}

	// 5. Public CHIRP allowlist works as expected.
	if mem.IsPublicChirp(goodHash) {
		t.Fatal("expected un-allowlisted CHIRP hash to be private")
	}
	mem.AllowPublicChirpHashes(goodHash)
	if !mem.IsPublicChirp(goodHash) {
		t.Fatalf("expected allowlisted CHIRP hash %s to be public", hex.EncodeToString(goodHash[:]))
	}
}
