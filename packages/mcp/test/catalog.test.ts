// Shard manifests (describeShard → __catalog__): the committed "what this
// shard covers" claim other agents discover by domain tag. Covers the memory
// layer, the pack flow, and the discovery-doc projection.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { stackWorld, stackChain, StackFunder } from '@nektar/chain'
import { NektarMemory, type NektarMemoryOptions, CATALOG_SHARD } from '../src/memory.js'
import { callTool } from '../src/server.js'
import { createHttpApi } from '../src/http.js'
import type { AddressInfo } from 'node:net'

async function setup(t: { skip: (m?: string) => void }, opts: NektarMemoryOptions = {}) {
  const w = await stackWorld(t, 50)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, ...opts })
  const settle = async (shard: string) => {
    await chain.mine(1)
    const l = mem.tailLink(shard)
    if (l) mem.confirmAnchor(shard, l.txid)
  }
  return { chain, funder, mem, settle }
}

test('describeShard: manifest committed to __catalog__, shardId binds label to chain', async () => {
  const _w = await setup(t)
  if (_w === undefined) return
  const { mem } = _w
  await mem.remember('ball', 'Metz beat Lyon 2-1')
  const r = await mem.describeShard('ball', { domains: ['Soccer', 'Ligue-1'], desc: 'French football results', offers: [{ kind: 'answer', priceSats: 250 }] })
  assert.equal(r.manifest.shard, 'ball')
  assert.deepEqual(r.manifest.domains, ['soccer', 'ligue-1'], 'tags normalized lowercase')
  assert.match(r.manifest.shardId, /^[0-9a-f]{40}$/, 'shard id = hash160 of the shard key')
  const got = await mem.shardManifest('ball')
  assert.equal(got?.desc, 'French football results')
  // the claim is a real anchored record, not a mutable field
  const cat = await mem.records(CATALOG_SHARD)
  assert.equal(cat.length, 1)
  assert.equal(JSON.parse(Buffer.from(cat[0]!.fragment.d).toString('utf8')).type, 'shard-manifest')
})

test('describeShard: unknown/system shards and junk tags refused; latest wins', async (t) => {
  const _w = await setup(t)
  if (_w === undefined) return
  const { mem } = _w
  await assert.rejects(() => mem.describeShard('nope', { domains: ['x'] }), /unknown shard/)
  await mem.remember('ball', 'x')
  await assert.rejects(() => mem.describeShard('__catalog__', { domains: ['x'] }), /system shards/)
  await assert.rejects(() => mem.describeShard('ball', { domains: [] }), /valid domain/)
  await assert.rejects(() => mem.describeShard('ball', { domains: ['***bad***'] }), /valid domain/)
  await mem.describeShard('ball', { domains: ['soccer'] })
  await mem.describeShard('ball', { domains: ['cricket'] })
  assert.deepEqual((await mem.shardManifest('ball'))?.domains, ['cricket'], 'latest manifest wins')
  assert.equal((await mem.shardCatalog()).length, 1)
})

test('describeShard via tool + /.well-known/nektar projects domains/offers/shardId', async (t) => {
  const _w = await setup(t)
  if (_w === undefined) return
  const { mem } = _w
  await mem.remember('ball', 'Metz beat Lyon 2-1')
  const out = await callTool(mem, 'describeShard', {
    shard: 'ball',
    domains: ['soccer'],
    desc: 'football',
    offers: [{ kind: 'pack' }, { kind: 'record', seq: 0, priceSats: 500 }]
  })
  assert.equal(out.manifest.shardId.length, 40)

  const api = createHttpApi(mem)
  await new Promise<void>((res) => api.listen(0, res))
  t.after(() => api.close())
  const port = (api.address() as AddressInfo).port
  const doc = await (await fetch(`http://localhost:${port}/.well-known/nektar`)).json()
  const entry = doc.publicShards.find((s: any) => s.shard === 'ball')
  assert.ok(entry, 'described shard listed')
  assert.deepEqual(entry.domains, ['soccer'])
  assert.equal(entry.shardId, out.manifest.shardId)
  assert.equal(entry.offers.length, 2)
  assert.equal(doc.logPublicKey, mem.logPublicKey())
})

test('exportShardPack carries the manifest as advisory metadata', async (t) => {
  const _w = await setup(t)
  if (_w === undefined) return
  const { mem } = _w
  await mem.remember('ball', 'Metz beat Lyon 2-1')
  await mem.describeShard('ball', { domains: ['soccer'] })
  const pack = await mem.exportShardPack('ball')
  assert.equal(pack.manifest?.shard, 'ball')
  assert.deepEqual(pack.manifest?.domains, ['soccer'])
})

test('importRecord: bought payload lands + __imports__ audit; forged provenance refused', async (t) => {
  const _w = await setup(t)
  if (_w === undefined) return
  const { mem } = _w
  const r = await mem.importRecord({ data: 'Metz beat Lyon 2-1', sourceShard: 'ball', sourceSeq: 3, leaf: 'd'.repeat(64), saleTxid: 'e'.repeat(64) })
  assert.equal(r.seq, 0)
  const recs = await mem.records('bought')
  assert.equal(recs.length, 1)
  const audit = await mem.records('__imports__')
  const a = JSON.parse(Buffer.from(audit[0]!.fragment.d).toString('utf8'))
  assert.equal(a.type, 'record-import')
  assert.equal(a.fromShard, 'ball')
  assert.equal(a.saleTxid, 'e'.repeat(64))
  assert.equal(a.toShard, 'bought')
  // provenance is fail-closed: a claim signed by a key that isn't sourcePub's rejects
  await assert.rejects(
    () => mem.importRecord({ data: 'forged', provenance: { sourcePub: 'a'.repeat(66), hash: 'b'.repeat(64), timestamp: 1, sig: 'c'.repeat(142) } }),
    /provenance signature invalid/
  )
})

test('contentPrice: manifest offers drive external read/pack pricing', async (t) => {
  const _w = await setup(t)
  if (_w === undefined) return
  const { mem } = _w
  await mem.remember('ball', 'x')
  assert.equal(await mem.contentPrice('ball', 'read'), 0, 'no manifest → free')
  await mem.describeShard('ball', {
    domains: ['soccer'],
    offers: [
      { kind: 'answer', priceSats: 300 },
      { kind: 'pack', priceSats: 2000 }
    ]
  })
  assert.equal(await mem.contentPrice('ball', 'read'), 300)
  assert.equal(await mem.contentPrice('ball', 'pack'), 2000)
})
