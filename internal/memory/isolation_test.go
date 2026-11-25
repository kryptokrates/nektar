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

	// 3. ExportShardPack -> tamper fragment text / fake Settled -> ImportShardPack must fail!
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
	var pack ShardPack
	if err := json.Unmarshal(packRaw, &pack); err != nil {
		t.Fatalf("unmarshal pack: %v", err)
	}

	// Tamper fragment Data while keeping original HDP/Leaf -> anchor status MUST be stripped (BatchN=-1, AnchorTxID="")!
	tampered := pack
	tampered.Fragments = append([]store.FragmentRecord(nil), pack.Fragments...)
	tampered.Fragments[0].Data = []byte("FORGED TEXT")
	tamperedJSON, _ := json.Marshal(tampered)

	db2, _ := store.Open(":memory:")
	defer db2.Close()
	port2, _ := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	mem2, _ := New(db2, port2, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), Config{})

	if _, _, err := mem2.importShardPackVerified(ctx, tamperedJSON, "imported-tampered"); err != nil {
		t.Fatalf("importShardPackVerified tampered: %v", err)
	}
	tamperedFrags, err := db2.ListFragments("imported-tampered")
	if err != nil || len(tamperedFrags) != 1 {
		t.Fatalf("ListFragments imported-tampered: len=%d err=%v", len(tamperedFrags), err)
	}
	if tamperedFrags[0].BatchN != -1 || tamperedFrags[0].AnchorTxID != "" || !tamperedFrags[0].Tainted {
		t.Fatalf("expected tampered fragment to lose anchored status and remain tainted, got %+v", tamperedFrags[0])
	}

	// Import into reserved __receipts__ must fail!
	if _, _, err := mem2.importShardPackVerified(ctx, packRaw, ReceiptsShard); err == nil {
		t.Fatal("expected importShardPackVerified into __receipts__ to fail")
	}

	// Forged Settled=true without a valid BUMP/BEEF must NEVER be trusted as settled!
	unsettledPack := pack
	unsettledPack.Anchors = append([]store.BatchAnchor(nil), pack.Anchors...)
	if len(unsettledPack.Anchors) > 0 {
		unsettledPack.Anchors[0].Settled = true
		unsettledPack.Anchors[0].Beef = nil // stripped proof
	}
	unsettledJSON, _ := json.Marshal(unsettledPack)
	n, _, err := mem2.importShardPackVerified(ctx, unsettledJSON, "imported-unsettled")
	if err != nil {
		t.Fatalf("importShardPackVerified with stripped BEEF should import fragments but skip unverified anchors, got err=%v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 imported fragment, got %d", n)
	}
	anchors, err := db2.ListAnchors("imported-unsettled")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range anchors {
		if a.Settled {
			t.Fatalf("imported anchor without verified BEEF/BUMP must NOT be marked Settled=true: %+v", a)
		}
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
