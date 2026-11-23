// walletmigrate.ts — the wallet-ledger's pre-split migration.
// Wallet state (txs, leases, tx events) is custody bookkeeping and lives in
// records.sqlite with the coin book — take()/leasePredicate and the resubmit
// path read one physical truth. Deployments that predate the split hold the
// same tables inside automation.sqlite — migrateWalletTables copies ROWS for
// exactly these tables (the migrateLaneTables precedent: a file copy would
// drag every automation table in and silently drop WAL-un-checkpointed
// commits).
import type { Knex } from 'knex'
import { copyTablesAcrossAttach } from '@nektar/bstack'

/** every table the wallet ledger owns — the migration boundary */
const WALLET_TABLES = ['wallet_reservations', 'wallet_transactions', 'wallet_tx_events'] as const

/** copy wallet-table rows from a legacy combined db into `dst` — the caller
 *  creates each table through AutomationStore.init() first (DDL stays
 *  single-sourced), then this runs the shared ATTACH row-copy
 *  (copyTablesAcrossAttach — the sentinel-gated algorithm bstack's lane
 *  migration also uses). Returns tables actually copied. */
export async function migrateWalletTables(srcPath: string, dst: Knex): Promise<string[]> {
  return copyTablesAcrossAttach(dst, srcPath, WALLET_TABLES, { table: 'store_meta', key: 'wallet_migrated' })
}
