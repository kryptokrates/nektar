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
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
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
	if p.wallet == nil {
		return nwallet.BalanceBreakdown{}, errors.New("chain: client wallet not initialized")
	}
	limit := uint32(1000)
	res, err := p.wallet.ListOutputs(ctx, sdkwallet.ListOutputsArgs{
		Basket:  nwallet.BasketDefault,
		Include: sdkwallet.OutputIncludeEntireTransactions,
		Limit:   &limit,
	}, Originator)
	if err != nil {
		// Some remote wallets refuse entire transactions on large baskets; retry metadata-only and
		// report all spendable outputs honestly as UnprovenSats (never ConfirmedSats).
		res, err = p.wallet.ListOutputs(ctx, sdkwallet.ListOutputsArgs{
			Basket: nwallet.BasketDefault,
			Limit:  &limit,
		}, Originator)
		if err != nil {
			return nwallet.BalanceBreakdown{}, err
		}
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

// UTXOCountChecked returns total spendable UTXOs and propagates any DB/RPC/timeout error.
func (p *Port) UTXOCountChecked(ctx context.Context) (int, error) {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	bal, err := p.Balance(ctx)
	if err != nil {
		return 0, err
	}
	return bal.SpendableCount, nil
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

// UTXOCount returns total spendable UTXOs (best-effort; prefer UTXOCountChecked when errors matter).
func (p *Port) UTXOCount() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cnt, err := p.UTXOCountChecked(ctx)
	if err != nil {
		return 0
	}
	return cnt
}

// EnsureSplitCoins warms up the fuel basket via FuelKeeper (embedded mode; no-op in client mode).
func (p *Port) EnsureSplitCoins(ctx context.Context, targetCount int, splitSats uint64) error {
	if p.engine == nil || targetCount <= 1 || splitSats == 0 {
		return nil
	}
	_, err := p.engine.FuelKeeper().WarmFuel(ctx, targetCount, splitSats)
	return err
}

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
		res, sErr := p.clientCreateAction(ctx, []*transaction.TransactionOutput{{Satoshis: args.Satoshis, LockingScript: lockScript}}, args.Description, args.OverrideGuard)
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
	return p.clientCreateAction(ctx, []*transaction.TransactionOutput{{Satoshis: args.Satoshis, LockingScript: lockScript}}, args.Description, args.OverrideGuard)
}

func (p *Port) clientCreateAction(ctx context.Context, outputs []*transaction.TransactionOutput, desc string, overrideGuard bool) (*nwallet.SendResult, error) {
	var outTotal uint64
	for _, o := range outputs {
		outTotal += o.Satoshis
	}
	estFee := uint64(200)
	if f, fErr := p.feeSource.Fees(ctx); fErr == nil {
		if ef, eErr := f.For(250 + len(outputs)*34); eErr == nil {
			estFee = ef
		}
	}
	var resID string
	if !overrideGuard {
		// Atomically reserve daily SpendGuard budget BEFORE asking remote BRC-100 wallet to broadcast.
		var rErr error
		resID, rErr = p.guard.Reserve(estFee, outTotal+estFee)
		if rErr != nil {
			return nil, rErr
		}
	}
	if desc == "" {
		desc = "nektar client wallet action"
	}
	wOuts := make([]sdkwallet.CreateActionOutput, len(outputs))
	for i, o := range outputs {
		wOuts[i] = sdkwallet.CreateActionOutput{
			LockingScript:     append([]byte(nil), (*o.LockingScript)...),
			Satoshis:          o.Satoshis,
			OutputDescription: desc,
		}
	}
	acceptDelayed := false
	randomize := false
	actRes, err := p.wallet.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: desc,
		Labels:      []string{"nektar"},
		Outputs:     wOuts,
		Options: &sdkwallet.CreateActionOptions{
			AcceptDelayedBroadcast: &acceptDelayed,
			RandomizeOutputs:       &randomize,
		},
	}, Originator)
	if err != nil {
		lower := strings.ToLower(err.Error())
		uncertain := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
			strings.Contains(lower, "timeout") || strings.Contains(lower, "eof") ||
			strings.Contains(lower, "connection reset") || strings.Contains(lower, "http 5") ||
			strings.Contains(lower, "status 5")
		if uncertain {
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
	actualFee := estFee
	if len(actRes.Tx) > 0 {
		_, parsedTx, _, parseErr = guard.ParseBEEF(actRes.Tx, guard.DefaultBound)
		p.rememberClientTxBytes(txid, actRes.Tx, parsedTx)
	} else {
		p.rememberClientTxBytes(txid, nil, nil)
	}
	if parsedTx != nil {
		if f := computeParsedTxFee(parsedTx); f > 0 {
			actualFee = f
		}
	}
	if !overrideGuard {
		// Unconditionally commit real broadcast spend so spentToday never under-counts.
		if cErr := p.guard.CommitReservation(resID, txid, actualFee, outTotal+actualFee); cErr != nil {
			return nil, fmt.Errorf("chain: persist SpendGuard record for broadcast tx %s: %w", txid, cErr)
		}
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

func computeParsedTxFee(tx *transaction.Transaction) uint64 {
	if tx == nil {
		return 0
	}
	var inTotal, outTotal uint64
	for _, in := range tx.Inputs {
		if in.SourceTransaction == nil || int(in.SourceTxOutIndex) >= len(in.SourceTransaction.Outputs) {
			return 0
		}
		inTotal += in.SourceTransaction.Outputs[in.SourceTxOutIndex].Satoshis
	}
	for _, o := range tx.Outputs {
		outTotal += o.Satoshis
	}
	if inTotal > outTotal {
		return inTotal - outTotal
	}
	return 0
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
	res, err := p.Send(ctx, nwallet.SendArgs{
		ToAddress:   address,
		Satoshis:    sats,
		Description: "nektar payment",
	})
	if err != nil {
		return nil, "", err
	}
	return res.Tx, res.TxID, nil
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
