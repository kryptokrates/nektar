package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/kryptokrates/nektar/internal/jevgraph"
	"github.com/kryptokrates/nektar/internal/store"
)

func cmdOntology(args []string) error {
	if len(args) == 0 {
		printOntologyUsage()
		return nil
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	rest := args[1:]
	switch sub {
	case "init":
		return cmdOntologyInit(rest)
	case "list", "ls":
		return cmdOntologyList(rest)
	case "show":
		return cmdOntologyShow(rest)
	case "validate":
		return cmdOntologyValidate(rest)
	case "extract":
		return cmdOntologyExtract(rest)
	case "edges":
		return cmdOntologyEdges(rest)
	case "review":
		return cmdOntologyReview(rest)
	case "align":
		return cmdOntologyAlign(rest)
	case "-h", "--help", "help":
		printOntologyUsage()
		return nil
	default:
		printOntologyUsage()
		return fmt.Errorf("unknown ontology subcommand %q", sub)
	}
}

func printOntologyUsage() {
	fmt.Println(`nektar ontology — Manage committed domain TBox ontologies, Bridge Shards & JevGraph extraction

Subcommands:
  nektar ontology init <repo> -file <ontology.yaml> [-anchor]
  nektar ontology list
  nektar ontology show <name-or-ontologyId>
  nektar ontology validate -file <ontology.yaml>
  nektar ontology extract <name-or-ontologyId> -text "..." [-shard <shard>] [-strict]
  nektar ontology edges [-shard <shard>] [-ontology <name-or-id>] [-status accepted|review|rejected] [-subject ...] [-predicate ...] [-object ...]
  nektar ontology review <shard> <seq> <tripleHash> (-accept | -reject) [-reason "..."]
  nektar ontology align <source-ont> <target-ont> [-repo bridge-...] [-anchor]`)
}

func cmdOntologyInit(args []string) error {
	fs := flag.NewFlagSet("ontology init", flag.ExitOnError)
	filePath := fs.String("file", "", "Path to YAML/JSON ontology TBox definition")
	anchor := fs.Bool("anchor", false, "Anchor immediately on BSV via NektarAnchor")
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	pos, err := parseRepoArgs(fs, args)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*filePath) == "" {
		return errors.New("usage: nektar ontology init [ont-<domain>] -file <ontology.yaml> [-anchor]")
	}
	raw, err := os.ReadFile(strings.TrimSpace(*filePath))
	if err != nil {
		return err
	}
	repo := ""
	if len(pos) > 0 {
		repo = pos[0]
	}

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveTool(info, "ontology", map[string]any{
			"action":     "init",
			"repo":       repo,
			"yaml":       string(raw),
			"autoAnchor": *anchor,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}

	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	info, compiled, err := rt.mem.CommitDomainOntology(ctx, repo, raw, *anchor)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"ontology": info, "mangleRules": compiled.MangleRules})
}

func cmdOntologyList(args []string) error {
	fs := flag.NewFlagSet("ontology list", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	_ = fs.Parse(args)

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveTool(info, "ontology", map[string]any{"action": "list"})
		if err != nil {
			return err
		}
		return printJSON(out)
	}

	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	onts, err := rt.mem.ListDomainOntologies(ctx)
	if err != nil {
		return err
	}
	bridges, err := rt.mem.ListBridges(ctx)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"ontologies": onts, "bridges": bridges, "count": len(onts)})
}

func cmdOntologyShow(args []string) error {
	fs := flag.NewFlagSet("ontology show", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	pos, err := parseRepoArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("usage: nektar ontology show <name-or-ontologyId>")
	}
	key := pos[0]

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveTool(info, "ontology", map[string]any{"action": "show", "ontology": key})
		if err != nil {
			return err
		}
		return printJSON(out)
	}

	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	comp, info, err := rt.mem.LoadDomainOntology(ctx, key)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"info": info, "ontology": comp.Ontology, "mangleRules": comp.MangleRules})
}

func cmdOntologyValidate(args []string) error {
	fs := flag.NewFlagSet("ontology validate", flag.ExitOnError)
	filePath := fs.String("file", "", "Path to YAML/JSON ontology TBox definition")
	pos, err := parseRepoArgs(fs, args)
	if err != nil {
		return err
	}
	p := strings.TrimSpace(*filePath)
	if p == "" && len(pos) > 0 {
		p = pos[0]
	}
	if p == "" {
		return errors.New("usage: nektar ontology validate -file <ontology.yaml>")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	ont, err := jevgraph.ParseOntologyAuto(raw)
	if err != nil {
		return err
	}
	comp, err := jevgraph.Compile(ont)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{
		"ok":            true,
		"ontologyId":    comp.ID,
		"name":          comp.Ontology.Name,
		"namespace":     comp.Ontology.Namespace,
		"classCount":    len(comp.Ontology.Classes),
		"relationCount": len(comp.Ontology.Relations),
		"entityCount":   len(comp.Ontology.Entities),
		"mangleRules":   comp.MangleRules,
	})
}

func cmdOntologyExtract(args []string) error {
	fs := flag.NewFlagSet("ontology extract", flag.ExitOnError)
	text := fs.String("text", "", "Raw text to extract typed knowledge graph edges from")
	shard := fs.String("shard", "", "Optional shard name for Mangle conflict checking against existing accepted triples")
	strict := fs.Bool("strict", false, "Fail closed if self-hosted Jev System One evaluator is offline")
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	pos, err := parseRepoArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 || strings.TrimSpace(*text) == "" {
		return errors.New("usage: nektar ontology extract <name-or-ontologyId> -text \"...\" [-shard <shard>] [-strict]")
	}
	key := pos[0]

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveTool(info, "ontology", map[string]any{
			"action":   "extract",
			"ontology": key,
			"text":     *text,
			"shard":    *shard,
			"strict":   *strict,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}

	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	res, err := rt.mem.ExtractWithOntology(ctx, key, *text, *shard, jevgraph.ExtractOptions{Strict: *strict})
	if err != nil {
		return err
	}
	return printJSON(res)
}

func cmdOntologyEdges(args []string) error {
	fs := flag.NewFlagSet("ontology edges", flag.ExitOnError)
	shard := fs.String("shard", "", "Filter by shard/repo")
	seq := fs.Int64("seq", 0, "Filter by fragment seq")
	ont := fs.String("ontology", "", "Filter by ontology name or ID")
	status := fs.String("status", "", "Filter by status (accepted|review|rejected)")
	subject := fs.String("subject", "", "Filter by subject entity ID")
	predicate := fs.String("predicate", "", "Filter by relation predicate ID")
	object := fs.String("object", "", "Filter by object entity ID")
	limit := fs.Int("limit", 100, "Max edges to return")
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	_ = fs.Parse(args)

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveTool(info, "ontology", map[string]any{
			"action":    "edges",
			"shard":     *shard,
			"seq":       *seq,
			"ontology":  *ont,
			"status":    *status,
			"subject":   *subject,
			"predicate": *predicate,
			"object":    *object,
			"limit":     *limit,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}

	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()

	ontID := strings.TrimSpace(*ont)
	if ontID != "" {
		if comp, _, lErr := rt.mem.LoadDomainOntology(ctx, ontID); lErr == nil && comp != nil {
			ontID = comp.ID
		}
	}
	rows, err := rt.mem.DB().ListKnowledgeEdges(store.KnowledgeEdgeFilter{
		Shard:      *shard,
		Seq:        *seq,
		OntologyID: ontID,
		Subject:    *subject,
		Predicate:  *predicate,
		Object:     *object,
		Status:     *status,
		Limit:      *limit,
	})
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"edges": rows, "count": len(rows)})
}

func cmdOntologyReview(args []string) error {
	fs := flag.NewFlagSet("ontology review", flag.ExitOnError)
	accept := fs.Bool("accept", false, "Promote edge to accepted (re-validated via Mangle gate)")
	reject := fs.Bool("reject", false, "Mark edge as rejected")
	reason := fs.String("reason", "", "Optional review note")
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	pos, err := parseRepoArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 3 || (*accept == *reject) {
		return errors.New("usage: nektar ontology review <shard> <seq> <tripleHash> (-accept | -reject) [-reason \"...\"]")
	}
	shard := pos[0]
	seq, err := strconv.ParseInt(pos[1], 10, 64)
	if err != nil || seq <= 0 {
		return fmt.Errorf("invalid seq %q", pos[1])
	}
	tripleHash := pos[2]
	acc := *accept

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveTool(info, "ontology", map[string]any{
			"action":     "review",
			"shard":      shard,
			"seq":        seq,
			"tripleHash": tripleHash,
			"accept":     acc,
			"reason":     *reason,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}

	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	row, err := rt.mem.ReviewKnowledgeEdge(ctx, shard, seq, tripleHash, acc, *reason)
	if err != nil {
		return err
	}
	return printJSON(row)
}

func cmdOntologyAlign(args []string) error {
	fs := flag.NewFlagSet("ontology align", flag.ExitOnError)
	bridgeRepo := fs.String("repo", "", "Optional target bridge repo name (default bridge-<src8>-<dst8>)")
	anchor := fs.Bool("anchor", false, "Anchor bridge shard immediately on BSV")
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	pos, err := parseRepoArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return errors.New("usage: nektar ontology align <source-ont> <target-ont> [-repo bridge-...] [-anchor]")
	}
	src, dst := pos[0], pos[1]

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveTool(info, "ontology", map[string]any{
			"action":     "align",
			"source":     src,
			"target":     dst,
			"bridgeRepo": *bridgeRepo,
			"autoAnchor": *anchor,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}

	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	info, manifest, err := rt.mem.AlignAndCommitBridge(ctx, src, dst, *bridgeRepo, *anchor)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"bridge": info, "manifest": manifest})
}
