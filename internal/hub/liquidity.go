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

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/distill"
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
				Domains: []string{"ops", "nektar", "agents"},
				Facts: []string{
					"Nektar v0.1 compiles into a single zero-CGO Go binary serving MCP, REST, A2A, Overlay Hub, and embedded Svelte dashboard on port 18420.",
					"Every Nektar receipt carries a nektarr1.* token verified against anchored Merkle roots, AWS Cedar policies, and Google Mangle Datalog grounding.",
					"Run `nektar up` to start a background daemon, auto-dock Claude Code, Codex CLI, Hermes Agent, OpenClaw, Cursor, and VS Code, and verify a live nektarr1.* receipt in under 30 seconds.",
					"Use `nektar mcp` in stdio MCP clients — it automatically discovers ~/.nektar/active-daemon.json (Mainnet or Testnet) and shares one SQLite vault across all open IDE windows with zero lock contention.",
				},
			},
			{
				Shard:   "ops/bsv-spv",
				Domains: []string{"bsv", "ops"},
				Facts: []string{
					"BSV SPV verification validates transaction Merkle paths against block headers offline without trusting any remote RPC server.",
					"BRC-369 keyed paid disclosure binds sha256(k) into a DisclosureSale covenant so a buyer verifies the content key before decrypting CHIRP ciphertext.",
					"Nektar v0.1 Mainnet uses dual-witness non-circular PoW headers (WhatsOnChain + Babbage Chaintracks) and official dual-region US/EU BSV Broadcaster endpoints.",
					"Hubs operate in Spine-and-Leaf topology with BRC-105 hybrid micro-fees (5 sats want, 2 sats offer, 2 sats NAT bbox relay, 10 sats per 100 metered egress events).",
				},
			},
			{
				Shard:   "ops/agent-grounding",
				Domains: []string{"agents", "ops"},
				Facts: []string{
					"Grounded agent workflow: always call recall first, route factual claims through groundedAnswer citing [shard:seq], and preserve the returned nektarr1.* receipt and verify URL verbatim.",
					"Claude Code Stop hooks (`nektar hook verify-receipt`) verify nektarr1.* tokens automatically and block once if any receipt fails or drifts; Codex CLI runs the same verifier via top-level `notify`.",
					"Honesty boundary: a verified Nektar receipt proves content was recorded, unaltered, and derivable from anchored records — never that the underlying statement is objectively true.",
				},
			},
		}
		domSeen := make(map[string]bool)
		for _, st := range starters {
			existing, _ := mem.DB().ListFragments(st.Shard)
			if len(existing) == 0 {
				for _, txt := range st.Facts {
					_, _ = mem.Remember(ctx, memory.RememberInput{
						Shard:     st.Shard,
						Text:      txt,
						Source:    "hub:curated",
						Class:     "guide",
						Public:    true,
						Distilled: true,
					})
				}
			}
			// Anchor starter shard if wallet has spendable balance.
			if mem.Chain().BalanceSats() > 1000 {
				_, _ = mem.AnchorShard(ctx, st.Shard)
			}
			isLive := mem.Chain().Network() != "memory"
			hasMined := !isLive || shardHasMinedAnchor(mem, st.Shard)
			// Export free starter CHIRP pack immediately once an anchor BEEF exists (even at 0-conf);
			// RefreshHubLiquidityStock re-exports with the embedded mined BUMP as soon as it mines.
			if hasMined || shardHasAnchorBEEF(mem, st.Shard) {
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
			}
			// Arm starter shard as a low-price Hub fallback SellListing (active only once mined on live networks).
			_ = mem.DB().SaveSellListing(store.SellListing{
				Shard:       st.Shard,
				Domains:     st.Domains,
				PriceSats:   cfg.StarterPriceSats,
				Description: "Hub curated starter knowledge (" + st.Shard + ")",
				Active:      hasMined,
			})
			if hasMined {
				for _, d := range st.Domains {
					if !domSeen[d] {
						domSeen[d] = true
						state.SellDomains = append(state.SellDomains, d)
					}
				}
			}
		}
	}

	// 2. Register default domain bounties (including a Hub-sponsored 0-sat starter bounty) & arm handlers!
	if cfg.ArmBounties {
		defaultBounties := []Bounty{
			{ID: "bounty-ops", Domain: "ops", Question: "Verified DevOps, deployment, or infrastructure runbook facts", RewardSats: cfg.BountyRewardSats, MaxFills: 50, Active: true},
			{ID: "bounty-bsv", Domain: "bsv", Question: "Verified BSV protocol, SPV, Rúnar, or overlay engineering facts", RewardSats: cfg.BountyRewardSats, MaxFills: 50, Active: true},
			{ID: "bounty-agents", Domain: "agents", Question: "Verified MCP, A2A, tool-use, or agent orchestration knowledge", RewardSats: cfg.BountyRewardSats, MaxFills: 50, Active: true},
			{ID: "bounty-starter-sponsored", Domain: "ops", Question: "Zero-sat cold-start sponsored bounty: PII-clean distilled engineering fact (Hub anchors and pays net of anchor fee)", RewardSats: cfg.BountyRewardSats, MaxFills: 50, Sponsored: true, AnchorFeeReserveSats: 5, Active: true},
		}
		for _, b := range defaultBounties {
			if _, err := node.Store().GetBounty(b.ID); err != nil {
				_ = node.Store().UpsertBounty(b)
			}
		}
		node.SetBountyHandler(func(bCtx context.Context, b Bounty, sub BountySubmission) (*BountyPayoutResult, error) {
			return handleVerifiedBounty(bCtx, mem, b, sub)
		})
		node.SetWorkHandler(makeDefaultWorkHandler(mem))
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

// shardHasMinedAnchor returns true iff shard has at least one settled anchor row carrying a verified BUMP BEEF.
func shardHasMinedAnchor(mem *memory.Service, shard string) bool {
	if mem == nil || mem.DB() == nil {
		return false
	}
	anchors, err := mem.DB().ListAnchors(shard)
	if err != nil {
		return false
	}
	for _, a := range anchors {
		if a.Settled && len(a.Beef) > 0 {
			return true
		}
	}
	return false
}

// shardHasAnchorBEEF returns true iff shard has at least one anchor row carrying a non-empty BEEF
// (including 0-conf carried first-party ancestry prior to mining).
func shardHasAnchorBEEF(mem *memory.Service, shard string) bool {
	if mem == nil || mem.DB() == nil {
		return false
	}
	anchors, err := mem.DB().ListAnchors(shard)
	if err != nil {
		return false
	}
	for _, a := range anchors {
		if len(a.Beef) > 0 {
			return true
		}
	}
	return false
}

// RefreshHubLiquidityStock activates any pending starter/curated SellListings whose shard anchors
// have now mined and re-exports public starter CHIRP packs with embedded mined BUMPs.
func RefreshHubLiquidityStock(ctx context.Context, mem *memory.Service) ([]bstack.PackRef, error) {
	if mem == nil || mem.DB() == nil {
		return nil, nil
	}
	listings, err := mem.DB().ListSellListings()
	if err != nil {
		return nil, err
	}
	var packs []bstack.PackRef
	for _, l := range listings {
		if !shardHasMinedAnchor(mem, l.Shard) {
			continue
		}
		if !l.Active {
			l.Active = true
			_ = mem.DB().SaveSellListing(l)
		}
		if strings.HasPrefix(l.Shard, "ops/") {
			if res, eErr := mem.ExportPublicShardPack(ctx, l.Shard); eErr == nil && res != nil && res.ChirpURL != "" {
				frags, _ := mem.DB().ListFragments(l.Shard)
				packs = append(packs, bstack.PackRef{
					Shard:    l.Shard,
					Domains:  l.Domains,
					ChirpURL: res.ChirpURL,
					RootHex:  res.RootHex,
					Count:    len(frags),
				})
			}
		}
	}
	return packs, nil
}

// handleVerifiedBounty verifies a contributing agent's ProofBundle + Merkle inclusion at settleDepth
// (or verifies cryptographic authorship + fail-closed PII scrub on Hub-sponsored 0-sat bounties),
// pays their BSV address via SpendGuard-enforced Port.Send returning full AtomicBEEF, and commits
// the verified fact into curated/<domain>.
func handleVerifiedBounty(ctx context.Context, mem *memory.Service, b Bounty, sub BountySubmission) (*BountyPayoutResult, error) {
	if strings.TrimSpace(sub.SellerAddress) == "" {
		return nil, errors.New("bounty: sellerAddress required for BSV payout")
	}
	if len(strings.TrimSpace(sub.Text)) < 12 {
		return nil, errors.New("bounty: fragment text too short to qualify")
	}
	// Fail-closed secret & PII gate on EVERY Hub bounty submission (never ingest or resell leaked secrets!).
	scrubRes := distill.Scrub(sub.Text)
	if scrubRes.Verdict == distill.ScrubBlocked {
		ruleID := "secret-detected"
		if len(scrubRes.BlockedRules) > 0 {
			ruleID = scrubRes.BlockedRules[0]
		}
		return nil, fmt.Errorf("bounty: rejected by fail-closed secret/PII scrubber (%s)", ruleID)
	}
	if b.Sponsored && scrubRes.Verdict == distill.ScrubDroppedLowSignal {
		return nil, errors.New("bounty: rejected low-signal ephemeral chatter on sponsored bounty")
	}

	commitText := sub.Text
	payoutSats := b.RewardSats
	autoAnchor := false

	if b.Sponsored && len(sub.ProofBundle) == 0 {
		// Track B (Zero-Sat Hub-Sponsored Knowledge Bounty):
		// Verify cryptographic authorship (HDP + ProvenanceSig + OwnerSig), scrub PII, Hub anchors on its UTXO pool,
		// and pays RewardSats minus AnchorFeeReserveSats via AtomicBEEF!
		if err := verifyBountyAuthorship(sub); err != nil {
			return nil, fmt.Errorf("bounty: sponsored authorship verification failed: %w", err)
		}
		if scrubRes.SanitizedText != "" {
			commitText = scrubRes.SanitizedText
		}
		if b.AnchorFeeReserveSats > 0 && payoutSats > b.AnchorFeeReserveSats {
			payoutSats -= b.AnchorFeeReserveSats
		} else if b.AnchorFeeReserveSats > 0 {
			payoutSats = 1
		}
		autoAnchor = mem.Chain().BalanceSats() > 1000
	} else {
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
	}

	// Persist acquired verified knowledge into Hub's curated/<domain> shard BEFORE broadcasting payout
	// (for standard bounties AutoAnchor is false so anchor errors never occur after Send).
	curatedShard := "curated/" + b.Domain
	rem, err := mem.Remember(ctx, memory.RememberInput{
		Shard:      curatedShard,
		Text:       commitText,
		Source:     "bounty:" + shortID(sub.SellerPub),
		Class:      b.Domain,
		Public:     true,
		Distilled:  true,
		AutoAnchor: autoAnchor,
	})
	if err != nil {
		return nil, fmt.Errorf("bounty: commit acquired fragment: %w", err)
	}

	// Execute SpendGuard-enforced payout via Port.Send returning strict AtomicBEEF so the seller can
	// SPV-verify and internalize immediately without a 60s poll + block wait.
	payCtx, payCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer payCancel()
	sendRes, err := mem.Chain().Send(payCtx, nwallet.SendArgs{
		ToAddress:   sub.SellerAddress,
		Satoshis:    payoutSats,
		Description: nwallet.ActionPayHubBounty(b.ID).Description,
	})
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
	// On live networks, keep curated/<domain> listing inactive until its shard anchor is mined (RefreshHubLiquidityStock activates it).
	listingActive := mem.Chain().Network() == "memory" || shardHasMinedAnchor(mem, curatedShard)
	_ = mem.DB().SaveSellListing(store.SellListing{
		Shard:       curatedShard,
		Domains:     []string{b.Domain},
		PriceSats:   resalePrice,
		Description: "Hub bounty-acquired verified knowledge (" + b.Domain + ")",
		Active:      listingActive,
	})
	return &BountyPayoutResult{
		Accepted:   true,
		BountyID:   b.ID,
		RewardSats: payoutSats,
		TxID:       sendRes.TxID,
		BEEFHex:    sendRes.BEEFHex,
		Shard:      curatedShard,
		Seq:        rem.Seq,
	}, nil
}

// verifyBountyAuthorship verifies HDP framing, Provenance signature, and OwnerSig for Hub-sponsored 0-sat bounties.
func verifyBountyAuthorship(sub BountySubmission) error {
	hdpBytes, err := hex.DecodeString(strings.TrimSpace(sub.HDP))
	if err != nil || len(hdpBytes) != 32 {
		return errors.New("invalid submission hdp hex")
	}
	canonicalHDP := hex.EncodeToString(hdpBytes)
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
	if !core.EqualBytes(core.ComputeHDPV2([]byte(sub.Text), prov), hdpBytes) {
		return errors.New("submission text/provenance does not match framed v2 hdp")
	}
	if strings.TrimSpace(sub.ProvenancePub) == "" || strings.TrimSpace(sub.OwnerSig) == "" {
		return errors.New("missing provenancePub or ownerSig authorship proof")
	}
	provPub, _, err := core.ParseStrictCompressedPubKey(strings.TrimSpace(sub.ProvenancePub))
	if err != nil {
		return fmt.Errorf("invalid provenancePub: %w", err)
	}
	provSig, err := core.ParseStrictLowSDER(hex.EncodeToString(prov.Signature))
	if err != nil {
		return fmt.Errorf("invalid provenance signature: %w", err)
	}
	if !provSig.Verify(core.ProvenanceDigest([]byte(sub.Text), prov), provPub) {
		return errors.New("provenance signature does not verify against provenancePub")
	}
	ownerSig, err := core.ParseStrictLowSDER(strings.TrimSpace(sub.OwnerSig))
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
	return nil
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
	if !core.EqualBytes(expectedHDPV2, hdpBytes) {
		return errors.New("submission text/provenance does not match framed v2 hdp")
	}

	// Verify authorship binding: Provenance signature AND BountySubmission OwnerSig must both verify
	// under the same ProvenancePub so third parties cannot replay someone else's public ProofBundle.
	if strings.TrimSpace(sub.ProvenancePub) == "" || strings.TrimSpace(sub.OwnerSig) == "" {
		return errors.New("missing provenancePub or ownerSig authorship proof")
	}
	provPub, _, err := core.ParseStrictCompressedPubKey(strings.TrimSpace(sub.ProvenancePub))
	if err != nil {
		return fmt.Errorf("invalid provenancePub: %w", err)
	}
	provSig, err := core.ParseStrictLowSDER(hex.EncodeToString(prov.Signature))
	if err != nil {
		return fmt.Errorf("invalid provenance signature: %w", err)
	}
	if !provSig.Verify(core.ProvenanceDigest([]byte(sub.Text), prov), provPub) {
		return errors.New("provenance signature does not verify against provenancePub")
	}
	ownerSig, err := core.ParseStrictLowSDER(strings.TrimSpace(sub.OwnerSig))
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

	mpeBytes, mErr := hex.DecodeString(strings.TrimSpace(sub.MPERoot))
	if mErr != nil || len(mpeBytes) != 32 {
		return errors.New("missing or invalid 32-byte submission mpeRoot")
	}
	var contentBytes []byte
	if strings.TrimSpace(sub.ContentSHA256) != "" {
		var cErr error
		contentBytes, cErr = hex.DecodeString(strings.TrimSpace(sub.ContentSHA256))
		if cErr != nil || len(contentBytes) != 32 {
			return errors.New("invalid 32-byte submission contentSha256")
		}
	}
	var commitBytes, transBytes []byte
	if strings.TrimSpace(sub.CommitHash) != "" {
		if cb, err := hex.DecodeString(strings.TrimSpace(sub.CommitHash)); err == nil && len(cb) == 32 {
			commitBytes = cb
		}
	}
	if strings.TrimSpace(sub.TransitionHash) != "" {
		if tb, err := hex.DecodeString(strings.TrimSpace(sub.TransitionHash)); err == nil && len(tb) == 32 {
			transBytes = tb
		}
	}
	var ctBytes []byte
	if strings.TrimSpace(sub.CiphertextHex) != "" {
		if b, err := hex.DecodeString(strings.TrimSpace(sub.CiphertextHex)); err == nil {
			ctBytes = b
		}
	}
	expectedLeaf := core.ToHex(core.ComputeLeaf(core.Fragment{
		HDP:            hdpBytes,
		Ciphertext:     ctBytes,
		MPERoot:        mpeBytes,
		ContentSHA256:  contentBytes,
		CommitHash:     commitBytes,
		TransitionHash: transBytes,
	}))
	if !strings.EqualFold(pb.Leaf, expectedLeaf) {
		return errors.New("proofBundle leaf does not commit to submission hdp + mpeRoot")
	}
	if len(pb.AnchorBEEF) == 0 {
		return errors.New("proofBundle missing anchorBEEF")
	}
	var vr contracts.VerifyResult
	if mem.Chain().Network() != "memory" {
		vr = contracts.VerifyFragmentWithDepth(ctx, pb, mem.Chain().Tracker(), mem.Chain().SettleDepth())
	} else {
		vr = contracts.VerifyFragmentCtx(ctx, pb, mem.Chain().Tracker())
	}
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
	// On live networks require mined BUMP confirmation at settleDepth verified against PoW headers.
	if mem.Chain().Network() != "memory" && !vr.Settled {
		return errors.New("proofBundle anchor must be mined and SPV-verified against block headers on live networks")
	}
	return nil
}

// DripFaucetBEEF sends a rate-limited starter testnet/memory drip to address and returns the full
// AtomicBEEF hex alongside txid so recipients can SPV-verify and internalize immediately at 0-conf.
func DripFaucetBEEF(ctx context.Context, mem *memory.Service, address string, dripSats, perAddressCap uint64) (txid string, beefHex string, sent uint64, err error) {
	if mem.Chain().Mainnet() {
		return "", "", 0, errors.New("faucet: disabled on mainnet")
	}
	address = strings.TrimSpace(address)
	if address == "" {
		return "", "", 0, errors.New("faucet: address is required")
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
			return "", "", 0, fmt.Errorf("faucet: daily cap reached (per-address %d / global %d sats) — try again tomorrow", perAddressCap, globalCap)
		}
		return "", "", 0, err
	}
	sendRes, err := mem.Chain().Send(ctx, nwallet.SendArgs{
		ToAddress:   address,
		Satoshis:    dripSats,
		Description: nwallet.ActionHubFaucetDrip().Description,
	})
	if err != nil {
		if !errors.Is(err, nwallet.ErrBroadcastUnknown) {
			_ = mem.DB().RefundFaucetDrip(address, dripSats)
		}
		return "", "", 0, err
	}
	return sendRes.TxID, sendRes.BEEFHex, dripSats, nil
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
