// GOAL-v6 S3 — §12 multi-chain consensus time.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { medianTime, weightedMedianTime, TemporalSources } from '../src/temporal.js'
import { WocTracker } from '../src/mainnet.js'
import type { BlockHeader, ChainTracker } from '../src/interfaces.js'

const hdr = (height: number, time: number): BlockHeader => ({ height, hash: `h${height}`, prevHash: 'p', merkleRoot: `r${height}`, time, bits: 0, nonce: 0 })
const stub = (times: Record<number, number>, miss = false): ChainTracker => ({
  currentHeight: async () => Math.max(...Object.keys(times).map(Number)),
  isValidRootForHeight: async (root, h) => root === `r${h}`,
  headerAtHeight: async (h) => (miss ? undefined : times[h] === undefined ? undefined : hdr(h, times[h]!))
})

test('medianTime: odd → middle; even → lower median (skewed-high can never push time up)', () => {
  assert.equal(medianTime([7]), 7)
  assert.equal(medianTime([3, 1, 2]), 2)
  assert.equal(medianTime([100, 200]), 100, 'lower weighted median')
  assert.equal(
    weightedMedianTime([
      { time: 100, weight: 1 },
      { time: 999, weight: 3 }
    ]),
    999,
    'weights decide'
  )
})

test('TemporalSources: a skewed second source does not move consensus time', async () => {
  const bsv = stub({ 10: 1_700_000_000 })
  const btc = stub({ 10: 1_700_000_000 + 99_999 }) // BTC clock wildly skewed high
  const t = new TemporalSources([
    { name: 'bsv', tracker: bsv },
    { name: 'btc', tracker: btc }
  ])
  assert.equal(await t.consensusTime(10), 1_700_000_000, 'median follows BSV, not the skewed clock')
  const h = await t.headerAtHeight(10)
  assert.equal(h!.time, 1_700_000_000)
  // and when BSV is the skewed one, BTC pulls the bound down — the weighted
  // median is symmetric in the sources
  const t2 = new TemporalSources([
    { name: 'bsv', tracker: stub({ 10: 1_700_000_000 + 99_999 }) },
    { name: 'btc', tracker: stub({ 10: 1_700_000_000 }) }
  ])
  assert.equal(await t2.consensusTime(10), 1_700_000_000)
  // weights: an overweighted source wins regardless of order
  const t3 = new TemporalSources([
    { name: 'bsv', tracker: bsv, weight: 3 },
    { name: 'btc', tracker: btc }
  ])
  assert.equal(await t3.consensusTime(10), 1_700_000_000)
})

test('TemporalSources: single source is bit-identical to the raw tracker', async () => {
  const bsv = stub({ 5: 12345 })
  const t = new TemporalSources([{ name: 'bsv', tracker: bsv }])
  const a = await bsv.headerAtHeight(5)
  const b = await t.headerAtHeight(5)
  assert.deepEqual(b, a, 'same header object, untouched')
  assert.equal(await t.consensusTime(5), 12345)
  assert.equal(await t.currentHeight(), await bsv.currentHeight())
  assert.equal(await t.isValidRootForHeight('r5', 5), true)
})

test('TemporalSources: a missing source abstains; hash/root stay on the primary chain', async () => {
  const bsv = stub({ 7: 500 })
  const t = new TemporalSources([
    { name: 'bsv', tracker: bsv },
    { name: 'btc', tracker: stub({}, true) }
  ])
  const h = await t.headerAtHeight(7)
  assert.equal(h!.time, 500, 'only BSV voted')
  assert.equal(h!.merkleRoot, 'r7', 'root is BSV, never the external chain')
})

test('BTC source: a second WocTracker pointed at /v1/btc/main (read-only stub fetch)', async () => {
  const calls: string[] = []
  const fetchImpl: typeof fetch = async (input: any, init?: any) => {
    calls.push(`${init?.method ?? 'GET'} ${String(input)}`)
    return new Response(JSON.stringify({ hash: 'x', previousblockhash: 'p', merkleroot: 'm', time: 1_600_000_000, bits: '1d00ffff', nonce: 1 }), {
      status: 200
    })
  }
  const btc = new WocTracker({ wocUrl: 'https://api.whatsonchain.com/v1/btc/main', fetchImpl })
  const h = await btc.headerAtHeight(700_000)
  assert.equal(h!.time, 1_600_000_000)
  assert.ok(
    calls.every((c) => c.startsWith('GET ')),
    'tracker is read-only: no POST'
  )
  assert.ok(
    calls.some((c) => c.includes('/v1/btc/main/block/height/700000')),
    'hits the BTC endpoint'
  )
})

test('BTC source is only read when enabled: bsv-only sources never touch it', async () => {
  let calls = 0
  const fetchImpl: typeof fetch = async () => {
    calls++
    return new Response('{}', { status: 404 })
  }
  const btc = new WocTracker({ wocUrl: 'https://api.whatsonchain.com/v1/btc/main', fetchImpl })
  const t = new TemporalSources([{ name: 'bsv', tracker: stub({ 3: 42 }) }])
  await t.headerAtHeight(3)
  assert.equal(calls, 0, 'no BTC fetch when btc is not configured')
})
