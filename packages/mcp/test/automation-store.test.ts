import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import knexFactory from 'knex'
import { PrivateKey, P2PKH, Transaction, UnlockingScript } from '@bsv/sdk'
import { stackWorld, stackChain, StackFunder } from '@nektar/chain'
import { AutomationStore, RevisionConflict } from '../src/automation/store.js'
import { WalletCoordinator } from '../src/automation/wallet.js'
import { policyHash, policyError, decidePurchase, decideOffer, type AutomationPolicy, type ActionContext } from '../src/automation/policy.js'
import { DEFAULT_SCORE_WEIGHTS } from '../src/automation/scoring.js'

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
      data: { wantId: 'w1' }
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
    data: { offers: 0 },
    outbox: [{ actionId: `${id}:publish`, kind: 'want', destination: 'rv', payload: { x: 1 } }]
  })
  assert.equal(next.revision, 2)
  assert.deepEqual(next.data, { offers: 0, wantId: 'w1' })
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

async function payment(funder: StackFunder, sats = 500) {
  const f = await funder.take(sats + 200)
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: f.tx, sourceOutputIndex: f.vout, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
  tx.addOutput({ satoshis: sats, lockingScript: new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toAddress([0x6f])) })
  tx.addOutput({ satoshis: f.satoshis - sats - 50, lockingScript: new P2PKH().lock(funder.address()) })
  await tx.sign()
  return tx
}

test('wallet: bytes persisted before submission; a crash between prepare and submit resumes the SAME tx', async (t) => {
  const file = join(mkdtempSync(join(tmpdir(), 'nektar-auto-')), 'a.sqlite')
  const w = await stackWorld(t, 3)
  if (w === undefined) return
  const { chain, funder } = w
  const tx = await payment(funder)
  const a = await open(file)
  const seen: string[] = []
  const w1 = new WalletCoordinator(a.store, {
    broadcast: async () => {
      throw new Error('process died')
    }
  })
  const prepared = await w1.prepare(ctx('h', 'pay:1'), 'send', tx)
  assert.equal(prepared.state, 'prepared')
  assert.equal(prepared.feeSats, 50)
  assert.equal((await w1.submit('pay:1')).state, 'submission_unknown', 'a transport failure is undecidable, not a rejection')
  assert.equal((await a.store.leaseState(tx.inputs[0].sourceTXID!, 0))?.state, 'leased', 'inputs stay leased while the outcome is unknown')
  await a.kx.destroy()
  const b = await open(file)
  t.after(() => b.kx.destroy())
  const w2 = new WalletCoordinator(b.store, {
    broadcast: async (x) => {
      seen.push(x.id('hex'))
      return await chain.broadcast(x)
    }
  })
  assert.deepEqual(await w2.resubmitPending(), [{ actionId: 'pay:1', state: 'submitted' }])
  assert.deepEqual(seen, [tx.id('hex')], 'the recorded bytes were sent — nothing was rebuilt')
  assert.equal((await w2.submit('pay:1')).state, 'submitted', 'resubmitting an accepted tx is harmless')
  assert.equal(await w2.observe(tx.id('hex'), { status: 'seen', source: 'arc' }), 'observed')
  assert.equal(await w2.observe(tx.id('hex'), { status: 'proven', source: 'spv', blockHeight: 7 }), 'confirmed')
  assert.equal((await b.store.leaseState(tx.inputs[0].sourceTXID!, 0))?.state, 'spent')
  assert.equal(await w2.observe('f'.repeat(64), { status: 'seen', source: 'arc' }), undefined, 'untracked txids change nothing')
})

test('wallet: one outpoint, one live action; guard refusal releases, network rejection does not', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const funder = StackFunder.dry()
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
  let reply = { status: 'error' as const, description: 'broadcaster: REJECTED — missing inputs' }
  const w = new WalletCoordinator(store, { broadcast: async () => reply })
  const t1 = await build(),
    t2 = await build()
  await w.prepare(ctx('h', 'a'), 'send', t1)
  await assert.rejects(w.prepare(ctx('h', 'b'), 'send', t2), /already leased/)
  await assert.rejects(w.prepare(ctx('h', 'a'), 'send', t2), /different transaction/)
  assert.equal((await w.submit('a')).state, 'rejected')
  await assert.rejects(w.prepare(ctx('h', 'b'), 'send', t2), /already leased/, 'a signed tx that reached the network keeps its inputs encumbered')
  const bare = new Transaction()
  bare.addInput({ sourceTXID: 'a'.repeat(64), sourceOutputIndex: 0 })
  bare.addOutput({ satoshis: 1, lockingScript: new P2PKH().lock(funder.address()) })
  await assert.rejects(w.prepare(ctx('h', 'c'), 'send', bare), /real source transaction/)
  const o2 = await open()
  t.after(() => o2.kx.destroy())
  const store2 = o2.store
  reply = { status: 'error', description: 'SpendGuard refused: fee 5000 sats > maxFeePerTx 1000' }
  const w2 = new WalletCoordinator(store2, { broadcast: async () => reply })
  await w2.prepare(ctx('h', 'a'), 'send', t1)
  assert.equal((await w2.submit('a')).state, 'rejected')
  assert.equal((await w2.prepare(ctx('h', 'b'), 'send', t2)).state, 'prepared', 'a tx that never left the process frees its inputs')
})
