// DDIA speed + resilience suite on REAL testnet — the market-resilience
// harness ported to the production live-network wiring: Chaintracks headers
// (SPV-verifiable), BSV Broadcaster submits, WoC status fallback, real funded
// WIFs, and faulthost proxies in front of the live docker bstack hosts.
//
// Zeroconf rules apply: nothing mines, nothing waits for confirmations —
// deals progress on broadcast acceptance + BEEF ancestry. The ONLY block
// gate is R9's covenant refundAfter — a protocol gate, not a wait-for-seal.
//
// Funding custody: each agent gets a FRESH identity key plus a ReserveFunder
// seeded from the live agent's reserve.json snapshot (entries + spent marks
// + deal income). The codex agents must be down so the suite owns the WIFs'
// UTXOs exclusively — respawn them afterwards with .state/respawn-agents.sh.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdirSync, copyFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import knex from 'knex'
import http from 'node:http'
import type { AddressInfo } from 'node:net'
import { PrivateKey, Transaction } from '@bsv/sdk'
import {
  ChaintracksTracker,
  WocTracker,
  BsvBroadcaster,
  fetchTxStatus,
  merklePathMainnet,
  ReserveFunder,
  type MainnetConfig,
  type ChainTracker,
  type Broadcaster
} from '@nektar/chain'
import { faulthost } from '@nektar/bstack/testing'
import { NektarMemory } from '../src/memory.js'
import { callTool } from '../src/server.js'
import { openAutomation, type Automation } from '@nektar/automation'
import { ScriptedExecutor } from '@nektar/automation'
import { DEFAULT_SCORE_WEIGHTS } from '@nektar/automation'
import type { AutomationPolicy } from '@nektar/automation'

const FINDING = 'breaking change: libfoo 3.0 renamed connect() to open(); replace connect( with open( to fix the build'
const NET = 'testnet'
const RUN = Date.now().toString(36)
let dseq = 0
// fresh domain per sub-run: stale ls_nektar records on shared testnet hosts
// must never win collection over this run's live offers
let DOMAIN = 'libfoo'
const HOST_A = 'http://127.0.0.1:18291'
const HOST_B = 'http://127.0.0.1:18292'
const ARC = 'https://arcade-v2-testnet-us-1.bsvblockchain.tech'
const WOC = 'https://api.whatsonchain.com/v1/bsv/test'
const COLLECT_MS = 15_000 // cold-seller margin measured live: want→offer ≈ 8-9s
const STATE = fileURLToPath(new URL('../../../../.state/testnet-agents', import.meta.url))
/** persistent per-WIF suite dirs — lanes.sqlite + reserve.json survive across
 *  tests AND runs: funding trees mint+prove ONCE (one block), then every deal
 *  runs on stock slots. This is the designed stock model — trees pre-mine
 *  ahead so publishing never waits on a block. */
const SUITE_DIR = fileURLToPath(new URL('../../../../.state/tn-suite', import.meta.url))

const WIF: Record<string, string> = {
  alice: 'cPfqX3j79L1QxVVv77C34Caq2hdRoUA61T5jfFZDGp1syWBNxd37',
  ben: 'cRJ9mNASGGJ4kTAf2HhN8CjPkbCwJ6xDiWp5DnvL9xveuM9PtuiF',
  bob: 'cRRCAmoRuN8o5VZo3jWeSf9XjcqeJSSoZ17wUTNSveRyKJHbvyxH'
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

interface A {
  mem: NektarMemory
  rt: Automation
  pub: string
  policyHash: string
  name: string
  dir: string
}

/** live-UTXO reconcile: entries the file still shows free but the network
 *  already spent must be marked before any funding call can take them. Only
 *  runs when WoC answers — a failed probe must not zero the reserve. */
const reconcileReserve = async (fundKey: PrivateKey, reserve: ReserveFunder, mc: MainnetConfig): Promise<void> => {
  const addr = fundKey.toAddress([0x6f]) // testnet pubkey-hash prefix
  const live = await fetch(`${mc.wocUrl}/address/${addr}/unspent`, { signal: AbortSignal.timeout(15_000) })
    .then(async (r) => (r.ok ? ((await r.json()) as Array<{ tx_hash: string; tx_pos: number }>) : undefined))
    .catch(() => undefined)
  if (live === undefined) return
  const liveSet = new Set(live.map((u) => `${u.tx_hash}:${u.tx_pos}`))
  const known = new Set(((reserve as unknown as { entries: Array<{ txid: string; vout: number }> }).entries ?? []).map((e) => `${e.txid}:${e.vout}`))
  for (const u of [...reserve.utxos]) {
    const txid = u.tx.id('hex')
    if (!liveSet.has(`${txid}:${u.vout}`)) reserve.markSpent(txid, u.vout)
  }
  // import live UTXOs the file doesn't know (minted-tree change, settlement
  // income from earlier runs) — fetch each parent tx hex and addOwned it
  for (const u of live) {
    const op = `${u.tx_hash}:${u.tx_pos}`
    if (known.has(op)) continue
    try {
      const r = await fetch(`${mc.wocUrl}/tx/${u.tx_hash}/hex`, { signal: AbortSignal.timeout(10_000) })
      if (!r.ok) continue
      reserve.addOwned(Transaction.fromHex((await r.text()).trim()), u.tx_pos)
      await new Promise((r2) => setTimeout(r2, 350)) // WoC ~3 req/s
    } catch {
      /* skip unparseable */
    }
  }
}

/** all deal-path lanes have a proven funding tree in lanes.sqlite.
 *  `log` warms lazily — it is not on the deal path. */
const treesReady = async (dataDir: string): Promise<boolean> => {
  const f = join(dataDir, 'lanes.sqlite')
  if (!existsSync(f)) return false
  const kx = knex({ client: 'sqlite3', connection: { filename: f }, useNullAsDefault: true })
  try {
    const kinds = (await kx('bstack_trees as t').join('bstack_tree_outputs as o', 'o.txid', 't.txid').where('t.proven', 1).distinct('o.kind')) as Array<{
      kind: string
    }>
    const have = new Set(kinds.map((r) => r.kind))
    return ['market', 'bbox', 'finger'].every((k) => have.has(k))
  } catch {
    return false
  } finally {
    await kx.destroy()
  }
}

/** cycle agents until every deal-path funding tree is proven — the one real
 *  block the suite pays. Persistent dirs make it a once-per-suite cost: a
 *  re-run with surviving lanes.sqlite skips it entirely. */
const warmup = async (agents: A[], logs: string[], ms = 1_500_000): Promise<void> => {
  const t0 = Date.now()
  while (Date.now() - t0 < ms) {
    const pending = [] as A[]
    for (const a of agents) if (!(await treesReady(a.dir))) pending.push(a)
    if (pending.length === 0) return
    await drive(agents.map((a) => a.rt))
    await sleep(15_000) // give the mints time to reach a block
  }
  throw new Error(`warmup: trees never proven | ` + logs.slice(-10).join(' | '))
}

const mcFor = (): MainnetConfig => ({
  broadcasterUrl: ARC,
  wocUrl: WOC
})

const mkChain = (mc: MainnetConfig): ChainTracker & Broadcaster => {
  const tracker = new ChaintracksTracker(mc, `${mc.broadcasterUrl}/chaintracks/v2`, new WocTracker(mc))
  const bcast = new BsvBroadcaster(mc)
  return {
    broadcast: (tx: Transaction) => bcast.broadcast(tx),
    currentHeight: () => tracker.currentHeight(),
    isValidRootForHeight: (r: string, h: number) => tracker.isValidRootForHeight(r, h),
    headerAtHeight: (h: number) => tracker.headerAtHeight(h),
    tipHeight: () => tracker.currentHeight()
  }
}

const mkagent = async (
  name: string,
  wifName: 'alice' | 'ben' | 'bob',
  hosts: string[],
  logs: string[],
  dir: string,
  opts?: Partial<import('@nektar/automation').MarketOptions>,
  watch?: string
): Promise<A> => {
  const wif = WIF[wifName]
  const mc = mcFor()
  // identity-keyed persistent dir — respawns and later tests reuse the SAME
  // lanes/reserve state (exactly what a real agent restart looks like)
  const dataDir = join(dir, wifName)
  mkdirSync(dataDir, { recursive: true })
  const fundKey = PrivateKey.fromWif(wif)
  const reserveFile = join(dataDir, 'reserve.json')
  // custody snapshot on first use only — afterwards the file is live custody
  const src = join(STATE, wifName, 'reserve.json')
  if (!existsSync(reserveFile) && existsSync(src)) copyFileSync(src, reserveFile)
  const reserve = new ReserveFunder(fundKey, reserveFile)
  // reconcile against chain truth: entries the file still shows free but the
  // network already spent (a prior run's tree mint, a killed agent's last
  // broadcast) must never be handed out — that is exactly how the
  // UTXO_SPENT double-mint wedge happened
  await reconcileReserve(fundKey, reserve, mc)
  const tracker = new ChaintracksTracker(mc, `${ARC}/chaintracks/v2`, new WocTracker(mc))
  const bcast = new BsvBroadcaster(mc)
  const chain = {
    broadcast: (tx: Transaction) => bcast.broadcast(tx),
    currentHeight: () => tracker.currentHeight(),
    isValidRootForHeight: (r: string, h: number) => tracker.isValidRootForHeight(r, h),
    headerAtHeight: (h: number) => tracker.headerAtHeight(h),
    tipHeight: () => tracker.currentHeight()
  } as ChainTracker & Broadcaster
  // fundKey IS the WIF — funding inputs pay the WIF's P2PKH and must be
  // signed by it (anchor/provider path). Identity = WIF pub, same as live.
  const mem = new NektarMemory(chain, reserve as never, PrivateKey.fromWif(wif), {
    network: 'testnet',
    modelId: 'test-model-1',
    feeRateSatPerKb: 1,
    txStatus: (txid: string) => fetchTxStatus(mc, txid, { wocFallback: true }),
    batchPolicy: { maxLeaves: 1 }
  })
  // live-runtime parity: pull-based proof + tx lookups so mined txs get their
  // BUMP attached (tree provenance, carrier admission, covenant refund gates)
  mem.proofLookup = async (txid: string) => {
    const r = await merklePathMainnet(mc, txid)
    if (!r) return undefined
    const root = r.merklePath.computeRoot(txid)
    return (await tracker.isValidRootForHeight(root, r.merklePath.blockHeight)) ? r.merklePath : undefined
  }
  mem.txLookup = async (txid: string) => {
    const r = await fetch(`${WOC}/tx/${txid}/hex`, { signal: AbortSignal.timeout(10_000) })
    return r.ok ? Transaction.fromHex((await r.text()).trim()) : undefined
  }
  const pub = mem.operatorKey().toPublicKey().toString()
  const rt = await openAutomation(mem, {
    network: 'testnet',
    localHosts: true, // loopback docker hosts → allowHttp/allowLocal for fingers
    dataDir,
    marketHosts: hosts,
    intervalMs: 0,
    call: (t2, a) => callTool(mem, t2, a),
    log: (l) => logs.push(`${name}:${l}`),
    ai: new ScriptedExecutor(),
    publishFinger: true,
    ...(watch !== undefined ? { watchHosts: [watch], watchPublic: watch } : {}),
    opts: { offerTtlBlocks: 48, refundBlocks: 1, ...opts }
  })
  mem.automation = rt
  return { mem, rt, pub, policyHash: await rt.store.approvePolicy(policyFor(pub), 'test:operator'), name, dir: dataDir }
}

const policyFor = (pub: string): AutomationPolicy => ({
  v: 1,
  policyId: 'testnet-ddia',
  network: NET,
  agentPub: pub,
  roles: ['buyer', 'seller'],
  notBefore: 0,
  notAfter: 4_000_000_000_000,
  domains: [DOMAIN],
  limits: {
    perPurchaseSats: 500,
    dailyPurchaseSats: 50_000,
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

const sellerize = async (s: A, price: number) => {
  await s.mem.remember('guide', FINDING)
  await s.mem.describeShard('guide', { domains: [DOMAIN], offers: [{ kind: 'record', priceSats: price }] })
  for (const l of s.mem.anchorLinks('guide')) s.mem.confirmAnchor('guide', l.txid)
  await s.rt.engine.enableSeller(s.policyHash)
}

const drive = async (rts: Automation[], n = 1) => {
  for (let i = 0; i < n; i++) await Promise.all(rts.map((rt) => rt.cycle()))
}

/** wall-clock until: cycles the engines and returns the elapsed ms when ok()
 *  holds, -1 on timeout. No virtual clock, no mining — real propagation. */
const until = async (rts: Automation[], ms: number, ok: () => Promise<boolean>): Promise<number> => {
  const t0 = Date.now()
  while (Date.now() - t0 < ms) {
    await drive(rts)
    if (await ok()) return Date.now() - t0
    await sleep(800)
  }
  return -1
}

const buyerOps = async (b: A) => (await b.rt.store.operations({ role: 'buyer' })).filter((o) => o.kind === 'purchase')
const saleOps = async (s: A) => (await s.rt.store.operations({ role: 'seller' })).filter((o) => o.kind === 'sale')
const opFor = async (b: A, wantId: string) =>
  (await buyerOps(b)).find((o) => (o.data.want as { want?: { wantId?: string } } | undefined)?.want?.wantId === wantId)
const saleFor = async (s: A, wantId: string) =>
  (await saleOps(s)).find((o) => (o.data.want as { want?: { wantId?: string } } | undefined)?.want?.wantId === wantId)
const COMPLETE = new Set(['completion_submitted', 'settled', 'useful'])
const BUYER_DONE = new Set(['disclosure_verified', 'useful'])

const deal = async (b: A, s: A, logs: string[], ms = 180_000) => {
  const w = await b.rt.engine.publishWant({ domains: [DOMAIN], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
  const t0 = Date.now()
  const elapsed = await until([b.rt, s.rt], ms, async () => {
    const bo = await opFor(b, w.wantId)
    const so = await saleFor(s, w.wantId)
    return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
  })
  assert.ok(elapsed > 0, 'deal never completed | ' + logs.slice(-25).join(' | '))
  return { wantId: w.wantId, ms: elapsed, wallTotal: Date.now() - t0 }
}

const respawn = async (a: A, hosts: string[], logs: string[]): Promise<A> => {
  const wifName = a.name.split('-')[0] as 'alice' | 'ben' | 'bob'
  return await mkagent(a.name, wifName, hosts, logs, SUITE_DIR)
}

const STOP_ALL = async (rts: Array<Automation | undefined>) => {
  for (const r of rts) {
    try {
      await r?.stop()
    } catch {
      /* */
    }
  }
}

/** gate: skip the file when the live hosts or the status endpoints are down —
 *  the suite must never fail a CI run just because testnet is unreachable. */
const live = async (t: { skip: (m?: string) => void }): Promise<boolean> => {
  try {
    for (const u of [HOST_A, HOST_B]) {
      const r = await fetch(`${u}/lookup`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: '{}',
        signal: AbortSignal.timeout(5_000)
      })
      if (r.status >= 500) throw new Error(`${u} → http ${r.status}`)
      await r.arrayBuffer() // drain — any HTTP response proves liveness
    }
    const h = await fetch(`${WOC}/chain/info`, { signal: AbortSignal.timeout(8_000) })
    if (!h.ok) throw new Error(`woc → http ${h.status}`)
    return true
  } catch (e) {
    t.skip(`testnet stack unreachable: ${(e as Error).message}`)
    return false
  }
}

// ── stub watch-shim (same as regtest T3): network-agnostic wake-bus ──
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
        waiters.set(service, [
          ...list,
          (x) => {
            clearTimeout(t)
            r(x)
          }
        ])
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

// ───────────────────────────── speed ─────────────────────────────

test('T1: deal latency distribution — 3 sequential live deals', { timeout: 1_800_000 }, async (t) => {
  DOMAIN = 'libfoo-tn' + RUN + '-' + dseq++
  if (!(await live(t))) return
  const dir = SUITE_DIR
  const logs: string[] = []
  const f1 = await faulthost(HOST_A)
  const f2 = await faulthost(HOST_B)
  let b!: A, s!: A
  const results: number[] = []
  try {
    b = await mkagent('alice-b', 'alice', [f1.url, f2.url], logs, dir)
    s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir)
    await sellerize(s, 200)
    await warmup([b, s], logs)
    await drive([b.rt, s.rt], 6)
    for (let i = 0; i < 3; i++) {
      const r = await deal(b, s, logs)
      results.push(r.ms)
      console.log(`    deal ${i + 1}: ${r.ms}ms wall`)
    }
    results.sort((x, y) => x - y)
    console.log(`    latency: p50=${results[1]}ms max=${results[2]}ms`)
  } finally {
    await STOP_ALL([b?.rt, s?.rt])
    await f1.close()
    await f2.close()
  }
})

test('T2: throughput — 2 concurrent buyers on one seller', { timeout: 1_200_000 }, async (t) => {
  DOMAIN = 'libfoo-tn' + RUN + '-' + dseq++
  if (!(await live(t))) return
  const dir = SUITE_DIR
  const logs: string[] = []
  const f1 = await faulthost(HOST_A)
  const f2 = await faulthost(HOST_B)
  const buyers: A[] = []
  let s!: A
  try {
    s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir)
    await sellerize(s, 200)
    for (const [i, w] of (['alice', 'bob'] as const).entries()) buyers.push(await mkagent(`${w}-b${i}`, w, [f1.url, f2.url], logs, dir))
    await warmup([s, ...buyers], logs)
    await drive([s.rt, ...buyers.map((b) => b.rt)], 6)
    const t0 = Date.now()
    const wants = await Promise.all(buyers.map((b) => b.rt.engine.publishWant({ domains: [DOMAIN], question: 'q?', maxSats: 450 }, b.policyHash)))
    const all = [s.rt, ...buyers.map((b) => b.rt)]
    const ms = await until(all, 240_000, async () => {
      for (const [i, b] of buyers.entries()) {
        const bo = await opFor(b, wants[i].wantId)
        if (bo === undefined || !BUYER_DONE.has(bo.state)) return false
      }
      return (await saleOps(s)).filter((o) => COMPLETE.has(o.state)).length >= wants.length
    })
    assert.ok(ms > 0, 'not all deals landed | ' + logs.slice(-25).join(' | '))
    const settled = (await saleOps(s)).filter((o) => COMPLETE.has(o.state))
    assert.equal(settled.length, wants.length)
    console.log(`    throughput: ${wants.length} live deals in ${ms}ms wall`)
  } finally {
    await STOP_ALL([s?.rt, ...buyers.map((b) => b.rt)])
    await f1.close()
    await f2.close()
  }
})

test('T3: watch-shim A/B on live hosts — notify fires, deal completes', { timeout: 600_000 }, async (t) => {
  DOMAIN = 'libfoo-tn' + RUN + '-' + dseq++
  if (!(await live(t))) return
  const run = async (watch: boolean) => {
    DOMAIN = 'libfoo-tn' + RUN + '-' + dseq++
    const dir = SUITE_DIR
    const logs: string[] = []
    const f1 = await faulthost(HOST_A)
    const f2 = await faulthost(HOST_B)
    const shim = watch ? await stubShim() : undefined
    let b!: A, s!: A
    try {
      b = await mkagent('alice-b', 'alice', [f1.url, f2.url], logs, dir, undefined, shim?.url)
      s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir, undefined, shim?.url)
      await sellerize(s, 200)
      await drive([b.rt, s.rt], 8)
      const w = await b.rt.engine.publishWant({ domains: [DOMAIN], question: 'q?', maxSats: 450 }, b.policyHash)
      const t0 = Date.now()
      const ms = await until([b.rt, s.rt], 180_000, async () => {
        const bo = await opFor(b, w.wantId)
        const so = await saleFor(s, w.wantId)
        return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
      })
      assert.ok(ms > 0, 'deal never completed | ' + logs.slice(-20).join(' | '))
      return { ms, notifies: shim?.notifies() ?? 0 }
    } finally {
      await STOP_ALL([b?.rt, s?.rt])
      await shim?.close()
      await f1.close()
      await f2.close()
    }
  }
  const poll = await run(false)
  const watched = await run(true)
  console.log(`    A/B: poll ${poll.ms}ms vs watch ${watched.ms}ms (notifies=${watched.notifies})`)
  assert.ok(watched.notifies > 0, 'shim never notified — publish→wake path did not fire on testnet')
})

test('T4: payload scaling — 1KB completes; 32KB hits the envelope cap honestly', { timeout: 600_000 }, async (t) => {
  DOMAIN = 'libfoo-tn' + RUN + '-' + dseq++
  if (!(await live(t))) return
  for (const kb of [1, 32]) {
    const dir = SUITE_DIR
    const logs: string[] = []
    const f1 = await faulthost(HOST_A)
    const f2 = await faulthost(HOST_B)
    let b!: A, s!: A
    try {
      b = await mkagent('alice-b', 'alice', [f1.url, f2.url], logs, dir)
      s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir)
      await s.mem.remember('guide', `libfoo 3.0 ${'x'.repeat(kb * 1024)}`)
      await s.mem.describeShard('guide', { domains: [DOMAIN], offers: [{ kind: 'record', priceSats: 200 }] })
      for (const l of s.mem.anchorLinks('guide')) s.mem.confirmAnchor('guide', l.txid)
      await s.rt.engine.enableSeller(s.policyHash)
      await warmup([b, s], logs)
      await drive([b.rt, s.rt], 6)
      const w = await b.rt.engine.publishWant({ domains: [DOMAIN], question: 'q?', maxSats: 450 }, b.policyHash)
      const t0 = Date.now()
      if (kb === 1) {
        const ms = await until([b.rt, s.rt], 180_000, async () => {
          const bo = await opFor(b, w.wantId)
          const so = await saleFor(s, w.wantId)
          return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
        })
        console.log(`    ${kb}KB: ${ms}ms — buyer=${(await opFor(b, w.wantId))?.state}`)
        assert.ok(ms > 0, `1KB deal failed | ` + logs.slice(-15).join(' | '))
      } else {
        // over the 16KiB envelope cap on testnet: the seller broadcasts the
        // covenant spend anyway (paid on-chain) while the delivered envelope
        // dead-letters — the honest end state is visible, not a hang
        const funded = await until([b.rt, s.rt], 120_000, async () => (await opFor(b, w.wantId))?.state === 'awaiting_disclosure')
        assert.ok(funded > 0, 'escrow never funded | ' + logs.slice(-15).join(' | '))
        await until([b.rt, s.rt], 90_000, async () => {
          const so = await saleFor(s, w.wantId)
          return so !== undefined && COMPLETE.has(so.state)
        })
        const bo = await opFor(b, w.wantId)
        const so = await saleFor(s, w.wantId)
        const dead = logs.some((l) => /range: content|dead/i.test(l))
        console.log(`    ${kb}KB: ${Date.now() - t0}ms — buyer=${bo?.state} seller=${so?.state} dead-letter-logged=${dead}`)
        assert.ok(so !== undefined && COMPLETE.has(so.state), 'seller never settled on-chain')
        assert.ok(dead, 'oversize envelope did not dead-letter visibly')
      }
    } finally {
      await STOP_ALL([b?.rt, s?.rt])
      await f1.close()
      await f2.close()
    }
  }
})

// ───────────────────────────── resilience ─────────────────────────────

test('R1: host dies mid-deal — survivor carries it home (live)', { timeout: 600_000 }, async (t) => {
  DOMAIN = 'libfoo-tn' + RUN + '-' + dseq++
  if (!(await live(t))) return
  const dir = SUITE_DIR
  const logs: string[] = []
  const f1 = await faulthost(HOST_A)
  const f2 = await faulthost(HOST_B)
  let b!: A, s!: A
  try {
    b = await mkagent('alice-b', 'alice', [f1.url, f2.url], logs, dir)
    s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir)
    await sellerize(s, 200)
    await warmup([b, s], logs)
    await drive([b.rt, s.rt], 6)
    const w = await b.rt.engine.publishWant({ domains: [DOMAIN], question: 'q?', maxSats: 450 }, b.policyHash)
    assert.ok(
      (await until([b.rt, s.rt], 90_000, async () => (await saleFor(s, w.wantId))?.state === 'quoted')) > 0,
      'no quote | ' + logs.slice(-15).join(' | ')
    )
    f1.fault.down = true // kill host A mid-deal
    const ms = await until([b.rt, s.rt], 120_000, async () => {
      const bo = await opFor(b, w.wantId)
      const so = await saleFor(s, w.wantId)
      return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
    })
    assert.ok(ms > 0, 'deal starved without host A | ' + logs.slice(-20).join(' | '))
  } finally {
    await STOP_ALL([b?.rt, s?.rt])
    await f1.close()
    await f2.close()
  }
})

test('R2: one host delayed 3s — quorum-1 rides the fast host (live)', { timeout: 600_000 }, async (t) => {
  DOMAIN = 'libfoo-tn' + RUN + '-' + dseq++
  if (!(await live(t))) return
  const dir = SUITE_DIR
  const logs: string[] = []
  const f1 = await faulthost(HOST_A)
  f1.fault.delayMs = 3_000
  const f2 = await faulthost(HOST_B)
  let b!: A, s!: A
  try {
    b = await mkagent('alice-b', 'alice', [f1.url, f2.url], logs, dir)
    s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir)
    await sellerize(s, 200)
    await warmup([b, s], logs)
    await drive([b.rt, s.rt], 6)
    const ms = (await deal(b, s, logs, 200_000)).ms
    console.log(`    deal under 3s-delayed host: ${ms}ms`)
  } finally {
    await STOP_ALL([b?.rt, s?.rt])
    await f1.close()
    await f2.close()
  }
})

test('R3: withholding host loses the offer — quorum-1 + withheld bookkeeping (live)', { timeout: 600_000 }, async (t) => {
  DOMAIN = 'libfoo-tn' + RUN + '-' + dseq++
  if (!(await live(t))) return
  const dir = SUITE_DIR
  const logs: string[] = []
  const f1 = await faulthost(HOST_A)
  const f2 = await faulthost(HOST_B)
  let b!: A, s!: A
  try {
    b = await mkagent('alice-b', 'alice', [f1.url, f2.url], logs, dir)
    s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir)
    await sellerize(s, 200)
    await warmup([b, s], logs)
    await drive([b.rt, s.rt], 6)
    const w = await b.rt.engine.publishWant({ domains: [DOMAIN], question: 'q?', maxSats: 450 }, b.policyHash)
    assert.ok(
      (await until([b.rt, s.rt], 90_000, async () => (await saleFor(s, w.wantId))?.state === 'quoted')) > 0,
      'no quote | ' + logs.slice(-15).join(' | ')
    )
    // withhold everything the seller just fanned out to host A — the host
    // stays reachable but stops serving the new carriers
    f1.fault.dropLookup = true
    const ms = await until([b.rt, s.rt], 120_000, async () => {
      const bo = await opFor(b, w.wantId)
      const so = await saleFor(s, w.wantId)
      return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
    })
    assert.ok(ms > 0, 'withheld offer starved the deal | ' + logs.slice(-20).join(' | '))
    assert.ok(
      logs.some((l) => l.includes('withheld') || l.includes('unreachable')),
      'divergence never surfaced in logs'
    )
  } finally {
    await STOP_ALL([b?.rt, s?.rt])
    await f1.close()
    await f2.close()
  }
})

test('R4: partition both hosts → deaf window → heal → completes, no double-broadcast (live)', { timeout: 600_000 }, async (t) => {
  DOMAIN = 'libfoo-tn' + RUN + '-' + dseq++
  if (!(await live(t))) return
  const dir = SUITE_DIR
  const logs: string[] = []
  const f1 = await faulthost(HOST_A)
  const f2 = await faulthost(HOST_B)
  let b!: A, s!: A
  try {
    b = await mkagent('alice-b', 'alice', [f1.url, f2.url], logs, dir)
    s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir)
    await sellerize(s, 200)
    await warmup([b, s], logs)
    await drive([b.rt, s.rt], 6)
    const w = await b.rt.engine.publishWant({ domains: [DOMAIN], question: 'q?', maxSats: 450 }, b.policyHash)
    assert.ok(
      (await until([b.rt, s.rt], 120_000, async () => (await opFor(b, w.wantId))?.state === 'awaiting_disclosure')) > 0,
      'never funded | ' + logs.slice(-15).join(' | ')
    )
    f1.fault.down = true
    f2.fault.down = true
    await warmup([b, s], logs)
    await drive([b.rt, s.rt], 6) // deaf window — in-flight ops persist, no new work
    const mid = await opFor(b, w.wantId)
    assert.ok(mid !== undefined && mid.state !== 'useful' && mid.state !== 'disclosure_verified', 'deal progressed while deaf — impossible')
    f1.fault.down = false
    f2.fault.down = false
    const ms = await until([b.rt, s.rt], 120_000, async () => {
      const bo = await opFor(b, w.wantId)
      const so = await saleFor(s, w.wantId)
      return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
    })
    assert.ok(ms > 0, 'deal never resumed after heal | ' + logs.slice(-20).join(' | '))
    const bo = await opFor(b, w.wantId)
    assert.equal((bo!.data as { escrowTxid?: string }).escrowTxid, (mid.data as { escrowTxid?: string }).escrowTxid, 'escrow re-broadcast after heal')
  } finally {
    await STOP_ALL([b?.rt, s?.rt])
    await f1.close()
    await f2.close()
  }
})

test('R5: buyer killed at awaiting_disclosure → respawn → completes (live)', { timeout: 600_000 }, async (t) => {
  DOMAIN = 'libfoo-tn' + RUN + '-' + dseq++
  if (!(await live(t))) return
  const dir = SUITE_DIR
  const logs: string[] = []
  const f1 = await faulthost(HOST_A)
  const f2 = await faulthost(HOST_B)
  let b!: A, s!: A, b2!: A
  try {
    b = await mkagent('alice-b', 'alice', [f1.url, f2.url], logs, dir)
    s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir)
    await sellerize(s, 200)
    await warmup([b, s], logs)
    await drive([b.rt, s.rt], 6)
    const w = await b.rt.engine.publishWant({ domains: [DOMAIN], question: 'q?', maxSats: 450 }, b.policyHash)
    assert.ok(
      (await until([b.rt, s.rt], 120_000, async () => (await opFor(b, w.wantId))?.state === 'awaiting_disclosure')) > 0,
      'never funded | ' + logs.slice(-15).join(' | ')
    )
    const opId = (await opFor(b, w.wantId))!.operationId
    await b.rt.stop() // kill -9 equivalent — store persists, respawn resumes
    b2 = await respawn(b, [f1.url, f2.url], logs)
    const ms = await until([b2.rt, s.rt], 120_000, async () => {
      const bo = (await buyerOps(b2)).find((o) => o.operationId === opId)
      const so = await saleFor(s, w.wantId)
      return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
    })
    assert.ok(ms > 0, 'respawned buyer never resumed | ' + logs.slice(-20).join(' | '))
    const bo = (await buyerOps(b2)).find((o) => o.operationId === opId)
    assert.ok(bo !== undefined && BUYER_DONE.has(bo.state), `op ${opId} ended ${bo?.state}`)
  } finally {
    await STOP_ALL([b?.rt, b2?.rt, s?.rt])
    await f1.close()
    await f2.close()
  }
})

test('R6: duplicate lookup outputs — deal completes once (live)', { timeout: 600_000 }, async (t) => {
  DOMAIN = 'libfoo-tn' + RUN + '-' + dseq++
  if (!(await live(t))) return
  const dir = SUITE_DIR
  const logs: string[] = []
  const f1 = await faulthost(HOST_A)
  f1.fault.duplicateOutputs = true
  const f2 = await faulthost(HOST_B)
  let b!: A, s!: A
  try {
    b = await mkagent('alice-b', 'alice', [f1.url, f2.url], logs, dir)
    s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir)
    await sellerize(s, 200)
    await warmup([b, s], logs)
    await drive([b.rt, s.rt], 6)
    const r = await deal(b, s, logs)
    assert.ok(r.ms > 0)
    assert.equal((await buyerOps(b)).length, 1, 'duplicate outputs spawned extra ops')
  } finally {
    await STOP_ALL([b?.rt, s?.rt])
    await f1.close()
    await f2.close()
  }
})

test('R7: late escrow rejection → funding_failed via txState event (live)', { timeout: 600_000 }, async (t) => {
  DOMAIN = 'libfoo-tn' + RUN + '-' + dseq++
  if (!(await live(t))) return
  const dir = SUITE_DIR
  const logs: string[] = []
  const f1 = await faulthost(HOST_A)
  const f2 = await faulthost(HOST_B)
  let b!: A, s!: A
  try {
    b = await mkagent('alice-b', 'alice', [f1.url, f2.url], logs, dir)
    s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir)
    await sellerize(s, 200)
    await warmup([b, s], logs)
    await drive([b.rt, s.rt], 6)
    const w = await b.rt.engine.publishWant({ domains: [DOMAIN], question: 'q?', maxSats: 450 }, b.policyHash)
    assert.ok(
      (await until([b.rt, s.rt], 120_000, async () => (await opFor(b, w.wantId))?.state === 'awaiting_disclosure')) > 0,
      'escrow never funded | ' + logs.slice(-15).join(' | ')
    )
    const escrowTxid = (await opFor(b, w.wantId))!.data.escrowTxid as string
    const orig = b.mem.txState.bind(b.mem)
    ;(b.mem as unknown as { txState: (txid: string) => Promise<string> }).txState = async (txid: string) => (txid === escrowTxid ? 'rejected' : orig(txid))
    const ms = await until([b.rt], 90_000, async () => (await opFor(b, w.wantId))?.state === 'funding_failed')
    assert.ok(ms > 0, 'rejected funding did not land funding_failed | ' + logs.slice(-20).join(' | '))
    const bo = (await opFor(b, w.wantId))!
    const evts = await b.rt.store.events(bo.operationId)
    assert.ok(
      evts.some((e) => e.cause === 'escrow_rejected_late'),
      `expected escrow_rejected_late, got ${evts.map((e) => e.cause).join(',')}`
    )
    ;(b.mem as unknown as { txState: (txid: string) => Promise<string> }).txState = orig
    await drive([b.rt], 3)
    assert.equal((await opFor(b, w.wantId))!.state, 'funding_failed', 'rejected funding retried itself back to life')
  } finally {
    await STOP_ALL([b?.rt, s?.rt])
    await f1.close()
    await f2.close()
  }
})

test('R8: concurrency invariant — 2 buyers race, all terminal, distinct settlements (live)', { timeout: 600_000 }, async (t) => {
  DOMAIN = 'libfoo-tn' + RUN + '-' + dseq++
  if (!(await live(t))) return
  const dir = SUITE_DIR
  const logs: string[] = []
  const f1 = await faulthost(HOST_A)
  const f2 = await faulthost(HOST_B)
  const buyers: A[] = []
  let s!: A
  try {
    s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir)
    await sellerize(s, 200)
    for (const [i, w] of (['alice', 'bob'] as const).entries()) buyers.push(await mkagent(`${w}-b${i}`, w, [f1.url, f2.url], logs, dir))
    await warmup([s, ...buyers], logs)
    await drive([s.rt, ...buyers.map((b) => b.rt)], 6)
    const wants = await Promise.all(buyers.map((b) => b.rt.engine.publishWant({ domains: [DOMAIN], question: 'q?', maxSats: 450 }, b.policyHash)))
    const all = [s.rt, ...buyers.map((b) => b.rt)]
    const terminal = new Set([...BUYER_DONE, 'denied', 'expired', 'funding_failed', 'canceled', 'refunded', 'disputed', 'not_useful'])
    const ms = await until(all, 240_000, async () => {
      for (const [i, b] of buyers.entries()) {
        const st = (await opFor(b, wants[i].wantId))?.state
        if (st === undefined || !terminal.has(st)) return false
      }
      return true
    })
    assert.ok(ms > 0, 'a buyer op never reached terminal | ' + logs.slice(-25).join(' | '))
    const sales = (await saleOps(s)).filter((o) => COMPLETE.has(o.state))
    const txids = sales.map((o) => (o.data as { completeTxid?: string }).completeTxid).filter(Boolean)
    assert.equal(new Set(txids).size, txids.length, 'settlement txid collision — double-spend slipped through')
    const won = (await Promise.all(buyers.map((b, i) => opFor(b, wants[i].wantId)))).filter((o) => o !== undefined && BUYER_DONE.has(o.state)).length
    console.log(`    race: ${won}/${buyers.length} buyers completed, ${sales.length} settlements`)
  } finally {
    await STOP_ALL([s?.rt, ...buyers.map((b) => b.rt)])
    await f1.close()
    await f2.close()
  }
})

// R9 — the only block-gated test: covenant refundAfter is a REAL protocol
// gate (absolute height). refundBlocks=1 → one testnet block lapses both
// rails' dueHeights; both deals run concurrently so a single block covers
// the refund (escrow) AND the dispute (direct) — ~10-20min total.
test('R9: timeout truth — escrow refund vs direct dispute on real blocks', { timeout: 1_800_000 }, async (t) => {
  DOMAIN = 'libfoo-tn' + RUN + '-' + dseq++
  if (!(await live(t))) return
  const dir = SUITE_DIR
  const logs: string[] = []
  const f1 = await faulthost(HOST_A)
  const f2 = await faulthost(HOST_B)
  let be!: A, bd!: A, s!: A
  try {
    s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir, { refundBlocks: 1, directAccept: true })
    await sellerize(s, 200)
    be = await mkagent('alice-e', 'alice', [f1.url, f2.url], logs, dir, { refundBlocks: 1 })
    bd = await mkagent('bob-d', 'bob', [f1.url, f2.url], logs, dir, { refundBlocks: 1, directMaxSats: 500 })
    await warmup([be, bd, s], logs)
    await drive([s.rt, be.rt, bd.rt], 6)
    const wE = await be.rt.engine.publishWant({ domains: [DOMAIN], question: 'escrow?', maxSats: 450 }, be.policyHash)
    const wD = await bd.rt.engine.publishWant({ domains: [DOMAIN], question: 'direct?', maxSats: 450 }, bd.policyHash)
    const all = [s.rt, be.rt, bd.rt]
    // wait until BOTH sale ops are quoted, then ghost the seller — accepts it
    // never reads can never be delivered
    assert.ok(
      (await until(all, 120_000, async () => (await saleFor(s, wE.wantId))?.state === 'quoted' && (await saleFor(s, wD.wantId))?.state === 'quoted')) > 0,
      'not both quoted | ' + logs.slice(-15).join(' | ')
    )
    await s.rt.stop()
    assert.ok(
      (await until(
        [be.rt, bd.rt],
        180_000,
        async () => (await opFor(be, wE.wantId))?.state === 'awaiting_disclosure' && (await opFor(bd, wD.wantId))?.state === 'awaiting_disclosure'
      )) > 0,
      'rails never funded | ' + logs.slice(-15).join(' | ')
    )
    const boD = await opFor(bd, wD.wantId)
    assert.ok((boD!.data as { direct?: boolean }).direct === true, 'bob did not take the direct rail')
    // one real testnet block lapses refundAfter=1 on both escrows/payments
    const h0 = await be.mem.chainHeight()
    console.log(`    funded at height ${h0} — waiting for one block…`)
    const ms = await until(
      [be.rt, bd.rt],
      1_500_000,
      async () => (await opFor(be, wE.wantId))?.state === 'refunded' && (await opFor(bd, wD.wantId))?.state === 'disputed'
    )
    const eEnd = await opFor(be, wE.wantId)
    const dEnd = await opFor(bd, wD.wantId)
    assert.ok(ms > 0, `timeout truth failed — escrow=${eEnd?.state} direct=${dEnd?.state} | ` + logs.slice(-15).join(' | '))
    console.log(`    R9: escrow→${eEnd!.state} direct→${dEnd!.state} in ${ms}ms`)
  } finally {
    await STOP_ALL([be?.rt, bd?.rt, s?.rt])
    await f1.close()
    await f2.close()
  }
})
