package hub

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

// LiquidityConfig configures the Hub's cold-start liquidity engine:
// curated starter shards, starter CHIRP packs, armed Hub seller listings,
// and Hub knowledge bounties that pay organic agents real sats for verified facts.
type LiquidityConfig struct {
	SeedStarterShards bool
	ArmBounties       bool
	StarterPriceSats  uint64 // default 50
	BountyRewardSats  uint64 // default 100
	FaucetDripSats    uint64 // default 2000
	FaucetDailyCap    uint64 // default 10000
}

// LiquidityState summarizes what ArmHubLiquidity seeded and armed on startup.
type LiquidityState struct {
	Packs         []bstack.PackRef `json:"packs"`
	SellDomains   []string         `json:"sellDomains"`
	BountyDomains []string         `json:"bountyDomains"`
	Bounties      []Bounty         `json:"bounties"`
}

// ArmHubLiquidity boots a cold-start Hub with curated anchored starter knowledge,
// content-addressed CHIRP packs, active SellListings, and a verified Bounty Buyer handler
// so any agent joining the Hub immediately finds real offers AND can sell verified facts TO the Hub.
func ArmHubLiquidity(ctx context.Context, node *Node, mem *memory.Service, cfg LiquidityConfig) (*LiquidityState, error) {
	if node == nil || mem == nil {
		return nil, errors.New("hub: node and memory service required for liquidity engine")
	}
	if cfg.StarterPriceSats == 0 {
		cfg.StarterPriceSats = 50
	}
	if cfg.BountyRewardSats == 0 {
		cfg.BountyRewardSats = 100
	}
	state := &LiquidityState{}

	// 1. Seed curated starter shards & export CHIRP packs for Layer-2 cold-start sync.
	if cfg.SeedStarterShards {
		starters := []struct {
			Shard   string
			Domains []string
			Facts   []string
		}{
			{
				Shard:   "ops/nektar-guide",
				Domains: []string{"ops", "nektar"},
				Facts: []string{
					"Nektar v0.4 compiles into a single zero-CGO Go binary serving MCP, REST, A2A, Overlay Hub, and embedded Svelte dashboard on port 18420.",
					"Every Nektar receipt carries a nektarr1.* token verified against anchored Merkle roots, AWS Cedar policies, and Google Mangle Datalog grounding.",
				},
			},
			{
				Shard:   "ops/bsv-spv",
				Domains: []string{"bsv", "ops"},
				Facts: []string{
					"BSV SPV verification validates transaction Merkle paths against block headers offline without trusting any remote RPC server.",
					"BRC-369 keyed paid disclosure binds sha256(k) into a DisclosureSale covenant so a buyer verifies the content key before decrypting CHIRP ciphertext.",
				},
			},
		}
		domSeen := make(map[string]bool)
		for _, st := range starters {
			existing, _ := mem.DB().ListFragments(st.Shard)
			if len(existing) == 0 {
				for _, txt := range st.Facts {
					_, _ = mem.Remember(ctx, memory.RememberInput{
						Shard:  st.Shard,
						Text:   txt,
						Source: "hub:curated",
						Class:  "guide",
					})
				}
			}
			// Anchor starter shard if wallet has spendable balance.
			if mem.Chain().BalanceSats() > 1000 {
				_, _ = mem.AnchorShard(ctx, st.Shard)
			}
			if res, err := mem.ExportShardPack(ctx, st.Shard); err == nil && res != nil && res.ChirpURL != "" {
				frags, _ := mem.DB().ListFragments(st.Shard)
				state.Packs = append(state.Packs, bstack.PackRef{
					Shard:    st.Shard,
					Domains:  st.Domains,
					ChirpURL: res.ChirpURL,
					RootHex:  res.RootHex,
					Count:    len(frags),
				})
			}
			// Arm starter shard as a low-price Hub fallback SellListing.
			_ = mem.DB().SaveSellListing(store.SellListing{
				Shard:       st.Shard,
				Domains:     st.Domains,
				PriceSats:   cfg.StarterPriceSats,
				Description: "Hub curated starter knowledge (" + st.Shard + ")",
				Active:      true,
			})
			for _, d := range st.Domains {
				if !domSeen[d] {
					domSeen[d] = true
					state.SellDomains = append(state.SellDomains, d)
				}
			}
		}
	}

	// 2. Register default domain bounties & arm the verified Bounty Buyer handler!
	if cfg.ArmBounties {
		defaultBounties := []Bounty{
			{ID: "bounty-ops", Domain: "ops", Question: "Verified DevOps, deployment, or infrastructure runbook facts", RewardSats: cfg.BountyRewardSats, MaxFills: 50, Active: true},
			{ID: "bounty-bsv", Domain: "bsv", Question: "Verified BSV protocol, SPV, Rúnar, or overlay engineering facts", RewardSats: cfg.BountyRewardSats, MaxFills: 50, Active: true},
			{ID: "bounty-agents", Domain: "agents", Question: "Verified MCP, A2A, tool-use, or agent orchestration knowledge", RewardSats: cfg.BountyRewardSats, MaxFills: 50, Active: true},
		}
		for _, b := range defaultBounties {
			if _, err := node.Store().GetBounty(b.ID); err != nil {
				_ = node.Store().UpsertBounty(b)
			}
		}
		node.SetBountyHandler(func(bCtx context.Context, b Bounty, sub BountySubmission) (*BountyPayoutResult, error) {
			return handleVerifiedBounty(bCtx, mem, b, sub)
		})
		bounties, _ := node.Store().ListBounties(true)
		state.Bounties = bounties
		bDomSeen := make(map[string]bool)
		for _, b := range bounties {
			if !bDomSeen[b.Domain] {
				bDomSeen[b.Domain] = true
				state.BountyDomains = append(state.BountyDomains, b.Domain)
			}
		}
	}

	return state, nil
}

// handleVerifiedBounty verifies a contributing agent's ProofBundle + Merkle inclusion, pays their
// BSV address via SpendGuard-enforced SendToAddress, and commits the verified fact into curated/<domain>.
func handleVerifiedBounty(ctx context.Context, mem *memory.Service, b Bounty, sub BountySubmission) (*BountyPayoutResult, error) {
	if strings.TrimSpace(sub.SellerAddress) == "" {
		return nil, errors.New("bounty: sellerAddress required for BSV payout")
	}
	if len(strings.TrimSpace(sub.Text)) < 12 {
		return nil, errors.New("bounty: fragment text too short to qualify")
	}
	// Verify SPV / Merkle ProofBundle structurally (and against headers when available on shared network).
	if len(sub.ProofBundle) > 0 {
		var pb contracts.ProofBundle
		if err := json.Unmarshal(sub.ProofBundle, &pb); err != nil {
			return nil, fmt.Errorf("bounty: invalid proofBundle JSON: %w", err)
		}
		if err := verifyProofBundleStructure(pb); err != nil {
			return nil, fmt.Errorf("bounty: proofBundle verification failed: %w", err)
		}
	}

	// Ensure Hub has spendable coins & execute real SpendGuard-enforced payout!
	_ = mem.Chain().EnsureSplitCoins(ctx, 4, maxUint64(b.RewardSats*2, 1000))
	_, txid, err := mem.Chain().SendToAddress(ctx, sub.SellerAddress, b.RewardSats)
	if err != nil {
		return nil, fmt.Errorf("bounty: payout failed: %w", err)
	}

	// Store acquired verified knowledge into Hub's curated/<domain> shard so other agents can discover & buy it!
	curatedShard := "curated/" + b.Domain
	rem, err := mem.Remember(ctx, memory.RememberInput{
		Shard:      curatedShard,
		Text:       sub.Text,
		Source:     "bounty:" + shortID(sub.SellerPub),
		Class:      b.Domain,
		AutoAnchor: mem.Chain().BalanceSats() > 1000,
	})
	if err != nil {
		return nil, fmt.Errorf("bounty: commit acquired fragment: %w", err)
	}
	// Ensure curated/<domain> is also armed for resale to future buyers!
	_ = mem.DB().SaveSellListing(store.SellListing{
		Shard:       curatedShard,
		Domains:     []string{b.Domain},
		PriceSats:   maxUint64(b.RewardSats/2, 25),
		Description: "Hub bounty-acquired verified knowledge (" + b.Domain + ")",
		Active:      true,
	})
	return &BountyPayoutResult{
		Accepted:   true,
		BountyID:   b.ID,
		RewardSats: b.RewardSats,
		TxID:       txid,
		Shard:      curatedShard,
		Seq:        rem.Seq,
	}, nil
}

// verifyProofBundleStructure checks offline Merkle leaf inclusion + Rúnar NektarAnchor covenant state.
func verifyProofBundleStructure(pb contracts.ProofBundle) error {
	if pb.Leaf == "" || pb.Anchor.R == "" {
		return errors.New("missing leaf or anchor root")
	}
	leafBytes, err := hex.DecodeString(pb.Leaf)
	if err != nil {
		return err
	}
	rootBytes, err := hex.DecodeString(pb.Anchor.R)
	if err != nil {
		return err
	}
	siblings := make([][]byte, len(pb.Path))
	for i, s := range pb.Path {
		b, err := hex.DecodeString(s)
		if err != nil {
			return err
		}
		siblings[i] = b
	}
	if !core.VerifyProof(leafBytes, &core.MerkleProof{Index: pb.LeafIndex, Siblings: siblings}, rootBytes) {
		return errors.New("merkle inclusion proof invalid")
	}
	return nil
}

// DripFaucet sends a rate-limited starter testnet/memory drip to address (STRICTLY refused on mainnet!).
func DripFaucet(ctx context.Context, mem *memory.Service, address string, dripSats, perAddressCap uint64) (txid string, sent uint64, err error) {
	if mem.Chain().Mainnet() {
		return "", 0, errors.New("faucet: disabled on mainnet")
	}
	address = strings.TrimSpace(address)
	if address == "" {
		return "", 0, errors.New("faucet: address is required")
	}
	if dripSats == 0 {
		dripSats = 2000
	}
	if perAddressCap == 0 {
		perAddressCap = 10_000
	}
	claim, err := mem.DB().GetFaucetClaim(address)
	if err != nil {
		return "", 0, err
	}
	now := time.Now().Unix()
	if claim.TotalSats+dripSats > perAddressCap && now-claim.LastClaimAt < 86400 {
		return "", 0, fmt.Errorf("faucet: daily cap (%d sats) reached for %s", perAddressCap, address)
	}
	_ = mem.Chain().EnsureSplitCoins(ctx, 4, dripSats*2)
	_, txid, err = mem.Chain().SendToAddress(ctx, address, dripSats)
	if err != nil {
		return "", 0, err
	}
	_ = mem.DB().RecordFaucetClaim(address, dripSats)
	return txid, dripSats, nil
}

func maxUint64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func shortID(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}
