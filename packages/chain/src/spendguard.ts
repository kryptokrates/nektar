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
    const fee = this.feeOf(tx)
    if (fee === undefined) return this.refuse('unknown input value — fee unresolvable (fail-closed)')
    if (fee < 0) return this.refuse(`negative fee ${fee} — resolved input values are wrong (fail-closed)`)
    const now = Date.now()
    if (!this.unlimited) {
      if (fee > this.opts.maxFeePerTx) return this.refuse(`fee ${fee} sats > maxFeePerTx ${this.opts.maxFeePerTx}`)
      if (this.ledger.day !== this.day()) {
        this.ledger.day = this.day()
        this.ledger.spentSats = 0
      }
      if (this.ledger.spentSats + fee > this.opts.maxPerDay) return this.refuse(`daily cap: ${this.ledger.spentSats}+${fee} > ${this.opts.maxPerDay}`)
      const wait = this.opts.minIntervalMs - (now - this.ledger.lastBroadcastAt)
      if (wait > 0) {
        // rate limit = delay, not failure (bounded by minIntervalMs); opt out → refuse
        if (this.refuseOnInterval) return this.refuse(`min interval ${this.opts.minIntervalMs}ms not elapsed`)
        await new Promise((r) => setTimeout(r, wait))
      }
    }
    const res = await this.inner.broadcast(tx)
    if (res.status === 'success') {
      this.ledger.spentSats += fee
      this.ledger.lastBroadcastAt = Date.now()
      this.ledger.records.push({ txid: tx.id('hex'), bytes: tx.toBinary().length, fee, at: new Date(now).toISOString() })
      this.save()
    }
    return res
  }

  private feeOf(tx: Transaction): number | undefined {
    let vin = 0
    for (const inp of tx.inputs) {
      const txid = inp.sourceTXID ?? inp.sourceTransaction?.id('hex')
      // tracked store first (our own spends); a foreign tx carrying its parent
      // (EF/BEEF) resolves from the attached source — the network validates
      // the real value at broadcast, the fee here is ledger bookkeeping only
      const v =
        txid === undefined
          ? undefined
          : (this.valueOf(txid, inp.sourceOutputIndex ?? 0) ?? inp.sourceTransaction?.outputs[inp.sourceOutputIndex ?? 0]?.satoshis)
      if (v === undefined) return undefined
      vin += v
    }
    let vout = 0
    for (const o of tx.outputs) vout += o.satoshis ?? 0
    return vin - vout
  }

  costs(): CostRecord[] {
    return [...this.ledger.records]
  }
  spentToday(): number {
    return this.ledger.day === this.day() ? this.ledger.spentSats : 0
  }
}
