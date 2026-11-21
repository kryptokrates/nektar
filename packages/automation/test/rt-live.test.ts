import { execFile } from 'node:child_process'
import { promisify } from 'node:util'
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { nektarEnv } from '@nektar/core'
import { RestateExecutor } from '../src/rt/exec.js'
import { legObject } from '../src/rt/services.js'
import { openEndpoint, type RtEndpoint } from '../src/rt/endpoint.js'

// rt-live — the durability drills the Dispatcher can't model, on a REAL
// restate server: journal replay across endpoint loss, ingress idempotency,
// per-key FIFO, delayed sends surviving a server restart. Gated on
// NEKTAR_RESTATE_URL the same way rt-spike is; a local one-liner stands the
// server up:
//   docker run -d --name nektar-rt-test -p 28511:8080 -p 28521:9070 \
//     docker.restate.dev/restatedev/restate:latest
//   NEKTAR_RESTATE_URL=http://127.0.0.1:28511 \
//   NEKTAR_RESTATE_ADMIN=http://127.0.0.1:28521 \
//     node --test packages/automation/dist/test/rt-live.test.js
// The endpoint binds on the host; host.docker.internal lets the container
// reach back (Docker Desktop). D4 spawns its own throwaway container so the
// restart doesn't disturb the shared one.

const RESTATE_URL = nektarEnv('NEKTAR_RESTATE_URL')
const ADMIN = nektarEnv('NEKTAR_RESTATE_ADMIN') ?? 'http://127.0.0.1:9070'
const ENDPOINT_PORT = nektarEnv('NEKTAR_LIVE_ENDPOINT_PORT') ?? '9081'
const ENDPOINT_URI = nektarEnv('NEKTAR_LIVE_ENDPOINT_URI') ?? `http://host.docker.internal:${ENDPOINT_PORT}`
const GATE = RESTATE_URL === undefined ? 'needs NEKTAR_RESTATE_URL (live restate server)' : false
const run = promisify(execFile)
const nap = (ms: number) => new Promise((r) => setTimeout(r, ms))
// restate state is durable across runs: a consumed idempotency key or a
// leftover self-arm chain from a previous suite run is still live on the
// server. Every test service name carries a per-run suffix so ghost traffic
// targets unregistered names and dies invisibly instead of polluting this
// run's counters.
const RUN = `${process.pid}-${Date.now().toString(36)}`

const env = () => ({
  NEKTAR_RESTATE_ENDPOINT_URI: ENDPOINT_URI,
  NEKTAR_RESTATE_ADMIN: ADMIN,
  NEKTAR_RESTATE_ENDPOINT_PORT: ENDPOINT_PORT
})

const until = async (what: () => Promise<boolean>, ms = 30_000): Promise<void> => {
  const at = Date.now() + ms
  while (Date.now() < at) {
    if (await what()) return
    await nap(150)
  }
  throw new Error('until: deadline')
}

const health = async (url: string): Promise<boolean> =>
  (await fetch(`${url}/restate/health`, { signal: AbortSignal.timeout(3000) }).catch(() => undefined))?.ok === true

test('live: journal replay across endpoint loss — ctx.run steps never re-fire', { skip: GATE, timeout: 120_000 }, async () => {
  const restate = await import('@restatedev/restate-sdk')
  const marks: string[] = []
  let executions = 0
  // a two-step handler with a hang between the journaled steps: the first
  // execution is killed mid-flight; the retry replays s1 from the journal
  // (never re-executes it) and completes s2 exactly once.
  const probe = restate.object({
    name: `live-probe-${RUN}`,
    handlers: {
      go: async (ctx: import('@restatedev/restate-sdk').ObjectContext) => {
        await ctx.run('s1', () => void marks.push('s1'))
        if (++executions === 1) await new Promise(() => {})
        await ctx.run('s2', () => void marks.push('s2'))
        return { done: true }
      }
    }
  })
  const ex = new RestateExecutor(RESTATE_URL!)
  let ep: RtEndpoint | undefined
  try {
    ep = await openEndpoint([probe], env(), () => {})
    // the invocation is fire-and-watch: the call hangs at the gate; what
    // matters is the handler's marks
    void ex.call({ service: `live-probe-${RUN}`, handler: 'go', key: 'k' }, null).catch(() => undefined)
    await until(async () => marks.includes('s1'))
    await ep.close()
    ep = undefined
    // the delivery died mid-invocation — the server retries on the same
    // deployment once a live endpoint exists again. The retry's delivery
    // backoff can stretch under suite load — give it a wide window
    ep = await openEndpoint([probe], env(), () => {})
    await until(async () => marks.includes('s2'), 90_000)
    assert.deepEqual(marks, ['s1', 's2'], 'the journaled step ran exactly once — replay skipped it')
  } finally {
    await ep?.close().catch(() => undefined)
  }
})

test('live: ingress idempotency-key dedups a repeat send to one invocation', { skip: GATE, timeout: 30_000 }, async () => {
  let runs = 0
  const def = await legObject(`live-idem-${RUN}`, async () => void runs++)
  const ex = new RestateExecutor(RESTATE_URL!)
  let ep: RtEndpoint | undefined
  try {
    ep = await openEndpoint([def], env(), () => {})
    const t = { service: `live-idem-${RUN}`, handler: 'tick', key: 'main' } as const
    await ex.send(t, {}, { idempotencyKey: 'K1' })
    await ex.send(t, {}, { idempotencyKey: 'K1' })
    await until(async () => runs >= 1)
    await nap(1500)
    assert.equal(runs, 1, 'the repeat send attached to the same invocation — one execution')
    // a NEW key is a NEW invocation — dedup scopes to the key, not the body
    await ex.send(t, {}, { idempotencyKey: 'K2' })
    await until(async () => runs >= 2)
    assert.equal(runs, 2)
  } finally {
    await ep?.close().catch(() => undefined)
  }
})

test('live: per-key FIFO — same-key invocations serialize, cross-key runs concurrent', { skip: GATE, timeout: 60_000 }, async () => {
  const restate = await import('@restatedev/restate-sdk')
  const order: string[] = []
  const probe = restate.object({
    name: `live-fifo-${RUN}`,
    handlers: {
      go: async (ctx: import('@restatedev/restate-sdk').ObjectContext) => {
        await ctx.run('s1', () => void order.push(`s1:${ctx.key}`))
        await nap(120)
        await ctx.run('s2', () => void order.push(`s2:${ctx.key}`))
        return { done: true }
      }
    }
  })
  const ex = new RestateExecutor(RESTATE_URL!)
  let ep: RtEndpoint | undefined
  try {
    ep = await openEndpoint([probe], env(), () => {})
    // same key twice — the second waits behind the first
    const a1 = ex.call({ service: `live-fifo-${RUN}`, handler: 'go', key: 'a' }, null)
    const a2 = ex.call({ service: `live-fifo-${RUN}`, handler: 'go', key: 'a' }, null)
    await Promise.all([a1, a2])
    assert.deepEqual(order.slice(0, 4), ['s1:a', 's2:a', 's1:a', 's2:a'], 'same-key invocations serialized')
    // cross-key concurrency — b's s1 lands before a's s2 if truly parallel;
    // a and b run on independent keys
    order.length = 0
    const slow = ex.call({ service: `live-fifo-${RUN}`, handler: 'go', key: 'a' }, null)
    const fast = ex.call({ service: `live-fifo-${RUN}`, handler: 'go', key: 'b' }, null)
    await Promise.all([slow, fast])
    assert.equal(order[0], 's1:a')
    assert.equal(order[1], 's1:b', 'a different key did not queue behind a')
  } finally {
    await ep?.close().catch(() => undefined)
  }
})

test('live: an armed delayed send outlives its origin invocation', { skip: GATE, timeout: 30_000 }, async () => {
  const restate = await import('@restatedev/restate-sdk')
  let ledgerRuns = 0
  const ledger = await legObject(`live-arm-ledger-${RUN}`, async () => void ledgerRuns++)
  const armer = restate.object({
    name: `live-armer-${RUN}`,
    handlers: {
      go: async (ctx: import('@restatedev/restate-sdk').ObjectContext) => {
        const c = ctx.objectSendClient({ name: `live-arm-ledger-${RUN}` } as never, 'main') as unknown as {
          tick: (o: unknown, opts?: unknown) => Promise<void>
        }
        await c.tick({}, restate.rpc.sendOpts({ delay: 400 }))
        return { armed: true }
      }
    }
  })
  const ex = new RestateExecutor(RESTATE_URL!)
  let ep: RtEndpoint | undefined
  try {
    ep = await openEndpoint([armer, ledger], env(), () => {})
    const r = await ex.call<{ armed: boolean }>({ service: `live-armer-${RUN}`, handler: 'go', key: 'k' }, null)
    assert.equal(r.armed, true)
    // the origin invocation is DONE — its detached delayed send still lands
    await until(async () => ledgerRuns >= 1)
    assert.equal(ledgerRuns, 1)
  } finally {
    await ep?.close().catch(() => undefined)
  }
})

test('live: server restart — the armed delayed send survives the durable timer store', { skip: GATE, timeout: 90_000 }, async (t) => {
  // own container so the restart can't disturb the shared test server;
  // skips when docker isn't reachable
  const name = `nektar-rt-live-${process.pid}`
  const ingress = Number(nektarEnv('NEKTAR_LIVE_INGRESS_PORT') ?? 28531)
  const admin = Number(nektarEnv('NEKTAR_LIVE_ADMIN_PORT') ?? 28541)
  const port = '9082'
  const docker = await run('docker', ['version', '--format', '{{.Server.Version}}']).then(
    () => true,
    () => false
  )
  if (!docker) {
    t.skip('docker unavailable — server-restart drill needs docker')
    return
  }
  try {
    await run('docker', ['rm', '-f', name]).catch(() => undefined)
    await run('docker', ['run', '-d', '--name', name, '-p', `${ingress}:8080`, '-p', `${admin}:9070`, 'docker.restate.dev/restatedev/restate:latest'])
    await until(async () => health(`http://127.0.0.1:${ingress}`), 30_000)
    const liveEnv = {
      NEKTAR_RESTATE_ENDPOINT_URI: `http://host.docker.internal:${port}`,
      NEKTAR_RESTATE_ADMIN: `http://127.0.0.1:${admin}`,
      NEKTAR_RESTATE_ENDPOINT_PORT: port
    }
    const restate = await import('@restatedev/restate-sdk')
    let ledgerRuns = 0
    const ledger = await legObject(`live-restart-ledger-${RUN}`, async () => void ledgerRuns++)
    const armer = restate.object({
      name: `live-restart-armer-${RUN}`,
      handlers: {
        go: async (ctx: import('@restatedev/restate-sdk').ObjectContext) => {
          const c = ctx.objectSendClient({ name: `live-restart-ledger-${RUN}` } as never, 'main') as unknown as {
            tick: (o: unknown, opts?: unknown) => Promise<void>
          }
          await c.tick({}, restate.rpc.sendOpts({ delay: 3000 }))
          return { armed: true }
        }
      }
    })
    const ex = new RestateExecutor(`http://127.0.0.1:${ingress}`)
    let ep: RtEndpoint | undefined
    try {
      ep = await openEndpoint([armer, ledger], liveEnv, () => {})
      await ex.call({ service: `live-restart-armer-${RUN}`, handler: 'go', key: 'k' }, null)
      // restart the SERVER mid-timer — the armed send must survive
      await run('docker', ['restart', name])
      await until(async () => health(`http://127.0.0.1:${ingress}`), 30_000)
      await until(async () => ledgerRuns >= 1, 30_000)
      assert.equal(ledgerRuns, 1, 'the delayed send fired after the server restarted')
    } finally {
      await ep?.close().catch(() => undefined)
    }
  } finally {
    await run('docker', ['rm', '-f', name]).catch(() => undefined)
  }
})
