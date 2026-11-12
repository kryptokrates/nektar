import type { Knex } from 'knex'

export interface CarrierJournal {
  get(hash: string): Promise<{ txid: string; beef: number[]; topics: string[] } | undefined>
  put(hash: string, c: { txid: string; beef: number[]; topics: string[] }): Promise<void>
  del?(hash: string): Promise<void>
}

export class MemoryCarrierJournal implements CarrierJournal {
  private m = new Map<string, { txid: string; beef: number[]; topics: string[] }>()
  async get(hash: string) {
    return this.m.get(hash)
  }
  async put(hash: string, c: { txid: string; beef: number[]; topics: string[] }) {
    this.m.set(hash, c)
  }
  async del(hash: string) {
    this.m.delete(hash)
  }
}

export class KnexCarrierJournal implements CarrierJournal {
  constructor(private kx: Knex) {}
  private ensured?: Promise<void>
  private async ensure(): Promise<void> {
    return await (this.ensured ??= this.doEnsure().catch((e) => {
      this.ensured = undefined
      throw e
    }))
  }
  private async doEnsure(): Promise<void> {
    if (!(await this.kx.schema.hasTable('bstack_carriers'))) {
      await this.kx.schema.createTable('bstack_carriers', (t) => {
        t.string('artifact_hash').primary()
        t.string('txid').notNullable()
        t.text('beef').notNullable()
        t.text('topics').notNullable()
        t.bigInteger('created_at').notNullable()
      })
    }
  }
  async get(hash: string) {
    await this.ensure()
    const r = await this.kx('bstack_carriers').where({ artifact_hash: hash }).first()
    return r === undefined ? undefined : { txid: r.txid, beef: JSON.parse(r.beef), topics: JSON.parse(r.topics) }
  }
  async put(hash: string, c: { txid: string; beef: number[]; topics: string[] }): Promise<void> {
    await this.ensure()
    await this.kx('bstack_carriers')
      .insert({ artifact_hash: hash, txid: c.txid, beef: JSON.stringify(c.beef), topics: JSON.stringify(c.topics), created_at: Date.now() })
      .onConflict('artifact_hash')
      .ignore()
  }
  async del(hash: string): Promise<void> {
    await this.ensure()
    await this.kx('bstack_carriers').where({ artifact_hash: hash }).delete()
  }
}
