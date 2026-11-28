package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/reasoning"
)

const manifestOntologyKVKey = "ontology_manifest_refs_v1"

// OntologyRepoRef identifies a versioned reasoning.OntologyManifest committed in an `ont-<domain>` repo.
type OntologyRepoRef struct {
	OntologyID   string `json:"ontologyId"`
	Domain       string `json:"domain"`
	Repo         string `json:"repo"`
	CommitHash   string `json:"commitHash,omitempty"`
	AnchorTxID   string `json:"anchorTxid,omitempty"`
	ManifestHash string `json:"manifestHash"`
}

// LoadedOntology pairs an OntologyRepoRef with its finalized manifest and compiled Mangle rules.
type LoadedOntology struct {
	Ref      OntologyRepoRef            `json:"ref"`
	Manifest reasoning.OntologyManifest `json:"manifest"`
	Rules    []string                   `json:"rules,omitempty"`
}

// CommitOntology finalizes a reasoning.OntologyManifest, commits it into `ont-<domain>` (`manifest.yaml`, Public: true),
// optionally anchors it on BSV so its OntologyID binds the NektarAnchor txid, and persists its ref index.
func (s *Service) CommitOntology(ctx context.Context, man reasoning.OntologyManifest, autoAnchor bool) (*LoadedOntology, error) {
	fin, rules, _, err := reasoning.FinalizeOntologyManifest(man)
	if err != nil {
		return nil, err
	}
	rawYAML, err := reasoning.MarshalOntologyManifestYAML(*fin)
	if err != nil {
		return nil, err
	}
	repo := OntologyRepoPrefix + reasoning.NormalizeOntologyDomain(fin.Domain)
	if err := ValidateShardName(repo, false); err != nil {
		return nil, err
	}
	res, err := s.Remember(ctx, RememberInput{
		Shard:         repo,
		Path:          "manifest.yaml",
		MediaType:     "application/yaml",
		Text:          string(rawYAML),
		CommitMessage: fmt.Sprintf("ontology: commit %s (%s)", fin.Domain, shortHex8(fin.Hash)),
		Source:        "ontology",
		Class:         "ontology:manifest",
		Public:        true,
		AutoAnchor:    autoAnchor,
	})
	if err != nil {
		return nil, err
	}
	ontID := reasoning.CanonicalOntologyID(fin.Domain, res.AnchorTxID, fin.Hash)
	ref := OntologyRepoRef{
		OntologyID:   ontID,
		Domain:       fin.Domain,
		Repo:         repo,
		CommitHash:   res.CommitHash,
		AnchorTxID:   res.AnchorTxID,
		ManifestHash: fin.Hash,
	}
	reg := make(map[string]OntologyRepoRef)
	if s.db != nil {
		_, _ = s.db.GetJSON(manifestOntologyKVKey, &reg)
		reg[ontID] = ref
		reg[fin.Hash] = ref
		reg[repo] = ref
		reg[fin.Domain] = ref
		_ = s.db.SetJSON(manifestOntologyKVKey, reg)
	}
	return &LoadedOntology{
		Ref:      ref,
		Manifest: *fin,
		Rules:    rules,
	}, nil
}

// ResolveOntologyPredicateSet returns the exact declared predicate set for ontID if this node holds
// the matching JevGraph CompiledOntology or reasoning.OntologyManifest locally, else nil.
func (s *Service) ResolveOntologyPredicateSet(ctx context.Context, ontID string) map[string]struct{} {
	key := strings.TrimSpace(ontID)
	if key == "" {
		return nil
	}
	if comp, _, err := s.LoadDomainOntology(ctx, key); err == nil && comp != nil {
		if ps := comp.PredicateSet(); len(ps) > 0 {
			return ps
		}
	}
	if loaded, err := s.ResolveOntology(ctx, key); err == nil && loaded != nil {
		if ps := loaded.Manifest.PredicateSet(); len(ps) > 0 {
			return ps
		}
	}
	return nil
}

// ResolveOntology resolves a committed reasoning.OntologyManifest by domain, `ont-<domain>` repo name, or canonical OntologyID.
func (s *Service) ResolveOntology(ctx context.Context, key string) (*LoadedOntology, error) {
	k := strings.TrimSpace(key)
	if k == "" {
		return nil, errors.New("memory: ontology key is required")
	}
	reg := make(map[string]OntologyRepoRef)
	if s.db != nil {
		_, _ = s.db.GetJSON(manifestOntologyKVKey, &reg)
	}
	var ref OntologyRepoRef
	found := false
	if r, ok := reg[k]; ok {
		ref = r
		found = true
	} else if r, ok := reg[strings.ToLower(k)]; ok {
		ref = r
		found = true
	} else {
		dom := reasoning.NormalizeOntologyDomain(k)
		if r, ok := reg[dom]; ok {
			ref = r
			found = true
		} else if r, ok := reg[OntologyRepoPrefix+dom]; ok {
			ref = r
			found = true
		}
	}
	repo := ref.Repo
	if repo == "" {
		repo = OntologyRepoPrefix + reasoning.NormalizeOntologyDomain(k)
	}
	if br, bErr := s.db.GetMemBranch(repo, memrepo.DefaultBranch); bErr == nil && br.HeadCommit != "" {
		cacheKey := repo + "@" + br.HeadCommit + "|" + ref.AnchorTxID
		s.ontCacheMu.RLock()
		cached := s.manifestCache[cacheKey]
		s.ontCacheMu.RUnlock()
		if cached != nil && cached.loaded != nil {
			cp := *cached.loaded
			return &cp, nil
		}
	}
	c, tr, blobs, err := s.RepoTree(ctx, repo, "", "")
	if err != nil || tr == nil {
		if err == nil {
			err = fmt.Errorf("memory: ontology repo %q not found", repo)
		}
		return nil, err
	}
	var raw []byte
	for _, e := range tr.Entries {
		if e.Path == "manifest.yaml" || e.Path == "ontology.yaml" {
			if b, ok := blobs[e.BlobHash]; ok && !b.Encrypted && !b.Forgotten {
				raw = memrepo.ResolveBlobBytes(b, s.objects)
				break
			}
		}
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("memory: ontology manifest not found in repo %q", repo)
	}
	fin, rules, _, err := reasoning.ParseOntologyManifest(raw)
	if err != nil {
		return nil, err
	}
	anchorTx := ref.AnchorTxID
	commitHash := ref.CommitHash
	if c != nil {
		if commitHash == "" {
			commitHash = c.CommitHash
		}
		if anchorTx == "" {
			anchorTx = c.AnchorTxID
		}
	}
	if anchorTx == "" {
		if anchors, aErr := s.db.ListAnchorSummaries(repo); aErr == nil && len(anchors) > 0 {
			anchorTx = anchors[len(anchors)-1].TxID
		}
	}
	ontID := ref.OntologyID
	if !found || ontID == "" {
		ontID = reasoning.CanonicalOntologyID(fin.Domain, anchorTx, fin.Hash)
	}
	loaded := &LoadedOntology{
		Ref: OntologyRepoRef{
			OntologyID:   ontID,
			Domain:       fin.Domain,
			Repo:         repo,
			CommitHash:   commitHash,
			AnchorTxID:   anchorTx,
			ManifestHash: fin.Hash,
		},
		Manifest: *fin,
		Rules:    rules,
	}
	if commitHash != "" {
		s.ontCacheMu.Lock()
		if s.manifestCache == nil || len(s.manifestCache) >= 64 {
			s.manifestCache = make(map[string]*cachedManifestEntry, 16)
		}
		cp := *loaded
		s.manifestCache[repo+"@"+commitHash+"|"+ref.AnchorTxID] = &cachedManifestEntry{loaded: &cp}
		s.ontCacheMu.Unlock()
	}
	return loaded, nil
}
