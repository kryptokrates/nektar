package memory

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/jev"
	"github.com/kryptokrates/nektar/internal/jevgraph"
	"github.com/kryptokrates/nektar/internal/lattice"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

type cachedGoGitProjection struct {
	proj    *memrepo.GoGitProjection
	commits []memrepo.CommitObject
	trees   map[string]memrepo.TreeObject
	blobs   map[string]memrepo.BlobObject
}

const (
	ReceiptsShard = "__receipts__"
	DealsShard    = "__deals__"
	PoliciesShard = "__policies__"
	OntologyShard = "__ontology__"
)

// IsSystemShard reports whether shard is a reserved internal audit shard anchored by NektarAnchor.
func IsSystemShard(shard string) bool {
	s := strings.TrimSpace(shard)
	return s == ReceiptsShard || s == DealsShard || s == PoliciesShard || s == OntologyShard
}

// ValidateShardName enforces safe, bounded shard names and blocks external access to reserved system shards.
func ValidateShardName(shard string, allowSystem bool) error {
	s := strings.TrimSpace(shard)
	if s == "" {
		return errors.New("memory: shard name is required")
	}
	if len(s) > 128 {
		return errors.New("memory: shard name exceeds 128 characters")
	}
	if IsSystemShard(s) {
		if allowSystem {
			return nil
		}
		return errors.New("memory: reserved system shard cannot be targeted directly")
	}
	if strings.Contains(s, "..") || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") {
		return fmt.Errorf("memory: invalid shard name %q", shard)
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '/' || c == '-') {
			return fmt.Errorf("memory: invalid character in shard name %q", shard)
		}
	}
	return nil
}

// SupersessionEvent notifies subscribers (Grade S live SSE feeds) when a new fragment supersedes an older one.
type SupersessionEvent struct {
	Shard         string `json:"shard"`
	NewSeq        int64  `json:"newSeq"`
	NewLeaf       string `json:"newLeaf"`
	NewHDP        string `json:"newHdp"`
	SupersededSeq int64  `json:"supersededSeq"`
	OntologyID    string `json:"ontologyId,omitempty"`
	EdgesRoot     string `json:"edgesRoot,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Timestamp     int64  `json:"timestamp"`
}

// Config configures a NektarMemory service instance.
type Config struct {
	PolicyText    string
	DatalogRules  []string
	VerifyBaseURL string // e.g. http://127.0.0.1:8080/v1/verify
	ModelID       reasoning.ModelIdentity
	JevEvaluator  jev.Evaluator
}

// OntologyCommitResult summarizes a batch of DL-Lite / OWL 2 RL / deontic axioms committed to __ontology__.
type OntologyCommitResult struct {
	OntologyHead string                    `json:"ontologyHead"`
	Axioms       []reasoning.OntologyAxiom `json:"axioms"`
	RuleRefs     []reasoning.FragmentRef   `json:"ruleRefs"`
	AnchorTxID   string                    `json:"anchorTxid,omitempty"`
}

// Service is the unified v0.1 NektarMemory engine composing:
//   - pure-Go SQLite persistence (internal/store),
//   - BSV chain + BRC-100 custody + SpendGuard (internal/chain),
//   - Rúnar NektarAnchor Merkle Automaton + SPV/MMR verification (internal/contracts),
//   - Cryptographic Lattice Key Assignment (CLKA) private-by-default encryption (internal/lattice),
//   - Lightweb Borg/BRC-369 encryption + CHIRP object storage (internal/bstack),
//   - native AWS Cedar authorization anchored in __policies__ (internal/policy),
//   - Google Mangle Datalog + DL-Lite/OWL 2 RL ontology in __ontology__ + proof-carrying nektarr1 receipts (internal/reasoning).
type Service struct {
	mu             sync.RWMutex
	keyMu          sync.Mutex
	hookWG         sync.WaitGroup
	shardLocks     [64]sync.Mutex
	publicChirpMu  sync.RWMutex
	publicChirps   map[[32]byte]struct{}
	projCacheMu    sync.RWMutex
	projCache      map[string]*cachedGoGitProjection
	db             *store.DB
	port           *chain.Port
	groups         *bstack.GroupRegistry
	lattice        *lattice.Manager
	objects        bstack.ObjectStore
	policyText     string
	policyCommit   *policy.PolicyCommitment
	policyRef      *reasoning.FragmentRef
	rules          []string
	committedRules []reasoning.CommittedRule
	ontologyHead   string
	verifyBaseURL  string
	modelID        reasoning.ModelIdentity
	onRemember       func(shard string)
	onSupersede      func(ev SupersessionEvent)
	jevEval          jev.Evaluator
	ontCacheMu       sync.RWMutex
	ontCache         map[string]*jevgraph.CompiledOntology
	lastActivityUnix atomic.Int64
}

// New constructs a NektarMemory Service.
func New(db *store.DB, port *chain.Port, groups *bstack.GroupRegistry, objects bstack.ObjectStore, cfg Config) (*Service, error) {
	if db == nil || port == nil {
		return nil, errors.New("memory: db and chain port are required")
	}
	if groups == nil {
		groups = bstack.NewGroupRegistry()
	}
	if objects == nil {
		objects = bstack.NewMemoryObjectStore()
	}
	polName := "default"
	pol := cfg.PolicyText
	if strings.TrimSpace(pol) == "" {
		var saved struct {
			Name string `json:"name"`
			Text string `json:"text"`
		}
		if ok, _ := db.GetJSON("cedar_policy_v1", &saved); ok && strings.TrimSpace(saved.Text) != "" {
			pol = saved.Text
			if saved.Name != "" {
				polName = saved.Name
			}
		} else {
			pol = policy.DefaultPolicySet
		}
	}
	pc, err := policy.Canonicalize(polName, pol)
	if err != nil {
		return nil, err
	}
	rules := append([]string(nil), cfg.DatalogRules...)
	if len(rules) == 0 {
		var savedRules []string
		if ok, _ := db.GetJSON("mangle_rules_v1", &savedRules); ok && len(savedRules) > 0 {
			rules = savedRules
		}
	}
	baseURL := strings.TrimRight(cfg.VerifyBaseURL, "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:18420/v1/verify"
	}
	mid := cfg.ModelID
	if mid.Kind == "" {
		mid = reasoning.ModelIdentity{Kind: "api", Provider: "nektar", Release: "v0.1"}
	}
	if err := groups.SetPersistence(&dbGroupStore{db: db, port: port}); err != nil {
		return nil, fmt.Errorf("memory: load borg groups: %w", err)
	}
	latMgr := lattice.NewManager(port.DeriveLatticeRoot)
	if err := latMgr.SetPersistence(db); err != nil {
		return nil, fmt.Errorf("memory: load lattice states: %w", err)
	}
	pubChirps := make(map[[32]byte]struct{})
	var savedPub []string
	if ok, _ := db.GetJSON("public_chirp_hashes_v1", &savedPub); ok {
		for _, hx := range savedPub {
			if b, err := hex.DecodeString(strings.TrimSpace(hx)); err == nil && len(b) == 32 {
				var arr [32]byte
				copy(arr[:], b)
				pubChirps[arr] = struct{}{}
			}
		}
	}
	svc := &Service{
		publicChirps:  pubChirps,
		db:            db,
		port:          port,
		groups:        groups,
		lattice:       latMgr,
		objects:       objects,
		policyText:    pol,
		policyCommit:  pc,
		rules:         rules,
		verifyBaseURL: baseURL,
		modelID:       mid,
		jevEval:       cfg.JevEvaluator,
		ontCache:      make(map[string]*jevgraph.CompiledOntology),
	}
	if err := svc.hydrateLawShards(context.Background(), polName, pol, pc, rules); err != nil {
		return nil, fmt.Errorf("memory: hydrate law shards: %w", err)
	}
	if err := svc.backfillLegacyRepos(context.Background()); err != nil {
		return nil, fmt.Errorf("memory: backfill legacy repos: %w", err)
	}
	return svc, nil
}

// hydrateLawShards loads previously committed __policies__ and __ontology__ fragments on startup
// and ensures the active Cedar policy and any configured Mangle rules are committed as immutable memory.
func (s *Service) hydrateLawShards(ctx context.Context, polName, polText string, pc *policy.PolicyCommitment, initRules []string) error {
	// 1. Hydrate __policies__ ref matching pc.Hash (or commit initial policy if absent).
	if polFrags, err := s.db.ListFragments(PoliciesShard); err == nil {
		for i := len(polFrags) - 1; i >= 0; i-- {
			f := polFrags[i]
			if f.Forgotten || len(f.Data) == 0 {
				continue
			}
			var rec struct {
				Hash string `json:"hash"`
			}
			if json.Unmarshal(f.Data, &rec) == nil && strings.EqualFold(rec.Hash, pc.Hash) {
				s.policyRef = &reasoning.FragmentRef{Shard: PoliciesShard, Seq: f.Seq}
				break
			}
		}
	}
	if s.policyRef == nil {
		stored, _, err := s.CommitSystemAuditRecord(ctx, PoliciesShard, "cedar-policy", map[string]any{
			"name":      polName,
			"hash":      pc.Hash,
			"canonical": pc.Text,
		}, false)
		if err != nil {
			return fmt.Errorf("commit initial policy to %s: %w", PoliciesShard, err)
		}
		if stored != nil {
			s.policyRef = &reasoning.FragmentRef{Shard: PoliciesShard, Seq: stored.Seq}
		}
	}

	// 2. Hydrate __ontology__ committed rules.
	seenRuleHash := make(map[string]bool)
	if ontFrags, err := s.db.ListFragments(OntologyShard); err == nil {
		for _, f := range ontFrags {
			if f.Forgotten || len(f.Data) == 0 {
				continue
			}
			var ax struct {
				CompiledRule  string   `json:"compiledRule"`
				CompiledRules []string `json:"compiledRules"`
				RuleText      string   `json:"ruleText"`
				RuleHash      string   `json:"ruleHash"`
			}
			if json.Unmarshal(f.Data, &ax) != nil {
				continue
			}
			var list []string
			if len(ax.CompiledRules) > 0 {
				list = ax.CompiledRules
			} else if strings.TrimSpace(ax.CompiledRule) != "" {
				list = []string{strings.TrimSpace(ax.CompiledRule)}
			} else if strings.TrimSpace(ax.RuleText) != "" {
				list = []string{strings.TrimSpace(ax.RuleText)}
			}
			for _, rText := range list {
				rText = strings.TrimSpace(rText)
				if rText == "" {
					continue
				}
				rh := reasoning.ComputeRuleHash(rText)
				if seenRuleHash[rh] {
					continue
				}
				seenRuleHash[rh] = true
				ref := reasoning.FragmentRef{Shard: OntologyShard, Seq: f.Seq}
				s.committedRules = append(s.committedRules, reasoning.CommittedRule{
					RuleText: rText,
					Ref:      &ref,
					RuleHash: rh,
				})
			}
		}
	}
	// Ensure any initial rules from Config or kv are committed into __ontology__.
	for _, r := range initRules {
		trimmed := strings.TrimSpace(r)
		if trimmed == "" {
			continue
		}
		rh := reasoning.ComputeRuleHash(trimmed)
		if seenRuleHash[rh] {
			continue
		}
		stored, _, err := s.CommitSystemAuditRecord(ctx, OntologyShard, "mangle-rule", map[string]any{
			"kind":          string(reasoning.AxiomMangleRule),
			"compiledRule":  trimmed,
			"compiledRules": []string{trimmed},
			"ruleHash":      rh,
		}, false)
		if err != nil {
			return fmt.Errorf("commit initial rule to %s: %w", OntologyShard, err)
		}
		seenRuleHash[rh] = true
		ref := reasoning.FragmentRef{Shard: OntologyShard, Seq: stored.Seq}
		s.committedRules = append(s.committedRules, reasoning.CommittedRule{
			RuleText: trimmed,
			Ref:      &ref,
			RuleHash: rh,
		})
	}
	s.syncRulesFromCommittedLocked()
	return nil
}

func (s *Service) syncRulesFromCommittedLocked() {
	rules := make([]string, 0, len(s.committedRules))
	var headBuf []byte
	headBuf = append(headBuf, core.TagOntologyV1...)
	for _, cr := range s.committedRules {
		rules = append(rules, cr.RuleText)
		headBuf = append(headBuf, []byte(cr.RuleHash)...)
	}
	s.rules = rules
	if len(s.committedRules) == 0 {
		s.ontologyHead = ""
	} else {
		s.ontologyHead = core.ToHex(core.SHA256(headBuf))
	}
}

// AllowPublicChirpHashes registers CHIRP DAG object hashes that may be fetched unauthenticated via /v1/chirp/{hash}
// and persists the allowlist in SQLite so intentionally published packs survive daemon restarts.
func (s *Service) AllowPublicChirpHashes(hashes ...[32]byte) {
	if len(hashes) == 0 {
		return
	}
	s.publicChirpMu.Lock()
	added := false
	for _, h := range hashes {
		if _, exists := s.publicChirps[h]; !exists {
			s.publicChirps[h] = struct{}{}
			added = true
		}
	}
	var persist []string
	if added && s.db != nil {
		persist = make([]string, 0, len(s.publicChirps))
		for h := range s.publicChirps {
			persist = append(persist, hex.EncodeToString(h[:]))
		}
		sort.Strings(persist)
	}
	s.publicChirpMu.Unlock()
	if added && s.db != nil {
		_ = s.db.SetJSON("public_chirp_hashes_v1", persist)
	}
}

// RevokePublicChirpHashes removes CHIRP DAG object hashes from the public unauthenticated allowlist
// and updates SQLite persistence (called when shredding forgotten fragments).
func (s *Service) RevokePublicChirpHashes(hashes ...[32]byte) {
	if len(hashes) == 0 {
		return
	}
	s.publicChirpMu.Lock()
	removed := false
	for _, h := range hashes {
		if _, exists := s.publicChirps[h]; exists {
			delete(s.publicChirps, h)
			removed = true
		}
	}
	var persist []string
	if removed && s.db != nil {
		persist = make([]string, 0, len(s.publicChirps))
		for h := range s.publicChirps {
			persist = append(persist, hex.EncodeToString(h[:]))
		}
		sort.Strings(persist)
	}
	s.publicChirpMu.Unlock()
	if removed && s.db != nil {
		_ = s.db.SetJSON("public_chirp_hashes_v1", persist)
	}
}

// IsPublicChirp reports whether hash h belongs to an intentionally published public CHIRP closure.
func (s *Service) IsPublicChirp(h [32]byte) bool {
	s.publicChirpMu.RLock()
	_, ok := s.publicChirps[h]
	s.publicChirpMu.RUnlock()
	return ok
}

func (s *Service) lockShard(shard string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(shard))
	m := &s.shardLocks[h.Sum32()%uint32(len(s.shardLocks))]
	m.Lock()
	return m.Unlock
}

// SetOnRemember registers an optional hook invoked whenever a new fragment is committed to a shard
// (used to trigger durable Restate AnchorCadence scheduling).
func (s *Service) SetOnRemember(fn func(shard string)) {
	s.mu.Lock()
	s.onRemember = fn
	s.mu.Unlock()
}

// SetOnSupersede registers an optional hook invoked whenever a non-dedup fragment supersedes
// an older fragment via Supersedes or core.RefinementSupersedes (powering Grade S live SSE feeds).
func (s *Service) SetOnSupersede(fn func(ev SupersessionEvent)) {
	s.mu.Lock()
	s.onSupersede = fn
	s.mu.Unlock()
}

func (s *Service) notifyRemember(shard string) {
	if IsSystemShard(shard) {
		return
	}
	s.mu.RLock()
	hook := s.onRemember
	s.mu.RUnlock()
	if hook != nil {
		go hook(shard)
	}
}

// Close waits for all in-flight asynchronous onRemember hooks to finish draining.
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.hookWG.Wait()
	return nil
}

func (s *Service) DB() *store.DB                 { return s.db }
func (s *Service) Chain() *chain.Port            { return s.port }
func (s *Service) Groups() *bstack.GroupRegistry { return s.groups }
func (s *Service) Lattice() *lattice.Manager     { return s.lattice }
func (s *Service) Objects() bstack.ObjectStore   { return s.objects }

// SetJevEvaluator attaches or replaces the self-hosted System One evaluator used by JevGraph extraction & alignment.
func (s *Service) SetJevEvaluator(eval jev.Evaluator) {
	s.mu.Lock()
	s.jevEval = eval
	s.mu.Unlock()
}

// JevEvaluator returns the configured System One evaluator, or nil when running offline/deterministic.
func (s *Service) JevEvaluator() jev.Evaluator {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.jevEval
}

func (s *Service) PolicyCommit() *policy.PolicyCommitment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.policyCommit
}

func (s *Service) PolicyRef() *reasoning.FragmentRef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.policyRef == nil {
		return nil
	}
	cp := *s.policyRef
	return &cp
}

func (s *Service) OntologyHead() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ontologyHead
}

func (s *Service) CommittedRules() []reasoning.CommittedRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]reasoning.CommittedRule, len(s.committedRules))
	copy(out, s.committedRules)
	return out
}

// SetPolicy updates the active Cedar policy set atomically, commits it into the __policies__ audit
// shard as immutable verifiable memory, and persists it to SQLite. Fails closed on any error.
func (s *Service) SetPolicy(name, text string) (*policy.PolicyCommitment, error) {
	pc, err := policy.Canonicalize(name, text)
	if err != nil {
		return nil, err
	}
	stored, _, err := s.CommitSystemAuditRecord(context.Background(), PoliciesShard, "cedar-policy", map[string]any{
		"name":      name,
		"hash":      pc.Hash,
		"canonical": pc.Text,
	}, false)
	if err != nil {
		return nil, fmt.Errorf("memory: commit cedar policy to %s: %w", PoliciesShard, err)
	}
	if err := s.db.SetJSON("cedar_policy_v1", map[string]string{"name": name, "text": text}); err != nil {
		return nil, fmt.Errorf("memory: persist cedar policy: %w", err)
	}
	s.mu.Lock()
	s.policyText = text
	s.policyCommit = pc
	if stored != nil {
		s.policyRef = &reasoning.FragmentRef{Shard: PoliciesShard, Seq: stored.Seq}
	}
	s.mu.Unlock()
	return pc, nil
}

// AddRules appends Mangle Datalog rules, commits each rule as an immutable fragment in __ontology__,
// and persists rules to SQLite. Fails closed on any error so RAM and disk never diverge.
func (s *Service) AddRules(rules ...string) error {
	ctx := context.Background()
	var added []reasoning.CommittedRule
	for _, r := range rules {
		trimmed := strings.TrimSpace(r)
		if trimmed == "" {
			continue
		}
		rh := reasoning.ComputeRuleHash(trimmed)
		stored, _, err := s.CommitSystemAuditRecord(ctx, OntologyShard, "mangle-rule", map[string]any{
			"kind":          string(reasoning.AxiomMangleRule),
			"compiledRule":  trimmed,
			"compiledRules": []string{trimmed},
			"ruleHash":      rh,
		}, false)
		if err != nil {
			return fmt.Errorf("memory: commit rule to %s: %w", OntologyShard, err)
		}
		ref := reasoning.FragmentRef{Shard: OntologyShard, Seq: stored.Seq}
		added = append(added, reasoning.CommittedRule{
			RuleText: trimmed,
			Ref:      &ref,
			RuleHash: rh,
		})
	}
	s.mu.Lock()
	seen := make(map[string]bool, len(s.committedRules))
	for _, cr := range s.committedRules {
		seen[cr.RuleHash] = true
	}
	nextCommitted := append([]reasoning.CommittedRule(nil), s.committedRules...)
	for _, cr := range added {
		if !seen[cr.RuleHash] {
			seen[cr.RuleHash] = true
			nextCommitted = append(nextCommitted, cr)
		}
	}
	nextRules := make([]string, 0, len(nextCommitted))
	for _, cr := range nextCommitted {
		nextRules = append(nextRules, cr.RuleText)
	}
	s.mu.Unlock()

	if err := s.db.SetJSON("mangle_rules_v1", nextRules); err != nil {
		return fmt.Errorf("memory: persist mangle rules: %w", err)
	}
	s.mu.Lock()
	s.committedRules = nextCommitted
	s.syncRulesFromCommittedLocked()
	s.mu.Unlock()
	return nil
}

// CommitOntologyAxioms compiles DL-Lite / OWL 2 RL / deontic axioms into stratified Mangle rules,
// verifies consistency, commits each axiom into __ontology__, updates OntologyHead, and optionally anchors __ontology__.
func (s *Service) CommitOntologyAxioms(ctx context.Context, axioms []reasoning.OntologyAxiom, autoAnchor bool) (*OntologyCommitResult, error) {
	if len(axioms) == 0 {
		return nil, errors.New("memory: at least one ontology axiom is required")
	}
	type compiledAx struct {
		axiom reasoning.OntologyAxiom
		rules []string
	}
	finalized := make([]reasoning.OntologyAxiom, 0, len(axioms))
	compiledList := make([]compiledAx, 0, len(axioms))
	var allCompiledRules []string
	for i, ax := range axioms {
		f, rules, err := reasoning.FinalizeAxiom(ax)
		if err != nil {
			return nil, fmt.Errorf("memory: axiom %d invalid: %w", i, err)
		}
		finalized = append(finalized, f)
		compiledList = append(compiledList, compiledAx{axiom: f, rules: rules})
		allCompiledRules = append(allCompiledRules, rules...)
	}
	if rep, err := reasoning.CheckOntologyConsistency(nil, allCompiledRules); err != nil {
		return nil, fmt.Errorf("memory: ontology consistency check failed: %w", err)
	} else if rep != nil && !rep.Consistent {
		return nil, fmt.Errorf("memory: inconsistent ontology (conflicts=%v violations=%v)", rep.Conflicts, rep.DeonticViolations)
	}

	var added []reasoning.CommittedRule
	var refs []reasoning.FragmentRef
	for _, item := range compiledList {
		ax := item.axiom
		primaryRule := ""
		if len(item.rules) > 0 {
			primaryRule = item.rules[0]
		}
		stored, _, err := s.CommitSystemAuditRecord(ctx, OntologyShard, "ontology-axiom", map[string]any{
			"id":            ax.ID,
			"kind":          string(ax.Kind),
			"subject":       ax.Subject,
			"object":        ax.Object,
			"ruleText":      ax.RuleText,
			"compiledRule":  primaryRule,
			"compiledRules": item.rules,
			"axiomHash":     ax.Hash,
		}, false)
		if err != nil {
			return nil, fmt.Errorf("memory: commit axiom to %s: %w", OntologyShard, err)
		}
		ref := reasoning.FragmentRef{Shard: OntologyShard, Seq: stored.Seq}
		refs = append(refs, ref)
		for _, rt := range item.rules {
			rc := ref
			added = append(added, reasoning.CommittedRule{
				RuleText: rt,
				Ref:      &rc,
				RuleHash: reasoning.ComputeRuleHash(rt),
			})
		}
	}

	s.mu.Lock()
	seen := make(map[string]bool, len(s.committedRules))
	for _, cr := range s.committedRules {
		seen[cr.RuleHash] = true
	}
	nextCommitted := append([]reasoning.CommittedRule(nil), s.committedRules...)
	for _, cr := range added {
		if !seen[cr.RuleHash] {
			seen[cr.RuleHash] = true
			nextCommitted = append(nextCommitted, cr)
		}
	}
	nextRules := make([]string, 0, len(nextCommitted))
	for _, cr := range nextCommitted {
		nextRules = append(nextRules, cr.RuleText)
	}
	s.mu.Unlock()

	if err := s.db.SetJSON("mangle_rules_v1", nextRules); err != nil {
		return nil, fmt.Errorf("memory: persist ontology rules: %w", err)
	}
	s.mu.Lock()
	s.committedRules = nextCommitted
	s.syncRulesFromCommittedLocked()
	head := s.ontologyHead
	s.mu.Unlock()

	var anchorTxID string
	if autoAnchor && s.port != nil {
		batch, err := s.AnchorShard(ctx, OntologyShard)
		if err != nil {
			return nil, err
		}
		if batch != nil {
			anchorTxID = batch.TxID
		}
	}
	return &OntologyCommitResult{
		OntologyHead: head,
		Axioms:       finalized,
		RuleRefs:     refs,
		AnchorTxID:   anchorTxID,
	}, nil
}

// AuthorizeAction evaluates the active Cedar policy for any action ("read"|"write"|"disclose"|"rotate"|"revoke").
func (s *Service) AuthorizeAction(principal policy.Principal, action string, resource policy.Resource) (policy.Decision, error) {
	s.mu.RLock()
	polText := s.policyText
	s.mu.RUnlock()
	return policy.Authorize(polText, principal, action, resource)
}
