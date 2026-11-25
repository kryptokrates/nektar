package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
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
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/server"
	"github.com/kryptokrates/nektar/internal/store"
)

const (
	Version          = "0.4.0"
	DefaultLocalPort = "18420"
)

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
	case "doctor":
		err = cmdDoctor(args)
	case "peers":
		err = cmdPeers(args)
	case "want":
		err = cmdWant(args)
	case "sell":
		err = cmdSell(args)
	case "policy":
		err = cmdPolicy(args)
	case "dock":
		err = cmdDock(args)
	case "hook":
		err = cmdHook(args)
	case "open":
		err = cmdOpen(args)
	case "check-mainnet":
		err = cmdCheckMainnet()
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
	fmt.Println(`nektar v0.4 — Verifiable, encrypted agent memory & autonomous knowledge market on BSV

Usage:
  nektar serve          Start unified HTTP + MCP + A2A + L0 Proxy + Market Broker + Svelte Dashboard (default :18420)
  nektar mcp            Run stdio MCP server (auto-bridges to live daemon if running; zero lock contention!)
  nektar hub            Run standalone durable SQLite Overlay Hub (16 lanes + bfinger + SSE + NAT bbox + bounties)
  nektar dock           Auto-detect & configure Claude Code, Cursor, Codex, VS Code, Windsurf, Hermes + live probe
  nektar open           Open the Nektar Dashboard in your browser
  nektar doctor         Run full node health diagnosis with exact next steps
  nektar policy         Manage operator spending/selling policies (approve --preset buyer|seller|both | list | revoke)
  nektar peers          Discover verified peers, bfinger handles, advertised CHIRP packs & Hub bounties
  nektar want           Buy verified knowledge live from peers or the Hub (-domains, -question, -max)
  nektar sell           Arm a local shard for A2A selling (-shard, -domains, -price) or sell to Hub (-bounty)
  nektar hook           Native zero-Node Claude Code / Codex hooks (session-start | prefetch | verify-receipt)
  nektar remember       Commit a fragment to a shard (-shard, -text, -anchor, -group)
  nektar recall         Recall fragments (-shard optional, -query, -spv)
  nektar anchor         Anchor unanchored fragments in a shard (-shard)
  nektar verify         Verify a nektarr1.* receipt token (-token)
  nektar status         Print node identity, wallet balance, shards, policy hash, and SpendGuard state
  nektar check-mainnet  Verify mainnet safety environment gates before live launch
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

func parseCSVEnv(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func defaultDataDir() string {
	if v := os.Getenv("NEKTAR_DATA_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".nektar")
}

type runtimeBundle struct {
	mem     *memory.Service
	mkt     *market.Engine
	disc    *discovery.Service
	dataDir string
	closeFn func()
}

func openRuntime(ctx context.Context, networkOverride, dataDirOverride string, listenPort string) (*runtimeBundle, error) {
	net := networkOverride
	if net == "" {
		net = envOr("NEKTAR_NETWORK", "memory")
	}
	dir := dataDirOverride
	if dir == "" {
		dir = defaultDataDir()
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dir, "nektar.sqlite")
	if net == "memory" && dataDirOverride == "" && os.Getenv("NEKTAR_DATA_DIR") == "" {
		dbPath = ":memory:"
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	objStore, err := bstack.NewFileObjectStore(dir)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if listenPort == "" {
		listenPort = envOr("NEKTAR_HTTP_PORT", DefaultLocalPort)
	}
	publicURL := strings.TrimRight(envOr("NEKTAR_PUBLIC_URL", "http://127.0.0.1:"+listenPort), "/")
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
		return nil, err
	}
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	_ = market.SyncPolicyFromDB(db, mkt.Policy())
	disc := discovery.New(mem, parseCSVEnv("NEKTAR_MARKET_HOSTS"), nil)
	return &runtimeBundle{
		mem:     mem,
		mkt:     mkt,
		disc:    disc,
		dataDir: dir,
		closeFn: func() { _ = db.Close() },
	}, nil
}

func extractPort(listenAddr string) (int, string) {
	_, portStr, err := net.SplitHostPort(listenAddr)
	if err != nil {
		portStr = strings.TrimPrefix(listenAddr, ":")
	}
	p, _ := strconv.Atoi(portStr)
	if p == 0 {
		p = 18420
		portStr = DefaultLocalPort
	}
	return p, portStr
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("listen", ":"+envOr("NEKTAR_HTTP_PORT", DefaultLocalPort), "HTTP listen address")
	restateAddr := fs.String("restate-listen", os.Getenv("NEKTAR_RESTATE_LISTEN"), "Optional dedicated Restate HTTP/2 endpoint address (e.g. :9080)")
	withHub := fs.Bool("hub", os.Getenv("NEKTAR_EMBED_HUB") == "1", "Mount embedded durable SQLite Go Overlay Hub at /hub/ with starter liquidity & bounties")
	netName := fs.String("network", "", "Network override (memory|regtest|testnet|main)")
	dataDir := fs.String("data-dir", "", "Data directory override")
	publicURLFlag := fs.String("public-url", os.Getenv("NEKTAR_PUBLIC_URL"), "Publicly reachable HTTP base URL for direct A2A trades (empty = NAT bbox fallback)")
	hostsFlag := fs.String("hosts", os.Getenv("NEKTAR_MARKET_HOSTS"), "Comma-separated market hub URLs to join")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	portNum, portStr := extractPort(*addr)
	rt, err := openRuntime(ctx, *netName, *dataDir, portStr)
	if err != nil {
		return err
	}
	defer rt.closeFn()

	// Cold-start zero-empty-room welcome shard + live UTXO sync & fan-out!
	if _, err := rt.mem.EnsureWelcome(ctx); err != nil {
		log.Printf("welcome seed warning: %v", err)
	}
	if rt.mem.Chain().Network() != "memory" {
		// Only block boot for SyncPool if wallet pool is completely empty; otherwise sync asynchronously
		// so daemon restarts recover in <500ms even under WhatsOnChain rate limits!
		if rt.mem.Chain().BalanceSats() == 0 {
			syncCtx, syncCancel := context.WithTimeout(ctx, 8*time.Second)
			if added, proved, sErr := rt.mem.Chain().SyncPool(syncCtx); sErr == nil && (added > 0 || proved > 0) {
				log.Printf("synced %s wallet pool: +%d UTXO(s), %d proved (balance=%d sats)", rt.mem.Chain().Network(), added, proved, rt.mem.Chain().BalanceSats())
			}
			syncCancel()
		}
		go func() {
			// Background initial sync + periodic 30s pool/proof refresh.
			if rt.mem.Chain().BalanceSats() > 0 {
				_, _, _ = rt.mem.Chain().SyncPool(ctx)
				_, _ = rt.mem.RefreshAnchorProofs(ctx)
			}
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					_, _, _ = rt.mem.Chain().SyncPool(ctx)
					_, _ = rt.mem.RefreshAnchorProofs(ctx)
				}
			}
		}()
	}
	if rt.mem.Chain().BalanceSats() > 10_000 {
		if err := rt.mem.Chain().EnsureSplitCoins(ctx, 6, 5000); err != nil {
			log.Printf("coin split warning: %v", err)
		}
	}

	// Optionally mount durable embedded Go Overlay Hub at /hub/ with Liquidity Engine + Bounties + Faucet!
	var hubNode *hub.Node
	var liqState *hub.LiquidityState
	if *withHub {
		hubDBPath := filepath.Join(rt.dataDir, "hub.sqlite")
		if rt.mem.Chain().Network() == "memory" && *dataDir == "" && os.Getenv("NEKTAR_DATA_DIR") == "" {
			hubDBPath = ":memory:"
		}
		hubNode, err = hub.NewNode(hub.Config{
			Network:       rt.mem.Chain().Network(),
			DBPath:        hubDBPath,
			EnableFinger:  true,
			EnableBbox:    true,
			EnableBlogs:   true,
			EnableBorg:    true,
			EnableBsecret: true,
			EnableBstore:  true,
			Getenv:        os.Getenv,
			ChainTracker:  rt.mem.Chain().Tracker(),
		})
		if err != nil {
			return err
		}
		defer hubNode.Close()
		liqState, err = hub.ArmHubLiquidity(ctx, hubNode, rt.mem, hub.LiquidityConfig{
			SeedStarterShards: true,
			ArmBounties:       true,
			StarterPriceSats:  90,
			BountyRewardSats:  100,
		})
		if err != nil {
			log.Printf("hub liquidity warning: %v", err)
		}
		if peers := parseCSVEnv("NEKTAR_PEER_HUBS"); len(peers) > 0 {
			hubNode.StartFederation(ctx, peers)
		}
	}

	localBase := fmt.Sprintf("http://127.0.0.1:%d", portNum)
	pubURL := strings.TrimRight(strings.TrimSpace(*publicURLFlag), "/")
	if pubURL == "" && *withHub {
		pubURL = localBase
	}

	// Merge any CLI --hosts into discovery!
	if strings.TrimSpace(*hostsFlag) != "" {
		var extra []string
		for _, h := range strings.Split(*hostsFlag, ",") {
			if h = strings.TrimSpace(h); h != "" {
				extra = append(extra, h)
			}
		}
		rt.disc = discovery.New(rt.mem, extra, nil)
	}

	var appRef *server.App
	broker := market.NewBroker(rt.mem, rt.mkt, rt.disc, market.BrokerConfig{
		PublicURL:  pubURL,
		BboxOffice: envOr("NEKTAR_BBOX_OFFICE", "default"),
		Handle:     os.Getenv("NEKTAR_HANDLE"),
		IsHub:      *withHub,
		OnStateChange: func(ev string, _ any) {
			if appRef != nil {
				appRef.BroadcastDashboardEvent(ev)
			}
		},
	})
	if liqState != nil {
		broker.SetAdvertisedPacks(liqState.Packs, liqState.BountyDomains)
	}

	apiTok := envOr("NEKTAR_API_TOKEN", os.Getenv("NEKTAR_HTTP_TOKEN"))
	opTok := os.Getenv("NEKTAR_OPERATOR_TOKEN")
	app, err := server.NewApp(rt.mem, rt.mkt, server.Config{
		ListenAddr:        *addr,
		PublicURL:         pubURL,
		APIToken:          apiTok,
		OperatorToken:     opTok,
		ToolProfile:       envOr("NEKTAR_TOOL_PROFILE", "operator"),
		OpenAIUpstream:    os.Getenv("NEKTAR_OPENAI_UPSTREAM"),
		AnthropicUpstream: os.Getenv("NEKTAR_ANTHROPIC_UPSTREAM"),
		Hub:               hubNode,
		Discovery:         rt.disc,
		Broker:            broker,
		MountRestate:      true,
		RestateAdminURL:   os.Getenv("NEKTAR_RESTATE_ADMIN"),
	})
	if err != nil {
		return err
	}
	appRef = app

	if *restateAddr != "" {
		go func() {
			log.Printf("nektar restate Go SDK endpoint listening on %s (BuyerDeal, SellerDeal, Custody, Tx, AnchorCadence)", *restateAddr)
			if rErr := rt.mkt.BindRestate(restateserver.NewRestate()).Start(ctx, *restateAddr); rErr != nil && ctx.Err() == nil {
				log.Printf("restate server stopped: %v", rErr)
			}
		}()
		if adminURL := strings.TrimSpace(os.Getenv("NEKTAR_RESTATE_ADMIN")); adminURL != "" {
			epURI := envOr("NEKTAR_RESTATE_ENDPOINT_URI", "http://127.0.0.1"+*restateAddr)
			go func() {
				time.Sleep(500 * time.Millisecond)
				rc := market.NewRestateClient(os.Getenv("NEKTAR_RESTATE_URL"), adminURL)
				if changed, err := rc.EnsureDeployment(ctx, epURI, false); err != nil {
					log.Printf("restate auto-register (%s -> %s): %v", adminURL, epURI, err)
				} else if changed {
					log.Printf("restate Go SDK deployment registered (%s -> %s)", adminURL, epURI)
				}
			}()
		}
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	actualURL := fmt.Sprintf("http://127.0.0.1:%d", actualPort)
	if *withHub && pubURL == localBase {
		broker.SetPublicURL(actualURL)
	}

	// Write daemon.json so `nektar mcp`, `nektar open`, `nektar hook`, `nektar dock`, and Hermes auto-discover us!
	_ = server.WriteDaemonInfo(rt.dataDir, server.DaemonInfo{
		PID:       os.Getpid(),
		Port:      actualPort,
		URL:       actualURL,
		Network:   rt.mem.Chain().Network(),
		DataDir:   rt.dataDir,
		AuthToken: apiTok,
		StartedAt: time.Now().Unix(),
	})
	defer server.RemoveDaemonInfo(rt.dataDir)

	httpSrv := &http.Server{
		Handler:           app.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	// Once listener is live, self-join embedded Hub, start Broker SSE/poll/bbox loops, announce on `dir`, and seed packs!
	go func() {
		time.Sleep(100 * time.Millisecond)
		if *withHub {
			_, _, _ = rt.disc.JoinHost(ctx, actualURL+"/hub")
		}
		broker.Start(ctx)
		_, _ = broker.AnnounceSelf(ctx)
		if seedDoms := parseCSVEnv("NEKTAR_SEED_DOMAINS"); len(seedDoms) > 0 {
			if n, sErr := rt.disc.SeedFromNetwork(ctx, seedDoms); sErr == nil && n > 0 {
				log.Printf("seeded %d verified fragment(s) from network CHIRP packs", n)
			}
		}
	}()

	log.Printf("nektar v%s listening on %s (network=%s, pub=%s, hub=%v)", Version, actualURL, rt.mem.Chain().Network(), rt.mem.Chain().IdentityPubHex()[:16]+"...", *withHub)
	if apiTok != "" || opTok != "" {
		log.Printf("dashboard 1-click pairing URL: %s/pair?code=%s", actualURL, app.CreatePairCode())
	}
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	netName := fs.String("network", "", "Network override")
	dataDir := fs.String("data-dir", "", "Data directory override")
	direct := fs.Bool("direct", false, "Force in-process stdio MCP instead of bridging to a running daemon")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dir := *dataDir
	if dir == "" {
		dir = defaultDataDir()
	}
	// One brain, many mouths: if a live `nektar serve` daemon is running, transparently bridge stdio -> /mcp!
	if !*direct {
		if info, ok := server.DetectLiveDaemon(dir); ok {
			return server.RunStdioBridge(ctx, info.URL, info.AuthToken, os.Stdin, os.Stdout)
		}
	}

	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	_, _ = rt.mem.EnsureWelcome(ctx)

	broker := market.NewBroker(rt.mem, rt.mkt, rt.disc, market.BrokerConfig{})
	tb := server.NewToolbox(rt.mem, rt.mkt, broker, rt.disc, nil, envOr("NEKTAR_TOOL_PROFILE", "agent"))
	srv := server.NewMCPServerWithToolbox(tb)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

func cmdHub(args []string) error {
	fs := flag.NewFlagSet("hub", flag.ExitOnError)
	addr := fs.String("listen", ":"+envOr("NEKTAR_HUB_PORT", "18421"), "Hub HTTP listen address")
	netName := fs.String("network", envOr("NEKTAR_NETWORK", "memory"), "Network name")
	dataDir := fs.String("data-dir", "", "Data directory for hub.sqlite")
	peersFlag := fs.String("peers", os.Getenv("NEKTAR_PEER_HUBS"), "Comma-separated sibling Hub URLs to federate with")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dir := *dataDir
	if dir == "" {
		dir = defaultDataDir()
	}
	dbPath := filepath.Join(dir, "hub.sqlite")
	node, err := hub.NewNode(hub.Config{
		Network:       *netName,
		DBPath:        dbPath,
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
	defer node.Close()

	if strings.TrimSpace(*peersFlag) != "" {
		var peers []string
		for _, p := range strings.Split(*peersFlag, ",") {
			if p = strings.TrimSpace(p); p != "" {
				peers = append(peers, p)
			}
		}
		node.StartFederation(ctx, peers)
	}

	httpSrv := &http.Server{Addr: *addr, Handler: node.Handler(), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()
	log.Printf("nektar hub v%s listening on %s (db=%s topics=%d lookups=%d)", Version, *addr, dbPath, len(node.Topics()), len(node.Lookups()))
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
	rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()

	res, err := rt.mem.Remember(ctx, memory.RememberInput{
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
	shard := fs.String("shard", "", "Optional shard name (omit to recall across all shards)")
	query := fs.String("query", "", "Filter query")
	spv := fs.Bool("spv", false, "Verify SPV proofs offline")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	ctx := context.Background()
	rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()

	shards, items, err := rt.mem.RecallAllShards(ctx, memory.RecallInput{Shard: *shard, Query: *query, VerifySPV: *spv})
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"shards": shards, "items": items, "count": len(items)})
}

func cmdAnchor(args []string) error {
	fs := flag.NewFlagSet("anchor", flag.ExitOnError)
	shard := fs.String("shard", "default", "Target shard name")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	ctx := context.Background()
	rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()

	res, err := rt.mem.AnchorShard(ctx, *shard)
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
	rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()

	res, err := rt.mem.VerifyReceipt(ctx, *token)
	if err != nil {
		return err
	}
	return printJSON(res)
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	dir := *dataDir
	if dir == "" {
		dir = defaultDataDir()
	}
	if info, ok := server.DetectLiveDaemon(dir); ok {
		if out, err := callLiveTool(info, "status", map[string]any{}); err == nil {
			return printJSON(out)
		}
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	return printJSON(server.BuildStatus(ctx, rt.mem, rt.mkt))
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

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
