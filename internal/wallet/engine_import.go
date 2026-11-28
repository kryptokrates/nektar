package wallet

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/entity"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/guard"
)

// -----------------------------------------------------------------------------
// Coin Ingestion, Legacy Migration, Pruning & Strict AtomicBEEF Encoding
// -----------------------------------------------------------------------------

// migrateLegacyPool imports existing bcommon/bwallet wallet.json or nektar-pool.json UTXOs
// into go-wallet-toolbox's "nektar funding" basket once and renames the legacy file to .migrated.
func (e *Engine) migrateLegacyPool(ctx context.Context, walletDir, legacyFile string) (int, error) {
	if walletDir == "" {
		return 0, nil
	}
	candidates := []string{filepath.Join(walletDir, "wallet.json")}
	if legacyFile != "" && legacyFile != "wallet.json" {
		candidates = append(candidates, filepath.Join(walletDir, legacyFile))
	}
	var poolPath string
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			poolPath = c
			break
		}
	}
	if poolPath == "" {
		return 0, nil
	}

	// Only migrate when wallet has no existing outputs so restarts never duplicate coins.
	existing, err := e.provider.OutputsEntity().Read().UserID().Equals(e.userID).Count(ctx)
	if err != nil || existing > 0 {
		return 0, err
	}

	pool, err := bwallet.LoadPool(poolPath)
	if err != nil {
		return 0, fmt.Errorf("load %s: %w", poolPath, err)
	}
	outs := pool.Outputs()
	if len(outs) == 0 {
		_ = os.Rename(poolPath, poolPath+".migrated")
		return 0, nil
	}

	beefDir := filepath.Join(walletDir, "txbeef")
	imported := 0
	for _, o := range outs {
		if o.TxID == "" || o.Satoshis == 0 {
			continue
		}
		var tx *transaction.Transaction
		var atomicBeef []byte

		if isHex64(o.TxID) {
			if rawBeef, rErr := os.ReadFile(filepath.Join(beefDir, o.TxID+".beef")); rErr == nil && len(rawBeef) > 0 {
				if _, parsed, _, pErr := guard.ParseBEEF(rawBeef, guard.DefaultBound); pErr == nil && parsed != nil {
					tx = parsed
					atomicBeef = rawBeef
				}
			}
		}
		if tx == nil && o.Raw != "" {
			if parsed, pErr := transaction.NewTransactionFromHex(strings.TrimSpace(o.Raw)); pErr == nil {
				tx = parsed
			}
		}
		if tx == nil {
			continue
		}
		if o.Bump != "" && tx.MerklePath == nil {
			if bumpBytes, bErr := hex.DecodeString(strings.TrimSpace(o.Bump)); bErr == nil {
				if mp, mpErr := transaction.NewMerklePathFromBinary(bumpBytes); mpErr == nil {
					tx.MerklePath = mp
				}
			}
		}
		if len(atomicBeef) == 0 {
			var aErr error
			atomicBeef, aErr = SafeAtomicBEEF(tx)
			if aErr != nil {
				continue
			}
		}
		_, iErr := e.InternalizeAction(WithIncomingInternalize(ctx), sdkwallet.InternalizeActionArgs{
			Tx:          atomicBeef,
			Description: "migrated from legacy wallet.json",
			Labels:      []string{"migrated"},
			Outputs: []sdkwallet.InternalizeOutput{{
				OutputIndex: o.Vout,
				Protocol:    sdkwallet.InternalizeProtocolBasketInsertion,
				InsertionRemittance: &sdkwallet.BasketInsertion{
					Basket: BasketFunding,
					Tags:   []string{"migrated", "fund"},
				},
			}},
		}, e.cfg.Originator)
		if iErr != nil {
			e.logf("wallet: migrate output %s:%d skipped: %v", o.TxID, o.Vout, iErr)
			continue
		}
		e.txCache.Put(o.TxID, tx)
		imported++
	}
	_ = os.Rename(poolPath, poolPath+".migrated")
	return imported, nil
}

// ImportBEEF verifies a BEEF/AtomicBEEF payload and internalizes any outputs paying FundAddress().
func (e *Engine) ImportBEEF(ctx context.Context, beefBytes []byte) (*ImportResult, error) {
	if len(beefBytes) == 0 {
		return nil, fmt.Errorf("%w: empty BEEF bytes", ErrInvalidArgs)
	}
	_, tx, _, err := guard.ParseBEEF(beefBytes, guard.DefaultBound)
	if err != nil || tx == nil {
		return nil, fmt.Errorf("wallet: parse BEEF: %v", err)
	}
	return e.importTxToFundingBasket(ctx, tx, beefBytes, "imported BEEF payment")
}

// ImportTxID fetches a transaction and its complete ancestor BEEF (back to mined header-verified
// BUMPs within MaxUnprovenDepth), SPV-verifies it, and internalizes outputs paying FundAddress().
func (e *Engine) ImportTxID(ctx context.Context, txid string) (*ImportResult, error) {
	txid = strings.TrimSpace(txid)
	if len(txid) != 64 {
		return nil, fmt.Errorf("%w: txid must be 64 hex chars", ErrInvalidArgs)
	}
	beef, err := e.svc.GetBEEF(ctx, txid, nil)
	if err != nil || beef == nil {
		return nil, fmt.Errorf("wallet: assemble complete ancestor BEEF for %s: %w", txid, err)
	}
	tx := beef.FindTransaction(txid)
	if tx == nil {
		return nil, fmt.Errorf("wallet: tx %s missing from assembled BEEF", txid)
	}
	hydrateInputs(tx, beef, e.txCache, e.svc.dbLoader(ctx), 0, e.cfg.MaxUnprovenDepth)
	atomicBeef, err := SafeAtomicBEEF(tx)
	if err != nil {
		return nil, fmt.Errorf("wallet: encode strict AtomicBEEF for %s: %w", txid, err)
	}
	return e.importTxToFundingBasket(ctx, tx, atomicBeef, "imported txid payment")
}

// AdoptReceivedTx credits any outputs in a verified counterparty payment tx paying FundAddress().
func (e *Engine) AdoptReceivedTx(ctx context.Context, tx *transaction.Transaction) (uint64, error) {
	if tx == nil {
		return 0, nil
	}
	atomicBeef, err := SafeAtomicBEEF(tx)
	if err != nil {
		return 0, err
	}
	res, err := e.importTxToFundingBasket(ctx, tx, atomicBeef, "adopted received payment")
	if err != nil {
		return 0, err
	}
	return res.Sats, nil
}

// SeedCoinbaseOutput records a mature coinbase output (paying FundAddress) into go-wallet-toolbox storage.
func (e *Engine) SeedCoinbaseOutput(tx *transaction.Transaction, vout uint32, mp *transaction.MerklePath, height uint32) error {
	if tx == nil || int(vout) >= len(tx.Outputs) {
		return fmt.Errorf("%w: invalid coinbase output", ErrInvalidArgs)
	}
	if mp != nil {
		tx.MerklePath = mp
	}
	atomicBeef, err := SafeAtomicBEEF(tx)
	if err != nil {
		return err
	}
	txid := tx.TxID().String()
	e.txCache.Put(txid, tx)

	// Avoid duplicate internalization if already stored.
	if existing, fErr := e.provider.OutputsEntity().Read().UserID().Equals(e.userID).TxID().Equals(txid).Vout().Equals(vout).Count(context.Background()); fErr == nil && existing > 0 {
		return nil
	}

	_, err = e.tb.InternalizeAction(context.Background(), sdkwallet.InternalizeActionArgs{
		Tx:          atomicBeef,
		Description: "seeded mature coinbase output",
		Labels:      []string{"coinbase", "seed"},
		Outputs: []sdkwallet.InternalizeOutput{{
			OutputIndex: vout,
			Protocol:    sdkwallet.InternalizeProtocolBasketInsertion,
			InsertionRemittance: &sdkwallet.BasketInsertion{
				Basket: BasketFunding,
				Tags:   []string{"coinbase", "fund"},
			},
		}},
	}, e.cfg.Originator)
	return err
}

func (e *Engine) importTxToFundingBasket(ctx context.Context, tx *transaction.Transaction, atomicBeef []byte, desc string) (*ImportResult, error) {
	txid := tx.TxID().String()
	stripe := &e.importStripes[txShardIdx(txid)]
	stripe.Lock()
	defer stripe.Unlock()
	fundBytes := []byte(*e.fundScript)
	var outputs []sdkwallet.InternalizeOutput
	var totalSats uint64
	for idx, o := range tx.Outputs {
		if o.LockingScript == nil || o.Satoshis == 0 {
			continue
		}
		if string(*o.LockingScript) == string(fundBytes) {
			// Guard against resurrecting outputs this wallet already knows or already spent!
			// go-wallet-toolbox BasketInsertion runs GORM Save which would overwrite spendable=1, spent_by=NULL.
			if e.HasOutput(ctx, txid, uint32(idx)) {
				continue
			}
			outputs = append(outputs, sdkwallet.InternalizeOutput{
				OutputIndex: uint32(idx),
				Protocol:    sdkwallet.InternalizeProtocolBasketInsertion,
				InsertionRemittance: &sdkwallet.BasketInsertion{
					Basket: BasketFunding,
					Tags:   []string{"fund"},
				},
			})
			totalSats += o.Satoshis
		}
	}
	if len(outputs) == 0 {
		return &ImportResult{TxID: txid}, nil
	}
	if len(atomicBeef) == 0 {
		var err error
		atomicBeef, err = SafeAtomicBEEF(tx)
		if err != nil {
			return nil, err
		}
	}
	// Route through Engine.InternalizeAction so full go-sdk/spv.Verify (coinbase maturity 100,
	// finality, overspend, scripts), ancestor depth cap, WithIncomingInternalize, and fuel.Wake run!
	_, err := e.InternalizeAction(WithIncomingInternalize(ctx), sdkwallet.InternalizeActionArgs{
		Tx:          atomicBeef,
		Description: ensureDesc(desc, "nektar import payment"),
		Labels:      []string{"import"},
		Outputs:     outputs,
	}, e.cfg.Originator)
	if err != nil {
		return nil, err
	}
	mined := tx.MerklePath != nil
	var height uint32
	if mined {
		height = tx.MerklePath.BlockHeight
	}
	return &ImportResult{
		TxID:    txid,
		Outputs: len(outputs),
		Sats:    totalSats,
		Mined:   mined,
		Height:  height,
	}, nil
}

// PruneUnrecognizedTxs marks any persisted outputs whose txid is not in keep as unspendable
// (used when re-seeding an ephemeral testchain).
func (e *Engine) PruneUnrecognizedTxs(ctx context.Context, keep func(txid string) bool) error {
	outs, err := e.provider.OutputsEntity().Read().UserID().Equals(e.userID).Spendable().Equals(true).Find(ctx)
	if err != nil {
		return err
	}
	f := false
	for _, o := range outs {
		if o.TxID == nil || *o.TxID == "" || keep(*o.TxID) {
			continue
		}
		_ = e.provider.OutputsEntity().Update(ctx, &entity.OutputUpdateSpecification{
			ID:        o.ID,
			Spendable: &f,
		})
		e.txCache.Delete(*o.TxID)
	}
	return nil
}

// SafeAtomicBEEF encodes tx strictly as BRC-95 AtomicBEEF (allowPartial=false).
// Never silently falls back to allowPartial=true (which drops missing parent ancestry).
func SafeAtomicBEEF(tx *transaction.Transaction) ([]byte, error) {
	if tx == nil {
		return nil, errors.New("wallet: nil transaction")
	}
	b, err := tx.AtomicBEEF(false)
	if err != nil {
		return nil, fmt.Errorf("wallet: strict AtomicBEEF encode failed for %s: %w", tx.TxID().String(), err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("wallet: empty AtomicBEEF for %s", tx.TxID().String())
	}
	return b, nil
}
