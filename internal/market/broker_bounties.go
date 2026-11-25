package market

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

// SellToHub submits an SPV-anchored local fragment directly to a Hub's knowledge bounty pool and receives a real BSV payout!
func (b *Broker) SellToHub(ctx context.Context, hubURL, bountyID, shard string, seq int64) (*hub.BountyPayoutResult, error) {
	if b.engine != nil && b.engine.Policy() != nil {
		kill, _, _, _, _, _, sellerOK := b.engine.Policy().Snapshot()
		if kill {
			return nil, errors.New("market: operator kill switch is engaged")
		}
		if !sellerOK && b.mem.Chain().Network() != "memory" {
			return nil, errors.New("market: seller policy not approved (approve a seller policy before selling to Hub bounties)")
		}
	}
	if hubURL == "" {
		hosts := b.disc.ActiveHosts()
		if len(hosts) == 0 {
			return nil, errors.New("market: no active hub host configured")
		}
		hubURL = hosts[0]
	}
	netName := b.mem.Chain().Network()
	if _, err := core.ValidateOutboundURL(hubURL, core.SafeHTTPOpts{
		AllowPrivate: netName == "memory" || netName == "regtest" || netName == "",
		RequireHTTPS: netName == "main",
	}); err != nil {
		return nil, fmt.Errorf("market: blocked bounty hubURL: %w", err)
	}
	// Strictly Level 0 + Cedar "disclose" + non-Borg fragments may be sold externally.
	frags, err := b.mem.Recall(ctx, memory.RecallInput{
		Shard:        shard,
		Principal:    policy.Principal{Type: "Agent", ID: "market-seller", Level: 0},
		Action:       "disclose",
		DisallowBorg: true,
	})
	if err != nil {
		return nil, err
	}
	var target *memory.RecalledFragment
	for i := range frags {
		if seq > 0 {
			if frags[i].Seq == seq {
				cp := frags[i]
				target = &cp
				break
			}
			continue
		}
		// When seq <= 0, prefer an already-mined anchored fragment (avoids doomed JIT submission on live nets).
		if target == nil || (target.AnchorState != "mined" && frags[i].AnchorState == "mined") {
			cp := frags[i]
			target = &cp
		}
	}
	if target == nil {
		return nil, fmt.Errorf("market: fragment %s#%d not found or unauthorized for external disclosure", shard, seq)
	}
	if target.AnchorTxID == "" && netName == "memory" && b.mem.Chain().BalanceSats() > 1000 {
		_, _ = b.mem.AnchorShard(ctx, shard)
	}
	pb, vr, pErr := b.mem.Prove(ctx, shard, target.Seq)
	if netName != "memory" {
		if pErr != nil || pb == nil || !vr.OK || !vr.Settled {
			return nil, fmt.Errorf("market: fragment %s#%d (anchor=%s) is not yet mined and SPV-settled (%s: %s) — wait for anchor confirmation before submitting to Hub bounty",
				shard, target.Seq, target.AnchorTxID, vr.Reason, vr.Detail)
		}
	}
	var pbRaw json.RawMessage
	if pb != nil {
		pbRaw, _ = json.Marshal(pb)
	}
	addr, err := b.mem.Chain().FundAddress()
	if err != nil {
		return nil, err
	}
	var provHex string
	if rec, gErr := b.mem.DB().GetFragment(shard, target.Seq); gErr == nil && len(rec.Provenance) > 0 {
		provHex = hex.EncodeToString(rec.Provenance)
	}
	provPub, err := b.mem.Chain().SigningPubKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("market: provenance signing pubkey: %w", err)
	}
	canonicalHDP := strings.ToLower(strings.TrimSpace(target.HDP))
	sub := hub.BountySubmission{
		BountyID:      bountyID,
		SellerPub:     b.PubHex(),
		ProvenancePub: hex.EncodeToString(provPub.Compressed()),
		SellerAddress: addr,
		Shard:         shard,
		Seq:           target.Seq,
		HDP:           canonicalHDP,
		Text:          target.Text,
		ProvenanceHex: provHex,
		ProofBundle:   pbRaw,
		TS:            time.Now().Unix(),
	}
	signable := sub.Signable(canonicalHDP)
	if sig, sErr := bstack.SignArtifact(b.engine.saleKey, signable); sErr == nil {
		sub.Sig = sig
	}
	if canon, cErr := core.CanonicalJSON(signable); cErr == nil {
		if ownerSig, oErr := b.mem.Chain().SignDigest(ctx, core.SHA256(canon)); oErr == nil {
			sub.OwnerSig = hex.EncodeToString(ownerSig)
		}
	}
	body, _ := json.Marshal(sub)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(hubURL, "/")+"/bounties/submit", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("hub bounty submit rejected (HTTP %d)", resp.StatusCode)
	}
	var out hub.BountyPayoutResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if out.Accepted {
		// Verify payout AtomicBEEF (tx bytes, output script & amount, and SPV on live networks) before recording revenue!
		var payoutBEEF []byte
		var confirmHeight int64
		dealStatus := "paid"
		if strings.TrimSpace(out.BEEFHex) == "" {
			if netName != "memory" {
				return nil, fmt.Errorf("market: hub bounty payout %s omitted required AtomicBEEF", out.TxID)
			}
		} else {
			rawBeef, dErr := hex.DecodeString(strings.TrimSpace(out.BEEFHex))
			if dErr != nil {
				return nil, fmt.Errorf("market: decode hub bounty payout beefHex: %w", dErr)
			}
			_, payTx, payHash, pErr := guard.ParseBEEF(rawBeef, guard.DefaultBound)
			if pErr != nil || payTx == nil || payHash == nil {
				return nil, fmt.Errorf("market: parse hub bounty payout BEEF: %v", pErr)
			}
			if out.TxID != "" && !strings.EqualFold(payHash.String(), out.TxID) {
				return nil, fmt.Errorf("market: hub bounty payout txid %s mismatches BEEF txid %s", out.TxID, payHash.String())
			}
			fundScript, fsErr := b.mem.Chain().Signer().FundScript()
			if fsErr != nil {
				return nil, fsErr
			}
			var gotSats uint64
			for _, o := range payTx.Outputs {
				if o.LockingScript != nil && bytes.Equal(*o.LockingScript, *fundScript) {
					gotSats += o.Satoshis
				}
			}
			if gotSats < out.RewardSats {
				return nil, fmt.Errorf("market: hub bounty payout output pays %d sats < promised %d sats", gotSats, out.RewardSats)
			}
			payoutBEEF = rawBeef
			if netName != "memory" {
				ver, vErr := chain.VerifyCounterpartyPaymentBEEF(ctx, rawBeef, b.mem.Chain().Tracker(), chain.PaymentVerifyOpts{
					ExpectedAmountSats: out.RewardSats,
					SettleDepth:        b.mem.Chain().SettleDepth(),
					AllowZeroConf:      true,
				})
				if vErr != nil {
					return nil, fmt.Errorf("market: hub bounty payout SPV verification failed: %w", vErr)
				}
				if ver.SettledAtDepth {
					dealStatus = "settled"
					confirmHeight = int64(ver.BlockHeight)
				}
				if b.mem.Chain().WalletMode() == "embedded" {
					if _, impErr := b.mem.Chain().ImportBEEF(ctx, rawBeef); impErr != nil {
						return nil, fmt.Errorf("market: import verified hub bounty payout BEEF: %w", impErr)
					}
				}
			} else {
				dealStatus = "settled"
				if payTx.MerklePath != nil {
					confirmHeight = int64(payTx.MerklePath.BlockHeight)
				}
			}
		}
		if netName == "memory" {
			dealStatus = "settled"
		}
		_ = b.mem.DB().SaveDeal(store.DealRecord{
			DealID:          "bounty-" + bountyID + "-" + shortPub(out.TxID),
			Role:            "seller",
			Status:          dealStatus,
			PaymentTxID:     out.TxID,
			PaymentBEEF:     payoutBEEF,
			DeliveredText:   target.Text,
			CounterpartyPub: "hub:" + hubURL,
			PriceSats:       out.RewardSats,
			IsHubSeller:     true,
			ConfirmHeight:   confirmHeight,
		})
	}
	return &out, nil
}

// LiquidityMetrics computes the platform-strategy scorecard across persisted deals & active peers!
func (b *Broker) LiquidityMetrics() map[string]any {
	deals, _ := b.mem.DB().ListDeals(200)
	var totalBuyer, settledBuyer, zeroOffers, hubFills int
	var offerTimes []int64
	for _, d := range deals {
		if d.Role != "buyer" {
			continue
		}
		totalBuyer++
		if d.Status == "settled" {
			settledBuyer++
			if d.IsHubSeller {
				hubFills++
			}
		}
		if d.Status == "expired" && d.FirstOfferMs == 0 {
			zeroOffers++
		}
		if d.FirstOfferMs > 0 {
			offerTimes = append(offerTimes, d.FirstOfferMs)
		}
	}
	var fillRate, zeroRate, hubShare float64
	if totalBuyer > 0 {
		fillRate = float64(settledBuyer) / float64(totalBuyer)
		zeroRate = float64(zeroOffers) / float64(totalBuyer)
	}
	if settledBuyer > 0 {
		hubShare = float64(hubFills) / float64(settledBuyer)
	}
	var medianMs int64
	if len(offerTimes) > 0 {
		sort.Slice(offerTimes, func(i, j int) bool { return offerTimes[i] < offerTimes[j] })
		medianMs = offerTimes[len(offerTimes)/2]
	}
	peers := b.disc.Peers()
	organicSellers := 0
	for _, p := range peers {
		if !p.IsHub && len(p.SellDomains) > 0 {
			organicSellers++
		}
	}
	return map[string]any{
		"fillRate":            fillRate,
		"medianTimeToOfferMs": medianMs,
		"zeroRate":            zeroRate,
		"hubSupplyShare":      hubShare,
		"organicSellers":      organicSellers,
		"activePeers":         len(peers),
		"sseWakes":            b.sseWakes.Load(),
		"pollWakes":           b.pollWakes.Load(),
		"bboxWakes":           b.bboxWakes.Load(),
	}
}
