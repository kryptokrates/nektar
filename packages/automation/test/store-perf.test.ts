import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory, { type Knex } from 'knex'
import { AutomationStore, type Operation } from '../src/store.js'

// storage-efficiency contract: the per-tick reads seek indexes instead of
// scanning, the reconcile cooldown is durable metadata, and the retention
// sweep only kills terminal rows past the cutoff. These are plan-shape
// assertions — a regression that drops an index or re-adds json_extract
// fails here even when every result is still correct.

async function open(filename = ':memory:') {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename }, useNullAsDefault: true })
  const store = new AutomationStore(kx)
  await store.init()
  return { kx, store }
}

async function op(store: AutomationStore, state: string, id: string, data: Record<string, unknown> = {}, role: Operation['role'] = 'buyer') {
  const { operation } = await store.createOperation({ role, kind: 'purchase', dedupeKey: id, policyHash: 'x', network: 'regtest', state, data })
  return operation
}

const plan = async (kx: Knex, sql: string, params: unknown[] = []): Promise<string[]> =>
  ((await kx.raw(`EXPLAIN QUERY PLAN ${sql}`, params as Knex.RawBinding[])) as Array<{ detail: string }>).map((r) => r.detail)

const seeks = (details: string[], indexFrag: string): void =>
  assert.ok(
    details.some((d) => d.startsWith('SEARCH') && d.includes(indexFrag)),
    `expected a SEARCH on an index matching '${indexFrag}', got: ${details.join(' | ')}`
  )

test('hot reads use their queue-head indexes', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  // seed enough terminal mass that the planner treats 'pending' as the
  // selective predicate — on a near-empty table a SCAN is legitimate
  for (let i = 0; i < 80; i++) {
    await kx('market_inbox').insert({
      source: 's',
      message_id: `done-${i}`,
      artifact_hash: `h${i}`,
      kind: 'want',
      payload: '{}',
      status: 'done',
      attempts: 0,
      received_at: i
    })
    await kx('market_outbox').insert({
      action_id: `ack-${i}`,
      operation_id: 'o',
      kind: 'message',
      destination: 'h',
      payload: '{}',
      status: 'acked',
      attempts: 0,
      next_at: i
    })
  }
  await kx('market_inbox').insert({
    source: 's',
    message_id: 'pend',
    artifact_hash: 'h',
    kind: 'want',
    payload: '{}',
    status: 'pending',
    attempts: 0,
    received_at: 200
  })
  await kx('market_outbox').insert({
    action_id: 'pend',
    operation_id: 'o',
    kind: 'message',
    destination: 'h',
    payload: '{}',
    status: 'pending',
    attempts: 0,
    next_at: 5
  })
  // mirror of pendingInbox()'s WHERE
  seeks(await plan(kx, `SELECT * FROM market_inbox WHERE status = 'pending' AND attempts < 8 ORDER BY received_at LIMIT 50`), 'market_inbox_status_received')
  // mirror of dueOutbox()'s WHERE
  seeks(
    await plan(kx, `SELECT * FROM market_outbox WHERE status = 'pending' AND next_at <= ? ORDER BY next_at LIMIT 50`, [Date.now()]),
    'market_outbox_status_next'
  )
  // mirror of hasDueAtHeight()'s WHERE — composite (state, due_height)
  for (let i = 0; i < 60; i++) await op(store, 'settled', `t-${i}`)
  const od = await op(store, 'watching', 'due-op', { escrow: 1 })
  await kx('market_operations').where({ operation_id: od.operationId }).update({ due_height: 100 })
  const d = await plan(kx, `SELECT 1 FROM market_operations WHERE state IN ('watching', 'collecting') AND due_height <= ? LIMIT 1`, [200])
  seeks(d, 'market_operations_state_due_height')
})

test('hasLiveOps carries no json_extract — the column is the probe', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const o = await op(store, 'completion_submitted', 'cs')
  // awaiting depth (column NULL) → still live work
  assert.equal(await store.hasLiveOps(['completion_submitted']), true)
  await store.transition(o.operationId, o.revision, 'completion_submitted', { cause: 'disclosure_proof_awaiting_depth', data: { provenHeight: 500 } })
  // stamped → parked until the depth lands, not live
  assert.equal(await store.hasLiveOps(['completion_submitted']), false)
  // and the column the WHERE actually reads was written
  const row = await kx('market_operations').where({ operation_id: o.operationId }).first()
  assert.equal(row.proven_height, 500)
  // a data-merge that drops the key clears the column with it
  await store.transition(o.operationId, o.revision + 1, 'completion_submitted', { cause: 'perf_probe', data: { provenHeight: undefined } })
  const row2 = await kx('market_operations').where({ operation_id: o.operationId }).first()
  assert.equal(row2.proven_height, null)
})

test('pre-column ops table migrates: columns added, proven_height backfilled', async (t) => {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  t.after(() => kx.destroy())
  // the pre-column shape — no proven_height, no next_reconcile_at
  await kx.schema.createTable('market_operations', (tb) => {
    tb.string('operation_id').primary()
    tb.string('role').notNullable()
    tb.string('kind').notNullable()
    tb.string('dedupe_key').notNullable().unique()
    tb.string('policy_hash').notNullable()
    tb.string('network').notNullable()
    tb.string('state').notNullable()
    tb.integer('revision').notNullable()
    tb.text('data').notNullable()
    tb.bigInteger('due_at')
    tb.integer('due_height')
    tb.text('last_error')
    tb.bigInteger('created_at').notNullable()
    tb.bigInteger('updated_at').notNullable()
  })
  await kx('market_operations').insert({
    operation_id: 'old',
    role: 'buyer',
    kind: 'purchase',
    dedupe_key: 'old',
    policy_hash: 'x',
    network: 'regtest',
    state: 'completion_submitted',
    revision: 3,
    data: '{"provenHeight":777,"escrow":{}}',
    created_at: 1,
    updated_at: 1
  })
  const store = new AutomationStore(kx)
  await store.init()
  const r = await kx('market_operations').where({ operation_id: 'old' }).first()
  assert.equal(r.proven_height, 777, 'backfill pulled provenHeight out of the data JSON')
  assert.ok(await kx.schema.hasColumn('market_operations', 'next_reconcile_at'))
  assert.equal(r.revision, 3, 'the migration is schema-only — rows untouched')
  // and the store still hydrates the migrated row
  assert.equal((await store.operation('old'))!.state, 'completion_submitted')
})

test('reconcileDue scan: only due rows hydrate; markReconciled stamps the cooldown', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const now = Date.now()
  const a = await op(store, 'funding_failed', 'a')
  await op(store, 'funding_failed', 'b')
  // NULL cooldown = due — a boot or a never-probed row always scans
  let due = await store.operations({ role: 'buyer', states: ['funding_failed'], reconcileDue: now })
  assert.deepEqual(due.map((o) => o.operationId).sort(), [a.operationId, (await store.operationByKey('b'))!.operationId].sort())
  // stamp a's cooldown: it leaves the due set without any state write
  await store.markReconciled(a.operationId, now + 30_000)
  due = await store.operations({ role: 'buyer', states: ['funding_failed'], reconcileDue: now })
  assert.equal(due.length, 1)
  assert.equal(due[0].dedupeKey, 'b')
  // the stamp hydrates back through op()
  assert.equal((await store.operation(a.operationId))!.nextReconcileAt, now + 30_000)
  // a hint clearing the cooldown (markReconciled(0)) makes it due again
  await store.markReconciled(a.operationId, 0)
  due = await store.operations({ role: 'buyer', states: ['funding_failed'], reconcileDue: now })
  assert.equal(due.length, 2)
  // the scan itself seeks the composite, no hydration for cooled rows
  const d = await plan(
    kx,
    `SELECT * FROM market_operations WHERE role = 'buyer' AND state IN ('funding_failed') AND (next_reconcile_at IS NULL OR next_reconcile_at <= ?)`,
    [now]
  )
  seeks(d, 'market_operations_state_next_reconcile')
})

test('prune: terminal rows past the cutoff die; live and young rows keep their history', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const now = Date.now()
  const cutoff = now - 30 * 86_400_000
  const old = cutoff - 1_000
  const dead = await op(store, 'collecting', 'dead-op')
  const live = await op(store, 'collecting', 'live-op')
  await store.transition(dead.operationId, dead.revision, 'expired', { cause: 'no_qualifying_offer' })
  // backdate the dead op's whole journal; the live op's journal stays fresh-looking only by age — backdate it too: age alone must not kill a live op's events
  await kx('market_events').update({ at: old })
  // queue rows: old done/dead vs old pending vs young done
  await kx('market_inbox').insert({
    source: 's',
    message_id: 'old-done',
    artifact_hash: 'h',
    kind: 'want',
    payload: '{}',
    status: 'done',
    attempts: 0,
    received_at: old
  })
  await kx('market_inbox').insert({
    source: 's',
    message_id: 'old-pend',
    artifact_hash: 'h',
    kind: 'want',
    payload: '{}',
    status: 'pending',
    attempts: 0,
    received_at: old
  })
  await kx('market_inbox').insert({
    source: 's',
    message_id: 'new-done',
    artifact_hash: 'h',
    kind: 'want',
    payload: '{}',
    status: 'done',
    attempts: 0,
    received_at: now
  })
  await kx('market_outbox').insert({
    action_id: 'old-ack',
    operation_id: dead.operationId,
    kind: 'message',
    destination: 'h',
    payload: '{}',
    status: 'acked',
    attempts: 0,
    next_at: old
  })
  await kx('market_outbox').insert({
    action_id: 'old-dead',
    operation_id: dead.operationId,
    kind: 'message',
    destination: 'h',
    payload: '{}',
    status: 'dead',
    attempts: 10,
    next_at: old
  })
  await kx('market_outbox').insert({
    action_id: 'old-pend',
    operation_id: live.operationId,
    kind: 'message',
    destination: 'h',
    payload: '{}',
    status: 'pending',
    attempts: 0,
    next_at: old
  })
  // a confirmed tx's journal is complete — prunable; an abandoned one's can still revive — kept
  await kx('wallet_transactions').insert({
    action_id: 'tx-conf',
    operation_id: 'x',
    kind: 'escrow',
    txid: 'aa',
    raw_hex: 'aa',
    parents: '{}',
    fee_sats: 1,
    state: 'confirmed',
    attempts: 0,
    at: old
  })
  await kx('wallet_transactions').insert({
    action_id: 'tx-ab',
    operation_id: 'x',
    kind: 'escrow',
    txid: 'bb',
    raw_hex: 'bb',
    parents: '{}',
    fee_sats: 1,
    state: 'abandoned',
    attempts: 0,
    at: old
  })
  await kx('wallet_tx_events').insert({
    action_id: 'tx-conf',
    at: old,
    from_state: 'submitted',
    event: 'obs.proven',
    guard: 'proof',
    to_state: 'confirmed',
    intents: '[]',
    effected: 1
  })
  await kx('wallet_tx_events').insert({
    action_id: 'tx-ab',
    at: old,
    from_state: 'submitted',
    event: 'abandon',
    to_state: 'abandoned',
    intents: '[]',
    effected: 1
  })
  const n = await store.prune(cutoff, { ops: ['expired', 'canceled'], txs: ['confirmed'] })
  assert.deepEqual(n, { inbox: 1, outbox: 2, events: 2, txEvents: 1 })
  assert.equal((await kx('market_inbox').count('* as c').first())!.c, 2, 'pending-old + done-young survive')
  assert.equal((await kx('market_outbox').count('* as c').first())!.c, 1, 'pending survives')
  // live op keeps even old events; dead op's journal is gone
  assert.equal((await kx('market_events').where({ operation_id: live.operationId }).count('* as c').first())!.c, 1)
  assert.equal((await kx('market_events').where({ operation_id: dead.operationId }).count('* as c').first())!.c, 0)
  assert.equal((await kx('wallet_tx_events').where({ action_id: 'tx-conf' }).count('* as c').first())!.c, 0)
  assert.equal((await kx('wallet_tx_events').where({ action_id: 'tx-ab' }).count('* as c').first())!.c, 1, 'a revivable tx keeps its audit')
})
