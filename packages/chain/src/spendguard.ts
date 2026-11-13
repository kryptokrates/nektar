// SpendGuard — hard caps in front of any Broadcaster: refuse over-cap fees,
// over-budget daily spend, and too-frequent broadcasts, and FAIL CLOSED when an
// input value can't be resolved (never guess a fee on mainnet).
import { readFileSync, writeFileSync, renameSync, mkdirSync } from 'node:fs'
import path from 'node:path'
import type { Transaction } from '@bsv/sdk'
import type { Broadcaster, BroadcastResult } from './interfaces.js'

export interface SpendGuardOpts {
  maxFeePerTx?: number // sats — refuse a tx whose fee exceeds this
  maxPerDay?: number // sats — refuse once today's UTC spend would exceed this
  minIntervalMs?: number // ms — refuse a broadcast sooner than this after the last
  ledgerFile?: string // persisted day ledger + cost record (atomic write)
  unlimited?: boolean // regtest default: caps off unless env set
  refuseOnInterval?: boolean // default false: wait out the interval instead of refusing
}

export interface CostRecord {
  txid: string
  bytes: number
  fee: number
  /** fee + value committed to scripts foreign to all inputs — the daily-cap
   *  metric. A covenant-input tx (refund/claim/reveal) moves value back to
   *  self and counts fee only. */
  spend: number
  at: string
}
type Ledger = { day: string; spentSats: number; lastBroadcastAt: number; records: CostRecord[] }

export class SpendGuard implements Broadcaster {
  private opts: Required<Pick<SpendGuardOpts, 'maxFeePerTx' | 'maxPerDay' | 'minIntervalMs'>>
  private ledger: Ledger
  private ledgerFile?: string
  private unlimited: boolean
  private refuseOnInterval: boolean

  constructor(
    private inner: Broadcaster,
    private valueOf: (txid: string, vout: number) => number | undefined,
    opts: SpendGuardOpts = {}
  ) {
    this.unlimited = opts.unlimited ?? false
    this.refuseOnInterval = opts.refuseOnInterval ?? false
    this.opts = {
      maxFeePerTx: opts.maxFeePerTx ?? Number.MAX_SAFE_INTEGER,
      maxPerDay: opts.maxPerDay ?? Number.MAX_SAFE_INTEGER,
      minIntervalMs: opts.minIntervalMs ?? 0
    }
    this.ledgerFile = opts.ledgerFile
    this.ledger = this.unlimited ? { day: this.day(), spentSats: 0, lastBroadcastAt: 0, records: [] } : this.load()
  }

  private day(): string {
    return new Date().toISOString().slice(0, 10)
  }
  private load(): Ledger {
    try {
      const l = JSON.parse(readFileSync(this.ledgerFile!, 'utf8')) as Ledger
      if (l.day !== this.day()) return { day: this.day(), spentSats: 0, lastBroadcastAt: l.lastBroadcastAt ?? 0, records: l.records ?? [] }
      return l
    } catch {
      return { day: this.day(), spentSats: 0, lastBroadcastAt: 0, records: [] }
    }
  }
  private save(): void {
    if (!this.ledgerFile) return
    mkdirSync(path.dirname(this.ledgerFile), { recursive: true })
    const tmp = this.ledgerFile + '.tmp'
    writeFileSync(tmp, JSON.stringify(this.ledger))
    renameSync(tmp, this.ledgerFile)
  }

  private refuse(reason: string): BroadcastResult {
    return { status: 'error', description: `SpendGuard refused: ${reason}` }
  }

  async broadcast(tx: Transaction): Promise<BroadcastResult> {
    const s = this.spendOf(tx)
    if (s === undefined) return this.refuse('unknown input value — fee unresolvable (fail-closed)')
    if (s.fee < 0) return this.refuse(`negative fee ${s.fee} — resolved input values are wrong (fail-closed)`)
    const now = Date.now()
    // 'unlimited' opts out of the DAILY cap only — fee and interval are
    // separate rails a soak still wants (a fat-fingered fee is catastrophic
    // at any daily budget), and dropping them would make the preflight
    // fee-probe unable to prove the path is guarded at all
    if (s.fee > this.opts.maxFeePerTx) return this.refuse(`fee ${s.fee} sats > maxFeePerTx ${this.opts.maxFeePerTx}`)
    if (!this.unlimited) {
      if (this.ledger.day !== this.day()) {
        this.ledger.day = this.day()
        this.ledger.spentSats = 0
      }
      if (this.ledger.spentSats + s.spend > this.opts.maxPerDay) return this.refuse(`daily cap: ${this.ledger.spentSats}+${s.spend} > ${this.opts.maxPerDay}`)
    }
    const wait = this.opts.minIntervalMs - (now - this.ledger.lastBroadcastAt)
    if (wait > 0) {
      // rate limit = delay, not failure (bounded by minIntervalMs); opt out → refuse
      if (this.refuseOnInterval) return this.refuse(`min interval ${this.opts.minIntervalMs}ms not elapsed`)
      await new Promise((r) => setTimeout(r, wait))
    }
    const res = await this.inner.broadcast(tx)
    if (res.status === 'success') {
      this.ledger.spentSats += s.spend
      this.ledger.lastBroadcastAt = Date.now()
      this.ledger.records.push({ txid: tx.id('hex'), bytes: tx.toBinary().length, fee: s.fee, spend: s.spend, at: new Date(now).toISOString() })
      this.save()
    }
    return res
  }

  /** P2PKH lock shape: 76 a9 14 <20B pkh> 88 ac (25 bytes). Covenant inputs
   *  are anything else — their spends return committed value, not new spend. */
  private spendOf(tx: Transaction): { fee: number; spend: number } | undefined {
    let vin = 0
    const inScripts = new Set<string>()
    let covenantInput = false
    for (const inp of tx.inputs) {
      const txid = inp.sourceTXID ?? inp.sourceTransaction?.id('hex')
      // tracked store first (our own spends); a foreign tx carrying its parent
      // (EF/BEEF) resolves from the attached source — the network validates
      // the real value at broadcast, the fee here is ledger bookkeeping only
      const srcOut = inp.sourceTransaction?.outputs[inp.sourceOutputIndex ?? 0]
      const v =
        txid === undefined
          ? undefined
          : (this.valueOf(txid, inp.sourceOutputIndex ?? 0) ?? srcOut?.satoshis)
      if (v === undefined || srcOut === undefined) return undefined
      vin += v
      const hex = srcOut.lockingScript.toHex()
      inScripts.add(hex)
      if (!/^76a914[0-9a-f]{40}88ac$/.test(hex)) covenantInput = true
    }
    let vout = 0
    let foreign = 0
    for (const o of tx.outputs) {
      vout += o.satoshis ?? 0
      if (!inScripts.has(o.lockingScript.toHex())) foreign += o.satoshis ?? 0
    }
    const fee = vin - vout
    // spend = the cap metric: fee plus value committed to foreign outputs.
    // Covenant-input txs (refund/claim/reveal) return committed value — fee
    // only, so a due refund still broadcasts at an exhausted cap.
    return { fee, spend: covenantInput ? fee : fee + foreign }
  }

  costs(): CostRecord[] {
    return [...this.ledger.records]
  }
  spentToday(): number {
    return this.ledger.day === this.day() ? this.ledger.spentSats : 0
  }
}
