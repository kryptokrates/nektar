package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
)

func (tb *Toolbox) ExecWant(ctx context.Context, req market.WantRequest) (any, error) {
	if tb.broker == nil {
		return nil, errors.New("market broker is not attached (start with `nektar serve`)")
	}
	return tb.broker.Want(ctx, req)
}

type WatchKnowledgeInput struct {
	Action        string   `json:"action,omitempty"` // create (default) | list | steer | cancel
	OrderID       string   `json:"orderId,omitempty"`
	Domains       []string `json:"domains,omitempty"`
	Domain        string   `json:"domain,omitempty"`
	Query         string   `json:"query,omitempty"`
	MaxSats       uint64   `json:"maxSats,omitempty"`
	TargetShard   string   `json:"targetShard,omitempty"`
	TTLSeconds    int64    `json:"ttlSeconds,omitempty"`
	MaxFills      int      `json:"maxFills,omitempty"`
	Paused        *bool    `json:"paused,omitempty"`
	ExtendSeconds int64    `json:"extendSeconds,omitempty"`
	Reason        string   `json:"reason,omitempty"`
}

func (tb *Toolbox) ExecWatchKnowledge(ctx context.Context, in WatchKnowledgeInput) (any, error) {
	if tb.broker == nil {
		return nil, errors.New("market broker is not attached (start with `nektar serve`)")
	}
	action := strings.ToLower(strings.TrimSpace(in.Action))
	if action == "" {
		action = "create"
	}
	switch action {
	case "list":
		return map[string]any{"standingOrders": tb.broker.ListStandingOrders(ctx)}, nil
	case "steer":
		if strings.TrimSpace(in.OrderID) == "" {
			return nil, errors.New("watchKnowledge steer requires orderId")
		}
		var maxPtr *uint64
		if in.MaxSats > 0 {
			v := in.MaxSats
			maxPtr = &v
		}
		ok, err := tb.broker.SteerStandingOrder(ctx, in.OrderID, market.SteerStandingOrderInput{
			MaxSats:       maxPtr,
			Paused:        in.Paused,
			ExtendSeconds: in.ExtendSeconds,
			Reason:        in.Reason,
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": ok, "orderId": in.OrderID}, nil
	case "cancel":
		if strings.TrimSpace(in.OrderID) == "" {
			return nil, errors.New("watchKnowledge cancel requires orderId")
		}
		ok, err := tb.broker.CancelStandingOrder(ctx, in.OrderID, in.Reason)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": ok, "orderId": in.OrderID}, nil
	default:
		doms := in.Domains
		if len(doms) == 0 && in.Domain != "" {
			doms = []string{in.Domain}
		}
		return tb.broker.StartStandingOrder(ctx, market.StandingOrderInput{
			OrderID:     in.OrderID,
			Domains:     doms,
			Query:       in.Query,
			MaxSats:     in.MaxSats,
			TargetShard: in.TargetShard,
			TTLSeconds:  in.TTLSeconds,
			MaxFills:    in.MaxFills,
		})
	}
}

type SellToolInput struct {
	Shard       string   `json:"shard"`
	Domains     []string `json:"domains,omitempty"`
	Domain      string   `json:"domain,omitempty"`
	PriceSats   uint64   `json:"priceSats,omitempty"`
	Description string   `json:"description,omitempty"`
	Desc        string   `json:"desc,omitempty"`
	// Optional: submit directly to a Hub knowledge bounty instead of listing!
	BountyID string `json:"bountyId,omitempty"`
	HubURL   string `json:"hubUrl,omitempty"`
	Seq      int64  `json:"seq,omitempty"`
}

func (tb *Toolbox) ExecSell(ctx context.Context, in SellToolInput) (any, error) {
	if tb.broker == nil {
		return nil, errors.New("market broker is not attached")
	}
	if in.BountyID != "" {
		return tb.broker.SellToHub(ctx, in.HubURL, in.BountyID, in.Shard, in.Seq)
	}
	doms := in.Domains
	if len(doms) == 0 && in.Domain != "" {
		doms = []string{in.Domain}
	}
	desc := in.Description
	if desc == "" {
		desc = in.Desc
	}
	return tb.broker.ArmSell(ctx, market.SellInput{
		Shard:       in.Shard,
		Domains:     doms,
		PriceSats:   in.PriceSats,
		Description: desc,
	})
}

func (tb *Toolbox) ExecDeals(_ context.Context) map[string]any {
	deals, _ := tb.mem.DB().ListDeals(50)
	pending, _ := tb.mem.DB().ListPendingApprovals()
	listings, _ := tb.mem.DB().ListSellListings()
	reps, _ := tb.mem.DB().ListReputations()
	var liq map[string]any
	var matcherMetrics *market.MatcherMetrics
	if tb.broker != nil {
		liq = tb.broker.LiquidityMetrics()
		mm := tb.broker.MatcherMetrics()
		matcherMetrics = &mm
	}
	return map[string]any{
		"deals":            deals,
		"pendingApprovals": pending,
		"sellListings":     listings,
		"reputations":      reps,
		"liquidity":        liq,
		"matcher":          matcherMetrics,
	}
}

type FundToolInput struct {
	Faucet     bool   `json:"faucet,omitempty"`
	Sync       bool   `json:"sync,omitempty"`
	HubURL     string `json:"hubUrl,omitempty"`
	ImportTxID string `json:"importTxid,omitempty"`
	ImportBEEF string `json:"importBeef,omitempty"`
}

func (tb *Toolbox) ExecFund(ctx context.Context, in FundToolInput) (map[string]any, error) {
	ch := tb.mem.Chain()
	addr, err := ch.FundAddress()
	if err != nil {
		return nil, err
	}
	var importRes any
	if strings.TrimSpace(in.ImportBEEF) != "" {
		raw, dErr := hex.DecodeString(strings.TrimSpace(in.ImportBEEF))
		if dErr != nil {
			return nil, fmt.Errorf("fund: decode importBeef hex: %w", dErr)
		}
		importRes, err = ch.ImportBEEF(ctx, raw)
		if err != nil {
			return nil, err
		}
	} else if strings.TrimSpace(in.ImportTxID) != "" {
		importRes, err = ch.ImportTxID(ctx, in.ImportTxID)
		if err != nil {
			return nil, err
		}
	}

	var syncedAdded, syncedProved, upgradedAnchors int
	if in.Sync {
		if rep, sErr := ch.SyncFull(ctx); sErr == nil {
			syncedAdded, syncedProved = rep.AddedOutputs, rep.ProvedTxs
		}
		upgradedAnchors, _ = tb.mem.RefreshAnchorProofs(ctx)
	}
	bal, _ := ch.Balance(ctx)
	spentToday, maxDay, maxFee, txCount := ch.SpendGuard().Stats()
	out := map[string]any{
		"network":         ch.Network(),
		"walletMode":      ch.WalletMode(),
		"identityPub":     ch.IdentityPubHex(),
		"address":         addr,
		"balanceSats":     bal.SpendableSats,
		"confirmedSats":   bal.ConfirmedSats,
		"unprovenSats":    bal.UnprovenSats,
		"reservedSats":    bal.ReservedSats,
		"utxos":           bal.SpendableCount,
		"fuelCount":       bal.FuelCount,
		"reserveCount":    bal.ReserveCount,
		"defaultCount":    bal.DefaultCount,
		"feeRateSatPerKB": ch.FeeRateSatPerKB(ctx),
		"syncedAdded":     syncedAdded,
		"syncedProved":    syncedProved,
		"upgradedAnchors": upgradedAnchors,
		"spendGuard": map[string]any{
			"spentToday": spentToday,
			"maxPerDay":  maxDay,
			"maxFee":     maxFee,
			"txCount":    txCount,
		},
	}
	if importRes != nil {
		out["imported"] = importRes
	}
	if in.Faucet {
		txid, sats, fErr := tb.requestFaucet(ctx, addr, in.HubURL)
		if fErr != nil {
			out["faucetError"] = fErr.Error()
		} else {
			out["faucetTxid"] = txid
			out["faucetSats"] = sats
			out["balanceSats"] = ch.BalanceSats()
		}
	}
	return out, nil
}

func (tb *Toolbox) requestFaucet(ctx context.Context, address, hubURL string) (string, uint64, error) {
	if tb.mem.Chain().Mainnet() {
		return "", 0, errors.New("faucet is disabled on mainnet")
	}
	// If this node hosts an embedded hub in memory/testnet mode, drip locally!
	if tb.hubNode != nil {
		txid, _, sats, err := hub.DripFaucetBEEF(ctx, tb.mem, address, 2000, 10_000)
		return txid, sats, err
	}
	// Otherwise call connected Hub /v1/faucet and verify carried AtomicBEEF immediately (Finding #12, #20)!
	targets := []string{}
	if hubURL != "" {
		targets = append(targets, strings.TrimRight(hubURL, "/"))
	} else if tb.disc != nil {
		for _, h := range tb.disc.ActiveHosts() {
			base := strings.TrimSuffix(strings.TrimRight(h, "/"), "/hub")
			targets = append(targets, base)
		}
	}
	body, _ := json.Marshal(map[string]any{"address": address})
	isLive := tb.mem.Chain().Network() != "memory"
	for _, base := range targets {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/faucet", bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := tb.httpClient.Do(req)
		if err != nil {
			continue
		}
		var parsed struct {
			TxID    string `json:"txid"`
			BEEFHex string `json:"beefHex"`
			Sats    uint64 `json:"sats"`
			Error   string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&parsed)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && parsed.TxID != "" {
			if strings.TrimSpace(parsed.BEEFHex) == "" {
				if isLive {
					return "", 0, fmt.Errorf("faucet %s returned bare txid %s without required AtomicBEEF", base, parsed.TxID)
				}
				return parsed.TxID, parsed.Sats, nil
			}
			rawBeef, dErr := hex.DecodeString(strings.TrimSpace(parsed.BEEFHex))
			if dErr != nil {
				return "", 0, fmt.Errorf("faucet %s returned invalid beefHex: %w", base, dErr)
			}
			_, tx, txHash, pErr := guard.ParseBEEF(rawBeef, guard.DefaultBound)
			if pErr != nil || tx == nil || txHash == nil {
				return "", 0, fmt.Errorf("faucet %s returned unparseable AtomicBEEF: %v", base, pErr)
			}
			if !strings.EqualFold(txHash.String(), parsed.TxID) {
				return "", 0, fmt.Errorf("faucet %s txid %s mismatches AtomicBEEF txid %s", base, parsed.TxID, txHash.String())
			}
			fundScript, fsErr := tb.mem.Chain().Signer().FundScript()
			if fsErr != nil {
				return "", 0, fsErr
			}
			var gotSats uint64
			for _, o := range tx.Outputs {
				if o.LockingScript != nil && bytes.Equal(*o.LockingScript, *fundScript) {
					gotSats += o.Satoshis
				}
			}
			if gotSats == 0 || (parsed.Sats > 0 && gotSats < parsed.Sats) {
				return "", 0, fmt.Errorf("faucet %s BEEF pays %d sats to our address (< declared %d sats)", base, gotSats, parsed.Sats)
			}
			if isLive {
				if _, vErr := chain.VerifyCounterpartyPaymentBEEF(ctx, rawBeef, tb.mem.Chain().Tracker(), chain.PaymentVerifyOpts{
					ExpectedAmountSats: gotSats,
					SettleDepth:        tb.mem.Chain().SettleDepth(),
					AllowZeroConf:      true,
				}); vErr != nil {
					return "", 0, fmt.Errorf("faucet %s SPV verification failed: %w", base, vErr)
				}
				if tb.mem.Chain().WalletMode() == "embedded" {
					if _, impErr := tb.mem.Chain().ImportBEEF(ctx, rawBeef); impErr != nil {
						return "", 0, fmt.Errorf("faucet %s import verified BEEF failed: %w", base, impErr)
					}
				}
			}
			return parsed.TxID, gotSats, nil
		}
		if parsed.Error != "" {
			return "", 0, errors.New(parsed.Error)
		}
	}
	return "", 0, errors.New("no testnet faucet reachable (join a hub with faucet enabled or send test coins to your fee address)")
}
