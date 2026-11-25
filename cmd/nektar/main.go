package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	restateserver "github.com/restatedev/sdk-go/server"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/server"
	"github.com/kryptokrates/nektar/internal/store"
)

const Version = "0.4.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(0)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "version", "-v", "--version":
		fmt.Printf("nektar v%s (pure-Go single binary — Lightweb bcommon + Rúnar + Cedar + Mangle + Restate + MCP)\n", Version)
	case "serve":
		err = cmdServe(args)
	case "mcp":
		err = cmdMCP(args)
	case "hub":
		err = cmdHub(args)
	case "remember":
		err = cmdRemember(args)
	case "recall":
		err = cmdRecall(args)
	case "anchor":
		err = cmdAnchor(args)
	case "verify":
		err = cmdVerify(args)
	case "status":
		err = cmdStatus(args)
	case "check-mainnet":
		err = cmdCheckMainnet()
	case "dock":
		err = cmdDock(args)
	default:
		printUsage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "nektar %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`nektar v0.4 — Verifiable, encrypted agent memory anchored on BSV

Usage:
  nektar serve          Start unified HTTP + MCP + A2A + L0 LLM Proxy + Svelte Dashboard + optional Hub/Restate
  nektar mcp            Run stdio MCP server (for Claude Desktop / Claude Code / Codex / Cursor)
  nektar hub            Run standalone embedded Go Overlay Hub (tm_nektar_* + bfinger/bbox/blogs/borg/bsecret/bstore + SSE)
  nektar remember       Commit a fragment to a shard (-shard, -text, -anchor, -group)
  nektar recall         Recall fragments from a shard (-shard, -query, -spv)
  nektar anchor         Anchor unanchored fragments in a shard (-shard)
  nektar verify         Verify a nektarr1.* receipt token (-token)
  nektar status         Print node identity, wallet balance, shards, policy hash, and SpendGuard state
  nektar check-mainnet  Verify mainnet safety environment gates before live launch
  nektar dock           Print client MCP / L0 Proxy connection config for Claude, Codex, and OpenAI SDKs
  nektar version        Print build version`)
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envUint64(key string, fallback uint64) uint64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

func defaultDataDir() string {
	if v := os.Getenv("NEKTAR_DATA_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".nektar")
}

func openRuntime(ctx context.Context, networkOverride, dataDirOverride string) (*memory.Service, *market.Engine, func(), error) {
	net := networkOverride
	if net == "" {
		net = envOr("NEKTAR_NETWORK", "memory")
	}
	dir := dataDirOverride
	if dir == "" {
		dir = defaultDataDir()
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, nil, nil, err
	}
	dbPath := filepath.Join(dir, "nektar.sqlite")
	if net == "memory" && dataDirOverride == "" && os.Getenv("NEKTAR_DATA_DIR") == "" {
		dbPath = ":memory:"
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, nil, nil, err
	}
	port, err := chain.Open(ctx, chain.Config{
		Network:        net,
		DataDir:        dir,
		WalletMode:     envOr("NEKTAR_WALLET", "embedded"),
		WalletWireURL:  envOr("NEKTAR_WALLET_WIRE_URL", "http://127.0.0.1:3321"),
		HeaderSource:   os.Getenv("NEKTAR_HEADERS_SOURCE"),
		ChainSpec:      os.Getenv("NEKTAR_CHAIN_SPEC"),
		SettleSpec:     os.Getenv("NEKTAR_SETTLE_SPEC"),
		ArcadeKey:      os.Getenv("NEKTAR_ARCADE_KEY"),
		RPCUser:        os.Getenv("NEKTAR_RPC_USER"),
		RPCPass:        os.Getenv("NEKTAR_RPC_PASS"),
		WoCKey:         os.Getenv("NEKTAR_WOC_KEY"),
		MainnetConfirm: os.Getenv("NEKTAR_MAINNET_CONFIRM"),
		SpendGuard: core.SpendGuardOpts{
			MaxFeeSatsPerTx: envUint64("NEKTAR_MAX_FEE_PER_TX", 10_000),
			MaxSpendSatsDay: envUint64("NEKTAR_MAX_SPEND_PER_DAY", 2_000_000),
		},
	})
	if err != nil {
		_ = db.Close()
		return nil, nil, nil, err
	}
	objStore, err := bstack.NewFileObjectStore(dir)
	if err != nil {
		_ = db.Close()
		return nil, nil, nil, err
	}
	publicURL := strings.TrimRight(envOr("NEKTAR_PUBLIC_URL", "http://127.0.0.1:"+envOr("NEKTAR_HTTP_PORT", "8080")), "/")
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), objStore, memory.Config{
		VerifyBaseURL: publicURL + "/v1/verify",
		ModelID: reasoning.ModelIdentity{
			Kind:     "api",
			Provider: envOr("NEKTAR_MODEL_PROVIDER", "nektar"),
			Release:  envOr("NEKTAR_MODEL_RELEASE", Version),
		},
	})
	if err != nil {
		_ = db.Close()
		return nil, nil, nil, err
	}
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	return mem, mkt, func() { _ = db.Close() }, nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("listen", ":"+envOr("NEKTAR_HTTP_PORT", "8080"), "HTTP listen address")
	restateAddr := fs.String("restate-listen", os.Getenv("NEKTAR_RESTATE_LISTEN"), "Optional dedicated Restate HTTP/2 endpoint address (e.g. :9080)")
	withHub := fs.Bool("hub", os.Getenv("NEKTAR_EMBED_HUB") == "1", "Mount embedded Go Overlay Hub at /hub/")
	net := fs.String("network", "", "Network override (memory|regtest|testnet|main)")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mem, mkt, closeFn, err := openRuntime(ctx, *net, *dataDir)
	if err != nil {
		return err
	}
	defer closeFn()

	var hubNode *hub.Node
	if *withHub {
		hubNode, err = hub.NewNode(hub.Config{
			Network:       mem.Chain().Network(),
			EnableFinger:  true,
			EnableBbox:    true,
			EnableBlogs:   true,
			EnableBorg:    true,
			EnableBsecret: true,
			EnableBstore:  true,
			Getenv:        os.Getenv,
			ChainTracker:  mem.Chain().Tracker(),
		})
		if err != nil {
			return err
		}
	}

	app, err := server.NewApp(mem, mkt, server.Config{
		ListenAddr:        *addr,
		PublicURL:         os.Getenv("NEKTAR_PUBLIC_URL"),
		APIToken:          os.Getenv("NEKTAR_API_TOKEN"),
		OpenAIUpstream:    os.Getenv("NEKTAR_OPENAI_UPSTREAM"),
		AnthropicUpstream: os.Getenv("NEKTAR_ANTHROPIC_UPSTREAM"),
		Hub:               hubNode,
		MountRestate:      true,
	})
	if err != nil {
		return err
	}

	if *restateAddr != "" {
		go func() {
			log.Printf("nektar restate Go SDK endpoint listening on %s (BuyerDeal, SellerDeal, Custody, Tx, AnchorCadence)", *restateAddr)
			if rErr := mkt.BindRestate(restateserver.NewRestate()).Start(ctx, *restateAddr); rErr != nil && ctx.Err() == nil {
				log.Printf("restate server stopped: %v", rErr)
			}
		}()
		if adminURL := strings.TrimSpace(os.Getenv("NEKTAR_RESTATE_ADMIN")); adminURL != "" {
			epURI := envOr("NEKTAR_RESTATE_ENDPOINT_URI", "http://127.0.0.1"+*restateAddr)
			go func() {
				time.Sleep(500 * time.Millisecond)
				rc := market.NewRestateClient(os.Getenv("NEKTAR_RESTATE_URL"), adminURL)
				if err := rc.RegisterDeployment(ctx, epURI, true); err != nil {
					log.Printf("restate auto-register (%s -> %s): %v", adminURL, epURI, err)
				} else {
					log.Printf("restate Go SDK deployment registered (%s -> %s)", adminURL, epURI)
				}
			}()
		}
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Printf("nektar v%s listening on %s (network=%s, pub=%s)", Version, *addr, mem.Chain().Network(), mem.Chain().IdentityPubHex()[:16]+"...")
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	net := fs.String("network", "", "Network override")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mem, mkt, closeFn, err := openRuntime(ctx, *net, *dataDir)
	if err != nil {
		return err
	}
	defer closeFn()

	srv := server.NewMCPServer(mem, mkt)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

func cmdHub(args []string) error {
	fs := flag.NewFlagSet("hub", flag.ExitOnError)
	addr := fs.String("listen", ":"+envOr("NEKTAR_HUB_PORT", "8081"), "Hub HTTP listen address")
	net := fs.String("network", envOr("NEKTAR_NETWORK", "regtest"), "Network name")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	node, err := hub.NewNode(hub.Config{
		Network:       *net,
		EnableFinger:  true,
		EnableBbox:    true,
		EnableBlogs:   true,
		EnableBorg:    true,
		EnableBsecret: true,
		EnableBstore:  true,
		Getenv:        os.Getenv,
	})
	if err != nil {
		return err
	}
	httpSrv := &http.Server{Addr: *addr, Handler: node.Handler(), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()
	log.Printf("nektar hub v%s listening on %s (topics=%d lookups=%d)", Version, *addr, len(node.Topics()), len(node.Lookups()))
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func cmdRemember(args []string) error {
	fs := flag.NewFlagSet("remember", flag.ExitOnError)
	shard := fs.String("shard", "default", "Target shard name")
	text := fs.String("text", "", "Memory fragment text")
	group := fs.String("group", "", "Optional Borg group ID for BRC-369 encryption")
	anchor := fs.Bool("anchor", false, "Anchor immediately on BSV")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	ctx := context.Background()
	mem, _, closeFn, err := openRuntime(ctx, "", *dataDir)
	if err != nil {
		return err
	}
	defer closeFn()

	res, err := mem.Remember(ctx, memory.RememberInput{
		Shard:      *shard,
		Text:       *text,
		BorgGroup:  *group,
		AutoAnchor: *anchor,
	})
	if err != nil {
		return err
	}
	return printJSON(res)
}

func cmdRecall(args []string) error {
	fs := flag.NewFlagSet("recall", flag.ExitOnError)
	shard := fs.String("shard", "default", "Target shard name")
	query := fs.String("query", "", "Filter query")
	spv := fs.Bool("spv", false, "Verify SPV proofs offline")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	ctx := context.Background()
	mem, _, closeFn, err := openRuntime(ctx, "", *dataDir)
	if err != nil {
		return err
	}
	defer closeFn()

	res, err := mem.Recall(ctx, memory.RecallInput{Shard: *shard, Query: *query, VerifySPV: *spv})
	if err != nil {
		return err
	}
	return printJSON(res)
}

func cmdAnchor(args []string) error {
	fs := flag.NewFlagSet("anchor", flag.ExitOnError)
	shard := fs.String("shard", "default", "Target shard name")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	ctx := context.Background()
	mem, _, closeFn, err := openRuntime(ctx, "", *dataDir)
	if err != nil {
		return err
	}
	defer closeFn()

	res, err := mem.AnchorShard(ctx, *shard)
	if err != nil {
		return err
	}
	return printJSON(res)
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	token := fs.String("token", "", "Receipt token (nektarr1....) or 64-hex hash")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	ctx := context.Background()
	mem, _, closeFn, err := openRuntime(ctx, "", *dataDir)
	if err != nil {
		return err
	}
	defer closeFn()

	res, err := mem.VerifyReceipt(ctx, *token)
	if err != nil {
		return err
	}
	return printJSON(res)
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	ctx := context.Background()
	mem, mkt, closeFn, err := openRuntime(ctx, "", *dataDir)
	if err != nil {
		return err
	}
	defer closeFn()
	return printJSON(server.BuildStatus(ctx, mem, mkt))
}

func cmdCheckMainnet() error {
	var missing []string
	if os.Getenv("NEKTAR_MAINNET_CONFIRM") != "I_UNDERSTAND" {
		missing = append(missing, "NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND")
	}
	if os.Getenv("NEKTAR_HEADERS_SOURCE") == "" {
		missing = append(missing, "NEKTAR_HEADERS_SOURCE (e.g. chaintracks:https://... or woc:main)")
	}
	if os.Getenv("NEKTAR_SETTLE_SPEC") == "" {
		missing = append(missing, "NEKTAR_SETTLE_SPEC (e.g. arcade:https://arcade.gorillapool.io)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("mainnet gate incomplete — set: %s", strings.Join(missing, ", "))
	}
	fmt.Println("mainnet gates OK: confirmation, headers source, settlement spec, and SpendGuard configured")
	return nil
}

func cmdDock(args []string) error {
	fs := flag.NewFlagSet("dock", flag.ExitOnError)
	url := fs.String("url", "http://127.0.0.1:"+envOr("NEKTAR_HTTP_PORT", "8080"), "Nektar HTTP base URL")
	_ = fs.Parse(args)
	u := strings.TrimRight(*url, "/")
	fmt.Printf(`Nektar v%s client docking snippets:

1) Claude Code / Cursor / Codex (stdio MCP):
   { "mcpServers": { "nektar": { "command": "nektar", "args": ["mcp"] } } }

2) Streamable HTTP MCP endpoint:
   %s/mcp

3) Zero-code L0 OpenAI SDK Memory Proxy:
   OPENAI_BASE_URL=%s/v1/proxy/openai/v1

4) Zero-code L0 Anthropic SDK Memory Proxy:
   ANTHROPIC_BASE_URL=%s/v1/proxy/anthropic

5) A2A Agent Card & Dashboard:
   %s/.well-known/agent.json
   %s/dashboard
`, Version, u, u, u, u, u)
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
