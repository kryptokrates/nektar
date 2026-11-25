package wallet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/entity"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk"
)

// Monitor runs go-wallet-toolbox's official storage lifecycle tasks (AbortAbandoned,
// SendWaitingTransactions, SynchronizeTransactionStatuses, UnFail) alongside Nektar's
// rate-limited WhatsOnChain scanner for external P2PKH deposits to FundAddress().
type Monitor struct {
	engine   *Engine
	httpCli  *http.Client
	mu       sync.Mutex
	lastScan time.Time
}

func newMonitor(e *Engine) *Monitor {
	return &Monitor{
		engine:  e,
		httpCli: &http.Client{Timeout: 15 * time.Second},
	}
}

func (m *Monitor) run(ctx context.Context) {
	sendTick := time.NewTicker(10 * time.Second)
	proofTick := time.NewTicker(30 * time.Second)
	abortTick := time.NewTicker(60 * time.Second)
	fundTick := time.NewTicker(60 * time.Second)
	defer sendTick.Stop()
	defer proofTick.Stop()
	defer abortTick.Stop()
	defer fundTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sendTick.C:
			_, _ = m.SendWaiting(ctx)
		case <-proofTick.C:
			_, _ = m.CheckForProofs(ctx)
			_, _ = m.UnFail(ctx)
		case <-abortTick.C:
			_, _ = m.AbortAbandoned(ctx)
			_, _ = ReconcilePendingBRC29(ctx, m.engine, "")
			if m.engine.sqlDB != nil {
				_, _ = m.engine.sqlDB.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
			}
		case <-fundTick.C:
			_, _, _ = m.SyncFundAddress(ctx)
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

	if n, err := m.CheckForProofs(ctx); err != nil && firstErr == nil {
		firstErr = err
	} else {
		rep.ProvedTxs = n
	}

	if n, err := m.UnFail(ctx); err == nil {
		rep.ProvedTxs += n
	}

	_, _ = ReconcilePendingBRC29(ctx, m.engine, "")

	added, pruned, err := m.SyncFundAddress(ctx)
	rep.AddedOutputs = added
	rep.PrunedSpent = pruned
	if err != nil && firstErr == nil {
		firstErr = err
	}

	if absorbed, aErr := m.engine.AbsorbFundingBasket(ctx); aErr == nil && absorbed > 0 {
		m.engine.fuel.Wake()
	}

	return rep, firstErr
}

// UnFail reconciles failed, aborted, or submission_unknown transactions against the chain:
//   - If a valid SPV MerklePath exists on chain, revives the transaction to "completed" and
//     restores unspent change/owned outputs to spendable=1.
//   - If a submission_unknown transaction is found in the remote mempool, promotes it to "unproven"
//     so CheckForProofs tracks confirmation without ever double-spending its inputs.
func (m *Monitor) UnFail(ctx context.Context) (int, error) {
	if m.engine == nil || m.engine.sqlDB == nil {
		return 0, nil
	}
	q := fmt.Sprintf(`SELECT id, tx_id, status FROM "%s" WHERE user_id = ? AND deleted_at IS NULL AND status IN ('failed','aborted','submission_unknown') AND tx_id IS NOT NULL AND tx_id != ''`, m.engine.tables.transactions)
	rows, err := m.engine.readDB.QueryContext(ctx, q, m.engine.userID)
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
	for _, c := range cands {
		mp, h, ok, _ := m.engine.svc.lookupProof(ctx, c.txid)
		if ok && mp != nil {
			if h == 0 {
				h = mp.BlockHeight
			}
			// Verify Merkle root against headers before reviving to completed.
			if m.engine.cfg.Tracker != nil {
				txHash, hErr := chainhash.NewHashFromHex(c.txid)
				if hErr != nil {
					continue
				}
				root, rErr := mp.ComputeRoot(txHash)
				if rErr != nil || root == nil {
					continue
				}
				valid, vErr := m.engine.svc.IsValidRootForHeight(ctx, root, h)
				if vErr != nil || !valid {
					continue
				}
			}
			if tx, tErr := m.engine.LoadParsedTx(ctx, c.txid); tErr == nil && tx != nil {
				tx.MerklePath = mp
				m.engine.txCache.Put(c.txid, tx)
			}
			_, uErr := m.engine.sqlDB.ExecContext(ctx,
				fmt.Sprintf(`UPDATE "%s" SET status = 'completed', updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ?`, m.engine.tables.transactions),
				c.id, m.engine.userID,
			)
			if uErr == nil {
				_, _ = m.engine.sqlDB.ExecContext(ctx,
					fmt.Sprintf(`UPDATE "%s" SET spendable = 1 WHERE transaction_id = ? AND user_id = ? AND spent_by IS NULL AND deleted_at IS NULL AND basket_name IN ('default','reserve','fuel','nektar funding')`, m.engine.tables.outputs),
					c.id, m.engine.userID,
				)
				if mpBytes := mp.Bytes(); len(mpBytes) > 0 {
					_, _ = m.engine.sqlDB.ExecContext(ctx,
						fmt.Sprintf(`UPDATE "%s" SET status = 'completed', block_height = ?, merkle_path = ? WHERE tx_id = ?`, m.engine.tables.knownTxs),
						h, mpBytes, c.txid,
					)
				}
				revived++
			}
			continue
		}
		if c.status == StatusSubmissionUnknown {
			stRes, sErr := m.engine.svc.GetStatusForTxIDs(ctx, []string{c.txid})
			if sErr == nil && stRes != nil && len(stRes.Results) == 1 && stRes.Results[0].Status == wdk.ResultStatusForTxIDKnown.String() {
				_, _ = m.engine.sqlDB.ExecContext(ctx,
					fmt.Sprintf(`UPDATE "%s" SET status = 'unproven', updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ? AND status = 'submission_unknown'`, m.engine.tables.transactions),
					c.id, m.engine.userID,
				)
			}
		}
	}
	return revived, nil
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

	if before > after {
		return int(before - after), nil
	}
	return 0, nil
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
// Provider.SynchronizeTransactionStatuses.
func (m *Monitor) CheckForProofs(ctx context.Context) (int, error) {
	statuses, err := m.engine.provider.SynchronizeTransactionStatuses(ctx)
	proved := 0
	for _, st := range statuses {
		if st.Status == wdk.ProvenTxStatusCompleted {
			proved++
		}
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
// for external P2PKH deposits sent to Nektar's static FundAddress(), internalizing new outputs
// and pruning any funding outputs confirmed spent externally.
func (m *Monitor) SyncFundAddress(ctx context.Context) (added int, pruned int, err error) {
	if m.engine.cfg.WoC == nil && m.engine.cfg.Chain == nil {
		return 0, 0, nil
	}

	m.mu.Lock()
	if time.Since(m.lastScan) < 5*time.Second {
		m.mu.Unlock()
		return 0, 0, nil
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
			if m.engine.HasTx(ctx, u.TxHash) {
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

	// Prune externally spent BasketFunding outputs using authoritative WoC unspent list + Chain.Spender check.
	if haveLiveSet || m.engine.cfg.Chain != nil {
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
				spentExternally := false
				if m.engine.cfg.Chain != nil {
					if spender, sErr := m.engine.cfg.Chain.Spender(ctx, *o.TxID, o.Vout); sErr == nil && spender != "" {
						spentExternally = true
					}
				} else if haveLiveSet && o.TxStatus == wdk.TxStatusCompleted {
					spentExternally = true
				}
				if spentExternally {
					if uErr := m.engine.provider.OutputsEntity().Update(ctx, &entity.OutputUpdateSpecification{
						ID:        o.ID,
						Spendable: &f,
					}); uErr == nil {
						pruned++
					}
				}
			}
		}
	}
	return added, pruned, nil
}
