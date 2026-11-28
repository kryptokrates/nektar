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
		Instructions: "Nektar v0.1 Golden Path: (1) Factual recall: call `recall` first, then route answers through `groundedAnswer` and preserve the `receipt: nektarr1.*` + `verify:` footer verbatim (never fake receipts; proofs show recorded + unaltered + derivable, never objective truth). (2) 1-call Capsule Market: call `buyCapsule` to buy & SPV-import verified capsules (auto-posts a live satoshi bounty if none exists yet), `sellCapsule` to scrub PII + commit + anchor + list & export a `.nkt` capsule in one step, and `importCapsule` to SPV-verify & import shared `.nkt` files/URLs. (3) Discover LightBSV Hubs & handles via `peers`.",
	})

	registerMCPResourcesAndPrompts(srv, tb)

	// --- Core Agent Profile Tools (all routed through canonical Toolbox.Exec* methods) ---
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remember",
		Description: "Commit or edit a provenance-signed memory fragment/file in a Memory Repo (CLKA private-by-default unless public:true, supports branch, path, triples, modality, signed DAG refines edges, volumetric blobBase64/chirpUrl, BRC-369 Borg encryption, unified TagLeaf MPE+Commit+Transition, and NektarAnchor UTXO anchoring).",
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

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ontology",
		Description: "Manage committed domain TBox ontologies (ont-<domain>), cross-ontology Bridge Shards, bounded JevGraph ABox extraction, and Mangle-gated edge review (action: init|list|show|validate|extract|edges|review|align).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in OntologyToolInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecOntology(ctx, in)
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
		Name:        "buyCapsule",
		Description: "1-call capsule purchase across active Hubs: SPV-verifies and imports into your local Memory Repo, or automatically posts a bounded 0%-CPU StandingOrder satoshi bounty if 0 offers match immediately.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in BuyCapsuleInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecBuyCapsule(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sellCapsule",
		Description: "1-call capsule publish & sell: scrubs PII/secrets fail-closed, commits & anchors to Memory Repo, auto-arms seller policy, broadcasts OfferAd to active Hubs, and optionally exports a portable .nkt file.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SellCapsuleInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecSellCapsule(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "importCapsule",
		Description: "1-call SPV-verified import of a portable .nkt Memory Capsule from a local file path, https:// URL, or chirp:// URI, discovering the creator's LightBSV handle & Hub.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ImportCapsuleInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecImportCapsule(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "want",
		Description: "Low-level market buy query over BSV AtomicBEEF + BRC-369 keyed disclosure (prefer `buyCapsule` for 1-call buying + automatic bounty fallback).",
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
		Description: "Arm a local memory shard for multi-mode NMU-1 autonomous selling (spot, per_query 402, subscription SSE feed), or submit a verified fragment to a Hub bounty when bountyId is set.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SellToolInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecSell(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "marketContract",
		Description: "Query the standardized NMU-1 Commodity Ticker (sats/triple grouped by ontology, grade S/A/B/C, and pricing mode) or list/revoke active shard feed subscriptions (action: ticker|subscriptions|revokeSubscription).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in MarketContractInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecMarketContract(ctx, in)
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

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "peers",
		Description: "Discover signature-verified peers, LightBSV bfinger handles, BRC-180 community Hubs, CHIRP capsules, and open satoshi bounties (pass `joinHub` to connect to a discovered Hub).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in PeersToolInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecPeersWithInput(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "lineage",
		Description: "Inspect the verified cryptographic Knowledge DAG (ancestors, descendants, signed refines/supersedes edges, and modality) for a fragment.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in LineageToolInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecLineage(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	// --- Operator Profile Tools ---
	if tb.Profile() != "agent" {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "commitOntology",
			Description: "Compile and commit DL-Lite / OWL 2 RL / deontic axioms into the immutable __ontology__ shard (Memory as Law).",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in CommitOntologyInput) (*mcp.CallToolResult, any, error) {
			res, err := tb.ExecCommitOntology(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "provisionClearance",
			Description: "Provision a downward-only CLKA clearance bundle (+ manifest, optionally ECDH-sealed to a recipient pubkey) for a shard.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in ProvisionClearanceInput) (*mcp.CallToolResult, any, error) {
			res, err := tb.ExecProvisionClearance(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "rotateLattice",
			Description: "Rotate a shard's CLKA encryption epoch for forward secrecy.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in RotateLatticeInput) (*mcp.CallToolResult, any, error) {
			res, err := tb.ExecRotateLattice(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "compactProve",
			Description: "Build and SPV-verify an O(log N) MMR CompactProofBundle proving a fragment directly against the head NektarAnchor UTXO.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in CompactProveInput) (*mcp.CallToolResult, any, error) {
			res, err := tb.ExecCompactProve(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "consistencyProof",
			Description: "Build an O(log N) MMR append-only consistency proof proving anchor batch fromN is an unaltered prefix of batch toN.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in ConsistencyProofInput) (*mcp.CallToolResult, any, error) {
			res, err := tb.ExecConsistencyProof(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

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
