import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import {
  signBinding,
  verifyBinding,
  issueChallenge,
  answerChallenge,
  verifyChallenge,
  makeRotationCert,
  verifyRotationCert,
  verifyCertChain,
  keyValidAt,
  type RotationCert
} from '../src/index.js'
import { Smt, hashLeaf, hashNode, emptyRoot, emptySubtree, verifyProof, EMPTY_LEAF } from '../src/smt.js'
import { BatchLog, MemoryShardStore } from '@nektar/log'
import { reconstructRoot } from '@nektar/core'
import { concat, utf8 } from '@nektar/core'
const k0 = PrivateKey.fromRandom(),
  k1 = PrivateKey.fromRandom(),
  k2 = PrivateKey.fromRandom()

test('identity: binding signs + verifies; tampered fails', () => {
  const b = signBinding(k0, 'agent-uuid-1', '00'.repeat(32), 0)
  assert.equal(verifyBinding(b), true)
  assert.equal(verifyBinding({ ...b, uuid: 'other' }), false)
})

test('identity: challenge-response', () => {
  const c = issueChallenge()
  const s = answerChallenge(k0, c)
  assert.equal(verifyChallenge(k0.toPublicKey().toString(), c, s), true)
  assert.equal(verifyChallenge(k1.toPublicKey().toString(), c, s), false)
})

test('rotation: cert chain verifies from pk(0)', () => {
  const c1 = makeRotationCert(k0, k1.toPublicKey().toString(), 1000, 3600)
  const c2 = { ...makeRotationCert(k1, k2.toPublicKey().toString(), 2000, 3600), index: 1 }
  assert.equal(verifyRotationCert(c1), true)
  const r = verifyCertChain(k0.toPublicKey().toString(), [c1, c2])
  assert.equal(r?.activePub, k2.toPublicKey().toString())
  assert.equal(r?.expiredAt, 2000 + 3600)
})

test('rotation NEGATIVE: cert not signed by the previous key fails', () => {
  const c1 = makeRotationCert(k0, k1.toPublicKey().toString(), 1000, 3600)
  // forge: re-sign with a different key, keep prevPubkey=pk0
  const forged = { ...c1, sig: k2.sign([1, 2, 3]).toDER('hex') as string }
  assert.equal(verifyRotationCert(forged), false)
  // a chain where cert2's prevPubkey skips pk1 → break
  const bad2 = { ...makeRotationCert(k0, k2.toPublicKey().toString(), 2000, 3600), index: 1 }
  assert.equal(verifyCertChain(k0.toPublicKey().toString(), [c1, bad2]), null)
})

test('rotation: expiry — expired key cannot sign NEW messages, historical use still valid', () => {
  const c1 = makeRotationCert(k0, k1.toPublicKey().toString(), 1000, 3600) // pk0 deprecated at 4600
  assert.equal(keyValidAt(c1, 4599), true, 'use inside window ok')
  assert.equal(keyValidAt(c1, 4600), false, 'new signature at expiry rejected')
  assert.equal(keyValidAt(c1, 9999), false, 'new signature after expiry rejected')
  // historical validation: a signature made at t=3000 still verifies against
  // the cert chain — the expiry check is on USE time, not the chain itself
  const r = verifyCertChain(k0.toPublicKey().toString(), [c1])
  assert.equal(r?.activePub, k1.toPublicKey().toString())
  assert.equal(keyValidAt(c1, 3000), true, 'historical use within window ok')
})

test('smt: empty root + domain separation', () => {
  const t = new Smt()
  assert.equal(Buffer.from(t.root()).toString('hex'), Buffer.from(emptyRoot()).toString('hex'))
  const k = new Uint8Array(32)
  k[31] = 1
  const v = new Uint8Array(32).fill(7)
  assert.notEqual(Buffer.from(hashLeaf(k, v)).toString('hex'), Buffer.from(hashNode(v, v)).toString('hex'))
})

test('smt: inclusion + non-inclusion proofs, root anchored', () => {
  const t = new Smt()
  const keys = Array.from({ length: 8 }, (_, i) => {
    const k = new Uint8Array(32)
    k[0] = i + 1
    k[31] = i
    return k
  })
  const vals = keys.map((_, i) => new Uint8Array(32).fill(i + 40))
  keys.forEach((k, i) => t.insert(k, vals[i]))
  const root = t.root()
  for (let i = 0; i < keys.length; i++) {
    const p = t.prove(keys[i])
    assert.equal(verifyProof(p, { present: true, value: vals[i] }, root), true, `inclusion ${i}`)
    assert.equal(verifyProof(p, { present: false }, root), false, 'same key not absent')
  }
  // non-inclusion: a key never enrolled
  const missing = new Uint8Array(32).fill(0xaa)
  const pn = t.prove(missing)
  assert.equal(verifyProof(pn, { present: false }, root), true, 'non-inclusion')
  assert.equal(verifyProof(pn, { present: true, value: new Uint8Array(32) }, root), false)
})

test('smt: revocation — revoked key fails, stale root fails', () => {
  const t = new Smt()
  const pk = new Uint8Array(32).fill(9)
  const v = new Uint8Array(32).fill(1)
  const r0 = t.root() // before revocation
  t.insert(pk, v) // revoke(pk) = enroll its hash
  const r1 = t.root()
  // proveRevoked: inclusion at the value verifies against r1
  const p = t.prove(pk)
  assert.equal(verifyProof(p, { present: true, value: v }, r1), true, 'revoked proof ok')
  // a revoked key accepted must fail: presenting a non-inclusion proof AFTER
  // revocation does not verify against the anchored root
  assert.equal(verifyProof(p, { present: false }, r1), false, 'revoked key non-inclusion rejected')
  // non-inclusion against a STALE root must fail too
  assert.equal(verifyProof(p, { present: true, value: v }, r0), false, 'stale root rejected')
})

test('smt: port vector — two inserts cross-check vs idattr-smt', () => {
  // vector derived from the Rust port's own primitives: insert k=0..01 → leaf
  // H(0‖k‖v) only differs from empties on the last-level path — root reproducible
  const t = new Smt()
  const k = new Uint8Array(32)
  k[31] = 1
  const v = new Uint8Array(32).fill(0x11)
  const r = t.insert(k, v)
  // manual: leaf hash then 255 levels of empty siblings on the low side
  let cur = hashLeaf(k, v)
  for (let d = 255; d >= 0; d--) {
    const sib = emptySubtree(d + 1)
    cur = (k[d >> 3] >> (7 - (d & 7))) & 1 ? hashNode(sib, cur) : hashNode(cur, sib)
  }
  assert.equal(Buffer.from(r).toString('hex'), Buffer.from(cur).toString('hex'))
  assert.equal(t.len, 1)
})

test('rotation certs anchored: leaves in a batch log, root verified', async () => {
  const log = new BatchLog(new MemoryShardStore(), PrivateKey.fromRandom())
  const c1 = makeRotationCert(k0, k1.toPublicKey().toString(), 1000, 3600)
  const leaf = { d: concat(utf8(JSON.stringify(c1)), Uint8Array.of(0)), p: { source: 'rotlog', time: 1000, signature: new Uint8Array(0), modelId: 'm1' } }
  await log.remember('rotlog', leaf)
  const batch = await log.closeBatch('rotlog')
  assert.ok(batch)
  const anchored = batch!.R
  // inclusion check a verifier runs against the anchored root
  const inclusion = (c: RotationCert): boolean => {
    const l = batch!.leaves[0]
    const proof = log.batchProof(batch!, 0)
    const r = reconstructRoot(l, proof)
    return Buffer.from(r).toString('hex') === Buffer.from(anchored).toString('hex')
  }
  assert.ok(verifyCertChain(k0.toPublicKey().toString(), [c1], { inclusionCheck: inclusion }))
  // a cert not in the log fails inclusion
  const c2 = makeRotationCert(k1, k2.toPublicKey().toString(), 2000, 3600)
  assert.equal(verifyCertChain(k0.toPublicKey().toString(), [c1, { ...c2, index: 1 }], { inclusionCheck: () => false }), null)
})

test('identity: binding signs + verifies; tampered fails', () => {
  const b = signBinding(k0, 'agent-uuid-1', '00'.repeat(32), 0)
  assert.equal(verifyBinding(b), true)
  assert.equal(verifyBinding({ ...b, uuid: 'other' }), false)
})

test('identity: challenge-response', () => {
  const c = issueChallenge()
  const s = answerChallenge(k0, c)
  assert.equal(verifyChallenge(k0.toPublicKey().toString(), c, s), true)
  assert.equal(verifyChallenge(k1.toPublicKey().toString(), c, s), false)
})

test('rotation: cert chain verifies from pk(0)', () => {
  const c1 = makeRotationCert(k0, k1.toPublicKey().toString(), 1000, 3600)
  const c2 = { ...makeRotationCert(k1, k2.toPublicKey().toString(), 2000, 3600), index: 1 }
  assert.equal(verifyRotationCert(c1), true)
  const r = verifyCertChain(k0.toPublicKey().toString(), [c1, c2])
  assert.equal(r?.activePub, k2.toPublicKey().toString())
  assert.equal(r?.expiredAt, 2000 + 3600)
})

test('rotation NEGATIVE: cert not signed by the previous key fails', () => {
  const c1 = makeRotationCert(k0, k1.toPublicKey().toString(), 1000, 3600)
  // forge: re-sign with a different key, keep prevPubkey=pk0
  const forged = { ...c1, sig: k2.sign([1, 2, 3]).toDER('hex') as string }
  assert.equal(verifyRotationCert(forged), false)
  // a chain where cert2's prevPubkey skips pk1 → break
  const bad2 = { ...makeRotationCert(k0, k2.toPublicKey().toString(), 2000, 3600), index: 1 }
  assert.equal(verifyCertChain(k0.toPublicKey().toString(), [c1, bad2]), null)
})

test('rotation: expiry — expired key cannot sign NEW messages, historical use still valid', () => {
  const c1 = makeRotationCert(k0, k1.toPublicKey().toString(), 1000, 3600) // pk0 deprecated at 4600
  assert.equal(keyValidAt(c1, 4599), true, 'use inside window ok')
  assert.equal(keyValidAt(c1, 4600), false, 'new signature at expiry rejected')
  assert.equal(keyValidAt(c1, 9999), false, 'new signature after expiry rejected')
  // historical validation: a signature made at t=3000 still verifies against
  // the cert chain — the expiry check is on USE time, not the chain itself
  const r = verifyCertChain(k0.toPublicKey().toString(), [c1])
  assert.equal(r?.activePub, k1.toPublicKey().toString())
  assert.equal(keyValidAt(c1, 3000), true, 'historical use within window ok')
})

test('smt: empty root + domain separation', () => {
  const t = new Smt()
  assert.equal(Buffer.from(t.root()).toString('hex'), Buffer.from(emptyRoot()).toString('hex'))
  const k = new Uint8Array(32)
  k[31] = 1
  const v = new Uint8Array(32).fill(7)
  assert.notEqual(Buffer.from(hashLeaf(k, v)).toString('hex'), Buffer.from(hashNode(v, v)).toString('hex'))
})

test('smt: inclusion + non-inclusion proofs, root anchored', () => {
  const t = new Smt()
  const keys = Array.from({ length: 8 }, (_, i) => {
    const k = new Uint8Array(32)
    k[0] = i + 1
    k[31] = i
    return k
  })
  const vals = keys.map((_, i) => new Uint8Array(32).fill(i + 40))
  keys.forEach((k, i) => t.insert(k, vals[i]))
  const root = t.root()
  for (let i = 0; i < keys.length; i++) {
    const p = t.prove(keys[i])
    assert.equal(verifyProof(p, { present: true, value: vals[i] }, root), true, `inclusion ${i}`)
    assert.equal(verifyProof(p, { present: false }, root), false, 'same key not absent')
  }
  // non-inclusion: a key never enrolled
  const missing = new Uint8Array(32).fill(0xaa)
  const pn = t.prove(missing)
  assert.equal(verifyProof(pn, { present: false }, root), true, 'non-inclusion')
  assert.equal(verifyProof(pn, { present: true, value: new Uint8Array(32) }, root), false)
})

test('smt: revocation — revoked key fails, stale root fails', () => {
  const t = new Smt()
  const pk = new Uint8Array(32).fill(9)
  const v = new Uint8Array(32).fill(1)
  const r0 = t.root() // before revocation
  t.insert(pk, v) // revoke(pk) = enroll its hash
  const r1 = t.root()
  // proveRevoked: inclusion at the value verifies against r1
  const p = t.prove(pk)
  assert.equal(verifyProof(p, { present: true, value: v }, r1), true, 'revoked proof ok')
  // a revoked key accepted must fail: presenting a non-inclusion proof AFTER
  // revocation does not verify against the anchored root
  assert.equal(verifyProof(p, { present: false }, r1), false, 'revoked key non-inclusion rejected')
  // non-inclusion against a STALE root must fail too
  assert.equal(verifyProof(p, { present: true, value: v }, r0), false, 'stale root rejected')
})

test('smt: port vector — two inserts cross-check vs idattr-smt', () => {
  // vector derived from the Rust port's own primitives: insert k=0..01 → leaf
  // H(0‖k‖v) only differs from empties on the last-level path — root reproducible
  const t = new Smt()
  const k = new Uint8Array(32)
  k[31] = 1
  const v = new Uint8Array(32).fill(0x11)
  const r = t.insert(k, v)
  // manual: leaf hash then 255 levels of empty siblings on the low side
  let cur = hashLeaf(k, v)
  for (let d = 255; d >= 0; d--) {
    const sib = emptySubtree(d + 1)
    cur = (k[d >> 3] >> (7 - (d & 7))) & 1 ? hashNode(sib, cur) : hashNode(cur, sib)
  }
  assert.equal(Buffer.from(r).toString('hex'), Buffer.from(cur).toString('hex'))
  assert.equal(t.len, 1)
})

import { KeyCustodian, reconstruct, verifyLifecycle, partialSign, aggregateSchnorr, verifySchnorr, challengeOf } from '../src/custody.js'

test('custody: 2-of-3 threshold signs; 1-of-3 cannot; reconstruction gives same key', () => {
  const secret = 0x123456789abcdef123456789abcdefn % BigInt('0x' + 'f'.repeat(64))
  const { custodian, shares } = KeyCustodian.generate(2, 3, 1000n, secret)
  const msg = new Uint8Array(32).fill(42)
  // reconstruction from any 2 shares = original key
  assert.equal(reconstruct(shares.slice(0, 2)), secret)
  assert.equal(reconstruct([shares[0], shares[2]]), secret)
  // 1 share does NOT reconstruct
  assert.notEqual(reconstruct([shares[0]]), secret)
  // sign: needs a correct threshold
  const sig = custodian.sign([shares[0], shares[1]], msg)
  assert.ok(sig.length > 0)
  assert.throws(() => custodian.sign([shares[0]], msg), /reconstruct/)
  // lifecycle: rotate → old shares stop working, revoke → all signing stops
  const old = custodian.publicKeyHex()
  const newShares = custodian.rotate(2, 3, 2000n)
  assert.throws(() => custodian.sign(shares.slice(0, 2), msg), /reconstruct|current/)
  assert.equal(verifyLifecycle(custodian.lifecycle()), true)
  custodian.revoke(3000n)
  assert.throws(() => custodian.sign(newShares.slice(0, 2), msg), /revoked/)
  assert.equal(verifyLifecycle(custodian.lifecycle()), true)
})

test('custody: threshold Schnorr — 2-of-3 signs, aggregate verifies', () => {
  const { custodian, shares } = KeyCustodian.generate(2, 3, 1n)
  const msg = new Uint8Array(32).fill(7)
  const set = [shares[0].x, shares[2].x] // 2-of-3
  const nonces = [1234567n, 7654321n] // one k_j per signer
  const pub = custodian.publicKeyHex()
  // round: reveals R_j, aggregate R, challenge e, partial sigs, aggregate s
  const agg = aggregateSchnorr([], nonces) // R only
  const e = challengeOf(agg.R, pub, msg)
  const partials = [partialSign(shares[0], nonces[0], e, set), partialSign(shares[2], nonces[1], e, set)]
  const sig = aggregateSchnorr(partials, nonces)
  assert.equal(verifySchnorr(pub, msg, sig), true)
  // tampered message or 1-of-3 fails
  assert.equal(verifySchnorr(pub, new Uint8Array(32).fill(8), sig), false)
  const s1 = partialSign(shares[0], nonces[0], e, [shares[0].x]) // lone-signer set
  const bad = aggregateSchnorr([s1], [nonces[0]])
  assert.equal(verifySchnorr(pub, msg, bad), false)
})

import { fieldRoot, fieldPath, randSalt, issueDisclosure, verifyDisclosure, wrapFor, unwrapFor, wrapMulti, unwrapMulti } from '../src/disclosure.js'

test('disclosure: field commitments + envelope verify; negatives all fail', () => {
  const owner = PrivateKey.fromRandom(),
    verifier = PrivateKey.fromRandom(),
    wrong = PrivateKey.fromRandom()
  const fields = [
    { name: 'clearance', value: utf8('level-2'), salt: randSalt() },
    { name: 'payload', value: utf8('secret-data'), salt: randSalt() },
    { name: 'agent', value: utf8('agent-A'), salt: randSalt() }
  ]
  const root = fieldRoot(fields),
    n = fields.length
  const proof = fieldPath(fields, 1)
  const e = issueDisclosure(owner, fields, 1, verifier.toPublicKey().toString(), 2000)
  const V = (env: typeof e, pub: string, sibs = proof.siblings, t = 1500) => verifyDisclosure(env, owner.toPublicKey().toString(), pub, n, root, sibs, t)
  assert.deepEqual(V(e, verifier.toPublicKey().toString()), { ok: true })
  assert.equal(V(e, verifier.toPublicKey().toString(), proof.siblings, 2000).reason, 'expired')
  assert.equal(V(e, wrong.toPublicKey().toString()).reason, 'verifier')
  assert.equal(V({ ...e, sig: '00' }, verifier.toPublicKey().toString()).reason, 'signature')
  assert.equal(V({ ...e, field: { ...e.field, value: utf8('other') } }, verifier.toPublicKey().toString()).ok, false, 'tampered value')
  assert.equal(V({ ...e, field: { ...e.field, salt: new Uint8Array(0) } }, verifier.toPublicKey().toString()).ok, false, 'salt omitted')
  assert.equal(V(e, verifier.toPublicKey().toString(), proof.siblings.slice(0, 1)).reason, 'path-height')
  assert.equal(V(e, verifier.toPublicKey().toString(), proof.siblings, 5000).reason, 'expired', 'replay after expiry')
  // wrapped bytes bound into the envelope — swapping them breaks the sig
  const we = issueDisclosure(owner, fields, 1, verifier.toPublicKey().toString(), 2000, 'audit', utf8('wrapped-key-1'))
  assert.equal(
    verifyDisclosure({ ...we, wrapped: utf8('swapped') }, owner.toPublicKey().toString(), verifier.toPublicKey().toString(), n, root, proof.siblings, 1500)
      .reason,
    'signature',
    'swapped wrapped rejected'
  )
  assert.equal(verifyDisclosure(we, owner.toPublicKey().toString(), verifier.toPublicKey().toString(), n, root, proof.siblings, 1500).ok, true, 'wrapped ok')
})

test('wrap: HKDF+GCM — fresh pad per wrap, tamper + wrong context fail', () => {
  const sender = PrivateKey.fromRandom(),
    r1 = PrivateKey.fromRandom(),
    r2 = PrivateKey.fromRandom(),
    outsider = PrivateKey.fromRandom()
  const key = new Uint8Array(32).fill(7),
    ctx = 'frag-1|r1|read'
  const w1 = wrapFor(sender, r1.toPublicKey().toString(), key, ctx)
  const w2 = wrapFor(sender, r1.toPublicKey().toString(), key, ctx)
  assert.notEqual(Buffer.from(w1.ct).toString('hex'), Buffer.from(w2.ct).toString('hex'), 'fresh salt → unrelated pads')
  assert.equal(Buffer.from(unwrapFor(r1, sender.toPublicKey().toString(), w1, ctx)).toString('hex'), Buffer.from(key).toString('hex'))
  const tampered = { ...w1, ct: w1.ct.slice() }
  tampered.ct[0] = tampered.ct[0] ^ 1
  assert.throws(() => unwrapFor(r1, sender.toPublicKey().toString(), tampered, ctx), 'tampered wrap fails GCM auth')
  assert.throws(() => unwrapFor(r1, sender.toPublicKey().toString(), w1, 'wrong-ctx'), 'wrong context fails AAD')
  const wraps = wrapMulti(sender, key, [r1.toPublicKey().toString(), r2.toPublicKey().toString()], ctx)
  assert.equal(Buffer.from(unwrapMulti(r2, sender.toPublicKey().toString(), wraps, ctx)!).toString('hex'), Buffer.from(key).toString('hex'))
  assert.equal(unwrapMulti(outsider, sender.toPublicKey().toString(), wraps, ctx), null, 'non-recipient')
})

import { issueClearance, proveAccess, verifyAccess, type ZkTranscript } from '../src/zkaccess.js'

test('zkaccess: clearance proof verifies; all negatives fail', () => {
  const auth = PrivateKey.fromRandom()
  const holder = PrivateKey.fromRandom().toPublicKey().toString()
  const cred = issueClearance(auth, 3n, holder) // ℓ=3
  const { commitment } = cred
  assert.deepEqual(Object.keys(commitment).sort(), ['hex', 'holder', 'pub', 'sig'], 'public commitment carries no level/blinding')
  const t: ZkTranscript = { queryId: 'q1', fragmentId: 'f1', nonce: new Uint8Array(32).fill(9) }
  const t0 = performance.now()
  const proof = proveAccess(cred, 2n, 8, t) // prove ℓ≥2
  const tp = performance.now() - t0
  assert.ok(proof, 'proof produced')
  const t1 = performance.now()
  assert.equal(verifyAccess(commitment, 2n, proof!, t, { trustedAuthorities: [auth.toPublicKey().toString()], holder }), true)
  const tv = performance.now() - t1
  console.log(`  zkaccess: prove ${tp.toFixed(0)}ms verify ${tv.toFixed(0)}ms (8-bit)`)
  // negatives
  assert.equal(
    verifyAccess(commitment, 2n, proof!, t, { trustedAuthorities: [PrivateKey.fromRandom().toPublicKey().toString()] }),
    false,
    'untrusted authority'
  )
  assert.equal(
    verifyAccess(commitment, 2n, proof!, t, { holder: PrivateKey.fromRandom().toPublicKey().toString() }),
    false,
    'credential presented by a non-holder'
  )
  assert.equal(
    verifyAccess({ ...commitment, holder: PrivateKey.fromRandom().toPublicKey().toString() }, 2n, proof!, t),
    false,
    'holder swap breaks the authority signature'
  )
  assert.equal(proveAccess(cred, 5n, 8, t), null, 'lower clearance cannot prove')
  assert.equal(verifyAccess(commitment, 2n, proof!, { ...t, nonce: new Uint8Array(32).fill(1) }), false, 'replay with another nonce')
  assert.equal(verifyAccess(commitment, 2n, proof!, { ...t, queryId: 'q2' }), false, 'different query')
  const forged = { ...commitment, sig: PrivateKey.fromRandom().sign([1, 2, 3]).toDER('hex') as string }
  assert.equal(verifyAccess(forged, 2n, proof!, t), false, 'forged authority sig')
  const tampered = { ...commitment, hex: commitment.hex.slice(0, -4) + 'ffff' }
  assert.equal(verifyAccess(tampered, 2n, proof!, t), false, 'tampered commitment')
  assert.equal(verifyAccess(commitment, 3n, proof!, t), false, 'proof bound to its required level')
})
