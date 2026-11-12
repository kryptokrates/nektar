import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { decodeFunding } from '@lightwebinc/bcommon'
import knex from 'knex'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { mintTree, FUNDING_TAG, KnexTreeLedger, MemoryTreeLedger } from '../src/tree.js'
import { fund, prove } from './helpers.js'

test('tree: n=8 mints 8 funding outputs + change, each decodes our tag', async () => {
  const key = PrivateKey.fromRandom()
  const t = await mintTree({ key, funding: fund(key), n: 8, satsPerOutput: 1000 })
  assert.equal(t.outputs.length, 9)
  for (let i = 0; i < 8; i++) assert.ok(decodeFunding(t.outputs[i].lockingScript, Array.from(FUNDING_TAG)))
})

test('ledger: unproven tree is not handed out; once proven it is', async () => {
  const key = PrivateKey.fromRandom()
  const t = await mintTree({ key, funding: fund(key), n: 2, satsPerOutput: 1000 })
  const l = new MemoryTreeLedger()
  await l.add(t, 2)
  assert.equal(await l.take(), undefined)
  prove(t)
  await l.markProven(t.id('hex'))
  assert.ok(await l.take())
})

test('ledger: two instances on one sqlite file never hand the same slot twice', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'bstack-'))
  const f = join(dir, 'l.db')
  const kx = (x: string) => knex({ client: 'sqlite3', connection: { filename: x }, useNullAsDefault: true })
  const k1 = kx(f)
  const key = PrivateKey.fromRandom()
  const t = await mintTree({ key, funding: fund(key), n: 4, satsPerOutput: 1000 })
  prove(t)
  const a = new KnexTreeLedger(k1)
  await a.add(t, 4)
  const k2 = kx(f)
  const b = new KnexTreeLedger(k2)
  const got = new Set<string>()
  for (let i = 0; i < 10; i++) {
    const s = i % 2 ? b : a
    const r = await s.take()
    if (r) {
      const k = `${r.tree.id('hex')}:${r.vout}`
      assert.ok(!got.has(k), `dup ${k}`)
      got.add(k)
    }
  }
  assert.equal(got.size, 4)
  await k1.destroy()
  await k2.destroy()
  rmSync(dir, { recursive: true, force: true })
})

// regression: a broadcaster BUMP can cover several txs of one block and marks
// a level-0 sibling txid:true when it is another of the same client's txs —
// hosts refuse such carriers ("the proof holds more than the funding tree's
// path"). take() must hand out the minimal per-tree path.
import { MerklePath, Hash, Utils } from '@bsv/sdk'
import { minimalPath } from '@lightwebinc/bbox-host/codec/beef.js'

// sdk hashPair(x,y) = sha256d(le(y)||le(x)) - parent of children (L,R) is h256(R,L)
const h256 = (a: string, b: string): string => Utils.toHex(Hash.hash256(Utils.toArray(a + b, 'hex').reverse()).reverse())

// one 8-leaf block: trees at offsets 2,3, a third flagged txid at offset 6.
// Shared node hashes so every single-tx path computes the same root.
const blockPaths = (h2: string, h3: string, h6: string): { compound: MerklePath; own: MerklePath; h3: string } => {
  const h7 = '77'.repeat(32),
    s01 = 'aa'.repeat(32),
    s45 = 'bb'.repeat(32)
  const s03 = h256(h256(h3, h2), s01) // node covering leaves 0..3
  const s47 = h256(h256(h7, h6), s45) // node covering leaves 4..7
  const p2 = new MerklePath(1, [
    [
      { offset: 2, hash: h2, txid: true },
      { offset: 3, hash: h3 }
    ],
    [{ offset: 0, hash: s01 }],
    [{ offset: 1, hash: s47 }]
  ])
  const p3 = new MerklePath(1, [
    [
      { offset: 3, hash: h3, txid: true },
      { offset: 2, hash: h2 }
    ],
    [{ offset: 0, hash: s01 }],
    [{ offset: 1, hash: s47 }]
  ])
  const p6 = new MerklePath(1, [
    [
      { offset: 6, hash: h6, txid: true },
      { offset: 7, hash: h7 }
    ],
    [{ offset: 2, hash: s45 }],
    [{ offset: 0, hash: s03 }]
  ])
  p2.combine(p3)
  p2.combine(p6) // same root for all three
  const compound = p2
  const own = new MerklePath(1, [
    [
      { offset: 2, hash: h2, txid: true },
      { offset: 3, hash: h3 }
    ],
    [{ offset: 0, hash: s01 }],
    [{ offset: 1, hash: s47 }]
  ])
  return { compound, own, h3 }
}

const assertOwnPath = (mp: MerklePath | undefined, txid: string, root: string, h3: string): void => {
  assert.ok(mp !== undefined)
  assert.equal(mp.computeRoot(txid), root)
  assert.equal(mp.path[0].length, 2)
  const me = mp.path[0].find((e) => e.hash === txid)
  assert.ok(me !== undefined && me.txid === true)
  const sib = mp.path[0].find((e) => e !== me)
  assert.ok(sib !== undefined && sib.txid !== true && sib.hash === h3)
  for (const [lvl, row] of mp.path.entries()) if (lvl > 0) assert.equal(row.length, 1, `level ${lvl}`)
  assert.equal(minimalPath(mp, txid), true)
}

for (const mk of [
  { name: 'memory', ledger: async () => ({ l: new MemoryTreeLedger() as MemoryTreeLedger | KnexTreeLedger, done: async () => {} }) },
  {
    name: 'knex',
    ledger: async () => {
      const kx = knex({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
      return { l: new KnexTreeLedger(kx), done: async () => await kx.destroy() }
    }
  }
] as const) {
  test(`ownPath (${mk.name}): compound broadcaster BUMP → minimal path for the tree alone`, async () => {
    const key = PrivateKey.fromRandom()
    const t2 = await mintTree({ key, funding: fund(key), n: 4, satsPerOutput: 1000 })
    const t3 = await mintTree({ key, funding: fund(key, 2_000_000), n: 4, satsPerOutput: 1000 })
    const txid = t2.id('hex')
    assert.notEqual(t3.id('hex'), txid, 'the sibling leaf must be a different tx')
    const { compound, h3 } = blockPaths(txid, t3.id('hex'), '55'.repeat(32))
    assert.equal(minimalPath(compound, txid), false, 'compound path is what hosts refused')
    const { l, done } = await mk.ledger()
    try {
      await l.add(t2, 4)
      await l.markProven(txid, compound)
      const got = await l.take()
      assert.ok(got !== undefined)
      assertOwnPath(got.tree.merklePath, txid, compound.computeRoot(txid), h3)
    } finally {
      await done()
    }
  })

  test(`ownPath (${mk.name}): an already-minimal path is preserved`, async () => {
    const key = PrivateKey.fromRandom()
    const t2 = await mintTree({ key, funding: fund(key), n: 4, satsPerOutput: 1000 })
    const txid = t2.id('hex')
    const { own, h3 } = blockPaths(txid, '44'.repeat(32), '55'.repeat(32))
    const { l, done } = await mk.ledger()
    try {
      await l.add(t2, 4)
      await l.markProven(txid, own)
      const got = await l.take()
      assert.ok(got !== undefined)
      assertOwnPath(got.tree.merklePath, txid, own.computeRoot(txid), h3)
    } finally {
      await done()
    }
  })
}
