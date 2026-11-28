package discovery

import (
	"bytes"
	"context"
	crand "crypto/rand"
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

	"github.com/kryptokrates/nektar/internal/bstack"
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

const maxHubTermsCache = 512

func (c *HubTermsCache) Put(hubURL string, t HubTerms) {
	if c == nil {
		return
	}
	u := normalizeHostURL(hubURL)
	now := time.Now()
	nowUnix := now.Unix()
	c.mu.Lock()
	if _, exists := c.items[u]; !exists && len(c.items) >= maxHubTermsCache {
		for k, v := range c.items {
			if now.Sub(v.fetchedAt) > 4*time.Minute || (v.terms.PrefixExpiresAt > 0 && nowUnix >= v.terms.PrefixExpiresAt-30) {
				delete(c.items, k)
			}
		}
		if len(c.items) >= maxHubTermsCache {
			var oldestKey string
			var oldestAt time.Time
			for k, v := range c.items {
				if oldestKey == "" || v.fetchedAt.Before(oldestAt) {
					oldestKey = k
					oldestAt = v.fetchedAt
				}
			}
			if oldestKey != "" {
				delete(c.items, oldestKey)
			}
		}
	}
	c.items[u] = cachedHubTerms{terms: t, fetchedAt: now}
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

func (s *Service) hubTopupLock(hubURL string) *sync.Mutex {
	s.topupMu.Lock()
	defer s.topupMu.Unlock()
	if s.topupLocks == nil {
		s.topupLocks = make(map[string]*sync.Mutex)
	}
	lk, ok := s.topupLocks[hubURL]
	if !ok {
		lk = &sync.Mutex{}
		s.topupLocks[hubURL] = lk
	}
	return lk
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
	// Serialize concurrent top-ups to the same Hub so simultaneous SSE/poll/plane 402s never double-pay.
	lk := s.hubTopupLock(u)
	lk.Lock()
	defer lk.Unlock()

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
	// Record on-chain spend IMMEDIATELY after port.Send broadcasts (before HTTP POST) so network errors never hide real spend!
	if recorder != nil {
		recorder(amount)
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
func SignEgressAuthHeaders(signer *nwallet.MarketSigner, action, hubURL string, cursor int64, domainsCSV string) (pub, tsStr, nonce, sig string, err error) {
	if signer == nil || signer.PubHex() == "" {
		return "", "", "", "", errors.New("discovery: signing key required for hub egress auth")
	}
	pub = signer.PubHex()
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
	payload := bstack.HubEgressAuth{
		Action:  action,
		Pub:     pub,
		TS:      ts,
		Nonce:   nonce,
		Aud:     aud,
		Cursor:  cursor,
		Domains: strings.ToLower(strings.TrimSpace(domainsCSV)),
	}
	sig, err = bstack.SignArtifactWithSigner(signer, payload)
	if err != nil {
		return "", "", "", "", err
	}
	return pub, tsStr, nonce, sig, nil
}
