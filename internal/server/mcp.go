package server

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
)

// NewMCPServer constructs the official modelcontextprotocol/go-sdk MCP server backed by the unified Toolbox.
func NewMCPServer(mem *memory.Service, mkt *market.Engine) *mcp.Server {
	return NewMCPServerWithToolbox(NewToolbox(mem, mkt, nil, nil, nil, "operator"))
}

// NewMCPServerWithToolbox binds every tool in the configured profile (`agent` = focused 10 tools; `operator` = all 18).
func NewMCPServerWithToolbox(tb *Toolbox) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "nektar",
		Version: "0.4.0",
	}, &mcp.ServerOptions{
		Instructions: "When answering factual questions from memory: (1) call recall first (omit shard to discover shards including 'welcome'); (2) route verifiable answers through groundedAnswer citing verified fragments; (3) preserve the returned receipt: and verify: footer verbatim; (4) never fabricate receipts — proofs show recorded + unaltered + derivable, never truth.",
	})

	srv.AddResource(&mcp.Resource{
		URI:         "nektar://welcome",
		Name:        "Nektar Welcome Memory",
		Description: "Seeded starter memory fragments and usage instructions.",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		res, err := tb.ExecRecall(ctx, memory.RecallInput{Shard: "welcome", Limit: 20})
		if err != nil {
			return nil, err
		}
		b, _ := json.MarshalIndent(res, "", "  ")
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     string(b),
			}},
		}, nil
	})

	// --- Core 10-tool Agent Profile ---
	type mcpRememberArgs struct {
		Shard      string            `json:"shard,omitempty"`
		Text       string            `json:"text,omitempty"`
		Data       string            `json:"data,omitempty"`
		Source     string            `json:"source,omitempty"`
		Level      int               `json:"level,omitempty"`
		Class      string            `json:"class,omitempty"`
		BorgGroup  string            `json:"borgGroup,omitempty"`
		CallerPub  string            `json:"callerPub,omitempty"`
		Supersedes int64             `json:"supersedes,omitempty"`
		AutoAnchor bool              `json:"autoAnchor,omitempty"`
		Extra      map[string]string `json:"extra,omitempty"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remember",
		Description: "Commit a provenance-signed memory fragment into a shard, optionally BRC-369 encrypting to a Borg group and/or anchoring on BSV.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, raw mcpRememberArgs) (*mcp.CallToolResult, any, error) {
		txt := raw.Text
		if txt == "" {
			txt = raw.Data
		}
		res, err := tb.mem.Remember(ctx, memory.RememberInput{
			Shard:      raw.Shard,
			Text:       txt,
			Source:     raw.Source,
			Level:      raw.Level,
			Class:      raw.Class,
			BorgGroup:  raw.BorgGroup,
			CallerPub:  raw.CallerPub,
			Supersedes: raw.Supersedes,
			AutoAnchor: raw.AutoAnchor,
			Extra:      raw.Extra,
		})
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "recall",
		Description: "Recall active, Cedar-authorized, Borg-decrypted fragments. Omit shard to recall across all non-system shards.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in memory.RecallInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecRecall(ctx, in)
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
		if err := tb.mem.Forget(ctx, in.Shard, in.Seq); err != nil {
			return nil, nil, err
		}
		return jsonToolResult(map[string]any{"ok": true, "shard": in.Shard, "seq": in.Seq})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "groundedAnswer",
		Description: "Verify claims against cited memory fragments (quote + Google Mangle Datalog), strip hallucinations, commit a receipt, and return the nektarr1.* verification footer.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in memory.GroundedAnswerInput) (*mcp.CallToolResult, any, error) {
		res, err := tb.mem.GroundedAnswer(ctx, in)
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
		res, err := tb.mem.VerifyReceipt(ctx, in.Token)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	type wantArgs struct {
		Domains     []string `json:"domains,omitempty"`
		Question    string   `json:"question"`
		MaxSats     uint64   `json:"maxSats,omitempty"`
		TargetShard string   `json:"targetShard,omitempty"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "want",
		Description: "Buy verified knowledge live from peer agents or the Hub over BSV AtomicBEEF + BRC-369 keyed disclosure.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in wantArgs) (*mcp.CallToolResult, any, error) {
		res, err := tb.ExecWant(ctx, market.WantRequest{
			Domains:     in.Domains,
			Question:    in.Question,
			MaxSats:     in.MaxSats,
			TargetShard: in.TargetShard,
		})
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

	// --- Operator Profile Tools ---
	if tb.Profile() != "agent" {
		type peersArgs struct {
			Refresh bool `json:"refresh,omitempty"`
		}
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "peers",
			Description: "Discover signature-verified peers, handles, sell domains, CHIRP packs, and active Hub bounties.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in peersArgs) (*mcp.CallToolResult, any, error) {
			return jsonToolResult(tb.ExecPeers(ctx, in.Refresh))
		})

		type anchorArgs struct {
			Shard string `json:"shard"`
		}
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "anchor",
			Description: "Anchor all unanchored fragments in a shard onto BSV via the NektarAnchor Rúnar covenant (guarded by SpendGuard).",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in anchorArgs) (*mcp.CallToolResult, any, error) {
			a, err := tb.mem.AnchorShard(ctx, in.Shard)
			if err != nil {
				return nil, nil, err
			}
			if a == nil {
				return jsonToolResult(map[string]any{"ok": true, "anchored": false, "reason": "nothing unanchored"})
			}
			return jsonToolResult(a)
		})

		type proveArgs struct {
			Shard string `json:"shard"`
			Seq   int64  `json:"seq"`
		}
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "prove",
			Description: "Build a self-contained SPV ProofBundle (Merkle path + NektarAnchor BEEF/BUMP) and verify it offline against headers.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in proveArgs) (*mcp.CallToolResult, any, error) {
			bundle, vr, err := tb.mem.Prove(ctx, in.Shard, in.Seq)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(map[string]any{"bundle": bundle, "verify": vr})
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "borgGroup",
			Description: "Manage Borg access groups and BRC-369 epoch key rotation (create, add, revoke, get).",
		}, func(_ context.Context, _ *mcp.CallToolRequest, in BorgGroupInput) (*mcp.CallToolResult, any, error) {
			res, err := tb.ExecBorgGroup(in)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

		type packArgs struct {
			Shard       string `json:"shard"`
			ChirpURL    string `json:"chirpUrl,omitempty"`
			TargetShard string `json:"targetShard,omitempty"`
			PeerURL     string `json:"peerUrl,omitempty"`
		}
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "exportPack",
			Description: "Export a shard and its anchor proofs as a verified BRC-167 CHIRP closure.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in packArgs) (*mcp.CallToolResult, any, error) {
			res, err := tb.mem.ExportShardPack(ctx, in.Shard)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "importPack",
			Description: "Import and verify a local or remote BRC-167 CHIRP shard pack closure into a local shard.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in packArgs) (*mcp.CallToolResult, any, error) {
			if in.PeerURL != "" && tb.disc != nil {
				raw, err := tb.disc.FetchRemoteChirp(ctx, in.PeerURL, in.ChirpURL)
				if err != nil {
					return nil, nil, err
				}
				n, sh, err := tb.mem.ImportShardPackBytes(raw, in.TargetShard)
				if err != nil {
					return nil, nil, err
				}
				return jsonToolResult(map[string]any{"imported": n, "shard": sh})
			}
			n, err := tb.mem.ImportShardPack(ctx, in.ChirpURL, in.TargetShard)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(map[string]any{"imported": n, "shard": in.TargetShard})
		})

		type authArgs struct {
			Principal policy.Principal `json:"principal"`
			Action    string           `json:"action"`
			Resource  policy.Resource  `json:"resource"`
		}
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "authorize",
			Description: "Evaluate the active native Cedar policy set for a principal, action, and resource.",
		}, func(_ context.Context, _ *mcp.CallToolRequest, in authArgs) (*mcp.CallToolResult, any, error) {
			pc := tb.mem.PolicyCommit()
			dec, err := policy.Authorize(pc.Text, in.Principal, in.Action, in.Resource)
			if err != nil {
				return nil, nil, err
			}
			rcp := policy.MakeReceipt(in.Principal, in.Action, in.Resource, pc, dec.Allow)
			return jsonToolResult(map[string]any{"decision": dec, "receipt": rcp})
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "status",
			Description: "Return Nektar v0.4 node status: identity, network, wallet balance, shards, SpendGuard, policy hash, and market deals.",
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
