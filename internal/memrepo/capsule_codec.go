package memrepo

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/lightwebinc/bcommon/cbor"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/store"
)

const (
	capRecHeader       uint64 = 1
	capRecBlobMeta     uint64 = 2
	capRecBlobChunk    uint64 = 3
	capRecTree         uint64 = 4
	capRecCommit       uint64 = 5
	capRecTransition   uint64 = 6
	capRecAnchor       uint64 = 7
	capRecFragmentMeta uint64 = 8
	capRecBatchProof   uint64 = 9
	capRecTrailer      uint64 = 10

	maxEnvelopeHeaderBytes = 64 << 10
	aeadStreamChunkSize    = 256 << 10
)

// --- Streaming Chunked AES-256-GCM Envelope (Mode 0x01) ---

type aeadChunkWriter struct {
	dst      io.Writer
	aead     cipher.AEAD
	salt     [4]byte
	hdrBytes []byte
	chunkIdx uint64
	buf      []byte
	closed   bool
}

func newAEADChunkWriter(dst io.Writer, dek []byte, salt [4]byte, hdrBytes []byte) (*aeadChunkWriter, error) {
	blk, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &aeadChunkWriter{
		dst:      dst,
		aead:     aead,
		salt:     salt,
		hdrBytes: hdrBytes,
		buf:      make([]byte, 0, aeadStreamChunkSize),
	}, nil
}

func (w *aeadChunkWriter) chunkNonce(idx uint64) []byte {
	var nonce [12]byte
	copy(nonce[:4], w.salt[:])
	binary.BigEndian.PutUint64(nonce[4:], idx)
	return nonce[:]
}

func (w *aeadChunkWriter) chunkAAD(idx uint64, final bool) []byte {
	var fin byte
	if final {
		fin = 1
	}
	return core.Concat(core.TagCapsuleAEADV1, w.hdrBytes, core.U64LE(idx), []byte{fin})
}

func (w *aeadChunkWriter) flushChunk(data []byte, final bool) error {
	ct := w.aead.Seal(nil, w.chunkNonce(w.chunkIdx), data, w.chunkAAD(w.chunkIdx, final))
	var frameHdr [5]byte
	binary.BigEndian.PutUint32(frameHdr[:4], uint32(len(ct)))
	if final {
		frameHdr[4] = 1
	}
	if _, err := w.dst.Write(frameHdr[:]); err != nil {
		return err
	}
	if _, err := w.dst.Write(ct); err != nil {
		return err
	}
	w.chunkIdx++
	return nil
}

func (w *aeadChunkWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("memrepo: write to closed AEAD stream")
	}
	written := 0
	for len(p) > 0 {
		space := aeadStreamChunkSize - len(w.buf)
		take := len(p)
		if take > space {
			take = space
		}
		w.buf = append(w.buf, p[:take]...)
		p = p[take:]
		written += take
		if len(w.buf) == aeadStreamChunkSize {
			if err := w.flushChunk(w.buf, false); err != nil {
				return written, err
			}
			w.buf = w.buf[:0]
		}
	}
	return written, nil
}

func (w *aeadChunkWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	return w.flushChunk(w.buf, true)
}

type aeadChunkReader struct {
	src      io.Reader
	aead     cipher.AEAD
	salt     [4]byte
	hdrBytes []byte
	chunkIdx uint64
	plain    []byte
	done     bool
}

func newAEADChunkReader(src io.Reader, dek []byte, salt [4]byte, hdrBytes []byte) (*aeadChunkReader, error) {
	blk, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &aeadChunkReader{
		src:      src,
		aead:     aead,
		salt:     salt,
		hdrBytes: hdrBytes,
	}, nil
}

func (r *aeadChunkReader) Read(p []byte) (int, error) {
	for len(r.plain) == 0 {
		if r.done {
			return 0, io.EOF
		}
		var frameHdr [5]byte
		if _, err := io.ReadFull(r.src, frameHdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, err
		}
		ctLen := binary.BigEndian.Uint32(frameHdr[:4])
		final := frameHdr[4] == 1
		if frameHdr[4] > 1 || ctLen > uint32(aeadStreamChunkSize+64) || ctLen < uint32(r.aead.Overhead()) {
			return 0, errors.New("memrepo: invalid AEAD frame header")
		}
		ct := make([]byte, ctLen)
		if _, err := io.ReadFull(r.src, ct); err != nil {
			return 0, err
		}
		var nonce [12]byte
		copy(nonce[:4], r.salt[:])
		binary.BigEndian.PutUint64(nonce[4:], r.chunkIdx)
		var fin byte
		if final {
			fin = 1
		}
		aad := core.Concat(core.TagCapsuleAEADV1, r.hdrBytes, core.U64LE(r.chunkIdx), []byte{fin})
		pt, err := r.aead.Open(nil, nonce[:], ct, aad)
		if err != nil {
			return 0, fmt.Errorf("memrepo: AEAD decrypt chunk %d: %w", r.chunkIdx, err)
		}
		r.chunkIdx++
		r.plain = pt
		if final {
			r.done = true
		}
	}
	n := copy(p, r.plain)
	r.plain = r.plain[n:]
	return n, nil
}

// --- Streaming RFC 8742 CBOR Sequence + zstd CapsuleWriter ---

// StreamCapsuleWriter streams a .nkt Memory Capsule record-by-record in O(1) memory,
// computing a running DSHA-256 stream digest committed into the final CapsuleTrailer.
type StreamCapsuleWriter struct {
	zenc      *zstd.Encoder
	aeadW     *aeadChunkWriter
	hasher    hash.Hash
	counts    map[uint64]uint64
	closed    bool
	opts      CapsuleEncodeOpts
	hdrDone   bool
}

// NewCapsuleWriter opens a streaming .nkt writer on w (mode 0x00 plaintext zstd, or mode 0x01 outer AEAD).
func NewCapsuleWriter(w io.Writer, opts CapsuleEncodeOpts) (*StreamCapsuleWriter, error) {
	if w == nil {
		return nil, errors.New("memrepo: nil capsule writer")
	}
	if _, err := w.Write([]byte(CapsuleMagicHeader)); err != nil {
		return nil, err
	}
	var zDst io.Writer = w
	var aeadW *aeadChunkWriter
	if opts.WrapDEK != nil {
		if _, err := w.Write([]byte{CapsuleModeAEAD}); err != nil {
			return nil, err
		}
		var dek [32]byte
		if _, err := crand.Read(dek[:]); err != nil {
			return nil, err
		}
		defer func() {
			for i := range dek {
				dek[i] = 0
			}
		}()
		var salt [4]byte
		if _, err := crand.Read(salt[:]); err != nil {
			return nil, err
		}
		wrappedDEK, recPub, err := opts.WrapDEK(dek[:])
		if err != nil {
			return nil, fmt.Errorf("memrepo: wrap capsule DEK: %w", err)
		}
		if recPub == "" {
			recPub = opts.RecipientPub
		}
		hdr := CapsuleEnvelopeHeader{
			Version:      1,
			KDF:          "BRC-42/ECDH",
			Cipher:       "AES-256-GCM",
			Compression:  "zstd",
			Salt:         salt[:],
			WrappedDEK:   wrappedDEK,
			RecipientPub: recPub,
		}
		hdrBytes, err := encodeEnvelopeHeaderCBOR(hdr)
		if err != nil {
			return nil, err
		}
		if len(hdrBytes) > maxEnvelopeHeaderBytes {
			return nil, errors.New("memrepo: envelope header exceeds max size")
		}
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(hdrBytes)))
		if _, err := w.Write(lenBuf[:]); err != nil {
			return nil, err
		}
		if _, err := w.Write(hdrBytes); err != nil {
			return nil, err
		}
		aeadW, err = newAEADChunkWriter(w, dek[:], salt, hdrBytes)
		if err != nil {
			return nil, err
		}
		zDst = aeadW
	} else {
		if _, err := w.Write([]byte{CapsuleModePlainZstd}); err != nil {
			return nil, err
		}
	}

	zenc, err := zstd.NewWriter(zDst, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		return nil, err
	}
	return &StreamCapsuleWriter{
		zenc:   zenc,
		aeadW:  aeadW,
		hasher: sha256.New(),
		counts: make(map[uint64]uint64),
		opts:   opts,
	}, nil
}

func (cw *StreamCapsuleWriter) writeRecord(recType uint64, m cbor.Map) error {
	if cw.closed {
		return errors.New("memrepo: capsule writer already closed")
	}
	item := make(cbor.Map, 0, len(m)+1)
	item = append(item, cbor.Pair{Key: uint64(0), Val: recType})
	item = append(item, m...)
	raw, err := core.WriteCBORSequenceItem(cw.zenc, item)
	if err != nil {
		return err
	}
	if recType != capRecTrailer {
		_, _ = cw.hasher.Write(raw)
		cw.counts[recType]++
	}
	return nil
}

func (cw *StreamCapsuleWriter) WriteHeader(h CapsuleHeader) error {
	if cw.hdrDone {
		return errors.New("memrepo: capsule header already written")
	}
	if h.Format == "" {
		h.Format = CapsuleFormatVersion
	}
	m := cbor.Map{
		{Key: uint64(1), Val: h.Format},
		{Key: uint64(2), Val: h.Repo},
		{Key: uint64(3), Val: h.Branch},
		{Key: uint64(6), Val: h.OwnerPub},
		{Key: uint64(9), Val: h.ExportedAt},
		{Key: uint64(10), Val: h.BaseAnchorN},
	}
	if h.GenesisPub != "" {
		m = append(m, cbor.Pair{Key: uint64(4), Val: h.GenesisPub})
	}
	if h.GenesisShardID != "" {
		m = append(m, cbor.Pair{Key: uint64(5), Val: h.GenesisShardID})
	}
	if h.BaseCommit != "" {
		m = append(m, cbor.Pair{Key: uint64(7), Val: h.BaseCommit})
	}
	if h.HeadCommit != "" {
		m = append(m, cbor.Pair{Key: uint64(8), Val: h.HeadCommit})
	}
	if h.BaseAnchorTxID != "" {
		m = append(m, cbor.Pair{Key: uint64(11), Val: h.BaseAnchorTxID})
	}
	if len(h.BaseStateQ) > 0 {
		m = append(m, cbor.Pair{Key: uint64(12), Val: h.BaseStateQ})
	}
	if len(h.BaseMmrRoot) > 0 {
		m = append(m, cbor.Pair{Key: uint64(13), Val: h.BaseMmrRoot})
	}
	if h.SelfContained {
		m = append(m, cbor.Pair{Key: uint64(14), Val: true})
	}
	var err error
	m, err = core.AppendExtraPairs(m, capHeaderKnownKeys, h.Extra)
	if err != nil {
		return err
	}
	cw.hdrDone = true
	return cw.writeRecord(capRecHeader, m)
}

// WriteBlob writes a BlobObject's metadata and optionally streams payload chunks <= 1 MiB.
func (cw *StreamCapsuleWriter) WriteBlob(b BlobObject, payload io.Reader) error {
	meta := b
	var inlineBytes []byte
	if !b.Redacted && !b.Encrypted && len(b.Inline) > 0 && payload == nil {
		inlineBytes = b.Inline
		meta.Inline = nil // emit payload via TypeBlobChunk (zero duplication, uniform streaming)
	}
	rawMeta, err := EncodeBlobObjectCBOR(meta)
	if err != nil {
		return err
	}
	if err := cw.writeRecord(capRecBlobMeta, cbor.Map{{Key: uint64(1), Val: rawMeta}}); err != nil {
		return err
	}
	if b.Redacted || b.Encrypted {
		return nil
	}
	if payload == nil && len(inlineBytes) > 0 {
		payload = bytes.NewReader(inlineBytes)
	}
	if payload == nil && cw.opts.SelfContained && b.ChirpURL != "" && cw.opts.Objects != nil {
		if resolved, rErr := bstack.ResolveChirp(cw.opts.Objects, b.ChirpURL); rErr == nil && len(resolved) > 0 {
			payload = bytes.NewReader(resolved)
		}
	}
	if payload == nil {
		return nil
	}
	buf := make([]byte, CapsuleBlobChunkSize)
	var chunkIdx uint64
	for {
		n, rErr := io.ReadFull(payload, buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if err := cw.writeRecord(capRecBlobChunk, cbor.Map{
				{Key: uint64(1), Val: b.BlobHash},
				{Key: uint64(2), Val: chunkIdx},
				{Key: uint64(3), Val: chunk},
			}); err != nil {
				return err
			}
			chunkIdx++
		}
		if errors.Is(rErr, io.EOF) || errors.Is(rErr, io.ErrUnexpectedEOF) {
			break
		}
		if rErr != nil {
			return rErr
		}
	}
	return nil
}

func (cw *StreamCapsuleWriter) WriteTree(tr TreeObject) error {
	raw, err := EncodeTreeObjectCBOR(tr)
	if err != nil {
		return err
	}
	return cw.writeRecord(capRecTree, cbor.Map{{Key: uint64(1), Val: raw}})
}

func (cw *StreamCapsuleWriter) WriteCommit(c CommitObject) error {
	raw, err := encodeCommitObjectCBOR(c)
	if err != nil {
		return err
	}
	return cw.writeRecord(capRecCommit, cbor.Map{{Key: uint64(1), Val: raw}})
}

func (cw *StreamCapsuleWriter) WriteTransition(tr core.TransitionRecord) error {
	raw, err := core.EncodeTransitionCBOR(tr)
	if err != nil {
		return err
	}
	return cw.writeRecord(capRecTransition, cbor.Map{{Key: uint64(1), Val: raw}})
}

func (cw *StreamCapsuleWriter) WriteAnchor(a store.BatchAnchor, consistency *core.MMRConsistencyProof) error {
	m := cbor.Map{
		{Key: uint64(1), Val: a.Shard},
		{Key: uint64(2), Val: a.ShardID},
		{Key: uint64(3), Val: a.N},
		{Key: uint64(4), Val: int64(a.Size)},
		{Key: uint64(5), Val: a.RootHex},
		{Key: uint64(6), Val: a.ChainedHex},
		{Key: uint64(9), Val: a.TxID},
		{Key: uint64(11), Val: a.Settled},
		{Key: uint64(12), Val: a.BlockHeight},
		{Key: uint64(13), Val: a.CreatedAt},
	}
	if a.StateHash != "" {
		m = append(m, cbor.Pair{Key: uint64(7), Val: a.StateHash})
	}
	if a.MmrRoot != "" {
		m = append(m, cbor.Pair{Key: uint64(8), Val: a.MmrRoot})
	}
	if len(a.Beef) > 0 {
		m = append(m, cbor.Pair{Key: uint64(10), Val: a.Beef})
	}
	if consistency != nil {
		m = append(m, cbor.Pair{Key: uint64(14), Val: mmrConsistencyToCBOR(*consistency)})
	}
	return cw.writeRecord(capRecAnchor, m)
}

// WriteFragmentMeta writes fragment epistemic/Merkle metadata WITHOUT duplicating Data/Ciphertext/KeyedMeta
// whenever blobBacked is true (the payload lives in TypeBlobMeta/TypeBlobChunk).
func (cw *StreamCapsuleWriter) WriteFragmentMeta(f store.FragmentRecord, blobHash string, blobBacked bool) error {
	m := cbor.Map{
		{Key: uint64(1), Val: f.Shard},
		{Key: uint64(2), Val: f.Seq},
		{Key: uint64(3), Val: f.HDP},
		{Key: uint64(4), Val: f.Leaf},
		{Key: uint64(7), Val: f.Provenance},
		{Key: uint64(8), Val: int64(f.Level)},
		{Key: uint64(9), Val: f.Class},
		{Key: uint64(14), Val: f.BatchN},
		{Key: uint64(15), Val: int64(f.LeafIndex)},
		{Key: uint64(20), Val: f.CreatedAt},
	}
	if f.PrevLeaf != "" {
		m = append(m, cbor.Pair{Key: uint64(5), Val: f.PrevLeaf})
	}
	if f.ChainHash != "" {
		m = append(m, cbor.Pair{Key: uint64(6), Val: f.ChainHash})
	}
	if f.BorgGroup != "" {
		m = append(m, cbor.Pair{Key: uint64(10), Val: f.BorgGroup})
	}
	if f.BorgEpoch != 0 {
		m = append(m, cbor.Pair{Key: uint64(11), Val: f.BorgEpoch})
	}
	if f.LatticeEpoch > 0 {
		m = append(m, cbor.Pair{Key: uint64(12), Val: f.LatticeEpoch})
	}
	if f.Public {
		m = append(m, cbor.Pair{Key: uint64(13), Val: true})
	}
	if f.AnchorTxID != "" {
		m = append(m, cbor.Pair{Key: uint64(16), Val: f.AnchorTxID})
	}
	if f.Supersedes != 0 {
		m = append(m, cbor.Pair{Key: uint64(17), Val: f.Supersedes})
	}
	if f.Forgotten {
		m = append(m, cbor.Pair{Key: uint64(18), Val: true})
	}
	if f.Tainted {
		m = append(m, cbor.Pair{Key: uint64(19), Val: true})
	}
	if f.Path != "" {
		m = append(m, cbor.Pair{Key: uint64(21), Val: f.Path})
	}
	if f.MediaType != "" {
		m = append(m, cbor.Pair{Key: uint64(22), Val: f.MediaType})
	}
	if f.ChirpURL != "" {
		m = append(m, cbor.Pair{Key: uint64(23), Val: f.ChirpURL})
	}
	if f.ContentSHA256 != "" {
		m = append(m, cbor.Pair{Key: uint64(24), Val: f.ContentSHA256})
	}
	if f.MPERoot != "" {
		m = append(m, cbor.Pair{Key: uint64(25), Val: f.MPERoot})
	}
	if f.CommitHash != "" {
		m = append(m, cbor.Pair{Key: uint64(26), Val: f.CommitHash})
	}
	if f.TransitionHash != "" {
		m = append(m, cbor.Pair{Key: uint64(27), Val: f.TransitionHash})
	}
	if f.Distilled {
		m = append(m, cbor.Pair{Key: uint64(28), Val: true})
	}
	if f.OntologyID != "" {
		m = append(m, cbor.Pair{Key: uint64(29), Val: f.OntologyID})
	}
	if len(f.TriplesJSON) > 0 {
		m = append(m, cbor.Pair{Key: uint64(30), Val: f.TriplesJSON})
	}
	if blobHash != "" {
		m = append(m, cbor.Pair{Key: uint64(31), Val: blobHash})
	}
	if !blobBacked {
		if len(f.Data) > 0 {
			m = append(m, cbor.Pair{Key: uint64(32), Val: f.Data})
		}
		if len(f.Ciphertext) > 0 {
			m = append(m, cbor.Pair{Key: uint64(33), Val: f.Ciphertext})
		}
		if len(f.KeyedMeta) > 0 {
			m = append(m, cbor.Pair{Key: uint64(34), Val: f.KeyedMeta})
		}
	} else if len(f.Ciphertext) > 0 {
		// Encrypted fragments carry ciphertext needed for ComputeLeaf H(E) if not reconstructible from KeyedMeta.
		m = append(m, cbor.Pair{Key: uint64(33), Val: f.Ciphertext})
	}
	return cw.writeRecord(capRecFragmentMeta, m)
}

func (cw *StreamCapsuleWriter) WriteBatchProof(batchN int64, sp SubsetDisclosureProof) error {
	return cw.writeRecord(capRecBatchProof, cbor.Map{
		{Key: uint64(1), Val: batchN},
		{Key: uint64(2), Val: subsetProofToCBOR(sp)},
	})
}

// Close writes the final authenticated CapsuleTrailer, flushes zstd, and seals any outer AEAD stream.
func (cw *StreamCapsuleWriter) Close() (*CapsuleTrailer, error) {
	if cw.closed {
		return nil, nil
	}
	// DSHA-256 over all preceding uncompressed CBOR sequence item bytes.
	first := cw.hasher.Sum(nil)
	streamDigest := core.SHA256(first)
	tr := CapsuleTrailer{
		StreamDigest: streamDigest,
		RecordCounts: make(map[uint64]uint64, len(cw.counts)),
	}
	for k, v := range cw.counts {
		tr.RecordCounts[k] = v
	}
	if cw.opts.SignDigest != nil && cw.opts.SignerPub != "" {
		tr.SignerPub = cw.opts.SignerPub
		dig, err := trailerSignDigest(tr)
		if err != nil {
			return nil, err
		}
		sig, err := cw.opts.SignDigest(dig)
		if err != nil {
			return nil, fmt.Errorf("memrepo: sign capsule trailer: %w", err)
		}
		tr.Signature = sig
	}
	if err := cw.writeRecord(capRecTrailer, trailerToCBOR(tr)); err != nil {
		return nil, err
	}
	cw.closed = true
	if err := cw.zenc.Close(); err != nil {
		return nil, err
	}
	if cw.aeadW != nil {
		if err := cw.aeadW.Close(); err != nil {
			return nil, err
		}
	}
	return &tr, nil
}

// WriteCapsuleStream streams cap cleanly to w in deterministic topological order.
func WriteCapsuleStream(w io.Writer, cap *MemoryCapsule, opts CapsuleEncodeOpts) error {
	if cap == nil {
		return errors.New("memrepo: nil capsule")
	}
	cw, err := NewCapsuleWriter(w, opts)
	if err != nil {
		return err
	}
	baseAnchorN := cap.BaseAnchorN
	if baseAnchorN == 0 && cap.BaseAnchorTxID == "" && len(cap.BaseMmrRoot) == 0 && (len(cap.Anchors) == 0 || cap.Anchors[0].N == 0) {
		baseAnchorN = -1
	}
	hdr := CapsuleHeader{
		Format:         cap.Format,
		Repo:           cap.Repo,
		Branch:         cap.Branch,
		GenesisPub:     cap.GenesisPub,
		GenesisShardID: cap.GenesisShardID,
		OwnerPub:       cap.OwnerPub,
		BaseCommit:     cap.BaseCommit,
		HeadCommit:     cap.HeadCommit,
		BaseAnchorN:    baseAnchorN,
		BaseAnchorTxID: cap.BaseAnchorTxID,
		BaseStateQ:     cap.BaseStateQ,
		BaseMmrRoot:    cap.BaseMmrRoot,
		ExportedAt:     cap.ExportedAt,
		SelfContained:  cap.SelfContained || opts.SelfContained,
		Extra:          cap.HeaderExtra,
	}
	if err := cw.WriteHeader(hdr); err != nil {
		return err
	}
	// Deterministic sorted order for blobs, trees, and batch proofs.
	blobKeys := make([]string, 0, len(cap.Blobs))
	blobByContent := make(map[string]string, len(cap.Blobs))
	for k, b := range cap.Blobs {
		blobKeys = append(blobKeys, k)
		if !b.Redacted && b.ContentSHA256 != "" {
			blobByContent[strings.ToLower(b.ContentSHA256)] = k
		}
	}
	sort.Strings(blobKeys)
	for _, k := range blobKeys {
		if err := cw.WriteBlob(cap.Blobs[k], nil); err != nil {
			return err
		}
	}
	treeKeys := make([]string, 0, len(cap.Trees))
	for k := range cap.Trees {
		treeKeys = append(treeKeys, k)
	}
	sort.Strings(treeKeys)
	for _, k := range treeKeys {
		if err := cw.WriteTree(cap.Trees[k]); err != nil {
			return err
		}
	}
	for _, c := range cap.Commits {
		if err := cw.WriteCommit(c); err != nil {
			return err
		}
	}
	for _, tr := range cap.Transitions {
		if err := cw.WriteTransition(tr); err != nil {
			return err
		}
	}
	for i, a := range cap.Anchors {
		var cons *core.MMRConsistencyProof
		if i == 0 {
			cons = cap.AnchorConsistency
		}
		if err := cw.WriteAnchor(a, cons); err != nil {
			return err
		}
	}
	for _, f := range cap.Fragments {
		bh := f.BlobHash
		if bh == "" && f.ContentSHA256 != "" {
			bh = blobByContent[strings.ToLower(f.ContentSHA256)]
		}
		blobBacked := false
		if bh != "" {
			if b, ok := cap.Blobs[bh]; ok && !b.Redacted {
				if !b.Encrypted && len(b.Inline) > 0 && bytes.Equal(b.Inline, f.Data) {
					blobBacked = true
				} else if b.Encrypted && len(b.KeyedMeta) > 0 && len(f.Data) == 0 && bytes.Equal(b.KeyedMeta, f.KeyedMeta) {
					blobBacked = true
				}
			}
		}
		if err := cw.WriteFragmentMeta(f, bh, blobBacked); err != nil {
			return err
		}
	}
	proofNs := make([]int64, 0, len(cap.BatchProofs))
	for n := range cap.BatchProofs {
		proofNs = append(proofNs, n)
	}
	sort.Slice(proofNs, func(i, j int) bool { return proofNs[i] < proofNs[j] })
	for _, n := range proofNs {
		if sp := cap.BatchProofs[n]; sp != nil {
			if err := cw.WriteBatchProof(n, *sp); err != nil {
				return err
			}
		}
	}
	tr, err := cw.Close()
	if err != nil {
		return err
	}
	cap.Trailer = tr
	return nil
}

// ReadCapsuleStream decodes a streaming .nkt capsule from r, validating NKTCAP1 framing,
// optional outer AEAD decryption, zstd decompression, RFC 8742 CBOR Sequence items, and mandatory Trailer digest.
func ReadCapsuleStream(r io.Reader, opts CapsuleVerifyOpts) (*MemoryCapsule, error) {
	cap, _, err := readCapsuleStreamInternal(r, opts)
	return cap, err
}

var (
	capsuleBufioReaderPool = sync.Pool{
		New: func() any { return bufio.NewReaderSize(nil, 32<<10) },
	}
	capsuleZstdDecoderPool = sync.Pool{
		New: func() any {
			dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
			if err != nil {
				return nil
			}
			return dec
		},
	}
)

func readCapsuleStreamInternal(r io.Reader, opts CapsuleVerifyOpts) (*MemoryCapsule, bool, error) {
	br := capsuleBufioReaderPool.Get().(*bufio.Reader)
	br.Reset(r)
	defer func() {
		br.Reset(nil)
		capsuleBufioReaderPool.Put(br)
	}()
	var magic [8]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return nil, false, errors.New("memrepo: invalid capsule header (expected NKTCAP1 binary .nkt)")
	}
	if string(magic[:]) != CapsuleMagicHeader {
		return nil, false, errors.New("memrepo: invalid capsule header (expected NKTCAP1 binary .nkt)")
	}
	modeByte, err := br.ReadByte()
	if err != nil {
		return nil, false, fmt.Errorf("memrepo: missing capsule mode byte: %w", err)
	}

	var zSrc io.Reader = br
	encrypted := false
	switch modeByte {
	case CapsuleModePlainZstd:
		// plain zstd stream follows directly
	case CapsuleModeAEAD:
		encrypted = true
		if opts.UnwrapDEK == nil {
			return nil, true, errors.New("memrepo: encrypted .nkt capsule requires UnwrapDEK")
		}
		var lenBuf [4]byte
		if _, err := io.ReadFull(br, lenBuf[:]); err != nil {
			return nil, true, fmt.Errorf("memrepo: read envelope header len: %w", err)
		}
		hdrLen := binary.BigEndian.Uint32(lenBuf[:])
		if hdrLen == 0 || hdrLen > maxEnvelopeHeaderBytes {
			return nil, true, fmt.Errorf("memrepo: invalid envelope header length %d", hdrLen)
		}
		hdrBytes := make([]byte, hdrLen)
		if _, err := io.ReadFull(br, hdrBytes); err != nil {
			return nil, true, fmt.Errorf("memrepo: read envelope header: %w", err)
		}
		hdr, err := decodeEnvelopeHeaderCBOR(hdrBytes)
		if err != nil {
			return nil, true, err
		}
		if len(hdr.Salt) != 4 {
			return nil, true, errors.New("memrepo: envelope header salt must be 4 bytes")
		}
		dek, err := opts.UnwrapDEK(hdr)
		if err != nil {
			return nil, true, fmt.Errorf("memrepo: unwrap capsule DEK: %w", err)
		}
		defer func() {
			for i := range dek {
				dek[i] = 0
			}
		}()
		var salt [4]byte
		copy(salt[:], hdr.Salt)
		ar, err := newAEADChunkReader(br, dek, salt, hdrBytes)
		if err != nil {
			return nil, true, err
		}
		zSrc = ar
	default:
		return nil, false, fmt.Errorf("memrepo: unsupported capsule mode byte 0x%02x", modeByte)
	}

	zdec, _ := capsuleZstdDecoderPool.Get().(*zstd.Decoder)
	if zdec == nil {
		var zErr error
		zdec, zErr = zstd.NewReader(zSrc, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
		if zErr != nil {
			return nil, encrypted, fmt.Errorf("memrepo: open zstd capsule: %w", zErr)
		}
	} else if rErr := zdec.Reset(zSrc); rErr != nil {
		zdec.Close()
		var zErr error
		zdec, zErr = zstd.NewReader(zSrc, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
		if zErr != nil {
			return nil, encrypted, fmt.Errorf("memrepo: open zstd capsule: %w", zErr)
		}
	}
	defer func() {
		_ = zdec.Reset(nil)
		capsuleZstdDecoderPool.Put(zdec)
	}()

	sr := core.NewCBORSeqReader(zdec, MaxCapsuleRecordBytes)
	hasher := sha256.New()
	counts := make(map[uint64]uint64)

	cap := &MemoryCapsule{
		BaseAnchorN: -1,
		Trees:       make(map[string]TreeObject),
		Blobs:       make(map[string]BlobObject),
		BatchProofs: make(map[int64]*SubsetDisclosureProof),
	}
	blobChunks := make(map[string]*bytes.Buffer)
	blobNextIdx := make(map[string]uint64)
	var totalBytes int64
	gotHeader := false
	gotTrailer := false

	for {
		rawItem, val, err := sr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, encrypted, fmt.Errorf("memrepo: read capsule record: %w", err)
		}
		if gotTrailer {
			return nil, encrypted, errors.New("memrepo: unexpected trailing record after capsule trailer")
		}
		totalBytes += int64(len(rawItem))
		if totalBytes > MaxCapsuleDecodedBytes*4 {
			return nil, encrypted, errors.New("memrepo: capsule stream exceeds hard safety cap")
		}
		m, err := core.CBORMapToUintMap(val)
		if err != nil {
			return nil, encrypted, fmt.Errorf("memrepo: capsule record map: %w", err)
		}
		recType, err := core.CBORUint(m, 0)
		if err != nil || recType == 0 {
			return nil, encrypted, errors.New("memrepo: capsule record missing type")
		}
		if !gotHeader && recType != capRecHeader {
			return nil, encrypted, errors.New("memrepo: first capsule record must be Header")
		}
		var recExtra []cbor.Pair
		if known := capsuleRecordKnownKeys(recType); known != nil {
			m, recExtra, err = core.SplitCBORMap(val, known)
			if err != nil {
				return nil, encrypted, fmt.Errorf("memrepo: capsule record type %d: %w", recType, err)
			}
		}
		if recType != capRecTrailer {
			_, _ = hasher.Write(rawItem)
			counts[recType]++
		}

		switch recType {
		case capRecHeader:
			if gotHeader {
				return nil, encrypted, errors.New("memrepo: duplicate capsule header")
			}
			h, hErr := decodeHeaderMap(m)
			if hErr != nil {
				return nil, encrypted, hErr
			}
			h.Extra = recExtra
			if h.Format != CapsuleFormatVersion {
				return nil, encrypted, fmt.Errorf("memrepo: unexpected capsule format %q", h.Format)
			}
			cap.Format = h.Format
			cap.Repo = h.Repo
			cap.Branch = h.Branch
			cap.GenesisPub = h.GenesisPub
			cap.GenesisShardID = h.GenesisShardID
			cap.OwnerPub = h.OwnerPub
			cap.BaseCommit = h.BaseCommit
			cap.HeadCommit = h.HeadCommit
			cap.BaseAnchorN = h.BaseAnchorN
			cap.BaseAnchorTxID = h.BaseAnchorTxID
			cap.BaseStateQ = h.BaseStateQ
			cap.BaseMmrRoot = h.BaseMmrRoot
			cap.ExportedAt = h.ExportedAt
			cap.SelfContained = h.SelfContained
			cap.HeaderExtra = recExtra
			gotHeader = true

		case capRecBlobMeta:
			rawMeta, bErr := core.CBORBytes(m, 1)
			if bErr != nil || len(rawMeta) == 0 {
				return nil, encrypted, errors.New("memrepo: invalid blob meta record")
			}
			b, bErr := DecodeBlobObjectCBOR(rawMeta)
			if bErr != nil {
				return nil, encrypted, fmt.Errorf("memrepo: decode blob meta: %w", bErr)
			}
			cap.Blobs[b.BlobHash] = b

		case capRecBlobChunk:
			bh, err := core.CBORText(m, 1)
			if err != nil || bh == "" {
				return nil, encrypted, errors.New("memrepo: blob chunk missing blobHash")
			}
			idx, err := core.CBORUint(m, 2)
			if err != nil {
				return nil, encrypted, err
			}
			data, err := core.CBORBytes(m, 3)
			if err != nil {
				return nil, encrypted, err
			}
			if len(data) > CapsuleBlobChunkSize {
				return nil, encrypted, errors.New("memrepo: blob chunk exceeds max chunk size")
			}
			if idx != blobNextIdx[bh] {
				return nil, encrypted, fmt.Errorf("memrepo: out-of-order blob chunk for %s (got %d want %d)", bh, idx, blobNextIdx[bh])
			}
			blobNextIdx[bh] = idx + 1
			buf := blobChunks[bh]
			if buf == nil {
				buf = &bytes.Buffer{}
				blobChunks[bh] = buf
			}
			buf.Write(data)

		case capRecTree:
			rawTree, err := core.CBORBytes(m, 1)
			if err != nil || len(rawTree) == 0 {
				return nil, encrypted, errors.New("memrepo: invalid tree record")
			}
			tr, err := DecodeTreeObjectCBOR(rawTree)
			if err != nil {
				return nil, encrypted, fmt.Errorf("memrepo: decode tree: %w", err)
			}
			cap.Trees[tr.TreeHash] = tr

		case capRecCommit:
			rawCommit, err := core.CBORBytes(m, 1)
			if err != nil || len(rawCommit) == 0 {
				return nil, encrypted, errors.New("memrepo: invalid commit record")
			}
			c, err := decodeCommitObjectCBOR(rawCommit)
			if err != nil {
				return nil, encrypted, fmt.Errorf("memrepo: decode commit: %w", err)
			}
			cap.Commits = append(cap.Commits, c)

		case capRecTransition:
			rawTr, err := core.CBORBytes(m, 1)
			if err != nil || len(rawTr) == 0 {
				return nil, encrypted, errors.New("memrepo: invalid transition record")
			}
			tr, err := core.DecodeTransitionCBOR(rawTr)
			if err != nil {
				return nil, encrypted, fmt.Errorf("memrepo: decode transition: %w", err)
			}
			cap.Transitions = append(cap.Transitions, tr)

		case capRecAnchor:
			anc, cons, err := decodeAnchorMap(m)
			if err != nil {
				return nil, encrypted, err
			}
			if cons != nil && cap.AnchorConsistency == nil {
				cap.AnchorConsistency = cons
			}
			cap.Anchors = append(cap.Anchors, anc)

		case capRecFragmentMeta:
			f, err := decodeFragmentMetaMap(m)
			if err != nil {
				return nil, encrypted, err
			}
			cap.Fragments = append(cap.Fragments, f)

		case capRecBatchProof:
			bn, err := core.CBORInt(m, 1)
			if err != nil {
				return nil, encrypted, err
			}
			sp, err := decodeSubsetProofCBOR(m[2])
			if err != nil {
				return nil, encrypted, fmt.Errorf("memrepo: decode batch proof: %w", err)
			}
			cap.BatchProofs[bn] = &sp

		case capRecTrailer:
			tr, err := decodeTrailerMap(m)
			if err != nil {
				return nil, encrypted, err
			}
			tr.Extra = recExtra
			wantDigest := core.SHA256(hasher.Sum(nil))
			if !core.EqualBytes(wantDigest, tr.StreamDigest) {
				return nil, encrypted, errors.New("memrepo: capsule trailer streamDigest mismatch (corrupted or tampered)")
			}
			if len(tr.RecordCounts) != len(counts) {
				return nil, encrypted, errors.New("memrepo: capsule trailer recordCounts mismatch")
			}
			for k, wantCnt := range counts {
				if tr.RecordCounts[k] != wantCnt {
					return nil, encrypted, fmt.Errorf("memrepo: capsule trailer count mismatch for type %d", k)
				}
			}
			cap.Trailer = &tr
			gotTrailer = true

		default:
			if recType%2 == 0 {
				return nil, encrypted, fmt.Errorf("memrepo: unknown critical capsule record type %d", recType)
			}
		}
	}

	if !gotHeader {
		return nil, encrypted, errors.New("memrepo: capsule missing Header record")
	}
	if !gotTrailer {
		return nil, encrypted, errors.New("memrepo: capsule truncated (missing Trailer record)")
	}

	// Reattach streamed blob chunks to BlobObjects (and rehydrate deduplicated FragmentRecord payloads).
	for bh, buf := range blobChunks {
		b, ok := cap.Blobs[bh]
		if !ok {
			return nil, encrypted, fmt.Errorf("memrepo: orphan blob chunks for unknown blob %s", bh)
		}
		b.Inline = buf.Bytes()
		cap.Blobs[bh] = b
	}
	blobByContent := make(map[string]BlobObject, len(cap.Blobs))
	for _, b := range cap.Blobs {
		if b.ContentSHA256 != "" {
			blobByContent[strings.ToLower(b.ContentSHA256)] = b
		}
	}
	for i := range cap.Fragments {
		f := &cap.Fragments[i]
		if f.Forgotten {
			continue
		}
		var b BlobObject
		var hasB bool
		if f.BlobHash != "" {
			b, hasB = cap.Blobs[f.BlobHash]
		}
		if !hasB && f.ContentSHA256 != "" {
			b, hasB = blobByContent[strings.ToLower(f.ContentSHA256)]
			if hasB {
				f.BlobHash = b.BlobHash
			}
		}
		if hasB && !b.Redacted {
			if !b.Encrypted && len(f.Data) == 0 && len(b.Inline) > 0 {
				f.Data = b.Inline
			}
			if b.Encrypted && len(f.KeyedMeta) == 0 && len(b.KeyedMeta) > 0 {
				f.KeyedMeta = b.KeyedMeta
			}
		}
	}
	if len(cap.BatchProofs) == 0 {
		cap.BatchProofs = nil
	}
	return cap, encrypted, nil
}

// --- Internal CBOR Struct Codecs for Capsule Records ---

var (
	capHeaderKnownKeys           = core.KnownKeyRange(0, 14)
	capBlobMetaKnownKeys         = core.KnownKeyRange(0, 1)
	capBlobChunkKnownKeys        = core.KnownKeyRange(0, 3)
	capSinglePayloadRecKnownKeys = core.KnownKeyRange(0, 1)
	capAnchorKnownKeys           = core.KnownKeyRange(0, 14)
	capFragmentMetaKnownKeys     = core.KnownKeyRange(0, 34)
	capBatchProofKnownKeys       = core.KnownKeyRange(0, 2)
	capTrailerKnownKeys          = core.KnownKeyRange(0, 4)
)

func capsuleRecordKnownKeys(recType uint64) map[uint64]struct{} {
	switch recType {
	case capRecHeader:
		return capHeaderKnownKeys
	case capRecBlobMeta:
		return capBlobMetaKnownKeys
	case capRecBlobChunk:
		return capBlobChunkKnownKeys
	case capRecTree, capRecCommit, capRecTransition:
		return capSinglePayloadRecKnownKeys
	case capRecAnchor:
		return capAnchorKnownKeys
	case capRecFragmentMeta:
		return capFragmentMetaKnownKeys
	case capRecBatchProof:
		return capBatchProofKnownKeys
	case capRecTrailer:
		return capTrailerKnownKeys
	default:
		return nil
	}
}

func encodeEnvelopeHeaderCBOR(h CapsuleEnvelopeHeader) ([]byte, error) {
	m := cbor.Map{
		{Key: uint64(1), Val: h.Version},
		{Key: uint64(2), Val: h.KDF},
		{Key: uint64(3), Val: h.Cipher},
		{Key: uint64(4), Val: h.Compression},
		{Key: uint64(5), Val: h.Salt},
		{Key: uint64(6), Val: h.WrappedDEK},
	}
	if h.RecipientPub != "" {
		m = append(m, cbor.Pair{Key: uint64(7), Val: h.RecipientPub})
	}
	var err error
	m, err = core.AppendExtraPairs(m, envelopeHdrKnownKeys, h.Extra)
	if err != nil {
		return nil, err
	}
	return core.EncodeCBOR(m)
}

var envelopeHdrKnownKeys = core.KnownKeyRange(1, 7)

func decodeEnvelopeHeaderCBOR(raw []byte) (CapsuleEnvelopeHeader, error) {
	v, err := core.DecodeCBOR(raw)
	if err != nil {
		return CapsuleEnvelopeHeader{}, fmt.Errorf("memrepo: decode envelope header: %w", err)
	}
	m, extra, err := core.SplitCBORMap(v, envelopeHdrKnownKeys)
	if err != nil {
		return CapsuleEnvelopeHeader{}, err
	}
	var h CapsuleEnvelopeHeader
	h.Extra = extra
	if h.Version, err = core.CBORUint(m, 1); err != nil {
		return CapsuleEnvelopeHeader{}, err
	}
	if h.KDF, err = core.CBORText(m, 2); err != nil {
		return CapsuleEnvelopeHeader{}, err
	}
	if h.Cipher, err = core.CBORText(m, 3); err != nil {
		return CapsuleEnvelopeHeader{}, err
	}
	if h.Compression, err = core.CBORText(m, 4); err != nil {
		return CapsuleEnvelopeHeader{}, err
	}
	if h.Salt, err = core.CBORBytes(m, 5); err != nil {
		return CapsuleEnvelopeHeader{}, err
	}
	if h.WrappedDEK, err = core.CBORBytes(m, 6); err != nil {
		return CapsuleEnvelopeHeader{}, err
	}
	if h.RecipientPub, err = core.CBORText(m, 7); err != nil {
		return CapsuleEnvelopeHeader{}, err
	}
	return h, nil
}

func decodeHeaderMap(m map[uint64]cbor.Value) (CapsuleHeader, error) {
	var h CapsuleHeader
	var err error
	if h.Format, err = core.CBORText(m, 1); err != nil {
		return h, err
	}
	if h.Repo, err = core.CBORText(m, 2); err != nil {
		return h, err
	}
	if h.Branch, err = core.CBORText(m, 3); err != nil {
		return h, err
	}
	if h.GenesisPub, err = core.CBORText(m, 4); err != nil {
		return h, err
	}
	if h.GenesisShardID, err = core.CBORText(m, 5); err != nil {
		return h, err
	}
	if h.OwnerPub, err = core.CBORText(m, 6); err != nil {
		return h, err
	}
	if h.BaseCommit, err = core.CBORText(m, 7); err != nil {
		return h, err
	}
	if h.HeadCommit, err = core.CBORText(m, 8); err != nil {
		return h, err
	}
	if h.ExportedAt, err = core.CBORInt(m, 9); err != nil {
		return h, err
	}
	if _, ok := m[10]; ok {
		if h.BaseAnchorN, err = core.CBORInt(m, 10); err != nil {
			return h, err
		}
	} else {
		h.BaseAnchorN = -1
	}
	if h.BaseAnchorTxID, err = core.CBORText(m, 11); err != nil {
		return h, err
	}
	if h.BaseStateQ, err = core.CBORBytes(m, 12); err != nil {
		return h, err
	}
	if h.BaseMmrRoot, err = core.CBORBytes(m, 13); err != nil {
		return h, err
	}
	if h.SelfContained, err = core.CBORBool(m, 14); err != nil {
		return h, err
	}
	return h, nil
}

func encodeCommitObjectCBOR(c CommitObject) ([]byte, error) {
	m := commitSignableCBOR(c)
	m = append(m,
		cbor.Pair{Key: uint64(14), Val: c.Signature},
		cbor.Pair{Key: uint64(15), Val: c.CommitHash},
		cbor.Pair{Key: uint64(16), Val: c.Seq},
		cbor.Pair{Key: uint64(18), Val: c.AnchorN},
	)
	if c.AnchorTxID != "" {
		m = append(m, cbor.Pair{Key: uint64(17), Val: c.AnchorTxID})
	}
	if c.FragmentLeaf != "" {
		m = append(m, cbor.Pair{Key: uint64(19), Val: c.FragmentLeaf})
	}
	var err error
	m, err = core.AppendExtraPairs(m, commitObjKnownKeys, c.Extra)
	if err != nil {
		return nil, err
	}
	return core.EncodeCBOR(m)
}

var commitObjKnownKeys = core.KnownKeyRange(1, 19)

func decodeCommitObjectCBOR(raw []byte) (CommitObject, error) {
	v, err := core.DecodeCBOR(raw)
	if err != nil {
		return CommitObject{}, err
	}
	m, extra, err := core.SplitCBORMap(v, commitObjKnownKeys)
	if err != nil {
		return CommitObject{}, err
	}
	var c CommitObject
	c.Extra = extra
	ver, err := core.CBORInt(m, 1)
	if err != nil {
		return CommitObject{}, err
	}
	c.Version = int(ver)
	if c.Repo, err = core.CBORText(m, 2); err != nil {
		return CommitObject{}, err
	}
	if c.Branch, err = core.CBORText(m, 3); err != nil {
		return CommitObject{}, err
	}
	if arr, err := core.CBORArray(m, 4); err != nil {
		return CommitObject{}, err
	} else if len(arr) > 0 {
		if c.Parents, err = decodeParentsCBOR(arr); err != nil {
			return CommitObject{}, err
		}
	}
	if c.TreeHash, err = core.CBORText(m, 5); err != nil {
		return CommitObject{}, err
	}
	if c.MPERoot, err = core.CBORText(m, 6); err != nil {
		return CommitObject{}, err
	}
	if c.AuthorPub, err = core.CBORText(m, 7); err != nil {
		return CommitObject{}, err
	}
	if c.ModelID, err = core.CBORText(m, 8); err != nil {
		return CommitObject{}, err
	}
	if c.PolicyCommit, err = core.CBORText(m, 9); err != nil {
		return CommitObject{}, err
	}
	if c.OntologyCommit, err = core.CBORText(m, 10); err != nil {
		return CommitObject{}, err
	}
	if c.Message, err = core.CBORText(m, 11); err != nil {
		return CommitObject{}, err
	}
	if c.Timestamp, err = core.CBORInt(m, 12); err != nil {
		return CommitObject{}, err
	}
	ck, err := core.CBORInt(m, 13)
	if err != nil {
		return CommitObject{}, err
	}
	c.CommitKind = int(ck)
	if c.Signature, err = core.CBORText(m, 14); err != nil {
		return CommitObject{}, err
	}
	if c.CommitHash, err = core.CBORText(m, 15); err != nil {
		return CommitObject{}, err
	}
	if c.Seq, err = core.CBORInt(m, 16); err != nil {
		return CommitObject{}, err
	}
	if c.AnchorTxID, err = core.CBORText(m, 17); err != nil {
		return CommitObject{}, err
	}
	if _, ok := m[18]; ok {
		if c.AnchorN, err = core.CBORInt(m, 18); err != nil {
			return CommitObject{}, err
		}
	} else {
		c.AnchorN = -1
	}
	if c.FragmentLeaf, err = core.CBORText(m, 19); err != nil {
		return CommitObject{}, err
	}
	return c, nil
}

func mmrConsistencyToCBOR(p core.MMRConsistencyProof) cbor.Map {
	return cbor.Map{
		{Key: uint64(1), Val: p.OldSize},
		{Key: uint64(2), Val: p.NewSize},
		{Key: uint64(3), Val: core.CBORStringsVal(p.OldPeaks)},
		{Key: uint64(4), Val: core.CBORStringsVal(p.RightChunks)},
	}
}

var mmrConsKnownKeys = core.KnownKeyRange(1, 4)

func decodeMMRConsistencyCBOR(v cbor.Value) (*core.MMRConsistencyProof, error) {
	m, _, err := core.SplitCBORMap(v, mmrConsKnownKeys)
	if err != nil {
		return nil, err
	}
	p := &core.MMRConsistencyProof{}
	if p.OldSize, err = core.CBORUint(m, 1); err != nil {
		return nil, err
	}
	if p.NewSize, err = core.CBORUint(m, 2); err != nil {
		return nil, err
	}
	if p.OldPeaks, err = core.CBORStrings(m, 3); err != nil {
		return nil, err
	}
	if p.RightChunks, err = core.CBORStrings(m, 4); err != nil {
		return nil, err
	}
	return p, nil
}

func decodeAnchorMap(m map[uint64]cbor.Value) (store.BatchAnchor, *core.MMRConsistencyProof, error) {
	var a store.BatchAnchor
	var err error
	if a.Shard, err = core.CBORText(m, 1); err != nil {
		return a, nil, err
	}
	if a.ShardID, err = core.CBORText(m, 2); err != nil {
		return a, nil, err
	}
	if a.N, err = core.CBORInt(m, 3); err != nil {
		return a, nil, err
	}
	sz, err := core.CBORInt(m, 4)
	if err != nil {
		return a, nil, err
	}
	a.Size = int(sz)
	if a.RootHex, err = core.CBORText(m, 5); err != nil {
		return a, nil, err
	}
	if a.ChainedHex, err = core.CBORText(m, 6); err != nil {
		return a, nil, err
	}
	if a.StateHash, err = core.CBORText(m, 7); err != nil {
		return a, nil, err
	}
	if a.MmrRoot, err = core.CBORText(m, 8); err != nil {
		return a, nil, err
	}
	if a.TxID, err = core.CBORText(m, 9); err != nil {
		return a, nil, err
	}
	if a.Beef, err = core.CBORBytes(m, 10); err != nil {
		return a, nil, err
	}
	if a.Settled, err = core.CBORBool(m, 11); err != nil {
		return a, nil, err
	}
	if a.BlockHeight, err = core.CBORInt(m, 12); err != nil {
		return a, nil, err
	}
	if a.CreatedAt, err = core.CBORInt(m, 13); err != nil {
		return a, nil, err
	}
	var cons *core.MMRConsistencyProof
	if cv, ok := m[14]; ok {
		if cons, err = decodeMMRConsistencyCBOR(cv); err != nil {
			return a, nil, fmt.Errorf("memrepo: anchor consistency proof: %w", err)
		}
	}
	return a, cons, nil
}

func decodeFragmentMetaMap(m map[uint64]cbor.Value) (store.FragmentRecord, error) {
	var f store.FragmentRecord
	var err error
	if f.Shard, err = core.CBORText(m, 1); err != nil {
		return f, err
	}
	if f.Seq, err = core.CBORInt(m, 2); err != nil {
		return f, err
	}
	if f.HDP, err = core.CBORText(m, 3); err != nil {
		return f, err
	}
	if f.Leaf, err = core.CBORText(m, 4); err != nil {
		return f, err
	}
	if f.PrevLeaf, err = core.CBORText(m, 5); err != nil {
		return f, err
	}
	if f.ChainHash, err = core.CBORText(m, 6); err != nil {
		return f, err
	}
	if f.Provenance, err = core.CBORBytes(m, 7); err != nil {
		return f, err
	}
	lvl, err := core.CBORInt(m, 8)
	if err != nil {
		return f, err
	}
	f.Level = int(lvl)
	if f.Class, err = core.CBORText(m, 9); err != nil {
		return f, err
	}
	if f.BorgGroup, err = core.CBORText(m, 10); err != nil {
		return f, err
	}
	if f.BorgEpoch, err = core.CBORInt(m, 11); err != nil {
		return f, err
	}
	if f.LatticeEpoch, err = core.CBORUint(m, 12); err != nil {
		return f, err
	}
	if f.Public, err = core.CBORBool(m, 13); err != nil {
		return f, err
	}
	if f.BatchN, err = core.CBORInt(m, 14); err != nil {
		return f, err
	}
	li, err := core.CBORInt(m, 15)
	if err != nil {
		return f, err
	}
	f.LeafIndex = int(li)
	if f.AnchorTxID, err = core.CBORText(m, 16); err != nil {
		return f, err
	}
	if f.Supersedes, err = core.CBORInt(m, 17); err != nil {
		return f, err
	}
	if f.Forgotten, err = core.CBORBool(m, 18); err != nil {
		return f, err
	}
	if f.Tainted, err = core.CBORBool(m, 19); err != nil {
		return f, err
	}
	if f.CreatedAt, err = core.CBORInt(m, 20); err != nil {
		return f, err
	}
	if f.Path, err = core.CBORText(m, 21); err != nil {
		return f, err
	}
	if f.MediaType, err = core.CBORText(m, 22); err != nil {
		return f, err
	}
	if f.ChirpURL, err = core.CBORText(m, 23); err != nil {
		return f, err
	}
	if f.ContentSHA256, err = core.CBORText(m, 24); err != nil {
		return f, err
	}
	if f.MPERoot, err = core.CBORText(m, 25); err != nil {
		return f, err
	}
	if f.CommitHash, err = core.CBORText(m, 26); err != nil {
		return f, err
	}
	if f.TransitionHash, err = core.CBORText(m, 27); err != nil {
		return f, err
	}
	if f.Distilled, err = core.CBORBool(m, 28); err != nil {
		return f, err
	}
	if f.OntologyID, err = core.CBORText(m, 29); err != nil {
		return f, err
	}
	if f.TriplesJSON, err = core.CBORBytes(m, 30); err != nil {
		return f, err
	}
	if f.BlobHash, err = core.CBORText(m, 31); err != nil {
		return f, err
	}
	if f.Data, err = core.CBORBytes(m, 32); err != nil {
		return f, err
	}
	if f.Ciphertext, err = core.CBORBytes(m, 33); err != nil {
		return f, err
	}
	if f.KeyedMeta, err = core.CBORBytes(m, 34); err != nil {
		return f, err
	}
	return f, nil
}

func subsetProofToCBOR(sp SubsetDisclosureProof) cbor.Map {
	leaves := make([]cbor.Value, len(sp.Leaves))
	for i, lp := range sp.Leaves {
		leaves[i] = cbor.Map{
			{Key: uint64(1), Val: int64(lp.Index)},
			{Key: uint64(2), Val: lp.Leaf},
			{Key: uint64(3), Val: int64(lp.ShardIdx)},
			{Key: uint64(4), Val: core.CBORStringsVal(lp.SubPath)},
		}
	}
	m := cbor.Map{
		{Key: uint64(1), Val: int64(sp.Version)},
		{Key: uint64(2), Val: int64(sp.BatchSize)},
		{Key: uint64(3), Val: sp.RootHex},
		{Key: uint64(4), Val: int64(sp.ShardLevel)},
		{Key: uint64(5), Val: leaves},
	}
	if len(sp.UpperSiblings) > 0 {
		m = append(m, cbor.Pair{Key: uint64(6), Val: core.CBORStringMapVal(sp.UpperSiblings)})
	}
	return m
}

var (
	subsetProofKnownKeys = core.KnownKeyRange(1, 6)
	leafProofKnownKeys   = core.KnownKeyRange(1, 4)
)

func decodeSubsetProofCBOR(v cbor.Value) (SubsetDisclosureProof, error) {
	m, _, err := core.SplitCBORMap(v, subsetProofKnownKeys)
	if err != nil {
		return SubsetDisclosureProof{}, err
	}
	var sp SubsetDisclosureProof
	ver, err := core.CBORInt(m, 1)
	if err != nil {
		return sp, err
	}
	sp.Version = int(ver)
	bs, err := core.CBORInt(m, 2)
	if err != nil {
		return sp, err
	}
	sp.BatchSize = int(bs)
	if sp.RootHex, err = core.CBORText(m, 3); err != nil {
		return sp, err
	}
	sl, err := core.CBORInt(m, 4)
	if err != nil {
		return sp, err
	}
	sp.ShardLevel = int(sl)
	arr, err := core.CBORArray(m, 5)
	if err != nil {
		return sp, err
	}
	sp.Leaves = make([]ShardedLeafProof, len(arr))
	for i, item := range arr {
		lm, _, lErr := core.SplitCBORMap(item, leafProofKnownKeys)
		if lErr != nil {
			return sp, lErr
		}
		var lp ShardedLeafProof
		idx, err := core.CBORInt(lm, 1)
		if err != nil {
			return sp, err
		}
		lp.Index = int(idx)
		if lp.Leaf, err = core.CBORText(lm, 2); err != nil {
			return sp, err
		}
		sidx, err := core.CBORInt(lm, 3)
		if err != nil {
			return sp, err
		}
		lp.ShardIdx = int(sidx)
		if lp.SubPath, err = core.CBORStrings(lm, 4); err != nil {
			return sp, err
		}
		sp.Leaves[i] = lp
	}
	if sp.UpperSiblings, err = core.CBORStringMap(m, 6); err != nil {
		return sp, err
	}
	return sp, nil
}

func trailerSignableCBOR(tr CapsuleTrailer) cbor.Map {
	cntMap := make(cbor.Map, 0, len(tr.RecordCounts))
	for k, v := range tr.RecordCounts {
		cntMap = append(cntMap, cbor.Pair{Key: k, Val: v})
	}
	m := cbor.Map{
		{Key: uint64(1), Val: tr.StreamDigest},
		{Key: uint64(2), Val: cntMap},
	}
	if tr.SignerPub != "" {
		m = append(m, cbor.Pair{Key: uint64(3), Val: tr.SignerPub})
	}
	return m
}

func trailerSignDigest(tr CapsuleTrailer) ([]byte, error) {
	return core.HashCBOR(core.TagCapsuleTrailerV1, trailerSignableCBOR(tr))
}

func trailerToCBOR(tr CapsuleTrailer) cbor.Map {
	m := trailerSignableCBOR(tr)
	if len(tr.Signature) > 0 {
		m = append(m, cbor.Pair{Key: uint64(4), Val: tr.Signature})
	}
	return m
}

func decodeTrailerMap(m map[uint64]cbor.Value) (CapsuleTrailer, error) {
	var tr CapsuleTrailer
	var err error
	if tr.StreamDigest, err = core.CBORBytes(m, 1); err != nil {
		return tr, err
	}
	if len(tr.StreamDigest) != 32 {
		return tr, errors.New("memrepo: trailer streamDigest must be 32 bytes")
	}
	cm, ok := m[2].(cbor.Map)
	if !ok {
		return tr, errors.New("memrepo: trailer recordCounts expected map")
	}
	tr.RecordCounts = make(map[uint64]uint64, len(cm))
	for _, p := range cm {
		k, ok1 := p.Key.(uint64)
		v, ok2 := p.Val.(uint64)
		if !ok1 || !ok2 {
			return tr, errors.New("memrepo: trailer recordCounts must map uint64->uint64")
		}
		tr.RecordCounts[k] = v
	}
	if tr.SignerPub, err = core.CBORText(m, 3); err != nil {
		return tr, err
	}
	if tr.Signature, err = core.CBORBytes(m, 4); err != nil {
		return tr, err
	}
	return tr, nil
}

func verifyTrailerSignature(tr *CapsuleTrailer, required bool) error {
	if tr == nil {
		if required {
			return errors.New("capsule missing required signed trailer")
		}
		return nil
	}
	if tr.SignerPub == "" && len(tr.Signature) == 0 {
		if required {
			return errors.New("capsule trailer is unsigned (exporter signature required)")
		}
		return nil
	}
	if tr.SignerPub == "" || len(tr.Signature) == 0 {
		return errors.New("capsule trailer has incomplete signerPub/signature")
	}
	dig, err := trailerSignDigest(*tr)
	if err != nil {
		return err
	}
	if err := core.FastVerifyStrictHex(tr.SignerPub, hex.EncodeToString(tr.Signature), dig); err != nil {
		return fmt.Errorf("capsule trailer signature verification failed: %w", err)
	}
	return nil
}
