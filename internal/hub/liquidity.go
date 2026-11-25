package hub

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
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
			if res, err := mem.ExportPublicShardPack(ctx, st.Shard); err == nil && res != nil && res.ChirpURL != "" {
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
	// Mandatory SPV / Merkle ProofBundle — never pay unverified text submissions.
	if len(sub.ProofBundle) == 0 {
		return nil, errors.New("bounty: anchored proofBundle is required for payout")
	}
	var pb contracts.ProofBundle
	if err := json.Unmarshal(sub.ProofBundle, &pb); err != nil {
		return nil, fmt.Errorf("bounty: invalid proofBundle JSON: %w", err)
	}
	if err := verifyBountyProofBundle(ctx, mem, pb, sub); err != nil {
		return nil, fmt.Errorf("bounty: proofBundle verification failed: %w", err)
	}

	// Persist acquired verified knowledge into Hub's curated/<domain> shard BEFORE broadcasting payout
	// (AutoAnchor: false so anchor/UTXO errors or HTTP client cancel NEVER happen after SendToAddress and trigger CancelBountyClaim!).
	curatedShard := "curated/" + b.Domain
	rem, err := mem.Remember(ctx, memory.RememberInput{
		Shard:      curatedShard,
		Text:       sub.Text,
		Source:     "bounty:" + shortID(sub.SellerPub),
		Class:      b.Domain,
		AutoAnchor: false,
	})
	if err != nil {
		return nil, fmt.Errorf("bounty: commit acquired fragment: %w", err)
	}

	// Execute SpendGuard-enforced payout directly (use detached context timeout so client disconnect mid-send cannot mask broadcast).
	payCtx, payCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer payCancel()
	_, txid, err := mem.Chain().SendToAddress(payCtx, sub.SellerAddress, b.RewardSats)
	if err != nil {
		return nil, fmt.Errorf("bounty: payout failed: %w", err)
	}

	// Sustainable Hub market-maker pricing: default 120% of bounty reward (or NEKTAR_HUB_RESALE_SATS override).
	resalePrice := maxUint64((b.RewardSats*12)/10, 120)
	if raw := strings.TrimSpace(os.Getenv("NEKTAR_HUB_RESALE_SATS")); raw != "" {
		if v, pErr := strconv.ParseUint(raw, 10, 64); pErr == nil && v > 0 {
			resalePrice = v
		}
	}
	// Ensure curated/<domain> is also armed for resale to future buyers!
	_ = mem.DB().SaveSellListing(store.SellListing{
		Shard:       curatedShard,
		Domains:     []string{b.Domain},
		PriceSats:   resalePrice,
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

// verifyBountyProofBundle performs full offline SPV verification of AnchorBEEF + Rúnar NektarAnchor
// + ChainTail + header BUMP root + mandatory Provenance & owner-authorship signature before any bounty payout.
func verifyBountyProofBundle(ctx context.Context, mem *memory.Service, pb contracts.ProofBundle, sub BountySubmission) error {
	if strings.TrimSpace(sub.HDP) == "" {
		return errors.New("missing submission hdp")
	}
	hdpBytes, err := hex.DecodeString(strings.TrimSpace(sub.HDP))
	if err != nil || len(hdpBytes) != 32 {
		return errors.New("invalid submission hdp hex")
	}
	canonicalHDP := hex.EncodeToString(hdpBytes)

	// Mandatory provenanceHex: HDP must cryptographically bind sub.Text + serialized Provenance.
	if strings.TrimSpace(sub.ProvenanceHex) == "" {
		return errors.New("missing submission provenanceHex")
	}
	provRaw, pErr := hex.DecodeString(strings.TrimSpace(sub.ProvenanceHex))
	if pErr != nil || len(provRaw) == 0 {
		return errors.New("invalid submission provenanceHex")
	}
	prov, parseErr := core.ParseProvenance(provRaw)
	if parseErr != nil {
		return fmt.Errorf("invalid provenance framing: %w", parseErr)
	}
	expectedHDPV2 := core.ComputeHDPV2([]byte(sub.Text), prov)
	expectedHDPV1 := core.SHA256(core.Concat(core.TagProv, []byte(sub.Text), provRaw))
	if !core.EqualBytes(expectedHDPV2, hdpBytes) && !core.EqualBytes(expectedHDPV1, hdpBytes) {
		return errors.New("submission text/provenance does not match hdp")
	}

	// Verify authorship binding: Provenance signature AND BountySubmission OwnerSig must both verify
	// under the same ProvenancePub so third parties cannot replay someone else's public ProofBundle.
	if strings.TrimSpace(sub.ProvenancePub) == "" || strings.TrimSpace(sub.OwnerSig) == "" {
		return errors.New("missing provenancePub or ownerSig authorship proof")
	}
	provPubBytes, err := hex.DecodeString(strings.TrimSpace(sub.ProvenancePub))
	if err != nil {
		return errors.New("invalid provenancePub hex")
	}
	provPub, err := ec.ParsePubKey(provPubBytes)
	if err != nil {
		return fmt.Errorf("invalid provenancePub: %w", err)
	}
	provSig, err := ec.ParseDERSignature(prov.Signature)
	if err != nil {
		return fmt.Errorf("invalid provenance signature: %w", err)
	}
	if !provSig.Verify(core.ProvenanceDigest([]byte(sub.Text), prov), provPub) {
		return errors.New("provenance signature does not verify against provenancePub")
	}
	ownerSigBytes, err := hex.DecodeString(strings.TrimSpace(sub.OwnerSig))
	if err != nil {
		return errors.New("invalid ownerSig hex")
	}
	ownerSig, err := ec.ParseDERSignature(ownerSigBytes)
	if err != nil {
		return fmt.Errorf("invalid ownerSig: %w", err)
	}
	signableRaw, err := core.CanonicalJSON(sub.Signable(canonicalHDP))
	if err != nil {
		return err
	}
	if !ownerSig.Verify(core.SHA256(signableRaw), provPub) {
		return errors.New("ownerSig does not verify against provenancePub")
	}

	expectedLeaf := core.ToHex(core.ComputeLeaf(core.Fragment{HDP: hdpBytes}))
	if !strings.EqualFold(pb.Leaf, expectedLeaf) {
		return errors.New("proofBundle leaf does not commit to submission hdp")
	}
	if len(pb.AnchorBEEF) == 0 {
		return errors.New("proofBundle missing anchorBEEF")
	}
	vr := contracts.VerifyFragmentCtx(ctx, pb, mem.Chain().Tracker())
	if !vr.OK {
		// In multi-node "memory" mode tests, each node runs an isolated in-memory testchain with
		// independent coinbase block headers; still verify every Rúnar/Merkle/ChainTail/BUMP commitment!
		if mem.Chain().Network() == "memory" && vr.Reason == "BEEFInvalid" && strings.Contains(vr.Detail, "BUMP root not valid in header chain") {
			vr = contracts.VerifyFragmentStructural(pb)
		}
	}
	if !vr.OK {
		return fmt.Errorf("%s: %s", vr.Reason, vr.Detail)
	}
	// On live networks require mined BUMP confirmation verified against PoW headers (prevent unmined fake-anchor drain).
	if mem.Chain().Network() != "memory" && !vr.Settled {
		return errors.New("proofBundle anchor must be mined and SPV-verified against block headers on live networks")
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
	globalCap := uint64(50_000)
	if raw := strings.TrimSpace(os.Getenv("NEKTAR_FAUCET_DAILY_CAP")); raw != "" {
		if v, pErr := strconv.ParseUint(raw, 10, 64); pErr == nil && v > 0 {
			globalCap = v
		}
	}
	// Atomically check AND reserve both per-address and global daily caps BEFORE broadcasting!
	if err := mem.DB().ReserveFaucetDrip(address, dripSats, perAddressCap, globalCap); err != nil {
		if errors.Is(err, store.ErrFaucetCapExceeded) {
			return "", 0, fmt.Errorf("faucet: daily cap reached (per-address %d / global %d sats) — try again tomorrow", perAddressCap, globalCap)
		}
		return "", 0, err
	}
	_, txid, err = mem.Chain().SendToAddress(ctx, address, dripSats)
	if err != nil {
		if !errors.Is(err, nwallet.ErrBroadcastUnknown) {
			_ = mem.DB().RefundFaucetDrip(address, dripSats)
		}
		return "", 0, err
	}
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
