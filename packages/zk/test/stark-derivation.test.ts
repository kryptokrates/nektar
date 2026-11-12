// Wright §14 derivation: the committed REAL RISC Zero receipt (2-step permit
// path under the gen-vectors policy DAG) through the WASM verifier + the
// bound verifier (anchored policy root + single-use queryId).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { encodeZkder, verifyZkder, verifyDerivationBound, PINNED_DATALOG_IMAGE_ID } from '../src/stark/index.js'

const fx = JSON.parse(readFileSync(new URL('../../test/fixtures/zkder-permit.json', import.meta.url), 'utf8'))
const H = (h: string) => Uint8Array.from(Buffer.from(h, 'hex'))
const base = () => ({
  imageId: H(fx.imageId),
  policyRoot: H(fx.journal.policyRoot),
  conclusionHash: H(fx.journal.conclusionHash),
  queryId: H(fx.journal.queryId),
  receipt: H(fx.receipt)
})
const issued = (...q: string[]) => {
  const s = new Set(q)
  return { consume: (id: string) => s.delete(id) }
}

test('STARK derivation fixture: pinned program, journal matches header', () => {
  assert.equal(fx.imageId, PINNED_DATALOG_IMAGE_ID, 'fixture was produced by the pinned derivation program')
  const r = verifyZkder(encodeZkder(base()))
  assert.ok(r.ok, r.ok ? '' : r.reason)
  if (r.ok) {
    assert.equal(r.journal.policyRoot, fx.journal.policyRoot)
    assert.equal(r.journal.conclusionHash, fx.journal.conclusionHash)
    assert.equal(r.journal.version, 1)
  }
})

test('STARK derivation NEGATIVES: tampered receipt, header≠journal, foreign program', () => {
  const t = base()
  t.receipt[Math.floor(t.receipt.length / 2)] ^= 1
  const cases: Array<[string, ReturnType<typeof base>, RegExp]> = [
    ['tampered receipt', t, /stark/],
    ['policyRoot swapped in header', { ...base(), policyRoot: new Uint8Array(32).fill(7) }, /header/],
    ['conclusionHash swapped in header', { ...base(), conclusionHash: new Uint8Array(32).fill(7) }, /header/],
    ['queryId swapped in header', { ...base(), queryId: new Uint8Array(32).fill(7) }, /header/],
    ['foreign program id', { ...base(), imageId: new Uint8Array(32).fill(1) }, /pinned/]
  ]
  for (const [name, p, re] of cases) {
    const r = verifyZkder(encodeZkder(p))
    assert.equal(r.ok, false, name)
    if (!r.ok) assert.match(r.reason, re, name)
  }
  const truncated = encodeZkder(base()).slice(0, 2000)
  const r = verifyZkder(truncated)
  assert.ok(!r.ok && /codec/.test(r.reason), 'truncated bytes → codec error, never a throw')
})

test('bound verifier: anchored policyRoot + issued single-use queryId', () => {
  const bytes = encodeZkder(base())
  const reg = issued(fx.journal.queryId)
  const ok = verifyDerivationBound(bytes, reg, fx.journal.policyRoot)
  assert.ok(ok.ok, JSON.stringify(ok))
  // replay: the same proof with the same query is accepted exactly once
  const r2 = verifyDerivationBound(bytes, reg, fx.journal.policyRoot)
  assert.equal(r2.ok, false)
  assert.match(String(r2.reason), /already used|not issued/)
  // a proof is NEVER accepted against a different anchored policy root
  const wrongRoot = verifyDerivationBound(bytes, issued(fx.journal.queryId), '00'.repeat(32))
  assert.equal(wrongRoot.ok, false)
  assert.match(String(wrongRoot.reason), /not the anchored policy DAG root/)
  // unissued query
  assert.equal(verifyDerivationBound(bytes, issued(), fx.journal.policyRoot).ok, false)
  // malformed expected root fails closed
  assert.equal(verifyDerivationBound(bytes, issued(fx.journal.queryId), 'zz').ok, false)
})
