package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// HostRecord tracks a configured, discovered, or user-blocked market hub host URL.
type HostRecord struct {
	URL      string `json:"url"`
	State    string `json:"state"`  // up | down | unknown
	Source   string `json:"source"` // seed | joined | dir
	Blocked  bool   `json:"blocked"`
	LastSeen int64  `json:"lastSeen"`
}

// SellListing persists a shard offered for sale on the knowledge market.
type SellListing struct {
	Shard       string   `json:"shard"`
	Domains     []string `json:"domains"`
	PriceSats   uint64   `json:"priceSats"`
	Description string   `json:"description,omitempty"`
	Active      bool     `json:"active"`
	CreatedAt   int64    `json:"createdAt"`
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

// FaucetClaim tracks per-address testnet faucet drips to prevent drain abuse.
type FaucetClaim struct {
	Address     string `json:"address"`
	TotalSats   uint64 `json:"totalSats"`
	LastClaimAt int64  `json:"lastClaimAt"`
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
	policy_id     TEXT PRIMARY KEY,
	policy_hash   TEXT NOT NULL,
	roles_json    TEXT NOT NULL DEFAULT '[]',
	domains_json  TEXT NOT NULL DEFAULT '[]',
	max_per_deal  INTEGER NOT NULL DEFAULT 0,
	max_per_day   INTEGER NOT NULL DEFAULT 0,
	zero_conf_max INTEGER NOT NULL DEFAULT 0,
	active        INTEGER NOT NULL DEFAULT 1,
	approved_by   TEXT NOT NULL DEFAULT 'operator',
	approved_at   INTEGER NOT NULL,
	revoked_at    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS deals (
	deal_id           TEXT PRIMARY KEY,
	role              TEXT NOT NULL,
	status            TEXT NOT NULL,
	want_json         BLOB,
	chosen_offer_json BLOB,
	payment_txid      TEXT NOT NULL DEFAULT '',
	payment_beef      BLOB,
	claim_key_hex     TEXT NOT NULL DEFAULT '',
	chirp_url         TEXT NOT NULL DEFAULT '',
	delivered_text    TEXT NOT NULL DEFAULT '',
	counterparty_pub  TEXT NOT NULL DEFAULT '',
	price_sats        INTEGER NOT NULL DEFAULT 0,
	is_hub_seller     INTEGER NOT NULL DEFAULT 0,
	first_offer_ms    INTEGER NOT NULL DEFAULT 0,
	error             TEXT NOT NULL DEFAULT '',
	revision          INTEGER NOT NULL DEFAULT 1,
	created_at        INTEGER NOT NULL,
	updated_at        INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS approvals (
	id             TEXT PRIMARY KEY,
	deal_id        TEXT NOT NULL,
	reason         TEXT NOT NULL,
	max_extra_sats INTEGER NOT NULL DEFAULT 0,
	status         TEXT NOT NULL DEFAULT 'pending',
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
	_ = d.sql.QueryRow(`PRAGMA user_version`).Scan(&userVer)
	if userVer < 2 {
		_, _ = d.sql.Exec(`ALTER TABLE deals ADD COLUMN shard TEXT NOT NULL DEFAULT ''`)
		_, _ = d.sql.Exec(`ALTER TABLE deals ADD COLUMN payment_output_index INTEGER NOT NULL DEFAULT 0`)
		_, _ = d.sql.Exec(`ALTER TABLE deals ADD COLUMN payment_deriv_prefix TEXT NOT NULL DEFAULT ''`)
		_, _ = d.sql.Exec(`ALTER TABLE deals ADD COLUMN payment_deriv_suffix TEXT NOT NULL DEFAULT ''`)
		_, _ = d.sql.Exec(`ALTER TABLE deals ADD COLUMN payment_sender_key TEXT NOT NULL DEFAULT ''`)
		_, _ = d.sql.Exec(`ALTER TABLE approvals ADD COLUMN awakeable_id TEXT NOT NULL DEFAULT ''`)
		// One-time transactional backfill of payment_claim_inputs for legacy payment_claims rows.
		if userVer < 1 {
			if tx, bErr := d.sql.Begin(); bErr == nil {
				rows, qErr := tx.Query(`SELECT txid, inputs_json, claimed_at FROM payment_claims`)
				if qErr == nil {
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
						if rows.Scan(&txid, &raw, &at) == nil {
							var ins []string
							_ = json.Unmarshal(raw, &ins)
							legacy = append(legacy, legacyClaim{txid: txid, inputs: ins, claimedAt: at})
						}
					}
					_ = rows.Close()
					for _, c := range legacy {
						for _, op := range c.inputs {
							if op != "" {
								_, _ = tx.Exec(`INSERT OR IGNORE INTO payment_claim_inputs (outpoint, txid, claimed_at) VALUES (?, ?, ?)`, op, c.txid, c.claimedAt)
							}
						}
					}
				}
				_ = tx.Commit()
			}
		}
		_, _ = d.sql.Exec(`PRAGMA user_version = 2`)
	}
	if userVer < 3 {
		_, _ = d.sql.Exec(`ALTER TABLE operator_policies ADD COLUMN ai_policy_json TEXT NOT NULL DEFAULT ''`)
		_, _ = d.sql.Exec(`ALTER TABLE deals ADD COLUMN match_audit_json BLOB`)
		_, _ = d.sql.Exec(`PRAGMA user_version = 3`)
	}
	if userVer < 4 {
		_, _ = d.sql.Exec(`ALTER TABLE deals ADD COLUMN confirm_height INTEGER NOT NULL DEFAULT 0`)
		_, _ = d.sql.Exec(`PRAGMA user_version = 4`)
	}
	if userVer < 5 {
		_, _ = d.sql.Exec(`ALTER TABLE operator_policies ADD COLUMN operator_pub TEXT NOT NULL DEFAULT ''`)
		_, _ = d.sql.Exec(`ALTER TABLE operator_policies ADD COLUMN operator_sig TEXT NOT NULL DEFAULT ''`)
		_, _ = d.sql.Exec(`ALTER TABLE deals ADD COLUMN deal_receipt_json BLOB`)
		_, _ = d.sql.Exec(`PRAGMA user_version = 5`)
	}
	return nil
}

// ListReceipts returns committed receipts newest first.
func (d *DB) ListReceipts(limit int) ([]ReceiptRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.sql.Query(`SELECT receipt_hash, shard_seq, body_json, anchor_txid, created_at FROM receipts ORDER BY created_at DESC, shard_seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReceiptRecord
	for rows.Next() {
		var r ReceiptRecord
		if err := rows.Scan(&r.ReceiptHash, &r.ShardSeq, &r.BodyJSON, &r.AnchorTxID, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountFragments returns total fragments and settled-anchored (mined BUMP) fragments across non-system shards.
func (d *DB) CountFragments() (total int, anchored int, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	err = d.sql.QueryRow(`
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN EXISTS (
				SELECT 1 FROM anchors a
				WHERE a.shard = fragments.shard AND a.n = fragments.batch_n AND a.settled = 1
			) THEN 1 ELSE 0 END), 0)
		FROM fragments
		WHERE shard != '__receipts__' AND forgotten = 0`).
		Scan(&total, &anchored)
	return total, anchored, err
}

// UpsertHost saves or updates a market host record.
func (d *DB) UpsertHost(h HostRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if h.State == "" {
		h.State = "unknown"
	}
	if h.Source == "" {
		h.Source = "joined"
	}
	if h.LastSeen == 0 {
		h.LastSeen = time.Now().Unix()
	}
	_, err := d.sql.Exec(`
		INSERT INTO hosts (url, state, source, blocked, last_seen) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(url) DO UPDATE SET state=excluded.state, source=excluded.source, blocked=excluded.blocked, last_seen=excluded.last_seen`,
		h.URL, h.State, h.Source, boolToInt(h.Blocked), h.LastSeen)
	return err
}

// UpdateHostHealth updates state and last_seen ONLY if the host is not user-blocked.
func (d *DB) UpdateHostHealth(url, state string, lastSeen int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if lastSeen == 0 {
		lastSeen = time.Now().Unix()
	}
	_, err := d.sql.Exec(`UPDATE hosts SET state = ?, last_seen = ? WHERE url = ? AND blocked = 0`, state, lastSeen, url)
	return err
}

// BlockHost marks a host URL as explicitly disconnected/blocked so seed restarts don't re-add it.
func (d *DB) BlockHost(url string) error {
	return d.UpsertHost(HostRecord{URL: url, State: "down", Source: "blocked", Blocked: true, LastSeen: time.Now().Unix()})
}

// DeleteHost removes a host URL from persisted storage.
func (d *DB) DeleteHost(url string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`DELETE FROM hosts WHERE url = ?`, url)
	return err
}

// ListHosts returns all persisted hosts ordered by URL.
func (d *DB) ListHosts() ([]HostRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`SELECT url, state, source, blocked, last_seen FROM hosts ORDER BY url ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostRecord
	for rows.Next() {
		var h HostRecord
		var blk int
		if err := rows.Scan(&h.URL, &h.State, &h.Source, &blk, &h.LastSeen); err != nil {
			return nil, err
		}
		h.Blocked = blk != 0
		out = append(out, h)
	}
	return out, rows.Err()
}

// SaveSellListing creates or updates a shard market listing.
func (d *DB) SaveSellListing(l SellListing) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if l.CreatedAt == 0 {
		l.CreatedAt = time.Now().Unix()
	}
	doms, _ := json.Marshal(l.Domains)
	_, err := d.sql.Exec(`
		INSERT OR REPLACE INTO sell_listings (shard, domains_json, price_sats, description, active, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		l.Shard, string(doms), l.PriceSats, l.Description, boolToInt(l.Active), l.CreatedAt)
	return err
}

// ListSellListings returns all persisted sell listings.
func (d *DB) ListSellListings() ([]SellListing, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`SELECT shard, domains_json, price_sats, description, active, created_at FROM sell_listings ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SellListing
	for rows.Next() {
		var l SellListing
		var domJSON string
		var active int
		if err := rows.Scan(&l.Shard, &domJSON, &l.PriceSats, &l.Description, &active, &l.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(domJSON), &l.Domains)
		l.Active = active != 0
		out = append(out, l)
	}
	return out, rows.Err()
}

// SaveOperatorPolicy persists an approved operator policy.
func (d *DB) SaveOperatorPolicy(p OperatorPolicyRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p.ApprovedAt == 0 {
		p.ApprovedAt = time.Now().Unix()
	}
	roles, _ := json.Marshal(p.Roles)
	doms, _ := json.Marshal(p.Domains)
	_, err := d.sql.Exec(`
		INSERT OR REPLACE INTO operator_policies
		(policy_id, policy_hash, roles_json, domains_json, max_per_deal, max_per_day, zero_conf_max, ai_policy_json, operator_pub, operator_sig, active, approved_by, approved_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.PolicyID, p.PolicyHash, string(roles), string(doms), p.MaxPerDeal, p.MaxPerDay, p.ZeroConfMax, p.AIPolicyJSON,
		p.OperatorPub, p.OperatorSig, boolToInt(p.Active), p.ApprovedBy, p.ApprovedAt, p.RevokedAt)
	return err
}

// RevokeOperatorPolicy marks a policy revoked.
func (d *DB) RevokeOperatorPolicy(policyID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`UPDATE operator_policies SET active = 0, revoked_at = ? WHERE policy_id = ?`, time.Now().Unix(), policyID)
	return err
}

// ListOperatorPolicies returns all persisted policies newest first.
func (d *DB) ListOperatorPolicies() ([]OperatorPolicyRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT policy_id, policy_hash, roles_json, domains_json, max_per_deal, max_per_day, zero_conf_max, COALESCE(ai_policy_json, ''), COALESCE(operator_pub, ''), COALESCE(operator_sig, ''), active, approved_by, approved_at, revoked_at
		FROM operator_policies ORDER BY approved_at DESC, rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OperatorPolicyRecord
	for rows.Next() {
		var p OperatorPolicyRecord
		var rolesJSON, domJSON string
		var active int
		if err := rows.Scan(&p.PolicyID, &p.PolicyHash, &rolesJSON, &domJSON, &p.MaxPerDeal, &p.MaxPerDay, &p.ZeroConfMax, &p.AIPolicyJSON, &p.OperatorPub, &p.OperatorSig, &active, &p.ApprovedBy, &p.ApprovedAt, &p.RevokedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(rolesJSON), &p.Roles)
		_ = json.Unmarshal([]byte(domJSON), &p.Domains)
		p.Active = active != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// SaveDeal persists a DealRecord.
func (d *DB) SaveDeal(rec DealRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().Unix()
	if rec.CreatedAt == 0 {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	if rec.Revision == 0 {
		rec.Revision = 1
	}
	_, err := d.sql.Exec(`
		INSERT OR REPLACE INTO deals
		(deal_id, role, status, shard, want_json, chosen_offer_json, match_audit_json, deal_receipt_json, payment_txid, payment_beef,
		 payment_output_index, payment_deriv_prefix, payment_deriv_suffix, payment_sender_key,
		 claim_key_hex, chirp_url, delivered_text, counterparty_pub, price_sats, is_hub_seller, first_offer_ms, confirm_height, error, revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.DealID, rec.Role, rec.Status, rec.Shard, rec.WantJSON, rec.ChosenOfferJSON, rec.MatchAuditJSON, rec.DealReceiptJSON, rec.PaymentTxID, rec.PaymentBEEF,
		rec.PaymentOutputIndex, rec.PaymentDerivPrefix, rec.PaymentDerivSuffix, rec.PaymentSenderKey,
		rec.ClaimKeyHex, rec.ChirpURL, rec.DeliveredText, rec.CounterpartyPub, rec.PriceSats,
		boolToInt(rec.IsHubSeller), rec.FirstOfferMs, rec.ConfirmHeight, rec.Error, rec.Revision, rec.CreatedAt, rec.UpdatedAt)
	return err
}

// UpdateDealCAS performs an optimistic compare-and-swap update on a DealRecord,
// incrementing revision only if the persisted revision matches rec.Revision.
func (d *DB) UpdateDealCAS(rec DealRecord) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().Unix()
	res, err := d.sql.Exec(`
		UPDATE deals SET
			role = ?, status = ?, shard = ?, want_json = ?, chosen_offer_json = ?, match_audit_json = ?, deal_receipt_json = ?,
			payment_txid = ?, payment_beef = ?,
			payment_output_index = ?, payment_deriv_prefix = ?, payment_deriv_suffix = ?, payment_sender_key = ?,
			claim_key_hex = ?, chirp_url = ?,
			delivered_text = ?, counterparty_pub = ?, price_sats = ?, is_hub_seller = ?,
			first_offer_ms = ?, confirm_height = ?, error = ?, revision = revision + 1, updated_at = ?
		WHERE deal_id = ? AND revision = ?`,
		rec.Role, rec.Status, rec.Shard, rec.WantJSON, rec.ChosenOfferJSON, rec.MatchAuditJSON, rec.DealReceiptJSON,
		rec.PaymentTxID, rec.PaymentBEEF,
		rec.PaymentOutputIndex, rec.PaymentDerivPrefix, rec.PaymentDerivSuffix, rec.PaymentSenderKey,
		rec.ClaimKeyHex, rec.ChirpURL,
		rec.DeliveredText, rec.CounterpartyPub, rec.PriceSats, boolToInt(rec.IsHubSeller),
		rec.FirstOfferMs, rec.ConfirmHeight, rec.Error, now, rec.DealID, rec.Revision)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RollbackDealsFromHeight reopens settled deals whose confirmation block was orphaned by a chain reorg.
func (d *DB) RollbackDealsFromHeight(forkHeight int64) (int64, error) {
	if forkHeight <= 0 {
		return 0, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.sql.Exec(`
		UPDATE deals
		SET status = 'paid', confirm_height = 0, revision = revision + 1, updated_at = ?
		WHERE status = 'settled' AND confirm_height >= ?`,
		time.Now().Unix(), forkHeight)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetDeal fetches one deal by dealId.
func (d *DB) GetDeal(dealID string) (DealRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	row := d.sql.QueryRow(`
		SELECT deal_id, role, status, shard, want_json, chosen_offer_json, match_audit_json, deal_receipt_json, payment_txid, payment_beef,
		       COALESCE(payment_output_index, 0), COALESCE(payment_deriv_prefix, ''), COALESCE(payment_deriv_suffix, ''), COALESCE(payment_sender_key, ''),
		       claim_key_hex, chirp_url, delivered_text, counterparty_pub, price_sats, is_hub_seller, first_offer_ms, COALESCE(confirm_height, 0), error, revision, created_at, updated_at
		FROM deals WHERE deal_id = ?`, dealID)
	var r DealRecord
	var hub int
	err := row.Scan(&r.DealID, &r.Role, &r.Status, &r.Shard, &r.WantJSON, &r.ChosenOfferJSON, &r.MatchAuditJSON, &r.DealReceiptJSON, &r.PaymentTxID, &r.PaymentBEEF,
		&r.PaymentOutputIndex, &r.PaymentDerivPrefix, &r.PaymentDerivSuffix, &r.PaymentSenderKey,
		&r.ClaimKeyHex, &r.ChirpURL, &r.DeliveredText, &r.CounterpartyPub, &r.PriceSats, &hub, &r.FirstOfferMs, &r.ConfirmHeight,
		&r.Error, &r.Revision, &r.CreatedAt, &r.UpdatedAt)
	r.IsHubSeller = hub != 0
	return r, err
}

// ListDeals returns persisted deals newest first.
func (d *DB) ListDeals(limit int) ([]DealRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.sql.Query(`
		SELECT deal_id, role, status, shard, want_json, chosen_offer_json, match_audit_json, deal_receipt_json, payment_txid, payment_beef,
		       COALESCE(payment_output_index, 0), COALESCE(payment_deriv_prefix, ''), COALESCE(payment_deriv_suffix, ''), COALESCE(payment_sender_key, ''),
		       claim_key_hex, chirp_url, delivered_text, counterparty_pub, price_sats, is_hub_seller, first_offer_ms, COALESCE(confirm_height, 0), error, revision, created_at, updated_at
		FROM deals ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DealRecord
	for rows.Next() {
		var r DealRecord
		var hub int
		if err := rows.Scan(&r.DealID, &r.Role, &r.Status, &r.Shard, &r.WantJSON, &r.ChosenOfferJSON, &r.MatchAuditJSON, &r.DealReceiptJSON, &r.PaymentTxID, &r.PaymentBEEF,
			&r.PaymentOutputIndex, &r.PaymentDerivPrefix, &r.PaymentDerivSuffix, &r.PaymentSenderKey,
			&r.ClaimKeyHex, &r.ChirpURL, &r.DeliveredText, &r.CounterpartyPub, &r.PriceSats, &hub, &r.FirstOfferMs, &r.ConfirmHeight,
			&r.Error, &r.Revision, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.IsHubSeller = hub != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountDeals returns total persisted deals in O(1) without loading payment_beef BLOBs.
func (d *DB) CountDeals() (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var n int64
	err := d.sql.QueryRow(`SELECT COUNT(1) FROM deals`).Scan(&n)
	return n, err
}

// CountReceipts returns total persisted receipts in O(1) without loading body_json BLOBs.
func (d *DB) CountReceipts() (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var n int64
	err := d.sql.QueryRow(`SELECT COUNT(1) FROM receipts`).Scan(&n)
	return n, err
}

// SaveApproval saves or updates a pending operator approval escalation.
func (d *DB) SaveApproval(a ApprovalRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().Unix()
	}
	_, err := d.sql.Exec(`
		INSERT OR REPLACE INTO approvals (id, deal_id, reason, max_extra_sats, status, awakeable_id, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.DealID, a.Reason, a.MaxExtraSats, a.Status, a.AwakeableID, a.ExpiresAt, a.CreatedAt)
	return err
}

// GetApproval fetches one approval by ID.
func (d *DB) GetApproval(id string) (ApprovalRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var a ApprovalRecord
	err := d.sql.QueryRow(`SELECT id, deal_id, reason, max_extra_sats, status, COALESCE(awakeable_id,''), expires_at, created_at FROM approvals WHERE id = ?`, id).
		Scan(&a.ID, &a.DealID, &a.Reason, &a.MaxExtraSats, &a.Status, &a.AwakeableID, &a.ExpiresAt, &a.CreatedAt)
	return a, err
}

// ListPendingApprovals returns all unexpired pending operator approvals.
func (d *DB) ListPendingApprovals() ([]ApprovalRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT id, deal_id, reason, max_extra_sats, status, COALESCE(awakeable_id,''), expires_at, created_at
		FROM approvals WHERE status = 'pending' AND expires_at > ? ORDER BY created_at ASC`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApprovalRecord
	for rows.Next() {
		var a ApprovalRecord
		if err := rows.Scan(&a.ID, &a.DealID, &a.Reason, &a.MaxExtraSats, &a.Status, &a.AwakeableID, &a.ExpiresAt, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DecideApproval atomically transitions a pending unexpired approval to approved or denied (strict CAS).
func (d *DB) DecideApproval(id string, approve bool) (ApprovalRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	status := "denied"
	if approve {
		status = "approved"
	}
	now := time.Now().Unix()
	res, err := d.sql.Exec(`UPDATE approvals SET status = ? WHERE id = ? AND status = 'pending' AND expires_at > ?`, status, id, now)
	if err != nil {
		return ApprovalRecord{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ApprovalRecord{}, ErrApprovalNotPending
	}
	var a ApprovalRecord
	err = d.sql.QueryRow(`SELECT id, deal_id, reason, max_extra_sats, status, COALESCE(awakeable_id,''), expires_at, created_at FROM approvals WHERE id = ?`, id).
		Scan(&a.ID, &a.DealID, &a.Reason, &a.MaxExtraSats, &a.Status, &a.AwakeableID, &a.ExpiresAt, &a.CreatedAt)
	return a, err
}

// ConsumeApprovalOverride atomically consumes a single-use approved override ID so it cannot be replayed.
func (d *DB) ConsumeApprovalOverride(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.sql.Exec(`UPDATE approvals SET status = 'consumed' WHERE id = ? AND status = 'approved' AND expires_at > ?`, id, time.Now().Unix())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrApprovalNotPending
	}
	return nil
}

// MarkApprovalResolvedAwakeable atomically transitions a pending/approved awakeable approval directly
// to 'consumed' (if approved) or 'denied' (if denied) so it can never be replayed via ApprovedOverrideID.
func (d *DB) MarkApprovalResolvedAwakeable(id string, approve bool) (ApprovalRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	status := "denied"
	if approve {
		status = "consumed"
	}
	now := time.Now().Unix()
	res, err := d.sql.Exec(`UPDATE approvals SET status = ? WHERE id = ? AND status IN ('pending','approved') AND expires_at > ?`, status, id, now)
	if err != nil {
		return ApprovalRecord{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ApprovalRecord{}, ErrApprovalNotPending
	}
	var a ApprovalRecord
	err = d.sql.QueryRow(`SELECT id, deal_id, reason, max_extra_sats, status, COALESCE(awakeable_id,''), expires_at, created_at FROM approvals WHERE id = ?`, id).
		Scan(&a.ID, &a.DealID, &a.Reason, &a.MaxExtraSats, &a.Status, &a.AwakeableID, &a.ExpiresAt, &a.CreatedAt)
	return a, err
}

// RecordReputation updates verified trade counters for a seller pubkey.
func (d *DB) RecordReputation(sellerPub string, settled bool, sats uint64) error {
	if sellerPub == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().Unix()
	var sInc, fInc int64
	if settled {
		sInc = 1
	} else {
		fInc = 1
	}
	_, err := d.sql.Exec(`
		INSERT INTO reputation (seller_pub, settled_deals, failed_deals, total_sats, last_settled_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(seller_pub) DO UPDATE SET
			settled_deals = settled_deals + excluded.settled_deals,
			failed_deals  = failed_deals  + excluded.failed_deals,
			total_sats    = total_sats    + excluded.total_sats,
			last_settled_at = CASE WHEN excluded.settled_deals > 0 THEN excluded.last_settled_at ELSE last_settled_at END`,
		sellerPub, sInc, fInc, sats, now)
	return err
}

// GetReputation fetches a seller's verified reputation row.
func (d *DB) GetReputation(sellerPub string) (SellerReputation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var r SellerReputation
	err := d.sql.QueryRow(`SELECT seller_pub, settled_deals, failed_deals, total_sats, last_settled_at FROM reputation WHERE seller_pub = ?`, sellerPub).
		Scan(&r.SellerPub, &r.SettledDeals, &r.FailedDeals, &r.TotalSats, &r.LastSettledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SellerReputation{SellerPub: sellerPub}, nil
	}
	return r, err
}

// ListReputations returns all known sellers ordered by settled deals desc.
func (d *DB) ListReputations() ([]SellerReputation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`SELECT seller_pub, settled_deals, failed_deals, total_sats, last_settled_at FROM reputation ORDER BY settled_deals DESC, total_sats DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SellerReputation
	for rows.Next() {
		var r SellerReputation
		if err := rows.Scan(&r.SellerPub, &r.SettledDeals, &r.FailedDeals, &r.TotalSats, &r.LastSettledAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetFaucetClaim loads the last faucet claim for an address.
func (d *DB) GetFaucetClaim(address string) (FaucetClaim, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var c FaucetClaim
	err := d.sql.QueryRow(`SELECT address, total_sats, last_claim_at FROM faucet_claims WHERE address = ?`, address).
		Scan(&c.Address, &c.TotalSats, &c.LastClaimAt)
	if errors.Is(err, sql.ErrNoRows) {
		return FaucetClaim{Address: address}, nil
	}
	return c, err
}

// RecordFaucetClaim records a completed testnet faucet drip to address,
// resetting the per-address 24h rolling accumulator once 86400s have elapsed.
func (d *DB) RecordFaucetClaim(address string, sats uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().Unix()
	_, err := d.sql.Exec(`
		INSERT INTO faucet_claims (address, total_sats, last_claim_at) VALUES (?, ?, ?)
		ON CONFLICT(address) DO UPDATE SET
			total_sats = CASE WHEN excluded.last_claim_at - faucet_claims.last_claim_at >= 86400
				THEN excluded.total_sats
				ELSE faucet_claims.total_sats + excluded.total_sats END,
			last_claim_at = excluded.last_claim_at`,
		address, sats, now)
	return err
}

// DeleteSellListing removes a shard listing from the market.
func (d *DB) DeleteSellListing(shard string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`DELETE FROM sell_listings WHERE shard = ?`, shard)
	return err
}

// SetSellListingActive toggles a shard listing's active state.
func (d *DB) SetSellListingActive(shard string, active bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`UPDATE sell_listings SET active = ? WHERE shard = ?`, boolToInt(active), shard)
	return err
}

// SellerShardStats computes settled deal count and earned sats for a sold shard.
func (d *DB) SellerShardStats(shard string) (works int64, earned uint64, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var total int64
	err = d.sql.QueryRow(`SELECT COUNT(*), COALESCE(SUM(price_sats), 0) FROM deals WHERE role = 'seller' AND status = 'settled' AND shard = ?`, shard).
		Scan(&works, &total)
	if total > 0 {
		earned = uint64(total)
	}
	return works, earned, err
}

// SaveBorgGroup persists a Borg group and its epoch symmetric keys.
func (d *DB) SaveBorgGroup(g BorgGroupRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if g.UpdatedAt == 0 {
		g.UpdatedAt = time.Now().Unix()
	}
	_, err := d.sql.Exec(`
		INSERT OR REPLACE INTO borg_groups
		(group_id, name, current_epoch, members_json, revoked_json, epochs_json, epoch_members_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		g.GroupID, g.Name, g.CurrentEpoch, g.MembersJSON, g.RevokedJSON, g.EpochsJSON, g.EpochMembersJSON, g.UpdatedAt)
	return err
}

// ListBorgGroups returns all persisted Borg groups.
func (d *DB) ListBorgGroups() ([]BorgGroupRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT group_id, name, current_epoch, members_json, revoked_json, epochs_json, epoch_members_json, updated_at
		FROM borg_groups ORDER BY group_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BorgGroupRecord
	for rows.Next() {
		var r BorgGroupRecord
		if err := rows.Scan(&r.GroupID, &r.Name, &r.CurrentEpoch, &r.MembersJSON, &r.RevokedJSON, &r.EpochsJSON, &r.EpochMembersJSON, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SavePendingSale persists an active BRC-369 offer across seller restarts.
func (d *DB) SavePendingSale(rec PendingSaleRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`
		INSERT OR REPLACE INTO pending_sales
		(offer_id, want_id, buyer_pub, shard, seq, sale_offer_json, reveal_key_hex, chirp_url, proof_bundle_json, expires_at, settled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.OfferID, rec.WantID, rec.BuyerPub, rec.Shard, rec.Seq, rec.SaleOfferJSON,
		rec.RevealKeyHex, rec.ChirpURL, rec.ProofBundleJSON, rec.ExpiresAt, boolToInt(rec.Settled))
	return err
}

// GetPendingSale fetches one pending sale by offerId.
func (d *DB) GetPendingSale(offerID string) (PendingSaleRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var r PendingSaleRecord
	var settled int
	err := d.sql.QueryRow(`
		SELECT offer_id, want_id, buyer_pub, shard, seq, sale_offer_json, reveal_key_hex, chirp_url, proof_bundle_json, expires_at, settled
		FROM pending_sales WHERE offer_id = ?`, offerID).
		Scan(&r.OfferID, &r.WantID, &r.BuyerPub, &r.Shard, &r.Seq, &r.SaleOfferJSON, &r.RevealKeyHex, &r.ChirpURL, &r.ProofBundleJSON, &r.ExpiresAt, &settled)
	r.Settled = settled != 0
	return r, err
}

// MarkPendingSaleSettled marks an offer settled in SQLite.
func (d *DB) MarkPendingSaleSettled(offerID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`UPDATE pending_sales SET settled = 1 WHERE offer_id = ?`, offerID)
	return err
}

// ListPendingSales returns all unexpired pending sales.
func (d *DB) ListPendingSales(nowUnix int64) ([]PendingSaleRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT offer_id, want_id, buyer_pub, shard, seq, sale_offer_json, reveal_key_hex, chirp_url, proof_bundle_json, expires_at, settled
		FROM pending_sales WHERE expires_at = 0 OR expires_at > ?`, nowUnix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingSaleRecord
	for rows.Next() {
		var r PendingSaleRecord
		var settled int
		if err := rows.Scan(&r.OfferID, &r.WantID, &r.BuyerPub, &r.Shard, &r.Seq, &r.SaleOfferJSON, &r.RevealKeyHex, &r.ChirpURL, &r.ProofBundleJSON, &r.ExpiresAt, &settled); err != nil {
			return nil, err
		}
		r.Settled = settled != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneExpiredPendingSales removes expired offers from SQLite.
func (d *DB) PruneExpiredPendingSales(nowUnix int64) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.sql.Exec(`DELETE FROM pending_sales WHERE expires_at > 0 AND expires_at < ?`, nowUnix)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ClaimPaymentAtomic atomically claims txid and all of its spent input outpoints inside one
// SQLite transaction. Shared by BOTH HTTP 402 paywall and /v1/market/purchase so a payment tx
// or double-spent UTXO can never be accepted twice across interfaces or restarts.
func (d *DB) ClaimPaymentAtomic(txid string, inputs []string) error {
	if txid == "" {
		return errors.New("store: empty payment txid")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().Unix()
	b, _ := json.Marshal(inputs)
	if _, err := tx.Exec(`INSERT INTO payment_claims (txid, inputs_json, claimed_at) VALUES (?, ?, ?)`, txid, b, now); err != nil {
		return ErrPaymentClaimReplay
	}
	for _, op := range inputs {
		if op == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO payment_claim_inputs (outpoint, txid, claimed_at) VALUES (?, ?, ?)`, op, txid, now); err != nil {
			return ErrPaymentClaimConflict
		}
	}
	return tx.Commit()
}

// ReleasePaymentClaim deletes a previously claimed txid and its input outpoints (used strictly
// when payment internalization/verification fails before delivering goods/services).
func (d *DB) ReleasePaymentClaim(txid string) error {
	if txid == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, _ = tx.Exec(`DELETE FROM payment_claim_inputs WHERE txid = ?`, txid)
	_, err = tx.Exec(`DELETE FROM payment_claims WHERE txid = ?`, txid)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SavePaymentClaim persists a claimed payment txid and its spent inputs (legacy helper; prefer ClaimPaymentAtomic).
func (d *DB) SavePaymentClaim(txid string, inputs []string) error {
	err := d.ClaimPaymentAtomic(txid, inputs)
	if errors.Is(err, ErrPaymentClaimReplay) {
		return nil
	}
	return err
}

// ReserveFaucetDrip atomically verifies and reserves BOTH per-address 24h cap and global UTC-day cap
// inside ONE SQLite transaction BEFORE broadcasting a faucet drip.
func (d *DB) ReserveFaucetDrip(address string, dripSats, perAddressCap, globalCap uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()
	dayKey := "faucet_global_daily:" + time.Now().UTC().Format("2006-01-02")
	var globalUsed uint64
	var raw []byte
	if qErr := tx.QueryRow(`SELECT value FROM kv WHERE key = ?`, dayKey).Scan(&raw); qErr == nil {
		_ = json.Unmarshal(raw, &globalUsed)
	}
	if globalCap > 0 && globalUsed+dripSats > globalCap {
		return ErrFaucetCapExceeded
	}

	var prevSats uint64
	var lastAt int64
	if qErr := tx.QueryRow(`SELECT total_sats, last_claim_at FROM faucet_claims WHERE address = ?`, address).Scan(&prevSats, &lastAt); qErr == nil {
		if now-lastAt >= 86400 {
			prevSats = 0
		}
		if perAddressCap > 0 && prevSats+dripSats > perAddressCap {
			return ErrFaucetCapExceeded
		}
	}

	nextGlobal := globalUsed + dripSats
	gb, _ := json.Marshal(nextGlobal)
	if _, err := tx.Exec(`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`, dayKey, gb); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO faucet_claims (address, total_sats, last_claim_at) VALUES (?, ?, ?)`, address, prevSats+dripSats, now); err != nil {
		return err
	}
	return tx.Commit()
}

// RefundFaucetDrip rolls back a reserved faucet drip on definitive pre-broadcast failure.
func (d *DB) RefundFaucetDrip(address string, dripSats uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	dayKey := "faucet_global_daily:" + time.Now().UTC().Format("2006-01-02")
	var globalUsed uint64
	var raw []byte
	if qErr := tx.QueryRow(`SELECT value FROM kv WHERE key = ?`, dayKey).Scan(&raw); qErr == nil {
		_ = json.Unmarshal(raw, &globalUsed)
	}
	if globalUsed >= dripSats {
		globalUsed -= dripSats
	} else {
		globalUsed = 0
	}
	gb, _ := json.Marshal(globalUsed)
	_, _ = tx.Exec(`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`, dayKey, gb)

	var prevSats uint64
	var lastAt int64
	if qErr := tx.QueryRow(`SELECT total_sats, last_claim_at FROM faucet_claims WHERE address = ?`, address).Scan(&prevSats, &lastAt); qErr == nil {
		if prevSats >= dripSats {
			prevSats -= dripSats
		} else {
			prevSats = 0
		}
		_, _ = tx.Exec(`UPDATE faucet_claims SET total_sats = ? WHERE address = ?`, prevSats, address)
	}
	return tx.Commit()
}

// ReserveSpendAtomic atomically reserves (opID, attempt) against maxPerDeal and UTC-day spend caps.
// Idempotent on identical (opID, attempt): returns (alreadyReserved=true, nil) on replay.
func (d *DB) ReserveSpendAtomic(opID string, attempt int, amountSats, maxPerDeal, maxPerDay uint64) (bool, error) {
	if opID == "" {
		return false, errors.New("store: empty opID")
	}
	if maxPerDeal == 0 || amountSats > maxPerDeal {
		return false, ErrSpendCapExceeded
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var existingStatus string
	if qErr := tx.QueryRow(`SELECT status FROM spend_reservations WHERE op_id = ? AND attempt = ?`, opID, attempt).Scan(&existingStatus); qErr == nil {
		if existingStatus == "reserved" {
			return true, nil
		}
	}

	dayKey := "market_daily_spend:" + time.Now().UTC().Format("2006-01-02")
	var used uint64
	var raw []byte
	if qErr := tx.QueryRow(`SELECT value FROM kv WHERE key = ?`, dayKey).Scan(&raw); qErr == nil {
		_ = json.Unmarshal(raw, &used)
	}
	if maxPerDay == 0 || used+amountSats > maxPerDay {
		return false, ErrSpendCapExceeded
	}
	next := used + amountSats
	nb, _ := json.Marshal(next)
	if _, err := tx.Exec(`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`, dayKey, nb); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`
		INSERT OR REPLACE INTO spend_reservations (op_id, attempt, amount_sats, status, created_at)
		VALUES (?, ?, ?, 'reserved', ?)`, opID, attempt, amountSats, time.Now().Unix()); err != nil {
		return false, err
	}
	return false, tx.Commit()
}

// ReleaseSpendReservation refunds a previously reserved (opID, attempt) on pre-broadcast failure.
func (d *DB) ReleaseSpendReservation(opID string, attempt int) error {
	if opID == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var amount uint64
	var status string
	var createdAt int64
	if qErr := tx.QueryRow(`SELECT amount_sats, status, created_at FROM spend_reservations WHERE op_id = ? AND attempt = ?`, opID, attempt).Scan(&amount, &status, &createdAt); qErr != nil {
		if errors.Is(qErr, sql.ErrNoRows) {
			return nil
		}
		return qErr
	}
	if status != "reserved" {
		return nil
	}
	if _, err := tx.Exec(`UPDATE spend_reservations SET status = 'released' WHERE op_id = ? AND attempt = ?`, opID, attempt); err != nil {
		return err
	}
	refTime := time.Unix(createdAt, 0).UTC()
	if createdAt == 0 {
		refTime = time.Now().UTC()
	}
	dayKey := "market_daily_spend:" + refTime.Format("2006-01-02")
	var used uint64
	var raw []byte
	if qErr := tx.QueryRow(`SELECT value FROM kv WHERE key = ?`, dayKey).Scan(&raw); qErr == nil {
		_ = json.Unmarshal(raw, &used)
	}
	if used >= amount {
		used -= amount
	} else {
		used = 0
	}
	nb, _ := json.Marshal(used)
	if _, err := tx.Exec(`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`, dayKey, nb); err != nil {
		return err
	}
	return tx.Commit()
}

// ListPaymentClaims loads persisted payment claims for replay protection across restarts
// and deterministic book reconciliation (optional limit caps rows returned).
func (d *DB) ListPaymentClaims(limits ...int) ([]PaymentClaimRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var (
		rows *sql.Rows
		err  error
	)
	if len(limits) > 0 && limits[0] > 0 {
		rows, err = d.sql.Query(`SELECT txid, inputs_json, claimed_at FROM payment_claims ORDER BY claimed_at DESC LIMIT ?`, limits[0])
	} else {
		rows, err = d.sql.Query(`SELECT txid, inputs_json, claimed_at FROM payment_claims ORDER BY claimed_at ASC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PaymentClaimRecord
	for rows.Next() {
		var r PaymentClaimRecord
		var raw []byte
		if err := rows.Scan(&r.TxID, &raw, &r.ClaimedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &r.Inputs)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordUnmatchedWant logs a zero-offer buyer want for seller demand analytics.
func (d *DB) RecordUnmatchedWant(rec UnmatchedWantRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if rec.SeenAt == 0 {
		rec.SeenAt = time.Now().Unix()
	}
	_, err := d.sql.Exec(`
		INSERT OR REPLACE INTO unmatched_wants (want_id, domain, query, max_sats, seen_at)
		VALUES (?, ?, ?, ?, ?)`, rec.WantID, rec.Domain, rec.Query, rec.MaxSats, rec.SeenAt)
	return err
}

// ListUnmatchedWants returns recent zero-offer wants newest first.
func (d *DB) ListUnmatchedWants(limit int) ([]UnmatchedWantRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if limit <= 0 {
		limit = 25
	}
	rows, err := d.sql.Query(`SELECT want_id, domain, query, max_sats, seen_at FROM unmatched_wants ORDER BY seen_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnmatchedWantRecord
	for rows.Next() {
		var r UnmatchedWantRecord
		if err := rows.Scan(&r.WantID, &r.Domain, &r.Query, &r.MaxSats, &r.SeenAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DailySpend returns a UTC-day accumulator stored in KV under prefix:YYYY-MM-DD.
func (d *DB) DailySpend(prefix string) (uint64, error) {
	key := prefix + ":" + time.Now().UTC().Format("2006-01-02")
	var v uint64
	ok, err := d.GetJSON(key, &v)
	if err != nil || !ok {
		return 0, err
	}
	return v, nil
}

// AddDailySpend atomically increments a UTC-day accumulator in KV inside a single SQLite transaction.
func (d *DB) AddDailySpend(prefix string, delta uint64) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	key := prefix + ":" + time.Now().UTC().Format("2006-01-02")
	var cur uint64
	var raw []byte
	if qErr := tx.QueryRow(`SELECT value FROM kv WHERE key = ?`, key).Scan(&raw); qErr == nil {
		_ = json.Unmarshal(raw, &cur)
	}
	next := cur + delta
	nb, _ := json.Marshal(next)
	if _, err := tx.Exec(`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`, key, nb); err != nil {
		return 0, err
	}
	return next, tx.Commit()
}

// LoadOrCreateStableKey loads or generates a persistent 32-byte hex private key in KV
// so market offers and directory announcements keep a stable pubkey across restarts.
func (d *DB) LoadOrCreateStableKey(keyName string, gen func() ([]byte, error)) ([]byte, error) {
	var hexKey string
	ok, err := d.GetJSON(keyName, &hexKey)
	if err == nil && ok && len(hexKey) == 64 {
		var raw [32]byte
		if n, decErr := decodeHex32(hexKey, raw[:]); decErr == nil && n == 32 {
			return raw[:], nil
		}
	}
	created, err := gen()
	if err != nil {
		return nil, err
	}
	_ = d.SetJSON(keyName, encodeHex(created))
	return created, nil
}

func decodeHex32(s string, dst []byte) (int, error) {
	if len(s) != 64 || len(dst) < 32 {
		return 0, errors.New("invalid 32-byte hex")
	}
	for i := 0; i < 32; i++ {
		hi := fromHexChar(s[2*i])
		lo := fromHexChar(s[2*i+1])
		if hi > 15 || lo > 15 {
			return 0, errors.New("invalid hex char")
		}
		dst[i] = (hi << 4) | lo
	}
	return 32, nil
}

func fromHexChar(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	default:
		return 255
	}
}

func encodeHex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[2*i] = digits[v>>4]
		out[2*i+1] = digits[v&0x0f]
	}
	return string(out)
}
