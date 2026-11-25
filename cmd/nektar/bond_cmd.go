package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/kryptokrates/nektar/internal/market"
)

func cmdBond(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nektar bond <open|commit|status|seize|close> [flags]")
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	rest := args[1:]
	ctx := context.Background()

	switch sub {
	case "open":
		fs := flag.NewFlagSet("bond open", flag.ExitOnError)
		deposit := fs.Uint64("deposit", 10000, "Slashable SLA bond deposit in satoshis (>= 1000)")
		epochLen := fs.Uint64("epoch-len", 144, "Blocks per checkpoint SLA epoch (144 ~= 24h)")
		releaseBlocks := fs.Uint64("release-blocks", 432, "Blocks until operator may Close() bond")
		maxFee := fs.Uint64("max-fee", 500, "Max miner fee per bond transition")
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest)
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		rec, err := market.OpenCheckpointBond(ctx, rt.mem.DB(), rt.mem.Chain(), *deposit, *epochLen, *releaseBlocks, *maxFee)
		if err != nil {
			return err
		}
		return printJSON(rec)

	case "commit":
		fs := flag.NewFlagSet("bond commit", flag.ExitOnError)
		rootHex := fs.String("root-hex", "", "Optional 32-byte hex root to commit (defaults to deterministic tip checkpoint)")
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest)
		var root []byte
		if strings.TrimSpace(*rootHex) != "" {
			var err error
			root, err = hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(*rootHex), "0x"))
			if err != nil || len(root) == 0 || len(root) > 75 {
				return errors.New("bond commit: -root-hex must be 1..75 bytes hex")
			}
		}
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		rec, err := market.CommitActiveBond(ctx, rt.mem.DB(), rt.mem.Chain(), root)
		if err != nil {
			return err
		}
		return printJSON(rec)

	case "status":
		fs := flag.NewFlagSet("bond status", flag.ExitOnError)
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest)
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		rec, _, err := market.LoadActiveBond(rt.mem.DB())
		if err != nil {
			return err
		}
		if rec == nil {
			return printJSON(map[string]any{"active": false})
		}
		return printJSON(map[string]any{
			"active":            true,
			"outpoint":          rec.Outpoint(),
			"satoshis":          rec.Satoshis,
			"operatorPkh":       hex.EncodeToString(rec.State.OperatorPKH),
			"n":                 rec.State.N,
			"lastRootHex":       hex.EncodeToString(rec.State.LastRoot),
			"deadline":          rec.State.Deadline,
			"bondReleaseHeight": rec.State.BondReleaseHeight,
			"epochLen":          rec.State.EpochLen,
			"maxFee":            rec.State.MaxFee,
			"seized":            rec.State.Seized,
			"updatedAt":         rec.UpdatedAt,
		})

	case "seize":
		fs := flag.NewFlagSet("bond seize", flag.ExitOnError)
		beefHex := fs.String("beef-hex", "", "Target delinquent peer/hub bond AtomicBEEF hex")
		vout := fs.Uint("vout", 0, "Output index of NektarCheckpointBond UTXO")
		to := fs.String("to", "", "Optional destination BSV P2PKH address or 20-byte PKH hex (defaults to local FundAddress)")
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest)
		if strings.TrimSpace(*beefHex) == "" {
			return errors.New("bond seize: -beef-hex is required")
		}
		beef, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(*beefHex), "0x"))
		if err != nil || len(beef) == 0 {
			return errors.New("bond seize: invalid -beef-hex")
		}
		var destPKH []byte
		if strings.TrimSpace(*to) != "" {
			destPKH, err = parseDestAddressOrPKH(*to)
			if err != nil {
				return err
			}
		}
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		txid, payoutSats, err := market.SeizePeerBond(ctx, rt.mem.Chain(), beef, uint32(*vout), destPKH)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"txid": txid, "slashedSats": payoutSats})

	case "close":
		fs := flag.NewFlagSet("bond close", flag.ExitOnError)
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest)
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		txid, payoutSats, err := market.CloseActiveBond(ctx, rt.mem.DB(), rt.mem.Chain())
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"txid": txid, "reclaimedSats": payoutSats})

	default:
		return fmt.Errorf("unknown bond subcommand %q (use open|commit|status|seize|close)", sub)
	}
}
