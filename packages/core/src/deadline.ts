// deadline.ts — the one place "stop waiting" and "stop the work" meet.
// `Promise.race` resolves the waiter; it never cancels the work — a
// race'd-off leg keeps running, and its late writes land in the
// successor pass's world (the orphan-write hazard: journal appends,
// provisions, publishes can double-run against the next pass). deadline()
// hands the work a real abort instead: `AbortSignal.timeout(ms)` merged
// with the caller's own signal via `AbortSignal.any`, so a leg that
// exceeds its bound dies at its next awaited IO instead of lingering.
//
// Work that ignores the signal still gets the race — the timeout resolves
// the waiter either way. The signal is the difference between an
// abandoned promise that dies with its holds (sockets, leases, the
// serialized mutex link) and one that keeps them silently.
//
// `outcome` names the branch that ended the wait: 'settled' (work
// finished), 'timeout' (our own bound fired — worth a log line), or
// 'aborted' (the caller's signal died first — a shutdown, not a
// slowness; do not log it as an overrun).
//
// The leg context: deadline() also publishes its merged signal on an
// AsyncLocalStorage (the Go context.Context shape). Inner IO reads
// currentSignal()/ioSignal()/nap() and inherits the operation deadline by
// construction — no caller threads it, so none can forget it. Nested
// deadline()s can only tighten (ambient merges in, never outlives the
// outer leg). Fire-and-forget work spawned inside a leg — post-commit
// hooks, event-handler launches, timer-job bodies — runs through
// detached() so it inherits NOTHING: a leg must not reach into work that
// outlives it, and work that outlives a leg must not die with it.

import { setTimeout as nodeSleep } from 'node:timers/promises'
import { AsyncLocalStorage } from 'node:async_hooks'

export type DeadlineOutcome<T> = { outcome: 'settled'; value: T } | { outcome: 'timeout' } | { outcome: 'aborted' }

const legCtx = new AsyncLocalStorage<AbortSignal>()

/** the ambient leg signal — undefined outside deadline()-bound work. IO
 *  helpers (ioSignal, nap) merge it automatically; raw call sites that
 *  take a signal merge it the same way (`AbortSignal.any`). */
export const currentSignal = (): AbortSignal | undefined => legCtx.getStore()

/** run `work` with NO ambient leg — the escape hatch for spawned
 *  fire-and-forget work (post-commit hooks, emitter-handler launches,
 *  the scheduler's job dispatch). The spawned chain is context-free:
 *  it neither inherits the spawning leg's deadline nor leaks it onward. */
export const detached = <T>(work: () => T): T => legCtx.exit(work)

/** the shared IO-signal shape for a bounded call inside (or outside) a
 *  leg: own `AbortSignal.timeout(ms)` + the ambient leg + an explicit
 *  caller signal — all merged, so the call dies at the earliest of its
 *  own bound, the operation deadline, or the caller's shutdown. An inner
 *  call can only tighten the budget, never outlive it. */
export const ioSignal = (ms: number, extra?: AbortSignal): AbortSignal => {
  const own = AbortSignal.timeout(ms)
  const ups = [extra, currentSignal()].filter((s): s is AbortSignal => s !== undefined)
  return ups.length === 0 ? own : AbortSignal.any([...ups, own])
}

/** run `work` with a fresh `AbortSignal.timeout(ms)` merged into the
 *  caller's signal AND the ambient leg — resolved with the branch that
 *  ended the wait. The merged signal also becomes the ambient context
 *  for everything `work` awaits, so an inner deadline can tighten but
 *  never outlive this leg. When the leg ends (any outcome) the composite
 *  settles: `AbortSignal.any` dependents hold a slot on every source
 *  until they abort or collect, so a settled leg that left its composite
 *  pending would pin slots on long-lived sources (the runtime umbrella)
 *  until GC — the settle-abort releases them at leg end and kills any
 *  leg leftover still holding the signal. */
export const deadline = async <T>(work: (signal: AbortSignal) => Promise<T>, ms: number, opts: { signal?: AbortSignal } = {}): Promise<DeadlineOutcome<T>> => {
  const own = AbortSignal.timeout(ms)
  const done = new AbortController()
  const ups = [opts.signal, currentSignal()].filter((s): s is AbortSignal => s !== undefined)
  const signal = AbortSignal.any([...ups, own, done.signal])
  const settled = legCtx
    .run(signal, () => work(signal))
    .then(
      (value): DeadlineOutcome<T> => ({ outcome: 'settled', value }),
      (e: unknown) => {
        if (signal.aborted) return own.aborted ? ({ outcome: 'timeout' } as const) : ({ outcome: 'aborted' } as const)
        throw e
      }
    )
  const fired = new Promise<'fired'>((res) => {
    if (signal.aborted) res('fired')
    else signal.addEventListener('abort', () => res('fired'), { once: true })
  })
  try {
    const r = await Promise.race([settled, fired])
    if (r !== 'fired') return r
    // the abandoned leg lives on — swallow its late rejection: the caller
    // already moved on, and an orphaned throw must not crash the process
    void settled.catch(() => undefined)
    return own.aborted ? { outcome: 'timeout' } : { outcome: 'aborted' }
  } finally {
    done.abort()
  }
}

/** the repo's one sleep — `node:timers/promises` so every sleep site can
 *  carry a signal for free: a nap inside a deadline()-bound leg dies with
 *  the leg instead of outliving it — explicit signal or not, the ambient
 *  leg merges in. Rejects with the firing signal's reason (AbortError on
 *  caller/shutdown abort, TimeoutError on a leg bound) — the leg's awaits
 *  observe cancellation as a throw, which is what deadline()'s
 *  aborted-branch check reads. */
export const nap = async (ms: number, opts: { signal?: AbortSignal } = {}): Promise<void> => {
  const ambient = currentSignal()
  const ups = [opts.signal, ambient].filter((s): s is AbortSignal => s !== undefined)
  const signal = ups.length === 0 ? undefined : ups.length === 1 ? ups[0] : AbortSignal.any(ups)
  await nodeSleep(ms, undefined, { signal })
}

/** a fixed cadence is a phase-aligned stampede the day N processes
 *  restart together — `ms * (0.5 + rand)` is the in-repo jitter shape
 *  (watches.ts). `rand` defaults to Math.random(); pass a stable [0,1)
 *  for a stored deadline (same spread on every recompute, no test
 *  flake — derive it from a row key, see stable01). */
export const spread = (ms: number, rand: number = Math.random()): number => Math.ceil(ms * (0.5 + rand))

/** a deterministic [0,1) for a string key — spread's seed for stored
 *  deadlines (outbox next_at): per-row phase separation that survives
 *  restarts and recomputes identically. FNV-1a — cheap, sufficient for
 *  spreading, not a hash of record. */
export const stable01 = (s: string): number => {
  let h = 0x811c9dc5
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i)
    h = Math.imul(h, 0x01000193)
  }
  return (h >>> 0) / 0xffffffff
}
