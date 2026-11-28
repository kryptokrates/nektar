package chain

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/guard"

	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

// Send pays to a P2PKH address or BRC-100 identity key (BRC-29) in either embedded or client mode.
func (p *Port) Send(ctx context.Context, args nwallet.SendArgs) (*nwallet.SendResult, error) {
	if p.engine != nil {
		return p.engine.Send(ctx, args)
	}
	// Client mode: execute directly through remote BRC-100 wallet.CreateAction!
	if p.wallet == nil {
		return nil, errors.New("chain: wallet not initialized")
	}
	if args.Satoshis == 0 {
		return nil, errors.New("chain: send amount must be > 0")
	}
	var lockScript *script.Script
	var err error
	if args.ToAddress != "" {
		addr, aErr := script.NewAddressFromString(strings.TrimSpace(args.ToAddress))
		if aErr != nil {
			return nil, fmt.Errorf("chain: invalid address %q: %w", args.ToAddress, aErr)
		}
		lockScript, err = p2pkh.Lock(addr)
		if err != nil {
			return nil, err
		}
	} else if args.ToIdentityKey != "" {
		preB64, sufB64 := randomBRC29Nonce()
		if s := strings.TrimSpace(args.DerivationPrefix); s != "" {
			preB64 = s
		}
		if s := strings.TrimSpace(args.DerivationSuffix); s != "" {
			sufB64 = s
		}
		lockScript, err = p.signer.PaymentDestination(ctx, strings.TrimSpace(args.ToIdentityKey), preB64, sufB64)
		if err != nil {
			return nil, err
		}
		res, sErr := p.clientCreateActionWithLabels(ctx, []*transaction.TransactionOutput{{Satoshis: args.Satoshis, LockingScript: lockScript}}, args.Description, append([]string{"brc29"}, args.Labels...), args.OverrideGuard)
		if sErr != nil {
			return nil, sErr
		}
		if res.Tx == nil {
			return nil, fmt.Errorf("chain: client wallet returned unparseable BRC-29 BEEF for broadcast tx %s (outputIndex unknown)", res.TxID)
		}
		matched := false
		for idx, o := range res.Tx.Outputs {
			if o.LockingScript != nil && bytes.Equal(*o.LockingScript, *lockScript) && o.Satoshis == args.Satoshis {
				res.OutputIndex = uint32(idx)
				matched = true
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("chain: client wallet broadcast tx %s did not contain expected BRC-29 output script", res.TxID)
		}
		res.DerivationPrefix = preB64
		res.DerivationSuffix = sufB64
		res.SenderIdentityKey = p.IdentityPubHex()
		return res, nil
	} else {
		return nil, errors.New("chain: toAddress or toIdentityKey required")
	}
	return p.clientCreateActionWithLabels(ctx, []*transaction.TransactionOutput{{Satoshis: args.Satoshis, LockingScript: lockScript}}, args.Description, args.Labels, args.OverrideGuard)
}

func (p *Port) clientCreateActionWithLabels(ctx context.Context, outputs []*transaction.TransactionOutput, desc string, labels []string, overrideGuard bool) (*nwallet.SendResult, error) {
	return p.clientCreateActionWithSpec(ctx, outputs, desc, labels, nil, overrideGuard)
}

func (p *Port) reserveClientSpend(ctx context.Context, outTotal uint64, numOutputs int, extraInputBytes int, overrideGuard bool) (string, uint64, error) {
	estBytes := 250 + numOutputs*34 + max(0, extraInputBytes)
	estFee := uint64(max(200, (estBytes+9)/10))
	if p.feeSource != nil {
		if f, fErr := p.feeSource.Fees(ctx); fErr == nil {
			if ef, eErr := f.For(estBytes); eErr == nil {
				estFee = ef
			}
		}
	}
	if overrideGuard || p.guard == nil {
		return "", estFee, nil
	}
	resID, err := p.guard.Reserve(estFee, outTotal+estFee)
	if err != nil {
		return "", estFee, err
	}
	return resID, estFee, nil
}

func (p *Port) commitClientSpend(resID, txid string, outTotal, estFee uint64, parsedTx *transaction.Transaction, overrideGuard bool) (uint64, error) {
	actualFee := estFee
	if parsedTx != nil {
		if f := nwallet.ComputeTxFee(parsedTx); f > 0 {
			actualFee = f
		}
	}
	if !overrideGuard && p.guard != nil {
		if err := p.guard.CommitReservation(resID, txid, actualFee, outTotal+actualFee); err != nil {
			return actualFee, fmt.Errorf("chain: persist SpendGuard record for broadcast tx %s: %w", txid, err)
		}
	}
	return actualFee, nil
}

func (p *Port) clientCreateActionWithSpec(ctx context.Context, outputs []*transaction.TransactionOutput, desc string, labels []string, spec *nwallet.OutputTagSpec, overrideGuard bool) (*nwallet.SendResult, error) {
	var outTotal uint64
	for _, o := range outputs {
		outTotal += o.Satoshis
	}
	// Atomically reserve daily SpendGuard budget BEFORE asking remote BRC-100 wallet to broadcast.
	resID, estFee, rErr := p.reserveClientSpend(ctx, outTotal, len(outputs), 0, overrideGuard)
	if rErr != nil {
		return nil, rErr
	}
	desc, normLabels := decorateActionWithContext(ctx, desc, labels)
	wOuts := make([]sdkwallet.CreateActionOutput, len(outputs))
	for i, o := range outputs {
		wOuts[i] = sdkwallet.CreateActionOutput{
			LockingScript:     append([]byte(nil), (*o.LockingScript)...),
			Satoshis:          o.Satoshis,
			OutputDescription: desc,
		}
	}
	applyOutputTagSpec(wOuts, spec)
	acceptDelayed := false
	randomize := false
	actRes, err := p.wallet.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: desc,
		Labels:      normLabels,
		Outputs:     wOuts,
		Options: &sdkwallet.CreateActionOptions{
			AcceptDelayedBroadcast: &acceptDelayed,
			RandomizeOutputs:       &randomize,
		},
	}, Originator)
	if err != nil {
		if nwallet.IsUncertainBroadcastError(err) {
			// Remote wallet may already have broadcast: keep SpendGuard reservation intact and return ErrBroadcastUnknown.
			return nil, fmt.Errorf("chain: client wallet CreateAction: %w: %w", nwallet.ErrBroadcastUnknown, err)
		}
		if resID != "" {
			p.guard.CancelReservation(resID)
		}
		return nil, fmt.Errorf("chain: client wallet CreateAction: %w", err)
	}
	txid := actRes.Txid.String()
	var parsedTx *transaction.Transaction
	var parseErr error
	if len(actRes.Tx) > 0 {
		_, parsedTx, _, parseErr = guard.ParseBEEF(actRes.Tx, guard.DefaultBound)
		p.rememberClientTxBytes(txid, actRes.Tx, parsedTx)
	} else {
		p.rememberClientTxBytes(txid, nil, nil)
	}
	// Unconditionally commit real broadcast spend so spentToday never under-counts.
	actualFee, cErr := p.commitClientSpend(resID, txid, outTotal, estFee, parsedTx, overrideGuard)
	if cErr != nil {
		return nil, cErr
	}
	if parseErr != nil {
		return nil, fmt.Errorf("chain: client wallet returned malformed AtomicBEEF for broadcast tx %s: %w", txid, parseErr)
	}
	return &nwallet.SendResult{
		TxID:     txid,
		Satoshis: outTotal,
		FeeSats:  actualFee,
		BEEF:     actRes.Tx,
		BEEFHex:  hex.EncodeToString(actRes.Tx),
		Tx:       parsedTx,
	}, nil
}

func (p *Port) rememberClientTxBytes(txid string, rawBEEF []byte, tx *transaction.Transaction) {
	if txid == "" {
		return
	}
	// Persist durably under dataDir/client-txs so RAM eviction never breaks Broadcast idempotency or Port.Tx.
	if p.clientTxDir != "" && len(txid) == 64 {
		if err := os.MkdirAll(p.clientTxDir, 0o700); err == nil {
			path := filepath.Join(p.clientTxDir, txid+".beef")
			payload := rawBEEF
			if len(payload) == 0 && tx != nil {
				if b, err := tx.AtomicBEEF(false); err == nil && len(b) > 0 {
					payload = b
				}
			}
			if len(payload) > 0 {
				_ = os.WriteFile(path, payload, 0o600)
			} else {
				_ = os.WriteFile(filepath.Join(p.clientTxDir, txid+".sent"), []byte(txid), 0o600)
			}
		}
	}
	if tx == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.clientBuilt) > 256 {
		for k := range p.clientBuilt {
			delete(p.clientBuilt, k)
			if len(p.clientBuilt) <= 128 {
				break
			}
		}
	}
	p.clientBuilt[txid] = tx
}

func (p *Port) hasClientTx(txid string) bool {
	if txid == "" {
		return false
	}
	p.mu.Lock()
	_, ok := p.clientBuilt[txid]
	p.mu.Unlock()
	if ok {
		return true
	}
	if p.clientTxDir != "" && len(txid) == 64 {
		if _, err := os.Stat(filepath.Join(p.clientTxDir, txid+".beef")); err == nil {
			return true
		}
		if _, err := os.Stat(filepath.Join(p.clientTxDir, txid+".sent")); err == nil {
			return true
		}
	}
	return false
}

func (p *Port) loadClientTx(txid string) *transaction.Transaction {
	if txid == "" {
		return nil
	}
	p.mu.Lock()
	tx := p.clientBuilt[txid]
	p.mu.Unlock()
	if tx != nil {
		return tx
	}
	if p.clientTxDir != "" && len(txid) == 64 {
		if raw, err := os.ReadFile(filepath.Join(p.clientTxDir, txid+".beef")); err == nil && len(raw) > 0 {
			if _, parsed, _, pErr := guard.ParseBEEF(raw, guard.DefaultBound); pErr == nil && parsed != nil {
				p.mu.Lock()
				p.clientBuilt[txid] = parsed
				p.mu.Unlock()
				return parsed
			}
		}
	}
	return nil
}

// SendToAddress builds, signs, SpendGuard-checks, and broadcasts a payment to a BSV P2PKH address.
// Works seamlessly in BOTH embedded and client (BSV Desktop / BRC-100 wire) modes.
// Never blindly retries a second payment on broadcaster error strings.
func (p *Port) SendToAddress(ctx context.Context, address string, sats uint64) (*transaction.Transaction, string, error) {
	if sats == 0 {
		return nil, "", errors.New("chain: send amount must be > 0")
	}
	sendMeta := nwallet.ActionSendBSV(sats)
	res, err := p.Send(ctx, nwallet.SendArgs{
		ToAddress:   address,
		Satoshis:    sats,
		Description: sendMeta.Description,
		Labels:      sendMeta.Labels,
	})
	if err != nil {
		return nil, "", err
	}
	return res.Tx, res.TxID, nil
}

func randomBRC29Nonce() (string, string) {
	var pre, suf [16]byte
	if _, err := rand.Read(pre[:]); err != nil {
		panic("chain: crypto/rand failed: " + err.Error())
	}
	if _, err := rand.Read(suf[:]); err != nil {
		panic("chain: crypto/rand failed: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString(pre[:]), base64.StdEncoding.EncodeToString(suf[:])
}
