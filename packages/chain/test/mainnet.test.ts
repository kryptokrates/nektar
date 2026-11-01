import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, P2PKH, UnlockingScript } from '@bsv/sdk'
import type { Broadcaster } from '../src/interfaces.js'
import { sha256 } from '@mamem/core'
import { ArcBroadcaster, fetchArcFeeRate, WocTracker, tscToMerklePath, merklePathMainnet, discoverUtxos } from '../src/mainnet.js'
import { SpendGuard } from '../src/spendguard.js'
import { buildBump } from '../src/mock.js'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'

// -- recorded fixture helpers --------------------------------------------------
const stubFetch = (routes: Record<string, { status?: number, body?: any, raw?: string }>) => {
  const calls: string[] = []
  const f: typeof fetch = async (input: any, init?: any) => {
    const url = String(input)
    calls.push(`${init?.method ?? 'GET'} ${url}`)
    for (const [k, v] of Object.entries(routes)) {
      if (url.includes(k)) {
        const status = v.status ?? 200
        return new Response(v.raw ?? JSON.stringify(v.body ?? {}), { status, headers: { 'content-type': 'application/json' } })
      }
    }
    return new Response('not found', { status: 404 })
  }
  ;(f as any).calls = calls
  return f
}

const tx = () => { // minimal valid tx for broadcast tests
  const parent = new Transaction()
  parent.addOutput({ satoshis: 1000, lockingScript: new P2PKH().lock('1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa') })
  const t = new Transaction()
  t.addInput({ sourceTXID: parent.id('hex'), sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff })
  t.addOutput({ satoshis: 900, lockingScript: parent.outputs[0]!.lockingScript })
  return t
}

test('ArcBroadcaster: success on 2xx non-rejected status', async () => {
  const fetchImpl = stubFetch({ '/v1/tx': { body: { txid: 'aa', txStatus: 'SEEN_ON_NETWORK' } } })
  const b = new ArcBroadcaster({ fetchImpl, arcUrl: 'http://arc' })
  const res = await b.broadcast(tx())
  assert.equal(res.status, 'success')
})

test('ArcBroadcaster: REJECTED is an error with NO WoC fallback', async () => {
  const fetchImpl = stubFetch({
    'arc.example/v1/tx': { body: { txStatus: 'REJECTED', extraInfo: 'low fee' } },
    'woc.example/tx/raw': { body: { txid: 'x' } },
  })
  const b = new ArcBroadcaster({ fetchImpl, arcUrl: 'http://arc.example', wocUrl: 'http://woc.example' })
  const res = await b.broadcast(tx())
  assert.equal(res.status, 'error')
  assert.match(res.description!, /REJECTED.*low fee/)
  assert.ok(!(fetchImpl as any).calls.some((c: string) => c.includes('woc.example')), 'policy rejection must NOT fall back')
})

test('ArcBroadcaster: 5xx falls back to WoC', async () => {
  const fetchImpl = stubFetch({
    'arc.example/v1/tx': { status: 503, body: 'oops', raw: 'oops' },
    'woc.example/tx/raw': { body: 'deadbeef', raw: '"deadbeef"' },
  })
  const b = new ArcBroadcaster({ fetchImpl, arcUrl: 'http://arc.example', wocUrl: 'http://woc.example' })
  const res = await b.broadcast(tx())
  assert.equal(res.status, 'success')
  assert.ok((fetchImpl as any).calls.some((c: string) => c.includes('woc.example/tx/raw')), 'WoC fallback used')
})

test('fetchArcFeeRate: policy 100sat/1000B → 100 sat/kB', async () => {
  const fetchImpl = stubFetch({ '/v1/policy': { body: { policy: { miningFee: { bytes: 1000, satoshis: 100 } } } } })
  assert.equal(await fetchArcFeeRate({ fetchImpl, arcUrl: 'http://arc' }), 100)
})

test('WocTracker: header cached; 429 retried', async () => {
  let headerCalls = 0
  const fetchImpl: typeof fetch = async (input: any) => {
    const url = String(input)
    if (url.includes('/chain/info')) return new Response(JSON.stringify({ blocks: 850000 }), { status: 200 })
    if (url.includes('/block/height/5')) {
      headerCalls++
      if (headerCalls === 1) return new Response('slow', { status: 429 })
      return new Response(JSON.stringify({ hash: 'h5', previousblockhash: 'h4', merkleroot: 'mr', time: 1700000000, bits: '1d00ffff', nonce: 1 }), { status: 200 })
    }
    return new Response('nf', { status: 404 })
  }
  const t = new WocTracker({ fetchImpl, wocUrl: 'http://woc' })
  assert.equal(await t.currentHeight(), 850000)
  const h1 = await t.headerAtHeight(5)
  assert.equal(h1?.merkleRoot, 'mr')
  const h2 = await t.headerAtHeight(5)
  assert.equal(headerCalls, 2, 'one retry for 429, then cached — no third call')
  assert.deepEqual(h1, h2)
  assert.ok(await t.isValidRootForHeight('mr', 5))
  assert.ok(!(await t.isValidRootForHeight('nope', 5)))
})

test('tscToMerklePath: real tree roots verify for N=1..8', () => {
  const dsha = (b: Buffer) => Buffer.from(sha256(sha256(b)))
  const rootOf = (hashes: Buffer[]): Buffer => {
    if (hashes.length === 1) return hashes[0]!
    const next: Buffer[] = []
    for (let i = 0; i < hashes.length; i += 2) {
      const l = hashes[i]!, r = hashes[i + 1] ?? l
      next.push(dsha(Buffer.concat([l, r])))
    }
    return rootOf(next)
  }
  for (const n of [1, 2, 3, 5, 8]) {
    const internal = Array.from({ length: n }, (_, i) => dsha(Buffer.from(`tx${i}`))) // internal order
    const display = internal.map(h => Buffer.from(h).reverse().toString('hex'))
    const rootDisplay = Buffer.from(rootOf(internal)).reverse().toString('hex')
    for (let i = 0; i < n; i++) {
      // TSC nodes are display-order hex siblings, '*' = odd self-pair
      const nodes: string[] = []
      let idx = i, level = internal.map(h => Buffer.from(h))
      while (level.length > 1) {
        const sib = idx % 2 === 0 ? idx + 1 : idx - 1
        nodes.push(sib < level.length ? Buffer.from(level[sib]!).reverse().toString('hex') : '*')
        const next: Buffer[] = []
        for (let k = 0; k < level.length; k += 2) next.push(dsha(Buffer.concat([level[k]!, level[k + 1] ?? level[k]!])))
        level = next; idx = Math.floor(idx / 2)
      }
      const mp = tscToMerklePath({ index: i, txOrId: display[i]!, target: rootDisplay, nodes }, display[i]!, 100)
      assert.equal(mp.computeRoot(display[i]!), rootDisplay, `N=${n} idx=${i}`)
    }
  }
})

test('merklePathMainnet (ARC BUMP) + discoverUtxos with recorded fixtures', async () => {
  const bumpHex = buildBump(['aa'.repeat(32), 'bb'.repeat(32)], 0, 850_000).toHex()
  const fetchImpl = stubFetch({
    'arc.example/v1/tx/aa': { body: { txStatus: 'MINED', merklePath: bumpHex } },
    '/address/addr/unspent': { body: [{ tx_hash: 'bb'.repeat(32), tx_pos: 0, value: 5000 }] },
    '/tx/bb': { raw: '01000000010000000000000000000000000000000000000000000000000000000000000000ffffffff00ffffffff01e8030000000000001976a914000000000000000000000000000000000000000088ac00000000' },
  })
  const mp = await merklePathMainnet({ fetchImpl, arcUrl: 'http://arc.example', wocUrl: 'http://woc' }, 'aa'.repeat(32))
  assert.ok(mp?.merklePath, 'ARC BUMP hex parses')
  const utxos = await discoverUtxos({ fetchImpl, wocUrl: 'http://woc' }, 'addr')
  assert.equal(utxos.length, 1)
  assert.equal(utxos[0]!.satoshis, 5000)
})

// -- SpendGuard ----------------------------------------------------------------
const okBroadcast: Broadcaster = { broadcast: async () => ({ status: 'success', txid: 'x' }) }

test('SpendGuard: caps, interval, unknown input, ledger persistence', async () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'sg-'))
  const ledger = path.join(dir, 'ledger.json')
  // craft a tx spending parent:0 worth 10_000 sats with 9_000 out → 1000 fee
  const parent = new Transaction()
  parent.addOutput({ satoshis: 10_000, lockingScript: new P2PKH().lock('1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa') })
  const pid = parent.id('hex')
  const mkTx = () => {
    const t = new Transaction()
    t.addInput({ sourceTXID: pid, sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff })
    t.addOutput({ satoshis: 9000, lockingScript: parent.outputs[0]!.lockingScript })
    return t
  }
  const resolve = (id: string) => (id === pid ? 10_000 : undefined)

  // unknown input → fail closed
  const g0 = new SpendGuard(okBroadcast, () => undefined, { ledgerFile: ledger, maxFeePerTx: 5000, maxPerDay: 100_000 })
  assert.match((await g0.broadcast(mkTx())).description!, /unknown input value/)

  // per-tx cap
  const g1 = new SpendGuard(okBroadcast, resolve, { ledgerFile: ledger, maxFeePerTx: 500, maxPerDay: 100_000 })
  assert.match((await g1.broadcast(mkTx())).description!, /fee 1000 sats > maxFeePerTx 500/)

  // ok under caps → recorded
  const g2 = new SpendGuard(okBroadcast, resolve, { ledgerFile: ledger, maxFeePerTx: 5000, maxPerDay: 1500, minIntervalMs: 0 })
  assert.equal((await g2.broadcast(mkTx())).status, 'success')
  // per-day cap hit on second tx (1500-1000=500 remaining < 1000)
  assert.match((await g2.broadcast(mkTx())).description!, /daily cap/)
  assert.equal(g2.costs().length, 1)
  // ledger persisted: new instance sees the spend
  const g3 = new SpendGuard(okBroadcast, resolve, { ledgerFile: ledger, maxFeePerTx: 5000, maxPerDay: 1500 })
  assert.equal(g3.spentToday(), 1000)
  assert.match((await g3.broadcast(mkTx())).description!, /daily cap/)

  // interval
  const g4 = new SpendGuard(okBroadcast, resolve, { ledgerFile: path.join(dir, 'l2.json'), minIntervalMs: 60_000, refuseOnInterval: true })
  assert.equal((await g4.broadcast(mkTx())).status, 'success')
  assert.match((await g4.broadcast(mkTx())).description!, /min interval/)
  // default: the interval delays instead of refusing
  const g5 = new SpendGuard(okBroadcast, resolve, { ledgerFile: path.join(dir, 'l3.json'), minIntervalMs: 300 })
  assert.equal((await g5.broadcast(mkTx())).status, 'success')
  const t0 = Date.now()
  assert.equal((await g5.broadcast(mkTx())).status, 'success')
  assert.ok(Date.now() - t0 >= 250, 'second broadcast waited out the interval')
})
