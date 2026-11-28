package bstack

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/lightwebinc/bcommon/cbor"

	"github.com/kryptokrates/nektar/internal/core"
)

// SignedCBORArtifact is implemented by every protocol message signed via SignArtifact/VerifyArtifact.
// Every artifact type MUST declare its own explicit domain-separation tag and canonical RFC 8949 CDE CBOR
// encoding — there is NO generic fallback tag.
type SignedCBORArtifact interface {
	DomainTag() []byte
	EncodeCanonicalCBOR() ([]byte, error)
}

var (
	wantDomainTag        = []byte("nektar:artifact:want:v2")
	offerDomainTag       = []byte("nektar:artifact:offer:v2")
	announceDomainTag    = []byte("nektar:artifact:announce:v2")
	attestationDomainTag = []byte("nektar:artifact:attestation:v2")
)

// HubEgressAuth is the canonical CBOR-signed payload authenticating metered Hub /events, /sync, and /submit calls.
type HubEgressAuth struct {
	Action  string
	Pub     string
	TS      int64
	Nonce   string
	Aud     string
	Cursor  int64
	Domains string
}

func (a HubEgressAuth) DomainTag() []byte { return core.TagHubEgressV1 }

func (a HubEgressAuth) EncodeCanonicalCBOR() ([]byte, error) {
	return core.EncodeCBOR(cbor.Map{
		{Key: uint64(1), Val: a.Action},
		{Key: uint64(2), Val: strings.ToLower(strings.TrimSpace(a.Pub))},
		{Key: uint64(3), Val: a.TS},
		{Key: uint64(4), Val: a.Nonce},
		{Key: uint64(5), Val: strings.ToLower(strings.TrimSpace(a.Aud))},
		{Key: uint64(6), Val: a.Cursor},
		{Key: uint64(7), Val: strings.ToLower(strings.TrimSpace(a.Domains))},
	})
}

// BboxInboxAuth is the canonical CBOR-signed payload authenticating GET /bbox/inbox polling.
type BboxInboxAuth struct {
	Pub   string
	TS    int64
	Nonce string
	Aud   string
	Since int64
}

func (a BboxInboxAuth) DomainTag() []byte { return core.TagBboxInboxV1 }

func (a BboxInboxAuth) EncodeCanonicalCBOR() ([]byte, error) {
	return core.EncodeCBOR(cbor.Map{
		{Key: uint64(1), Val: strings.ToLower(strings.TrimSpace(a.Pub))},
		{Key: uint64(2), Val: a.TS},
		{Key: uint64(3), Val: a.Nonce},
		{Key: uint64(4), Val: strings.ToLower(strings.TrimSpace(a.Aud))},
		{Key: uint64(5), Val: a.Since},
	})
}

// --- WantAd CBOR ---

var wantKnownKeys = core.KnownKeyRange(1, 14)

func (w WantAd) DomainTag() []byte { return wantDomainTag }

func (w WantAd) EncodeCanonicalCBOR() ([]byte, error) {
	m := cbor.Map{
		{Key: uint64(1), Val: w.WantID},
		{Key: uint64(2), Val: w.Pub},
		{Key: uint64(3), Val: w.Net},
		{Key: uint64(4), Val: core.CBORStringsVal(w.Domains)},
		{Key: uint64(5), Val: w.Query},
		{Key: uint64(7), Val: w.MaxSats},
		{Key: uint64(13), Val: int64(w.Revision)},
		{Key: uint64(14), Val: w.Exp},
	}
	if w.OntologyID != "" {
		m = append(m, cbor.Pair{Key: uint64(6), Val: w.OntologyID})
	}
	if w.MinGrade != "" {
		m = append(m, cbor.Pair{Key: uint64(8), Val: w.MinGrade})
	}
	if w.MaxSatsPer1kUnits > 0 {
		m = append(m, cbor.Pair{Key: uint64(9), Val: w.MaxSatsPer1kUnits})
	}
	if w.FreshnessClass != "" {
		m = append(m, cbor.Pair{Key: uint64(10), Val: w.FreshnessClass})
	}
	if w.RequiredRights != "" {
		m = append(m, cbor.Pair{Key: uint64(11), Val: w.RequiredRights})
	}
	if w.PricingMode != "" {
		m = append(m, cbor.Pair{Key: uint64(12), Val: w.PricingMode})
	}
	return core.EncodeCBOR(m)
}

func decodeWantCBOR(raw []byte) (WantAd, []cbor.Pair, error) {
	v, err := core.DecodeCBOR(raw)
	if err != nil {
		return WantAd{}, nil, fmt.Errorf("bstack: decode want cbor: %w", err)
	}
	m, extra, err := core.SplitCBORMap(v, wantKnownKeys)
	if err != nil {
		return WantAd{}, nil, fmt.Errorf("bstack: want map: %w", err)
	}
	var w WantAd
	if w.WantID, err = core.CBORText(m, 1); err != nil {
		return WantAd{}, nil, err
	}
	if w.Pub, err = core.CBORText(m, 2); err != nil {
		return WantAd{}, nil, err
	}
	if w.Net, err = core.CBORText(m, 3); err != nil {
		return WantAd{}, nil, err
	}
	if w.Domains, err = core.CBORStrings(m, 4); err != nil {
		return WantAd{}, nil, err
	}
	if w.Query, err = core.CBORText(m, 5); err != nil {
		return WantAd{}, nil, err
	}
	if w.OntologyID, err = core.CBORText(m, 6); err != nil {
		return WantAd{}, nil, err
	}
	if w.MaxSats, err = core.CBORUint(m, 7); err != nil {
		return WantAd{}, nil, err
	}
	if w.MinGrade, err = core.CBORText(m, 8); err != nil {
		return WantAd{}, nil, err
	}
	if w.MaxSatsPer1kUnits, err = core.CBORUint(m, 9); err != nil {
		return WantAd{}, nil, err
	}
	if w.FreshnessClass, err = core.CBORText(m, 10); err != nil {
		return WantAd{}, nil, err
	}
	if w.RequiredRights, err = core.CBORText(m, 11); err != nil {
		return WantAd{}, nil, err
	}
	if w.PricingMode, err = core.CBORText(m, 12); err != nil {
		return WantAd{}, nil, err
	}
	rev, err := core.CBORInt(m, 13)
	if err != nil {
		return WantAd{}, nil, err
	}
	w.Revision = int(rev)
	if w.Exp, err = core.CBORInt(m, 14); err != nil {
		return WantAd{}, nil, err
	}
	return w, extra, nil
}

// --- OfferAd CBOR ---

var offerKnownKeys = core.KnownKeyRange(1, 37)

func (o OfferAd) DomainTag() []byte { return offerDomainTag }

func (o OfferAd) EncodeCanonicalCBOR() ([]byte, error) {
	m := cbor.Map{
		{Key: uint64(1), Val: o.OfferID},
		{Key: uint64(2), Val: o.WantID},
		{Key: uint64(3), Val: o.Pub},
		{Key: uint64(4), Val: o.BuyerPub},
		{Key: uint64(5), Val: o.Net},
		{Key: uint64(6), Val: core.CBORStringsVal(o.Domains)},
		{Key: uint64(7), Val: o.PriceSats},
		{Key: uint64(35), Val: int64(o.Revision)},
		{Key: uint64(36), Val: o.Exp},
	}
	addStr := func(k uint64, s string) {
		if s != "" {
			m = append(m, cbor.Pair{Key: k, Val: s})
		}
	}
	addUint := func(k uint64, u uint64) {
		if u > 0 {
			m = append(m, cbor.Pair{Key: k, Val: u})
		}
	}
	addInt := func(k uint64, n int64) {
		if n != 0 {
			m = append(m, cbor.Pair{Key: k, Val: n})
		}
	}
	addBool := func(k uint64, b bool) {
		if b {
			m = append(m, cbor.Pair{Key: k, Val: true})
		}
	}
	addStr(8, o.Grade)
	addUint(9, uint64(o.UnitCount))
	addUint(10, o.SatsPer1kUnits)
	addStr(11, o.FreshnessClass)
	addInt(12, o.ValidUntilUnix)
	addStr(13, o.SubjectVersion)
	addStr(14, o.UsageRights)
	addStr(15, o.PricingMode)
	addInt(16, o.SubscriptionSec)
	addStr(17, o.FeedURL)
	addStr(18, o.Shard)
	addStr(19, o.Leaf)
	addStr(20, o.OntologyID)
	addStr(21, o.EdgesRoot)
	addStr(22, o.ChirpURL)
	addStr(23, o.KeyCommitHex)
	addStr(24, o.PayAddress)
	addStr(25, o.PayIdentityKey)
	addStr(26, o.SellerURL)
	addStr(27, o.BboxOffice)
	if len(o.SaleOffer) > 0 {
		m = append(m, cbor.Pair{Key: uint64(28), Val: []byte(o.SaleOffer)})
	}
	addStr(29, o.Description)
	if len(o.Tags) > 0 {
		m = append(m, cbor.Pair{Key: uint64(30), Val: core.CBORStringsVal(o.Tags)})
	}
	addStr(31, o.BondOutpoint)
	addStr(32, o.BondBEEFHex)
	addBool(33, o.IsHub)
	addBool(34, o.Direct)
	addStr(37, o.PayloadFormat)
	return core.EncodeCBOR(m)
}

func decodeOfferCBOR(raw []byte) (OfferAd, []cbor.Pair, error) {
	v, err := core.DecodeCBOR(raw)
	if err != nil {
		return OfferAd{}, nil, fmt.Errorf("bstack: decode offer cbor: %w", err)
	}
	m, extra, err := core.SplitCBORMap(v, offerKnownKeys)
	if err != nil {
		return OfferAd{}, nil, fmt.Errorf("bstack: offer map: %w", err)
	}
	var o OfferAd
	if o.OfferID, err = core.CBORText(m, 1); err != nil {
		return OfferAd{}, nil, err
	}
	if o.WantID, err = core.CBORText(m, 2); err != nil {
		return OfferAd{}, nil, err
	}
	if o.Pub, err = core.CBORText(m, 3); err != nil {
		return OfferAd{}, nil, err
	}
	if o.BuyerPub, err = core.CBORText(m, 4); err != nil {
		return OfferAd{}, nil, err
	}
	if o.Net, err = core.CBORText(m, 5); err != nil {
		return OfferAd{}, nil, err
	}
	if o.Domains, err = core.CBORStrings(m, 6); err != nil {
		return OfferAd{}, nil, err
	}
	if o.PriceSats, err = core.CBORUint(m, 7); err != nil {
		return OfferAd{}, nil, err
	}
	if o.Grade, err = core.CBORText(m, 8); err != nil {
		return OfferAd{}, nil, err
	}
	uc, err := core.CBORUint(m, 9)
	if err != nil {
		return OfferAd{}, nil, err
	}
	o.UnitCount = uint32(uc)
	if o.SatsPer1kUnits, err = core.CBORUint(m, 10); err != nil {
		return OfferAd{}, nil, err
	}
	if o.FreshnessClass, err = core.CBORText(m, 11); err != nil {
		return OfferAd{}, nil, err
	}
	if o.ValidUntilUnix, err = core.CBORInt(m, 12); err != nil {
		return OfferAd{}, nil, err
	}
	if o.SubjectVersion, err = core.CBORText(m, 13); err != nil {
		return OfferAd{}, nil, err
	}
	if o.UsageRights, err = core.CBORText(m, 14); err != nil {
		return OfferAd{}, nil, err
	}
	if o.PricingMode, err = core.CBORText(m, 15); err != nil {
		return OfferAd{}, nil, err
	}
	if o.SubscriptionSec, err = core.CBORInt(m, 16); err != nil {
		return OfferAd{}, nil, err
	}
	if o.FeedURL, err = core.CBORText(m, 17); err != nil {
		return OfferAd{}, nil, err
	}
	if o.Shard, err = core.CBORText(m, 18); err != nil {
		return OfferAd{}, nil, err
	}
	if o.Leaf, err = core.CBORText(m, 19); err != nil {
		return OfferAd{}, nil, err
	}
	if o.OntologyID, err = core.CBORText(m, 20); err != nil {
		return OfferAd{}, nil, err
	}
	if o.EdgesRoot, err = core.CBORText(m, 21); err != nil {
		return OfferAd{}, nil, err
	}
	if o.ChirpURL, err = core.CBORText(m, 22); err != nil {
		return OfferAd{}, nil, err
	}
	if o.KeyCommitHex, err = core.CBORText(m, 23); err != nil {
		return OfferAd{}, nil, err
	}
	if o.PayAddress, err = core.CBORText(m, 24); err != nil {
		return OfferAd{}, nil, err
	}
	if o.PayIdentityKey, err = core.CBORText(m, 25); err != nil {
		return OfferAd{}, nil, err
	}
	if o.SellerURL, err = core.CBORText(m, 26); err != nil {
		return OfferAd{}, nil, err
	}
	if o.BboxOffice, err = core.CBORText(m, 27); err != nil {
		return OfferAd{}, nil, err
	}
	if soBytes, err := core.CBORBytes(m, 28); err != nil {
		return OfferAd{}, nil, err
	} else if len(soBytes) > 0 {
		o.SaleOffer = soBytes
	}
	if o.Description, err = core.CBORText(m, 29); err != nil {
		return OfferAd{}, nil, err
	}
	if o.Tags, err = core.CBORStrings(m, 30); err != nil {
		return OfferAd{}, nil, err
	}
	if o.BondOutpoint, err = core.CBORText(m, 31); err != nil {
		return OfferAd{}, nil, err
	}
	if o.BondBEEFHex, err = core.CBORText(m, 32); err != nil {
		return OfferAd{}, nil, err
	}
	if o.IsHub, err = core.CBORBool(m, 33); err != nil {
		return OfferAd{}, nil, err
	}
	if o.Direct, err = core.CBORBool(m, 34); err != nil {
		return OfferAd{}, nil, err
	}
	rev, err := core.CBORInt(m, 35)
	if err != nil {
		return OfferAd{}, nil, err
	}
	o.Revision = int(rev)
	if o.Exp, err = core.CBORInt(m, 36); err != nil {
		return OfferAd{}, nil, err
	}
	if o.PayloadFormat, err = core.CBORText(m, 37); err != nil {
		return OfferAd{}, nil, err
	}
	return o, extra, nil
}

// --- AnnounceAd CBOR ---

var (
	packRefKnownKeys  = core.KnownKeyRange(1, 20)
	hubFeesKnownKeys  = core.KnownKeyRange(1, 6)
	announceKnownKeys = core.KnownKeyRange(1, 18)
)

func packRefToCBOR(pk PackRef) cbor.Map {
	m := cbor.Map{
		{Key: uint64(1), Val: pk.Shard},
		{Key: uint64(3), Val: pk.ChirpURL},
	}
	if len(pk.Domains) > 0 {
		m = append(m, cbor.Pair{Key: uint64(2), Val: core.CBORStringsVal(pk.Domains)})
	}
	if pk.RootHex != "" {
		m = append(m, cbor.Pair{Key: uint64(4), Val: pk.RootHex})
	}
	if pk.OntologyID != "" {
		m = append(m, cbor.Pair{Key: uint64(5), Val: pk.OntologyID})
	}
	if pk.TargetOntologyID != "" {
		m = append(m, cbor.Pair{Key: uint64(6), Val: pk.TargetOntologyID})
	}
	if pk.Count != 0 {
		m = append(m, cbor.Pair{Key: uint64(7), Val: int64(pk.Count)})
	}
	if pk.Grade != "" {
		m = append(m, cbor.Pair{Key: uint64(8), Val: pk.Grade})
	}
	if pk.UnitCount > 0 {
		m = append(m, cbor.Pair{Key: uint64(9), Val: uint64(pk.UnitCount)})
	}
	if pk.PricingMode != "" {
		m = append(m, cbor.Pair{Key: uint64(10), Val: pk.PricingMode})
	}
	if len(pk.EnabledModes) > 0 {
		m = append(m, cbor.Pair{Key: uint64(11), Val: core.CBORStringsVal(pk.EnabledModes)})
	}
	if pk.PriceSats > 0 {
		m = append(m, cbor.Pair{Key: uint64(12), Val: pk.PriceSats})
	}
	if pk.SatsPer1kUnits > 0 {
		m = append(m, cbor.Pair{Key: uint64(13), Val: pk.SatsPer1kUnits})
	}
	if pk.QueryPriceSats > 0 {
		m = append(m, cbor.Pair{Key: uint64(14), Val: pk.QueryPriceSats})
	}
	if pk.SubscriptionPriceSats > 0 {
		m = append(m, cbor.Pair{Key: uint64(15), Val: pk.SubscriptionPriceSats})
	}
	if pk.SubscriptionSec > 0 {
		m = append(m, cbor.Pair{Key: uint64(16), Val: pk.SubscriptionSec})
	}
	if pk.FreshnessClass != "" {
		m = append(m, cbor.Pair{Key: uint64(17), Val: pk.FreshnessClass})
	}
	if pk.UsageRights != "" {
		m = append(m, cbor.Pair{Key: uint64(18), Val: pk.UsageRights})
	}
	if pk.QueryEndpoint != "" {
		m = append(m, cbor.Pair{Key: uint64(19), Val: pk.QueryEndpoint})
	}
	if pk.FeedEndpoint != "" {
		m = append(m, cbor.Pair{Key: uint64(20), Val: pk.FeedEndpoint})
	}
	return m
}

func decodePackRefCBOR(v cbor.Value) (PackRef, error) {
	m, _, err := core.SplitCBORMap(v, packRefKnownKeys)
	if err != nil {
		return PackRef{}, err
	}
	var pk PackRef
	if pk.Shard, err = core.CBORText(m, 1); err != nil {
		return PackRef{}, err
	}
	if pk.Domains, err = core.CBORStrings(m, 2); err != nil {
		return PackRef{}, err
	}
	if pk.ChirpURL, err = core.CBORText(m, 3); err != nil {
		return PackRef{}, err
	}
	if pk.RootHex, err = core.CBORText(m, 4); err != nil {
		return PackRef{}, err
	}
	if pk.OntologyID, err = core.CBORText(m, 5); err != nil {
		return PackRef{}, err
	}
	if pk.TargetOntologyID, err = core.CBORText(m, 6); err != nil {
		return PackRef{}, err
	}
	cnt, err := core.CBORInt(m, 7)
	if err != nil {
		return PackRef{}, err
	}
	pk.Count = int(cnt)
	if pk.Grade, err = core.CBORText(m, 8); err != nil {
		return PackRef{}, err
	}
	uc, err := core.CBORUint(m, 9)
	if err != nil {
		return PackRef{}, err
	}
	pk.UnitCount = uint32(uc)
	if pk.PricingMode, err = core.CBORText(m, 10); err != nil {
		return PackRef{}, err
	}
	if pk.EnabledModes, err = core.CBORStrings(m, 11); err != nil {
		return PackRef{}, err
	}
	if pk.PriceSats, err = core.CBORUint(m, 12); err != nil {
		return PackRef{}, err
	}
	if pk.SatsPer1kUnits, err = core.CBORUint(m, 13); err != nil {
		return PackRef{}, err
	}
	if pk.QueryPriceSats, err = core.CBORUint(m, 14); err != nil {
		return PackRef{}, err
	}
	if pk.SubscriptionPriceSats, err = core.CBORUint(m, 15); err != nil {
		return PackRef{}, err
	}
	if pk.SubscriptionSec, err = core.CBORInt(m, 16); err != nil {
		return PackRef{}, err
	}
	if pk.FreshnessClass, err = core.CBORText(m, 17); err != nil {
		return PackRef{}, err
	}
	if pk.UsageRights, err = core.CBORText(m, 18); err != nil {
		return PackRef{}, err
	}
	if pk.QueryEndpoint, err = core.CBORText(m, 19); err != nil {
		return PackRef{}, err
	}
	if pk.FeedEndpoint, err = core.CBORText(m, 20); err != nil {
		return PackRef{}, err
	}
	return pk, nil
}

func (a AnnounceAd) DomainTag() []byte { return announceDomainTag }

func (a AnnounceAd) EncodeCanonicalCBOR() ([]byte, error) {
	m := cbor.Map{
		{Key: uint64(1), Val: a.Pub},
		{Key: uint64(2), Val: a.Net},
		{Key: uint64(6), Val: core.CBORStringsVal(a.Hosts)},
		{Key: uint64(18), Val: a.Exp},
	}
	if a.Handle != "" {
		m = append(m, cbor.Pair{Key: uint64(3), Val: a.Handle})
	}
	if a.PublicURL != "" {
		m = append(m, cbor.Pair{Key: uint64(4), Val: a.PublicURL})
	}
	if a.BboxOffice != "" {
		m = append(m, cbor.Pair{Key: uint64(5), Val: a.BboxOffice})
	}
	if len(a.Domains) > 0 {
		m = append(m, cbor.Pair{Key: uint64(7), Val: core.CBORStringsVal(a.Domains)})
	}
	if len(a.SellDomains) > 0 {
		m = append(m, cbor.Pair{Key: uint64(8), Val: core.CBORStringsVal(a.SellDomains)})
	}
	if len(a.BountyDomains) > 0 {
		m = append(m, cbor.Pair{Key: uint64(9), Val: core.CBORStringsVal(a.BountyDomains)})
	}
	if len(a.Packs) > 0 {
		pks := make([]cbor.Value, len(a.Packs))
		for i, pk := range a.Packs {
			pks[i] = packRefToCBOR(pk)
		}
		m = append(m, cbor.Pair{Key: uint64(10), Val: pks})
	}
	if a.BondOutpoint != "" {
		m = append(m, cbor.Pair{Key: uint64(11), Val: a.BondOutpoint})
	}
	if a.BondBEEFHex != "" {
		m = append(m, cbor.Pair{Key: uint64(12), Val: a.BondBEEFHex})
	}
	if a.IsHub {
		m = append(m, cbor.Pair{Key: uint64(13), Val: true})
	}
	if a.HubRole != "" {
		m = append(m, cbor.Pair{Key: uint64(14), Val: a.HubRole})
	}
	if a.HubPayIdentityKey != "" {
		m = append(m, cbor.Pair{Key: uint64(15), Val: a.HubPayIdentityKey})
	}
	if a.HubTermsURL != "" {
		m = append(m, cbor.Pair{Key: uint64(16), Val: a.HubTermsURL})
	}
	if a.HubFees != nil {
		fm := cbor.Map{}
		if a.HubFees.SubmitWantSats > 0 {
			fm = append(fm, cbor.Pair{Key: uint64(1), Val: a.HubFees.SubmitWantSats})
		}
		if a.HubFees.SubmitOfferSats > 0 {
			fm = append(fm, cbor.Pair{Key: uint64(2), Val: a.HubFees.SubmitOfferSats})
		}
		if a.HubFees.SubmitAnnounceSats > 0 {
			fm = append(fm, cbor.Pair{Key: uint64(3), Val: a.HubFees.SubmitAnnounceSats})
		}
		if a.HubFees.BboxSendSats > 0 {
			fm = append(fm, cbor.Pair{Key: uint64(4), Val: a.HubFees.BboxSendSats})
		}
		if a.HubFees.EgressSatsPer100Events > 0 {
			fm = append(fm, cbor.Pair{Key: uint64(5), Val: a.HubFees.EgressSatsPer100Events})
		}
		if a.HubFees.EgressMinTopupSats > 0 {
			fm = append(fm, cbor.Pair{Key: uint64(6), Val: a.HubFees.EgressMinTopupSats})
		}
		m = append(m, cbor.Pair{Key: uint64(17), Val: fm})
	}
	return core.EncodeCBOR(m)
}

func decodeAnnounceCBOR(raw []byte) (AnnounceAd, []cbor.Pair, error) {
	v, err := core.DecodeCBOR(raw)
	if err != nil {
		return AnnounceAd{}, nil, fmt.Errorf("bstack: decode announce cbor: %w", err)
	}
	m, extra, err := core.SplitCBORMap(v, announceKnownKeys)
	if err != nil {
		return AnnounceAd{}, nil, fmt.Errorf("bstack: announce map: %w", err)
	}
	var a AnnounceAd
	if a.Pub, err = core.CBORText(m, 1); err != nil {
		return AnnounceAd{}, nil, err
	}
	if a.Net, err = core.CBORText(m, 2); err != nil {
		return AnnounceAd{}, nil, err
	}
	if a.Handle, err = core.CBORText(m, 3); err != nil {
		return AnnounceAd{}, nil, err
	}
	if a.PublicURL, err = core.CBORText(m, 4); err != nil {
		return AnnounceAd{}, nil, err
	}
	if a.BboxOffice, err = core.CBORText(m, 5); err != nil {
		return AnnounceAd{}, nil, err
	}
	if a.Hosts, err = core.CBORStrings(m, 6); err != nil {
		return AnnounceAd{}, nil, err
	}
	if a.Domains, err = core.CBORStrings(m, 7); err != nil {
		return AnnounceAd{}, nil, err
	}
	if a.SellDomains, err = core.CBORStrings(m, 8); err != nil {
		return AnnounceAd{}, nil, err
	}
	if a.BountyDomains, err = core.CBORStrings(m, 9); err != nil {
		return AnnounceAd{}, nil, err
	}
	if rawPacks, err := core.CBORArray(m, 10); err != nil {
		return AnnounceAd{}, nil, err
	} else if len(rawPacks) > 0 {
		a.Packs = make([]PackRef, len(rawPacks))
		for i, item := range rawPacks {
			pk, pErr := decodePackRefCBOR(item)
			if pErr != nil {
				return AnnounceAd{}, nil, fmt.Errorf("bstack: pack[%d]: %w", i, pErr)
			}
			a.Packs[i] = pk
		}
	}
	if a.BondOutpoint, err = core.CBORText(m, 11); err != nil {
		return AnnounceAd{}, nil, err
	}
	if a.BondBEEFHex, err = core.CBORText(m, 12); err != nil {
		return AnnounceAd{}, nil, err
	}
	if a.IsHub, err = core.CBORBool(m, 13); err != nil {
		return AnnounceAd{}, nil, err
	}
	if a.HubRole, err = core.CBORText(m, 14); err != nil {
		return AnnounceAd{}, nil, err
	}
	if a.HubPayIdentityKey, err = core.CBORText(m, 15); err != nil {
		return AnnounceAd{}, nil, err
	}
	if a.HubTermsURL, err = core.CBORText(m, 16); err != nil {
		return AnnounceAd{}, nil, err
	}
	if fv, ok := m[17]; ok {
		fm, _, fErr := core.SplitCBORMap(fv, hubFeesKnownKeys)
		if fErr != nil {
			return AnnounceAd{}, nil, fmt.Errorf("bstack: hubFees: %w", fErr)
		}
		hf := &HubFeeAdvert{}
		if hf.SubmitWantSats, err = core.CBORUint(fm, 1); err != nil {
			return AnnounceAd{}, nil, err
		}
		if hf.SubmitOfferSats, err = core.CBORUint(fm, 2); err != nil {
			return AnnounceAd{}, nil, err
		}
		if hf.SubmitAnnounceSats, err = core.CBORUint(fm, 3); err != nil {
			return AnnounceAd{}, nil, err
		}
		if hf.BboxSendSats, err = core.CBORUint(fm, 4); err != nil {
			return AnnounceAd{}, nil, err
		}
		if hf.EgressSatsPer100Events, err = core.CBORUint(fm, 5); err != nil {
			return AnnounceAd{}, nil, err
		}
		if hf.EgressMinTopupSats, err = core.CBORUint(fm, 6); err != nil {
			return AnnounceAd{}, nil, err
		}
		a.HubFees = hf
	}
	if a.Exp, err = core.CBORInt(m, 18); err != nil {
		return AnnounceAd{}, nil, err
	}
	return a, extra, nil
}

// --- AttestationAd CBOR ---

var attestationKnownKeys = core.KnownKeyRange(1, 12)

func (a AttestationAd) DomainTag() []byte { return attestationDomainTag }

func (a AttestationAd) EncodeCanonicalCBOR() ([]byte, error) {
	m := cbor.Map{
		{Key: uint64(1), Val: a.AttestHash},
		{Key: uint64(2), Val: a.Role},
		{Key: uint64(3), Val: a.Pub},
		{Key: uint64(4), Val: a.SubjectPub},
		{Key: uint64(5), Val: a.Net},
		{Key: uint64(8), Val: a.DealID},
		{Key: uint64(9), Val: a.DealReceiptHash},
		{Key: uint64(10), Val: a.Token},
		{Key: uint64(11), Val: a.CompositeBP},
		{Key: uint64(12), Val: a.Exp},
	}
	if len(a.Domains) > 0 {
		m = append(m, cbor.Pair{Key: uint64(6), Val: core.CBORStringsVal(a.Domains)})
	}
	if a.OntologyID != "" {
		m = append(m, cbor.Pair{Key: uint64(7), Val: a.OntologyID})
	}
	return core.EncodeCBOR(m)
}

func decodeAttestationCBOR(raw []byte) (AttestationAd, []cbor.Pair, error) {
	v, err := core.DecodeCBOR(raw)
	if err != nil {
		return AttestationAd{}, nil, fmt.Errorf("bstack: decode attestation cbor: %w", err)
	}
	m, extra, err := core.SplitCBORMap(v, attestationKnownKeys)
	if err != nil {
		return AttestationAd{}, nil, fmt.Errorf("bstack: attestation map: %w", err)
	}
	var a AttestationAd
	if a.AttestHash, err = core.CBORText(m, 1); err != nil {
		return AttestationAd{}, nil, err
	}
	if a.Role, err = core.CBORText(m, 2); err != nil {
		return AttestationAd{}, nil, err
	}
	if a.Pub, err = core.CBORText(m, 3); err != nil {
		return AttestationAd{}, nil, err
	}
	if a.SubjectPub, err = core.CBORText(m, 4); err != nil {
		return AttestationAd{}, nil, err
	}
	if a.Net, err = core.CBORText(m, 5); err != nil {
		return AttestationAd{}, nil, err
	}
	if a.Domains, err = core.CBORStrings(m, 6); err != nil {
		return AttestationAd{}, nil, err
	}
	if a.OntologyID, err = core.CBORText(m, 7); err != nil {
		return AttestationAd{}, nil, err
	}
	if a.DealID, err = core.CBORText(m, 8); err != nil {
		return AttestationAd{}, nil, err
	}
	if a.DealReceiptHash, err = core.CBORText(m, 9); err != nil {
		return AttestationAd{}, nil, err
	}
	if a.Token, err = core.CBORText(m, 10); err != nil {
		return AttestationAd{}, nil, err
	}
	if a.CompositeBP, err = core.CBORInt(m, 11); err != nil {
		return AttestationAd{}, nil, err
	}
	if a.Exp, err = core.CBORInt(m, 12); err != nil {
		return AttestationAd{}, nil, err
	}
	return a, extra, nil
}

// --- Signed Envelope Helpers ({1: rawCanonicalAdCBOR (bstr), 2: sigDERBytes (bstr)}) ---

var signedEnvelopeKnownKeys = core.KnownKeyRange(1, 2)

func encodeSignedEnvelope(rawAd []byte, sigHex string) ([]byte, error) {
	sigBytes, err := hex.DecodeString(strings.TrimSpace(sigHex))
	if err != nil {
		return nil, fmt.Errorf("bstack: invalid signature hex: %w", err)
	}
	return core.EncodeCBOR(cbor.Map{
		{Key: uint64(1), Val: rawAd},
		{Key: uint64(2), Val: sigBytes},
	})
}

func decodeSignedEnvelope(body []byte) (rawAd []byte, sigHex string, err error) {
	v, err := core.DecodeCBOR(body)
	if err != nil {
		return nil, "", fmt.Errorf("bstack: decode signed envelope: %w", err)
	}
	m, _, err := core.SplitCBORMap(v, signedEnvelopeKnownKeys)
	if err != nil {
		return nil, "", fmt.Errorf("bstack: signed envelope map: %w", err)
	}
	rawAd, err = core.CBORBytes(m, 1)
	if err != nil || len(rawAd) == 0 {
		if err == nil {
			err = errors.New("bstack: signed envelope missing payload bytes")
		}
		return nil, "", err
	}
	sigBytes, err := core.CBORBytes(m, 2)
	if err != nil {
		return nil, "", err
	}
	return rawAd, hex.EncodeToString(sigBytes), nil
}

// NewWantPayload encodes a SignedWant into a pure RFC 8949 CDE CBOR MarketPayload.
func NewWantPayload(sw SignedWant) (MarketPayload, error) {
	raw := sw.RawCBOR
	if len(raw) == 0 {
		var err error
		raw, err = sw.Want.EncodeCanonicalCBOR()
		if err != nil {
			return MarketPayload{}, err
		}
	}
	body, err := encodeSignedEnvelope(raw, sw.Sig)
	if err != nil {
		return MarketPayload{}, err
	}
	return MarketPayload{Kind: KindWant, Body: body}, nil
}

// DecodeWantPayload decodes a pure CBOR SignedWant body (preserving RawCBOR for exact signature verification).
func DecodeWantPayload(body []byte) (SignedWant, error) {
	rawAd, sigHex, err := decodeSignedEnvelope(body)
	if err != nil {
		return SignedWant{}, err
	}
	w, _, err := decodeWantCBOR(rawAd)
	if err != nil {
		return SignedWant{}, err
	}
	return SignedWant{Want: w, Sig: sigHex, RawCBOR: rawAd}, nil
}

// NewOfferPayload encodes a SignedOffer into a pure RFC 8949 CDE CBOR MarketPayload.
func NewOfferPayload(so SignedOffer) (MarketPayload, error) {
	raw := so.RawCBOR
	if len(raw) == 0 {
		var err error
		raw, err = so.Offer.EncodeCanonicalCBOR()
		if err != nil {
			return MarketPayload{}, err
		}
	}
	body, err := encodeSignedEnvelope(raw, so.Sig)
	if err != nil {
		return MarketPayload{}, err
	}
	return MarketPayload{Kind: KindOffer, Body: body}, nil
}

// DecodeOfferPayload decodes a pure CBOR SignedOffer body.
func DecodeOfferPayload(body []byte) (SignedOffer, error) {
	rawAd, sigHex, err := decodeSignedEnvelope(body)
	if err != nil {
		return SignedOffer{}, err
	}
	o, _, err := decodeOfferCBOR(rawAd)
	if err != nil {
		return SignedOffer{}, err
	}
	return SignedOffer{Offer: o, Sig: sigHex, RawCBOR: rawAd}, nil
}

// NewAnnouncePayload encodes a SignedAnnounce into a pure RFC 8949 CDE CBOR MarketPayload.
func NewAnnouncePayload(sa SignedAnnounce) (MarketPayload, error) {
	raw := sa.RawCBOR
	if len(raw) == 0 {
		var err error
		raw, err = sa.Announce.EncodeCanonicalCBOR()
		if err != nil {
			return MarketPayload{}, err
		}
	}
	body, err := encodeSignedEnvelope(raw, sa.Sig)
	if err != nil {
		return MarketPayload{}, err
	}
	return MarketPayload{Kind: KindAnnounce, Body: body}, nil
}

// DecodeAnnouncePayload decodes a pure CBOR SignedAnnounce body.
func DecodeAnnouncePayload(body []byte) (SignedAnnounce, error) {
	rawAd, sigHex, err := decodeSignedEnvelope(body)
	if err != nil {
		return SignedAnnounce{}, err
	}
	a, _, err := decodeAnnounceCBOR(rawAd)
	if err != nil {
		return SignedAnnounce{}, err
	}
	return SignedAnnounce{Announce: a, Sig: sigHex, RawCBOR: rawAd}, nil
}

// NewAttestationPayload encodes a SignedAttestation into a pure RFC 8949 CDE CBOR MarketPayload.
func NewAttestationPayload(sat SignedAttestation) (MarketPayload, error) {
	raw := sat.RawCBOR
	if len(raw) == 0 {
		var err error
		raw, err = sat.Attestation.EncodeCanonicalCBOR()
		if err != nil {
			return MarketPayload{}, err
		}
	}
	body, err := encodeSignedEnvelope(raw, sat.Sig)
	if err != nil {
		return MarketPayload{}, err
	}
	return MarketPayload{Kind: KindAttestation, Body: body}, nil
}

// DecodeAttestationPayload decodes a pure CBOR SignedAttestation body.
func DecodeAttestationPayload(body []byte) (SignedAttestation, error) {
	rawAd, sigHex, err := decodeSignedEnvelope(body)
	if err != nil {
		return SignedAttestation{}, err
	}
	att, _, err := decodeAttestationCBOR(rawAd)
	if err != nil {
		return SignedAttestation{}, err
	}
	return SignedAttestation{Attestation: att, Sig: sigHex, RawCBOR: rawAd}, nil
}
