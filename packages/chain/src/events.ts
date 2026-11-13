// ChainEvents — one interest-set + one poller per process for "is it mined?"
// questions. Replaces the per-consumer polling loops (confirmer proof scans,
// watch lists, proof waits) that each used to carry their own timer, their
// own re-entrancy guard and their own hung-fetch failure mode.
//
// Trust model: `observe` (our own chain reads — asset API, RPC, chaintracks)
// produces events verified by construction and they emit 'tx'. Pushed
// broadcaster statuses are hints only — `note()` records them for waiters and
// dedup but never emits 'tx' and never ends interest: only a verified
// observe (deliver) drops a watch, so a push that can't be verified keeps
// its reconcile floor (the verified delivery path stays with the caller —
// handleBroadcasterStatus verifies BUMPs against our own tracker).
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
  /** retained statuses for statusOf/catch-up — timestamped and TTL-evicted on
   *  the same sweep as `watched` (retention is for catch-up, not growth).
   *  `verified` marks observed/delivered truth vs pushed hints: a hint record
   *  must never dedup-suppress the 'tx' emit of a later verified event. */
  private lastStatus = new Map<string, { ev: TxStatusEvent; at: number; verified: boolean }>()
  private waiters = new Map<string, Array<(ev: TxStatusEvent | undefined, terminal: boolean) => void>>()
  /** in-flight observes by txid — the nudge loop and the tick coalesce onto
   *  one fetch instead of multiplying identical chain reads */
  private probing = new Map<string, Promise<TxStatusEvent | undefined>>()
  private tip: number | undefined
  private scannedAtTip: number | undefined
  private dirty = true
  private busy = false
  private rotate = 0
  private timer?: NodeJS.Timeout
  private pokeTimer?: NodeJS.Timeout
  private pokeAgain = false

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
      // new interest is itself an event — the first observe happens on the
      // next macrotask, not at the interval floor
      this.poke()
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
    return this.lastStatus.get(txid)?.ev
  }

  /** an externally pushed status (SSE, webhook, broadcast response): records
   *  the status for statusOf/waitFor and wakes waiters — a hint, never truth:
   *  it never emits 'tx' and it never ends a watch. Interest only drops on a
   *  verified observe (deliver), so a push that can't be verified — a MINED
   *  status whose BUMP didn't check out — keeps the poller's reconcile floor
   *  alive instead of stranding the wait. */
  note(ev: TxStatusEvent): void {
    const prev = this.lastStatus.get(ev.txid)
    // a pushed hint never downgrades a verified record for the same status —
    // otherwise hint-verified-hint would re-emit a duplicate 'tx'
    const verified = prev?.verified === true && prev.ev.status === ev.status
    this.lastStatus.set(ev.txid, { ev, at: Date.now(), verified })
    this.resolve(ev, TERMINAL.has(ev.status))
    // verify-on-hint: a pushed status (MINED from a status stream, a
    // broadcaster callback) is checked by our own observe on the next
    // macrotask — the 'tx' emit arrives in ms, not at the interval floor
    this.poke()
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
    return await this.probe(txid)
  }

  /** one observed read for `txid`, dedup'd across concurrent callers — a
   *  waitFor nudge and the tick sharing a txid get one fetch, not two. */
  private probe(txid: string): Promise<TxStatusEvent | undefined> {
    const inflight = this.probing.get(txid)
    if (inflight !== undefined) return inflight
    const p = this.opts
      .observe(txid)
      .catch(() => undefined)
      .then((ev) => {
        this.probing.delete(txid)
        if (ev !== undefined) this.deliver(ev)
        return ev
      })
    this.probing.set(txid, p)
    return p
  }

  /** resolve with the txid's terminal event (or first delivered event), or
   *  undefined on timeout. */
  async waitFor(txid: string, timeoutMs = 30_000, match: (ev: TxStatusEvent) => boolean = () => true): Promise<TxStatusEvent | undefined> {
    const existing = this.lastStatus.get(txid)?.ev
    if (existing !== undefined && match(existing)) return existing
    this.watch(txid)
    const deadline = Date.now() + timeoutMs
    let done!: (ev: TxStatusEvent | undefined, terminal: boolean) => void
    const pending = new Promise<TxStatusEvent | undefined>((resolve) => {
      done = (ev, terminal) => {
        // a non-matching event parks the waiter only while the txid can still
        // move — a terminal event ends the wait for EVERY waiter on it,
        // matcher or not, and must answer them rather than stranding them
        // on their own clock
        if (ev !== undefined && !match(ev) && !terminal) return
        this.waiters.set(
          txid,
          (this.waiters.get(txid) ?? []).filter((w) => w !== done)
        )
        resolve(ev)
      }
      this.waiters.set(txid, [...(this.waiters.get(txid) ?? []), done])
    })
    // nudge: the waiter's arrival is itself a probe-worthy event — poke now,
    // then re-probe on a pace proportional to the shared tick (never hotter
    // than a quarter of it; floor 150ms, ceiling 5s) so a synchronous caller
    // doesn't stall a whole interval without hammering a live status API.
    let settled = false
    const nudgeMs = Math.max(150, Math.min((this.opts.intervalMs ?? 5_000) / 4, timeoutMs / 4, 5_000))
    const nudge = (async () => {
      while (!settled && Date.now() < deadline && this.watched.has(txid)) {
        await this.probeNow(txid)
        await new Promise((r) => setTimeout(r, nudgeMs))
      }
    })()
    const timeout = new Promise<undefined>((r) => setTimeout(() => r(undefined), timeoutMs))
    const ev = await Promise.race([pending, timeout])
    settled = true
    if (ev === undefined) done(undefined, true) // timeout: drop the waiter, don't leak it
    void nudge.catch(() => undefined)
    return ev
  }

  /** schedule a tick on the next macrotask — producers (watch, note, pushed
   *  statuses, post-mine) call this so the interval stays the reconcile
   *  floor, never the delivery path. Coalesced: N pokes before the flush
   *  run one tick. A poke during a running tick re-queues once so wakes are
   *  lossless, while interval ticks during busy still drop — wakes shorten
   *  latency, they never multiply overlapping sweeps. */
  poke(): void {
    if (this.busy) {
      this.pokeAgain = true
      return
    }
    if (this.pokeTimer !== undefined) return
    this.pokeTimer = setTimeout(() => {
      this.pokeTimer = undefined
      void this.tick().catch(() => undefined)
    }, 0)
    this.pokeTimer.unref?.()
  }

  start(): void {
    this.timer ??= setInterval(() => void this.tick().catch(() => undefined), this.opts.intervalMs ?? 5_000)
    this.timer.unref?.()
  }

  stop(): void {
    if (this.timer !== undefined) clearInterval(this.timer)
    this.timer = undefined
    if (this.pokeTimer !== undefined) clearTimeout(this.pokeTimer)
    this.pokeTimer = undefined
    this.pokeAgain = false
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
      // ttl sweep + first-seen heights + abandonment; retained statuses age
      // out on the same window (catch-up cache, not permanent memory)
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
      for (const [txid, s] of this.lastStatus) {
        if (s.at < staleAt) this.lastStatus.delete(txid)
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
      // a wake that landed mid-sweep re-runs once — the new watch/hint is
      // observed immediately after, not an interval later
      if (this.pokeAgain) {
        this.pokeAgain = false
        this.poke()
      }
    }
  }

  private async probeOne(txid: string): Promise<void> {
    await this.probe(txid)
  }

  private deliver(ev: TxStatusEvent): void {
    const prev = this.lastStatus.get(ev.txid)
    this.lastStatus.set(ev.txid, { ev, at: Date.now(), verified: true })
    // re-emit only on change — mined→mined duplicates are suppressed, but a
    // hint record (verified:false) never suppresses the first verified emit
    if (prev === undefined || !prev.verified || prev.ev.status !== ev.status || !!prev.ev.merklePath !== !!ev.merklePath) {
      this.emit('tx', ev)
    }
    if (TERMINAL.has(ev.status)) this.watched.delete(ev.txid)
    this.resolve(ev, TERMINAL.has(ev.status))
  }

  private resolve(ev: TxStatusEvent | undefined, terminal = true): void {
    if (ev !== undefined) {
      // every waiter on the txid hears each delivered event: matching ones
      // resolve; non-matching ones stay parked while the txid can still move —
      // a terminal event ends the wait for EVERY waiter, matcher or not
      for (const w of this.waiters.get(ev.txid) ?? []) w(ev, terminal)
      if (terminal) this.waiters.delete(ev.txid)
      return
    }
    for (const ws of this.waiters.values()) for (const w of ws) w(undefined, true)
    this.waiters.clear()
  }
}
