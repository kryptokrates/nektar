package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
)

func registerMCPResourcesAndPrompts(srv *mcp.Server, tb *Toolbox) {
	// --- MCP Static & Dynamic Resources ---
	srv.AddResource(&mcp.Resource{
		URI:         "nektar://welcome",
		Name:        "Nektar Welcome Memory",
		Description: "Seeded starter memory fragments and usage instructions.",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		res, err := tb.ExecRecall(ctx, memory.RecallInput{Shard: "welcome", Limit: 20})
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     marshalPrettyJSON(res),
			}},
		}, nil
	})

	srv.AddResource(&mcp.Resource{
		URI:         "nektar://repos",
		Name:        "Nektar Memory Repos",
		Description: "List of all Git-compatible, BSV UTXO-anchored Memory Repos with branch heads and anchor status.",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		cc := CallerContextFor(ctx, policy.Principal{})
		repos, err := tb.mem.ListReposAuthorized(ctx, cc.Principal)
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     marshalPrettyJSON(map[string]any{"repos": repos, "count": len(repos)}),
			}},
		}, nil
	})

	srv.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "nektar://repo/{repo}/tree",
		Name:        "Memory Repo File Tree",
		Description: "Active file tree snapshot of a Memory Repo (default branch main).",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		repo := extractRepoFromResourceURI(req.Params.URI, "/tree")
		if repo == "" {
			return nil, fmt.Errorf("invalid repo tree URI %q", req.Params.URI)
		}
		cc := CallerContextFor(ctx, policy.Principal{})
		c, tr, _, err := tb.mem.RepoTreeAuthorized(ctx, repo, memrepo.DefaultBranch, "", cc.Principal, cc.DisallowBorg)
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     marshalPrettyJSON(map[string]any{"repo": repo, "commit": c, "tree": tr}),
			}},
		}, nil
	})

	srv.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "nektar://repo/{repo}/log",
		Name:        "Memory Repo Commit Log",
		Description: "Recent signed Git/Metanet commits and BSV UTXO anchors for a Memory Repo.",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		repo := extractRepoFromResourceURI(req.Params.URI, "/log")
		if repo == "" {
			return nil, fmt.Errorf("invalid repo log URI %q", req.Params.URI)
		}
		cc := CallerContextFor(ctx, policy.Principal{})
		commits, err := tb.mem.RepoLogAuthorized(ctx, repo, "", 50, cc.Principal)
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     marshalPrettyJSON(map[string]any{"repo": repo, "commits": commits}),
			}},
		}, nil
	})

	srv.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "nektar://repo/{repo}/file/{+path}",
		Name:        "Memory Repo File Content",
		Description: "Read a specific memory file at {path} inside Memory Repo {repo}.",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		repo, relPath := extractRepoFileFromResourceURI(req.Params.URI)
		if repo == "" || relPath == "" {
			return nil, fmt.Errorf("invalid repo file URI %q", req.Params.URI)
		}
		res, err := tb.ExecRepo(ctx, RepoToolInput{Action: "read", Repo: repo, Path: relPath})
		if err != nil {
			return nil, err
		}
		m, _ := res.(map[string]any)
		mime, _ := m["mediaType"].(string)
		if mime == "" {
			mime = "text/plain"
		}
		txt, _ := m["text"].(string)
		if txt == "" {
			txt = marshalPrettyJSON(res)
			mime = "application/json"
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      req.Params.URI,
				MIMEType: mime,
				Text:     txt,
			}},
		}, nil
	})

	// --- MCP Workflow Prompts ---
	srv.AddPrompt(&mcp.Prompt{
		Name:        "grounded_brief",
		Description: "Recall verified memory and draft a strictly grounded answer with a checkable nektarr1.* receipt footer.",
		Arguments: []*mcp.PromptArgument{
			{Name: "question", Description: "Factual question to answer from verified memory", Required: true},
			{Name: "repo", Description: "Optional Memory Repo name to scope recall", Required: false},
		},
	}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		q := req.Params.Arguments["question"]
		repo := req.Params.Arguments["repo"]
		scope := "across all readable repos"
		if repo != "" {
			scope = fmt.Sprintf("in repo %q", repo)
		}
		return &mcp.GetPromptResult{
			Description: "Recall and produce a strictly grounded, receipt-backed answer",
			Messages: []*mcp.PromptMessage{{
				Role: mcp.Role("user"),
				Content: &mcp.TextContent{
					Text: fmt.Sprintf("1. Call recall %s for query %q.\n2. Route every factual claim through groundedAnswer citing exact verified fragments.\n3. End with the exact receipt: and verify: footer returned by groundedAnswer (never fabricate a receipt; if nothing grounds, say so honestly).", scope, q),
				},
			}},
		}, nil
	})

	srv.AddPrompt(&mcp.Prompt{
		Name:        "fork_and_investigate",
		Description: "Create an isolated reasoning branch in a Memory Repo, commit findings non-destructively, diff against main, and prepare a clean 3-way merge.",
		Arguments: []*mcp.PromptArgument{
			{Name: "repo", Description: "Target Memory Repo name", Required: true},
			{Name: "branch", Description: "New branch name (e.g. agent/research)", Required: true},
			{Name: "goal", Description: "Research or editing goal", Required: true},
		},
	}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		repo := req.Params.Arguments["repo"]
		br := req.Params.Arguments["branch"]
		goal := req.Params.Arguments["goal"]
		return &mcp.GetPromptResult{
			Description: "Branch, commit findings, diff, and prepare a 3-way merge",
			Messages: []*mcp.PromptMessage{{
				Role: mcp.Role("user"),
				Content: &mcp.TextContent{
					Text: fmt.Sprintf("In Memory Repo %q:\n1. Call repo {action:\"branch\", repo:%q, branch:%q} to fork an isolated branch.\n2. Investigate %q and commit structured markdown notes via repo {action:\"commit\", repo:%q, branch:%q, path:\"notes/...\", text:..., message:...}.\n3. Call repo {action:\"diff\", repo:%q, from:\"main\", to:%q} to review exact line changes before proposing a 3-way merge.", repo, repo, br, goal, repo, br, repo, br),
				},
			}},
		}, nil
	})

	srv.AddPrompt(&mcp.Prompt{
		Name:        "audit_provenance",
		Description: "Audit line-by-line authorship (blame), strict NektarAnchor UTXO spend lineage, and selective disclosure proofs for a Memory Repo.",
		Arguments: []*mcp.PromptArgument{
			{Name: "repo", Description: "Memory Repo name to audit", Required: true},
			{Name: "path", Description: "Optional file path for line-by-line UTXO blame", Required: false},
		},
	}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		repo := req.Params.Arguments["repo"]
		p := req.Params.Arguments["path"]
		extra := ""
		if p != "" {
			extra = fmt.Sprintf("\n3. Call repo {action:\"blame\", repo:%q, path:%q} to trace every line to its authoring commit, modelId, and BSV UTXO.", repo, p)
		}
		return &mcp.GetPromptResult{
			Description: "Audit cryptographic provenance and UTXO lineage",
			Messages: []*mcp.PromptMessage{{
				Role: mcp.Role("user"),
				Content: &mcp.TextContent{
					Text: fmt.Sprintf("Audit Memory Repo %q:\n1. Call repo {action:\"verify\", repo:%q} to verify strict NektarAnchor UTXO spend lineage (tx_{n-1}:0 -> tx_n:0) and TagLeafV2 commitments.\n2. Call repo {action:\"log\", repo:%q} to review commit ancestry and any external Market Forks.%s", repo, repo, repo, extra),
				},
			}},
		}, nil
	})
}

func extractRepoFromResourceURI(uri, suffix string) string {
	const prefix = "nektar://repo/"
	if !strings.HasPrefix(uri, prefix) || !strings.HasSuffix(uri, suffix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(uri, prefix), suffix))
}

func extractRepoFileFromResourceURI(uri string) (string, string) {
	const prefix = "nektar://repo/"
	if !strings.HasPrefix(uri, prefix) {
		return "", ""
	}
	rest := strings.TrimPrefix(uri, prefix)
	idx := strings.Index(rest, "/file/")
	if idx <= 0 {
		return "", ""
	}
	return strings.TrimSpace(rest[:idx]), strings.TrimSpace(rest[idx+len("/file/"):])
}
