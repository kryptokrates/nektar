package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	crand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/store"
)

const (
	archiveMagicV1        = "NKTBAK01"
	archiveMagic          = "NKTBAK02"
	archiveVersionV1      = 1
	archiveVersion        = 2
	aeadChunkSize         = 1 << 20 // 1 MiB constant-RAM streaming AEAD frame size (ARCH-F10)
	cipherMonolithicGCM   = "aes-256-gcm"
	cipherChunkedGCM1M    = "aes-256-gcm-chunked-1m"
	DefaultBackupInterval = 6 * time.Hour
	DefaultBackupKeep     = 14
	backupStatusKVKey     = "backup_last_status_v1"
)

var (
	backupProtocol = sdkwallet.Protocol{
		SecurityLevel: sdkwallet.SecurityLevelEveryAppAndCounterparty,
		Protocol:      "nektar backup",
	}
	backupKeyID = "state-v1"
)

// EnvelopeHeader is the authenticated plaintext header (AAD) of every .nktbak encrypted archive.
type EnvelopeHeader struct {
	Version       int    `json:"version"`
	Network       string `json:"network"`
	IdentityPub   string `json:"identityPub"`
	WalletMode    string `json:"walletMode"`
	SchemaVersion int    `json:"schemaVersion"`
	CreatedAt     int64  `json:"createdAt"`
	KDF           string `json:"kdf"`
	Cipher        string `json:"cipher"`
	Compression   string `json:"compression"`
	WrappedDEKB64 string `json:"wrappedDekB64"`
	NonceB64      string `json:"nonceB64"`
}

// FileChecksum records expected byte length and SHA-256 digest of an archived state file.
type FileChecksum struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is stored inside the encrypted tar archive and cross-checked during Verify and Restore.
type Manifest struct {
	Version       int                     `json:"version"`
	Network       string                  `json:"network"`
	IdentityPub   string                  `json:"identityPub"`
	WalletMode    string                  `json:"walletMode"`
	SchemaVersion int                     `json:"schemaVersion"`
	CreatedAt     int64                   `json:"createdAt"`
	Files         map[string]FileChecksum `json:"files"`
}

// Result describes one completed encrypted backup snapshot.
type Result struct {
	ArchiveName string   `json:"archiveName"`
	Location    string   `json:"location"`
	SizeBytes   int64    `json:"sizeBytes"`
	SHA256      string   `json:"sha256"`
	CreatedAt   int64    `json:"createdAt"`
	Pruned      int      `json:"pruned"`
	Manifest    Manifest `json:"manifest"`
}

// Status captures operational telemetry for nektar doctor, /state, /v1/metrics, and nektar backup status.
type Status struct {
	Configured         bool   `json:"configured"`
	Target             string `json:"target,omitempty"`
	IntervalSec        int64  `json:"intervalSec,omitempty"`
	Keep               int    `json:"keep,omitempty"`
	LastBackupAt       int64  `json:"lastBackupAt,omitempty"`
	LastArchive        string `json:"lastArchive,omitempty"`
	LastLocation       string `json:"lastLocation,omitempty"`
	LastSizeBytes      int64  `json:"lastSizeBytes,omitempty"`
	LastSHA256         string `json:"lastSha256,omitempty"`
	LastError          string `json:"lastError,omitempty"`
	TotalBackups       int64  `json:"totalBackups"`
	WalletMode         string `json:"walletMode,omitempty"`
	IdentityBackupHint string `json:"identityBackupHint,omitempty"`
}

// Config configures the encrypted automated state backup Manager.
type Config struct {
	DB           *store.DB
	Port         *chain.Port
	DataDir      string
	Sink         Sink
	Interval     time.Duration
	StartupDelay time.Duration
	Keep         int
	OnComplete   func(Status)
}

// Manager runs WAL-safe SQLite snapshots compressed with zstd and envelope-encrypted with BRC-42/AES-256-GCM.
type Manager struct {
	cfg Config

	mu     sync.Mutex
	status Status

	startMu sync.Mutex
	started bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// ParseIntervalEnv reads NEKTAR_BACKUP_INTERVAL (Go duration e.g. "6h" or integer seconds).
func ParseIntervalEnv(raw string) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return DefaultBackupInterval
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	if sec, err := strconv.ParseInt(s, 10, 64); err == nil && sec > 0 {
		return time.Duration(sec) * time.Second
	}
	return DefaultBackupInterval
}

// ParseKeepEnv reads NEKTAR_BACKUP_KEEP (default 14).
func ParseKeepEnv(raw string) int {
	if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && v > 0 {
		return v
	}
	return DefaultBackupKeep
}

// NewManager creates an encrypted state backup Manager.
func NewManager(cfg Config) *Manager {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultBackupInterval
	}
	if cfg.Keep <= 0 {
		cfg.Keep = DefaultBackupKeep
	}
	if cfg.StartupDelay == 0 {
		cfg.StartupDelay = 2 * time.Minute
	}
	walletMode := ""
	if cfg.Port != nil {
		walletMode = cfg.Port.WalletMode()
	}
	st := Status{
		Configured:  cfg.Sink != nil,
		IntervalSec: int64(cfg.Interval.Seconds()),
		Keep:        cfg.Keep,
		WalletMode:  walletMode,
	}
	if cfg.Sink != nil {
		st.Target = cfg.Sink.Name()
	}
	if walletMode == "embedded" {
		st.IdentityBackupHint = "Keep a separate offline backup of wallet/identity.json — archives are BRC-42 encrypted to your wallet root identity and intentionally exclude raw private keys."
	}
	if cfg.DB != nil {
		var saved Status
		if ok, err := cfg.DB.GetJSON(backupStatusKVKey, &saved); err == nil && ok {
			st.LastBackupAt = saved.LastBackupAt
			st.LastArchive = saved.LastArchive
			st.LastLocation = saved.LastLocation
			st.LastSizeBytes = saved.LastSizeBytes
			st.LastSHA256 = saved.LastSHA256
			st.TotalBackups = saved.TotalBackups
		}
	}
	return &Manager{cfg: cfg, status: st}
}

// Start launches the scheduled background backup loop when a Sink is configured.
func (m *Manager) Start(ctx context.Context) {
	if m == nil || m.cfg.Sink == nil {
		return
	}
	m.startMu.Lock()
	if m.started {
		m.startMu.Unlock()
		return
	}
	m.started = true
	runCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.startMu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		delay := m.cfg.StartupDelay
		if delay < 0 {
			delay = 0
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-runCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		_, _ = m.BackupNow(runCtx)

		ticker := time.NewTicker(m.cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				_, _ = m.BackupNow(runCtx)
			}
		}
	}()
}

// Stop cancels the scheduled backup loop and waits for any active snapshot to finish cleanly.
func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.startMu.Lock()
	cancel := m.cancel
	m.started = false
	m.cancel = nil
	m.startMu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.wg.Wait()
}

// Status returns the current backup telemetry snapshot.
func (m *Manager) Status() Status {
	if m == nil {
		return Status{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// BackupNow takes an atomic WAL-safe snapshot, streams tar -> zstd -> chunked AES-256-GCM in O(1 MiB) RAM,
// writes to the configured Sink, and enforces retention pruning.
func (m *Manager) BackupNow(ctx context.Context) (*Result, error) {
	if m == nil || m.cfg.Sink == nil {
		return nil, errors.New("backup: no backup sink configured (set NEKTAR_BACKUP_DIR or NEKTAR_BACKUP_S3_BUCKET)")
	}
	res, err := m.BackupToSink(ctx, m.cfg.Sink, m.cfg.Keep)
	m.mu.Lock()
	if err != nil {
		m.status.LastError = err.Error()
	} else {
		m.status.LastError = ""
		m.status.LastBackupAt = res.CreatedAt
		m.status.LastArchive = res.ArchiveName
		m.status.LastLocation = res.Location
		m.status.LastSizeBytes = res.SizeBytes
		m.status.LastSHA256 = res.SHA256
		m.status.TotalBackups++
	}
	stCopy := m.status
	m.mu.Unlock()
	if m.cfg.DB != nil && err == nil {
		_ = m.cfg.DB.SetJSON(backupStatusKVKey, stCopy)
	}
	if m.cfg.OnComplete != nil {
		m.cfg.OnComplete(stCopy)
	}
	return res, err
}

// BackupToSink creates a streamed encrypted snapshot and uploads it to sink (using O(1) disk move/copy for LocalDirSink).
func (m *Manager) BackupToSink(ctx context.Context, sink Sink, keep int) (*Result, error) {
	if m == nil || m.cfg.DB == nil || m.cfg.Port == nil {
		return nil, errors.New("backup: DB and chain Port are required")
	}
	if sink == nil {
		return nil, errors.New("backup: sink is required")
	}
	tmpOut, err := os.CreateTemp("", "nektar-bak-out-*.nktbak")
	if err != nil {
		return nil, err
	}
	tmpPath := tmpOut.Name()
	_ = tmpOut.Close()
	defer os.Remove(tmpPath)

	manifest, sizeBytes, shaHex, err := CreateEncryptedArchiveToFile(ctx, m.cfg.DB, m.cfg.Port, m.cfg.DataDir, tmpPath)
	if err != nil {
		return nil, err
	}
	ts := time.Unix(manifest.CreatedAt, 0).UTC().Format("20060102T150405Z")
	idShort := manifest.IdentityPub
	if len(idShort) > 10 {
		idShort = idShort[:10]
	}
	name := fmt.Sprintf("nektar-%s-%s-%s.nktbak", manifest.Network, idShort, ts)

	var loc string
	if localSink, ok := sink.(*LocalDirSink); ok {
		loc, err = localSink.PutFile(ctx, name, tmpPath)
	} else {
		raw, rErr := os.ReadFile(tmpPath)
		if rErr != nil {
			return nil, rErr
		}
		loc, err = sink.Put(ctx, name, raw)
	}
	if err != nil {
		return nil, err
	}
	pruned := 0
	if keep > 0 {
		pruned, _ = sink.Prune(ctx, keep)
	}
	return &Result{
		ArchiveName: name,
		Location:    loc,
		SizeBytes:   sizeBytes,
		SHA256:      shaHex,
		CreatedAt:   manifest.CreatedAt,
		Pruned:      pruned,
		Manifest:    *manifest,
	}, nil
}

type stagedMember struct {
	name string
	path string
	size int64
}

// CreateEncryptedArchive snapshots live SQLite state via VACUUM INTO, streams tar -> zstd -> 1 MiB chunked AES-256-GCM
// into a temporary file on disk, and returns the resulting archive bytes and Manifest.
func CreateEncryptedArchive(ctx context.Context, db *store.DB, port *chain.Port, dataDir string) ([]byte, *Manifest, error) {
	tmpOut, err := os.CreateTemp("", "nektar-bak-mem-*.nktbak")
	if err != nil {
		return nil, nil, err
	}
	tmpPath := tmpOut.Name()
	_ = tmpOut.Close()
	defer os.Remove(tmpPath)

	manifest, _, _, err := CreateEncryptedArchiveToFile(ctx, db, port, dataDir, tmpPath)
	if err != nil {
		return nil, nil, err
	}
	raw, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, nil, err
	}
	return raw, manifest, nil
}

// CreateEncryptedArchiveToFile snapshots nektar.sqlite, wallet/wallet.sqlite, workflows.sqlite, hub.sqlite,
// spend-guard.json, chirp/*, and client-txs/* using O(1 MiB) constant memory via streamed tar -> zstd -> chunked AES-256-GCM.
func CreateEncryptedArchiveToFile(
	ctx context.Context,
	db *store.DB,
	port *chain.Port,
	dataDir string,
	dstPath string,
) (*Manifest, int64, string, error) {
	if db == nil || port == nil || port.WalletInterface() == nil {
		return nil, 0, "", errors.New("backup: db, chain port, and wallet interface are required")
	}
	tmpDir, err := os.MkdirTemp("", "nektar-bak-stage-*")
	if err != nil {
		return nil, 0, "", fmt.Errorf("backup: create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	now := time.Now().UTC().Unix()
	schemaVer, _ := db.UserVersion()
	manifest := &Manifest{
		Version:       archiveVersion,
		Network:       port.Network(),
		IdentityPub:   port.IdentityPubHex(),
		WalletMode:    port.WalletMode(),
		SchemaVersion: schemaVer,
		CreatedAt:     now,
		Files:         make(map[string]FileChecksum),
	}

	var members []stagedMember
	addStaged := func(arcName, diskPath string) error {
		cs, cErr := hashFileStream(diskPath)
		if cErr != nil {
			return cErr
		}
		manifest.Files[arcName] = cs
		members = append(members, stagedMember{name: arcName, path: diskPath, size: cs.Size})
		return nil
	}

	// 1. Atomic WAL-safe snapshot of nektar.sqlite.
	nektarSnap := filepath.Join(tmpDir, "nektar.sqlite")
	if err := db.VacuumInto(ctx, nektarSnap); err != nil {
		return nil, 0, "", fmt.Errorf("backup: snapshot nektar.sqlite: %w", err)
	}
	if err := addStaged("nektar.sqlite", nektarSnap); err != nil {
		return nil, 0, "", err
	}

	// 2. Atomic WAL-safe snapshot of embedded wallet/wallet.sqlite (NEVER raw identity.json).
	if eng := port.Engine(); eng != nil {
		walletSnap := filepath.Join(tmpDir, "wallet.sqlite")
		if err := eng.SnapshotTo(ctx, walletSnap); err != nil {
			return nil, 0, "", fmt.Errorf("backup: snapshot wallet.sqlite: %w", err)
		}
		if err := addStaged("wallet/wallet.sqlite", walletSnap); err != nil {
			return nil, 0, "", err
		}
	}

	// 3. Optional workflows.sqlite, hub.sqlite, spend-guard.json, chirp/*, and client-txs/* in dataDir.
	if strings.TrimSpace(dataDir) != "" {
		for _, auxDB := range []string{"workflows.sqlite", "hub.sqlite"} {
			srcDB := filepath.Join(dataDir, auxDB)
			if fi, sErr := os.Stat(srcDB); sErr == nil && !fi.IsDir() && fi.Size() > 0 {
				snapPath := filepath.Join(tmpDir, auxDB)
				if vErr := vacuumSQLiteFileInto(ctx, srcDB, snapPath); vErr != nil {
					return nil, 0, "", fmt.Errorf("backup: snapshot %s: %w", auxDB, vErr)
				}
				if err := addStaged(auxDB, snapPath); err != nil {
					return nil, 0, "", err
				}
			}
		}

		sgPath := filepath.Join(dataDir, "spend-guard.json")
		if fi, sErr := os.Stat(sgPath); sErr == nil && !fi.IsDir() && fi.Size() > 0 {
			sgCopy := filepath.Join(tmpDir, "spend-guard.json")
			if err := copyFileStream(sgPath, sgCopy); err == nil {
				if err := addStaged("spend-guard.json", sgCopy); err != nil {
					return nil, 0, "", err
				}
			}
		}

		for _, dirPrefix := range []string{"chirp", "client-txs"} {
			subRoot := filepath.Join(dataDir, dirPrefix)
			if info, statErr := os.Stat(subRoot); statErr == nil && info.IsDir() {
				var relPaths []string
				if wErr := filepath.WalkDir(subRoot, func(p string, d os.DirEntry, err error) error {
					if err != nil || d.IsDir() {
						return err
					}
					rel, rErr := filepath.Rel(dataDir, p)
					if rErr != nil {
						return rErr
					}
					relSlash := filepath.ToSlash(rel)
					if !strings.HasPrefix(relSlash, dirPrefix+"/") || strings.Contains(relSlash, "..") {
						return nil
					}
					relPaths = append(relPaths, relSlash)
					return nil
				}); wErr != nil {
					return nil, 0, "", fmt.Errorf("backup: walk %s dir: %w", dirPrefix, wErr)
				}
				sort.Strings(relPaths)
				for _, relSlash := range relPaths {
					srcFile := filepath.Join(dataDir, filepath.FromSlash(relSlash))
					if err := addStaged(relSlash, srcFile); err != nil {
						return nil, 0, "", err
					}
				}
			}
		}
	}

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, 0, "", err
	}
	manifestPath := filepath.Join(tmpDir, "manifest.json")
	if err := os.WriteFile(manifestPath, manifestBytes, 0o600); err != nil {
		return nil, 0, "", err
	}
	members = append(members, stagedMember{name: "manifest.json", path: manifestPath, size: int64(len(manifestBytes))})

	// Envelope encryption: random 32-byte AES-256-GCM DEK wrapped with BRC-42 wallet.Encrypt (self).
	dek := make([]byte, 32)
	if _, err := crand.Read(dek); err != nil {
		return nil, 0, "", fmt.Errorf("backup: generate DEK: %w", err)
	}
	defer clear(dek)

	baseNonce := make([]byte, 12)
	if _, err := crand.Read(baseNonce); err != nil {
		return nil, 0, "", fmt.Errorf("backup: generate nonce: %w", err)
	}

	wrapRes, err := port.WalletInterface().Encrypt(ctx, sdkwallet.EncryptArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{
			ProtocolID:   backupProtocol,
			KeyID:        backupKeyID,
			Counterparty: sdkwallet.Counterparty{Type: sdkwallet.CounterpartyTypeSelf},
		},
		Plaintext: dek,
	}, chain.Originator)
	if err != nil || wrapRes == nil || len(wrapRes.Ciphertext) == 0 {
		return nil, 0, "", fmt.Errorf("backup: wallet BRC-42 wrap DEK: %w", err)
	}

	envHdr := EnvelopeHeader{
		Version:       archiveVersion,
		Network:       manifest.Network,
		IdentityPub:   manifest.IdentityPub,
		WalletMode:    manifest.WalletMode,
		SchemaVersion: manifest.SchemaVersion,
		CreatedAt:     manifest.CreatedAt,
		KDF:           "brc42-self-v1",
		Cipher:        cipherChunkedGCM1M,
		Compression:   "zstd",
		WrappedDEKB64: base64.StdEncoding.EncodeToString(wrapRes.Ciphertext),
		NonceB64:      base64.StdEncoding.EncodeToString(baseNonce),
	}
	hdrJSON, err := json.Marshal(envHdr)
	if err != nil {
		return nil, 0, "", err
	}

	blk, err := aes.NewCipher(dek)
	if err != nil {
		return nil, 0, "", err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, 0, "", err
	}

	outFile, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, 0, "", err
	}
	outHasher := sha256.New()
	var outSize int64
	outWriter := io.MultiWriter(outFile, outHasher, &byteCounter{n: &outSize})

	if _, err := outWriter.Write([]byte(archiveMagic)); err != nil {
		_ = outFile.Close()
		return nil, 0, "", err
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(hdrJSON)))
	if _, err := outWriter.Write(lenBuf[:]); err != nil {
		_ = outFile.Close()
		return nil, 0, "", err
	}
	if _, err := outWriter.Write(hdrJSON); err != nil {
		_ = outFile.Close()
		return nil, 0, "", err
	}

	encWriter := newChunkedGCMWriter(outWriter, gcm, baseNonce, hdrJSON)
	zw, err := zstd.NewWriter(encWriter, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		_ = outFile.Close()
		return nil, 0, "", fmt.Errorf("backup: zstd writer: %w", err)
	}
	tw := tar.NewWriter(zw)
	copyBuf := make([]byte, 64<<10)
	for _, m := range members {
		hdr := &tar.Header{
			Name:    m.name,
			Mode:    0o600,
			Size:    m.size,
			ModTime: time.Unix(now, 0).UTC(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = tw.Close()
			_ = zw.Close()
			_ = outFile.Close()
			return nil, 0, "", err
		}
		f, oErr := os.Open(m.path)
		if oErr != nil {
			_ = tw.Close()
			_ = zw.Close()
			_ = outFile.Close()
			return nil, 0, "", oErr
		}
		_, cErr := io.CopyBuffer(tw, io.LimitReader(f, m.size), copyBuf)
		_ = f.Close()
		if cErr != nil {
			_ = tw.Close()
			_ = zw.Close()
			_ = outFile.Close()
			return nil, 0, "", cErr
		}
	}
	if err := tw.Close(); err != nil {
		_ = zw.Close()
		_ = outFile.Close()
		return nil, 0, "", err
	}
	if err := zw.Close(); err != nil {
		_ = outFile.Close()
		return nil, 0, "", err
	}
	if err := encWriter.Close(); err != nil {
		_ = outFile.Close()
		return nil, 0, "", err
	}
	if err := outFile.Sync(); err != nil {
		_ = outFile.Close()
		return nil, 0, "", err
	}
	if err := outFile.Close(); err != nil {
		return nil, 0, "", err
	}
	return manifest, outSize, hex.EncodeToString(outHasher.Sum(nil)), nil
}

type byteCounter struct {
	n *int64
}

func (c *byteCounter) Write(p []byte) (int, error) {
	*c.n += int64(len(p))
	return len(p), nil
}

func deriveChunkNonce(baseNonce []byte, chunkIdx uint64) [12]byte {
	var n [12]byte
	copy(n[:], baseNonce)
	tail := binary.BigEndian.Uint64(n[4:12]) ^ chunkIdx
	binary.BigEndian.PutUint64(n[4:12], tail)
	return n
}

func buildChunkAAD(hdrJSON []byte, chunkIdx uint64, final bool) []byte {
	aad := make([]byte, len(hdrJSON)+9)
	copy(aad, hdrJSON)
	binary.BigEndian.PutUint64(aad[len(hdrJSON):len(hdrJSON)+8], chunkIdx)
	if final {
		aad[len(hdrJSON)+8] = 1
	}
	return aad
}

type chunkedGCMWriter struct {
	dst       io.Writer
	gcm       cipher.AEAD
	baseNonce []byte
	hdrJSON   []byte
	buf       []byte
	chunkIdx  uint64
	closed    bool
}

func newChunkedGCMWriter(dst io.Writer, gcm cipher.AEAD, baseNonce, hdrJSON []byte) *chunkedGCMWriter {
	return &chunkedGCMWriter{
		dst:       dst,
		gcm:       gcm,
		baseNonce: append([]byte(nil), baseNonce...),
		hdrJSON:   hdrJSON,
		buf:       make([]byte, 0, aeadChunkSize),
	}
}

func (w *chunkedGCMWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("backup: write to closed chunked AEAD writer")
	}
	total := len(p)
	for len(p) > 0 {
		space := aeadChunkSize - len(w.buf)
		if len(p) < space {
			w.buf = append(w.buf, p...)
			break
		}
		// Keep at least 1 byte or emit non-final full frame when more bytes follow; if exact fit, wait until next Write or Close so Close marks final=true!
		if len(p) == space {
			w.buf = append(w.buf, p...)
			break
		}
		w.buf = append(w.buf, p[:space]...)
		p = p[space:]
		if err := w.flushChunk(false); err != nil {
			return 0, err
		}
	}
	return total, nil
}

func (w *chunkedGCMWriter) flushChunk(final bool) error {
	nonce := deriveChunkNonce(w.baseNonce, w.chunkIdx)
	aad := buildChunkAAD(w.hdrJSON, w.chunkIdx, final)
	ct := w.gcm.Seal(nil, nonce[:], w.buf, aad)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(ct)))
	if _, err := w.dst.Write(lenBuf[:]); err != nil {
		return err
	}
	if _, err := w.dst.Write(ct); err != nil {
		return err
	}
	w.buf = w.buf[:0]
	w.chunkIdx++
	return nil
}

func (w *chunkedGCMWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	return w.flushChunk(true)
}

type chunkedGCMReader struct {
	src       io.Reader
	gcm       cipher.AEAD
	baseNonce []byte
	hdrJSON   []byte
	plainBuf  []byte
	chunkIdx  uint64
	sawFinal  bool
}

func newChunkedGCMReader(src io.Reader, gcm cipher.AEAD, baseNonce, hdrJSON []byte) *chunkedGCMReader {
	return &chunkedGCMReader{
		src:       src,
		gcm:       gcm,
		baseNonce: append([]byte(nil), baseNonce...),
		hdrJSON:   hdrJSON,
	}
}

func (r *chunkedGCMReader) Read(p []byte) (int, error) {
	for len(r.plainBuf) == 0 {
		if r.sawFinal {
			return 0, io.EOF
		}
		var lenBuf [4]byte
		if _, err := io.ReadFull(r.src, lenBuf[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return 0, errors.New("backup: truncated archive (missing final AEAD chunk)")
			}
			return 0, err
		}
		frameLen := binary.BigEndian.Uint32(lenBuf[:])
		if frameLen < uint32(r.gcm.Overhead()) || frameLen > uint32(aeadChunkSize+r.gcm.Overhead()+64) {
			return 0, fmt.Errorf("backup: invalid AEAD frame length %d", frameLen)
		}
		ct := make([]byte, frameLen)
		if _, err := io.ReadFull(r.src, ct); err != nil {
			return 0, fmt.Errorf("backup: truncated AEAD frame: %w", err)
		}
		nonce := deriveChunkNonce(r.baseNonce, r.chunkIdx)
		// Try non-final AAD first, or final AAD if this is the final chunk.
		pt, err := r.gcm.Open(nil, nonce[:], ct, buildChunkAAD(r.hdrJSON, r.chunkIdx, false))
		if err != nil {
			ptFinal, fErr := r.gcm.Open(nil, nonce[:], ct, buildChunkAAD(r.hdrJSON, r.chunkIdx, true))
			if fErr != nil {
				return 0, fmt.Errorf("backup: AES-256-GCM authentication failed on chunk %d (tampered or wrong key): %w", r.chunkIdx, fErr)
			}
			var extra [1]byte
			if nExtra, _ := r.src.Read(extra[:]); nExtra > 0 {
				return 0, errors.New("backup: unexpected trailing data after final AEAD chunk")
			}
			pt = ptFinal
			r.sawFinal = true
		}
		r.chunkIdx++
		r.plainBuf = pt
	}
	n := copy(p, r.plainBuf)
	r.plainBuf = r.plainBuf[n:]
	return n, nil
}

// unpackToDir streams decryption + zstd decompression + tar extraction directly onto disk inside outDir
// in O(1 MiB) constant memory while verifying every manifest SHA-256 checksum (supports both NKTBAK02 and legacy NKTBAK01).
func unpackToDir(ctx context.Context, wallet sdkwallet.Interface, r io.Reader, outDir string) (*EnvelopeHeader, *Manifest, map[string]string, error) {
	if wallet == nil {
		return nil, nil, nil, errors.New("backup: wallet interface required to decrypt archive")
	}
	var magicBuf [8]byte
	if _, err := io.ReadFull(r, magicBuf[:]); err != nil {
		return nil, nil, nil, errors.New("backup: invalid archive magic header")
	}
	magic := string(magicBuf[:])
	if magic != archiveMagic && magic != archiveMagicV1 {
		return nil, nil, nil, errors.New("backup: invalid archive magic header")
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, nil, nil, errors.New("backup: malformed envelope header length")
	}
	hdrLen := binary.BigEndian.Uint32(lenBuf[:])
	if hdrLen == 0 || hdrLen > 1<<20 {
		return nil, nil, nil, errors.New("backup: malformed envelope header length")
	}
	hdrJSON := make([]byte, hdrLen)
	if _, err := io.ReadFull(r, hdrJSON); err != nil {
		return nil, nil, nil, errors.New("backup: truncated envelope header")
	}

	var hdr EnvelopeHeader
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		return nil, nil, nil, fmt.Errorf("backup: parse envelope header: %w", err)
	}
	if hdr.Compression != "zstd" {
		return nil, nil, nil, fmt.Errorf("backup: unsupported compression %q", hdr.Compression)
	}
	isV2 := magic == archiveMagic && hdr.Version == archiveVersion && hdr.Cipher == cipherChunkedGCM1M
	isV1 := magic == archiveMagicV1 && hdr.Version == archiveVersionV1 && hdr.Cipher == cipherMonolithicGCM
	if !isV2 && !isV1 {
		return nil, nil, nil, fmt.Errorf("backup: unsupported archive format v%d (%s/%s)", hdr.Version, hdr.Cipher, hdr.Compression)
	}

	wrappedDEK, err := base64.StdEncoding.DecodeString(hdr.WrappedDEKB64)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("backup: decode wrapped DEK: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(hdr.NonceB64)
	if err != nil || len(nonce) != 12 {
		return nil, nil, nil, errors.New("backup: invalid AES-GCM nonce")
	}

	unwrapRes, err := wallet.Decrypt(ctx, sdkwallet.DecryptArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{
			ProtocolID:   backupProtocol,
			KeyID:        backupKeyID,
			Counterparty: sdkwallet.Counterparty{Type: sdkwallet.CounterpartyTypeSelf},
		},
		Ciphertext: wrappedDEK,
	}, chain.Originator)
	if err != nil || unwrapRes == nil || len(unwrapRes.Plaintext) != 32 {
		return nil, nil, nil, fmt.Errorf("backup: wallet BRC-42 unwrap DEK failed (wrong wallet identity?): %w", err)
	}
	dek := append([]byte(nil), unwrapRes.Plaintext...)
	defer clear(dek)

	blk, err := aes.NewCipher(dek)
	if err != nil {
		return nil, nil, nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, nil, nil, err
	}

	var plainStream io.Reader
	var v2Reader *chunkedGCMReader
	if isV2 {
		v2Reader = newChunkedGCMReader(r, gcm, nonce, hdrJSON)
		plainStream = v2Reader
	} else {
		ct, rErr := io.ReadAll(r)
		if rErr != nil {
			return nil, nil, nil, rErr
		}
		compressed, oErr := gcm.Open(nil, nonce, ct, hdrJSON)
		if oErr != nil {
			return nil, nil, nil, fmt.Errorf("backup: AES-256-GCM authentication failed (tampered or wrong key): %w", oErr)
		}
		plainStream = bytes.NewReader(compressed)
	}

	zr, err := zstd.NewReader(plainStream)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("backup: zstd reader: %w", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	actualSums := make(map[string]FileChecksum)
	extractedPaths := make(map[string]string)
	copyBuf := make([]byte, 64<<10)

	for {
		th, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("backup: read tar entry: %w", err)
		}
		cleanName := filepath.ToSlash(filepath.Clean(th.Name))
		if cleanName == "" || cleanName == "." || strings.HasPrefix(cleanName, "..") || strings.HasPrefix(cleanName, "/") || strings.Contains(cleanName, "../") {
			return nil, nil, nil, fmt.Errorf("backup: illegal archive entry path %q", th.Name)
		}
		dst := filepath.Join(outDir, filepath.FromSlash(cleanName))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, nil, nil, err
		}
		f, cErr := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if cErr != nil {
			return nil, nil, nil, cErr
		}
		h := sha256.New()
		n, wErr := io.CopyBuffer(io.MultiWriter(f, h), tr, copyBuf)
		closeErr := f.Close()
		if wErr != nil {
			return nil, nil, nil, wErr
		}
		if closeErr != nil {
			return nil, nil, nil, closeErr
		}
		actualSums[cleanName] = FileChecksum{
			Size:   n,
			SHA256: hex.EncodeToString(h.Sum(nil)),
		}
		extractedPaths[cleanName] = dst
	}
	// Drain v2Reader to EOF so final-chunk authentication & trailing-byte rejection always execute!
	if v2Reader != nil {
		if _, err := io.CopyBuffer(io.Discard, v2Reader, copyBuf); err != nil {
			return nil, nil, nil, err
		}
	}

	manifestPath, ok := extractedPaths["manifest.json"]
	if !ok {
		return nil, nil, nil, errors.New("backup: archive missing manifest.json")
	}
	manifestRaw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, nil, nil, err
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return nil, nil, nil, fmt.Errorf("backup: parse manifest.json: %w", err)
	}
	for name, expected := range manifest.Files {
		got, exists := actualSums[name]
		if !exists {
			return nil, nil, nil, fmt.Errorf("backup: archive missing expected file %q", name)
		}
		if got.Size != expected.Size {
			return nil, nil, nil, fmt.Errorf("backup: size mismatch for %q (%d != %d)", name, got.Size, expected.Size)
		}
		if got.SHA256 != expected.SHA256 {
			return nil, nil, nil, fmt.Errorf("backup: SHA-256 checksum mismatch for %q", name)
		}
	}
	return &hdr, &manifest, extractedPaths, nil
}

var archivedSQLiteDBNames = []string{
	"nektar.sqlite",
	"wallet/wallet.sqlite",
	"workflows.sqlite",
	"hub.sqlite",
}

// VerifyArchive stream-decrypts, checks chunked GCM authentication & SHA-256 checksums, and runs SQLite
// PRAGMA integrity_check on every archived database inside a temporary sandbox directory.
func VerifyArchive(ctx context.Context, wallet sdkwallet.Interface, archiveBytes []byte) (*Manifest, error) {
	return VerifyArchiveReader(ctx, wallet, bytes.NewReader(archiveBytes))
}

// VerifyArchiveReader verifies an encrypted .nktbak stream in O(1 MiB) RAM.
func VerifyArchiveReader(ctx context.Context, wallet sdkwallet.Interface, r io.Reader) (*Manifest, error) {
	tmpDir, err := os.MkdirTemp("", "nektar-bak-verify-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	_, manifest, extracted, err := unpackToDir(ctx, wallet, r, tmpDir)
	if err != nil {
		return nil, err
	}
	for _, dbName := range archivedSQLiteDBNames {
		staged, ok := extracted[dbName]
		if !ok {
			continue
		}
		if err := sqliteIntegrityCheck(ctx, staged); err != nil {
			return nil, fmt.Errorf("backup: integrity_check failed for %s: %w", dbName, err)
		}
	}
	return manifest, nil
}

// RestoreArchive verifies an encrypted .nktbak archive end-to-end and atomically restores its SQLite
// databases, spend-guard state, CHIRP objects, and client-txs into targetDataDir.
func RestoreArchive(ctx context.Context, wallet sdkwallet.Interface, archiveBytes []byte, targetDataDir string) (*Manifest, error) {
	return RestoreArchiveReader(ctx, wallet, bytes.NewReader(archiveBytes), targetDataDir)
}

// RestoreArchiveReader stream-restores an encrypted .nktbak archive in O(1 MiB) constant memory.
func RestoreArchiveReader(ctx context.Context, wallet sdkwallet.Interface, r io.Reader, targetDataDir string) (*Manifest, error) {
	if strings.TrimSpace(targetDataDir) == "" {
		return nil, errors.New("backup: target data directory is required")
	}
	tmpDir, err := os.MkdirTemp("", "nektar-bak-restore-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	_, manifest, extracted, err := unpackToDir(ctx, wallet, r, tmpDir)
	if err != nil {
		return nil, err
	}
	for _, dbName := range archivedSQLiteDBNames {
		staged, ok := extracted[dbName]
		if !ok {
			continue
		}
		if err := sqliteIntegrityCheck(ctx, staged); err != nil {
			return nil, fmt.Errorf("backup: integrity_check failed for %s: %w", dbName, err)
		}
	}

	if err := os.MkdirAll(targetDataDir, 0o700); err != nil {
		return nil, err
	}
	allowedTargets := []string{
		"nektar.sqlite",
		"wallet/wallet.sqlite",
		"workflows.sqlite",
		"hub.sqlite",
		"spend-guard.json",
	}
	var dynamicTargets []string
	for rel := range extracted {
		if (strings.HasPrefix(rel, "chirp/") || strings.HasPrefix(rel, "client-txs/")) &&
			!strings.Contains(rel, "..") && !strings.HasPrefix(rel, "/") {
			dynamicTargets = append(dynamicTargets, rel)
		}
	}
	sort.Strings(dynamicTargets)
	allowedTargets = append(allowedTargets, dynamicTargets...)

	for _, rel := range allowedTargets {
		staged, ok := extracted[rel]
		if !ok {
			continue
		}
		dst := filepath.Join(targetDataDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, err
		}
		if err := atomicCopyFromStaged(staged, dst, 0o600); err != nil {
			return nil, fmt.Errorf("backup: write %s: %w", rel, err)
		}
		if strings.HasSuffix(rel, ".sqlite") {
			_ = os.Remove(dst + "-wal")
			_ = os.Remove(dst + "-shm")
		}
	}
	return manifest, nil
}

func vacuumSQLiteFileInto(ctx context.Context, srcPath, dstPath string) error {
	_ = os.Remove(dstPath)
	db, err := sql.Open("sqlite", srcPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	escaped := strings.ReplaceAll(dstPath, "'", "''")
	_, err = db.ExecContext(ctx, fmt.Sprintf("VACUUM INTO '%s'", escaped))
	return err
}

func hashFileStream(path string) (FileChecksum, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileChecksum{}, err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 64<<10)
	n, err := io.CopyBuffer(h, f, buf)
	if err != nil {
		return FileChecksum{}, err
	}
	return FileChecksum{
		Size:   n,
		SHA256: hex.EncodeToString(h.Sum(nil)),
	}, nil
}

func copyFileStream(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	buf := make([]byte, 64<<10)
	_, cErr := io.CopyBuffer(out, in, buf)
	clErr := out.Close()
	if cErr != nil {
		return cErr
	}
	return clErr
}

func sqliteIntegrityCheck(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(res), "ok") {
		return fmt.Errorf("integrity_check returned %q", res)
	}
	return nil
}

func atomicCopyFromStaged(stagedPath, dst string, perm os.FileMode) error {
	dir := filepath.Dir(dst)
	f, err := os.CreateTemp(dir, ".restore-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	in, oErr := os.Open(stagedPath)
	if oErr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return oErr
	}
	buf := make([]byte, 64<<10)
	_, wErr := io.CopyBuffer(f, in, buf)
	_ = in.Close()
	sErr := f.Sync()
	cErr := f.Close()
	if wErr != nil || sErr != nil || cErr != nil {
		_ = os.Remove(tmp)
		if wErr != nil {
			return wErr
		}
		if sErr != nil {
			return sErr
		}
		return cErr
	}
	_ = os.Chmod(tmp, perm)
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
