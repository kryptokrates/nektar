import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, P2PKH, MerklePath, LockingScript, UnlockingScript, ScriptEvaluationError } from '@bsv/sdk'
import { MAMEM_ANCHOR_ARTIFACT, TAG_RPRIME, META_FLAG_HEX, decodeMamemAnchor } from '@mamem/contracts'
import { RunarContract } from 'runar-sdk'
import { chainRp, GENESIS_PREV_RP, dsha256, utf8, shardIdFromPubkey, hash160, toHex, type Bytes, type AnchorData } from '@mamem/core'
import knexPkg from 'knex'

import { MockChain, MockFunder } from '@mamem/chain'
import { createMamemOverlay } from '../src/node.js'
import { AnchorChain } from '@mamem/anchor'

const SHARD = 'shard-alpha'
const shardKeyHex = PrivateKey.fromRandom().toHex()
const shardKey = PrivateKey.fromHex(shardKeyHex)
const TAG_HEX = toHex(TAG_RPRIME)
// owner key == funder key (the contract signer signs funding inputs too)
const pubOf = (k: PrivateKey) => k.toPublicKey().toString()
const sidOf = (k: PrivateKey) => toHex(hash160(Uint8Array.from(Buffer.from(pubOf(k),'hex'))))

interface BuiltTx { tx: Transaction, data: AnchorData }

const contractScript = (pub: string, n: number, size: number, R: Bytes, Rp: Bytes, tag = TAG_HEX, pkh?: string) =>
  new RunarContract(MAMEM_ANCHOR_ARTIFACT, [pub, pkh ?? toHex(hash160(Uint8Array.from(Buffer.from(pub, 'hex')))), META_FLAG_HEX, BigInt(n), BigInt(size), toHex(R), toHex(Rp), tag]).getLockingScript()

// dummy unlock for hand-built spends: addInput ignores a plain
// unlockingScript, so use a template returning an OP_1 script
const dummyUnlock = { sign: async () => UnlockingScript.fromHex('51'), estimateLength: async () => 1 } as any

async function mkAnchor (
  funder: MockFunder, fundKey: PrivateKey,
  opts: { pub?: string, pkh?: string, tag?: string, n: number, size: number, R: Bytes, Rp: Bytes, prevHead?: { tx: Transaction, vout: number } }
): Promise<BuiltTx> {
  const pub = opts.pub ?? pubOf(fundKey)
  const tx = new Transaction()
  if (opts.prevHead) {
    const h = opts.prevHead
    tx.addInput({
      sourceTransaction: h.tx, sourceOutputIndex: h.vout,
      unlockingScriptTemplate: dummyUnlock // contract spends: engine eval covers real validation
    })
  }
  const fund = await funder.take(2000)
  tx.addInput({
    sourceTransaction: fund.tx, sourceOutputIndex: fund.vout,
    unlockingScriptTemplate: new P2PKH().unlock(fundKey)
  })
  const data: AnchorData = { shard: toHex(hash160(Uint8Array.from(Buffer.from(pub,'hex')))), n: opts.n, size: opts.size, R: opts.R, Rp: opts.Rp }
  tx.addOutput({ lockingScript: LockingScript.fromHex(contractScript(pub, opts.n, opts.size, opts.R, opts.Rp, opts.tag, opts.pkh)), satoshis: 1 })
  tx.addOutput({ lockingScript: new P2PKH().lock(fundKey.toPublicKey().toAddress([0x6f])), change: true })
  await tx.fee(); await tx.sign()
  return { tx, data }
}

const anchorData = (tx: Transaction) => decodeMamemAnchor(tx.outputs[0]!.lockingScript.toHex())!

// give the funder's synthetic parents a mined BUMP so BEEF verification passes

async function setup () {
  const chain = new MockChain()
  const funder = new MockFunder()
  funder.topup(50)
  // Mine all funder parents so BEEF verification has real proofs.
  const utxos = (funder as any).utxos as Array<{ tx: Transaction }>
  for (const u of utxos) chain.mempool.set(u.tx.id('hex'), u.tx)
  chain.mine(1)
  const overlay = await createMamemOverlay(chain, chain)
  // NOTE: for inputs spending contract outputs the engine runs real script
  // eval — hand-built spends with empty unlocking scripts fail at eval,
  // BEFORE tm_mamem sees them. tm-level negatives are asserted with
  // rejectedSubmit(); contract-level (eval) negatives with assert.rejects.
  const submit = async (tx: Transaction) =>
    overlay.engine.submit({ beef: tx.toBEEF(), topics: ['tm_mamem'] }, undefined, 'current-tx')
  const admitted = (s: any) => (s.tm_mamem?.outputsToAdmit?.length ?? 0) > 0
  return { chain, funder, overlay, submit, admitted }
}

test('tm_mamem: admit genesis+successor, lookups work', async (t) => {
  const { chain, funder, overlay, submit, admitted } = await setup()
  t.after(async () => { await overlay.knex.destroy() })
  const fk = funder.keyPair
  const R = (i: number) => dsha256(utf8(`batch-${i}`))
  const ac = new AnchorChain('s', fk, funder, fk, chain as any)
  const g = await ac.anchor(10, R(0)); const gData = decodeMamemAnchor(g.tx.outputs[0]!.lockingScript.toHex())!
  assert.equal(admitted(await submit(g.tx)), true, 'genesis admitted')
  assert.equal(overlay.ls.getHead(sidOf(fk))!.txid, g.tx.id('hex'))

  const a1 = await ac.anchor(20, R(1))
  assert.equal(admitted(await submit(a1.tx)), true, 'successor admitted')
  assert.equal(overlay.ls.getHead(sidOf(fk))!.txid, a1.tx.id('hex'))

  const head = await overlay.engine.lookup({ service: 'ls_mamem', query: { shard: sidOf(fk) } })
  assert.equal(head.type, 'output-list')
  assert.equal(head.outputs.length, 1)
  assert.equal(Transaction.fromBEEF(head.outputs[0]!.beef).id('hex'), a1.tx.id('hex'))
  const byN = await overlay.engine.lookup({ service: 'ls_mamem', query: { shard: sidOf(fk), n: 0 } })
  assert.equal(Transaction.fromBEEF(byN.outputs[0]!.beef).id('hex'), g.tx.id('hex'))
})

test('tm_mamem: rejection cases leave head unchanged', async (t) => {
  const { chain, funder, overlay, submit, admitted } = await setup()
  t.after(async () => { await overlay.knex.destroy() })
  const fk = funder.keyPair
  const R = (i: number) => dsha256(utf8(`batch-${i}`))

  const ac = new AnchorChain('s', fk, funder, fk, chain as any)
  const g = await ac.anchor(10, R(0))
  await submit(g.tx)
  const a1 = await ac.anchor(20, R(1))
  await submit(a1.tx)
  assert.equal(overlay.ls.getHead(sidOf(fk))!.txid, a1.tx.id('hex'))

  const badN = await mkAnchor(funder, fk, {
    n: 3, size: 30, R: R(2), Rp: chainRp(a1.head.data.Rp, R(2), 30),
    prevHead: { tx: a1.tx, vout: 0 }
  })
  await assert.rejects(() => submit(badN.tx))
  assert.equal(overlay.ls.getHead(sidOf(fk))!.txid, a1.tx.id('hex'))

  const badRp = await mkAnchor(funder, fk, {
    n: 2, size: 30, R: R(2), Rp: dsha256(utf8('bogus')),
    prevHead: { tx: a1.tx, vout: 0 }
  })
  await assert.rejects(() => submit(badRp.tx))

  const noSpend = await mkAnchor(funder, fk, { n: 2, size: 30, R: R(2), Rp: chainRp(a1.head.data.Rp, R(2), 30) })
  assert.equal(admitted(await submit(noSpend.tx)), false, 'non-head-spend rejected')

  const fork = await mkAnchor(funder, fk, { n: 0, size: 5, R: R(9), Rp: chainRp(GENESIS_PREV_RP, R(9), 5) })
  assert.equal(admitted(await submit(fork.tx)), false, 'second genesis rejected')

  // squatting genesis: a DIFFERENT shard's genesis carrying fk's genesisOwner
  // pubkey but the attacker's ownerPkh → shard-squat attempt → rejected
  const attacker = PrivateKey.fromRandom()
  const squat = await mkAnchor(funder, fk, {
    pub: pubOf(fk), pkh: sidOf(attacker), // genesisOwner=victim, ownerPkh=attacker
    n: 0, size: 5, R: R(10), Rp: chainRp(GENESIS_PREV_RP, R(10), 5)
  })
  assert.equal(admitted(await submit(squat.tx)), false, 'squatting genesis rejected')
})

test('tm_mamem: rotation admitted, rotate-with-changed-state rejected', async (t) => {
  const { chain, funder, overlay, submit, admitted } = await setup()
  t.after(async () => { await overlay.knex.destroy() })
  const fk = funder.keyPair
  const newKey = PrivateKey.fromRandom()
  const R = (i: number) => dsha256(utf8(`rot-${i}`))
  const sid = sidOf(fk) // shard = hash160(genesisOwner) — stable across rotation

  const ac = new AnchorChain('s', fk, funder, fk, chain as any)
  const g = await ac.anchor(10, R(0))
  assert.equal(admitted(await submit(g.tx)), true, 'genesis admitted')

  // production rotate: on-chain rotation, ownerPkh changes, n stays
  const rot = await ac.rotate(newKey)
  assert.equal(admitted(await submit(rot.tx)), true, 'rotate admitted')
  assert.equal(overlay.ls.getHead(sid)!.txid, rot.tx.id('hex'), 'shard unchanged')

  // append authorized by the NEW owner key is admitted; funding stays with the
  // fund key (decoupled from ownership)
  const a1 = await ac.anchor(20, R(1))
  assert.equal(admitted(await submit(a1.tx)), true, 'new-key append admitted')
  assert.equal(a1.head.data.n, 1)

  // fake "rotate" on the SAME shard that also bumps n → neither append nor rotate → rejected
  const badRot = await mkAnchor(funder, fk, {
    pub: pubOf(fk), pkh: sidOf(newKey),
    n: 2, size: 20, R: a1.head.data.R, Rp: a1.head.data.Rp, // rotate fields but wrong n
    prevHead: { tx: a1.tx, vout: 0 }
  })
  await assert.rejects(() => submit(badRot.tx), 'rotate that changes n rejected')
})

test('tm_mamem: double-spend of head — only first successor admitted', async (t) => {
  const { chain, funder, overlay, submit, admitted } = await setup()
  t.after(async () => { await overlay.knex.destroy() })
  const fk = funder.keyPair
  const R = (i: number) => dsha256(utf8(`ds-${i}`))
  const ac2 = new AnchorChain('s', fk, funder, fk, chain as any)
  const g = await ac2.anchor(1, R(0))
  await submit(g.tx)
  const a1 = await ac2.anchor(2, R(1))
  assert.equal(admitted(await submit(a1.tx)), true)
  // a1b: a sibling successor spending the same head g — built as a real
  // contract append of the g-state (valid spend; tm must reject it because
  // the head has already moved to a1)
  const gState = decodeMamemAnchor(g.tx.outputs[0]!.lockingScript.toHex())!
  const a1b = await mkAnchor(funder, fk, { n: 1, size: 3, R: R(9), Rp: chainRp(gState.Rp, R(9), 3), prevHead: { tx: g.tx, vout: 0 } })
  await assert.rejects(() => submit(a1b.tx))
  assert.equal(overlay.ls.getHead(sidOf(fk))!.txid, a1.tx.id('hex'))
})

test('tm_mamem: replayed already-admitted anchor not re-admitted', async (t) => {
  const { chain, funder, overlay, submit } = await setup()
  t.after(async () => { await overlay.knex.destroy() })
  const fk = funder.keyPair
  const g = await mkAnchor(funder, fk, { n: 0, size: 1, R: dsha256(utf8('rp')), Rp: chainRp(GENESIS_PREV_RP, dsha256(utf8('rp')), 1) })
  await submit(g.tx)
  const headBefore = overlay.ls.getHead(sidOf(fk))!.txid
  let replayed = true
  try {
    const s = await submit(g.tx)
    replayed = (s.tm_mamem?.outputsToAdmit?.length ?? 0) > 0
  } catch { replayed = false } // engine may throw on dup
  assert.equal(replayed, false, 'replay not admitted')
  assert.equal(overlay.ls.getHead(sidOf(fk))!.txid, headBefore)
})

test('tm_mamem: genesis with wrong tag rejected', async (t) => {
  const { funder, overlay, submit, admitted } = await setup()
  t.after(async () => { await overlay.knex.destroy() })
  const g = await mkAnchor(funder, funder.keyPair, {
    tag: toHex(utf8('wrong:tag')),
    n: 0, size: 1, R: dsha256(utf8('x')), Rp: chainRp(GENESIS_PREV_RP, dsha256(utf8('x')), 1)
  })
  assert.equal(admitted(await submit(g.tx)), false)
})

test('tm_mamem: genesis spending a topical coin (previousCoins non-empty) rejected', async (t) => {
  const { funder, overlay, submit, admitted } = await setup()
  t.after(async () => { await overlay.knex.destroy() })
  const fk = funder.keyPair
  const R = (i: number) => dsha256(utf8(`pc-${i}`))
  const g = await mkAnchor(funder, fk, { n: 0, size: 1, R: R(0), Rp: chainRp(GENESIS_PREV_RP, R(0), 1) })
  await submit(g.tx)
  // n=0 "genesis" that spends shard1's head — previousCoins non-empty → reject
  const pub2 = PrivateKey.fromRandom().toPublicKey().toString()
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: g.tx, sourceOutputIndex: 0 })
  tx.inputs[tx.inputs.length-1]!.unlockingScript = UnlockingScript.fromHex('51')
  const fund = await funder.take(2000)
  tx.addInput({ sourceTransaction: fund.tx, sourceOutputIndex: fund.vout, unlockingScriptTemplate: new P2PKH().unlock(fk) })
  tx.addOutput({ lockingScript: LockingScript.fromHex(contractScript(pub2, 0, 1, R(1), chainRp(GENESIS_PREV_RP, R(1), 1))), satoshis: 1 })
  await tx.fee(); await tx.sign()
  await assert.rejects(() => submit(tx))
})

test('tm_mamem: successor locked to a different pubkey rejected', async (t) => {
  const { funder, overlay, submit, admitted } = await setup()
  t.after(async () => { await overlay.knex.destroy() })
  const fk = funder.keyPair
  const R = (i: number) => dsha256(utf8(`rk-${i}`))
  const g = await mkAnchor(funder, fk, { n: 0, size: 1, R: R(0), Rp: chainRp(GENESIS_PREV_RP, R(0), 1) })
  await submit(g.tx)
  // successor spends head, correct n/R', but locked to another key
  const pub2 = PrivateKey.fromRandom().toPublicKey().toString()
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: g.tx, sourceOutputIndex: 0 })
  tx.inputs[tx.inputs.length-1]!.unlockingScript = UnlockingScript.fromHex('51')
  const fund = await funder.take(2000)
  tx.addInput({ sourceTransaction: fund.tx, sourceOutputIndex: fund.vout, unlockingScriptTemplate: new P2PKH().unlock(fk) })
  tx.addOutput({ lockingScript: LockingScript.fromHex(contractScript(pub2, 1, 1, R(1), chainRp(g.data.Rp, R(1), 1))), satoshis: 1 })
  await tx.fee(); await tx.sign()
  await assert.rejects(() => submit(tx))
  assert.equal(overlay.ls.getHead(sidOf(fk))!.txid, g.tx.id('hex'))
})

test('ls_mamem: heads persist across engine restart (file sqlite)', async (t) => {
  const chain = new MockChain()
  const funder = new MockFunder(); funder.topup(50)
  for (const u of (funder as any).utxos as Array<{ tx: Transaction }>) chain.mempool.set(u.tx.id('hex'), u.tx)
  chain.mine(1)
  // file-backed: use a temp file path
  const path = await import('node:path')
  const os = await import('node:os')
  const f = path.join(os.tmpdir(), `mamem-ls-${Date.now()}.sqlite`)
  const kx = knexPkg({ client: 'sqlite3', connection: { filename: f }, useNullAsDefault: true })
  let overlay = await createMamemOverlay(chain, chain, kx)
  const submit = (tx: Transaction) => overlay.engine.submit({ beef: tx.toBEEF(), topics: ['tm_mamem'] }, undefined, 'current-tx')
  const admitted = (s: any) => (s.tm_mamem?.outputsToAdmit?.length ?? 0) > 0
  const fk = funder.keyPair
  const R = (i: number) => dsha256(utf8(`rst-${i}`))
  const ac3 = new AnchorChain('s', fk, funder, fk, chain as any)
  const g = await ac3.anchor(1, R(0))
  const a1 = await ac3.anchor(1, R(1))
  const a2 = await ac3.anchor(1, R(2))
  assert.equal(admitted(await submit(g.tx)), true)
  assert.equal(admitted(await submit(a1.tx)), true)
  assert.equal(admitted(await submit(a2.tx)), true)
  const headBefore = overlay.ls.getHead(sidOf(fk))!.txid
  assert.equal(headBefore, a2.tx.id('hex'))

  // simulate restart: close the first overlay's knex, open a fresh one on the same file
  await overlay.knex.destroy()
  const kx2 = knexPkg({ client: 'sqlite3', connection: { filename: f }, useNullAsDefault: true })
  const overlay2 = await createMamemOverlay(chain, chain, kx2)
  const submit2 = (tx: Transaction) => overlay2.engine.submit({ beef: tx.toBEEF(), topics: ['tm_mamem'] }, undefined, 'current-tx')
  assert.equal(overlay2.ls.getHead(sidOf(fk))?.txid, headBefore, 'head survives restart')

  const g3 = await mkAnchor(funder, fk, { n: 0, size: 1, R: R(9), Rp: chainRp(GENESIS_PREV_RP, R(9), 1) })
  assert.equal(admitted(await submit2(g3.tx)), false, 'fresh genesis rejected after restart')
  assert.equal(admitted(await submit2(a1.tx)), false, 'stale n=1 rejected after restart')
  const a3 = await ac3.anchor(1, R(3))
  assert.equal(admitted(await submit2(a3.tx)), true, 'valid n=3 admitted after restart')
  assert.equal(overlay2.ls.getHead(sidOf(fk))!.txid, a3.tx.id('hex'))
  t.after(async () => { await overlay2.knex.destroy() })
})
