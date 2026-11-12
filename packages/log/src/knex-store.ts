// KnexShardStore — Postgres (or sqlite for tests) backed shard store.
// Table: nektar_log(shard, seq, leaf, hdp, fragment_json, state, batch_index,
// leaf_index, anchor_txid, PK(shard,seq), UNIQUE(shard,hdp))
import type { Knex } from 'knex'
import { fromHex, toHex } from '@nektar/core'
import type { Fragment } from '@nektar/core'
import type { LogRecord, ShardStore } from './store.js'

const TABLE = 'nektar_log'

export class KnexShardStore implements ShardStore {
  constructor(private knex: Knex<any, any>) {}

  async init(): Promise<void> {
    const exists = await this.knex.schema.hasTable(TABLE)
    if (!exists) {
      await this.knex.schema.createTable(TABLE, (t) => {
        t.string('shard').notNullable()
        t.integer('seq').notNullable()
        t.binary('leaf').notNullable()
        t.binary('hdp').notNullable()
        t.text('fragment_json').notNullable()
        t.string('state').notNullable()
        t.integer('batch_index')
        t.integer('leaf_index')
        t.string('anchor_txid')
        t.text('meta_json')
        t.primary(['shard', 'seq'])
        t.unique(['shard', 'hdp'])
        t.index(['shard', 'state', 'batch_index'], 'nektar_log_pending_idx')
      })
    } else if (!(await this.knex.schema.hasColumn(TABLE, 'meta_json'))) {
      await this.knex.schema.alterTable(TABLE, (t) => {
        t.text('meta_json')
      })
    }
    // hot scan: committedUnbatched runs on every remember + flush tick
    await this.knex.raw('CREATE INDEX IF NOT EXISTS nektar_log_pending_idx ON ?? (??, ??, ??)', [TABLE, 'shard', 'state', 'batch_index'])
  }

  private row(r: LogRecord) {
    return {
      shard: r.shard,
      seq: r.seq,
      leaf: Buffer.from(r.leaf),
      hdp: Buffer.from(r.hdp),
      fragment_json: JSON.stringify(encodeFragment(r.fragment)),
      state: r.state,
      batch_index: r.batchIndex ?? null,
      leaf_index: r.leafIndex ?? null,
      anchor_txid: r.anchorTxid ?? null,
      meta_json: JSON.stringify({ level: r.level, authorPub: r.authorPub, authorEpoch: r.authorEpoch })
    }
  }

  private rec(row: any): LogRecord {
    return {
      shard: row.shard,
      seq: row.seq,
      leaf: new Uint8Array(row.leaf),
      hdp: new Uint8Array(row.hdp),
      fragment: decodeFragment(JSON.parse(row.fragment_json)),
      state: row.state,
      batchIndex: row.batch_index ?? undefined,
      leafIndex: row.leaf_index ?? undefined,
      anchorTxid: row.anchor_txid ?? undefined,
      ...(row.meta_json ? (JSON.parse(row.meta_json) as Pick<LogRecord, 'level' | 'authorPub' | 'authorEpoch'>) : {})
    }
  }

  async append(rec: LogRecord): Promise<void> {
    await this.knex(TABLE).insert(this.row(rec))
  }
  async byIdentity(shard: string, hdpHex: string): Promise<LogRecord | undefined> {
    const row = await this.knex(TABLE)
      .where({ shard, hdp: Buffer.from(hdpHex, 'hex') })
      .first()
    return row && this.rec(row)
  }
  async bySeq(shard: string, seq: number): Promise<LogRecord | undefined> {
    const row = await this.knex(TABLE).where({ shard, seq }).first()
    return row && this.rec(row)
  }
  async byBatch(shard: string, batchIndex: number): Promise<LogRecord[]> {
    return (await this.knex(TABLE).where({ shard, batch_index: batchIndex }).orderBy('leaf_index')).map((r) => this.rec(r))
  }
  async nextSeq(shard: string): Promise<number> {
    const row = await this.knex(TABLE).where({ shard }).max('seq as m').first()
    return (row?.m ?? -1) + 1
  }
  async pending(shard: string): Promise<LogRecord[]> {
    return (await this.knex(TABLE).where({ shard, state: 'committed' }).orderBy('seq')).map((r) => this.rec(r))
  }
  async committedUnbatched(shard: string): Promise<LogRecord[]> {
    return (await this.knex(TABLE).where({ shard, state: 'committed' }).whereNull('batch_index').orderBy('seq')).map((r) => this.rec(r))
  }
  async resetUnanchored(shard: string): Promise<number> {
    return await this.knex(TABLE).where({ shard, state: 'committed' }).whereNotNull('batch_index').update({ batch_index: null, leaf_index: null })
  }
  async markBatched(shard: string, seqs: number[], batchIndex: number): Promise<void> {
    // one transaction — a partial batch assignment is the same orphan class as
    // a dropped anchor, only finer-grained
    const leafIndex = new Map(seqs.map((s, i) => [s, i] as const))
    await this.knex.transaction(async (trx) => {
      for (const [seq, idx] of leafIndex) {
        await trx(TABLE).where({ shard, seq }).update({ batch_index: batchIndex, leaf_index: idx })
      }
    })
  }
  async markAnchored(shard: string, batchIndex: number, txid: string): Promise<void> {
    await this.knex(TABLE).where({ shard, batch_index: batchIndex }).update({ state: 'anchored', anchor_txid: txid })
  }
  async all(shard: string): Promise<LogRecord[]> {
    return (await this.knex(TABLE).where({ shard }).orderBy('seq')).map((r) => this.rec(r))
  }
  async lastBatchIndex(shard: string): Promise<number> {
    const row = await this.knex(TABLE).where({ shard }).max('batch_index as m').first()
    return row?.m ?? -1
  }
}

// Fragments contain Uint8Array fields — JSON-safe encode.
function encodeFragment(f: Fragment): any {
  return {
    d: toHex(f.d),
    p: { ...f.p, signature: toHex(f.p.signature) },
    hdp: f.hdp ? toHex(f.hdp) : undefined,
    E: f.E ? toHex(f.E) : undefined
  }
}
function decodeFragment(j: any): Fragment {
  return {
    d: fromHex(j.d),
    p: { ...j.p, signature: fromHex(j.p.signature) },
    hdp: j.hdp ? fromHex(j.hdp) : undefined,
    E: j.E ? fromHex(j.E) : undefined
  }
}
