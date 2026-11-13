import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { stackChain, stackTreasury, StackFunder, StackChain } from '@nektar/chain'
import { minihost, faulthost } from '@nektar/bstack/testing'
import { NektarMemory } from '../src/memory.js'
import { callTool } from '../src/server.js'
import { openAutomation, type Automation } from '../src/automation/runtime.js'
import { ScriptedExecutor } from '../src/automation/ai.js'
import { DEFAULT_SCORE_WEIGHTS } from '../src/automation/scoring.js'
import type { AutomationPolicy } from '../src/automation/policy.js'

// DDIA ch.5/8 resilience plane: deals must survive dead hosts, slow hosts,
// withholding hosts and mid-deal process kills — and T1 puts numbers on the
// healthy-deal latency distribution so collectMs stops being a guess.
// Carriers ride REAL minihost/faulthost HTTP (no in-process bus) or the
// faults would never touch the wire.

process.env.NEKTAR_PAID_DISCLOSURE = '1'
const FINDING = 'breaking change: libfoo 3.0 renamed connect() to open(); replace connect( with open( to fix the build'
const NET = 'regtest'
const OFFICE = 'nektar_abcdefghij'
let clock = Date.now()

const COLLECT_MS = 2500
const policyFor = (pub: string): AutomationPolicy => ({
  v: 1,
  policyId: 'demo',
  network: NET,
  agentPub: pub,
  roles: ['buyer', 'seller'],
  notBefore: 0,
  notAfter: 4_000_000_000_000,
  domains: ['libfoo'],
  limits: {
    perPurchaseSats: 500,
    dailyPurchaseSats: 50_000, // bench runs back-to-back deals — cap is for operators, not the harness
    perTxFeeSats: 600,
    dailyFeeSats: 50_000,
    runTotalSats: 500_000,
    maxLockedSats: 5000,
    maxUnconfirmedSats: 5000,
    maxUnconfirmedCount: 16
  },
  scoring: { minimumScoreBps: 0, weights: { ...DEFAULT_SCORE_WEIGHTS }, collectMs: COLLECT_MS },
  seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: 50, sellableShards: ['guide'] },
  disclosure: { allowPublic: false },
  ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 200, timeoutMs: 5000 },
  sandbox: { mode: 'sandbox-only' }
})

interface A {
  mem: NektarMemory
  rt: Automation
  pub: string
  policyHash: string
  name: string
}
const mkagent = async (name: string, hosts: string[], chain: StackChain, logs: string[], dir: string, opts?: Partial<import('../src/automation/market.js').MarketOptions>): Promise<A> => {
  const funder = await StackFunder.fund(await stackTreasury(), 60)
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 1 }, modelId: 'test-model-1' })
  const pub = mem.operatorKey().toPublicKey().toString()
  const rt = await openAutomation(mem, {
    network: NET,
    dataDir: join(dir, name),
    marketHosts: hosts,
    intervalMs: 0,
    call: (t2, a) => callTool(mem, t2, a),
    log: (l) => logs.push(`${name}:${l}`),
    ai: new ScriptedExecutor(),
    now: () => clock,
    publishFinger: true,
    opts: { offerTtlBlocks: 10000, refundBlocks: 10000, ...opts }
  })
  mem.automation = rt
  return { mem, rt, pub, policyHash: await rt.store.approvePolicy(policyFor(pub), 'test:operator'), name }
}
// respawn = restart on the SAME memory + dataDir (sqlite survives: kill -9
// keeps files). The op records rebuild from automation.sqlite; identity and
// keys come from the unchanged NektarMemory.
const respawn = async (a: A, hosts: string[], chain: StackChain, logs: string[], dir: string): Promise<A> => {
  const rt = await openAutomation(a.mem, {
    network: NET,
    dataDir: join(dir, a.name),
    marketHosts: hosts,
    intervalMs: 0,
    call: (t2, args) => callTool(a.mem, t2, args),
    log: (l) => logs.push(`${a.name}:${l}`),
    ai: new ScriptedExecutor(),
    now: () => clock,
    publishFinger: true,
    opts: { offerTtlBlocks: 10000, refundBlocks: 10000 }
  })
  a.mem.automation = rt
  return { mem: a.mem, rt, pub: a.pub, policyHash: await rt.store.approvePolicy(policyFor(a.pub), 'test:operator'), name: a.name }
}
const sellerize = async (s: A, price: number) => {
  await s.mem.remember('guide', FINDING)
  await s.mem.describeShard('guide', { domains: ['libfoo'], offers: [{ kind: 'record', priceSats: price }] })
  for (const l of s.mem.anchorLinks('guide')) s.mem.confirmAnchor('guide', l.txid)
  await s.rt.engine.enableSeller(s.policyHash)
}
const drive = async (rts: Automation[], chain: StackChain, n = 1) => {
  for (let i = 0; i < n; i++) {
    clock += 500
    await Promise.all(rts.map((rt) => rt.cycle()))
    if ((await chain.pendingBroadcasts()) > 0) await chain.mine(1)
  }
}
const until = async (rts: Automation[], chain: StackChain, ok: () => Promise<boolean>, n = 100): Promise<number> => {
  for (let i = 0; i < n; i++) {
    await drive(rts, chain, 1)
    if (await ok()) return i + 1
  }
  return -1
}
const trackerFor = (chain: StackChain) => ({
  isValidRootForHeight: (r: string, h: number) => chain.isValidRootForHeight(r, h),
  currentHeight: () => chain.currentHeight()
})
const buyerOps = async (b: A) => (await b.rt.store.operations({ role: 'buyer' })).filter((o) => o.kind === 'purchase')
const saleOps = async (s: A) => (await s.rt.store.operations({ role: 'seller' })).filter((o) => o.kind === 'sale')
const opFor = async (b: A, wantId: string) =>
  (await buyerOps(b)).find((o) => (o.data.want as { want?: { wantId?: string } } | undefined)?.want?.wantId === wantId)
const COMPLETE = new Set(['completion_submitted', 'settled', 'useful'])
const BUYER_DONE = new Set(['disclosure_verified', 'useful'])

const deal = async (b: A, s: A, chain: StackChain, logs: string[], extraRts: Automation[] = []) => {
  const w = await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
  const all = [b.rt, s.rt, ...extraRts]
  const t0 = Date.now()
  const rounds = await until(all, chain, async () => {
    const bo = await opFor(b, w.wantId)
    const so = (await saleOps(s)).find((o) => (o.data as { wantId?: string }).wantId === w.wantId || (o.data.want as { want?: { wantId?: string } } | undefined)?.want?.wantId === w.wantId)
    return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
  })
  assert.ok(rounds > 0, 'deal never completed | ' + logs.slice(-20).join(' | '))
  return { wantId: w.wantId, rounds, ms: Date.now() - t0 }
}

test('T1: deal latency distribution — 8 sequential deals, p50/p95 on rounds and wall ms', { timeout: 300_000 }, async (t) => {
  const chain = (await stackChain(t))!
  const dir = mkdtempSync(join(tmpdir(), 'nektar-res-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  let b!: A, s!: A
  try {
    b = await mkagent('buyer', [h1.url, h2.url], chain, logs, dir)
    s = await mkagent('seller', [h1.url, h2.url], chain, logs, dir)
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    const runs: Array<{ rounds: number; ms: number }> = []
    for (let i = 0; i < 8; i++) runs.push(await deal(b, s, chain, logs))
    const pct = (xs: number[], p: number) => xs.slice().sort((a, z) => a - z)[Math.min(xs.length - 1, Math.floor((p / 100) * xs.length))]
    const rounds = runs.map((r) => r.rounds)
    const ms = runs.map((r) => r.ms)
    console.log(
      `    deal latency over ${runs.length} deals — rounds p50=${pct(rounds, 50)} p95=${pct(rounds, 95)} max=${Math.max(...rounds)}` +
        ` | wall ms p50=${pct(ms, 50)} p95=${pct(ms, 95)} max=${Math.max(...ms)}`
    )
    // guardrail, not a regression gate: a healthy deal today lands ~25-45 rounds;
    // 200 is far above that but still catches a hang-shaped regression
    assert.ok(Math.max(...rounds) < 200, `slowest deal took ${Math.max(...rounds)} rounds — latency regressed hard`)
  } finally {
    for (const r of [b?.rt, s?.rt]) {
      try {
        await r?.stop()
      } catch {
        /* */
      }
    }
    await h1.close()
    await h2.close()
  }
})

test('R1: host dies mid-deal — fan-out still completes through the survivor', { timeout: 300_000 }, async (t) => {
  const chain = (await stackChain(t))!
  const dir = mkdtempSync(join(tmpdir(), 'nektar-res-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  let b!: A, s!: A
  try {
    b = await mkagent('buyer', [h1.url, h2.url], chain, logs, dir)
    s = await mkagent('seller', [h1.url, h2.url], chain, logs, dir)
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    const w = await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    assert.ok(
      (await until([b.rt, s.rt], chain, async () => {
        const so = (await saleOps(s)).find((o) => (o.data as { wantId?: string }).wantId === w.wantId)
        return so?.state === 'quoted'
      })) > 0,
      'no quote | ' + logs.slice(-15).join(' | ')
    )
    await h1.close() // the want+offer carrier host dies before the accept is sent
    const rounds = await until([b.rt, s.rt], chain, async () => {
      const bo = await opFor(b, w.wantId)
      const so = (await saleOps(s)).find((o) => (o.data as { wantId?: string }).wantId === w.wantId)
      return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
    })
    assert.ok(rounds > 0, 'deal died with h1 | ' + logs.slice(-20).join(' | '))
  } finally {
    for (const r of [b?.rt, s?.rt]) {
      try {
        await r?.stop()
      } catch {
        /* */
      }
    }
    await h2.close()
  }
})

test('R2: one host delayed 1.5s — quorum-1 rides the fast host, deal does not stall', { timeout: 300_000 }, async (t) => {
  const chain = (await stackChain(t))!
  const dir = mkdtempSync(join(tmpdir(), 'nektar-res-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const f1 = await faulthost(h1.url)
  f1.fault.delayMs = 1500
  let b!: A, s!: A
  try {
    b = await mkagent('buyer', [f1.url, h2.url], chain, logs, dir)
    s = await mkagent('seller', [f1.url, h2.url], chain, logs, dir)
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    const r = await deal(b, s, chain, logs)
    assert.ok(r.rounds < 200, `deal through delayed host took ${r.rounds} rounds`)
    assert.ok(f1.forwarded() > 0, 'slow host never saw traffic — writes should still fan out to it')
    // the slow host's records do land (delayed, not dropped) — completeness check
    assert.ok(h1.txids().length > 0, 'slow host stored nothing')
  } finally {
    for (const r of [b?.rt, s?.rt]) {
      try {
        await r?.stop()
      } catch {
        /* */
      }
    }
    await f1.close()
    await h1.close()
    await h2.close()
  }
})

test('R5: kill matrix — buyer respawn mid-deal from collecting / awaiting_disclosure reaches terminal state', { timeout: 300_000 }, async (t) => {
  const chain = (await stackChain(t))!
  for (const checkpoint of ['collecting', 'awaiting_disclosure'] as const) {
    const dir = mkdtempSync(join(tmpdir(), 'nektar-res-'))
    const logs: string[] = []
    const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
    const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
    let b!: A, s!: A
    try {
      b = await mkagent('buyer', [h1.url, h2.url], chain, logs, dir)
      s = await mkagent('seller', [h1.url, h2.url], chain, logs, dir)
      await sellerize(s, 200)
      await drive([b.rt, s.rt], chain, 12)
      const w = await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
      const hit = await until([b.rt, s.rt], chain, async () => (await opFor(b, w.wantId))?.state === checkpoint)
      assert.ok(hit > 0, `[${checkpoint}] never reached | ` + logs.slice(-15).join(' | '))
      await b.rt.stop() // kill -9 keeps the sqlite files; never pump this rt again
      const b2 = await respawn(b, [h1.url, h2.url], chain, logs, dir)
      const rounds = await until([b2.rt, s.rt], chain, async () => {
        const bo = await opFor(b2, w.wantId)
        const so = (await saleOps(s)).find((o) => (o.data as { wantId?: string }).wantId === w.wantId)
        return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
      })
      assert.ok(rounds > 0, `[${checkpoint}] deal never recovered | ` + logs.slice(-20).join(' | '))
      const bo = (await opFor(b2, w.wantId))!
      assert.equal(bo.operationId, (await opFor(b, w.wantId))!.operationId, `[${checkpoint}] respawn spawned a duplicate op`)
      await b2.rt.stop()
      await s.rt.stop()
    } finally {
      for (const r of [b?.rt, s?.rt]) {
        try {
          await r?.stop()
        } catch {
          /* */
        }
      }
      await h1.close()
      await h2.close()
    }
  }
})

test('R5b: seller killed after quoting respawns and settles; withholding host cannot starve the deal', { timeout: 300_000 }, async (t) => {
  const chain = (await stackChain(t))!
  const dir = mkdtempSync(join(tmpdir(), 'nektar-res-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const f1 = await faulthost(h1.url)
  f1.fault.dropLookup = true // h1 stores but never answers — the worst honest-but-useless host
  let b!: A, s!: A
  try {
    b = await mkagent('buyer', [f1.url, h2.url], chain, logs, dir)
    s = await mkagent('seller', [f1.url, h2.url], chain, logs, dir)
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    const w = await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    const hit = await until([b.rt, s.rt], chain, async () => {
      const so = (await saleOps(s)).find((o) => (o.data as { wantId?: string }).wantId === w.wantId)
      return so?.state === 'quoted'
    })
    assert.ok(hit > 0, 'no quote through withholding host | ' + logs.slice(-15).join(' | '))
    await s.rt.stop()
    const s2 = await respawn(s, [f1.url, h2.url], chain, logs, dir)
    await s2.rt.engine.enableSeller(s2.policyHash)
    const rounds = await until([b.rt, s2.rt], chain, async () => {
      const bo = await opFor(b, w.wantId)
      const so = (await saleOps(s2)).find((o) => (o.data as { wantId?: string }).wantId === w.wantId)
      return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
    })
    assert.ok(rounds > 0, 'deal never completed after seller kill | ' + logs.slice(-20).join(' | '))
    await b.rt.stop()
    await s2.rt.stop()
  } finally {
    for (const r of [b?.rt, s?.rt]) {
      try {
        await r?.stop()
      } catch {
        /* */
      }
    }
    await f1.close()
    await h1.close()
    await h2.close()
  }
})
