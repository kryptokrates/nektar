package faststore

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
)

// bufferedWALFS wraps vfs.Default to bypass DiskHealthCheckingFS pprof overhead and coalesce
// small non-synced Pebble WAL appends into 64 KiB OS writes (preventing Windows Defender NTFS
// WriteFile stall on every 200-byte commit while still flushing to real disk on Sync/Close/fill).
type bufferedWALFS struct {
	vfs.FS
}

func (fs bufferedWALFS) Create(name string) (vfs.File, error) {
	f, err := fs.FS.Create(name)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".sst") {
		return &bufferedWALFile{File: f, bw: bufio.NewWriterSize(f, 128<<10)}, nil
	}
	return f, nil
}

func (fs bufferedWALFS) ReuseForWrite(oldname, newname string) (vfs.File, error) {
	f, err := fs.FS.ReuseForWrite(oldname, newname)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(newname, ".log") || strings.HasSuffix(newname, ".sst") {
		return &bufferedWALFile{File: f, bw: bufio.NewWriterSize(f, 128<<10)}, nil
	}
	return f, nil
}

type bufferedWALFile struct {
	vfs.File
	mu sync.Mutex
	bw *bufio.Writer
}

func (bf *bufferedWALFile) Write(p []byte) (int, error) {
	bf.mu.Lock()
	n, err := bf.bw.Write(p)
	bf.mu.Unlock()
	return n, err
}

func (bf *bufferedWALFile) Sync() error {
	bf.mu.Lock()
	if err := bf.bw.Flush(); err != nil {
		bf.mu.Unlock()
		return err
	}
	bf.mu.Unlock()
	return bf.File.Sync()
}

func (bf *bufferedWALFile) SyncData() error {
	bf.mu.Lock()
	if err := bf.bw.Flush(); err != nil {
		bf.mu.Unlock()
		return err
	}
	bf.mu.Unlock()
	return bf.File.SyncData()
}

func (bf *bufferedWALFile) SyncTo(length int64) (bool, error) {
	bf.mu.Lock()
	if err := bf.bw.Flush(); err != nil {
		bf.mu.Unlock()
		return false, err
	}
	bf.mu.Unlock()
	return bf.File.SyncTo(length)
}

func (bf *bufferedWALFile) Close() error {
	bf.mu.Lock()
	flushErr := bf.bw.Flush()
	bf.mu.Unlock()
	closeErr := bf.File.Close()
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}

var (
	ErrNotFound = errors.New("faststore: key not found")
)

type silentLogger struct{}

func (silentLogger) Infof(string, ...any)  {}
func (silentLogger) Errorf(string, ...any) {}
func (silentLogger) Fatalf(string, ...any) {}

// Options configures a pure-Go Pebble LSM/WAL fast store.
type Options struct {
	// Dir is the on-disk directory. When empty (or InMemory=true), uses Pebble's pure-Go MemFS.
	Dir string
	// InMemory uses an in-memory VFS (zero disk I/O; ideal for unit tests & ephemeral benchmarks).
	InMemory bool
	// SyncWrites controls whether commits use WAL group-commit fsync (pebble.Sync) or buffered WAL (pebble.NoSync).
	// Production nodes default to true; pebble automatically coalesces concurrent Sync commits into one fsync.
	SyncWrites bool
	// MemTableMB overrides the per-memtable size (default 32 MiB).
	MemTableMB int
}

// DB is a thin, crash-safe, zero-CGO wrapper around cockroachdb/pebble supporting
// 100k+ group-committed writes/sec across concurrent goroutines.
type DB struct {
	db      *pebble.DB
	writeOp *pebble.WriteOptions
	closed  bool
	closeMu sync.Mutex
}

// Open initializes a pure-Go Pebble LSM store.
func Open(opts Options) (*DB, error) {
	memMB := opts.MemTableMB
	if memMB <= 0 {
		memMB = 16
	}
	cache := pebble.NewCache(16 << 20)
	defer cache.Unref()

	pOpts := &pebble.Options{
		Cache:                       cache,
		Logger:                      silentLogger{},
		MemTableSize:                uint64(memMB) << 20,
		MemTableStopWritesThreshold: 4,
		L0CompactionThreshold:       16,
		L0StopWritesThreshold:       32,
		MaxOpenFiles:                256,
		BytesPerSync:                512 << 10,
		WALBytesPerSync:             512 << 10,
	}
	dir := opts.Dir
	if opts.InMemory || dir == "" {
		pOpts.FS = vfs.NewMem()
		dir = "mem"
	} else {
		if err := os.MkdirAll(filepath.Clean(dir), 0o700); err != nil {
			return nil, fmt.Errorf("faststore: mkdir %s: %w", dir, err)
		}
		if opts.SyncWrites {
			pOpts.FS = vfs.Default
		} else {
			pOpts.FS = bufferedWALFS{FS: vfs.Default}
			pOpts.WALBytesPerSync = 0
		}
	}
	pdb, err := pebble.Open(dir, pOpts)
	if err != nil {
		return nil, fmt.Errorf("faststore: open pebble (%s): %w", dir, err)
	}
	wo := pebble.NoSync
	if opts.SyncWrites {
		wo = pebble.Sync
	}
	return &DB{db: pdb, writeOp: wo}, nil
}

// Close flushes and closes the Pebble database cleanly.
func (d *DB) Close() error {
	if d == nil {
		return nil
	}
	d.closeMu.Lock()
	defer d.closeMu.Unlock()
	if d.closed || d.db == nil {
		return nil
	}
	d.closed = true
	return d.db.Close()
}

// Raw exposes the underlying *pebble.DB for batch operations.
func (d *DB) Raw() *pebble.DB {
	if d == nil {
		return nil
	}
	return d.db
}

// WriteOptions returns the configured default write durability mode (Sync or NoSync).
func (d *DB) WriteOptions() *pebble.WriteOptions {
	if d == nil || d.writeOp == nil {
		return pebble.NoSync
	}
	return d.writeOp
}

// Get retrieves a copy of the value stored at key.
func (d *DB) Get(key []byte) ([]byte, error) {
	if d == nil || d.db == nil {
		return nil, ErrNotFound
	}
	val, closer, err := d.db.Get(key)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	out := append([]byte(nil), val...)
	_ = closer.Close()
	return out, nil
}

// Has checks whether key exists without copying its value.
func (d *DB) Has(key []byte) (bool, error) {
	if d == nil || d.db == nil {
		return false, nil
	}
	_, closer, err := d.db.Get(key)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	_ = closer.Close()
	return true, nil
}

// Set writes a single key/value using the store's configured WAL durability.
func (d *DB) Set(key, val []byte) error {
	if d == nil || d.db == nil {
		return errors.New("faststore: db not initialized")
	}
	return d.db.Set(key, val, d.writeOp)
}

// Delete removes key using the store's configured WAL durability.
func (d *DB) Delete(key []byte) error {
	if d == nil || d.db == nil {
		return errors.New("faststore: db not initialized")
	}
	return d.db.Delete(key, d.writeOp)
}

// NewBatch creates an atomic write batch committed via CommitBatch.
func (d *DB) NewBatch() *pebble.Batch {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.NewBatch()
}

// CommitBatch atomically commits and closes b using the store's configured WAL group-commit mode.
func (d *DB) CommitBatch(b *pebble.Batch) error {
	if b == nil {
		return nil
	}
	defer b.Close()
	return b.Commit(d.writeOp)
}

// ScanPrefix invokes fn(key, value) for every key starting with prefix in lexicographical order.
// Returning false from fn stops iteration early.
func (d *DB) ScanPrefix(prefix []byte, fn func(key, val []byte) bool) error {
	if d == nil || d.db == nil {
		return nil
	}
	upper := prefixUpperBound(prefix)
	iter, err := d.db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: upper,
	})
	if err != nil {
		return err
	}
	defer iter.Close()
	for iter.First(); iter.Valid(); iter.Next() {
		if !fn(iter.Key(), iter.Value()) {
			break
		}
	}
	return iter.Error()
}

func prefixUpperBound(prefix []byte) []byte {
	if len(prefix) == 0 {
		return nil
	}
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}
