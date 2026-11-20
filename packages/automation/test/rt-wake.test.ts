// rt-wake.test.ts — the wake service bodies through a LocalExecutor: the
// mem.onTx*/onHeight/onReorg callbacks' durable shape. The leg is journaled
// work; the cycle tail leaves through journaled sends (dealfan.tick +
// transport.drain) — the test asserts both halves land.
import test from 'node:test'
import assert from 'node:assert/strict'
import { LocalExecutor, EXEC_SVC } from '../src/rt/exec.js'
import { registerWakeHandlers, TRANSPORT_DRAIN_KEY, type WakeDeps } from '../src/rt/services.js'

interface Seen {
  service: string
  handler: string
  key?: string
  arg: unknown
}

const rig = (deps: WakeDeps): { ex: LocalExecutor; seen: Seen[] } => {
  const ex = new LocalExecutor()
  const seen: Seen[] = []
  // the sends the wake handlers fan out — recorded so the test sees the
  // cycle tail, registered so LocalExecutor has a route for them
  ex.register(EXEC_SVC.dealfan, 'tick', async (arg, key) => void seen.push({ service: EXEC_SVC.dealfan, handler: 'tick', key, arg }))
  ex.register(EXEC_SVC.transport, 'drain', async (arg, key) => void seen.push({ service: EXEC_SVC.transport, handler: 'drain', key, arg }))
  registerWakeHandlers(ex, deps)
  return { ex, seen }
}

test('wake.txProof: leg runs journaled, hint fires, cycle tail sends dealfan.tick + transport.drain', async () => {
  const legs: string[] = []
  const hinted: string[] = []
  const { ex, seen } = rig({
    legs: {
      txProof: async (txid, mpHex) => void legs.push(`${txid}:${mpHex.slice(0, 8)}`)
    },
    hint: (txid) => void hinted.push(txid),
    log: () => {}
  })
  const r = await ex.call<{ done: boolean }>({ service: EXEC_SVC.wake, handler: 'txProof' }, { txid: 'tx1', mpHex: 'aabbccdd'.repeat(10) })
  await ex.drain()
  assert.deepEqual(r, { done: true })
  assert.deepEqual(legs, ['tx1:aabbccdd'])
  assert.deepEqual(hinted, ['tx1'])
  assert.deepEqual(
    seen.map((s) => `${s.service}/${s.handler}:${s.key ?? '-'}`),
    [`${EXEC_SVC.dealfan}/tick:-`, `${EXEC_SVC.transport}/drain:${TRANSPORT_DRAIN_KEY}`]
  )
  ex.close()
})

test('wake.txRejected: hint + cycle tail, no ledger leg', async () => {
  const hinted: string[] = []
  const { ex, seen } = rig({ legs: {}, hint: (txid) => void hinted.push(txid), log: () => {} })
  const r = await ex.call<{ done: boolean }>({ service: EXEC_SVC.wake, handler: 'txRejected' }, { txid: 'tx2' })
  await ex.drain()
  assert.deepEqual(r, { done: true })
  assert.deepEqual(hinted, ['tx2'])
  assert.equal(seen.length, 2)
  ex.close()
})

test('wake.height: due gate — dealfan only when a row is height-due', async () => {
  let due = true
  const { ex, seen } = rig({ legs: { dueAtHeight: async () => due }, hint: () => {}, log: () => {} })
  assert.deepEqual(await ex.call({ service: EXEC_SVC.wake, handler: 'height' }, { tip: 100 }), { due: true })
  await ex.drain()
  assert.deepEqual(
    seen.map((s) => s.handler),
    ['tick']
  )
  seen.length = 0
  due = false
  assert.deepEqual(await ex.call({ service: EXEC_SVC.wake, handler: 'height' }, { tip: 101 }), { due: false })
  await ex.drain()
  assert.equal(seen.length, 0)
  ex.close()
})

test('wake.txAbandoned: ledger leg + cycle tail', async () => {
  const dropped: string[] = []
  const { ex, seen } = rig({ legs: { txAbandoned: async (txid) => void dropped.push(txid) }, hint: () => {}, log: () => {} })
  await ex.call({ service: EXEC_SVC.wake, handler: 'txAbandoned' }, { txid: 'dead1' })
  await ex.drain()
  assert.deepEqual(dropped, ['dead1'])
  assert.equal(seen.length, 2)
  ex.close()
})

test('wake.reorg: demotion leg only — no cycle tail (matches the local shape)', async () => {
  const reorgs: number[] = []
  const { ex, seen } = rig({ legs: { reorg: async (from) => void reorgs.push(from) }, hint: () => {}, log: () => {} })
  await ex.call({ service: EXEC_SVC.wake, handler: 'reorg' }, { from: 42 })
  await ex.drain()
  assert.deepEqual(reorgs, [42])
  assert.equal(seen.length, 0)
  ex.close()
})
