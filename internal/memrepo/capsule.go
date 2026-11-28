package memrepo

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/store"
)

const (
	// CapsuleMagicHeader identifies a binary .nkt Memory Capsule container (NKTCAP1\n + mode byte + zstd CBOR sequence).
	CapsuleMagicHeader = "NKTCAP1\n"
	// CapsuleFormatVersion identifies the canonical capsule schema version.
	CapsuleFormatVersion = "nektar-capsule-v1"
	// MaxCapsuleDecodedBytes bounds convenience in-memory decoded capsule payloads (64 MiB);
	// streaming callers via VerifyCapsuleStream / NewCapsuleWriter have O(1) chunk memory.
	MaxCapsuleDecodedBytes = 64 << 20
	// CapsuleBlobChunkSize is the maximum binary chunk size emitted per TypeBlobChunk record (1 MiB).
	CapsuleBlobChunkSize = 1 << 20
	// MaxCapsuleRecordBytes caps any single CBOR sequence record (16 MiB).
	MaxCapsuleRecordBytes = 16 << 20

	// CapsuleModePlainZstd indicates an unencrypted zstd stream of RFC 8742 CBOR Sequence records.
	CapsuleModePlainZstd byte = 0x00
	// CapsuleModeAEAD indicates an outer BRC-42/ECDH AES-256-GCM chunked envelope wrapping the zstd CBOR Sequence.
	CapsuleModeAEAD byte = 0x01
)

// CapsuleEnvelopeHeader is the authenticated plaintext CBOR header (AAD) of an encrypted (mode 0x01) .nkt capsule.
type CapsuleEnvelopeHeader struct {
	Version      uint64 `json:"version"`
	KDF          string `json:"kdf"`
	Cipher       string `json:"cipher"`
	Compression  string `json:"compression"`
	Salt         []byte `json:"salt"`       // 4-byte nonce salt for chunked AES-256-GCM
	WrappedDEK   []byte `json:"wrappedDek"` // ephemeral 32-byte DEK wrapped via BRC-42 / ECDH
	RecipientPub string `json:"recipientPub,omitempty"`
}

// CapsuleTrailer is the mandatory final record of every .nkt capsule authenticating stream completeness.
type CapsuleTrailer struct {
	StreamDigest []byte            `json:"streamDigest"` // 32B DSHA-256 of all preceding CBOR sequence item bytes
	RecordCounts map[uint64]uint64 `json:"recordCounts"`
	SignerPub    string            `json:"signerPub,omitempty"`
	Signature    []byte            `json:"signature,omitempty"` // low-S DER over DSHA256(TagCapsuleTrailerV1 || signableCBOR)
}

// CapsuleHeader holds the top-level metadata record (first item in every .nkt CBOR sequence).
type CapsuleHeader struct {
	Format         string `json:"format"`
	Repo           string `json:"repo"`
	Branch         string `json:"branch"`
	GenesisPub     string `json:"genesisPub,omitempty"`
	GenesisShardID string `json:"genesisShardId,omitempty"`
	OwnerPub       string `json:"ownerPub"`
	BaseCommit     string `json:"baseCommit,omitempty"`
	HeadCommit     string `json:"headCommit,omitempty"`
	BaseAnchorN    int64  `json:"baseAnchorN"` // -1 for full capsule starting at n=0; >=0 for incremental delta
	BaseAnchorTxID string `json:"baseAnchorTxid,omitempty"`
	BaseStateQ     []byte `json:"baseStateQ,omitempty"`  // 32B q_base for incremental automaton replay
	BaseMmrRoot    []byte `json:"baseMmrRoot,omitempty"` // 32B MMR root at BaseAnchorN for MMRConsistencyProof
	ExportedAt     int64  `json:"exportedAt"`
	SelfContained  bool   `json:"selfContained,omitempty"`
}

// MemoryCapsule is the in-memory view of a portable, self-verifying .nkt Memory Capsule.
type MemoryCapsule struct {
	Format            string                           `json:"format"`
	Repo              string                           `json:"repo"`
	Branch            string                           `json:"branch"`
	GenesisPub        string                           `json:"genesisPub,omitempty"`
	GenesisShardID    string                           `json:"genesisShardId,omitempty"`
	OwnerPub          string                           `json:"ownerPub"`
	BaseCommit        string                           `json:"baseCommit,omitempty"`
	HeadCommit        string                           `json:"headCommit,omitempty"`
	BaseAnchorN       int64                            `json:"baseAnchorN"`
	BaseAnchorTxID    string                           `json:"baseAnchorTxid,omitempty"`
	BaseStateQ        []byte                           `json:"baseStateQ,omitempty"`
	BaseMmrRoot       []byte                           `json:"baseMmrRoot,omitempty"`
	SelfContained     bool                             `json:"selfContained,omitempty"`
	ExportedAt        int64                            `json:"exportedAt"`
	Commits           []CommitObject                   `json:"commits"`
	Trees             map[string]TreeObject            `json:"trees"`
	Blobs             map[string]BlobObject            `json:"blobs"`
	Transitions       []core.TransitionRecord          `json:"transitions,omitempty"`
	Anchors           []store.BatchAnchor              `json:"anchors,omitempty"`
	AnchorConsistency *core.MMRConsistencyProof        `json:"anchorConsistency,omitempty"`
	Fragments         []store.FragmentRecord           `json:"fragments,omitempty"`
	BatchProofs       map[int64]*SubsetDisclosureProof `json:"batchProofs,omitempty"`
	Trailer           *CapsuleTrailer                  `json:"trailer,omitempty"`
}

// CapsuleEncodeOpts configures streaming .nkt encoding (self-contained blob chunking, trailer signing, outer AEAD).
type CapsuleEncodeOpts struct {
	// Objects resolves volumetric CHIRP blobs when SelfContained is true.
	Objects bstack.ObjectStore
	// SelfContained streams volumetric (>64 KiB) plaintext blobs inline as <=1 MiB TypeBlobChunk records.
	SelfContained bool
	// SignerPub + SignDigest optionally sign the final CapsuleTrailer for exporter authentication.
	SignerPub  string
	SignDigest func(digest []byte) ([]byte, error)
	// WrapDEK optionally enables mode 0x01 outer BRC-42/ECDH AEAD encryption over the entire zstd CBOR stream.
	WrapDEK      func(dek []byte) (wrappedDEK []byte, recipientPub string, err error)
	RecipientPub string
}

// CapsuleVerifyOpts configures streaming/offline .nkt verification.
type CapsuleVerifyOpts struct {
	Objects              bstack.ObjectStore
	Tracker              chaintracker.ChainTracker
	MemoryMode           bool
	AllowMissingExternal bool               // false = fail closed on missing external CHIRP blobs
	RequireSignedTrailer bool               // true = reject capsules without a verified exporter Trailer signature
	LocalBaseAnchor      *store.BatchAnchor // receiver's verified anchor at BaseAnchorN for incremental deltas
	// UnwrapDEK decrypts the ephemeral 32-byte DEK when reading an encrypted (mode 0x01) .nkt capsule.
	UnwrapDEK func(hdr CapsuleEnvelopeHeader) (dek []byte, err error)
}

// CapsuleVerifyReport summarizes offline cryptographic, Automaton, and BSV SPV verification of a .nkt capsule.
type CapsuleVerifyReport struct {
	OK                   bool            `json:"ok"`
	Repo                 string          `json:"repo"`
	Branch               string          `json:"branch"`
	HeadCommit           string          `json:"headCommit"`
	CommitsVerified      int             `json:"commitsVerified"`
	TreesVerified        int             `json:"treesVerified"`
	BlobsVerified        int             `json:"blobsVerified"`
	BlobsRedacted        int             `json:"blobsRedacted,omitempty"`
	BlobsMissingExternal int             `json:"blobsMissingExternal,omitempty"`
	EdgesVerified        int             `json:"edgesVerified,omitempty"`
	TransitionsVerified  int             `json:"transitionsVerified,omitempty"`
	AnchorsVerified      int             `json:"anchorsVerified"`
	SettledAnchors       int             `json:"settledAnchors"`
	TrailerVerified      bool            `json:"trailerVerified"`
	TrailerSignerPub     string          `json:"trailerSignerPub,omitempty"`
	EncryptedEnvelope    bool            `json:"encryptedEnvelope,omitempty"`
	SettledByN           map[int64]bool  `json:"-"`
	BlockHeightByN       map[int64]int64 `json:"-"`
	GenesisShardID       string          `json:"genesisShardId,omitempty"`
	Reason               string          `json:"reason,omitempty"`
}

// IsCapsuleBytes reports whether raw starts with the NKTCAP1 binary framing magic.
func IsCapsuleBytes(raw []byte) bool {
	return bytes.HasPrefix(raw, []byte(CapsuleMagicHeader))
}

// EncodeCapsule serializes cap as a zstd-compressed RFC 8742 CBOR Sequence with NKTCAP1 framing.
func EncodeCapsule(cap *MemoryCapsule) ([]byte, error) {
	return EncodeCapsuleWithOpts(cap, CapsuleEncodeOpts{})
}

// EncodeCapsuleWithOpts serializes cap using the streaming CBOR Sequence + zstd encoder with optional
// self-contained volumetric chunking, signed completeness trailer, and outer BRC-42/ECDH AEAD envelope.
func EncodeCapsuleWithOpts(cap *MemoryCapsule, opts CapsuleEncodeOpts) ([]byte, error) {
	if cap == nil {
		return nil, errors.New("memrepo: nil capsule")
	}
	var buf bytes.Buffer
	if err := WriteCapsuleStream(&buf, cap, opts); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecodeCapsule strictly decodes a plaintext (mode 0x00) .nkt CBOR Sequence capsule and verifies its Trailer digest.
func DecodeCapsule(raw []byte) (*MemoryCapsule, error) {
	return DecodeCapsuleWithOpts(raw, CapsuleVerifyOpts{})
}

// DecodeCapsuleWithOpts decodes a plaintext or outer-AEAD encrypted .nkt capsule and verifies its Trailer digest.
func DecodeCapsuleWithOpts(raw []byte, opts CapsuleVerifyOpts) (*MemoryCapsule, error) {
	return ReadCapsuleStream(bytes.NewReader(raw), opts)
}

// VerifyCapsule verifies every cryptographic commitment in cap offline (convenience wrapper over VerifyCapsuleWithOpts).
func VerifyCapsule(
	ctx context.Context,
	cap *MemoryCapsule,
	objects bstack.ObjectStore,
	tracker chaintracker.ChainTracker,
	memoryMode bool,
) (*CapsuleVerifyReport, error) {
	return VerifyCapsuleWithOpts(ctx, cap, CapsuleVerifyOpts{
		Objects:    objects,
		Tracker:    tracker,
		MemoryMode: memoryMode,
	})
}

// VerifyCapsuleWithOpts verifies every cryptographic commitment in cap offline:
// blob hashes & streamed/CHIRP contents (fail-closed on missing blobs unless AllowMissingExternal),
// redacted hash-preserving stubs, trees & JevGraph sidecars, signed commits & DAG lineage,
// Merkle Automaton TransitionRecord replay, contiguous/checkpointed NektarAnchor Rúnar UTXO SPV + MMR,
// and fragment/SubsetDisclosureProof batch roots.
func VerifyCapsuleWithOpts(
	ctx context.Context,
	cap *MemoryCapsule,
	opts CapsuleVerifyOpts,
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
	if cap.Trailer != nil {
		if err := verifyTrailerSignature(cap.Trailer, opts.RequireSignedTrailer); err != nil {
			rep.OK = false
			rep.Reason = err.Error()
			return rep, nil
		}
		rep.TrailerVerified = len(cap.Trailer.StreamDigest) == 32
		rep.TrailerSignerPub = cap.Trailer.SignerPub
	} else if opts.RequireSignedTrailer {
		rep.OK = false
		rep.Reason = "capsule missing required signed trailer"
		return rep, nil
	}

	if !verifyCapsuleObjects(cap, opts, rep) {
		return rep, nil
	}
	if !verifyCapsuleTransitions(cap, rep) {
		return rep, nil
	}
	if err := verifyCapsuleAnchors(ctx, cap, opts, rep); err != nil || !rep.OK {
		return rep, err
	}
	verifyCapsuleFragmentsAndBatches(cap, rep)
	return rep, nil
}

// VerifyCapsuleStream decodes and verifies a .nkt capsule directly from an io.Reader stream.
func VerifyCapsuleStream(ctx context.Context, r io.Reader, opts CapsuleVerifyOpts) (*MemoryCapsule, *CapsuleVerifyReport, error) {
	cap, encrypted, err := readCapsuleStreamInternal(r, opts)
	if err != nil {
		return nil, nil, err
	}
	rep, err := VerifyCapsuleWithOpts(ctx, cap, opts)
	if rep != nil {
		rep.EncryptedEnvelope = encrypted
		if cap.Trailer != nil {
			rep.TrailerVerified = true
		}
	}
	return cap, rep, err
}
