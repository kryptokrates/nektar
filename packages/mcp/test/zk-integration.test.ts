// Wright §13 membership, verifier side: the committed REAL RISC Zero receipt
// (leaf 4 of a 5-leaf core/merkle.ts batch) is checked against real NektarAnchor
// transactions on the regtest chain — every check of verifyMembershipAnchoredStark.
// NEKTAR_STARK_PROVE=1 additionally proves end-to-end through NektarMemory (~50 s).
import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { PrivateKey } from '@bsv/sdk'
import { stackWorld, stackChain, stackTreasury, StackFunder } from '@nektar/chain'
import { AnchorChain } from '@nektar/anchor'
import { decodeNektarAnchor } from '@nektar/contracts'
import { fromHex, dsha256, utf8 } from '@nektar/core'
import { issueClearance, proveAccess } from '@nektar/identity'
import { encodeZkm2, verifyMembershipAnchoredStark, type QueryRegistry } from '@nektar/zk'
import { NektarMemory, type ZkClearance } from '../src/index.js'

const fx = JSON.parse(readFileSync(new URL('../../../zk/test/fixtures/zkm2-n5i4.json', import.meta.url), 'utf8'))
const proofBytes = () =>
  encodeZkm2({
    imageId: fromHex(fx.imageId),
    root: fromHex(fx.journal.root),
    size: fx.journal.size,
    queryId: fromHex(fx.journal.queryId),
    receipt: fromHex(fx.receipt)
  })
const issued = (...q: string[]): QueryRegistry => {
  const s = new Set(q)
  return { consume: (id) => s.delete(id) }
}

async function anchoredBatch(t: { skip: (m?: string) => void }, R: Uint8Array, size: number, mine = true) {
  const w = await stackWorld(t, 6)
  if (w === undefined) return
  const { chain, funder: f } = w
  const ac = new AnchorChain('stark', f.keyPair, f, f.keyPair, chain)
  await ac.anchor(size, R)
  const tx = ac.getHead()!.tx
  if (mine) {
    await chain.mine(1)
    ac.confirm(tx.id('hex'), chain.merklePathOf(tx.id('hex')))
  }
  return { chain, tx, ac, shard: decodeNektarAnchor(tx.outputs[0]!.lockingScript.toHex())!.shard }
}

test('STARK membership verifies against a real anchor whose R is the proven root', async (t) => {
  const _w = await anchoredBatch(t, fromHex(fx.journal.root), 5)
  if (_w === undefined) return
  const { chain, tx, shard } = _w
  const res = await verifyMembershipAnchoredStark(proofBytes(), tx, chain, issued(fx.journal.queryId), { shard })
  assert.ok(res.ok, JSON.stringify(res))
  assert.equal(res.anchorTxid, tx.id('hex'))
})

test('STARK membership NEGATIVES (each check fails closed with a specific reason)', async (t) => {
  const good = await anchoredBatch(t, fromHex(fx.journal.root), 5)
  if (good === undefined) return
  const q = fx.journal.queryId
  const v = (bytes: Uint8Array, a = good, reg = issued(q), exp: any = { shard: good.shard }) => verifyMembershipAnchoredStark(bytes, a.tx, a.chain, reg, exp)
  const cases: Array<[string, Promise<{ ok: boolean; reason?: string }>, RegExp]> = [
    [
      'anchor of another batch (R differs)',
      (async () => {
        const o = await anchoredBatch(t, dsha256(utf8('other batch')), 5)
  if (o === undefined) return { ok: false, reason: 'skipped' } as any
        return v(proofBytes(), o, issued(q), { shard: o.shard })
      })(),
      /not the anchored R/
    ],
    [
      'anchored size differs',
      (async () => {
        const o = await anchoredBatch(t, fromHex(fx.journal.root), 6)
  if (o === undefined) return { ok: false, reason: 'skipped' } as any
        return v(proofBytes(), o, issued(q), { shard: o.shard })
      })(),
      /size/
    ],
    ['anchor of another shard', v(proofBytes(), good, issued(q), { shard: '00'.repeat(20) }), /another shard/],
    ['anchor not canonical', v(proofBytes(), good, issued(q), { shard: good.shard, isCanonical: () => false }), /canonical/],
    [
      'anchor not mined',
      (async () => {
        const o = await anchoredBatch(t, fromHex(fx.journal.root), 5, false)
  if (o === undefined) return { ok: false, reason: 'skipped' } as any
        return v(proofBytes(), o, issued(q), { shard: o.shard })
      })(),
      /NotMined/
    ],
    [
      'anchor not SPV-valid on this header chain',
      verifyMembershipAnchoredStark(proofBytes(), good.tx, (await stackChain(t))!, issued(q), { shard: good.shard }),
      /BEEFInvalid|NotMined/
    ],
    ['queryId never issued', v(proofBytes(), good, issued()), /queryId/],
    [
      'tampered receipt',
      (() => {
        const b = proofBytes()
        b[b.length - 5000] ^= 1
        return v(b)
      })(),
      /stark/
    ],
    ['truncated bytes', v(proofBytes().slice(0, 500)), /codec/],
    [
      'foreign program id',
      (() => {
        const b = proofBytes()
        b[6] ^= 1
        return v(b)
      })(),
      /pinned/
    ]
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

test('NektarMemory: issued ZK queries are single use and survive a restart (persisted)', async (t) => {
  let saved: any
  const store = {
    load: () => saved,
    save: (s: any) => {
      saved = JSON.parse(JSON.stringify(s))
    }
  }
  const w = await stackWorld(t, 2)
  if (w === undefined) return
  const { chain, funder: f } = w
  const m1 = new NektarMemory(chain, f, f.keyPair, { securityStore: store })
  const q = m1.issueZkQuery('auditor')
  assert.match(q.queryId, /^[0-9a-f]{64}$/)
  assert.ok(
    saved.zkQueries.some(([id]: [string]) => id === q.queryId),
    'persisted'
  )
  const m2 = new NektarMemory(chain, f, f.keyPair, { securityStore: store })
  assert.equal((m2 as any).consumeZkQuery(q.queryId), true, 'issued query restored after restart')
  assert.equal((m2 as any).consumeZkQuery(q.queryId), false, 'single use')
})

test('end-to-end: NektarMemory proves (RISC Zero) and verifies its own anchored batch', { skip: process.env.NEKTAR_STARK_PROVE !== '1' }, async (t) => {
  const w = await stackWorld(t, 20)
  if (w === undefined) return
  const { chain, funder: f } = w
  const mem = new NektarMemory(chain, f, f.keyPair, { batchPolicy: { maxLeaves: 3 } })
  for (const d of ['a', 'b', 'c']) await mem.remember('s', d, {}, 0)
  await chain.mine(1)
  mem.confirmAnchor('s', mem.tailLink('s')!.txid)
  const q = mem.issueZkQuery('auditor')
  const { bytes } = await mem.proveMembership('s', 1, q.queryId)
  const ok = await mem.verifyMembership('s', 1, bytes)
  assert.ok(ok.ok, JSON.stringify(ok))
  const replay = await mem.verifyMembership('s', 1, bytes)
  assert.equal(replay.ok, false, 'replay rejected (query consumed)')
})

/** mem whose 's' shard has ONE anchored batch committing the fixture's R — the
 *  record is a stub (verifyCombined only reads seq/batchIndex); the anchor is
 *  real (NektarAnchor tx on the regtest chain, mined). */
async function memWithFixtureAnchor(t: { skip: (m?: string) => void }) {
  const _w = await anchoredBatch(t, fromHex(fx.journal.root), 5)
  if (_w === undefined) return
  const { chain, tx, ac } = _w
  const authority = PrivateKey.fromRandom()
  const f = await StackFunder.fund(await stackTreasury(), 2)
  const mem = new NektarMemory(chain, f, f.keyPair, { clearanceAuthorities: [authority.toPublicKey().toString()] })
  ;(mem as any).anchors.set('s', ac)
  ;(mem as any).anchorTxs.set(tx.id('hex'), tx)
  ;(mem as any).tails.set('s', [{ txid: tx.id('hex'), outputIndex: 0, n: 0, size: 5, R: fx.journal.root, Rp: '00'.repeat(32) }])
  ;(mem as any).store = { all: async () => [{ seq: 4, batchIndex: 0, leaf: new Uint8Array(32), leafIndex: 4 }] }
  const zk = (queryId: string, nonce: Uint8Array): ZkClearance => ({
    commitment: {} as any,
    proof: {} as any,
    requiredLevel: 0n,
    transcript: { queryId, fragmentId: 's/4', nonce }
  })
  return { mem, authority, zk }
}

test('§13 combined Π: the Π binding rejects proofs paired with an unrelated clearance', async (t) => {
  const _mw = await memWithFixtureAnchor(t)
  if (_mw === undefined) return
  const { mem, zk } = _mw
  const bytes = proofBytes() // bound to the fixture's journal queryId
  // 1. proof queryId was never issued here
  let r = await mem.verifyCombined('s', 4, bytes, zk(fx.journal.queryId, new Uint8Array(32)))
  assert.equal(r.ok, false)
  assert.match(String(r.reason), /not issued by this verifier/)
  // 2. an inconsistent registry row (nonce/verifierId do not recompute to the key)
  ;(mem as any).zkQueries.set(fx.journal.queryId, { nonce: 'ab'.repeat(32), verifierId: 'auditor', expiry: Date.now() + 60_000 })
  r = await mem.verifyCombined('s', 4, bytes, zk(fx.journal.queryId, fromHex('ab'.repeat(32))))
  assert.equal(r.ok, false)
  assert.match(String(r.reason), /recompute to the proof queryId/)
  // 3. a real issued query but a transcript bound to a DIFFERENT query → Π binding fails
  const q0 = mem.issueZkQuery('auditor')
  const b2 = proofBytes()
  b2.set(fromHex(q0.queryId), 74) // v2 header: queryId at offset 74 (header≠journal fails later at the STARK check)
  r = await mem.verifyCombined('s', 4, b2, zk('ff'.repeat(32), fromHex(q0.nonce)))
  assert.equal(r.ok, false)
  assert.match(String(r.reason), /Π binding/)
  // 4. transcript bound to the right queryId but a wrong nonce → Π binding fails
  r = await mem.verifyCombined('s', 4, b2, zk(q0.queryId, new Uint8Array(32).fill(9)))
  assert.equal(r.ok, false)
  assert.match(String(r.reason), /Π binding/)
  // 5. fully paired (queryId + nonce) → binding passes, falls through to the STARK check
  //    which rejects the header≠journal forgery — the pairing itself was accepted
  r = await mem.verifyCombined('s', 4, b2, zk(q0.queryId, fromHex(q0.nonce)))
  assert.equal(r.ok, false)
  assert.match(String(r.reason), /header does not match|stark/)
})

test('end-to-end §13 Π: membership STARK + ZKAccess clearance under ONE verifier query', { skip: process.env.NEKTAR_STARK_PROVE !== '1' }, async (t) => {
  const w = await stackWorld(t, 20)
  if (w === undefined) return
  const { chain, funder: f } = w
  const authority = PrivateKey.fromRandom()
  const mem = new NektarMemory(chain, f, f.keyPair, { batchPolicy: { maxLeaves: 3 }, clearanceAuthorities: [authority.toPublicKey().toString()] })
  for (const d of ['a', 'b', 'c']) await mem.remember('s', d, {}, 0)
  await chain.mine(1)
  mem.confirmAnchor('s', mem.tailLink('s')!.txid)
  const q = mem.issueZkQuery('auditor')
  const holder = PrivateKey.fromRandom().toPublicKey().toString()
  const cred = issueClearance(authority, 5n, holder)
  const transcript = { queryId: q.queryId, fragmentId: 's/1', nonce: fromHex(q.nonce) }
  const zk = { commitment: cred.commitment, proof: proveAccess(cred, 2n, 8, transcript)!, transcript, requiredLevel: 2n }
  const { bytes } = await mem.proveMembership('s', 1, q.queryId)
  const ok = await mem.verifyCombined('s', 1, bytes, zk)
  assert.ok(ok.ok, JSON.stringify(ok))
  // the same Π replays for nothing: the query was consumed
  const replay = await mem.verifyCombined('s', 1, bytes, zk)
  assert.equal(replay.ok, false)
  // a clearance bound to a different query can never pair with this STARK
  const q2 = mem.issueZkQuery('auditor')
  const t2 = { queryId: q2.queryId, fragmentId: 's/1', nonce: fromHex(q2.nonce) }
  const cred2 = issueClearance(authority, 5n, holder)
  const zk2 = { commitment: cred2.commitment, proof: proveAccess(cred2, 2n, 8, t2)!, transcript: t2, requiredLevel: 2n }
  const mm = await mem.verifyCombined('s', 1, bytes, zk2)
  assert.equal(mm.ok, false)
})
