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
	headerSpec := strings.TrimSpace(os.Getenv("NEKTAR_HEADERS_SOURCE"))
	if headerSpec == "" {
		missing = append(missing, fmt.Sprintf("NEKTAR_HEADERS_SOURCE (>=2 distinct-origin PoW witnesses, e.g. %q)", chain.DefaultHeaderSources("main")))
	}
	settleSpec := strings.TrimSpace(os.Getenv("NEKTAR_SETTLE_SPEC"))
	if settleSpec == "" {
		missing = append(missing, "NEKTAR_SETTLE_SPEC (e.g. arcade:https://arcade.gorillapool.io)")
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
