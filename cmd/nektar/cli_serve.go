package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/server"
)

func cmdServe(args []string) error {
	bootStart := time.Now()
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	defaultListen := "127.0.0.1:" + envOr("NEKTAR_HTTP_PORT", DefaultLocalPort)
	if server.IsContainerEnv() {
		defaultListen = ":" + envOr("NEKTAR_HTTP_PORT", DefaultLocalPort)
	}
	// Always default Restate SDK listener to loopback 127.0.0.1:9080 so single-container deployments
	// never trip the non-loopback identity guard; multi-container Compose stacks pass `-restate-listen :9080`.
	defaultRestateListen := "127.0.0.1:9080"
	addr := fs.String("listen", defaultListen, "HTTP listen address")
	restateAddr := fs.String("restate-listen", envOr("NEKTAR_RESTATE_LISTEN", defaultRestateListen), "Dedicated Restate HTTP/2 (h2c) endpoint address (default 127.0.0.1:9080)")
	withHub := fs.Bool("hub", os.Getenv("NEKTAR_EMBED_HUB") == "1", "Mount embedded durable SQLite Go Overlay Hub at /hub/ with starter liquidity & bounties")
	hubRoleFlag := fs.String("hub-role", os.Getenv("NEKTAR_HUB_ROLE"), "Embedded Hub topology role (leaf|spine)")
	feePresetFlag := fs.String("fee-preset", os.Getenv("NEKTAR_HUB_FEE_PRESET"), "Embedded Hub fee preset (free|lightbsv_egress|hybrid)")
	upstreamSpinesFlag := fs.String("upstream-spines", os.Getenv("NEKTAR_UPSTREAM_SPINES"), "Comma-separated Spine Hub URLs for Leaf upstream forwarding")
	netName := fs.String("network", "", "Network override (memory|regtest|testnet|main)")
	confirmFlag := fs.String("confirm", "", "Mainnet acknowledgement (pass I_UNDERSTAND with --network main)")
	dataDir := fs.String("data-dir", "", "Data directory override")
	ephemeral := fs.Bool("ephemeral", false, "Run with in-memory SQLite (data discarded on exit)")
	publicURLFlag := fs.String("public-url", os.Getenv("NEKTAR_PUBLIC_URL"), "Publicly reachable HTTP base URL for direct A2A trades (empty = NAT bbox fallback)")
	hostsFlag := fs.String("hosts", os.Getenv("NEKTAR_MARKET_HOSTS"), "Comma-separated market hub URLs to join")
	_ = fs.Parse(args)

	if strings.TrimSpace(*confirmFlag) != "" {
		_ = os.Setenv("NEKTAR_MAINNET_CONFIRM", strings.TrimSpace(*confirmFlag))
	}
	if *ephemeral {
		_ = os.Setenv("NEKTAR_EPHEMERAL", "1")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	effectiveNet := *netName
	if effectiveNet == "" {
		effectiveNet = envOr("NEKTAR_NETWORK", "testnet")
	}
	lockDir := *dataDir
	if lockDir == "" {
		lockDir = defaultDataDirForNetwork(effectiveNet)
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

	// Reconcile any pre-crash pending shard anchor intents and surviving commercial obligations before accepting traffic!
	if nRec, rErr := rt.mem.ReconcilePendingAnchors(ctx); rErr == nil && nRec > 0 {
		log.Printf("reconciled %d pending shard anchor(s) from crash journal", nRec)
	}
	if obRep, oErr := market.ReconcileObligations(ctx, rt.mem.DB(), nil, time.Now().Unix()); oErr == nil && obRep.Checked > 0 {
		log.Printf("reconciled %d surviving commercial obligation(s): fulfilled=%d overdue=%d refundClaimable=%d open=%d",
			obRep.Checked, obRep.Fulfilled, obRep.MarkedOverdue, obRep.RefundClaimable, obRep.StillOpen)
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
	}
	stopRefresher := startAnchorProofRefresher(ctx, rt.mem)
	defer stopRefresher()

	// Optionally mount durable embedded Go Overlay Hub at /hub/ with Liquidity Engine + Bounties + Faucet!
	var hubNode *hub.Node
	var liqState *hub.LiquidityState
	var upstreamSpines []string
	if *withHub {
		hubDBPath := filepath.Join(rt.dataDir, "hub.sqlite")
		if rt.mem.Chain().Network() == "memory" && *dataDir == "" && os.Getenv("NEKTAR_DATA_DIR") == "" {
			hubDBPath = ":memory:"
		}
		feeSched := hub.LoadFeeSchedule(os.Getenv)
		if fp := strings.TrimSpace(*feePresetFlag); fp != "" && fp != strings.TrimSpace(os.Getenv("NEKTAR_HUB_FEE_PRESET")) {
			feeSched = hub.DefaultFeeSchedule(fp)
		}
		if hr := strings.ToLower(strings.TrimSpace(*hubRoleFlag)); hr == string(hub.RoleSpine) || hr == string(hub.RoleLeaf) {
			feeSched.Role = hub.HubRole(hr)
		}
		feeSched.Normalize()
		for _, s := range strings.Split(*upstreamSpinesFlag, ",") {
			if s = strings.TrimSpace(s); s != "" {
				upstreamSpines = append(upstreamSpines, s)
			}
		}
		hubNode, err = hub.NewNode(hub.Config{
			Network:        rt.mem.Chain().Network(),
			DBPath:         hubDBPath,
			EnableFinger:   true,
			EnableBbox:     true,
			EnableBlogs:    true,
			EnableBorg:     true,
			EnableBsecret:  true,
			EnableBstore:   true,
			Getenv:         os.Getenv,
			ChainTracker:   rt.mem.Chain().Tracker(),
			Fees:           feeSched,
			FeeCollector:   hub.NewPortFeeCollector(rt.mem.Chain()),
			UpstreamSpines: upstreamSpines,
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
	}

	localBase := fmt.Sprintf("http://127.0.0.1:%d", portNum)
	pubURL := strings.TrimRight(strings.TrimSpace(*publicURLFlag), "/")
	if pubURL == "" {
		// Derive public URL from standard PaaS platform env vars when present; NEVER advertise 127.0.0.1
		// on public market lanes (empty pubURL cleanly selects NAT/bbox fallback).
		if u := strings.TrimRight(strings.TrimSpace(os.Getenv("RENDER_EXTERNAL_URL")), "/"); u != "" {
			pubURL = u
		} else if d := strings.TrimSpace(os.Getenv("RAILWAY_PUBLIC_DOMAIN")); d != "" {
			pubURL = "https://" + strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
		} else if app := strings.TrimSpace(os.Getenv("FLY_APP_NAME")); app != "" {
			pubURL = "https://" + app + ".fly.dev"
		}
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

	// Construct Broker and App BEFORE starting the Restate durable plane so any immediately
	// replaying BuyerDeal/SellerDeal invocation always sees a live Broker and App (zero nil-broker skip).
	var appRef atomic.Pointer[server.App]
	jevMatcher := buildJevMatcherFromEnv(rt.mem.Chain().Network())
	broker := market.NewBroker(rt.mem, rt.mkt, rt.disc, market.BrokerConfig{
		PublicURL:  pubURL,
		BboxOffice: envOr("NEKTAR_BBOX_OFFICE", "default"),
		Handle:     os.Getenv("NEKTAR_HANDLE"),
		IsHub:      *withHub,
		Hub:        hubNode,
		Matcher:    jevMatcher,
		OnStateChange: func(ev string, _ any) {
			if a := appRef.Load(); a != nil {
				a.BroadcastDashboardEvent(ev)
			}
		},
	})
	defer broker.Stop()
	if liqState != nil {
		broker.SetAdvertisedPacks(liqState.Packs, liqState.BountyDomains)
	}
	if hubNode != nil {
		if rt.disc != nil && rt.mkt != nil && rt.mkt.Policy() != nil {
			rt.disc.SetHubFeePolicy(rt.mkt.Policy().CanPayHubFee, rt.mkt.Policy().RecordHubFeeSpend)
		}
		hubNode.SetUpstreamSubmitter(rt.disc.SubmitBEEF)
		var leafDomFn func() []string
		if doms := parseCSVEnv("NEKTAR_HUB_DOMAINS"); len(doms) > 0 {
			leafDomFn = func() []string { return doms }
		}
		hubNode.SetEgressAuth(
			func(action, aud string, cursor int64, domainsCSV string) (string, string, string, string, error) {
				return discovery.SignEgressAuthHeaders(rt.disc.AnnounceKey(), action, aud, cursor, domainsCSV)
			},
			func(tCtx context.Context, spineURL string, minTopupSats uint64) error {
				_, tErr := rt.disc.TopUpHubEgressCredits(tCtx, spineURL, minTopupSats, rt.disc.AnnouncePubHex())
				return tErr
			},
			leafDomFn,
		)
		if peers := parseCSVEnv("NEKTAR_PEER_HUBS"); len(peers) > 0 {
			hubNode.StartFederationWithTier(ctx, peers, true)
		}
		if len(upstreamSpines) > 0 {
			hubNode.StartFederationWithTier(ctx, upstreamSpines, true)
		}
	}

	rawAPITok := envOr("NEKTAR_API_TOKEN", os.Getenv("NEKTAR_HTTP_TOKEN"))
	rawOpTok := os.Getenv("NEKTAR_OPERATOR_TOKEN")
	requireAuth := server.IsContainerEnv() || server.IsNonLoopbackListen(*addr)
	apiTok, opTok, genTokens := server.ResolveOrGenerateTokens(rt.dataDir, rawAPITok, rawOpTok, requireAuth)
	restateAdminDisplay := strings.TrimSpace(os.Getenv("NEKTAR_RESTATE_ADMIN"))
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
		RestateAdminURL:   restateAdminDisplay,
		ExposeWalletWire:  os.Getenv("NEKTAR_EXPOSE_WALLET_WIRE") == "1",
	})
	if err != nil {
		return err
	}
	appRef.Store(app)
	defer app.Close()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	actualURL := fmt.Sprintf("http://127.0.0.1:%d", actualPort)
	if *withHub && pubURL == localBase {
		broker.SetPublicURL(actualURL)
	}

	httpSrv := &http.Server{
		Handler:           app.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- httpSrv.Serve(ln)
	}()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		app.Close()
		if hubNode != nil {
			hubNode.CloseSSE()
		}
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	// Listener is already accepting TCP connections: self-join embedded Hub and start Broker loops before Restate.
	if *withHub {
		_, _, _ = rt.disc.JoinHost(ctx, actualURL+"/hub")
	}
	// Wire Monitor verified-BUMP OnTxMined & WitnessedTracker OnReorg callbacks across memory, market broker, and hub!
	rt.mem.Chain().OnTxMined(func(txid string, height uint32, mp *transaction.MerklePath) {
		mCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if _, err := rt.mem.RefreshAnchorProofByTxID(mCtx, txid, height, mp); err == nil && hubNode != nil {
			if packs, rErr := hub.RefreshHubLiquidityStock(mCtx, rt.mem); rErr == nil && len(packs) > 0 {
				var bountyDoms []string
				if liqState != nil {
					bountyDoms = liqState.BountyDomains
				}
				broker.SetAdvertisedPacks(packs, bountyDoms)
			}
		}
		broker.OnPaymentTxMined(mCtx, txid, height, mp)
		if hubNode != nil {
			_ = hubNode.OnBountyPayoutMined(txid)
			_ = hubNode.OnFeePaymentMined(txid, height)
		}
	})
	rt.mem.Chain().OnReorg(func(forkHeight uint32, _ []string) {
		rCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		_, _ = rt.mem.HandleReorg(forkHeight)
		broker.HandleReorg(rCtx, forkHeight)
		if hubNode != nil {
			_ = hubNode.HandleReorg(rCtx, forkHeight)
		}
	})
	broker.Start(ctx)

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
		stop()
		<-shutdownDone
		return err
	}
	defer stopRestate()

	if restateClient != nil {
		broker.SetRestateClient(restateClient)
		if restateAdminDisplay == "" {
			restateAdminDisplay = restateClient.AdminURL
		}
		app.SetRestateClient(restateClient, restateAdminDisplay)

		anchorCadenceSec := envInt64("NEKTAR_ANCHOR_CADENCE_SEC", 300)
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

	var postBootWG sync.WaitGroup
	postBootWG.Add(1)
	go func() {
		defer postBootWG.Done()
		_, _ = broker.AnnounceSelf(ctx)
		if seedDoms := parseCSVEnv("NEKTAR_SEED_DOMAINS"); len(seedDoms) > 0 {
			if n, sErr := rt.disc.SeedFromNetwork(ctx, seedDoms); sErr == nil && n > 0 {
				log.Printf("seeded %d verified fragment(s) from network CHIRP packs", n)
			}
		}
	}()
	defer postBootWG.Wait()

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
	serveErr := <-serveErrCh
	if serveErr != nil && serveErr != http.ErrServerClosed {
		stop()
		<-shutdownDone
		return serveErr
	}
	<-shutdownDone
	return nil
}

// startAnchorProofRefresher runs one shared 45s proof-upgrade loop on live networks and returns a drainable stop function.
func startAnchorProofRefresher(ctx context.Context, mem *memory.Service) func() {
	if mem == nil || mem.Chain() == nil || mem.Chain().Network() == "memory" {
		return func() {}
	}
	rCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(45 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-rCtx.Done():
				return
			case <-ticker.C:
				_, _ = mem.RefreshAnchorProofs(rCtx)
			}
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
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

	if *netName != "memory" {
		releaseLock, lockErr := acquireInstanceLock(dir, "mcp-stdio")
		if lockErr != nil {
			return lockErr
		}
		defer releaseLock()
	}

	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	_, _ = rt.mem.ReconcilePendingAnchors(ctx)
	_, _ = rt.mem.EnsureWelcome(ctx)
	rt.mem.Chain().StartBackground(ctx)
	stopRefresher := startAnchorProofRefresher(ctx, rt.mem)
	defer stopRefresher()

	broker := market.NewBroker(rt.mem, rt.mkt, rt.disc, market.BrokerConfig{
		Matcher: buildJevMatcherFromEnv(rt.mem.Chain().Network()),
	})
	tb := server.NewToolbox(rt.mem, rt.mkt, broker, rt.disc, nil, envOr("NEKTAR_TOOL_PROFILE", "agent"))
	srv := server.NewMCPServerWithToolbox(tb)
	return srv.Run(ctx, &mcp.StdioTransport{})
}
