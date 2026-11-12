// L_ZKDis v2 (DIS-002 proof sharding): canonical codec + subset multiproof
// verification over core/merkle.ts trees. Sizes exercise: one shard (≤64),
// the shard boundary (65+), partial last shards and multi-level top paths.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { dsha256, utf8, merkleRoot, buildTree } from '@nektar/core'
import { buildSubsetProof, encodeZkdis, decodeZkdis, verifyZkdis, verifySubsetProof, ZksCodecError } from '../src/stark/subproof.js'

const leaves = (n: number) => Array.from({ length: n }, (_, i) => dsha256(utf8(`leaf-${i}`)))
const proofOf = (n: number, idx: number[]) => buildSubsetProof(leaves(n), idx)

test('ZKS2 codec: round-trip is exact for every shape', () => {
  for (const [n, idx] of [
    [1, [0]],
    [5, [1]],
    [40, [3, 39]],
    [64, [0, 63]],
    [65, [0, 64]],
    [70, [1, 63, 64, 69]],
    [128, [0, 127]],
    [200, [0, 63, 64, 127, 128, 199]]
  ] as Array<[number, number[]]>) {
    const p = proofOf(n, idx)
    const back = decodeZkdis(encodeZkdis(p))
    assert.deepEqual(back, p, `size ${n}`)
    assert.deepEqual(back.batchRoot, merkleRoot(leaves(n)), `size ${n}: commits the real batch root`)
  }
})

test('subset proof verifies: single leaf, runs, across the shard boundary, all leaves', () => {
  for (const [n, idx] of [
    [5, [1]],
    [5, [1, 2, 3]],
    [70, [63, 64]],
    [70, [1, 63, 64, 69]],
    [200, [64, 128, 199]],
    [70, Array.from({ length: 70 }, (_, i) => i)],
    [1, [0]],
    [64, [0, 31, 63]],
    [65, [64]]
  ] as Array<[number, number[]]>) {
    const v = verifyZkdis(encodeZkdis(proofOf(n, idx)))
    assert.ok(v.ok, `size ${n} subset ${JSON.stringify(idx.slice(0, 5))}…: ${JSON.stringify(v)}`)
    assert.deepEqual(
      v.indices,
      [...idx].sort((a, b) => a - b)
    )
    assert.deepEqual(v.root, merkleRoot(leaves(n)))
  }
})

test('ZKS2 codec (GEN-007): rev-2 carries the algorithm id; rev-1 still decodes', () => {
  const ok = encodeZkdis(proofOf(70, [1, 63, 64]))
  assert.equal(ok[4], 2)
  assert.equal(ok[5], 0x01)
  const v1 = Uint8Array.from([...ok.slice(0, 4), 1, ...ok.slice(6)])
  const d = decodeZkdis(v1)
  assert.equal(d.algorithm, 0x01)
  assert.equal(d.size, 70)
})

test('structure: shard split at depth 6; the shared top path is deduplicated', () => {
  // 70 leaves → two shards; proving {63,64} covers both, so the level-6
  // siblings are the other shard's root (already known): topPath is EMPTY.
  const both = proofOf(70, [63, 64])
  assert.equal(both.shards.length, 2)
  assert.equal(both.shards[0].leaves[0].subpath.length, 6)
  assert.equal(both.topPath.length, 0)
  // proving only shard 0 needs shard 1's root as the single top sibling
  const one = proofOf(70, [63])
  assert.equal(one.topPath.length, 1)
  const tree = buildTree(leaves(70))
  assert.deepEqual(one.topPath[0], tree.levels[6][1], 'top sibling IS the other shard root')
  // 200 leaves → height 8: subset {0,128} spans shards 0,2 — level-6 sibs 1,3
  // needed, then the level-7 ancestors 0,1 pair internally → 2 stored hashes
  const p200 = proofOf(200, [0, 128])
  assert.equal(p200.topPath.length, 2)
  // a ≤64-leaf batch is ONE shard: subpath = the whole standard path
  const small = proofOf(40, [3])
  assert.equal(small.shards[0].leaves[0].subpath.length, 6) // height(40)=6
  const s1 = proofOf(1, [0])
  assert.equal(s1.shards[0].leaves[0].subpath.length, 0)
  assert.equal(s1.topPath.length, 0)
})

test('ZKS2 codec: every malformed input is refused with ZksCodecError', () => {
  const ok = encodeZkdis(proofOf(70, [1, 63, 64]))
  const mut = (f: (b: Uint8Array) => Uint8Array) => f(ok.slice())
  const ENTRY = 4 + 32 + 1 + 6 * 32 // leafIndex ‖ leafHash ‖ subpathLen ‖ 6×32
  const bad: Array<[string, Uint8Array]> = [
    ['empty', new Uint8Array()],
    ['truncated header', ok.slice(0, 30)],
    [
      'bad magic',
      mut((b) => {
        b[0] = 0
        return b
      })
    ],
    [
      'zkm2 magic',
      mut((b) => {
        b[3] = 0x4d
        return b
      })
    ],
    [
      'unknown version',
      mut((b) => {
        b[4] = 3
        return b
      })
    ],
    [
      'unknown algorithm',
      mut((b) => {
        b[5] = 0x7f
        return b
      })
    ],
    [
      'size 0',
      mut((b) => {
        b.fill(0, 38, 42)
        return b
      })
    ],
    [
      'numShards 0',
      mut((b) => {
        b.fill(0, 42, 46)
        return b
      })
    ],
    [
      'shardIndex out of range',
      mut((b) => {
        new DataView(b.buffer).setUint32(46, 9, true)
        return b
      })
    ],
    [
      'leafIndex in wrong shard',
      mut((b) => {
        new DataView(b.buffer).setUint32(52, 65, true)
        return b
      })
    ],
    [
      'subpathLen wrong',
      mut((b) => {
        b[88] = 5
        return b
      })
    ],
    [
      'non-canonical leaf order',
      mut((b) => {
        const e0 = b.slice(52, 52 + ENTRY),
          e1 = b.slice(52 + ENTRY, 52 + 2 * ENTRY)
        b.set(e1, 52)
        b.set(e0, 52 + ENTRY)
        return b
      })
    ],
    [
      'topPathLen +1',
      mut((b) => {
        new DataView(b.buffer).setUint32(b.length - 4, 1, true)
        return b
      })
    ],
    ['truncated proof', ok.slice(0, ok.length - 1)],
    ['trailing bytes', Uint8Array.from([...ok, 0])]
  ]
  for (const [name, b] of bad) assert.throws(() => decodeZkdis(b), ZksCodecError, name)
  assert.throws(() => encodeZkdis({ ...proofOf(5, [1]), batchRoot: new Uint8Array(31) }), ZksCodecError)
  assert.throws(() => buildSubsetProof(leaves(5), []), ZksCodecError)
  assert.throws(() => buildSubsetProof(leaves(5), [1, 1]), ZksCodecError)
  assert.throws(() => buildSubsetProof(leaves(5), [5]), ZksCodecError)
  // encoding non-canonical order is refused too
  const dis = proofOf(5, [1, 2])
  dis.shards[0].leaves.reverse()
  assert.throws(() => encodeZkdis(dis), ZksCodecError)
})

test('subset proof NEGATIVES (fails closed with a specific reason)', () => {
  const L = leaves(70)
  const ok = () => proofOf(70, [1, 63, 64, 69])
  const cases: Array<[string, () => { ok: boolean; reason?: string }]> = [
    [
      'tampered subpath sibling',
      () => {
        const p = ok()
        p.shards[0].leaves[0].subpath[2] = dsha256(utf8('forged'))
        return verifySubsetProof(p)
      }
    ],
    [
      'leaves disagree on the shard root',
      () => {
        const p = proofOf(70, [61, 62, 63])
        p.shards[0].leaves[1].subpath[5] = dsha256(utf8('x'))
        return verifySubsetProof(p)
      }
    ],
    [
      'wrong claimed batch root',
      () => {
        const p = ok()
        p.batchRoot = dsha256(utf8('other'))
        return verifySubsetProof(p)
      }
    ],
    ['missing sibling (truncated)', () => verifyZkdis(encodeZkdis(ok()).slice(0, 60))],
    [
      'mutated wire leaf hash',
      () => {
        const b = encodeZkdis(ok())
        b[60] ^= 1
        return verifyZkdis(b)
      }
    ],
    [
      'mutated wire top path',
      () => {
        const p = proofOf(70, [63])
        const b = encodeZkdis(p)
        b[b.length - 16] ^= 1
        return verifyZkdis(b)
      }
    ],
    [
      'disclosed leaf swapped for a foreign hash',
      () => {
        const p = ok()
        p.shards[1].leaves[0].hash = dsha256(utf8('impostor'))
        return verifySubsetProof(p)
      }
    ]
  ]
  for (const [name, run] of cases) {
    const r = run()
    assert.equal(r.ok, false, `${name} accepted: ${JSON.stringify(r)}`)
    assert.ok(r.reason, `${name}: no reason`)
  }
})

test('mutated L_ZKDis bytes never verify (seeded walk)', () => {
  const ok = encodeZkdis(proofOf(70, [1, 63, 64, 69]))
  let s = 0x5eed
  const rnd = () => {
    s ^= s << 13
    s ^= s >>> 17
    s ^= s << 5
    return (s >>> 0) / 2 ** 32
  }
  for (let i = 0; i < 300; i++) {
    const m = Uint8Array.from(ok)
    const j = Math.floor(rnd() * m.length)
    m[j] = m[j] ^ (1 << Math.floor(rnd() * 8))
    const v = verifyZkdis(m)
    assert.equal(v.ok, false, `mutation ${i} at byte ${j} accepted`)
  }
})
