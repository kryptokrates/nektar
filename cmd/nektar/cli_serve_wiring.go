package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/server"
)

func initEmbeddedHub(
	ctx context.Context,
	rt *runtimeBundle,
	dataDirFlag, hubRoleFlag, feePresetFlag, upstreamSpinesFlag string,
	seedBounties bool,
) (*hub.Node, *hub.LiquidityState, []string, error) {
	hubDBPath := filepath.Join(rt.dataDir, "hub.sqlite")
	if rt.mem.Chain().Network() == "memory" && dataDirFlag == "" && os.Getenv("NEKTAR_DATA_DIR") == "" {
		hubDBPath = ":memory:"
	}
	feeSched := hub.LoadFeeSchedule(os.Getenv)
	if fp := strings.TrimSpace(feePresetFlag); fp != "" && fp != strings.TrimSpace(os.Getenv("NEKTAR_HUB_FEE_PRESET")) {
		feeSched = hub.DefaultFeeSchedule(fp)
	}
	if hr := strings.ToLower(strings.TrimSpace(hubRoleFlag)); hr == string(hub.RoleSpine) || hr == string(hub.RoleLeaf) {
		feeSched.Role = hub.HubRole(hr)
	}
	feeSched.Normalize()
	var upstreamSpines []string
	for _, s := range strings.Split(upstreamSpinesFlag, ",") {
		if s = strings.TrimSpace(s); s != "" {
			upstreamSpines = append(upstreamSpines, s)
		}
	}
	if len(upstreamSpines) == 0 && feeSched.Role == hub.RoleLeaf && os.Getenv("NEKTAR_UPSTREAM_SPINES") == "" {
		switch rt.mem.Chain().Network() {
		case "main":
			upstreamSpines = []string{"https://nektar.run/hub"}
		case "testnet":
			upstreamSpines = []string{"https://testnet.nektar.run/hub"}
		}
	}
	isLiveNet := rt.mem.Chain().Network() == "main" || rt.mem.Chain().Network() == "testnet"
	hubNode, err := hub.NewNode(hub.Config{
		Network:              rt.mem.Chain().Network(),
		DBPath:               hubDBPath,
		EnableFinger:         true,
		EnableBbox:           true,
		EnableBlogs:          true,
		EnableBorg:           true,
		EnableBsecret:        true,
		EnableBstore:         true,
		EnableFabric:         true,
		DemandDrivenUpstream: isLiveNet && feeSched.Role == hub.RoleLeaf,
		Getenv:               os.Getenv,
		ChainTracker:         rt.mem.Chain().Tracker(),
		HeaderHandler:        rt.mem.Chain().HeaderStoreHandler(),
		Fees:                 feeSched,
		FeeCollector:         hub.NewPortFeeCollector(rt.mem.Chain()),
		UpstreamSpines:       upstreamSpines,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	// Ensure Hub has an active zero-risk "seller" policy so curated/starter shards can be quoted and resold!
	if rt.mem.DB() != nil && rt.mkt != nil && rt.mkt.Policy() != nil {
		hasActivePol := false
		if pols, pErr := rt.mem.DB().ListOperatorPolicies(); pErr == nil {
			for _, p := range pols {
				if p.Active {
					hasActivePol = true
					break
				}
			}
		}
		if !hasActivePol {
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
	liqState, lErr := hub.ArmHubLiquidity(ctx, hubNode, rt.mem, hub.LiquidityConfig{
		SeedStarterShards: true,
		ArmBounties:       seedBounties,
		StarterPriceSats:  90,
		BountyRewardSats:  100,
	})
	if lErr != nil {
		log.Printf("hub liquidity warning: %v", lErr)
	}
	return hubNode, liqState, upstreamSpines, nil
}

func wireEmbeddedHub(ctx context.Context, rt *runtimeBundle, hubNode *hub.Node, upstreamSpines []string, pubURL string) func() {
	cleanup := func() {}
	hubNode.SetEventBus(rt.mem.EventBus())
	// Exempt this node's own local discovery & market keys from paying BRC-105 fees to its own embedded Hub.
	if rt.disc != nil {
		hubNode.AddSpinePeerPub(rt.disc.AnnouncePubHex())
		rt.disc.SetLocalHandle(os.Getenv("NEKTAR_HANDLE"), pubURL, hubNode.DefaultBboxOffice())
		hubNode.SetHandleResolver(rt.disc.WellKnownHandleHandler("", hubNode.DefaultBboxOffice()))
	}
	if rt.mkt != nil {
		hubNode.AddSpinePeerPub(rt.mkt.SalePubHex())
	}
	if rt.disc != nil && rt.mkt != nil && rt.mkt.Policy() != nil {
		rt.disc.SetHubFeePolicy(rt.mkt.Policy().CanPayHubFee, rt.mkt.Policy().RecordHubFeeSpend)
	}
	hubNode.SetUpstreamSubmitter(rt.disc.SubmitBEEF)
	hubNode.SetEgressAuth(
		func(action, aud string, cursor int64, domainsCSV string) (string, string, string, string, error) {
			return discovery.SignEgressAuthHeaders(rt.disc.AnnounceKey(), action, aud, cursor, domainsCSV)
		},
		func(tCtx context.Context, spineURL string, minTopupSats uint64) error {
			_, tErr := rt.disc.TopUpHubEgressCredits(tCtx, spineURL, minTopupSats, rt.disc.AnnouncePubHex())
			return tErr
		},
	)
	var engSrv *http.Server
	// Optional bare-origin loopback engine listener for stock lightwebinc/overlay-bridge (Paper 008 §2).
	if engineListen := strings.TrimSpace(os.Getenv("NEKTAR_HUB_ENGINE_LISTEN")); engineListen != "" {
		engSrv = &http.Server{
			Addr:              engineListen,
			Handler:           hubNode.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			if lErr := engSrv.ListenAndServe(); lErr != nil && !errors.Is(lErr, http.ErrServerClosed) {
				log.Printf("hub bare-origin engine listener (%s) error: %v", engineListen, lErr)
			}
		}()
	}
	// Hub-to-Hub transport wiring (1BSV Papers 006/008/010 — zero mesh gossip):
	//   - relay (default): Leaf connects to Spine's BRC-149 Application-Layer Object Plane (/plane/stream + /plane/nack)
	//   - plane:           External lightwebinc/overlay-bridge handles live propagation; Nektar propagation OFF
	//   - unicast:         Publisher unicast fan-out + explicit BRC-24 /lookup catch-up; Hub propagation OFF
	tMode := strings.ToLower(strings.TrimSpace(os.Getenv("NEKTAR_HUB_TRANSPORT")))
	switch tMode {
	case string(hub.TransportPlane), string(hub.TransportUnicast):
		// Propagation switched OFF by design (Paper 008 §5 / Paper 009 §4.5).
	default:
		if len(upstreamSpines) > 0 {
			hubNode.StartPlaneRelay(ctx, upstreamSpines)
		}
	}
	// Explicit offline catch-up backfill via BRC-24 /lookup (strictly separated from live propagation, Paper 008 §6).
	catchupCtx, cancelCatchup := context.WithCancel(ctx)
	var catchupWG sync.WaitGroup
	if catchupPeers := parseCSVEnv("NEKTAR_CATCHUP_PEERS"); len(catchupPeers) > 0 {
		catchupWG.Add(1)
		go func(peers []string) {
			defer catchupWG.Done()
			cCtx, cCancel := context.WithTimeout(catchupCtx, 45*time.Second)
			defer cCancel()
			for _, cp := range peers {
				if _, cErr := hubNode.CatchUpFromPeer(cCtx, cp, 10); cErr != nil {
					log.Printf("hub catchup from %s: %v", cp, cErr)
				}
			}
		}(catchupPeers)
	}
	cleanup = func() {
		cancelCatchup()
		catchupWG.Wait()
		if engSrv != nil {
			sCtx, sCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer sCancel()
			_ = engSrv.Shutdown(sCtx)
		}
	}
	return cleanup
}

// wireChainCallbacks wires Monitor verified-BUMP OnTxMined & WitnessedTracker OnReorg callbacks across memory, market broker, and hub.
func wireChainCallbacks(ctx context.Context, rt *runtimeBundle, broker *market.Broker, hubNode *hub.Node, liqState *hub.LiquidityState) {
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
		if rt.disc != nil {
			rt.disc.CarrierTree(nil).OnTxMined(txid, height, mp)
		}
		market.CompactBondWithBUMP(mCtx, rt.mem.DB(), rt.mem.Chain(), txid, mp)
		market.CompactVaultWithBUMP(mCtx, rt.mem.DB(), rt.mem.Chain(), txid, mp)
		broker.OnPaymentTxMined(mCtx, txid, height, mp)
		broker.OnTipAdvanced(mCtx, height)
		if hubNode != nil {
			_ = hubNode.OnBountyPayoutMined(txid)
			_ = hubNode.OnFeePaymentMined(txid, height)
		}
	})
	rt.mem.Chain().OnReorg(func(forkHeight uint32, _ []string) {
		rCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		_, _ = rt.mem.HandleReorg(forkHeight)
		if rt.disc != nil {
			rt.disc.CarrierTree(nil).HandleReorg(forkHeight)
		}
		broker.HandleReorg(rCtx, forkHeight)
		if hubNode != nil {
			_ = hubNode.HandleReorg(rCtx, forkHeight)
		}
	})
}

func attachRestateClient(
	ctx context.Context,
	rt *runtimeBundle,
	broker *market.Broker,
	app *server.App,
	restateClient *market.RestateClient,
	restateAdminDisplay string,
	restateAliveFn func() bool,
) func() {
	if restateClient == nil {
		return func() {}
	}
	broker.SetRestateClient(restateClient)
	if restateAdminDisplay == "" {
		restateAdminDisplay = "embedded://workflows.sqlite"
	}
	app.SetRestateClient(restateClient, restateAdminDisplay)
	if restateAliveFn != nil {
		app.SetRestateLiveness(restateAliveFn)
	}

	hookCtx, cancelHooks := context.WithCancel(ctx)
	var hookMu sync.Mutex
	var hookClosed bool
	var hookWG sync.WaitGroup
	spawnHook := func(fn func(context.Context)) {
		hookMu.Lock()
		if hookClosed {
			hookMu.Unlock()
			return
		}
		hookWG.Add(1)
		hookMu.Unlock()
		go func() {
			defer hookWG.Done()
			fn(hookCtx)
		}()
	}

	anchorCadenceSec := envInt64("NEKTAR_ANCHOR_CADENCE_SEC", 300)
	// Coalesce Remember notifications into an O(1) dirty-shard set drained on a bounded background ticker
	// (eliminating unbounded goroutine storms + per-purchase AnchorShard lock contention at 100+ TPS!).
	var dirtyMu sync.Mutex
	dirtyShards := make(map[string]struct{}, 16)
	dirtyWake := make(chan struct{}, 1)
	markShardDirty := func(shard string) {
		shard = strings.TrimSpace(shard)
		if shard == "" {
			return
		}
		dirtyMu.Lock()
		dirtyShards[shard] = struct{}{}
		dirtyMu.Unlock()
		select {
		case dirtyWake <- struct{}{}:
		default:
		}
	}
	rt.mem.SetOnRemember(markShardDirty)
	spawnHook(func(hCtx context.Context) {
		if shards, err := rt.mem.DB().ListShards(); err == nil {
			for _, sh := range shards {
				if hCtx.Err() != nil {
					return
				}
				if un, uErr := rt.mem.DB().ListUnanchored(sh); uErr == nil && len(un) > 0 {
					markShardDirty(sh)
				}
			}
		}
		tickDur := 5 * time.Second
		if anchorCadenceSec > 0 && anchorCadenceSec < 5 {
			tickDur = time.Duration(anchorCadenceSec) * time.Second
		}
		cadenceTick := time.NewTicker(tickDur)
		defer cadenceTick.Stop()
		flushDirty := func() {
			dirtyMu.Lock()
			if len(dirtyShards) == 0 {
				dirtyMu.Unlock()
				return
			}
			batch := make([]string, 0, len(dirtyShards))
			for sh := range dirtyShards {
				batch = append(batch, sh)
			}
			clear(dirtyShards)
			dirtyMu.Unlock()
			for _, sh := range batch {
				if hCtx.Err() != nil {
					return
				}
				pCtx, cancel := context.WithTimeout(hCtx, 15*time.Second)
				_, err := restateClient.TriggerAnchorCadence(pCtx, sh, anchorCadenceSec)
				st, _ := restateClient.GetAnchorCadenceStatus(pCtx, sh)
				cancel()
				if err != nil || (st != nil && st.NextSlot > 0) {
					dirtyMu.Lock()
					dirtyShards[sh] = struct{}{}
					dirtyMu.Unlock()
				}
			}
		}
		for {
			select {
			case <-hCtx.Done():
				return
			case <-dirtyWake:
				select {
				case <-hCtx.Done():
					return
				case <-time.After(250 * time.Millisecond):
				}
				flushDirty()
			case <-cadenceTick.C:
				flushDirty()
			}
		}
	})

	// Background Restate SleepCycle (idle PII-scrubbing distillation & dynamic price decay)
	// and zero-sat MicroEarner (auto-parks at 0% CPU once wallet float >= target).
	sleepSec := envInt64("NEKTAR_SLEEP_INTERVAL_SEC", market.DefaultSleepIntervalSec)
	if sleepSec < 30 {
		sleepSec = 30
	}
	targetFloat := uint64(envInt64("NEKTAR_MICROEARNER_TARGET_SATS", int64(market.DefaultMicroEarnerFloatSats)))
	spawnHook(func(hCtx context.Context) {
		sleepTicker := time.NewTicker(time.Duration(sleepSec) * time.Second)
		earnTicker := time.NewTicker(5 * time.Minute)
		defer sleepTicker.Stop()
		defer earnTicker.Stop()
		for {
			select {
			case <-hCtx.Done():
				return
			case <-sleepTicker.C:
				sCtx, cancel := context.WithTimeout(hCtx, 30*time.Second)
				_, _ = restateClient.TriggerSleepCycleAsync(sCtx, market.SleepCycleInput{
					IdleThresholdSeconds: market.DefaultSleepIdleSeconds,
					AutoAnchor:           true,
				})
				cancel()
			case <-earnTicker.C:
				if rt.mem.Chain().WalletMode() == "embedded" && rt.mem.Chain().BalanceSats() < targetFloat {
					eCtx, cancel := context.WithTimeout(hCtx, 30*time.Second)
					_, _ = restateClient.RunMicroEarner(eCtx, market.MicroEarnerInput{
						TargetFloatSats: targetFloat,
					})
					cancel()
				}
			}
		}
	})
	return func() {
		hookMu.Lock()
		hookClosed = true
		hookMu.Unlock()
		cancelHooks()
		hookWG.Wait()
	}
}

func runPostBootDiscovery(ctx context.Context, rt *runtimeBundle, broker *market.Broker) {
	// Pre-warm M=16 carrier funding tree stock on live networks so the first Want/Offer never stalls.
	if rt.disc != nil && rt.mem != nil && rt.mem.Chain() != nil && rt.mem.Chain().Network() != "memory" && rt.mem.Chain().BalanceSats() > 1000 {
		wCtx, wCancel := context.WithTimeout(ctx, 20*time.Second)
		_ = rt.disc.CarrierTree(nil).WarmStock(wCtx)
		wCancel()
	}
	_, _ = broker.AnnounceSelf(ctx)
	rawSeed := strings.TrimSpace(os.Getenv("NEKTAR_SEED_DOMAINS"))
	var seedDoms []string
	if strings.EqualFold(rawSeed, "off") || strings.EqualFold(rawSeed, "none") || rawSeed == "0" {
		seedDoms = nil
	} else if rawSeed != "" {
		seedDoms = parseCSVEnv("NEKTAR_SEED_DOMAINS")
	} else if rt.mem.Chain().Network() == "testnet" || rt.mem.Chain().Network() == "main" {
		// Layer-2 cold-start auto-seeding: discover & verify curated CHIRP packs automatically on live networks.
		seedDoms = []string{"ops", "bsv", "agents"}
	}
	if len(seedDoms) > 0 && rt.disc != nil && len(rt.disc.ActiveHosts()) > 0 {
		seedCtx, seedCancel := context.WithTimeout(ctx, 15*time.Second)
		defer seedCancel()
		if n, sErr := rt.disc.SeedFromNetwork(seedCtx, seedDoms); sErr == nil && n > 0 {
			log.Printf("seeded %d verified fragment(s) from network CHIRP packs (%s)", n, strings.Join(seedDoms, ","))
		}
	}
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

// startPeriodicReconciliationFloor runs a low-frequency 2-minute safety reconciliation floor on live networks
// for pending anchors, commercial obligations, settled 0-conf exposure, and aged unsettleable Hub fees (STATE-9).
func startPeriodicReconciliationFloor(
	ctx context.Context,
	rt *runtimeBundle,
	hubNode *hub.Node,
	isTxSettled func(txid string) bool,
) func() {
	if rt == nil || rt.mem == nil || rt.mem.Chain() == nil || rt.mem.Chain().Network() == "memory" {
		return func() {}
	}
	rCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-rCtx.Done():
				return
			case <-ticker.C:
				_, _ = rt.mem.ReconcilePendingAnchors(rCtx)
				_, _ = market.ReconcileObligations(rCtx, rt.mem.DB(), isTxSettled, time.Now().Unix())
				if tip, err := rt.mem.Chain().Height(rCtx); err == nil {
					rt.mem.Chain().ZeroConfWatch().SweepSettled(tip, rt.mem.Chain().SettleDepth())
				}
				if hubNode != nil && hubNode.Store() != nil {
					if stale, sErr := hubNode.Store().SweepExpiredFastFees(7200); sErr == nil {
						for _, e := range stale {
							_ = hubNode.Store().FlagPayer(e.PayerPub, e.TxID, "fast fee payment failed to mine within 2h")
						}
					}
				}
			}
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

func envInt64(key string, def int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def
	}
	return v
}
