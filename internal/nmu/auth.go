package nmu

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/core"
)

const (
	// FeedAuthMaxClockSkewSec enforces a strict +/-60s replay window on X-Nektar-TS for SSE shard feeds.
	FeedAuthMaxClockSkewSec int64 = 60

	// Canonical HTTP headers authenticating subscriber requests to GET /v1/market/feeds/{shard}.
	HeaderSubID = "X-Nektar-Sub-ID"
	HeaderPub   = "X-Nektar-Pub"
	HeaderSig   = "X-Nektar-Sig"
	HeaderTS    = "X-Nektar-TS"
)

var (
	feedAuthDomainTag = []byte("nektar:feed-auth:v1")

	// ErrFeedAuthExpired is returned when X-Nektar-TS falls outside the +/-60s replay window.
	ErrFeedAuthExpired = errors.New("nmu: feed auth timestamp outside replay window")
	// ErrFeedAuthInvalid is returned when the subscriber signature or headers are malformed or invalid.
	ErrFeedAuthInvalid = errors.New("nmu: feed auth signature invalid")
)

// FeedAuthCanonicalMessage returns the exact canonical string signed by X-Nektar-Sig:
// GET|/v1/market/feeds/{shard}|{subId}|{ts}
func FeedAuthCanonicalMessage(shard, subID string, tsUnix int64) string {
	return fmt.Sprintf("GET|/v1/market/feeds/%s|%s|%d", strings.TrimSpace(shard), strings.TrimSpace(subID), tsUnix)
}

func feedAuthDigest(shard, subID string, tsUnix int64) []byte {
	msg := FeedAuthCanonicalMessage(shard, subID, tsUnix)
	return core.DSHA256(core.Concat(feedAuthDomainTag, []byte(msg)))
}

// SignFeedAuth signs GET|/v1/market/feeds/{shard}|{subId}|{ts} with priv and returns
// the compressed 33-byte hex public key and low-S DER hex signature.
func SignFeedAuth(priv *ec.PrivateKey, shard, subID string, tsUnix int64) (pubHex, sigHex string, err error) {
	if priv == nil {
		return "", "", errors.New("nmu: signing key required")
	}
	if strings.TrimSpace(shard) == "" || strings.TrimSpace(subID) == "" || tsUnix <= 0 {
		return "", "", errors.New("nmu: shard, subId, and positive tsUnix are required")
	}
	digest := feedAuthDigest(shard, subID, tsUnix)
	sig, err := priv.Sign(digest)
	if err != nil {
		return "", "", err
	}
	return hex.EncodeToString(priv.PubKey().Compressed()), hex.EncodeToString(sig.Serialize()), nil
}

// VerifyFeedAuth validates the +/-60s replay window and verifies the subscriber's strict compressed
// pubkey and low-S DER ECDSA signature over GET|/v1/market/feeds/{shard}|{subId}|{ts}.
func VerifyFeedAuth(pubHex, sigHex, shard, subID string, tsUnix, nowUnix int64) error {
	if strings.TrimSpace(shard) == "" || strings.TrimSpace(subID) == "" || tsUnix <= 0 {
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
	digest := feedAuthDigest(shard, subID, tsUnix)
	if !sig.Verify(digest, pub) {
		return ErrFeedAuthInvalid
	}
	return nil
}
