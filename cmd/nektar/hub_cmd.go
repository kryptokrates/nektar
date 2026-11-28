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
	"syscall"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
)

const hubUsage = `nektar hub — 1-command zero-Docker LightBSV Hub deployment & standalone overlay server

  nektar hub up [options]               start native single-binary Hub, peer with Spine & announce on dir
  nektar hub status [--dir <path>]      inspect hub process/service, health, fees & earnings
  nektar hub logs [--dir <path>]        show hub logs
  nektar hub down [--dir <path>]        stop hub cleanly (keys & SQLite preserved)
  nektar hub template <kind> [options]  print a 1-click template (coolify|dokploy|cloud-init|railway|render|fly|umbrel|casaos)
  nektar hub run [flags]                run foreground Go overlay hub directly in this process

Options for 'up':
  --domain <fqdn>          Public domain (serves BRC-180 /manifest.json + bfinger handles)
  --auto-ip                Zero-domain HTTPS host (<public-ip>.sslip.io)
  --tunnel                 Zero-port HTTPS via Cloudflare Quick Tunnel (works behind NAT)
  --port <port>            Hub HTTP listen port in native mode (default: 18421)
  --docker                 Use Docker Compose stack instead of zero-Docker native binary mode
  --native                 Explicit alias for default zero-Docker native binary mode
  --network <testnet|main> Network (default: testnet; main requires --confirm I_UNDERSTAND)
  --mainnet                Shorthand for --network main (requires --confirm I_UNDERSTAND)
  --confirm I_UNDERSTAND   Required acknowledgement for mainnet
  --hub-role <leaf|spine>  Spine-and-Leaf topology role (default: leaf)
  --fee-preset <preset>    Monetization preset (free|spine_wholesale|lightbsv_egress|hybrid)
  --seed-bounties          Explicitly fund Hub-sponsored bounties out of Hub wallet (default: false)
  --dir <path>             Hub directory (default: ~/.nektar-hub)
  --dry                    Write .env + systemd unit without starting the Hub process
  --no-join                Skip auto-joining local running agent to this new hub
`

func cmdHub(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Print(hubUsage)
			return nil
		case "up":
			return cmdHubUp(args[1:])
		case "status", "logs", "down":
			return cmdHubLifecycle(args[0], args[1:])
		case "template":
			return cmdHubTemplate(args[1:])
		case "run":
			return cmdHubRun(args[1:])
		}
	}
	return cmdHubRun(args)
}

func cmdHubRun(args []string) error {
	fs := flag.NewFlagSet("hub run", flag.ExitOnError)
	addr := fs.String("listen", ":"+envOr("NEKTAR_HUB_PORT", "18421"), "Hub HTTP listen address")
	netName := fs.String("network", envOr("NEKTAR_NETWORK", "testnet"), "Network name (memory|regtest|testnet|main)")
	dataDir := fs.String("data-dir", "", "Data directory for hub.sqlite")
	upstreamFlag := fs.String("upstream-spines", os.Getenv("NEKTAR_UPSTREAM_SPINES"), "Comma-separated Spine Hub URLs for Leaf BRC-149 Object Plane relay & upstream submission")
	withLiquidity := fs.Bool("liquidity", true, "Seed curated Hub starter shards & packs")
	seedBounties := fs.Bool("seed-bounties", os.Getenv("NEKTAR_HUB_SEED_BOUNTIES") == "1", "Explicitly fund Hub-sponsored bounties out of Hub wallet (default: false)")
	hubRoleFlag := fs.String("hub-role", os.Getenv("NEKTAR_HUB_ROLE"), "Hub topology role (leaf|spine)")
	feePresetFlag := fs.String("fee-preset", os.Getenv("NEKTAR_HUB_FEE_PRESET"), "Hub fee preset (free|spine_wholesale|lightbsv_egress|hybrid)")
	wantFeeFlag := fs.Uint64("want-fee-sats", 0, "BRC-105 fee in satoshis for WantAd POST /submit")
	offerFeeFlag := fs.Uint64("offer-fee-sats", 0, "BRC-105 fee in satoshis for OfferAd POST /submit")
	announceFeeFlag := fs.Uint64("announce-fee-sats", 0, "BRC-105 fee in satoshis for AnnounceAd POST /submit")
	bboxFeeFlag := fs.Uint64("bbox-fee-sats", 0, "BRC-105 fee in satoshis for NAT POST /bbox/send")
	egressFeeFlag := fs.Uint64("egress-sats-per-100", 0, "Metered egress fee in satoshis per 100 market events")
	lookupPricesFlag := fs.String("lookup-prices", os.Getenv("NEKTAR_HUB_LOOKUP_PRICES"), "Priced lookup classes (e.g. history=5,export=20)")
	spinePeersFlag := fs.String("spine-peer-pubs", os.Getenv("NEKTAR_SPINE_PEER_PUBS"), "Comma-separated bilateral Spine peer pubkeys (settlement-free peering)")
	lanesFlag := fs.String("lanes", os.Getenv("NEKTAR_HUB_LANES"), "Optional comma-separated hex nibble lanes to mount (e.g. 0,1,2,3; default all 0..f)")
	fabricUDPFlag := fs.String("brc223-udp", os.Getenv("NEKTAR_HUB_BRC223_UDP"), "UDP listen address for BRC-223 topic interest datagrams (e.g. :18423)")
	_ = fs.Parse(args)

	visited := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })

	feeSched := hub.LoadFeeSchedule(os.Getenv)
	if visited["fee-preset"] {
		feeSched = hub.DefaultFeeSchedule(*feePresetFlag)
		// Re-apply explicit env vars only if not overridden by CLI flags below.
		if envRole := strings.ToLower(strings.TrimSpace(os.Getenv("NEKTAR_HUB_ROLE"))); envRole == string(hub.RoleSpine) || envRole == string(hub.RoleLeaf) {
			feeSched.Role = hub.HubRole(envRole)
		}
	}
	if visited["hub-role"] {
		if r := strings.ToLower(strings.TrimSpace(*hubRoleFlag)); r == string(hub.RoleSpine) || r == string(hub.RoleLeaf) {
			feeSched.Role = hub.HubRole(r)
		}
	}
	if visited["want-fee-sats"] {
		feeSched.SubmitWantSats = *wantFeeFlag
	}
	if visited["offer-fee-sats"] {
		feeSched.SubmitOfferSats = *offerFeeFlag
	}
	if visited["announce-fee-sats"] {
		feeSched.SubmitAnnounceSats = *announceFeeFlag
	}
	if visited["bbox-fee-sats"] {
		feeSched.BboxSendSats = *bboxFeeFlag
	}
	if visited["egress-sats-per-100"] {
		feeSched.EgressSatsPer100Events = *egressFeeFlag
	}
	if visited["lookup-prices"] {
		feeSched.LookupPrices = hub.ParseLookupPrices(*lookupPricesFlag)
	}
	if visited["spine-peer-pubs"] {
		feeSched.SpinePeerPubs = nil
		for _, p := range strings.Split(*spinePeersFlag, ",") {
			if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
				feeSched.SpinePeerPubs = append(feeSched.SpinePeerPubs, p)
			}
		}
	}
	feeSched.Normalize()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dir := *dataDir
	if dir == "" {
		dir = defaultDataDirForNetwork(*netName)
	}
	dbPath := filepath.Join(dir, "hub.sqlite")
	if *netName == "memory" && *dataDir == "" && os.Getenv("NEKTAR_DATA_DIR") == "" {
		dbPath = ":memory:"
	}

	if *netName != "memory" {
		releaseLock, lockErr := acquireInstanceLock(dir, *addr)
		if lockErr != nil {
			return lockErr
		}
		defer releaseLock()
	}

	var upstreamSpines []string
	for _, s := range strings.Split(*upstreamFlag, ",") {
		if s = strings.TrimSpace(s); s != "" {
			upstreamSpines = append(upstreamSpines, s)
		}
	}

	var rt *runtimeBundle
	if *withLiquidity || *seedBounties || feeSched.HasAnyFees() || len(upstreamSpines) > 0 {
		var rErr error
		rt, rErr = openRuntime(ctx, *netName, *dataDir, envOr("NEKTAR_HUB_PORT", "18421"))
		if rErr != nil {
			if feeSched.HasAnyFees() {
				return fmt.Errorf("hub: wallet runtime required when fees are enabled: %w", rErr)
			}
			log.Printf("hub liquidity runtime skipped: %v", rErr)
		} else {
			defer rt.closeFn()
			_, _ = rt.mem.ReconcilePendingAnchors(ctx)
			rt.mem.Chain().StartBackground(ctx)
			stopRefresher := startAnchorProofRefresher(ctx, rt.mem)
			defer stopRefresher()
			if (*withLiquidity || *seedBounties) && rt.mem.DB() != nil && rt.mkt != nil && rt.mkt.Policy() != nil {
				hasPol := false
				if pols, pErr := rt.mem.DB().ListOperatorPolicies(); pErr == nil {
					for _, p := range pols {
						if p.Active {
							hasPol = true
							break
						}
					}
				}
				if !hasPol {
					if prof, ok := market.PolicyProfileDefaults("standard"); ok {
						ai := prof.AIMatching
						zc := prof.ZeroConfMax
						_, _ = market.ApprovePolicyCustom(rt.mem.DB(), rt.mkt.Policy(), "seller", nil, &market.PolicyLimitsOverride{
							MaxPerDeal:  prof.MaxPerDeal,
							MaxPerDay:   prof.MaxPerDay,
							ZeroConfMax: &zc,
						}, "hub-seller", &ai)
					}
				}
			}
		}
	}

	isLiveNet := *netName == "main" || *netName == "testnet"
	hCfg := hub.Config{
		Network:              *netName,
		DBPath:               dbPath,
		Lanes:                []string{*lanesFlag},
		EnableFinger:         true,
		EnableBbox:           true,
		EnableBlogs:          true,
		EnableBorg:           true,
		EnableBsecret:        true,
		EnableBstore:         true,
		EnableFabric:         true,
		FabricUDPListen:      strings.TrimSpace(*fabricUDPFlag),
		DemandDrivenUpstream: isLiveNet && feeSched.Role == hub.RoleLeaf,
		Getenv:               os.Getenv,
		Fees:                 feeSched,
		UpstreamSpines:       upstreamSpines,
	}
	if rt != nil {
		hCfg.ChainTracker = rt.mem.Chain().Tracker()
		hCfg.FeeCollector = hub.NewPortFeeCollector(rt.mem.Chain())
	} else if *netName == "main" || *netName == "testnet" {
		headerSpec := strings.TrimSpace(os.Getenv("NEKTAR_HEADERS_SOURCE"))
		if headerSpec == "" {
			headerSpec = chain.DefaultHeaderSources(*netName)
		}
		chainSpec := strings.TrimSpace(os.Getenv("NEKTAR_CHAIN_SPEC"))
		if chainSpec == "" {
			if *netName == "main" {
				chainSpec = "woc:main"
			} else {
				chainSpec = "woc:test"
			}
		}
		settleSpec := strings.TrimSpace(os.Getenv("NEKTAR_SETTLE_SPEC"))
		wt, wErr := chain.NewWitnessedTrackerFromSpec(*netName, headerSpec, chainSpec, settleSpec, core.SafeHTTPClientForNetwork(*netName, 15*time.Second))
		if wErr != nil {
			return fmt.Errorf("hub: live network %q requires independent witnessed ChainTracker: %w", *netName, wErr)
		}
		hCfg.ChainTracker = wt
	}
	node, err := hub.NewNode(hCfg)
	if err != nil {
		return err
	}
	defer node.Close()

	if rt != nil {
		node.SetEventBus(rt.mem.EventBus())
		if rt.disc != nil && rt.mkt != nil && rt.mkt.Policy() != nil {
			rt.disc.SetHubFeePolicy(rt.mkt.Policy().CanPayHubFee, rt.mkt.Policy().RecordHubFeeSpend)
		}
		if rt.disc != nil {
			node.SetHandleResolver(rt.disc.WellKnownHandleHandler("", node.DefaultBboxOffice()))
			node.SetUpstreamSubmitter(rt.disc.SubmitBEEF)
			node.SetEgressAuth(
				func(action, aud string, cursor int64, domainsCSV string) (string, string, string, string, error) {
					return discovery.SignEgressAuthHeaders(rt.disc.AnnounceKey(), action, aud, cursor, domainsCSV)
				},
				func(tCtx context.Context, spineURL string, minTopupSats uint64) error {
					_, tErr := rt.disc.TopUpHubEgressCredits(tCtx, spineURL, minTopupSats, rt.disc.AnnouncePubHex())
					return tErr
				},
			)
		}
		rt.mem.Chain().OnTxMined(func(txid string, height uint32, mp *transaction.MerklePath) {
			mCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if _, aErr := rt.mem.RefreshAnchorProofByTxID(mCtx, txid, height, mp); aErr == nil {
				_, _ = hub.RefreshHubLiquidityStock(mCtx, rt.mem)
			}
			_ = node.OnBountyPayoutMined(txid)
			_ = node.OnFeePaymentMined(txid, height)
		})
		rt.mem.Chain().OnReorg(func(forkHeight uint32, _ []string) {
			rCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			_, _ = rt.mem.HandleReorg(forkHeight)
			_ = node.HandleReorg(rCtx, forkHeight)
		})
		if *withLiquidity {
			if _, lErr := hub.ArmHubLiquidity(ctx, node, rt.mem, hub.LiquidityConfig{
				SeedStarterShards: true,
				ArmBounties:       *seedBounties,
				StarterPriceSats:  90,
				BountyRewardSats:  100,
			}); lErr != nil {
				log.Printf("hub liquidity warning: %v", lErr)
			}
		}
	}

	tMode := strings.ToLower(strings.TrimSpace(os.Getenv("NEKTAR_HUB_TRANSPORT")))
	switch tMode {
	case string(hub.TransportPlane), string(hub.TransportUnicast):
		// Propagation switched OFF by design (1BSV Paper 008 §5 / Paper 009 §4.5).
	default:
		if len(upstreamSpines) > 0 {
			node.StartPlaneRelay(ctx, upstreamSpines)
		}
	}
	catchupCtx, cancelCatchup := context.WithCancel(ctx)
	var catchupWG sync.WaitGroup
	defer func() {
		cancelCatchup()
		catchupWG.Wait()
	}()
	if catchupPeers := parseCSVEnv("NEKTAR_CATCHUP_PEERS"); len(catchupPeers) > 0 {
		catchupWG.Add(1)
		go func(peers []string) {
			defer catchupWG.Done()
			cCtx, cCancel := context.WithTimeout(catchupCtx, 45*time.Second)
			defer cCancel()
			for _, cp := range peers {
				if _, cErr := node.CatchUpFromPeer(cCtx, cp, 10); cErr != nil {
					log.Printf("hub catchup from %s: %v", cp, cErr)
				}
			}
		}(catchupPeers)
	}

	drainCtx, cancelDrain := context.WithCancel(context.Background())
	defer cancelDrain()
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           node.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
		BaseContext: func(net.Listener) context.Context {
			return drainCtx
		},
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		node.CloseSSE()
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		cancelDrain()
	}()
	log.Printf("nektar hub v%s listening on %s (network=%s role=%s fees=%s db=%s topics=%d lookups=%d liquidity=%v)",
		Version, *addr, *netName, feeSched.Role, feeSched.Preset, dbPath, len(node.Topics()), len(node.Lookups()), rt != nil && *withLiquidity)
	serveErr := httpSrv.ListenAndServe()
	if serveErr != nil && serveErr != http.ErrServerClosed {
		stop()
		<-shutdownDone
		return serveErr
	}
	<-shutdownDone
	return nil
}
