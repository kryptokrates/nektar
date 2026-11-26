package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/carrier"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
)

// BuildMarketBEEF encodes a MarketPayload into an OP_FALSE OP_RETURN AtomicBEEF transaction for Hub /submit
// (memory/regtest test mode only; live networks require BuildFundedMarketCarrierBEEF).
func BuildMarketBEEF(p bstack.MarketPayload) ([]byte, error) {
	cborBytes, err := bstack.EncodePayload(p)
	if err != nil {
		return nil, err
	}
	ls := &script.Script{}
	_ = ls.AppendOpcodes(script.OpFALSE, script.OpRETURN)
	_ = ls.AppendPushData(cborBytes)
	tx := transaction.NewTransaction()
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 0, LockingScript: ls})
	return tx.AtomicBEEF(false)
}

// BuildFundedMarketCarrierBEEF mints a funded, non-mineable (LockTime=2100, Sequence=0) PushDrop carrier
// transaction via lightwebinc/bcommon/carrier whose funding input carries full SPV ancestry up to mined BUMPs.
// Satisfies all REFUSED-* checks in MarketTopicManager and verify.Check on live networks.
func BuildFundedMarketCarrierBEEF(ctx context.Context, port *chain.Port, priv *ec.PrivateKey, p bstack.MarketPayload) ([]byte, error) {
	if port == nil {
		return nil, errors.New("discovery: chain port required for funded market carrier")
	}
	if priv == nil {
		return nil, errors.New("discovery: signing key required for funded market carrier")
	}
	cborBytes, err := bstack.EncodePayload(p)
	if err != nil {
		return nil, err
	}
	protoWallet, err := sdkwallet.NewCompletedProtoWallet(priv)
	if err != nil {
		return nil, fmt.Errorf("discovery: init carrier signer wallet: %w", err)
	}
	fundLock, err := carrier.FundingLock(ctx, protoWallet, bstack.Originator, bstack.MarketCarrierParams)
	if err != nil {
		return nil, fmt.Errorf("discovery: derive carrier funding lock: %w", err)
	}
	fundTx, _, feeSats, spendSats, err := port.BuildFundedTxWithBEEF(ctx, []*transaction.TransactionOutput{
		{Satoshis: 1, LockingScript: fundLock},
	})
	if err != nil {
		return nil, fmt.Errorf("discovery: build carrier funding tx: %w", err)
	}
	if _, err := port.Broadcast(ctx, fundTx, feeSats, spendSats); err != nil {
		return nil, fmt.Errorf("discovery: broadcast carrier funding tx: %w", err)
	}
	var fundVout uint32
	foundOut := false
	for i, o := range fundTx.Outputs {
		if o != nil && o.LockingScript != nil && bytes.Equal(*o.LockingScript, *fundLock) {
			fundVout = uint32(i)
			foundOut = true
			break
		}
	}
	if !foundOut {
		return nil, errors.New("discovery: carrier funding output not found in built transaction")
	}
	carrierTx, err := carrier.Mint(ctx, protoWallet, bstack.Originator, bstack.MarketCarrierParams, cborBytes, fundTx, fundVout)
	if err != nil {
		return nil, fmt.Errorf("discovery: mint PushDrop carrier: %w", err)
	}
	if len(carrierTx.Inputs) > 0 && carrierTx.Inputs[0].SourceTransaction == nil {
		carrierTx.Inputs[0].SourceTransaction = fundTx
	}
	beef, err := carrierTx.AtomicBEEF(false)
	if err != nil {
		return nil, fmt.Errorf("discovery: encode carrier AtomicBEEF: %w", err)
	}
	return beef, nil
}

// BuildAndSubmitMarketAd builds a market artifact BEEF and submits it to all eligible hosts,
// automatically piggybacking paid Hub BRC-29 tolls onto ONE single mined fundTx on live networks
// (zero extra miner fee!) and enforcing HubFeeApprover fail-closed against fee-gouging hubs.
func (s *Service) BuildAndSubmitMarketAd(
	ctx context.Context,
	priv *ec.PrivateKey,
	p bstack.MarketPayload,
	hosts []string,
	topics []string,
) (int, error) {
	if len(hosts) == 0 {
		return 0, nil
	}
	action := "submit:" + string(p.Kind)
	s.mu.RLock()
	approver := s.feeApprover
	recorder := s.feeRecorder
	s.mu.RUnlock()

	isLive := s.network == "main" || s.network == "testnet"
	var eligibleHosts []string
	var piggyTargets []HubPiggybackTarget
	for _, h := range hosts {
		hu := normalizeHostURL(h)
		if hu == "" {
			continue
		}
		if terms, tErr := s.FetchHubTerms(ctx, hu, false); tErr == nil {
			fee := terms.Fees[action]
			if fee > 0 {
				if approver == nil || approver(action, fee) != nil {
					continue // skip gouging/over-policy hub fail-closed before spending a single sat!
				}
				if isLive && terms.PayeeIdentityKey != "" && terms.Prefixes[action] != "" {
					piggyTargets = append(piggyTargets, HubPiggybackTarget{
						HubURL:           hu,
						PayeeIdentityKey: terms.PayeeIdentityKey,
						DerivationPrefix: terms.Prefixes[action],
						Action:           action,
						AmountSats:       fee,
					})
				}
			}
		}
		eligibleHosts = append(eligibleHosts, hu)
	}
	if len(eligibleHosts) == 0 {
		return 0, fmt.Errorf("discovery: all %d hub(s) exceeded operator fee policy for %s", len(hosts), action)
	}

	var beef []byte
	var perHubPay map[string]string
	var err error
	if isLive {
		if s.mem == nil || s.mem.Chain() == nil {
			return 0, errors.New("discovery: chain port required to mint funded PushDrop carrier on live network")
		}
		if len(piggyTargets) == 0 {
			// Paper 011 §3.2 / bcommon/producer.Trees: reuse persistent M=16 mined funding tree
			// (16x lower miner fees, instant mined-parent BUMP, kill-switch retractable).
			tree := s.CarrierTree(priv)
			beef, _, _, err = tree.MintCarrier(ctx, p)
			if err != nil {
				return 0, err
			}
		} else {
			var piggySats uint64
			beef, perHubPay, piggySats, err = BuildFundedMarketCarrierWithHubFees(ctx, s.mem.Chain(), priv, p, piggyTargets)
			if err != nil {
				return 0, err
			}
			if piggySats > 0 && recorder != nil {
				recorder(piggySats)
			}
		}
	} else {
		beef, err = BuildMarketBEEF(p)
		if err != nil {
			return 0, err
		}
	}

	submitted := 0
	var lastErr error
	for _, hu := range eligibleHosts {
		if sErr := s.SubmitBEEFWithPayment(ctx, hu, beef, topics, perHubPay[hu]); sErr == nil {
			submitted++
		} else {
			lastErr = sErr
		}
	}
	return submitted, lastErr
}

// SubmitBEEF posts an AtomicBEEF to a Hub's /submit endpoint for topics, handling BRC-105 HTTP 402
// payment challenges automatically within operator policy caps.
func (s *Service) SubmitBEEF(ctx context.Context, hubURL string, beef []byte, topics []string) error {
	return s.SubmitBEEFWithPayment(ctx, hubURL, beef, topics, "")
}

// SubmitBEEFWithPayment posts an AtomicBEEF to a Hub's /submit endpoint with an optional prebuilt
// X-BSV-Payment header (e.g. from fundTx piggybacking) and falls back to 1-shot 402 retry on challenge.
func (s *Service) SubmitBEEFWithPayment(ctx context.Context, hubURL string, beef []byte, topics []string, prebuiltPaymentHdr string) error {
	target := strings.TrimRight(hubURL, "/") + "/submit"
	tHdr, _ := json.Marshal(topics)

	sendOnce := func(payHdr string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(beef))
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Topics", string(tHdr))
		req.Header.Set("Content-Type", "application/octet-stream")
		if payHdr != "" {
			req.Header.Set("X-BSV-Payment", payHdr)
		}
		return s.httpClient.Do(req)
	}

	resp, err := sendOnce(prebuiltPaymentHdr)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusPaymentRequired && prebuiltPaymentHdr == "" {
		s.termsCache.Invalidate(hubURL)
		if s.mem == nil || s.mem.Chain() == nil {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			return fmt.Errorf("hub submit HTTP 402: %s", strings.TrimSpace(string(msg)))
		}
		s.mu.RLock()
		approver := s.feeApprover
		recorder := s.feeRecorder
		s.mu.RUnlock()
		payHdr, _, pErr := PayHub402Challenge(ctx, s.mem.Chain(), approver, recorder, "submit:market", resp)
		resp.Body.Close()
		if pErr != nil {
			return fmt.Errorf("hub submit HTTP 402: %w", pErr)
		}
		resp, err = sendOnce(payHdr)
		if err != nil {
			return err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("hub submit HTTP 202 (payment held awaiting confirmation): %s", strings.TrimSpace(string(msg)))
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("hub submit HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
