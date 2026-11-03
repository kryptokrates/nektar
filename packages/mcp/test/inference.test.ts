// Model-execution attestation (nChain ai_inference Groth16): the statement
// poseidon(input‖output‖poseidon(model)) re-derives in pure TS; receipts can
// embed a validated attestation; verify surfaces a three-state model check —
// declared → statement-attested → consensus-verified (verifierTxid).
// Only the canned demo vector has a proof today (dev trusted setup) — any
// other (input,output) is refused before it can enter a receipt.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { MockChain, MockFunder } from '@mamem/chain'
import { MamemMemory, type MamemMemoryOptions } from '../src/memory.js'
import { callTool } from '../src/server.js'
import { verifyReceipt } from '../src/receipt.js'
import { AIINF_DEMO, demoAttestation, validateAttestation } from '@mamem/zk'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

// point the artifact lookup somewhere empty — on-chain spend is e2e's job
process.env.MAMEM_GROTH16_OUT = '/nonexistent-mamem-aiinf'

function setup (opts: MamemMemoryOptions = {}) {
  const chain = new MockChain(); chain.verifyScripts = true
  const funder = new MockFunder(); funder.topup(20)
  for (const t of funder.parents) chain.registerParent(t)
  const mem = new MamemMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, dataDir: mkdtempSync(join(tmpdir(), 'mamem-inf-')), ...opts })
  const settle = (shard: string) => { chain.mine(1); const l = mem.tailLink(shard); if (l) mem.confirmAnchor(shard, l.txid) }
  return { chain, mem, settle }
}

test('attestation re-derives the shipped proof statement', () => {
  const att = demoAttestation()
  assert.equal(att.publicStatement, '0x' + AIINF_DEMO.publicStatement.toString(16).padStart(64, '0'))
  assert.equal(validateAttestation(att).ok, true)
})

test('non-demo input fails closed — no proof can be minted', () => {
  const bad: number[] = [...AIINF_DEMO.input]; bad[0] = 1
  assert.throws(() => demoAttestation(bad), /no proof exists/)
})

test('tampered attestation fails validation', () => {
  const att = demoAttestation()
  att.output = '4' // wrong argmax — statement no longer re-derives
  assert.equal(validateAttestation(att).ok, false)
})

test('attestInference: off-chain attestation when artifacts absent', async () => {
  const { mem } = setup()
  const r = await callTool(mem, 'attestInference', {})
  assert.equal(r.onchain, false)
  assert.equal(r.attestation.publicStatement.slice(0, 10), '0x5c034d38')
  assert.equal(validateAttestation(r.attestation).ok, true)
  assert.match(r.note ?? '', /artifacts absent/)
})

test('attestInference: non-demo input refused', async () => {
  const { mem } = setup()
  const bad: number[] = [...AIINF_DEMO.input]; bad[5] = 99
  await assert.rejects(() => callTool(mem, 'attestInference', { input: bad }), /no proof exists/)
})

test('attestInference: on-chain when artifacts + funding exist (mock evaluates the 398KB verifier)', { timeout: 120_000 }, async () => {
  delete process.env.MAMEM_GROTH16_OUT
  const chain = new MockChain(); chain.verifyScripts = true
  const f = new MockFunder(); f.topup(5, 100_000)
  for (const t of f.parents) chain.registerParent(t)
  const mem = new MamemMemory(chain, f, f.keyPair, { batchPolicy: { maxLeaves: 2 }, dataDir: mkdtempSync(join(tmpdir(), 'mamem-inf-on-')) })
  const { existsSync } = await import('node:fs')
  const dir = new URL('../../../../tools/groth16-spike/out/', import.meta.url)
  if (!existsSync(new URL('lock-aiinf.hex', dir))) { console.log('  (skipped — artifacts not emitted locally)'); return }
  const r = await callTool(mem, 'attestInference', {})
  assert.equal(r.onchain, true)
  assert.match(r.attestation.verifierTxid ?? '', /^[0-9a-f]{64}$/)
  assert.equal(typeof r.attestation.proofCommit, 'string')
  // the attestation now upgrades verify's model line to consensus-verified
  const w = await callTool(mem, 'remember', { shard: 'facts', data: 'ml ran' })
  await mem.anchorShard('facts')
  const c = await callTool(mem, 'commitReceipt', {
    answer: 'ml ran', fragmentRefs: [{ shard: 'facts', seq: w.seq }], checkResult: 'verified', modelAttestation: r.attestation
  })
  chain.mine(1); const l = mem.tailLink('__receipts__'); if (l) mem.confirmAnchor('__receipts__', l.txid)
  const v = await verifyReceipt(mem, { receiptHash: c.receiptHash })
  const model = v.checks.find(cc => cc.name === 'model')
  assert.equal(model?.ok, true)
  assert.match(model?.detail ?? '', /consensus-checked/)
  process.env.MAMEM_GROTH16_OUT = '/nonexistent-mamem-aiinf'
})

test('commitReceipt embeds a validated attestation; verify shows model check', async () => {
  const { mem, settle } = setup()
  const w = await callTool(mem, 'remember', { shard: 'facts', data: 'the sky is blue' })
  await mem.anchorShard('facts')
  settle('facts')
  const att = demoAttestation()
  const r = await callTool(mem, 'commitReceipt', {
    answer: 'the sky is blue',
    fragmentRefs: [{ shard: 'facts', seq: w.seq }],
    checkResult: 'verified',
    modelAttestation: att
  })
  assert.ok(r.receiptHash)
  settle('__receipts__')
  const v = await verifyReceipt(mem, { receiptHash: r.receiptHash })
  const model = v.checks.find(c => c.name === 'model')
  assert.ok(model, 'model check present')
  assert.equal(model.ok, true)
  assert.match(model.detail, /statement re-derived/)
  assert.match(model.detail, /off-chain attestation only/)
  assert.equal(v.ok, true)
})

test('commitReceipt refuses an attestation whose statement does not re-derive', async () => {
  const { mem } = setup()
  const att = demoAttestation(); att.publicStatement = '0xdeadbeef'
  await assert.rejects(() => callTool(mem, 'commitReceipt', {
    answer: 'x', fragmentRefs: [], checkResult: 'verified', modelAttestation: att
  }), /modelAttestation rejected/)
})

test('a receipt without attestation shows declared model (info)', async () => {
  const { mem, settle } = setup()
  const w = await callTool(mem, 'remember', { shard: 'facts', data: 'plain fact' })
  await mem.anchorShard('facts')
  settle('facts')
  const r = await callTool(mem, 'commitReceipt', {
    answer: 'plain fact', fragmentRefs: [{ shard: 'facts', seq: w.seq }], checkResult: 'verified'
  })
  settle('__receipts__')
  const v = await verifyReceipt(mem, { receiptHash: r.receiptHash })
  const model = v.checks.find(c => c.name === 'model')
  assert.ok(model)
  assert.equal(model.ok, 'info')
  assert.match(model.detail, /declared model only/)
})
