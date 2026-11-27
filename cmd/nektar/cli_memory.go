package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/server"
)

func cmdRemember(args []string) error {
	fs := flag.NewFlagSet("remember", flag.ExitOnError)
	shard := fs.String("shard", "default", "Target shard name")
	text := fs.String("text", "", "Memory fragment text")
	group := fs.String("group", "", "Optional Borg group ID for BRC-369 encryption")
	publicFlag := fs.Bool("public", false, "Opt out of private-by-default CLKA encryption")
	ontology := fs.String("ontology", "", "Optional committed domain ontology repo (ont-<domain>) or 64-hex OntologyID for JevGraph extraction")
	ontologyStrict := fs.Bool("ontology-strict", false, "Fail closed on -ontology writes if Jev System One evaluator is offline")
	freshness := fs.String("freshness", "", "Optional NMU-1 freshness class: ephemeral | versioned | evergreen")
	validUntil := fs.Int64("valid-until", 0, "Optional Unix timestamp expiry for ephemeral/versioned facts")
	subjectVersion := fs.String("subject-version", "", "Optional subject version tag (e.g. go-1.27, sdk-v1.7.8)")
	rights := fs.String("rights", "", "Optional NMU-1 usage rights: inference_only | distill_resellable")
	anchor := fs.Bool("anchor", false, "Anchor immediately on BSV")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	if info, ok := detectDaemon(*dataDir, ""); ok {
		out, err := callLiveTool(info, "remember", map[string]any{
			"shard":          *shard,
			"text":           *text,
			"borgGroup":      *group,
			"public":         *publicFlag,
			"ontology":       *ontology,
			"ontologyStrict": *ontologyStrict,
			"freshnessClass": strings.TrimSpace(*freshness),
			"validUntilUnix": *validUntil,
			"subjectVersion": strings.TrimSpace(*subjectVersion),
			"usageRights":    strings.TrimSpace(*rights),
			"autoAnchor":     *anchor,
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
		Shard:          *shard,
		Text:           *text,
		BorgGroup:      *group,
		Public:         *publicFlag,
		Ontology:       *ontology,
		OntologyStrict: *ontologyStrict,
		FreshnessClass: strings.TrimSpace(*freshness),
		ValidUntilUnix: *validUntil,
		SubjectVersion: strings.TrimSpace(*subjectVersion),
		UsageRights:    strings.TrimSpace(*rights),
		AutoAnchor:     *anchor,
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

	if info, ok := detectDaemon(*dataDir, ""); ok {
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

	_, _ = rt.mem.EnsureWelcome(ctx)
	shards, items, err := rt.mem.RecallAllShards(ctx, memory.RecallInput{Shard: *shard, Query: *query, VerifySPV: *spv})
	if err != nil {
		return err
	}
	if shards == nil {
		shards = []string{}
	}
	if items == nil {
		items = []memory.RecalledFragment{}
	}
	return printJSON(map[string]any{"shards": shards, "items": items, "count": len(items)})
}

func cmdAnchor(args []string) error {
	if len(args) > 0 && args[0] == "rotate" {
		fs := flag.NewFlagSet("anchor rotate", flag.ExitOnError)
		shard := fs.String("shard", "default", "Target shard name")
		newOwner := fs.String("new-owner", "", "New owner compressed 33-byte pubkey hex or 20-byte PKH hex")
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(args[1:])
		if strings.TrimSpace(*newOwner) == "" {
			return errors.New("anchor rotate: -new-owner <pubHex|pkhHex> is required")
		}
		ctx := context.Background()
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		res, err := rt.mem.RotateShardOwner(ctx, *shard, strings.TrimSpace(*newOwner))
		if err != nil {
			return err
		}
		return printJSON(res)
	}

	fs := flag.NewFlagSet("anchor", flag.ExitOnError)
	shard := fs.String("shard", "default", "Target shard name")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	if info, ok := detectDaemon(*dataDir, ""); ok {
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
	token := fs.String("token", "", "Receipt token (nektarr1.... or nektardr1....) or 64-hex hash")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	if strings.HasPrefix(strings.TrimSpace(*token), market.DealReceiptTokenPrefix) {
		rcp, err := market.DecodeDealReceiptToken(*token)
		if err != nil {
			return err
		}
		vr := market.VerifyDealReceipt(*rcp, false)
		return printJSON(map[string]any{"verification": vr, "receipt": rcp})
	}

	if info, ok := detectDaemon(*dataDir, ""); ok {
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
	netName := fs.String("network", "", "Network override (testnet|main|memory)")
	asJSON := fs.Bool("json", false, "Machine-readable JSON output")
	_ = fs.Parse(args)

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		if out, err := callLiveTool(info, "status", map[string]any{}); err == nil {
			if *asJSON {
				return printJSON(out)
			}
			printStatusHuman(info, out)
			return nil
		}
	}
	effectiveNet := strings.TrimSpace(*netName)
	if effectiveNet == "" {
		effectiveNet = envOr("NEKTAR_NETWORK", "testnet")
	}
	dir := strings.TrimSpace(*dataDir)
	if dir == "" {
		dir = defaultDataDirForNetwork(effectiveNet)
	}
	if effectiveNet != "memory" {
		if _, err := os.Stat(dir + string(os.PathSeparator) + "nektar.sqlite"); os.IsNotExist(err) {
			stopped := map[string]any{
				"running": false,
				"network": effectiveNet,
				"dataDir": dir,
				"next":    "Start your node with `nektar up` (or `nektar up --network main --confirm I_UNDERSTAND`)",
			}
			if *asJSON {
				return printJSON(stopped)
			}
			fmt.Printf("nektar status: STOPPED (uninitialized · %s · %s)\n  Next: run `nektar up`\n", effectiveNet, dir)
			return nil
		}
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	st := server.BuildStatus(ctx, rt.mem, rt.mkt)
	if *asJSON {
		return printJSON(st)
	}
	printStatusHuman(nil, st)
	return nil
}

func printStatusHuman(daemon *server.DaemonInfo, raw any) {
	m, ok := raw.(map[string]any)
	if !ok {
		_ = printJSON(raw)
		return
	}
	var b strings.Builder
	if daemon != nil {
		fmt.Fprintf(&b, "nektar status: UP at %s (%s · pid %d)\n", daemon.URL, m["network"], daemon.PID)
	} else {
		fmt.Fprintf(&b, "nektar status: STOPPED (offline snapshot · %s)\n", m["network"])
	}
	fmt.Fprintf(&b, "  identity : %v\n", m["identityPub"])
	fmt.Fprintf(&b, "  wallet   : %v (%v sats · %v UTXO(s))\n", m["fundAddress"], m["balanceSats"], m["utxos"])
	fmt.Fprintf(&b, "  shards   : %v\n", m["shards"])
	fmt.Fprintf(&b, "  policy   : %v\n", m["policyHash"])
	if sg, ok := m["spendGuard"].(map[string]any); ok {
		fmt.Fprintf(&b, "  guard    : %v/%v sats today (maxFee=%v, txCount=%v)\n",
			sg["spentToday"], sg["maxPerDay"], sg["maxFee"], sg["txCount"])
	}
	fmt.Print(formatConsoleText(b.String()))
}

func cmdCheckMainnet() error {
	var missing []string
	if os.Getenv("NEKTAR_MAINNET_CONFIRM") != "I_UNDERSTAND" {
		missing = append(missing, "NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND")
	}
	headerSpec := strings.TrimSpace(os.Getenv("NEKTAR_HEADERS_SOURCE"))
	if headerSpec == "" {
		missing = append(missing, fmt.Sprintf("NEKTAR_HEADERS_SOURCE (>=2 distinct-origin PoW witnesses, e.g. %q)", chain.DefaultHeaderSources("main")))
	}
	settleSpec := strings.TrimSpace(os.Getenv("NEKTAR_SETTLE_SPEC"))
	if settleSpec == "" {
		missing = append(missing, fmt.Sprintf("NEKTAR_SETTLE_SPEC (e.g. %s)", chain.DefaultSettleSpec("main")))
	}
	if len(missing) > 0 {
		return fmt.Errorf("mainnet gate incomplete — set: %s", strings.Join(missing, ", "))
	}
	chainSpec := strings.TrimSpace(os.Getenv("NEKTAR_CHAIN_SPEC"))
	if chainSpec == "" {
		chainSpec = "woc:main"
	}
	if err := chain.ValidateHeaderIndependence("main", headerSpec, chainSpec, settleSpec); err != nil {
		return fmt.Errorf("mainnet header independence gate failed: %w", err)
	}
	fmt.Println("mainnet gates OK: confirmation, >=2 independent PoW header witnesses (non-circular), settlement spec, and SpendGuard configured")
	return nil
}
