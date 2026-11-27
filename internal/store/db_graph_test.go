package store

import (
	"encoding/json"
	"testing"
)

func TestKnowledgeEdgesCRUDAndForgetShredding(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Append fragment + companion graph sidecar blob inside mem_objects/mem_commits.
	frag, _, err := db.AppendFragment(FragmentRecord{
		Shard:         "ops",
		HDP:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Leaf:          "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Data:          []byte("The fee-bump patch resolves the UTXO stall."),
		Provenance:    []byte("prov"),
		Class:         "general",
		Path:          "facts/0001.md",
		ContentSHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		MPERoot:       "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		CreatedAt:     1730000001,
	})
	if err != nil {
		t.Fatalf("AppendFragment: %v", err)
	}
	sidecarBlob, _ := json.Marshal(map[string]any{
		"blobHash":      "sidecar-hash-1",
		"path":          "graph/facts/0001.md.edges.json",
		"inline":        []byte(`{"edges":[{"s":"mit:fee-bump"}]}`),
		"contentSha256": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
	})
	if err := db.PutMemObject("sidecar-hash-1", "blob", sidecarBlob, 1730000001); err != nil {
		t.Fatalf("PutMemObject sidecar: %v", err)
	}
	treeBody, _ := json.Marshal(map[string]any{
		"treeHash": "tree-1",
		"entries": []map[string]any{
			{"path": "facts/0001.md", "blobHash": "fact-hash-1"},
			{"path": "graph/facts/0001.md.edges.json", "blobHash": "sidecar-hash-1"},
		},
	})
	if err := db.PutMemObject("tree-1", "tree", treeBody, 1730000001); err != nil {
		t.Fatalf("PutMemObject tree: %v", err)
	}
	if err := db.SaveMemCommit(MemCommitRow{
		CommitHash: "commit-1",
		Repo:       "ops",
		Branch:     "main",
		Seq:        frag.Seq,
		TreeHash:   "tree-1",
		MPERoot:    frag.MPERoot,
		AuthorPub:  "02" + "1111111111111111111111111111111111111111111111111111111111111111",
		ModelID:    "test",
		Message:    "commit 1",
		Timestamp:  1730000001,
	}); err != nil {
		t.Fatalf("SaveMemCommit: %v", err)
	}
	_ = db.UpdateFragmentCommitHash("ops", frag.Seq, "commit-1")

	ontID := "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	if err := db.SaveKnowledgeEdges([]KnowledgeEdgeRow{
		{
			Shard:         "ops",
			Seq:           frag.Seq,
			TripleHash:    "th-1",
			OntologyID:    ontID,
			Subject:       "mit:fee-bump",
			Predicate:     "mitigates",
			Object:        "inc:utxo-stall",
			SubjectType:   "Mitigation",
			ObjectType:    "Incident",
			Status:        "review",
			EvidenceQuote: "The fee-bump patch resolves the UTXO stall.",
			StartOffset:   0,
			EndOffset:     43,
			ConfidenceBP:  6800,
			MarginBP:      1200,
			SupportBP:     8000,
			ModelID:       "kev-0.8b",
			CreatedAt:     1730000001,
		},
	}); err != nil {
		t.Fatalf("SaveKnowledgeEdges: %v", err)
	}

	if err := db.UpdateKnowledgeEdgeStatus("ops", frag.Seq, "th-1", "accepted", "operator approved"); err != nil {
		t.Fatalf("UpdateKnowledgeEdgeStatus: %v", err)
	}
	gotOnt, err := db.FragmentOntologyID("ops", frag.Seq)
	if err != nil || gotOnt != ontID {
		t.Fatalf("FragmentOntologyID = %q err=%v, want %q", gotOnt, err, ontID)
	}
	edges, err := db.ListKnowledgeEdges(KnowledgeEdgeFilter{Shard: "ops", Status: "accepted"})
	if err != nil || len(edges) != 1 || edges[0].Predicate != "mitigates" {
		t.Fatalf("ListKnowledgeEdges = %+v err=%v", edges, err)
	}

	// ForgetFragment MUST atomically shred knowledge_edges AND companion graph sidecar blob!
	if err := db.ForgetFragment("ops", frag.Seq); err != nil {
		t.Fatalf("ForgetFragment: %v", err)
	}
	after, err := db.ListKnowledgeEdges(KnowledgeEdgeFilter{Shard: "ops", IncludeInactive: true})
	if err != nil || len(after) != 0 {
		t.Fatalf("expected 0 edges after ForgetFragment, got %+v err=%v", after, err)
	}
	_, rawObj, err := db.GetMemObject("sidecar-hash-1")
	if err != nil {
		t.Fatalf("GetMemObject sidecar: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(rawObj, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["forgotten"] != true || obj["inline"] != nil {
		t.Fatalf("expected sidecar blob shredded (forgotten=true, inline=nil), got %+v", obj)
	}
}
