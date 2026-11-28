package bstack

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lightwebinc/bcommon/chirp"
)

// ObjectStore is a content-addressed CHIRP (BRC-167) store compatible with lightwebinc/bstore.
type ObjectStore interface {
	PutObject(hash [32]byte, data []byte) error
	GetObject(hash [32]byte) ([]byte, error)
}

const (
	// MaxChirpObjectBytes caps any single CHIRP block/blob stored or served (8 MiB).
	MaxChirpObjectBytes = 8 << 20
	// MaxChirpTotalBytes caps any single reassembled CHIRP closure (16 MiB).
	MaxChirpTotalBytes = 16 << 20
	// MaxChirpReferences caps total DAG references followed during CHIRP verification.
	MaxChirpReferences = 512
	// DefaultChirpStoreQuotaBytes caps total disk usage of FileObjectStore (16 GiB default, overridable via NEKTAR_CHIRP_QUOTA_MB).
	DefaultChirpStoreQuotaBytes int64 = 16 << 30
)

// DefaultChirpLimits returns bounded bcommon/chirp verification limits against DAG amplification.
func DefaultChirpLimits() chirp.Limits {
	return chirp.Limits{
		MaxReferences: MaxChirpReferences,
		MaxLength:     MaxChirpTotalBytes,
	}
}

// FileObjectStore stores CHIRP objects on disk under <dir>/chirp/<ab>/<cd>/<sha256hex>
// (falling back transparently to legacy flat <dir>/chirp/<sha256hex> on reads/deletes).
type FileObjectStore struct {
	mu        sync.Mutex
	dir       string
	maxBytes  int64
	usedBytes int64
}

func NewFileObjectStore(dir string) (*FileObjectStore, error) {
	cdir := filepath.Join(dir, "chirp")
	if err := os.MkdirAll(cdir, 0o750); err != nil {
		return nil, err
	}
	var used int64
	_ = filepath.WalkDir(cdir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || strings.HasSuffix(d.Name(), ".tmp") {
			return nil
		}
		if info, iErr := d.Info(); iErr == nil && info.Mode().IsRegular() {
			used += info.Size()
		}
		return nil
	})
	quota := DefaultChirpStoreQuotaBytes
	if v := strings.TrimSpace(os.Getenv("NEKTAR_CHIRP_QUOTA_MB")); v != "" {
		var mb int64
		if _, sErr := fmt.Sscanf(v, "%d", &mb); sErr == nil && mb > 0 {
			quota = mb << 20
		}
	}
	return &FileObjectStore{dir: cdir, maxBytes: quota, usedBytes: used}, nil
}

func (s *FileObjectStore) shardedPath(hx string) (subDir, sharded, flat string) {
	flat = filepath.Join(s.dir, hx)
	if len(hx) >= 4 {
		subDir = filepath.Join(s.dir, hx[:2], hx[2:4])
		sharded = filepath.Join(subDir, hx)
		return subDir, sharded, flat
	}
	return s.dir, flat, flat
}

func (s *FileObjectStore) PutObject(hash [32]byte, data []byte) error {
	if len(data) > MaxChirpObjectBytes {
		return fmt.Errorf("chirp: object size %d exceeds max %d", len(data), MaxChirpObjectBytes)
	}
	if sha256.Sum256(data) != hash {
		return errors.New("chirp: object data sha256 mismatch")
	}
	hx := hex.EncodeToString(hash[:])
	subDir, p, flat := s.shardedPath(hx)
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	if flat != p {
		if _, err := os.Stat(flat); err == nil {
			return nil
		}
	}

	s.mu.Lock()
	if s.maxBytes > 0 && s.usedBytes+int64(len(data)) > s.maxBytes {
		used := s.usedBytes
		maxB := s.maxBytes
		s.mu.Unlock()
		return fmt.Errorf("chirp: object store quota exceeded (%d + %d > %d)", used, len(data), maxB)
	}
	s.mu.Unlock()

	if err := os.MkdirAll(subDir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(subDir, hx[:min(8, len(hx))]+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, wErr := tmp.Write(data); wErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return wErr
	}
	if cErr := tmp.Close(); cErr != nil {
		_ = os.Remove(tmpName)
		return cErr
	}
	if err := os.Rename(tmpName, p); err != nil {
		_ = os.Remove(tmpName)
		if _, stErr := os.Stat(p); stErr == nil {
			return nil
		}
		return err
	}
	s.mu.Lock()
	s.usedBytes += int64(len(data))
	s.mu.Unlock()
	return nil
}

func (s *FileObjectStore) GetObject(hash [32]byte) ([]byte, error) {
	hx := hex.EncodeToString(hash[:])
	_, p, flat := s.shardedPath(hx)
	info, err := os.Stat(p)
	if err != nil && flat != p && os.IsNotExist(err) {
		p = flat
		info, err = os.Stat(p)
	}
	if err != nil {
		return nil, err
	}
	if info.Size() > MaxChirpObjectBytes {
		return nil, fmt.Errorf("chirp: stored object size %d exceeds max %d", info.Size(), MaxChirpObjectBytes)
	}
	return os.ReadFile(p)
}

// DeleteObject removes hash from disk and updates quota accounting (used by Forget() shredding).
func (s *FileObjectStore) DeleteObject(hash [32]byte) error {
	hx := hex.EncodeToString(hash[:])
	_, p, flat := s.shardedPath(hx)
	s.mu.Lock()
	defer s.mu.Unlock()
	var freed int64
	for _, cand := range []string{p, flat} {
		if info, err := os.Stat(cand); err == nil {
			if rErr := os.Remove(cand); rErr != nil && !os.IsNotExist(rErr) {
				return rErr
			}
			freed += info.Size()
		} else if !os.IsNotExist(err) {
			return err
		}
		if cand == flat {
			break
		}
	}
	if s.usedBytes >= freed {
		s.usedBytes -= freed
	} else {
		s.usedBytes = 0
	}
	return nil
}

// MemoryObjectStore is an in-memory CHIRP object store for fast tests.
type MemoryObjectStore struct {
	mu   sync.RWMutex
	objs map[[32]byte][]byte
}

func NewMemoryObjectStore() *MemoryObjectStore {
	return &MemoryObjectStore{objs: make(map[[32]byte][]byte)}
}

func (s *MemoryObjectStore) PutObject(hash [32]byte, data []byte) error {
	if len(data) > MaxChirpObjectBytes {
		return fmt.Errorf("chirp: object size %d exceeds max %d", len(data), MaxChirpObjectBytes)
	}
	if sha256.Sum256(data) != hash {
		return errors.New("chirp: object data sha256 mismatch")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	s.objs[hash] = cp
	return nil
}

func (s *MemoryObjectStore) GetObject(hash [32]byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.objs[hash]
	if !ok {
		return nil, os.ErrNotExist
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp, nil
}

// DeleteObject removes hash from the in-memory store (used by Forget() shredding).
func (s *MemoryObjectStore) DeleteObject(hash [32]byte) error {
	s.mu.Lock()
	delete(s.objs, hash)
	s.mu.Unlock()
	return nil
}

// ObjectDeleter is an optional extension implemented by CHIRP stores that support shredding objects.
type ObjectDeleter interface {
	DeleteObject(hash [32]byte) error
}

// DeleteChirp collects all DAG hashes referenced by url and deletes them from store if it implements ObjectDeleter.
func DeleteChirp(store ObjectStore, url string) ([][32]byte, error) {
	if store == nil || strings.TrimSpace(url) == "" {
		return nil, nil
	}
	hashes, err := CollectChirpHashes(store, url)
	if err != nil {
		// Fall back to deleting at least the root hash if the closure is already partial.
		if rootHash, pErr := chirp.ParseURL(url); pErr == nil {
			hashes = [][32]byte{rootHash}
		} else {
			return nil, err
		}
	}
	if del, ok := store.(ObjectDeleter); ok {
		for _, h := range hashes {
			_ = del.DeleteObject(h)
		}
	}
	return hashes, nil
}

// StoreChirpWithHashes stores a CHIRP closure and returns every object hash in the DAG
// so callers publishing public packs/sales can allowlist exact object hashes for unauthenticated reads.
func StoreChirpWithHashes(store ObjectStore, content []byte) (string, [32]byte, [][32]byte, error) {
	built, err := chirp.Build(content, nil)
	if err != nil {
		return "", [32]byte{}, nil, fmt.Errorf("chirp: build: %w", err)
	}
	hashes := make([][32]byte, 0, len(built.Blobs)+len(built.Branches)+1)
	for _, blob := range built.Blobs {
		h := sha256.Sum256(blob)
		if err := store.PutObject(h, blob); err != nil {
			return "", [32]byte{}, nil, err
		}
		hashes = append(hashes, h)
	}
	for _, br := range built.Branches {
		h := sha256.Sum256(br)
		if err := store.PutObject(h, br); err != nil {
			return "", [32]byte{}, nil, err
		}
		hashes = append(hashes, h)
	}
	if err := store.PutObject(built.RootHash, built.Root); err != nil {
		return "", [32]byte{}, nil, err
	}
	hashes = append(hashes, built.RootHash)
	return chirp.URL(built.RootHash), built.RootHash, hashes, nil
}

// ResolveChirp resolves a chirp:// URL from store, verifies every object & closure via bcommon/chirp.Verify
// with bounded DAG limits, and reassembles the exact verified byte stream.
func ResolveChirp(store ObjectStore, url string) ([]byte, error) {
	rootHash, err := chirp.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("chirp: parse url: %w", err)
	}
	held := make(map[[32]byte][]byte)
	closure, err := chirp.Verify(rootHash, func(h [32]byte) ([]byte, error) {
		b, err := store.GetObject(h)
		if err != nil {
			return nil, err
		}
		held[h] = b
		return b, nil
	}, DefaultChirpLimits())
	if err != nil {
		return nil, fmt.Errorf("chirp: verify closure: %w", err)
	}
	var out []byte
	for _, blobRef := range closure.Blobs {
		b, ok := held[blobRef.Hash]
		if !ok {
			return nil, errors.New("chirp: missing verified blob in closure")
		}
		if len(out)+len(b) > MaxChirpTotalBytes {
			return nil, fmt.Errorf("chirp: reassembled closure exceeds max %d bytes", MaxChirpTotalBytes)
		}
		out = append(out, b...)
	}
	return out, nil
}

// CollectChirpHashes verifies a stored chirp:// closure and returns every object hash in its DAG
// so public packs/listings can restore their exact unauthenticated object allowlist on restart.
func CollectChirpHashes(store ObjectStore, url string) ([][32]byte, error) {
	rootHash, err := chirp.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("chirp: parse url: %w", err)
	}
	seen := make(map[[32]byte]struct{})
	hashes := make([][32]byte, 0, 8)
	_, err = chirp.Verify(rootHash, func(h [32]byte) ([]byte, error) {
		b, err := store.GetObject(h)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[h]; !ok {
			seen[h] = struct{}{}
			hashes = append(hashes, h)
		}
		return b, nil
	}, DefaultChirpLimits())
	if err != nil {
		return nil, fmt.Errorf("chirp: verify closure: %w", err)
	}
	return hashes, nil
}
