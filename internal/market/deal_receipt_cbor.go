package market

import (
	"encoding/json"
	"fmt"

	"github.com/lightwebinc/bcommon/cbor"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/policy"
)

// MaxDealReceiptTokenBytes caps incoming nektardr1.* tokens before base64/CBOR decoding.
const MaxDealReceiptTokenBytes = 64 << 10

var dealReceiptKnownKeys = core.KnownKeyRange(1, 22)

// EncodeDealReceiptCBOR encodes r into pure RFC 8949 CDE CBOR (integer keys 1..22).
func EncodeDealReceiptCBOR(r DealReceipt) ([]byte, error) {
	if r.Version == 0 {
		r.Version = 1
	}
	m := cbor.Map{
		{Key: uint64(1), Val: uint64(r.Version)},
		{Key: uint64(2), Val: r.DealID},
		{Key: uint64(3), Val: r.OfferID},
		{Key: uint64(4), Val: r.BuyerPub},
		{Key: uint64(5), Val: r.SellerPub},
		{Key: uint64(7), Val: r.PriceSats},
		{Key: uint64(13), Val: r.PaymentTxID},
		{Key: uint64(14), Val: uint64(r.PaymentOutputIndex)},
		{Key: uint64(15), Val: r.KeyCommitment},
		{Key: uint64(18), Val: r.FieldsRoot},
		{Key: uint64(19), Val: r.SettledAt},
	}
	if len(r.Domains) > 0 {
		m = append(m, cbor.Pair{Key: uint64(6), Val: core.CBORStringsVal(r.Domains)})
	}
	if r.WantHash != "" {
		m = append(m, cbor.Pair{Key: uint64(8), Val: r.WantHash})
	}
	if r.OfferDigestHash != "" {
		m = append(m, cbor.Pair{Key: uint64(9), Val: r.OfferDigestHash})
	}
	if r.BuyerPolicyHash != "" {
		m = append(m, cbor.Pair{Key: uint64(10), Val: r.BuyerPolicyHash})
	}
	if r.MatchAuditHash != "" {
		m = append(m, cbor.Pair{Key: uint64(11), Val: r.MatchAuditHash})
	}
	if r.CedarDecision != nil {
		cdRaw, err := core.CanonicalJSON(r.CedarDecision)
		if err != nil {
			return nil, fmt.Errorf("deal-receipt: encode cedarDecision: %w", err)
		}
		m = append(m, cbor.Pair{Key: uint64(12), Val: cdRaw})
	}
	if r.FragmentLeaf != "" {
		m = append(m, cbor.Pair{Key: uint64(16), Val: r.FragmentLeaf})
	}
	if r.NMU != nil && !r.NMU.IsEmpty() {
		nm := cbor.Map{}
		if r.NMU.Version != "" {
			nm = append(nm, cbor.Pair{Key: uint64(1), Val: r.NMU.Version})
		}
		if r.NMU.Grade != "" {
			nm = append(nm, cbor.Pair{Key: uint64(2), Val: string(r.NMU.Grade)})
		}
		if r.NMU.UnitCount > 0 {
			nm = append(nm, cbor.Pair{Key: uint64(3), Val: uint64(r.NMU.UnitCount)})
		}
		if r.NMU.SatsPer1kUnits > 0 {
			nm = append(nm, cbor.Pair{Key: uint64(4), Val: r.NMU.SatsPer1kUnits})
		}
		if r.NMU.FreshnessClass != "" {
			nm = append(nm, cbor.Pair{Key: uint64(5), Val: string(r.NMU.FreshnessClass)})
		}
		if r.NMU.ValidUntilUnix > 0 {
			nm = append(nm, cbor.Pair{Key: uint64(6), Val: r.NMU.ValidUntilUnix})
		}
		if r.NMU.SubjectVersion != "" {
			nm = append(nm, cbor.Pair{Key: uint64(7), Val: r.NMU.SubjectVersion})
		}
		if r.NMU.UsageRights != "" {
			nm = append(nm, cbor.Pair{Key: uint64(8), Val: string(r.NMU.UsageRights)})
		}
		if r.NMU.PricingMode != "" {
			nm = append(nm, cbor.Pair{Key: uint64(9), Val: string(r.NMU.PricingMode)})
		}
		if r.NMU.SubscriptionSec > 0 {
			nm = append(nm, cbor.Pair{Key: uint64(10), Val: r.NMU.SubscriptionSec})
		}
		if r.NMU.FeedURL != "" {
			nm = append(nm, cbor.Pair{Key: uint64(11), Val: r.NMU.FeedURL})
		}
		if r.NMU.AdvisoryNote != "" {
			nm = append(nm, cbor.Pair{Key: uint64(12), Val: r.NMU.AdvisoryNote})
		}
		m = append(m, cbor.Pair{Key: uint64(17), Val: nm})
	}
	if r.SellerSig != "" {
		m = append(m, cbor.Pair{Key: uint64(20), Val: r.SellerSig})
	}
	if r.BuyerSig != "" {
		m = append(m, cbor.Pair{Key: uint64(21), Val: r.BuyerSig})
	}
	if r.AdvisoryNote != "" {
		m = append(m, cbor.Pair{Key: uint64(22), Val: r.AdvisoryNote})
	}
	return core.EncodeCBOR(m)
}

var nmuSpecKnownKeys = core.KnownKeyRange(1, 12)

// DecodeDealReceiptCBOR strictly decodes an RFC 8949 CDE CBOR DealReceipt.
func DecodeDealReceiptCBOR(raw []byte) (*DealReceipt, error) {
	v, err := core.DecodeCBOR(raw)
	if err != nil {
		return nil, fmt.Errorf("deal-receipt: decode cbor: %w", err)
	}
	m, _, err := core.SplitCBORMap(v, dealReceiptKnownKeys)
	if err != nil {
		return nil, fmt.Errorf("deal-receipt: map: %w", err)
	}
	var r DealReceipt
	ver, err := core.CBORUint(m, 1)
	if err != nil {
		return nil, err
	}
	r.Version = int(ver)
	if r.DealID, err = core.CBORText(m, 2); err != nil {
		return nil, err
	}
	if r.OfferID, err = core.CBORText(m, 3); err != nil {
		return nil, err
	}
	if r.BuyerPub, err = core.CBORText(m, 4); err != nil {
		return nil, err
	}
	if r.SellerPub, err = core.CBORText(m, 5); err != nil {
		return nil, err
	}
	if r.Domains, err = core.CBORStrings(m, 6); err != nil {
		return nil, err
	}
	if r.PriceSats, err = core.CBORUint(m, 7); err != nil {
		return nil, err
	}
	if r.WantHash, err = core.CBORText(m, 8); err != nil {
		return nil, err
	}
	if r.OfferDigestHash, err = core.CBORText(m, 9); err != nil {
		return nil, err
	}
	if r.BuyerPolicyHash, err = core.CBORText(m, 10); err != nil {
		return nil, err
	}
	if r.MatchAuditHash, err = core.CBORText(m, 11); err != nil {
		return nil, err
	}
	if cdRaw, err := core.CBORBytes(m, 12); err != nil {
		return nil, err
	} else if len(cdRaw) > 0 {
		var cd policy.DecisionReceipt
		if err := json.Unmarshal(cdRaw, &cd); err != nil {
			return nil, fmt.Errorf("deal-receipt: decode cedarDecision: %w", err)
		}
		r.CedarDecision = &cd
	}
	if r.PaymentTxID, err = core.CBORText(m, 13); err != nil {
		return nil, err
	}
	poi, err := core.CBORUint(m, 14)
	if err != nil {
		return nil, err
	}
	r.PaymentOutputIndex = uint32(poi)
	if r.KeyCommitment, err = core.CBORText(m, 15); err != nil {
		return nil, err
	}
	if r.FragmentLeaf, err = core.CBORText(m, 16); err != nil {
		return nil, err
	}
	if nv, ok := m[17]; ok {
		nm, _, nErr := core.SplitCBORMap(nv, nmuSpecKnownKeys)
		if nErr != nil {
			return nil, fmt.Errorf("deal-receipt: nmu map: %w", nErr)
		}
		sp := &nmu.Spec{}
		if sp.Version, err = core.CBORText(nm, 1); err != nil {
			return nil, err
		}
		if g, err := core.CBORText(nm, 2); err != nil {
			return nil, err
		} else {
			sp.Grade = nmu.Grade(g)
		}
		if uc, err := core.CBORUint(nm, 3); err != nil {
			return nil, err
		} else {
			sp.UnitCount = uint32(uc)
		}
		if sp.SatsPer1kUnits, err = core.CBORUint(nm, 4); err != nil {
			return nil, err
		}
		if fc, err := core.CBORText(nm, 5); err != nil {
			return nil, err
		} else {
			sp.FreshnessClass = nmu.FreshnessClass(fc)
		}
		if sp.ValidUntilUnix, err = core.CBORInt(nm, 6); err != nil {
			return nil, err
		}
		if sp.SubjectVersion, err = core.CBORText(nm, 7); err != nil {
			return nil, err
		}
		if ur, err := core.CBORText(nm, 8); err != nil {
			return nil, err
		} else {
			sp.UsageRights = nmu.UsageRights(ur)
		}
		if pm, err := core.CBORText(nm, 9); err != nil {
			return nil, err
		} else {
			sp.PricingMode = nmu.PricingMode(pm)
		}
		if sp.SubscriptionSec, err = core.CBORInt(nm, 10); err != nil {
			return nil, err
		}
		if sp.FeedURL, err = core.CBORText(nm, 11); err != nil {
			return nil, err
		}
		if sp.AdvisoryNote, err = core.CBORText(nm, 12); err != nil {
			return nil, err
		}
		r.NMU = sp
	}
	if r.FieldsRoot, err = core.CBORText(m, 18); err != nil {
		return nil, err
	}
	if r.SettledAt, err = core.CBORInt(m, 19); err != nil {
		return nil, err
	}
	if r.SellerSig, err = core.CBORText(m, 20); err != nil {
		return nil, err
	}
	if r.BuyerSig, err = core.CBORText(m, 21); err != nil {
		return nil, err
	}
	if r.AdvisoryNote, err = core.CBORText(m, 22); err != nil {
		return nil, err
	}
	return &r, nil
}
