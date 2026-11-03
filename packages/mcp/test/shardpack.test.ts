import { test } from 'node:test'
import assert from 'node:assert/strict'
import { MockChain, MockFunder } from '@mamem/chain'
import { MamemMemory, type MamemMemoryOptions } from '../src/memory.js'
import { callTool } from '../src/server.js'

function setup (opts: MamemMemoryOptions = {}) {
  const chain = new MockChain(); chain.verifyScripts = true
  const funder = new MockFunder(); funder.topup(50)
  for (const t of funder.parents) chain.registerParent(t)
  const mem = new MamemMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, modelId: 'test-model-1', ...opts })
  const settle = (shard: string) => { chain.mine(1); const l = mem.tailLink(shard); if (l) mem.confirmAnchor(shard, l.txid) }
  return { chain, funder, mem, settle }
}
/** second instance on the SAME network (shared MockChain) — the real case:
 *  an importer on a different network could never anchor-verify anyway */
function peerOf (a: ReturnType<typeof setup>, opts: MamemMemoryOptions = {}) {
  return new MamemMemory(a.chain, a.funder, a.funder.keyPair, { batchPolicy: { maxLeaves: 2 }, modelId: 'test-model-1', ...opts })
}

test('shard packs: export → import re-verifies every bundle; records provable on the peer', async () => {
  const a = setup()
  await a.mem.remember('guide', 'fact one')
  await a.mem.remember('guide', 'fact two')
  a.settle('guide')

  const pack = await a.mem.exportShardPack('guide')
  assert.equal(pack.v, 1); assert.equal(pack.kind, 'mamem-shard-pack'); assert.equal(pack.shard, 'guide')
  assert.equal(pack.records.length, 2)
  assert.ok(pack.records.every(r => r.bundle), 'anchored records carry their proof bundle')
  assert.ok(pack.records.every(r => typeof r.data === 'string' && typeof r.prov?.signature === 'string'))

  const b = peerOf(a)
  const imp = await b.importShardPack(pack)
  assert.equal(imp.imported, 2); assert.equal(imp.anchored, 2)
  assert.deepEqual(imp.unverifiable, [])

  // imported records are provable on the peer via the imported-bundle path
  const { bundle, result } = await b.prove('guide', 0)
  assert.ok(bundle); assert.ok(result.ok)

  // the import event itself is recorded on __imports__ (anchored lineage, not asserted)
  const imports = await b.records('__imports__')
  assert.ok(imports.some(i => Buffer.from(i.fragment.d).toString('utf8').includes('shard-import')))

  // a non-empty target refuses — imported + native seqs must never mix
  const c = peerOf(a)
  await c.remember('guide', 'native record')
  await assert.rejects(c.importShardPack(pack), /already has records/)

  // private shards refuse export outright; level>0 too
  const d = setup()
  d.mem.setShardPrivate('hr')
  await d.mem.remember('hr', 'salary band')
  await assert.rejects(d.mem.exportShardPack('hr'), /private/)
  const e = setup()
  await e.mem.remember('restricted', 'gate this', undefined, 5)
  await assert.rejects(e.mem.exportShardPack('restricted'), /level>0/)
})

test('shard packs: tampered data recomputes a different leaf → reported, never trusted', async () => {
  const a = setup()
  await a.mem.remember('guide', 'fact one')
  await a.mem.remember('guide', 'fact two')
  a.settle('guide')
  const pack = await a.mem.exportShardPack('guide')

  const b = peerOf(a)
  const tampered = JSON.parse(JSON.stringify(pack)) as typeof pack
  tampered.records[0]!.data = 'fact one, altered'
  const imp = await b.importShardPack(tampered)
  assert.equal(imp.imported, 2) // records land — the PACK is the evidence, not a blacklist
  assert.equal(imp.anchored, 1)
  assert.equal(imp.unverifiable.length, 1)
  assert.match(imp.unverifiable[0]!, /leaf/)

  // malformed pack → hard error, nothing lands
  const c = peerOf(a)
  await assert.rejects(c.importShardPack({ v: 2 }), /malformed/)
})

test('doctor: one-call diagnostic carries funding/shards/records/anchors/payments/prover', async () => {
  const { mem, settle } = setup()
  await mem.remember('s', 'x')
  await mem.remember('s', 'y')
  settle('s')
  const d = await callTool(mem, 'doctor', {})
  assert.equal(d.network, 'main')
  assert.equal(d.shards, 1); assert.equal(d.records, 2); assert.equal(d.anchoredRecords, 2)
  assert.ok(d.funding.address && d.funding.balanceSats > 0 && d.funding.funded === true)
  assert.equal(d.unprovenAnchors, 0)
  assert.equal(d.payments, 'off')
  assert.match(d.prover, /docker image|native binary|absent/)
})

test('groundedAnswer/commitReceipt share block: token + verifyUrl + badgeUrl + markdown', async () => {
  const { mem, settle } = setup()
  await mem.remember('manual', 'drain port 4 torque 45Nm')
  await mem.remember('manual', 'inspect belt every 10h')
  settle('manual')
  const r = await callTool(mem, 'groundedAnswer', { claims: [{ text: 'drain port 4 torque 45Nm', cites: [{ shard: 'manual', seq: 0 }] }] })
  assert.ok(r.share, 'share block present')
  assert.ok(r.share.token.startsWith('mamemr1.'))
  assert.ok(r.share.verifyUrl.includes('/v1/verify?r='))
  assert.ok(r.share.badgeUrl.includes('/v1/badge?r='))
  assert.match(r.share.markdown, /\[!?\[?[^\]]*\]\(.*\/v1\/badge\?r=.*\)\]\(.*\/v1\/verify\?r=.*\)|\/v1\/verify\?r=/)
})
