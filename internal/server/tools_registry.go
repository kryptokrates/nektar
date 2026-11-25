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
	"time"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
)

// ToolSpec describes one unified tool exposed across MCP, POST /v1/tools/{name}, GET /v1/tools, and A2A.
type ToolSpec struct {
	Name        string `json:"name"`
	Profile     string `json:"profile"` // agent | operator
	Description string `json:"description"`
}

// Toolbox is the single source of truth backing MCP stdio/HTTP, REST /v1/tools/{name}, and A2A.
type Toolbox struct {
	mem        *memory.Service
	mkt        *market.Engine
	broker     *market.Broker
	disc       *discovery.Service
	hubNode    *hub.Node
	profile    string // "agent" (focused 10-tool default) | "operator" (all 18 tools)
	httpClient *http.Client
}

func NewToolbox(mem *memory.Service, mkt *market.Engine, broker *market.Broker, disc *discovery.Service, hubNode *hub.Node, profile string) *Toolbox {
	if profile == "" {
		profile = "operator"
	}
	if disc == nil && broker != nil {
		disc = broker.Discovery()
	}
	netName := ""
	if mem != nil && mem.Chain() != nil {
		netName = mem.Chain().Network()
	}
	return &Toolbox{
		mem:        mem,
		mkt:        mkt,
		broker:     broker,
		disc:       disc,
		hubNode:    hubNode,
		profile:    strings.ToLower(strings.TrimSpace(profile)),
		httpClient: core.SafeHTTPClientForNetwork(netName, 15*time.Second),
	}
}

func (tb *Toolbox) Profile() string { return tb.profile }

// ListTools returns the catalog visible to the active tool profile.
func (tb *Toolbox) ListTools() []ToolSpec {
	all := []ToolSpec{
		// Core 10-tool agent profile (default cognitive-load budget):
		{Name: "remember", Profile: "agent", Description: "Commit a provenance-signed memory fragment into a shard, optionally BRC-369 encrypting to a Borg group and/or anchoring on BSV."},
		{Name: "recall", Profile: "agent", Description: "Recall active, Cedar-authorized, Borg-decrypted fragments. Omit shard to recall across all non-system shards."},
		{Name: "forget", Profile: "agent", Description: "Shred local plaintext/ciphertext of a fragment while preserving its on-chain Merkle commitment."},
		{Name: "groundedAnswer", Profile: "agent", Description: "Verify claims against cited memory fragments (quote + Google Mangle Datalog), strip hallucinations, commit a receipt, and return the nektarr1.* verification footer."},
		{Name: "verifyReceipt", Profile: "agent", Description: "Verify a nektarr1.* receipt token, its §16 delegations, and the SPV settlement status of every cited fragment."},
		{Name: "want", Profile: "agent", Description: "Buy verified knowledge live from peer agents or the Hub over BSV AtomicBEEF + BRC-369 keyed disclosure."},
		{Name: "sell", Profile: "agent", Description: "Arm a local memory shard for autonomous A2A selling, or submit a verified anchored fragment to a Hub knowledge bounty."},
		{Name: "deals", Profile: "agent", Description: "List persisted market deals, pending operator approvals, verified seller reputation, and network liquidity metrics."},
		{Name: "fund", Profile: "agent", Description: "Show wallet funding address, satoshi balance, UTXO count, SpendGuard caps, and optionally request a testnet faucet drip."},
		{Name: "doctor", Profile: "agent", Description: "Run a complete self-diagnosis (SQLite, wallet, hosts, peers, policies, SSE vs poll health, welcome shard) with exact next steps."},
		// Operator / advanced profile tools:
		{Name: "peers", Profile: "operator", Description: "Discover and list signature-verified peers, bfinger handles, sell domains, advertised CHIRP packs, and Hub bounties."},
		{Name: "anchor", Profile: "operator", Description: "Anchor all unanchored fragments in a shard onto BSV via the NektarAnchor Rúnar covenant (guarded by SpendGuard)."},
		{Name: "prove", Profile: "operator", Description: "Build a self-contained SPV ProofBundle (Merkle path + NektarAnchor BEEF/BUMP) and verify it offline against headers."},
		{Name: "borgGroup", Profile: "operator", Description: "Manage Borg access groups and BRC-369 epoch key rotation (create, add, revoke, get)."},
		{Name: "exportPack", Profile: "operator", Description: "Export a shard and its anchor proofs as a verified BRC-167 CHIRP closure."},
		{Name: "importPack", Profile: "operator", Description: "Import and verify a local or remote BRC-167 CHIRP shard pack closure into a local shard."},
		{Name: "authorize", Profile: "operator", Description: "Evaluate the active native Cedar policy set for a principal, action, and resource."},
		{Name: "status", Profile: "operator", Description: "Return Nektar v0.4 node status: identity, network, wallet balance, shards, SpendGuard, policy hash, and market deals."},
	}
	if tb.profile == "agent" {
		out := make([]ToolSpec, 0, 10)
		for _, t := range all {
			if t.Profile == "agent" {
				out = append(out, t)
			}
		}
		return out
	}
	return all
}

// IsAllowedTool reports whether tool name is permitted under the active tool profile.
func (tb *Toolbox) IsAllowedTool(name string) bool {
	for _, t := range tb.ListTools() {
		if t.Name == name {
			return true
		}
	}
	return false
}

// Call executes any tool by name from raw JSON args (used identically by HTTP /v1/tools/{name}, A2A, and CLI).
func (tb *Toolbox) Call(ctx context.Context, name string, rawArgs json.RawMessage) (any, error) {
	if !tb.IsAllowedTool(name) {
		return nil, fmt.Errorf("tool %q is disabled under profile %q (set NEKTAR_TOOL_PROFILE=operator to enable)", name, tb.profile)
	}
	if len(rawArgs) == 0 {
		rawArgs = json.RawMessage(`{}`)
	}
	switch name {
	case "remember":
		var raw struct {
			Shard      string            `json:"shard"`
			Text       string            `json:"text"`
			Data       string            `json:"data,omitempty"`
			Source     string            `json:"source,omitempty"`
			Level      int               `json:"level,omitempty"`
			Class      string            `json:"class,omitempty"`
			BorgGroup  string            `json:"borgGroup,omitempty"`
			CallerPub  string            `json:"callerPub,omitempty"`
			Supersedes json.RawMessage   `json:"supersedes,omitempty"`
			AutoAnchor bool              `json:"autoAnchor,omitempty"`
			Extra      map[string]string `json:"extra,omitempty"`
		}
		if err := json.Unmarshal(rawArgs, &raw); err != nil {
			return nil, err
		}
		in := memory.RememberInput{
			Shard:      raw.Shard,
			Text:       raw.Text,
			Source:     raw.Source,
			Level:      raw.Level,
			Class:      raw.Class,
			BorgGroup:  raw.BorgGroup,
			CallerPub:  raw.CallerPub,
			AutoAnchor: raw.AutoAnchor,
			Extra:      raw.Extra,
		}
		if in.Text == "" && raw.Data != "" {
			in.Text = raw.Data // Hermes NektarMemoryProvider uses `data`
		}
		if len(raw.Supersedes) > 0 && string(raw.Supersedes) != "null" {
			var seq int64
			if err := json.Unmarshal(raw.Supersedes, &seq); err == nil {
				in.Supersedes = seq
			} else {
				var obj struct {
					Shard string `json:"shard"`
					Seq   int64  `json:"seq"`
				}
				if err := json.Unmarshal(raw.Supersedes, &obj); err == nil {
					in.Supersedes = obj.Seq
					if in.Shard == "" && obj.Shard != "" {
						in.Shard = obj.Shard
					}
				}
			}
		}
		if p, disallowBorg, overridden := PrincipalForContext(ctx, policy.Principal{}); overridden {
			in.Principal = &p
			in.CallerPub = ""
			if disallowBorg && in.BorgGroup != "" {
				return nil, errors.New("operator token required for Borg group writes")
			}
		}
		return tb.mem.Remember(ctx, in)

	case "recall":
		var in memory.RecallInput
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		if p, disallowBorg, overridden := PrincipalForContext(ctx, in.Principal); overridden {
			in.Principal = p
			in.CallerPub = ""
			in.DisallowBorg = disallowBorg
		}
		return tb.ExecRecall(ctx, in)

	case "forget":
		var in struct {
			Shard string `json:"shard"`
			Seq   int64  `json:"seq"`
		}
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		if p, _, overridden := PrincipalForContext(ctx, policy.Principal{}); overridden {
			if err := tb.mem.ForgetAuthorized(ctx, in.Shard, in.Seq, p); err != nil {
				return nil, err
			}
		} else if err := tb.mem.Forget(ctx, in.Shard, in.Seq); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "shard": in.Shard, "seq": in.Seq}, nil

	case "groundedAnswer":
		var in memory.GroundedAnswerInput
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		if p, disallowBorg, overridden := PrincipalForContext(ctx, in.Principal); overridden {
			in.Principal = p
			in.CallerPub = ""
			in.DisallowBorg = disallowBorg
		}
		return tb.mem.GroundedAnswer(ctx, in)

	case "verifyReceipt":
		var in struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		return tb.mem.VerifyReceipt(ctx, in.Token)

	case "want":
		var in struct {
			Domains     []string `json:"domains"`
			Domain      string   `json:"domain,omitempty"`
			Question    string   `json:"question"`
			Query       string   `json:"query,omitempty"`
			MaxSats     uint64   `json:"maxSats"`
			TargetShard string   `json:"targetShard,omitempty"`
		}
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		if len(in.Domains) == 0 && in.Domain != "" {
			in.Domains = []string{in.Domain}
		}
		q := in.Question
		if q == "" {
			q = in.Query
		}
		return tb.ExecWant(ctx, market.WantRequest{
			Domains:     in.Domains,
			Question:    q,
			MaxSats:     in.MaxSats,
			TargetShard: in.TargetShard,
		})

	case "sell":
		var in SellToolInput
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		return tb.ExecSell(ctx, in)

	case "deals":
		return tb.ExecDeals(ctx), nil

	case "fund":
		var in FundToolInput
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		return tb.ExecFund(ctx, in)

	case "doctor":
		return tb.ExecDoctor(ctx), nil

	case "peers":
		var in struct {
			Refresh bool `json:"refresh,omitempty"`
		}
		_ = json.Unmarshal(rawArgs, &in)
		return tb.ExecPeers(ctx, in.Refresh), nil

	case "anchor":
		var in struct {
			Shard string `json:"shard"`
		}
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		if in.Shard == "" {
			in.Shard = "default"
		}
		a, err := tb.mem.AnchorShard(ctx, in.Shard)
		if err != nil {
			return nil, err
		}
		if a == nil {
			return map[string]any{"ok": true, "anchored": false, "reason": "nothing unanchored"}, nil
		}
		return a, nil

	case "prove":
		var in struct {
			Shard string `json:"shard"`
			Seq   int64  `json:"seq"`
		}
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		if p, _, overridden := PrincipalForContext(ctx, policy.Principal{}); overridden {
			bundle, vr, err := tb.mem.ProveAuthorized(ctx, in.Shard, in.Seq, p)
			if err != nil {
				return nil, err
			}
			return map[string]any{"bundle": bundle, "verify": vr}, nil
		}
		bundle, vr, err := tb.mem.Prove(ctx, in.Shard, in.Seq)
		if err != nil {
			return nil, err
		}
		return map[string]any{"bundle": bundle, "verify": vr}, nil

	case "borgGroup":
		var in BorgGroupInput
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		return tb.ExecBorgGroupCtx(ctx, in)

	case "exportPack":
		var in struct {
			Shard string `json:"shard"`
		}
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		if p, disallowBorg, overridden := PrincipalForContext(ctx, policy.Principal{}); overridden {
			return tb.mem.ExportShardPackAuthorized(ctx, in.Shard, p, "disclose", disallowBorg)
		}
		return tb.mem.ExportShardPack(ctx, in.Shard)

	case "importPack":
		var in struct {
			ChirpURL    string `json:"chirpUrl"`
			TargetShard string `json:"targetShard,omitempty"`
			PeerURL     string `json:"peerUrl,omitempty"`
		}
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		if in.PeerURL != "" && tb.disc != nil {
			raw, err := tb.disc.FetchRemoteChirp(ctx, in.PeerURL, in.ChirpURL)
			if err != nil {
				return nil, err
			}
			n, sh, err := tb.mem.ImportShardPackBytes(raw, in.TargetShard)
			if err != nil {
				return nil, err
			}
			return map[string]any{"imported": n, "shard": sh}, nil
		}
		n, err := tb.mem.ImportShardPack(ctx, in.ChirpURL, in.TargetShard)
		if err != nil {
			return nil, err
		}
		return map[string]any{"imported": n, "shard": in.TargetShard}, nil

	case "authorize":
		var in struct {
			Principal policy.Principal `json:"principal"`
			Action    string           `json:"action"`
			Resource  policy.Resource  `json:"resource"`
		}
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		pc := tb.mem.PolicyCommit()
		dec, err := policy.Authorize(pc.Text, in.Principal, in.Action, in.Resource)
		if err != nil {
			return nil, err
		}
		rcp := policy.MakeReceipt(in.Principal, in.Action, in.Resource, pc, dec.Allow)
		return map[string]any{"decision": dec, "receipt": rcp}, nil

	case "status":
		return BuildStatus(ctx, tb.mem, tb.mkt), nil

	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

// ExecRecall supports omitted shard (returning shards + items + fragments + count for Dashboard & agents).
func (tb *Toolbox) ExecRecall(ctx context.Context, in memory.RecallInput) (map[string]any, error) {
	shards, items, err := tb.mem.RecallAllShards(ctx, in)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []memory.RecalledFragment{}
	}
	if shards == nil {
		shards = []string{}
	}
	return map[string]any{
		"shards":    shards,
		"items":     items,
		"fragments": items,
		"count":     len(items),
	}, nil
}

func (tb *Toolbox) ExecWant(ctx context.Context, req market.WantRequest) (any, error) {
	if tb.broker == nil {
		return nil, errors.New("market broker is not attached (start with `nektar serve`)")
	}
	return tb.broker.Want(ctx, req)
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
		syncedAdded, syncedProved, _ = ch.SyncPool(ctx)
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
		return hub.DripFaucet(ctx, tb.mem, address, 2000, 10_000)
	}
	// Otherwise try calling connected Hub /v1/faucet!
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
			TxID  string `json:"txid"`
			Sats  uint64 `json:"sats"`
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && parsed.TxID != "" {
			return parsed.TxID, parsed.Sats, nil
		}
		if parsed.Error != "" {
			return "", 0, errors.New(parsed.Error)
		}
	}
	return "", 0, errors.New("no testnet faucet reachable (join a hub with faucet enabled or send test coins to your fee address)")
}

type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // green | amber | red
	Detail string `json:"detail"`
}

func (tb *Toolbox) ExecDoctor(ctx context.Context) map[string]any {
	checks := []DoctorCheck{}
	nextSteps := []string{}

	// 1. Memory & welcome shard check
	shards, _ := tb.mem.ListUserShards()
	total, anchored, _ := tb.mem.DB().CountFragments()
	if total > 0 {
		checks = append(checks, DoctorCheck{Name: "memory", Status: "green", Detail: fmt.Sprintf("%d fragment(s) across %d shard(s), %d anchored", total, len(shards), anchored)})
	} else {
		checks = append(checks, DoctorCheck{Name: "memory", Status: "amber", Detail: "no user fragments yet"})
		nextSteps = append(nextSteps, "Run `nektar remember -shard notes -text \"...\"`")
	}

	// 2. Wallet & UTXO check
	bal := tb.mem.Chain().BalanceSats()
	utxos := tb.mem.Chain().UTXOCount()
	addr, _ := tb.mem.Chain().FundAddress()
	if bal > 0 && utxos >= 2 {
		checks = append(checks, DoctorCheck{Name: "wallet", Status: "green", Detail: fmt.Sprintf("%d sats across %d UTXO(s) (%s)", bal, utxos, addr)})
	} else if bal > 0 {
		checks = append(checks, DoctorCheck{Name: "wallet", Status: "amber", Detail: fmt.Sprintf("%d sats on 1 UTXO — will auto-split on next trade", bal)})
	} else {
		checks = append(checks, DoctorCheck{Name: "wallet", Status: "amber", Detail: fmt.Sprintf("0 sats at %s", addr)})
		nextSteps = append(nextSteps, "Fund wallet via 1-click Testnet Faucet or `nektar serve` dashboard")
	}

	// 3. Operator policy check
	policies, _ := tb.mem.DB().ListOperatorPolicies()
	activePols := 0
	for _, p := range policies {
		if p.Active {
			activePols++
		}
	}
	if activePols > 0 {
		checks = append(checks, DoctorCheck{Name: "policy", Status: "green", Detail: fmt.Sprintf("%d active operator market policy(ies)", activePols)})
	} else {
		checks = append(checks, DoctorCheck{Name: "policy", Status: "amber", Detail: "no market policy approved yet (trades will pause for approval)"})
		nextSteps = append(nextSteps, "Run `nektar policy approve --preset both` to enable autonomous buying & selling")
	}

	// 4. Network hosts & peers check
	var hosts []string
	var peers []discovery.PeerInfo
	if tb.disc != nil {
		hosts = tb.disc.ActiveHosts()
		peers = tb.disc.Peers()
	}
	if len(hosts) > 0 {
		checks = append(checks, DoctorCheck{Name: "network", Status: "green", Detail: fmt.Sprintf("%d active hub host(s), %d verified peer(s)", len(hosts), len(peers))})
	} else {
		checks = append(checks, DoctorCheck{Name: "network", Status: "amber", Detail: "single-player mode (no hub hosts configured)"})
		nextSteps = append(nextSteps, "Start a local Hub (`nektar serve --hub`) or join a Hub in Dashboard -> Network")
	}

	// 5. SSE vs Poll health & host withholding telemetry
	if tb.broker != nil {
		liq := tb.broker.LiquidityMetrics()
		checks = append(checks, DoctorCheck{
			Name:   "market_stream",
			Status: "green",
			Detail: fmt.Sprintf("sseWakes=%v pollWakes=%v bboxWakes=%v", liq["sseWakes"], liq["pollWakes"], liq["bboxWakes"]),
		})
		if mm := tb.broker.MatcherMetrics(); mm.Enabled {
			st := "green"
			detail := fmt.Sprintf("mode=%s requests=%d rejections=%d escalations=%d fallbacks=%d inputTokens=%d",
				mm.DefaultMode, mm.Requests, mm.Rejections, mm.Escalations, mm.Fallbacks, mm.InputTokens)
			if mm.ClientHealth != nil && mm.ClientHealth.CircuitOpen {
				st = "amber"
				detail += " (circuit breaker open)"
			}
			checks = append(checks, DoctorCheck{
				Name:   "jev_matcher",
				Status: st,
				Detail: detail,
			})
		}
	}
	var withholdings []discovery.WithholdingWarning
	if tb.disc != nil {
		withholdings = tb.disc.Withholdings()
		if len(withholdings) > 0 {
			checks = append(checks, DoctorCheck{
				Name:   "host_integrity",
				Status: "amber",
				Detail: fmt.Sprintf("%d host withholding event(s) detected across directory quorum", len(withholdings)),
			})
		}
	}

	overall := "green"
	for _, c := range checks {
		if c.Status == "red" {
			overall = "red"
			break
		}
		if c.Status == "amber" && overall == "green" {
			overall = "amber"
		}
	}
	return map[string]any{
		"ok":              overall != "red",
		"verdict":         overall,
		"network":         tb.mem.Chain().Network(),
		"shardNames":      shards,
		"records":         total,
		"anchoredRecords": anchored,
		"checks":          checks,
		"withholdings":    withholdings,
		"nextSteps":       nextSteps,
		"hints":           nextSteps,
		"truthNote":       reasoning.TruthNote,
	}
}

func (tb *Toolbox) ExecPeers(ctx context.Context, refresh bool) map[string]any {
	var peers []discovery.PeerInfo
	var hosts []string
	if tb.disc != nil {
		hosts = tb.disc.ActiveHosts()
		if refresh {
			peers, _ = tb.disc.DiscoverPeers(ctx, hosts)
		} else {
			peers = tb.disc.Peers()
			if len(peers) == 0 && len(hosts) > 0 {
				peers, _ = tb.disc.DiscoverPeers(ctx, hosts)
			}
		}
	}
	bounties := tb.fetchHubBounties(ctx, hosts)
	return map[string]any{
		"hosts":    hosts,
		"peers":    peers,
		"bounties": bounties,
	}
}

func (tb *Toolbox) fetchHubBounties(ctx context.Context, hosts []string) []hub.Bounty {
	if tb.hubNode != nil {
		if b, err := tb.hubNode.Store().ListBounties(true); err == nil {
			return b
		}
	}
	var out []hub.Bounty
	seen := make(map[string]bool)
	for _, h := range hosts {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(h, "/")+"/bounties", nil)
		if err != nil {
			continue
		}
		resp, err := tb.httpClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}
		var parsed struct {
			Bounties []hub.Bounty `json:"bounties"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed)
		resp.Body.Close()
		for _, b := range parsed.Bounties {
			if !seen[b.ID] {
				seen[b.ID] = true
				out = append(out, b)
			}
		}
	}
	return out
}

type BorgGroupInput struct {
	Op        string            `json:"op"`
	GroupID   string            `json:"groupId"`
	Name      string            `json:"name,omitempty"`
	PubHex    string            `json:"pubHex,omitempty"`
	Role      string            `json:"role,omitempty"`
	Principal *policy.Principal `json:"principal,omitempty"`
}

// ExecBorgGroupCtx enforces operator authentication before mutating or inspecting Borg groups.
func (tb *Toolbox) ExecBorgGroupCtx(ctx context.Context, in BorgGroupInput) (any, error) {
	if _, _, overridden := PrincipalForContext(ctx, policy.Principal{}); overridden {
		return nil, errors.New("operator token required for borgGroup")
	}
	return tb.ExecBorgGroup(in)
}

func (tb *Toolbox) ExecBorgGroup(in BorgGroupInput) (any, error) {
	princ := policy.Principal{Type: "Agent", ID: "owner", Level: 100}
	if in.Principal != nil && in.Principal.ID != "" {
		princ = *in.Principal
	}
	switch in.Op {
	case "create":
		owner := in.PubHex
		if owner == "" {
			owner = tb.mem.Chain().IdentityPubHex()
		}
		return tb.mem.Groups().CreateGroup(in.GroupID, in.Name, owner)
	case "add":
		if dec, err := tb.mem.AuthorizeAction(princ, "rotate", policy.Resource{Type: "Fragment", ID: "group:" + in.GroupID, Shard: in.GroupID, Level: 0}); err != nil || !dec.Allow {
			return nil, fmt.Errorf("cedar policy denied rotate on group %q", in.GroupID)
		}
		if err := tb.mem.Groups().AddMember(in.GroupID, in.PubHex, in.Role); err != nil {
			return nil, err
		}
		g, _ := tb.mem.Groups().GetGroup(in.GroupID)
		return g, nil
	case "revoke":
		if dec, err := tb.mem.AuthorizeAction(princ, "revoke", policy.Resource{Type: "Fragment", ID: "group:" + in.GroupID, Shard: in.GroupID, Level: 0}); err != nil || !dec.Allow {
			return nil, fmt.Errorf("cedar policy denied revoke on group %q", in.GroupID)
		}
		ep, err := tb.mem.Groups().RevokeMember(in.GroupID, in.PubHex)
		if err != nil {
			return nil, err
		}
		return map[string]any{"groupId": in.GroupID, "revoked": in.PubHex, "newEpoch": ep}, nil
	default:
		g, ok := tb.mem.Groups().GetGroup(in.GroupID)
		if !ok {
			return nil, fmt.Errorf("unknown group %q", in.GroupID)
		}
		return g, nil
	}
}
