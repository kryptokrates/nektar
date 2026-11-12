// Model-execution attestation (nChain ai_inference Groth16): the statement
// poseidon(input‖output‖poseidon(model)) re-derives in pure TS; receipts can
// embed a validated attestation; verify surfaces a three-state model check —
// declared → statement-attested → consensus-verified (verifierTxid).
// Only the canned demo vector has a proof today (dev trusted setup) — any
// other (input,output) is refused before it can enter a receipt.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { stackWorld, stackChain, StackFunder } from '@nektar/chain'
import { NektarMemory, type NektarMemoryOptions } from '../src/memory.js'
import { callTool } from '../src/server.js'
import { verifyReceipt } from '../src/receipt.js'
import { AIINF_DEMO, demoAttestation, validateAttestation } from '@nektar/zk'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

// point the artifact lookup somewhere empty — on-chain spend is e2e's job
process.env.NEKTAR_GROTH16_OUT = '/nonexistent-nektar-aiinf'

async function setup(t: { skip: (m?: string) => void }, opts: NektarMemoryOptions = {}) {
  const w = await stackWorld(t, 20)
  if (w === undefined) return
  const { chain: chain, funder: funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, dataDir: mkdtempSync(join(tmpdir(), 'nektar-inf-')), ...opts })
  const settle = async (shard: string) => {
    await chain.mine(1)
    const l = mem.tailLink(shard)
    if (l) mem.confirmAnchor(shard, l.txid)
  }
  return { chain, mem, settle }
}

test('attestation re-derives the shipped proof statement', () => {
  const att = demoAttestation()
  assert.equal(att.publicStatement, '0x' + AIINF_DEMO.publicStatement.toString(16).padStart(64, '0'))
  assert.equal(validateAttestation(att).ok, true)
})

test('non-demo input fails closed — no proof can be minted', () => {
  const bad: number[] = [...AIINF_DEMO.input]
  bad[0] = 1
  assert.throws(() => demoAttestation(bad), /no proof exists/)
})

test('tampered attestation fails validation', () => {
  const att = demoAttestation()
  att.output = '4' // wrong argmax — statement no longer re-derives
  assert.equal(validateAttestation(att).ok, false)
})

test('attestInference: off-chain attestation when artifacts absent', async (t) => {
  const _w = await setup(t)
  if (_w === undefined) return
  const { mem } = _w
  const r = await callTool(mem, 'attestInference', {})
  assert.equal(r.onchain, false)
  assert.equal(r.attestation.publicStatement.slice(0, 10), '0x5c034d38')
  assert.equal(validateAttestation(r.attestation).ok, true)
  assert.match(r.note ?? '', /artifacts absent/)
})

test('attestInference: non-demo input refused', async (t) => {
  const _w = await setup(t)
  if (_w === undefined) return
  const { mem } = _w
  const bad: number[] = [...AIINF_DEMO.input]
  bad[5] = 99
  await assert.rejects(() => callTool(mem, 'attestInference', { input: bad }), /no proof exists/)
})

test('attestInference: DEFAULT path anchors sha256(proof) in one small tx', { timeout: 30_000 }, async (t) => {
  delete process.env.NEKTAR_GROTH16_OUT
  const _w = await setup(t)
  if (_w === undefined) return
  const { mem } = _w
  const { existsSync } = await import('node:fs')
  const dir = new URL('../../../../tools/groth16-spike/out/', import.meta.url)
  if (!existsSync(new URL('lock-aiinf.hex', dir))) {
    process.env.NEKTAR_GROTH16_OUT = '/nonexistent-nektar-aiinf'
    console.log('  (skipped — artifacts not emitted locally)')
    return
  }
  const r = await callTool(mem, 'attestInference', {})
  assert.equal(r.onchain, false)
  assert.equal(r.anchored, true)
  assert.match(r.attestation.attestationTxid ?? '', /^[0-9a-f]{64}$/)
  assert.match(r.attestation.commitment ?? '', /^[0-9a-f]{64}$/)
  assert.equal(r.attestation.commitment, r.attestation.proofCommit)
  // the proof bytes are fetchable + the anchored commitment binds them
  const bytes = mem.getProof(r.attestation.proofCommit)
  assert.ok(bytes && bytes.length > 30_000, 'proof bytes in the store')
  const va = await mem.verifyAttestation(r.attestation.attestationTxid, bytes)
  assert.equal(va.ok, true)
  process.env.NEKTAR_GROTH16_OUT = '/nonexistent-nektar-aiinf'
})

test('anchored attestation verifies end-to-end in a receipt', { timeout: 30_000 }, async (t) => {
  delete process.env.NEKTAR_GROTH16_OUT
  const _w = await setup(t)
  if (_w === undefined) return
  const { mem, settle } = _w
  const { existsSync } = await import('node:fs')
  const dir = new URL('../../../../tools/groth16-spike/out/', import.meta.url)
  if (!existsSync(new URL('lock-aiinf.hex', dir))) {
    process.env.NEKTAR_GROTH16_OUT = '/nonexistent-nektar-aiinf'
    console.log('  (skipped — artifacts not emitted locally)')
    return
  }
  const a = await callTool(mem, 'attestInference', {})
  const w = await callTool(mem, 'remember', { shard: 'facts', data: 'ml ran' })
  await mem.anchorShard('facts')
  const c = await callTool(mem, 'commitReceipt', {
    answer: 'ml ran',
    fragmentRefs: [{ shard: 'facts', seq: w.seq }],
    checkResult: 'verified',
    modelAttestation: a.attestation
  })
  settle('__receipts__')
  const v = await verifyReceipt(mem, { receiptHash: c.receiptHash })
  const model = v.checks.find((cc) => cc.name === 'model')
  assert.equal(model?.ok, true)
  assert.match(model?.detail ?? '', /attested\+anchored/)
  // a receipt whose anchored commitment does NOT bind the bytes fails closed
  const c2 = await callTool(mem, 'commitReceipt', {
    answer: 'ml ran 2',
    fragmentRefs: [{ shard: 'facts', seq: w.seq }],
    checkResult: 'verified',
    modelAttestation: { ...a.attestation, attestationTxid: 'f'.repeat(64) }
  })
  const v2 = await verifyReceipt(mem, { receiptHash: c2.receiptHash })
  assert.equal(v2.checks.find((cc) => cc.name === 'model')?.ok, false)
  process.env.NEKTAR_GROTH16_OUT = '/nonexistent-nektar-aiinf'
})

test('commitReceipt auto-attests in one call; exclusive with modelAttestation', async (t) => {
  const _w = await setup(t)
  if (_w === undefined) return
  const { mem, settle } = _w
  const w = await callTool(mem, 'remember', { shard: 'facts', data: 'auto attest' })
  await mem.anchorShard('facts')
  // artifacts absent under the /nonexistent dir → statement-verified attestation still embeds
  const r = await callTool(mem, 'commitReceipt', {
    answer: 'auto attest',
    fragmentRefs: [{ shard: 'facts', seq: w.seq }],
    checkResult: 'verified',
    attestInference: {}
  })
  settle('__receipts__')
  const v = await verifyReceipt(mem, { receiptHash: r.receiptHash })
  const model = v.checks.find((c) => c.name === 'model')
  assert.equal(model?.ok, true)
  assert.match(model?.detail ?? '', /statement re-derived/)
  await assert.rejects(
    () =>
      callTool(mem, 'commitReceipt', {
        answer: 'x',
        fragmentRefs: [],
        checkResult: 'verified',
        attestInference: {},
        modelAttestation: demoAttestation()
      }),
    /not both/
  )
})

test('attestInference: on-chain when artifacts + funding exist (mock evaluates the 398KB verifier)', { timeout: 120_000 }, async (t) => {
  delete process.env.NEKTAR_GROTH16_OUT
  const w = await stackWorld(t, 5, 100_000)
  if (w === undefined) return
  const { chain: chain, funder: f } = w
  const mem = new NektarMemory(chain, f, f.keyPair, { batchPolicy: { maxLeaves: 2 }, dataDir: mkdtempSync(join(tmpdir(), 'nektar-inf-on-')) })
  const { existsSync } = await import('node:fs')
  const dir = new URL('../../../../tools/groth16-spike/out/', import.meta.url)
  if (!existsSync(new URL('lock-aiinf.hex', dir))) {
    console.log('  (skipped — artifacts not emitted locally)')
    return
  }
  const r = await callTool(mem, 'attestInference', { onchain: true })
  assert.equal(r.onchain, true)
  assert.match(r.attestation.verifierTxid ?? '', /^[0-9a-f]{64}$/)
  assert.equal(typeof r.attestation.proofCommit, 'string')
  // the attestation now upgrades verify's model line to consensus-verified
  const w = await callTool(mem, 'remember', { shard: 'facts', data: 'ml ran' })
  await mem.anchorShard('facts')
  const c = await callTool(mem, 'commitReceipt', {
    answer: 'ml ran',
    fragmentRefs: [{ shard: 'facts', seq: w.seq }],
    checkResult: 'verified',
    modelAttestation: r.attestation
  })
  await chain.mine(1)
  const l = mem.tailLink('__receipts__')
  if (l) mem.confirmAnchor('__receipts__', l.txid)
  const v = await verifyReceipt(mem, { receiptHash: c.receiptHash })
  const model = v.checks.find((cc) => cc.name === 'model')
  assert.equal(model?.ok, true)
  assert.match(model?.detail ?? '', /consensus-checked/)
  process.env.NEKTAR_GROTH16_OUT = '/nonexistent-nektar-aiinf'
})

test('commitReceipt embeds a validated attestation; verify shows model check', async (t) => {
  const _w = await setup(t)
  if (_w === undefined) return
  const { mem, settle } = _w
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
  const model = v.checks.find((c) => c.name === 'model')
  assert.ok(model, 'model check present')
  assert.equal(model.ok, true)
  assert.match(model.detail, /statement re-derived/)
  assert.match(model.detail, /statement-verified only/)
  assert.equal(v.ok, true)
})

test('commitReceipt refuses an attestation whose statement does not re-derive', async (t) => {
  const _w = await setup(t)
  if (_w === undefined) return
  const { mem } = _w
  const att = demoAttestation()
  att.publicStatement = '0xdeadbeef'
  await assert.rejects(
    () =>
      callTool(mem, 'commitReceipt', {
        answer: 'x',
        fragmentRefs: [],
        checkResult: 'verified',
        modelAttestation: att
      }),
    /modelAttestation rejected/
  )
})

test('a receipt without attestation shows declared model (info)', async (t) => {
  const _w = await setup(t)
  if (_w === undefined) return
  const { mem, settle } = _w
  const w = await callTool(mem, 'remember', { shard: 'facts', data: 'plain fact' })
  await mem.anchorShard('facts')
  settle('facts')
  const r = await callTool(mem, 'commitReceipt', {
    answer: 'plain fact',
    fragmentRefs: [{ shard: 'facts', seq: w.seq }],
    checkResult: 'verified'
  })
  settle('__receipts__')
  const v = await verifyReceipt(mem, { receiptHash: r.receiptHash })
  const model = v.checks.find((c) => c.name === 'model')
  assert.ok(model)
  assert.equal(model.ok, 'info')
  assert.match(model.detail, /declared model only/)
})
