package distill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/jev"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

const (
	DefaultMaxBatch        = 32
	DefaultMarketPriceSats = uint64(50)
	MarketShardPrefix      = "market-"
)

// Config configures the autonomous SleepCycle distillation & consolidation pipeline.
type Config struct {
	DefaultDomain    string        // fallback ontology domain (default "ops")
	MaxBatch         int           // max raw fragments processed per cycle (default 32)
	AutoAnchor       bool          // anchor newly distilled market-<domain> shards when wallet funded
	AutoList         bool          // automatically arm market-<domain> SellListings (with AllowRaw: false)
	DefaultPriceSats uint64        // initial dynamic ask price for newly created market-* listings (default 50)
	JevEval          jev.Evaluator // optional self-hosted Jev evaluator (falls back deterministically offline)
}

// RunOpts configures one invocation of Distiller.RunCycle.
type RunOpts struct {
	Shards     []string // optional shard allowlist (empty = all non-system working shards)
	MaxBatch   int      // optional override
	AutoAnchor *bool    // optional override
	AutoList   *bool    // optional override
}

// CycleSummary captures deterministic telemetry for one SleepCycle distillation pass.
type CycleSummary struct {
	Scanned          int      `json:"scanned"`
	Distilled        int      `json:"distilled"`
	Quarantined      int      `json:"quarantined"`
	DroppedLowSignal int      `json:"droppedLowSignal"`
	RejectedConflict int      `json:"rejectedConflict"`
	Superseded       int      `json:"superseded"`
	MarketShards     []string `json:"marketShards,omitempty"`
	Errors           []string `json:"errors,omitempty"`
	RanAtUnix        int64    `json:"ranAtUnix"`
}

// Distiller transforms noisy Level-0 private working shards into PII-scrubbed, ontology-pinned,
// bi-temporally consolidated `market-<domain>` sellable shards.
type Distiller struct {
	mem *memory.Service
	cfg Config
}

// NewDistiller constructs a Distiller bound to mem.
func NewDistiller(mem *memory.Service, cfg Config) (*Distiller, error) {
	if mem == nil {
		return nil, errors.New("distill: memory service is required")
	}
	if strings.TrimSpace(cfg.DefaultDomain) == "" {
		cfg.DefaultDomain = "ops"
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = DefaultMaxBatch
	}
	if cfg.DefaultPriceSats == 0 {
		cfg.DefaultPriceSats = DefaultMarketPriceSats
	}
	return &Distiller{mem: mem, cfg: cfg}, nil
}

// IsWorkingShard reports whether shard is an eligible private working shard (excluding system,
// market, ontology, bridge, and curated shards).
func IsWorkingShard(shard string) bool {
	s := strings.ToLower(strings.TrimSpace(shard))
	if s == "" || memory.IsSystemShard(s) {
		return false
	}
	for _, pref := range []string{"market-", "market/", "ont-", "bridge-", "curated/", "ops/"} {
		if strings.HasPrefix(s, pref) {
			return false
		}
	}
	return true
}

// RunCycle scans unprocessed fragments across working shards, enforces the fail-closed PII/secret gate,
// extracts ontology-pinned triples, supersedes stale facts in `market-<domain>`, and optionally anchors/lists.
func (d *Distiller) RunCycle(ctx context.Context, opts RunOpts) (*CycleSummary, error) {
	now := time.Now().Unix()
	sum := &CycleSummary{RanAtUnix: now}

	maxBatch := d.cfg.MaxBatch
	if opts.MaxBatch > 0 {
		maxBatch = opts.MaxBatch
	}
	autoAnchor := d.cfg.AutoAnchor
	if opts.AutoAnchor != nil {
		autoAnchor = *opts.AutoAnchor
	}
	autoList := d.cfg.AutoList
	if opts.AutoList != nil {
		autoList = *opts.AutoList
	}

	shards := opts.Shards
	if len(shards) == 0 {
		all, err := d.mem.DB().ListShards()
		if err != nil {
			return nil, fmt.Errorf("distill: list shards: %w", err)
		}
		for _, sh := range all {
			if IsWorkingShard(sh) {
				shards = append(shards, sh)
			}
		}
	}
	sort.Strings(shards)

	touchedMarkets := make(map[string]string) // marketShard -> ontologyID
	eval := d.cfg.JevEval
	if eval == nil {
		eval = d.mem.JevEvaluator()
	}

	// Owner-level-0 principal: reads local Level-0 CLKA working shards while strictly blocking Level > 0 & Borg secrets!
	distillPrincipal := policy.Principal{Type: "Agent", ID: "sleep-distiller", Level: 0}

	for _, shard := range shards {
		if sum.Scanned >= maxBatch {
			break
		}
		if !IsWorkingShard(shard) {
			continue
		}
		cursor, cErr := d.mem.DB().GetDistillCursor(shard)
		if cErr != nil {
			sum.Errors = append(sum.Errors, fmt.Sprintf("%s: cursor: %v", shard, cErr))
			continue
		}
		recalled, rErr := d.mem.Recall(ctx, memory.RecallInput{
			Shard:        shard,
			Principal:    distillPrincipal,
			Action:       "read",
			DisallowBorg: true,
		})
		if rErr != nil {
			sum.Errors = append(sum.Errors, fmt.Sprintf("%s: recall: %v", shard, rErr))
			continue
		}

		domain := inferDomainFromShard(shard, d.cfg.DefaultDomain)
		manifest, ontID := d.resolveOrEnsureOntology(ctx, domain)
		marketShard := MarketShardPrefix + domain

		var maxSeenSeq int64 = cursor
		for _, frag := range recalled {
			if frag.Seq <= cursor {
				continue
			}
			if sum.Scanned >= maxBatch {
				break
			}
			sum.Scanned++
			if frag.Seq > maxSeenSeq {
				maxSeenSeq = frag.Seq
			}
			// Never promote high-clearance, Borg-group, or tainted external market fragments back onto the market.
			if frag.Level > 0 || frag.BorgGroup != "" || frag.Tainted {
				continue
			}

			// Stage 1: Fail-Closed Secret & PII Gate (gitleaks + explicit rules + PII/path redaction).
			scrub := Scrub(frag.Text)
			switch scrub.Verdict {
			case ScrubBlocked:
				sum.Quarantined++
				ruleID := "secret-detected"
				if len(scrub.BlockedRules) > 0 {
					ruleID = scrub.BlockedRules[0]
				}
				_ = d.mem.DB().RecordDistillQuarantine(store.DistillQuarantineRecord{
					ID:          fmt.Sprintf("q-%s-%d-%s", shard, frag.Seq, ruleID),
					Shard:       shard,
					Seq:         frag.Seq,
					RuleID:      ruleID,
					ContentHash: scrub.ContentHashHex,
					CreatedAt:   now,
				})
				continue
			case ScrubDroppedLowSignal:
				sum.DroppedLowSignal++
				continue
			}

			cleanText := strings.TrimSpace(scrub.SanitizedText)
			if cleanText == "" {
				sum.DroppedLowSignal++
				continue
			}

			// Stage 2: O(n) Aho-Corasick Blocking + JevGraph Extraction + Mangle Consistency Gate.
			graphRes, gErr := jev.ExtractGraph(ctx, eval, jev.ExtractGraphInput{
				Text:     cleanText,
				Manifest: manifest,
			})
			if gErr != nil {
				sum.Errors = append(sum.Errors, fmt.Sprintf("%s#%d: extract: %v", shard, frag.Seq, gErr))
				continue
			}
			if !graphRes.Consistent {
				sum.RejectedConflict++
				continue
			}

			triples := make([]reasoning.Triple, 0, len(graphRes.Triples))
			for _, st := range graphRes.Triples {
				triples = append(triples, st.Triple)
			}

			// Stage 3: Bi-temporal AORG supersession check against active facts in market-<domain>.
			supSeq := d.findSupersededMarketSeq(marketShard, triples)
			var refines []core.RefinementEdge
			if supSeq > 0 {
				refines = append(refines, core.RefinementEdge{
					Kind:        core.RefinementSupersedes,
					TargetShard: marketShard,
					TargetSeq:   supSeq,
				})
			}

			// Stage 4: Commit distilled, PII-clean, ontology-pinned fragment to market-<domain>.
			rem, mErr := d.mem.Remember(ctx, memory.RememberInput{
				Shard:         marketShard,
				Text:          cleanText,
				Triples:       triples,
				CommitMessage: fmt.Sprintf("distill: %s#%d -> %s (%d triples)", shard, frag.Seq, marketShard, len(triples)),
				Source:        "sleep-cycle:" + shard,
				Class:         "distilled:" + domain,
				Level:         0,
				Public:        true,
				Distilled:     true,
				Supersedes:    supSeq,
				Refines:       refines,
				Extra:         map[string]string{"ontologyId": ontID},
			})
			if mErr != nil {
				sum.Errors = append(sum.Errors, fmt.Sprintf("%s#%d: commit market: %v", shard, frag.Seq, mErr))
				continue
			}
			if !rem.Dedup {
				sum.Distilled++
				if supSeq > 0 {
					sum.Superseded++
				}
				touchedMarkets[marketShard] = ontID
			}
		}

		if maxSeenSeq > cursor {
			_ = d.mem.DB().SetDistillCursor(shard, maxSeenSeq)
		}
	}

	// Stage 5: Optional anchoring & auto-listing of updated market-<domain> shards (strictly AllowRaw: false).
	for marketShard, ontID := range touchedMarkets {
		sum.MarketShards = append(sum.MarketShards, marketShard)
		if autoAnchor && d.mem.Chain().BalanceSats() > 1000 {
			if _, aErr := d.mem.AnchorShard(ctx, marketShard); aErr != nil {
				sum.Errors = append(sum.Errors, fmt.Sprintf("%s: anchor: %v", marketShard, aErr))
			}
		}
		if autoList {
			domain := strings.TrimPrefix(marketShard, MarketShardPrefix)
			existing, _ := d.mem.DB().ListSellListings()
			alreadyListed := false
			for _, l := range existing {
				if l.Shard == marketShard {
					alreadyListed = true
					break
				}
			}
			if !alreadyListed {
				dynJSON, _ := json.Marshal(map[string]any{
					"enabled":          true,
					"floorSats":        maxU64(10, d.cfg.DefaultPriceSats/2),
					"ceilingSats":      d.cfg.DefaultPriceSats * 4,
					"currentPriceSats": d.cfg.DefaultPriceSats,
					"decayIntervalSec": 86400,
				})
				_ = d.mem.DB().SaveSellListing(store.SellListing{
					Shard:              marketShard,
					Domains:            []string{domain},
					PriceSats:          d.cfg.DefaultPriceSats,
					Description:        fmt.Sprintf("Auto-distilled PII-scrubbed %s knowledge", domain),
					OntologyID:         ontID,
					AllowRaw:           false,
					DynamicPricingJSON: dynJSON,
					Active:             true,
				})
			}
		}
	}
	sort.Strings(sum.MarketShards)
	return sum, nil
}

func (d *Distiller) resolveOrEnsureOntology(ctx context.Context, domain string) (reasoning.OntologyManifest, string) {
	if loaded, err := d.mem.ResolveOntology(ctx, domain); err == nil && loaded != nil {
		return loaded.Manifest, loaded.Ref.OntologyID
	}
	man := reasoning.DefaultOntologyManifest(domain)
	if loaded, err := d.mem.CommitOntology(ctx, man, false); err == nil && loaded != nil {
		return loaded.Manifest, loaded.Ref.OntologyID
	}
	fin, _, _, err := reasoning.FinalizeOntologyManifest(man)
	if err == nil && fin != nil {
		return *fin, reasoning.CanonicalOntologyID(domain, "", fin.Hash)
	}
	return man, ""
}

func (d *Distiller) findSupersededMarketSeq(marketShard string, incoming []reasoning.Triple) int64 {
	if len(incoming) == 0 {
		return 0
	}
	frags, err := d.mem.DB().ListFragments(marketShard)
	if err != nil || len(frags) == 0 {
		return 0
	}
	superseded := d.mem.CollectSupersededSeqs(marketShard)
	for i := len(frags) - 1; i >= 0; i-- {
		f := frags[i]
		if f.Forgotten || superseded[f.Seq] > 0 || len(f.TriplesJSON) == 0 {
			continue
		}
		var prev []reasoning.Triple
		if json.Unmarshal(f.TriplesJSON, &prev) != nil {
			continue
		}
		for _, pt := range prev {
			for _, nt := range incoming {
				if strings.EqualFold(pt.Subject, nt.Subject) &&
					strings.EqualFold(pt.Predicate, nt.Predicate) &&
					!strings.EqualFold(pt.Object, nt.Object) {
					return f.Seq
				}
			}
		}
	}
	return 0
}

func inferDomainFromShard(shard, fallback string) string {
	s := strings.ToLower(strings.TrimSpace(shard))
	for _, known := range []string{"bsv", "cve", "sec", "security", "agents", "golang", "go", "ops", "rust", "ts"} {
		if s == known || strings.HasPrefix(s, known+"-") || strings.HasPrefix(s, known+"/") || strings.HasSuffix(s, "-"+known) {
			if known == "sec" {
				return "security"
			}
			if known == "go" {
				return "golang"
			}
			return known
		}
	}
	return reasoning.NormalizeOntologyDomain(fallback)
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
