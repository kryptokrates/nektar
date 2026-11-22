import type { Knex } from 'knex'
import { Transaction } from '@bsv/sdk'
import type { MerklePath } from '@bsv/sdk'

export interface CarrierJournal {
  get(hash: string): Promise<{ txid: string; beef: number[]; topics: string[]; fundingTxid?: string } | undefined>
  put(hash: string, c: { txid: string; beef: number[]; topics: string[]; fundingTxid?: string }): Promise<void>
  /** every stored carrier's index — hash → txid/topics, no beef payload.
   *  A delivery view for diagnostics and tests; get() fetches the payload. */
  list?(): Promise<Array<{ hash: string; txid: string; topics: string[] }>>
  /** rows funded by a tree txid — lets a proof that just landed republish its
   *  carriers with the parent's BUMP attached (the same txid, so hosts
   *  re-record the entry free of the unproven-funding eviction deadline) */
  fundedBy?(txid: string): Promise<Array<{ hash: string; txid: string; beef: number[]; topics: string[] }>>
  /** proof seam — the lanes runtime wires fetchTx here; reads then hand back
   *  a BEEF with the funding parent's BUMP attached (and persist it) instead
   *  of re-serving stale ancestry forever */
  enrich?(txid: string): Promise<MerklePath | undefined>
  del?(hash: string): Promise<void>
  /** a reorg orphaned every merkle path at ≥ fromHeight — strip them from
   *  stored beefs so the enrich seam re-attaches proofs computed against
   *  the new headers on next read. Returns the rows touched. */
  clearPaths?(fromHeight: number): Promise<number>
}

/** stored beef is either subject form — rows journal atomic today, but
 *  legacy token rows journaled plain; try both before calling a row
 *  unparseable */
export const parseBeef = (beef: number[]): Transaction | undefined => {
  try {
    return Transaction.fromAtomicBEEF(beef)
  } catch {
    try {
      return Transaction.fromBEEF(beef)
    } catch {
      return undefined
    }
  }
}

/** enrich a row's stored beef with every ancestor BUMP the proof seam can
 *  now answer — the funding parent is only the shallowest level; trees
 *  funded by a previous tree's change sit deeper and are what the host
 *  walks into next. Returns the enriched beef or undefined */
const enrichBeef = async (
  row: { beef: number[]; fundingTxid?: string },
  enrich?: (txid: string) => Promise<MerklePath | undefined>
): Promise<number[] | undefined> => {
  if (enrich === undefined) return undefined
  const tx = parseBeef(row.beef)
  if (tx === undefined) return undefined
  let changed = false
  const seen = new Set<string>()
  const walk = async (t: Transaction): Promise<void> => {
    for (const i of t.inputs) {
      const s = i.sourceTransaction
      if (s === undefined) continue
      const id = s.id('hex')
      if (seen.has(id)) continue
      seen.add(id)
      if (s.merklePath === undefined) {
        const mp = await enrich(id)
        if (mp !== undefined) {
          s.merklePath = mp
          changed = true
          continue
        }
        await walk(s)
      }
    }
  }
  await walk(tx)
  return changed ? tx.toAtomicBEEF() : undefined
}

/** reorg companion to enrichBeef: drop every embedded path anchored at ≥
 *  fromHeight — the block it proves against is gone, so the path is a stale
 *  claim riding to counterparties. Clearing re-opens the enrich seam on the
 *  next read instead of serving the dead fork's evidence. */
const stripPaths = (beef: number[], fromHeight: number): number[] | undefined => {
  const tx = parseBeef(beef)
  if (tx === undefined) return undefined
  let changed = false
  const seen = new Set<string>()
  const walk = (t: Transaction): void => {
    const id = t.id('hex')
    if (seen.has(id)) return
    seen.add(id)
    if (t.merklePath !== undefined && t.merklePath.blockHeight >= fromHeight) {
      t.merklePath = undefined
      changed = true
    }
    for (const i of t.inputs) if (i.sourceTransaction !== undefined) walk(i.sourceTransaction)
  }
  walk(tx)
  return changed ? tx.toAtomicBEEF() : undefined
}

export class MemoryCarrierJournal implements CarrierJournal {
  public enrich?: (txid: string) => Promise<MerklePath | undefined>
  private m = new Map<string, { txid: string; beef: number[]; topics: string[]; fundingTxid?: string }>()
  async get(hash: string) {
    const row = this.m.get(hash)
    // a failing enrich must not fail the read — same contract the Knex
    // impl keeps: serve the stored beef, retry the attach next call
    const beef = row === undefined ? undefined : await enrichBeef(row, this.enrich).catch(() => undefined)
    if (row !== undefined && beef !== undefined) {
      row.beef = beef
      this.m.set(hash, row)
    }
    return row
  }
  put(hash: string, c: { txid: string; beef: number[]; topics: string[]; fundingTxid?: string }) {
    this.m.set(hash, c)
    return Promise.resolve()
  }
  list() {
    return Promise.resolve([...this.m].map(([hash, c]) => ({ hash, txid: c.txid, topics: c.topics })))
  }
  fundedBy(txid: string) {
    return Promise.resolve([...this.m].filter(([, c]) => c.fundingTxid === txid).map(([hash, c]) => ({ hash, ...c })))
  }
  del(hash: string) {
    this.m.delete(hash)
    return Promise.resolve()
  }
  clearPaths(fromHeight: number) {
    let n = 0
    for (const [hash, row] of this.m) {
      const beef = stripPaths(row.beef, fromHeight)
      if (beef !== undefined) {
        row.beef = beef
        this.m.set(hash, row)
        n++
      }
    }
    return Promise.resolve(n)
  }
}

export class KnexCarrierJournal implements CarrierJournal {
  public enrich?: (txid: string) => Promise<MerklePath | undefined>
  constructor(private kx: Knex) {}
  private ensured?: Promise<void>
  /** create the table up front (migrations/seed copies need the DDL before
   *  they can insert rows; methods self-ensure lazily otherwise) */
  async init(): Promise<void> {
    await this.ensure()
  }
  private async ensure(): Promise<void> {
    return await (this.ensured ??= this.doEnsure().catch((e) => {
      this.ensured = undefined
      throw e
    }))
  }
  private async doEnsure(): Promise<void> {
    if (await this.kx.schema.hasTable('bstack_carriers')) {
      if (!(await this.kx.schema.hasColumn('bstack_carriers', 'funding_txid'))) {
        await this.kx.schema.alterTable('bstack_carriers', (t) => {
          t.string('funding_txid').nullable()
        })
      }
    } else {
      await this.kx.schema.createTable('bstack_carriers', (t) => {
        t.string('artifact_hash').primary()
        t.string('txid').notNullable()
        t.text('beef').notNullable()
        t.text('topics').notNullable()
        t.string('funding_txid').nullable()
        t.bigInteger('created_at').notNullable()
      })
    }
    // one-time migration for pre-fundingTxid journals: the parent a row's
    // input-0 spends IS its funding txid — recover it from the stored beef
    // so legacy rows become republishable/enrichable instead of re-serving
    // stale ancestry forever
    for (const r of await this.kx('bstack_carriers').whereNull('funding_txid').select('artifact_hash', 'beef')) {
      const parent = parseBeef(JSON.parse(r.beef as string) as number[])?.inputs[0]?.sourceTransaction?.id('hex')
      if (parent !== undefined)
        await this.kx('bstack_carriers')
          .where({ artifact_hash: r.artifact_hash as string })
          .update({ funding_txid: parent })
    }
  }
  async get(hash: string) {
    await this.ensure()
    const r = await this.kx('bstack_carriers').where({ artifact_hash: hash }).first()
    if (r === undefined) return undefined
    const row = { txid: r.txid, beef: JSON.parse(r.beef), topics: JSON.parse(r.topics), ...(r.funding_txid != null ? { fundingTxid: r.funding_txid } : {}) }
    const beef = await enrichBeef(row, this.enrich).catch(() => undefined)
    if (beef !== undefined) {
      await this.put(hash, { ...row, beef })
      row.beef = beef
    }
    return row
  }
  async put(hash: string, c: { txid: string; beef: number[]; topics: string[]; fundingTxid?: string }): Promise<void> {
    await this.ensure()
    await this.kx('bstack_carriers')
      .insert({
        artifact_hash: hash,
        txid: c.txid,
        beef: JSON.stringify(c.beef),
        topics: JSON.stringify(c.topics),
        funding_txid: c.fundingTxid ?? null,
        created_at: Date.now()
      })
      // same artifact, same carrier — the beef column alone may be enriched
      // (a funding parent's BUMP attached after the tree proved)
      .onConflict('artifact_hash')
      .merge(['txid', 'beef', 'funding_txid'])
  }
  async list() {
    await this.ensure()
    return (await this.kx('bstack_carriers').select('artifact_hash', 'txid', 'topics')).map((r) => ({
      hash: r.artifact_hash as string,
      txid: r.txid as string,
      topics: JSON.parse(r.topics as string) as string[]
    }))
  }
  async fundedBy(txid: string) {
    await this.ensure()
    return (await this.kx('bstack_carriers').where({ funding_txid: txid }).select('artifact_hash', 'txid', 'beef', 'topics')).map((r) => ({
      hash: r.artifact_hash as string,
      txid: r.txid as string,
      beef: JSON.parse(r.beef as string) as number[],
      topics: JSON.parse(r.topics as string) as string[]
    }))
  }
  async del(hash: string): Promise<void> {
    await this.ensure()
    await this.kx('bstack_carriers').where({ artifact_hash: hash }).delete()
  }
  async clearPaths(fromHeight: number): Promise<number> {
    await this.ensure()
    let n = 0
    // rows are few (one per artifact/finger token); a full pass at reorg
    // time beats a stale path riding to every counterparty forever
    for (const r of await this.kx('bstack_carriers').select('artifact_hash', 'beef')) {
      const beef = stripPaths(JSON.parse(r.beef as string) as number[], fromHeight)
      if (beef !== undefined) {
        await this.kx('bstack_carriers')
          .where({ artifact_hash: r.artifact_hash as string })
          .update({ beef: JSON.stringify(beef) })
        n++
      }
    }
    return n
  }
}
