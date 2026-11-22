import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import knexFactory from 'knex'
import { PrivateKey, P2PKH, Transaction, UnlockingScript } from '@bsv/sdk'
import { testWorld, dryFunder, type TestFunder } from '@nektar/chain'
import { AutomationStore, RevisionConflict, classifyDeliveryError } from '../src/store.js'
import { WalletCoordinator, type WalletDeps } from '../src/wallet.js'
import type { TxEffects } from '../src/machines/effects.js'
import { Dispatcher, wireTx } from './rig.js'
import { policyHash, policyError, decidePurchase, decideOffer, type AutomationPolicy, type ActionContext } from '../src/policy.js'
import { DEFAULT_SCORE_WEIGHTS } from '../src/scoring.js'

const agent = PrivateKey.fromRandom().toPublicKey().toString()
const policy = (limits: Partial<AutomationPolicy['limits']> = {}, extra: Partial<AutomationPolicy> = {}): AutomationPolicy => ({
  v: 1,
  policyId: 'p1',
  network: 'regtest',
  agentPub: agent,
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
    maxUnconfirmedCount: 5,
    ...limits
  },
  scoring: { minimumScoreBps: 0, weights: { ...DEFAULT_SCORE_WEIGHTS }, collectMs: 1000 },
  seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: 10, sellableShards: ['guide'] },
  disclosure: { allowPublic: false },
  ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 1000, timeoutMs: 10_000 },
  sandbox: { mode: 'sandbox-only' },
  ...extra
})

async function open(file = ':memory:', now?: () => number) {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: file }, useNullAsDefault: true })
  const store = new AutomationStore(kx, now)
  await store.init()
  return { kx, store }
}
const ctx = (hash: string, actionId: string, operationId = 'op'): ActionContext => ({ operationId, actionId, policyHash: hash, networkId: 'regtest' })

test('policy: strict shape, stable hash, unknown constraints rejected', () => {
  const p = policy()
  assert.equal(policyError(p), undefined)
  assert.equal(policyHash(p), policyHash(JSON.parse(JSON.stringify(p))))
  assert.notEqual(policyHash(p), policyHash(policy({ perPurchaseSats: 501 })))
  assert.match(policyError({ ...p, surprise: 1 })!, /unknown/)
  assert.ok(policyError({ ...p, limits: { ...p.limits, perPurchaseSats: -1 } }))
  assert.ok(policyError({ ...p, limits: { ...p.limits, perPurchaseSats: 9999 } }), 'per-action cap above daily cap')
  assert.ok(policyError({ ...p, sandbox: { mode: 'host' } }))
  assert.throws(() => policyHash({ ...p, v: 2 } as never))
  // disclosure.paid is optional for pre-field policies but strict when present
  assert.equal(policyError({ ...p, disclosure: { allowPublic: false, paid: true } }), undefined)
  assert.ok(policyError({ ...p, disclosure: { allowPublic: false, paid: 'yes' } as never }))
  assert.ok(policyError({ ...p, disclosure: { allowPublic: false, paid: true, extra: 1 } as never }))
})

test('policy decisions: allow inside, escalate over a limit, deny wrong agent/network/role', () => {
  const p = policy()
  const i = { network: 'regtest', agentPub: agent, domains: ['soccer'], priceSats: 200, feeSats: 50, lockedSats: 1000, now: 1000 }
  assert.deepEqual(decidePurchase(p, i), { kind: 'allow' })
  assert.deepEqual(decidePurchase(p, { ...i, priceSats: 501 }), { kind: 'escalate', code: 'price_above_policy' })
  assert.deepEqual(decidePurchase(p, { ...i, domains: ['rugby'] }), { kind: 'escalate', code: 'domain_outside_policy' })
  assert.deepEqual(decidePurchase(p, { ...i, network: 'main' }), { kind: 'deny', code: 'wrong_network' })
  assert.deepEqual(decidePurchase(p, { ...i, priceSats: NaN }), { kind: 'deny', code: 'invalid_amount' })
  assert.deepEqual(decidePurchase(policy({}, { roles: ['seller'] }), i), { kind: 'deny', code: 'role_not_authorized' })
  const o = { network: 'regtest', agentPub: agent, domains: ['soccer'], shard: 'guide', priceSats: 200, publicDisclosure: false, now: 1000 }
  assert.deepEqual(decideOffer(p, o), { kind: 'allow' })
  assert.deepEqual(decideOffer(p, { ...o, shard: 'private-notes' }), { kind: 'deny', code: 'shard_not_sellable' })
  assert.deepEqual(decideOffer(p, { ...o, publicDisclosure: true }), { kind: 'deny', code: 'public_disclosure_not_authorized' })
  assert.deepEqual(decideOffer(p, { ...o, priceSats: 50 }), { kind: 'deny', code: 'price_outside_policy' })
})

test('budget: 20 concurrent purchases cannot exceed the aggregate daily cap', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const hash = await store.approvePolicy(policy(), 'test')
  const results = await Promise.all(
    Array.from({ length: 20 }, (_, i) => store.reserveBudget(ctx(hash, `a${i}`), { purchaseSats: 500, feeSats: 10, lockedSats: 600 }))
  )
  assert.equal(results.filter((r) => r.ok).length, 5)
  assert.ok(results.filter((r) => !r.ok).every((r) => !r.ok && r.code === 'daily_purchase_cap'))
  const s = await store.budgetStatus(hash)
  assert.equal(s.purchaseTodaySats, 2500)
  assert.equal(s.feeTodaySats, 50)
  assert.equal(s.lockedSats, 3000)
})

test('budget: idempotent action ids, locked capital, release vs settle, revocation, day rollover', async (t) => {
  let now = Date.UTC(2026, 0, 1, 12)
  const { kx, store } = await open(':memory:', () => now)
  t.after(() => kx.destroy())
  const hash = await store.approvePolicy(policy({ maxLockedSats: 1000 }), 'test')
  const amt = { purchaseSats: 200, feeSats: 10, lockedSats: 600 }
  assert.deepEqual(await store.reserveBudget(ctx(hash, 'a'), amt), { ok: true, existing: false })
  assert.deepEqual(await store.reserveBudget(ctx(hash, 'a'), amt), { ok: true, existing: true })
  assert.deepEqual(await store.reserveBudget(ctx(hash, 'a'), { ...amt, purchaseSats: 300 }), { ok: false, code: 'action_reused_with_different_terms' })
  assert.deepEqual(await store.reserveBudget(ctx(hash, 'b'), amt), { ok: false, code: 'locked_capital_cap' })
  assert.equal(await store.commitBudget('a'), true)
  // a committed row still releases: when the escrow tx is rejected for good
  // the locked capital must come back — the late-rejection path relies on it
  assert.equal(await store.releaseBudget('a'), true, 'a rejected (committed) escrow releases its locked capital')
  assert.deepEqual(await store.reserveBudget(ctx(hash, 'b'), amt), { ok: true, existing: false }, 'released capital frees the lock')
  assert.equal(await store.releaseBudget('b'), true)
  assert.deepEqual(await store.reserveBudget(ctx(hash, 'b'), amt), { ok: false, code: 'action_released' })
  assert.deepEqual(await store.reserveBudget(ctx(hash, 'b2'), amt), { ok: true, existing: false })
  now += 86_400_000
  assert.deepEqual(await store.reserveBudget(ctx(hash, 'b3'), amt), { ok: false, code: 'locked_capital_cap' }, 'day rollover does not free locked capital')
  assert.equal(await store.settleBudget('b2'), true)
  assert.deepEqual(await store.reserveBudget(ctx(hash, 'b3'), amt), { ok: true, existing: false })
  assert.deepEqual(await store.reserveBudget(ctx(hash, 'c'), { ...amt, purchaseSats: 501 }), { ok: false, code: 'price_above_policy' })
  assert.deepEqual(await store.reserveBudget({ ...ctx(hash, 'd'), networkId: 'main' }, amt), { ok: false, code: 'wrong_network' })
  assert.deepEqual(await store.reserveBudget(ctx('0'.repeat(64), 'e'), amt), { ok: false, code: 'unknown_policy' })
  assert.deepEqual(await store.reserveBudget(ctx(hash, 'f'), { ...amt, feeSats: -1 }), { ok: false, code: 'invalid_amount' })
  await store.revokePolicy(hash)
  assert.equal(await store.activePolicy(hash), undefined)
  assert.deepEqual(await store.reserveBudget(ctx(hash, 'g'), amt), { ok: false, code: 'policy_revoked' })
})

test('inbox: batch + cursor commit atomically; failed work stays pending; cross-source duplicates collapse', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  assert.equal(await store.cursor('rv1'), undefined)
  assert.deepEqual(
    await store.ingest(
      'rv1',
      [
        { messageId: '1', artifactHash: 'h1', kind: 'want', payload: { a: 1 } },
        { messageId: '2', artifactHash: 'h2', kind: 'want', payload: { a: 2 } }
      ],
      '2'
    ),
    { accepted: 2, duplicates: 0 }
  )
  assert.deepEqual(await store.ingest('rv1', [{ messageId: '2', artifactHash: 'h2', kind: 'want', payload: { a: 2 } }], '2'), { accepted: 0, duplicates: 1 })
  assert.deepEqual(await store.ingest('rv2', [{ messageId: '9', artifactHash: 'h1', kind: 'want', payload: { a: 1 } }], '9'), { accepted: 0, duplicates: 1 })
  assert.equal(await store.cursor('rv1'), '2')
  assert.equal(await store.cursor('rv2'), '9')
  await assert.rejects(
    store.ingest(
      'rv1',
      [
        { messageId: '3', artifactHash: 'h3', kind: 'want', payload: { a: 3 } },
        { messageId: '4', artifactHash: undefined as never, kind: 'want', payload: {} }
      ],
      '4'
    )
  )
  assert.equal(await store.cursor('rv1'), '2', 'a failed batch leaves the cursor behind')
  let pending = await store.pendingInbox()
  assert.deepEqual(
    pending.map((p) => p.messageId),
    ['1', '2']
  )
  await store.completeInbox('rv1', '1')
  await store.failInbox('rv1', '2', 'boom')
  pending = await store.pendingInbox()
  assert.deepEqual(
    pending.map((p) => [p.messageId, p.attempts]),
    [['2', 1]]
  )
  for (let i = 0; i < 7; i++) await store.failInbox('rv1', '2', 'boom')
  assert.equal((await store.pendingInbox()).length, 0, 'dead-lettered after bounded attempts')
})

test('operations: idempotent creation, CAS transitions, transactional outbox, event trail', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const mk = () =>
    store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey: 'buyer:w1',
      policyHash: 'h',
      network: 'regtest',
      state: 'published',
      data: { escrowTxid: 'w1' }
    })
  const a = await mk()
  if (a === undefined) return
  const b = await mk()
  if (b === undefined) return
  assert.equal(a.created, true)
  assert.equal(b.created, false)
  assert.equal(a.operation.operationId, b.operation.operationId)
  const id = a.operation.operationId
  const next = await store.transition(id, 1, 'collecting', {
    cause: 'want_published',
    data: { offers: {} },
    outbox: [{ actionId: `${id}:publish`, kind: 'want', destination: 'rv', payload: { x: 1 } }]
  })
  assert.equal(next.revision, 2)
  assert.deepEqual(next.data, { offers: {}, escrowTxid: 'w1' })
  await assert.rejects(store.transition(id, 1, 'ranked', { cause: 'stale' }), RevisionConflict)
  const racers = await Promise.allSettled([store.transition(id, 2, 'ranked', { cause: 'x' }), store.transition(id, 2, 'expired', { cause: 'y' })])
  assert.equal(racers.filter((r) => r.status === 'fulfilled').length, 1, 'exactly one of two racing transitions wins')
  assert.deepEqual((await store.events(id)).map((e) => e.to).slice(0, 2), ['published', 'collecting'])
  assert.equal((await store.dueOutbox()).length, 1)
  assert.equal(await store.enqueue({ actionId: `${id}:publish`, operationId: id, kind: 'want', destination: 'rv', payload: { x: 2 } }), false)
  await store.retryOutbox(`${id}:publish`, 'net down')
  assert.equal((await store.dueOutbox()).length, 0, 'backoff defers the retry')
  await store.ackOutbox(`${id}:publish`)
})

test('op.data codec: closed schema rejects undeclared/malformed writes; null clears rather than persisting', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  await assert.rejects(
    store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey: 'buyer:bad',
      policyHash: 'h',
      network: 'regtest',
      state: 'published',
      data: { typoField: 1 } as never
    }),
    /undeclared field/
  )
  await assert.rejects(
    store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey: 'buyer:bad2',
      policyHash: 'h',
      network: 'regtest',
      state: 'published',
      data: { escrowTxid: 42 } as never
    }),
    /malformed value/
  )
  const a = await store.createOperation({
    role: 'buyer',
    kind: 'purchase',
    dedupeKey: 'buyer:w2',
    policyHash: 'h',
    network: 'regtest',
    state: 'published',
    data: { escrowTxid: 'e1', fundingSince: 7 }
  })
  const id = a.operation.operationId
  await assert.rejects(store.transition(id, 1, 'collecting', { cause: 'x', data: { mystery: true } as never }), /undeclared field/)
  const still = await store.operation(id)
  assert.equal(still?.state, 'published', 'a rejected transition leaves state untouched')
  assert.equal(still?.revision, 1)
  const cleared = await store.transition(id, 1, 'collecting', { cause: 'clear', data: { escrowTxid: null, fundingSince: null } })
  assert.equal(cleared.data.escrowTxid, undefined)
  assert.equal(cleared.data.fundingSince, undefined)
  assert.equal('escrowTxid' in cleared.data, false, 'null deletes the key — no literal null persists under a typed read')
  const row = await kx('market_operations').where({ operation_id: id }).first()
  assert.equal(JSON.parse(row.data).escrowTxid, undefined, 'the persisted JSON carries no null literal either')
})

test('approvals: bound to the exact action digest, single use, expiry enforced', async (t) => {
  let now = 1000
  const { kx, store } = await open(':memory:', () => now)
  t.after(() => kx.destroy())
  const a = await store.requestApproval({
    operationId: 'op',
    reason: 'price_above_policy',
    policyHash: 'h',
    actionDigest: 'd1',
    maxExtraSats: 100,
    expiresAt: 5000
  })
  assert.equal(
    (await store.requestApproval({ operationId: 'op', reason: 'price_above_policy', policyHash: 'h', actionDigest: 'd1', maxExtraSats: 100, expiresAt: 5000 }))
      .approvalId,
    a.approvalId
  )
  assert.equal(await store.consumeApproval('op', 'd1'), false, 'pending is not approved')
  await store.decideApproval(a.approvalId, true, 'cli:operator')
  assert.equal(await store.consumeApproval('op', 'd2'), false, 'a different action is not covered')
  assert.equal(await store.consumeApproval('op', 'd1'), true)
  assert.equal(await store.consumeApproval('op', 'd1'), false, 'single use')
  await assert.rejects(store.decideApproval(a.approvalId, true, 'x'), /already/)
  const b = await store.requestApproval({ operationId: 'op', reason: 'r', policyHash: 'h', actionDigest: 'd3', maxExtraSats: 1, expiresAt: 2000 })
  now = 3000
  await assert.rejects(store.decideApproval(b.approvalId, true, 'x'), /expired/)
})

async function payment(funder: TestFunder, sats = 500) {
  const f = await funder.take(sats + 200)
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: f.tx, sourceOutputIndex: f.vout, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
  tx.addOutput({ satoshis: sats, lockingScript: new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toAddress([0x6f])) })
  tx.addOutput({ satoshis: f.satoshis - sats - 50, lockingScript: new P2PKH().lock(funder.address()) })
  await tx.sign()
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

test('wallet: bytes persisted before submission; a crash between prepare and submit resumes the SAME tx', async (t) => {
  const file = join(mkdtempSync(join(tmpdir(), 'nektar-auto-')), 'a.sqlite')
  const w = await testWorld(t, 3)
  if (w === undefined) return
  const { chain, funder } = w
  const tx = await payment(funder)
  const a = await open(file)
  const seen: string[] = []
  const ex1 = new Dispatcher()
  t.after(() => ex1.close())
  const w1 = new WalletCoordinator(a.store, {
    broadcast: async () => {
      throw new Error('process died')
    },
    exec: ex1
  })
  wireTx(ex1, { store: a.store, fx: {}, log: () => {}, submit: (id) => w1.submit(id) })
  const prepared = await w1.prepare(ctx('h', 'pay:1'), 'send', tx)
  assert.equal(prepared.state, 'prepared')
  assert.equal(prepared.feeSats, 50)
  assert.equal((await w1.submit('pay:1')).state, 'submission_unknown', 'a transport failure is undecidable, not a rejection')
  assert.equal((await a.store.leaseState(tx.inputs[0].sourceTXID!, 0))?.state, 'leased', 'inputs stay leased while the outcome is unknown')
  await a.kx.destroy()
  const b = await open(file)
  t.after(() => b.kx.destroy())
  const ex2 = new Dispatcher()
  t.after(() => ex2.close())
  const w2 = new WalletCoordinator(b.store, {
    broadcast: async (x) => {
      seen.push(x.id('hex'))
      return await chain.broadcast(x)
    },
    exec: ex2
  })
  wireTx(ex2, { store: b.store, fx: {}, log: () => {}, submit: (id) => w2.submit(id) })
  assert.deepEqual(await w2.resubmitPending(), [{ actionId: 'pay:1', state: 'submitted' }])
  assert.deepEqual(seen, [tx.id('hex')], 'the recorded bytes were sent — nothing was rebuilt')
  assert.equal((await w2.submit('pay:1')).state, 'submitted', 'resubmitting an accepted tx is harmless')
  assert.equal(await w2.observe(tx.id('hex'), { status: 'seen', source: 'arc' }), 'observed')
  assert.equal(await w2.observe(tx.id('hex'), { status: 'proven', source: 'spv', blockHeight: 7 }), 'confirmed')
  assert.equal((await b.store.leaseState(tx.inputs[0].sourceTXID!, 0))?.state, 'spent')
  assert.equal(await w2.observe('f'.repeat(64), { status: 'seen', source: 'arc' }), undefined, 'untracked txids change nothing')
})

test('wallet: one outpoint, one live action; guard refusal and network rejection both release', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const funder = dryFunder()
  // fabricated coinbase parent — the tx below is never broadcast; the wallet
  // test only exercises lease + persistence semantics
  const ftx = new Transaction()
  ftx.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromHex('51') })
  ftx.addOutput({ satoshis: 50_000, lockingScript: new P2PKH().lock(funder.keyPair.toPublicKey().toAddress()) })
  const f = { tx: ftx, vout: 0, satoshis: 50_000 }
  const build = async () => {
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: f.tx, sourceOutputIndex: f.vout, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
    tx.addOutput({ satoshis: f.satoshis - 100, lockingScript: new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toAddress([0x6f])) })
    await tx.sign()
    return tx
  }
  let reply: import('@nektar/chain').BroadcastResult = { status: 'error', description: 'broadcaster: REJECTED — missing inputs' }
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const w = new WalletCoordinator(store, { broadcast: async () => reply, exec: ex })
  wireTx(ex, { store, fx: {}, log: () => {}, submit: (id) => w.submit(id) })
  const t1 = await build(),
    t2 = await build()
  await w.prepare(ctx('h', 'a'), 'send', t1)
  await assert.rejects(w.prepare(ctx('h', 'b'), 'send', t2), /already leased/)
  await assert.rejects(w.prepare(ctx('h', 'a'), 'send', t2), /different transaction/)
  assert.equal((await w.submit('a')).state, 'rejected')
  // a definitively-rejected tx is dead-in-mempool — the lease follows it;
  // outpoint liveness is custody's call (spendCheck/liveSet), not the lease's
  assert.equal((await w.prepare(ctx('h', 'b'), 'send', t2)).state, 'prepared', 'a dead tx frees its inputs for the next attempt')
  // once bytes air the inputs' fate is the tx's fate — custody tracks it.
  // A lease that outlives its broadcast wedges the change chain it consumed
  reply = { status: 'success' }
  assert.equal((await w.submit('b')).state, 'submitted')
  const t3 = await build()
  assert.equal((await w.prepare(ctx('h', 'c'), 'send', t3)).state, 'prepared', 'an aired tx releases its claim to custody')
  const bare = new Transaction()
  bare.addInput({ sourceTXID: 'a'.repeat(64), sourceOutputIndex: 0 })
  bare.addOutput({ satoshis: 1, lockingScript: new P2PKH().lock(funder.address()) })
  await assert.rejects(w.prepare(ctx('h', 'c'), 'send', bare), /real source transaction/)
  const o2 = await open()
  t.after(() => o2.kx.destroy())
  const store2 = o2.store
  reply = { status: 'error', description: 'SpendGuard refused: fee 5000 sats > maxFeePerTx 1000' }
  const ex2 = new Dispatcher()
  t.after(() => ex2.close())
  const w2 = new WalletCoordinator(store2, { broadcast: async () => reply, exec: ex2 })
  wireTx(ex2, { store: store2, fx: {}, log: () => {}, submit: (id) => w2.submit(id) })
  await w2.prepare(ctx('h', 'a'), 'send', t1)
  assert.equal((await w2.submit('a')).state, 'rejected')
  assert.equal((await w2.prepare(ctx('h', 'b'), 'send', t2)).state, 'prepared', 'a tx that never left the process frees its inputs')
})

test('wallet: first-party deaths run custody hooks — txDied on refusal/rejection, txSpent on air', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const funder = dryFunder()
  const mkfund = (seed: string) => {
    const ftx = new Transaction()
    ftx.addInput({ sourceTXID: seed, sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromHex('51') })
    ftx.addOutput({ satoshis: 50_000, lockingScript: new P2PKH().lock(funder.keyPair.toPublicKey().toAddress()) })
    return { tx: ftx, vout: 0, satoshis: 50_000 }
  }
  const f = mkfund('00'.repeat(32))
  const f2 = mkfund('11'.repeat(32))
  const build = async (src = f) => {
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: src.tx, sourceOutputIndex: src.vout, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
    tx.addOutput({ satoshis: src.satoshis - 100, lockingScript: new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toAddress([0x6f])) })
    await tx.sign()
    return tx
  }
  // the SpendGuard-refused shape that leaked phantom change: refused at the
  // gate → txDied gets the rebuilt tx so custody drops its outputs
  const died: string[] = []
  const spent: string[] = []
  const hooks: Pick<WalletDeps, 'txDied' | 'txSpent'> = {
    txDied: (tx) => void died.push(tx.id('hex')),
    txSpent: (tx) => void spent.push(tx.id('hex'))
  }
  // one dispatcher per store: the three coordinators below ride the same
  // observe handler — their custody deps are identical by construction
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const w = new WalletCoordinator(store, {
    broadcast: async () => ({ status: 'error', description: 'SpendGuard refused: daily cap' }),
    ...hooks,
    exec: ex
  })
  wireTx(ex, { store, fx: fxFor(w, hooks), log: () => {} })
  const t1 = await build()
  await w.prepare(ctx('h', 'a'), 'send', t1)
  assert.equal((await w.submit('a')).state, 'rejected')
  assert.deepEqual(died, [t1.id('hex')], 'the refusal hands custody the tx to un-spend')
  assert.equal(spent.length, 0)
  // a transport failure is NOT a death — undecidable, no hooks
  const w2 = new WalletCoordinator(store, {
    broadcast: async () => {
      throw new Error('socket died')
    },
    ...hooks,
    exec: ex
  })
  const t2 = await build()
  await w2.prepare(ctx('h', 'b'), 'send', t2)
  assert.equal((await w2.submit('b')).state, 'submission_unknown')
  assert.deepEqual(died, [t1.id('hex')], 'submission_unknown must not kill custody')
  // a network-observed rejection/conflict is a death too
  const w3 = new WalletCoordinator(store, {
    broadcast: async () => ({ status: 'success' }),
    ...hooks,
    exec: ex
  })
  // t2's input stays leased to 'b' — submission_unknown is undecidable, so a
  // new attempt must draw on a different coin (exactly custody's contract)
  const t3 = await build(f2)
  await w3.prepare(ctx('h', 'c'), 'send', t3)
  assert.equal((await w3.submit('c')).state, 'submitted')
  assert.deepEqual(spent, [t3.id('hex')], 'a successful submit settles custody')
  assert.equal(await w3.observe(t3.id('hex'), { status: 'rejected', source: 'arc', reason: 'double spend' }), 'rejected')
  assert.deepEqual(died, [t1.id('hex'), t3.id('hex')], 'a late chain rejection also returns custody')
})

test('wallet: a refused tx aired by the counterparty revives custody, not a double-take', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const funder = dryFunder()
  const mkfund = (seed: string) => {
    const ftx = new Transaction()
    ftx.addInput({ sourceTXID: seed, sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromHex('51') })
    ftx.addOutput({ satoshis: 50_000, lockingScript: new P2PKH().lock(funder.keyPair.toPublicKey().toAddress()) })
    return { tx: ftx, vout: 0, satoshis: 50_000 }
  }
  const f = mkfund('00'.repeat(32))
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: f.tx, sourceOutputIndex: f.vout, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
  tx.addOutput({ satoshis: f.satoshis - 100, lockingScript: new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toAddress([0x6f])) })
  await tx.sign()
  const died: string[] = []
  const spent: string[] = []
  const hooks: Pick<WalletDeps, 'txDied' | 'txSpent'> = {
    txDied: (x) => void died.push(x.id('hex')),
    txSpent: (x) => void spent.push(x.id('hex'))
  }
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const w = new WalletCoordinator(store, {
    broadcast: async () => ({ status: 'error', description: 'SpendGuard refused: daily cap' }),
    ...hooks,
    exec: ex
  })
  wireTx(ex, { store, fx: fxFor(w, hooks), log: () => {} })
  await w.prepare(ctx('h', 'a'), 'send', tx)
  assert.equal((await w.submit('a')).state, 'rejected', 'our own broadcast never left')
  assert.deepEqual(died, [tx.id('hex')], 'the refusal freed custody')
  assert.equal(spent.length, 0)
  // the accept carried the signed bytes anyway — the seller re-aired them
  // through their own guard. 'SpendGuard refused' never meant 'not on the
  // wire': the observation must re-mark custody or the released-but-spent
  // input gets re-taken into a doomed tx and the change stays invisible.
  assert.equal(await w.observe(tx.id('hex'), { status: 'seen', source: 'arc' }), 'observed')
  assert.deepEqual(spent, [tx.id('hex')], 'a refused-but-aired tx re-marks custody — inputs are provably spent, change is ours')
})

test('watcher: recently-rejected rows still re-observe — the counterparty-air window', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const funder = dryFunder()
  const ftx = new Transaction()
  ftx.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromHex('51') })
  ftx.addOutput({ satoshis: 50_000, lockingScript: new P2PKH().lock(funder.keyPair.toPublicKey().toAddress()) })
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: ftx, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
  tx.addOutput({ satoshis: 49_900, lockingScript: new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toAddress([0x6f])) })
  await tx.sign()
  const revived: string[] = []
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const w = new WalletCoordinator(store, { broadcast: async () => ({ status: 'error', description: 'SpendGuard refused: daily cap' }), exec: ex })
  // driver mode: the dead→live revive fires the 'tx-spent' intent on the
  // rejected --obs.seen--> observed edge — the same custody re-mark the
  // legacy onRevived hook ran (the observe body runs it via wireTx's fx)
  const effects: TxEffects = { txSpent: (row) => void revived.push(row.txid) }
  wireTx(ex, { store, fx: effects, log: () => {}, submit: (id) => w.submit(id) })
  await w.prepare(ctx('h', 'a'), 'send', tx)
  assert.equal((await w.submit('a')).state, 'rejected')
  const { TransactionWatcher } = await import('../src/ledger.js')
  const watcher = new TransactionWatcher(store, async () => ({ status: 'seen' as const, source: 't' }), undefined, { effects, exec: ex })
  assert.equal(await watcher.tick(), 1, 'the rejected row is inside its re-air window and must be re-observed')
  assert.deepEqual(revived, [tx.id('hex')])
  assert.equal((await store.walletTx({ actionId: 'a' }))?.state, 'observed')
  assert.equal(await watcher.tick(), 0, 'observed settles — no repeat revive')
})

test('wallet leases: a released row is dead to take() and prepare() readers alike', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const txid = (c: string) => c.repeat(64)
  // arm 1 — released lease with NO tx row (the escrow_never_built shape:
  // releaseLeases ran before any record existed)
  assert.equal(await store.leaseOutpoints('act-a', [{ txid: txid('a'), vout: 0, satoshis: 100 }]), true)
  await store.releaseLeases('act-a')
  // arm 2 — released lease beside a live-looking 'prepared' tx row
  await store.recordPrepared({ actionId: 'act-b', operationId: 'op', kind: 'send', txid: txid('b'), rawHex: '00', parents: {}, feeSats: 1 }, [
    { txid: txid('b'), vout: 0, satoshis: 100 }
  ])
  await store.releaseLeases('act-b')
  // arm 3 — released lease beside 'submission_unknown' — the wedge that used
  // to read live forever while the observer waited on an unaired tx
  await store.recordPrepared({ actionId: 'act-c', operationId: 'op', kind: 'send', txid: txid('c'), rawHex: '00', parents: {}, feeSats: 1 }, [
    { txid: txid('c'), vout: 0, satoshis: 100 }
  ])
  await store.setTxState('act-c', 'submission_unknown')
  await store.releaseLeases('act-c')
  // take()'s reader and prepare()'s reader MUST agree — a released row freed
  // by one but not the other was the terminal funding_failed wedge
  for (const c of ['a', 'b', 'c']) {
    const id = txid(c)
    assert.equal(await store.leasedOutpoint(id, 0), false, `${c}: released lease is not live to take()`)
    assert.equal(await store.leaseOutpoints(`act-next-${c}`, [{ txid: id, vout: 0, satoshis: 100 }]), true, `${c}: released lease does not block prepare()`)
  }
  // control — a live lease still blocks both readers
  assert.equal(await store.leaseOutpoints('act-live', [{ txid: txid('d'), vout: 0, satoshis: 100 }]), true)
  assert.equal(await store.leasedOutpoint(txid('d'), 0), true, 'a live lease stays live to take()')
  assert.equal(await store.leaseOutpoints('act-other', [{ txid: txid('d'), vout: 0, satoshis: 100 }]), false, 'a live lease still blocks prepare()')
})

test('wallet: lease+record are atomic — a lease conflict leaves no tx row behind', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const txid = (c: string) => c.repeat(64)
  // act-a holds the live lease on 'a':0
  assert.equal(await store.leaseOutpoints('act-a', [{ txid: txid('a'), vout: 0, satoshis: 100 }]), true)
  // act-b tries to prepare over it — the conflict must refuse AND roll the
  // tx row back with the lease (the old split write could commit one and
  // crash before the other: a 'leased'-with-no-tx wedge either way)
  await assert.rejects(
    store.recordPrepared({ actionId: 'act-b', operationId: 'op', kind: 'send', txid: txid('b'), rawHex: '00', parents: {}, feeSats: 1 }, [
      { txid: txid('a'), vout: 0, satoshis: 100 }
    ]),
    /already leased/
  )
  assert.equal(await store.walletTx({ actionId: 'act-b' }), undefined, 'a refused prepare leaves no tx row')
  // and the lease survives intact — the failed attempt didn't touch it
  assert.equal((await store.leaseState(txid('a'), 0))?.actionId, 'act-a')
})

test('classifyDeliveryError: funding starvation vs transport vs unclassed', () => {
  assert.equal(classifyDeliveryError('no funding slot'), 'funding-slot')
  assert.equal(classifyDeliveryError('The operation was aborted due to timeout'), 'transport')
  assert.equal(classifyDeliveryError('fetch failed'), 'transport')
  assert.equal(classifyDeliveryError('getaddrinfo ENOTFOUND host'), 'transport')
  assert.equal(classifyDeliveryError('http 503: down'), 'transport')
  assert.equal(classifyDeliveryError('socket hang up'), 'transport')
  // broadcaster code 476 / locktime-not-yet is a timing wait — retry, never dead-letter
  assert.equal(classifyDeliveryError('broadcaster REJECTED: non-final: lockTime 2100 not reached'), 'transport')
  assert.equal(classifyDeliveryError('broadcaster HTTP 400: error 476'), 'transport')
  assert.equal(classifyDeliveryError('range: content too large'), undefined, 'a payload-size fault is not a delivery class')
  assert.equal(classifyDeliveryError('policy denied'), undefined)
})

test('outbox defer classes: unblockOutbox keys on the class, not the error text', async (t) => {
  let now = 1_000
  const { kx, store } = await open(':memory:', () => now)
  t.after(() => kx.destroy())
  await store.enqueue({ actionId: 'a', operationId: 'lanes', kind: 'repair-host', destination: 'h1', payload: { key: 'k' } })
  await store.enqueue({ actionId: 'b', operationId: 'lanes', kind: 'repair-host', destination: 'h2', payload: { key: 'k' } })
  await store.enqueue({ actionId: 'c', operationId: 'lanes', kind: 'repair-host', destination: 'h3', payload: { key: 'k' } })
  await store.retryOutbox('a', 'fetch failed', undefined, 'transport')
  await store.retryOutbox('b', 'net down') // classless
  await store.deferOutbox('c', 'no funding slot')
  assert.equal((await store.dueOutbox()).length, 0, 'all three rows deferred')
  assert.equal(await store.unblockOutbox('transport'), 1, 'only the transport-classed row wakes')
  assert.deepEqual(
    (await store.dueOutbox()).map((o) => o.actionId),
    ['a']
  )
  await store.ackOutbox('a')
  assert.equal(await store.unblockOutbox('transport'), 0, 'classless and funding-slot rows are not transport')
  assert.equal(await store.unblockOutbox('funding-slot'), 1)
  assert.deepEqual(
    (await store.dueOutbox()).map((o) => o.actionId),
    ['c']
  )
})

test('enqueue delayMs: a delayed row is not due until the delay passes', async (t) => {
  let now = 1_000
  const { kx, store } = await open(':memory:', () => now)
  t.after(() => kx.destroy())
  await store.enqueue({ actionId: 'now', operationId: 'lanes', kind: 'repair-host', destination: 'h', payload: { key: 'k' } })
  await store.enqueue({ actionId: 'later', operationId: 'lanes', kind: 'repair-host', destination: 'h', payload: { key: 'k' } }, { delayMs: 20_000 })
  assert.deepEqual(
    (await store.dueOutbox()).map((o) => o.actionId),
    ['now'],
    'the delayed row waits'
  )
  now += 20_001
  assert.deepEqual((await store.dueOutbox()).map((o) => o.actionId).sort(), ['later', 'now'].sort())
})

// Batch H: the abandon reap drops every unproven row, but only self-minted
// trees ghost-mint — a provision row's inputs are the payer's, not ours
test('abandonedReap: provision rows drop without ghost-mint; self-minted kinds ghost', async () => {
  const mod = (await import('../src/runtime.js')) as { abandonedReap?: (kind: string) => { drop: boolean; ghost: boolean } }
  assert.ok(mod.abandonedReap !== undefined, 'runtime must export abandonedReap')
  assert.deepEqual(mod.abandonedReap!('provision'), { drop: true, ghost: false })
  assert.deepEqual(mod.abandonedReap!('market'), { drop: true, ghost: true })
  assert.deepEqual(mod.abandonedReap!('bbox'), { drop: true, ghost: true })
})

// Stage 7 gate — a silent wait must carry its reason: the deferred outbox
// row is what 'deals {operationId}' renders as the waiting-on line
test('waitingOn: deferred rows surface their class and plain reason', async () => {
  const { store, kx } = await open()
  await store.enqueue({ actionId: 'a1', operationId: 'op1', kind: 'offer', destination: 'host:x', payload: { hi: 1 } })
  await store.enqueue({ actionId: 'a2', operationId: 'op1', kind: 'pay', destination: 'host:y', payload: {} })
  await store.deferOutbox('a1', 'no spendable funding slot', 2_000, 'funding-slot')
  const w = await store.waitingOn('op1')
  assert.equal(w.length, 2)
  const slot = w.find((r) => r.actionId === 'a1')!
  assert.equal(slot.status, 'pending')
  assert.equal(slot.deferClass, 'funding-slot')
  assert.equal(slot.lastError, 'no spendable funding slot')
  assert.ok(slot.nextAt > 0)
  const plain = w.find((r) => r.actionId === 'a2')!
  assert.equal(plain.deferClass, undefined)
  // acked rows drop out; dead rows stay (a dead row's error IS the reason)
  await store.ackOutbox('a2')
  assert.equal((await store.waitingOn('op1')).length, 1)
  for (let i = 0; i < 10; i++) await store.retryOutbox('a1', 'host unreachable', 2)
  const dead = (await store.waitingOn('op1'))[0]
  assert.equal(dead.status, 'dead')
  assert.equal(dead.lastError, 'host unreachable')
  await kx.destroy()
})

test('market: refund window inside the seller safety margin refuses at construction', async () => {
  const { MarketEngine } = await import('../src/market.js')
  const { store, kx } = await open()
  const deps = (opts: { refundBlocks: number; sellerSafetyBlocks?: number }) =>
    ({
      store,
      call: async () => ({}),
      memory: {} as never,
      key: PrivateKey.fromRandom(),
      network: 'regtest',
      height: async () => 1,
      fetchTx: async () => undefined,
      transport: {} as never,
      ai: {} as never,
      opts
    }) as never
  assert.throws(() => new MarketEngine(deps({ refundBlocks: 2 })), /refundBlocks.*sellerSafetyBlocks/)
  assert.throws(() => new MarketEngine(deps({ refundBlocks: 5, sellerSafetyBlocks: 5 })), /refundBlocks.*sellerSafetyBlocks/)
  assert.doesNotThrow(() => new MarketEngine(deps({ refundBlocks: 3, sellerSafetyBlocks: 2 })))
  await kx.destroy()
})

test('splitDailySpend: escrow/payment kinds are deal spend; the rest is upkeep', async () => {
  const { splitDailySpend } = await import('../src/ledger.js')
  const { store, kx } = await open()
  const tx = (id: string) => ({ txid: id, rawHex: '00', parents: {}, feeSats: 10 })
  await store.recordPrepared({ ...tx('aa'.repeat(32)), actionId: 'a-esc', operationId: 'op', kind: 'escrow' })
  await store.recordPrepared({ ...tx('bb'.repeat(32)), actionId: 'a-pay', operationId: 'op', kind: 'payment' })
  await store.recordPrepared({ ...tx('cc'.repeat(32)), actionId: 'a-mint', operationId: 'op', kind: 'anchor' })
  const today = new Date().toISOString()
  const costs = [
    { txid: 'aa'.repeat(32), spend: 300, at: today },
    { txid: 'bb'.repeat(32), spend: 200, at: today },
    { txid: 'cc'.repeat(32), spend: 150, at: today },
    { txid: 'dd'.repeat(32), spend: 50, at: today }, // no wallet row — upkeep by default
    { txid: 'ee'.repeat(32), spend: 999, at: '2000-01-01T00:00:00Z' } // other day — ignored
  ]
  const { deal, upkeep } = await splitDailySpend(store, costs)
  assert.equal(deal, 500)
  assert.equal(upkeep, 200)
  await kx.destroy()
})

// the set-shaped reads behind the hot loop + the journal batch — each must
// agree with the per-row query it replaces
test('aggregates: opsByIds, eventCountsByOp, hasLiveOps, hasDueAtHeight', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const mk = async (dedupeKey: string, extra: { state?: string; data?: Record<string, unknown>; dueHeight?: number } = {}) => {
    const r = await store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey,
      policyHash: 'h',
      network: 'regtest',
      state: extra.state ?? 'collecting',
      data: extra.data ?? {},
      ...(extra.dueHeight !== undefined ? { dueHeight: extra.dueHeight } : {})
    })
    return r!.operation.operationId
  }
  assert.equal(await store.hasLiveOps(['collecting']), false, 'empty store has no live ops')
  assert.equal((await store.opsByIds([])).size, 0)
  assert.equal((await store.eventCountsByOp([], 99)).size, 0)
  assert.equal(await store.hasLiveOps([]), false)
  assert.equal(await store.hasDueAtHeight([], 100), false)

  const a = await mk('d:a', { dueHeight: 100 })
  const b = await mk('d:b')
  await store.transition(a, 1, 'ranked', { cause: 'x' })

  const ops = await store.opsByIds([a, b, 'missing'])
  assert.equal(ops.size, 2)
  assert.equal(ops.get(a)?.state, 'ranked')
  assert.equal(ops.get(b)?.data.escrowTxid, undefined)

  // agrees with per-row eventCount at every cursor boundary
  for (const cursor of [0, 1, 2, 99]) {
    const counts = await store.eventCountsByOp([a, b], cursor)
    for (const id of [a, b]) assert.equal(counts.get(id) ?? 0, await store.eventCount(id, cursor), `cursor ${cursor} op ${id}`)
  }

  assert.equal(await store.hasLiveOps(['collecting', 'ranked']), true)
  // a settled proof waiting only on block depth is not live work
  const c = await mk('d:c', { state: 'completion_submitted', data: { provenHeight: 50 } })
  assert.ok(c.length > 0)
  assert.equal(await store.hasLiveOps(['completion_submitted']), false)
  assert.equal(await store.hasLiveOps(['collecting', 'completion_submitted']), true, 'live ops elsewhere keep it true')
  // the same state WITHOUT provenHeight is still live (proof not yet recorded)
  await mk('d:d', { state: 'completion_submitted' })
  assert.equal(await store.hasLiveOps(['completion_submitted']), true)
  assert.equal(await store.hasLiveOps(['settled']), false, 'a terminal-only set is not live')

  // due_height boundary: tip == due_height fires, tip below does not
  assert.equal(await store.hasDueAtHeight(['collecting', 'ranked'], 99), false)
  assert.equal(await store.hasDueAtHeight(['collecting', 'ranked'], 100), true)
  assert.equal(await store.hasDueAtHeight(['expired'], 100), false, 'the state filter still applies')
  void c
})
