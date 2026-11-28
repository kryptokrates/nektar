package memory

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

// RepoSummary describes one Memory Repo (formerly called a shard) with Git/UTXO lineage telemetry.
type RepoSummary struct {
	Repo           string              `json:"repo"`
	Branch         string              `json:"branch"`
	Branches       []string            `json:"branches"`
	GenesisPub     string              `json:"genesisPub,omitempty"`
	GenesisShardID string              `json:"genesisShardId,omitempty"`
	HeadCommit     string              `json:"headCommit,omitempty"`
	CommitCount    int                 `json:"commitCount"`
	FileCount      int                 `json:"fileCount"`
	FragmentCount  int                 `json:"fragmentCount"`
	AnchorCount    int                 `json:"anchorCount"`
	SettledAnchors int                 `json:"settledAnchors"`
	HeadAnchorTxID string              `json:"headAnchorTxid,omitempty"`
	HeadAnchorN    int64               `json:"headAnchorN"`
	UpdatedAt      int64               `json:"updatedAt"`
	LatestMessage  string              `json:"latestMessage,omitempty"`
	Anchors        []RepoAnchorSummary `json:"anchors,omitempty"`
	Tags           []memrepo.TagRef    `json:"tags,omitempty"`
}

// RepoAnchorSummary is a compact UI-friendly view of one NektarAnchor UTXO in a repo's chain.
type RepoAnchorSummary struct {
	N           int64  `json:"n"`
	Size        int    `json:"size"`
	TxID        string `json:"txid"`
	RootHex     string `json:"R"`
	ChainedHex  string `json:"Rp"`
	Settled     bool   `json:"settled"`
	BlockHeight int64  `json:"blockHeight"`
	CreatedAt   int64  `json:"createdAt"`
}

func isOperatorPrincipal(p policy.Principal) bool {
	return p.ID == "" || p.ID == "owner" || p.Level >= 100
}

// ListRepos returns every user Memory Repo with its branch heads, commit count, file count, and UTXO lineage.
func (s *Service) ListRepos(ctx context.Context) ([]RepoSummary, error) {
	return s.ListReposAuthorized(ctx, ownerPrincipal())
}

// ListReposAuthorized returns every user Memory Repo, redacting LatestMessage for non-operator principals.
func (s *Service) ListReposAuthorized(ctx context.Context, principal policy.Principal) ([]RepoSummary, error) {
	names, err := s.db.ListAllMemRepos()
	if err != nil {
		return nil, err
	}
	isOp := isOperatorPrincipal(principal)
	out := make([]RepoSummary, 0, len(names))
	for _, repo := range names {
		if IsSystemShard(repo) {
			continue
		}
		sum, err := s.GetRepoSummaryAuthorized(ctx, repo, principal)
		if err != nil {
			continue
		}
		if !isOp {
			sum.LatestMessage = ""
		}
		out = append(out, *sum)
	}
	return out, nil
}

// GetRepoSummary builds the full Git + BSV UTXO lineage summary for one Memory Repo.
func (s *Service) GetRepoSummary(ctx context.Context, repo string) (*RepoSummary, error) {
	return s.GetRepoSummaryAuthorized(ctx, repo, ownerPrincipal())
}

// GetRepoSummaryAuthorized enforces system-shard denial and metadata redaction for non-operator principals.
func (s *Service) GetRepoSummaryAuthorized(ctx context.Context, repo string, principal policy.Principal) (*RepoSummary, error) {
	isOp := isOperatorPrincipal(principal)
	if err := ValidateShardName(repo, isOp); err != nil {
		return nil, err
	}

	branches, _ := s.db.ListMemBranches(repo)
	commitCount, _ := s.db.CountMemCommits(repo)
	latestCommits, _ := s.db.ListMemCommits(repo, "", 1)
	fragCount, _ := s.db.CountShardFragments(repo)
	anchors, _ := s.db.ListAnchorSummaries(repo)

	branchNames := make([]string, 0, len(branches))
	var mainBranch *store.MemBranchRow
	for i := range branches {
		branchNames = append(branchNames, branches[i].Branch)
		if branches[i].Branch == memrepo.DefaultBranch {
			cp := branches[i]
			mainBranch = &cp
		}
	}
	if len(branchNames) == 0 {
		branchNames = []string{memrepo.DefaultBranch}
	}

	var genPub, genID string
	if mainBranch != nil && mainBranch.GenesisPub != "" && mainBranch.GenesisShardID != "" {
		genPub, genID = mainBranch.GenesisPub, mainBranch.GenesisShardID
	} else if len(anchors) > 0 {
		fullAnchors, _ := s.db.ListAnchors(repo)
		genPub, genID, _ = s.resolveRepoGenesis(ctx, repo, fullAnchors)
	} else {
		genPub, genID, _ = s.resolveRepoGenesis(ctx, repo, nil)
	}
	sum := &RepoSummary{
		Repo:           repo,
		Branch:         memrepo.DefaultBranch,
		Branches:       branchNames,
		GenesisPub:     genPub,
		GenesisShardID: genID,
		CommitCount:    commitCount,
		FragmentCount:  fragCount,
		AnchorCount:    len(anchors),
		HeadAnchorN:    -1,
	}
	if mainBranch != nil {
		sum.HeadCommit = mainBranch.HeadCommit
		sum.UpdatedAt = mainBranch.UpdatedAt
	}
	if len(latestCommits) > 0 {
		if sum.HeadCommit == "" {
			sum.HeadCommit = latestCommits[0].CommitHash
		}
		if latestCommits[0].Timestamp > sum.UpdatedAt {
			sum.UpdatedAt = latestCommits[0].Timestamp
		}
		if isOp {
			sum.LatestMessage = latestCommits[0].Message
		}
		if tr, err := s.loadTreeObject(latestCommits[0].TreeHash); err == nil && tr != nil {
			sum.FileCount = len(tr.Entries)
		}
	}
	for _, a := range anchors {
		if a.Settled {
			sum.SettledAnchors++
		}
		sum.HeadAnchorTxID = a.TxID
		sum.HeadAnchorN = a.N
		if a.CreatedAt > sum.UpdatedAt {
			sum.UpdatedAt = a.CreatedAt
		}
		sum.Anchors = append(sum.Anchors, RepoAnchorSummary{
			N:           a.N,
			Size:        a.Size,
			TxID:        a.TxID,
			RootHex:     a.RootHex,
			ChainedHex:  a.ChainedHex,
			Settled:     a.Settled,
			BlockHeight: a.BlockHeight,
			CreatedAt:   a.CreatedAt,
		})
	}
	if tags, tErr := s.ListTagsAuthorized(repo, principal); tErr == nil && len(tags) > 0 {
		sum.Tags = tags
	}
	return sum, nil
}

// RepoLog returns the commit log for repo (and optional branch, newest first).
func (s *Service) RepoLog(ctx context.Context, repo, branch string, limit int) ([]memrepo.CommitObject, error) {
	return s.RepoLogAuthorized(ctx, repo, branch, limit, ownerPrincipal())
}

// RepoLogAuthorized enforces system-shard denial and Cedar read authorization per commit.
func (s *Service) RepoLogAuthorized(ctx context.Context, repo, branch string, limit int, principal policy.Principal) ([]memrepo.CommitObject, error) {
	isOp := isOperatorPrincipal(principal)
	if err := ValidateShardName(repo, isOp); err != nil {
		return nil, err
	}
	rows, err := s.db.ListMemCommits(repo, strings.TrimSpace(branch), limit)
	if err != nil {
		return nil, err
	}
	if !isOp {
		if dec, aErr := s.AuthorizeAction(principal, "read", policy.Resource{
			Type:  "Fragment",
			ID:    repo + "#log",
			Level: 0,
			Shard: repo,
			Class: "repo",
		}); aErr != nil || !dec.Allow {
			return nil, fmt.Errorf("memory: cedar policy denied log read on repo %q", repo)
		}
	}
	out := make([]memrepo.CommitObject, 0, len(rows))
	for _, r := range rows {
		c := commitFromRow(r)
		if c.Seq > 0 {
			if frag, fErr := s.db.GetFragment(repo, c.Seq); fErr == nil {
				if frag.Forgotten {
					c.Message = "[shredded]"
				} else if !isOp && frag.BorgGroup != "" {
					c.Message = "[redacted]"
				} else if !isOp {
					if dec, aErr := s.AuthorizeAction(principal, "read", policy.Resource{
						Type:  "Fragment",
						ID:    fmt.Sprintf("%s#%d", frag.Shard, frag.Seq),
						Level: int64(frag.Level),
						Shard: frag.Shard,
						Class: frag.Class,
					}); aErr != nil || !dec.Allow {
						continue
					}
				}
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// RepoTree returns the TreeObject and resolved BlobObjects at commitHash/ref (or the head of branch).
func (s *Service) RepoTree(ctx context.Context, repo, branch, commitHash string) (*memrepo.CommitObject, *memrepo.TreeObject, map[string]memrepo.BlobObject, error) {
	return s.RepoTreeAuthorized(ctx, repo, branch, commitHash, ownerPrincipal(), false)
}

// RepoTreeAuthorized returns the TreeObject and BlobObjects filtered by Cedar read policy
// and optional Borg exclusion (always excluding forgotten blobs).
func (s *Service) RepoTreeAuthorized(ctx context.Context, repo, branch, commitHash string, principal policy.Principal, disallowBorg bool) (*memrepo.CommitObject, *memrepo.TreeObject, map[string]memrepo.BlobObject, error) {
	if err := ValidateShardName(repo, isOperatorPrincipal(principal)); err != nil {
		return nil, nil, nil, err
	}
	var c memrepo.CommitObject
	if strings.TrimSpace(commitHash) != "" {
		resolved, err := s.ResolveCommitRef(ctx, repo, commitHash)
		if err != nil {
			return nil, nil, nil, err
		}
		c = resolved
	} else {
		brName, err := memrepo.NormalizeBranch(branch)
		if err != nil {
			return nil, nil, nil, err
		}
		br, err := s.db.GetMemBranch(repo, brName)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("memory: branch %s/%s not found: %w", repo, brName, err)
		}
		if br.HeadCommit == "" {
			emptyTree, _ := memrepo.BuildTree(nil)
			return nil, emptyTree, map[string]memrepo.BlobObject{}, nil
		}
		row, err := s.db.GetMemCommit(br.HeadCommit)
		if err != nil {
			return nil, nil, nil, err
		}
		c = commitFromRow(row)
	}
	tr, err := s.loadTreeObject(c.TreeHash)
	if err != nil {
		return &c, nil, nil, err
	}
	rawBlobs := make(map[string]memrepo.BlobObject, len(tr.Entries))
	for _, e := range tr.Entries {
		if b, bErr := s.loadBlobObject(e.BlobHash); bErr == nil && b != nil {
			rawBlobs[b.BlobHash] = *b
		}
	}
	filteredTree, filteredBlobs := s.filterTreeForPrincipal(repo, c.CommitHash, tr, rawBlobs, principal, disallowBorg)
	return &c, filteredTree, filteredBlobs, nil
}

// RepoDiff computes a Git-style tree diff between two commits/refs (if fromCommit is empty, diffs against toCommit's primary parent).
func (s *Service) RepoDiff(ctx context.Context, repo, fromCommit, toCommit string) ([]memrepo.TreeDiffEntry, error) {
	return s.RepoDiffAuthorized(ctx, repo, fromCommit, toCommit, ownerPrincipal(), false)
}

// RepoDiffAuthorized computes a Git-style tree diff filtered through Cedar read policy.
func (s *Service) RepoDiffAuthorized(ctx context.Context, repo, fromCommit, toCommit string, principal policy.Principal, disallowBorg bool) ([]memrepo.TreeDiffEntry, error) {
	if err := ValidateShardName(repo, isOperatorPrincipal(principal)); err != nil {
		return nil, err
	}
	if strings.TrimSpace(toCommit) == "" {
		toCommit = memrepo.DefaultBranch
	}
	toC, toTree, _, err := s.RepoTreeAuthorized(ctx, repo, "", toCommit, principal, disallowBorg)
	if err != nil {
		return nil, err
	}
	var fromTree *memrepo.TreeObject
	if strings.TrimSpace(fromCommit) == "" && toC != nil && len(toC.Parents) > 0 {
		fromCommit = toC.Parents[0].CommitHash
	}
	if strings.TrimSpace(fromCommit) != "" {
		if _, ft, _, fErr := s.RepoTreeAuthorized(ctx, repo, "", fromCommit, principal, disallowBorg); fErr == nil {
			fromTree = ft
		}
	}
	return memrepo.DiffTrees(fromTree, toTree), nil
}

// CreateBranch forks a new branch in repo pointing at fromBranchOrCommit (defaults to main head)
// and derives a distinct BRC-43 genesis identity for that branch.
func (s *Service) CreateBranch(ctx context.Context, repo, newBranch, fromBranchOrCommit string) (*memrepo.BranchRef, error) {
	return s.CreateBranchAuthorized(ctx, repo, newBranch, fromBranchOrCommit, nil)
}

// CreateBranchAuthorized enforces Cedar write authorization before creating a branch.
func (s *Service) CreateBranchAuthorized(ctx context.Context, repo, newBranch, fromBranchOrCommit string, principal *policy.Principal) (*memrepo.BranchRef, error) {
	if err := ValidateShardName(repo, false); err != nil {
		return nil, err
	}
	brName, err := memrepo.NormalizeBranch(newBranch)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeRepoWrite(repo, repo+"#branch:"+brName, "branch", principal); err != nil {
		return nil, err
	}
	unlock := s.lockShard(repo)
	defer unlock()
	var headCommit string
	from := strings.TrimSpace(fromBranchOrCommit)
	if from == "" {
		from = memrepo.DefaultBranch
	}
	if c, cErr := s.ResolveCommitRef(ctx, repo, from); cErr == nil {
		headCommit = c.CommitHash
	}
	var genPubHex, genID string
	if pub, pErr := s.port.DeriveRepoGenesisPubKey(ctx, repo, brName); pErr == nil && pub != nil {
		genPubHex = hex.EncodeToString(pub.Compressed())
		genID = core.ShardIDFromPubkey(pub.Compressed())
	}
	now := time.Now().Unix()
	row := store.MemBranchRow{
		Repo:           repo,
		Branch:         brName,
		HeadCommit:     headCommit,
		GenesisPub:     genPubHex,
		GenesisShardID: genID,
		UpdatedAt:      now,
	}
	if err := s.db.CreateMemBranchStrict(row); err != nil {
		return nil, err
	}
	return &memrepo.BranchRef{
		Repo:           row.Repo,
		Branch:         row.Branch,
		HeadCommit:     row.HeadCommit,
		GenesisPub:     row.GenesisPub,
		GenesisShardID: row.GenesisShardID,
		UpdatedAt:      row.UpdatedAt,
	}, nil
}

// MergeBranch performs a true LCA 3-way Git merge commit of sourceBranch into targetBranch
// (using go-git/v5 Commit.MergeBase), preserving both parents' BSV UTXO links.
func (s *Service) MergeBranch(ctx context.Context, repo, sourceBranch, targetBranch, message string) (*memrepo.CommitObject, error) {
	res, err := s.MergeBranchWithOptions(ctx, MergeBranchInput{
		Repo:         repo,
		SourceBranch: sourceBranch,
		TargetBranch: targetBranch,
		Message:      message,
	})
	if err != nil {
		return nil, err
	}
	return res.Commit, nil
}

// CheckoutGitWorkspace materializes repo/branch into a real standard `.git` working tree on disk.
// Authorized CLKA-encrypted blobs are ephemerally decrypted into the local checkout copy while
// keeping SQLite, CHIRP, and exported capsules 100% encrypted at rest.
func (s *Service) CheckoutGitWorkspace(ctx context.Context, repo, branch, destDir string) (*memrepo.GitExportResult, error) {
	_, cap, _, err := s.ExportCapsule(ctx, repo, branch, "")
	if err != nil {
		return nil, err
	}
	if cap != nil && len(cap.Blobs) > 0 {
		decBlobs := make(map[string]memrepo.BlobObject, len(cap.Blobs))
		for bh, b := range cap.Blobs {
			if b.Encrypted && b.BorgGroup == "" && b.LatticeEpoch > 0 && len(b.KeyedMeta) > 0 {
				if pt, ok := s.openBlobPlaintext(ctx, b, ownerPrincipal(), "", nil, false, false); ok {
					cp := b
					cp.Encrypted = false
					cp.Inline = pt
					decBlobs[bh] = cp
					continue
				}
			}
			decBlobs[bh] = b
		}
		capCopy := *cap
		capCopy.Blobs = decBlobs
		cap = &capCopy
	}
	return memrepo.ExportGitWorkspace(destDir, cap, s.objects)
}

// ProveSubset builds a US 12,730,827 B2 level-6 proof-sharded subset proof over chosen seqs in batchN.
func (s *Service) ProveSubset(ctx context.Context, repo string, batchN int64, seqs []int64) (*memrepo.SubsetDisclosureProof, error) {
	return s.ProveSubsetAuthorized(ctx, repo, batchN, seqs, ownerPrincipal())
}

// ProveSubsetAuthorized enforces system-shard denial and Cedar read authorization on every disclosed fragment.
func (s *Service) ProveSubsetAuthorized(_ context.Context, repo string, batchN int64, seqs []int64, principal policy.Principal) (*memrepo.SubsetDisclosureProof, error) {
	isOp := isOperatorPrincipal(principal)
	if err := ValidateShardName(repo, isOp); err != nil {
		return nil, err
	}
	frags, err := s.db.ListBatchFragments(repo, batchN)
	if err != nil {
		return nil, err
	}
	if len(frags) == 0 {
		return nil, fmt.Errorf("memory: no fragments found in %s batch %d", repo, batchN)
	}
	wantSeq := make(map[int64]bool, len(seqs))
	for _, sq := range seqs {
		wantSeq[sq] = true
	}
	leaves := make([][]byte, len(frags))
	var indices []int
	for i, f := range frags {
		lb, err := hex.DecodeString(f.Leaf)
		if err != nil {
			return nil, err
		}
		leaves[i] = lb
		if wantSeq[f.Seq] {
			if !isOp {
				if f.Forgotten {
					return nil, fmt.Errorf("memory: cannot prove forgotten fragment %s#%d", repo, f.Seq)
				}
				if dec, aErr := s.AuthorizeAction(principal, "read", policy.Resource{
					Type:  "Fragment",
					ID:    fmt.Sprintf("%s#%d", f.Shard, f.Seq),
					Level: int64(f.Level),
					Shard: f.Shard,
					Class: f.Class,
				}); aErr != nil || !dec.Allow {
					return nil, fmt.Errorf("memory: cedar policy denied prove on %s#%d", f.Shard, f.Seq)
				}
			}
			indices = append(indices, i)
		}
	}
	if len(indices) == 0 {
		return nil, errors.New("memory: none of the requested seqs exist in target batch")
	}
	return memrepo.BuildSubsetProof(leaves, indices)
}
