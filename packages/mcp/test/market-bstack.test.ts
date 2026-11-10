import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync } from 'node:fs'
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
let clock = 1_800_000_000_000

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
  scoring: { minimumScoreBps: 0, weights: { ...DEFAULT_SCORE_WEIGHTS }, collectMs: 1000 },
  seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: 5, sellableShards: ['guide'] },
  disclosure: { allowPublic: false },
  ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 200, timeoutMs: 5000 },
  sandbox: { mode: 'sandbox-only' }
})

interface A { mem: NektarMemory, rt: Automation, pub: string, policyHash: string, name: string }
interface MkOpts { hosts: string[], msgs?: ReturnType<typeof bus>, chain: MockChain, logs: string[], dir: string, opts?: Partial<import('../src/automation/market.js').MarketOptions> }
const mkagent = async (name: string, o: MkOpts): Promise<A> => {
  const funder = new MockFunder(); funder.topup(30)
  for (const t of funder.parents) o.chain.registerParent(t)
  const mem = new NektarMemory(o.chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 1 }, modelId: 'test-model-1' })
  const pub = mem.operatorKey().toPublicKey().toString()
  const rt = await openAutomation(mem, { network: NET, dataDir: join(o.dir, name), rendezvousUrls: [], marketHosts: o.hosts, intervalMs: 0, call: (t2, a) => callTool(mem, t2, a), log: l => o.logs.push(`${name}:${l}`), ai: new ScriptedExecutor(), now: () => clock, messages: o.msgs?.for(pub), opts: o.opts })
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
const trackerFor = (chain: MockChain) => ({
  isValidRootForHeight: (r: string, h: number) => chain.isValidRootForHeight(r, h),
  currentHeight: () => chain.currentHeight()
})
const count = async (dir: string, name: string, table: string): Promise<number> => {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: join(dir, name, 'automation.sqlite') }, useNullAsDefault: true })
  try { const r = await kx(table).count('* as n').first(); return Number((r as { n: number }).n) } finally { await kx.destroy() }
}
const buyerOp = async (b: A) => (await b.rt.store.operations({ role: 'buyer' })).find(o => o.kind === 'purchase')
const saleOps = async (s: A) => (await s.rt.store.operations({ role: 'seller' })).filter(o => o.kind === 'sale')
const COMPLETE = ['completion_submitted', 'settled', 'useful']

test('bstack A: buyer+seller over two minihosts — journal+slot conservation, dedupe, both hosts hold', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const msgs = bus(); const logs: string[] = []
  let h2Failed = false
  const h1 = await minihost([NET], { tracker: trackerFor(chain) })
  const h2 = await minihost([NET], { tracker: trackerFor(chain), failNext: () => { if (!h2Failed) { h2Failed = true; return true } return false } })
  try {
    const b = await mkagent('buyer', { hosts: [h1.url, h2.url], msgs, chain, logs, dir })
    const s = await mkagent('seller', { hosts: [h1.url, h2.url], msgs, chain, logs, dir })
    await sellerize(s, 200)
    await drive([b.rt, s.rt], chain, 8)
    const slotsBefore = (await count(dir, 'buyer', 'bstack_trees')) * 16 - (await count(dir, 'buyer', 'bstack_tree_outputs'))
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
    // journal: exactly one carrier row total, exactly one slot consumed
    assert.equal(await count(dir, 'buyer', 'bstack_carriers'), 1)
    const slotsAfter = (await count(dir, 'buyer', 'bstack_trees')) * 16 - (await count(dir, 'buyer', 'bstack_tree_outputs'))
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
  const h1 = await minihost([NET], { tracker: trackerFor(chain) })
  const h2 = await minihost([NET], { tracker: trackerFor(chain) })
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
  const h1 = await minihost([NET], { tracker: trackerFor(chain) })
  const h2 = await minihost([NET], { tracker: trackerFor(chain), drop: (txid) => txid === dropTxid })
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
  const h1 = await minihost([NET], { tracker: trackerFor(chain) })
  try {
    const funderB = new MockFunder(); funderB.topup(30)
    for (const t of funderB.parents) chain.registerParent(t)
    const memB = new NektarMemory(chain, funderB, funderB.keyPair, { batchPolicy: { maxLeaves: 1 }, modelId: 'test-model-1' })
    const pubB = memB.operatorKey().toPublicKey().toString()
    const open = async () => await openAutomation(memB, { network: NET, dataDir: join(dir, 'buyer'), rendezvousUrls: [], marketHosts: [h1.url], intervalMs: 0, call: (t2, a) => callTool(memB, t2, a), log: l => logs.push('B:' + l), ai: new ScriptedExecutor(), now: () => clock, messages: msgs.for(pubB) })
    let bRt = await open()
    const policyHash = await bRt.store.approvePolicy(policyFor(pubB), 'test:operator')
    const s = await mkagent('seller', { hosts: [h1.url], msgs, chain, logs, dir })
    await sellerize(s, 200)
    await drive([bRt, s.rt], chain, 8)
    await bRt.engine.publishWant({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 500 }, policyHash)
    await drive([bRt, s.rt], chain, 6)
    const carriersBefore = await count(dir, 'buyer', 'bstack_carriers')
    const slotsBefore = (await count(dir, 'buyer', 'bstack_trees')) * 16 - (await count(dir, 'buyer', 'bstack_tree_outputs'))
    await bRt.stop()
    bRt = await open()
    const all = [bRt, s.rt]
    assert.ok(await until(all, chain, async () => (await buyerOp({ rt: bRt } as A))?.state === 'disclosure_verified', 100), 'logs: ' + logs.slice(-20).join(' | '))
    const bo = (await bRt.store.operations({ role: 'buyer' })).find(o => o.kind === 'purchase')
    assert.equal(bo?.state, 'disclosure_verified')
    assert.equal(await count(dir, 'buyer', 'bstack_carriers'), carriersBefore, 'no second want carrier after restart')
    const slotsAfter = (await count(dir, 'buyer', 'bstack_trees')) * 16 - (await count(dir, 'buyer', 'bstack_tree_outputs'))
    assert.equal(slotsAfter - slotsBefore, 0, 'zero funding slots consumed for the want after restart')
    for (const r of all) await r.stop()
  } finally { await h1.close() }
})

test('bstack E: hosts down after escrow funded — chain advance still refunds the buyer', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const dir = mkdtempSync(join(tmpdir(), 'nektar-bstack-'))
  const msgs = bus(); const logs: string[] = []
  const h1 = await minihost([NET], { tracker: trackerFor(chain) })
  try {
    const b = await mkagent('buyer', { hosts: [h1.url], msgs, chain, logs, dir, opts: { refundBlocks: 6 } })
    const s = await mkagent('seller', { hosts: [h1.url], msgs, chain, logs, dir, opts: { refundBlocks: 6 } })
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
  const h1 = await minihost([NET], { tracker: trackerFor(chain), office })
  const h2 = await minihost([NET], { tracker: trackerFor(chain), office })
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
