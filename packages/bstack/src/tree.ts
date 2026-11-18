import { PrivateKey, ProtoWallet, Transaction, P2PKH, MerklePath } from '@bsv/sdk'
import { fundingLock, feeFor, type Derivation, type Fees, type SigningWallet } from '@lightwebinc/bcommon'
import { unlockP2PKH, signerAddress, nektarEnv, type SignerLike } from '@nektar/core'
import type { FundingOut } from '@nektar/chain'
import type { Knex } from 'knex'

export const DERIVATION: Derivation = { protocol: [2, 'nektar market'], keyID: '1' }
/** tree-row kind for adopted provision transactions — a tx that funds the
 *  instance AND carries lane-locked outputs; its slots serve every lane
 *  once proven */
export const PROVISION_KIND = 'provision'
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

// the market lane's default funding lock — mintTree computes it inline
// when no explicit lock is passed; provisioning needs the concrete script
export const marketFundingLock = async (wallet: SigningWallet): Promise<import('@bsv/sdk').LockingScript> =>
  fundingLock(wallet, ORIGINATOR, DERIVATION, FUNDING_TAG)

// A broadcaster BUMP can prove several transactions of one block at once, and
// marks a sibling leaf as a txid when it is another of the client's own
// transactions; hosts admit a carrier only when the proof is the funding
// tree's path alone, with its sibling an ordinary hash. An odd-count block
// duplicates the last leaf at the next offset — extract() re-flags that copy
// because it finds the txid by hash, so the flag must be restored to the
// offset the source path flagged.
export const ownPath = (tree: Transaction): Transaction => {
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
  /** proven means evidenced: the caller must hold the BUMP it claims — a
   *  pathless mark would label stock proven while its beef carries no
   *  anchor, which host admission refuses */
  markProven(txid: string, merklePath: MerklePath): Promise<void>
  /** a funding parent's own proof: write it into every stored tree whose
   *  input ancestry still shows that parent unproven — after this the tree
   *  is ancestry-anchored and its slots serve. true when a beef changed. */
  attachParentPath(txid: string, merklePath: MerklePath): Promise<boolean>
  /** a reorg orphaned every proof anchored at >= `height`: strip those paths
   *  — the tree's own BUMP AND any parent BUMP inside its carried ancestry —
   *  and flip affected rows back to unproven so the proving pass re-anchors
   *  them against the surviving fork (mirrors CarrierJournal.clearPaths).
   *  Without this a proven row is permanently stale: markProven only touches
   *  unproven() rows, so a re-mined proof on the new fork can never heal it.
   *  Returns the txids that were demoted. */
  unproveFrom(height: number): Promise<string[]>
  unproven(): Promise<Array<{ txid: string; kind: string }>>
  /** remove a tree + all its slots — a mint that never landed on-chain
   *  (rejected / evicted) would otherwise sit in unproven() forever and gate
   *  out every future mint of that kind */
  drop(txid: string): Promise<void>
  remaining(kind?: string): Promise<number>
  /** why a lane came up empty: free leaves on proven trees, free leaves on
   *  anchored (zero-conf-admissible) unproven trees, and unproven trees of
   *  the kind still awaiting a block. `taken` is the consumed count —
   *  minted outputs of the kind minus free outputs remaining (provision
   *  trees record under 'provision' kind, so their slots are neither
   *  minted-of-kind nor free-of-kind here). */
  stock(kind?: string): Promise<{ proven: number; anchored: number; unprovenTrees: number; taken: number }>
  /** register a foreign provision tx: the tree row once (kind 'provision'),
   *  and one free slot per entry carrying its own kind at its real vout.
   *  Idempotent — re-adopting the same tx adds nothing and returns 0. */
  adopt(tx: Transaction, slots: Array<{ vout: number; kind: string }>): Promise<number>
}

// a tree hosts may fund-accept before it mines: proven, or one unconfirmed
// level deep on anchored ancestry — every input's source carries a merkle
// path (the same §10.3 rule finger applies to tokens). The tree still lists
// in unproven() so its own BUMP attaches when the topup mines.
export const anchoredTree = (tree: Transaction): boolean =>
  tree.merklePath !== undefined || (tree.inputs.length > 0 && tree.inputs.every((i) => i.sourceTransaction?.merklePath !== undefined))

/** the ancestry gaps that kill a tree's carriers: every input on the
 *  unproven chain must resolve — carried bytes or a fetched copy. An input
 *  whose sourceTXID the resolver cannot serve is dead ancestry: the tree
 *  can neither prove (its own broadcast needs the parent's UTXO store) nor
 *  anchor, and carriers minted on it are refused by host SPV verification
 *  forever (`missing an associated source transaction`). Returns the dead
 *  txids; the caller owns strike/eviction policy. Walks unproven ancestry
 *  only — a BUMP-anchored ancestor terminates the chain — and is bounded
 *  so a deep or cyclic graph can't spin the audit. */
export const deadAncestry = async (tree: Transaction, resolveTx: (txid: string) => Promise<Transaction | undefined>, maxNodes = 32): Promise<string[]> => {
  const dead = new Set<string>()
  const seen = new Set<string>()
  try {
    seen.add(tree.id('hex'))
  } catch {
    // a subject that cannot serialize is itself a stand-in — walk its
    // inputs by sourceTXID alone; they name what it cannot
  }
  const queue: Transaction[] = tree.merklePath === undefined ? [tree] : []
  while (queue.length > 0 && seen.size <= maxNodes) {
    const t = queue.shift()!
    for (const i of t.inputs) {
      // the wire reference names the parent; sourceTransaction may be an
      // EF/lazy stand-in whose id() throws — try it only as the fallback
      let id = i.sourceTXID
      if (id === undefined && i.sourceTransaction !== undefined) {
        try {
          id = i.sourceTransaction.id('hex')
        } catch {
          id = undefined
        }
      }
      // a null prevout (coinbase-shaped input) is a structural terminal,
      // not an ancestry gap — there is no parent to resolve
      if (id === undefined || id === '00'.repeat(32) || seen.has(id)) continue
      seen.add(id)
      let p = i.sourceTransaction
      if (p !== undefined) {
        try {
          p.id('hex')
        } catch {
          // a stand-in carries no ancestry — resolve the real parent;
          // unresolvable IS dead ancestry (it can neither prove nor anchor)
          p = await resolveTx(id)
        }
      } else {
        p = await resolveTx(id)
      }
      if (p === undefined) {
        dead.add(id)
        continue
      }
      if (p.merklePath === undefined) queue.push(p)
    }
  }
  return [...dead]
}

// only the market lane tolerates unproven funding by default: the nektar
// module accepts anchored ancestry, and vendored hosts do so only where
// their admission allows it. blogs/finger carriers carry ancestry natively;
// bbox admits it once the host runs the anchored-admission patch (one BUMP
// may anchor a deeper ancestor). NEKTAR_ANCHORED_LANES names the kinds an
// anchored tree may serve — 'all' or a comma list — for hosts known to
// admit it; elsewhere a refused carrier would only cost a broadcast.
const anchoredKinds = (env: NodeJS.ProcessEnv = process.env): Set<string> => {
  const v = nektarEnv('NEKTAR_ANCHORED_LANES', env) ?? ''
  const kinds = new Set(v === 'all' || v === '1' ? ['bbox', 'log', 'finger'] : v.split(',').map((s) => s.trim()).filter(Boolean))
  kinds.add('market')
  return kinds
}
const serves = (tree: { tree: Transaction; proven: boolean }, kind: string): boolean =>
  tree.proven || (anchoredKinds().has(kind) && anchoredTree(tree.tree))

/** outputs of `tx` that carry a lane's funding lock at ≥ its slot value —
 *  the slots a provision tx donates to each lane, at their real vouts */
export const matchProvision = (tx: Transaction, lanes: Array<{ kind: string; lockHex: string; minSats: number }>): Array<{ vout: number; kind: string }> => {
  const out: Array<{ vout: number; kind: string }> = []
  tx.outputs.forEach((o, vout) => {
    const hex = o.lockingScript?.toHex()
    const lane = lanes.find((l) => l.lockHex === hex && (o.satoshis ?? 0) >= l.minSats)
    if (lane) out.push({ vout, kind: lane.kind })
  })
  return out
}

export class MemoryTreeLedger implements TreeLedger {
  private trees = new Map<string, { tree: Transaction; proven: boolean; kind: string; n: number }>()
  // free slot key -> lane kind; adopt() writes per-slot kinds, add() the
  // tree's single kind — provisioning makes trees mixed-kind
  private free = new Map<string, string>()
  async add(tree: Transaction, n: number, kind = 'market'): Promise<void> {
    const txid = tree.id('hex')
    this.trees.set(txid, { tree, proven: false, kind, n })
    for (let i = 0; i < n; i++) this.free.set(`${txid}:${i}`, kind)
  }
  async adopt(tx: Transaction, slots: Array<{ vout: number; kind: string }>): Promise<number> {
    const txid = tx.id('hex')
    if (this.trees.has(txid)) return 0
    // provision trees minted nothing on this ledger — n:0 keeps stock()'s
    // `taken` (own-kind minted minus own-kind free) out of foreign stock
    this.trees.set(txid, { tree: tx, proven: tx.merklePath !== undefined, kind: PROVISION_KIND, n: 0 })
    for (const s of slots) this.free.set(`${txid}:${s.vout}`, s.kind)
    return slots.length
  }
  async take(kind = 'market', expectedLockHex?: string): Promise<{ tree: Transaction; vout: number } | undefined> {
    for (const [k, slotKind] of this.free) {
      const [txid, v] = k.split(':')
      const t = this.trees.get(txid)!
      if (slotKind !== kind || !serves(t, kind)) continue
      this.free.delete(k)
      if (expectedLockHex !== undefined && t.tree.outputs[Number(v)]?.lockingScript.toHex() !== expectedLockHex) continue
      return { tree: ownPath(t.tree), vout: Number(v) }
    }
    return undefined
  }
  async markProven(txid: string, merklePath: MerklePath): Promise<void> {
    const t = this.trees.get(txid)
    if (t) {
      t.proven = true
      t.tree.merklePath = merklePath
      // a BUMP-anchored tx terminates BEEF ancestry: unproven parents left
      // attached would be refused by hosts for missing source transactions
      for (const i of t.tree.inputs) i.sourceTransaction = undefined
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
  async unproveFrom(height: number): Promise<string[]> {
    const out: string[] = []
    for (const [txid, t] of this.trees) {
      if (t.proven && t.tree.merklePath !== undefined && t.tree.merklePath.blockHeight >= height) {
        delete t.tree.merklePath
        t.proven = false
        out.push(txid)
      }
      for (const i of t.tree.inputs) {
        const src = i.sourceTransaction
        if (src?.merklePath !== undefined && src.merklePath.blockHeight >= height) delete src.merklePath
      }
    }
    return out
  }
  async unproven(): Promise<Array<{ txid: string; kind: string }>> {
    return [...this.trees].filter(([, t]) => !t.proven).map(([id, t]) => ({ txid: id, kind: t.kind }))
  }
  async drop(txid: string): Promise<void> {
    this.trees.delete(txid)
    for (const k of [...this.free.keys()]) if (k.startsWith(`${txid}:`)) this.free.delete(k)
  }
  async remaining(kind = 'market'): Promise<number> {
    let n = 0
    for (const [k, slotKind] of this.free) {
      const t = this.trees.get(k.split(':')[0])!
      if (slotKind === kind && serves(t, kind)) n++
    }
    return n
  }
  async stock(kind = 'market'): Promise<{ proven: number; anchored: number; unprovenTrees: number; taken: number }> {
    let proven = 0,
      anchored = 0,
      minted = 0,
      freeMine = 0
    const waiting = new Set<string>()
    for (const t of this.trees.values()) if (t.kind === kind) minted += t.n
    for (const [k, slotKind] of this.free) {
      const txid = k.split(':')[0]
      const t = this.trees.get(txid)!
      if (slotKind !== kind) continue
      if (t.kind === kind) freeMine++
      if (t.proven) proven++
      else {
        waiting.add(txid)
        if (anchoredTree(t.tree)) anchored++
      }
    }
    // `taken` is own-kind consumed: slots this ledger minted minus the ones
    // still free on its own trees — adopted provision slots are foreign
    // stock and must neither inflate (change outputs) nor underflow it
    return { proven, anchored, unprovenTrees: waiting.size, taken: minted - freeMine }
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
  async adopt(tx: Transaction, slots: Array<{ vout: number; kind: string }>): Promise<number> {
    await this.ensure()
    const txid = tx.id('hex')
    if (await this.kx('bstack_trees').where({ txid }).first()) return 0
    await this.kx.transaction(async (trx) => {
      await trx('bstack_trees')
        .insert({ txid, n: slots.length, proven: tx.merklePath ? 1 : 0, beef: JSON.stringify(tx.toBEEF()), kind: PROVISION_KIND })
        .onConflict('txid')
        .ignore()
      const rows = slots.map((s) => ({ key: `${txid}:${s.vout}`, txid, vout: s.vout, kind: s.kind }))
      if (rows.length) await trx('bstack_tree_outputs').insert(rows).onConflict('key').ignore()
    })
    return slots.length
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
  async markProven(txid: string, merklePath: MerklePath): Promise<void> {
    await this.ensure()
    await this.kx.transaction(async (trx) => {
      await trx('bstack_trees').where({ txid }).update({ proven: 1 })
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
  async unproveFrom(height: number): Promise<string[]> {
    await this.ensure()
    const out: string[] = []
    for (const row of await this.kx('bstack_trees').select('txid', 'proven', 'beef')) {
      const tree = Transaction.fromBEEF(JSON.parse(row.beef as string))
      let touched = false
      // a dead-fork BUMP on the tree itself demotes the row: slots stop
      // serving (proven:0, path stripped) and the proving pass re-anchors it
      const deadRoot = tree.merklePath !== undefined && tree.merklePath.blockHeight >= height
      if (deadRoot) {
        delete tree.merklePath
        touched = true
        if (Number(row.proven) === 1) out.push(row.txid)
      }
      for (const i of tree.inputs) {
        const src = i.sourceTransaction
        if (src?.merklePath !== undefined && src.merklePath.blockHeight >= height) {
          delete src.merklePath
          touched = true
        }
      }
      if (!touched) continue
      // proven:<read> is the race guard (same shape attachParentPath uses):
      // a markProven that committed between our SELECT and this write already
      // re-anchored the row on the new fork — leave it alone
      await this.kx('bstack_trees')
        .where({ txid: row.txid, proven: Number(row.proven) })
        .update({ ...(deadRoot ? { proven: 0 } : {}), beef: JSON.stringify(tree.toBEEF()) })
    }
    return out
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
  async stock(kind = 'market'): Promise<{ proven: number; anchored: number; unprovenTrees: number; taken: number }> {
    await this.ensure()
    const r = await this.kx('bstack_tree_outputs as o')
      .join('bstack_trees as t', 'o.txid', 't.txid')
      .where('t.proven', 1)
      .where('o.kind', kind)
      .count('o.key as c')
      .first()
    let anchored = 0
    for (const row of await this.kx('bstack_tree_outputs as o')
      .join('bstack_trees as t', 'o.txid', 't.txid')
      .where('t.proven', 0)
      .where('o.kind', kind)
      .groupBy('t.txid', 't.beef')
      .select('t.beef')
      .count('o.key as c')) {
      if (anchoredTree(Transaction.fromBEEF(JSON.parse(row.beef as string)))) anchored += Number(row.c)
    }
    // a provision tree counts toward every kind it still holds slots for
    const u = await this.kx('bstack_trees as t')
      .join('bstack_tree_outputs as o', 'o.txid', 't.txid')
      .where('t.proven', 0)
      .where('o.kind', kind)
      .countDistinct('t.txid as c')
      .first()
    const [m, f] = await Promise.all([
      this.kx('bstack_trees').sum('n as s').where('kind', kind).first(),
      // free slots on the ledger's OWN trees of this kind — adopted provision
      // rows carry their leaf kind but the tree kind is 'provision', so the
      // join keeps foreign stock out of the consumed count
      this.kx('bstack_tree_outputs as o').join('bstack_trees as t', 'o.txid', 't.txid').where('o.kind', kind).where('t.kind', kind).count('o.key as c').first()
    ])
    const taken = Number((m as { s: number | null } | undefined)?.s ?? 0) - Number((f as { c: number } | undefined)?.c ?? 0)
    return { proven: Number(r?.c ?? 0), anchored, unprovenTrees: Number(u?.c ?? 0), taken }
  }
}

/** the stock-out line every funding-slot consumer reports — keeps the
 *  'funding slot' substring classifyDeliveryError keys on, and says why:
 *  proven stock, anchored zero-conf stock and trees still awaiting a block.
 *  A foreign ledger without stock() gets the plain message. */
export const fundingSlotReason = async (ledger: { stock?: TreeLedger['stock'] }, kind: string): Promise<string> => {
  const s = await ledger.stock?.(kind)
  return s === undefined
    ? 'no funding slot'
    : `no funding slot (${kind}: ${s.proven} proven, ${s.anchored} anchored, ${s.unprovenTrees} tree(s) awaiting a block)`
}

export { feeFor, type Fees }
