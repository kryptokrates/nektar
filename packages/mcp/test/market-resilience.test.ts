import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import http from 'node:http'
import type { AddressInfo } from 'node:net'
import { stackChain, stackTreasury, StackFunder, StackChain } from '@nektar/chain'
import { minihost, faulthost } from '@nektar/bstack/testing'
import { NektarMemory } from '../src/memory.js'
import { callTool } from '../src/server.js'
import { openAutomation, type Automation } from '@nektar/automation'
import { ScriptedExecutor } from '@nektar/automation'
import { DEFAULT_SCORE_WEIGHTS } from '@nektar/automation'
import type { AutomationPolicy } from '@nektar/automation'

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
const mkagent = async (
  name: string,
  hosts: string[],
  chain: StackChain,
  logs: string[],
  dir: string,
  opts?: Partial<import('@nektar/automation').MarketOptions>,
  watch?: string
): Promise<A> => {
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
    ...(watch !== undefined ? { watchHosts: [watch], watchPublic: watch } : {}),
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
const saleFor = async (s: A, wantId: string) =>
  (await saleOps(s)).find((o) => (o.data.want as { want?: { wantId?: string } } | undefined)?.want?.wantId === wantId)
const COMPLETE = new Set(['completion_submitted', 'settled', 'useful'])
const BUYER_DONE = new Set(['disclosure_verified', 'useful'])

const deal = async (b: A, s: A, chain: StackChain, logs: string[], extraRts: Automation[] = []) => {
  const w = await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
  const all = [b.rt, s.rt, ...extraRts]
  const t0 = Date.now()
  const rounds = await until(all, chain, async () => {
    const bo = await opFor(b, w.wantId)
    const so = await saleFor(s, w.wantId)
    return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
  })
  assert.ok(rounds > 0, 'deal never completed | ' + logs.slice(-20).join(' | '))
  return { wantId: w.wantId, rounds, ms: Date.now() - t0 }
}

test('T1: deal latency distribution — 5 sequential deals, p50/p95 on rounds and wall ms', { timeout: 300_000 }, async (t) => {
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
    for (let i = 0; i < 5; i++) runs.push(await deal(b, s, chain, logs))
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
    assert.ok((await until([b.rt, s.rt], chain, async () => (await saleFor(s, w.wantId))?.state === 'quoted')) > 0, 'no quote | ' + logs.slice(-15).join(' | '))
    await h1.close() // the want+offer carrier host dies before the accept is sent
    const rounds = await until([b.rt, s.rt], chain, async () => {
      const bo = await opFor(b, w.wantId)
      const so = await saleFor(s, w.wantId)
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
      const opIdBefore = (await opFor(b, w.wantId))!.operationId
      await b.rt.stop() // kill -9 keeps the sqlite files; never pump this rt again
      // note: the stopped rt's store is dead with it — compare ids captured above
      const b2 = await respawn(b, [h1.url, h2.url], chain, logs, dir)
      const rounds = await until([b2.rt, s.rt], chain, async () => {
        const bo = await opFor(b2, w.wantId)
        const so = await saleFor(s, w.wantId)
        return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
      })
      if (rounds <= 0) {
        const bo = await opFor(b2, w.wantId)
        const sos = await saleOps(s)
        console.log(
          `[${checkpoint}] STUCK buyer:`,
          JSON.stringify({ state: bo?.state, lastError: bo?.lastError, dueAt: bo?.dueAt, rev: bo?.revision }),
          'seller:',
          JSON.stringify(sos.map((o) => ({ state: o.state, lastError: o.lastError })))
        )
      }
      assert.ok(rounds > 0, `[${checkpoint}] deal never recovered | ` + logs.slice(-20).join(' | '))
      const bo = (await opFor(b2, w.wantId))!
      assert.equal(bo.operationId, opIdBefore, `[${checkpoint}] respawn spawned a duplicate op`)
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
    const hit = await until([b.rt, s.rt], chain, async () => (await saleFor(s, w.wantId))?.state === 'quoted')
    assert.ok(hit > 0, 'no quote through withholding host | ' + logs.slice(-15).join(' | '))
    await s.rt.stop()
    const s2 = await respawn(s, [f1.url, h2.url], chain, logs, dir)
    await s2.rt.engine.enableSeller(s2.policyHash)
    const rounds = await until([b.rt, s2.rt], chain, async () => {
      const bo = await opFor(b, w.wantId)
      const so = await saleFor(s2, w.wantId)
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

// ── stub watch-shim for T3: GET /watch long-polls park by service name and a
// POST /notify wakes every waiter of that service — the same contract the real
// shim gives hosts that cannot long-poll natively.
const stubShim = async () => {
  const waiters = new Map<string, Array<(v: object) => void>>()
  const seen: string[] = []
  let notifies = 0
  const srv = http.createServer(async (req, res) => {
    const u = new URL(req.url ?? '/', 'http://x')
    seen.push(`${req.method} ${u.pathname}`)
    if (req.method === 'GET' && u.pathname === '/watch') {
      const service = u.searchParams.get('service') ?? ''
      const waitMs = Number(u.searchParams.get('waitMs') ?? 25_000)
      const v = await new Promise<object>((r) => {
        const t = setTimeout(() => r({ changed: false }), Math.min(waitMs, 10_000))
        const list = waiters.get(service) ?? []
        waiters.set(service, [...list, (x) => { clearTimeout(t); r(x) }])
      })
      res.writeHead(200, { 'content-type': 'application/json' })
      res.end(JSON.stringify(v))
      return
    }
    if (req.method === 'POST' && u.pathname === '/notify') {
      const chunks: Buffer[] = []
      req.on('data', (c) => chunks.push(c))
      await new Promise((r) => req.on('end', r))
      notifies++
      const { service } = JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}')
      for (const w of waiters.get(service) ?? []) w({ changed: true })
      waiters.delete(service)
      res.writeHead(204)
      res.end()
      return
    }
    res.writeHead(404)
    res.end()
  })
  await new Promise<void>((r) => srv.listen(0, '127.0.0.1', r))
  const { port } = srv.address() as AddressInfo
  return { url: `http://127.0.0.1:${port}`, notifies: () => notifies, seen: () => seen, close: () => new Promise<void>((r) => srv.close(() => r())) }
}

const STOP_ALL = async (rts: Array<Automation | undefined>) => {
  for (const r of rts) { try { await r?.stop() } catch { /* */ } }
}

// T2 — throughput: K buyers race one seller's shard; every deal must land
test('T2: throughput — 3 concurrent buyers on one seller, all deals land', { timeout: 300_000 }, async (t) => {
  const chain = (await stackChain(t))!
  const dir = mkdtempSync(join(tmpdir(), 'nektar-res-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const buyers: A[] = []
  let s!: A
  try {
    s = await mkagent('seller', [h1.url, h2.url], chain, logs, dir)
    await sellerize(s, 200)
    for (const n of ['b1', 'b2', 'b3']) buyers.push(await mkagent(n, [h1.url, h2.url], chain, logs, dir))
    await drive([s.rt, ...buyers.map((b) => b.rt)], chain, 10)
    const t0 = Date.now()
    const wants = await Promise.all(
      buyers.map((b) => b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash))
    )
    const all = [s.rt, ...buyers.map((b) => b.rt)]
    const done = async () => {
      for (const [i, b] of buyers.entries()) {
        const bo = await opFor(b, wants[i].wantId)
        if (bo === undefined || !BUYER_DONE.has(bo.state)) return false
      }
      return (await saleOps(s)).filter((o) => COMPLETE.has(o.state)).length >= wants.length
    }
    const rounds = await until(all, chain, done, 140)
    const ms = Date.now() - t0
    assert.ok(rounds > 0, 'not all deals landed | ' + logs.slice(-25).join(' | '))
    const settled = (await saleOps(s)).filter((o) => COMPLETE.has(o.state))
    assert.equal(settled.length, wants.length, 'a sale op per want')
    console.log(`    throughput: ${wants.length} deals in ${rounds} rounds / ${ms}ms wall`)
  } finally {
    await STOP_ALL([s?.rt, ...buyers.map((b) => b.rt)])
    await h1.close(); await h2.close()
  }
})

// T3 — watch vs poll A/B: the shim notify must fire and the deal must complete;
// rounds are reported so the push layer's contribution is measurable.
test('T3: watch-shim A/B — notify fires, deal completes at-or-below poll rounds', { timeout: 300_000 }, async (t) => {
  const chain = (await stackChain(t))!
  const run = async (watch: boolean) => {
    const dir = mkdtempSync(join(tmpdir(), 'nektar-res-'))
    const logs: string[] = []
    // finger:true + logStream: real ls_finger resolution — without it records
    // never resolve, the counterparty's `watch` field is invisible, and the
    // /notify hint can never fire (deals still work via the marketHosts fallback)
    const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE, logStream: OFFICE, finger: true })
    const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE, logStream: OFFICE, finger: true })
    const shim = watch ? await stubShim() : undefined
    let b!: A, s!: A
    try {
      b = await mkagent('buyer', [h1.url, h2.url], chain, logs, dir, undefined, shim?.url)
      s = await mkagent('seller', [h1.url, h2.url], chain, logs, dir, undefined, shim?.url)
      await sellerize(s, 200)
      await drive([b.rt, s.rt], chain, 12)
      const w = await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'q?', maxSats: 450 }, b.policyHash)
      const t0 = Date.now()
      const rounds = await until([b.rt, s.rt], chain, async () => {
        const bo = await opFor(b, w.wantId)
        const so = await saleFor(s, w.wantId)
        return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
      })
      assert.ok(rounds > 0, 'deal never completed | ' + logs.slice(-20).join(' | '))
      if (shim !== undefined) {
        const { resolveFinger } = await import('@nektar/bstack')
        const r = await resolveFinger({ hosts: [h1.url, h2.url], pub: b.pub, tracker: chain as never, allowHttp: true, timeoutMs: 5_000 })
        console.log(`    buyer finger after deal: ${r === undefined ? 'UNRESOLVED' : `seq=${r.seq} watch=${r.body.watch ?? 'absent'}`}`)
      }
      return { rounds, ms: Date.now() - t0, notifies: shim?.notifies() ?? 0, seen: shim?.seen() ?? [] }
    } finally {
      await STOP_ALL([b?.rt, s?.rt])
      await shim?.close(); await h1.close(); await h2.close()
    }
  }
  const poll = await run(false)
  const watched = await run(true)
  console.log(`    A/B: poll ${poll.rounds}r/${poll.ms}ms vs watch ${watched.rounds}r/${watched.ms}ms (notifies=${watched.notifies}, shim saw: ${watched.seen.join(', ') || 'nothing'})`)
  assert.ok(watched.notifies > 0, 'shim never notified — the publish→wake path did not fire')
  assert.ok(watched.rounds <= poll.rounds + 2, `watch path was slower (${watched.rounds}r) than polling (${poll.rounds}r)`)
})

// T4 — payload scaling: deal latency vs disclosure size. The 16KiB envelope
// cap (MAX_PAYLOAD) means >~14KiB disclosures can never ride the `delivered`
// envelope — the honest contract: small deals complete, oversized ones leave
// the buyer in a visible terminal/escalated state (refunded | uncertain |
// verified) once the refund covenant gate lapses — NEVER a silent hang.
test('T4: payload scaling — 1KB completes, 32KB/128KB land honest states', { timeout: 300_000 }, async (t) => {
  const chain = (await stackChain(t))!
  for (const kb of [1, 32, 128]) {
    const dir = mkdtempSync(join(tmpdir(), 'nektar-res-'))
    const logs: string[] = []
    const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
    const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
    let b!: A, s!: A
    try {
      b = await mkagent('buyer', [h1.url, h2.url], chain, logs, dir, { refundBlocks: 40 })
      s = await mkagent('seller', [h1.url, h2.url], chain, logs, dir, { refundBlocks: 40 })
      const content = `libfoo 3.0 ${'x'.repeat(kb * 1024)}`
      await s.mem.remember('guide', content)
      await s.mem.describeShard('guide', { domains: ['libfoo'], offers: [{ kind: 'record', priceSats: 200 }] })
      for (const l of s.mem.anchorLinks('guide')) s.mem.confirmAnchor('guide', l.txid)
      await s.rt.engine.enableSeller(s.policyHash)
      await drive([b.rt, s.rt], chain, 10)
      const w = await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'q?', maxSats: 450 }, b.policyHash)
      const t0 = Date.now()
      if (kb === 1) {
        // sanity: under the envelope cap the deal completes end-to-end
        const rounds = await until([b.rt, s.rt], chain, async () => {
          const bo = await opFor(b, w.wantId)
          const so = await saleFor(s, w.wantId)
          return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
        }, 80)
        const bo = await opFor(b, w.wantId)
        console.log(`    ${kb}KB: ${rounds} rounds / ${Date.now() - t0}ms — buyer=${bo?.state}`)
        assert.ok(rounds > 0 && bo !== undefined && BUYER_DONE.has(bo.state), `1KB deal failed: buyer=${bo?.state} | ` + logs.slice(-12).join(' | '))
      } else {
        // over the cap: the disclosure can never ride the envelope. The buyer
        // must not sit at awaiting_disclosure — the escrow covenant gives it
        // a way out at dueHeight (refund) or the reconcile path escalates it.
        assert.ok((await until([b.rt, s.rt], chain, async () => (await opFor(b, w.wantId))?.state === 'awaiting_disclosure', 60)) > 0,
          `${kb}KB escrow never funded | ` + logs.slice(-15).join(' | '))
        await drive([b.rt, s.rt], chain, 15) // delivery retries burn + outbox dead-letters
        await chain.mine(40)
        const honest = new Set(['refunded', 'uncertain', 'disclosure_verified', 'useful', 'refunding'])
        const rounds = await until([b.rt, s.rt], chain, async () => {
          const bo = await opFor(b, w.wantId)
          return bo !== undefined && honest.has(bo.state)
        }, 30)
        const bo = await opFor(b, w.wantId)
        const so = await saleFor(s, w.wantId)
        console.log(`    ${kb}KB: ${rounds} rounds / ${Date.now() - t0}ms — buyer=${bo?.state} cause=${JSON.stringify((bo?.data as { cause?: string })?.cause)} seller=${so?.state}`)
        assert.ok(rounds > 0, `${kb}KB buyer wedged at ${bo?.state} past the refund gate — an undeliverable payload must end visibly | ` + logs.slice(-12).join(' | '))
      }
    } finally {
      await STOP_ALL([b?.rt, s?.rt])
      await h1.close(); await h2.close()
    }
  }
})

// R3 — host divergence: f1 withholds the offer record while accepting the
// submit; quorum-1 reads h2, the withheld bookkeeping must surface it.
test('R3: withholding host loses the offer — deal completes + withheld is logged', { timeout: 300_000 }, async (t) => {
  const chain = (await stackChain(t))!
  const dir = mkdtempSync(join(tmpdir(), 'nektar-res-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const f1 = await faulthost(h1.url)
  let b!: A, s!: A
  try {
    b = await mkagent('buyer', [f1.url, h2.url], chain, logs, dir)
    s = await mkagent('seller', [f1.url, h2.url], chain, logs, dir)
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    const w = await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'q?', maxSats: 450 }, b.policyHash)
    const before = new Set(h1.txids())
    assert.ok((await until([b.rt, s.rt], chain, async () => (await saleFor(s, w.wantId))?.state === 'quoted')) > 0,
      'no quote | ' + logs.slice(-15).join(' | '))
    // everything the seller just published to h1 becomes invisible to lookups
    if (f1.fault.withholdTxids === undefined) f1.fault.withholdTxids = new Set()
    for (const id of h1.txids()) if (!before.has(id)) f1.fault.withholdTxids.add(id)
    const rounds = await until([b.rt, s.rt], chain, async () => {
      const bo = await opFor(b, w.wantId)
      const so = await saleFor(s, w.wantId)
      return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
    })
    assert.ok(rounds > 0, 'withheld offer starved the deal | ' + logs.slice(-20).join(' | '))
    assert.ok(logs.some((l) => l.includes('withheld')), 'withholding was never logged — bookkeeping is blind')
  } finally {
    await STOP_ALL([b?.rt, s?.rt])
    await f1.close(); await h1.close(); await h2.close()
  }
})

// R4 — partition & heal: both hosts down mid-deal, ops pause not die, heal
// completes it; exactly one funding tx ever broadcast.
test('R4: partition heals — ops resume after deaf window, no double-broadcast', { timeout: 300_000 }, async (t) => {
  const chain = (await stackChain(t))!
  const dir = mkdtempSync(join(tmpdir(), 'nektar-res-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const f1 = await faulthost(h1.url)
  const f2 = await faulthost(h2.url)
  let b!: A, s!: A
  try {
    b = await mkagent('buyer', [f1.url, f2.url], chain, logs, dir)
    s = await mkagent('seller', [f1.url, f2.url], chain, logs, dir)
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    const w = await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'q?', maxSats: 450 }, b.policyHash)
    assert.ok((await until([b.rt, s.rt], chain, async () => (await opFor(b, w.wantId))?.state === 'awaiting_disclosure')) > 0,
      'never funded | ' + logs.slice(-15).join(' | '))
    f1.fault.down = true; f2.fault.down = true
    await drive([b.rt, s.rt], chain, 10) // deaf window — new work pauses, in-flight persists
    const mid = await opFor(b, w.wantId)
    assert.ok(mid !== undefined && mid.state !== 'useful' && mid.state !== 'disclosure_verified', 'deal progressed while deaf — impossible')
    f1.fault.down = false; f2.fault.down = false
    const rounds = await until([b.rt, s.rt], chain, async () => {
      const bo = await opFor(b, w.wantId)
      const so = await saleFor(s, w.wantId)
      return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
    })
    assert.ok(rounds > 0, 'deal never resumed after heal | ' + logs.slice(-20).join(' | '))
    const bo = await opFor(b, w.wantId)
    assert.equal((bo!.data as { escrowTxid?: string }).escrowTxid, (mid.data as { escrowTxid?: string }).escrowTxid, 'escrow was re-broadcast after heal')
  } finally {
    await STOP_ALL([b?.rt, s?.rt])
    await f1.close(); await f2.close(); await h1.close(); await h2.close()
  }
})

// R6 — idempotent redelivery: a host that returns every output twice must not
// double-ingest — one op, one claim, no double spend.
test('R6: duplicate lookup outputs — deal completes once, no double-ingest', { timeout: 300_000 }, async (t) => {
  const chain = (await stackChain(t))!
  const dir = mkdtempSync(join(tmpdir(), 'nektar-res-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const f1 = await faulthost(h1.url)
  f1.fault.duplicateOutputs = true
  let b!: A, s!: A
  try {
    b = await mkagent('buyer', [f1.url, h2.url], chain, logs, dir)
    s = await mkagent('seller', [f1.url, h2.url], chain, logs, dir)
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    const r = await deal(b, s, chain, logs)
    assert.ok(r.rounds > 0)
    assert.equal((await buyerOps(b)).length, 1, 'duplicate outputs spawned extra ops')
    assert.ok(!logs.some((l) => /double|already.*claim|rejected/i.test(l)), 'duplicate delivery leaked into the op path | ' + logs.slice(-15).join(' | '))
  } finally {
    await STOP_ALL([b?.rt, s?.rt])
    await f1.close(); await h1.close(); await h2.close()
  }
})

// R7 — rejection honesty: the escrow broadcast was accepted at submit, then the
// network rejects it (double-spend event). txState must drive the op to
// funding_failed with the budget released — not uncertain, not silent, not retry.
test('R7: late escrow rejection — funding_failed via txState event, budget released', { timeout: 300_000 }, async (t) => {
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
    const w = await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'q?', maxSats: 450 }, b.policyHash)
    assert.ok((await until([b.rt, s.rt], chain, async () => (await opFor(b, w.wantId))?.state === 'awaiting_disclosure')) > 0,
      'escrow never funded | ' + logs.slice(-15).join(' | '))
    const escrowTxid = (await opFor(b, w.wantId))!.data.escrowTxid as string
    const orig = b.mem.txState.bind(b.mem)
    ;(b.mem as unknown as { txState: (txid: string) => Promise<string> }).txState =
      async (txid: string) => (txid === escrowTxid ? 'rejected' : orig(txid))
    const rounds = await until([b.rt, s.rt], chain, async () => (await opFor(b, w.wantId))?.state === 'funding_failed')
    assert.ok(rounds > 0, 'rejected funding did not land funding_failed | ' + logs.slice(-20).join(' | '))
    const bo = (await opFor(b, w.wantId))!
    assert.equal(bo.state, 'funding_failed')
    const evts = await b.rt.store.events(bo.operationId)
    assert.ok(evts.some((e) => e.cause === 'escrow_rejected_late'),
      `expected an escrow_rejected_late event, got: ${evts.map((e) => e.cause).join(', ')}`)
    b.mem.txState = orig as never
    await drive([b.rt], chain, 4) // settled state must stick — no retry resurrection
    assert.equal((await opFor(b, w.wantId))!.state, 'funding_failed', 'rejected funding retried itself back to life')
  } finally {
    await STOP_ALL([b?.rt, s?.rt])
    await h1.close(); await h2.close()
  }
})

// R8 — no-loss invariant: concurrent buyers race; every op ends in a valid
// terminal state and no two settlement txids collide.
test('R8: concurrency invariant — 3 buyers race, all terminal, settlement txids distinct', { timeout: 300_000 }, async (t) => {
  const chain = (await stackChain(t))!
  const dir = mkdtempSync(join(tmpdir(), 'nektar-res-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const buyers: A[] = []
  let s!: A
  try {
    s = await mkagent('seller', [h1.url, h2.url], chain, logs, dir)
    await sellerize(s, 200)
    for (const n of ['b1', 'b2', 'b3']) buyers.push(await mkagent(n, [h1.url, h2.url], chain, logs, dir))
    await drive([s.rt, ...buyers.map((b) => b.rt)], chain, 10)
    const wants = await Promise.all(
      buyers.map((b) => b.rt.engine.publishWant({ domains: ['libfoo'], question: 'q?', maxSats: 450 }, b.policyHash))
    )
    const all = [s.rt, ...buyers.map((b) => b.rt)]
    const terminal = new Set([...BUYER_DONE, 'denied', 'expired', 'funding_failed', 'canceled', 'refunded', 'disputed', 'not_useful'])
    const rounds = await until(all, chain, async () => {
      for (const [i, b] of buyers.entries()) {
        const st = (await opFor(b, wants[i].wantId))?.state
        if (st === undefined || !terminal.has(st)) return false
      }
      return true
    }, 160)
    assert.ok(rounds > 0, 'a buyer op never reached a terminal state | ' + logs.slice(-25).join(' | '))
    const sales = (await saleOps(s)).filter((o) => COMPLETE.has(o.state))
    const txids = sales.map((o) => (o.data as { completeTxid?: string }).completeTxid).filter(Boolean)
    assert.equal(new Set(txids).size, txids.length, 'two settlements spent the same txid — double-spend slipped through')
    const won = (await Promise.all(buyers.map((b, i) => opFor(b, wants[i].wantId)))).filter((o) => o !== undefined && BUYER_DONE.has(o.state)).length
    assert.ok(won >= 1, 'no buyer completed — the race starved everyone')
    console.log(`    race: ${won}/${buyers.length} buyers completed, ${sales.length} seller settlements`)
  } finally {
    await STOP_ALL([s?.rt, ...buyers.map((b) => b.rt)])
    await h1.close(); await h2.close()
  }
})

// R9 — timeout truth: seller takes the money and ghosts. Escrow rail refunds
// the buyer; the direct rail disputes — payment-final is honest.
test('R9: timeout truth — escrow refund vs direct dispute on seller ghosting', { timeout: 300_000 }, async (t) => {
  const chain = (await stackChain(t))!
  // (a) escrow rail → refunded
  {
    const dir = mkdtempSync(join(tmpdir(), 'nektar-res-'))
    const logs: string[] = []
    const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
    const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
    let b!: A, s!: A
    try {
      b = await mkagent('buyer', [h1.url, h2.url], chain, logs, dir, { refundBlocks: 40 })
      s = await mkagent('seller', [h1.url, h2.url], chain, logs, dir, { refundBlocks: 40 })
      await sellerize(s, 200)
      await drive([b.rt, s.rt], chain, 10)
      const w = await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'q?', maxSats: 450 }, b.policyHash)
      // ghost after the quote lands — the accept the buyer sends never gets read
      assert.ok((await until([b.rt, s.rt], chain, async () => (await saleFor(s, w.wantId))?.state === 'quoted')) > 0,
        'no quote | ' + logs.slice(-15).join(' | '))
      await s.rt.stop()
      assert.ok((await until([b.rt], chain, async () => (await opFor(b, w.wantId))?.state === 'awaiting_disclosure')) > 0,
        'escrow never funded | ' + logs.slice(-15).join(' | '))
      await chain.mine(40)
      const refundRounds = await until([b.rt], chain, async () => (await opFor(b, w.wantId))?.state === 'refunded', 40)
      const boEnd = await opFor(b, w.wantId)
      const evts = boEnd !== undefined ? await b.rt.store.events(boEnd.operationId) : []
      assert.ok(refundRounds > 0,
        `escrow rail did not refund — buyer=${boEnd?.state} dueHeight=${boEnd?.dueHeight} lastError=${boEnd?.lastError} events=${evts.map((e) => e.cause).join(',')} | ` + logs.slice(-15).join(' | '))
    } finally {
      await STOP_ALL([b?.rt, s?.rt])
      await h1.close(); await h2.close()
    }
  }
  // (b) direct rail → disputed
  {
    const dir = mkdtempSync(join(tmpdir(), 'nektar-res-'))
    const logs: string[] = []
    const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
    const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
    let b!: A, s!: A
    try {
      b = await mkagent('buyer', [h1.url, h2.url], chain, logs, dir, { refundBlocks: 40, directMaxSats: 500 })
      s = await mkagent('seller', [h1.url, h2.url], chain, logs, dir, { refundBlocks: 40, directAccept: true })
      await sellerize(s, 200)
      await drive([b.rt, s.rt], chain, 10)
      const w = await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'q?', maxSats: 450 }, b.policyHash)
      assert.ok((await until([b.rt, s.rt], chain, async () => (await saleFor(s, w.wantId))?.state === 'quoted')) > 0,
        'no quote | ' + logs.slice(-15).join(' | '))
      await s.rt.stop() // ghost before the accept is read — delivery can never start
      const hit = await until([b.rt], chain, async () => {
        const bo = await opFor(b, w.wantId)
        return bo?.state === 'awaiting_disclosure' && (bo.data as { direct?: boolean }).direct === true
      })
      assert.ok(hit > 0, 'direct payment never sent | ' + logs.slice(-15).join(' | '))
      await chain.mine(40)
      const dispRounds = await until([b.rt], chain, async () => (await opFor(b, w.wantId))?.state === 'disputed', 40)
      const boEnd = await opFor(b, w.wantId)
      const evts = boEnd !== undefined ? await b.rt.store.events(boEnd.operationId) : []
      assert.ok(dispRounds > 0,
        `direct rail did not dispute — buyer=${boEnd?.state} dueHeight=${boEnd?.dueHeight} lastError=${boEnd?.lastError} events=${evts.map((e) => e.cause).join(',')} | ` + logs.slice(-15).join(' | '))
    } finally {
      await STOP_ALL([b?.rt, s?.rt])
      await h1.close(); await h2.close()
    }
  }
})
