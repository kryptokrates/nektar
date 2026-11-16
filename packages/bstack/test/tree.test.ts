import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, ProtoWallet, Transaction, UnlockingScript, P2PKH } from '@bsv/sdk'
import { decodeFunding, fundingLock } from '@lightwebinc/bcommon'
import knex from 'knex'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { mintTree, DERIVATION, FUNDING_TAG, ORIGINATOR, KnexTreeLedger, MemoryTreeLedger, anchoredTree as anchoredPred } from '../src/tree.js'
import * as treemod from '../src/tree.js'
import { boxFundingLock } from '../src/box.js'
import { anchoredTree, fakeParent, fund, prove } from './helpers.js'

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

test('ledger: unproven tree with anchored ancestry hands out zero-conf slots', async () => {
  const key = PrivateKey.fromRandom()
  const t = await anchoredTree(key, 2)
  assert.equal(t.merklePath, undefined)
  const l = new MemoryTreeLedger()
  await l.add(t, 2)
  assert.equal(await l.remaining(), 2)
  const s = await l.take()
  assert.ok(s !== undefined)
  assert.equal(s.tree.id('hex'), t.id('hex'))
  assert.equal(await l.remaining(), 1)
  // it still lists as unproven so markProven anchors it once mined
  assert.deepEqual(await l.unproven(), [{ txid: t.id('hex'), kind: 'market' }])
})

test('ledger: anchored ancestry serves only the market lane — bbox still waits for mined', async () => {
  const key = PrivateKey.fromRandom()
  const t = await anchoredTree(key, 2)
  const l = new MemoryTreeLedger()
  await l.add(t, 2, 'bbox')
  assert.equal(await l.remaining('bbox'), 0, 'unproven bbox tree must not serve')
  assert.equal(await l.take('bbox'), undefined)
  await l.markProven(t.id('hex'))
  assert.equal(await l.remaining('bbox'), 2)
})

// E1: a funding tx carrying its own proof anchors the fresh tree — the
// market lane serves it at once while the vendored-codec lanes still wait
test('ledger: a tree minted on a proven funding parent serves market, never bbox', async () => {
  const kx = knex({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  try {
    const key = PrivateKey.fromRandom()
    const t = await anchoredTree(key, 4)
    assert.equal(t.merklePath, undefined)
    assert.equal(anchoredPred(t), true, 'the funding parent proof must anchor the fresh tree')
    for (const l of [new MemoryTreeLedger(), new KnexTreeLedger(kx)]) {
      await l.add(t, 4, 'market')
      assert.ok((await l.take('market')) !== undefined, 'anchored tree must serve the market lane')
      assert.equal(await l.take('bbox'), undefined)
    }
  } finally {
    await kx.destroy()
  }
})

type Stock = { proven: number; anchored: number; unprovenTrees: number }
// stock() lands on the TreeLedger interface — the cast keeps this file
// compiling ahead of it
const stockOf = (l: MemoryTreeLedger | KnexTreeLedger, kind = 'market'): Promise<Stock | undefined> =>
  (l as unknown as { stock?: (kind: string) => Promise<Stock> }).stock?.(kind) ?? Promise.resolve(undefined)

test('ledger.stock: reports proven leaves, anchored leaves and waiting trees', async () => {
  const kx = knex({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  try {
    const key = PrivateKey.fromRandom()
    const mined = await mintTree({ key, funding: fund(key), n: 2, satsPerOutput: 1000 })
    prove(mined)
    const anchored = await anchoredTree(key, 3)
    const unanchored = await mintTree({ key, funding: fund(key), n: 5, satsPerOutput: 1000 })
    for (const l of [new MemoryTreeLedger(), new KnexTreeLedger(kx)]) {
      await l.add(mined, 2)
      await l.markProven(mined.id('hex'))
      await l.add(anchored, 3)
      await l.add(unanchored, 5)
      assert.deepEqual(await stockOf(l), { proven: 2, anchored: 3, unprovenTrees: 2 })
    }
  } finally {
    await kx.destroy()
  }
})

test('ledger (knex): unproven tree with anchored ancestry hands out zero-conf slots', async () => {
  const kx = knex({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  try {
    const key = PrivateKey.fromRandom()
    const t = await anchoredTree(key, 2)
    assert.equal(t.merklePath, undefined)
    const l = new KnexTreeLedger(kx)
    await l.add(t, 2)
    assert.equal(await l.remaining(), 2)
    const s = await l.take()
    assert.ok(s !== undefined)
    assert.equal(s.tree.id('hex'), t.id('hex'))
    assert.equal(await l.remaining(), 1)
    assert.deepEqual(await l.unproven(), [{ txid: t.id('hex'), kind: 'market' }])
    // a second anchored tree registered as bbox still waits for the BUMP
    const b = await anchoredTree(key, 1)
    await l.add(b, 1, 'bbox')
    assert.equal(await l.take('bbox'), undefined)
    assert.equal(await l.remaining('bbox'), 0)
  } finally {
    await kx.destroy()
  }
})

test('ledger: a stale tree minted under another identity is skipped, not handed out', async () => {
  const key = PrivateKey.fromRandom()
  const other = PrivateKey.fromRandom()
  const expectHex = (await fundingLock(new ProtoWallet(key), ORIGINATOR, DERIVATION, FUNDING_TAG)).toHex()
  const foreign = await fundingLock(new ProtoWallet(other), ORIGINATOR, DERIVATION, FUNDING_TAG)
  // a proven tree whose leaves lock to a different identity — the on-chain
  // slots are real but no carrier built from them can ever evaluate
  const stale = await mintTree({ key, funding: fund(key), n: 4, satsPerOutput: 1000, lock: foreign })
  prove(stale)
  const good = await mintTree({ key, funding: fund(key), n: 2, satsPerOutput: 1000 })
  prove(good)
  for (const l of [new MemoryTreeLedger(), new KnexTreeLedger(knex({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true }))]) {
    await l.add(stale, 4)
    await l.add(good, 2)
    const slot = await l.take('market', expectHex)
    assert.ok(slot !== undefined, 'the correctly-keyed tree still serves')
    assert.equal(slot.tree.id('hex'), good.id('hex'))
    assert.equal((await l.take('market', expectHex)) !== undefined, true, 'second good slot')
    assert.equal(await l.take('market', expectHex), undefined, 'stale slots consumed, not served')
    if (l instanceof KnexTreeLedger) await l['kx'].destroy()
  }
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
  test(`attachParentPath (${mk.name}): a funding parent's proof anchors the tree's ancestry`, async () => {
    const key = PrivateKey.fromRandom()
    const parent = fakeParent(key)
    const tree = await mintTree({ key, funding: { tx: parent, vout: 0, satoshis: 1_000_000 }, n: 4, satsPerOutput: 1000 })
    const { l, done } = await mk.ledger()
    try {
      await l.add(tree, 4)
      assert.equal(await l.take('market'), undefined, 'unanchored ancestry must not serve')
      const mp = new MerklePath(1, [
        [
          { offset: 0, hash: '00'.repeat(32) },
          { offset: 1, hash: parent.id('hex'), txid: true }
        ]
      ])
      assert.equal(await l.attachParentPath(parent.id('hex'), mp), true)
      const got = await l.take('market')
      assert.ok(got !== undefined, 'parent proof makes the unproven tree serveable')
      assert.equal(got.tree.inputs[0].sourceTransaction?.merklePath?.computeRoot(parent.id('hex')), mp.computeRoot(parent.id('hex')))
      assert.equal(await l.take('bbox'), undefined, 'bbox still requires the tree proven')
      assert.equal(await l.attachParentPath(parent.id('hex'), mp), false, 'idempotent second attach')
    } finally {
      await done()
    }
  })

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

  // regression: an odd-count block duplicates the last leaf — the tree's own
  // txid legitimately appears at both the real offset and the offset after.
  // MerklePath.extract() resolves the txid by hash and re-flags that copy, so
  // take() used to emit carriers hosts refuse ("the proof holds more than the
  // funding tree's path"). The flag must stay on the source path's offset.
  test(`ownPath (${mk.name}): self-paired last leaf keeps a single txid flag`, async () => {
    const key = PrivateKey.fromRandom()
    const t2 = await mintTree({ key, funding: fund(key), n: 4, satsPerOutput: 1000 })
    const txid = t2.id('hex')
    // 3-leaf block: tree at offset 2, duplicated to offset 3 by tree pairing.
    // n0 covers leaves 0..1, n1 = h(t2,t2); root = h256(n1, n0).
    const t0 = '44'.repeat(32),
      t1 = '55'.repeat(32)
    const n0 = h256(t1, t0)
    const root = h256(h256(txid, txid), n0)
    const stored = new MerklePath(1, [
      [
        { offset: 0, hash: t0, txid: true },
        { offset: 1, hash: t1 },
        { offset: 2, hash: txid, txid: true },
        { offset: 3, hash: txid }
      ],
      [
        { offset: 0, hash: n0 },
        { offset: 1, hash: h256(txid, txid) }
      ]
    ])
    const { l, done } = await mk.ledger()
    try {
      await l.add(t2, 4)
      await l.markProven(txid, stored)
      const got = await l.take()
      assert.ok(got !== undefined)
      const mp = got.tree.merklePath
      assert.ok(mp !== undefined)
      assert.equal(mp.computeRoot(txid), root)
      const flagged = mp.path[0].filter((e) => e.txid === true)
      assert.equal(flagged.length, 1, 'extract re-flags the duplicate leaf — hosts refuse it')
      assert.equal(flagged[0].offset, 2)
      assert.equal(minimalPath(mp, txid), true)
    } finally {
      await done()
    }
  })
}

// spv/unstocked-lane: a funding tx that already carries lane-locked outputs
// provisions stock for the block that confirms the money — adopted slots
// behave like tree slots but live at their real output index
test('ledger.adopt: lane-locked outputs of a provision tx become slots', async () => {
  const kx = knex({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  try {
    const key = PrivateKey.fromRandom()
    const marketLock = await fundingLock(new ProtoWallet(key), ORIGINATOR, DERIVATION, FUNDING_TAG)
    const bboxLock = await boxFundingLock(key, new ProtoWallet(key))
    const tx = new Transaction()
    tx.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromASM('OP_1') })
    tx.addOutput({ satoshis: 100, lockingScript: marketLock })
    tx.addOutput({ satoshis: 100, lockingScript: marketLock })
    tx.addOutput({ satoshis: 100, lockingScript: bboxLock })
    tx.addOutput({ satoshis: 100, lockingScript: bboxLock })
    tx.addOutput({ satoshis: 5000, lockingScript: new P2PKH().lock(key.toPublicKey().toAddress()) })
    const lanes = [
      { kind: 'market', lockHex: marketLock.toHex(), minSats: 1 },
      { kind: 'bbox', lockHex: bboxLock.toHex(), minSats: 100 }
    ]
    const mod = treemod as unknown as {
      matchProvision?: (t: Transaction, l: typeof lanes) => { vout: number; kind: string }[]
    }
    assert.ok(mod.matchProvision !== undefined, 'tree.ts must export matchProvision')
    const matched = mod.matchProvision(tx, lanes)
    assert.deepEqual(
      matched.map((m) => `${m.kind}:${m.vout}`).sort(),
      ['bbox:2', 'bbox:3', 'market:0', 'market:1'],
      'the four lane-locked outputs at their real vouts'
    )
    for (const l of [new MemoryTreeLedger(), new KnexTreeLedger(kx)]) {
      // each ledger must see the tx unproven — prove() below mutates it
      tx.merklePath = undefined
      const ledger = l as unknown as { adopt?: (t: Transaction, s: typeof matched) => Promise<number> }
      assert.ok(ledger.adopt !== undefined, 'ledger must implement adopt')
      assert.equal(await ledger.adopt(tx, matched), 4)
      assert.equal(await ledger.adopt(tx, matched), 0, 're-adopt is idempotent')
      assert.deepEqual(await stockOf(l, 'bbox'), { proven: 0, anchored: 0, unprovenTrees: 1 }, 'provision tree counts toward bbox stock')
      assert.equal(await l.take('bbox', bboxLock.toHex()), undefined, 'unproven provision stock must not serve bbox')
      assert.equal(await l.take('market'), undefined, 'unanchored provision stock must not serve market zero-conf either')
      prove(tx)
      await l.markProven(tx.id('hex'), tx.merklePath)
      const slot = await l.take('bbox', bboxLock.toHex())
      assert.ok(slot !== undefined, 'proven provision tree must serve a bbox slot')
      assert.ok([2, 3].includes(slot.vout), `real output index, got ${slot.vout}`)
      assert.ok(slot.tree.merklePath !== undefined, 'served tree carries the proof')
      const m = await l.take('market')
      assert.ok(m !== undefined && [0, 1].includes(m.vout), `market slot from provision tree, got ${m?.vout}`)
    }
  } finally {
    await kx.destroy()
  }
})

// G3: a rejected provision tx is reaped — drop removes its slot rows of
// every kind and the same tx can be adopted again afterwards
test('ledger.adopt: drop frees provision slots of every kind and re-adopt registers again', async () => {
  const kx = knex({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  try {
    const key = PrivateKey.fromRandom()
    const marketLock = await fundingLock(new ProtoWallet(key), ORIGINATOR, DERIVATION, FUNDING_TAG)
    const bboxLock = await boxFundingLock(key, new ProtoWallet(key))
    const tx = new Transaction()
    tx.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromASM('OP_1') })
    tx.addOutput({ satoshis: 100, lockingScript: marketLock })
    tx.addOutput({ satoshis: 100, lockingScript: bboxLock })
    const lanes = [
      { kind: 'market', lockHex: marketLock.toHex(), minSats: 1 },
      { kind: 'bbox', lockHex: bboxLock.toHex(), minSats: 100 }
    ]
    const mod = treemod as unknown as { matchProvision?: (t: Transaction, l: typeof lanes) => { vout: number; kind: string }[] }
    const matched = mod.matchProvision!(tx, lanes)
    for (const l of [new MemoryTreeLedger(), new KnexTreeLedger(kx)]) {
      const ledger = l as unknown as { adopt?: (t: Transaction, s: typeof matched) => Promise<number> }
      await ledger.adopt!(tx, matched)
      assert.deepEqual(await stockOf(l, 'bbox'), { proven: 0, anchored: 0, unprovenTrees: 1 })
      await l.drop(tx.id('hex'))
      assert.deepEqual(await stockOf(l, 'bbox'), { proven: 0, anchored: 0, unprovenTrees: 0 }, 'drop removes bbox slot rows')
      assert.deepEqual(await stockOf(l, 'market'), { proven: 0, anchored: 0, unprovenTrees: 0 }, 'drop removes market slot rows')
      assert.equal(await ledger.adopt!(tx, matched), 2, 're-adopt after a drop registers the same tx again')
      await l.drop(tx.id('hex'))
    }
  } finally {
    await kx.destroy()
  }
})
