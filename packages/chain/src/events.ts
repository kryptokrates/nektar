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
import { deadline, detached, nap, type DeadlineOutcome } from '@nektar/core'
import type { TxStatusEvent } from './interfaces.js'

const TERMINAL: ReadonlySet<string> = new Set(['mined', 'rejected', 'double-spent'])

export interface ChainEventsOpts {
  /** chain truth for one txid. Resolving `undefined` = asked a reachable
   *  source and it is definitively not terminal yet — the answer may feed
   *  the abandon clock. THROWING = couldn't ask (host down, rate-limited):
   *  a throw must never age silence into a verdict. */
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
  /** bound on one observe / height read — default 45s; a probe past it is
   *  'couldn't ask' (the same class as a throw): the dedup entry clears, the
   *  caller moves on, and the abandon clock never counts it. The substrate
   *  must not depend on every port's observe being well-behaved — one read
   *  that never settles must not starve every watch and waitFor behind it. */
  observeTimeoutMs?: number
  /** fired when an observe or height read exceeds observeTimeoutMs — the
   *  host's stall metric/log seam. Not fired for a throw (couldn't-ask the
   *  ordinary way) or an ambient abort (a shutdown, not a slowness). */
  onStall?: (what: 'observe' | 'height', ms: number, txid?: string) => void
}

interface WatchEntry {
  since: number
  /** first-seen tip height — filled lazily on the entry's first tick */
  atHeight?: number
  /** last tip where an observe ANSWERED "not terminal" — the abandon clock
   *  only counts definitive reads: a throwing observe never lets silence
   *  age into a verdict */
  answeredTip?: number
}

export class ChainEvents extends EventEmitter {
  private watched = new Map<string, WatchEntry>()
  /** retained statuses for statusOf/catch-up — timestamped and TTL-evicted on
   *  the same sweep as `watched` (retention is for catch-up, not growth).
   *  `verified` marks observed/delivered truth vs pushed hints: a hint record
   *  must never dedup-suppress the 'tx' emit of a later verified event. */
  private lastStatus = new Map<string, { ev: TxStatusEvent; at: number; verified: boolean }>()
  private waiters = new Map<string, Array<(ev: TxStatusEvent | undefined, terminal: boolean, verified: boolean) => void>>()
  /** in-flight observes by txid — the nudge loop and the tick coalesce onto
   *  one fetch instead of multiplying identical chain reads */
  private probing = new Map<string, Promise<TxStatusEvent | undefined>>()
  private tip: number | undefined
  private scannedAtTip: number | undefined
  private dirty = true
  private busy = false
  private rotate = 0
  private timer?: NodeJS.Timeout
  private running = false
  private pokeTimer?: NodeJS.Timeout
  private pokeAgain = false
  // push-vs-floor counters — the substrate's contract is "pokes deliver,
  // the floor only reconciles"; a floor-dominated ratio means producers
  // stopped waking us (a silent degrade the cadence itself can't feel)
  private pushTicks = 0
  private floorTicks = 0
  /** {push, floor} tick counts since start — floor should stay the
   *  exception; push ≈ 0 means every answer arrived on the poll ceiling */
  get wakeRatio(): { push: number; floor: number } {
    return { push: this.pushTicks, floor: this.floorTicks }
  }

  constructor(private opts: ChainEventsOpts) {
    super()
    // shared by design: every consumer (memory, confirmer, ledger, tests
    // with several memory instances on one chain) adds a 'tx' listener —
    // the slow tiers run dozens of engines on one substrate
    this.setMaxListeners(256)
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
   *  catching up without a fresh chain read. `verified` splits observed
   *  truth from pushed hints: a consumer that flips durable state must
   *  gate terminal claims on it — a hinted 'mined' is a wake, never proof. */
  statusOf(txid: string): { ev: TxStatusEvent; verified: boolean } | undefined {
    const r = this.lastStatus.get(txid)
    return r === undefined ? undefined : { ev: r.ev, verified: r.verified }
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
    this.resolve(ev, false, verified)
    // verify-on-hint: a pushed status (MINED from a status stream, a
    // broadcaster callback) is checked by our own observe on the next
    // macrotask — the 'tx' emit arrives in ms, not at the interval floor.
    // dirty re-opens the proof gate: under a static tip the batch would
    // otherwise be skipped and the hint would wait for the next block.
    this.dirty = true
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
   *  waitFor nudge and the tick sharing a txid get one fetch, not two. The
   *  read is BOUNDED: a port whose observe never settles used to pin this
   *  dedup entry (and busy, and every later probe's coalesce) forever; the
   *  deadline makes 'couldn't ask' a recoverable outcome instead of a silent
   *  substrate death. detached() because the shared probe must not inherit a
   *  caller's leg deadline — its own bound is the only clock it answers. */
  private probe(txid: string): Promise<TxStatusEvent | undefined> {
    const inflight = this.probing.get(txid)
    if (inflight !== undefined) return inflight
    const ms = this.opts.observeTimeoutMs ?? 45_000
    const p = detached(() => deadline(() => this.opts.observe(txid), ms))
      .then((r) => {
        this.probing.delete(txid)
        if (r.outcome === 'settled') {
          const ev = r.value
          if (ev !== undefined) {
            this.deliver(ev)
          } else {
            // a resolved undefined is a definitive "not terminal yet" — only
            // an answered read may feed the abandon clock; a timeout or a
            // throw is "couldn't ask" and must never advance it
            const w = this.watched.get(txid)
            if (w !== undefined && this.tip !== undefined) w.answeredTip = this.tip
          }
          return ev
        }
        // timeout/aborted — couldn't ask: probing cleared above, clock
        // untouched. A timeout names the slow port leg; an ambient abort is
        // a shutdown, not a slowness, and stays silent.
        if (r.outcome === 'timeout') this.opts.onStall?.('observe', ms, txid)
        return undefined
      })
      .catch(() => {
        this.probing.delete(txid)
        return undefined
      })
    this.probing.set(txid, p)
    return p
  }

  /** resolve with the txid's terminal event (or first delivered event) plus
   *  its verification bit — the same `{ev, verified}` split statusOf keeps:
   *  a pushed hint answers `verified:false`, only an observed/reported
   *  delivery carries true. undefined on timeout, abandon or TTL drop. */
  async waitFor(
    txid: string,
    timeoutMs = 30_000,
    match: (ev: TxStatusEvent) => boolean = () => true
  ): Promise<{ ev: TxStatusEvent; verified: boolean } | undefined> {
    const existing = this.lastStatus.get(txid)
    if (existing !== undefined && match(existing.ev)) return { ev: existing.ev, verified: existing.verified }
    this.watch(txid)
    const deadline = Date.now() + timeoutMs
    let done!: (ev: TxStatusEvent | undefined, terminal: boolean, verified: boolean) => void
    const pending = new Promise<{ ev: TxStatusEvent; verified: boolean } | undefined>((resolve) => {
      done = (ev, terminal, verified) => {
        // a non-matching event parks the waiter only while the txid can still
        // move — a terminal event ends the wait for EVERY waiter on it,
        // matcher or not, and must answer them rather than stranding them
        // on their own clock
        if (ev !== undefined && !match(ev) && !terminal) return
        this.waiters.set(
          txid,
          (this.waiters.get(txid) ?? []).filter((w) => w !== done)
        )
        resolve(ev === undefined ? undefined : { ev, verified })
      }
      this.waiters.set(txid, [...(this.waiters.get(txid) ?? []), done])
    })
    // nudge: the waiter's arrival is itself a probe-worthy event — poke now,
    // then re-probe on a pace proportional to the shared tick (never hotter
    // than a quarter of it; floor 150ms, ceiling 5s) so a synchronous caller
    // doesn't stall a whole interval without hammering a live status API.
    let settled = false as boolean
    const nudgeAc = new AbortController()
    const nudgeMs = Math.max(150, Math.min((this.opts.intervalMs ?? 5_000) / 4, timeoutMs / 4, 5_000))
    const nudge = (async () => {
      while (!settled && Date.now() < deadline && this.watched.has(txid)) {
        await this.probeNow(txid)
        if (nudgeAc.signal.aborted) break
        await nap(nudgeMs, { signal: nudgeAc.signal })
      }
    })()
    let timeoutTimer: ReturnType<typeof setTimeout> | undefined
    const timeout = new Promise<undefined>((r) => {
      timeoutTimer = setTimeout(() => r(undefined), timeoutMs)
      timeoutTimer.unref?.()
    })
    const ev = await Promise.race([pending, timeout])
    settled = true
    nudgeAc.abort()
    // a wait resolved early (delivery or stop()) must not leave its timer
    // holding the process for the remaining timeoutMs
    if (timeoutTimer !== undefined) clearTimeout(timeoutTimer)
    if (ev === undefined) done(undefined, true, false) // timeout: drop the waiter, don't leak it
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
      this.pushTicks++
      void this.tick().catch(() => undefined)
    }, 0)
    this.pokeTimer.unref?.()
  }

  start(): void {
    if (this.running) return
    this.running = true
    const arm = (): void => {
      const base = this.opts.intervalMs ?? 5_000
      // self-arming tick, ±10% per-tick skew: setInterval would keep a
      // fleet-restarted set of hosts phase-locked on the status API
      // forever — the jitter drifts them back apart on every pass
      this.timer = setTimeout(
        () => {
          this.timer = undefined
          this.floorTicks++
          void this.tick()
            .catch(() => undefined)
            .finally(() => {
              if (this.running) arm()
            })
        },
        base * (0.9 + Math.random() * 0.2)
      )
      this.timer.unref?.()
    }
    arm()
  }

  stop(): void {
    this.running = false
    if (this.timer !== undefined) clearTimeout(this.timer)
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
      // a FAILED height read is not "tip unchanged" — couldn't-ask must not
      // satisfy the proof-gate, or the observe batch starves on the stale
      // tip while observe itself would have answered. Bounded like probe():
      // a height read that never settles is the same failed-read class.
      let heightKnown = true
      if (this.opts.height !== undefined) {
        const ms = this.opts.observeTimeoutMs ?? 45_000
        const r = await detached(() => deadline(() => this.opts.height!(), ms)).catch((): DeadlineOutcome<number> => ({ outcome: 'aborted' }))
        const tip = r.outcome === 'settled' ? r.value : undefined
        heightKnown = tip !== undefined
        if (r.outcome === 'timeout') this.opts.onStall?.('height', ms)
        if (tip !== undefined && tip !== this.tip) {
          this.tip = tip
          this.emit('height', tip)
        }
      }
      // ttl sweep + first-seen heights; retained statuses age out on the
      // same window (catch-up cache, not permanent memory)
      const staleAt = Date.now() - (this.opts.ttlMs ?? 2 * 60 * 60_000)
      for (const [txid, w] of this.watched) {
        if (w.since < staleAt) {
          this.watched.delete(txid)
          // a watch that ends without a terminal answer is still an ending:
          // the abandoned signal + a waiter flush — a failure path that
          // leaves waiters parked on their own timeout is a lost wakeup
          this.emit('abandoned', { txid, tip: this.tip, seenAtHeight: w.atHeight })
          this.end(txid)
          continue
        }
        if (w.atHeight === undefined) w.atHeight = this.tip
      }
      for (const [txid, s] of this.lastStatus) {
        if (s.at < staleAt) this.lastStatus.delete(txid)
      }
      // proof-gating: a static tip and no new watches make every observe a
      // guaranteed miss — skip the batch like the old proofScanTip gate did
      const gated = this.opts.height !== undefined && heightKnown && this.tip === this.scannedAtTip && !this.dirty
      if (!gated && this.watched.size > 0) {
        const keys = [...this.watched.keys()]
        const n = Math.min(this.opts.maxPerTick ?? 8, keys.length)
        const batch = Array.from({ length: n }, (_, i) => keys[(this.rotate + i) % keys.length])
        this.rotate = keys.length > 0 ? (this.rotate + n) % keys.length : 0
        await Promise.all(batch.map((txid) => this.probeOne(txid)))
        this.scannedAtTip = this.tip
        this.dirty = false
      }
      // abandonment is evaluated AFTER the batch so a probe that just
      // answered lands in the same sweep — and only answered reads move the
      // clock: an unreachable source must never become a money-state verdict
      if (this.opts.abandonAfterBlocks !== undefined) {
        for (const [txid, w] of this.watched) {
          if (w.answeredTip !== undefined && w.atHeight !== undefined && w.answeredTip - w.atHeight > this.opts.abandonAfterBlocks) {
            this.watched.delete(txid)
            this.emit('abandoned', { txid, tip: this.tip, seenAtHeight: w.atHeight })
            this.end(txid)
          }
        }
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
    this.resolve(ev, TERMINAL.has(ev.status), true)
  }

  /** the watch on `txid` ended without a terminal event (abandon, TTL drop):
   *  answer its waiters with undefined — the same answer a timeout gives —
   *  instead of leaving them parked on a watch that no longer exists */
  private end(txid: string): void {
    for (const w of this.waiters.get(txid) ?? []) w(undefined, true, false)
    this.waiters.delete(txid)
  }

  private resolve(ev: TxStatusEvent | undefined, terminal = true, verified = false): void {
    if (ev !== undefined) {
      // every waiter on the txid hears each delivered event: matching ones
      // resolve; non-matching ones stay parked while the txid can still move —
      // a terminal event ends the wait for EVERY waiter, matcher or not
      for (const w of this.waiters.get(ev.txid) ?? []) w(ev, terminal, verified)
      if (terminal) this.waiters.delete(ev.txid)
      return
    }
    for (const ws of this.waiters.values()) for (const w of ws) w(undefined, true, false)
    this.waiters.clear()
  }
}
