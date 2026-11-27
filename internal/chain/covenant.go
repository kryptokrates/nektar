package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
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
	if p.broadcaster == nil {
		return "", errors.New("chain: no live broadcaster configured for covenant tx")
	}
	if err := p.broadcaster.Broadcast(ctx, tx); err != nil {
		return "", fmt.Errorf("chain: %s covenant broadcast: %w", p.broadcaster.Name(), err)
	}
	if p.engine != nil {
		p.engine.CacheTx(tx)
	}
	return txid, nil
}

// FindSpendingTx looks up any known on-chain transaction spending outpoint (txid:vout),
// returning (spendTx, inputIdx, true, nil) when found and verified against raw tx bytes + input linkage,
// (nil, -1, false, nil) when unspent, or an explicit error on provider outage (never collapsing outage to unspent).
func (p *Port) FindSpendingTx(ctx context.Context, txid string, vout uint32) (*transaction.Transaction, int, bool, error) {
	if p == nil || len(txid) != 64 {
		return nil, -1, false, nil
	}
	if p.testChain != nil {
		p.tcMu.Lock()
		defer p.tcMu.Unlock()
		opKey := fmt.Sprintf("%s:%d", txid, vout)
		if spendID, ok := p.memCovenantSpent[opKey]; ok {
			if tx := p.memCovenantTxs[spendID]; tx != nil {
				for vin, in := range tx.Inputs {
					if in != nil && in.SourceTXID != nil && in.SourceTXID.String() == txid && in.SourceTxOutIndex == vout {
						return tx, vin, true, nil
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
					return tx, vin, true, nil
				}
			}
		}
		return nil, -1, false, nil
	}
	if p.nodeChain == nil {
		return nil, -1, false, nwallet.ErrNoChain
	}
	spenderTxID, err := p.nodeChain.Spender(ctx, txid, vout)
	if err != nil {
		return nil, -1, false, err
	}
	if spenderTxID == "" {
		return nil, -1, false, nil
	}
	raw, err := p.nodeChain.TxRaw(ctx, spenderTxID)
	if err != nil {
		return nil, -1, false, err
	}
	if len(raw) == 0 {
		return nil, -1, false, fmt.Errorf("chain: empty raw tx returned for spender %s", spenderTxID)
	}
	spendTx, err := transaction.NewTransactionFromBytes(raw)
	if err != nil {
		return nil, -1, false, fmt.Errorf("chain: parse spender tx %s: %w", spenderTxID, err)
	}
	if spendTx.TxID().String() != spenderTxID {
		return nil, -1, false, fmt.Errorf("chain: spender txid mismatch: got %s want %s", spendTx.TxID().String(), spenderTxID)
	}
	for vin, in := range spendTx.Inputs {
		if in != nil && in.SourceTXID != nil && in.SourceTXID.String() == txid && in.SourceTxOutIndex == vout {
			return spendTx, vin, true, nil
		}
	}
	return nil, -1, false, fmt.Errorf("chain: spender tx %s does not spend %s:%d", spenderTxID, txid, vout)
}
