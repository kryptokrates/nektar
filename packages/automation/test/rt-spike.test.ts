import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { PrivateKey, P2PKH, Transaction } from '@bsv/sdk'
import { nektarEnv } from '@nektar/core'
import { AutomationStore } from '../src/store.js'
import { RestateExecutor, EXEC_SVC } from '../src/rt/exec.js'
import { txObject, sweepService, type SweepDeps } from '../src/rt/services.js'
import { openEndpoint } from '../src/rt/endpoint.js'
import { txEmit } from '../src/rt/emit.js'

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
