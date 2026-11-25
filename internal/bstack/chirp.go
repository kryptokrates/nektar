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

// FileObjectStore stores CHIRP objects on disk under <dir>/chirp/<sha256hex>.
type FileObjectStore struct {
	mu  sync.RWMutex
	dir string
}

func NewFileObjectStore(dir string) (*FileObjectStore, error) {
	cdir := filepath.Join(dir, "chirp")
	if err := os.MkdirAll(cdir, 0o750); err != nil {
		return nil, err
	}
	return &FileObjectStore{dir: cdir}, nil
}

func (s *FileObjectStore) PutObject(hash [32]byte, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Join(s.dir, hex.EncodeToString(hash[:]))
	if _, err := os.Stat(p); err == nil {
		return nil
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
	return os.ReadFile(filepath.Join(s.dir, hex.EncodeToString(hash[:])))
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
	built, err := chirp.Build(content, nil)
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("chirp: build: %w", err)
	}
	for _, blob := range built.Blobs {
		if err := store.PutObject(sha256.Sum256(blob), blob); err != nil {
			return "", [32]byte{}, err
		}
	}
	for _, br := range built.Branches {
		if err := store.PutObject(sha256.Sum256(br), br); err != nil {
			return "", [32]byte{}, err
		}
	}
	if err := store.PutObject(built.RootHash, built.Root); err != nil {
		return "", [32]byte{}, err
	}
	return chirp.URL(built.RootHash), built.RootHash, nil
}

// ResolveChirp resolves a chirp:// URL from store, verifies every object & closure via bcommon/chirp.Verify,
// and reassembles the exact verified byte stream.
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
	}, chirp.Limits{})
	if err != nil {
		return nil, fmt.Errorf("chirp: verify closure: %w", err)
	}
	var out []byte
	for _, blobRef := range closure.Blobs {
		b, ok := held[blobRef.Hash]
		if !ok {
			return nil, errors.New("chirp: missing verified blob in closure")
		}
		out = append(out, b...)
	}
	return out, nil
}
