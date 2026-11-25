package memory

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

// RecallInput specifies a shard recall query (omit Shard to search across all non-system shards).
type RecallInput struct {
	Shard             string           `json:"shard,omitempty"`
	Query             string           `json:"query,omitempty"`
	Principal         policy.Principal `json:"principal,omitempty"`
	CallerPub         string           `json:"callerPub,omitempty"`
	Action            string           `json:"-"` // internal Cedar action ("read" default, or "disclose")
	DisallowBorg      bool             `json:"-"` // internal: when true, Borg-encrypted fragments are skipped
	IncludeSuperseded bool             `json:"includeSuperseded,omitempty"`
	VerifySPV         bool             `json:"verifySpv,omitempty"`
	Limit             int              `json:"limit,omitempty"`
}

// RecalledFragment is one authorized, decrypted, optionally SPV-verified fragment.
type RecalledFragment struct {
	Shard       string                  `json:"shard"`
	Seq         int64                   `json:"seq"`
	Text        string                  `json:"text"`
	HDP         string                  `json:"hdp"`
	Leaf        string                  `json:"leaf"`
	Level       int                     `json:"level"`
	Class       string                  `json:"class"`
	BorgGroup   string                  `json:"borgGroup,omitempty"`
	BorgEpoch   int64                   `json:"borgEpoch,omitempty"`
	AnchorTxID  string                  `json:"anchorTxid,omitempty"`
	AnchorState string                  `json:"anchorState,omitempty"`
	Tainted     bool                    `json:"tainted,omitempty"`
	SPV         *contracts.VerifyResult `json:"spv,omitempty"`
	CreatedAt   int64                   `json:"createdAt"`
}

// Recall returns active, Cedar-authorized, Borg-decrypted fragments from shard.
func (s *Service) Recall(ctx context.Context, in RecallInput) ([]RecalledFragment, error) {
	if in.Shard == "" {
		in.Shard = "default"
	}
	if err := ValidateShardName(in.Shard, false); err != nil {
		return nil, err
	}
	principal := in.Principal
	if principal.ID == "" {
		principal = policy.Principal{Type: "Agent", ID: "owner", Level: 100}
	}
	action := strings.TrimSpace(in.Action)
	if action == "" {
		action = "read"
	}
	callerPub := in.CallerPub
	if callerPub == "" && !in.DisallowBorg {
		callerPub = s.port.IdentityPubHex()
	}

	records, err := s.db.ListFragments(in.Shard)
	if err != nil {
		return nil, err
	}
	superseded := make(map[int64]bool)
	for _, r := range records {
		if r.Supersedes > 0 {
			superseded[r.Supersedes] = true
		}
	}

	s.mu.RLock()
	polText := s.policyText
	s.mu.RUnlock()

	qLower := strings.ToLower(strings.TrimSpace(in.Query))
	var ftsRank map[int64]int
	if qLower != "" {
		if hits, err := s.db.SearchFTS(in.Shard, in.Query, 200); err == nil && len(hits) > 0 {
			ftsRank = make(map[int64]int, len(hits))
			for idx, h := range hits {
				ftsRank[h.Seq] = idx
			}
		}
	}
	var out []RecalledFragment
	// Load batch anchors once to report honest mined vs broadcast state and memoize per-batch SPV verification.
	anchorSettled := make(map[int64]bool)
	if shardAnchors, aErr := s.db.ListAnchors(in.Shard); aErr == nil {
		for _, a := range shardAnchors {
			anchorSettled[a.N] = a.Settled
		}
	}
	type batchKey struct {
		shard string
		n     int64
	}
	spvMemos := make(map[batchKey]*contracts.AnchorBatchMemo)

	for _, r := range records {
		if r.Forgotten {
			continue
		}
		if !in.IncludeSuperseded && superseded[r.Seq] {
			continue
		}
		// Cedar authorization gate (native cedar-go).
		dec, err := policy.Authorize(polText, principal, action, policy.Resource{
			Type:  "Fragment",
			ID:    fmt.Sprintf("%s#%d", r.Shard, r.Seq),
			Level: int64(r.Level),
			Shard: r.Shard,
			Class: r.Class,
		})
		if err != nil || !dec.Allow {
			continue
		}

		// Decrypt BRC-369 Borg-sealed fragment if encrypted.
		plaintext := r.Data
		if r.BorgGroup != "" {
			if in.DisallowBorg || callerPub == "" || len(r.KeyedMeta) == 0 {
				continue
			}
			var env bstack.SealedEnvelope
			if err := json.Unmarshal(r.KeyedMeta, &env); err != nil {
				continue
			}
			epochSym, err := s.groups.EpochKey(r.BorgGroup, callerPub, r.BorgEpoch)
			if err != nil {
				// Revoked or non-member: fail closed!
				continue
			}
			opened, err := bstack.OpenWithEpoch(&env, epochSym)
			core.Zero32Array(&epochSym)
			if err != nil {
				continue
			}
			// Re-verify decrypted plaintext cryptographically matches committed HDP.
			if hdpBytes, hErr := hex.DecodeString(r.HDP); hErr == nil && len(hdpBytes) == 32 && len(r.Provenance) > 0 {
				v1 := core.SHA256(core.Concat(core.TagProv, opened, r.Provenance))
				v2 := core.SHA256(core.Concat(core.TagProvV2, core.Field(opened), r.Provenance))
				if !core.EqualBytes(v1, hdpBytes) && !core.EqualBytes(v2, hdpBytes) {
					continue
				}
			}
			plaintext = opened
		}

		text := string(plaintext)
		if qLower != "" {
			_, ftsHit := ftsRank[r.Seq]
			if !ftsHit && !strings.Contains(strings.ToLower(text), qLower) {
				continue
			}
		}

		item := RecalledFragment{
			Shard:      r.Shard,
			Seq:        r.Seq,
			Text:       text,
			HDP:        r.HDP,
			Leaf:       r.Leaf,
			Level:      r.Level,
			Class:      r.Class,
			BorgGroup:  r.BorgGroup,
			BorgEpoch:  r.BorgEpoch,
			AnchorTxID: r.AnchorTxID,
			Tainted:    r.Tainted,
			CreatedAt:  r.CreatedAt,
		}
		if r.BatchN < 0 {
			item.AnchorState = "unanchored"
		} else if anchorSettled[r.BatchN] {
			item.AnchorState = "mined"
		} else {
			item.AnchorState = "broadcast"
		}
		if in.VerifySPV && r.BatchN >= 0 {
			bk := batchKey{shard: r.Shard, n: r.BatchN}
			_, vr, nextMemo, pErr := s.proveWithMemo(ctx, r.Shard, r.Seq, policy.Principal{Type: "Agent", ID: "owner", Level: 100}, spvMemos[bk])
			if nextMemo != nil {
				spvMemos[bk] = nextMemo
			}
			if pErr == nil && vr != nil {
				item.SPV = vr
				if vr.AnchorState != "" {
					item.AnchorState = vr.AnchorState
				}
			}
		}
		out = append(out, item)
		if in.Limit > 0 && len(out) >= in.Limit {
			break
		}
	}
	return out, nil
}

// Prove constructs a self-contained ProofBundle for (shard, seq) and runs full offline SPV verification.
func (s *Service) Prove(ctx context.Context, shard string, seq int64) (*contracts.ProofBundle, *contracts.VerifyResult, error) {
	return s.ProveAuthorized(ctx, shard, seq, policy.Principal{Type: "Agent", ID: "owner", Level: 100})
}

// ProveAuthorized enforces Cedar read authorization before constructing a ProofBundle for (shard, seq).
func (s *Service) ProveAuthorized(ctx context.Context, shard string, seq int64, principal policy.Principal) (*contracts.ProofBundle, *contracts.VerifyResult, error) {
	bundle, vr, _, err := s.proveWithMemo(ctx, shard, seq, principal, nil)
	return bundle, vr, err
}

func (s *Service) proveWithMemo(ctx context.Context, shard string, seq int64, principal policy.Principal, memo *contracts.AnchorBatchMemo) (*contracts.ProofBundle, *contracts.VerifyResult, *contracts.AnchorBatchMemo, error) {
	if err := ValidateShardName(shard, true); err != nil {
		return nil, nil, memo, err
	}
	rec, err := s.db.GetFragment(shard, seq)
	if err != nil {
		return nil, nil, memo, err
	}
	if principal.ID == "" {
		principal = policy.Principal{Type: "Agent", ID: "owner", Level: 100}
	}
	if dec, aErr := s.AuthorizeAction(principal, "read", policy.Resource{
		Type:  "Fragment",
		ID:    fmt.Sprintf("%s#%d", rec.Shard, rec.Seq),
		Level: int64(rec.Level),
		Shard: rec.Shard,
		Class: rec.Class,
	}); aErr != nil || !dec.Allow {
		if aErr != nil {
			return nil, nil, memo, fmt.Errorf("memory: cedar prove policy error: %w", aErr)
		}
		return nil, nil, memo, fmt.Errorf("memory: cedar policy denied prove on %s#%d", rec.Shard, rec.Seq)
	}
	if rec.BatchN < 0 {
		return nil, &contracts.VerifyResult{OK: false, AnchorState: "unanchored", Reason: "fragment not yet anchored"}, memo, nil
	}
	batchFrags, err := s.db.ListBatchFragments(shard, rec.BatchN)
	if err != nil {
		return nil, nil, memo, err
	}
	leaves := make([][]byte, len(batchFrags))
	for i, f := range batchFrags {
		lb, err := hex.DecodeString(f.Leaf)
		if err != nil {
			return nil, nil, memo, err
		}
		leaves[i] = lb
	}
	mp, err := core.ComputeMerkleProof(leaves, rec.LeafIndex)
	if err != nil {
		return nil, nil, memo, err
	}
	pathHex := make([]string, len(mp.Siblings))
	for i, p := range mp.Siblings {
		pathHex[i] = core.ToHex(p)
	}
	anchors, err := s.db.ListAnchors(shard)
	if err != nil {
		return nil, nil, memo, err
	}
	var target *store.BatchAnchor
	var tail []contracts.AnchorLink
	for i := range anchors {
		a := anchors[i]
		link := contracts.AnchorLink{
			TxID:        a.TxID,
			OutputIndex: 0,
			N:           uint64(a.N),
			Size:        uint64(a.Size),
			R:           a.RootHex,
			Rp:          a.ChainedHex,
		}
		if a.N <= rec.BatchN {
			tail = append(tail, link)
		}
		if a.N == rec.BatchN {
			cp := a
			target = &cp
		}
	}
	if target == nil {
		return nil, nil, memo, fmt.Errorf("memory: missing anchor batch %d for shard %q", rec.BatchN, shard)
	}
	bundle := &contracts.ProofBundle{
		Version:   1,
		Shard:     target.ShardID,
		N:         uint64(target.N),
		Size:      uint64(target.Size),
		Leaf:      rec.Leaf,
		LeafIndex: rec.LeafIndex,
		Path:      pathHex,
		Anchor: contracts.AnchorLink{
			TxID:        target.TxID,
			OutputIndex: 0,
			N:           uint64(target.N),
			Size:        uint64(target.Size),
			R:           target.RootHex,
			Rp:          target.ChainedHex,
		},
		ChainTail:  tail,
		AnchorBEEF: target.Beef,
	}
	vr, outMemo := contracts.VerifyFragmentWithMemo(ctx, *bundle, s.port.Tracker(), 1, memo)
	return bundle, &vr, outMemo, nil
}
