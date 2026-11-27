package market

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// ClusterPrefix marks canonical semantic topic cluster tags carried inside existing
// Domains []string fields on WantAd, OfferAd, SellListing, and DirectoryAd.
// Carrying soft multi-cluster tags and dynamically correlated peer topics inside Domains
// preserves 100% nkt1 CBOR wire compatibility while allowing non-hierarchical, emergent
// topic neighborhoods across the 16-lane overlay.
const (
	ClusterPrefix             = "cluster:"
	softClusterMinConfidence  = 0.20
	softClusterMaxMembers     = 3
	peerTopicMinConfidence    = 0.65
	peerTopicMaxCorrelations  = 4
	maxPeerTopicCandidates    = 16
	expandMaxClusterTags      = 2
	expandMaxCorrelatedTopics = 3
)

// TopicMembership represents one soft cluster membership with its calibrated confidence [0,1].
type TopicMembership struct {
	Cluster    string  `json:"cluster"`
	ClusterTag string  `json:"clusterTag"`
	Label      string  `json:"label"`
	Confidence float64 `json:"confidence"`
}

// RelatedTopic represents a flat, non-hierarchical dynamic correlation to a live
// peer topic on the network with its calibrated confidence [0,1].
type RelatedTopic struct {
	Topic      string  `json:"topic"`
	Confidence float64 `json:"confidence"`
	Source     string  `json:"source"` // "jev" | "deterministic"
}

// TopicClassifier is an optional interface implemented by semantic matchers (e.g. *JevMatcher)
// to compute soft probability distributions over canonical clusters and direct noul correlations
// against live peer topics in single System One forward passes.
type TopicClassifier interface {
	ClassifyTopicSoft(ctx context.Context, topic, hint string) ([]TopicMembership, bool)
	CorrelatePeerTopics(ctx context.Context, topic, hint string, peerTopics []string) ([]RelatedTopic, bool)
}

// TopicClusterResult reports a topic's soft multi-cluster memberships and dynamic
// peer-topic correlations (with primary Cluster/Confidence kept for backward compatibility).
type TopicClusterResult struct {
	Topic      string            `json:"topic"`
	Cluster    string            `json:"cluster"`
	ClusterTag string            `json:"clusterTag"`
	Label      string            `json:"label"`
	Confidence float64           `json:"confidence"`
	Source     string            `json:"source"` // "jev" | "deterministic"
	Clusters   []TopicMembership `json:"clusters,omitempty"`
	Related    []RelatedTopic    `json:"related,omitempty"`
}

type topicCacheEntry struct {
	res       TopicClusterResult
	expiresAt time.Time
}

var (
	topicCacheMu  sync.Mutex
	topicCache    = make(map[string]topicCacheEntry)
	topicCacheTTL = 10 * time.Minute
	topicCacheCap = 1024
)

// ClassifyTopic maps a free-form topic (e.g. "calisthenics" or "sports-nutrition") to
// soft multi-cluster memberships using Jev when available and falling back deterministically offline.
func ClassifyTopic(ctx context.Context, matcher Matcher, topic, hint string) TopicClusterResult {
	return ClassifyTopicWithPeers(ctx, matcher, topic, hint, nil)
}

// ClassifyTopicWithPeers computes BOTH soft multi-cluster memberships AND dynamic
// flat correlations against active peerTopics on the network.
func ClassifyTopicWithPeers(ctx context.Context, matcher Matcher, topic, hint string, peerTopics []string) TopicClusterResult {
	norm := strings.ToLower(strings.TrimSpace(topic))
	if norm == "" && strings.TrimSpace(hint) == "" {
		norm = "general"
	}
	if strings.HasPrefix(norm, ClusterPrefix) {
		slug := strings.TrimPrefix(norm, ClusterPrefix)
		if spec, ok := clusterBySlug[slug]; ok {
			m := TopicMembership{Cluster: spec.Slug, ClusterTag: ClusterPrefix + spec.Slug, Label: spec.Label, Confidence: 1.0}
			return TopicClusterResult{
				Topic:      norm,
				Cluster:    spec.Slug,
				ClusterTag: ClusterPrefix + spec.Slug,
				Label:      spec.Label,
				Confidence: 1.0,
				Source:     "deterministic",
				Clusters:   []TopicMembership{m},
			}
		}
	}

	// Normalize & bound peer topics for deterministic cache key and Jev fan-out.
	cleanPeers := normalizePeerTopicList(norm, peerTopics)

	useJev := false
	var clf TopicClassifier
	if matcher != nil && matcher.DefaultMode() != AIModeOff {
		if tc, ok := matcher.(TopicClassifier); ok {
			clf = tc
			useJev = true
		}
	}

	hintKey := strings.ToLower(strings.TrimSpace(hint))
	if len(hintKey) > 96 {
		hintKey = hintKey[:96]
	}
	cacheMode := "det"
	if useJev {
		cacheMode = "jev"
	}
	cacheKey := cacheMode + "|" + norm + "|" + hintKey + "|" + strings.Join(cleanPeers, ",")

	now := time.Now()
	topicCacheMu.Lock()
	if ent, ok := topicCache[cacheKey]; ok && now.Before(ent.expiresAt) {
		topicCacheMu.Unlock()
		return ent.res
	}
	topicCacheMu.Unlock()

	var members []TopicMembership
	source := "deterministic"
	if useJev && clf != nil {
		if soft, ok := clf.ClassifyTopicSoft(ctx, norm, hint); ok && len(soft) > 0 {
			members = soft
			source = "jev"
		}
	}
	if len(members) == 0 {
		members = deterministicSoftClusters(norm, hint)
		source = "deterministic"
	}

	var related []RelatedTopic
	if len(cleanPeers) > 0 {
		if useJev && clf != nil {
			if rel, ok := clf.CorrelatePeerTopics(ctx, norm, hint, cleanPeers); ok {
				related = rel
			}
		}
		if len(related) == 0 {
			related = deterministicCorrelatePeerTopics(norm, members, cleanPeers)
		}
	}

	primary := members[0]
	res := TopicClusterResult{
		Topic:      norm,
		Cluster:    primary.Cluster,
		ClusterTag: primary.ClusterTag,
		Label:      primary.Label,
		Confidence: primary.Confidence,
		Source:     source,
		Clusters:   members,
		Related:    related,
	}

	topicCacheMu.Lock()
	if len(topicCache) >= topicCacheCap {
		for k, v := range topicCache {
			if now.After(v.expiresAt) || len(topicCache) >= topicCacheCap {
				delete(topicCache, k)
				if len(topicCache) < topicCacheCap/2 {
					break
				}
			}
		}
	}
	topicCache[cacheKey] = topicCacheEntry{res: res, expiresAt: now.Add(topicCacheTTL)}
	topicCacheMu.Unlock()

	return res
}

func normalizePeerTopicList(exclude string, peerTopics []string) []string {
	if len(peerTopics) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(peerTopics))
	out := make([]string, 0, min(len(peerTopics), maxPeerTopicCandidates))
	for _, raw := range peerTopics {
		pt := strings.ToLower(strings.TrimSpace(raw))
		if pt == "" || pt == "*" || pt == exclude || pt == "general" || pt == "notes" || IsClusterDomain(pt) || seen[pt] {
			continue
		}
		seen[pt] = true
		out = append(out, pt)
		if len(out) >= maxPeerTopicCandidates {
			break
		}
	}
	sort.Strings(out)
	return out
}

// ExpandDomainsWithClusters normalizes user domains and appends their soft multi-cluster
// tags (up to top 2 non-general clusters with confidence >= 0.20) so related topics
// converge on shared overlay lanes without strict 1-parent hierarchy.
func ExpandDomainsWithClusters(ctx context.Context, matcher Matcher, domains []string, hint string) []string {
	return ExpandDomainsWithCorrelations(ctx, matcher, domains, hint, nil)
}

// ExpandDomainsWithCorrelations expands user domains with BOTH soft multi-cluster tags
// AND dynamically correlated live peer topics (confidence >= 0.65).
func ExpandDomainsWithCorrelations(ctx context.Context, matcher Matcher, domains []string, hint string, peerTopics []string) []string {
	seen := make(map[string]bool, len(domains)*3)
	out := make([]string, 0, len(domains)*3)
	var extraTags []string

	for _, raw := range domains {
		d := strings.ToLower(strings.TrimSpace(raw))
		if d == "" {
			continue
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
		if d == "*" || IsClusterDomain(d) {
			continue
		}
		res := ClassifyTopicWithPeers(ctx, matcher, d, hint, peerTopics)
		addedClusters := 0
		for _, m := range res.Clusters {
			if addedClusters >= expandMaxClusterTags {
				break
			}
			if m.Cluster == "general" && d != "general" {
				continue
			}
			if m.ClusterTag != "" && !seen[m.ClusterTag] {
				seen[m.ClusterTag] = true
				extraTags = append(extraTags, m.ClusterTag)
				addedClusters++
			}
		}
		addedRel := 0
		for _, rel := range res.Related {
			if addedRel >= expandMaxCorrelatedTopics {
				break
			}
			if rel.Confidence < peerTopicMinConfidence {
				continue
			}
			if rel.Topic != "" && !seen[rel.Topic] {
				seen[rel.Topic] = true
				extraTags = append(extraTags, rel.Topic)
				addedRel++
			}
		}
	}
	return append(out, extraTags...)
}

// HasExactUserDomainOverlap reports whether wantDomains and listingDomains share
// at least one non-cluster, non-wildcard user domain (e.g. "calisthenics" == "calisthenics").
func HasExactUserDomainOverlap(wantDomains, listingDomains []string) bool {
	wantSet := make(map[string]bool, len(wantDomains))
	for _, d := range wantDomains {
		n := strings.ToLower(strings.TrimSpace(d))
		if n == "" || n == "*" || IsClusterDomain(n) {
			continue
		}
		wantSet[n] = true
	}
	for _, d := range listingDomains {
		n := strings.ToLower(strings.TrimSpace(d))
		if n == "" || n == "*" || IsClusterDomain(n) {
			continue
		}
		if wantSet[n] {
			return true
		}
	}
	return false
}
