package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kryptokrates/nektar/internal/jevgraph"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

// OntologyToolInput is the unified input schema for the `ontology` tool across MCP, REST /v1/tools/ontology, and A2A.
type OntologyToolInput struct {
	Action     string `json:"action"` // init|list|show|validate|extract|edges|review|align
	Repo       string `json:"repo,omitempty"`
	Ontology   string `json:"ontology,omitempty"`
	OntologyID string `json:"ontologyId,omitempty"`
	YAML       string `json:"yaml,omitempty"`
	Text       string `json:"text,omitempty"`
	Shard      string `json:"shard,omitempty"`
	Seq        int64  `json:"seq,omitempty"`
	TripleHash string `json:"tripleHash,omitempty"`
	Accept     *bool  `json:"accept,omitempty"`
	Status     string `json:"status,omitempty"`
	Subject    string `json:"subject,omitempty"`
	Predicate  string `json:"predicate,omitempty"`
	Object     string `json:"object,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	Source     string `json:"source,omitempty"`
	Target     string `json:"target,omitempty"`
	BridgeRepo string `json:"bridgeRepo,omitempty"`
	Strict     bool   `json:"strict,omitempty"`
	AutoAnchor bool   `json:"autoAnchor,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// ExecOntology executes domain TBox ontology, Bridge Shard, JevGraph extraction, and edge review operations.
func (tb *Toolbox) ExecOntology(ctx context.Context, in OntologyToolInput) (any, error) {
	cc := CallerContextFor(ctx, policy.Principal{})
	action := strings.ToLower(strings.TrimSpace(in.Action))
	if action == "" {
		action = "list"
	}

	resolveOntKey := func() string {
		if v := strings.TrimSpace(in.Ontology); v != "" {
			return v
		}
		if v := strings.TrimSpace(in.OntologyID); v != "" {
			return v
		}
		return strings.TrimSpace(in.Repo)
	}

	switch action {
	case "init":
		if cc.FromHTTP && !cc.IsOperator && in.AutoAnchor {
			return nil, errors.New("operator token required for autoAnchor")
		}
		raw := strings.TrimSpace(in.YAML)
		if raw == "" {
			raw = strings.TrimSpace(in.Text)
		}
		if raw == "" {
			return nil, errors.New("ontology init requires yaml (or text) ontology definition")
		}
		info, compiled, err := tb.mem.CommitDomainOntology(ctx, in.Repo, []byte(raw), in.AutoAnchor)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"ontology":    info,
			"mangleRules": compiled.MangleRules,
		}, nil

	case "list":
		onts, err := tb.mem.ListDomainOntologies(ctx)
		if err != nil {
			return nil, err
		}
		bridges, err := tb.mem.ListBridges(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"ontologies": onts,
			"bridges":    bridges,
			"count":      len(onts),
		}, nil

	case "show":
		key := resolveOntKey()
		if key == "" {
			return nil, errors.New("ontology show requires ontology, ontologyId, or repo")
		}
		compiled, info, err := tb.mem.LoadDomainOntology(ctx, key)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"info":        info,
			"ontology":    compiled.Ontology,
			"mangleRules": compiled.MangleRules,
		}, nil

	case "validate":
		raw := strings.TrimSpace(in.YAML)
		if raw == "" {
			raw = strings.TrimSpace(in.Text)
		}
		if raw == "" {
			return nil, errors.New("ontology validate requires yaml (or text) ontology definition")
		}
		ont, err := jevgraph.ParseOntologyAuto([]byte(raw))
		if err != nil {
			return nil, err
		}
		compiled, err := jevgraph.Compile(ont)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"ok":            true,
			"ontologyId":    compiled.ID,
			"name":          compiled.Ontology.Name,
			"namespace":     compiled.Ontology.Namespace,
			"classCount":    len(compiled.Ontology.Classes),
			"relationCount": len(compiled.Ontology.Relations),
			"entityCount":   len(compiled.Ontology.Entities),
			"mangleRules":   compiled.MangleRules,
		}, nil

	case "extract":
		key := resolveOntKey()
		if key == "" {
			return nil, errors.New("ontology extract requires ontology, ontologyId, or repo")
		}
		if strings.TrimSpace(in.Text) == "" {
			return nil, errors.New("ontology extract requires text")
		}
		return tb.mem.ExtractWithOntology(ctx, key, in.Text, in.Shard, jevgraph.ExtractOptions{
			Strict: in.Strict,
		})

	case "edges":
		ontID := strings.TrimSpace(in.OntologyID)
		if ontID == "" && strings.TrimSpace(in.Ontology) != "" {
			if comp, _, err := tb.mem.LoadDomainOntology(ctx, in.Ontology); err == nil && comp != nil {
				ontID = comp.ID
			} else {
				ontID = strings.TrimSpace(in.Ontology)
			}
		}
		rows, err := tb.mem.ListKnowledgeEdgesAuthorized(ctx, store.KnowledgeEdgeFilter{
			Shard:      in.Shard,
			Seq:        in.Seq,
			OntologyID: ontID,
			Subject:    in.Subject,
			Predicate:  in.Predicate,
			Object:     in.Object,
			Status:     in.Status,
			Limit:      in.Limit,
		}, cc.Principal)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"edges": rows,
			"count": len(rows),
		}, nil

	case "review":
		if cc.FromHTTP && !cc.IsOperator {
			return nil, errors.New("operator token required for ontology review")
		}
		if in.Accept == nil {
			return nil, errors.New("ontology review requires accept (true|false)")
		}
		if strings.TrimSpace(in.Shard) == "" || in.Seq <= 0 || strings.TrimSpace(in.TripleHash) == "" {
			return nil, errors.New("ontology review requires shard, seq, and tripleHash")
		}
		return tb.mem.ReviewKnowledgeEdge(ctx, in.Shard, in.Seq, in.TripleHash, *in.Accept, in.Reason)

	case "align":
		if cc.FromHTTP && !cc.IsOperator && in.AutoAnchor {
			return nil, errors.New("operator token required for autoAnchor")
		}
		if strings.TrimSpace(in.Source) == "" || strings.TrimSpace(in.Target) == "" {
			return nil, errors.New("ontology align requires source and target ontology names or IDs")
		}
		info, manifest, err := tb.mem.AlignAndCommitBridge(ctx, in.Source, in.Target, in.BridgeRepo, in.AutoAnchor)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"bridge":   info,
			"manifest": manifest,
		}, nil

	default:
		return nil, fmt.Errorf("unknown ontology action %q (use init|list|show|validate|extract|edges|review|align)", in.Action)
	}
}
