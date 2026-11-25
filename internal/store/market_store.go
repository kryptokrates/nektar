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
	PolicyID    string   `json:"policyId"`
	PolicyHash  string   `json:"policyHash"`
	Roles       []string `json:"roles"` // buyer | seller
	Domains     []string `json:"domains,omitempty"`
	MaxPerDeal  uint64   `json:"maxPerDeal"`
	MaxPerDay   uint64   `json:"maxPerDay"`
	ZeroConfMax uint64   `json:"zeroConfMax"`
	Active      bool     `json:"active"`
	ApprovedBy  string   `json:"approvedBy"`
	ApprovedAt  int64    `json:"approvedAt"`
	RevokedAt   int64    `json:"revokedAt,omitempty"`
}

// DealRecord persists a buyer or seller deal across restarts with optimistic CAS revision.
type DealRecord struct {
	DealID          string `json:"dealId"`
	Role            string `json:"role"`   // buyer | seller
	Status          string `json:"status"` // open | collecting | offered | approval_pending | paid | settled | failed | expired
	WantJSON        []byte `json:"wantJson,omitempty"`
	ChosenOfferJSON []byte `json:"chosenOfferJson,omitempty"`
	PaymentTxID     string `json:"paymentTxid,omitempty"`
	PaymentBEEF     []byte `json:"paymentBeef,omitempty"`
	ClaimKeyHex     string `json:"claimKeyHex,omitempty"`
	ChirpURL        string `json:"chirpUrl,omitempty"`
	DeliveredText   string `json:"deliveredText,omitempty"`
	CounterpartyPub string `json:"counterpartyPub,omitempty"`
	PriceSats       uint64 `json:"priceSats"`
	IsHubSeller     bool   `json:"isHubSeller,omitempty"`
	FirstOfferMs    int64  `json:"firstOfferMs,omitempty"`
	Error           string `json:"error,omitempty"`
	Revision        int64  `json:"revision"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
}

// ApprovalRecord tracks an out-of-policy spend awaiting explicit operator consent.
type ApprovalRecord struct {
	ID           string `json:"id"`
	DealID       string `json:"dealId"`
	Reason       string `json:"reason"`
	MaxExtraSats uint64 `json:"maxExtraSats"`
	Status       string `json:"status"` // pending | approved | denied | expired
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
`
	_, err := d.sql.Exec(schema)
	return err
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

// CountFragments returns total fragments and anchored fragments across non-system shards.
func (d *DB) CountFragments() (total int, anchored int, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	err = d.sql.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN batch_n >= 0 THEN 1 ELSE 0 END), 0) FROM fragments WHERE shard != '__receipts__' AND forgotten = 0`).
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
		(policy_id, policy_hash, roles_json, domains_json, max_per_deal, max_per_day, zero_conf_max, active, approved_by, approved_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.PolicyID, p.PolicyHash, string(roles), string(doms), p.MaxPerDeal, p.MaxPerDay, p.ZeroConfMax,
		boolToInt(p.Active), p.ApprovedBy, p.ApprovedAt, p.RevokedAt)
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
		SELECT policy_id, policy_hash, roles_json, domains_json, max_per_deal, max_per_day, zero_conf_max, active, approved_by, approved_at, revoked_at
		FROM operator_policies ORDER BY approved_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OperatorPolicyRecord
	for rows.Next() {
		var p OperatorPolicyRecord
		var rolesJSON, domJSON string
		var active int
		if err := rows.Scan(&p.PolicyID, &p.PolicyHash, &rolesJSON, &domJSON, &p.MaxPerDeal, &p.MaxPerDay, &p.ZeroConfMax, &active, &p.ApprovedBy, &p.ApprovedAt, &p.RevokedAt); err != nil {
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
		(deal_id, role, status, want_json, chosen_offer_json, payment_txid, payment_beef, claim_key_hex, chirp_url, delivered_text, counterparty_pub, price_sats, is_hub_seller, first_offer_ms, error, revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.DealID, rec.Role, rec.Status, rec.WantJSON, rec.ChosenOfferJSON, rec.PaymentTxID, rec.PaymentBEEF,
		rec.ClaimKeyHex, rec.ChirpURL, rec.DeliveredText, rec.CounterpartyPub, rec.PriceSats,
		boolToInt(rec.IsHubSeller), rec.FirstOfferMs, rec.Error, rec.Revision, rec.CreatedAt, rec.UpdatedAt)
	return err
}

// GetDeal fetches one deal by dealId.
func (d *DB) GetDeal(dealID string) (DealRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	row := d.sql.QueryRow(`
		SELECT deal_id, role, status, want_json, chosen_offer_json, payment_txid, payment_beef, claim_key_hex, chirp_url, delivered_text, counterparty_pub, price_sats, is_hub_seller, first_offer_ms, error, revision, created_at, updated_at
		FROM deals WHERE deal_id = ?`, dealID)
	var r DealRecord
	var hub int
	err := row.Scan(&r.DealID, &r.Role, &r.Status, &r.WantJSON, &r.ChosenOfferJSON, &r.PaymentTxID, &r.PaymentBEEF,
		&r.ClaimKeyHex, &r.ChirpURL, &r.DeliveredText, &r.CounterpartyPub, &r.PriceSats, &hub, &r.FirstOfferMs,
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
		SELECT deal_id, role, status, want_json, chosen_offer_json, payment_txid, payment_beef, claim_key_hex, chirp_url, delivered_text, counterparty_pub, price_sats, is_hub_seller, first_offer_ms, error, revision, created_at, updated_at
		FROM deals ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DealRecord
	for rows.Next() {
		var r DealRecord
		var hub int
		if err := rows.Scan(&r.DealID, &r.Role, &r.Status, &r.WantJSON, &r.ChosenOfferJSON, &r.PaymentTxID, &r.PaymentBEEF,
			&r.ClaimKeyHex, &r.ChirpURL, &r.DeliveredText, &r.CounterpartyPub, &r.PriceSats, &hub, &r.FirstOfferMs,
			&r.Error, &r.Revision, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.IsHubSeller = hub != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveApproval saves or updates a pending operator approval escalation.
func (d *DB) SaveApproval(a ApprovalRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().Unix()
	}
	_, err := d.sql.Exec(`
		INSERT OR REPLACE INTO approvals (id, deal_id, reason, max_extra_sats, status, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.DealID, a.Reason, a.MaxExtraSats, a.Status, a.ExpiresAt, a.CreatedAt)
	return err
}

// ListPendingApprovals returns all unexpired pending operator approvals.
func (d *DB) ListPendingApprovals() ([]ApprovalRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT id, deal_id, reason, max_extra_sats, status, expires_at, created_at
		FROM approvals WHERE status = 'pending' AND expires_at > ? ORDER BY created_at ASC`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApprovalRecord
	for rows.Next() {
		var a ApprovalRecord
		if err := rows.Scan(&a.ID, &a.DealID, &a.Reason, &a.MaxExtraSats, &a.Status, &a.ExpiresAt, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DecideApproval updates an approval's status to approved or denied.
func (d *DB) DecideApproval(id string, approve bool) (ApprovalRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	status := "denied"
	if approve {
		status = "approved"
	}
	if _, err := d.sql.Exec(`UPDATE approvals SET status = ? WHERE id = ?`, status, id); err != nil {
		return ApprovalRecord{}, err
	}
	var a ApprovalRecord
	err := d.sql.QueryRow(`SELECT id, deal_id, reason, max_extra_sats, status, expires_at, created_at FROM approvals WHERE id = ?`, id).
		Scan(&a.ID, &a.DealID, &a.Reason, &a.MaxExtraSats, &a.Status, &a.ExpiresAt, &a.CreatedAt)
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

// RecordFaucetClaim records a completed testnet faucet drip to address.
func (d *DB) RecordFaucetClaim(address string, sats uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().Unix()
	_, err := d.sql.Exec(`
		INSERT INTO faucet_claims (address, total_sats, last_claim_at) VALUES (?, ?, ?)
		ON CONFLICT(address) DO UPDATE SET total_sats = total_sats + excluded.total_sats, last_claim_at = excluded.last_claim_at`,
		address, sats, now)
	return err
}
