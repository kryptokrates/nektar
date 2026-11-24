// waits.ts — the waits registry: every wait-site duration in the
// automation runtime is a named value here, the same enumerable-pattern
// the repo already runs for causes (shadow census), machine edges
// (*.table.json), scheduler jobs + interval floors (wake-coverage) and
// oracle writes (check:arch). A wait used to be smeared across a timer
// primitive + a literal + a comparison site; naming it makes its class
// reviewable — and the census test (test/waits-census.test.ts) makes a
// new anonymous literal a failure, not a drift.
//
// class semantics:
//   ceiling   — a correctness floor: events drive, this only catches a
//               missed wake (sloppy is fine)
//   ttl       — evidence lifetime: a mark/cache dies when it ages out
//   mechanism — the wait IS the trigger (a cadence, not a backstop):
//               flagged — needs `why` stating the reason it can't ride
//               a producer signal
//   backoff   — a retry delay bound (base or cap); pair with spread()
//               jitter so N processes don't phase-align
//   budget    — a bound on work/wait cost: bound() legs, hold limits,
//               report thresholds
//   io        — a per-call transport bound (fetch/exec timeout): not a
//               wait site — a dead-man's switch on one call; registered
//               anyway so the census sees every duration literal
import { REFUSED_MARK_MS } from './refusals.js'

export interface WaitSpec {
  ms: number
  class: 'ceiling' | 'ttl' | 'mechanism' | 'backoff' | 'budget' | 'io'
  /** survives a restart? (persisted column vs in-memory — a declaration,
   *  not a judgment; both are legal, neither may be accidental) */
  durable: boolean
  /** the producer signal that fires it early, or 'floor-only'/'probe' */
  wake: string
  /** required when class === 'mechanism' — why the wait itself is the
   *  trigger rather than an event wake */
  why?: string
}

export const WAITS = {
  // ── pass/leg budgets ────────────────────────────────────────────────
  /** one deals-pass leg (poll/tick/wtx) — rides the ambient leg context
   *  (deadline() publishes it on AsyncLocalStorage; inner IO merges it via
   *  ioSignal()/nap()/currentSignal() — no field to thread or forget) */
  dealsBound: { ms: 120_000, class: 'budget', durable: false, wake: 'deadline-abort' },
  /** the whole upkeep pass — ambient leg context, same as dealsBound */
  upkeepBound: { ms: 300_000, class: 'budget', durable: false, wake: 'deadline-abort' },
  /** stop(): bounded inflight-settle before teardown proceeds */
  stopSettle: { ms: 15_000, class: 'budget', durable: false, wake: 'inflight settle' },
  /** stop(): bounded drain of busy/upkeep/ack/pump flags */
  stopDrain: { ms: 30_000, class: 'budget', durable: false, wake: 'flag clear' },
  /** the drain loop's re-check tick — a poll inside a bounded teardown */
  drainPoll: {
    ms: 25,
    class: 'mechanism',
    durable: false,
    wake: 'flag clear',
    why: 'the bound (stopDrain) owns the exit; the tick is only a re-check interval inside it'
  },
  /** report a pass slower than this — diagnostic bound, not a wait */
  passReport: { ms: 2_000, class: 'budget', durable: false, wake: 'floor-only' },
  /** wakeStats summary-log cadence — the runtime measurement of which
   *  source (push vs floor) drove each pass */
  wakeReport: { ms: 60_000, class: 'budget', durable: false, wake: 'floor-only' },
  /** the serialized-work mutex's held-too-long threshold (base.ts) */
  runHold: {
    ms: 60_000,
    class: 'budget',
    durable: false,
    wake: 'link settle',
    why: 'detect-only by design: custody serialization must not drop a wedged link'
  },
  /** one whole deals pass (poll+tick+wtx share it) — a leg outliving the
   *  shared budget abandons so a hung poll can't eat 3× leg bound */
  passBound: { ms: 180_000, class: 'budget', durable: false, wake: 'deadline-abort' },

  // ── push-path evidence windows ──────────────────────────────────────
  /** how long recent work keeps the deals loop hot — evidence TTL, and
   *  the pump returns to idle cadence when it decays */
  hotWindow: { ms: 5_000, class: 'ttl', durable: false, wake: 'any work item' },
  /** ledger-sweep throttle — a floor the push path clears (-Infinity) */
  ledgerSweep: { ms: 15_000, class: 'ceiling', durable: false, wake: 'push verdict → sweep now' },

  // ── periodic floors (backstops under event wakes) ──────────────────
  /** watches job re-arm — the watch list derives from live interest;
   *  a post-pass refresh is the wake, this is the missed-wake floor */
  watchRearm: { ms: 15_000, class: 'ceiling', durable: false, wake: 'post-pass refreshWatches + changed:self' },
  /** upkeep pass cadence — the upkeep object's journaled self-arm under
   *  restate, the local executor's delay timer in dev/test mode */
  upkeepCadence: { ms: 10_000, class: 'ceiling', durable: false, wake: 'cycle() push path + self-arm' },
  /** upkeep tree-mint starvation backoff base (2^n × this, spread()-jittered) */
  upkeepStarveBase: { ms: 30_000, class: 'backoff', durable: false, wake: 'funds.adopted / txProof' },
  /** upkeep tree-mint starvation backoff cap */
  upkeepStarveCap: { ms: 300_000, class: 'backoff', durable: false, wake: 'funds.adopted / txProof' },
  /** watchdog probe cadence — the cheap tier only; the full preflight
   *  stays operator-driven (automationDoctor) */
  watchdogCadence: { ms: 60_000, class: 'ceiling', durable: false, wake: 'self-arm / local executor delay' },
  /** inbox-repair defer — the real wake is unblockOutbox on the freeing
   *  event; the re-check floor just covers a missed pull-forward */
  deferCheck: { ms: 2_000, class: 'ceiling', durable: true, wake: 'unblockOutbox / pullOutbox' },
  /** in-run resubmit cadence (ingress tick) — prepared/unknown rows are
   *  re-driven at most this often */
  resubmitCadence: {
    ms: 60_000,
    class: 'mechanism',
    durable: false,
    wake: 'tick-pass',
    why: 'the cadence IS the heal mechanism — a rate limit on replay writes, not a wake proxy'
  },
  /** deterministic per-agent spread on the resubmit cadence — restarts
   *  would otherwise phase-align every agent's heal pass to one tick */
  resubmitJitter: { ms: 30_000, class: 'backoff', durable: false, wake: 'tick-pass' },
  /** dealfan event re-fan bound — a pending inbox row is re-sent at most
   *  this often; under restate each send is a durable queued invocation so
   *  per-tick re-fan piles identical legs onto the key's FIFO and starves
   *  new work. The window doubles as the recovery bound for sends whose
   *  invocation completed without resolving the row (killed/deaf want gate) */
  fanEventResend: {
    ms: 10_000,
    class: 'mechanism',
    durable: false,
    wake: 'tick-pass',
    why: 'a rate limit on identical durable sends — dedup cannot be an idempotency key (gated wants must re-fire after the flap ends)'
  },
  /** dealfan advance/reconcile re-fan bound — one send per live op per
   *  window; early-select stays inside collectMs while the key's queue
   *  drains instead of accumulating one advance per tick per op */
  fanAdvanceResend: {
    ms: 5_000,
    class: 'mechanism',
    durable: false,
    wake: 'tick-pass',
    why: 'a rate limit on identical durable sends — advance is a poke, not distinct work; the armed due send carries the exact deadline'
  },
  /** terminal-row reconcile probe cooldown (base.ts reconcileGate) —
   *  a rate limit on indexer probes, not a wake proxy; persists via
   *  markReconciled so restarts don't reset the probe rate */
  reconcileGate: {
    ms: 30_000,
    class: 'mechanism',
    durable: true,
    wake: 'gate expiry',
    why: 'the cooldown IS the rate limit — terminal ops re-probe the indexer at most this often; faster would hammer the host for a row that only a reorg revives'
  },
  /** deterministic per-op spread on the reconcile cooldown (stable01) */
  reconcileJitter: { ms: 30_000, class: 'backoff', durable: true, wake: 'gate expiry' },

  // ── caches / marks (evidence TTLs) ──────────────────────────────────
  /** txFacts throttle for the mint-gate heal/reap read — ChainEvents
   *  polls these txids itself, so this cache loses no signal */
  statusCache: { ms: 15_000, class: 'ttl', durable: false, wake: 'ChainEvents own floor' },
  /** finger-record negative cache — a missing record re-probes hourly
   *  only via fresh=true; cached 'absent' answers live this long */
  fingerNeg: { ms: 60_000, class: 'ttl', durable: false, wake: 'fresh=true re-probe' },
  /** finger-record positive cache */
  fingerPos: { ms: 600_000, class: 'ttl', durable: false, wake: 'finger publish / fresh=true' },
  /** exported-tx tombstone retention (ledger opt default) */
  exportedTtl: { ms: 24 * 60 * 60_000, class: 'ttl', durable: true, wake: 'export expiry sweep' },
  /** refused-ancestor mark epoch — a 'conflicting' verdict dies when a
   *  reorg could flip it; alias of refusals.ts REFUSED_MARK_MS */
  refusalMark: { ms: REFUSED_MARK_MS, class: 'ttl', durable: false, wake: 'next refusal evidence' },

  // ── mechanisms (the wait IS the trigger — flagged, needs why) ───────
  /** stuck-mint heal attempts — one resubmit per window per mint */
  mintHeal: {
    ms: 10 * 60_000,
    class: 'mechanism',
    durable: false,
    wake: 'upkeep-pass',
    why: 'deliberate rate limit on resubmits — a fast re-fire would spam the broadcaster; two definitive refusals still reap early'
  },
  /** journal/outbox/tx-event retention sweep cadence */
  pruneInterval: {
    ms: 86_400_000,
    class: 'mechanism',
    durable: false,
    wake: 'upkeep-pass',
    why: 'a daily cron by design — deletes are IO the hot loop never needs; a per-pass sweep would churn the WAL for nothing'
  },
  /** the day unit retention math multiplies */
  day: { ms: 86_400_000, class: 'ttl', durable: true, wake: 'floor-only' },

  // ── retry cadences ──────────────────────────────────────────────────
  /** outbox dead-letter backoff base (2^attempts × this, spread()-jittered) */
  outboxRetryBase: { ms: 1_000, class: 'backoff', durable: true, wake: 'unblockOutbox / pullOutbox / ceiling' },
  /** outbox backoff cap */
  outboxRetryCap: { ms: 300_000, class: 'backoff', durable: true, wake: 'unblockOutbox / ceiling' },
  /** failed tx-effect replay backoff base (2^(attempts-1) × this, spread()-jittered) */
  txEffectRetryBase: { ms: 60_000, class: 'backoff', durable: true, wake: 'replayTxEffects' },
  /** failed tx-effect replay backoff cap */
  txEffectRetryCap: { ms: 3_600_000, class: 'backoff', durable: true, wake: 'replayTxEffects' },
  /** repair-queue enqueue delay (fanout) */
  repairDelay: { ms: 20_000, class: 'backoff', durable: true, wake: 'queueRepairs on miss' },
  /** watch-shim reconnect backoff bounds (jittered: * (0.5+rand)) */
  watchBackoffMin: { ms: 1_000, class: 'backoff', durable: false, wake: 'answered request resets' },
  watchBackoffCap: { ms: 30_000, class: 'backoff', durable: false, wake: 'answered request resets' },

  // ── io bounds (per-call dead-man switches — not wait sites) ─────────
  resolveFingerIo: { ms: 5_000, class: 'io', durable: false, wake: 'floor-only' },
  laneReadIo: { ms: 5_000, class: 'io', durable: false, wake: 'floor-only' },
  /** direct-push / notify POSTs — fire-and-forget; the bound only frees
   *  the socket */
  pushIo: { ms: 2_000, class: 'io', durable: false, wake: 'floor-only' },
  /** watch-shim fetch bound — the shim holds ≤25s server-side; the call
   *  bound must exceed it or the long-poll becomes an error loop */
  watchFetchIo: { ms: 35_000, class: 'io', durable: false, wake: 'shim answer' },
  sandboxIo: { ms: 15_000, class: 'io', durable: false, wake: 'floor-only' },
  /** toolbox WalletServices regtest RPC calls (header/height reads) */
  rpcIo: { ms: 15_000, class: 'io', durable: false, wake: 'floor-only' },
  preflightHostIo: { ms: 5_000, class: 'io', durable: false, wake: 'floor-only' },
  preflightChainIo: { ms: 3_000, class: 'io', durable: false, wake: 'floor-only' },
  preflightStoreIo: { ms: 10_000, class: 'io', durable: false, wake: 'floor-only' },
  /** restate ingress SEND — the sidecar is local; a wedged one fails fast
   *  rather than parking a durable send on a dead socket. Calls get a
   *  separate budget — see callIo */
  ingressIo: { ms: 10_000, class: 'io', durable: false, wake: 'floor-only' },
  /** restate ingress CALL — a request-response call waits for the handler
   *  to run on its key's queue (queue depth + leg time), so its abort must
   *  outlive the worst serialized leg, not just the socket. An aborted call
   *  does NOT cancel the invocation — it keeps running detached and its
   *  effect may still land; the caller must read abort as "unknown", never
   *  as "failed" (rt/timeout-as-verdict). Sized over the longest journaled
   *  legs (deliverBoundMs 45s + queue) */
  callIo: { ms: 120_000, class: 'io', durable: false, wake: 'floor-only' },
  /** the deployment-registration POST to the admin API — boot-path io */
  registerIo: { ms: 5_000, class: 'io', durable: false, wake: 'floor-only' },
  /** nap between registration attempts while the server warms up */
  registerRetry: { ms: 500, class: 'backoff', durable: false, wake: 'server answer' },
  /** the transport-paused self-heal floor — restate pushes no pause event;
   *  the mechanism is 'don't pause' (endpoint reachable), this floor only
   *  unpauses whatever slipped anyway. ceiling, sloppy is fine */
  healPaused: { ms: 60_000, class: 'ceiling', durable: false, wake: 'floor-only' }
} satisfies Record<string, WaitSpec>
