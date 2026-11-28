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
		if disallowBorg {
			// DisallowBorg always blocks Borg-group secrets; if blob metadata is missing and entry is encrypted, fail closed.
			if hasB {
				if b.BorgGroup != "" {
					continue
				}
			} else if e.Encrypted {
				continue
			}
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

// prepareFragmentCommitTx writes blob & tree objects inside rtx and finalizes a signed CommitObject
// WITHOUT saving the commit row or advancing the branch head yet, allowing Remember() to include
// CommitHash and TransitionHash directly inside core.ComputeLeaf before committing atomically!
func (s *Service) prepareFragmentCommitTx(
	ctx context.Context,
	rtx *store.RepoTx,
	shard string,
	seq int64,
	now int64,
	blob *memrepo.BlobObject,
	branch, message string,
	extraParents []memrepo.ParentRef,
	authorPubHex, policyCommit, ontologyCommit string,
) (memrepo.CommitObject, string, error) {
	return s.prepareFragmentCommitWithExtrasTx(ctx, rtx, shard, seq, now, blob, nil, branch, message, extraParents, authorPubHex, policyCommit, ontologyCommit)
}

func (s *Service) prepareFragmentCommitWithExtrasTx(
	ctx context.Context,
	rtx *store.RepoTx,
	shard string,
	seq int64,
	now int64,
	blob *memrepo.BlobObject,
	extraBlobs []*memrepo.BlobObject,
	branch, message string,
	extraParents []memrepo.ParentRef,
	authorPubHex, policyCommit, ontologyCommit string,
) (memrepo.CommitObject, string, error) {
	brName, err := memrepo.NormalizeBranch(branch)
	if err != nil {
		brName = memrepo.DefaultBranch
	}
	if now == 0 {
		now = time.Now().Unix()
	}
	allBlobs := make([]*memrepo.BlobObject, 0, 1+len(extraBlobs))
	allBlobs = append(allBlobs, blob)
	for _, eb := range extraBlobs {
		if eb != nil {
			allBlobs = append(allBlobs, eb)
		}
	}
	for _, b := range allBlobs {
		blobRaw, mErr := memrepo.EncodeBlobObjectCBOR(*b)
		if mErr != nil {
			return memrepo.CommitObject{}, "", mErr
		}
		if err := rtx.PutMemObject(b.BlobHash, "blob", blobRaw, now); err != nil {
			return memrepo.CommitObject{}, "", err
		}
	}

	var parents []memrepo.ParentRef
	var expectedHead string
	var prevTree *memrepo.TreeObject

	if br, bErr := rtx.GetMemBranch(shard, brName); bErr == nil && br.HeadCommit != "" {
		expectedHead = br.HeadCommit
		if prevRow, rErr := rtx.GetMemCommit(br.HeadCommit); rErr == nil {
			prevC := commitFromRow(prevRow)
			if pt, tErr := loadRawTreeObjectTx(rtx, prevC.TreeHash); tErr == nil && pt != nil {
				prevTree = pt
			}
			parents = append(parents, memrepo.ParentRef{
				CommitHash: prevC.CommitHash,
				AnchorTxID: prevC.AnchorTxID,
			})
		}
	}
	parents = append(parents, extraParents...)

	var tr *memrepo.TreeObject
	if prevTree != nil && len(prevTree.Subtrees) > 0 {
		// Fast O(1) incremental 256-bucket subtree update: only read & rewrite touched bucket(s) + root map!
		subtrees := make(map[string]string, len(prevTree.Subtrees)+len(allBlobs))
		for k, v := range prevTree.Subtrees {
			subtrees[k] = v
		}
		dirtyByBucket := make(map[string][]memrepo.TreeEntry, len(allBlobs))
		for _, b := range allBlobs {
			bk := memrepo.TreeBucketKey(b.Path)
			dirtyByBucket[bk] = append(dirtyByBucket[bk], memrepo.TreeEntry{
				Mode:          "100644",
				Path:          b.Path,
				BlobHash:      b.BlobHash,
				MediaType:     b.MediaType,
				Size:          b.Size,
				ContentSHA256: b.ContentSHA256,
				ChirpURL:      b.ChirpURL,
				Encrypted:     b.Encrypted,
				Level:         b.Level,
				Class:         b.Class,
			})
		}
		for bk, updates := range dirtyByBucket {
			bucketEntriesByPath := make(map[string]memrepo.TreeEntry)
			if oldHash := subtrees[bk]; oldHash != "" {
				if oldBucket, bErr := loadRawTreeObjectTx(rtx, oldHash); bErr == nil && oldBucket != nil {
					for _, e := range oldBucket.Entries {
						bucketEntriesByPath[e.Path] = e
					}
				}
			}
			for _, u := range updates {
				bucketEntriesByPath[u.Path] = u
			}
			bList := make([]memrepo.TreeEntry, 0, len(bucketEntriesByPath))
			for _, e := range bucketEntriesByPath {
				bList = append(bList, e)
			}
			bTree, bErr := memrepo.BuildTree(bList)
			if bErr != nil {
				return memrepo.CommitObject{}, "", bErr
			}
			bRaw, mErr := memrepo.EncodeTreeObjectCBOR(*bTree)
			if mErr != nil {
				return memrepo.CommitObject{}, "", mErr
			}
			if err := rtx.PutMemObject(bTree.TreeHash, "tree", bRaw, now); err != nil {
				return memrepo.CommitObject{}, "", err
			}
			subtrees[bk] = bTree.TreeHash
		}
		tr, err = memrepo.BuildShardedRootTree(subtrees)
		if err != nil {
			return memrepo.CommitObject{}, "", err
		}
	} else {
		// Fresh repo or automatic in-place upgrade of a legacy v1 flat TreeObject into 256-bucket subtrees.
		entriesByPath := make(map[string]memrepo.TreeEntry)
		if prevTree != nil {
			for _, e := range prevTree.Entries {
				entriesByPath[e.Path] = e
			}
		}
		for _, b := range allBlobs {
			entriesByPath[b.Path] = memrepo.TreeEntry{
				Mode:          "100644",
				Path:          b.Path,
				BlobHash:      b.BlobHash,
				MediaType:     b.MediaType,
				Size:          b.Size,
				ContentSHA256: b.ContentSHA256,
				ChirpURL:      b.ChirpURL,
				Encrypted:     b.Encrypted,
				Level:         b.Level,
				Class:         b.Class,
			}
		}
		entries := make([]memrepo.TreeEntry, 0, len(entriesByPath))
		for _, e := range entriesByPath {
			entries = append(entries, e)
		}
		var buckets map[string]memrepo.TreeObject
		tr, buckets, err = memrepo.BuildShardedTree(entries)
		if err != nil {
			return memrepo.CommitObject{}, "", err
		}
		for _, bt := range buckets {
			bRaw, mErr := memrepo.EncodeTreeObjectCBOR(bt)
			if mErr != nil {
				return memrepo.CommitObject{}, "", mErr
			}
			if err := rtx.PutMemObject(bt.TreeHash, "tree", bRaw, now); err != nil {
				return memrepo.CommitObject{}, "", err
			}
		}
	}
	treeRaw, err := memrepo.EncodeTreeObjectCBOR(*tr)
	if err != nil {
		return memrepo.CommitObject{}, "", err
	}
	if err := rtx.PutMemObject(tr.TreeHash, "tree", treeRaw, now); err != nil {
		return memrepo.CommitObject{}, "", err
	}

	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = fmt.Sprintf("Commit %s (#%d)", blob.Path, seq)
	}
	c, err := memrepo.FinalizeCommit(memrepo.CommitObject{
		Version:        1,
		Repo:           shard,
		Branch:         brName,
		Seq:            seq,
		Parents:        parents,
		TreeHash:       tr.TreeHash,
		AuthorPub:      authorPubHex,
		ModelID:        s.modelID.Provider + "/" + s.modelID.Release,
		PolicyCommit:   policyCommit,
		OntologyCommit: ontologyCommit,
		Message:        msg,
		Timestamp:      now,
		AnchorN:        -1,
	}, func(digest []byte) ([]byte, error) {
		return s.port.SignDigest(ctx, digest)
	})
	if err != nil {
		return memrepo.CommitObject{}, "", err
	}
	return c, expectedHead, nil
}

// recordFragmentCommitTx appends a signed Git/Metanet CommitObject inside an active RepoTx
// (used by backfillLegacyRepos for pre-existing fragments).
func (s *Service) recordFragmentCommitTx(
	ctx context.Context,
	rtx *store.RepoTx,
	stored store.FragmentRecord,
	blob *memrepo.BlobObject,
	branch, message string,
	extraParents []memrepo.ParentRef,
	genPub, genID string,
) (memrepo.CommitObject, error) {
	c, expectedHead, err := s.prepareFragmentCommitTx(
		ctx, rtx, stored.Shard, stored.Seq, stored.CreatedAt,
		blob, branch, message, extraParents,
		s.commitAuthorPubHex(ctx), s.PolicyCommit().Hash, s.OntologyHead(),
	)
	if err != nil {
		return memrepo.CommitObject{}, err
	}
	c.AnchorTxID = stored.AnchorTxID
	c.AnchorN = stored.BatchN
	c.FragmentLeaf = stored.Leaf
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
				blob, bErr := s.buildFragmentBlob(relPath, f.MediaType, f.Data, nil, f.ChirpURL, f.ContentSHA256, f.BorgGroup, f.BorgEpoch, f.LatticeEpoch, f.KeyedMeta, f.Level, f.Class)
				if bErr != nil {
					return bErr
				}
				msg := summarizeCommitText(string(f.Data), relPath)
				if f.BorgGroup != "" || f.LatticeEpoch > 0 {
					msg = fmt.Sprintf("encrypted commit (%s)", relPath)
				}
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

func decodeStoredTreeObject(raw []byte) (*memrepo.TreeObject, error) {
	if len(raw) > 0 && raw[0] == '{' {
		var tr memrepo.TreeObject
		if err := json.Unmarshal(raw, &tr); err != nil {
			return nil, err
		}
		return &tr, nil
	}
	tr, err := memrepo.DecodeTreeObjectCBOR(raw)
	if err != nil {
		return nil, err
	}
	return &tr, nil
}

func decodeStoredBlobObject(raw []byte) (*memrepo.BlobObject, error) {
	if len(raw) > 0 && raw[0] == '{' {
		var b memrepo.BlobObject
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, err
		}
		return &b, nil
	}
	b, err := memrepo.DecodeBlobObjectCBOR(raw)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func loadRawTreeObjectTx(rtx *store.RepoTx, hash string) (*memrepo.TreeObject, error) {
	_, raw, err := rtx.GetMemObject(hash)
	if err != nil {
		return nil, err
	}
	return decodeStoredTreeObject(raw)
}

func loadTreeEntriesTx(rtx *store.RepoTx, hash string) ([]memrepo.TreeEntry, error) {
	tr, err := loadRawTreeObjectTx(rtx, hash)
	if err != nil || tr == nil {
		return nil, err
	}
	if len(tr.Subtrees) > 0 && len(tr.Entries) == 0 {
		return memrepo.ResolveTreeEntries(*tr, func(bh string) (memrepo.TreeObject, bool) {
			bt, bErr := loadRawTreeObjectTx(rtx, bh)
			if bErr != nil || bt == nil {
				return memrepo.TreeObject{}, false
			}
			return *bt, true
		})
	}
	return tr.Entries, nil
}

func loadBlobObjectTx(rtx *store.RepoTx, hash string) (*memrepo.BlobObject, error) {
	_, raw, err := rtx.GetMemObject(hash)
	if err != nil {
		return nil, err
	}
	return decodeStoredBlobObject(raw)
}

func (s *Service) loadRawTreeObject(hash string) (*memrepo.TreeObject, error) {
	_, raw, err := s.db.GetMemObject(hash)
	if err != nil {
		return nil, err
	}
	return decodeStoredTreeObject(raw)
}

func (s *Service) loadTreeObject(hash string) (*memrepo.TreeObject, error) {
	tr, err := s.loadRawTreeObject(hash)
	if err != nil || tr == nil {
		return nil, err
	}
	if len(tr.Subtrees) > 0 && len(tr.Entries) == 0 {
		entries, rErr := memrepo.ResolveTreeEntries(*tr, func(bh string) (memrepo.TreeObject, bool) {
			bt, bErr := s.loadRawTreeObject(bh)
			if bErr != nil || bt == nil {
				return memrepo.TreeObject{}, false
			}
			return *bt, true
		})
		if rErr != nil {
			return nil, rErr
		}
		tr.Entries = entries
	}
	return tr, nil
}

func (s *Service) loadBlobObject(hash string) (*memrepo.BlobObject, error) {
	_, raw, err := s.db.GetMemObject(hash)
	if err != nil {
		return nil, err
	}
	return decodeStoredBlobObject(raw)
}

func rowFromCommit(c memrepo.CommitObject) store.MemCommitRow {
	pJSON, _ := json.Marshal(c.Parents)
	var gitMetaJSON []byte
	if c.GitMeta != nil {
		gitMetaJSON, _ = json.Marshal(c.GitMeta)
	}
	return store.MemCommitRow{
		CommitHash:     c.CommitHash,
		Repo:           c.Repo,
		Branch:         c.Branch,
		Seq:            c.Seq,
		CommitKind:     c.CommitKind,
		TreeHash:       c.TreeHash,
		MPERoot:        c.MPERoot,
		ParentsJSON:    pJSON,
		AuthorPub:      c.AuthorPub,
		ModelID:        c.ModelID,
		PolicyCommit:   c.PolicyCommit,
		OntologyCommit: c.OntologyCommit,
		Message:        c.Message,
		Timestamp:      c.Timestamp,
		Signature:      c.Signature,
		AnchorTxID:     c.AnchorTxID,
		AnchorN:        c.AnchorN,
		FragmentLeaf:   c.FragmentLeaf,
		GitMetaJSON:    gitMetaJSON,
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
	signedSeq := r.Seq
	if r.OriginSeq > 0 {
		signedSeq = r.OriginSeq
	}
	return memrepo.CommitObject{
		Version:        1,
		CommitHash:     r.CommitHash,
		Repo:           signedRepo,
		Branch:         signedBranch,
		Seq:            signedSeq,
		CommitKind:     r.CommitKind,
		Parents:        parents,
		TreeHash:       r.TreeHash,
		MPERoot:        r.MPERoot,
		AuthorPub:      r.AuthorPub,
		ModelID:        r.ModelID,
		PolicyCommit:   r.PolicyCommit,
		OntologyCommit: r.OntologyCommit,
		Message:        r.Message,
		Timestamp:      r.Timestamp,
		Signature:      r.Signature,
		AnchorTxID:     r.AnchorTxID,
		AnchorN:        r.AnchorN,
		FragmentLeaf:   r.FragmentLeaf,
		GitMeta:        gitMeta,
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
