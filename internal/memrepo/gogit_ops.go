package memrepo

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

// UnifiedDiffLine represents one line in a Myers unified diff patch.
type UnifiedDiffLine struct {
	Op   string `json:"op"` // " " (context) | "+" (add) | "-" (del)
	Text string `json:"text"`
}

// UnifiedFileDiff represents the Myers line-by-line diff for one file between two commits.
type UnifiedFileDiff struct {
	Status    string            `json:"status"` // added | modified | deleted
	Path      string            `json:"path"`
	Binary    bool              `json:"binary,omitempty"`
	Additions int               `json:"additions"`
	Deletions int               `json:"deletions"`
	Lines     []UnifiedDiffLine `json:"lines,omitempty"`
}

// BlameLine traces a single line of a memory file back to its originating Commit, ModelID, and BSV UTXO.
type BlameLine struct {
	LineNo     int    `json:"lineNo"`
	Text       string `json:"text"`
	CommitHash string `json:"commitHash"`
	AuthorPub  string `json:"authorPub,omitempty"`
	ModelID    string `json:"modelId,omitempty"`
	Timestamp  int64  `json:"timestamp"`
	AnchorTxID string `json:"anchorTxid,omitempty"`
	AnchorN    int64  `json:"anchorN"`
}

// PushedGitCommit describes one Git commit unpacked from an incoming Smart HTTP git-receive-pack.
type PushedGitCommit struct {
	GitOID             plumbing.Hash
	Parents            []plumbing.Hash
	Message            string
	RawMessage         string
	Timestamp          int64
	AuthorName         string
	AuthorEmail        string
	AuthorWhen         int64
	AuthorOffsetSec    int
	CommitterName      string
	CommitterEmail     string
	CommitterWhen      int64
	CommitterOffsetSec int
	Files              map[string][]byte
}

// ComputeUnifiedDiff computes Myers unified diffs between fromNektarHash (may be empty for root commit)
// and toNektarHash using go-git/v5's native Tree.Patch engine.
func ComputeUnifiedDiff(proj *GoGitProjection, fromNektarHash, toNektarHash string) ([]UnifiedFileDiff, error) {
	if proj == nil || proj.Repo == nil {
		return nil, errors.New("memrepo: nil go-git projection")
	}
	toOID, ok := proj.GitByNektar[toNektarHash]
	if !ok {
		return nil, fmt.Errorf("memrepo: target commit %s not in go-git projection", toNektarHash)
	}
	toCommit, err := proj.Repo.CommitObject(toOID)
	if err != nil {
		return nil, err
	}
	toTree, err := toCommit.Tree()
	if err != nil {
		return nil, err
	}

	var fromTree *object.Tree
	if fromNektarHash != "" {
		if fromOID, ok := proj.GitByNektar[fromNektarHash]; ok {
			if fromCommit, cErr := proj.Repo.CommitObject(fromOID); cErr == nil {
				fromTree, _ = fromCommit.Tree()
			}
		}
	}
	if fromTree == nil {
		// Root commit: compare against an empty tree so every file renders cleanly as added lines.
		fromTree = &object.Tree{}
	}

	patch, err := fromTree.Patch(toTree)
	if err != nil {
		return nil, fmt.Errorf("memrepo: go-git tree patch: %w", err)
	}

	fps := patch.FilePatches()
	out := make([]UnifiedFileDiff, 0, len(fps))
	for _, fp := range fps {
		fromFile, toFile := fp.Files()
		var status, p string
		switch {
		case fromFile == nil && toFile != nil:
			status = "added"
			p = toFile.Path()
		case fromFile != nil && toFile == nil:
			status = "deleted"
			p = fromFile.Path()
		case fromFile != nil && toFile != nil:
			status = "modified"
			p = toFile.Path()
		default:
			continue
		}
		fd := UnifiedFileDiff{
			Status: status,
			Path:   p,
			Binary: fp.IsBinary(),
		}
		if !fp.IsBinary() {
			for _, ch := range fp.Chunks() {
				var op string
				switch ch.Type() {
				case diff.Add:
					op = "+"
				case diff.Delete:
					op = "-"
				default:
					op = " "
				}
				content := ch.Content()
				// Split preserving lines without duplicating trailing empty line after final \n.
				trimmed := strings.TrimSuffix(content, "\n")
				var split []string
				if content != "" {
					split = strings.Split(trimmed, "\n")
				}
				for _, ln := range split {
					fd.Lines = append(fd.Lines, UnifiedDiffLine{Op: op, Text: ln})
					if op == "+" {
						fd.Additions++
					} else if op == "-" {
						fd.Deletions++
					}
				}
			}
		}
		out = append(out, fd)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// ComputeCommitBlame runs go-git's native git.Blame over relPath at nektarCommitHash and enriches
// every single line with Nektar's commit hash, AuthorPub, ModelID, and BSV NektarAnchor UTXO.
func ComputeCommitBlame(proj *GoGitProjection, nektarCommitHash, relPath string) ([]BlameLine, error) {
	if proj == nil || proj.Repo == nil {
		return nil, errors.New("memrepo: nil go-git projection")
	}
	normPath, err := NormalizePath(relPath)
	if err != nil {
		return nil, err
	}
	gitOID, ok := proj.GitByNektar[nektarCommitHash]
	if !ok {
		return nil, fmt.Errorf("memrepo: commit %s not found in go-git projection", nektarCommitHash)
	}
	gc, err := proj.Repo.CommitObject(gitOID)
	if err != nil {
		return nil, err
	}
	res, err := git.Blame(gc, normPath)
	if err != nil {
		return nil, fmt.Errorf("memrepo: go-git blame %s: %w", normPath, err)
	}
	out := make([]BlameLine, 0, len(res.Lines))
	for i, ln := range res.Lines {
		bl := BlameLine{
			LineNo:    i + 1,
			Text:      ln.Text,
			Timestamp: ln.Date.Unix(),
			AnchorN:   -1,
		}
		if nc, ok := proj.NektarByGit[ln.Hash]; ok {
			bl.CommitHash = nc.CommitHash
			bl.AuthorPub = nc.AuthorPub
			bl.ModelID = nc.ModelID
			bl.Timestamp = nc.Timestamp
			bl.AnchorTxID = nc.AnchorTxID
			bl.AnchorN = nc.AnchorN
		} else {
			bl.CommitHash = ln.Hash.String()
		}
		out = append(out, bl)
	}
	return out, nil
}

// ExtractPushedCommits walks newly pushed Git commits in store from oldOID..newOID in topological
// parent-first order. Strictly enforces fast-forward ancestry (rejects history rewrites when
// !oldOID.IsZero() and oldOID is not an ancestor of newOID).
func ExtractPushedCommits(st *memory.Storage, oldOID, newOID plumbing.Hash) ([]PushedGitCommit, error) {
	if st == nil {
		return nil, errors.New("memrepo: nil git storage")
	}
	repo, err := git.Open(st, nil)
	if err != nil {
		return nil, fmt.Errorf("memrepo: open pushed git store: %w", err)
	}
	newCommit, err := repo.CommitObject(newOID)
	if err != nil {
		return nil, fmt.Errorf("memrepo: pushed commit %s not found: %w", newOID.String(), err)
	}

	// Enforce SingularGrit anti-revisionism: non-fast-forward history rewrites are strictly refused!
	if !oldOID.IsZero() {
		oldCommit, err := repo.CommitObject(oldOID)
		if err != nil {
			return nil, fmt.Errorf("memrepo: existing head %s missing: %w", oldOID.String(), err)
		}
		isAnc, err := oldCommit.IsAncestor(newCommit)
		if err != nil || !isAnc {
			return nil, errors.New("memrepo: non-fast-forward push rejected (memory history is append-only; use git revert or a new branch)")
		}
	}

	// Collect all commits reachable from newOID stopping at oldOID (or existing branch heads on new-branch push)
	// so pushing a new branch NEVER re-imports the entire already-persisted repository history!
	visited := make(map[plumbing.Hash]bool)
	markReachable := func(start *object.Commit) {
		if start == nil || visited[start.Hash] {
			return
		}
		_ = object.NewCommitPreorderIter(start, nil, nil).ForEach(func(c *object.Commit) error {
			visited[c.Hash] = true
			return nil
		})
	}
	if !oldOID.IsZero() {
		oldCommit, _ := repo.CommitObject(oldOID)
		markReachable(oldCommit)
	} else {
		if brIter, bErr := repo.Branches(); bErr == nil {
			_ = brIter.ForEach(func(ref *plumbing.Reference) error {
				if ref == nil || ref.Hash() == newOID || ref.Hash().IsZero() {
					return nil
				}
				if c, cErr := repo.CommitObject(ref.Hash()); cErr == nil {
					markReachable(c)
				}
				return nil
			})
		}
	}

	var ordered []*object.Commit
	var dfs func(c *object.Commit) error
	dfs = func(c *object.Commit) error {
		if c == nil || visited[c.Hash] {
			return nil
		}
		visited[c.Hash] = true
		for _, pHash := range c.ParentHashes {
			if visited[pHash] {
				continue
			}
			pc, err := repo.CommitObject(pHash)
			if err != nil {
				return fmt.Errorf("memrepo: missing parent commit %s in packfile: %w", pHash.String(), err)
			}
			if err := dfs(pc); err != nil {
				return err
			}
		}
		ordered = append(ordered, c)
		return nil
	}
	if err := dfs(newCommit); err != nil {
		return nil, err
	}

	out := make([]PushedGitCommit, 0, len(ordered))
	for _, c := range ordered {
		tr, err := c.Tree()
		if err != nil {
			return nil, fmt.Errorf("memrepo: commit %s tree: %w", c.Hash.String(), err)
		}
		files, err := ReadGoGitTreeFiles(tr)
		if err != nil {
			return nil, fmt.Errorf("memrepo: commit %s files: %w", c.Hash.String(), err)
		}
		ts := c.Author.When.Unix()
		if ts <= 0 {
			ts = c.Committer.When.Unix()
		}
		_, aOff := c.Author.When.Zone()
		_, cOff := c.Committer.When.Zone()
		out = append(out, PushedGitCommit{
			GitOID:             c.Hash,
			Parents:            append([]plumbing.Hash(nil), c.ParentHashes...),
			Message:            strings.TrimSpace(c.Message),
			RawMessage:         c.Message,
			Timestamp:          ts,
			AuthorName:         c.Author.Name,
			AuthorEmail:        c.Author.Email,
			AuthorWhen:         c.Author.When.Unix(),
			AuthorOffsetSec:    aOff,
			CommitterName:      c.Committer.Name,
			CommitterEmail:     c.Committer.Email,
			CommitterWhen:      c.Committer.When.Unix(),
			CommitterOffsetSec: cOff,
			Files:              files,
		})
	}
	return out, nil
}
