package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/guard"

	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

// BuildFundedTx builds and signs a transaction paying outputs using multi-input CanonicalChangeSelector
// and dynamic feepolicy in embedded mode, OR delegates directly to wallet.CreateAction in client mode!
func (p *Port) BuildFundedTx(ctx context.Context, outputs []*transaction.TransactionOutput) (*transaction.Transaction, uint64, uint64, error) {
	tx, _, fee, spend, err := p.BuildFundedTxWithMeta(ctx, outputs, "", nil)
	return tx, fee, spend, err
}

// BuildFundedTxWithMeta builds and signs a funded transaction with a human-readable BRC-100
// description and structured labels (shown in BSV Desktop's Activity feed in client mode).
func (p *Port) BuildFundedTxWithMeta(ctx context.Context, outputs []*transaction.TransactionOutput, desc string, labels []string) (*transaction.Transaction, []byte, uint64, uint64, error) {
	return p.BuildFundedTxWithBasket(ctx, outputs, desc, labels, nil)
}

// BuildFundedTxWithBasket builds and signs a funded transaction while tagging its stateful
// covenant or carrier output(s) into a canonical BRC-100 asset basket (e.g. nektar-anchors).
func (p *Port) BuildFundedTxWithBasket(ctx context.Context, outputs []*transaction.TransactionOutput, desc string, labels []string, spec *nwallet.OutputTagSpec) (*transaction.Transaction, []byte, uint64, uint64, error) {
	if len(outputs) == 0 {
		return nil, nil, 0, 0, errors.New("chain: BuildFundedTx requires at least one output")
	}
	normDesc, normLabels := decorateActionWithContext(ctx, desc, labels)

	// Client mode: delegate cleanly to remote BRC-100 wallet.CreateAction.
	if p.engine == nil {
		res, err := p.clientCreateActionWithSpec(ctx, outputs, normDesc, normLabels, spec, false)
		if err != nil {
			return nil, nil, 0, 0, err
		}
		if res.Tx == nil || len(res.BEEF) == 0 {
			return nil, nil, 0, 0, fmt.Errorf("chain: client wallet did not return parseable AtomicBEEF for %s", res.TxID)
		}
		return res.Tx, res.BEEF, res.FeeSats, res.Satoshis + res.FeeSats, nil
	}

	// Embedded mode: build & sign atomically as NoSend via Engine.CreateAction so inputs stay reserved
	// until Broadcast() runs SpendGuard and submits to chain!
	wOuts := make([]sdkwallet.CreateActionOutput, len(outputs))
	var outTotal uint64
	for i, o := range outputs {
		if o == nil || o.LockingScript == nil {
			return nil, nil, 0, 0, fmt.Errorf("chain: output %d has nil locking script", i)
		}
		outTotal += o.Satoshis
		wOuts[i] = sdkwallet.CreateActionOutput{
			LockingScript:     append([]byte(nil), (*o.LockingScript)...),
			Satoshis:          o.Satoshis,
			OutputDescription: normDesc,
		}
	}
	applyOutputTagSpec(wOuts, spec)
	noSend := true
	randomize := false
	res, err := p.engine.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: normDesc,
		Labels:      normLabels,
		Outputs:     wOuts,
		Options: &sdkwallet.CreateActionOptions{
			NoSend:           &noSend,
			RandomizeOutputs: &randomize,
		},
	}, Originator)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	txid := res.Txid.String()
	var tx *transaction.Transaction
	beef := res.Tx
	if len(beef) > 0 {
		var pErr error
		_, tx, _, pErr = guard.ParseBEEF(beef, guard.DefaultBound)
		if pErr != nil {
			_ = p.engine.AbortNoSendByTxID(ctx, txid)
			return nil, nil, 0, 0, fmt.Errorf("chain: parse toolbox AtomicBEEF for %s: %w", txid, pErr)
		}
	}
	if tx == nil {
		tx, err = p.engine.LoadParsedTx(ctx, txid)
		if err != nil {
			_ = p.engine.AbortNoSendByTxID(ctx, txid)
			return nil, nil, 0, 0, err
		}
	}
	if len(beef) == 0 {
		var bErr error
		beef, bErr = SafeAtomicBEEF(tx)
		if bErr != nil {
			_ = p.engine.AbortNoSendByTxID(ctx, txid)
			return nil, nil, 0, 0, bErr
		}
	}
	feeSats := p.engine.TxFeeSats(ctx, txid)
	p.mu.Lock()
	p.pendingNoSend[txid] = txid
	p.mu.Unlock()

	return tx, beef, feeSats, outTotal + feeSats, nil
}

// BuildBRC29Tx constructs a signed (NoSend in embedded mode) BRC-29 payment transaction and returns
// its remittance metadata BEFORE broadcasting so durable workflows can persist txid + BEEF + nonces
// atomically prior to Broadcast (eliminating double-pay and stranded remittance on crash/retry).
// In client mode, where the remote wallet signs and broadcasts atomically, it executes Send directly
// and sets alreadyBroadcast=true.
func (p *Port) BuildBRC29Tx(ctx context.Context, toIdentityKey string, sats uint64, desc string) (tx *transaction.Transaction, fee, spend uint64, res *nwallet.SendResult, alreadyBroadcast bool, err error) {
	if p.engine == nil {
		sr, sErr := p.Send(ctx, nwallet.SendArgs{
			ToIdentityKey: toIdentityKey,
			Satoshis:      sats,
			Description:   desc,
			Labels:        []string{"market", "purchase"},
		})
		if sErr != nil {
			return nil, 0, 0, nil, false, sErr
		}
		return sr.Tx, sr.FeeSats, sr.Satoshis + sr.FeeSats, sr, true, nil
	}
	preB64, sufB64 := randomBRC29Nonce()
	lockScript, err := p.signer.PaymentDestination(ctx, strings.TrimSpace(toIdentityKey), preB64, sufB64)
	if err != nil {
		return nil, 0, 0, nil, false, err
	}
	tx, beef, fee, spend, err := p.BuildFundedTxWithMeta(ctx, []*transaction.TransactionOutput{{Satoshis: sats, LockingScript: lockScript}}, desc, []string{"market", "purchase", "brc29"})
	if err != nil {
		return nil, 0, 0, nil, false, err
	}
	var outIdx uint32
	matched := false
	if tx != nil && lockScript != nil {
		for idx, o := range tx.Outputs {
			if o.LockingScript != nil && bytes.Equal(*o.LockingScript, *lockScript) && o.Satoshis == sats {
				outIdx = uint32(idx)
				matched = true
				break
			}
		}
	}
	if !matched {
		_ = p.engine.AbortNoSendByTxID(ctx, tx.TxID().String())
		return nil, 0, 0, nil, false, errors.New("chain: built BRC-29 tx missing expected destination output")
	}
	if len(beef) == 0 {
		var bErr error
		beef, bErr = SafeAtomicBEEF(tx)
		if bErr != nil {
			_ = p.engine.AbortNoSendByTxID(ctx, tx.TxID().String())
			return nil, 0, 0, nil, false, bErr
		}
	}
	return tx, fee, spend, &nwallet.SendResult{
		TxID:              tx.TxID().String(),
		Satoshis:          sats,
		FeeSats:           fee,
		BEEF:              beef,
		BEEFHex:           hex.EncodeToString(beef),
		Tx:                tx,
		OutputIndex:       outIdx,
		DerivationPrefix:  preB64,
		DerivationSuffix:  sufB64,
		SenderIdentityKey: p.IdentityPubHex(),
	}, false, nil
}

// Broadcast enforces SpendGuard caps, then broadcasts tx on testchain or configured live settler.
// On ANY failure before broadcast succeeds, reserved UTXOs are atomically rolled back via go-wallet-toolbox AbortAction!
func (p *Port) Broadcast(ctx context.Context, tx *transaction.Transaction, feeSats, spendSats uint64) (string, error) {
	if tx == nil {
		return "", errors.New("chain: nil transaction")
	}
	txid := tx.TxID().String()

	// Client mode: BuildFundedTx already broadcast via remote BRC-100 CreateAction (and ran SpendGuard).
	if p.engine == nil && p.hasClientTx(txid) {
		return txid, nil
	}

	p.mu.Lock()
	_, hadNoSend := p.pendingNoSend[txid]
	if hadNoSend {
		delete(p.pendingNoSend, txid)
	}
	p.mu.Unlock()
	// Consult durable wallet.sqlite status in addition to RAM pendingNoSend so a restart/retry
	// between BuildFundedTx and Broadcast NEVER bypasses SendNoSendTx into raw settler.Submit.
	if p.engine != nil && !hadNoSend {
		if st, ok := p.engine.TxStatus(ctx, txid); ok && st == "nosend" {
			hadNoSend = true
		}
	}

	rollback := func() {
		if p.engine == nil || !hadNoSend {
			return
		}
		_ = p.engine.AbortNoSendByTxID(ctx, txid)
	}

	// If tx was created via BuildFundedTx (NoSend=true in go-wallet-toolbox), broadcast it via
	// go-wallet-toolbox SendWith so PostFromBEEF runs the single exact SpendGuard chokepoint
	// and storage status, KnownTx, and UTXO lifecycle stay 100% consistent!
	if p.engine != nil && hadNoSend {
		if err := p.engine.SendNoSendTx(ctx, txid); err != nil {
			if errors.Is(err, nwallet.ErrBroadcastUnknown) {
				// Broadcast outcome uncertain: NEVER abort/unlock UTXOs! Mark submission_unknown for Monitor.UnFail.
				_ = p.engine.MarkSubmissionUnknown(ctx, txid)
				return "", fmt.Errorf("chain: broadcast %s: %w", txid, err)
			}
			// Only release UTXOs locally on genuine pre-broadcast refusal (SpendGuard cap);
			// post-submit broadcaster error strings leave the tx in submission_unknown until reconciled.
			if errors.Is(err, nwallet.ErrSpendRejected) {
				rollback()
			} else {
				_ = p.engine.MarkSubmissionUnknown(ctx, txid)
			}
			return "", fmt.Errorf("chain: broadcast %s: %w", txid, err)
		}
		if p.testChain != nil {
			p.tcMu.Lock()
			if mp, _, ok := p.testChain.Proof(txid); ok && mp != nil {
				tx.MerklePath = mp
			}
			p.tcMu.Unlock()
		}
		p.engine.CacheTx(tx)
		return txid, nil
	}

	if err := p.guard.CheckAndRecord(txid, feeSats, spendSats); err != nil {
		rollback()
		return "", err
	}

	if p.testChain != nil {
		p.tcMu.Lock()
		err := p.testChain.Send(tx)
		if err == nil {
			if pmp, _, ok := p.testChain.Proof(txid); ok {
				tx.MerklePath = pmp
			}
		}
		p.tcMu.Unlock()
		if err != nil {
			if rErr := p.guard.Refund(txid); rErr != nil {
				return "", fmt.Errorf("chain: testchain broadcast: %w (spendguard refund failed: %v)", err, rErr)
			}
			return "", fmt.Errorf("chain: testchain broadcast: %w", err)
		}
		if p.engine != nil {
			p.engine.CacheTx(tx)
			p.engine.FuelKeeper().Wake()
		}
		return txid, nil
	}

	if p.broadcaster == nil {
		if rErr := p.guard.Refund(txid); rErr != nil {
			return "", fmt.Errorf("chain: no live broadcaster configured (spendguard refund failed: %w)", rErr)
		}
		return "", errors.New("chain: no live broadcaster configured (set NEKTAR_SETTLE_SPEC)")
	}
	if err := p.broadcaster.Broadcast(ctx, tx); err != nil {
		// Never refund SpendGuard on post-submit broadcaster error strings: treat all post-submit
		// failures conservatively (ErrBroadcastUnknown on transport/timeout, error hint otherwise).
		if nwallet.IsUncertainBroadcastError(err) {
			return "", fmt.Errorf("chain: %s broadcast: %w: %w", p.broadcaster.Name(), nwallet.ErrBroadcastUnknown, err)
		}
		return "", fmt.Errorf("chain: %s broadcast: %w", p.broadcaster.Name(), err)
	}
	if p.engine != nil {
		p.engine.CacheTx(tx)
		p.engine.FuelKeeper().Wake()
	}
	return txid, nil
}

// RelayExternalTx broadcasts an incoming third-party (buyer) transaction to the configured live settler
// before releasing paid goods under zero-conf policy (does not consume the receiver's SpendGuard budget).
func (p *Port) RelayExternalTx(ctx context.Context, tx *transaction.Transaction) error {
	if tx == nil {
		return errors.New("chain: nil external transaction")
	}
	if p.testChain != nil || p.network == "memory" {
		return nil
	}
	if p.broadcaster == nil {
		return errors.New("chain: no live broadcaster configured to broadcast zero-conf buyer payment")
	}
	return p.broadcaster.Broadcast(ctx, tx)
}

// AnchorOutput helper creates a 1-sat output locked to a NektarAnchor script.
func AnchorOutput(lockScript *script.Script) *transaction.TransactionOutput {
	return &transaction.TransactionOutput{
		Satoshis:      1,
		LockingScript: lockScript,
	}
}
