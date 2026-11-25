package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"

	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/market"
)

func cmdVault(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nektar vault <deploy|status|pause|resume|rotate-agent|sweep> [flags]")
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	rest := args[1:]
	ctx := context.Background()

	switch sub {
	case "deploy":
		fs := flag.NewFlagSet("vault deploy", flag.ExitOnError)
		operatorPub := fs.String("operator-pub", envOr("NEKTAR_OPERATOR_PUBKEY", ""), "Cold operator 33-byte compressed pubkey hex or 20-byte PKH hex")
		deposit := fs.Uint64("deposit", 10000, "Initial vault deposit in satoshis")
		perTxCap := fs.Uint64("per-tx-cap", 2000, "Consensus max satoshis per single agent spend")
		windowCap := fs.Uint64("window-cap", 10000, "Consensus max satoshis per rolling block window")
		windowBlocks := fs.Uint64("window-blocks", 144, "Rolling window duration in blocks (144 ~= 24h)")
		maxFee := fs.Uint64("max-fee", 600, "Consensus max miner fee per agent spend tx")
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest)
		if strings.TrimSpace(*operatorPub) == "" {
			return errors.New("vault deploy: -operator-pub <pubHex|pkhHex> (or NEKTAR_OPERATOR_PUBKEY) is required")
		}
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		rec, err := market.DeployAgentVault(ctx, rt.mem.DB(), rt.mem.Chain(), *operatorPub, *deposit, *perTxCap, *windowCap, *windowBlocks, *maxFee)
		if err != nil {
			return err
		}
		return printJSON(rec)

	case "status":
		fs := flag.NewFlagSet("vault status", flag.ExitOnError)
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest)
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		rec, _, err := market.LoadActiveVault(rt.mem.DB())
		if err != nil {
			return err
		}
		if rec == nil {
			return printJSON(map[string]any{"active": false})
		}
		return printJSON(map[string]any{
			"active":        true,
			"txid":          rec.TxID,
			"vout":          rec.Vout,
			"satoshis":      rec.Satoshis,
			"paused":        rec.State.Paused,
			"operatorPkh":   hex.EncodeToString(rec.State.OperatorPKH),
			"agentPkh":      hex.EncodeToString(rec.State.AgentPKH),
			"perTxCap":      rec.State.PerTxCap,
			"windowCap":     rec.State.WindowCap,
			"windowBlocks":  rec.State.WindowBlocks,
			"maxFee":        rec.State.MaxFee,
			"spentInWindow": rec.State.SpentInWindow,
			"windowStart":   rec.State.WindowStart,
			"updatedAt":     rec.UpdatedAt,
		})

	case "pause", "resume":
		fs := flag.NewFlagSet("vault "+sub, flag.ExitOnError)
		opWIF := fs.String("operator-wif", os.Getenv("NEKTAR_OPERATOR_WIF"), "Cold operator WIF private key (never stored by hot agent)")
		opHex := fs.String("operator-key-hex", os.Getenv("NEKTAR_OPERATOR_KEY_HEX"), "Cold operator 32-byte private key hex")
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest)
		signer, err := loadOperatorVaultSigner(*opWIF, *opHex)
		if err != nil {
			return err
		}
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		paused := sub == "pause"
		rec, err := market.ApplyOperatorVaultControl(ctx, rt.mem.DB(), rt.mem.Chain(), contracts.OperatorControlUpdate{
			NewPaused: &paused,
		}, signer)
		if err != nil {
			return err
		}
		return printJSON(rec)

	case "rotate-agent":
		fs := flag.NewFlagSet("vault rotate-agent", flag.ExitOnError)
		newAgent := fs.String("new-agent", "", "New hot agent 33-byte compressed pubkey hex or 20-byte PKH hex")
		opWIF := fs.String("operator-wif", os.Getenv("NEKTAR_OPERATOR_WIF"), "Cold operator WIF private key")
		opHex := fs.String("operator-key-hex", os.Getenv("NEKTAR_OPERATOR_KEY_HEX"), "Cold operator 32-byte private key hex")
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest)
		if strings.TrimSpace(*newAgent) == "" {
			return errors.New("vault rotate-agent: -new-agent <pubHex|pkhHex> is required")
		}
		newPKH, err := market.ParsePubKeyOrPKH(*newAgent)
		if err != nil {
			return err
		}
		signer, err := loadOperatorVaultSigner(*opWIF, *opHex)
		if err != nil {
			return err
		}
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		rec, err := market.ApplyOperatorVaultControl(ctx, rt.mem.DB(), rt.mem.Chain(), contracts.OperatorControlUpdate{
			NewAgentPKH: newPKH,
		}, signer)
		if err != nil {
			return err
		}
		return printJSON(rec)

	case "sweep":
		fs := flag.NewFlagSet("vault sweep", flag.ExitOnError)
		to := fs.String("to", "", "Destination BSV P2PKH address or 20-byte PKH hex")
		opWIF := fs.String("operator-wif", os.Getenv("NEKTAR_OPERATOR_WIF"), "Cold operator WIF private key")
		opHex := fs.String("operator-key-hex", os.Getenv("NEKTAR_OPERATOR_KEY_HEX"), "Cold operator 32-byte private key hex")
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest)
		if strings.TrimSpace(*to) == "" {
			return errors.New("vault sweep: -to <address|pkhHex> is required")
		}
		destPKH, err := parseDestAddressOrPKH(*to)
		if err != nil {
			return err
		}
		signer, err := loadOperatorVaultSigner(*opWIF, *opHex)
		if err != nil {
			return err
		}
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		txid, sweptSats, err := market.SweepActiveVault(ctx, rt.mem.DB(), rt.mem.Chain(), destPKH, signer)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"txid": txid, "sweptSats": sweptSats, "to": *to})

	default:
		return fmt.Errorf("unknown vault subcommand %q (use deploy|status|pause|resume|rotate-agent|sweep)", sub)
	}
}

func loadOperatorVaultSigner(wif, keyHex string) (contracts.CovenantSighashSigner, error) {
	var priv *ec.PrivateKey
	switch {
	case strings.TrimSpace(wif) != "":
		var err error
		priv, err = ec.PrivateKeyFromWif(strings.TrimSpace(wif))
		if err != nil {
			return nil, fmt.Errorf("vault: invalid operator WIF: %w", err)
		}
	case strings.TrimSpace(keyHex) != "":
		raw, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(keyHex), "0x"))
		if err != nil || len(raw) != 32 {
			return nil, errors.New("vault: operator-key-hex must be 32-byte hex")
		}
		priv, _ = ec.PrivateKeyFromBytes(raw)
		clear(raw)
		if priv == nil {
			return nil, errors.New("vault: invalid operator private key")
		}
	default:
		return nil, errors.New("vault: cold operator key required (-operator-wif / NEKTAR_OPERATOR_WIF or -operator-key-hex)")
	}
	return func(_ context.Context, _ []byte, sighash32 []byte) ([]byte, []byte, error) {
		sig, err := contracts.SignDigestLowS(priv, sighash32, sighash.AllForkID)
		if err != nil {
			return nil, nil, err
		}
		return sig, priv.PubKey().Compressed(), nil
	}, nil
}

func parseDestAddressOrPKH(to string) ([]byte, error) {
	s := strings.TrimSpace(to)
	if len(strings.TrimPrefix(s, "0x")) == 40 {
		if raw, err := hex.DecodeString(strings.TrimPrefix(s, "0x")); err == nil && len(raw) == 20 {
			return raw, nil
		}
	}
	addr, err := script.NewAddressFromString(s)
	if err != nil {
		return nil, fmt.Errorf("vault: invalid destination address/PKH %q: %w", to, err)
	}
	return append([]byte(nil), addr.PublicKeyHash...), nil
}
