package memory

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

func ownerPrincipal() policy.Principal {
	return policy.Principal{Type: "Agent", ID: "owner", Level: 100}
}

func (s *Service) authorizeRepoWrite(repo, resID, class string, principal *policy.Principal) error {
	writePrincipal := ownerPrincipal()
	if principal != nil && principal.ID != "" {
		writePrincipal = *principal
	}
	if class == "" {
		class = "repo"
	}
	dec, err := s.AuthorizeAction(writePrincipal, "write", policy.Resource{
		Type:  "Fragment",
		ID:    resID,
		Level: 0,
		Shard: repo,
		Class: class,
	})
	if err != nil {
		return fmt.Errorf("memory: cedar write policy error: %w", err)
	}
	if !dec.Allow {
		return fmt.Errorf("memory: cedar policy denied write on %s", resID)
	}
	return nil
}

// filterTreeForPrincipal removes forgotten blobs, Borg-encrypted entries (when disallowBorg),
// and any TreeEntry whose Cedar read policy denies principal.
func (s *Service) filterTreeForPrincipal(
	repo, commitHash string,
	tr *memrepo.TreeObject,
	blobs map[string]memrepo.BlobObject,
	principal policy.Principal,
	disallowBorg bool,
) (*memrepo.TreeObject, map[string]memrepo.BlobObject) {
	if tr == nil {
		return nil, map[string]memrepo.BlobObject{}
	}
	if principal.ID == "" {
		principal = ownerPrincipal()
	}
	s.mu.RLock()
	polText := s.policyText
	s.mu.RUnlock()

	short := commitHash
	if len(short) > 7 {
		short = short[:7]
	}
	keptEntries := make([]memrepo.TreeEntry, 0, len(tr.Entries))
	keptBlobs := make(map[string]memrepo.BlobObject, len(tr.Entries))
	for _, e := range tr.Entries {
		b, hasB := blobs[e.BlobHash]
		if hasB && b.Forgotten {
			continue
		}
		if disallowBorg && (e.Encrypted || (hasB && b.Encrypted)) {
			continue
		}
		lvl := e.Level
		if hasB && b.Level > lvl {
			lvl = b.Level
		}
		cls := e.Class
		if cls == "" && hasB {
			cls = b.Class
		}
		if cls == "" {
			cls = "repo"
		}
		dec, err := policy.Authorize(polText, principal, "read", policy.Resource{
			Type:  "Fragment",
			ID:    fmt.Sprintf("%s@%s:%s", repo, short, e.Path),
			Level: int64(lvl),
			Shard: repo,
			Class: cls,
		})
		if err != nil || !dec.Allow {
			continue
		}
		keptEntries = append(keptEntries, e)
		if hasB {
			keptBlobs[e.BlobHash] = b
		}
	}
	filtered := &memrepo.TreeObject{
		TreeHash: tr.TreeHash,
		Entries:  keptEntries,
	}
	return filtered, keptBlobs
}

// recordFragmentCommitTx appends a signed Git/Metanet CommitObject inside an active RepoTx.
func (s *Service) recordFragmentCommitTx(
	ctx context.Context,
	rtx *store.RepoTx,
	stored store.FragmentRecord,
	blob *memrepo.BlobObject,
	branch, message string,
	extraParents []memrepo.ParentRef,
	genPub, genID string,
) (memrepo.CommitObject, error) {
	brName, err := memrepo.NormalizeBranch(branch)
	if err != nil {
		brName = memrepo.DefaultBranch
	}
	now := stored.CreatedAt
	if now == 0 {
		now = time.Now().Unix()
	}
	blobRaw, err := json.Marshal(blob)
	if err != nil {
		return memrepo.CommitObject{}, err
	}
	if err := rtx.PutMemObject(blob.BlobHash, "blob", blobRaw, now); err != nil {
		return memrepo.CommitObject{}, err
	}

	entriesByPath := make(map[string]memrepo.TreeEntry)
	var parents []memrepo.ParentRef
	var expectedHead string

	if br, bErr := rtx.GetMemBranch(stored.Shard, brName); bErr == nil && br.HeadCommit != "" {
		expectedHead = br.HeadCommit
		if prevRow, rErr := rtx.GetMemCommit(br.HeadCommit); rErr == nil {
			prevC := commitFromRow(prevRow)
			if _, treeRaw, tErr := rtx.GetMemObject(prevC.TreeHash); tErr == nil {
				var prevTree memrepo.TreeObject
				if json.Unmarshal(treeRaw, &prevTree) == nil {
					for _, e := range prevTree.Entries {
						entriesByPath[e.Path] = e
					}
				}
			}
			parents = append(parents, memrepo.ParentRef{
				CommitHash: prevC.CommitHash,
				AnchorTxID: prevC.AnchorTxID,
			})
		}
	}
	parents = append(parents, extraParents...)

	entriesByPath[blob.Path] = memrepo.TreeEntry{
		Mode:          "100644",
		Path:          blob.Path,
		BlobHash:      blob.BlobHash,
		MediaType:     blob.MediaType,
		Size:          blob.Size,
		ContentSHA256: blob.ContentSHA256,
		ChirpURL:      blob.ChirpURL,
		Encrypted:     blob.Encrypted,
		Level:         blob.Level,
		Class:         blob.Class,
	}
	entries := make([]memrepo.TreeEntry, 0, len(entriesByPath))
	for _, e := range entriesByPath {
		entries = append(entries, e)
	}
	tr, err := memrepo.BuildTree(entries)
	if err != nil {
		return memrepo.CommitObject{}, err
	}
	treeRaw, err := json.Marshal(tr)
	if err != nil {
		return memrepo.CommitObject{}, err
	}
	if err := rtx.PutMemObject(tr.TreeHash, "tree", treeRaw, now); err != nil {
		return memrepo.CommitObject{}, err
	}

	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = fmt.Sprintf("Commit %s (#%d)", blob.Path, stored.Seq)
	}
	c, err := memrepo.FinalizeCommit(memrepo.CommitObject{
		Version:      1,
		Repo:         stored.Shard,
		Branch:       brName,
		Seq:          stored.Seq,
		Parents:      parents,
		TreeHash:     tr.TreeHash,
		AuthorPub:    s.commitAuthorPubHex(ctx),
		ModelID:      s.modelID.Provider + "/" + s.modelID.Release,
		PolicyCommit: s.PolicyCommit().Hash,
		Message:      msg,
		Timestamp:    now,
		AnchorTxID:   stored.AnchorTxID,
		AnchorN:      stored.BatchN,
		FragmentLeaf: stored.Leaf,
	}, func(digest []byte) ([]byte, error) {
		return s.port.SignDigest(ctx, digest)
	})
	if err != nil {
		return memrepo.CommitObject{}, err
	}
	if err := rtx.SaveMemCommit(rowFromCommit(c)); err != nil {
		return memrepo.CommitObject{}, err
	}
	if err := rtx.CompareAndSwapMemBranch(store.MemBranchRow{
		Repo:           c.Repo,
		Branch:         c.Branch,
		HeadCommit:     c.CommitHash,
		GenesisPub:     genPub,
		GenesisShardID: genID,
		UpdatedAt:      c.Timestamp,
	}, expectedHead); err != nil {
		return memrepo.CommitObject{}, err
	}
	if err := rtx.UpdateFragmentCommitHash(stored.Shard, stored.Seq, c.CommitHash); err != nil {
		return memrepo.CommitObject{}, err
	}
	return c, nil
}

// backfillLegacyRepos performs a one-time startup migration synthesizing mem_commits for any
// legacy fragments that predate the Memory Repo tables. Runs once in New() instead of mutating
// SQLite lazily inside read paths.
func (s *Service) backfillLegacyRepos(ctx context.Context) error {
	repos, err := s.db.ListAllMemRepos()
	if err != nil || len(repos) == 0 {
		return err
	}
	for _, repo := range repos {
		if IsSystemShard(repo) {
			continue
		}
		cnt, cErr := s.db.CountMemCommits(repo)
		if cErr != nil {
			return cErr
		}
		if cnt > 0 {
			continue
		}
		frags, fErr := s.db.ListFragments(repo)
		if fErr != nil || len(frags) == 0 {
			if fErr != nil {
				return fErr
			}
			continue
		}
		anchors, _ := s.db.ListAnchors(repo)
		genPub, genID, _ := s.resolveRepoGenesis(ctx, repo, anchors)
		if err := s.db.RunRepoTx(func(rtx *store.RepoTx) error {
			for _, f := range frags {
				if f.Forgotten {
					continue
				}
				relPath := f.Path
				if relPath == "" {
					relPath = fmt.Sprintf("facts/%04d.md", f.Seq)
				}
				blob, bErr := s.buildFragmentBlob(relPath, f.MediaType, f.Data, nil, f.ChirpURL, f.ContentSHA256, f.BorgGroup, f.BorgEpoch, f.KeyedMeta, f.Level, f.Class)
				if bErr != nil {
					return bErr
				}
				msg := summarizeCommitText(string(f.Data), relPath)
				if _, err := s.recordFragmentCommitTx(ctx, rtx, f, blob, memrepo.DefaultBranch, msg, nil, genPub, genID); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) commitAuthorPubHex(ctx context.Context) string {
	if pub, err := s.port.SigningPubKey(ctx); err == nil && pub != nil {
		return hex.EncodeToString(pub.Compressed())
	}
	return s.port.IdentityPubHex()
}

func (s *Service) loadTreeObject(hash string) (*memrepo.TreeObject, error) {
	_, raw, err := s.db.GetMemObject(hash)
	if err != nil {
		return nil, err
	}
	var tr memrepo.TreeObject
	if err := json.Unmarshal(raw, &tr); err != nil {
		return nil, err
	}
	return &tr, nil
}

func (s *Service) loadBlobObject(hash string) (*memrepo.BlobObject, error) {
	_, raw, err := s.db.GetMemObject(hash)
	if err != nil {
		return nil, err
	}
	var b memrepo.BlobObject
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

func rowFromCommit(c memrepo.CommitObject) store.MemCommitRow {
	pJSON, _ := json.Marshal(c.Parents)
	var gitMetaJSON []byte
	if c.GitMeta != nil {
		gitMetaJSON, _ = json.Marshal(c.GitMeta)
	}
	return store.MemCommitRow{
		CommitHash:   c.CommitHash,
		Repo:         c.Repo,
		Branch:       c.Branch,
		Seq:          c.Seq,
		TreeHash:     c.TreeHash,
		MPERoot:      c.MPERoot,
		ParentsJSON:  pJSON,
		AuthorPub:    c.AuthorPub,
		ModelID:      c.ModelID,
		PolicyCommit: c.PolicyCommit,
		Message:      c.Message,
		Timestamp:    c.Timestamp,
		Signature:    c.Signature,
		AnchorTxID:   c.AnchorTxID,
		AnchorN:      c.AnchorN,
		FragmentLeaf: c.FragmentLeaf,
		GitMetaJSON:  gitMetaJSON,
	}
}

func commitFromRow(r store.MemCommitRow) memrepo.CommitObject {
	var parents []memrepo.ParentRef
	if len(r.ParentsJSON) > 0 {
		_ = json.Unmarshal(r.ParentsJSON, &parents)
	}
	var gitMeta *memrepo.GitCommitMeta
	if len(r.GitMetaJSON) > 0 {
		var gm memrepo.GitCommitMeta
		if json.Unmarshal(r.GitMetaJSON, &gm) == nil {
			gitMeta = &gm
		}
	}
	signedRepo := r.Repo
	if r.OriginRepo != "" {
		signedRepo = r.OriginRepo
	}
	signedBranch := r.Branch
	if r.OriginBranch != "" {
		signedBranch = r.OriginBranch
	}
	return memrepo.CommitObject{
		Version:      1,
		CommitHash:   r.CommitHash,
		Repo:         signedRepo,
		Branch:       signedBranch,
		Seq:          r.Seq,
		Parents:      parents,
		TreeHash:     r.TreeHash,
		MPERoot:      r.MPERoot,
		AuthorPub:    r.AuthorPub,
		ModelID:      r.ModelID,
		PolicyCommit: r.PolicyCommit,
		Message:      r.Message,
		Timestamp:    r.Timestamp,
		Signature:    r.Signature,
		AnchorTxID:   r.AnchorTxID,
		AnchorN:      r.AnchorN,
		FragmentLeaf: r.FragmentLeaf,
		GitMeta:      gitMeta,
	}
}

func tagFromRow(r store.MemTagRow) memrepo.TagRef {
	return memrepo.TagRef{
		Repo:       r.Repo,
		Tag:        r.Tag,
		CommitHash: r.CommitHash,
		AnchorTxID: r.AnchorTxID,
		AnchorN:    r.AnchorN,
		Message:    r.Message,
		AuthorPub:  r.AuthorPub,
		Signature:  r.Signature,
		CreatedAt:  r.CreatedAt,
	}
}

// collectReachableCommitsTopo performs a post-order DFS from headHash back to genesis (or stopping
// at sinceCommit and external cross-repo parents), emitting commits in strict topological order
// (every local parent strictly precedes its children, including merged side-branch commits).
func (s *Service) collectReachableCommitsTopo(headHash, sinceCommit string) []memrepo.CommitObject {
	if headHash == "" || headHash == sinceCommit {
		return nil
	}
	visited := make(map[string]bool)
	var out []memrepo.CommitObject
	var dfs func(h string)
	dfs = func(h string) {
		if h == "" || h == sinceCommit || visited[h] {
			return
		}
		visited[h] = true
		row, err := s.db.GetMemCommit(h)
		if err != nil {
			return
		}
		c := commitFromRow(row)
		for _, p := range c.Parents {
			if p.RepoGenesis != "" {
				continue // cross-repo Market parent lives in seller's repo
			}
			dfs(p.CommitHash)
		}
		out = append(out, c)
	}
	dfs(headHash)
	return out
}

func summarizeCommitText(text, fallbackPath string) string {
	line := strings.TrimSpace(text)
	if idx := strings.IndexAny(line, "\r\n"); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	if line == "" {
		return "Commit " + fallbackPath
	}
	if len(line) > 84 {
		return line[:81] + "..."
	}
	return line
}
