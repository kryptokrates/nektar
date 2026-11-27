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
		if anchors, aErr := s.db.ListAnchors(repo); aErr == nil && len(anchors) > 0 {
			anchorTx = anchors[len(anchors)-1].TxID
		}
	}
	ontID := ref.OntologyID
	if !found || ontID == "" {
		ontID = reasoning.CanonicalOntologyID(fin.Domain, anchorTx, fin.Hash)
	}
	return &LoadedOntology{
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
	}, nil
}
