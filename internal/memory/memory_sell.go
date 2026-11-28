package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

const (
	maxSellEvalCacheEntries = 65536
	nmuClassCacheTTL        = 3 * time.Second
)

type nmuClassCacheEntry struct {
	cls       nmu.GradeClassification
	ontID     string
	edgesRoot string
	expiresAt time.Time
}

var (
	sellCacheMu       sync.RWMutex
	cedarSellCache    = make(map[string]bool, 1024)
	cedarSellKeys     []string
	cedarSellHead     int
	scrubVerdictCache = make(map[string]string, 1024)
	scrubVerdictKeys  []string
	scrubVerdictHead  int
	nmuClassCache     = make(map[string]nmuClassCacheEntry, 1024)
)

func lookupCedarSellCache(key string) (bool, bool) {
	sellCacheMu.RLock()
	v, ok := cedarSellCache[key]
	sellCacheMu.RUnlock()
	return v, ok
}

func putCedarSellCache(key string, allow bool) {
	sellCacheMu.Lock()
	if _, exists := cedarSellCache[key]; !exists {
		for len(cedarSellCache) >= maxSellEvalCacheEntries && cedarSellHead < len(cedarSellKeys) {
			old := cedarSellKeys[cedarSellHead]
			cedarSellKeys[cedarSellHead] = ""
			cedarSellHead++
			delete(cedarSellCache, old)
		}
		if cedarSellHead > 0 && cedarSellHead*2 >= len(cedarSellKeys) {
			rem := copy(cedarSellKeys, cedarSellKeys[cedarSellHead:])
			cedarSellKeys = cedarSellKeys[:rem]
			cedarSellHead = 0
		}
		cedarSellKeys = append(cedarSellKeys, key)
	}
	cedarSellCache[key] = allow
	sellCacheMu.Unlock()
}

func lookupScrubVerdictCache(hdp string) (string, bool) {
	sellCacheMu.RLock()
	v, ok := scrubVerdictCache[hdp]
	sellCacheMu.RUnlock()
	return v, ok
}

func putScrubVerdictCache(hdp, blockedRule string) {
	sellCacheMu.Lock()
	if _, exists := scrubVerdictCache[hdp]; !exists {
		for len(scrubVerdictCache) >= maxSellEvalCacheEntries && scrubVerdictHead < len(scrubVerdictKeys) {
			old := scrubVerdictKeys[scrubVerdictHead]
			scrubVerdictKeys[scrubVerdictHead] = ""
			scrubVerdictHead++
			delete(scrubVerdictCache, old)
		}
		if scrubVerdictHead > 0 && scrubVerdictHead*2 >= len(scrubVerdictKeys) {
			rem := copy(scrubVerdictKeys, scrubVerdictKeys[scrubVerdictHead:])
			scrubVerdictKeys = scrubVerdictKeys[:rem]
			scrubVerdictHead = 0
		}
		scrubVerdictKeys = append(scrubVerdictKeys, hdp)
	}
	scrubVerdictCache[hdp] = blockedRule
	sellCacheMu.Unlock()
}

// SellEvaluation captures the unified fail-closed market egress verdict and provable NMU-1
// classification for a recalled fragment across spot quotes, per_query HTTP 402, Grade S SSE feeds,
// and direct disclosures.
type SellEvaluation struct {
	Allowed        bool
	Reason         string
	Classification nmu.GradeClassification
	OntologyID     string
	EdgesRoot      string
	UsageRights    nmu.UsageRights
	FreshnessClass nmu.FreshnessClass
	ValidUntilUnix int64
	SubjectVersion string
}

// ClassifyFragmentNMU computes the exact provable NMU-1 Grade and ontology-scoped UnitCount
// for one local fragment using canonical triples, L1 MPE graph metadata, active bond, and live feed availability.
func (s *Service) ClassifyFragmentNMU(
	ctx context.Context,
	shard string,
	seq int64,
	distilled bool,
	fragOnt string,
	hasActiveBond bool,
	liveFeedURL string,
) (nmu.GradeClassification, string, string) {
	if s == nil || s.db == nil {
		return nmu.GradeClassification{Grade: nmu.GradeC, UnitCount: 1}, fragOnt, ""
	}
	cacheKey := fmt.Sprintf("%p|%s#%d|%t|%s|%t|%s", s, shard, seq, distilled, fragOnt, hasActiveBond, liveFeedURL)
	now := time.Now()
	sellCacheMu.RLock()
	if hit, ok := nmuClassCache[cacheKey]; ok && now.Before(hit.expiresAt) {
		sellCacheMu.RUnlock()
		return hit.cls, hit.ontID, hit.edgesRoot
	}
	sellCacheMu.RUnlock()

	ontID, edgesRoot, _ := s.db.GetFragmentGraphMeta(shard, seq)
	if ontID == "" {
		ontID, _ = s.db.FragmentOntologyID(shard, seq)
	}
	if ontID == "" {
		ontID = strings.ToLower(strings.TrimSpace(fragOnt))
	}
	var triples []reasoning.Triple
	if rawRec, gErr := s.db.GetFragment(shard, seq); gErr == nil && len(rawRec.TriplesJSON) > 0 {
		_ = json.Unmarshal(rawRec.TriplesJSON, &triples)
	}
	if edges, eErr := s.db.ListKnowledgeEdges(store.KnowledgeEdgeFilter{
		Shard: shard, Seq: seq, Status: "accepted", Limit: 512,
	}); eErr == nil {
		for _, ke := range edges {
			triples = append(triples, reasoning.Triple{
				Subject:   ke.Subject,
				Predicate: ke.Predicate,
				Object:    ke.Object,
				Hash:      ke.TripleHash,
			})
		}
	}
	var allowedPreds map[string]struct{}
	if ontID != "" {
		allowedPreds = s.ResolveOntologyPredicateSet(ctx, ontID)
	}
	cls := nmu.ClassifyFragmentGrade(nmu.FragmentGradeInput{
		HasCleanText:           true,
		Distilled:              distilled || len(edgesRoot) == 64,
		OntologyID:             ontID,
		Triples:                triples,
		AllowedPredicates:      allowedPreds,
		MangleConsistent:       true,
		EdgesRoot:              edgesRoot,
		MPEHasOntologyAndEdges: len(ontID) == 64 && len(edgesRoot) == 64,
		ActiveBond:             hasActiveBond,
		LiveFeedURL:            liveFeedURL,
	})
	sellCacheMu.Lock()
	if len(nmuClassCache) >= maxSellEvalCacheEntries {
		for k, v := range nmuClassCache {
			if now.After(v.expiresAt) {
				delete(nmuClassCache, k)
			}
		}
		if len(nmuClassCache) >= maxSellEvalCacheEntries {
			clear(nmuClassCache)
		}
	}
	nmuClassCache[cacheKey] = nmuClassCacheEntry{
		cls:       cls,
		ontID:     ontID,
		edgesRoot: edgesRoot,
		expiresAt: now.Add(nmuClassCacheTTL),
	}
	sellCacheMu.Unlock()
	return cls, ontID, edgesRoot
}

// EvaluateSellableFragment enforces the single authoritative fail-closed sell egress gate:
//  1. Borg-group secrets and non-Level-0 fragments are unconditionally blocked.
//  2. Cedar Action::"sell" policy gate (requiring resource.distilled == true unless listing.AllowRaw).
//  3. Deterministic PII & secret scrubber (distill.Scrub != ScrubBlocked).
//  4. NMU-1 UsageRights resale gate (blocks Tainted inference_only or GradeMismatch fragments).
//  5. NMU-1 Freshness expiry gate (blocks expired ephemeral fragments).
//  6. Listing MinAdvertisedGrade floor when configured.
func (s *Service) EvaluateSellableFragment(
	ctx context.Context,
	f RecalledFragment,
	listing *store.SellListing,
	hasActiveBond bool,
	liveFeedURL string,
	nowUnix int64,
) SellEvaluation {
	if s == nil {
		return SellEvaluation{Allowed: false, Reason: "service_nil"}
	}
	if nowUnix <= 0 {
		nowUnix = time.Now().Unix()
	}
	if f.BorgGroup != "" || f.Level > 0 {
		return SellEvaluation{Allowed: false, Reason: "restricted_level_or_borg"}
	}
	allowRaw := listing != nil && listing.AllowRaw
	polHash := ""
	if pc := s.PolicyCommit(); pc != nil {
		polHash = pc.Hash
	}
	cedarKey := fmt.Sprintf("%p|%s|%s#%d|%d|%s|%t|%t", s, polHash, f.Shard, f.Seq, f.Level, f.Class, f.Distilled, allowRaw)
	if allowed, cached := lookupCedarSellCache(cedarKey); cached {
		if !allowed {
			return SellEvaluation{Allowed: false, Reason: "cedar_sell_denied"}
		}
	} else {
		dec, aErr := s.AuthorizeSell(
			policy.Principal{Type: "Agent", ID: "market-seller", Level: 0},
			policy.Resource{
				Type:      "Fragment",
				ID:        fmt.Sprintf("%s#%d", f.Shard, f.Seq),
				Level:     int64(f.Level),
				Shard:     f.Shard,
				Class:     f.Class,
				Distilled: f.Distilled,
			},
			allowRaw,
		)
		ok := aErr == nil && dec.Allow
		putCedarSellCache(cedarKey, ok)
		if !ok {
			return SellEvaluation{Allowed: false, Reason: "cedar_sell_denied"}
		}
	}

	scrubKey := f.HDP
	if scrubKey == "" {
		scrubKey = core.ToHex(core.SHA256([]byte(f.Text)))
	}
	if blockedRule, cached := lookupScrubVerdictCache(scrubKey); cached {
		if blockedRule != "" {
			return SellEvaluation{Allowed: false, Reason: "scrub_blocked:" + blockedRule}
		}
	} else {
		sr := policy.Scrub(f.Text)
		rule := ""
		if sr.Verdict == policy.ScrubBlocked {
			rule = "secret"
			if len(sr.BlockedRules) > 0 {
				rule = sr.BlockedRules[0]
			}
		}
		putScrubVerdictCache(scrubKey, rule)
		if rule != "" {
			return SellEvaluation{Allowed: false, Reason: "scrub_blocked:" + rule}
		}
	}
	if f.Tainted {
		if f.GradeMismatch || f.NMU == nil || nmu.EffectiveRights(f.NMU.UsageRights) != nmu.RightsDistillResellable {
			return SellEvaluation{Allowed: false, Reason: "tainted_resale_forbidden"}
		}
	}
	if f.NMU != nil && nmu.IsExpired(f.NMU.FreshnessClass, f.NMU.ValidUntilUnix, nowUnix) {
		return SellEvaluation{Allowed: false, Reason: "nmu_expired"}
	}

	var effRights nmu.UsageRights
	var effFresh nmu.FreshnessClass
	if listing != nil {
		effRights = nmu.EffectiveRights(nmu.UsageRights(listing.UsageRights))
		effFresh = nmu.NormalizeFreshness(listing.FreshnessClass)
	} else {
		effRights = nmu.RightsInferenceOnly
		effFresh = nmu.FreshnessEvergreen
	}
	var validUntil int64
	var subjVer string
	if f.NMU != nil {
		if f.NMU.UsageRights != "" {
			effRights = nmu.EffectiveRights(f.NMU.UsageRights)
		}
		if f.NMU.FreshnessClass != "" {
			effFresh = nmu.NormalizeFreshness(string(f.NMU.FreshnessClass))
		}
		validUntil = f.NMU.ValidUntilUnix
		subjVer = f.NMU.SubjectVersion
	}

	cls, ontID, edgesRoot := s.ClassifyFragmentNMU(ctx, f.Shard, f.Seq, f.Distilled, f.OntologyID, hasActiveBond, liveFeedURL)
	if listing != nil && listing.MinAdvertisedGrade != "" && !nmu.MeetsMinGrade(cls.Grade, nmu.Grade(listing.MinAdvertisedGrade)) {
		return SellEvaluation{Allowed: false, Reason: "below_listing_min_grade"}
	}
	return SellEvaluation{
		Allowed:        true,
		Classification: cls,
		OntologyID:     ontID,
		EdgesRoot:      edgesRoot,
		UsageRights:    effRights,
		FreshnessClass: effFresh,
		ValidUntilUnix: validUntil,
		SubjectVersion: subjVer,
	}
}
