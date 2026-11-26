package bstack

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/core"
)

// Dynamic Topic Addressing & Sealed-Payment Carriers (1BSV Paper 010 §2, §6 & Paper 011 §5.4–6).
const (
	// SealedPaymentMinLifetime enforces Paper 011 Fig. 11: a sealed payment carrier lives at least
	// 2 hours so the payee has a safe claim window before expiry-1h and the payer reclaims only
	// after expiry+1h (a 2-hour quiet buffer where neither side acts).
	SealedPaymentMinLifetime = 2 * time.Hour
	SealedPaymentGuardBuffer = 1 * time.Hour

	// KindSealedPayment is a carrier payload kind carrying an E2E-encrypted BRC-29 payment / deal
	// message that travels safely across any shared multicast plane (Paper 011 §5.4).
	KindSealedPayment ArtifactKind = "sealed_payment"
)

// DomainTopic returns an exact-domain BRC-148 topic name (`tm_nektar_<net>_d_<domain>`)
// alongside the 16 nibble lanes (Paper 010 §6.1: arbitrary UTF-8 names work with zero registry).
func DomainTopic(net, domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	return fmt.Sprintf("tm_nektar_%s_d_%s", net, d)
}

// DerivedRendezvousTopic derives a private pairwise BRC-148 topic name known only to two
// counterparties using ECDH shared secret + deal/invoice keyID (Paper 010 §6.2 Derivation Privacy).
// An observer sees only the 32-byte TopicID hash sitting among its band's co-residents.
func DerivedRendezvousTopic(net string, selfPriv *ec.PrivateKey, peerPubHex, keyID string) (string, [32]byte, error) {
	var zero [32]byte
	if selfPriv == nil {
		return "", zero, errors.New("bstack: nil private key for rendezvous topic")
	}
	peerPub, _, err := core.ParseStrictCompressedPubKey(peerPubHex)
	if err != nil {
		return "", zero, fmt.Errorf("bstack: invalid peerPub: %w", err)
	}
	shared, err := selfPriv.DeriveSharedSecret(peerPub)
	if err != nil {
		return "", zero, fmt.Errorf("bstack: ecdh rendezvous: %w", err)
	}
	sharedBytes := shared.Compressed()
	defer core.ZeroBytes(sharedBytes)

	tag := sha256.Sum256(core.Concat([]byte("nektar/rendezvous/v1"), sharedBytes, []byte(strings.TrimSpace(keyID))))
	topic := fmt.Sprintf("tm_nektar_%s_r_%s", net, hex.EncodeToString(tag[:16]))
	return topic, sha256.Sum256([]byte(topic)), nil
}

// TopicBand computes the BRC-148 multicast band index `0x1000 + (TopicID[0:4] >> (32-bits))`
// for any topic name at shard width `bits` (0..15), matching 1BSV Paper 010 §2.
func TopicBand(topic string, bits uint8) uint16 {
	if bits == 0 {
		return 0x1000
	}
	if bits > 15 {
		bits = 15
	}
	tid := sha256.Sum256([]byte(topic))
	w := binary.BigEndian.Uint32(tid[0:4])
	return 0x1000 + uint16(w>>(32-bits))
}

// SealedPaymentCarrier is the plaintext structure encrypted inside a cross-hub unmineable
// carrier so unbroadcast BRC-29 Atomic BEEF payments & NAT deal messages travel safely over
// any shared Object Plane without exposing settlement authority to any Hub or third party
// (1BSV Paper 011 §5.4 & Fig. 11).
type SealedPaymentCarrier struct {
	Version      int             `json:"v"`
	DealID       string          `json:"dealId"`
	SenderPub    string          `json:"senderPub"`
	RecipientPub string          `json:"recipientPub"`
	Kind         string          `json:"kind"` // purchase | reveal | ack
	CreatedAt    int64           `json:"createdAt"` // unix sec
	ExpiresAt    int64           `json:"expiresAt"` // unix sec (>= CreatedAt + 2h)
	PaymentBEEF  string          `json:"paymentBeefHex,omitempty"`
	Body         json.RawMessage `json:"body,omitempty"`
}

// SealedPaymentEnvelope is the public ciphertext envelope placed inside an unmineable carrier
// on the recipient's rendezvous or bbox topic. Intermediate Hubs see only ciphertext.
type SealedPaymentEnvelope struct {
	DealID       string          `json:"dealId"`
	SenderPub    string          `json:"senderPub"`
	RecipientPub string          `json:"recipientPub"`
	CreatedAt    int64           `json:"createdAt"`
	ExpiresAt    int64           `json:"expiresAt"`
	Sealed       json.RawMessage `json:"sealed"` // BboxSealedBody (ECDH + HKDF-SHA256 + AES-256-GCM)
	Sig          string          `json:"sig"`
}

// SealPaymentForPlane encrypts msg for msg.RecipientPub and signs the outer envelope, enforcing
// the Paper 011 Fig. 11 minimum 2-hour lifetime window.
func SealPaymentForPlane(senderPriv *ec.PrivateKey, msg SealedPaymentCarrier) (SealedPaymentEnvelope, error) {
	if senderPriv == nil {
		return SealedPaymentEnvelope{}, errors.New("bstack: sender private key required")
	}
	if msg.CreatedAt == 0 {
		msg.CreatedAt = time.Now().Unix()
	}
	if msg.ExpiresAt < msg.CreatedAt+int64(SealedPaymentMinLifetime.Seconds()) {
		return SealedPaymentEnvelope{}, fmt.Errorf("bstack: sealed payment lifetime must be >= %v (Paper 011 §6)", SealedPaymentMinLifetime)
	}
	msg.Version = 2
	plain, err := json.Marshal(msg)
	if err != nil {
		return SealedPaymentEnvelope{}, err
	}
	aad := BboxAAD("plane", msg.SenderPub, msg.RecipientPub, msg.DealID, msg.Kind)
	sealed, err := SealBboxBody(msg.RecipientPub, aad, plain)
	if err != nil {
		return SealedPaymentEnvelope{}, err
	}
	env := SealedPaymentEnvelope{
		DealID:       msg.DealID,
		SenderPub:    msg.SenderPub,
		RecipientPub: msg.RecipientPub,
		CreatedAt:    msg.CreatedAt,
		ExpiresAt:    msg.ExpiresAt,
		Sealed:       sealed,
	}
	sig, err := SignArtifact(senderPriv, env.signable())
	if err != nil {
		return SealedPaymentEnvelope{}, err
	}
	env.Sig = sig
	return env, nil
}

// OpenPaymentFromPlane verifies the outer signature, enforces the payee claim window
// (`now <= ExpiresAt - 1h`, Paper 011 Fig. 11), and decrypts the sealed payment payload.
func OpenPaymentFromPlane(recipientPriv *ec.PrivateKey, env SealedPaymentEnvelope, kind string, now time.Time) (SealedPaymentCarrier, error) {
	if recipientPriv == nil {
		return SealedPaymentCarrier{}, errors.New("bstack: recipient private key required")
	}
	if err := VerifyArtifact(env.SenderPub, env.Sig, env.signable()); err != nil {
		return SealedPaymentCarrier{}, fmt.Errorf("bstack: sealed payment signature invalid: %w", err)
	}
	if !PayeeCanClaimSealedPayment(env.CreatedAt, env.ExpiresAt, now) {
		return SealedPaymentCarrier{}, errors.New("bstack: outside payee claim window (must be <= expires - 1h per Paper 011 Fig. 11)")
	}
	aad := BboxAAD("plane", env.SenderPub, env.RecipientPub, env.DealID, kind)
	plain, err := OpenBboxBody(recipientPriv, aad, env.Sealed)
	if err != nil {
		return SealedPaymentCarrier{}, err
	}
	var out SealedPaymentCarrier
	if err := json.Unmarshal(plain, &out); err != nil {
		return SealedPaymentCarrier{}, err
	}
	return out, nil
}

// PayeeCanClaimSealedPayment reports whether now is inside the safe payee broadcast window
// (Paper 011 Fig. 11: expires - created >= 2h AND now <= expires - 1h).
func PayeeCanClaimSealedPayment(createdAt, expiresAt int64, now time.Time) bool {
	if expiresAt-createdAt < int64(SealedPaymentMinLifetime.Seconds()) {
		return false
	}
	return now.Unix() <= expiresAt-int64(SealedPaymentGuardBuffer.Seconds())
}

// PayerCanReclaimSealedPayment reports whether now is past the 2-hour quiet buffer so the payer
// may safely reclaim unbroadcast payment coins (Paper 011 Fig. 11: now >= expires + 1h).
func PayerCanReclaimSealedPayment(createdAt, expiresAt int64, now time.Time) bool {
	if expiresAt-createdAt < int64(SealedPaymentMinLifetime.Seconds()) {
		return false
	}
	return now.Unix() >= expiresAt+int64(SealedPaymentGuardBuffer.Seconds())
}

func (e SealedPaymentEnvelope) signable() map[string]any {
	return map[string]any{
		"action":       "sealed_payment_carrier",
		"dealId":       e.DealID,
		"senderPub":    e.SenderPub,
		"recipientPub": e.RecipientPub,
		"createdAt":    e.CreatedAt,
		"expiresAt":    e.ExpiresAt,
		"sealed":       string(e.Sealed),
	}
}
