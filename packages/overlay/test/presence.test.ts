import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, P2PKH, LockingScript } from '@bsv/sdk'
import { MockChain, MockFunder } from '@nektar/chain'
import { createNektarOverlay } from '../src/node.js'
import { signPresence, presenceOutputScript, parsePresence, verifyPresence, signCatalog, verifyCatalog, signWant, catalogOutputScript, wantOutputScript } from '../src/presence.js'

const ad = async (key: PrivateKey, fields: { endpoint: string, shards: string[], ts: number }) =>
  signPresence(key, fields)

async function mkAdTx (funder: MockFunder, scriptHex: string): Promise<Transaction> {
  const tx = new Transaction()
  const fund = await funder.take(1500)
  tx.addInput({ sourceTransaction: fund.tx, sourceOutputIndex: fund.vout, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
  tx.addOutput({ lockingScript: LockingScript.fromHex(scriptHex), satoshis: 0 })
  tx.addOutput({ lockingScript: new P2PKH().lock(funder.keyPair.toPublicKey().toAddress([0x6f])), change: true })
  await tx.fee(); await tx.sign()
  return tx
}

async function setup () {
  const chain = new MockChain()
  const funder = new MockFunder()
  funder.topup(50)
  const utxos = (funder as any).utxos as Array<{ tx: Transaction }>
  for (const u of utxos) chain.mempool.set(u.tx.id('hex'), u.tx)
  chain.mine(1)
  const overlay = await createNektarOverlay(chain, chain)
  const submitAd = async (tx: Transaction) =>
    overlay.engine.submit({ beef: tx.toBEEF(), topics: ['tm_presence'] }, undefined, 'current-tx')
  const look = (query: any) => overlay.engine.lookup({ service: 'ls_presence', query })
  const decode = (o: any) => JSON.parse(Buffer.from(o.context ?? []).toString())
  return { chain, funder, overlay, submitAd, look, decode }
}

test('tm_presence: signed ad admitted; directory answers all/shard/pub', async (t) => {
  const { funder, overlay, submitAd, look, decode } = await setup()
  t.after(async () => { await overlay.knex.destroy() })
  const agentA = PrivateKey.fromRandom()
  const sA = await ad(agentA, { endpoint: 'http://a.example:8080/mcp', shards: ['manual', 'tnet'], ts: 1_700_000_000_000 })
  assert.ok(verifyPresence(sA.ad, sA.sig), 'self-signed ad verifies')
  const txA = await mkAdTx(funder, presenceOutputScript(sA))
  const st = await submitAd(txA)
  assert.equal(st.tm_presence?.outputsToAdmit?.length, 1, 'ad admitted')

  const agentB = PrivateKey.fromRandom()
  const sB = await ad(agentB, { endpoint: 'http://b.example:8080/mcp', shards: ['telemetry'], ts: 1_700_000_001_000 })
  await submitAd(await mkAdTx(funder, presenceOutputScript(sB)))

  const dir = await look({ all: true })
  assert.equal(dir.outputs.length, 2)
  const who = await look({ shard: 'manual' })
  assert.equal(who.outputs.length, 1)
  assert.equal(decode(who.outputs[0]).endpoint, 'http://a.example:8080/mcp')
  assert.equal(decode(who.outputs[0]).pub, agentA.toPublicKey().toString())
  const one = await look({ pub: agentB.toPublicKey().toString() })
  assert.equal(decode(one.outputs[0]).endpoint, 'http://b.example:8080/mcp')

  // newer ad replaces the older one for the same pubkey
  const sA2 = await ad(agentA, { endpoint: 'http://a2.example/mcp', shards: ['telemetry'], ts: 1_700_000_002_000 })
  await submitAd(await mkAdTx(funder, presenceOutputScript(sA2)))
  const again = await look({ pub: agentA.toPublicKey().toString() })
  assert.equal(decode(again.outputs[0]).endpoint, 'http://a2.example/mcp')
  assert.equal((await look({ shard: 'manual' })).outputs.length, 0, 'shard index updated on replace')
  assert.equal((await look({ all: true })).outputs.length, 2)
})

test('tm_presence: forged/unsigned/malformed ads rejected', async (t) => {
  const { funder, overlay, submitAd, look } = await setup()
  t.after(async () => { await overlay.knex.destroy() })
  const key = PrivateKey.fromRandom()
  const forged = await ad(key, { endpoint: 'http://victim.example/mcp', shards: ['s'], ts: 1 })
  forged.sig = '30' + '44'.repeat(35) // mangled signature
  const st = await submitAd(await mkAdTx(funder, presenceOutputScript(forged)))
  assert.equal(st.tm_presence?.outputsToAdmit?.length ?? 0, 0, 'bad sig not admitted')
  const junk = new Transaction()
  const fund = await funder.take(1500)
  junk.addInput({ sourceTransaction: fund.tx, sourceOutputIndex: fund.vout, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
  junk.addOutput({ lockingScript: LockingScript.fromHex('6a03787878'), satoshis: 0 }) // OP_RETURN 'xxx'
  junk.addOutput({ lockingScript: new P2PKH().lock(funder.keyPair.toPublicKey().toAddress([0x6f])), change: true })
  await junk.fee(); await junk.sign()
  const st2 = await submitAd(junk)
  assert.equal(st2.tm_presence?.outputsToAdmit?.length ?? 0, 0, 'non-presence OP_RETURN not admitted')
  assert.equal((await look({ all: true })).outputs.length, 0)
})

test('presence script round-trip: parse(build(ad)) === ad', () => {
  const key = PrivateKey.fromRandom()
  const s = signPresence(key, { endpoint: 'http://x/mcp', shards: ['a', 'b'], ts: 42 })
  const back = parsePresence(presenceOutputScript(s))!
  assert.deepEqual(back.ad, s.ad)
  assert.ok(verifyPresence(back.ad, back.sig))
})

test('catalog: signed manifest announced; directory answers domain/q', async (t) => {
  const { overlay } = await setup()
  t.after(async () => { await overlay.knex.destroy() })
  const ls = overlay.lsPresence as any
  const agentA = PrivateKey.fromRandom()
  const adA = signPresence(agentA, { endpoint: 'http://a.example:8080/mcp', shards: ['ball'], ts: 1_700_000_000_000 })
  assert.equal(await ls.announce(adA), true)
  const cat = signCatalog(agentA, {
    endpoint: 'http://a.example:8080/mcp',
    shards: [
      { shard: 'ball', shardId: 'a'.repeat(40), domains: ['soccer', 'transfer-rumours'], desc: 'match results and transfer news', offers: [{ kind: 'answer', priceSats: 250 }] },
      { shard: 'misc', domains: ['weather'] }
    ],
    ts: 1_700_000_001_000
  })
  assert.equal(await ls.announceCatalog(cat), true)

  // domain tag finds the claimant's signed catalog
  const dom = await ls.lookup({ query: { domain: 'soccer' } })
  assert.equal(dom.length, 1)
  const got = JSON.parse(Buffer.from(dom[0].context).toString())
  assert.equal(got.catalog.pub, agentA.toPublicKey().toString())
  assert.equal(got.catalog.shards[0].shard, 'ball')
  assert.equal((await ls.lookup({ query: { domain: 'SOCCER' } })).length, 1, 'domain match is case-insensitive')
  assert.equal((await ls.lookup({ query: { domain: 'cricket' } })).length, 0)

  // free-text hits desc + domains
  assert.equal((await ls.lookup({ query: { q: 'transfer' } })).length, 1)
  assert.equal((await ls.lookup({ query: { q: 'weather' } })).length, 1)
  assert.equal((await ls.lookup({ query: { q: 'rugby' } })).length, 0)

  // catalogFor joins catalogs to ad queries
  assert.equal(ls.catalogFor(agentA.toPublicKey().toString()).catalog.shards.length, 2)

  // newer catalog replaces; domain index follows
  const cat2 = signCatalog(agentA, { shards: [{ shard: 'ball', domains: ['cricket'] }], ts: 1_700_000_002_000 })
  assert.equal(await ls.announceCatalog(cat2), true)
  assert.equal((await ls.lookup({ query: { domain: 'soccer' } })).length, 0, 'stale domain dropped')
  assert.equal((await ls.lookup({ query: { domain: 'cricket' } })).length, 1)
  // stale ts is ignored
  assert.equal(await ls.announceCatalog(cat), true)
  assert.equal((await ls.lookup({ query: { domain: 'cricket' } })).length, 1, 'latest-wins kept')
})

test('catalog: forged/unsigned/malformed catalogs rejected', async (t) => {
  const { overlay } = await setup()
  t.after(async () => { await overlay.knex.destroy() })
  const ls = overlay.lsPresence as any
  const key = PrivateKey.fromRandom()
  const good = signCatalog(key, { shards: [{ shard: 's', domains: ['soccer'] }], ts: 5 })
  good.sig = '30' + '44'.repeat(35)
  assert.equal(await ls.announceCatalog(good), false)
  const bad = signCatalog(key, { shards: [{ shard: 's', domains: ['soccer'] }], ts: 6 })
  bad.catalog.shards[0]!.domains = ['rugby'] // post-sign mutation
  assert.equal(await ls.announceCatalog(bad), false)
  assert.equal(verifyCatalog({ pub: 'zz', shards: [], ts: 1 } as any, 'aa'), false)
})

test('events: feed replays ads/catalogs/wants by cursor + domain filter', async (t) => {
  const { overlay } = await setup()
  t.after(async () => { await overlay.knex.destroy() })
  const ls = overlay.lsPresence as any
  const a = PrivateKey.fromRandom(), b = PrivateKey.fromRandom()
  await ls.announce(signPresence(a, { endpoint: 'http://a/mcp', shards: ['ball'], ts: 1 }))
  const cat = signCatalog(a, { shards: [{ shard: 'ball', domains: ['soccer'] }], ts: 2 })
  await ls.announceCatalog(cat)
  const want = signWant(b, { domains: ['soccer'], question: 'need results', maxSats: 500, ts: 3 })
  assert.equal(await ls.announceWant(want), true)

  const all = await ls.feed({})
  assert.equal(all.length, 3)
  assert.deepEqual(all.map((e: any) => e.kind), ['ad', 'catalog', 'want'])
  const cur = await ls.feed({ since: 0 })
  assert.equal(cur.length, 2, 'cursor skips already-seen events')
  const soccer = await ls.feed({ domains: ['soccer'] })
  assert.equal(soccer.length, 2, 'domain filter hits catalog + want, not the bare ad')
  const wants = await ls.feed({ kind: 'want' })
  assert.equal(wants.length, 1)
  assert.equal(ls.wantFor(b.toPublicKey().toString()).want.maxSats, 500)

  // stale want doesn't append
  const old = signWant(b, { domains: ['soccer'], ts: 2 })
  assert.equal(await ls.announceWant(old), true) // accepted no-op
  assert.equal((await ls.feed({ kind: 'want' })).length, 1)
  // invalid sig rejected
  const bad = signWant(b, { domains: ['soccer'], ts: 9 }); bad.sig = '30' + '44'.repeat(35)
  assert.equal(await ls.announceWant(bad), false)
})

test('multi-kind outputs: catalog + want admitted on-chain via tm_presence', async (t) => {
  const { funder, overlay, submitAd, look } = await setup()
  t.after(async () => { await overlay.knex.destroy() })
  const key = PrivateKey.fromRandom()
  const sc = signCatalog(key, { shards: [{ shard: 'ball', shardId: 'a'.repeat(40), domains: ['soccer'] }], ts: 11 })
  const st = await submitAd(await mkAdTx(funder, catalogOutputScript(sc)))
  assert.equal(st.tm_presence?.outputsToAdmit?.length, 1, 'catalog output admitted')
  const sw = signWant(key, { domains: ['soccer'], maxSats: 250, ts: 12 })
  const st2 = await submitAd(await mkAdTx(funder, wantOutputScript(sw)))
  assert.equal(st2.tm_presence?.outputsToAdmit?.length, 1, 'want output admitted')

  // on-chain admission indexed the catalog (domain lookup works) + appended events
  const dom = await (overlay.lsPresence as any).lookup({ query: { domain: 'soccer' } })
  assert.equal(dom.length, 1)
  const feed = await (overlay.lsPresence as any).feed({})
  assert.equal(feed.length, 2)
  assert.equal(feed[1].kind, 'want')

  // forged catalog output rejected
  const forged = signCatalog(key, { shards: [{ shard: 'x', domains: ['rugby'] }], ts: 13 })
  forged.sig = '30' + '44'.repeat(35)
  const st3 = await submitAd(await mkAdTx(funder, catalogOutputScript(forged)))
  assert.equal(st3.tm_presence?.outputsToAdmit?.length ?? 0, 0)
})
