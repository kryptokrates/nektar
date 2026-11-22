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
//
// Workflow: warm lane stock FIRST — `node tools/tn-warmup.mjs` (unbounded,
// Targeted runs: `--test-name-pattern` + `TN_TREE_FLOOR=<n>` (gate floor,
// default 8) + `TN_VERBOSE=1` (live per-agent logs — never run a live suite
// silent: TAP buffers until exit, so a quiet process is indistinguishable
// from a wedged one; the suite's own quiet-heartbeat prints past 90s silence)
// resumeable, owns the block wait). Tests then fail fast on cold stock
// instead of paying a 25-min block lottery inside their timeout; opt back
// in with TN_WARMUP=1. Iterate single tests with
// `node --test --test-name-pattern='R5' ...`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdirSync, copyFileSync, existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import http from 'node:http'
import type { AddressInfo } from 'node:net'
import { PrivateKey } from '@bsv/sdk'
import {
  indexFailover,
  livePort,
  reconcileLiveReserve,
  ReserveFunder,
  fundsOf,
  testTier,
  type MainnetConfig,
  type ChainTracker,
  type Broadcaster
} from '@nektar/chain'
import { faulthost } from '@nektar/bstack/testing'
import { NektarMemory } from '../src/memory.js'
import { acquireLock, keyLockFile } from '../src/index.js'
import { callTool } from '../src/server.js'
import { openAutomation, type Automation, Dispatcher } from '@nektar/automation'
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
// the compose publishes 18291/18292 by default; a stack started with a
// port-override map (or a stale process squatting the defaults) answers on
// different ports — NEKTAR_TN_HOST_A/B override
const HOST_A = process.env.NEKTAR_TN_HOST_A ?? 'http://127.0.0.1:18291'
const HOST_B = process.env.NEKTAR_TN_HOST_B ?? 'http://127.0.0.1:18292'
// the suite exercises the paid-disclosure deal path end-to-end — without the
// flag every sale#* op throws and offers never leave the seller
process.env.NEKTAR_PAID_DISCLOSURE ??= '1'
const ARC = 'https://arcade-v2-testnet-us-1.bsvblockchain.tech'
// the index dialect is picked from the URL (bananablocks default; WoC when
// the primary rate-limits the suite's read volume — indexFor guesses the
// dialect from the host)
const INDEX = process.env.NEKTAR_TN_INDEX ?? 'https://test.bananablocks.com/api/v1'
// WoC testnet mirror — banana 429s under suite read volume; failover keeps a
// throttled primary from reading as chain-unreachable (preflight + agents)
const TN_INDEXES = [...new Set([INDEX, 'https://api.whatsonchain.com/v1/bsv/test'])]
// happy path is unaffected — early-select fires at minCollectMs (1s) once an
// offer lands; this ceiling only bounds the degraded path: a withholding
// host leaves one carrier route, and single-host submit→collect→decode
// measured ~16-20s live, over the old 15s line (R3's expired-in-collecting).
const COLLECT_MS = 45_000 // cold-seller margin measured live: want→offer ≈ 8-9s healthy, ~20s single-host
const STATE = fileURLToPath(new URL('../../../../.state/testnet-agents', import.meta.url))
/** persistent per-WIF suite dirs — lanes.sqlite + reserve.json survive across
 *  tests AND runs: funding trees mint+prove ONCE (one block), then every deal
 *  runs on stock slots. This is the designed stock model — trees pre-mine
 *  ahead so publishing never waits on a block. */
const SUITE_DIR = fileURLToPath(new URL('../../../../.state/tn-suite', import.meta.url))

// keys live in the gitignored state dir, never in source — the suite skips
// when they are absent (see `live`)
const KEYS_FILE = join(STATE, 'agent-keys.json')
const WIF: Record<string, string> = existsSync(KEYS_FILE) ? (JSON.parse(readFileSync(KEYS_FILE, 'utf8')) as Record<string, string>) : {}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

interface A {
  mem: NektarMemory
  rt: Automation
  pub: string
  policyHash: string
  name: string
  dir: string
}

/** all deal-path lanes hold free slots on a proven funding tree — counted,
 *  not just present: a run that opens with one free bbox slot burns it on
 *  the first carrier, then every disclosure rides a block wait inside the
 *  deal's own deadline. Eight free slots covers a test's carrier traffic
 *  with margin; drier stock tops up in warmup where the block wait belongs.
 *  `log` warms lazily — not on the deal path. The laneStock accessor is the
 *  ledger's own accounting — the suite asks the runtime, never the tables. */
const TREE_READY_FLOOR = Number(process.env.TN_TREE_FLOOR ?? 8)
// mirrors anchoredKinds() in bstack/tree.ts: 'all'|'1' → every carrier kind,
// market is always anchored-capable. Servable = proven + anchored.
const ANCHORED_V = process.env.NEKTAR_ANCHORED_LANES ?? ''
const ANCHORED_KINDS = new Set(
  ANCHORED_V === 'all' || ANCHORED_V === '1'
    ? ['bbox', 'log', 'finger']
    : ANCHORED_V.split(',')
        .map((s) => s.trim())
        .filter(Boolean)
)
ANCHORED_KINDS.add('market')
const treesReady = async (rt: Automation): Promise<boolean> => {
  const s = await rt.laneStock()
  return ['market', 'bbox', 'finger'].every((k) => (s[k]?.proven ?? 0) + (ANCHORED_KINDS.has(k) ? (s[k]?.anchored ?? 0) : 0) >= TREE_READY_FLOOR)
}

/** cycle agents until every deal-path funding tree is proven — the one real
 *  block the suite pays. Persistent dirs make it a once-per-suite cost: a
 *  re-run with surviving lanes.sqlite skips it entirely. */
const warmup = async (agents: A[], logs: string[], ms = Number(process.env.TN_WARMUP_MS ?? 1_500_000)): Promise<void> => {
  const t0 = Date.now()
  while (Date.now() - t0 < ms) {
    const pending = [] as A[]
    for (const a of agents) if (!(await treesReady(a.rt))) pending.push(a)
    if (pending.length === 0) return
    // cold stock is a block wait, not test work — it belongs in the standalone
    // warmer (tools/tn-warmup.mjs, unbounded, resumeable) unless the operator
    // explicitly opts back into paying it in-test.
    if (process.env.TN_WARMUP !== '1')
      throw new Error(
        `lane stock unproven (${pending.map((a) => a.name).join(', ')}) — run \`node tools/tn-warmup.mjs\` first, or set TN_WARMUP=1 to pay the block wait inside this test`
      )
    await drive(agents.map((a) => a.rt))
    await sleep(15_000) // give the mints time to reach a block
  }
  throw new Error(`warmup: trees never proven | ` + logs.slice(-10).join(' | '))
}

const mcFor = (): MainnetConfig => ({
  broadcasterUrl: ARC,
  // broadcast fan-out, same as runtime defaults — a strained endpoint can't
  // stall submits; TAAL + eu-1 accept the same bytes concurrently, first wins
  broadcasterUrls: ['https://arc-test.taal.com', 'https://arcade-v2-testnet-eu-1.bsvblockchain.tech'],
  indexerUrl: INDEX,
  indexerUrls: TN_INDEXES.slice(1)
})

// liveness that survives TAP buffering: a long-running test writes nothing to
// stdout until it ends, so silence is indistinguishable from a wedge. Any agent
// log line refreshes the stamp; the interval re-prints it when >90s stale —
// every pass/wake/transition keeps this beating, no per-test plumbing needed
const LIVENESS = { at: 0, line: '' }
setInterval(() => {
  const quiet = Date.now() - LIVENESS.at
  if (LIVENESS.at > 0 && quiet > 90_000) console.error(`[suite quiet ${(quiet / 1000) | 0}s] last: ${LIVENESS.line}`)
}, 30_000).unref()

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
  const pkhHex = Buffer.from(fundKey.toPublicKey().toHash()).toString('hex')
  // one process per fund key — refuses while a live agent (tmux) holds it;
  // the live() gate below is the earlier, friendlier skip
  const keyRelease = await acquireLock(keyLockFile(pkhHex), `fund key ${wifName}`)
  const reserveFile = join(dataDir, 'reserve.json')
  // custody snapshot on first use only — afterwards the file is live custody
  const src = join(STATE, wifName, 'reserve.json')
  if (!existsSync(reserveFile) && existsSync(src)) copyFileSync(src, reserveFile)
  const reserve = new ReserveFunder(fundKey, reserveFile)
  const addr = fundKey.toAddress([0x6f]) // testnet pubkey-hash prefix
  // the live port — the same witnessed-tracker + SpendGuard-wrapped
  // broadcaster + proof/tx/spend lookups the production runtime composes
  // (port.ts); the suite adapts it, it does not rebuild it
  const port = await livePort(mc, 'testnet', {
    dataDir,
    log: (l) => logs.push(`${name}:${l}`),
    // SpendGuard's spend metric — the reserve prices its own funding inputs;
    // covenant/foreign inputs return undefined and count fee-only
    valueOf: (txid, vout) => reserve.utxos.find((u) => u.tx.id('hex') === txid && u.vout === vout)?.satoshis
  })
  const spendStatus = port.spendStatus ?? (async () => 'unknown' as const)
  // reconcile against chain truth: entries the file still shows free but the
  // network already spent (a prior run's tree mint, a killed agent's last
  // broadcast) must never be handed out — that is exactly how the
  // UTXO_SPENT double-mint wedge happened
  await reconcileLiveReserve(reserve, addr, mc, spendStatus)
  const index = indexFailover(TN_INDEXES, { apiKey: mc.indexerApiKey })
  const chain = {
    broadcast: port.broadcast,
    currentHeight: () => port.height(),
    isValidRootForHeight: (r: string, h: number) => port.tracker.isValidRootForHeight(r, h),
    headerAtHeight: (h: number) => port.tracker.headerAtHeight(h),
    tipHeight: () => port.height()
  } as ChainTracker & Broadcaster
  // fundKey IS the WIF — funding inputs pay the WIF's P2PKH and must be
  // signed by it (anchor/provider path). Identity = WIF pub, same as live.
  const mem = new NektarMemory(chain, reserve, PrivateKey.fromWif(wif), {
    network: 'testnet',
    modelId: 'test-model-1',
    feeRateSatPerKb: 1,
    statusHint: port.statusHint,
    batchPolicy: { maxLeaves: 1 },
    // fundsOf needs the owned lock — ReserveFunder keeps pkhHex private, so
    // the suite hands it the same hash the key lock is filed under. The
    // oracles are the port's own lookups (production wires the same at
    // runtime.ts): spendCheck lets take() burn phantom change entries —
    // broadcast churn the chain already consumed elsewhere
    funds: fundsOf(reserve, {
      pkhHex,
      spendCheck: async (txid, vout) => (await spendStatus(txid, vout)) === 'spent',
      // starve sweep's revive oracle — the index's address listing positively
      // names live outpoints (the same oracle reconcileLiveReserve trusts at
      // boot); an unanswered call revives nothing, never a chain claim
      liveSet: async () => {
        const live = await index.utxos(addr).catch(() => undefined)
        return live === undefined ? undefined : new Set(live.map((u) => `${u.txid}:${u.vout}`))
      },
      // take() attaches the funding parent's witnessed BUMP — without it the
      // host's SPV verify walks to parent-of-parent and refuses every submit
      proofOf: async (tx) => {
        const mp = await port.proofLookup?.(tx.id('hex'))
        if (mp !== undefined) tx.merklePath = mp
      }
    })
  })
  // live-runtime parity: pull-based proof + tx lookups so mined txs get their
  // BUMP attached (tree provenance, carrier admission, covenant refund gates)
  if (port.proofLookup !== undefined) mem.proofLookup = port.proofLookup
  if (port.txLookup !== undefined) mem.txLookup = port.txLookup
  const pub = mem.operatorKey().toPublicKey().toString()
  const rt = await openAutomation(mem, {
    network: 'testnet',
    localHosts: true, // loopback docker hosts → allowHttp/allowLocal for fingers
    dataDir,
    marketHosts: hosts,
    intervalMs: 0,
    // in-process executor — several agents share this process; cfg.exec wires
    // the service bodies onto a per-runtime dispatcher, no restate endpoint
    exec: new Dispatcher(),
    call: (t2, a) => callTool(mem, t2, a),
    log: (l) => {
      const line = `${new Date().toISOString().slice(11, 19)} ${name}:${l}`
      logs.push(line)
      LIVENESS.at = Date.now()
      LIVENESS.line = line
      if (process.env.TN_VERBOSE === '1') console.error(line)
    },
    ai: new ScriptedExecutor(),
    publishFinger: true,
    // in-flight mint ceiling — NEKTAR_TREE_MAX_UNPROVEN lets a run overfill
    // the pipeline when the gate needs a lane refilled in one block-window
    treeMaxUnproven: Number(process.env.NEKTAR_TREE_MAX_UNPROVEN ?? 0) || undefined,
    ...(watch !== undefined ? { watchHosts: [watch], watchPublic: watch } : {}),
    opts: { offerTtlBlocks: 48, ...opts }
  })
  mem.automation = rt
  const stopRt = rt.stop.bind(rt)
  rt.stop = async () => {
    await stopRt()
    await keyRelease()
    liveRuntimes.delete(rt)
  }
  liveRuntimes.add(rt)
  return { mem, rt, pub, policyHash: await rt.store.approvePolicy(policyFor(pub), 'test:operator'), name, dir: dataDir }
}

/** every runtime this suite has live — a timed-out test keeps its agents
 *  (and their key beats) running in-process until its function settles, so
 *  live() drains same-pid leftovers instead of cascade-skipping the file */
const liveRuntimes = new Set<Automation>()

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
    perTxFeeSats: 1200, // 500 escrow reserve + payload-sized maxFee headroom
    dailyFeeSats: 50_000,
    runTotalSats: 500_000,
    maxLockedSats: 5000,
    maxUnconfirmedSats: 5000,
    maxUnconfirmedCount: 16
  },
  scoring: { minimumScoreBps: 0, weights: { ...DEFAULT_SCORE_WEIGHTS }, collectMs: COLLECT_MS },
  seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: 5000, sellableShards: ['guide'] },
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

/** priming passes: deals every pass, ONE upkeep pass overlapping them —
 *  serializing ~90s of upkeep per iteration makes spinup alone outrun a
 *  test's whole deal budget (the same wedge until() had). n=1 keeps the
 *  warmup loop's old shape: one deals pass + one upkeep pass, awaited. */
const drive = async (rts: Automation[], n = 1) => {
  const up = Promise.all(rts.map((rt) => rt.upkeep()))
  for (let i = 0; i < n; i++) await Promise.all(rts.map((rt) => rt.deals()))
  await up
}

/** wall-clock until: polls the engines and returns the elapsed ms when ok()
 *  holds, -1 on timeout. No virtual clock, no mining — real propagation.
 *  Polls run on the production cadence (deals every pass, upkeep on a ~10s
 *  ceiling, detached): a slow upkeep — a starved reserve probing hundreds of
 *  candidates — must never hold a want's 15s collect window hostage. */
const until = async (rts: Automation[], ms: number, ok: () => Promise<boolean>): Promise<number> => {
  const t0 = Date.now()
  let upkeepAt = 0
  let upkeepRunning = false
  while (Date.now() - t0 < ms) {
    await Promise.all(rts.map((rt) => rt.deals()))
    if (!upkeepRunning && Date.now() - upkeepAt >= 10_000) {
      upkeepAt = Date.now()
      upkeepRunning = true
      void Promise.all(rts.map((rt) => rt.upkeep()))
        .catch(() => undefined)
        .finally(() => {
          upkeepRunning = false
        })
    }
    if (await ok()) return Date.now() - t0
    await sleep(150)
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

const deal = async (b: A, s: A, logs: string[], ms = 400_000) => {
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

const respawn = async (a: A, hosts: string[], logs: string[], watch?: string): Promise<A> => {
  const wifName = a.name.split('-')[0] as 'alice' | 'ben' | 'bob'
  return await mkagent(a.name, wifName, hosts, logs, SUITE_DIR, undefined, watch)
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
  // testnet coins move — only an explicit testnet tier may run this file
  if (testTier() !== 'testnet') {
    t.skip('testnet tier only — set NEKTAR_TEST_TIER=testnet to run')
    return false
  }
  if (['alice', 'ben', 'bob'].some((n) => WIF[n] === undefined)) {
    t.skip(`no agent keys at ${KEYS_FILE}`)
    return false
  }
  // one process per fund key — a live tmux agent holds the lock file. A beat
  // <20s old can also be THIS suite's own timed-out test still running its
  // agents in-process (a node:test timeout reports the test but cannot cancel
  // the function): owner "pid@host" distinguishes the two — same-pid beats get
  // force-drained, external owners get a bounded wait for the beat to go stale
  for (const n of ['alice', 'ben', 'bob'] as const) {
    const lf = keyLockFile(Buffer.from(PrivateKey.fromWif(WIF[n]).toPublicKey().toHash()).toString('hex'))
    const waitUntil = Date.now() + 45_000
    for (;;) {
      let l: { owner?: string; beat: number } | undefined
      try {
        l = JSON.parse(readFileSync(lf, 'utf8')) as { owner?: string; beat: number }
      } catch {
        break /* no lock held */
      }
      if (l === undefined || Date.now() - l.beat >= 20_000) break // stale beat — no live owner
      if (Number(l.owner?.split('@')[0]) === process.pid) await STOP_ALL([...liveRuntimes])
      if (Date.now() >= waitUntil) {
        t.skip(`live agent holds ${n}'s key — stop it before running this suite`)
        return false
      }
      await new Promise((r) => setTimeout(r, 3_000))
    }
  }
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
    // dialect-agnostic liveness: the index client's own tip read — a raw
    // /block/tip fetch is a bananablocks path and 404s on WoC
    const tip = await indexFailover(TN_INDEXES).tip()
    if (tip === undefined) throw new Error('index → tip unreachable')
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
  const shim = await stubShim()
  let b!: A, s!: A
  const results: number[] = []
  try {
    b = await mkagent('alice-b', 'alice', [f1.url, f2.url], logs, dir, undefined, shim.url)
    s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir, undefined, shim.url)
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
    await shim.close()
    await f1.close()
    await f2.close()
  }
})

test('T2: throughput — 2 concurrent buyers on one seller', { timeout: 1_800_000 }, async (t) => {
  DOMAIN = 'libfoo-tn' + RUN + '-' + dseq++
  if (!(await live(t))) return
  const dir = SUITE_DIR
  const logs: string[] = []
  const f1 = await faulthost(HOST_A)
  const f2 = await faulthost(HOST_B)
  const shim = await stubShim()
  const buyers: A[] = []
  let s!: A
  try {
    s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir, undefined, shim.url)
    await sellerize(s, 200)
    for (const [i, w] of (['alice', 'bob'] as const).entries()) buyers.push(await mkagent(`${w}-b${i}`, w, [f1.url, f2.url], logs, dir, undefined, shim.url))
    await warmup([s, ...buyers], logs)
    await drive([s.rt, ...buyers.map((b) => b.rt)], 6)
    const t0 = Date.now()
    const wants = await Promise.all(buyers.map((b) => b.rt.engine.publishWant({ domains: [DOMAIN], question: 'q?', maxSats: 450 }, b.policyHash)))
    const all = [s.rt, ...buyers.map((b) => b.rt)]
    const settledFor = async () => (await Promise.all(wants.map((w) => saleFor(s, w.wantId)))).filter((o) => o !== undefined && COMPLETE.has(o.state))
    const ms = await until(all, 400_000, async () => {
      for (const [i, b] of buyers.entries()) {
        const bo = await opFor(b, wants[i].wantId)
        if (bo === undefined || !BUYER_DONE.has(bo.state)) return false
      }
      return (await settledFor()).length >= wants.length
    })
    assert.ok(ms > 0, 'not all deals landed | ' + logs.slice(-25).join(' | '))
    const settled = await settledFor()
    assert.equal(settled.length, wants.length)
    console.log(`    throughput: ${wants.length} live deals in ${ms}ms wall`)
  } finally {
    await STOP_ALL([s?.rt, ...buyers.map((b) => b.rt)])
    await shim.close()
    await f1.close()
    await f2.close()
  }
})

test('T3: watch-shim A/B on live hosts — notify fires, deal completes', { timeout: 1_800_000 }, async (t) => {
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
      const ms = await until([b.rt, s.rt], 400_000, async () => {
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

test('T4: payload scaling — 1KB completes; 32KB hits the envelope cap honestly', { timeout: 1_800_000 }, async (t) => {
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
        const ms = await until([b.rt, s.rt], 400_000, async () => {
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
        const funded = await until([b.rt, s.rt], 400_000, async () => (await opFor(b, w.wantId))?.state === 'awaiting_disclosure')
        assert.ok(funded > 0, 'escrow never funded | ' + logs.slice(-15).join(' | '))
        await until([b.rt, s.rt], 400_000, async () => {
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

test('R1: host dies mid-deal — survivor carries it home (live)', { timeout: 1_800_000 }, async (t) => {
  DOMAIN = 'libfoo-tn' + RUN + '-' + dseq++
  if (!(await live(t))) return
  const dir = SUITE_DIR
  const logs: string[] = []
  const f1 = await faulthost(HOST_A)
  const f2 = await faulthost(HOST_B)
  const shim = await stubShim()
  let b!: A, s!: A
  try {
    b = await mkagent('alice-b', 'alice', [f1.url, f2.url], logs, dir, undefined, shim.url)
    s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir, undefined, shim.url)
    await sellerize(s, 200)
    await warmup([b, s], logs)
    await drive([b.rt, s.rt], 6)
    const w = await b.rt.engine.publishWant({ domains: [DOMAIN], question: 'q?', maxSats: 450 }, b.policyHash)
    assert.ok(
      (await until([b.rt, s.rt], 240_000, async () => (await saleFor(s, w.wantId))?.state === 'quoted')) > 0,
      'no quote | ' + logs.slice(-15).join(' | ')
    )
    f1.fault.down = true // kill host A mid-deal
    const ms = await until([b.rt, s.rt], 400_000, async () => {
      const bo = await opFor(b, w.wantId)
      const so = await saleFor(s, w.wantId)
      return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
    })
    assert.ok(ms > 0, 'deal starved without host A | ' + logs.slice(-20).join(' | '))
  } finally {
    await STOP_ALL([b?.rt, s?.rt])
    await shim.close()
    await f1.close()
    await f2.close()
  }
})

test('R2: one host delayed 3s — quorum-1 rides the fast host (live)', { timeout: 1_800_000 }, async (t) => {
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
    const ms = (await deal(b, s, logs, 400_000)).ms
    console.log(`    deal under 3s-delayed host: ${ms}ms`)
  } finally {
    await STOP_ALL([b?.rt, s?.rt])
    await f1.close()
    await f2.close()
  }
})

test('R3: withholding host loses the offer — quorum-1 + withheld bookkeeping (live)', { timeout: 1_800_000 }, async (t) => {
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
      (await until([b.rt, s.rt], 240_000, async () => (await saleFor(s, w.wantId))?.state === 'quoted')) > 0,
      'no quote | ' + logs.slice(-15).join(' | ')
    )
    // withhold everything the seller just fanned out to host A — the host
    // stays reachable but stops serving the new carriers
    f1.fault.dropLookup = true
    const ms = await until([b.rt, s.rt], 400_000, async () => {
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

test('R4: partition both hosts → deaf window → heal → completes, no double-broadcast (live)', { timeout: 1_800_000 }, async (t) => {
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
      (await until([b.rt, s.rt], 240_000, async () => (await opFor(b, w.wantId))?.state === 'awaiting_disclosure')) > 0,
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
    const ms = await until([b.rt, s.rt], 400_000, async () => {
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

test('R5: buyer killed at awaiting_disclosure → respawn → completes (live)', { timeout: 1_800_000 }, async (t) => {
  DOMAIN = 'libfoo-tn' + RUN + '-' + dseq++
  if (!(await live(t))) return
  const dir = SUITE_DIR
  const logs: string[] = []
  const f1 = await faulthost(HOST_A)
  const f2 = await faulthost(HOST_B)
  const shim = await stubShim()
  let b!: A, s!: A, b2!: A
  try {
    b = await mkagent('alice-b', 'alice', [f1.url, f2.url], logs, dir, undefined, shim.url)
    s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir, undefined, shim.url)
    await sellerize(s, 200)
    await warmup([b, s], logs)
    await drive([b.rt, s.rt], 6)
    const w = await b.rt.engine.publishWant({ domains: [DOMAIN], question: 'q?', maxSats: 450 }, b.policyHash)
    assert.ok(
      (await until([b.rt, s.rt], 240_000, async () => (await opFor(b, w.wantId))?.state === 'awaiting_disclosure')) > 0,
      'never funded | ' + logs.slice(-15).join(' | ')
    )
    const opId = (await opFor(b, w.wantId))!.operationId
    await b.rt.stop() // kill -9 equivalent — store persists, respawn resumes
    b2 = await respawn(b, [f1.url, f2.url], logs, shim.url)
    const ms = await until([b2.rt, s.rt], 400_000, async () => {
      const bo = (await buyerOps(b2)).find((o) => o.operationId === opId)
      const so = await saleFor(s, w.wantId)
      return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
    })
    assert.ok(ms > 0, 'respawned buyer never resumed | ' + logs.slice(-20).join(' | '))
    const bo = (await buyerOps(b2)).find((o) => o.operationId === opId)
    assert.ok(bo !== undefined && BUYER_DONE.has(bo.state), `op ${opId} ended ${bo?.state}`)
  } finally {
    await STOP_ALL([b?.rt, b2?.rt, s?.rt])
    await shim.close()
    await f1.close()
    await f2.close()
  }
})

test('R6: duplicate lookup outputs — deal completes once (live)', { timeout: 1_800_000 }, async (t) => {
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
    const dupes = (await buyerOps(b)).filter((o) => (o.data.want as { want?: { wantId?: string } } | undefined)?.want?.wantId === r.wantId)
    assert.equal(dupes.length, 1, 'duplicate outputs spawned extra ops')
  } finally {
    await STOP_ALL([b?.rt, s?.rt])
    await f1.close()
    await f2.close()
  }
})

test('R7: late escrow rejection → funding_failed via txFacts verdict (live)', { timeout: 1_800_000 }, async (t) => {
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
      (await until([b.rt, s.rt], 240_000, async () => (await opFor(b, w.wantId))?.state === 'awaiting_disclosure')) > 0,
      'escrow never funded | ' + logs.slice(-15).join(' | ')
    )
    const escrowTxid = (await opFor(b, w.wantId))!.data.escrowTxid as string
    const orig = b.mem.txFacts.bind(b.mem)
    ;(b.mem as unknown as { txFacts: (txid: string) => Promise<import('@nektar/chain').TxFacts> }).txFacts = async (txid: string) =>
      txid === escrowTxid ? { verdict: 'dead', source: 'first-party' } : orig(txid)
    const ms = await until([b.rt], 240_000, async () => (await opFor(b, w.wantId))?.state === 'funding_failed')
    assert.ok(ms > 0, 'rejected funding did not land funding_failed | ' + logs.slice(-20).join(' | '))
    const bo = (await opFor(b, w.wantId))!
    const evts = await b.rt.store.events(bo.operationId)
    assert.ok(
      evts.some((e) => e.cause === 'escrow_rejected_late'),
      `expected escrow_rejected_late, got ${evts.map((e) => e.cause).join(',')}`
    )
    ;(b.mem as unknown as { txFacts: (txid: string) => Promise<import('@nektar/chain').TxFacts> }).txFacts = orig
    await drive([b.rt], 3)
    assert.equal((await opFor(b, w.wantId))!.state, 'funding_failed', 'rejected funding retried itself back to life')
  } finally {
    await STOP_ALL([b?.rt, s?.rt])
    await f1.close()
    await f2.close()
  }
})

test('R8: concurrency invariant — 2 buyers race, all terminal, distinct settlements (live)', { timeout: 1_800_000 }, async (t) => {
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
    // refundBlocks=1 exercises the real height gate on one block; a matching
    // sellerSafetyBlocks=0 keeps refund > safety so the config is consistent
    // (0-conf settlement for the seller, refund unlocks at +1)
    s = await mkagent('ben-s', 'ben', [f1.url, f2.url], logs, dir, { refundBlocks: 1, sellerSafetyBlocks: 0, directAccept: true })
    await sellerize(s, 200)
    be = await mkagent('alice-e', 'alice', [f1.url, f2.url], logs, dir, { refundBlocks: 1, sellerSafetyBlocks: 0 })
    bd = await mkagent('bob-d', 'bob', [f1.url, f2.url], logs, dir, { refundBlocks: 1, sellerSafetyBlocks: 0, directMaxSats: 500 })
    await warmup([be, bd, s], logs)
    await drive([s.rt, be.rt, bd.rt], 6)
    const wE = await be.rt.engine.publishWant({ domains: [DOMAIN], question: 'escrow?', maxSats: 450 }, be.policyHash)
    const wD = await bd.rt.engine.publishWant({ domains: [DOMAIN], question: 'direct?', maxSats: 450 }, bd.policyHash)
    const all = [s.rt, be.rt, bd.rt]
    // wait until BOTH sale ops are quoted, then ghost the seller — accepts it
    // never reads can never be delivered
    assert.ok(
      (await until(all, 240_000, async () => (await saleFor(s, wE.wantId))?.state === 'quoted' && (await saleFor(s, wD.wantId))?.state === 'quoted')) > 0,
      'not both quoted | ' + logs.slice(-15).join(' | ')
    )
    await s.rt.stop()
    assert.ok(
      (await until(
        [be.rt, bd.rt],
        400_000,
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
