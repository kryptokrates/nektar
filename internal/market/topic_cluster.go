package market

import (
	"context"
	"math"
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
	ClusterPrefix              = "cluster:"
	softClusterMinConfidence   = 0.20
	softClusterMaxMembers      = 3
	peerTopicMinConfidence     = 0.65
	peerTopicMaxCorrelations   = 4
	maxPeerTopicCandidates     = 16
	expandMaxClusterTags       = 2
	expandMaxCorrelatedTopics  = 3
)

// TopicClusterSpec defines one soft semantic anchor neighborhood aligned with
// Nektar's 16 overlay lanes. Topics can belong to multiple neighborhoods simultaneously
// with continuous confidence weights (never a strict 1-parent hierarchy).
type TopicClusterSpec struct {
	Slug     string   `json:"slug"`
	Label    string   `json:"label"`
	Rubric   string   `json:"rubric"`
	Keywords []string `json:"-"`
}

// CanonicalClusters is the 16-neighborhood soft basis used for Jev System One
// probability distributions and deterministic offline fallback.
var CanonicalClusters = []TopicClusterSpec{
	{
		Slug:     "sports-fitness",
		Label:    "Sports & Fitness",
		Rubric:   "Sports, athletics, calisthenics, bodyweight training, gym workouts, running, martial arts, outdoor recreation, coaching, and athletic performance.",
		Keywords: []string{"sport", "sports", "calisthenics", "fitness", "workout", "gym", "pullup", "pushup", "bodyweight", "running", "marathon", "football", "soccer", "basketball", "tennis", "cycling", "swimming", "boxing", "mma", "yoga", "crossfit", "strength", "cardio", "athlete", "training", "exercise", "climbing", "lifting"},
	},
	{
		Slug:     "crypto-bsv",
		Label:    "Crypto & BSV",
		Rubric:   "BSV, Bitcoin, blockchain, SPV, UTXOs, Rúnar covenants, smart contracts, satoshis, wallets, mining, overlays, and Web3 protocols.",
		Keywords: []string{"bsv", "bitcoin", "btc", "crypto", "blockchain", "utxo", "spv", "runar", "covenant", "satoshi", "sats", "wallet", "mining", "overlay", "brc", "metanet", "web3", "defi", "token", "onchain", "txid"},
	},
	{
		Slug:     "finance-markets",
		Label:    "Finance & Markets",
		Rubric:   "Stocks, equities, macroeconomics, FX, commodities, trading, accounting, taxes, investing, valuation, and personal finance.",
		Keywords: []string{"finance", "market", "markets", "stock", "stocks", "equity", "equities", "macro", "trading", "invest", "investing", "portfolio", "forex", "fx", "bond", "bonds", "treasury", "inflation", "tax", "taxes", "accounting", "valuation", "earnings", "options", "futures", "banking"},
	},
	{
		Slug:     "software-ops",
		Label:    "Software & Ops",
		Rubric:   "Software engineering, DevOps, SRE, cloud infrastructure, Kubernetes, Linux, databases, APIs, Go, Rust, TypeScript, debugging, and ops runbooks.",
		Keywords: []string{"ops", "devops", "sre", "software", "code", "coding", "programming", "golang", "rust", "python", "typescript", "javascript", "svelte", "react", "docker", "kubernetes", "k8s", "linux", "cloud", "aws", "gcp", "sqlite", "postgres", "sql", "api", "backend", "frontend", "infra", "ci", "cd", "server", "debug"},
	},
	{
		Slug:     "ai-ml",
		Label:    "AI & Agents",
		Rubric:   "Artificial intelligence, machine learning, LLMs, autonomous agents, prompts, MCP tools, embeddings, RAG, fine-tuning, and model evaluations.",
		Keywords: []string{"ai", "ml", "llm", "llms", "agent", "agents", "mcp", "prompt", "prompts", "rag", "embedding", "embeddings", "transformer", "neural", "pytorch", "inference", "jev", "claude", "openai", "gemini", "model", "models", "dataset", "eval", "evals"},
	},
	{
		Slug:     "security-privacy",
		Label:    "Security & Privacy",
		Rubric:   "Cybersecurity, cryptography, zero-knowledge proofs, penetration testing, threat intel, vulnerability advisories, OPSEC, and privacy engineering.",
		Keywords: []string{"security", "infosec", "cyber", "cybersecurity", "privacy", "opsec", "cve", "vuln", "vulnerability", "pentest", "exploit", "malware", "threat", "audit", "zk", "zkp", "encryption", "tls", "auth", "iam", "firewall", "zero-trust"},
	},
	{
		Slug:     "science-health",
		Label:    "Science & Health",
		Rubric:   "Medicine, biology, nutrition, longevity, sleep, psychology, physics, chemistry, climate science, and peer-reviewed scientific research.",
		Keywords: []string{"science", "health", "medical", "medicine", "bio", "biology", "biohacking", "nutrition", "diet", "supplement", "longevity", "sleep", "neuroscience", "psychology", "physics", "chemistry", "genetics", "genomics", "climate", "clinical", "wellness", "mental"},
	},
	{
		Slug:     "legal-policy",
		Label:    "Legal & Policy",
		Rubric:   "Law, regulation, compliance, contracts, intellectual property, corporate governance, public policy, and jurisdiction-specific rules.",
		Keywords: []string{"legal", "law", "regulation", "regulatory", "compliance", "gdpr", "contract", "contracts", "patent", "copyright", "trademark", "policy", "governance", "jurisdiction", "court", "litigation", "sec", "mica"},
	},
	{
		Slug:     "business-sales",
		Label:    "Business & Growth",
		Rubric:   "Startups, SaaS, B2B sales, marketing, growth, pricing, product strategy, operations, supply chain, and company intelligence.",
		Keywords: []string{"business", "startup", "startups", "saas", "sales", "marketing", "growth", "seo", "pricing", "product", "strategy", "b2b", "crm", "revenue", "fundraising", "vc", "supply", "logistics", "ecommerce", "retail"},
	},
	{
		Slug:     "hardware-iot",
		Label:    "Hardware & IoT",
		Rubric:   "Electronics, semiconductors, GPUs, robotics, embedded systems, microcontrollers, drones, 3D printing, and industrial IoT sensors.",
		Keywords: []string{"hardware", "iot", "gpu", "gpus", "cpu", "chip", "chips", "semiconductor", "robotics", "robot", "embedded", "arduino", "esp32", "fpga", "pcb", "drone", "sensor", "sensors", "3dprint", "electronics"},
	},
	{
		Slug:     "media-culture",
		Label:    "Media & Culture",
		Rubric:   "News, journalism, music, film, books, design, art, photography, writing, social media trends, and creative production.",
		Keywords: []string{"media", "culture", "news", "music", "film", "movie", "movies", "book", "books", "design", "art", "photo", "photography", "video", "podcast", "writing", "brand", "creative", "fashion"},
	},
	{
		Slug:     "travel-geo",
		Label:    "Travel & Places",
		Rubric:   "Travel guides, cities, flights, hotels, visas, local recommendations, geography, maps, real estate, and regional weather.",
		Keywords: []string{"travel", "city", "cities", "flight", "flights", "hotel", "hotels", "visa", "geo", "geography", "maps", "local", "weather", "realestate", "housing", "relocation", "nomad", "tourism"},
	},
	{
		Slug:     "food-lifestyle",
		Label:    "Food & Lifestyle",
		Rubric:   "Cooking, recipes, restaurants, coffee, wine, home improvement, gardening, parenting, and everyday personal productivity.",
		Keywords: []string{"food", "cooking", "recipe", "recipes", "restaurant", "coffee", "tea", "wine", "kitchen", "home", "garden", "gardening", "lifestyle", "productivity", "habits", "parenting"},
	},
	{
		Slug:     "education-research",
		Label:    "Education & Learning",
		Rubric:   "Courses, tutorials, study guides, languages, mathematics, history, philosophy, academic papers, and skill acquisition.",
		Keywords: []string{"education", "learning", "study", "course", "courses", "tutorial", "math", "mathematics", "history", "philosophy", "language", "languages", "academic", "research", "paper", "papers", "exam"},
	},
	{
		Slug:     "gaming-esports",
		Label:    "Gaming & Esports",
		Rubric:   "Video games, esports, game design, strategies, patch notes, tabletop RPGs, chess, poker, and competitive gaming.",
		Keywords: []string{"gaming", "game", "games", "esports", "chess", "poker", "rpg", "fps", "mmo", "steam", "console", "speedrun", "gamedev"},
	},
	{
		Slug:     "general",
		Label:    "General",
		Rubric:   "General notes, miscellaneous facts, or broad topics that do not fit a specific specialized cluster above.",
		Keywords: []string{"general", "notes", "misc", "other", "welcome", "default"},
	},
}

var (
	clusterBySlug map[string]TopicClusterSpec
	keywordToSlug map[string]string
)

func init() {
	clusterBySlug = make(map[string]TopicClusterSpec, len(CanonicalClusters))
	keywordToSlug = make(map[string]string, 256)
	for _, c := range CanonicalClusters {
		clusterBySlug[c.Slug] = c
		keywordToSlug[c.Slug] = c.Slug
		for _, kw := range c.Keywords {
			keywordToSlug[strings.ToLower(kw)] = c.Slug
		}
	}
}

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
	ClassifyTopicCluster(ctx context.Context, topic, hint string) (cluster string, confidence float64, ok bool)
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

// IsClusterDomain reports whether d is a canonical cluster domain tag ("cluster:<slug>").
func IsClusterDomain(d string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(d)), ClusterPrefix)
}

// ClusterLabel returns the human-friendly label for a cluster slug or "cluster:<slug>" tag.
func ClusterLabel(d string) string {
	slug := strings.ToLower(strings.TrimSpace(d))
	slug = strings.TrimPrefix(slug, ClusterPrefix)
	if spec, ok := clusterBySlug[slug]; ok {
		return spec.Label
	}
	return slug
}

func splitTopicParts(s string) []string {
	f := func(r rune) bool {
		return r == '-' || r == '_' || r == '/' || r == ' ' || r == ',' || r == ':' || r == '.'
	}
	var out []string
	for _, p := range strings.FieldsFunc(strings.ToLower(strings.TrimSpace(s)), f) {
		if len(p) >= 2 {
			out = append(out, p)
		}
	}
	return out
}

// deterministicSoftClusters scores ALL matching clusters for a topic (+ optional hint)
// so compound topics like "sports-nutrition" or "crypto-tax" get genuine multi-membership
// even when Jev is offline.
func deterministicSoftClusters(topic, hint string) []TopicMembership {
	norm := strings.ToLower(strings.TrimSpace(topic))
	if norm == "" && strings.TrimSpace(hint) == "" {
		spec := clusterBySlug["general"]
		return []TopicMembership{{Cluster: spec.Slug, ClusterTag: ClusterPrefix + spec.Slug, Label: spec.Label, Confidence: 0.50}}
	}
	if strings.HasPrefix(norm, ClusterPrefix) {
		slug := strings.TrimPrefix(norm, ClusterPrefix)
		if spec, ok := clusterBySlug[slug]; ok {
			return []TopicMembership{{Cluster: spec.Slug, ClusterTag: ClusterPrefix + spec.Slug, Label: spec.Label, Confidence: 1.0}}
		}
	}

	weights := make(map[string]float64)
	if norm != "" && norm != "general" && norm != "notes" {
		if slug, ok := keywordToSlug[norm]; ok {
			weights[slug] += 3.0
		}
		for _, part := range splitTopicParts(norm) {
			if slug, ok := keywordToSlug[part]; ok {
				weights[slug] += 2.0
			} else if len(part) >= 4 {
				for _, c := range CanonicalClusters {
					if c.Slug == "general" {
						continue
					}
					for _, kw := range c.Keywords {
						if len(kw) >= 4 && (strings.Contains(part, kw) || strings.Contains(kw, part)) {
							weights[c.Slug] += 1.2
							break
						}
					}
				}
			}
		}
	}
	if h := strings.TrimSpace(hint); h != "" {
		for _, tok := range extractQueryTokens(h) {
			if slug, ok := keywordToSlug[tok]; ok && slug != "general" {
				weights[slug] += 1.0
			}
		}
	}

	if len(weights) == 0 {
		spec := clusterBySlug["general"]
		return []TopicMembership{{Cluster: spec.Slug, ClusterTag: ClusterPrefix + spec.Slug, Label: spec.Label, Confidence: 0.50}}
	}

	var total float64
	for _, w := range weights {
		total += w
	}
	// If only a single cluster matched deterministically, keep calibrated confidence <= 0.90
	// (honest signal, never claiming 100% certainty from keyword heuristics).
	scale := 0.92
	if len(weights) == 1 {
		scale = 0.88
	}

	out := make([]TopicMembership, 0, len(weights))
	for slug, w := range weights {
		spec, ok := clusterBySlug[slug]
		if !ok {
			continue
		}
		conf := math.Round(((w/total)*scale)*100) / 100
		if conf < softClusterMinConfidence {
			continue
		}
		out = append(out, TopicMembership{
			Cluster:    spec.Slug,
			ClusterTag: ClusterPrefix + spec.Slug,
			Label:      spec.Label,
			Confidence: conf,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].Cluster < out[j].Cluster
	})
	if len(out) > softClusterMaxMembers {
		out = out[:softClusterMaxMembers]
	}
	return out
}

// deterministicCorrelatePeerTopics computes flat, non-hierarchical correlations between
// topic and live peer topics when Jev is offline, using shared soft-cluster overlap.
func deterministicCorrelatePeerTopics(topic string, topicMembers []TopicMembership, peerTopics []string) []RelatedTopic {
	if len(peerTopics) == 0 {
		return nil
	}
	norm := strings.ToLower(strings.TrimSpace(topic))
	topicWeights := make(map[string]float64, len(topicMembers))
	for _, m := range topicMembers {
		if m.Cluster != "general" {
			topicWeights[m.Cluster] = m.Confidence
		}
	}
	if len(topicWeights) == 0 {
		return nil
	}

	var out []RelatedTopic
	seen := make(map[string]bool)
	for _, rawPeer := range peerTopics {
		pt := strings.ToLower(strings.TrimSpace(rawPeer))
		if pt == "" || pt == "*" || pt == norm || IsClusterDomain(pt) || seen[pt] {
			continue
		}
		seen[pt] = true
		pMembers := deterministicSoftClusters(pt, "")
		var overlap float64
		for _, pm := range pMembers {
			if tw, ok := topicWeights[pm.Cluster]; ok {
				overlap += math.Min(tw, pm.Confidence)
			}
		}
		if overlap >= 0.40 {
			conf := math.Min(0.84, math.Round((0.55+overlap*0.32)*100)/100)
			if conf >= peerTopicMinConfidence {
				out = append(out, RelatedTopic{
					Topic:      pt,
					Confidence: conf,
					Source:     "deterministic",
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].Topic < out[j].Topic
	})
	if len(out) > peerTopicMaxCorrelations {
		out = out[:peerTopicMaxCorrelations]
	}
	return out
}

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
