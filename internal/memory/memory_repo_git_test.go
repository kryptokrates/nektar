package memory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestThreeWayMergeAutoMergeAndConflicts(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestMemoryService(t)

	// 1. Seed base file on main and anchor batch #0.
	baseText := strings.Join([]string{
		"line 1: title",
		"line 2: overview",
		"line 3: protocol",
		"line 4: details",
		"line 5: footer",
	}, "\n") + "\n"
	if _, err := svc.Remember(ctx, RememberInput{
		Shard:         "spec",
		Path:          "docs/spec.md",
		Text:          baseText,
		CommitMessage: "Initial spec",
		AutoAnchor:    true,
	}); err != nil {
		t.Fatalf("Remember base: %v", err)
	}

	// 2. Branch feat-clean from main; edit disjoint lines (line 1 on main, line 5 on feat-clean).
	if _, err := svc.CreateBranch(ctx, "spec", "feat-clean", "main"); err != nil {
		t.Fatalf("CreateBranch feat-clean: %v", err)
	}
	mainEdit := strings.Replace(baseText, "line 1: title", "line 1: Nektar Title", 1)
	if _, err := svc.Remember(ctx, RememberInput{
		Shard:         "spec",
		Branch:        "main",
		Path:          "docs/spec.md",
		Text:          mainEdit,
		CommitMessage: "Update title on main",
	}); err != nil {
		t.Fatalf("Remember main edit: %v", err)
	}
	featEdit := strings.Replace(baseText, "line 5: footer", "line 5: Signed BSV Footer", 1)
	if _, err := svc.Remember(ctx, RememberInput{
		Shard:         "spec",
		Branch:        "feat-clean",
		Path:          "docs/spec.md",
		Text:          featEdit,
		CommitMessage: "Update footer on feat-clean",
	}); err != nil {
		t.Fatalf("Remember feat-clean edit: %v", err)
	}

	mRes, err := svc.MergeBranchWithOptions(ctx, MergeBranchInput{
		Repo:         "spec",
		SourceBranch: "feat-clean",
		TargetBranch: "main",
		AutoAnchor:   true,
	})
	if err != nil {
		t.Fatalf("expected clean 3-way auto-merge, got err: %v", err)
	}
	if mRes.Commit == nil || len(mRes.Commit.Parents) != 2 {
		t.Fatalf("expected 2-parent merge commit, got %+v", mRes.Commit)
	}
	if len(mRes.AutoMerged) != 1 || mRes.AutoMerged[0] != "docs/spec.md" {
		t.Fatalf("expected docs/spec.md in AutoMerged, got %v", mRes.AutoMerged)
	}

	// Verify BOTH disjoint edits exist in merged main tree!
	_, tr, blobs, err := svc.RepoTree(ctx, "spec", "main", "")
	if err != nil || tr == nil || len(tr.Entries) != 1 {
		t.Fatalf("RepoTree after auto-merge failed: err=%v tr=%+v", err, tr)
	}
	mergedText := string(memrepo.ResolveBlobBytes(blobs[tr.Entries[0].BlobHash], svc.Objects()))
	if !strings.Contains(mergedText, "line 1: Nektar Title") || !strings.Contains(mergedText, "line 5: Signed BSV Footer") {
		t.Fatalf("merged content missing expected disjoint edits:\n%s", mergedText)
	}

	// 3. Now create overlapping conflict on line 3 across main and feat-conflict.
	if _, err := svc.CreateBranch(ctx, "spec", "feat-conflict", "main"); err != nil {
		t.Fatalf("CreateBranch feat-conflict: %v", err)
	}
	oursConf := strings.Replace(mergedText, "line 3: protocol", "line 3: protocol-v2-main", 1)
	theirsConf := strings.Replace(mergedText, "line 3: protocol", "line 3: protocol-v2-branch", 1)
	if _, err := svc.Remember(ctx, RememberInput{
		Shard: "spec", Branch: "main", Path: "docs/spec.md", Text: oursConf, CommitMessage: "Main protocol v2",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Remember(ctx, RememberInput{
		Shard: "spec", Branch: "feat-conflict", Path: "docs/spec.md", Text: theirsConf, CommitMessage: "Branch protocol v2",
	}); err != nil {
		t.Fatal(err)
	}

	confRes, err := svc.MergeBranchWithOptions(ctx, MergeBranchInput{
		Repo:         "spec",
		SourceBranch: "feat-conflict",
		TargetBranch: "main",
	})
	if !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("expected ErrMergeConflict, got err=%v res=%+v", err, confRes)
	}
	if confRes == nil || len(confRes.Conflicts) != 1 || confRes.Conflicts[0].Path != "docs/spec.md" {
		t.Fatalf("expected 1 conflict on docs/spec.md, got %+v", confRes)
	}
	if !strings.Contains(confRes.Conflicts[0].OursText, "protocol-v2-main") ||
		!strings.Contains(confRes.Conflicts[0].TheirsText, "protocol-v2-branch") {
		t.Fatalf("unexpected conflict texts: %+v", confRes.Conflicts[0])
	}

	// 4. Resolve conflict explicitly and anchor!
	resolved := strings.Replace(mergedText, "line 3: protocol", "line 3: protocol-v2-unified", 1)
	okRes, err := svc.MergeBranchWithOptions(ctx, MergeBranchInput{
		Repo:         "spec",
		SourceBranch: "feat-conflict",
		TargetBranch: "main",
		Resolutions:  map[string]string{"docs/spec.md": resolved},
		AutoAnchor:   true,
	})
	if err != nil {
		t.Fatalf("resolved 3-way merge failed: %v", err)
	}
	if okRes.AnchorTxID == "" {
		t.Fatalf("expected resolved merge to anchor via NektarAnchor.Append(), got empty AnchorTxID")
	}
	rep, err := svc.VerifyShardChain("spec")
	if err != nil || !rep.OK {
		t.Fatalf("VerifyShardChain(spec) failed after merges: err=%v rep=%+v", err, rep)
	}
}

func TestMyersDiffBlameDeleteRevertCherryPickForkTagAndTimeTravel(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestMemoryService(t)

	// Commit 1 (v1) + anchor #0
	r1, err := svc.Remember(ctx, RememberInput{
		Shard:         "arch",
		Path:          "notes/design.md",
		Text:          "alpha genesis fact\nbeta invariant\n",
		CommitMessage: "Genesis design notes",
		AutoAnchor:    true,
	})
	if err != nil {
		t.Fatalf("Remember v1: %v", err)
	}

	// Create immutable checkpoint tag on Commit 1.
	tg, err := svc.CreateTag(ctx, "arch", "v1.0-audit", r1.CommitHash[:8], "Initial audit baseline")
	if err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	if tg.CommitHash != r1.CommitHash {
		t.Fatalf("tag commit mismatch: got %s want %s", tg.CommitHash, r1.CommitHash)
	}
	// Duplicate immutable tag MUST fail!
	if _, err := svc.CreateTag(ctx, "arch", "v1.0-audit", r1.CommitHash, "overwrite attempt"); err == nil {
		t.Fatal("expected duplicate tag creation to fail")
	}

	// Commit 2 (v2) modifies line 2 and adds line 3, then anchor #1.
	r2, err := svc.Remember(ctx, RememberInput{
		Shard:         "arch",
		Path:          "notes/design.md",
		Text:          "alpha genesis fact\nbeta updated invariant\ngamma third line\n",
		CommitMessage: "Add gamma invariant",
		AutoAnchor:    true,
	})
	if err != nil {
		t.Fatalf("Remember v2: %v", err)
	}

	// 1. Myers Unified Diff via go-git Tree.Patch
	patch, err := svc.RepoDiffPatch(ctx, "arch", r1.CommitHash, r2.CommitHash)
	if err != nil {
		t.Fatalf("RepoDiffPatch: %v", err)
	}
	if len(patch) != 1 || patch[0].Path != "notes/design.md" || patch[0].Additions < 2 || patch[0].Deletions < 1 {
		t.Fatalf("unexpected Myers patch: %+v", patch)
	}

	// 2. Cryptographic + BSV UTXO Blame via go-git git.Blame
	blame, err := svc.RepoBlame(ctx, "arch", "main", "", "notes/design.md")
	if err != nil {
		t.Fatalf("RepoBlame: %v", err)
	}
	if len(blame) != 3 {
		t.Fatalf("expected 3 blame lines, got %d: %+v", len(blame), blame)
	}
	if blame[0].CommitHash != r1.CommitHash || blame[0].AnchorTxID == "" {
		t.Fatalf("line 1 should blame commit 1 with anchor UTXO, got %+v", blame[0])
	}
	if blame[2].CommitHash != r2.CommitHash || blame[2].AnchorTxID == "" {
		t.Fatalf("line 3 should blame commit 2 with anchor UTXO, got %+v", blame[2])
	}

	// 3. Epistemic Time-Travel Recall pinned to immutable tag v1.0-audit (must NOT see "gamma"!)
	past, err := svc.Recall(ctx, RecallInput{
		Repo:     "arch",
		AtCommit: "v1.0-audit",
		Query:    "gamma",
	})
	if err != nil {
		t.Fatalf("Recall(AtCommit=v1.0-audit, gamma): %v", err)
	}
	if len(past) != 0 {
		t.Fatalf("time-travel recall leaked future content 'gamma': %+v", past)
	}
	pastHit, err := svc.Recall(ctx, RecallInput{
		Repo:     "arch",
		AtCommit: "v1.0-audit",
		Query:    "alpha",
	})
	if err != nil || len(pastHit) != 1 || pastHit[0].Path != "notes/design.md" {
		t.Fatalf("time-travel recall missed historical 'alpha': err=%v res=%+v", err, pastHit)
	}

	// 4. Non-destructive git rm (DeleteFile) + RevertCommit restoring the deleted file!
	delC, err := svc.DeleteFile(ctx, "arch", "main", "notes/design.md", "Retire design doc", true, nil)
	if err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	_, trDel, _, err := svc.RepoTree(ctx, "arch", "main", "")
	if err != nil || len(trDel.Entries) != 0 {
		t.Fatalf("expected empty tree after git rm, got err=%v entries=%+v", err, trDel)
	}
	revRes, err := svc.RevertCommit(ctx, "arch", "main", delC.CommitHash, true, nil)
	if err != nil || revRes.Commit == nil {
		t.Fatalf("RevertCommit: err=%v res=%+v", err, revRes)
	}
	_, trRestored, _, err := svc.RepoTree(ctx, "arch", "main", "")
	if err != nil || len(trRestored.Entries) != 1 || trRestored.Entries[0].Path != "notes/design.md" {
		t.Fatalf("expected notes/design.md restored after revert, got %+v", trRestored)
	}

	// 5. CherryPickCommit from an experimental branch onto main.
	if _, err := svc.CreateBranch(ctx, "arch", "exp", "main"); err != nil {
		t.Fatal(err)
	}
	cpSource, err := svc.Remember(ctx, RememberInput{
		Shard: "arch", Branch: "exp", Path: "notes/cherry.md", Text: "cherry picked wisdom", CommitMessage: "Add cherry",
	})
	if err != nil {
		t.Fatal(err)
	}
	cpRes, err := svc.CherryPickCommit(ctx, "arch", "main", cpSource.CommitHash, true, nil)
	if err != nil || cpRes.Commit == nil {
		t.Fatalf("CherryPickCommit: err=%v res=%+v", err, cpRes)
	}
	_, trAfterCP, _, err := svc.RepoTree(ctx, "arch", "main", "")
	if err != nil || len(trAfterCP.Entries) != 2 {
		t.Fatalf("expected 2 files on main after cherry-pick, got %+v", trAfterCP)
	}

	// 6. ForkRepo creates isolated repo with distinct BRC-43 GenesisShardID and parent link!
	srcSum, err := svc.GetRepoSummary(ctx, "arch")
	if err != nil {
		t.Fatal(err)
	}
	forkSum, err := svc.ForkRepo(ctx, "arch", "main", "arch-fork", true, nil)
	if err != nil {
		t.Fatalf("ForkRepo: %v", err)
	}
	if forkSum.GenesisShardID == "" || forkSum.GenesisShardID == srcSum.GenesisShardID {
		t.Fatalf("expected distinct GenesisShardID for fork, src=%s fork=%s", srcSum.GenesisShardID, forkSum.GenesisShardID)
	}
	forkLog, err := svc.RepoLog(ctx, "arch-fork", "main", 5)
	if err != nil || len(forkLog) != 1 || len(forkLog[0].Parents) != 1 || forkLog[0].Parents[0].RepoGenesis != srcSum.GenesisShardID {
		t.Fatalf("expected fork genesis commit to link parent RepoGenesis=%s, got %+v", srcSum.GenesisShardID, forkLog)
	}
}

func TestInspectCapsuleReadOnlyAndFailClosed(t *testing.T) {
	ctx := context.Background()
	shared := chain.NewSharedMemoryChain()

	openSvc := func() *Service {
		t.Helper()
		port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir(), SharedMemoryChain: shared})
		if err != nil {
			t.Fatal(err)
		}
		db, err := store.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		svc, err := New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), Config{})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}

	alice := openSvc()
	bob := openSvc()

	if _, err := alice.Remember(ctx, RememberInput{
		Shard:         "vault",
		Path:          "facts/utxo.md",
		Text:          "Every UTXO spend proves unbroken lineage.",
		CommitMessage: "Seed vault",
		AutoAnchor:    true,
	}); err != nil {
		t.Fatal(err)
	}
	_, capObj, raw, err := alice.ExportCapsule(ctx, "vault", "main", "")
	if err != nil {
		t.Fatalf("ExportCapsule: %v", err)
	}

	// 1. Read-only InspectCapsule on Bob MUST verify OK and mutate ZERO local repos!
	insp, err := bob.InspectCapsule(ctx, raw, "")
	if err != nil {
		t.Fatalf("InspectCapsule(valid): %v", err)
	}
	if insp.Verification == nil || !insp.Verification.OK {
		t.Fatalf("expected valid capsule verification, got %+v", insp.Verification)
	}
	if insp.LocalExists || len(insp.Commits) != 1 || insp.Tree == nil || len(insp.Tree.Entries) != 1 {
		t.Fatalf("unexpected inspect preview: %+v", insp)
	}
	bobRepos, err := bob.ListRepos(ctx)
	if err != nil || len(bobRepos) != 0 {
		t.Fatalf("InspectCapsule mutated Bob's SQLite state! repos=%+v err=%v", bobRepos, err)
	}

	// 2. Tampered capsule MUST fail closed!
	tampered := *capObj
	tamperedCommits := append([]memrepo.CommitObject(nil), capObj.Commits...)
	tamperedCommits[0].Message = "FORGED MESSAGE"
	tampered.Commits = tamperedCommits
	badRaw, err := memrepo.EncodeCapsule(&tampered)
	if err != nil {
		t.Fatal(err)
	}
	badInsp, err := bob.InspectCapsule(ctx, badRaw, "")
	if err != nil {
		t.Fatalf("InspectCapsule(tampered) unexpected error: %v", err)
	}
	if badInsp.Verification == nil || badInsp.Verification.OK {
		t.Fatalf("tampered capsule MUST fail verification, got %+v", badInsp.Verification)
	}
}
