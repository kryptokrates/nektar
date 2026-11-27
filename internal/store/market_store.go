package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kryptokrates/nektar/internal/nmu"
)

// DefaultSubscriptionDurationSec is the default 7-day Shard Feed subscription window.
const DefaultSubscriptionDurationSec int64 = 7 * 86400

// HostRecord tracks a configured, discovered, or user-blocked market hub host URL.
type HostRecord struct {
	URL      string `json:"url"`
	State    string `json:"state"`  // up | down | unknown
	Source   string `json:"source"` // seed | joined | dir
	Blocked  bool   `json:"blocked"`
	LastSeen int64  `json:"lastSeen"`
}

// SellListing persists a shard offered for sale on the knowledge market.
// Multi-mode by design: PriceSats > 0 (spot), QueryPriceSats > 0 (per_query), and
// SubscriptionPriceSats > 0 (subscription) operate as independent enabled contract modes on the same shard.
type SellListing struct {
	Shard                   string   `json:"shard"`
	Domains                 []string `json:"domains"`
	PriceSats               uint64   `json:"priceSats"`
	PricePerUnit            bool     `json:"pricePerUnit,omitempty"`            // when true, PriceSats is satsPer1kUnits (milli-sats per triple)
	QueryPriceSats          uint64   `json:"queryPriceSats,omitempty"`          // > 0 enables per_query BRC-105 HTTP 402 metered recall
	SubscriptionPriceSats   uint64   `json:"subscriptionPriceSats,omitempty"`   // > 0 enables subscription SSE Shard Feed & Grade S updates
	SubscriptionDurationSec int64    `json:"subscriptionDurationSec,omitempty"` // duration seconds (defaults to 7 days when SubscriptionPriceSats > 0)
	FreshnessClass          string   `json:"freshnessClass,omitempty"`          // default ephemeral | versioned | evergreen
	UsageRights             string   `json:"usageRights,omitempty"`             // inference_only | distill_resellable
	MinAdvertisedGrade      string   `json:"minAdvertisedGrade,omitempty"`      // optional minimum grade floor (C | B | A | S)
	Description             string   `json:"description,omitempty"`
	OntologyID              string   `json:"ontologyId,omitempty"`
	AllowRaw                bool     `json:"allowRaw,omitempty"`
	DynamicPricingJSON      []byte   `json:"dynamicPricingJson,omitempty"`
	Active                  bool     `json:"active"`
	CreatedAt               int64    `json:"createdAt"`
}

// EnabledModes returns all independently enabled NMU-1 pricing modes on this SellListing.
func (l SellListing) EnabledModes() []nmu.PricingMode {
	var modes []nmu.PricingMode
	if l.PriceSats > 0 {
		modes = append(modes, nmu.ModeSpot)
	}
	if l.QueryPriceSats > 0 {
		modes = append(modes, nmu.ModePerQuery)
	}
	if l.SubscriptionPriceSats > 0 {
		modes = append(modes, nmu.ModeSubscription)
	}
	if len(modes) == 0 {
		modes = []nmu.PricingMode{nmu.ModeSpot}
	}
	return modes
}

// SupportsMode reports whether this SellListing serves the requested NMU-1 contract mode.
func (l SellListing) SupportsMode(mode nmu.PricingMode) bool {
	target := nmu.EffectivePricingMode(mode)
	for _, m := range l.EnabledModes() {
		if m == target {
			return true
		}
	}
	return false
}

// EffectiveSubscriptionDurationSec returns SubscriptionDurationSec or the 7-day default.
func (l SellListing) EffectiveSubscriptionDurationSec() int64 {
	if l.SubscriptionDurationSec > 0 {
		return l.SubscriptionDurationSec
	}
	return DefaultSubscriptionDurationSec
}

// OperatorPolicyRecord persists an operator-approved market spending/selling policy.
type OperatorPolicyRecord struct {
	PolicyID     string   `json:"policyId"`
	PolicyHash   string   `json:"policyHash"`
	Roles        []string `json:"roles"` // buyer | seller
	Domains      []string `json:"domains,omitempty"`
	MaxPerDeal   uint64   `json:"maxPerDeal"`
	MaxPerDay    uint64   `json:"maxPerDay"`
	ZeroConfMax  uint64   `json:"zeroConfMax"`
	AIPolicyJSON string   `json:"aiPolicyJson,omitempty"`
	OperatorPub  string   `json:"operatorPub,omitempty"`
	OperatorSig  string   `json:"operatorSig,omitempty"`
	Active       bool     `json:"active"`
	ApprovedBy   string   `json:"approvedBy"`
	ApprovedAt   int64    `json:"approvedAt"`
	RevokedAt    int64    `json:"revokedAt,omitempty"`
}

// DealRecord persists a buyer or seller deal across restarts with optimistic CAS revision.
type DealRecord struct {
	DealID             string `json:"dealId"`
	Role               string `json:"role"`   // buyer | seller
	Status             string `json:"status"` // open | collecting | offered | approval_pending | paid | settled | failed | expired
	Shard              string `json:"shard,omitempty"`
	WantJSON           []byte `json:"wantJson,omitempty"`
	ChosenOfferJSON    []byte `json:"chosenOfferJson,omitempty"`
	MatchAuditJSON     []byte `json:"matchAuditJson,omitempty"`
	DealReceiptJSON    []byte `json:"dealReceiptJson,omitempty"`
	PaymentTxID        string `json:"paymentTxid,omitempty"`
	PaymentBEEF        []byte `json:"paymentBeef,omitempty"`
	PaymentOutputIndex uint32 `json:"paymentOutputIndex,omitempty"`
	PaymentDerivPrefix string `json:"paymentDerivPrefix,omitempty"`
	PaymentDerivSuffix string `json:"paymentDerivSuffix,omitempty"`
	PaymentSenderKey   string `json:"paymentSenderKey,omitempty"`
	ClaimKeyHex        string `json:"claimKeyHex,omitempty"`
	ChirpURL           string `json:"chirpUrl,omitempty"`
	DeliveredText      string `json:"deliveredText,omitempty"`
	CounterpartyPub    string `json:"counterpartyPub,omitempty"`
	PriceSats          uint64 `json:"priceSats"`
	IsHubSeller        bool   `json:"isHubSeller,omitempty"`
	FirstOfferMs       int64  `json:"firstOfferMs,omitempty"`
	ConfirmHeight      int64  `json:"confirmHeight,omitempty"`
	Error              string `json:"error,omitempty"`
	Revision           int64  `json:"revision"`
	CreatedAt          int64  `json:"createdAt"`
	UpdatedAt          int64  `json:"updatedAt"`
}

// Error sentinels for atomic CAS and replay-protection operations.
var (
	ErrPaymentClaimReplay   = errors.New("store: payment txid already claimed")
	ErrPaymentClaimConflict = errors.New("store: payment input outpoint already claimed by another tx")
	ErrApprovalNotPending   = errors.New("store: approval is not pending or has expired")
	ErrDealConflict         = errors.New("store: deal revision conflict")
	ErrSpendCapExceeded     = errors.New("store: spend cap exceeded")
	ErrFaucetCapExceeded    = errors.New("store: faucet cap exceeded")
)

// ApprovalRecord tracks an out-of-policy spend awaiting explicit operator consent.
type ApprovalRecord struct {
	ID           string `json:"id"`
	DealID       string `json:"dealId"`
	Reason       string `json:"reason"`
	MaxExtraSats uint64 `json:"maxExtraSats"`
	Status       string `json:"status"` // pending | approved | consumed | denied | expired
	AwakeableID  string `json:"awakeableId,omitempty"`
	ExpiresAt    int64  `json:"expiresAt"`
	CreatedAt    int64  `json:"createdAt"`
}

// SellerReputation tracks verified completed trades per seller pubkey.
type SellerReputation struct {
	SellerPub     string `json:"sellerPub"`
	SettledDeals  int64  `json:"settledDeals"`
	FailedDeals   int64  `json:"failedDeals"`
	TotalSats     uint64 `json:"totalSats"`
	LastSettledAt int64  `json:"lastSettledAt"`
}

// BorgGroupRecord persists a Borg group and its epoch keys in SQLite.
type BorgGroupRecord struct {
	GroupID          string `json:"groupId"`
	Name             string `json:"name"`
	CurrentEpoch     int64  `json:"currentEpoch"`
	MembersJSON      []byte `json:"membersJson"`
	RevokedJSON      []byte `json:"revokedJson"`
	EpochsJSON       []byte `json:"epochsJson"`
	EpochMembersJSON []byte `json:"epochMembersJson"`
	UpdatedAt        int64  `json:"updatedAt"`
}

// PendingSaleRecord persists an offered BRC-369 sale so seller restarts never lose buyer payments.
type PendingSaleRecord struct {
	OfferID         string `json:"offerId"`
	WantID          string `json:"wantId"`
	BuyerPub        string `json:"buyerPub"`
	Shard           string `json:"shard"`
	Seq             int64  `json:"seq"`
	SaleOfferJSON   []byte `json:"saleOfferJson"`
	RevealKeyHex    string `json:"revealKeyHex"`
	ChirpURL        string `json:"chirpUrl"`
	ProofBundleJSON []byte `json:"proofBundleJson,omitempty"`
	ExpiresAt       int64  `json:"expiresAt"`
	Settled         bool   `json:"settled"`
}

// PaymentClaimRecord persists claimed payment txids/inputs across restarts for replay protection.
type PaymentClaimRecord struct {
	TxID      string   `json:"txid"`
	Inputs    []string `json:"inputs"`
	ClaimedAt int64    `json:"claimedAt"`
}

// UnmatchedWantRecord captures buyer demand that received zero offers (seller analytics feed).
type UnmatchedWantRecord struct {
	WantID  string `json:"wantId"`
	Domain  string `json:"domain"`
	Query   string `json:"query"`
	MaxSats uint64 `json:"maxSats"`
	SeenAt  int64  `json:"seenAt"`
}

func (d *DB) migrateMarket() error {
	const schema = `
CREATE TABLE IF NOT EXISTS hosts (
	url       TEXT PRIMARY KEY,
	state     TEXT NOT NULL DEFAULT 'unknown',
	source    TEXT NOT NULL DEFAULT 'joined',
	blocked   INTEGER NOT NULL DEFAULT 0,
	last_seen INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS sell_listings (
	shard        TEXT PRIMARY KEY,
	domains_json TEXT NOT NULL DEFAULT '[]',
	price_sats   INTEGER NOT NULL DEFAULT 0,
	description  TEXT NOT NULL DEFAULT '',
	active       INTEGER NOT NULL DEFAULT 1,
	created_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS operator_policies (
	policy_id      TEXT PRIMARY KEY,
	policy_hash    TEXT NOT NULL,
	roles_json     TEXT NOT NULL DEFAULT '[]',
	domains_json   TEXT NOT NULL DEFAULT '[]',
	max_per_deal   INTEGER NOT NULL DEFAULT 0,
	max_per_day    INTEGER NOT NULL DEFAULT 0,
	zero_conf_max  INTEGER NOT NULL DEFAULT 0,
	ai_policy_json TEXT NOT NULL DEFAULT '',
	operator_pub   TEXT NOT NULL DEFAULT '',
	operator_sig   TEXT NOT NULL DEFAULT '',
	active         INTEGER NOT NULL DEFAULT 1,
	approved_by    TEXT NOT NULL DEFAULT 'operator',
	approved_at    INTEGER NOT NULL,
	revoked_at     INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS deals (
	deal_id              TEXT PRIMARY KEY,
	role                 TEXT NOT NULL,
	status               TEXT NOT NULL,
	shard                TEXT NOT NULL DEFAULT '',
	want_json            BLOB,
	chosen_offer_json    BLOB,
	match_audit_json     BLOB,
	deal_receipt_json    BLOB,
	payment_txid         TEXT NOT NULL DEFAULT '',
	payment_beef         BLOB,
	payment_output_index INTEGER NOT NULL DEFAULT 0,
	payment_deriv_prefix TEXT NOT NULL DEFAULT '',
	payment_deriv_suffix TEXT NOT NULL DEFAULT '',
	payment_sender_key   TEXT NOT NULL DEFAULT '',
	claim_key_hex        TEXT NOT NULL DEFAULT '',
	chirp_url            TEXT NOT NULL DEFAULT '',
	delivered_text       TEXT NOT NULL DEFAULT '',
	counterparty_pub     TEXT NOT NULL DEFAULT '',
	price_sats           INTEGER NOT NULL DEFAULT 0,
	is_hub_seller        INTEGER NOT NULL DEFAULT 0,
	first_offer_ms       INTEGER NOT NULL DEFAULT 0,
	confirm_height       INTEGER NOT NULL DEFAULT 0,
	error                TEXT NOT NULL DEFAULT '',
	revision             INTEGER NOT NULL DEFAULT 1,
	created_at           INTEGER NOT NULL,
	updated_at           INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS approvals (
	id             TEXT PRIMARY KEY,
	deal_id        TEXT NOT NULL,
	reason         TEXT NOT NULL,
	max_extra_sats INTEGER NOT NULL DEFAULT 0,
	status         TEXT NOT NULL DEFAULT 'pending',
	awakeable_id   TEXT NOT NULL DEFAULT '',
	expires_at     INTEGER NOT NULL,
	created_at     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS reputation (
	seller_pub      TEXT PRIMARY KEY,
	settled_deals   INTEGER NOT NULL DEFAULT 0,
	failed_deals    INTEGER NOT NULL DEFAULT 0,
	total_sats      INTEGER NOT NULL DEFAULT 0,
	last_settled_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS faucet_claims (
	address       TEXT PRIMARY KEY,
	total_sats    INTEGER NOT NULL DEFAULT 0,
	last_claim_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS borg_groups (
	group_id           TEXT PRIMARY KEY,
	name               TEXT NOT NULL,
	current_epoch      INTEGER NOT NULL,
	members_json       BLOB NOT NULL,
	revoked_json       BLOB NOT NULL,
	epochs_json        BLOB NOT NULL,
	epoch_members_json BLOB NOT NULL,
	updated_at         INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS pending_sales (
	offer_id          TEXT PRIMARY KEY,
	want_id           TEXT NOT NULL,
	buyer_pub         TEXT NOT NULL,
	shard             TEXT NOT NULL,
	seq               INTEGER NOT NULL,
	sale_offer_json   BLOB NOT NULL,
	reveal_key_hex    TEXT NOT NULL,
	chirp_url         TEXT NOT NULL,
	proof_bundle_json BLOB,
	expires_at        INTEGER NOT NULL,
	settled           INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS payment_claims (
	txid        TEXT PRIMARY KEY,
	inputs_json BLOB NOT NULL,
	claimed_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS payment_claim_inputs (
	outpoint   TEXT PRIMARY KEY,
	txid       TEXT NOT NULL,
	claimed_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_payment_claim_inputs_txid ON payment_claim_inputs(txid);

CREATE TABLE IF NOT EXISTS spend_reservations (
	op_id       TEXT NOT NULL,
	attempt     INTEGER NOT NULL,
	amount_sats INTEGER NOT NULL,
	status      TEXT NOT NULL DEFAULT 'reserved',
	created_at  INTEGER NOT NULL,
	PRIMARY KEY (op_id, attempt)
);

CREATE TABLE IF NOT EXISTS unmatched_wants (
	want_id  TEXT PRIMARY KEY,
	domain   TEXT NOT NULL,
	query    TEXT NOT NULL,
	max_sats INTEGER NOT NULL DEFAULT 0,
	seen_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS obligations (
	id              TEXT PRIMARY KEY,
	deal_id         TEXT NOT NULL,
	role            TEXT NOT NULL,
	kind            TEXT NOT NULL,
	counterparty    TEXT NOT NULL DEFAULT '',
	amount_sats     INTEGER NOT NULL DEFAULT 0,
	txid            TEXT NOT NULL DEFAULT '',
	deadline_unix   INTEGER NOT NULL DEFAULT 0,
	deadline_height INTEGER NOT NULL DEFAULT 0,
	status          TEXT NOT NULL DEFAULT 'open',
	payload_json    BLOB,
	attempts        INTEGER NOT NULL DEFAULT 0,
	last_error      TEXT NOT NULL DEFAULT '',
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_obligations_status ON obligations(status, deadline_unix);
`
	_, err := d.sql.Exec(schema)
	if err != nil {
		return err
	}
	var userVer int
	if err := d.sql.QueryRow(`PRAGMA user_version`).Scan(&userVer); err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}
	runVersionStep := func(target int, fn func(tx *sql.Tx) error) error {
		tx, err := d.sql.Begin()
		if err != nil {
			return fmt.Errorf("store: begin market migration v%d: %w", target, err)
		}
		defer func() { _ = tx.Rollback() }()
		if err := fn(tx); err != nil {
			return fmt.Errorf("store: market migration v%d: %w", target, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, target)); err != nil {
			return fmt.Errorf("store: set user_version=%d: %w", target, err)
		}
		return tx.Commit()
	}
	if userVer < 2 {
		if err := runVersionStep(2, func(tx *sql.Tx) error {
			cols := []struct{ table, col, def string }{
				{"deals", "shard", "TEXT NOT NULL DEFAULT ''"},
				{"deals", "payment_output_index", "INTEGER NOT NULL DEFAULT 0"},
				{"deals", "payment_deriv_prefix", "TEXT NOT NULL DEFAULT ''"},
				{"deals", "payment_deriv_suffix", "TEXT NOT NULL DEFAULT ''"},
				{"deals", "payment_sender_key", "TEXT NOT NULL DEFAULT ''"},
				{"approvals", "awakeable_id", "TEXT NOT NULL DEFAULT ''"},
			}
			for _, c := range cols {
				if err := ensureColumnTx(tx, c.table, c.col, c.def); err != nil {
					return err
				}
			}
			if userVer < 1 {
				rows, err := tx.Query(`SELECT txid, inputs_json, claimed_at FROM payment_claims`)
				if err != nil {
					return err
				}
				type legacyClaim struct {
					txid      string
					inputs    []string
					claimedAt int64
				}
				var legacy []legacyClaim
				for rows.Next() {
					var txid string
					var raw []byte
					var at int64
					if err := rows.Scan(&txid, &raw, &at); err != nil {
						_ = rows.Close()
						return err
					}
					var ins []string
					if len(raw) > 0 {
						if err := json.Unmarshal(raw, &ins); err != nil {
							_ = rows.Close()
							return fmt.Errorf("parse payment_claims inputs for %s: %w", txid, err)
						}
					}
					legacy = append(legacy, legacyClaim{txid: txid, inputs: ins, claimedAt: at})
				}
				if err := rows.Err(); err != nil {
					_ = rows.Close()
					return err
				}
				if err := rows.Close(); err != nil {
					return err
				}
				for _, c := range legacy {
					for _, op := range c.inputs {
						if op != "" {
							if _, err := tx.Exec(`INSERT OR IGNORE INTO payment_claim_inputs (outpoint, txid, claimed_at) VALUES (?, ?, ?)`, op, c.txid, c.claimedAt); err != nil {
								return err
							}
						}
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if userVer < 3 {
		if err := runVersionStep(3, func(tx *sql.Tx) error {
			if err := ensureColumnTx(tx, "operator_policies", "ai_policy_json", "TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
			return ensureColumnTx(tx, "deals", "match_audit_json", "BLOB")
		}); err != nil {
			return err
		}
	}
	if userVer < 4 {
		if err := runVersionStep(4, func(tx *sql.Tx) error {
			return ensureColumnTx(tx, "deals", "confirm_height", "INTEGER NOT NULL DEFAULT 0")
		}); err != nil {
			return err
		}
	}
	if userVer < 5 {
		if err := runVersionStep(5, func(tx *sql.Tx) error {
			if err := ensureColumnTx(tx, "operator_policies", "operator_pub", "TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
			if err := ensureColumnTx(tx, "operator_policies", "operator_sig", "TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
			return ensureColumnTx(tx, "deals", "deal_receipt_json", "BLOB")
		}); err != nil {
			return err
		}
	}
	if userVer < 6 {
		if err := runVersionStep(6, migrateDistillTx); err != nil {
			return err
		}
	}
	if userVer < 7 {
		if err := runVersionStep(7, migrateReputationTx); err != nil {
			return err
		}
	}
	if userVer < 8 {
		if err := runVersionStep(8, migrateEconomicsTx); err != nil {
			return err
		}
	}
	if userVer < 9 {
		if err := runVersionStep(9, migrateNMUTx); err != nil {
			return err
		}
	}
	if userVer < 10 {
		if err := runVersionStep(10, migrateAutonomyTx); err != nil {
			return err
		}
	}
	return nil
}
