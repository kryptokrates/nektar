package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

const ReceiptsShard = "__receipts__"

// ValidateShardName enforces safe, bounded shard names and blocks external access to reserved system shards.
func ValidateShardName(shard string, allowSystem bool) error {
	s := strings.TrimSpace(shard)
	if s == "" {
		return errors.New("memory: shard name is required")
	}
	if len(s) > 128 {
		return errors.New("memory: shard name exceeds 128 characters")
	}
	if s == ReceiptsShard {
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

// Service is the unified v0.4 NektarMemory engine composing:
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
		mid = reasoning.ModelIdentity{Kind: "api", Provider: "nektar", Release: "v0.4"}
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

func (s *Service) DB() *store.DB                   { return s.db }
func (s *Service) Chain() *chain.Port              { return s.port }
func (s *Service) Groups() *bstack.GroupRegistry   { return s.groups }
func (s *Service) Objects() bstack.ObjectStore     { return s.objects }
func (s *Service) AORG() *reasoning.AORGraph       { return s.aorg }
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
	hdp := core.ComputeHDP(data, prov)
	frag := core.Fragment{Data: data, Provenance: prov, HDP: hdp}
	leaf := core.ComputeLeaf(frag)

	rec := store.FragmentRecord{
		Shard:      in.Shard,
		HDP:        core.ToHex(hdp),
		Leaf:       core.ToHex(leaf),
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
		var contentID [32]byte
		copy(contentID[:], sha256.New().Sum(hdp)[:32])
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
		rec.Ciphertext, _ = hex.DecodeString(env.Ciphertext)
		rec.KeyedMeta = metaBytes
		rec.BorgGroup = in.BorgGroup
		rec.BorgEpoch = epoch
	}

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
	} else if !dedup && in.Shard != ReceiptsShard {
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

// AnchorShard commits all unanchored fragments in shard into a real NektarAnchor BSV transaction,
// serialized per-shard with durable pre-broadcast pending_anchors persistence, guarded by SpendGuard.
func (s *Service) AnchorShard(ctx context.Context, shard string) (*store.BatchAnchor, error) {
	unlock := s.lockShard(shard)
	defer unlock()

	unanchored, err := s.db.ListUnanchored(shard)
	if err != nil {
		return nil, err
	}
	if len(unanchored) == 0 {
		return nil, nil
	}
	leaves := make([][]byte, len(unanchored))
	seqs := make([]int64, len(unanchored))
	for i, f := range unanchored {
		lb, err := hex.DecodeString(f.Leaf)
		if err != nil {
			return nil, fmt.Errorf("memory: decode leaf seq=%d: %w", f.Seq, err)
		}
		leaves[i] = lb
		seqs[i] = f.Seq
	}
	root, err := core.MerkleRoot(leaves)
	if err != nil {
		return nil, err
	}

	anchors, err := s.db.ListAnchors(shard)
	if err != nil {
		return nil, err
	}
	prevRp := core.Zero32
	var nextN int64
	if len(anchors) > 0 {
		last := anchors[len(anchors)-1]
		nextN = last.N + 1
		prevRp, err = hex.DecodeString(last.ChainedHex)
		if err != nil {
			return nil, err
		}
	}
	rp := core.ChainRp(prevRp, root, uint64(len(leaves)))
	pubBytes := s.port.Signer().IdentityKey().Compressed()
	shardID := core.ShardIDFromPubkey(pubBytes)

	// Persist pending_anchors intent BEFORE building/broadcasting so a crash after broadcast is recoverable!
	now := time.Now().Unix()
	if err := s.db.SavePendingAnchor(store.PendingAnchor{
		Shard:      shard,
		N:          nextN,
		Seqs:       seqs,
		RootHex:    core.ToHex(root),
		ChainedHex: core.ToHex(rp),
		CreatedAt:  now,
	}); err != nil {
		return nil, fmt.Errorf("memory: persist pending anchor: %w", err)
	}

	state := contracts.AnchorState{
		Shard:    shardID,
		OwnerPub: s.port.IdentityPubHex(),
		OwnerPKH: core.Hash160(pubBytes),
		N:        uint64(nextN),
		Size:     uint64(len(leaves)),
		R:        root,
		Rp:       rp,
		Tag:      core.TagRPrime,
		Metanet:  true,
	}
	lockScript, err := contracts.EncodeAnchorLockingScript(state)
	if err != nil {
		_ = s.db.DeletePendingAnchor(shard, nextN)
		return nil, err
	}
	var tx *transaction.Transaction
	var txid string
	for attempt := range 3 {
		var fee, spend uint64
		tx, fee, spend, err = s.port.BuildFundedTx(ctx, []*transaction.TransactionOutput{chain.AnchorOutput(lockScript)})
		if err != nil {
			_ = s.db.DeletePendingAnchor(shard, nextN)
			return nil, err
		}
		if beefPre, bErr := chain.SafeAtomicBEEF(tx); bErr == nil {
			_ = s.db.UpdatePendingAnchorTx(shard, nextN, tx.TxID().String(), beefPre)
		}
		txid, err = s.port.Broadcast(ctx, tx, fee, spend)
		if err != nil {
			if attempt < 2 && (strings.Contains(err.Error(), "UTXO_SPENT") || strings.Contains(err.Error(), "spent by another transaction")) {
				continue
			}
			// Keep pending_anchors row if broadcast outcome is unknown so boot reconciliation can recover it!
			if !strings.Contains(err.Error(), "broadcast outcome unknown") {
				_ = s.db.DeletePendingAnchor(shard, nextN)
			}
			return nil, err
		}
		break
	}
	if err := s.port.AdoptChange(tx); err != nil {
		return nil, err
	}
	beef, err := chain.SafeAtomicBEEF(tx)
	if err != nil {
		return nil, fmt.Errorf("memory: encode AtomicBEEF: %w", err)
	}
	var settled bool
	var height int64
	if mp, h, ok := s.port.Proof(ctx, txid); ok && mp != nil {
		settled = true
		height = int64(h)
	}
	batch := store.BatchAnchor{
		Shard:       shard,
		ShardID:     shardID,
		N:           nextN,
		Size:        len(leaves),
		RootHex:     core.ToHex(root),
		ChainedHex:  core.ToHex(rp),
		TxID:        txid,
		Beef:        beef,
		Settled:     settled,
		BlockHeight: height,
		CreatedAt:   now,
	}
	if err := s.db.RecordBatchAnchor(batch, seqs); err != nil {
		return nil, err
	}
	if shard == ReceiptsShard {
		for _, seq := range seqs {
			_ = s.db.UpdateReceiptAnchor(seq, txid)
		}
	}
	return &batch, nil
}

// ReconcilePendingAnchors recovers any in-flight pending_anchors persisted before a crash.
func (s *Service) ReconcilePendingAnchors(ctx context.Context) (int, error) {
	pending, err := s.db.ListPendingAnchors()
	if err != nil || len(pending) == 0 {
		return 0, err
	}
	recovered := 0
	pubBytes := s.port.Signer().IdentityKey().Compressed()
	shardID := core.ShardIDFromPubkey(pubBytes)
	for _, p := range pending {
		unlock := s.lockShard(p.Shard)
		if p.TxID == "" {
			_ = s.db.DeletePendingAnchor(p.Shard, p.N)
			unlock()
			continue
		}
		// Check if transaction landed on chain or in local wallet storage.
		mp, height, mined := s.port.Proof(ctx, p.TxID)
		tx := s.port.Tx(p.TxID)
		if !mined && tx == nil && len(p.Beef) == 0 {
			_ = s.db.DeletePendingAnchor(p.Shard, p.N)
			unlock()
			continue
		}
		beef := p.Beef
		if tx != nil {
			if mp != nil {
				tx.MerklePath = mp
			}
			if b, bErr := chain.SafeAtomicBEEF(tx); bErr == nil && len(b) > 0 {
				beef = b
			}
		}
		if len(beef) > 0 && (mined || tx != nil) {
			batch := store.BatchAnchor{
				Shard:       p.Shard,
				ShardID:     shardID,
				N:           p.N,
				Size:        len(p.Seqs),
				RootHex:     p.RootHex,
				ChainedHex:  p.ChainedHex,
				TxID:        p.TxID,
				Beef:        beef,
				Settled:     mined,
				BlockHeight: int64(height),
				CreatedAt:   p.CreatedAt,
			}
			if rErr := s.db.RecordBatchAnchor(batch, p.Seqs); rErr == nil {
				recovered++
			}
		} else {
			_ = s.db.DeletePendingAnchor(p.Shard, p.N)
		}
		unlock()
	}
	return recovered, nil
}

func shardIdentifier(shard string, pubBytes []byte) string {
	h := core.DSHA256(core.Concat([]byte("nektar/shard/v1"), pubBytes, []byte(shard)))
	return core.ToHex(h)
}

// RecallInput specifies a shard recall query (omit Shard to search across all non-system shards).
type RecallInput struct {
	Shard             string           `json:"shard,omitempty"`
	Query             string           `json:"query,omitempty"`
	Principal         policy.Principal `json:"principal,omitempty"`
	CallerPub         string           `json:"callerPub,omitempty"`
	Action            string           `json:"-"` // internal Cedar action ("read" default, or "disclose")
	DisallowBorg      bool             `json:"-"` // internal: when true, Borg-encrypted fragments are skipped
	IncludeSuperseded bool             `json:"includeSuperseded,omitempty"`
	VerifySPV         bool             `json:"verifySpv,omitempty"`
	Limit             int              `json:"limit,omitempty"`
}

// RecalledFragment is one authorized, decrypted, optionally SPV-verified fragment.
type RecalledFragment struct {
	Shard       string                  `json:"shard"`
	Seq         int64                   `json:"seq"`
	Text        string                  `json:"text"`
	HDP         string                  `json:"hdp"`
	Leaf        string                  `json:"leaf"`
	Level       int                     `json:"level"`
	Class       string                  `json:"class"`
	BorgGroup   string                  `json:"borgGroup,omitempty"`
	BorgEpoch   int64                   `json:"borgEpoch,omitempty"`
	AnchorTxID  string                  `json:"anchorTxid,omitempty"`
	AnchorState string                  `json:"anchorState,omitempty"`
	Tainted     bool                    `json:"tainted,omitempty"`
	SPV         *contracts.VerifyResult `json:"spv,omitempty"`
	CreatedAt   int64                   `json:"createdAt"`
}

// Recall returns active, Cedar-authorized, Borg-decrypted fragments from shard.
func (s *Service) Recall(ctx context.Context, in RecallInput) ([]RecalledFragment, error) {
	if in.Shard == "" {
		in.Shard = "default"
	}
	if err := ValidateShardName(in.Shard, false); err != nil {
		return nil, err
	}
	principal := in.Principal
	if principal.ID == "" {
		principal = policy.Principal{Type: "Agent", ID: "owner", Level: 100}
	}
	action := strings.TrimSpace(in.Action)
	if action == "" {
		action = "read"
	}
	callerPub := in.CallerPub
	if callerPub == "" && !in.DisallowBorg {
		callerPub = s.port.IdentityPubHex()
	}

	records, err := s.db.ListFragments(in.Shard)
	if err != nil {
		return nil, err
	}
	superseded := make(map[int64]bool)
	for _, r := range records {
		if r.Supersedes > 0 {
			superseded[r.Supersedes] = true
		}
	}

	s.mu.RLock()
	polText := s.policyText
	s.mu.RUnlock()

	qLower := strings.ToLower(strings.TrimSpace(in.Query))
	var ftsRank map[int64]int
	if qLower != "" {
		if hits, err := s.db.SearchFTS(in.Shard, in.Query, 200); err == nil && len(hits) > 0 {
			ftsRank = make(map[int64]int, len(hits))
			for idx, h := range hits {
				ftsRank[h.Seq] = idx
			}
		}
	}
	var out []RecalledFragment

	for _, r := range records {
		if r.Forgotten {
			continue
		}
		if !in.IncludeSuperseded && superseded[r.Seq] {
			continue
		}
		// Cedar authorization gate (native cedar-go).
		dec, err := policy.Authorize(polText, principal, action, policy.Resource{
			Type:  "Fragment",
			ID:    fmt.Sprintf("%s#%d", r.Shard, r.Seq),
			Level: int64(r.Level),
			Shard: r.Shard,
			Class: r.Class,
		})
		if err != nil || !dec.Allow {
			continue
		}

		// Decrypt BRC-369 Borg-sealed fragment if encrypted.
		plaintext := r.Data
		if r.BorgGroup != "" {
			if in.DisallowBorg || callerPub == "" || len(r.KeyedMeta) == 0 {
				continue
			}
			var env bstack.SealedEnvelope
			if err := json.Unmarshal(r.KeyedMeta, &env); err != nil {
				continue
			}
			epochSym, err := s.groups.EpochKey(r.BorgGroup, callerPub, r.BorgEpoch)
			if err != nil {
				// Revoked or non-member: fail closed!
				continue
			}
			opened, err := bstack.OpenWithEpoch(&env, epochSym)
			core.Zero32Array(&epochSym)
			if err != nil {
				continue
			}
			// Re-verify decrypted plaintext cryptographically matches committed HDP.
			if hdpBytes, hErr := hex.DecodeString(r.HDP); hErr == nil && len(hdpBytes) == 32 && len(r.Provenance) > 0 {
				v1 := core.SHA256(core.Concat(core.TagProv, opened, r.Provenance))
				v2 := core.SHA256(core.Concat(core.TagProvV2, core.Field(opened), r.Provenance))
				if !core.EqualBytes(v1, hdpBytes) && !core.EqualBytes(v2, hdpBytes) {
					continue
				}
			}
			plaintext = opened
		}

		text := string(plaintext)
		if qLower != "" {
			_, ftsHit := ftsRank[r.Seq]
			if !ftsHit && !strings.Contains(strings.ToLower(text), qLower) {
				continue
			}
		}

		item := RecalledFragment{
			Shard:      r.Shard,
			Seq:        r.Seq,
			Text:       text,
			HDP:        r.HDP,
			Leaf:       r.Leaf,
			Level:      r.Level,
			Class:      r.Class,
			BorgGroup:  r.BorgGroup,
			BorgEpoch:  r.BorgEpoch,
			AnchorTxID: r.AnchorTxID,
			Tainted:    r.Tainted,
			CreatedAt:  r.CreatedAt,
		}
		if r.BatchN < 0 {
			item.AnchorState = "unanchored"
		} else {
			item.AnchorState = "anchored"
		}
		if in.VerifySPV && r.BatchN >= 0 {
			_, vr, pErr := s.Prove(ctx, r.Shard, r.Seq)
			if pErr == nil && vr != nil {
				item.SPV = vr
				item.AnchorState = vr.AnchorState
			}
		}
		out = append(out, item)
		if in.Limit > 0 && len(out) >= in.Limit {
			break
		}
	}
	return out, nil
}

// Prove constructs a self-contained ProofBundle for (shard, seq) and runs full offline SPV verification.
func (s *Service) Prove(ctx context.Context, shard string, seq int64) (*contracts.ProofBundle, *contracts.VerifyResult, error) {
	return s.ProveAuthorized(ctx, shard, seq, policy.Principal{Type: "Agent", ID: "owner", Level: 100})
}

// ProveAuthorized enforces Cedar read authorization before constructing a ProofBundle for (shard, seq).
func (s *Service) ProveAuthorized(ctx context.Context, shard string, seq int64, principal policy.Principal) (*contracts.ProofBundle, *contracts.VerifyResult, error) {
	if err := ValidateShardName(shard, true); err != nil {
		return nil, nil, err
	}
	rec, err := s.db.GetFragment(shard, seq)
	if err != nil {
		return nil, nil, err
	}
	if principal.ID == "" {
		principal = policy.Principal{Type: "Agent", ID: "owner", Level: 100}
	}
	if dec, aErr := s.AuthorizeAction(principal, "read", policy.Resource{
		Type:  "Fragment",
		ID:    fmt.Sprintf("%s#%d", rec.Shard, rec.Seq),
		Level: int64(rec.Level),
		Shard: rec.Shard,
		Class: rec.Class,
	}); aErr != nil || !dec.Allow {
		if aErr != nil {
			return nil, nil, fmt.Errorf("memory: cedar prove policy error: %w", aErr)
		}
		return nil, nil, fmt.Errorf("memory: cedar policy denied prove on %s#%d", rec.Shard, rec.Seq)
	}
	if rec.BatchN < 0 {
		return nil, &contracts.VerifyResult{OK: false, AnchorState: "unanchored", Reason: "fragment not yet anchored"}, nil
	}
	batchFrags, err := s.db.ListBatchFragments(shard, rec.BatchN)
	if err != nil {
		return nil, nil, err
	}
	leaves := make([][]byte, len(batchFrags))
	for i, f := range batchFrags {
		lb, err := hex.DecodeString(f.Leaf)
		if err != nil {
			return nil, nil, err
		}
		leaves[i] = lb
	}
	mp, err := core.ComputeMerkleProof(leaves, rec.LeafIndex)
	if err != nil {
		return nil, nil, err
	}
	pathHex := make([]string, len(mp.Siblings))
	for i, p := range mp.Siblings {
		pathHex[i] = core.ToHex(p)
	}
	anchors, err := s.db.ListAnchors(shard)
	if err != nil {
		return nil, nil, err
	}
	var target *store.BatchAnchor
	var tail []contracts.AnchorLink
	for i := range anchors {
		a := anchors[i]
		link := contracts.AnchorLink{
			TxID:        a.TxID,
			OutputIndex: 0,
			N:           uint64(a.N),
			Size:        uint64(a.Size),
			R:           a.RootHex,
			Rp:          a.ChainedHex,
		}
		if a.N <= rec.BatchN {
			tail = append(tail, link)
		}
		if a.N == rec.BatchN {
			cp := a
			target = &cp
		}
	}
	if target == nil {
		return nil, nil, fmt.Errorf("memory: missing anchor batch %d for shard %q", rec.BatchN, shard)
	}
	bundle := &contracts.ProofBundle{
		Version:   1,
		Shard:     target.ShardID,
		N:         uint64(target.N),
		Size:      uint64(target.Size),
		Leaf:      rec.Leaf,
		LeafIndex: rec.LeafIndex,
		Path:      pathHex,
		Anchor: contracts.AnchorLink{
			TxID:        target.TxID,
			OutputIndex: 0,
			N:           uint64(target.N),
			Size:        uint64(target.Size),
			R:           target.RootHex,
			Rp:          target.ChainedHex,
		},
		ChainTail:  tail,
		AnchorBEEF: target.Beef,
	}
	vr := contracts.VerifyFragmentCtx(ctx, *bundle, s.port.Tracker())
	return bundle, &vr, nil
}

// RefreshAnchorProofs polls the chain for mined MerklePaths on any unsettled broadcast anchors,
// verifies the computed Merkle root against PoW headers before upgrading settled=true, and attaches
// the mined BUMP into the stored AnchorBEEF.
func (s *Service) RefreshAnchorProofs(ctx context.Context) (int, error) {
	unsettled, err := s.db.ListUnsettledAnchors()
	if err != nil || len(unsettled) == 0 {
		return 0, err
	}
	upgraded := 0
	tracker := s.port.Tracker()
	for _, a := range unsettled {
		mp, height, ok := s.port.Proof(ctx, a.TxID)
		if !ok || mp == nil {
			continue
		}
		if height == 0 {
			height = mp.BlockHeight
		}
		tx, err := transaction.NewTransactionFromBEEF(a.Beef)
		if err != nil {
			continue
		}
		root, rErr := mp.ComputeRoot(tx.TxID())
		if rErr != nil || root == nil {
			continue
		}
		if tracker != nil {
			valid, vErr := tracker.IsValidRootForHeight(ctx, root, height)
			if vErr != nil || !valid {
				continue
			}
		}
		tx.MerklePath = mp
		newBeef, err := tx.AtomicBEEF(false)
		if err != nil {
			continue
		}
		if err := s.db.UpdateAnchorProof(a.Shard, a.N, newBeef, int64(height)); err == nil {
			upgraded++
		}
	}
	return upgraded, nil
}

// GroundedAnswerInput requests a grounded, receipt-backed answer from claims citing memory fragments.
type GroundedAnswerInput struct {
	Claims       []reasoning.Claim         `json:"claims"`
	Mode         string                    `json:"mode,omitempty"` // remove (default) | flag
	Principal    policy.Principal          `json:"principal,omitempty"`
	CallerPub    string                    `json:"callerPub,omitempty"`
	DisallowBorg bool                      `json:"-"`
	Delegations  []reasoning.Delegation    `json:"delegations,omitempty"`
	Payment      *reasoning.PaymentSummary `json:"payment,omitempty"`
	AutoAnchor   bool                      `json:"autoAnchor,omitempty"`
}

// GroundedAnswerResult carries the grounded answer text and auditable receipt footer.
type GroundedAnswerResult struct {
	Answer       string                     `json:"answer"`
	Ungrounded   bool                       `json:"ungrounded"`
	ReceiptHash  string                     `json:"receiptHash,omitempty"`
	ReceiptToken string                     `json:"receiptToken,omitempty"`
	VerifyURL    string                     `json:"verifyUrl,omitempty"`
	Footer       string                     `json:"footer,omitempty"`
	Report       *reasoning.GroundingReport `json:"report"`
	Receipt      *reasoning.Receipt         `json:"receipt,omitempty"`
	AnchorTxID   string                     `json:"anchorTxid,omitempty"`
}

// GroundedAnswer verifies claims against authorized fragments, strips ungrounded claims,
// commits the receipt to __receipts__, and returns the nektarr1.* receipt footer.
func (s *Service) GroundedAnswer(ctx context.Context, in GroundedAnswerInput) (*GroundedAnswerResult, error) {
	principal := in.Principal
	if principal.ID == "" {
		principal = policy.Principal{Type: "Agent", ID: "owner", Level: 100}
	}
	callerPub := in.CallerPub
	if callerPub == "" && !in.DisallowBorg {
		callerPub = s.port.IdentityPubHex()
	}

	s.mu.RLock()
	polText := s.policyText
	rules := append([]string(nil), s.rules...)
	s.mu.RUnlock()

	fetch := func(ref reasoning.FragmentRef) (bool, string) {
		r, err := s.db.GetFragment(ref.Shard, ref.Seq)
		if err != nil || r.Forgotten {
			return false, ""
		}
		// Never ground new answers on superseded facts!
		if newerSeq, sErr := s.db.SupersededBy(ref.Shard, ref.Seq); sErr == nil && newerSeq > 0 {
			return false, ""
		}
		dec, err := policy.Authorize(polText, principal, "read", policy.Resource{
			Type:  "Fragment",
			ID:    fmt.Sprintf("%s#%d", r.Shard, r.Seq),
			Level: int64(r.Level),
			Shard: r.Shard,
			Class: r.Class,
		})
		if err != nil || !dec.Allow {
			return false, ""
		}
		if r.BorgGroup != "" {
			if in.DisallowBorg || callerPub == "" {
				return false, ""
			}
			var env bstack.SealedEnvelope
			if err := json.Unmarshal(r.KeyedMeta, &env); err != nil {
				return false, ""
			}
			epochSym, err := s.groups.EpochKey(r.BorgGroup, callerPub, r.BorgEpoch)
			if err != nil {
				return false, ""
			}
			pt, err := bstack.OpenWithEpoch(&env, epochSym)
			if err != nil {
				return false, ""
			}
			return true, string(pt)
		}
		return true, string(r.Data)
	}

	report, err := reasoning.Ground(in.Claims, fetch, in.Mode, rules)
	if err != nil {
		return nil, err
	}

	// Build grounded answer strictly from kept grounded claims.
	var parts []string
	seenRefs := make(map[reasoning.FragmentRef]bool)
	var refs []reasoning.FragmentRef
	for _, gc := range report.Claims {
		if !gc.Grounded {
			continue
		}
		parts = append(parts, gc.Text)
		for _, r := range gc.Cites {
			if !seenRefs[r] {
				seenRefs[r] = true
				refs = append(refs, r)
			}
		}
		for _, q := range gc.Quotes {
			r := reasoning.FragmentRef{Shard: q.Shard, Seq: q.Seq}
			if !seenRefs[r] {
				seenRefs[r] = true
				refs = append(refs, r)
			}
		}
	}

	// Honesty rule: if nothing grounded, never emit a receipt footer!
	if len(parts) == 0 {
		return &GroundedAnswerResult{
			Answer:     "",
			Ungrounded: true,
			Report:     report,
		}, nil
	}

	answer := strings.Join(parts, " ")
	rcp := reasoning.Receipt{
		Answer:              answer,
		FragmentRefs:        refs,
		CheckResult:         "verified",
		ModelID:             s.modelID,
		Timestamp:           time.Now().Unix(),
		GroundingReportHash: report.ReportHash,
		GroundingTier:       "mangle+quote",
		TruthNote:           reasoning.TruthNote,
		Delegations:         in.Delegations,
		Payment:             in.Payment,
	}
	if err := reasoning.VerifyDelegations(rcp); err != nil {
		return nil, err
	}
	rcpHash, err := reasoning.ReceiptHash(rcp)
	if err != nil {
		return nil, err
	}
	tok, err := reasoning.ReceiptToken(rcpHash)
	if err != nil {
		return nil, err
	}
	bodyJSON, err := core.CanonicalJSON(rcp)
	if err != nil {
		return nil, err
	}

	// Atomically commit receipt fragment (__receipts__), receipts index, and citation edges in ONE SQLite tx!
	now := rcp.Timestamp
	prov := core.Provenance{
		Source:  "receipt",
		Time:    now,
		ModelID: s.modelID.Provider + "/" + s.modelID.Release,
	}
	sigBytes, err := s.port.SignDigest(ctx, core.ProvenanceDigest(bodyJSON, prov))
	if err != nil {
		return nil, fmt.Errorf("memory: sign receipt provenance: %w", err)
	}
	prov.Signature = sigBytes
	provBytes := core.SerializeProvenance(prov)
	hdp := core.ComputeHDP(bodyJSON, prov)
	leaf := core.ComputeLeaf(core.Fragment{Data: bodyJSON, Provenance: prov, HDP: hdp})
	fragRec := store.FragmentRecord{
		Shard:      ReceiptsShard,
		HDP:        core.ToHex(hdp),
		Leaf:       core.ToHex(leaf),
		Data:       bodyJSON,
		Provenance: provBytes,
		Class:      "receipt",
		BatchN:     -1,
		LeafIndex:  -1,
		CreatedAt:  now,
	}
	cites := make([]store.CitationEdge, 0, len(refs))
	for _, r := range refs {
		cites = append(cites, store.CitationEdge{Shard: r.Shard, Seq: r.Seq, ReceiptHash: rcpHash})
	}
	storedFrag, err := s.db.CommitReceiptAtomic(fragRec, store.ReceiptRecord{
		ReceiptHash: rcpHash,
		BodyJSON:    bodyJSON,
		CreatedAt:   now,
	}, cites)
	if err != nil {
		return nil, err
	}
	var anchorTxID string
	if in.AutoAnchor {
		if a, aErr := s.AnchorShard(ctx, ReceiptsShard); aErr != nil {
			return nil, aErr
		} else if a != nil {
			anchorTxID = a.TxID
			_ = s.db.UpdateReceiptAnchor(storedFrag.Seq, anchorTxID)
		}
	}

	verifyURL := s.verifyBaseURL + "/" + tok
	footer := fmt.Sprintf("receipt: %s\nverify: %s", tok, verifyURL)

	return &GroundedAnswerResult{
		Answer:       answer,
		Ungrounded:   false,
		ReceiptHash:  rcpHash,
		ReceiptToken: tok,
		VerifyURL:    verifyURL,
		Footer:       footer,
		Report:       report,
		Receipt:      &rcp,
		AnchorTxID:   anchorTxID,
	}, nil
}

// ReceiptVerification is the traffic-light verdict returned by VerifyReceipt and GET /v1/verify/{token}.
type ReceiptVerification struct {
	OK               bool                      `json:"ok"`
	Verdict          string                    `json:"verdict"` // green | amber | red
	Reason           string                    `json:"reason"`
	ReceiptHash      string                    `json:"receiptHash"`
	ReceiptToken     string                    `json:"receiptToken"`
	Receipt          *reasoning.Receipt        `json:"receipt,omitempty"`
	ReceiptAnchored  bool                      `json:"receiptAnchored"`
	CitationsChecked    int                       `json:"citationsChecked"`
	CitationsMined      int                       `json:"citationsMined"`
	CitationsMissing    int                       `json:"citationsMissing"`
	CitationsSuperseded int                       `json:"citationsSuperseded,omitempty"`
	SupersededRefs      []string                  `json:"supersededRefs,omitempty"`
	TruthNote           string                    `json:"truthNote"`
}

// VerifyReceipt validates a nektarr1.* token or 64-hex hash, its §16 delegations, and its cited SPV proofs.
func (s *Service) VerifyReceipt(ctx context.Context, tokenOrHash string) (*ReceiptVerification, error) {
	h, err := reasoning.ParseReceiptToken(tokenOrHash)
	if err != nil {
		return &ReceiptVerification{Verdict: "red", Reason: err.Error(), TruthNote: reasoning.TruthNote}, nil
	}
	tok, _ := reasoning.ReceiptToken(h)
	rec, err := s.db.GetReceipt(h)
	if err != nil {
		return &ReceiptVerification{
			Verdict:      "red",
			Reason:       "receipt not found",
			ReceiptHash:  h,
			ReceiptToken: tok,
			TruthNote:    reasoning.TruthNote,
		}, nil
	}
	var rcp reasoning.Receipt
	if err := json.Unmarshal(rec.BodyJSON, &rcp); err != nil {
		return &ReceiptVerification{Verdict: "red", Reason: "corrupt receipt JSON", ReceiptHash: h, ReceiptToken: tok, TruthNote: reasoning.TruthNote}, nil
	}
	recomputed, err := reasoning.ReceiptHash(rcp)
	if err != nil || recomputed != h {
		return &ReceiptVerification{Verdict: "red", Reason: "receipt hash mismatch", ReceiptHash: h, ReceiptToken: tok, TruthNote: reasoning.TruthNote}, nil
	}
	if err := reasoning.VerifyDelegations(rcp); err != nil {
		return &ReceiptVerification{Verdict: "red", Reason: err.Error(), ReceiptHash: h, ReceiptToken: tok, Receipt: &rcp, TruthNote: reasoning.TruthNote}, nil
	}

	// Derive ReceiptAnchored live from the __receipts__ fragment's anchor_txid (so later anchoring reflects immediately!).
	receiptAnchored := rec.AnchorTxID != ""
	if !receiptAnchored && rec.ShardSeq > 0 {
		if rf, rfErr := s.db.GetFragment(ReceiptsShard, rec.ShardSeq); rfErr == nil && rf.AnchorTxID != "" {
			receiptAnchored = true
		}
	}
	out := &ReceiptVerification{
		ReceiptHash:     h,
		ReceiptToken:    tok,
		Receipt:         &rcp,
		ReceiptAnchored: receiptAnchored,
		TruthNote:       reasoning.TruthNote,
	}
	for _, ref := range rcp.FragmentRefs {
		out.CitationsChecked++
		f, fErr := s.db.GetFragment(ref.Shard, ref.Seq)
		if fErr != nil || f.Forgotten {
			out.CitationsMissing++
			continue
		}
		if newerSeq, sErr := s.db.SupersededBy(ref.Shard, ref.Seq); sErr == nil && newerSeq > 0 {
			out.CitationsSuperseded++
			out.SupersededRefs = append(out.SupersededRefs, fmt.Sprintf("%s#%d->#%d", ref.Shard, ref.Seq, newerSeq))
		}
		if f.BatchN >= 0 {
			_, vr, pErr := s.Prove(ctx, ref.Shard, ref.Seq)
			if pErr == nil && vr != nil && vr.OK && vr.Settled {
				out.CitationsMined++
			}
		}
	}

	switch {
	case out.CitationsMissing > 0:
		out.Verdict = "red"
		out.OK = false
		out.Reason = fmt.Sprintf("%d cited fragment(s) missing or forgotten", out.CitationsMissing)
	case out.CitationsSuperseded > 0:
		out.Verdict = "amber"
		out.OK = true
		out.Reason = fmt.Sprintf("%d cited fragment(s) were superseded by a newer revision; re-grounding recommended", out.CitationsSuperseded)
	case out.CitationsChecked > 0 && out.CitationsMined == out.CitationsChecked:
		out.Verdict = "green"
		out.OK = true
		out.Reason = "all cited fragments SPV-verified against mined anchors"
	default:
		out.Verdict = "amber"
		out.OK = true
		out.Reason = "receipt valid locally; one or more cited fragments are not yet mined"
	}
	return out, nil
}

// OnRevision returns receipt hashes citing (shard, seq) so downstream consumers can re-ground on edits.
func (s *Service) OnRevision(_ context.Context, shard string, seq int64) ([]string, error) {
	return s.db.CitingReceipts(shard, seq)
}

// ShardPack is a portable, CHIRP-addressed shard snapshot with fragments and anchor proofs.
type ShardPack struct {
	Version    int                    `json:"version"`
	Shard      string                 `json:"shard"`
	OwnerPub   string                 `json:"ownerPub"`
	ExportedAt int64                  `json:"exportedAt"`
	Fragments  []store.FragmentRecord `json:"fragments"`
	Anchors    []store.BatchAnchor    `json:"anchors"`
}

// ShardPackResult describes an exported BRC-167 CHIRP shard pack.
type ShardPackResult struct {
	Shard    string `json:"shard"`
	ChirpURL string `json:"chirpUrl"`
	RootHex  string `json:"rootHex"`
	Size     int    `json:"size"`
}

// LoadOrCreateProtectedKey loads or creates a 32-byte key in SQLite wrapped at rest via BRC-43
// wallet EncryptAtRest/DecryptAtRest (transparently migrating any legacy plaintext key, failing closed
// on encryption errors so fresh keys are never persisted in plaintext SQLite/WAL).
func (s *Service) LoadOrCreateProtectedKey(name string, gen func() ([]byte, error)) ([]byte, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("memory: db unavailable")
	}
	var encBlob struct {
		EncHex string `json:"encHex"`
	}
	if ok, _ := s.db.GetJSON(name+"_enc", &encBlob); ok && encBlob.EncHex != "" {
		if s.port == nil {
			return nil, fmt.Errorf("memory: encrypted key %q requires active wallet port", name)
		}
		ct, err := hex.DecodeString(encBlob.EncHex)
		if err != nil {
			return nil, fmt.Errorf("memory: decode encrypted key %q: %w", name, err)
		}
		pt, err := s.port.DecryptAtRest(context.Background(), name, ct)
		if err != nil || len(pt) != 32 {
			return nil, fmt.Errorf("memory: decrypt protected key %q: %w", name, err)
		}
		return pt, nil
	}
	// Check for legacy plaintext key to migrate (never create a new plaintext key if s.port != nil).
	var legacyHex string
	if ok, _ := s.db.GetJSON(name, &legacyHex); ok && len(legacyHex) == 64 {
		if raw, err := hex.DecodeString(legacyHex); err == nil && len(raw) == 32 {
			if s.port != nil {
				ct, encErr := s.port.EncryptAtRest(context.Background(), name, raw)
				if encErr != nil {
					core.ZeroBytes(raw)
					return nil, fmt.Errorf("memory: migrate key %q at rest: %w", name, encErr)
				}
				if err := s.db.SetJSON(name+"_enc", map[string]string{"encHex": hex.EncodeToString(ct)}); err != nil {
					core.ZeroBytes(raw)
					return nil, err
				}
				_ = s.db.SetJSON(name, map[string]string{"migrated": "encrypted"})
			}
			return raw, nil
		}
	}
	if s.port != nil {
		created, err := gen()
		if err != nil {
			return nil, err
		}
		if len(created) != 32 {
			core.ZeroBytes(created)
			return nil, fmt.Errorf("memory: generated key %q must be 32 bytes", name)
		}
		ct, encErr := s.port.EncryptAtRest(context.Background(), name, created)
		if encErr != nil {
			core.ZeroBytes(created)
			return nil, fmt.Errorf("memory: encrypt new key %q at rest: %w", name, encErr)
		}
		if err := s.db.SetJSON(name+"_enc", map[string]string{"encHex": hex.EncodeToString(ct)}); err != nil {
			core.ZeroBytes(created)
			return nil, err
		}
		return created, nil
	}
	return s.db.LoadOrCreateStableKey(name, gen)
}

// ExportShardPack bundles shard fragments and anchors into a content-addressed BRC-167 CHIRP closure.
// For external/HTTP export, use ExportShardPackAuthorized or ExportPublicShardPack.
func (s *Service) ExportShardPack(ctx context.Context, shard string) (*ShardPackResult, error) {
	res, _, err := s.exportShardPackInternal(ctx, shard, policy.Principal{Type: "Agent", ID: "owner", Level: 100}, "read", false, false)
	return res, err
}

// ExportPublicShardPack exports only active, Level-0, non-Borg, Cedar-"disclose" fragments and registers the
// resulting CHIRP DAG object hashes in the public unauthenticated allowlist (for starter/market packs).
func (s *Service) ExportPublicShardPack(ctx context.Context, shard string) (*ShardPackResult, error) {
	res, hashes, err := s.exportShardPackInternal(ctx, shard, policy.Principal{Type: "Agent", ID: "public", Level: 0}, "disclose", true, true)
	if err != nil {
		return nil, err
	}
	s.AllowPublicChirpHashes(hashes...)
	return res, nil
}

// ExportShardPackAuthorized exports only active, Cedar-authorized fragments from shard into CHIRP.
func (s *Service) ExportShardPackAuthorized(ctx context.Context, shard string, principal policy.Principal, action string, disallowBorg bool) (*ShardPackResult, error) {
	res, _, err := s.exportShardPackInternal(ctx, shard, principal, action, disallowBorg, true)
	return res, err
}

func (s *Service) exportShardPackInternal(_ context.Context, shard string, principal policy.Principal, action string, disallowBorg, excludeSuperseded bool) (*ShardPackResult, [][32]byte, error) {
	if err := ValidateShardName(shard, false); err != nil {
		return nil, nil, err
	}
	if principal.ID == "" {
		principal = policy.Principal{Type: "Agent", ID: "public", Level: 0}
	}
	if action == "" {
		action = "disclose"
	}
	allFrags, err := s.db.ListFragments(shard)
	if err != nil {
		return nil, nil, err
	}
	superseded := make(map[int64]bool)
	for _, r := range allFrags {
		if r.Supersedes > 0 {
			superseded[r.Supersedes] = true
		}
	}
	s.mu.RLock()
	polText := s.policyText
	s.mu.RUnlock()

	frags := make([]store.FragmentRecord, 0, len(allFrags))
	for _, r := range allFrags {
		if r.Forgotten || (excludeSuperseded && superseded[r.Seq]) {
			continue
		}
		if disallowBorg && r.BorgGroup != "" {
			continue
		}
		dec, aErr := policy.Authorize(polText, principal, action, policy.Resource{
			Type:  "Fragment",
			ID:    fmt.Sprintf("%s#%d", r.Shard, r.Seq),
			Level: int64(r.Level),
			Shard: r.Shard,
			Class: r.Class,
		})
		if aErr != nil || !dec.Allow {
			continue
		}
		frags = append(frags, r)
	}
	anchors, err := s.db.ListAnchors(shard)
	if err != nil {
		return nil, nil, err
	}
	pack := ShardPack{
		Version:    1,
		Shard:      shard,
		OwnerPub:   s.port.IdentityPubHex(),
		ExportedAt: time.Now().Unix(),
		Fragments:  frags,
		Anchors:    anchors,
	}
	raw, err := json.Marshal(pack)
	if err != nil {
		return nil, nil, err
	}
	u, root, hashes, err := bstack.StoreChirpWithHashes(s.objects, raw)
	if err != nil {
		return nil, nil, err
	}
	return &ShardPackResult{
		Shard:    shard,
		ChirpURL: u,
		RootHex:  hex.EncodeToString(root[:]),
		Size:     len(raw),
	}, hashes, nil
}

// ImportShardPack resolves and verifies a CHIRP shard pack closure and imports its fragments into targetShard.
func (s *Service) ImportShardPack(ctx context.Context, chirpURL, targetShard string) (int, error) {
	raw, err := bstack.ResolveChirp(s.objects, chirpURL)
	if err != nil {
		return 0, err
	}
	n, _, err := s.importShardPackVerified(ctx, raw, targetShard)
	return n, err
}

func (s *Service) importShardPackVerified(ctx context.Context, raw []byte, targetShard string) (int, string, error) {
	var pack ShardPack
	if err := json.Unmarshal(raw, &pack); err != nil {
		return 0, "", fmt.Errorf("memory: decode shard pack: %w", err)
	}
	if targetShard == "" {
		targetShard = pack.Shard
	}
	if err := ValidateShardName(targetShard, false); err != nil {
		return 0, "", err
	}
	// Verify every claimed anchor's AnchorBEEF (bounded via guard.ParseBEEF) + Rúnar NektarAnchor state
	// + contiguous ChainRp continuity from genesis + header BUMP settlement (never trusting untrusted JSON flags)
	// and refuse overwriting any conflicting existing local anchor!
	sortedAnchors := append([]store.BatchAnchor(nil), pack.Anchors...)
	for i := 1; i < len(sortedAnchors); i++ {
		for j := i; j > 0 && sortedAnchors[j].N < sortedAnchors[j-1].N; j-- {
			sortedAnchors[j], sortedAnchors[j-1] = sortedAnchors[j-1], sortedAnchors[j]
		}
	}
	verifiedAnchors := make(map[int64]store.BatchAnchor, len(sortedAnchors))
	prevRp := core.Zero32Bytes()
	expectedN := int64(0)
	for _, a := range sortedAnchors {
		if len(a.Beef) == 0 {
			continue
		}
		// Enforce contiguous anchor numbering from genesis (no skipped N=0 or gaps bypassing ChainRp!).
		if a.N != expectedN {
			continue
		}
		_, tx, txHash, tErr := guard.ParseBEEF(a.Beef, guard.DefaultBound)
		if tErr != nil || tx == nil || txHash == nil || len(tx.Outputs) == 0 || tx.Outputs[0].LockingScript == nil {
			continue
		}
		st, dErr := contracts.DecodeNektarAnchor(*tx.Outputs[0].LockingScript)
		if dErr != nil {
			continue
		}
		if st.N != uint64(a.N) || st.Size != uint64(a.Size) || !strings.EqualFold(hex.EncodeToString(st.R), a.RootHex) ||
			!strings.EqualFold(hex.EncodeToString(st.Rp), a.ChainedHex) || txHash.String() != a.TxID {
			continue
		}
		// Enforce sequential ChainRp linkage across every imported anchor.
		wantTag := core.ChainRpWithTag(st.Tag, prevRp, st.R, st.Size)
		wantLegacy := core.EqualBytes(st.Tag, core.TagAnchorTag) && core.EqualBytes(st.Rp, core.ChainRp(prevRp, st.R, st.Size))
		if !core.EqualBytes(wantTag, st.Rp) && !wantLegacy {
			continue
		}
		prevRp = st.Rp
		expectedN = a.N + 1
		// Derive Settled strictly from verified BUMP against header chain (never trust untrusted JSON flag!).
		a.Settled = false
		if tx.MerklePath != nil {
			if s.port.Network() == "memory" {
				a.Settled = true
			} else {
				tracker := s.port.Tracker()
				if tracker == nil {
					continue
				}
				root, rErr := tx.MerklePath.ComputeRoot(txHash)
				if rErr != nil || root == nil {
					continue
				}
				valid, vErr := tracker.IsValidRootForHeight(ctx, root, tx.MerklePath.BlockHeight)
				if vErr != nil || !valid {
					continue
				}
				a.Settled = true
			}
		}
		a.Shard = targetShard
		if err := s.db.SaveBatchAnchorStrict(a); err != nil {
			return 0, targetShard, fmt.Errorf("memory: import shard pack anchor conflict: %w", err)
		}
		verifiedAnchors[a.N] = a
	}

	// Recompute every fragment's HDP/Leaf and verify full batch Merkle root against a.RootHex
	// before allowing any imported fragment to retain anchored (BatchN, LeafIndex, AnchorTxID) status!
	batchValid := make(map[int64]bool, len(verifiedAnchors))
	for batchN, a := range verifiedAnchors {
		byIdx := make(map[int][]byte)
		okBatch := a.Size > 0
		for _, f := range pack.Fragments {
			if f.BatchN != batchN {
				continue
			}
			if f.LeafIndex < 0 || f.LeafIndex >= a.Size {
				okBatch = false
				break
			}
			hdpBytes, hErr := hex.DecodeString(strings.TrimSpace(f.HDP))
			if hErr != nil || len(hdpBytes) != 32 {
				okBatch = false
				break
			}
			// Whenever plaintext Data is present for a non-forgotten, non-Borg fragment, Provenance MUST be present
			// and HDP MUST cryptographically commit to Data+Provenance (prevents empty-provenance plaintext forgery!).
			if len(f.Data) > 0 && !f.Forgotten && f.BorgGroup == "" {
				if len(f.Provenance) == 0 {
					okBatch = false
					break
				}
				expectedV1 := core.SHA256(core.Concat(core.TagProv, f.Data, f.Provenance))
				expectedV2 := core.SHA256(core.Concat(core.TagProvV2, core.Field(f.Data), f.Provenance))
				if !core.EqualBytes(expectedV1, hdpBytes) && !core.EqualBytes(expectedV2, hdpBytes) {
					okBatch = false
					break
				}
			}
			leafBytes := core.ComputeLeaf(core.Fragment{HDP: hdpBytes, Ciphertext: f.Ciphertext})
			if !strings.EqualFold(f.Leaf, core.ToHex(leafBytes)) {
				// Backward compatibility for legacy encrypted fragments committed with empty ciphertext.
				legacyLeaf := core.ComputeLeaf(core.Fragment{HDP: hdpBytes})
				if len(f.Ciphertext) == 0 || !strings.EqualFold(f.Leaf, core.ToHex(legacyLeaf)) {
					okBatch = false
					break
				}
				leafBytes = legacyLeaf
			}
			byIdx[f.LeafIndex] = leafBytes
		}
		if okBatch && len(byIdx) == a.Size {
			leaves := make([][]byte, a.Size)
			for i := 0; i < a.Size; i++ {
				lb, ok := byIdx[i]
				if !ok {
					okBatch = false
					break
				}
				leaves[i] = lb
			}
			if okBatch {
				if root, rErr := core.MerkleRoot(leaves); rErr == nil && strings.EqualFold(core.ToHex(root), a.RootHex) {
					batchValid[batchN] = true
				}
			}
		}
	}

	imported := 0
	for _, f := range pack.Fragments {
		f.Shard = targetShard
		f.Tainted = true
		if a, hasAnchor := verifiedAnchors[f.BatchN]; !hasAnchor || !batchValid[f.BatchN] || f.BatchN < 0 || f.AnchorTxID != a.TxID {
			f.BatchN = -1
			f.LeafIndex = -1
			f.AnchorTxID = ""
		}
		if _, dedup, err := s.db.AppendFragment(f); err == nil && !dedup {
			imported++
		}
	}
	return imported, targetShard, nil
}

type dbGroupStore struct {
	db   *store.DB
	port *chain.Port
}

type sealedEpochsBlob struct {
	EncHex string `json:"encHex"`
}

func (g *dbGroupStore) LoadBorgGroups() ([]*bstack.BorgGroup, error) {
	rows, err := g.db.ListBorgGroups()
	if err != nil {
		return nil, err
	}
	out := make([]*bstack.BorgGroup, 0, len(rows))
	for _, r := range rows {
		bg := &bstack.BorgGroup{
			GroupID:      r.GroupID,
			Name:         r.Name,
			CurrentEpoch: r.CurrentEpoch,
			Members:      make(map[string]string),
			Revoked:      make(map[string]int64),
			Epochs:       make(map[int64][32]byte),
			EpochMembers: make(map[int64][]string),
		}
		_ = json.Unmarshal(r.MembersJSON, &bg.Members)
		_ = json.Unmarshal(r.RevokedJSON, &bg.Revoked)
		_ = json.Unmarshal(r.EpochMembersJSON, &bg.EpochMembers)

		epJSON := r.EpochsJSON
		var wrapped sealedEpochsBlob
		if json.Unmarshal(r.EpochsJSON, &wrapped) == nil && wrapped.EncHex != "" {
			if g.port == nil {
				return nil, fmt.Errorf("borg: encrypted epoch keys for group %q require active wallet port", r.GroupID)
			}
			ct, dErr := hex.DecodeString(wrapped.EncHex)
			if dErr != nil {
				return nil, fmt.Errorf("borg: decode encrypted epochs for group %q: %w", r.GroupID, dErr)
			}
			pt, decErr := g.port.DecryptAtRest(context.Background(), "borg-group:"+r.GroupID, ct)
			if decErr != nil {
				return nil, fmt.Errorf("borg: decrypt epochs for group %q: %w", r.GroupID, decErr)
			}
			epJSON = pt
			defer core.ZeroBytes(pt)
		}
		var hexEpochs map[int64]string
		if json.Unmarshal(epJSON, &hexEpochs) == nil {
			for ep, hx := range hexEpochs {
				if raw, decErr := hex.DecodeString(hx); decErr == nil && len(raw) == 32 {
					var k [32]byte
					copy(k[:], raw)
					core.ZeroBytes(raw)
					bg.Epochs[ep] = k
				}
			}
		}
		out = append(out, bg)
	}
	return out, nil
}

func (g *dbGroupStore) SaveBorgGroup(bg *bstack.BorgGroup) error {
	memJSON, _ := json.Marshal(bg.Members)
	revJSON, _ := json.Marshal(bg.Revoked)
	epMemJSON, _ := json.Marshal(bg.EpochMembers)
	hexEpochs := make(map[int64]string, len(bg.Epochs))
	for ep, k := range bg.Epochs {
		hexEpochs[ep] = hex.EncodeToString(k[:])
	}
	epJSON, _ := json.Marshal(hexEpochs)
	defer core.ZeroBytes(epJSON)
	persistEpochs := epJSON
	if g.port != nil {
		ct, err := g.port.EncryptAtRest(context.Background(), "borg-group:"+bg.GroupID, epJSON)
		if err != nil {
			// Fail closed: never fall back to persisting plaintext Borg epoch symmetric keys in SQLite.
			return fmt.Errorf("borg: encrypt epoch keys at rest for group %q: %w", bg.GroupID, err)
		}
		persistEpochs, _ = json.Marshal(sealedEpochsBlob{EncHex: hex.EncodeToString(ct)})
	}
	return g.db.SaveBorgGroup(store.BorgGroupRecord{
		GroupID:          bg.GroupID,
		Name:             bg.Name,
		CurrentEpoch:     bg.CurrentEpoch,
		MembersJSON:      memJSON,
		RevokedJSON:      revJSON,
		EpochsJSON:       persistEpochs,
		EpochMembersJSON: epMemJSON,
	})
}
