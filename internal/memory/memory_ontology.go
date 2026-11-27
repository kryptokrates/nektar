package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/jevgraph"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

const (
	OntologyRepoPrefix    = "ont-"
	BridgeRepoPrefix      = "bridge-"
	ontologyRegistryKVKey = "domain_ontologies_v1"
	bridgeRegistryKVKey   = "ontology_bridges_v1"
)

// DomainOntologyInfo summarizes a first-class TBox Memory Repo (`ont-<domain>`).
type DomainOntologyInfo struct {
	Repo          string `json:"repo"`
	OntologyID    string `json:"ontologyId"`
	Name          string `json:"name"`
	Namespace     string `json:"namespace,omitempty"`
	Description   string `json:"description,omitempty"`
	ClassCount    int    `json:"classCount"`
	RelationCount int    `json:"relationCount"`
	EntityCount   int    `json:"entityCount"`
	Seq           int64  `json:"seq,omitempty"`
	CommitHash    string `json:"commitHash,omitempty"`
	AnchorTxID    string `json:"anchorTxid,omitempty"`
	UpdatedAt     int64  `json:"updatedAt,omitempty"`
}

// BridgeInfo summarizes a committed cross-ontology Bridge Shard (`bridge-<src8>-<dst8>`).
type BridgeInfo struct {
	Repo             string `json:"repo"`
	BridgeID         string `json:"bridgeId"`
	SourceOntologyID string `json:"sourceOntologyId"`
	TargetOntologyID string `json:"targetOntologyId"`
	AcceptedCount    int    `json:"acceptedCount"`
	ReviewCount      int    `json:"reviewCount"`
	Seq              int64  `json:"seq,omitempty"`
	CommitHash       string `json:"commitHash,omitempty"`
	AnchorTxID       string `json:"anchorTxid,omitempty"`
	UpdatedAt        int64  `json:"updatedAt,omitempty"`
}

func sanitizeSlug(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else if r == ' ' || r == '/' || r == ':' {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_.")
	if out == "" {
		return "domain"
	}
	return out
}

func normalizeOntologyRepoName(repoName, fallbackName string) string {
	r := strings.TrimSpace(repoName)
	if r == "" {
		r = OntologyRepoPrefix + sanitizeSlug(fallbackName)
	} else if !strings.HasPrefix(r, OntologyRepoPrefix) {
		r = OntologyRepoPrefix + sanitizeSlug(r)
	}
	return r
}

func shortHex8(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if len(h) >= 8 {
		return h[:8]
	}
	if h == "" {
		return "00000000"
	}
	return h
}

// CommitDomainOntology validates, compiles, and commits a YAML/JSON TBox ontology as a first-class
// Memory Repo (`ont-<domain>`, Public: true) and indexes its canonical OntologyID in SQLite KV.
func (s *Service) CommitDomainOntology(ctx context.Context, repoName string, rawYAMLorJSON []byte, autoAnchor bool) (*DomainOntologyInfo, *jevgraph.CompiledOntology, error) {
	if len(strings.TrimSpace(string(rawYAMLorJSON))) == 0 {
		return nil, nil, errors.New("memory: ontology definition is empty")
	}
	ont, err := jevgraph.ParseOntologyAuto(rawYAMLorJSON)
	if err != nil {
		return nil, nil, err
	}
	compiled, err := jevgraph.Compile(ont)
	if err != nil {
		return nil, nil, err
	}
	repo := normalizeOntologyRepoName(repoName, compiled.Ontology.Name)
	if err := ValidateShardName(repo, false); err != nil {
		return nil, nil, err
	}

	res, err := s.Remember(ctx, RememberInput{
		Shard:         repo,
		Path:          "ontology.yaml",
		MediaType:     "application/yaml",
		Text:          string(rawYAMLorJSON),
		CommitMessage: fmt.Sprintf("ontology: commit %s (%s)", compiled.Ontology.Name, shortHex8(compiled.ID)),
		Source:        "ontology",
		Class:         "ontology:tbox",
		Public:        true,
		AutoAnchor:    autoAnchor,
	})
	if err != nil {
		return nil, nil, err
	}

	info := &DomainOntologyInfo{
		Repo:          repo,
		OntologyID:    compiled.ID,
		Name:          compiled.Ontology.Name,
		Namespace:     compiled.Ontology.Namespace,
		Description:   compiled.Ontology.Description,
		ClassCount:    len(compiled.Ontology.Classes),
		RelationCount: len(compiled.Ontology.Relations),
		EntityCount:   len(compiled.Ontology.Entities),
		Seq:           res.Seq,
		CommitHash:    res.CommitHash,
		AnchorTxID:    res.AnchorTxID,
		UpdatedAt:     time.Now().Unix(),
	}

	s.ontCacheMu.Lock()
	if s.ontCache == nil {
		s.ontCache = make(map[string]*jevgraph.CompiledOntology)
	}
	s.ontCache[repo] = compiled
	s.ontCache[compiled.ID] = compiled
	s.ontCacheMu.Unlock()

	reg := s.loadOntologyRegistry()
	reg[compiled.ID] = *info
	_ = s.db.SetJSON(ontologyRegistryKVKey, reg)
	return info, compiled, nil
}

func (s *Service) loadOntologyRegistry() map[string]DomainOntologyInfo {
	reg := make(map[string]DomainOntologyInfo)
	if s.db != nil {
		_, _ = s.db.GetJSON(ontologyRegistryKVKey, &reg)
	}
	return reg
}

func (s *Service) loadBridgeRegistry() map[string]BridgeInfo {
	reg := make(map[string]BridgeInfo)
	if s.db != nil {
		_, _ = s.db.GetJSON(bridgeRegistryKVKey, &reg)
	}
	return reg
}

func (s *Service) readOntologyFromRepo(ctx context.Context, repo string) (*jevgraph.CompiledOntology, *DomainOntologyInfo, error) {
	c, tr, blobs, err := s.RepoTree(ctx, repo, "", "")
	if err != nil || tr == nil {
		if err == nil {
			err = fmt.Errorf("memory: ontology repo %q has no tree", repo)
		}
		return nil, nil, err
	}
	var raw []byte
	for _, e := range tr.Entries {
		if e.Path == "ontology.yaml" || e.Path == "ontology.yml" || e.Path == "ontology.json" {
			if b, ok := blobs[e.BlobHash]; ok && !b.Encrypted && !b.Forgotten {
				raw = memrepo.ResolveBlobBytes(b, s.objects)
				break
			}
		}
	}
	if len(raw) == 0 {
		return nil, nil, fmt.Errorf("memory: ontology.yaml not found in repo %q", repo)
	}
	ont, err := jevgraph.ParseOntologyAuto(raw)
	if err != nil {
		return nil, nil, err
	}
	compiled, err := jevgraph.Compile(ont)
	if err != nil {
		return nil, nil, err
	}
	info := &DomainOntologyInfo{
		Repo:          repo,
		OntologyID:    compiled.ID,
		Name:          compiled.Ontology.Name,
		Namespace:     compiled.Ontology.Namespace,
		Description:   compiled.Ontology.Description,
		ClassCount:    len(compiled.Ontology.Classes),
		RelationCount: len(compiled.Ontology.Relations),
		EntityCount:   len(compiled.Ontology.Entities),
	}
	if c != nil {
		info.Seq = c.Seq
		info.CommitHash = c.CommitHash
		info.AnchorTxID = c.AnchorTxID
		info.UpdatedAt = c.Timestamp
	}
	s.ontCacheMu.Lock()
	if s.ontCache == nil {
		s.ontCache = make(map[string]*jevgraph.CompiledOntology)
	}
	s.ontCache[repo] = compiled
	s.ontCache[compiled.ID] = compiled
	s.ontCacheMu.Unlock()
	return compiled, info, nil
}

// LoadDomainOntology resolves a committed domain ontology by repo name (`ont-<domain>`), bare domain name, or 64-hex OntologyID.
func (s *Service) LoadDomainOntology(ctx context.Context, nameOrID string) (*jevgraph.CompiledOntology, *DomainOntologyInfo, error) {
	key := strings.TrimSpace(nameOrID)
	if key == "" {
		return nil, nil, errors.New("memory: ontology name or ontologyId is required")
	}
	lower := strings.ToLower(key)
	reg := s.loadOntologyRegistry()

	// 1. Direct match in KV registry by OntologyID, repo name, or bare name.
	var matchedRepo string
	if info, ok := reg[lower]; ok && info.Repo != "" {
		matchedRepo = info.Repo
	} else {
		prefixed := OntologyRepoPrefix + sanitizeSlug(lower)
		for _, info := range reg {
			if strings.EqualFold(info.Repo, key) || strings.EqualFold(info.Repo, prefixed) || strings.EqualFold(info.Name, key) {
				matchedRepo = info.Repo
				break
			}
		}
	}
	if matchedRepo != "" {
		return s.readOntologyFromRepo(ctx, matchedRepo)
	}

	// 2. Fallback scan of local repos (supports capsule-imported ont-* repos not yet in KV).
	candidates := []string{key}
	if !strings.HasPrefix(key, OntologyRepoPrefix) {
		candidates = append(candidates, OntologyRepoPrefix+sanitizeSlug(key))
	}
	for _, cand := range candidates {
		if ValidateShardName(cand, false) != nil {
			continue
		}
		if compiled, info, err := s.readOntologyFromRepo(ctx, cand); err == nil {
			reg[compiled.ID] = *info
			_ = s.db.SetJSON(ontologyRegistryKVKey, reg)
			return compiled, info, nil
		}
	}
	if allRepos, err := s.db.ListAllMemRepos(); err == nil {
		for _, r := range allRepos {
			if !strings.HasPrefix(r, OntologyRepoPrefix) {
				continue
			}
			compiled, info, rErr := s.readOntologyFromRepo(ctx, r)
			if rErr != nil {
				continue
			}
			reg[compiled.ID] = *info
			if strings.EqualFold(compiled.ID, lower) || strings.EqualFold(compiled.Ontology.Name, key) {
				_ = s.db.SetJSON(ontologyRegistryKVKey, reg)
				return compiled, info, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("memory: ontology %q not found", nameOrID)
}

// ListDomainOntologies returns every committed `ont-*` TBox Memory Repo on this node.
func (s *Service) ListDomainOntologies(ctx context.Context) ([]DomainOntologyInfo, error) {
	byID := make(map[string]DomainOntologyInfo)
	if repos, err := s.db.ListAllMemRepos(); err == nil {
		for _, r := range repos {
			if !strings.HasPrefix(r, OntologyRepoPrefix) {
				continue
			}
			if _, info, rErr := s.readOntologyFromRepo(ctx, r); rErr == nil && info != nil {
				byID[info.OntologyID] = *info
			}
		}
	}
	for id, info := range s.loadOntologyRegistry() {
		if _, exists := byID[id]; !exists {
			if _, live, rErr := s.readOntologyFromRepo(ctx, info.Repo); rErr == nil && live != nil {
				byID[live.OntologyID] = *live
			}
		}
	}
	out := make([]DomainOntologyInfo, 0, len(byID))
	for _, info := range byID {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].OntologyID < out[j].OntologyID
	})
	return out, nil
}

// CommitBridgeManifest persists a verified BridgeManifest into a first-class Bridge Shard Memory Repo.
func (s *Service) CommitBridgeManifest(ctx context.Context, bridgeRepo string, manifest *jevgraph.BridgeManifest, autoAnchor bool) (*BridgeInfo, error) {
	if manifest == nil {
		return nil, errors.New("memory: bridge manifest is nil")
	}
	srcID := strings.ToLower(strings.TrimSpace(manifest.SourceOntologyID))
	dstID := strings.ToLower(strings.TrimSpace(manifest.TargetOntologyID))
	if srcID == "" || dstID == "" {
		return nil, errors.New("memory: bridge requires sourceOntologyId and targetOntologyId")
	}
	cp := *manifest
	cp.SourceOntologyID = srcID
	cp.TargetOntologyID = dstID
	if cp.Version == 0 {
		cp.Version = 1
	}
	if cp.AdvisoryNote == "" {
		cp.AdvisoryNote = jevgraph.AdvisoryBridgeNote
	}
	if cp.CreatedAt == 0 {
		cp.CreatedAt = time.Now().Unix()
	}
	acc, rev, rej := 0, 0, 0
	for _, m := range cp.Mappings {
		switch m.Status {
		case jevgraph.EdgeAccepted:
			acc++
		case jevgraph.EdgeReview:
			rev++
		case jevgraph.EdgeRejected:
			rej++
		}
	}
	cp.AcceptedCount = acc
	cp.ReviewCount = rev
	cp.RejectedCount = rej

	bid, err := jevgraph.ComputeBridgeID(&cp)
	if err != nil {
		return nil, err
	}
	cp.BridgeID = bid

	repo := strings.TrimSpace(bridgeRepo)
	if repo == "" {
		repo = fmt.Sprintf("%s%s-%s", BridgeRepoPrefix, shortHex8(srcID), shortHex8(dstID))
	} else if !strings.HasPrefix(repo, BridgeRepoPrefix) {
		repo = BridgeRepoPrefix + sanitizeSlug(repo)
	}
	if err := ValidateShardName(repo, false); err != nil {
		return nil, err
	}
	rawJSON, err := core.CanonicalJSON(cp)
	if err != nil {
		return nil, err
	}
	res, err := s.Remember(ctx, RememberInput{
		Shard:         repo,
		Path:          "bridge.json",
		MediaType:     "application/json",
		Text:          string(rawJSON),
		CommitMessage: fmt.Sprintf("bridge: align %s -> %s (%d accepted)", shortHex8(srcID), shortHex8(dstID), cp.AcceptedCount),
		Source:        "ontology-bridge",
		Class:         "ontology:bridge",
		Public:        true,
		AutoAnchor:    autoAnchor,
	})
	if err != nil {
		return nil, err
	}
	info := &BridgeInfo{
		Repo:             repo,
		BridgeID:         cp.BridgeID,
		SourceOntologyID: srcID,
		TargetOntologyID: dstID,
		AcceptedCount:    cp.AcceptedCount,
		ReviewCount:      cp.ReviewCount,
		Seq:              res.Seq,
		CommitHash:       res.CommitHash,
		AnchorTxID:       res.AnchorTxID,
		UpdatedAt:        time.Now().Unix(),
	}
	reg := s.loadBridgeRegistry()
	reg[cp.BridgeID] = *info
	_ = s.db.SetJSON(bridgeRegistryKVKey, reg)
	return info, nil
}

// AlignAndCommitBridge aligns two committed domain ontologies via bounded Jev Choice+Noul + Mangle disjointness checks
// and commits the resulting BridgeManifest as a portable Bridge Shard Memory Repo.
func (s *Service) AlignAndCommitBridge(ctx context.Context, srcNameOrID, dstNameOrID, bridgeRepo string, autoAnchor bool) (*BridgeInfo, *jevgraph.BridgeManifest, error) {
	src, _, err := s.LoadDomainOntology(ctx, srcNameOrID)
	if err != nil {
		return nil, nil, fmt.Errorf("memory: load source ontology: %w", err)
	}
	dst, _, err := s.LoadDomainOntology(ctx, dstNameOrID)
	if err != nil {
		return nil, nil, fmt.Errorf("memory: load target ontology: %w", err)
	}
	manifest, err := jevgraph.AlignOntologies(ctx, s.JevEvaluator(), src, dst, jevgraph.AlignOptions{})
	if err != nil {
		return nil, nil, err
	}
	info, err := s.CommitBridgeManifest(ctx, bridgeRepo, manifest, autoAnchor)
	if err != nil {
		return nil, nil, err
	}
	return info, manifest, nil
}

func (s *Service) readBridgeFromRepo(ctx context.Context, repo string) (*jevgraph.BridgeManifest, *BridgeInfo, error) {
	c, tr, blobs, err := s.RepoTree(ctx, repo, "", "")
	if err != nil || tr == nil {
		if err == nil {
			err = fmt.Errorf("memory: bridge repo %q has no tree", repo)
		}
		return nil, nil, err
	}
	var raw []byte
	for _, e := range tr.Entries {
		if e.Path == "bridge.json" {
			if b, ok := blobs[e.BlobHash]; ok && !b.Encrypted && !b.Forgotten {
				raw = memrepo.ResolveBlobBytes(b, s.objects)
				break
			}
		}
	}
	if len(raw) == 0 {
		return nil, nil, fmt.Errorf("memory: bridge.json not found in repo %q", repo)
	}
	var m jevgraph.BridgeManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, err
	}
	if m.BridgeID == "" {
		m.BridgeID, _ = jevgraph.ComputeBridgeID(&m)
	}
	info := &BridgeInfo{
		Repo:             repo,
		BridgeID:         m.BridgeID,
		SourceOntologyID: strings.ToLower(strings.TrimSpace(m.SourceOntologyID)),
		TargetOntologyID: strings.ToLower(strings.TrimSpace(m.TargetOntologyID)),
		AcceptedCount:    m.AcceptedCount,
		ReviewCount:      m.ReviewCount,
	}
	if c != nil {
		info.Seq = c.Seq
		info.CommitHash = c.CommitHash
		info.AnchorTxID = c.AnchorTxID
		info.UpdatedAt = c.Timestamp
	}
	return &m, info, nil
}

// ListBridges returns every committed cross-ontology Bridge Shard Memory Repo on this node.
func (s *Service) ListBridges(ctx context.Context) ([]BridgeInfo, error) {
	byID := make(map[string]BridgeInfo)
	if repos, err := s.db.ListAllMemRepos(); err == nil {
		for _, r := range repos {
			if !strings.HasPrefix(r, BridgeRepoPrefix) {
				continue
			}
			if _, info, rErr := s.readBridgeFromRepo(ctx, r); rErr == nil && info != nil {
				byID[info.BridgeID] = *info
			}
		}
	}
	for id, info := range s.loadBridgeRegistry() {
		if _, exists := byID[id]; !exists {
			if _, live, rErr := s.readBridgeFromRepo(ctx, info.Repo); rErr == nil && live != nil {
				byID[live.BridgeID] = *live
			}
		}
	}
	out := make([]BridgeInfo, 0, len(byID))
	for _, info := range byID {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].BridgeID < out[j].BridgeID
	})
	return out, nil
}

// LoadBridgeRulesBetween finds a committed Bridge Shard linking srcOntID -> dstOntID (or a bidirectional
// equivalence bridge in reverse) and returns its manifest plus committed Mangle rules with FragmentRefs.
func (s *Service) LoadBridgeRulesBetween(ctx context.Context, srcOntID, dstOntID string) (*jevgraph.BridgeManifest, []reasoning.CommittedRule, error) {
	src := strings.ToLower(strings.TrimSpace(srcOntID))
	dst := strings.ToLower(strings.TrimSpace(dstOntID))
	if src == "" || dst == "" {
		return nil, nil, errors.New("memory: source and target ontologyIds are required")
	}
	bridges, err := s.ListBridges(ctx)
	if err != nil {
		return nil, nil, err
	}
	findMatch := func(a, b string, requireSymmetric bool) (*jevgraph.BridgeManifest, []reasoning.CommittedRule, bool) {
		for _, br := range bridges {
			if !strings.EqualFold(br.SourceOntologyID, a) || !strings.EqualFold(br.TargetOntologyID, b) {
				continue
			}
			m, info, rErr := s.readBridgeFromRepo(ctx, br.Repo)
			if rErr != nil || m == nil || info == nil {
				continue
			}
			if requireSymmetric {
				hasSym := false
				for _, mp := range m.Mappings {
					if mp.Status != jevgraph.EdgeAccepted {
						continue
					}
					p := mp.Triple.Predicate
					if p == jevgraph.PredEquivalentClass || p == jevgraph.PredEquivalentProperty || p == jevgraph.PredExactMatch {
						hasSym = true
						break
					}
				}
				if !hasSym {
					continue
				}
			}
			_, compiledRules, cErr := jevgraph.CompileBridgeRules(m)
			if cErr != nil {
				continue
			}
			ref := reasoning.FragmentRef{Shard: info.Repo, Seq: info.Seq}
			cr := make([]reasoning.CommittedRule, 0, len(compiledRules))
			for _, rt := range compiledRules {
				rc := ref
				cr = append(cr, reasoning.CommittedRule{
					RuleText: rt,
					Ref:      &rc,
					RuleHash: reasoning.ComputeRuleHash(rt),
				})
			}
			return m, cr, true
		}
		return nil, nil, false
	}
	if m, cr, ok := findMatch(src, dst, false); ok {
		return m, cr, nil
	}
	if m, cr, ok := findMatch(dst, src, true); ok {
		return m, cr, nil
	}
	return nil, nil, fmt.Errorf("memory: no committed bridge found between %s and %s", shortHex8(src), shortHex8(dst))
}

// ExtractWithOntology runs the 4-stage JevGraph extraction pipeline dry-run (without persisting),
// optionally checking functional/disjointness conflicts against existing accepted edges in shardForConflictCheck.
func (s *Service) ExtractWithOntology(ctx context.Context, ontNameOrID, text, shardForConflictCheck string, opts jevgraph.ExtractOptions) (*jevgraph.ExtractionResult, error) {
	compiled, _, err := s.LoadDomainOntology(ctx, ontNameOrID)
	if err != nil {
		return nil, err
	}
	var existing []reasoning.Triple
	if strings.TrimSpace(shardForConflictCheck) != "" {
		existing = s.activeShardTriples(shardForConflictCheck, compiled.ID)
	}
	return jevgraph.Extract(ctx, s.JevEvaluator(), compiled, text, existing, opts)
}

func (s *Service) activeShardTriples(shard, ontologyID string) []reasoning.Triple {
	rows, err := s.db.ListKnowledgeEdges(store.KnowledgeEdgeFilter{
		Shard:      shard,
		OntologyID: ontologyID,
		Status:     "accepted",
		Limit:      2000,
	})
	if err != nil || len(rows) == 0 {
		return nil
	}
	out := make([]reasoning.Triple, 0, len(rows))
	for _, r := range rows {
		out = append(out, reasoning.Triple{
			Subject:   r.Subject,
			Predicate: r.Predicate,
			Object:    r.Object,
			Hash:      r.TripleHash,
		})
	}
	return out
}

// ReviewKnowledgeEdge promotes (accept=true) or rejects (accept=false) a persisted knowledge edge.
// Promotions are strictly re-validated through the deterministic Mangle gate against the committed TBox,
// verbatim fragment plaintext, and active shard triples before status flips to "accepted".
func (s *Service) ReviewKnowledgeEdge(ctx context.Context, shard string, seq int64, tripleHash string, accept bool, reason string) (*store.KnowledgeEdgeRow, error) {
	if err := ValidateShardName(shard, false); err != nil {
		return nil, err
	}
	row, err := s.db.GetKnowledgeEdge(shard, seq, tripleHash)
	if err != nil {
		return nil, err
	}
	if !accept {
		why := strings.TrimSpace(reason)
		if why == "" {
			why = "operator_rejected"
		}
		if err := s.db.UpdateKnowledgeEdgeStatus(shard, seq, tripleHash, "rejected", why); err != nil {
			return nil, err
		}
		row.Status = "rejected"
		row.Reason = why
		return &row, nil
	}

	compiled, _, err := s.LoadDomainOntology(ctx, row.OntologyID)
	if err != nil {
		return nil, fmt.Errorf("memory: load ontology %s for review gate: %w", row.OntologyID, err)
	}
	frag, fErr := s.db.GetFragment(shard, seq)
	if fErr != nil || frag.Forgotten {
		return nil, fmt.Errorf("memory: source fragment %s#%d unavailable for review gate", shard, seq)
	}
	pt, ok := s.openFragmentPlaintext(ctx, frag, ownerPrincipal(), s.port.IdentityPubHex(), nil, false, false)
	if !ok {
		return nil, fmt.Errorf("memory: cannot open source fragment %s#%d plaintext for verbatim quote verification", shard, seq)
	}
	existingRows, _ := s.db.ListKnowledgeEdges(store.KnowledgeEdgeFilter{
		Shard:      shard,
		OntologyID: compiled.ID,
		Status:     "accepted",
		Limit:      2000,
	})
	var existing []reasoning.Triple
	for _, er := range existingRows {
		if er.Shard == shard && er.Seq == seq && strings.EqualFold(er.TripleHash, tripleHash) {
			continue
		}
		existing = append(existing, reasoning.Triple{
			Subject:   er.Subject,
			Predicate: er.Predicate,
			Object:    er.Object,
			Hash:      er.TripleHash,
		})
	}
	edge := jevgraph.ExtractedEdge{
		Triple: reasoning.Triple{
			Subject:   row.Subject,
			Predicate: row.Predicate,
			Object:    row.Object,
			Hash:      row.TripleHash,
		},
		SubjectType: row.SubjectType,
		ObjectType:  row.ObjectType,
		OntologyID:  row.OntologyID,
		Evidence: jevgraph.EvidenceSpan{
			StartOffset: row.StartOffset,
			EndOffset:   row.EndOffset,
			Quote:       row.EvidenceQuote,
		},
		Provenance: jevgraph.ExtractionProvenance{
			Extractor:    jevgraph.ExtractorVersion,
			ModelID:      row.ModelID,
			ConfidenceBP: row.ConfidenceBP,
			MarginBP:     row.MarginBP,
			SupportBP:    row.SupportBP,
			AdvisoryNote: jevgraph.AdvisoryExtractionNote,
		},
	}
	if _, vErr := jevgraph.ValidateEdgeAgainstOntology(compiled, string(pt), edge, existing, nil); vErr != nil {
		return nil, fmt.Errorf("memory: Mangle gate rejected edge promotion: %w", vErr)
	}
	why := strings.TrimSpace(reason)
	if why == "" {
		why = "operator_approved"
	}
	if err := s.db.UpdateKnowledgeEdgeStatus(shard, seq, tripleHash, "accepted", why); err != nil {
		return nil, err
	}
	row.Status = "accepted"
	row.Reason = why
	return &row, nil
}

// ListKnowledgeEdgesAuthorized lists indexed knowledge edges filtered by Cedar read authorization on each fragment.
func (s *Service) ListKnowledgeEdgesAuthorized(_ context.Context, filter store.KnowledgeEdgeFilter, principal policy.Principal) ([]store.KnowledgeEdgeRow, error) {
	rows, err := s.db.ListKnowledgeEdges(filter)
	if err != nil {
		return nil, err
	}
	if isOperatorPrincipal(principal) {
		return rows, nil
	}
	s.mu.RLock()
	polText := s.policyText
	s.mu.RUnlock()

	type fragKey struct {
		shard string
		seq   int64
	}
	allowed := make(map[fragKey]bool)
	out := make([]store.KnowledgeEdgeRow, 0, len(rows))
	for _, r := range rows {
		fk := fragKey{shard: r.Shard, seq: r.Seq}
		ok, seen := allowed[fk]
		if !seen {
			frag, fErr := s.db.GetFragment(r.Shard, r.Seq)
			if fErr != nil || frag.Forgotten || frag.BorgGroup != "" {
				ok = false
			} else {
				dec, aErr := policy.Authorize(polText, principal, "read", policy.Resource{
					Type:  "Fragment",
					ID:    fmt.Sprintf("%s#%d", frag.Shard, frag.Seq),
					Level: int64(frag.Level),
					Shard: frag.Shard,
					Class: frag.Class,
				})
				ok = aErr == nil && dec.Allow
			}
			allowed[fk] = ok
		}
		if ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// knowledgeEdgeToMangleAtoms emits strictly parsed ground Mangle atoms for an accepted KnowledgeEdgeRow.
func knowledgeEdgeToMangleAtoms(e store.KnowledgeEdgeRow) []string {
	var atoms []string
	if a, err := (reasoning.Triple{
		Subject:   e.Subject,
		Predicate: e.Predicate,
		Object:    e.Object,
	}).ToMangleAtoms(); err == nil {
		atoms = append(atoms, a...)
	}
	if strings.TrimSpace(e.SubjectType) != "" {
		if a, err := (reasoning.Triple{
			Subject:   e.Subject,
			Predicate: "rdf:type",
			Object:    e.SubjectType,
		}).ToMangleAtoms(); err == nil {
			atoms = append(atoms, a...)
		}
	}
	if strings.TrimSpace(e.ObjectType) != "" {
		if a, err := (reasoning.Triple{
			Subject:   e.Object,
			Predicate: "rdf:type",
			Object:    e.ObjectType,
		}).ToMangleAtoms(); err == nil {
			atoms = append(atoms, a...)
		}
	}
	return atoms
}

// domainOntologyCommittedRules gathers compiled Mangle rules (with FragmentRefs) for referenced domain
// ontologies AND any local committed Bridge Shards touching those ontologies.
func (s *Service) domainOntologyCommittedRules(ctx context.Context, ontIDs map[string]bool) []reasoning.CommittedRule {
	if len(ontIDs) == 0 {
		return nil
	}
	seenHash := make(map[string]bool)
	var out []reasoning.CommittedRule
	addRules := func(ref reasoning.FragmentRef, rules []string) {
		for _, rt := range rules {
			trimmed := strings.TrimSpace(rt)
			if trimmed == "" {
				continue
			}
			rh := reasoning.ComputeRuleHash(trimmed)
			if seenHash[rh] {
				continue
			}
			seenHash[rh] = true
			rc := ref
			out = append(out, reasoning.CommittedRule{
				RuleText: trimmed,
				Ref:      &rc,
				RuleHash: rh,
			})
		}
	}

	expandedIDs := make(map[string]bool, len(ontIDs))
	for id := range ontIDs {
		if id != "" {
			expandedIDs[strings.ToLower(id)] = true
		}
	}

	// Include any local Bridge Shards bridging from/to the cited ontologies.
	if bridges, err := s.ListBridges(ctx); err == nil {
		for _, br := range bridges {
			sID := strings.ToLower(br.SourceOntologyID)
			tID := strings.ToLower(br.TargetOntologyID)
			if !expandedIDs[sID] && !expandedIDs[tID] {
				continue
			}
			m, info, rErr := s.readBridgeFromRepo(ctx, br.Repo)
			if rErr != nil || m == nil || info == nil || info.Seq <= 0 {
				continue
			}
			if _, rules, cErr := jevgraph.CompileBridgeRules(m); cErr == nil && len(rules) > 0 {
				addRules(reasoning.FragmentRef{Shard: info.Repo, Seq: info.Seq}, rules)
				expandedIDs[sID] = true
				expandedIDs[tID] = true
			}
		}
	}

	// Include compiled TBox rules for every cited or bridged ontology.
	for id := range expandedIDs {
		compiled, info, err := s.LoadDomainOntology(ctx, id)
		if err != nil || compiled == nil || info == nil || info.Seq <= 0 {
			continue
		}
		addRules(reasoning.FragmentRef{Shard: info.Repo, Seq: info.Seq}, compiled.MangleRules)
	}
	return out
}

func edgeToStoreRow(shard string, seq int64, now int64, e jevgraph.ExtractedEdge) store.KnowledgeEdgeRow {
	return store.KnowledgeEdgeRow{
		Shard:         shard,
		Seq:           seq,
		TripleHash:    e.Triple.Hash,
		OntologyID:    strings.ToLower(strings.TrimSpace(e.OntologyID)),
		Subject:       e.Triple.Subject,
		Predicate:     e.Triple.Predicate,
		Object:        e.Triple.Object,
		SubjectType:   e.SubjectType,
		ObjectType:    e.ObjectType,
		Status:        string(e.Status),
		EvidenceQuote: e.Evidence.Quote,
		StartOffset:   e.Evidence.StartOffset,
		EndOffset:     e.Evidence.EndOffset,
		ConfidenceBP:  e.Provenance.ConfidenceBP,
		MarginBP:      e.Provenance.MarginBP,
		SupportBP:     e.Provenance.SupportBP,
		ModelID:       e.Provenance.ModelID,
		Reason:        e.Reason,
		CreatedAt:     now,
	}
}
