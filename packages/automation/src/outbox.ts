// outbox.ts — the outbox drain: receipt pushes after a tick, and the
// store-driven pump (outboxReady pokes the transport drain, whose journaled
// scan → per-destination deliver invocations self-schedule the ceiling at
// the earliest next_at). Rows stay the durable intent; scheduling lives on
// the executor — the drain's delayed self-arm is a durable server timer.
import { detached } from '@nektar/core'
import type { Rt } from './rtctx.js'
import { executor, EXEC_SVC } from './rt/exec.js'
import { TRANSPORT_DRAIN_KEY } from './rt/services.js'

export function openOutbox(rt: Rt): void {
  const { store, log } = rt
  const DRAIN = { service: EXEC_SVC.transport, handler: 'drain', key: TRANSPORT_DRAIN_KEY } as const
  // envelope receipts ride the journaled ack path — submitted AFTER the
  // engine's tick under their own busy guard, so a slow or dead host can
  // never delay the processing of messages that just arrived. detached():
  // the ack drain is spawned work — it must not die with the leg that
  // happened to poke it
  const pushAcks = (): void => {
    const ack = rt.msgs?.ack
    if (rt.acking || rt.stopped || ack === undefined) return
    rt.acking = true
    const clear = (): void => {
      rt.acking = false
    }
    void detached(() =>
      ack()
        .catch((e) => log(`automation: box ack: ${(e as Error).message}`))
        .then(clear, clear)
    )
  }
  rt.pushAcks = pushAcks
  // rows are the durable intent; delivery + scheduling live on the
  // transport object — outboxReady pokes the drain, deliver lands per
  // destination key, the drain self-arms its ceiling at the earliest
  // next_at. A boot poke covers rows pending across a restart.
  const poke = (): void => {
    // a wake hint — it must never throw: at boot the executor may not have
    // its transport handlers wired yet (Dispatcher registers at the wire
    // step, which runs after openOutbox); the drain's self-armed ceiling
    // is the missed-wake backstop either way
    try {
      void (rt.exec ?? executor())
        .send(DRAIN, {})
        .catch((e: unknown) => {
          log(`automation: transport drain send: ${(e as Error).message}`)
        })
    } catch (e) {
      log(`automation: transport drain send: ${(e as Error).message}`)
    }
  }
  rt.pumpOutbox = () => {
    rt.drainDone = (rt.exec ?? executor())
      .call(DRAIN, {})
      .then(() => undefined)
      .catch((e: unknown) => {
        log(`automation: outbox pump: ${(e as Error).message}`)
      })
  }
  store.outboxReady = poke
  poke()
}
