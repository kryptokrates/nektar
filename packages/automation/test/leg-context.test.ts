import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { currentSignal, deadline, detached, nap } from '@nektar/core'
import { AutomationStore } from '../src/store.js'
import { LocalExecutor } from '../src/rt/exec.js'

// leg-context — the AsyncLocalStorage deadline contract at the automation
// seams: post-commit hooks and spawned work run OUTSIDE the leg that
// happened to commit/fire them; awaited IO inside a leg inherits the
// operation deadline by construction. A regression here is the
// abandon-holds-resource / stacked-deadline class returning silently.

const open = async () => {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx)
  await store.init()
  return { kx, store }
}

const mkOp = {
  role: 'buyer' as const,
  kind: 'purchase',
  dedupeKey: 'buyer:w1',
  policyHash: 'h',
  network: 'regtest',
  state: 'collecting',
  data: {}
}

test('store post-commit hooks fire detached — a workReady spawn survives the leg', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  let ambientInHook: AbortSignal | undefined | 'unset' = 'unset'
  let spawnSettled = false
  // detached dispatch is ASYNC by design — await the hook/spawn landing
  // instead of racing it (a fixed sleep flakes under suite CPU contention)
  let hookFired!: () => void
  let spawnDone!: () => void
  const fired = new Promise<void>((r) => {
    hookFired = r
  })
  const settled = new Promise<void>((r) => {
    spawnDone = r
  })
  store.workReady = () => {
    ambientInHook = currentSignal()
    hookFired()
    // what the runtime's hook does: launch a pass — it must outlive the leg
    void nap(30).then(() => {
      spawnSettled = true
      spawnDone()
    })
  }
  const r = await deadline(async () => {
    await store.createOperation(mkOp)
    await nap(60_000)
    return 'never'
  }, 25)
  assert.deepEqual(r, { outcome: 'timeout' })
  assert.equal((await deadline(() => fired, 5_000)).outcome, 'settled', 'the post-commit hook fired')
  assert.equal(ambientInHook, undefined, "the hook ran outside the committer's leg — a commit inside a leg must not hand its deadline to spawned work")
  assert.equal((await deadline(() => settled, 5_000)).outcome, 'settled', 'the spawned pass completed after the spawning leg died')
  assert.equal(spawnSettled, true)
})

test('executor send dispatch is detached — a send armed inside a leg cannot leak its context', async () => {
  const ex = new LocalExecutor()
  let ambientInJob: AbortSignal | undefined | 'unset' = 'unset'
  let jobDone!: () => void
  const ran = new Promise<void>((r) => {
    jobDone = r
  })
  ex.register('nektar-test', 'tick', async () => {
    ambientInJob = currentSignal()
    jobDone()
  })
  const r = await deadline(async () => {
    void ex.send({ service: 'nektar-test', handler: 'tick' }, {}, { delayMs: 1 }) // armed inside the leg — timers capture context at creation
    await nap(60_000)
    return 'never'
  }, 30)
  assert.deepEqual(r, { outcome: 'timeout' })
  assert.equal((await deadline(() => ran, 5_000)).outcome, 'settled', 'the executor dispatched the send')
  assert.equal(ambientInJob, undefined, 'timer-dispatched work is never the armer’s child — sends run detached')
  ex.close()
})

test('the run() mutex shape: promise links keep their registrant’s context', async () => {
  // base.ts serializes via `this.chain = link.then(fn)` — each link's
  // context is captured at registration, so serialized legs stay isolated
  let chain: Promise<void> = Promise.resolve()
  let aSig: AbortSignal | undefined
  let bSig: AbortSignal | undefined
  const mkLink = (into: (s: AbortSignal | undefined) => void) => {
    chain = chain.then(() => {
      into(currentSignal())
    })
  }
  const [a, b] = await Promise.all([
    deadline(async () => {
      mkLink((s) => {
        aSig = s
      })
      await nap(10)
      return 'a'
    }, 5_000),
    deadline(async () => {
      mkLink((s) => {
        bSig = s
      })
      await nap(60_000)
      return 'b-never'
    }, 25)
  ])
  await chain // both links ran
  assert.deepEqual(a, { outcome: 'settled', value: 'a' })
  assert.deepEqual(b, { outcome: 'timeout' })
  assert.ok(aSig !== undefined && bSig !== undefined)
  assert.notEqual(aSig, bSig, 'serialized links never see each other’s leg')
})

test('knex/sqlite round-trips keep the ambient — a store call inside a leg sees the context after IO', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  let afterDb: AbortSignal | undefined
  const r = await deadline(async () => {
    await store.operations({ states: ['collecting'] })
    afterDb = currentSignal()
    return 'ok'
  }, 5_000)
  assert.deepEqual(r, { outcome: 'settled', value: 'ok' })
  assert.ok(afterDb !== undefined, 'the leg context survives awaited knex/sqlite work')
})

test('detached() inside leg work yields context-free spawned work', async () => {
  let spawnedAmbient: AbortSignal | undefined | 'unset' = 'unset'
  const r = await deadline(async () => {
    detached(() => {
      spawnedAmbient = currentSignal()
    })
    return 'ok'
  }, 5_000)
  assert.deepEqual(r, { outcome: 'settled', value: 'ok' })
  assert.equal(spawnedAmbient, undefined)
})
