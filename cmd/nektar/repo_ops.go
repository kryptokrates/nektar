package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/server"
)

func cmdRepoCommit(args []string) error {
	fs := flag.NewFlagSet("repo commit", flag.ExitOnError)
	text := fs.String("text", "", "text fact to commit")
	filePath := fs.String("file", "", "local file path to attach as volumetric CHIRP blob")
	repoPath := fs.String("path", "", "path inside repo tree (e.g. docs/spec.pdf)")
	branch := fs.String("branch", "main", "branch name")
	msg := fs.String("m", "", "commit message")
	anchor := fs.Bool("anchor", false, "anchor immediately on BSV via NektarAnchor")
	group := fs.String("group", "", "optional Borg group for BRC-369 encryption")
	publicFlag := fs.Bool("public", false, "opt out of private-by-default CLKA encryption")
	ontology := fs.String("ontology", "", "optional committed domain ontology (ont-<domain> or 64-hex OntologyID) for JevGraph extraction")
	ontologyStrict := fs.Bool("ontology-strict", false, "fail closed on -ontology commit if Jev evaluator is offline")
	dataDir := fs.String("data-dir", "", "data directory override")
	netName := fs.String("network", "", "network override")
	pos, err := parseRepoArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("usage: nektar repo commit <repo> -text \"...\" | -file <path> [-path repo/file.md] [-branch main] [-m msg] [-ontology ont-<domain>] [-anchor]")
	}
	repo := pos[0]

	var blob []byte
	targetPath := strings.TrimSpace(*repoPath)
	if strings.TrimSpace(*filePath) != "" {
		raw, err := os.ReadFile(*filePath)
		if err != nil {
			return err
		}
		blob = raw
		if targetPath == "" {
			targetPath = "files/" + filepath.Base(*filePath)
		}
	}

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		payload := map[string]any{
			"action":         "commit",
			"repo":           repo,
			"branch":         *branch,
			"path":           targetPath,
			"text":           *text,
			"message":        *msg,
			"borgGroup":      *group,
			"public":         *publicFlag,
			"ontology":       *ontology,
			"ontologyStrict": *ontologyStrict,
			"autoAnchor":     *anchor,
		}
		if len(blob) > 0 {
			payload["blobBase64"] = base64.StdEncoding.EncodeToString(blob)
		}
		out, err := callLiveTool(info, "repo", payload)
		if err != nil {
			return err
		}
		return printJSON(out)
	}

	dir := resolveRepoDataDir(*dataDir, *netName)
	ctx := context.Background()
	mem, _, cleanup, err := openNode(ctx, *netName, dir, false)
	if err != nil {
		return err
	}
	defer cleanup()

	res, err := mem.Remember(ctx, memory.RememberInput{
		Shard:          repo,
		Branch:         *branch,
		Path:           targetPath,
		Text:           *text,
		Blob:           blob,
		CommitMessage:  *msg,
		BorgGroup:      *group,
		Public:         *publicFlag,
		Ontology:       *ontology,
		OntologyStrict: *ontologyStrict,
		AutoAnchor:     *anchor,
	})
	if err != nil {
		return err
	}
	return printJSON(res)
}

func cmdRepoBranch(args []string) error {
	fs := flag.NewFlagSet("repo branch", flag.ExitOnError)
	from := fs.String("from", "main", "source branch or commit")
	dataDir := fs.String("data-dir", "", "data directory override")
	netName := fs.String("network", "", "network override")
	pos, err := parseRepoArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return errors.New("usage: nektar repo branch <repo> <new-branch> [-from main]")
	}
	repo, newBr := pos[0], pos[1]

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveTool(info, "repo", map[string]any{
			"action": "branch",
			"repo":   repo,
			"branch": newBr,
			"from":   *from,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}

	dir := resolveRepoDataDir(*dataDir, *netName)
	ctx := context.Background()
	mem, _, cleanup, err := openNode(ctx, *netName, dir, false)
	if err != nil {
		return err
	}
	defer cleanup()

	br, err := mem.CreateBranch(ctx, repo, newBr, *from)
	if err != nil {
		return err
	}
	return printJSON(br)
}

func cmdRepoMerge(args []string) error {
	fs := flag.NewFlagSet("repo merge", flag.ExitOnError)
	into := fs.String("into", "main", "target branch")
	msg := fs.String("m", "", "merge commit message")
	dataDir := fs.String("data-dir", "", "data directory override")
	netName := fs.String("network", "", "network override")
	pos, err := parseRepoArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return errors.New("usage: nektar repo merge <repo> <source-branch> [-into main] [-m msg]")
	}
	repo, srcBr := pos[0], pos[1]

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveTool(info, "repo", map[string]any{
			"action":       "merge",
			"repo":         repo,
			"sourceBranch": srcBr,
			"targetBranch": *into,
			"message":      *msg,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}

	dir := resolveRepoDataDir(*dataDir, *netName)
	ctx := context.Background()
	mem, _, cleanup, err := openNode(ctx, *netName, dir, false)
	if err != nil {
		return err
	}
	defer cleanup()

	c, err := mem.MergeBranch(ctx, repo, srcBr, *into, *msg)
	if err != nil {
		return err
	}
	return printJSON(c)
}

func downloadLiveCapsule(info *server.DaemonInfo, repo, branch, since string) ([]byte, error) {
	q := url.Values{}
	if branch != "" {
		q.Set("branch", branch)
	}
	if since != "" {
		q.Set("since", since)
	}
	q.Set("download", "1")
	ep := strings.TrimRight(info.URL, "/") + "/v1/repos/" + url.PathEscape(repo) + "/capsule?" + q.Encode()
	req, err := http.NewRequest(http.MethodGet, ep, nil)
	if err != nil {
		return nil, err
	}
	tok := info.AuthToken
	if tok == "" {
		tok = info.Token
	}
	if opTok := strings.TrimSpace(os.Getenv("NEKTAR_OPERATOR_TOKEN")); opTok != "" {
		tok = opTok
	} else if rawOp, rErr := os.ReadFile(filepath.Join(info.DataDir, "operator-token")); rErr == nil {
		if s := strings.TrimSpace(string(rawOp)); s != "" {
			tok = s
		}
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("daemon HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func cmdRepoExport(args []string) error {
	fs := flag.NewFlagSet("repo export", flag.ExitOnError)
	branch := fs.String("branch", "main", "branch name")
	since := fs.String("since", "", "export incremental delta after commit hash")
	outFile := fs.String("out", "", "optional .nkt file path")
	dataDir := fs.String("data-dir", "", "data directory override")
	netName := fs.String("network", "", "network override")
	pos, err := parseRepoArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("usage: nektar repo export <repo> [-branch main] [-since commit] [-out file.nkt]")
	}
	repo := pos[0]

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		if strings.TrimSpace(*outFile) != "" {
			raw, err := downloadLiveCapsule(info, repo, *branch, *since)
			if err != nil {
				return err
			}
			if err := os.WriteFile(*outFile, raw, 0o640); err != nil {
				return err
			}
			return printJSON(map[string]any{"ok": true, "repo": repo, "branch": *branch, "out": *outFile, "bytes": len(raw)})
		}
		out, err := callLiveTool(info, "repo", map[string]any{
			"action": "export",
			"repo":   repo,
			"branch": *branch,
			"since":  *since,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}

	dir := resolveRepoDataDir(*dataDir, *netName)
	ctx := context.Background()
	mem, _, cleanup, err := openNode(ctx, *netName, dir, false)
	if err != nil {
		return err
	}
	defer cleanup()

	res, _, raw, err := mem.ExportCapsule(ctx, repo, *branch, *since)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*outFile) != "" {
		if err := os.WriteFile(*outFile, raw, 0o640); err != nil {
			return err
		}
	}
	return printJSON(res)
}

func cmdRepoImport(args []string) error {
	fs := flag.NewFlagSet("repo import", flag.ExitOnError)
	targetRepo := fs.String("repo", "", "target local repo name")
	targetBranch := fs.String("branch", "main", "target branch")
	merge := fs.Bool("merge", true, "fork & merge preserving seller UTXO parent lineage")
	dataDir := fs.String("data-dir", "", "data directory override")
	netName := fs.String("network", "", "network override")
	pos, err := parseRepoArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("usage: nektar repo import <file.nkt | chirp://...> [-repo target] [-branch main] [-merge=true]")
	}
	source := pos[0]

	var raw []byte
	var chirpURL string
	if strings.HasPrefix(source, "chirp://") {
		chirpURL = source
	} else {
		var err error
		raw, err = os.ReadFile(source)
		if err != nil {
			return err
		}
	}

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		payload := map[string]any{
			"action":        "import",
			"repo":          *targetRepo,
			"targetBranch":  *targetBranch,
			"chirpUrl":      chirpURL,
			"mergeIntoMain": *merge,
		}
		if len(raw) > 0 {
			payload["capsuleBase64"] = base64.StdEncoding.EncodeToString(raw)
		}
		out, err := callLiveTool(info, "repo", payload)
		if err != nil {
			return err
		}
		return printJSON(out)
	}

	dir := resolveRepoDataDir(*dataDir, *netName)
	ctx := context.Background()
	mem, _, cleanup, err := openNode(ctx, *netName, dir, false)
	if err != nil {
		return err
	}
	defer cleanup()

	res, err := mem.ImportCapsule(ctx, raw, chirpURL, *targetRepo, *targetBranch, *merge)
	if err != nil {
		return err
	}
	return printJSON(res)
}

func cmdRepoVerify(args []string) error {
	fs := flag.NewFlagSet("repo verify", flag.ExitOnError)
	netName := fs.String("network", envOr("NEKTAR_NETWORK", "testnet"), "network for witnessed header SPV verification")
	offlineStructural := fs.Bool("offline-structural", false, "skip witnessed PoW header verification (structural check only; NOT SPV-verified)")
	pos, err := parseRepoArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("usage: nektar repo verify <file.nkt> [-network testnet|main|memory] [-offline-structural]")
	}
	filePath := pos[0]

	raw, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	cap, err := memrepo.DecodeCapsule(raw)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var tracker chaintracker.ChainTracker
	memoryMode := *offlineStructural || strings.EqualFold(strings.TrimSpace(*netName), "memory")
	if *offlineStructural {
		fmt.Fprintln(os.Stderr, "WARNING: --offline-structural skips witnessed PoW header verification; anchors are NOT verified against chain headers")
	} else if !memoryMode {
		headerSpec := strings.TrimSpace(os.Getenv("NEKTAR_HEADERS_SOURCE"))
		if headerSpec == "" {
			headerSpec = strings.TrimSpace(os.Getenv("NEKTAR_HEADER_SOURCES"))
		}
		if headerSpec == "" {
			headerSpec = chain.DefaultHeaderSources(*netName)
		}
		chainSpec := strings.TrimSpace(os.Getenv("NEKTAR_CHAIN_SPEC"))
		if chainSpec == "" {
			if strings.EqualFold(strings.TrimSpace(*netName), "main") {
				chainSpec = "woc:main"
			} else {
				chainSpec = "woc:test"
			}
		}
		settleSpec := strings.TrimSpace(os.Getenv("NEKTAR_SETTLE_SPEC"))
		safeClient := core.SafeHTTPClientForNetwork(*netName, 12*time.Second)
		wt, wErr := chain.NewWitnessedTrackerFromSpec(*netName, headerSpec, chainSpec, settleSpec, safeClient)
		if wErr != nil {
			return fmt.Errorf("init witnessed header tracker for %s: %w (pass --offline-structural for structural-only check)", *netName, wErr)
		}
		tracker = wt
	}

	rep, err := memrepo.VerifyCapsule(ctx, cap, nil, tracker, memoryMode)
	if err != nil {
		return err
	}
	if err := printJSON(rep); err != nil {
		return err
	}
	if !rep.OK {
		return fmt.Errorf("capsule verification failed: %s", rep.Reason)
	}
	return nil
}

func cmdRepoCheckout(args []string) error {
	fs := flag.NewFlagSet("repo checkout", flag.ExitOnError)
	branch := fs.String("branch", "main", "branch name")
	dir := fs.String("dir", "", "destination directory (default <dataDir>/repos/<repo>)")
	dataDir := fs.String("data-dir", "", "data directory override")
	netName := fs.String("network", "", "network override")
	pos, err := parseRepoArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("usage: nektar repo checkout <repo> [-branch main] [-dir ./out]")
	}
	repo := pos[0]

	resolvedDataDir := resolveRepoDataDir(*dataDir, *netName)
	customDir := strings.TrimSpace(*dir)
	dest := customDir
	if dest == "" {
		dest = filepath.Join(resolvedDataDir, "repos", strings.ReplaceAll(repo, "/", "_"))
	}
	// Proxy to the live daemon only when using the default managed path; explicit -dir runs locally
	// (or via live capsule export) so the daemon never writes to arbitrary paths and -dir is never ignored.
	if customDir == "" {
		if info, ok := detectDaemon(*dataDir, *netName); ok {
			out, err := callLiveAPI(info, http.MethodPost, "/v1/repos/"+url.PathEscape(repo)+"/checkout", map[string]any{
				"branch": *branch,
			})
			if err != nil {
				return err
			}
			return printJSON(out)
		}
	} else if info, ok := detectDaemon(*dataDir, *netName); ok {
		raw, err := downloadLiveCapsule(info, repo, *branch, "")
		if err != nil {
			return err
		}
		capObj, err := memrepo.DecodeCapsule(raw)
		if err != nil {
			return err
		}
		res, err := memrepo.ExportGitWorkspace(dest, capObj, nil)
		if err != nil {
			return err
		}
		return printJSON(res)
	}

	ctx := context.Background()
	mem, _, cleanup, err := openNode(ctx, *netName, resolvedDataDir, false)
	if err != nil {
		return err
	}
	defer cleanup()

	res, err := mem.CheckoutGitWorkspace(ctx, repo, *branch, dest)
	if err != nil {
		return err
	}
	return printJSON(res)
}
