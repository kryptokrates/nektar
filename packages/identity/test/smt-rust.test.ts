// Cross-check: the TypeScript SMT port must reproduce, byte for byte, the
// vectors emitted by the reference Rust crate idattr-smt
// (tools/smt-vectors, run with: docker run rust:1-slim cargo run).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { sha256, utf8, toHex, fromHex } from '@nektar/core'
import { Smt, deriveKey, emptyRoot, verifyProof } from '../src/index.js'

const V = JSON.parse(readFileSync(fileURLToPath(new URL('../../../../vectors/smt-rust.json', import.meta.url)), 'utf8')) as {
  keys: string[]
  rootsAfterEachInsert: string[]
  rootAfterRemoveId2: string
  inclusion: { key: string; siblings: [number, string][] }
  nonInclusion: { key: string; siblings: [number, string][] }
}
const proofOf = (p: typeof V.inclusion) => ({ key: fromHex(p.key), siblings: p.siblings.map(([d, h]) => [d, fromHex(h)] as [number, Uint8Array]) })

test('SMT TS ≡ Rust idattr-smt: derive_key, roots after each insert, remove', () => {
  const t = new Smt()
  assert.equal(toHex(emptyRoot()), V.rootsAfterEachInsert[0])
  for (let i = 0; i < 8; i++) {
    const k = deriveKey(utf8(`id-${i}`))
    assert.equal(toHex(k), V.keys[i], `derive_key(id-${i})`)
    assert.equal(toHex(t.insert(k, sha256(utf8(`v-${i}`)))), V.rootsAfterEachInsert[i + 1], `root after insert ${i}`)
  }
  const incl = t.prove(deriveKey(utf8('id-3')))
  assert.deepEqual({ key: toHex(incl.key), siblings: incl.siblings.map(([d, h]) => [d, toHex(h)]) }, V.inclusion, 'inclusion proof identical')
  const non = t.prove(deriveKey(utf8('absent')))
  assert.deepEqual({ key: toHex(non.key), siblings: non.siblings.map(([d, h]) => [d, toHex(h)]) }, V.nonInclusion, 'non-inclusion proof identical')
  const root = t.root()
  assert.ok(verifyProof(proofOf(V.inclusion), { present: true, value: sha256(utf8('v-3')) }, root), 'Rust inclusion proof verifies in TS')
  assert.ok(verifyProof(proofOf(V.nonInclusion), { present: false }, root), 'Rust non-inclusion proof verifies in TS')
  assert.ok(!verifyProof(proofOf(V.nonInclusion), { present: true, value: sha256(utf8('v-3')) }, root), 'and cannot be passed off as inclusion')
  assert.equal(toHex(t.remove(deriveKey(utf8('id-2')))), V.rootAfterRemoveId2)
})
