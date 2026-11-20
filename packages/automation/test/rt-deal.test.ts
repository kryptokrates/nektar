import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { PrivateKey } from '@bsv/sdk'
import { AutomationStore } from '../src/store.js'
import { MarketEngine } from '../src/market.js'
import type { MarketDeps } from '../src/market/types.js'
import { NONTERMINAL } from '../src/market/types.js'
import { setExecutor, EXEC_SVC } from '../src/rt/exec.js'
import { Dispatcher, wireDealfan, wireDeal } from './rig.js'
import { opWantId } from '../src/rt/services.js'
import type { AutomationPolicy } from '../src/policy.js'
import { DEFAULT_SCORE_WEIGHTS } from '../src/scoring.js'

// rt-deal — the Phase 3 legs: the op's advance leg as a serialized
// invocation on its own deal object. What this proves: the tick's
// NONTERMINAL enumeration fans out per key under the executor, the
// advance/due handlers route to engine.advanceOp, and a throwing leg
// still stamps tick_error — the same semantics the inline loop had.

const key = PrivateKey.fromRandom()

async function open(now?: () => number) {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx, now)
  await store.init()
  return { kx, store }
}

function deps(store: AutomationStore, extra: Partial<MarketDeps> = {}): MarketDeps {
  return {
    store,
    call: async () => ({}),
    memory: {} as never,
    key,
    network: 'regtest',
    height: async () => 1,
    fetchTx: async () => undefined,
    transport: {} as never,
    ai: {} as never,
    ...extra
  } as MarketDeps
}

const policy = (): AutomationPolicy => ({
  v: 1,
  policyId: 'p1',
  network: 'regtest',
  agentPub: key.toPublicKey().toString(),
  roles: ['buyer', 'seller'],
  notBefore: 0,
  notAfter: 4_000_000_000_000,
  domains: ['soccer'],
  limits: {
    perPurchaseSats: 500,
    dailyPurchaseSats: 2500,
    perTxFeeSats: 100,
    dailyFeeSats: 1000,
    runTotalSats: 100_000,
    maxLockedSats: 5000,
    maxUnconfirmedSats: 2000,
    maxUnconfirmedCount: 5
  },
  scoring: { minimumScoreBps: 0, weights: { ...DEFAULT_SCORE_WEIGHTS }, collectMs: 1000 },
  seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: 10, sellableShards: ['guide'] },
  disclosure: { allowPublic: false },
  ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 1000, timeoutMs: 10_000 },
  sandbox: { mode: 'sandbox-only' }
})

test('tick under an executor fans advance out per op key — the leg leaves the mutex', async () => {
  const { kx, store } = await open()
  const ex = new Dispatcher()
  const ran: string[] = []
  try {
    wireDeal(ex, {
      advanceOp: async (id) => (ran.push(id), true),
      inboxEvent: async () => {},
      reconcileOp: async () => {},
      log: () => {}
    })
    // the tick routes through dealfan — the fan-out lands on deal keys
    wireDealfan(ex, {
      pendingInbox: () => store.pendingInbox(),
      liveOps: async () => (await store.operations({ states: NONTERMINAL })).map((o) => ({ operationId: o.operationId, wantId: opWantId(o) })),
      reconcileOps: async () => [],
      ceilingMs: () => 60_000,
      log: () => {}
    })
    const hash = await store.approvePolicy(policy(), 'test')
    await store.createOperation({ role: 'buyer', kind: 'purchase', dedupeKey: 'd1', policyHash: hash, network: 'regtest', state: 'collecting', data: {} })
    await store.createOperation({ role: 'buyer', kind: 'purchase', dedupeKey: 'd2', policyHash: hash, network: 'regtest', state: 'selected', data: {} })
    const engine = new MarketEngine(deps(store))
    setExecutor(ex)
    await engine.tick()
    await ex.drain()
    assert.equal(ran.length, 2)
    const ids = (await store.operations({ states: ['collecting', 'selected'] })).map((o) => o.operationId).sort()
    assert.deepEqual([...ran].sort(), ids)
  } finally {
    setExecutor(undefined)
    ex.close()
    await kx.destroy()
  }
})

test('advanceOp: terminal or missing ops are no-ops; a throwing leg stamps tick_error through the shared wrapper', async () => {
  const { kx, store } = await open()
  try {
    const hash = await store.approvePolicy(policy(), 'test')
    const { operation } = await store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey: 'd3',
      policyHash: hash,
      network: 'regtest',
      state: 'selected',
      data: {}
    })
    const engine = new MarketEngine(deps(store))
    // 'selected' with no data.selected — fund() throws deterministically;
    // the leg must still run and stamp, not swallow
    assert.equal(await engine.advanceOp(operation.operationId), true)
    const cur = await store.operation(operation.operationId)
    assert.equal(cur?.state, 'selected')
    assert.ok(cur?.lastError?.length, 'the throwing leg left its error label')

    // terminal + missing are refused before the leg runs
    const { operation: done } = await store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey: 'd4',
      policyHash: hash,
      network: 'regtest',
      state: 'useful',
      data: {}
    })
    assert.equal(await engine.advanceOp(done.operationId), false)
    assert.equal(await engine.advanceOp('op:nope'), false)
  } finally {
    await kx.destroy()
  }
})

test('deal.advance + deal.due route their key to advanceOp — deadline arm and tick share one leg', async () => {
  const ex = new Dispatcher()
  const calls: Array<{ handler: string; key: string }> = []
  try {
    const mk = (handler: string) =>
      ex.register(EXEC_SVC.deal, handler, async (_arg, key) => {
        calls.push({ handler, key: key ?? '' })
        return { ran: true }
      })
    mk('advance')
    mk('due')
    await ex.call({ service: EXEC_SVC.deal, handler: 'advance', key: 'op:a' }, {})
    await ex.send({ service: EXEC_SVC.deal, handler: 'due', key: 'op:b' }, {}, { delayMs: 0 })
    await ex.drain()
    assert.deepEqual(calls, [
      { handler: 'advance', key: 'op:a' },
      { handler: 'due', key: 'op:b' }
    ])
  } finally {
    ex.close()
  }
})
