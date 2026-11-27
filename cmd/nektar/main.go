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
	"github.com/kryptokrates/nektar/internal/server"
	"github.com/kryptokrates/nektar/internal/store"
)

const (
	Version          = "0.1.0"
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
	case "up", "start":
		err = cmdUp(args)
	case "stop":
		err = cmdStop(args)
	case "serve":
		err = cmdServe(args)
	case "mcp":
		err = cmdMCP(args)
	case "hub":
		err = cmdHub(args)
	case "earn":
		err = cmdEarn(args)
	case "remember":
		err = cmdRemember(args)
	case "repo":
		err = cmdRepo(args)
	case "ontology":
		err = cmdOntology(args)
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
	fmt.Println(`nektar v0.1 — Verifiable, encrypted AI agent memory & autonomous knowledge market on BSV

Get Started (25 seconds, zero Docker):
  nektar up             Start background daemon + auto-dock Claude/Codex/Hermes/OpenClaw/Cursor + live receipt check
                        Mainnet 1-liner:     nektar up --network main --confirm I_UNDERSTAND
                        BSV Desktop custody: nektar up --wallet client  (zero spending keys in Nektar)
  nektar stop           Gracefully stop the background Nektar daemon
  nektar serve          Run foreground daemon + Dashboard + MCP + L0 Proxy + Market (default testnet :18420)
  nektar open           Open the Nektar Dashboard in your browser
  nektar dock           Auto-detect & wire AI apps (Claude, Cursor, Codex, VS Code, Hermes, OpenClaw…)
  nektar wallet         Manage BRC-100 wallet (status | address [--qr] | sync | fund | send | sweep | fuel)

Memory & Knowledge Market:
  nektar remember       Commit a fragment to a Memory Repo (-shard, -text, -ontology, -anchor, -group)
  nektar repo           Git-compatible BSV Memory Repos (list | log | tree | diff | commit | branch | merge | export | import | verify | checkout)
  nektar ontology       Manage domain TBox ontologies, Bridge Shards & JevGraph extraction (init | list | show | validate | extract | edges | review | align)
  nektar recall         Recall Cedar-authorized, Borg-decrypted fragments (-shard, -query, -spv)
  nektar anchor         Anchor unanchored fragments via NektarAnchor covenant (or anchor rotate)
  nektar verify         Verify a nektarr1.* receipt token offline (-token)
  nektar want           Buy verified knowledge live from peers or Hubs (-domains, -question, -max)
  nektar sell           Arm a local shard for A2A selling (-shard, -domains, -price) or sell to Hub (-bounty)
  nektar earn           Guided seller fast-path: inspect open Hub bounties & earn sats from anchored shards
  nektar policy         Manage operator trading policies (approve --preset buyer|seller|both | list | revoke)
  nektar peers          Discover verified peers, bfinger handles, CHIRP packs & Hub bounties

Hub & Operations:
  nektar hub            Deploy/manage a public Hub (up [--auto-ip|--domain|--tunnel|--native] | status | logs | down | run)
  nektar mcp            Stdio MCP bridge (auto-connects & auto-starts shared daemon; zero lock contention)
  nektar hook           Native zero-Node Claude Code / Codex hooks (session-start | prefetch | verify-receipt)
  nektar doctor         Run full node health diagnosis (wallet, Restate, peers, cohort density & next steps)
  nektar check-mainnet  Verify Mainnet confirmation, dual-witness PoW headers, broadcaster & SpendGuard
  nektar status         Print node identity, wallet balance, shards, policy hash & SpendGuard state
  nektar vault / bond   Manage on-chain NektarAgentVault & NektarCheckpointBond covenants
  nektar restate-up     Download SHA-256 verified restate-server v1.8.1 (macOS, Linux, or Windows WSL2)
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

func buildJevClientFromEnv(network string) (*jev.Client, string, string) {
	mode := strings.ToLower(strings.TrimSpace(envOr("NEKTAR_JEV_MODE", market.AIModeGated)))
	if mode == market.AIModeOff {
		return nil, mode, ""
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
		log.Printf("openweight jev client disabled: %v", err)
		return nil, mode, ""
	}
	privacy := envOr("NEKTAR_JEV_PRIVACY", market.PrivacyDisclosablePreview)
	return cl, mode, privacy
}

func buildJevMatcherFromEnv(network string) market.Matcher {
	cl, mode, privacy := buildJevClientFromEnv(network)
	if cl == nil {
		return nil
	}
	log.Printf("openweight matcher armed: endpoint=%s model=%s mode=%s privacy=%s (local keyless; falls back cleanly if offline)",
		cl.Health().Endpoint, cl.Model(), mode, privacy)
	return market.NewJevMatcher(cl, market.JevMatcherOptions{
		DefaultMode:        mode,
		DefaultPrivacyMode: privacy,
	})
}

func defaultDataDir() string {
	return defaultDataDirForNetwork(os.Getenv("NEKTAR_NETWORK"))
}

// defaultDataDirForNetwork isolates BSV Mainnet into ~/.nektar/main when no explicit
// NEKTAR_DATA_DIR is configured so testnet and mainnet wallets/SQLite databases never collide.
func defaultDataDirForNetwork(network string) string {
	if v := strings.TrimSpace(os.Getenv("NEKTAR_DATA_DIR")); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".nektar")
	if strings.EqualFold(strings.TrimSpace(network), "main") || strings.EqualFold(strings.TrimSpace(network), "mainnet") {
		return filepath.Join(base, "main")
	}
	return base
}

// detectDaemon resolves a live Nektar daemon across explicit overrides or any active network
// (~/.nektar/active-daemon.json, ~/.nektar/main, ~/.nektar) so Mainnet daemons are always found
// by zero-flag CLI commands, MCP stdio bridges, and hooks.
func detectDaemon(dataDirFlag, networkFlag string) (*server.DaemonInfo, bool) {
	if d := strings.TrimSpace(dataDirFlag); d != "" {
		return server.DetectLiveDaemon(d)
	}
	if n := strings.TrimSpace(networkFlag); n != "" {
		return server.DetectLiveDaemon(defaultDataDirForNetwork(n))
	}
	if d := strings.TrimSpace(os.Getenv("NEKTAR_DATA_DIR")); d != "" {
		return server.DetectLiveDaemon(d)
	}
	if n := strings.TrimSpace(os.Getenv("NEKTAR_NETWORK")); n != "" {
		return server.DetectLiveDaemon(defaultDataDirForNetwork(n))
	}
	return server.DetectAnyLiveDaemon("")
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
		dir = defaultDataDirForNetwork(net)
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
	hdrSrc := strings.TrimSpace(os.Getenv("NEKTAR_HEADERS_SOURCE"))
	port, err := chain.Open(ctx, chain.Config{
		Network:         net,
		DataDir:         dir,
		WalletMode:      envOr("NEKTAR_WALLET", "auto"),
		WalletWireURL:   envOr("NEKTAR_WALLET_WIRE_URL", "http://127.0.0.1:3321"),
		WalletProfile:   envOr("NEKTAR_WALLET_PROFILE", "standard"),
		FeeRateSatPerKB: envUint64("NEKTAR_FEE_RATE_SAT_KB", 0),
		HeaderSource:    hdrSrc,
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
	jevClient, _, _ := buildJevClientFromEnv(net)
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), objStore, memory.Config{
		VerifyBaseURL: verifyBase,
		ModelID: reasoning.ModelIdentity{
			Kind:     "api",
			Provider: envOr("NEKTAR_MODEL_PROVIDER", "nektar"),
			Release:  modelRelease,
		},
		JevEvaluator: jevClient,
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	if pinnedOp := strings.TrimSpace(os.Getenv("NEKTAR_OPERATOR_PUBKEY")); pinnedOp != "" {
		mkt.Policy().SetPinnedOperatorPub(pinnedOp)
	}
	if err := market.SyncPolicyFromDB(db, mkt.Policy()); err != nil {
		_ = port.Close()
		_ = db.Close()
		return nil, fmt.Errorf("sync operator policies from db: %w", err)
	}
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
		// Global emergency stop: if running isolated Mainnet under ~/.nektar/main, also honor parent ~/.nektar/KILL.
		clean := filepath.Clean(dataDir)
		parent := filepath.Dir(clean)
		if strings.EqualFold(filepath.Base(clean), "main") && strings.EqualFold(filepath.Base(parent), ".nektar") {
			if _, err := os.Stat(filepath.Join(parent, "KILL")); err == nil {
				return true
			}
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
