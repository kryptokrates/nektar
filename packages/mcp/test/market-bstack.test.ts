import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import knexFactory from 'knex'
import { MockChain, MockFunder } from '@nektar/chain'
import { minihost, type MinihostHandle, type MinihostOpts } from '@nektar/bstack/testing'
import { NektarMemory } from '../src/memory.js'
import { callTool } from '../src/server.js'
import { openAutomation, type Automation } from '../src/automation/runtime.js'
import { ScriptedExecutor } from '../src/automation/ai.js'
import { DEFAULT_SCORE_WEIGHTS } from '../src/automation/scoring.js'
import type { AutomationPolicy } from '../src/automation/policy.js'
import type { MarketMessage } from '../src/automation/market.js'

process.env.NEKTAR_PAID_DISCLOSURE = '1'
const FINDING = 'breaking change: libfoo 3.0 renamed connect() to open(); replace connect( with open( to fix the build'
const NET = 'mock'
let clock = Date.now()

const bus = () => {
  const boxes = new Map<string, Array<{ from: string, msg: MarketMessage }>>()
  return {
    for: (pub: string) => ({
      send: async (to: string, msg: MarketMessage) => { if (!boxes.has(to)) boxes.set(to, []); boxes.get(to)!.push({ from: pub, msg }) },
      collect: async () => { const q = boxes.get(pub) ?? []; boxes.set(pub, []); return q }
    })
  }
}

const policyFor = (pub: string): AutomationPolicy => ({
  v: 1, policyId: 'demo', network: NET, agentPub: pub, roles: ['buyer', 'seller'], notBefore: 0, notAfter: 4_000_000_000_000, domains: ['libfoo'],
  limits: { perPurchaseSats: 500, dailyPurchaseSats: 1000, perTxFeeSats: 600, dailyFeeSats: 2000, runTotalSats: 5000, maxLockedSats: 1000, maxUnconfirmedSats: 1000, maxUnconfirmedCount: 3 },
  scoring: { minimumScoreBps: 0, weights: { ...DEFAULT_SCORE_WEIGHTS }, collectMs: 8000 },
  seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: 5, sellableShards: ['guide'] },
  disclosure: { allowPublic: false },
  ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 200, timeoutMs: 5000 },
  sandbox: { mode: 'sandbox-only' }
})

interface A { mem: NektarMemory, rt: Automation, pub: string, policyHash: string, name: string }
interface MkOpts { hosts: string[], msgs?: ReturnType<typeof bus>, chain: MockChain, logs: string[], dir: string, opts?: Partial<import('../src/automation/market.js').MarketOptions>, call?: (tool: string, args: any) => Promise<any> }
const mkagent = async (name: string, o: MkOpts): Promise<A> => {
  const funder = new MockFunder(); funder.topup(30)
  for (const t of funder.parents) o.chain.registerParent(t)
  const mem = new NektarMemory(o.chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 1 }, modelId: 'test-model-1' })
  const pub = mem.operatorKey().toPublicKey().toString()
  const rt = await openAutomation(mem, { network: NET, dataDir: join(o.dir, name), marketHosts: o.hosts, intervalMs: 0, call: o.call ?? ((t2, a) => callTool(mem, t2, a)), log: l => o.logs.push(`${name}:${l}`), ai: new ScriptedExecutor(), now: () => clock, messages: o.msgs?.for(pub), opts: { offerTtlBlocks: 10000, refundBlocks: 10000, ...o.opts } })
  return { mem, rt, pub, policyHash: await rt.store.approvePolicy(policyFor(pub), 'test:operator'), name }
}
const sellerize = async (s: A, price: number) => {
  await s.mem.remember('guide', FINDING)
  await s.mem.remember('guide', 'unrelated note about gardening and tomatoes')
  await s.mem.describeShard('guide', { domains: ['libfoo'], offers: [{ kind: 'record', priceSats: price }] })
  for (const l of s.mem.anchorLinks('guide')) s.mem.confirmAnchor('guide', l.txid)
  await s.rt.engine.enableSeller(s.policyHash)
}
const drive = async (rts: Automation[], chain: MockChain, n = 2) => { for (let i = 0; i < n; i++) { clock += 500; for (const rt of rts) await rt.cycle(); chain.mine(1) } }
const until = async (rts: Automation[], chain: MockChain, ok: () => Promise<boolean>, n = 60) => { for (let i = 0; i < n; i++) { await drive(rts, chain, 1); if (await ok()) return true } return false }
// like until but checks right after the buyer's cycle, before the seller gets
// another turn — so a test can freeze the buyer before the seller can react to
// the accept it just flushed (a delivered the dead runtime consumes is lost)
const untilBuyer = async (b: A, s: A, chain: MockChain, ok: () => Promise<boolean>, n = 60): Promise<boolean> => {
  for (let i = 0; i < n; i++) {
    clock += 500
    await b.rt.cycle(); if (await ok()) return true
    await s.rt.cycle(); if (await ok()) return true
    chain.mine(1)
  }
  return false
}
const trackerFor = (chain: MockChain) => ({
  isValidRootForHeight: (r: string, h: number) => chain.isValidRootForHeight(r, h),
  currentHeight: () => chain.currentHeight()
})
const countWhere = async (dir: string, name: string, table: string, col: string, val: string): Promise<number> => {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: join(dir, name, 'automation.sqlite') }, useNullAsDefault: true })
  try { const r = await kx(table).count('* as n').where(col, val).first(); return Number((r as { n: number }).n) } finally { await kx.destroy() }
}
const countLike = async (dir: string, name: string, table: string, col: string, pat: string): Promise<number> => {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: join(dir, name, 'automation.sqlite') }, useNullAsDefault: true })
  try { const r = await kx(table).count('* as n').where(col, 'like', pat).first(); return Number((r as { n: number }).n) } finally { await kx.destroy() }
}
const buyerOp = async (b: A) => (await b.rt.store.operations({ role: 'buyer' })).find(o => o.kind === 'purchase')
const saleOps = async (s: A) => (await s.rt.store.operations({ role: 'seller' })).filter(o => o.kind === 'sale')
const COMPLETE = ['completion_submitted', 'settled', 'useful']

test('bstack A: buyer+seller over two minihosts — journal+slot conservation, dedupe, both hosts hold', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const msgs = bus(); const logs: string[] = []
  let h2Failed = false
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain) })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), failNext: () => { if (!h2Failed) { h2Failed = true; return true } return false } })
  try {
    const b = await mkagent('buyer', { hosts: [h1.url, h2.url], msgs, chain, logs, dir })
    const s = await mkagent('seller', { hosts: [h1.url, h2.url], msgs, chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 8)
    // market-kind slots only — the runtime also keeps bbox + log trees
    const marketSlots = async () => (await countWhere(dir, 'buyer', 'bstack_trees', 'kind', 'market')) * 16 - (await countWhere(dir, 'buyer', 'bstack_tree_outputs', 'kind', 'market'))
    const slotsBefore = await marketSlots()
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    const done = async () => {
      const bo = await buyerOp(b); const so = (await saleOps(s))[0]
      return bo?.state === 'disclosure_verified' && so !== undefined && COMPLETE.includes(so.state)
    }
    assert.ok(await until([b.rt, s.rt], chain, done, 80), 'logs: ' + logs.slice(-20).join(' | '))
    const bo = await buyerOp(b); const so = (await saleOps(s))[0]
    assert.equal(bo?.state, 'disclosure_verified')
    assert.ok(so && COMPLETE.includes(so.state), `seller op ${so?.state}`)
    assert.ok(h2Failed, 'h2 received the 503-injected first submit')
    // both hosts hold the want carrier
    const wantTxid = [...h1.txids()].filter(t => h2.has(t))
    assert.ok(wantTxid.length >= 1, `both hosts hold want; h1=${h1.txids().length} h2=${h2.txids().length}`)
    // journal: exactly one market carrier row (the want), one market slot —
    // log-journal batches live in the same tables under tm_log topics/kind
    assert.equal(await countLike(dir, 'buyer', 'bstack_carriers', 'topics', '%tm_nektar%'), 1)
    const slotsAfter = await marketSlots()
    assert.equal(slotsAfter - slotsBefore, 1, 'one funding slot for the want')
    // dedupe: 5+ more cycles, still ONE sale op and ONE inbox row for the want txid
    await drive([b.rt, s.rt], chain, 5)
    assert.equal((await saleOps(s)).length, 1)
    const kx = knexFactory({ client: 'sqlite3', connection: { filename: join(dir, 'seller', 'automation.sqlite') }, useNullAsDefault: true })
    const inboxRows = await kx('market_inbox').select('*').whereIn('message_id', wantTxid)
    await kx.destroy()
    assert.equal(inboxRows.length, 1)
    await b.rt.stop(); await s.rt.stop()
  } finally { await h1.close(); await h2.close() }
})

test('bstack B: competing sellers — buyer completes with the 200-sat offer', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const msgs = bus(); const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain) })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain) })
  try {
    const b = await mkagent('buyer', { hosts: [h1.url, h2.url], msgs, chain, logs, dir })
    const sA = await mkagent('sA', { hosts: [h1.url], msgs, chain, logs, dir })
    const sB = await mkagent('sB', { hosts: [h2.url], msgs, chain, logs, dir })
    const sC = await mkagent('sC', { hosts: [h1.url, h2.url], msgs, chain, logs, dir })
    await sellerize(sA, 450); await sellerize(sB, 300); await sellerize(sC, 200)
    await drive([b.rt, sA.rt, sB.rt, sC.rt], chain, 8)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 500 }, b.policyHash)
    const all = [b.rt, sA.rt, sB.rt, sC.rt]
    assert.ok(await until(all, chain, async () => (await buyerOp(b))?.state === 'disclosure_verified', 100), 'logs: ' + logs.slice(-20).join(' | '))
    const bo = await buyerOp(b)
    const sel = (bo?.data.selected as { key?: string, priceSats?: number } | undefined)
    assert.ok(sel?.key?.startsWith(sC.pub + ':'), `selected the 200-sat seller, got ${JSON.stringify(sel)}`)
    const amounts = bo?.data.amounts as { priceSats?: number, purchaseSats?: number } | undefined
    assert.equal(amounts?.purchaseSats ?? amounts?.priceSats ?? sel?.priceSats, 200)
    for (const loser of [sA, sB]) for (const op of await saleOps(loser)) assert.ok(!COMPLETE.includes(op.state), `${loser.name} sale op reached ${op.state}`)
    for (const r of all) await r.stop()
  } finally { await h1.close(); await h2.close() }
})

test('bstack C: withheld offer still completes via other host; withheld logged naming h2', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const msgs = bus(); const logs: string[] = []
  let dropTxid = ''
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain) })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), drop: (txid) => txid === dropTxid })
  try {
    const b = await mkagent('buyer', { hosts: [h1.url, h2.url], msgs, chain, logs, dir })
    const s = await mkagent('seller', { hosts: [h1.url, h2.url], msgs, chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 8)
    const w = await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 500 }, b.policyHash)
    const all = [b.rt, s.rt]
    for (let i = 0; i < 20 && dropTxid === ''; i++) {
      await drive(all, chain, 1)
      const { readOffers } = await import('@nektar/bstack')
      const r = await readOffers([h1.url], { net: NET, wantId: w.wantId, buyerPub: b.pub }, { tracker: trackerFor(chain), net: NET })
      if (r.items.length > 0) dropTxid = r.items[0]!.txid
    }
    assert.notEqual(dropTxid, '', 'offer carrier landed on h1')
    assert.ok(await until(all, chain, async () => (await buyerOp(b))?.state === 'disclosure_verified', 100), 'logs: ' + logs.slice(-20).join(' | '))
    const bo = await buyerOp(b)
    assert.equal(bo?.state, 'disclosure_verified')
    const sel = (bo?.data.selected as { key?: string } | undefined)?.key ?? ''
    assert.ok(sel.startsWith(s.pub + ':'), 'dropped offer is the selected one')
    assert.ok(logs.some(l => l.includes('withheld') && l.includes(h2.url)), 'withheld notice naming h2 | ' + logs.filter(l => l.includes('withheld')).join(';'))
    for (const r of all) await r.stop()
  } finally { await h1.close(); await h2.close() }
})

test('bstack D: restart — journal+ledger prevent second carrier, purchase completes', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const msgs = bus(); const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain) })
  try {
    const funderB = new MockFunder(); funderB.topup(30)
    for (const t of funderB.parents) chain.registerParent(t)
    const memB = new NektarMemory(chain, funderB, funderB.keyPair, { batchPolicy: { maxLeaves: 1 }, modelId: 'test-model-1' })
    const pubB = memB.operatorKey().toPublicKey().toString()
    const open = async () => await openAutomation(memB, { network: NET, dataDir: join(dir, 'buyer'), marketHosts: [h1.url], intervalMs: 0, call: (t2, a) => callTool(memB, t2, a), log: l => logs.push('B:' + l), ai: new ScriptedExecutor(), now: () => clock, messages: msgs.for(pubB), opts: { offerTtlBlocks: 10000, refundBlocks: 10000 } })
    let bRt = await open()
    const policyHash = await bRt.store.approvePolicy(policyFor(pubB), 'test:operator')
    const s = await mkagent('seller', { hosts: [h1.url], msgs, chain, logs, dir })
    await sellerize(s, 200)
    await drive([bRt, s.rt], chain, 8)
    await bRt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 500 }, policyHash)
    await drive([bRt, s.rt], chain, 6)
    // market-kind only — log journal batches keep accruing in the same tables
    const carriersBefore = await countLike(dir, 'buyer', 'bstack_carriers', 'topics', '%tm_nektar%')
    const slotsBefore = (await countWhere(dir, 'buyer', 'bstack_trees', 'kind', 'market')) * 16 - (await countWhere(dir, 'buyer', 'bstack_tree_outputs', 'kind', 'market'))
    await bRt.stop()
    bRt = await open()
    const all = [bRt, s.rt]
    assert.ok(await until(all, chain, async () => (await buyerOp({ rt: bRt } as A))?.state === 'disclosure_verified', 100), 'logs: ' + logs.slice(-20).join(' | '))
    const bo = (await bRt.store.operations({ role: 'buyer' })).find(o => o.kind === 'purchase')
    assert.equal(bo?.state, 'disclosure_verified')
    assert.equal(await countLike(dir, 'buyer', 'bstack_carriers', 'topics', '%tm_nektar%'), carriersBefore, 'no second want carrier after restart')
    const slotsAfter = (await countWhere(dir, 'buyer', 'bstack_trees', 'kind', 'market')) * 16 - (await countWhere(dir, 'buyer', 'bstack_tree_outputs', 'kind', 'market'))
    assert.equal(slotsAfter - slotsBefore, 0, 'zero funding slots consumed for the want after restart')
    for (const r of all) await r.stop()
  } finally { await h1.close() }
})

test('bstack E: hosts down after escrow funded — chain advance still refunds the buyer', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const msgs = bus(); const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain) })
  try {
    const b = await mkagent('buyer', { hosts: [h1.url], msgs, chain, logs, dir, opts: { refundBlocks: 40 } })
    const s = await mkagent('seller', { hosts: [h1.url], msgs, chain, logs, dir, opts: { refundBlocks: 40 } })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 8)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 500 }, b.policyHash)
    let funded = false
    for (let i = 0; i < 80 && !funded; i++) {
      clock += 500
      await b.rt.cycle()
      if ((await buyerOp(b))?.state === 'awaiting_disclosure') { funded = true; break }
      await s.rt.cycle(); chain.mine(1)
    }
    assert.ok(funded, 'escrow never funded | ' + logs.slice(-20).join(' | '))
    // both hosts go down and the seller stops — tick must still fire refunds
    await h1.close()
    await s.rt.stop()
    chain.mine(10)
    const refunded = await until([b.rt], chain, async () => (await buyerOp(b))?.state === 'refunded', 30)
    const fin = await buyerOp(b)
    assert.ok(refunded, `buyer not refunded state=${fin?.state} err=${fin?.lastError} | ` + logs.slice(-20).join(' | '))
    await b.rt.stop()
  } finally { await h1.close() }
})


test('bstack F: default bbox channel � no injected bus; forged delivered ignored', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  const office = 'nektar_abcdefghij'
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office })
  try {
    // no `msgs` -> runtime uses the default bbox channel over the overlay hosts
    const b = await mkagent('buyer', { hosts: [h1.url, h2.url], chain, logs, dir })
    const s = await mkagent('seller', { hosts: [h1.url, h2.url], chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    const w = await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    const all = [b.rt, s.rt]
    // wait for the escrow to land so the forged 'delivered' hits the from-check
    assert.ok(await until(all, chain, async () => (await buyerOp(b))?.state === 'awaiting_disclosure', 80), 'escrow never funded | ' + logs.slice(-20).join(' | '))
    const bo0 = await buyerOp(b)
    const sel = (bo0?.data.selected as { key: string }).key
    const offerId = (bo0?.data.offers as Record<string, { so: { offer: { offerId: string } } }>)[sel]!.so.offer.offerId
    const { sendBox, MemoryTreeLedger, MemoryCarrierJournal, boxFundingLock, mintTree } = await import('@nektar/bstack')
    const { PrivateKey, Transaction, P2PKH, MerklePath } = await import('@bsv/sdk')
    const ghost = PrivateKey.fromRandom()
    const parent = new Transaction(); parent.addOutput({ satoshis: 100_000, lockingScript: new P2PKH().lock(ghost.toPublicKey().toAddress()) })
    const gTree = await mintTree({ key: ghost, funding: { tx: parent, vout: 0, satoshis: 100_000 }, n: 4, satsPerOutput: 1000, lock: await boxFundingLock(ghost) })
    gTree.merklePath = new MerklePath(1, [[{ offset: 0, hash: '00'.repeat(32) }, { offset: 1, hash: gTree.id('hex'), txid: true }]])
    const gl = new MemoryTreeLedger(); await gl.add(gTree, 4, 'bbox'); await gl.markProven(gTree.id('hex'))
    await sendBox({ key: ghost, ledger: gl, journal: new MemoryCarrierJournal(), hosts: [h1.url, h2.url], office, to: b.pub, body: { market: { type: 'delivered', wantId: w.wantId, offerId, txid: 'ff'.repeat(32), txHex: 'ff' } } })
    const done = async () => {
      const bo = await buyerOp(b); const so = (await saleOps(s))[0]
      return bo?.state === 'disclosure_verified' && so !== undefined && COMPLETE.includes(so.state)
    }
    assert.ok(await until(all, chain, done, 100), 'logs: ' + logs.slice(-20).join(' | '))
    const bo = await buyerOp(b)
    assert.equal(bo?.state, 'disclosure_verified', 'forged delivered did not derail the purchase')
    const envs = async (name: string) => {
      const kx = knexFactory({ client: 'sqlite3', connection: { filename: join(dir, name, 'automation.sqlite') }, useNullAsDefault: true })
      const n = (await kx('bstack_carriers').where('topics', 'like', '%tm_bbox%').count('* as c').first() as { c: number }).c
      await kx.destroy(); return Number(n)
    }
    assert.ok((await envs('buyer')) + (await envs('seller')) >= 3, 'quote+accept+delivered envelopes were sent')
    for (const r of all) await r.stop()
  } finally { await h1.close(); await h2.close() }
})

test('bstack G: read envelopes are acked off the hosts; want+offer carry signed expiry', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  const office = 'nektar_abcdefghij'
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office })
  try {
    const b = await mkagent('buyer', { hosts: [h1.url, h2.url], chain, logs, dir })
    const s = await mkagent('seller', { hosts: [h1.url, h2.url], chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    const all = [b.rt, s.rt]
    assert.ok(await until(all, chain, async () => (await buyerOp(b))?.state === 'disclosure_verified', 100), 'no deal | ' + logs.slice(-20).join(' | '))
    const bo = (await buyerOp(b))!
    const sw = (bo.data.want as { want: { ts: number, exp?: number } }).want
    assert.equal(sw.exp, sw.ts + 8000, 'want expires with its collect window')
    const sel = (bo.data.selected as { key: string }).key
    const so = (bo.data.offers as Record<string, { so: { offer: { exp?: number } } }>)[sel]!.so
    assert.ok(Number.isSafeInteger(so.offer.exp), 'offer carries a signed expiry')
    await drive(all, chain, 2)
    const { readBox } = await import('@nektar/bstack')
    const tracker = trackerFor(chain)
    assert.equal((await readBox({ key: b.mem.operatorKey(), hosts: [h1.url, h2.url], office, tracker })).items.length, 0, 'buyer inbox acked empty')
    assert.equal((await readBox({ key: s.mem.operatorKey(), hosts: [h1.url, h2.url], office, tracker })).items.length, 0, 'seller inbox acked empty')
    for (const r of all) await r.stop()
  } finally { await h1.close(); await h2.close() }
})

test('bstack H: blind seller completes via accept txHex — seller broadcasts the escrow itself', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const msgs = bus(); const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain) })
  let b!: Awaited<ReturnType<typeof mkagent>>
  let s!: Awaited<ReturnType<typeof mkagent>>
  try {
    b = await mkagent('buyer', { hosts: [h1.url], msgs, chain, logs, dir })
    s = await mkagent('seller', { hosts: [h1.url], msgs, chain, logs, dir })
    // the seller's view can never see the escrow tx (indexer lag) — but it
    // must broadcast it itself once, from the bytes inside the accept
    let blindTxid: string | undefined
    const origFetch = s.mem.fetchTx.bind(s.mem)
    s.mem.fetchTx = async (id: string) => id === blindTxid ? undefined : await origFetch(id)
    let escrowBroadcasts = 0
    const origBcast = s.mem.broadcastTx.bind(s.mem)
    s.mem.broadcastTx = async (t: import('@bsv/sdk').Transaction) => { if (t.id('hex') === blindTxid) escrowBroadcasts++; return await origBcast(t) }
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    let done = false
    for (let i = 0; i < 100 && !done; i++) {
      clock += 500
      await b.rt.cycle()
      const bo = await buyerOp(b)
      if (blindTxid === undefined && bo?.data.escrowTxid !== undefined) blindTxid = bo.data.escrowTxid as string
      chain.mine(1)
      await s.rt.cycle()
      if (bo?.state === 'disclosure_verified') done = true
    }
    assert.ok(done, 'no deal | ' + logs.slice(-20).join(' | '))
    assert.ok(escrowBroadcasts >= 1, 'seller broadcast the escrow tx itself')
  } finally { if (b !== undefined) await b.rt.stop(); if (s !== undefined) await s.rt.stop(); await h1.close() }
})

test('bstack I: tampered accept — txHex of a different transaction → seller stays quoted with accept_refused', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const msgs = bus(); const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain) })
  let b!: Awaited<ReturnType<typeof mkagent>>
  let s!: Awaited<ReturnType<typeof mkagent>>
  try {
    b = await mkagent('buyer', { hosts: [h1.url], msgs, chain, logs, dir })
    s = await mkagent('seller', { hosts: [h1.url], msgs, chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    assert.ok(await until([b.rt, s.rt], chain, async () => (await saleOps(s))[0]?.state === 'quoted', 80), 'no quote | ' + logs.slice(-20).join(' | '))
    const op = (await saleOps(s))[0]!
    const want = (op.data.want as { want: { wantId: string, pub: string } }).want
    const offer = op.data.offer as { cfg: { price: number | string, refundAfter: number | string, maxFee: number | string } }
    const { PublicKey, Script, Transaction } = await import('@bsv/sdk')
    const { DisclosureSaleHost } = await import('@nektar/anchor')
    const pkh = Buffer.from(PublicKey.fromString(b.pub).toHash() as number[]).toString('hex')
    const cfg = { ...offer.cfg, price: BigInt(offer.cfg.price), refundAfter: BigInt(offer.cfg.refundAfter), maxFee: BigInt(offer.cfg.maxFee) }
    const script = DisclosureSaleHost.escrowScript(cfg as never, pkh)
    // a perfectly valid script+amount claim — but the carried bytes are a DIFFERENT transaction
    const other = new Transaction()
    other.addInput({ sourceTXID: '11'.repeat(32), sourceOutputIndex: 0, unlockingScript: Script.fromHex('51') })
    other.addOutput({ satoshis: 9999, lockingScript: Script.fromHex(script) })
    const fakeUtxo = { txid: 'aa'.repeat(32), vout: 0, satoshis: 9999, script }
    await s.rt.engine.ingestMessage(b.pub, { type: 'accept', wantId: want.wantId, offerId: op.operationId, utxo: fakeUtxo, depositorPkh: pkh, txHex: other.toHex() })
    await s.rt.cycle() // tick drains the inbox → onAccept
    const op2 = (await saleOps(s))[0]!
    assert.equal(op2.state, 'quoted', 'tampered accept must not advance the sale')
    const evs = await s.rt.store.events(op.operationId)
    assert.ok(evs.some(e => e.cause === 'accept_refused'), 'accept_refused event recorded')
  } finally { if (b !== undefined) await b.rt.stop(); if (s !== undefined) await s.rt.stop(); await h1.close() }
})

test('bstack J: push — a mined funding tree is proven and a cycle runs without cycle()', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain) })
  let a!: Awaited<ReturnType<typeof mkagent>>
  let kx: ReturnType<typeof knexFactory> | undefined
  try {
    a = await mkagent('push', { hosts: [h1.url], chain, logs, dir })
    await a.rt.cycle() // broadcast market + bbox funding trees — still unproven
    kx = knexFactory({ client: 'sqlite3', connection: { filename: join(dir, 'push', 'automation.sqlite') }, useNullAsDefault: true })
    const { KnexTreeLedger } = await import('@nektar/bstack')
    const ledger2 = new KnexTreeLedger(kx)
    assert.ok((await ledger2.unproven()).length >= 2, 'funding trees broadcast but unproven')
    let ticks = 0
    const engine = a.rt.engine as unknown as { tick: () => Promise<void> }
    const origTick = engine.tick.bind(engine)
    engine.tick = async () => { ticks++; return await origTick() }
    chain.mine(1) // the push: mined events → proofs delivered → markProven + cycle
    const t0 = Date.now()
    while ((ticks === 0 || (await ledger2.unproven()).length > 0) && Date.now() - t0 < 5000) await new Promise(r => setTimeout(r, 25))
    assert.equal((await ledger2.unproven()).length, 0, 'funding trees proven on the push — no manual cycle')
    assert.ok(ticks > 0, 'a cycle ran on the push (intervalMs was 0)')
  } finally {
    if (a !== undefined) await a.rt.stop() // unsubscribe proofs before pools close
    if (kx !== undefined) await kx.destroy()
    await h1.close()
  }
})


// recovery-note helpers: wipe the automation dir and reopen on the SAME memory
const respawn = async (a: A, o: { dir: string, name: string, hosts: string[], logs: string[], chain: MockChain, opts?: MkOpts['opts'] }): Promise<A> => {
  const rt = await openAutomation(a.mem, { network: NET, dataDir: join(o.dir, o.name), marketHosts: o.hosts, intervalMs: 0, call: (t2, args) => callTool(a.mem, t2, args), log: l => o.logs.push(`${o.name}:${l}`), ai: new ScriptedExecutor(), now: () => clock, opts: { offerTtlBlocks: 10000, refundBlocks: 10000, ...o.opts } })
  return { mem: a.mem, rt, pub: a.pub, policyHash: await rt.store.approvePolicy(policyFor(a.pub), 'test:operator'), name: o.name }
}
const OFFICE = 'nektar_abcdefghij'
const readBuyer = async (key: import('@bsv/sdk').PrivateKey, hosts: string[], chain: MockChain) => {
  const { readBox } = await import('@nektar/bstack')
  return await readBox({ key, hosts, office: OFFICE, tracker: trackerFor(chain) })
}
const noteItems = <T extends { body: object }>(items: T[]): T[] => items.filter(i => (i.body as { market?: { type?: string } }).market?.type === 'note')

test('bstack K: buyer loses its database — the inbox note rebuilds the deal and the refund still fires', { timeout: 300_000 }, async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  let b!: A; let s!: A; let b2!: A
  try {
    b = await mkagent('buyer', { hosts: [h1.url, h2.url], chain, logs, dir, opts: { refundBlocks: 40 } })
    s = await mkagent('seller', { hosts: [h1.url, h2.url], chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    assert.ok(await until([b.rt, s.rt], chain, async () => (await saleOps(s))[0]?.state === 'quoted', 80), 'no quote | ' + logs.slice(-20).join(' | '))
    await s.rt.stop() // seller dies before the accept is processed
    s = undefined as never
    assert.ok(await until([b.rt], chain, async () => (await buyerOp(b))?.state === 'awaiting_disclosure', 80), 'never funded | ' + logs.slice(-20).join(' | '))
    const escrowTxid = (await buyerOp(b))!.data.escrowTxid as string
    const refundAfter = Number(((await buyerOp(b))!.data.escrow as { refundAfter: number }).refundAfter)
    await drive([b.rt], chain, 2) // the note lands + is ingested but stays unacked
    await b.rt.stop()
    rmSync(join(dir, 'buyer'), { recursive: true, force: true })
    b2 = await respawn(b, { dir, name: 'buyer', hosts: [h1.url, h2.url], logs, chain, opts: { refundBlocks: 40 } })
    assert.equal(b2.policyHash, b.policyHash, 're-approved same policy file → same content hash')
    assert.ok(await until([b2.rt], chain, async () => (await buyerOp(b2))?.state === 'awaiting_disclosure', 60), 'op not restored | ' + logs.slice(-20).join(' | '))
    const ops = (await b2.rt.store.operations({ role: 'buyer' })).filter(o => o.kind === 'purchase')
    assert.equal(ops.length, 1, 'exactly one buyer operation after restore')
    assert.equal(ops[0]!.data.restored, true)
    assert.equal(ops[0]!.data.escrowTxid, escrowTxid, 'same escrow')
    while ((await b2.mem.chainHeight()) < refundAfter) chain.mine(1) // refundAfter is on the engine's height scale
    const wasRefunded = await until([b2.rt], chain, async () => (await buyerOp(b2))?.state === 'refunded', 40)
    if (!wasRefunded) console.log('KFAIL', JSON.stringify(await buyerOp(b2)), '\nLOGS', logs.slice(-25))
    assert.ok(wasRefunded, 'never refunded | ' + logs.slice(-20).join(' | '))
    const refunded = (await buyerOp(b2))!
    const rtx = chain.getTx(refunded.data.refundTxid as string)
    assert.equal(rtx?.inputs[0]?.sourceTXID, escrowTxid, 'refund tx spends the escrow output')
    await drive([b2.rt], chain, 2)
    assert.equal((await readBuyer(b.mem.operatorKey(), [h1.url, h2.url], chain)).items.length, 0, 'note acked once the deal was finished')
  } finally { try { await b?.rt.stop() } catch { /* already stopped */ } try { await s?.rt.stop() } catch { /* already stopped */ } try { await b2?.rt.stop() } catch { /* already stopped */ } await h1.close(); await h2.close() }
})

test('bstack L: buyer loses its database — delivery still completes after restore', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  let b!: A; let s!: A; let b2!: A
  try {
    b = await mkagent('buyer', { hosts: [h1.url, h2.url], chain, logs, dir })
    s = await mkagent('seller', { hosts: [h1.url, h2.url], chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    assert.ok(await untilBuyer(b, s, chain, async () => (await buyerOp(b))?.state === 'awaiting_disclosure', 100), 'never funded | ' + logs.slice(-20).join(' | '))
    const escrowTxid = (await buyerOp(b))!.data.escrowTxid as string
    await b.rt.stop()
    rmSync(join(dir, 'buyer'), { recursive: true, force: true })
    b2 = await respawn(b, { dir, name: 'buyer', hosts: [h1.url, h2.url], logs, chain })
    assert.ok(await until([b2.rt, s.rt], chain, async () => (await buyerOp(b2))?.state === 'disclosure_verified', 100), 'delivery never verified | ' + logs.slice(-20).join(' | '))
    const bo = (await buyerOp(b2))!
    assert.equal(bo.data.restored, true)
    assert.equal(bo.data.escrowTxid, escrowTxid)
    assert.equal((bo.data.result as { verified?: boolean }).verified, true, 'restored buyer verified the disclosure')
    await drive([b2.rt, s.rt], chain, 2)
    assert.equal((await readBuyer(b.mem.operatorKey(), [h1.url, h2.url], chain)).items.length, 0, 'inbox empty after the restored deal finished')
  } finally { try { await b?.rt.stop() } catch { /* already stopped */ } try { await s?.rt.stop() } catch { /* already stopped */ } try { await b2?.rt.stop() } catch { /* already stopped */ } await h1.close(); await h2.close() }
})

test('bstack M: no loss — one operation; the note is listed while the deal is open and acked when it ends', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  let b!: A; let s!: A
  try {
    b = await mkagent('buyer', { hosts: [h1.url, h2.url], chain, logs, dir })
    s = await mkagent('seller', { hosts: [h1.url, h2.url], chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    assert.ok(await untilBuyer(b, s, chain, async () => (await buyerOp(b))?.state === 'awaiting_disclosure', 100), 'never funded | ' + logs.slice(-20).join(' | '))
    const listed = await readBuyer(b.mem.operatorKey(), [h1.url, h2.url], chain)
    assert.equal(noteItems(listed.items).length, 1, 'recovery note stays listed while the deal is open')
    assert.ok(await until([b.rt, s.rt], chain, async () => (await buyerOp(b))?.state === 'disclosure_verified', 100), 'no deal | ' + logs.slice(-20).join(' | '))
    const purchases = (await b.rt.store.operations({ role: 'buyer' })).filter(o => o.kind === 'purchase')
    assert.equal(purchases.length, 1, 'the note did not duplicate the operation')
    await drive([b.rt, s.rt], chain, 2)
    assert.equal((await readBuyer(b.mem.operatorKey(), [h1.url, h2.url], chain)).items.length, 0, 'note + delivered acked once settled')
  } finally { try { await b?.rt.stop() } catch { /* already stopped */ } try { await s?.rt.stop() } catch { /* already stopped */ } await h1.close(); await h2.close() }
})

test('bstack N: a note from a foreign key is ignored and still acked', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  let b!: A
  try {
    b = await mkagent('buyer', { hosts: [h1.url, h2.url], chain, logs, dir })
    await drive([b.rt], chain, 12)
    const { sendBox, MemoryTreeLedger, MemoryCarrierJournal, boxFundingLock, mintTree } = await import('@nektar/bstack')
    const { PrivateKey, Transaction, P2PKH, MerklePath } = await import('@bsv/sdk')
    const ghost = PrivateKey.fromRandom()
    const parent = new Transaction(); parent.addOutput({ satoshis: 100_000, lockingScript: new P2PKH().lock(ghost.toPublicKey().toAddress()) })
    const gTree = await mintTree({ key: ghost, funding: { tx: parent, vout: 0, satoshis: 100_000 }, n: 4, satsPerOutput: 1000, lock: await boxFundingLock(ghost) })
    gTree.merklePath = new MerklePath(1, [[{ offset: 0, hash: '00'.repeat(32) }, { offset: 1, hash: gTree.id('hex'), txid: true }]])
    const gl = new MemoryTreeLedger(); await gl.add(gTree, 4, 'bbox'); await gl.markProven(gTree.id('hex'))
    const forged = { type: 'note', v: 1, wantId: 'cc'.repeat(32), offerId: 'sale#fake', policyHash: 'deadbeef', want: { want: { wantId: 'cc'.repeat(32), pub: b.pub, net: NET }, sig: '00' }, offer: { so: { offer: { wantId: 'cc'.repeat(32), buyerPub: b.pub }, sig: '00' } }, escrow: { refundAfter: 1 }, utxo: { txid: 'dd'.repeat(32), vout: 0, satoshis: 500, script: '76a914' + '00'.repeat(20) + '88ac' }, escrowAction: 'x', amounts: { purchaseSats: 200, feeSats: 10, lockedSats: 210 } }
    await sendBox({ key: ghost, ledger: gl, journal: new MemoryCarrierJournal(), hosts: [h1.url, h2.url], office: OFFICE, to: b.pub, body: { market: forged } })
    await drive([b.rt], chain, 4)
    assert.equal((await b.rt.store.operations({ role: 'buyer' })).filter(o => o.kind === 'purchase').length, 0, 'forged note created no operation')
    assert.equal((await readBuyer(b.mem.operatorKey(), [h1.url, h2.url], chain)).items.length, 0, 'foreign envelope was acked anyway')
  } finally { try { await b?.rt.stop() } catch { /* already stopped */ } await h1.close(); await h2.close() }
})

test('bstack O: the recovery note fits the 16 KiB box content limit', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  let b!: A; let s!: A
  try {
    b = await mkagent('buyer', { hosts: [h1.url, h2.url], chain, logs, dir })
    s = await mkagent('seller', { hosts: [h1.url, h2.url], chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    assert.ok(await untilBuyer(b, s, chain, async () => (await buyerOp(b))?.state === 'awaiting_disclosure', 100), 'never funded | ' + logs.slice(-20).join(' | '))
    const items = (await readBuyer(b.mem.operatorKey(), [h1.url, h2.url], chain)).items
    const note = noteItems(items)[0]
    assert.ok(note, 'note is listed')
    const { Transaction } = await import('@bsv/sdk')
    const noteTxidDisplay = note.txid.match(/../g)!.reverse().join('') // BoxItem.txid is hash byte order
    let recordBytes = -1
    for (const beef of h1.raw().concat(h2.raw())) {
      try {
        const tx = Transaction.fromAtomicBEEF(beef)
        if (tx.id('hex') !== noteTxidDisplay) continue
        recordBytes = Math.max(recordBytes, ...tx.outputs.map(o => o.lockingScript.toHex().length / 2))
      } catch { /* not the note carrier */ }
    }
    assert.ok(recordBytes > 0, 'note carrier found on a host')
    assert.ok(recordBytes < 16384, `sealed envelope record is ${recordBytes} bytes — under the 16 KiB limit`)
    console.log(`    note: sealed envelope record is ${recordBytes} bytes (limit 16384)`)
  } finally { try { await b?.rt.stop() } catch { /* already stopped */ } try { await s?.rt.stop() } catch { /* already stopped */ } await h1.close(); await h2.close() }
})

// ── Phase 2: reconcile, late rejection, seller recovery ──────────────────────

// the escrow covenant script for a quoted sale — used to identify (and fake a
// transport failure on) exactly the escrow broadcast
const escrowScriptFor = async (s: A, buyerPub: string): Promise<string | undefined> => {
  const op = (await saleOps(s))[0]
  const offer = op?.data.offer as { cfg?: { price: string | number, refundAfter: string | number, maxFee: string | number } } | undefined
  if (!offer?.cfg) return undefined
  const { DisclosureSaleHost } = await import('@nektar/anchor')
  const { PublicKey } = await import('@bsv/sdk')
  const depositorPkh = Buffer.from(PublicKey.fromString(buyerPub).toHash() as number[]).toString('hex')
  const cfg = { ...offer.cfg, price: BigInt(offer.cfg.price), refundAfter: BigInt(offer.cfg.refundAfter), maxFee: BigInt(offer.cfg.maxFee) }
  return DisclosureSaleHost.escrowScript(cfg as never, depositorPkh)
}

// broadcast that lets the escrow onto the chain but reports a transport error
// — the wallet records submission_unknown and the op escalates to `uncertain`
const sabotageEscrowBroadcast = (b: A, script: string): { txid: () => string | undefined } => {
  let txid: string | undefined
  let armed = true
  const orig = b.mem.broadcastTx.bind(b.mem)
  b.mem.broadcastTx = async (tx: import('@bsv/sdk').Transaction) => {
    const hit = tx.outputs.some(o => o.lockingScript.toHex() === script)
    const r = await orig(tx)
    if (hit && armed) { armed = false; txid = tx.id('hex'); return { status: 'error', description: 'transport: connection reset' } }
    return r
  }
  return { txid: () => txid }
}

test('bstack P: uncertain + escrow observable — reconcile resumes the deal with no operator action', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  let b!: A; let s!: A
  try {
    b = await mkagent('buyer', { hosts: [h1.url], chain, logs, dir })
    s = await mkagent('seller', { hosts: [h1.url], chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    assert.ok(await until([b.rt, s.rt], chain, async () => (await saleOps(s))[0]?.state === 'quoted', 80), 'no quote | ' + logs.slice(-20).join(' | '))
    const script = (await escrowScriptFor(s, b.pub))!
    const sab = sabotageEscrowBroadcast(b, script)
    const all = [b.rt, s.rt]
    assert.ok(await until(all, chain, async () => (await buyerOp(b))?.state === 'disclosure_verified', 120), 'no deal | ' + logs.slice(-25).join(' | '))
    assert.ok(sab.txid() !== undefined, 'the sabotaged escrow broadcast happened')
    const bo = (await buyerOp(b))!
    const evs = await b.rt.store.events(bo.operationId)
    assert.ok(evs.some(e => e.cause === 'needs_human:unresolved_payment'), 'went through uncertain: ' + evs.map(e => e.cause).join(','))
    assert.ok(evs.some(e => e.cause === 'reconciled_escrow_observed'), 'reconcile observed the escrow: ' + evs.map(e => e.cause).join(','))
    assert.equal((await b.rt.store.approvals('pending')).length, 0, 'no approval left open')
    for (const r of all) await r.stop()
  } finally { try { await b?.rt.stop() } catch { /* already stopped */ } try { await s?.rt.stop() } catch { /* already stopped */ } await h1.close() }
})

test('bstack Q: uncertain + rejected escrow — budget released, approval canceled, funding_failed', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  let b!: A; let s!: A
  try {
    b = await mkagent('buyer', { hosts: [h1.url], chain, logs, dir })
    s = await mkagent('seller', { hosts: [h1.url], chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    assert.ok(await until([b.rt, s.rt], chain, async () => (await saleOps(s))[0]?.state === 'quoted', 80), 'no quote | ' + logs.slice(-20).join(' | '))
    const script = (await escrowScriptFor(s, b.pub))!
    const sab = sabotageEscrowBroadcast(b, script)
    assert.ok(await until([b.rt], chain, async () => (await buyerOp(b))?.state === 'uncertain', 80), 'never went uncertain | ' + logs.slice(-20).join(' | '))
    const txid = sab.txid() ?? (((await buyerOp(b))!.data.pendingEscrow as { utxo: { txid: string } }).utxo.txid)
    chain.reject(txid) // pushes 'rejected' → hint → immediate reconcile
    assert.ok(await until([b.rt], chain, async () => (await buyerOp(b))?.state === 'funding_failed', 40), 'not funding_failed | ' + logs.slice(-20).join(' | '))
    const bo = (await buyerOp(b))!
    const evs = await b.rt.store.events(bo.operationId)
    assert.ok(evs.some(e => e.cause === 'escrow_rejected'), 'cause escrow_rejected: ' + evs.map(e => e.cause).join(','))
    assert.equal((await b.rt.store.budgetStatus(b.policyHash)).lockedSats, 0, 'locked sats released')
    assert.equal((await b.rt.store.approvals('pending')).length, 0, 'no approval left open')
    await b.rt.stop(); await s.rt.stop()
  } finally { try { await b?.rt.stop() } catch { /* already stopped */ } try { await s?.rt.stop() } catch { /* already stopped */ } await h1.close() }
})

test('bstack R: escrow rejected while awaiting_disclosure — late rejection frees the budget before the refund height', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  let b!: A; let s!: A
  try {
    b = await mkagent('buyer', { hosts: [h1.url], chain, logs, dir })
    s = await mkagent('seller', { hosts: [h1.url], chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    assert.ok(await untilBuyer(b, s, chain, async () => (await buyerOp(b))?.state === 'awaiting_disclosure', 100), 'never funded | ' + logs.slice(-20).join(' | '))
    const escrowTxid = (await buyerOp(b))!.data.escrowTxid as string
    chain.reject(escrowTxid) // late rejection push — must not need the refund height
    assert.ok(await until([b.rt], chain, async () => (await buyerOp(b))?.state === 'funding_failed', 40), 'not funding_failed | ' + logs.slice(-20).join(' | '))
    const bo = (await buyerOp(b))!
    const evs = await b.rt.store.events(bo.operationId)
    assert.ok(evs.some(e => e.cause === 'escrow_rejected_late'), 'cause escrow_rejected_late: ' + evs.map(e => e.cause).join(','))
    assert.equal((await b.rt.store.budgetStatus(b.policyHash)).lockedSats, 0, 'committed reservation freed')
    await b.rt.stop(); await s.rt.stop()
  } finally { try { await b?.rt.stop() } catch { /* already stopped */ } try { await s?.rt.stop() } catch { /* already stopped */ } await h1.close() }
})

test('bstack S: disclosure rejected after completion_submitted — seller moves to completion_failed', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  let b!: A; let s!: A
  try {
    b = await mkagent('buyer', { hosts: [h1.url], chain, logs, dir })
    s = await mkagent('seller', { hosts: [h1.url], chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    // check right after each seller cycle, before the disclosure tx gets mined
    let submitted = false
    for (let i = 0; i < 120 && !submitted; i++) {
      clock += 500
      await b.rt.cycle()
      await s.rt.cycle()
      submitted = (await saleOps(s))[0]?.state === 'completion_submitted'
      if (!submitted) chain.mine(1)
    }
    assert.ok(submitted, 'never completion_submitted | ' + logs.slice(-20).join(' | '))
    const completeTxid = (await saleOps(s))[0]!.data.completeTxid as string
    chain.reject(completeTxid) // push → hint → reconcile → completion_failed
    assert.ok(await until([s.rt], chain, async () => (await saleOps(s))[0]?.state === 'completion_failed', 40), 'not completion_failed | ' + logs.slice(-20).join(' | '))
    const evs = await s.rt.store.events((await saleOps(s))[0]!.operationId)
    assert.ok(evs.some(e => e.cause === 'disclosure_rejected_late'), 'cause disclosure_rejected_late: ' + evs.map(e => e.cause).join(','))
    await b.rt.stop(); await s.rt.stop()
  } finally { try { await b?.rt.stop() } catch { /* already stopped */ } try { await s?.rt.stop() } catch { /* already stopped */ } await h1.close() }
})

test('bstack T: refused refund retries on reconcile — succeeds after one refusal; five refusals stay uncertain', async () => {
  for (const refuse of [1, 10]) {
    const chain = new MockChain(); chain.verifyScripts = true
    const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
    const logs: string[] = []
    const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
    let b!: A; let s!: A
    try {
      let left = refuse
      const memRef: { mem?: NektarMemory } = {}
      const call = (t2: string, a: any) => t2 === 'sale#refund' && left-- > 0 ? Promise.resolve({ accepted: false, reason: 'forced refusal' }) : callTool(memRef.mem!, t2, a)
      b = await mkagent('buyer', { hosts: [h1.url], chain, logs, dir, opts: { refundBlocks: 40 }, call })
      memRef.mem = b.mem
      s = await mkagent('seller', { hosts: [h1.url], chain, logs, dir, opts: { refundBlocks: 40 } })
      await sellerize(s, 200)
      await drive([b.rt, s.rt], chain, 12)
      await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
      assert.ok(await untilBuyer(b, s, chain, async () => (await buyerOp(b))?.state === 'awaiting_disclosure', 100), 'never funded | ' + logs.slice(-20).join(' | '))
      const refundAfter = Number(((await buyerOp(b))!.data.escrow as { refundAfter: number }).refundAfter)
      await s.rt.stop() // seller gone — the refund deadline will hit
      s = undefined as never
      await h1.close()
      while ((await b.mem.chainHeight()) < refundAfter) chain.mine(1)
      if (refuse === 1) {
        assert.ok(await until([b.rt], chain, async () => (await buyerOp(b))?.state === 'refunded', 80), 'not refunded | ' + logs.slice(-20).join(' | '))
        const evs = await b.rt.store.events((await buyerOp(b))!.operationId)
        assert.ok(evs.some(e => e.cause === 'needs_human:unresolved_settlement'), 'went through unresolved_settlement: ' + evs.map(e => e.cause).join(','))
        assert.ok(evs.some(e => e.cause === 'refund_broadcast'), 'refund broadcast on retry: ' + evs.map(e => e.cause).join(','))
        assert.equal((await b.rt.store.approvals('pending')).length, 0, 'no open approval after auto-refund')
      } else {
        assert.ok(await until([b.rt], chain, async () => (await buyerOp(b))?.state === 'uncertain', 60), 'never uncertain | ' + logs.slice(-20).join(' | '))
        for (let i = 0; i < 7; i++) { clock += 31_000; await b.rt.cycle(); chain.mine(1) }
        const op = (await buyerOp(b))!
        assert.equal(op.state, 'uncertain', 'five refusals leave the op for the operator')
        assert.equal(Number(op.data.refundTries), 5, 'exactly five retries were counted')
        assert.equal((await b.rt.store.approvals('pending')).length, 1, 'exactly one open approval')
      }
      await b.rt.stop()
    } finally { try { await b?.rt.stop() } catch { /* already stopped */ } try { await s?.rt.stop() } catch { /* already stopped */ } try { await h1.close() } catch { /* already closed */ } }
  }
})

test('bstack U: seller loses its database — the sellerNote rebuilds the quoted sale and the deal completes', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  let b!: A; let s!: A; let s2!: A
  try {
    b = await mkagent('buyer', { hosts: [h1.url, h2.url], chain, logs, dir })
    s = await mkagent('seller', { hosts: [h1.url, h2.url], chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    assert.ok(await until([b.rt, s.rt], chain, async () => (await saleOps(s))[0]?.state === 'quoted', 80), 'no quote | ' + logs.slice(-20).join(' | '))
    const origOp = (await saleOps(s))[0]!
    // measure the sealed sellerNote on the wire — must fit the 12 KiB budget
    await drive([b.rt, s.rt], chain, 2)
    const items = (await readBuyer(s.mem.operatorKey(), [h1.url, h2.url], chain)).items
    const note = items.find(i => (i.body as { market?: { type?: string } }).market?.type === 'sellerNote')
    assert.ok(note, 'sellerNote is listed in the seller inbox')
    const { Transaction } = await import('@bsv/sdk')
    const noteTxidDisplay = note.txid.match(/../g)!.reverse().join('')
    let recordBytes = -1
    for (const beef of h1.raw().concat(h2.raw())) {
      try {
        const tx = Transaction.fromAtomicBEEF(beef)
        if (tx.id('hex') !== noteTxidDisplay) continue
        recordBytes = Math.max(recordBytes, ...tx.outputs.map(o => o.lockingScript.toHex().length / 2))
      } catch { /* not the note carrier */ }
    }
    assert.ok(recordBytes > 0, 'sellerNote carrier found on a host')
    console.log(`    sellerNote: sealed envelope record is ${recordBytes} bytes (budget 12288)`)
    assert.ok(recordBytes < 12 * 1024, `sealed sellerNote is ${recordBytes} bytes — over the 12 KiB budget`)
    // wipe the seller and respawn on the same key
    await s.rt.stop()
    rmSync(join(dir, 'seller'), { recursive: true, force: true })
    s2 = await respawn(s, { dir, name: 'seller', hosts: [h1.url, h2.url], logs, chain })
    s = undefined as never
    await s2.rt.engine.enableSeller(s2.policyHash)
    assert.ok(await until([b.rt, s2.rt], chain, async () => (await buyerOp(b))?.state === 'disclosure_verified', 120), 'no deal | ' + logs.slice(-25).join(' | '))
    const ops = await saleOps(s2)
    assert.equal(ops.length, 1, 'exactly one sale operation after restore')
    assert.equal(ops[0]!.operationId, origOp.operationId, 'restored with the original operationId')
    assert.equal(ops[0]!.data.restored, true)
    assert.ok(COMPLETE.includes(ops[0]!.state), `restored sale reached ${ops[0]!.state}`)
    await drive([b.rt, s2.rt], chain, 2)
    assert.equal((await readBuyer(s2.mem.operatorKey(), [h1.url, h2.url], chain)).items.length, 0, 'seller inbox empty after settlement')
    for (const r of [b.rt, s2.rt]) await r.stop()
  } finally { try { await b?.rt.stop() } catch { /* already stopped */ } try { await s?.rt.stop() } catch { /* already stopped */ } try { await s2?.rt.stop() } catch { /* already stopped */ } await h1.close(); await h2.close() }
})

test('bstack V: delivered collected but not yet processed — the held envelope survives a buyer database loss', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  let b!: A; let s!: A; let b2!: A
  try {
    b = await mkagent('buyer', { hosts: [h1.url, h2.url], chain, logs, dir })
    s = await mkagent('seller', { hosts: [h1.url, h2.url], chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    assert.ok(await untilBuyer(b, s, chain, async () => (await buyerOp(b))?.state === 'awaiting_disclosure', 100), 'never funded | ' + logs.slice(-20).join(' | '))
    // the seller delivers; the buyer's next cycle collects + holds the envelope
    // but its tick dies before processing it — the doomed store loses the inbox row
    assert.ok(await until([s.rt], chain, async () => (await saleOps(s))[0]?.state === 'completion_submitted', 100), 'never delivered | ' + logs.slice(-20).join(' | '))
    const engine = b.rt.engine as unknown as { tick: () => Promise<void> }
    const origTick = engine.tick.bind(engine)
    let fail = true
    engine.tick = async () => { if (fail) { fail = false; throw new Error('simulated crash after collect') } return origTick() }
    await b.rt.cycle() // collect + ack-hold ran; tick threw
    await b.rt.stop()
    rmSync(join(dir, 'buyer'), { recursive: true, force: true })
    b2 = await respawn(b, { dir, name: 'buyer', hosts: [h1.url, h2.url], logs, chain })
    b = undefined as never
    assert.ok(await until([b2.rt, s.rt], chain, async () => (await buyerOp(b2))?.state === 'disclosure_verified', 120), 'delivery never verified | ' + logs.slice(-25).join(' | '))
    const bo = (await buyerOp(b2))!
    assert.equal(bo.data.restored, true, 'the note rebuilt the purchase first')
    assert.equal((bo.data.result as { verified?: boolean }).verified, true)
    await drive([b2.rt, s.rt], chain, 2)
    assert.equal((await readBuyer(b2.mem.operatorKey(), [h1.url, h2.url], chain)).items.length, 0, 'note + delivered acked once settled')
    for (const r of [b2.rt, s.rt]) await r.stop()
  } finally { try { await b?.rt.stop() } catch { /* already stopped */ } try { await s?.rt.stop() } catch { /* already stopped */ } try { await b2?.rt.stop() } catch { /* already stopped */ } await h1.close(); await h2.close() }
})

test('bstack W: a sellerNote from a foreign key is ignored and still acked', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE })
  let s!: A
  try {
    s = await mkagent('seller', { hosts: [h1.url, h2.url], chain, logs, dir })
    await drive([s.rt], chain, 4)
    const { sendBox, MemoryTreeLedger, MemoryCarrierJournal, boxFundingLock, mintTree, signWant } = await import('@nektar/bstack')
    const { PrivateKey, Transaction, P2PKH, MerklePath } = await import('@bsv/sdk')
    const ghost = PrivateKey.fromRandom()
    const parent = new Transaction(); parent.addOutput({ satoshis: 100_000, lockingScript: new P2PKH().lock(ghost.toPublicKey().toAddress()) })
    const gTree = await mintTree({ key: ghost, funding: { tx: parent, vout: 0, satoshis: 100_000 }, n: 4, satsPerOutput: 1000, lock: await boxFundingLock(ghost) })
    gTree.merklePath = new MerklePath(1, [[{ offset: 0, hash: '00'.repeat(32) }, { offset: 1, hash: gTree.id('hex'), txid: true }]])
    const gl = new MemoryTreeLedger(); await gl.add(gTree, 4, 'bbox'); await gl.markProven(gTree.id('hex'))
    const sw = signWant(ghost, { domains: ['libfoo'], maxSats: 500, ts: Date.now(), exp: Date.now() + 3_600_000, net: NET })
    const forged = { type: 'sellerNote', v: 1, wantId: sw.want.wantId, offerId: 'fake-offer', policyHash: 'deadbeef', want: sw, shard: 'guide', seq: 1, priceSats: 200, offer: { cfg: {}, sellerPub: ghost.toPublicKey().toString() }, dueHeight: 100 }
    await sendBox({ key: ghost, ledger: gl, journal: new MemoryCarrierJournal(), hosts: [h1.url, h2.url], office: OFFICE, to: s.pub, body: { market: forged } })
    await drive([s.rt], chain, 4)
    assert.equal((await saleOps(s)).length, 0, 'forged sellerNote created no sale operation')
    const items = (await readBuyer(s.mem.operatorKey(), [h1.url, h2.url], chain)).items
    assert.equal(items.filter(i => (i.body as { market?: { type?: string } }).market?.type === 'sellerNote').length, 0, 'foreign sellerNote was acked anyway')
    await s.rt.stop()
  } finally { try { await s?.rt.stop() } catch { /* already stopped */ } await h1.close(); await h2.close() }
})

const LOG_STREAM = 'nektar_abcdefghij'
const journalLines = async (hosts: MinihostHandle[], chain: MockChain, stream: string, producer: string) => {
  const { readLog } = await import('@nektar/bstack')
  return await readLog({ hosts: hosts.map(h => h.url), stream, producer, tracker: trackerFor(chain) })
}
const jTxid = async (dir: string, name: string, seq: number): Promise<string> => {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: join(dir, name, 'automation.sqlite') }, useNullAsDefault: true })
  try { const r = await kx('bstack_carriers').where({ artifact_hash: `log:${LOG_STREAM}:${seq}` }).first(); return String(r.txid) } finally { await kx.destroy() }
}
const dealJournal = async (a: A, opId: string): Promise<Record<string, any>> => {
  const op = await a.rt.store.operation(opId)
  return (await a.rt.dealJournal?.(op!)) ?? {}
}

test('bstack X: a full deal journals every state on both streams — complete for the counterparty, nothing sensitive in the lines', { timeout: 300_000 }, async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE, logStream: LOG_STREAM })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE, logStream: LOG_STREAM })
  let b!: A; let s!: A
  try {
    b = await mkagent('buyer', { hosts: [h1.url, h2.url], chain, logs, dir })
    s = await mkagent('seller', { hosts: [h1.url, h2.url], chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    const done = async () => (await buyerOp(b))?.state === 'disclosure_verified' && COMPLETE.includes((await saleOps(s))[0]?.state ?? '')
    assert.ok(await until([b.rt, s.rt], chain, done, 120), 'no deal | ' + logs.slice(-25).join(' | '))
    await drive([b.rt, s.rt], chain, 3) // let the journal append settle
    const bo = (await buyerOp(b))!
    const so = (await saleOps(s))[0]!

    // the buyer sees the seller's stream for this deal: complete, with the
    // seller-side states
    const bj = await dealJournal(b, bo.operationId)
    assert.equal((bj.own as { stream: string }).stream, LOG_STREAM)
    const cp = bj.counterparty as { complete: boolean, entries: Array<{ n: number, state: string }>, gaps: unknown[] }
    assert.equal(cp.complete, true, 'buyer sees a complete seller journal | ' + JSON.stringify(bj).slice(0, 400))
    const cpStates = cp.entries.map(e => e.state)
    assert.ok(cpStates.includes('quoted'), 'seller journal has quoted | ' + cpStates.join(','))
    assert.ok(cpStates.includes('completion_submitted'), 'seller journal has completion_submitted | ' + cpStates.join(','))

    // the seller sees the buyer's stream: collecting -> disclosure_verified
    const sj = await dealJournal(s, so.operationId)
    const scp = sj.counterparty as { complete: boolean, entries: Array<{ n: number, state: string }> }
    assert.equal(scp.complete, true, 'seller sees a complete buyer journal | ' + JSON.stringify(sj).slice(0, 400))
    const sStates = scp.entries.map(e => e.state)
    for (const st of ['collecting', 'awaiting_disclosure', 'disclosure_verified']) assert.ok(sStates.includes(st), `buyer journal has ${st} | ` + sStates.join(','))

    // no journal batch leaks the finding, the question or any wrapped secret:
    // decode every tm_log carrier the hosts hold and scan its lines
    const { Transaction } = await import('@bsv/sdk')
    const { readPushDrop } = await import('@lightwebinc/blogs-host/dist/script.js')
    const { decodeBatch } = await import('@lightwebinc/blogs-host/dist/index.js')
    let checked = 0
    for (const beef of h1.raw().concat(h2.raw())) {
      let tx: InstanceType<typeof Transaction>
      try { tx = Transaction.fromAtomicBEEF(beef) } catch { continue }
      for (const o of tx.outputs) {
        const pd = readPushDrop(new Uint8Array(o.lockingScript.toBinary()))
        if (pd === undefined || pd.fields.length !== 2) continue
        let b2: ReturnType<typeof decodeBatch>
        try { b2 = decodeBatch(pd.fields[0]!) } catch { continue }
        if (b2.stream !== LOG_STREAM || b2.content === undefined) continue
        checked++
        const text = new TextDecoder().decode(b2.content)
        assert.ok(!text.includes('"wrapped"'), 'journal line mentions wrapped')
        assert.ok(!text.includes(FINDING), 'journal line leaks the finding text')
        assert.ok(!text.includes('libfoo 3.0 migration?'), 'journal line leaks the question')
      }
    }
    assert.ok(checked >= 2, `decoded ${checked} journal batches`)
  } finally { try { await b?.rt.stop() } catch { /* already stopped */ } try { await s?.rt.stop() } catch { /* already stopped */ } await h1.close(); await h2.close() }
})

test('bstack Y: a journal batch dropped by every host shows the gap — complete:false', { timeout: 300_000 }, async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  const dropped = new Set<string>()
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE, logStream: LOG_STREAM, drop: txid => dropped.has(txid) })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE, logStream: LOG_STREAM, drop: txid => dropped.has(txid) })
  let b!: A; let s!: A
  try {
    b = await mkagent('buyer', { hosts: [h1.url, h2.url], chain, logs, dir })
    s = await mkagent('seller', { hosts: [h1.url, h2.url], chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    const done = async () => (await buyerOp(b))?.state === 'disclosure_verified' && COMPLETE.includes((await saleOps(s))[0]?.state ?? '')
    assert.ok(await until([b.rt, s.rt], chain, done, 120), 'no deal | ' + logs.slice(-25).join(' | '))
    await drive([b.rt, s.rt], chain, 3)
    const bo = (await buyerOp(b))!

    // find the seller batch whose lines mention this deal and drop it everywhere
    const sj = await dealJournal(b, bo.operationId)
    const seqs = [...new Set((sj.counterparty as { entries: Array<{ seq: number }> }).entries.map(e => e.seq))]
    assert.ok(seqs.length >= 1, 'deal entries exist in the seller journal')
    const mid = seqs[Math.floor(seqs.length / 2)]!
    dropped.add(await jTxid(dir, 'seller', mid))
    const bj = await dealJournal(b, bo.operationId)
    const cp = bj.counterparty as { complete: boolean, gaps: Array<{ from: number, to: number }> }
    assert.equal(cp.complete, false, 'the withheld batch breaks completeness | ' + JSON.stringify(bj).slice(0, 400))
    assert.ok(cp.gaps.some(g => g.from <= mid && g.to >= mid), `gap at seq ${mid} listed | ` + JSON.stringify(cp.gaps))
  } finally { try { await b?.rt.stop() } catch { /* already stopped */ } try { await s?.rt.stop() } catch { /* already stopped */ } await h1.close(); await h2.close() }
})

test('bstack Z: restart between tick and append — events land exactly once', { timeout: 300_000 }, async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const logs: string[] = []
  let armed = false
  const logOnly = (topics?: string[]) => armed && (topics ?? []).some(t => t.startsWith('tm_log_'))
  const h1 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE, logStream: LOG_STREAM, failNext: logOnly })
  const h2 = await minihost([NET], { now: () => clock, tracker: trackerFor(chain), office: OFFICE, logStream: LOG_STREAM, failNext: logOnly })
  let b!: A; let s!: A; let b2!: A
  try {
    b = await mkagent('buyer', { hosts: [h1.url, h2.url], chain, logs, dir })
    s = await mkagent('seller', { hosts: [h1.url, h2.url], chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 12)
    await b.rt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }, b.policyHash)
    await drive([b.rt, s.rt], chain, 2) // a first batch lands, then the append stalls
    armed = true
    assert.ok(await until([b.rt, s.rt], chain, async () => (await buyerOp(b))?.state === 'disclosure_verified', 120), 'no deal | ' + logs.slice(-25).join(' | '))
    await drive([b.rt], chain, 3)
    const stalled = await journalLines([h1, h2], chain, LOG_STREAM, b.pub)
    const stalledLines = stalled.batches.flatMap(x => x.lines).length
    // restart on the same database — nothing lost, the cursor just lagged
    await b.rt.stop()
    b2 = await respawn(b, { dir, name: 'buyer', hosts: [h1.url, h2.url], logs, chain })
    armed = false
    await drive([b2.rt], chain, 4)
    const read = await journalLines([h1, h2], chain, LOG_STREAM, b.pub)
    const all = read.batches.flatMap(x => x.lines.map(l => JSON.parse(l) as { op: string, n: number }))
    const bo = (await buyerOp(b2))!
    const mine = all.filter(l => l.op === bo.operationId).map(l => l.n).sort((a, z) => a - z)
    assert.ok(mine.length > 0, 'the deal\'s events were appended after the restart')
    assert.equal(new Set(mine).size, mine.length, `duplicate n values: ${mine.join(',')}`)
    assert.deepEqual(mine, Array.from({ length: mine.length }, (_, i) => i + 1), 'n values contiguous from 1')
    assert.ok(read.batches.flatMap(x => x.lines).length > stalledLines, 'the queued events caught up')
  } finally { try { await b?.rt.stop() } catch { /* already stopped */ } try { await b2?.rt.stop() } catch { /* already stopped */ } try { await s?.rt.stop() } catch { /* already stopped */ } await h1.close(); await h2.close() }
})
