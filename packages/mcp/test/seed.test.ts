import { test } from 'node:test'
import assert from 'node:assert/strict'
import { seedFromDirectory, type SeedRead } from '../src/seed.js'
import type { AnnounceAd } from '@nektar/bstack'

// the injected `read` returns items in the post-verify shape readAnnounces
// emits — signature/SPV/expiry checks happen inside the real read, so what
// arrives here is already accepted. seedFromDirectory's own logic is what's
// under test: domain filtering, newest-announce dedupe, ref selection,
// host union, and failure isolation.
const pub = 'a'.repeat(66)
const item = (announce: Partial<AnnounceAd>, txid = `t${announce.streamSeq ?? 0}-${announce.ts}`) => ({
  txid,
  payload: { body: { announce: { pub, hosts: ['https://h.example'], ts: 1, ...announce }, sig: 'x' } }
})
const mem = { sdkTracker: () => ({}) as never }
const calls: Array<{ tool: string; args: Record<string, unknown> }> = []
const call = (tool: string, args: Record<string, unknown>) => {
  calls.push({ tool, args })
  return Promise.resolve({})
}

void test('seed: domain filter + newest announce wins', async () => {
  calls.length = 0
  const read: SeedRead = () =>
    Promise.resolve({
      items: [
        item({
          streamSeq: 2,
          ts: 2,
          packs: [
            { shard: 'ops1', domains: ['ops'], store: { key: 'k', sha256: 's' } },
            { shard: 'x1', domains: ['x'], packUrl: 'https://p/x' }
          ]
        }),
        item({ streamSeq: 1, ts: 1, packs: [{ shard: 'old', domains: ['ops'], store: { key: 'k2', sha256: 's2' } }] })
      ]
    })
  const r = await seedFromDirectory({ mem, call, hosts: ['https://seed'], net: 'testnet', domains: ['ops'], read })
  assert.equal(r.imported, 1)
  assert.deepEqual(calls, [{ tool: 'importShard', args: { store: { key: 'k', sha256: 's' } } }])
})

void test('seed: post-restart card (low seq, new ts) beats an unexpired stale card', async () => {
  calls.length = 0
  const read: SeedRead = () =>
    Promise.resolve({
      items: [
        // producer restarted: streamState reset → the fresh card carries seq 1
        // while the pre-restart seq-9 card is still live on the lane
        item({ streamSeq: 9, ts: 5, packs: [{ shard: 'stale', packUrl: 'https://p/stale' }] }),
        item({ streamSeq: 1, ts: 9, packs: [{ shard: 'fresh', packUrl: 'https://p/fresh' }] })
      ]
    })
  const r = await seedFromDirectory({ mem, call, hosts: ['https://seed'], net: 'testnet', domains: [], all: true, read })
  assert.equal(r.imported, 1)
  assert.deepEqual(calls, [{ tool: 'importShard', args: { packUrl: 'https://p/fresh' } }])
})

void test('seed: wildcard imports everything; store beats packUrl; refless packs skip', async () => {
  calls.length = 0
  const read: SeedRead = () =>
    Promise.resolve({
      items: [item({ packs: [{ shard: 'a', store: { chirp: 'chirp://x' }, packUrl: 'https://p/a' }, { shard: 'b', packUrl: 'https://p/b' }, { shard: 'c' }] })]
    })
  const r = await seedFromDirectory({ mem, call, hosts: [], net: 'testnet', domains: [], all: true, read })
  assert.equal(r.imported, 2)
  assert.deepEqual(calls[0].args, { store: { chirp: 'chirp://x' } })
  assert.deepEqual(calls[1].args, { packUrl: 'https://p/b' })
})

void test('seed: failures isolate; already-imported counts as skipped', async () => {
  calls.length = 0
  const failCall = (_t: string, args: Record<string, unknown>) => {
    if (args.packUrl === 'https://p/dup') throw new Error('importShard: shard "dup" already has records — import needs an empty shard')
    if (args.packUrl === 'https://p/bad') throw new Error('importShard: fetch → 500')
    return Promise.resolve({})
  }
  const read: SeedRead = () =>
    Promise.resolve({
      items: [
        item({
          packs: [
            { shard: 'dup', packUrl: 'https://p/dup' },
            { shard: 'bad', packUrl: 'https://p/bad' },
            { shard: 'ok', packUrl: 'https://p/ok' }
          ]
        })
      ]
    })
  const r = await seedFromDirectory({ mem, call: failCall, hosts: [], net: 'testnet', domains: [], all: true, read })
  assert.deepEqual({ imported: r.imported, skipped: r.skipped, failed: r.failed }, { imported: 1, skipped: 1, failed: 1 })
})

void test('seed: announced hosts union into a second read (screened); a dead publisher still imports', async () => {
  calls.length = 0
  const seen: string[][] = []
  const read: SeedRead = (hosts) => {
    seen.push(hosts)
    if (seen.length === 1)
      return Promise.resolve({
        items: [
          item({
            pub: 'b'.repeat(66),
            hosts: ['https://hub.example/hosts/a', 'javascript://evil'],
            packs: [{ shard: 's1', packUrl: 'https://hub.example/v1/pack/s1' }]
          }),
          item({ pub: 'c'.repeat(66), hosts: [], packs: [] })
        ]
      })
    return Promise.resolve({ items: [item({ pub: 'd'.repeat(66), hosts: [], packs: [{ shard: 's2', packUrl: 'https://hub.example/v1/pack/s2' }] })] })
  }
  const r = await seedFromDirectory({ mem, call, hosts: ['https://seed'], net: 'testnet', domains: [], all: true, read })
  assert.equal(seen.length, 2)
  assert.deepEqual(seen[1], ['https://hub.example/hosts/a'])
  assert.equal(r.hostsAdded, 1)
  assert.equal(r.imported, 2)
  assert.equal(r.announces, 3)
})
