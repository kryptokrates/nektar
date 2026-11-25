package wallet

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/wirewallet"
)

const DefaultDesktopWireURL = "http://127.0.0.1:3321"

// dialWireWallet can be overridden in unit tests to simulate crashes between CreateAction and InternalizeAction.
var dialWireWallet = func(originator, url string, timeout time.Duration) (sdkwallet.Interface, error) {
	return wirewallet.Dial(originator, url, timeout)
}

// DesktopFundResult summarizes a 1-click BRC-29 funding transfer from BSV Desktop into Nektar.
type DesktopFundResult struct {
	TxID             string `json:"txid"`
	Satoshis         uint64 `json:"satoshis"`
	DesktopIdentity  string `json:"desktopIdentity"`
	NektarIdentity   string `json:"nektarIdentity"`
	DerivationPrefix string `json:"derivationPrefix"`
	DerivationSuffix string `json:"derivationSuffix"`
	Internalized     bool   `json:"internalized"`
}

// DesktopSweepResult summarizes a 1-click BRC-29 sweep from Nektar back into BSV Desktop.
type DesktopSweepResult struct {
	TxIDs           []string `json:"txids"`
	SweptSats       uint64   `json:"sweptSats"`
	TotalFeeSats    uint64   `json:"totalFeeSats"`
	InputsSwept     int      `json:"inputsSwept"`
	DesktopIdentity string   `json:"desktopIdentity"`
	Internalized    int      `json:"internalized"`
}

// FundFromWireWallet dials a local BRC-100 wallet (BSV Desktop / Metanet Desktop / external go-wallet-toolbox)
// over loopback binary wire, requests one BRC-29 payment output to target's identity key, and immediately
// internalizes the resulting AtomicBEEF into target so the balance is verified and spendable on arrival.
func FundFromWireWallet(ctx context.Context, wireURL string, amountSats uint64, target *Engine) (*DesktopFundResult, error) {
	if target == nil {
		return nil, fmt.Errorf("%w: target embedded wallet is required", ErrInvalidArgs)
	}
	if amountSats == 0 {
		return nil, fmt.Errorf("%w: amountSats must be > 0", ErrInvalidArgs)
	}
	if wireURL == "" {
		wireURL = DefaultDesktopWireURL
	}
	if err := wirewallet.CheckURL(wireURL); err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: %w", err)
	}
	desktop, err := dialWireWallet(target.cfg.Originator, wireURL, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: dial %s: %w", wireURL, err)
	}

	// Refuse any mainnet/testnet mismatch before requesting a spend!
	if err := checkMatchingNetwork(ctx, desktop, target.cfg.Mainnet, target.cfg.Originator); err != nil {
		return nil, err
	}
	desktopID, err := wirewallet.IdentityKeyOf(ctx, desktop, target.cfg.Originator)
	if err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: get desktop identity: %w", err)
	}
	desktopHex := hex.EncodeToString(desktopID.Compressed())

	// Generate fresh BRC-29 derivation nonce pair and ask desktop wallet to derive recipient's payment pubkey.
	var prefixRaw, suffixRaw [16]byte
	if _, err := rand.Read(prefixRaw[:]); err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: crypto/rand failed: %w", err)
	}
	if _, err := rand.Read(suffixRaw[:]); err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: crypto/rand failed: %w", err)
	}
	prefixB64 := base64.StdEncoding.EncodeToString(prefixRaw[:])
	suffixB64 := base64.StdEncoding.EncodeToString(suffixRaw[:])

	forSelf := false
	pubRes, err := desktop.GetPublicKey(ctx, sdkwallet.GetPublicKeyArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{
			ProtocolID: bwallet.PaymentProtocol,
			KeyID:      bwallet.PaymentKeyID(prefixB64, suffixB64),
			Counterparty: sdkwallet.Counterparty{
				Type:         sdkwallet.CounterpartyTypeOther,
				Counterparty: target.IdentityKey(),
			},
		},
		ForSelf: &forSelf,
	}, target.cfg.Originator)
	if err != nil || pubRes == nil || pubRes.PublicKey == nil {
		return nil, fmt.Errorf("wallet desktop bridge: derive BRC-29 payment key: %w", err)
	}
	addr, err := script.NewAddressFromPublicKey(pubRes.PublicKey, target.cfg.Mainnet)
	if err != nil {
		return nil, err
	}
	lockScript, err := p2pkh.Lock(addr)
	if err != nil {
		return nil, err
	}

	// Persist (prefix, suffix, counterparty, lockingScript) BEFORE desktop.CreateAction so a crash
	// between broadcast and InternalizeAction can always recover the funds on restart!
	pendingID, err := target.SavePendingBRC29(ctx, PendingBRC29Record{
		Direction:        "fund",
		WireURL:          wireURL,
		CounterpartyPub:  desktopHex,
		DerivationPrefix: prefixB64,
		DerivationSuffix: suffixB64,
		LockingScriptHex: hex.EncodeToString(*lockScript),
		Satoshis:         amountSats,
	})
	if err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: journal pending BRC-29 fund: %w", err)
	}

	// Ask BSV Desktop to fund, sign, and broadcast (AcceptDelayedBroadcast=false so AtomicBEEF is live).
	acceptDelayed := false
	randomize := false
	actRes, err := desktop.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: fmt.Sprintf("Fund Nektar agent wallet (%d sats)", amountSats),
		Labels:      []string{"nektar", "fund"},
		Outputs: []sdkwallet.CreateActionOutput{{
			LockingScript:     *lockScript,
			Satoshis:          amountSats,
			OutputDescription: "BRC-29 funding payment to Nektar",
		}},
		Options: &sdkwallet.CreateActionOptions{
			AcceptDelayedBroadcast: &acceptDelayed,
			RandomizeOutputs:       &randomize,
		},
	}, target.cfg.Originator)
	if err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: desktop CreateAction: %w", err)
	}
	if len(actRes.Tx) == 0 {
		return nil, fmt.Errorf("wallet desktop bridge: desktop CreateAction returned empty AtomicBEEF (txid %s)", actRes.Txid.String())
	}

	// Locate exact output index matching our derived locking script (handles wallets that shuffle outputs).
	outIdx := uint32(0)
	if _, parsedTx, _, pErr := guard.ParseBEEF(actRes.Tx, guard.DefaultBound); pErr == nil && parsedTx != nil {
		for idx, o := range parsedTx.Outputs {
			if o.LockingScript != nil && string(*o.LockingScript) == string(*lockScript) && o.Satoshis == amountSats {
				outIdx = uint32(idx)
				break
			}
		}
	}
	_ = target.UpdatePendingBRC29Tx(ctx, pendingID, actRes.Txid.String(), outIdx, actRes.Tx, amountSats)

	// Immediately internalize AtomicBEEF + BRC-29 remittance into target Engine!
	intRes, err := target.InternalizeAction(ctx, sdkwallet.InternalizeActionArgs{
		Tx:          actRes.Tx,
		Description: fmt.Sprintf("Funded %d sats from BSV Desktop", amountSats),
		Labels:      []string{"desktop-fund", "brc29"},
		Outputs: []sdkwallet.InternalizeOutput{{
			OutputIndex: outIdx,
			Protocol:    sdkwallet.InternalizeProtocolWalletPayment,
			PaymentRemittance: &sdkwallet.Payment{
				DerivationPrefix:  prefixRaw[:],
				DerivationSuffix:  suffixRaw[:],
				SenderIdentityKey: desktopID,
			},
		}},
	}, target.cfg.Originator)
	if err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: internalize into Nektar (txid %s): %w", actRes.Txid.String(), err)
	}
	_ = target.MarkPendingBRC29Done(ctx, pendingID)

	return &DesktopFundResult{
		TxID:             actRes.Txid.String(),
		Satoshis:         amountSats,
		DesktopIdentity:  desktopHex,
		NektarIdentity:   target.IdentityPubHex(),
		DerivationPrefix: prefixB64,
		DerivationSuffix: suffixB64,
		Internalized:     intRes != nil && intRes.Accepted,
	}, nil
}

// SweepToWireWallet sweeps 100% of spendable funds from source Engine to fresh BRC-29 keys
// owned by the local BSV Desktop / BRC-100 wallet and calls desktop.InternalizeAction for every chunk
// so the swept funds appear instantly in the user's desktop wallet!
func SweepToWireWallet(ctx context.Context, wireURL string, source *Engine, overrideGuard bool) (*DesktopSweepResult, error) {
	if source == nil {
		return nil, fmt.Errorf("%w: source embedded wallet is required", ErrInvalidArgs)
	}
	if wireURL == "" {
		wireURL = DefaultDesktopWireURL
	}
	if err := wirewallet.CheckURL(wireURL); err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: %w", err)
	}
	desktop, err := dialWireWallet(source.cfg.Originator, wireURL, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: dial %s: %w", wireURL, err)
	}
	if err := checkMatchingNetwork(ctx, desktop, source.cfg.Mainnet, source.cfg.Originator); err != nil {
		return nil, err
	}
	desktopID, err := wirewallet.IdentityKeyOf(ctx, desktop, source.cfg.Originator)
	if err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: get desktop identity: %w", err)
	}
	desktopHex := hex.EncodeToString(desktopID.Compressed())

	sweepRes, err := source.Sweep(ctx, SweepArgs{
		ToIdentityKey: desktopHex,
		WireURL:       wireURL,
		Description:   "Sweep Nektar wallet to BSV Desktop",
		MaxPerTx:      DefaultSweepChunkInputs,
		OverrideGuard: overrideGuard,
	})
	if err != nil {
		return nil, err
	}

	out := &DesktopSweepResult{
		TxIDs:           sweepRes.TxIDs,
		SweptSats:       sweepRes.SweptSats,
		TotalFeeSats:    sweepRes.TotalFeeSats,
		InputsSwept:     sweepRes.InputsSwept,
		DesktopIdentity: desktopHex,
	}
	for i, rem := range sweepRes.Remittances {
		if i >= len(sweepRes.BEEFs) {
			break
		}
		preRaw, err1 := base64.StdEncoding.DecodeString(rem.DerivationPrefix)
		sufRaw, err2 := base64.StdEncoding.DecodeString(rem.DerivationSuffix)
		if err1 != nil || err2 != nil {
			continue
		}
		intRes, iErr := desktop.InternalizeAction(ctx, sdkwallet.InternalizeActionArgs{
			Tx:          sweepRes.BEEFs[i],
			Description: "Swept funds from Nektar agent wallet",
			Labels:      []string{"nektar", "sweep"},
			Outputs: []sdkwallet.InternalizeOutput{{
				OutputIndex: rem.OutputIndex,
				Protocol:    sdkwallet.InternalizeProtocolWalletPayment,
				PaymentRemittance: &sdkwallet.Payment{
					DerivationPrefix:  preRaw,
					DerivationSuffix:  sufRaw,
					SenderIdentityKey: source.IdentityKey(),
				},
			}},
		}, source.cfg.Originator)
		if iErr != nil {
			return out, fmt.Errorf("wallet desktop bridge: desktop InternalizeAction for sweep %s: %w", rem.TxID, iErr)
		}
		_ = source.MarkPendingBRC29DoneByTx(ctx, rem.TxID, rem.OutputIndex)
		if intRes != nil && intRes.Accepted {
			out.Internalized++
		}
	}
	return out, nil
}

// ReconcilePendingBRC29 replays any persisted pending BRC-29 desktop fund/sweep transfers
// that were interrupted by a crash or transient network failure between broadcast and internalization.
func ReconcilePendingBRC29(ctx context.Context, e *Engine, defaultWireURL string) (int, error) {
	if e == nil {
		return 0, nil
	}
	pending, err := e.ListPendingBRC29(ctx)
	if err != nil || len(pending) == 0 {
		return 0, err
	}
	reconciled := 0
	var firstErr error
	for _, rec := range pending {
		preRaw, err1 := base64.StdEncoding.DecodeString(rec.DerivationPrefix)
		sufRaw, err2 := base64.StdEncoding.DecodeString(rec.DerivationSuffix)
		if err1 != nil || err2 != nil {
			continue
		}
		switch rec.Direction {
		case "fund":
			beef := rec.AtomicBEEF
			outIdx := rec.OutputIndex
			txID := rec.TxID
			// Recover pre-UpdatePendingBRC29Tx crash by scanning local/chain outputs matching LockingScriptHex.
			if len(beef) == 0 && txID == "" && rec.LockingScriptHex != "" {
				if foundTxID, foundVout, foundBeef := e.findTxByLockingScript(ctx, rec.LockingScriptHex); foundTxID != "" {
					txID = foundTxID
					outIdx = foundVout
					beef = foundBeef
					_ = e.UpdatePendingBRC29Tx(ctx, rec.ID, txID, outIdx, beef, rec.Satoshis)
				}
			}
			if len(beef) == 0 && txID != "" {
				if tx, tErr := e.LoadParsedTx(ctx, txID); tErr == nil && tx != nil {
					beef, _ = SafeAtomicBEEF(tx)
				} else if bObj, bErr := e.svc.GetBEEF(ctx, txID, nil); bErr == nil && bObj != nil {
					if tx := bObj.FindTransaction(txID); tx != nil {
						beef, _ = SafeAtomicBEEF(tx)
					}
				}
			}
			if len(beef) == 0 {
				continue
			}
			// Skip cleanly if output is already internalized in wallet storage.
			if txID != "" {
				if existing, cErr := e.provider.OutputsEntity().Read().UserID().Equals(e.userID).TxID().Equals(txID).Vout().Equals(outIdx).Count(ctx); cErr == nil && existing > 0 {
					_ = e.MarkPendingBRC29Done(ctx, rec.ID)
					reconciled++
					continue
				}
			}
			senderPub, pErr := guard.ParsePubKeyHex(rec.CounterpartyPub)
			if pErr != nil {
				if firstErr == nil {
					firstErr = pErr
				}
				continue
			}
			intRes, iErr := e.InternalizeAction(ctx, sdkwallet.InternalizeActionArgs{
				Tx:          beef,
				Description: "Recovered BRC-29 funding payment from BSV Desktop",
				Labels:      []string{"desktop-fund", "brc29", "recovered"},
				Outputs: []sdkwallet.InternalizeOutput{{
					OutputIndex: outIdx,
					Protocol:    sdkwallet.InternalizeProtocolWalletPayment,
					PaymentRemittance: &sdkwallet.Payment{
						DerivationPrefix:  preRaw,
						DerivationSuffix:  sufRaw,
						SenderIdentityKey: senderPub,
					},
				}},
			}, e.cfg.Originator)
			if iErr != nil || intRes == nil || !intRes.Accepted {
				if iErr == nil {
					iErr = errors.New("wallet: InternalizeAction declined BRC-29 fund recovery")
				}
				if firstErr == nil {
					firstErr = iErr
				}
				continue
			}
			_ = e.MarkPendingBRC29Done(ctx, rec.ID)
			reconciled++

		case "sweep":
			beef := rec.AtomicBEEF
			outIdx := rec.OutputIndex
			txID := rec.TxID
			// Recover pre-UpdatePendingBRC29Tx crash by scanning wallet.sqlite outputs matching LockingScriptHex.
			if len(beef) == 0 && txID == "" && rec.LockingScriptHex != "" {
				if foundTxID, foundVout, foundBeef := e.findTxByLockingScript(ctx, rec.LockingScriptHex); foundTxID != "" {
					txID = foundTxID
					outIdx = foundVout
					beef = foundBeef
					_ = e.UpdatePendingBRC29Tx(ctx, rec.ID, txID, outIdx, beef, rec.Satoshis)
				}
			}
			if len(beef) == 0 && txID != "" {
				if tx, tErr := e.LoadParsedTx(ctx, txID); tErr == nil && tx != nil {
					beef, _ = SafeAtomicBEEF(tx)
				}
			}
			if len(beef) == 0 {
				continue
			}
			wURL := rec.WireURL
			if wURL == "" {
				wURL = defaultWireURL
			}
			if wURL == "" {
				wURL = DefaultDesktopWireURL
			}
			if cErr := wirewallet.CheckURL(wURL); cErr != nil {
				if firstErr == nil {
					firstErr = cErr
				}
				continue
			}
			desktop, dErr := dialWireWallet(e.cfg.Originator, wURL, 15*time.Second)
			if dErr != nil {
				if firstErr == nil {
					firstErr = dErr
				}
				continue
			}
			intRes, iErr := desktop.InternalizeAction(ctx, sdkwallet.InternalizeActionArgs{
				Tx:          beef,
				Description: "Recovered swept funds from Nektar agent wallet",
				Labels:      []string{"nektar", "sweep", "recovered"},
				Outputs: []sdkwallet.InternalizeOutput{{
					OutputIndex: outIdx,
					Protocol:    sdkwallet.InternalizeProtocolWalletPayment,
					PaymentRemittance: &sdkwallet.Payment{
						DerivationPrefix:  preRaw,
						DerivationSuffix:  sufRaw,
						SenderIdentityKey: e.IdentityKey(),
					},
				}},
			}, e.cfg.Originator)
			if iErr != nil || intRes == nil || !intRes.Accepted {
				if iErr == nil {
					iErr = errors.New("wallet: remote desktop declined BRC-29 sweep internalization")
				}
				if firstErr == nil {
					firstErr = iErr
				}
				continue
			}
			_ = e.MarkPendingBRC29Done(ctx, rec.ID)
			reconciled++
		}
	}
	return reconciled, firstErr
}

func checkMatchingNetwork(ctx context.Context, remote sdkwallet.Interface, localMainnet bool, originator string) error {
	netRes, err := remote.GetNetwork(ctx, nil, originator)
	if err != nil {
		return fmt.Errorf("wallet desktop bridge: get remote network: %w", err)
	}
	want := sdkwallet.NetworkTestnet
	if localMainnet {
		want = sdkwallet.NetworkMainnet
	}
	if netRes == nil || netRes.Network != want {
		got := sdkwallet.Network("")
		if netRes != nil {
			got = netRes.Network
		}
		return fmt.Errorf("%w: Nektar is on %q, desktop wallet is on %q", ErrNetworkMismatch, want, got)
	}
	return nil
}
