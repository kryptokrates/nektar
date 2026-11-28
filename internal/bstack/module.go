package bstack

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkpushdrop "github.com/bsv-blockchain/go-sdk/transaction/template/pushdrop"
	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/module"
	"github.com/lightwebinc/bcommon/record"

	"github.com/kryptokrates/nektar/internal/nmu"
)

// MarketEvent is emitted whenever the Nektar overlay LookupService admits a verified market artifact.
// The embedded Hub assigns monotonic Seq numbers and fans these out over cursored SSE (/events).
// BEEF carries the verified transaction bytes so subscribers & federation peers verify locally without refetch.
type MarketEvent struct {
	Seq        int64         `json:"seq,omitempty"`
	Topic      string        `json:"topic"`
	TxID       string        `json:"txid"`
	Vout       uint32        `json:"vout"`
	AdmittedAt int64         `json:"admittedAt"`
	Payload    MarketPayload `json:"payload"`
	BEEF       []byte        `json:"beef,omitempty"`
	RawJSON    []byte        `json:"-"` // optional pre-marshaled JSON shared across Hub SSE fanout
	SlimJSON   []byte        `json:"-"` // optional pre-marshaled JSON omitting raw BEEF for ?format=slim SSE clients
	BuyerPub   string        `json:"-"` // optional OfferAd recipient pubkey for targeted Hub SSE delivery
}

// MarketTopicManager implements lightwebinc/bcommon/module.TopicManager for one `tm_nektar_<net>_<nibble>` lane.
type MarketTopicManager struct {
	Net    string
	Topic  string
	Nibble string
	Now    func() time.Time
}

func (m *MarketTopicManager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// ExtractNkt1Payload extracts the nkt1 canonical CBOR payload from a PushDrop locking script or OP_RETURN data push.
func ExtractNkt1Payload(s *script.Script) (MarketPayload, bool, error) {
	if s == nil {
		return MarketPayload{}, false, nil
	}
	// 1. Canonical PushDrop carrier (Lightweb native format).
	if pd := sdkpushdrop.Decode(s); pd != nil && len(pd.Fields) > 0 {
		p, decErr := DecodePayload(pd.Fields[0])
		if decErr == nil {
			return p, true, nil
		}
	}
	// 2. Also inspect raw data pushes (OP_RETURN / script pushes carrying nkt1 CBOR).
	ops, err := s.ParseOps()
	if err != nil {
		return MarketPayload{}, false, nil
	}
	for _, op := range ops {
		if len(op.Data) >= 6 {
			if p, decErr := DecodePayload(op.Data); decErr == nil {
				return p, true, nil
			}
		}
	}
	return MarketPayload{}, false, nil
}

// ValidateMarketPayload verifies the signed body, network tag, domain bounds, lane routing, and expiry.
func ValidateMarketPayload(p MarketPayload, expectedNet, expectedTopic string, now time.Time) error {
	nowMs := now.UnixMilli()
	maxExp := now.Add(MaxLifetime + 5*time.Minute).UnixMilli()
	switch p.Kind {
	case KindWant:
		sw, err := DecodeWantPayload(p.Body)
		if err != nil {
			return fmt.Errorf("invalid want CBOR: %w", err)
		}
		if sw.Want.WantID == "" || sw.Want.Pub == "" {
			return errors.New("want missing wantId or pub")
		}
		if expectedNet != "" && sw.Want.Net != expectedNet {
			return fmt.Errorf("want net %q != %q", sw.Want.Net, expectedNet)
		}
		if len(sw.Want.Domains) == 0 || len(sw.Want.Domains) > MaxDomains {
			return errors.New("want domains out of range")
		}
		if len(sw.Want.Query) > 1024 {
			return errors.New("want query exceeds 1024 bytes")
		}
		if err := validateOptionalHash32("want ontologyId", sw.Want.OntologyID); err != nil {
			return err
		}
		if err := nmu.ValidateWantFilters(sw.Want.MinGrade, sw.Want.FreshnessClass, sw.Want.RequiredRights, sw.Want.PricingMode); err != nil {
			return fmt.Errorf("want %w", err)
		}
		if sw.Want.Exp <= 0 || sw.Want.Exp < nowMs || sw.Want.Exp > maxExp {
			return errors.New("want missing exp, expired, or lifetime too long")
		}
		if err := VerifyArtifactRaw(sw.Want.Pub, sw.Sig, sw.Want.DomainTag(), sw.RawCBOR); err != nil {
			return err
		}
		if expectedTopic != "" && !topicMatchesAd(expectedNet, expectedTopic, sw.Want.Domains, sw.Want.OntologyID) {
			return errors.New("want domain or ontology does not match topic lane")
		}
		return nil

	case KindOffer:
		so, err := DecodeOfferPayload(p.Body)
		if err != nil {
			return fmt.Errorf("invalid offer CBOR: %w", err)
		}
		if so.Offer.OfferID == "" || so.Offer.WantID == "" || so.Offer.Pub == "" {
			return errors.New("offer missing required ids or pub")
		}
		leafRaw, lErr := hex.DecodeString(strings.TrimSpace(so.Offer.Leaf))
		if lErr != nil || len(leafRaw) != 32 {
			return errors.New("offer missing or invalid 32-byte hex leaf")
		}
		if err := validateOptionalHash32("offer ontologyId", so.Offer.OntologyID); err != nil {
			return err
		}
		if err := validateOptionalHash32("offer edgesRoot", so.Offer.EdgesRoot); err != nil {
			return err
		}
		if err := nmu.ValidateOfferSpec(nmu.Spec{
			Grade:           nmu.Grade(so.Offer.Grade),
			UnitCount:       so.Offer.UnitCount,
			SatsPer1kUnits:  so.Offer.SatsPer1kUnits,
			FreshnessClass:  nmu.FreshnessClass(so.Offer.FreshnessClass),
			ValidUntilUnix:  so.Offer.ValidUntilUnix,
			SubjectVersion:  so.Offer.SubjectVersion,
			UsageRights:     nmu.UsageRights(so.Offer.UsageRights),
			PricingMode:     nmu.PricingMode(so.Offer.PricingMode),
			SubscriptionSec: so.Offer.SubscriptionSec,
			FeedURL:         so.Offer.FeedURL,
		}, so.Offer.PriceSats); err != nil {
			return fmt.Errorf("offer %w", err)
		}
		if expectedNet != "" && so.Offer.Net != expectedNet {
			return fmt.Errorf("offer net %q != %q", so.Offer.Net, expectedNet)
		}
		if len(so.Offer.Domains) == 0 || len(so.Offer.Domains) > MaxDomains {
			return errors.New("offer domains out of range")
		}
		if len(so.Offer.Description) > 280 {
			return errors.New("offer description exceeds 280 bytes")
		}
		if len(so.Offer.Tags) > 8 {
			return errors.New("offer tags exceed max 8")
		}
		for _, tag := range so.Offer.Tags {
			if len(tag) > 32 {
				return errors.New("offer tag exceeds 32 bytes")
			}
		}
		if so.Offer.Exp <= 0 || so.Offer.Exp < nowMs || so.Offer.Exp > maxExp {
			return errors.New("offer missing exp, expired, or lifetime too long")
		}
		if err := VerifyArtifactRaw(so.Offer.Pub, so.Sig, so.Offer.DomainTag(), so.RawCBOR); err != nil {
			return err
		}
		if expectedTopic != "" && !topicMatchesAd(expectedNet, expectedTopic, so.Offer.Domains, so.Offer.OntologyID) {
			return errors.New("offer domain or ontology does not match topic lane")
		}
		return nil

	case KindAnnounce:
		sa, err := DecodeAnnouncePayload(p.Body)
		if err != nil {
			return fmt.Errorf("invalid announce CBOR: %w", err)
		}
		if sa.Announce.Pub == "" || len(sa.Announce.Hosts) == 0 {
			return errors.New("announce missing pub or hosts")
		}
		if expectedNet != "" && sa.Announce.Net != expectedNet {
			return fmt.Errorf("announce net %q != %q", sa.Announce.Net, expectedNet)
		}
		if sa.Announce.Exp <= 0 || sa.Announce.Exp < nowMs || sa.Announce.Exp > maxExp {
			return errors.New("announce missing exp, expired, or lifetime too long")
		}
		var packOnts []string
		for _, pk := range sa.Announce.Packs {
			if err := validateOptionalHash32("pack ontologyId", pk.OntologyID); err != nil {
				return err
			}
			if err := validateOptionalHash32("pack targetOntologyId", pk.TargetOntologyID); err != nil {
				return err
			}
			if err := validatePackRefNMU(pk); err != nil {
				return err
			}
			if pk.OntologyID != "" {
				packOnts = append(packOnts, pk.OntologyID)
			}
		}
		if err := VerifyArtifactRaw(sa.Announce.Pub, sa.Sig, sa.Announce.DomainTag(), sa.RawCBOR); err != nil {
			return err
		}
		if expectedTopic != "" {
			allowed := expectedTopic == LaneOf(expectedNet, DirDomain) ||
				expectedTopic == DomainTopic(expectedNet, DirDomain) ||
				topicMatchesAd(expectedNet, expectedTopic, sa.Announce.Domains, "") ||
				topicMatchesAd(expectedNet, expectedTopic, sa.Announce.SellDomains, "")
			if !allowed {
				for _, oid := range packOnts {
					if OntologyTopic(expectedNet, oid) == expectedTopic {
						allowed = true
						break
					}
				}
			}
			if !allowed {
				return errors.New("announce only allowed on dir or matching semantic/ontology lane")
			}
		}
		return nil

	case KindAttestation:
		sat, err := DecodeAttestationPayload(p.Body)
		if err != nil {
			return fmt.Errorf("invalid attestation CBOR: %w", err)
		}
		att := sat.Attestation
		if att.AttestHash == "" || att.Pub == "" || att.SubjectPub == "" || att.DealID == "" || att.DealReceiptHash == "" {
			return errors.New("attestation missing required fields")
		}
		if strings.EqualFold(att.Pub, att.SubjectPub) {
			return errors.New("attestation self-rating is forbidden")
		}
		if att.Role != "buyer_rates_seller" && att.Role != "seller_rates_buyer" {
			return fmt.Errorf("attestation invalid role %q", att.Role)
		}
		if !strings.HasPrefix(strings.TrimSpace(att.Token), "nektarrep1.") {
			return errors.New("attestation token must start with nektarrep1.")
		}
		if len(att.Token) > 12288 {
			return errors.New("attestation token exceeds size cap")
		}
		if err := validateOptionalHash32("attestation attestHash", att.AttestHash); err != nil {
			return err
		}
		if err := validateOptionalHash32("attestation dealReceiptHash", att.DealReceiptHash); err != nil {
			return err
		}
		if att.OntologyID != "" && !strings.EqualFold(att.OntologyID, "global") {
			if err := validateOptionalHash32("attestation ontologyId", att.OntologyID); err != nil {
				return err
			}
		}
		if att.CompositeBP < 0 || att.CompositeBP > 10000 {
			return errors.New("attestation compositeBp out of range [0, 10000]")
		}
		if expectedNet != "" && att.Net != expectedNet {
			return fmt.Errorf("attestation net %q != %q", att.Net, expectedNet)
		}
		if len(att.Domains) > MaxDomains {
			return errors.New("attestation domains out of range")
		}
		if att.Exp <= 0 || att.Exp < nowMs || att.Exp > maxExp {
			return errors.New("attestation missing exp, expired, or lifetime too long")
		}
		if err := VerifyArtifactRaw(att.Pub, sat.Sig, att.DomainTag(), sat.RawCBOR); err != nil {
			return err
		}
		if expectedTopic != "" {
			doms := att.Domains
			if len(doms) == 0 {
				doms = []string{DirDomain}
			}
			if !topicMatchesAd(expectedNet, expectedTopic, doms, att.OntologyID) &&
				expectedTopic != LaneOf(expectedNet, DirDomain) &&
				expectedTopic != DomainTopic(expectedNet, DirDomain) {
				return errors.New("attestation domain does not match topic lane")
			}
		}
		return nil

	default:
		return fmt.Errorf("unsupported market kind %q", p.Kind)
	}
}

func validateOptionalHash32(label, val string) error {
	s := strings.TrimSpace(val)
	if s == "" {
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return fmt.Errorf("%s must be 32-byte hex", label)
	}
	return nil
}

func validatePackRefNMU(pk PackRef) error {
	if pk.Grade != "" {
		g := nmu.NormalizeGrade(pk.Grade)
		if !nmu.ValidGrade(g) {
			return fmt.Errorf("pack invalid grade %q", pk.Grade)
		}
		if g == nmu.GradeS && strings.TrimSpace(pk.FeedEndpoint) == "" {
			return errors.New("pack grade S requires feedEndpoint")
		}
	}
	if pk.UnitCount > nmu.MaxUnitCount {
		return fmt.Errorf("pack unitCount %d exceeds max %d", pk.UnitCount, nmu.MaxUnitCount)
	}
	if pk.PricingMode != "" && !nmu.ValidPricingMode(nmu.NormalizePricingMode(pk.PricingMode)) {
		return fmt.Errorf("pack invalid pricingMode %q", pk.PricingMode)
	}
	if len(pk.EnabledModes) > 3 {
		return errors.New("pack enabledModes exceeds max 3")
	}
	for _, m := range pk.EnabledModes {
		if !nmu.ValidPricingMode(nmu.NormalizePricingMode(m)) {
			return fmt.Errorf("pack invalid enabledMode %q", m)
		}
	}
	if pk.FreshnessClass != "" && !nmu.ValidFreshness(nmu.NormalizeFreshness(pk.FreshnessClass)) {
		return fmt.Errorf("pack invalid freshnessClass %q", pk.FreshnessClass)
	}
	if pk.UsageRights != "" && !nmu.ValidRights(nmu.NormalizeRights(pk.UsageRights)) {
		return fmt.Errorf("pack invalid usageRights %q", pk.UsageRights)
	}
	if pk.SubscriptionSec < 0 {
		return errors.New("pack subscriptionSec cannot be negative")
	}
	if len(pk.QueryEndpoint) > nmu.MaxFeedURLLen || len(pk.FeedEndpoint) > nmu.MaxFeedURLLen {
		return errors.New("pack endpoint exceeds max length")
	}
	if pk.SatsPer1kUnits > 0 && pk.PriceSats > 0 {
		want := nmu.ComputeSatsPer1kUnits(pk.PriceSats, pk.UnitCount)
		if pk.SatsPer1kUnits != want {
			return fmt.Errorf("pack satsPer1kUnits %d does not match priceSats=%d unitCount=%d (want %d)",
				pk.SatsPer1kUnits, pk.PriceSats, pk.UnitCount, want)
		}
	}
	return nil
}

func topicMatchesAd(net, topic string, domains []string, ontologyID string) bool {
	if ot := OntologyTopic(net, ontologyID); ot != "" && ot == topic {
		return true
	}
	for _, d := range domains {
		if DomainTopic(net, d) == topic || LaneOf(net, d) == topic {
			return true
		}
	}
	return false
}

// DynamicTopicForMarket returns an on-demand MarketTopicManager when topic is a valid
// `tm_nektar_<net>_dom_<slug>` or `tm_nektar_<net>_ont_<id16>` topic for net.
func DynamicTopicForMarket(net, topic string) *MarketTopicManager {
	if !IsValidDynamicMarketTopic(net, topic) {
		return nil
	}
	return &MarketTopicManager{
		Net:   net,
		Topic: topic,
	}
}

// ClassifyNkt1 implements bcommon/carrier.Classify for canonical CBOR nkt1 PushDrop records.
func ClassifyNkt1(payload []byte) (bool, error) {
	if !record.Claims(payload, MarketMagic) {
		return false, nil
	}
	if _, err := DecodePayload(payload); err != nil {
		return true, err
	}
	return true, nil
}

// ExtractPayloadIdentity returns the compressed 33-byte identity pubkey declared inside a MarketPayload.
func ExtractPayloadIdentity(p MarketPayload) ([]byte, error) {
	var pubHex string
	switch p.Kind {
	case KindWant:
		sw, err := DecodeWantPayload(p.Body)
		if err != nil {
			return nil, fmt.Errorf("invalid want CBOR: %w", err)
		}
		pubHex = sw.Want.Pub
	case KindOffer:
		so, err := DecodeOfferPayload(p.Body)
		if err != nil {
			return nil, fmt.Errorf("invalid offer CBOR: %w", err)
		}
		pubHex = so.Offer.Pub
	case KindAnnounce:
		sa, err := DecodeAnnouncePayload(p.Body)
		if err != nil {
			return nil, fmt.Errorf("invalid announce CBOR: %w", err)
		}
		pubHex = sa.Announce.Pub
	case KindAttestation:
		sat, err := DecodeAttestationPayload(p.Body)
		if err != nil {
			return nil, fmt.Errorf("invalid attestation CBOR: %w", err)
		}
		pubHex = sat.Attestation.Pub
	default:
		return nil, fmt.Errorf("unsupported market kind %q", p.Kind)
	}
	raw, err := hex.DecodeString(strings.TrimSpace(pubHex))
	if err != nil || len(raw) != 33 {
		return nil, errors.New("invalid 33-byte compressed identity pubkey")
	}
	return raw, nil
}

// VerifyMarketCarrier decodes and enforces all bcommon/carrier REFUSED-* rules
// (canonical PushDrop shape, unmineable locktime/sequence, canonical single-push unlocking script,
// BRC-42 key derivation from the payload's declared identity key, and PushDrop field signature)
// followed by ValidateMarketPayload.
func VerifyMarketCarrier(tx *transaction.Transaction, expectedNet, expectedTopic string, now time.Time) (uint32, MarketPayload, error) {
	c, err := carrier.Decode(tx, ClassifyNkt1)
	if err != nil {
		return 0, MarketPayload{}, err
	}
	p, err := DecodePayload(c.Payload)
	if err != nil {
		return 0, MarketPayload{}, err
	}
	idBytes, err := ExtractPayloadIdentity(p)
	if err != nil {
		return 0, MarketPayload{}, err
	}
	if err := c.Validate(MarketCarrierParams, idBytes); err != nil {
		return 0, MarketPayload{}, err
	}
	if err := ValidateMarketPayload(p, expectedNet, expectedTopic, now); err != nil {
		return 0, MarketPayload{}, err
	}
	return c.OutputIndex, p, nil
}

func (m *MarketTopicManager) IdentifyAdmissibleOutputs(_ context.Context, beef *transaction.Beef, txid *chainhash.Hash, _ []uint32) (overlay.AdmittanceInstructions, error) {
	var out overlay.AdmittanceInstructions
	if beef == nil || txid == nil {
		return out, nil
	}
	tx := beef.FindTransaction(txid.String())
	if tx == nil {
		return out, nil
	}
	now := m.now()
	isLive := m.Net == "main" || m.Net == "testnet"
	if isLive && (len(tx.Inputs) > 0 || os.Getenv("NEKTAR_OFFCHAIN_ADS") != "1") {
		vout, _, err := VerifyMarketCarrier(tx, m.Net, m.Topic, now)
		if err != nil {
			return out, fmt.Errorf("market carrier refused: %w", err)
		}
		out.OutputsToAdmit = append(out.OutputsToAdmit, vout)
		return out, nil
	}
	// On memory/regtest: if the transaction carries a PushDrop carrier (with inputs), enforce
	// the full REFUSED-* carrier contract so tests can exercise live carrier rules directly.
	if len(tx.Inputs) > 0 {
		vout, _, err := VerifyMarketCarrier(tx, m.Net, m.Topic, now)
		if err == nil {
			out.OutputsToAdmit = append(out.OutputsToAdmit, vout)
			return out, nil
		}
		if !errors.Is(err, carrier.ErrNotCarrier) {
			return out, fmt.Errorf("market carrier refused: %w", err)
		}
	}
	for i, txOut := range tx.Outputs {
		p, ok, _ := ExtractNkt1Payload(txOut.LockingScript)
		if !ok {
			continue
		}
		if err := ValidateMarketPayload(p, m.Net, m.Topic, now); err == nil {
			out.OutputsToAdmit = append(out.OutputsToAdmit, uint32(i))
		}
	}
	return out, nil
}

func (m *MarketTopicManager) GetDocumentation() string {
	return "Nektar v0.1 market lane topic manager (`tm_nektar_<net>_<nibble>`) validating signed canonical CBOR `nkt1` wants, offers, and directory announcements."
}

func (m *MarketTopicManager) GetMetaData() *overlay.MetaData {
	return &overlay.MetaData{
		Name:        m.Topic,
		Description: "Nektar v0.1 signed market lane",
		Version:     "0.1.0",
	}
}

// ParseLaneNibbles normalizes an optional slice or comma-separated list of hex nibbles ("0".."f")
// into a deduplicated ordered slice. Empty input returns all 16 nibble lanes ("0".."f").
func ParseLaneNibbles(raw []string) []string {
	seen := make(map[string]bool, 16)
	var out []string
	for _, item := range raw {
		for _, tok := range strings.Split(item, ",") {
			tok = strings.ToLower(strings.TrimSpace(tok))
			if idx := strings.LastIndex(tok, "_"); idx >= 0 && idx+1 < len(tok) {
				tok = tok[idx+1:]
			}
			if len(tok) == 1 && ((tok[0] >= '0' && tok[0] <= '9') || (tok[0] >= 'a' && tok[0] <= 'f')) && !seen[tok] {
				seen[tok] = true
				out = append(out, tok)
			}
		}
	}
	if len(out) == 0 {
		out = make([]string, 16)
		for i := range 16 {
			out[i] = fmt.Sprintf("%x", i)
		}
	}
	return out
}

// NewMarketModule returns a Lightweb-compatible module.Factory registering all 16 nibble topics
// (`tm_nektar_<net>_0..f`) and `ls_nektar`.
func NewMarketModule(net string, onAdmit func(MarketEvent)) module.Factory {
	return NewMarketModuleWithLanes(net, nil, onAdmit)
}

// NewMarketModuleWithLanes registers only the configured subset of nibble lanes (or all 0..f when lanes is empty),
// enabling horizontal Hub sharding across independent nodes.
func NewMarketModuleWithLanes(net string, lanes []string, onAdmit func(MarketEvent)) module.Factory {
	nibbles := ParseLaneNibbles(lanes)
	return func(h module.Host) (module.Module, error) {
		topics := make(map[string]module.TopicManager, len(nibbles))
		for _, nibble := range nibbles {
			name := fmt.Sprintf("tm_nektar_%s_%s", net, nibble)
			topics[name] = &MarketTopicManager{
				Net:    net,
				Topic:  name,
				Nibble: nibble,
			}
		}
		return module.Module{
			Topics: topics,
			Lookups: map[string]module.LookupService{
				LookupService: NewMarketLookupService(h, onAdmit),
			},
		}, nil
	}
}
