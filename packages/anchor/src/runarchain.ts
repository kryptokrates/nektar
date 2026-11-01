// AnchorChain v2 — one mamem anchor UTXO per shard, enforced by the Rúnar
// stateful contract `MamemAnchor` (packages/contracts). No hand-written
// script: locking/unlocking is built by runar-sdk; funding inputs use the
// SDK P2PKH template (plain fee funding is allowed).
import { makeRotationCert } from "@mamem/identity"
import { Transaction, PrivateKey, LockingScript, P2PKH } from '@bsv/sdk'
import { RunarContract, LocalSigner } from 'runar-sdk'
import type { Provider, UTXO as RUTXO, TransactionData } from 'runar-sdk'
// default burstLimit=100: measured 100 unconfirmed contract anchors on
// regtest with no limit (see spikes/depth-contract.mjs); conservative default.
import { MAMEM_ANCHOR_ARTIFACT, contractRp, decodeMamemAnchor, TAG_RPRIME } from '@mamem/contracts'
const TAG_HEX = toHex(TAG_RPRIME)
import { PROTOCOL_ID, dsha256, utf8, toHex, fromHex, expectedRp, type AnchorData, type Bytes } from '@mamem/core'
import type { Broadcaster, BroadcastResult, Funder } from '@mamem/chain'

export interface Head {
  tx: Transaction
  vout: number
  data: AnchorData & { ownerPkh?: Bytes }
}
export interface AnchorResult { tx: Transaction, head: Head }
export interface AnchorOptions { burstLimit?: number, rotationHorizon?: number, feeRateSatPerKb?: number }

// Spent outpoints and spendable change are tracked PER FUNDER, not per
// provider: several AnchorChains (one per shard) share one funder, and each
// must see what the others already spent.
interface FunderBook { spent: Set<string>, owned: Map<string, RUTXO> }
const books = new WeakMap<object, FunderBook>()
function bookFor (funder: object): FunderBook {
  let b = books.get(funder)
  if (!b) { b = { spent: new Set(), owned: new Map() }; books.set(funder, b) }
  return b
}

/** runar-sdk Provider backed by our Broadcaster/Funder (+ optional utxo feed). */
export class MamemProvider implements Provider {
  network: 'testnet' = 'testnet'
  lastTx?: Transaction
  constructor (
    public broadcaster: Broadcaster,
    private funder: Funder,
    public funderAddress?: string
  ) {}
  /** called on every broadcast attempt AND again on success. */
  onAttempt?: (tx: Transaction) => void
  onSuccess?: (tx: Transaction) => void

  async getTransaction (txid: string): Promise<TransactionData> {
    const hex = await this.getRawTransaction(txid)
    if (!hex) throw new Error(`tx ${txid} not found`)
    // parse the real tx — runar uses these fields to fill the input's
    // sourceSatoshis/lockingScript; an empty outputs[] produces a bad preimage
    const tx = Transaction.fromHex(hex)
    return {
      txid, version: tx.version,
      inputs: tx.inputs.map(i => ({ txid: i.sourceTXID ?? i.sourceTransaction?.id('hex'), outputIndex: i.sourceOutputIndex, script: '', sequence: i.sequence })),
      outputs: tx.outputs.map(o => ({ script: o.lockingScript.toHex(), satoshis: o.satoshis })),
      locktime: tx.lockTime, raw: hex
    } as unknown as TransactionData
  }
  private recent = new Map<string, Transaction>()
  recentOf (txid: string): Transaction | undefined { return this.recent.get(txid) }
  /** make a known tx resolvable as a parent (e.g. a head restored after restart) */
  rememberTx (tx: Transaction): void { this.recent.set(tx.id('hex'), tx) }
  async getRawTransaction (txid: string): Promise<string> {
    if (this.lastTx && this.lastTx.id('hex') === txid) return this.lastTx.toHex()
    return this.recent.get(txid)?.toHex() ?? ''
  }
  private get spent (): Set<string> { return bookFor(this.funder).spent }
  private get owned (): Map<string, RUTXO> { return bookFor(this.funder).owned } // outpoint -> spendable output we created

  /** the tx as it was last presented for broadcast — success or failure. */
  lastAttempt?: Transaction
  async broadcast (tx: Transaction): Promise<string> {
    this.lastAttempt = tx
    this.onAttempt?.(tx) // record BEFORE the outcome — an ambiguous accept must be findable
    const res = await this.broadcaster.broadcast(tx)
    if (res.status !== 'success') throw new Error(res.description ?? 'broadcast failed')
    return this.markAccepted(tx)
  }

  /** Book-keeping for a tx the network accepted — on a direct success AND
   *  when an earlier ambiguous attempt is adopted or re-broadcast: its inputs
   *  are spent (in the shared per-funder book and the funder itself) and our
   *  own P2PKH change outputs become spendable. */
  markAccepted (tx: Transaction): string {
    // record spent outpoints + our own spendable change outputs — mark them in
    // the funder too, so a peeking reserve stops handing them out
    for (const inp of tx.inputs) {
      const stx = inp.sourceTXID ?? inp.sourceTransaction?.id('hex')
      if (stx != null) {
        this.spent.add(`${stx}:${inp.sourceOutputIndex ?? 0}`)
        ;(this.funder as any).markSpent?.(stx, inp.sourceOutputIndex ?? 0)
      }
    }
    const txid = tx.id('hex')
    const fundScript = new P2PKH().lock(this.funderAddress ?? '').toHex()
    tx.outputs.forEach((o, vout) => {
      const script = o.lockingScript.toHex()
      // spendable only if it's the funder's own P2PKH script
      if (script === fundScript) {
        this.owned.set(`${txid}:${vout}`, { txid, outputIndex: vout, satoshis: o.satoshis ?? 0, script })
        ;(this.funder as any).addOwned?.(tx, vout) // return change to the reserve
      }
    })
    // record AFTER success only — a failed tx must never shadow its own parent.
    // Keep the ORIGINAL object (mine()/confirm() attach merklePath to it); the
    // 2.8.11→4.0.1 normalization happens in beefOf().
    this.lastTx = tx
    this.recent.set(txid, tx)
    this.onSuccess?.(tx)
    return txid
  }
  async getUtxos (address: string): Promise<RUTXO[]> {
    // reserve funders hand out one entry per call (optimistically spent) —
    // every stale/spent candidate is consumed at most once across all runs
    const takeFn = (this.funder as any).takeReserve
    if (typeof takeFn === 'function') {
      const u = await takeFn.call(this.funder)
      if (!u) return []
      this.taken.set(u.tx.id('hex'), u.tx)
      return [{ txid: u.tx.id('hex'), outputIndex: u.vout, satoshis: u.satoshis ?? u.tx.outputs[u.vout]!.satoshis!, script: u.tx.outputs[u.vout]!.lockingScript.toHex() }]
    }
    const all = (this.funder as any).utxos as Array<{ tx: Transaction, vout: number, satoshis: number }> | undefined ?? []
    const funding = all.map(u => ({
      txid: u.tx.id('hex'), outputIndex: u.vout,
      satoshis: u.satoshis ?? u.tx.outputs[u.vout]!.satoshis!,
      script: u.tx.outputs[u.vout]!.lockingScript.toHex()
    }))
    // only hand runar a few unspent candidates — a large pool makes it
    // iterate every entry (slow/hang), and an empty pool must fail fast
    const free = [...funding, ...this.owned.values()]
      .filter(u => !this.spent.has(`${u.txid}:${u.outputIndex}`))
    return free.slice(0, 10)
  }
  taken = new Map<string, Transaction>()
  async getContractUtxo (_scriptHash: string): Promise<RUTXO | null> { return null }
  getNetwork (): 'testnet' { return 'testnet' }
  feeRate = 1 // sats per KB, per runar-sdk fee computation
  async getFeeRate (): Promise<number> { return this.feeRate }
}

export class AnchorChain {
  readonly shard: string // label (also used as nothing else — shardId = hash160(owner))
  shardKey: PrivateKey // changes on rotate()
  private fundKey: PrivateKey
  private signer: LocalSigner
  private provider: MamemProvider
  private contract?: RunarContract
  private head?: Head
  private pendingHeads: Head[] = []
  private txById = new Map<string, Transaction>()
  readonly rotationCerts: import("@mamem/identity").RotationCert[] = []

  constructor (
    shard: string,
    shardKey: PrivateKey,
    private funder: Funder,
    fundKey: PrivateKey,
    broadcaster: Broadcaster,
    private opts: AnchorOptions = {}
  ) {
    this.shard = shard
    this.shardKey = shardKey
    this.fundKey = fundKey
    // Two keys, two roles: the runar-connected signer is the FUND key (it signs
    // the P2PKH funding inputs, which are locked to the funder); the contract's
    // Sig argument (owner authorization) is signed separately with the SHARD
    // key via prepareCall/finalizeCall. Shards therefore don't need their own
    // funders, and distinct shards get distinct owners (distinct shard ids).
    this.signer = new LocalSigner(fundKey.toHex())
    this.provider = new MamemProvider(broadcaster, funder)
    if (opts.feeRateSatPerKb !== undefined) this.provider.feeRate = opts.feeRateSatPerKb
    const rec = (tx: Transaction) => { this.ownAttempts.add(tx.id('hex')); this.txById.set(tx.id('hex'), tx) }
    this.provider.onAttempt = rec
    this.provider.onSuccess = rec
    void this.signer.getAddress().then(a => { this.provider.funderAddress = a })
  }

  private _shardId?: string
  getShardId (): string {
    // shard = hash160(genesisOwner) — taken from the decoded on-chain head so
    // it stays stable across owner rotations (the signer key changes)
    const fromHead = this.getHead()?.data.shard
    if (fromHead) return (this._shardId = fromHead)
    if (this._shardId) return this._shardId
    return (this._shardId = toHexPub160(this.ownerPub()))
  }
  /** the current owner (shard) public key — authorizes contract calls */
  private ownerPub (): string { return this.shardKey.toPublicKey().toString() }

  getHead (): Head | undefined { return this.pendingHeads.at(-1) ?? this.head }

  /** Restart: rehydrate from a persisted head tx (with its ancestry/BUMP via
   *  BEEF). The tx must decode as an authentic anchor of THIS owner; the next
   *  append spends exactly this UTXO — no new genesis, no fork. */
  resume (headTx: Transaction, opts: { confirmed: boolean, certs?: import('@mamem/identity').RotationCert[] }): Head {
    if (this.getHead()) throw new Error('resume: chain already has a head')
    const dec = decodeMamemAnchor(headTx.outputs[0]!.lockingScript.toHex())
    if (!dec) throw new Error('resume: head tx output 0 is not an authentic MamemAnchor')
    if (toHex(dec.ownerPkh) !== toHexPub160(this.ownerPub())) throw new Error('resume: head owner is not this shard key')
    const head: Head = { tx: headTx, vout: 0, data: { shard: dec.shard, n: dec.n, size: dec.size, R: dec.R, Rp: dec.Rp, ownerPkh: dec.ownerPkh } }
    if (opts.confirmed) this.head = head
    else this.pendingHeads.push(head)
    this.register(headTx)
    this.provider.rememberTx(headTx)
    if (opts.certs) this.rotationCerts.push(...opts.certs)
    return head
  }

  async anchor (size: number, R: Bytes): Promise<AnchorResult> {
    const limit = this.opts.burstLimit ?? 100
    if (this.pendingHeads.length >= limit) {
      throw new Error(`anchor back-pressure: unconfirmed depth ${this.pendingHeads.length} >= burstLimit ${limit}`)
    }
    return await this.withRetry(async () => await this.anchorOnce(size, R))
  }

  /** Accept a tx as the new head: mark its inputs spent / change spendable,
   *  register it, decode its continuation output. Used for direct successes,
   *  identical re-broadcasts and adopted earlier attempts alike. */
  private acceptHead (tx: Transaction): AnchorResult {
    this.provider.markAccepted(tx)
    this.register(tx)
    const dec = decodeMamemAnchor(tx.outputs[0]!.lockingScript.toHex())
    if (!dec) throw new Error('accepted tx: continuation output did not decode')
    const head: Head = { tx, vout: 0, data: { shard: dec.shard, n: dec.n, size: dec.size, R: dec.R, Rp: dec.Rp, ownerPkh: dec.ownerPkh } }
    this.pendingHeads.push(head)
    return { tx, head }
  }

  /** If `msg` reports our head spent by one of our own attempts, that earlier
   *  attempt landed — return it for adoption. */
  private ownSpenderOfHead (msg: string): Transaction | undefined {
    const m = /([0-9a-f]{64}):(\d+) utxo already spent(?: by tx ([0-9a-f]{64}))?/.exec(msg)
    const h = this.getHead()
    if (!m || !h || !m[3] || `${m[1]}:${m[2]}` !== `${h.tx.id('hex')}:${h.vout}`) return undefined
    if (!this.isOwnAttempt(m[3])) return undefined
    return this.txById.get(m[3]) ?? this.provider.recentOf(m[3])
  }

  /** Shared retry policy for every head-moving call (append, rotate):
   *  - UTXO_SPENT on our head by our own earlier attempt → adopt that attempt;
   *  - UTXO_SPENT on a funding input → mark it spent and rebuild;
   *  - ambiguous errors (TX_MISSING_PARENT, timeouts) → re-broadcast the
   *    IDENTICAL bytes with backoff, never rebuild.
   *  Everything else is thrown. */
  private async withRetry (buildOnce: () => Promise<AnchorResult>): Promise<AnchorResult> {
    for (let attempt = 0; ; attempt++) {
      try { return await buildOnce() } catch (e) {
        const msg = String((e as Error).message)
        const adopted = this.ownSpenderOfHead(msg)
        if (adopted) return this.acceptHead(adopted)
        const sm = /([0-9a-f]{64}):(\d+) utxo already spent/.exec(msg)
        const h = this.getHead()
        if (sm && (!h || `${sm[1]}:${sm[2]}` !== `${h.tx.id('hex')}:${h.vout}`)) {
          ;(this.funder as any).markSpent?.(sm[1]!, Number(sm[2]))
          bookFor(this.funder).spent.add(`${sm[1]}:${sm[2]}`)
          this.contract = undefined
          if (attempt >= 5) throw e
          continue
        }
        if (!/TX_MISSING_PARENT|timed? ?out|ETIMEDOUT/i.test(msg)) throw e
        const attemptTx = this.provider.lastAttempt
        if (!attemptTx) throw e
        for (let r = 0; r < 10; r++) {
          await new Promise(resolve => setTimeout(resolve, 250 * (r + 1)))
          let res
          try { res = await this.provider.broadcaster.broadcast(attemptTx) } catch { res = undefined }
          if (res?.status === 'success') return this.acceptHead(attemptTx)
          const inner = res?.status === 'error' ? res.description ?? '' : ''
          const adoptedInner = this.ownSpenderOfHead(inner)
          if (adoptedInner) return this.acceptHead(adoptedInner)
          if (inner && !/TX_MISSING_PARENT/.test(inner)) throw new Error(inner)
        }
        throw e
      }
    }
  }

  /** txids we broadcast during this chain's lifetime (for own-attempt detection). */
  private ownAttempts = new Set<string>()
  private isOwnAttempt (txid: string): boolean { return this.ownAttempts.has(txid) }

  private async anchorOnce (size: number, R: Bytes): Promise<AnchorResult> {
    // fail fast on dry funding — never build or broadcast a contract-only tx
    const funding = await this.provider.getUtxos(await this.signer.getAddress())
    if (funding.length === 0) throw new Error('reserve dry: no funding UTXO available')
    const cur = this.getHead()
    const pub = this.ownerPub()
    if (!cur) {
      // genesis: deploy with n=0, Rp = contractRp(0, R, size)
      const Rp = contractRp(new Uint8Array(32), R, size)
      this.contract = new RunarContract(MAMEM_ANCHOR_ARTIFACT, [pub, pkhOf(pub), 0n, BigInt(size), toHex(R), toHex(Rp), TAG_HEX])
      this.contract.connect(this.provider, this.signer)
      const d = await this.contract.deploy({ satoshis: 1 })
      const tx = this.provider.lastTx!
      this.register(tx)
      const gdec = decodeMamemAnchor(tx.outputs[0]!.lockingScript.toHex())
      const head: Head = { tx, vout: 0, data: gdec ? { shard: gdec.shard, n: gdec.n, size: gdec.size, R: gdec.R, Rp: gdec.Rp } : { shard: toHexPub160(pub), n: 0, size, R, Rp } }
      this.pendingHeads.push(head)
      return { tx, head }
    }
    // successor: always rehydrate a FRESH contract from the current head —
    // runar mutates _state at prepareCall, so a failed broadcast would leave
    // it advanced. Head state comes only from decoding the broadcast tx's
    // continuation output, never recomputed.
    const contract = await this.freshContract(cur)
    const prep = await contract.prepareCall('append', [null, pub, toHex(R), BigInt(size)])
    const digest = Array.from(Buffer.from(prep.sighash, 'hex'))
    const der = Buffer.from(this.shardKey.sign(digest).toDER()).toString('hex') + '41'
    const sigMap: Record<number, string> = {}
    for (const i of prep.sigIndices) sigMap[i] = der
    await contract.finalizeCall(prep, sigMap)
    const tx = this.provider.lastTx!
    this.register(tx)
    const decoded = decodeMamemAnchor(tx.outputs[0]!.lockingScript.toHex())
    if (!decoded) throw new Error('anchor: continuation output did not decode')
    const head: Head = { tx, vout: 0, data: { shard: decoded.shard, n: decoded.n, size: decoded.size, R: decoded.R, Rp: decoded.Rp, ownerPkh: decoded.ownerPkh } }
    this.pendingHeads.push(head)
    return { tx, head }
  }

  /** Rotate the anchor owner to a new key. Only after the rotate tx is
   *  broadcast and its continuation decoded does the signer switch. */
  async rotate (newKey: PrivateKey): Promise<AnchorResult> {
    const oldKey = this.shardKey
    const newPub = newKey.toPublicKey().toString()
    const res = await this.withRetry(async () => await this.rotateOnce(newKey))
    // emit the matching rotation certificate (IDN-002..005): C(i) signed by
    // the old key binding pk(i-1)→pk(i), indexed by this shard's cert chain
    this.rotationCerts.push(makeRotationCert(oldKey, newPub, Math.floor(Date.now() / 1000), this.opts.rotationHorizon ?? 86_400))
    // switch the owner key only after the rotate is accepted (the funding
    // signer is the fund key and does not change)
    this.shardKey = newKey
    return res
  }

  private async rotateOnce (newKey: PrivateKey): Promise<AnchorResult> {
    const funding = await this.provider.getUtxos(await this.signer.getAddress())
    if (funding.length === 0) throw new Error('reserve dry: no funding UTXO available')
    const cur = this.getHead()
    if (!cur) throw new Error('rotate: no anchor head')
    const contract = await this.freshContract(cur)
    const pub = this.ownerPub()
    const newPub = newKey.toPublicKey().toString()
    const prep = await contract.prepareCall('rotate', [null, pub, pkhOf(newPub)])
    const digest = Array.from(Buffer.from(prep.sighash, 'hex'))
    const der = Buffer.from(this.shardKey.sign(digest).toDER()).toString('hex') + '41'
    const sigMap: Record<number, string> = {}
    for (const i of prep.sigIndices) sigMap[i] = der
    await contract.finalizeCall(prep, sigMap)
    const tx = this.provider.lastTx!
    this.register(tx)
    const decoded = decodeMamemAnchor(tx.outputs[0]!.lockingScript.toHex())
    if (!decoded) throw new Error('rotate: continuation output did not decode')
    const head: Head = { tx, vout: 0, data: { shard: decoded.shard, n: decoded.n, size: decoded.size, R: decoded.R, Rp: decoded.Rp, ownerPkh: decoded.ownerPkh } }
    this.pendingHeads.push(head)
    return { tx, head }
  }

  private async freshContract (cur: Head): Promise<RunarContract> {
    // every field comes from the decoded on-chain head (genesisOwner and
    // ownerPkh included) — never from local key state
    const script = cur.tx.outputs[cur.vout]!.lockingScript.toHex()
    const dec = decodeMamemAnchor(script)
    if (!dec) throw new Error('freshContract: head output is not an authentic anchor')
    const c = new RunarContract(MAMEM_ANCHOR_ARTIFACT, [dec.ownerPub, toHex(dec.ownerPkh), BigInt(dec.n), BigInt(dec.size), toHex(dec.R), toHex(dec.Rp), TAG_HEX])
    c.connect(this.provider, this.signer)
    c['currentUtxo'] = { txid: cur.tx.id('hex'), outputIndex: cur.vout, satoshis: cur.tx.outputs[cur.vout]!.satoshis!, script }
    return c
  }

  confirm (txid: string, merklePath?: any): void {
    const idx = this.pendingHeads.findIndex(h => h.tx.id('hex') === txid)
    if (idx < 0) return
    for (let i = 0; i <= idx; i++) {
      const h = this.pendingHeads[i]!
      // a mined descendant implies its ancestors are mined, but each tx keeps
      // ITS OWN proof — never the descendant's BUMP (it does not contain them)
      if (i === idx && merklePath) h.tx.merklePath = merklePath
      this.head = h
    }
    this.pendingHeads.splice(0, idx + 1)
  }

  async handleReorg (lostTxids: string[]): Promise<void> {
    if (this.head && lostTxids.includes(this.head.tx.id('hex'))) {
      this.pendingHeads.unshift(this.head)
      this.head = undefined
    }
    for (const h of this.pendingHeads) {
      if (lostTxids.includes(h.tx.id('hex'))) {
        const res = await (this.provider as any).broadcaster.broadcast(h.tx)
        if (res.status !== 'success') throw new Error(`re-anchor broadcast failed: ${res.description}`)
      }
    }
  }

  get shardId (): string {
    return this.getShardId()
  }
  headAtomicBEEF (): number[] | undefined {
    const h = this.getHead()
    return h ? h.tx.toAtomicBEEF() : undefined
  }
  private register (tx: Transaction): void {
    // attach source parents for BEEF completeness: funding inputs come from
    // the funder; the contract input comes from the tracked head chain
    const fundUtxos = new Map<string, Transaction>(this.provider.taken)
    for (const u of ((this.funder as any).utxos ?? []) as Array<{ tx: Transaction, vout: number }>) fundUtxos.set(u.tx.id('hex'), u.tx)
    for (const inp of tx.inputs) {
      if (inp.sourceTransaction) continue
      const txid = inp.sourceTXID
      if (!txid) continue
      const parent = fundUtxos.get(txid) ?? this.txById.get(txid)
      if (parent) inp.sourceTransaction = parent
    }
    this.txById.set(tx.id('hex'), tx)
  }

  beefOf (tx: Transaction, atomic = false): number[] {
    const ntx = this.normalize(tx, new Map())
    return atomic ? ntx.toAtomicBEEF() : ntx.toBEEF()
  }

  /** Recursively rebuild `tx` and its ancestry as project-class (sdk 4.0.1)
   *  Transactions, attaching every input's parent from our registries, until
   *  a tx carries a merkle proof (a proven tx needs no further ancestry).
   *  runar-sdk builds txs with its own @bsv/sdk 2.8.11 class, whose objects
   *  lack the source links 4.0.1 BEEF serialization requires. */
  private normalize (tx: Transaction, memo: Map<string, Transaction>): Transaction {
    const txid = tx.id('hex')
    const hit = memo.get(txid)
    if (hit) return hit
    const ntx = Transaction.fromHex(tx.toHex())
    const mp = tx.merklePath ?? this.txById.get(txid)?.merklePath
    if (mp) ntx.merklePath = mp
    memo.set(txid, ntx)
    if (ntx.merklePath) return ntx // proven: ancestry not needed
    tx.inputs.forEach((inp: any, ix: number) => {
      const ptxid = inp.sourceTXID ?? inp.sourceTransaction?.id('hex')
      if (ntx.inputs[ix]!.sourceTXID === undefined) ntx.inputs[ix]!.sourceTXID = ptxid
      const src = inp.sourceTransaction ?? (ptxid ? (this.txById.get(ptxid) ?? this.provider.recentOf(ptxid) ?? this.provider.taken.get(ptxid)) : undefined)
      if (src) ntx.inputs[ix]!.sourceTransaction = this.normalize(src as Transaction, memo)
    })
    return ntx
  }
}

function toHexPub160 (pub: string): string {
  return toHexPub160Impl(pub)
}
const pkhOf = toHexPub160
import { hash160 } from '@mamem/core'
function toHexPub160Impl (pub: string): string {
  return toHex(hash160(Uint8Array.from(Buffer.from(pub, 'hex'))))
}
