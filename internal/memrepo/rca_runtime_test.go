package memrepo

import (
	"bytes"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/kryptokrates/nektar/internal/bstack"
)

func TestRCA_GitOIDImmutableAcrossAnchoringAndNotesRef(t *testing.T) {
	objs := bstack.NewMemoryObjectStore()
	b, _, err := BuildBlob(objs, "README.md", "text/markdown", []byte("hello immutable git oid\n"), false)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := BuildTree([]TreeEntry{{
		Mode: "100644", Path: b.Path, BlobHash: b.BlobHash, MediaType: b.MediaType, Size: b.Size, ContentSHA256: b.ContentSHA256,
	}})
	if err != nil {
		t.Fatal(err)
	}
	cUnanchored, err := FinalizeCommit(CommitObject{
		Version: 1, Repo: "r", Branch: "main", TreeHash: tr.TreeHash, AuthorPub: "02abcdef1234567890", ModelID: "nektar/v0.1", Message: "init", Timestamp: 1700000000, AnchorN: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	trees := map[string]TreeObject{tr.TreeHash: *tr}
	blobs := map[string]BlobObject{b.BlobHash: *b}
	branches := []BranchRef{{Repo: "r", Branch: "main", HeadCommit: cUnanchored.CommitHash}}

	projBefore, err := BuildGoGitRepo([]CommitObject{cUnanchored}, trees, blobs, branches, nil, objs)
	if err != nil {
		t.Fatal(err)
	}
	oidBefore := projBefore.GitByNektar[cUnanchored.CommitHash]

	// Anchor the commit (mutating only AnchorTxID & AnchorN) -> Git commit SHA-1 MUST NOT change!
	cAnchored := cUnanchored
	cAnchored.AnchorTxID = "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"
	cAnchored.AnchorN = 0

	projAfter, err := BuildGoGitRepo([]CommitObject{cAnchored}, trees, blobs, branches, nil, objs)
	if err != nil {
		t.Fatal(err)
	}
	oidAfter := projAfter.GitByNektar[cAnchored.CommitHash]
	if oidBefore != oidAfter {
		t.Fatalf("Git commit OID mutated after anchoring! before=%s after=%s", oidBefore, oidAfter)
	}

	// Verify anchor metadata is exposed cleanly via refs/notes/nektar!
	notesRef, err := projAfter.Storer.Reference(plumbing.ReferenceName("refs/notes/nektar"))
	if err != nil || notesRef == nil {
		t.Fatalf("expected refs/notes/nektar ref after anchoring, got err=%v", err)
	}
	nc, err := projAfter.Repo.CommitObject(notesRef.Hash())
	if err != nil {
		t.Fatal(err)
	}
	nt, err := nc.Tree()
	if err != nil {
		t.Fatal(err)
	}
	nf, err := nt.File(oidAfter.String())
	if err != nil {
		t.Fatalf("expected note entry for %s in refs/notes/nektar: %v", oidAfter.String(), err)
	}
	noteContent, err := nf.Contents()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(noteContent, "BSV-Anchor-UTXO: "+cAnchored.AnchorTxID+":0") {
		t.Fatalf("expected BSV-Anchor-UTXO in note, got %q", noteContent)
	}
}

func TestRCA_PushedGitCommitPreservesExactClientOID(t *testing.T) {
	// Construct a real go-git commit with custom author/committer/timezone/message.
	st := memory.NewStorage()
	if _, err := git.Init(st, nil); err != nil {
		t.Fatal(err)
	}
	treeOID, err := storeGoGitTree(st, map[string][]byte{"notes/a.md": []byte("line 1\n")})
	if err != nil {
		t.Fatal(err)
	}
	when := time.Unix(1712345678, 0).In(time.FixedZone("", 7200))
	clientCommit := &object.Commit{
		Author:    object.Signature{Name: "Alice Dev", Email: "alice@example.com", When: when},
		Committer: object.Signature{Name: "Alice Dev", Email: "alice@example.com", When: when},
		Message:   "feat: client pushed commit\n\nDetailed body.\n",
		TreeHash:  treeOID,
	}
	enc := st.NewEncodedObject()
	if err := clientCommit.Encode(enc); err != nil {
		t.Fatal(err)
	}
	wantOID, err := st.SetEncodedObject(enc)
	if err != nil {
		t.Fatal(err)
	}

	pushed, err := ExtractPushedCommits(st, plumbing.ZeroHash, wantOID)
	if err != nil || len(pushed) != 1 {
		t.Fatalf("ExtractPushedCommits: len=%d err=%v", len(pushed), err)
	}
	pc := pushed[0]
	b, _, err := BuildBlob(nil, "notes/a.md", "", pc.Files["notes/a.md"], false)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := BuildTree([]TreeEntry{{
		Mode: "100644", Path: b.Path, BlobHash: b.BlobHash, MediaType: b.MediaType, Size: b.Size, ContentSHA256: b.ContentSHA256,
	}})
	if err != nil {
		t.Fatal(err)
	}
	nc, err := FinalizeCommit(CommitObject{
		Version: 1, Repo: "r", Branch: "main", TreeHash: tr.TreeHash, AuthorPub: "02pub", ModelID: "nektar/v0.1", Message: pc.Message, Timestamp: pc.Timestamp, AnchorN: -1,
		GitMeta: &GitCommitMeta{
			AuthorName:         pc.AuthorName,
			AuthorEmail:        pc.AuthorEmail,
			AuthorWhen:         pc.AuthorWhen,
			AuthorOffsetSec:    pc.AuthorOffsetSec,
			CommitterName:      pc.CommitterName,
			CommitterEmail:     pc.CommitterEmail,
			CommitterWhen:      pc.CommitterWhen,
			CommitterOffsetSec: pc.CommitterOffsetSec,
			RawMessage:         pc.RawMessage,
			GitOID:             pc.GitOID.String(),
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := BuildGoGitRepo(
		[]CommitObject{nc},
		map[string]TreeObject{tr.TreeHash: *tr},
		map[string]BlobObject{b.BlobHash: *b},
		[]BranchRef{{Repo: "r", Branch: "main", HeadCommit: nc.CommitHash}},
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	gotOID := proj.GitByNektar[nc.CommitHash]
	if gotOID != wantOID {
		t.Fatalf("projected Git OID mismatch! got=%s want=%s", gotOID, wantOID)
	}
}

func TestRCA_ReadGoGitTreeFilesFailsClosedOnUnsafePath(t *testing.T) {
	st := memory.NewStorage()
	blobOID, err := storeGoGitBlob(st, []byte("evil"))
	if err != nil {
		t.Fatal(err)
	}
	tr := &object.Tree{
		Entries: []object.TreeEntry{
			{Name: "../escape.txt", Mode: filemode.Regular, Hash: blobOID},
		},
	}
	obj := st.NewEncodedObject()
	if err := tr.Encode(obj); err != nil {
		t.Fatal(err)
	}
	trOID, err := st.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	decodedTree, err := object.GetTree(st, trOID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadGoGitTreeFiles(decodedTree); err == nil {
		t.Fatal("expected ReadGoGitTreeFiles to fail closed on unsafe path ../escape.txt")
	}
}

func TestRCA_BuildEncryptedBlobZeroPlaintext(t *testing.T) {
	b, err := BuildEncryptedBlob("secrets/key.bin", "application/octet-stream", 1024, "deadbeef", "grp1", 1, []byte(`{"ct":"abc"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !b.Encrypted || len(b.Inline) != 0 || b.ChirpURL != "" || b.BorgGroup != "grp1" || !bytes.Equal(b.KeyedMeta, []byte(`{"ct":"abc"}`)) {
		t.Fatalf("unexpected encrypted blob: %+v", b)
	}
}
