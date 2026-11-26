package market

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"
	restate "github.com/restatedev/sdk-go"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func (e *Engine) buyerPayAndDeliver(rctx restate.RunContext, dealID string, chosen bstack.OfferAd) (buyerPayResult, error) {
	now := time.Now().Unix()
	br := e.getBroker()
	if br == nil {
		// Fail retryably if Broker is not yet initialized so Restate never pays without delivering!
		return buyerPayResult{}, errors.New("market: broker not initialized yet for purchase delivery")
	}
	// Check if we already built/persisted PaymentTxID + BRC-29 remittance before a crash or ErrBroadcastUnknown retry!
	var payTxID string
	var payBEEF []byte
	var outIdx uint32
	var derivPrefix, derivSuffix, senderKey string
	var escrowPayload *EscrowObligationPayload
	if existing, gErr := e.mem.DB().GetDeal(dealID); gErr == nil && existing.PaymentTxID != "" {
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
		mined, wStatus, _, _ := e.mem.Chain().TxBroadcastStatus(rctx, payTxID)
		if !mined && len(payBEEF) > 0 {
			if _, parsedTx, _, pErr := guard.ParseBEEF(payBEEF, guard.DefaultBound); pErr == nil && parsedTx != nil {
				switch wStatus {
				case "nosend":
					if _, bErr := e.mem.Chain().Broadcast(rctx, parsedTx, 0, chosen.PriceSats); bErr != nil {
						if errors.Is(bErr, nwallet.ErrBroadcastUnknown) {
							return buyerPayResult{}, bErr
						}
						return buyerPayResult{NowUnix: now, PreBroadcastErr: bErr.Error()}, nil
					}
				case "submission_unknown", "sending", "unproven":
					// Re-air unproven/uncertain payment idempotently so mempool/peers receive full BEEF ancestry.
					_ = e.mem.Chain().RelayExternalTx(rctx, parsedTx)
				}
			}
		}
	} else {
		e.persistDealRecord(store.DealRecord{
			DealID:          dealID,
			Role:            "buyer",
			Status:          "payment_pending",
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
			// DisclosureSale escrow DIRECTLY from the vault via AgentSpend (Output 0 = vault, Output 1 = escrow)!
			fundedFromVault := false
			if vRec, _, vErr := LoadActiveVault(e.mem.DB()); vErr == nil && vRec != nil && !vRec.State.Paused {
				vTx, vBEEF, vOutIdx, _, _, svErr := SpendActiveVault(rctx, e.mem.DB(), e.mem.Chain(), escrowLock, escrowSats)
				if svErr == nil && vTx != nil {
					payTxID = vTx.TxID().String()
					payBEEF = vBEEF
					outIdx = vOutIdx
					fundedFromVault = true
				}
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
					if errors.Is(brErr, nwallet.ErrBroadcastUnknown) {
						return buyerPayResult{}, brErr
					}
					return buyerPayResult{NowUnix: now, PreBroadcastErr: brErr.Error()}, nil
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
					if errors.Is(brErr, nwallet.ErrBroadcastUnknown) {
						// Persisted PaymentTxID ensures retry reconciles the SAME txid instead of double-paying!
						return buyerPayResult{}, brErr
					}
					return buyerPayResult{NowUnix: now, PreBroadcastErr: brErr.Error()}, nil
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
			if e.mem.Chain().Network() != "memory" {
				return buyerPayResult{NowUnix: now, PreBroadcastErr: "seller payIdentityKey (BRC-29) is required on live networks"}, nil
			}
			sendRes, sErr := e.mem.Chain().Send(rctx, nwallet.SendArgs{
				ToAddress:   chosen.PayAddress,
				Satoshis:    chosen.PriceSats,
				Description: "market purchase " + chosen.OfferID,
				Labels:      []string{"market", "purchase"},
			})
			if sErr != nil {
				if errors.Is(sErr, nwallet.ErrBroadcastUnknown) {
					return buyerPayResult{}, sErr
				}
				return buyerPayResult{NowUnix: now, PreBroadcastErr: sErr.Error()}, nil
			}
			payTxID = sendRes.TxID
			payBEEF = sendRes.BEEF
			if len(payBEEF) == 0 && sendRes.Tx != nil {
				payBEEF, _ = chain.SafeAtomicBEEF(sendRes.Tx)
			}
			e.persistDealRecord(store.DealRecord{
				DealID:          dealID,
				Role:            "buyer",
				Status:          "paid_awaiting_reveal",
				CounterpartyPub: chosen.Pub,
				PaymentTxID:     payTxID,
				PaymentBEEF:     payBEEF,
			})
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

func (e *Engine) buyerVerifyAndCommit(
	rctx restate.RunContext,
	dealID string,
	want bstack.WantAd,
	targetShardIn string,
	chosen bstack.OfferAd,
	paymentTxID string,
	matchAudit *MatchAudit,
	sig ClaimSignal,
) (buyerVerifyResult, error) {
	now := time.Now().Unix()
	var deliveredText string
	targetShard := strings.TrimSpace(targetShardIn)
	if targetShard == "" {
		dom := "ops"
		if len(want.Domains) > 0 {
			dom = want.Domains[0]
		}
		targetShard = "market/" + dom
	}

	if strings.TrimSpace(sig.ContentKeyHex) == "" && strings.TrimSpace(sig.ClaimTxBEEFHex) == "" {
		return buyerVerifyResult{NowUnix: now, FailedReason: "missing contentKeyHex and claimTxBeefHex in seller claim signal"}, nil
	}
	if len(chosen.SaleOffer) == 0 {
		return buyerVerifyResult{NowUnix: now, FailedReason: "missing signed BRC-369 SaleOffer in chosen offer"}, nil
	}
	{
		var saleOffer contracts.KeyedSaleOffer
		if err := json.Unmarshal(chosen.SaleOffer, &saleOffer); err != nil {
			return buyerVerifyResult{NowUnix: now, FailedReason: "decode saleOffer: " + err.Error()}, nil
		}
		// Trustless L1 fallback: if ContentKeyHex was omitted/dropped off HTTP, extract `k` directly
		// from the on-chain DisclosureSale.Buy(k) transaction's unlocking script!
		if strings.TrimSpace(sig.ContentKeyHex) == "" && strings.TrimSpace(sig.ClaimTxBEEFHex) != "" {
			if rawClaim, dErr := hex.DecodeString(strings.TrimSpace(sig.ClaimTxBEEFHex)); dErr == nil {
				if _, claimTx, _, pErr := guard.ParseBEEF(rawClaim, guard.DefaultBound); pErr == nil && claimTx != nil {
					if extracted, eErr := contracts.ExtractRevealedKeyFromBuyTx(claimTx, 0, saleOffer.Config.HWrapped); eErr == nil {
						sig.ContentKeyHex = hex.EncodeToString(extracted)
					}
				}
			}
		}
		if strings.TrimSpace(sig.ContentKeyHex) == "" {
			return buyerVerifyResult{NowUnix: now, FailedReason: "missing contentKeyHex in seller claim signal (and on-chain extraction failed)"}, nil
		}
		// Skip remote CHIRP HTTP refetch when verified CiphertextHex is already inline in signed SaleOffer (Finding #33).
		if saleOffer.CiphertextHex == "" {
			if chosen.SellerURL != "" && chosen.ChirpURL != "" {
				if br := e.getBroker(); br != nil {
					ct, cErr := br.disc.FetchRemoteChirp(rctx, chosen.SellerURL, chosen.ChirpURL)
					if cErr != nil {
						return buyerVerifyResult{NowUnix: now, FailedReason: "remote CHIRP verification failed: " + cErr.Error()}, nil
					}
					saleOffer.CiphertextHex = hex.EncodeToString(ct)
				}
			} else if sig.CiphertextHex != "" {
				saleOffer.CiphertextHex = sig.CiphertextHex
			}
		}
		keyBytes, kErr := hex.DecodeString(strings.TrimSpace(sig.ContentKeyHex))
		if kErr != nil || len(keyBytes) != 32 {
			return buyerVerifyResult{NowUnix: now, FailedReason: "seller returned invalid 32-byte content key"}, nil
		}
		plaintext, pErr := contracts.ClaimKeyedSale(&saleOffer, keyBytes)
		if pErr != nil {
			return buyerVerifyResult{NowUnix: now, FailedReason: "BRC-369 key verification failed: " + pErr.Error()}, nil
		}
		// Unconditional SPV ProofBundle verification on live networks (zero Leaf=="" bypass — Finding #3!).
		if sig.ProofBundle == nil && e.mem != nil && e.mem.Chain().Network() != "memory" {
			return buyerVerifyResult{NowUnix: now, FailedReason: "missing SPV ProofBundle from seller"}, nil
		}
		if sig.ProofBundle != nil {
			if !strings.EqualFold(sig.ProofBundle.Leaf, chosen.Leaf) {
				return buyerVerifyResult{NowUnix: now, FailedReason: "seller SPV ProofBundle leaf mismatches signed offer leaf"}, nil
			}
			var vr contracts.VerifyResult
			if e.mem.Chain().Network() != "memory" {
				vr = contracts.VerifyFragmentWithDepth(rctx, *sig.ProofBundle, e.mem.Chain().Tracker(), e.mem.Chain().SettleDepth())
			} else {
				vr = contracts.VerifyFragmentCtx(rctx, *sig.ProofBundle, e.mem.Chain().Tracker())
				if !vr.OK && vr.Reason == "BEEFInvalid" && strings.Contains(vr.Detail, "BUMP root not valid in header chain") {
					vr = contracts.VerifyFragmentStructural(*sig.ProofBundle)
				}
			}
			if !vr.OK {
				return buyerVerifyResult{NowUnix: now, FailedReason: fmt.Sprintf("seller SPV ProofBundle invalid (%s: %s)", vr.Reason, vr.Detail)}, nil
			}
			if e.mem.Chain().Network() != "memory" && !vr.Settled {
				return buyerVerifyResult{NowUnix: now, FailedReason: "seller SPV ProofBundle is not settled in a mined block"}, nil
			}
		}
		deliveredText = string(plaintext)
		dom := "ops"
		if len(want.Domains) > 0 {
			dom = want.Domains[0]
		}
		if _, rErr := e.mem.Remember(rctx, memory.RememberInput{
			Shard:      targetShard,
			Text:       deliveredText,
			Source:     "market:" + shortPub(chosen.Pub),
			Class:      "market:" + dom,
			Tainted:    true, // F10: purchased external knowledge is ALWAYS marked tainted!
			AutoAnchor: e.mem.Chain().Network() == "memory" && e.mem.Chain().BalanceSats() > 1000,
		}); rErr != nil {
			return buyerVerifyResult{}, rErr
		}
	}

	if e.mem != nil && e.mem.DB() != nil && chosen.Pub != "" {
		_ = e.mem.DB().RecordReputation(chosen.Pub, true, chosen.PriceSats)
	}
	var dealReceipt *DealReceipt
	var dealReceiptJSON []byte
	if e.mem != nil && e.mem.Chain() != nil {
		buyerSignPub := e.mem.Chain().IdentityPubHex()
		if bPub, pErr := e.mem.Chain().SigningPubKey(rctx); pErr == nil && bPub != nil {
			buyerSignPub = hex.EncodeToString(bPub.Compressed())
		}
		var dr DealReceipt
		if sig.DealReceipt != nil {
			dr = *sig.DealReceipt
			if dr.SellerSig != "" {
				if vr := VerifyDealReceipt(dr, false); !vr.OK || !vr.SellerVerified {
					return buyerVerifyResult{NowUnix: now, FailedReason: "seller triple-entry deal receipt signature invalid: " + vr.Error}, nil
				}
			}
		} else {
			dr = DealReceipt{
				Version:       1,
				DealID:        dealID,
				OfferID:       chosen.OfferID,
				BuyerPub:      buyerSignPub,
				SellerPub:     chosen.Pub,
				Domains:       want.Domains,
				PriceSats:     chosen.PriceSats,
				WantHash:      sha256CanonicalHex(want),
				PaymentTxID:   paymentTxID,
				KeyCommitment: chosen.KeyCommitHex,
				FragmentLeaf:  chosen.Leaf,
				SettledAt:     now,
			}
			if dr.PaymentTxID == "" {
				dr.PaymentTxID = sig.ClaimTxID
			}
			if e.policy != nil {
				dr.BuyerPolicyHash = e.policy.ActivePolicyHash()
			}
			if matchAudit != nil {
				dr.MatchAuditHash = sha256CanonicalHex(matchAudit)
			}
		}
		if sErr := SignDealReceiptBuyer(rctx, buyerSignPub, e.mem.Chain().SignDigest, &dr); sErr == nil {
			dealReceipt = &dr
			dealReceiptJSON, _ = json.Marshal(dr)
			_, _, _ = CommitDealAudit(rctx, e.mem, dr, false)
		}
	}
	if e.mem != nil && e.mem.DB() != nil {
		_ = FulfillRevealOwed(e.mem.DB(), dealID)
	}
	e.persistDealRecord(store.DealRecord{
		DealID:          dealID,
		Role:            "buyer",
		Status:          "settled",
		Shard:           targetShard,
		DealReceiptJSON: dealReceiptJSON,
		PaymentTxID:     paymentTxID,
		ClaimKeyHex:     sig.ContentKeyHex,
		DeliveredText:   deliveredText,
		Error:           "",
	})
	if br := e.getBroker(); br != nil {
		br.notify("deal_settled", map[string]any{"role": "buyer", "dealId": dealID, "txid": paymentTxID, "sats": chosen.PriceSats, "shard": targetShard})
	}
	return buyerVerifyResult{NowUnix: now, DeliveredText: deliveredText, TargetShard: targetShard, DealReceipt: dealReceipt}, nil
}
