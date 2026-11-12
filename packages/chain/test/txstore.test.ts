// the wire payload a counterparty receives must respect maxBytes on EVERY
// shape — a fat unproven parent chain makes Extended Format just as heavy as
// BEEF, and an oversized 'delivered' envelope dead-letters at the host's
// MaxContent admission bound (live testnet: a 5.9KB EF inside a 12KB payload,
// seller settled while the buyer waited forever)
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, P2PKH, Script, UnlockingScript } from '@bsv/sdk'
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
