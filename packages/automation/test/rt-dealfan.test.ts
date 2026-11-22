import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { PrivateKey } from '@bsv/sdk'
import { AutomationStore } from '../src/store.js'
import { MarketEngine } from '../src/market.js'
import type { MarketDeps } from '../src/market/types.js'
import { EXEC_SVC } from '../src/rt/exec.js'
import { type DealEvent } from '../src/rt/services.js'
import { Dispatcher, wireDealfan } from './rig.js'
import type { AutomationPolicy } from '../src/policy.js'
import { DEFAULT_SCORE_WEIGHTS } from '../src/scoring.js'

// rt-dealfan — the Phase 3 remainder: the tick's scan+fan-out legs as ONE
// journaled invocation on nektar-dealfan, and the deal object's event/
// reconcile handlers. What this proves: inbox items fan out keyed by the
// deal's correlation id (wantId), live ops and reconcile rows fan out per
// op key, dealEvent runs the item leg end-to-end (complete/fail inside),
// and reconcileOp runs the refused-escrow arm per op.

const key = PrivateKey.fromRandom()

async function open() {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx)
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

test('dealfan.tick: inbox → ev:<wantId>, ops → advance keys, reconcile rows → reconcile keys', async () => {
  const ex = new Dispatcher()
  const sent: Array<{ handler: string; key: string }> = []
  try {
    for (const h of ['advance', 'due', 'reconcile']) ex.register(EXEC_SVC.deal, h, async (_a, k) => void sent.push({ handler: h, key: k ?? '' }))
    ex.register(EXEC_SVC.deal, 'event', async (_a, k) => void sent.push({ handler: 'event', key: k ?? '' }))
    const items: DealEvent[] = [
      { source: 'lane', messageId: 'm1', kind: 'want', payload: { wantId: 'w-want' } },
      { source: 'msg:peer', messageId: 'm2', kind: 'message', payload: { from: 'pk', msg: { type: 'accept', wantId: 'w-msg' } } },
      { source: 'lane', messageId: 'm3', kind: 'offer', payload: { offerId: 'o-noWant' } }
    ]
    wireDealfan(ex, {
      pendingInbox: async () => items,
      liveOps: async () => [{ operationId: 'op:a' }, { operationId: 'op:b' }],
      reconcileOps: async () => [{ operationId: 'op:r' }],
      ceilingMs: () => 60_000,
      log: () => {}
    })
    const r = await ex.call<{ events: number; advanced: number; reconciled: number }>({ service: EXEC_SVC.dealfan, handler: 'tick', key: 'main' }, {})
    assert.deepEqual(r, { events: 3, advanced: 2, reconciled: 1 })
    await ex.drain()
    assert.deepEqual(sent, [
      { handler: 'event', key: 'ev:w-want' },
      { handler: 'event', key: 'ev:w-msg' },
      { handler: 'event', key: 'ev:o-noWant' },
      { handler: 'advance', key: 'op:a' },
      { handler: 'advance', key: 'op:b' },
      { handler: 'reconcile', key: 'op:r' }
    ])
  } finally {
    ex.close()
  }
})

test('dealEvent: a routed message completes its inbox row; a gated want stays pending unharmed', async () => {
  const { kx, store } = await open()
  try {
    const engine = new MarketEngine(deps(store))
    await store.ingest(
      'msg:peer',
      [{ messageId: 'im1', artifactHash: 'h1', kind: 'message', payload: { from: 'pk', msg: { type: 'not-a-wire-type', wantId: 'w1' } } }],
      'im1'
    )
    await engine.dealEvent((await store.pendingInbox())[0])
    assert.equal((await store.pendingInbox()).length, 0, 'the leg completed its row')

    // a gated want returns without completing OR failing — pending, zero
    // attempts spent, retried on the next fan-out
    const killed = new MarketEngine(deps(store, { killed: () => true }))
    await store.ingest('lane', [{ messageId: 'iw1', artifactHash: 'h2', kind: 'want', payload: { wantId: 'w2' } }], 'iw1')
    await killed.dealEvent((await store.pendingInbox())[0])
    const left = await store.pendingInbox()
    assert.equal(left.length, 1)
    assert.equal(left[0].attempts, 0)
  } finally {
    await kx.destroy()
  }
})

test('reconcileOp: a landed refused escrow arms refund_pending on the op key', async () => {
  const { kx, store } = await open()
  try {
    const hash = await store.approvePolicy(policy(), 'test')
    const { operation } = await store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey: 'r1',
      policyHash: hash,
      network: 'regtest',
      state: 'funding_failed',
      data: { escrowTxid: 'tx-esc', pendingEscrow: { cfg: { refundAfter: 500 }, utxo: { txid: 'tx-esc', vout: 0, satoshis: 1, script: 'aa' } } }
    })
    const engine = new MarketEngine(deps(store, { txFacts: async () => ({ verdict: 'mined' as const, txid: 'tx-esc' }) }))
    await engine.reconcileOp(operation.operationId)
    const cur = await store.operation(operation.operationId)
    assert.equal(cur?.state, 'refund_pending')
    assert.equal(cur?.dueHeight, 500)
  } finally {
    await kx.destroy()
  }
})
