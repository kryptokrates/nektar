package memrepo

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/store"
)

const (
	// CapsuleMagicHeader identifies a binary gzip-compressed .nkt Memory Capsule container.
	CapsuleMagicHeader = "NKTCAP1\n"
	// CapsuleFormatVersion identifies the JSON payload format inside a MemoryCapsule.
	CapsuleFormatVersion = "nektar-capsule-v1"
	// MaxCapsuleDecodedBytes bounds decoded capsule size against zip-bomb amplification (32 MiB).
	MaxCapsuleDecodedBytes = 32 << 20
)

// MemoryCapsule is a portable, self-verifying container packaging a Memory Repo (or incremental
// base..head commit delta): deduplicated content-addressed commits, trees, inline or CHIRP volumetric
// blobs, and contiguous BSV NektarAnchor AtomicBEEF/BUMP proofs.
type MemoryCapsule struct {
	Format         string                           `json:"format"`
	Repo           string                           `json:"repo"`
	Branch         string                           `json:"branch"`
	GenesisPub     string                           `json:"genesisPub,omitempty"`
	GenesisShardID string                           `json:"genesisShardId,omitempty"`
	OwnerPub       string                           `json:"ownerPub"`
	BaseCommit     string                           `json:"baseCommit,omitempty"`
	HeadCommit     string                           `json:"headCommit,omitempty"`
	ExportedAt     int64                            `json:"exportedAt"`
	Commits        []CommitObject                   `json:"commits"`
	Trees          map[string]TreeObject            `json:"trees"`
	Blobs          map[string]BlobObject            `json:"blobs"`
	Anchors        []store.BatchAnchor              `json:"anchors,omitempty"`
	Fragments      []store.FragmentRecord           `json:"fragments,omitempty"`
	BatchProofs    map[int64]*SubsetDisclosureProof `json:"batchProofs,omitempty"`
}

// CapsuleVerifyReport summarizes offline cryptographic & BSV SPV verification of a MemoryCapsule.
type CapsuleVerifyReport struct {
	OK              bool            `json:"ok"`
	Repo            string          `json:"repo"`
	Branch          string          `json:"branch"`
	HeadCommit      string          `json:"headCommit"`
	CommitsVerified int             `json:"commitsVerified"`
	TreesVerified   int             `json:"treesVerified"`
	BlobsVerified   int             `json:"blobsVerified"`
	EdgesVerified   int             `json:"edgesVerified,omitempty"`
	AnchorsVerified int             `json:"anchorsVerified"`
	SettledAnchors  int             `json:"settledAnchors"`
	SettledByN      map[int64]bool  `json:"-"`
	BlockHeightByN  map[int64]int64 `json:"-"`
	GenesisShardID  string          `json:"genesisShardId,omitempty"`
	Reason          string          `json:"reason,omitempty"`
}

// EncodeCapsule serializes and gzip-compresses cap with the NKTCAP1 binary framing header.
func EncodeCapsule(cap *MemoryCapsule) ([]byte, error) {
	if cap == nil {
		return nil, errors.New("memrepo: nil capsule")
	}
	if cap.Format == "" {
		cap.Format = CapsuleFormatVersion
	}
	rawJSON, err := json.Marshal(cap)
	if err != nil {
		return nil, fmt.Errorf("memrepo: marshal capsule: %w", err)
	}
	var buf bytes.Buffer
	buf.WriteString(CapsuleMagicHeader)
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := gz.Write(rawJSON); err != nil {
		_ = gz.Close()
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// IsCapsuleBytes reports whether raw is a binary NKTCAP1 .nkt MemoryCapsule.
func IsCapsuleBytes(raw []byte) bool {
	return bytes.HasPrefix(raw, []byte(CapsuleMagicHeader))
}

// DecodeCapsule strictly decodes a binary NKTCAP1 MemoryCapsule with size caps.
func DecodeCapsule(raw []byte) (*MemoryCapsule, error) {
	if !bytes.HasPrefix(raw, []byte(CapsuleMagicHeader)) {
		return nil, errors.New("memrepo: invalid capsule header (expected NKTCAP1 binary .nkt)")
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw[len(CapsuleMagicHeader):]))
	if err != nil {
		return nil, fmt.Errorf("memrepo: gunzip capsule: %w", err)
	}
	defer gz.Close()
	jsonBytes, err := io.ReadAll(io.LimitReader(gz, MaxCapsuleDecodedBytes+1))
	if err != nil {
		return nil, fmt.Errorf("memrepo: read capsule gzip: %w", err)
	}
	if len(jsonBytes) > MaxCapsuleDecodedBytes {
		return nil, fmt.Errorf("memrepo: decoded capsule exceeds max %d bytes", MaxCapsuleDecodedBytes)
	}
	var cap MemoryCapsule
	if err := json.Unmarshal(jsonBytes, &cap); err != nil {
		return nil, fmt.Errorf("memrepo: decode capsule json: %w", err)
	}
	if cap.Format != CapsuleFormatVersion {
		return nil, fmt.Errorf("memrepo: unexpected capsule format %q", cap.Format)
	}
	return &cap, nil
}

// VerifyCapsule verifies every cryptographic commitment in cap offline:
// commit hashes/MPE roots/signatures, DAG parent links, tree hashes, blob hashes (inline + CHIRP),
// and contiguous NektarAnchor Rúnar state + ChainRp + BUMP/SPV header proofs.
func VerifyCapsule(
	ctx context.Context,
	cap *MemoryCapsule,
	objects bstack.ObjectStore,
	tracker chaintracker.ChainTracker,
	memoryMode bool,
) (*CapsuleVerifyReport, error) {
	if cap == nil {
		return nil, errors.New("memrepo: nil capsule")
	}
	rep := &CapsuleVerifyReport{
		OK:             true,
		Repo:           cap.Repo,
		Branch:         cap.Branch,
		HeadCommit:     cap.HeadCommit,
		GenesisShardID: cap.GenesisShardID,
		SettledByN:     make(map[int64]bool),
		BlockHeightByN: make(map[int64]int64),
	}

	if !verifyCapsuleObjects(cap, objects, rep) {
		return rep, nil
	}
	if err := verifyCapsuleAnchors(ctx, cap, tracker, memoryMode, rep); err != nil || !rep.OK {
		return rep, err
	}
	verifyCapsuleFragmentsAndBatches(cap, rep)
	return rep, nil
}
