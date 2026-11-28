package nmu

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/lightwebinc/bcommon/cbor"

	"github.com/kryptokrates/nektar/internal/core"
)

const (
	// FeedAuthMaxClockSkewSec enforces a strict +/-60s replay window on X-Nektar-Ts for SSE shard feeds.
	FeedAuthMaxClockSkewSec int64 = 60

	// Canonical HTTP headers authenticating subscriber requests to GET /v1/market/feeds/{shard}
	// (unified with Hub egress & bbox X-Nektar-* headers).
	HeaderSubID = "X-Nektar-Sub-ID"
	HeaderPub   = "X-Nektar-Pub"
	HeaderSig   = "X-Nektar-Sig"
	HeaderTS    = "X-Nektar-Ts"
	HeaderNonce = "X-Nektar-Nonce"
)

var (
	// ErrFeedAuthExpired is returned when X-Nektar-Ts falls outside the +/-60s replay window.
	ErrFeedAuthExpired = errors.New("nmu: feed auth timestamp outside replay window")
	// ErrFeedAuthInvalid is returned when the subscriber signature or headers are malformed or invalid.
	ErrFeedAuthInvalid = errors.New("nmu: feed auth signature invalid")
	// ErrFeedAuthReplay is returned when an X-Nektar-Nonce is reused within the replay window.
	ErrFeedAuthReplay = errors.New("nmu: feed auth nonce replayed")
)

func feedAuthCBOR(shard, subID string, tsUnix int64, nonce, aud string) ([]byte, error) {
	return core.EncodeCBOR(cbor.Map{
		{Key: uint64(1), Val: strings.TrimSpace(shard)},
		{Key: uint64(2), Val: strings.TrimSpace(subID)},
		{Key: uint64(3), Val: tsUnix},
		{Key: uint64(4), Val: strings.TrimSpace(nonce)},
		{Key: uint64(5), Val: strings.ToLower(strings.TrimSpace(aud))},
	})
}

func feedAuthDigest(shard, subID string, tsUnix int64, nonce, aud string) ([]byte, error) {
	raw, err := feedAuthCBOR(shard, subID, tsUnix, nonce, aud)
	if err != nil {
		return nil, err
	}
	return core.DSHA256(core.Concat(core.TagFeedAuthV1, raw)), nil
}

// NewFeedNonce generates a random 12-byte lowercase hex replay-protection nonce.
func NewFeedNonce() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// SignFeedAuth signs canonical CBOR feed auth bound to (shard, subID, tsUnix, nonce, aud)
// under TagFeedAuthV1 and returns compressed 33-byte hex pubkey + low-S DER hex signature.
func SignFeedAuth(priv *ec.PrivateKey, shard, subID string, tsUnix int64, nonce, aud string) (pubHex, sigHex string, err error) {
	if priv == nil {
		return "", "", errors.New("nmu: signing key required")
	}
	return SignFeedAuthWithSigner(
		hex.EncodeToString(priv.PubKey().Compressed()),
		func(digest []byte) ([]byte, error) {
			sig, sErr := priv.Sign(digest)
			if sErr != nil {
				return nil, sErr
			}
			return sig.Serialize(), nil
		},
		shard, subID, tsUnix, nonce, aud,
	)
}

// SignFeedAuthWithSigner signs canonical CBOR feed auth using an abstract digest signer so callers
// outside internal/wallet never touch raw *ec.PrivateKey.
func SignFeedAuthWithSigner(pubHex string, signDigest func([]byte) ([]byte, error), shard, subID string, tsUnix int64, nonce, aud string) (string, string, error) {
	if strings.TrimSpace(pubHex) == "" || signDigest == nil {
		return "", "", errors.New("nmu: signing key required")
	}
	if strings.TrimSpace(shard) == "" || strings.TrimSpace(subID) == "" || tsUnix <= 0 || strings.TrimSpace(nonce) == "" {
		return "", "", errors.New("nmu: shard, subId, nonce, and positive tsUnix are required")
	}
	digest, err := feedAuthDigest(shard, subID, tsUnix, nonce, aud)
	if err != nil {
		return "", "", err
	}
	der, err := signDigest(digest)
	if err != nil {
		return "", "", err
	}
	return strings.ToLower(strings.TrimSpace(pubHex)), hex.EncodeToString(der), nil
}

// VerifyFeedAuth validates the +/-60s replay window and verifies the subscriber's strict compressed
// pubkey and low-S DER ECDSA signature over canonical CBOR (shard, subID, tsUnix, nonce, aud).
func VerifyFeedAuth(pubHex, sigHex, shard, subID string, tsUnix, nowUnix int64, nonce, aud string) error {
	if strings.TrimSpace(shard) == "" || strings.TrimSpace(subID) == "" || tsUnix <= 0 || strings.TrimSpace(nonce) == "" {
		return ErrFeedAuthInvalid
	}
	skew := nowUnix - tsUnix
	if skew < -FeedAuthMaxClockSkewSec || skew > FeedAuthMaxClockSkewSec {
		return ErrFeedAuthExpired
	}
	pub, _, err := core.ParseStrictCompressedPubKey(pubHex)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFeedAuthInvalid, err)
	}
	sig, err := core.ParseStrictLowSDER(sigHex)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFeedAuthInvalid, err)
	}
	digest, err := feedAuthDigest(shard, subID, tsUnix, nonce, aud)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFeedAuthInvalid, err)
	}
	if !sig.Verify(digest, pub) {
		return ErrFeedAuthInvalid
	}
	return nil
}

const maxFeedNonceEntries = 65536

type feedNonceEntry struct {
	key string
	ts  int64
}

// FeedNonceCache is an O(1) FIFO-evicting bounded TTL replay cache for SSE feed authentication nonces.
type FeedNonceCache struct {
	mu    sync.Mutex
	seen  map[string]int64
	order []feedNonceEntry
	head  int
}

// Claim records (pubHex, nonce) at nowUnix and returns false if already claimed within the replay window.
func (c *FeedNonceCache) Claim(pubHex, nonce string, nowUnix int64) bool {
	if c == nil {
		return true
	}
	key := strings.ToLower(strings.TrimSpace(pubHex)) + ":" + strings.TrimSpace(nonce)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = make(map[string]int64, 256)
	}
	cutoff := nowUnix - FeedAuthMaxClockSkewSec*2
	// Pop expired entries from the chronological head in amortized O(1).
	for c.head < len(c.order) && c.order[c.head].ts < cutoff {
		old := c.order[c.head]
		c.order[c.head] = feedNonceEntry{}
		c.head++
		if curTS, ok := c.seen[old.key]; ok && curTS < cutoff {
			delete(c.seen, old.key)
		}
	}
	// Evict oldest entries if hard capacity is still exceeded under burst flood.
	for len(c.seen) >= maxFeedNonceEntries && c.head < len(c.order) {
		old := c.order[c.head]
		c.order[c.head] = feedNonceEntry{}
		c.head++
		delete(c.seen, old.key)
	}
	if c.head > 0 && c.head*2 >= len(c.order) {
		rem := copy(c.order, c.order[c.head:])
		c.order = c.order[:rem]
		c.head = 0
	}
	if ts, exists := c.seen[key]; exists && ts >= cutoff {
		return false
	}
	c.seen[key] = nowUnix
	c.order = append(c.order, feedNonceEntry{key: key, ts: nowUnix})
	return true
}
