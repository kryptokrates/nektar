// the wire payload a counterparty receives must respect maxBytes on EVERY
// shape — a fat unproven parent chain makes Extended Format just as heavy as
// BEEF, and an oversized 'delivered' envelope dead-letters at the host's
// MaxContent admission bound (live testnet: a 5.9KB EF inside a 12KB payload,
// seller settled while the buyer waited forever)
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, P2PKH, Script, UnlockingScript, MerklePath } from '@bsv/sdk'
import { TxStore } from '../src/txstore.js'

const key = PrivateKey.fromRandom()
const addr = key.toPublicKey().toAddress()

const coinbase = (fat: number): Transaction => {
  const p = new Transaction()
  p.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromASM('OP_1') })
  // EF embeds the SPENT output — a fat locking script on it is what inflates
  // the wire (padding on sibling outputs never travels)
  p.addOutput({ satoshis: 100_000, lockingScript: fat > 0 ? Script.fromHex('ab'.repeat(fat)) : new P2PKH().lock(addr) })
  return p
}

test('wire: an unproven fat EF falls back to the raw tx', async () => {
  const store = new TxStore()
  const parent = coinbase(3000) // ~3KB of padding — unproven
  const child = new Transaction()
  child.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(key) })
  child.addOutput({ satoshis: 80_000, lockingScript: new P2PKH().lock(addr) })
  await child.sign()
  store.put(child)
  const wire = store.wire(child.id('hex'), 2000)
  assert.equal(wire, child.toHex(), 'EF over maxBytes must degrade to the raw tx')
  assert.ok(Buffer.from(wire!, 'hex').length <= 2000)
  // a roomy budget may still carry ancestry
  const wide = store.wire(child.id('hex'), 100_000)
  assert.notEqual(wide, child.toHex(), 'wide budgets keep the EF shape')
})

// spv/reimport-spent: a stored tx marks the outpoints it consumes — the
// index survives a reload because it derives from the stored raw txs
test('TxStore.spender: indexes spent outpoints and rebuilds them on load', async () => {
  const rows: Array<{ txid: string; raw: string; proof?: string }> = []
  const persist = {
    load: async () => rows,
    put: async (r: { txid: string; raw: string; proof?: string }): Promise<void> => {
      rows.push(r)
    }
  }
  const store = new TxStore(persist)
  const parent = coinbase(0)
  const child = new Transaction()
  child.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(key) })
  child.addOutput({ satoshis: 80_000, lockingScript: new P2PKH().lock(addr) })
  await child.sign()
  store.put(parent)
  const s = store as TxStore & { spender?: (txid: string, vout: number) => string | undefined }
  assert.equal(s.spender?.(parent.id('hex'), 0), undefined, 'nothing recorded spends it yet')
  store.put(child)
  assert.equal(s.spender?.(parent.id('hex'), 0), child.id('hex'))
  assert.equal(s.spender?.(parent.id('hex'), 1), undefined)
  await store.flushed()
  const reloaded = new TxStore(persist)
  await reloaded.load()
  const r2 = reloaded as TxStore & { spender?: (txid: string, vout: number) => string | undefined }
  assert.equal(r2.spender?.(parent.id('hex'), 0), child.id('hex'), 'the index must rebuild from the stored raw txs')
})

// A1: a counterparty-attached merkle path is evidence we must compute — read
// deletes every path that fails verification against OUR tracker (or any path
// when no tracker answers), so put can never adopt an unverified BUMP. A path
// that does verify survives even on an unproven subject — zeroconf ancestry
// keeps traveling.
const pathOn = (tx: Transaction, height: number): MerklePath =>
  new MerklePath(height, [
    [
      { offset: 0, hash: 'aa'.repeat(32) },
      { offset: 1, hash: tx.id('hex'), txid: true }
    ]
  ])
const yesTracker = { isValidRootForHeight: async () => true, currentHeight: async () => 0 }
const noTracker = { isValidRootForHeight: async () => false, currentHeight: async () => 0 }

test('TxStore.read: a forged merkle path is stripped at ingest, never adopted', async () => {
  const parent = coinbase(0)
  const child = new Transaction()
  child.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(key) })
  child.addOutput({ satoshis: 80_000, lockingScript: new P2PKH().lock(addr) })
  await child.sign()
  // the unproven-subject shape: the wire links the carried parent — a forged
  // ancestor BUMP is exactly the adoption vector A1 closed
  parent.merklePath = pathOn(parent, 9)
  const beef = Buffer.from(child.toBEEF()).toString('hex')

  // a tracker that recognizes no root: the carried parent path is deleted
  // before the caller's put() could adopt it
  const { tx } = await TxStore.read(beef, noTracker)
  const src = tx.inputs[0].sourceTransaction
  assert.equal(src?.merklePath, undefined, 'a forged ancestor path is stripped at read')
  const store = new TxStore()
  store.put(tx)
  assert.ok(store.unproven().includes(parent.id('hex')), 'no proof adopted — the observe loop must prove it itself')
  assert.equal(store.get(parent.id('hex'))?.merklePath, undefined)

  // with no tracker at all nothing is verifiable: same strip
  const { tx: blind } = await TxStore.read(beef, undefined)
  assert.equal(blind.inputs[0].sourceTransaction?.merklePath, undefined)
})

test('TxStore.read: verified paths survive the gate — evidence keeps traveling', async () => {
  const parent = coinbase(0)
  const child = new Transaction()
  child.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(key) })
  child.addOutput({ satoshis: 80_000, lockingScript: new P2PKH().lock(addr) })
  await child.sign()
  parent.merklePath = pathOn(parent, 9)
  const beef = Buffer.from(child.toBEEF()).toString('hex')
  const { tx } = await TxStore.read(beef, yesTracker)
  assert.equal(tx.inputs[0].sourceTransaction?.merklePath?.blockHeight, 9, 'a valid ancestor path keeps traveling')
})

// a reorg horizon drops only the proofs at/above it — survivors keep theirs
test('TxStore.clearProofsFrom: proofs at/above the horizon drop, older keep', () => {
  const store = new TxStore()
  const mk = (vout: number): Transaction => {
    const t = new Transaction()
    t.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: vout, unlockingScript: new UnlockingScript() })
    t.addOutput({ satoshis: 1_000, lockingScript: new P2PKH().lock(addr) })
    return t
  }
  const t1 = mk(0),
    t2 = mk(1)
  store.put(t1)
  store.put(t2)
  store.setProof(t1.id('hex'), { blockHeight: 10 } as import('@bsv/sdk').MerklePath)
  store.setProof(t2.id('hex'), { blockHeight: 20 } as import('@bsv/sdk').MerklePath)
  assert.deepEqual(store.clearProofsFrom(15), [t2.id('hex')], 'only the ≥horizon proof is reported')
  assert.ok(store.unproven().includes(t2.id('hex')), 'the cleared tx is unproven again — the observe loop re-proves it')
  assert.ok(!store.unproven().includes(t1.id('hex')), 'a proof below the horizon is untouched')
  assert.equal(store.get(t1.id('hex'))?.merklePath?.blockHeight, 10)
})
