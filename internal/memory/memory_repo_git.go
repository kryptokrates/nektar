package memory

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

// ResolveCommitRef resolves a full commit hash, short prefix (>=6 chars), branch name, or tag name in repo.
func (s *Service) ResolveCommitRef(ctx context.Context, repo, ref string) (memrepo.CommitObject, error) {
	return s.ResolveCommitRefAuthorized(ctx, repo, ref, ownerPrincipal())
}

// ResolveCommitRefAuthorized enforces system-shard denial and Cedar read policy when resolving a ref.
func (s *Service) ResolveCommitRefAuthorized(_ context.Context, repo, ref string, principal policy.Principal) (memrepo.CommitObject, error) {
	isOp := isOperatorPrincipal(principal)
	if err := ValidateShardName(repo, isOp); err != nil {
		return memrepo.CommitObject{}, err
	}
	if !isOp {
		if dec, aErr := s.AuthorizeAction(principal, "read", policy.Resource{
			Type:  "Fragment",
			ID:    repo + "#ref",
			Level: 0,
			Shard: repo,
			Class: "repo",
		}); aErr != nil || !dec.Allow {
			return memrepo.CommitObject{}, fmt.Errorf("memory: cedar policy denied ref lookup on repo %q", repo)
		}
	}
	clean := strings.TrimSpace(ref)
	if clean == "" || strings.EqualFold(clean, "HEAD") {
		clean = memrepo.DefaultBranch
	}
	// 1. Exact branch match.
	if br, err := s.db.GetMemBranch(repo, clean); err == nil && br.HeadCommit != "" {
		row, rErr := s.db.GetMemCommit(br.HeadCommit)
		if rErr == nil {
			return commitFromRow(row), nil
		}
	}
	// 2. Exact tag match.
	if tg, err := s.db.GetMemTag(repo, clean); err == nil && tg.CommitHash != "" {
		row, rErr := s.db.GetMemCommit(tg.CommitHash)
		if rErr == nil {
			return commitFromRow(row), nil
		}
	}
	// 3. Exact commit hash match.
	if row, err := s.db.GetMemCommit(clean); err == nil {
		if row.Repo == repo {
			return commitFromRow(row), nil
		}
	}
	// 4. Short commit hash prefix match (>= 6 hex chars).
	if row, err := s.db.GetMemCommitByPrefix(repo, clean); err == nil {
		return commitFromRow(row), nil
	}
	return memrepo.CommitObject{}, fmt.Errorf("memory: ref %q not found in repo %q", ref, repo)
}

// BuildRepoGoGitProjection projects repo's full commit DAG, trees, blobs, branches, and tags into go-git/v5.
func (s *Service) BuildRepoGoGitProjection(ctx context.Context, repo string) (
	*memrepo.GoGitProjection,
	[]memrepo.CommitObject,
	map[string]memrepo.TreeObject,
	map[string]memrepo.BlobObject,
	error,
) {
	return s.BuildRepoGoGitProjectionAuthorized(ctx, repo, ownerPrincipal(), false)
}

// collectCommitDAGObjects loads and Cedar-filters all trees and blobs referenced by commits,
// optionally collecting CHIRP object hashes for plaintext blobs.
func (s *Service) collectCommitDAGObjects(
	repo string,
	commits []memrepo.CommitObject,
	principal policy.Principal,
	disallowBorg bool,
	collectChirpHashes bool,
) (map[string]memrepo.TreeObject, map[string]memrepo.BlobObject, [][32]byte) {
	trees := make(map[string]memrepo.TreeObject)
	blobs := make(map[string]memrepo.BlobObject)
	var chirpHashes [][32]byte
	for _, c := range commits {
		if _, seen := trees[c.TreeHash]; seen {
			continue
		}
		tr, tErr := s.loadTreeObject(c.TreeHash)
		if tErr != nil || tr == nil {
			continue
		}
		rawBlobs := make(map[string]memrepo.BlobObject, len(tr.Entries))
		for _, e := range tr.Entries {
			if b, bErr := s.loadBlobObject(e.BlobHash); bErr == nil && b != nil {
				rawBlobs[b.BlobHash] = *b
			}
		}
		filteredTree, filteredBlobs := s.filterTreeForPrincipal(repo, c.CommitHash, tr, rawBlobs, principal, disallowBorg)
		trees[tr.TreeHash] = *filteredTree
		for bh, b := range filteredBlobs {
			if _, bSeen := blobs[bh]; !bSeen {
				blobs[bh] = b
				if collectChirpHashes && b.ChirpURL != "" && !b.Encrypted && !b.Forgotten {
					if hs, cErr := bstack.CollectChirpHashes(s.objects, b.ChirpURL); cErr == nil {
						chirpHashes = append(chirpHashes, hs...)
					}
				}
			}
		}
	}
	return trees, blobs, chirpHashes
}

// BuildRepoGoGitProjectionAuthorized projects repo's uncapped commit DAG into go-git/v5 while
// filtering every commit's tree and blobs through Cedar read authorization and optional Borg exclusion.
func (s *Service) BuildRepoGoGitProjectionAuthorized(ctx context.Context, repo string, principal policy.Principal, disallowBorg bool) (
	*memrepo.GoGitProjection,
	[]memrepo.CommitObject,
	map[string]memrepo.TreeObject,
	map[string]memrepo.BlobObject,
	error,
) {
	if err := ValidateShardName(repo, isOperatorPrincipal(principal)); err != nil {
		return nil, nil, nil, nil, err
	}

	brRows, _ := s.db.ListMemBranches(repo)
	tgRows, _ := s.db.ListMemTags(repo)
	anchSums, _ := s.db.ListAnchorSummaries(repo)
	fragCount, _ := s.db.CountShardFragments(repo)

	var keyBuf strings.Builder
	fmt.Fprintf(&keyBuf, "%s|%s|%s|%d|%t|%s|%d", repo, principal.Type, principal.ID, principal.Level, disallowBorg, s.PolicyCommit().Hash, fragCount)
	for _, b := range brRows {
		fmt.Fprintf(&keyBuf, "|B:%s=%s", b.Branch, b.HeadCommit)
	}
	for _, tg := range tgRows {
		fmt.Fprintf(&keyBuf, "|T:%s=%s", tg.Tag, tg.CommitHash)
	}
	if len(anchSums) > 0 {
		last := anchSums[len(anchSums)-1]
		fmt.Fprintf(&keyBuf, "|A:%d:%d:%s:%t", len(anchSums), last.N, last.TxID, last.Settled)
	}
	cacheKey := keyBuf.String()

	s.projCacheMu.RLock()
	if cached, ok := s.projCache[cacheKey]; ok {
		s.projCacheMu.RUnlock()
		return cached.proj, cached.commits, cached.trees, cached.blobs, nil
	}
	s.projCacheMu.RUnlock()

	rows, err := s.db.ListAllMemCommits(repo)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	commits := make([]memrepo.CommitObject, 0, len(rows))
	for _, r := range rows {
		commits = append(commits, commitFromRow(r))
	}
	trees, blobs, _ := s.collectCommitDAGObjects(repo, commits, principal, disallowBorg, false)

	branches := make([]memrepo.BranchRef, 0, len(brRows))
	for _, b := range brRows {
		branches = append(branches, memrepo.BranchRef{
			Repo:           b.Repo,
			Branch:         b.Branch,
			HeadCommit:     b.HeadCommit,
			GenesisPub:     b.GenesisPub,
			GenesisShardID: b.GenesisShardID,
			UpdatedAt:      b.UpdatedAt,
		})
	}

	tags := make([]memrepo.TagRef, 0, len(tgRows))
	for _, tg := range tgRows {
		tags = append(tags, tagFromRow(tg))
	}

	proj, err := memrepo.BuildGoGitRepo(commits, trees, blobs, branches, tags, s.objects)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	s.projCacheMu.Lock()
	if s.projCache == nil || len(s.projCache) >= 16 {
		s.projCache = make(map[string]*cachedGoGitProjection, 8)
	}
	s.projCache[cacheKey] = &cachedGoGitProjection{
		proj:    proj,
		commits: commits,
		trees:   trees,
		blobs:   blobs,
	}
	s.projCacheMu.Unlock()

	return proj, commits, trees, blobs, nil
}

// RepoDiffPatch computes Myers unified line-by-line diffs between fromRef and toRef via go-git/v5.
// If fromRef is empty, diffs toRef against its first parent (or empty tree for root commits).
func (s *Service) RepoDiffPatch(ctx context.Context, repo, fromRef, toRef string) ([]memrepo.UnifiedFileDiff, error) {
	return s.RepoDiffPatchAuthorized(ctx, repo, fromRef, toRef, ownerPrincipal(), false)
}

// RepoDiffPatchAuthorized enforces Cedar read authorization before computing unified diffs.
func (s *Service) RepoDiffPatchAuthorized(ctx context.Context, repo, fromRef, toRef string, principal policy.Principal, disallowBorg bool) ([]memrepo.UnifiedFileDiff, error) {
	if toRef == "" {
		toRef = memrepo.DefaultBranch
	}
	toCommit, err := s.ResolveCommitRefAuthorized(ctx, repo, toRef, principal)
	if err != nil {
		return nil, err
	}
	var fromHash string
	if strings.TrimSpace(fromRef) != "" {
		fc, err := s.ResolveCommitRefAuthorized(ctx, repo, fromRef, principal)
		if err != nil {
			return nil, err
		}
		fromHash = fc.CommitHash
	} else if len(toCommit.Parents) > 0 {
		fromHash = toCommit.Parents[0].CommitHash
	}

	proj, _, _, _, err := s.BuildRepoGoGitProjectionAuthorized(ctx, repo, principal, disallowBorg)
	if err != nil {
		return nil, err
	}
	return memrepo.ComputeUnifiedDiff(proj, fromHash, toCommit.CommitHash)
}

// RepoBlame runs go-git/v5 git.Blame over relPath at commitRef (or branch head) enriched with ModelID & BSV UTXO.
func (s *Service) RepoBlame(ctx context.Context, repo, branch, commitRef, relPath string) ([]memrepo.BlameLine, error) {
	return s.RepoBlameAuthorized(ctx, repo, branch, commitRef, relPath, ownerPrincipal(), false)
}

// RepoBlameAuthorized enforces Cedar read authorization before running line-level blame.
func (s *Service) RepoBlameAuthorized(ctx context.Context, repo, branch, commitRef, relPath string, principal policy.Principal, disallowBorg bool) ([]memrepo.BlameLine, error) {
	ref := strings.TrimSpace(commitRef)
	if ref == "" {
		ref = strings.TrimSpace(branch)
	}
	if ref == "" {
		ref = memrepo.DefaultBranch
	}
	c, err := s.ResolveCommitRefAuthorized(ctx, repo, ref, principal)
	if err != nil {
		return nil, err
	}
	proj, _, _, _, err := s.BuildRepoGoGitProjectionAuthorized(ctx, repo, principal, disallowBorg)
	if err != nil {
		return nil, err
	}
	return memrepo.ComputeCommitBlame(proj, c.CommitHash, relPath)
}

// ForkRepo forks sourceRepo/sourceBranch into a brand-new isolated Memory Repo with its own
// unique BRC-43 GenesisOwner on-chain identity, linking its genesis commit back to the source UTXO.
func (s *Service) ForkRepo(ctx context.Context, sourceRepo, sourceBranch, newRepo string, autoAnchor bool, principal *policy.Principal) (*RepoSummary, error) {
	if err := ValidateShardName(sourceRepo, false); err != nil {
		return nil, err
	}
	if err := ValidateShardName(newRepo, false); err != nil {
		return nil, err
	}
	if strings.EqualFold(sourceRepo, newRepo) {
		return nil, errors.New("memory: fork target repo must differ from source repo")
	}
	if existing, _ := s.db.ListMemCommits(newRepo, "", 1); len(existing) > 0 {
		return nil, fmt.Errorf("memory: target repo %q already exists", newRepo)
	}
	srcBr, err := memrepo.NormalizeBranch(sourceBranch)
	if err != nil {
		return nil, err
	}
	srcSum, err := s.GetRepoSummary(ctx, sourceRepo)
	if err != nil {
		return nil, err
	}
	readPrincipal := ownerPrincipal()
	if principal != nil && principal.ID != "" {
		readPrincipal = *principal
	}
	srcHead, srcTree, srcBlobs, err := s.RepoTreeAuthorized(ctx, sourceRepo, srcBr, "", readPrincipal, true)
	if err != nil {
		return nil, err
	}
	if srcHead == nil || srcTree == nil {
		return nil, fmt.Errorf("memory: source repo %s/%s has no commits to fork", sourceRepo, srcBr)
	}

	files := make(map[string][]byte, len(srcTree.Entries))
	existingEntries := make(map[string]memrepo.TreeEntry, len(srcTree.Entries))
	for _, e := range srcTree.Entries {
		if b, ok := srcBlobs[e.BlobHash]; ok && !b.Encrypted && !b.Forgotten {
			files[e.Path] = memrepo.ResolveBlobBytes(b, s.objects)
			existingEntries[e.Path] = e
		}
	}
	srcAnchor := srcHead.AnchorTxID
	if srcAnchor == "" {
		srcAnchor = srcSum.HeadAnchorTxID
	}
	parents := []memrepo.ParentRef{{
		CommitHash:  srcHead.CommitHash,
		AnchorTxID:  srcAnchor,
		RepoGenesis: srcSum.GenesisShardID,
	}}
	msg := fmt.Sprintf("Fork %s/%s (%s)", sourceRepo, srcBr, srcHead.CommitHash[:min(7, len(srcHead.CommitHash))])
	if _, _, err := s.commitFileSnapshotWithOpts(ctx, newRepo, memrepo.DefaultBranch, files, parents, msg, "fork", autoAnchor, principal, &commitSnapshotOpts{
		ExistingEntries: existingEntries,
	}); err != nil {
		return nil, err
	}
	return s.GetRepoSummary(ctx, newRepo)
}

// CreateTag creates a signed immutable checkpoint tag pinned to commitRef (defaulting to main head).
func (s *Service) CreateTag(ctx context.Context, repo, tagName, commitRef, message string) (*memrepo.TagRef, error) {
	return s.CreateTagAuthorized(ctx, repo, tagName, commitRef, message, nil)
}

// CreateTagAuthorized enforces Cedar write authorization before creating an immutable checkpoint tag.
func (s *Service) CreateTagAuthorized(ctx context.Context, repo, tagName, commitRef, message string, principal *policy.Principal) (*memrepo.TagRef, error) {
	if err := ValidateShardName(repo, false); err != nil {
		return nil, err
	}
	normTag, err := memrepo.NormalizeTagName(tagName)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeRepoWrite(repo, repo+"#tag:"+normTag, "tag", principal); err != nil {
		return nil, err
	}
	unlock := s.lockShard(repo)
	defer unlock()
	if _, err := s.db.GetMemTag(repo, normTag); err == nil {
		return nil, fmt.Errorf("memory: tag %q already exists in repo %q (tags are immutable)", normTag, repo)
	}
	c, err := s.ResolveCommitRef(ctx, repo, commitRef)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	authorPub := s.commitAuthorPubHex(ctx)
	signable, _ := core.CanonicalJSON(map[string]any{
		"repo":       repo,
		"tag":        normTag,
		"commitHash": c.CommitHash,
		"anchorTxid": c.AnchorTxID,
		"anchorN":    c.AnchorN,
		"message":    strings.TrimSpace(message),
		"timestamp":  now,
	})
	sigBytes, err := s.port.SignDigest(ctx, core.DSHA256(core.Concat([]byte("nektar:tag:v1"), signable)))
	if err != nil {
		return nil, fmt.Errorf("memory: sign tag: %w", err)
	}
	row := store.MemTagRow{
		Repo:       repo,
		Tag:        normTag,
		CommitHash: c.CommitHash,
		AnchorTxID: c.AnchorTxID,
		AnchorN:    c.AnchorN,
		Message:    strings.TrimSpace(message),
		AuthorPub:  authorPub,
		Signature:  hex.EncodeToString(sigBytes),
		CreatedAt:  now,
	}
	if err := s.db.SaveMemTag(row); err != nil {
		return nil, err
	}
	ref := tagFromRow(row)
	return &ref, nil
}

// ListTags returns all immutable checkpoint tags for repo.
func (s *Service) ListTags(repo string) ([]memrepo.TagRef, error) {
	return s.ListTagsAuthorized(repo, ownerPrincipal())
}

// ListTagsAuthorized enforces system-shard denial and Cedar read authorization before listing tags.
func (s *Service) ListTagsAuthorized(repo string, principal policy.Principal) ([]memrepo.TagRef, error) {
	isOp := isOperatorPrincipal(principal)
	if err := ValidateShardName(repo, isOp); err != nil {
		return nil, err
	}
	if !isOp {
		if dec, aErr := s.AuthorizeAction(principal, "read", policy.Resource{
			Type:  "Fragment",
			ID:    repo + "#tags",
			Level: 0,
			Shard: repo,
			Class: "repo",
		}); aErr != nil || !dec.Allow {
			return nil, fmt.Errorf("memory: cedar policy denied tag list on repo %q", repo)
		}
	}
	rows, err := s.db.ListMemTags(repo)
	if err != nil {
		return nil, err
	}
	out := make([]memrepo.TagRef, 0, len(rows))
	for _, r := range rows {
		out = append(out, tagFromRow(r))
	}
	return out, nil
}
