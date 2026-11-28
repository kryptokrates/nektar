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

func (a *workflowActivities) BuyerBroadcastPayment(ctx context.Context, dealID string, chosen bstack.OfferAd) (buyerPayResult, error) {
	return a.eng.buyerBroadcastPayment(ctx, dealID, chosen)
}

func (a *workflowActivities) BuyerDeliverPurchase(ctx context.Context, dealID string, chosen bstack.OfferAd, payRes buyerPayResult) (buyerDeliverResult, error) {
	return a.eng.buyerDeliverPurchase(ctx, dealID, chosen, payRes)
}

func buildRevealObligationRecord(dealID, sellerPub, txid string, amountSats uint64, nowUnix int64, escrow *EscrowObligationPayload) store.ObligationRecord {
	dl := nowUnix + 7200
	if escrow != nil && escrow.RefundAfter >= 500_000_000 {
		dl = int64(escrow.RefundAfter)
	}
	var payloadJSON []byte
	if escrow != nil {
		payloadJSON, _ = json.Marshal(escrow)
	}
	return store.ObligationRecord{
		ID:           ObligationID(dealID, ObligationRevealOwed),
		DealID:       dealID,
		Role:         "buyer",
		Kind:         ObligationRevealOwed,
		Counterparty: sellerPub,
		AmountSats:   amountSats,
		TxID:         txid,
		DeadlineUnix: dl,
		Status:       "open",
		PayloadJSON:  payloadJSON,
	}
}

// buyerBroadcastPayment builds, pre-journals (including EscrowObligationPayload BEFORE Broadcast per STATE-2),
// and broadcasts the buyer's on-chain payment without coupling HTTP/bbox delivery retries into payment state (RT-3).
func (e *Engine) buyerBroadcastPayment(rctx context.Context, dealID string, chosen bstack.OfferAd) (buyerPayResult, error) {
	now := time.Now().Unix()
	if e.getBroker() == nil {
		return buyerPayResult{}, errors.New("market: broker not initialized yet for purchase delivery")
	}
	db := e.mem.DB()

	var payTxID string
	var payBEEF []byte
	var outIdx uint32
	var derivPrefix, derivSuffix, senderKey string
	var escrowPayload *EscrowObligationPayload

	fj := e.FastJournal()
	var existing store.DealRecord
	var gErr error
	if fj != nil {
		if rec, ok := fj.GetDealWAL(dealID); ok {
			existing = rec
		} else {
			gErr = errors.New("not found")
		}
	} else {
		existing, gErr = db.GetDeal(dealID)
	}
	if gErr == nil && existing.PaymentTxID != "" &&
		strings.EqualFold(existing.CounterpartyPub, chosen.Pub) &&
		offerMatchesDealRecord(existing, chosen.OfferID) {
		payTxID = existing.PaymentTxID
		payBEEF = existing.PaymentBEEF
		outIdx = existing.PaymentOutputIndex
		derivPrefix = existing.PaymentDerivPrefix
		derivSuffix = existing.PaymentDerivSuffix
		senderKey = existing.PaymentSenderKey
		if fj == nil {
			if ob, obErr := db.GetObligation(ObligationID(dealID, ObligationRevealOwed)); obErr == nil && len(ob.PayloadJSON) > 0 {
				var ep EscrowObligationPayload
				if json.Unmarshal(ob.PayloadJSON, &ep) == nil && ep.EscrowTxID != "" {
					escrowPayload = &ep
				}
			}
		}
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
						return buyerPayResult{}, bErr
					}
				case "submission_unknown", "sending", "unproven":
					_ = e.mem.Chain().RelayExternalTx(rctx, parsedTx)
				}
			}
		}
	} else {
		chosenJSON, _ := json.Marshal(chosen)
		if fj == nil {
			if err := e.persistDealRecordChecked(store.DealRecord{
				DealID:          dealID,
				Role:            "buyer",
				Status:          "payment_pending",
				ChosenOfferJSON: chosenJSON,
				CounterpartyPub: chosen.Pub,
				PriceSats:       chosen.PriceSats,
			}); err != nil {
				return buyerPayResult{}, err
			}
		}

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

			fundedFromVault := false
			vRec, _, vErr := LoadActiveVault(db)
			if vErr != nil {
				return buyerPayResult{NowUnix: now, PreBroadcastErr: "load active vault: " + vErr.Error()}, nil
			}
			if vRec != nil && !vRec.State.Paused {
				vTx, vBEEF, vOutIdx, _, _, svErr := SpendActiveVault(rctx, db, e.mem.Chain(), escrowLock, escrowSats)
				if svErr != nil {
					if vTx != nil {
						payTxID = vTx.TxID().String()
						payBEEF = vBEEF
						outIdx = vOutIdx
						ep := &EscrowObligationPayload{
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
						_ = db.SaveDealWithObligations(mergeDealOrNew(db, store.DealRecord{
							DealID:             dealID,
							Role:               "buyer",
							Status:             "payment_pending",
							ChosenOfferJSON:    chosenJSON,
							CounterpartyPub:    chosen.Pub,
							PriceSats:          chosen.PriceSats,
							PaymentTxID:        payTxID,
							PaymentBEEF:        payBEEF,
							PaymentOutputIndex: outIdx,
						}), buildRevealObligationRecord(dealID, chosen.Pub, payTxID, chosen.PriceSats, now, ep))
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
				// STATE-2 + RCA-2: Atomically commit BOTH deal PaymentTxID AND EscrowObligationPayload BEFORE Broadcast!
				if err := db.SaveDealWithObligations(mergeDealOrNew(db, store.DealRecord{
					DealID:             dealID,
					Role:               "buyer",
					Status:             "payment_pending",
					ChosenOfferJSON:    chosenJSON,
					CounterpartyPub:    chosen.Pub,
					PriceSats:          chosen.PriceSats,
					PaymentTxID:        payTxID,
					PaymentBEEF:        payBEEF,
					PaymentOutputIndex: outIdx,
				}), buildRevealObligationRecord(dealID, chosen.Pub, payTxID, chosen.PriceSats, now, escrowPayload)); err != nil {
					return buyerPayResult{}, err
				}
				if _, brErr := e.mem.Chain().Broadcast(rctx, builtTx, fee, spend); brErr != nil {
					if errors.Is(brErr, nwallet.ErrSpendRejected) {
						e.clearPendingPayment(dealID)
						_ = db.FulfillObligation(ObligationID(dealID, ObligationRevealOwed))
						return buyerPayResult{NowUnix: now, PreBroadcastErr: brErr.Error()}, nil
					}
					return buyerPayResult{}, brErr
				}
				if freshBEEF, fErr := chain.SafeAtomicBEEF(builtTx); fErr == nil && len(freshBEEF) > 0 {
					payBEEF = freshBEEF
					escrowPayload.EscrowBEEF = hex.EncodeToString(payBEEF)
				}
			} else {
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
			}
			if err := db.SaveDealWithObligations(mergeDealOrNew(db, store.DealRecord{
				DealID:             dealID,
				Role:               "buyer",
				Status:             "paid_awaiting_reveal",
				ChosenOfferJSON:    chosenJSON,
				CounterpartyPub:    chosen.Pub,
				PriceSats:          chosen.PriceSats,
				PaymentTxID:        payTxID,
				PaymentBEEF:        payBEEF,
				PaymentOutputIndex: outIdx,
			}), buildRevealObligationRecord(dealID, chosen.Pub, payTxID, chosen.PriceSats, now, escrowPayload)); err != nil {
				return buyerPayResult{}, err
			}
			e.publishDealEvent(dealID, "paid_awaiting_reveal")
		} else if chosen.PayIdentityKey != "" {
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
			preRec := store.DealRecord{
				DealID:             dealID,
				Role:               "buyer",
				Status:             "payment_pending",
				ChosenOfferJSON:    chosenJSON,
				CounterpartyPub:    chosen.Pub,
				PriceSats:          chosen.PriceSats,
				PaymentTxID:        payTxID,
				PaymentBEEF:        payBEEF,
				PaymentOutputIndex: outIdx,
				PaymentDerivPrefix: derivPrefix,
				PaymentDerivSuffix: derivSuffix,
				PaymentSenderKey:   senderKey,
				UpdatedAt:          now,
			}
			if fj != nil {
				if err := fj.SaveDealWAL(preRec); err != nil {
					return buyerPayResult{}, err
				}
			} else {
				if err := db.SaveDealWithObligations(mergeDealOrNew(db, preRec), buildRevealObligationRecord(dealID, chosen.Pub, payTxID, chosen.PriceSats, now, nil)); err != nil {
					return buyerPayResult{}, err
				}
			}
			if !alreadyBroadcast {
				bcastCtx := rctx
				if netName := e.mem.Chain().Network(); netName != "memory" && netName != "regtest" &&
					chosen.PriceSats <= e.mem.Chain().ZeroConfMaxSats() {
					bcastCtx = nwallet.WithAsyncBroadcast(rctx)
				}
				if _, brErr := e.mem.Chain().Broadcast(bcastCtx, builtTx, fee, spend); brErr != nil {
					if errors.Is(brErr, nwallet.ErrSpendRejected) {
						e.clearPendingPayment(dealID)
						if fj == nil {
							_ = db.FulfillObligation(ObligationID(dealID, ObligationRevealOwed))
						}
						return buyerPayResult{NowUnix: now, PreBroadcastErr: brErr.Error()}, nil
					}
					return buyerPayResult{}, brErr
				}
			}
			if fj == nil {
				if freshBEEF, fErr := chain.SafeAtomicBEEF(builtTx); fErr == nil && len(freshBEEF) > 0 {
					payBEEF = freshBEEF
				}
				if err := e.persistDealRecordChecked(store.DealRecord{
					DealID:             dealID,
					Role:               "buyer",
					Status:             "paid_awaiting_reveal",
					ChosenOfferJSON:    chosenJSON,
					CounterpartyPub:    chosen.Pub,
					PriceSats:          chosen.PriceSats,
					PaymentTxID:        payTxID,
					PaymentBEEF:        payBEEF,
					PaymentOutputIndex: outIdx,
					PaymentDerivPrefix: derivPrefix,
					PaymentDerivSuffix: derivSuffix,
					PaymentSenderKey:   senderKey,
				}); err != nil {
					return buyerPayResult{}, err
				}
			}
		} else {
			return buyerPayResult{NowUnix: now, PreBroadcastErr: "seller payIdentityKey (BRC-29) is required"}, nil
		}
	}

	if payTxID != "" && fj == nil {
		_, _ = e.EmitTxEventLocal(rctx, payTxID, TxEventInput{
			Kind:       "broadcast",
			Detail:     "buyer-deal:" + dealID,
			ObservedAt: now,
		})
	}
	res := buyerPayResult{
		NowUnix:       now,
		TxID:          payTxID,
		PayBEEFHex:    hex.EncodeToString(payBEEF),
		OutputIndex:   outIdx,
		DerivPrefix:   derivPrefix,
		DerivSuffix:   derivSuffix,
		SenderKey:     senderKey,
		EscrowPayload: escrowPayload,
	}
	if escrowPayload != nil {
		res.EscrowTxID = escrowPayload.EscrowTxID
		res.EscrowRefundAfter = escrowPayload.RefundAfter
	}
	return res, nil
}

// buyerDeliverPurchase delivers the signed PurchaseRequest to Seller (direct HTTP or NAT bbox fallback)
// in its own retried activity (RT-3) and explicitly handles Status == "awaiting_confirmation" (STATE-1).
func (e *Engine) buyerDeliverPurchase(rctx context.Context, dealID string, chosen bstack.OfferAd, payRes buyerPayResult) (buyerDeliverResult, error) {
	now := time.Now().Unix()
	br := e.getBroker()
	if br == nil {
		return buyerDeliverResult{}, errors.New("market: broker not initialized yet for purchase delivery")
	}
	pReq := PurchaseRequest{
		OfferID:           chosen.OfferID,
		WantID:            dealID,
		BuyerPub:          br.PubHex(),
		TxID:              payRes.TxID,
		PaymentBEEFHex:    payRes.PayBEEFHex,
		OutputIndex:       payRes.OutputIndex,
		DerivationPrefix:  payRes.DerivPrefix,
		DerivationSuffix:  payRes.DerivSuffix,
		SenderIdentityKey: payRes.SenderKey,
	}
	if payRes.EscrowPayload != nil {
		pReq.EscrowMode = true
		pReq.DepositorPKH = payRes.EscrowPayload.DepositorPKH
		pReq.EscrowSellerPKH = payRes.EscrowPayload.SellerPKH
		pReq.EscrowRefundAfter = payRes.EscrowPayload.RefundAfter
		pReq.EscrowMaxFee = payRes.EscrowPayload.MaxFee
	}
	br.signPurchaseRequest(&pReq)

	pResp, dErr := br.DeliverPurchaseAsync(rctx, chosen, dealID, pReq)
	if dErr != nil {
		// Propagate error so go-workflows retries transient HTTP/hub delivery hiccups cleanly (RT-3)!
		return buyerDeliverResult{NowUnix: now, DeliveryErr: dErr.Error()}, dErr
	}
	out := buyerDeliverResult{NowUnix: now}
	if pResp == nil {
		// NAT bbox async path: BuyerDeal suspends on SignalChannel("claim").
		return out, nil
	}
	// STATE-1 fix: when Seller accepts over-cap / unmined payment into payment_pending, it returns
	// Status == "awaiting_confirmation" with NO ContentKeyHex yet — suspend on SignalChannel("claim")!
	if pResp.Status == "awaiting_confirmation" {
		out.AwaitingConfirmation = true
		out.RequiredDepth = pResp.RequiredDepth
		return out, nil
	}
	if pResp.OK && (strings.TrimSpace(pResp.ContentKeyHex) != "" || strings.TrimSpace(pResp.ClaimTxBEEFHex) != "") {
		claimTID := payRes.TxID
		if pResp.ClaimTxID != "" {
			claimTID = pResp.ClaimTxID
		}
		out.ImmediateClaim = &ClaimSignal{
			ClaimTxID:      claimTID,
			ClaimTxBEEFHex: pResp.ClaimTxBEEFHex,
			ContentKeyHex:  pResp.ContentKeyHex,
			CiphertextHex:  pResp.CiphertextHex,
			ChirpURL:       pResp.ChirpURL,
			ProofBundle:    pResp.ProofBundle,
			DealReceipt:    pResp.DealReceipt,
		}
	}
	return out, nil
}

func mergeDealOrNew(db *store.DB, rec store.DealRecord) store.DealRecord {
	if db == nil {
		return rec
	}
	if existing, err := db.GetDeal(rec.DealID); err == nil {
		return mergeExistingDealFields(rec, existing)
	}
	return rec
}
