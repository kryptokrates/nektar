import { test } from 'node:test'
import assert from 'node:assert/strict'
import { encodeZkder, decodeZkder, ZkdCodecError, ZKD3_MAX_RECEIPT, ALG_SHA256D } from '../src/stark/datalog.js'

const b32 = (v: number) => new Uint8Array(32).fill(v)
const proof = () => ({
  algorithm: ALG_SHA256D,
  imageId: b32(1),
  policyRoot: b32(2),
  conclusionHash: b32(4),
  queryId: b32(3),
  receipt: Uint8Array.from([9, 8, 7, 6])
})

test('ZKD3 codec: round-trip is exact', () => {
  const p = proof()
  const back = decodeZkder(encodeZkder(p))
  assert.deepEqual(back, p)
})

test('ZKD3 codec (GEN-007): rev-2 carries the algorithm id; rev-1 still decodes', () => {
  const ok = encodeZkder(proof())
  assert.equal(ok[4], 2)
  assert.equal(ok[5], ALG_SHA256D)
  const v1 = Uint8Array.from([...ok.slice(0, 4), 1, ...ok.slice(6)])
  assert.equal(decodeZkder(v1).algorithm, ALG_SHA256D)
  const badAlg = ok.slice()
  badAlg[5] = 0x7f
  assert.throws(() => decodeZkder(badAlg), ZkdCodecError)
})

test('ZKD3 codec: every malformed input is refused with ZkdCodecError', () => {
  const ok = encodeZkder(proof())
  const mut = (f: (b: Uint8Array) => Uint8Array) => f(ok.slice())
  const bad: Array<[string, Uint8Array]> = [
    ['empty', new Uint8Array()],
    ['truncated header', ok.slice(0, 50)],
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
        b[3] = 0x32
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
      'receiptLen 0',
      mut((b) => {
        b.fill(0, 134, 138)
        return b
      })
    ],
    [
      'receiptLen too large',
      mut((b) => {
        new DataView(b.buffer).setUint32(134, ZKD3_MAX_RECEIPT + 1, true)
        return b
      })
    ],
    ['truncated receipt', ok.slice(0, ok.length - 1)],
    ['trailing bytes', Uint8Array.from([...ok, 0])]
  ]
  for (const [name, b] of bad) assert.throws(() => decodeZkder(b), ZkdCodecError, name)
  assert.throws(() => encodeZkder({ ...proof(), policyRoot: new Uint8Array(31) }), ZkdCodecError)
  assert.throws(() => encodeZkder({ ...proof(), conclusionHash: new Uint8Array(64) }), ZkdCodecError)
  assert.throws(() => encodeZkder({ ...proof(), receipt: new Uint8Array() }), ZkdCodecError)
})
