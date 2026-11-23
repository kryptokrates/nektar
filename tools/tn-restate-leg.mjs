#!/usr/bin/env node
// Gate-1 Restate leg: one real testnet deal where the buyer's durable legs
// run through a REAL Restate sidecar — the production executor shape.
//
//   node tools/tn-restate-leg.mjs
//
// One process, two warmed tn-suite agents:
//   ben   — seller, in-process Dispatcher (control: the deal does not need
//           every leg on restate; the buyer's side IS the proof)
//   alice — buyer, NO cfg.exec → openAutomation takes the production path:
//           openEndpoint() binds NEKTAR_RESTATE_ENDPOINT_PORT and registers
//           NEKTAR_RESTATE_ENDPOINT_URI with NEKTAR_RESTATE_ADMIN; every
//           emitTxEvent/custody/deal send round-trips the sidecar ingress at
//           NEKTAR_RESTATE_URL. A completed deal is the cutover proof — a dead
//           or unregistered ingress 500s every durable send and the want
//           starves in `collecting`.
//
// Prereqs:
//   - `node tools/tn-warmup.mjs` done — both agents' lane stock proven
//     (the leg reuses .state/tn-suite/{alice,ben} and REFUSES while the
//     warmer holds the key locks).
//   - a restate container with the host-gateway mapping:
//     docker run -d --name nektar-restate-test --add-host host.docker.internal:host-gateway \
//       -p 28080:8080 -p 29070:9070 docker.restate.dev/restatedev/restate:latest
//
// Env (defaults match the dev sidecar + the 28xxx host pair):
//   NEKTAR_TN_HOST_A/B          http://127.0.0.1:28291 / :28292
//   NEKTAR_RESTATE_URL          http://127.0.0.1:28080   (ingress)
//   NEKTAR_RESTATE_ADMIN        http://127.0.0.1:29070   (admin API)
//   NEKTAR_RESTATE_ENDPOINT_PORT 9080  (bind on this host)
//   NEKTAR_RESTATE_ENDPOINT_URI  http://host.docker.internal:9080 (docker → host)
//   TN_BUYER / TN_SELLER        alice / ben
import { existsSync, mkdirSync, copyFileSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const ROOT = fileURLToPath(new URL('..', import.meta.url))
const dist = (p) => pathToFileURL(join(ROOT, p)).href
const { PrivateKey } = await import(dist('packages/mcp/node_modules/@bsv/sdk/dist/esm/mod.js'))
const { indexFor, livePort, reconcileLiveReserve, ReserveFunder, fundsOf } = await import(dist('packages/chain/dist/src/index.js'))
const { NektarMemory } = await import(dist('packages/mcp/dist/src/memory.js'))
const { acquireLock, keyLockFile } = await import(dist('packages/mcp/dist/src/index.js'))
const { callTool } = await import(dist('packages/mcp/dist/src/dispatch/index.js'))
const { openAutomation, ScriptedExecutor, DEFAULT_SCORE_WEIGHTS, Dispatcher } = await import(dist('packages/automation/dist/src/index.js'))
const { nap: sleep } = await import(dist('packages/core/dist/src/index.js'))

// paid disclosure is opt-in — the suite sets the same default
process.env.NEKTAR_PAID_DISCLOSURE ??= '1'
process.env.NEKTAR_RESTATE_URL ??= 'http://127.0.0.1:28080'
process.env.NEKTAR_RESTATE_ADMIN ??= 'http://127.0.0.1:29070'
process.env.NEKTAR_RESTATE_ENDPOINT_PORT ??= '9080'
process.env.NEKTAR_RESTATE_ENDPOINT_URI ??= 'http://host.docker.internal:9080'

const STATE = join(ROOT, '.state/testnet-agents')
const SUITE_DIR = join(ROOT, '.state/tn-suite')
const KEYS_FILE = join(STATE, 'agent-keys.json')
const HOSTS = [process.env.NEKTAR_TN_HOST_A ?? 'http://127.0.0.1:28291', process.env.NEKTAR_TN_HOST_B ?? 'http://127.0.0.1:28292']
const ARC = process.env.NEKTAR_TN_ARC ?? 'https://arcade-v2-testnet-us-1.bsvblockchain.tech'
const INDEX = process.env.NEKTAR_TN_INDEX ?? 'https://test.bananablocks.com/api/v1'
const BUYER = process.env.TN_BUYER ?? 'alice'
const SELLER = process.env.TN_SELLER ?? 'ben'
const FINDING = 'breaking change: libfoo 3.0 renamed connect() to open(); replace connect( with open( to fix the build'
const DOMAIN = `rtleg${Date.now().toString(36)}` // fresh domain — stale host records never win collection
const COLLECT_MS = 45_000
const WIF = existsSync(KEYS_FILE) ? JSON.parse(readFileSync(KEYS_FILE, 'utf8')) : {}
if (WIF[BUYER] === undefined || WIF[SELLER] === undefined) {
  console.error(`no suite WIFs for ${BUYER}/${SELLER} — expected ${KEYS_FILE}`)
  process.exit(1)
}
const logs = []
const mkagent = async (wifName, restate) => {
  const dataDir = join(SUITE_DIR, wifName)
  mkdirSync(dataDir, { recursive: true })
  const fundKey = PrivateKey.fromWif(WIF[wifName])
  const pkhHex = Buffer.from(fundKey.toPublicKey().toHash()).toString('hex')
  const keyRelease = await acquireLock(keyLockFile(pkhHex), `fund key ${wifName}`)
  const reserveFile = join(dataDir, 'reserve.json')
  const src = join(STATE, wifName, 'reserve.json')
  if (!existsSync(reserveFile) && existsSync(src)) copyFileSync(src, reserveFile)
  const reserve = new ReserveFunder(fundKey, reserveFile)
  const addr = fundKey.toAddress([0x6f])
  const mc = { broadcasterUrl: ARC, indexerUrl: INDEX }
  const port = await livePort(mc, 'testnet', {
    dataDir,
    log: (l) => logs.push(`${wifName}:${l}`),
    valueOf: (txid, vout) => reserve.utxos.find((u) => u.tx.id('hex') === txid && u.vout === vout)?.satoshis
  })
  const spendStatus = port.spendStatus ?? (() => 'unknown')
  const rec = await reconcileLiveReserve(reserve, addr, mc, spendStatus)
  console.log(`${wifName}: reconcile ${JSON.stringify(rec)}`)
  const index = indexFor({ url: INDEX })
  const chain = {
    broadcast: port.broadcast,
    currentHeight: () => port.height(),
    isValidRootForHeight: (r, h) => port.tracker.isValidRootForHeight(r, h),
    headerAtHeight: (h) => port.tracker.headerAtHeight(h),
    tipHeight: () => port.height()
  }
  const mem = new NektarMemory(chain, reserve, fundKey, {
    network: 'testnet',
    modelId: 'tn-restate-leg',
    feeRateSatPerKb: 1,
    statusHint: port.statusHint,
    batchPolicy: { maxLeaves: 1 },
    funds: fundsOf(reserve, {
      pkhHex,
      spendCheck: async (txid, vout) => (await spendStatus(txid, vout)) === 'spent',
      liveSet: async () => {
        const live = await index.utxos(addr).catch(() => undefined)
        return live === undefined ? undefined : new Set(live.map((u) => `${u.txid}:${u.vout}`))
      },
      proofOf: async (tx) => {
        const mp = await port.proofLookup?.(tx.id('hex'))
        if (mp !== undefined) tx.merklePath = mp
      }
    })
  })
  if (port.proofLookup !== undefined) mem.proofLookup = port.proofLookup
  if (port.txLookup !== undefined) mem.txLookup = port.txLookup
  const pub = mem.operatorKey().toPublicKey().toString()
  const rt = await openAutomation(mem, {
    network: 'testnet',
    localHosts: true,
    dataDir,
    marketHosts: HOSTS,
    intervalMs: 0,
    // restate agents take the production path; local ones the in-process
    // dispatcher (same handler bodies, no endpoint)
    ...(restate ? {} : { exec: new Dispatcher() }),
    call: (t, a) => callTool(mem, t, a),
    log: (l) => {
      const line = `${wifName}:${l}`
      logs.push(line)
      if (process.env.TN_VERBOSE === '1') console.error(line)
    },
    ai: new ScriptedExecutor(),
    publishFinger: true,
    opts: { offerTtlBlocks: 48, collectMs: COLLECT_MS }
  })
  mem.automation = rt
  const stopRt = rt.stop.bind(rt)
  rt.stop = async () => {
    await stopRt()
    await keyRelease()
  }
  const policyHash = await rt.store.approvePolicy(
    {
      v: 1,
      policyId: 'testnet-restate-leg',
      network: 'testnet',
      agentPub: pub,
      roles: ['buyer', 'seller'],
      notBefore: 0,
      notAfter: 4_000_000_000_000,
      domains: [DOMAIN],
      limits: {
        perPurchaseSats: 500,
        dailyPurchaseSats: 50_000,
        perTxFeeSats: 1200,
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
    },
    'test:operator'
  )
  return { mem, rt, pub, policyHash, name: wifName }
}

const COMPLETE = new Set(['completion_submitted', 'settled', 'useful'])
const BUYER_DONE = new Set(['disclosure_verified', 'useful'])
const until = async (rts, ms, ok) => {
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

console.log(`restate leg: buyer=${BUYER} (restate) seller=${SELLER} (local) domain=${DOMAIN}`)
console.log(`  ingress ${process.env.NEKTAR_RESTATE_URL} admin ${process.env.NEKTAR_RESTATE_ADMIN} endpoint ${process.env.NEKTAR_RESTATE_ENDPOINT_URI}`)
// seller first — the offer must exist before the want's collect window opens
const s = await mkagent(SELLER, false)
const b = await mkagent(BUYER, true)
await s.mem.remember('guide', FINDING)
await s.mem.describeShard('guide', { domains: [DOMAIN], offers: [{ kind: 'record', priceSats: 300 }] })
for (const l of s.mem.anchorLinks('guide')) s.mem.confirmAnchor('guide', l.txid)
await s.rt.engine.enableSeller(s.policyHash)
console.log('seller ready — publishing want')
const w = await b.rt.engine.publishWant({ domains: [DOMAIN], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
const elapsed = await until([b.rt, s.rt], 600_000, async () => {
  const bo = (await b.rt.store.operations({ role: 'buyer' })).find((o) => o.data.want?.want?.wantId === w.wantId)
  const so = (await s.rt.store.operations({ role: 'seller' })).find((o) => o.data.want?.want?.wantId === w.wantId)
  return bo !== undefined && BUYER_DONE.has(bo.state) && so !== undefined && COMPLETE.has(so.state)
})
if (elapsed < 0) {
  console.error('deal never completed | ' + logs.slice(-25).join(' | '))
  await Promise.all([b.rt.stop(), s.rt.stop()])
  process.exit(1)
}
const bo = (await b.rt.store.operations({ role: 'buyer' })).find((o) => o.data.want?.want?.wantId === w.wantId)
const so = (await s.rt.store.operations({ role: 'seller' })).find((o) => o.data.want?.want?.wantId === w.wantId)
console.log(`deal complete in ${elapsed}ms — buyer ${bo.state} seller ${so.state} wantId ${w.wantId}`)
// admin-side evidence: the sidecar journaled invocations for this endpoint's
// services — the completed deal through them is the cutover proof
try {
  const r = await fetch(`${process.env.NEKTAR_RESTATE_ADMIN}/services`, { signal: AbortSignal.timeout(5_000) })
  const svcs = await r.json()
  const ours = (svcs.services ?? svcs ?? []).filter((x) => (x.name ?? '').startsWith('nektar-'))
  console.log(`sidecar serves ${ours.length} nektar services: ${ours.map((x) => x.name).join(', ')}`)
} catch (e) {
  console.log(`admin introspection skipped: ${e.message}`)
}
await Promise.all([b.rt.stop(), s.rt.stop()])
console.log('restate leg: PASS')
