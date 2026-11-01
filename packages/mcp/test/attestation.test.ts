// GOAL-v6 S1a — §13 cheap on-chain commitment: OP_FALSE OP_RETURN
// sha256(proof) in a fee-key-funded data tx (unit codec + MockChain spend).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { MockChain, MockFunder } from '@mamem/chain'
import { utf8, dsha256, toHex } from '@mamem/core'
import { MamemMemory } from '../src/memory.js'
import { callTool } from '../src/server.js'
import { attestationScript, decodeAttestationScript, attestationsOf, attestationCommitment, ATTEST_TAG } from '../src/onchain.js'

const fakeProof = () => dsha256(utf8('lzkmem-stand-in')) // any ≥32B payload; commitment = sha256(it)
const proofBytes = () => new Uint8Array([...fakeProof(), 1, 2, 3])

function setup () {
  const chain = new MockChain(); chain.verifyScripts = true
  const funder = new MockFunder(); funder.topup(10)
  for (const t of funder.parents) chain.registerParent(t)
  const mem = new MamemMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, modelId: 'test' })
  return { chain, funder, mem }
}

test('attestation script: encode/decode round-trip; non-attestation scripts → null', () => {
  const c = dsha256(utf8('commit'))
  const s = attestationScript(c)
  const d = decodeAttestationScript(s.toHex())!
  assert.equal(d.tag, ATTEST_TAG)
  assert.deepEqual([...d.commitment], [...c])
  // script starts OP_FALSE OP_RETURN and pushes exactly tag+hash
  assert.ok(s.toHex().startsWith('006a'), 'OP_FALSE OP_RETURN prefix')
  assert.equal(decodeAttestationScript('76a914' + '00'.repeat(20) + '88ac'), null, 'P2PKH is not an attestation')
  assert.equal(decodeAttestationScript('6a' + '044d455441'), null, 'OP_RETURN alone without OP_FALSE/tag/hash → null')
  assert.throws(() => attestationScript(new Uint8Array(31)), /32 bytes/)
})

test('anchorAttestation on MockChain: real signed spend, commitment extractable, verifyAttestation round-trip', async () => {
  const { chain, mem } = setup()
  const pb = proofBytes()
  const { txid, commitment } = await mem.anchorAttestation(pb)
  assert.equal(commitment, toHex(attestationCommitment(pb)))
  // the tx is a real signed P2PKH spend the script-verifying mock accepted
  const tx = chain.mempool.get(txid)!
  assert.ok(tx, 'broadcast accepted into mempool')
  const atts = attestationsOf(tx)
  assert.equal(atts.length, 1)
  assert.equal(toHex(atts[0]!.commitment), commitment)
  // verifyAttestation: matching proof verifies; a different proof does not
  const ok = await mem.verifyAttestation(txid, pb)
  assert.equal(ok.ok, true)
  assert.equal(ok.vout, 0)
  const bad = await mem.verifyAttestation(txid, dsha256(utf8('other proof')))
  assert.equal(bad.ok, false)
  assert.match(bad.reason!, /sha256\(proof\)/)
  assert.equal((await mem.verifyAttestation('ab'.repeat(32), pb)).ok, false)
})

test('MCP tools: attestMembership/verifyAttestation via callTool (anchorAttestation path)', async () => {
  const { mem } = setup()
  const pb = proofBytes()
  const { txid } = await mem.anchorAttestation(pb)
  const r = await callTool(mem, 'verifyAttestation', { txid, bytes: Buffer.from(pb).toString('hex') })
  assert.equal(r.ok, true)
  assert.equal(r.commitment, toHex(attestationCommitment(pb)))
})
