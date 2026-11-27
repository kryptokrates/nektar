package market

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/policy"
)

const (
	// DealReceiptTokenPrefix is the portable offline-verifiable prefix for bilaterally signed
	// triple-entry commercial deal receipts (distinct from nektarr1.* epistemic answer receipts).
	DealReceiptTokenPrefix = "nektardr1."

	// DealReceiptAdvisoryNote preserves Nektar's Honesty Rule on commercial evidence.
	DealReceiptAdvisoryNote = "Bilaterally signed third-entry commercial receipt binding offer, payment, BRC-369 key commitment, and delivery; proves agreed exchange terms, not factual truth of purchased text."
)

// DealReceipt is the shared, bilaterally countersigned third-entry commercial evidence
// linking buyer and seller books without requiring a shared ledger operator (triple-entry-bsv-sql).
type DealReceipt struct {
	Version            int                     `json:"v"`
	DealID             string                  `json:"dealId"`
	OfferID            string                  `json:"offerId"`
	BuyerPub           string                  `json:"buyerPub"`
	SellerPub          string                  `json:"sellerPub"`
	Domains            []string                `json:"domains,omitempty"`
	PriceSats          uint64                  `json:"priceSats"`
	WantHash           string                  `json:"wantHash,omitempty"`
	OfferDigestHash    string                  `json:"offerDigestHash,omitempty"`
	BuyerPolicyHash    string                  `json:"buyerPolicyHash,omitempty"`
	MatchAuditHash     string                  `json:"matchAuditHash,omitempty"`
	CedarDecision      *policy.DecisionReceipt `json:"cedarDecision,omitempty"`
	PaymentTxID        string                  `json:"paymentTxid"`
	PaymentOutputIndex uint32                  `json:"paymentOutputIndex,omitempty"`
	KeyCommitment      string                  `json:"keyCommitment"`
	FragmentLeaf       string                  `json:"fragmentLeaf,omitempty"`
	FieldsRoot         string                  `json:"fieldsRoot"` // MPE root over canonical receipt fields for selective audit disclosure
	SettledAt          int64                   `json:"settledAt"`
	SellerSig          string                  `json:"sellerSig,omitempty"` // low-S DER signature by SellerPub over DealReceiptDigest
	BuyerSig           string                  `json:"buyerSig,omitempty"`  // low-S DER countersignature by BuyerPub over DealReceiptDigest
	AdvisoryNote       string                  `json:"advisoryNote,omitempty"`
}

// DealReceiptVerification reports offline cryptographic validation of a triple-entry DealReceipt.
type DealReceiptVerification struct {
	OK               bool   `json:"ok"`
	Bilateral        bool   `json:"bilateral"` // true iff BOTH SellerSig and BuyerSig verified
	SellerVerified   bool   `json:"sellerVerified"`
	BuyerVerified    bool   `json:"buyerVerified"`
	FieldsRootValid  bool   `json:"fieldsRootValid"`
	CedarVerified    bool   `json:"cedarVerified,omitempty"`
	DealID           string `json:"dealId,omitempty"`
	OfferID          string `json:"offerId,omitempty"`
	PriceSats        uint64 `json:"priceSats,omitempty"`
	PaymentTxID      string `json:"paymentTxid,omitempty"`
	Error            string `json:"error,omitempty"`
	AdvisoryNote     string `json:"advisoryNote,omitempty"`
}

// CanonicalFields returns the deterministic named-field map committed into FieldsRoot (MPE).
func (r DealReceipt) CanonicalFields() map[string][]byte {
	doms := append([]string(nil), r.Domains...)
	sort.Strings(doms)
	m := map[string][]byte{
		"v":                  []byte(strconv.Itoa(r.Version)),
		"dealId":             []byte(r.DealID),
		"offerId":            []byte(r.OfferID),
		"buyerPub":           []byte(r.BuyerPub),
		"sellerPub":          []byte(r.SellerPub),
		"domains":            []byte(strings.Join(doms, ",")),
		"priceSats":          []byte(strconv.FormatUint(r.PriceSats, 10)),
		"wantHash":           []byte(r.WantHash),
		"offerDigestHash":    []byte(r.OfferDigestHash),
		"buyerPolicyHash":    []byte(r.BuyerPolicyHash),
		"matchAuditHash":     []byte(r.MatchAuditHash),
		"paymentTxid":        []byte(r.PaymentTxID),
		"paymentOutputIndex": []byte(strconv.FormatUint(uint64(r.PaymentOutputIndex), 10)),
		"keyCommitment":      []byte(r.KeyCommitment),
		"fragmentLeaf":       []byte(r.FragmentLeaf),
		"settledAt":          []byte(strconv.FormatInt(r.SettledAt, 10)),
	}
	if r.CedarDecision != nil {
		m["cedarPolicySetHash"] = []byte(r.CedarDecision.PolicySetHash)
		m["cedarDecision"] = []byte(r.CedarDecision.Decision)
	}
	return m
}

// ComputeFieldsMPE builds the Merkle Proof Entity over r's canonical commercial fields.
func (r DealReceipt) ComputeFieldsMPE() (*core.MPE, error) {
	if r.Version == 0 {
		r.Version = 1
	}
	return core.BuildMPE(r.CanonicalFields())
}

// DealReceiptDigest computes the domain-separated, length-framed digest signed by both Seller and Buyer.
func DealReceiptDigest(r DealReceipt) ([]byte, error) {
	if r.Version == 0 {
		r.Version = 1
	}
	if r.FieldsRoot == "" {
		mpe, err := r.ComputeFieldsMPE()
		if err != nil {
			return nil, err
		}
		r.FieldsRoot = mpe.RootHex
	}
	fieldsRootBytes, err := hex.DecodeString(r.FieldsRoot)
	if err != nil || len(fieldsRootBytes) != 32 {
		return nil, errors.New("deal-receipt: invalid 32-byte fieldsRoot hex")
	}
	return core.DSHA256(core.Concat(
		[]byte("nektar:deal-receipt:v1"),
		core.U64LE(uint64(r.Version)),
		core.Field([]byte(r.DealID)),
		core.Field([]byte(r.OfferID)),
		core.Field([]byte(r.BuyerPub)),
		core.Field([]byte(r.SellerPub)),
		core.U64LE(r.PriceSats),
		core.Field([]byte(r.PaymentTxID)),
		core.U64LE(uint64(r.PaymentOutputIndex)),
		core.Field([]byte(r.KeyCommitment)),
		core.Field([]byte(r.FragmentLeaf)),
		core.Field(fieldsRootBytes),
		core.U64LE(uint64(r.SettledAt)),
	)), nil
}

// FinalizeFieldsRoot computes and binds r.FieldsRoot and default Version/AdvisoryNote.
func (r *DealReceipt) FinalizeFieldsRoot() error {
	if r == nil {
		return errors.New("deal-receipt: nil receipt")
	}
	if r.Version == 0 {
		r.Version = 1
	}
	if r.AdvisoryNote == "" {
		r.AdvisoryNote = DealReceiptAdvisoryNote
	}
	mpe, err := r.ComputeFieldsMPE()
	if err != nil {
		return err
	}
	r.FieldsRoot = mpe.RootHex
	return nil
}

// SignSeller signs r using the seller's BRC-43 digest signer (preserving zero raw key exposure).
func SignDealReceiptSeller(ctx context.Context, sellerPub string, signDigest func(context.Context, []byte) ([]byte, error), r *DealReceipt) error {
	if r == nil || sellerPub == "" || signDigest == nil {
		return errors.New("deal-receipt: missing receipt, sellerPub, or signer")
	}
	if _, _, err := core.ParseStrictCompressedPubKey(sellerPub); err != nil {
		return fmt.Errorf("deal-receipt: invalid sellerPub: %w", err)
	}
	r.SellerPub = sellerPub
	if err := r.FinalizeFieldsRoot(); err != nil {
		return err
	}
	digest, err := DealReceiptDigest(*r)
	if err != nil {
		return err
	}
	sig, err := signDigest(ctx, digest)
	if err != nil {
		return err
	}
	r.SellerSig = hex.EncodeToString(sig)
	return nil
}

// SignDealReceiptBuyer countersigns r using the buyer's digest signer.
// When r already carries SellerSig, BuyerPub MUST match the seller-signed BuyerPub so countersigning
// never mutates BuyerPub/FieldsRoot and invalidates SellerSig.
func SignDealReceiptBuyer(ctx context.Context, buyerPub string, signDigest func(context.Context, []byte) ([]byte, error), r *DealReceipt) error {
	if r == nil || buyerPub == "" || signDigest == nil {
		return errors.New("deal-receipt: missing receipt, buyerPub, or signer")
	}
	if _, _, err := core.ParseStrictCompressedPubKey(buyerPub); err != nil {
		return fmt.Errorf("deal-receipt: invalid buyerPub: %w", err)
	}
	if r.SellerSig != "" && r.BuyerPub != "" && !strings.EqualFold(r.BuyerPub, buyerPub) {
		return fmt.Errorf("deal-receipt: buyerPub %s mismatches seller-signed buyerPub %s", buyerPub, r.BuyerPub)
	}
	if r.BuyerPub == "" {
		r.BuyerPub = buyerPub
	}
	if err := r.FinalizeFieldsRoot(); err != nil {
		return err
	}
	digest, err := DealReceiptDigest(*r)
	if err != nil {
		return err
	}
	sig, err := signDigest(ctx, digest)
	if err != nil {
		return err
	}
	r.BuyerSig = hex.EncodeToString(sig)
	return nil
}

// SelectiveDisclose produces an offline-verifiable MPE proof exposing ONLY the requested fields
// (e.g. "priceSats", "paymentTxid", "buyerPolicyHash", "settledAt") to an external auditor.
func (r DealReceipt) SelectiveDisclose(fields []string) (*core.SelectiveDisclosureProof, error) {
	mpe, err := r.ComputeFieldsMPE()
	if err != nil {
		return nil, err
	}
	return mpe.ProveFields(fields)
}

// EncodeDealReceiptToken serializes r into a portable `nektardr1.<base64url>` token.
func EncodeDealReceiptToken(r DealReceipt) (string, error) {
	if err := r.FinalizeFieldsRoot(); err != nil {
		return "", err
	}
	raw, err := core.CanonicalJSON(r)
	if err != nil {
		return "", err
	}
	return DealReceiptTokenPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeDealReceiptToken parses a `nektardr1.<base64url>` token back into a DealReceipt.
func DecodeDealReceiptToken(token string) (*DealReceipt, error) {
	tok := strings.TrimSpace(token)
	if !strings.HasPrefix(tok, DealReceiptTokenPrefix) {
		return nil, errors.New("deal-receipt: token must start with nektardr1.")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(tok, DealReceiptTokenPrefix))
	if err != nil {
		return nil, fmt.Errorf("deal-receipt: base64 decode: %w", err)
	}
	var r DealReceipt
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("deal-receipt: json decode: %w", err)
	}
	return &r, nil
}

// VerifyDealReceipt verifies r's MPE FieldsRoot, optional signed CedarDecision, and Seller/Buyer low-S DER signatures.
// If requireBilateral is true, both SellerSig and BuyerSig must be present and valid.
func VerifyDealReceipt(r DealReceipt, requireBilateral bool) DealReceiptVerification {
	res := DealReceiptVerification{
		DealID:       r.DealID,
		OfferID:      r.OfferID,
		PriceSats:    r.PriceSats,
		PaymentTxID:  r.PaymentTxID,
		AdvisoryNote: DealReceiptAdvisoryNote,
	}
	if r.DealID == "" || r.OfferID == "" || r.PaymentTxID == "" || r.KeyCommitment == "" {
		res.Error = "missing required commercial fields (dealId, offerId, paymentTxid, keyCommitment)"
		return res
	}
	mpe, err := r.ComputeFieldsMPE()
	if err != nil {
		res.Error = "compute MPE: " + err.Error()
		return res
	}
	if !strings.EqualFold(mpe.RootHex, r.FieldsRoot) {
		res.Error = fmt.Sprintf("fieldsRoot mismatch: got %s recomputed %s", r.FieldsRoot, mpe.RootHex)
		return res
	}
	res.FieldsRootValid = true

	if r.CedarDecision != nil {
		if err := policy.VerifyDecisionReceipt(*r.CedarDecision); err != nil {
			res.Error = "cedar decision receipt invalid: " + err.Error()
			return res
		}
		res.CedarVerified = true
	}

	digest, err := DealReceiptDigest(r)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	if r.SellerSig != "" {
		pub, _, err := core.ParseStrictCompressedPubKey(r.SellerPub)
		if err != nil {
			res.Error = "invalid sellerPub: " + err.Error()
			return res
		}
		sig, err := core.ParseStrictLowSDER(r.SellerSig)
		if err != nil {
			res.Error = "invalid sellerSig: " + err.Error()
			return res
		}
		if !sig.Verify(digest, pub) {
			res.Error = "sellerSig verification failed"
			return res
		}
		res.SellerVerified = true
	}
	if r.BuyerSig != "" {
		pub, _, err := core.ParseStrictCompressedPubKey(r.BuyerPub)
		if err != nil {
			res.Error = "invalid buyerPub: " + err.Error()
			return res
		}
		sig, err := core.ParseStrictLowSDER(r.BuyerSig)
		if err != nil {
			res.Error = "invalid buyerSig: " + err.Error()
			return res
		}
		if !sig.Verify(digest, pub) {
			res.Error = "buyerSig verification failed"
			return res
		}
		res.BuyerVerified = true
	}

	res.Bilateral = res.SellerVerified && res.BuyerVerified
	if requireBilateral && !res.Bilateral {
		res.Error = fmt.Sprintf("bilateral countersignature required (seller=%v buyer=%v)", res.SellerVerified, res.BuyerVerified)
		return res
	}
	if !res.SellerVerified && !res.BuyerVerified {
		res.Error = "receipt has neither valid sellerSig nor valid buyerSig"
		return res
	}
	res.OK = true
	return res
}

func sha256CanonicalHex(v any) string {
	b, err := core.CanonicalJSON(v)
	if err != nil || len(b) == 0 {
		return ""
	}
	return hex.EncodeToString(core.SHA256(b))
}
