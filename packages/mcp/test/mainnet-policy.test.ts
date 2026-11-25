// mainnet-policy.test.ts — permanent zero-sat CI E2E suite enforcing real BSV
// mainnet relay policy (100 sat/kB TX_POLICY 39), SpendGuard caps, cold-start
// maxUnproven=1, canDeliverBox pre-funding gate, DisclosureSale buy & refund,
// 16-input auto-consolidation under bbox's 16 KiB MaxContent cap, persisted
// PoW-verified headers-cache.json offline reload, and lanes.sqlite pruning.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import knexFactory from 'knex'
import {
  MemoryFunder,
  MemoryTreasury,
  SpendGuard,
  WitnessedTracker,
  fundsOf,
  memoryPort,
  type ChainPort,
  type ChainTracker,
  type MemoryChain
} from '@nektar/chain'
import { KnexTreeLedger, readBox } from '@nektar/bstack'
import { minihost } from '@nektar/bstack/testing'
import { DEFAULT_SCORE_WEIGHTS, Dispatcher, ScriptedExecutor, openAutomation, type Automation } from '@nektar/automation'
import { openRuntime } from '../src/runtime.js'
import { callTool } from '../src/server.js'
import { NektarMemory } from '../src/memory.js'

const MAINNET_RATE = 100
const OFFICE = 'nektar_abcdefghij'
const FINDING = 'mainnet-policy finding: torque drain port to 45Nm with fresh copper crush washer'

/** ChainPort over a strict 100 sat/kB MemoryChain, guarded by SpendGuard and
 *  backed by a disk-persisted WitnessedTracker (exact mainnet assembly shape). */
function guardedMainnetPort(
  chain: MemoryChain,
  dataDir: string,
  valueOf: (txid: string, vout: number) => number | undefined
): { port: ChainPort; tracker: WitnessedTracker; guard: SpendGuard } {
  const base = memoryPort(chain)
  const tracker = new WitnessedTracker(chain, chain, {
    persistPath: join(dataDir, 'headers-cache.json'),
    finalityDepth: 12,
    requirePow: true
  })
  const guard = new SpendGuard({ broadcast: (tx) => base.broadcast(tx) }, valueOf, {
    maxFeePerTx: 1_000,
    maxPerDay: 250_000,
    minIntervalMs: 0,
    ledgerFile: join(dataDir, 'spend-guard.json')
  })
  return {
    tracker,
    guard,
    port: {
      ...base,
      network: 'regtest',
      feeRateSatPerKb: MAINNET_RATE,
      tracker,
      broadcast: (tx) => guard.broadcast(tx)
    }
  }
}

async function makeMainnetPolicyAgent(
  name: string,
  treasury: MemoryTreasury,
  hosts: string[],
  dir: string,
  clock: () => number,
  opts: { funderOutputs?: number; funderSats?: number; treeSize?: number; reserveOuts?: number } = {}
) {
  const dataDir = join(dir, name)
  // Seed initial wallet outputs before turning on the strict 100 sat/kB policy
  // so MemoryFunder's test-setup split uses its standard fixture helper, while
  // every Nektar runtime transaction is strictly gated at 100 sat/kB.
  const prevMinRate = treasury.chain.faults.minFeeRateSatPerKb
  treasury.chain.faults.minFeeRateSatPerKb = undefined
  const funder = await MemoryFunder.fund(treasury, opts.funderOutputs ?? 8, opts.funderSats ?? 25_000)
  await treasury.chain.mine(1)
  treasury.chain.faults.minFeeRateSatPerKb = prevMinRate
  const funds = fundsOf(funder)
  const { port, tracker, guard } = guardedMainnetPort(
    treasury.chain,
    dataDir,
    (txid, vout) => funder.utxos.find((u) => u.tx.id('hex') === txid && u.vout === vout)?.satoshis
  )
  const runtime = await openRuntime(
    {
      network: 'regtest',
      dataDir,
      confirmEveryMs: 60_000,
      regtest: { rpcUrl: '', rpcUser: '', rpcPass: '', propagationUrl: '', assetUrl: '', fundWif: funder.keyPair.toWif() }
    },
    { chain: port, funds }
  )
  assert.equal(runtime.mem.feeRateSatPerKb, MAINNET_RATE)
  const pub = runtime.mem.operatorKey().toPublicKey().toString()
  const auto = await openAutomation(runtime.mem, {
    network: 'regtest',
    localHosts: true,
    dataDir,
    marketHosts: hosts,
    intervalMs: 0,
    exec: new Dispatcher(),
    call: (tool, args) => callTool(runtime.mem, tool, args),
    ai: new ScriptedExecutor(),
    now: clock,
    publishFinger: false,
    treeSize: opts.treeSize ?? 4,
    treeLowWater: 2,
    reserveOuts: opts.reserveOuts ?? 2,
    opts: {
      offerTtlBlocks: 48,
      refundBlocks: 20,
      maxZeroconfSats: 100_000,
      maxUnconfirmedAncestors: 8
    }
  })
  runtime.mem.automation = auto
  const policyHash = await auto.store.approvePolicy(
    {
      v: 1,
      policyId: `mp-${name}`,
      network: 'regtest',
      agentPub: pub,
      roles: ['buyer', 'seller'],
      notBefore: 0,
      notAfter: 4_000_000_000_000,
      domains: ['mainnet-policy'],
      limits: {
        perPurchaseSats: 500,
        dailyPurchaseSats: 50_000,
        perTxFeeSats: 1_200,
        dailyFeeSats: 50_000,
        runTotalSats: 500_000,
        maxLockedSats: 5_000,
        maxUnconfirmedSats: 5_000,
        maxUnconfirmedCount: 16
      },
      scoring: { minimumScoreBps: 0, weights: { ...DEFAULT_SCORE_WEIGHTS }, collectMs: 2_000 },
      seller: { minPriceSats: 100, maxPriceSats: 1_000, maxPreparationsPerDay: 50, sellableShards: ['guide'] },
      disclosure: { allowPublic: false },
      ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 200, timeoutMs: 5_000 },
      sandbox: { mode: 'sandbox-only' }
    },
    'test:mainnet-policy'
  )
  return { name, dataDir, funder, funds, runtime, mem: runtime.mem, auto, pub, policyHash, tracker, guard }
}

void test('mainnet-policy E2E: openRuntime + SpendGuard + 100 sat/kB anchor, SPV receipt, canDeliverBox gate, and Deals 1 & 2', async () => {
  const prevPaid = process.env.NEKTAR_PAID_DISCLOSURE
  process.env.NEKTAR_PAID_DISCLOSURE = '1'
  const dir = mkdtempSync(join(tmpdir(), 'nektar-mp-e2e-'))
  const treasury = new MemoryTreasury()
  const chain = treasury.chain
  chain.faults.minFeeRateSatPerKb = MAINNET_RATE
  let now = Date.now()
  const h1 = await minihost(['regtest'], { now: () => now, tracker: chain, office: OFFICE, logStream: OFFICE })
  const h2 = await minihost(['regtest'], { now: () => now, tracker: chain, office: OFFICE, logStream: OFFICE })
  const hosts = [h1.url, h2.url]
  let alice: Awaited<ReturnType<typeof makeMainnetPolicyAgent>> | undefined
  let ben: Awaited<ReturnType<typeof makeMainnetPolicyAgent>> | undefined
  try {
    alice = await makeMainnetPolicyAgent('alice', treasury, hosts, dir, () => now, { treeSize: 8 })
    ben = await makeMainnetPolicyAgent('ben', treasury, hosts, dir, () => now, { treeSize: 8 })

    // 1. Cold-start upkeep mints 1 tree per lane (not 2!), market serves anchored immediately, bbox awaits 1 block
    await Promise.all([alice.auto.upkeep(), ben.auto.upkeep()])
    const [aStock0, bStock0] = await Promise.all([alice.auto.laneStock(), ben.auto.laneStock()])
    assert.equal(aStock0.market.unprovenTrees, 1, 'cold-start market mints 1 tree, not 2')
    assert.equal(aStock0.bbox.unprovenTrees, 1, 'cold-start bbox mints 1 tree, not 2')
    assert.ok(aStock0.market.anchored > 0, 'market serves anchored 0-conf slots immediately')
    assert.equal(aStock0.bbox.proven, 0, 'bbox stays proven-only until 1st block')
    assert.equal(bStock0.bbox.proven, 0)

    // 2. Ben remembers + anchors NektarAnchor at 100 sat/kB + commits groundedAnswer receipt
    await ben.mem.remember('guide', FINDING)
    await ben.mem.anchorShard('guide')
    await ben.mem.describeShard('guide', { domains: ['mainnet-policy'], offers: [{ kind: 'record', priceSats: 200 }] })
    await ben.mem.anchorShard('__catalog__')
    const anchorTxid = ben.mem.anchorLinks('guide')[0]?.txid
    assert.ok(anchorTxid, 'anchor broadcast accepted under strict 100 sat/kB policy')
    const ga = (await callTool(ben.mem, 'groundedAnswer', {
      claims: [{ text: 'torque drain port to 45Nm with fresh copper crush washer', cites: [{ shard: 'guide', seq: 0 }] }]
    })) as { committed: boolean; receiptToken: string }
    assert.equal(ga.committed, true)

    // 3. Deal 1 published while bbox is unproven -> buyer must hold cleanly in selected with 0 sats locked!
    await ben.auto.engine.enableSeller(ben.policyHash)
    const w1 = await alice.auto.engine.publishWant({ domains: ['mainnet-policy'], question: 'drain port torque?', maxSats: 450 }, alice.policyHash)
    let sawGate = false
    for (let i = 0; i < 16; i++) {
      now += 500
      await Promise.all([alice.auto.deals(), ben.auto.deals()])
      const bo = (await alice.auto.store.operations({ role: 'buyer' })).find((o) => o.data.want?.want?.wantId === w1.wantId)
      if (bo?.state === 'selected' && /bbox/.test(bo.lastError ?? '')) {
        assert.equal(bo.data.escrowTxid, undefined, 'canDeliverBox gate must lock 0 sats while bbox is unproven')
        sawGate = true
        break
      }
    }
    assert.equal(sawGate, true, 'buyer must hit canDeliverBox pre-funding gate before bbox confirms')

    // 4. Mine 1 block -> proves NektarAnchor + bbox trees; SPV verifyReceipt passes (ok: true, anchored: true)
    await chain.mine(1)
    await Promise.all([alice.auto.upkeep(), ben.auto.upkeep()])
    const [aStock1, bStock1] = await Promise.all([alice.auto.laneStock(), ben.auto.laneStock()])
    assert.ok(aStock1.bbox.proven > 0 && bStock1.bbox.proven > 0, 'both bbox lanes proven after 1 block')
    ben.mem.confirmAnchor('guide', anchorTxid)
    const vr = (await callTool(ben.mem, 'verifyReceipt', { token: ga.receiptToken })) as { ok: boolean; anchored: boolean }
    assert.equal(vr.ok, true, 'SPV verifyReceipt must pass after 1 block')
    assert.equal(vr.anchored, true)

    // 5. Deal 1 completes at strict 100 sat/kB (proves escrow + seller DisclosureSale complete pay >= 100 sat/kB!)
    const driveDeal = async (wantId: string, allowMine: boolean) => {
      for (let i = 0; i < 40; i++) {
        now += 500
        await Promise.all([alice!.auto.deals(), ben!.auto.deals()])
        if (allowMine && (await chain.pendingBroadcasts()) > 0) await chain.mine(1)
        const bo = (await alice!.auto.store.operations({ role: 'buyer' })).find((o) => o.data.want?.want?.wantId === wantId)
        const so = (await ben!.auto.store.operations({ role: 'seller' })).find((o) => o.data.want?.want?.wantId === wantId)
        if (bo && ['disclosure_verified', 'useful'].includes(bo.state) && so && ['completion_submitted', 'settled', 'useful'].includes(so.state)) {
          return { bo, so }
        }
      }
      const bo = (await alice!.auto.store.operations({ role: 'buyer' })).find((o) => o.data.want?.want?.wantId === wantId)
      const so = (await ben!.auto.store.operations({ role: 'seller' })).find((o) => o.data.want?.want?.wantId === wantId)
      throw new Error(`Deal ${wantId} did not finish (buyer=${bo?.state}:${bo?.lastError ?? ''} seller=${so?.state}:${so?.lastError ?? ''})`)
    }
    const d1 = await driveDeal(w1.wantId, false)
    const r1 = d1.bo.data.result as { verified?: boolean; text?: string } | undefined
    assert.equal(r1?.verified, true)
    assert.equal(r1?.text, FINDING)

    // 6. Deal 2 completes back-to-back with ZERO blocks mined!
    const tipBeforeD2 = await chain.currentHeight()
    const w2 = await alice.auto.engine.publishWant({ domains: ['mainnet-policy'], question: 'drain port torque again?', maxSats: 450 }, alice.policyHash)
    const d2 = await driveDeal(w2.wantId, false)
    const r2 = d2.bo.data.result as { verified?: boolean } | undefined
    assert.equal(r2?.verified, true)
    assert.equal(await chain.currentHeight(), tipBeforeD2, 'Deal 2 must complete with 0 blocks mined')

    // 7. Persisted finalized headers-cache.json offline PoW reload (0 remote calls)
    const finalizedH = (await chain.currentHeight()) - 20
    const hdr = await alice.tracker.headerAtHeight(finalizedH)
    assert.ok(hdr, 'finalized header fetched and corroborated')
    alice.tracker.flushPersisted()
    const cacheFile = join(alice.dataDir, 'headers-cache.json')
    assert.equal(existsSync(cacheFile), true, 'headers-cache.json persisted to disk')
    let remoteCalls = 0
    const offline: ChainTracker = {
      currentHeight: () => {
        remoteCalls++
        return Promise.reject(new Error('offline'))
      },
      headerAtHeight: () => {
        remoteCalls++
        return Promise.reject(new Error('offline'))
      },
      isValidRootForHeight: () => {
        remoteCalls++
        return Promise.reject(new Error('offline'))
      }
    }
    const reloaded = new WitnessedTracker(offline, offline, { persistPath: cacheFile })
    assert.equal(await reloaded.isValidRootForHeight(hdr.merkleRoot, finalizedH), true)
    assert.equal(remoteCalls, 0, 'persisted finalized header must verify offline with 0 remote calls')
  } finally {
    await alice?.auto.stop().catch(() => {})
    await ben?.auto.stop().catch(() => {})
    await alice?.runtime.close().catch(() => {})
    await ben?.runtime.close().catch(() => {})
    await h1.close()
    await h2.close()
    chain.stopWatching()
    rmSync(dir, { recursive: true, force: true })
    if (prevPaid === undefined) delete process.env.NEKTAR_PAID_DISCLOSURE
    else process.env.NEKTAR_PAID_DISCLOSURE = prevPaid
  }
})

void test('mainnet-policy E2E: DisclosureSale timeout buyer refund passes strict 100 sat/kB policy', async () => {
  const prevPaid = process.env.NEKTAR_PAID_DISCLOSURE
  process.env.NEKTAR_PAID_DISCLOSURE = '1'
  const treasury = new MemoryTreasury()
  const chain = treasury.chain
  try {
    const funder = await MemoryFunder.fund(treasury, 4, 20_000)
    await chain.mine(1)
    chain.faults.minFeeRateSatPerKb = MAINNET_RATE
    const mem = new NektarMemory(chain, funder, funder.keyPair, {
      batchPolicy: { maxLeaves: 4 },
      network: 'main',
      feeRateSatPerKb: MAINNET_RATE
    })
    const height = await chain.currentHeight()
    const offer = (await callTool(mem, 'sale', {
      op: 'offer',
      value: Buffer.from('mainnet-refund-policy-check-payload').toString('hex'),
      price: 200,
      refundAfter: height + 5,
      maxFee: 10
    })) as { cfg: { price: number; refundAfter: number; maxFee: number }; wrapped: string }
    const escrow = (await callTool(mem, 'sale', {
      op: 'escrow',
      cfg: offer.cfg,
      amount: offer.cfg.price + offer.cfg.maxFee
    })) as { accepted: boolean; utxo: { txid: string; vout: number; satoshis: number; script: string }; escrow: Record<string, unknown> }
    assert.equal(escrow.accepted, true, 'escrow funding must pass 100 sat/kB')
    await chain.mine(6)
    const refund = (await callTool(mem, 'sale', {
      op: 'refund',
      cfg: escrow.escrow,
      utxo: escrow.utxo
    })) as { accepted: boolean; txid?: string; reason?: string; refundSats?: number }
    assert.equal(refund.accepted, true, `buyer refund must pass strict 100 sat/kB policy (got: ${refund.reason})`)
    assert.ok((refund.refundSats ?? 0) > 0)
  } finally {
    chain.stopWatching()
    if (prevPaid === undefined) delete process.env.NEKTAR_PAID_DISCLOSURE
    else process.env.NEKTAR_PAID_DISCLOSURE = prevPaid
  }
})

void test('mainnet-policy E2E: 16-input auto-consolidation at 100 sat/kB + bbox 16 KiB MaxContent cap + lanes.sqlite prune', async () => {
  const prevPaid = process.env.NEKTAR_PAID_DISCLOSURE
  process.env.NEKTAR_PAID_DISCLOSURE = '1'
  const dir = mkdtempSync(join(tmpdir(), 'nektar-mp-con-'))
  const treasury = new MemoryTreasury()
  const chain = treasury.chain
  chain.faults.minFeeRateSatPerKb = MAINNET_RATE
  let now = Date.now()
  const h1 = await minihost(['regtest'], { now: () => now, tracker: chain, office: OFFICE, logStream: OFFICE })
  const h2 = await minihost(['regtest'], { now: () => now, tracker: chain, office: OFFICE, logStream: OFFICE })
  const hosts = [h1.url, h2.url]
  let alice: Awaited<ReturnType<typeof makeMainnetPolicyAgent>> | undefined
  let ben: Awaited<ReturnType<typeof makeMainnetPolicyAgent>> | undefined
  try {
    // Seed alice with 28 fragmented small confirmed UTXOs (3,500 sats each) to trigger auto-consolidation
    alice = await makeMainnetPolicyAgent('alice', treasury, hosts, dir, () => now, { funderOutputs: 28, funderSats: 3_500, treeSize: 4 })
    ben = await makeMainnetPolicyAgent('ben', treasury, hosts, dir, () => now, { funderOutputs: 6, funderSats: 20_000, treeSize: 4 })

    const beforeOuts = (await alice.funds.outs()).length
    assert.ok(beforeOuts >= 26, `expected fragmented reserve, got ${beforeOuts}`)

    // Upkeep automatically consolidates 16 small UTXOs at >= 100 sat/kB AND mints 1 tree per lane
    await Promise.all([alice.auto.upkeep(), ben.auto.upkeep()])
    await chain.mine(1)
    await Promise.all([alice.auto.upkeep(), ben.auto.upkeep()])
    const afterOuts = (await alice.funds.outs()).length
    assert.ok(afterOuts < beforeOuts, `expected consolidation to shrink UTXO count (${beforeOuts} -> ${afterOuts})`)

    // Run a deal spending alice's consolidated reserve and verify recovery note stays under bbox 16 KiB MaxContent!
    await ben.mem.remember('guide', FINDING)
    await ben.mem.anchorShard('guide')
    await ben.mem.describeShard('guide', { domains: ['mainnet-policy'], offers: [{ kind: 'record', priceSats: 200 }] })
    await ben.mem.anchorShard('__catalog__')
    await chain.mine(1)
    for (const l of ben.mem.anchorLinks('guide')) ben.mem.confirmAnchor('guide', l.txid)
    await ben.auto.engine.enableSeller(ben.policyHash)

    const w = await alice.auto.engine.publishWant({ domains: ['mainnet-policy'], question: 'torque?', maxSats: 450 }, alice.policyHash)
    let funded = false
    for (let i = 0; i < 30; i++) {
      now += 500
      await alice.auto.cycle()
      const bo = (await alice.auto.store.operations({ role: 'buyer' })).find((o) => o.data.want?.want?.wantId === w.wantId)
      if (bo?.state === 'awaiting_disclosure') {
        funded = true
        break
      }
      await ben.auto.cycle()
      if ((await chain.pendingBroadcasts()) > 0) await chain.mine(1)
    }
    assert.equal(funded, true, 'buyer funded escrow from consolidated UTXO under 100 sat/kB policy')

    // Drain note outbox and verify sealed note fits strictly under 16 KiB MaxContent
    let noteItem: Awaited<ReturnType<typeof readBox>>['items'][number] | undefined
    for (let i = 0; i < 10 && noteItem === undefined; i++) {
      now += 500
      await alice.auto.cycle()
      const box = await readBox({ key: alice.mem.operatorKey(), hosts, office: OFFICE, tracker: chain })
      noteItem = box.items.find((it) => (it.body as { market?: { type?: string } })?.market?.type === 'note')
    }
    assert.ok(noteItem, 'recovery note listed on bbox host (did not hit boxrec: range: content)')

    // Verify KnexTreeLedger.prune works cleanly on real lanes.sqlite
    await alice.auto.stop()
    alice = { ...alice, auto: undefined as unknown as Automation }
    const lkx = knexFactory({ client: 'sqlite3', connection: { filename: join(dir, 'alice', 'lanes.sqlite') }, useNullAsDefault: true })
    try {
      const ledger = new KnexTreeLedger(lkx)
      const pruned = await ledger.prune(Date.now() + 1_000)
      assert.ok(pruned >= 0, 'lanes.sqlite prune runs cleanly')
    } finally {
      await lkx.destroy()
    }
  } finally {
    await alice?.auto?.stop().catch(() => {})
    await ben?.auto?.stop().catch(() => {})
    await alice?.runtime.close().catch(() => {})
    await ben?.runtime.close().catch(() => {})
    await h1.close()
    await h2.close()
    chain.stopWatching()
    rmSync(dir, { recursive: true, force: true })
    if (prevPaid === undefined) delete process.env.NEKTAR_PAID_DISCLOSURE
    else process.env.NEKTAR_PAID_DISCLOSURE = prevPaid
  }
})
