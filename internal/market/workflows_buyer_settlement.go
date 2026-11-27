package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func offerMatchesDealRecord(existing store.DealRecord, offerID string) bool {
	if offerID == "" || len(existing.ChosenOfferJSON) == 0 {
		return true
	}
	var prevOffer bstack.OfferAd
	if err := json.Unmarshal(existing.ChosenOfferJSON, &prevOffer); err != nil || prevOffer.OfferID == "" {
		return true
	}
	return prevOffer.OfferID == offerID
}

func (e *Engine) buyerPayAndDeliver(rctx context.Context, dealID string, chosen bstack.OfferAd) (buyerPayResult, error) {
	now := time.Now().Unix()
	br := e.getBroker()
	if br == nil {
		// Fail retryably if Broker is not yet initialized so Restate never pays without delivering!
		return buyerPayResult{}, errors.New("market: broker not initialized yet for purchase delivery")
	}
	// Check if we already built/persisted PaymentTxID + BRC-29 remittance for THIS exact seller & offer
	// before a crash or ErrBroadcastUnknown retry! Never reuse a previous seller's tx across failover.
	var payTxID string
	var payBEEF []byte
	var outIdx uint32
	var derivPrefix, derivSuffix, senderKey string
	var escrowPayload *EscrowObligationPayload
	if existing, gErr := e.mem.DB().GetDeal(dealID); gErr == nil && existing.PaymentTxID != "" &&
		strings.EqualFold(existing.CounterpartyPub, chosen.Pub) &&
		offerMatchesDealRecord(existing, chosen.OfferID) {
		payTxID = existing.PaymentTxID
		payBEEF = existing.PaymentBEEF
		outIdx = existing.PaymentOutputIndex
		derivPrefix = existing.PaymentDerivPrefix
		derivSuffix = existing.PaymentDerivSuffix
		senderKey = existing.PaymentSenderKey
		if ob, obErr := e.mem.DB().GetObligation(ObligationID(dealID, ObligationRevealOwed)); obErr == nil && len(ob.PayloadJSON) > 0 {
			var ep EscrowObligationPayload
			if json.Unmarshal(ob.PayloadJSON, &ep) == nil && ep.EscrowTxID != "" {
				escrowPayload = &ep
			}
		}
		// Ensure previously built/uncertain tx is actually broadcast idempotently without constructing a second tx!
		mined, wStatus, _, _, _ := e.mem.Chain().TxBroadcastStatus(rctx, payTxID)
		if !mined && len(payBEEF) > 0 {
			if _, parsedTx, _, pErr := guard.ParseBEEF(payBEEF, guard.DefaultBound); pErr == nil && parsedTx != nil {
				switch wStatus {
				case "nosend":
					if _, bErr := e.mem.Chain().Broadcast(rctx, parsedTx, 0, chosen.PriceSats); bErr != nil {
						if errors.Is(bErr, nwallet.ErrSpendRejected) {
							e.clearPendingPayment(dealID)
							return buyerPayResult{NowUnix: now, PreBroadcastErr: bErr.Error()}, nil
						}
						// Non-ErrSpendRejected broadcast errors are ambiguous post-submit: keep PaymentTxID
						// persisted and fail/retry reconciliation rather than double-paying another seller!
						return buyerPayResult{}, bErr
					}
				case "submission_unknown", "sending", "unproven":
					// Re-air unproven/uncertain payment idempotently so mempool/peers receive full BEEF ancestry.
					_ = e.mem.Chain().RelayExternalTx(rctx, parsedTx)
				}
			}
		}
	} else {
		chosenJSON, _ := json.Marshal(chosen)
		e.persistDealRecord(store.DealRecord{
			DealID:          dealID,
			Role:            "buyer",
			Status:          "payment_pending",
			ChosenOfferJSON: chosenJSON,
			CounterpartyPub: chosen.Pub,
		})

		// Wave 1 Rúnar Escrow Path: when policy selects DisclosureSale escrow (default auto >= 500 sats
		// or > ZeroConfMaxSats) and Seller supplied a signed KeyedSaleOffer, fund an atomic DisclosureSale
		// UTXO (from an active NektarAgentVault in 1 tx when deployed, else from BRC-100 wallet!).
		useEscrow := e.policy != nil && e.policy.ShouldUseEscrow(chosen.PriceSats) && len(chosen.SaleOffer) > 0
		if useEscrow {
			var so contracts.KeyedSaleOffer
			if uErr := json.Unmarshal(chosen.SaleOffer, &so); uErr != nil {
				return buyerPayResult{NowUnix: now, PreBroadcastErr: "decode saleOffer for escrow: " + uErr.Error()}, nil
			}
			hWrapped, hwErr := hex.DecodeString(so.Config.HWrapped)
			if hwErr != nil || len(hWrapped) != 32 {
				return buyerPayResult{NowUnix: now, PreBroadcastErr: "saleOffer missing valid 32-byte hWrapped"}, nil
			}
			var sellerPKH []byte
			if chosen.PayAddress != "" {
				if addr, aErr := script.NewAddressFromString(strings.TrimSpace(chosen.PayAddress)); aErr == nil && len(addr.PublicKeyHash) == 20 {
					sellerPKH = append([]byte(nil), addr.PublicKeyHash...)
				}
			}
			if len(sellerPKH) != 20 {
				sellerPKH, _ = hex.DecodeString(so.Config.SellerPKH)
			}
			depPKH, fErr := e.mem.Chain().FundPKH(rctx)
			if fErr != nil || len(depPKH) != 20 {
				return buyerPayResult{NowUnix: now, PreBroadcastErr: fmt.Sprintf("derive depositor PKH: %v", fErr)}, nil
			}
			maxFee := so.Config.MaxFee
			if maxFee < 300 {
				maxFee = 500
			}
			if maxFee > 2000 {
				maxFee = 2000
			}
			refundAfter := so.Config.RefundAfter
			if refundAfter == 0 || (refundAfter >= 500_000_000 && int64(refundAfter) <= now+120) {
				refundAfter = uint64(now + 3600)
			}
			escrowParams := contracts.DisclosureSaleParams{
				SellerPKH:    sellerPKH,
				Price:        chosen.PriceSats,
				HWrapped:     hWrapped,
				RefundAfter:  refundAfter,
				MaxFee:       maxFee,
				DepositorPKH: depPKH,
			}
			escrowLock, lErr := contracts.EncodeDisclosureSaleLockingScript(escrowParams)
			if lErr != nil {
				return buyerPayResult{NowUnix: now, PreBroadcastErr: "encode DisclosureSale escrow: " + lErr.Error()}, nil
			}
			escrowSats := escrowParams.RequiredEscrowSatoshis() + 100

			// Atomic 1-tx composition: if an active unpaused NektarAgentVault is deployed, fund the
			// DisclosureSale escrow DIRECTLY from the vault via AgentSpend (Output 0 = vault, Output 1 = escrow).
			// Fail closed if vault loading or vault spend fails — NEVER silently bypass vault caps into hot wallet.
			fundedFromVault := false
			vRec, _, vErr := LoadActiveVault(e.mem.DB())
			if vErr != nil {
				return buyerPayResult{NowUnix: now, PreBroadcastErr: "load active vault: " + vErr.Error()}, nil
			}
			if vRec != nil && !vRec.State.Paused {
				vTx, vBEEF, vOutIdx, _, _, svErr := SpendActiveVault(rctx, e.mem.DB(), e.mem.Chain(), escrowLock, escrowSats)
				if svErr != nil {
					if vTx != nil {
						payTxID = vTx.TxID().String()
						payBEEF = vBEEF
						outIdx = vOutIdx
						e.persistDealRecord(store.DealRecord{
							DealID:             dealID,
							Role:               "buyer",
							Status:             "payment_pending",
							CounterpartyPub:    chosen.Pub,
							PaymentTxID:        payTxID,
							PaymentBEEF:        payBEEF,
							PaymentOutputIndex: outIdx,
						})
						return buyerPayResult{}, svErr
					}
					if errors.Is(svErr, nwallet.ErrBroadcastUnknown) {
						return buyerPayResult{}, svErr
					}
					return buyerPayResult{NowUnix: now, PreBroadcastErr: "active vault spend refused: " + svErr.Error()}, nil
				}
				if vTx == nil {
					return buyerPayResult{NowUnix: now, PreBroadcastErr: "active vault spend returned nil transaction"}, nil
				}
				payTxID = vTx.TxID().String()
				payBEEF = vBEEF
				outIdx = vOutIdx
				fundedFromVault = true
			}
			if !fundedFromVault {
				meta := nwallet.ActionLockEscrow(chosen.OfferID, escrowSats)
				escrowSpec := &nwallet.OutputTagSpec{
					Basket:             nwallet.BasketEscrows,
					Tags:               []string{"escrow", "deal:" + dealID, "offer:" + chosen.OfferID},
					CustomInstructions: fmt.Sprintf(`{"v":1,"kind":"escrow","dealId":%q,"offerId":%q,"price":%d,"refundAfter":%d}`, dealID, chosen.OfferID, chosen.PriceSats, refundAfter),
				}
				builtTx, builtBEEF, fee, spend, bErr := e.mem.Chain().BuildFundedTxWithBasket(rctx, []*transaction.TransactionOutput{{
					Satoshis:      escrowSats,
					LockingScript: escrowLock,
				}}, meta.Description, meta.Labels, escrowSpec)
				if bErr != nil {
					if errors.Is(bErr, nwallet.ErrBroadcastUnknown) {
						return buyerPayResult{}, bErr
					}
					return buyerPayResult{NowUnix: now, PreBroadcastErr: bErr.Error()}, nil
				}
				payTxID = builtTx.TxID().String()
				payBEEF = builtBEEF
				if len(payBEEF) == 0 {
					if b2, sErr := chain.SafeAtomicBEEF(builtTx); sErr == nil && len(b2) > 0 {
						payBEEF = b2
					}
				}
				outIdx = 0
				e.persistDealRecord(store.DealRecord{
					DealID:             dealID,
					Role:               "buyer",
					Status:             "payment_pending",
					CounterpartyPub:    chosen.Pub,
					PaymentTxID:        payTxID,
					PaymentBEEF:        payBEEF,
					PaymentOutputIndex: outIdx,
				})
				if _, brErr := e.mem.Chain().Broadcast(rctx, builtTx, fee, spend); brErr != nil {
					if errors.Is(brErr, nwallet.ErrSpendRejected) {
						e.clearPendingPayment(dealID)
						return buyerPayResult{NowUnix: now, PreBroadcastErr: brErr.Error()}, nil
					}
					// Any non-ErrSpendRejected broadcast error is ambiguous post-submit: keep PaymentTxID
					// persisted so retries reconcile the SAME txid instead of double-paying Seller #2!
					return buyerPayResult{}, brErr
				}
				if freshBEEF, fErr := chain.SafeAtomicBEEF(builtTx); fErr == nil && len(freshBEEF) > 0 {
					payBEEF = freshBEEF
				}
			}
			escrowPayload = &EscrowObligationPayload{
				OfferID:      chosen.OfferID,
				EscrowTxID:   payTxID,
				EscrowVout:   outIdx,
				EscrowBEEF:   hex.EncodeToString(payBEEF),
				SellerPKH:    hex.EncodeToString(sellerPKH),
				DepositorPKH: hex.EncodeToString(depPKH),
				Price:        chosen.PriceSats,
				HWrapped:     so.Config.HWrapped,
				RefundAfter:  refundAfter,
				MaxFee:       maxFee,
			}
			e.persistDealRecord(store.DealRecord{
				DealID:             dealID,
				Role:               "buyer",
				Status:             "paid_awaiting_reveal",
				CounterpartyPub:    chosen.Pub,
				PaymentTxID:        payTxID,
				PaymentBEEF:        payBEEF,
				PaymentOutputIndex: outIdx,
			})
		} else if chosen.PayIdentityKey != "" {
			// Pre-build signed NoSend BRC-29 tx and persist txid + BEEF + remittance metadata BEFORE broadcast
			// so neither crashes nor ErrBroadcastUnknown retries can ever double-pay or lose BRC-29 nonces!
			buyMeta := nwallet.ActionBuyKnowledge(chosen.OfferID, chosen.PriceSats)
			builtTx, fee, spend, sendRes, alreadyBroadcast, bErr := e.mem.Chain().BuildBRC29Tx(rctx, chosen.PayIdentityKey, chosen.PriceSats, buyMeta.Description)
			if bErr != nil {
				if errors.Is(bErr, nwallet.ErrBroadcastUnknown) {
					return buyerPayResult{}, bErr
				}
				return buyerPayResult{NowUnix: now, PreBroadcastErr: bErr.Error()}, nil
			}
			payTxID = sendRes.TxID
			payBEEF = sendRes.BEEF
			outIdx = sendRes.OutputIndex
			derivPrefix = sendRes.DerivationPrefix
			derivSuffix = sendRes.DerivationSuffix
			senderKey = sendRes.SenderIdentityKey
			e.persistDealRecord(store.DealRecord{
				DealID:             dealID,
				Role:               "buyer",
				Status:             "payment_pending",
				CounterpartyPub:    chosen.Pub,
				PaymentTxID:        payTxID,
				PaymentBEEF:        payBEEF,
				PaymentOutputIndex: outIdx,
				PaymentDerivPrefix: derivPrefix,
				PaymentDerivSuffix: derivSuffix,
				PaymentSenderKey:   senderKey,
			})
			if !alreadyBroadcast {
				if _, brErr := e.mem.Chain().Broadcast(rctx, builtTx, fee, spend); brErr != nil {
					if errors.Is(brErr, nwallet.ErrSpendRejected) {
						e.clearPendingPayment(dealID)
						return buyerPayResult{NowUnix: now, PreBroadcastErr: brErr.Error()}, nil
					}
					// Persisted PaymentTxID ensures retry reconciles the SAME txid instead of double-paying!
					return buyerPayResult{}, brErr
				}
			}
			if freshBEEF, fErr := chain.SafeAtomicBEEF(builtTx); fErr == nil && len(freshBEEF) > 0 {
				payBEEF = freshBEEF
			}
			e.persistDealRecord(store.DealRecord{
				DealID:             dealID,
				Role:               "buyer",
				Status:             "paid_awaiting_reveal",
				CounterpartyPub:    chosen.Pub,
				PaymentTxID:        payTxID,
				PaymentBEEF:        payBEEF,
				PaymentOutputIndex: outIdx,
				PaymentDerivPrefix: derivPrefix,
				PaymentDerivSuffix: derivSuffix,
				PaymentSenderKey:   senderKey,
			})
		} else {
			return buyerPayResult{NowUnix: now, PreBroadcastErr: "seller payIdentityKey (BRC-29) is required"}, nil
		}
	}

	if payTxID != "" && e.mem != nil && e.mem.DB() != nil {
		dl := now + 7200
		if escrowPayload != nil && escrowPayload.RefundAfter >= 500_000_000 {
			dl = int64(escrowPayload.RefundAfter)
		}
		_ = RecordRevealOwedWithEscrow(e.mem.DB(), dealID, chosen.Pub, payTxID, chosen.PriceSats, dl, escrowPayload)
	}

	res := buyerPayResult{NowUnix: now, TxID: payTxID}
	if escrowPayload != nil {
		res.EscrowTxID = escrowPayload.EscrowTxID
		res.EscrowRefundAfter = escrowPayload.RefundAfter
	}
	pReq := PurchaseRequest{
		OfferID:           chosen.OfferID,
		WantID:            dealID,
		BuyerPub:          br.PubHex(),
		TxID:              payTxID,
		PaymentBEEFHex:    hex.EncodeToString(payBEEF),
		OutputIndex:       outIdx,
		DerivationPrefix:  derivPrefix,
		DerivationSuffix:  derivSuffix,
		SenderIdentityKey: senderKey,
	}
	if escrowPayload != nil {
		pReq.EscrowMode = true
		pReq.DepositorPKH = escrowPayload.DepositorPKH
		pReq.EscrowSellerPKH = escrowPayload.SellerPKH
		pReq.EscrowRefundAfter = escrowPayload.RefundAfter
		pReq.EscrowMaxFee = escrowPayload.MaxFee
	}
	br.signPurchaseRequest(&pReq)
	// Try direct HTTP first; if Seller is NAT-ed, fire bbox purchase_beef asynchronously
	// and return nil so BuyerDeal suspends durably on Restate Promise("claim")!
	pResp, dErr := br.DeliverPurchaseAsync(rctx, chosen, dealID, pReq)
	if dErr != nil {
		res.DeliveryErr = dErr.Error()
	} else if pResp != nil && pResp.OK {
		claimTID := payTxID
		if pResp.ClaimTxID != "" {
			claimTID = pResp.ClaimTxID
		}
		res.ImmediateClaim = &ClaimSignal{
			ClaimTxID:      claimTID,
			ClaimTxBEEFHex: pResp.ClaimTxBEEFHex,
			ContentKeyHex:  pResp.ContentKeyHex,
			CiphertextHex:  pResp.CiphertextHex,
			ChirpURL:       pResp.ChirpURL,
			ProofBundle:    pResp.ProofBundle,
			DealReceipt:    pResp.DealReceipt,
		}
	}
	return res, nil
}
