package market

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

// ArmSellResult embeds store.SellListing with additive readiness diagnostics so callers and UIs
// immediately see whether autoQuoteWant will quote incoming WantAds or why quoting is blocked.
type ArmSellResult struct {
	store.SellListing
	QuotingReady   bool     `json:"quotingReady"`
	BlockedReasons []string `json:"blockedReasons,omitempty"`
}

// ArmSell persists a SellListing, anchors the shard if spendable coins exist, and re-announces on `dir`.
func (b *Broker) ArmSell(ctx context.Context, in SellInput) (ArmSellResult, error) {
	if strings.TrimSpace(in.Shard) == "" {
		return ArmSellResult{}, errors.New("market: shard is required")
	}
	if len(in.Domains) == 0 {
		in.Domains = []string{"general"}
	}
	if in.PriceSats == 0 {
		in.PriceSats = 100
	}
	norm := make([]string, 0, len(in.Domains))
	for _, d := range in.Domains {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			norm = append(norm, d)
		}
	}
	norm = ExpandDomainsWithCorrelations(ctx, b.Matcher(), norm, in.Description, b.ActiveNetworkTopics())
	listing := store.SellListing{
		Shard:       strings.TrimSpace(in.Shard),
		Domains:     norm,
		PriceSats:   in.PriceSats,
		Description: in.Description,
		Active:      true,
		CreatedAt:   time.Now().Unix(),
	}
	if err := b.mem.DB().SaveSellListing(listing); err != nil {
		return ArmSellResult{}, err
	}
	if b.mem.Chain().BalanceSats() > 1000 {
		_, _ = b.mem.AnchorShard(ctx, listing.Shard)
	}
	_, _ = b.AnnounceSelf(ctx)

	var blocked []string
	if b.engine != nil && b.engine.Policy() != nil {
		_ = SyncPolicyFromDB(b.mem.DB(), b.engine.Policy())
		kill, _, _, _, _, _, sellerOK := b.engine.Policy().Snapshot()
		if kill {
			blocked = append(blocked, "kill_switch_engaged")
		}
		if !b.cfg.IsHub && !sellerOK {
			blocked = append(blocked, "seller_policy_needed")
		}
	}
	if b.mem != nil && b.mem.Chain() != nil && b.mem.Chain().Network() != "memory" {
		if b.mem.Chain().BalanceSats() == 0 {
			blocked = append(blocked, "wallet_unfunded")
		}
		hasMined := false
		if frags, rErr := b.mem.Recall(ctx, memory.RecallInput{
			Shard:        listing.Shard,
			Principal:    policy.Principal{Type: "Agent", ID: "market-seller", Level: 0},
			Action:       "disclose",
			DisallowBorg: true,
		}); rErr == nil {
			for _, f := range frags {
				if f.AnchorState == "mined" {
					hasMined = true
					break
				}
			}
		}
		if !hasMined {
			blocked = append(blocked, "anchor_pending_mine")
		}
	}

	res := ArmSellResult{
		SellListing:    listing,
		QuotingReady:   len(blocked) == 0,
		BlockedReasons: blocked,
	}
	b.notify("sell_armed", res)
	return res, nil
}

// AnnounceSelf publishes this node's signed AnnounceAd to the enumerable `dir` lane across active hubs.
func (b *Broker) AnnounceSelf(ctx context.Context) (int, error) {
	listings, _ := b.mem.DB().ListSellListings()
	domSeen := make(map[string]bool)
	var sellDoms []string
	for _, l := range listings {
		if !l.Active {
			continue
		}
		for _, d := range l.Domains {
			if !domSeen[d] {
				domSeen[d] = true
				sellDoms = append(sellDoms, d)
			}
		}
	}
	b.mu.RLock()
	pubURL := b.cfg.PublicURL
	office := b.cfg.BboxOffice
	handle := b.cfg.Handle
	isHub := b.cfg.IsHub
	packs := append([]bstack.PackRef(nil), b.packs...)
	bountyDoms := append([]string(nil), b.bountyDoms...)
	b.mu.RUnlock()

	var bondOutpoint, bondBEEF string
	if bRec, _, bErr := LoadActiveBond(b.mem.DB()); bErr == nil && bRec != nil && !bRec.State.Seized {
		bondOutpoint = bRec.Outpoint()
		if len(bRec.BEEFHex) <= 8192 {
			bondBEEF = bRec.BEEFHex
		}
	}

	var hubRole, hubPayKey, hubTermsURL string
	var hubFees *bstack.HubFeeAdvert
	if b.cfg.Hub != nil {
		isHub = true
		fs := b.cfg.Hub.FeeSchedule()
		hubRole = string(fs.Role)
		hubPayKey = b.cfg.Hub.PayeeIdentityKey()
		hubFees = fs.AdvertSummary()
		if pubURL != "" {
			base := strings.TrimRight(pubURL, "/")
			if strings.HasSuffix(base, "/hub") {
				hubTermsURL = base + "/v1/terms"
			} else {
				hubTermsURL = base + "/hub/v1/terms"
			}
		}
	}

	return b.disc.PublishAnnounce(ctx, bstack.AnnounceAd{
		Handle:            handle,
		PublicURL:         pubURL,
		BboxOffice:        office,
		Domains:           sellDoms,
		SellDomains:       sellDoms,
		BountyDomains:     bountyDoms,
		Packs:             packs,
		BondOutpoint:      bondOutpoint,
		BondBEEFHex:       bondBEEF,
		IsHub:             isHub,
		HubRole:           hubRole,
		HubPayIdentityKey: hubPayKey,
		HubTermsURL:       hubTermsURL,
		HubFees:           hubFees,
	})
}
