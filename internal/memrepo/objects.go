package memrepo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/lightwebinc/bcommon/cbor"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
)

const (
	// InlineBlobMaxBytes caps inline blob storage inside Commit/Capsule metadata (64 KiB).
	// Larger volumetric blobs (PDFs, datasets, embeddings, images) are stored in BRC-167 CHIRP
	// and referenced by chirp://<sha256> + ContentSHA256.
	InlineBlobMaxBytes = 64 << 10

	// DefaultBranch is the canonical default branch of every Memory Repo.
	DefaultBranch = "main"
)

// ParentRef links a commit to a parent commit and (when anchored) its BSV NektarAnchor UTXO outpoint,
// forming a Metanet/Git Merkle DAG across branches, forks, and purchased market repositories.
type ParentRef struct {
	CommitHash  string      `json:"commitHash"`
	AnchorTxID  string      `json:"anchorTxid,omitempty"`
	AnchorVout  uint32      `json:"anchorVout,omitempty"`
	RepoGenesis string      `json:"repoGenesis,omitempty"` // populated on cross-repo Market forks/merges
	Extra       []cbor.Pair `json:"-"`                     // preserved non-critical odd CBOR extension keys
}

// BlobObject is a content-addressed memory item or volumetric file attachment.
type BlobObject struct {
	BlobHash        string      `json:"blobHash"`
	Path            string      `json:"path"`
	MediaType       string      `json:"mediaType"`
	Size            int64       `json:"size"`
	ContentSHA256   string      `json:"contentSha256"`
	Inline          []byte      `json:"inline,omitempty"`
	ChirpURL        string      `json:"chirpUrl,omitempty"`
	Encrypted       bool        `json:"encrypted,omitempty"`
	BorgGroup       string      `json:"borgGroup,omitempty"`
	BorgEpoch       int64       `json:"borgEpoch,omitempty"`
	LatticeEpoch    uint64      `json:"latticeEpoch,omitempty"`
	KeyedMeta       []byte      `json:"keyedMeta,omitempty"`
	KeyedMetaSHA256 string      `json:"keyedMetaSha256,omitempty"`
	Level           int         `json:"level,omitempty"`
	Class           string      `json:"class,omitempty"`
	Redacted        bool        `json:"redacted,omitempty"`
	Forgotten       bool        `json:"forgotten,omitempty"`
	Extra           []cbor.Pair `json:"-"` // preserved non-critical odd CBOR extension keys
}

// TreeEntry is one deterministically ordered file or memory fact inside a TreeObject.
type TreeEntry struct {
	Mode          string      `json:"mode"` // "100644" regular file/fact
	Path          string      `json:"path"`
	BlobHash      string      `json:"blobHash"`
	MediaType     string      `json:"mediaType"`
	Size          int64       `json:"size"`
	ContentSHA256 string      `json:"contentSha256"`
	ChirpURL      string      `json:"chirpUrl,omitempty"`
	Encrypted     bool        `json:"encrypted,omitempty"`
	Level         int         `json:"level,omitempty"`
	Class         string      `json:"class,omitempty"`
	Extra         []cbor.Pair `json:"-"` // preserved non-critical odd CBOR extension keys
}

// TreeObject is an immutable Merkle directory snapshot of all active paths in a Memory Repo.
// Legacy/bucket trees store sorted Entries directly (TagTreeV1); sharded root trees store up to
// 256 deterministic 2-hex-char bucket hashes in Subtrees (TagTreeV2) for O(1) sub-millisecond writes.
type TreeObject struct {
	TreeHash string            `json:"treeHash"`
	Entries  []TreeEntry       `json:"entries,omitempty"`
	Subtrees map[string]string `json:"subtrees,omitempty"`
	Extra    []cbor.Pair       `json:"-"` // preserved non-critical odd CBOR extension keys
}

// GitCommitMeta preserves the exact Git commit header fields for commits pushed from a real
// Git client so BuildGoGitRepo projects the exact client Git OID on subsequent pulls/pushes.
// Excluded from CommitObject's signed Nektar payload (commitSignable).
type GitCommitMeta struct {
	AuthorName         string `json:"authorName,omitempty"`
	AuthorEmail        string `json:"authorEmail,omitempty"`
	AuthorWhen         int64  `json:"authorWhen,omitempty"`
	AuthorOffsetSec    int    `json:"authorOffsetSec,omitempty"`
	CommitterName      string `json:"committerName,omitempty"`
	CommitterEmail     string `json:"committerEmail,omitempty"`
	CommitterWhen      int64  `json:"committerWhen,omitempty"`
	CommitterOffsetSec int    `json:"committerOffsetSec,omitempty"`
	RawMessage         string `json:"rawMessage,omitempty"`
	GitOID             string `json:"gitOid,omitempty"`
}

// CommitObject is a signed, content-addressed Git/Metanet commit node in a Memory Repo DAG.
type CommitObject struct {
	Version        int            `json:"v"`
	CommitKind     int            `json:"commitKind,omitempty"` // 0 = normal, 1 = tombstone (append-only Forget)
	CommitHash     string         `json:"commitHash"`
	Repo           string         `json:"repo"`
	Branch         string         `json:"branch"`
	Seq            int64          `json:"seq,omitempty"`
	Parents        []ParentRef    `json:"parents,omitempty"`
	TreeHash       string         `json:"treeHash"`
	MPERoot        string         `json:"mpeRoot"`
	AuthorPub      string         `json:"authorPub"`
	ModelID        string         `json:"modelId"`
	PolicyCommit   string         `json:"policyCommit,omitempty"`
	OntologyCommit string         `json:"ontologyCommit,omitempty"`
	Message        string         `json:"message"`
	Timestamp      int64          `json:"timestamp"`
	Signature      string         `json:"signature"`
	AnchorTxID     string         `json:"anchorTxid,omitempty"`
	AnchorN        int64          `json:"anchorN"`
	FragmentLeaf   string         `json:"fragmentLeaf,omitempty"`
	GitMeta        *GitCommitMeta `json:"gitMeta,omitempty"`
	Extra          []cbor.Pair    `json:"-"` // preserved non-critical odd CBOR extension keys
}

// BranchRef tracks the current head commit and on-chain NektarAnchor GenesisOwner of a branch.
type BranchRef struct {
	Repo           string `json:"repo"`
	Branch         string `json:"branch"`
	HeadCommit     string `json:"headCommit"`
	GenesisPub     string `json:"genesisPub,omitempty"`
	GenesisShardID string `json:"genesisShardId,omitempty"`
	UpdatedAt      int64  `json:"updatedAt"`
}

// NormalizePath validates and canonicalizes a relative POSIX repo path (e.g. "facts/0001.md").
// Rejects traversal, control chars, drive letters/NTFS ADS colons, and case-insensitive/NTFS
// .git variants (e.g. ".GIT/config", ".git./hooks", "GIT~1/config") at any directory depth.
func NormalizePath(raw string) (string, error) {
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x20 || raw[i] == 0x7f {
			return "", fmt.Errorf("memrepo: control character in path %q", raw)
		}
		if raw[i] == ':' {
			return "", fmt.Errorf("memrepo: colon/ADS not allowed in path %q", raw)
		}
	}
	p := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", errors.New("memrepo: path is required")
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("memrepo: unsafe path %q", raw)
	}
	if len(clean) > 256 {
		return "", errors.New("memrepo: path exceeds 256 characters")
	}
	for _, seg := range strings.Split(clean, "/") {
		trimmed := strings.TrimRight(seg, ". ")
		low := strings.ToLower(trimmed)
		if low == "" || low == "." || low == ".." {
			return "", fmt.Errorf("memrepo: unsafe path segment %q in %q", seg, raw)
		}
		if isReservedGitSegment(low) {
			return "", fmt.Errorf("memrepo: reserved .git path %q", raw)
		}
	}
	return clean, nil
}

// isReservedGitSegment matches ".git" and Windows 8.3 short-name aliases ("git~1", ".git~1", etc.)
// after case folding and NTFS trailing-dot/space stripping.
func isReservedGitSegment(low string) bool {
	if low == ".git" {
		return true
	}
	rest := ""
	switch {
	case strings.HasPrefix(low, "git~"):
		rest = low[len("git~"):]
	case strings.HasPrefix(low, ".git~"):
		rest = low[len(".git~"):]
	default:
		return false
	}
	if rest == "" {
		return false
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] < '0' || rest[i] > '9' {
			return false
		}
	}
	return true
}

// NormalizeBranch validates a branch name (e.g. "main", "agent/codex", "fork/seller-abc").
func NormalizeBranch(branch string) (string, error) {
	b := strings.TrimSpace(branch)
	if b == "" {
		return DefaultBranch, nil
	}
	if len(b) > 96 || strings.Contains(b, "..") || strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") || strings.HasPrefix(b, "-") {
		return "", fmt.Errorf("memrepo: invalid branch name %q", branch)
	}
	for _, c := range b {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '/' || c == '.') {
			return "", fmt.Errorf("memrepo: invalid character in branch %q", branch)
		}
	}
	return b, nil
}

// BuildBlob constructs a canonical content-addressed BlobObject with default policy metadata.
func BuildBlob(objects bstack.ObjectStore, relPath, mediaType string, raw []byte, forceChirp bool) (*BlobObject, [][32]byte, error) {
	return BuildBlobWithMeta(objects, relPath, mediaType, raw, forceChirp, 0, "")
}

// BuildBlobWithMeta constructs a canonical content-addressed BlobObject with Level and Class committed
// directly into BlobHash (preventing post-hash policy metadata tampering).
func BuildBlobWithMeta(objects bstack.ObjectStore, relPath, mediaType string, raw []byte, forceChirp bool, level int, class string) (*BlobObject, [][32]byte, error) {
	normPath, err := NormalizePath(relPath)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(mediaType) == "" {
		mediaType = guessMediaType(normPath, raw)
	}
	sum := sha256.Sum256(raw)
	b := &BlobObject{
		Path:          normPath,
		MediaType:     mediaType,
		Size:          int64(len(raw)),
		ContentSHA256: hex.EncodeToString(sum[:]),
		Level:         level,
		Class:         class,
	}
	var chirpHashes [][32]byte
	if (len(raw) > InlineBlobMaxBytes || forceChirp) && objects != nil {
		u, _, hashes, err := bstack.StoreChirpWithHashes(objects, raw)
		if err != nil {
			return nil, nil, fmt.Errorf("memrepo: store volumetric CHIRP blob: %w", err)
		}
		b.ChirpURL = u
		chirpHashes = hashes
	} else {
		cp := make([]byte, len(raw))
		copy(cp, raw)
		b.Inline = cp
	}
	h, err := ComputeBlobHash(*b)
	if err != nil {
		return nil, nil, err
	}
	b.BlobHash = h
	return b, chirpHashes, nil
}

// BuildEncryptedBlob constructs a canonical content-addressed BlobObject for a Borg- or CLKA-encrypted
// fragment WITHOUT ever storing plaintext inline or writing plaintext bytes to CHIRP.
func BuildEncryptedBlob(relPath, mediaType string, size int64, contentSHA256Hex, borgGroup string, borgEpoch int64, keyedMeta []byte) (*BlobObject, error) {
	return BuildEncryptedBlobWithMeta(relPath, mediaType, size, contentSHA256Hex, "", borgGroup, borgEpoch, 0, keyedMeta, 0, "")
}

// BuildEncryptedBlobWithLattice constructs a canonical content-addressed BlobObject for an encrypted
// fragment (BorgGroup or CLKA lattice epoch), optionally referencing encrypted volumetric ciphertext via chirpURL.
func BuildEncryptedBlobWithLattice(relPath, mediaType string, size int64, contentSHA256Hex, chirpURL, borgGroup string, borgEpoch int64, latticeEpoch uint64, keyedMeta []byte) (*BlobObject, error) {
	return BuildEncryptedBlobWithMeta(relPath, mediaType, size, contentSHA256Hex, chirpURL, borgGroup, borgEpoch, latticeEpoch, keyedMeta, 0, "")
}

// BuildEncryptedBlobWithMeta constructs an encrypted BlobObject with Level and Class bound into BlobHash.
func BuildEncryptedBlobWithMeta(relPath, mediaType string, size int64, contentSHA256Hex, chirpURL, borgGroup string, borgEpoch int64, latticeEpoch uint64, keyedMeta []byte, level int, class string) (*BlobObject, error) {
	normPath, err := NormalizePath(relPath)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(mediaType) == "" {
		mediaType = guessMediaType(normPath, nil)
	}
	var kmCopy []byte
	var kmSHA string
	if len(keyedMeta) > 0 {
		kmCopy = make([]byte, len(keyedMeta))
		copy(kmCopy, keyedMeta)
		sum := sha256.Sum256(kmCopy)
		kmSHA = hex.EncodeToString(sum[:])
	}
	b := &BlobObject{
		Path:            normPath,
		MediaType:       mediaType,
		Size:            size,
		ContentSHA256:   contentSHA256Hex,
		ChirpURL:        strings.TrimSpace(chirpURL),
		Encrypted:       true,
		BorgGroup:       borgGroup,
		BorgEpoch:       borgEpoch,
		LatticeEpoch:    latticeEpoch,
		KeyedMeta:       kmCopy,
		KeyedMetaSHA256: kmSHA,
		Level:           level,
		Class:           class,
	}
	h, err := ComputeBlobHash(*b)
	if err != nil {
		return nil, err
	}
	b.BlobHash = h
	return b, nil
}

// ComputeBlobHash computes the canonical DSHA-256 content ID of a BlobObject over RFC 8949 CDE CBOR
// (including Level, Class, and KeyedMetaSHA256, while remaining invariant under Redacted/Forgotten stubbing).
func ComputeBlobHash(b BlobObject) (string, error) {
	h, err := core.HashCBOR(core.TagBlobV1, blobCommitmentCBOR(b))
	if err != nil {
		return "", err
	}
	return core.ToHex(h), nil
}

// BuildTree constructs a deterministically sorted TreeObject hashed via DSHA-256 over RFC 8949 CDE CBOR.
func BuildTree(entries []TreeEntry) (*TreeObject, error) {
	byPath := make(map[string]TreeEntry, len(entries))
	for _, e := range entries {
		p, err := NormalizePath(e.Path)
		if err != nil {
			return nil, err
		}
		e.Path = p
		if e.Mode == "" {
			e.Mode = "100644"
		}
		byPath[p] = e
	}
	sorted := make([]TreeEntry, 0, len(byPath))
	for _, e := range byPath {
		sorted = append(sorted, e)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h, err := core.HashCBOR(core.TagTreeV1, treeEntriesCBOR(sorted))
	if err != nil {
		return nil, err
	}
	return &TreeObject{
		TreeHash: core.ToHex(h),
		Entries:  sorted,
	}, nil
}

// TreeBucketKey maps a normalized repo path deterministically to one of 256 uniform hex buckets ("00".."ff").
func TreeBucketKey(normPath string) string {
	sum := sha256.Sum256([]byte(normPath))
	return hex.EncodeToString(sum[:1])
}

func isValidBucketKey(k string) bool {
	if len(k) != 2 {
		return false
	}
	for i := 0; i < 2; i++ {
		c := k[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// BuildShardedRootTree constructs a v2 radix root TreeObject over up to 256 non-empty bucket TreeHashes.
func BuildShardedRootTree(subtrees map[string]string) (*TreeObject, error) {
	if len(subtrees) == 0 {
		return BuildTree(nil)
	}
	clean := make(map[string]string, len(subtrees))
	keys := make([]string, 0, len(subtrees))
	for k, h := range subtrees {
		key := strings.ToLower(strings.TrimSpace(k))
		val := strings.ToLower(strings.TrimSpace(h))
		if val == "" {
			continue
		}
		if !isValidBucketKey(key) || len(val) != 64 {
			return nil, fmt.Errorf("memrepo: invalid subtree bucket %q -> %q", k, h)
		}
		if _, dup := clean[key]; !dup {
			keys = append(keys, key)
		}
		clean[key] = val
	}
	if len(clean) == 0 {
		return BuildTree(nil)
	}
	h, err := core.HashCBOR(core.TagTreeV2, core.CBORStringMapVal(clean))
	if err != nil {
		return nil, err
	}
	return &TreeObject{
		TreeHash: core.ToHex(h),
		Subtrees: clean,
	}, nil
}

// BuildShardedTree partitions entries across 256 deterministic prefix buckets and returns the
// v2 root TreeObject alongside a map[bucketTreeHash]TreeObject of all bucket leaf trees.
func BuildShardedTree(entries []TreeEntry) (*TreeObject, map[string]TreeObject, error) {
	if len(entries) == 0 {
		empty, err := BuildTree(nil)
		return empty, nil, err
	}
	byBucket := make(map[string][]TreeEntry)
	seen := make(map[string]TreeEntry, len(entries))
	for _, e := range entries {
		p, err := NormalizePath(e.Path)
		if err != nil {
			return nil, nil, err
		}
		e.Path = p
		if e.Mode == "" {
			e.Mode = "100644"
		}
		seen[p] = e
	}
	for p, e := range seen {
		bk := TreeBucketKey(p)
		byBucket[bk] = append(byBucket[bk], e)
	}
	subtrees := make(map[string]string, len(byBucket))
	bucketObjs := make(map[string]TreeObject, len(byBucket))
	for bk, list := range byBucket {
		bt, err := BuildTree(list)
		if err != nil {
			return nil, nil, err
		}
		subtrees[bk] = bt.TreeHash
		bucketObjs[bt.TreeHash] = *bt
	}
	root, err := BuildShardedRootTree(subtrees)
	if err != nil {
		return nil, nil, err
	}
	return root, bucketObjs, nil
}

// VerifyTreeObject verifies the canonical TreeHash of either a v1 leaf/flat TreeObject or a v2 sharded root TreeObject.
func VerifyTreeObject(tr TreeObject) error {
	if len(tr.Subtrees) > 0 {
		if len(tr.Entries) > 0 {
			return errors.New("memrepo: sharded root tree must not carry inline entries")
		}
		rebuilt, err := BuildShardedRootTree(tr.Subtrees)
		if err != nil {
			return err
		}
		if !strings.EqualFold(rebuilt.TreeHash, tr.TreeHash) {
			return fmt.Errorf("memrepo: sharded tree hash mismatch (want %s got %s)", rebuilt.TreeHash, tr.TreeHash)
		}
		return nil
	}
	rebuilt, err := BuildTree(tr.Entries)
	if err != nil {
		return err
	}
	if !strings.EqualFold(rebuilt.TreeHash, tr.TreeHash) {
		return fmt.Errorf("memrepo: tree hash mismatch (want %s got %s)", rebuilt.TreeHash, tr.TreeHash)
	}
	return nil
}

// ResolveTreeEntries expands a sharded root TreeObject (or returns a flat TreeObject's entries)
// in deterministic sorted path order using lookup(bucketHash).
func ResolveTreeEntries(tr TreeObject, lookup func(hash string) (TreeObject, bool)) ([]TreeEntry, error) {
	if len(tr.Subtrees) == 0 {
		return tr.Entries, nil
	}
	if lookup == nil {
		return nil, errors.New("memrepo: subtree lookup required for sharded tree")
	}
	keys := make([]string, 0, len(tr.Subtrees))
	for k := range tr.Subtrees {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var all []TreeEntry
	for _, k := range keys {
		bh := tr.Subtrees[k]
		if bh == "" {
			continue
		}
		bTr, ok := lookup(bh)
		if !ok {
			return nil, fmt.Errorf("memrepo: missing subtree %s (bucket %s)", bh, k)
		}
		if len(bTr.Subtrees) > 0 {
			return nil, fmt.Errorf("memrepo: nested subtrees forbidden in bucket %s", k)
		}
		all = append(all, bTr.Entries...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Path < all[j].Path })
	return all, nil
}

// FlattenTreeFromMap resolves any TreeObject (v1 flat or v2 sharded root) from an in-memory map[hash]TreeObject.
func FlattenTreeFromMap(hash string, trees map[string]TreeObject) ([]TreeEntry, error) {
	if hash == "" {
		return nil, nil
	}
	tr, ok := trees[hash]
	if !ok {
		return nil, fmt.Errorf("memrepo: tree %s not found", hash)
	}
	return ResolveTreeEntries(tr, func(bh string) (TreeObject, bool) {
		sub, exists := trees[bh]
		return sub, exists
	})
}

func guessMediaType(p string, raw []byte) string {
	lower := strings.ToLower(p)
	switch {
	case strings.HasSuffix(lower, ".md"):
		return "text/markdown"
	case strings.HasSuffix(lower, ".json"):
		return "application/json"
	case strings.HasSuffix(lower, ".pdf"):
		return "application/pdf"
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".csv"):
		return "text/csv"
	case strings.HasSuffix(lower, ".txt"):
		return "text/plain"
	}
	for _, b := range raw {
		if b == 0 {
			return "application/octet-stream"
		}
	}
	return "text/plain"
}
