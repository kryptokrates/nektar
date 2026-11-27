package hub

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/chain"
)

type feeCollectOutcome struct {
	HTTPStatus int
	Code       string
	Error      string
	TxID       string
	PayerPub   string
	PaidSats   uint64
	Decision   string
}

// verifyAndCollectFee enforces BRC-105 payment verification, Lightweb acceptance limits,
// replay/conflict protection, and wallet internalization for any priced Hub action.
func (n *Node) verifyAndCollectFee(ctx context.Context, action, kind string, priceSats uint64, payHeader string) feeCollectOutcome {
	if priceSats == 0 {
		return feeCollectOutcome{HTTPStatus: http.StatusOK}
	}
	if n.feeCollector == nil {
		return feeCollectOutcome{
			HTTPStatus: http.StatusServiceUnavailable,
			Code:       "ERR_HUB_FEE_UNCONFIGURED",
			Error:      "hub priced action requires a configured FeeCollector wallet",
		}
	}
	payHeader = strings.TrimSpace(payHeader)
	if payHeader == "" {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_REQUIRED",
			Error:      fmt.Sprintf("payment of %d sats required for %s", priceSats, action),
		}
	}
	var pay BRC105PaymentHeader
	if err := json.Unmarshal([]byte(payHeader), &pay); err != nil {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_MALFORMED",
			Error:      "invalid X-BSV-Payment JSON header",
		}
	}
	if pay.BEEFHex == "" || pay.DerivationPrefix == "" || pay.DerivationSuffix == "" || pay.SenderIdentityKey == "" {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_INCOMPLETE",
			Error:      "X-BSV-Payment requires beefHex, derivationPrefix, derivationSuffix, and senderIdentityKey",
		}
	}
	if err := n.VerifyPrefix(pay.DerivationPrefix, action, priceSats, time.Now()); err != nil {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_PREFIX",
			Error:      err.Error(),
		}
	}
	rawBeef, err := hex.DecodeString(strings.TrimSpace(pay.BEEFHex))
	if err != nil || len(rawBeef) == 0 {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_MALFORMED",
			Error:      "invalid X-BSV-Payment beefHex",
		}
	}
	_, tx, txHash, err := guard.ParseBEEF(rawBeef, guard.DefaultBound)
	if err != nil || tx == nil || txHash == nil {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_MALFORMED",
			Error:      "unparseable X-BSV-Payment AtomicBEEF",
		}
	}
	txid := txHash.String()
	if pay.TxID != "" && !strings.EqualFold(strings.TrimSpace(pay.TxID), txid) {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_MALFORMED",
			Error:      "X-BSV-Payment txid does not match AtomicBEEF",
		}
	}
	if uint64(pay.OutputIndex) >= uint64(len(tx.Outputs)) {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_WRONG_SCRIPT",
			Error:      "X-BSV-Payment outputIndex out of range",
		}
	}

	// Derive expected BRC-29 locking script and assert output pays >= priceSats.
	wantScript, dErr := n.feeCollector.DeriveBRC29Script(ctx, pay.SenderIdentityKey, pay.DerivationPrefix, pay.DerivationSuffix)
	if dErr != nil || wantScript == nil {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_WRONG_SCRIPT",
			Error:      fmt.Sprintf("BRC-29 script derivation failed: %v", dErr),
		}
	}
	out := tx.Outputs[pay.OutputIndex]
	if out == nil || out.LockingScript == nil || !bytes.Equal(*out.LockingScript, *wantScript) {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_WRONG_SCRIPT",
			Error:      "payment output does not match derived BRC-29 locking script",
		}
	}
	paidSats := out.Satoshis
	if paidSats < priceSats {
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_INSUFFICIENT",
			Error:      fmt.Sprintf("insufficient payment (%d < %d sats)", paidSats, priceSats),
		}
	}

	inKeys := make([]string, 0, len(tx.Inputs))
	for _, in := range tx.Inputs {
		if in != nil && in.SourceTXID != nil {
			inKeys = append(inKeys, fmt.Sprintf("%s:%d", in.SourceTXID.String(), in.SourceTxOutIndex))
		}
	}
	payerPub := strings.ToLower(strings.TrimSpace(pay.SenderIdentityKey))

	// Serialize exposure check + claim + ledger recording so concurrent 0-conf payments cannot bypass AcceptPayerLimitSats / AcceptTotalLimitSats.
	n.feeCollectMu.Lock()
	defer n.feeCollectMu.Unlock()

	// Check Lightweb acceptance policy: flagged payer OR over fast threshold/payer/total exposure -> require mined.
	// Fail closed on store errors so broken DB state can never permit 0-conf fast acceptance.
	flagged, flagReason, flagErr := n.store.IsPayerFlagged(payerPub)
	if flagErr != nil {
		return feeCollectOutcome{
			HTTPStatus: http.StatusServiceUnavailable,
			Code:       "ERR_HUB_STORE_UNAVAILABLE",
			Error:      fmt.Sprintf("payer risk lookup failed: %v", flagErr),
		}
	}
	payerExp, totalExp, expErr := n.store.UnminedFastExposure(payerPub, int64(n.fees.AcceptWindow.Seconds()))
	if expErr != nil {
		return feeCollectOutcome{
			HTTPStatus: http.StatusServiceUnavailable,
			Code:       "ERR_HUB_STORE_UNAVAILABLE",
			Error:      fmt.Sprintf("unmined exposure lookup failed: %v", expErr),
		}
	}
	holdReason := ""
	if flagged {
		holdReason = "payer flagged for prior unsettleable payment: " + flagReason
	} else if paidSats > n.fees.AcceptThresholdSats {
		holdReason = fmt.Sprintf("payment %d sats exceeds fast accept threshold %d sats", paidSats, n.fees.AcceptThresholdSats)
	} else if payerExp+paidSats > n.fees.AcceptPayerLimitSats {
		holdReason = fmt.Sprintf("payer unmined exposure %d+%d exceeds limit %d sats", payerExp, paidSats, n.fees.AcceptPayerLimitSats)
	} else if totalExp+paidSats > n.fees.AcceptTotalLimitSats {
		holdReason = fmt.Sprintf("hub total unmined exposure %d+%d exceeds limit %d sats", totalExp, paidSats, n.fees.AcceptTotalLimitSats)
	}

	// Run full local consensus SPV verification BEFORE replay claim or wallet internalization.
	settled := false
	var blockHeight uint32
	if n.feeCollector.Network() != "memory" {
		allowZeroConf := holdReason == ""
		ver, vErr := n.feeCollector.VerifyPaymentBEEF(ctx, rawBeef, chain.PaymentVerifyOpts{
			ExpectedAmountSats:       priceSats,
			ZeroConfMaxSats:          n.fees.AcceptThresholdSats,
			ZeroConfMaxAggregateSats: n.fees.AcceptTotalLimitSats,
			SettleDepth:              n.feeCollector.SettleDepth(),
			AllowZeroConf:            allowZeroConf,
			ConflictWatch:            n.feeCollector.ZeroConfWatch(),
			TrackOnZeroConf:          allowZeroConf,
		})
		if vErr != nil {
			if errors.Is(vErr, chain.ErrTrackerUnavailable) {
				return feeCollectOutcome{
					HTTPStatus: http.StatusServiceUnavailable,
					Code:       "ERR_TRACKER_UNAVAILABLE",
					Error:      vErr.Error(),
				}
			}
			if errors.Is(vErr, chain.ErrConflictDetected) {
				_ = n.store.FlagPayer(payerPub, txid, vErr.Error())
				return feeCollectOutcome{
					HTTPStatus: http.StatusConflict,
					Code:       "ERR_PAYMENT_CONFLICT",
					Error:      vErr.Error(),
					TxID:       txid,
				}
			}
			return feeCollectOutcome{
				HTTPStatus: http.StatusPaymentRequired,
				Code:       "ERR_PAYMENT_SPV_FAILED",
				Error:      vErr.Error(),
			}
		}
		if ver.NeedsConfirmation {
			// Record held claim so the payer can retry the exact same txid once mined without paying twice!
			if cErr := n.store.ClaimFeePaymentAtomic(txid, inKeys, action, payerPub, paidSats, "held"); cErr != nil {
				return mapClaimErr(cErr, txid)
			}
			_ = n.store.RecordFeeLedger(FeeLedgerEntry{
				TxID:     txid,
				Action:   action,
				Kind:     kind,
				PayerPub: payerPub,
				Sats:     paidSats,
				Decision: "hold",
				Status:   "held",
			})
			reason := holdReason
			if reason == "" {
				reason = ver.EscalationReason
			}
			return feeCollectOutcome{
				HTTPStatus: http.StatusAccepted,
				Code:       "ERR_PAYMENT_HELD",
				Error:      reason,
				TxID:       txid,
				PayerPub:   payerPub,
				PaidSats:   paidSats,
				Decision:   "hold",
			}
		}
		settled = ver.SettledAtDepth
		blockHeight = ver.BlockHeight
	} else {
		// In memory mode, still honor flagged-payer & explicit hold threshold when unmined so tests can verify 202 hold!
		if tx.MerklePath != nil {
			settled = true
			blockHeight = tx.MerklePath.BlockHeight
		} else if holdReason != "" {
			if cErr := n.store.ClaimFeePaymentAtomic(txid, inKeys, action, payerPub, paidSats, "held"); cErr != nil {
				return mapClaimErr(cErr, txid)
			}
			_ = n.store.RecordFeeLedger(FeeLedgerEntry{
				TxID:     txid,
				Action:   action,
				Kind:     kind,
				PayerPub: payerPub,
				Sats:     paidSats,
				Decision: "hold",
				Status:   "held",
			})
			return feeCollectOutcome{
				HTTPStatus: http.StatusAccepted,
				Code:       "ERR_PAYMENT_HELD",
				Error:      holdReason,
				TxID:       txid,
				PayerPub:   payerPub,
				PaidSats:   paidSats,
				Decision:   "hold",
			}
		}
	}

	// Atomic anti-replay & anti-conflict claim BEFORE internalizing or admitting.
	if cErr := n.store.ClaimFeePaymentAtomic(txid, inKeys, action, payerPub, paidSats, "claimed"); cErr != nil {
		return mapClaimErr(cErr, txid)
	}

	// Internalize directly into the Hub's wallet (exempt from Hub outgoing SpendGuard!).
	if _, iErr := n.feeCollector.InternalizeBRC29(ctx, rawBeef, pay.OutputIndex, pay.DerivationPrefix, pay.DerivationSuffix, pay.SenderIdentityKey, "hub fee "+action, []string{"nektar", "hub-fee", action}); iErr != nil {
		_ = n.store.ReleaseFeeClaim(txid)
		return feeCollectOutcome{
			HTTPStatus: http.StatusPaymentRequired,
			Code:       "ERR_PAYMENT_INTERNALIZE",
			Error:      fmt.Sprintf("internalize hub fee failed: %v", iErr),
		}
	}

	decision := "fast"
	status := "unmined"
	if settled {
		decision = "mined"
		status = "mined"
	}
	_ = n.store.RecordFeeLedger(FeeLedgerEntry{
		TxID:        txid,
		Action:      action,
		Kind:        kind,
		PayerPub:    payerPub,
		Sats:        paidSats,
		Decision:    decision,
		Status:      status,
		BlockHeight: blockHeight,
	})
	return feeCollectOutcome{
		HTTPStatus: http.StatusOK,
		TxID:       txid,
		PayerPub:   payerPub,
		PaidSats:   paidSats,
		Decision:   decision,
	}
}

func mapClaimErr(err error, txid string) feeCollectOutcome {
	if errors.Is(err, ErrFeePaymentConflict) {
		return feeCollectOutcome{
			HTTPStatus: http.StatusConflict,
			Code:       "ERR_PAYMENT_CONFLICT",
			Error:      err.Error(),
			TxID:       txid,
		}
	}
	if errors.Is(err, ErrFeePaymentReplayed) {
		return feeCollectOutcome{
			HTTPStatus: http.StatusConflict,
			Code:       "ERR_PAYMENT_REPLAYED",
			Error:      err.Error(),
			TxID:       txid,
		}
	}
	return feeCollectOutcome{
		HTTPStatus: http.StatusServiceUnavailable,
		Code:       "ERR_HUB_STORE_UNAVAILABLE",
		Error:      err.Error(),
		TxID:       txid,
	}
}
