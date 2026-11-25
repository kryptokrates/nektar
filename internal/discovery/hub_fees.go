package discovery

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/carrier"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

// HubFeeApprover checks whether paying sats for a Hub action is allowed by operator policy (fail-closed).
type HubFeeApprover func(action string, sats uint64) error

// HubFeeRecorder accounts a completed Hub fee payment against the operator's daily hub fee budget.
type HubFeeRecorder func(sats uint64)

// HubTerms mirrors hub.TermsDocument without importing internal/hub.
type HubTerms struct {
	Service          string            `json:"service"`
	Terms            int               `json:"terms"`
	Network          string            `json:"network"`
	HubRole          string            `json:"hubRole"`
	PayeeIdentityKey string            `json:"payeeIdentityKey,omitempty"`
	Fees             map[string]uint64 `json:"fees"`
	Egress           struct {
		SatsPer100Events uint64 `json:"satsPer100Events"`
		MinTopupSats     uint64 `json:"minTopupSats"`
		TopupPath        string `json:"topupPath"`
		BalancePath      string `json:"balancePath"`
	} `json:"egress"`
	AcceptThresholdSats uint64            `json:"acceptThresholdSats"`
	Prefixes            map[string]string `json:"prefixes,omitempty"`
	PrefixExpiresAt     int64             `json:"prefixExpiresAt,omitempty"`
}

// HubPaymentHeader is the canonical BRC-105 X-BSV-Payment JSON header.
type HubPaymentHeader struct {
	TxID              string `json:"txid"`
	AmountSats        uint64 `json:"amountSats,omitempty"`
	BEEFHex           string `json:"beefHex"`
	OutputIndex       uint32 `json:"outputIndex"`
	DerivationPrefix  string `json:"derivationPrefix"`
	DerivationSuffix  string `json:"derivationSuffix"`
	SenderIdentityKey string `json:"senderIdentityKey"`
}

// HubPiggybackTarget specifies one paid Hub output to embed directly into a carrier's mined fundTx
// so broadcasting to k paid hubs costs only ONE BSV transaction / miner fee total.
type HubPiggybackTarget struct {
	HubURL           string
	PayeeIdentityKey string
	DerivationPrefix string
	Action           string
	AmountSats       uint64
}

type cachedHubTerms struct {
	terms     HubTerms
	fetchedAt time.Time
}

// HubTermsCache caches GET /v1/terms responses per hub URL with TTL + prefix expiry awareness.
type HubTermsCache struct {
	mu    sync.RWMutex
	items map[string]cachedHubTerms
}

func NewHubTermsCache() *HubTermsCache {
	return &HubTermsCache{items: make(map[string]cachedHubTerms)}
}

func (c *HubTermsCache) Get(hubURL string) (HubTerms, bool) {
	if c == nil {
		return HubTerms{}, false
	}
	u := normalizeHostURL(hubURL)
	c.mu.RLock()
	hit, ok := c.items[u]
	c.mu.RUnlock()
	if !ok {
		return HubTerms{}, false
	}
	if time.Since(hit.fetchedAt) > 4*time.Minute {
		return HubTerms{}, false
	}
	if hit.terms.PrefixExpiresAt > 0 && time.Now().Unix() >= hit.terms.PrefixExpiresAt-30 {
		return HubTerms{}, false
	}
	return hit.terms, true
}

func (c *HubTermsCache) Put(hubURL string, t HubTerms) {
	if c == nil {
		return
	}
	u := normalizeHostURL(hubURL)
	c.mu.Lock()
	c.items[u] = cachedHubTerms{terms: t, fetchedAt: time.Now()}
	c.mu.Unlock()
}

func (c *HubTermsCache) Invalidate(hubURL string) {
	if c == nil {
		return
	}
	u := normalizeHostURL(hubURL)
	c.mu.Lock()
	delete(c.items, u)
	c.mu.Unlock()
}

// FetchHubTerms retrieves (and caches) a Hub's GET /v1/terms document.
func (s *Service) FetchHubTerms(ctx context.Context, hubURL string, forceRefresh bool) (HubTerms, error) {
	u := normalizeHostURL(hubURL)
	if u == "" {
		return HubTerms{}, errors.New("discovery: invalid hubURL")
	}
	if !forceRefresh {
		if hit, ok := s.termsCache.Get(u); ok {
			return hit, nil
		}
	}
	var terms HubTerms
	code, err := s.getJSON(ctx, u+"/v1/terms", &terms)
	if err != nil {
		return HubTerms{}, err
	}
	if code != http.StatusOK {
		return HubTerms{}, fmt.Errorf("discovery: hub terms HTTP %d", code)
	}
	s.termsCache.Put(u, terms)
	return terms, nil
}

// BuildFundedMarketCarrierWithHubFees mints ONE single mined fundTx paying Output 0 (1-sat carrier
// funding lock) PLUS Outputs 1..k (BRC-29 P2PKH tolls for each paid Hub), broadcasts fundTx ONCE,
// mints the unmineable PushDrop carrier spending fundTx:0, and returns per-hub X-BSV-Payment headers.
func BuildFundedMarketCarrierWithHubFees(
	ctx context.Context,
	port *chain.Port,
	priv *ec.PrivateKey,
	p bstack.MarketPayload,
	targets []HubPiggybackTarget,
) (carrierBEEF []byte, perHubHeaders map[string]string, totalFeeSats uint64, err error) {
	if port == nil {
		return nil, nil, 0, errors.New("discovery: chain port required for funded market carrier")
	}
	if priv == nil {
		return nil, nil, 0, errors.New("discovery: signing key required for funded market carrier")
	}
	cborBytes, err := bstack.EncodePayload(p)
	if err != nil {
		return nil, nil, 0, err
	}
	protoWallet, err := sdkwallet.NewCompletedProtoWallet(priv)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("discovery: init carrier signer wallet: %w", err)
	}
	fundLock, err := carrier.FundingLock(ctx, protoWallet, bstack.Originator, bstack.MarketCarrierParams)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("discovery: derive carrier funding lock: %w", err)
	}

	outputs := []*transaction.TransactionOutput{{Satoshis: 1, LockingScript: fundLock}}
	type builtTarget struct {
		target   HubPiggybackTarget
		suffix   string
		lock     []byte
		outIndex uint32
	}
	var paidTargets []builtTarget
	for _, t := range targets {
		if t.AmountSats == 0 || strings.TrimSpace(t.PayeeIdentityKey) == "" || strings.TrimSpace(t.DerivationPrefix) == "" {
			continue
		}
		sufB64, rErr := randomBase64Nonce16()
		if rErr != nil {
			return nil, nil, 0, rErr
		}
		lockScript, dErr := port.Signer().PaymentDestination(ctx, strings.TrimSpace(t.PayeeIdentityKey), strings.TrimSpace(t.DerivationPrefix), sufB64)
		if dErr != nil || lockScript == nil {
			return nil, nil, 0, fmt.Errorf("discovery: derive hub BRC-29 output for %s: %w", t.HubURL, dErr)
		}
		outputs = append(outputs, &transaction.TransactionOutput{
			Satoshis:      t.AmountSats,
			LockingScript: lockScript,
		})
		paidTargets = append(paidTargets, builtTarget{
			target: t,
			suffix: sufB64,
			lock:   append([]byte(nil), (*lockScript)...),
		})
		totalFeeSats += t.AmountSats
	}

	fundTx, fundBEEF, feeSats, spendSats, err := port.BuildFundedTxWithBEEF(ctx, outputs)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("discovery: build carrier funding tx: %w", err)
	}
	if _, err := port.Broadcast(ctx, fundTx, feeSats, spendSats); err != nil {
		return nil, nil, 0, fmt.Errorf("discovery: broadcast carrier funding tx: %w", err)
	}
	if len(fundBEEF) == 0 {
		fundBEEF, _ = fundTx.AtomicBEEF(false)
	}
	var fundVout uint32
	foundFund := false
	for i, o := range fundTx.Outputs {
		if o != nil && o.LockingScript != nil && bytes.Equal(*o.LockingScript, *fundLock) {
			fundVout = uint32(i)
			foundFund = true
			break
		}
	}
	if !foundFund {
		return nil, nil, 0, errors.New("discovery: carrier funding output not found in built transaction")
	}

	carrierTx, err := carrier.Mint(ctx, protoWallet, bstack.Originator, bstack.MarketCarrierParams, cborBytes, fundTx, fundVout)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("discovery: mint PushDrop carrier: %w", err)
	}
	if len(carrierTx.Inputs) > 0 && carrierTx.Inputs[0].SourceTransaction == nil {
		carrierTx.Inputs[0].SourceTransaction = fundTx
	}
	carrierBEEF, err = carrierTx.AtomicBEEF(false)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("discovery: encode carrier AtomicBEEF: %w", err)
	}

	if len(paidTargets) > 0 {
		perHubHeaders = make(map[string]string, len(paidTargets))
		fundTxID := fundTx.TxID().String()
		fundBEEFHex := hex.EncodeToString(fundBEEF)
		senderHex := port.IdentityPubHex()
		for _, pt := range paidTargets {
			matched := false
			for i, o := range fundTx.Outputs {
				if o != nil && o.LockingScript != nil && o.Satoshis == pt.target.AmountSats && bytes.Equal(*o.LockingScript, pt.lock) {
					pt.outIndex = uint32(i)
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			hdrBytes, _ := json.Marshal(HubPaymentHeader{
				TxID:              fundTxID,
				AmountSats:        pt.target.AmountSats,
				BEEFHex:           fundBEEFHex,
				OutputIndex:       pt.outIndex,
				DerivationPrefix:  pt.target.DerivationPrefix,
				DerivationSuffix:  pt.suffix,
				SenderIdentityKey: senderHex,
			})
			perHubHeaders[normalizeHostURL(pt.target.HubURL)] = string(hdrBytes)
		}
	}
	return carrierBEEF, perHubHeaders, totalFeeSats, nil
}

// BuildStandaloneHubPayment builds and broadcasts a single BRC-29 payment honoring a Hub's server-issued prefix.
func BuildStandaloneHubPayment(ctx context.Context, port *chain.Port, payeePub, prefixB64, action string, amountSats uint64) (string, string, error) {
	if port == nil {
		return "", "", errors.New("discovery: chain port required for hub fee payment")
	}
	if payeePub == "" || prefixB64 == "" || amountSats == 0 {
		return "", "", errors.New("discovery: payeePub, derivationPrefix, and amountSats > 0 required")
	}
	res, err := port.Send(ctx, nwallet.SendArgs{
		ToIdentityKey:    strings.TrimSpace(payeePub),
		DerivationPrefix: strings.TrimSpace(prefixB64),
		Satoshis:         amountSats,
		Description:      "nektar hub fee " + action,
		Labels:           []string{"nektar", "hub-fee", action},
	})
	if err != nil {
		return "", "", err
	}
	beefHex := res.BEEFHex
	if beefHex == "" && len(res.BEEF) > 0 {
		beefHex = hex.EncodeToString(res.BEEF)
	}
	hdrBytes, err := json.Marshal(HubPaymentHeader{
		TxID:              res.TxID,
		AmountSats:        amountSats,
		BEEFHex:           beefHex,
		OutputIndex:       res.OutputIndex,
		DerivationPrefix:  res.DerivationPrefix,
		DerivationSuffix:  res.DerivationSuffix,
		SenderIdentityKey: res.SenderIdentityKey,
	})
	if err != nil {
		return "", "", err
	}
	return string(hdrBytes), res.TxID, nil
}

// PayHub402Challenge inspects an HTTP 402 Payment Required response, enforces operator fee policy
// fail-closed via approver, builds a BRC-29 payment, records spend, and returns the X-BSV-Payment header JSON.
func PayHub402Challenge(
	ctx context.Context,
	port *chain.Port,
	approver HubFeeApprover,
	recorder HubFeeRecorder,
	fallbackAction string,
	resp *http.Response,
) (headerJSON string, paidSats uint64, err error) {
	if resp == nil {
		return "", 0, errors.New("discovery: nil 402 response")
	}
	var body struct {
		Action           string `json:"action"`
		AmountSats       uint64 `json:"amountSats"`
		IdentityKey      string `json:"identityKey"`
		DerivationPrefix string `json:"derivationPrefix"`
	}
	rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = json.Unmarshal(rawBody, &body)

	action := strings.TrimSpace(body.Action)
	if action == "" {
		action = fallbackAction
	}
	amountSats := body.AmountSats
	if h := strings.TrimSpace(resp.Header.Get("X-BSV-Payment-Satoshis-Required")); h != "" {
		if v, pErr := strconv.ParseUint(h, 10, 64); pErr == nil && v > 0 {
			amountSats = v
		}
	} else if h := strings.TrimSpace(resp.Header.Get("X-BSV-Payment-Amount")); h != "" {
		if v, pErr := strconv.ParseUint(h, 10, 64); pErr == nil && v > 0 {
			amountSats = v
		}
	}
	payeePub := strings.TrimSpace(resp.Header.Get("X-BSV-Payment-Identity"))
	if payeePub == "" {
		payeePub = strings.TrimSpace(body.IdentityKey)
	}
	prefix := strings.TrimSpace(resp.Header.Get("X-BSV-Payment-Derivation-Prefix"))
	if prefix == "" {
		prefix = strings.TrimSpace(body.DerivationPrefix)
	}
	if amountSats == 0 || payeePub == "" || prefix == "" {
		return "", 0, fmt.Errorf("discovery: incomplete 402 BRC-105 challenge (sats=%d payee=%v prefix=%v)", amountSats, payeePub != "", prefix != "")
	}
	if approver == nil {
		return "", 0, errors.New("discovery: hub fee approver policy required to auto-pay 402")
	}
	if err := approver(action, amountSats); err != nil {
		return "", 0, err
	}
	hdr, _, err := BuildStandaloneHubPayment(ctx, port, payeePub, prefix, action, amountSats)
	if err != nil {
		return "", 0, err
	}
	if recorder != nil {
		recorder(amountSats)
	}
	return hdr, amountSats, nil
}

// TopUpHubEgressCredits tops up a caller's prepaid BRC-105 egress meter on hubURL (bounded by operator policy).
// creditPub is the X-Nektar-Pub that will authenticate GET /events and GET /sync (defaults to wallet identity key).
func (s *Service) TopUpHubEgressCredits(ctx context.Context, hubURL string, minTopupSats uint64, creditPub string) (uint64, error) {
	if s.mem == nil || s.mem.Chain() == nil {
		return 0, errors.New("discovery: chain port required to top up hub egress credits")
	}
	u := normalizeHostURL(hubURL)
	if u == "" {
		return 0, errors.New("discovery: invalid hubURL")
	}
	s.mu.RLock()
	approver := s.feeApprover
	recorder := s.feeRecorder
	s.mu.RUnlock()
	if approver == nil {
		return 0, errors.New("discovery: hub fee policy approver not configured")
	}
	terms, err := s.FetchHubTerms(ctx, u, true)
	if err != nil {
		return 0, err
	}
	amount := minTopupSats
	if terms.Egress.MinTopupSats > amount {
		amount = terms.Egress.MinTopupSats
	}
	if amount == 0 {
		amount = 20
	}
	if err := approver("egress:topup", amount); err != nil {
		return 0, err
	}
	prefix := terms.Prefixes["egress:topup"]
	if prefix == "" || terms.PayeeIdentityKey == "" {
		return 0, errors.New("discovery: hub terms missing payeeIdentityKey or egress:topup prefix")
	}
	payHdr, _, err := BuildStandaloneHubPayment(ctx, s.mem.Chain(), terms.PayeeIdentityKey, prefix, "egress:topup", amount)
	if err != nil {
		return 0, err
	}
	topupPath := terms.Egress.TopupPath
	if topupPath == "" {
		topupPath = "/v1/credits/topup"
	}
	bodyBytes, _ := json.Marshal(map[string]string{"payerPub": strings.ToLower(strings.TrimSpace(creditPub))})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u+topupPath, bytes.NewReader(bodyBytes))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BSV-Payment", payHdr)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 0, fmt.Errorf("discovery: hub egress topup HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if recorder != nil {
		recorder(amount)
	}
	return amount, nil
}

// PostJSONWithHub402 sends a JSON POST request and automatically handles a single BRC-105 HTTP 402
// payment retry strictly within the configured HubFeeApprover policy caps.
func (s *Service) PostJSONWithHub402(ctx context.Context, targetURL, action string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusPaymentRequired {
		return resp, nil
	}
	defer resp.Body.Close()
	if s.mem == nil || s.mem.Chain() == nil {
		return nil, fmt.Errorf("discovery: %s requires payment (HTTP 402) but no wallet is attached", action)
	}
	s.mu.RLock()
	approver := s.feeApprover
	recorder := s.feeRecorder
	s.mu.RUnlock()
	payHdr, _, pErr := PayHub402Challenge(ctx, s.mem.Chain(), approver, recorder, action, resp)
	if pErr != nil {
		return nil, pErr
	}
	retryReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	retryReq.Header.Set("Content-Type", "application/json")
	retryReq.Header.Set("X-BSV-Payment", payHdr)
	return s.httpClient.Do(retryReq)
}

// SignEgressAuthHeaders produces signed X-Nektar-Pub / X-Nektar-Ts / X-Nektar-Nonce / X-Nektar-Sig
// headers matching hub.Node.authenticateEgressCaller for metered GET /events and GET /sync streams.
func SignEgressAuthHeaders(priv *ec.PrivateKey, action, hubURL string, cursor int64, domainsCSV string) (pub, tsStr, nonce, sig string, err error) {
	if priv == nil {
		return "", "", "", "", errors.New("discovery: signing key required for hub egress auth")
	}
	pub = hex.EncodeToString(priv.PubKey().Compressed())
	ts := time.Now().Unix()
	tsStr = strconv.FormatInt(ts, 10)
	var nRaw [12]byte
	if _, err := crand.Read(nRaw[:]); err != nil {
		return "", "", "", "", err
	}
	nonce = hex.EncodeToString(nRaw[:])
	aud := strings.ToLower(strings.TrimSpace(hubURL))
	if u, uErr := url.Parse(strings.TrimSpace(hubURL)); uErr == nil && u.Host != "" {
		aud = strings.ToLower(u.Host)
	}
	payload := map[string]any{
		"action":  action,
		"pub":     pub,
		"ts":      ts,
		"nonce":   nonce,
		"aud":     aud,
		"cursor":  cursor,
		"domains": strings.ToLower(strings.TrimSpace(domainsCSV)),
	}
	sig, err = bstack.SignArtifact(priv, payload)
	if err != nil {
		return "", "", "", "", err
	}
	return pub, tsStr, nonce, sig, nil
}

func randomBase64Nonce16() (string, error) {
	var raw [16]byte
	if _, err := crand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw[:]), nil
}
