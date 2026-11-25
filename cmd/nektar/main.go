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
	case "wallet":
		err = cmdWallet(args)
	case "dock":
		err = cmdDock(args)
	case "hook":
		err = cmdHook(args)
	case "open":
		err = cmdOpen(args)
	case "restate-up":
		err = cmdRestateUp(args)
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
  nektar wallet         Manage BRC-100 wallet (status | address | utxos | actions | sync | fund | import | send | sweep | consolidate | fuel)
  nektar restate-up     Download SHA-256 verified restate-server v1.8.1 into ~/.nektar/bin for local durable workflows
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
		net = envOr("NEKTAR_NETWORK", "testnet")
	}
	dir := dataDirOverride
	if dir == "" {
		dir = defaultDataDir()
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dir, "nektar.sqlite")
	if os.Getenv("NEKTAR_EPHEMERAL") == "1" || (net == "memory" && dataDirOverride == "" && os.Getenv("NEKTAR_DATA_DIR") == "") {
		dbPath = ":memory:"
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	maxFee := envUint64("NEKTAR_MAX_FEE_PER_TX", 0)
	if maxFee == 0 {
		maxFee = envUint64("NEKTAR_MAX_FEE_SATS_PER_TX", core.DefaultMaxFeeSatsPerTx)
	}
	maxDay := envUint64("NEKTAR_MAX_SPEND_PER_DAY", 0)
	if maxDay == 0 {
		maxDay = envUint64("NEKTAR_MAX_SPEND_SATS_PER_DAY", core.DefaultMaxSpendSatsDay)
	}
	minIntervalMs := envUint64("NEKTAR_MIN_BROADCAST_INTERVAL_MS", 0)
	port, err := chain.Open(ctx, chain.Config{
		Network:         net,
		DataDir:         dir,
		WalletMode:      envOr("NEKTAR_WALLET", "embedded"),
		WalletWireURL:   envOr("NEKTAR_WALLET_WIRE_URL", "http://127.0.0.1:3321"),
		WalletProfile:   envOr("NEKTAR_WALLET_PROFILE", "standard"),
		FeeRateSatPerKB: envUint64("NEKTAR_FEE_RATE_SAT_KB", 0),
		HeaderSource:    os.Getenv("NEKTAR_HEADERS_SOURCE"),
		ChainSpec:       os.Getenv("NEKTAR_CHAIN_SPEC"),
		SettleSpec:      os.Getenv("NEKTAR_SETTLE_SPEC"),
		ArcadeKey:       os.Getenv("NEKTAR_ARCADE_KEY"),
		RPCUser:         os.Getenv("NEKTAR_RPC_USER"),
		RPCPass:         os.Getenv("NEKTAR_RPC_PASS"),
		WoCKey:          os.Getenv("NEKTAR_WOC_KEY"),
		MainnetConfirm:  os.Getenv("NEKTAR_MAINNET_CONFIRM"),
		SpendGuard: core.SpendGuardOpts{
			MaxFeeSatsPerTx: maxFee,
			MaxSpendSatsDay: maxDay,
			MinInterval:     time.Duration(minIntervalMs) * time.Millisecond,
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
	publicURL := strings.TrimRight(envOr("NEKTAR_PUBLIC_URL", envOr("NEKTAR_VERIFY_BASE", "http://127.0.0.1:"+listenPort)), "/")
	publicURL = strings.TrimSuffix(publicURL, "/v1/verify")
	verifyBase := publicURL + "/v1/verify"
	if strings.Contains(publicURL, "127.0.0.1") || strings.Contains(publicURL, "localhost") {
		if relay := strings.TrimRight(strings.TrimSpace(os.Getenv("NEKTAR_PUBLIC_VERIFY_RELAY")), "/"); relay != "" && strings.ToLower(relay) != "off" {
			verifyBase = relay
		}
	}
	modelRelease := envOr("NEKTAR_MODEL_RELEASE", envOr("NEKTAR_MODEL_ID", Version))
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), objStore, memory.Config{
		VerifyBaseURL: verifyBase,
		ModelID: reasoning.ModelIdentity{
			Kind:     "api",
			Provider: envOr("NEKTAR_MODEL_PROVIDER", "nektar"),
			Release:  modelRelease,
		},
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	_ = market.SyncPolicyFromDB(db, mkt.Policy())
	if killActive(dir) {
		mkt.Policy().SetKillSwitch(true)
	}
	disc := discovery.New(mem, parseCSVEnv("NEKTAR_MARKET_HOSTS"), nil)
	return &runtimeBundle{
		mem:     mem,
		mkt:     mkt,
		disc:    disc,
		dataDir: dir,
		closeFn: func() {
			_ = port.Close()
			_ = db.Close()
		},
	}, nil
}

func killActive(dataDir string) bool {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("NEKTAR_KILL"))); v == "1" || v == "true" || v == "yes" {
		return true
	}
	if dataDir != "" {
		if _, err := os.Stat(filepath.Join(dataDir, "KILL")); err == nil {
			return true
		}
	}
	return false
}

func parseToolPricesEnv(raw string) map[string]uint64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := make(map[string]uint64)
	for _, part := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		if sats, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil && sats > 0 {
			out[strings.TrimSpace(k)] = sats
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
	bootStart := time.Now()
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	defaultListen := "127.0.0.1:" + envOr("NEKTAR_HTTP_PORT", DefaultLocalPort)
	if server.IsContainerEnv() {
		defaultListen = ":" + envOr("NEKTAR_HTTP_PORT", DefaultLocalPort)
	}
	defaultRestateListen := "127.0.0.1:9080"
	if server.IsContainerEnv() {
		defaultRestateListen = ":9080"
	}
	addr := fs.String("listen", defaultListen, "HTTP listen address")
	restateAddr := fs.String("restate-listen", envOr("NEKTAR_RESTATE_LISTEN", defaultRestateListen), "Dedicated Restate HTTP/2 (h2c) endpoint address (default 127.0.0.1:9080)")
	withHub := fs.Bool("hub", os.Getenv("NEKTAR_EMBED_HUB") == "1", "Mount embedded durable SQLite Go Overlay Hub at /hub/ with starter liquidity & bounties")
	netName := fs.String("network", "", "Network override (memory|regtest|testnet|main)")
	dataDir := fs.String("data-dir", "", "Data directory override")
	ephemeral := fs.Bool("ephemeral", false, "Run with in-memory SQLite (data discarded on exit)")
	publicURLFlag := fs.String("public-url", os.Getenv("NEKTAR_PUBLIC_URL"), "Publicly reachable HTTP base URL for direct A2A trades (empty = NAT bbox fallback)")
	hostsFlag := fs.String("hosts", os.Getenv("NEKTAR_MARKET_HOSTS"), "Comma-separated market hub URLs to join")
	_ = fs.Parse(args)

	if *ephemeral {
		_ = os.Setenv("NEKTAR_EPHEMERAL", "1")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	lockDir := *dataDir
	if lockDir == "" {
		lockDir = defaultDataDir()
	}
	if !*ephemeral {
		releaseLock, lockErr := acquireInstanceLock(lockDir, *addr)
		if lockErr != nil {
			return lockErr
		}
		defer releaseLock()
	}

	portNum, portStr := extractPort(*addr)
	rt, err := openRuntime(ctx, *netName, *dataDir, portStr)
	if err != nil {
		return err
	}
	defer rt.closeFn()

	// Reconcile any pre-crash pending shard anchor intents before accepting new traffic (F8)!
	if nRec, rErr := rt.mem.ReconcilePendingAnchors(ctx); rErr == nil && nRec > 0 {
		log.Printf("reconciled %d pending shard anchor(s) from crash journal", nRec)
	}

	// Cold-start zero-empty-room welcome shard + live UTXO sync & fan-out!
	if _, err := rt.mem.EnsureWelcome(ctx); err != nil {
		log.Printf("welcome seed warning: %v", err)
	}
	// Start non-blocking background FuelKeeper (Reserve -> Fuel fan-out + dust consolidation)
	// and Monitor (SendWaiting, CheckForProofs + BEEF compaction, AbortAbandoned, AddressSync).
	rt.mem.Chain().StartBackground(ctx)

	if rt.mem.Chain().Network() != "memory" {
		// Only block boot briefly if wallet is empty; Monitor handles ongoing sync & proof upgrades asynchronously!
		if rt.mem.Chain().BalanceSats() == 0 {
			syncCtx, syncCancel := context.WithTimeout(ctx, 8*time.Second)
			if added, proved, sErr := rt.mem.Chain().SyncPool(syncCtx); sErr == nil && (added > 0 || proved > 0) {
				log.Printf("synced %s wallet: +%d UTXO(s), %d proved (balance=%d sats)", rt.mem.Chain().Network(), added, proved, rt.mem.Chain().BalanceSats())
			}
			syncCancel()
		}
		go func() {
			ticker := time.NewTicker(45 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					_, _ = rt.mem.RefreshAnchorProofs(ctx)
				}
			}
		}()
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

	// Start active Restate durable execution plane (dedicated h2c endpoint + auto-supervised or external restate-server).
	restateClient, stopRestate, err := StartRestatePlane(ctx, rt.mkt, RestateRuntimeConfig{
		ListenAddr:            *restateAddr,
		DataDir:               rt.dataDir,
		ExternalIngressURL:    os.Getenv("NEKTAR_RESTATE_URL"),
		ExternalAdminURL:      os.Getenv("NEKTAR_RESTATE_ADMIN"),
		EndpointURI:           os.Getenv("NEKTAR_RESTATE_ENDPOINT_URI"),
		IdentityPublicKey:     os.Getenv("NEKTAR_RESTATE_IDENTITY_KEY"),
		AllowInsecureEndpoint: os.Getenv("NEKTAR_RESTATE_ALLOW_INSECURE_ENDPOINT") == "1",
		InternalToken:         os.Getenv("NEKTAR_RESTATE_INTERNAL_TOKEN"),
		DisableAutoStart:      os.Getenv("NEKTAR_RESTATE_DISABLE") == "1",
		Required:              os.Getenv("NEKTAR_RESTATE_REQUIRED") == "1",
	})
	if err != nil {
		return err
	}
	defer stopRestate()

	anchorCadenceSec := envInt64("NEKTAR_ANCHOR_CADENCE_SEC", 300)
	if restateClient != nil {
		// Poke durable Restate AnchorCadence on Remember (short 5s dedup bucket) + re-arm any unanchored shards on boot (RT0011)!
		rt.mem.SetOnRemember(func(shard string) {
			go func(sh string) {
				pCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, _ = restateClient.TriggerAnchorCadence(pCtx, sh, anchorCadenceSec)
			}(shard)
		})
		go func() {
			shards, _ := rt.mem.DB().ListShards()
			for _, sh := range shards {
				if un, uErr := rt.mem.DB().ListUnanchored(sh); uErr == nil && len(un) > 0 {
					pCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
					_, _ = restateClient.TriggerAnchorCadence(pCtx, sh, anchorCadenceSec)
					cancel()
				}
			}
		}()
	}

	var appRef *server.App
	broker := market.NewBroker(rt.mem, rt.mkt, rt.disc, market.BrokerConfig{
		PublicURL:     pubURL,
		BboxOffice:    envOr("NEKTAR_BBOX_OFFICE", "default"),
		Handle:        os.Getenv("NEKTAR_HANDLE"),
		IsHub:         *withHub,
		RestateClient: restateClient,
		OnStateChange: func(ev string, _ any) {
			if appRef != nil {
				appRef.BroadcastDashboardEvent(ev)
			}
		},
	})
	if liqState != nil {
		broker.SetAdvertisedPacks(liqState.Packs, liqState.BountyDomains)
	}

	rawAPITok := envOr("NEKTAR_API_TOKEN", os.Getenv("NEKTAR_HTTP_TOKEN"))
	rawOpTok := os.Getenv("NEKTAR_OPERATOR_TOKEN")
	requireAuth := server.IsContainerEnv() || server.IsNonLoopbackListen(*addr)
	apiTok, opTok, genTokens := server.ResolveOrGenerateTokens(rt.dataDir, rawAPITok, rawOpTok, requireAuth)
	restateAdminDisplay := strings.TrimSpace(os.Getenv("NEKTAR_RESTATE_ADMIN"))
	if restateAdminDisplay == "" && restateClient != nil {
		restateAdminDisplay = restateClient.AdminURL
	}
	app, err := server.NewApp(rt.mem, rt.mkt, server.Config{
		ListenAddr:        *addr,
		PublicURL:         pubURL,
		APIToken:          apiTok,
		OperatorToken:     opTok,
		ToolProfile:       envOr("NEKTAR_TOOL_PROFILE", "operator"),
		ToolPrices:        parseToolPricesEnv(os.Getenv("NEKTAR_TOOL_PRICES")),
		OpenAIUpstream:    os.Getenv("NEKTAR_OPENAI_UPSTREAM"),
		AnthropicUpstream: os.Getenv("NEKTAR_ANTHROPIC_UPSTREAM"),
		Hub:               hubNode,
		Discovery:         rt.disc,
		Broker:            broker,
		RestateClient:     restateClient,
		RestateAdminURL:   restateAdminDisplay,
		ExposeWalletWire:  os.Getenv("NEKTAR_EXPOSE_WALLET_WIRE") == "1",
	})
	if err != nil {
		return err
	}
	appRef = app

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

	pairCode := ""
	if apiTok != "" || opTok != "" {
		pairCode = app.CreatePairCode()
	}
	feeAddr, _ := rt.mem.Chain().FundAddress()
	bannerURL := actualURL
	if pubURL != "" && !strings.Contains(pubURL, "127.0.0.1") {
		bannerURL = pubURL
	}
	fmt.Fprint(os.Stderr, server.FormatStartupBanner(server.StartupBannerOpts{
		Version:         Version,
		URL:             bannerURL,
		Network:         rt.mem.Chain().Network(),
		BootDuration:    time.Since(bootStart),
		PairCode:        pairCode,
		FeeAddress:      feeAddr,
		BalanceSats:     rt.mem.Chain().BalanceSats(),
		WithHub:         *withHub,
		ContainerMode:   server.IsContainerEnv(),
		GeneratedTokens: genTokens,
	}))
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
	_, _ = rt.mem.ReconcilePendingAnchors(ctx)
	_, _ = rt.mem.EnsureWelcome(ctx)
	rt.mem.Chain().StartBackground(ctx)
	if rt.mem.Chain().Network() != "memory" {
		go func() {
			ticker := time.NewTicker(45 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					_, _ = rt.mem.RefreshAnchorProofs(ctx)
				}
			}
		}()
	}

	broker := market.NewBroker(rt.mem, rt.mkt, rt.disc, market.BrokerConfig{})
	tb := server.NewToolbox(rt.mem, rt.mkt, broker, rt.disc, nil, envOr("NEKTAR_TOOL_PROFILE", "agent"))
	srv := server.NewMCPServerWithToolbox(tb)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

func cmdRemember(args []string) error {
	fs := flag.NewFlagSet("remember", flag.ExitOnError)
	shard := fs.String("shard", "default", "Target shard name")
	text := fs.String("text", "", "Memory fragment text")
	group := fs.String("group", "", "Optional Borg group ID for BRC-369 encryption")
	anchor := fs.Bool("anchor", false, "Anchor immediately on BSV")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	dir := *dataDir
	if dir == "" {
		dir = defaultDataDir()
	}
	if info, ok := server.DetectLiveDaemon(dir); ok {
		out, err := callLiveTool(info, "remember", map[string]any{
			"shard":      *shard,
			"text":       *text,
			"borgGroup":  *group,
			"autoAnchor": *anchor,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}

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

	dir := *dataDir
	if dir == "" {
		dir = defaultDataDir()
	}
	if info, ok := server.DetectLiveDaemon(dir); ok {
		out, err := callLiveTool(info, "recall", map[string]any{
			"shard":     *shard,
			"query":     *query,
			"verifySpv": *spv,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}

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

	dir := *dataDir
	if dir == "" {
		dir = defaultDataDir()
	}
	if info, ok := server.DetectLiveDaemon(dir); ok {
		out, err := callLiveTool(info, "anchor", map[string]any{"shard": *shard})
		if err != nil {
			return err
		}
		return printJSON(out)
	}

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

	dir := *dataDir
	if dir == "" {
		dir = defaultDataDir()
	}
	if info, ok := server.DetectLiveDaemon(dir); ok {
		out, err := callLiveTool(info, "verifyReceipt", map[string]any{"token": *token})
		if err != nil {
			return err
		}
		return printJSON(out)
	}

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
