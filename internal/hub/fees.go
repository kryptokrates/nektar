package hub

import (
	"bytes"
	"context"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/guard"

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

	PresetFree          = "free"
	PresetLightBSVEgress = "lightbsv_egress"
	PresetHybrid        = "hybrid"

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

type feeCollectOutcome struct {
	HTTPStatus int
	Code       string
	Error      string
	TxID       string
	PayerPub   string
	PaidSats   uint64
	Decision   string
}

// verifyAndCollectFee enforces BRC-105 payment verification, Lightweb acceptance limits,
// replay/conflict protection, and wallet internalization for any priced Hub action.
func (n *Node) verifyAndCollectFee(ctx context.Context, action, kind string, priceSats uint64, payHeader string) feeCollectOutcome {
	if priceSats == 0 {
		return feeCollectOutcome{HTTPStatus: http.StatusOK}
	}
	if n.feeCollector == nil {
		return feeCollectOutcome{
			HTTPStatus: http.StatusServiceUnavailable,
			Code:       "ERR_HUB_FEE_UNCONFIGURED",
			Error:      "hub priced action requires a configured FeeCollector wallet",
		}
	}
	payHeader = strings.TrimSpace(payHeader)
	if payHeader == "" {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_REQUIRED",
			Error:      fmt.Sprintf("payment of %d sats required for %s", priceSats, action),
		}
	}
	var pay BRC105PaymentHeader
	if err := json.Unmarshal([]byte(payHeader), &pay); err != nil {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_MALFORMED",
			Error:      "invalid X-BSV-Payment JSON header",
		}
	}
	if pay.BEEFHex == "" || pay.DerivationPrefix == "" || pay.DerivationSuffix == "" || pay.SenderIdentityKey == "" {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_INCOMPLETE",
			Error:      "X-BSV-Payment requires beefHex, derivationPrefix, derivationSuffix, and senderIdentityKey",
		}
	}
	if err := n.VerifyPrefix(pay.DerivationPrefix, action, priceSats, time.Now()); err != nil {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_PREFIX",
			Error:      err.Error(),
		}
	}
	rawBeef, err := hex.DecodeString(strings.TrimSpace(pay.BEEFHex))
	if err != nil || len(rawBeef) == 0 {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_MALFORMED",
			Error:      "invalid X-BSV-Payment beefHex",
		}
	}
	_, tx, txHash, err := guard.ParseBEEF(rawBeef, guard.DefaultBound)
	if err != nil || tx == nil || txHash == nil {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_MALFORMED",
			Error:      "unparseable X-BSV-Payment AtomicBEEF",
		}
	}
	txid := txHash.String()
	if pay.TxID != "" && !strings.EqualFold(strings.TrimSpace(pay.TxID), txid) {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_MALFORMED",
			Error:      "X-BSV-Payment txid does not match AtomicBEEF",
		}
	}
	if uint64(pay.OutputIndex) >= uint64(len(tx.Outputs)) {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_WRONG_SCRIPT",
			Error:      "X-BSV-Payment outputIndex out of range",
		}
	}

	// Derive expected BRC-29 locking script and assert output pays >= priceSats.
	wantScript, dErr := n.feeCollector.DeriveBRC29Script(ctx, pay.SenderIdentityKey, pay.DerivationPrefix, pay.DerivationSuffix)
	if dErr != nil || wantScript == nil {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_WRONG_SCRIPT",
			Error:      fmt.Sprintf("BRC-29 script derivation failed: %v", dErr),
		}
	}
	out := tx.Outputs[pay.OutputIndex]
	if out == nil || out.LockingScript == nil || !bytes.Equal(*out.LockingScript, *wantScript) {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_WRONG_SCRIPT",
			Error:      "payment output does not match derived BRC-29 locking script",
		}
	}
	paidSats := out.Satoshis
	if paidSats < priceSats {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_INSUFFICIENT",
			Error:      fmt.Sprintf("insufficient payment (%d < %d sats)", paidSats, priceSats),
		}
	}

	inKeys := make([]string, 0, len(tx.Inputs))
	for _, in := range tx.Inputs {
		if in != nil && in.SourceTXID != nil {
			inKeys = append(inKeys, fmt.Sprintf("%s:%d", in.SourceTXID.String(), in.SourceTxOutIndex))
		}
	}
	payerPub := strings.ToLower(strings.TrimSpace(pay.SenderIdentityKey))

	// Check Lightweb acceptance policy: flagged payer OR over fast threshold/payer/total exposure -> require mined.
	flagged, flagReason, _ := n.store.IsPayerFlagged(payerPub)
	payerExp, totalExp, _ := n.store.UnminedFastExposure(payerPub, int64(n.fees.AcceptWindow.Seconds()))
	holdReason := ""
	if flagged {
		holdReason = "payer flagged for prior unsettleable payment: " + flagReason
	} else if paidSats > n.fees.AcceptThresholdSats {
		holdReason = fmt.Sprintf("payment %d sats exceeds fast accept threshold %d sats", paidSats, n.fees.AcceptThresholdSats)
	} else if payerExp+paidSats > n.fees.AcceptPayerLimitSats {
		holdReason = fmt.Sprintf("payer unmined exposure %d+%d exceeds limit %d sats", payerExp, paidSats, n.fees.AcceptPayerLimitSats)
	} else if totalExp+paidSats > n.fees.AcceptTotalLimitSats {
		holdReason = fmt.Sprintf("hub total unmined exposure %d+%d exceeds limit %d sats", totalExp, paidSats, n.fees.AcceptTotalLimitSats)
	}

	// Run full local consensus SPV verification BEFORE replay claim or wallet internalization.
	settled := false
	var blockHeight uint32
	if n.feeCollector.Network() != "memory" {
		allowZeroConf := holdReason == ""
		ver, vErr := n.feeCollector.VerifyPaymentBEEF(ctx, rawBeef, chain.PaymentVerifyOpts{
			ExpectedAmountSats:       priceSats,
			ZeroConfMaxSats:          n.fees.AcceptThresholdSats,
			ZeroConfMaxAggregateSats: n.fees.AcceptTotalLimitSats,
			SettleDepth:              n.feeCollector.SettleDepth(),
			AllowZeroConf:            allowZeroConf,
			ConflictWatch:            n.feeCollector.ZeroConfWatch(),
			TrackOnZeroConf:          allowZeroConf,
		})
		if vErr != nil {
			if errors.Is(vErr, chain.ErrTrackerUnavailable) {
				return feeCollectOutcome{
					HTTPStatus: http.StatusServiceUnavailable,
					Code:       "ERR_TRACKER_UNAVAILABLE",
					Error:      vErr.Error(),
				}
			}
			if errors.Is(vErr, chain.ErrConflictDetected) {
				_ = n.store.FlagPayer(payerPub, txid, vErr.Error())
				return feeCollectOutcome{
					HTTPStatus: http.StatusConflict,
					Code:       "ERR_PAYMENT_CONFLICT",
					Error:      vErr.Error(),
					TxID:       txid,
				}
			}
			return feeCollectOutcome{
				HTTPStatus: http.StatusPaymentRequired,
				Code:       "ERR_PAYMENT_SPV_FAILED",
				Error:      vErr.Error(),
			}
		}
		if ver.NeedsConfirmation {
			// Record held claim so the payer can retry the exact same txid once mined without paying twice!
			if cErr := n.store.ClaimFeePaymentAtomic(txid, inKeys, action, payerPub, paidSats, "held"); cErr != nil {
				return mapClaimErr(cErr, txid)
			}
			_ = n.store.RecordFeeLedger(FeeLedgerEntry{
				TxID:     txid,
				Action:   action,
				Kind:     kind,
				PayerPub: payerPub,
				Sats:     paidSats,
				Decision: "hold",
				Status:   "held",
			})
			reason := holdReason
			if reason == "" {
				reason = ver.EscalationReason
			}
			return feeCollectOutcome{
				HTTPStatus: http.StatusAccepted,
				Code:       "ERR_PAYMENT_HELD",
				Error:      reason,
				TxID:       txid,
				PayerPub:   payerPub,
				PaidSats:   paidSats,
				Decision:   "hold",
			}
		}
		settled = ver.SettledAtDepth
		blockHeight = ver.BlockHeight
	} else {
		// In memory mode, still honor flagged-payer & explicit hold threshold when unmined so tests can verify 202 hold!
		if tx.MerklePath != nil {
			settled = true
			blockHeight = tx.MerklePath.BlockHeight
		} else if holdReason != "" {
			if cErr := n.store.ClaimFeePaymentAtomic(txid, inKeys, action, payerPub, paidSats, "held"); cErr != nil {
				return mapClaimErr(cErr, txid)
			}
			_ = n.store.RecordFeeLedger(FeeLedgerEntry{
				TxID:     txid,
				Action:   action,
				Kind:     kind,
				PayerPub: payerPub,
				Sats:     paidSats,
				Decision: "hold",
				Status:   "held",
			})
			return feeCollectOutcome{
				HTTPStatus: http.StatusAccepted,
				Code:       "ERR_PAYMENT_HELD",
				Error:      holdReason,
				TxID:       txid,
				PayerPub:   payerPub,
				PaidSats:   paidSats,
				Decision:   "hold",
			}
		}
	}

	// Atomic anti-replay & anti-conflict claim BEFORE internalizing or admitting.
	if cErr := n.store.ClaimFeePaymentAtomic(txid, inKeys, action, payerPub, paidSats, "claimed"); cErr != nil {
		return mapClaimErr(cErr, txid)
	}

	// Internalize directly into the Hub's wallet (exempt from Hub outgoing SpendGuard!).
	if _, iErr := n.feeCollector.InternalizeBRC29(ctx, rawBeef, pay.OutputIndex, pay.DerivationPrefix, pay.DerivationSuffix, pay.SenderIdentityKey, "hub fee "+action, []string{"nektar", "hub-fee", action}); iErr != nil {
		_ = n.store.ReleaseFeeClaim(txid)
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_INTERNALIZE",
			Error:      fmt.Sprintf("internalize hub fee failed: %v", iErr),
		}
	}

	decision := "fast"
	status := "unmined"
	if settled {
		decision = "mined"
		status = "mined"
	}
	_ = n.store.RecordFeeLedger(FeeLedgerEntry{
		TxID:        txid,
		Action:      action,
		Kind:        kind,
		PayerPub:    payerPub,
		Sats:        paidSats,
		Decision:    decision,
		Status:      status,
		BlockHeight: blockHeight,
	})
	return feeCollectOutcome{
		HTTPStatus: http.StatusOK,
		TxID:       txid,
		PayerPub:   payerPub,
		PaidSats:   paidSats,
		Decision:   decision,
	}
}

func mapClaimErr(err error, txid string) feeCollectOutcome {
	if errors.Is(err, ErrFeePaymentConflict) {
		return feeCollectOutcome{
			HTTPStatus: http.StatusConflict,
			Code:       "ERR_PAYMENT_CONFLICT",
			Error:      err.Error(),
			TxID:       txid,
		}
	}
	return feeCollectOutcome{
		HTTPStatus: http.StatusConflict,
		Code:       "ERR_PAYMENT_REPLAYED",
		Error:      err.Error(),
		TxID:       txid,
	}
}

// writeFeeOutcome writes standard Lightweb/BRC-105 challenge headers & JSON body for non-200 fee outcomes.
func (n *Node) writeFeeOutcome(w http.ResponseWriter, action string, priceSats uint64, out feeCollectOutcome) {
	if out.HTTPStatus == http.StatusPaymentRequired {
		prefix := n.IssuePrefix(action, priceSats, time.Now())
		payeePub := n.PayeeIdentityKey()
		w.Header().Set("X-BSV-Payment-Version", "1.0")
		w.Header().Set("X-BSV-Payment-Protocol", "BRC-105")
		w.Header().Set("X-BSV-Payment-Satoshis-Required", strconv.FormatUint(priceSats, 10))
		w.Header().Set("X-BSV-Payment-Amount", strconv.FormatUint(priceSats, 10))
		w.Header().Set("X-BSV-Payment-Derivation-Prefix", prefix)
		w.Header().Set("X-BSV-Payment-Identity", payeePub)
		writeJSON(w, http.StatusPaymentRequired, map[string]any{
			"status":           "error",
			"code":             out.Code,
			"error":            out.Error,
			"action":           action,
			"amountSats":       priceSats,
			"identityKey":      payeePub,
			"derivationPrefix": prefix,
			"termsPath":        "/v1/terms",
			"topupPath":        "/v1/credits/topup",
		})
		return
	}
	body := map[string]any{
		"status": "error",
		"code":   out.Code,
		"error":  out.Error,
		"action": action,
	}
	if out.TxID != "" {
		body["txid"] = out.TxID
	}
	if out.HTTPStatus == http.StatusAccepted {
		body["held"] = true
	}
	writeJSON(w, out.HTTPStatus, body)
}

// authenticateEgressCaller verifies signed X-Nektar-Pub / X-Nektar-Ts / X-Nektar-Nonce / X-Nektar-Sig headers
// on metered GET /events and GET /sync requests (same replay-safe contract as GET /bbox/inbox).
func (n *Node) authenticateEgressCaller(r *http.Request, action string, cursor int64, domainsCSV string) (pub string, isSpinePeer bool, err error) {
	hdrPub := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Nektar-Pub")))
	hdrTs := strings.TrimSpace(r.Header.Get("X-Nektar-Ts"))
	hdrNonce := strings.TrimSpace(r.Header.Get("X-Nektar-Nonce"))
	hdrSig := strings.TrimSpace(r.Header.Get("X-Nektar-Sig"))
	if hdrPub == "" || hdrTs == "" || hdrNonce == "" || hdrSig == "" {
		return "", false, errors.New("signed X-Nektar-Pub / X-Nektar-Ts / X-Nektar-Nonce / X-Nektar-Sig headers required for metered hub egress")
	}
	ts, tsErr := strconv.ParseInt(hdrTs, 10, 64)
	now := time.Now().Unix()
	if tsErr != nil || ts < now-bboxSkewSeconds || ts > now+bboxSkewSeconds {
		return "", false, errors.New("expired or invalid X-Nektar-Ts")
	}
	aud := strings.ToLower(strings.TrimSpace(r.Host))
	authPayload := map[string]any{
		"action":  action,
		"pub":     hdrPub,
		"ts":      ts,
		"nonce":   hdrNonce,
		"aud":     aud,
		"cursor":  cursor,
		"domains": strings.ToLower(strings.TrimSpace(domainsCSV)),
	}
	if vErr := bstack.VerifyArtifact(hdrPub, hdrSig, authPayload); vErr != nil {
		return "", false, errors.New("invalid hub egress signature")
	}
	if !n.claimBboxNonce("egress:"+action+":"+hdrPub, hdrNonce, now) {
		return "", false, errors.New("replayed hub egress nonce")
	}
	return hdrPub, n.IsSpinePeer(hdrPub), nil
}

// domainsOnlyDir reports whether the caller explicitly requested ONLY the free `dir` discovery lane.
func domainsOnlyDir(doms []string) bool {
	if len(doms) == 0 {
		return false
	}
	for _, d := range doms {
		if !strings.EqualFold(strings.TrimSpace(d), bstack.DirDomain) {
			return false
		}
	}
	return true
}

// classifyLookupClass determines the pricing class for a BRC-24 ls_nektar lookup question.
// Standard live queries are "live" (free); historical cursor walks are "history"; large unscoped dumps are "export".
func classifyLookupClass(q *lookup.LookupQuestion) string {
	if q == nil || q.Service != bstack.LookupService || len(q.Query) == 0 {
		return "live"
	}
	var raw map[string]any
	if json.Unmarshal(q.Query, &raw) == nil {
		if cls, ok := raw["class"].(string); ok && strings.TrimSpace(cls) != "" {
			return strings.ToLower(strings.TrimSpace(cls))
		}
	}
	var mq bstack.MarketQuery
	if json.Unmarshal(q.Query, &mq) != nil {
		return "live"
	}
	if mq.Kind == bstack.KindAnnounce && strings.EqualFold(strings.TrimSpace(mq.Domain), bstack.DirDomain) {
		return "live"
	}
	if mq.SinceMs > 0 || mq.SinceSeq > 0 {
		return "history"
	}
	if mq.Limit > 100 && mq.WantID == "" && mq.OfferID == "" && mq.Domain == "" && mq.Pub == "" {
		return "export"
	}
	return "live"
}

func (n *Node) handleCreditsTopup(w http.ResponseWriter, r *http.Request) {
	minTopup := n.fees.EgressMinTopupSats
	if minTopup == 0 {
		minTopup = defaultMinTopupSats
	}
	// Optional JSON body may name a beneficiary pubkey (defaults to BRC-29 senderIdentityKey).
	var body struct {
		PayerPub string `json:"payerPub,omitempty"`
	}
	if r.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 16<<10))
		if len(bytes.TrimSpace(raw)) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
	}
	out := n.verifyAndCollectFee(r.Context(), ActionEgressTopup, "egress", minTopup, r.Header.Get("X-BSV-Payment"))
	if out.HTTPStatus != http.StatusOK {
		n.writeFeeOutcome(w, ActionEgressTopup, minTopup, out)
		return
	}
	creditPub := strings.ToLower(strings.TrimSpace(body.PayerPub))
	if creditPub == "" {
		creditPub = out.PayerPub
	}
	cred, err := n.store.CreditEgressMilliSats(creditPub, int64(out.PaidSats)*1000, out.PaidSats)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var eventsRemaining int64
	if perEv := n.fees.EgressMilliSatsPerEvent(); perEv > 0 {
		eventsRemaining = cred.BalanceMilliSats / perEv
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"txid":             out.TxID,
		"payerPub":         cred.PayerPub,
		"creditedSats":     out.PaidSats,
		"balanceMilliSats": cred.BalanceMilliSats,
		"eventsRemaining":  eventsRemaining,
		"decision":         out.Decision,
	})
}

func (n *Node) handleCreditsBalance(w http.ResponseWriter, r *http.Request) {
	pub := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("pub")))
	if pub == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "pub query parameter required"})
		return
	}
	cred, err := n.store.GetEgressCredit(pub)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var eventsRemaining int64
	if perEv := n.fees.EgressMilliSatsPerEvent(); perEv > 0 {
		eventsRemaining = cred.BalanceMilliSats / perEv
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"payerPub":             cred.PayerPub,
		"balanceMilliSats":     cred.BalanceMilliSats,
		"totalFundedSats":      cred.TotalFundedSats,
		"totalEventsDelivered": cred.TotalEventsDelivered,
		"eventsRemaining":      eventsRemaining,
		"satsPer100Events":     n.fees.EgressSatsPer100Events,
		"minTopupSats":         n.fees.EgressMinTopupSats,
	})
}
