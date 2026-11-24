import { test } from 'node:test'
import assert from 'node:assert/strict'
import type * as restate from '@restatedev/restate-sdk'
import { nektarEnv } from '@nektar/core'
import { inRtScope, rtScopeKey, RestateExecutor, type Executor, type InvokeTarget } from '../src/rt/exec.js'
import { Dispatcher, type DispatchHandler } from '../src/rt/dispatch.js'
import { openEndpoint } from '../src/rt/endpoint.js'

// rt-conformance — the executor conformance matrix: ONE scenario suite run
// against every Executor implementation. The Dispatcher (in-process test
// seam, dispatch.ts) and the RestateExecutor (durable ingress + endpoint,
// exec.ts) are two implementations of one contract — this file pins which
// semantics are the same contract and which are deliberately not parity.
//
// matrix (MATCH — both executors must satisfy):
//   call resolves to the handler's result; a handler failure propagates
//   send is fire-and-forget — resolves at accept, the handler lands async
//   idempotencyKey dedupes sends (one run) and calls (attach to the
//     original's result); a call carrying a send's key attaches to it
//   same-key invocations run one-at-a-time in accept order
//   different keys are independent — a blocked key holds nothing else
//   delayMs defers the invocation (fires at/after the arm, never early)
//   the handler's rt scope holds only its own key — a spawned send lands
//     with a fresh scope (the spawner's held keys never leak)
//   an unrouted target refuses
//
// divergent BY DESIGN — documented so a future "fix" can't force parity
// where parity would lie:
//   call() joins spawned sends — Dispatcher test determinism ('one call =
//     the whole pass'); the server's journaled sends run detached past
//     handler-return
//   close() exists — the ingress has no close
//   same-key self-call — the Dispatcher throws loudly; on the server it
//     would queue behind itself (the hang inRtScope exists to prevent —
//     asserting it live means asserting a deadlock)
//   idempotency retention — the Dispatcher's map never expires; the
//     server's window is deployment config outside this repo
//   delay horizons — the Dispatcher clamps at the int32 timer ceiling;
//     the server carries true durable delays
//   arg transport — the server JSON-serializes; the Dispatcher passes the
//     reference. Handlers must not rely on either direction.
//
// The live leg needs a restate server (see rt-spike.test.ts for the docker
// one-liner): NEKTAR_RESTATE_URL=... NEKTAR_RESTATE_ADMIN=... pnpm --filter
// @nektar/automation test:rt-live

const RESTATE_URL = nektarEnv('NEKTAR_RESTATE_URL')
const ADMIN = nektarEnv('NEKTAR_RESTATE_ADMIN') ?? 'http://127.0.0.1:9070'
const ENDPOINT_URI = nektarEnv('NEKTAR_RESTATE_ENDPOINT_URI') ?? 'http://host.docker.internal:9080'
const ENDPOINT_PORT = nektarEnv('NEKTAR_RESTATE_ENDPOINT_PORT') ?? '9080'
// the server's state is durable across suite runs — service names, keys
// and idempotency keys get a per-run suffix or leftover invocations answer
const RUN = `${process.pid}-${Date.now().toString(36)}`

const until = async (what: () => Promise<boolean> | boolean, ms = 10_000): Promise<void> => {
  const at = Date.now() + ms
  while (Date.now() < at) {
    if (await what()) return
    await new Promise((r) => setTimeout(r, 100))
  }
  throw new Error('until: deadline')
}

interface Gate {
  promise: Promise<void>
  resolve(): void
}
const mkGate = (): Gate => {
  let resolve!: () => void
  const promise = new Promise<void>((r) => (resolve = r))
  return { promise, resolve }
}

interface ProbeArg {
  mark?: string
  /** wait on a named in-process gate — proves a send resolved at accept */
  gate?: string
  sleepMs?: number
  /** throw — pins failure propagation (mark is the message) */
  fail?: boolean
  /** emit a send on the SAME probe service from inside the handler */
  spawn?: { key: string; arg: ProbeArg }
}

interface ProbeRecord {
  mark: string
  key?: string
  held: readonly string[]
  at: number
  end?: number
}

/** one probe body, two schedules: registered raw on a Dispatcher, wrapped
 *  in ctx.run on the endpoint — the SAME function answers on both planes,
 *  so a divergence is the executor's, never a fixture skew. */
const mkProbe = (svc: string, emit: (t: InvokeTarget, arg: unknown) => Promise<void>) => {
  const records: ProbeRecord[] = []
  const gates = new Map<string, Gate>()
  const gate = (name: string): Gate => {
    let g = gates.get(name)
    if (g === undefined) {
      g = mkGate()
      gates.set(name, g)
    }
    return g
  }
  const handler: DispatchHandler = async (arg, key) => {
    const a = (arg ?? {}) as ProbeArg
    const rec: ProbeRecord = { mark: a.mark ?? '', key, held: rtScopeKey()?.held ?? [], at: Date.now() }
    records.push(rec)
    try {
      if (a.fail === true) throw new Error(a.mark ?? 'boom')
      if (a.gate !== undefined) await gate(a.gate).promise
      if (a.sleepMs !== undefined) await new Promise((r) => setTimeout(r, a.sleepMs))
      if (a.spawn !== undefined) await emit({ service: svc, handler: 'tick', key: a.spawn.key }, a.spawn.arg)
      return { mark: a.mark ?? 'ok', key }
    } finally {
      rec.end = Date.now()
    }
  }
  return { records, gate, handler }
}

/** the endpoint def wrapping the probe — mirrors the wire registration's
 *  scope shape: the handler holds exactly its own service/key scope */
const probeObject = async (svc: string, probe: DispatchHandler) => {
  const restate = await import('@restatedev/restate-sdk')
  return restate.object({
    name: svc,
    handlers: {
      tick: async (ctx: restate.ObjectContext, arg: unknown) =>
        await inRtScope({ held: [`${svc}/${ctx.key}`] }, () => ctx.run('tick', () => probe(arg, ctx.key)))
    }
  })
}

interface Ctx {
  ex: Executor
  svc: string
  probe: ReturnType<typeof mkProbe>
  run: string
}
interface Scenario {
  name: string
  /** match: the shared contract — runs on both executors. false pins a
   *  documented Dispatcher-only semantic (see the header) */
  match: boolean
  run: (c: Ctx) => Promise<void>
}

const matrix: Scenario[] = [
  {
    name: 'call resolves to the handler result',
    match: true,
    run: async (c) => {
      const r = await c.ex.call<{ mark: string; key?: string }>({ service: c.svc, handler: 'tick', key: `A-${c.run}` }, { mark: 'r1' })
      assert.deepEqual(r, { mark: 'r1', key: `A-${c.run}` })
    }
  },
  {
    name: 'call propagates the handler failure',
    match: true,
    run: async (c) => {
      await assert.rejects(c.ex.call({ service: c.svc, handler: 'tick', key: `E-${c.run}` }, { mark: 'boom', fail: true }), /boom/)
    }
  },
  {
    name: 'send resolves at accept — a gated handler still lands after',
    match: true,
    run: async (c) => {
      const key = `S-${c.run}`
      const g = c.probe.gate(`g-${key}`)
      await c.ex.send({ service: c.svc, handler: 'tick', key }, { mark: 'send', gate: `g-${key}` })
      // send already returned; the handler must be waiting on the gate
      await until(() => c.probe.records.some((r) => r.key === key))
      assert.equal(c.probe.records.find((r) => r.key === key)?.end, undefined)
      g.resolve()
      await until(() => c.probe.records.find((r) => r.key === key)?.end !== undefined)
    }
  },
  {
    name: 'idempotencyKey dedupes sends — one invocation for the window',
    match: true,
    run: async (c) => {
      const key = `IS-${c.run}`
      const idem = `idem-send-${c.run}`
      await c.ex.send({ service: c.svc, handler: 'tick', key }, { mark: 'once' }, { idempotencyKey: idem })
      await c.ex.send({ service: c.svc, handler: 'tick', key }, { mark: 'dup' }, { idempotencyKey: idem })
      await until(() => c.probe.records.some((r) => r.key === key))
      await new Promise((r) => setTimeout(r, 300))
      assert.equal(c.probe.records.filter((r) => r.key === key).length, 1)
      assert.equal(c.probe.records.find((r) => r.key === key)?.mark, 'once')
    }
  },
  {
    name: 'idempotencyKey dedupes calls — a dup attaches to the original result',
    match: true,
    run: async (c) => {
      const key = `IC-${c.run}`
      const idem = `idem-call-${c.run}`
      const r1 = await c.ex.call<{ mark: string }>({ service: c.svc, handler: 'tick', key }, { mark: 'first' }, { idempotencyKey: idem })
      const r2 = await c.ex.call<{ mark: string }>({ service: c.svc, handler: 'tick', key }, { mark: 'second' }, { idempotencyKey: idem })
      assert.deepEqual(r1, r2)
      assert.equal(r2.mark, 'first')
      assert.equal(c.probe.records.filter((r) => r.key === key).length, 1)
    }
  },
  {
    name: "a call carrying a send's key attaches to the send's invocation",
    match: true,
    run: async (c) => {
      const key = `SC-${c.run}`
      const idem = `idem-sc-${c.run}`
      await c.ex.send({ service: c.svc, handler: 'tick', key }, { mark: 'sendWins' }, { idempotencyKey: idem })
      const r = await c.ex.call<{ mark: string }>({ service: c.svc, handler: 'tick', key }, { mark: 'callDup' }, { idempotencyKey: idem })
      assert.equal(r.mark, 'sendWins')
      assert.equal(c.probe.records.filter((r) => r.key === key).length, 1)
    }
  },
  {
    name: 'same-key sends run one-at-a-time in accept order',
    match: true,
    run: async (c) => {
      const key = `ORD-${c.run}`
      await c.ex.send({ service: c.svc, handler: 'tick', key }, { mark: 's1', sleepMs: 120 })
      await c.ex.send({ service: c.svc, handler: 'tick', key }, { mark: 's2' })
      await until(() => c.probe.records.filter((r) => r.key === key).length === 2)
      const [s1, s2] = c.probe.records.filter((r) => r.key === key)
      assert.deepEqual([s1.mark, s2.mark], ['s1', 's2'])
      assert.ok(s2.at >= s1.end!, `second invocation overlapped the first: at=${s2.at} end=${s1.end}`)
    }
  },
  {
    name: 'different keys are independent — a blocked key holds nothing else',
    match: true,
    run: async (c) => {
      const g = c.probe.gate(`gA-${c.run}`)
      const ka = `BLK-A-${c.run}`
      const kb = `BLK-B-${c.run}`
      await c.ex.send({ service: c.svc, handler: 'tick', key: ka }, { mark: 'blocked', gate: `gA-${c.run}` })
      await until(() => c.probe.records.some((r) => r.key === ka))
      await c.ex.send({ service: c.svc, handler: 'tick', key: kb }, { mark: 'free' })
      // B completes while A still holds — false per-key serialization
      // would deadlock here
      await until(() => c.probe.records.find((r) => r.key === kb)?.end !== undefined)
      g.resolve()
      await until(() => c.probe.records.find((r) => r.key === ka)?.end !== undefined)
    }
  },
  {
    name: 'delayMs defers the invocation — never early, within a bound',
    match: true,
    run: async (c) => {
      const key = `DLY-${c.run}`
      const at = Date.now()
      await c.ex.send({ service: c.svc, handler: 'tick', key }, { mark: 'delayed' }, { delayMs: 400 })
      await until(() => c.probe.records.some((r) => r.key === key))
      const fired = c.probe.records.find((r) => r.key === key)!.at
      assert.ok(fired - at >= 250, `fired ${fired - at}ms after arm — an early arm is a wasted wake`)
    }
  },
  {
    name: "the handler's scope holds only its own key — a spawned send lands fresh",
    match: true,
    run: async (c) => {
      const parent = `P-${c.run}`
      const child = `C-${c.run}`
      await c.ex.call({ service: c.svc, handler: 'tick', key: parent }, { mark: 'parent', spawn: { key: child, arg: { mark: 'child' } } })
      await until(() => c.probe.records.some((r) => r.key === child))
      const p = c.probe.records.find((r) => r.key === parent)!
      const ch = c.probe.records.find((r) => r.key === child)!
      assert.deepEqual(p.held, [`${c.svc}/${parent}`])
      assert.deepEqual(ch.held, [`${c.svc}/${child}`], 'spawned invocation inherited the spawner\'s held keys — it can "go direct" past locks it never took')
    }
  },
  {
    name: 'an unrouted target refuses',
    match: true,
    run: async (c) => {
      await assert.rejects(async () => await c.ex.send({ service: `${c.svc}-none`, handler: 'tick', key: 'x' }, {}))
      await assert.rejects(c.ex.call({ service: `${c.svc}-none`, handler: 'tick', key: 'x' }, {}))
    }
  },
  // ── Dispatcher-only semantics — the documented divergences, pinned so a
  // future change can't quietly slide the seam away from its contract ──
  {
    name: 'call() joins the sends its handler spawned (Dispatcher-only)',
    match: false,
    run: async (c) => {
      const parent = `JP-${c.run}`
      const child = `JC-${c.run}`
      await c.ex.call({ service: c.svc, handler: 'tick', key: parent }, { mark: 'p', spawn: { key: child, arg: { mark: 'c' } } })
      // no until(): the child already ran — the 'one call = the whole pass'
      // contract joins spawned sends before the call resolves (the server's
      // journaled sends run detached — the divergence is deliberate)
      assert.equal(c.probe.records.find((r) => r.key === child)?.end !== undefined, true)
    }
  },
  {
    name: 'close() stops accepting: sends no-op, calls throw (Dispatcher-only)',
    match: false,
    run: async (c) => {
      const ex = c.ex as Dispatcher
      await ex.close()
      await c.ex.send({ service: c.svc, handler: 'tick', key: `Z-${c.run}` }, { mark: 'post-close' })
      await new Promise((r) => setTimeout(r, 50))
      assert.equal(
        c.probe.records.some((r) => r.key === `Z-${c.run}`),
        false
      )
      await assert.rejects(c.ex.call({ service: c.svc, handler: 'tick', key: `Z-${c.run}` }, {}), /closed/)
    }
  },
  {
    name: 'a same-key call on a held scope throws — never queues behind itself (Dispatcher-only)',
    match: false,
    run: async (c) => {
      const scope = `${c.svc}/HELD-${c.run}`
      await assert.rejects(
        inRtScope({ held: [scope] }, () => c.ex.call({ service: c.svc, handler: 'tick', key: `HELD-${c.run}` }, {})),
        /re-entrant/
      )
    }
  },
  {
    name: 'idempotency outlives completion — the map never expires (Dispatcher-only)',
    match: false,
    run: async (c) => {
      const key = `NE-${c.run}`
      const idem = `idem-ne-${c.run}`
      await c.ex.call({ service: c.svc, handler: 'tick', key }, { mark: 'first' }, { idempotencyKey: idem })
      await c.ex.send({ service: c.svc, handler: 'tick', key }, { mark: 'late' }, { idempotencyKey: idem })
      await new Promise((r) => setTimeout(r, 100))
      assert.equal(c.probe.records.filter((r) => r.key === key).length, 1, 'a completed invocation still dedupes — the runtime retry discipline relies on it')
    }
  },
  {
    name: 'call() clears txKey/custodyKey ALS in callee while preserving held deadlock guard (Dispatcher-only)',
    match: false,
    run: async (c) => {
      const ex = c.ex as Dispatcher
      let calleeScope: ReturnType<typeof rtScopeKey>
      ex.register(c.svc, 'inspect', () => {
        calleeScope = rtScopeKey()
        return Promise.resolve({ ok: true })
      })
      await inRtScope({ txKey: 'tx-caller', custodyKey: 'cust-caller', held: [`${c.svc}/caller`] }, () =>
        ex.call({ service: c.svc, handler: 'inspect', key: `CALLEE-${c.run}` }, {})
      )
      assert.equal(calleeScope?.txKey, undefined, 'call() must not leak caller txKey across invocation boundary')
      assert.equal(calleeScope?.custodyKey, undefined, 'call() must not leak caller custodyKey across invocation boundary')
      assert.ok(calleeScope?.held?.includes(`${c.svc}/caller`), 'held keys still accumulate for deadlock detection')
    }
  }
]

void test('executor matrix: the whole suite against the in-process Dispatcher', async () => {
  for (const s of matrix) {
    const ex = new Dispatcher()
    const svc = 'probe'
    const probe = mkProbe(svc, (t, a) => ex.send(t, a))
    ex.register(svc, 'tick', probe.handler)
    try {
      await s.run({ ex, svc, probe, run: `${RUN}-d` })
    } catch (e) {
      throw new Error(`matrix scenario "${s.name}": ${(e as Error).message}`)
    } finally {
      await ex.close()
    }
  }
})

void test(
  'executor matrix (live restate): the match scenarios against the real ingress + endpoint',
  { skip: RESTATE_URL === undefined ? 'needs NEKTAR_RESTATE_URL (live restate server)' : false },
  async () => {
    const ex = new RestateExecutor(RESTATE_URL!)
    const svc = `probe-${RUN}`
    const probe = mkProbe(svc, (t, a) => ex.send(t, a))
    const endpoint = await openEndpoint([await probeObject(svc, probe.handler)], {
      NEKTAR_RESTATE_ENDPOINT_URI: ENDPOINT_URI,
      NEKTAR_RESTATE_ADMIN: ADMIN,
      NEKTAR_RESTATE_ENDPOINT_PORT: ENDPOINT_PORT
    })
    try {
      for (const s of matrix.filter((x) => x.match)) {
        try {
          await s.run({ ex, svc, probe, run: RUN })
        } catch (e) {
          throw new Error(`matrix scenario "${s.name}": ${(e as Error).message}`)
        }
      }
    } finally {
      await endpoint.close().catch(() => undefined)
    }
  }
)
