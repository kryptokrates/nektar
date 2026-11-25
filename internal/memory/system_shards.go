package memory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/store"
)

// CommitSystemAuditRecord commits a signed canonical JSON audit record into a reserved system
// shard (__deals__, __policies__, or __receipts__), preserving the per-fragment prev_leaf/chain_hash
// ledger chain, and optionally anchors it immediately via NektarAnchor.Append() singleton lineage.
func (s *Service) CommitSystemAuditRecord(
	ctx context.Context,
	shard string,
	class string,
	payload any,
	autoAnchor bool,
) (*store.FragmentRecord, *store.BatchAnchor, error) {
	if s == nil || s.db == nil {
		return nil, nil, errors.New("memory: nil service or store")
	}
	if !IsSystemShard(shard) {
		return nil, nil, fmt.Errorf("memory: %q is not a reserved system audit shard", shard)
	}
	if class == "" {
		class = "audit"
	}
	data, err := core.CanonicalJSON(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("memory: canonicalize system audit payload: %w", err)
	}

	now := time.Now().Unix()
	prov := core.Provenance{
		Source:  "system:" + shard + ":" + class,
		Time:    now,
		ModelID: s.modelID.Provider + "/" + s.modelID.Release,
	}
	if s.port != nil {
		if sigBytes, sErr := s.port.SignDigest(ctx, core.ProvenanceDigest(data, prov)); sErr == nil {
			prov.Signature = sigBytes
		}
	}
	provBytes := core.SerializeProvenance(prov)
	hdp := core.ComputeHDPV2(data, prov)
	leaf := core.ComputeLeaf(core.Fragment{
		Data:       data,
		Provenance: prov,
		HDP:        hdp,
	})
	rec := store.FragmentRecord{
		Shard:      shard,
		HDP:        core.ToHex(hdp),
		Leaf:       core.ToHex(leaf),
		Data:       data,
		Provenance: provBytes,
		Class:      class,
		BatchN:     -1,
		LeafIndex:  -1,
		CreatedAt:  now,
	}
	stored, _, err := s.db.AppendFragment(rec)
	if err != nil {
		return nil, nil, fmt.Errorf("memory: append %s audit fragment: %w", shard, err)
	}
	var batch *store.BatchAnchor
	if autoAnchor && s.port != nil {
		batch, err = s.AnchorShard(ctx, shard)
		if err != nil {
			return &stored, nil, err
		}
	}
	return &stored, batch, nil
}
