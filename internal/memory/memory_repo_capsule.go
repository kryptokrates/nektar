package memory

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

// CapsuleExportOpts controls streaming .nkt Memory Capsule generation.
type CapsuleExportOpts struct {
	SelfContained   bool                                                                // default true: inline volumetric CHIRP blobs as <=1 MiB chunks
	WrapDEK         func(dek []byte) (wrappedDEK []byte, recipientPub string, err error) // optional mode 0x01 outer AEAD key wrapper
	RecipientPubHex string                                                              // optional recipient pubkey recorded in outer AEAD envelope header
	SignExport      bool                                                                // default true: sign CapsuleTrailer digest with node signing key
	Principal       *policy.Principal                                                   // optional Cedar principal (defaults to owner)
	DisallowBorg    bool                                                                // when true, redact Borg-encrypted blobs as stubs
	StoreInChirp    bool                                                                // when true (default for ExportCapsule), also store bytes in CHIRP
}

// CapsuleExportResult summarizes an exported self-verifying .nkt Memory Capsule.
type CapsuleExportResult struct {
	Repo           string `json:"repo"`
	Branch         string `json:"branch"`
	BaseCommit     string `json:"baseCommit,omitempty"`
	HeadCommit     string `json:"headCommit,omitempty"`
	GenesisShardID string `json:"genesisShardId,omitempty"`
	ChirpURL       string `json:"chirpUrl,omitempty"`
	RootHex        string `json:"rootHex,omitempty"`
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
	ImportedSeqs    []int64                      `json:"importedSeqs,omitempty"`
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

// buildMemoryCapsule assembles a self-verifying MemoryCapsule (full or incremental sinceCommit..head delta)
// preserving exact TreeHashes, emitting hash-preserving BlobStubs for redacted/forgotten blobs,
// embedding CBOR Merkle Automaton TransitionRecords, and constructing MMR consistency proofs for deltas.
func (s *Service) buildMemoryCapsule(
	ctx context.Context,
	repo, branch, sinceCommit string,
	principal policy.Principal,
	disallowBorg bool,
) (*memrepo.MemoryCapsule, error) {
	if err := ValidateShardName(repo, false); err != nil {
		return nil, err
	}
	brName, err := memrepo.NormalizeBranch(branch)
	if err != nil {
		return nil, err
	}
	if principal.ID == "" {
		principal = ownerPrincipal()
	}

	var headCommitHash string
	if br, bErr := s.db.GetMemBranch(repo, brName); bErr == nil && br.HeadCommit != "" {
		headCommitHash = br.HeadCommit
	}
	chrono := s.collectReachableCommitsTopo(headCommitHash, sinceCommit)
	if len(chrono) == 0 && sinceCommit == "" {
		rows, err := s.db.ListAllMemCommits(repo)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if r.Branch == brName {
				chrono = append(chrono, commitFromRow(r))
			}
		}
	}

	trees, blobs, _ := s.collectCapsuleDAGObjects(repo, chrono, principal, disallowBorg, false)
	allAnchors, _ := s.db.ListAnchors(repo)
	frags, _ := s.db.ListFragments(repo)
	s.mu.RLock()
	polText := s.policyText
	s.mu.RUnlock()

	commitSeqSet := make(map[int64]bool, len(chrono))
	commitHashSet := make(map[string]bool, len(chrono))
	for _, c := range chrono {
		commitHashSet[c.CommitHash] = true
		if c.Seq > 0 {
			commitSeqSet[c.Seq] = true
		}
	}

	// Handle incremental delta anchor slicing + MMRConsistencyProof when sinceCommit is specified and anchored.
	anchors := allAnchors
	baseAnchorN := int64(-1)
	var baseAnchorTxID string
	var baseStateQ, baseMmrRoot []byte
	var anchorConsistency *core.MMRConsistencyProof
	if sinceCommit != "" && len(allAnchors) > 0 {
		if baseRow, bErr := s.db.GetMemCommit(sinceCommit); bErr == nil && baseRow.AnchorN >= 0 {
			var baseAnc *store.BatchAnchor
			deltaAnchors := make([]store.BatchAnchor, 0)
			allRoots := make([][]byte, 0, len(allAnchors))
			for i := range allAnchors {
				rBytes, _ := hex.DecodeString(allAnchors[i].RootHex)
				allRoots = append(allRoots, rBytes)
				if allAnchors[i].N == baseRow.AnchorN {
					cp := allAnchors[i]
					baseAnc = &cp
				} else if allAnchors[i].N > baseRow.AnchorN {
					deltaAnchors = append(deltaAnchors, allAnchors[i])
				}
			}
			if baseAnc != nil {
				baseAnchorN = baseAnc.N
				baseAnchorTxID = baseAnc.TxID
				if baseAnc.MmrRoot != "" {
					baseMmrRoot, _ = hex.DecodeString(baseAnc.MmrRoot)
				}
				anchors = deltaAnchors
				if len(deltaAnchors) > 0 {
					mmr, mErr := core.NewMMR(allRoots)
					if mErr == nil {
						if cp, pErr := mmr.ProveConsistency(uint64(baseAnc.N+1), uint64(deltaAnchors[0].N+1)); pErr == nil && cp != nil {
							anchorConsistency = cp
						}
					}
				}
			}
		}
	}

	activeFrags := make([]store.FragmentRecord, 0, len(frags))
	for _, f := range frags {
		if f.Forgotten {
			continue
		}
		if sinceCommit != "" && !commitSeqSet[f.Seq] && !commitHashSet[f.CommitHash] {
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

	// Collect CBOR Merkle Automaton TransitionRecords for verifiable q_base -> q_head replay.
	var transitions []core.TransitionRecord
	if trRows, tErr := s.db.ListAllTransitions(repo); tErr == nil && len(trRows) > 0 {
		for _, trRow := range trRows {
			if sinceCommit != "" && !commitHashSet[trRow.CommitHash] {
				continue
			}
			if dec, dErr := core.DecodeTransitionCBOR(trRow.BodyCBOR); dErr == nil {
				transitions = append(transitions, dec)
			}
		}
		if sinceCommit != "" && len(baseStateQ) == 0 && len(transitions) > 0 && len(transitions[0].PrevStateQ) == 32 {
			baseStateQ = append([]byte(nil), transitions[0].PrevStateQ...)
		}
	}

	genPub, genID, _ := s.resolveRepoGenesis(ctx, repo, allAnchors)
	headHash := ""
	if len(chrono) > 0 {
		headHash = chrono[len(chrono)-1].CommitHash
	}
	return &memrepo.MemoryCapsule{
		Format:            memrepo.CapsuleFormatVersion,
		Repo:              repo,
		Branch:            brName,
		GenesisPub:        genPub,
		GenesisShardID:    genID,
		OwnerPub:          s.port.IdentityPubHex(),
		BaseCommit:        sinceCommit,
		HeadCommit:        headHash,
		BaseAnchorN:       baseAnchorN,
		BaseAnchorTxID:    baseAnchorTxID,
		BaseStateQ:        baseStateQ,
		BaseMmrRoot:       baseMmrRoot,
		ExportedAt:        time.Now().Unix(),
		Commits:           chrono,
		Trees:             trees,
		Blobs:             blobs,
		Transitions:       transitions,
		Anchors:           anchors,
		AnchorConsistency: anchorConsistency,
		Fragments:         activeFrags,
		BatchProofs:       s.buildCapsuleBatchProofs(repo, anchors, activeFrags, chrono),
	}, nil
}

func (s *Service) capsuleEncodeOpts(ctx context.Context, opts *CapsuleExportOpts) memrepo.CapsuleEncodeOpts {
	selfContained := true
	signExport := true
	var wrapDEK func(dek []byte) ([]byte, string, error)
	var recipPub string
	if opts != nil {
		selfContained = opts.SelfContained
		signExport = opts.SignExport
		wrapDEK = opts.WrapDEK
		recipPub = opts.RecipientPubHex
	}
	enc := memrepo.CapsuleEncodeOpts{
		SelfContained: selfContained,
		Objects:       s.objects,
		WrapDEK:       wrapDEK,
		RecipientPub:  recipPub,
	}
	if signExport && s.port != nil {
		enc.SignerPub = s.commitAuthorPubHex(ctx)
		enc.SignDigest = func(digest []byte) ([]byte, error) {
			return s.port.SignDigest(ctx, digest)
		}
	}
	return enc
}

// ExportCapsuleStream streams a self-verifying RFC 8742 CBOR Sequence + zstd .nkt Memory Capsule directly to w.
func (s *Service) ExportCapsuleStream(ctx context.Context, w io.Writer, repo, branch, sinceCommit string, opts *CapsuleExportOpts) (*CapsuleExportResult, error) {
	principal := ownerPrincipal()
	disallowBorg := false
	if opts != nil {
		if opts.Principal != nil && opts.Principal.ID != "" {
			principal = *opts.Principal
		}
		disallowBorg = opts.DisallowBorg
	}
	capObj, err := s.buildMemoryCapsule(ctx, repo, branch, sinceCommit, principal, disallowBorg)
	if err != nil {
		return nil, err
	}
	encOpts := s.capsuleEncodeOpts(ctx, opts)
	cw := &countingWriter{w: w}
	if err := memrepo.WriteCapsuleStream(cw, capObj, encOpts); err != nil {
		return nil, err
	}
	return &CapsuleExportResult{
		Repo:           capObj.Repo,
		Branch:         capObj.Branch,
		BaseCommit:     capObj.BaseCommit,
		HeadCommit:     capObj.HeadCommit,
		GenesisShardID: capObj.GenesisShardID,
		CommitCount:    len(capObj.Commits),
		BlobCount:      len(capObj.Blobs),
		AnchorCount:    len(capObj.Anchors),
		SizeBytes:      cw.n,
	}, nil
}

// ExportFragmentCapsuleStream builds and streams a compact, self-verifying .nkt sub-capsule for a single
// (shard, seq) — containing its CommitObject, TreeObject, BlobObject, TransitionRecord, and NektarAnchor
// lineage + SubsetDisclosureProof — for universal delivery across live BSV knowledge deals and P2P transfers.
func (s *Service) ExportFragmentCapsuleStream(ctx context.Context, w io.Writer, shard string, seq int64, opts *CapsuleExportOpts) (*CapsuleExportResult, error) {
	if err := ValidateShardName(shard, false); err != nil {
		return nil, err
	}
	frag, err := s.db.GetFragment(shard, seq)
	if err != nil {
		return nil, fmt.Errorf("memory: get fragment %s#%d: %w", shard, seq, err)
	}
	if frag.Forgotten {
		return nil, fmt.Errorf("memory: fragment %s#%d is forgotten", shard, seq)
	}
	if frag.BorgGroup != "" {
		return nil, fmt.Errorf("memory: cannot export borg-encrypted fragment %s#%d as plaintext market capsule", shard, seq)
	}

	// Resolve plaintext bytes (opening CLKA ephemerally for authorized owner export if needed) so the
	// buyer receives a self-contained verifiable fragment inside the BRC-369 sealed .nkt capsule.
	principal := ownerPrincipal()
	if opts != nil && opts.Principal != nil && opts.Principal.ID != "" {
		principal = *opts.Principal
	}
	if frag.LatticeEpoch > 0 && len(frag.Data) == 0 {
		pt, ok := s.openFragmentPlaintext(ctx, frag, principal, s.port.IdentityPubHex(), nil, true, false)
		if !ok {
			return nil, fmt.Errorf("memory: failed to open fragment %s#%d for capsule export", shard, seq)
		}
		frag.Data = pt
		frag.KeyedMeta = nil
		frag.LatticeEpoch = 0
		frag.Public = true
	}

	relPath := frag.Path
	if relPath == "" {
		relPath = fmt.Sprintf("facts/%04d.md", frag.Seq)
	}
	blob, err := s.buildFragmentBlob(relPath, frag.MediaType, frag.Data, nil, frag.ChirpURL, frag.ContentSHA256, "", 0, 0, nil, frag.Level, frag.Class)
	if err != nil {
		return nil, fmt.Errorf("memory: build fragment capsule blob: %w", err)
	}
	tr, err := memrepo.BuildTree([]memrepo.TreeEntry{{
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
	}})
	if err != nil {
		return nil, fmt.Errorf("memory: build fragment capsule tree: %w", err)
	}

	var parents []memrepo.ParentRef
	if frag.CommitHash != "" {
		if origRow, rErr := s.db.GetMemCommit(frag.CommitHash); rErr == nil {
			parents = append(parents, memrepo.ParentRef{
				CommitHash: origRow.CommitHash,
				AnchorTxID: origRow.AnchorTxID,
			})
		}
	}
	c, err := memrepo.FinalizeCommit(memrepo.CommitObject{
		Version:        1,
		Repo:           shard,
		Branch:         memrepo.DefaultBranch,
		Seq:            frag.Seq,
		Parents:        parents,
		TreeHash:       tr.TreeHash,
		MPERoot:        frag.MPERoot,
		AuthorPub:      s.commitAuthorPubHex(ctx),
		ModelID:        s.modelID.Provider + "/" + s.modelID.Release,
		PolicyCommit:   s.PolicyCommit().Hash,
		OntologyCommit: s.OntologyHead(),
		Message:        fmt.Sprintf("Disclosed fragment %s#%d", shard, frag.Seq),
		Timestamp:      frag.CreatedAt,
		AnchorTxID:     frag.AnchorTxID,
		AnchorN:        frag.BatchN,
		FragmentLeaf:   frag.Leaf,
	}, func(digest []byte) ([]byte, error) {
		return s.port.SignDigest(ctx, digest)
	})
	if err != nil {
		return nil, fmt.Errorf("memory: sign fragment capsule commit: %w", err)
	}

	var anchors []store.BatchAnchor
	allAnchors, _ := s.db.ListAnchors(shard)
	if frag.BatchN >= 0 {
		for _, a := range allAnchors {
			if a.N <= frag.BatchN {
				anchors = append(anchors, a)
			}
		}
	}
	genPub, genID, _ := s.resolveRepoGenesis(ctx, shard, allAnchors)

	var transitions []core.TransitionRecord
	if frag.TransitionHash != "" {
		if trRows, tErr := s.db.ListAllTransitions(shard); tErr == nil {
			for _, trRow := range trRows {
				if strings.EqualFold(trRow.Hash, frag.TransitionHash) {
					if dec, dErr := core.DecodeTransitionCBOR(trRow.BodyCBOR); dErr == nil {
						transitions = append(transitions, dec)
					}
					break
				}
			}
		}
	}
	var baseStateQ []byte
	if len(transitions) > 0 && len(transitions[0].PrevStateQ) == 32 {
		baseStateQ = append([]byte(nil), transitions[0].PrevStateQ...)
	}

	frags := []store.FragmentRecord{frag}
	commits := []memrepo.CommitObject{c}
	capObj := &memrepo.MemoryCapsule{
		Format:         memrepo.CapsuleFormatVersion,
		Repo:           shard,
		Branch:         memrepo.DefaultBranch,
		GenesisPub:     genPub,
		GenesisShardID: genID,
		OwnerPub:       s.port.IdentityPubHex(),
		BaseCommit:     frag.CommitHash,
		HeadCommit:     c.CommitHash,
		BaseAnchorN:    -1,
		BaseStateQ:     baseStateQ,
		ExportedAt:     time.Now().Unix(),
		Commits:        commits,
		Trees:          map[string]memrepo.TreeObject{tr.TreeHash: *tr},
		Blobs:          map[string]memrepo.BlobObject{blob.BlobHash: *blob},
		Transitions:    transitions,
		Anchors:        anchors,
		Fragments:      frags,
		BatchProofs:    s.buildCapsuleBatchProofs(shard, anchors, frags, commits),
	}
	encOpts := s.capsuleEncodeOpts(ctx, opts)
	cw := &countingWriter{w: w}
	if err := memrepo.WriteCapsuleStream(cw, capObj, encOpts); err != nil {
		return nil, err
	}
	return &CapsuleExportResult{
		Repo:           capObj.Repo,
		Branch:         capObj.Branch,
		BaseCommit:     capObj.BaseCommit,
		HeadCommit:     capObj.HeadCommit,
		GenesisShardID: capObj.GenesisShardID,
		CommitCount:    len(capObj.Commits),
		BlobCount:      len(capObj.Blobs),
		AnchorCount:    len(capObj.Anchors),
		SizeBytes:      cw.n,
	}, nil
}

// ExportCapsule packages repo (full history or incremental sinceCommit..head delta) into a
// self-verifying .nkt MemoryCapsule, stores it in BRC-167 CHIRP, and returns metadata + raw bytes.
func (s *Service) ExportCapsule(ctx context.Context, repo, branch, sinceCommit string) (*CapsuleExportResult, *memrepo.MemoryCapsule, []byte, error) {
	return s.ExportCapsuleAuthorized(ctx, repo, branch, sinceCommit, ownerPrincipal(), false)
}

// ExportCapsuleAuthorized packages repo into a .nkt MemoryCapsule while enforcing Cedar read
// authorization and redacting forgotten/unauthorized blobs into hash-preserving BlobStubs.
func (s *Service) ExportCapsuleAuthorized(ctx context.Context, repo, branch, sinceCommit string, principal policy.Principal, disallowBorg bool) (*CapsuleExportResult, *memrepo.MemoryCapsule, []byte, error) {
	capObj, err := s.buildMemoryCapsule(ctx, repo, branch, sinceCommit, principal, disallowBorg)
	if err != nil {
		return nil, nil, nil, err
	}
	var buf bytes.Buffer
	encOpts := s.capsuleEncodeOpts(ctx, &CapsuleExportOpts{
		SelfContained: true,
		SignExport:    true,
		Principal:     &principal,
		DisallowBorg:  disallowBorg,
	})
	if err := memrepo.WriteCapsuleStream(&buf, capObj, encOpts); err != nil {
		return nil, nil, nil, err
	}
	encoded := buf.Bytes()
	chirpURL, rootHash, _, err := bstack.StoreChirpWithHashes(s.objects, encoded)
	if err != nil {
		return nil, nil, nil, err
	}

	res := &CapsuleExportResult{
		Repo:           capObj.Repo,
		Branch:         capObj.Branch,
		BaseCommit:     capObj.BaseCommit,
		HeadCommit:     capObj.HeadCommit,
		GenesisShardID: capObj.GenesisShardID,
		ChirpURL:       chirpURL,
		RootHex:        hex.EncodeToString(rootHash[:]),
		CommitCount:    len(capObj.Commits),
		BlobCount:      len(capObj.Blobs),
		AnchorCount:    len(capObj.Anchors),
		SizeBytes:      len(encoded),
	}
	return res, capObj, encoded, nil
}

// ImportCapsule verifies and imports a .nkt MemoryCapsule (raw bytes or chirp:// URL) into targetRepo.
// When mergeIntoMain is true, it imports into a provenance branch ("fork/<genesis8>") and creates a
// 2-parent Merge Commit on targetBranch atomically inside ONE SQLite transaction!
func (s *Service) ImportCapsule(ctx context.Context, raw []byte, chirpURL, targetRepo, targetBranch string, mergeIntoMain bool) (*CapsuleImportResult, error) {
	return s.ImportCapsuleAuthorized(ctx, raw, chirpURL, targetRepo, targetBranch, mergeIntoMain, nil)
}

// ImportCapsuleStream verifies and imports a streaming .nkt Memory Capsule from r into targetRepo.
func (s *Service) ImportCapsuleStream(ctx context.Context, r io.Reader, targetRepo, targetBranch string, mergeIntoMain bool, principal *policy.Principal, unwrapDEK func(memrepo.CapsuleEnvelopeHeader) ([]byte, error)) (*CapsuleImportResult, error) {
	capObj, err := memrepo.ReadCapsuleStream(r, memrepo.CapsuleVerifyOpts{
		Objects:   s.objects,
		UnwrapDEK: unwrapDEK,
	})
	if err != nil {
		return nil, err
	}
	return s.importDecodedCapsule(ctx, capObj, nil, targetRepo, targetBranch, mergeIntoMain, principal)
}

// ImportVerifiedCapsule imports an ALREADY-decoded and ALREADY-verified *memrepo.MemoryCapsule into
// targetRepo, eliminating duplicate decompression, parsing, and Chronicle Bitcoin Script VM verification
// on the BuyerVerifyAndCommit market settlement hot path.
func (s *Service) ImportVerifiedCapsule(
	ctx context.Context,
	capObj *memrepo.MemoryCapsule,
	preVer *memrepo.CapsuleVerifyReport,
	targetRepo, targetBranch string,
	mergeIntoMain bool,
	principal *policy.Principal,
) (*CapsuleImportResult, error) {
	if capObj == nil {
		return nil, errors.New("memory: nil decoded capsule")
	}
	return s.importDecodedCapsule(ctx, capObj, preVer, targetRepo, targetBranch, mergeIntoMain, principal)
}

// ImportCapsuleAuthorized enforces Cedar write policy, verifies the capsule, and persists all
// blobs, trees, commits, transitions, branch ref, anchors, fragments, and optional 2-parent merge
// commit atomically in ONE single SQLite transaction while holding the shard lock.
func (s *Service) ImportCapsuleAuthorized(ctx context.Context, raw []byte, chirpURL, targetRepo, targetBranch string, mergeIntoMain bool, principal *policy.Principal) (*CapsuleImportResult, error) {
	if len(raw) == 0 && strings.TrimSpace(chirpURL) != "" {
		var err error
		raw, err = bstack.ResolveChirp(s.objects, strings.TrimSpace(chirpURL))
		if err != nil {
			return nil, fmt.Errorf("memory: resolve capsule chirp: %w", err)
		}
	}
	return s.ImportCapsuleStream(ctx, bytes.NewReader(raw), targetRepo, targetBranch, mergeIntoMain, principal, nil)
}

func (s *Service) resolveLocalBaseAnchor(targetRepo string, capObj *memrepo.MemoryCapsule, mergeIntoMain bool) *store.BatchAnchor {
	if capObj == nil || capObj.BaseAnchorN < 0 {
		return nil
	}
	candidates := []string{targetRepo}
	isForeign := mergeIntoMain || !strings.EqualFold(capObj.OwnerPub, s.port.IdentityPubHex()) || !strings.EqualFold(targetRepo, capObj.Repo)
	if isForeign {
		tag := capObj.GenesisShardID
		if tag == "" {
			tag = capObj.Repo
		}
		if tag == "" {
			tag = "external"
		}
		candidates = append([]string{"import:" + tag}, candidates...)
	}
	for _, sh := range candidates {
		if a, err := s.db.GetAnchor(sh, capObj.BaseAnchorN); err == nil {
			return &a
		}
	}
	return nil
}

func (s *Service) importDecodedCapsule(
	ctx context.Context,
	cap *memrepo.MemoryCapsule,
	preVer *memrepo.CapsuleVerifyReport,
	targetRepo, targetBranch string,
	mergeIntoMain bool,
	principal *policy.Principal,
) (*CapsuleImportResult, error) {
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

	var ver *memrepo.CapsuleVerifyReport
	if preVer != nil && preVer.OK {
		ver = preVer
	} else {
		localBase := s.resolveLocalBaseAnchor(targetRepo, cap, mergeIntoMain)
		ver, err = memrepo.VerifyCapsuleWithOpts(ctx, cap, memrepo.CapsuleVerifyOpts{
			Objects:         s.objects,
			Tracker:         s.port.Tracker(),
			MemoryMode:      s.port.Network() == "memory",
			LocalBaseAnchor: localBase,
		})
		if err != nil {
			return nil, err
		}
		if !ver.OK {
			return &CapsuleImportResult{Repo: targetRepo, Branch: dstBranch, Verification: ver}, fmt.Errorf("memory: capsule verification failed: %s", ver.Reason)
		}
	}

	unlock := s.lockShard(targetRepo)
	defer unlock()

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

	// Fail closed BEFORE mutating local state if any imported anchor conflicts with an existing anchor at batch n.
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

	var lastHead memrepo.CommitObject
	if len(cap.Commits) > 0 {
		lastHead = cap.Commits[len(cap.Commits)-1]
	}

	// Pre-prepare the 2-parent merge commit snapshot BEFORE opening the single atomic SQLite tx
	// (no nested DB reads inside RunRepoTx; shard lock is held continuously!).
	var (
		mergePrep    *preparedFileSnapshot
		mergeParents []memrepo.ParentRef
		mergeGenPub  string
		mergeGenID   string
		mergeExpHead string
	)
	if mergeIntoMain && lastHead.CommitHash != "" {
		mergedEntries := make(map[string]memrepo.TreeEntry)
		if curBr, bErr := s.db.GetMemBranch(targetRepo, dstBranch); bErr == nil && curBr.HeadCommit != "" {
			mergeExpHead = curBr.HeadCommit
			mergeGenPub = curBr.GenesisPub
			mergeGenID = curBr.GenesisShardID
			if curRow, rErr := s.db.GetMemCommit(curBr.HeadCommit); rErr == nil {
				curC := commitFromRow(curRow)
				if curTree, ctErr := s.loadTreeObject(curC.TreeHash); ctErr == nil && curTree != nil {
					for _, e := range curTree.Entries {
						mergedEntries[e.Path] = e
					}
				}
				mergeParents = append(mergeParents, memrepo.ParentRef{
					CommitHash: curC.CommitHash,
					AnchorTxID: curC.AnchorTxID,
				})
			}
		}
		if mergeGenPub == "" || mergeGenID == "" {
			localAnchors, _ := s.db.ListAnchors(targetRepo)
			mergeGenPub, mergeGenID, _ = s.resolveRepoGenesis(ctx, targetRepo, localAnchors)
		}
		if srcEntries, fErr := memrepo.FlattenTreeFromMap(lastHead.TreeHash, cap.Trees); fErr == nil {
			for _, e := range srcEntries {
				mergedEntries[e.Path] = e
			}
		} else if tr, ok := cap.Trees[lastHead.TreeHash]; ok {
			for _, e := range tr.Entries {
				mergedEntries[e.Path] = e
			}
		}
		sellerAnchorTx := lastHead.AnchorTxID
		if sellerAnchorTx == "" && len(cap.Anchors) > 0 {
			sellerAnchorTx = cap.Anchors[len(cap.Anchors)-1].TxID
		}
		mergeParents = append(mergeParents, memrepo.ParentRef{
			CommitHash:  lastHead.CommitHash,
			AnchorTxID:  sellerAnchorTx,
			RepoGenesis: cap.GenesisShardID,
		})
		var pErr error
		mergePrep, pErr = s.prepareFileSnapshot(
			ctx,
			targetRepo,
			nil,
			fmt.Sprintf("Fork & merge verified capsule %s (%d commits)", cap.Repo, len(cap.Commits)),
			"capsule-merge",
			&commitSnapshotOpts{PreservedEntries: mergedEntries},
		)
		if pErr != nil {
			return nil, fmt.Errorf("memory: prepare capsule merge: %w", pErr)
		}
	}

	importedCommits := 0
	importedFrags := 0
	var importedSeqs []int64
	var mergeCommitHash string

	if err := s.db.RunRepoTx(func(rtx *store.RepoTx) error {
		for _, b := range cap.Blobs {
			bRaw, mErr := memrepo.EncodeBlobObjectCBOR(b)
			if mErr != nil {
				return fmt.Errorf("memory: encode imported blob %s: %w", b.BlobHash, mErr)
			}
			if err := rtx.PutMemObject(b.BlobHash, "blob", bRaw, now); err != nil {
				return fmt.Errorf("memory: save imported blob %s: %w", b.BlobHash, err)
			}
			if b.Redacted || b.Forgotten {
				if err := rtx.MarkBlobRedacted(b.BlobHash, "imported-stub", "", now); err != nil {
					return err
				}
			}
		}
		for _, tr := range cap.Trees {
			tRaw, mErr := memrepo.EncodeTreeObjectCBOR(tr)
			if mErr != nil {
				return fmt.Errorf("memory: encode imported tree %s: %w", tr.TreeHash, mErr)
			}
			if err := rtx.PutMemObject(tr.TreeHash, "tree", tRaw, now); err != nil {
				return fmt.Errorf("memory: save imported tree %s: %w", tr.TreeHash, err)
			}
		}

		// Append fragments first so we know each fragment's local assigned seq for mem_commits.seq
		// while preserving the seller's original seq in mem_commits.origin_seq!
		localSeqByCommit := make(map[string]int64, len(cap.Fragments))
		localSeqByOriginSeq := make(map[int64]int64, len(cap.Fragments))
		for _, f := range cap.Fragments {
			origSeq := f.Seq
			f.Shard = targetRepo
			f.Tainted = true
			if isForeignImport {
				f.BatchN = -1
				f.LeafIndex = -1
				f.AnchorTxID = ""
			}
			storedF, dedup, aErr := rtx.AppendFragment(f)
			if aErr != nil {
				return fmt.Errorf("memory: append imported fragment seq=%d: %w", origSeq, aErr)
			}
			if storedF.CommitHash != "" {
				localSeqByCommit[storedF.CommitHash] = storedF.Seq
			}
			if origSeq > 0 {
				localSeqByOriginSeq[origSeq] = storedF.Seq
			}
			if !dedup {
				importedFrags++
				importedSeqs = append(importedSeqs, storedF.Seq)
			}
		}

		for _, c := range cap.Commits {
			row := rowFromCommit(c)
			// Preserve original signed (Repo, Branch, Seq) so VerifyCommitObject stays valid on re-export.
			row.OriginRepo = c.Repo
			row.OriginBranch = c.Branch
			row.OriginSeq = c.Seq
			row.Repo = targetRepo
			row.Branch = importBranch
			if ls, ok := localSeqByCommit[c.CommitHash]; ok {
				row.Seq = ls
			} else if ls, ok := localSeqByOriginSeq[c.Seq]; ok {
				row.Seq = ls
			}
			if err := rtx.SaveMemCommit(row); err != nil {
				return fmt.Errorf("memory: save imported commit %s: %w", c.CommitHash, err)
			}
			importedCommits++
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

		// Persist verified Merkle Automaton transitions under anchorTargetShard (or targetRepo for same-owner restore).
		for _, trRec := range cap.Transitions {
			trCBOR, eErr := core.EncodeTransitionCBOR(trRec)
			if eErr != nil {
				return fmt.Errorf("memory: encode imported transition seq=%d: %w", trRec.Seq, eErr)
			}
			trShard := anchorTargetShard
			if !isForeignImport {
				trShard = targetRepo
			}
			if err := rtx.SaveTransitionIgnore(store.TransitionRow{
				Shard:        trShard,
				Seq:          trRec.Seq,
				Hash:         core.ToHex(trRec.Hash),
				PrevQ:        core.ToHex(trRec.PrevStateQ),
				NextQ:        core.ToHex(trRec.NextStateQ),
				CommitHash:   core.ToHex(trRec.CommitHash),
				TreeHash:     core.ToHex(trRec.TreeHash),
				PolicyHash:   core.ToHex(trRec.PolicyHash),
				OntologyHash: core.ToHex(trRec.OntologyHash),
				BodyCBOR:     trCBOR,
				CreatedAt:    trRec.Timestamp,
			}); err != nil {
				return fmt.Errorf("memory: save imported transition seq=%d: %w", trRec.Seq, err)
			}
		}

		for _, a := range cap.Anchors {
			a.Shard = anchorTargetShard
			// Never trust untrusted Settled=true or BlockHeight; strictly trust verified BUMP values from VerifyCapsule.
			a.Settled = ver.SettledByN[a.N]
			a.BlockHeight = ver.BlockHeightByN[a.N]
			if err := rtx.SaveBatchAnchorStrict(a); err != nil {
				return fmt.Errorf("memory: save imported capsule anchor n=%d: %w", a.N, err)
			}
		}

		// Execute the 2-parent merge commit inside the SAME atomic transaction!
		if mergePrep != nil {
			mc, mErr := s.commitFileSnapshotTx(
				ctx,
				rtx,
				targetRepo,
				dstBranch,
				mergeParents,
				mergePrep,
				mergeGenPub,
				mergeGenID,
				&commitSnapshotOpts{
					ExpectedHead:          mergeExpHead,
					HasExpectedHead:       true,
					SkipSyntheticFragment: importedFrags > 0,
				},
			)
			if mErr != nil {
				return fmt.Errorf("memory: commit capsule merge: %w", mErr)
			}
			mergeCommitHash = mc.CommitHash
		}
		return nil
	}); err != nil {
		return nil, err
	}

	s.projCacheMu.Lock()
	if s.projCache != nil {
		delete(s.projCache, targetRepo)
	}
	s.projCacheMu.Unlock()

	if importedFrags > 0 || mergeCommitHash != "" {
		s.notifyRemember(targetRepo)
	}

	return &CapsuleImportResult{
		Repo:            targetRepo,
		Branch:          dstBranch,
		ImportedCommits: importedCommits,
		ImportedFrags:   importedFrags,
		ImportedSeqs:    importedSeqs,
		MergeCommitHash: mergeCommitHash,
		Verification:    ver,
	}, nil
}

type countingWriter struct {
	w io.Writer
	n int
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += n
	return n, err
}
