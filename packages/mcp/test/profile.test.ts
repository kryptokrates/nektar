// NEKTAR_TOOL_PROFILE: the agent profile surfaces exactly the nine tools an
// agent needs (memory + the market trio + funding + doctor); the operator
// profile exposes everything. The gate lives on the HTTP + MCP transports —
// in-process callTool is unaffected.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import type { AddressInfo } from 'node:net'
import { Client } from '@modelcontextprotocol/sdk/client/index.js'
import { InMemoryTransport } from '@modelcontextprotocol/sdk/inMemory.js'
import { MockChain, MockFunder } from '@nektar/chain'
import { minihost } from '@nektar/bstack/testing'
import { NektarMemory } from '../src/memory.js'
import { callTool, visibleTools } from '../src/server.js'
import { createHttpApi } from '../src/http.js'
import { createMcpServer } from '../src/mcp-server.js'
import { openAutomation, type Automation } from '../src/automation/runtime.js'
import { ScriptedExecutor } from '../src/automation/ai.js'
import { DEFAULT_SCORE_WEIGHTS } from '../src/automation/scoring.js'
import type { AutomationPolicy } from '../src/automation/policy.js'

process.env.NEKTAR_PAID_DISCLOSURE = '1'
const AGENT_NINE = ['remember', 'recall', 'groundedAnswer', 'verifyReceipt', 'want', 'sell', 'deals', 'fund', 'doctor']
const FINDING = 'breaking change: libfoo 3.0 renamed connect() to open(); replace connect( with open( to fix the build'
const NET = 'mock'
let clock = Date.now()

const policyFor = (pub: string, policyId = 'demo'): AutomationPolicy => ({
  v: 1, policyId, network: NET, agentPub: pub, roles: ['buyer', 'seller'], notBefore: 0, notAfter: 4_000_000_000_000, domains: ['libfoo'],
  limits: { perPurchaseSats: 500, dailyPurchaseSats: 1500, perTxFeeSats: 600, dailyFeeSats: 3000, runTotalSats: 6000, maxLockedSats: 1200, maxUnconfirmedSats: 1200, maxUnconfirmedCount: 3 },
  scoring: { minimumScoreBps: 0, weights: { ...DEFAULT_SCORE_WEIGHTS }, collectMs: 8000 },
  seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: 10, sellableShards: ['guide'] },
  disclosure: { allowPublic: false },
  ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 200, timeoutMs: 5000 },
  sandbox: { mode: 'sandbox-only' }
})

interface A { mem: NektarMemory, rt: Automation, pub: string, policyHash: string }
const mkagent = async (name: string, o: { hosts: string[], chain: MockChain, dir: string }): Promise<A> => {
  const funder = new MockFunder(); funder.topup(30)
  for (const t of funder.parents) o.chain.registerParent(t)
  const mem = new NektarMemory(o.chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 1 }, modelId: 'test-model-1' })
  const pub = mem.operatorKey().toPublicKey().toString()
  const rt = await openAutomation(mem, { network: NET, dataDir: join(o.dir, name), marketHosts: o.hosts, intervalMs: 0, call: (t2, a) => callTool(mem, t2, a), ai: new ScriptedExecutor(), now: () => clock, opts: { offerTtlBlocks: 10000, refundBlocks: 10000 } })
  mem.automation = rt
  return { mem, rt, pub, policyHash: await rt.store.approvePolicy(policyFor(pub), 'test:operator') }
}
// real-time yield: want/sell/onTxProof kick a detached cycle that holds `busy`
// for ~hundreds of ms of real work — without a delay the manual cycles all
// no-op on `busy` and the loop finishes before any detached work lands
const drive = async (rts: Automation[], chain: MockChain, n = 2) => { for (let i = 0; i < n; i++) { clock += 500; for (const rt of rts) await rt.cycle(); chain.mine(1); await new Promise(r => setTimeout(r, 25)); for (const rt of rts) await rt.cycle() } }
const until = async (rts: Automation[], chain: MockChain, ok: () => Promise<boolean>, n = 80) => { for (let i = 0; i < n; i++) { await drive(rts, chain, 1); if (await ok()) return true } return false }
const trackerFor = (chain: MockChain) => ({ isValidRootForHeight: (r: string, h: number) => chain.isValidRootForHeight(r, h), currentHeight: () => chain.currentHeight() })

test('profile: agent lists exactly the nine agent tools; operator lists all', () => {
  const prev = process.env.NEKTAR_TOOL_PROFILE
  try {
    delete process.env.NEKTAR_TOOL_PROFILE
    assert.deepEqual(visibleTools().map(t => t.name).sort(), [...AGENT_NINE].sort())
    process.env.NEKTAR_TOOL_PROFILE = 'agent'
    assert.deepEqual(visibleTools().map(t => t.name).sort(), [...AGENT_NINE].sort())
    process.env.NEKTAR_TOOL_PROFILE = 'operator'
    const all = visibleTools().map(t => t.name)
    assert.ok(all.length > AGENT_NINE.length, `operator sees more than nine (${all.length})`)
    const names: readonly string[] = all
    for (const n of AGENT_NINE) assert.ok(names.includes(n), `operator keeps ${n}`)
    assert.ok(names.includes('describeShard') && names.includes('automation'), 'operator-only tools present')
  } finally { if (prev === undefined) delete process.env.NEKTAR_TOOL_PROFILE; else process.env.NEKTAR_TOOL_PROFILE = prev }
})

test('profile: an operator-only tool is refused over HTTP and MCP in agent profile, allowed in operator profile', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const funder = new MockFunder(); funder.topup(10); for (const t of funder.parents) chain.registerParent(t)
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })
  const prev = process.env.NEKTAR_TOOL_PROFILE
  const srv = createHttpApi(mem).listen(0, '127.0.0.1')
  await new Promise(r => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`
  try {
    process.env.NEKTAR_TOOL_PROFILE = 'agent'
    assert.deepEqual(((await (await fetch(`${base}/v1/tools`)).json()) as { tools: Array<{ name: string }> }).tools.map(t => t.name).sort(), [...AGENT_NINE].sort())
    const refused = await fetch(`${base}/v1/tools/fundingStatus`, { method: 'POST', body: '{}', signal: AbortSignal.timeout(5000) })
    assert.equal(refused.status, 404, 'HTTP refuses an operator tool under agent profile')
    assert.match(((await refused.json()) as { error: string }).error, /unknown tool/)

    const [ct, st] = InMemoryTransport.createLinkedPair()
    const server = createMcpServer(mem)
    await server.connect(st)
    const client = new Client({ name: 'profile-test', version: '1.0.0' })
    await client.connect(ct)
    const r = await client.callTool({ name: 'fundingStatus', arguments: {} })
    assert.equal((r as { isError?: boolean }).isError, true, 'MCP refuses an operator tool under agent profile')
    assert.match((r as { content: Array<{ text: string }> }).content[0]!.text, /unknown tool/)
    await client.close()

    process.env.NEKTAR_TOOL_PROFILE = 'operator'
    const allowed = await fetch(`${base}/v1/tools/fundingStatus`, { method: 'POST', body: '{}', signal: AbortSignal.timeout(5000) })
    assert.equal(allowed.status, 200, 'HTTP allows the same call under operator profile')
  } finally { srv.close(); if (prev === undefined) delete process.env.NEKTAR_TOOL_PROFILE; else process.env.NEKTAR_TOOL_PROFILE = prev }
})

test('profile: want refuses zero or several buyer policies; exactly one publishes', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-profile-'))
  const h = await minihost([NET], { tracker: trackerFor(chain) })
  let rt: Automation | undefined
  try {
    const funder = new MockFunder(); funder.topup(30)
    for (const t of funder.parents) chain.registerParent(t)
    const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 1 }, modelId: 'test' })
    const pub = mem.operatorKey().toPublicKey().toString()
    rt = await openAutomation(mem, { network: NET, dataDir: join(dir, 'b'), marketHosts: [h.url], intervalMs: 0, call: (t2, a) => callTool(mem, t2, a), ai: new ScriptedExecutor(), now: () => clock, opts: { offerTtlBlocks: 10000, refundBlocks: 10000 } })
    mem.automation = rt
    // zero buyer policies
    await assert.rejects(callTool(mem, 'want', { domains: ['libfoo'], maxSats: 300 }), /exactly one buyer policy/)
    // one
    await rt.store.approvePolicy(policyFor(pub), 'test:operator')
    const r = await callTool(mem, 'want', { domains: ['libfoo'], maxSats: 300 }) as { wantId: string, operationId: string }
    assert.ok(r.wantId && r.operationId)
    // two buyer policies → ambiguous, refuse
    await rt.store.approvePolicy(policyFor(pub, 'second'), 'test:operator')
    await assert.rejects(callTool(mem, 'want', { domains: ['libfoo'], maxSats: 300 }), /exactly one buyer policy/)
  } finally { try { if (rt) await rt.stop() } catch {} await h.close(); rmSync(dir, { recursive: true, force: true }) }
})

test('profile: sell → want → deals drives a full deal; fund reports slot counts', { timeout: 300_000 }, async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-profile-'))
  const office = 'nektar_abcdefghij'
  const h1 = await minihost([NET], { tracker: trackerFor(chain), office })
  const h2 = await minihost([NET], { tracker: trackerFor(chain), office })
  let b: A | undefined, s: A | undefined
  try {
    b = await mkagent('buyer', { hosts: [h1.url, h2.url], chain, dir })
    s = await mkagent('seller', { hosts: [h1.url, h2.url], chain, dir })
    const bb = b, ss = s
    await s.mem.remember('guide', FINDING)
    chain.mine(1)
    for (const l of s.mem.anchorLinks('guide')) s.mem.confirmAnchor('guide', l.txid)
    const sr = await callTool(ss.mem, 'sell', { shard: 'guide', domains: ['libfoo'], priceSats: 200 }) as { policyHash: string }
    assert.equal(sr.policyHash, s.policyHash)
    // mint + prove the funding trees before slot bookkeeping
    assert.ok(await until([bb.rt], chain, async () => (await callTool(bb.mem, 'fund', {}) as { marketSlots: { market: number } }).marketSlots.market > 0, 30), 'no market slots minted')
    const slots0 = (await callTool(bb.mem, 'fund', {})) as { marketSlots: { market: number, bbox: number }, spendableSats: number, address: string, network: string }
    assert.equal(slots0.network, NET)
    assert.ok(typeof slots0.address === 'string' && slots0.address.length > 0)
    const w = await callTool(bb.mem, 'want', { domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }) as { wantId: string, operationId: string }
    const done = async () => {
      const d = await callTool(bb.mem, 'deals', { operationId: w.operationId }) as { state: string }
      return d.state === 'disclosure_verified'
    }
    assert.ok(await until([bb.rt, ss.rt], chain, done), 'deal did not reach disclosure_verified')
    const d = await callTool(bb.mem, 'deals', { operationId: w.operationId }) as { state: string, result: { text: string }, events: unknown[] }
    assert.equal(d.state, 'disclosure_verified')
    assert.equal(d.result.text, FINDING)
    assert.ok(Array.isArray(d.events) && d.events.length > 0)
    const list = await callTool(bb.mem, 'deals', {}) as { deals: Array<{ operationId: string, state: string, wantId: string }> }
    assert.equal(list.deals.length, 1)
    assert.equal(list.deals[0]!.wantId, w.wantId)
    const slots1 = (await callTool(bb.mem, 'fund', {})) as { marketSlots: { market: number, bbox: number } }
    assert.ok(slots1.marketSlots.market < slots0.marketSlots.market, `market slot consumed by the want (${slots0.marketSlots.market} → ${slots1.marketSlots.market})`)
  } finally { try { if (b) await b.rt.stop() } catch {} try { if (s) await s.rt.stop() } catch {} await h1.close(); await h2.close(); rmSync(dir, { recursive: true, force: true }) }
})
