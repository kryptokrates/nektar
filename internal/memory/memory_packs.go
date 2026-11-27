package memory

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

// ShardPackResult describes an exported self-verifying .nkt CHIRP Memory Capsule closure.
type ShardPackResult struct {
	Shard    string `json:"shard"`
	ChirpURL string `json:"chirpUrl"`
	RootHex  string `json:"rootHex"`
	Size     int    `json:"size"`
}

// ExportShardPack exports a Memory Repo as a self-verifying .nkt CHIRP Memory Capsule
// (Git/Metanet Commit DAG + volumetric blobs + NektarAnchor UTXO lineage BEEF proofs).
// Legacy flat JSON ShardPack v1 has been removed; .nkt Capsules are the sole portable format.
func (s *Service) ExportShardPack(ctx context.Context, shard string) (*ShardPackResult, error) {
	res, _, err := s.exportCapsuleAuthorized(ctx, shard, policy.Principal{Type: "Agent", ID: "owner", Level: 100}, "read", false, false, false)
	return res, err
}

// ExportPublicShardPack exports only active, Public (unencrypted), Level-0, non-Borg, Cedar-"disclose"
// fragments as a self-verifying .nkt CHIRP Capsule and registers its DAG object hashes for unauthenticated reads.
func (s *Service) ExportPublicShardPack(ctx context.Context, shard string) (*ShardPackResult, error) {
	res, hashes, err := s.exportCapsuleAuthorized(ctx, shard, policy.Principal{Type: "Agent", ID: "public", Level: 0}, "disclose", true, true, true)
	if err != nil {
		return nil, err
	}
	s.AllowPublicChirpHashes(hashes...)
	return res, nil
}

// ExportShardPackAuthorized exports only active, Cedar-authorized fragments from shard as a
// self-verifying .nkt CHIRP Memory Capsule.
func (s *Service) ExportShardPackAuthorized(ctx context.Context, shard string, principal policy.Principal, action string, disallowBorg bool) (*ShardPackResult, error) {
	res, _, err := s.exportCapsuleAuthorized(ctx, shard, principal, action, disallowBorg, false, true)
	return res, err
}

func (s *Service) exportCapsuleAuthorized(ctx context.Context, shard string, principal policy.Principal, action string, disallowBorg, disallowPrivate, excludeSuperseded bool) (*ShardPackResult, [][32]byte, error) {
	if err := ValidateShardName(shard, false); err != nil {
		return nil, nil, err
	}
	if principal.ID == "" {
		principal = policy.Principal{Type: "Agent", ID: "public", Level: 0}
	}
	if action == "" {
		action = "disclose"
	}

	allFrags, err := s.db.ListFragments(shard)
	if err != nil {
		return nil, nil, err
	}
	superseded := s.CollectSupersededSeqs(shard)
	s.mu.RLock()
	polText := s.policyText
	s.mu.RUnlock()

	frags := make([]store.FragmentRecord, 0, len(allFrags))
	for _, r := range allFrags {
		if r.Forgotten || (excludeSuperseded && superseded[r.Seq] > 0) {
			continue
		}
		if disallowBorg && r.BorgGroup != "" {
			continue
		}
		if disallowPrivate && r.LatticeEpoch > 0 && !r.Public {
			continue
		}
		dec, aErr := policy.Authorize(polText, principal, action, policy.Resource{
			Type:  "Fragment",
			ID:    fmt.Sprintf("%s#%d", r.Shard, r.Seq),
			Level: int64(r.Level),
			Shard: r.Shard,
			Class: r.Class,
		})
		if aErr != nil || !dec.Allow {
			continue
		}
		frags = append(frags, r)
	}

	// Build a policy-filtered .nkt MemoryCapsule preserving all authorized commits, trees, blobs, and UTXO anchors.
	filtered := len(frags) != len(allFrags)
	var headCommitHash string
	if br, bErr := s.db.GetMemBranch(shard, memrepo.DefaultBranch); bErr == nil {
		headCommitHash = br.HeadCommit
	}
	var commits []memrepo.CommitObject
	trees := make(map[string]memrepo.TreeObject)
	blobs := make(map[string]memrepo.BlobObject)
	var chirpHashes [][32]byte

	if !filtered {
		commits = s.collectReachableCommitsTopo(headCommitHash, "")
		trees, blobs, chirpHashes = s.collectCommitDAGObjects(shard, commits, principal, disallowBorg, true)
	} else {
		// Policy-filtered export: assemble a clean authorized snapshot tree so restricted fragments never leak via parent trees.
		entries := make([]memrepo.TreeEntry, 0, len(frags))
		for _, r := range frags {
			relPath := r.Path
			if relPath == "" {
				relPath = fmt.Sprintf("facts/%04d.md", r.Seq)
			}
			b, bErr := s.buildFragmentBlob(relPath, r.MediaType, r.Data, nil, r.ChirpURL, r.ContentSHA256, r.BorgGroup, r.BorgEpoch, r.LatticeEpoch, r.KeyedMeta, r.Level, r.Class)
			if bErr != nil || b == nil {
				continue
			}
			if b.ChirpURL != "" && !b.Encrypted {
				if hs, cErr := bstack.CollectChirpHashes(s.objects, b.ChirpURL); cErr == nil {
					chirpHashes = append(chirpHashes, hs...)
				}
			}
			blobs[b.BlobHash] = *b
			entries = append(entries, memrepo.TreeEntry{
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
		if tr, tErr := memrepo.BuildTree(entries); tErr == nil && tr != nil {
			trees[tr.TreeHash] = *tr
			if c, cErr := memrepo.FinalizeCommit(memrepo.CommitObject{
				Version:        1,
				Repo:           shard,
				Branch:         memrepo.DefaultBranch,
				TreeHash:       tr.TreeHash,
				AuthorPub:      s.commitAuthorPubHex(ctx),
				ModelID:        s.modelID.Provider + "/" + s.modelID.Release,
				PolicyCommit:   s.PolicyCommit().Hash,
				OntologyCommit: s.OntologyHead(),
				Message:        fmt.Sprintf("Authorized capsule export (%d entries)", len(entries)),
				Timestamp:      time.Now().Unix(),
				AnchorN:        -1,
			}, func(digest []byte) ([]byte, error) {
				return s.port.SignDigest(ctx, digest)
			}); cErr == nil {
				commits = []memrepo.CommitObject{c}
				headCommitHash = c.CommitHash
			}
		}
	}

	anchors, err := s.db.ListAnchors(shard)
	if err != nil {
		return nil, nil, err
	}
	genPub, genID, _ := s.resolveRepoGenesis(ctx, shard, anchors)
	if len(commits) > 0 {
		headCommitHash = commits[len(commits)-1].CommitHash
	}
	cap := &memrepo.MemoryCapsule{
		Format:         memrepo.CapsuleFormatVersion,
		Repo:           shard,
		Branch:         memrepo.DefaultBranch,
		GenesisPub:     genPub,
		GenesisShardID: genID,
		OwnerPub:       s.port.IdentityPubHex(),
		HeadCommit:     headCommitHash,
		ExportedAt:     time.Now().Unix(),
		Commits:        commits,
		Trees:          trees,
		Blobs:          blobs,
		Anchors:        anchors,
		Fragments:      frags,
		BatchProofs:    s.buildCapsuleBatchProofs(shard, anchors, frags, commits),
	}
	raw, err := memrepo.EncodeCapsule(cap)
	if err != nil {
		return nil, nil, err
	}
	u, root, packHashes, err := bstack.StoreChirpWithHashes(s.objects, raw)
	if err != nil {
		return nil, nil, err
	}
	chirpHashes = append(chirpHashes, packHashes...)
	return &ShardPackResult{
		Shard:    shard,
		ChirpURL: u,
		RootHex:  hex.EncodeToString(root[:]),
		Size:     len(raw),
	}, chirpHashes, nil
}

// ImportShardPack resolves and strictly verifies a .nkt CHIRP Memory Capsule (enforcing
// NektarAnchor UTXO spend lineage) and forks/merges it into targetShard.
func (s *Service) ImportShardPack(ctx context.Context, chirpURL, targetShard string) (int, error) {
	raw, err := bstack.ResolveChirp(s.objects, chirpURL)
	if err != nil {
		return 0, err
	}
	n, _, err := s.ImportShardPackBytesCtx(ctx, raw, targetShard)
	return n, err
}

// ImportShardPackBytes verifies and imports raw .nkt Memory Capsule bytes into targetShard.
func (s *Service) ImportShardPackBytes(raw []byte, targetShard string) (int, string, error) {
	return s.ImportShardPackBytesCtx(context.Background(), raw, targetShard)
}

// ImportShardPackBytesCtx strictly decodes and verifies a .nkt Memory Capsule (rejecting legacy
// flat JSON packs without UTXO-lineage capsule framing) and imports it into targetShard.
func (s *Service) ImportShardPackBytesCtx(ctx context.Context, raw []byte, targetShard string) (int, string, error) {
	if !memrepo.IsCapsuleBytes(raw) {
		return 0, targetShard, errors.New("memory: legacy flat ShardPack format is unsupported; only self-verifying .nkt Memory Capsules with UTXO lineage are accepted")
	}
	res, err := s.ImportCapsule(ctx, raw, "", targetShard, memrepo.DefaultBranch, true)
	if err != nil {
		return 0, targetShard, err
	}
	return res.ImportedFrags, res.Repo, nil
}
