package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/memrepo"
)

func cmdRepo(args []string) error {
	if len(args) == 0 {
		printRepoUsage()
		return nil
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "list", "ls":
		return cmdRepoList(rest)
	case "log":
		return cmdRepoLog(rest)
	case "tree":
		return cmdRepoTree(rest)
	case "diff":
		return cmdRepoDiff(rest)
	case "commit":
		return cmdRepoCommit(rest)
	case "branch":
		return cmdRepoBranch(rest)
	case "merge":
		return cmdRepoMerge(rest)
	case "export":
		return cmdRepoExport(rest)
	case "import":
		return cmdRepoImport(rest)
	case "verify":
		return cmdRepoVerify(rest)
	case "checkout":
		return cmdRepoCheckout(rest)
	default:
		printRepoUsage()
		return fmt.Errorf("unknown repo subcommand %q", sub)
	}
}

func printRepoUsage() {
	fmt.Println(`nektar repo — Git-compatible, BSV UTXO-anchored Memory Repositories & .nkt Capsules

Usage:
  nektar repo list                                List all Memory Repos with UTXO head & branch status
  nektar repo log <repo> [-branch main]           Show signed commit DAG & BSV NektarAnchor UTXOs
  nektar repo tree <repo> [-branch main]          List files/facts and volumetric CHIRP refs in tree
  nektar repo diff <repo> [-from H] [-to H]       Show Git-style file diff between commits
  nektar repo commit <repo> -text ... | -file ... Commit text or binary file (CHIRP) with L1 MPE binding
  nektar repo branch <repo> <name> [-from main]   Create a new branch with derived BRC-43 genesis key
  nektar repo merge <repo> <src> [-into main]     Create a 2-parent merge commit uniting two branches
  nektar repo export <repo> [-out repo.nkt]       Export a self-verifying .nkt CHIRP Memory Capsule
  nektar repo import <file.nkt | chirp://...>     Verify SPV proofs and fork/merge a .nkt Capsule
  nektar repo verify <file.nkt>                   Verify a .nkt Capsule offline (commits, MPE, anchors)
  nektar repo checkout <repo> [-dir ./out]        Materialize a real .git workspace (MemFS) on disk`)
}

// parseRepoArgs reorders flags before positional arguments so callers can pass either
// `nektar repo log --network main myrepo` or `nektar repo log myrepo --network main`.
func parseRepoArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var flagArgs, posArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			posArgs = append(posArgs, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flagArgs = append(flagArgs, a)
			raw := strings.TrimLeft(a, "-")
			if raw == "h" || raw == "help" || strings.ContainsRune(raw, '=') {
				continue
			}
			f := fs.Lookup(raw)
			isBool := false
			if f != nil {
				if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
					isBool = true
				}
			}
			if !isBool && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		posArgs = append(posArgs, a)
	}
	if err := fs.Parse(flagArgs); err != nil {
		return nil, err
	}
	posArgs = append(posArgs, fs.Args()...)
	return posArgs, nil
}

// resolveRepoDataDir resolves dataDir AFTER flag parsing so `--network main` never accidentally opens ~/.nektar (testnet).
func resolveRepoDataDir(dataDirFlag, netFlag string) string {
	if d := strings.TrimSpace(dataDirFlag); d != "" {
		return d
	}
	net := strings.TrimSpace(netFlag)
	if net == "" {
		net = envOr("NEKTAR_NETWORK", "testnet")
	}
	return defaultDataDirForNetwork(net)
}

func cmdRepoList(args []string) error {
	fs := flag.NewFlagSet("repo list", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "data directory override")
	netName := fs.String("network", "", "network override (testnet|main|memory)")
	jsonOut := fs.Bool("json", false, "output raw JSON")
	if _, err := parseRepoArgs(fs, args); err != nil {
		return err
	}

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveTool(info, "repo", map[string]any{"action": "list"})
		if err != nil {
			return err
		}
		if *jsonOut {
			return printRepoJSON(out)
		}
		return printLiveRepoList(out)
	}

	dir := resolveRepoDataDir(*dataDir, *netName)
	ctx := context.Background()
	mem, _, cleanup, err := openNode(ctx, *netName, dir, false)
	if err != nil {
		return err
	}
	defer cleanup()

	repos, err := mem.ListRepos(ctx)
	if err != nil {
		return err
	}
	if *jsonOut {
		return printRepoJSON(repos)
	}
	if len(repos) == 0 {
		fmt.Println("No Memory Repos yet. Run `nektar repo commit my-repo -text \"...\"` to create one.")
		return nil
	}
	for _, r := range repos {
		utxo := "unanchored"
		if r.HeadAnchorTxID != "" {
			utxo = fmt.Sprintf("UTXO %s:0 (n=%d, %d/%d settled)", shortRepoStr(r.HeadAnchorTxID, 12), r.HeadAnchorN, r.SettledAnchors, r.AnchorCount)
		}
		fmt.Printf("%-18s branches=%v  commits=%d  files=%d  genesis=%s  %s\n",
			r.Repo, r.Branches, r.CommitCount, r.FileCount, shortRepoStr(r.GenesisShardID, 12), utxo)
	}
	return nil
}

func printLiveRepoList(raw any) error {
	b, _ := json.Marshal(raw)
	var wrapped struct {
		Repos []memory.RepoSummary `json:"repos"`
	}
	if json.Unmarshal(b, &wrapped) != nil {
		return printRepoJSON(raw)
	}
	if len(wrapped.Repos) == 0 {
		fmt.Println("No Memory Repos yet. Run `nektar repo commit my-repo -text \"...\"` to create one.")
		return nil
	}
	for _, r := range wrapped.Repos {
		utxo := "unanchored"
		if r.HeadAnchorTxID != "" {
			utxo = fmt.Sprintf("UTXO %s:0 (n=%d, %d/%d settled)", shortRepoStr(r.HeadAnchorTxID, 12), r.HeadAnchorN, r.SettledAnchors, r.AnchorCount)
		}
		fmt.Printf("%-18s branches=%v  commits=%d  files=%d  genesis=%s  %s\n",
			r.Repo, r.Branches, r.CommitCount, r.FileCount, shortRepoStr(r.GenesisShardID, 12), utxo)
	}
	return nil
}

func cmdRepoLog(args []string) error {
	fs := flag.NewFlagSet("repo log", flag.ExitOnError)
	branch := fs.String("branch", "", "filter by branch")
	limit := fs.Int("limit", 25, "max commits")
	dataDir := fs.String("data-dir", "", "data directory override")
	netName := fs.String("network", "", "network override")
	jsonOut := fs.Bool("json", false, "output raw JSON")
	pos, err := parseRepoArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("usage: nektar repo log <repo> [-branch main] [-limit 20]")
	}
	repo := pos[0]

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveTool(info, "repo", map[string]any{
			"action": "log",
			"repo":   repo,
			"branch": *branch,
			"limit":  *limit,
		})
		if err != nil {
			return err
		}
		if *jsonOut {
			return printRepoJSON(out)
		}
		b, _ := json.Marshal(out)
		var wrapped struct {
			Commits []memrepo.CommitObject `json:"commits"`
		}
		if json.Unmarshal(b, &wrapped) == nil {
			for _, c := range wrapped.Commits {
				anchor := "unanchored"
				if c.AnchorTxID != "" {
					anchor = fmt.Sprintf("BSV %s:0 (n=%d)", shortRepoStr(c.AnchorTxID, 12), c.AnchorN)
				}
				fmt.Printf("commit %s (%s) [%s]\n  MPE: %s  Parents: %d\n  %s\n\n",
					shortRepoStr(c.CommitHash, 16), c.Branch, anchor, shortRepoStr(c.MPERoot, 16), len(c.Parents), c.Message)
			}
			return nil
		}
		return printRepoJSON(out)
	}

	dir := resolveRepoDataDir(*dataDir, *netName)
	ctx := context.Background()
	mem, _, cleanup, err := openNode(ctx, *netName, dir, false)
	if err != nil {
		return err
	}
	defer cleanup()

	commits, err := mem.RepoLog(ctx, repo, *branch, *limit)
	if err != nil {
		return err
	}
	if *jsonOut {
		return printRepoJSON(commits)
	}
	for _, c := range commits {
		anchor := "unanchored"
		if c.AnchorTxID != "" {
			anchor = fmt.Sprintf("BSV %s:0 (n=%d)", shortRepoStr(c.AnchorTxID, 12), c.AnchorN)
		}
		fmt.Printf("commit %s (%s) [%s]\n  MPE: %s  Parents: %d\n  %s\n\n",
			shortRepoStr(c.CommitHash, 16), c.Branch, anchor, shortRepoStr(c.MPERoot, 16), len(c.Parents), c.Message)
	}
	return nil
}

func cmdRepoTree(args []string) error {
	fs := flag.NewFlagSet("repo tree", flag.ExitOnError)
	branch := fs.String("branch", "main", "branch name")
	commit := fs.String("commit", "", "commit hash")
	dataDir := fs.String("data-dir", "", "data directory override")
	netName := fs.String("network", "", "network override")
	jsonOut := fs.Bool("json", false, "output raw JSON")
	pos, err := parseRepoArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("usage: nektar repo tree <repo> [-branch main] [-commit hash]")
	}
	repo := pos[0]

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveTool(info, "repo", map[string]any{
			"action": "tree",
			"repo":   repo,
			"branch": *branch,
			"commit": *commit,
		})
		if err != nil {
			return err
		}
		if *jsonOut {
			return printRepoJSON(out)
		}
		b, _ := json.Marshal(out)
		var wrapped struct {
			Tree *memrepo.TreeObject `json:"tree"`
		}
		if json.Unmarshal(b, &wrapped) == nil && wrapped.Tree != nil {
			for _, e := range wrapped.Tree.Entries {
				ref := ""
				if e.ChirpURL != "" {
					ref = "  " + e.ChirpURL
				}
				fmt.Printf("%s %-36s %-22s %7dB%s\n", e.Mode, e.Path, e.MediaType, e.Size, ref)
			}
			return nil
		}
		return printRepoJSON(out)
	}

	dir := resolveRepoDataDir(*dataDir, *netName)
	ctx := context.Background()
	mem, _, cleanup, err := openNode(ctx, *netName, dir, false)
	if err != nil {
		return err
	}
	defer cleanup()

	c, tr, _, err := mem.RepoTree(ctx, repo, *branch, *commit)
	if err != nil {
		return err
	}
	if *jsonOut {
		return printRepoJSON(map[string]any{"commit": c, "tree": tr})
	}
	for _, e := range tr.Entries {
		ref := ""
		if e.ChirpURL != "" {
			ref = "  " + e.ChirpURL
		}
		fmt.Printf("%s %-36s %-22s %7dB%s\n", e.Mode, e.Path, e.MediaType, e.Size, ref)
	}
	return nil
}

func cmdRepoDiff(args []string) error {
	fs := flag.NewFlagSet("repo diff", flag.ExitOnError)
	fromC := fs.String("from", "", "base commit hash")
	toC := fs.String("to", "", "target commit hash")
	dataDir := fs.String("data-dir", "", "data directory override")
	netName := fs.String("network", "", "network override")
	pos, err := parseRepoArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("usage: nektar repo diff <repo> [-from hash] [-to hash]")
	}
	repo := pos[0]

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveTool(info, "repo", map[string]any{
			"action": "diff",
			"repo":   repo,
			"from":   *fromC,
			"to":     *toC,
		})
		if err != nil {
			return err
		}
		return printRepoJSON(out)
	}

	dir := resolveRepoDataDir(*dataDir, *netName)
	ctx := context.Background()
	mem, _, cleanup, err := openNode(ctx, *netName, dir, false)
	if err != nil {
		return err
	}
	defer cleanup()

	changes, err := mem.RepoDiff(ctx, repo, *fromC, *toC)
	if err != nil {
		return err
	}
	return printRepoJSON(changes)
}

func openNode(ctx context.Context, netName, dataDir string, _ bool) (*memory.Service, any, func(), error) {
	rt, err := openRuntime(ctx, netName, dataDir, DefaultLocalPort)
	if err != nil {
		return nil, nil, nil, err
	}
	return rt.mem, rt, rt.closeFn, nil
}

func printRepoJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func shortRepoStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
