package memory

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"strings"

	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

// InspectCapsule decodes and cryptographically verifies a .nkt Memory Capsule completely read-only
// (mutating ZERO local SQLite state) and computes a diff against any existing local repo of the same name.
func (s *Service) InspectCapsule(ctx context.Context, raw []byte, targetRepo string) (*CapsuleInspectResult, error) {
	return s.InspectCapsuleAuthorized(ctx, raw, targetRepo, ownerPrincipal(), false)
}

// InspectCapsuleStream streams, decodes, and verifies a .nkt Memory Capsule read-only from r.
func (s *Service) InspectCapsuleStream(ctx context.Context, r io.Reader, targetRepo string, principal policy.Principal, disallowBorg bool, unwrapDEK func(memrepo.CapsuleEnvelopeHeader) ([]byte, error)) (*CapsuleInspectResult, error) {
	capObj, err := memrepo.ReadCapsuleStream(r, memrepo.CapsuleVerifyOpts{
		Objects:   s.objects,
		UnwrapDEK: unwrapDEK,
	})
	if err != nil {
		return nil, err
	}
	return s.inspectDecodedCapsule(ctx, capObj, targetRepo, principal, disallowBorg)
}

// InspectCapsuleAuthorized verifies a .nkt Memory Capsule read-only and computes DiffVsLocal
// strictly through Cedar-authorized RepoTreeAuthorized (never owner-level RepoTree).
func (s *Service) InspectCapsuleAuthorized(ctx context.Context, raw []byte, targetRepo string, principal policy.Principal, disallowBorg bool) (*CapsuleInspectResult, error) {
	return s.InspectCapsuleStream(ctx, bytes.NewReader(raw), targetRepo, principal, disallowBorg, nil)
}

func (s *Service) inspectDecodedCapsule(
	ctx context.Context,
	capObj *memrepo.MemoryCapsule,
	targetRepo string,
	principal policy.Principal,
	disallowBorg bool,
) (*CapsuleInspectResult, error) {
	repoName := strings.TrimSpace(targetRepo)
	if repoName == "" {
		repoName = capObj.Repo
	}
	localBase := s.resolveLocalBaseAnchor(repoName, capObj, false)
	ver, err := memrepo.VerifyCapsuleWithOpts(ctx, capObj, memrepo.CapsuleVerifyOpts{
		Objects:         s.objects,
		Tracker:         s.port.Tracker(),
		MemoryMode:      s.port.Network() == "memory",
		LocalBaseAnchor: localBase,
	})
	if err != nil {
		return nil, err
	}
	var headTree *memrepo.TreeObject
	if capObj.HeadCommit != "" {
		for _, c := range capObj.Commits {
			if c.CommitHash == capObj.HeadCommit {
				if tr, ok := capObj.Trees[c.TreeHash]; ok {
					cp := tr
					headTree = &cp
				}
				break
			}
		}
	}
	if headTree == nil && len(capObj.Commits) > 0 {
		lastC := capObj.Commits[len(capObj.Commits)-1]
		if tr, ok := capObj.Trees[lastC.TreeHash]; ok {
			cp := tr
			headTree = &cp
		}
	}
	if headTree != nil && len(headTree.Subtrees) > 0 && len(headTree.Entries) == 0 {
		if flat, fErr := memrepo.FlattenTreeFromMap(headTree.TreeHash, capObj.Trees); fErr == nil {
			headTree.Entries = flat
		}
	}

	anchors := make([]RepoAnchorSummary, 0, len(capObj.Anchors))
	for _, a := range capObj.Anchors {
		anchors = append(anchors, RepoAnchorSummary{
			N:           a.N,
			Size:        a.Size,
			TxID:        a.TxID,
			RootHex:     a.RootHex,
			ChainedHex:  a.ChainedHex,
			Settled:     ver.SettledByN[a.N],
			BlockHeight: ver.BlockHeightByN[a.N],
			CreatedAt:   a.CreatedAt,
		})
	}

	// Reverse commits to newest-first for UI consistency with RepoLog.
	newestFirst := make([]memrepo.CommitObject, len(capObj.Commits))
	for i, c := range capObj.Commits {
		newestFirst[len(capObj.Commits)-1-i] = c
	}

	res := &CapsuleInspectResult{
		Repo:           capObj.Repo,
		Branch:         capObj.Branch,
		GenesisPub:     capObj.GenesisPub,
		GenesisShardID: capObj.GenesisShardID,
		OwnerPub:       capObj.OwnerPub,
		HeadCommit:     capObj.HeadCommit,
		ExportedAt:     capObj.ExportedAt,
		Verification:   ver,
		Commits:        newestFirst,
		Tree:           headTree,
		Blobs:          capObj.Blobs,
		Anchors:        anchors,
	}

	// Check if a local repo already exists to compute a clean Cedar-authorized DiffVsLocal preview.
	if repoName != "" && ValidateShardName(repoName, false) == nil {
		if _, localTree, _, lErr := s.RepoTreeAuthorized(ctx, repoName, memrepo.DefaultBranch, "", principal, disallowBorg); lErr == nil && localTree != nil && len(localTree.Entries) > 0 {
			res.LocalExists = true
			res.DiffVsLocal = memrepo.DiffTrees(localTree, headTree)
		}
	}
	return res, nil
}

// buildCapsuleBatchProofs builds compact SubsetDisclosureProofs for any anchored batch where
// fewer than all batch leaves are exported (e.g. due to Cedar filtering, forgotten fragments,
// or incremental delta exports) so offline VerifyCapsule cryptographically proves inclusion
// of every disclosed fragment and anchored commit leaf against each batch's NektarAnchor R_n.
func (s *Service) buildCapsuleBatchProofs(
	repo string,
	anchors []store.BatchAnchor,
	disclosedFrags []store.FragmentRecord,
	commits []memrepo.CommitObject,
) map[int64]*memrepo.SubsetDisclosureProof {
	if len(anchors) == 0 {
		return nil
	}
	fragIdxByBatch := make(map[int64]map[int]bool)
	for _, f := range disclosedFrags {
		if f.BatchN < 0 || f.LeafIndex < 0 {
			continue
		}
		if fragIdxByBatch[f.BatchN] == nil {
			fragIdxByBatch[f.BatchN] = make(map[int]bool)
		}
		fragIdxByBatch[f.BatchN][f.LeafIndex] = true
	}
	commitLeafByBatch := make(map[int64]map[string]bool)
	for _, c := range commits {
		if c.AnchorN < 0 || strings.TrimSpace(c.FragmentLeaf) == "" {
			continue
		}
		if commitLeafByBatch[c.AnchorN] == nil {
			commitLeafByBatch[c.AnchorN] = make(map[string]bool)
		}
		commitLeafByBatch[c.AnchorN][strings.ToLower(strings.TrimSpace(c.FragmentLeaf))] = true
	}

	var out map[int64]*memrepo.SubsetDisclosureProof
	for _, anc := range anchors {
		wantFragIdx := fragIdxByBatch[anc.N]
		wantCommitLeaves := commitLeafByBatch[anc.N]
		if len(wantFragIdx) == 0 && len(wantCommitLeaves) == 0 {
			continue
		}
		// Full batch already present in disclosedFrags with no missing commit leaves: no subset proof needed.
		if len(wantFragIdx) == anc.Size && len(wantCommitLeaves) == 0 {
			continue
		}
		leafHashes, err := s.db.ListBatchLeafHashes(repo, anc.N)
		if err != nil || len(leafHashes) != anc.Size || len(leafHashes) == 0 {
			continue
		}
		leaves := make([][]byte, len(leafHashes))
		indices := make([]int, 0, len(wantFragIdx)+len(wantCommitLeaves))
		valid := true
		for i, leafHex := range leafHashes {
			lb, dErr := hex.DecodeString(strings.TrimSpace(leafHex))
			if dErr != nil || len(lb) != 32 {
				valid = false
				break
			}
			leaves[i] = lb
			if wantFragIdx[i] || wantCommitLeaves[strings.ToLower(strings.TrimSpace(leafHex))] {
				indices = append(indices, i)
			}
		}
		if !valid || len(indices) == 0 {
			continue
		}
		// If every leaf is already carried directly by disclosedFrags, omit redundant subset proof.
		if len(wantFragIdx) == anc.Size && len(indices) == anc.Size {
			continue
		}
		sp, spErr := memrepo.BuildSubsetProof(leaves, indices)
		if spErr != nil || sp == nil {
			continue
		}
		if out == nil {
			out = make(map[int64]*memrepo.SubsetDisclosureProof)
		}
		out[anc.N] = sp
	}
	return out
}
