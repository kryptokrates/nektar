// tx-proof-cache.test.ts — live-network fetchTx proof attachment.
// On testnet/ttn/main the chain stub has no getTx to carry a BUMP: proofs
// arrive by push (deliverTxProof) or by pull (proofLookup, restart recovery).
// Without the remembered-proof path a sale stays completion_submitted after
// its collect tx is mined — the defect this guards.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, Transaction, P2PKH, MerklePath } from '@bsv/sdk'
import { MockChain, MockFunder } from '@nektar/chain'
import { NektarMemory } from '../src/index.js'

const liveMem = (): { mem: NektarMemory, funder: MockFunder } => {
  const chain = new MockChain()
  delete (chain as { getTx?: unknown }).getTx // live backends have no getTx
  const funder = new MockFunder(); funder.topup(4)
  return { mem: new NektarMemory(chain, funder, funder.keyPair, { network: 'testnet' }), funder }
}

// any transaction — content is irrelevant, only its txid and hex matter
const aTx = async (funder: MockFunder): Promise<Transaction> => {
  const key = PrivateKey.fromRandom()
  const parent = new Transaction()
  parent.addOutput({ satoshis: 10_000, lockingScript: new P2PKH().lock(key.toPublicKey().toAddress()) })
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(key) })
  tx.addOutput({ satoshis: 9_000, lockingScript: new P2PKH().lock(funder.keyPair.toPublicKey().toAddress()) })
  await tx.sign()
  return tx
}

const bumpFor = (txid: string): MerklePath =>
  new MerklePath(1, [[{ offset: 0, hash: txid, txid: true }, { offset: 1, hash: '00'.repeat(32) }]])

test('fetchTx on a live network: pushed proof attaches to a fetched tx', async () => {
  const { mem, funder } = liveMem()
  const tx = await aTx(funder)
  const txid = tx.id('hex')
  ;(mem as unknown as { noteTxHex: (t: string, h: string) => void }).noteTxHex(txid, tx.toHex())
  const before = await mem.fetchTx(txid)
  assert.ok(before !== undefined && before.merklePath === undefined)
  const mp = bumpFor(txid)
  mem.deliverTxProof(txid, mp)
  const after = await mem.fetchTx(txid)
  assert.equal(after?.merklePath, mp)
})

test('fetchTx proofLookup: a pulled path is cached; a miss is not retried inside 60s', async () => {
  const { mem, funder } = liveMem()
  const tx = await aTx(funder)
  const txid = tx.id('hex')
  ;(mem as unknown as { noteTxHex: (t: string, h: string) => void }).noteTxHex(txid, tx.toHex())

  let hits = 0
  const mp = bumpFor(txid)
  mem.proofLookup = async id => { hits++; return id === txid ? mp : undefined }
  assert.equal((await mem.fetchTx(txid))?.merklePath, mp)
  assert.equal(hits, 1)
  await mem.fetchTx(txid) // served from the remembered proof — no second lookup
  assert.equal(hits, 1)

  const txid2 = '22'.repeat(32)
  let misses = 0
  mem.proofLookup = async () => { misses++; return undefined }
  ;(mem as unknown as { noteTxHex: (t: string, h: string) => void }).noteTxHex(txid2, tx.toHex())
  await mem.fetchTx(txid2)
  await mem.fetchTx(txid2) // a negative result is held for the 60s window
  assert.equal(misses, 1)
})
