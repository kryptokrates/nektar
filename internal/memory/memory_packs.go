package memory

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

// ShardPack is a portable, CHIRP-addressed shard snapshot with fragments and anchor proofs.
type ShardPack struct {
	Version    int                    `json:"version"`
	Shard      string                 `json:"shard"`
	OwnerPub   string                 `json:"ownerPub"`
	ExportedAt int64                  `json:"exportedAt"`
	Fragments  []store.FragmentRecord `json:"fragments"`
	Anchors    []store.BatchAnchor    `json:"anchors"`
}

// ShardPackResult describes an exported BRC-167 CHIRP shard pack.
type ShardPackResult struct {
	Shard    string `json:"shard"`
	ChirpURL string `json:"chirpUrl"`
	RootHex  string `json:"rootHex"`
	Size     int    `json:"size"`
}

// ExportShardPack bundles shard fragments and anchors into a content-addressed BRC-167 CHIRP closure.
// For external/HTTP export, use ExportShardPackAuthorized or ExportPublicShardPack.
func (s *Service) ExportShardPack(ctx context.Context, shard string) (*ShardPackResult, error) {
	res, _, err := s.exportShardPackInternal(ctx, shard, policy.Principal{Type: "Agent", ID: "owner", Level: 100}, "read", false, false)
	return res, err
}

// ExportPublicShardPack exports only active, Level-0, non-Borg, Cedar-"disclose" fragments and registers the
// resulting CHIRP DAG object hashes in the public unauthenticated allowlist (for starter/market packs).
func (s *Service) ExportPublicShardPack(ctx context.Context, shard string) (*ShardPackResult, error) {
	res, hashes, err := s.exportShardPackInternal(ctx, shard, policy.Principal{Type: "Agent", ID: "public", Level: 0}, "disclose", true, true)
	if err != nil {
		return nil, err
	}
	s.AllowPublicChirpHashes(hashes...)
	return res, nil
}

// ExportShardPackAuthorized exports only active, Cedar-authorized fragments from shard into CHIRP.
func (s *Service) ExportShardPackAuthorized(ctx context.Context, shard string, principal policy.Principal, action string, disallowBorg bool) (*ShardPackResult, error) {
	res, _, err := s.exportShardPackInternal(ctx, shard, principal, action, disallowBorg, true)
	return res, err
}

func (s *Service) exportShardPackInternal(_ context.Context, shard string, principal policy.Principal, action string, disallowBorg, excludeSuperseded bool) (*ShardPackResult, [][32]byte, error) {
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
	superseded := make(map[int64]bool)
	for _, r := range allFrags {
		if r.Supersedes > 0 {
			superseded[r.Supersedes] = true
		}
	}
	s.mu.RLock()
	polText := s.policyText
	s.mu.RUnlock()

	frags := make([]store.FragmentRecord, 0, len(allFrags))
	for _, r := range allFrags {
		if r.Forgotten || (excludeSuperseded && superseded[r.Seq]) {
			continue
		}
		if disallowBorg && r.BorgGroup != "" {
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
	anchors, err := s.db.ListAnchors(shard)
	if err != nil {
		return nil, nil, err
	}
	pack := ShardPack{
		Version:    1,
		Shard:      shard,
		OwnerPub:   s.port.IdentityPubHex(),
		ExportedAt: time.Now().Unix(),
		Fragments:  frags,
		Anchors:    anchors,
	}
	raw, err := json.Marshal(pack)
	if err != nil {
		return nil, nil, err
	}
	u, root, hashes, err := bstack.StoreChirpWithHashes(s.objects, raw)
	if err != nil {
		return nil, nil, err
	}
	return &ShardPackResult{
		Shard:    shard,
		ChirpURL: u,
		RootHex:  hex.EncodeToString(root[:]),
		Size:     len(raw),
	}, hashes, nil
}

// ImportShardPack resolves and verifies a CHIRP shard pack closure and imports its fragments into targetShard.
func (s *Service) ImportShardPack(ctx context.Context, chirpURL, targetShard string) (int, error) {
	raw, err := bstack.ResolveChirp(s.objects, chirpURL)
	if err != nil {
		return 0, err
	}
	n, _, err := s.importShardPackVerified(ctx, raw, targetShard)
	return n, err
}

func (s *Service) importShardPackVerified(ctx context.Context, raw []byte, targetShard string) (int, string, error) {
	var pack ShardPack
	if err := json.Unmarshal(raw, &pack); err != nil {
		return 0, "", fmt.Errorf("memory: decode shard pack: %w", err)
	}
	if targetShard == "" {
		targetShard = pack.Shard
	}
	if err := ValidateShardName(targetShard, false); err != nil {
		return 0, "", err
	}
	// Verify every claimed anchor's AnchorBEEF (bounded via guard.ParseBEEF) + Rúnar NektarAnchor state
	// + contiguous ChainRp continuity from genesis + header BUMP settlement (never trusting untrusted JSON flags)
	// and refuse overwriting any conflicting existing local anchor!
	sortedAnchors := append([]store.BatchAnchor(nil), pack.Anchors...)
	for i := 1; i < len(sortedAnchors); i++ {
		for j := i; j > 0 && sortedAnchors[j].N < sortedAnchors[j-1].N; j-- {
			sortedAnchors[j], sortedAnchors[j-1] = sortedAnchors[j-1], sortedAnchors[j]
		}
	}
	verifiedAnchors := make(map[int64]store.BatchAnchor, len(sortedAnchors))
	prevRp := core.Zero32Bytes()
	expectedN := int64(0)
	tracker := s.port.Tracker()
	for _, a := range sortedAnchors {
		if len(a.Beef) == 0 {
			break
		}
		// Enforce contiguous anchor numbering from genesis (no skipped N=0 or gaps bypassing ChainRp!).
		if a.N != expectedN {
			break
		}
		parsedBeef, tx, txHash, tErr := guard.ParseBEEF(a.Beef, guard.DefaultBound)
		if tErr != nil || tx == nil || txHash == nil || len(tx.Outputs) == 0 || tx.Outputs[0].LockingScript == nil {
			break
		}
		st, dErr := contracts.DecodeNektarAnchor(*tx.Outputs[0].LockingScript)
		if dErr != nil {
			break
		}
		if st.N != uint64(a.N) || st.Size != uint64(a.Size) || !strings.EqualFold(hex.EncodeToString(st.R), a.RootHex) ||
			!strings.EqualFold(hex.EncodeToString(st.Rp), a.ChainedHex) || txHash.String() != a.TxID {
			break
		}
		// Enforce sequential ChainRp linkage across every imported anchor.
		wantTag := core.ChainRpWithTag(st.Tag, prevRp, st.R, st.Size)
		wantLegacy := core.EqualBytes(st.Tag, core.TagAnchorTag) && core.EqualBytes(st.Rp, core.ChainRp(prevRp, st.R, st.Size))
		if !core.EqualBytes(wantTag, st.Rp) && !wantLegacy {
			break
		}
		// Derive Settled and BlockHeight strictly from verified BUMP or full carried-ancestry SPV
		// BEFORE advancing prevRp/expectedN or persisting anything. Never trust untrusted JSON flags/heights!
		a.Settled = false
		a.BlockHeight = 0
		if tx.MerklePath != nil {
			root, rErr := tx.MerklePath.ComputeRoot(txHash)
			if rErr != nil || root == nil {
				break
			}
			if s.port.Network() == "memory" {
				// Even in memory mode, ComputeRoot already ran above; check tracker if same chain or mark settled.
				a.Settled = true
				a.BlockHeight = int64(tx.MerklePath.BlockHeight)
			} else {
				if tracker == nil {
					return 0, targetShard, fmt.Errorf("memory: import shard pack requires chain tracker for anchor %d: %w", a.N, chain.ErrTrackerUnavailable)
				}
				valid, vErr := tracker.IsValidRootForHeight(ctx, root, tx.MerklePath.BlockHeight)
				if vErr != nil {
					// Return retryable error on tracker outage instead of silently dropping valid anchors and corrupting ChainTail!
					return 0, targetShard, fmt.Errorf("memory: header tracker unavailable verifying imported anchor %d: %w", a.N, vErr)
				}
				if !valid {
					break
				}
				a.Settled = true
				a.BlockHeight = int64(tx.MerklePath.BlockHeight)
			}
		} else {
			// Unmined pack anchor: MUST pass full spv.Verify of carried input ancestry (reject pathless/unproven anchors!).
			if s.port.Network() != "memory" {
				if tracker == nil {
					return 0, targetShard, fmt.Errorf("memory: import shard pack requires chain tracker for unmined anchor %d: %w", a.N, chain.ErrTrackerUnavailable)
				}
				if sErr := chain.VerifyTransactionSPV(ctx, tx, parsedBeef, tracker); sErr != nil {
					if errors.Is(sErr, chain.ErrTrackerUnavailable) {
						return 0, targetShard, fmt.Errorf("memory: header tracker unavailable verifying unmined anchor %d: %w", a.N, sErr)
					}
					break
				}
			} else if tracker != nil {
				if sErr := chain.VerifyTransactionSPV(ctx, tx, parsedBeef, tracker); sErr != nil {
					break
				}
			}
		}
		a.Shard = targetShard
		if err := s.db.SaveBatchAnchorStrict(a); err != nil {
			return 0, targetShard, fmt.Errorf("memory: import shard pack anchor conflict: %w", err)
		}
		prevRp = st.Rp
		expectedN = a.N + 1
		verifiedAnchors[a.N] = a
	}

	// Recompute every fragment's HDP/Leaf and verify full batch Merkle root against a.RootHex
	// before allowing any imported fragment to retain anchored (BatchN, LeafIndex, AnchorTxID) status!
	batchValid := make(map[int64]bool, len(verifiedAnchors))
	for batchN, a := range verifiedAnchors {
		byIdx := make(map[int][]byte)
		okBatch := a.Size > 0
		for _, f := range pack.Fragments {
			if f.BatchN != batchN {
				continue
			}
			if f.LeafIndex < 0 || f.LeafIndex >= a.Size {
				okBatch = false
				break
			}
			hdpBytes, hErr := hex.DecodeString(strings.TrimSpace(f.HDP))
			if hErr != nil || len(hdpBytes) != 32 {
				okBatch = false
				break
			}
			// Whenever plaintext Data is present for a non-forgotten, non-Borg fragment, Provenance MUST be present
			// and HDP MUST cryptographically commit to Data+Provenance (prevents empty-provenance plaintext forgery!).
			if len(f.Data) > 0 && !f.Forgotten && f.BorgGroup == "" {
				if len(f.Provenance) == 0 {
					okBatch = false
					break
				}
				expectedV1 := core.SHA256(core.Concat(core.TagProv, f.Data, f.Provenance))
				expectedV2 := core.SHA256(core.Concat(core.TagProvV2, core.Field(f.Data), f.Provenance))
				if !core.EqualBytes(expectedV1, hdpBytes) && !core.EqualBytes(expectedV2, hdpBytes) {
					okBatch = false
					break
				}
			}
			leafBytes := core.ComputeLeaf(core.Fragment{HDP: hdpBytes, Ciphertext: f.Ciphertext})
			if !strings.EqualFold(f.Leaf, core.ToHex(leafBytes)) {
				// Backward compatibility for legacy encrypted fragments committed with empty ciphertext.
				legacyLeaf := core.ComputeLeaf(core.Fragment{HDP: hdpBytes})
				if len(f.Ciphertext) == 0 || !strings.EqualFold(f.Leaf, core.ToHex(legacyLeaf)) {
					okBatch = false
					break
				}
				leafBytes = legacyLeaf
			}
			byIdx[f.LeafIndex] = leafBytes
		}
		if okBatch && len(byIdx) == a.Size {
			leaves := make([][]byte, a.Size)
			for i := 0; i < a.Size; i++ {
				lb, ok := byIdx[i]
				if !ok {
					okBatch = false
					break
				}
				leaves[i] = lb
			}
			if okBatch {
				if root, rErr := core.MerkleRoot(leaves); rErr == nil && strings.EqualFold(core.ToHex(root), a.RootHex) {
					batchValid[batchN] = true
				}
			}
		}
	}

	imported := 0
	for _, f := range pack.Fragments {
		f.Shard = targetShard
		f.Tainted = true
		if a, hasAnchor := verifiedAnchors[f.BatchN]; !hasAnchor || !batchValid[f.BatchN] || f.BatchN < 0 || f.AnchorTxID != a.TxID {
			f.BatchN = -1
			f.LeafIndex = -1
			f.AnchorTxID = ""
		}
		if _, dedup, err := s.db.AppendFragment(f); err == nil && !dedup {
			imported++
		}
	}
	return imported, targetShard, nil
}
