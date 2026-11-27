package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/server"
)

func cmdWallet(args []string) error {
	if len(args) == 0 {
		printWalletUsage()
		return nil
	}
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "help", "-h", "--help":
		printWalletUsage()
		return nil
	case "status":
		return walletStatus(rest)
	case "address":
		return walletAddress(rest)
	case "utxos":
		return walletUTXOs(rest)
	case "actions":
		return walletActions(rest)
	case "sync":
		return walletSync(rest)
	case "fund":
		return walletFund(rest)
	case "import":
		return walletImport(rest)
	case "send":
		return walletSend(rest)
	case "sweep":
		return walletSweep(rest)
	case "consolidate":
		return walletConsolidate(rest)
	case "fuel":
		return walletFuel(rest)
	default:
		printWalletUsage()
		return fmt.Errorf("unknown wallet subcommand %q", sub)
	}
}

func printWalletUsage() {
	fmt.Println(`nektar wallet — Pure-Go BRC-100 wallet, UTXO management, funding & retrieval

Subcommands:
  nektar wallet status                                           Show confirmed/unproven/reserved balance, UTXOs by basket & fee rate
  nektar wallet address                                          Show P2PKH funding address, identity pubkey & bitcoin: URI
  nektar wallet utxos [--basket all|default|reserve|fuel]        List wallet UTXOs by basket and maturity
  nektar wallet actions [--limit 20]                             List recent BRC-100 actions and settlement status
  nektar wallet sync                                             Sync external deposits, collect proofs, compact BEEFs & clean stale holds
  nektar wallet fund --from-desktop --sats 25000 [--wire-url ..] 1-click BRC-29 funding from local BSV Desktop (127.0.0.1:3321)
  nektar wallet import --beef <hex|@file> | --txid <txid>        Import verified BEEF/AtomicBEEF or mined txid into wallet
  nektar wallet send --to <addr|identityPub> --sats <n>          Send sats to a P2PKH address or BRC-100 identity pubkey (BRC-29)
  nektar wallet sweep --to <addr|identityPub> | --to-desktop     Sweep 100% of spendable funds (default+reserve+fuel, fund+BRC-29 keys)
  nektar wallet consolidate [--below 10000] [--max 100]          Merge fragmented small UTXOs into clean reserve outputs
  nektar wallet fuel [--target 24] [--sats 2000]                 Pre-warm exact-denomination fuel UTXOs for high-load bursts`)
}

func callLiveWallet(info *server.DaemonInfo, method, path string, body any) (any, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(info.URL, "/")+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	tok := strings.TrimSpace(os.Getenv("NEKTAR_OPERATOR_TOKEN"))
	if tok == "" && info.DataDir != "" {
		if rawOp, rErr := os.ReadFile(filepath.Join(info.DataDir, "operator-token")); rErr == nil {
			tok = strings.TrimSpace(string(rawOp))
		}
	}
	if tok == "" {
		tok = info.AuthToken
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: 65 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("daemon HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return string(raw), nil
	}
	return out, nil
}

func walletStatus(args []string) error {
	fs := flag.NewFlagSet("wallet status", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	_ = fs.Parse(args)

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		if out, err := callLiveWallet(info, http.MethodGet, "/v1/wallet", nil); err == nil {
			return printJSON(out)
		}
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	ch := rt.mem.Chain()
	addr, _ := ch.FundAddress()
	bal, err := ch.Balance(ctx)
	if err != nil {
		return err
	}
	spentToday, maxDay, maxFee, txCount := ch.SpendGuard().Stats()
	return printJSON(map[string]any{
		"network":          ch.Network(),
		"walletMode":       ch.WalletMode(),
		"identityPub":      ch.IdentityPubHex(),
		"fundAddress":      addr,
		"balanceSats":      bal.SpendableSats,
		"confirmedSats":    bal.ConfirmedSats,
		"unprovenSats":     bal.UnprovenSats,
		"reservedSats":     bal.ReservedSats,
		"totalSats":        bal.TotalSats,
		"utxos":            bal.SpendableCount,
		"fuelCount":        bal.FuelCount,
		"fuelSats":         bal.FuelSats,
		"reserveCount":     bal.ReserveCount,
		"reserveSats":      bal.ReserveSats,
		"defaultCount":     bal.DefaultCount,
		"defaultSats":      bal.DefaultSats,
		"customCount":      bal.CustomCount,
		"maxUnprovenDepth": bal.MaxUnprovenDepth,
		"pendingTxCount":   bal.PendingTxCount,
		"feeRateSatPerKB":  ch.FeeRateSatPerKB(ctx),
		"spendGuard": map[string]any{
			"spentToday": spentToday,
			"maxPerDay":  maxDay,
			"maxFee":     maxFee,
			"txCount":    txCount,
		},
	})
}

func walletAddress(args []string) error {
	fs := flag.NewFlagSet("wallet address", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	showQR := fs.Bool("qr", false, "Render scannable terminal QR code for the funding address")
	_ = fs.Parse(args)

	var netOut, addr, idPub string
	if info, ok := detectDaemon(*dataDir, *netName); ok {
		if out, err := callLiveWallet(info, http.MethodGet, "/v1/wallet", nil); err == nil {
			if m, ok := out.(map[string]any); ok {
				netOut, _ = m["network"].(string)
				addr, _ = m["fundAddress"].(string)
				idPub, _ = m["identityPub"].(string)
			}
		}
	}
	if addr == "" {
		ctx := context.Background()
		rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		a, err := rt.mem.Chain().FundAddress()
		if err != nil {
			return err
		}
		netOut = rt.mem.Chain().Network()
		addr = a
		idPub = rt.mem.Chain().IdentityPubHex()
	}
	uri := "bitcoin:" + addr
	if *showQR {
		qr, err := core.RenderTerminalQR(uri)
		if err != nil {
			return err
		}
		fmt.Printf("\nBSV %s Funding Address\n", strings.ToUpper(netOut))
		fmt.Print(qr)
		fmt.Printf("Address  : %s\n", addr)
		fmt.Printf("URI      : %s\n", uri)
		fmt.Printf("Identity : %s\n", idPub)
		if netOut == "main" {
			fmt.Println("Tip      : Send 5,000–10,000 real sats (~$0.003), then run `nektar wallet sync --network main`")
		}
		return nil
	}
	return printJSON(map[string]any{
		"network":     netOut,
		"fundAddress": addr,
		"identityPub": idPub,
		"uri":         uri,
	})
}

func walletUTXOs(args []string) error {
	fs := flag.NewFlagSet("wallet utxos", flag.ExitOnError)
	basket := fs.String("basket", "all", "Basket filter: all|default|reserve|fuel|<custom>")
	limit := fs.Int("limit", 100, "Max UTXOs to display")
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	_ = fs.Parse(args)

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		if out, err := callLiveWallet(info, http.MethodGet, fmt.Sprintf("/v1/wallet/utxos?basket=%s&limit=%d", *basket, *limit), nil); err == nil {
			return printJSON(out)
		}
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	eng := rt.mem.Chain().Engine()
	if eng == nil {
		return errors.New("utxos listing directly requires embedded mode or a live daemon")
	}
	outs, total, err := eng.ListUTXOs(ctx, *basket, false, *limit, 0)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"basket": *basket, "total": total, "utxos": outs})
}

func walletActions(args []string) error {
	fs := flag.NewFlagSet("wallet actions", flag.ExitOnError)
	limit := fs.Int("limit", 20, "Max actions to display")
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	_ = fs.Parse(args)

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		if out, err := callLiveWallet(info, http.MethodGet, fmt.Sprintf("/v1/wallet/actions?limit=%d", *limit), nil); err == nil {
			return printJSON(out)
		}
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	eng := rt.mem.Chain().Engine()
	if eng == nil {
		return errors.New("actions listing directly requires embedded mode or a live daemon")
	}
	txs, total, err := eng.ListRecentActions(ctx, *limit, 0)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"total": total, "actions": txs})
}

func walletSync(args []string) error {
	fs := flag.NewFlagSet("wallet sync", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	_ = fs.Parse(args)

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveWallet(info, http.MethodPost, "/v1/wallet/sync", map[string]any{})
		if err != nil {
			return fmt.Errorf("live daemon sync failed (refusing concurrent local wallet open): %w", err)
		}
		return printJSON(out)
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	rep, err := rt.mem.Chain().SyncFull(ctx)
	upgraded, _ := rt.mem.RefreshAnchorProofs(ctx)
	bal, _ := rt.mem.Chain().Balance(ctx)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{
		"ok":              true,
		"report":          rep,
		"upgradedAnchors": upgraded,
		"balance":         bal,
	})
}
