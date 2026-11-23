// lanes.ts — lane-table ownership + the pre-split migration.
// Lane infrastructure (funding trees, carrier journal, finger chain, log
// anchors) lives in its own sqlite file so an op-db loss can't take carrier
// funding with it. Deployments that predate the split hold the same tables
// inside automation.sqlite — migrateLaneTables copies ROWS for exactly these
// tables. A file copy would drag every automation table into lanes.sqlite
// and silently drop WAL-un-checkpointed commits.
import type { Knex } from 'knex'
import { KnexTreeLedger } from './tree.js'
import { KnexCarrierJournal } from './journal.js'
import { KnexLogState } from './log.js'
import { KnexFingerState } from './finger.js'

/** every table the lane stores own — the migration boundary */
export const LANE_TABLES = ['bstack_trees', 'bstack_tree_outputs', 'bstack_carriers', 'bstack_log_state', 'bstack_finger_state'] as const

/** copy lane-table rows from a legacy combined db into `dst` — creates each
 *  lane table through its owning store first (DDL stays single-sourced),
 *  then INSERT…SELECT across an ATTACH. Returns tables actually copied.
 *  Completion is marked by a bstack_meta sentinel row, NOT by the file
 *  existing — a crash mid-copy leaves lanes.sqlite on disk, and gating on
 *  file existence would skip the migration forever (silently partial lane
 *  tables). Re-runs are safe: the copy is one transaction, and INSERT OR
 *  IGNORE keeps any dst rows written since (they may be newer than src). */
export async function migrateLaneTables(srcPath: string, dst: Knex): Promise<string[]> {
  await Promise.all([new KnexTreeLedger(dst).init(), new KnexCarrierJournal(dst).init(), new KnexLogState(dst).init(), new KnexFingerState(dst).init()])
  return copyTablesAcrossAttach(dst, srcPath, LANE_TABLES, { table: 'bstack_meta', key: 'lanes_migrated' })
}

/** the row-copy migration algorithm, one copy: ATTACH src, then in one
 *  transaction INSERT…SELECT each of `tables` over the column intersection
 *  (schema drift — a src table written before a column landed still migrates;
 *  new dst columns take their DDL defaults), and write a sentinel row marking
 *  completion. The sentinel, NOT the dst file existing, is the gate — a crash
 *  mid-copy leaves the file on disk, and gating on existence would skip the
 *  migration forever (silently partial tables). Re-runs are safe: INSERT OR
 *  IGNORE keeps any dst rows written since (they may be newer than src).
 *  Returns tables actually copied; [] when the sentinel is already set. */
export async function copyTablesAcrossAttach(
  dst: Knex,
  srcPath: string,
  tables: readonly string[],
  sentinel: { table: string; key: string }
): Promise<string[]> {
  await dst.raw(`CREATE TABLE IF NOT EXISTS ${sentinel.table} (key TEXT PRIMARY KEY, value TEXT)`)
  if ((await dst.raw(`SELECT value FROM ${sentinel.table} WHERE key = ?`, [sentinel.key])).length > 0) return []
  await dst.raw('ATTACH DATABASE ? AS src', [srcPath])
  try {
    return await dst.transaction(async (trx) => {
      const copied: string[] = []
      for (const t of tables) {
        const has = await trx.raw(`SELECT name FROM src.sqlite_master WHERE type='table' AND name = ?`, [t])
        if (has.length === 0) continue
        const srcCols = (await trx.raw('PRAGMA src.table_info(??)', [t])).map((r: { name: string }) => r.name)
        const dstCols = new Set((await trx.raw('PRAGMA main.table_info(??)', [t])).map((r: { name: string }) => r.name))
        const cols = srcCols.filter((c: string) => dstCols.has(c))
        if (cols.length === 0) continue
        const ph = cols.map(() => '??').join(', ')
        await trx.raw(`INSERT OR IGNORE INTO ?? (${ph}) SELECT ${ph} FROM src.??`, [t, ...cols, ...cols, t])
        copied.push(t)
      }
      await trx.raw(`INSERT INTO ${sentinel.table} (key, value) VALUES (?, ?)`, [sentinel.key, new Date().toISOString()])
      return copied
    })
  } finally {
    await dst.raw('DETACH DATABASE src')
  }
}
