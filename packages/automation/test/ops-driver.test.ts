import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { AutomationStore, MachineBlocked } from '../src/store.js'

// Phase C — ops.machine as DRIVER of transition(): every state-changing
// write must resolve its cause to a declared machine edge that accepts
// from the persisted state and lands exactly on the caller's target.
// Impossible or divergent writes roll back whole — state, event row,
// outbox — and surface as MachineBlocked, not a silent spec-less row.

async function open() {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx)
  await store.init()
  return { kx, store }
}

async function op(store: AutomationStore, state: string, id = `op-${state}`) {
  const { operation } = await store.createOperation({
    role: 'buyer',
    kind: 'purchase',
    dedupeKey: id,
    policyHash: 'x',
    network: 'regtest',
    state,
    data: {}
  })
  return operation
}

void test('driver: declared edge writes state + event atomically', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const o = await op(store, 'published')
  const next = await store.transition(o.operationId, o.revision, 'collecting', { cause: 'want_signed' })
  assert.equal(next.state, 'collecting')
  const evs = await store.events(o.operationId)
  assert.equal(evs[evs.length - 1].cause, 'want_signed')
})

void test('driver: impossible transition throws MachineBlocked and writes nothing', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const o = await op(store, 'collecting')
  const evBefore = (await store.events(o.operationId)).length
  // collecting --want_signed--> anything is not a declared edge
  await assert.rejects(store.transition(o.operationId, o.revision, 'collecting', { cause: 'want_signed' }), MachineBlocked)
  const row = await store.operation(o.operationId)
  assert.equal(row!.state, 'collecting')
  assert.equal(row!.revision, o.revision)
  assert.equal((await store.events(o.operationId)).length, evBefore)
})

void test('driver: divergent target is rejected — the machine decides where an edge lands', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const o = await op(store, 'collecting')
  // collect.expired is a real edge but targets 'expired', not 'selected'
  await assert.rejects(store.transition(o.operationId, o.revision, 'selected', { cause: 'no_qualifying_offer' }), MachineBlocked)
  assert.equal((await store.operation(o.operationId))!.state, 'collecting')
})

void test('driver: guarded siblings resolve by cause — unresolved→uncertain, other→escalated', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const a = await op(store, 'funding', 'op-fu')
  const up = await store.transition(a.operationId, a.revision, 'uncertain', { cause: 'needs_human:unresolved_payment' })
  assert.equal(up.state, 'uncertain')
  const b = await op(store, 'funding', 'op-fe')
  const es = await store.transition(b.operationId, b.revision, 'escalated', { cause: 'needs_human:stuck_state_timeout' })
  assert.equal(es.state, 'escalated')
  // crossed guards must NOT validate: unresolved cause cannot land 'escalated'
  const c = await op(store, 'funding', 'op-fx')
  await assert.rejects(store.transition(c.operationId, c.revision, 'escalated', { cause: 'needs_human:unresolved_payment' }), MachineBlocked)
})

void test('driver: multi-candidate cause picks the edge whose target matches', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const a = await op(store, 'disclosure_verified', 'op-sa')
  const up = await store.transition(a.operationId, a.revision, 'useful', { cause: 'sandbox_evaluation' })
  assert.equal(up.state, 'useful')
  const b = await op(store, 'disclosure_verified', 'op-sb')
  const dn = await store.transition(b.operationId, b.revision, 'not_useful', { cause: 'sandbox_evaluation' })
  assert.equal(dn.state, 'not_useful')
})

void test('driver: funding_deferred escrow-aired reconcile lands awaiting_disclosure', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const o = await op(store, 'funding_deferred')
  const next = await store.transition(o.operationId, o.revision, 'awaiting_disclosure', { cause: 'deferred_escrow_aired' })
  assert.equal(next.state, 'awaiting_disclosure')
})

void test('driver: tick_error keeps the residence clock — watchdog bound is not refreshed', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const o = await op(store, 'funding')
  const before = (await store.operation(o.operationId))!.updatedAt
  const next = await store.transition(o.operationId, o.revision, 'funding', { cause: 'tick_error', lastError: 'rpc timeout' })
  assert.equal(next.state, 'funding')
  assert.equal(next.updatedAt, before)
  const evs = await store.events(o.operationId)
  assert.equal(evs[evs.length - 1].cause, 'tick_error')
})

void test('driver: same-state data-stamp causes validate through their self-edges', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const o = await op(store, 'awaiting_disclosure')
  const next = await store.transition(o.operationId, o.revision, 'awaiting_disclosure', { cause: 'evidence_stored', data: { escrowTxid: 'x' } })
  assert.equal(next.state, 'awaiting_disclosure')
})

void test('driver: unmodeled cause passes through — the census gate owns it', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const o = await op(store, 'funding')
  const next = await store.transition(o.operationId, o.revision, 'funding', { cause: 'brand_new_unmapped_cause' })
  assert.equal(next.state, 'funding')
})

void test('driver: every production recovery transition agrees with ops.table.json', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const cases: Array<[from: string, to: string, cause: string]> = [
    ['completing', 'completion_failed', 'completion_refused'],
    ['funding', 'funding_failed', 'spend_guard_refused'],
    ['awaiting_disclosure', 'funding_failed', 'spend_guard_refused'],
    ['awaiting_disclosure', 'refunded', 'refund_observed_on_chain'],
    ['refund_pending', 'refunded', 'refund_observed_on_chain'],
    ['refund_pending', 'awaiting_disclosure', 'escrow_spent_by_completion'],
    ['uncertain', 'awaiting_disclosure', 'escrow_spent_by_completion'],
    ['seizing', 'watching', 'seize_not_yet'],
    ['seizing', 'seize_failed', 'seize_refused'],
    ['collecting', 'denied', 'policy_revoked_or_expired'],
    ['escalated', 'denied', 'action_reused_with_different_terms'],
    ['escalated', 'escalated', 'needs_human:daily_cap']
  ]
  for (let i = 0; i < cases.length; i++) {
    const [from, to, cause] = cases[i]
    const o = await op(store, from, `rec-${i}`)
    const next = await store.transition(o.operationId, o.revision, to, { cause })
    assert.equal(next.state, to, `${from} --${cause}--> ${to}`)
  }
})
