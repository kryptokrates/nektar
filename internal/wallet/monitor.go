package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/entity"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk"
)

type unfailBackoffEntry struct {
	attempts int
	nextTry  time.Time
}

// Monitor runs go-wallet-toolbox's official storage lifecycle tasks (AbortAbandoned,
// SendWaitingTransactions, SynchronizeTransactionStatuses, UnFail) alongside Nektar's
// rate-limited WhatsOnChain scanner for external P2PKH deposits to FundAddress().
type Monitor struct {
	engine   *Engine
	httpCli  *http.Client
	mu       sync.Mutex
	lastScan time.Time
	backoff  map[string]unfailBackoffEntry
}

func newMonitor(e *Engine) *Monitor {
	return &Monitor{
		engine:  e,
		httpCli: &http.Client{Timeout: 15 * time.Second},
		backoff: make(map[string]unfailBackoffEntry),
	}
}

func (m *Monitor) run(ctx context.Context) {
	// Decouple slow external scans (Desktop BRC-29 dial + WoC address sync) onto a dedicated
	// goroutine so 15s network I/O never head-of-line blocks fast SendWaiting/CheckForProofs.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		abortTick := time.NewTicker(60 * time.Second)
		fundTick := time.NewTicker(60 * time.Second)
		defer abortTick.Stop()
		defer fundTick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-abortTick.C:
				if _, err := m.AbortAbandoned(ctx); err != nil && ctx.Err() == nil {
					m.engine.logf("wallet monitor: AbortAbandoned warning: %v", err)
				}
				if _, err := ReconcilePendingBRC29(ctx, m.engine, ""); err != nil && ctx.Err() == nil {
					m.engine.logf("wallet monitor: ReconcilePendingBRC29 warning: %v", err)
				}
				if m.engine.sqlDB != nil {
					_, _ = m.engine.sqlDB.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
				}
			case <-fundTick.C:
				if _, _, err := m.SyncFundAddress(ctx); err != nil && !errors.Is(err, ErrThrottled) && ctx.Err() == nil {
					m.engine.logf("wallet monitor: SyncFundAddress warning: %v", err)
				}
			}
		}
	}()
	defer wg.Wait()

	sendTick := time.NewTicker(10 * time.Second)
	proofTick := time.NewTicker(30 * time.Second)
	defer sendTick.Stop()
	defer proofTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sendTick.C:
			if _, err := m.SendWaiting(ctx); err != nil && ctx.Err() == nil {
				m.engine.logf("wallet monitor: SendWaiting warning: %v", err)
			}
		case <-proofTick.C:
			if _, err := m.CheckForProofs(ctx); err != nil && ctx.Err() == nil {
				m.engine.logf("wallet monitor: CheckForProofs warning: %v", err)
			}
			if _, err := m.UnFail(ctx); err != nil && ctx.Err() == nil {
				m.engine.logf("wallet monitor: UnFail warning: %v", err)
			}
		}
	}
}

// SyncAll executes a complete synchronous reconciliation pass.
func (m *Monitor) SyncAll(ctx context.Context) (SyncReport, error) {
	var rep SyncReport
	var firstErr error

	if n, err := m.AbortAbandoned(ctx); err != nil && firstErr == nil {
		firstErr = err
	} else {
		rep.AbortedStale = n
	}

	if n, err := m.SendWaiting(ctx); err != nil && firstErr == nil {
		firstErr = err
	} else {
		rep.SentWaiting = n
	}

	// Clear go-wallet-toolbox's cached last_block watermark on explicit SyncAll so a prior
	// transient indexer error cannot poison manual `wallet sync` until the next block mines.
	if m.engine != nil && m.engine.sqlDB != nil {
		_, _ = m.engine.sqlDB.ExecContext(ctx, `DELETE FROM bsv_key_values WHERE key = 'synchronize_tx_statuses_last_block'`)
	}
	if n, err := m.CheckForProofs(ctx); err != nil && firstErr == nil {
		firstErr = err
	} else {
		rep.ProvedTxs = n
	}

	if n, err := m.UnFail(ctx); err == nil {
		rep.ProvedTxs += n
	} else if firstErr == nil {
		firstErr = err
	}

	if _, err := ReconcilePendingBRC29(ctx, m.engine, ""); err != nil && firstErr == nil {
		firstErr = err
	}

	added, pruned, err := m.SyncFundAddress(ctx)
	rep.AddedOutputs = added
	rep.PrunedSpent = pruned
	if err != nil && !errors.Is(err, ErrThrottled) && firstErr == nil {
		firstErr = err
	}

	if absorbed, aErr := m.engine.AbsorbFundingBasket(ctx); aErr == nil && absorbed > 0 {
		m.engine.fuel.Wake()
	}

	return rep, firstErr
}

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

// AbortAbandoned reclaims stale unsigned/unprocessed transactions and releases their reserved UTXOs
// via go-wallet-toolbox's Provider.AbortAbandoned.
func (m *Monitor) AbortAbandoned(ctx context.Context) (int, error) {
	before, _ := m.engine.provider.TransactionEntity().Read().
		UserID().Equals(m.engine.userID).
		Status().In(wdk.TxStatusUnsigned, wdk.TxStatusUnprocessed).
		Count(ctx)

	if err := m.engine.provider.AbortAbandoned(ctx); err != nil {
		return 0, err
	}

	after, _ := m.engine.provider.TransactionEntity().Read().
		UserID().Equals(m.engine.userID).
		Status().In(wdk.TxStatusUnsigned, wdk.TxStatusUnprocessed).
		Count(ctx)

	aborted := 0
	if before > after {
		aborted = int(before - after)
	}
	if ns, err := m.engine.ReconcileOrphanedNoSend(ctx); err == nil && ns > 0 {
		aborted += ns
	}
	return aborted, nil
}

// SendWaiting retries queued delayed/unprocessed transactions via go-wallet-toolbox's Provider.SendWaitingTransactions.
func (m *Monitor) SendWaiting(ctx context.Context) (int, error) {
	if !m.engine.svc.hasBroadcaster() {
		return 0, nil
	}
	res, err := m.engine.provider.SendWaitingTransactions(ctx, 0)
	sent := 0
	if res != nil {
		for _, r := range res.SendWithResults {
			if r.Status != wdk.SendWithResultStatusFailed {
				sent++
			}
		}
		for _, r := range res.NotDelayedResults {
			if r.Status != wdk.ReviewActionResultStatusDoubleSpend && r.Status != wdk.ReviewActionResultStatusServiceError {
				sent++
			}
		}
	}
	return sent, err
}

// CheckForProofs polls Arcade/Chain for MerklePaths on unproven transactions via go-wallet-toolbox's
// Provider.SynchronizeTransactionStatuses, evicts stale txCache entries, notifies OnTxMined listeners,
// and wakes FuelKeeper when proven stock increases.
func (m *Monitor) CheckForProofs(ctx context.Context) (int, error) {
	statuses, err := m.engine.provider.SynchronizeTransactionStatuses(ctx)
	proved := 0
	for _, st := range statuses {
		if st.Status == wdk.ProvenTxStatusCompleted {
			proved++
			// Evict stale MerklePath==nil entry from txCache and reload freshly mined tx + BUMP.
			m.engine.txCache.Delete(st.TxID)
			if txObj, lErr := m.engine.LoadParsedTx(ctx, st.TxID); lErr == nil && txObj != nil && txObj.MerklePath != nil {
				if m.engine.cfg.OnTxMined != nil {
					m.engine.cfg.OnTxMined(st.TxID, txObj.MerklePath.BlockHeight, txObj.MerklePath)
				}
			}
		}
	}
	if proved > 0 {
		m.engine.fuel.Wake()
	}
	return proved, err
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
			if seenTx[u.TxHash] {
				continue
			}
			seenTx[u.TxHash] = true
			if m.engine.HasOutput(ctx, u.TxHash, u.TxPos) {
				continue
			}
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
		if absorbed, aErr := m.engine.AbsorbFundingBasket(ctx); aErr != nil {
			m.engine.logf("wallet sync: post-deposit absorb warning: %v", aErr)
		} else if absorbed > 0 {
			m.engine.fuel.Wake()
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
