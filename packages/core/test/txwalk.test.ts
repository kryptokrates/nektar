import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, P2PKH, Transaction } from '@bsv/sdk'
import { ancestorsOf, fullSourceTx, inputPrevout, isEfStub } from '../src/txwalk.js'

void test('txwalk: EF stand-in stubs are recognized safely and skipped by ancestorsOf', async () => {
  const k = PrivateKey.fromRandom()
  const addr = k.toPublicKey().toAddress()
  const parent = new Transaction()
  parent.addOutput({ satoshis: 1000, lockingScript: new P2PKH().lock(addr) })
  parent.addOutput({ satoshis: 2000, lockingScript: new P2PKH().lock(addr) })
  const child = new Transaction()
  child.addInput({ sourceTransaction: parent, sourceOutputIndex: 1, unlockingScriptTemplate: new P2PKH().unlock(k) })
  child.addOutput({ satoshis: 1500, lockingScript: new P2PKH().lock(addr) })
  await child.sign()

  // full linked tx: parent is real ancestry
  assert.equal(isEfStub(parent), false)
  assert.equal(fullSourceTx(child.inputs[0])?.id('hex'), parent.id('hex'))
  assert.deepEqual(inputPrevout(child.inputs[0]), { txid: parent.id('hex'), vout: 1 })
  assert.deepEqual(
    ancestorsOf(child).map((t) => t.id('hex')),
    [parent.id('hex')]
  )

  // Extended Format round-trip: sourceTransaction is an EF stub (null-filled outputs, 0 inputs)
  const ef = Transaction.fromHexEF(child.toHexEF())
  const stub = ef.inputs[0].sourceTransaction!
  assert.equal(isEfStub(stub), true)
  assert.equal(fullSourceTx(ef.inputs[0]), undefined)
  assert.deepEqual(inputPrevout(ef.inputs[0]), { txid: parent.id('hex'), vout: 1 })
  assert.deepEqual(ancestorsOf(ef), [])
})
