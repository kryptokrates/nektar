package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

func (s *Service) buildFragmentBlob(
	relPath, mediaType string,
	data, rawBlob []byte,
	chirpURL, contentSHAHex string,
	borgGroup string,
	borgEpoch int64,
	latticeEpoch uint64,
	keyedMeta []byte,
	level int,
	class string,
) (*memrepo.BlobObject, error) {
	payload := data
	if len(rawBlob) > 0 {
		payload = rawBlob
	}
	if borgGroup != "" || latticeEpoch > 0 {
		// Encrypted fragments (BorgGroup or CLKA) MUST NEVER store plaintext inline or write plaintext to CHIRP.
		shaHex := contentSHAHex
		if shaHex == "" && len(payload) > 0 {
			sum := sha256.Sum256(payload)
			shaHex = hex.EncodeToString(sum[:])
		}
		return memrepo.BuildEncryptedBlobWithMeta(relPath, mediaType, int64(len(payload)), shaHex, chirpURL, borgGroup, borgEpoch, latticeEpoch, keyedMeta, level, class)
	}
	b, _, err := memrepo.BuildBlobWithMeta(s.objects, relPath, mediaType, payload, len(rawBlob) > 0, level, class)
	if err != nil {
		return nil, err
	}
	if chirpURL != "" && b.ChirpURL == "" {
		b.ChirpURL = chirpURL
		h, hErr := memrepo.ComputeBlobHash(*b)
		if hErr != nil {
			return nil, hErr
		}
		b.BlobHash = h
	}
	return b, nil
}

func buildCommittedFragmentMPE(shard, hdpHex string, prov core.Provenance, level int, class, relPath, mediaType, chirpURL, dataHashHex string) (*core.MPE, error) {
	return buildCommittedFragmentMPEWithGraph(shard, hdpHex, prov, level, class, relPath, mediaType, chirpURL, dataHashHex, "", "")
}

func buildCommittedFragmentMPEWithGraph(shard, hdpHex string, prov core.Provenance, level int, class, relPath, mediaType, chirpURL, dataHashHex, ontologyID, edgesRoot string) (*core.MPE, error) {
	fields := map[string][]byte{
		"shard":      []byte(shard),
		"hdp":        []byte(hdpHex),
		"source":     []byte(prov.Source),
		"time":       []byte(strconv.FormatInt(prov.Time, 10)),
		"modelId":    []byte(prov.ModelID),
		"level":      []byte(strconv.Itoa(level)),
		"class":      []byte(class),
		"path":       []byte(relPath),
		"mediaType":  []byte(mediaType),
		"chirpUrl":   []byte(chirpURL),
		"signature":  []byte(hex.EncodeToString(prov.Signature)),
		"dataSha256": []byte(dataHashHex),
	}
	if ontologyID != "" {
		fields["ontologyId"] = []byte(ontologyID)
	}
	if edgesRoot != "" {
		fields["edgesRoot"] = []byte(edgesRoot)
	}
	return core.BuildMPE(fields)
}

// Forget performs append-only cryptographic erasure of (shard, seq): shreds local plaintext/ciphertext/FTS5/CHIRP,
// replaces the content-addressed blob payload with its hash-preserving BlobStub, and appends a signed tombstone
// CommitObject + TransitionRecord removing the path from HEAD without mutating any historical commit.
func (s *Service) Forget(ctx context.Context, shard string, seq int64) error {
	return s.ForgetAuthorized(ctx, shard, seq, policy.Principal{Type: "Agent", ID: "owner", Level: 100})
}

// ForgetAuthorized enforces Cedar write authorization and performs strict append-only cryptographic erasure.
func (s *Service) ForgetAuthorized(ctx context.Context, shard string, seq int64, principal policy.Principal) error {
	if err := ValidateShardName(shard, false); err != nil {
		return err
	}
	unlock := s.lockShard(shard)
	defer unlock()

	rec, err := s.db.GetFragment(shard, seq)
	if err != nil {
		return err
	}
	if principal.ID == "" {
		principal = policy.Principal{Type: "Agent", ID: "owner", Level: 100}
	}
	if dec, aErr := s.AuthorizeAction(principal, "write", policy.Resource{
		Type:  "Fragment",
		ID:    fmt.Sprintf("%s#%d", rec.Shard, rec.Seq),
		Level: int64(rec.Level),
		Shard: rec.Shard,
		Class: rec.Class,
	}); aErr != nil || !dec.Allow {
		if aErr != nil {
			return fmt.Errorf("memory: cedar forget policy error: %w", aErr)
		}
		return fmt.Errorf("memory: cedar policy denied forget on %s#%d (level %d > principal level %d)", rec.Shard, rec.Seq, rec.Level, principal.Level)
	}
	if rec.ChirpURL != "" && s.objects != nil {
		if hashes, _ := bstack.DeleteChirp(s.objects, rec.ChirpURL); len(hashes) > 0 {
			s.RevokePublicChirpHashes(hashes...)
		}
	}

	now := time.Now().Unix()
	authorPubHex := s.commitAuthorPubHex(ctx)
	policyHashHex := ""
	if pc := s.PolicyCommit(); pc != nil {
		policyHashHex = pc.Hash
	}
	ontologyHashHex := s.OntologyHead()

	err = s.db.RunRepoTx(func(rtx *store.RepoTx) error {
		// 1. Shred fragment row, FTS5, graph edges, and NMU metadata (never mutating mem_commits!).
		if err := rtx.ForgetFragmentRow(shard, seq); err != nil {
			return err
		}

		// 2. Identify target blob hashes & paths to redact.
		targetPaths := make(map[string]bool)
		if rec.Path != "" {
			targetPaths[rec.Path] = true
			targetPaths["graph/"+rec.Path+".edges.json"] = true
		}
		defaultMD := fmt.Sprintf("facts/%04d.md", seq)
		defaultTxt := fmt.Sprintf("facts/%d.txt", seq)
		targetPaths[defaultMD] = true
		targetPaths[defaultTxt] = true
		targetPaths["graph/"+defaultMD+".edges.json"] = true
		targetPaths["graph/"+defaultTxt+".edges.json"] = true

		blobHashes := make(map[string]bool)
		if rec.BlobHash != "" {
			blobHashes[rec.BlobHash] = true
		}

		// Inspect the fragment's original commit tree to catch any sidecar blobs or legacy rows without blob_hash.
		if rec.CommitHash != "" {
			if cRow, cErr := rtx.GetMemCommit(rec.CommitHash); cErr == nil && cRow.TreeHash != "" {
				if origEntries, tErr := loadTreeEntriesTx(rtx, cRow.TreeHash); tErr == nil {
					for _, e := range origEntries {
						if targetPaths[e.Path] && e.BlobHash != "" {
							blobHashes[e.BlobHash] = true
						}
					}
				}
			}
		}

		// 3. Replace each affected blob in mem_objects with its hash-preserving BlobStub.
		for bh := range blobHashes {
			b, bErr := loadBlobObjectTx(rtx, bh)
			if bErr != nil || b == nil {
				continue
			}
			stub := b.ToRedactedStub()
			stub.Forgotten = true
			stubRaw, eErr := memrepo.EncodeBlobObjectCBOR(stub)
			if eErr != nil {
				return fmt.Errorf("memory: encode redacted blob stub %s: %w", bh, eErr)
			}
			if err := rtx.ReplaceMemObjectBody(bh, stubRaw); err != nil {
				return fmt.Errorf("memory: replace redacted blob %s: %w", bh, err)
			}
		}

		// 4. Append a signed Tombstone CommitObject + TransitionRecord removing targetPaths from HEAD.
		var tombstoneCommitHash string
		if !IsSystemShard(shard) {
			br, bErr := rtx.GetMemBranch(shard, memrepo.DefaultBranch)
			if bErr == nil && br.HeadCommit != "" {
				headRow, hErr := rtx.GetMemCommit(br.HeadCommit)
				if hErr == nil {
					headCommit := commitFromRow(headRow)
					headEntries, tErr := loadTreeEntriesTx(rtx, headCommit.TreeHash)
					if tErr == nil {
						kept := make([]memrepo.TreeEntry, 0, len(headEntries))
						for _, e := range headEntries {
							if targetPaths[e.Path] || blobHashes[e.BlobHash] {
								continue
							}
							kept = append(kept, e)
						}
						newRootTree, bucketTrees, trErr := memrepo.BuildShardedTree(kept)
						if trErr != nil {
							return fmt.Errorf("memory: build tombstone tree: %w", trErr)
						}
						for _, bt := range bucketTrees {
							btRaw, eErr := memrepo.EncodeTreeObjectCBOR(bt)
							if eErr != nil {
								return eErr
							}
							if err := rtx.PutMemObject(bt.TreeHash, "tree", btRaw, now); err != nil {
								return err
							}
						}
						rootRaw, eErr := memrepo.EncodeTreeObjectCBOR(*newRootTree)
						if eErr != nil {
							return eErr
						}
						if err := rtx.PutMemObject(newRootTree.TreeHash, "tree", rootRaw, now); err != nil {
							return err
						}

						tombstone, finErr := memrepo.FinalizeCommit(memrepo.CommitObject{
							Version:        1,
							Repo:           shard,
							Branch:         memrepo.DefaultBranch,
							CommitKind:     memrepo.CommitKindTombstone,
							Parents:        []memrepo.ParentRef{{CommitHash: headCommit.CommitHash, AnchorTxID: headCommit.AnchorTxID}},
							TreeHash:       newRootTree.TreeHash,
							AuthorPub:      authorPubHex,
							ModelID:        s.modelID.Provider + "/" + s.modelID.Release,
							PolicyCommit:   policyHashHex,
							OntologyCommit: ontologyHashHex,
							Message:        fmt.Sprintf("Tombstone forget %s#%d", shard, seq),
							Timestamp:      now,
							AnchorN:        -1,
						}, func(digest []byte) ([]byte, error) {
							return s.port.SignDigest(ctx, digest)
						})
						if finErr != nil {
							return fmt.Errorf("memory: sign tombstone commit: %w", finErr)
						}
						tombstoneCommitHash = tombstone.CommitHash
						if err := rtx.SaveMemCommit(rowFromCommit(tombstone)); err != nil {
							return err
						}
						if err := rtx.CompareAndSwapMemBranch(store.MemBranchRow{
							Repo:           shard,
							Branch:         memrepo.DefaultBranch,
							HeadCommit:     tombstone.CommitHash,
							GenesisPub:     br.GenesisPub,
							GenesisShardID: br.GenesisShardID,
							UpdatedAt:      now,
						}, br.HeadCommit); err != nil {
							return err
						}

						// Advance Merkle Automaton TransitionRecord (q_i -> q_{i+1}) for the tombstone.
						nextTrSeq, sErr := rtx.NextTransitionSeq(shard)
						if sErr != nil {
							return sErr
						}
						prevQHex, qErr := rtx.LatestStateQ(shard)
						if qErr != nil {
							return qErr
						}
						prevQBytes := core.Zero32Bytes()
						if prevQHex != "" {
							if qb, dErr := hex.DecodeString(prevQHex); dErr == nil && len(qb) == 32 {
								prevQBytes = qb
							}
						}
						tombInput := core.DSHA256(core.Concat(
							core.TagTombstoneV1,
							core.Field([]byte(shard)),
							core.Field([]byte(strconv.FormatInt(seq, 10))),
							core.Field([]byte(rec.HDP)),
						))
						commitHashBytes, _ := hex.DecodeString(tombstone.CommitHash)
						treeHashBytes, _ := hex.DecodeString(newRootTree.TreeHash)
						var polBytes, ontBytes []byte
						if policyHashHex != "" {
							polBytes, _ = hex.DecodeString(policyHashHex)
						}
						if ontologyHashHex != "" {
							ontBytes, _ = hex.DecodeString(ontologyHashHex)
						}
						nextQBytes, qnErr := core.NextStepStateQ(prevQBytes, tombInput, commitHashBytes, treeHashBytes, polBytes, ontBytes, now)
						if qnErr != nil {
							return fmt.Errorf("memory: compute tombstone stateQ: %w", qnErr)
						}
						trRec := core.TransitionRecord{
							Version:      1,
							Shard:        shard,
							Seq:          nextTrSeq,
							PrevStateQ:   prevQBytes,
							InputDigest:  tombInput,
							NextStateQ:   nextQBytes,
							CommitHash:   commitHashBytes,
							TreeHash:     treeHashBytes,
							PolicyHash:   polBytes,
							OntologyHash: ontBytes,
							Timestamp:    now,
						}
						trHashBytes, trErr := core.ComputeTransitionHash(trRec)
						if trErr != nil {
							return fmt.Errorf("memory: compute tombstone transition hash: %w", trErr)
						}
						trRec.Hash = trHashBytes
						trCBOR, trErr := core.EncodeTransitionCBOR(trRec)
						if trErr != nil {
							return fmt.Errorf("memory: encode tombstone transition CBOR: %w", trErr)
						}
						if err := rtx.SaveTransition(store.TransitionRow{
							Shard:        shard,
							Seq:          nextTrSeq,
							Hash:         core.ToHex(trHashBytes),
							PrevQ:        core.ToHex(prevQBytes),
							NextQ:        core.ToHex(nextQBytes),
							CommitHash:   tombstone.CommitHash,
							TreeHash:     newRootTree.TreeHash,
							PolicyHash:   policyHashHex,
							OntologyHash: ontologyHashHex,
							BodyCBOR:     trCBOR,
							CreatedAt:    now,
						}); err != nil {
							return err
						}
					}
				}
			}
		}

		for bh := range blobHashes {
			if err := rtx.MarkBlobRedacted(bh, "forgotten", tombstoneCommitHash, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.projCacheMu.Lock()
	s.projCache = nil
	s.projCacheMu.Unlock()
	return nil
}
