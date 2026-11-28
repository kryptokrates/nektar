package market

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/contracts"
)

// refreshPendingSaleProof reuses an already-mined ProofBundle (cleared on HandleReorg)
// and only calls Prove() when unmined or absent.
func (b *Broker) refreshPendingSaleProof(ctx context.Context, s *pendingSale) *contracts.ProofBundle {
	if s == nil {
		return nil
	}
	if s.ProofBundle != nil && len(s.ProofBundle.AnchorBEEF) > 0 {
		if _, pTx, _, pErr := guard.ParseBEEF(s.ProofBundle.AnchorBEEF, guard.DefaultBound); pErr == nil && pTx != nil && pTx.MerklePath != nil {
			return s.ProofBundle
		}
	}
	if s.Shard != "" && s.Seq > 0 {
		if freshPB, vr, pErr := b.mem.Prove(ctx, s.Shard, s.Seq); pErr == nil && freshPB != nil && (vr.OK || vr.AncestryVerified) {
			s.ProofBundle = freshPB
		}
	}
	return s.ProofBundle
}

// verifyPurchasePaymentOutput verifies that tx pays >= Offer.Price via DisclosureSale on-chain escrow
// OR BRC-29 derived key (strictly required on live networks to prevent replay of historical static deposits).
func (b *Broker) verifyPurchasePaymentOutput(
	ctx context.Context,
	req PurchaseRequest,
	sale *pendingSale,
	tx *transaction.Transaction,
	nowUnix int64,
) (isEscrow bool, isBRC29 bool, paidSats uint64, escrowParams contracts.DisclosureSaleParams, err error) {
	isEscrow = req.EscrowMode
	isBRC29 = !isEscrow && req.DerivationPrefix != "" && req.DerivationSuffix != "" && req.SenderIdentityKey != ""
	if !isEscrow && !isBRC29 {
		return false, false, 0, escrowParams, errors.New("market: DisclosureSale escrow or BRC-29 payment remittance is required")
	}
	if isEscrow {
		if uint64(req.OutputIndex) >= uint64(len(tx.Outputs)) {
			return false, false, 0, escrowParams, fmt.Errorf("market: escrow outputIndex %d out of range", req.OutputIndex)
		}
		depPKH, dErr := hex.DecodeString(strings.TrimSpace(req.DepositorPKH))
		if dErr != nil || len(depPKH) != 20 {
			return false, false, 0, escrowParams, errors.New("market: escrow requires 20-byte hex depositorPkh")
		}
		sellerPKH, sErr := hex.DecodeString(strings.TrimSpace(req.EscrowSellerPKH))
		if sErr != nil || len(sellerPKH) != 20 {
			return false, false, 0, escrowParams, errors.New("market: escrow requires 20-byte hex escrowSellerPkh")
		}
		// Verify EscrowSellerPKH belongs to THIS seller (either our wallet FundPKH or our signed offer SellerPKH)!
		sellerPKHMatches := false
		if myFundPKH, fErr := b.mem.Chain().FundPKH(ctx); fErr == nil && bytes.Equal(myFundPKH, sellerPKH) {
			sellerPKHMatches = true
		}
		if offerPKH, oErr := hex.DecodeString(sale.SaleOffer.Config.SellerPKH); oErr == nil && bytes.Equal(offerPKH, sellerPKH) {
			sellerPKHMatches = true
		}
		if !sellerPKHMatches {
			return false, false, 0, escrowParams, errors.New("market: escrowSellerPkh does not belong to this seller")
		}
		hWrappedBytes, hwErr := hex.DecodeString(sale.SaleOffer.Config.HWrapped)
		if hwErr != nil || len(hWrappedBytes) != 32 {
			return false, false, 0, escrowParams, errors.New("market: seller offer has invalid hWrapped")
		}
		maxFee := req.EscrowMaxFee
		if maxFee < 100 || maxFee > 5000 {
			return false, false, 0, escrowParams, fmt.Errorf("market: escrow maxFee %d outside safe range [100, 5000]", maxFee)
		}
		refundAfter := req.EscrowRefundAfter
		if refundAfter == 0 {
			refundAfter = sale.SaleOffer.Config.RefundAfter
		}
		// Ensure RefundAfter has safe headroom so buyer cannot front-run our Buy() with an immediate Refund()!
		if refundAfter < 500_000_000 {
			if curH, hErr := b.mem.Chain().Height(ctx); hErr == nil && refundAfter <= uint64(curH)+1 {
				return false, false, 0, escrowParams, fmt.Errorf("market: escrow refundAfter height %d too close to tip %d", refundAfter, curH)
			}
		} else if int64(refundAfter) <= nowUnix+60 {
			return false, false, 0, escrowParams, fmt.Errorf("market: escrow refundAfter timestamp %d too close to now %d", refundAfter, nowUnix)
		}
		escrowParams = contracts.DisclosureSaleParams{
			SellerPKH:    sellerPKH,
			Price:        sale.SaleOffer.Config.Price,
			HWrapped:     hWrappedBytes,
			RefundAfter:  refundAfter,
			MaxFee:       maxFee,
			DepositorPKH: depPKH,
		}
		expectedEscrowLock, eErr := contracts.EncodeDisclosureSaleLockingScript(escrowParams)
		if eErr != nil {
			return false, false, 0, escrowParams, fmt.Errorf("market: encode DisclosureSale escrow: %w", eErr)
		}
		out := tx.Outputs[req.OutputIndex]
		if out == nil || out.LockingScript == nil || !bytes.Equal(*out.LockingScript, *expectedEscrowLock) {
			return false, false, 0, escrowParams, errors.New("market: escrow output locking script does not match committed DisclosureSale covenant")
		}
		if out.Satoshis < escrowParams.RequiredEscrowSatoshis() {
			return false, false, 0, escrowParams, fmt.Errorf("market: escrow balance %d < required %d (price + maxFee + 1)",
				out.Satoshis, escrowParams.RequiredEscrowSatoshis())
		}
		paidSats = sale.SaleOffer.Config.Price
	} else if isBRC29 {
		if uint64(req.OutputIndex) >= uint64(len(tx.Outputs)) {
			return false, false, 0, escrowParams, fmt.Errorf("market: BRC-29 outputIndex %d out of range", req.OutputIndex)
		}
		out := tx.Outputs[req.OutputIndex]
		if out == nil || out.LockingScript == nil {
			return false, false, 0, escrowParams, errors.New("market: BRC-29 payment output missing locking script")
		}
		if fl := b.mem.Chain().FastLane(); fl != nil {
			expectedBytes, fErr := fl.DeriveBRC29ReceiveScriptFast(req.SenderIdentityKey, req.DerivationPrefix, req.DerivationSuffix)
			if fErr != nil {
				return false, false, 0, escrowParams, fmt.Errorf("market: verify BRC-29 payment script: %w", fErr)
			}
			if !bytes.Equal(*out.LockingScript, expectedBytes) {
				return false, false, 0, escrowParams, errors.New("market: BRC-29 payment output locking script does not match derived seller key")
			}
			if out.Satoshis < sale.SaleOffer.Config.Price {
				return false, false, 0, escrowParams, fmt.Errorf("market: BRC-29 payment %d sats < required %d sats", out.Satoshis, sale.SaleOffer.Config.Price)
			}
			paidSats = out.Satoshis
			return isEscrow, isBRC29, paidSats, escrowParams, nil
		}
		d := &bwallet.Derivation{
			SecurityLevel:   int(bwallet.PaymentProtocol.SecurityLevel),
			Protocol:        bwallet.PaymentProtocol.Protocol,
			KeyID:           bwallet.PaymentKeyID(req.DerivationPrefix, req.DerivationSuffix),
			CounterpartyHex: req.SenderIdentityKey,
			OwnerHex:        b.mem.Chain().IdentityPubHex(),
		}
		wantScript, dErr := b.mem.Chain().Signer().DerivedScript(ctx, d)
		if dErr != nil {
			return false, false, 0, escrowParams, fmt.Errorf("market: derive BRC-29 script: %w", dErr)
		}
		if !bytes.Equal(*out.LockingScript, *wantScript) {
			return false, false, 0, escrowParams, errors.New("market: payment output does not match BRC-29 derivation")
		}
		paidSats = out.Satoshis
	}
	if paidSats < sale.SaleOffer.Config.Price {
		return isEscrow, isBRC29, paidSats, escrowParams, fmt.Errorf("market: insufficient payment (%d < %d sats)", paidSats, sale.SaleOffer.Config.Price)
	}
	return isEscrow, isBRC29, paidSats, escrowParams, nil
}
