package reasoning

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/lightwebinc/bcommon/cbor"
	"gopkg.in/yaml.v3"

	"github.com/kryptokrates/nektar/internal/core"
)

// OntologyEntity declares one canonical entity & alias list for O(n) Aho-Corasick candidate blocking.
type OntologyEntity struct {
	ID      string   `json:"id" yaml:"id"`
	Type    string   `json:"type" yaml:"type"`
	Aliases []string `json:"aliases,omitempty" yaml:"aliases,omitempty"`
}

// OntologyPredicate declares an allowed binary relation with optional DL-Lite domain & range classes.
type OntologyPredicate struct {
	ID          string `json:"id" yaml:"id"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	DomainClass string `json:"domainClass,omitempty" yaml:"domainClass,omitempty"`
	RangeClass  string `json:"rangeClass,omitempty" yaml:"rangeClass,omitempty"`
}

// OntologyManifest is the versioned, CBOR-hashed TBox specification committed into `ont-<domain>` Memory Repos.
// Its canonical network ontology_id binds its NektarAnchor txid (when anchored) and canonical Hash.
type OntologyManifest struct {
	Version    int                 `json:"v" yaml:"v"`
	Domain     string              `json:"domain" yaml:"domain"`
	Title      string              `json:"title" yaml:"title"`
	Entities   []OntologyEntity    `json:"entities,omitempty" yaml:"entities,omitempty"`
	Predicates []OntologyPredicate `json:"predicates,omitempty" yaml:"predicates,omitempty"`
	Axioms     []OntologyAxiom     `json:"axioms,omitempty" yaml:"axioms,omitempty"`
	Hash       string              `json:"hash,omitempty" yaml:"hash,omitempty"`
}

// JSONLDImporter is an optional extension point for external W3C JSON-LD / URDNA2015 ontology datasets
// that require blank-node canonicalization before importing into OntologyManifest / Triple sets.
type JSONLDImporter interface {
	ImportJSONLD(raw []byte) ([]Triple, error)
}

// NormalizeDomain canonicalizes an ontology domain label to lowercase trimmed ASCII tokens.
func NormalizeOntologyDomain(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	d = strings.TrimPrefix(d, "ont-")
	d = strings.TrimPrefix(d, "#")
	return d
}

// NormalizeOntologyManifest returns a deterministically sorted deep copy of m with clean fields.
func NormalizeOntologyManifest(m OntologyManifest) OntologyManifest {
	out := OntologyManifest{
		Version: m.Version,
		Domain:  NormalizeOntologyDomain(m.Domain),
		Title:   strings.TrimSpace(m.Title),
	}
	if out.Version <= 0 {
		out.Version = 1
	}
	if out.Title == "" && out.Domain != "" {
		out.Title = "Nektar " + out.Domain + " Ontology"
	}

	seenEnt := make(map[string]bool, len(m.Entities))
	for _, e := range m.Entities {
		id := strings.TrimSpace(e.ID)
		if id == "" || seenEnt[id] {
			continue
		}
		seenEnt[id] = true
		seenAl := make(map[string]bool, len(e.Aliases))
		aliases := make([]string, 0, len(e.Aliases))
		for _, a := range e.Aliases {
			a = strings.TrimSpace(a)
			if a == "" || seenAl[strings.ToLower(a)] {
				continue
			}
			seenAl[strings.ToLower(a)] = true
			aliases = append(aliases, a)
		}
		sort.Strings(aliases)
		out.Entities = append(out.Entities, OntologyEntity{
			ID:      id,
			Type:    strings.TrimSpace(e.Type),
			Aliases: aliases,
		})
	}
	sort.Slice(out.Entities, func(i, j int) bool { return out.Entities[i].ID < out.Entities[j].ID })

	seenPred := make(map[string]bool, len(m.Predicates))
	for _, p := range m.Predicates {
		id := strings.TrimSpace(p.ID)
		if id == "" || seenPred[id] {
			continue
		}
		seenPred[id] = true
		out.Predicates = append(out.Predicates, OntologyPredicate{
			ID:          id,
			Description: strings.TrimSpace(p.Description),
			DomainClass: strings.TrimSpace(p.DomainClass),
			RangeClass:  strings.TrimSpace(p.RangeClass),
		})
	}
	sort.Slice(out.Predicates, func(i, j int) bool { return out.Predicates[i].ID < out.Predicates[j].ID })

	for _, ax := range m.Axioms {
		out.Axioms = append(out.Axioms, OntologyAxiom{
			ID:       strings.TrimSpace(ax.ID),
			Kind:     ax.Kind,
			Subject:  strings.TrimSpace(ax.Subject),
			Object:   strings.TrimSpace(ax.Object),
			RuleText: strings.TrimSpace(ax.RuleText),
		})
	}
	sort.Slice(out.Axioms, func(i, j int) bool {
		if out.Axioms[i].Kind != out.Axioms[j].Kind {
			return out.Axioms[i].Kind < out.Axioms[j].Kind
		}
		if out.Axioms[i].Subject != out.Axioms[j].Subject {
			return out.Axioms[i].Subject < out.Axioms[j].Subject
		}
		if out.Axioms[i].Object != out.Axioms[j].Object {
			return out.Axioms[i].Object < out.Axioms[j].Object
		}
		return out.Axioms[i].RuleText < out.Axioms[j].RuleText
	})
	return out
}

// ComputeOntologyManifestHash computes the deterministic RFC 8949 CBOR commitment hash for m.
func ComputeOntologyManifestHash(m OntologyManifest) (string, error) {
	norm := NormalizeOntologyManifest(m)
	if norm.Domain == "" {
		return "", errors.New("ontology: manifest domain is required")
	}

	entArr := make([]cbor.Value, len(norm.Entities))
	for i, e := range norm.Entities {
		al := make([]cbor.Value, len(e.Aliases))
		for j, a := range e.Aliases {
			al[j] = a
		}
		entArr[i] = cbor.Map{
			{Key: uint64(0), Val: e.ID},
			{Key: uint64(1), Val: e.Type},
			{Key: uint64(2), Val: al},
		}
	}
	predArr := make([]cbor.Value, len(norm.Predicates))
	for i, p := range norm.Predicates {
		predArr[i] = cbor.Map{
			{Key: uint64(0), Val: p.ID},
			{Key: uint64(1), Val: p.Description},
			{Key: uint64(2), Val: p.DomainClass},
			{Key: uint64(3), Val: p.RangeClass},
		}
	}
	axArr := make([]cbor.Value, len(norm.Axioms))
	for i, ax := range norm.Axioms {
		axArr[i] = cbor.Map{
			{Key: uint64(0), Val: ax.ID},
			{Key: uint64(1), Val: string(ax.Kind)},
			{Key: uint64(2), Val: ax.Subject},
			{Key: uint64(3), Val: ax.Object},
			{Key: uint64(4), Val: ax.RuleText},
		}
	}
	h, err := core.HashCBOR(core.TagOntologyManifestV1, cbor.Map{
		{Key: uint64(0), Val: uint64(norm.Version)},
		{Key: uint64(1), Val: norm.Domain},
		{Key: uint64(2), Val: norm.Title},
		{Key: uint64(3), Val: entArr},
		{Key: uint64(4), Val: predArr},
		{Key: uint64(5), Val: axArr},
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h), nil
}

// EffectiveAxioms expands Predicate domain/range declarations alongside explicit Axioms.
func (m OntologyManifest) EffectiveAxioms() []OntologyAxiom {
	norm := NormalizeOntologyManifest(m)
	out := make([]OntologyAxiom, 0, len(norm.Axioms)+len(norm.Predicates)*2)
	out = append(out, norm.Axioms...)
	for _, p := range norm.Predicates {
		if p.DomainClass != "" {
			out = append(out, OntologyAxiom{
				ID:      "dom:" + p.ID,
				Kind:    AxiomDomain,
				Subject: p.ID,
				Object:  p.DomainClass,
			})
		}
		if p.RangeClass != "" {
			out = append(out, OntologyAxiom{
				ID:      "ran:" + p.ID,
				Kind:    AxiomRange,
				Subject: p.ID,
				Object:  p.RangeClass,
			})
		}
	}
	return out
}

// FinalizeOntologyManifest validates, compiles all axioms to stratified Mangle Datalog,
// checks TBox consistency, and returns the finalized manifest (with Hash populated) + compiled rules.
func FinalizeOntologyManifest(m OntologyManifest) (*OntologyManifest, []string, []OntologyAxiom, error) {
	norm := NormalizeOntologyManifest(m)
	if norm.Domain == "" {
		return nil, nil, nil, errors.New("ontology: manifest domain is required")
	}
	effAxioms := norm.EffectiveAxioms()
	compiledRules := make([]string, 0, len(effAxioms))
	finalAxioms := make([]OntologyAxiom, 0, len(effAxioms))
	seenRule := make(map[string]bool)
	for i, ax := range effAxioms {
		fin, rules, err := FinalizeAxiom(ax)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("ontology: axiom %d (%s) invalid: %w", i, ax.ID, err)
		}
		finalAxioms = append(finalAxioms, fin)
		for _, r := range rules {
			if !seenRule[r] {
				seenRule[r] = true
				compiledRules = append(compiledRules, r)
			}
		}
	}
	if len(compiledRules) > 0 {
		rep, err := CheckOntologyConsistency(nil, compiledRules)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("ontology: TBox consistency check failed: %w", err)
		}
		if rep != nil && !rep.Consistent {
			return nil, nil, nil, fmt.Errorf("ontology: inconsistent TBox (conflicts=%v violations=%v)", rep.Conflicts, rep.DeonticViolations)
		}
	}
	h, err := ComputeOntologyManifestHash(norm)
	if err != nil {
		return nil, nil, nil, err
	}
	norm.Hash = h
	return &norm, compiledRules, finalAxioms, nil
}

// ParseOntologyManifest parses a YAML or JSON OntologyManifest and validates/finalizes it.
func ParseOntologyManifest(raw []byte) (*OntologyManifest, []string, []OntologyAxiom, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil, nil, errors.New("ontology: empty manifest")
	}
	var m OntologyManifest
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
			return nil, nil, nil, fmt.Errorf("ontology: parse JSON manifest: %w", err)
		}
	} else {
		if err := yaml.Unmarshal([]byte(trimmed), &m); err != nil {
			return nil, nil, nil, fmt.Errorf("ontology: parse YAML manifest: %w", err)
		}
	}
	return FinalizeOntologyManifest(m)
}

// MarshalOntologyManifestYAML encodes m deterministically as YAML for Git Memory Repo commits.
func MarshalOntologyManifestYAML(m OntologyManifest) ([]byte, error) {
	fin, _, _, err := FinalizeOntologyManifest(m)
	if err != nil {
		return nil, err
	}
	return yaml.Marshal(fin)
}

// ComputeTripleSetHash computes a deterministic, order-independent RFC 8949 CBOR hash over a set of triples.
// Two agents extracting the same triples in any order produce the exact same 32-byte hex digest.
func ComputeTripleSetHash(triples []Triple) (string, error) {
	seen := make(map[string]bool, len(triples))
	hashes := make([]string, 0, len(triples))
	for i, tr := range triples {
		fin, err := FinalizeTriple(tr)
		if err != nil {
			return "", fmt.Errorf("ontology: triple %d invalid: %w", i, err)
		}
		if !seen[fin.Hash] {
			seen[fin.Hash] = true
			hashes = append(hashes, fin.Hash)
		}
	}
	sort.Strings(hashes)
	arr := make([]cbor.Value, len(hashes))
	for i, h := range hashes {
		arr[i] = h
	}
	digest, err := core.HashCBOR(core.TagTripleV1, cbor.Map{
		{Key: uint64(0), Val: uint64(1)},
		{Key: uint64(1), Val: arr},
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(digest), nil
}

// CanonicalOntologyID formats the canonical BSV-pinned ontology identifier from anchor txid and manifest hash.
func CanonicalOntologyID(domain, anchorTxID, manifestHash string) string {
	dom := NormalizeOntologyDomain(domain)
	shortHash := manifestHash
	if len(shortHash) > 16 {
		shortHash = shortHash[:16]
	}
	tx := strings.ToLower(strings.TrimSpace(anchorTxID))
	if tx != "" {
		return fmt.Sprintf("ont:%s:%s", tx, shortHash)
	}
	return fmt.Sprintf("ont:local:%s:%s", dom, shortHash)
}

// DefaultOntologyManifest returns built-in starter TBox manifests for core Nektar domains (ops, bsv, agents).
func DefaultOntologyManifest(domain string) OntologyManifest {
	dom := NormalizeOntologyDomain(domain)
	switch dom {
	case "bsv":
		return OntologyManifest{
			Version: 1,
			Domain:  "bsv",
			Title:   "BSV Protocol, SPV & Rúnar Covenant Ontology",
			Entities: []OntologyEntity{
				{ID: "bsv:spv", Type: "Protocol", Aliases: []string{"SPV", "BUMP", "BEEF", "AtomicBEEF", "MerklePath"}},
				{ID: "bsv:runar", Type: "Toolchain", Aliases: []string{"Rúnar", "Runar", "NektarAnchor", "DisclosureSale", "NektarCheckpointBond", "NektarAgentVault"}},
				{ID: "bsv:brc29", Type: "Protocol", Aliases: []string{"BRC-29", "BRC29"}},
				{ID: "bsv:brc369", Type: "Protocol", Aliases: []string{"BRC-369", "BRC369", "keyed"}},
				{ID: "bsv:brc100", Type: "Protocol", Aliases: []string{"BRC-100", "BRC100", "BSV Desktop", "Metanet Desktop"}},
				{ID: "bsv:brc105", Type: "Protocol", Aliases: []string{"BRC-105", "BRC105", "402"}},
				{ID: "bsv:overlay", Type: "Infrastructure", Aliases: []string{"Overlay", "tm_nektar", "ls_nektar", "Hub", "Spine", "Leaf"}},
				{ID: "bsv:teranode", Type: "Infrastructure", Aliases: []string{"Teranode", "ARC", "Broadcaster", "WhatsOnChain", "Chaintracks"}},
			},
			Predicates: []OntologyPredicate{
				{ID: "requires", Description: "Subject requires Object to operate safely", DomainClass: "Protocol", RangeClass: "Protocol"},
				{ID: "verifies", Description: "Subject cryptographically verifies Object"},
				{ID: "fixes", Description: "Subject resolves an error or vulnerability in Object"},
				{ID: "supersedes", Description: "Subject replaces an older Object specification"},
				{ID: "incompatible_with", Description: "Subject conflicts with Object"},
				{ID: "mitigates", Description: "Subject mitigates an attack or failure mode"},
			},
			Axioms: []OntologyAxiom{
				{ID: "bsv-proto-sub", Kind: AxiomSubClassOf, Subject: "Toolchain", Object: "Protocol"},
				{ID: "bsv-disjoint-dep", Kind: AxiomDisjointWith, Subject: "Deprecated", Object: "Recommended"},
			},
		}
	case "agents":
		return OntologyManifest{
			Version: 1,
			Domain:  "agents",
			Title:   "AI Agent Tooling, MCP, A2A & Reasoning Ontology",
			Entities: []OntologyEntity{
				{ID: "agent:mcp", Type: "Protocol", Aliases: []string{"MCP", "Streamable HTTP", "stdio", "go-sdk"}},
				{ID: "agent:a2a", Type: "Protocol", Aliases: []string{"A2A", "agent.json"}},
				{ID: "agent:cedar", Type: "PolicyEngine", Aliases: []string{"Cedar", "cedar-go", "PolicySet"}},
				{ID: "agent:mangle", Type: "Reasoner", Aliases: []string{"Mangle", "mangle-go", "Datalog"}},
				{ID: "agent:restate", Type: "Runtime", Aliases: []string{"Restate", "restate-server", "StandingOrder", "BuyerDeal", "SellerDeal", "SleepCycle"}},
				{ID: "agent:jev", Type: "Matcher", Aliases: []string{"Jev", "System One", "OpenJev", "Decis", "kev-0.8b"}},
				{ID: "agent:claude_code", Type: "Client", Aliases: []string{"Claude Code", "claude-code"}},
				{ID: "agent:codex", Type: "Client", Aliases: []string{"Codex", "Codex CLI"}},
				{ID: "agent:cursor", Type: "Client", Aliases: []string{"Cursor"}},
			},
			Predicates: []OntologyPredicate{
				{ID: "requires", Description: "Subject requires Object"},
				{ID: "integrates_with", Description: "Subject docks or integrates with Object"},
				{ID: "fixes", Description: "Subject resolves a failure in Object"},
				{ID: "supersedes", Description: "Subject supersedes Object"},
				{ID: "enforces", Description: "Subject enforces policy or invariant Object"},
			},
			Axioms: []OntologyAxiom{
				{ID: "ag-disjoint", Kind: AxiomDisjointWith, Subject: "Deprecated", Object: "Recommended"},
			},
		}
	default:
		if dom == "" {
			dom = "ops"
		}
		return OntologyManifest{
			Version: 1,
			Domain:  dom,
			Title:   "Software Engineering, Operations & CVE Workarounds Ontology (" + dom + ")",
			Entities: []OntologyEntity{
				{ID: "ops:go", Type: "Runtime", Aliases: []string{"Go", "Golang", "Go 1.27", "CGO_ENABLED=0"}},
				{ID: "ops:sqlite", Type: "Database", Aliases: []string{"SQLite", "modernc.org/sqlite", "FTS5", "WAL", "busy_timeout"}},
				{ID: "ops:svelte", Type: "Framework", Aliases: []string{"Svelte", "Svelte 5", "Vite"}},
				{ID: "ops:docker", Type: "Infrastructure", Aliases: []string{"Docker", "Caddy", "systemd", "WSL2"}},
				{ID: "ops:git", Type: "Toolchain", Aliases: []string{"Git", "go-git", "git-receive-pack"}},
				{ID: "ops:tls", Type: "Security", Aliases: []string{"TLS", "HTTPS", "SSRF", "OAuth 2.1", "CORS"}},
				{ID: "ops:cve", Type: "Advisory", Aliases: []string{"CVE", "vulnerability", "breaking change", "workaround", "regression"}},
			},
			Predicates: []OntologyPredicate{
				{ID: "requires", Description: "Subject requires setting, version, or flag Object"},
				{ID: "fixes", Description: "Subject fixes bug, race, or failure mode Object"},
				{ID: "breaks_in", Description: "Subject introduces a breaking change in Object"},
				{ID: "supersedes", Description: "Subject supersedes earlier workaround Object"},
				{ID: "mitigates", Description: "Subject mitigates security or operational risk Object"},
				{ID: "causes", Description: "Subject triggers error or failure mode Object"},
			},
			Axioms: []OntologyAxiom{
				{ID: "ops-disjoint", Kind: AxiomDisjointWith, Subject: "Deprecated", Object: "Recommended"},
			},
		}
	}
}
