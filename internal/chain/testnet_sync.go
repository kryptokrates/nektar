package chain

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/guard"

	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

// SafeAtomicBEEF encodes tx as a strict BRC-95 AtomicBEEF (allowPartial=false).
func SafeAtomicBEEF(tx *transaction.Transaction) ([]byte, error) {
	return nwallet.SafeAtomicBEEF(tx)
}

// AdoptReceived credits any outputs in a counterparty's verified AtomicBEEF payment tx that pay
// this wallet's FundScript() into wallet.sqlite. Fails closed in client mode where BRC-29 remittance is required.
func (p *Port) AdoptReceived(ctx context.Context, tx *transaction.Transaction) (uint64, error) {
	if tx == nil {
		return 0, nil
	}
	if p.engine != nil {
		return p.engine.AdoptReceivedTx(ctx, tx)
	}
	return 0, errors.New("chain: static P2PKH AdoptReceived is unsupported in client wallet mode; use BRC-29 payment remittance")
}

// InternalizeBRC29 runs full local consensus SPV verification (go-sdk/spv.Verify against this Port's
// witnessed ChainTracker) BEFORE internalizing an incoming AtomicBEEF payment with BRC-29 remittance
// in BOTH embedded and client (BSV Desktop / BRC-100 wire) modes.
func (p *Port) InternalizeBRC29(ctx context.Context, beef []byte, outputIndex uint32, prefixB64, suffixB64, senderPubHex, description string, labels []string) (uint64, error) {
	if p.wallet == nil {
		return 0, errors.New("chain: wallet not initialized")
	}
	preRaw, err := base64.StdEncoding.DecodeString(prefixB64)
	if err != nil || len(preRaw) == 0 {
		return 0, fmt.Errorf("chain: invalid BRC-29 derivationPrefix: %w", err)
	}
	sufRaw, err := base64.StdEncoding.DecodeString(suffixB64)
	if err != nil || len(sufRaw) == 0 {
		return 0, fmt.Errorf("chain: invalid BRC-29 derivationSuffix: %w", err)
	}
	senderPub, err := guard.ParsePubKeyHex(senderPubHex)
	if err != nil {
		return 0, fmt.Errorf("chain: invalid senderIdentityKey: %w", err)
	}
	var tx *transaction.Transaction
	if preTx := nwallet.PreverifiedTxFromCtx(ctx); preTx != nil {
		tx = preTx
	} else {
		tracker := p.tracker
		if tracker == nil && p.engine != nil {
			tracker = p.engine.Tracker()
		}
		ver, err := VerifyCounterpartyPaymentBEEF(ctx, beef, tracker, PaymentVerifyOpts{
			MaxUnconfirmedAncestors: p.maxUnconfAnc,
			SettleDepth:             p.settleDepth,
			AllowZeroConf:           true, // caller enforces deal/paywall 0-conf value cap before goods release
		})
		if err != nil {
			return 0, fmt.Errorf("chain: BRC-29 SPV verification failed: %w", err)
		}
		tx = ver.Tx
		ctx = nwallet.WithPreverifiedTx(ctx, tx)
	}
	if uint64(outputIndex) >= uint64(len(tx.Outputs)) {
		return 0, fmt.Errorf("chain: BRC-29 outputIndex %d out of range", outputIndex)
	}
	// Fast-lane zero-GORM internalization: SPV already verified above; commit incoming BRC-29 UTXO + BEEF
	// atomically into Pebble WAL in ~50us without blocking Seller's HandlePurchase on wallet.sqlite GORM!
	if p.engine != nil {
		if fl := p.engine.FastLane(); fl != nil {
			if err := fl.RecordIncomingBRC29(tx, outputIndex, prefixB64, suffixB64, senderPubHex, beef); err != nil {
				return 0, fmt.Errorf("chain: fast-lane internalize BRC-29: %w", err)
			}
			return tx.Outputs[outputIndex].Satoshis, nil
		}
	}
	// Mark incoming internalization so go-wallet-toolbox's BackgroundBroadcast never charges the
	// buyer's change output against the seller's SpendGuard budget.
	intCtx := nwallet.WithIncomingInternalize(ctx)
	res, err := p.wallet.InternalizeAction(intCtx, sdkwallet.InternalizeActionArgs{
		Tx:          beef,
		Description: description,
		Labels:      labels,
		Outputs: []sdkwallet.InternalizeOutput{{
			OutputIndex: outputIndex,
			Protocol:    sdkwallet.InternalizeProtocolWalletPayment,
			PaymentRemittance: &sdkwallet.Payment{
				DerivationPrefix:  preRaw,
				DerivationSuffix:  sufRaw,
				SenderIdentityKey: senderPub,
			},
		}},
	}, Originator)
	if err != nil {
		return 0, err
	}
	if res == nil || !res.Accepted {
		return 0, errors.New("chain: wallet rejected BRC-29 payment internalization")
	}
	return tx.Outputs[outputIndex].Satoshis, nil
}

// SyncFull returns the complete SyncReport from Monitor.
func (p *Port) SyncFull(ctx context.Context) (nwallet.SyncReport, error) {
	if p.engine == nil {
		return nwallet.SyncReport{}, nil
	}
	return p.engine.Monitor().SyncAll(ctx)
}
