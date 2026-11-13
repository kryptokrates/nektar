import type { Knex } from 'knex'

export interface CarrierJournal {
  get(hash: string): Promise<{ txid: string; beef: number[]; topics: string[] } | undefined>
  put(hash: string, c: { txid: string; beef: number[]; topics: string[]; fundingTxid?: string }): Promise<void>
  /** rows funded by a tree txid — lets a proof that just landed republish its
   *  carriers with the parent's BUMP attached (the same txid, so hosts
   *  re-record the entry free of the unproven-funding eviction deadline) */
  fundedBy?(txid: string): Promise<Array<{ hash: string; txid: string; beef: number[]; topics: string[] }>>
  del?(hash: string): Promise<void>
}

export class MemoryCarrierJournal implements CarrierJournal {
  private m = new Map<string, { txid: string; beef: number[]; topics: string[]; fundingTxid?: string }>()
  async get(hash: string) {
    return this.m.get(hash)
  }
  async put(hash: string, c: { txid: string; beef: number[]; topics: string[]; fundingTxid?: string }) {
    this.m.set(hash, c)
  }
  async fundedBy(txid: string) {
    return [...this.m].filter(([, c]) => c.fundingTxid === txid).map(([hash, c]) => ({ hash, ...c }))
  }
  async del(hash: string) {
    this.m.delete(hash)
  }
}

export class KnexCarrierJournal implements CarrierJournal {
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
      return
    }
    await this.kx.schema.createTable('bstack_carriers', (t) => {
      t.string('artifact_hash').primary()
      t.string('txid').notNullable()
      t.text('beef').notNullable()
      t.text('topics').notNullable()
      t.string('funding_txid').nullable()
      t.bigInteger('created_at').notNullable()
    })
  }
  async get(hash: string) {
    await this.ensure()
    const r = await this.kx('bstack_carriers').where({ artifact_hash: hash }).first()
    return r === undefined ? undefined : { txid: r.txid, beef: JSON.parse(r.beef), topics: JSON.parse(r.topics) }
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
}
