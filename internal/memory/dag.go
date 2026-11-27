package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

// LineageNode describes one fragment node in a cryptographic Knowledge DAG.
type LineageNode struct {
	Shard          string                `json:"shard"`
	Seq            int64                 `json:"seq"`
	HDP            string                `json:"hdp"`
	Leaf           string                `json:"leaf"`
	CommitHash     string                `json:"commitHash,omitempty"`
	TransitionHash string                `json:"transitionHash,omitempty"`
	Class          string                `json:"class"`
	Level          int                   `json:"level"`
	Modality       *core.Modality        `json:"modality,omitempty"`
	CreatedAt      int64                 `json:"createdAt"`
	Forgotten      bool                  `json:"forgotten,omitempty"`
	SupersededBy   int64                 `json:"supersededBy,omitempty"`
	Refines        []core.RefinementEdge `json:"refines,omitempty"`
}

// LineageGraphEdge is one verified directed refinement edge in a LineageReport.
type LineageGraphEdge struct {
	FromShard  string              `json:"fromShard"`
	FromSeq    int64               `json:"fromSeq"`
	ToShard    string              `json:"toShard"`
	ToSeq      int64               `json:"toSeq"`
	Kind       core.RefinementKind `json:"kind"`
	Reason     string              `json:"reason,omitempty"`
	TargetHDP  string              `json:"targetHdp"`
	TargetLeaf string              `json:"targetLeaf,omitempty"`
	Verified   bool                `json:"verified"`
}

// LineageReport returns the verified ancestor & descendant refinement DAG for a fragment.
type LineageReport struct {
	Root        LineageNode        `json:"root"`
	Ancestors   []LineageNode      `json:"ancestors,omitempty"`
	Descendants []LineageNode      `json:"descendants,omitempty"`
	Edges       []LineageGraphEdge `json:"edges,omitempty"`
}

// ResolveAndValidateRefinements normalizes caller-supplied RefinementEdges against persisted fragments,
// fills canonical TargetShard/TargetSeq/TargetHDP/TargetLeaf, rejects missing/forgotten/mismatched targets,
// and verifies the referenced ancestor subgraph is acyclic.
func (s *Service) ResolveAndValidateRefinements(defaultShard string, edges []core.RefinementEdge) ([]core.RefinementEdge, error) {
	if len(edges) == 0 {
		return nil, nil
	}
	if len(edges) > 32 {
		return nil, errors.New("memory: refinement edge count exceeds maximum of 32")
	}
	resolved := make([]core.RefinementEdge, 0, len(edges))
	seen := make(map[string]bool, len(edges))
	for i, e := range edges {
		if strings.TrimSpace(e.TargetShard) == "" {
			e.TargetShard = defaultShard
		}
		if err := ValidateShardName(e.TargetShard, true); err != nil {
			return nil, fmt.Errorf("memory: refinement edge %d invalid targetShard: %w", i, err)
		}
		if e.Kind == "" {
			e.Kind = core.RefinementRefines
		}
		var target store.FragmentRecord
		var err error
		if e.TargetSeq > 0 {
			target, err = s.db.GetFragment(e.TargetShard, e.TargetSeq)
		} else if strings.TrimSpace(e.TargetHDP) != "" {
			target, err = s.db.GetFragmentByHDP(e.TargetShard, strings.ToLower(strings.TrimSpace(e.TargetHDP)))
		} else if strings.TrimSpace(e.TargetLeaf) != "" {
			target, err = s.db.GetFragmentByLeaf(e.TargetShard, strings.ToLower(strings.TrimSpace(e.TargetLeaf)))
		} else {
			return nil, fmt.Errorf("memory: refinement edge %d requires targetSeq, targetHdp, or targetLeaf", i)
		}
		if err != nil {
			return nil, fmt.Errorf("memory: refinement edge %d target not found (%s#%d): %w", i, e.TargetShard, e.TargetSeq, err)
		}
		if target.Forgotten {
			return nil, fmt.Errorf("memory: refinement edge %d cannot reference forgotten fragment %s#%d", i, target.Shard, target.Seq)
		}
		if strings.TrimSpace(e.TargetHDP) != "" && !strings.EqualFold(strings.TrimSpace(e.TargetHDP), target.HDP) {
			return nil, fmt.Errorf("memory: refinement edge %d targetHdp mismatch for %s#%d", i, target.Shard, target.Seq)
		}
		if strings.TrimSpace(e.TargetLeaf) != "" && !strings.EqualFold(strings.TrimSpace(e.TargetLeaf), target.Leaf) {
			return nil, fmt.Errorf("memory: refinement edge %d targetLeaf mismatch for %s#%d", i, target.Shard, target.Seq)
		}
		e.TargetShard = target.Shard
		e.TargetSeq = target.Seq
		e.TargetHDP = target.HDP
		e.TargetLeaf = target.Leaf
		if err := e.Validate(); err != nil {
			return nil, fmt.Errorf("memory: refinement edge %d invalid: %w", i, err)
		}
		dedupKey := fmt.Sprintf("%s#%d|%s", e.TargetShard, e.TargetSeq, e.Kind)
		if seen[dedupKey] {
			continue
		}
		seen[dedupKey] = true

		// Verify existing ancestor graph from target has zero cycles.
		if err := s.checkAncestorAcyclic(target.Shard, target.Seq); err != nil {
			return nil, err
		}
		resolved = append(resolved, e)
	}
	return resolved, nil
}

// ResolveSupersedesEdge turns a legacy Supersedes seq pointer into a cryptographically signed RefinementEdge.
func (s *Service) ResolveSupersedesEdge(shard string, supersedesSeq int64) (*core.RefinementEdge, error) {
	if supersedesSeq <= 0 {
		return nil, nil
	}
	target, err := s.db.GetFragment(shard, supersedesSeq)
	if err != nil {
		return nil, fmt.Errorf("memory: supersedes target %s#%d not found: %w", shard, supersedesSeq, err)
	}
	return &core.RefinementEdge{
		TargetShard: target.Shard,
		TargetSeq:   target.Seq,
		TargetHDP:   target.HDP,
		TargetLeaf:  target.Leaf,
		Kind:        core.RefinementSupersedes,
	}, nil
}

func (s *Service) checkAncestorAcyclic(startShard string, startSeq int64) error {
	const (
		unvisited = 0
		visiting  = 1
		visited   = 2
	)
	color := make(map[string]int)
	var dfs func(sh string, sq int64) error
	dfs = func(sh string, sq int64) error {
		key := fmt.Sprintf("%s#%d", sh, sq)
		if color[key] == visiting {
			return fmt.Errorf("memory: refinement DAG cycle detected at %s", key)
		}
		if color[key] == visited {
			return nil
		}
		color[key] = visiting
		rec, err := s.db.GetFragment(sh, sq)
		if err == nil && len(rec.Provenance) > 0 {
			if prov, pErr := core.ParseProvenance(rec.Provenance); pErr == nil {
				for _, e := range prov.Refines {
					tShard := e.TargetShard
					if tShard == "" {
						tShard = sh
					}
					if e.TargetSeq > 0 {
						if err := dfs(tShard, e.TargetSeq); err != nil {
							return err
						}
					}
				}
			}
		}
		color[key] = visited
		return nil
	}
	return dfs(startShard, startSeq)
}

// CollectSupersededSeqs returns a map targetSeq -> newest superseding seq across both legacy
// Supersedes columns AND cryptographically signed Provenance.Refines edges of kind "supersedes".
func (s *Service) CollectSupersededSeqs(shard string) map[int64]int64 {
	out := make(map[int64]int64)
	records, err := s.db.ListFragments(shard)
	if err != nil {
		return out
	}
	hdpToSeq := make(map[string]int64, len(records))
	for _, r := range records {
		hdpToSeq[strings.ToLower(r.HDP)] = r.Seq
	}
	for _, r := range records {
		if r.Forgotten {
			continue
		}
		if r.Supersedes > 0 && r.Seq > out[r.Supersedes] {
			out[r.Supersedes] = r.Seq
		}
		if len(r.Provenance) > 0 {
			if prov, pErr := core.ParseProvenance(r.Provenance); pErr == nil {
				for _, e := range prov.Refines {
					if e.Kind != core.RefinementSupersedes {
						continue
					}
					tShard := e.TargetShard
					if tShard == "" {
						tShard = r.Shard
					}
					if tShard != shard {
						continue
					}
					tSeq := e.TargetSeq
					if tSeq <= 0 && e.TargetHDP != "" {
						tSeq = hdpToSeq[strings.ToLower(e.TargetHDP)]
					}
					if tSeq > 0 && r.Seq > out[tSeq] {
						out[tSeq] = r.Seq
					}
				}
			}
		}
	}
	return out
}

// IsSupersededCryptographically reports whether (shard, seq) has been superseded via either
// a signed Provenance.Refines supersedes edge or the legacy Supersedes column.
func (s *Service) IsSupersededCryptographically(shard string, seq int64) (int64, bool) {
	sup := s.CollectSupersededSeqs(shard)
	if newer, ok := sup[seq]; ok && newer > 0 {
		return newer, true
	}
	return 0, false
}

// Lineage returns the verified ancestor & descendant refinement DAG for (shard, seq).
func (s *Service) Lineage(ctx context.Context, shard string, seq int64) (*LineageReport, error) {
	return s.LineageAuthorized(ctx, shard, seq, ownerPrincipal())
}

// LineageAuthorized enforces Cedar read authorization and returns the verified cryptographic
// Knowledge DAG rooted at (shard, seq).
func (s *Service) LineageAuthorized(_ context.Context, shard string, seq int64, principal policy.Principal) (*LineageReport, error) {
	isOp := isOperatorPrincipal(principal)
	if err := ValidateShardName(shard, isOp); err != nil {
		return nil, err
	}
	if principal.ID == "" {
		principal = ownerPrincipal()
	}
	rootRec, err := s.db.GetFragment(shard, seq)
	if err != nil {
		return nil, err
	}
	if !isOp {
		dec, aErr := s.AuthorizeAction(principal, "read", policy.Resource{
			Type:  "Fragment",
			ID:    fmt.Sprintf("%s#%d", rootRec.Shard, rootRec.Seq),
			Level: int64(rootRec.Level),
			Shard: rootRec.Shard,
			Class: rootRec.Class,
		})
		if aErr != nil || !dec.Allow {
			return nil, fmt.Errorf("memory: cedar policy denied lineage read on %s#%d", rootRec.Shard, rootRec.Seq)
		}
	}

	supByShard := map[string]map[int64]int64{
		shard: s.CollectSupersededSeqs(shard),
	}
	getSup := func(sh string, sq int64) int64 {
		m, ok := supByShard[sh]
		if !ok {
			m = s.CollectSupersededSeqs(sh)
			supByShard[sh] = m
		}
		return m[sq]
	}

	toNode := func(r store.FragmentRecord) LineageNode {
		node := LineageNode{
			Shard:          r.Shard,
			Seq:            r.Seq,
			HDP:            r.HDP,
			Leaf:           r.Leaf,
			CommitHash:     r.CommitHash,
			TransitionHash: r.TransitionHash,
			Class:          r.Class,
			Level:          r.Level,
			CreatedAt:      r.CreatedAt,
			Forgotten:      r.Forgotten,
			SupersededBy:   getSup(r.Shard, r.Seq),
		}
		if len(r.Provenance) > 0 {
			if prov, pErr := core.ParseProvenance(r.Provenance); pErr == nil {
				node.Modality = prov.Modality
				node.Refines = prov.Refines
			}
		}
		return node
	}

	rootNode := toNode(rootRec)
	rep := &LineageReport{Root: rootNode}

	// 1. Walk ancestors via signed Provenance.Refines edges.
	visitedAnc := map[string]bool{fmt.Sprintf("%s#%d", rootRec.Shard, rootRec.Seq): true}
	var walkAnc func(cur LineageNode)
	walkAnc = func(cur LineageNode) {
		for _, e := range cur.Refines {
			tShard := e.TargetShard
			if tShard == "" {
				tShard = cur.Shard
			}
			var target store.FragmentRecord
			var gErr error
			if e.TargetSeq > 0 {
				target, gErr = s.db.GetFragment(tShard, e.TargetSeq)
			} else if e.TargetHDP != "" {
				target, gErr = s.db.GetFragmentByHDP(tShard, strings.ToLower(e.TargetHDP))
			} else {
				continue
			}
			verified := false
			if gErr == nil {
				hdpOK := e.TargetHDP == "" || strings.EqualFold(e.TargetHDP, target.HDP)
				leafOK := e.TargetLeaf == "" || strings.EqualFold(e.TargetLeaf, target.Leaf)
				verified = hdpOK && leafOK
			}
			rep.Edges = append(rep.Edges, LineageGraphEdge{
				FromShard:  cur.Shard,
				FromSeq:    cur.Seq,
				ToShard:    tShard,
				ToSeq:      target.Seq,
				Kind:       e.Kind,
				Reason:     e.Reason,
				TargetHDP:  e.TargetHDP,
				TargetLeaf: e.TargetLeaf,
				Verified:   verified,
			})
			if gErr != nil {
				continue
			}
			if !isOp {
				dec, aErr := s.AuthorizeAction(principal, "read", policy.Resource{
					Type:  "Fragment",
					ID:    fmt.Sprintf("%s#%d", target.Shard, target.Seq),
					Level: int64(target.Level),
					Shard: target.Shard,
					Class: target.Class,
				})
				if aErr != nil || !dec.Allow {
					continue
				}
			}
			key := fmt.Sprintf("%s#%d", target.Shard, target.Seq)
			if visitedAnc[key] {
				continue
			}
			visitedAnc[key] = true
			tNode := toNode(target)
			rep.Ancestors = append(rep.Ancestors, tNode)
			walkAnc(tNode)
		}
	}
	walkAnc(rootNode)

	// 2. Find descendants in the shard referencing (shard, seq) or rootRec.HDP.
	allFrags, err := s.db.ListFragments(shard)
	if err == nil {
		descSet := map[string]bool{fmt.Sprintf("%s#%d", rootRec.Shard, rootRec.Seq): true}
		// Fixed-point scan to collect transitive descendants within shard.
		changed := true
		for changed {
			changed = false
			for _, f := range allFrags {
				fkey := fmt.Sprintf("%s#%d", f.Shard, f.Seq)
				if descSet[fkey] {
					continue
				}
				if !isOp {
					dec, aErr := s.AuthorizeAction(principal, "read", policy.Resource{
						Type:  "Fragment",
						ID:    fkey,
						Level: int64(f.Level),
						Shard: f.Shard,
						Class: f.Class,
					})
					if aErr != nil || !dec.Allow {
						continue
					}
				}
				node := toNode(f)
				matched := false
				for _, e := range node.Refines {
					tShard := e.TargetShard
					if tShard == "" {
						tShard = f.Shard
					}
					tKey := fmt.Sprintf("%s#%d", tShard, e.TargetSeq)
					if descSet[tKey] || (strings.EqualFold(e.TargetHDP, rootRec.HDP) && e.TargetHDP != "") {
						matched = true
						tRec, tErr := s.db.GetFragment(tShard, e.TargetSeq)
						verified := tErr == nil && (e.TargetHDP == "" || strings.EqualFold(e.TargetHDP, tRec.HDP)) &&
							(e.TargetLeaf == "" || strings.EqualFold(e.TargetLeaf, tRec.Leaf))
						rep.Edges = append(rep.Edges, LineageGraphEdge{
							FromShard:  f.Shard,
							FromSeq:    f.Seq,
							ToShard:    tShard,
							ToSeq:      e.TargetSeq,
							Kind:       e.Kind,
							Reason:     e.Reason,
							TargetHDP:  e.TargetHDP,
							TargetLeaf: e.TargetLeaf,
							Verified:   verified,
						})
					}
				}
				if matched {
					descSet[fkey] = true
					rep.Descendants = append(rep.Descendants, node)
					changed = true
				}
			}
		}
	}

	return rep, nil
}
