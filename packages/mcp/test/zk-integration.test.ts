// Wright §13 membership, verifier side: the committed REAL RISC Zero receipt
// (leaf 4 of a 5-leaf core/merkle.ts batch) is checked against real MamemAnchor
// transactions on the mock chain — every check of verifyMembershipAnchoredStark.
// MAMEM_STARK_PROVE=1 additionally proves end-to-end through MamemMemory (~50 s).
import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { MockChain, MockFunder } from '@mamem/chain'
import { AnchorChain } from '@mamem/anchor'
import { decodeMamemAnchor } from '@mamem/contracts'
import { fromHex, dsha256, utf8 } from '@mamem/core'
import { encodeZkm2, verifyMembershipAnchoredStark, type QueryRegistry } from '@mamem/zk'
import { MamemMemory } from '../src/index.js'

const fx = JSON.parse(readFileSync(new URL('../../../zk/test/fixtures/zkm2-n5i4.json', import.meta.url), 'utf8'))
const proofBytes = () => encodeZkm2({ imageId: fromHex(fx.imageId), root: fromHex(fx.journal.root), size: fx.journal.size, queryId: fromHex(fx.journal.queryId), receipt: fromHex(fx.receipt) })
const issued = (...q: string[]): QueryRegistry => { const s = new Set(q); return { consume: id => s.delete(id) } }

async function anchoredBatch (R: Uint8Array, size: number, mine = true) {
  const chain = new MockChain(); chain.verifyScripts = true
  const f = new MockFunder(); f.topup(6); for (const u of (f as any).utxos) chain.registerParent(u.tx)
  const ac = new AnchorChain('stark', f.keyPair, f, f.keyPair, chain)
  await ac.anchor(size, R)
  const tx = ac.getHead()!.tx
  if (mine) { chain.mine(1); ac.confirm(tx.id('hex'), chain.merklePathOf(tx.id('hex'))) }
  return { chain, tx, shard: decodeMamemAnchor(tx.outputs[0]!.lockingScript.toHex())!.shard }
}

test('STARK membership verifies against a real anchor whose R is the proven root', async () => {
  const { chain, tx, shard } = await anchoredBatch(fromHex(fx.journal.root), 5)
  const res = await verifyMembershipAnchoredStark(proofBytes(), tx, chain, issued(fx.journal.queryId), { shard })
  assert.ok(res.ok, JSON.stringify(res))
  assert.equal(res.anchorTxid, tx.id('hex'))
})

test('STARK membership NEGATIVES (each check fails closed with a specific reason)', async () => {
  const good = await anchoredBatch(fromHex(fx.journal.root), 5)
  const q = fx.journal.queryId
  const v = (bytes: Uint8Array, a = good, reg = issued(q), exp: any = { shard: good.shard }) => verifyMembershipAnchoredStark(bytes, a.tx, a.chain, reg, exp)
  const cases: Array<[string, Promise<{ ok: boolean, reason?: string }>, RegExp]> = [
    ['anchor of another batch (R differs)', (async () => { const o = await anchoredBatch(dsha256(utf8('other batch')), 5); return v(proofBytes(), o, issued(q), { shard: o.shard }) })(), /not the anchored R/],
    ['anchored size differs', (async () => { const o = await anchoredBatch(fromHex(fx.journal.root), 6); return v(proofBytes(), o, issued(q), { shard: o.shard }) })(), /size/],
    ['anchor of another shard', v(proofBytes(), good, issued(q), { shard: '00'.repeat(20) }), /another shard/],
    ['anchor not canonical', v(proofBytes(), good, issued(q), { shard: good.shard, isCanonical: () => false }), /canonical/],
    ['anchor not mined', (async () => { const o = await anchoredBatch(fromHex(fx.journal.root), 5, false); return v(proofBytes(), o, issued(q), { shard: o.shard }) })(), /NotMined/],
    ['anchor not SPV-valid on this header chain', verifyMembershipAnchoredStark(proofBytes(), good.tx, new MockChain(), issued(q), { shard: good.shard }), /BEEFInvalid|NotMined/],
    ['queryId never issued', v(proofBytes(), good, issued()), /queryId/],
    ['tampered receipt', (() => { const b = proofBytes(); b[b.length - 5000] ^= 1; return v(b) })(), /stark/],
    ['truncated bytes', v(proofBytes().slice(0, 500)), /codec/],
    ['foreign program id', (() => { const b = proofBytes(); b[5] ^= 1; return v(b) })(), /pinned/]
  ]
  for (const [name, p, re] of cases) {
    const r = await p
    assert.equal(r.ok, false, name)
    assert.match(String(r.reason), re, `${name}: ${r.reason}`)
  }
  // single use: the same proof with the same issued query is accepted exactly once
  const reg = issued(q)
  assert.equal((await v(proofBytes(), good, reg)).ok, true)
  assert.match(String((await v(proofBytes(), good, reg)).reason), /already used|not issued/)
})

test('MamemMemory: issued ZK queries are single use and survive a restart (persisted)', async () => {
  let saved: any
  const store = { load: () => saved, save: (s: any) => { saved = JSON.parse(JSON.stringify(s)) } }
  const chain = new MockChain(); const f = new MockFunder(); f.topup(2)
  const m1 = new MamemMemory(chain, f, f.keyPair, { securityStore: store })
  const q = m1.issueZkQuery('auditor')
  assert.match(q.queryId, /^[0-9a-f]{64}$/)
  assert.ok(saved.zkQueries.some(([id]: [string]) => id === q.queryId), 'persisted')
  const m2 = new MamemMemory(chain, f, f.keyPair, { securityStore: store })
  assert.equal((m2 as any).consumeZkQuery(q.queryId), true, 'issued query restored after restart')
  assert.equal((m2 as any).consumeZkQuery(q.queryId), false, 'single use')
})

test('end-to-end: MamemMemory proves (RISC Zero) and verifies its own anchored batch', { skip: process.env.MAMEM_STARK_PROVE !== '1' }, async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const f = new MockFunder(); f.topup(20); for (const u of (f as any).utxos) chain.registerParent(u.tx)
  const mem = new MamemMemory(chain, f, f.keyPair, { batchPolicy: { maxLeaves: 3 } })
  for (const d of ['a', 'b', 'c']) await mem.remember('s', d, {}, 0)
  chain.mine(1); mem.confirmAnchor('s', mem.tailLink('s')!.txid)
  const q = mem.issueZkQuery('auditor')
  const { bytes } = await mem.proveMembership('s', 1, q.queryId)
  const ok = await mem.verifyMembership('s', 1, bytes)
  assert.ok(ok.ok, JSON.stringify(ok))
  const replay = await mem.verifyMembership('s', 1, bytes)
  assert.equal(replay.ok, false, 'replay rejected (query consumed)')
})
