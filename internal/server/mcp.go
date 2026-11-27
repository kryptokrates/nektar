package server

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/reasoning"
)

// NewMCPServer constructs the official modelcontextprotocol/go-sdk MCP server backed by the unified Toolbox.
func NewMCPServer(mem *memory.Service, mkt *market.Engine) *mcp.Server {
	return NewMCPServerWithToolbox(NewToolbox(mem, mkt, nil, nil, nil, "operator"))
}

// NewMCPServerWithToolbox binds every tool in the configured profile (`agent` = focused default; `operator` = all tools),
// plus Git-native Memory Repo MCP Resource Templates and Prompts.
func NewMCPServerWithToolbox(tb *Toolbox) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "nektar",
		Version: "0.1.0",
	}, &mcp.ServerOptions{
		Instructions: "When answering factual questions from memory: (1) call recall first (omit shard/repo to discover repos including 'welcome'; pass branch or atCommit for time-travel); (2) use the unified 'repo' tool (list|log|show|tree|read|diff|blame|verify|commit|rm|branch|fork|merge|cherryPick|revert|tag|export|import|prove|rotate) or nektar://repo/... resources for Git-native UTXO-anchored memory repos; (3) route verifiable answers through groundedAnswer citing verified fragments and preserve the returned receipt: and verify: footer verbatim; (4) never fabricate receipts — proofs show recorded + unaltered + derivable with unbroken NektarAnchor UTXO lineage, never truth.",
	})

	registerMCPResourcesAndPrompts(srv, tb)

	// --- Core Agent Profile Tools (all routed through canonical Toolbox.Exec* methods) ---
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remember",
		Description: "Commit or edit a provenance-signed memory file/fragment in a Memory Repo (supports branch, path, volumetric blobBase64/chirpUrl, BRC-369 Borg encryption, L1-bound TagLeafV2 MPE, and NektarAnchor UTXO anchoring).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, raw RememberToolInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecRemember(ctx, raw)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "recall",
		Description: "Recall active, Cedar-authorized, Borg-decrypted fragments. Omit shard/repo to search across all repos; pass branch or atCommit (SHA/prefix/tag) for epistemic time-travel recall.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in memory.RecallInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecRecall(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "repo",
		Description: "Full Git + SingularGrit Memory Repo tool anchored via strict BSV NektarAnchor UTXO lineage (action: list|log|show|tree|read|diff|blame|verify|commit|rm|branch|fork|merge|cherryPick|revert|tag|export|import|prove|rotate).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RepoToolInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecRepo(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	type forgetArgs struct {
		Shard string `json:"shard"`
		Seq   int64  `json:"seq"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "forget",
		Description: "Shred local plaintext/ciphertext of a fragment while preserving its on-chain Merkle commitment.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in forgetArgs) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecForget(ctx, in.Shard, in.Seq)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "groundedAnswer",
		Description: "Verify claims against cited memory fragments (quote + Google Mangle Datalog), strip hallucinations, commit a receipt, and return the nektarr1.* verification footer.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in memory.GroundedAnswerInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecGroundedAnswer(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	type verifyArgs struct {
		Token string `json:"token"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "verifyReceipt",
		Description: "Verify a nektarr1.* receipt token, its §16 delegations, and the SPV settlement status of every cited fragment.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in verifyArgs) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecVerifyReceipt(ctx, in.Token)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "want",
		Description: "Buy verified knowledge live from peer agents or the Hub over BSV AtomicBEEF + BRC-369 keyed disclosure. Optionally set standingTtlSeconds to fall back to a durable StandingOrder.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in WantToolInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecWantTool(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "watchKnowledge",
		Description: "Create, list, live-steer, or cancel durable Restate StandingOrders (Limit-Orders for Knowledge) that sleep at 0% CPU until a matching offer arrives.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in WatchKnowledgeInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecWatchKnowledge(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sell",
		Description: "Arm a local memory shard for autonomous A2A selling, or submit a verified fragment to a Hub bounty when bountyId is set.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SellToolInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecSell(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deals",
		Description: "Inspect persisted market deals, pending operator approvals, verified seller reputation, and network liquidity metrics.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return jsonToolResult(tb.ExecDeals(ctx))
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fund",
		Description: "Show wallet address, balance, UTXOs, SpendGuard caps, and optionally request a testnet faucet drip (faucet: true).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in FundToolInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecFund(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "doctor",
		Description: "Run complete self-diagnosis (memory, wallet, policies, hosts, peers, SSE health) with exact next steps.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return jsonToolResult(tb.ExecDoctor(ctx))
	})

	type peersArgs struct {
		Refresh bool `json:"refresh,omitempty"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "peers",
		Description: "Discover signature-verified peers, handles, sell domains, CHIRP capsules, and active Hub bounties.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in peersArgs) (*mcp.CallToolResult, any, error) {
		return jsonToolResult(tb.ExecPeers(ctx, in.Refresh))
	})

	// --- Operator Profile Tools ---
	if tb.Profile() != "agent" {
		type anchorArgs struct {
			Shard string `json:"shard"`
		}
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "anchor",
			Description: "Anchor all unanchored fragments in a shard onto BSV via the NektarAnchor Rúnar covenant (guarded by SpendGuard).",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in anchorArgs) (*mcp.CallToolResult, any, error) {
			res, err := tb.ExecAnchor(ctx, in.Shard)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

		type proveArgs struct {
			Shard string `json:"shard"`
			Seq   int64  `json:"seq"`
		}
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "prove",
			Description: "Build a self-contained SPV ProofBundle (Merkle path + NektarAnchor BEEF/BUMP) and verify it offline against headers.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in proveArgs) (*mcp.CallToolResult, any, error) {
			res, err := tb.ExecProve(ctx, in.Shard, in.Seq)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "borgGroup",
			Description: "Manage Borg access groups and BRC-369 epoch key rotation (create, add, revoke, get).",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in BorgGroupInput) (*mcp.CallToolResult, any, error) {
			res, err := tb.ExecBorgGroupCtx(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "exportPack",
			Description: "Export a Memory Repo as a self-verifying .nkt CHIRP Capsule closure.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in PackToolInput) (*mcp.CallToolResult, any, error) {
			res, err := tb.ExecExportPack(ctx, in.Shard)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "importPack",
			Description: "Import and verify a local or remote .nkt CHIRP Capsule into a local Memory Repo.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in PackToolInput) (*mcp.CallToolResult, any, error) {
			res, err := tb.ExecImportPack(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "authorize",
			Description: "Evaluate the active native Cedar policy set for a principal, action, and resource.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in AuthorizeToolInput) (*mcp.CallToolResult, any, error) {
			res, err := tb.ExecAuthorize(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "status",
			Description: "Return Nektar v0.1 node status: identity, network, wallet balance, repos, SpendGuard, policy hash, and market deals.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			st := BuildStatus(ctx, tb.mem, tb.mkt)
			return jsonToolResult(st)
		})
	}

	_ = reasoning.TruthNote
	return srv
}

func jsonToolResult(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(b)},
		},
	}, v, nil
}
