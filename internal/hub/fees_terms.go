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
	if strings.Contains(action, "|") {
		return ""
	}
	exp := now.Add(defaultPrefixTTL).Unix()
	var nonce [8]byte
	if _, err := crand.Read(nonce[:]); err != nil {
		panic("hub: crypto/rand failed: " + err.Error())
	}
	payload := fmt.Sprintf("v1|%d|%s|%d|%s", exp, action, minSats, hex.EncodeToString(nonce[:]))
	mac := hmac.New(sha256.New, n.prefixSecret)
	_, _ = mac.Write([]byte(payload))
	sigHex := hex.EncodeToString(mac.Sum(nil)[:16])
	token := payload + "|" + sigHex
	return base64.StdEncoding.EncodeToString([]byte(token))
}

// VerifyPrefix validates a server-issued base64 HMAC derivation prefix against action, minSats, and expiry.
func (n *Node) VerifyPrefix(prefixB64, wantAction string, wantMinSats uint64, now time.Time) error {
	if strings.Contains(wantAction, "|") {
		return errors.New("invalid derivationPrefix action")
	}
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
