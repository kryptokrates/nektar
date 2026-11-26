//go:build testnetload

// Nektar v0.4 — Real BSV Testnet Spine-and-Leaf Heavy Load & Chaos Harness (Pure Go, zero external deps).
//
// Launches 9 real compiled `nektar` processes + live auto-supervised `restate-server v1.8.1`:
//   - Spine Hub S  (:18810) : --hub --hub-role spine --fee-preset hybrid
//   - Leaf Hub A   (:18811) : --hub --hub-role leaf  --fee-preset lightbsv_egress --upstream-spines Spine
//   - Leaf Hub B   (:18812) : --hub --hub-role leaf  --fee-preset hybrid          --upstream-spines Spine
//   - Alpha        (:18820) : Direct HTTP seller+buyer on Leaf A (Restate v1.8.1)
//   - Beta         (:18821) : Direct HTTP seller+buyer on Leaf A (Restate v1.8.1)
//   - Gamma        (:18822) : Direct HTTP seller+buyer on Leaf B (Restate v1.8.1)
//   - Delta-NAT    (:18823) : NAT-only (/bbox) seller+buyer on Leaf B (Restate v1.8.1)
//   - Epsilon      (:18824) : Direct HTTP load buyer on Leaf A (Restate v1.8.1)
//   - Zeta-NAT     (:18825) : NAT-only (/bbox) load buyer on Spine S (Restate v1.8.1)
//
// Executes:
//   - Stage 0: Wallet sync, funding fan-out from Spine, FuelKeeper UTXO warmup, policy approval & sell listings
//   - Wave 1 : 120 concurrent Remembers + 12 real BSV Testnet Anchors + 1,000 offline SPV Verifies
//   - Wave 2 : Valve 2 free-rider 402 proof + gouging-hub policy cap proof + 24 parallel cross-hub paid trades
//   - Wave 3 : Mid-flight SIGKILL + restart of Leaf Hub A and Spine Hub S + post-chaos durable recovery trades
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type nodeSpec struct {
	Key         string
	Port        int
	RestatePort int
	Args        []string
	Env         []string
	Dir         string
	URL         string
}

type managedProc struct {
	spec nodeSpec
	cmd  *exec.Cmd
	logF *os.File
}

type tradeResult struct {
	Wave      string `json:"wave"`
	Buyer     string `json:"buyer"`
	Domain    string `json:"domain"`
	Route     string `json:"route"`
	Status    string `json:"status"`
	PriceSats uint64 `json:"priceSats"`
	TxID      string `json:"txid,omitempty"`
	LatencyMs int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
}

type scorecard struct {
	Network            string                 `json:"network"`
	RestateEnabled     bool                   `json:"restateEnabled"`
	StartedAt          string                 `json:"startedAt"`
	DurationSec        float64                `json:"durationSec"`
	RemembersOK        int                    `json:"remembersOk"`
	RemembersTotal     int                    `json:"remembersTotal"`
	RememberP50Ms      int64                  `json:"rememberP50Ms"`
	RememberP95Ms      int64                  `json:"rememberP95Ms"`
	AnchorsOK          int                    `json:"anchorsOk"`
	AnchorsTotal       int                    `json:"anchorsTotal"`
	AnchorTxIDs        []string               `json:"anchorTxids"`
	AnchorP50Ms        int64                  `json:"anchorP50Ms"`
	AnchorP95Ms        int64                  `json:"anchorP95Ms"`
	SPVVerifiesOK      int                    `json:"spvVerifiesOk"`
	SPVVerifiesTotal   int                    `json:"spvVerifiesTotal"`
	SPVThroughputPerS  float64                `json:"spvThroughputPerSec"`
	SPVP95Ms           int64                  `json:"spvP95Ms"`
	FreeRiderBlocked   bool                   `json:"freeRiderBlocked402"`
	GougingHubBlocked  bool                   `json:"gougingHubBlocked"`
	TradesSettled      int                    `json:"tradesSettled"`
	TradesTotal        int                    `json:"tradesTotal"`
	TradeP50Ms         int64                  `json:"tradeP50Ms"`
	TradeP95Ms         int64                  `json:"tradeP95Ms"`
	TradeP99Ms         int64                  `json:"tradeP99Ms"`
	ChaosLeafRecovery  bool                   `json:"chaosLeafRecovery"`
	ChaosSpineRecovery bool                   `json:"chaosSpineRecovery"`
	PostChaosTradesOK  int                    `json:"postChaosTradesOk"`
	HubEarnings        map[string]any         `json:"hubEarnings"`
	ProcessRSSMB       map[string]float64     `json:"processRssMb,omitempty"`
	SampleVerifiedTxs  map[string]string      `json:"sampleVerifiedTxs,omitempty"`
	FundingTxIDs       []string               `json:"fundingTxids,omitempty"`
	Trades             []tradeResult          `json:"trades"`
	Failures           []string               `json:"failures,omitempty"`
}

var httpCli = &http.Client{Timeout: 210 * time.Second}

func main() {
	binPath := flag.String("bin", "./nektar", "Path to compiled nektar binary")
	network := flag.String("network", "testnet", "Network: testnet | main | memory")
	confirmMainnet := flag.String("confirm-mainnet", "", "Must equal I_UNDERSTAND when -network=main")
	sweepBack := flag.Bool("sweep-back", false, "Sweep leftover Leaf & Agent balances back to Spine after completion")
	workDir := flag.String("work-dir", "", "Working directory containing node data dirs")
	remembersN := flag.Int("remembers", 120, "Total concurrent remember ops in Wave 1")
	anchorsN := flag.Int("anchors", 12, "Total anchor txs in Wave 1")
	verifiesN := flag.Int("verifies", 1000, "Total offline SPV recall verifications in Wave 1")
	tradesN := flag.Int("trades", 24, "Total parallel cross-hub paid trades in Wave 2")
	tradeStaggerMs := flag.Int("trade-stagger-ms", 0, "Optional stagger (ms) between trade launches inside a wave")
	useRestate := flag.Bool("restate", true, "Require live Restate v1.8.1 on all 6 trading agents")
	reportPath := flag.String("report", "", "Optional path to write JSON scorecard")
	flag.Parse()

	isMain := *network == "main"
	isLive := *network == "testnet" || isMain
	if isMain && *confirmMainnet != "I_UNDERSTAND" {
		fatal("refusing -network=main without -confirm-mainnet=I_UNDERSTAND")
	}

	if *workDir == "" {
		d, err := os.MkdirTemp("", "nektar-load-*")
		if err != nil {
			fatal("mktemp: %v", err)
		}
		*workDir = d
	}

	absBin, err := filepath.Abs(*binPath)
	if err != nil {
		fatal("bin path: %v", err)
	}
	if _, err := os.Stat(absBin); err != nil {
		fatal("binary not found at %s: %v", absBin, err)
	}

	startAll := time.Now()
	sc := &scorecard{
		Network:           *network,
		RestateEnabled:    *useRestate,
		StartedAt:         startAll.UTC().Format(time.RFC3339),
		RemembersTotal:    *remembersN,
		AnchorsTotal:      *anchorsN,
		SPVVerifiesTotal:  *verifiesN,
		TradesTotal:       *tradesN,
		HubEarnings:       make(map[string]any),
		ProcessRSSMB:      make(map[string]float64),
		SampleVerifiedTxs: make(map[string]string),
	}

	spineURL := "http://127.0.0.1:18810"
	leafAURL := "http://127.0.0.1:18811"
	leafBURL := "http://127.0.0.1:18812"
	spineHub := spineURL + "/hub"
	leafAHub := leafAURL + "/hub"
	leafBHub := leafBURL + "/hub"

	commonEnv := []string{
		"NEKTAR_WALLET_PROFILE=throughput",
		"NEKTAR_JEV_MODE=off",
		"NEKTAR_ALLOW_LOCAL_PEERS=1",
		"NEKTAR_ANCHOR_CADENCE_SEC=3600",
	}
	if isMain {
		commonEnv = append(commonEnv,
			"NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND",
			"NEKTAR_SETTLE_SPEC=broadcaster:main",
			"NEKTAR_SETTLE_DEPTH=1",
			"NEKTAR_MAX_SPEND_SATS_PER_DAY=60000",
			"NEKTAR_MAX_FEE_SATS_PER_TX=2000",
			"NEKTAR_FEE_RATE_SAT_KB=100",
		)
	} else {
		commonEnv = append(commonEnv,
			"NEKTAR_MAX_SPEND_SATS_PER_DAY=2000000",
			"NEKTAR_MAX_FEE_SATS_PER_TX=15000",
			"NEKTAR_FEE_RATE_SAT_KB=150",
		)
	}
	// When 9 processes share 1 IP against public unkeyed WhatsOnChain (3 req/s limit),
	// run a shared read-through Chaintracks v2 header cache proxy on 127.0.0.1:18809
	// backed 100% by real api.whatsonchain.com/v1/bsv/<net> block headers (paced at <2 req/s).
	// On Mainnet, pair it with official Babbage Chaintracks v2 for strict >=2 distinct-origin PoW quorum!
	// Every Nektar process still verifies 100% of 80-byte PoW + Prev-linkage locally.
	if isLive {
		wocNet := "test"
		if isMain {
			wocNet = "main"
		}
		stopHdrProxy := startWoCReadThroughHeaderProxy("127.0.0.1:18809", *workDir, wocNet)
		defer stopHdrProxy()
		if isMain {
			commonEnv = append(commonEnv, "NEKTAR_HEADERS_SOURCE=chaintracks:http://127.0.0.1:18809/v2,chaintracks:https://mainnet-chaintracks.babbage.systems/v2")
		} else {
			commonEnv = append(commonEnv, "NEKTAR_HEADERS_SOURCE=chaintracks:http://127.0.0.1:18809/v2")
		}
	}
	hubEnv := append(append([]string(nil), commonEnv...), "NEKTAR_RESTATE_DISABLE=1")
	agentEnv := append([]string(nil), commonEnv...)
	if *useRestate {
		agentEnv = append(agentEnv, "NEKTAR_RESTATE_REQUIRED=1")
	} else {
		agentEnv = append(agentEnv, "NEKTAR_RESTATE_DISABLE=1")
	}

	specs := map[string]nodeSpec{
		"spine": {
			Key: "spine", Port: 18810, URL: spineURL, Dir: filepath.Join(*workDir, "spine"),
			Args: []string{"serve", "--hub", "--hub-role", "spine", "--fee-preset", "hybrid",
				"--listen", "127.0.0.1:18810", "--restate-listen", "127.0.0.1:19810",
				"--public-url", spineURL, "--data-dir", filepath.Join(*workDir, "spine"), "--network", *network},
			Env: hubEnv,
		},
		"leaf-a": {
			Key: "leaf-a", Port: 18811, URL: leafAURL, Dir: filepath.Join(*workDir, "leaf-a"),
			Args: []string{"serve", "--hub", "--hub-role", "leaf", "--fee-preset", "lightbsv_egress",
				"--upstream-spines", spineHub,
				"--listen", "127.0.0.1:18811", "--restate-listen", "127.0.0.1:19811",
				"--public-url", leafAURL, "--data-dir", filepath.Join(*workDir, "leaf-a"), "--network", *network},
			Env: hubEnv,
		},
		"leaf-b": {
			Key: "leaf-b", Port: 18812, URL: leafBURL, Dir: filepath.Join(*workDir, "leaf-b"),
			Args: []string{"serve", "--hub", "--hub-role", "leaf", "--fee-preset", "hybrid",
				"--upstream-spines", spineHub,
				"--listen", "127.0.0.1:18812", "--restate-listen", "127.0.0.1:19812",
				"--public-url", leafBURL, "--data-dir", filepath.Join(*workDir, "leaf-b"), "--network", *network},
			Env: hubEnv,
		},
		"alpha": {
			Key: "alpha", Port: 18820, RestatePort: 19820, URL: "http://127.0.0.1:18820", Dir: filepath.Join(*workDir, "alpha"),
			Args: []string{"serve", "--listen", "127.0.0.1:18820", "--restate-listen", "127.0.0.1:19820",
				"--public-url", "http://127.0.0.1:18820", "--hosts", leafAHub,
				"--data-dir", filepath.Join(*workDir, "alpha"), "--network", *network},
			Env: agentEnv,
		},
		"beta": {
			Key: "beta", Port: 18821, RestatePort: 19821, URL: "http://127.0.0.1:18821", Dir: filepath.Join(*workDir, "beta"),
			Args: []string{"serve", "--listen", "127.0.0.1:18821", "--restate-listen", "127.0.0.1:19821",
				"--public-url", "http://127.0.0.1:18821", "--hosts", leafAHub,
				"--data-dir", filepath.Join(*workDir, "beta"), "--network", *network},
			Env: agentEnv,
		},
		"gamma": {
			Key: "gamma", Port: 18822, RestatePort: 19822, URL: "http://127.0.0.1:18822", Dir: filepath.Join(*workDir, "gamma"),
			Args: []string{"serve", "--listen", "127.0.0.1:18822", "--restate-listen", "127.0.0.1:19822",
				"--public-url", "http://127.0.0.1:18822", "--hosts", leafBHub,
				"--data-dir", filepath.Join(*workDir, "gamma"), "--network", *network},
			Env: agentEnv,
		},
		"delta-nat": {
			Key: "delta-nat", Port: 18823, RestatePort: 19823, URL: "http://127.0.0.1:18823", Dir: filepath.Join(*workDir, "delta-nat"),
			Args: []string{"serve", "--listen", "127.0.0.1:18823", "--restate-listen", "127.0.0.1:19823",
				"--hosts", leafBHub,
				"--data-dir", filepath.Join(*workDir, "delta-nat"), "--network", *network},
			Env: agentEnv,
		},
		"epsilon": {
			Key: "epsilon", Port: 18824, RestatePort: 19824, URL: "http://127.0.0.1:18824", Dir: filepath.Join(*workDir, "epsilon"),
			Args: []string{"serve", "--listen", "127.0.0.1:18824", "--restate-listen", "127.0.0.1:19824",
				"--public-url", "http://127.0.0.1:18824", "--hosts", leafAHub,
				"--data-dir", filepath.Join(*workDir, "epsilon"), "--network", *network},
			Env: agentEnv,
		},
		"zeta-nat": {
			Key: "zeta-nat", Port: 18825, RestatePort: 19825, URL: "http://127.0.0.1:18825", Dir: filepath.Join(*workDir, "zeta-nat"),
			Args: []string{"serve", "--listen", "127.0.0.1:18825", "--restate-listen", "127.0.0.1:19825",
				"--hosts", spineHub,
				"--data-dir", filepath.Join(*workDir, "zeta-nat"), "--network", *network},
			Env: agentEnv,
		},
	}

	procs := make(map[string]*managedProc)
	var procMu sync.Mutex
	cleanup := func() {
		procMu.Lock()
		defer procMu.Unlock()
		for k, mp := range procs {
			stopProc(mp, false)
			delete(procs, k)
		}
	}
	defer cleanup()

	startNode := func(key string) {
		sp := specs[key]
		if err := os.MkdirAll(sp.Dir, 0o750); err != nil {
			fatal("mkdir %s: %v", sp.Dir, err)
		}
		logPath := filepath.Join(sp.Dir, "load-node.log")
		lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		if err != nil {
			fatal("open log %s: %v", logPath, err)
		}
		cmd := exec.Command(absBin, sp.Args...)
		cmd.Env = append(os.Environ(), sp.Env...)
		cmd.Stdout = lf
		cmd.Stderr = lf
		t0 := time.Now()
		if err := cmd.Start(); err != nil {
			_ = lf.Close()
			fatal("start %s: %v", key, err)
		}
		mp := &managedProc{spec: sp, cmd: cmd, logF: lf}
		procMu.Lock()
		procs[key] = mp
		procMu.Unlock()

		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			if out, err := httpJSON(http.MethodGet, sp.URL+"/v1/health", nil); err == nil {
				if ok, _ := out["ok"].(bool); ok {
					fmt.Printf("[BOOT] %-10s healthy at %s (pid=%d, %dms)\n", key, sp.URL, cmd.Process.Pid, time.Since(t0).Milliseconds())
					return
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		tail := tailFile(logPath, 30)
		fatal("%s failed to become healthy within 45s:\n%s", key, tail)
	}

	fmt.Printf("\n===========================================================================\n")
	fmt.Printf(" NEKTAR v0.4 — SPINE-AND-LEAF HEAVY LOAD & CHAOS TEST (%s)\n", strings.ToUpper(*network))
	fmt.Printf(" 1 Spine (:18810) + 2 Leaves (:18811,:18812) + 6 Agents (:18820..:18825)\n")
	fmt.Printf(" Restate v1.8.1 Required: %v | WorkDir: %s\n", *useRestate, *workDir)
	fmt.Printf("===========================================================================\n\n")

	// Boot 3 Hubs first so Spine & Leaves are listening before agents connect.
	startNode("spine")
	startNode("leaf-a")
	startNode("leaf-b")

	// Verify Spine-and-Leaf terms documents on all 3 hubs!
	for _, hKey := range []string{"spine", "leaf-a", "leaf-b"} {
		terms, err := httpJSON(http.MethodGet, specs[hKey].URL+"/hub/v1/terms", nil)
		if err != nil {
			fatal("%s /hub/v1/terms failed: %v", hKey, err)
		}
		feesMap, _ := terms["fees"].(map[string]any)
		egMap, _ := terms["egress"].(map[string]any)
		fmt.Printf("[HUB ] %-10s role=%v submitWant=%v submitOffer=%v bbox=%v egressPer100=%v minTopup=%v\n",
			hKey, terms["hubRole"], feesMap["submit:want"], feesMap["submit:offer"], feesMap["bbox:send"], egMap["satsPer100Events"], egMap["minTopupSats"])
	}

	// Boot all 6 trading agents (staggered 350ms so Restate free-port selection never races).
	agentOrder := []string{"alpha", "beta", "gamma", "delta-nat", "epsilon", "zeta-nat"}
	for _, k := range agentOrder {
		startNode(k)
		time.Sleep(350 * time.Millisecond)
	}

	// =========================================================================
	// STAGE 0: Wallet Sync, P2P BRC-29 Funding Fan-Out & FuelKeeper UTXO Warmup
	// =========================================================================
	fmt.Printf("\n=== Stage 0: Wallet Sync, P2P BRC-29 Funding Fan-Out & FuelKeeper UTXO Warmup ===\n")
	allKeys := append([]string{"spine", "leaf-a", "leaf-b"}, agentOrder...)
	wallets := make(map[string]map[string]any)
	getWallet := func(k string) map[string]any {
		w, err := httpJSON(http.MethodGet, specs[k].URL+"/v1/wallet", nil)
		if err != nil {
			fatal("getWallet %s: %v", k, err)
		}
		wallets[k] = w
		return w
	}

	// Sync any node with unproven UTXOs so newly mined BSV blocks reset depth!
	if isLive {
		for _, k := range allKeys {
			w := getWallet(k)
			if asUint64(w["maxUnprovenDepth"]) > 3 || (isMain && asUint64(w["unprovenSats"]) > 0) {
				if sr, sErr := httpJSON(http.MethodPost, specs[k].URL+"/v1/wallet/sync", map[string]any{}); sErr == nil {
					fmt.Printf("  [PROOF] %-10s proved=%v confirmed=%v unproven=%v depth=%v\n",
						k, sr["proved"], sr["confirmedSats"], sr["unprovenSats"], getWallet(k)["maxUnprovenDepth"])
				}
				time.Sleep(400 * time.Millisecond)
			}
		}
	}
	spineNeedMin := uint64(40000)
	if isMain {
		spineNeedMin = 45000
	}
	sw := getWallet("spine")
	if isLive && asUint64(sw["balanceSats"]) < spineNeedMin {
		fmt.Printf("  Waiting 6s past boot sync throttle, then syncing Spine wallet from BSV %s...\n", strings.ToUpper(*network))
		time.Sleep(6 * time.Second)
		syncRes, sErr := httpJSON(http.MethodPost, specs["spine"].URL+"/v1/wallet/sync", map[string]any{})
		if sErr != nil {
			fmt.Printf("  [WARN] spine sync error: %v\n", sErr)
		} else {
			fmt.Printf("  [SYNC] spine sync ok: added=%v proved=%v balance=%v\n", syncRes["added"], syncRes["proved"], syncRes["balanceSats"])
		}
		sw = getWallet("spine")
	}
	fmt.Printf("  %-10s addr=%v  bal=%v (conf=%v depth=%v) utxos=%v fuel=%v\n", "spine", sw["fundAddress"], sw["balanceSats"], sw["confirmedSats"], sw["maxUnprovenDepth"], sw["utxos"], sw["fuelCount"])
	for _, k := range allKeys[1:] {
		w := getWallet(k)
		fmt.Printf("  %-10s addr=%v  bal=%v (conf=%v depth=%v) utxos=%v fuel=%v\n", k, w["fundAddress"], w["balanceSats"], w["confirmedSats"], w["maxUnprovenDepth"], w["utxos"], w["fuelCount"])
	}

	if isLive {
		spineBal := asUint64(wallets["spine"]["balanceSats"])
		// Count how many non-spine nodes still need funding to determine if Spine actually needs the full fan-out float.
		leafMin, leafTarget := uint64(55000), uint64(70000)
		agentMin, agentTarget := uint64(30000), uint64(42000)
		if isMain {
			leafMin, leafTarget = 3000, 5000
			agentMin, agentTarget = 2000, 3500
		}
		var needFanoutTotal uint64
		for _, k := range allKeys[1:] {
			minB, tgtB := agentMin, agentTarget
			if k == "leaf-a" || k == "leaf-b" {
				minB, tgtB = leafMin, leafTarget
			}
			if asUint64(wallets[k]["balanceSats"]) < minB {
				needFanoutTotal += tgtB + 60
			}
		}
		if spineBal < needFanoutTotal+4000 {
			fatal("Spine wallet %v needs >= %d %s sats for fan-out + reserve (currently %d)",
				wallets["spine"]["fundAddress"], needFanoutTotal+4000, strings.ToUpper(*network), spineBal)
		}
		if !isMain && asUint64(wallets["spine"]["fuelCount"]) < 16 {
			_, _ = httpJSON(http.MethodPost, specs["spine"].URL+"/v1/wallet/fuel", map[string]any{"target": 16, "sats": 10000})
			time.Sleep(1500 * time.Millisecond)
		}

		for _, k := range allKeys[1:] {
			needMin, targetAmt := agentMin, agentTarget
			if k == "leaf-a" || k == "leaf-b" {
				needMin, targetAmt = leafMin, leafTarget
			}
			bal := asUint64(wallets[k]["balanceSats"])
			if bal < needMin {
				idPub := fmt.Sprint(wallets[k]["identityPub"])
				sendBody := map[string]any{
					"toIdentityKey": idPub,
					"sats":          targetAmt,
					"description":   "load-test BRC-29 fund " + k,
				}
				if isMain {
					sendBody["confirm"] = "I_UNDERSTAND"
				}
				sendRes, err := httpJSON(http.MethodPost, specs["spine"].URL+"/v1/wallet/send", sendBody)
				if err != nil {
					fatal("BRC-29 fund %s (%s): %v", k, idPub, err)
				}
				txid := fmt.Sprint(sendRes["txid"])
				sc.FundingTxIDs = append(sc.FundingTxIDs, txid)
				impRes, iErr := httpJSON(http.MethodPost, specs[k].URL+"/v1/wallet/import", map[string]any{
					"beefHex":           sendRes["beefHex"],
					"outputIndex":       sendRes["outputIndex"],
					"derivationPrefix":  sendRes["derivationPrefix"],
					"derivationSuffix":  sendRes["derivationSuffix"],
					"senderIdentityKey": sendRes["senderIdentityKey"],
					"description":       "load-test BRC-29 funding from Spine",
				})
				if iErr != nil {
					fatal("BRC-29 import on %s (tx=%s): %v", k, txid, iErr)
				}
				w := getWallet(k)
				fmt.Printf("  [BRC-29] spine -> %-10s +%v sats (bal=%v) txid=%s\n", k, impRes["sats"], w["balanceSats"], shortTx(txid))
				time.Sleep(250 * time.Millisecond)
			}
		}
	}

	// Warm flat 1-tx FuelKeeper parallel UTXO pools so hubs & agents never chain unconfirmed default change!
	fmt.Printf("  Checking FuelKeeper parallel UTXO pools...\n")
	for _, k := range allKeys {
		fc := asUint64(getWallet(k)["fuelCount"])
		wantFuel := uint64(24)
		fuelDenom := uint64(1500)
		if isMain {
			wantFuel = 6
			fuelDenom = 350
			if k == "leaf-a" || k == "leaf-b" {
				wantFuel = 8
				fuelDenom = 400
			} else if k == "spine" {
				wantFuel = 8
				fuelDenom = 500
			}
		} else if k == "leaf-a" || k == "leaf-b" {
			wantFuel = 42
		}
		if (isMain || k != "spine") && fc < wantFuel-2 {
			_, _ = httpJSON(http.MethodPost, specs[k].URL+"/v1/wallet/fuel", map[string]any{"target": wantFuel, "sats": fuelDenom})
			time.Sleep(300 * time.Millisecond)
		}
	}
	time.Sleep(1500 * time.Millisecond)
	for _, k := range allKeys {
		w := getWallet(k)
		fmt.Printf("  [POOL] %-10s bal=%v (conf=%v depth=%v) utxos=%v fuel=%v\n", k, w["balanceSats"], w["confirmedSats"], w["maxUnprovenDepth"], w["utxos"], w["fuelCount"])
	}

	// Approve operator buyer+seller policy on all nodes (including Leaf Hubs for Spine fee caps!),
	// and prune any stale legacy host (e.g. 127.0.0.1:18620) carried over from staged DBs.
	for _, k := range allKeys {
		polBody := map[string]any{"preset": "both"}
		if isMain {
			polBody["confirm"] = "I_UNDERSTAND"
		}
		if _, err := httpJSON(http.MethodPost, specs[k].URL+"/v1/onboard/policy-preset", polBody); err != nil {
			fatal("policy-preset %s: %v", k, err)
		}
		_, _ = httpJSON(http.MethodDelete, specs[k].URL+"/v1/hosts?url=http://127.0.0.1:18620/hub", nil)
	}

	// Ensure v0.4 Rúnar-covenant seller shards exist, are anchored, AND have verified mined BUMPs!
	type sellSpec struct {
		Node        string
		Shard       string
		Domains     []string
		Price       uint64
		Description string
		FallbackTxt string
	}
	sellers := []sellSpec{
		{Node: "alpha", Shard: "v04-ops-alpha", Domains: []string{"ops"}, Price: 40, Description: "Alpha v0.4 ops runbook", FallbackTxt: "Alpha ops runbook: zero-downtime blue-green Kubernetes rollouts use PodDisruptionBudget."},
		{Node: "beta", Shard: "v04-ops-beta", Domains: []string{"ops"}, Price: 35, Description: "Beta v0.4 cheaper ops runbook", FallbackTxt: "Beta ops runbook: etcd snapshots rotate every 15m with zstd-3 compression."},
		{Node: "beta", Shard: "v04-bsv-beta", Domains: []string{"bsv"}, Price: 45, Description: "Beta v0.4 BSV Broadcaster policy", FallbackTxt: "Beta BSV Broadcaster policy: BSV Broadcaster accepts EF transactions at 100 sat/kB."},
		{Node: "gamma", Shard: "v04-agents-gamma", Domains: []string{"agents"}, Price: 40, Description: "Gamma v0.4 multi-agent architecture", FallbackTxt: "Gamma multi-agent architecture: Cursor MCP + L0 transparent proxy + BRC-369 Borg encryption."},
		{Node: "delta-nat", Shard: "v04-nat-delta", Domains: []string{"nat"}, Price: 35, Description: "Delta v0.4 NAT-only A2A bbox relay", FallbackTxt: "Delta NAT-only A2A agents settle paid knowledge trades over Hub /bbox store-and-forward relay with zero open ports."},
	}
	proveShardSettled := func(nodeKey, shard string) bool {
		res, err := httpJSON(http.MethodPost, specs[nodeKey].URL+"/v1/tools/prove", map[string]any{"shard": shard, "seq": 1})
		if err != nil {
			return false
		}
		v, _ := res["verify"].(map[string]any)
		ok, _ := v["ok"].(bool)
		settled, _ := v["settled"].(bool)
		return ok && settled
	}
	var pendingMine []sellSpec
	for _, s := range sellers {
		if proveShardSettled(s.Node, s.Shard) {
			fmt.Printf("  [SEED] %-10s/%-16s already v0.4 SPV-mined & verified!\n", s.Node, s.Shard)
			continue
		}
		// Check if fragment #1 already exists (just awaiting block confirmation); otherwise remember + anchor it.
		recRes, recErr := httpJSON(http.MethodPost, specs[s.Node].URL+"/v1/recall", map[string]any{"shard": s.Shard})
		hasFrag := false
		if recErr == nil {
			if items, ok := recRes["items"].([]any); ok && len(items) > 0 {
				hasFrag = true
			}
		}
		if !hasFrag {
			if _, err := httpJSON(http.MethodPost, specs[s.Node].URL+"/v1/remember", map[string]any{
				"shard": s.Shard, "text": s.FallbackTxt, "autoAnchor": true,
			}); err != nil {
				fatal("seed %s/%s: %v", s.Node, s.Shard, err)
			}
			// Trigger explicit anchor in case cadence didn't fire yet (ignore empty/already-anchored).
			_, _ = httpJSON(http.MethodPost, specs[s.Node].URL+"/v1/anchor", map[string]any{"shard": s.Shard})
			fmt.Printf("  [SEED] %-10s/%-16s remembered & anchored fresh v0.4 Rúnar covenant!\n", s.Node, s.Shard)
		} else {
			_, _ = httpJSON(http.MethodPost, specs[s.Node].URL+"/v1/anchor", map[string]any{"shard": s.Shard})
			fmt.Printf("  [SEED] %-10s/%-16s exists; waiting for BSV %s block BUMP...\n", s.Node, s.Shard, strings.ToUpper(*network))
		}
		pendingMine = append(pendingMine, s)
	}
	if isLive && len(pendingMine) > 0 {
		fmt.Printf("  Waiting for BSV %s block to mine %d fresh v0.4 seller covenant anchor(s)...\n", strings.ToUpper(*network), len(pendingMine))
		mineDeadline := time.Now().Add(30 * time.Minute)
		for len(pendingMine) > 0 && time.Now().Before(mineDeadline) {
			// Sync unique seller wallets so Monitor.CheckForProofs -> OnTxMined upgrades anchor BUMPs!
			synced := make(map[string]bool)
			for _, s := range pendingMine {
				if !synced[s.Node] {
					synced[s.Node] = true
					_, _ = httpJSON(http.MethodPost, specs[s.Node].URL+"/v1/wallet/sync", map[string]any{})
				}
			}
			var still []sellSpec
			for _, s := range pendingMine {
				if proveShardSettled(s.Node, s.Shard) {
					fmt.Printf("  [MINED] %-10s/%-16s BUMP verified against live BSV %s PoW!\n", s.Node, s.Shard, strings.ToUpper(*network))
				} else {
					still = append(still, s)
				}
			}
			pendingMine = still
			if len(pendingMine) > 0 {
				time.Sleep(15 * time.Second)
			}
		}
		if len(pendingMine) > 0 {
			fatal("timed out waiting for BSV %s block to mine %d seller shard(s)", strings.ToUpper(*network), len(pendingMine))
		}
	}
	for _, s := range sellers {
		if _, err := httpJSON(http.MethodPost, specs[s.Node].URL+"/v1/tools/sell", map[string]any{
			"shard":       s.Shard,
			"domains":     s.Domains,
			"priceSats":   s.Price,
			"description": s.Description,
		}); err != nil {
			fatal("sell %s/%s: %v", s.Node, s.Shard, err)
		}
	}
	// Let Leaf <-> Spine SSE federation and initial announcements settle!
	fmt.Printf("  Waiting 4s for Spine <-> Leaf A / Leaf B federation & agent SSE handshakes...\n")
	time.Sleep(4 * time.Second)

	// =========================================================================
	// WAVE 1: 120 Concurrent Remembers + 12 Real Anchors + 1,000 SPV Verifies
	// =========================================================================
	fmt.Printf("\n=== Wave 1: %d Remembers + %d Real %s Anchors + %d Offline SPV Verifies ===\n",
		*remembersN, *anchorsN, strings.ToUpper(*network), *verifiesN)
	runTag := fmt.Sprintf("load-%d", time.Now().Unix()%100000)

	// 1a. Concurrent Remember Storm
	var remMu sync.Mutex
	var remLat []int64
	var remOK int64
	perAgentRem := *remembersN / len(agentOrder)
	if perAgentRem < 1 {
		perAgentRem = 1
	}
	var wg sync.WaitGroup
	semRem := make(chan struct{}, 18)
	for ai, k := range agentOrder {
		for j := 0; j < perAgentRem; j++ {
			wg.Add(1)
			go func(nodeKey string, idx, shardIdx int) {
				defer wg.Done()
				semRem <- struct{}{}
				defer func() { <-semRem }()
				sh := fmt.Sprintf("load-%s-%d", runTag, shardIdx)
				t0 := time.Now()
				_, err := httpJSON(http.MethodPost, specs[nodeKey].URL+"/v1/remember", map[string]any{
					"shard": sh,
					"text":  fmt.Sprintf("LoadTest[%s] agent=%s seq=%d: verified Spine-and-Leaf throughput fragment #%d.", runTag, nodeKey, idx, idx),
				})
				ms := time.Since(t0).Milliseconds()
				remMu.Lock()
				if err == nil {
					atomic.AddInt64(&remOK, 1)
					remLat = append(remLat, ms)
				} else {
					sc.Failures = append(sc.Failures, fmt.Sprintf("remember %s#%d: %v", nodeKey, idx, err))
				}
				remMu.Unlock()
			}(k, j, (ai*2)+(j%2))
		}
	}
	wg.Wait()
	sc.RemembersOK = int(remOK)
	sc.RememberP50Ms = percentile(remLat, 50)
	sc.RememberP95Ms = percentile(remLat, 95)
	fmt.Printf("[WAVE 1a] Remembers: %d/%d OK | p50=%dms p95=%dms\n", sc.RemembersOK, sc.RemembersTotal, sc.RememberP50Ms, sc.RememberP95Ms)

	// 1b. Real On-Chain Anchor Burst (2 shards per agent = 12 live NektarAnchor txs!)
	var ancMu sync.Mutex
	var ancLat []int64
	var ancOK int64
	ancTarget := *anchorsN
	for ai, k := range agentOrder {
		for sOff := 0; sOff < 2; sOff++ {
			if ai*2+sOff >= ancTarget {
				break
			}
			wg.Add(1)
			go func(nodeKey string, shardIdx int) {
				defer wg.Done()
				sh := fmt.Sprintf("load-%s-%d", runTag, shardIdx)
				t0 := time.Now()
				res, err := httpJSON(http.MethodPost, specs[nodeKey].URL+"/v1/anchor", map[string]any{"shard": sh})
				ms := time.Since(t0).Milliseconds()
				ancMu.Lock()
				if err == nil {
					txid := fmt.Sprint(res["txid"])
					if txid == "" || txid == "<nil>" {
						if ancObj, ok := res["anchor"].(map[string]any); ok {
							txid = fmt.Sprint(ancObj["txid"])
						}
					}
					// If Restate AnchorCadence already anchored the shard right after Remember, read anchorTxid via /v1/recall!
					if txid == "" || txid == "<nil>" {
						if rec, rErr := httpJSON(http.MethodPost, specs[nodeKey].URL+"/v1/recall", map[string]any{"shard": sh}); rErr == nil {
							items, _ := rec["items"].([]any)
							if len(items) == 0 {
								items, _ = rec["fragments"].([]any)
							}
							for _, raw := range items {
								if m, ok := raw.(map[string]any); ok {
									if s, _ := m["anchorTxid"].(string); s != "" {
										txid = s
										break
									}
								}
							}
						}
					}
					if txid != "" && txid != "<nil>" {
						atomic.AddInt64(&ancOK, 1)
						sc.AnchorTxIDs = append(sc.AnchorTxIDs, txid)
						ancLat = append(ancLat, ms)
					} else {
						msg := fmt.Sprintf("anchor %s/%s: empty txid (%v)", nodeKey, sh, res)
						sc.Failures = append(sc.Failures, msg)
						fmt.Printf("  [ANCHOR FAIL] %s\n", msg)
					}
				} else {
					msg := fmt.Sprintf("anchor %s/%s: %v", nodeKey, sh, err)
					sc.Failures = append(sc.Failures, msg)
					fmt.Printf("  [ANCHOR FAIL] %s\n", msg)
				}
				ancMu.Unlock()
			}(k, ai*2+sOff)
			time.Sleep(150 * time.Millisecond) // slight stagger to avoid local SQLite bursts
		}
	}
	wg.Wait()
	sc.AnchorsOK = int(ancOK)
	sc.AnchorP50Ms = percentile(ancLat, 50)
	sc.AnchorP95Ms = percentile(ancLat, 95)
	fmt.Printf("[WAVE 1b] Anchors  : %d/%d OK | p50=%dms p95=%dms | txids=%d\n", sc.AnchorsOK, sc.AnchorsTotal, sc.AnchorP50Ms, sc.AnchorP95Ms, len(sc.AnchorTxIDs))

	// 1c. 1,000 Offline SPV Recall Verifications against v0.4 mined shards!
	spvTargets := []struct{ node, shard string }{
		{"alpha", "v04-ops-alpha"},
		{"beta", "v04-ops-beta"},
		{"beta", "v04-bsv-beta"},
		{"gamma", "v04-agents-gamma"},
		{"delta-nat", "v04-nat-delta"},
	}
	var spvMu sync.Mutex
	var spvLat []int64
	var spvOK int64
	tSPVStart := time.Now()
	semSPV := make(chan struct{}, 24)
	for i := 0; i < *verifiesN; i++ {
		wg.Add(1)
		tgt := spvTargets[i%len(spvTargets)]
		go func(nodeKey, shard string) {
			defer wg.Done()
			semSPV <- struct{}{}
			defer func() { <-semSPV }()
			t0 := time.Now()
			res, err := httpJSON(http.MethodPost, specs[nodeKey].URL+"/v1/recall", map[string]any{
				"shard":     shard,
				"verifySpv": true,
			})
			ms := time.Since(t0).Milliseconds()
			ok := false
			if err == nil {
				if items, isArr := res["items"].([]any); isArr && len(items) > 0 {
					ok = true
				} else if frags, isArr := res["fragments"].([]any); isArr && len(frags) > 0 {
					ok = true
				}
			}
			spvMu.Lock()
			if ok {
				atomic.AddInt64(&spvOK, 1)
				spvLat = append(spvLat, ms)
			} else if err != nil && len(sc.Failures) < 40 {
				sc.Failures = append(sc.Failures, fmt.Sprintf("spv %s/%s: %v", nodeKey, shard, err))
			}
			spvMu.Unlock()
		}(tgt.node, tgt.shard)
	}
	wg.Wait()
	spvSec := time.Since(tSPVStart).Seconds()
	sc.SPVVerifiesOK = int(spvOK)
	sc.SPVP95Ms = percentile(spvLat, 95)
	if spvSec > 0 {
		sc.SPVThroughputPerS = float64(sc.SPVVerifiesOK) / spvSec
	}
	fmt.Printf("[WAVE 1c] SPV Verify: %d/%d OK | %.1f verifies/sec | p95=%dms\n", sc.SPVVerifiesOK, sc.SPVVerifiesTotal, sc.SPVThroughputPerS, sc.SPVP95Ms)

	// =========================================================================
	// WAVE 2: Anti-Freerider Proofs + 24 Parallel Cross-Hub Paid Trades (Restate!)
	// =========================================================================
	fmt.Printf("\n=== Wave 2: Spine-and-Leaf Anti-Freerider + %d Parallel Cross-Hub Paid Trades ===\n", *tradesN)

	// 2a. Valve 2 Free-Rider Proof against Spine S:
	// Unauthenticated /sync MUST fail closed (HTTP 401/402), preventing anonymous order-flow scraping!
	frResp, frErr := httpCli.Get(spineHub + "/sync?cursor=0")
	if frErr == nil {
		reqSats := frResp.Header.Get("X-BSV-Payment-Satoshis-Required")
		_, _ = io.Copy(io.Discard, frResp.Body)
		_ = frResp.Body.Close()
		if frResp.StatusCode == http.StatusUnauthorized || frResp.StatusCode == http.StatusPaymentRequired {
			sc.FreeRiderBlocked = true
			fmt.Printf("[VALVE 2] Unauthenticated free-rider GET /sync -> HTTP %d (blocked; hdr=%q) PASS\n", frResp.StatusCode, reqSats)
		} else {
			sc.Failures = append(sc.Failures, fmt.Sprintf("free-rider /sync expected 401/402, got %d (hdr=%q)", frResp.StatusCode, reqSats))
			fmt.Printf("[FAIL   ] Free-rider /sync got HTTP %d\n", frResp.StatusCode)
		}
	} else {
		sc.Failures = append(sc.Failures, fmt.Sprintf("free-rider probe error: %v", frErr))
	}

	// 2b. Gouging-Hub Policy Cap Proof (mock hub returning 402 asking 50,000 sats!)
	var gougePaidCount atomic.Int64
	gougeMux := http.NewServeMux()
	gougeMux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"role":"spine","preset":"gouge"}`))
	})
	gougeMux.HandleFunc("/v1/terms", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"service":"ls_nektar","terms":1,"hubRole":"spine","fees":{"submit:want":50000,"submit:offer":50000},"egress":{"satsPer100Events":50000,"minTopupSats":50000}}`))
	})
	gougeMux.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(r.Header.Get("X-BSV-Payment")) != "" {
			gougePaidCount.Add(1)
		}
		w.Header().Set("X-BSV-Payment-Satoshis-Required", "50000")
		w.Header().Set("X-BSV-Payment-Derivation-Prefix", "gouge-prefix")
		w.Header().Set("X-BSV-Payment-Identity-Key", fmt.Sprint(wallets["spine"]["identityPub"]))
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":"gouging hub demands 50000 sats"}`))
	})
	gougeMux.HandleFunc("/sync", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-BSV-Payment-Satoshis-Required", "50000")
		w.WriteHeader(http.StatusPaymentRequired)
	})
	gougeMux.HandleFunc("/v1/credits/topup", func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(r.Header.Get("X-BSV-Payment")) != "" {
			gougePaidCount.Add(1)
		}
		w.Header().Set("X-BSV-Payment-Satoshis-Required", "50000")
		w.WriteHeader(http.StatusPaymentRequired)
	})
	gougeSrv := &http.Server{Addr: "127.0.0.1:18819", Handler: gougeMux}
	go func() { _ = gougeSrv.ListenAndServe() }()
	time.Sleep(150 * time.Millisecond)
	_, _ = httpJSON(http.MethodPost, specs["epsilon"].URL+"/v1/hosts", map[string]any{"url": "http://127.0.0.1:18819"})
	time.Sleep(500 * time.Millisecond)
	_, _ = httpJSON(http.MethodDelete, specs["epsilon"].URL+"/v1/hosts?url=http://127.0.0.1:18819", nil)
	_ = gougeSrv.Shutdown(context.Background())
	if gougePaidCount.Load() == 0 {
		sc.GougingHubBlocked = true
		fmt.Printf("[POLICY ] Gouging Hub (50,000 sat 402) refused fail-closed (paymentsSent=0) PASS\n")
	} else {
		sc.Failures = append(sc.Failures, fmt.Sprintf("gouging hub received %d payment attempt(s)", gougePaidCount.Load()))
	}

	// 2c. 24 Parallel Cross-Hub Paid Trades (4 concurrent waves of 6 buyers!)
	type tradeSpec struct {
		Buyer    string
		Domain   string
		Question string
		Route    string
		MaxSats  uint64
	}
	routes := []tradeSpec{
		{Buyer: "epsilon", Domain: "agents", Question: "Cursor MCP L0 transparent proxy Borg encryption", Route: "Epsilon(LeafA)->Gamma(LeafB) [cross-hub HTTP]", MaxSats: 120},
		{Buyer: "alpha", Domain: "nat", Question: "NAT-only A2A agents bbox store-and-forward relay", Route: "Alpha(LeafA)->DeltaNAT(LeafB) [cross-hub /bbox]", MaxSats: 120},
		{Buyer: "gamma", Domain: "bsv", Question: "BSV Broadcaster EF transactions 100 sat", Route: "Gamma(LeafB)->Beta(LeafA) [cross-hub HTTP]", MaxSats: 120},
		{Buyer: "zeta-nat", Domain: "ops", Question: "etcd snapshots rotate every 15m zstd compression", Route: "ZetaNAT(Spine)->Beta(LeafA) [spine->leaf]", MaxSats: 120},
		{Buyer: "beta", Domain: "nat", Question: "NAT-only A2A agents bbox relay zero open ports", Route: "Beta(LeafA)->DeltaNAT(LeafB) [cross-hub /bbox]", MaxSats: 120},
		{Buyer: "delta-nat", Domain: "ops", Question: "etcd snapshots rotate Kubernetes ops runbook", Route: "DeltaNAT(LeafB)->Beta(LeafA) [NAT-buyer->leaf]", MaxSats: 120},
	}

	runTrade := func(waveLabel string, ts tradeSpec, idx int) tradeResult {
		t0 := time.Now()
		res, err := httpJSON(http.MethodPost, specs[ts.Buyer].URL+"/v1/tools/want", map[string]any{
			"domains":        []string{ts.Domain},
			"question":       ts.Question,
			"maxSats":        ts.MaxSats,
			"targetShard":    fmt.Sprintf("bought/%s-%d", ts.Domain, idx),
			"collectTimeout": int64(60 * time.Second),
		})
		ms := time.Since(t0).Milliseconds()
		tr := tradeResult{
			Wave:      waveLabel,
			Buyer:     ts.Buyer,
			Domain:    ts.Domain,
			Route:     ts.Route,
			LatencyMs: ms,
		}
		if err != nil {
			tr.Status = "error"
			tr.Error = err.Error()
			return tr
		}
		tr.Status = fmt.Sprint(res["status"])
		tr.PriceSats = asUint64(res["priceSats"])
		if tx, ok := res["paymentTxid"].(string); ok {
			tr.TxID = tx
		}
		if eStr, ok := res["error"].(string); ok && eStr != "" {
			tr.Error = eStr
		}
		return tr
	}

	var tradeLat []int64
	var tradeMu sync.Mutex
	waves := (*tradesN + len(routes) - 1) / len(routes)
	dispatched := 0
	for wIdx := 0; wIdx < waves; wIdx++ {
		var waveWG sync.WaitGroup
		for rIdx, rt := range routes {
			if dispatched >= *tradesN {
				break
			}
			tradeNum := dispatched
			dispatched++
			waveWG.Add(1)
			go func(spec tradeSpec, n int) {
				defer waveWG.Done()
				tr := runTrade(fmt.Sprintf("wave2-%d", wIdx+1), spec, n)
				tradeMu.Lock()
				sc.Trades = append(sc.Trades, tr)
				if tr.Status == "settled" {
					sc.TradesSettled++
					tradeLat = append(tradeLat, tr.LatencyMs)
					fmt.Printf("  [TRADE #%02d OK  ] %-46s %3d sats  %5dms  tx=%s\n", n+1, tr.Route, tr.PriceSats, tr.LatencyMs, shortTx(tr.TxID))
				} else {
					sc.Failures = append(sc.Failures, fmt.Sprintf("trade#%d %s: status=%s err=%s", n+1, tr.Route, tr.Status, tr.Error))
					fmt.Printf("  [TRADE #%02d FAIL] %-46s status=%s (%dms) err=%s\n", n+1, tr.Route, tr.Status, tr.LatencyMs, tr.Error)
				}
				tradeMu.Unlock()
			}(rt, tradeNum)
			_ = rIdx
			if *tradeStaggerMs > 0 {
				time.Sleep(time.Duration(*tradeStaggerMs) * time.Millisecond)
			}
		}
		waveWG.Wait()
	}
	sc.TradeP50Ms = percentile(tradeLat, 50)
	sc.TradeP95Ms = percentile(tradeLat, 95)
	sc.TradeP99Ms = percentile(tradeLat, 99)
	fmt.Printf("[WAVE 2 ] Cross-Hub Trades: %d/%d settled | p50=%dms p95=%dms p99=%dms\n",
		sc.TradesSettled, sc.TradesTotal, sc.TradeP50Ms, sc.TradeP95Ms, sc.TradeP99Ms)

	// =========================================================================
	// WAVE 3: Mid-Flight SIGKILL Chaos & Crash Recovery (Leaf Hub A + Spine Hub S)
	// =========================================================================
	fmt.Printf("\n=== Wave 3: Mid-Flight SIGKILL Chaos & Crash Recovery ===\n")

	// 3a. Hard SIGKILL Leaf Hub A, restart on same SQLite DB, verify cross-hub trade recovers!
	fmt.Printf("  [CHAOS 1] Hard SIGKILL Leaf Hub A (:18811)...\n")
	procMu.Lock()
	stopProc(procs["leaf-a"], true)
	delete(procs, "leaf-a")
	procMu.Unlock()
	time.Sleep(1500 * time.Millisecond)

	// Launch a trade from Gamma (Leaf B) -> Beta (Leaf A) right as Leaf A restarts so Spine buffers & Leaf A catches up!
	fmt.Printf("  [CHAOS 1] Restarting Leaf Hub A (:18811) on persisted SQLite...\n")
	startNode("leaf-a")
	time.Sleep(4 * time.Second) // let Leaf A resume persisted federation cursor & local agents reconnect SSE!

	trLeaf := runTrade("chaos-leaf-a", routes[0], 101) // Epsilon(LeafA) -> Gamma(LeafB)
	sc.Trades = append(sc.Trades, trLeaf)
	if trLeaf.Status == "settled" {
		sc.ChaosLeafRecovery = true
		sc.PostChaosTradesOK++
		fmt.Printf("  [CHAOS 1 OK  ] Post-SIGKILL Leaf A recovery trade settled in %dms tx=%s\n", trLeaf.LatencyMs, shortTx(trLeaf.TxID))
	} else {
		sc.Failures = append(sc.Failures, fmt.Sprintf("chaos leaf-a trade failed: status=%s err=%s", trLeaf.Status, trLeaf.Error))
		fmt.Printf("  [CHAOS 1 FAIL] Post-SIGKILL Leaf A trade status=%s err=%s\n", trLeaf.Status, trLeaf.Error)
	}

	// 3b. Hard SIGKILL Spine Hub S, restart on same SQLite DB, verify fee claims & credits survive + trade settles!
	fmt.Printf("  [CHAOS 2] Hard SIGKILL Spine Hub S (:18810)...\n")
	procMu.Lock()
	stopProc(procs["spine"], true)
	delete(procs, "spine")
	procMu.Unlock()
	time.Sleep(1500 * time.Millisecond)

	fmt.Printf("  [CHAOS 2] Restarting Spine Hub S (:18810) on persisted SQLite...\n")
	startNode("spine")
	time.Sleep(5 * time.Second) // let Leaf A & Leaf B reconnect to Spine S with persisted cursors & credits!

	trSpine := runTrade("chaos-spine", routes[1], 102) // Alpha(LeafA) -> DeltaNAT(LeafB /bbox)
	sc.Trades = append(sc.Trades, trSpine)
	if trSpine.Status == "settled" {
		sc.ChaosSpineRecovery = true
		sc.PostChaosTradesOK++
		fmt.Printf("  [CHAOS 2 OK  ] Post-SIGKILL Spine S recovery NAT-/bbox trade settled in %dms tx=%s\n", trSpine.LatencyMs, shortTx(trSpine.TxID))
	} else {
		sc.Failures = append(sc.Failures, fmt.Sprintf("chaos spine trade failed: status=%s err=%s", trSpine.Status, trSpine.Error))
		fmt.Printf("  [CHAOS 2 FAIL] Post-SIGKILL Spine S trade status=%s err=%s\n", trSpine.Status, trSpine.Error)
	}

	// =========================================================================
	// STAGE 4: Hub Fee Earnings, RAM Telemetry & On-Chain WoC Spot-Check
	// =========================================================================
	fmt.Printf("\n=== Stage 4: Hub Fee Ledgers, Memory RSS & On-Chain Verification ===\n")
	for _, hKey := range []string{"spine", "leaf-a", "leaf-b"} {
		if earn, err := httpJSON(http.MethodGet, specs[hKey].URL+"/hub/v1/hub/earnings", nil); err == nil {
			sc.HubEarnings[hKey] = earn
			b, _ := json.Marshal(earn)
			fmt.Printf("  [FEES] %-8s %s\n", hKey, string(b))
		}
	}

	if runtime.GOOS == "linux" {
		procMu.Lock()
		for k, mp := range procs {
			if mp.cmd != nil && mp.cmd.Process != nil {
				if mb := readLinuxRSSMB(mp.cmd.Process.Pid); mb > 0 {
					sc.ProcessRSSMB[k] = mb
				}
			}
		}
		procMu.Unlock()
	}

	// Spot-check up to 4 real live txids on WhatsOnChain (paced 500ms apart to avoid 429).
	if isLive {
		wocSeg := "test"
		wocExplorer := "https://test.whatsonchain.com/tx/"
		if isMain {
			wocSeg = "main"
			wocExplorer = "https://whatsonchain.com/tx/"
		}
		var sampleTxs []string
		if len(sc.AnchorTxIDs) > 0 {
			sampleTxs = append(sampleTxs, sc.AnchorTxIDs[0])
		}
		if len(sc.AnchorTxIDs) > 1 {
			sampleTxs = append(sampleTxs, sc.AnchorTxIDs[len(sc.AnchorTxIDs)-1])
		}
		for _, tr := range sc.Trades {
			if tr.TxID != "" && len(sampleTxs) < 4 {
				sampleTxs = append(sampleTxs, tr.TxID)
			}
		}
		for _, txid := range sampleTxs {
			time.Sleep(600 * time.Millisecond)
			resp, err := httpCli.Get("https://api.whatsonchain.com/v1/bsv/" + wocSeg + "/tx/hash/" + txid)
			if err == nil {
				raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				_ = resp.Body.Close()
				if resp.StatusCode == 200 {
					sc.SampleVerifiedTxs[txid] = "woc_confirmed_seen_200"
					fmt.Printf("  [WoC ] VERIFIED ON-CHAIN %s: %s%s\n", strings.ToUpper(*network), wocExplorer, txid)
				} else {
					sc.SampleVerifiedTxs[txid] = fmt.Sprintf("woc_http_%d:%s", resp.StatusCode, strings.TrimSpace(string(raw)))
					fmt.Printf("  [WoC ] tx=%s -> HTTP %d (BSV Broadcaster mempool propagation)\n", txid, resp.StatusCode)
				}
			}
		}
	}

	if *sweepBack && isLive {
		spineAddr := fmt.Sprint(getWallet("spine")["fundAddress"])
		fmt.Printf("\n=== Optional Sweep-Back to Spine (%s) ===\n", spineAddr)
		for _, k := range allKeys[1:] {
			swRes, swErr := httpJSON(http.MethodPost, specs[k].URL+"/v1/wallet/sweep", map[string]any{
				"to":      spineAddr,
				"confirm": "I_UNDERSTAND",
			})
			if swErr != nil {
				fmt.Printf("  [SWEEP] %-10s skipped/warn: %v\n", k, swErr)
			} else {
				fmt.Printf("  [SWEEP] %-10s -> spine sats=%v tx=%v\n", k, swRes["sats"], shortTx(fmt.Sprint(swRes["txid"])))
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

	sc.DurationSec = time.Since(startAll).Seconds()
	outJSON, _ := json.MarshalIndent(sc, "", "  ")
	if *reportPath != "" {
		_ = os.WriteFile(*reportPath, outJSON, 0o644)
	}
	fmt.Printf("\n===========================================================================\n")
	fmt.Printf(" SCORECARD SUMMARY (%.1fs)\n", sc.DurationSec)
	fmt.Printf("   Remembers  : %d/%d OK (p50=%dms p95=%dms)\n", sc.RemembersOK, sc.RemembersTotal, sc.RememberP50Ms, sc.RememberP95Ms)
	fmt.Printf("   Anchors    : %d/%d OK (p50=%dms p95=%dms)\n", sc.AnchorsOK, sc.AnchorsTotal, sc.AnchorP50Ms, sc.AnchorP95Ms)
	fmt.Printf("   SPV Verify : %d/%d OK (%.1f/sec, p95=%dms)\n", sc.SPVVerifiesOK, sc.SPVVerifiesTotal, sc.SPVThroughputPerS, sc.SPVP95Ms)
	fmt.Printf("   Free-Rider : blocked402=%v | GougingHub blocked=%v\n", sc.FreeRiderBlocked, sc.GougingHubBlocked)
	fmt.Printf("   Trades     : %d/%d settled (p50=%dms p95=%dms p99=%dms)\n", sc.TradesSettled, sc.TradesTotal, sc.TradeP50Ms, sc.TradeP95Ms, sc.TradeP99Ms)
	fmt.Printf("   Chaos      : Leaf-A SIGKILL=%v | Spine-S SIGKILL=%v | Post-Chaos OK=%d/2\n", sc.ChaosLeafRecovery, sc.ChaosSpineRecovery, sc.PostChaosTradesOK)
	fmt.Printf("   Failures   : %d\n", len(sc.Failures))
	fmt.Printf("===========================================================================\n")

	if len(sc.Failures) > 0 {
		for i, f := range sc.Failures {
			if i >= 15 {
				fmt.Printf("   ... +%d more\n", len(sc.Failures)-15)
				break
			}
			fmt.Printf("   FAIL: %s\n", f)
		}
		os.Exit(1)
	}
}

func stopProc(mp *managedProc, hardKill bool) {
	if mp == nil || mp.cmd == nil || mp.cmd.Process == nil {
		return
	}
	if hardKill || runtime.GOOS == "windows" {
		_ = mp.cmd.Process.Kill()
	} else {
		_ = mp.cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() {
			_ = mp.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			_ = mp.cmd.Process.Kill()
			<-done
		}
		if mp.logF != nil {
			_ = mp.logF.Close()
		}
		return
	}
	_ = mp.cmd.Wait()
	if mp.logF != nil {
		_ = mp.logF.Close()
	}
}

func httpJSON(method, url string, body any) (map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpCli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d %s %s: %s", resp.StatusCode, method, url, strings.TrimSpace(string(raw)))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode JSON from %s: %w (body=%s)", url, err, strings.TrimSpace(string(raw)))
	}
	return out, nil
}

func percentile(vals []int64, p int) int64 {
	if len(vals) == 0 {
		return 0
	}
	cp := append([]int64(nil), vals...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := (len(cp)*p + 99) / 100 - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func asUint64(v any) uint64 {
	switch n := v.(type) {
	case float64:
		if n < 0 {
			return 0
		}
		return uint64(n)
	case int64:
		if n < 0 {
			return 0
		}
		return uint64(n)
	case string:
		u, _ := strconv.ParseUint(strings.TrimSpace(n), 10, 64)
		return u
	default:
		return 0
	}
}

func shortTx(tx string) string {
	if len(tx) <= 16 {
		return tx
	}
	return tx[:16] + "..."
}

func readLinuxRSSMB(pid int) float64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseFloat(fields[1], 64)
				return kb / 1024.0
			}
		}
	}
	return 0
}

func tailFile(path string, lines int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	parts := strings.Split(string(b), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FATAL: "+format+"\n", args...)
	os.Exit(1)
}

// startWoCReadThroughHeaderProxy serves real BSV headers fetched live from
// api.whatsonchain.com/v1/bsv/<wocNet> over a local Chaintracks v2 API with global 600ms pacing,
// per-height request coalescing, and on-disk caching of immutable mined block headers so
// 9 co-located processes never trip WoC's per-IP 3 req/s limit.
func startWoCReadThroughHeaderProxy(listenAddr, workDir, wocNet string) func() {
	var paceMu sync.Mutex
	var nextSlot time.Time
	wocClient := &http.Client{Timeout: 20 * time.Second}

	wocGet := func(ctx context.Context, path string) ([]byte, int, error) {
		for attempt := 0; attempt < 6; attempt++ {
			paceMu.Lock()
			now := time.Now()
			wait := time.Duration(0)
			if now.Before(nextSlot) {
				wait = nextSlot.Sub(now)
				nextSlot = nextSlot.Add(600 * time.Millisecond)
			} else {
				nextSlot = now.Add(600 * time.Millisecond)
			}
			paceMu.Unlock()
			if wait > 0 {
				t := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					t.Stop()
					return nil, 0, ctx.Err()
				case <-t.C:
				}
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.whatsonchain.com/v1/bsv/"+wocNet+path, nil)
			if err != nil {
				return nil, 0, err
			}
			resp, err := wocClient.Do(req)
			if err != nil {
				if attempt < 5 {
					time.Sleep(time.Duration(attempt+1) * time.Second)
					continue
				}
				return nil, 0, err
			}
			raw, rErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if rErr != nil {
				return nil, resp.StatusCode, rErr
			}
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
				paceMu.Lock()
				nextSlot = time.Now().Add(time.Duration(attempt+1) * 1500 * time.Millisecond)
				paceMu.Unlock()
				continue
			}
			return raw, resp.StatusCode, nil
		}
		return nil, http.StatusTooManyRequests, fmt.Errorf("WoC 429 persisted for %s", path)
	}

	var cacheMu sync.RWMutex
	hdrCache := make(map[string]json.RawMessage)
	cacheFile := filepath.Join(workDir, fmt.Sprintf("woc-%s-headers-cache.json", wocNet))
	if b, err := os.ReadFile(cacheFile); err == nil {
		_ = json.Unmarshal(b, &hdrCache)
	}
	saveCache := func() {
		cacheMu.RLock()
		cp := make(map[string]json.RawMessage, len(hdrCache))
		for k, v := range hdrCache {
			cp[k] = v
		}
		cacheMu.RUnlock()
		if b, err := json.Marshal(cp); err == nil {
			_ = os.WriteFile(cacheFile, b, 0o640)
		}
	}

	// Per-height singleflight coalescing so concurrent nodes requesting the same height share 1 WoC fetch.
	type flight struct {
		done chan struct{}
		raw  []byte
		code int
		err  error
	}
	var flightMu sync.Mutex
	inFlight := make(map[string]*flight)

	var tipHeight uint32
	var tipAt time.Time

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/height", func(w http.ResponseWriter, r *http.Request) {
		cacheMu.RLock()
		h, at := tipHeight, tipAt
		cacheMu.RUnlock()
		if h == 0 || time.Since(at) > 30*time.Second {
			raw, code, err := wocGet(r.Context(), "/chain/info")
			if err != nil || code != 200 {
				if code == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "2")
					http.Error(w, "upstream rate limited", http.StatusTooManyRequests)
					return
				}
				http.Error(w, "upstream tip error", http.StatusBadGateway)
				return
			}
			var info struct {
				Blocks uint32 `json:"blocks"`
			}
			if json.Unmarshal(raw, &info) != nil || info.Blocks == 0 {
				http.Error(w, "invalid tip json", http.StatusBadGateway)
				return
			}
			cacheMu.Lock()
			tipHeight = info.Blocks
			tipAt = time.Now()
			h = info.Blocks
			cacheMu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"success","value":{"height":%d}}`, h)
	})
	mux.HandleFunc("GET /v2/header/height/{h}", func(w http.ResponseWriter, r *http.Request) {
		hs := strings.TrimSpace(r.PathValue("h"))
		cacheMu.RLock()
		cached, ok := hdrCache[hs]
		cacheMu.RUnlock()
		if !ok {
			flightMu.Lock()
			fl, exists := inFlight[hs]
			if !exists {
				fl = &flight{done: make(chan struct{})}
				inFlight[hs] = fl
				flightMu.Unlock()
				// Use detached timeout so one cancelled client doesn't abort other waiters.
				fctx, fcancel := context.WithTimeout(context.Background(), 25*time.Second)
				fl.raw, fl.code, fl.err = wocGet(fctx, "/block/"+hs+"/header")
				fcancel()
				if fl.err == nil && fl.code == http.StatusOK && len(fl.raw) > 0 {
					cacheMu.Lock()
					hdrCache[hs] = append(json.RawMessage(nil), fl.raw...)
					cacheMu.Unlock()
					go saveCache()
				}
				close(fl.done)
				flightMu.Lock()
				delete(inFlight, hs)
				flightMu.Unlock()
			} else {
				flightMu.Unlock()
			}
			select {
			case <-fl.done:
			case <-r.Context().Done():
				return
			}
			if fl.code == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "2")
				http.Error(w, "upstream rate limited", http.StatusTooManyRequests)
				return
			}
			if fl.err != nil {
				http.Error(w, fl.err.Error(), http.StatusBadGateway)
				return
			}
			if fl.code == http.StatusNotFound {
				http.NotFound(w, r)
				return
			}
			if fl.code != http.StatusOK {
				http.Error(w, string(fl.raw), http.StatusBadGateway)
				return
			}
			cached = fl.raw
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","value":`))
		_, _ = w.Write(cached)
		_, _ = w.Write([]byte(`}`))
	})

	srv := &http.Server{Addr: listenAddr, Handler: mux}
	go func() { _ = srv.ListenAndServe() }()
	time.Sleep(100 * time.Millisecond)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}
