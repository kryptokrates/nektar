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
	archiveMagic          = "NKTBAK01"
	archiveVersion        = 1
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

// BackupNow takes an atomic WAL-safe snapshot, compresses with zstd, envelope-encrypts via BRC-42 + AES-256-GCM,
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

// BackupToSink creates an encrypted snapshot and uploads it to an explicit Sink (used by CLI `nektar backup now`).
func (m *Manager) BackupToSink(ctx context.Context, sink Sink, keep int) (*Result, error) {
	if m == nil || m.cfg.DB == nil || m.cfg.Port == nil {
		return nil, errors.New("backup: DB and chain Port are required")
	}
	if sink == nil {
		return nil, errors.New("backup: sink is required")
	}
	archiveBytes, manifest, err := CreateEncryptedArchive(ctx, m.cfg.DB, m.cfg.Port, m.cfg.DataDir)
	if err != nil {
		return nil, err
	}
	ts := time.Unix(manifest.CreatedAt, 0).UTC().Format("20060102T150405Z")
	idShort := manifest.IdentityPub
	if len(idShort) > 10 {
		idShort = idShort[:10]
	}
	name := fmt.Sprintf("nektar-%s-%s-%s.nktbak", manifest.Network, idShort, ts)
	loc, err := sink.Put(ctx, name, archiveBytes)
	if err != nil {
		return nil, err
	}
	pruned := 0
	if keep > 0 {
		pruned, _ = sink.Prune(ctx, keep)
	}
	sum := sha256.Sum256(archiveBytes)
	return &Result{
		ArchiveName: name,
		Location:    loc,
		SizeBytes:   int64(len(archiveBytes)),
		SHA256:      hex.EncodeToString(sum[:]),
		CreatedAt:   manifest.CreatedAt,
		Pruned:      pruned,
		Manifest:    *manifest,
	}, nil
}

// CreateEncryptedArchive snapshots live SQLite state via VACUUM INTO, packs a deterministic zstd tar,
// and encrypts it with a fresh random 32-byte AES-256-GCM DEK wrapped via BRC-100 wallet.Encrypt (BRC-42 self).
func CreateEncryptedArchive(ctx context.Context, db *store.DB, port *chain.Port, dataDir string) ([]byte, *Manifest, error) {
	if db == nil || port == nil || port.WalletInterface() == nil {
		return nil, nil, errors.New("backup: db, chain port, and wallet interface are required")
	}
	tmpDir, err := os.MkdirTemp("", "nektar-bak-stage-*")
	if err != nil {
		return nil, nil, fmt.Errorf("backup: create temp dir: %w", err)
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

	type archivedFile struct {
		name string
		data []byte
	}
	var members []archivedFile

	// 1. Atomic WAL-safe snapshot of nektar.sqlite (includes all fragments, anchors, receipts, memrepo Git objects, market deals, subscriptions).
	nektarSnap := filepath.Join(tmpDir, "nektar.sqlite")
	if err := db.VacuumInto(ctx, nektarSnap); err != nil {
		return nil, nil, fmt.Errorf("backup: snapshot nektar.sqlite: %w", err)
	}
	nektarBytes, err := os.ReadFile(nektarSnap)
	if err != nil {
		return nil, nil, err
	}
	members = append(members, archivedFile{name: "nektar.sqlite", data: nektarBytes})

	// 2. Atomic WAL-safe snapshot of embedded wallet/wallet.sqlite (UTXOs, baskets, tx BEEFs; NEVER raw identity.json).
	if eng := port.Engine(); eng != nil {
		walletSnap := filepath.Join(tmpDir, "wallet.sqlite")
		if err := eng.SnapshotTo(ctx, walletSnap); err != nil {
			return nil, nil, fmt.Errorf("backup: snapshot wallet.sqlite: %w", err)
		}
		walletBytes, err := os.ReadFile(walletSnap)
		if err != nil {
			return nil, nil, err
		}
		members = append(members, archivedFile{name: "wallet/wallet.sqlite", data: walletBytes})
	}

	// 3. Optional spend-guard.json daily spend ledger.
	if strings.TrimSpace(dataDir) != "" {
		sgPath := filepath.Join(dataDir, "spend-guard.json")
		if sgBytes, rErr := os.ReadFile(sgPath); rErr == nil && len(sgBytes) > 0 {
			members = append(members, archivedFile{name: "spend-guard.json", data: sgBytes})
		}
	}

	for _, m := range members {
		sum := sha256.Sum256(m.data)
		manifest.Files[m.name] = FileChecksum{
			Size:   int64(len(m.data)),
			SHA256: hex.EncodeToString(sum[:]),
		}
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	members = append(members, archivedFile{name: "manifest.json", data: manifestBytes})

	// Pack tar and compress with pure-Go zstd.
	var compressed bytes.Buffer
	zw, err := zstd.NewWriter(&compressed, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, nil, fmt.Errorf("backup: zstd writer: %w", err)
	}
	tw := tar.NewWriter(zw)
	for _, m := range members {
		hdr := &tar.Header{
			Name:    m.name,
			Mode:    0o600,
			Size:    int64(len(m.data)),
			ModTime: time.Unix(now, 0).UTC(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = tw.Close()
			_ = zw.Close()
			return nil, nil, err
		}
		if _, err := tw.Write(m.data); err != nil {
			_ = tw.Close()
			_ = zw.Close()
			return nil, nil, err
		}
	}
	if err := tw.Close(); err != nil {
		_ = zw.Close()
		return nil, nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, nil, err
	}

	// Envelope encryption: random 32-byte AES-256-GCM DEK wrapped with BRC-42 wallet.Encrypt (self).
	dek := make([]byte, 32)
	if _, err := crand.Read(dek); err != nil {
		return nil, nil, fmt.Errorf("backup: generate DEK: %w", err)
	}
	defer clear(dek)

	nonce := make([]byte, 12)
	if _, err := crand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("backup: generate nonce: %w", err)
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
		return nil, nil, fmt.Errorf("backup: wallet BRC-42 wrap DEK: %w", err)
	}

	envHdr := EnvelopeHeader{
		Version:       archiveVersion,
		Network:       manifest.Network,
		IdentityPub:   manifest.IdentityPub,
		WalletMode:    manifest.WalletMode,
		SchemaVersion: manifest.SchemaVersion,
		CreatedAt:     manifest.CreatedAt,
		KDF:           "brc42-self-v1",
		Cipher:        "aes-256-gcm",
		Compression:   "zstd",
		WrappedDEKB64: base64.StdEncoding.EncodeToString(wrapRes.Ciphertext),
		NonceB64:      base64.StdEncoding.EncodeToString(nonce),
	}
	hdrJSON, err := json.Marshal(envHdr)
	if err != nil {
		return nil, nil, err
	}

	blk, err := aes.NewCipher(dek)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, nil, err
	}
	ciphertext := gcm.Seal(nil, nonce, compressed.Bytes(), hdrJSON)

	var out bytes.Buffer
	out.Grow(len(archiveMagic) + 4 + len(hdrJSON) + len(ciphertext))
	out.WriteString(archiveMagic)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(hdrJSON)))
	out.Write(lenBuf[:])
	out.Write(hdrJSON)
	out.Write(ciphertext)

	return out.Bytes(), manifest, nil
}

// decryptAndUnpack decrypts, decompresses, verifies manifest SHA-256 checksums, and returns extracted files.
func decryptAndUnpack(ctx context.Context, wallet sdkwallet.Interface, archiveBytes []byte) (*EnvelopeHeader, *Manifest, map[string][]byte, error) {
	if wallet == nil {
		return nil, nil, nil, errors.New("backup: wallet interface required to decrypt archive")
	}
	if len(archiveBytes) < len(archiveMagic)+4 || string(archiveBytes[:len(archiveMagic)]) != archiveMagic {
		return nil, nil, nil, errors.New("backup: invalid archive magic header")
	}
	hdrLen := binary.BigEndian.Uint32(archiveBytes[len(archiveMagic) : len(archiveMagic)+4])
	hdrStart := len(archiveMagic) + 4
	if hdrLen == 0 || int(hdrLen) > 1<<20 || hdrStart+int(hdrLen) >= len(archiveBytes) {
		return nil, nil, nil, errors.New("backup: malformed envelope header length")
	}
	hdrJSON := archiveBytes[hdrStart : hdrStart+int(hdrLen)]
	ciphertext := archiveBytes[hdrStart+int(hdrLen):]

	var hdr EnvelopeHeader
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		return nil, nil, nil, fmt.Errorf("backup: parse envelope header: %w", err)
	}
	if hdr.Version != archiveVersion || hdr.Cipher != "aes-256-gcm" || hdr.Compression != "zstd" {
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
	compressed, err := gcm.Open(nil, nonce, ciphertext, hdrJSON)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("backup: AES-256-GCM authentication failed (tampered or wrong key): %w", err)
	}

	zr, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("backup: zstd reader: %w", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	files := make(map[string][]byte)
	for {
		th, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("backup: read tar entry: %w", err)
		}
		cleanName := filepath.ToSlash(filepath.Clean(th.Name))
		if strings.HasPrefix(cleanName, "..") || strings.HasPrefix(cleanName, "/") {
			return nil, nil, nil, fmt.Errorf("backup: illegal archive entry path %q", th.Name)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, nil, nil, err
		}
		files[cleanName] = data
	}

	manifestRaw, ok := files["manifest.json"]
	if !ok {
		return nil, nil, nil, errors.New("backup: archive missing manifest.json")
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return nil, nil, nil, fmt.Errorf("backup: parse manifest.json: %w", err)
	}
	for name, expected := range manifest.Files {
		data, exists := files[name]
		if !exists {
			return nil, nil, nil, fmt.Errorf("backup: archive missing expected file %q", name)
		}
		if int64(len(data)) != expected.Size {
			return nil, nil, nil, fmt.Errorf("backup: size mismatch for %q (%d != %d)", name, len(data), expected.Size)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != expected.SHA256 {
			return nil, nil, nil, fmt.Errorf("backup: SHA-256 checksum mismatch for %q", name)
		}
	}
	return &hdr, &manifest, files, nil
}

// VerifyArchive decrypts, checks GCM authentication & SHA-256 checksums, and runs SQLite PRAGMA integrity_check
// on every archived database inside a temporary sandbox directory.
func VerifyArchive(ctx context.Context, wallet sdkwallet.Interface, archiveBytes []byte) (*Manifest, error) {
	_, manifest, files, err := decryptAndUnpack(ctx, wallet, archiveBytes)
	if err != nil {
		return nil, err
	}
	tmpDir, err := os.MkdirTemp("", "nektar-bak-verify-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	for _, dbName := range []string{"nektar.sqlite", "wallet/wallet.sqlite"} {
		data, ok := files[dbName]
		if !ok {
			continue
		}
		dst := filepath.Join(tmpDir, filepath.Base(dbName))
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return nil, err
		}
		if err := sqliteIntegrityCheck(ctx, dst); err != nil {
			return nil, fmt.Errorf("backup: integrity_check failed for %s: %w", dbName, err)
		}
	}
	return manifest, nil
}

// RestoreArchive verifies an encrypted .nktbak archive end-to-end and atomically restores its SQLite
// databases and spend-guard state into targetDataDir. Caller must ensure targetDataDir is not locked by a live daemon.
func RestoreArchive(ctx context.Context, wallet sdkwallet.Interface, archiveBytes []byte, targetDataDir string) (*Manifest, error) {
	if strings.TrimSpace(targetDataDir) == "" {
		return nil, errors.New("backup: target data directory is required")
	}
	_, manifest, files, err := decryptAndUnpack(ctx, wallet, archiveBytes)
	if err != nil {
		return nil, err
	}
	tmpDir, err := os.MkdirTemp("", "nektar-bak-restore-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	// Stage and verify every SQLite DB before modifying targetDataDir (fail-closed!).
	for _, dbName := range []string{"nektar.sqlite", "wallet/wallet.sqlite"} {
		data, ok := files[dbName]
		if !ok {
			continue
		}
		staged := filepath.Join(tmpDir, filepath.Base(dbName))
		if err := os.WriteFile(staged, data, 0o600); err != nil {
			return nil, err
		}
		if err := sqliteIntegrityCheck(ctx, staged); err != nil {
			return nil, fmt.Errorf("backup: integrity_check failed for %s: %w", dbName, err)
		}
	}

	if err := os.MkdirAll(targetDataDir, 0o700); err != nil {
		return nil, err
	}
	allowedTargets := []string{"nektar.sqlite", "wallet/wallet.sqlite", "spend-guard.json"}
	for _, rel := range allowedTargets {
		data, ok := files[rel]
		if !ok {
			continue
		}
		dst := filepath.Join(targetDataDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, err
		}
		if err := atomicWriteFile(dst, data, 0o600); err != nil {
			return nil, fmt.Errorf("backup: write %s: %w", rel, err)
		}
		// Remove stale WAL/SHM sidecars for restored SQLite files so SQLite opens cleanly.
		if strings.HasSuffix(rel, ".sqlite") {
			_ = os.Remove(dst + "-wal")
			_ = os.Remove(dst + "-shm")
		}
	}
	return manifest, nil
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

func atomicWriteFile(dst string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(dst)
	f, err := os.CreateTemp(dir, ".restore-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, wErr := f.Write(data)
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
