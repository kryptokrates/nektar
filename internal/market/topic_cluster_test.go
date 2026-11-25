package market

import (
	"context"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/jev"
)

type stubTopicJevEval struct {
	chooseFn func(req jev.Request) jev.Response
	calls    int
}

func (s *stubTopicJevEval) Evaluate(_ context.Context, req jev.Request) (*jev.Response, error) {
	s.calls++
	resp := s.chooseFn(req)
	return &resp, nil
}

func TestTopicClusterJevAndDeterministicFallback(t *testing.T) {
	// 1. Deterministic offline fallback correlates "calisthenics" and "sports" into the exact same cluster.
	r1 := ClassifyTopic(context.Background(), nil, "calisthenics", "")
	r2 := ClassifyTopic(context.Background(), nil, "sports", "")
	if r1.Cluster != "sports-fitness" || r2.Cluster != "sports-fitness" {
		t.Fatalf("expected both calisthenics and sports in sports-fitness, got %+v and %+v", r1, r2)
	}
	if r1.ClusterTag != "cluster:sports-fitness" || r1.Label != "Sports & Fitness" {
		t.Fatalf("unexpected cluster metadata: %+v", r1)
	}

	// 2. Jev System One classification maps a niche topic not in the static keyword table.
	conf := 0.91
	eval := &stubTopicJevEval{
		chooseFn: func(_ jev.Request) jev.Response {
			return jev.Response{
				Model: "kev-0.8b",
				Answers: map[string]jev.Answer{
					"cluster": {
						Type:       jev.TypeChoice,
						Choice:     "sports-fitness",
						Confidence: &conf,
					},
				},
			}
		},
	}
	jm := NewJevMatcher(eval, JevMatcherOptions{DefaultMode: AIModeGated})
	r3 := ClassifyTopic(context.Background(), jm, "handstand-progression", "gymnastics rings routine")
	if r3.Source != "jev" || r3.Cluster != "sports-fitness" {
		t.Fatalf("expected jev classification sports-fitness, got %+v", r3)
	}
	// Second call with same topic+hint must hit cache (calls stays 1).
	r3Cached := ClassifyTopic(context.Background(), jm, "handstand-progression", "gymnastics rings routine")
	if r3Cached.Cluster != "sports-fitness" || eval.calls != 1 {
		t.Fatalf("expected cache hit with calls=1, got calls=%d res=%+v", eval.calls, r3Cached)
	}

	// 3. Expanded domains converge onto the exact same overlay lane.
	sellDoms := ExpandDomainsWithClusters(context.Background(), jm, []string{"calisthenics"}, "")
	wantDoms := ExpandDomainsWithClusters(context.Background(), jm, []string{"sports"}, "")
	sellLanes := lanesForDomains("memory", sellDoms)
	wantLanes := lanesForDomains("memory", wantDoms)
	sharedLane := bstack.LaneOf("memory", "cluster:sports-fitness")
	foundSell, foundWant := false, false
	for _, l := range sellLanes {
		if l == sharedLane {
			foundSell = true
		}
	}
	for _, l := range wantLanes {
		if l == sharedLane {
			foundWant = true
		}
	}
	if !foundSell || !foundWant {
		t.Fatalf("expected calisthenics (%v) and sports (%v) to share lane %s", sellLanes, wantLanes, sharedLane)
	}
}

func TestTopicClusterExactMatchBonusAndPolicySafety(t *testing.T) {
	// Exact user-domain overlap beats cluster-only overlap on equal price.
	want := bstack.WantAd{
		WantID:  "w1",
		Net:     "memory",
		Domains: []string{"calisthenics", "cluster:sports-fitness"},
		MaxSats: 200,
	}
	exactOffer := bstack.OfferAd{
		OfferID:   "o-exact",
		WantID:    "w1",
		Pub:       "seller-exact",
		Domains:   []string{"calisthenics", "cluster:sports-fitness"},
		PriceSats: 100,
	}
	clusterOffer := bstack.OfferAd{
		OfferID:   "o-cluster",
		WantID:    "w1",
		Pub:       "seller-cluster",
		Domains:   []string{"sports", "cluster:sports-fitness"},
		PriceSats: 100,
	}
	ranked := RankOffersWithRepAndBonds(want, []bstack.OfferAd{clusterOffer, exactOffer}, nil, nil, nil)
	if len(ranked) != 2 || ranked[0].Offer.OfferID != "o-exact" {
		t.Fatalf("expected exact-domain offer ranked first, got %+v", ranked)
	}
	if ranked[0].ScoreBP-ranked[1].ScoreBP != 250 {
		t.Fatalf("expected +250 bp exact-topic tiebreak bonus, got diff=%d", ranked[0].ScoreBP-ranked[1].ScoreBP)
	}

	// Policy AllowedDomains cannot be bypassed by cluster expansion.
	pol := &Policy{
		MaxSatsPerDeal: 500,
		MaxSatsPerDay:  1000,
		AllowedDomains: map[string]bool{"ops": true},
	}
	if err := pol.CanSpendForDomains("seller-x", []string{"calisthenics", "cluster:sports-fitness"}, 100); err == nil {
		t.Fatalf("expected disallowed domain to be rejected even with cluster tag")
	}
}

func TestHasExactUserDomainOverlapIgnoresClusterTags(t *testing.T) {
	if HasExactUserDomainOverlap([]string{"sports", "cluster:sports-fitness"}, []string{"calisthenics", "cluster:sports-fitness"}) {
		t.Fatalf("cluster-only overlap must not count as exact user domain overlap")
	}
	if !HasExactUserDomainOverlap([]string{"Sports", "cluster:sports-fitness"}, []string{"sports", "cluster:sports-fitness"}) {
		t.Fatalf("expected case-insensitive exact user domain overlap")
	}
	if !strings.HasPrefix(ClusterPrefix, "cluster:") {
		t.Fatalf("unexpected ClusterPrefix %q", ClusterPrefix)
	}
}

func TestSoftMultiClusterAndDynamicPeerCorrelation(t *testing.T) {
	// 1. Offline compound topic gets soft multi-membership (NOT a strict 1-parent bucket).
	det := ClassifyTopic(context.Background(), nil, "sports-nutrition", "")
	if len(det.Clusters) < 2 {
		t.Fatalf("expected multi-cluster membership for sports-nutrition, got %+v", det.Clusters)
	}
	hasSports, hasHealth := false, false
	for _, m := range det.Clusters {
		if m.Cluster == "sports-fitness" && m.Confidence >= 0.20 {
			hasSports = true
		}
		if m.Cluster == "science-health" && m.Confidence >= 0.20 {
			hasHealth = true
		}
	}
	if !hasSports || !hasHealth {
		t.Fatalf("expected both sports-fitness and science-health for sports-nutrition, got %+v", det.Clusters)
	}

	// 2. Jev probability distribution returns multiple soft clusters + dynamic TypeNoul peer correlations.
	pSports := 0.89
	pGym := 0.81
	pGo := 0.04
	eval := &stubTopicJevEval{
		chooseFn: func(req jev.Request) jev.Response {
			if _, isChoice := req.Questions["cluster"]; isChoice {
				return jev.Response{
					Model: "kev-0.8b",
					Answers: map[string]jev.Answer{
						"cluster": {
							Type:   jev.TypeChoice,
							Choice: "sports-fitness",
							Probabilities: jev.ProbabilityMap{
								"sports-fitness": 0.62,
								"science-health": 0.29,
								"food-lifestyle": 0.06,
							},
						},
					},
				}
			}
			// Dynamic peer-topic correlation pass (alphabetical sorted cleanPeers: golang, gymnastics, sports).
			return jev.Response{
				Model: "kev-0.8b",
				Answers: map[string]jev.Answer{
					"rel_0": {Type: jev.TypeNoul, Noul: &pGo},
					"rel_1": {Type: jev.TypeNoul, Noul: &pGym},
					"rel_2": {Type: jev.TypeNoul, Noul: &pSports},
				},
			}
		},
	}
	jm := NewJevMatcher(eval, JevMatcherOptions{DefaultMode: AIModeGated})
	res := ClassifyTopicWithPeers(context.Background(), jm, "calisthenics-recovery", "post-workout mobility", []string{"sports", "gymnastics", "golang"})
	if res.Source != "jev" || len(res.Clusters) != 2 {
		t.Fatalf("expected 2 soft Jev clusters (>=20%%), got %+v", res.Clusters)
	}
	if len(res.Related) != 2 || res.Related[0].Topic != "sports" || res.Related[1].Topic != "gymnastics" {
		t.Fatalf("expected dynamic peer correlations [sports 89%%, gymnastics 81%%] and golang filtered out, got %+v", res.Related)
	}

	// 3. Domain expansion includes BOTH soft clusters and dynamically correlated peer topics.
	expanded := ExpandDomainsWithCorrelations(context.Background(), jm, []string{"calisthenics-recovery"}, "post-workout mobility", []string{"sports", "gymnastics", "golang"})
	joined := strings.Join(expanded, ",")
	for _, wantTag := range []string{"calisthenics-recovery", "cluster:sports-fitness", "cluster:science-health", "sports", "gymnastics"} {
		if !strings.Contains(joined, wantTag) {
			t.Fatalf("expected expanded domains to include %q, got %v", wantTag, expanded)
		}
	}
	if strings.Contains(joined, "golang") {
		t.Fatalf("low-confidence peer topic golang must not be included, got %v", expanded)
	}
}
