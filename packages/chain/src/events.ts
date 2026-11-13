// ChainEvents — one interest-set + one poller per process for "is it mined?"
// questions. Replaces the per-consumer polling loops (confirmer proof scans,
// watch lists, proof waits) that each used to carry their own timer, their
// own re-entrancy guard and their own hung-fetch failure mode.
//
// Trust model: `observe` (our own chain reads — asset API, RPC, chaintracks)
// produces events verified by construction and they emit 'tx'. Pushed
// broadcaster statuses are hints only — `note()` records them for waiters and
// dedup but never emits 'tx'; the verified delivery path stays with the
// caller (handleBroadcasterStatus verifies BUMPs against our own tracker).
import { EventEmitter } from 'node:events'
import type { TxStatusEvent } from './interfaces.js'

const TERMINAL: ReadonlySet<string> = new Set(['mined', 'rejected', 'double-spent'])

export interface ChainEventsOpts {
  /** chain truth for one txid. undefined → still pending; never throws. */
  observe: (txid: string) => Promise<TxStatusEvent | undefined>
  /** tip probe — enables 'height' events and proof-gating: when the tip has
   *  not advanced and no new watches arrived, a tick skips the txid batch
   *  entirely (no new block ⇒ no new proofs possible). */
  height?: () => Promise<number>
  /** poll interval. default 5s; testkit uses 500ms. */
  intervalMs?: number
  /** watched entries older than this are dropped — a tx that never seals
   *  must not pin the poll (and its keep-alive sockets) forever. */
  ttlMs?: number
  /** max txids observed per tick — oversized interest sets rotate. */
  maxPerTick?: number
  /** requires `height`: an entry watched for N+ blocks without a terminal
   *  status emits 'abandoned' once and is dropped (conflicting-spend case). */
  abandonAfterBlocks?: number
}

interface WatchEntry {
  since: number
  /** first-seen tip height — filled lazily on the entry's first tick */
  atHeight?: number
}

export class ChainEvents extends EventEmitter {
  private watched = new Map<string, WatchEntry>()
  private lastStatus = new Map<string, TxStatusEvent>()
  private waiters = new Map<string, Array<(ev: TxStatusEvent | undefined) => void>>()
  private tip: number | undefined
  private scannedAtTip: number | undefined
  private dirty = true
  private busy = false
  private rotate = 0
  private timer?: NodeJS.Timeout

  constructor(private opts: ChainEventsOpts) {
    super()
    // shared by design: every consumer (memory, confirmer, ledger, tests
    // with several memory instances on one chain) adds a 'tx' listener
    this.setMaxListeners(64)
  }

  /** register interest — idempotent; a txid already at a terminal status
   *  resolves a subsequent waitFor from the retained status. */
  watch(txid: string, atHeight?: number): void {
    if (!this.watched.has(txid)) {
      this.watched.set(txid, { since: Date.now(), atHeight })
      this.dirty = true
    }
  }

  unwatch(txid: string): void {
    this.watched.delete(txid)
  }

  watching(txid: string): boolean {
    return this.watched.has(txid)
  }

  /** interest-set size — mine()/confirm() nudges loop until this drains */
  get pending(): number {
    return this.watched.size
  }

  /** last status this instance delivered or was handed — for consumers
   *  catching up without a fresh chain read. */
  statusOf(txid: string): TxStatusEvent | undefined {
    return this.lastStatus.get(txid)
  }

  /** an externally pushed status (SSE, webhook, broadcast response): records
   *  the status for statusOf/waitFor, drops the watch on terminal states.
   *  Never emits 'tx' — pushed statuses are unverified hints; attachment is
   *  the caller's verified path. */
  note(ev: TxStatusEvent): void {
    this.lastStatus.set(ev.txid, ev)
    if (TERMINAL.has(ev.status)) {
      this.watched.delete(ev.txid)
      this.resolve(ev)
    }
  }

  /** a first-party observation (e.g. a definitive broadcast refusal): emits
   *  'tx' through the same dedup/delivery path as a polled observation. */
  report(ev: TxStatusEvent): void {
    this.deliver(ev)
  }

  /** observe one txid immediately, outside the tick schedule — the waitFor
   *  and post-mine nudge path. Emits 'tx' on a result. */
  async probeNow(txid: string): Promise<TxStatusEvent | undefined> {
    this.watch(txid)
    const ev = await this.opts.observe(txid).catch(() => undefined)
    if (ev !== undefined) this.deliver(ev)
    return ev
  }

  /** resolve with the txid's terminal event (or first delivered event), or
   *  undefined on timeout. */
  async waitFor(txid: string, timeoutMs = 30_000, match: (ev: TxStatusEvent) => boolean = () => true): Promise<TxStatusEvent | undefined> {
    const existing = this.lastStatus.get(txid)
    if (existing !== undefined && match(existing)) return existing
    this.watch(txid)
    const deadline = Date.now() + timeoutMs
    let done!: (ev: TxStatusEvent | undefined) => void
    const pending = new Promise<TxStatusEvent | undefined>((resolve) => {
      done = (ev: TxStatusEvent | undefined) => {
        if (ev !== undefined && !match(ev)) return
        this.waiters.set(
          txid,
          (this.waiters.get(txid) ?? []).filter((w) => w !== done)
        )
        resolve(ev)
      }
      this.waiters.set(txid, [...(this.waiters.get(txid) ?? []), done])
    })
    // nudge: poll this txid on a short loop alongside the shared tick so a
    // caller waiting synchronously does not stall for the next interval
    const nudge = (async () => {
      while (Date.now() < deadline && this.watched.has(txid)) {
        await this.probeNow(txid)
        await new Promise((r) => setTimeout(r, 150))
      }
    })()
    const timeout = new Promise<undefined>((r) => setTimeout(() => r(undefined), timeoutMs))
    const ev = await Promise.race([pending, timeout])
    if (ev === undefined) done(undefined) // timeout: drop the waiter, don't leak it
    void nudge.catch(() => undefined)
    return ev
  }

  start(): void {
    this.timer ??= setInterval(() => void this.tick().catch(() => undefined), this.opts.intervalMs ?? 5_000)
    this.timer.unref?.()
  }

  stop(): void {
    if (this.timer !== undefined) clearInterval(this.timer)
    this.timer = undefined
    this.watched.clear()
    this.resolve(undefined)
  }

  /** the single sweep — re-entrant-safe, shared by every consumer. */
  async tick(): Promise<void> {
    if (this.busy) return
    this.busy = true
    try {
      if (this.opts.height !== undefined) {
        const tip = await this.opts.height().catch(() => undefined)
        if (tip !== undefined && tip !== this.tip) {
          this.tip = tip
          this.emit('height', tip)
        }
      }
      // ttl sweep + first-seen heights + abandonment
      const staleAt = Date.now() - (this.opts.ttlMs ?? 2 * 60 * 60_000)
      for (const [txid, w] of this.watched) {
        if (w.since < staleAt) {
          this.watched.delete(txid)
          continue
        }
        if (w.atHeight === undefined) w.atHeight = this.tip
        if (
          this.opts.abandonAfterBlocks !== undefined &&
          this.tip !== undefined &&
          w.atHeight !== undefined &&
          this.tip - w.atHeight > this.opts.abandonAfterBlocks
        ) {
          this.watched.delete(txid)
          this.emit('abandoned', { txid, tip: this.tip, seenAtHeight: w.atHeight })
        }
      }
      // proof-gating: a static tip and no new watches make every observe a
      // guaranteed miss — skip the batch like the old proofScanTip gate did
      const gated = this.opts.height !== undefined && this.tip === this.scannedAtTip && !this.dirty
      if (!gated && this.watched.size > 0) {
        const keys = [...this.watched.keys()]
        const n = Math.min(this.opts.maxPerTick ?? 8, keys.length)
        const batch = Array.from({ length: n }, (_, i) => keys[(this.rotate + i) % keys.length])
        this.rotate = keys.length > 0 ? (this.rotate + n) % keys.length : 0
        await Promise.all(batch.map((txid) => this.probeOne(txid)))
        this.scannedAtTip = this.tip
        this.dirty = false
      }
      this.emit('tick')
    } finally {
      this.busy = false
    }
  }

  private async probeOne(txid: string): Promise<void> {
    const ev = await this.opts.observe(txid).catch(() => undefined)
    if (ev !== undefined) this.deliver(ev)
  }

  private deliver(ev: TxStatusEvent): void {
    const prev = this.lastStatus.get(ev.txid)
    this.lastStatus.set(ev.txid, ev)
    // re-emit only on change — mined→mined duplicates are suppressed
    if (prev?.status !== ev.status || !!prev?.merklePath !== !!ev.merklePath) {
      this.emit('tx', ev)
    }
    if (TERMINAL.has(ev.status)) {
      this.watched.delete(ev.txid)
      this.resolve(ev)
    }
  }

  private resolve(ev: TxStatusEvent | undefined): void {
    if (ev !== undefined) {
      for (const w of this.waiters.get(ev.txid) ?? []) w(ev)
      this.waiters.delete(ev.txid)
      return
    }
    for (const ws of this.waiters.values()) for (const w of ws) w(undefined)
    this.waiters.clear()
  }
}
