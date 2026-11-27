package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
)

// ErrMergeConflict is returned when a 3-way merge detects overlapping edits that require resolution.
var ErrMergeConflict = errors.New("memory: 3-way merge conflict")

// MergeBranchInput configures a true LCA 3-way Git merge between two branches.
type MergeBranchInput struct {
	Repo         string            `json:"repo"`
	SourceBranch string            `json:"sourceBranch"`
	TargetBranch string            `json:"targetBranch,omitempty"`
	Message      string            `json:"message,omitempty"`
	Strategy     string            `json:"strategy,omitempty"` // "" (3-way fail on conflict) | "ours" | "theirs"
	Resolutions  map[string]string `json:"resolutions,omitempty"`
	AutoAnchor   bool              `json:"autoAnchor,omitempty"`
	Principal    *policy.Principal `json:"principal,omitempty"`
}

// MergeBranchResult describes the outcome of a 3-way merge or cherry-pick.
type MergeBranchResult struct {
	Commit         *memrepo.CommitObject   `json:"commit,omitempty"`
	BaseCommitHash string                  `json:"baseCommitHash,omitempty"`
	AutoMerged     []string                `json:"autoMerged,omitempty"`
	Conflicts      []memrepo.MergeConflict `json:"conflicts,omitempty"`
	AnchorTxID     string                  `json:"anchorTxid,omitempty"`
}

// collectStoredTreeMeta loads the REAL persisted TreeObjects from SQLite (preserving true Encrypted/CLKA
// flags, BlobHash, ContentSHA256, and CHIRP URLs instead of ephemeral decrypted go-git projection entries).
// Returns both per-path highest-level metadata (for re-sealing modified files at the right level/class)
// and per-(path, contentSha256) exact entries so unchanged CLKA blobs are reused byte-for-byte on merge/revert/cherry-pick.
func (s *Service) collectStoredTreeMeta(fallback map[string]memrepo.TreeObject, hashes ...string) (map[string]memrepo.TreeEntry, map[string]memrepo.TreeEntry) {
	byPath := make(map[string]memrepo.TreeEntry)
	byContent := make(map[string]memrepo.TreeEntry)
	for _, h := range hashes {
		if h == "" {
			continue
		}
		var tr memrepo.TreeObject
		if loaded, err := s.loadTreeObject(h); err == nil && loaded != nil {
			tr = *loaded
		} else if fb, ok := fallback[h]; ok {
			tr = fb
		} else {
			continue
		}
		for _, e := range tr.Entries {
			if prev, exists := byPath[e.Path]; !exists || e.Level > prev.Level || (e.Encrypted && !prev.Encrypted) {
				byPath[e.Path] = e
			}
			if e.ContentSHA256 != "" && e.BlobHash != "" {
				key := e.Path + "\x00" + strings.ToLower(e.ContentSHA256)
				if prev, exists := byContent[key]; !exists || (e.Encrypted && !prev.Encrypted) {
					byContent[key] = e
				}
			}
		}
	}
	return byPath, byContent
}

// MergeBranchWithOptions performs a true LCA 3-way merge using go-git/v5 Commit.MergeBase().
// Non-overlapping edits auto-merge cleanly; overlapping edits return MergeBranchResult{Conflicts} + ErrMergeConflict
// unless resolved via in.Resolutions or in.Strategy ("ours" | "theirs").
func (s *Service) MergeBranchWithOptions(ctx context.Context, in MergeBranchInput) (*MergeBranchResult, error) {
	if err := ValidateShardName(in.Repo, false); err != nil {
		return nil, err
	}
	srcName, err := memrepo.NormalizeBranch(in.SourceBranch)
	if err != nil {
		return nil, err
	}
	dstName, err := memrepo.NormalizeBranch(in.TargetBranch)
	if err != nil {
		return nil, err
	}
	if srcName == dstName {
		return nil, errors.New("memory: cannot merge a branch into itself")
	}

	unlock := s.lockShard(in.Repo)
	locked := true
	release := func() {
		if locked {
			locked = false
			unlock()
		}
	}
	defer release()

	srcBr, err := s.db.GetMemBranch(in.Repo, srcName)
	if err != nil || srcBr.HeadCommit == "" {
		return nil, fmt.Errorf("memory: source branch %s/%s has no commits", in.Repo, srcName)
	}
	srcRow, err := s.db.GetMemCommit(srcBr.HeadCommit)
	if err != nil {
		return nil, err
	}
	srcCommit := commitFromRow(srcRow)

	var dstCommit *memrepo.CommitObject
	if dstBr, dErr := s.db.GetMemBranch(in.Repo, dstName); dErr == nil && dstBr.HeadCommit != "" {
		if dstRow, rErr := s.db.GetMemCommit(dstBr.HeadCommit); rErr == nil {
			dc := commitFromRow(dstRow)
			dstCommit = &dc
		}
	}

	proj, _, trees, blobs, err := s.BuildRepoGoGitProjection(ctx, in.Repo)
	if err != nil {
		return nil, err
	}

	dstHash := ""
	dstTreeHash := ""
	if dstCommit != nil {
		dstHash = dstCommit.CommitHash
		dstTreeHash = dstCommit.TreeHash
	}
	mergeRes, err := memrepo.ThreeWayMergeTrees(proj, dstHash, srcCommit.CommitHash, trees, blobs, s.objects, in.Resolutions, in.Strategy)
	if err != nil {
		return nil, err
	}
	if len(mergeRes.Conflicts) > 0 {
		return &MergeBranchResult{
			BaseCommitHash: mergeRes.BaseCommitHash,
			AutoMerged:     mergeRes.AutoMerged,
			Conflicts:      mergeRes.Conflicts,
		}, fmt.Errorf("%w: %d file(s) have overlapping edits", ErrMergeConflict, len(mergeRes.Conflicts))
	}

	msg := strings.TrimSpace(in.Message)
	if msg == "" {
		msg = fmt.Sprintf("Merge branch %q into %q", srcName, dstName)
	}
	var parents []memrepo.ParentRef
	if dstCommit != nil {
		parents = append(parents, memrepo.ParentRef{CommitHash: dstCommit.CommitHash, AnchorTxID: dstCommit.AnchorTxID})
	}
	parents = append(parents, memrepo.ParentRef{CommitHash: srcCommit.CommitHash, AnchorTxID: srcCommit.AnchorTxID})

	existingByPath, existingByContent := s.collectStoredTreeMeta(trees, dstTreeHash, srcCommit.TreeHash)
	commit, _, err := s.commitFileSnapshotWithOpts(ctx, in.Repo, dstName, mergeRes.MergedFiles, parents, msg, "merge", false, in.Principal, &commitSnapshotOpts{
		PreservedEntries:  mergeRes.PreservedEncrypted,
		ExistingEntries:   existingByPath,
		ExistingByContent: existingByContent,
		CallerHoldsLock:   true,
		ExpectedHead:      dstHash,
		HasExpectedHead:   true,
	})
	release()
	if err != nil {
		return nil, err
	}
	var anchorTxID string
	if in.AutoAnchor {
		if batch, aErr := s.AnchorShard(ctx, in.Repo); aErr != nil {
			return &MergeBranchResult{Commit: commit, BaseCommitHash: mergeRes.BaseCommitHash, AutoMerged: mergeRes.AutoMerged}, aErr
		} else if batch != nil {
			anchorTxID = batch.TxID
			commit.AnchorTxID = batch.TxID
			commit.AnchorN = batch.N
		}
	}
	return &MergeBranchResult{
		Commit:         commit,
		BaseCommitHash: mergeRes.BaseCommitHash,
		AutoMerged:     mergeRes.AutoMerged,
		AnchorTxID:     anchorTxID,
	}, nil
}

// DeleteFile performs a non-destructive `git rm` removing relPath from the branch's new TreeObject
// while preserving every historical commit, BSV UTXO, and untouched encrypted blob untouched.
func (s *Service) DeleteFile(ctx context.Context, repo, branch, relPath, message string, autoAnchor bool, principal *policy.Principal) (*memrepo.CommitObject, error) {
	if err := ValidateShardName(repo, false); err != nil {
		return nil, err
	}
	normPath, err := memrepo.NormalizePath(relPath)
	if err != nil {
		return nil, err
	}
	brName, err := memrepo.NormalizeBranch(branch)
	if err != nil {
		return nil, err
	}
	unlock := s.lockShard(repo)
	locked := true
	release := func() {
		if locked {
			locked = false
			unlock()
		}
	}
	defer release()
	headC, tr, blobs, err := s.RepoTree(ctx, repo, brName, "")
	if err != nil {
		return nil, err
	}
	if headC == nil || tr == nil {
		return nil, fmt.Errorf("memory: branch %s/%s has no commits", repo, brName)
	}

	files := make(map[string][]byte, len(tr.Entries))
	preservedEncrypted := make(map[string]memrepo.TreeEntry)
	existingMeta := make(map[string]memrepo.TreeEntry, len(tr.Entries))
	found := false
	for _, e := range tr.Entries {
		if e.Path == normPath {
			found = true
			continue
		}
		b, ok := blobs[e.BlobHash]
		if ok && b.Forgotten {
			continue
		}
		if e.Encrypted || (ok && b.Encrypted) {
			e.Encrypted = true
			preservedEncrypted[e.Path] = e
			continue
		}
		if ok {
			files[e.Path] = memrepo.ResolveBlobBytes(b, s.objects)
			existingMeta[e.Path] = e
		}
	}
	if !found {
		return nil, fmt.Errorf("memory: file %q not found on %s/%s", normPath, repo, brName)
	}
	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = fmt.Sprintf("Remove %s", normPath)
	}
	parents := []memrepo.ParentRef{{CommitHash: headC.CommitHash, AnchorTxID: headC.AnchorTxID}}
	c, _, err := s.commitFileSnapshotWithOpts(ctx, repo, brName, files, parents, msg, "rm", false, principal, &commitSnapshotOpts{
		PreservedEntries: preservedEncrypted,
		ExistingEntries:  existingMeta,
		CallerHoldsLock:  true,
		ExpectedHead:     headC.CommitHash,
		HasExpectedHead:  true,
	})
	release()
	if err != nil {
		return nil, err
	}
	if autoAnchor {
		if batch, aErr := s.AnchorShard(ctx, repo); aErr != nil {
			return c, aErr
		} else if batch != nil {
			c.AnchorTxID = batch.TxID
			c.AnchorN = batch.N
		}
	}
	return c, nil
}

// RevertCommit creates a non-destructive Git revert commit inverting commitRef onto branch
// using the unified line-level 3-way merge engine (memrepo.ThreeWayMergeWithBase).
func (s *Service) RevertCommit(ctx context.Context, repo, branch, commitRef string, autoAnchor bool, principal *policy.Principal) (*MergeBranchResult, error) {
	if err := ValidateShardName(repo, false); err != nil {
		return nil, err
	}
	brName, err := memrepo.NormalizeBranch(branch)
	if err != nil {
		return nil, err
	}
	unlock := s.lockShard(repo)
	locked := true
	release := func() {
		if locked {
			locked = false
			unlock()
		}
	}
	defer release()
	target, err := s.ResolveCommitRef(ctx, repo, commitRef)
	if err != nil {
		return nil, err
	}
	headC, _, _, err := s.RepoTree(ctx, repo, brName, "")
	if err != nil || headC == nil {
		return nil, fmt.Errorf("memory: branch %s/%s has no head commit", repo, brName)
	}
	proj, _, trees, blobs, err := s.BuildRepoGoGitProjection(ctx, repo)
	if err != nil {
		return nil, err
	}

	// Invert commit by applying (target -> parent) onto current branch head:
	// base = target, ours = head, theirs = target's first parent (or empty tree for root commit).
	parentHash := ""
	if len(target.Parents) > 0 {
		parentHash = target.Parents[0].CommitHash
	}
	res, err := memrepo.ThreeWayMergeWithBase(proj, target.CommitHash, headC.CommitHash, parentHash, trees, blobs, s.objects, nil, "")
	if err != nil {
		return nil, err
	}
	if len(res.Conflicts) > 0 {
		return &MergeBranchResult{Conflicts: res.Conflicts}, fmt.Errorf("%w: revert conflicted on %d file(s)", ErrMergeConflict, len(res.Conflicts))
	}
	short := target.CommitHash
	if len(short) > 7 {
		short = short[:7]
	}
	msg := fmt.Sprintf("Revert %q (%s)", summarizeCommitText(target.Message, short), short)
	parents := []memrepo.ParentRef{{CommitHash: headC.CommitHash, AnchorTxID: headC.AnchorTxID}}
	existingByPath, existingByContent := s.collectStoredTreeMeta(trees, headC.TreeHash, target.TreeHash)
	c, _, err := s.commitFileSnapshotWithOpts(ctx, repo, brName, res.MergedFiles, parents, msg, "revert", false, principal, &commitSnapshotOpts{
		PreservedEntries:  res.PreservedEncrypted,
		ExistingEntries:   existingByPath,
		ExistingByContent: existingByContent,
		CallerHoldsLock:   true,
		ExpectedHead:      headC.CommitHash,
		HasExpectedHead:   true,
	})
	release()
	if err != nil {
		return nil, err
	}
	var anchorTxID string
	if autoAnchor {
		if batch, aErr := s.AnchorShard(ctx, repo); aErr != nil {
			return &MergeBranchResult{Commit: c, AutoMerged: res.AutoMerged}, aErr
		} else if batch != nil {
			anchorTxID = batch.TxID
			c.AnchorTxID = batch.TxID
			c.AnchorN = batch.N
		}
	}
	return &MergeBranchResult{Commit: c, AutoMerged: res.AutoMerged, AnchorTxID: anchorTxID}, nil
}

// CherryPickCommit applies a single commit's diff cleanly onto targetBranch
// using the unified line-level 3-way merge engine (memrepo.ThreeWayMergeWithBase).
func (s *Service) CherryPickCommit(ctx context.Context, repo, targetBranch, commitRef string, autoAnchor bool, principal *policy.Principal) (*MergeBranchResult, error) {
	if err := ValidateShardName(repo, false); err != nil {
		return nil, err
	}
	brName, err := memrepo.NormalizeBranch(targetBranch)
	if err != nil {
		return nil, err
	}
	unlock := s.lockShard(repo)
	locked := true
	release := func() {
		if locked {
			locked = false
			unlock()
		}
	}
	defer release()
	target, err := s.ResolveCommitRef(ctx, repo, commitRef)
	if err != nil {
		return nil, err
	}
	headC, _, _, err := s.RepoTree(ctx, repo, brName, "")
	if err != nil {
		return nil, err
	}
	proj, _, trees, blobs, err := s.BuildRepoGoGitProjection(ctx, repo)
	if err != nil {
		return nil, err
	}
	baseHash := ""
	if len(target.Parents) > 0 {
		baseHash = target.Parents[0].CommitHash
	}
	oursHash := ""
	oursTreeHash := ""
	if headC != nil {
		oursHash = headC.CommitHash
		oursTreeHash = headC.TreeHash
	}
	res, err := memrepo.ThreeWayMergeWithBase(proj, baseHash, oursHash, target.CommitHash, trees, blobs, s.objects, nil, "")
	if err != nil {
		return nil, err
	}
	if len(res.Conflicts) > 0 {
		return &MergeBranchResult{Conflicts: res.Conflicts}, fmt.Errorf("%w: cherry-pick conflicted on %d file(s)", ErrMergeConflict, len(res.Conflicts))
	}
	short := target.CommitHash
	if len(short) > 7 {
		short = short[:7]
	}
	msg := fmt.Sprintf("%s (cherry-picked from %s)", strings.TrimSpace(target.Message), short)
	var parents []memrepo.ParentRef
	if headC != nil {
		parents = append(parents, memrepo.ParentRef{CommitHash: headC.CommitHash, AnchorTxID: headC.AnchorTxID})
	}
	existingByPath, existingByContent := s.collectStoredTreeMeta(trees, oursTreeHash, target.TreeHash)
	c, _, err := s.commitFileSnapshotWithOpts(ctx, repo, brName, res.MergedFiles, parents, msg, "cherry-pick", false, principal, &commitSnapshotOpts{
		PreservedEntries:  res.PreservedEncrypted,
		ExistingEntries:   existingByPath,
		ExistingByContent: existingByContent,
		CallerHoldsLock:   true,
		ExpectedHead:      oursHash,
		HasExpectedHead:   true,
	})
	release()
	if err != nil {
		return nil, err
	}
	var anchorTxID string
	if autoAnchor {
		if batch, aErr := s.AnchorShard(ctx, repo); aErr != nil {
			return &MergeBranchResult{Commit: c, AutoMerged: res.AutoMerged}, aErr
		} else if batch != nil {
			anchorTxID = batch.TxID
			c.AnchorTxID = batch.TxID
			c.AnchorN = batch.N
		}
	}
	return &MergeBranchResult{Commit: c, AutoMerged: res.AutoMerged, AnchorTxID: anchorTxID}, nil
}
