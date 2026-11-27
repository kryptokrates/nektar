package chain

import (
	"context"
	"errors"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/guard"

	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func (p *Port) FundAddress() (string, error) {
	if p.engine != nil {
		return p.engine.FundAddress(), nil
	}
	return p.signer.FundAddress(p.mainnet)
}

// FeeRateSatPerKB returns the current fee rate in sat/kB (live Arcade policy or static fallback).
func (p *Port) FeeRateSatPerKB(ctx context.Context) uint64 {
	if p.engine != nil {
		return p.engine.FeeRateSatPerKB(ctx)
	}
	if p.feeSource != nil {
		fCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if f, err := p.feeSource.Fees(fCtx); err == nil && !f.Rate.IsZero() && f.Rate.Bytes > 0 {
			return (f.Rate.Sats*1000 + f.Rate.Bytes - 1) / f.Rate.Bytes
		}
	}
	return 100
}

// Balance returns structured confirmed / unproven / reserved / per-basket telemetry in both embedded and client modes.
func (p *Port) Balance(ctx context.Context) (nwallet.BalanceBreakdown, error) {
	if p.engine != nil {
		return p.engine.Balance(ctx)
	}
	// Client mode: request full transaction BEEF via ListOutputs so ConfirmedSats ONLY counts
	// outputs backed by a BUMP verified against our witnessed ChainTracker (never trusting bare spendable flags).
	// Note: BSV Desktop forbids third-party apps from listing its internal "default" basket (400 Bad Request),
	// so fall back gracefully to Nektar's app basket ("nektar funding").
	if p.wallet == nil {
		return nwallet.BalanceBreakdown{}, errors.New("chain: client wallet not initialized")
	}
	limit := uint32(1000)
	var res *sdkwallet.ListOutputsResult
	var err error
	for _, basket := range []string{nwallet.BasketDefault, NektarWalletProfile.FundBasket} {
		res, err = p.wallet.ListOutputs(ctx, sdkwallet.ListOutputsArgs{
			Basket:  basket,
			Include: sdkwallet.OutputIncludeEntireTransactions,
			Limit:   &limit,
		}, Originator)
		if err == nil {
			break
		}
		// Some remote wallets refuse entire transactions on large baskets; retry metadata-only and
		// report all spendable outputs honestly as UnprovenSats (never ConfirmedSats).
		res, err = p.wallet.ListOutputs(ctx, sdkwallet.ListOutputsArgs{
			Basket: basket,
			Limit:  &limit,
		}, Originator)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nwallet.BalanceBreakdown{}, err
	}
	var beef *transaction.Beef
	if len(res.BEEF) > 0 {
		if parsedBeef, _, _, pErr := guard.ParseBEEF(res.BEEF, guard.DefaultBound); pErr == nil {
			beef = parsedBeef
		}
	}
	verifiedMined := make(map[string]bool)
	isTxConfirmed := func(txid string) bool {
		if txid == "" || beef == nil || p.tracker == nil {
			return false
		}
		if v, ok := verifiedMined[txid]; ok {
			return v
		}
		tx := beef.FindTransaction(txid)
		if tx == nil || tx.MerklePath == nil {
			verifiedMined[txid] = false
			return false
		}
		txHash, hErr := chainhash.NewHashFromHex(txid)
		if hErr != nil {
			verifiedMined[txid] = false
			return false
		}
		root, rErr := tx.MerklePath.ComputeRoot(txHash)
		if rErr != nil || root == nil {
			verifiedMined[txid] = false
			return false
		}
		valid, vErr := p.tracker.IsValidRootForHeight(ctx, root, tx.MerklePath.BlockHeight)
		ok := vErr == nil && valid
		verifiedMined[txid] = ok
		return ok
	}

	var b nwallet.BalanceBreakdown
	for _, o := range res.Outputs {
		b.UTXOCount++
		b.TotalSats += o.Satoshis
		if o.Spendable {
			b.SpendableCount++
			b.SpendableSats += o.Satoshis
			if isTxConfirmed(o.Outpoint.Txid.String()) {
				b.ConfirmedSats += o.Satoshis
			} else {
				b.UnprovenSats += o.Satoshis
			}
			b.DefaultCount++
			b.DefaultSats += o.Satoshis
		} else {
			b.ReservedSats += o.Satoshis
		}
	}
	return b, nil
}

// BalanceChecked returns total spendable satoshis and propagates any DB/RPC/timeout error
// distinctly from a true zero balance.
func (p *Port) BalanceChecked(ctx context.Context) (uint64, error) {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	bal, err := p.Balance(ctx)
	if err != nil {
		return 0, err
	}
	return bal.SpendableSats, nil
}

// BalanceSats returns total spendable satoshis (best-effort; prefer BalanceChecked when errors matter).
func (p *Port) BalanceSats() uint64 {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sats, err := p.BalanceChecked(ctx)
	if err != nil {
		return 0
	}
	return sats
}

// UTXOCount returns total spendable UTXOs (best-effort).
func (p *Port) UTXOCount() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bal, err := p.Balance(ctx)
	if err != nil {
		return 0
	}
	return bal.SpendableCount
}

// Sweep sweeps 100% of spendable funds in embedded mode to a P2PKH address or BRC-100 identity key.
func (p *Port) Sweep(ctx context.Context, args nwallet.SweepArgs) (*nwallet.SweepResult, error) {
	if p.engine == nil {
		return nil, errors.New("chain: sweep is supported in embedded wallet mode (manage funds directly in BSV Desktop in client mode)")
	}
	return p.engine.Sweep(ctx, args)
}

// Consolidate merges fragmented small UTXOs into clean reserve outputs (embedded mode).
func (p *Port) Consolidate(ctx context.Context, args nwallet.ConsolidateArgs) (*nwallet.ConsolidateResult, error) {
	if p.engine == nil {
		return nil, errors.New("chain: consolidate is supported in embedded wallet mode")
	}
	return p.engine.Consolidate(ctx, args)
}

// ImportBEEF verifies and imports a BEEF/AtomicBEEF payment into the embedded wallet.
func (p *Port) ImportBEEF(ctx context.Context, beefBytes []byte) (*nwallet.ImportResult, error) {
	if p.engine == nil {
		return nil, errors.New("chain: import BEEF is supported in embedded wallet mode")
	}
	return p.engine.ImportBEEF(ctx, beefBytes)
}

// ImportTxID fetches, SPV-verifies, and imports a mined txid into the embedded wallet.
func (p *Port) ImportTxID(ctx context.Context, txid string) (*nwallet.ImportResult, error) {
	if p.engine == nil {
		return nil, errors.New("chain: import txid is supported in embedded wallet mode")
	}
	return p.engine.ImportTxID(ctx, txid)
}

// FundFromDesktop performs 1-click BRC-29 funding from local BSV Desktop / BRC-100 wire into Nektar.
func (p *Port) FundFromDesktop(ctx context.Context, wireURL string, sats uint64) (*nwallet.DesktopFundResult, error) {
	if p.engine == nil {
		return nil, errors.New("chain: fund-from-desktop is for embedded wallet mode (already using client wallet)")
	}
	if wireURL == "" {
		wireURL = p.wireURL
	}
	return nwallet.FundFromWireWallet(ctx, wireURL, sats, p.engine)
}

// SweepToDesktop sweeps 100% of spendable funds from Nektar into local BSV Desktop via BRC-29 + InternalizeAction.
func (p *Port) SweepToDesktop(ctx context.Context, wireURL string, overrideGuard bool) (*nwallet.DesktopSweepResult, error) {
	if p.engine == nil {
		return nil, errors.New("chain: sweep-to-desktop is for embedded wallet mode")
	}
	if wireURL == "" {
		wireURL = p.wireURL
	}
	return nwallet.SweepToWireWallet(ctx, wireURL, p.engine, overrideGuard)
}
