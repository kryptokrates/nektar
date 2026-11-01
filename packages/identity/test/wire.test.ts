import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { issueClearance, proveAccess, verifyAccess, encodePresentation, decodePresentation, encodeCredential, decodeCredential, WireError } from '../src/index.js'

test('wire: clearance presentation + credential round-trip through JSON and still verify', () => {
  const authority = PrivateKey.fromRandom(); const holder = PrivateKey.fromRandom().toPublicKey().toString()
  const cred = issueClearance(authority, 3n, holder)
  const transcript = { queryId: 'q1', fragmentId: 's/2', nonce: Uint8Array.from([1, 2, 3, 4]) }
  const p = { commitment: cred.commitment, proof: proveAccess(cred, 2n, 8, transcript)!, transcript, requiredLevel: 2n }
  const back = decodePresentation(JSON.parse(JSON.stringify(encodePresentation(p))))
  assert.deepEqual(back.transcript.nonce, transcript.nonce); assert.equal(back.requiredLevel, 2n)
  assert.ok(verifyAccess(back.commitment, back.requiredLevel, back.proof, back.transcript, { trustedAuthorities: [authority.toPublicKey().toString()], holder }))
  const c2 = decodeCredential(JSON.parse(JSON.stringify(encodeCredential(cred))))
  assert.equal(c2.level, 3n); assert.deepEqual(c2.commitment, cred.commitment)
})

test('wire: strict decoding rejects malformed input with WireError', () => {
  const authority = PrivateKey.fromRandom(); const holder = PrivateKey.fromRandom().toPublicKey().toString()
  const cred = issueClearance(authority, 3n, holder)
  const t = { queryId: 'q', fragmentId: 's/0', nonce: new Uint8Array(4) }
  const w = encodePresentation({ commitment: cred.commitment, proof: proveAccess(cred, 1n, 8, t)!, transcript: t, requiredLevel: 1n })
  const bad: unknown[] = [
    null, [], 'x',
    { ...w, requiredLevel: 2 }, { ...w, requiredLevel: '-1' }, { ...w, requiredLevel: '01' },
    { ...w, transcript: { ...w.transcript, nonce: 'zz' } }, { ...w, transcript: { ...w.transcript, nonce: 'abc' } },
    { ...w, commitment: { ...w.commitment, pub: 'AB'.repeat(33) } }, { ...w, commitment: { ...w.commitment, level: '3' } },
    { ...w, proof: {} }
  ]
  for (const b of bad) assert.throws(() => decodePresentation(b), WireError, JSON.stringify(b)?.slice(0, 60))
  assert.throws(() => decodeCredential({ ...encodeCredential(cred), blinding: 'x' }), WireError)
})
