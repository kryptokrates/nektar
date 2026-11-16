// memchain.ts — the in-memory tier of the test world (NEKTAR_TEST_TIER
// unset). The contract is StackChain's, backed by in-process state: real
// 80-byte headers that pass headerPowValid, real merkle trees and BUMPs via
// buildBump/merkleRootOf, and admission that mirrors the node — scripts are
// always evaluated through the SDK interpreter against THIS chain's own
// record of the parent output (never caller-attached sourceTransaction
// objects). Proofs reach consumers through the same ChainEvents substrate
// StackChain uses: mine() writes them, the observe path delivers them, so
// fault injection (faults.observer) can reproduce "proven on-chain but not
// delivered" exactly.
import { Transaction, MerklePath, PrivateKey, P2PKH, Hash, UnlockingScript, Spend } from '@bsv/sdk'
import type { BlockHeader, BroadcastResult, FundingOut, TxStatusEvent } from './interfaces.js'
import { ChainEvents } from './events.js'
import { buildBump } from './bump.js'
import { serializeHeader, headerPowValid } from './headers.js'
import { merkleRootOf } from './regtest.js'
import type { TestChain, TestFunder } from './testtier.js'

const ZERO = '0'.repeat(64)
const GENESIS_BITS = 0x207fffff
const SUBSIDY = 5_000_000_000
const COINBASE_MATURITY = 100
/** same TTL the stack tier gives watched broadcasts (see testkit WATCH_TTL_MS) */
const WATCH_TTL_MS = 60_000

const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms))

/** BIP34 coinbase scriptSig: the block height, minimally LE-encoded */
function pushHeight(h: number): string {
  if (h === 0) return '00'
  const b: number[] = []
  for (let v = h; v > 0; v >>= 8) b.push(v & 0xff)
  if ((b[b.length - 1]! & 0x80) !== 0) b.push(0)
  return b.length.toString(16).padStart(2, '0') + Buffer.from(b).toString('hex')
}

interface TxRec {
  /** the caller's own object for broadcast txs (getTx returns it — the same
   *  contract StackChain keeps); the chain's object for coinbases */
  stored: Transaction
  /** frozen chain-side copy — admission and script eval resolve parents
   *  from THIS, never from fields a caller may have mutated */
  snapshot: Transaction
  coinbase: boolean
  /** set once the tx is in a block; cleared on reorg */
  height?: number
  /** the chain's record of the proof — set at mine time; the caller object
   *  only receives it through the observe path (observer faults hold it) */
  mp?: MerklePath
}

interface MemBlock {
  header: BlockHeader
  /** every txid in the block, coinbase at index 0 */
  txids: string[]
}

export interface MemoryChainFaults {
  /** return a description to refuse this broadcast as a definitive rejection */
  reject?: (tx: Transaction) => string | undefined
  /** true = accepted into the mempool but never mined (nor its descendants) */
  hold?: (txid: string) => boolean
  /** the events observer throws ("couldn't ask") as a timeout or an HTTP 429 */
  observer?: 'timeout' | 'rate-limit'
}

export class MemoryChain implements TestChain {
  readonly treasuryKey: PrivateKey
  private known = new Map<string, TxRec>()
  /** admission-ordered mempool txids (Map preserves insertion order) */
  private mempool = new Map<string, true>()
  /** outpoints consumed by accepted txs, mempool or mined */
  private spent = new Set<string>()
  private spentBy = new Map<string, string[]>()
  /** coinbase txid → the height it was mined at (for the maturity rule) */
  private coinbases = new Map<string, number>()
  private rejectedSet = new Set<string>()
  private blocks: MemBlock[] = []
  private reorgCbs: Array<(removed: BlockHeader[], newTip: BlockHeader) => void> = []
  private events: ChainEvents

  faults: MemoryChainFaults = {}

  constructor(opts: { treasuryKey?: PrivateKey; premine?: number } = {}) {
    this.treasuryKey = opts.treasuryKey ?? PrivateKey.fromRandom()
    this.events = new ChainEvents({
      intervalMs: 50,
      ttlMs: WATCH_TTL_MS,
      height: async () => this.tip(),
      observe: async (txid) => {
        if (this.faults.observer !== undefined)
          throw new Error(`memchain: observer ${this.faults.observer === 'timeout' ? 'timed out' : 'returned 429'} for ${txid}`)
        const rec = this.known.get(txid)
        if (rec?.mp === undefined) return undefined
        // proofs attach on delivery, like StackChain's observe — not at mine
        rec.stored.merklePath ??= MerklePath.fromBinary(rec.mp.toBinary())
        return { txid, status: 'mined', blockHeight: rec.height!, merklePath: rec.mp }
      }
    })
    // genesis + premine so treasury coinbases are mature before any test runs
    this.mineSync((opts.premine ?? 110) + 1)
  }

  // --- chain state -------------------------------------------------------

  private tip(): number {
    return this.blocks.length - 1
  }

  /** the block list — treasury reads mature coinbases from it */
  get blockList(): ReadonlyArray<MemBlock> {
    return this.blocks
  }

  /** coinbases mined so far — a re-mined height must not repeat a txid */
  private coinbaseCount = 0

  private coinbaseTx(height: number): Transaction {
    const t = new Transaction()
    t.addInput({
      sourceTXID: ZERO,
      sourceOutputIndex: 0xffffffff,
      // BIP34 height push + an extranonce counter: a block re-mined after a
      // reorg gets a different coinbase txid, hence a different header
      unlockingScript: UnlockingScript.fromHex(pushHeight(height) + pushHeight(this.coinbaseCount++))
    })
    t.addOutput({ lockingScript: new P2PKH().lock(this.treasuryKey.toPublicKey().toAddress([0x6f])), satoshis: SUBSIDY })
    return t
  }

  /** nonce-grind at regtest difficulty — a handful of hashes per block */
  private buildHeader(merkleRoot: string): BlockHeader {
    const prev = this.blocks[this.blocks.length - 1]
    const h: BlockHeader = {
      height: this.blocks.length,
      hash: '',
      prevHash: prev?.header.hash ?? ZERO,
      merkleRoot,
      time: Math.max(Math.floor(Date.now() / 1000), prev?.header.time ?? 0),
      bits: GENESIS_BITS,
      nonce: 0,
      version: 0x20000000
    }
    for (let nonce = 0; ; nonce++) {
      h.nonce = nonce
      const raw = serializeHeader(h)
      if (raw === undefined) throw new Error('memchain: header could not serialize')
      h.hash = Buffer.from(Hash.hash256([...raw]))
        .reverse()
        .toString('hex')
      if (headerPowValid(h)) return h
    }
  }

  // --- admission ---------------------------------------------------------

  async broadcast(tx: Transaction): Promise<BroadcastResult> {
    let txid: string
    let copy: Transaction
    try {
      txid = tx.id('hex')
      copy = Transaction.fromBinary(tx.toBinary())
    } catch (e) {
      return { status: 'error', description: `TX_INVALID (31): unserializable: ${String((e as Error).message).slice(0, 90)}` }
    }
    // idempotent resubmit, like the node's 202 for a known tx
    const known = this.known.get(txid)
    if (this.mempool.has(txid) || known?.height !== undefined) return { status: 'success', txid }

    const fail = (description: string, definitive: boolean): BroadcastResult => {
      if (definitive) {
        this.rejectedSet.add(txid)
        // a broadcast-time refusal is a final state — push it like 'mined' so
        // reconcile gates clear immediately (same as StackChain.broadcast)
        this.events.report({ txid, status: 'rejected' })
      }
      return { status: 'error', txid, description }
    }

    const custom = this.faults.reject?.(copy)
    if (custom !== undefined) return fail(custom, true)

    // parents resolve from the chain's own store — missing parents are
    // transient (lagging ingestion), spent outpoints are definitive
    let inSats = 0
    const inputs: Array<{ rec: TxRec; vout: number }> = []
    for (const inp of copy.inputs) {
      const ptxid = inp.sourceTXID
      const vout = inp.sourceOutputIndex
      if (ptxid === undefined || vout === undefined) return fail(`TX_MISSING_PARENT: input without a source outpoint`, false)
      if (ptxid === ZERO || vout === 0xffffffff) return fail(`TX_INVALID (18): coinbase-style input in a mempool tx`, true)
      const rec = this.known.get(ptxid)
      const out = rec?.snapshot.outputs[vout]
      if (rec === undefined || out === undefined) return fail(`TX_MISSING_PARENT: ${ptxid}`, false)
      const op = `${ptxid}:${vout}`
      if (this.spent.has(op)) return fail(`UTXO_SPENT: ${op} already spent`, true)
      const cbh = this.coinbases.get(ptxid)
      // mature when the tip is at least 99 blocks above the coinbase's block
      // (confirmations = tip - height + 1 ≥ 100)
      if (cbh !== undefined && this.tip() - cbh < COINBASE_MATURITY - 1) return fail(`premature spend of coinbase ${ptxid}`, true)
      inSats += out.satoshis ?? 0
      inputs.push({ rec, vout })
    }
    const outSats = copy.outputs.reduce((s, o) => s + (o.satoshis ?? 0), 0)
    if (inSats < outSats) return fail(`TX_INVALID (31): [ProcessTransaction][${txid}] outputs exceed inputs`, true)

    // nLockTime: final when every sequence is final, or the lock is at/below
    // the threshold — height for <500M, the tip header's time above (the
    // node's MTP is approximated by the tip time; regtest tests never sit on
    // the boundary)
    const lt = copy.lockTime ?? 0
    const anyNonFinal = copy.inputs.some((i) => (i.sequence ?? 0xffffffff) !== 0xffffffff)
    if (lt !== 0 && anyNonFinal) {
      const threshold = lt < 500_000_000 ? this.tip() : (this.blocks[this.blocks.length - 1]?.header.time ?? 0)
      if (lt > threshold) return fail(`non-final: lockTime ${lt} not reached`, false)
    }

    // script admission — the SDK interpreter on the chain's record of each
    // parent output, one input at a time, strict mode
    for (let ix = 0; ix < copy.inputs.length; ix++) {
      const inp = copy.inputs[ix]!
      const src = inputs[ix]!.rec.snapshot.outputs[inputs[ix]!.vout]!
      try {
        const spend = new Spend({
          sourceTXID: inp.sourceTXID!,
          sourceOutputIndex: inp.sourceOutputIndex!,
          sourceSatoshis: src.satoshis ?? 0,
          lockingScript: src.lockingScript,
          transactionVersion: copy.version,
          otherInputs: copy.inputs.filter((_, j) => j !== ix),
          outputs: copy.outputs,
          inputIndex: ix,
          unlockingScript: inp.unlockingScript ?? UnlockingScript.fromHex(''),
          inputSequence: inp.sequence ?? 0xffffffff,
          lockTime: lt
        })
        if (!spend.validate()) throw new Error('script evaluated false')
      } catch (e) {
        return fail(`400: Failed to process transaction: TX_INVALID (31): [ProcessTransaction][${txid}] ${String((e as Error).message).slice(0, 200)}`, true)
      }
    }

    for (const inp of copy.inputs) this.spent.add(`${inp.sourceTXID}:${inp.sourceOutputIndex}`)
    this.spentBy.set(
      txid,
      copy.inputs.map((i) => `${i.sourceTXID}:${i.sourceOutputIndex}`)
    )
    this.mempool.set(txid, true)
    this.known.set(txid, { stored: tx, snapshot: copy, coinbase: false })
    this.events.watch(txid)
    return { status: 'success', txid }
  }

  /** evict an accepted mempool tx and every mempool tx that descends from it:
   *  the freed inputs are spendable again and the txids are reported rejected —
   *  the accepted-then-rejected case. A block must never hold an orphan, so a
   *  child of the evicted tx goes with it (transitively). */
  reject(txid: string): boolean {
    if (!this.mempool.has(txid)) return false
    const doomed = new Set<string>([txid])
    let grew = true
    while (grew) {
      grew = false
      for (const id of this.mempool.keys()) {
        if (doomed.has(id)) continue
        const rec = this.known.get(id)
        if (rec?.snapshot.inputs.some((i) => i.sourceTXID !== undefined && doomed.has(i.sourceTXID))) {
          doomed.add(id)
          grew = true
        }
      }
    }
    for (const id of doomed) {
      this.mempool.delete(id)
      this.known.delete(id)
      this.rejectedSet.add(id)
      for (const op of this.spentBy.get(id) ?? []) this.spent.delete(op)
      this.spentBy.delete(id)
      this.events.report({ txid: id, status: 'rejected' })
    }
    return true
  }

  // --- mining ------------------------------------------------------------

  /** the synchronous core — the constructor pre-mines without async */
  private mineSync(n: number): string[] {
    const minedHere: string[] = []
    for (let b = 0; b < n; b++) {
      const height = this.blocks.length
      const cb = this.coinbaseTx(height)
      const cbid = cb.id('hex')
      // included = every mempool tx that is not held and does not descend
      // from an excluded tx (admission order keeps parents before children)
      const excluded = new Set<string>()
      const included: string[] = []
      for (const txid of this.mempool.keys()) {
        if (this.faults.hold?.(txid) === true) {
          excluded.add(txid)
          continue
        }
        const blocked = this.known.get(txid)!.snapshot.inputs.some((i) => i.sourceTXID !== undefined && excluded.has(i.sourceTXID))
        if (blocked) {
          excluded.add(txid)
          continue
        }
        included.push(txid)
      }
      const txids = [cbid, ...included]
      const header = this.buildHeader(merkleRootOf(txids))
      this.blocks.push({ header, txids })
      const cbMp = buildBump(txids, 0, height)
      cb.merklePath = cbMp
      this.known.set(cbid, { stored: cb, snapshot: cb, coinbase: true, height, mp: cbMp })
      this.coinbases.set(cbid, height)
      included.forEach((txid, i) => {
        const rec = this.known.get(txid)!
        rec.height = height
        rec.mp = buildBump(txids, i + 1, height)
        this.mempool.delete(txid)
        minedHere.push(txid)
      })
    }
    return minedHere
  }

  /** mine `n` blocks; the first takes every admissible mempool tx */
  async mine(n = 1): Promise<void> {
    const minedHere = this.mineSync(n)
    // proofs travel through the observe path, exactly like the stack tier —
    // tick until every watched txid mined in this call has been delivered
    for (let i = 0; i < 10; i++) {
      if (!minedHere.some((id) => this.events.watching(id))) break
      await this.events.tick()
      if (minedHere.some((id) => this.events.watching(id))) await sleep(5)
    }
  }

  /** drop the last `n` blocks: their non-coinbase txs return to the mempool
   *  (their outpoints stay spent — the spends are still pending), coinbases
   *  are forgotten, onReorg fires with the removed headers (deepest first).
   *  Refuses to reorg past genesis or a block whose coinbase was spent. */
  reorg(n = 1): void {
    if (n < 1 || n >= this.blocks.length) throw new Error(`memchain: reorg(${n}) out of range (height ${this.tip()})`)
    const removed = this.blocks.slice(this.blocks.length - n)
    for (const b of removed) if (this.spent.has(`${b.txids[0]}:0`)) throw new Error(`memchain: cannot reorg block ${b.header.height} — its coinbase is spent`)
    this.blocks.length -= n
    // requeue in original order — oldest removed block first keeps
    // parent-before-child admission order in the mempool
    for (const b of removed) {
      this.coinbases.delete(b.txids[0]!)
      for (const txid of b.txids.slice(1)) {
        const rec = this.known.get(txid)!
        rec.height = undefined
        rec.mp = undefined
        delete rec.stored.merklePath
        this.mempool.set(txid, true)
      }
    }
    const tip = this.blocks[this.blocks.length - 1]!.header
    const headers = [...removed].reverse().map((b) => b.header)
    for (const cb of this.reorgCbs) cb(headers, tip)
    for (const b of removed) {
      for (const txid of b.txids.slice(1)) {
        this.events.watch(txid)
        this.events.report({ txid, status: 'unconfirmed' })
      }
    }
  }

  onReorg(cb: (removed: BlockHeader[], newTip: BlockHeader) => void): void {
    this.reorgCbs.push(cb)
  }

  // --- reads -------------------------------------------------------------

  /** sync view: caller objects for broadcast txs (merklePath attaches on
   *  proof delivery), chain objects for coinbases — the StackChain contract */
  getTx(txid: string): Transaction | undefined {
    return this.known.get(txid)?.stored
  }

  /** chain-side copy with the proof attached when mined — the
   *  getrawtransaction+merkle_proof lookup, for txids not broadcast by us */
  async fetchTx(txid: string): Promise<Transaction | undefined> {
    const rec = this.known.get(txid)
    if (rec === undefined) return undefined
    const t = Transaction.fromBinary(rec.snapshot.toBinary())
    if (rec.mp !== undefined) t.merklePath = rec.mp
    return t
  }

  isRejected(txid: string): boolean {
    return this.rejectedSet.has(txid)
  }

  onStatus(cb: (ev: TxStatusEvent) => void): void {
    this.events.on('tx', cb)
    this.events.start()
  }

  get chainEvents(): ChainEvents {
    return this.events
  }

  stopWatching(): void {
    this.events.stop()
  }

  async waitProof(txid: string, timeoutMs = 30_000): Promise<Transaction | undefined> {
    await this.events.waitFor(txid, timeoutMs)
    return this.known.get(txid)?.stored
  }

  async currentHeight(): Promise<number> {
    return this.tip()
  }

  async tipHeight(): Promise<number> {
    return this.tip()
  }

  async isValidRootForHeight(root: string, height: number): Promise<boolean> {
    return this.blocks[height]?.header.merkleRoot === root
  }

  async headerAtHeight(height: number): Promise<BlockHeader | undefined> {
    return this.blocks[height]?.header
  }

  async merklePathOf(txid: string): Promise<MerklePath | undefined> {
    return this.known.get(txid)?.mp
  }

  async utxoStatus(txid: string, vout: number): Promise<'unspent' | 'spent' | 'unknown'> {
    const rec = this.known.get(txid)
    if (rec === undefined || vout < 0 || vout >= rec.snapshot.outputs.length) return 'unknown'
    return this.spent.has(`${txid}:${vout}`) ? 'spent' : 'unspent'
  }

  async pendingBroadcasts(): Promise<number> {
    return this.mempool.size
  }
}

// ---------------------------------------------------------------------------
// treasury + funder — the memory-tier counterparts of StackTreasury/StackFunder

export class MemoryTreasury {
  readonly key: PrivateKey
  readonly chain: MemoryChain

  constructor(chain?: MemoryChain, key: PrivateKey = PrivateKey.fromRandom()) {
    this.key = key
    this.chain = chain ?? new MemoryChain({ treasuryKey: key })
  }

  /** change outputs this treasury owns (index = last output of each split) */
  private change: Array<{ tx: Transaction; vout: number }> = []

  /** mature, unspent treasury outputs — coinbases at +100 plus adopted change */
  private async candidates(): Promise<FundingOut[]> {
    const mine = new P2PKH().lock(this.key.toPublicKey().toAddress([0x6f])).toHex()
    const outs: FundingOut[] = []
    for (const b of this.chain.blockList) {
      const cbid = b.txids[0]!
      if (b.header.height + COINBASE_MATURITY - 1 > this.chain.blockList.length - 1) break
      const rec = this.chain.getTx(cbid)
      // only coinbases paying THIS key — a treasury on another key's chain
      // would sign inputs it cannot unlock
      if (rec === undefined || rec.outputs[0]?.lockingScript.toHex() !== mine) continue
      if ((await this.chain.utxoStatus(cbid, 0)) === 'unspent') outs.push({ tx: rec, vout: 0, satoshis: rec.outputs[0]!.satoshis ?? 0 })
    }
    for (const c of this.change) {
      const txid = c.tx.id('hex')
      if ((await this.chain.utxoStatus(txid, c.vout)) === 'unspent') outs.push({ tx: c.tx, vout: c.vout, satoshis: c.tx.outputs[c.vout]!.satoshis ?? 0 })
    }
    return outs
  }

  /** splits serialize: parallel testWorld() calls would otherwise pick the
   *  same candidate outpoints and collide on UTXO_SPENT */
  private splitQueue: Promise<unknown> = Promise.resolve()

  /** one P2PKH split tx paying `outputs` × `sats` to `address`, change back to
   *  the treasury — the memory-tier counterpart of a reserve bootstrap */
  async split(address: string, outputs: number, sats: number): Promise<Transaction> {
    const run = this.splitQueue.then(() => this.doSplit(address, outputs, sats))
    this.splitQueue = run.catch(() => undefined)
    return run
  }

  private async doSplit(address: string, outputs: number, sats: number): Promise<Transaction> {
    const need = outputs * sats + 5_000
    const tx = new Transaction()
    let inSats = 0
    for (const src of await this.candidates()) {
      if (inSats >= need) break
      // proof-less copy: StackFunder spends treasury parents that carry no
      // proofs (reserve entries reload from hex), so children of a fresh
      // funder output travel as Extended Format, not pre-proven BEEF
      tx.addInput({
        sourceTransaction: Transaction.fromHex(src.tx.toHex()),
        sourceOutputIndex: src.vout,
        unlockingScriptTemplate: new P2PKH().unlock(this.key)
      })
      inSats += src.satoshis
    }
    if (inSats < need) throw new Error(`memchain: treasury cannot cover ${need} sats`)
    for (let i = 0; i < outputs; i++) tx.addOutput({ lockingScript: new P2PKH().lock(address), satoshis: sats })
    tx.addOutput({ lockingScript: new P2PKH().lock(this.key.toPublicKey().toAddress([0x6f])), change: true })
    await tx.fee()
    await tx.sign()
    const r = await this.chain.broadcast(tx)
    if (r.status !== 'success') throw new Error(`memchain: treasury split refused: ${r.description ?? ''}`)
    this.change.push({ tx, vout: tx.outputs.length - 1 })
    return tx
  }
}

/** the memory-tier funder — same surface as StackFunder: the funding parent
 *  is a real split tx broadcast to the chain, unconfirmed until a test mines
 *  it, so its outputs carry proofs only once mined (zeroconf parity) */
export class MemoryFunder implements TestFunder {
  keyPair: PrivateKey
  utxos: FundingOut[] = []
  private fundingTx?: Transaction

  private constructor(key: PrivateKey) {
    this.keyPair = key
  }

  /** a funder with no utxos — take() always throws; no chain needed */
  static dry(key: PrivateKey = PrivateKey.fromRandom()): MemoryFunder {
    return new MemoryFunder(key)
  }

  static async fund(t: MemoryTreasury, outputs: number, sats = 10_000): Promise<MemoryFunder> {
    const f = new MemoryFunder(PrivateKey.fromRandom())
    const tx = await t.split(f.address(), outputs, sats)
    // zeroconf, like StackFunder: the split is broadcast but unconfirmed —
    // tests mine only where a proof is actually asserted (chain.mine +
    // merklePathOf). Pre-mining here would hide the unproven-parent paths
    // production hits constantly.
    f.fundingTx = tx
    for (let i = 0; i < outputs; i++) f.utxos.push({ tx, vout: i, satoshis: sats })
    return f
  }

  async take(satoshis: number): Promise<FundingOut> {
    const i = this.utxos.findIndex((u) => u.satoshis >= satoshis)
    if (i < 0) throw new Error('MemoryFunder: no utxo of that size')
    return this.utxos.splice(i, 1)[0]!
  }

  markSpent(txid: string, vout: number): void {
    const i = this.utxos.findIndex((u) => u.tx.id('hex') === txid && u.vout === vout)
    if (i >= 0) this.utxos.splice(i, 1)
  }

  /** return a broadcast tx's output to the pool (change/escrow outs) */
  addOwned(tx: Transaction, vout: number): void {
    const o = tx.outputs[vout]
    if (o !== undefined) this.utxos.push({ tx, vout, satoshis: o.satoshis ?? 0 })
  }

  address(): string {
    return this.keyPair.toPublicKey().toAddress([0x6f])
  }

  get parents(): Transaction[] {
    return this.fundingTx !== undefined ? [this.fundingTx] : []
  }
}

let memTreasury: MemoryTreasury | undefined
export function memoryTreasury(): MemoryTreasury {
  memTreasury ??= new MemoryTreasury()
  return memTreasury
}
