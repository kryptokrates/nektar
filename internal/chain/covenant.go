package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
)

// FundPubKey returns the compressed public key for Nektar's canonical BRC-43 funding key
// ("nektar fund" / "nektar-fund" / Self) in both embedded and BRC-100 client modes.
func (p *Port) FundPubKey(ctx context.Context) (*ec.PublicKey, error) {
	if p.engine != nil {
		return p.engine.FundPubKey(), nil
	}
	if p.wallet == nil {
		return nil, errors.New("chain: wallet not initialized")
	}
	res, err := p.wallet.GetPublicKey(ctx, sdkwallet.GetPublicKeyArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{
			ProtocolID: NektarWalletProfile.FundProtocol,
			KeyID:      NektarWalletProfile.FundKeyID,
			Counterparty: sdkwallet.Counterparty{
				Type: sdkwallet.CounterpartyTypeSelf,
			},
		},
	}, Originator)
	if err != nil {
		return nil, err
	}
	return res.PublicKey, nil
}

// FundPKH returns the 20-byte Hash160 of FundPubKey (matching FundAddress()).
func (p *Port) FundPKH(ctx context.Context) ([]byte, error) {
	pub, err := p.FundPubKey(ctx)
	if err != nil {
		return nil, err
	}
	return core.Hash160(pub.Compressed()), nil
}

// SignCovenantSighash signs a 32-byte BIP-143 sighash using the local BRC-43/identity key whose
// Hash160 matches targetPKH, returning (lowSDERWithAllForkID, compressedPubKey33, error).
// Raw private keys NEVER leave internal/wallet or the remote BRC-100 wallet.
func (p *Port) SignCovenantSighash(ctx context.Context, targetPKH []byte, sighash32 []byte) ([]byte, []byte, error) {
	if len(targetPKH) != 20 {
		return nil, nil, fmt.Errorf("chain: targetPKH must be 20 bytes, got %d", len(targetPKH))
	}
	if len(sighash32) != 32 {
		return nil, nil, fmt.Errorf("chain: sighash must be 32 bytes, got %d", len(sighash32))
	}

	// 1. Match BRC-43 funding key (default for DisclosureSale.DepositorPkh, NektarAgentVault.AgentPkh, new NektarAnchor.OwnerPKH).
	if fundPub, err := p.FundPubKey(ctx); err == nil && fundPub != nil {
		comp := fundPub.Compressed()
		if bytes.Equal(core.Hash160(comp), targetPKH) {
			var der []byte
			if p.engine != nil {
				der, err = p.engine.SignFundHash(sighash32)
			} else {
				sigRes, sErr := p.wallet.CreateSignature(ctx, sdkwallet.CreateSignatureArgs{
					EncryptionArgs: sdkwallet.EncryptionArgs{
						ProtocolID: NektarWalletProfile.FundProtocol,
						KeyID:      NektarWalletProfile.FundKeyID,
						Counterparty: sdkwallet.Counterparty{
							Type: sdkwallet.CounterpartyTypeSelf,
						},
					},
					HashToDirectlySign: sighash32,
				}, Originator)
				if sErr == nil && sigRes != nil && sigRes.Signature != nil {
					der = sigRes.Signature.Serialize()
				} else {
					err = sErr
				}
			}
			if err != nil {
				return nil, nil, fmt.Errorf("chain: sign covenant hash with fund key: %w", err)
			}
			sigFlag, err := contracts.CanonicalizeLowSDERWithFlag(der, sighash.AllForkID)
			if err != nil {
				return nil, nil, err
			}
			return sigFlag, comp, nil
		}
	}

	// 2. Match BRC-43 provenance signing key ("nektar provenance" / "default" / Self).
	if signPub, err := p.SigningPubKey(ctx); err == nil && signPub != nil {
		comp := signPub.Compressed()
		if bytes.Equal(core.Hash160(comp), targetPKH) {
			der, err := p.SignDigest(ctx, sighash32)
			if err != nil {
				return nil, nil, fmt.Errorf("chain: sign covenant hash with signing key: %w", err)
			}
			sigFlag, err := contracts.CanonicalizeLowSDERWithFlag(der, sighash.AllForkID)
			if err != nil {
				return nil, nil, err
			}
			return sigFlag, comp, nil
		}
	}

	// 3. Match embedded wallet root identity key (supports spending legacy NektarAnchor UTXOs whose OwnerPKH = Hash160(IdentityKey)).
	if p.engine != nil && p.engine.IdentityKey() != nil {
		comp := p.engine.IdentityKey().Compressed()
		if bytes.Equal(core.Hash160(comp), targetPKH) {
			der, err := p.engine.SignIdentityHash(sighash32)
			if err != nil {
				return nil, nil, fmt.Errorf("chain: sign covenant hash with identity key: %w", err)
			}
			sigFlag, err := contracts.CanonicalizeLowSDERWithFlag(der, sighash.AllForkID)
			if err != nil {
				return nil, nil, err
			}
			return sigFlag, comp, nil
		}
	}

	return nil, nil, fmt.Errorf("chain: no local wallet key matches covenant targetPKH %s", hex.EncodeToString(targetPKH))
}

// CovenantSpendRequest describes spending a Rúnar covenant UTXO (Input 0) with optional
// automatic BRC-100 wallet fuel + change handling and mandatory Chronicle Script VM verification.
type CovenantSpendRequest struct {
	InputTx               *transaction.Transaction
	InputBEEF             []byte
	InputVout             uint32
	Outputs               []*transaction.TransactionOutput
	LockTime              uint32
	Sequence              *uint32 // nil => 0xfffffffe if LockTime > 0 else 0xffffffff
	UnlockingScriptLength uint32  // fee-sizing hint when FuelFromWallet == true
	FuelFromWallet        bool    // true => add wallet BRC-29 fuel + change via CreateAction/SignAction; false => self-funded 1-input tx
	Description           string
	Labels                []string
	// BuildUnlock is invoked with the assembled transaction (including wallet fuel inputs & change output if any)
	// and must return Input 0's complete unlocking script.
	BuildUnlock func(tx *transaction.Transaction, changePKH []byte, changeSats uint64) (*script.Script, error)
}

// CovenantSpendResult holds the Chronicle-verified signed covenant spending transaction and AtomicBEEF.
type CovenantSpendResult struct {
	Tx               *transaction.Transaction
	BEEF             []byte
	FeeSats          uint64
	SpendSats        uint64
	ChangePKH        []byte
	ChangeSats       uint64
	AlreadyBroadcast bool
}

// BuildCovenantSpendTx builds, unlocks, and verifies (via post-Chronicle Bitcoin Script VM)
// a transaction spending a Rúnar covenant UTXO as Input 0.
// When FuelFromWallet is true, it uses BRC-100 CreateAction(SignAndProcess:false) + SignAction(NoSend:true)
// so wallet UTXOs are atomically reserved and released on any failure, and Broadcast() submits through SpendGuard.
func (p *Port) BuildCovenantSpendTx(ctx context.Context, req CovenantSpendRequest) (*CovenantSpendResult, error) {
	if req.InputTx == nil {
		return nil, errors.New("chain: BuildCovenantSpendTx requires InputTx")
	}
	if int(req.InputVout) >= len(req.InputTx.Outputs) || req.InputTx.Outputs[req.InputVout] == nil || req.InputTx.Outputs[req.InputVout].LockingScript == nil {
		return nil, fmt.Errorf("chain: BuildCovenantSpendTx invalid input vout %d", req.InputVout)
	}
	if len(req.Outputs) == 0 {
		return nil, errors.New("chain: BuildCovenantSpendTx requires at least one output")
	}
	if req.BuildUnlock == nil {
		return nil, errors.New("chain: BuildCovenantSpendTx requires BuildUnlock callback")
	}

	seq := uint32(0xffffffff)
	if req.LockTime > 0 {
		seq = 0xfffffffe
	}
	if req.Sequence != nil {
		seq = *req.Sequence
	}
	prevOut := req.InputTx.Outputs[req.InputVout]

	// Mode A: Self-funded 1-input covenant tx (DisclosureSale.Buy/Refund, NektarAgentVault.*).
	if !req.FuelFromWallet {
		tx := transaction.NewTransaction()
		tx.LockTime = req.LockTime
		tx.AddInput(&transaction.TransactionInput{
			SourceTXID:        req.InputTx.TxID(),
			SourceTxOutIndex:  req.InputVout,
			SourceTransaction: req.InputTx,
			SequenceNumber:    seq,
		})
		var outTotal uint64
		for _, o := range req.Outputs {
			tx.AddOutput(o)
			outTotal += o.Satoshis
		}
		if prevOut.Satoshis < outTotal {
			return nil, fmt.Errorf("chain: self-funded covenant input %d < output sum %d", prevOut.Satoshis, outTotal)
		}
		unlock, err := req.BuildUnlock(tx, make([]byte, 20), 0)
		if err != nil {
			return nil, err
		}
		tx.Inputs[0].UnlockingScript = unlock
		if err := contracts.VerifyInputScriptChronicleCtx(ctx, tx, 0, prevOut); err != nil {
			return nil, err
		}
		beef, err := SafeAtomicBEEF(tx)
		if err != nil {
			return nil, fmt.Errorf("chain: encode covenant AtomicBEEF: %w", err)
		}
		fee := prevOut.Satoshis - outTotal
		return &CovenantSpendResult{
			Tx:        tx,
			BEEF:      beef,
			FeeSats:   fee,
			SpendSats: outTotal + fee,
		}, nil
	}

	// Mode B: Wallet-fueled multi-input covenant tx (NektarAnchor.Append / NektarAnchor.Rotate).
	if p.wallet == nil {
		return nil, errors.New("chain: wallet not initialized for fueled covenant spend")
	}
	inputBeef := req.InputBEEF
	if len(inputBeef) == 0 {
		var err error
		inputBeef, err = SafeAtomicBEEF(req.InputTx)
		if err != nil {
			return nil, fmt.Errorf("chain: encode InputBEEF for covenant spend: %w", err)
		}
	}
	unlockLen := req.UnlockingScriptLength
	if unlockLen == 0 {
		unlockLen = 5120
	}
	desc := req.Description
	if desc == "" {
		desc = "nektar covenant spend"
	}
	labels := req.Labels
	if len(labels) == 0 {
		labels = []string{"nektar", "covenant"}
	}
	wOuts := make([]sdkwallet.CreateActionOutput, len(req.Outputs))
	var outTotal uint64
	for i, o := range req.Outputs {
		if o == nil || o.LockingScript == nil {
			return nil, fmt.Errorf("chain: covenant output %d has nil locking script", i)
		}
		outTotal += o.Satoshis
		wOuts[i] = sdkwallet.CreateActionOutput{
			LockingScript:     append([]byte(nil), (*o.LockingScript)...),
			Satoshis:          o.Satoshis,
			OutputDescription: desc,
		}
	}

	signAndProc := false
	randomize := false
	noSend := p.engine != nil
	acceptDelayed := false
	var lockTimePtr *uint32
	if req.LockTime > 0 {
		lt := req.LockTime
		lockTimePtr = &lt
	}
	created, err := p.wallet.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: desc,
		Labels:      labels,
		InputBEEF:   inputBeef,
		Inputs: []sdkwallet.CreateActionInput{{
			Outpoint:              transaction.Outpoint{Txid: *req.InputTx.TxID(), Index: req.InputVout},
			InputDescription:      desc,
			UnlockingScriptLength: unlockLen,
			SequenceNumber:        &seq,
		}},
		Outputs:  wOuts,
		LockTime: lockTimePtr,
		Options: &sdkwallet.CreateActionOptions{
			SignAndProcess:         &signAndProc,
			NoSend:                 &noSend,
			AcceptDelayedBroadcast: &acceptDelayed,
			RandomizeOutputs:       &randomize,
		},
	}, Originator)
	if err != nil {
		return nil, fmt.Errorf("chain: covenant CreateAction: %w", err)
	}
	if created == nil || created.SignableTransaction == nil {
		return nil, errors.New("chain: covenant CreateAction did not return SignableTransaction")
	}
	abortAction := func() {
		_, _ = p.wallet.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, Originator)
	}

	signableTx, err := transaction.NewTransactionFromBEEF(created.SignableTransaction.Tx)
	if err != nil {
		abortAction()
		return nil, fmt.Errorf("chain: parse signable covenant tx: %w", err)
	}
	if len(signableTx.Inputs) == 0 {
		abortAction()
		return nil, errors.New("chain: signable covenant tx has no inputs")
	}
	// Ensure Input 0 has its SourceTransaction attached for preimage & Script VM execution.
	if signableTx.Inputs[0].SourceTransaction == nil {
		signableTx.Inputs[0].SourceTransaction = req.InputTx
	}

	// Extract optional wallet change output placed after explicit outputs (RandomizeOutputs=false).
	changePKH := make([]byte, 20)
	var changeSats uint64
	extraOuts := len(signableTx.Outputs) - len(req.Outputs)
	if extraOuts > 1 {
		abortAction()
		return nil, fmt.Errorf("chain: covenant CreateAction produced %d change outputs (stateful covenants support at most 1)", extraOuts)
	}
	if extraOuts == 1 {
		chgOut := signableTx.Outputs[len(req.Outputs)]
		if chgOut == nil || chgOut.LockingScript == nil {
			abortAction()
			return nil, errors.New("chain: covenant change output missing locking script")
		}
		pkh, ok := contracts.ExtractP2PKH(*chgOut.LockingScript)
		if !ok {
			abortAction()
			return nil, errors.New("chain: wallet change output is not standard P2PKH")
		}
		changePKH = pkh
		changeSats = chgOut.Satoshis
	}

	unlock, err := req.BuildUnlock(signableTx, changePKH, changeSats)
	if err != nil {
		abortAction()
		return nil, fmt.Errorf("chain: build covenant unlock: %w", err)
	}
	signableTx.Inputs[0].UnlockingScript = unlock

	// Pre-sign fail-closed Chronicle Script VM gate on Input 0 BEFORE finalizing SignAction!
	if err := contracts.VerifyInputScriptChronicleCtx(ctx, signableTx, 0, prevOut); err != nil {
		abortAction()
		return nil, fmt.Errorf("chain: pre-broadcast Chronicle Script VM rejected covenant spend: %w", err)
	}

	signed, err := p.wallet.SignAction(ctx, sdkwallet.SignActionArgs{
		Reference: created.SignableTransaction.Reference,
		Spends: map[uint32]sdkwallet.SignActionSpend{
			0: {
				UnlockingScript: append([]byte(nil), (*unlock)...),
				SequenceNumber:  &seq,
			},
		},
		Options: &sdkwallet.SignActionOptions{
			NoSend:                 &noSend,
			AcceptDelayedBroadcast: &acceptDelayed,
		},
	}, Originator)
	if err != nil {
		abortAction()
		return nil, fmt.Errorf("chain: covenant SignAction: %w", err)
	}

	txid := signed.Txid.String()
	beef := signed.Tx
	var finalTx *transaction.Transaction
	if len(beef) > 0 {
		_, finalTx, _, err = guard.ParseBEEF(beef, guard.DefaultBound)
		if err != nil {
			if p.engine != nil {
				_ = p.engine.AbortNoSendByTxID(ctx, txid)
			}
			return nil, fmt.Errorf("chain: parse signed covenant AtomicBEEF: %w", err)
		}
	}
	if finalTx == nil && p.engine != nil {
		finalTx, err = p.engine.LoadParsedTx(ctx, txid)
		if err != nil {
			_ = p.engine.AbortNoSendByTxID(ctx, txid)
			return nil, err
		}
	}
	if finalTx == nil {
		return nil, fmt.Errorf("chain: SignAction did not return parseable tx for %s", txid)
	}
	if len(beef) == 0 {
		beef, err = SafeAtomicBEEF(finalTx)
		if err != nil {
			if p.engine != nil {
				_ = p.engine.AbortNoSendByTxID(ctx, txid)
			}
			return nil, err
		}
	}

	feeSats := computeParsedTxFee(finalTx)
	if feeSats == 0 && p.engine != nil {
		feeSats = p.engine.TxFeeSats(ctx, txid)
	}
	if p.engine != nil {
		p.mu.Lock()
		p.pendingNoSend[txid] = txid
		p.mu.Unlock()
	} else {
		p.rememberClientTxBytes(txid, beef, finalTx)
	}

	return &CovenantSpendResult{
		Tx:               finalTx,
		BEEF:             beef,
		FeeSats:          feeSats,
		SpendSats:        outTotal + feeSats,
		ChangePKH:        changePKH,
		ChangeSats:       changeSats,
		AlreadyBroadcast: !noSend,
	}, nil
}

// SubmitCovenantTx submits an already-verified self-funded covenant transaction (such as
// DisclosureSale.Buy, DisclosureSale.Refund, or NektarAgentVault.AgentSpend/OperatorControl/OperatorSweep)
// to testchain (memory mode) or the configured live settler, attaches any instant BUMP proof,
// and caches the tx in the local wallet engine without double-counting SpendGuard on incoming claims/refunds.
func (p *Port) SubmitCovenantTx(ctx context.Context, tx *transaction.Transaction) (string, error) {
	if tx == nil {
		return "", errors.New("chain: nil covenant transaction")
	}
	txid := tx.TxID().String()
	if p.testChain != nil {
		p.tcMu.Lock()
		// Check whether tx uses BIP-113 nLockTime with non-final nSequence OR spends a prior
		// memory-mode locktime-final covenant tx (bcommon/testchain naively rejects ALL
		// LockTime!=0 && Sequence!=MaxTxInSequenceNum even when LockTime <= tipHeight!).
		hasLocktime := false
		spendsMemCov := false
		for _, in := range tx.Inputs {
			if in == nil || in.SourceTXID == nil {
				p.tcMu.Unlock()
				return "", errors.New("chain: covenant tx has nil input")
			}
			if tx.LockTime != 0 && in.SequenceNumber != transaction.MaxTxInSequenceNum {
				hasLocktime = true
			}
			if _, ok := p.memCovenantTxs[in.SourceTXID.String()]; ok {
				spendsMemCov = true
			}
		}

		if !hasLocktime && !spendsMemCov {
			err := p.testChain.Send(tx)
			if err == nil {
				if mp, _, ok := p.testChain.Proof(txid); ok && mp != nil {
					tx.MerklePath = mp
				}
			}
			p.tcMu.Unlock()
			if err != nil {
				return "", fmt.Errorf("chain: testchain covenant submit: %w", err)
			}
			if p.engine != nil {
				p.engine.CacheTx(tx)
			}
			return txid, nil
		}

		// Memory-mode locktime-final covenant path: enforce real Bitcoin consensus IsFinalTx,
		// UTXO existence + double-spend checks, AND full post-Chronicle Bitcoin Script VM!
		if _, dup := p.memCovenantTxs[txid]; dup {
			p.tcMu.Unlock()
			return txid, nil
		}
		if hasLocktime {
			if tx.LockTime < 500_000_000 {
				tip := p.testChain.Height()
				if tx.LockTime > tip {
					p.tcMu.Unlock()
					return "", fmt.Errorf("chain: locktime height %d not final at chain tip %d", tx.LockTime, tip)
				}
			} else if int64(tx.LockTime) > time.Now().Unix() {
				p.tcMu.Unlock()
				return "", fmt.Errorf("chain: locktime timestamp %d not final", tx.LockTime)
			}
		}
		prevOuts := make([]*transaction.TransactionOutput, len(tx.Inputs))
		outpoints := make([]string, len(tx.Inputs))
		for i, in := range tx.Inputs {
			parentID := in.SourceTXID.String()
			opKey := fmt.Sprintf("%s:%d", parentID, in.SourceTxOutIndex)
			if spender, spent := p.memCovenantSpent[opKey]; spent {
				p.tcMu.Unlock()
				return "", fmt.Errorf("chain: double-spend of %s (already spent by %s)", opKey, spender)
			}
			parent := p.memCovenantTxs[parentID]
			if parent == nil {
				parent = p.testChain.Tx(parentID)
			}
			if parent == nil || int(in.SourceTxOutIndex) >= len(parent.Outputs) || parent.Outputs[in.SourceTxOutIndex] == nil {
				p.tcMu.Unlock()
				return "", fmt.Errorf("chain: missing parent output %s", opKey)
			}
			prevOuts[i] = parent.Outputs[in.SourceTxOutIndex]
			outpoints[i] = opKey
		}
		for i, prevOut := range prevOuts {
			if err := contracts.VerifyInputScriptChronicleCtx(ctx, tx, i, prevOut); err != nil {
				p.tcMu.Unlock()
				return "", fmt.Errorf("chain: covenant script verification failed: %w", err)
			}
		}
		for _, opKey := range outpoints {
			p.memCovenantSpent[opKey] = txid
		}
		p.memCovenantTxs[txid] = tx
		p.tcMu.Unlock()
		if p.engine != nil {
			p.engine.CacheTx(tx)
		}
		return txid, nil
	}
	if p.settler == nil {
		return "", errors.New("chain: no live settler configured for covenant tx")
	}
	if err := p.settler.Submit(ctx, tx); err != nil {
		return "", fmt.Errorf("chain: %s covenant submit: %w", p.settler.Name(), err)
	}
	if p.engine != nil {
		p.engine.CacheTx(tx)
	}
	return txid, nil
}

// FindSpendingTx looks up any known on-chain transaction spending outpoint (txid:vout),
// returning (spendTx, inputIdx, true) when found (used to extract revealed keys from
// DisclosureSale.Buy transactions even if the seller vanished off HTTP/bbox).
func (p *Port) FindSpendingTx(_ context.Context, txid string, vout uint32) (*transaction.Transaction, int, bool) {
	if p == nil || len(txid) != 64 {
		return nil, -1, false
	}
	if p.testChain != nil {
		p.tcMu.Lock()
		defer p.tcMu.Unlock()
		opKey := fmt.Sprintf("%s:%d", txid, vout)
		if spendID, ok := p.memCovenantSpent[opKey]; ok {
			if tx := p.memCovenantTxs[spendID]; tx != nil {
				for vin, in := range tx.Inputs {
					if in != nil && in.SourceTXID != nil && in.SourceTXID.String() == txid && in.SourceTxOutIndex == vout {
						return tx, vin, true
					}
				}
			}
		}
		for _, id := range p.testChain.Txids() {
			tx := p.testChain.Tx(id)
			if tx == nil {
				continue
			}
			for vin, in := range tx.Inputs {
				if in != nil && in.SourceTXID != nil && in.SourceTXID.String() == txid && in.SourceTxOutIndex == vout {
					return tx, vin, true
				}
			}
		}
	}
	return nil, -1, false
}
