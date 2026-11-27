package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/lattice"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

// RecallInput specifies a shard/repo recall query (omit Shard/Repo to search across all non-system repos).
// Optionally pass Branch or AtCommit (commit hash/prefix or tag name) for epistemic time-travel recall,
// or Clearance for strict offline CLKA cryptographic lattice decryption.
type RecallInput struct {
	Shard             string             `json:"shard,omitempty"`
	Repo              string             `json:"repo,omitempty"`     // alias for Shard
	Branch            string             `json:"branch,omitempty"`   // optional branch (defaults to main)
	AtCommit          string             `json:"atCommit,omitempty"` // optional historical commit SHA/prefix or tag for time-travel recall
	Query             string             `json:"query,omitempty"`
	Principal         policy.Principal   `json:"principal,omitempty"`
	Clearance         *lattice.Clearance `json:"clearance,omitempty"`
	CallerPub         string             `json:"callerPub,omitempty"`
	Action            string             `json:"-"` // internal Cedar action ("read" default, or "disclose")
	DisallowBorg      bool               `json:"-"` // internal: when true, Borg-encrypted fragments are skipped
	DisallowPrivate   bool               `json:"-"` // internal: when true, CLKA-encrypted fragments are skipped
	IncludeSuperseded bool               `json:"includeSuperseded,omitempty"`
	VerifySPV         bool               `json:"verifySpv,omitempty"`
	Limit             int                `json:"limit,omitempty"`
}

// RecalledFragment is one authorized, decrypted, optionally SPV-verified fragment.
type RecalledFragment struct {
	Shard          string                  `json:"shard"`
	Seq            int64                   `json:"seq"`
	Path           string                  `json:"path,omitempty"`
	CommitHash     string                  `json:"commitHash,omitempty"`
	TransitionHash string                  `json:"transitionHash,omitempty"`
	Text           string                  `json:"text"`
	HDP            string                  `json:"hdp"`
	Leaf           string                  `json:"leaf"`
	Level          int                     `json:"level"`
	Class          string                  `json:"class"`
	Public         bool                    `json:"public,omitempty"`
	LatticeEpoch   uint64                  `json:"latticeEpoch,omitempty"`
	BorgGroup      string                  `json:"borgGroup,omitempty"`
	BorgEpoch      int64                   `json:"borgEpoch,omitempty"`
	Modality       *core.Modality          `json:"modality,omitempty"`
	Refines        []core.RefinementEdge   `json:"refines,omitempty"`
	AnchorTxID     string                  `json:"anchorTxid,omitempty"`
	AnchorState    string                  `json:"anchorState,omitempty"`
	Tainted        bool                    `json:"tainted,omitempty"`
	Distilled      bool                    `json:"distilled,omitempty"`
	OntologyID     string                  `json:"ontologyId,omitempty"`
	NMU            *nmu.Spec               `json:"nmu,omitempty"`
	GradeMismatch  bool                    `json:"gradeMismatch,omitempty"`
	SPV            *contracts.VerifyResult `json:"spv,omitempty"`
	CreatedAt      int64                   `json:"createdAt"`
}

// openFragmentPlaintext decrypts a Borg- or CLKA-encrypted fragment (or returns public plaintext)
// and cryptographically verifies core.VerifyHDP(plaintext, r.Provenance, hdpBytes). Fails closed on any error.
func (s *Service) openFragmentPlaintext(
	ctx context.Context,
	r store.FragmentRecord,
	principal policy.Principal,
	callerPub string,
	clearance *lattice.Clearance,
	disallowBorg, disallowPrivate bool,
) ([]byte, bool) {
	if r.Forgotten {
		return nil, false
	}
	hdpBytes, hErr := hex.DecodeString(strings.TrimSpace(r.HDP))
	if hErr != nil || len(hdpBytes) != 32 {
		return nil, false
	}
	var plaintext []byte
	switch {
	case r.BorgGroup != "":
		if disallowBorg || callerPub == "" || len(r.KeyedMeta) == 0 {
			return nil, false
		}
		var env bstack.SealedEnvelope
		if err := json.Unmarshal(r.KeyedMeta, &env); err != nil {
			return nil, false
		}
		epochSym, err := s.groups.EpochKey(r.BorgGroup, callerPub, r.BorgEpoch)
		if err != nil {
			return nil, false
		}
		opened, err := bstack.OpenWithEpoch(&env, epochSym)
		core.Zero32Array(&epochSym)
		if err != nil {
			return nil, false
		}
		plaintext = opened
	case r.LatticeEpoch > 0:
		if disallowPrivate || len(r.KeyedMeta) == 0 {
			return nil, false
		}
		var env lattice.SealedEnvelope
		if err := json.Unmarshal(r.KeyedMeta, &env); err != nil {
			return nil, false
		}
		var opened []byte
		var err error
		if clearance != nil {
			var man *lattice.Manifest
			if s.lattice != nil {
				man, _ = s.lattice.BuildManifest(ctx, env.Shard, env.Epoch)
			}
			opened, err = lattice.OpenWithClearance(&env, clearance, man)
		} else if s.lattice != nil {
			opened, err = s.lattice.OpenFragmentForPrincipal(ctx, &env, int(principal.Level))
		} else {
			return nil, false
		}
		if err != nil {
			return nil, false
		}
		plaintext = opened
	default:
		plaintext = r.Data
	}

	if len(r.Provenance) > 0 && !core.VerifyHDP(plaintext, r.Provenance, hdpBytes) {
		return nil, false
	}
	return plaintext, true
}

// OpenBlobAuthorized ephemerally decrypts an authorized CLKA BlobObject (or returns public blob bytes)
// for principal while never persisting plaintext to disk/SQLite/CHIRP. BorgGroup secrets require explicit group access.
func (s *Service) OpenBlobAuthorized(ctx context.Context, b memrepo.BlobObject, principal policy.Principal, disallowBorg bool) ([]byte, bool) {
	if principal.ID == "" {
		principal = ownerPrincipal()
	}
	return s.openBlobPlaintext(ctx, b, principal, "", nil, disallowBorg, false)
}

// openBlobPlaintext decrypts an encrypted BlobObject (BorgGroup or CLKA) ephemerally in memory
// and verifies ContentSHA256. Never writes plaintext to disk/SQLite/CHIRP.
func (s *Service) openBlobPlaintext(
	ctx context.Context,
	b memrepo.BlobObject,
	principal policy.Principal,
	callerPub string,
	clearance *lattice.Clearance,
	disallowBorg, disallowPrivate bool,
) ([]byte, bool) {
	if b.Forgotten {
		return nil, false
	}
	if !b.Encrypted {
		if len(b.Inline) > 0 {
			if b.ContentSHA256 != "" {
				sum := sha256.Sum256(b.Inline)
				if !strings.EqualFold(hex.EncodeToString(sum[:]), b.ContentSHA256) {
					return nil, false
				}
			}
			return b.Inline, true
		}
		if b.ChirpURL != "" && s.objects != nil {
			if resolved, rErr := bstack.ResolveChirp(s.objects, b.ChirpURL); rErr == nil {
				if b.ContentSHA256 != "" {
					sum := sha256.Sum256(resolved)
					if !strings.EqualFold(hex.EncodeToString(sum[:]), b.ContentSHA256) {
						return nil, false
					}
				}
				return resolved, true
			}
		}
		return nil, false
	}
	if len(b.KeyedMeta) == 0 {
		return nil, false
	}
	var opened []byte
	if b.BorgGroup != "" {
		if disallowBorg || callerPub == "" {
			return nil, false
		}
		var env bstack.SealedEnvelope
		if jErr := json.Unmarshal(b.KeyedMeta, &env); jErr != nil {
			return nil, false
		}
		if env.GroupID != b.BorgGroup || env.Epoch != b.BorgEpoch {
			return nil, false
		}
		epochSym, kErr := s.groups.EpochKey(b.BorgGroup, callerPub, b.BorgEpoch)
		if kErr != nil {
			return nil, false
		}
		var oErr error
		opened, oErr = bstack.OpenWithEpoch(&env, epochSym)
		core.Zero32Array(&epochSym)
		if oErr != nil {
			return nil, false
		}
	} else if b.LatticeEpoch > 0 {
		if disallowPrivate {
			return nil, false
		}
		var env lattice.SealedEnvelope
		if jErr := json.Unmarshal(b.KeyedMeta, &env); jErr != nil {
			return nil, false
		}
		if env.Epoch != b.LatticeEpoch {
			return nil, false
		}
		var oErr error
		if clearance != nil {
			var man *lattice.Manifest
			if s.lattice != nil {
				man, _ = s.lattice.BuildManifest(ctx, env.Shard, env.Epoch)
			}
			opened, oErr = lattice.OpenWithClearance(&env, clearance, man)
		} else if s.lattice != nil {
			opened, oErr = s.lattice.OpenFragmentForPrincipal(ctx, &env, int(principal.Level))
		} else {
			return nil, false
		}
		if oErr != nil {
			return nil, false
		}
	} else {
		return nil, false
	}
	if b.ContentSHA256 != "" {
		sum := sha256.Sum256(opened)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), b.ContentSHA256) {
			return nil, false
		}
	}
	return opened, true
}

// Recall returns active, Cedar-authorized, CLKA/Borg-decrypted fragments from shard/repo.
// When AtCommit (or a non-main Branch) is specified, performs epistemic time-travel recall
// against the exact immutable TreeObject at that commit/branch.
func (s *Service) Recall(ctx context.Context, in RecallInput) ([]RecalledFragment, error) {
	s.TouchActivity()
	if in.Shard == "" && strings.TrimSpace(in.Repo) != "" {
		in.Shard = strings.TrimSpace(in.Repo)
	}
	if in.Shard == "" {
		in.Shard = "default"
	}
	if err := ValidateShardName(in.Shard, false); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.AtCommit) != "" || (strings.TrimSpace(in.Branch) != "" && strings.TrimSpace(in.Branch) != "main") {
		return s.recallAtCommit(ctx, in)
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
	superseded := s.CollectSupersededSeqs(in.Shard)

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
		if !in.IncludeSuperseded && superseded[r.Seq] > 0 {
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

		// Cryptographic opening + framed v2 HDP verification (CLKA, BorgGroup, or public).
		plaintext, ok := s.openFragmentPlaintext(ctx, r, principal, callerPub, in.Clearance, in.DisallowBorg, in.DisallowPrivate)
		if !ok {
			continue
		}

		text := string(plaintext)
		if qLower != "" {
			_, ftsHit := ftsRank[r.Seq]
			if !ftsHit && !strings.Contains(strings.ToLower(text), qLower) {
				continue
			}
		}

		var mod *core.Modality
		var refines []core.RefinementEdge
		if len(r.Provenance) > 0 {
			if prov, pErr := core.ParseProvenance(r.Provenance); pErr == nil {
				mod = prov.Modality
				refines = prov.Refines
			}
		}

		item := RecalledFragment{
			Shard:          r.Shard,
			Seq:            r.Seq,
			Path:           r.Path,
			CommitHash:     r.CommitHash,
			TransitionHash: r.TransitionHash,
			Text:           text,
			HDP:            r.HDP,
			Leaf:           r.Leaf,
			Level:          r.Level,
			Class:          r.Class,
			Public:         r.Public,
			LatticeEpoch:   r.LatticeEpoch,
			BorgGroup:      r.BorgGroup,
			BorgEpoch:      r.BorgEpoch,
			Modality:       mod,
			Refines:        refines,
			AnchorTxID:     r.AnchorTxID,
			Tainted:        r.Tainted,
			Distilled:      r.Distilled,
			OntologyID:     r.OntologyID,
			CreatedAt:      r.CreatedAt,
		}
		if nm, nmErr := s.db.GetFragmentNMUMeta(r.Shard, r.Seq); nmErr == nil && nm != nil {
			item.NMU = &nmu.Spec{
				Version:        nmu.SpecVersion,
				Grade:          nmu.Grade(nm.Grade),
				UnitCount:      nm.UnitCount,
				FreshnessClass: nmu.FreshnessClass(nm.FreshnessClass),
				ValidUntilUnix: nm.ValidUntilUnix,
				SubjectVersion: nm.SubjectVersion,
				UsageRights:    nmu.EffectiveRights(nmu.UsageRights(nm.UsageRights)),
				AdvisoryNote:   nmu.AdvisoryNMUNote,
			}
			item.GradeMismatch = nm.GradeMismatch
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
			StateHash:   a.StateHash,
			MmrRoot:     a.MmrRoot,
			AnchorBEEF:  a.Beef,
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
			StateHash:   target.StateHash,
			MmrRoot:     target.MmrRoot,
		},
		ChainTail:  tail,
		AnchorBEEF: target.Beef,
	}
	minDepth := 1
	if s.port != nil && s.port.SettleDepth() > 0 {
		minDepth = s.port.SettleDepth()
	}
	vr, outMemo := contracts.VerifyFragmentWithMemo(ctx, *bundle, s.port.Tracker(), minDepth, memo)
	return bundle, &vr, outMemo, nil
}
