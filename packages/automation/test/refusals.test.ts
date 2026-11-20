// refusals.test.ts — the refused-ancestor registry: which wire shapes
// mark (real broadcaster refusal formats), which must not (transient
// holds), mark TTL, and the transitive-mark behavior that makes a hit
// dead for its descendants.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, P2PKH, Transaction, UnlockingScript, MerklePath } from '@bsv/sdk'
import { openRefusals, REFUSED_MARK_MS } from '../src/refusals.js'

const key = PrivateKey.fromRandom()
const lock = () => new P2PKH().lock(key.toPublicKey().toAddress())

const parentOf = (ancestor: Transaction): Transaction => {
  const t = new Transaction()
  t.addInput({ sourceTXID: ancestor.id('hex'), sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromASM('OP_1') })
  t.addOutput({ satoshis: 1_000, lockingScript: lock() })
  return t
}

test('refusals: only refusal shapes that name a dead ancestor mark', () => {
  const r = openRefusals({ fetchTx: async () => undefined })
  const dead = 'ab'.repeat(32)
  // Teranode's ERR_TX_CONFLICTING wire text — a double-spend-loser parent
  r.noteRefusal(`utxo ${dead}:0 belongs to a conflicting transaction`)
  assert.equal(r.refused(dead), true, 'Teranode conflicting-parent marks')
  // bitcoind/ARC wording for the same verdict
  const other = 'cd'.repeat(32)
  r.noteRefusal(`bad-txns-spends-conflicting-tx, ${other}`)
  assert.equal(r.refused(other), true, 'bitcoind conflicting-parent marks')
  // the literal 'ancestor' phrasing kept for unobserved broadcaster formats
  const third = 'ef'.repeat(32)
  r.noteRefusal(`parent rejected (ancestor ${third}): retryable`)
  assert.equal(r.refused(third), true, 'ancestor phrasing still marks')

  // transient holds must never mark: missing, locked, frozen, non-final
  const holds = 'aa'.repeat(32)
  for (const d of [
    'TX rejected: Missing inputs',
    `utxo ${holds}:0 belongs to a locked transaction`,
    `utxo ${holds}:0 is frozen`,
    `utxo ${holds}:1 is held until height 900 (current 800)`,
    'tx is non-final: lockTime 2100 not reached',
    `coinbase ${holds}:0 not spendable until height 900 (current 800)`
  ]) {
    r.noteRefusal(d)
  }
  assert.equal(r.refused(holds), false, 'missing/locked/frozen/non-final stay unmarked')
  r.noteRefusal(undefined)
})

test('refusals: a mark expires with the refusal epoch', () => {
  let now = 1_000_000
  const r = openRefusals({ fetchTx: async () => undefined, now: () => now })
  const dead = 'ab'.repeat(32)
  r.noteRefusal(`utxo ${dead}:0 belongs to a conflicting transaction`)
  now += REFUSED_MARK_MS - 1
  assert.equal(r.refused(dead), true, 'live inside the epoch')
  now += 2
  assert.equal(r.refused(dead), false, 'expired after the epoch')
})

test('refusals: parent() marks the subject transitively — with the evidence timestamp', async () => {
  let now = 1_000_000
  const dead = new Transaction()
  dead.addOutput({ satoshis: 2_000, lockingScript: lock() })
  const mid = parentOf(dead)
  const subject = parentOf(mid)
  const byId = new Map([
    [mid.id('hex'), mid],
    [dead.id('hex'), dead]
  ])
  const r2 = openRefusals({ fetchTx: async (id) => byId.get(id), now: () => now })
  r2.noteRefusal(`utxo ${dead.id('hex')}:0 belongs to a conflicting transaction`)

  now += 20 * 60_000 // twenty minutes into the mark's epoch
  assert.equal(await r2.parent(subject), true, 'a refused grandparent fails the subject')
  assert.equal(r2.refused(subject.id('hex')), true, 'the hit marks the subject for its own descendants')
  // the derived mark inherited the EVIDENCE's timestamp — it dies when the
  // refusal dies, not a fresh epoch after the walk that derived it
  now += 11 * 60_000
  assert.equal(r2.refused(subject.id('hex')), false, 'the derived mark dies with its evidence')
})

test('refusals: parent() — direct mark short-circuits; proven and clean pass', async () => {
  const r = openRefusals({ fetchTx: async () => undefined })
  const dead = new Transaction()
  dead.addOutput({ satoshis: 2_000, lockingScript: lock() })
  r.noteRefusal(`utxo ${dead.id('hex')}:0 belongs to a conflicting transaction`)
  assert.equal(await r.parent(dead), true, 'a directly-refused parent hits without a walk')

  const clean = parentOf(dead)
  const noResolve = openRefusals({ fetchTx: async () => undefined })
  assert.equal(await noResolve.parent(clean), false, 'nothing marked, nothing dead')

  const proven = parentOf(dead)
  proven.merklePath = new MerklePath(900, [[{ offset: 0 }]])
  assert.equal(await r.parent(proven), false, 'a proven subject is never a refused ancestor')
})
