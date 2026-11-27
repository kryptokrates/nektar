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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
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
	walletModeFlag := fs.String("wallet", os.Getenv("NEKTAR_WALLET"), "Wallet custody mode: auto (default: BSV Desktop if running on :3321, else embedded) | client | embedded")
	walletWireFlag := fs.String("wallet-wire-url", os.Getenv("NEKTAR_WALLET_WIRE_URL"), "BSV Desktop / BRC-100 wire URL (default http://127.0.0.1:3321)")
	dataDir := fs.String("data-dir", "", "Data directory override")
	ephemeral := fs.Bool("ephemeral", false, "Run with in-memory SQLite (data discarded on exit)")
	publicURLFlag := fs.String("public-url", os.Getenv("NEKTAR_PUBLIC_URL"), "Publicly reachable HTTP base URL for direct A2A trades (empty = NAT bbox fallback)")
	hostsFlag := fs.String("hosts", os.Getenv("NEKTAR_MARKET_HOSTS"), "Comma-separated market hub URLs to join")
	_ = fs.Parse(args)

	if strings.TrimSpace(*confirmFlag) != "" {
		_ = os.Setenv("NEKTAR_MAINNET_CONFIRM", strings.TrimSpace(*confirmFlag))
	}
	if strings.TrimSpace(*walletModeFlag) != "" {
		_ = os.Setenv("NEKTAR_WALLET", strings.TrimSpace(*walletModeFlag))
	}
	if strings.TrimSpace(*walletWireFlag) != "" {
		_ = os.Setenv("NEKTAR_WALLET_WIRE_URL", strings.TrimSpace(*walletWireFlag))
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
	var isTxSettled func(txid string) bool
	if rt.mem != nil && rt.mem.Chain() != nil && rt.mem.Chain().Network() != "memory" {
		isTxSettled = func(txid string) bool {
			pCtx, pCancel := context.WithTimeout(ctx, 10*time.Second)
			defer pCancel()
			mp, bh, ok, pErr := rt.mem.Chain().ProofChecked(pCtx, txid)
			if pErr != nil || !ok || mp == nil {
				return false
			}
			depth := rt.mem.Chain().SettleDepth()
			if depth <= 0 {
				depth = 1
			}
			tip, hErr := rt.mem.Chain().Height(pCtx)
			return hErr == nil && tip >= bh && int(tip-bh)+1 >= depth
		}
	}
	if obRep, oErr := market.ReconcileObligations(ctx, rt.mem.DB(), isTxSettled, time.Now().Unix()); oErr == nil && obRep.Checked > 0 {
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

	var postBootWG sync.WaitGroup
	bgCtx, cancelBG := context.WithCancel(ctx)
	defer func() {
		cancelBG()
		postBootWG.Wait()
	}()
	if rt.mem.Chain().Network() != "memory" && rt.mem.Chain().BalanceSats() == 0 {
		// Run initial wallet sync asynchronously so zero-env cold boot opens HTTP/MCP in <100ms
		// instead of silently blocking up to 8s on empty fresh wallets.
		postBootWG.Add(1)
		go func() {
			defer postBootWG.Done()
			syncCtx, syncCancel := context.WithTimeout(bgCtx, 8*time.Second)
			defer syncCancel()
			if rep, sErr := rt.mem.Chain().SyncFull(syncCtx); sErr == nil && (rep.AddedOutputs > 0 || rep.ProvedTxs > 0) {
				log.Printf("synced %s wallet: +%d UTXO(s), %d proved (balance=%d sats)", rt.mem.Chain().Network(), rep.AddedOutputs, rep.ProvedTxs, rt.mem.Chain().BalanceSats())
			}
		}()
	}
	stopRefresher := startAnchorProofRefresher(ctx, rt.mem)
	defer stopRefresher()

	// Optionally mount durable embedded Go Overlay Hub at /hub/ with Liquidity Engine + Bounties + Faucet!
	var hubNode *hub.Node
	var liqState *hub.LiquidityState
	var upstreamSpines []string
	if *withHub {
		hubNode, liqState, upstreamSpines, err = initEmbeddedHub(ctx, rt, *dataDir, *hubRoleFlag, *feePresetFlag, *upstreamSpinesFlag)
		if err != nil {
			return err
		}
		defer hubNode.Close()
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
		stopEngineSrv := wireEmbeddedHub(ctx, rt, hubNode, upstreamSpines)
		defer stopEngineSrv()
	}

	rawAPITok := os.Getenv("NEKTAR_API_TOKEN")
	rawOpTok := os.Getenv("NEKTAR_OPERATOR_TOKEN")
	requireAuth := server.IsContainerEnv() || server.IsNonLoopbackListen(*addr)
	apiTok, opTok, genTokens := server.ResolveOrGenerateTokens(rt.dataDir, rawAPITok, rawOpTok, requireAuth)
	restateAdminDisplay := strings.TrimSpace(os.Getenv("NEKTAR_RESTATE_ADMIN"))
	app, err := server.NewApp(rt.mem, rt.mkt, server.Config{
		ListenAddr:        *addr,
		PublicURL:         pubURL,
		DataDir:           rt.dataDir,
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

	drainCtx, cancelDrain := context.WithCancel(context.Background())
	defer cancelDrain()
	httpSrv := &http.Server{
		Handler:           app.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return drainCtx
		},
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
		cancelDrain()
	}()

	// Listener is already accepting TCP connections: self-join embedded Hub and start Broker loops before Restate.
	if *withHub {
		_, _, _ = rt.disc.JoinHost(ctx, actualURL+"/hub")
	}
	wireChainCallbacks(ctx, rt, broker, hubNode, liqState)
	broker.Start(ctx)

	// Start active Restate durable execution plane (dedicated h2c endpoint + auto-supervised or external restate-server).
	var restateAlive atomic.Bool
	restateAlive.Store(true)
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
		OnUnexpectedExit: func(error) {
			restateAlive.Store(false)
			app.BroadcastDashboardEvent("restate_exited")
		},
	})
	if err != nil {
		stop()
		<-shutdownDone
		return err
	}
	defer stopRestate()

	stopRestateHooks := attachRestateClient(ctx, rt, broker, app, restateClient, restateAdminDisplay, restateAlive.Load)
	defer stopRestateHooks()

	// Write daemon.json + active-daemon.json so `nektar mcp`, `nektar open`, `nektar hook`, `nektar dock`,
	// Hermes, and OpenClaw auto-discover us across both Mainnet and Testnet!
	dInfo := server.DaemonInfo{
		PID:       os.Getpid(),
		Port:      actualPort,
		URL:       actualURL,
		Network:   rt.mem.Chain().Network(),
		DataDir:   rt.dataDir,
		AuthToken: apiTok,
		StartedAt: time.Now().Unix(),
	}
	_ = server.WriteDaemonInfo(rt.dataDir, dInfo)
	_ = server.WriteActiveDaemonPointer("", rt.dataDir, dInfo)
	defer server.RemoveDaemonInfo(rt.dataDir)

	postBootWG.Add(1)
	go func() {
		defer postBootWG.Done()
		runPostBootDiscovery(bgCtx, rt, broker)
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
	fmt.Fprint(os.Stderr, formatConsoleText(server.FormatStartupBanner(server.StartupBannerOpts{
		Version:         Version,
		URL:             bannerURL,
		Network:         rt.mem.Chain().Network(),
		BootDuration:    time.Since(bootStart),
		PairCode:        pairCode,
		FeeAddress:      feeAddr,
		BalanceSats:     rt.mem.Chain().BalanceSats(),
		WalletMode:      rt.mem.Chain().WalletMode(),
		WithHub:         *withHub,
		ContainerMode:   server.IsContainerEnv(),
		GeneratedTokens: genTokens,
	})))
	serveErr := <-serveErrCh
	if serveErr != nil && serveErr != http.ErrServerClosed {
		stop()
		<-shutdownDone
		return serveErr
	}
	<-shutdownDone
	return nil
}
