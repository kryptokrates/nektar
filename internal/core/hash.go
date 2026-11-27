package core

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"slices"

	bsvhash "github.com/bsv-blockchain/go-sdk/primitives/hash"
)

// Domain-separation tags — every hash in Nektar is domain-tagged so digests
// never collide across layers or protocols.
var (
	TagLeaf         = []byte("nektar:leaf:v1")
	TagCommitV1     = []byte("nektar:commit:v1")
	TagTreeV1       = []byte("nektar:tree:v1")
	TagBlobV1       = []byte("nektar:blob:v1")
	TagRPrime       = []byte("nektar:rprime:v1")
	TagBatchMeta    = []byte("nektar:batch-meta:v1")
	TagStateQ       = []byte("nektar:state-q:v1")
	TagTransitionV1 = []byte("nektar:transition:v1")
	TagMMRNode      = []byte("nektar:mmr:node:v1")
	TagMMRBag       = []byte("nektar:mmr:bag:v1")
	TagTripleV1           = []byte("nektar:triple:v1")
	TagOntologyV1         = []byte("nektar:ontology:v1")
	TagOntologyManifestV1 = []byte("nektar:ont-manifest:v1")
	TagRepAttestV1        = []byte("nektar:rep-attest:v1")
	TagWorkBountyV1       = []byte("nektar:work-bounty:v1")
	TagLatticeDown        = []byte("nektar:lattice:down:v1")
	TagProvV2       = []byte("nektar:prov:v2")
	TagProvSig      = []byte("nektar:provsig:v1")
	TagReceiptSig   = []byte("nektar:rcptsig:v1")
	TagDelegationV2 = []byte("nektar:delegation:v2")

	// MetaFlag is the 4-byte Metanet prefix ("META") pushed at the start of NektarAnchor state.
	MetaFlag = []byte{0x4d, 0x45, 0x54, 0x41}
)

// Zero32Bytes returns a freshly allocated 32-byte zero slice for genesis R'_-1.
func Zero32Bytes() []byte {
	return make([]byte, 32)
}

// SHA256 computes single SHA-256.
func SHA256(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// DSHA256 computes double SHA-256 (SHA256(SHA256(b))).
func DSHA256(b []byte) []byte {
	first := sha256.Sum256(b)
	second := sha256.Sum256(first[:])
	return second[:]
}

// Hash160 computes RIPEMD160(SHA256(b)).
func Hash160(b []byte) []byte {
	return bsvhash.Hash160(b)
}

// ShardIDFromPubkey derives the canonical 20-byte hex shard ID = hex(hash160(pubKeyBytes)).
func ShardIDFromPubkey(pubKeyBytes []byte) string {
	return hex.EncodeToString(Hash160(pubKeyBytes))
}

// Concat joins byte slices into a freshly allocated buffer.
func Concat(parts ...[]byte) []byte {
	total := 0
	for _, p := range parts {
		total += len(p)
	}
	out := make([]byte, 0, total)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// U32LE encodes n as 4 little-endian bytes.
func U32LE(n uint32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], n)
	return b[:]
}

// U64LE encodes n as 8 little-endian bytes.
func U64LE(n uint64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], n)
	return b[:]
}

// EqualBytes compares two byte slices for exact equality.
func EqualBytes(a, b []byte) bool {
	return slices.Equal(a, b)
}

// ToHex encodes bytes as lowercase hex.
func ToHex(b []byte) string {
	return hex.EncodeToString(b)
}

// ChainRpWithTag computes R'_n = DSHA256(tag || prevRp || R || u64le(size)).
func ChainRpWithTag(tag, prevRp, r []byte, size uint64) []byte {
	return DSHA256(Concat(tag, prevRp, r, U64LE(size)))
}

// ChainRp computes R'_n = DSHA256(TagRPrime || prevRp || R || u64le(size)).
func ChainRp(prevRp, r []byte, size uint64) []byte {
	return ChainRpWithTag(TagRPrime, prevRp, r, size)
}

func pad32(b []byte) []byte {
	out := make([]byte, 32)
	if len(b) == 32 {
		copy(out, b)
	}
	return out
}

// ComputeBatchMetaHash computes the domain-separated 32-byte commitment over the Git tree head,
// active Cedar policy commitment, and active ontology shard head at anchor time.
func ComputeBatchMetaHash(treeHash, policyHash, ontologyHash []byte) []byte {
	return DSHA256(Concat(TagBatchMeta, pad32(treeHash), pad32(policyHash), pad32(ontologyHash)))
}

// ChainStateQ computes the consensus-enforced NektarAnchor Merkle Automaton state hash:
// Q_n = DSHA256(prevQ32 || newR32 || batchMetaHash32 || newMmrRoot32).
// Matches NektarAnchor.Append() on-chain byte-for-byte.
func ChainStateQ(prevQ, newR, batchMetaHash, newMmrRoot []byte) []byte {
	return DSHA256(Concat(pad32(prevQ), pad32(newR), pad32(batchMetaHash), pad32(newMmrRoot)))
}
