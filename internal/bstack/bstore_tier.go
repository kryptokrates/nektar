package bstack

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lightwebinc/bbox/boxrec"
	"github.com/lightwebinc/bstore/storerec"
)

const (
	// InlineOfferMaxBytes is the maximum raw BRC-369 ciphertext size embedded directly inside an OfferAd
	// on the `tm_nektar_*` CBOR wire (12 KiB leaves headroom for OfferAd metadata under 16 KiB MaxPayload).
	InlineOfferMaxBytes = 12 << 10
	// InlineOfferMaxHexLen is the hex-encoded length corresponding to InlineOfferMaxBytes (24,576 chars).
	InlineOfferMaxHexLen = InlineOfferMaxBytes * 2
	// InlineDirectMaxBytes is Lightweb's 16 KiB (`boxrec.MaxContent`) Tier-1 inline payload bound.
	InlineDirectMaxBytes = boxrec.MaxContent
	// InlineDirectMaxHexLen is the hex-encoded length corresponding to InlineDirectMaxBytes (32,768 chars).
	InlineDirectMaxHexLen = InlineDirectMaxBytes * 2

	DeliveryTierInline = "inline"
	DeliveryTierBstore = "bstore"

	defaultBstoreSuffix10 = "nektarstor"
)

// ClassifyDeliveryTier returns "inline" for <= 16 KiB payloads (`boxrec.MaxContent`) and "bstore"
// for > 16 KiB capsules/blobs offloaded to `bstore` + CHIRP (BRC-167).
func ClassifyDeliveryTier(payloadBytes int) string {
	if payloadBytes <= InlineDirectMaxBytes {
		return DeliveryTierInline
	}
	return DeliveryTierBstore
}

// NormalizeBstoreBucket normalizes a bucket name into Lightweb's `<name>_<10letters>` grammar (`storerec.MinBucket`..`MaxBucket`).
func NormalizeBstoreBucket(bucket string) string {
	s := strings.ToLower(strings.TrimSpace(bucket))
	if s == "" {
		s = "nektar"
	}
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < storerec.MaxName; i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteByte(c)
		}
	}
	prefix := strings.Trim(b.String(), "-")
	if prefix == "" {
		prefix = "nektar"
	}
	return prefix + "_" + defaultBstoreSuffix10
}

// ComputeBstoreBucketID computes Lightweb's deterministic 32-byte `bstore` bucket ID (`storerec.DomainBucketID`).
func ComputeBstoreBucketID(ownerPub []byte, bucket string) [32]byte {
	h := sha256.New()
	h.Write([]byte(storerec.DomainBucketID))
	h.Write(ownerPub)
	h.Write([]byte(NormalizeBstoreBucket(bucket)))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// BuildBstoreEntryForData stores `data` into a content-addressed BRC-167 CHIRP store and constructs
// a spec-validated Lightweb `storerec.Entry` (`ste\x01` canonical CBOR record) describing the object.
func BuildBstoreEntryForData(
	store ObjectStore,
	bucket string,
	writerPub []byte,
	objectKey string,
	contentType string,
	data []byte,
	createdMs uint64,
) (chirpURL string, entry *storerec.Entry, entryCBOR []byte, hashes [][32]byte, err error) {
	if store == nil {
		return "", nil, nil, nil, errors.New("bstore tier: object store required")
	}
	if len(writerPub) != 33 {
		return "", nil, nil, nil, storerec.ErrIdentity
	}
	normBucket := NormalizeBstoreBucket(bucket)
	cleanKey := strings.TrimLeft(strings.TrimSpace(objectKey), "/")
	if cleanKey == "" {
		cleanKey = "capsules/default.nkt"
	}
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/octet-stream"
	}
	if createdMs == 0 {
		createdMs = uint64(time.Now().UnixMilli())
	}

	var rootHash [32]byte
	var contentHash [32]byte
	var proofRoot [32]byte
	if len(data) == 0 {
		rootHash = storerec.EmptyRoot
		contentHash = storerec.EmptyHash
		proofRoot = storerec.EmptyHash
	} else {
		var sErr error
		chirpURL, rootHash, hashes, sErr = StoreChirpWithHashes(store, data)
		if sErr != nil {
			return "", nil, nil, nil, sErr
		}
		contentHash = sha256.Sum256(data)
		proofRoot = computeSegmentProofRoot(data)
	}

	e := &storerec.Entry{
		Bucket:       normBucket,
		ID:           ComputeBstoreBucketID(writerPub, normBucket),
		Writer:       append([]byte(nil), writerPub...),
		Key:          cleanKey,
		Kind:         storerec.KindPut,
		Created:      createdMs,
		Size:         uint64(len(data)),
		Root:         rootHash,
		ContentHash:  contentHash,
		ProofRoot:    proofRoot,
		ProofProfile: storerec.ProofProfile,
		ContentType:  contentType,
	}
	enc, encErr := e.Encode()
	if encErr != nil {
		return "", nil, nil, nil, fmt.Errorf("bstore tier: encode storerec.Entry: %w", encErr)
	}
	return chirpURL, e, enc, hashes, nil
}

func computeSegmentProofRoot(data []byte) [32]byte {
	if len(data) == 0 {
		return storerec.EmptyHash
	}
	h := sha256.New()
	for off := 0; off < len(data); off += storerec.SegmentSize {
		end := off + storerec.SegmentSize
		if end > len(data) {
			end = len(data)
		}
		seg := sha256.Sum256(data[off:end])
		h.Write(seg[:])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
