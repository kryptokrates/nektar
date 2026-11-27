package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/store"
)

// TestMetricsAlertsParity enforces S2: every nektar_* metric referenced in ops/alerts.yml
// must be emitted verbatim by GET /v1/metrics so alerts can never silently evaluate to empty.
func TestMetricsAlertsParity(t *testing.T) {
	alertsBytes, err := os.ReadFile(filepath.Join("..", "..", "ops", "alerts.yml"))
	if err != nil {
		t.Fatalf("read ops/alerts.yml: %v", err)
	}
	re := regexp.MustCompile(`\bnektar_[a-z0-9_]+\b`)
	matches := re.FindAllString(string(alertsBytes), -1)
	if len(matches) == 0 {
		t.Fatal("ops/alerts.yml referenced zero nektar_* metrics")
	}

	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	app, err := NewApp(mem, mkt, Config{})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/metrics", nil)
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/metrics status=%d", rec.Code)
	}
	body := rec.Body.String()

	seen := make(map[string]bool)
	for _, metric := range matches {
		if seen[metric] {
			continue
		}
		seen[metric] = true
		if !strings.Contains(body, "# TYPE "+metric+" ") {
			t.Errorf("ops/alerts.yml references metric %q which is NOT emitted by GET /v1/metrics", metric)
		}
	}
}

// TestToolAuthBoundaries verifies non-operator callers cannot invoke borgGroup or export Level>0/Borg fragments.
func TestToolAuthBoundaries(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Remember(ctx, memory.RememberInput{Shard: "mixed", Text: "public level 0 fact", Level: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Remember(ctx, memory.RememberInput{Shard: "mixed", Text: "classified level 5 secret", Level: 5}); err != nil {
		t.Fatal(err)
	}
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	app, err := NewApp(mem, mkt, Config{
		APIToken:      "agent-tok-1234567890",
		OperatorToken: "op-tok-1234567890",
		ToolProfile:   "operator",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Non-operator caller MUST be denied borgGroup.
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/borgGroup", strings.NewReader(`{"op":"create","groupId":"g1","name":"test"}`))
	req.Header.Set("Authorization", "Bearer agent-tok-1234567890")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("non-operator was allowed to call borgGroup: %s", rec.Body.String())
	}

	// 2. Non-operator exportPack MUST filter out Level-5 fragments (only Level 0 exported).
	req2 := httptest.NewRequest(http.MethodPost, "/v1/tools/exportPack", strings.NewReader(`{"shard":"mixed"}`))
	req2.Header.Set("Authorization", "Bearer agent-tok-1234567890")
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("exportPack status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var out struct {
		ChirpURL string `json:"chirpUrl"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &out); err != nil || out.ChirpURL == "" {
		t.Fatalf("decode exportPack: err=%v body=%s", err, rec2.Body.String())
	}
	raw, err := bstack.ResolveChirp(mem.Objects(), out.ChirpURL)
	if err != nil {
		t.Fatalf("ResolveChirp: %v", err)
	}
	capObj, err := memrepo.DecodeCapsule(raw)
	if err != nil {
		t.Fatalf("DecodeCapsule: %v", err)
	}
	foundPublic := false
	for _, f := range capObj.Fragments {
		txt := string(f.Data)
		if strings.Contains(txt, "classified level 5 secret") {
			t.Fatal("non-operator exportPack leaked Level-5 classified fragment!")
		}
		if strings.Contains(txt, "public level 0 fact") {
			foundPublic = true
		}
	}
	for _, b := range capObj.Blobs {
		if strings.Contains(string(b.Inline), "classified level 5 secret") {
			t.Fatal("non-operator exportPack leaked Level-5 classified blob!")
		}
	}
	if !foundPublic {
		t.Fatalf("non-operator exportPack did not include Level-0 public fragment (got %d fragments)", len(capObj.Fragments))
	}
}

// TestDashboardStateFeedEnrichment verifies GET /state includes recent feed fragments and receipt answers.
func TestDashboardStateFeedEnrichment(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	rem, err := mem.Remember(ctx, memory.RememberInput{Shard: "notes", Text: "Twetch style timeline test fact"})
	if err != nil {
		t.Fatal(err)
	}
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	app, err := NewApp(mem, mkt, Config{})
	if err != nil {
		t.Fatal(err)
	}
	st := app.BuildDashboardState(ctx, "http://127.0.0.1:18420")
	feed, ok := st["feed"].([]map[string]any)
	if !ok || len(feed) == 0 {
		t.Fatalf("expected non-empty feed in BuildDashboardState, got %T %v", st["feed"], st["feed"])
	}
	if feed[0]["shard"] != "notes" || feed[0]["seq"] != rem.Seq {
		t.Fatalf("unexpected feed[0]: %+v", feed[0])
	}
}

// TestPolicyProfileDefaultsConfidence verifies Easy / Standard / Secure profiles automatically attach
// backend semantic-matching confidence floors (0.80 / 0.85 / 0.92) via handlePolicyPreset.
func TestPolicyProfileDefaultsConfidence(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	app, err := NewApp(mem, mkt, Config{})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		profile string
		wantBP  int64
		wantMax uint64
	}{
		{"easy", 8000, 5000},
		{"standard", 8500, 2000},
		{"secure", 9200, 250},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/onboard/policy-preset", strings.NewReader(`{"profile":"`+tc.profile+`"}`))
		rr := httptest.NewRecorder()
		app.handlePolicyPreset(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("profile %s returned %d: %s", tc.profile, rr.Code, rr.Body.String())
		}
		var rec store.OperatorPolicyRecord
		if err := json.Unmarshal(rr.Body.Bytes(), &rec); err != nil {
			t.Fatalf("decode %s: %v", tc.profile, err)
		}
		if rec.MaxPerDeal != tc.wantMax {
			t.Fatalf("profile %s maxPerDeal=%d want %d", tc.profile, rec.MaxPerDeal, tc.wantMax)
		}
		var ai market.AIMatchingPolicy
		if err := json.Unmarshal([]byte(rec.AIPolicyJSON), &ai); err != nil {
			t.Fatalf("profile %s invalid AIPolicyJSON %q: %v", tc.profile, rec.AIPolicyJSON, err)
		}
		if ai.MinConfidenceBP != tc.wantBP {
			t.Fatalf("profile %s minConfidenceBp=%d want %d", tc.profile, ai.MinConfidenceBP, tc.wantBP)
		}
	}
}

// TestNoEnvNektarDataDirInServerPackage enforces RCA-RT-7 ratchet:
// internal/server/*.go must receive DataDir via server.Config.DataDir rather than reading os.Getenv("NEKTAR_DATA_DIR").
func TestNoEnvNektarDataDirInServerPackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `os.Getenv("NEKTAR_DATA_DIR")`) {
			t.Errorf("RCA-RT-7 ratchet: %s must use Config.DataDir instead of os.Getenv(\"NEKTAR_DATA_DIR\")", name)
		}
	}
}

// TestAuthMatrixMemRepoGitOAuthWellKnownAndToolParity verifies RCA-RT-2, RCA-RT-6, RCA-RT-7, F-05, F-11, #7.4, and #7.13.
func TestAuthMatrixMemRepoGitOAuthWellKnownAndToolParity(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// Seed Level-0 public file and Level-50 secret file in the same repo.
	if _, err := mem.Remember(ctx, memory.RememberInput{
		Shard: "classified", Path: "public.md", Text: "public level 0 line", Level: 0, AutoAnchor: true,
	}); err != nil {
		t.Fatal(err)
	}
	secretRes, err := mem.Remember(ctx, memory.RememberInput{
		Shard: "classified", Path: "secret.md", Text: "TOP-SECRET-LEVEL-50-PAYLOAD", CommitMessage: "Add classified file", Level: 50, AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	mkt := market.NewEngine(mem, market.DefaultPolicy())
	explicitDataDir := t.TempDir()
	app, err := NewApp(mem, mkt, Config{
		ListenAddr:    "127.0.0.1:18420",
		PublicURL:     "https://node.example.com",
		DataDir:       explicitDataDir,
		APIToken:      "agent-tok-xyz",
		OperatorToken: "op-tok-xyz",
		ToolProfile:   "operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	// 1. RCA-RT-2: Level:0 APIToken MUST NOT read Level:50 secret across tree, diff, blame, tools/repo, atCommit recall, or git clone!
	doReq := func(method, path, token, ct, body string) *http.Response {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}
	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"rest-tree", http.MethodGet, "/v1/repos/classified/tree", ""},
		{"rest-diff", http.MethodGet, "/v1/repos/classified/diff?to=" + secretRes.CommitHash, ""},
		{"rest-blame", http.MethodGet, "/v1/repos/classified/blame?path=secret.md", ""},
		{"tool-repo-tree", http.MethodPost, "/v1/tools/repo", `{"action":"tree","repo":"classified"}`},
		{"tool-repo-read", http.MethodPost, "/v1/tools/repo", `{"action":"read","repo":"classified","path":"secret.md"}`},
		{"recall-atCommit", http.MethodPost, "/v1/recall", `{"shard":"classified","atCommit":"` + secretRes.CommitHash + `"}`},
	} {
		resp := doReq(tc.method, tc.path, "agent-tok-xyz", "application/json", tc.body)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(raw), "TOP-SECRET-LEVEL-50-PAYLOAD") {
			t.Fatalf("RCA-RT-2 REGRESSION: %s leaked Level-50 secret to Level-0 agent token: %s", tc.name, string(raw))
		}
	}

	// 2. RCA-RT-2: Operator token MUST NOT be locked out of borgGroup, Borg writes, or operator repo mutations!
	opBorg := doReq(http.MethodPost, "/v1/tools/borgGroup", "op-tok-xyz", "application/json", `{"op":"create","groupId":"op-grp","name":"Op Group"}`)
	opBorgRaw, _ := io.ReadAll(opBorg.Body)
	opBorg.Body.Close()
	if opBorg.StatusCode != http.StatusOK {
		t.Fatalf("RCA-RT-2 REGRESSION: operator token locked out of borgGroup (status=%d body=%s)", opBorg.StatusCode, string(opBorgRaw))
	}
	opBranch := doReq(http.MethodPost, "/v1/tools/repo", "op-tok-xyz", "application/json", `{"action":"branch","repo":"classified","branch":"op-feature"}`)
	opBranchRaw, _ := io.ReadAll(opBranch.Body)
	opBranch.Body.Close()
	if opBranch.StatusCode != http.StatusOK {
		t.Fatalf("RCA-RT-2 REGRESSION: operator token locked out of repo branch (status=%d body=%s)", opBranch.StatusCode, string(opBranchRaw))
	}

	// Agent token MUST be denied operator repo mutations via /v1/tools/repo (branch, fork, merge, revert, cherryPick, tag, rotate, export, import)!
	for _, actionBody := range []string{
		`{"action":"branch","repo":"classified","branch":"evil-br"}`,
		`{"action":"fork","repo":"classified","newRepo":"evil-fork"}`,
		`{"action":"merge","repo":"classified","sourceBranch":"op-feature","targetBranch":"main"}`,
		`{"action":"tag","repo":"classified","tag":"evil-tag","commit":"main"}`,
		`{"action":"rotate","repo":"classified","newOwner":"02deadbeef"}`,
		`{"action":"export","repo":"classified"}`,
	} {
		r := doReq(http.MethodPost, "/v1/tools/repo", "agent-tok-xyz", "application/json", actionBody)
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode == http.StatusOK {
			t.Fatalf("RCA-RT-2 REGRESSION: agent token permitted operator repo action %s: %s", actionBody, string(b))
		}
	}

	// 3. RCA-RT-2: Loopback CSRF Content-Type guard covers /v1/repos/!
	csrfRepo := doReq(http.MethodPost, "/v1/repos/classified/commit", "op-tok-xyz", "text/plain", `{"path":"x.md","text":"csrf"}`)
	csrfRepo.Body.Close()
	if csrfRepo.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415 on text/plain POST /v1/repos/*/commit, got %d", csrfRepo.StatusCode)
	}

	// 4. RCA-RT-6: /v1/tools/repo MUST route to full 20-action ExecRepo (not stale 7-action callRepoTool)!
	for _, actionBody := range []string{
		`{"action":"show","repo":"classified","commit":"main"}`,
		`{"action":"read","repo":"classified","path":"public.md"}`,
		`{"action":"blame","repo":"classified","path":"public.md"}`,
		`{"action":"verify","repo":"classified"}`,
		`{"action":"tags","repo":"classified"}`,
	} {
		r := doReq(http.MethodPost, "/v1/tools/repo", "op-tok-xyz", "application/json", actionBody)
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("RCA-RT-6 REGRESSION: /v1/tools/repo rejected advertised action %s (status=%d body=%s)", actionBody, r.StatusCode, string(b))
		}
	}

	// 5. #7.13: agent profile MUST include read-only `peers` tool!
	agentTB := NewToolbox(mem, mkt, nil, nil, nil, "agent")
	if !agentTB.IsAllowedTool("peers") {
		t.Fatal("#7.13 REGRESSION: peers tool missing from agent profile Toolbox")
	}

	// 6. RCA-RT-7: handleRepoCheckout uses Config.DataDir (NOT NEKTAR_DATA_DIR env var)!
	t.Setenv("NEKTAR_DATA_DIR", filepath.Join(t.TempDir(), "wrong-env-dir"))
	coResp := doReq(http.MethodPost, "/v1/repos/classified/checkout", "op-tok-xyz", "application/json", `{}`)
	coBody, _ := io.ReadAll(coResp.Body)
	coResp.Body.Close()
	var coOut struct {
		Dir string `json:"dir"`
	}
	_ = json.Unmarshal(coBody, &coOut)
	if coResp.StatusCode != http.StatusOK || !strings.HasPrefix(filepath.Clean(coOut.Dir), filepath.Clean(explicitDataDir)) {
		t.Fatalf("RCA-RT-7 REGRESSION: checkout did not use Config.DataDir (%s): status=%d dir=%q body=%s", explicitDataDir, coResp.StatusCode, coOut.Dir, string(coBody))
	}

	// 7. F-05: /.well-known/nektar emits publicUrl (non-loopback) and omits fundAddress when Hub == nil; includes fundAddress when Hub != nil!
	wkResp := doReq(http.MethodGet, "/.well-known/nektar", "", "", "")
	var wk map[string]any
	_ = json.NewDecoder(wkResp.Body).Decode(&wk)
	wkResp.Body.Close()
	if wk["publicUrl"] != "https://node.example.com" {
		t.Fatalf("F-05: expected non-loopback publicUrl in /.well-known/nektar, got %+v", wk)
	}
	if _, hasFund := wk["fundAddress"]; hasFund {
		t.Fatalf("F-05 PRIVACY REGRESSION: non-Hub agent exposed fundAddress in /.well-known/nektar: %+v", wk)
	}

	hubNode, err := hub.NewNode(hub.Config{Network: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	defer hubNode.Close()
	hubApp, err := NewApp(mem, mkt, Config{Hub: hubNode, PublicURL: "http://127.0.0.1:18420"})
	if err != nil {
		t.Fatal(err)
	}
	wkHubRec := httptest.NewRecorder()
	hubApp.Handler().ServeHTTP(wkHubRec, httptest.NewRequest(http.MethodGet, "/.well-known/nektar", nil))
	var wkHub map[string]any
	_ = json.Unmarshal(wkHubRec.Body.Bytes(), &wkHub)
	if addr, _ := wkHub["fundAddress"].(string); addr == "" {
		t.Fatalf("F-05: expected Hub /.well-known/nektar to include fundAddress, got %+v", wkHub)
	}
	if _, hasPubURL := wkHub["publicUrl"]; hasPubURL {
		t.Fatalf("F-05: loopback PublicURL must not be emitted in /.well-known/nektar, got %+v", wkHub)
	}

	// 8. #7.4 + F-11: /mcp 401 emits RFC 9728 WWW-Authenticate + hint; /.well-known/oauth-protected-resource/mcp returns 200;
	// expired /pair returns styled HTML with 'nektar open'; wrong OAuth authorize token re-renders inline HTML error!
	mcp401 := doReq(http.MethodPost, "/mcp", "", "application/json", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	mcp401Body, _ := io.ReadAll(mcp401.Body)
	mcp401.Body.Close()
	if mcp401.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on unauth /mcp, got %d", mcp401.StatusCode)
	}
	wwwAuth := mcp401.Header.Get("WWW-Authenticate")
	if !strings.Contains(wwwAuth, `resource_metadata="https://node.example.com/.well-known/oauth-protected-resource"`) {
		t.Fatalf("#7.4 REGRESSION: /mcp 401 missing RFC 9728 WWW-Authenticate header, got %q", wwwAuth)
	}
	if !strings.Contains(string(mcp401Body), `"hint"`) {
		t.Fatalf("F-11 REGRESSION: 401 JSON missing actionable hint: %s", string(mcp401Body))
	}

	prmResp := doReq(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", "", "", "")
	prmResp.Body.Close()
	if prmResp.StatusCode != http.StatusOK {
		t.Fatalf("#7.4 REGRESSION: GET /.well-known/oauth-protected-resource/mcp status=%d", prmResp.StatusCode)
	}

	pair401 := doReq(http.MethodGet, "/pair?code=expired-bogus", "", "", "")
	pairBody, _ := io.ReadAll(pair401.Body)
	pair401.Body.Close()
	if pair401.StatusCode != http.StatusUnauthorized || !strings.Contains(pair401.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(pairBody), "nektar open") {
		t.Fatalf("F-11 REGRESSION: expired /pair should render HTML recovery bumper with 'nektar open', got %d %q %s", pair401.StatusCode, pair401.Header.Get("Content-Type"), string(pairBody))
	}

	// Register OAuth client and submit wrong token -> 401 inline HTML error with fresh CSRF cookie.
	regResp := doReq(http.MethodPost, "/oauth/register", "", "application/json", `{"client_name":"html-err-test","redirect_uris":["http://127.0.0.1/cb"]}`)
	var regOut struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(regResp.Body).Decode(&regOut)
	regResp.Body.Close()
	getAuth := doReq(http.MethodGet, "/oauth/authorize?client_id="+url.QueryEscape(regOut.ClientID)+"&redirect_uri="+url.QueryEscape("http://127.0.0.1/cb")+"&code_challenge=abc123&code_challenge_method=S256", "", "", "")
	var csrfVal string
	for _, c := range getAuth.Cookies() {
		if c.Name == "nektar_oauth_csrf" {
			csrfVal = c.Value
		}
	}
	getAuth.Body.Close()
	badForm := url.Values{
		"csrf_token":            {csrfVal},
		"client_id":             {regOut.ClientID},
		"redirect_uri":          {"http://127.0.0.1/cb"},
		"code_challenge":        {"abc123"},
		"code_challenge_method": {"S256"},
		"token":                 {"wrong-token"},
	}
	badPostReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/oauth/authorize", strings.NewReader(badForm.Encode()))
	badPostReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	badPostReq.AddCookie(&http.Cookie{Name: "nektar_oauth_csrf", Value: csrfVal})
	badPostResp, err := http.DefaultClient.Do(badPostReq)
	if err != nil {
		t.Fatal(err)
	}
	badPostBody, _ := io.ReadAll(badPostResp.Body)
	badPostResp.Body.Close()
	if badPostResp.StatusCode != http.StatusUnauthorized || !strings.Contains(badPostResp.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(badPostBody), "Invalid Nektar access token") {
		t.Fatalf("#7.4 REGRESSION: wrong OAuth token should render inline HTML error, got %d %q %s", badPostResp.StatusCode, badPostResp.Header.Get("Content-Type"), string(badPostBody))
	}
	_ = time.Now()
}
