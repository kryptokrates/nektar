// outbox.ts — the outbox drain: receipt pushes after a tick, and the
// store-driven pump (wake on outboxReady + a bounded ceiling timer on the
// next row's next_at) that keeps durable delivery off the poll cadence.
import type { Rt } from './rtctx.js'

export function openOutbox(rt: Rt): void {
  const { store, engine, log } = rt
  // envelope receipts ride the journaled ack path — submitted AFTER the
  // engine's tick under their own busy guard, so a slow or dead host can
  // never delay the processing of messages that just arrived
  const pushAcks = (): void => {
    if (rt.acking || rt.stopped || rt.msgs?.ack === undefined) return
    rt.acking = true
    void rt.msgs
      .ack()
      .catch((e) => log(`automation: box ack: ${(e as Error).message}`))
      .finally(() => {
        rt.acking = false
      })
  }
  // outbox delivery is detached under its own busy guard — the store's
  // outboxReady hook wakes it on every enqueue (so a want/accept/message
  // hits the wire mid-tick, not at the next one), a next_at timer covers
  // deferred rows exactly, and the end-of-tick poke is the correctness
  // floor. Deliveries never touch op state — safe off the engine mutex.
  const armOutboxTimer = async (): Promise<void> => {
    if (rt.stopped) return
    try {
      const nextAt = await store.nextOutboxAt()
      if (nextAt === undefined || rt.stopped) return
      clearTimeout(rt.outboxTimer)
      if (rt.scheduler !== undefined) {
        // the shared wheel owns the ceiling — this arm is the exact next_at wake
        rt.scheduler.dueIn('outbox', Math.max(0, nextAt - Date.now()))
      } else {
        rt.outboxTimer = setTimeout(
          () => {
            rt.outboxTimer = undefined
            rt.pumpOutbox()
          },
          Math.max(0, nextAt - Date.now())
        )
        rt.outboxTimer.unref()
      }
    } catch {
      /* the deals-pass floor covers a missed arm */
    }
  }
  rt.pumpOutbox = (): void => {
    if (rt.stopped) return
    if (rt.pumping) {
      rt.pumpAgain = true
      return
    }
    rt.pumping = true
    rt.pumpDone = engine
      .pumpOutbox()
      .catch((e) => log(`automation: outbox pump: ${(e as Error).message}`))
      .finally(() => {
        rt.pumping = false
        if (rt.pumpAgain) {
          rt.pumpAgain = false
          rt.pumpOutbox()
          return
        }
        void armOutboxTimer()
      })
  }
  store.outboxReady = () => rt.pumpOutbox()
  // register the job once — exact-at wakes come from dueIn() on each arm;
  // the 60s ceiling is only the missed-wake backstop (the end-of-tick pump
  // is already the correctness floor)
  rt.scheduler?.add({ name: 'outbox', run: () => rt.pumpOutbox(), ceilingMs: () => 60_000 })
  rt.pushAcks = pushAcks
}
