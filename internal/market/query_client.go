package market

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/distill"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/reputation"
	"github.com/kryptokrates/nektar/internal/store"
)

// MarketQueryItem matches POST /v1/market/query's verified FactPack item wire schema.
type MarketQueryItem struct {
	Shard       string                 `json:"shard"`
	Seq         int64                  `json:"seq"`
	Text        string                 `json:"text"`
	HDP         string                 `json:"hdp"`
	Leaf        string                 `json:"leaf"`
	OntologyID  string                 `json:"ontologyId,omitempty"`
	EdgesRoot   string                 `json:"edgesRoot,omitempty"`
	AnchorTxID  string                 `json:"anchorTxid,omitempty"`
	NMU         *nmu.Spec              `json:"nmu,omitempty"`
	ProofBundle *contracts.ProofBundle `json:"proofBundle,omitempty"`
	CreatedAt   int64                  `json:"createdAt"`
}

// PeerQueryRequest configures a direct buyer-side BRC-105 HTTP 402 metered recall against one peer.
type PeerQueryRequest struct {
	QueryEndpoint string
	SellerPub     string
	Shard         string
	Query         string
	OntologyID    string
	MinGrade      string
	Domains       []string
	TargetShard   string
	MaxSats       uint64
	Limit         int
}

// PeerQueryResult summarizes one verified paid per_query recall response.
type PeerQueryResult struct {
	SellerPub        string            `json:"sellerPub"`
	Shard            string            `json:"shard"`
	TargetShard      string            `json:"targetShard"`
	PaidSats         uint64            `json:"paidSats"`
	PaymentTxID      string            `json:"paymentTxid"`
	DealReceiptToken string            `json:"dealReceiptToken,omitempty"`
	Items            []MarketQueryItem `json:"items"`
	StoredSeqs       []int64           `json:"storedSeqs,omitempty"`
	SPVVerifiedCount int               `json:"spvVerifiedCount"`
}

// FallbackRecallRequest configures automatic per_query 402 fallback when local recall returns 0 hits.
type FallbackRecallRequest struct {
	Shard       string
	Query       string
	Domains     []string
	OntologyID  string
	MinGrade    string
	TargetShard string
	MaxSats     uint64
	Limit       int
	ForceEnable bool // true when caller explicitly opted in via RecallInput.AllowMarketFallback
}

type queryCandidate struct {
	sellerPub string
	pack      bstack.PackRef
	endpoint  string
	repScore  float64
	preferred bool
}

// QueryPeer402 executes a metered NMU-1 `per_query` recall against a peer's POST /v1/market/query
// over BRC-105 HTTP 402, enforcing Watchdog + Policy + SpendGuard + anti-MITM payee verification +
// DealReceipt verification + offline SPV ProofBundle verification + tainted local caching.
func (b *Broker) QueryPeer402(ctx context.Context, req PeerQueryRequest) (*PeerQueryResult, error) {
	if b == nil || b.mem == nil || b.engine == nil {
		return nil, errors.New("market: broker not initialized")
	}
	endpoint := strings.TrimSpace(req.QueryEndpoint)
	shard := strings.TrimSpace(req.Shard)
	if endpoint == "" || shard == "" {
		return nil, errors.New("market: queryEndpoint and shard are required")
	}
	wd := b.Watchdog()
	if wd != nil {
		if err := wd.AllowAutonomousSpend(); err != nil {
			return nil, err
		}
		if req.SellerPub != "" {
			if err := wd.AllowPeerQuery(req.SellerPub); err != nil {
				return nil, err
			}
		}
	}
	pol := b.engine.Policy()
	if pol != nil {
		if kill, _, _, _, _, _, _ := pol.Snapshot(); kill {
			return nil, errors.New("market: operator kill switch is engaged")
		}
	}

	limit := req.Limit
	if limit <= 0 || limit > 10 {
		limit = 5
	}
	bodyPayload, err := json.Marshal(map[string]any{
		"shard":      shard,
		"query":      strings.TrimSpace(req.Query),
		"ontologyId": strings.TrimSpace(req.OntologyID),
		"minGrade":   strings.TrimSpace(req.MinGrade),
		"buyerPub":   b.PubHex(),
		"limit":      limit,
	})
	if err != nil {
		return nil, err
	}

	qCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	hc := b.httpClient
	if hc == nil {
		hc = core.SafeHTTPClientForNetwork(b.mem.Chain().Network(), 8*time.Second)
	}

	var result *PeerQueryResult
	runQuery := func() error {
		doPost := func(payHdr string) (*http.Response, error) {
			httpReq, rErr := http.NewRequestWithContext(qCtx, http.MethodPost, endpoint, bytes.NewReader(bodyPayload))
			if rErr != nil {
				return nil, rErr
			}
			httpReq.Header.Set("Content-Type", "application/json")
			if payHdr != "" {
				httpReq.Header.Set("X-BSV-Payment", payHdr)
			}
			return hc.Do(httpReq)
		}

		resp, err := doPost("")
		if err != nil {
			return err
		}

		var expectedTxID string
		var paidSats uint64
		sellerPub := strings.ToLower(strings.TrimSpace(req.SellerPub))

		if resp.StatusCode == http.StatusPaymentRequired {
			requiredSats, payeePub, derivPrefix, challengeSellerPub, parseErr := parseBRC105Challenge(resp)
			_ = resp.Body.Close()
			if parseErr != nil {
				return parseErr
			}
			// Fail-closed anti-MITM check: challenge sellerPub (when declared) OR payeePub (when sellerPub is omitted)
			// MUST match the directory-signed peer identity.
			if err := verifyBRC105ChallengeIdentity(sellerPub, challengeSellerPub, payeePub); err != nil {
				return err
			}
			if sellerPub == "" && challengeSellerPub != "" {
				sellerPub = challengeSellerPub
			}
			if sellerPub == "" {
				sellerPub = payeePub
			}
			if req.MaxSats > 0 && requiredSats > req.MaxSats {
				return fmt.Errorf("market: per_query price %d sats exceeds fallback cap %d sats", requiredSats, req.MaxSats)
			}
			if pol != nil {
				if err := pol.CanSpendForDomains(sellerPub, req.Domains, requiredSats); err != nil {
					return err
				}
			}
			payHdr, txid, payErr := discovery.BuildStandaloneHubPayment(qCtx, b.mem.Chain(), payeePub, derivPrefix, "market_query:"+shard, requiredSats)
			if payErr != nil {
				if wd != nil {
					wd.RecordSpendOutcome(payErr)
				}
				return payErr
			}
			expectedTxID = txid
			paidSats = requiredSats
			resp, err = doPost(payHdr)
			if err != nil {
				if wd != nil {
					wd.RecordSpendOutcome(err)
				}
				return err
			}
		}
		defer resp.Body.Close()
		rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
		if resp.StatusCode != http.StatusOK {
			err := fmt.Errorf("market: per_query HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(rawBody)))
			if wd != nil && paidSats > 0 {
				wd.RecordSpendOutcome(err)
			}
			return err
		}

		var wire struct {
			OK               bool              `json:"ok"`
			Shard            string            `json:"shard"`
			PaidSats         uint64            `json:"paidSats"`
			PaymentTxID      string            `json:"paymentTxid"`
			Items            []MarketQueryItem `json:"items"`
			DealReceipt      *DealReceipt      `json:"dealReceipt"`
			DealReceiptToken string            `json:"dealReceiptToken"`
		}
		if err := json.Unmarshal(rawBody, &wire); err != nil {
			return fmt.Errorf("market: decode per_query response: %w", err)
		}
		if !wire.OK {
			return errors.New("market: per_query seller returned ok=false")
		}
		if wire.DealReceipt != nil {
			if vr := VerifyDealReceipt(*wire.DealReceipt, false); !vr.OK {
				return fmt.Errorf("market: invalid per_query deal receipt: %s", vr.Error)
			}
			if sellerPub != "" && !strings.EqualFold(wire.DealReceipt.SellerPub, sellerPub) {
				return fmt.Errorf("market: deal receipt seller mismatch (%s != %s)", wire.DealReceipt.SellerPub, sellerPub)
			}
			if expectedTxID != "" && wire.DealReceipt.PaymentTxID != "" && !strings.EqualFold(wire.DealReceipt.PaymentTxID, expectedTxID) {
				return fmt.Errorf("market: deal receipt txid mismatch")
			}
			if sellerPub == "" {
				sellerPub = strings.ToLower(strings.TrimSpace(wire.DealReceipt.SellerPub))
			}
		}

		targetShard := strings.TrimSpace(req.TargetShard)
		if targetShard == "" {
			dom := "general"
			if len(req.Domains) > 0 && strings.TrimSpace(req.Domains[0]) != "" {
				dom = strings.ToLower(strings.TrimSpace(req.Domains[0]))
			}
			targetShard = "bought-query-" + dom
		}

		verifiedItems := make([]MarketQueryItem, 0, len(wire.Items))
		storedSeqs := make([]int64, 0, len(wire.Items))
		spvCount := 0
		tracker := b.mem.Chain().Tracker()
		for _, item := range wire.Items {
			if strings.TrimSpace(item.Text) == "" {
				continue
			}
			if sr := distill.Scrub(item.Text); sr.Verdict == distill.ScrubBlocked {
				continue
			}
			if item.ProofBundle != nil {
				vr := contracts.VerifyFragment(*item.ProofBundle, tracker)
				switch {
				case vr.OK:
					spvCount++
				case vr.Reason == "Unsettled" || vr.Reason == "InsufficientDepth":
					// Cryptographic commitments (leaf -> R_n -> NektarAnchor state -> ChainTail UTXO lineage)
					// already verified in VerifyFragment before returning Unsettled/InsufficientDepth; accept
					// into tainted cache without counting as mined SPV-settled.
				default:
					continue
				}
			}
			rights := string(nmu.RightsInferenceOnly)
			if item.NMU != nil && item.NMU.UsageRights != "" {
				rights = string(nmu.EffectiveRights(item.NMU.UsageRights))
			}
			memRes, mErr := b.mem.Remember(qCtx, memory.RememberInput{
				Shard:       targetShard,
				Text:        item.Text,
				Source:      "per-query-402:" + sellerPub,
				Tainted:     true,
				Ontology:    item.OntologyID,
				NMU:         item.NMU,
				UsageRights: rights,
				Extra: map[string]string{
					"seller_pub":    sellerPub,
					"source_shard":  item.Shard,
					"source_seq":    strconv.FormatInt(item.Seq, 10),
					"payment_txid":  wire.PaymentTxID,
					"pricing_mode":  string(nmu.ModePerQuery),
					"receipt_token": wire.DealReceiptToken,
				},
			})
			if mErr == nil && memRes != nil {
				storedSeqs = append(storedSeqs, memRes.Seq)
			}
			verifiedItems = append(verifiedItems, item)
		}
		if len(wire.Items) > 0 && len(verifiedItems) == 0 {
			return errors.New("market: all per_query items failed SPV or safety verification")
		}

		effPaid := paidSats
		if effPaid == 0 {
			effPaid = wire.PaidSats
		}
		if effPaid > 0 {
			if pol != nil {
				pol.RecordSpend(effPaid)
			}
			if b.mem.DB() != nil {
				_ = b.mem.DB().RecordEconomicsEvent(store.EconomicsEventRow{
					Kind:       "deal_buy",
					SatsDelta:  -int64(effPaid),
					OntologyID: reputation.NormalizeOntologyID(req.OntologyID),
					Shard:      targetShard,
					DealID:     "qry:" + wire.PaymentTxID,
					CreatedAt:  time.Now().Unix(),
				})
			}
		}
		if wd != nil {
			wd.RecordSpendOutcome(nil)
		}

		result = &PeerQueryResult{
			SellerPub:        sellerPub,
			Shard:            shard,
			TargetShard:      targetShard,
			PaidSats:         effPaid,
			PaymentTxID:      wire.PaymentTxID,
			DealReceiptToken: wire.DealReceiptToken,
			Items:            verifiedItems,
			StoredSeqs:       storedSeqs,
			SPVVerifiedCount: spvCount,
		}
		return nil
	}

	if wd != nil && strings.TrimSpace(req.SellerPub) != "" {
		if err := wd.ExecutePeerQuery(req.SellerPub, runQuery); err != nil {
			return nil, err
		}
	} else {
		if err := runQuery(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// QueryFallbackOnRecallMiss attempts an opt-in metered per_query 402 recall against top-ranked peers
// when local recall yields 0 hits. Fails closed (returns nil, nil) unless explicitly enabled.
func (b *Broker) QueryFallbackOnRecallMiss(ctx context.Context, req FallbackRecallRequest) (*PeerQueryResult, error) {
	if b == nil || b.mem == nil || b.engine == nil || b.disc == nil {
		return nil, nil
	}
	if strings.TrimSpace(req.Query) == "" {
		return nil, nil
	}
	pol := b.engine.Policy()
	if pol == nil {
		return nil, nil
	}
	autoCfg := pol.AutonomyConfig()
	if !req.ForceEnable && !autoCfg.AutoQueryOnMiss {
		return nil, nil
	}
	kill, maxDeal, _, _, _, _, _ := pol.Snapshot()
	if kill {
		return nil, nil
	}
	wd := b.Watchdog()
	if wd != nil {
		if err := wd.AllowAutonomousSpend(); err != nil {
			return nil, err
		}
	}
	capSats := req.MaxSats
	if capSats == 0 || (autoCfg.MaxAutoQuerySats > 0 && capSats > autoCfg.MaxAutoQuerySats) {
		capSats = autoCfg.MaxAutoQuerySats
	}
	if maxDeal > 0 && capSats > maxDeal {
		capSats = maxDeal
	}
	if capSats == 0 {
		return nil, nil
	}

	selfPub := strings.ToLower(b.PubHex())
	wantOnt := strings.ToLower(strings.TrimSpace(req.OntologyID))
	wantMinGrade := nmu.NormalizeGrade(req.MinGrade)
	wantDomSet := make(map[string]bool)
	for _, d := range req.Domains {
		if s := strings.ToLower(strings.TrimSpace(d)); s != "" {
			wantDomSet[s] = true
		}
	}
	if req.Shard != "" && len(wantDomSet) == 0 {
		s := strings.ToLower(strings.TrimSpace(req.Shard))
		s = strings.TrimPrefix(s, "market-")
		s = strings.TrimPrefix(s, "ont-")
		if s != "" && s != "default" {
			wantDomSet[s] = true
		}
	}

	peers := b.disc.Peers()
	var cands []queryCandidate
	for _, p := range peers {
		pub := strings.ToLower(strings.TrimSpace(p.Pub))
		if pub == "" || pub == selfPub {
			continue
		}
		if wd != nil && wd.AllowPeerQuery(pub) != nil {
			continue
		}
		blocked := pol.IsBlockedSeller(pub)
		preferred := pol.IsPreferredSeller(pub)
		if blocked {
			continue
		}
		var repScore float64
		if b.mem.DB() != nil {
			if rec, err := b.mem.DB().GetReputation6D(pub, reputation.NormalizeOntologyID(wantOnt)); err == nil {
				repScore = float64(rec.CompositeBP)
			}
		}
		for _, pk := range p.Packs {
			if !packSupportsPerQuery(pk) || pk.QueryPriceSats == 0 || pk.QueryPriceSats > capSats {
				continue
			}
			ep := strings.TrimSpace(pk.QueryEndpoint)
			if ep == "" && strings.TrimSpace(p.PublicURL) != "" {
				ep = strings.TrimRight(strings.TrimSpace(p.PublicURL), "/") + "/v1/market/query"
			}
			if ep == "" {
				continue
			}
			if wantOnt != "" && pk.OntologyID != "" && !strings.EqualFold(pk.OntologyID, wantOnt) {
				continue
			}
			if wantMinGrade != "" && pk.Grade != "" && !nmu.MeetsMinGrade(nmu.NormalizeGrade(pk.Grade), wantMinGrade) {
				continue
			}
			if len(wantDomSet) > 0 && !packMatchesDomains(pk, wantDomSet) {
				continue
			}
			cands = append(cands, queryCandidate{
				sellerPub: pub,
				pack:      pk,
				endpoint:  ep,
				repScore:  repScore,
				preferred: preferred,
			})
		}
	}
	if len(cands) == 0 {
		return nil, nil
	}

	// Deterministic ranking: preferred first -> lowest query price -> higher reputation -> pubkey tie-breaker.
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].preferred != cands[j].preferred {
			return cands[i].preferred
		}
		if cands[i].pack.QueryPriceSats != cands[j].pack.QueryPriceSats {
			return cands[i].pack.QueryPriceSats < cands[j].pack.QueryPriceSats
		}
		if cands[i].repScore != cands[j].repScore {
			return cands[i].repScore > cands[j].repScore
		}
		return cands[i].sellerPub < cands[j].sellerPub
	})

	maxAttempts := min(2, len(cands))
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		c := cands[i]
		res, err := b.QueryPeer402(ctx, PeerQueryRequest{
			QueryEndpoint: c.endpoint,
			SellerPub:     c.sellerPub,
			Shard:         c.pack.Shard,
			Query:         req.Query,
			OntologyID:    req.OntologyID,
			MinGrade:      req.MinGrade,
			Domains:       c.pack.Domains,
			TargetShard:   req.TargetShard,
			MaxSats:       capSats,
			Limit:         req.Limit,
		})
		if err != nil {
			lastErr = err
			continue
		}
		if res != nil && len(res.Items) > 0 {
			return res, nil
		}
	}
	return nil, lastErr
}

func packSupportsPerQuery(pk bstack.PackRef) bool {
	if strings.EqualFold(pk.PricingMode, string(nmu.ModePerQuery)) {
		return true
	}
	for _, m := range pk.EnabledModes {
		if strings.EqualFold(strings.TrimSpace(m), string(nmu.ModePerQuery)) {
			return true
		}
	}
	return pk.QueryEndpoint != "" && pk.QueryPriceSats > 0
}

func packMatchesDomains(pk bstack.PackRef, want map[string]bool) bool {
	if want[strings.ToLower(strings.TrimPrefix(strings.TrimSpace(pk.Shard), "market-"))] {
		return true
	}
	for _, d := range pk.Domains {
		if want[strings.ToLower(strings.TrimSpace(d))] {
			return true
		}
	}
	return false
}

// verifyBRC105ChallengeIdentity enforces fail-closed anti-MITM binding between a directory-signed
// expectedSellerPub and the HTTP 402 challenge. If the optional JSON sellerPub field is declared,
// it must match expectedSellerPub; if omitted, payeePub itself must match expectedSellerPub so a
// malicious server or MITM cannot redirect payment to a foreign key by stripping sellerPub.
func verifyBRC105ChallengeIdentity(expectedSellerPub, challengeSellerPub, payeePub string) error {
	expected := strings.ToLower(strings.TrimSpace(expectedSellerPub))
	if expected == "" {
		return nil
	}
	declared := strings.ToLower(strings.TrimSpace(challengeSellerPub))
	if declared != "" {
		if !strings.EqualFold(declared, expected) {
			return fmt.Errorf("market: 402 sellerPub identity mismatch (expected %s, got %s)", expected, declared)
		}
		return nil
	}
	payee := strings.ToLower(strings.TrimSpace(payeePub))
	if payee == "" || !strings.EqualFold(payee, expected) {
		return fmt.Errorf("market: 402 payeePub identity mismatch with omitted sellerPub (expected %s, got %s)", expected, payee)
	}
	return nil
}
