package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

const ReceiptsShard = "__receipts__"

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
	pol := cfg.PolicyText
	if strings.TrimSpace(pol) == "" {
		pol = policy.DefaultPolicySet
	}
	pc, err := policy.Canonicalize("default", pol)
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimRight(cfg.VerifyBaseURL, "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8080/v1/verify"
	}
	mid := cfg.ModelID
	if mid.Kind == "" {
		mid = reasoning.ModelIdentity{Kind: "api", Provider: "nektar", Release: "v0.4"}
	}
	return &Service{
		db:            db,
		port:          port,
		groups:        groups,
		objects:       objects,
		policyText:    pol,
		policyCommit:  pc,
		rules:         append([]string(nil), cfg.DatalogRules...),
		verifyBaseURL: baseURL,
		modelID:       mid,
		aorg:          reasoning.NewAORGraph(cfg.DatalogRules),
	}, nil
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

// SetPolicy updates the active Cedar policy set atomically.
func (s *Service) SetPolicy(name, text string) (*policy.PolicyCommitment, error) {
	pc, err := policy.Canonicalize(name, text)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.policyText = text
	s.policyCommit = pc
	s.mu.Unlock()
	return pc, nil
}

// AddRules appends Mangle Datalog rules used during grounding and AORG reasoning.
func (s *Service) AddRules(rules ...string) {
	s.mu.Lock()
	s.rules = append(s.rules, rules...)
	s.aorg = reasoning.NewAORGraph(s.rules)
	s.mu.Unlock()
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
	Supersedes int64             `json:"supersedes,omitempty"`
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
	if in.Text == "" {
		return nil, errors.New("memory: text is required")
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
func (s *Service) Forget(_ context.Context, shard string, seq int64) error {
	return s.db.ForgetFragment(shard, seq)
}

// AnchorShard commits all unanchored fragments in shard into a real NektarAnchor BSV transaction,
// guarded by SpendGuard, and stores the AtomicBEEF proof bundle locally.
func (s *Service) AnchorShard(ctx context.Context, shard string) (*store.BatchAnchor, error) {
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

	state := contracts.AnchorState{
		Shard:    shardID,
		OwnerPub: s.port.IdentityPubHex(),
		OwnerPKH: core.Hash160(pubBytes),
		N:        uint64(nextN),
		Size:     uint64(len(leaves)),
		R:        root,
		Rp:       rp,
		Tag:      core.TagAnchorTag,
		Metanet:  true,
	}
	lockScript, err := contracts.EncodeAnchorLockingScript(state)
	if err != nil {
		return nil, err
	}
	tx, fee, spend, err := s.port.BuildFundedTx(ctx, []*transaction.TransactionOutput{chain.AnchorOutput(lockScript)})
	if err != nil {
		return nil, err
	}
	txid, err := s.port.Broadcast(ctx, tx, fee, spend)
	if err != nil {
		return nil, err
	}
	if err := s.port.AdoptChange(tx); err != nil {
		return nil, err
	}
	beef, err := tx.AtomicBEEF(false)
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
		CreatedAt:   time.Now().Unix(),
	}
	if err := s.db.RecordBatchAnchor(batch, seqs); err != nil {
		return nil, err
	}
	return &batch, nil
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
	SPV         *contracts.VerifyResult `json:"spv,omitempty"`
	CreatedAt   int64                   `json:"createdAt"`
}

// Recall returns active, Cedar-authorized, Borg-decrypted fragments from shard.
func (s *Service) Recall(ctx context.Context, in RecallInput) ([]RecalledFragment, error) {
	if in.Shard == "" {
		in.Shard = "default"
	}
	principal := in.Principal
	if principal.ID == "" {
		principal = policy.Principal{Type: "Agent", ID: "owner", Level: 100}
	}
	callerPub := in.CallerPub
	if callerPub == "" {
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
	var out []RecalledFragment

	for _, r := range records {
		if r.Forgotten {
			continue
		}
		if !in.IncludeSuperseded && superseded[r.Seq] {
			continue
		}
		// Cedar authorization gate (native cedar-go).
		dec, err := policy.Authorize(polText, principal, "read", policy.Resource{
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
			if len(r.KeyedMeta) == 0 {
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
			if err != nil {
				continue
			}
			plaintext = opened
		}

		text := string(plaintext)
		if qLower != "" && !strings.Contains(strings.ToLower(text), qLower) {
			continue
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
func (s *Service) Prove(_ context.Context, shard string, seq int64) (*contracts.ProofBundle, *contracts.VerifyResult, error) {
	rec, err := s.db.GetFragment(shard, seq)
	if err != nil {
		return nil, nil, err
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
	vr := contracts.VerifyFragment(*bundle, s.port.Tracker())
	return bundle, &vr, nil
}

// GroundedAnswerInput requests a grounded, receipt-backed answer from claims citing memory fragments.
type GroundedAnswerInput struct {
	Claims      []reasoning.Claim         `json:"claims"`
	Mode        string                    `json:"mode,omitempty"` // remove (default) | flag
	Principal   policy.Principal          `json:"principal,omitempty"`
	CallerPub   string                    `json:"callerPub,omitempty"`
	Delegations []reasoning.Delegation    `json:"delegations,omitempty"`
	Payment     *reasoning.PaymentSummary `json:"payment,omitempty"`
	AutoAnchor  bool                      `json:"autoAnchor,omitempty"`
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
	if callerPub == "" {
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

	// Commit receipt into __receipts__ shard.
	remRes, err := s.Remember(ctx, RememberInput{
		Shard:      ReceiptsShard,
		Text:       string(bodyJSON),
		Source:     "receipt",
		Class:      "receipt",
		AutoAnchor: in.AutoAnchor,
	})
	if err != nil {
		return nil, err
	}
	cites := make([]store.CitationEdge, 0, len(refs))
	for _, r := range refs {
		cites = append(cites, store.CitationEdge{Shard: r.Shard, Seq: r.Seq, ReceiptHash: rcpHash})
	}
	if err := s.db.SaveReceipt(store.ReceiptRecord{
		ReceiptHash: rcpHash,
		ShardSeq:    remRes.Seq,
		BodyJSON:    bodyJSON,
		AnchorTxID:  remRes.AnchorTxID,
		CreatedAt:   rcp.Timestamp,
	}, cites); err != nil {
		return nil, err
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
		AnchorTxID:   remRes.AnchorTxID,
	}, nil
}

// ReceiptVerification is the traffic-light verdict returned by VerifyReceipt and GET /v1/verify/{token}.
type ReceiptVerification struct {
	Verdict          string                    `json:"verdict"` // green | amber | red
	Reason           string                    `json:"reason"`
	ReceiptHash      string                    `json:"receiptHash"`
	ReceiptToken     string                    `json:"receiptToken"`
	Receipt          *reasoning.Receipt        `json:"receipt,omitempty"`
	ReceiptAnchored  bool                      `json:"receiptAnchored"`
	CitationsChecked int                       `json:"citationsChecked"`
	CitationsMined   int                       `json:"citationsMined"`
	CitationsMissing int                       `json:"citationsMissing"`
	TruthNote        string                    `json:"truthNote"`
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

	out := &ReceiptVerification{
		ReceiptHash:     h,
		ReceiptToken:    tok,
		Receipt:         &rcp,
		ReceiptAnchored: rec.AnchorTxID != "",
		TruthNote:       reasoning.TruthNote,
	}
	for _, ref := range rcp.FragmentRefs {
		out.CitationsChecked++
		f, fErr := s.db.GetFragment(ref.Shard, ref.Seq)
		if fErr != nil || f.Forgotten {
			out.CitationsMissing++
			continue
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
		out.Reason = fmt.Sprintf("%d cited fragment(s) missing or forgotten", out.CitationsMissing)
	case out.CitationsChecked > 0 && out.CitationsMined == out.CitationsChecked:
		out.Verdict = "green"
		out.Reason = "all cited fragments SPV-verified against mined anchors"
	default:
		out.Verdict = "amber"
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

// ExportShardPack bundles shard fragments and anchors into a content-addressed BRC-167 CHIRP closure.
func (s *Service) ExportShardPack(_ context.Context, shard string) (*ShardPackResult, error) {
	frags, err := s.db.ListFragments(shard)
	if err != nil {
		return nil, err
	}
	anchors, err := s.db.ListAnchors(shard)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	u, root, err := bstack.StoreChirp(s.objects, raw)
	if err != nil {
		return nil, err
	}
	return &ShardPackResult{
		Shard:    shard,
		ChirpURL: u,
		RootHex:  hex.EncodeToString(root[:]),
		Size:     len(raw),
	}, nil
}

// ImportShardPack resolves and verifies a CHIRP shard pack closure and imports its fragments into targetShard.
func (s *Service) ImportShardPack(_ context.Context, chirpURL, targetShard string) (int, error) {
	raw, err := bstack.ResolveChirp(s.objects, chirpURL)
	if err != nil {
		return 0, err
	}
	var pack ShardPack
	if err := json.Unmarshal(raw, &pack); err != nil {
		return 0, fmt.Errorf("memory: decode shard pack: %w", err)
	}
	if targetShard == "" {
		targetShard = pack.Shard
	}
	imported := 0
	for _, f := range pack.Fragments {
		f.Shard = targetShard
		f.BatchN = -1
		f.LeafIndex = -1
		if _, dedup, err := s.db.AppendFragment(f); err == nil && !dedup {
			imported++
		}
	}
	return imported, nil
}
