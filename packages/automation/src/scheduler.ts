// scheduler.ts — the one timer in the automation runtime. Every recurring
// job (deals, outbox, upkeep, watches, watchdog) registers a name, a run
// body and a ceiling: the longest it may go without firing. Wakes are
// event sources calling wake(name) or dueIn(name, ms); the wheel itself is
// a single setTimeout aimed at the earliest due job — no job owns a private
// interval and no two jobs race a shared clock.

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
}

export class Scheduler {
  private jobs = new Map<string, Entry>()
  private timer: ReturnType<typeof setTimeout> | undefined
  private stopped = false
  constructor(private readonly now: () => number = Date.now) {}

  /** register a job; first fire is ceilingMs() out unless a sooner wake lands */
  add(job: SchedJob): void {
    this.jobs.set(job.name, { job, nextAt: this.now() + job.ceilingMs(), running: undefined })
    this.arm()
  }

  /** an event removed this job's wait cause — fire it at the next wheel tick */
  wake(name: string): void {
    const e = this.jobs.get(name)
    if (e === undefined || this.stopped) return
    e.nextAt = this.now()
    this.arm()
  }

  /** exact due time — the outbox job's next_at; a no-op for unregistered names */
  dueIn(name: string, ms: number): void {
    const e = this.jobs.get(name)
    if (e === undefined || this.stopped) return
    e.nextAt = this.now() + Math.max(0, ms)
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
        e.nextAt = now + e.job.ceilingMs()
        due.push(e)
      }
    }
    // due jobs fire concurrently — today's semantics (watchdog/watch passes
    // already ran independent of the pump); each job guards its own re-entry
    for (const e of due) {
      const work = Promise.resolve()
        .then(() => e.job.run())
        .then(
          () => undefined,
          () => undefined
        )
      e.running = work
      void work.finally(() => {
        if (e.running === work) e.running = undefined
      })
    }
    this.arm()
  }

  /** diagnostics — name → ms until due (negative = overdue/running) */
  schedule(): Record<string, number> {
    const out: Record<string, number> = {}
    for (const [name, e] of this.jobs) out[name] = e.nextAt - this.now()
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
