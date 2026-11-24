import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { AutomationStore } from '../src/store.js'
import { inertTxEffects } from '../src/machines/effects.js'
import { openHostPlane, openLocalHostPlane } from '../src/rt/hostplane.js'
import { svcName } from '../src/rt/endpoint.js'
import { EXEC_SVC, executor, type Executor, type InvokeTarget } from '../src/rt/exec.js'

// host-plane — the automation-less registration path. Fakes sit at the two
// seams only: `open` stands in for openEndpoint (records the service names
// it was handed with the endpoint's own svcName extraction — never a
// re-derived copy) and `exec` stands in for the ambient executor (records
// sends + ordering vs the endpoint coming up). The tx object itself is the
// real def txObject() builds — wiring it end-to-end needs a running restate
// and is deliberately NOT faked here.

const store = async () => {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const s = new AutomationStore(kx)
  await s.init()
  return { kx, s }
}

/** host defs shaped like the SDK defs confirmer/watch build — svcName reads
 *  `definition.name ?? name`. */
const hostDef = (name: string) => ({ name })

void test('host plane: registers nektar-tx + every host def, then fires each kick exactly once', async () => {
  const { kx, s } = await store()
  const names: string[] = []
  let closes = 0
  let openResolved = false
  const sends: InvokeTarget[] = []
  const exec: Executor = {
    send: async (t) => {
      assert.equal(openResolved, true, `kick ${t.service} sent before the endpoint was up`)
      sends.push(t)
    },
    call: async () => {
      throw new Error('no calls')
    }
  }
  const open = async (services: unknown[]) => {
    for (const d of services) names.push(svcName(d) ?? '?')
    openResolved = true
    return { uri: 'test://ep', close: async () => void closes++ }
  }
  const rtDefs = [hostDef('nektar-confirmer'), hostDef('nektar-watcher')]
  const rtKicks: InvokeTarget[] = [
    { service: 'nektar-confirmer', handler: 'tick', key: 'confirmer' },
    { service: 'nektar-watcher', handler: 'leg', key: 'watcher' }
  ]
  const hp = await openHostPlane({ store: s, txEffects: inertTxEffects, rtDefs, rtKicks, log: () => {}, open, exec })
  assert.equal(hp.uri, 'test://ep')
  assert.deepEqual(names.sort(), [EXEC_SVC.tx, 'nektar-confirmer', 'nektar-watcher'].sort())
  assert.equal(sends.length, rtKicks.length)
  assert.deepEqual(sends, rtKicks)
  await hp.stop()
  assert.equal(closes, 1)
  await kx.destroy()
})

void test('host plane: a failing endpoint registration rejects and no kick is sent', async () => {
  const { kx, s } = await store()
  const sends: InvokeTarget[] = []
  const exec: Executor = {
    send: async (t) => void sends.push(t),
    call: async () => {
      throw new Error('no calls')
    }
  }
  const open = async () => {
    throw new Error('admin down')
  }
  await assert.rejects(
    openHostPlane({
      store: s,
      txEffects: inertTxEffects,
      rtDefs: [hostDef('nektar-confirmer')],
      rtKicks: [{ service: 'nektar-confirmer', handler: 'tick' }],
      log: () => {},
      open,
      exec
    }),
    /admin down/
  )
  assert.equal(sends.length, 0)
  await kx.destroy()
})

void test('local host plane: boots in-process without Restate, wires tx + localWire, fires kicks, and cleans up executor', async () => {
  const { kx, s } = await store()
  let kicked = 0
  const hp = await openLocalHostPlane({
    store: s,
    txEffects: inertTxEffects,
    localWire: (ex) => {
      ex.register(EXEC_SVC.confirmer, 'tick', () => {
        kicked++
        return Promise.resolve({ ok: true })
      })
    },
    rtKicks: [{ service: EXEC_SVC.confirmer, handler: 'tick', key: 'main' }],
    log: () => {}
  })
  assert.equal(hp.uri, 'local://in-process')
  await new Promise((r) => setTimeout(r, 25))
  assert.equal(kicked, 1, 'boot kick ran in-process on local Dispatcher')
  // ambient executor() routes directly to our local Dispatcher
  const direct = await executor().call<{ ok: boolean }>({ service: EXEC_SVC.confirmer, handler: 'tick', key: 'main' }, {})
  assert.equal(direct.ok, true)
  assert.equal(kicked, 2)
  await hp.stop()
  await kx.destroy()
})
