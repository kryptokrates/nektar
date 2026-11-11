// Autonomous marketplace scenarios — mock-chain lane, real everything else:
// two independent in-process overlay hosts (test double running the real
// nektar module and the real bbox admission codec), bstack carriers for
// wants/offers, bbox envelopes for quote/accept/delivered, the production
// automation runtime per agent, operator CLI for policy approval, independent
// evidence verification. A shared MockChain stands in for the network (real
// covenant script evaluation, no funds). Honest labels: this lane proves the
// workflow; public-network confirmation is a separate lane.
import { test, before, after } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { mkdtempSync, rmSync, writeFileSync, mkdirSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { setTimeout as delay } from 'node:timers/promises'
import { P2PKH } from '@bsv/sdk'
import { MockChain, MockFunder } from '../packages/chain/dist/src/index.js'
import { NektarMemory } from '../packages/mcp/dist/src/memory.js'
import { callTool } from '../packages/mcp/dist/src/server.js'
import { openAutomation } from '../packages/mcp/dist/src/automation/runtime.js'
import { collectTradeEvidence, verifyTradeEvidence } from '../packages/mcp/dist/src/automation/evidence.js'
import { parsePatch, evaluatePatch } from '../packages/mcp/dist/src/automation/sandbox.js'

process.env.NEKTAR_PAID_DISCLOSURE = '1'
const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const tmp = mkdtempSync(join(tmpdir(), 'nektar-scenarios-'))
let H1, H2
const FINDING = 'libfoo 3.0 renamed connect() to open(); callers must switch.\n```nektar-patch\n{"edits":[{"file":"src/app.js","find":"libfoo.connect(","replace":"libfoo.open("}]}\n```'

const chain = new MockChain(); chain.verifyScripts = true
const agents = []

async function agent (name, hosts, { seller, publicDisclosure = false, bond } = {}) {
  const funder = new MockFunder(); funder.topup(30)
  for (const t of funder.parents) chain.registerParent(t)
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 1 }, modelId: 'scripted', network: 'mock', ...(bond ? { bond } : {}) })
  const dataDir = join(tmp, name); mkdirSync(dataDir, { recursive: true })
  const pub = mem.operatorKey().toPublicKey().toString()
  const policy = {
    v: 1, policyId: `${name}-policy`, network: 'mock', agentPub: pub, roles: ['buyer', 'seller'], notBefore: 0, notAfter: 4102444800000, domains: ['libfoo'],
    limits: { perPurchaseSats: 500, dailyPurchaseSats: 1500, perTxFeeSats: 600, dailyFeeSats: 3000, runTotalSats: 6000, maxLockedSats: 1200, maxUnconfirmedSats: 1200, maxUnconfirmedCount: 3 },
    scoring: { minimumScoreBps: 0, weights: { relevance: 3000, provenance: 2500, history: 1500, price: 2000, latency: 1000 }, collectMs: 12000 },
    seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: 10, sellableShards: ['guide'] },
    disclosure: { allowPublic: publicDisclosure },
    ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 200, timeoutMs: 5000 },
    sandbox: { mode: 'sandbox-only' }
  }
  const au = await openAutomation(mem, { network: 'mock', dataDir, marketHosts: hosts, intervalMs: 0, opts: { refundBlocks: 10000, offerTtlBlocks: 10000 }, call: (tool, args) => callTool(mem, tool, args) })
  mem.automation = au
  // the human step, once: the operator approves the policy from the CLI
  const file = join(dataDir, 'policy.json'); writeFileSync(file, JSON.stringify(policy))
  const { policyHash } = JSON.parse(execFileSync(process.execPath, [join(root, 'tools/automation.mjs'), '--data', dataDir, '--json', 'policy', 'approve', file], { encoding: 'utf8' }))
  if (seller) {
    await mem.remember('guide', FINDING)
    chain.mine(1)
    for (const l of mem.anchorLinks('guide')) mem.confirmAnchor('guide', l.txid)
    await mem.describeShard('guide', { domains: ['libfoo'], offers: [{ kind: 'record', priceSats: seller }] })
    await callTool(mem, 'automation', { op: 'enableSeller', policyHash })
  }
  const a = { name, mem, au, pub, policyHash, dataDir }
  agents.push(a)
  return a
}
const stateOf = async (a, operationId) => (await callTool(a.mem, 'automation', { op: 'status', operationId })).operation
async function until (who, fn, ms = 90_000) {
  const end = Date.now() + ms
  for (;;) {
    for (const a of who) await a.au.cycle()
    chain.mine(1)
    const v = await fn()
    if (v) return v
    if (Date.now() > end) throw new Error('scenario timed out')
    await delay(300)
  }
}

const hostTracker = { isValidRootForHeight: (r, h) => chain.isValidRootForHeight(r, h), currentHeight: () => chain.currentHeight() }
before(async () => {
  const { minihost } = await import('../packages/bstack/dist/src/testing.js')
  const { DEFAULT_OFFICE } = await import('../packages/bstack/dist/src/index.js')
  H1 = await minihost(['mock'], { tracker: hostTracker, office: DEFAULT_OFFICE })
  H2 = await minihost(['mock'], { tracker: hostTracker, office: DEFAULT_OFFICE })
})
after(async () => {
  for (const a of agents) await a.au.stop().catch(() => {})
  if (H1) await H1.close(); if (H2) await H2.close()
  await delay(300)
  try { rmSync(tmp, { recursive: true, force: true }) } catch {}
})

test('S1: competing sellers across two independent hosts → autonomous purchase → verified disclosure → sandboxed fix', async (t) => {
  const alice = await agent('alice', [H1.url], { seller: 450 })
  const ben = await agent('ben', [H2.url], { seller: 300 })
  const cara = await agent('cara', [H2.url], { seller: 200 })
  const bob = await agent('bob', [H1.url, H2.url])
  const all = [alice, ben, cara, bob]
  const { operationId, wantId } = await callTool(bob.mem, 'automation', { op: 'publishWant', policyHash: bob.policyHash, domains: ['libfoo'], question: 'build fails after libfoo 3.0 upgrade: libfoo.connect is not a function', maxSats: 500 })
  // nothing below is a marketplace tool call — the runtimes do the rest
  const op = await until(all, async () => { const o = await stateOf(bob, operationId); return ['disclosure_verified', 'expired', 'denied', 'uncertain', 'disputed', 'funding_failed'].includes(o.state) ? o : undefined })
  assert.equal(op.state, 'disclosure_verified', JSON.stringify({ lastError: op.lastError, ranking: op.data.ranking }))
  const offers = Object.values(op.data.offers)
  assert.equal(offers.length, 3, 'offers from sellers on BOTH hosts reached the buyer')
  assert.deepEqual(offers.map(o => o.so.offer.priceSats).sort((a, b) => a - b), [200, 300, 450])
  assert.equal(op.data.selected.key.split(':')[0], cara.pub, 'policy score picks the 200-sat offer (equal quality)')
  assert.equal(op.data.result.text, FINDING)
  assert.equal(op.data.result.verified, true, 'payload hashes to the anchored leaf the seller committed to')
  // public feeds on both hosts never carried the reveal credential
  const sealed = (await cara.au.store.operations({ role: 'seller' })).find(o => o.kind === 'sale').data.offer.wrapped
  for (const h of [H1, H2]) {
    const raw = Buffer.concat(h.raw().map(b => Buffer.from(b)))
    assert.ok(raw.includes(wantId), `${h.url} carries the want`)
    assert.ok(!raw.includes(sealed), `${h.url} never saw the reveal credential`)
  }
  // budget: 200 purchase, nothing left locked, no human prompt
  const budget = await callTool(bob.mem, 'automation', { op: 'budget', policyHash: bob.policyHash })
  assert.equal(budget.purchaseTodaySats, 200); assert.equal(budget.lockedSats, 0)
  assert.deepEqual((await callTool(bob.mem, 'automation', { op: 'approvals' })).approvals, [])
  // independent evidence check from captured artifacts only
  const evidence = await collectTradeEvidence(bob.au.store, await bob.au.store.operation(operationId), txid => bob.mem.fetchTx(txid))
  const verdict = verifyTradeEvidence(JSON.parse(JSON.stringify(evidence)))
  assert.ok(verdict.ok, JSON.stringify(verdict.checks.filter(c => !c.ok)))
  assert.equal(evidence.claims.semantic, 'not_asserted')
  const forged = JSON.parse(JSON.stringify(evidence)); forged.offers.find(o => `${o.offer.pub}:${o.offer.offerId}` === forged.selectedOfferKey).offer.priceSats = 1
  assert.equal(verifyTradeEvidence(forged).ok, false, 'tampered evidence fails verification')
  // usefulness is a separate question: apply the purchased finding in isolation
  const patch = parsePatch(op.data.result.text)
  const sandbox = await evaluatePatch({ fixtureDir: join(root, 'test/fixtures/libfoo-break'), hiddenTestsDir: join(root, 'test/fixtures/libfoo-break-hidden'), allowedPaths: ['src'], testCommand: ['node', '--test', 'test/build.test.js'], hiddenTestCommand: ['node', '--test', '/hidden/regress.test.js'], image: 'node:22-alpine@sha256:0e1754758c2df4cfa3e68d59e2e1b7374bb7fd9ea6328eaaacf15cc934af8202', timeoutMs: 120_000, maxPatchBytes: 4096 }, patch)
  if (sandbox.outcome === 'BLOCKED') { t.diagnostic(`sandbox BLOCKED — ${sandbox.reason}; purchase + verification passed, usefulness NOT evaluated`); return }
  assert.equal(sandbox.outcome, 'PASS', sandbox.reason)
  await bob.au.engine.markUsefulness(operationId, true, { sandbox: { outcome: sandbox.outcome, isolation: sandbox.isolation } })
  assert.equal((await stateOf(bob, operationId)).state, 'useful')
})

test('S3: one buyer pays for a public disclosure; a third agent reads and verifies it for free', async () => {
  const pat = await agent('pat', [H2.url], { seller: 150, publicDisclosure: true })
  const dee = await agent('dee', [H1.url, H2.url])
  const reader = await agent('reader', [H1.url, H2.url])
  const { operationId } = await callTool(dee.mem, 'automation', { op: 'publishWant', policyHash: dee.policyHash, domains: ['libfoo'], question: 'libfoo 3.0 connect open rename', maxSats: 180 })
  const op = await until([pat, dee], async () => { const o = await stateOf(dee, operationId); return ['disclosure_verified', 'expired', 'denied', 'uncertain', 'disputed', 'funding_failed'].includes(o.state) ? o : undefined })
  assert.equal(op.state, 'disclosure_verified', op.lastError)
  assert.equal(op.data.selected.key.split(':')[0], pat.pub, 'only the offer priced under this want\'s ceiling qualified')
  // the reader never bought anything: it finds the public listing on its own host and reads the reveal from the chain
  const { readOffers } = await import('../packages/bstack/dist/src/index.js')
  // pat only submits to H2 — the reader queries every host it knows
  const lres = await readOffers([H1.url, H2.url], { net: 'mock', wantId: op.data.want.want.wantId, buyerPub: dee.pub }, { tracker: hostTracker, net: 'mock' })
  const listing = lres.items.find(i => i.payload.body.offer.pub === pat.pub)?.payload.body
  assert.ok(listing, 'the public listing is readable from the overlay hosts')
  const reveal = await reader.mem.fetchTx(op.data.result.disclosureTxid)
  const read = await callTool(reader.mem, 'sale#claim', { txHex: reveal.toHex(), hWrapped: listing.offer.offer.cfg.hWrapped, offerMode: 'fragment', commitment: { leaf: listing.offer.leaf, hdp: listing.offer.hdp } })
  assert.equal(read.mode, 'public')
  assert.equal(read.verified, true, 'the free reader verifies the value against the publicly committed leaf')
  assert.equal((await reader.au.store.budgetStatus(reader.policyHash)).runTotalSats, 0, 'the reader spent nothing')
  // the private S1 sale stays private: the same read against a private reveal yields no plaintext
  const cara = agents.find(a => a.name === 'cara')
  const privSale = (await cara.au.store.operations({ role: 'seller' })).find(o => o.kind === 'sale' && o.state !== 'quoted')
  const privTx = await reader.mem.fetchTx(privSale.data.completeTxid)
  const leak = await callTool(reader.mem, 'sale#claim', { txHex: privTx.toHex(), hWrapped: privSale.data.offer.cfg.hWrapped, sellerPub: cara.pub }).catch(e => ({ error: e.message }))
  assert.ok(leak.error || !Buffer.from(leak.valueHex ?? '', 'hex').toString('utf8').includes('nektar-patch'), 'a non-buyer cannot recover a private disclosure')
})

test('S2: operator goes dark after a checkpoint; an independent observer seizes the bond at the deadline', async () => {
  const alice = await agent('alice2', [H1.url], { bond: { epochBlocks: 4 } })
  const observer = await agent('observer', [H2.url])
  await alice.mem.bondDeploy(5000)
  const commit = await alice.mem.bondCommit('ab'.repeat(32))
  assert.equal(commit.n, 1n, 'a successful checkpoint precedes the outage')
  const st = alice.mem.bondStatus()
  const descriptor = { cfg: { operatorPkh: st.cfg.operatorPkh, bondReleaseHeight: st.cfg.bondReleaseHeight.toString(), maxFee: st.cfg.maxFee.toString(), epochLen: st.cfg.epochLen.toString() }, txid: st.utxo.txid, vout: st.utxo.vout }
  // Alice stops: her runtime is closed and no code path below touches her key or data dir
  await alice.au.stop(); agents.splice(agents.indexOf(alice), 1)
  const { operationId, deadline } = await observer.au.engine.watchBond(JSON.parse(JSON.stringify(descriptor)), observer.policyHash)
  assert.equal(deadline, Number(st.deadline), 'the observer derives the deadline from the on-chain script, not from Alice')
  await observer.au.cycle()
  assert.equal((await stateOf(observer, operationId)).state, 'watching', 'no seizure before the deadline')
  while (await observer.mem.chainHeight() < deadline) chain.mine(1)
  const op = await until([observer], async () => { const o = await stateOf(observer, operationId); return o.state === 'watching' ? undefined : o }, 30_000)
  assert.equal(op.state, 'seized', op.lastError)
  const seizeTx = chain.getTx(op.data.seizeTxid)
  const mine = new P2PKH().lock(observer.mem.operatorKey().toPublicKey().toHash()).toHex()
  assert.ok(seizeTx.outputs.some(o => o.lockingScript.toHex() === mine && o.satoshis > 0), 'the contract paid the observer — no administrator involved')
  assert.equal(seizeTx.inputs[0].sourceTXID, descriptor.txid, 'the seizure spends the bond head Alice left behind')
  // guarantee boundary, stated not implied: this shows a deadline-conditioned spend of an unspent bond
  // output. It does not show that an answer was wrong, nor that a commit was mined before a deadline.
})
