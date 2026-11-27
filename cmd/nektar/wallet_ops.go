package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func walletFund(args []string) error {
	fs := flag.NewFlagSet("wallet fund", flag.ExitOnError)
	fromDesktop := fs.Bool("from-desktop", false, "Request 1-click BRC-29 payment from local BSV Desktop")
	sats := fs.Uint64("sats", 25_000, "Amount in satoshis to request from BSV Desktop")
	wireURL := fs.String("wire-url", envOr("NEKTAR_WALLET_WIRE_URL", nwallet.DefaultDesktopWireURL), "Local BRC-100 wallet wire URL")
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	_ = fs.Parse(args)

	if !*fromDesktop {
		var addrArgs []string
		if *dataDir != "" {
			addrArgs = append(addrArgs, "-data-dir", *dataDir)
		}
		if *netName != "" {
			addrArgs = append(addrArgs, "-network", *netName)
		}
		return walletAddress(addrArgs)
	}
	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveWallet(info, http.MethodPost, "/v1/wallet/fund-desktop", map[string]any{
			"sats":    *sats,
			"wireUrl": *wireURL,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	res, err := rt.mem.Chain().FundFromDesktop(ctx, *wireURL, *sats)
	if err != nil {
		return err
	}
	return printJSON(res)
}

func walletImport(args []string) error {
	fs := flag.NewFlagSet("wallet import", flag.ExitOnError)
	beefFlag := fs.String("beef", "", "BEEF/AtomicBEEF hex string, or @path/to/file")
	txidFlag := fs.String("txid", "", "Mined BSV transaction ID to fetch and verify")
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	_ = fs.Parse(args)

	beefHex := strings.TrimSpace(*beefFlag)
	if strings.HasPrefix(beefHex, "@") {
		raw, err := os.ReadFile(strings.TrimPrefix(beefHex, "@"))
		if err != nil {
			return err
		}
		// Accept either raw binary BEEF file or hex text file.
		trimmed := strings.TrimSpace(string(raw))
		if _, hErr := hex.DecodeString(trimmed); hErr == nil && len(trimmed)%2 == 0 {
			beefHex = trimmed
		} else {
			beefHex = hex.EncodeToString(raw)
		}
	}
	txid := strings.TrimSpace(*txidFlag)
	if beefHex == "" && txid == "" {
		return errors.New("usage: nektar wallet import --beef <hex|@file> | --txid <txid>")
	}

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveWallet(info, http.MethodPost, "/v1/wallet/import", map[string]any{
			"beefHex": beefHex,
			"txid":    txid,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	if beefHex != "" {
		raw, err := hex.DecodeString(beefHex)
		if err != nil {
			return fmt.Errorf("invalid beef hex: %w", err)
		}
		res, err := rt.mem.Chain().ImportBEEF(ctx, raw)
		if err != nil {
			return err
		}
		return printJSON(res)
	}
	res, err := rt.mem.Chain().ImportTxID(ctx, txid)
	if err != nil {
		return err
	}
	return printJSON(res)
}

func walletSend(args []string) error {
	fs := flag.NewFlagSet("wallet send", flag.ExitOnError)
	to := fs.String("to", "", "Destination P2PKH address or 66-hex BRC-100 identity pubkey")
	sats := fs.Uint64("sats", 0, "Amount in satoshis to send")
	desc := fs.String("desc", "nektar wallet send", "Action description")
	confirm := fs.String("confirm", os.Getenv("NEKTAR_MAINNET_CONFIRM"), "Required I_UNDERSTAND on mainnet")
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	_ = fs.Parse(args)

	if strings.TrimSpace(*to) == "" || *sats == 0 {
		return errors.New("usage: nektar wallet send --to <address|identityPub> --sats <n>")
	}
	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveWallet(info, http.MethodPost, "/v1/wallet/send", map[string]any{
			"to":          *to,
			"sats":        *sats,
			"description": *desc,
			"confirm":     *confirm,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	if rt.mem.Chain().WalletMode() == "client" {
		return errors.New("direct wallet sends are disabled in BSV Desktop client mode — send funds directly from BSV Desktop (Nektar holds zero signing keys)")
	}
	if rt.mem.Chain().Mainnet() && *confirm != "I_UNDERSTAND" {
		return errors.New("mainnet send requires --confirm I_UNDERSTAND")
	}
	sa := nwallet.SendArgs{Satoshis: *sats, Description: *desc}
	t := strings.TrimSpace(*to)
	if len(t) == 66 && (strings.HasPrefix(t, "02") || strings.HasPrefix(t, "03")) {
		sa.ToIdentityKey = t
	} else {
		sa.ToAddress = t
	}
	res, err := rt.mem.Chain().Send(ctx, sa)
	if err != nil {
		return err
	}
	return printJSON(res)
}

func walletSweep(args []string) error {
	fs := flag.NewFlagSet("wallet sweep", flag.ExitOnError)
	to := fs.String("to", "", "Destination P2PKH address or 66-hex BRC-100 identity pubkey")
	toDesktop := fs.Bool("to-desktop", false, "Sweep 100% of funds directly into local BSV Desktop via BRC-29")
	wireURL := fs.String("wire-url", envOr("NEKTAR_WALLET_WIRE_URL", nwallet.DefaultDesktopWireURL), "Local BRC-100 wallet wire URL")
	confirm := fs.String("confirm", os.Getenv("NEKTAR_MAINNET_CONFIRM"), "Required I_UNDERSTAND on mainnet")
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	_ = fs.Parse(args)

	if !*toDesktop && strings.TrimSpace(*to) == "" {
		return errors.New("usage: nektar wallet sweep --to <address|identityPub> | --to-desktop")
	}
	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveWallet(info, http.MethodPost, "/v1/wallet/sweep", map[string]any{
			"to":        *to,
			"toDesktop": *toDesktop,
			"wireUrl":   *wireURL,
			"confirm":   *confirm,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	if rt.mem.Chain().WalletMode() == "client" {
		return errors.New("direct wallet sends are disabled in BSV Desktop client mode — send funds directly from BSV Desktop (Nektar holds zero signing keys)")
	}
	if rt.mem.Chain().Mainnet() && *confirm != "I_UNDERSTAND" {
		return errors.New("mainnet sweep requires --confirm I_UNDERSTAND")
	}
	if *toDesktop {
		res, err := rt.mem.Chain().SweepToDesktop(ctx, *wireURL, true)
		if err != nil {
			return err
		}
		return printJSON(res)
	}
	sw := nwallet.SweepArgs{Description: "cli wallet sweep", OverrideGuard: true}
	t := strings.TrimSpace(*to)
	if len(t) == 66 && (strings.HasPrefix(t, "02") || strings.HasPrefix(t, "03")) {
		sw.ToIdentityKey = t
	} else {
		sw.ToAddress = t
	}
	res, err := rt.mem.Chain().Sweep(ctx, sw)
	if err != nil {
		return err
	}
	return printJSON(res)
}

func walletConsolidate(args []string) error {
	fs := flag.NewFlagSet("wallet consolidate", flag.ExitOnError)
	below := fs.Uint64("below", 10_000, "Merge spendable UTXOs smaller than this satoshi amount")
	maxIn := fs.Int("max", 100, "Maximum inputs per consolidation tx")
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	_ = fs.Parse(args)

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveWallet(info, http.MethodPost, "/v1/wallet/consolidate", map[string]any{
			"belowSats": *below,
			"maxInputs": *maxIn,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	res, err := rt.mem.Chain().Consolidate(ctx, nwallet.ConsolidateArgs{
		BelowSats: *below,
		MaxInputs: *maxIn,
	})
	if err != nil {
		return err
	}
	return printJSON(res)
}

func walletFuel(args []string) error {
	fs := flag.NewFlagSet("wallet fuel", flag.ExitOnError)
	target := fs.Int("target", 24, "Target number of spendable fuel UTXOs")
	sats := fs.Uint64("sats", 2000, "Satoshis per fuel UTXO")
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	_ = fs.Parse(args)

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveWallet(info, http.MethodPost, "/v1/wallet/fuel", map[string]any{
			"target": *target,
			"sats":   *sats,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	eng := rt.mem.Chain().Engine()
	if eng == nil {
		return errors.New("fuel fan-out requires embedded wallet mode")
	}
	minted, err := eng.FuelKeeper().WarmFuel(ctx, *target, *sats)
	if err != nil {
		return err
	}
	bal, _ := eng.Balance(ctx)
	return printJSON(map[string]any{
		"ok":        true,
		"minted":    minted,
		"fuelCount": bal.FuelCount,
		"utxos":     bal.SpendableCount,
	})
}
