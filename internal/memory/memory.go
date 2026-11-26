package memory

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

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
	shardLocks    [64]sync.Mutex
	publicChirpMu sync.RWMutex
	publicChirps  map[[32]byte]struct{}
	db            *store.DB
	port          *chain.Port
	groups        *bstack.GroupRegistry
	objects       bstack.ObjectStore
	policyText    string
	policyCommit  *policy.PolicyCommitment
	rules         []string
	verifyBaseURL string
	modelID       reasoning.ModelIdentity
	aorg          *reasoning.AORGraph
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
	aorg := reasoning.NewAORGraph(rules)
	var aorgSnap reasoning.AORGSnapshot
	if ok, _ := db.GetJSON("aorg_state_v1", &aorgSnap); ok && len(aorgSnap.Vertices) > 0 {
		aorg.Restore(aorgSnap)
		if len(rules) > 0 {
			aorg.AddRules(rules...)
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
	return &Service{
		publicChirps:  make(map[[32]byte]struct{}),
		db:            db,
		port:          port,
		groups:        groups,
		objects:       objects,
		policyText:    pol,
		policyCommit:  pc,
		rules:         rules,
		verifyBaseURL: baseURL,
		modelID:       mid,
		aorg:          aorg,
	}, nil
}

// AllowPublicChirpHashes registers CHIRP DAG object hashes that may be fetched unauthenticated via /v1/chirp/{hash}.
func (s *Service) AllowPublicChirpHashes(hashes ...[32]byte) {
	s.publicChirpMu.Lock()
	for _, h := range hashes {
		s.publicChirps[h] = struct{}{}
	}
	s.publicChirpMu.Unlock()
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

func (s *Service) DB() *store.DB                 { return s.db }
func (s *Service) Chain() *chain.Port            { return s.port }
func (s *Service) Groups() *bstack.GroupRegistry { return s.groups }
func (s *Service) Objects() bstack.ObjectStore   { return s.objects }
func (s *Service) AORG() *reasoning.AORGraph     { return s.aorg }
func (s *Service) PolicyCommit() *policy.PolicyCommitment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.policyCommit
}

// SetPolicy updates the active Cedar policy set atomically and persists it to SQLite.
func (s *Service) SetPolicy(name, text string) (*policy.PolicyCommitment, error) {
	pc, err := policy.Canonicalize(name, text)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.policyText = text
	s.policyCommit = pc
	s.mu.Unlock()
	_ = s.db.SetJSON("cedar_policy_v1", map[string]string{"name": name, "text": text})
	return pc, nil
}

// AddRules appends Mangle Datalog rules without wiping existing AORG vertices, and persists rules to SQLite.
func (s *Service) AddRules(rules ...string) {
	s.mu.Lock()
	s.rules = append(s.rules, rules...)
	allRules := append([]string(nil), s.rules...)
	s.aorg.AddRules(rules...)
	s.mu.Unlock()
	_ = s.db.SetJSON("mangle_rules_v1", allRules)
}

// AppendAORGVertex appends a vertex to the AORGraph and persists the graph snapshot to SQLite.
func (s *Service) AppendAORGVertex(v reasoning.Vertex, premises []string, ruleTag string) (reasoning.Vertex, error) {
	out, err := s.aorg.Append(v, premises, ruleTag)
	if err != nil {
		return reasoning.Vertex{}, err
	}
	_ = s.db.SetJSON("aorg_state_v1", s.aorg.Snapshot())
	return out, nil
}

// AuthorizeAction evaluates the active Cedar policy for any action ("read"|"write"|"disclose"|"rotate"|"revoke").
func (s *Service) AuthorizeAction(principal policy.Principal, action string, resource policy.Resource) (policy.Decision, error) {
	s.mu.RLock()
	polText := s.policyText
	s.mu.RUnlock()
	return policy.Authorize(polText, principal, action, resource)
}

// RememberInput describes a fragment to persist into a shard.
type RememberInput struct {
	Shard      string            `json:"shard"`
	Text       string            `json:"text"`
	Source     string            `json:"source,omitempty"`
	Level      int               `json:"level,omitempty"`
	Class      string            `json:"class,omitempty"`
	BorgGroup  string            `json:"borgGroup,omitempty"` // if set, seals via BRC-369 keyed under group epoch
	CallerPub  string            `json:"callerPub,omitempty"`
	Principal  *policy.Principal `json:"principal,omitempty"`
	Supersedes int64             `json:"supersedes,omitempty"`
	Tainted    bool              `json:"tainted,omitempty"`
	AutoAnchor bool              `json:"autoAnchor,omitempty"`
	Extra      map[string]string `json:"extra,omitempty"`
}

// RememberResult summarizes a persisted fragment.
type RememberResult struct {
	Shard            string   `json:"shard"`
	Seq              int64    `json:"seq"`
	HDP              string   `json:"hdp"`
	Leaf             string   `json:"leaf"`
	Encrypted        bool     `json:"encrypted"`
	BorgGroup        string   `json:"borgGroup,omitempty"`
	BorgEpoch        int64    `json:"borgEpoch,omitempty"`
	Dedup            bool     `json:"dedup"`
	AnchorTxID       string   `json:"anchorTxid,omitempty"`
	ImpactedReceipts []string `json:"impactedReceipts,omitempty"`
}

// Remember commits a provenance-signed fragment to a shard, optionally BRC-369 encrypting it
// to a Borg group epoch and/or anchoring immediately on-chain.
func (s *Service) Remember(ctx context.Context, in RememberInput) (*RememberResult, error) {
	if strings.TrimSpace(in.Shard) == "" {
		in.Shard = "default"
	}
	if err := ValidateShardName(in.Shard, false); err != nil {
		return nil, err
	}
	if in.Text == "" {
		return nil, errors.New("memory: text is required")
	}
	// Cedar write-gate enforcement (default owner level 100 when Principal omitted).
	writePrincipal := policy.Principal{Type: "Agent", ID: "owner", Level: 100}
	if in.Principal != nil && in.Principal.ID != "" {
		writePrincipal = *in.Principal
	}
	if dec, err := s.AuthorizeAction(writePrincipal, "write", policy.Resource{
		Type:  "Fragment",
		ID:    in.Shard + "#new",
		Level: int64(in.Level),
		Shard: in.Shard,
		Class: in.Class,
	}); err != nil || !dec.Allow {
		if err != nil {
			return nil, fmt.Errorf("memory: cedar write policy error: %w", err)
		}
		return nil, fmt.Errorf("memory: cedar policy denied write to shard %q (level %d > principal level %d)", in.Shard, in.Level, writePrincipal.Level)
	}
	src := in.Source
	if src == "" {
		src = "agent"
	}
	now := time.Now().Unix()
	prov := core.Provenance{
		Source:  src,
		Time:    now,
		ModelID: s.modelID.Provider + "/" + s.modelID.Release,
	}
	data := []byte(in.Text)

	// Provenance signature via BRC-43 wallet key (zero raw key exposure).
	sigBytes, err := s.port.SignDigest(ctx, core.ProvenanceDigest(data, prov))
	if err != nil {
		return nil, fmt.Errorf("memory: sign provenance: %w", err)
	}
	prov.Signature = sigBytes
	provBytes := core.SerializeProvenance(prov)
	hdp := core.ComputeHDPV2(data, prov)

	rec := store.FragmentRecord{
		Shard:      in.Shard,
		HDP:        core.ToHex(hdp),
		Data:       data,
		Provenance: provBytes,
		Level:      in.Level,
		Class:      in.Class,
		Supersedes: in.Supersedes,
		Tainted:    in.Tainted,
		CreatedAt:  now,
	}

	if in.BorgGroup != "" {
		caller := in.CallerPub
		if caller == "" {
			caller = s.port.IdentityPubHex()
		}
		grp, ok := s.groups.GetGroup(in.BorgGroup)
		if !ok {
			grp, err = s.groups.CreateGroup(in.BorgGroup, in.BorgGroup, caller)
			if err != nil {
				return nil, err
			}
		}
		epoch := grp.CurrentEpoch
		epochSym, err := s.groups.EpochKey(in.BorgGroup, caller, epoch)
		if err != nil {
			return nil, fmt.Errorf("memory: borg encrypt access: %w", err)
		}
		defer core.Zero32Array(&epochSym)
		var contentID [32]byte
		copy(contentID[:], hdp)
		env, _, err := bstack.SealForEpoch(in.BorgGroup, epoch, epochSym, contentID, data)
		if err != nil {
			return nil, err
		}
		metaBytes, err := json.Marshal(env)
		if err != nil {
			return nil, err
		}
		rec.Data = nil // never store plaintext alongside BRC-369 ciphertext
		rec.Ciphertext, err = hex.DecodeString(env.Ciphertext)
		if err != nil {
			return nil, fmt.Errorf("memory: decode sealed ciphertext: %w", err)
		}
		rec.KeyedMeta = metaBytes
		rec.BorgGroup = in.BorgGroup
		rec.BorgEpoch = epoch
	}

	// F-11: compute leaf AFTER BorgGroup sealing so H(E) commits to the actual BRC-369 ciphertext!
	frag := core.Fragment{Data: data, Provenance: prov, HDP: hdp, Ciphertext: rec.Ciphertext}
	leaf := core.ComputeLeaf(frag)
	rec.Leaf = core.ToHex(leaf)

	stored, dedup, err := s.db.AppendFragment(rec)
	if err != nil {
		return nil, err
	}

	var impacted []string
	if in.Supersedes > 0 && !dedup {
		impacted, _ = s.OnRevision(ctx, in.Shard, in.Supersedes)
	}

	var anchorTxID string
	if in.AutoAnchor {
		if a, err := s.AnchorShard(ctx, in.Shard); err == nil && a != nil {
			anchorTxID = a.TxID
		} else if err != nil {
			return nil, err
		}
	} else if !dedup && !IsSystemShard(in.Shard) {
		s.mu.RLock()
		hook := s.onRemember
		s.mu.RUnlock()
		if hook != nil {
			go hook(in.Shard)
		}
	}

	return &RememberResult{
		Shard:            stored.Shard,
		Seq:              stored.Seq,
		HDP:              stored.HDP,
		Leaf:             stored.Leaf,
		Encrypted:        stored.BorgGroup != "",
		BorgGroup:        stored.BorgGroup,
		BorgEpoch:        stored.BorgEpoch,
		Dedup:            dedup,
		AnchorTxID:       anchorTxID,
		ImpactedReceipts: impacted,
	}, nil
}

// Forget shreds local plaintext/ciphertext of (shard, seq) while keeping the anchored commitment.
func (s *Service) Forget(ctx context.Context, shard string, seq int64) error {
	return s.ForgetAuthorized(ctx, shard, seq, policy.Principal{Type: "Agent", ID: "owner", Level: 100})
}

// ForgetAuthorized checks shard validity and Cedar write authorization before shredding (shard, seq).
func (s *Service) ForgetAuthorized(_ context.Context, shard string, seq int64, principal policy.Principal) error {
	if err := ValidateShardName(shard, false); err != nil {
		return err
	}
	rec, err := s.db.GetFragment(shard, seq)
	if err != nil {
		return err
	}
	if principal.ID == "" {
		principal = policy.Principal{Type: "Agent", ID: "owner", Level: 100}
	}
	if dec, aErr := s.AuthorizeAction(principal, "write", policy.Resource{
		Type:  "Fragment",
		ID:    fmt.Sprintf("%s#%d", rec.Shard, rec.Seq),
		Level: int64(rec.Level),
		Shard: rec.Shard,
		Class: rec.Class,
	}); aErr != nil || !dec.Allow {
		if aErr != nil {
			return fmt.Errorf("memory: cedar forget policy error: %w", aErr)
		}
		return fmt.Errorf("memory: cedar policy denied forget on %s#%d (level %d > principal level %d)", rec.Shard, rec.Seq, rec.Level, principal.Level)
	}
	return s.db.ForgetFragment(shard, seq)
}
