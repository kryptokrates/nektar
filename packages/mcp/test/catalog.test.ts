// Shard manifests (describeShard → __catalog__): the committed "what this
// shard covers" claim other agents discover by domain tag. Covers the memory
// layer, the pack flow, and the discovery-doc projection.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { MockChain, MockFunder } from '@nektar/chain'
import { NektarMemory, type NektarMemoryOptions, CATALOG_SHARD } from '../src/memory.js'
import { callTool } from '../src/server.js'
import { createHttpApi } from '../src/http.js'
import type { AddressInfo } from 'node:net'

function setup (opts: NektarMemoryOptions = {}) {
  const chain = new MockChain(); chain.verifyScripts = true
  const funder = new MockFunder(); funder.topup(50)
  for (const t of funder.parents) chain.registerParent(t)
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, ...opts })
  const settle = (shard: string) => { chain.mine(1); const l = mem.tailLink(shard); if (l) mem.confirmAnchor(shard, l.txid) }
  return { chain, funder, mem, settle }
}

test('describeShard: manifest committed to __catalog__, shardId binds label to chain', async () => {
  const { mem } = setup()
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

test('describeShard: unknown/system shards and junk tags refused; latest wins', async () => {
  const { mem } = setup()
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
  const { mem } = setup()
  await mem.remember('ball', 'Metz beat Lyon 2-1')
  const out = await callTool(mem, 'describeShard', { shard: 'ball', domains: ['soccer'], desc: 'football', offers: [{ kind: 'pack' }, { kind: 'record', seq: 0, priceSats: 500 }] })
  assert.equal(out.manifest.shardId.length, 40)

  const api = createHttpApi(mem)
  await new Promise<void>(res => api.listen(0, res))
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

test('exportShardPack carries the manifest as advisory metadata', async () => {
  const { mem } = setup()
  await mem.remember('ball', 'Metz beat Lyon 2-1')
  await mem.describeShard('ball', { domains: ['soccer'] })
  const pack = await mem.exportShardPack('ball')
  assert.equal(pack.manifest?.shard, 'ball')
  assert.deepEqual(pack.manifest?.domains, ['soccer'])
})
