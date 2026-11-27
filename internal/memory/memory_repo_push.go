package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

// GitPushResult summarizes commits ingested via Smart HTTP git-receive-pack.
type GitPushResult struct {
	Repo          string            `json:"repo"`
	Branch        string            `json:"branch"`
	CommitCount   int               `json:"commitCount"`
	HeadCommit    string            `json:"headCommit"`
	AnchorTxID    string            `json:"anchorTxid,omitempty"`
	AnchorN       int64             `json:"anchorN"`
	CommitHashes  []string          `json:"commitHashes,omitempty"`
	CommitGitOIDs map[string]string `json:"commitGitOids,omitempty"` // pushed Git OID hex -> Nektar CommitHash
}

// commitSnapshotOpts supplies optional preserved tree entries & Git identity metadata to commitFileSnapshotWithOpts.
type commitSnapshotOpts struct {
	// PreservedEntries are pre-built TreeEntries (e.g. untouched encrypted or imported CHIRP blobs)
	// copied verbatim into the new tree without re-hashing or writing plaintext blobs.
	PreservedEntries map[string]memrepo.TreeEntry
	// ExistingEntries carries prior Level/Class metadata for plaintext files whose bytes are updated/kept.
	ExistingEntries map[string]memrepo.TreeEntry
	// GitMeta preserves client-pushed Git author/committer/message so BuildGoGitRepo reproduces exact OIDs.
	GitMeta *memrepo.GitCommitMeta
	// CallerHoldsLock indicates caller already holds s.lockShard(repo).
	CallerHoldsLock bool
	// ExpectedHead optionally enforces CAS on the target branch head inside RunRepoTx.
	ExpectedHead string
	// HasExpectedHead enables CAS check even when ExpectedHead == "" (new branch).
	HasExpectedHead bool
}

// ApplyPushedGitCommits ingests one or more fast-forward Git commits unpacked from a Smart HTTP
// `git-receive-pack` push, enforcing Cedar write policy, signing every commit with BRC-43,
// binding L1 TagLeafV2 MPE roots, and anchoring the pushed batch in ONE NektarAnchor.Append() spend.
func (s *Service) ApplyPushedGitCommits(
	ctx context.Context,
	repo, branch string,
	pushed []memrepo.PushedGitCommit,
	autoAnchor bool,
	principal *policy.Principal,
) (*GitPushResult, error) {
	if err := ValidateShardName(repo, false); err != nil {
		return nil, err
	}
	brName, err := memrepo.NormalizeBranch(branch)
	if err != nil {
		return nil, err
	}
	if len(pushed) == 0 {
		return &GitPushResult{Repo: repo, Branch: brName, AnchorN: -1}, nil
	}

	out := &GitPushResult{
		Repo:          repo,
		Branch:        brName,
		AnchorN:       -1,
		CommitGitOIDs: make(map[string]string, len(pushed)),
	}
	unlock := s.lockShard(repo)
	// Seed GitOID -> CommitHash lookup from existing persisted commits so merge parents resolve cleanly.
	knownByGitOID := make(map[string]memrepo.CommitObject)
	if existingRows, lErr := s.db.ListAllMemCommits(repo); lErr == nil {
		for _, r := range existingRows {
			ec := commitFromRow(r)
			if ec.GitMeta != nil && ec.GitMeta.GitOID != "" {
				knownByGitOID[ec.GitMeta.GitOID] = ec
			}
		}
	}
	for _, pc := range pushed {
		var parents []memrepo.ParentRef
		seenParent := make(map[string]bool)
		var expectedHead string
		if br, bErr := s.db.GetMemBranch(repo, brName); bErr == nil && br.HeadCommit != "" {
			expectedHead = br.HeadCommit
		}
		// Preserve all pushed Git parent links (including 2nd+ merge parents) when mapped to Nektar commits.
		for _, pOID := range pc.Parents {
			pHex := pOID.String()
			if nh, ok := out.CommitGitOIDs[pHex]; ok && nh != "" && !seenParent[nh] {
				seenParent[nh] = true
				parents = append(parents, memrepo.ParentRef{CommitHash: nh})
			} else if ec, ok := knownByGitOID[pHex]; ok && !seenParent[ec.CommitHash] {
				seenParent[ec.CommitHash] = true
				parents = append(parents, memrepo.ParentRef{CommitHash: ec.CommitHash, AnchorTxID: ec.AnchorTxID})
			}
		}
		if len(parents) == 0 && expectedHead != "" {
			if prevRow, rErr := s.db.GetMemCommit(expectedHead); rErr == nil {
				prevC := commitFromRow(prevRow)
				parents = append(parents, memrepo.ParentRef{CommitHash: prevC.CommitHash, AnchorTxID: prevC.AnchorTxID})
			}
		}
		var gitMeta *memrepo.GitCommitMeta
		if pc.AuthorName != "" || pc.RawMessage != "" {
			gitMeta = &memrepo.GitCommitMeta{
				AuthorName:         pc.AuthorName,
				AuthorEmail:        pc.AuthorEmail,
				AuthorWhen:         pc.AuthorWhen,
				AuthorOffsetSec:    pc.AuthorOffsetSec,
				CommitterName:      pc.CommitterName,
				CommitterEmail:     pc.CommitterEmail,
				CommitterWhen:      pc.CommitterWhen,
				CommitterOffsetSec: pc.CommitterOffsetSec,
				RawMessage:         pc.RawMessage,
				GitOID:             pc.GitOID.String(),
			}
		}
		c, _, err := s.commitFileSnapshotWithOpts(ctx, repo, brName, pc.Files, parents, pc.Message, "git-push", false, principal, &commitSnapshotOpts{
			GitMeta:         gitMeta,
			CallerHoldsLock: true,
			ExpectedHead:    expectedHead,
			HasExpectedHead: true,
		})
		if err != nil {
			unlock()
			return nil, err
		}
		out.CommitCount++
		out.HeadCommit = c.CommitHash
		out.CommitHashes = append(out.CommitHashes, c.CommitHash)
		if !pc.GitOID.IsZero() {
			out.CommitGitOIDs[pc.GitOID.String()] = c.CommitHash
		}
	}
	unlock()

	if autoAnchor && s.port != nil && s.port.BalanceSats() > 1000 {
		if batch, aErr := s.AnchorShard(ctx, repo); aErr == nil && batch != nil {
			out.AnchorTxID = batch.TxID
			out.AnchorN = batch.N
		} else if aErr != nil {
			return out, aErr
		}
	} else if out.CommitCount > 0 {
		s.notifyRemember(repo)
	}
	return out, nil
}

// commitFileSnapshot atomically persists a full tree snapshot, records a signed L1-MPE TagLeafV2
// fragment record for anchoring, signs the CommitObject, updates the branch head, and optionally anchors.
func (s *Service) commitFileSnapshot(
	ctx context.Context,
	repo, branch string,
	files map[string][]byte,
	parents []memrepo.ParentRef,
	message, class string,
	autoAnchor bool,
	principal *policy.Principal,
) (*memrepo.CommitObject, string, error) {
	return s.commitFileSnapshotWithOpts(ctx, repo, branch, files, parents, message, class, autoAnchor, principal, nil)
}

func (s *Service) commitFileSnapshotWithOpts(
	ctx context.Context,
	repo, branch string,
	files map[string][]byte,
	parents []memrepo.ParentRef,
	message, class string,
	autoAnchor bool,
	principal *policy.Principal,
	opts *commitSnapshotOpts,
) (*memrepo.CommitObject, string, error) {
	if err := s.authorizeRepoWrite(repo, repo+"#commit", class, principal); err != nil {
		return nil, "", err
	}

	now := time.Now().Unix()
	var preserved map[string]memrepo.TreeEntry
	var existing map[string]memrepo.TreeEntry
	var gitMeta *memrepo.GitCommitMeta
	if opts != nil {
		preserved = opts.PreservedEntries
		existing = opts.ExistingEntries
		gitMeta = opts.GitMeta
	}

	blobsToSave := make([]*memrepo.BlobObject, 0, len(files))
	entriesByPath := make(map[string]memrepo.TreeEntry, len(files)+len(preserved))

	for p, pe := range preserved {
		normP, err := memrepo.NormalizePath(p)
		if err != nil {
			return nil, "", fmt.Errorf("memory: invalid preserved path %q: %w", p, err)
		}
		pe.Path = normP
		entriesByPath[normP] = pe
	}

	for p, raw := range files {
		normP, err := memrepo.NormalizePath(p)
		if err != nil {
			return nil, "", fmt.Errorf("memory: invalid file path %q: %w", p, err)
		}
		if _, isPreserved := entriesByPath[normP]; isPreserved {
			continue
		}
		b, _, err := memrepo.BuildBlob(s.objects, normP, "", raw, len(raw) > memrepo.InlineBlobMaxBytes)
		if err != nil {
			return nil, "", err
		}
		if prev, ok := existing[normP]; ok {
			b.Level = prev.Level
			b.Class = prev.Class
		}
		blobsToSave = append(blobsToSave, b)
		entriesByPath[normP] = memrepo.TreeEntry{
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
	tr, err := memrepo.BuildTree(entries)
	if err != nil {
		return nil, "", err
	}
	treeRaw, err := json.Marshal(tr)
	if err != nil {
		return nil, "", err
	}

	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = fmt.Sprintf("Update tree (%d files)", len(entries))
	}

	// Record a canonical L1-MPE TagLeafV2 fragment representing this tree transition so AnchorShard
	// commits this exact treeHash & message into the NektarAnchor singleton UTXO chain!
	payload := []byte(fmt.Sprintf("%s\ntree:%s\nfiles:%d", msg, tr.TreeHash, len(entries)))
	prov := core.Provenance{
		Source:  "git:" + class,
		Time:    now,
		ModelID: s.modelID.Provider + "/" + s.modelID.Release,
	}
	sigBytes, err := s.port.SignDigest(ctx, core.ProvenanceDigest(payload, prov))
	if err != nil {
		return nil, "", fmt.Errorf("memory: sign commit fragment: %w", err)
	}
	prov.Signature = sigBytes
	provBytes := core.SerializeProvenance(prov)
	hdp := core.ComputeHDPV2(payload, prov)
	hdpHex := core.ToHex(hdp)
	sum := sha256.Sum256(payload)
	mpe, mErr := buildCommittedFragmentMPE(repo, hdpHex, prov, 0, class, "", "text/plain", "", hex.EncodeToString(sum[:]))
	if mErr != nil || mpe == nil {
		return nil, "", fmt.Errorf("memory: build commit fragment MPE: %w", mErr)
	}
	leaf := core.ComputeLeafV2(core.Fragment{
		Data:       payload,
		Provenance: prov,
		HDP:        hdp,
		MPERoot:    mpe.Root,
	})

	anchors, _ := s.db.ListAnchors(repo)
	genPub, genID, _ := s.resolveRepoGenesis(ctx, repo, anchors)

	callerHoldsLock := opts != nil && opts.CallerHoldsLock
	var unlockShard func()
	if !callerHoldsLock {
		unlockShard = s.lockShard(repo)
	}
	var c memrepo.CommitObject
	txErr := s.db.RunRepoTx(func(rtx *store.RepoTx) error {
		for _, b := range blobsToSave {
			bRaw, mErr := json.Marshal(b)
			if mErr != nil {
				return mErr
			}
			if err := rtx.PutMemObject(b.BlobHash, "blob", bRaw, now); err != nil {
				return err
			}
		}
		if err := rtx.PutMemObject(tr.TreeHash, "tree", treeRaw, now); err != nil {
			return err
		}
		var expectedHead string
		if opts != nil && opts.HasExpectedHead {
			expectedHead = opts.ExpectedHead
		} else if curBr, bErr := rtx.GetMemBranch(repo, branch); bErr == nil {
			expectedHead = curBr.HeadCommit
		}
		stored, _, err := rtx.AppendFragment(store.FragmentRecord{
			Shard:      repo,
			HDP:        hdpHex,
			Leaf:       core.ToHex(leaf),
			MPERoot:    mpe.RootHex,
			Data:       payload,
			Provenance: provBytes,
			Level:      0,
			Class:      class,
			BatchN:     -1,
			LeafIndex:  -1,
			CreatedAt:  now,
		})
		if err != nil {
			return err
		}
		finalized, err := memrepo.FinalizeCommit(memrepo.CommitObject{
			Version:      1,
			Repo:         repo,
			Branch:       branch,
			Seq:          stored.Seq,
			Parents:      parents,
			TreeHash:     tr.TreeHash,
			AuthorPub:    s.commitAuthorPubHex(ctx),
			ModelID:      s.modelID.Provider + "/" + s.modelID.Release,
			PolicyCommit: s.PolicyCommit().Hash,
			Message:      msg,
			Timestamp:    now,
			AnchorN:      -1,
			FragmentLeaf: stored.Leaf,
			GitMeta:      gitMeta,
		}, func(digest []byte) ([]byte, error) {
			return s.port.SignDigest(ctx, digest)
		})
		if err != nil {
			return err
		}
		if err := rtx.SaveMemCommit(rowFromCommit(finalized)); err != nil {
			return err
		}
		if err := rtx.CompareAndSwapMemBranch(store.MemBranchRow{
			Repo:           finalized.Repo,
			Branch:         finalized.Branch,
			HeadCommit:     finalized.CommitHash,
			GenesisPub:     genPub,
			GenesisShardID: genID,
			UpdatedAt:      finalized.Timestamp,
		}, expectedHead); err != nil {
			return err
		}
		if err := rtx.UpdateFragmentCommitHash(repo, stored.Seq, finalized.CommitHash); err != nil {
			return err
		}
		c = finalized
		return nil
	})
	if unlockShard != nil {
		unlockShard()
	}
	if txErr != nil {
		return nil, "", txErr
	}

	var anchorTxID string
	if autoAnchor {
		batch, aErr := s.AnchorShard(ctx, repo)
		if aErr != nil {
			return &c, "", aErr
		}
		if batch != nil {
			anchorTxID = batch.TxID
			c.AnchorTxID = batch.TxID
			c.AnchorN = batch.N
		}
	} else {
		s.notifyRemember(repo)
	}
	return &c, anchorTxID, nil
}
