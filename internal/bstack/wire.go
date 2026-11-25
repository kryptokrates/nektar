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
	KindWant     ArtifactKind = "want"
	KindOffer    ArtifactKind = "offer"
	KindAnnounce ArtifactKind = "announce"
)

// MarketPayload is the canonical nkt1 CBOR envelope riding on tm_nektar_<net>_<nibble> lanes.
type MarketPayload struct {
	Kind ArtifactKind    `json:"kind"`
	Body json.RawMessage `json:"body"`
}

// SignedWant advertises a buyer's knowledge request on the market lanes.
type SignedWant struct {
	Want WantAd `json:"want"`
	Sig  string `json:"sig"`
}

type WantAd struct {
	WantID   string   `json:"wantId"`
	Pub      string   `json:"pub"`
	Net      string   `json:"net"`
	Domains  []string `json:"domains"`
	Query    string   `json:"query"`
	MaxSats  uint64   `json:"maxSats"`
	Revision int      `json:"revision"`
	Exp      int64    `json:"exp"` // unix ms
}

// SignedOffer advertises a seller's signed quote answering a WantAd.
type SignedOffer struct {
	Offer OfferAd `json:"offer"`
	Sig   string  `json:"sig"`
}

type OfferAd struct {
	OfferID      string          `json:"offerId"`
	WantID       string          `json:"wantId"`
	Pub          string          `json:"pub"`
	BuyerPub     string          `json:"buyerPub"`
	Net          string          `json:"net"`
	Domains      []string        `json:"domains"`
	PriceSats    uint64          `json:"priceSats"`
	Shard        string          `json:"shard,omitempty"`
	Leaf         string          `json:"leaf,omitempty"`
	ChirpURL     string          `json:"chirpUrl,omitempty"`
	KeyCommitHex   string          `json:"keyCommitHex,omitempty"`
	PayAddress     string          `json:"payAddress,omitempty"`
	PayIdentityKey string          `json:"payIdentityKey,omitempty"`
	SellerURL      string          `json:"sellerUrl,omitempty"`
	BboxOffice   string          `json:"bboxOffice,omitempty"`
	SaleOffer    json.RawMessage `json:"saleOffer,omitempty"`
	IsHub        bool            `json:"isHub,omitempty"`
	Direct       bool            `json:"direct,omitempty"`
	Revision     int             `json:"revision"`
	Exp          int64           `json:"exp"`
}

// PackRef advertises a content-addressed CHIRP shard pack in a signed AnnounceAd.
type PackRef struct {
	Shard    string   `json:"shard"`
	Domains  []string `json:"domains,omitempty"`
	ChirpURL string   `json:"chirpUrl"`
	RootHex  string   `json:"rootHex,omitempty"`
	Count    int      `json:"count,omitempty"`
}

// SignedAnnounce advertises an operator/hub node on the enumerable dir lane.
type SignedAnnounce struct {
	Announce AnnounceAd `json:"announce"`
	Sig      string     `json:"sig"`
}

type AnnounceAd struct {
	Pub           string    `json:"pub"`
	Net           string    `json:"net"`
	Handle        string    `json:"handle,omitempty"` // bfinger user@domain
	PublicURL     string    `json:"publicUrl,omitempty"`
	BboxOffice    string    `json:"bboxOffice,omitempty"`
	Hosts         []string  `json:"hosts"`
	Domains       []string  `json:"domains,omitempty"`
	SellDomains   []string  `json:"sellDomains,omitempty"`
	BountyDomains []string  `json:"bountyDomains,omitempty"`
	Packs         []PackRef `json:"packs,omitempty"`
	IsHub         bool      `json:"isHub,omitempty"`
	Exp           int64     `json:"exp"`
}

// EncodePayload encodes a MarketPayload as a stock Lightweb canonical CBOR nkt1 record.
func EncodePayload(p MarketPayload) ([]byte, error) {
	var kindNum uint64
	switch p.Kind {
	case KindWant:
		kindNum = 1
	case KindOffer:
		kindNum = 2
	case KindAnnounce:
		kindNum = 3
	default:
		return nil, fmt.Errorf("bstack: unknown artifact kind %q", p.Kind)
	}
	m := cbor.Map{
		{Key: uint64(0), Val: MarketMagic},
		{Key: uint64(1), Val: kindNum},
		{Key: uint64(2), Val: string(p.Body)},
	}
	return record.Encode(m, nil, MaxPayload)
}

// DecodePayload decodes a canonical CBOR nkt1 record via bcommon/record.
func DecodePayload(b []byte) (MarketPayload, error) {
	if !record.Claims(b, MarketMagic) {
		return MarketPayload{}, errors.New("bstack: payload does not carry nkt1 magic")
	}
	f, err := record.Decode(b, MaxPayload, 2, MarketMagic)
	if err != nil {
		return MarketPayload{}, fmt.Errorf("bstack: decode nkt1 record: %w", err)
	}
	kindNum, err := f.Uint(1, 1, 3)
	if err != nil {
		return MarketPayload{}, fmt.Errorf("bstack: kind field: %w", err)
	}
	bodyStr, err := f.Text(2)
	if err != nil {
		return MarketPayload{}, fmt.Errorf("bstack: body field: %w", err)
	}
	var kind ArtifactKind
	switch kindNum {
	case 1:
		kind = KindWant
	case 2:
		kind = KindOffer
	case 3:
		kind = KindAnnounce
	}
	if !json.Valid([]byte(bodyStr)) {
		return MarketPayload{}, errors.New("bstack: nkt1 body is not valid JSON")
	}
	return MarketPayload{Kind: kind, Body: json.RawMessage(bodyStr)}, nil
}

// LaneNibble hashes domain to one hex nibble [0-9a-f].
func LaneNibble(domain string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(domain))))
	return fmt.Sprintf("%x", sum[0]>>4)
}

// LaneOf returns the overlay topic name `tm_nektar_<net>_<nibble>`.
func LaneOf(net, domain string) string {
	return fmt.Sprintf("tm_nektar_%s_%s", net, LaneNibble(domain))
}

// AllLanes returns all 16 nibble topics for a network.
func AllLanes(net string) []string {
	out := make([]string, 16)
	for i := range 16 {
		out[i] = fmt.Sprintf("tm_nektar_%s_%x", net, i)
	}
	return out
}

// SignArtifact canonically signs any WantAd / OfferAd / AnnounceAd with priv.
func SignArtifact(priv *ec.PrivateKey, artifact any) (string, error) {
	canon, err := core.CanonicalJSON(artifact)
	if err != nil {
		return "", err
	}
	digest := core.DSHA256(canon)
	sig, err := priv.Sign(digest)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sig.Serialize()), nil
}

// VerifyArtifact verifies a signature produced by SignArtifact against canonical compressed pubHex,
// rejecting uncompressed/hybrid pubkeys, non-canonical DER trailing bytes, and malleable high-S signatures.
func VerifyArtifact(pubHex, sigHex string, artifact any) error {
	pub, _, err := core.ParseStrictCompressedPubKey(pubHex)
	if err != nil {
		return err
	}
	sig, err := core.ParseStrictLowSDER(sigHex)
	if err != nil {
		return err
	}
	canon, err := core.CanonicalJSON(artifact)
	if err != nil {
		return err
	}
	if !sig.Verify(core.DSHA256(canon), pub) {
		return errors.New("bstack: artifact signature invalid")
	}
	return nil
}
