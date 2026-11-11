// MockChain — an in-memory chain for offline testing.
// Accepts txs via broadcast(), mines them into blocks on demand, computes real
// BUMPs (MerklePath), tracks unconfirmed ancestor depth, and can reorg.
import { Transaction, MerklePath, PrivateKey, P2PKH, Script } from '@bsv/sdk'
import { dsha256, concat, toHex, type Bytes } from '@nektar/core'
import type { BlockHeader, Broadcaster, BroadcastResult, ChainTracker, Funder, FundingOut, TxStatusEvent } from './interfaces.js'

const rev = (b: Bytes) => Uint8Array.from(b).reverse()
const toInternal = (txid: string) => rev(new Uint8Array(Buffer.from(txid, 'hex')))
const toDisplay = (internal: Bytes) => toHex(rev(internal))

function nodeHash (l: Bytes, r: Bytes): Bytes {
  return dsha256(concat(l, r))
}

export class MockChain implements ChainTracker, Broadcaster {
  height = 0
  headers: BlockHeader[] = []
  mempool = new Map<string, Transaction>()
  private mempoolDepth = new Map<string, number>()
  minedTxs = new Map<string, { height: number }>()
  /** spent outpoints `txid:vout` — Teranode-faithful UTXO_SPENT behaviour */
  private spent = new Set<string>()
  /** outpoints spent by each txid — released on reorg */
  private spentBy = new Map<string, Set<string>>()
  statusCbs: Array<(ev: TxStatusEvent) => void> = []
  reorgCbs: Array<(removed: BlockHeader[], newTip: BlockHeader) => void> = []
  /** mempool ancestor-count limit for a new tx (Infinity = no limit) */
  ancestorLimit: number

  private tipHash = '00'.repeat(32)

  /** Teranode-faithful script eval: when true, every input's unlocking script
   * is run via the SDK Spend against its real parent output before accept. */
  verifyScripts = false

  constructor (ancestorLimit = Infinity) {
    this.ancestorLimit = ancestorLimit
  }

  /** node view: every tx we have accepted (mempool + mined) plus parents the
   * caller explicitly registers — script eval resolves parents from HERE, not
   * from caller-attached objects. */
  private known = new Map<string, Transaction>()
  /** register a parent tx (e.g. a MockFunder faucet utxo) into the node view */
  registerParent (tx: Transaction): void { this.known.set(tx.id('hex'), tx) }
  getTx (txid: string): Transaction | undefined { return this.known.get(txid) }

  async broadcast (tx: Transaction): Promise<BroadcastResult> {
    const txid = tx.id('hex')
    // idempotent resubmit, like BSV Broadcaster's 202 for a known tx — the
    // same bytes can only produce the same txid, so this is never a double-spend
    if (this.mempool.has(txid) || this.minedTxs.has(txid)) return { status: 'success', txid }
    if (this.verifyScripts) {
      for (let ix = 0; ix < tx.inputs.length; ix++) {
        const inp = tx.inputs[ix]!
        const ptxid = inp.sourceTXID ?? inp.sourceTransaction?.id('hex')
        if (!ptxid) continue // synthetic input
        // node-side resolution: use OUR store, not the caller-attached object
        let parent = this.known.get(ptxid)
        if (!parent && inp.sourceTransaction) {
          // explicitly synthetic parent (faucet etc.) — register it once
          parent = Transaction.fromHex(inp.sourceTransaction.toHex())
          this.registerParent(parent)
        }
        if (!parent || inp.sourceOutputIndex === undefined) {
          return { status: 'error', description: `TX_MISSING_PARENT: ${ptxid.slice(0, 12)}…` }
        }
        try {
          const { Spend } = await import('@bsv/sdk')
          const src = parent.outputs[inp.sourceOutputIndex]!
          const spend = new Spend({
            sourceTXID: ptxid,
            sourceSatoshis: src.satoshis ?? 0,
            sourceOutputIndex: inp.sourceOutputIndex,
            lockingScript: src.lockingScript,
            transactionVersion: tx.version,
            otherInputs: tx.inputs.filter((_, j) => j !== ix).map(i => ({
              sourceTXID: i.sourceTXID ?? i.sourceTransaction?.id('hex') ?? '',
              sourceOutputIndex: i.sourceOutputIndex ?? 0,
              sequence: i.sequence ?? 0xffffffff
            })),
            inputIndex: ix, unlockingScript: inp.unlockingScript,
            outputs: tx.outputs,
            inputSequence: inp.sequence ?? 0xffffffff,
            lockTime: tx.lockTime ?? 0
          } as any)
          if (!spend.validate()) {
            return { status: 'error', description: `SCRIPT_INVALID: input ${ix} evaluated false on ${ptxid.slice(0, 12)}` }
          }
        } catch (e) {
          return { status: 'error', description: `SCRIPT_INVALID: input ${ix} threw ${String((e as Error).message).slice(0, 90)}` }
        }
      }
    }
    // ancestor depth = longest chain of unconfirmed parents above the new tx
    // O(1) amortized: each mempool tx's unconfirmed ancestor depth is
    // memoized, so the depth check walks only this tx's inputs.
    let depth = 0
    for (const i of tx.inputs) {
      const ptxid = i.sourceTXID ?? i.sourceTransaction?.id('hex')
      if (!ptxid || !this.mempool.has(ptxid)) continue
      depth = Math.max(depth, (this.mempoolDepth.get(ptxid) ?? 0) + 1)
    }
    if (depth + 1 > this.ancestorLimit) {
      return { status: 'error', description: `unconfirmed ancestor chain depth ${depth + 1} exceeds limit ${this.ancestorLimit}` }
    }
    // Teranode-faithful double-spend check: an outpoint may be spent once,
    // across mempool AND mined history — no silent reuse.
    const ours = new Set<string>()
    for (const i of tx.inputs) {
      const ptxid = i.sourceTXID ?? i.sourceTransaction?.id('hex')
      if (!ptxid) continue // synthetic/coinbase-style input
      const op = `${ptxid}:${i.sourceOutputIndex}`
      if (this.spent.has(op)) {
        return { status: 'error', description: `UTXO_SPENT: ${op} already spent` }
      }
      ours.add(op)
    }
    for (const op of ours) this.spent.add(op)
    this.spentBy.set(txid, ours)
    this.mempool.set(txid, tx)
    this.known.set(txid, tx)
    this.mempoolDepth.set(txid, depth)
    this.emit({ txid, status: 'unconfirmed' })
    return { status: 'success', txid }
  }

  onStatus (cb: (ev: TxStatusEvent) => void): void { this.statusCbs.push(cb) }
  onReorg (cb: (removed: BlockHeader[], newTip: BlockHeader) => void): void { this.reorgCbs.push(cb) }

  private emit (ev: TxStatusEvent): void { for (const cb of this.statusCbs) cb(ev) }

  /** mine up to `count` blocks, including all mempool txs in the first block */
  private bumps = new Map<string, MerklePath>()
  /** the BUMP of a mined tx by txid (like a node's merkle-proof API) */
  merklePathOf (txid: string): MerklePath | undefined { return this.minedTxs.has(txid) ? this.bumps.get(txid) : undefined }

  mine (count = 1): BlockHeader[] {
    const out: BlockHeader[] = []
    for (let b = 0; b < count; b++) {
      const realTxids = [...this.mempool.keys()]
      // every real block has a coinbase at index 0 — keep a synthetic leaf so
      // no mined tx ever ends up with a lone-leaf level-0 BUMP
      const txids = realTxids.length ? [toHex(dsha256(new TextEncoder().encode(`coinbase:${this.height + 1}`))), ...realTxids] : realTxids
      this.height++
      const leaves = txids.map(toInternal)
      const rootInternal = merkleRootDisplay(leaves)
      const header: BlockHeader = {
        height: this.height,
        hash: toHex(dsha256(new TextEncoder().encode(`${this.height}:${this.tipHash}:${rootInternal}`))),
        prevHash: this.tipHash,
        merkleRoot: rootInternal,
        time: Math.floor(Date.now() / 1000),
        bits: 0x207fffff,
        nonce: this.height
      }
      // assign each tx a BUMP
      realTxids.forEach((txid, i) => {
        const tx = this.mempool.get(txid)!
        tx.merklePath = buildBump(txids, i + 1, this.height)
        this.minedTxs.set(txid, { height: this.height })
        this.bumps.set(txid, tx.merklePath)
        this.emit({ txid, status: 'mined', blockHeight: this.height, merklePath: tx.merklePath })
      })
      this.mempool.clear()
      this.mempoolDepth.clear()
      this.headers.push(header)
      this.tipHash = header.hash
      out.push(header)
    }
    return out
  }

  /** reorg the last `count` blocks: their txs go back to mempool (or are dropped) */
  reorg (count: number): void {
    const removed = this.headers.splice(this.headers.length - count, count)
    this.height -= count
    this.tipHash = this.headers.length ? this.headers[this.headers.length - 1]!.hash : '00'.repeat(32)
    for (const h of removed) {
      for (const [txid, rec] of this.minedTxs) {
        if (rec.height === h.height) {
          this.minedTxs.delete(txid)
          // release the outpoints this tx consumed — they become spendable again
          for (const op of this.spentBy.get(txid) ?? []) this.spent.delete(op)
          this.spentBy.delete(txid)
          this.emit({ txid, status: 'unconfirmed' })
        }
      }
    }
    if (removed.length) {
      const tip = this.headers[this.headers.length - 1] ?? { hash: this.tipHash } as BlockHeader
      for (const cb of this.reorgCbs) cb(removed, tip)
    }
  }

  // +100 so sdk merklePath.verify's coinbase-maturity rule (index-0 tx needs
  // blockHeight+100 <= currentHeight) doesn't reject mock-mined txs.
  async currentHeight (): Promise<number> { return this.height + 100 }
  async tipHeight (): Promise<number> { return this.height }
  async isValidRootForHeight (root: string, height: number): Promise<boolean> {
    return this.headers[height - 1]?.merkleRoot === root
  }
  async headerAtHeight (h: number): Promise<BlockHeader | undefined> { return this.headers[h - 1] }
}

function merkleRootDisplay (leavesInternal: Bytes[]): string {
  if (leavesInternal.length === 0) return toHex(dsha256(Uint8Array.of(0)))
  let cur = leavesInternal
  while (cur.length > 1) {
    const next: Bytes[] = []
    for (let i = 0; i < cur.length; i += 2) {
      const l = cur[i]!
      const r = i + 1 < cur.length ? cur[i + 1]! : l
      next.push(nodeHash(l, r))
    }
    cur = next
  }
  return toDisplay(cur[0]!)
}

/**
 * Build a MerklePath (BUMP) for tx at index `index` among `txids` at `height`.
 * All hashes are display-order hex (as the sdk expects: path[0] entries carry
 * hash=txid; sibling nodes carry their node hash in display order).
 */
export function buildBump (txids: string[], index: number, height: number): MerklePath {
  // internal-order node hashes per level
  let level: Bytes[] = txids.map(toInternal)
  const path: Array<Array<{ offset: number, hash?: string, txid?: boolean, duplicate?: boolean }>> = []
  const own = { offset: index, hash: txids[index]!, txid: true }
  let pos = index
  while (level.length > 1) {
    const row: Array<{ offset: number, hash?: string, txid?: boolean, duplicate?: boolean }> = []
    if (path.length === 0) row.push(own)
    const sib = pos % 2 === 0 ? pos + 1 : pos - 1
    if (sib < level.length) row.push({ offset: sib, hash: toDisplay(level[sib]!) })
    else row.push({ offset: pos + 1, duplicate: true })
    path.push(row)
    const next: Bytes[] = []
    for (let i = 0; i < level.length; i += 2) {
      const l = level[i]!
      const r = i + 1 < level.length ? level[i + 1]! : l
      next.push(nodeHash(l, r))
    }
    level = next
    pos = Math.floor(pos / 2)
  }
  if (path.length === 0) path.push([own])
  return new MerklePath(height, path)
}

/** MockFunder — a faucet of synthetic P2PKH utxos. */
export class MockFunder implements Funder {
  private key: PrivateKey
  private utxos: FundingOut[] = []
  private n = 0
  constructor (key = PrivateKey.fromRandom()) {
    this.key = key
  }
  address (): string { return this.key.toPublicKey().toAddress([0x6f]) }
  /** top up with `count` utxos of `satoshis` each (synthetic parents) */
  topup (count: number, satoshis = 10_000): void {
    for (let i = 0; i < count; i++) {
      const t = new Transaction()
      t.addInput({
        // unique per funder — two MockFunders used to collide on faucet:N
        sourceTXID: toHex(dsha256(new TextEncoder().encode(`faucet:${this.address()}:${this.n++}`))),
        sourceOutputIndex: 0,
        unlockingScript: Script.fromHex('51')
      })
      t.addOutput({ lockingScript: new P2PKH().lock(this.address()), satoshis })
      this.utxos.push({ tx: t, vout: 0, satoshis })
    }
  }
  async take (satoshis: number): Promise<FundingOut> {
    const u = this.utxos.find(u => u.satoshis >= satoshis)
    if (!u) throw new Error('MockFunder: no utxo (call topup)')
    this.utxos.splice(this.utxos.indexOf(u), 1)
    return u
  }
  /** mark an outpoint spent so take() never hands it out again */
  markSpent (txid: string, vout: number): void {
    const i = this.utxos.findIndex(u => u.tx.id('hex') === txid && u.vout === vout)
    if (i >= 0) this.utxos.splice(i, 1)
  }
  /** return a broadcast tx's output to the pool (change/escrow outs) */
  addOwned (tx: Transaction, vout: number): void {
    const o = tx.outputs[vout]
    if (o !== undefined) this.utxos.push({ tx, vout, satoshis: o.satoshis ?? 0 })
  }
  get keyPair (): PrivateKey { return this.key }
  /** the synthetic funding parents (register them with MockChain.registerParent) */
  get parents (): Transaction[] { return this.utxos.map(u => u.tx) }
}
