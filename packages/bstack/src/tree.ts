import { PrivateKey, ProtoWallet, Transaction, P2PKH, MerklePath } from '@bsv/sdk'
import { fundingLock, feeFor, type Derivation, type Fees, type SigningWallet } from '@lightwebinc/bcommon'
import { unlockP2PKH, signerAddress, type SignerLike } from '@nektar/core'
import type { FundingOut } from '@nektar/chain'
import type { Knex } from 'knex'

export const DERIVATION: Derivation = { protocol: [2, 'nektar market'], keyID: '1' }
export const FUNDING_TAG = new TextEncoder().encode('nektar-fund')
export const ORIGINATOR = 'nektar'
export const MAX_TREE_OUTPUTS = 1023

export async function mintTree(p: {
  key: SignerLike
  wallet?: SigningWallet
  funding: FundingOut
  n: number
  satsPerOutput?: number
  lock?: import('@bsv/sdk').LockingScript
  changeScript?: import('@bsv/sdk').LockingScript
}): Promise<Transaction> {
  const { key, funding, n } = p
  const sats = p.satsPerOutput ?? 1
  if (!Number.isInteger(n) || n < 1 || n > MAX_TREE_OUTPUTS) throw new Error(`mintTree: n must be 1..${MAX_TREE_OUTPUTS}`)
  if (!Number.isSafeInteger(sats) || sats < 1) throw new Error('mintTree: bad satsPerOutput')
  const w = p.wallet ?? (key instanceof PrivateKey ? new ProtoWallet(key) : undefined)
  if (!w) throw new Error('mintTree: wallet required when key is a signer')
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: funding.tx, sourceOutputIndex: funding.vout, unlockingScriptTemplate: unlockP2PKH(key) })
  const lock = p.lock ?? (await fundingLock(w, ORIGINATOR, DERIVATION, FUNDING_TAG))
  for (let i = 0; i < n; i++) tx.addOutput({ satoshis: sats, lockingScript: lock })
  tx.addOutput({ lockingScript: p.changeScript ?? new P2PKH().lock(await signerAddress(key)), change: true })
  await tx.fee()
  await tx.sign()
  return tx
}

// A broadcaster BUMP can prove several transactions of one block at once, and
// marks a sibling leaf as a txid when it is another of the client's own
// transactions; hosts admit a carrier only when the proof is the funding
// tree's path alone, with its sibling an ordinary hash. An odd-count block
// duplicates the last leaf at the next offset — extract() re-flags that copy
// because it finds the txid by hash, so the flag must be restored to the
// offset the source path flagged.
const ownPath = (tree: Transaction): Transaction => {
  if (tree.merklePath === undefined) return tree
  const txid = tree.id('hex')
  const own = tree.merklePath.path[0]?.find((e) => e.txid === true && e.hash === txid)?.offset ?? tree.merklePath.indexOf(txid)
  const mp = tree.merklePath.extract([txid])
  tree.merklePath = new MerklePath(
    mp.blockHeight,
    mp.path.map((level, i) =>
      i !== 0
        ? level
        : level.map((e) => (e.hash === txid && e.offset === own ? { offset: e.offset, hash: e.hash, txid: true } : { offset: e.offset, hash: e.hash }))
    )
  )
  // a BUMP-anchored tx terminates BEEF ancestry (same rule markProven
  // applies): parents left attached ride into carrier beefs and get the
  // carrier refused by hosts that count transactions strictly
  for (const i of tree.inputs) i.sourceTransaction = undefined
  return tree
}

export interface TreeLedger {
  add(tree: Transaction, n: number, kind?: string): Promise<void>
  /**
   *  Hands out the next free slot. `expectedLockHex`, when given, must equal
   *  the leaf output's locking script hex — a stored tree minted under an
   *  older identity produces carriers that always fail script evaluation, so
   *  its slots are consumed and skipped instead of handed out.
   */
  take(kind?: string, expectedLockHex?: string): Promise<{ tree: Transaction; vout: number } | undefined>
  markProven(txid: string, merklePath?: MerklePath): Promise<void>
  /** a funding parent's own proof: write it into every stored tree whose
   *  input ancestry still shows that parent unproven — after this the tree
   *  is ancestry-anchored and its slots serve. true when a beef changed. */
  attachParentPath(txid: string, merklePath: MerklePath): Promise<boolean>
  unproven(): Promise<Array<{ txid: string; kind: string }>>
  /** remove a tree + all its slots — a mint that never landed on-chain
   *  (rejected / evicted) would otherwise sit in unproven() forever and gate
   *  out every future mint of that kind */
  drop(txid: string): Promise<void>
  remaining(kind?: string): Promise<number>
}

// a tree hosts may fund-accept before it mines: proven, or one unconfirmed
// level deep on anchored ancestry — every input's source carries a merkle
// path (the same §10.3 rule finger applies to tokens). The tree still lists
// in unproven() so its own BUMP attaches when the topup mines.
export const anchoredTree = (tree: Transaction): boolean =>
  tree.merklePath !== undefined || (tree.inputs.length > 0 && tree.inputs.every((i) => i.sourceTransaction?.merklePath !== undefined))

// only the market lane tolerates unproven funding: the nektar module accepts
// anchored ancestry, but the vendored bbox/blogs/finger modules still demand
// the tree's own path — serving them zero-conf slots would mint carriers
// they refuse (and submitAll would count the refusal as delivered)
const serves = (tree: { tree: Transaction; proven: boolean }, kind: string): boolean => tree.proven || (kind === 'market' && anchoredTree(tree.tree))

export class MemoryTreeLedger implements TreeLedger {
  private trees = new Map<string, { tree: Transaction; proven: boolean; kind: string }>()
  private free = new Set<string>()
  async add(tree: Transaction, n: number, kind = 'market'): Promise<void> {
    const txid = tree.id('hex')
    this.trees.set(txid, { tree, proven: false, kind })
    for (let i = 0; i < n; i++) this.free.add(`${txid}:${i}`)
  }
  async take(kind = 'market', expectedLockHex?: string): Promise<{ tree: Transaction; vout: number } | undefined> {
    for (const k of this.free) {
      const [txid, v] = k.split(':')
      const t = this.trees.get(txid)!
      if (!serves(t, kind) || t.kind !== kind) continue
      this.free.delete(k)
      if (expectedLockHex !== undefined && t.tree.outputs[Number(v)]?.lockingScript.toHex() !== expectedLockHex) continue
      return { tree: ownPath(t.tree), vout: Number(v) }
    }
    return undefined
  }
  async markProven(txid: string, merklePath?: MerklePath): Promise<void> {
    const t = this.trees.get(txid)
    if (t) {
      t.proven = true
      if (merklePath !== undefined) {
        t.tree.merklePath = merklePath
        // a BUMP-anchored tx terminates BEEF ancestry: unproven parents left
        // attached would be refused by hosts for missing source transactions
        for (const i of t.tree.inputs) i.sourceTransaction = undefined
      }
    }
  }
  async attachParentPath(txid: string, merklePath: MerklePath): Promise<boolean> {
    let changed = false
    for (const t of this.trees.values()) {
      if (t.proven) continue
      for (const i of t.tree.inputs) {
        const src = i.sourceTransaction
        if (src !== undefined && src.id('hex') === txid && src.merklePath === undefined) {
          src.merklePath = merklePath
          changed = true
        }
      }
    }
    return changed
  }
  async unproven(): Promise<Array<{ txid: string; kind: string }>> {
    return [...this.trees].filter(([, t]) => !t.proven).map(([id, t]) => ({ txid: id, kind: t.kind }))
  }
  async drop(txid: string): Promise<void> {
    this.trees.delete(txid)
    for (const k of [...this.free]) if (k.startsWith(`${txid}:`)) this.free.delete(k)
  }
  async remaining(kind = 'market'): Promise<number> {
    let n = 0
    for (const k of this.free) {
      const t = this.trees.get(k.split(':')[0])!
      if (serves(t, kind) && t.kind === kind) n++
    }
    return n
  }
}

export class KnexTreeLedger implements TreeLedger {
  constructor(private kx: Knex) {}
  private ensured?: Promise<void>
  /** create the lane tables up front (migrations/seed copies need the DDL
   *  before they can insert rows; methods self-ensure lazily otherwise) */
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
    if ((await this.kx.schema.hasTable('bstack_trees')) && !(await this.kx.schema.hasColumn('bstack_trees', 'kind'))) {
      await this.kx.schema.alterTable('bstack_trees', (t) => {
        t.string('kind').notNullable().defaultTo('market')
      })
      await this.kx.schema.alterTable('bstack_tree_outputs', (t) => {
        t.string('kind').notNullable().defaultTo('market')
      })
    }
    if (!(await this.kx.schema.hasTable('bstack_trees'))) {
      await this.kx.schema.createTable('bstack_trees', (t) => {
        t.string('txid').primary()
        t.integer('n').notNullable()
        t.integer('proven').notNullable().defaultTo(0)
        t.string('beef').notNullable()
        t.string('kind').notNullable().defaultTo('market')
      })
    }
    if (!(await this.kx.schema.hasTable('bstack_tree_outputs'))) {
      await this.kx.schema.createTable('bstack_tree_outputs', (t) => {
        t.string('key').primary()
        t.string('txid').notNullable()
        t.integer('vout').notNullable()
        t.string('kind').notNullable().defaultTo('market')
      })
    }
  }
  async add(tree: Transaction, n: number, kind = 'market'): Promise<void> {
    await this.ensure()
    const txid = tree.id('hex')
    await this.kx.transaction(async (trx) => {
      await trx('bstack_trees')
        .insert({ txid, n, proven: tree.merklePath ? 1 : 0, beef: JSON.stringify(tree.toBEEF()), kind })
        .onConflict('txid')
        .ignore()
      const rows = Array.from({ length: n }, (_, i) => ({ key: `${txid}:${i}`, txid, vout: i, kind }))
      if (rows.length) await trx('bstack_tree_outputs').insert(rows).onConflict('key').ignore()
    })
  }
  async take(kind = 'market', expectedLockHex?: string): Promise<{ tree: Transaction; vout: number } | undefined> {
    await this.ensure()
    for (let attempt = 0; attempt < 5;) {
      const r = await this.kx.transaction(async (trx) => {
        const row = await trx('bstack_tree_outputs as o')
          .join('bstack_trees as t', 'o.txid', 't.txid')
          .where('t.proven', 1)
          .where('o.kind', kind)
          .select('o.key', 'o.txid', 'o.vout')
          .first()
        if (!row) return null
        const n = await trx('bstack_tree_outputs').where({ key: row.key }).del()
        if (n !== 1) return false
        const t = await trx('bstack_trees').where({ txid: row.txid }).first()
        const tree = ownPath(Transaction.fromBEEF(JSON.parse(t.beef)))
        if (tree.merklePath === undefined) {
          // a proven=1 row whose beef lost the tree's own path can't anchor a
          // carrier — the leaf is burned already; flip the row back so the
          // upkeep poll re-fetches the proof instead of serving bad beef
          await trx('bstack_trees').where({ txid: row.txid }).update({ proven: 0 })
          return 'skip'
        }
        if (expectedLockHex !== undefined && tree.outputs[row.vout]?.lockingScript.toHex() !== expectedLockHex) return 'skip'
        return { tree, vout: row.vout }
      })
      if (r === 'skip') continue // poisoned leaf consumed — doesn't count against the race budget
      attempt++
      if (r !== false && r !== null) return r
      if (r !== false) break
    }
    // zero-conf path (market lane only): an unproven tree whose ancestry is
    // anchored is host-admissible — carriers step back to a mined anchor
    if (kind !== 'market') return undefined
    for (const row of await this.kx('bstack_tree_outputs as o')
      .join('bstack_trees as t', 'o.txid', 't.txid')
      .where('t.proven', 0)
      .where('o.kind', kind)
      .select('o.key', 'o.txid', 'o.vout', 't.beef')) {
      const tree = Transaction.fromBEEF(JSON.parse(row.beef as string))
      if (!anchoredTree(tree)) continue
      if (expectedLockHex !== undefined && tree.outputs[row.vout]?.lockingScript.toHex() !== expectedLockHex) {
        await this.kx('bstack_tree_outputs').where({ key: row.key }).del()
        continue
      }
      if ((await this.kx('bstack_tree_outputs').where({ key: row.key }).del()) === 1) return { tree, vout: row.vout }
    }
    return undefined
  }
  async markProven(txid: string, merklePath?: MerklePath): Promise<void> {
    await this.ensure()
    await this.kx.transaction(async (trx) => {
      await trx('bstack_trees').where({ txid }).update({ proven: 1 })
      if (merklePath !== undefined) {
        const t = await trx('bstack_trees').where({ txid }).first()
        if (t) {
          const tree = Transaction.fromBEEF(JSON.parse(t.beef))
          tree.merklePath = merklePath
          // a BUMP-anchored tx terminates BEEF ancestry: unproven parents left
          // attached would be refused by hosts for missing source transactions
          for (const i of tree.inputs) i.sourceTransaction = undefined
          await trx('bstack_trees')
            .where({ txid })
            .update({ beef: JSON.stringify(tree.toBEEF()) })
        }
      }
    })
  }
  async attachParentPath(txid: string, merklePath: MerklePath): Promise<boolean> {
    await this.ensure()
    let changed = false
    for (const row of await this.kx('bstack_trees').where({ proven: 0 }).select('txid', 'beef')) {
      const tree = Transaction.fromBEEF(JSON.parse(row.beef as string))
      let touched = false
      for (const i of tree.inputs) {
        const src = i.sourceTransaction
        if (src !== undefined && src.id('hex') === txid && src.merklePath === undefined) {
          src.merklePath = merklePath
          touched = true
        }
      }
      if (touched) {
        // proven:0 in the UPDATE is the race guard — markProven can commit
        // (proven:1 + a stripped beef) between our SELECT and this write, and
        // resurrecting the parented beef over it leaves a proven row whose
        // beef fails host admission (extra txs in the carrier graph)
        const n = await this.kx('bstack_trees')
          .where({ txid: row.txid, proven: 0 })
          .update({ beef: JSON.stringify(tree.toBEEF()) })
        changed = changed || n === 1
      }
    }
    return changed
  }
  async unproven(): Promise<Array<{ txid: string; kind: string }>> {
    await this.ensure()
    return (await this.kx('bstack_trees').where({ proven: 0 }).select('txid', 'kind')).map((r) => ({ txid: r.txid, kind: r.kind }))
  }
  async drop(txid: string): Promise<void> {
    await this.ensure()
    await this.kx('bstack_trees').where({ txid }).del()
    await this.kx('bstack_tree_outputs').where({ txid }).del()
  }
  async remaining(kind = 'market'): Promise<number> {
    await this.ensure()
    const r = await this.kx('bstack_tree_outputs as o')
      .join('bstack_trees as t', 'o.txid', 't.txid')
      .where('t.proven', 1)
      .where('o.kind', kind)
      .count('o.key as c')
      .first()
    let n = Number(r?.c ?? 0)
    if (kind === 'market') {
      for (const row of await this.kx('bstack_tree_outputs as o')
        .join('bstack_trees as t', 'o.txid', 't.txid')
        .where('t.proven', 0)
        .where('o.kind', kind)
        .groupBy('t.txid', 't.beef')
        .select('t.beef')
        .count('o.key as c')) {
        if (anchoredTree(Transaction.fromBEEF(JSON.parse(row.beef as string)))) n += Number(row.c)
      }
    }
    return n
  }
}

export { feeFor, type Fees }
