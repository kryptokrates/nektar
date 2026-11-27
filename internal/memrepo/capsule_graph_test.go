package memrepo

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/jevgraph"
	"github.com/kryptokrates/nektar/internal/reasoning"
)

func TestVerifyCapsuleGraphEdgeSidecarAndTamperDetection(t *testing.T) {
	priv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())

	srcText := []byte("The fee-bump patch resolves the UTXO stall cleanly.")
	tr, err := reasoning.FinalizeTriple(reasoning.Triple{
		Subject:   "mit:fee-bump",
		Predicate: "mitigates",
		Object:    "inc:utxo-stall",
	})
	if err != nil {
		t.Fatal(err)
	}
	ontID := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	accepted := jevgraph.ExtractedEdge{
		Triple:     tr,
		OntologyID: ontID,
		Status:     jevgraph.EdgeAccepted,
		Evidence: jevgraph.EvidenceSpan{
			Quote:       string(srcText),
			StartOffset: 0,
			EndOffset:   len(srcText),
		},
		Provenance: jevgraph.ExtractionProvenance{
			Extractor:    jevgraph.ExtractorVersion,
			ConfidenceBP: 9300,
			MarginBP:     8600,
			SupportBP:    9500,
			AdvisoryNote: jevgraph.AdvisoryExtractionNote,
		},
	}
	edgesRoot, err := jevgraph.ComputeEdgesRoot([]jevgraph.ExtractedEdge{accepted})
	if err != nil {
		t.Fatal(err)
	}
	sidecar := jevgraph.BuildSidecar("facts/0001.md", srcText, &jevgraph.ExtractionResult{
		OntologyID:      ontID,
		OntologyName:    "bsv-ops",
		ExtractorStatus: "ok",
		Edges:           []jevgraph.ExtractedEdge{accepted},
		AcceptedCount:   1,
		EdgesRoot:       edgesRoot,
	})
	sidecarBytes, err := json.Marshal(sidecar)
	if err != nil {
		t.Fatal(err)
	}

	buildCap := func(scRaw []byte) *MemoryCapsule {
		b1, _, err := BuildBlob(nil, "facts/0001.md", "text/markdown", srcText, false)
		if err != nil {
			t.Fatal(err)
		}
		b2, _, err := BuildBlob(nil, "graph/facts/0001.md.edges.json", "application/json", scRaw, false)
		if err != nil {
			t.Fatal(err)
		}
		trObj, err := BuildTree([]TreeEntry{
			{Mode: "100644", Path: b1.Path, BlobHash: b1.BlobHash, MediaType: b1.MediaType, Size: b1.Size, ContentSHA256: b1.ContentSHA256},
			{Mode: "100644", Path: b2.Path, BlobHash: b2.BlobHash, MediaType: b2.MediaType, Size: b2.Size, ContentSHA256: b2.ContentSHA256},
		})
		if err != nil {
			t.Fatal(err)
		}
		cObj, err := FinalizeCommit(CommitObject{
			Version:   1,
			Repo:      "ops",
			Branch:    "main",
			TreeHash:  trObj.TreeHash,
			AuthorPub: pubHex,
			ModelID:   "test/v1",
			Message:   "commit with graph sidecar",
			Timestamp: 1730000100,
		}, func(digest []byte) ([]byte, error) {
			sig, sErr := priv.Sign(digest)
			if sErr != nil {
				return nil, sErr
			}
			return sig.Serialize(), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return &MemoryCapsule{
			Format:     CapsuleFormatVersion,
			Repo:       "ops",
			Branch:     "main",
			OwnerPub:   pubHex,
			HeadCommit: cObj.CommitHash,
			ExportedAt: 1730000100,
			Commits:    []CommitObject{cObj},
			Trees:      map[string]TreeObject{trObj.TreeHash: *trObj},
			Blobs:      map[string]BlobObject{b1.BlobHash: *b1, b2.BlobHash: *b2},
		}
	}

	rep, err := VerifyCapsule(context.Background(), buildCap(sidecarBytes), nil, nil, true)
	if err != nil || !rep.OK || rep.EdgesVerified != 1 {
		t.Fatalf("VerifyCapsule valid sidecar failed: rep=%+v err=%v", rep, err)
	}

	// Tampered quote inside sidecar (even with re-signed commit!) MUST fail VerifyCapsule!
	badSidecar := sidecar
	badSidecar.Edges = append([]jevgraph.ExtractedEdge(nil), sidecar.Edges...)
	badSidecar.Edges[0].Evidence.Quote = "fabricated quote not in source"
	badRaw, _ := json.Marshal(badSidecar)
	badRep, err := VerifyCapsule(context.Background(), buildCap(badRaw), nil, nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if badRep.OK {
		t.Fatalf("expected VerifyCapsule to reject tampered graph sidecar, got OK=true: %+v", badRep)
	}
}
