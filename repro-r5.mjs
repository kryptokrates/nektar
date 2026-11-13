// repro-r5: kill buyer at 'collecting', respawn, watch op states. ~90s max.
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { stackChain, stackTreasury, StackFunder } from './packages/chain/dist/src/index.js'
import { minihost } from './packages/bstack/dist/src/testing.js'
import { NektarMemory } from './packages/mcp/dist/src/memory.js'
import { callTool } from './packages/mcp/dist/src/server.js'
import { openAutomation, ScriptedExecutor, DEFAULT_SCORE_WEIGHTS } from './packages/automation/dist/src/index.js'

process.env.NEKTAR_PAID_DISCLOSURE = '1'
const NET = 'regtest'
const OFFICE = 'nektar_abcdefghij'
let clock = Date.now()
const FINDING = 'x: libfoo 3.0 renamed connect() to open()'
const logs = []

const policyFor = (pub) => ({
  v: 1, policyId: 'demo', network: NET, agentPub: pub, roles: ['buyer', 'seller'],
  notBefore: 0, notAfter: 4e12, domains: ['libfoo'],
  limits: { perPurchaseSats: 500, dailyPurchaseSats: 50000, perTxFeeSats: 600, dailyFeeSats: 50000, runTotalSats: 500000, maxLockedSats: 5000, maxUnconfirmedSats: 5000, maxUnconfirmedCount: 16 },
  scoring: { minimumScoreBps: 0, weights: { ...DEFAULT_SCORE_WEIGHTS }, collectMs: 2500 },
  seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: 50, sellableShards: ['guide'] },
  disclosure: { allowPublic: false },
  ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 200, timeoutMs: 5000 },
  sandbox: { mode: 'sandbox-only' }
})
const trackerFor = (chain) => ({ isValidRootForHeight: (r, h) => chain.isValidRootForHeight(r, h), currentHeight: () => chain.currentHeight() })
const mk = async (name, dir, hosts, chain) => {
  const funder = await StackFunder.fund(await stackTreasury(), 60)
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 1 }, modelId: 'm' })
  const pub = mem.operatorKey().toPublicKey().toString()
  const rt = await openAutomation(mem, {
    network: NET, dataDir: join(dir, name), marketHosts: hosts, intervalMs: 0,
    call: (t, a) => callTool(mem, t, a), log: (l) => logs.push(`${name}:${l}`),
    ai: new ScriptedExecutor(), now: () => clock, publishFinger: true,
    opts: { offerTtlBlocks: 10000, refundBlocks: 10000 }
  })
  mem.automation = rt
  return { mem, rt, pub, policyHash: await rt.store.approvePolicy(policyFor(pub), 'op'), name }
}
const respawn = async (a, dir, hosts) => {
  const rt = await openAutomation(a.mem, {
    network: NET, dataDir: join(dir, a.name), marketHosts: hosts, intervalMs: 0,
    call: (t, args) => callTool(a.mem, t, args), log: (l) => logs.push(`${a.name}:${l}`),
    ai: new ScriptedExecutor(), now: () => clock, publishFinger: true,
    opts: { offerTtlBlocks: 10000, refundBlocks: 10000 }
  })
  a.mem.automation = rt
  return { mem: a.mem, rt, pub: a.pub, policyHash: a.policyHash, name: a.name }
}
const drive = async (rts, chain, n = 1) => {
  for (let i = 0; i < n; i++) {
    clock += 500
    await Promise.all(rts.map((r) => r.cycle()))
    if ((await chain.pendingBroadcasts()) > 0) await chain.mine(1)
  }
}
const ops = async (a, role, kind) => (await a.rt.store.operations({ role })).filter((o) => o.kind === kind)
const dump = async (tag, b, s) => {
  const bo = (await ops(b, 'buyer', 'purchase')).map((o) => `${o.state}${o.lastError ? ` err=${o.lastError.slice(0, 80)}` : ''} dueAt=${o.dueAt} rev=${o.revision}`)
  const so = (await ops(s, 'seller', 'sale')).map((o) => `${o.state}${o.lastError ? ` err=${o.lastError.slice(0, 80)}` : ''}`)
  console.log(`--- ${tag} t=${clock} buyer=[${bo.join(', ')}] seller=[${so.join(', ')}]`)
}

const t0 = Date.now()
const t = { after() { } }
const dir = mkdtempSync(join(tmpdir(), 'r5-'))
const chain = await stackChain({ afterEach: async () => { } })
const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
let b = await mk('buyer', dir, [h1.url, h2.url], chain)
const s = await mk('seller', dir, [h1.url, h2.url], chain)
await s.mem.remember('guide', FINDING)
await s.mem.describeShard('guide', { domains: ['libfoo'], offers: [{ kind: 'record', priceSats: 200 }] })
for (const l of s.mem.anchorLinks('guide')) s.mem.confirmAnchor('guide', l.txid)
await s.rt.engine.enableSeller(s.policyHash)
await drive([b.rt, s.rt], chain, 8)
await dump('armed', b, s)
const w = await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'q?', maxSats: 450 }, b.policyHash)
for (let i = 0; i < 30; i++) {
  await drive([b.rt, s.rt], chain, 1)
  const st = (await ops(b, 'buyer', 'purchase'))[0]?.state
  if (st === 'collecting') break
}
await dump('collecting?', b, s)
await b.rt.stop()
const b2 = await respawn(b, dir, [h1.url, h2.url])
await dump('respawned', b2, s)
for (let i = 0; i < 40; i++) {
  await drive([b2.rt, s.rt], chain, 1)
  const bo = (await ops(b2, 'buyer', 'purchase'))[0]
  const so = (await ops(s, 'seller', 'sale'))[0]
  if (i % 8 === 0 || ['disclosure_verified', 'useful'].includes(bo?.state)) await dump(`round ${i}`, b2, s)
  if (['disclosure_verified', 'useful'].includes(bo?.state) && ['completion_submitted', 'settled', 'useful'].includes(so?.state)) break
}
console.log(`wall ${Date.now() - t0}ms`)
console.log('TAIL LOGS:', logs.slice(-15).join('\n  '))
await b2.rt.stop(); await s.rt.stop(); await h1.close(); await h2.close()
process.exit(0)
