// Regression suite for the 6-part solo/agent memory architecture overhaul:
//   #1 Unfunded local-readable memory & local-signed receipts (strict SPV gates preserved)
//   #2 Fast local search (query/limit/offset/since), O(1) __receipts__ lookup, SPV proof cache
//   #3 Quote-backed synthesis in groundedAnswer (fail-closed on quote mismatch)
//   #4 Sidecar-free solo host plane (openLocalHostPlane)
//   #5 Append-only memory superseding & forget tombstones (active view vs includeHistory)
//   #6 Auto-detect first testnet funding (opt-out via NEKTAR_INDEXER_DISCOVERY=0)
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import type { AddressInfo } from 'node:net'
import knexFactory from 'knex'
import { PrivateKey } from '@bsv/sdk'
import { testWorld, TxStore, type Funds } from '@nektar/chain'
import { AutomationStore, SqlFunds } from '@nektar/automation'
import { KnexShardStore } from '@nektar/log'
import { NektarMemory, createHttpApi, callTool } from '../src/index.js'
import { liveFunding } from '../src/funding.js'
import { seedWelcome } from '../src/welcome.js'
import { runHookCli } from '../src/hooks-cli.js'

const mkTmp = (prefix: string) => mkdtempSync(join(tmpdir(), prefix))

/** zero-sat coin book — models an unfunded first-boot user */
function emptyFunds(): Funds {
  const k = PrivateKey.fromRandom()
  const addr = k.toPublicKey().toAddress([0x6f])
  return {
    address: () => addr,
    balance: () => Promise.resolve(0),
    outs: () => Promise.resolve([]),
    list: () => Promise.resolve([]),
    take: () => Promise.reject(new Error('reserve empty — 0 utxos')),
    spent: () => Promise.resolve(),
    exported: () => Promise.resolve(),
    died: () => Promise.resolve(),
    adopt: () => Promise.resolve(false),
    revive: () => Promise.resolve(),
    burn: () => Promise.resolve(),
    hold: () => false,
    release: () => {}
  }
}

void test('#1: unfunded user reads own memories & commits local-signed receipts while strict SPV gates stay closed', async (t) => {
  const w = await testWorld(t, 2)
  if (w === undefined) return
  const dir = mkTmp('nektar-overhaul-1-')
  const seed = PrivateKey.fromRandom()
  const mem = new NektarMemory(w.chain, undefined, PrivateKey.fromRandom(), {
    funds: emptyFunds(),
    agentSeed: seed,
    maxLevel: 1,
    batchPolicy: { maxLeaves: 10 },
    network: 'testnet'
  })
  await seedWelcome({ mem, dataDir: dir })
  await mem.remember('notes', 'deployment port is 18420 and default network is testnet', { source: 'user' })
  const srv = createHttpApi(mem).listen(0, '127.0.0.1')
  await new Promise((r) => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`

  try {
    // 1. Direct programmatic mem.recall() stays strict SPV-only by default (S6.1 / KEY-008)
    const strictDirect = await mem.recall('notes')
    assert.equal(strictDirect.items[0].text, undefined)
    assert.equal(strictDirect.items[0].unverifiable, true)
    assert.match(strictDirect.items[0].reason ?? '', /NotAnchored/)

    // 2. External callers & requireAnchored:true stay strict SPV-only
    const ext = (await callTool(mem, 'recall', { shard: 'notes' }, { external: true })) as { items: Array<{ text?: string; unverifiable?: boolean }> }
    assert.equal(ext.items[0].text, undefined)
    assert.equal(ext.items[0].unverifiable, true)

    const reqAnch = (await callTool(mem, 'recall', { shard: 'notes', requireAnchored: true })) as {
      items: Array<{ text?: string; unverifiable?: boolean }>
    }
    assert.equal(reqAnch.items[0].text, undefined)
    assert.equal(reqAnch.items[0].unverifiable, true)

    // 3. Local owner recall works immediately at 0 sats with honest local-committed labels
    const local = (await callTool(mem, 'recall', { shard: 'notes' })) as {
      items: Array<{ text?: string; anchored?: boolean; verification?: string; unverifiable?: boolean }>
      receipt: { checkResult: string }
    }
    assert.equal(local.items[0].text, 'deployment port is 18420 and default network is testnet')
    assert.equal(local.items[0].unverifiable, undefined)
    assert.equal(local.items[0].anchored, false)
    assert.equal(local.items[0].verification, 'local-committed')
    assert.equal(local.receipt.checkResult, 'unverifiable')

    // 4. Local owner groundedAnswer commits a local-signed receipt without crashing on 0 sats
    const ga = (await callTool(mem, 'groundedAnswer', {
      claims: [{ text: 'deployment port is 18420', cites: [{ shard: 'notes', seq: 0 }] }]
    })) as {
      committed: boolean
      anchored: boolean
      verification: string
      checkResult: string
      receiptToken: string
      receiptHash: string
    }
    assert.equal(ga.committed, true)
    assert.equal(ga.anchored, false)
    assert.equal(ga.verification, 'local-signed-unanchored')
    assert.equal(ga.checkResult, 'unverifiable')
    assert.ok(ga.receiptToken.startsWith('nektarr1.'))

    // 5. verifyReceipt stays honest (ok: false because not anchored on BSV) AND reports localVerified: true
    const vr = (await callTool(mem, 'verifyReceipt', { token: ga.receiptToken })) as {
      ok: boolean
      localVerified: boolean
      anchored: boolean
      checks: Array<{ name: string; ok: unknown }>
    }
    assert.equal(vr.ok, false)
    assert.equal(vr.anchored, false)
    assert.equal(vr.localVerified, true)
    const byName = Object.fromEntries(vr.checks.map((c) => [c.name, c]))
    assert.equal(byName.found.ok, true)
    assert.equal(byName.hash.ok, true)
    assert.equal(byName.provenance.ok, true)
    assert.equal(byName.anchored.ok, false)

    // 6. Stop hook accepts local-signed unanchored receipts without blocking, while still catching answer-drift
    let outOk = ''
    let errOk = ''
    await runHookCli(['verify-receipt'], {
      out: (s) => (outOk += s),
      err: (s) => (errOk += s),
      stdinText: () =>
        Promise.resolve(
          JSON.stringify({
            stop_hook_active: false,
            last_assistant_message: `The deployment port is 18420.\nreceipt: ${ga.receiptToken}\nverify: ${base}/v1/verify?r=${ga.receiptToken}`
          })
        ),
      env: { NEKTAR_URL: base }
    })
    assert.equal(outOk, '', 'local-signed unanchored receipt must not block Stop hook')
    assert.match(errOk, /LOCAL-SIGNED/)

    let outDrift = ''
    await runHookCli(['verify-receipt'], {
      out: (s) => (outDrift += s),
      err: () => {},
      stdinText: () =>
        Promise.resolve(
          JSON.stringify({
            stop_hook_active: false,
            last_assistant_message: `The deployment port is 99999.\nreceipt: ${ga.receiptToken}\nverify: ${base}/v1/verify?r=${ga.receiptToken}`
          })
        ),
      env: { NEKTAR_URL: base }
    })
    assert.equal((JSON.parse(outDrift) as { decision?: string }).decision, 'block', 'answer-drift must still block')
  } finally {
    srv.close()
    rmSync(dir, { recursive: true, force: true })
  }
})

void test('#2 & #5: local search, pagination, O(1) SQLite receipt index, proof cache, superseding, and forget tombstones', async (t) => {
  const w = await testWorld(t, 4)
  if (w === undefined) return
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new KnexShardStore(kx)
  await store.init()
  const mem = new NektarMemory(w.chain, w.funder, w.funder.keyPair, { batchPolicy: { maxLeaves: 4 }, network: 'regtest' }, store)

  try {
    const tBase = Math.floor(Date.now() / 1000) - 100
    await mem.remember('kb', 'drain port 4 torque is 40Nm', { source: 'manual', time: tBase })
    await mem.remember('kb', 'inspect belt every 10 hours', { source: 'manual', time: tBase + 10 })
    await mem.remember('kb', 'coolant pump pressure 2.4 bar', { source: 'manual', time: tBase + 20 })
    // supersede seq 0 with updated torque spec
    const sup = (await callTool(mem, 'remember', {
      shard: 'kb',
      data: 'drain port 4 torque is 45Nm (revised)',
      supersedes: { shard: 'kb', seq: 0 }
    })) as { seq: number }
    assert.equal(sup.seq, 3)

    // anchor + settle so we also test SPV proof cache
    await mem.anchorShard('kb')
    await w.chain.mine(1)
    for (const l of mem.anchorLinks('kb')) mem.confirmAnchor('kb', l.txid)

    // default active recall excludes superseded seq 0
    const active = (await callTool(mem, 'recall', { shard: 'kb' })) as { items: Array<{ seq: number; text?: string }> }
    assert.deepEqual(
      active.items.map((i) => i.seq),
      [1, 2, 3]
    )

    // keyword search ranks relevant record first and respects limit
    const searched = (await callTool(mem, 'recall', { shard: 'kb', query: 'torque 45Nm', limit: 2 })) as {
      items: Array<{ seq: number; text?: string }>
    }
    assert.equal(searched.items.length, 1)
    assert.equal(searched.items[0].seq, 3)
    assert.match(searched.items[0].text ?? '', /45Nm/)

    // pagination (offset + limit) and since timestamp filter
    const paged = (await callTool(mem, 'recall', { shard: 'kb', offset: 1, limit: 1 })) as { items: Array<{ seq: number }> }
    assert.deepEqual(
      paged.items.map((i) => i.seq),
      [2]
    )
    const recent = (await callTool(mem, 'recall', { shard: 'kb', since: tBase + 15 })) as { items: Array<{ seq: number }> }
    assert.deepEqual(
      recent.items.map((i) => i.seq),
      [2, 3]
    )

    // forget appends a tombstone (seq 4) hiding seq 1 from active view without deleting history
    const fg = (await callTool(mem, 'forget', { shard: 'kb', seq: 1, reason: 'obsolete schedule' })) as { seq: number }
    assert.equal(fg.seq, 4)

    const activeAfterForget = (await callTool(mem, 'recall', { shard: 'kb' })) as { items: Array<{ seq: number }> }
    assert.deepEqual(
      activeAfterForget.items.map((i) => i.seq),
      [2, 3]
    )

    const hist = (await callTool(mem, 'recall', { shard: 'kb', includeHistory: true })) as {
      items: Array<{ seq: number; superseded?: boolean; forgotten?: boolean; forgets?: { seq: number } }>
    }
    assert.equal(hist.items.length, 5)
    assert.equal(hist.items.find((i) => i.seq === 0)?.superseded, true)
    assert.equal(hist.items.find((i) => i.seq === 1)?.forgotten, true)
    assert.equal(hist.items.find((i) => i.seq === 4)?.forgets?.seq, 1)

    // explicit seq read still resolves historical record so old receipts never break
    const explicitOld = (await callTool(mem, 'recall', { shard: 'kb', seq: 0 })) as { items: Array<{ seq: number; text?: string }> }
    assert.equal(explicitOld.items.length, 1)
    assert.match(explicitOld.items[0].text ?? '', /40Nm/)

    // SPV proof cache hit + clearProofCache invalidation
    const p1 = await mem.prove('kb', 3)
    const p2 = await mem.prove('kb', 3)
    assert.equal(p1.result.ok, true)
    assert.equal(p2.bundle, p1.bundle, 'second prove() should return cached bundle reference')
    mem.clearProofCache()
    const p3 = await mem.prove('kb', 3)
    assert.equal(p3.result.ok, true)
    assert.notEqual(p3.bundle, p1.bundle, 'clearProofCache() must force fresh proof rebuild')

    // O(1) receipt index on KnexShardStore
    const ga = (await callTool(mem, 'groundedAnswer', {
      claims: [{ text: 'drain port 4 torque is 45Nm (revised)', cites: [{ shard: 'kb', seq: 3 }] }]
    })) as { receiptHash: string; receiptToken: string }
    await w.chain.mine(1)
    for (const l of mem.anchorLinks('__receipts__')) mem.confirmAnchor('__receipts__', l.txid)
    const indexedSeq = await mem.findReceiptSeq(ga.receiptHash)
    assert.equal(indexedSeq, 0, 'receipt_hash index must locate __receipts__ seq in O(1)')
    const vr = (await callTool(mem, 'verifyReceipt', { token: ga.receiptToken })) as { ok: boolean; anchored: boolean }
    assert.equal(vr.ok, true)
    assert.equal(vr.anchored, true)
  } finally {
    await kx.destroy()
  }
})

void test('#3: groundedAnswer supports quote-backed synthesis and fails closed on mismatched quotes', async (t) => {
  const w = await testWorld(t, 4)
  if (w === undefined) return
  const mem = new NektarMemory(w.chain, w.funder, w.funder.keyPair, { batchPolicy: { maxLeaves: 2 }, network: 'regtest' })
  await mem.remember('manual', 'Hydraulic drain port 4 requires torque 45Nm and a fresh copper crush washer.')
  await mem.remember('manual', 'Replace hydraulic oil filter every 500 operating hours.')
  await w.chain.mine(1)
  for (const l of mem.anchorLinks('manual')) mem.confirmAnchor('manual', l.txid)

  // Synthesized natural claim backed by verbatim quotes from verified fragments → grounded:quote-backed
  const synth = (await callTool(mem, 'groundedAnswer', {
    claims: [
      {
        text: 'Torque port 4 to 45Nm with a new copper crush washer, and swap the oil filter every 500 hours.',
        quotes: [
          { quote: 'torque 45Nm and a fresh copper crush washer', shard: 'manual', seq: 0 },
          { quote: 'every 500 operating hours', shard: 'manual', seq: 1 }
        ]
      }
    ]
  })) as {
    committed: boolean
    groundingTier: string
    grounding: { claims: Array<{ grounded: boolean; reason: string }> }
    receipt: { groundingTier?: string; fragmentRefs: Array<{ shard: string; seq: number }> }
    receiptToken: string
  }
  assert.equal(synth.committed, true)
  assert.equal(synth.groundingTier, 'quote-backed-synthesis')
  assert.equal(synth.receipt.groundingTier, 'quote-backed-synthesis')
  assert.equal(synth.grounding.claims[0].grounded, true)
  assert.equal(synth.grounding.claims[0].reason, 'grounded:quote-backed')
  assert.equal(synth.receipt.fragmentRefs.length, 2)

  await w.chain.mine(1)
  for (const l of mem.anchorLinks('__receipts__')) mem.confirmAnchor('__receipts__', l.txid)
  const vr = (await callTool(mem, 'verifyReceipt', { token: synth.receiptToken })) as {
    ok: boolean
    checks: Array<{ name: string; detail: string }>
  }
  assert.equal(vr.ok, true)
  assert.equal(vr.checks.find((c) => c.name === 'groundingTier')?.detail, 'quote-backed-synthesis')

  // Hallucinated/mismatched quote MUST fail closed (quote-mismatch) and strip the claim
  const bad = (await callTool(mem, 'groundedAnswer', {
    claims: [
      {
        text: 'Torque port 4 to 99Nm.',
        quotes: [{ quote: 'torque 99Nm', shard: 'manual', seq: 0 }]
      }
    ]
  })) as {
    committed: boolean
    checkResult: string
    flagged: Array<{ reason: string }>
  }
  assert.equal(bad.committed, false)
  assert.equal(bad.checkResult, 'failed')
  assert.equal(bad.flagged[0].reason, 'quote-mismatch')
})

void test('#6: testnet auto-detects first funding when 0 UTXOs (opt-out via NEKTAR_INDEXER_DISCOVERY=0; mainnet stays opt-in)', async () => {
  const dir = mkTmp('nektar-overhaul-6-')
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const ledger = new AutomationStore(kx)
  await ledger.init()
  const txStore = new TxStore()
  const key = PrivateKey.fromRandom()
  const pkhHex = Buffer.from(key.toPublicKey().toHash()).toString('hex')
  const addr = key.toPublicKey().toAddress([0x6f])
  const funds = await SqlFunds.open(kx, txStore, { pkhHex, address: addr })
  const prevDisc = process.env.NEKTAR_INDEXER_DISCOVERY
  delete process.env.NEKTAR_INDEXER_DISCOVERY

  try {
    const runProbe = async (net: 'testnet' | 'main') => {
      let hits = 0
      const fetchImpl: typeof fetch = () => {
        hits++
        return Promise.resolve(new Response('[]', { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      await liveFunding({
        cfg: { dataDir: dir },
        mc: {
          broadcasterUrl: 'http://127.0.0.1:1',
          indexerUrl: 'http://127.0.0.1:1',
          fetchImpl,
          maxFeePerTx: 1000,
          maxSpendPerDay: 20_000,
          minBroadcastIntervalMs: 250
        },
        net,
        custody: { funds, fundKey: key, fundAddress: addr, pkhHex },
        txStore,
        ledger,
        port: {
          network: net,
          broadcast: () => Promise.resolve({ status: 'success' }),
          tracker: {
            currentHeight: () => Promise.resolve(1),
            isValidRootForHeight: () => Promise.resolve(true),
            headerAtHeight: () => Promise.resolve(undefined)
          },
          height: () => Promise.resolve(1),
          observe: () => Promise.resolve(undefined),
          abandonAfterBlocks: 10
        },
        broadcast: () => Promise.resolve({ status: 'success' }),
        watchTx: () => {}
      })
      return hits
    }

    assert.ok((await runProbe('testnet')) > 0, 'testnet with 0 UTXOs must auto-probe indexer for first funding')
    assert.equal(await runProbe('main'), 0, 'mainnet must remain strictly opt-in for indexer discovery')
    process.env.NEKTAR_INDEXER_DISCOVERY = '0'
    assert.equal(await runProbe('testnet'), 0, 'NEKTAR_INDEXER_DISCOVERY=0 must disable testnet auto-discovery')
  } finally {
    if (prevDisc === undefined) delete process.env.NEKTAR_INDEXER_DISCOVERY
    else process.env.NEKTAR_INDEXER_DISCOVERY = prevDisc
    await kx.destroy()
    rmSync(dir, { recursive: true, force: true })
  }
})
