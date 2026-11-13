// diagnostic: one testnet agent, a few cycles, full logs — surfaces the
// earliest ensureTrees/broadcast/submit failures that test tails truncate.
import { mkdtempSync, mkdirSync, copyFileSync, existsSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { PrivateKey, Transaction } from '@bsv/sdk'
import { ChaintracksTracker, WocTracker, BsvBroadcaster, fetchTxStatus, ReserveFunder } from '@nektar/chain'
import { NektarMemory } from './dist/src/memory.js'
import { callTool } from './dist/src/server.js'
import { openAutomation, ScriptedExecutor, DEFAULT_SCORE_WEIGHTS } from '@nektar/automation'

const ARC = 'https://arcade-v2-testnet-us-1.bsvblockchain.tech'
const WOC = 'https://api.whatsonchain.com/v1/bsv/test'
const HOSTS = ['http://127.0.0.1:18291', 'http://127.0.0.1:18292']
const WIF = 'cRJ9mNASGGJ4kTAf2HhN8CjPkbCwJ6xDiWp5DnvL9xveuM9PtuiF' // ben

const dir = mkdtempSync(join(tmpdir(), 'tn-probe-'))
const dataDir = join(dir, 'ben-s')
mkdirSync(dataDir, { recursive: true })
const src = join(process.cwd(), '../../.state/testnet-agents/ben/reserve.json')
if (existsSync(src)) copyFileSync(src, join(dataDir, 'reserve.json'))

const mc = { broadcasterUrl: ARC, wocUrl: WOC }
const tracker = new ChaintracksTracker(mc, `${ARC}/chaintracks/v2`, new WocTracker(mc))
const bcast = new BsvBroadcaster(mc)
const chain = {
  broadcast: (tx) => bcast.broadcast(tx),
  currentHeight: () => tracker.currentHeight(),
  isValidRootForHeight: (r, h) => tracker.isValidRootForHeight(r, h),
  headerAtHeight: (h) => tracker.headerAtHeight(h),
  tipHeight: () => tracker.currentHeight()
}
const fundKey = PrivateKey.fromWif(WIF)
const reserve = new ReserveFunder(fundKey, join(dataDir, 'reserve.json'))
const mem = new NektarMemory(chain, reserve, fundKey, {
  network: 'testnet',
  modelId: 'test-model-1',
  feeRateSatPerKb: 1,
  txStatus: (txid) => fetchTxStatus(mc, txid, { wocFallback: true }),
  batchPolicy: { maxLeaves: 1 }
})
const logs = []
const rt = await openAutomation(mem, {
  network: 'testnet',
  localHosts: true,
  dataDir,
  marketHosts: HOSTS,
  intervalMs: 0,
  call: (t, a) => callTool(mem, t, a),
  log: (l) => { logs.push(l); console.log('LOG', l) },
  ai: new ScriptedExecutor(),
  publishFinger: true,
  opts: { offerTtlBlocks: 48, refundBlocks: 1 }
})
mem.automation = rt
const pub = mem.operatorKey().toPublicKey().toString()
const policyHash = await rt.store.approvePolicy({
  v: 1, policyId: 'probe', network: 'testnet', agentPub: pub,
  roles: ['buyer', 'seller'], notBefore: 0, notAfter: 4_000_000_000_000,
  domains: ['libfoo'],
  limits: { perPurchaseSats: 500, dailyPurchaseSats: 50_000, perTxFeeSats: 600, dailyFeeSats: 50_000, runTotalSats: 500_000, maxLockedSats: 5000, maxUnconfirmedSats: 5000, maxUnconfirmedCount: 16 },
  scoring: { minimumScoreBps: 0, weights: { ...DEFAULT_SCORE_WEIGHTS }, collectMs: 15_000 },
  seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: 50, sellableShards: ['guide'] },
  disclosure: { allowPublic: false },
  ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 200, timeoutMs: 5000 },
  sandbox: { mode: 'sandbox-only' }
}, 'test:operator')

console.log('--- sellerize ---')
try {
  await mem.remember('guide', 'libfoo 3.0 renamed connect() to open()')
  await mem.describeShard('guide', { domains: ['libfoo'], offers: [{ kind: 'record', priceSats: 200 }] })
  for (const l of mem.anchorLinks('guide')) mem.confirmAnchor('guide', l.txid)
  await rt.engine.enableSeller(policyHash)
} catch (e) { console.log('SELLERIZE FAIL', e) }
console.log('--- 8 cycles ---')
for (let i = 0; i < 8; i++) {
  try { await rt.cycle() } catch (e) { console.log(`CYCLE ${i} FAIL`, (e).message) }
}
console.log('--- done; last logs ---')
for (const l of logs.slice(-30)) console.log(' ', l)
await rt.stop()
