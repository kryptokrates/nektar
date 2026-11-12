import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { dsha256, utf8 } from '@nektar/core'
import { stackWorld } from '@nektar/chain'
import { AnchorChain } from '../src/runarchain.js'
import { Smt, verifyProof } from '@nektar/identity'
import { verifyCertChain, makeRotationCert, type RotationCert } from '@nektar/identity'

test('smt revocation: anchored root verifies a status proof; stale root fails', async (t) => {
  const w = await stackWorld(t, 6)
  if (w === undefined) return
  const { chain, funder: f } = w
  const smt = new Smt()
  const pk = new Uint8Array(32).fill(9)
  const ac = new AnchorChain('revoke', f.keyPair, f, f.keyPair, chain)
  const h0 = await ac.anchor(1, smt.root()) // R = empty revocation root
  const v = new Uint8Array(32).fill(1)
  smt.insert(pk, v) // revoke(pk)
  const h1 = await ac.anchor(1, smt.root()) // R = root after revocation
  await chain.mine(1)
  const p = smt.prove(pk)
  assert.equal(verifyProof(p, { present: true, value: v }, h1.head.data.R), true, 'revoked-status vs anchored root')
  assert.equal(verifyProof(p, { present: true, value: v }, h0.head.data.R), false, 'vs stale anchored root')
  assert.equal(verifyProof(p, { present: false }, h1.head.data.R), false, 'revoked key non-inclusion rejected')
  const h1m = (await chain.getTx(h1.tx.id('hex')))?.merklePath
  assert.ok(h1m, 'anchor tx mined with BUMP')
  assert.equal(await chain.isValidRootForHeight(h1m.computeRoot(h1.tx.id('hex')), h1m.blockHeight), true)
})

test('rotated anchor chain verifies alongside the cert chain', async (t) => {
  const w = await stackWorld(t, 10)
  if (w === undefined) return
  const { chain, funder: f } = w
  const ac = new AnchorChain('s', f.keyPair, f, f.keyPair, chain)
  await ac.anchor(1, dsha256(utf8('d')))
  const newKey = PrivateKey.fromRandom()
  await ac.rotate(newKey)
  const certs = (ac as any).rotationCerts as RotationCert[]
  assert.equal(certs.length, 1)
  const r = verifyCertChain(f.keyPair.toPublicKey().toString(), certs)
  assert.equal(r?.activePub, newKey.toPublicKey().toString())
})

import { BatchLog, MemoryShardStore } from '@nektar/log'
import { reconstructRoot } from '@nektar/core'

test('rotation-cert log: batch root anchored on-chain; cert verifies via inclusion', async (t) => {
  const w = await stackWorld(t, 6)
  if (w === undefined) return
  const { chain, funder: f } = w
  const log = new BatchLog(new MemoryShardStore(), PrivateKey.fromRandom())
  const ac = new AnchorChain('rotlog', f.keyPair, f, f.keyPair, chain)
  const k0 = PrivateKey.fromRandom(),
    k1 = PrivateKey.fromRandom()
  const cert = await makeRotationCert(k0, k1.toPublicKey().toString(), 1000, 3600)
  await log.remember('rotlog', { d: utf8(JSON.stringify(cert)), p: { source: 'rotlog', time: 1000, signature: new Uint8Array(0), modelId: 'm1' } })
  const batch = await log.closeBatch('rotlog')
  // anchor the batch root through the AnchorChain shard (mined, BUMP)
  const h = await ac.anchor(batch!.size, batch!.R)
  await chain.mine(1)
  assert.ok((await chain.getTx(h.tx.id('hex')))?.merklePath)
  // inclusion check against the ANCHORED root
  const proof = log.batchProof(batch!, 0)
  const recomputed = reconstructRoot(batch!.leaves[0], proof)
  const inLog = (c: RotationCert) =>
    JSON.parse(Buffer.from(batch!.records[0].fragment.d).toString()).sig === c.sig &&
    Buffer.from(recomputed).toString('hex') === Buffer.from(h.head.data.R).toString('hex')
  const r = verifyCertChain(k0.toPublicKey().toString(), [cert], { inclusionCheck: inLog })
  assert.ok(r?.activePub === k1.toPublicKey().toString())
  // a cert never anchored fails the anchored-inclusion check
  const outsider = await makeRotationCert(k1, PrivateKey.fromRandom().toPublicKey().toString(), 2000, 3600)
  assert.equal(verifyCertChain(k0.toPublicKey().toString(), [cert, { ...outsider, index: 1 }], { inclusionCheck: inLog }), null)
})

import { issueDisclosure, fieldPath, randSalt, fieldsCommitment, type FieldCommitment } from '@nektar/identity'

test('DIS-006: disclosure event logged as a leaf and anchored', async (t) => {
  const w = await stackWorld(t, 6)
  if (w === undefined) return
  const { chain, funder: f } = w
  const log = new BatchLog(new MemoryShardStore(), PrivateKey.fromRandom())
  const ac = new AnchorChain('discl', f.keyPair, f, f.keyPair, chain)
  const owner = PrivateKey.fromRandom(),
    verifier = PrivateKey.fromRandom()
  const fields: FieldCommitment[] = [
    { name: 'a', value: utf8('1'), salt: randSalt() },
    { name: 'b', value: utf8('2'), salt: randSalt() }
  ]
  const env = issueDisclosure(owner, fields, 0, verifier.toPublicKey().toString(), 3000)
  // the disclosure EVENT (envelope) becomes a log leaf, anchored like fragments
  await log.remember('discl', { d: utf8(JSON.stringify(env)), p: { source: 'discl', time: env.expiresAt, signature: new Uint8Array(0), modelId: 'm1' } })
  const batch = await log.closeBatch('discl')
  const h = await ac.anchor(batch!.size, batch!.R)
  await chain.mine(1)
  assert.ok((await chain.getTx(h.tx.id('hex')))?.merklePath)
  const recomputed = reconstructRoot(batch!.leaves[0], log.batchProof(batch!, 0))
  assert.equal(Buffer.from(recomputed).toString('hex'), Buffer.from(h.head.data.R).toString('hex'), 'disclosure event committed to anchored root')
  // and the field commitment binds count+root for the anchored fragment path
  const fc = fieldsCommitment(fields)
  assert.ok(fc.length === 32)
})
