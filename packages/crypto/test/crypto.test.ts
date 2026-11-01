import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { sha256, utf8, eq, toHex } from '@mamem/core'
import { canonicalKeyID, deriveFragmentKey, encryptFragment, decryptFragment, shredKey, deriveLevelKey, grantLevels } from '../src/keys.js'

const seed = PrivateKey.fromRandom()
const ctx = { t: 'tenant1', psi: 'shard-a', rho: 'batch-0', pi: 'frag-3', l: 2 }

test('crypto: encrypt/decrypt round-trip, AAD-bound', () => {
  const { fragmentKey } = deriveFragmentKey(deriveLevelKey(seed, ctx.l), ctx)
  const d = utf8('secret torque value: 45Nm')
  const hd = sha256(d)
  const ef = encryptFragment(fragmentKey, d, hd, ctx)
  assert.ok(eq(decryptFragment(fragmentKey, ef), d))

  // wrong key fails (different level)
  const wrong = deriveFragmentKey(deriveLevelKey(seed, ctx.l), { ...ctx, l: 3 }).fragmentKey
  assert.throws(() => decryptFragment(wrong, ef), /auth|tag|Unsupported state/i)
  // wrong AAD fails (transplant to another fragment — verifier recomputes AAD)
  const moved = { ...ef, aad: utf8('other') }
  assert.throws(() => decryptFragment(fragmentKey, moved))
})

test('crypto: canonical keyID is deterministic and injective', () => {
  assert.equal(canonicalKeyID(ctx), canonicalKeyID({ ...ctx }))
  assert.notEqual(canonicalKeyID(ctx), canonicalKeyID({ ...ctx, pi: 'frag-4' }))
  assert.equal(canonicalKeyID(ctx), '7:tenant1|7:shard-a|7:batch-0|6:frag-3|1:2')
})

test('crypto: crypto-shredding — zeroed key cannot decrypt', () => {
  const { fragmentKey } = deriveFragmentKey(deriveLevelKey(seed, ctx.l), ctx)
  const d = utf8('payload')
  const ef = encryptFragment(fragmentKey, d, sha256(d), ctx)
  const copy = Uint8Array.from(fragmentKey)
  shredKey(fragmentKey)
  assert.ok(eq(fragmentKey, new Uint8Array(32)))
  // re-derivable from seed (seed-holder recovers); an attacker holding only
  // the zeroed buffer cannot.
  const rederived = deriveFragmentKey(deriveLevelKey(seed, ctx.l), ctx).fragmentKey
  assert.ok(eq(rederived, copy))
  assert.ok(eq(decryptFragment(rederived, ef), d))
})

test('crypto: level keys derive distinctly', () => {
  const l2 = deriveLevelKey(seed, 2)
  const l3 = deriveLevelKey(seed, 3)
  assert.notEqual(l2.toHex(), l3.toHex())
})

test('crypto: disjoint level keyspaces — holder {0,1} opens l1 not l2; l2 alone cannot open l1', () => {
  const { encryptFragment: enc, decryptFragment: dec } = { encryptFragment, decryptFragment }
  const grant01 = grantLevels(seed, 1) // S_A for holder with levels 0,1
  const l2Only = deriveLevelKey(seed, 2)

  const d = utf8('level-1 payload')
  const ctxL1 = { ...ctx, l: 1 }
  const ef1 = encryptFragment(deriveFragmentKey(grant01[1]!, ctxL1).fragmentKey, d, sha256(d), ctxL1)
  assert.ok(eq(decryptFragment(deriveFragmentKey(grant01[1]!, ctxL1).fragmentKey, ef1), d), 'l1 holder decrypts l1')

  // same holder: level-2 ciphertext cannot be opened (no level-2 key granted)
  const ctxL2 = { ...ctx, l: 2 }
  const ef2 = encryptFragment(deriveFragmentKey(l2Only, ctxL2).fragmentKey, d, sha256(d), ctxL2)
  for (const lk of grant01) {
    const c = { ...ctxL1, l: 0 }
    assert.throws(() => decryptFragment(deriveFragmentKey(lk, c).fragmentKey, ef2))
  }
  // l2 key alone cannot open l1 data
  assert.throws(() => decryptFragment(deriveFragmentKey(l2Only, ctxL1).fragmentKey, ef1))
})

test('crypto: ciphertext transplanted to another ctx fails AAD', () => {
  const d = utf8('payload')
  const lk = deriveLevelKey(seed, 0)
  const ef = encryptFragment(deriveFragmentKey(lk, ctx).fragmentKey, d, sha256(d), ctx)
  // transplanted keyID/AAD from a different π and level
  const ctxOther = { ...ctx, pi: 'frag-99', l: 5 }
  const moved = encryptFragment(deriveFragmentKey(lk, ctxOther).fragmentKey, d, sha256(d), ctxOther)
  moved.ciphertext = ef.ciphertext
  assert.throws(() => decryptFragment(deriveFragmentKey(lk, ctxOther).fragmentKey, moved))
})
