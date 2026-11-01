import { test } from 'node:test'
import assert from 'node:assert/strict'
import { encodeZkm2, decodeZkm2, ZkmCodecError, ZKM2_MAX_RECEIPT } from '../src/stark/codec.js'

const b32 = (v: number) => new Uint8Array(32).fill(v)
const proof = () => ({ imageId: b32(1), root: b32(2), size: 5, queryId: b32(3), receipt: Uint8Array.from([9, 8, 7, 6]) })

test('ZKM2 codec: round-trip is exact', () => {
  const p = proof()
  const back = decodeZkm2(encodeZkm2(p))
  assert.deepEqual(back, p)
})

test('ZKM2 codec: every malformed input is refused with ZkmCodecError', () => {
  const ok = encodeZkm2(proof())
  const mut = (f: (b: Uint8Array) => Uint8Array) => f(ok.slice())
  const bad: Array<[string, Uint8Array]> = [
    ['empty', new Uint8Array()],
    ['truncated header', ok.slice(0, 50)],
    ['bad magic', mut(b => { b[0] = 0; return b })],
    ['v1 magic', mut(b => { b[3] = 0x31; return b })],
    ['unknown version', mut(b => { b[4] = 2; return b })],
    ['size 0', mut(b => { b.fill(0, 69, 73); return b })],
    ['receiptLen 0', mut(b => { b.fill(0, 105, 109); return b })],
    ['receiptLen too large', mut(b => { new DataView(b.buffer).setUint32(105, ZKM2_MAX_RECEIPT + 1, true); return b })],
    ['truncated receipt', ok.slice(0, ok.length - 1)],
    ['trailing bytes', Uint8Array.from([...ok, 0])]
  ]
  for (const [name, b] of bad) assert.throws(() => decodeZkm2(b), ZkmCodecError, name)
  assert.throws(() => encodeZkm2({ ...proof(), root: new Uint8Array(31) }), ZkmCodecError)
  assert.throws(() => encodeZkm2({ ...proof(), size: 0 }), ZkmCodecError)
  assert.throws(() => encodeZkm2({ ...proof(), receipt: new Uint8Array() }), ZkmCodecError)
})
