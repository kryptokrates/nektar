package market

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

// DealAuditEntry is the canonical record committed into the __deals__ NektarAnchor system shard.
type DealAuditEntry struct {
	Kind          string `json:"kind"`
	DealID        string `json:"dealId"`
	OfferID       string `json:"offerId"`
	BuyerPub      string `json:"buyerPub"`
	SellerPub     string `json:"sellerPub"`
	PriceSats     uint64 `json:"priceSats"`
	PaymentTxID   string `json:"paymentTxid"`
	KeyCommitment string `json:"keyCommitment"`
	FieldsRoot    string `json:"fieldsRoot"`
	ReceiptSHA256 string `json:"receiptSha256"`
	Bilateral     bool   `json:"bilateral"`
	SettledAt     int64  `json:"settledAt"`
}

// PolicyAuditEntry is the canonical record committed into the __policies__ NektarAnchor system shard.
type PolicyAuditEntry struct {
	Kind        string   `json:"kind"`
	PolicyID    string   `json:"policyId"`
	PolicyHash  string   `json:"policyHash"`
	OperatorPub string   `json:"operatorPub,omitempty"`
	OperatorSig string   `json:"operatorSig,omitempty"`
	Roles       []string `json:"roles"`
	Domains     []string `json:"domains,omitempty"`
	MaxPerDeal  uint64   `json:"maxPerDeal"`
	MaxPerDay   uint64   `json:"maxPerDay"`
	ZeroConfMax uint64   `json:"zeroConfMax"`
	ApprovedBy  string   `json:"approvedBy"`
	ApprovedAt  int64    `json:"approvedAt"`
}

// CommitDealAudit commits a canonical settled DealReceipt digest into the reserved __deals__
// system shard (inheriting per-fragment prev_leaf chaining and NektarAnchor.Append() singleton anchoring).
func CommitDealAudit(ctx context.Context, mem *memory.Service, rcp DealReceipt, autoAnchor bool) (*store.FragmentRecord, *store.BatchAnchor, error) {
	if mem == nil {
		return nil, nil, errors.New("market: nil memory service for CommitDealAudit")
	}
	canon, err := core.CanonicalJSON(rcp)
	if err != nil {
		return nil, nil, err
	}
	entry := DealAuditEntry{
		Kind:          "deal_receipt_v1",
		DealID:        rcp.DealID,
		OfferID:       rcp.OfferID,
		BuyerPub:      rcp.BuyerPub,
		SellerPub:     rcp.SellerPub,
		PriceSats:     rcp.PriceSats,
		PaymentTxID:   rcp.PaymentTxID,
		KeyCommitment: rcp.KeyCommitment,
		FieldsRoot:    rcp.FieldsRoot,
		ReceiptSHA256: hex.EncodeToString(core.SHA256(canon)),
		Bilateral:     rcp.BuyerSig != "" && rcp.SellerSig != "",
		SettledAt:     rcp.SettledAt,
	}
	return mem.CommitSystemAuditRecord(ctx, memory.DealsShard, "deal_receipt", entry, autoAnchor)
}

// CommitPolicyAudit commits a canonical OperatorPolicyRecord commitment into the reserved __policies__
// system shard (inheriting per-fragment prev_leaf chaining and NektarAnchor.Append() singleton anchoring).
func CommitPolicyAudit(ctx context.Context, mem *memory.Service, rec store.OperatorPolicyRecord, autoAnchor bool) (*store.FragmentRecord, *store.BatchAnchor, error) {
	if mem == nil {
		return nil, nil, errors.New("market: nil memory service for CommitPolicyAudit")
	}
	entry := PolicyAuditEntry{
		Kind:        "operator_policy_v1",
		PolicyID:    rec.PolicyID,
		PolicyHash:  rec.PolicyHash,
		OperatorPub: rec.OperatorPub,
		OperatorSig: rec.OperatorSig,
		Roles:       rec.Roles,
		Domains:     rec.Domains,
		MaxPerDeal:  rec.MaxPerDeal,
		MaxPerDay:   rec.MaxPerDay,
		ZeroConfMax: rec.ZeroConfMax,
		ApprovedBy:  rec.ApprovedBy,
		ApprovedAt:  rec.ApprovedAt,
	}
	return mem.CommitSystemAuditRecord(ctx, memory.PoliciesShard, "operator_policy", entry, autoAnchor)
}
