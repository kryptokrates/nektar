// MemoryChain — an in-memory chain that behaves like the node for everything
// the test world uses. Broadcasts go through real admission rules (parents
// resolved from the chain's own store, UTXO spent set, finality, value and a
// ALWAYS-on SDK interpreter run) and mined txs get real BUMPs against real
// 80-byte headers whose PoW passes headerPowValid. The pre-mined 101 blocks
// mean currentHeight() === tipHeight() with genuinely mature coinbases — no
// +100 offset hack, no fake proofs, and nothing the memory tier accepts that
// the node would refuse.
import { Hash, MerklePath, P2PKH, PrivateKey, Script, Spend, Transaction } from '@bsv/sdk'
import { buildBump } from './bump.js'
import { ChainEvents } from './events.js'
import { headerPowValid, serializeHeader } from './headers.js'
import type { BlockHeader, BroadcastResult, Broadcaster, ChainTracker, Funder, FundingOut, TxStatusEvent } from './interfaces.js'
import type { TestChain, TestFunder } from './testtier.js'

const ZERO = '0'.repeat(64)
const COINBASE_SATS = 5_000_000_000
const FAUCET_KEY = PrivateKey.fromRandom()

// the same definitive-reject split StackChain.broadcast applies: spent/script/
// invalid classes are terminal, missing-parent and non-final are transient
const DEFINITIVE_REJECT = /double.?spend|conflict|already.*spent|UTXO_SPENT|mandatory|invalid|reject/i

const rev = (b: Uint8Array) => Uint8Array.from(b).reverse()
const toInternal = (txid: string) => rev(new Uint8Array(Buffer.from(txid, 'hex')))
const toDisplay = (internal: Uint8Array) => Buffer.from(rev(internal)).toString('hex')

function nodeHash(l: Uint8Array, r: Uint8Array): Uint8Array {
  return new Uint8Array(Hash.hash256([...l, ...r]))
}

/** display-order merkle root over display-order txids (same tree buildBump walks) */
function merkleRoot(txids: string[]): string {
  let level = txids.map(toInternal)
  if (level.length === 0) return toDisplay(new Uint8Array(Hash.hash256([0])))
  while (level.length > 1) {
    const next: Uint8Array[] = []
    for (let i = 0; i < level.length; i += 2) {
      const l = level[i]!
      const r = i + 1 < level.length ? level[i + 1]! : l
      next.push(nodeHash(l, r))
    }
    level = next
  }
  return toDisplay(level[0]!)
}

/** height as exactly 4 little-endian bytes — the push len is always valid
 *  (a raw hex string goes odd-length at height 256) */
const le4 = (height: number): string => {
  const b = height.toString(16).padStart(8, '0')
  return b.slice(6, 8) + b.slice(4, 6) + b.slice(2, 4) + b.slice(0, 2)
}

const coinbaseTx = (height: number, key: PrivateKey): Transaction => {
  const tx = new Transaction()
  tx.addInput({
    sourceTXID: ZERO,
    sourceOutputIndex: 0xffffffff,
    unlockingScript: Script.fromHex('04' + le4(height)),
    sequence: 0xffffffff
  })
  tx.addOutput({ lockingScript: new P2PKH().lock(key.toPublicKey().toAddress([0x6f])), satoshis: COINBASE_SATS })
  return tx
}

export interface MemoryFaults {
  /** accepted but never mined until release */
  hold(txid: string): void
  release(txid: string): void
  /** the next broadcast whose txid matches is refused with this description */
  refuseNext(match: string | RegExp, description: string): void
  /** 'timeout' | 'rate-limit' — the events observe throws ("couldn't ask") */
  observe: undefined | 'timeout' | 'rate-limit'
}

export class MemoryChain implements TestChain {
  private headers: BlockHeader[] = []
  private mempool = new Map<string, Transaction>()
  private mined = new Map<string, { height: number; bump: MerklePath }>()
  /** every tx the chain has ever accepted (mempool + mined + coinbases) —
   *  input parents resolve from HERE, never from caller-attached objects */
  private known = new Map<string, Transaction>()
  private spent = new Set<string>()
  private spentBy = new Map<string, Set<string>>()
  private rejectedSet = new Set<string>()
  private held = new Set<string>()
  private refuseQueue: Array<{ match: string | RegExp; description: string }> = []
  private reorgCbs: Array<(removed: BlockHeader[], newTip: BlockHeader) => void> = []
  /** mature + immature faucet coinbase outputs (fund() draws mature only) */
  readonly faucetUtxos: Array<{ tx: Transaction; vout: number; atHeight: number }> = []

  readonly faults: MemoryFaults
  readonly chainEvents: ChainEvents

  constructor() {
    this.faults = {
      observe: undefined,
      hold: (txid) => this.held.add(txid),
      release: (txid) => this.held.delete(txid),
      refuseNext: (match, description) => this.refuseQueue.push({ match, description })
    }
    this.chainEvents = new ChainEvents({
      intervalMs: 50,
      ttlMs: 30_000,
      height: () => Promise.resolve(this.tip()),
      observe: async (txid) => {
        if (this.faults.observe === 'timeout') throw new Error(`observe timeout for ${txid}`)
        if (this.faults.observe === 'rate-limit') throw new Error(`observe rate-limited for ${txid}`)
        const rec = this.mined.get(txid)
        if (rec === undefined) return undefined
        const tx = this.known.get(txid)
        if (tx !== undefined && tx.merklePath === undefined) tx.merklePath = rec.bump
        return { txid, status: 'mined', blockHeight: rec.height, merklePath: rec.bump }
      }
    })
    // pre-mine 101 blocks: the first coinbase is mature and tipHeight is real
    for (let h = 0; h < 101; h++) this.sealBlock([])
  }

  private tip(): number {
    return this.headers.length
  }

  private coinbaseMature(atHeight: number): boolean {
    return this.tip() - atHeight >= 100
  }

  /** seal one block: real coinbase at index 0 + the given mempool txs */
  private sealBlock(txids: string[]): BlockHeader {
    const height = this.tip() + 1
    const cb = coinbaseTx(height, FAUCET_KEY)
    this.known.set(cb.id('hex'), cb)
    this.faucetUtxos.push({ tx: cb, vout: 0, atHeight: height })
    const all = [cb.id('hex'), ...txids]
    const prev = this.headers[this.headers.length - 1]
    const header: BlockHeader = {
      height,
      hash: '',
      prevHash: prev?.hash ?? ZERO,
      merkleRoot: merkleRoot(all),
      time: Math.max(Math.floor(Date.now() / 1000), (prev?.time ?? 0) + 1),
      bits: 0x207fffff,
      nonce: 0,
      version: 4
    }
    for (let n = 0; ; n++) {
      header.nonce = n
      header.hash = Buffer.from(Hash.hash256([...serializeHeader(header)!]))
        .reverse()
        .toString('hex')
      if (headerPowValid(header)) break
    }
    this.headers.push(header)
    // BUMPs attach to the stored txs; the coinbase gets one too (funding
    // parents carry their proof like a real node's mined parents do)
    all.forEach((txid, i) => {
      const bump = buildBump(all, i, height)
      this.mined.set(txid, { height, bump })
      const tx = this.known.get(txid)
      if (tx !== undefined) tx.merklePath = bump
    })
    return header
  }

  async broadcast(tx: Transaction): Promise<BroadcastResult> {
    const txid = tx.id('hex')
    const refuse = (description: string): BroadcastResult => {
      if (DEFINITIVE_REJECT.test(description)) {
        // definitive refuses are terminal: same signal StackChain pushes so
        // reconcile gates clear immediately; transient refusals stay retryable
        this.rejectedSet.add(txid)
        this.chainEvents.report({ txid, status: 'rejected' })
      }
      return { status: 'error', description }
    }
    const staged = this.refuseQueue.findIndex((r) => (typeof r.match === 'string' ? txid.startsWith(r.match) : r.match.test(txid)))
    if (staged >= 0) {
      const { description } = this.refuseQueue.splice(staged, 1)[0]!
      return refuse(description)
    }
    // idempotent resubmit — the same bytes can only produce the same txid
    if (this.mempool.has(txid) || this.mined.has(txid)) return { status: 'success', txid }
    // admission rules, node order: parents → spent → finality → value → scripts
    const parents: Array<{ tx: Transaction; vout: number }> = []
    for (const inp of tx.inputs) {
      const ptxid = inp.sourceTXID ?? inp.sourceTransaction?.id('hex')
      const vout = inp.sourceOutputIndex ?? 0
      if (ptxid === undefined || ptxid === ZERO || vout === 0xffffffff) {
        return refuse(`TX_INVALID (18): coinbase-style input in a mempool tx`)
      }
      const parent = this.known.get(ptxid)
      if (parent === undefined || parent.outputs[vout] === undefined) {
        return refuse(`TX_MISSING_PARENT: ${ptxid}:${vout}`)
      }
      parents.push({ tx: parent, vout })
    }
    const ours = new Set<string>()
    for (let i = 0; i < tx.inputs.length; i++) {
      const ptxid = tx.inputs[i]!.sourceTXID ?? tx.inputs[i]!.sourceTransaction!.id('hex')
      const op = `${ptxid}:${parents[i]!.vout}`
      if (this.spent.has(op)) return refuse(`UTXO_SPENT: ${op} — utxo already spent`)
      ours.add(op)
    }
    if ((tx.lockTime ?? 0) !== 0 && tx.inputs.some((i) => (i.sequence ?? 0xffffffff) !== 0xffffffff)) {
      const tip = this.tip()
      const tipTime = this.headers[tip - 1]?.time ?? 0
      const final = tx.lockTime! < 500_000_000 ? tx.lockTime! < tip + 1 : tx.lockTime! < tipTime
      if (!final) return refuse(`TX_NON_FINAL: lockTime ${tx.lockTime} not reached`)
    }
    const inSats = parents.reduce((s, p) => s + (p.tx.outputs[p.vout].satoshis ?? 0), 0)
    const outSats = tx.outputs.reduce((s, o) => s + (o.satoshis ?? 0), 0)
    if (inSats < outSats) {
      return refuse(`TX_INVALID (4): bad-txns-in-belowout (${inSats} < ${outSats})`)
    }
    // script validation is NOT optional — every input runs the SDK
    // interpreter against the real parent output, exactly like the node
    for (let ix = 0; ix < tx.inputs.length; ix++) {
      const inp = tx.inputs[ix]!
      const p = parents[ix]!
      const ptxid = inp.sourceTXID ?? inp.sourceTransaction!.id('hex')
      try {
        const spend = new Spend({
          sourceTXID: ptxid,
          sourceSatoshis: p.tx.outputs[p.vout].satoshis ?? 0,
          sourceOutputIndex: p.vout,
          lockingScript: p.tx.outputs[p.vout].lockingScript,
          transactionVersion: tx.version,
          otherInputs: tx.inputs
            .filter((_, j) => j !== ix)
            .map((i) => ({
              sourceTXID: i.sourceTXID ?? i.sourceTransaction?.id('hex') ?? '',
              sourceOutputIndex: i.sourceOutputIndex ?? 0,
              sequence: i.sequence ?? 0xffffffff
            })),
          inputIndex: ix,
          unlockingScript: inp.unlockingScript,
          outputs: tx.outputs,
          inputSequence: inp.sequence ?? 0xffffffff,
          lockTime: tx.lockTime ?? 0
        } as ConstructorParameters<typeof Spend>[0])
        if (!spend.validate()) {
          return refuse(`TX_INVALID (31): Script failed an OP_EQUALVERIFY operation (input ${ix})`)
        }
      } catch (e) {
        return refuse(`TX_INVALID (31): Script evaluation threw on input ${ix}: ${String((e as Error).message).slice(0, 90)}`)
      }
    }
    for (const op of ours) this.spent.add(op)
    this.spentBy.set(txid, ours)
    this.mempool.set(txid, tx)
    this.known.set(txid, tx)
    this.chainEvents.watch(txid)
    return { status: 'success', txid }
  }

  /** mine n blocks; when this resolves, 'tx' mined events for every watched
   *  tx sealed in them have already been delivered (probeNow, not a timer) */
  async mine(n = 1): Promise<void> {
    for (let b = 0; b < n; b++) {
      const txids = [...this.mempool.keys()].filter((t) => !this.held.has(t))
      this.sealBlock(txids)
      for (const txid of txids) this.mempool.delete(txid)
      // deliver proofs now — callers must see 'mined' + merklePath before
      // mine() resolves, and no interval tick may be left owning the wait
      for (const txid of txids) await this.chainEvents.probeNow(txid)
    }
  }

  /** drop the last n blocks: txs return to the mempool unproven, onReorg
   *  fires with the removed headers and the new tip */
  reorg(n: number): void {
    const removed = this.headers.splice(this.headers.length - n, n)
    for (const h of removed) {
      for (const [txid, rec] of [...this.mined]) {
        if (rec.height !== h.height) continue
        this.mined.delete(txid)
        const tx = this.known.get(txid)
        if (tx !== undefined) {
          // coinbases die with their block; real txs go back to the mempool
          if (tx.inputs.some((i) => (i.sourceTXID ?? '') === ZERO)) {
            this.known.delete(txid)
            for (const op of this.spentBy.get(txid) ?? []) this.spent.delete(op)
            this.spentBy.delete(txid)
            const fi = this.faucetUtxos.findIndex((u) => u.tx.id('hex') === txid)
            if (fi >= 0) this.faucetUtxos.splice(fi, 1)
          } else {
            delete tx.merklePath
            this.mempool.set(txid, tx)
            // the chain itself knows the tx regressed to the mempool — a
            // verified unconfirmed supersedes the stale mined status so the
            // re-mine delivers a fresh event instead of deduping away
            this.chainEvents.report({ txid, status: 'unconfirmed' })
            this.chainEvents.watch(txid)
          }
        }
      }
    }
    if (removed.length > 0) {
      const tip = this.headers[this.headers.length - 1]!
      for (const cb of this.reorgCbs) cb(removed, tip)
    }
  }

  /** evict an accepted mempool tx, free its inputs, report rejected — the
   *  accepted-then-rejected case */
  reject(txid: string): boolean {
    if (!this.mempool.has(txid)) return false
    this.mempool.delete(txid)
    this.known.delete(txid)
    this.rejectedSet.add(txid)
    for (const op of this.spentBy.get(txid) ?? []) this.spent.delete(op)
    this.spentBy.delete(txid)
    this.chainEvents.report({ txid, status: 'rejected' })
    return true
  }

  getTx(txid: string): Transaction | undefined {
    return this.known.get(txid)
  }
  isRejected(txid: string): boolean {
    return this.rejectedSet.has(txid)
  }
  onStatus(cb: (ev: TxStatusEvent) => void): void {
    this.chainEvents.on('tx', cb)
    this.chainEvents.start()
  }
  onReorg(cb: (removed: BlockHeader[], newTip: BlockHeader) => void): void {
    this.reorgCbs.push(cb)
  }
  stopWatching(): void {
    this.chainEvents.stop()
  }
  async waitProof(txid: string, timeoutMs = 30_000): Promise<Transaction | undefined> {
    await this.chainEvents.waitFor(txid, timeoutMs)
    return this.known.get(txid)
  }
  async currentHeight(): Promise<number> {
    return this.tip()
  }
  async tipHeight(): Promise<number> {
    return this.tip()
  }
  async isValidRootForHeight(root: string, height: number): Promise<boolean> {
    return this.headers[height - 1]?.merkleRoot === root
  }
  async headerAtHeight(height: number): Promise<BlockHeader | undefined> {
    return this.headers[height - 1]
  }
  async fetchTx(txid: string): Promise<Transaction | undefined> {
    return this.known.get(txid)
  }
  async merklePathOf(txid: string): Promise<MerklePath | undefined> {
    return this.mined.get(txid)?.bump
  }
  async utxoStatus(txid: string, vout: number): Promise<'unspent' | 'spent' | 'unknown'> {
    if (this.spent.has(`${txid}:${vout}`)) return 'spent'
    const tx = this.known.get(txid)
    if (tx === undefined || tx.outputs[vout] === undefined) return 'unknown'
    return 'unspent'
  }
  async pendingBroadcasts(): Promise<number> {
    return this.mempool.size
  }

  /** the faucet's mature, unspent coinbase outputs */
  matureFaucet(): Array<{ tx: Transaction; vout: number; atHeight: number }> {
    return this.faucetUtxos.filter((u) => this.coinbaseMature(u.atHeight) && !this.spent.has(`${u.tx.id('hex')}:${u.vout}`))
  }
  /** track a faucet-side change outpoint (spendable immediately — only
   *  coinbase outputs carry the maturity rule) */
  addFaucetUtxo(tx: Transaction, vout: number): void {
    this.faucetUtxos.push({ tx, vout, atHeight: -10_000 })
  }
}

/** a funder backed by the chain's faucet — same zero-conf semantics as
 *  StackFunder.fund: one funding tx with `outputs` P2PKH outputs to a fresh
 *  key, broadcast and left unconfirmed; the faucet parent is attached
 *  proof-less so a child of a fresh funder output travels as Extended Format —
 *  tier parity with StackFunder's unproven treasury parents. Concurrent fund
 *  calls serialise (the faucet change is a chain). Nothing is written to disk. */
export class MemoryFunder implements TestFunder {
  keyPair: PrivateKey
  private utxos: FundingOut[] = []
  private fundingTx?: Transaction
  private static queue: Promise<unknown> = Promise.resolve()

  private constructor(key: PrivateKey) {
    this.keyPair = key
  }

  static dry(key: PrivateKey = PrivateKey.fromRandom()): MemoryFunder {
    return new MemoryFunder(key)
  }

  static async fund(chain: MemoryChain, outputs: number, sats = 10_000): Promise<MemoryFunder> {
    const run = MemoryFunder.queue.then(() => MemoryFunder.fundNow(chain, outputs, sats))
    MemoryFunder.queue = run.catch(() => undefined)
    return run
  }

  private static async fundNow(chain: MemoryChain, outputs: number, sats: number): Promise<MemoryFunder> {
    const f = new MemoryFunder(PrivateKey.fromRandom())
    const addr = f.keyPair.toPublicKey().toAddress([0x6f])
    const need = outputs * sats + 5_000
    const tx = new Transaction()
    let inSats = 0
    const used: Array<{ tx: Transaction; vout: number }> = []
    for (const u of chain.matureFaucet()) {
      // proof-less copy: StackFunder spends treasury parents that carry no
      // proofs, so children of a fresh funder output travel as Extended Format
      tx.addInput({ sourceTransaction: Transaction.fromHex(u.tx.toHex()), sourceOutputIndex: u.vout, unlockingScriptTemplate: new P2PKH().unlock(FAUCET_KEY) })
      used.push(u)
      inSats += u.tx.outputs[u.vout].satoshis ?? 0
      if (inSats >= need) break
    }
    if (inSats < need) throw new Error('MemoryFunder: faucet dry — mine blocks first')
    for (let i = 0; i < outputs; i++) tx.addOutput({ lockingScript: new P2PKH().lock(addr), satoshis: sats })
    tx.addOutput({ lockingScript: new P2PKH().lock(FAUCET_KEY.toPublicKey().toAddress([0x6f])), change: true })
    await tx.fee()
    await tx.sign()
    const r = await chain.broadcast(tx)
    if (r.status !== 'success') throw new Error(`MemoryFunder: fund broadcast failed ${r.description ?? ''}`.slice(0, 160))
    const changeVout = tx.outputs.length - 1
    if ((tx.outputs[changeVout].satoshis ?? 0) > 0) chain.addFaucetUtxo(tx, changeVout)
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
