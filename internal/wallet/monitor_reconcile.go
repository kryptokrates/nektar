package wallet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/entity"
)

// UnFail reconciles bounded recent failed/aborted and submission_unknown transactions against the chain:
//   - Fails closed when Tracker == nil.
//   - Requires raw tx bytes (verified sha256d == txid) + header-verified BUMP (with block_hash and
//     merkle_root persisted in known_txes) before reviving to "completed".
//   - When promoting submission_unknown -> unproven (from verified raw tx bytes on chain), atomically
//     restores outputs.spendable=1, upserts user_utxos, and updates known_txes so UTXOs are never stranded.
func (m *Monitor) UnFail(ctx context.Context) (int, error) {
	if m.engine == nil || m.engine.sqlDB == nil {
		return 0, nil
	}
	if m.engine.cfg.Tracker == nil {
		return 0, ErrNoChain
	}
	// Bound scan to submission_unknown plus recent (<=24h) failed/aborted txs (max 20 per pass).
	cutoff := time.Now().Add(-24 * time.Hour)
	q := fmt.Sprintf(`
		SELECT id, tx_id, status
		FROM "%s"
		WHERE user_id = ?
		  AND deleted_at IS NULL
		  AND tx_id IS NOT NULL
		  AND tx_id != ''
		  AND (status = 'submission_unknown' OR (status IN ('failed','aborted') AND updated_at >= ?))
		ORDER BY updated_at DESC
		LIMIT 20`, m.engine.tables.transactions)
	rows, err := m.engine.readDB.QueryContext(ctx, q, m.engine.userID, cutoff)
	if err != nil {
		return 0, err
	}
	type cand struct {
		id     int64
		txid   string
		status string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.id, &c.txid, &c.status) == nil {
			cands = append(cands, c)
		}
	}
	_ = rows.Close()
	if len(cands) == 0 {
		return 0, nil
	}

	revived := 0
	now := time.Now()
	for _, c := range cands {
		if !m.shouldAttemptUnfail(c.txid, now) {
			continue
		}
		// Single verified proof lookup (validates ComputeRoot + IsValidRootForHeight internally).
		mp, h, ok, pErr := m.engine.svc.lookupProof(ctx, c.txid)
		if pErr != nil {
			m.recordUnfailBackoff(c.txid, now)
			continue
		}
		if ok && mp != nil {
			if h == 0 {
				h = mp.BlockHeight
			}
			txHash, hErr := chainhash.NewHashFromHex(c.txid)
			if hErr != nil {
				continue
			}
			root, rErr := mp.ComputeRoot(txHash)
			if rErr != nil || root == nil {
				continue
			}
			rootHex := root.String()
			hdr, hdrErr := m.engine.svc.ChainHeaderByHeight(ctx, h)
			if hdrErr != nil || hdr == nil || len(hdr.Hash) != 64 {
				m.recordUnfailBackoff(c.txid, now)
				continue
			}
			if !m.engine.cfg.InMemoryStore && !strings.EqualFold(hdr.MerkleRoot, rootHex) {
				m.recordUnfailBackoff(c.txid, now)
				continue
			}
			// Require actual verified raw tx bytes before reviving outputs and marking inputs spent!
			txObj, _ := m.engine.LoadParsedTx(ctx, c.txid)
			if txObj == nil && m.engine.cfg.Chain != nil {
				if raw, rErr := m.engine.cfg.Chain.TxRaw(ctx, c.txid); rErr == nil && len(raw) > 0 {
					if parsed, parseErr := transaction.NewTransactionFromBytes(raw); parseErr == nil && parsed.TxID().String() == c.txid {
						txObj = parsed
					}
				}
			}
			if txObj == nil {
				m.recordUnfailBackoff(c.txid, now)
				continue
			}
			txObj.MerklePath = mp
			if err := m.engine.promoteTxStatusAtomic(ctx, c.id, c.txid, StatusCompleted, txObj, mp, h, hdr.Hash, rootHex); err != nil {
				m.recordUnfailBackoff(c.txid, now)
				continue
			}
			m.engine.txCache.Put(c.txid, txObj)
			m.clearUnfailBackoff(c.txid)
			if m.engine.cfg.OnTxMined != nil {
				m.engine.cfg.OnTxMined(c.txid, h, mp)
			}
			revived++
			continue
		}

		// Not mined yet: for submission_unknown, verify actual raw tx bytes on chain (never bare WoC JSON)
		// without re-calling lookupProof.
		if c.status == StatusSubmissionUnknown && m.engine.cfg.Chain != nil {
			raw, rErr := m.engine.cfg.Chain.TxRaw(ctx, c.txid)
			if rErr == nil && len(raw) > 0 {
				if parsed, parseErr := transaction.NewTransactionFromBytes(raw); parseErr == nil && parsed.TxID().String() == c.txid {
					if err := m.engine.promoteTxStatusAtomic(ctx, c.id, c.txid, StatusUnproven, parsed, nil, 0, "", ""); err == nil {
						m.clearUnfailBackoff(c.txid)
						continue
					}
				}
			}
		}
		m.recordUnfailBackoff(c.txid, now)
	}
	return revived, nil
}

func (m *Monitor) shouldAttemptUnfail(txid string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	ent, ok := m.backoff[txid]
	if !ok {
		return true
	}
	return !now.Before(ent.nextTry)
}

func (m *Monitor) recordUnfailBackoff(txid string, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.backoff) > 512 {
		clear(m.backoff)
	}
	ent := m.backoff[txid]
	ent.attempts++
	shift := min(ent.attempts-1, 6)
	wait := time.Duration(30<<shift) * time.Second
	if wait > 30*time.Minute {
		wait = 30 * time.Minute
	}
	ent.nextTry = now.Add(wait)
	m.backoff[txid] = ent
}

func (m *Monitor) clearUnfailBackoff(txid string) {
	m.mu.Lock()
	delete(m.backoff, txid)
	m.mu.Unlock()
}

type wocUnspentResp struct {
	Result []struct {
		TxHash string `json:"tx_hash"`
		TxPos  uint32 `json:"tx_pos"`
		Value  uint64 `json:"value"`
		Height uint32 `json:"height"`
	} `json:"result"`
}

// SyncFundAddress performs a rate-limited scan (honoring cfg.WoC Base/Key/Client and cfg.Chain)
// for external P2PKH deposits sent to Nektar's static FundAddress().
// Indexers are used strictly for discovery: outputs are NEVER pruned from WoC omission or bare
// Spender JSON — only verified spending transaction bytes (recomputed txid + matching input +
// verified unlocking script or mined BUMP) may mark a BasketFunding output spent.
func (m *Monitor) SyncFundAddress(ctx context.Context) (added int, pruned int, err error) {
	if m.engine.cfg.WoC == nil && m.engine.cfg.Chain == nil {
		return 0, 0, nil
	}

	m.mu.Lock()
	if time.Since(m.lastScan) < 5*time.Second {
		m.mu.Unlock()
		return 0, 0, ErrThrottled
	}
	m.lastScan = time.Now()
	m.mu.Unlock()

	liveOutpoints := make(map[string]bool)
	haveLiveSet := false

	if m.engine.cfg.WoC != nil {
		netSeg := "test"
		if m.engine.cfg.Mainnet {
			netSeg = "main"
		}
		if m.engine.cfg.WoC.Network != "" {
			netSeg = m.engine.cfg.WoC.Network
		}
		base := strings.TrimRight(m.engine.cfg.WoC.Base, "/")
		if base == "" {
			base = fmt.Sprintf("https://api.whatsonchain.com/v1/bsv/%s", netSeg)
		}
		urlStr := fmt.Sprintf("%s/address/%s/unspent/all", base, m.engine.FundAddress())
		req, rErr := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
		if rErr != nil {
			return 0, 0, rErr
		}
		if m.engine.cfg.WoC.Key != "" {
			req.Header.Set("Authorization", m.engine.cfg.WoC.Key)
		}
		cli := m.httpCli
		if m.engine.cfg.WoC.Client != nil {
			cli = m.engine.cfg.WoC.Client
		}
		resp, dErr := cli.Do(req)
		if dErr != nil {
			return 0, 0, dErr
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return 0, 0, fmt.Errorf("wallet sync: WoC HTTP %d: %s", resp.StatusCode, string(body))
		}
		var parsed wocUnspentResp
		if jErr := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&parsed); jErr != nil {
			return 0, 0, fmt.Errorf("wallet sync: decode WoC unspent: %w", jErr)
		}
		haveLiveSet = true
		seenTx := make(map[string]bool)
		for _, u := range parsed.Result {
			if u.TxHash == "" || u.Value == 0 {
				continue
			}
			liveOutpoints[fmt.Sprintf("%s:%d", u.TxHash, u.TxPos)] = true
			if m.engine.HasOutput(ctx, u.TxHash, u.TxPos) {
				continue
			}
			if seenTx[u.TxHash] {
				continue
			}
			seenTx[u.TxHash] = true
			res, iErr := m.engine.ImportTxID(ctx, u.TxHash)
			if iErr != nil {
				m.engine.logf("wallet sync: import %s skipped: %v", u.TxHash, iErr)
				continue
			}
			added += res.Outputs
		}
	}

	// Absorb newly discovered deposits into native BRC-29 "default" and wake FuelKeeper immediately.
	if added > 0 {
		if _, aErr := m.engine.AbsorbFundingBasket(ctx); aErr != nil {
			m.engine.logf("wallet sync: post-deposit absorb warning: %v", aErr)
		} else {
			m.engine.fuel.Wake()
		}
	}

	// Prune externally spent BasketFunding outputs ONLY when verified spending tx bytes prove the spend!
	// Never prune from WoC omission or bare Chain.Spender JSON alone.
	if m.engine.cfg.Chain != nil {
		outs, oErr := m.engine.provider.OutputsEntity().Read().
			UserID().Equals(m.engine.userID).
			BasketName().Equals(BasketFunding).
			Spendable().Equals(true).
			Find(ctx)
		if oErr == nil {
			f := false
			for _, o := range outs {
				if o.TxID == nil || *o.TxID == "" || o.SpentBy != nil {
					continue
				}
				key := fmt.Sprintf("%s:%d", *o.TxID, o.Vout)
				if haveLiveSet && liveOutpoints[key] {
					continue
				}
				spentVerified, vErr := m.engine.verifyOutpointSpent(ctx, *o.TxID, o.Vout, uint64(o.Satoshis), o.LockingScript)
				if vErr != nil || !spentVerified {
					continue
				}
				if uErr := m.engine.provider.OutputsEntity().Update(ctx, &entity.OutputUpdateSpecification{
					ID:        o.ID,
					Spendable: &f,
				}); uErr == nil {
					pruned++
				}
			}
		}
	}
	return added, pruned, nil
}
