// scheduler.ts — the one timer in the automation runtime. Every recurring
// job (deals, outbox, upkeep, watches, watchdog) registers a name, a run
// body and a ceiling: the longest it may go without firing. Wakes are
// event sources calling wake(name) or dueIn(name, ms); the wheel itself is
// a single setTimeout aimed at the earliest due job — no job owns a private
// interval and no two jobs race a shared clock.
//
// Job bodies run detached(): Node timers capture the AsyncLocalStorage
// context at creation, so a dueIn() armed inside a leg would otherwise run
// the job inside THAT leg's context — the job's own bound() legs would
// merge a dead deadline. Timer-fired work is never the armer's child.

import { detached } from '@nektar/core'

export interface SchedJob {
  name: string
  run: () => void | Promise<void>
  /** ms until this job MUST next fire — may vary per call (the deals job
   *  returns the hot cadence while the loop is hot, idle otherwise) */
  ceilingMs: () => number
}

interface Entry {
  job: SchedJob
  nextAt: number
  running: Promise<void> | undefined
  /** what armed the current nextAt — the witness for 'events drive, polls
   *  are ceilings': schedule() reports per-job fire counts by source */
  armSource: 'wake' | 'dueIn' | undefined
  fires: { wake: number; dueIn: number; ceiling: number }
}

export class Scheduler {
  private jobs = new Map<string, Entry>()
  private timer: ReturnType<typeof setTimeout> | undefined
  private stopped = false
  constructor(private readonly now: () => number = Date.now) {}

  /** register a job; first fire is ceilingMs() out unless a sooner wake lands */
  add(job: SchedJob): void {
    this.jobs.set(job.name, { job, nextAt: this.now() + job.ceilingMs(), running: undefined, armSource: undefined, fires: { wake: 0, dueIn: 0, ceiling: 0 } })
    this.arm()
  }

  /** an event removed this job's wait cause — fire it at the next wheel tick */
  wake(name: string): void {
    const e = this.jobs.get(name)
    if (e === undefined || this.stopped) return
    e.nextAt = this.now()
    e.armSource = 'wake'
    this.arm()
  }

  /** exact due time — the outbox job's next_at; a no-op for unregistered names */
  dueIn(name: string, ms: number): void {
    const e = this.jobs.get(name)
    if (e === undefined || this.stopped) return
    e.nextAt = this.now() + Math.max(0, ms)
    e.armSource = 'dueIn'
    this.arm()
  }

  /** jobs run their own re-entry guards — the wheel never skips a due fire,
   *  overlapping a still-running job just means it fires again after */
  private arm(): void {
    if (this.stopped) return
    let next: number | undefined
    for (const e of this.jobs.values()) next = next === undefined ? e.nextAt : Math.min(next, e.nextAt)
    if (next === undefined) return
    clearTimeout(this.timer)
    this.timer = setTimeout(() => void this.fire(), Math.max(0, next - this.now()))
    this.timer.unref()
  }

  private async fire(): Promise<void> {
    if (this.stopped) return
    const now = this.now()
    const due: Entry[] = []
    for (const e of this.jobs.values()) {
      if (e.nextAt <= now) {
        e.fires[e.armSource ?? 'ceiling']++
        e.armSource = undefined
        e.nextAt = now + e.job.ceilingMs()
        due.push(e)
      }
    }
    // due jobs fire concurrently — today's semantics (watchdog/watch passes
    // already ran independent of the pump); each job guards its own re-entry
    for (const e of due) {
      const work = Promise.resolve()
        .then(() => detached(() => e.job.run()))
        .then(
          () => undefined,
          () => undefined
        )
      e.running = work
      const clear = (): void => {
        if (e.running === work) e.running = undefined
      }
      // then(both), not .finally — a void'd .finally-derived promise
      // inherits the rejection and lands as an unhandledRejection
      void work.then(clear, clear)
    }
    this.arm()
  }

  /** diagnostics — name → { ms until due (negative = overdue/running),
   *  whether a fire is in flight, and the witness counts: which mechanism
   *  armed each fire. A job whose fires are all 'ceiling' is a poll loop
   *  in disguise — its wakes exist on paper but never land. */
  schedule(): Record<string, { dueIn: number; running: boolean; fires: { wake: number; dueIn: number; ceiling: number } }> {
    const out: Record<string, { dueIn: number; running: boolean; fires: { wake: number; dueIn: number; ceiling: number } }> = {}
    for (const [name, e] of this.jobs) out[name] = { dueIn: e.nextAt - this.now(), running: e.running !== undefined, fires: { ...e.fires } }
    return out
  }

  async stop(): Promise<void> {
    this.stopped = true
    clearTimeout(this.timer)
    await Promise.allSettled([...this.jobs.values()].flatMap((e) => (e.running !== undefined ? [e.running] : [])))
  }

  get isStopped(): boolean {
    return this.stopped
  }
}
