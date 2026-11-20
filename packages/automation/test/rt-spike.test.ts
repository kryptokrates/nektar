import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { PrivateKey, P2PKH, Transaction } from '@bsv/sdk'
import { nektarEnv } from '@nektar/core'
import { AutomationStore } from '../src/store.js'
import { WalletCoordinator } from '../src/wallet.js'
import { RestateExecutor, EXEC_SVC, setExecutor } from '../src/rt/exec.js'
import {
  txObject,
  sweepService,
  dealObject,
  custodyObject,
  dealfanService,
  transportObject,
  TRANSPORT_DRAIN_KEY,
  wakeService,
  cronObject,
  legObject,
  CRON_KEY,
  type DealEvent
} from '../src/rt/services.js'
import { openEndpoint } from '../src/rt/endpoint.js'
import { custodySubmit, txEmit } from '../src/rt/emit.js'

// rt-spike — the live proof: a tx leg end-to-end through a REAL restate
// server. Gated on NEKTAR_RESTATE_URL (stack tier / `docker compose up
// restate`); skips otherwise. What it proves: the ingress round-trip
// (call → server → endpoint → journaled ctx.run → emitTxEvent → SQLite
// commit) works against the actual runtime, not just the seam's fakes.

const RESTATE_URL = nektarEnv('NEKTAR_RESTATE_URL')
const ADMIN = nektarEnv('NEKTAR_RESTATE_ADMIN') ?? 'http://127.0.0.1:9070'
const ENDPOINT_URI = nektarEnv('NEKTAR_RESTATE_ENDPOINT_URI') ?? 'http://host.docker.internal:9080'
const ENDPOINT_PORT = nektarEnv('NEKTAR_RESTATE_ENDPOINT_PORT') ?? '9080'

const key = PrivateKey.fromRandom()

async function open() {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx)
  await store.init()
  return { kx, store }
}

const until = async (what: () => Promise<boolean>, ms = 10_000): Promise<void> => {
  const at = Date.now() + ms
  while (Date.now() < at) {
    if (await what()) return
    await new Promise((r) => setTimeout(r, 100))
  }
  throw new Error('until: deadline')
}

test(
  'spike: submission_unknown → submit.success → obs.proven end-to-end through a live restate server',
  { skip: RESTATE_URL === undefined ? 'needs NEKTAR_RESTATE_URL (live restate server)' : false },
  async () => {
    const { kx, store } = await open()
    const t = new Transaction()
    t.addOutput({ satoshis: 1, lockingScript: new P2PKH().lock(key.toPublicKey().toHash()) })
    await store.recordPrepared({
      actionId: 'spike:1',
      operationId: 'op:spike:1',
      kind: 'escrow',
      txid: t.id('hex'),
      rawHex: t.toHex(),
      parents: {},
      feeSats: 1
    })
    const endpoint = await openEndpoint([await txObject({ store, fx: {}, log: () => {} })], {
      NEKTAR_RESTATE_ENDPOINT_URI: ENDPOINT_URI,
      NEKTAR_RESTATE_ADMIN: ADMIN,
      NEKTAR_RESTATE_ENDPOINT_PORT: ENDPOINT_PORT
    })
    const ex = new RestateExecutor(RESTATE_URL!)
    try {
      // leg 1: the submit verdict lands as a journaled call on the tx object
      const w = (await store.walletTx({ actionId: 'spike:1' }))!
      const r1 = await txEmit(ex, store, w, 'submit.unknown', { observation: { at: 1, error: 'transport' }, attempt: true }, {})
      assert.equal(r1.accepted, true)
      assert.equal(r1.next, 'submission_unknown')

      // leg 2: a late verified proof revives — the journaled emit commits
      // through the same object, serialized after leg 1
      const r2 = await txEmit(ex, store, w, 'obs.proven', { guard: 'proof', observation: { at: 2 } }, {})
      assert.equal(r2.accepted, true)
      assert.equal(r2.next, 'confirmed')
      assert.equal((await store.walletTx({ actionId: 'spike:1' }))!.state, 'confirmed')

      // the journal is the audit: the completed invocations above ran
      // journaled through the runtime — inspectable via `restate invocations
      // list` / the :9070 UI (the /query endpoint returns Arrow, not JSON —
      // operator surface, not a contract the test asserts on)
    } finally {
      await endpoint.close().catch(() => undefined)
      await kx.destroy()
    }
  }
)

test(
  'spike: sweep.tick fans observations out through journaled sends — ctx.objectSendClient on the real server',
  { skip: RESTATE_URL === undefined ? 'needs NEKTAR_RESTATE_URL (live restate server)' : false },
  async () => {
    const { kx, store } = await open()
    const t = new Transaction()
    t.addOutput({ satoshis: 1, lockingScript: new P2PKH().lock(key.toPublicKey().toHash()) })
    await store.recordPrepared({
      actionId: 'sweep:1',
      operationId: 'op:sweep:1',
      kind: 'escrow',
      txid: t.id('hex'),
      rawHex: t.toHex(),
      parents: {},
      feeSats: 1
    })
    // the staged scan — tick 1 sees the submit verdict, tick 2 the proof
    const stages = [
      [{ actionId: 'sweep:1', observe: { event: 'submit.unknown', observation: { at: 1 }, attempt: true } }],
      [{ actionId: 'sweep:1', observe: { event: 'obs.proven', guard: 'proof', observation: { at: 2 } } }]
    ]
    const txDef = await txObject({ store, fx: {}, log: () => {} })
    const sweepDef = await sweepService(
      {
        scan: async function* () {
          for (const o of stages.shift() ?? []) yield o
        },
        resubmitRows: async () => [],
        log: () => {}
      },
      txDef
    )
    const endpoint = await openEndpoint([txDef, sweepDef], {
      NEKTAR_RESTATE_ENDPOINT_URI: ENDPOINT_URI,
      NEKTAR_RESTATE_ADMIN: ADMIN,
      NEKTAR_RESTATE_ENDPOINT_PORT: ENDPOINT_PORT
    })
    const ex = new RestateExecutor(RESTATE_URL!)
    try {
      // tick 1: the scan result journals; the row's observe goes out as a
      // journaled send on its own object — async, so poll for the landing
      const r1 = await ex.call<{ sent: number }>({ service: EXEC_SVC.sweep, handler: 'tick' }, {})
      assert.equal(r1.sent, 1)
      await until(async () => (await store.walletTx({ actionId: 'sweep:1' }))?.state === 'submission_unknown')

      const r2 = await ex.call<{ sent: number }>({ service: EXEC_SVC.sweep, handler: 'tick' }, {})
      assert.equal(r2.sent, 1)
      await until(async () => (await store.walletTx({ actionId: 'sweep:1' }))?.state === 'confirmed')
      assert.equal((await store.walletTx({ actionId: 'sweep:1' }))!.state, 'confirmed')
    } finally {
      await endpoint.close().catch(() => undefined)
      await kx.destroy()
    }
  }
)

test(
  'spike: deal.advance lands journaled on the op key — the leg runs domain code in-process',
  { skip: RESTATE_URL === undefined ? 'needs NEKTAR_RESTATE_URL (live restate server)' : false },
  async () => {
    const { kx, store } = await open()
    const { operation } = await store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey: 'deal:spike',
      policyHash: 'p',
      network: 'regtest',
      state: 'collecting',
      data: {}
    })
    // a stand-in advanceOp: loads the op and stamps — proves a keyed
    // invocation reaches domain code in-process, not a mock
    const ran: string[] = []
    const dealDef = await dealObject({
      advanceOp: async (id) => {
        const op = await store.operation(id)
        if (op === undefined) return false
        ran.push(id)
        await store.transition(op.operationId, op.revision, op.state, { cause: 'tick_error', lastError: 'spike leg ran' })
        return true
      },
      inboxEvent: async () => {},
      reconcileOp: async () => {},
      log: () => {}
    })
    const endpoint = await openEndpoint([dealDef], {
      NEKTAR_RESTATE_ENDPOINT_URI: ENDPOINT_URI,
      NEKTAR_RESTATE_ADMIN: ADMIN,
      NEKTAR_RESTATE_ENDPOINT_PORT: ENDPOINT_PORT
    })
    const ex = new RestateExecutor(RESTATE_URL!)
    try {
      const r = await ex.call<{ ran: boolean }>({ service: EXEC_SVC.deal, handler: 'advance', key: operation.operationId }, {})
      assert.equal(r.ran, true)
      assert.deepEqual(ran, [operation.operationId])
      assert.equal((await store.operation(operation.operationId))!.lastError, 'spike leg ran')
    } finally {
      await endpoint.close().catch(() => undefined)
      await kx.destroy()
    }
  }
)

test(
  'spike: custody.submit on the fixed key — broadcast leg journaled, emits route to the tx object',
  { skip: RESTATE_URL === undefined ? 'needs NEKTAR_RESTATE_URL (live restate server)' : false },
  async () => {
    const { kx, store } = await open()
    const t = new Transaction()
    t.addOutput({ satoshis: 1, lockingScript: new P2PKH().lock(key.toPublicKey().toHash()) })
    await store.recordPrepared({
      actionId: 'custody:1',
      operationId: 'op:custody:1',
      kind: 'escrow',
      txid: t.id('hex'),
      rawHex: t.toHex(),
      parents: {},
      feeSats: 1
    })
    const wallet = new WalletCoordinator(store, { broadcast: async (tx) => ({ status: 'success', txid: tx.id('hex') }) })
    const txDef = await txObject({ store, fx: {}, log: () => {} })
    const custodyDef = await custodyObject({ wallet, log: () => {} })
    const endpoint = await openEndpoint([txDef, custodyDef], {
      NEKTAR_RESTATE_ENDPOINT_URI: ENDPOINT_URI,
      NEKTAR_RESTATE_ADMIN: ADMIN,
      NEKTAR_RESTATE_ENDPOINT_PORT: ENDPOINT_PORT
    })
    const ex = new RestateExecutor(RESTATE_URL!)
    try {
      // the deployment topology: emits inside handlers route back through
      // the ingress — custody.submit → wallet.submit → txEmit → tx.observe
      setExecutor(ex)
      const r = await custodySubmit(ex, wallet, 'custody:1')
      assert.equal(r.state, 'submitted')
      await until(async () => (await store.walletTx({ actionId: 'custody:1' }))?.state === 'submitted')
    } finally {
      setExecutor(undefined)
      await endpoint.close().catch(() => undefined)
      await kx.destroy()
    }
  }
)

test(
  'spike: dealfan.tick journals the scans and fans out to deal keys on the real server',
  { skip: RESTATE_URL === undefined ? 'needs NEKTAR_RESTATE_URL (live restate server)' : false },
  async () => {
    const { kx, store } = await open()
    const { operation } = await store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey: 'fan:spike',
      policyHash: 'p',
      network: 'regtest',
      state: 'collecting',
      data: {}
    })
    const landed: Array<{ handler: string; key: string }> = []
    const dealDef = await dealObject({
      advanceOp: async (id) => (landed.push({ handler: 'advance', key: id }), true),
      inboxEvent: async (item) => void landed.push({ handler: 'event', key: item.messageId }),
      reconcileOp: async (id) => void landed.push({ handler: 'reconcile', key: id }),
      log: () => {}
    })
    const items: DealEvent[] = [{ source: 'lane', messageId: 'fm1', kind: 'want', payload: { wantId: 'w-fan' } }]
    const dealfanDef = await dealfanService(
      {
        pendingInbox: async () => items,
        liveOps: async () => [{ operationId: operation.operationId }],
        reconcileOps: async () => [],
        ceilingMs: () => 60_000,
        log: () => {}
      },
      dealDef
    )
    const endpoint = await openEndpoint([dealDef, dealfanDef], {
      NEKTAR_RESTATE_ENDPOINT_URI: ENDPOINT_URI,
      NEKTAR_RESTATE_ADMIN: ADMIN,
      NEKTAR_RESTATE_ENDPOINT_PORT: ENDPOINT_PORT
    })
    const ex = new RestateExecutor(RESTATE_URL!)
    try {
      const r = await ex.call<{ events: number; advanced: number; reconciled: number }>({ service: EXEC_SVC.dealfan, handler: 'tick' }, {})
      assert.deepEqual(r, { events: 1, advanced: 1, reconciled: 0 })
      // the sends are async — poll until both keyed invocations land
      await until(async () => landed.length === 2)
      assert.deepEqual(
        [...landed].sort((a, b) => a.handler.localeCompare(b.handler)),
        [
          { handler: 'advance', key: operation.operationId },
          { handler: 'event', key: 'fm1' }
        ]
      )
    } finally {
      await endpoint.close().catch(() => undefined)
      await kx.destroy()
    }
  }
)

test(
  'spike: transport.drain journals the scan and lands deliver on the destination key',
  { skip: RESTATE_URL === undefined ? 'needs NEKTAR_RESTATE_URL (live restate server)' : false },
  async () => {
    const { kx, store } = await open()
    void store
    const delivered: string[] = []
    // a fresh actionId per run — the deliver send dedupes on
    // `deliver:${actionId}` at the journal; a stable id would dedupe
    // against an earlier run's completed invocation (the dedup working)
    const actionId = `tp:${Date.now()}`
    const transportDef = await transportObject({
      dueRows: async () => [{ actionId, operationId: `op:${actionId}`, kind: 'message', destination: 'host:live', payload: { type: 'decline' }, attempts: 0 }],
      nextAt: async () => undefined,
      deliverItem: async (item) => (delivered.push(`${item.actionId}@${item.destination}`), 'acked' as const),
      now: () => Date.now(),
      log: () => {}
    })
    const endpoint = await openEndpoint([transportDef], {
      NEKTAR_RESTATE_ENDPOINT_URI: ENDPOINT_URI,
      NEKTAR_RESTATE_ADMIN: ADMIN,
      NEKTAR_RESTATE_ENDPOINT_PORT: ENDPOINT_PORT
    })
    const ex = new RestateExecutor(RESTATE_URL!)
    try {
      const r = await ex.call<{ sent: number }>({ service: EXEC_SVC.transport, handler: 'drain', key: TRANSPORT_DRAIN_KEY }, {})
      assert.equal(r.sent, 1)
      await until(async () => delivered.length === 1)
      assert.deepEqual(delivered, [`${actionId}@host:live`])
    } finally {
      await endpoint.close().catch(() => undefined)
      await kx.destroy()
    }
  }
)

test(
  'spike: wake.height journals the due probe and lands the dealfan tick via a journaled send',
  { skip: RESTATE_URL === undefined ? 'needs NEKTAR_RESTATE_URL (live restate server)' : false },
  async () => {
    const { kx, store } = await open()
    void store
    // the wake's ctx send targets nektar-dealfan — both services ride the
    // same endpoint; the tick's scan deps record having run server-side
    const fanned: string[] = []
    const dealfanDef = await dealfanService(
      {
        pendingInbox: async () => (fanned.push('inbox'), [] as DealEvent[]),
        liveOps: async () => (fanned.push('ops'), []),
        reconcileOps: async () => (fanned.push('reconcile'), []),
        ceilingMs: () => 60_000,
        log: () => {}
      },
      undefined
    )
    const wakeDef = await wakeService({
      legs: { dueAtHeight: async (tip) => tip >= 100 },
      hint: () => {},
      log: () => {}
    })
    const endpoint = await openEndpoint([dealfanDef, wakeDef], {
      NEKTAR_RESTATE_ENDPOINT_URI: ENDPOINT_URI,
      NEKTAR_RESTATE_ADMIN: ADMIN,
      NEKTAR_RESTATE_ENDPOINT_PORT: ENDPOINT_PORT
    })
    const ex = new RestateExecutor(RESTATE_URL!)
    try {
      const r = await ex.call<{ due: boolean }>({ service: EXEC_SVC.wake, handler: 'height' }, { tip: 150 })
      assert.equal(r.due, true)
      // the journaled send lands async — the dealfan tick running its three
      // scans is the witness that the chain completed end-to-end
      await until(async () => fanned.length === 3)
      assert.deepEqual(fanned, ['inbox', 'ops', 'reconcile'])
      // a below-tip wake gates out — no fan-out follows
      const before = fanned.length
      assert.equal((await ex.call<{ due: boolean }>({ service: EXEC_SVC.wake, handler: 'height' }, { tip: 50 })).due, false)
      await new Promise((r) => setTimeout(r, 500))
      assert.equal(fanned.length, before)
    } finally {
      await endpoint.close().catch(() => undefined)
      await kx.destroy()
    }
  }
)

test(
  'spike: cron.tick journals the pass and self-arms — the durable cadence chain re-fires',
  { skip: RESTATE_URL === undefined ? 'needs NEKTAR_RESTATE_URL (live restate server)' : false },
  async () => {
    const { kx, store } = await open()
    void store
    const ticks: number[] = []
    const upkeepDef = await cronObject(EXEC_SVC.upkeep, {
      tick: async () => void ticks.push(Date.now()),
      ceilingMs: () => 200,
      log: () => {}
    })
    const endpoint = await openEndpoint([upkeepDef], {
      NEKTAR_RESTATE_ENDPOINT_URI: ENDPOINT_URI,
      NEKTAR_RESTATE_ADMIN: ADMIN,
      NEKTAR_RESTATE_ENDPOINT_PORT: ENDPOINT_PORT
    })
    const ex = new RestateExecutor(RESTATE_URL!)
    try {
      await ex.call({ service: EXEC_SVC.upkeep, handler: 'tick', key: CRON_KEY }, {})
      assert.equal(ticks.length, 1)
      // the self-arm is a durable delayed send — the chain re-fires without
      // any process-side timer owning it
      await until(async () => ticks.length >= 2)
    } finally {
      await endpoint.close().catch(() => undefined)
      await kx.destroy()
    }
  }
)

test(
  'spike: leg object — the host event-sink runs journaled on its key through ingress calls',
  { skip: RESTATE_URL === undefined ? 'needs NEKTAR_RESTATE_URL (live restate server)' : false },
  async () => {
    const { kx, store } = await open()
    void store
    const legs: string[] = []
    const watcherDef = await legObject(EXEC_SVC.watcher, async () => void legs.push(`t${legs.length}`))
    const endpoint = await openEndpoint([watcherDef], {
      NEKTAR_RESTATE_ENDPOINT_URI: ENDPOINT_URI,
      NEKTAR_RESTATE_ADMIN: ADMIN,
      NEKTAR_RESTATE_ENDPOINT_PORT: ENDPOINT_PORT
    })
    const ex = new RestateExecutor(RESTATE_URL!)
    try {
      // the event-plane wake shape: ingress call, completes when the leg does
      await ex.call({ service: EXEC_SVC.watcher, handler: 'tick', key: 'main' }, {})
      await ex.call({ service: EXEC_SVC.watcher, handler: 'tick', key: 'main' }, {})
      assert.deepEqual(legs, ['t0', 't1'])
    } finally {
      await endpoint.close().catch(() => undefined)
      await kx.destroy()
    }
  }
)
