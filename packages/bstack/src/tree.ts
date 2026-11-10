import { PrivateKey, ProtoWallet, Transaction, P2PKH, type MerklePath } from '@bsv/sdk'
import { fundingLock, sweep as bSweep, feeFor, type Derivation, type Fees } from '@lightwebinc/bcommon'
import type { FundingOut } from '@nektar/chain'
import type { Knex } from 'knex'

export const DERIVATION: Derivation = { protocol: [2, 'nektar market'], keyID: '1' }
export const FUNDING_TAG = new TextEncoder().encode('nektar-fund')
export const ORIGINATOR = 'nektar'
export const MAX_TREE_OUTPUTS = 1023

export async function mintTree (p: { key: PrivateKey, funding: FundingOut, n: number, satsPerOutput?: number, changeScript?: import('@bsv/sdk').LockingScript }): Promise<Transaction> {
  const { key, funding, n } = p
  const sats = p.satsPerOutput ?? 1
  if (!Number.isInteger(n) || n < 1 || n > MAX_TREE_OUTPUTS) throw new Error(`mintTree: n must be 1..${MAX_TREE_OUTPUTS}`)
  if (!Number.isSafeInteger(sats) || sats < 1) throw new Error('mintTree: bad satsPerOutput')
  const w = new ProtoWallet(key)
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: funding.tx, sourceOutputIndex: funding.vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
  const lock = await fundingLock(w, ORIGINATOR, DERIVATION, FUNDING_TAG)
  for (let i = 0; i < n; i++) tx.addOutput({ satoshis: sats, lockingScript: lock })
  tx.addOutput({ lockingScript: p.changeScript ?? new P2PKH().lock(key.toPublicKey().toAddress()), change: true })
  await tx.fee()
  await tx.sign()
  return tx
}

export interface TreeLedger {
  add (tree: Transaction, n: number): Promise<void>
  take (): Promise<{ tree: Transaction, vout: number } | undefined>
  markProven (txid: string, merklePath?: MerklePath): Promise<void>
  unproven (): Promise<string[]>
  remaining (): Promise<number>
}

export class MemoryTreeLedger implements TreeLedger {
  private trees = new Map<string, { tree: Transaction, proven: boolean }>()
  private free = new Set<string>()
  async add (tree: Transaction, n: number): Promise<void> {
    const txid = tree.id('hex')
    this.trees.set(txid, { tree, proven: false })
    for (let i = 0; i < n; i++) this.free.add(`${txid}:${i}`)
  }
  async take (): Promise<{ tree: Transaction, vout: number } | undefined> {
    for (const k of this.free) {
      const [txid, v] = k.split(':')
      if (!this.trees.get(txid!)!.proven) continue
      this.free.delete(k)
      return { tree: this.trees.get(txid!)!.tree, vout: Number(v) }
    }
    return undefined
  }
  async markProven (txid: string, merklePath?: MerklePath): Promise<void> { const t = this.trees.get(txid); if (t) { t.proven = true; if (merklePath !== undefined) t.tree.merklePath = merklePath } }
  async unproven (): Promise<string[]> { return [...this.trees].filter(([, t]) => !t.proven).map(([id]) => id) }
  async remaining (): Promise<number> { let n = 0; for (const k of this.free) if (this.trees.get(k.split(':')[0]!)!.proven) n++; return n }
}

export class KnexTreeLedger implements TreeLedger {
  constructor (private kx: Knex) {}
  private async ensure (): Promise<void> {
    if (!(await this.kx.schema.hasTable('bstack_trees'))) {
      await this.kx.schema.createTable('bstack_trees', t => {
        t.string('txid').primary(); t.integer('n').notNullable(); t.integer('proven').notNullable().defaultTo(0); t.string('beef').notNullable()
      })
    }
    if (!(await this.kx.schema.hasTable('bstack_tree_outputs'))) {
      await this.kx.schema.createTable('bstack_tree_outputs', t => {
        t.string('key').primary(); t.string('txid').notNullable(); t.integer('vout').notNullable()
      })
    }
  }
  async add (tree: Transaction, n: number): Promise<void> {
    await this.ensure()
    const txid = tree.id('hex')
    await this.kx.transaction(async trx => {
      await trx('bstack_trees').insert({ txid, n, proven: tree.merklePath ? 1 : 0, beef: JSON.stringify(tree.toBEEF()) }).onConflict('txid').ignore()
      const rows = Array.from({ length: n }, (_, i) => ({ key: `${txid}:${i}`, txid, vout: i }))
      if (rows.length) await trx('bstack_tree_outputs').insert(rows).onConflict('key').ignore()
    })
  }
  async take (): Promise<{ tree: Transaction, vout: number } | undefined> {
    await this.ensure()
    for (let attempt = 0; attempt < 5; attempt++) {
      const r = await this.kx.transaction(async trx => {
        const row = await trx('bstack_tree_outputs as o').join('bstack_trees as t', 'o.txid', 't.txid').where('t.proven', 1).select('o.key', 'o.txid', 'o.vout').first()
        if (!row) return null
        const n = await trx('bstack_tree_outputs').where({ key: row.key }).del()
        if (n !== 1) return false
        const t = await trx('bstack_trees').where({ txid: row.txid }).first()
        return { tree: Transaction.fromBEEF(JSON.parse(t.beef)), vout: row.vout }
      })
      if (r !== false) return r ?? undefined
    }
    return undefined
  }
  async markProven (txid: string, merklePath?: MerklePath): Promise<void> {
    await this.ensure()
    await this.kx.transaction(async trx => {
      await trx('bstack_trees').where({ txid }).update({ proven: 1 })
      if (merklePath !== undefined) {
        const t = await trx('bstack_trees').where({ txid }).first()
        if (t) { const tree = Transaction.fromBEEF(JSON.parse(t.beef)); tree.merklePath = merklePath; await trx('bstack_trees').where({ txid }).update({ beef: JSON.stringify(tree.toBEEF()) }) }
      }
    })
  }
  async unproven (): Promise<string[]> { await this.ensure(); return (await this.kx('bstack_trees').where({ proven: 0 }).select('txid')).map(r => r.txid) }
  async remaining (): Promise<number> { await this.ensure(); const r = await this.kx('bstack_tree_outputs as o').join('bstack_trees as t', 'o.txid', 't.txid').where('t.proven', 1).count('o.key as c').first(); return Number(r?.c ?? 0) }
}

export { feeFor, type Fees }
