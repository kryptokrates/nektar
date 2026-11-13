// probe2: buyer publishes a want; watch every log + op state + lane trees
import { mkdtempSync, mkdirSync, copyFileSync, existsSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { PrivateKey } from '@bsv/sdk'
import { ChaintracksTracker, WocTracker, BsvBroadcaster, fetchTxStatus, ReserveFunder } from '@nektar/chain'
import { NektarMemory } from './dist/src/memory.js'
import { callTool } from './dist/src/server.js'
import { openAutomation, ScriptedExecutor, DEFAULT_SCORE_WEIGHTS } from '@nektar/automation'
import { execSync } from 'node:child_process'

const ARC = 'https://arcade-v2-testnet-us-1.bsvblockchain.tech'
const WOC = 'https://api.whatsonchain.com/v1/bsv/test'
const HOSTS = ['http://127.0.0.1:18291', 'http://127.0.0.1:18292']

const mk = async (wif, name, dir) => {
  const dataDir = join(dir, name)
  mkdirSync(dataDir, { recursive: true })
  const src = join(process.cwd(), `../../.state/testnet-agents/${wif}/reserve.json`)
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
  const fundKey = PrivateKey.fromWif(WIFS[wif])
  const reserve = new ReserveFunder(fundKey, join(dataDir, 'reserve.json'))
  const mem = new NektarMemory(chain, reserve, fundKey, {
    network: 'testnet', modelId: 'm', feeRateSatPerKb: 1,
    txStatus: (txid) => fetchTxStatus(mc, txid, { wocFallback: true }),
    batchPolicy: { maxLeaves: 1 }
  })
  const logs = []
  const rt = await openAutomation(mem, {
    network: 'testnet', localHosts: true, dataDir, marketHosts: HOSTS, intervalMs: 0,
    call: (t, a) => callTool(mem, t, a),
    log: (l) => { logs.push(`${name}:${l}`); console.log(`LOG ${name}: ${l}`) },
    ai: new ScriptedExecutor(), publishFinger: true,
    opts: { offerTtlBlocks: 48, refundBlocks: 1 }
  })
  mem.automation = rt
  const pub = mem.operatorKey().toPublicKey().toString()
  const policyHash = await rt.store.approvePolicy({
    v: 1, policyId: 'probe', network: 'testnet', agentPub: pub,
    roles: ['buyer', 'seller'], notBefore: 0, notAfter: 4_000_000_000_000, domains: ['libfoo'],
    limits: { perPurchaseSats: 500, dailyPurchaseSats: 50_000, perTxFeeSats: 600, dailyFeeSats: 50_000, runTotalSats: 500_000, maxLockedSats: 5000, maxUnconfirmedSats: 5000, maxUnconfirmedCount: 16 },
    scoring: { minimumScoreBps: 0, weights: { ...DEFAULT_SCORE_WEIGHTS }, collectMs: 8_000 },
    seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: 50, sellableShards: ['guide'] },
    disclosure: { allowPublic: false },
    ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 200, timeoutMs: 5000 },
    sandbox: { mode: 'sandbox-only' }
  }, 'test:operator')
  return { mem, rt, pub, policyHash, logs, name }
}

const WIFS = {
  alice: 'cPfqX3j79L1QxVVv77C34Caq2hdRoUA61T5jfFZDGp1syWBNxd37',
  ben: 'cRJ9mNASGGJ4kTAf2HhN8CjPkbCwJ6xDiWp5DnvL9xveuM9PtuiF'
}

const dir = mkdtempSync(join(tmpdir(), 'tn-probe2-'))
const b = await mk('alice', 'alice-b', dir)
const s = await mk('ben', 'ben-s', dir)

await s.mem.remember('guide', 'libfoo 3.0 renamed connect() to open()')
await s.mem.describeShard('guide', { domains: ['libfoo'], offers: [{ kind: 'record', priceSats: 200 }] })
for (const l of s.mem.anchorLinks('guide')) s.mem.confirmAnchor('guide', l.txid)
await s.rt.engine.enableSeller(s.policyHash)

console.log('--- publish want ---')
const w = await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'q?', maxSats: 450 }, b.policyHash)
console.log('wantId', w.wantId)

for (let i = 0; i < 20; i++) {
  await Promise.all([b.rt.cycle(), s.rt.cycle()])
  await new Promise((r) => setTimeout(r, 1200))
  if (i % 5 === 4) {
    const ops = await b.rt.store.operations({ role: 'buyer' })
    const sops = await s.rt.store.operations({ role: 'seller' })
    console.log(`i=${i} buyerOps=${ops.map((o) => `${o.kind}:${o.state}`).join(',') || 'none'} sellerOps=${sops.map((o) => `${o.kind}:${o.state}`).join(',') || 'none'}`)
    // lane trees?
    try {
      const out = execSync(`sqlite3 "${join(dir, 'alice-b', 'lanes.sqlite')}" "select kind, count(*) from trees group by kind"`, { stdio: 'pipe' }).toString()
      console.log('  alice trees:', out.trim() || 'none')
    } catch { console.log('  (no sqlite3 cli)') }
  }
}
console.log('--- tail logs ---')
for (const l of [...b.logs, ...s.logs].slice(-25)) console.log(' ', l)
await b.rt.stop(); await s.rt.stop()
