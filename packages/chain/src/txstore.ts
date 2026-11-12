// txstore.ts — the instance's own transaction graph (BRC-62 BEEF, BRC-67 SPV).
//
// Every transaction the instance builds, broadcasts or is handed is kept here
// with its merkle proof once one exists. Reads are local: a spend finds its
// parent, a counterparty gets a BEEF, and a received BEEF is checked against
// the instance's own headers — no indexer and no waiting for one to catch up.
import { Transaction, MerklePath, Spend, LockingScript, type ChainTracker } from '@bsv/sdk'

export interface TxRow { txid: string, raw: string, proof?: string }
export interface TxPersistence {
  load: () => Promise<TxRow[]>
  put: (row: TxRow) => Promise<void>
}

const hexOf = (b: number[] | Uint8Array): string => Buffer.from(b as Uint8Array).toString('hex')
const MAX_DEPTH = 64

/** what a wire payload turned out to be: `spv` is true only when the whole
 *  ancestry verified — scripts, amounts and every proof against `tracker` */
export interface Ingested { tx: Transaction, spv: boolean }

export class TxStore {
  private rows = new Map<string, { raw: string, proof?: MerklePath }>()
  private writes: Array<Promise<void>> = []
  constructor (private readonly persist?: TxPersistence, private readonly max = 20_000) {}

  async load (): Promise<void> {
    for (const r of await this.persist?.load() ?? []) {
      try { this.rows.set(r.txid, { raw: r.raw, ...(r.proof !== undefined ? { proof: MerklePath.fromHex(r.proof) } : {}) }) } catch { /* unreadable row */ }
    }
  }

  /** resolves once every write issued so far has reached the persistence layer */
  async flushed (): Promise<void> { const w = this.writes; this.writes = []; await Promise.allSettled(w) }

  private save (txid: string): void {
    const r = this.rows.get(txid)
    if (r === undefined || this.persist === undefined) return
    const p = this.persist.put({ txid, raw: r.raw, ...(r.proof !== undefined ? { proof: r.proof.toHex() } : {}) }).catch(() => {})
    this.writes.push(p)
    if (this.writes.length > 256) this.writes = this.writes.slice(-64)
  }

  has (txid: string): boolean { return this.rows.has(txid) }

  /** keep `tx` and every ancestor it carries; a proof already held is never dropped */
  put (tx: Transaction, depth = 0): void {
    const txid = tx.id('hex')
    const cur = this.rows.get(txid)
    const proof = cur?.proof ?? tx.merklePath
    if (cur === undefined || (cur.proof === undefined && proof !== undefined)) {
      if (cur === undefined && this.rows.size >= this.max) this.rows.delete(this.rows.keys().next().value!)
      this.rows.set(txid, { raw: cur?.raw ?? tx.toHex(), ...(proof !== undefined ? { proof } : {}) })
      this.save(txid)
    }
    if (proof !== undefined || depth >= MAX_DEPTH) return // a proven tx ends the ancestry
    for (const i of tx.inputs) if (i.sourceTransaction !== undefined) this.put(i.sourceTransaction, depth + 1)
  }

  /** attach a proof the caller has verified against its own headers */
  setProof (txid: string, proof: MerklePath): boolean {
    const cur = this.rows.get(txid)
    if (cur === undefined) return false
    if (cur.proof === undefined) { cur.proof = proof; this.save(txid) }
    return true
  }

  /** the transaction with its known ancestry linked, down to proven parents */
  get (txid: string, depth = 0): Transaction | undefined {
    const r = this.rows.get(txid)
    if (r === undefined) return undefined
    const tx = Transaction.fromHex(r.raw)
    if (r.proof !== undefined) { tx.merklePath = r.proof; return tx }
    if (depth >= MAX_DEPTH) return tx
    for (const i of tx.inputs) {
      const src = i.sourceTXID !== undefined ? this.get(i.sourceTXID, depth + 1) : undefined
      if (src !== undefined) i.sourceTransaction = src
    }
    return tx
  }

  /** true when every path from `tx` back ends in a proven transaction */
  private complete (tx: Transaction, depth = 0): boolean {
    if (tx.merklePath !== undefined) return true
    if (depth >= MAX_DEPTH || tx.inputs.length === 0) return false
    return tx.inputs.every(i => i.sourceTransaction !== undefined && this.complete(i.sourceTransaction, depth + 1))
  }

  /** stored transactions that still lack a proof — candidates for a proof lookup */
  unproven (): string[] { return [...this.rows].filter(([, r]) => r.proof === undefined).map(([id]) => id) }

  /** hex to hand a counterparty: BEEF when the ancestry is complete and fits
   *  `maxBytes` (an unmined chain of large parents does not belong in a
   *  message), else Extended Format when the direct parents are known, else
   *  the raw tx. The receiver runs SPV on a BEEF and otherwise lets the
   *  network's acceptance of the transaction stand in for it. */
  wire (txid: string, maxBytes = 6000): string | undefined {
    const tx = this.get(txid)
    if (tx === undefined) return undefined
    if (this.complete(tx)) {
      try { const beef = tx.toAtomicBEEF(); if (beef.length <= maxBytes) return hexOf(beef) } catch { /* fall through */ }
    }
    if (tx.inputs.every(i => i.sourceTransaction !== undefined)) { try { return hexOf(tx.toEF()) } catch { /* fall through */ } }
    return tx.toHex()
  }

  /** read a wire payload (BEEF, Atomic BEEF, Extended Format or raw) and run
   *  SPV on it. Nothing is stored — the caller keeps it with `put` once it
   *  accepts the transaction. Throws when the bytes are not a transaction. */
  static async read (hex: string, tracker: ChainTracker | undefined): Promise<Ingested> {
    const head = hex.slice(0, 8).toLowerCase()
    if (head === '01010101' || head === '0100beef' || head === '0200beef') {
      const bytes = Array.from(Buffer.from(hex, 'hex'))
      const tx = head === '01010101' ? Transaction.fromAtomicBEEF(bytes) : Transaction.fromBEEF(bytes)
      let spv = false
      if (tracker !== undefined) { try { spv = await tx.verify(tracker) } catch { spv = false } }
      return { tx, spv }
    }
    if (/^[0-9a-f]{8}0000000000ef/i.test(hex)) return { tx: Transaction.fromHexEF(hex), spv: false }
    return { tx: Transaction.fromHex(hex), spv: false }
  }
}

/** does input `vin` of `tx` satisfy the output it spends — run locally, no node */
export function spendValid (tx: Transaction, vin: number, source: { txid: string, vout: number, satoshis: number, script: string }): boolean {
  const i = tx.inputs[vin]
  if (i === undefined || i.unlockingScript === undefined) return false
  if ((i.sourceTXID ?? i.sourceTransaction?.id('hex')) !== source.txid || i.sourceOutputIndex !== source.vout) return false
  try {
    return new Spend({
      sourceTXID: source.txid, sourceOutputIndex: source.vout, sourceSatoshis: source.satoshis,
      lockingScript: LockingScript.fromHex(source.script), transactionVersion: tx.version,
      otherInputs: tx.inputs.filter((_, k) => k !== vin), outputs: tx.outputs, inputIndex: vin,
      unlockingScript: i.unlockingScript, inputSequence: i.sequence ?? 0xffffffff, lockTime: tx.lockTime
    }).validate()
  } catch { return false }
}
