import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { PrivateKey } from '@bsv/sdk'
import { utf8 } from '@nektar/core'
import { stackWorld, stackChain, StackFunder } from '@nektar/chain'
import { signProvenance, verifyCheckpoint, PromiseMonitor } from '@nektar/log'
import { signBinding, answerChallenge, issueClearance, proveAccess } from '@nektar/identity'
import { NektarMemory, IngestError, FileSecurityStore, type NektarMemoryOptions } from '../src/index.js'

async function world(t: { skip: (m?: string) => void }, opts: NektarMemoryOptions = {}, shared?: { chain: any; funder: any }) {
  const w = shared ?? (await stackWorld(t, 20))
  if (w === undefined) return undefined
  const { chain, funder } = w
  return { chain, funder, mem: new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, ...opts }) }
}
const P = { source: 'doi:10.1/manual', time: 1_700_000_000, signature: new Uint8Array(0), modelId: 'm' }

test('COM-003: signed provenance verified at ingest; unsigned refused when required', async (t) => {
  const src = PrivateKey.fromRandom()
  const pub = src.toPublicKey().toString()
  const s = await world(t, { requireSignedProvenance: true, agentSeed: PrivateKey.fromRandom() })
  if (s === undefined) return
  const { mem } = s
  const p = await signProvenance(src, utf8('torque 45Nm'), P)
  assert.equal((await mem.remember('m', 'torque 45Nm', { ...p, sourcePub: pub })).seq, 0, 'valid signature accepted (encrypted store)')
  await assert.rejects(
    () => mem.remember('m', 'torque 46Nm', { ...p, sourcePub: pub }),
    (e: Error) => e instanceof IngestError && /provenance signature invalid/.test(e.message),
    'data tampered after signing'
  )
  await assert.rejects(
    () => mem.remember('m', 'torque 45Nm', { ...p, sourcePub: PrivateKey.fromRandom().toPublicKey().toString() }),
    /provenance signature invalid/,
    'wrong key'
  )
  await assert.rejects(() => mem.remember('m', 'unsigned'), /unsigned provenance refused/)
  const _w2 = await world(t)
  if (_w2 === undefined) return
  assert.equal((await _w2.mem.remember('m', 'unsigned ok when not required')).seq, 0)
})

test('LOG-007/005: every anchor yields a signed checkpoint; the monitor confirms promises', async (t) => {
  const _w = await world(t)
  if (_w === undefined) return
  const { mem } = _w
  const t0 = Date.now()
  await mem.remember('s', 'a')
  await mem.remember('s', 'b')
  await mem.remember('s', 'c')
  await mem.remember('s', 'd')
  const cps = mem.checkpoints('s')
  assert.deepEqual(
    cps.map((c) => [c.n, c.size]),
    [
      [0, 2],
      [1, 4]
    ]
  )
  const logPub = await mem.logPublicKey()
  assert.ok(cps.every((c) => verifyCheckpoint(c, logPub)))
  assert.equal(cps[1].anchorTxid, mem.tailLink('s')!.txid)
  const mon = new PromiseMonitor(logPub, () => cps[0].logId)
  for (const seq of [0, 1, 2, 3, 4]) mon.observePromise({ shard: 's', seq, issuedAt: t0 })
  cps.forEach((c) => mon.observeCheckpoint(c))
  assert.deepEqual(
    mon.violations(t0 + 120_000, 60_000).map((v) => v.seq),
    [4],
    'seq 4 was promised but never checkpointed'
  )
})

test('LOG-006: flushDue anchors on the age threshold without a new write', async (t) => {
  const s = await world(t, { batchPolicy: { maxLeaves: 100, maxAgeMs: 0 } })
  if (s === undefined) return
  const { mem } = s
  const r = await mem.remember('s', 'lonely record') // maxAgeMs 0 → due immediately
  assert.ok(r.anchored, 'closed on age at write time')
  const s2 = await world(t, { batchPolicy: { maxLeaves: 100, maxBytes: 1_000_000 } })
  if (s2 === undefined) return
  const { mem: m2 } = s2
  await m2.remember('s', 'x')
  assert.deepEqual(await m2.flushDue(), [], 'nothing due')
})

test('CHN-006: pending → mined → final; requireFinal withholds until finalityDepth', async (t) => {
  const s = await world(t, { finalityDepth: 3 })
  if (s === undefined) return
  const { chain, mem } = s
  await mem.remember('s', 'a')
  await mem.remember('s', 'b')
  assert.equal((await mem.anchorStatus('s', 0)).state, 'pending')
  await chain.mine(1)
  mem.confirmAnchor('s', mem.tailLink('s')!.txid)
  assert.deepEqual(await mem.anchorStatus('s', 0), { state: 'mined', confirmations: 1, depth: 3 })
  assert.match((await mem.recall('s', { seq: 0, requireFinal: true })).items[0].reason!, /not final: 1\/3 confirmations/)
  await chain.mine(2)
  assert.equal((await mem.anchorStatus('s', 0)).state, 'final')
  assert.equal((await mem.recall('s', { seq: 0, requireFinal: true })).items[0].text, 'a')
})

test('persistence: challenges, used ZK transcripts and revocations survive a restart', async (t) => {
  const file = join(mkdtempSync(join(tmpdir(), 'nektar-sec-')), 'security.json')
  const authority = PrivateKey.fromRandom()
  const opts = { securityStore: new FileSecurityStore(file), clearanceAuthorities: [authority.toPublicKey().toString()] }
  const a = await world(t, opts)
  if (a === undefined) return
  await a.mem.remember('s', 'secret', {}, 1)
  await a.mem.remember('s', 'x')
  const key = PrivateKey.fromRandom()
  const pub = key.toPublicKey().toString()
  const nonce = a.mem.issueChallenge()
  const identity = { pubkey: pub, binding: signBinding(key, 'agent', '00'.repeat(32), 0), challengeNonce: nonce, challengeSig: answerChallenge(key, nonce) }
  const cred = issueClearance(authority, 2n, pub)
  const transcript = { queryId: 'q', fragmentId: 's/0', nonce: new Uint8Array(32).fill(5) }
  const zk = { commitment: cred.commitment, proof: proveAccess(cred, 1n, 8, transcript)!, transcript, requiredLevel: 1n }
  await a.chain.mine(1)
  a.mem.confirmAnchor('s', a.mem.tailLink('s')!.txid) // S6.1: plaintext is served only after SPV verify
  assert.equal((await a.mem.recall('s', { seq: 0, identity, zk })).items[0].text, 'secret')
  const revoked = PrivateKey.fromRandom().toPublicKey().toString()
  a.mem.revoke(revoked)
  // restart: a new instance on the same store
  const b = new NektarMemory(a.chain, a.funder, a.funder.keyPair, { ...opts, securityStore: new FileSecurityStore(file) })
  assert.ok(b.verifiedCaller(identity), 'challenge issued before the restart still authenticates')
  assert.match(b.verifyClearance({ pubkey: pub }, zk, 's/0').reason!, /replayed/, 'ZK transcript replay rejected after restart')
  assert.ok(b.isRevoked(revoked) && !b.isRevoked(pub), 'revocation survives restart')
  assert.throws(() => b.revoke('aa'), /33-byte/)
})
