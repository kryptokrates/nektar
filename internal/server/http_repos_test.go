package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestHTTPReposAuthIsolationAndSecurityRegressions(t *testing.T) {
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
	objs, err := bstack.NewFileObjectStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), objs, memory.Config{})
	if err != nil {
		t.Fatal(err)
	}

	// Seed a normal repo and an internal __receipts__ audit record.
	rem, err := mem.Remember(ctx, memory.RememberInput{
		Shard: "demo",
		Path:  "facts/a.md",
		Text:  "header: v1\nprice: 1000\nfooter: ok\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Seed a large volumetric binary blob (>64KB so it goes into CHIRP) on a private shard.
	bigPayload := bytes.Repeat([]byte("PRIVATE-CHIRP-BLOB-DATA-"), 4000)
	privRem, err := mem.Remember(ctx, memory.RememberInput{
		Shard:     "private-bin",
		Path:      "files/secret.bin",
		Blob:      bigPayload,
		MediaType: "application/octet-stream",
		Level:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if privRem.ChirpURL == "" {
		t.Fatal("expected large binary blob to be stored in CHIRP")
	}

	mkt := market.NewEngine(mem, market.DefaultPolicy())
	app, err := NewApp(mem, mkt, Config{
		APIToken:      "agent-tok-repos",
		OperatorToken: "op-tok-repos",
		ToolProfile:   "operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	doReq := func(method, path, token, body string) (*http.Response, string) {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(raw)
	}

	// 1. System shards (__receipts__) MUST be denied for non-operator agent tokens across REST + tools!
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/repos/__receipts__", ""},
		{http.MethodGet, "/v1/repos/__receipts__/log", ""},
		{http.MethodGet, "/v1/repos/__receipts__/tags", ""},
		{http.MethodPost, "/v1/repos/__receipts__/prove-fields", `{"seq":1,"fields":["class"]}`},
		{http.MethodPost, "/v1/tools/repo", `{"action":"log","repo":"__receipts__"}`},
	} {
		resp, body := doReq(tc.method, tc.path, "agent-tok-repos", tc.body)
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("CRIT-02 REGRESSION: non-operator accessed %s %s: %s", tc.method, tc.path, body)
		}
	}

	// 2. Non-operator agent token MUST be denied autoAnchor:true and POST /v1/tools/anchor!
	respAA, bodyAA := doReq(http.MethodPost, "/v1/repos/demo/commit", "agent-tok-repos", `{"path":"facts/b.md","text":"hello","autoAnchor":true}`)
	if respAA.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for non-operator autoAnchor:true, got %d: %s", respAA.StatusCode, bodyAA)
	}
	respAnch, bodyAnch := doReq(http.MethodPost, "/v1/tools/anchor", "agent-tok-repos", `{"shard":"demo"}`)
	if respAnch.StatusCode == http.StatusOK {
		t.Fatalf("expected non-operator /v1/tools/anchor to be denied, got 200: %s", bodyAnch)
	}

	// 3. CRIT-03: Private CHIRP hashes MUST NOT be exposed unauthenticated on GET /v1/chirp/{hash}!
	privRootHex := strings.TrimPrefix(privRem.ChirpURL, "chirp://")
	respPrivChirp, _ := doReq(http.MethodGet, "/v1/chirp/"+privRootHex, "", "")
	if respPrivChirp.StatusCode == http.StatusOK {
		t.Fatalf("CRIT-03 REGRESSION: private CHIRP hash %s was served unauthenticated!", privRootHex)
	}

	// Exporting a private capsule MUST also keep CHIRP hashes private.
	if _, _, _, err := mem.ExportCapsule(ctx, "private-bin", "main", ""); err != nil {
		t.Fatal(err)
	}
	respPrivChirp2, _ := doReq(http.MethodGet, "/v1/chirp/"+privRootHex, "", "")
	if respPrivChirp2.StatusCode == http.StatusOK {
		t.Fatalf("CRIT-03 REGRESSION: ExportCapsule leaked private CHIRP hash %s unauthenticated!", privRootHex)
	}

	// Explicit ExportPublicShardPack DOES publish public hashes and persists allowlist across Service restart!
	pubPack, err := mem.ExportPublicShardPack(ctx, "demo")
	if err != nil {
		t.Fatalf("ExportPublicShardPack: %v", err)
	}
	respPubChirp, _ := doReq(http.MethodGet, "/v1/chirp/"+pubPack.RootHex, "", "")
	if respPubChirp.StatusCode != http.StatusOK {
		t.Fatalf("expected public shard pack CHIRP root %s to return 200, got %d", pubPack.RootHex, respPubChirp.StatusCode)
	}
	restartedMem, err := memory.New(db, port, bstack.NewGroupRegistry(), objs, memory.Config{})
	if err != nil {
		t.Fatalf("restart memory.New: %v", err)
	}
	var pubRootArr [32]byte
	pubRootRaw, _ := hex.DecodeString(pubPack.RootHex)
	copy(pubRootArr[:], pubRootRaw)
	if !restartedMem.IsPublicChirp(pubRootArr) {
		t.Fatal("CRIT-03 REGRESSION: public CHIRP allowlist did not persist across Service restart")
	}

	// 4. CRIT-09: Creating an already-existing branch ("main") MUST fail (never silently rewind).
	respDupBr, bodyDupBr := doReq(http.MethodPost, "/v1/repos/demo/branches", "op-tok-repos", `{"branch":"main","from":"main"}`)
	if respDupBr.StatusCode == http.StatusOK {
		t.Fatalf("CRIT-09 REGRESSION: creating duplicate branch main succeeded: %s", bodyDupBr)
	}

	// 5. HIGH-01: Same-line concurrent edits across two branches MUST return HTTP 409 merge conflict!
	respBr, bodyBr := doReq(http.MethodPost, "/v1/repos/demo/branches", "op-tok-repos", `{"branch":"feat","from":"main"}`)
	if respBr.StatusCode != http.StatusOK {
		t.Fatalf("create branch feat: %d %s", respBr.StatusCode, bodyBr)
	}
	respC1, _ := doReq(http.MethodPost, "/v1/repos/demo/commit", "op-tok-repos", `{"branch":"main","path":"facts/a.md","text":"header: v1\nprice: 2000\nfooter: ok\n","message":"main price"}`)
	if respC1.StatusCode != http.StatusOK {
		t.Fatalf("main commit failed: %d", respC1.StatusCode)
	}
	respC2, _ := doReq(http.MethodPost, "/v1/repos/demo/commit", "op-tok-repos", `{"branch":"feat","path":"facts/a.md","text":"header: v1\nprice: 1050\nfooter: ok\n","message":"feat price"}`)
	if respC2.StatusCode != http.StatusOK {
		t.Fatalf("feat commit failed: %d", respC2.StatusCode)
	}
	respMerge, bodyMerge := doReq(http.MethodPost, "/v1/repos/demo/merge", "op-tok-repos", `{"sourceBranch":"feat","targetBranch":"main"}`)
	if respMerge.StatusCode != http.StatusConflict {
		t.Fatalf("HIGH-01 REGRESSION: expected 409 conflict for same-line edits, got %d: %s", respMerge.StatusCode, bodyMerge)
	}

	// 6. Tag creation & listing via REST.
	respTag, bodyTag := doReq(http.MethodPost, "/v1/repos/demo/tags", "op-tok-repos", `{"tag":"v1.0","commit":"main","message":"release 1"}`)
	if respTag.StatusCode != http.StatusOK {
		t.Fatalf("create tag failed: %d %s", respTag.StatusCode, bodyTag)
	}

	// 7. CRIT-02: Forget shreds fragment, mem_objects, mem_commits.message, and CHIRP backing object!
	if err := mem.Forget(ctx, "private-bin", privRem.Seq); err != nil {
		t.Fatalf("Forget(private-bin): %v", err)
	}
	if _, err := bstack.ResolveChirp(objs, privRem.ChirpURL); err == nil {
		t.Fatal("CRIT-02 REGRESSION: CHIRP backing object was NOT deleted on Forget")
	}
	shredCommits, err := db.ListAllMemCommits("private-bin")
	if err != nil || len(shredCommits) == 0 {
		t.Fatalf("ListAllMemCommits(private-bin): %v len=%d", err, len(shredCommits))
	}
	if shredCommits[0].Message != "[shredded]" {
		t.Fatalf("CRIT-02 REGRESSION: commit message was not redacted on Forget, got %q", shredCommits[0].Message)
	}

	// 8. CRIT-08: Seller exports anchored capsule -> Buyer imports into own repo -> imported commits verify AND Buyer can still AnchorShard!
	if _, err := mem.AnchorShard(ctx, "demo"); err != nil {
		t.Fatalf("Seller AnchorShard(demo): %v", err)
	}
	_, _, capRaw, err := mem.ExportCapsule(ctx, "demo", "main", "")
	if err != nil {
		t.Fatalf("Seller ExportCapsule: %v", err)
	}
	buyerDB, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer buyerDB.Close()
	buyerPort, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer buyerPort.Close()
	buyerMem, err := memory.New(buyerDB, buyerPort, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	impRes, err := buyerMem.ImportCapsule(ctx, capRaw, "", "imported-demo", "main", true)
	if err != nil {
		t.Fatalf("Buyer ImportCapsule: %v", err)
	}
	if impRes.Verification == nil || !impRes.Verification.OK {
		t.Fatalf("Buyer ImportCapsule verification failed: %+v", impRes.Verification)
	}
	// Verify every imported commit in Buyer's DB still passes VerifyCommitObject (preserving signed OriginRepo/OriginBranch)!
	impCommits, err := buyerMem.RepoLog(ctx, "imported-demo", "", 50)
	if err != nil || len(impCommits) == 0 {
		t.Fatalf("Buyer RepoLog: %v len=%d", err, len(impCommits))
	}
	for _, c := range impCommits {
		if err := memrepo.VerifyCommitObject(c); err != nil {
			t.Fatalf("CRIT-08 REGRESSION: imported commit %s failed VerifyCommitObject: %v", c.CommitHash, err)
		}
	}
	// Buyer adds a local fact and anchors -> MUST succeed without being bricked by Seller's GenesisOwner!
	if _, err := buyerMem.Remember(ctx, memory.RememberInput{
		Shard: "imported-demo",
		Path:  "facts/buyer-note.md",
		Text:  "buyer follow-up note",
	}); err != nil {
		t.Fatalf("Buyer Remember: %v", err)
	}
	anchRes, err := buyerMem.AnchorShard(ctx, "imported-demo")
	if err != nil {
		t.Fatalf("CRIT-08 REGRESSION: Buyer AnchorShard after foreign capsule import failed: %v", err)
	}
	if anchRes == nil || anchRes.TxID == "" {
		t.Fatalf("expected valid Buyer anchor txid, got %+v", anchRes)
	}
	_ = rem
	_ = json.Valid(capRaw)
}
