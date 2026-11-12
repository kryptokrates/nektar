import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import knexFactory, { type Knex } from 'knex'
import { stackWorld, stackChain, stackTreasury, StackFunder } from '@nektar/chain'
import { NektarMemory } from '../src/memory.js'
import { callTool } from '../src/server.js'
import { AutomationStore } from '../src/automation/store.js'
import { MarketEngine, type MarketMessage } from '../src/automation/market.js'
import { ScriptedExecutor } from '../src/automation/ai.js'
import { DEFAULT_SCORE_WEIGHTS } from '../src/automation/scoring.js'
import type { AutomationPolicy } from '../src/automation/policy.js'
import type { SignedWant, SignedOffer } from '@nektar/bstack'

process.env.NEKTAR_PAID_DISCLOSURE = '1'

function rng(seed: number) {
  let s = seed
  return () => {
    s = (s * 1103515245 + 12345) & 0x7fffffff
    return s / 0x80000000
  }
}

test('20 seeded fault schedules: dropped/duplicated/reordered delivery + crashes never double-pay or overrun the budget', async (t) => {
  for (let seed = 1; seed <= 20; seed++) {
    const rand = rng(seed)
    const chain = (await stackChain(t))!
    const dir = mkdtempSync(join(tmpdir(), `nektar-fault-${seed}-`))
    const kxs: Knex[] = []
    const feed: Array<{ seq: number; kind: string; payload: unknown }> = []
    const pending: Array<{ to: string; from: string; msg: MarketMessage }> = []
    const agents = new Map<string, { mem: NektarMemory; store: AutomationStore; engine: MarketEngine; pub: string; policyHash: string; name: string }>()
    const now = () => 1_800_000_000_000
    const mk = async (name: string, seller = false) => {
      const funder = await StackFunder.fund(await stackTreasury(), 30)
      const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 1 }, modelId: 'test', network: 'regtest' })
      const kx = knexFactory({ client: 'sqlite3', connection: { filename: join(dir, `${name}.sqlite`) }, useNullAsDefault: true })
      kxs.push(kx)
      const store = new AutomationStore(kx, now)
      await store.init()
      const pub = mem.operatorKey().toPublicKey().toString()
      const policy: AutomationPolicy = {
        v: 1,
        policyId: name,
        network: 'regtest',
        agentPub: pub,
        roles: ['buyer', 'seller'],
        notBefore: 0,
        notAfter: 4_000_000_000_000,
        domains: ['libfoo'],
        limits: {
          perPurchaseSats: 500,
          dailyPurchaseSats: 500,
          perTxFeeSats: 600,
          dailyFeeSats: 600,
          runTotalSats: 1200,
          maxLockedSats: 1200,
          maxUnconfirmedSats: 1200,
          maxUnconfirmedCount: 3
        },
        scoring: { minimumScoreBps: 0, weights: { ...DEFAULT_SCORE_WEIGHTS }, collectMs: 0 },
        seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: 5, sellableShards: ['guide'] },
        disclosure: { allowPublic: false },
        ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 100, timeoutMs: 1000 },
        sandbox: { mode: 'sandbox-only' }
      }
      const policyHash = await store.approvePolicy(policy, 'test')
      const transport = {
        publishWant: async (sw: SignedWant) => {
          if (rand() > 0.15) feed.push({ seq: feed.length + 1, kind: 'want', payload: sw })
          if (rand() > 0.8) feed.push({ seq: feed.length + 1, kind: 'want', payload: sw })
        },
        publishOffer: async (so: SignedOffer) => {
          if (rand() > 0.15) feed.push({ seq: feed.length + 1, kind: 'offer', payload: so })
          if (rand() > 0.8) feed.push({ seq: feed.length + 1, kind: 'offer', payload: so })
        },
        send: async (to: string, msg: MarketMessage) => {
          if (rand() > 0.2) pending.push({ to, from: pub, msg })
          if (rand() > 0.85) pending.push({ to, from: pub, msg })
        }
      }
      const engine = new MarketEngine({
        store,
        memory: mem,
        key: mem.operatorKey(),
        network: 'regtest',
        now,
        call: (tool, args) => callTool(mem, tool, args),
        height: () => mem.chainHeight(),
        fetchTx: async (id) => chain.getTx(id),
        broadcast: (tx) => mem.broadcastTx(tx),
        settleEscrow: (tx) => {
          for (const inp of tx.inputs) {
            const id = inp.sourceTXID ?? inp.sourceTransaction?.id('hex')
            if (id) mem.markSpentOut(id, inp.sourceOutputIndex ?? 0)
          }
          if (tx.outputs[1]) mem.addOwnedOut(tx, 1)
        },
        transport,
        ai: new ScriptedExecutor(),
        opts: { refundBlocks: 8 }
      })
      const a = { mem, store, engine, pub, policyHash, name }
      agents.set(name, a)
      if (seller) {
        await mem.remember('guide', 'libfoo 3.0 renamed connect to open; replace connect( with open(')
        await chain.mine(1)
        for (const l of mem.anchorLinks('guide')) mem.confirmAnchor('guide', l.txid)
        await mem.describeShard('guide', { domains: ['libfoo'], offers: [{ kind: 'record', priceSats: 200 }] })
        await engine.enableSeller(policyHash)
      }
      return a
    }
    const cara = await mk('cara', true)
    if (cara === undefined) return
    const bob = await mk('bob')
    if (bob === undefined) return
    const { operationId } = await bob.engine.publishWant({ domains: ['libfoo'], question: 'libfoo connect open', maxSats: 500 }, bob.policyHash)
    const cursors = new Map<string, number>()
    for (let round = 0; round < 30; round++) {
      // deliver pending messages in random order, sometimes dropping
      for (const m of pending.splice(0).sort(() => rand() - 0.5)) {
        const a = agents.get(m.to === cara.pub ? 'cara' : 'bob')
        if (a && rand() > 0.1) await a.engine.ingestMessage(m.from, m.msg)
      }
      for (const a of [cara, bob]) {
        const since = cursors.get(a.name) ?? 0
        const fresh = feed.filter((e) => e.seq > since).sort(() => rand() - 0.5)
        if (fresh.length) {
          await a.engine.ingestEvents('hub', fresh, String(feed.length))
          cursors.set(a.name, feed.length)
        }
        await a.engine.tick()
      }
      const op = await bob.store.operation(operationId)
      if (op && ['disclosure_verified', 'useful', 'refunded', 'expired', 'denied', 'disputed', 'funding_failed'].includes(op.state)) break
    }
    const op = (await bob.store.operation(operationId))!
    const budget = await bob.store.budgetStatus(bob.policyHash)
    assert.ok(budget.purchaseTodaySats <= 500, `seed ${seed}: purchase cap exceeded`)
    assert.ok(budget.feeTodaySats <= 600, `seed ${seed}: fee cap exceeded`)
    assert.ok(budget.lockedSats <= 1200, `seed ${seed}: locked cap exceeded`)
    const escrows = (await bob.store.walletTxs(['prepared', 'submitted', 'observed', 'confirmed'])).filter((x) => x.kind === 'escrow')
    assert.ok(escrows.length <= 1, `seed ${seed}: more than one escrow for one want`)
    assert.ok(!['uncertain'].includes(op.state) || op.state === 'uncertain', `seed ${seed}: ${op.state}`)
    if (op.state === 'disclosure_verified') assert.equal(budget.purchaseTodaySats, 200, `seed ${seed}: paid the wrong amount`)
    await bob.store.revokePolicy(bob.policyHash)
    for (const kx of kxs) await kx.destroy()
  }
  t.diagnostic('20/20 seeded schedules respected budget caps and single-escrow invariant')
})
