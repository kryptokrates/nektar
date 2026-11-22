import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFile } from 'node:child_process'
import { promisify } from 'node:util'
import { RestateExecutor, EXEC_SVC } from '../src/rt/exec.js'
import { transportObject, cronObject, TRANSPORT_DRAIN_KEY, CRON_KEY } from '../src/rt/services.js'
import type { RestateTestEnvironment } from '@restatedev/restate-sdk-testcontainers'

// rt-replay — the alwaysReplay determinism hunt + the disableRetries error
// path on Testcontainers-spawned servers (each test gets its own restate).
//
// What alwaysReplay actually does (RESTATE_WORKER__INVOKER__INACTIVITY_
// TIMEOUT=0s): the invocation suspends the moment it hits a real
// suspension point — ctx.sleep, an awakeable, a call response — and the
// resume replays the journal from the top. Journaled steps must skip;
// unjournaled code re-executes.
//
// Consequence for the plane: our fan-out bodies are suspension-free
// between journaled steps (send + ctx.run don't suspend) — alwaysReplay
// can't engage on them. Their replay coverage is the endpoint-loss kill
// drill in rt-live. What runs here:
//   - a probe WITH ctx.sleep suspension points — the actual determinism
//     drill: executions>1 proves replays happened; journaled marks and a
//     journaled send firing exactly once prove determinism held
//   - the real plane defs served under the environment's own endpoint
//     host — registration/invocation integration (a different seam than
//     our openEndpoint path)
//   - disableRetries: a deterministic throw surfaces immediately instead
//     of hanging in backoff
//
// Gate: docker reachable — skips otherwise.

const run = promisify(execFile)
const nap = (ms: number) => new Promise((r) => setTimeout(r, ms))

const until = async (what: () => Promise<boolean>, ms = 30_000): Promise<void> => {
  const at = Date.now() + ms
  while (Date.now() < at) {
    if (await what()) return
    await nap(150)
  }
  throw new Error('until: deadline')
}

const dockerUp = async (): Promise<boolean> =>
  await run('docker', ['version', '--format', '{{.Server.Version}}']).then(
    () => true,
    () => false
  )

const startEnv = async (t: { skip: (m?: string) => void }, services: unknown[], opts: Record<string, unknown>): Promise<RestateTestEnvironment | undefined> => {
  const { RestateTestEnvironment } = await import('@restatedev/restate-sdk-testcontainers')
  try {
    return await RestateTestEnvironment.start({ services: services as never[], ...opts } as never)
  } catch (e) {
    t.skip(`restate test environment unavailable: ${e instanceof Error ? e.message : String(e)}`)
    return undefined
  }
}

const RUN = `${process.pid}-${Date.now().toString(36)}`

test('replay: suspension probe — replays re-run the body but journaled steps and sends fire once', { timeout: 120_000 }, async (t) => {
  if (!(await dockerUp())) {
    t.skip('docker unavailable')
    return
  }
  const restate = await import('@restatedev/restate-sdk')
  const svc = `replay-probe-${RUN}`
  const marks: string[] = []
  const sent: string[] = []
  let executions = 0
  const ledger = restate.object({
    name: `replay-ledger-${RUN}`,
    handlers: {
      ping: async (ctx: import('@restatedev/restate-sdk').ObjectContext) => {
        await ctx.run('ping', () => void sent.push(ctx.key))
        return { done: true }
      }
    }
  })
  const probe = restate.object({
    name: svc,
    handlers: {
      go: async (ctx: import('@restatedev/restate-sdk').ObjectContext) => {
        executions++
        await ctx.run('s1', () => void marks.push('s1'))
        // a real suspension point — with INACTIVITY_TIMEOUT=0s the
        // invocation suspends here and resumes by replaying the journal
        await ctx.sleep(50)
        const ledgerClient = ctx.objectSendClient({ name: `replay-ledger-${RUN}` } as never, 'l') as unknown as {
          ping: (o: unknown) => Promise<void>
        }
        await ledgerClient.ping({})
        await ctx.sleep(50)
        await ctx.run('s2', () => void marks.push('s2'))
        return { done: true }
      }
    }
  })
  const env = await startEnv(t, [probe, ledger], { alwaysReplay: true })
  if (env === undefined) return
  try {
    const ex = new RestateExecutor(env.baseUrl())
    const r = await ex.call<{ done: boolean }>({ service: svc, handler: 'go', key: 'k' }, null)
    assert.equal(r.done, true)
    await until(async () => sent.length === 1)
    assert.ok(executions >= 2, `expected forced replays at the sleeps — got ${executions} execution(s) (replay may not have engaged)`)
    assert.deepEqual(marks, ['s1', 's2'], 'journaled steps executed exactly once — replays consumed the journal')
    assert.equal(sent.length, 1, 'the journaled send issued once — replay deduped it')
  } finally {
    await env.stop().catch(() => undefined)
  }
})

test('replay: plane defs complete under the testcontainer endpoint host — transport.drain sends + arm', { timeout: 120_000 }, async (t) => {
  if (!(await dockerUp())) {
    t.skip('docker unavailable')
    return
  }
  let scans = 0
  let nexts = 0
  const delivered: string[] = []
  const transportDef = await transportObject({
    dueRows: async () => (scans++, [{ actionId: 'rp:1', operationId: 'op:1', kind: 'message', destination: `rp:host-${RUN}`, payload: {}, attempts: 0 }]),
    nextAt: async () => (nexts++, Date.now() + 60_000),
    deliverItem: async (item) => (delivered.push(item.actionId), 'acked' as const),
    now: () => Date.now(),
    log: () => {}
  })
  const env = await startEnv(t, [transportDef], { alwaysReplay: true })
  if (env === undefined) return
  try {
    const ex = new RestateExecutor(env.baseUrl())
    const r = await ex.call<{ sent: number }>({ service: EXEC_SVC.transport, handler: 'drain', key: TRANSPORT_DRAIN_KEY }, {})
    assert.equal(r.sent, 1)
    await until(async () => delivered.length === 1)
    assert.equal(scans, 1)
    assert.equal(nexts, 1)
    assert.equal(delivered.length, 1)
  } finally {
    await env.stop().catch(() => undefined)
  }
})

test('replay: plane defs complete under the testcontainer endpoint host — cron.tick self-arms once', { timeout: 120_000 }, async (t) => {
  if (!(await dockerUp())) {
    t.skip('docker unavailable')
    return
  }
  const ticks: number[] = []
  const svc = `replay-cron-${RUN}`
  const cronDef = await cronObject(svc, {
    tick: async () => void ticks.push(Date.now()),
    ceilingMs: () => 60_000,
    now: () => Date.now(),
    log: () => {}
  })
  const env = await startEnv(t, [cronDef], { alwaysReplay: true })
  if (env === undefined) return
  try {
    const ex = new RestateExecutor(env.baseUrl())
    await ex.call({ service: svc, handler: 'tick', key: CRON_KEY }, {})
    assert.equal(ticks.length, 1)
    await nap(1200)
    assert.equal(ticks.length, 1, 'the arm sent once — no duplicate chain')
  } finally {
    await env.stop().catch(() => undefined)
  }
})

test('replay: disableRetries — a poisoned leg surfaces immediately instead of hanging in backoff', { timeout: 60_000 }, async (t) => {
  if (!(await dockerUp())) {
    t.skip('docker unavailable')
    return
  }
  const restate = await import('@restatedev/restate-sdk')
  const svc = `replay-poison-${RUN}`
  const poison = restate.object({
    name: svc,
    handlers: {
      go: async (_ctx: import('@restatedev/restate-sdk').ObjectContext) => {
        throw new Error('deterministic poison — must surface, not wedge')
      }
    }
  })
  const env = await startEnv(t, [poison], { disableRetries: true })
  if (env === undefined) return
  try {
    const ex = new RestateExecutor(env.baseUrl())
    await assert.rejects(ex.call({ service: svc, handler: 'go', key: 'k' }, null), /poison|failed|error/i)
  } finally {
    await env.stop().catch(() => undefined)
  }
})
