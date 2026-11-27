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
	"strconv"
	"strings"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/carrier"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

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

	feeMeta := nwallet.ActionPublishDirectoryWithFee(totalFeeSats)
	fundTx, fundBEEF, feeSats, spendSats, err := port.BuildFundedTxWithMeta(ctx, outputs, feeMeta.Description, feeMeta.Labels)
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
	hubFeeMeta := nwallet.ActionPayHubFee(action, amountSats)
	res, err := port.Send(ctx, nwallet.SendArgs{
		ToIdentityKey:    strings.TrimSpace(payeePub),
		DerivationPrefix: strings.TrimSpace(prefixB64),
		Satoshis:         amountSats,
		Description:      hubFeeMeta.Description,
		Labels:           hubFeeMeta.Labels,
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

func randomBase64Nonce16() (string, error) {
	var raw [16]byte
	if _, err := crand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw[:]), nil
}
