package hub

import (
	"context"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/lightwebinc/bcommon/bwallet"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
)

// HubRole defines a Hub's position in Nektar's Spine-and-Leaf topology (adapted from @LightBSV's
// deterministic sharded plumbing):
//   - RoleSpine: backbone hub carrying sharded slices; peers settlement-free ONLY with explicitly
//     configured bilateral Spine peers (NEKTAR_PEER_HUBS + SpinePeerPubs), accepts POST /submit UP
//     from Leaf hubs and direct agents, and meters GET /events & GET /sync DOWN.
//   - RoleLeaf: edge proxy hub serving local agents; forwards local ads UP to configured Spine(s)
//     via POST /submit and subscribes DOWN to interested domain slices (?domains=) paying Spine egress.
type HubRole string

const (
	RoleLeaf  HubRole = "leaf"
	RoleSpine HubRole = "spine"

	PresetFree           = "free"
	PresetLightBSVEgress = "lightbsv_egress"
	PresetHybrid         = "hybrid"

	ActionSubmitWant     = "submit:want"
	ActionSubmitOffer    = "submit:offer"
	ActionSubmitAnnounce = "submit:announce"
	ActionBboxSend       = "bbox:send"
	ActionEgressTopup    = "egress:topup"
	ActionLookupPrefix   = "lookup:"

	defaultPrefixTTL        = 10 * time.Minute
	defaultAcceptThreshold  = uint64(500)
	defaultAcceptPayerLimit = uint64(500)
	defaultAcceptTotalLimit = uint64(5000)
	defaultAcceptWindow     = time.Hour
	defaultMinTopupSats     = uint64(20)
)

// FeeSchedule configures a Hub operator's 4-valve Spine-and-Leaf monetization policy.
// All satoshi prices default to 0 (free) unless explicitly configured or a preset is enabled.
type FeeSchedule struct {
	Role                   HubRole           `json:"role"`
	Preset                 string            `json:"preset,omitempty"`
	SubmitWantSats         uint64            `json:"submitWantSats"`
	SubmitOfferSats        uint64            `json:"submitOfferSats"`
	SubmitAnnounceSats     uint64            `json:"submitAnnounceSats"`
	BboxSendSats           uint64            `json:"bboxSendSats"`
	EgressSatsPer100Events uint64            `json:"egressSatsPer100Events"`
	EgressMinTopupSats     uint64            `json:"egressMinTopupSats"`
	LookupPrices           map[string]uint64 `json:"lookupPrices,omitempty"` // e.g. {"history": 5, "export": 20}
	SpinePeerPubs          []string          `json:"spinePeerPubs,omitempty"`
	AcceptThresholdSats    uint64            `json:"acceptThresholdSats"`
	AcceptPayerLimitSats   uint64            `json:"acceptPayerLimitSats"`
	AcceptTotalLimitSats   uint64            `json:"acceptTotalLimitSats"`
	AcceptWindow           time.Duration     `json:"-"`
}

// DefaultFeeSchedule returns canonical fee presets ("free" | "lightbsv_egress" | "hybrid").
func DefaultFeeSchedule(preset string) FeeSchedule {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case PresetLightBSVEgress, "egress", "lightbsv":
		return FeeSchedule{
			Role:                   RoleLeaf,
			Preset:                 PresetLightBSVEgress,
			SubmitWantSats:         0,
			SubmitOfferSats:        0,
			SubmitAnnounceSats:     0,
			BboxSendSats:           2,
			EgressSatsPer100Events: 10,
			EgressMinTopupSats:     defaultMinTopupSats,
			LookupPrices:           map[string]uint64{"history": 5, "export": 20},
			AcceptThresholdSats:    defaultAcceptThreshold,
			AcceptPayerLimitSats:   defaultAcceptPayerLimit,
			AcceptTotalLimitSats:   defaultAcceptTotalLimit,
			AcceptWindow:           defaultAcceptWindow,
		}
	case PresetHybrid:
		return FeeSchedule{
			Role:                   RoleLeaf,
			Preset:                 PresetHybrid,
			SubmitWantSats:         5,
			SubmitOfferSats:        2,
			SubmitAnnounceSats:     0,
			BboxSendSats:           2,
			EgressSatsPer100Events: 10,
			EgressMinTopupSats:     defaultMinTopupSats,
			LookupPrices:           map[string]uint64{"history": 5, "export": 20},
			AcceptThresholdSats:    defaultAcceptThreshold,
			AcceptPayerLimitSats:   defaultAcceptPayerLimit,
			AcceptTotalLimitSats:   defaultAcceptTotalLimit,
			AcceptWindow:           defaultAcceptWindow,
		}
	default:
		return FeeSchedule{
			Role:                 RoleLeaf,
			Preset:               PresetFree,
			EgressMinTopupSats:   defaultMinTopupSats,
			AcceptThresholdSats:  defaultAcceptThreshold,
			AcceptPayerLimitSats: defaultAcceptPayerLimit,
			AcceptTotalLimitSats: defaultAcceptTotalLimit,
			AcceptWindow:         defaultAcceptWindow,
		}
	}
}

// LoadFeeSchedule builds a FeeSchedule from environment variables via getenv.
// Defaults to free (0 sats) unless NEKTAR_HUB_FEE_PRESET or explicit fee env vars are set.
func LoadFeeSchedule(getenv func(string) string) FeeSchedule {
	if getenv == nil {
		return DefaultFeeSchedule(PresetFree)
	}
	fs := DefaultFeeSchedule(getenv("NEKTAR_HUB_FEE_PRESET"))
	if role := strings.ToLower(strings.TrimSpace(getenv("NEKTAR_HUB_ROLE"))); role == string(RoleSpine) || role == string(RoleLeaf) {
		fs.Role = HubRole(role)
	}
	parseU64 := func(key string, dst *uint64) {
		if raw := strings.TrimSpace(getenv(key)); raw != "" {
			if v, err := strconv.ParseUint(raw, 10, 64); err == nil {
				*dst = v
			}
		}
	}
	parseU64("NEKTAR_HUB_SUBMIT_WANT_SATS", &fs.SubmitWantSats)
	parseU64("NEKTAR_HUB_SUBMIT_OFFER_SATS", &fs.SubmitOfferSats)
	parseU64("NEKTAR_HUB_SUBMIT_ANNOUNCE_SATS", &fs.SubmitAnnounceSats)
	parseU64("NEKTAR_HUB_BBOX_SEND_SATS", &fs.BboxSendSats)
	parseU64("NEKTAR_HUB_EGRESS_SATS_PER_100", &fs.EgressSatsPer100Events)
	parseU64("NEKTAR_HUB_EGRESS_MIN_TOPUP_SATS", &fs.EgressMinTopupSats)
	parseU64("NEKTAR_HUB_ACCEPT_THRESHOLD_SATS", &fs.AcceptThresholdSats)
	parseU64("NEKTAR_HUB_ACCEPT_PAYER_LIMIT_SATS", &fs.AcceptPayerLimitSats)
	parseU64("NEKTAR_HUB_ACCEPT_TOTAL_LIMIT_SATS", &fs.AcceptTotalLimitSats)
	if raw := strings.TrimSpace(getenv("NEKTAR_HUB_LOOKUP_PRICES")); raw != "" {
		fs.LookupPrices = ParseLookupPrices(raw)
	}
	if raw := strings.TrimSpace(getenv("NEKTAR_SPINE_PEER_PUBS")); raw != "" {
		for _, p := range strings.Split(raw, ",") {
			if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
				fs.SpinePeerPubs = append(fs.SpinePeerPubs, p)
			}
		}
	}
	fs.Normalize()
	return fs
}

// ParseLookupPrices parses a Lightweb-style price list like "history=5,export=20".
func ParseLookupPrices(raw string) map[string]uint64 {
	out := make(map[string]uint64)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(kv[0]))
		v, err := strconv.ParseUint(strings.TrimSpace(kv[1]), 10, 64)
		if err == nil && k != "" && v > 0 {
			out[k] = v
		}
	}
	return out
}

// Normalize fills safe defaults for missing role, exposure caps, and top-up minimums.
func (f *FeeSchedule) Normalize() {
	if f.Role != RoleSpine && f.Role != RoleLeaf {
		f.Role = RoleLeaf
	}
	if f.EgressMinTopupSats == 0 {
		f.EgressMinTopupSats = defaultMinTopupSats
	}
	if f.AcceptThresholdSats == 0 {
		f.AcceptThresholdSats = defaultAcceptThreshold
	}
	if f.AcceptPayerLimitSats == 0 {
		f.AcceptPayerLimitSats = defaultAcceptPayerLimit
	}
	if f.AcceptTotalLimitSats == 0 {
		f.AcceptTotalLimitSats = defaultAcceptTotalLimit
	}
	if f.AcceptWindow <= 0 {
		f.AcceptWindow = defaultAcceptWindow
	}
}

// HasAnyFees reports whether any of the 4 Hub fee valves has a non-zero price.
func (f FeeSchedule) HasAnyFees() bool {
	if f.SubmitWantSats > 0 || f.SubmitOfferSats > 0 || f.SubmitAnnounceSats > 0 || f.BboxSendSats > 0 || f.EgressSatsPer100Events > 0 {
		return true
	}
	for _, p := range f.LookupPrices {
		if p > 0 {
			return true
		}
	}
	return false
}

// FeeForKind returns the required ingress fee (satoshis) and canonical action name for a MarketPayload kind.
func (f FeeSchedule) FeeForKind(kind bstack.ArtifactKind) (uint64, string) {
	switch kind {
	case bstack.KindWant:
		return f.SubmitWantSats, ActionSubmitWant
	case bstack.KindOffer:
		return f.SubmitOfferSats, ActionSubmitOffer
	case bstack.KindAnnounce:
		return f.SubmitAnnounceSats, ActionSubmitAnnounce
	default:
		return 0, ""
	}
}

// EgressMilliSatsPerEvent returns the exact milli-satoshi cost per delivered market event (1000 msats = 1 sat).
func (f FeeSchedule) EgressMilliSatsPerEvent() int64 {
	if f.EgressSatsPer100Events == 0 {
		return 0
	}
	// satsPer100 * 1000 / 100 == satsPer100 * 10 milli-sats per event.
	return int64(f.EgressSatsPer100Events) * 10
}

// AdvertSummary returns the compact wire advertisement embedded in AnnounceAd.HubFees.
func (f FeeSchedule) AdvertSummary() *bstack.HubFeeAdvert {
	return &bstack.HubFeeAdvert{
		SubmitWantSats:         f.SubmitWantSats,
		SubmitOfferSats:        f.SubmitOfferSats,
		SubmitAnnounceSats:     f.SubmitAnnounceSats,
		BboxSendSats:           f.BboxSendSats,
		EgressSatsPer100Events: f.EgressSatsPer100Events,
		EgressMinTopupSats:     f.EgressMinTopupSats,
	}
}

// FeeCollector abstracts the Hub's BRC-29 payment verification & wallet internalization boundary.
type FeeCollector interface {
	IdentityPubHex() string
	Network() string
	DeriveBRC29Script(ctx context.Context, senderPub, prefixB64, suffixB64 string) (*script.Script, error)
	VerifyPaymentBEEF(ctx context.Context, rawBeef []byte, opts chain.PaymentVerifyOpts) (*chain.PaymentVerifyResult, error)
	InternalizeBRC29(ctx context.Context, rawBeef []byte, outputIndex uint32, prefixB64, suffixB64, senderPub, description string, labels []string) (uint64, error)
	ZeroConfWatch() *chain.ZeroConfWatch
	SettleDepth() int
}

// PortFeeCollector implements FeeCollector directly over a Nektar *chain.Port.
type PortFeeCollector struct {
	port *chain.Port
}

// NewPortFeeCollector wraps a Nektar chain.Port as a Hub FeeCollector.
func NewPortFeeCollector(port *chain.Port) *PortFeeCollector {
	if port == nil {
		return nil
	}
	return &PortFeeCollector{port: port}
}

func (c *PortFeeCollector) IdentityPubHex() string { return c.port.IdentityPubHex() }
func (c *PortFeeCollector) Network() string        { return c.port.Network() }
func (c *PortFeeCollector) ZeroConfWatch() *chain.ZeroConfWatch {
	return c.port.ZeroConfWatch()
}
func (c *PortFeeCollector) SettleDepth() int { return c.port.SettleDepth() }

func (c *PortFeeCollector) DeriveBRC29Script(ctx context.Context, senderPub, prefixB64, suffixB64 string) (*script.Script, error) {
	d := &bwallet.Derivation{
		SecurityLevel:   int(bwallet.PaymentProtocol.SecurityLevel),
		Protocol:        bwallet.PaymentProtocol.Protocol,
		KeyID:           bwallet.PaymentKeyID(prefixB64, suffixB64),
		CounterpartyHex: strings.TrimSpace(senderPub),
		OwnerHex:        c.port.IdentityPubHex(),
	}
	return c.port.Signer().DerivedScript(ctx, d)
}

func (c *PortFeeCollector) VerifyPaymentBEEF(ctx context.Context, rawBeef []byte, opts chain.PaymentVerifyOpts) (*chain.PaymentVerifyResult, error) {
	return chain.VerifyCounterpartyPaymentBEEF(ctx, rawBeef, c.port.Tracker(), opts)
}

func (c *PortFeeCollector) InternalizeBRC29(ctx context.Context, rawBeef []byte, outputIndex uint32, prefixB64, suffixB64, senderPub, description string, labels []string) (uint64, error) {
	return c.port.InternalizeBRC29(ctx, rawBeef, outputIndex, prefixB64, suffixB64, senderPub, description, labels)
}

// BRC105PaymentHeader is the canonical X-BSV-Payment JSON payload accepted across all 4 Hub fee valves.
// Wire-identical to server.BRC105PaymentHeader so existing clients and piggybacked fundTx outputs work seamlessly.
type BRC105PaymentHeader struct {
	TxID              string   `json:"txid"`
	AmountSats        uint64   `json:"amountSats,omitempty"`
	Inputs            []string `json:"inputs,omitempty"`
	BEEFHex           string   `json:"beefHex"`
	OutputIndex       uint32   `json:"outputIndex"`
	DerivationPrefix  string   `json:"derivationPrefix"`
	DerivationSuffix  string   `json:"derivationSuffix"`
	SenderIdentityKey string   `json:"senderIdentityKey"`
}

// TermsClass matches Lightweb's bcommon/termsroute terms document class format.
type TermsClass struct {
	Class    string `json:"class"`
	Satoshis uint64 `json:"satoshis"`
}

// EgressTerms advertises Spine-and-Leaf metered egress rates and top-up endpoint.
type EgressTerms struct {
	SatsPer100Events uint64 `json:"satsPer100Events"`
	MinTopupSats     uint64 `json:"minTopupSats"`
	TopupPath        string `json:"topupPath"`
	BalancePath      string `json:"balancePath"`
}

// BountyTaperTerms transparently publishes the Hub's temporary launch-subsidy taper schedule
// so sellers build on honest, predictable economics.
type BountyTaperTerms struct {
	Phase              string `json:"phase"`
	InitialRewardSats  uint64 `json:"initialRewardSats"`
	StepDownAtFills    int    `json:"stepDownAtFills"`
	StepDownRewardSats uint64 `json:"stepDownRewardSats"`
	MaxFillsPerDomain  int    `json:"maxFillsPerDomain"`
	ExitCriterion      string `json:"exitCriterion"`
}

// TermsDocument is published at GET /v1/terms and GET /ls_nektar/terms.
type TermsDocument struct {
	Service             string            `json:"service"`
	Terms               int               `json:"terms"`
	Network             string            `json:"network"`
	HubRole             HubRole           `json:"hubRole"`
	PayeeIdentityKey    string            `json:"payeeIdentityKey,omitempty"`
	Fees                map[string]uint64 `json:"fees"`
	Egress              EgressTerms       `json:"egress"`
	Classes             []TermsClass      `json:"classes"`
	AcceptThresholdSats uint64            `json:"acceptThresholdSats"`
	Prefixes            map[string]string `json:"prefixes,omitempty"`
	PrefixExpiresAt     int64             `json:"prefixExpiresAt,omitempty"`
	BountyTaper         *BountyTaperTerms `json:"bountyTaper,omitempty"`
}

// IssuePrefix creates a stateless, standard-base64-encoded HMAC-SHA256 derivation prefix
// bound to (action, minSats, expiry, hubSecret). Safe for BRC-29 base64 validation and zero server RAM.
func (n *Node) IssuePrefix(action string, minSats uint64, now time.Time) string {
	exp := now.Add(defaultPrefixTTL).Unix()
	var nonce [8]byte
	_, _ = crand.Read(nonce[:])
	payload := fmt.Sprintf("v1|%d|%s|%d|%s", exp, action, minSats, hex.EncodeToString(nonce[:]))
	mac := hmac.New(sha256.New, n.prefixSecret)
	_, _ = mac.Write([]byte(payload))
	sigHex := hex.EncodeToString(mac.Sum(nil)[:16])
	token := payload + "|" + sigHex
	return base64.StdEncoding.EncodeToString([]byte(token))
}

// VerifyPrefix validates a server-issued base64 HMAC derivation prefix against action, minSats, and expiry.
func (n *Node) VerifyPrefix(prefixB64, wantAction string, wantMinSats uint64, now time.Time) error {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(prefixB64))
	if err != nil {
		return errors.New("invalid base64 derivationPrefix")
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 6 || parts[0] != "v1" {
		return errors.New("malformed server derivationPrefix")
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || exp < now.Unix() {
		return errors.New("expired derivationPrefix")
	}
	action := parts[2]
	if action != wantAction {
		return fmt.Errorf("derivationPrefix action %q != %q", action, wantAction)
	}
	minSats, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil || minSats < wantMinSats {
		return fmt.Errorf("derivationPrefix minSats %d < required %d", minSats, wantMinSats)
	}
	payload := strings.Join(parts[:5], "|")
	mac := hmac.New(sha256.New, n.prefixSecret)
	_, _ = mac.Write([]byte(payload))
	wantSig := hex.EncodeToString(mac.Sum(nil)[:16])
	if !hmac.Equal([]byte(parts[5]), []byte(wantSig)) {
		return errors.New("invalid derivationPrefix signature")
	}
	return nil
}

// Terms builds the Hub's current TermsDocument with fresh stateless prefixes for 1-RTT & fundTx piggyback payments.
func (n *Node) Terms() TermsDocument {
	now := time.Now()
	payeePub := ""
	if n.feeCollector != nil {
		payeePub = n.feeCollector.IdentityPubHex()
	}
	fees := map[string]uint64{
		ActionSubmitWant:     n.fees.SubmitWantSats,
		ActionSubmitOffer:    n.fees.SubmitOfferSats,
		ActionSubmitAnnounce: n.fees.SubmitAnnounceSats,
		ActionBboxSend:       n.fees.BboxSendSats,
	}
	classes := make([]TermsClass, 0, len(n.fees.LookupPrices))
	for cls, price := range n.fees.LookupPrices {
		if price > 0 {
			fees[ActionLookupPrefix+cls] = price
			classes = append(classes, TermsClass{Class: cls, Satoshis: price})
		}
	}
	prefixes := make(map[string]string)
	if payeePub != "" && n.fees.HasAnyFees() {
		for act, price := range fees {
			if price > 0 {
				prefixes[act] = n.IssuePrefix(act, price, now)
			}
		}
		if n.fees.EgressSatsPer100Events > 0 {
			prefixes[ActionEgressTopup] = n.IssuePrefix(ActionEgressTopup, n.fees.EgressMinTopupSats, now)
		}
	}
	var exp int64
	if len(prefixes) > 0 {
		exp = now.Add(defaultPrefixTTL).Unix()
	}
	return TermsDocument{
		Service:          bstack.LookupService,
		Terms:            1,
		Network:          n.net,
		HubRole:          n.fees.Role,
		PayeeIdentityKey: payeePub,
		Fees:             fees,
		Egress: EgressTerms{
			SatsPer100Events: n.fees.EgressSatsPer100Events,
			MinTopupSats:     n.fees.EgressMinTopupSats,
			TopupPath:        "/v1/credits/topup",
			BalancePath:      "/v1/credits/balance",
		},
		Classes:             classes,
		AcceptThresholdSats: n.fees.AcceptThresholdSats,
		Prefixes:            prefixes,
		PrefixExpiresAt:     exp,
		BountyTaper: &BountyTaperTerms{
			Phase:              "launch-seeding",
			InitialRewardSats:  100,
			StepDownAtFills:    25,
			StepDownRewardSats: 50,
			MaxFillsPerDomain:  50,
			ExitCriterion:      "hubSupplyShare < 20% and organicSellers >= 3 per domain",
		},
	}
}

// FeeSchedule returns a copy of the Hub's active FeeSchedule.
func (n *Node) FeeSchedule() FeeSchedule { return n.fees }

// PayeeIdentityKey returns the Hub's BRC-29 payee identity key hex (empty if unconfigured).
func (n *Node) PayeeIdentityKey() string {
	if n.feeCollector == nil {
		return ""
	}
	return n.feeCollector.IdentityPubHex()
}

// IsSpinePeer reports whether pubHex belongs to an explicitly configured bilateral Spine peer.
func (n *Node) IsSpinePeer(pubHex string) bool {
	pubHex = strings.ToLower(strings.TrimSpace(pubHex))
	if pubHex == "" {
		return false
	}
	n.mu.RLock()
	ok := n.spinePeers[pubHex]
	n.mu.RUnlock()
	return ok
}

// OnFeePaymentMined promotes a collected fee payment to 'mined' once confirmed at settleDepth.
func (n *Node) OnFeePaymentMined(txid string, height uint32) error {
	return n.store.MarkFeeMined(txid, height)
}

// OnFeePaymentLost records an unsettleable fast fee payment and permanently flags its payer
// (forcing all future payments from that pubkey onto confirmation hold).
func (n *Node) OnFeePaymentLost(txid, payerPub, reason string) error {
	_ = n.store.MarkFeeLost(txid)
	return n.store.FlagPayer(payerPub, txid, reason)
}
