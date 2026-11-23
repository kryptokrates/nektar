// walletmigrate.ts — the wallet-ledger's pre-split migration.
// Wallet state (txs, leases, tx events) is custody bookkeeping and lives in
// records.sqlite with the coin book — take()/leasePredicate and the resubmit
// path read one physical truth. Deployments that predate the split hold the
// same tables inside automation.sqlite — migrateWalletTables copies ROWS for
// exactly these tables (the migrateLaneTables precedent: a file copy would
// drag every automation table in and silently drop WAL-un-checkpointed
// commits).
import type { Knex } from 'knex'

/** every table the wallet ledger owns — the migration boundary */
const WALLET_TABLES = ['wallet_reservations', 'wallet_transactions', 'wallet_tx_events'] as const

/** copy wallet-table rows from a legacy combined db into `dst` — the caller
 *  creates each table through AutomationStore.init() first (DDL stays
 *  single-sourced), then this runs INSERT…SELECT across an ATTACH. Completion
 *  is marked by a store_meta sentinel row, NOT by the file existing — a crash
 *  mid-copy leaves records.sqlite on disk, and gating on file existence would
 *  skip the migration forever (silently partial ledger tables). Re-runs are
 *  safe: the copy is one transaction, and INSERT OR IGNORE keeps any dst rows
 *  written since (they may be newer than src). Returns tables actually copied. */
export async function migrateWalletTables(srcPath: string, dst: Knex): Promise<string[]> {
  await dst.raw('CREATE TABLE IF NOT EXISTS store_meta (key TEXT PRIMARY KEY, value TEXT)')
  if ((await dst.raw(`SELECT value FROM store_meta WHERE key = 'wallet_migrated'`)).length > 0) return []
  await dst.raw('ATTACH DATABASE ? AS src', [srcPath])
  try {
    return await dst.transaction(async (trx) => {
      const copied: string[] = []
      for (const t of WALLET_TABLES) {
        const has = await trx.raw(`SELECT name FROM src.sqlite_master WHERE type='table' AND name = ?`, [t])
        if (has.length === 0) continue
        // copy the column intersection only — a src table written before a
        // column landed (schema drift) must still migrate; new dst columns
        // take their DDL defaults
        const srcCols = (await trx.raw('PRAGMA src.table_info(??)', [t])).map((r: { name: string }) => r.name)
        const dstCols = new Set((await trx.raw('PRAGMA main.table_info(??)', [t])).map((r: { name: string }) => r.name))
        const cols = srcCols.filter((c: string) => dstCols.has(c))
        if (cols.length === 0) continue
        const ph = cols.map(() => '??').join(', ')
        await trx.raw(`INSERT OR IGNORE INTO ?? (${ph}) SELECT ${ph} FROM src.??`, [t, ...cols, ...cols, t])
        copied.push(t)
      }
      await trx.raw(`INSERT INTO store_meta (key, value) VALUES ('wallet_migrated', ?)`, [new Date().toISOString()])
      return copied
    })
  } finally {
    await dst.raw('DETACH DATABASE src')
  }
}
