package bstack

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// DefaultChirpStoreQuotaBytes caps total disk usage of FileObjectStore (512 MiB).
	DefaultChirpStoreQuotaBytes int64 = 512 << 20
)

// DefaultChirpLimits returns bounded bcommon/chirp verification limits against DAG amplification.
func DefaultChirpLimits() chirp.Limits {
	return chirp.Limits{
		MaxReferences: MaxChirpReferences,
		MaxLength:     MaxChirpTotalBytes,
	}
}

// FileObjectStore stores CHIRP objects on disk under <dir>/chirp/<sha256hex>.
type FileObjectStore struct {
	mu       sync.RWMutex
	dir      string
	maxBytes int64
}

func NewFileObjectStore(dir string) (*FileObjectStore, error) {
	cdir := filepath.Join(dir, "chirp")
	if err := os.MkdirAll(cdir, 0o750); err != nil {
		return nil, err
	}
	return &FileObjectStore{dir: cdir, maxBytes: DefaultChirpStoreQuotaBytes}, nil
}

func (s *FileObjectStore) PutObject(hash [32]byte, data []byte) error {
	if len(data) > MaxChirpObjectBytes {
		return fmt.Errorf("chirp: object size %d exceeds max %d", len(data), MaxChirpObjectBytes)
	}
	if sha256.Sum256(data) != hash {
		return errors.New("chirp: object data sha256 mismatch")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Join(s.dir, hex.EncodeToString(hash[:]))
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	if s.maxBytes > 0 {
		var used int64
		if entries, err := os.ReadDir(s.dir); err == nil {
			for _, ent := range entries {
				if info, iErr := ent.Info(); iErr == nil && info.Mode().IsRegular() {
					used += info.Size()
				}
			}
		}
		if used+int64(len(data)) > s.maxBytes {
			return fmt.Errorf("chirp: object store quota exceeded (%d + %d > %d)", used, len(data), s.maxBytes)
		}
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s *FileObjectStore) GetObject(hash [32]byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := filepath.Join(s.dir, hex.EncodeToString(hash[:]))
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if info.Size() > MaxChirpObjectBytes {
		return nil, fmt.Errorf("chirp: stored object size %d exceeds max %d", info.Size(), MaxChirpObjectBytes)
	}
	return os.ReadFile(p)
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

// StoreChirp splits content into a canonical BRC-167 CHIRP Merkle tree (bcommon/chirp),
// writes blobs -> branches -> root bottom-up, and returns the chirp:// URL and root hash.
func StoreChirp(store ObjectStore, content []byte) (string, [32]byte, error) {
	u, root, _, err := StoreChirpWithHashes(store, content)
	return u, root, err
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
