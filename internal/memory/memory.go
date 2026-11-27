package memory

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
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
)

// IsSystemShard reports whether shard is a reserved internal audit shard anchored by NektarAnchor.
func IsSystemShard(shard string) bool {
	s := strings.TrimSpace(shard)
	return s == ReceiptsShard || s == DealsShard || s == PoliciesShard
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

// Config configures a NektarMemory service instance.
type Config struct {
	PolicyText    string
	DatalogRules  []string
	VerifyBaseURL string // e.g. http://127.0.0.1:8080/v1/verify
	ModelID       reasoning.ModelIdentity
}

// Service is the unified v0.1 NektarMemory engine composing:
//   - pure-Go SQLite persistence (internal/store),
//   - BSV chain + BRC-100 custody + SpendGuard (internal/chain),
//   - Rúnar NektarAnchor + SPV verification (internal/contracts),
//   - Lightweb Borg/BRC-369 encryption + CHIRP object storage (internal/bstack),
//   - native AWS Cedar authorization (internal/policy),
//   - Google Mangle Datalog grounding + AORG + nektarr1 receipts (internal/reasoning).
type Service struct {
	mu            sync.RWMutex
	keyMu         sync.Mutex
	hookWG        sync.WaitGroup
	shardLocks    [64]sync.Mutex
	publicChirpMu sync.RWMutex
	publicChirps  map[[32]byte]struct{}
	projCacheMu   sync.RWMutex
	projCache     map[string]*cachedGoGitProjection
	db            *store.DB
	port          *chain.Port
	groups        *bstack.GroupRegistry
	objects       bstack.ObjectStore
	policyText    string
	policyCommit  *policy.PolicyCommitment
	rules         []string
	verifyBaseURL string
	modelID       reasoning.ModelIdentity
	onRemember    func(shard string)
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
		objects:       objects,
		policyText:    pol,
		policyCommit:  pc,
		rules:         rules,
		verifyBaseURL: baseURL,
		modelID:       mid,
	}
	if err := svc.backfillLegacyRepos(context.Background()); err != nil {
		return nil, fmt.Errorf("memory: backfill legacy repos: %w", err)
	}
	return svc, nil
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
func (s *Service) Objects() bstack.ObjectStore   { return s.objects }
func (s *Service) PolicyCommit() *policy.PolicyCommitment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.policyCommit
}

// SetPolicy updates the active Cedar policy set atomically and persists it to SQLite.
// Fails closed if SQLite persistence fails so RAM and disk never diverge.
func (s *Service) SetPolicy(name, text string) (*policy.PolicyCommitment, error) {
	pc, err := policy.Canonicalize(name, text)
	if err != nil {
		return nil, err
	}
	if err := s.db.SetJSON("cedar_policy_v1", map[string]string{"name": name, "text": text}); err != nil {
		return nil, fmt.Errorf("memory: persist cedar policy: %w", err)
	}
	s.mu.Lock()
	s.policyText = text
	s.policyCommit = pc
	s.mu.Unlock()
	return pc, nil
}

// AddRules appends Mangle Datalog rules and persists rules to SQLite.
// Fails closed if SQLite persistence fails so RAM and disk never diverge.
func (s *Service) AddRules(rules ...string) error {
	s.mu.Lock()
	nextRules := append(append([]string(nil), s.rules...), rules...)
	s.mu.Unlock()
	if err := s.db.SetJSON("mangle_rules_v1", nextRules); err != nil {
		return fmt.Errorf("memory: persist mangle rules: %w", err)
	}
	s.mu.Lock()
	s.rules = nextRules
	s.mu.Unlock()
	return nil
}

// AuthorizeAction evaluates the active Cedar policy for any action ("read"|"write"|"disclose"|"rotate"|"revoke").
func (s *Service) AuthorizeAction(principal policy.Principal, action string, resource policy.Resource) (policy.Decision, error) {
	s.mu.RLock()
	polText := s.policyText
	s.mu.RUnlock()
	return policy.Authorize(polText, principal, action, resource)
}
