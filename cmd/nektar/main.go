package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/jev"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/reasoning"
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
	case "vault":
		err = cmdVault(args)
	case "bond":
		err = cmdBond(args)
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
  nektar anchor         Anchor unanchored fragments in a shard (-shard) or rotate ownership (anchor rotate -shard -new-owner)
  nektar vault          Manage two-key on-chain NektarAgentVault (deploy | status | pause | resume | rotate-agent | sweep)
  nektar bond           Manage slashable NektarCheckpointBond SLA bond (open | commit | status | seize | close)
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

func buildJevMatcherFromEnv(network string) market.Matcher {
	mode := strings.ToLower(strings.TrimSpace(envOr("NEKTAR_JEV_MODE", market.AIModeGated)))
	if mode == market.AIModeOff {
		return nil
	}
	timeoutMs := envUint64("NEKTAR_JEV_TIMEOUT_MS", 2500)
	cl, err := jev.NewClient(jev.Config{
		BaseURL:      os.Getenv("NEKTAR_JEV_BASE_URL"),
		Model:        os.Getenv("NEKTAR_JEV_MODEL"),
		Network:      network,
		AllowedHosts: parseCSVEnv("NEKTAR_JEV_ALLOWED_HOSTS"),
		Timeout:      time.Duration(timeoutMs) * time.Millisecond,
	})
	if err != nil {
		log.Printf("openweight matcher disabled: %v", err)
		return nil
	}
	privacy := envOr("NEKTAR_JEV_PRIVACY", market.PrivacyDisclosablePreview)
	log.Printf("openweight matcher armed: endpoint=%s model=%s mode=%s privacy=%s (local keyless; falls back cleanly if offline)",
		cl.Health().Endpoint, cl.Model(), mode, privacy)
	return market.NewJevMatcher(cl, market.JevMatcherOptions{
		DefaultMode:        mode,
		DefaultPrivacyMode: privacy,
	})
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
		SettleDepth:     int(envInt64("NEKTAR_SETTLE_DEPTH", 0)),
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
	if pinnedOp := strings.TrimSpace(os.Getenv("NEKTAR_OPERATOR_PUBKEY")); pinnedOp != "" {
		mkt.Policy().SetPinnedOperatorPub(pinnedOp)
	}
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

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
