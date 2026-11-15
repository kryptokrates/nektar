import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Hash } from '@bsv/sdk'
import { serializeHeader, headerPowValid, linksTo, WitnessedTracker } from '../src/headers.js'
import { ChaintracksTracker, WocTracker } from '../src/mainnet.js'
import type { BlockHeader, ChainTracker } from '../src/interfaces.js'

// real mainnet header at height 800000 — the ground-truth PoW vector
const H800K: BlockHeader = {
  height: 800000,
  version: 770793472,
  prevHash: '00000000000000000b6ae23bbe9f549844c20943d8c20b8ceedbae8aa1dde8e0',
  merkleRoot: '244993b9d8d961f5b4c91afa569adf9b6d8cd18e0bb6f769f5e62cdf2cc1468d',
  time: 1688957834,
  bits: 403527837,
  nonce: 742652432,
  hash: '000000000000000000ad9056924410005d91b57f100bce345944e5caf56e8565'
}

// a fabricated header carrying REAL self-consistent PoW (trivial bits) so the
// tracker tests exercise the true validation path rather than the opt-out
const fakeHeader = (height: number, prevHash: string, merkleRoot = '11'.repeat(32)): BlockHeader => {
  const h: BlockHeader = { height, version: 1, prevHash, merkleRoot, time: 1_700_000_000, bits: 0x207fffff, nonce: 0, hash: '' }
  for (;;) {
    const raw = serializeHeader(h)!
    h.hash = Buffer.from(Hash.hash256([...raw]))
      .reverse()
      .toString('hex')
    if (headerPowValid(h)) return h
    h.nonce++
  }
}
const wocJson = (h: BlockHeader) => ({
  hash: h.hash,
  previousblockhash: h.prevHash,
  merkleroot: h.merkleRoot,
  time: h.time,
  bits: h.bits.toString(16),
  nonce: h.nonce,
  version: h.version
})
const chaintracksJson = (h: BlockHeader) => ({
  version: h.version,
  previousHash: h.prevHash,
  merkleRoot: h.merkleRoot,
  time: h.time,
  bits: h.bits,
  nonce: h.nonce,
  height: h.height,
  hash: h.hash
})
const stubFetch = (routes: Record<string, { status?: number; body?: any; raw?: string }>) => {
  const calls: string[] = []
  const f: typeof fetch = async (input: any) => {
    const url = String(input)
    calls.push(url)
    for (const [k, v] of Object.entries(routes)) if (url.includes(k)) return new Response(v.raw ?? JSON.stringify(v.body ?? {}), { status: v.status ?? 200 })
    return new Response('not found', { status: 404 })
  }
  ;(f as any).calls = calls
  return f
}
const stubTracker = (h?: BlockHeader, mode: 'ok' | 'throw' | 'slow' = 'ok'): ChainTracker => ({
  currentHeight: async () => 999,
  tipHeight: async () => 999,
  headerAtHeight: async () => {
    if (mode === 'slow') await new Promise((r) => setTimeout(r, 300))
    if (mode === 'throw') throw new Error('witness down')
    return h
  },
  isValidRootForHeight: async (root, height) => (await stubTracker(h).headerAtHeight(height))?.merkleRoot === root
})

test('headerPowValid: the real 800000 header validates; forgeries do not', () => {
  assert.equal(headerPowValid(H800K), true)
  const forged = { ...H800K, merkleRoot: '1' + H800K.merkleRoot.slice(1) }
  assert.equal(headerPowValid(forged), false, 'one flipped merkleRoot nibble breaks the claimed hash')
  assert.equal(headerPowValid({ ...H800K, nonce: H800K.nonce + 1 }), false, 'nonce+1 breaks the claimed hash')
  const { version: _v, ...noVersion } = H800K
  assert.equal(headerPowValid(noVersion as BlockHeader), false, 'no version → cannot serialize → cannot validate')
  assert.equal(serializeHeader(H800K)!.length, 80)
})

test('ChaintracksTracker: a PoW-invalid answer is treated as not served', async () => {
  const forged = { ...H800K, merkleRoot: '0'.repeat(64) }
  const routes = { '/header/height/800000': { body: chaintracksJson(forged) } }
  const t = new ChaintracksTracker({ fetchImpl: stubFetch(routes) }, 'http://ct.test/chaintracks/v2')
  assert.equal(await t.isValidRootForHeight(forged.merkleRoot, 800000), false, 'a forged root must not be served as truth')
  assert.equal(await t.headerAtHeight(800000), undefined, 'the invalid answer is not cached either')
  routes['/header/height/800000'] = { body: chaintracksJson(H800K) }
  assert.equal(await t.isValidRootForHeight(H800K.merkleRoot, 800000), true, 'the genuine header serves its root')
  assert.equal(await t.isValidRootForHeight('0'.repeat(64), 800000), false, 'a wrong root is refused')
})

test('WitnessedTracker: agree corroborates, disagree fails closed, silence degrades', async () => {
  const h = fakeHeader(800000, 'aa'.repeat(32))
  // agree
  const wt = new WitnessedTracker(stubTracker(h), stubTracker(h))
  assert.equal((await wt.headerAtHeight(800000))?.hash, h.hash)
  assert.equal(wt.corroborated(800000), true)
  // disagree → fail closed + onDisagree
  let disagreement = ''
  const alt = fakeHeader(800000, 'bb'.repeat(32))
  const wd = new WitnessedTracker(stubTracker(h), stubTracker(alt), {
    onDisagree: (height, ph, wh) => (disagreement = `${height}:${ph === h.hash}:${wh === alt.hash}`)
  })
  assert.equal(await wd.headerAtHeight(800000), undefined, 'a split serves no root')
  assert.equal(disagreement, '800000:true:true', 'onDisagree reports both hashes')
  assert.equal(wd.corroborated(800000), false)
  // witness throws → non-strict serves primary uncorroborated; strict fails closed
  const wn = new WitnessedTracker(stubTracker(h), stubTracker(undefined, 'throw'))
  assert.equal((await wn.headerAtHeight(800000))?.hash, h.hash)
  assert.equal(wn.corroborated(800000), false)
  const ws = new WitnessedTracker(stubTracker(h), stubTracker(undefined, 'throw'), { strict: true })
  assert.equal(await ws.headerAtHeight(800000), undefined, 'strict refuses without the witness')
  // slow witness → uncorroborated now, corroborated once its answer lands
  const slow = new WitnessedTracker(stubTracker(h), stubTracker(h, 'slow'), { witnessTimeoutMs: 50 })
  assert.equal((await slow.headerAtHeight(800000))?.hash, h.hash)
  assert.equal(slow.corroborated(800000), false, 'a timed-out witness leaves the height uncorroborated')
  await new Promise((r) => setTimeout(r, 400))
  assert.equal((await slow.headerAtHeight(800000))?.hash, h.hash)
  assert.equal(slow.corroborated(800000), true, 'the landed witness answer corroborates a later read')
  // primary down → non-strict falls back to the witness
  const wp = new WitnessedTracker(stubTracker(undefined, 'throw'), stubTracker(h))
  assert.equal((await wp.headerAtHeight(800000))?.hash, h.hash)
  assert.equal(wp.corroborated(800000), false)
})

test('reorg: a fresh header that does not link to the cached parent fires onReorg once', async () => {
  const h100 = fakeHeader(100, 'aa'.repeat(32))
  const h101 = fakeHeader(101, h100.hash)
  const h101b = fakeHeader(101, h100.hash, '22'.repeat(32)) // replacement block at the same height
  const h102 = fakeHeader(102, h101b.hash)
  const routes: Record<string, { body: any }> = {
    '/block/height/100': { body: wocJson(h100) },
    '/block/height/101': { body: wocJson(h101) },
    '/block/height/102': { body: wocJson(h102) }
  }
  const t = new WocTracker({ fetchImpl: stubFetch(routes) })
  let fired = 0
  let removed: BlockHeader[] = []
  let tip: BlockHeader | undefined
  t.onReorg((r, n) => {
    fired++
    removed = r
    tip = n
  })
  await t.headerAtHeight(100)
  await t.headerAtHeight(101)
  // the source reorgs: 101' and 102 land where 101 used to be
  routes['/block/height/101'] = { body: wocJson(h101b) }
  const fresh = await t.headerAtHeight(102)
  assert.equal(fresh?.hash, h102.hash)
  assert.equal(fired, 1, 'the reorg fires exactly once per divergence')
  assert.deepEqual(
    removed.map((x) => x.hash),
    [h101.hash],
    'the stale cached 101 is reported, highest first'
  )
  assert.equal(tip?.hash, h102.hash, 'newTip is the freshest fetched header')
  const served = await t.headerAtHeight(101)
  assert.equal(served?.hash, h101b.hash, 'the cache now serves the replacement 101')
})

// a tracker stub with a real (if trivial) cache — refreshHeader bypasses it,
// like the concrete trackers do
const cacheTracker = (upstream: () => BlockHeader | undefined): ChainTracker => {
  const cache = new Map<number, BlockHeader>()
  return {
    currentHeight: async () => 999,
    headerAtHeight: async (h) => {
      const hit = cache.get(h)
      if (hit !== undefined) return hit
      const x = upstream()
      if (x !== undefined) cache.set(h, x)
      return x
    },
    refreshHeader: async (h) => {
      const x = upstream()
      if (x !== undefined) cache.set(h, x)
      return x
    },
    isValidRootForHeight: async () => true
  }
}

test('WitnessedTracker: a stale witness cache heals through refreshHeader', async () => {
  const a = fakeHeader(800000, 'aa'.repeat(32))
  const b = fakeHeader(800000, 'aa'.repeat(32), '22'.repeat(32)) // the replacement block
  let pUp: BlockHeader | undefined = a,
    wUp: BlockHeader | undefined = a
  const primary = cacheTracker(() => pUp),
    witness = cacheTracker(() => wUp)
  let disagreements = 0
  const wt = new WitnessedTracker(primary, witness, { onDisagree: () => disagreements++ })
  assert.equal((await wt.headerAtHeight(800000))?.hash, a.hash)
  assert.equal(wt.corroborated(800000), true)
  // the chain reorgs at 800000: the primary's own cache refreshes to B (its
  // reorg machinery already ran), the witness upstream serves B but its
  // cache still holds A — the stale answer must NOT become a disagreement
  pUp = b
  wUp = b
  await primary.refreshHeader!(800000)
  const h = await wt.headerAtHeight(800000)
  assert.equal(h?.hash, b.hash, 'the refreshed witness answer agrees — the stale cache was healed, not trusted')
  assert.equal(wt.corroborated(800000), true)
  assert.equal(disagreements, 0, 'a healed mismatch never reaches onDisagree')
})

test('WitnessedTracker: a disagreement that survives refresh still fails closed', async () => {
  const a = fakeHeader(800000, 'aa'.repeat(32))
  const b = fakeHeader(800000, 'bb'.repeat(32))
  let disagreements = ''
  const wt = new WitnessedTracker(
    cacheTracker(() => a),
    cacheTracker(() => b),
    { onDisagree: (h2, ph, wh) => (disagreements += `${h2}:${ph === a.hash}:${wh === b.hash};`) }
  )
  assert.equal(await wt.headerAtHeight(800000), undefined, 'a real split serves no root')
  assert.equal(disagreements, '800000:true:true;', 'onDisagree fired once with both hashes')
  // inside the retry window a second read fails closed without re-refreshing
  assert.equal(await wt.headerAtHeight(800000), undefined)
  assert.equal(disagreements, '800000:true:true;', 'still once — the verdict holds for retryMs')
})

test('linksTo: parent-child linkage is hash + adjacent height', () => {
  const a = fakeHeader(10, 'aa'.repeat(32))
  const b = fakeHeader(11, a.hash)
  assert.equal(linksTo(b, a), true)
  assert.equal(linksTo(a, b), false)
  assert.equal(linksTo(fakeHeader(11, 'cc'.repeat(32)), a), false, 'wrong prevHash is not a link')
  assert.equal(linksTo(fakeHeader(12, a.hash), a), false, 'height gap is not a link')
})
