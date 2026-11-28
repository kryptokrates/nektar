package memrepo

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/lightwebinc/bcommon/cbor"

	"github.com/kryptokrates/nektar/internal/core"
)

const (
	// CommitKindNormal is a standard data/merge/branch commit.
	CommitKindNormal = 0
	// CommitKindTombstone is an append-only cryptographic-erasure commit produced by Forget().
	CommitKindTombstone = 1
)

// ToRedactedStub returns a hash-preserving BlobStub with all secret/plaintext payloads stripped
// so historical TreeHash and CommitHash commitments remain 100% cryptographically verifiable.
func (b BlobObject) ToRedactedStub() BlobObject {
	kmSHA := b.KeyedMetaSHA256
	if kmSHA == "" && len(b.KeyedMeta) > 0 {
		sum := sha256.Sum256(b.KeyedMeta)
		kmSHA = hex.EncodeToString(sum[:])
	}
	stub := b
	stub.Inline = nil
	stub.KeyedMeta = nil
	stub.KeyedMetaSHA256 = kmSHA
	stub.Redacted = true
	return stub
}

func blobCommitmentCBOR(b BlobObject) cbor.Map {
	kmSHA := b.KeyedMetaSHA256
	if kmSHA == "" && len(b.KeyedMeta) > 0 {
		sum := sha256.Sum256(b.KeyedMeta)
		kmSHA = hex.EncodeToString(sum[:])
	}
	m := cbor.Map{
		{Key: uint64(1), Val: b.Path},
		{Key: uint64(2), Val: b.MediaType},
		{Key: uint64(3), Val: b.Size},
		{Key: uint64(4), Val: b.ContentSHA256},
	}
	if b.ChirpURL != "" {
		m = append(m, cbor.Pair{Key: uint64(5), Val: b.ChirpURL})
	}
	if b.Encrypted {
		m = append(m, cbor.Pair{Key: uint64(6), Val: true})
	}
	if b.BorgGroup != "" {
		m = append(m, cbor.Pair{Key: uint64(7), Val: b.BorgGroup})
	}
	if b.BorgEpoch != 0 {
		m = append(m, cbor.Pair{Key: uint64(8), Val: b.BorgEpoch})
	}
	if b.LatticeEpoch > 0 {
		m = append(m, cbor.Pair{Key: uint64(9), Val: b.LatticeEpoch})
	}
	if kmSHA != "" {
		m = append(m, cbor.Pair{Key: uint64(10), Val: kmSHA})
	}
	if b.Level != 0 {
		m = append(m, cbor.Pair{Key: uint64(11), Val: int64(b.Level)})
	}
	if b.Class != "" {
		m = append(m, cbor.Pair{Key: uint64(12), Val: b.Class})
	}
	return m
}

func treeEntryToCBOR(e TreeEntry) cbor.Map {
	m := cbor.Map{
		{Key: uint64(1), Val: e.Mode},
		{Key: uint64(2), Val: e.Path},
		{Key: uint64(3), Val: e.BlobHash},
		{Key: uint64(4), Val: e.MediaType},
		{Key: uint64(5), Val: e.Size},
		{Key: uint64(6), Val: e.ContentSHA256},
	}
	if e.ChirpURL != "" {
		m = append(m, cbor.Pair{Key: uint64(7), Val: e.ChirpURL})
	}
	if e.Encrypted {
		m = append(m, cbor.Pair{Key: uint64(8), Val: true})
	}
	if e.Level != 0 {
		m = append(m, cbor.Pair{Key: uint64(9), Val: int64(e.Level)})
	}
	if e.Class != "" {
		m = append(m, cbor.Pair{Key: uint64(10), Val: e.Class})
	}
	return m
}

var treeEntryKnownKeys = core.KnownKeyRange(1, 10)

func decodeTreeEntryCBOR(v cbor.Value) (TreeEntry, error) {
	m, _, err := core.SplitCBORMap(v, treeEntryKnownKeys)
	if err != nil {
		return TreeEntry{}, err
	}
	var e TreeEntry
	if e.Mode, err = core.CBORText(m, 1); err != nil {
		return TreeEntry{}, err
	}
	if e.Path, err = core.CBORText(m, 2); err != nil {
		return TreeEntry{}, err
	}
	if e.BlobHash, err = core.CBORText(m, 3); err != nil {
		return TreeEntry{}, err
	}
	if e.MediaType, err = core.CBORText(m, 4); err != nil {
		return TreeEntry{}, err
	}
	if e.Size, err = core.CBORInt(m, 5); err != nil {
		return TreeEntry{}, err
	}
	if e.ContentSHA256, err = core.CBORText(m, 6); err != nil {
		return TreeEntry{}, err
	}
	if e.ChirpURL, err = core.CBORText(m, 7); err != nil {
		return TreeEntry{}, err
	}
	if e.Encrypted, err = core.CBORBool(m, 8); err != nil {
		return TreeEntry{}, err
	}
	lvl, err := core.CBORInt(m, 9)
	if err != nil {
		return TreeEntry{}, err
	}
	e.Level = int(lvl)
	if e.Class, err = core.CBORText(m, 10); err != nil {
		return TreeEntry{}, err
	}
	return e, nil
}

func treeEntriesCBOR(sorted []TreeEntry) []cbor.Value {
	arr := make([]cbor.Value, len(sorted))
	for i, e := range sorted {
		arr[i] = treeEntryToCBOR(e)
	}
	return arr
}

func parentsToCBOR(parents []ParentRef) []cbor.Value {
	arr := make([]cbor.Value, len(parents))
	for i, p := range parents {
		pm := cbor.Map{
			{Key: uint64(1), Val: p.CommitHash},
		}
		if p.AnchorTxID != "" {
			pm = append(pm, cbor.Pair{Key: uint64(2), Val: p.AnchorTxID})
		}
		if p.AnchorVout != 0 {
			pm = append(pm, cbor.Pair{Key: uint64(3), Val: uint64(p.AnchorVout)})
		}
		if p.RepoGenesis != "" {
			pm = append(pm, cbor.Pair{Key: uint64(4), Val: p.RepoGenesis})
		}
		arr[i] = pm
	}
	return arr
}

var parentRefKnownKeys = core.KnownKeyRange(1, 4)

func decodeParentsCBOR(arr []cbor.Value) ([]ParentRef, error) {
	if len(arr) == 0 {
		return nil, nil
	}
	out := make([]ParentRef, len(arr))
	for i, item := range arr {
		m, _, err := core.SplitCBORMap(item, parentRefKnownKeys)
		if err != nil {
			return nil, fmt.Errorf("memrepo: parent[%d]: %w", i, err)
		}
		var p ParentRef
		if p.CommitHash, err = core.CBORText(m, 1); err != nil {
			return nil, err
		}
		if p.AnchorTxID, err = core.CBORText(m, 2); err != nil {
			return nil, err
		}
		vout, err := core.CBORUint(m, 3)
		if err != nil {
			return nil, err
		}
		p.AnchorVout = uint32(vout)
		if p.RepoGenesis, err = core.CBORText(m, 4); err != nil {
			return nil, err
		}
		out[i] = p
	}
	return out, nil
}

func commitSignableCBOR(c CommitObject) cbor.Map {
	m := cbor.Map{
		{Key: uint64(1), Val: int64(c.Version)},
		{Key: uint64(2), Val: c.Repo},
		{Key: uint64(3), Val: c.Branch},
		{Key: uint64(5), Val: c.TreeHash},
		{Key: uint64(6), Val: c.MPERoot},
		{Key: uint64(7), Val: c.AuthorPub},
		{Key: uint64(8), Val: c.ModelID},
		{Key: uint64(11), Val: c.Message},
		{Key: uint64(12), Val: c.Timestamp},
	}
	if len(c.Parents) > 0 {
		m = append(m, cbor.Pair{Key: uint64(4), Val: parentsToCBOR(c.Parents)})
	}
	if c.PolicyCommit != "" {
		m = append(m, cbor.Pair{Key: uint64(9), Val: c.PolicyCommit})
	}
	if c.OntologyCommit != "" {
		m = append(m, cbor.Pair{Key: uint64(10), Val: c.OntologyCommit})
	}
	if c.CommitKind != 0 {
		m = append(m, cbor.Pair{Key: uint64(13), Val: int64(c.CommitKind)})
	}
	return m
}

// EncodeBlobObjectCBOR encodes b (including inline/keyedMeta unless redacted) into canonical RFC 8949 CDE CBOR.
func EncodeBlobObjectCBOR(b BlobObject) ([]byte, error) {
	m := blobCommitmentCBOR(b)
	m = append(m, cbor.Pair{Key: uint64(13), Val: b.BlobHash})
	if len(b.Inline) > 0 && !b.Redacted {
		m = append(m, cbor.Pair{Key: uint64(14), Val: b.Inline})
	}
	if len(b.KeyedMeta) > 0 && !b.Redacted {
		m = append(m, cbor.Pair{Key: uint64(15), Val: b.KeyedMeta})
	}
	if b.Redacted {
		m = append(m, cbor.Pair{Key: uint64(16), Val: true})
	}
	if b.Forgotten {
		m = append(m, cbor.Pair{Key: uint64(17), Val: true})
	}
	return core.EncodeCBOR(m)
}

var blobObjectKnownKeys = core.KnownKeyRange(1, 17)

// DecodeBlobObjectCBOR decodes a canonical CBOR BlobObject (enforcing critical/non-critical unknown-key rules).
func DecodeBlobObjectCBOR(raw []byte) (BlobObject, error) {
	v, err := core.DecodeCBOR(raw)
	if err != nil {
		return BlobObject{}, err
	}
	return decodeBlobObjectValue(v)
}

func decodeBlobObjectValue(v cbor.Value) (BlobObject, error) {
	m, _, err := core.SplitCBORMap(v, blobObjectKnownKeys)
	if err != nil {
		return BlobObject{}, err
	}
	var b BlobObject
	if b.Path, err = core.CBORText(m, 1); err != nil {
		return BlobObject{}, err
	}
	if b.MediaType, err = core.CBORText(m, 2); err != nil {
		return BlobObject{}, err
	}
	if b.Size, err = core.CBORInt(m, 3); err != nil {
		return BlobObject{}, err
	}
	if b.ContentSHA256, err = core.CBORText(m, 4); err != nil {
		return BlobObject{}, err
	}
	if b.ChirpURL, err = core.CBORText(m, 5); err != nil {
		return BlobObject{}, err
	}
	if b.Encrypted, err = core.CBORBool(m, 6); err != nil {
		return BlobObject{}, err
	}
	if b.BorgGroup, err = core.CBORText(m, 7); err != nil {
		return BlobObject{}, err
	}
	if b.BorgEpoch, err = core.CBORInt(m, 8); err != nil {
		return BlobObject{}, err
	}
	if b.LatticeEpoch, err = core.CBORUint(m, 9); err != nil {
		return BlobObject{}, err
	}
	if b.KeyedMetaSHA256, err = core.CBORText(m, 10); err != nil {
		return BlobObject{}, err
	}
	lvl, err := core.CBORInt(m, 11)
	if err != nil {
		return BlobObject{}, err
	}
	b.Level = int(lvl)
	if b.Class, err = core.CBORText(m, 12); err != nil {
		return BlobObject{}, err
	}
	if b.BlobHash, err = core.CBORText(m, 13); err != nil {
		return BlobObject{}, err
	}
	if b.Inline, err = core.CBORBytes(m, 14); err != nil {
		return BlobObject{}, err
	}
	if b.KeyedMeta, err = core.CBORBytes(m, 15); err != nil {
		return BlobObject{}, err
	}
	if b.Redacted, err = core.CBORBool(m, 16); err != nil {
		return BlobObject{}, err
	}
	if b.Forgotten, err = core.CBORBool(m, 17); err != nil {
		return BlobObject{}, err
	}
	return b, nil
}

// EncodeTreeObjectCBOR encodes a flat or v2 sharded TreeObject into canonical RFC 8949 CDE CBOR.
func EncodeTreeObjectCBOR(tr TreeObject) ([]byte, error) {
	m := cbor.Map{
		{Key: uint64(1), Val: tr.TreeHash},
	}
	if len(tr.Entries) > 0 {
		sorted := append([]TreeEntry(nil), tr.Entries...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
		m = append(m, cbor.Pair{Key: uint64(2), Val: treeEntriesCBOR(sorted)})
	}
	if len(tr.Subtrees) > 0 {
		m = append(m, cbor.Pair{Key: uint64(3), Val: core.CBORStringMapVal(tr.Subtrees)})
	}
	return core.EncodeCBOR(m)
}

var treeObjectKnownKeys = core.KnownKeyRange(1, 3)

// DecodeTreeObjectCBOR decodes a canonical CBOR TreeObject.
func DecodeTreeObjectCBOR(raw []byte) (TreeObject, error) {
	v, err := core.DecodeCBOR(raw)
	if err != nil {
		return TreeObject{}, err
	}
	return decodeTreeObjectValue(v)
}

func decodeTreeObjectValue(v cbor.Value) (TreeObject, error) {
	m, _, err := core.SplitCBORMap(v, treeObjectKnownKeys)
	if err != nil {
		return TreeObject{}, err
	}
	var tr TreeObject
	if tr.TreeHash, err = core.CBORText(m, 1); err != nil {
		return TreeObject{}, err
	}
	if arr, err := core.CBORArray(m, 2); err != nil {
		return TreeObject{}, err
	} else if len(arr) > 0 {
		tr.Entries = make([]TreeEntry, len(arr))
		for i, item := range arr {
			e, eErr := decodeTreeEntryCBOR(item)
			if eErr != nil {
				return TreeObject{}, fmt.Errorf("memrepo: tree entry[%d]: %w", i, eErr)
			}
			tr.Entries[i] = e
		}
	}
	if tr.Subtrees, err = core.CBORStringMap(m, 3); err != nil {
		return TreeObject{}, err
	}
	return tr, nil
}
