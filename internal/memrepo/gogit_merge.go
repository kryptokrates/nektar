package memrepo

import (
	"bytes"
	"sort"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/kryptokrates/nektar/internal/bstack"
)

// MergeConflict describes an overlapping 3-way text or binary conflict on one file path.
type MergeConflict struct {
	Path       string `json:"path"`
	BaseText   string `json:"baseText,omitempty"`
	OursText   string `json:"oursText,omitempty"`
	TheirsText string `json:"theirsText,omitempty"`
	Binary     bool   `json:"binary,omitempty"`
}

// ThreeWayMergeResult holds either the cleanly merged file contents or the exact conflict set.
type ThreeWayMergeResult struct {
	BaseCommitHash     string               `json:"baseCommitHash,omitempty"`
	MergedFiles        map[string][]byte    `json:"-"`
	PreservedEncrypted map[string]TreeEntry `json:"-"`
	Conflicts          []MergeConflict      `json:"conflicts,omitempty"`
	AutoMerged         []string             `json:"autoMerged,omitempty"`
}

// ThreeWayMergeTrees performs a true LCA 3-way merge using go-git/v5's Commit.MergeBase()
// and line-level 3-way merging (diffmatchpatch). Non-overlapping edits auto-merge cleanly;
// overlapping edits return structured MergeConflicts unless resolved via resolutions or strategy.
func ThreeWayMergeTrees(
	proj *GoGitProjection,
	dstCommitHash, srcCommitHash string,
	trees map[string]TreeObject,
	blobs map[string]BlobObject,
	objects bstack.ObjectStore,
	resolutions map[string]string,
	strategy string,
) (*ThreeWayMergeResult, error) {
	// 1. Find exact Lowest Common Ancestor (MergeBase) via go-git!
	var baseCommitHash string
	if proj != nil && proj.Repo != nil && dstCommitHash != "" && srcCommitHash != "" {
		dstOID, okD := proj.GitByNektar[dstCommitHash]
		srcOID, okS := proj.GitByNektar[srcCommitHash]
		if okD && okS {
			dstGC, errD := proj.Repo.CommitObject(dstOID)
			srcGC, errS := proj.Repo.CommitObject(srcOID)
			if errD == nil && errS == nil {
				if bases, bErr := srcGC.MergeBase(dstGC); bErr == nil && len(bases) > 0 {
					if nc, ok := proj.NektarByGit[bases[0].Hash]; ok {
						baseCommitHash = nc.CommitHash
					}
				}
			}
		}
	}
	return ThreeWayMergeWithBase(proj, baseCommitHash, dstCommitHash, srcCommitHash, trees, blobs, objects, resolutions, strategy)
}

// ThreeWayMergeWithBase performs a line-level 3-way merge given an explicit base commit hash.
// Shared by ThreeWayMergeTrees (LCA merge), RevertCommit (base=target, ours=head, theirs=parent),
// and CherryPickCommit (base=parent, ours=head, theirs=target).
// Encrypted files are preserved by TreeEntry (BlobHash + KeyedMeta) and never replaced with
// ResolveBlobBytes placeholder JSON.
func ThreeWayMergeWithBase(
	proj *GoGitProjection,
	baseCommitHash, oursCommitHash, theirsCommitHash string,
	trees map[string]TreeObject,
	blobs map[string]BlobObject,
	objects bstack.ObjectStore,
	resolutions map[string]string,
	strategy string,
) (*ThreeWayMergeResult, error) {
	res := &ThreeWayMergeResult{
		BaseCommitHash:     baseCommitHash,
		MergedFiles:        make(map[string][]byte),
		PreservedEncrypted: make(map[string]TreeEntry),
	}

	loadTree := func(commitHash string) (map[string][]byte, map[string]TreeEntry) {
		bytesMap := make(map[string][]byte)
		entryMap := make(map[string]TreeEntry)
		if commitHash == "" {
			return bytesMap, entryMap
		}
		var treeHash string
		if proj != nil {
			if oid, ok := proj.GitByNektar[commitHash]; ok {
				if nc, ok2 := proj.NektarByGit[oid]; ok2 {
					treeHash = nc.TreeHash
				}
			}
		}
		entries, err := FlattenTreeFromMap(treeHash, trees)
		if err != nil {
			return bytesMap, entryMap
		}
		for _, e := range entries {
			entryMap[e.Path] = e
			if b, bOK := blobs[e.BlobHash]; bOK {
				if b.Forgotten {
					delete(entryMap, e.Path)
					continue
				}
				if b.Encrypted || e.Encrypted {
					e.Encrypted = true
					entryMap[e.Path] = e
					// Keep encrypted entry metadata only; do NOT synthesize plaintext bytes.
					continue
				}
				bytesMap[e.Path] = ResolveBlobBytes(b, objects)
			}
		}
		return bytesMap, entryMap
	}

	baseFiles, baseEntries := loadTree(baseCommitHash)
	oursFiles, oursEntries := loadTree(oursCommitHash)
	theirsFiles, theirsEntries := loadTree(theirsCommitHash)

	allPaths := make(map[string]bool)
	for p := range baseEntries {
		allPaths[p] = true
	}
	for p := range oursEntries {
		allPaths[p] = true
	}
	for p := range theirsEntries {
		allPaths[p] = true
	}
	sortedPaths := make([]string, 0, len(allPaths))
	for p := range allPaths {
		sortedPaths = append(sortedPaths, p)
	}
	sort.Strings(sortedPaths)

	strat := strings.ToLower(strings.TrimSpace(strategy))
	for _, p := range sortedPaths {
		if resolved, hasRes := resolutions[p]; hasRes {
			res.MergedFiles[p] = []byte(resolved)
			continue
		}
		bEntry, hasB := baseEntries[p]
		oEntry, hasO := oursEntries[p]
		tEntry, hasT := theirsEntries[p]

		// Handle Borg-encrypted paths purely by BlobHash without ever synthesizing placeholder JSON.
		if bEntry.Encrypted || oEntry.Encrypted || tEntry.Encrypted {
			keep := func(has bool, e TreeEntry, raw []byte) {
				if !has {
					return
				}
				if e.Encrypted {
					res.PreservedEncrypted[p] = e
				} else {
					res.MergedFiles[p] = raw
				}
			}
			if hasO == hasT && (!hasO || oEntry.BlobHash == tEntry.BlobHash) {
				keep(hasO, oEntry, oursFiles[p])
				continue
			}
			if hasO == hasB && (!hasO || oEntry.BlobHash == bEntry.BlobHash) {
				keep(hasT, tEntry, theirsFiles[p])
				continue
			}
			if hasT == hasB && (!hasT || tEntry.BlobHash == bEntry.BlobHash) {
				keep(hasO, oEntry, oursFiles[p])
				continue
			}
			if strat == "ours" {
				keep(hasO, oEntry, oursFiles[p])
				continue
			}
			if strat == "theirs" {
				keep(hasT, tEntry, theirsFiles[p])
				continue
			}
			res.Conflicts = append(res.Conflicts, MergeConflict{
				Path:   p,
				Binary: true,
			})
			continue
		}

		bVal := baseFiles[p]
		oVal := oursFiles[p]
		tVal := theirsFiles[p]

		// Identical in ours and theirs (including both deleted).
		if hasO == hasT && bytes.Equal(oVal, tVal) {
			if hasO {
				res.MergedFiles[p] = oVal
			}
			continue
		}
		// Only theirs changed relative to base -> take theirs.
		if hasO == hasB && bytes.Equal(oVal, bVal) {
			if hasT {
				res.MergedFiles[p] = tVal
			}
			continue
		}
		// Only ours changed relative to base -> take ours.
		if hasT == hasB && bytes.Equal(tVal, bVal) {
			if hasO {
				res.MergedFiles[p] = oVal
			}
			continue
		}

		// Both sides diverged relative to base!
		if strat == "ours" {
			if hasO {
				res.MergedFiles[p] = oVal
			}
			continue
		}
		if strat == "theirs" {
			if hasT {
				res.MergedFiles[p] = tVal
			}
			continue
		}

		// Attempt clean non-overlapping line-level 3-way merge for UTF-8 text files.
		isBin := isBinaryBytes(bVal) || isBinaryBytes(oVal) || isBinaryBytes(tVal)
		if hasO && hasT && !isBin {
			if merged, ok := mergeTextThreeWay(string(bVal), string(oVal), string(tVal)); ok {
				res.MergedFiles[p] = []byte(merged)
				res.AutoMerged = append(res.AutoMerged, p)
				continue
			}
		}

		res.Conflicts = append(res.Conflicts, MergeConflict{
			Path:       p,
			BaseText:   string(bVal),
			OursText:   string(oVal),
			TheirsText: string(tVal),
			Binary:     isBin,
		})
	}
	return res, nil
}

// mergeTextThreeWay performs a true LINE-LEVEL 3-way merge (diff3 semantics).
// Same-line concurrent edits (e.g. "price: 1000" -> "price: 2000" vs "price: 1050") ALWAYS
// return ("", false) conflict instead of silently splicing characters into fabricated values!
// Returns (merged, true) ONLY when ours and theirs modify strictly disjoint line ranges.
func mergeTextThreeWay(baseText, oursText, theirsText string) (string, bool) {
	if oursText == theirsText {
		return oursText, true
	}
	if baseText == "" {
		// Both added a brand-new file with different contents -> genuine conflict.
		return "", false
	}
	baseLines := splitLinesPreserve(baseText)
	oursLines := splitLinesPreserve(oursText)
	theirsLines := splitLinesPreserve(theirsText)

	type lineEdit struct {
		start int // inclusive base line index
		end   int // exclusive base line index (start==end => pure insertion before start)
		lines []string
	}
	computeEdits := func(a, b []string) []lineEdit {
		dmp := diffmatchpatch.New()
		aStr := strings.Join(a, "")
		bStr := strings.Join(b, "")
		c1, c2, lineArray := dmp.DiffLinesToChars(aStr, bStr)
		diffs := dmp.DiffMain(c1, c2, false)
		diffs = dmp.DiffCharsToLines(diffs, lineArray)

		var edits []lineEdit
		basePos := 0
		var cur *lineEdit
		flush := func() {
			if cur != nil {
				edits = append(edits, *cur)
				cur = nil
			}
		}
		for _, d := range diffs {
			seg := splitLinesPreserve(d.Text)
			switch d.Type {
			case diffmatchpatch.DiffEqual:
				flush()
				basePos += len(seg)
			case diffmatchpatch.DiffDelete:
				if cur == nil {
					cur = &lineEdit{start: basePos, end: basePos}
				}
				cur.end += len(seg)
				basePos += len(seg)
			case diffmatchpatch.DiffInsert:
				if cur == nil {
					cur = &lineEdit{start: basePos, end: basePos}
				}
				cur.lines = append(cur.lines, seg...)
			}
		}
		flush()
		return edits
	}

	oursEdits := computeEdits(baseLines, oursLines)
	theirsEdits := computeEdits(baseLines, theirsLines)
	if len(theirsEdits) == 0 {
		return oursText, true
	}
	if len(oursEdits) == 0 {
		return theirsText, true
	}

	// Reject any overlapping or co-located line edits (unless both sides made the exact same edit).
	type taggedEdit struct {
		lineEdit
		fromTheirs bool
	}
	var mergedEdits []taggedEdit
	i, j := 0, 0
	for i < len(oursEdits) && j < len(theirsEdits) {
		oe := oursEdits[i]
		te := theirsEdits[j]
		if oe.start == te.start && oe.end == te.end && equalStringSlices(oe.lines, te.lines) {
			mergedEdits = append(mergedEdits, taggedEdit{lineEdit: oe})
			i++
			j++
			continue
		}
		// Strictly non-overlapping: oe ends strictly before te starts (and if both are insertions, distinct positions).
		if oe.end < te.start || (oe.end == te.start && !(oe.start == oe.end && te.start == te.end) && oe.start < oe.end && te.start < te.end) {
			mergedEdits = append(mergedEdits, taggedEdit{lineEdit: oe})
			i++
			continue
		}
		if te.end < oe.start || (te.end == oe.start && !(oe.start == oe.end && te.start == te.end) && te.start < te.end && oe.start < oe.end) {
			mergedEdits = append(mergedEdits, taggedEdit{lineEdit: te, fromTheirs: true})
			j++
			continue
		}
		// Overlapping or adjacent insertion/modification conflict on the same line region.
		return "", false
	}
	for ; i < len(oursEdits); i++ {
		mergedEdits = append(mergedEdits, taggedEdit{lineEdit: oursEdits[i]})
	}
	for ; j < len(theirsEdits); j++ {
		mergedEdits = append(mergedEdits, taggedEdit{lineEdit: theirsEdits[j], fromTheirs: true})
	}

	var out strings.Builder
	pos := 0
	for _, ed := range mergedEdits {
		for pos < ed.start {
			out.WriteString(baseLines[pos])
			pos++
		}
		for _, ln := range ed.lines {
			out.WriteString(ln)
		}
		pos = ed.end
	}
	for pos < len(baseLines) {
		out.WriteString(baseLines[pos])
		pos++
	}
	return out.String(), true
}

func splitLinesPreserve(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func isBinaryBytes(b []byte) bool {
	limit := min(len(b), 4096)
	for i := 0; i < limit; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}
