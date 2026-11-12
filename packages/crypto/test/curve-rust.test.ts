// KEY-012 cross-implementation: TEA's Rust `bsvcurve` (tools/curve-vectors,
// run with Docker cargo) vs @bsv/sdk + node:crypto — byte for byte.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { createHmac } from 'node:crypto'
import { PrivateKey, PublicKey, ECDSA, BigNumber } from '@bsv/sdk'

interface Case {
  skA: string
  pubA: string
  skB: string
  pubB: string
  ecdhX: string
  salt: string
  prk: string
  info: string
  okm: string
  prehash: string
  sigRS: string
}
const V = JSON.parse(readFileSync(fileURLToPath(new URL('../../../../vectors/curve-rust.json', import.meta.url)), 'utf8')) as { cases: Case[] }
const hmac = (k: Buffer, ...d: Buffer[]) => {
  const h = createHmac('sha256', k)
  for (const x of d) h.update(x)
  return h.digest()
}

test('KEY-012: ECDH x, HKDF extract/expand and RFC 6979 low-S ECDSA match Rust bsvcurve', () => {
  assert.ok(V.cases.length >= 6)
  for (const c of V.cases) {
    const a = PrivateKey.fromHex(c.skA)
    const b = PrivateKey.fromHex(c.skB)
    assert.equal(a.toPublicKey().toString(), c.pubA, 'pubkey derivation')
    assert.equal(b.toPublicKey().toString(), c.pubB)
    const x = (sk: PrivateKey, pub: string) => sk.deriveSharedSecret(PublicKey.fromString(pub)).getX().toHex(32)
    assert.equal(x(a, c.pubB), c.ecdhX, 'ECDH x (A side)')
    assert.equal(x(b, c.pubA), c.ecdhX, 'ECDH x (B side)')
    const prk = hmac(Buffer.from(c.salt, 'hex'), Buffer.from(c.ecdhX, 'hex'))
    assert.equal(prk.toString('hex'), c.prk, 'HKDF-extract')
    assert.equal(hmac(prk, Buffer.from(c.info), Buffer.from([1])).toString('hex'), c.okm, 'HKDF-expand T(1)')
    const sig = ECDSA.sign(new BigNumber(c.prehash, 16), a, true)
    assert.equal(sig.r.toHex(32) + sig.s.toHex(32), c.sigRS, 'RFC 6979 deterministic low-S signature')
    assert.ok(ECDSA.verify(new BigNumber(c.prehash, 16), sig, a.toPublicKey()))
  }
})
