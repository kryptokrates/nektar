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
import { testWorld, testChain, testFunder, type TestChain } from '@nektar/chain'
import { minihost } from '@nektar/bstack/testing'
import { NektarMemory } from '../src/memory.js'
import { callTool, visibleTools } from '../src/server.js'
import { createHttpApi } from '../src/http.js'
import { createMcpServer } from '../src/mcp-server.js'
import { openAutomation, type Automation, Dispatcher } from '@nektar/automation'
import { ScriptedExecutor } from '@nektar/automation'
import type { AutomationPolicy } from '@nektar/automation'
import { testAgent, testPolicyFor, ZEROCONF_OPEN, type TestAgent, type TestPolicyOpts } from './agentkit.js'

process.env.NEKTAR_PAID_DISCLOSURE = '1'
const AGENT_TEN = ['remember', 'recall', 'groundedAnswer', 'verifyReceipt', 'want', 'sell', 'deals', 'fund', 'doctor', 'automationDoctor']
const FINDING = 'breaking change: libfoo 3.0 renamed connect() to open(); replace connect( with open( to fix the build'
const NET = 'regtest'
let clock = Date.now()

const POLICY: TestPolicyOpts = {
  collectMs: 8000,
  maxPreparationsPerDay: 10,
  limits: { dailyPurchaseSats: 1500, dailyFeeSats: 3000, runTotalSats: 6000, maxLockedSats: 1200, maxUnconfirmedSats: 1200 }
}
const policyFor = (pub: string, policyId = 'demo'): AutomationPolicy => testPolicyFor(pub, { ...POLICY, policyId })

type A = TestAgent
const mkagent = async (name: string, o: { hosts: string[]; chain: TestChain; dir: string }): Promise<A> =>
  // slot-conservation is asserted mid-deal — refill-ahead would mint a
  // second 64-slot tree the moment the want consumes one leaf (treeLowWater:1)
  testAgent(name, { ...o, now: () => clock, funderOutputs: 30, treeLowWater: 1, policy: POLICY })
// real-time yield: want/sell/onTxProof kick a detached cycle that holds `busy`
// for ~hundreds of ms of real work — without a delay the manual cycles all
// no-op on `busy` and the loop finishes before any detached work lands
const drive = async (rts: Automation[], chain: TestChain, n = 2) => {
  for (let i = 0; i < n; i++) {
    clock += 500
    for (const rt of rts) await rt.cycle()
    await chain.mine(1)
    await new Promise((r) => setTimeout(r, 25))
    for (const rt of rts) await rt.cycle()
  }
}
// wall-clock bound, not iteration-bound: under batch contention detached
// passes hold `busy` and manual cycles join rather than progress — a fixed
// iteration count exhausts before the deal converges; a deadline does not
const until = async (rts: Automation[], chain: TestChain, ok: () => Promise<boolean>, ms = 120_000) => {
  const at = Date.now() + ms
  while (Date.now() < at) {
    await drive(rts, chain, 1)
    if (await ok()) return true
  }
  return false
}
const trackerFor = (chain: TestChain) => ({
  isValidRootForHeight: (r: string, h: number) => chain.isValidRootForHeight(r, h),
  currentHeight: () => chain.currentHeight()
})
/** used market funding slots = minted outputs − free remaining — the
 *  ledger's own `taken` counter via laneStock, not a table read */
const usedMarketSlots = async (rt: Automation): Promise<number> => (await rt.laneStock()).market.taken

void test('profile: agent lists exactly the ten agent tools; operator lists all', async () => {
  const prev = process.env.NEKTAR_TOOL_PROFILE
  try {
    delete process.env.NEKTAR_TOOL_PROFILE
    assert.deepEqual((await visibleTools()).map((t) => t.name).sort(), [...AGENT_TEN].sort())
    process.env.NEKTAR_TOOL_PROFILE = 'agent'
    assert.deepEqual((await visibleTools()).map((t) => t.name).sort(), [...AGENT_TEN].sort())
    process.env.NEKTAR_TOOL_PROFILE = 'operator'
    const all = (await visibleTools()).map((t) => t.name)
    assert.ok(all.length > AGENT_TEN.length, `operator sees more than ten (${all.length})`)
    const names: readonly string[] = all
    for (const n of AGENT_TEN) assert.ok(names.includes(n), `operator keeps ${n}`)
    assert.ok(names.includes('describeShard') && names.includes('automation'), 'operator-only tools present')
  } finally {
    if (prev === undefined) delete process.env.NEKTAR_TOOL_PROFILE
    else process.env.NEKTAR_TOOL_PROFILE = prev
  }
})

void test('profile: an operator-only tool is refused over HTTP and MCP in agent profile, allowed in operator profile', async (t) => {
  const w = await testWorld(t, 10)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })
  const prev = process.env.NEKTAR_TOOL_PROFILE
  const srv = createHttpApi(mem).listen(0, '127.0.0.1')
  await new Promise((r) => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`
  try {
    process.env.NEKTAR_TOOL_PROFILE = 'agent'
    assert.deepEqual(
      ((await (await fetch(`${base}/v1/tools`)).json()) as { tools: Array<{ name: string }> }).tools.map((t) => t.name).sort(),
      [...AGENT_TEN].sort()
    )
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
    assert.match((r as { content: Array<{ text: string }> }).content[0].text, /unknown tool/)
    await client.close()

    process.env.NEKTAR_TOOL_PROFILE = 'operator'
    const allowed = await fetch(`${base}/v1/tools/fundingStatus`, { method: 'POST', body: '{}', signal: AbortSignal.timeout(5000) })
    assert.equal(allowed.status, 200, 'HTTP allows the same call under operator profile')
  } finally {
    srv.close()
    if (prev === undefined) delete process.env.NEKTAR_TOOL_PROFILE
    else process.env.NEKTAR_TOOL_PROFILE = prev
  }
})

void test('profile: want refuses zero or several buyer policies; exactly one publishes', async (t) => {
  const chain = (await testChain(t))!
  const dir = mkdtempSync(join(tmpdir(), 'nektar-profile-'))
  const h = await minihost([NET], { now: () => clock, tracker: trackerFor(chain) })
  let rt: Automation | undefined
  try {
    const funder = await testFunder(30)
    const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 1 }, modelId: 'test' })
    const pub = mem.operatorKey().toPublicKey().toString()
    rt = await openAutomation(mem, {
      network: NET,
      dataDir: join(dir, 'b'),
      marketHosts: [h.url],
      intervalMs: 0,
      // in-process executor — no restate server in the memory tier; without it
      // openAutomation boots the shared endpoint and its ticks outlive the test
      exec: new Dispatcher(),
      call: (t2, a) => callTool(mem, t2, a),
      ai: new ScriptedExecutor(),
      now: () => clock,
      opts: { offerTtlBlocks: 10000, refundBlocks: 10000, ...ZEROCONF_OPEN }
    })
    mem.automation = rt
    // zero buyer policies
    await assert.rejects(callTool(mem, 'want', { domains: ['libfoo'], maxSats: 300 }), /exactly one buyer policy/)
    // one
    await rt.store.approvePolicy(policyFor(pub), 'test:operator')
    const r = (await callTool(mem, 'want', { domains: ['libfoo'], maxSats: 300 })) as { wantId: string; operationId: string }
    assert.ok(r.wantId && r.operationId)
    // two buyer policies → ambiguous, refuse
    await rt.store.approvePolicy(policyFor(pub, 'second'), 'test:operator')
    await assert.rejects(callTool(mem, 'want', { domains: ['libfoo'], maxSats: 300 }), /exactly one buyer policy/)
  } finally {
    try {
      if (rt) await rt.stop()
    } catch {}
    await h.close()
    rmSync(dir, { recursive: true, force: true })
  }
})

void test('profile: sell → want → deals drives a full deal; fund reports slot counts', { timeout: 300_000 }, async (t) => {
  const chain = (await testChain(t))!
  const dir = mkdtempSync(join(tmpdir(), 'nektar-profile-'))
  const office = 'nektar_abcdefghij'
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office })
  let b: A | undefined, s: A | undefined
  try {
    b = await mkagent('buyer', { hosts: [h1.url, h2.url], chain, dir })
    s = await mkagent('seller', { hosts: [h1.url, h2.url], chain, dir })
    const bb = b,
      ss = s
    await s.mem.remember('guide', FINDING)
    await chain.mine(1)
    for (const l of s.mem.anchorLinks('guide')) s.mem.confirmAnchor('guide', l.txid)
    const sr = (await callTool(ss.mem, 'sell', { shard: 'guide', domains: ['libfoo'], priceSats: 200 })) as { policyHash: string }
    assert.equal(sr.policyHash, s.policyHash)
    // mint + prove the funding trees before slot bookkeeping
    assert.ok(
      await until([bb.rt], chain, async () => ((await callTool(bb.mem, 'fund', {})) as { marketSlots: { market: number } }).marketSlots.market > 0, 30_000),
      'no market slots minted'
    )
    const used0 = await usedMarketSlots(bb.rt)
    const slots0 = (await callTool(bb.mem, 'fund', {})) as {
      marketSlots: { market: number; bbox: number }
      spendableSats: number
      address: string
      network: string
    }
    assert.equal(slots0.network, NET)
    assert.ok(typeof slots0.address === 'string' && slots0.address.length > 0)
    const w = (await callTool(bb.mem, 'want', { domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 })) as {
      wantId: string
      operationId: string
    }
    const done = async () => {
      const d = (await callTool(bb.mem, 'deals', { operationId: w.operationId })) as { state: string }
      return d.state === 'disclosure_verified'
    }
    // stack tier shares one chain across files — mining rounds and proof
    // delivery compete, so the deal needs headroom past the memory-tier pace
    assert.ok(await until([bb.rt, ss.rt], chain, done, 240_000), 'deal did not reach disclosure_verified')
    const d = (await callTool(bb.mem, 'deals', { operationId: w.operationId })) as { state: string; result: { text: string }; events: unknown[] }
    assert.equal(d.state, 'disclosure_verified')
    assert.equal(d.result.text, FINDING)
    assert.ok(Array.isArray(d.events) && d.events.length > 0)
    const list = (await callTool(bb.mem, 'deals', {})) as { deals: Array<{ operationId: string; state: string; wantId: string }> }
    assert.equal(list.deals.length, 1)
    assert.equal(list.deals[0].wantId, w.wantId)
    const slots1 = (await callTool(bb.mem, 'fund', {})) as { marketSlots: { market: number; bbox: number } }
    assert.ok(slots1.marketSlots.market > 0, 'fund still reports market slots')
    const used1 = await usedMarketSlots(bb.rt)
    assert.equal(used1 - used0, 1, `market slot consumed by the want (${used0} → ${used1} used)`)
  } finally {
    try {
      if (b) await b.rt.stop()
    } catch {}
    try {
      if (s) await s.rt.stop()
    } catch {}
    await h1.close()
    await h2.close()
    rmSync(dir, { recursive: true, force: true })
  }
})
