#!/usr/bin/env node
// Standalone lane-stock warmer for packages/mcp/test/market-testnet.test.ts.
//
// The suite's one real block dependency: deal-path lanes (market/bbox/finger)
// need >=8 proven free slots — proven means the funding tree's root is mined.
// lanes.sqlite persists under .state/tn-suite/<wif>, so trees mint+prove ONCE
// and every later run skips the wait. This tool owns that wait OUTSIDE the
// test budget: run it any time, leave it until ready, then run the suite.
//
//   node tools/tn-warmup.mjs                 # warm all three suite WIFs
//   node tools/tn-warmup.mjs alice ben       # subset
//   NEKTAR_TN_HOST_A=http://127.0.0.1:28291 NEKTAR_TN_HOST_B=http://127.0.0.1:28292 node tools/tn-warmup.mjs
//
// Exit 0 when every requested agent reports proven stock; exit 2 on the bound
// (default 90 min — testnet mining is the variable, not the code). The suite's
// own warmup loop stays available behind TN_WARMUP=1.
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
const { openAutomation, ScriptedExecutor, DEFAULT_SCORE_WEIGHTS } = await import(dist('packages/automation/dist/src/index.js'))

const STATE = join(ROOT, '.state/testnet-agents')
const SUITE_DIR = join(ROOT, '.state/tn-suite')
const KEYS_FILE = join(STATE, 'agent-keys.json')
const HOSTS = [process.env.NEKTAR_TN_HOST_A ?? 'http://127.0.0.1:18291', process.env.NEKTAR_TN_HOST_B ?? 'http://127.0.0.1:18292']
const ARC = process.env.NEKTAR_TN_ARC ?? 'https://arcade-v2-testnet-us-1.bsvblockchain.tech'
const INDEX = process.env.NEKTAR_TN_INDEX ?? 'https://test.bananablocks.com/api/v1'
const BOUND = Number(process.env.TN_WARMUP_MS ?? 90 * 60_000)
const FLOOR = Number(process.env.TN_TREE_FLOOR ?? 8)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const WIF = existsSync(KEYS_FILE) ? JSON.parse(readFileSync(KEYS_FILE, 'utf8')) : {}
const names = (process.argv.slice(2).filter((a) => !a.startsWith('-')).length > 0 ? process.argv.slice(2) : ['alice', 'ben', 'bob']).filter((n) => WIF[n] !== undefined)
if (names.length === 0) {
  console.error(`no suite WIFs — expected ${KEYS_FILE} with alice/ben/bob`)
  process.exit(1)
}

const treesReady = async (rt) => {
  const s = await rt.laneStock()
  return ['market', 'bbox', 'finger'].every((k) => (s[k]?.proven ?? 0) >= FLOOR)
}

const mkagent = async (wifName) => {
  const wif = WIF[wifName]
  const dataDir = join(SUITE_DIR, wifName)
  mkdirSync(dataDir, { recursive: true })
  const fundKey = PrivateKey.fromWif(wif)
  const pkhHex = Buffer.from(fundKey.toPublicKey().toHash()).toString('hex')
  const keyRelease = await acquireLock(keyLockFile(pkhHex), `fund key ${wifName}`)
  const reserveFile = join(dataDir, 'reserve.json')
  const src = join(STATE, wifName, 'reserve.json')
  if (!existsSync(reserveFile) && existsSync(src)) copyFileSync(src, reserveFile)
  const reserve = new ReserveFunder(fundKey, reserveFile)
  const addr = fundKey.toAddress([0x6f])
  const mc = { broadcasterUrl: ARC, indexerUrl: INDEX }
  const port = await livePort(mc, 'testnet', { dataDir, log: (l) => console.log(`${wifName}:${l}`), valueOf: (txid, vout) => reserve.utxos.find((u) => u.tx.id('hex') === txid && u.vout === vout)?.satoshis })
  const spendStatus = port.spendStatus ?? (async () => 'unknown')
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
  const mem = new NektarMemory(chain, reserve, PrivateKey.fromWif(wif), {
    network: 'testnet',
    modelId: 'tn-warmup',
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
  const rt = await openAutomation(mem, {
    network: 'testnet',
    localHosts: true,
    dataDir,
    marketHosts: HOSTS,
    intervalMs: 0,
    call: (t, a) => callTool(mem, t, a),
    log: (l) => console.log(`${wifName}:${l}`),
    ai: new ScriptedExecutor(),
    publishFinger: true,
    opts: { offerTtlBlocks: 48 }
  })
  mem.automation = rt
  const pub = mem.operatorKey().toPublicKey().toString()
  await rt.store.approvePolicy(
    {
      v: 1, policyId: 'tn-warmup', network: 'testnet', agentPub: pub, roles: ['buyer', 'seller'],
      notBefore: 0, notAfter: 4_000_000_000_000, domains: ['tn-warmup'],
      limits: { perPurchaseSats: 500, dailyPurchaseSats: 50_000, perTxFeeSats: 1200, dailyFeeSats: 50_000, runTotalSats: 500_000, maxLockedSats: 5000, maxUnconfirmedSats: 5000, maxUnconfirmedCount: 16 },
      scoring: { minimumScoreBps: 0, weights: { ...DEFAULT_SCORE_WEIGHTS }, collectMs: 800 },
      seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: 5000, sellableShards: ['guide'] },
      disclosure: { allowPublic: false },
      ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 200, timeoutMs: 5000 },
      sandbox: { mode: 'sandbox-only' }
    },
    'tn-warmup'
  )
  return { name: wifName, rt, stop: async () => { await rt.stop(); await keyRelease() } }
}

const agents = []
try {
  for (const n of names) agents.push(await mkagent(n))
  const t0 = Date.now()
  for (;;) {
    const rows = []
    let ready = true
    for (const a of agents) {
      const s = await a.rt.laneStock()
      const ok = await treesReady(a.rt)
      if (!ok) ready = false
      rows.push(`${a.name} ${['market', 'bbox', 'finger'].map((k) => `${k}:${s[k]?.proven ?? 0}p/${s[k]?.taken ?? 0}t`).join(' ')}`)
    }
    console.log(`[${new Date().toISOString().slice(11, 19)}]`, rows.join(' | '))
    if (ready) {
      console.log(`tn-warmup: all agents have >=${FLOOR} proven slots on market/bbox/finger — suite-ready`)
      process.exitCode = 0
      break
    }
    if (Date.now() - t0 > BOUND) {
      console.error(`tn-warmup: bound ${BOUND}ms hit with stock still unproven — testnet mining is the variable; rerun to continue`)
      process.exitCode = 2
      break
    }
    await Promise.all(agents.map((a) => a.rt.upkeep()))
    await sleep(15_000)
  }
} finally {
  for (const a of agents) await a.stop().catch(() => {})
}
