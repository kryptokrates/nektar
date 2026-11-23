// sqlfunds.ts — the coin book as a sqlite table (Stage 3 custody).
//
// One store, no parallel truth: `utxos` rows carry the outpoint bookkeeping
// (live/dropped — 'dropped' IS the tombstone), transaction bytes and proofs
// stay in TxStore where they already live. Every FundsStore verb is a local
// SQL statement — adopt/list/drop/revive cost zero network calls, so funding
// imports and discovery no longer pay an AtomicBEEF ancestry build per coin
// (the internalize-storm failure class), and restart state is the table
// itself rather than a re-run of wallet ingest.
import { Transaction } from '@bsv/sdk'
import { FundsBase, TxStore, type FundingOut, type FundsDeps, type FundsStore } from '@nektar/chain'
import type { Knex } from 'knex'

type Row = { txid: string; vout: number; satoshis: number; state: 'live' | 'dropped'; created_at: number }
const LIVE = 'live' as const
const DROPPED = 'dropped' as const

/** knex-backed FundsStore + FundsBase — the self-custody coin book for both
 *  wallet modes (held key and external substrate sign identically). The
 *  mirror `cache` serves the synchronous `utxos` getter; the table is truth
 *  and every verb writes through before the mirror moves, so a single writer
 *  can never drift the two apart. */
export class SqlFunds extends FundsBase {
  private cache: FundingOut[] = []
  protected readonly store: FundsStore
  private log: (s: string) => void

  private constructor(
    private knex: Knex,
    private txs: TxStore,
    deps: FundsDeps & { log?: (s: string) => void }
  ) {
    super(deps)
    this.log = deps.log ?? (() => {})
    this.store = {
      list: async () => await this.live(),
      has: async (txid, vout) => (await this.state(txid, vout)) === LIVE,
      add: async (tx, vout) => {
        this.txs.put(tx)
        await this.set(tx, vout, LIVE)
      },
      drop: async (txid, vout) => {
        // tombstone only what was ours — a foreign input consumed by our tx
        // must not become revivable (it was never ours to bring back). The
        // state guard rides the UPDATE itself: one atomic transition, never
        // a check-then-write pair.
        const n = await this.knex('utxos').where({ txid, vout, state: LIVE }).update({ state: DROPPED })
        if (n > 0) this.cache = this.cache.filter((u) => !(u.tx.id('hex') === txid && u.vout === vout))
      },
      revive: async (tx, vout) => {
        this.txs.put(tx)
        await this.set(tx, vout, LIVE)
      },
      tombstoned: async (txid, vout) => (await this.state(txid, vout)) === DROPPED,
      tombstonedList: async () => {
        const rows = (await this.knex<Row>('utxos').where({ state: DROPPED }).select('txid', 'vout', 'satoshis')) as Row[]
        return rows.map((r) => ({ txid: r.txid, vout: r.vout, satoshis: r.satoshis, tx: this.txs.get(r.txid) }))
      }
    }
  }

  /** open/create the `utxos` table and load the live mirror — call once at
   *  boot with the records-db knex and the instance TxStore. */
  static async open(knex: Knex, txs: TxStore, deps: FundsDeps & { log?: (s: string) => void }): Promise<SqlFunds> {
    if (!(await knex.schema.hasTable('utxos'))) {
      await knex.schema.createTable('utxos', (t) => {
        t.string('txid').notNullable()
        t.integer('vout').notNullable()
        t.integer('satoshis').notNullable()
        t.string('state').notNullable()
        t.bigInteger('created_at').notNullable()
        t.primary(['txid', 'vout'])
      })
    }
    // take()/refresh() filter by state — backstop for when the coin book
    // outgrows the in-memory mirror's warm set
    await knex.raw('CREATE INDEX IF NOT EXISTS utxos_state_index ON utxos (state)')
    const f = new SqlFunds(knex, txs, deps)
    await f.refresh()
    return f
  }

  /** is this outpoint a live, owned coin? — callers check before re-importing */
  async has(txid: string, vout: number): Promise<boolean> {
    return (await this.state(txid, vout)) === LIVE
  }

  /** synchronous live view — `waitForFunding`-style callers poll this */
  get utxos(): FundingOut[] {
    return this.cache
  }

  /** balance is a row count, not a rehydrated-out count: a live row whose tx
   *  the store can't return is still our money — unspendable until the bytes
   *  resolve, but never invisible. `take`/`list` still skip it (you can't
   *  build an input on a tx you don't hold). */
  async balance(): Promise<number> {
    const rows = (await this.knex<Row>('utxos').where({ state: LIVE }).select('txid', 'vout', 'satoshis')) as Row[]
    let sum = 0
    for (const r of rows) if (!this.reserved.has(`${r.txid}:${r.vout}`)) sum += r.satoshis
    return sum
  }

  async refresh(): Promise<void> {
    await this.live()
  }

  private async state(txid: string, vout: number): Promise<string | undefined> {
    const r = (await this.knex<Row>('utxos').where({ txid, vout }).select('state').first()) as Row | undefined
    return r?.state
  }

  private async set(tx: Transaction, vout: number, state: 'live' | 'dropped'): Promise<void> {
    const txid = tx.id('hex')
    const satoshis = tx.outputs[vout]?.satoshis ?? 0
    await this.knex('utxos').insert({ txid, vout, satoshis, state, created_at: Date.now() }).onConflict(['txid', 'vout']).merge({ satoshis, state })
    const fo: FundingOut = { tx, vout, satoshis }
    const i = this.cache.findIndex((u) => u.tx.id('hex') === txid && u.vout === vout)
    if (i >= 0) {
      if (state === LIVE) this.cache[i] = fo
      else this.cache.splice(i, 1)
    } else if (state === LIVE) this.cache.push(fo)
  }

  /** live rows rehydrated through TxStore — proofs and linked ancestry ride
   *  the stored tx (a row whose tx the store no longer holds is unspendable
   *  stock: logged and passed over, same verdict `take` gives unserializable
   *  parents). */
  /** live rows whose tx bytes the store can't return — counted by balance()
   *  (the money is ours) but unspendable until the bytes resolve; surfaced so
   *  "owned but unusable" stock is a visible wedge, not a silent skip */
  async unspendableSats(): Promise<number> {
    const rows = (await this.knex<Row>('utxos').where({ state: LIVE }).select('txid', 'vout', 'satoshis')) as Row[]
    let sum = 0
    for (const r of rows) if (this.txs.get(r.txid) === undefined) sum += r.satoshis
    return sum
  }

  private async live(): Promise<FundingOut[]> {
    const rows = (await this.knex<Row>('utxos').where({ state: LIVE }).select('txid', 'vout', 'satoshis')) as Row[]
    const out: FundingOut[] = []
    let unspendable = 0,
      unspendableSats = 0
    for (const r of rows) {
      const tx = this.txs.get(r.txid)
      if (tx === undefined) {
        unspendable++
        unspendableSats += r.satoshis
        continue
      }
      out.push({ tx, vout: r.vout, satoshis: r.satoshis })
    }
    if (unspendable > 0)
      this.log(`sqlfunds: ${unspendable} live row(s) totaling ${unspendableSats} sats have no stored tx — unspendable until the bytes resolve`)
    this.cache = out
    return out
  }
}
