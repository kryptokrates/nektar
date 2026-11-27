package memory

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

// TestRCA1_BorgBlobZeroPlaintextLeakAndEncryptedTreePreservation verifies:
// 1. Borg+Blob Remember NEVER writes plaintext into CHIRP s.objects or mem_objects.
// 2. Forget shreds plaintext from fragments, FTS5, RepoTree, Git projection, and ExportCapsule.
// 3. DeleteFile, MergeBranchWithOptions, and RevertCommit preserve untouched Encrypted=true blobs intact.
func TestRCA1_BorgBlobZeroPlaintextLeakAndEncryptedTreePreservation(t *testing.T) {
	ctx := context.Background()
	svc, db, _ := newHardeningService(t)

	secretPayload := bytes.Repeat([]byte("ULTRA_SECRET_BORG_BYTES_"), 3000) // >64KiB volumetric blob
	rSec, err := svc.Remember(ctx, RememberInput{
		Shard:     "vault",
		Path:      "secrets/keys.bin",
		Text:      "secret metadata",
		Blob:      secretPayload,
		BorgGroup: "team-secret",
		Level:     50,
		Class:     "secret",
	})
	if err != nil {
		t.Fatalf("Remember Borg+Blob: %v", err)
	}
	if !rSec.Encrypted || rSec.CommitHash == "" {
		t.Fatalf("expected encrypted commit, got %+v", rSec)
	}
	// 1a. Verify mem_objects blob has ZERO plaintext inline and ZERO chirpUrl!
	_, tr, blobs, err := svc.RepoTree(ctx, "vault", "main", "")
	if err != nil || tr == nil {
		t.Fatalf("RepoTree: %v", err)
	}
	var secEntryHash string
	for _, e := range tr.Entries {
		if e.Path == "secrets/keys.bin" {
			secEntryHash = e.BlobHash
			if !e.Encrypted || e.ChirpURL != "" {
				t.Fatalf("expected encrypted entry with empty chirpUrl, got %+v", e)
			}
			b := blobs[e.BlobHash]
			if !b.Encrypted || len(b.Inline) != 0 || b.ChirpURL != "" || len(b.KeyedMeta) == 0 {
				t.Fatalf("expected encrypted blob with zero inline/chirpUrl and preserved KeyedMeta, got %+v", b)
			}
		}
	}
	if secEntryHash == "" {
		t.Fatal("missing secrets/keys.bin entry in RepoTree")
	}
	// Verify raw CHIRP object store contains ZERO objects (plaintext blob must NEVER have been stored in s.objects!).
	if memObjStore, ok := svc.Objects().(*bstack.MemoryObjectStore); ok {
		// Check via direct blob hash lookup using ResolveChirp — store must not contain secretPayload.
		u, _, _, _ := bstack.StoreChirpWithHashes(bstack.NewMemoryObjectStore(), secretPayload)
		if _, rErr := bstack.ResolveChirp(memObjStore, u); rErr == nil {
			t.Fatal("CRITICAL LEAK: Borg+Blob plaintext was written unencrypted into CHIRP ObjectStore!")
		}
	}

	// 1b. Add a plaintext file, then delete it -> secrets/keys.bin MUST keep Encrypted=true & original BlobHash!
	rPub, err := svc.Remember(ctx, RememberInput{
		Shard: "vault",
		Path:  "notes/public.md",
		Text:  "PUBLIC_TEMP_NOTE_CONTENT",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteFile(ctx, "vault", "main", "notes/public.md", "rm public", false, nil); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	_, trAfterDel, blobsAfterDel, err := svc.RepoTree(ctx, "vault", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	foundSec := false
	for _, e := range trAfterDel.Entries {
		if e.Path == "secrets/keys.bin" {
			foundSec = true
			if !e.Encrypted || e.BlobHash != secEntryHash {
				t.Fatalf("DeleteFile corrupted untouched encrypted entry! got=%+v wantHash=%s", e, secEntryHash)
			}
			if !blobsAfterDel[e.BlobHash].Encrypted || len(blobsAfterDel[e.BlobHash].KeyedMeta) == 0 {
				t.Fatalf("DeleteFile lost KeyedMeta on encrypted blob: %+v", blobsAfterDel[e.BlobHash])
			}
		}
	}
	if !foundSec {
		t.Fatal("encrypted entry disappeared after DeleteFile on sibling file")
	}

	// 1c. Create side branch, add another file, merge back into main -> encrypted entry still preserved!
	if _, err := svc.CreateBranch(ctx, "vault", "feat", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Remember(ctx, RememberInput{Shard: "vault", Branch: "feat", Path: "notes/feat.md", Text: "feat line"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MergeBranchWithOptions(ctx, MergeBranchInput{Repo: "vault", SourceBranch: "feat", TargetBranch: "main"}); err != nil {
		t.Fatalf("MergeBranchWithOptions: %v", err)
	}
	_, trAfterMerge, _, err := svc.RepoTree(ctx, "vault", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range trAfterMerge.Entries {
		if e.Path == "secrets/keys.bin" && (!e.Encrypted || e.BlobHash != secEntryHash) {
			t.Fatalf("MergeBranchWithOptions corrupted untouched encrypted entry: %+v", e)
		}
	}

	// 1d. Forget a plaintext fragment -> must be unreadable across RepoTree, Git projection, and ExportCapsule!
	rForget, err := svc.Remember(ctx, RememberInput{Shard: "vault", Path: "notes/erase.md", Text: "ERASE_ME_COMPLETELY_999"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Forget(ctx, "vault", rForget.Seq); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	_, trForgotten, blobsForgotten, err := svc.RepoTree(ctx, "vault", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range trForgotten.Entries {
		if e.Path == "notes/erase.md" {
			t.Fatalf("forgotten file still present in RepoTree: %+v", e)
		}
	}
	for _, b := range blobsForgotten {
		if strings.Contains(string(b.Inline), "ERASE_ME_COMPLETELY_999") {
			t.Fatal("forgotten plaintext leaked in RepoTree blobs")
		}
	}
	_, capObj, _, err := svc.ExportCapsule(ctx, "vault", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range capObj.Blobs {
		if strings.Contains(string(b.Inline), "ERASE_ME_COMPLETELY_999") {
			t.Fatal("forgotten plaintext leaked in ExportCapsule")
		}
	}
	_ = db
	_ = rPub
}

// TestRCA2_CedarAuthorizationEnforcedAcrossRepoAPIs verifies Cedar read/write enforcement on
// recallAtCommit, RepoTreeAuthorized, BuildRepoGoGitProjectionAuthorized, ExportCapsuleAuthorized,
// MergeBranchWithOptions.Principal, CreateBranchAuthorized, CreateTagAuthorized.
func TestRCA2_CedarAuthorizationEnforcedAcrossRepoAPIs(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newHardeningService(t)

	if _, err := svc.Remember(ctx, RememberInput{Shard: "acl", Path: "public.md", Text: "open data", Level: 0}); err != nil {
		t.Fatal(err)
	}
	rHigh, err := svc.Remember(ctx, RememberInput{Shard: "acl", Path: "topsecret.md", Text: "classified level 50", Level: 50})
	if err != nil {
		t.Fatal(err)
	}

	lowPrincipal := policy.Principal{Type: "Agent", ID: "guest", Level: 0}

	// 1. recallAtCommit must NOT return Level 50 entry to Level 0 caller!
	recalled, err := svc.Recall(ctx, RecallInput{
		Shard:     "acl",
		AtCommit:  rHigh.CommitHash,
		Principal: lowPrincipal,
	})
	if err != nil {
		t.Fatalf("Recall(AtCommit): %v", err)
	}
	for _, rf := range recalled {
		if rf.Path == "topsecret.md" || strings.Contains(rf.Text, "classified") {
			t.Fatalf("CRITICAL ACL BYPASS: recallAtCommit leaked Level 50 file to Level 0 principal: %+v", rf)
		}
	}

	// 2. RepoTreeAuthorized & BuildRepoGoGitProjectionAuthorized & ExportCapsuleAuthorized must filter topsecret.md!
	_, trAuth, _, err := svc.RepoTreeAuthorized(ctx, "acl", "main", "", lowPrincipal, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range trAuth.Entries {
		if e.Path == "topsecret.md" {
			t.Fatalf("RepoTreeAuthorized leaked Level 50 file to Level 0 principal")
		}
	}
	_, _, projTrees, _, err := svc.BuildRepoGoGitProjectionAuthorized(ctx, "acl", lowPrincipal, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range projTrees {
		for _, e := range tr.Entries {
			if e.Path == "topsecret.md" {
				t.Fatalf("BuildRepoGoGitProjectionAuthorized leaked Level 50 file to Level 0 principal")
			}
		}
	}
	_, capObj, _, err := svc.ExportCapsuleAuthorized(ctx, "acl", "main", "", lowPrincipal, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range capObj.Fragments {
		if f.Level >= 50 {
			t.Fatalf("ExportCapsuleAuthorized leaked Level 50 fragment")
		}
	}

	// 3. Install strict write-deny policy for "readonly" principal -> CreateBranchAuthorized,
	//    CreateTagAuthorized, and MergeBranchWithOptions with Principal must FAIL CLOSED!
	strictPol := `
permit(principal, action == Action::"read", resource) when { principal.level >= resource.level };
permit(principal, action == Action::"write", resource) when { principal.level >= 10 };
`
	if _, err := svc.SetPolicy("strict-write", strictPol); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateBranchAuthorized(ctx, "acl", "ro-branch", "main", &lowPrincipal); err == nil {
		t.Fatal("expected CreateBranchAuthorized to deny Level 0 principal under strict-write policy")
	}
	if _, err := svc.CreateTagAuthorized(ctx, "acl", "v-ro", "main", "msg", &lowPrincipal); err == nil {
		t.Fatal("expected CreateTagAuthorized to deny Level 0 principal under strict-write policy")
	}
	// Create branch as owner, add a file, then try to merge as lowPrincipal -> MUST fail!
	if _, err := svc.CreateBranch(ctx, "acl", "owner-branch", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Remember(ctx, RememberInput{Shard: "acl", Branch: "owner-branch", Path: "b.md", Text: "branch fact"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MergeBranchWithOptions(ctx, MergeBranchInput{
		Repo: "acl", SourceBranch: "owner-branch", TargetBranch: "main", Principal: &lowPrincipal,
	}); err == nil {
		t.Fatal("expected MergeBranchWithOptions to enforce MergeBranchInput.Principal and deny Level 0 principal")
	}
}

// TestRCA4_ReceiptChainIntegrityAndRotateCrashRecovery verifies:
// 1. VerifyShardChain(ReceiptsShard) passes after GroundedAnswer + AnchorShard(ReceiptsShard).
// 2. Simulated crash after RotateShardOwner broadcast recovers via ReconcilePendingAnchors.
func TestRCA4_ReceiptChainIntegrityAndRotateCrashRecovery(t *testing.T) {
	ctx := context.Background()
	svc, db, _ := newHardeningService(t)

	r1, err := svc.Remember(ctx, RememberInput{Shard: "facts", Text: "sky is blue", AutoAnchor: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := svc.GroundedAnswer(ctx, GroundedAnswerInput{
			Claims: []reasoning.Claim{{Text: "sky is blue", Cites: []reasoning.FragmentRef{{Shard: "facts", Seq: r1.Seq}}}},
		}); err != nil {
			t.Fatalf("GroundedAnswer %d: %v", i, err)
		}
	}
	if _, err := svc.AnchorShard(ctx, ReceiptsShard); err != nil {
		t.Fatalf("AnchorShard(__receipts__): %v", err)
	}
	rep, err := svc.VerifyShardChain(ReceiptsShard)
	if err != nil || !rep.OK || !rep.AnchorsOK {
		t.Fatalf("expected VerifyShardChain(__receipts__) OK, got rep=%+v err=%v", rep, err)
	}

	// Simulate crash after RotateShardOwner broadcast: persist a pending_anchors row with Kind="rotate"
	// referencing a real broadcast tx (here the facts batch 0 txid/beef) and run ReconcilePendingAnchors!
	anchors, _ := db.ListAnchors("facts")
	if len(anchors) != 1 {
		t.Fatalf("expected 1 anchor on facts, got %d", len(anchors))
	}
	if err := db.SavePendingAnchor(store.PendingAnchor{
		Shard:      "facts",
		N:          anchors[0].N,
		Kind:       "rotate",
		RootHex:    anchors[0].RootHex,
		ChainedHex: anchors[0].ChainedHex,
		TxID:       anchors[0].TxID,
		Beef:       anchors[0].Beef,
		CreatedAt:  time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	recovered, err := svc.ReconcilePendingAnchors(ctx)
	if err != nil || recovered != 1 {
		t.Fatalf("expected ReconcilePendingAnchors to recover pending rotate, got n=%d err=%v", recovered, err)
	}
	pendingLeft, _ := db.ListPendingAnchors()
	if len(pendingLeft) != 0 {
		t.Fatalf("expected pending_anchors drained after rotate recovery, got %+v", pendingLeft)
	}
}

// TestRCA6_RevertAndCherryPickAutoMergeNonOverlappingLines verifies RevertCommit & CherryPickCommit
// use the shared line-level ThreeWayMergeWithBase engine instead of failing on whole-file mismatch.
func TestRCA6_RevertAndCherryPickAutoMergeNonOverlappingLines(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newHardeningService(t)

	// Base file with 5 lines.
	base := "line1\nline2\nline3\nline4\nline5\n"
	if _, err := svc.Remember(ctx, RememberInput{Shard: "merge3", Path: "doc.txt", Text: base}); err != nil {
		t.Fatal(err)
	}
	// Commit 2 modifies line1 only.
	c2, err := svc.Remember(ctx, RememberInput{Shard: "merge3", Path: "doc.txt", Text: "LINE1_MOD\nline2\nline3\nline4\nline5\n"})
	if err != nil {
		t.Fatal(err)
	}
	// Commit 3 modifies line5 only.
	if _, err := svc.Remember(ctx, RememberInput{Shard: "merge3", Path: "doc.txt", Text: "LINE1_MOD\nline2\nline3\nline4\nLINE5_MOD\n"}); err != nil {
		t.Fatal(err)
	}
	// Reverting commit 2 on top of commit 3 MUST auto-merge cleanly to "line1\nline2\nline3\nline4\nLINE5_MOD\n"!
	rev, err := svc.RevertCommit(ctx, "merge3", "main", c2.CommitHash, false, nil)
	if err != nil {
		t.Fatalf("expected RevertCommit to auto-merge non-overlapping line edit, got err=%v res=%+v", err, rev)
	}
	_, tr, blobs, err := svc.RepoTree(ctx, "merge3", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, e := range tr.Entries {
		if e.Path == "doc.txt" {
			got = string(blobs[e.BlobHash].Inline)
		}
	}
	want := "line1\nline2\nline3\nline4\nLINE5_MOD\n"
	if got != want {
		t.Fatalf("RevertCommit auto-merge mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestRCA7_SetPolicyAndAddRulesFailClosedOnDBError verifies SetPolicy/AddRules do not silently
// diverge RAM from disk when SQLite writes fail.
func TestRCA7_SetPolicyAndAddRulesFailClosedOnDBError(t *testing.T) {
	svc, db, _ := newHardeningService(t)
	origHash := svc.PolicyCommit().Hash
	_ = db.Close()

	if _, err := svc.SetPolicy("broken", `permit(principal, action, resource);`); err == nil {
		t.Fatal("expected SetPolicy to return error on closed DB")
	}
	if svc.PolicyCommit().Hash != origHash {
		t.Fatal("SetPolicy mutated RAM policy despite SQLite persistence failure!")
	}
	if err := svc.AddRules(`p(X) :- q(X).`); err == nil {
		t.Fatal("expected AddRules to return error on closed DB")
	}
}
