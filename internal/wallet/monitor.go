package wallet

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk"

	"github.com/kryptokrates/nektar/internal/core"
)

// Monitor runs go-wallet-toolbox's official storage lifecycle tasks (AbortAbandoned,
// SendWaitingTransactions, SynchronizeTransactionStatuses, UnFail) alongside Nektar's
// rate-limited WhatsOnChain scanner for external P2PKH deposits to FundAddress().
type Monitor struct {
	engine   *Engine
	httpCli  *http.Client
	mu       sync.Mutex
	lastScan time.Time
	backoff  *core.KeyBackoff[string]
}

func newMonitor(e *Engine) *Monitor {
	netName := "testnet"
	if e != nil {
		if e.cfg.Mainnet {
			netName = "main"
		} else if e.cfg.InMemoryStore {
			netName = "memory"
		}
	}
	return &Monitor{
		engine:  e,
		httpCli: core.SafeHTTPClientForNetwork(netName, 15*time.Second),
		backoff: core.NewKeyBackoff[string](16384, 30*time.Second, 30*time.Minute),
	}
}

func (m *Monitor) run(ctx context.Context) {
	// Decouple slow external scans (Desktop BRC-29 dial + WoC address sync) AND SendWaiting
	// onto dedicated goroutines so CheckForProofs / network I/O never head-of-line blocks SendWaiting.
	var wg sync.WaitGroup
	wg.Add(2)
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
					_, _ = m.engine.sqlDB.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`)
				}
			case <-fundTick.C:
				if _, _, err := m.SyncFundAddress(ctx); err != nil && !errors.Is(err, ErrThrottled) && ctx.Err() == nil {
					m.engine.logf("wallet monitor: SyncFundAddress warning: %v", err)
				}
			}
		}
	}()
	go func() {
		defer wg.Done()
		sendTick := time.NewTicker(5 * time.Second)
		defer sendTick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sendTick.C:
				if _, err := m.SendWaiting(ctx); err != nil && ctx.Err() == nil {
					m.engine.logf("wallet monitor: SendWaiting warning: %v", err)
				}
			}
		}
	}()
	defer wg.Wait()

	proofTick := time.NewTicker(30 * time.Second)
	defer proofTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
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
	// Clear go-wallet-toolbox's synchronize_tx_statuses_last_block watermark ONLY when an error occurred
	// during status synchronization (never unconditionally on every 30s tick when unproven txs exist,
	// which previously caused an O(N_unproven) status stampede every 30s).
	if err != nil {
		if m.engine.sqlDB != nil {
			_, _ = m.engine.sqlDB.ExecContext(ctx, `DELETE FROM bsv_key_values WHERE key = 'synchronize_tx_statuses_last_block'`)
		}
	}
	return proved, err
}

func (m *Monitor) hasUnprovenTxs(ctx context.Context) bool {
	if m.engine == nil || m.engine.readDB == nil || m.engine.tables.transactions == "" {
		return false
	}
	var count int
	_ = m.engine.readDB.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(1) FROM "%s" WHERE user_id = ? AND status = 'unproven' AND deleted_at IS NULL`, m.engine.tables.transactions),
		m.engine.userID,
	).Scan(&count)
	return count > 0
}
