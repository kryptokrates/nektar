package memrepo

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/store"
)

// TreeDiffEntry represents a file/fact change between two Memory Repo commits.
type TreeDiffEntry struct {
	Status       string `json:"status"` // added | modified | deleted
	Path         string `json:"path"`
	OldBlobHash  string `json:"oldBlobHash,omitempty"`
	NewBlobHash  string `json:"newBlobHash,omitempty"`
	OldSize      int64  `json:"oldSize,omitempty"`
	NewSize      int64  `json:"newSize,omitempty"`
	OldMediaType string `json:"oldMediaType,omitempty"`
	NewMediaType string `json:"newMediaType,omitempty"`
}

// DiffTrees computes a deterministic Git-style diff between two TreeObjects.
func DiffTrees(oldTree, newTree *TreeObject) []TreeDiffEntry {
	oldMap := make(map[string]TreeEntry)
	if oldTree != nil {
		for _, e := range oldTree.Entries {
			oldMap[e.Path] = e
		}
	}
	newMap := make(map[string]TreeEntry)
	if newTree != nil {
		for _, e := range newTree.Entries {
			newMap[e.Path] = e
		}
	}
	paths := make(map[string]bool, len(oldMap)+len(newMap))
	for p := range oldMap {
		paths[p] = true
	}
	for p := range newMap {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	out := make([]TreeDiffEntry, 0)
	for _, p := range sorted {
		o, hasO := oldMap[p]
		n, hasN := newMap[p]
		switch {
		case !hasO && hasN:
			out = append(out, TreeDiffEntry{
				Status:       "added",
				Path:         p,
				NewBlobHash:  n.BlobHash,
				NewSize:      n.Size,
				NewMediaType: n.MediaType,
			})
		case hasO && !hasN:
			out = append(out, TreeDiffEntry{
				Status:       "deleted",
				Path:         p,
				OldBlobHash:  o.BlobHash,
				OldSize:      o.Size,
				OldMediaType: o.MediaType,
			})
		case o.BlobHash != n.BlobHash:
			out = append(out, TreeDiffEntry{
				Status:       "modified",
				Path:         p,
				OldBlobHash:  o.BlobHash,
				NewBlobHash:  n.BlobHash,
				OldSize:      o.Size,
				NewSize:      n.Size,
				OldMediaType: o.MediaType,
				NewMediaType: n.MediaType,
			})
		}
	}
	return out
}

// GitExportResult describes a checked-out real Git working directory (Letta MemFS projection).
type GitExportResult struct {
	Dir         string `json:"dir"`
	Repo        string `json:"repo"`
	Branch      string `json:"branch"`
	GitHeadOID  string `json:"gitHeadOid"`
	CommitCount int    `json:"commitCount"`
	FileCount   int    `json:"fileCount"`
}

// ExportGitWorkspace materializes a MemoryCapsule into a real, standard `.git` repository +
// working tree on disk (pure Go, zero external `git` CLI required!).
// Humans and AI coding agents (Claude Code, Codex, Cursor) can run `git log`, `git show`,
// `git diff`, `grep`, or open the directory in VS Code immediately.
func ExportGitWorkspace(dir string, cap *MemoryCapsule, objects bstack.ObjectStore) (*GitExportResult, error) {
	if cap == nil {
		return nil, errors.New("memrepo: nil capsule for git export")
	}
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("memrepo: target directory is required")
	}
	branch, err := NormalizeBranch(cap.Branch)
	if err != nil {
		return nil, fmt.Errorf("memrepo: invalid capsule branch: %w", err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("memrepo: resolve export dir: %w", err)
	}
	gitDir := filepath.Join(absDir, ".git")
	refsHeadsDir := filepath.Join(gitDir, "refs", "heads")
	for _, sub := range []string{
		filepath.Join(gitDir, "objects"),
		refsHeadsDir,
		filepath.Join(gitDir, "refs", "tags"),
	} {
		if err := os.MkdirAll(sub, 0o750); err != nil {
			return nil, fmt.Errorf("memrepo: mkdir git: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/"+branch+"\n"), 0o640); err != nil {
		return nil, err
	}
	const gitConfig = "[core]\n\trepositoryformatversion = 0\n\tfilemode = false\n\tbare = false\n\tlogallrefupdates = true\n"
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(gitConfig), 0o640); err != nil {
		return nil, err
	}

	resolveBlobBytes := func(b BlobObject) []byte {
		return ResolveBlobBytes(b, objects)
	}

	// Build standard Git blob/tree/commit objects in chronological order so `git log` mirrors the DAG.
	gitCommitByNektar := make(map[string]string, len(cap.Commits))
	var headGitOID string
	var headFiles map[string][]byte

	for _, c := range cap.Commits {
		entries, _ := FlattenTreeFromMap(c.TreeHash, cap.Trees)
		files := make(map[string][]byte, len(entries)+2)
		var indexLines []string
		indexLines = append(indexLines, fmt.Sprintf("# %s (%s)\n", cap.Repo, branch))
		indexLines = append(indexLines, fmt.Sprintf("- **Nektar Commit:** `%s`\n", c.CommitHash))
		if cap.GenesisShardID != "" {
			indexLines = append(indexLines, fmt.Sprintf("- **On-Chain Repo Genesis ID:** `%s`\n", cap.GenesisShardID))
		}
		if c.AnchorTxID != "" {
			indexLines = append(indexLines, fmt.Sprintf("- **BSV NektarAnchor UTXO:** `%s:0` (batch `n=%d`)\n", c.AnchorTxID, c.AnchorN))
		}
		indexLines = append(indexLines, "\n## Files & Memory Facts\n")

		for _, e := range entries {
			b, ok := cap.Blobs[e.BlobHash]
			if ok && b.Forgotten {
				continue
			}
			var data []byte
			if ok {
				data = resolveBlobBytes(b)
			}
			files[e.Path] = data
			indexLines = append(indexLines, fmt.Sprintf("- `%s` (%s, %d bytes)\n", e.Path, e.MediaType, e.Size))
		}
		// Auto-generate Letta-style progressive-disclosure MEMORY.md index unless user committed one.
		if _, hasMem := files["MEMORY.md"]; !hasMem {
			files["MEMORY.md"] = []byte(strings.Join(indexLines, ""))
		}
		// Write cryptographic BSV lineage metadata inside .nektar/lineage.json.
		lineageRaw, _ := json.MarshalIndent(buildLineageSummary(cap, c), "", "  ")
		files[".nektar/lineage.json"] = lineageRaw

		rootTreeOID, err := writeGitTreeRecursive(gitDir, files)
		if err != nil {
			return nil, err
		}
		var commitBody bytes.Buffer
		fmt.Fprintf(&commitBody, "tree %s\n", rootTreeOID)
		for _, p := range c.Parents {
			if pOID, ok := gitCommitByNektar[p.CommitHash]; ok {
				fmt.Fprintf(&commitBody, "parent %s\n", pOID)
			}
		}
		if c.GitMeta != nil && (c.GitMeta.AuthorName != "" || c.GitMeta.RawMessage != "") {
			fmt.Fprintf(&commitBody, "author %s <%s> %d %s\n", c.GitMeta.AuthorName, c.GitMeta.AuthorEmail, c.GitMeta.AuthorWhen, formatGitTZ(c.GitMeta.AuthorOffsetSec))
			fmt.Fprintf(&commitBody, "committer %s <%s> %d %s\n\n", c.GitMeta.CommitterName, c.GitMeta.CommitterEmail, c.GitMeta.CommitterWhen, formatGitTZ(c.GitMeta.CommitterOffsetSec))
			commitBody.WriteString(c.GitMeta.RawMessage)
		} else {
			authorName := "nektar-agent"
			if len(c.AuthorPub) >= 12 {
				authorName = "nektar-" + c.AuthorPub[:12]
			}
			ts := c.Timestamp
			if ts <= 0 {
				ts = 1
			}
			fmt.Fprintf(&commitBody, "author %s <%s@nektar.local> %d +0000\n", authorName, authorName, ts)
			fmt.Fprintf(&commitBody, "committer %s <%s@nektar.local> %d +0000\n\n", authorName, authorName, ts)
			msg := strings.TrimSpace(c.Message)
			if msg == "" {
				msg = fmt.Sprintf("Commit %s", c.CommitHash[:min(12, len(c.CommitHash))])
			}
			commitBody.WriteString(msg)
			commitBody.WriteString("\n\n")
			fmt.Fprintf(&commitBody, "Nektar-Commit: %s\n", c.CommitHash)
			fmt.Fprintf(&commitBody, "Nektar-MPE-Root: %s\n", c.MPERoot)
			if c.ModelID != "" {
				fmt.Fprintf(&commitBody, "Model-Identity: %s\n", c.ModelID)
			}
		}

		gitOID, err := writeGitLooseObject(gitDir, "commit", commitBody.Bytes())
		if err != nil {
			return nil, err
		}
		gitCommitByNektar[c.CommitHash] = gitOID
		headGitOID = gitOID
		headFiles = files
	}

	if headGitOID != "" {
		refPath := filepath.Join(refsHeadsDir, filepath.FromSlash(branch))
		if err := ensurePathContained(refsHeadsDir, refPath); err != nil {
			return nil, fmt.Errorf("memrepo: unsafe ref path for branch %q: %w", branch, err)
		}
		if err := os.MkdirAll(filepath.Dir(refPath), 0o750); err != nil {
			return nil, err
		}
		if err := os.WriteFile(refPath, []byte(headGitOID+"\n"), 0o640); err != nil {
			return nil, err
		}
	}

	// Materialize working tree files AND write a real Git v2 .git/index so `git status` is clean!
	written := 0
	type indexEntry struct {
		path    string
		oid     [20]byte
		size    uint32
		mtimeS  uint32
		mtimeNs uint32
	}
	indexEntries := make([]indexEntry, 0, len(headFiles))
	for rel, content := range headFiles {
		clean, err := NormalizePath(rel)
		if err != nil {
			return nil, fmt.Errorf("memrepo: unsafe path in exported workspace %q: %w", rel, err)
		}
		dst := filepath.Join(absDir, filepath.FromSlash(clean))
		if err := ensurePathContained(absDir, dst); err != nil {
			return nil, fmt.Errorf("memrepo: checkout path escape for %q: %w", rel, err)
		}
		if err := rejectSymlinkComponents(absDir, clean); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, content, 0o640); err != nil {
			return nil, err
		}
		hdr := fmt.Sprintf("blob %d\x00", len(content))
		sum := sha1.Sum(append([]byte(hdr), content...))
		var mtS, mtNs uint32
		if fi, sErr := os.Stat(dst); sErr == nil {
			mt := fi.ModTime()
			mtS = uint32(mt.Unix())
			mtNs = uint32(mt.Nanosecond())
		}
		indexEntries = append(indexEntries, indexEntry{
			path:    clean,
			oid:     sum,
			size:    uint32(len(content)),
			mtimeS:  mtS,
			mtimeNs: mtNs,
		})
		written++
	}
	if len(indexEntries) > 0 {
		sort.Slice(indexEntries, func(i, j int) bool { return indexEntries[i].path < indexEntries[j].path })
		var idxBuf bytes.Buffer
		idxBuf.WriteString("DIRC")
		_ = binary.Write(&idxBuf, binary.BigEndian, uint32(2))
		_ = binary.Write(&idxBuf, binary.BigEndian, uint32(len(indexEntries)))
		for _, ent := range indexEntries {
			start := idxBuf.Len()
			_ = binary.Write(&idxBuf, binary.BigEndian, ent.mtimeS)  // ctime s
			_ = binary.Write(&idxBuf, binary.BigEndian, ent.mtimeNs) // ctime ns
			_ = binary.Write(&idxBuf, binary.BigEndian, ent.mtimeS)  // mtime s
			_ = binary.Write(&idxBuf, binary.BigEndian, ent.mtimeNs) // mtime ns
			_ = binary.Write(&idxBuf, binary.BigEndian, uint32(0))   // dev
			_ = binary.Write(&idxBuf, binary.BigEndian, uint32(0))   // ino
			_ = binary.Write(&idxBuf, binary.BigEndian, uint32(0o100644))
			_ = binary.Write(&idxBuf, binary.BigEndian, uint32(0)) // uid
			_ = binary.Write(&idxBuf, binary.BigEndian, uint32(0)) // gid
			_ = binary.Write(&idxBuf, binary.BigEndian, ent.size)
			idxBuf.Write(ent.oid[:])
			nameLen := len(ent.path)
			if nameLen > 0x0fff {
				nameLen = 0x0fff
			}
			_ = binary.Write(&idxBuf, binary.BigEndian, uint16(nameLen))
			idxBuf.WriteString(ent.path)
			idxBuf.WriteByte(0)
			for (idxBuf.Len()-start)%8 != 0 {
				idxBuf.WriteByte(0)
			}
		}
		chk := sha1.Sum(idxBuf.Bytes())
		idxBuf.Write(chk[:])
		if err := os.WriteFile(filepath.Join(gitDir, "index"), idxBuf.Bytes(), 0o640); err != nil {
			return nil, err
		}
	}

	return &GitExportResult{
		Dir:         dir,
		Repo:        cap.Repo,
		Branch:      branch,
		GitHeadOID:  headGitOID,
		CommitCount: len(cap.Commits),
		FileCount:   written,
	}, nil
}

func formatGitTZ(offsetSec int) string {
	sign := "+"
	if offsetSec < 0 {
		sign = "-"
		offsetSec = -offsetSec
	}
	h := offsetSec / 3600
	m := (offsetSec % 3600) / 60
	return fmt.Sprintf("%s%02d%02d", sign, h, m)
}

func ensurePathContained(baseDir, targetPath string) error {
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return err
	}
	absTarget, err := filepath.Abs(targetPath)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absBase, absTarget)
	if err != nil {
		return err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("path %q escapes base %q", targetPath, baseDir)
	}
	return nil
}

func rejectSymlinkComponents(baseDir, cleanPosixPath string) error {
	cur := baseDir
	for _, seg := range strings.Split(cleanPosixPath, "/") {
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("memrepo: refusing to traverse symlink %q", cur)
		}
	}
	return nil
}

type lineageSummary struct {
	Repo           string                `json:"repo"`
	Branch         string                `json:"branch"`
	GenesisPub     string                `json:"genesisPub,omitempty"`
	GenesisShardID string                `json:"genesisShardId,omitempty"`
	CommitHash     string                `json:"commitHash"`
	MPERoot        string                `json:"mpeRoot"`
	Parents        []ParentRef           `json:"parents,omitempty"`
	Anchors        []lineageAnchorRecord `json:"anchors,omitempty"`
}

type lineageAnchorRecord struct {
	N           int64  `json:"n"`
	Size        int    `json:"size"`
	TxID        string `json:"txid"`
	RootHex     string `json:"R"`
	ChainedHex  string `json:"Rp"`
	Settled     bool   `json:"settled"`
	BlockHeight int64  `json:"blockHeight"`
	BEEFHex     string `json:"beefHex,omitempty"`
}

func buildLineageSummary(cap *MemoryCapsule, c CommitObject) lineageSummary {
	anchors := make([]lineageAnchorRecord, 0, len(cap.Anchors))
	for _, a := range cap.Anchors {
		anchors = append(anchors, toLineageAnchor(a))
	}
	return lineageSummary{
		Repo:           cap.Repo,
		Branch:         cap.Branch,
		GenesisPub:     cap.GenesisPub,
		GenesisShardID: cap.GenesisShardID,
		CommitHash:     c.CommitHash,
		MPERoot:        c.MPERoot,
		Parents:        c.Parents,
		Anchors:        anchors,
	}
}

// toLineageAnchor emits lightweight on-chain anchor pointers into .nektar/lineage.json
// without duplicating full AtomicBEEF blobs across every commit tree (avoiding O(N^2) bloat).
func toLineageAnchor(a store.BatchAnchor) lineageAnchorRecord {
	return lineageAnchorRecord{
		N:           a.N,
		Size:        a.Size,
		TxID:        a.TxID,
		RootHex:     a.RootHex,
		ChainedHex:  a.ChainedHex,
		Settled:     a.Settled,
		BlockHeight: a.BlockHeight,
	}
}

type gitTreeNode struct {
	files map[string][]byte
	dirs  map[string]*gitTreeNode
}

func newGitTreeNode() *gitTreeNode {
	return &gitTreeNode{
		files: make(map[string][]byte),
		dirs:  make(map[string]*gitTreeNode),
	}
}

func writeGitTreeRecursive(gitDir string, flat map[string][]byte) (string, error) {
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
	return encodeGitTreeNode(gitDir, root)
}

func encodeGitTreeNode(gitDir string, node *gitTreeNode) (string, error) {
	type entry struct {
		sortName string
		mode     string
		name     string
		oidRaw   []byte
	}
	var entries []entry
	for name, data := range node.files {
		oidHex, err := writeGitLooseObject(gitDir, "blob", data)
		if err != nil {
			return "", err
		}
		oidRaw, _ := hex.DecodeString(oidHex)
		entries = append(entries, entry{sortName: name, mode: "100644", name: name, oidRaw: oidRaw})
	}
	for name, sub := range node.dirs {
		oidHex, err := encodeGitTreeNode(gitDir, sub)
		if err != nil {
			return "", err
		}
		oidRaw, _ := hex.DecodeString(oidHex)
		// Git sorts directory entries as if suffixed with '/'.
		entries = append(entries, entry{sortName: name + "/", mode: "40000", name: name, oidRaw: oidRaw})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].sortName < entries[j].sortName })

	var body bytes.Buffer
	for _, e := range entries {
		body.WriteString(e.mode)
		body.WriteByte(' ')
		body.WriteString(e.name)
		body.WriteByte(0)
		body.Write(e.oidRaw)
	}
	return writeGitLooseObject(gitDir, "tree", body.Bytes())
}

func writeGitLooseObject(gitDir, objType string, body []byte) (string, error) {
	header := fmt.Sprintf("%s %d\x00", objType, len(body))
	full := make([]byte, 0, len(header)+len(body))
	full = append(full, header...)
	full = append(full, body...)
	sum := sha1.Sum(full)
	oid := hex.EncodeToString(sum[:])

	objDir := filepath.Join(gitDir, "objects", oid[:2])
	objPath := filepath.Join(objDir, oid[2:])
	if _, err := os.Stat(objPath); err == nil {
		return oid, nil
	}
	if err := os.MkdirAll(objDir, 0o750); err != nil {
		return "", err
	}
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if _, err := zw.Write(full); err != nil {
		_ = zw.Close()
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	if err := os.WriteFile(objPath, zbuf.Bytes(), 0o640); err != nil {
		return "", err
	}
	return oid, nil
}
