package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func newGitMCPTestApp(t *testing.T) (*App, *memory.Service, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	app, err := NewApp(mem, mkt, Config{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)
	return app, mem, ts
}

func TestNoHTTPGitRoutesExposed(t *testing.T) {
	ctx := context.Background()
	_, mem, ts := newGitMCPTestApp(t)

	if _, err := mem.Remember(ctx, memory.RememberInput{
		Shard:         "alpha",
		Path:          "notes.md",
		Text:          "# Alpha\nGenesis memory.\n",
		CommitMessage: "Seed alpha memory",
		AutoAnchor:    true,
	}); err != nil {
		t.Fatalf("seed Remember: %v", err)
	}

	// Verify zero HTTP Git endpoints are mounted (attack surface stays strictly closed).
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/git/alpha.git/info/refs?service=git-upload-pack"},
		{http.MethodPost, "/git/alpha.git/git-upload-pack"},
		{http.MethodPost, "/git/alpha.git/git-receive-pack"},
	} {
		req, _ := http.NewRequest(tc.method, ts.URL+tc.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("HTTP git route %s %s must NOT be exposed, got 200 OK", tc.method, tc.path)
		}
	}
}

func TestMCPToolsMatchToolboxParityResourcesAndPrompts(t *testing.T) {
	ctx := context.Background()
	app, mem, _ := newGitMCPTestApp(t)

	if _, err := mem.Remember(ctx, memory.RememberInput{
		Shard:         "alpha",
		Path:          "notes.md",
		Text:          "SingularGrit unifies Git DAGs with BSV UTXO anchors.\n",
		CommitMessage: "Initial note",
		AutoAnchor:    true,
	}); err != nil {
		t.Fatal(err)
	}

	// Connect an official MCP Go SDK client over in-memory transport!
	srv := app.MCP()
	tClient, tServer := mcp.NewInMemoryTransports()
	srvSess, err := srv.Connect(ctx, tServer, nil)
	if err != nil {
		t.Fatalf("MCP server connect: %v", err)
	}
	defer srvSess.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "nektar-test-client", Version: "1.0.0"}, nil)
	cliSess, err := client.Connect(ctx, tClient, nil)
	if err != nil {
		t.Fatalf("MCP client connect: %v", err)
	}
	defer cliSess.Close()

	// 1. Verify 100% tool parity between mcp.go and tools_registry.go!
	listRes, err := cliSess.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("MCP ListTools: %v", err)
	}
	var mcpNames []string
	for _, tl := range listRes.Tools {
		mcpNames = append(mcpNames, tl.Name)
	}
	var tbNames []string
	for _, tl := range app.Toolbox().ListTools() {
		tbNames = append(tbNames, tl.Name)
	}
	sort.Strings(mcpNames)
	sort.Strings(tbNames)
	if strings.Join(mcpNames, ",") != strings.Join(tbNames, ",") {
		t.Fatalf("MCP/Toolbox drift detected!\nMCP:     %v\nToolbox: %v", mcpNames, tbNames)
	}

	// 2. Exercise unified MCP `repo` tool (commit, read, blame, tag, verify)!
	callRepo := func(args map[string]any) string {
		t.Helper()
		res, err := cliSess.CallTool(ctx, &mcp.CallToolParams{Name: "repo", Arguments: args})
		if err != nil {
			t.Fatalf("MCP repo(%v) error: %v", args["action"], err)
		}
		if res.IsError {
			t.Fatalf("MCP repo(%v) returned tool error: %+v", args["action"], res.Content)
		}
		if len(res.Content) == 0 {
			return ""
		}
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			return tc.Text
		}
		return ""
	}

	readOut := callRepo(map[string]any{"action": "read", "repo": "alpha", "path": "notes.md"})
	if !strings.Contains(readOut, "SingularGrit unifies Git DAGs") {
		t.Fatalf("MCP repo read unexpected output: %s", readOut)
	}
	blameOut := callRepo(map[string]any{"action": "blame", "repo": "alpha", "path": "notes.md"})
	if !strings.Contains(blameOut, `"anchorTxid"`) {
		t.Fatalf("MCP repo blame missing anchorTxid: %s", blameOut)
	}
	tagOut := callRepo(map[string]any{"action": "tag", "repo": "alpha", "tag": "v1-mcp", "message": "MCP signed checkpoint"})
	if !strings.Contains(tagOut, `"v1-mcp"`) {
		t.Fatalf("MCP repo tag failed: %s", tagOut)
	}
	verOut := callRepo(map[string]any{"action": "verify", "repo": "alpha"})
	if !strings.Contains(verOut, `"ok": true`) {
		t.Fatalf("MCP repo verify failed: %s", verOut)
	}

	// 3. Exercise MCP Resources & Resource Templates!
	for _, uri := range []string{
		"nektar://repos",
		"nektar://repo/alpha/tree",
		"nektar://repo/alpha/log",
		"nektar://repo/alpha/file/notes.md",
	} {
		rr, err := cliSess.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatalf("MCP ReadResource(%s): %v", uri, err)
		}
		if len(rr.Contents) == 0 || strings.TrimSpace(rr.Contents[0].Text) == "" {
			t.Fatalf("MCP ReadResource(%s) returned empty content", uri)
		}
		if uri == "nektar://repo/alpha/file/notes.md" && !strings.Contains(rr.Contents[0].Text, "SingularGrit") {
			t.Fatalf("unexpected file resource text: %s", rr.Contents[0].Text)
		}
	}

	// 4. Exercise MCP Prompts!
	pList, err := cliSess.ListPrompts(ctx, &mcp.ListPromptsParams{})
	if err != nil {
		t.Fatalf("MCP ListPrompts: %v", err)
	}
	if len(pList.Prompts) < 3 {
		t.Fatalf("expected >=3 MCP prompts, got %d", len(pList.Prompts))
	}
	for _, name := range []string{"grounded_brief", "fork_and_investigate", "audit_provenance"} {
		gp, err := cliSess.GetPrompt(ctx, &mcp.GetPromptParams{
			Name:      name,
			Arguments: map[string]string{"repo": "alpha", "question": "What is SingularGrit?", "branch": "exp", "path": "notes.md"},
		})
		if err != nil {
			t.Fatalf("MCP GetPrompt(%s): %v", name, err)
		}
		if len(gp.Messages) == 0 {
			t.Fatalf("MCP GetPrompt(%s) returned zero messages", name)
		}
	}
}

func TestRepoRESTEndpointsDiffBlameDeleteTagForkProveInspect(t *testing.T) {
	ctx := context.Background()
	_, mem, ts := newGitMCPTestApp(t)

	r1, err := mem.Remember(ctx, memory.RememberInput{
		Shard: "beta", Path: "a.md", Text: "line one\nline two\n", CommitMessage: "v1", AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := mem.Remember(ctx, memory.RememberInput{
		Shard: "beta", Path: "a.md", Text: "line one\nline two edited\nline three\n", CommitMessage: "v2", AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. GET /v1/repos/beta/diff -> Myers unified patch
	resp, err := http.Get(ts.URL + "/v1/repos/beta/diff?from=" + r1.CommitHash + "&to=" + r2.CommitHash)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET diff failed: %v %+v", err, resp)
	}
	var diffOut struct {
		Patch []struct {
			Path      string `json:"path"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
		} `json:"patch"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&diffOut)
	resp.Body.Close()
	if len(diffOut.Patch) != 1 || diffOut.Patch[0].Additions < 2 || diffOut.Patch[0].Deletions < 1 {
		t.Fatalf("unexpected REST diff patch: %+v", diffOut)
	}

	// 2. GET /v1/repos/beta/blame
	bResp, err := http.Get(ts.URL + "/v1/repos/beta/blame?path=a.md")
	if err != nil || bResp.StatusCode != http.StatusOK {
		t.Fatalf("GET blame failed: %v %+v", err, bResp)
	}
	var blameOut struct {
		Lines []map[string]any `json:"lines"`
	}
	_ = json.NewDecoder(bResp.Body).Decode(&blameOut)
	bResp.Body.Close()
	if len(blameOut.Lines) != 3 {
		t.Fatalf("expected 3 blame lines, got %+v", blameOut)
	}

	// 3. POST /v1/repos/beta/prove-fields
	pResp, err := http.Post(ts.URL+"/v1/repos/beta/prove-fields", "application/json",
		strings.NewReader(`{"commit":"`+r2.CommitHash+`","fields":["source","path","modelId"]}`))
	if err != nil || pResp.StatusCode != http.StatusOK {
		t.Fatalf("POST prove-fields failed: %v %+v", err, pResp)
	}
	pResp.Body.Close()

	// 4. POST /v1/repos/beta/tags & GET /v1/repos/beta/tags
	tResp, err := http.Post(ts.URL+"/v1/repos/beta/tags", "application/json",
		strings.NewReader(`{"tag":"v2-release","commit":"main","message":"Release v2"}`))
	if err != nil || tResp.StatusCode != http.StatusOK {
		t.Fatalf("POST tags failed: %v %+v", err, tResp)
	}
	tResp.Body.Close()

	// 5. POST /v1/repos/beta/fork
	fResp, err := http.Post(ts.URL+"/v1/repos/beta/fork", "application/json",
		strings.NewReader(`{"branch":"main","newRepo":"beta-fork","autoAnchor":true}`))
	if err != nil || fResp.StatusCode != http.StatusOK {
		t.Fatalf("POST fork failed: %v %+v", err, fResp)
	}
	fResp.Body.Close()

	// 6. Export .nkt capsule & inspect read-only via POST /v1/repos/inspect-capsule!
	_, _, capRaw, err := mem.ExportCapsule(ctx, "beta", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	iResp, err := http.Post(ts.URL+"/v1/repos/inspect-capsule", "application/octet-stream", bytes.NewReader(capRaw))
	if err != nil || iResp.StatusCode != http.StatusOK {
		t.Fatalf("POST inspect-capsule failed: %v %+v", err, iResp)
	}
	var inspOut struct {
		LocalExists  bool `json:"localExists"`
		Verification struct {
			OK bool `json:"ok"`
		} `json:"verification"`
	}
	_ = json.NewDecoder(iResp.Body).Decode(&inspOut)
	iResp.Body.Close()
	if !inspOut.Verification.OK || !inspOut.LocalExists {
		t.Fatalf("unexpected inspect-capsule output: %+v", inspOut)
	}

	// 7. DELETE /v1/repos/beta/file + POST /v1/repos/beta/revert
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/repos/beta/file",
		strings.NewReader(`{"branch":"main","path":"a.md","autoAnchor":true}`))
	delReq.Header.Set("Content-Type", "application/json")
	dResp, err := http.DefaultClient.Do(delReq)
	if err != nil || dResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /file failed: %v %+v", err, dResp)
	}
	var delC struct {
		CommitHash string `json:"commitHash"`
	}
	_ = json.NewDecoder(dResp.Body).Decode(&delC)
	dResp.Body.Close()

	revResp, err := http.Post(ts.URL+"/v1/repos/beta/revert", "application/json",
		strings.NewReader(`{"branch":"main","commit":"`+delC.CommitHash+`","autoAnchor":true}`))
	if err != nil || revResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /revert failed: %v %+v", err, revResp)
	}
	revResp.Body.Close()
}
