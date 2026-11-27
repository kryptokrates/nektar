package market

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memory"
)

// TestCheckpointBondLifecycleCommitSeizeCloseAndRankingBoostE2E proves Wave 2 NektarCheckpointBond
// end-to-end on real BSV testchain + Chronicle Script VM:
//  1. OpenCheckpointBond locks slashable SLA bond UTXO on-chain.
//  2. CommitActiveBond rolls deadline forward before SLA expiry.
//  3. VerifyOfferBondSats + RankOffersWithRepAndBonds awards deterministic bps boost to genuinely
//     bonded sellers and rejects spoofed/copied bonds belonging to a different PayAddress!
//  4. Delinquent bond past Deadline is permissionlessly slashed via SeizePeerBond.
//  5. Second bond past BondReleaseHeight is cleanly reclaimed via CloseActiveBond.
func TestCheckpointBondLifecycleCommitSeizeCloseAndRankingBoostE2E(t *testing.T) {
	ctx := context.Background()
	sellerMem, _, _ := newTestNodeForEscrow(t, nil)
	port := sellerMem.Chain()
	db := sellerMem.DB()

	// 1. Open a 20,000-sat SLA bond with epochLen=5 blocks, releaseBlocks=15 blocks.
	b0, err := OpenCheckpointBond(ctx, db, port, 20_000, 5, 15, 500)
	if err != nil {
		t.Fatalf("OpenCheckpointBond: %v", err)
	}
	if b0.Satoshis != 20_000 || b0.State.N != 0 || b0.State.Seized {
		t.Fatalf("unexpected initial bond: %+v", b0)
	}

	// 2. Advance 2 blocks (< epochLen 5) and Commit checkpoint 1 -> Deadline rolls forward!
	fundAddr, _ := port.FundAddress()
	if _, err := port.TestChain().Generate(2, fundAddr); err != nil {
		t.Fatalf("Generate(2): %v", err)
	}
	root1 := core.SHA256([]byte("hub-sla-root-epoch-1"))
	b1, err := CommitActiveBond(ctx, db, port, root1)
	if err != nil {
		t.Fatalf("CommitActiveBond: %v", err)
	}
	if b1.State.N != 1 || b1.State.Deadline <= b0.State.Deadline {
		t.Fatalf("expected N=1 and rolled deadline, got %+v (prev deadline=%d)", b1, b0.State.Deadline)
	}

	// 3. SPV Bond Verification + Sybil-resistant Ranking Boost!
	//    VerifyOfferBondSats accepts the real seller's PayAddress, and rejects a Sybil seller copying the BEEF!
	otherKey, _ := ec.NewPrivateKey()
	otherAddr, _ := script.NewAddressFromPublicKey(otherKey.PubKey(), false)
	bondedOffer := bstack.OfferAd{
		OfferID:      "off-bonded",
		Pub:          "seller-bonded",
		Domains:      []string{"ops"},
		PriceSats:    500,
		PayAddress:   fundAddr,
		BondOutpoint: b1.Outpoint(),
		BondBEEFHex:  b1.BEEFHex,
	}
	unbondedOffer := bstack.OfferAd{
		OfferID:    "off-unbonded",
		Pub:        "seller-unbonded",
		Domains:    []string{"ops"},
		PriceSats:  500,
		PayAddress: otherAddr.AddressString,
	}
	spoofedBondOffer := bstack.OfferAd{
		OfferID:      "off-spoofed",
		Pub:          "seller-sybil",
		Domains:      []string{"ops"},
		PriceSats:    500,
		PayAddress:   otherAddr.AddressString, // DIFFERENT PayAddress != Bond.OperatorPKH!
		BondOutpoint: b1.Outpoint(),
		BondBEEFHex:  b1.BEEFHex,
	}

	bondedSats := VerifyOfferBondSats(ctx, bondedOffer, port)
	if bondedSats == 0 {
		t.Fatal("expected VerifyOfferBondSats > 0 for genuine bonded seller")
	}
	if spoofSats := VerifyOfferBondSats(ctx, spoofedBondOffer, port); spoofSats != 0 {
		t.Fatalf("CRITICAL: VerifyOfferBondSats accepted spoofed bond copied by different PayAddress: %d", spoofSats)
	}
	want := bstack.WantAd{WantID: "w-bond-rank", Net: "memory", Domains: []string{"ops"}, MaxSats: 1000}
	ranked := RankOffersWithRepAndBonds(want, []bstack.OfferAd{unbondedOffer, spoofedBondOffer, bondedOffer}, DefaultPolicy(), nil, map[string]uint64{
		bondedOffer.OfferID: bondedSats,
	})
	if len(ranked) != 3 || ranked[0].Offer.OfferID != "off-bonded" {
		t.Fatalf("expected bonded seller ranked #1, got %+v", ranked)
	}
	if ranked[0].ScoreBP <= ranked[1].ScoreBP {
		t.Fatalf("expected bonded score (%d) > unbonded score (%d)", ranked[0].ScoreBP, ranked[1].ScoreBP)
	}

	// 4. Advance past b1.State.Deadline -> Bond becomes expired (VerifyOfferBondSats == 0) AND permissionlessly slashable!
	curH, _ := port.Height(ctx)
	needBlocks := int(b1.State.Deadline - uint64(curH) + 1)
	if _, err := port.TestChain().Generate(needBlocks, fundAddr); err != nil {
		t.Fatalf("Generate(%d): %v", needBlocks, err)
	}
	if expSats := VerifyOfferBondSats(ctx, bondedOffer, port); expSats != 0 {
		t.Fatalf("expected expired slashable bond to earn 0 sats, got %d", expSats)
	}
	slasherKey, _ := ec.NewPrivateKey()
	slasherPKH := core.Hash160(slasherKey.PubKey().Compressed())
	beefRaw, _ := hex.DecodeString(b1.BEEFHex)
	seizeTxID, slashedSats, err := SeizePeerBond(ctx, port, beefRaw, b1.Vout, slasherPKH)
	if err != nil || seizeTxID == "" || slashedSats < 18_000 {
		t.Fatalf("SeizePeerBond failed: txid=%s slashed=%d err=%v", seizeTxID, slashedSats, err)
	}

	// 5. Open a second bond (releaseBlocks=4), advance 4 blocks, and reclaim via operator CloseActiveBond!
	b2, err := OpenCheckpointBond(ctx, db, port, 12_000, 4, 4, 500)
	if err != nil {
		t.Fatalf("OpenCheckpointBond(2): %v", err)
	}
	if _, err := port.TestChain().Generate(4, fundAddr); err != nil {
		t.Fatalf("Generate(4): %v", err)
	}
	closeTxID, reclaimedSats, err := CloseActiveBond(ctx, db, port)
	if err != nil || closeTxID == "" || reclaimedSats != b2.Satoshis-350 {
		t.Fatalf("CloseActiveBond failed: txid=%s sats=%d err=%v", closeTxID, reclaimedSats, err)
	}
	after, _, err := LoadActiveBond(db)
	if err != nil || after != nil {
		t.Fatalf("expected active bond cleared after Close, got %+v err=%v", after, err)
	}
}

// TestSystemShardsDealsAndPoliciesSingletonAnchoringE2E proves Wave 2 __deals__ and __policies__
// reserved system shards:
//  1. External Remember / ValidateShardName(..., false) fail closed on __deals__ and __policies__.
//  2. UserShards() excludes __deals__ and __policies__.
//  3. CommitDealAudit and CommitPolicyAudit anchor multiple batches using the consensus-linearized
//     NektarAnchor.Append() singleton UTXO chain (verified via VerifyAnchorUTXOLineage + Chronicle VM),
//     passing VerifyShardChain and SPV Prove!
func TestSystemShardsDealsAndPoliciesSingletonAnchoringE2E(t *testing.T) {
	ctx := context.Background()
	mem, _, _ := newTestNodeForEscrow(t, nil)

	// 1. Isolation: external callers CANNOT target __deals__ or __policies__ directly!
	for _, sys := range []string{memory.DealsShard, memory.PoliciesShard} {
		if err := memory.ValidateShardName(sys, false); err == nil {
			t.Fatalf("expected ValidateShardName(%q, false) to fail", sys)
		}
		if _, err := mem.Remember(ctx, memory.RememberInput{Shard: sys, Text: "spoofed audit entry"}); err == nil {
			t.Fatalf("expected Remember(%q) to fail", sys)
		}
	}

	// 2. Commit 2 signed OperatorPolicyRecords into __policies__ anchored by NektarAnchor.Append()!
	opKey, _ := ec.NewPrivateKey()
	opPubHex := hex.EncodeToString(opKey.PubKey().Compressed())
	opSign := func(_ context.Context, d []byte) ([]byte, error) {
		sig, err := opKey.Sign(d)
		if err != nil {
			return nil, err
		}
		return sig.Serialize(), nil
	}
	pol1, err := ApprovePolicyPresetSigned(ctx, mem.DB(), nil, "buyer", []string{"ops"}, "op-cold", nil, opPubHex, opSign)
	if err != nil {
		t.Fatalf("ApprovePolicyPresetSigned(1): %v", err)
	}
	fPol1, aPol0, err := CommitPolicyAudit(ctx, mem, pol1, true)
	if err != nil || fPol1 == nil || aPol0 == nil {
		t.Fatalf("CommitPolicyAudit(1): f=%v a=%v err=%v", fPol1, aPol0, err)
	}

	pol2, err := ApprovePolicyPresetSigned(ctx, mem.DB(), nil, "both", []string{"ops", "bsv"}, "op-cold", nil, opPubHex, opSign)
	if err != nil {
		t.Fatalf("ApprovePolicyPresetSigned(2): %v", err)
	}
	fPol2, aPol1, err := CommitPolicyAudit(ctx, mem, pol2, true)
	if err != nil || fPol2 == nil || aPol1 == nil {
		t.Fatalf("CommitPolicyAudit(2): f=%v a=%v err=%v", fPol2, aPol1, err)
	}

	// Verify __policies__ batch 1 spent batch 0 via NektarAnchor.Append() under real Chronicle Script VM!
	_, polTx0, _, _ := guard.ParseBEEF(aPol0.Beef, guard.DefaultBound)
	_, polTx1, _, _ := guard.ParseBEEF(aPol1.Beef, guard.DefaultBound)
	if err := contracts.VerifyAnchorUTXOLineage(polTx0, polTx1); err != nil {
		t.Fatalf("__policies__ singleton NektarAnchor.Append lineage failed: %v", err)
	}

	// 3. Commit 2 settled bilateral DealReceipts into __deals__ anchored by NektarAnchor.Append()!
	dr1 := DealReceipt{
		Version:       1,
		DealID:        "deal-audit-1",
		OfferID:       "off-audit-1",
		BuyerPub:      mem.Chain().IdentityPubHex(),
		SellerPub:     opPubHex,
		PriceSats:     1000,
		PaymentTxID:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		KeyCommitment: hex.EncodeToString(core.SHA256([]byte("k1"))),
		SettledAt:     time.Now().Unix(),
	}
	fDeal1, aDeal0, err := CommitDealAudit(ctx, mem, dr1, true)
	if err != nil || fDeal1 == nil || aDeal0 == nil {
		t.Fatalf("CommitDealAudit(1): f=%v a=%v err=%v", fDeal1, aDeal0, err)
	}
	dr2 := dr1
	dr2.DealID = "deal-audit-2"
	dr2.OfferID = "off-audit-2"
	dr2.PriceSats = 2500
	fDeal2, aDeal1, err := CommitDealAudit(ctx, mem, dr2, true)
	if err != nil || fDeal2 == nil || aDeal1 == nil {
		t.Fatalf("CommitDealAudit(2): f=%v a=%v err=%v", fDeal2, aDeal1, err)
	}

	_, dealTx0, _, _ := guard.ParseBEEF(aDeal0.Beef, guard.DefaultBound)
	_, dealTx1, _, _ := guard.ParseBEEF(aDeal1.Beef, guard.DefaultBound)
	if err := contracts.VerifyAnchorUTXOLineage(dealTx0, dealTx1); err != nil {
		t.Fatalf("__deals__ singleton NektarAnchor.Append lineage failed: %v", err)
	}

	// 4. Both system shards pass full VerifyShardChain integrity AND SPV Prove, and stay hidden from UserShards!
	// __policies__ includes 1 bootstrap Cedar policy commitment (Memory as Law) + 2 OperatorPolicyRecords = 3 fragments.
	expectedCounts := map[string]int{
		memory.DealsShard:    2,
		memory.PoliciesShard: 3,
	}
	for _, sys := range []string{memory.DealsShard, memory.PoliciesShard} {
		rep, err := mem.VerifyShardChain(sys)
		if err != nil || !rep.OK || rep.FragmentCount != expectedCounts[sys] || rep.AnchorCount != 2 {
			t.Fatalf("VerifyShardChain(%s) failed: rep=%+v err=%v", sys, rep, err)
		}
	}
	if pb, vr, err := mem.Prove(ctx, memory.DealsShard, fDeal2.Seq); err != nil || pb == nil || !vr.OK || !vr.Settled {
		t.Fatalf("Prove(__deals__#%d) failed: vr=%+v err=%v", fDeal2.Seq, vr, err)
	}
	if pb, vr, err := mem.Prove(ctx, memory.PoliciesShard, fPol2.Seq); err != nil || pb == nil || !vr.OK || !vr.Settled {
		t.Fatalf("Prove(__policies__#%d) failed: vr=%+v err=%v", fPol2.Seq, vr, err)
	}
	userShards, err := mem.ListUserShards()
	if err != nil {
		t.Fatalf("ListUserShards: %v", err)
	}
	for _, s := range userShards {
		if memory.IsSystemShard(s) {
			t.Fatalf("UserShards leaked reserved system shard %q", s)
		}
	}
}
