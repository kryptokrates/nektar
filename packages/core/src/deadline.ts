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

import { setTimeout as nodeSleep } from 'node:timers/promises'

export type DeadlineOutcome<T> = { outcome: 'settled'; value: T } | { outcome: 'timeout' } | { outcome: 'aborted' }

/** run `work` with a fresh `AbortSignal.timeout(ms)` merged into the
 *  caller's signal — resolved with the branch that ended the wait.
 *  Real work errors propagate; a signal-aware leg that throws as it
 *  aborts reports the signal's outcome, never its own error. */
export const deadline = async <T>(work: (signal: AbortSignal) => Promise<T>, ms: number, opts: { signal?: AbortSignal } = {}): Promise<DeadlineOutcome<T>> => {
  const own = AbortSignal.timeout(ms)
  const signal = opts.signal !== undefined ? AbortSignal.any([opts.signal, own]) : own
  const settled = work(signal).then(
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
  const r = await Promise.race([settled, fired])
  if (r !== 'fired') return r
  return own.aborted ? { outcome: 'timeout' } : { outcome: 'aborted' }
}

/** the repo's one sleep — `node:timers/promises` so every sleep site can
 *  carry a signal for free: a nap inside a deadline()-bound leg dies with
 *  the leg instead of outliving it. Rejects (AbortError) on signal — the
 *  leg's awaits observe cancellation as a throw, which is what
 *  deadline()'s aborted-branch check reads. */
export const nap = async (ms: number, opts: { signal?: AbortSignal } = {}): Promise<void> => {
  await nodeSleep(ms, undefined, { signal: opts.signal })
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
