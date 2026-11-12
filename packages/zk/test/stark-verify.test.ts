// Real RISC Zero succinct receipt (committed fixture) through the WASM verifier.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { merkleRoot, dsha256, utf8, toHex } from '@nektar/core'
import { encodeZkm2, verifyZkm2, PINNED_IMAGE_ID, starkQueryId, verifyCombinedStark } from '../src/stark/index.js'

const fx = JSON.parse(readFileSync(new URL('../../test/fixtures/zkm2-n5i4.json', import.meta.url), 'utf8'))
const H = (h: string) => Uint8Array.from(Buffer.from(h, 'hex'))
const base = () => ({
  imageId: H(fx.imageId),
  root: H(fx.journal.root),
  size: fx.journal.size as number,
  queryId: H(fx.journal.queryId),
  receipt: H(fx.receipt)
})

test('STARK fixture: pinned program, proven root == core/merkle.ts root of the batch', () => {
  assert.equal(fx.imageId, PINNED_IMAGE_ID, 'fixture was produced by the pinned program')
  const leaves = Array.from({ length: 5 }, (_, i) => dsha256(utf8(`leaf-5-${i}`)))
  const r = verifyZkm2(encodeZkm2(base()))
  assert.ok(r.ok, r.ok ? '' : r.reason)
  if (r.ok) {
    assert.equal(r.journal.root, toHex(merkleRoot(leaves)))
    assert.equal(r.journal.size, 5)
  }
})

test('STARK verify NEGATIVES: tampered receipt, header≠journal, foreign/unpinned program', () => {
  const t = base()
  t.receipt[Math.floor(t.receipt.length / 2)] ^= 1
  const cases: Array<[string, ReturnType<typeof base>, RegExp]> = [
    ['tampered receipt', t, /stark/],
    ['root swapped in header', { ...base(), root: new Uint8Array(32).fill(7) }, /header/],
    ['size swapped in header', { ...base(), size: 6 }, /header/],
    ['queryId swapped in header', { ...base(), queryId: new Uint8Array(32).fill(7) }, /header/],
    ['foreign program id', { ...base(), imageId: new Uint8Array(32).fill(1) }, /pinned/]
  ]
  for (const [name, p, re] of cases) {
    const r = verifyZkm2(encodeZkm2(p))
    assert.equal(r.ok, false, name)
    if (!r.ok) assert.match(r.reason, re, name)
  }
  const truncated = encodeZkm2(base()).slice(0, 2000)
  const r = verifyZkm2(truncated)
  assert.ok(!r.ok && /codec/.test(r.reason), 'truncated bytes → codec error, never a throw')
})

test('§13 Π binding (verifyCombinedStark): access transcript and derivation journal must share the issued query', () => {
  const q = { nonce: H('ab'.repeat(32)), verifierId: 'auditor-1' }
  const qid = toHex(starkQueryId(q))
  const okT = { queryId: qid, nonce: q.nonce }
  assert.equal(verifyCombinedStark(q, okT), true, 'paired transcript accepted')
  assert.equal(verifyCombinedStark(q, { ...okT, queryId: 'ff'.repeat(32) }), false, 'transcript on a different query')
  assert.equal(verifyCombinedStark(q, { ...okT, nonce: H('cd'.repeat(32)) }), false, 'transcript with a different nonce')
  assert.equal(verifyCombinedStark(q, okT, { queryId: qid }), true, 'derivation under the same query accepted')
  assert.equal(verifyCombinedStark(q, okT, { queryId: 'ee'.repeat(32) }), false, 'derivation on a different query')
})
