package wallet

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/wirewallet"
)

const maxPendingBRC29PerPass = 10

func (e *Engine) shouldAttemptBRC29(id int64, now time.Time) bool {
	if e == nil {
		return false
	}
	e.brc29BackoffMu.Lock()
	defer e.brc29BackoffMu.Unlock()
	ent, ok := e.brc29Backoff[id]
	if !ok {
		return true
	}
	return !now.Before(ent.nextTry)
}

func (e *Engine) recordBRC29Backoff(id int64, now time.Time) {
	if e == nil {
		return
	}
	e.brc29BackoffMu.Lock()
	defer e.brc29BackoffMu.Unlock()
	if e.brc29Backoff == nil {
		e.brc29Backoff = make(map[int64]unfailBackoffEntry)
	}
	if len(e.brc29Backoff) > 512 {
		clear(e.brc29Backoff)
	}
	ent := e.brc29Backoff[id]
	ent.attempts++
	shift := min(ent.attempts-1, 5)
	wait := time.Duration(30<<shift) * time.Second
	if wait > 15*time.Minute {
		wait = 15 * time.Minute
	}
	ent.nextTry = now.Add(wait)
	e.brc29Backoff[id] = ent
}

func (e *Engine) clearBRC29Backoff(id int64) {
	if e == nil {
		return
	}
	e.brc29BackoffMu.Lock()
	delete(e.brc29Backoff, id)
	e.brc29BackoffMu.Unlock()
}

// findFundTxOnWoCByScript queries WhatsOnChain /script/{scriptHash}/unspent/all using the
// ElectrumX byte-reversed SHA-256 scriptHash to recover a crashed BSV Desktop fund txid.
func (e *Engine) findFundTxOnWoCByScript(ctx context.Context, lockScriptHex string) (string, uint32) {
	if e == nil || e.cfg.WoC == nil || lockScriptHex == "" {
		return "", 0
	}
	rawScript, err := hex.DecodeString(strings.TrimSpace(lockScriptHex))
	if err != nil || len(rawScript) == 0 {
		return "", 0
	}
	// ElectrumX / WhatsOnChain scriptHash is single SHA-256 displayed in reversed byte order.
	scriptHash := chainhash.HashH(rawScript).String()
	netSeg := "test"
	if e.cfg.Mainnet {
		netSeg = "main"
	}
	if e.cfg.WoC.Network != "" {
		netSeg = e.cfg.WoC.Network
	}
	base := strings.TrimRight(e.cfg.WoC.Base, "/")
	if base == "" {
		base = fmt.Sprintf("https://api.whatsonchain.com/v1/bsv/%s", netSeg)
	}
	urlStr := fmt.Sprintf("%s/script/%s/unspent/all", base, scriptHash)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return "", 0
	}
	if e.cfg.WoC.Key != "" {
		req.Header.Set("Authorization", e.cfg.WoC.Key)
	}
	cli := e.cfg.WoC.Client
	if cli == nil {
		cli = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return "", 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0
	}
	var parsed wocUnspentResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&parsed); err != nil {
		return "", 0
	}
	for _, u := range parsed.Result {
		if u.TxHash != "" && u.Value > 0 {
			return u.TxHash, u.TxPos
		}
	}
	return "", 0
}

// findFundTxOnDesktopByScript queries BSV Desktop ListActions for recent fund actions matching
// lockScriptHex when a crash occurred after desktop.CreateAction broadcast and before UpdatePendingBRC29Tx.
func (e *Engine) findFundTxOnDesktopByScript(ctx context.Context, wireURL, lockScriptHex string) (string, uint32) {
	if e == nil || lockScriptHex == "" || wirewallet.CheckURL(wireURL) != nil {
		return "", 0
	}
	wantBytes, err := hex.DecodeString(strings.TrimSpace(lockScriptHex))
	if err != nil || len(wantBytes) == 0 {
		return "", 0
	}
	desktop, err := dialWireWallet(e.cfg.Originator, wireURL, 5*time.Second)
	if err != nil {
		return "", 0
	}
	incOuts := true
	lim := uint32(50)
	res, err := desktop.ListActions(ctx, sdkwallet.ListActionsArgs{
		Labels:                      []string{"nektar", "fund"},
		IncludeOutputs:              &incOuts,
		IncludeOutputLockingScripts: &incOuts,
		Limit:                       &lim,
	}, e.cfg.Originator)
	if err != nil || res == nil {
		return "", 0
	}
	for _, act := range res.Actions {
		txid := act.Txid.String()
		if txid == "" || act.Txid.IsEqual(&chainhash.Hash{}) {
			continue
		}
		for _, o := range act.Outputs {
			if len(o.LockingScript) == len(wantBytes) && string(o.LockingScript) == string(wantBytes) {
				return txid, o.OutputIndex
			}
		}
	}
	return "", 0
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
	attempted := 0
	now := time.Now()
	unreachableWire := make(map[string]error)
	var firstErr error
	recordErr := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	resolveWireURL := func(recWireURL string) string {
		wURL := recWireURL
		if wURL == "" {
			wURL = defaultWireURL
		}
		if wURL == "" {
			wURL = DefaultDesktopWireURL
		}
		return wURL
	}
	for _, rec := range pending {
		if ctx.Err() != nil {
			recordErr(ctx.Err())
			break
		}
		if attempted >= maxPendingBRC29PerPass {
			break
		}
		if !e.shouldAttemptBRC29(rec.ID, now) {
			continue
		}
		attempted++
		preRaw, err1 := base64.StdEncoding.DecodeString(rec.DerivationPrefix)
		sufRaw, err2 := base64.StdEncoding.DecodeString(rec.DerivationSuffix)
		if err1 != nil || err2 != nil {
			err := fmt.Errorf("wallet: pending BRC-29 %d has invalid derivation encoding", rec.ID)
			e.logf("%v", err)
			e.recordBRC29Backoff(rec.ID, now)
			recordErr(err)
			continue
		}
		switch rec.Direction {
		case "fund":
			beef := rec.AtomicBEEF
			outIdx := rec.OutputIndex
			txID := rec.TxID
			var beefErr error
			// Recover pre-UpdatePendingBRC29Tx crash by scanning local outputs, WoC scriptHash, or Desktop ListActions.
			if len(beef) == 0 && txID == "" && rec.LockingScriptHex != "" {
				if foundTxID, foundVout, foundBeef := e.findTxByLockingScript(ctx, rec.LockingScriptHex); foundTxID != "" {
					txID = foundTxID
					outIdx = foundVout
					beef = foundBeef
				}
				if txID == "" {
					if foundTxID, foundVout := e.findFundTxOnWoCByScript(ctx, rec.LockingScriptHex); foundTxID != "" {
						txID = foundTxID
						outIdx = foundVout
					}
				}
				if txID == "" {
					wURL := resolveWireURL(rec.WireURL)
					if _, offline := unreachableWire[wURL]; !offline {
						if foundTxID, foundVout := e.findFundTxOnDesktopByScript(ctx, wURL, rec.LockingScriptHex); foundTxID != "" {
							txID = foundTxID
							outIdx = foundVout
						}
					}
				}
				if txID != "" {
					if uErr := e.UpdatePendingBRC29Tx(ctx, rec.ID, txID, outIdx, beef, rec.Satoshis); uErr != nil {
						e.logf("wallet: update pending BRC-29 fund %d (%s): %v", rec.ID, txID, uErr)
						recordErr(uErr)
					}
				}
			}
			if len(beef) == 0 && txID != "" {
				if tx, tErr := e.LoadParsedTx(ctx, txID); tErr == nil && tx != nil {
					if encoded, sErr := SafeAtomicBEEF(tx); sErr == nil {
						beef = encoded
					} else {
						beefErr = fmt.Errorf("wallet: encode local AtomicBEEF for pending fund %s: %w", txID, sErr)
					}
				} else if tErr != nil {
					beefErr = tErr
				}
				if len(beef) == 0 {
					if bObj, bErr := e.svc.GetBEEF(ctx, txID, nil); bErr == nil && bObj != nil {
						if tx := bObj.FindTransaction(txID); tx != nil {
							encoded, sErr := SafeAtomicBEEF(tx)
							if sErr != nil {
								beefErr = fmt.Errorf("wallet: encode chain AtomicBEEF for pending fund %s: %w", txID, sErr)
							} else {
								beef = encoded
								if rec.LockingScriptHex != "" {
									if wantLS, hErr := hex.DecodeString(rec.LockingScriptHex); hErr == nil {
										for idx, o := range tx.Outputs {
											if o.LockingScript != nil && string(*o.LockingScript) == string(wantLS) {
												outIdx = uint32(idx)
												break
											}
										}
									}
								}
								if uErr := e.UpdatePendingBRC29Tx(ctx, rec.ID, txID, outIdx, beef, rec.Satoshis); uErr != nil {
									e.logf("wallet: update pending BRC-29 fund %d (%s): %v", rec.ID, txID, uErr)
									recordErr(uErr)
								}
							}
						}
					} else if bErr != nil && beefErr == nil {
						beefErr = bErr
					}
				}
			}
			if len(beef) == 0 {
				e.recordBRC29Backoff(rec.ID, now)
				if beefErr != nil {
					e.logf("wallet: pending BRC-29 fund %d (%s) missing AtomicBEEF: %v", rec.ID, txID, beefErr)
					recordErr(beefErr)
				} else if txID != "" {
					err := fmt.Errorf("wallet: pending BRC-29 fund %d (%s) AtomicBEEF unavailable", rec.ID, txID)
					e.logf("%v", err)
					recordErr(err)
				}
				continue
			}
			// Skip cleanly if output is already internalized in wallet storage.
			if txID != "" {
				if existing, cErr := e.provider.OutputsEntity().Read().UserID().Equals(e.userID).TxID().Equals(txID).Vout().Equals(outIdx).Count(ctx); cErr == nil && existing > 0 {
					if mErr := e.MarkPendingBRC29Done(ctx, rec.ID); mErr != nil {
						e.logf("wallet: mark pending BRC-29 fund %d done: %v", rec.ID, mErr)
						e.recordBRC29Backoff(rec.ID, now)
						recordErr(mErr)
						continue
					}
					e.clearBRC29Backoff(rec.ID)
					reconciled++
					continue
				}
			}
			senderPub, pErr := guard.ParsePubKeyHex(rec.CounterpartyPub)
			if pErr != nil {
				e.recordBRC29Backoff(rec.ID, now)
				recordErr(pErr)
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
				e.recordBRC29Backoff(rec.ID, now)
				recordErr(iErr)
				continue
			}
			if mErr := e.MarkPendingBRC29Done(ctx, rec.ID); mErr != nil {
				e.logf("wallet: mark pending BRC-29 fund %d done: %v", rec.ID, mErr)
				e.recordBRC29Backoff(rec.ID, now)
				recordErr(mErr)
				continue
			}
			e.clearBRC29Backoff(rec.ID)
			reconciled++

		case "sweep":
			beef := rec.AtomicBEEF
			outIdx := rec.OutputIndex
			txID := rec.TxID
			var beefErr error
			// Recover pre-UpdatePendingBRC29Tx crash by scanning wallet.sqlite outputs matching LockingScriptHex.
			if len(beef) == 0 && txID == "" && rec.LockingScriptHex != "" {
				if foundTxID, foundVout, foundBeef := e.findTxByLockingScript(ctx, rec.LockingScriptHex); foundTxID != "" {
					txID = foundTxID
					outIdx = foundVout
					beef = foundBeef
					if uErr := e.UpdatePendingBRC29Tx(ctx, rec.ID, txID, outIdx, beef, rec.Satoshis); uErr != nil {
						e.logf("wallet: update pending BRC-29 sweep %d (%s): %v", rec.ID, txID, uErr)
						recordErr(uErr)
					}
				}
			}
			if len(beef) == 0 && txID != "" {
				if tx, tErr := e.LoadParsedTx(ctx, txID); tErr == nil && tx != nil {
					if encoded, sErr := SafeAtomicBEEF(tx); sErr == nil {
						beef = encoded
					} else {
						beefErr = fmt.Errorf("wallet: encode AtomicBEEF for pending sweep %s: %w", txID, sErr)
					}
				} else if tErr != nil {
					beefErr = tErr
				}
			}
			if len(beef) == 0 {
				e.recordBRC29Backoff(rec.ID, now)
				if beefErr != nil {
					e.logf("wallet: pending BRC-29 sweep %d (%s) missing AtomicBEEF: %v", rec.ID, txID, beefErr)
					recordErr(beefErr)
				} else if txID != "" {
					err := fmt.Errorf("wallet: pending BRC-29 sweep %d (%s) AtomicBEEF unavailable", rec.ID, txID)
					e.logf("%v", err)
					recordErr(err)
				}
				continue
			}
			wURL := resolveWireURL(rec.WireURL)
			if cErr := wirewallet.CheckURL(wURL); cErr != nil {
				e.recordBRC29Backoff(rec.ID, now)
				recordErr(cErr)
				continue
			}
			if cachedErr, offline := unreachableWire[wURL]; offline {
				e.recordBRC29Backoff(rec.ID, now)
				recordErr(cachedErr)
				continue
			}
			desktop, dErr := dialWireWallet(e.cfg.Originator, wURL, 15*time.Second)
			if dErr != nil {
				unreachableWire[wURL] = dErr
				e.recordBRC29Backoff(rec.ID, now)
				recordErr(dErr)
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
				e.recordBRC29Backoff(rec.ID, now)
				recordErr(iErr)
				continue
			}
			if mErr := e.MarkPendingBRC29Done(ctx, rec.ID); mErr != nil {
				e.logf("wallet: mark pending BRC-29 sweep %d done: %v", rec.ID, mErr)
				e.recordBRC29Backoff(rec.ID, now)
				recordErr(mErr)
				continue
			}
			e.clearBRC29Backoff(rec.ID)
			reconciled++
		}
	}
	return reconciled, firstErr
}
