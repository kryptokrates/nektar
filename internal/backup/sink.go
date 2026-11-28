package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
)

// ObjectInfo describes one stored encrypted backup archive in a Sink.
type ObjectInfo struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
}

// Sink is the storage backend interface for encrypted Nektar backup archives (.nktbak).
type Sink interface {
	Name() string
	Put(ctx context.Context, key string, data []byte) (location string, err error)
	Get(ctx context.Context, key string) ([]byte, error)
	List(ctx context.Context) ([]ObjectInfo, error)
	Prune(ctx context.Context, keep int) (deleted int, err error)
}

// LocalDirSink writes encrypted .nktbak files atomically to a local directory with 0o600 permissions.
type LocalDirSink struct {
	Dir string
}

// NewLocalDirSink returns a LocalDirSink rooted at dir.
func NewLocalDirSink(dir string) (*LocalDirSink, error) {
	clean := strings.TrimSpace(dir)
	if clean == "" {
		return nil, errors.New("backup: local backup directory is required")
	}
	if err := os.MkdirAll(clean, 0o700); err != nil {
		return nil, fmt.Errorf("backup: mkdir %s: %w", clean, err)
	}
	return &LocalDirSink{Dir: filepath.Clean(clean)}, nil
}

func (s *LocalDirSink) Name() string { return "local:" + s.Dir }

func (s *LocalDirSink) Put(_ context.Context, key string, data []byte) (string, error) {
	base := filepath.Base(strings.TrimSpace(key))
	if base == "" || base == "." || base == ".." {
		return "", errors.New("backup: invalid backup key")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(s.Dir, base)
	tmpFile, err := os.CreateTemp(s.Dir, ".nktbak-*.tmp")
	if err != nil {
		return "", fmt.Errorf("backup: create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	_, wErr := tmpFile.Write(data)
	sErr := tmpFile.Sync()
	cErr := tmpFile.Close()
	if wErr != nil || sErr != nil || cErr != nil {
		_ = os.Remove(tmpPath)
		if wErr != nil {
			return "", wErr
		}
		if sErr != nil {
			return "", sErr
		}
		return "", cErr
	}
	_ = os.Chmod(tmpPath, 0o600)
	if err := os.Rename(tmpPath, dst); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("backup: atomic rename: %w", err)
	}
	return dst, nil
}

// PutFile streams srcPath into s.Dir atomically in O(1) RAM without loading the entire archive into memory.
func (s *LocalDirSink) PutFile(_ context.Context, key, srcPath string) (string, error) {
	base := filepath.Base(strings.TrimSpace(key))
	if base == "" || base == "." || base == ".." {
		return "", errors.New("backup: invalid backup key")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return "", err
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer in.Close()

	dst := filepath.Join(s.Dir, base)
	tmpFile, err := os.CreateTemp(s.Dir, ".nktbak-*.tmp")
	if err != nil {
		return "", fmt.Errorf("backup: create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	buf := make([]byte, 64<<10)
	_, cpyErr := io.CopyBuffer(tmpFile, in, buf)
	sErr := tmpFile.Sync()
	cErr := tmpFile.Close()
	if cpyErr != nil || sErr != nil || cErr != nil {
		_ = os.Remove(tmpPath)
		if cpyErr != nil {
			return "", cpyErr
		}
		if sErr != nil {
			return "", sErr
		}
		return "", cErr
	}
	_ = os.Chmod(tmpPath, 0o600)
	if err := os.Rename(tmpPath, dst); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("backup: atomic rename: %w", err)
	}
	return dst, nil
}

func (s *LocalDirSink) Get(_ context.Context, key string) ([]byte, error) {
	base := filepath.Base(strings.TrimSpace(key))
	if base == "" || base == "." || base == ".." {
		return nil, errors.New("backup: invalid backup key")
	}
	return os.ReadFile(filepath.Join(s.Dir, base))
}

func (s *LocalDirSink) List(_ context.Context) ([]ObjectInfo, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []ObjectInfo
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".nktbak") {
			continue
		}
		fi, fErr := ent.Info()
		if fErr != nil {
			continue
		}
		out = append(out, ObjectInfo{
			Key:          ent.Name(),
			Size:         fi.Size(),
			LastModified: fi.ModTime().UTC(),
		})
	}
	// Sort newest-first by archive filename (timestamp-prefixed), then ModTime.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key > out[j].Key
		}
		return out[i].LastModified.After(out[j].LastModified)
	})
	return out, nil
}

func (s *LocalDirSink) Prune(ctx context.Context, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	objs, err := s.List(ctx)
	if err != nil || len(objs) <= keep {
		return 0, err
	}
	deleted := 0
	for _, obj := range objs[keep:] {
		if err := os.Remove(filepath.Join(s.Dir, filepath.Base(obj.Key))); err == nil {
			deleted++
		}
	}
	return deleted, nil
}

// S3Config configures the lightweight pure-Go stdlib AWS SigV4 S3/R2/B2/MinIO Sink (zero extra deps).
type S3Config struct {
	Endpoint   string // e.g. https://s3.us-east-1.amazonaws.com or https://<account>.r2.cloudflarestorage.com
	Bucket     string
	Region     string // default "auto" or "us-east-1"
	Prefix     string // optional key prefix (e.g. "nektar-backups/")
	AccessKey  string
	SecretKey  string
	HTTPClient *http.Client
	Now        func() time.Time
}

// S3Sink implements Sink over any S3-compatible object store using stdlib AWS Signature Version 4.
type S3Sink struct {
	cfg S3Config
	hc  *http.Client
	now func() time.Time
}

// NewS3Sink constructs a path-style AWS SigV4 S3/R2/B2/MinIO Sink.
func NewS3Sink(cfg S3Config, network string) (*S3Sink, error) {
	cfg.Endpoint = strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	cfg.Region = strings.TrimSpace(cfg.Region)
	cfg.Prefix = strings.Trim(strings.TrimSpace(cfg.Prefix), "/")
	cfg.AccessKey = strings.TrimSpace(cfg.AccessKey)
	cfg.SecretKey = strings.TrimSpace(cfg.SecretKey)
	if cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("backup: S3 sink requires bucket, access key, and secret key")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = fmt.Sprintf("https://s3.%s.amazonaws.com", cfg.Region)
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = core.SafeHTTPClientForNetwork(network, 60*time.Second)
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &S3Sink{cfg: cfg, hc: hc, now: nowFn}, nil
}

func (s *S3Sink) Name() string {
	if s.cfg.Prefix != "" {
		return fmt.Sprintf("s3://%s/%s", s.cfg.Bucket, s.cfg.Prefix)
	}
	return fmt.Sprintf("s3://%s", s.cfg.Bucket)
}

func (s *S3Sink) fullKey(key string) string {
	k := strings.TrimLeft(strings.TrimSpace(key), "/")
	if s.cfg.Prefix != "" && !strings.HasPrefix(k, s.cfg.Prefix+"/") {
		return s.cfg.Prefix + "/" + k
	}
	return k
}

func (s *S3Sink) objectURL(fullKey string) string {
	return s.cfg.Endpoint + "/" + url.PathEscape(s.cfg.Bucket) + "/" + encodeS3Path(fullKey)
}

func encodeS3Path(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = awsURIEncode(part)
	}
	return strings.Join(parts, "/")
}

func awsURIEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func (s *S3Sink) signAndDo(ctx context.Context, method, rawURL string, body []byte, contentType string) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	payloadHash := sha256Hex(body)
	now := s.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
	if err != nil {
		return nil, err
	}
	req.Host = u.Host
	req.Header.Set("Host", u.Host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	canonicalURI := u.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalQuery := canonicalizeS3Query(u.Query())
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", u.Host, payloadHash, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalReq := strings.Join([]string{
		method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, s.cfg.Region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalReq)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+s.cfg.SecretKey), dateStamp)
	kRegion := hmacSHA256(kDate, s.cfg.Region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	auth := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.cfg.AccessKey, scope, signedHeaders, sig)
	req.Header.Set("Authorization", auth)

	return s.hc.Do(req)
}

func canonicalizeS3Query(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var pairs []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			pairs = append(pairs, awsURIEncode(k)+"="+awsURIEncode(v))
		}
	}
	return strings.Join(pairs, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *S3Sink) Put(ctx context.Context, key string, data []byte) (string, error) {
	fk := s.fullKey(key)
	resp, err := s.signAndDo(ctx, http.MethodPut, s.objectURL(fk), data, "application/octet-stream")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("backup: S3 PUT HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return fmt.Sprintf("s3://%s/%s", s.cfg.Bucket, fk), nil
}

func (s *S3Sink) Get(ctx context.Context, key string) ([]byte, error) {
	fk := s.fullKey(key)
	resp, err := s.signAndDo(ctx, http.MethodGet, s.objectURL(fk), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("backup: S3 GET HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return io.ReadAll(resp.Body)
}

type listBucketResultXML struct {
	Contents []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
}

func (s *S3Sink) List(ctx context.Context) ([]ObjectInfo, error) {
	u := s.cfg.Endpoint + "/" + url.PathEscape(s.cfg.Bucket) + "?list-type=2"
	if s.cfg.Prefix != "" {
		u += "&prefix=" + awsURIEncode(s.cfg.Prefix+"/")
	}
	resp, err := s.signAndDo(ctx, http.MethodGet, u, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backup: S3 LIST HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed listBucketResultXML
	if err := xml.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("backup: parse S3 list XML: %w", err)
	}
	var out []ObjectInfo
	for _, c := range parsed.Contents {
		if !strings.HasSuffix(c.Key, ".nktbak") {
			continue
		}
		lm, _ := time.Parse(time.RFC3339, c.LastModified)
		out = append(out, ObjectInfo{
			Key:          c.Key,
			Size:         c.Size,
			LastModified: lm.UTC(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key > out[j].Key
		}
		return out[i].LastModified.After(out[j].LastModified)
	})
	return out, nil
}

func (s *S3Sink) Prune(ctx context.Context, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	objs, err := s.List(ctx)
	if err != nil || len(objs) <= keep {
		return 0, err
	}
	deleted := 0
	for _, obj := range objs[keep:] {
		resp, dErr := s.signAndDo(ctx, http.MethodDelete, s.objectURL(obj.Key), nil, "")
		if dErr != nil {
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			deleted++
		}
	}
	return deleted, nil
}

// NewSinkFromEnv resolves the configured backup Sink from NEKTAR_BACKUP_S3_BUCKET or NEKTAR_BACKUP_DIR.
// Returns (nil, nil) if neither backup target is configured.
func NewSinkFromEnv(network string) (Sink, error) {
	if bucket := strings.TrimSpace(os.Getenv("NEKTAR_BACKUP_S3_BUCKET")); bucket != "" {
		return NewS3Sink(S3Config{
			Endpoint:  os.Getenv("NEKTAR_BACKUP_S3_ENDPOINT"),
			Bucket:    bucket,
			Region:    os.Getenv("NEKTAR_BACKUP_S3_REGION"),
			Prefix:    os.Getenv("NEKTAR_BACKUP_S3_PREFIX"),
			AccessKey: os.Getenv("NEKTAR_BACKUP_S3_ACCESS_KEY"),
			SecretKey: os.Getenv("NEKTAR_BACKUP_S3_SECRET_KEY"),
		}, network)
	}
	if dir := strings.TrimSpace(os.Getenv("NEKTAR_BACKUP_DIR")); dir != "" {
		return NewLocalDirSink(dir)
	}
	return nil, nil
}
