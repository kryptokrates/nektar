package memory

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

// CapsuleExportResult summarizes an exported self-verifying .nkt Memory Capsule.
type CapsuleExportResult struct {
	Repo           string `json:"repo"`
	Branch         string `json:"branch"`
	BaseCommit     string `json:"baseCommit,omitempty"`
	HeadCommit     string `json:"headCommit,omitempty"`
	GenesisShardID string `json:"genesisShardId,omitempty"`
	ChirpURL       string `json:"chirpUrl"`
	RootHex        string `json:"rootHex"`
	CommitCount    int    `json:"commitCount"`
	BlobCount      int    `json:"blobCount"`
	AnchorCount    int    `json:"anchorCount"`
	SizeBytes      int    `json:"sizeBytes"`
}

// CapsuleImportResult summarizes a verified .nkt Memory Capsule import (or Market fork/merge).
type CapsuleImportResult struct {
	Repo            string                       `json:"repo"`
	Branch          string                       `json:"branch"`
	ImportedCommits int                          `json:"importedCommits"`
	ImportedFrags   int                          `json:"importedFragments"`
	MergeCommitHash string                       `json:"mergeCommitHash,omitempty"`
	Verification    *memrepo.CapsuleVerifyReport `json:"verification"`
}

// CapsuleInspectResult is a read-only offline verification & preview summary of a .nkt Capsule.
type CapsuleInspectResult struct {
	Repo           string                        `json:"repo"`
	Branch         string                        `json:"branch"`
	GenesisPub     string                        `json:"genesisPub,omitempty"`
	GenesisShardID string                        `json:"genesisShardId,omitempty"`
	OwnerPub       string                        `json:"ownerPub,omitempty"`
	HeadCommit     string                        `json:"headCommit,omitempty"`
	ExportedAt     int64                         `json:"exportedAt"`
	Verification   *memrepo.CapsuleVerifyReport  `json:"verification"`
	Commits        []memrepo.CommitObject        `json:"commits"`
	Tree           *memrepo.TreeObject           `json:"tree,omitempty"`
	Blobs          map[string]memrepo.BlobObject `json:"blobs,omitempty"`
	Anchors        []RepoAnchorSummary           `json:"anchors,omitempty"`
	LocalExists    bool                          `json:"localExists"`
	DiffVsLocal    []memrepo.TreeDiffEntry       `json:"diffVsLocal,omitempty"`
}

// ExportCapsule packages repo (full history or incremental sinceCommit..head delta) into a
// self-verifying .nkt MemoryCapsule, stores it in BRC-167 CHIRP, and returns metadata + raw bytes.
func (s *Service) ExportCapsule(ctx context.Context, repo, branch, sinceCommit string) (*CapsuleExportResult, *memrepo.MemoryCapsule, []byte, error) {
	return s.ExportCapsuleAuthorized(ctx, repo, branch, sinceCommit, ownerPrincipal(), false)
}

// ExportCapsuleAuthorized packages repo into a .nkt MemoryCapsule while enforcing Cedar read
// authorization and excluding forgotten/unauthorized fragments and blobs.
func (s *Service) ExportCapsuleAuthorized(ctx context.Context, repo, branch, sinceCommit string, principal policy.Principal, disallowBorg bool) (*CapsuleExportResult, *memrepo.MemoryCapsule, []byte, error) {
	if err := ValidateShardName(repo, false); err != nil {
		return nil, nil, nil, err
	}
	brName, err := memrepo.NormalizeBranch(branch)
	if err != nil {
		return nil, nil, nil, err
	}
	if principal.ID == "" {
		principal = ownerPrincipal()
	}

	// Walk the full reachable commit DAG from branch head in topological order (parents before children,
	// including merged side-branch commits like agent/codex), stopping at sinceCommit or cross-repo forks.
	var headCommitHash string
	if br, bErr := s.db.GetMemBranch(repo, brName); bErr == nil && br.HeadCommit != "" {
		headCommitHash = br.HeadCommit
	}
	chrono := s.collectReachableCommitsTopo(headCommitHash, sinceCommit)
	if len(chrono) == 0 {
		rows, err := s.db.ListAllMemCommits(repo)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, r := range rows {
			if r.Branch == brName {
				chrono = append(chrono, commitFromRow(r))
			}
		}
	}

	trees, blobs, _ := s.collectCommitDAGObjects(repo, chrono, principal, disallowBorg, false)
	anchors, _ := s.db.ListAnchors(repo)
	frags, _ := s.db.ListFragments(repo)
	s.mu.RLock()
	polText := s.policyText
	s.mu.RUnlock()
	activeFrags := make([]store.FragmentRecord, 0, len(frags))
	for _, f := range frags {
		if f.Forgotten {
			continue
		}
		if disallowBorg && f.BorgGroup != "" {
			continue
		}
		dec, aErr := policy.Authorize(polText, principal, "read", policy.Resource{
			Type:  "Fragment",
			ID:    fmt.Sprintf("%s#%d", f.Shard, f.Seq),
			Level: int64(f.Level),
			Shard: f.Shard,
			Class: f.Class,
		})
		if aErr != nil || !dec.Allow {
			continue
		}
		activeFrags = append(activeFrags, f)
	}
	genPub, genID, _ := s.resolveRepoGenesis(ctx, repo, anchors)
	headHash := ""
	if len(chrono) > 0 {
		headHash = chrono[len(chrono)-1].CommitHash
	}
	cap := &memrepo.MemoryCapsule{
		Format:         memrepo.CapsuleFormatVersion,
		Repo:           repo,
		Branch:         brName,
		GenesisPub:     genPub,
		GenesisShardID: genID,
		OwnerPub:       s.port.IdentityPubHex(),
		BaseCommit:     sinceCommit,
		HeadCommit:     headHash,
		ExportedAt:     time.Now().Unix(),
		Commits:        chrono,
		Trees:          trees,
		Blobs:          blobs,
		Anchors:        anchors,
		Fragments:      activeFrags,
		BatchProofs:    s.buildCapsuleBatchProofs(repo, anchors, activeFrags, chrono),
	}
	encoded, err := memrepo.EncodeCapsule(cap)
	if err != nil {
		return nil, nil, nil, err
	}
	chirpURL, rootHash, _, err := bstack.StoreChirpWithHashes(s.objects, encoded)
	if err != nil {
		return nil, nil, nil, err
	}

	res := &CapsuleExportResult{
		Repo:           repo,
		Branch:         brName,
		BaseCommit:     sinceCommit,
		HeadCommit:     headHash,
		GenesisShardID: genID,
		ChirpURL:       chirpURL,
		RootHex:        hex.EncodeToString(rootHash[:]),
		CommitCount:    len(chrono),
		BlobCount:      len(blobs),
		AnchorCount:    len(anchors),
		SizeBytes:      len(encoded),
	}
	return res, cap, encoded, nil
}

// ImportCapsule verifies and imports a .nkt MemoryCapsule (raw bytes or chirp:// URL) into targetRepo.
// When mergeIntoMain is true, it imports into a provenance branch ("fork/<genesis8>") and creates a
// 2-parent Merge Commit on targetBranch linking directly to the source's SPV-verified NektarAnchor UTXO!
func (s *Service) ImportCapsule(ctx context.Context, raw []byte, chirpURL, targetRepo, targetBranch string, mergeIntoMain bool) (*CapsuleImportResult, error) {
	return s.ImportCapsuleAuthorized(ctx, raw, chirpURL, targetRepo, targetBranch, mergeIntoMain, nil)
}

// ImportCapsuleAuthorized enforces Cedar write policy, verifies the capsule, and persists all
// blobs, trees, commits, branch ref, anchors, and fragments atomically in ONE SQLite transaction.
func (s *Service) ImportCapsuleAuthorized(ctx context.Context, raw []byte, chirpURL, targetRepo, targetBranch string, mergeIntoMain bool, principal *policy.Principal) (*CapsuleImportResult, error) {
	if len(raw) == 0 && strings.TrimSpace(chirpURL) != "" {
		var err error
		raw, err = bstack.ResolveChirp(s.objects, strings.TrimSpace(chirpURL))
		if err != nil {
			return nil, fmt.Errorf("memory: resolve capsule chirp: %w", err)
		}
	}
	cap, err := memrepo.DecodeCapsule(raw)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(targetRepo) == "" {
		targetRepo = cap.Repo
	}
	if err := ValidateShardName(targetRepo, false); err != nil {
		return nil, err
	}
	dstBranch, err := memrepo.NormalizeBranch(targetBranch)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeRepoWrite(targetRepo, targetRepo+"#import", "capsule-import", principal); err != nil {
		return nil, err
	}
	ver, err := memrepo.VerifyCapsule(ctx, cap, s.objects, s.port.Tracker(), s.port.Network() == "memory")
	if err != nil {
		return nil, err
	}
	if !ver.OK {
		return &CapsuleImportResult{Repo: targetRepo, Branch: dstBranch, Verification: ver}, fmt.Errorf("memory: capsule verification failed: %s", ver.Reason)
	}

	unlock := s.lockShard(targetRepo)
	locked := true
	release := func() {
		if locked {
			locked = false
			unlock()
		}
	}
	defer release()

	// Isolate foreign Seller anchors under "import:<genesis>" so importing a third-party capsule
	// into targetRepo NEVER overwrites or bricks the local node's own NektarAnchor UTXO chain!
	isForeignImport := mergeIntoMain || !strings.EqualFold(cap.OwnerPub, s.port.IdentityPubHex()) || !strings.EqualFold(targetRepo, cap.Repo)
	anchorTargetShard := targetRepo
	if isForeignImport {
		tag := cap.GenesisShardID
		if tag == "" {
			tag = cap.Repo
		}
		if tag == "" {
			tag = "external"
		}
		anchorTargetShard = "import:" + tag
	}

	// F20: fail closed BEFORE mutating local state if any imported anchor conflicts with an existing
	// anchor at batch n (always checking targetRepo when importing into the same repo name, plus
	// anchorTargetShard when foreign anchors are isolated under "import:<genesis>").
	if len(cap.Anchors) > 0 {
		checkShards := []string{anchorTargetShard}
		if anchorTargetShard != targetRepo && strings.EqualFold(targetRepo, cap.Repo) {
			checkShards = append([]string{targetRepo}, checkShards...)
		}
		for _, chkShard := range checkShards {
			existingAnchors, aErr := s.db.ListAnchors(chkShard)
			if aErr != nil {
				return nil, fmt.Errorf("memory: list local anchors for conflict check: %w", aErr)
			}
			existingByN := make(map[int64]store.BatchAnchor, len(existingAnchors))
			for _, ea := range existingAnchors {
				existingByN[ea.N] = ea
			}
			for _, a := range cap.Anchors {
				if ea, exists := existingByN[a.N]; exists {
					if ea.TxID != a.TxID || ea.RootHex != a.RootHex || ea.ChainedHex != a.ChainedHex || ea.Size != a.Size {
						return nil, fmt.Errorf("memory: capsule anchor conflict on %s batch n=%d (local tx %s vs imported tx %s)", chkShard, a.N, ea.TxID, a.TxID)
					}
				}
			}
		}
	}

	now := time.Now().Unix()
	importBranch := dstBranch
	if mergeIntoMain {
		tag := cap.GenesisShardID
		if len(tag) > 8 {
			tag = tag[:8]
		}
		if tag == "" {
			tag = "import"
		}
		importBranch = "fork/" + tag
	}

	importedCommits := 0
	importedFrags := 0
	var lastHead memrepo.CommitObject

	if err := s.db.RunRepoTx(func(rtx *store.RepoTx) error {
		for _, b := range cap.Blobs {
			bRaw, mErr := json.Marshal(b)
			if mErr != nil {
				return fmt.Errorf("memory: marshal imported blob %s: %w", b.BlobHash, mErr)
			}
			if err := rtx.PutMemObject(b.BlobHash, "blob", bRaw, now); err != nil {
				return fmt.Errorf("memory: save imported blob %s: %w", b.BlobHash, err)
			}
		}
		for _, tr := range cap.Trees {
			tRaw, mErr := json.Marshal(tr)
			if mErr != nil {
				return fmt.Errorf("memory: marshal imported tree %s: %w", tr.TreeHash, mErr)
			}
			if err := rtx.PutMemObject(tr.TreeHash, "tree", tRaw, now); err != nil {
				return fmt.Errorf("memory: save imported tree %s: %w", tr.TreeHash, err)
			}
		}
		for _, c := range cap.Commits {
			row := rowFromCommit(c)
			// Preserve original signed (Repo, Branch) so VerifyCommitObject stays valid on re-export.
			row.OriginRepo = c.Repo
			row.OriginBranch = c.Branch
			row.Repo = targetRepo
			row.Branch = importBranch
			if err := rtx.SaveMemCommit(row); err != nil {
				return fmt.Errorf("memory: save imported commit %s: %w", c.CommitHash, err)
			}
			importedCommits++
			lastHead = c
		}
		if lastHead.CommitHash != "" {
			if err := rtx.UpsertMemBranch(store.MemBranchRow{
				Repo:           targetRepo,
				Branch:         importBranch,
				HeadCommit:     lastHead.CommitHash,
				GenesisPub:     cap.GenesisPub,
				GenesisShardID: cap.GenesisShardID,
				UpdatedAt:      now,
			}); err != nil {
				return fmt.Errorf("memory: save imported branch %s/%s: %w", targetRepo, importBranch, err)
			}
		}
		for _, a := range cap.Anchors {
			a.Shard = anchorTargetShard
			// Never trust untrusted JSON Settled=true or BlockHeight; strictly trust verified BUMP values from VerifyCapsule.
			a.Settled = ver.SettledByN[a.N]
			a.BlockHeight = ver.BlockHeightByN[a.N]
			if err := rtx.SaveBatchAnchorStrict(a); err != nil {
				return fmt.Errorf("memory: save imported capsule anchor n=%d: %w", a.N, err)
			}
		}
		for _, f := range cap.Fragments {
			f.Shard = targetRepo
			f.Tainted = true
			if isForeignImport {
				f.BatchN = -1
				f.LeafIndex = -1
				f.AnchorTxID = ""
			}
			_, dedup, err := rtx.AppendFragment(f)
			if err != nil {
				return fmt.Errorf("memory: append imported fragment seq=%d: %w", f.Seq, err)
			}
			if !dedup {
				importedFrags++
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	release()
	if importedFrags > 0 {
		s.notifyRemember(targetRepo)
	}

	res := &CapsuleImportResult{
		Repo:            targetRepo,
		Branch:          dstBranch,
		ImportedCommits: importedCommits,
		ImportedFrags:   importedFrags,
		Verification:    ver,
	}

	if mergeIntoMain && lastHead.CommitHash != "" {
		// Create a 2-parent Merge Commit linking targetBranch head + seller's SPV-verified commit & UTXO,
		// routed through commitFileSnapshotWithOpts so the merge commit also produces an anchorable fragment!
		srcTree, tErr := s.loadTreeObject(lastHead.TreeHash)
		if tErr != nil {
			return nil, fmt.Errorf("memory: load imported head tree %s: %w", lastHead.TreeHash, tErr)
		}
		mergedEntries := make(map[string]memrepo.TreeEntry)
		var parents []memrepo.ParentRef
		if curBr, bErr := s.db.GetMemBranch(targetRepo, dstBranch); bErr == nil && curBr.HeadCommit != "" {
			if curRow, rErr := s.db.GetMemCommit(curBr.HeadCommit); rErr == nil {
				curC := commitFromRow(curRow)
				if curTree, ctErr := s.loadTreeObject(curC.TreeHash); ctErr == nil && curTree != nil {
					for _, e := range curTree.Entries {
						mergedEntries[e.Path] = e
					}
				}
				parents = append(parents, memrepo.ParentRef{
					CommitHash: curC.CommitHash,
					AnchorTxID: curC.AnchorTxID,
				})
			}
		}
		if srcTree != nil {
			for _, e := range srcTree.Entries {
				mergedEntries[e.Path] = e
			}
		}
		sellerAnchorTx := lastHead.AnchorTxID
		if sellerAnchorTx == "" && len(cap.Anchors) > 0 {
			sellerAnchorTx = cap.Anchors[len(cap.Anchors)-1].TxID
		}
		parents = append(parents, memrepo.ParentRef{
			CommitHash:  lastHead.CommitHash,
			AnchorTxID:  sellerAnchorTx,
			RepoGenesis: cap.GenesisShardID,
		})
		mc, _, mErr := s.commitFileSnapshotWithOpts(
			ctx,
			targetRepo,
			dstBranch,
			nil,
			parents,
			fmt.Sprintf("Fork & merge verified capsule %s (%d commits)", cap.Repo, len(cap.Commits)),
			"capsule-merge",
			false,
			principal,
			&commitSnapshotOpts{PreservedEntries: mergedEntries},
		)
		if mErr != nil {
			return nil, fmt.Errorf("memory: commit capsule merge: %w", mErr)
		}
		res.MergeCommitHash = mc.CommitHash
	}
	return res, nil
}
