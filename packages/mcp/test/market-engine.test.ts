import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import knexFactory, { type Knex } from 'knex'
import { testChain, testFunder } from '@nektar/chain'
import { verifyWant, verifyOffer, type SignedWant, type SignedOffer } from '@nektar/bstack'
import { NektarMemory } from '../src/memory.js'
import { callTool } from '../src/server.js'
import { AutomationStore } from '@nektar/automation'
import { MarketEngine, Dispatcher, inertTxEffects, type MarketMessage, type WalletTx } from '@nektar/automation'
import { ScriptedExecutor } from '@nektar/automation'
import type { AutomationPolicy } from '@nektar/automation'
import { testPolicyFor, wireEngine } from './agentkit.js'
import { PrivateKey, Transaction } from '@bsv/sdk'

process.env.NEKTAR_PAID_DISCLOSURE = '1'
const FINDING = 'breaking change: libfoo 3.0 renamed connect() to open(); replace connect( with open( to fix the build'

interface Agent {
  name: string
  mem: NektarMemory
  store: AutomationStore
  kx: Knex
  engine: MarketEngine
  pub: string
  policyHash: string
  file: string
}

async function world(t: { skip: (m?: string) => void }, opts: { refundBlocks?: number; wantExtendMs?: number } = {}) {
  const chain = await testChain(t)
  if (chain === undefined) return undefined
  const dir = mkdtempSync(join(tmpdir(), 'nektar-market-'))
  const agents = new Map<string, Agent>()
  const feed: Array<{ seq: number; kind: string; payload: unknown }> = []
  const wire: string[] = []
  let clock = 1_800_000_000_000
  const now = () => clock
  const transportFor = (pub: string) => ({
    publishWant: (sw: SignedWant) => {
      assert.ok(verifyWant(sw.want, sw.sig))
      wire.push(JSON.stringify(sw))
      feed.push({ seq: feed.length + 1, kind: 'want', payload: sw })
      return Promise.resolve()
    },
    publishOffer: (so: SignedOffer) => {
      assert.ok(verifyOffer(so.offer, so.sig))
      wire.push(JSON.stringify(so))
      feed.push({ seq: feed.length + 1, kind: 'offer', payload: so })
      return Promise.resolve()
    },
    send: async (to: string, msg: MarketMessage) => {
      const a = [...agents.values()].find((x) => x.pub === to)
      if (!a) throw new Error('recipient offline')
      await a.engine.ingestMessage(pub, msg)
    }
  })
  const hooks = { afterPrepare: undefined as undefined | ((actionId: string) => void) }
  const engineFor = (mem: NektarMemory, store: AutomationStore) => {
    // engine-level suite: no runtime, so the executor is an in-process
    // dispatcher carrying the same deal-path bodies the endpoint would serve
    const ex = new Dispatcher()
    const engine = new MarketEngine({
      store,
      memory: mem,
      key: mem.operatorKey(),
      network: 'regtest',
      now,
      call: (tool, args) => callTool(mem, tool, args),
      height: () => mem.chainHeight(),
      fetchTx: async (txid) => chain.fetchTx(txid),
      transport: transportFor(mem.operatorKey().toPublicKey().toString()),
      ai: new ScriptedExecutor(),
      broadcast: (tx) => mem.broadcastTx(tx),
      settleEscrow: (tx) => mem.funds.spent(tx),
      afterPrepare: (id) => hooks.afterPrepare?.(id),
      executor: ex,
      opts: { refundBlocks: opts.refundBlocks ?? 144, ...(opts.wantExtendMs !== undefined ? { wantExtendMs: opts.wantExtendMs } : {}) }
    })
    const rebuild = (w: WalletTx) => engine.walletCoordinator!.rebuildTx(w)
    wireEngine(ex, engine, store, {
      fx: {
        ...inertTxEffects,
        txDied: async (w) => {
          await mem.funds.died(await rebuild(w))
        },
        txSpent: async (w) => {
          await mem.funds.spent(await rebuild(w))
        }
      },
      now
    })
    return engine
  }
  const policyFor = (pub: string, extra: Partial<AutomationPolicy['limits']> = {}): AutomationPolicy => testPolicyFor(pub, { limits: extra })
  const add = async (name: string, limits: Partial<AutomationPolicy['limits']> = {}): Promise<Agent> => {
    // real funding: anchors, escrows and refunds all broadcast real txs — a
    // dry funder fails at the first remember() (mid-migration regression)
    const funder = await testFunder(40)
    const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 1 }, modelId: 'test-model-1' })
    const file = join(dir, `${name}.sqlite`)
    const kx = knexFactory({ client: 'sqlite3', connection: { filename: file }, useNullAsDefault: true })
    const store = new AutomationStore(kx, now)
    await store.init()
    const pub = mem.operatorKey().toPublicKey().toString()
    const policyHash = await store.approvePolicy(policyFor(pub, limits), 'test:operator')
    const a: Agent = { name, mem, store, kx, engine: engineFor(mem, store), pub, policyHash, file }
    agents.set(name, a)
    everyone.push(a)
    return a
  }
  const seller = async (name: string, price: number) => {
    const a = await add(name)
    await a.mem.remember('guide', FINDING)
    await a.mem.remember('guide', 'unrelated note about gardening and tomatoes')
    await chain.mine(1)
    for (const l of a.mem.anchorLinks('guide')) a.mem.confirmAnchor('guide', l.txid)
    await a.mem.describeShard('guide', { domains: ['libfoo'], offers: [{ kind: 'record', priceSats: price }] })
    await a.engine.enableSeller(a.policyHash)
    return a
  }
  const cursors = new Map<string, number>()
  const round = async (only?: Agent[]) => {
    for (const a of only ?? agents.values()) {
      const since = cursors.get(a.name) ?? 0
      const fresh = feed.filter((e) => e.seq > since)
      if (fresh.length) {
        await a.engine.ingestEvents('hub', fresh, String(feed.length))
        cursors.set(a.name, feed.length)
      }
      await a.engine.tick()
    }
  }
  const restart = async (a: Agent) => {
    await a.kx.destroy()
    a.kx = knexFactory({ client: 'sqlite3', connection: { filename: a.file }, useNullAsDefault: true })
    a.store = new AutomationStore(a.kx, now)
    await a.store.init()
    a.engine = engineFor(a.mem, a.store)
  }
  const everyone: Agent[] = []
  const close = async () => {
    for (const a of everyone) await a.kx.destroy()
  }
  return {
    chain,
    agents,
    add,
    seller,
    round,
    restart,
    close,
    feed,
    wire,
    hooks,
    advance: (ms: number) => {
      clock += ms
    }
  }
}

void test('S1 regtest: three sellers quote 450/300/200, buyer scores, pays the best, verifies the disclosure — no manual tool calls', async (t) => {
  const w = await world(t)
  if (w === undefined) return
  t.after(w.close)
  const a = await w.seller('alice', 450),
    b = await w.seller('ben', 300),
    c = await w.seller('cara', 200)
  const bob = await w.add('bob')
  const { operationId, wantId } = await bob.engine.publishWant(
    { domains: ['libfoo'], question: 'libfoo 3.0 build fails: connect is not a function', maxSats: 500 },
    bob.policyHash
  )
  await w.round()
  await w.round()
  assert.equal(w.feed.filter((e) => e.kind === 'offer').length, 3, 'all three sellers answered the same want')
  for (const line of w.wire)
    for (const s of [a, b, c]) {
      const sealed = (await s.store.operations({ role: 'seller' })).find((o) => o.kind === 'sale')!.data.offer as any
      assert.ok(!line.includes(sealed.wrapped), 'the reveal credential never appears in a public artifact')
    }
  await w.round()
  assert.equal((await bob.engine.status(operationId))!.operation.state, 'collecting', 'the buyer waits for its collection window')
  w.advance(1500)
  await w.round([bob])
  let st = (await bob.engine.status(operationId))!.operation
  assert.equal(st.state, 'awaiting_disclosure')
  assert.equal((st.data.selected as any).key.split(':')[0], c.pub, 'the 200-sat offer wins when quality is equal')
  assert.deepEqual(
    (st.data.ranking as any).ranked.map((r: any) => r.maxAllInSats),
    [855, 955, 1105] // price + maxFee(~155 covenant bound) + 500 escrow reserve
  )
  await w.restart(bob)
  await w.round()
  await w.round()
  st = (await bob.engine.status(operationId))!.operation
  assert.equal(st.state, 'disclosure_verified', st.lastError)
  const result = st.data.result as any
  assert.equal(result.text, FINDING)
  assert.equal(result.verified, true)
  const sale = (await c.store.operations({ role: 'seller' })).find((o) => o.kind === 'sale')!
  assert.equal(sale.state, 'completion_submitted')
  const reveal = w.chain.getTx(sale.data.completeTxid as string)!
  assert.equal(reveal.outputs[0].satoshis, 200, 'the seller was paid exactly its quoted price on-chain')
  for (const loser of [a, b]) assert.equal((await loser.store.operations({ role: 'seller' })).find((o) => o.kind === 'sale')!.state, 'quoted')
  const budget = await bob.store.budgetStatus(bob.policyHash)
  assert.equal(budget.purchaseTodaySats, 200)
  assert.equal(budget.lockedSats, 0, 'locked capital is released once the disclosure settles')
  assert.equal((await bob.store.approvals()).length, 0, 'no human prompt for a policy-covered purchase')
  const again = await bob.engine.publishWant({ domains: ['libfoo'], maxSats: 500, wantId }, bob.policyHash)
  assert.equal(again.operationId, operationId, 'republishing the same want id is idempotent')
  await bob.engine.markUsefulness(operationId, true, { tests: 'pass' })
  assert.equal((await bob.engine.status(operationId))!.operation.state, 'useful')
  assert.equal(((await c.engine.status(sale.operationId))!.operation.data.offer as any).wrapped, '[sealed]', 'status never echoes the sealed credential')
})

void test('two wants from one buyer are answered and settled independently', async (t) => {
  const w = await world(t)
  if (w === undefined) return
  t.after(w.close)
  const c = await w.seller('cara', 200)
  const bob = await w.add('bob')
  const one = await bob.engine.publishWant({ domains: ['libfoo'], question: 'connect is not a function', maxSats: 500 }, bob.policyHash)
  const two = await bob.engine.publishWant({ domains: ['libfoo'], question: 'libfoo open build', maxSats: 400 }, bob.policyHash)
  assert.notEqual(one.wantId, two.wantId)
  await w.round()
  await w.round()
  w.advance(1500)
  for (let i = 0; i < 4; i++) await w.round()
  for (const id of [one.operationId, two.operationId]) assert.equal((await bob.engine.status(id))!.operation.state, 'disclosure_verified')
  assert.equal((await c.store.operations({ role: 'seller' })).filter((o) => o.kind === 'sale' && o.state === 'completion_submitted').length, 2)
  assert.equal((await bob.store.budgetStatus(bob.policyHash)).purchaseTodaySats, 400)
})

void test('outside policy → paused for a human; approval is bound to the exact action; denial stops it', async (t) => {
  const w = await world(t)
  if (w === undefined) return
  t.after(w.close)
  await w.seller('cara', 200)
  const bob = await w.add('bob', { maxLockedSats: 100 })
  const run = async () => {
    const r = await bob.engine.publishWant({ domains: ['libfoo'], question: 'connect is not a function', maxSats: 500 }, bob.policyHash)
    await w.round()
    await w.round()
    w.advance(1500)
    await w.round([bob])
    return r.operationId
  }
  const op1 = await run()
  assert.equal((await bob.engine.status(op1))!.operation.state, 'escalated')
  const [pending] = await bob.store.approvals('pending')
  assert.equal(pending.reason, 'locked_capital_above_policy')
  assert.equal(pending.maxExtraSats, 855)
  await w.round([bob])
  assert.equal((await bob.engine.status(op1))!.operation.state, 'escalated', 'nothing is spent while the approval is pending')
  assert.equal((await bob.store.budgetStatus(bob.policyHash)).purchaseTodaySats, 0)
  await bob.store.decideApproval(pending.approvalId, true, 'cli:operator')
  for (let i = 0; i < 3; i++) await w.round()
  assert.equal((await bob.engine.status(op1))!.operation.state, 'disclosure_verified')
  const op2 = await run()
  const [p2] = await bob.store.approvals('pending')
  await bob.store.decideApproval(p2.approvalId, false, 'cli:operator')
  await w.round([bob])
  assert.equal((await bob.engine.status(op2))!.operation.state, 'denied')
  assert.equal((await bob.store.budgetStatus(bob.policyHash)).purchaseTodaySats, 200, 'only the approved purchase was spent')
})

void test('seller goes dark after quoting → buyer refunds itself at the deadline; revoked policy stops new buying', async (t) => {
  const w = await world(t, { refundBlocks: 6 })
  if (w === undefined) return
  t.after(w.close)
  const c = await w.seller('cara', 200)
  const bob = await w.add('bob')
  const { operationId } = await bob.engine.publishWant({ domains: ['libfoo'], question: 'connect is not a function', maxSats: 500 }, bob.policyHash)
  await w.round()
  await w.round()
  w.advance(1500)
  w.agents.delete('cara')
  await w.round([bob])
  let st = (await bob.engine.status(operationId))!.operation
  assert.equal(st.state, 'awaiting_disclosure')
  assert.equal((await bob.store.budgetStatus(bob.policyHash)).lockedSats, 355) // 200 price + ~155 maxFee bound
  await w.round([bob])
  assert.equal((await bob.engine.status(operationId))!.operation.state, 'awaiting_disclosure', 'no early refund')
  // mine until the indexed tip the engine polls actually passes the refund
  // deadline — mine() returns before indexing catches up
  const deadline = (await bob.engine.status(operationId))!.operation.dueHeight!
  for (let i = 0; i < 30 && (await w.chain.tipHeight()) < deadline; i++) await w.chain.mine(1)
  await w.round([bob])
  st = (await bob.engine.status(operationId))!.operation
  assert.equal(st.state, 'refunded', st.lastError)
  assert.equal((await bob.store.budgetStatus(bob.policyHash)).lockedSats, 0)
  assert.equal((await c.store.operations({ role: 'seller' })).find((o) => o.kind === 'sale')!.state, 'quoted', 'the dark seller never completed')
  await bob.store.revokePolicy(bob.policyHash)
  await assert.rejects(bob.engine.publishWant({ domains: ['libfoo'], maxSats: 300 }, bob.policyHash), /no active buyer policy/)
})

void test('hostile offers are rejected with reasons and never funded', async (t) => {
  const w = await world(t)
  if (w === undefined) return
  t.after(w.close)
  const c = await w.seller('cara', 200)
  const bob = await w.add('bob')
  const { operationId } = await bob.engine.publishWant({ domains: ['libfoo'], question: 'connect is not a function', maxSats: 150 }, bob.policyHash)
  await w.round()
  await w.round()
  // past the want's own exp — collectMs only re-arms a still-live want now
  w.advance(125_000)
  await w.round([bob])
  const st = (await bob.engine.status(operationId))!.operation
  assert.equal(st.state, 'expired', 'a seller priced above the ceiling stays silent → no qualifying offer')
  assert.equal(w.feed.filter((e) => e.kind === 'offer').length, 0)
  assert.equal((await c.store.operations({ role: 'seller' })).filter((o) => o.kind === 'sale').length, 0, 'no preparation spent on an unaffordable want')
  assert.equal((await bob.store.budgetStatus(bob.policyHash)).runTotalSats, 0)
})

void test('crash between escrow prepare and submit: restart resubmits the SAME bytes, no second escrow, no operator prompt', async (t) => {
  const w = await world(t)
  if (w === undefined) return
  t.after(w.close)
  await w.seller('cara', 200)
  const bob = await w.add('bob')
  const { operationId } = await bob.engine.publishWant({ domains: ['libfoo'], question: 'connect is not a function', maxSats: 500 }, bob.policyHash)
  await w.round()
  await w.round()
  w.advance(1500)
  let crashed = false
  w.hooks.afterPrepare = () => {
    crashed = true
    throw new Error('simulated process death between prepare and submit')
  }
  await w.round([bob])
  assert.ok(crashed, 'the fault fired at the escrow prepare boundary')
  let st = (await bob.engine.status(operationId))!.operation
  assert.equal(st.state, 'funding')
  const pending = st.data.pendingEscrow as any
  assert.ok(pending?.utxo?.txid, 'the signed escrow was persisted before the crash')
  const recorded = await bob.store.walletTx({ actionId: `${operationId}:escrow:0` })
  assert.equal(recorded!.state, 'prepared', 'bytes persisted, never submitted')
  w.hooks.afterPrepare = undefined
  await w.restart(bob)
  for (let i = 0; i < 3; i++) await w.round()
  st = (await bob.engine.status(operationId))!.operation
  assert.equal(st.state, 'disclosure_verified', st.lastError)
  assert.equal((st.data.utxo as any).txid, pending.utxo.txid, 'the SAME escrow transaction was used after restart — no rebuild, no double fund')
  assert.equal((await bob.store.approvals()).length, 0, 'no human needed for a deterministic crash')
})

void test('minCollectMs: a cheaper offer inside the window still wins; a lone offer before the window does not select', async (t) => {
  const w = await world(t)
  if (w === undefined) return
  t.after(w.close)
  const a = await w.seller('alice', 450)
  const c = await w.seller('cara', 200)
  const bob = await w.add('bob')
  const { operationId } = await bob.engine.publishWant({ domains: ['libfoo'], question: 'connect is not a function', maxSats: 500 }, bob.policyHash)
  await w.round([bob])
  await w.round([a]) // alice's 450-sat offer lands first
  await w.round([bob])
  let st = (await bob.engine.status(operationId))!.operation
  assert.equal(st.state, 'collecting', 'inside minCollectMs a recorded offer does not early-select')
  w.advance(400)
  await w.round([c]) // cara's 200-sat offer lands inside the 1000ms window
  await w.round([bob])
  st = (await bob.engine.status(operationId))!.operation
  assert.equal(st.state, 'collecting', 'still collecting until the window closes')
  w.advance(700) // now ≥1100ms in — first acceptable offer may select
  await w.round([bob])
  st = (await bob.engine.status(operationId))!.operation
  assert.equal(st.state, 'awaiting_disclosure', st.lastError)
  assert.equal((st.data.selected as any).key.split(':')[0], c.pub, 'the cheaper inside-window offer wins')
})

void test('quote commitment attaches in every live state and is ignored once terminal', async (t) => {
  const w = await world(t)
  if (w === undefined) return
  t.after(w.close)
  const c = await w.seller('cara', 200)
  const bob = await w.add('bob')
  const { operationId, wantId } = await bob.engine.publishWant({ domains: ['libfoo'], question: 'connect is not a function', maxSats: 500 }, bob.policyHash)
  await w.round([bob])
  await w.round([c]) // offer → feed AND the real quote → bob's inbox, before bob records anything
  const sale = (await c.store.operations({ role: 'seller' })).find((o) => o.kind === 'sale')!
  const key = `${c.pub}:${sale.operationId}`
  const offersOf = (st: any) => st.data.offers as Record<string, { commitment?: unknown; mode?: string }>
  // quote-before-offer is a retry, never a dead letter: at most two rounds and
  // the commitment is attached while the op is still collecting
  for (let i = 0; i < 4; i++) {
    await w.round([bob])
    const st = (await bob.engine.status(operationId))!.operation
    if (offersOf(st)[key]?.commitment !== undefined) break
  }
  let st = (await bob.engine.status(operationId))!.operation
  assert.ok(offersOf(st)[key]?.commitment !== undefined, 'the quote commitment attaches while collecting')
  // ride the deal to awaiting_disclosure — a duplicate quote in a later state
  // still attaches (post-selection attachment is what the claim needs)
  w.advance(1500)
  for (let i = 0; i < 6; i++) {
    await w.round()
    st = (await bob.engine.status(operationId))!.operation
    if (st.state === 'awaiting_disclosure' || st.state === 'disclosure_verified') break
  }
  assert.equal(st.state, 'awaiting_disclosure', st.lastError)
  const attached = (await bob.store.events(operationId)).filter((e) => e.cause === 'quote_attached').length
  // a byte-identical duplicate is deduped at ingest — the wake can repeat
  // without the op ever seeing it twice
  const commitment = offersOf(st)[key].commitment as { leaf?: string }
  const mode = offersOf(st)[key].mode
  assert.ok(mode !== undefined, 'the attached quote carried its mode')
  await bob.engine.ingestMessage(c.pub, { type: 'quote', wantId, offerId: sale.operationId, commitment, mode })
  await w.round([bob])
  assert.equal(
    (await bob.store.events(operationId)).filter((e) => e.cause === 'quote_attached').length,
    attached,
    'a byte-identical duplicate quote is deduped — no double processing'
  )
  // a quote delivery the store has NOT seen (fresh digest, same commitment)
  // still attaches post-selection — the early-select gap this fixes
  await bob.engine.ingestMessage(c.pub, {
    type: 'quote',
    wantId,
    offerId: sale.operationId,
    commitment: { ...commitment, redelivery: 1 },
    mode
  })
  await w.round([bob])
  assert.ok(
    (await bob.store.events(operationId)).filter((e) => e.cause === 'quote_attached').length > attached,
    'a quote landing after selection still attaches'
  )
  // settle the deal so the claim path proves the commitment did its job
  for (let i = 0; i < 6; i++) {
    await w.round()
    st = (await bob.engine.status(operationId))!.operation
    if (st.state === 'disclosure_verified') break
  }
  assert.equal(st.state, 'disclosure_verified', st.lastError)
  assert.equal((st.data.result as any).verified, true)
  // terminal: a quote that outlives its deal is ignored — no event, no mutation
  await bob.store.transition(operationId, st.revision, 'refunded', { cause: 'test_forced_terminal' })
  const attachedBefore = (await bob.store.events(operationId)).filter((e) => e.cause === 'quote_attached').length
  await bob.engine.ingestMessage(c.pub, {
    type: 'quote',
    wantId,
    offerId: sale.operationId,
    commitment: { ...commitment, redelivery: 2 },
    mode
  })
  await w.round([bob])
  assert.equal((await bob.store.events(operationId)).filter((e) => e.cause === 'quote_attached').length, attachedBefore, 'a terminal op ignores a late quote')
})

// finding 6a: a shallow proof must not settle — the op parks until depth
void test('settleDepth: a depth-1 proof parks completion_submitted, settles at depth', async () => {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  try {
    const store = new AutomationStore(kx, () => 1_800_000_000_000)
    await store.init()
    let height = 100
    const ex = new Dispatcher()
    const engine = new MarketEngine({
      store,
      memory: { shardManifest: () => Promise.resolve(undefined), records: () => Promise.resolve([]) },
      key: PrivateKey.fromRandom(),
      network: 'testnet',
      call: () => Promise.resolve({}),
      height: () => Promise.resolve(height),
      fetchTx: () => Promise.resolve({ merklePath: { blockHeight: 100 } } as Transaction),
      transport: { publishWant: async () => {}, publishOffer: async () => {}, send: async () => {} },
      ai: new ScriptedExecutor(),
      executor: ex,
      settleDepth: 3
    })
    wireEngine(ex, engine, store)
    const { operation: op } = await store.createOperation({
      role: 'seller',
      kind: 'sale',
      dedupeKey: 'settle-depth-test',
      policyHash: 'p',
      network: 'testnet',
      state: 'completion_submitted',
      data: { completeTxid: 'aa'.repeat(32) }
    })
    // depth 1: two ticks, one park event — same-state transitions dedupe
    await engine.tick()
    await engine.tick()
    let cur = (await store.operation(op.operationId))!
    assert.equal(cur.state, 'completion_submitted', 'a depth-1 proof does not settle at settleDepth 3')
    assert.equal(cur.data.provenHeight, 100)
    const evs = await store.events(op.operationId)
    assert.equal(evs.filter((e) => e.cause === 'disclosure_proof_awaiting_depth').length, 1, 'the park is recorded once, not re-emitted every tick')
    // depth 3 (height H+2): settles
    height = 102
    await engine.tick()
    cur = (await store.operation(op.operationId))!
    assert.equal(cur.state, 'settled', 'the op settles once the proof is deep enough')
  } finally {
    await kx.destroy()
  }
})

void test('want-TTL race: a live want republishes at the collection boundary instead of expiring — only its own exp ends the deal', async (t) => {
  const w = await world(t, { wantExtendMs: 3000 })
  if (w === undefined) return
  t.after(w.close)
  const bob = await w.add('bob')
  const { operationId } = await bob.engine.publishWant({ domains: ['libfoo'], question: 'slow seller race', maxSats: 500 }, bob.policyHash)
  await w.round([bob])
  assert.equal(w.feed.filter((e) => e.kind === 'want').length, 1, 'the first publish lands')
  // collectMs (1000) elapses with zero offers — the want is still live, so the
  // op must not die on no_qualifying_offer: it re-arms and republishes
  w.advance(1100)
  await w.round([bob])
  let st = (await bob.engine.status(operationId))!.operation
  assert.equal(st.state, 'collecting', 'a live want survives an empty collection window')
  await w.round([bob])
  assert.equal(w.feed.filter((e) => e.kind === 'want').length, 2, 'the same signed want is republished, not re-minted')
  const evs = await bob.store.events(operationId)
  assert.ok(
    evs.some((e) => e.cause === 'want_republished'),
    'the republish is journaled'
  )
  // still live inside the extension — a second empty boundary republishes again
  w.advance(1100)
  await w.round([bob])
  st = (await bob.engine.status(operationId))!.operation
  assert.equal(st.state, 'collecting')
  // the want's own exp is the deal's end — only then does it expire
  w.advance(2000)
  await w.round([bob])
  st = (await bob.engine.status(operationId))!.operation
  assert.equal(st.state, 'expired')
  assert.equal((await bob.store.events(operationId)).at(-1)?.cause, 'no_qualifying_offer')
})
