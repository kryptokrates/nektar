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
 *  then INSERT…SELECT across an ATTACH. Returns tables actually copied. */
export async function migrateLaneTables(srcPath: string, dst: Knex): Promise<string[]> {
  await Promise.all([new KnexTreeLedger(dst).init(), new KnexCarrierJournal(dst).init(), new KnexLogState(dst).init(), new KnexFingerState(dst).init()])
  await dst.raw('ATTACH DATABASE ? AS src', [srcPath])
  try {
    const copied: string[] = []
    for (const t of LANE_TABLES) {
      const has = await dst.raw(`SELECT name FROM src.sqlite_master WHERE type='table' AND name = ?`, [t])
      if (has.length === 0) continue
      // copy the column intersection only — a src table written before a
      // column landed (schema drift) must still migrate; new dst columns
      // take their DDL defaults
      const srcCols = (await dst.raw('PRAGMA src.table_info(??)', [t])).map((r: { name: string }) => r.name)
      const dstCols = new Set((await dst.raw('PRAGMA main.table_info(??)', [t])).map((r: { name: string }) => r.name))
      const cols = srcCols.filter((c: string) => dstCols.has(c))
      if (cols.length === 0) continue
      const ph = cols.map(() => '??').join(', ')
      await dst.raw(`INSERT INTO ?? (${ph}) SELECT ${ph} FROM src.??`, [t, ...cols, ...cols, t])
      copied.push(t)
    }
    return copied
  } finally {
    await dst.raw('DETACH DATABASE src')
  }
}
