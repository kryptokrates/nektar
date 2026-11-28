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

// commitSnapshotOpts supplies optional preserved tree entries & Git identity metadata to commitFileSnapshotWithOpts.
type commitSnapshotOpts struct {
	// PreservedEntries are pre-built TreeEntries (e.g. untouched encrypted or imported CHIRP blobs)
	// copied verbatim into the new tree without re-hashing or writing plaintext blobs.
	PreservedEntries map[string]memrepo.TreeEntry
	// ExistingEntries carries prior Level/Class/Encrypted metadata for files whose bytes are updated/kept.
	ExistingEntries map[string]memrepo.TreeEntry
	// ExistingByContent maps "path\x00contentSha256" -> exact persisted TreeEntry so unchanged CLKA blobs keep their BlobHash.
	ExistingByContent map[string]memrepo.TreeEntry
	// GitMeta preserves client Git author/committer/message metadata when present.
	GitMeta *memrepo.GitCommitMeta
	// CallerHoldsLock indicates caller already holds s.lockShard(repo).
	CallerHoldsLock bool
	// ExpectedHead optionally enforces CAS on the target branch head inside RunRepoTx.
	ExpectedHead string
	// HasExpectedHead enables CAS check even when ExpectedHead == "" (new branch).
	HasExpectedHead bool
	// GenesisPub / GenesisShardID optionally supply pre-resolved repo genesis metadata when inside an active RepoTx.
	GenesisPub     string
	GenesisShardID string
	// SkipSyntheticFragment skips inserting a synthetic fragment row when the caller already imported real fragments
	// in the same RepoTx (creating only the signed 2-parent merge CommitObject + TransitionRecord + branch HEAD).
	SkipSyntheticFragment bool
}

type preparedFileSnapshot struct {
	now          int64
	blobsToSave  []*memrepo.BlobObject
	bucketTrees  map[string]memrepo.TreeObject
	rootTree     *memrepo.TreeObject
	treeRaw      []byte
	msg          string
	class        string
	payload      []byte
	prov         core.Provenance
	provBytes    []byte
	hdp          []byte
	hdpHex       string
	mpe          *core.MPE
	authorPubHex string
	policyHash   string
	ontologyHash string
	gitMeta      *memrepo.GitCommitMeta
}

func (s *Service) prepareFileSnapshot(
	ctx context.Context,
	repo string,
	files map[string][]byte,
	message, class string,
	opts *commitSnapshotOpts,
) (*preparedFileSnapshot, error) {
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
			return nil, fmt.Errorf("memory: invalid preserved path %q: %w", p, err)
		}
		pe.Path = normP
		entriesByPath[normP] = pe
	}

	for p, raw := range files {
		normP, err := memrepo.NormalizePath(p)
		if err != nil {
			return nil, fmt.Errorf("memory: invalid file path %q: %w", p, err)
		}
		if _, isPreserved := entriesByPath[normP]; isPreserved {
			continue
		}
		prev, hasPrev := existing[normP]
		var b *memrepo.BlobObject
		if hasPrev && prev.Encrypted && s.lattice != nil {
			sum := sha256.Sum256(raw)
			sumHex := hex.EncodeToString(sum[:])
			if opts != nil && len(opts.ExistingByContent) > 0 {
				if exact, ok := opts.ExistingByContent[normP+"\x00"+sumHex]; ok && exact.BlobHash != "" {
					entriesByPath[normP] = exact
					continue
				}
			}
			if prev.ContentSHA256 != "" && strings.EqualFold(prev.ContentSHA256, sumHex) && prev.BlobHash != "" {
				entriesByPath[normP] = prev
				continue
			}
			cls := prev.Class
			if cls == "" {
				cls = "general"
			}
			env, ck, sErr := s.lattice.SealFragment(ctx, repo, prev.Level, cls, sum, raw)
			core.Zero32Array(&ck)
			if sErr != nil {
				return nil, fmt.Errorf("memory: re-seal merged file %q: %w", normP, sErr)
			}
			km, _ := json.Marshal(env)
			b, err = memrepo.BuildEncryptedBlobWithMeta(normP, "", int64(len(raw)), hex.EncodeToString(sum[:]), "", "", 0, env.Epoch, km, prev.Level, cls)
			if err != nil {
				return nil, err
			}
		} else {
			lvl := 0
			cls := ""
			if hasPrev {
				lvl = prev.Level
				cls = prev.Class
			}
			b, _, err = memrepo.BuildBlobWithMeta(s.objects, normP, "", raw, len(raw) > memrepo.InlineBlobMaxBytes, lvl, cls)
			if err != nil {
				return nil, err
			}
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
	tr, bucketTrees, err := memrepo.BuildShardedTree(entries)
	if err != nil {
		return nil, err
	}
	treeRaw, err := memrepo.EncodeTreeObjectCBOR(*tr)
	if err != nil {
		return nil, err
	}

	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = fmt.Sprintf("Update tree (%d files)", len(entries))
	}

	payload := []byte(fmt.Sprintf("%s\ntree:%s\nfiles:%d", msg, tr.TreeHash, len(entries)))
	prov := core.Provenance{
		Source:  "git:" + class,
		Time:    now,
		ModelID: s.modelID.Provider + "/" + s.modelID.Release,
	}
	sigBytes, err := s.port.SignDigest(ctx, core.ProvenanceDigest(payload, prov))
	if err != nil {
		return nil, fmt.Errorf("memory: sign commit fragment: %w", err)
	}
	prov.Signature = sigBytes
	provBytes := core.SerializeProvenance(prov)
	hdp := core.ComputeHDPV2(payload, prov)
	hdpHex := core.ToHex(hdp)
	sum := sha256.Sum256(payload)
	mpe, mErr := buildCommittedFragmentMPE(repo, hdpHex, prov, 0, class, "", "text/plain", "", hex.EncodeToString(sum[:]))
	if mErr != nil || mpe == nil {
		return nil, fmt.Errorf("memory: build commit fragment MPE: %w", mErr)
	}

	policyHashHex := ""
	if pc := s.PolicyCommit(); pc != nil {
		policyHashHex = pc.Hash
	}
	return &preparedFileSnapshot{
		now:          now,
		blobsToSave:  blobsToSave,
		bucketTrees:  bucketTrees,
		rootTree:     tr,
		treeRaw:      treeRaw,
		msg:          msg,
		class:        class,
		payload:      payload,
		prov:         prov,
		provBytes:    provBytes,
		hdp:          hdp,
		hdpHex:       hdpHex,
		mpe:          mpe,
		authorPubHex: s.commitAuthorPubHex(ctx),
		policyHash:   policyHashHex,
		ontologyHash: s.OntologyHead(),
		gitMeta:      gitMeta,
	}, nil
}

// commitFileSnapshotTx persists a prepared tree snapshot, CommitObject, Branch CAS, TransitionRecord,
// and L1-MPE FragmentRecord inside an already-open RepoTx (enabling atomic multi-step import+merge).
func (s *Service) commitFileSnapshotTx(
	ctx context.Context,
	rtx *store.RepoTx,
	repo, branch string,
	parents []memrepo.ParentRef,
	prep *preparedFileSnapshot,
	genPub, genID string,
	opts *commitSnapshotOpts,
) (memrepo.CommitObject, error) {
	for _, b := range prep.blobsToSave {
		bRaw, mErr := memrepo.EncodeBlobObjectCBOR(*b)
		if mErr != nil {
			return memrepo.CommitObject{}, mErr
		}
		if err := rtx.PutMemObject(b.BlobHash, "blob", bRaw, prep.now); err != nil {
			return memrepo.CommitObject{}, err
		}
	}
	for _, bt := range prep.bucketTrees {
		btRaw, mErr := memrepo.EncodeTreeObjectCBOR(bt)
		if mErr != nil {
			return memrepo.CommitObject{}, mErr
		}
		if err := rtx.PutMemObject(bt.TreeHash, "tree", btRaw, prep.now); err != nil {
			return memrepo.CommitObject{}, err
		}
	}
	if err := rtx.PutMemObject(prep.rootTree.TreeHash, "tree", prep.treeRaw, prep.now); err != nil {
		return memrepo.CommitObject{}, err
	}

	var expectedHead string
	if opts != nil && opts.HasExpectedHead {
		expectedHead = opts.ExpectedHead
	} else if curBr, bErr := rtx.GetMemBranch(repo, branch); bErr == nil {
		expectedHead = curBr.HeadCommit
	}
	skipSynthetic := opts != nil && opts.SkipSyntheticFragment
	var commitSeq int64
	if !skipSynthetic {
		var sErr error
		commitSeq, sErr = rtx.NextFragmentSeq(repo)
		if sErr != nil {
			return memrepo.CommitObject{}, sErr
		}
	}
	nextTrSeq, trSeqErr := rtx.NextTransitionSeq(repo)
	if trSeqErr != nil {
		return memrepo.CommitObject{}, trSeqErr
	}
	prevQHex, qErr := rtx.LatestStateQ(repo)
	if qErr != nil {
		return memrepo.CommitObject{}, qErr
	}
	prevQBytes := core.Zero32Bytes()
	if prevQHex != "" {
		if b, dErr := hex.DecodeString(prevQHex); dErr == nil && len(b) == 32 {
			prevQBytes = b
		}
	}

	finalized, err := memrepo.FinalizeCommit(memrepo.CommitObject{
		Version:        1,
		Repo:           repo,
		Branch:         branch,
		Seq:            commitSeq,
		Parents:        parents,
		TreeHash:       prep.rootTree.TreeHash,
		AuthorPub:      prep.authorPubHex,
		ModelID:        s.modelID.Provider + "/" + s.modelID.Release,
		PolicyCommit:   prep.policyHash,
		OntologyCommit: prep.ontologyHash,
		Message:        prep.msg,
		Timestamp:      prep.now,
		AnchorN:        -1,
		GitMeta:        prep.gitMeta,
	}, func(digest []byte) ([]byte, error) {
		return s.port.SignDigest(ctx, digest)
	})
	if err != nil {
		return memrepo.CommitObject{}, err
	}

	commitHashBytes, _ := hex.DecodeString(finalized.CommitHash)
	treeHashBytes, _ := hex.DecodeString(prep.rootTree.TreeHash)
	var polBytes, ontBytes []byte
	if prep.policyHash != "" {
		polBytes, _ = hex.DecodeString(prep.policyHash)
	}
	if prep.ontologyHash != "" {
		ontBytes, _ = hex.DecodeString(prep.ontologyHash)
	}
	nextQBytes, qnErr := core.NextStepStateQ(prevQBytes, prep.hdp, commitHashBytes, treeHashBytes, polBytes, ontBytes, prep.now)
	if qnErr != nil {
		return memrepo.CommitObject{}, fmt.Errorf("memory: compute commit stateQ: %w", qnErr)
	}
	trRec := core.TransitionRecord{
		Version:      1,
		Shard:        repo,
		Seq:          nextTrSeq,
		PrevStateQ:   prevQBytes,
		InputDigest:  prep.hdp,
		NextStateQ:   nextQBytes,
		CommitHash:   commitHashBytes,
		TreeHash:     treeHashBytes,
		PolicyHash:   polBytes,
		OntologyHash: ontBytes,
		Timestamp:    prep.now,
	}
	trHashBytes, trErr := core.ComputeTransitionHash(trRec)
	if trErr != nil {
		return memrepo.CommitObject{}, fmt.Errorf("memory: compute commit transition hash: %w", trErr)
	}
	trRec.Hash = trHashBytes
	trCBOR, trErr := core.EncodeTransitionCBOR(trRec)
	if trErr != nil {
		return memrepo.CommitObject{}, fmt.Errorf("memory: encode commit transition CBOR: %w", trErr)
	}
	transitionHashHex := core.ToHex(trHashBytes)

	leaf := core.ComputeLeaf(core.Fragment{
		Data:           prep.payload,
		Provenance:     prep.prov,
		HDP:            prep.hdp,
		MPERoot:        prep.mpe.Root,
		CommitHash:     commitHashBytes,
		TransitionHash: trHashBytes,
	})
	leafHex := core.ToHex(leaf)
	finalized.FragmentLeaf = leafHex

	if err := rtx.SaveMemCommit(rowFromCommit(finalized)); err != nil {
		return memrepo.CommitObject{}, err
	}
	if err := rtx.CompareAndSwapMemBranch(store.MemBranchRow{
		Repo:           finalized.Repo,
		Branch:         finalized.Branch,
		HeadCommit:     finalized.CommitHash,
		GenesisPub:     genPub,
		GenesisShardID: genID,
		UpdatedAt:      finalized.Timestamp,
	}, expectedHead); err != nil {
		return memrepo.CommitObject{}, err
	}
	if err := rtx.SaveTransition(store.TransitionRow{
		Shard:        repo,
		Seq:          nextTrSeq,
		Hash:         transitionHashHex,
		PrevQ:        core.ToHex(prevQBytes),
		NextQ:        core.ToHex(nextQBytes),
		CommitHash:   finalized.CommitHash,
		TreeHash:     prep.rootTree.TreeHash,
		PolicyHash:   prep.policyHash,
		OntologyHash: prep.ontologyHash,
		BodyCBOR:     trCBOR,
		CreatedAt:    prep.now,
	}); err != nil {
		return memrepo.CommitObject{}, err
	}
	if !skipSynthetic {
		if _, _, err := rtx.AppendFragment(store.FragmentRecord{
			Shard:          repo,
			HDP:            prep.hdpHex,
			Leaf:           leafHex,
			MPERoot:        prep.mpe.RootHex,
			CommitHash:     finalized.CommitHash,
			TransitionHash: transitionHashHex,
			Data:           prep.payload,
			Provenance:     prep.provBytes,
			Level:          0,
			Class:          prep.class,
			Public:         true,
			BatchN:         -1,
			LeafIndex:      -1,
			CreatedAt:      prep.now,
		}); err != nil {
			return memrepo.CommitObject{}, err
		}
	}
	return finalized, nil
}

// commitFileSnapshot atomically persists a full tree snapshot, records a signed L1-MPE canonical leaf
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
	prep, err := s.prepareFileSnapshot(ctx, repo, files, message, class, opts)
	if err != nil {
		return nil, "", err
	}

	var genPub, genID string
	if opts != nil && opts.GenesisPub != "" && opts.GenesisShardID != "" {
		genPub, genID = opts.GenesisPub, opts.GenesisShardID
	} else if br, bErr := s.db.GetMemBranch(repo, "main"); bErr == nil && br.GenesisPub != "" && br.GenesisShardID != "" {
		genPub, genID = br.GenesisPub, br.GenesisShardID
	} else {
		anchors, _ := s.db.ListAnchors(repo)
		genPub, genID, _ = s.resolveRepoGenesis(ctx, repo, anchors)
	}

	callerHoldsLock := opts != nil && opts.CallerHoldsLock
	var unlockShard func()
	if !callerHoldsLock {
		unlockShard = s.lockShard(repo)
	}
	var c memrepo.CommitObject
	txErr := s.db.RunRepoTx(func(rtx *store.RepoTx) error {
		var cErr error
		c, cErr = s.commitFileSnapshotTx(ctx, rtx, repo, branch, parents, prep, genPub, genID, opts)
		return cErr
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
