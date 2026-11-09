// conformance.test.mjs — the spec's acceptance cases as executable checks.
// Every signed artifact family: sign → verify ok, tamper → reject. Plus the
// budget invariants, stream chaining, envelope lifecycle, and revocation.
import { PrivateKey } from '@bsv/sdk'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import {
  signWant, verifyWant, signOffer, verifyOffer, signCatalog, verifyCatalog,
  signDelegation, verifyDelegation, delegationSubset, signRevocation, verifyRevocation,
  signInterval, verifyInterval, signEnvelope, verifyEnvelope, signBeacon, verifyBeacon,
  signAttestation, verifyAttestation, signSettlement, verifySettlement,
  signPresence, verifyPresence
} from '../packages/overlay/dist/src/index.js'
import { BudgetAuthority } from '../packages/mcp/dist/src/budget.js'

let pass = 0, fail = 0
const ok = (cond, name) => { if (cond) { pass++; console.log(`  ok — ${name}`) } else { fail++; console.log(`  FAIL — ${name}`) } }
const tamper = (sig) => sig.slice(0, -2) + (sig.endsWith('00') ? '01' : '00')

const key = PrivateKey.fromRandom()
const ts = Date.now()

console.log('== artifact sign/verify + tamper rejection ==')
{
  const s = signPresence(key, { endpoint: 'https://agent.example:8080', shards: ['soccer'], ts })
  ok(verifyPresence(s.ad, s.sig), 'presence verifies')
  ok(!verifyPresence(s.ad, tamper(s.sig)), 'presence rejects tampered sig')

  const c = signCatalog(key, { shards: [{ shard: 'soccer', domains: ['soccer'], entries: 3 }], ts })
  ok(verifyCatalog(c.catalog, c.sig), 'catalog verifies')
  ok(!verifyCatalog({ ...c.catalog, ts: ts + 1 }, c.sig), 'catalog rejects mutated ts')

  const w = signWant(key, { domains: ['soccer'], question: 'who scored?', maxSats: 10, ts, net: 'testnet' })
  ok(verifyWant(w.want, w.sig), 'want verifies')
  ok(!verifyWant({ ...w.want, maxSats: 999 }, w.sig), 'want rejects mutated price')
  ok(w.want.streamSeq === 1 && typeof w.want.prevHash === 'string', 'want carries streamSeq+prevHash')

  const w2 = signWant(key, { domains: ['soccer'], ts: ts + 1, net: 'testnet' })
  ok(w2.want.streamSeq === 2 && w2.want.prevHash !== w.want.prevHash, 'second want chains a different prevHash')

  const o = signOffer(key, { shard: 'soccer', seq: 0, priceSats: 50, domains: ['soccer'], ts, net: 'testnet' })
  ok(verifyOffer(o.offer, o.sig), 'offer verifies')
  ok(!verifyOffer({ ...o.offer, priceSats: 0 }, o.sig), 'offer rejects mutated price')

  const b = signBeacon(key, { endpoint: 'https://x.example', topics: ['tm_presence'], net: 'testnet', ts })
  ok(verifyBeacon(b.beacon, b.sig), 'beacon verifies')

  const i = signInterval(key, { streamId: 'presence', firstSeq: 0, lastSeq: 9, eventCount: 10, merkleRoot: 'ab'.repeat(32), ts })
  ok(verifyInterval(i.commit, i.sig), 'interval verifies')

  const st = signSettlement(key, { invoiceId: 'inv-0001', tool: 'recall', sats: 5, txid: 'cd'.repeat(32), domains: ['soccer'], ts })
  ok(verifySettlement(st.ad, st.sig), 'settlement verifies')

  const at = signAttestation(key, { modelId: 'm1', provider: 'p', claims: ['x'], ts })
  ok(verifyAttestation(at.attestation, at.sig), 'attestation verifies')
}

console.log('== envelopes: sign → verify, tamper rejections ==')
{
  const env = {
    messageId: 'm1', version: 1, sender: key.toPublicKey().toString(), recipient: PrivateKey.fromRandom().toPublicKey().toString(),
    conversationId: 'c1', operationId: 'o1', kind: 'request', createdAt: ts,
    expiresAt: ts + 60_000, ciphertextHash: 'ab'.repeat(32), encryptionProfile: 'ecdh-aesgcm',
    ciphertext: 'deadbeef', topic: 'cd'.repeat(32)
  }
  const { signEnvelope } = await import('../packages/overlay/dist/src/index.js')
  const se = signEnvelope(key, env)
  ok(verifyEnvelope(se.envelope, se.sig), 'envelope verifies')
  ok(!verifyEnvelope({ ...se.envelope, topic: 'ef'.repeat(32) }, se.sig), 'envelope rejects altered topic')
  ok(!verifyEnvelope({ ...se.envelope, ciphertextHash: '00'.repeat(32) }, se.sig), 'envelope rejects altered ciphertextHash')
}

console.log('== delegation: caps enforced in ancestry ==')
{
  const grantee = PrivateKey.fromRandom() // the parent's grantee — children must be signed by it
  const parent = signDelegation(key, { grantId: 'g-root', granteePub: grantee.toPublicKey().toString(), scope: { tools: ['pay'], domains: ['soccer'] }, spendCapSats: 500, totalCapSats: 500, perActionCapSats: 100, expiresAt: ts + 3_600_000, ts })
  ok(verifyDelegation(parent.grant, parent.sig), 'grant verifies')
  const child = signDelegation(grantee, { grantId: 'g-child', granteePub: PrivateKey.fromRandom().toPublicKey().toString(), scope: { tools: ['pay'] }, spendCapSats: 200, totalCapSats: 200, perActionCapSats: 50, expiresAt: ts + 1_800_000, parentGrantId: 'g-root', ts })
  ok(delegationSubset(child.grant, parent.grant), 'narrower child passes subset')
  const over = signDelegation(grantee, { grantId: 'g-over', granteePub: PrivateKey.fromRandom().toPublicKey().toString(), scope: {}, spendCapSats: 100, totalCapSats: 600, perActionCapSats: 100, expiresAt: ts + 1_800_000, parentGrantId: 'g-root', ts })
  ok(!delegationSubset(over.grant, parent.grant), 'child with wider totalCap rejected')
  const overAction = signDelegation(grantee, { grantId: 'g-over2', granteePub: PrivateKey.fromRandom().toPublicKey().toString(), scope: {}, spendCapSats: 100, totalCapSats: 100, perActionCapSats: 200, expiresAt: ts + 1_800_000, parentGrantId: 'g-root', ts })
  ok(!delegationSubset(overAction.grant, parent.grant), 'child with wider perActionCap rejected')

  const rv = signRevocation(key, { grantId: 'g-root', pub: key.toPublicKey().toString(), ts })
  ok(verifyRevocation(rv.revocation, rv.sig), 'revocation verifies')
}

console.log('== budget authority: caps, ancestors, replay, revocation ==')
{
  const dir = mkdtempSync(path.join(tmpdir(), 'budget-'))
  const ba = new BudgetAuthority(dir)
  ba.setCap('root', 500)
  ba.setCap('c1', 300, ['root'])
  ba.setCap('c2', 300, ['root'])

  ok(ba.reserve('c1', 'a1', 300) === 'ok', 'child1 reserves 300 under root')
  ok(ba.reserve('c2', 'a2', 300) === 'denied', 'child2 300 denied — sibling reservation visible to ancestors (N4)')
  ba.commit('c1', 'a1')
  ok(ba.reserve('c2', 'a2', 300) === 'denied', 'child2 still denied — committed counts')
  ok(ba.reserve('unknown', 'a3', 1) === 'no-cap', 'unknown grant → no-cap (fail closed)')
  ok(ba.reserve('c1', 'a1', 300) === 'exists', 'completed action → exists')
  ba.revoke('c2')
  ok(ba.reserve('c2', 'a4', 1) === 'denied', 'revoked grant → denied')
  const ba2 = new BudgetAuthority(dir)
  ok(ba2.isRevoked('c2'), 'revocation persists across restart (N6)')
  rmSync(dir, { recursive: true, force: true })
}

console.log(`\n${pass} passed, ${fail} failed`)
process.exit(fail === 0 ? 0 : 1)
