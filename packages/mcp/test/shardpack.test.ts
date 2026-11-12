import { test } from 'node:test'
import assert from 'node:assert/strict'
import { stackWorld, stackChain, StackFunder } from '@nektar/chain'
import type { AddressInfo } from 'node:net'
import { NektarMemory, type NektarMemoryOptions } from '../src/memory.js'
import { callTool } from '../src/server.js'

async function setup(t: { skip: (m?: string) => void }, opts: NektarMemoryOptions = {}) {
  const w = await stackWorld(t, 50)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, modelId: 'test-model-1', ...opts })
  const settle = async (shard: string) => {
    await chain.mine(1)
    const l = mem.tailLink(shard)
    if (l) mem.confirmAnchor(shard, l.txid)
  }
  return { chain, funder, mem, settle }
}
/** second instance on the SAME network (shared MockChain) — the real case:
 *  an importer on a different network could never anchor-verify anyway */
function peerOf(a: NonNullable<Awaited<ReturnType<typeof setup>>>, opts: NektarMemoryOptions = {}) {
  return new NektarMemory(a.chain, a.funder, a.funder.keyPair, { batchPolicy: { maxLeaves: 2 }, modelId: 'test-model-1', ...opts })
}

test('shard packs: export → import re-verifies every bundle; records provable on the peer', async (t) => {
  const a = await setup(t)
  if (a === undefined) return
  await a.mem.remember('guide', 'fact one')
  await a.mem.remember('guide', 'fact two')
  a.settle('guide')

  const pack = await a.mem.exportShardPack('guide')
  assert.equal(pack.v, 1)
  assert.equal(pack.kind, 'nektar-shard-pack')
  assert.equal(pack.shard, 'guide')
  assert.equal(pack.records.length, 2)
  assert.ok(
    pack.records.every((r) => r.bundle),
    'anchored records carry their proof bundle'
  )
  assert.ok(pack.records.every((r) => typeof r.data === 'string' && typeof r.prov?.signature === 'string'))

  const b = await peerOf(a)
  if (b === undefined) return
  const imp = await b.importShardPack(pack)
  assert.equal(imp.imported, 2)
  assert.equal(imp.anchored, 2)
  assert.deepEqual(imp.unverifiable, [])

  // imported records are provable on the peer via the imported-bundle path
  const { bundle, result } = await b.prove('guide', 0)
  assert.ok(bundle)
  assert.ok(result.ok)

  // the import event itself is recorded on __imports__ (anchored lineage, not asserted)
  const imports = await b.records('__imports__')
  assert.ok(imports.some((i) => Buffer.from(i.fragment.d).toString('utf8').includes('shard-import')))

  // a non-empty target refuses — imported + native seqs must never mix
  const c = await peerOf(a)
  if (c === undefined) return
  await c.remember('guide', 'native record')
  await assert.rejects(c.importShardPack(pack), /already has records/)

  // private shards refuse export outright; level>0 too
  const d = await setup(t)
  if (d === undefined) return
  d.mem.setShardPrivate('hr')
  await d.mem.remember('hr', 'salary band')
  await assert.rejects(d.mem.exportShardPack('hr'), /private/)
  const e = await setup(t)
  if (e === undefined) return
  await e.mem.remember('restricted', 'gate this', undefined, 5)
  await assert.rejects(e.mem.exportShardPack('restricted'), /level>0/)
})

test('shard packs: tampered data recomputes a different leaf → reported, never trusted', async t => {
  const a = await setup(t)
  if (a === undefined) return
  await a.mem.remember('guide', 'fact one')
  await a.mem.remember('guide', 'fact two')
  a.settle('guide')
  const pack = await a.mem.exportShardPack('guide')

  const b = await peerOf(a)
  if (b === undefined) return
  const tampered = JSON.parse(JSON.stringify(pack)) as typeof pack
  tampered.records[0]!.data = 'fact one, altered'
  const imp = await b.importShardPack(tampered)
  assert.equal(imp.imported, 2) // records land — the PACK is the evidence, not a blacklist
  assert.equal(imp.anchored, 1)
  assert.equal(imp.unverifiable.length, 1)
  assert.match(imp.unverifiable[0]!, /leaf/)

  // malformed pack → hard error, nothing lands
  const c = await peerOf(a)
  if (c === undefined) return
  await assert.rejects(c.importShardPack({ v: 2 }), /malformed/)
})

test('doctor: one-call diagnostic carries funding/shards/records/anchors/payments/prover', async (t) => {
  const _w = await setup(t)
  if (_w === undefined) return
  const { mem, settle } = _w
  await mem.remember('s', 'x')
  await mem.remember('s', 'y')
  settle('s')
  const d = await callTool(mem, 'doctor', {})
  assert.equal(d.network, 'main')
  assert.equal(d.shards, 1)
  assert.equal(d.records, 2)
  assert.equal(d.anchoredRecords, 2)
  assert.ok(d.funding.address && d.funding.balanceSats > 0 && d.funding.funded === true)
  assert.equal(d.unprovenAnchors, 0)
  assert.equal(d.payments, 'off')
  assert.match(d.prover, /docker image|native binary|absent/)
})

test('groundedAnswer/commitReceipt share block: token + verifyUrl + badgeUrl + markdown', async (t) => {
  const _w = await setup(t)
  if (_w === undefined) return
  const { mem, settle } = _w
  await mem.remember('manual', 'drain port 4 torque 45Nm')
  await mem.remember('manual', 'inspect belt every 10h')
  settle('manual')
  const r = await callTool(mem, 'groundedAnswer', { claims: [{ text: 'drain port 4 torque 45Nm', cites: [{ shard: 'manual', seq: 0 }] }] })
  assert.ok(r.share, 'share block present')
  assert.ok(r.share.token.startsWith('nektarr1.'))
  assert.ok(r.share.verifyUrl.includes('/v1/verify?r='))
  assert.ok(r.share.badgeUrl.includes('/v1/badge?r='))
  assert.match(r.share.markdown, /\[!?\[?[^\]]*\]\(.*\/v1\/badge\?r=.*\)\]\(.*\/v1\/verify\?r=.*\)|\/v1\/verify\?r=/)
})

test('pack endpoint + remote import: /.well-known/nektar lists offerable shards; importShard {from,shard} pulls + re-verifies', async t => {
  const { createHttpApi } = await import('../src/http.js')
  const a = await setup(t)
  if (a === undefined) return
  await a.mem.remember('guide', 'fact one')
  await a.mem.remember('guide', 'fact two')
  a.settle('guide')
  a.mem.setShardPrivate('hr')
  await a.mem.remember('hr', 'secret') // private — must not leak
  const srv = createHttpApi(a.mem).listen(0)
  await new Promise((r) => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`
  try {
    // discovery doc: public shards only — private + __* excluded
    const disco = await (await fetch(`${base}/.well-known/nektar`)).json()
    assert.equal(disco.kind, 'nektar-discovery')
    const names = (disco.publicShards as Array<{ shard: string }>).map((s) => s.shard)
    assert.ok(names.includes('guide'))
    assert.ok(!names.includes('hr') && !names.some((n) => n.startsWith('__')))

    // direct fetch == the tool's export (sans packUrl)
    const pack = await (await fetch(`${base}/v1/pack/guide`)).json()
    assert.equal(pack.kind, 'nektar-shard-pack')
    assert.equal(pack.records.length, 2)
    assert.equal((await fetch(`${base}/v1/pack/hr`)).status, 403)
    assert.equal((await fetch(`${base}/v1/pack/nope`)).status, 404)

    // remote import: peer pulls the pack over HTTP and re-verifies every bundle
    const b = await peerOf(a)
  if (b === undefined) return
    const imp = await callTool(b, 'importShard', { from: base, shard: 'guide' })
    assert.equal(imp.imported, 2)
    assert.equal(imp.anchored, 2)
    assert.deepEqual(imp.unverifiable, [])

    // exportShard hands out the shareable URL (the invite artifact)
    const ex = await callTool(a.mem, 'exportShard', { shard: 'guide' })
    assert.ok(ex.packUrl.endsWith('/v1/pack/guide'))

    // doctor counts offerable packs
    const d = await callTool(b, 'doctor', {})
    assert.equal(d.publicPacks, 1) // 'guide' only — 'hr' is private, __imports__ is system
  } finally {
    srv.close()
  }
})

test('bstore packs: describeShard publishes the pack; .well-known carries store; importShard {store} verifies; tamper refuses', async (t) => {
  const { createServer } = await import('node:http')
  const { S3ObjectStore } = await import('../src/objectstore.js')
  const { createHttpApi } = await import('../src/http.js')
  // accept-any-signature fake S3 — path-style PUT/GET/HEAD (the SigV4 check
  // lives in objectstore.test.ts)
  const objects = new Map<string, Uint8Array>()
  const metas = new Map<string, string>()
  const fake = createServer((req, res) => {
    const key = req.url!.split('/').slice(2).join('/')
    if (req.method === 'PUT') {
      const c: Buffer[] = []
      req.on('data', (x) => c.push(x))
      req.on('end', () => {
        objects.set(key, new Uint8Array(Buffer.concat(c)))
        metas.set(key, String(req.headers['x-amz-meta-sha256'] ?? ''))
        res.writeHead(200).end()
      })
      return
    }
    if (req.method === 'HEAD') {
      const o = objects.get(key)
      if (o === undefined) {
        res.writeHead(404).end()
        return
      }
      res.writeHead(200, { 'content-length': o.length, 'x-amz-meta-sha256': metas.get(key) ?? '' }).end()
      return
    }
    if (req.method === 'GET') {
      const o = objects.get(key)
      if (o === undefined) {
        res.writeHead(404).end()
        return
      }
      res.writeHead(200).end(Buffer.from(o))
      return
    }
    res.writeHead(405).end()
  })
  await new Promise<void>((r) => fake.listen(0, '127.0.0.1', r))
  const s3port = (fake.address() as AddressInfo).port
  const client = new S3ObjectStore({ endpoint: `http://127.0.0.1:${s3port}`, bucket: 'packs-regtest', accessKey: 'a', secretKey: 's' })
  const a = await setup(t, { objectStore: client })
  if (a === undefined) return
  const srv = createHttpApi(a.mem).listen(0)
  await new Promise((r) => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`
  try {
    await a.mem.remember('guide', 'fact one')
    await a.mem.remember('guide', 'fact two')
    a.settle('guide')
    // committing the manifest for a public shard publishes the pack
    await a.mem.describeShard('guide', { domains: ['libfoo'], offers: [{ kind: 'pack', priceSats: 50 }] })
    const ref = a.mem.packStoreRef('guide')
    assert.ok(ref !== undefined, 'pack stored after describeShard')
    assert.equal(ref!.bucket, 'packs-regtest')
    assert.match(ref!.key, /^packs\/regtest\/[0-9a-f]+\/[0-9a-f]{64}\.json$/)
    // the discovery doc names the store ref for the shard
    const disco = await (await fetch(`${base}/.well-known/nektar`)).json()
    const entry = (disco.publicShards as Array<{ shard: string; store?: { key: string }; packUrl?: string }>).find((s) => s.shard === 'guide')
    assert.equal(entry?.store?.key, ref!.key, '.well-known carries the store ref')
    assert.ok(entry?.packUrl, 'http fallback stays advertised')
    // a peer imports through the store ref — getVerified + pack re-verification
    const b = await peerOf(a, { objectStore: client })
  if (b === undefined) return
    const imp = (await callTool(b, 'importShard', { store: ref })) as { imported: number; anchored: number }
    assert.equal(imp.imported, 2)
    assert.equal(imp.anchored, 2)
    // a tampered object is refused by getVerified (size/digest) before it can land
    const c = await peerOf(a, { objectStore: client })
  if (c === undefined) return
    objects.set(ref!.key, new TextEncoder().encode('{"kind":"forged"}'))
    await assert.rejects(callTool(c, 'importShard', { store: ref }) as Promise<unknown>, /objectstore/)
    // and a pack that passes the digest but fails re-verification is refused too
    const d2 = await peerOf(a, { objectStore: client })
  if (d2 === undefined) return
    const pack = await a.mem.exportShardPack('guide')
    ;(pack.records[0] as { data: string }).data = 'swapped plaintext'
    const forged = await client.put(`packs/regtest/forged/${'ab'.repeat(32)}.json`, new TextEncoder().encode(JSON.stringify(pack)))
    const r2 = (await callTool(d2, 'importShard', { store: forged })) as { imported: number; unverifiable: string[] }
    assert.equal(r2.imported, 2, 'tampered records still land — as unverifiable, never trusted')
    assert.ok(r2.unverifiable.length > 0)
  } finally {
    fake.close()
    srv.close()
  }
})
