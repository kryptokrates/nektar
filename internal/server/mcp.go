package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
)

// NewMCPServer constructs the official modelcontextprotocol/go-sdk MCP server exposing
// Nektar v0.4 verifiable memory, receipts, Cedar policy, Borg groups, CHIRP packs, and market tools.
func NewMCPServer(mem *memory.Service, mkt *market.Engine) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "nektar",
		Version: "0.4.0",
	}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remember",
		Description: "Commit a provenance-signed memory fragment into a shard, optionally BRC-369 encrypting to a Borg group and/or anchoring on BSV.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in memory.RememberInput) (*mcp.CallToolResult, any, error) {
		res, err := mem.Remember(ctx, in)
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
		if err := mem.Forget(ctx, in.Shard, in.Seq); err != nil {
			return nil, nil, err
		}
		return jsonToolResult(map[string]any{"ok": true, "shard": in.Shard, "seq": in.Seq})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "recall",
		Description: "Recall active, Cedar-authorized, Borg-decrypted fragments from a shard (with optional offline SPV verification).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in memory.RecallInput) (*mcp.CallToolResult, any, error) {
		res, err := mem.Recall(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(map[string]any{"fragments": res, "count": len(res)})
	})

	type anchorArgs struct {
		Shard string `json:"shard"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "anchor",
		Description: "Anchor all unanchored fragments in a shard onto BSV via the NektarAnchor Rúnar covenant (guarded by SpendGuard).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in anchorArgs) (*mcp.CallToolResult, any, error) {
		a, err := mem.AnchorShard(ctx, in.Shard)
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
		bundle, vr, err := mem.Prove(ctx, in.Shard, in.Seq)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(map[string]any{"bundle": bundle, "verify": vr})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "groundedAnswer",
		Description: "Verify claims against cited memory fragments (quote + Google Mangle Datalog), strip hallucinations, commit a receipt, and return the nektarr1.* verification footer.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in memory.GroundedAnswerInput) (*mcp.CallToolResult, any, error) {
		res, err := mem.GroundedAnswer(ctx, in)
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
		res, err := mem.VerifyReceipt(ctx, in.Token)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	type borgArgs struct {
		Op      string `json:"op"` // create | add | revoke | get
		GroupID string `json:"groupId"`
		Name    string `json:"name,omitempty"`
		PubHex  string `json:"pubHex,omitempty"`
		Role    string `json:"role,omitempty"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "borgGroup",
		Description: "Manage Borg access groups and BRC-369 epoch key rotation (create, add, revoke, get).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in borgArgs) (*mcp.CallToolResult, any, error) {
		switch in.Op {
		case "create":
			owner := in.PubHex
			if owner == "" {
				owner = mem.Chain().IdentityPubHex()
			}
			g, err := mem.Groups().CreateGroup(in.GroupID, in.Name, owner)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(g)
		case "add":
			if err := mem.Groups().AddMember(in.GroupID, in.PubHex, in.Role); err != nil {
				return nil, nil, err
			}
			g, _ := mem.Groups().GetGroup(in.GroupID)
			return jsonToolResult(g)
		case "revoke":
			ep, err := mem.Groups().RevokeMember(in.GroupID, in.PubHex)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(map[string]any{"groupId": in.GroupID, "revoked": in.PubHex, "newEpoch": ep})
		default:
			g, ok := mem.Groups().GetGroup(in.GroupID)
			if !ok {
				return nil, nil, fmt.Errorf("unknown group %q", in.GroupID)
			}
			return jsonToolResult(g)
		}
	})

	type packArgs struct {
		Shard       string `json:"shard"`
		ChirpURL    string `json:"chirpUrl,omitempty"`
		TargetShard string `json:"targetShard,omitempty"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "exportPack",
		Description: "Export a shard and its anchor proofs as a verified BRC-167 CHIRP closure.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in packArgs) (*mcp.CallToolResult, any, error) {
		res, err := mem.ExportShardPack(ctx, in.Shard)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "importPack",
		Description: "Import and verify a BRC-167 CHIRP shard pack closure into a local shard.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in packArgs) (*mcp.CallToolResult, any, error) {
		n, err := mem.ImportShardPack(ctx, in.ChirpURL, in.TargetShard)
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
		pc := mem.PolicyCommit()
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
		st := BuildStatus(ctx, mem, mkt)
		return jsonToolResult(st)
	})

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
