package bstack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/pushdrop"
	"github.com/lightwebinc/bcommon/record"

	"github.com/kryptokrates/nektar/internal/core"
)

const (
	MaxPayload    = 16384
	MaxDomains    = 3
	MaxLifetime   = 7 * 24 * time.Hour
	LookupService = "ls_nektar"
	DirDomain     = "dir"
	Originator    = "nektar"
)

var (
	MarketMagic = []byte("nkt1")
	FundingTag  = []byte("nektar-fund")

	// MarketCarrierParams pins Nektar's PushDrop carrier derivation & tag on Lightweb's bcommon/carrier.
	MarketCarrierParams = carrier.Params{
		Derivation: pushdrop.Derivation{
			Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryAppAndCounterparty, Protocol: "nektar market"},
			KeyID:    "1",
		},
		FundingTag: FundingTag,
		ValidatePayload: func(b []byte) error {
			_, err := DecodePayload(b)
			return err
		},
	}
)

type ArtifactKind string

const (
	KindWant        ArtifactKind = "want"
	KindOffer       ArtifactKind = "offer"
	KindAnnounce    ArtifactKind = "announce"
	KindAttestation ArtifactKind = "attestation"
)

// MarketPayload is the canonical nkt1 pure RFC 8949 CDE CBOR envelope riding on tm_nektar_<net>_<nibble> lanes.
type MarketPayload struct {
	Kind  ArtifactKind `json:"kind"`
	Body  []byte       `json:"body"`            // exact canonical RFC 8949 CDE CBOR bytes of Signed* envelope
	Extra []cbor.Pair  `json:"extra,omitempty"` // preserved non-critical odd extension keys
}

// SignedWant advertises a buyer's knowledge request on the market lanes.
type SignedWant struct {
	Want    WantAd `json:"want"`
	Sig     string `json:"sig"`
	RawCBOR []byte `json:"-"`
}

type WantAd struct {
	WantID            string   `json:"wantId"`
	Pub               string   `json:"pub"`
	Net               string   `json:"net"`
	Domains           []string `json:"domains"`
	Query             string   `json:"query"`
	OntologyID        string   `json:"ontologyId,omitempty"`        // optional 32-byte hex committed TBox OntologyID
	MaxSats           uint64   `json:"maxSats"`
	MinGrade          string   `json:"minGrade,omitempty"`          // NMU-1 minimum grade: C | B | A | S
	MaxSatsPer1kUnits uint64   `json:"maxSatsPer1kUnits,omitempty"` // NMU-1 max normalized unit price (milli-sats/triple)
	FreshnessClass    string   `json:"freshnessClass,omitempty"`    // NMU-1: ephemeral | versioned | evergreen
	RequiredRights    string   `json:"requiredRights,omitempty"`    // NMU-1: inference_only | distill_resellable
	PricingMode       string   `json:"pricingMode,omitempty"`       // NMU-1: spot | per_query | subscription
	Revision          int      `json:"revision"`
	Exp               int64    `json:"exp"` // unix ms
}

// SignedOffer advertises a seller's signed quote answering a WantAd.
type SignedOffer struct {
	Offer   OfferAd `json:"offer"`
	Sig     string  `json:"sig"`
	RawCBOR []byte  `json:"-"`
}

type OfferAd struct {
	OfferID         string          `json:"offerId"`
	WantID          string          `json:"wantId"`
	Pub             string          `json:"pub"`
	BuyerPub        string          `json:"buyerPub"`
	Net             string          `json:"net"`
	Domains         []string        `json:"domains"`
	PriceSats       uint64          `json:"priceSats"`
	Grade           string          `json:"grade,omitempty"`           // NMU-1 commodity grade: C | B | A | S
	UnitCount       uint32          `json:"unitCount,omitempty"`       // NMU-1 verified unique triples (or 1 text unit for C)
	SatsPer1kUnits  uint64          `json:"satsPer1kUnits,omitempty"`  // NMU-1 exact (priceSats*1000)/max(1,unitCount)
	FreshnessClass  string          `json:"freshnessClass,omitempty"`  // NMU-1: ephemeral | versioned | evergreen
	ValidUntilUnix  int64           `json:"validUntilUnix,omitempty"`  // NMU-1 unix seconds TTL for ephemeral units
	SubjectVersion  string          `json:"subjectVersion,omitempty"`  // NMU-1 version pin (e.g. "go-1.27.4")
	UsageRights     string          `json:"usageRights,omitempty"`     // NMU-1: inference_only | distill_resellable
	PricingMode     string          `json:"pricingMode,omitempty"`     // NMU-1: spot | per_query | subscription
	SubscriptionSec int64           `json:"subscriptionSec,omitempty"` // NMU-1 duration seconds when pricingMode==subscription
	FeedURL         string          `json:"feedUrl,omitempty"`         // NMU-1 live SSE supersession endpoint (Grade S / subscription)
	Shard           string          `json:"shard,omitempty"`
	Leaf            string          `json:"leaf,omitempty"`
	OntologyID      string          `json:"ontologyId,omitempty"` // optional 32-byte hex TBox OntologyID
	EdgesRoot       string          `json:"edgesRoot,omitempty"`  // optional 32-byte hex accepted-edge Merkle root
	ChirpURL        string          `json:"chirpUrl,omitempty"`
	KeyCommitHex    string          `json:"keyCommitHex,omitempty"`
	PayAddress      string          `json:"payAddress,omitempty"`
	PayIdentityKey  string          `json:"payIdentityKey,omitempty"`
	SellerURL       string          `json:"sellerUrl,omitempty"`
	BboxOffice      string          `json:"bboxOffice,omitempty"`
	SaleOffer       json.RawMessage `json:"saleOffer,omitempty"`
	Description     string          `json:"description,omitempty"`
	Tags            []string        `json:"tags,omitempty"`
	BondOutpoint    string          `json:"bondOutpoint,omitempty"`
	BondBEEFHex     string          `json:"bondBeefHex,omitempty"`
	IsHub           bool            `json:"isHub,omitempty"`
	Direct          bool            `json:"direct,omitempty"`
	Revision        int             `json:"revision"`
	Exp             int64           `json:"exp"`
	PayloadFormat   string          `json:"payloadFormat,omitempty"` // "nkt" for self-verifying .nkt capsule delivery
}

// PackRef advertises a content-addressed CHIRP shard pack (or ontology/bridge repo) in a signed AnnounceAd.
type PackRef struct {
	Shard                 string   `json:"shard"`
	Domains               []string `json:"domains,omitempty"`
	ChirpURL              string   `json:"chirpUrl"`
	RootHex               string   `json:"rootHex,omitempty"`
	OntologyID            string   `json:"ontologyId,omitempty"`
	TargetOntologyID      string   `json:"targetOntologyId,omitempty"` // populated for cross-ontology Bridge Shards
	Count                 int      `json:"count,omitempty"`
	Grade                 string   `json:"grade,omitempty"`                 // NMU-1 highest provable grade in shard
	UnitCount             uint32   `json:"unitCount,omitempty"`             // NMU-1 verified units per pack/average
	PricingMode           string   `json:"pricingMode,omitempty"`           // primary pricing mode (spot | per_query | subscription)
	EnabledModes          []string `json:"enabledModes,omitempty"`          // independent enabled modes for multi-mode listings
	PriceSats             uint64   `json:"priceSats,omitempty"`             // spot ask in sats
	SatsPer1kUnits        uint64   `json:"satsPer1kUnits,omitempty"`        // normalized spot unit price (milli-sats/triple)
	QueryPriceSats        uint64   `json:"queryPriceSats,omitempty"`        // per_query metered recall ask (BRC-105 402)
	SubscriptionPriceSats uint64   `json:"subscriptionPriceSats,omitempty"` // subscription feed ask
	SubscriptionSec       int64    `json:"subscriptionSec,omitempty"`       // subscription window duration in seconds
	FreshnessClass        string   `json:"freshnessClass,omitempty"`
	UsageRights           string   `json:"usageRights,omitempty"`
	QueryEndpoint         string   `json:"queryEndpoint,omitempty"` // POST /v1/market/query URL
	FeedEndpoint          string   `json:"feedEndpoint,omitempty"`  // GET /v1/market/feeds/{shard} SSE URL
}

// SignedAnnounce advertises an operator/hub node on the enumerable dir lane.
type SignedAnnounce struct {
	Announce AnnounceAd `json:"announce"`
	Sig      string     `json:"sig"`
	RawCBOR  []byte     `json:"-"`
}

// HubFeeAdvert summarizes a Hub operator's advertised fee schedule on the directory lane.
type HubFeeAdvert struct {
	SubmitWantSats         uint64 `json:"submitWantSats,omitempty"`
	SubmitOfferSats        uint64 `json:"submitOfferSats,omitempty"`
	SubmitAnnounceSats     uint64 `json:"submitAnnounceSats,omitempty"`
	BboxSendSats           uint64 `json:"bboxSendSats,omitempty"`
	EgressSatsPer100Events uint64 `json:"egressSatsPer100Events,omitempty"`
	EgressMinTopupSats     uint64 `json:"egressMinTopupSats,omitempty"`
}

type AnnounceAd struct {
	Pub               string        `json:"pub"`
	Net               string        `json:"net"`
	Handle            string        `json:"handle,omitempty"` // bfinger user@domain
	PublicURL         string        `json:"publicUrl,omitempty"`
	BboxOffice        string        `json:"bboxOffice,omitempty"`
	Hosts             []string      `json:"hosts"`
	Domains           []string      `json:"domains,omitempty"`
	SellDomains       []string      `json:"sellDomains,omitempty"`
	BountyDomains     []string      `json:"bountyDomains,omitempty"`
	Packs             []PackRef     `json:"packs,omitempty"`
	BondOutpoint      string        `json:"bondOutpoint,omitempty"`
	BondBEEFHex       string        `json:"bondBeefHex,omitempty"`
	IsHub             bool          `json:"isHub,omitempty"`
	HubRole           string        `json:"hubRole,omitempty"` // spine | leaf
	HubPayIdentityKey string        `json:"hubPayIdentityKey,omitempty"`
	HubTermsURL       string        `json:"hubTermsUrl,omitempty"`
	HubFees           *HubFeeAdvert `json:"hubFees,omitempty"`
	Exp               int64         `json:"exp"`
}

// SignedAttestation advertises a signed bilateral reputation attestation (`nektarrep1.*`) on the overlay.
type SignedAttestation struct {
	Attestation AttestationAd `json:"attestation"`
	Sig         string        `json:"sig"`
	RawCBOR     []byte        `json:"-"`
}

// AttestationAd wraps a signed canonical CBOR `nektarrep1.*` token for overlay gossip & ls_nektar lookup.
type AttestationAd struct {
	AttestHash      string   `json:"attestHash"`
	Role            string   `json:"role"` // buyer_rates_seller | seller_rates_buyer
	Pub             string   `json:"pub"`  // issuer compressed pubkey (33-byte hex)
	SubjectPub      string   `json:"subjectPub"`
	Net             string   `json:"net"`
	Domains         []string `json:"domains,omitempty"`
	OntologyID      string   `json:"ontologyId,omitempty"`
	DealID          string   `json:"dealId"`
	DealReceiptHash string   `json:"dealReceiptHash"`
	Token           string   `json:"token"` // nektarrep1.<base64url>
	CompositeBP     int64    `json:"compositeBp"`
	Exp             int64    `json:"exp"` // unix ms
}

// EncodePayload encodes a MarketPayload as a pure RFC 8949 CDE CBOR nkt1 record via bcommon/record.
// Key 0 = b"nkt1", Key 1 = uint(kind), Key 2 = bstr(canonical CBOR signed envelope), plus any preserved odd Extra keys.
func EncodePayload(p MarketPayload) ([]byte, error) {
	var kindNum uint64
	switch p.Kind {
	case KindWant:
		kindNum = 1
	case KindOffer:
		kindNum = 2
	case KindAnnounce:
		kindNum = 3
	case KindAttestation:
		kindNum = 4
	default:
		return nil, fmt.Errorf("bstack: unknown artifact kind %q", p.Kind)
	}
	if len(p.Body) == 0 {
		return nil, errors.New("bstack: empty nkt1 body")
	}
	if _, err := core.DecodeCBOR(p.Body); err != nil {
		return nil, fmt.Errorf("bstack: nkt1 body must be canonical CBOR: %w", err)
	}
	m := cbor.Map{
		{Key: uint64(0), Val: MarketMagic},
		{Key: uint64(1), Val: kindNum},
		{Key: uint64(2), Val: p.Body},
	}
	var extra cbor.Map
	if len(p.Extra) > 0 {
		extra = make(cbor.Map, 0, len(p.Extra))
		for _, pair := range p.Extra {
			k, ok := pair.Key.(uint64)
			if !ok || k <= 2 {
				return nil, fmt.Errorf("bstack: invalid nkt1 extension key %v", pair.Key)
			}
			extra = append(extra, pair)
		}
	}
	return record.Encode(m, extra, MaxPayload)
}

// DecodePayload decodes a pure RFC 8949 CDE CBOR nkt1 record via bcommon/record,
// enforcing critical (even) unknown-key rejection and preserving non-critical (odd) extension keys.
func DecodePayload(b []byte) (MarketPayload, error) {
	if !record.Claims(b, MarketMagic) {
		return MarketPayload{}, errors.New("bstack: payload does not carry nkt1 magic")
	}
	f, err := record.Decode(b, MaxPayload, 2, MarketMagic)
	if err != nil {
		return MarketPayload{}, fmt.Errorf("bstack: decode nkt1 record: %w", err)
	}
	kindNum, err := f.Uint(1, 1, 4)
	if err != nil {
		return MarketPayload{}, fmt.Errorf("bstack: kind field: %w", err)
	}
	bodyBytes, err := f.Bytes(2)
	if err != nil {
		return MarketPayload{}, fmt.Errorf("bstack: body field: %w", err)
	}
	if len(bodyBytes) == 0 {
		return MarketPayload{}, errors.New("bstack: empty nkt1 body")
	}
	if _, err := core.DecodeCBOR(bodyBytes); err != nil {
		return MarketPayload{}, fmt.Errorf("bstack: nkt1 body is not canonical CBOR: %w", err)
	}
	var extra []cbor.Pair
	for _, p := range f.Extra {
		k, ok := p.Key.(uint64)
		if !ok {
			return MarketPayload{}, fmt.Errorf("bstack: non-uint extension key %T", p.Key)
		}
		if k%2 == 0 {
			return MarketPayload{}, fmt.Errorf("bstack: %w: %d", core.ErrUnknownCriticalKey, k)
		}
		extra = append(extra, p)
	}
	var kind ArtifactKind
	switch kindNum {
	case 1:
		kind = KindWant
	case 2:
		kind = KindOffer
	case 3:
		kind = KindAnnounce
	case 4:
		kind = KindAttestation
	}
	cp := make([]byte, len(bodyBytes))
	copy(cp, bodyBytes)
	return MarketPayload{Kind: kind, Body: cp, Extra: extra}, nil
}

// LaneNibble hashes domain to one hex nibble [0-9a-f].
func LaneNibble(domain string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(domain))))
	return fmt.Sprintf("%x", sum[0]>>4)
}

// LaneOf returns the legacy nibble overlay topic name `tm_nektar_<net>_<nibble>`.
func LaneOf(net, domain string) string {
	return fmt.Sprintf("tm_nektar_%s_%s", net, LaneNibble(domain))
}

// DomainSlug canonicalizes a free-form domain label into a safe ASCII topic slug ([a-z0-9_-]{1,32}).
func DomainSlug(domain string) string {
	s := strings.ToLower(strings.TrimSpace(domain))
	var b strings.Builder
	prevUnderscore := false
	for i := 0; i < len(s) && b.Len() < 32; i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteByte(c)
			prevUnderscore = false
		} else if !prevUnderscore && b.Len() > 0 {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	out := strings.Trim(b.String(), "_-")
	if out == "" {
		return "general"
	}
	return out
}

// DomainTopic returns the dynamic BRC-223 semantic domain topic `tm_nektar_<net>_dom_<slug>`.
func DomainTopic(net, domain string) string {
	if strings.TrimSpace(net) == "" {
		net = "regtest"
	}
	return fmt.Sprintf("tm_nektar_%s_dom_%s", strings.ToLower(strings.TrimSpace(net)), DomainSlug(domain))
}

// OntologyTopic returns the dynamic BRC-223 ontology topic `tm_nektar_<net>_ont_<id16>` (empty if invalid).
func OntologyTopic(net, ontologyID string) string {
	oid := strings.ToLower(strings.TrimSpace(ontologyID))
	if len(oid) < 16 || strings.EqualFold(oid, "global") {
		return ""
	}
	prefix := oid[:16]
	if _, err := hex.DecodeString(prefix); err != nil {
		return ""
	}
	if strings.TrimSpace(net) == "" {
		net = "regtest"
	}
	return fmt.Sprintf("tm_nektar_%s_ont_%s", strings.ToLower(strings.TrimSpace(net)), prefix)
}

// TopicsForMarketAd returns deduplicated dynamic semantic & ontology topics for a market artifact.
func TopicsForMarketAd(net string, domains []string, ontologyID string) []string {
	seen := make(map[string]bool, len(domains)+1)
	out := make([]string, 0, len(domains)+1)
	for _, d := range domains {
		t := DomainTopic(net, d)
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	if ot := OntologyTopic(net, ontologyID); ot != "" && !seen[ot] {
		seen[ot] = true
		out = append(out, ot)
	}
	return out
}

// IsValidDynamicMarketTopic validates whether topic is a well-formed `tm_nektar_<net>_dom_<slug>`
// or `tm_nektar_<net>_ont_<id16>` topic for net.
func IsValidDynamicMarketTopic(net, topic string) bool {
	net = strings.ToLower(strings.TrimSpace(net))
	if net == "" {
		return false
	}
	domPfx := "tm_nektar_" + net + "_dom_"
	if strings.HasPrefix(topic, domPfx) {
		slug := strings.TrimPrefix(topic, domPfx)
		if len(slug) == 0 || len(slug) > 32 {
			return false
		}
		for i := 0; i < len(slug); i++ {
			c := slug[i]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
				return false
			}
		}
		return true
	}
	ontPfx := "tm_nektar_" + net + "_ont_"
	if strings.HasPrefix(topic, ontPfx) {
		id16 := strings.TrimPrefix(topic, ontPfx)
		if len(id16) != 16 {
			return false
		}
		for i := 0; i < len(id16); i++ {
			c := id16[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return false
			}
		}
		return true
	}
	return false
}

// DigestSigner signs 32-byte digests without exposing raw *ec.PrivateKey across package boundaries (ARCH-S1).
type DigestSigner interface {
	PubHex() string
	SignDigest(digest32 []byte) ([]byte, error)
}

// SignArtifactRawWithSigner signs exact canonical CBOR bytes under an explicit domain tag via DigestSigner.
func SignArtifactRawWithSigner(signer DigestSigner, tag, canonCBOR []byte) (string, error) {
	if signer == nil || signer.PubHex() == "" {
		return "", errors.New("bstack: nil signing key")
	}
	if len(tag) == 0 || len(canonCBOR) == 0 {
		return "", errors.New("bstack: domain tag and canonical CBOR required")
	}
	digest := core.DSHA256(core.Concat(tag, canonCBOR))
	der, err := signer.SignDigest(digest)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(der), nil
}

// SignArtifactWithSigner canonically signs any registered SignedCBORArtifact via DigestSigner.
func SignArtifactWithSigner(signer DigestSigner, artifact any) (string, error) {
	a, ok := artifact.(SignedCBORArtifact)
	if !ok {
		return "", fmt.Errorf("bstack: unregistered artifact type %T (must implement SignedCBORArtifact)", artifact)
	}
	canon, err := a.EncodeCanonicalCBOR()
	if err != nil {
		return "", err
	}
	return SignArtifactRawWithSigner(signer, a.DomainTag(), canon)
}

// SignArtifactRaw signs exact canonical CBOR bytes under an explicit domain tag.
func SignArtifactRaw(priv *ec.PrivateKey, tag, canonCBOR []byte) (string, error) {
	if priv == nil {
		return "", errors.New("bstack: nil signing key")
	}
	if len(tag) == 0 || len(canonCBOR) == 0 {
		return "", errors.New("bstack: domain tag and canonical CBOR required")
	}
	digest := core.DSHA256(core.Concat(tag, canonCBOR))
	sig, err := priv.Sign(digest)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sig.Serialize()), nil
}

// VerifyArtifactRaw verifies a domain-separated signature over exact canonical CBOR bytes.
func VerifyArtifactRaw(pubHex, sigHex string, tag, canonCBOR []byte) error {
	if len(tag) == 0 || len(canonCBOR) == 0 {
		return errors.New("bstack: domain tag and canonical CBOR required")
	}
	digest := core.DSHA256(core.Concat(tag, canonCBOR))
	if err := core.FastVerifyStrictHex(pubHex, sigHex, digest); err != nil {
		if errors.Is(err, core.ErrNonCanonicalPubKey) || errors.Is(err, core.ErrNonCanonicalDER) || errors.Is(err, core.ErrHighSSignature) {
			return err
		}
		return errors.New("bstack: artifact signature invalid")
	}
	return nil
}

// SignArtifact canonically signs any registered SignedCBORArtifact under its explicit domain-separation tag.
// Unregistered types fail closed (zero generic fallback tag).
func SignArtifact(priv *ec.PrivateKey, artifact any) (string, error) {
	a, ok := artifact.(SignedCBORArtifact)
	if !ok {
		return "", fmt.Errorf("bstack: unregistered artifact type %T (must implement SignedCBORArtifact)", artifact)
	}
	canon, err := a.EncodeCanonicalCBOR()
	if err != nil {
		return "", err
	}
	return SignArtifactRaw(priv, a.DomainTag(), canon)
}

// VerifyArtifact verifies a domain-separated CBOR signature produced by SignArtifact against canonical
// compressed pubHex, rejecting unregistered types, uncompressed/hybrid pubkeys, non-canonical DER
// trailing bytes, malleable high-S signatures, and cross-type artifact replays.
func VerifyArtifact(pubHex, sigHex string, artifact any) error {
	a, ok := artifact.(SignedCBORArtifact)
	if !ok {
		return fmt.Errorf("bstack: unregistered artifact type %T (must implement SignedCBORArtifact)", artifact)
	}
	canon, err := a.EncodeCanonicalCBOR()
	if err != nil {
		return err
	}
	return VerifyArtifactRaw(pubHex, sigHex, a.DomainTag(), canon)
}
