import { test, type TestContext } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { PrivateKey, P2PKH, Transaction } from '@bsv/sdk'
import { AutomationStore } from '../src/store.js'
import { WalletCoordinator, type WalletDeps } from '../src/wallet.js'
import { TransactionWatcher } from '../src/ledger.js'
import { MarketEngine } from '../src/market.js'
import { NONTERMINAL, TERMINAL, type MarketDeps } from '../src/market/types.js'
import type { TxEffects } from '../src/machines/effects.js'
import { Dispatcher, wireTx } from './rig.js'
import { wireEngine } from '../src/rt/dispatch.js'
import { policyError, type AutomationPolicy } from '../src/policy.js'
import { DEFAULT_SCORE_WEIGHTS } from '../src/scoring.js'

// state-audit Phase 1 — each test reproduces one finding F1–F8 and must fail
// for the stated reason before the fix lands.

const key = PrivateKey.fromRandom()
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

async function open(now?: () => number) {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx, now)
  await store.init()
  return { kx, store }
}

function deps(store: AutomationStore, now: () => number, extra: Partial<MarketDeps> = {}): MarketDeps {
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
    now,
    ...extra
  }
}

function oneOutputTx(payTo = key): Transaction {
  const tx = new Transaction()
  tx.addOutput({ satoshis: 1, lockingScript: new P2PKH().lock(payTo.toPublicKey().toHash()) })
  return tx
}

// the observe handler's intent set mirrors the coordinator's own fx(): an
// emit routed through the Dispatcher lands on wireTx's handler, whose fx
// must rebuild the stored bytes and call the same deps — the test-side twin
// of wallet.ts's driver fx (runtime.ts does the same with txFx)
const fxFor = (w: WalletCoordinator, deps: Pick<WalletDeps, 'txSpent' | 'txDied' | 'recordAired'>): TxEffects => ({
  txSpent: deps.txSpent === undefined ? undefined : async (wt) => await deps.txSpent!(await w.rebuildTx(wt)),
  txDied: deps.txDied === undefined ? undefined : async (wt) => await deps.txDied!(await w.rebuildTx(wt)),
  recordAired: deps.recordAired === undefined ? undefined : async (wt) => await deps.recordAired!(await w.rebuildTx(wt))
})

// the engine-side twin: base.ts builds the coordinator's custody deps as
// {txDied, txSpent: settleEscrow, recordAired} — the observe intents run
// those same impls (rebuilt to a Transaction first, exactly like fx())
const engineFx = (engine: MarketEngine, d: MarketDeps): TxEffects => {
  const coord = engine.walletCoordinator
  const fx: TxEffects = {}
  if (coord === undefined) return fx
  if (d.txDied !== undefined) fx.txDied = async (wt) => await d.txDied!(await coord.rebuildTx(wt))
  if (d.settleEscrow !== undefined) fx.txSpent = async (wt) => await d.settleEscrow!(await coord.rebuildTx(wt))
  if (d.recordAired !== undefined) fx.recordAired = async (wt) => await d.recordAired!(await coord.rebuildTx(wt))
  return fx
}

// an engine whose durable legs run on an in-process Dispatcher — wireEngine
// mirrors runtime.ts's service surface; the floor self-arm sits on a 60s
// ceiling that close() clears in t.after (no leak, no hot re-fire)
const mkEngine = (t: TestContext, d: MarketDeps): MarketEngine => {
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const engine = new MarketEngine({ ...d, executor: ex })
  wireEngine(ex, engine, { store: d.store, fx: engineFx(engine, d), ceilingMs: () => 60_000, now: d.now ?? Date.now, log: () => {} })
  return engine
}

void test('F1: a poisoned op still reaches its declared stuck exit — error labels must not reset the watchdog clock', async (t) => {
  let now = 1_000_000
  const { kx, store } = await open(() => now)
  t.after(() => kx.destroy())
  const hash = await store.approvePolicy(policy(), 'test')
  assert.equal(policyError(policy()), undefined)
  const { operation } = await store.createOperation({
    role: 'buyer',
    kind: 'purchase',
    dedupeKey: 'f1',
    policyHash: hash,
    network: 'regtest',
    state: 'selected',
    data: {} // no data.selected — fund() throws deterministically below
  })
  const engine = mkEngine(
    t,
    deps(store, () => now)
  )
  // 'selected' bound is 15 min, exit abort → 'canceled'. Six 5-minute ticks
  // is 30 min elapsed: today every tick's tick_error write refreshes
  // updatedAt — the clock the bound measures — so it never fires.
  for (let i = 0; i < 6; i++) {
    now += 5 * 60_000
    await engine.tick()
  }
  const cur = await store.operation(operation.operationId)
  assert.equal(cur?.state, 'canceled', 'a deterministically-throwing op must hit its declared exit')
})

void test('F2/F6: an abandoned verdict runs the custody rollback and labels the row abandoned — never conflicted-on-absence', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  let died = 0
  const hooks: Pick<WalletDeps, 'txSpent' | 'txDied'> = {
    txSpent: () => {},
    txDied: () => {
      died++
    }
  }
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const w = new WalletCoordinator(store, {
    broadcast: () => Promise.resolve({ status: 'success' }),
    ...hooks,
    exec: ex
  })
  wireTx(ex, { store, fx: fxFor(w, hooks), log: () => {} })
  const tx = oneOutputTx()
  const actionId = `owner:${tx.id('hex')}`
  await w.prepareOwner('upkeep', tx)
  const sub = await w.submit(actionId)
  assert.equal(sub.state, 'submitted')
  await w.observe(tx.id('hex'), { status: 'abandoned', source: 'events' } as never)
  const row = await store.walletTx({ txid: tx.id('hex') })
  assert.notEqual(row?.state, 'conflicted', 'absence is not a competing spend — the verdict must not wear the conflicted label')
  assert.equal(row?.state, 'abandoned')
  assert.equal(died, 1, 'every non-confirmed terminal verdict must run txDied — the phantom-parent rollback')
})

void test('F3: releaseUnsigned is gone — the abandon-prepared semantic lives on the reconcile path', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  // the inline equivalent already covers the semantic (reconcileUncertain
  // releases budget+leases and rejects the row on a 'dead' verdict) — the
  // dedicated helper was a weaker duplicate nobody could reach: it must be
  // deleted, not kept as dormant surface.
  assert.equal(typeof (store as unknown as { releaseUnsigned?: unknown }).releaseUnsigned, 'undefined', 'releaseUnsigned is dead code — delete it')
})

void test('F3-regression: an uncertain op whose escrow never aired releases budget, leases, and marks the row rejected', async (t) => {
  let now = 1_000_000
  const { kx, store } = await open(() => now)
  t.after(() => kx.destroy())
  const tx = oneOutputTx()
  const actionId = 'esc1'
  await store.recordPrepared({ actionId, operationId: 'op-f3', kind: 'escrow', txid: tx.id('hex'), rawHex: tx.toHex(), parents: {}, feeSats: 0 })
  const { operation } = await store.createOperation({
    role: 'buyer',
    kind: 'purchase',
    dedupeKey: 'f3',
    policyHash: 'x',
    network: 'regtest',
    state: 'uncertain',
    data: { pendingApproval: { reason: 'unresolved_payment' }, escrowAction: actionId }
  })
  const engine = mkEngine(
    t,
    deps(store, () => now, { txFacts: () => ({ verdict: 'dead' }) as never })
  )
  await engine.tick()
  const row = await store.walletTx({ actionId })
  const op = await store.operation(operation.operationId)
  assert.equal(row?.state, 'rejected')
  assert.equal(op?.state, 'funding_failed')
})

void test('N1: reconcile-dead on a submission_unknown escrow runs the custody rollback — the terminal write carries tx-died, not a bare setTxState', async (t) => {
  let now = 1_000_000
  const { kx, store } = await open(() => now)
  t.after(() => kx.destroy())
  const tx = oneOutputTx()
  const actionId = 'esc1'
  await store.recordPrepared({ actionId, operationId: 'op-n1', kind: 'escrow', txid: tx.id('hex'), rawHex: tx.toHex(), parents: {}, feeSats: 0 })
  // the broadcast call outcome is unknown — the tx may have escaped with its
  // adopted change; a dead verdict here MUST tombstone that adoption
  await store.setTxState(actionId, 'submission_unknown')
  let died = 0
  const { operation } = await store.createOperation({
    role: 'buyer',
    kind: 'purchase',
    dedupeKey: 'n1',
    policyHash: 'x',
    network: 'regtest',
    state: 'uncertain',
    data: { pendingApproval: { reason: 'unresolved_payment' }, escrowAction: actionId }
  })
  const engine = mkEngine(
    t,
    deps(store, () => now, {
      broadcast: () => ({ status: 'error', description: 'transport: down' }) as never,
      txFacts: () => ({ verdict: 'dead' }) as never,
      txDied: () => {
        died++
      }
    })
  )
  await engine.tick()
  const row = await store.walletTx({ actionId })
  const op = await store.operation(operation.operationId)
  assert.equal(row?.state, 'rejected')
  assert.equal(op?.state, 'funding_failed')
  assert.equal(died, 1, 'a dead verdict on submission_unknown must run txDied — the escape may have aired its change')
})

void test('F4: every op state is classified — NONTERMINAL ∪ TERMINAL ∪ PARKED covers the union', async (_t) => {
  const mod = (await import('../src/market/types.js')) as unknown as { PARKED?: string[] }
  const classified = new Set([...NONTERMINAL, ...TERMINAL, ...(mod.PARKED ?? [])])
  for (const s of NONTERMINAL) assert.ok(!TERMINAL.includes(s), `${s} in both lists`)
  assert.ok(classified.has('disclosure_verified'), 'disclosure_verified parks on an external entry point — declare it')
})

void test('F5: an expired export that lands late is observed, not stranded in a mislabeled row', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const tx = oneOutputTx()
  const txid = tx.id('hex')
  await store.recordPrepared({ actionId: 'a1', operationId: 'op', kind: 'payment', txid, rawHex: tx.toHex(), parents: {}, feeSats: 0 })
  await store.setTxState('a1', 'expired', { reason: 'test expiry' })
  const w = new TransactionWatcher(store, () => Promise.resolve({ status: 'seen' as const, source: 't' }))
  await w.tick()
  assert.equal((await store.walletTx({ txid }))!.state, 'observed', 'a landed-late export must move to observed')
})

void test('F7: a prepared row is resubmitted on the upkeep cadence — not only at boot', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const aired: Transaction[] = []
  const tx = oneOutputTx()
  await store.recordPrepared({ actionId: 'up:1', operationId: 'owner', kind: 'upkeep', txid: tx.id('hex'), rawHex: tx.toHex(), parents: {}, feeSats: 0 })
  const engine = mkEngine(t, deps(store, Date.now, { broadcast: (t: Transaction) => (aired.push(t), { status: 'success' }) as never }))
  await engine.tick()
  assert.ok(aired.length > 0, 'in-run prepared rows must reach the broadcaster without a restart')
})

void test('F8: a refund rejection surfacing after the 60-minute rescan still un-marks refunded', async (t) => {
  let now = 1_000_000
  const { kx, store } = await open(() => now)
  t.after(() => kx.destroy())
  const { operation } = await store.createOperation({
    role: 'buyer',
    kind: 'purchase',
    dedupeKey: 'f8',
    policyHash: 'x',
    network: 'regtest',
    state: 'refunded',
    data: { refundTxid: 'aa'.repeat(32) }
  })
  const engine = mkEngine(
    t,
    deps(store, () => now, { txFacts: () => ({ verdict: 'dead' }) as never })
  )
  now += 61 * 60_000
  await engine.tick()
  assert.equal((await store.operation(operation.operationId))!.state, 'uncertain', 'a dead refund must not stay refunded past the rescan window')
})
