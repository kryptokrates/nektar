package memrepo

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/kryptokrates/nektar/internal/bstack"
)

// TagRef represents an immutable signed Git/UTXO checkpoint tag pinned to a commit.
type TagRef struct {
	Repo       string `json:"repo"`
	Tag        string `json:"tag"`
	CommitHash string `json:"commitHash"`
	AnchorTxID string `json:"anchorTxid,omitempty"`
	AnchorN    int64  `json:"anchorN"`
	Message    string `json:"message,omitempty"`
	AuthorPub  string `json:"authorPub,omitempty"`
	Signature  string `json:"signature,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
}

// NormalizeTagName validates a Git-compatible immutable checkpoint tag name (e.g. "v1.0-audit").
func NormalizeTagName(tag string) (string, error) {
	t := strings.TrimSpace(tag)
	if t == "" || len(t) > 96 || strings.Contains(t, "..") || strings.HasPrefix(t, "/") || strings.HasSuffix(t, "/") || strings.HasPrefix(t, "-") {
		return "", fmt.Errorf("memrepo: invalid tag name %q", tag)
	}
	for _, c := range t {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '/' || c == '.') {
			return "", fmt.Errorf("memrepo: invalid character in tag %q", tag)
		}
	}
	return t, nil
}

// GoGitProjection holds an in-memory pure-Go go-git/v5 Repository projected from Nektar's
// content-addressed Commit/Tree/Blob DAG along with bidirectional commit hash maps.
type GoGitProjection struct {
	Repo        *git.Repository
	Storer      *memory.Storage
	GitByNektar map[string]plumbing.Hash
	NektarByGit map[plumbing.Hash]CommitObject
	HeadGitHash plumbing.Hash
}

// ResolveBlobBytes returns the canonical plaintext (or encrypted metadata placeholder) bytes for b.
func ResolveBlobBytes(b BlobObject, objects bstack.ObjectStore) []byte {
	if b.Forgotten {
		return nil
	}
	if b.Encrypted {
		meta, _ := json.MarshalIndent(map[string]any{
			"encrypted":     true,
			"borgGroup":     b.BorgGroup,
			"borgEpoch":     b.BorgEpoch,
			"contentSha256": b.ContentSHA256,
			"chirpUrl":      b.ChirpURL,
		}, "", "  ")
		return meta
	}
	if len(b.Inline) > 0 {
		return b.Inline
	}
	if b.ChirpURL != "" && objects != nil {
		if raw, err := bstack.ResolveChirp(objects, b.ChirpURL); err == nil {
			return raw
		}
	}
	if b.ChirpURL != "" {
		return []byte(fmt.Sprintf("nektar-volumetric-ref: %s\nsha256: %s\nsize: %d\n", b.ChirpURL, b.ContentSHA256, b.Size))
	}
	return nil
}

// BuildGoGitRepo projects Nektar's CommitObject DAG, TreeObjects, BlobObjects, branches, and tags
// into a pure-Go in-memory go-git/v5 Repository (zero disk I/O, zero CGO).
// Commits may be passed in any order; they are topologically ordered parent-first automatically.
// Git commit OIDs are strictly immutable across anchoring: anchor UTXO metadata is attached as
// standard Git notes under refs/notes/nektar instead of mutating commit messages.
func BuildGoGitRepo(
	commits []CommitObject,
	trees map[string]TreeObject,
	blobs map[string]BlobObject,
	branches []BranchRef,
	tags []TagRef,
	objects bstack.ObjectStore,
) (*GoGitProjection, error) {
	st := memory.NewStorage()
	repo, err := git.Init(st, nil)
	if err != nil {
		return nil, fmt.Errorf("memrepo: init go-git memory repo: %w", err)
	}

	proj := &GoGitProjection{
		Repo:        repo,
		Storer:      st,
		GitByNektar: make(map[string]plumbing.Hash, len(commits)),
		NektarByGit: make(map[plumbing.Hash]CommitObject, len(commits)),
	}

	// Topologically sort commits parent-first so every parent Git OID is encoded before its children.
	ordered := topoSortCommits(commits)
	anchorNotes := make(map[string][]byte)
	blobOIDCache := make(map[string]plumbing.Hash, len(blobs))
	treeOIDCache := make(map[string]plumbing.Hash, len(trees))

	for _, c := range ordered {
		rootTreeOID, cachedTree := treeOIDCache[c.TreeHash]
		if !cachedTree {
			entries, _ := FlattenTreeFromMap(c.TreeHash, trees)
			fileOIDs := make(map[string]plumbing.Hash, len(entries))
			for _, e := range entries {
				b, ok := blobs[e.BlobHash]
				if ok && b.Forgotten {
					continue
				}
				cacheKey := e.BlobHash
				if !ok {
					cacheKey = "__missing__"
				}
				oid, cachedBlob := blobOIDCache[cacheKey]
				if !cachedBlob {
					var data []byte
					if ok {
						data = ResolveBlobBytes(b, objects)
					}
					var bErr error
					oid, bErr = storeGoGitBlob(st, data)
					if bErr != nil {
						return nil, fmt.Errorf("memrepo: store go-git blob for %s: %w", c.CommitHash, bErr)
					}
					blobOIDCache[cacheKey] = oid
				}
				fileOIDs[e.Path] = oid
			}
			var tErr error
			rootTreeOID, tErr = storeGoGitTreeOIDs(st, fileOIDs)
			if tErr != nil {
				return nil, fmt.Errorf("memrepo: store go-git tree for %s: %w", c.CommitHash, tErr)
			}
			if c.TreeHash != "" {
				treeOIDCache[c.TreeHash] = rootTreeOID
			}
		}

		var parentHashes []plumbing.Hash
		for _, p := range c.Parents {
			if pOID, ok := proj.GitByNektar[p.CommitHash]; ok {
				parentHashes = append(parentHashes, pOID)
			}
		}

		var authorSig, committerSig object.Signature
		var commitMsg string
		if c.GitMeta != nil && c.GitMeta.AuthorName != "" {
			authorSig = object.Signature{
				Name:  c.GitMeta.AuthorName,
				Email: c.GitMeta.AuthorEmail,
				When:  time.Unix(c.GitMeta.AuthorWhen, 0).In(time.FixedZone("", c.GitMeta.AuthorOffsetSec)),
			}
			cName := c.GitMeta.CommitterName
			cEmail := c.GitMeta.CommitterEmail
			cWhen := c.GitMeta.CommitterWhen
			cOff := c.GitMeta.CommitterOffsetSec
			if cName == "" {
				cName = c.GitMeta.AuthorName
				cEmail = c.GitMeta.AuthorEmail
				cWhen = c.GitMeta.AuthorWhen
				cOff = c.GitMeta.AuthorOffsetSec
			}
			committerSig = object.Signature{
				Name:  cName,
				Email: cEmail,
				When:  time.Unix(cWhen, 0).In(time.FixedZone("", cOff)),
			}
			commitMsg = c.GitMeta.RawMessage
		} else {
			authorName := "nektar-agent"
			if len(c.AuthorPub) >= 12 {
				authorName = "nektar-" + c.AuthorPub[:12]
			}
			when := time.Unix(c.Timestamp, 0).UTC()
			if c.Timestamp <= 0 {
				when = time.Unix(1, 0).UTC()
			}
			authorSig = object.Signature{
				Name:  authorName,
				Email: authorName + "@nektar.local",
				When:  when,
			}
			committerSig = authorSig
			msg := strings.TrimSpace(c.Message)
			if msg == "" {
				msg = fmt.Sprintf("Commit %s", c.CommitHash[:min(12, len(c.CommitHash))])
			}
			var sb strings.Builder
			sb.WriteString(msg)
			sb.WriteString("\n\n")
			fmt.Fprintf(&sb, "Nektar-Commit: %s\n", c.CommitHash)
			if c.MPERoot != "" {
				fmt.Fprintf(&sb, "Nektar-MPE-Root: %s\n", c.MPERoot)
			}
			if c.ModelID != "" {
				fmt.Fprintf(&sb, "Model-Identity: %s\n", c.ModelID)
			}
			commitMsg = sb.String()
		}

		gc := &object.Commit{
			Author:       authorSig,
			Committer:    committerSig,
			Message:      commitMsg,
			TreeHash:     rootTreeOID,
			ParentHashes: parentHashes,
		}
		encObj := st.NewEncodedObject()
		if err := gc.Encode(encObj); err != nil {
			return nil, fmt.Errorf("memrepo: encode go-git commit: %w", err)
		}
		gitOID, err := st.SetEncodedObject(encObj)
		if err != nil {
			return nil, fmt.Errorf("memrepo: store go-git commit: %w", err)
		}
		proj.GitByNektar[c.CommitHash] = gitOID
		proj.NektarByGit[gitOID] = c
		proj.HeadGitHash = gitOID

		if c.AnchorTxID != "" {
			var nb strings.Builder
			fmt.Fprintf(&nb, "BSV-Anchor-UTXO: %s:0\n", c.AnchorTxID)
			if c.AnchorN >= 0 {
				fmt.Fprintf(&nb, "Anchor-Batch: %d\n", c.AnchorN)
			}
			fmt.Fprintf(&nb, "Nektar-Commit: %s\n", c.CommitHash)
			anchorNotes[gitOID.String()] = []byte(nb.String())
		}
	}

	// Emit BSV anchor metadata as standard Git notes under refs/notes/nektar so anchoring
	// never mutates already-cloned Git commit SHA-1s.
	if len(anchorNotes) > 0 {
		if notesTreeOID, nErr := storeGoGitTree(st, anchorNotes); nErr == nil {
			notesSig := object.Signature{
				Name:  "nektar-anchor",
				Email: "nektar-anchor@nektar.local",
				When:  time.Unix(1, 0).UTC(),
			}
			notesCommit := &object.Commit{
				Author:    notesSig,
				Committer: notesSig,
				Message:   "Nektar BSV anchor notes\n",
				TreeHash:  notesTreeOID,
			}
			encNotes := st.NewEncodedObject()
			if err := notesCommit.Encode(encNotes); err == nil {
				if notesOID, err := st.SetEncodedObject(encNotes); err == nil {
					_ = st.SetReference(plumbing.NewHashReference(plumbing.ReferenceName("refs/notes/nektar"), notesOID))
				}
			}
		}
	}

	// Populate branch refs (refs/heads/<branch>) and symbolic HEAD -> refs/heads/main.
	defaultHeadSet := false
	for _, br := range branches {
		if br.Branch == "" || br.HeadCommit == "" {
			continue
		}
		if oid, ok := proj.GitByNektar[br.HeadCommit]; ok {
			ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(br.Branch), oid)
			_ = st.SetReference(ref)
			if br.Branch == DefaultBranch {
				proj.HeadGitHash = oid
				_ = st.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(DefaultBranch)))
				defaultHeadSet = true
			}
		}
	}
	if !defaultHeadSet && !proj.HeadGitHash.IsZero() {
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(DefaultBranch), proj.HeadGitHash)
		_ = st.SetReference(ref)
		_ = st.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(DefaultBranch)))
	}

	// Populate immutable tag refs (refs/tags/<tag>).
	for _, tg := range tags {
		if tg.Tag == "" || tg.CommitHash == "" {
			continue
		}
		if oid, ok := proj.GitByNektar[tg.CommitHash]; ok {
			_ = st.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName(tg.Tag), oid))
		}
	}

	return proj, nil
}

func topoSortCommits(commits []CommitObject) []CommitObject {
	byHash := make(map[string]CommitObject, len(commits))
	for _, c := range commits {
		byHash[c.CommitHash] = c
	}
	// Deterministic base order by timestamp ASC, CommitHash ASC.
	keys := make([]string, 0, len(byHash))
	for h := range byHash {
		keys = append(keys, h)
	}
	sort.Slice(keys, func(i, j int) bool {
		ci, cj := byHash[keys[i]], byHash[keys[j]]
		if ci.Timestamp != cj.Timestamp {
			return ci.Timestamp < cj.Timestamp
		}
		return ci.CommitHash < cj.CommitHash
	})

	visited := make(map[string]bool, len(byHash))
	inStack := make(map[string]bool, len(byHash))
	out := make([]CommitObject, 0, len(byHash))
	var visit func(h string)
	visit = func(h string) {
		if visited[h] || inStack[h] {
			return
		}
		c, ok := byHash[h]
		if !ok {
			return
		}
		inStack[h] = true
		for _, p := range c.Parents {
			if _, exists := byHash[p.CommitHash]; exists {
				visit(p.CommitHash)
			}
		}
		inStack[h] = false
		visited[h] = true
		out = append(out, c)
	}
	for _, h := range keys {
		visit(h)
	}
	return out
}

func storeGoGitBlob(st *memory.Storage, data []byte) (plumbing.Hash, error) {
	obj := st.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(data)))
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return st.SetEncodedObject(obj)
}

type gitTreeOIDNode struct {
	files map[string]plumbing.Hash
	dirs  map[string]*gitTreeOIDNode
}

func newGitTreeOIDNode() *gitTreeOIDNode {
	return &gitTreeOIDNode{
		files: make(map[string]plumbing.Hash),
		dirs:  make(map[string]*gitTreeOIDNode),
	}
}

func storeGoGitTreeOIDs(st *memory.Storage, flat map[string]plumbing.Hash) (plumbing.Hash, error) {
	root := newGitTreeOIDNode()
	for p, oid := range flat {
		parts := strings.Split(p, "/")
		cur := root
		for i := 0; i < len(parts)-1; i++ {
			seg := parts[i]
			if cur.dirs[seg] == nil {
				cur.dirs[seg] = newGitTreeOIDNode()
			}
			cur = cur.dirs[seg]
		}
		cur.files[parts[len(parts)-1]] = oid
	}
	return encodeGoGitTreeOIDNode(st, root)
}

func encodeGoGitTreeOIDNode(st *memory.Storage, node *gitTreeOIDNode) (plumbing.Hash, error) {
	type sortEntry struct {
		sortKey string
		entry   object.TreeEntry
	}
	items := make([]sortEntry, 0, len(node.files)+len(node.dirs))
	for name, h := range node.files {
		items = append(items, sortEntry{
			sortKey: name,
			entry: object.TreeEntry{
				Name: name,
				Mode: filemode.Regular,
				Hash: h,
			},
		})
	}
	for name, sub := range node.dirs {
		h, err := encodeGoGitTreeOIDNode(st, sub)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		items = append(items, sortEntry{
			sortKey: name + "/",
			entry: object.TreeEntry{
				Name: name,
				Mode: filemode.Dir,
				Hash: h,
			},
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].sortKey < items[j].sortKey })
	tr := &object.Tree{Entries: make([]object.TreeEntry, len(items))}
	for i, it := range items {
		tr.Entries[i] = it.entry
	}
	obj := st.NewEncodedObject()
	if err := tr.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return st.SetEncodedObject(obj)
}

func storeGoGitTree(st *memory.Storage, flat map[string][]byte) (plumbing.Hash, error) {
	root := newGitTreeNode()
	for p, data := range flat {
		parts := strings.Split(p, "/")
		cur := root
		for i := 0; i < len(parts)-1; i++ {
			seg := parts[i]
			if cur.dirs[seg] == nil {
				cur.dirs[seg] = newGitTreeNode()
			}
			cur = cur.dirs[seg]
		}
		cur.files[parts[len(parts)-1]] = data
	}
	return encodeGoGitTreeNode(st, root)
}

func encodeGoGitTreeNode(st *memory.Storage, node *gitTreeNode) (plumbing.Hash, error) {
	type sortEntry struct {
		sortKey string
		entry   object.TreeEntry
	}
	items := make([]sortEntry, 0, len(node.files)+len(node.dirs))
	for name, data := range node.files {
		h, err := storeGoGitBlob(st, data)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		items = append(items, sortEntry{
			sortKey: name,
			entry: object.TreeEntry{
				Name: name,
				Mode: filemode.Regular,
				Hash: h,
			},
		})
	}
	for name, sub := range node.dirs {
		h, err := encodeGoGitTreeNode(st, sub)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		items = append(items, sortEntry{
			sortKey: name + "/",
			entry: object.TreeEntry{
				Name: name,
				Mode: filemode.Dir,
				Hash: h,
			},
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].sortKey < items[j].sortKey })
	tr := &object.Tree{Entries: make([]object.TreeEntry, len(items))}
	for i, it := range items {
		tr.Entries[i] = it.entry
	}
	obj := st.NewEncodedObject()
	if err := tr.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return st.SetEncodedObject(obj)
}

const (
	// MaxGitTreeFileBytes caps any single file read out of a go-git tree (32 MiB).
	MaxGitTreeFileBytes = 32 << 20
	// MaxGitTreeTotalBytes caps total memory consumed across all files in a go-git tree (64 MiB).
	MaxGitTreeTotalBytes = 64 << 20
	// MaxGitTreeFiles caps total file count in a go-git tree (4096 files).
	MaxGitTreeFiles = 4096
)

// ReadGoGitTreeFiles extracts all files from a go-git Tree object into a flat map[relPath][]byte.
// Fails closed on any unsafe/reserved path or total-tree memory/file-count DoS cap.
func ReadGoGitTreeFiles(tr *object.Tree) (map[string][]byte, error) {
	if tr == nil {
		return nil, errors.New("memrepo: nil go-git tree")
	}
	out := make(map[string][]byte)
	var totalBytes int64
	err := tr.Files().ForEach(func(f *object.File) error {
		if len(out) >= MaxGitTreeFiles {
			return fmt.Errorf("memrepo: tree exceeds max %d files", MaxGitTreeFiles)
		}
		norm, nErr := NormalizePath(f.Name)
		if nErr != nil {
			return fmt.Errorf("memrepo: invalid tree path %q: %w", f.Name, nErr)
		}
		r, err := f.Reader()
		if err != nil {
			return err
		}
		defer r.Close()
		remain := int64(MaxGitTreeTotalBytes) - totalBytes
		if remain < 0 {
			remain = 0
		}
		fileCap := int64(MaxGitTreeFileBytes)
		if remain < fileCap {
			fileCap = remain
		}
		raw, err := io.ReadAll(io.LimitReader(r, fileCap+1))
		if err != nil {
			return err
		}
		if int64(len(raw)) > fileCap {
			return fmt.Errorf("memrepo: tree file %q exceeds size/total-tree memory cap (%d bytes)", norm, MaxGitTreeTotalBytes)
		}
		totalBytes += int64(len(raw))
		out[norm] = raw
		return nil
	})
	return out, err
}
