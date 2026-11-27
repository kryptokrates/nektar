package reputation

import "strings"

const (
	// RoleBuyerRatesSeller identifies a buyer's post-purchase evaluation of a seller.
	RoleBuyerRatesSeller = "buyer_rates_seller"
	// RoleSellerRatesBuyer identifies a seller's bilateral evaluation of a buyer's collaboration quality.
	RoleSellerRatesBuyer = "seller_rates_buyer"

	// TokenPrefix is the canonical wire prefix for signed CBOR reputation attestations.
	TokenPrefix = "nektarrep1."

	// GlobalOntologyID is used when a deal or attestation is not pinned to a specific domain ontology.
	GlobalOntologyID = "global"

	// AdvisoryReputationNote enforces Nektar's Honesty Rule on every nektarrep1.* attestation.
	AdvisoryReputationNote = "Signed peer-observed reputation attestation bound to a verified bilateral deal receipt; proves issuer commitment and settlement linkage, never objective truth."

	// MaxBP is 100.00% in integer basis points.
	MaxBP int64 = 10000

	// DefaultHalfLifeSeconds is the 60-day exponential reputation decay half-life.
	DefaultHalfLifeSeconds int64 = 60 * 86400

	// Default deterministic trust thresholds (integer basis points & satoshis).
	DefaultMinCompositeBP   int64  = 6500
	DefaultMinAccuracyBP    int64  = 6000
	DefaultMinDisputeBP     int64  = 7500
	DefaultMinEigenTrustBP  int64  = 2000
	DefaultMinBondSats      uint64 = 10000
	DefaultPriorityCollabBP int64  = 8000
)

// Vector6D holds deterministic integer-bps (0..10000) multidimensional reputation for one
// (SubjectPub, OntologyID) pair. Zero floats in consensus/policy paths.
type Vector6D struct {
	SubjectPub   string `json:"subjectPub,omitempty"`
	OntologyID   string `json:"ontologyId,omitempty"`
	AccBP        int64  `json:"accBp"`                  // r_acc: epistemic accuracy & Mangle consistency
	ConsBP       int64  `json:"consBp"`                 // r_cons: temporal consistency across AORG revisions
	DispBP       int64  `json:"dispBp"`                 // r_disp: dispute-free settlement & bond integrity
	PredBP       int64  `json:"predBp"`                 // r_pred: predictive / downstream task utility
	CollabBP     int64  `json:"collabBp"`               // r_collab: bilateral collaboration quality (query/delivery)
	ProvBP       int64  `json:"provBp"`                 // r_prov: provenance & schema completeness (SPV, spans, bond)
	EigenTrustBP int64  `json:"eigenTrustBp,omitempty"` // personalized transitive trust mass in [0..10000]
	CompositeBP  int64  `json:"compositeBp"`            // deterministic weighted 6D composite in [0..10000]
	DirectDeals  int64  `json:"directDeals,omitempty"`
	AttestCount  int64  `json:"attestCount,omitempty"`
	UpdatedAt    int64  `json:"updatedAt,omitempty"`
}

// ClampBP bounds any integer basis-point value to [0, 10000].
func ClampBP(v int64) int64 {
	if v < 0 {
		return 0
	}
	if v > MaxBP {
		return MaxBP
	}
	return v
}

// ComputeComposite6D computes the deterministic weighted composite score in [0, 10000]:
// 30% Accuracy + 15% Consistency + 20% Dispute + 15% Predictive + 10% Collaboration + 10% Provenance.
func ComputeComposite6D(acc, cons, disp, pred, collab, prov int64) int64 {
	num := ClampBP(acc)*30 +
		ClampBP(cons)*15 +
		ClampBP(disp)*20 +
		ClampBP(pred)*15 +
		ClampBP(collab)*10 +
		ClampBP(prov)*10
	return ClampBP(num / 100)
}

// NormalizeOntologyID canonicalizes an ontology ID (empty -> "global", lowercase trimmed).
func NormalizeOntologyID(ontID string) string {
	s := strings.ToLower(strings.TrimSpace(ontID))
	if s == "" {
		return GlobalOntologyID
	}
	return s
}

// DealReceiptBinding carries the verified bilateral fields extracted from a nektardr1.* deal receipt.
// Injected via DealReceiptVerifier so internal/reputation has zero dependency on internal/market.
type DealReceiptBinding struct {
	DealID      string `json:"dealId"`
	ReceiptHash string `json:"receiptHash"`
	BuyerPub    string `json:"buyerPub"`
	SellerPub   string `json:"sellerPub"`
	OntologyID  string `json:"ontologyId,omitempty"`
	PriceSats   uint64 `json:"priceSats"`
	SettledAt   int64  `json:"settledAt"`
}

// DealReceiptVerifier verifies a serialized nektardr1.* token and returns its bilateral binding.
type DealReceiptVerifier func(dealReceiptToken string) (*DealReceiptBinding, error)

// TrustThresholds configures the deterministic integer-bps cutoffs compiled into Mangle Datalog.
type TrustThresholds struct {
	MinCompositeBP   int64
	MinAccuracyBP    int64
	MinDisputeBP     int64
	MinEigenTrustBP  int64
	MinBondSats      uint64
	PriorityCollabBP int64
}

// Normalized fills zero thresholds with safe defaults.
func (t TrustThresholds) Normalized() TrustThresholds {
	if t.MinCompositeBP <= 0 {
		t.MinCompositeBP = DefaultMinCompositeBP
	}
	if t.MinAccuracyBP <= 0 {
		t.MinAccuracyBP = DefaultMinAccuracyBP
	}
	if t.MinDisputeBP <= 0 {
		t.MinDisputeBP = DefaultMinDisputeBP
	}
	if t.MinEigenTrustBP <= 0 {
		t.MinEigenTrustBP = DefaultMinEigenTrustBP
	}
	if t.MinBondSats == 0 {
		t.MinBondSats = DefaultMinBondSats
	}
	if t.PriorityCollabBP <= 0 {
		t.PriorityCollabBP = DefaultPriorityCollabBP
	}
	return t
}

// PeerBondInfo summarizes a peer's on-chain NektarCheckpointBond state for Mangle trust rules.
type PeerBondInfo struct {
	Pub      string `json:"pub"`
	BondSats uint64 `json:"bondSats"`
	Status   string `json:"status"` // active | expired | seized | none
}

// ContradictionRecord records a verified Mangle contradiction attributed to a peer in an ontology.
type ContradictionRecord struct {
	PeerPub    string `json:"peerPub"`
	OntologyID string `json:"ontologyId"`
	Seq        int64  `json:"seq"`
}

// TrustEvaluation holds the derived Mangle stratified Datalog trust decisions for an ontology.
type TrustEvaluation struct {
	OntologyID       string          `json:"ontologyId"`
	TrustedSellers   map[string]bool `json:"trustedSellers"`
	PriorityBuyers   map[string]bool `json:"priorityBuyers"`
	SybilOrUntrusted map[string]bool `json:"sybilOrUntrusted"`
}
