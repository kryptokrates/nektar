import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { P2PKH, PrivateKey, Transaction, UnlockingScript } from '@bsv/sdk'
import { AutomationStore } from '../src/store.js'
import { MarketEngine } from '../src/market.js'
import type { MarketDeps } from '../src/market/types.js'
import { NONTERMINAL } from '../src/market/types.js'
import { setExecutor, EXEC_SVC } from '../src/rt/exec.js'
import { Dispatcher, wireDealfan, wireDeal } from './rig.js'
import { wireEngine } from '../src/rt/dispatch.js'
import { inertTxEffects } from '../src/machines/effects.js'
import { opWantId } from '../src/rt/services.js'
import type { AutomationPolicy } from '../src/policy.js'
import { DEFAULT_SCORE_WEIGHTS } from '../src/scoring.js'

// rt-deal — the Phase 3 legs: the op's advance leg as a serialized
// invocation on its own deal object. What this proves: the tick's
// NONTERMINAL enumeration fans out per key under the executor, the
// advance/due handlers route to engine.advanceOp, and a throwing leg
// still stamps tick_error — the same semantics the inline loop had.

const key = PrivateKey.fromRandom()
const sellerKey = PrivateKey.fromRandom()

async function open(now?: () => number) {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx, now)
  await store.init()
  return { kx, store }
}

function deps(store: AutomationStore, extra: Partial<MarketDeps> = {}): MarketDeps {
  return {
    store,
    call: () => Promise.resolve({}),
    memory: {} as never,
    key,
    network: 'regtest',
    height: () => Promise.resolve(1),
    fetchTx: () => Promise.resolve(undefined),
    transport: {} as never,
    ai: {} as never,
    ...extra
  }
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

void test('tick under an executor fans advance out per op key — the leg leaves the mutex', async () => {
  const { kx, store } = await open()
  const ex = new Dispatcher()
  const ran: string[] = []
  try {
    wireDeal(ex, {
      advanceOp: (id) => Promise.resolve((ran.push(id), true)),
      inboxEvent: async () => {},
      reconcileOp: async () => {},
      log: () => {}
    })
    // the tick routes through dealfan — the fan-out lands on deal keys
    wireDealfan(ex, {
      pendingInbox: () => store.pendingInbox(),
      liveOps: async () => (await store.operations({ states: NONTERMINAL })).map((o) => ({ operationId: o.operationId, wantId: opWantId(o) })),
      reconcileOps: () => Promise.resolve([]),
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
    void ex.close()
    await kx.destroy()
  }
})

void test('advanceOp: terminal or missing ops are no-ops; a throwing leg stamps tick_error through the shared wrapper', async () => {
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
    assert.ok(cur.lastError?.length, 'the throwing leg left its error label')

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

void test('deal.advance + deal.due route their key to advanceOp — deadline arm and tick share one leg', async () => {
  const ex = new Dispatcher()
  const calls: Array<{ handler: string; key: string }> = []
  try {
    const mk = (handler: string) =>
      ex.register(EXEC_SVC.deal, handler, (_arg, key) => {
        calls.push({ handler, key: key ?? '' })
        return Promise.resolve({ ran: true })
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
    void ex.close()
  }
})

// a `direct` quote settles by outright P2PKH — the policy accounting must
// quote the payment-fee bound, not the covenant's maxFee + escrow reserve:
// under the old accounting this 50-sat deal escalated fee_above_policy on a
// phantom 1000-sat exposure and starved on a ≥650-sat take
const directOffer = () => ({
  so: {
    offer: {
      pub: 'aa'.repeat(33),
      offerId: 'o1',
      revision: 1,
      shard: 'guide',
      seq: 1,
      priceSats: 50,
      domains: ['soccer'],
      ts: 0,
      offer: {
        cfg: { sellerPkh: '11'.repeat(20), price: 50, hWrapped: '22'.repeat(32), refundAfter: 5000, maxFee: 500 },
        sellerPub: 'bb'.repeat(33),
        offerSig: 'cc'.repeat(33),
        expiresAt: 200,
        direct: true
      }
    },
    sig: 'sig'
  }
})
const want = () => ({
  want: {
    pub: key.toPublicKey().toString(),
    wantId: 'w-direct',
    revision: 1,
    status: 'open' as const,
    taskHash: 'dd'.repeat(32),
    domains: ['soccer'],
    ts: 0
  },
  sig: 'sig'
})

void test('direct rail: policy + budget quote the payment bound — no covenant-class escalation', async () => {
  const { kx, store } = await open()
  try {
    const p = policy()
    p.limits.perTxFeeSats = 150 // > directFeeReserveSats(100), < escrowFeeReserveSats+maxFee(1000)
    const hash = await store.approvePolicy(p, 'test')
    const { operation } = await store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey: 'dd1',
      policyHash: hash,
      network: 'regtest',
      state: 'selected',
      data: { selected: { key: 'k1' }, offers: { k1: directOffer() }, want: want() }
    })
    const calls: Array<{ tool: string; amount: unknown }> = []
    const engine = new MarketEngine(
      deps(store, {
        opts: { directMaxSats: 100, directFeeReserveSats: 100 },
        call: (tool, args) => {
          calls.push({ tool, amount: (args as { amount?: number }).amount })
          return Promise.resolve({ accepted: true, txid: 't-pay', utxo: { txid: 't-pay', vout: 0, satoshis: 50, script: 'aa' } })
        }
      })
    )
    await engine.advanceOp(operation.operationId)
    const cur = await store.operation(operation.operationId)
    assert.equal(cur?.state, 'awaiting_disclosure')
    assert.equal(cur?.data.direct, true)
    assert.deepEqual(calls, [{ tool: 'sale#pay', amount: 50 }], 'the direct rail paid outright — no escrow build')
    assert.deepEqual(cur?.data.amounts, { purchaseSats: 50, feeSats: 100, lockedSats: 150 }, 'the payment bound, not maxFee+reserve')
    assert.equal((await store.approvals()).length, 0, 'no human escalation was demanded')
  } finally {
    await kx.destroy()
  }
})

void test('direct rail: ranking admits an offer whose covenant all-in would bust the cap', async () => {
  const { kx, store } = await open()
  try {
    const p = policy()
    p.limits.perTxFeeSats = 150 // all-in cap 650: direct 150 fits, escrow 1050 busts
    const hash = await store.approvePolicy(p, 'test')
    const { operation } = await store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey: 'dd2',
      policyHash: hash,
      network: 'regtest',
      state: 'collecting',
      data: { collectingSince: 0, offers: { k1: directOffer() }, want: want() }
    })
    const engine = new MarketEngine(
      deps(store, {
        opts: { directMaxSats: 100, directFeeReserveSats: 100 },
        call: () => Promise.resolve({ accepted: true, txid: 't-pay', utxo: { txid: 't-pay', vout: 0, satoshis: 50, script: 'aa' } })
      })
    )
    await engine.advanceOp(operation.operationId)
    const cur = await store.operation(operation.operationId)
    assert.equal(cur?.state, 'awaiting_disclosure', 'the direct offer selected and funded — escrow accounting would have rejected it over_budget')
  } finally {
    await kx.destroy()
  }
})

void test('direct rail: wallet broadcast refusal / unknown joins pendingSubmit and never optimistically adopts change', async () => {
  const mkPayTx = () => {
    const parent = new Transaction()
    parent.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromASM('OP_1') })
    parent.addOutput({ satoshis: 500, lockingScript: new P2PKH().lock(key.toPublicKey().toHash()) })
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromASM('OP_1') })
    tx.addOutput({ satoshis: 50, lockingScript: new P2PKH().lock(sellerKey.toPublicKey().toHash()) })
    tx.addOutput({ satoshis: 400, lockingScript: new P2PKH().lock(key.toPublicKey().toHash()) })
    return tx
  }
  for (const [label, br, wantState] of [
    ['daily-cap', { status: 'error' as const, description: 'SpendGuard refused: daily cap exceeded' }, 'funding_deferred'],
    ['unknown', { status: 'error' as const, description: 'transport: timeout' }, 'uncertain']
  ] as const) {
    const { kx, store } = await open(() => 1_700_000_000_000)
    const ex = new Dispatcher()
    try {
      const p = policy()
      p.limits.perTxFeeSats = 150
      const hash = await store.approvePolicy(p, 'test')
      const { operation } = await store.createOperation({
        role: 'buyer',
        kind: 'purchase',
        dedupeKey: `dd-join-${label}`,
        policyHash: hash,
        network: 'regtest',
        state: 'selected',
        data: { selected: { key: 'k1' }, offers: { k1: directOffer() }, want: want() }
      })
      const payTx = mkPayTx()
      let settled = 0
      const fx = {
        ...inertTxEffects,
        txSpent: () => {
          settled++
        }
      }
      const d = deps(store, {
        executor: ex,
        now: () => 1_700_000_000_000,
        opts: { directMaxSats: 100, directFeeReserveSats: 100 },
        broadcast: () => Promise.resolve(br),
        txEffects: fx,
        call: (tool) => {
          assert.equal(tool, 'sale#payBuild')
          return Promise.resolve({
            tx: payTx,
            txHex: payTx.toHex(),
            utxo: { txid: payTx.id('hex'), vout: 0, satoshis: 50, script: payTx.outputs[0].lockingScript.toHex() }
          })
        }
      })
      const engine = new MarketEngine(d)
      wireEngine(ex, engine, {
        store,
        fx,
        ceilingMs: () => 60_000,
        now: () => 1_700_000_000_000
      })
      await engine.advanceOp(operation.operationId)
      const cur = await store.operation(operation.operationId)
      assert.equal(cur?.state, wantState, `${label}: direct rail must join pendingSubmit and unwind to ${wantState}`)
      assert.equal(settled, 0, `${label}: txSpent must never adopt change before broadcast succeeds`)
      if (label === 'daily-cap') {
        assert.equal((await store.budgetStatus(hash)).lockedSats, 0, 'budget released on SpendGuard refusal')
        // clock domain check: dueAt computed on domain now (1_700_000_000_000 → next UTC midnight), not wall clock
        const expectedMidnight = new Date(1_700_000_000_000)
        expectedMidnight.setUTCHours(24, 0, 0, 0)
        assert.equal(cur?.dueAt, expectedMidnight.getTime())
      }
    } finally {
      await ex.close()
      await kx.destroy()
    }
  }
})

void test('seller direct rail: failed accept broadcast rolls back to quoted without calling ownOutput', async () => {
  const { kx, store } = await open()
  try {
    const pkhHex = Buffer.from(sellerKey.toPublicKey().toHash()).toString('hex')
    const buyerPkhHex = Buffer.from(key.toPublicKey().toHash()).toString('hex')
    const directScript = new P2PKH().lock(sellerKey.toPublicKey().toHash()).toHex()
    const payTx = new Transaction()
    payTx.addOutput({ satoshis: 50, lockingScript: new P2PKH().lock(sellerKey.toPublicKey().toHash()) })
    const txid = payTx.id('hex')
    const { operation } = await store.createOperation({
      role: 'seller',
      kind: 'sale',
      dedupeKey: 'sell-direct-refuse',
      policyHash: 'p',
      network: 'regtest',
      state: 'quoted',
      data: {
        want: want(),
        offer: {
          cfg: { sellerPkh: pkhHex, price: 50, hWrapped: 'aa'.repeat(32), refundAfter: 200, maxFee: 10 },
          sellerPub: sellerKey.toPublicKey().toString(),
          offerSig: 'sig',
          direct: true,
          wrapped: 'deadbeef'
        }
      }
    })
    let owned = 0
    const engine = new MarketEngine({
      ...deps(store, {
        key: sellerKey,
        height: () => Promise.resolve(10),
        readTx: () => Promise.resolve({ tx: payTx, spv: true, format: 'raw' as const }),
        broadcast: () => Promise.resolve({ status: 'error', description: 'txn-mempool-conflict' }),
        ownOutput: () => {
          owned++
        }
      })
    })
    await engine.ingestMessage(key.toPublicKey().toString(), {
      type: 'accept',
      wantId: 'w-direct',
      offerId: operation.operationId,
      depositorPkh: buyerPkhHex,
      utxo: { txid, vout: 0, satoshis: 50, script: directScript },
      tx: payTx.toHex()
    })
    const items = await engine.pendingInbox()
    await engine.dealEvent(items[0])
    const cur = await store.operation(operation.operationId)
    assert.equal(cur?.state, 'quoted', 'refused payment broadcast rolls sale back to quoted')
    assert.equal(owned, 0, 'ownOutput must not adopt a payment whose broadcast failed')
  } finally {
    await kx.destroy()
  }
})

void test('funding_deferred: pending txFacts without held tx bytes must not promote to awaiting_disclosure', async () => {
  const { kx, store } = await open(() => 1_000_000)
  try {
    const { operation } = await store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey: 'fd-pending-absent',
      policyHash: 'p',
      network: 'regtest',
      state: 'funding_deferred',
      dueAt: 2_000_000,
      data: { escrowTxid: 'cc'.repeat(32) }
    })
    const engine = new MarketEngine(
      deps(store, {
        now: () => 1_000_000,
        txFacts: () => Promise.resolve({ verdict: 'pending' })
      })
    )
    await engine.advanceOp(operation.operationId)
    assert.equal((await store.operation(operation.operationId))?.state, 'funding_deferred', 'absent tx stays deferred')
  } finally {
    await kx.destroy()
  }
})

void test('buyer pre-funding gate: canDeliverBox=false holds op in selected without locking funds', async () => {
  const { kx, store } = await open(() => 1_000_000)
  try {
    const hash = await store.approvePolicy(policy(), 'test')
    const { operation } = await store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey: 'bbox-gate-1',
      policyHash: hash,
      network: 'regtest',
      state: 'selected',
      data: {}
    })
    let callCount = 0
    const engine = new MarketEngine(
      deps(store, {
        now: () => 1_000_000,
        canDeliverBox: () => Promise.resolve(false),
        call: () => {
          callCount++
          return Promise.resolve({})
        }
      })
    )
    await engine.advanceOp(operation.operationId)
    const cur = await store.operation(operation.operationId)
    assert.equal(cur?.state, 'selected', 'op must stay in selected until bbox lane has servable stock')
    assert.match(cur?.lastError ?? '', /bbox/, 'operator-visible reason names bbox warmup')
    assert.equal(callCount, 0, 'must not call sale#fund or lock escrow funds while bbox is unready')
  } finally {
    await kx.destroy()
  }
})
