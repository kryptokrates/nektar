package chain

import (
	"context"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"

	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

// VerifyPaymentBEEF verifies an incoming counterparty payment BEEF against this Port's witnessed
// ChainTracker, settleDepth, and ZeroConfWatch policy.
func (p *Port) VerifyPaymentBEEF(ctx context.Context, beefBytes []byte, opts PaymentVerifyOpts) (*PaymentVerifyResult, error) {
	tracker := p.tracker
	if tracker == nil && p.engine != nil {
		tracker = p.engine.Tracker()
	}
	if opts.SettleDepth <= 0 {
		opts.SettleDepth = p.settleDepth
	}
	if opts.MaxUnconfirmedAncestors <= 0 {
		opts.MaxUnconfirmedAncestors = p.maxUnconfAnc
	}
	if opts.ConflictWatch == nil {
		opts.ConflictWatch = p.zeroConfWatch
	}
	return VerifyCounterpartyPaymentBEEF(ctx, beefBytes, tracker, opts)
}

// Height returns the witnessed chain tip height (never delegating trust to a remote client wallet).
func (p *Port) Height(ctx context.Context) (uint32, error) {
	if p.testChain != nil {
		p.tcMu.Lock()
		defer p.tcMu.Unlock()
		return p.testChain.Height(), nil
	}
	if p.tracker == nil {
		return 0, nwallet.ErrNoChain
	}
	return p.tracker.CurrentHeight(ctx)
}

// ProofChecked returns the witnessed-header-verified MerklePath + height if txid is mined,
// returning explicit errors on tracker/upstream outages instead of collapsing outages to false.
func (p *Port) ProofChecked(ctx context.Context, txid string) (*transaction.MerklePath, uint32, bool, error) {
	if p.testChain != nil {
		p.tcMu.Lock()
		defer p.tcMu.Unlock()
		mp, h, ok := p.testChain.Proof(txid)
		return mp, h, ok, nil
	}
	if p.tracker == nil {
		return nil, 0, false, fmt.Errorf("%w: no chain tracker configured", ErrTrackerUnavailable)
	}
	txHash, err := chainhash.NewHashFromHex(txid)
	if err != nil {
		return nil, 0, false, fmt.Errorf("chain: invalid txid %q: %w", txid, err)
	}
	verifyProof := func(mp *transaction.MerklePath, h uint32) (bool, error) {
		if mp == nil {
			return false, nil
		}
		if h == 0 {
			h = mp.BlockHeight
		}
		root, rErr := mp.ComputeRoot(txHash)
		if rErr != nil || root == nil {
			return false, nil
		}
		valid, vErr := p.tracker.IsValidRootForHeight(ctx, root, h)
		if vErr != nil {
			return false, fmt.Errorf("%w: %w", ErrTrackerUnavailable, vErr)
		}
		return valid, nil
	}

	// 1. Check locally held verified proof FIRST (avoid unnecessary network refetch).
	if p.engine != nil {
		if tx, lErr := p.engine.LoadParsedTx(ctx, txid); lErr == nil && tx != nil && tx.MerklePath != nil {
			valid, vErr := verifyProof(tx.MerklePath, tx.MerklePath.BlockHeight)
			if vErr != nil {
				return nil, 0, false, vErr
			}
			if valid {
				return tx.MerklePath, tx.MerklePath.BlockHeight, true, nil
			}
		}
	}

	var lastErr error
	// 2. Ask Arcade, then nodeChain (every proof verified against independent witnessed headers).
	if p.arcade != nil {
		mp, h, aErr := p.arcade.Proof(ctx, txid)
		if aErr == nil && mp != nil {
			valid, vErr := verifyProof(mp, h)
			if vErr != nil {
				return nil, 0, false, vErr
			}
			if valid {
				if h == 0 {
					h = mp.BlockHeight
				}
				return mp, h, true, nil
			}
		} else if aErr != nil && !errors.Is(aErr, nodeapi.ErrNotMined) && !errors.Is(aErr, publish.ErrArcadeUnknown) && !errors.Is(aErr, publish.ErrArcadeRefused) {
			lastErr = aErr
		}
	}
	if p.nodeChain != nil {
		mp, h, cErr := p.nodeChain.Proof(ctx, txid)
		if cErr == nil && mp != nil {
			valid, vErr := verifyProof(mp, h)
			if vErr != nil {
				return nil, 0, false, vErr
			}
			if valid {
				if h == 0 {
					h = mp.BlockHeight
				}
				return mp, h, true, nil
			}
		} else if cErr != nil && !errors.Is(cErr, nodeapi.ErrNotMined) {
			lastErr = cErr
		}
	}
	return nil, 0, false, lastErr
}

// Proof returns the header-verified MerklePath + height if txid is mined.
func (p *Port) Proof(ctx context.Context, txid string) (*transaction.MerklePath, uint32, bool) {
	mp, h, ok, _ := p.ProofChecked(ctx, txid)
	return mp, h, ok
}

// TxBroadcastStatus returns verified mining proof AND durable local wallet status for txid,
// enabling crash recovery (ReconcilePendingAnchors) to distinguish unbroadcast NoSend txs from live broadcasts.
func (p *Port) TxBroadcastStatus(ctx context.Context, txid string) (mined bool, walletStatus string, mp *transaction.MerklePath, height uint32) {
	mp, height, mined, _ = p.ProofChecked(ctx, txid)
	if p.engine != nil {
		if st, ok := p.engine.TxStatus(ctx, txid); ok {
			walletStatus = st
		}
	} else if mined {
		walletStatus = "completed"
	} else if p.Tx(txid) != nil {
		walletStatus = "unproven"
	}
	return mined, walletStatus, mp, height
}

// Tx returns a known transaction by txid (from testchain, bounded SQLite LRU cache, or durable client cache).
func (p *Port) Tx(txid string) *transaction.Transaction {
	if p.testChain != nil {
		p.tcMu.Lock()
		tx := p.testChain.Tx(txid)
		if tx == nil {
			tx = p.memCovenantTxs[txid]
		}
		p.tcMu.Unlock()
		if tx != nil {
			return tx
		}
	}
	if p.engine != nil {
		if tx, err := p.engine.LoadParsedTx(context.Background(), txid); err == nil && tx != nil {
			return tx
		}
	}
	return p.loadClientTx(txid)
}

// SignDigest signs a 32-byte digest under Nektar's BRC-43 identity key using wallet.Interface.
func (p *Port) SignDigest(ctx context.Context, digest []byte) ([]byte, error) {
	res, err := p.signer.CreateSignature(ctx, sdkwallet.CreateSignatureArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{
			ProtocolID: sdkwallet.Protocol{SecurityLevel: sdkwallet.SecurityLevelEveryApp, Protocol: "nektar provenance"},
			KeyID:      "default",
			Counterparty: sdkwallet.Counterparty{
				Type: sdkwallet.CounterpartyTypeSelf,
			},
		},
		HashToDirectlySign: digest,
	}, Originator)
	if err != nil {
		return nil, err
	}
	return res.Signature.Serialize(), nil
}

// SigningPubKey returns the public key corresponding to SignDigest.
func (p *Port) SigningPubKey(ctx context.Context) (*ec.PublicKey, error) {
	res, err := p.signer.GetPublicKey(ctx, sdkwallet.GetPublicKeyArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{
			ProtocolID: sdkwallet.Protocol{SecurityLevel: sdkwallet.SecurityLevelEveryApp, Protocol: "nektar provenance"},
			KeyID:      "default",
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

// EncryptAtRest wraps sensitive local secrets (e.g. Borg epoch keys, market signing key)
// using the node's BRC-43 wallet key (CounterpartyTypeSelf) so raw secrets are never stored in plaintext SQLite.
func (p *Port) EncryptAtRest(ctx context.Context, keyID string, plaintext []byte) ([]byte, error) {
	if p == nil || p.wallet == nil {
		return nil, errors.New("chain: wallet unavailable for EncryptAtRest")
	}
	res, err := p.wallet.Encrypt(ctx, sdkwallet.EncryptArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{
			ProtocolID: sdkwallet.Protocol{SecurityLevel: sdkwallet.SecurityLevelEveryApp, Protocol: "nektar storage"},
			KeyID:      keyID,
			Counterparty: sdkwallet.Counterparty{
				Type: sdkwallet.CounterpartyTypeSelf,
			},
		},
		Plaintext: plaintext,
	}, Originator)
	if err != nil {
		return nil, err
	}
	return res.Ciphertext, nil
}

// DecryptAtRest unwraps a ciphertext created by EncryptAtRest.
func (p *Port) DecryptAtRest(ctx context.Context, keyID string, ciphertext []byte) ([]byte, error) {
	if p == nil || p.wallet == nil {
		return nil, errors.New("chain: wallet unavailable for DecryptAtRest")
	}
	res, err := p.wallet.Decrypt(ctx, sdkwallet.DecryptArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{
			ProtocolID: sdkwallet.Protocol{SecurityLevel: sdkwallet.SecurityLevelEveryApp, Protocol: "nektar storage"},
			KeyID:      keyID,
			Counterparty: sdkwallet.Counterparty{
				Type: sdkwallet.CounterpartyTypeSelf,
			},
		},
		Ciphertext: ciphertext,
	}, Originator)
	if err != nil {
		return nil, err
	}
	return res.Plaintext, nil
}
