import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import knexFactory, { type Knex } from 'knex'
import { stackWorld, stackChain, StackFunder } from '@nektar/chain'
import { verifyWant, verifyOffer, type SignedWant, type SignedOffer } from '@nektar/bstack'
import { NektarMemory } from '../src/memory.js'
import { callTool } from '../src/server.js'
import { AutomationStore } from '../src/automation/store.js'
import { MarketEngine, type MarketMessage } from '../src/automation/market.js'
import { ScriptedExecutor } from '../src/automation/ai.js'
import { DEFAULT_SCORE_WEIGHTS } from '../src/automation/scoring.js'
import type { AutomationPolicy } from '../src/automation/policy.js'

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

async function world(t: { skip: (m?: string) => void }, opts: { refundBlocks?: number } = {}) {
  const chain = (await stackChain(t))!
  const dir = mkdtempSync(join(tmpdir(), 'nektar-market-'))
  const agents = new Map<string, Agent>()
  const feed: Array<{ seq: number; kind: string; payload: unknown }> = []
  const wire: string[] = []
  let clock = 1_800_000_000_000
  const now = () => clock
  const transportFor = (pub: string) => ({
    publishWant: async (sw: SignedWant) => {
      assert.ok(verifyWant(sw.want, sw.sig))
      wire.push(JSON.stringify(sw))
      feed.push({ seq: feed.length + 1, kind: 'want', payload: sw })
    },
    publishOffer: async (so: SignedOffer) => {
      assert.ok(verifyOffer(so.offer, so.sig))
      wire.push(JSON.stringify(so))
      feed.push({ seq: feed.length + 1, kind: 'offer', payload: so })
    },
    send: async (to: string, msg: MarketMessage) => {
      const a = [...agents.values()].find((x) => x.pub === to)
      if (!a) throw new Error('recipient offline')
      await a.engine.ingestMessage(pub, msg)
    }
  })
  const hooks = { afterPrepare: undefined as undefined | ((actionId: string) => void) }
  const engineFor = (mem: NektarMemory, store: AutomationStore) =>
    new MarketEngine({
      store,
      memory: mem as never,
      key: mem.operatorKey(),
      network: 'mock',
      now,
      call: (tool, args) => callTool(mem, tool, args),
      height: () => mem.chainHeight(),
      fetchTx: async (txid) => (chain as any).known.get(txid),
      transport: transportFor(mem.operatorKey().toPublicKey().toString()),
      ai: new ScriptedExecutor(),
      broadcast: (tx) => mem.broadcastTx(tx),
      settleEscrow: (tx) => {
        for (const inp of tx.inputs) {
          const id = inp.sourceTXID ?? inp.sourceTransaction?.id('hex')
          if (id) mem.markSpentOut(id, inp.sourceOutputIndex ?? 0)
        }
        if (tx.outputs[1]) mem.addOwnedOut(tx, 1)
      },
      afterPrepare: (id) => hooks.afterPrepare?.(id),
      opts: { refundBlocks: opts.refundBlocks ?? 144 }
    })
  const policyFor = (pub: string, extra: Partial<AutomationPolicy['limits']> = {}): AutomationPolicy => ({
    v: 1,
    policyId: 'demo',
    network: 'mock',
    agentPub: pub,
    roles: ['buyer', 'seller'],
    notBefore: 0,
    notAfter: 4_000_000_000_000,
    domains: ['libfoo'],
    limits: {
      perPurchaseSats: 500,
      dailyPurchaseSats: 1000,
      perTxFeeSats: 600,
      dailyFeeSats: 2000,
      runTotalSats: 5000,
      maxLockedSats: 1000,
      maxUnconfirmedSats: 1000,
      maxUnconfirmedCount: 3,
      ...extra
    },
    scoring: { minimumScoreBps: 0, weights: { ...DEFAULT_SCORE_WEIGHTS }, collectMs: 1000 },
    seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: 5, sellableShards: ['guide'] },
    disclosure: { allowPublic: false },
    ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 200, timeoutMs: 5000 },
    sandbox: { mode: 'sandbox-only' }
  })
  const add = async (name: string, limits: Partial<AutomationPolicy['limits']> = {}): Promise<Agent> => {
    const funder = StackFunder.dry()
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

test('S1 mock: three sellers quote 450/300/200, buyer scores, pays the best, verifies the disclosure — no manual tool calls', async (t) => {
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
    [710, 810, 960]
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
  const reveal = (w.chain as any).known.get(sale.data.completeTxid)
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

test('two wants from one buyer are answered and settled independently', async (t) => {
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

test('outside policy → paused for a human; approval is bound to the exact action; denial stops it', async (t) => {
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
  assert.equal(pending!.reason, 'locked_capital_above_policy')
  assert.equal(pending!.maxExtraSats, 710)
  await w.round([bob])
  assert.equal((await bob.engine.status(op1))!.operation.state, 'escalated', 'nothing is spent while the approval is pending')
  assert.equal((await bob.store.budgetStatus(bob.policyHash)).purchaseTodaySats, 0)
  await bob.store.decideApproval(pending!.approvalId, true, 'cli:operator')
  for (let i = 0; i < 3; i++) await w.round()
  assert.equal((await bob.engine.status(op1))!.operation.state, 'disclosure_verified')
  const op2 = await run()
  const [p2] = await bob.store.approvals('pending')
  await bob.store.decideApproval(p2!.approvalId, false, 'cli:operator')
  await w.round([bob])
  assert.equal((await bob.engine.status(op2))!.operation.state, 'denied')
  assert.equal((await bob.store.budgetStatus(bob.policyHash)).purchaseTodaySats, 200, 'only the approved purchase was spent')
})

test('seller goes dark after quoting → buyer refunds itself at the deadline; revoked policy stops new buying', async (t) => {
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
  assert.equal((await bob.store.budgetStatus(bob.policyHash)).lockedSats, 210)
  await w.round([bob])
  assert.equal((await bob.engine.status(operationId))!.operation.state, 'awaiting_disclosure', 'no early refund')
  await w.chain.mine(8)
  await w.round([bob])
  st = (await bob.engine.status(operationId))!.operation
  assert.equal(st.state, 'refunded', st.lastError)
  assert.equal((await bob.store.budgetStatus(bob.policyHash)).lockedSats, 0)
  assert.equal((await c.store.operations({ role: 'seller' })).find((o) => o.kind === 'sale')!.state, 'quoted', 'the dark seller never completed')
  await bob.store.revokePolicy(bob.policyHash)
  await assert.rejects(bob.engine.publishWant({ domains: ['libfoo'], maxSats: 300 }, bob.policyHash), /no active buyer policy/)
})

test('hostile offers are rejected with reasons and never funded', async (t) => {
  const w = await world(t)
  if (w === undefined) return
  t.after(w.close)
  const c = await w.seller('cara', 200)
  const bob = await w.add('bob')
  const { operationId } = await bob.engine.publishWant({ domains: ['libfoo'], question: 'connect is not a function', maxSats: 150 }, bob.policyHash)
  await w.round()
  await w.round()
  w.advance(1500)
  await w.round([bob])
  const st = (await bob.engine.status(operationId))!.operation
  assert.equal(st.state, 'expired', 'a seller priced above the ceiling stays silent → no qualifying offer')
  assert.equal(w.feed.filter((e) => e.kind === 'offer').length, 0)
  assert.equal((await c.store.operations({ role: 'seller' })).filter((o) => o.kind === 'sale').length, 0, 'no preparation spent on an unaffordable want')
  assert.equal((await bob.store.budgetStatus(bob.policyHash)).runTotalSats, 0)
})

test('crash between escrow prepare and submit: restart resubmits the SAME bytes, no second escrow, no operator prompt', async (t) => {
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
  const recorded = await bob.store.walletTx({ actionId: `${operationId}:escrow` })
  assert.equal(recorded!.state, 'prepared', 'bytes persisted, never submitted')
  w.hooks.afterPrepare = undefined
  await w.restart(bob)
  for (let i = 0; i < 3; i++) await w.round()
  st = (await bob.engine.status(operationId))!.operation
  assert.equal(st.state, 'disclosure_verified', st.lastError)
  assert.equal((st.data.utxo as any).txid, pending.utxo.txid, 'the SAME escrow transaction was used after restart — no rebuild, no double fund')
  assert.equal((await bob.store.approvals()).length, 0, 'no human needed for a deterministic crash')
})
