package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ListReceipts returns committed receipts newest first.
func (d *DB) ListReceipts(limit int) ([]ReceiptRecord, error) {
	rdb, release := d.rdb()
	defer release()
	if limit <= 0 {
		limit = 100
	}
	rows, err := rdb.Query(`SELECT receipt_hash, shard_seq, body_json, anchor_txid, created_at FROM receipts ORDER BY created_at DESC, shard_seq DESC LIMIT ?`, limit)
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

// CountReceipts returns total persisted receipts in O(1) without loading body_json BLOBs.
func (d *DB) CountReceipts() (int64, error) {
	rdb, release := d.rdb()
	defer release()
	var n int64
	err := rdb.QueryRow(`SELECT COUNT(1) FROM receipts`).Scan(&n)
	return n, err
}

// CountFragments returns total fragments and settled-anchored (mined BUMP) fragments across non-system shards.
func (d *DB) CountFragments() (total int, anchored int, err error) {
	rdb, release := d.rdb()
	defer release()
	err = rdb.QueryRow(`
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

// ListHosts returns all persisted hosts ordered by URL.
func (d *DB) ListHosts() ([]HostRecord, error) {
	rdb, release := d.rdb()
	defer release()
	rows, err := rdb.Query(`SELECT url, state, source, blocked, last_seen FROM hosts ORDER BY url ASC`)
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
	usageRights := strings.ToLower(strings.TrimSpace(l.UsageRights))
	if usageRights == "" {
		usageRights = "inference_only"
	}
	doms, _ := json.Marshal(l.Domains)
	_, err := d.sql.Exec(`
		INSERT OR REPLACE INTO sell_listings (
			shard, domains_json, price_sats, price_per_unit, query_price_sats, subscription_price_sats,
			subscription_duration_sec, freshness_class, usage_rights, min_advertised_grade,
			description, ontology_id, allow_raw, dynamic_pricing_json, active, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.Shard, string(doms), l.PriceSats, boolToInt(l.PricePerUnit), l.QueryPriceSats, l.SubscriptionPriceSats,
		l.SubscriptionDurationSec, strings.ToLower(strings.TrimSpace(l.FreshnessClass)), usageRights,
		strings.ToUpper(strings.TrimSpace(l.MinAdvertisedGrade)),
		l.Description, l.OntologyID, boolToInt(l.AllowRaw), l.DynamicPricingJSON, boolToInt(l.Active), l.CreatedAt)
	return err
}

func scanSellListing(s scanner) (SellListing, error) {
	var l SellListing
	var domJSON string
	var pricePerUnit, allowRaw, active int
	if err := s.Scan(
		&l.Shard, &domJSON, &l.PriceSats, &pricePerUnit, &l.QueryPriceSats, &l.SubscriptionPriceSats,
		&l.SubscriptionDurationSec, &l.FreshnessClass, &l.UsageRights, &l.MinAdvertisedGrade,
		&l.Description, &l.OntologyID, &allowRaw, &l.DynamicPricingJSON, &active, &l.CreatedAt,
	); err != nil {
		return SellListing{}, err
	}
	_ = json.Unmarshal([]byte(domJSON), &l.Domains)
	l.PricePerUnit = pricePerUnit != 0
	l.AllowRaw = allowRaw != 0
	l.Active = active != 0
	return l, nil
}

const sellListingSelectCols = `shard, domains_json, price_sats, COALESCE(price_per_unit, 0),
	COALESCE(query_price_sats, 0), COALESCE(subscription_price_sats, 0), COALESCE(subscription_duration_sec, 0),
	COALESCE(freshness_class, ''), COALESCE(usage_rights, 'inference_only'), COALESCE(min_advertised_grade, ''),
	description, COALESCE(ontology_id, ''), COALESCE(allow_raw, 0), dynamic_pricing_json, active, created_at`

// GetSellListing returns the SellListing for shard, or (nil, nil) if not listed.
func (d *DB) GetSellListing(shard string) (*SellListing, error) {
	rdb, release := d.rdb()
	defer release()
	l, err := scanSellListing(rdb.QueryRow(`SELECT `+sellListingSelectCols+` FROM sell_listings WHERE shard = ?`, shard))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// ListSellListings returns all persisted sell listings.
func (d *DB) ListSellListings() ([]SellListing, error) {
	rdb, release := d.rdb()
	defer release()
	rows, err := rdb.Query(`SELECT ` + sellListingSelectCols + ` FROM sell_listings ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SellListing
	for rows.Next() {
		l, err := scanSellListing(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// UpdateSellListingPricing updates dynamic pricing state and active price for a shard listing.
func (d *DB) UpdateSellListingPricing(shard string, priceSats uint64, dynJSON []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`UPDATE sell_listings SET price_sats = ?, dynamic_pricing_json = ? WHERE shard = ?`, priceSats, dynJSON, shard)
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
	rdb, release := d.rdb()
	defer release()
	var total int64
	err = rdb.QueryRow(`SELECT COUNT(*), COALESCE(SUM(price_sats), 0) FROM deals WHERE role = 'seller' AND status = 'settled' AND shard = ?`, shard).
		Scan(&works, &total)
	if total > 0 {
		earned = uint64(total)
	}
	return works, earned, err
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
	if err == nil {
		d.policyEpoch.Add(1)
	}
	return err
}

// RevokeOperatorPolicy marks a policy revoked.
func (d *DB) RevokeOperatorPolicy(policyID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`UPDATE operator_policies SET active = 0, revoked_at = ? WHERE policy_id = ?`, time.Now().Unix(), policyID)
	if err == nil {
		d.policyEpoch.Add(1)
	}
	return err
}

// ListOperatorPolicies returns all persisted policies newest first.
func (d *DB) ListOperatorPolicies() ([]OperatorPolicyRecord, error) {
	rdb, release := d.rdb()
	defer release()
	rows, err := rdb.Query(`
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
	rdb, release := d.rdb()
	defer release()
	rows, err := rdb.Query(`
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
	rdb, release := d.rdb()
	defer release()
	if limit <= 0 {
		limit = 25
	}
	rows, err := rdb.Query(`SELECT want_id, domain, query, max_sats, seen_at FROM unmatched_wants ORDER BY seen_at DESC LIMIT ?`, limit)
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
