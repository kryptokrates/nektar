package store

import (
	"database/sql"
	"time"
)

// EconomicsEventRow logs one operational economics telemetry sample (LLM proxy usage,
// conservative token-savings attribution, market deal spend/earn, or micro-earner bounty payout).
type EconomicsEventRow struct {
	ID          int64  `json:"id,omitempty"`
	Kind        string `json:"kind"` // proxy_usage | tokens_saved | deal_buy | deal_sell | bounty_earn | work_earn
	TokensIn    int64  `json:"tokensIn,omitempty"`
	TokensOut   int64  `json:"tokensOut,omitempty"`
	TokensSaved int64  `json:"tokensSaved,omitempty"`
	SatsDelta   int64  `json:"satsDelta,omitempty"`
	OntologyID  string `json:"ontologyId,omitempty"`
	Shard       string `json:"shard,omitempty"`
	DealID      string `json:"dealId,omitempty"`
	Provider    string `json:"provider,omitempty"`
	CreatedAt   int64  `json:"createdAt"`
}

// EconomicsSummary aggregates operational token-arbitrage and net earnings metrics for the Zero-Crypto UX.
type EconomicsSummary struct {
	TokensIn         int64  `json:"tokensIn"`
	TokensOut        int64  `json:"tokensOut"`
	TokensSaved      int64  `json:"tokensSaved"`
	EarnedSats       uint64 `json:"earnedSats"`
	SpentSats        uint64 `json:"spentSats"`
	NetSats          int64  `json:"netSats"`
	FactsBought      int64  `json:"factsBought"`
	FactsSold        int64  `json:"factsSold"`
	WorkBounties     int64  `json:"workBounties"`
	EstSavedUSDCents int64  `json:"estSavedUsdCents"` // integer cents (@ $0.015 per 1k tokens)
}

func migrateEconomicsTx(tx *sql.Tx) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS economics_events (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	kind         TEXT NOT NULL,
	tokens_in    INTEGER NOT NULL DEFAULT 0,
	tokens_out   INTEGER NOT NULL DEFAULT 0,
	tokens_saved INTEGER NOT NULL DEFAULT 0,
	sats_delta   INTEGER NOT NULL DEFAULT 0,
	ontology_id  TEXT NOT NULL DEFAULT '',
	shard        TEXT NOT NULL DEFAULT '',
	deal_id      TEXT NOT NULL DEFAULT '',
	provider     TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_economics_events_created ON economics_events(created_at);
CREATE INDEX IF NOT EXISTS idx_economics_events_kind ON economics_events(kind, created_at);
`
	_, err := tx.Exec(ddl)
	return err
}

// RecordEconomicsEvent persists one economics telemetry event.
func (d *DB) RecordEconomicsEvent(ev EconomicsEventRow) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ev.CreatedAt == 0 {
		ev.CreatedAt = time.Now().Unix()
	}
	_, err := d.sql.Exec(`
		INSERT INTO economics_events (
			kind, tokens_in, tokens_out, tokens_saved, sats_delta, ontology_id, shard, deal_id, provider, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.Kind, ev.TokensIn, ev.TokensOut, ev.TokensSaved, ev.SatsDelta,
		ev.OntologyID, ev.Shard, ev.DealID, ev.Provider, ev.CreatedAt,
	)
	return err
}

// SummarizeEconomics aggregates operational economics metrics since sinceUnix (0 = all time).
func (d *DB) SummarizeEconomics(sinceUnix int64) (EconomicsSummary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var s EconomicsSummary
	var earned, spent int64
	err := d.sql.QueryRow(`
		SELECT
			COALESCE(SUM(tokens_in), 0),
			COALESCE(SUM(tokens_out), 0),
			COALESCE(SUM(tokens_saved), 0),
			COALESCE(SUM(CASE WHEN sats_delta > 0 THEN sats_delta ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN sats_delta < 0 THEN -sats_delta ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN kind = 'deal_buy' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN kind IN ('deal_sell', 'bounty_earn') THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN kind = 'work_earn' THEN 1 ELSE 0 END), 0)
		FROM economics_events
		WHERE created_at >= ?`, sinceUnix).Scan(
		&s.TokensIn, &s.TokensOut, &s.TokensSaved,
		&earned, &spent,
		&s.FactsBought, &s.FactsSold, &s.WorkBounties,
	)
	if err != nil {
		return EconomicsSummary{}, err
	}
	if earned > 0 {
		s.EarnedSats = uint64(earned)
	}
	if spent > 0 {
		s.SpentSats = uint64(spent)
	}
	s.NetSats = earned - spent
	// Conservative LLM reasoning-token cost valuation: 1.5 cents per 1,000 saved tokens (integer math).
	s.EstSavedUSDCents = (s.TokensSaved * 15) / 10000
	return s, nil
}
