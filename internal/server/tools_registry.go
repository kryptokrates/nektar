package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
)

// ToolSpec describes one unified tool exposed across MCP, POST /v1/tools/{name}, GET /v1/tools, and A2A.
type ToolSpec struct {
	Name        string `json:"name"`
	Profile     string `json:"profile"` // agent | operator
	Description string `json:"description"`
}

// Toolbox is the single source of truth backing MCP stdio/HTTP, REST /v1/tools/{name}, and A2A.
type Toolbox struct {
	mem            *memory.Service
	mkt            *market.Engine
	broker         *market.Broker
	disc           *discovery.Service
	hubNode        *hub.Node
	profile        string // "agent" (focused 10-tool default) | "operator" (all 18 tools)
	httpClient     *http.Client
	restateMu      sync.RWMutex
	restateAliveFn func() bool
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

// SetRestateLiveness registers a liveness probe for an auto-supervised Restate runtime.
func (tb *Toolbox) SetRestateLiveness(aliveFn func() bool) {
	tb.restateMu.Lock()
	tb.restateAliveFn = aliveFn
	tb.restateMu.Unlock()
}

func (tb *Toolbox) isRestateAlive() bool {
	tb.restateMu.RLock()
	fn := tb.restateAliveFn
	tb.restateMu.RUnlock()
	return fn == nil || fn()
}

// ListTools returns the catalog visible to the active tool profile.
func (tb *Toolbox) ListTools() []ToolSpec {
	all := []ToolSpec{
		// Core 10-tool agent profile (default cognitive-load budget):
		{Name: "remember", Profile: "agent", Description: "Commit a provenance-signed memory fragment into a shard with optional NMU-1 freshness/rights metadata, BRC-369 Borg group encryption, and/or BSV anchoring."},
		{Name: "recall", Profile: "agent", Description: "Recall active, Cedar-authorized, Borg-decrypted fragments. Omit shard to recall across all non-system shards."},
		{Name: "forget", Profile: "agent", Description: "Shred local plaintext/ciphertext of a fragment while preserving its on-chain Merkle commitment."},
		{Name: "groundedAnswer", Profile: "agent", Description: "Verify claims against cited memory fragments (quote + Google Mangle Datalog), strip hallucinations, commit a receipt, and return the nektarr1.* verification footer."},
		{Name: "verifyReceipt", Profile: "agent", Description: "Verify a nektarr1.* receipt token, its §16 delegations, and the SPV settlement status of every cited fragment."},
		{Name: "want", Profile: "agent", Description: "Buy verified knowledge live from peer agents or the Hub over BSV AtomicBEEF + BRC-369 keyed disclosure with NMU-1 grade, maxSatsPer1kUnits, freshness, rights, and mode filters. Set standingTtlSeconds to auto-create a durable StandingOrder."},
		{Name: "watchKnowledge", Profile: "agent", Description: "Create, list, live-steer, or cancel durable 0%-CPU Restate StandingOrders (Limit-Orders for Knowledge) with NMU-1 commodity filters that auto-buy when a matching seller arrives."},
		{Name: "sell", Profile: "agent", Description: "Arm a local memory shard for multi-mode NMU-1 selling (spot, per_query 402, subscription SSE feed), or submit a verified anchored fragment to a Hub knowledge bounty."},
		{Name: "marketContract", Profile: "agent", Description: "Inspect the live NMU-1 Commodity Ticker (sats/triple by ontology, grade, and pricing mode) or manage paid shard feed subscriptions (action: ticker|subscriptions|revokeSubscription)."},
		{Name: "deals", Profile: "agent", Description: "List persisted market deals, pending operator approvals, verified seller reputation, and network liquidity metrics."},
		{Name: "fund", Profile: "agent", Description: "Show wallet funding address, satoshi balance, UTXO count, SpendGuard caps, and optionally request a testnet faucet drip."},
		{Name: "doctor", Profile: "agent", Description: "Run a complete self-diagnosis (SQLite, wallet, hosts, peers, policies, SSE vs poll health, welcome shard) with exact next steps."},
		{Name: "peers", Profile: "agent", Description: "Discover and list signature-verified peers, bfinger handles, sell domains, advertised CHIRP packs, and Hub bounties."},
		// Operator / advanced profile tools:
		{Name: "anchor", Profile: "operator", Description: "Anchor all unanchored fragments in a shard onto BSV via the NektarAnchor Rúnar covenant (guarded by SpendGuard)."},
		{Name: "prove", Profile: "operator", Description: "Build a self-contained SPV ProofBundle (Merkle path + NektarAnchor BEEF/BUMP) and verify it offline against headers."},
		{Name: "borgGroup", Profile: "operator", Description: "Manage Borg access groups and BRC-369 epoch key rotation (create, add, revoke, get)."},
		{Name: "exportPack", Profile: "operator", Description: "Export a shard and its anchor proofs as a verified BRC-167 CHIRP closure."},
		{Name: "importPack", Profile: "operator", Description: "Import and verify a local or remote BRC-167 CHIRP shard pack closure into a local shard."},
		{Name: "repo", Profile: "agent", Description: "Full Git + SingularGrit Memory Repo tool anchored via strict BSV NektarAnchor UTXO lineage (action: list|log|show|tree|read|diff|blame|verify|commit|rm|branch|fork|merge|cherryPick|revert|tag|export|import|prove|rotate)."},
		{Name: "ontology", Profile: "agent", Description: "Manage committed domain TBox ontologies (ont-<domain>), cross-ontology Bridge Shards, bounded JevGraph ABox extraction, and Mangle-gated edge review (action: init|list|show|validate|extract|edges|review|align)."},
		{Name: "lineage", Profile: "agent", Description: "Inspect the verified cryptographic Knowledge DAG (ancestors, descendants, signed refinement/supersedes edges, and modality) for a fragment."},
		{Name: "commitOntology", Profile: "operator", Description: "Compile and commit DL-Lite / OWL 2 RL / deontic axioms into the immutable __ontology__ shard (Memory as Law)."},
		{Name: "provisionClearance", Profile: "operator", Description: "Provision a downward-only CLKA clearance bundle (+ public manifest, optionally ECDH-sealed to a recipient pubkey) for a shard."},
		{Name: "rotateLattice", Profile: "operator", Description: "Rotate a shard's CLKA encryption epoch for forward secrecy (revoking prior-epoch clearances for future writes)."},
		{Name: "compactProve", Profile: "operator", Description: "Build and SPV-verify an O(log N) MMR CompactProofBundle proving a fragment against the shard's head NektarAnchor UTXO."},
		{Name: "consistencyProof", Profile: "operator", Description: "Build an O(log N) MMR consistency proof proving anchor batch fromN is an unaltered prefix of batch toN."},
		{Name: "authorize", Profile: "operator", Description: "Evaluate the active native Cedar policy set for a principal, action, and resource."},
		{Name: "status", Profile: "operator", Description: "Return Nektar v0.1 node status: identity, network, wallet balance, shards, SpendGuard, policy hash, and market deals."},
	}
	if tb.profile == "agent" {
		out := make([]ToolSpec, 0, 14)
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
		return tb.callRememberTool(ctx, rawArgs)
	case "recall":
		return tb.callRecallTool(ctx, rawArgs)
	case "forget":
		return tb.callForgetTool(ctx, rawArgs)
	case "groundedAnswer":
		return tb.callGroundedAnswerTool(ctx, rawArgs)
	case "verifyReceipt":
		return tb.callVerifyReceiptTool(ctx, rawArgs)
	case "want":
		return tb.callWantTool(ctx, rawArgs)
	case "watchKnowledge":
		var in WatchKnowledgeInput
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		return tb.ExecWatchKnowledge(ctx, in)
	case "sell":
		var in SellToolInput
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		return tb.ExecSell(ctx, in)
	case "marketContract":
		var in MarketContractInput
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		return tb.ExecMarketContract(ctx, in)
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
		return tb.callAnchorTool(ctx, rawArgs)
	case "prove":
		return tb.callProveTool(ctx, rawArgs)
	case "borgGroup":
		var in BorgGroupInput
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		return tb.ExecBorgGroupCtx(ctx, in)
	case "exportPack":
		return tb.callExportPackTool(ctx, rawArgs)
	case "importPack":
		return tb.callImportPackTool(ctx, rawArgs)
	case "authorize":
		return tb.callAuthorizeTool(ctx, rawArgs)
	case "status":
		return BuildStatus(ctx, tb.mem, tb.mkt), nil
	case "repo":
		var in RepoToolInput
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		return tb.ExecRepo(ctx, in)
	case "ontology":
		var in OntologyToolInput
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		return tb.ExecOntology(ctx, in)
	case "lineage":
		var in LineageToolInput
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		return tb.ExecLineage(ctx, in)
	case "commitOntology":
		var in CommitOntologyInput
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		return tb.ExecCommitOntology(ctx, in)
	case "provisionClearance":
		var in ProvisionClearanceInput
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		return tb.ExecProvisionClearance(ctx, in)
	case "rotateLattice":
		var in RotateLatticeInput
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		return tb.ExecRotateLattice(ctx, in)
	case "compactProve":
		var in CompactProveInput
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		return tb.ExecCompactProve(ctx, in)
	case "consistencyProof":
		var in ConsistencyProofInput
		if err := json.Unmarshal(rawArgs, &in); err != nil {
			return nil, err
		}
		return tb.ExecConsistencyProof(ctx, in)
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}
