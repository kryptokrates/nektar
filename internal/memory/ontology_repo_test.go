package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestOntologyReposAndDeterministicIDs(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("chain.Open: %v", err)
	}
	svc, err := New(db, port, nil, nil, Config{})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}

	// Commit ont-ops with autoAnchor so ontology_id binds the NektarAnchor txid!
	man := reasoning.DefaultOntologyManifest("ops")
	loaded, err := svc.CommitOntology(ctx, man, true)
	if err != nil {
		t.Fatalf("CommitOntology: %v", err)
	}
	if loaded.Ref.Repo != "ont-ops" {
		t.Fatalf("expected repo ont-ops, got %q", loaded.Ref.Repo)
	}
	if loaded.Ref.AnchorTxID == "" {
		t.Fatalf("expected anchored ont-ops txid")
	}
	if !strings.HasPrefix(loaded.Ref.OntologyID, "ont:"+loaded.Ref.AnchorTxID+":") {
		t.Fatalf("expected ontology_id bound to anchor txid, got %q", loaded.Ref.OntologyID)
	}

	// Resolve by domain, repo name, and canonical ontology_id.
	for _, key := range []string{"ops", "ont-ops", loaded.Ref.OntologyID} {
		got, err := svc.ResolveOntology(ctx, key)
		if err != nil {
			t.Fatalf("ResolveOntology(%q): %v", key, err)
		}
		if got.Ref.OntologyID != loaded.Ref.OntologyID || got.Manifest.Hash != loaded.Manifest.Hash {
			t.Fatalf("ResolveOntology(%q) mismatch: got %+v want %+v", key, got.Ref, loaded.Ref)
		}
	}

	// Verify order-independent manifest hash & triple-set hash determinism across nodes.
	manReversed := reasoning.DefaultOntologyManifest("ops")
	for i, j := 0, len(manReversed.Entities)-1; i < j; i, j = i+1, j-1 {
		manReversed.Entities[i], manReversed.Entities[j] = manReversed.Entities[j], manReversed.Entities[i]
	}
	h1, err := reasoning.ComputeOntologyManifestHash(man)
	if err != nil {
		t.Fatalf("ComputeOntologyManifestHash: %v", err)
	}
	h2, err := reasoning.ComputeOntologyManifestHash(manReversed)
	if err != nil {
		t.Fatalf("ComputeOntologyManifestHash(reversed): %v", err)
	}
	if h1 != h2 {
		t.Fatalf("manifest hash not order-independent: %s != %s", h1, h2)
	}

	t1 := reasoning.Triple{Subject: "ops:go", Predicate: "requires", Object: "ops:sqlite"}
	t2 := reasoning.Triple{Subject: "ops:tls", Predicate: "mitigates", Object: "ops:cve"}
	thA, err := reasoning.ComputeTripleSetHash([]reasoning.Triple{t1, t2})
	if err != nil {
		t.Fatalf("ComputeTripleSetHash A: %v", err)
	}
	thB, err := reasoning.ComputeTripleSetHash([]reasoning.Triple{t2, t1, t1})
	if err != nil {
		t.Fatalf("ComputeTripleSetHash B: %v", err)
	}
	if thA != thB {
		t.Fatalf("triple set hash not order/dedup-independent: %s != %s", thA, thB)
	}
}
