// tx-graph.test.ts — the instance's transaction graph (TxStore) behind fetchTx.
// On testnet/ttn/main the chain stub has no getTx: parents, BEEF for
// counterparties and proofs all come from what the instance itself holds.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, Transaction, P2PKH, MerklePath } from '@bsv/sdk'
import { TxStore, spendValid, stackWorld, stackChain, StackChain, StackFunder } from '@nektar/chain'
import { NektarMemory } from '../src/index.js'

const liveMem = async (t: { skip: (m?: string) => void }): Promise<{ mem: NektarMemory; chain: StackChain; funder: StackFunder } | undefined> => {
  const w = await stackWorld(t, 4)
  if (w === undefined) return
  const { chain, funder } = w
  const view = Object.create(chain) as StackChain
  ;(view as { getTx?: unknown }).getTx = undefined // live backends have no getTx
  return { mem: new NektarMemory(view, funder, funder.keyPair, { network: 'testnet' }), chain, funder }
}

/** parent → child, both signed; the parent is mined on `chain` when asked */
const pair = async (chain: StackChain, mineParent: boolean): Promise<{ parent: Transaction; child: Transaction; key: PrivateKey }> => {
  const key = PrivateKey.fromRandom()
  const root = new Transaction()
  root.addOutput({ satoshis: 20_000, lockingScript: new P2PKH().lock(key.toPublicKey().toAddress()) })
  const parent = new Transaction()
  parent.addInput({ sourceTransaction: root, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(key) })
  parent.addOutput({ satoshis: 10_000, lockingScript: new P2PKH().lock(key.toPublicKey().toAddress()) })
  await parent.sign()
  if (mineParent) {
    assert.equal((await chain.broadcast(parent)).status, 'success')
    await chain.mine(1)
    parent.merklePath = await chain.merklePathOf(parent.id('hex'))
    assert.ok(parent.merklePath !== undefined)
  }
  const child = new Transaction()
  child.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(key) })
  child.addOutput({ satoshis: 9_000, lockingScript: new P2PKH().lock(key.toPublicKey().toAddress()) })
  await child.sign()
  return { parent, child, key }
}

const bumpFor = (txid: string): MerklePath =>
  new MerklePath(1, [
    [
      { offset: 0, hash: txid, txid: true },
      { offset: 1, hash: '00'.repeat(32) }
    ]
  ])

test('fetchTx: a remembered tx is served locally and a pushed proof sticks to it', async (t) => {
  const _lm = await liveMem(t)
  if (_lm === undefined) return
  const { mem, chain } = _lm
  const { child } = await pair(chain, false)
  const txid = child.id('hex')
  mem.rememberTx(child)
  let lookups = 0
  mem.txLookup = async () => {
    lookups++
    return undefined
  }
  const before = await mem.fetchTx(txid)
  assert.ok(before !== undefined && before.merklePath === undefined)
  assert.equal(lookups, 0, 'a held transaction never goes to the network')
  const mp = bumpFor(txid)
  mem.deliverTxProof(txid, mp)
  assert.equal((await mem.fetchTx(txid))?.merklePath, mp)
})

test('fetchTx: proof and tx lookups are single attempts, remembered when they hit', async (t) => {
  const _lm = await liveMem(t)
  if (_lm === undefined) return
  const { mem, chain } = _lm
  const { child } = await pair(chain, false)
  const txid = child.id('hex')
  let txHits = 0
  let proofHits = 0
  mem.txLookup = async (id: string) => {
    txHits++
    return id === txid ? Transaction.fromHex(child.toHex()) : undefined
  }
  const mp = bumpFor(txid)
  mem.proofLookup = async (id: string) => {
    proofHits++
    return id === txid ? mp : undefined
  }
  assert.equal((await mem.fetchTx(txid))?.merklePath, mp)
  await mem.fetchTx(txid)
  assert.deepEqual([txHits, proofHits], [1, 1], 'both served from the graph afterwards')
  const t0 = Date.now()
  assert.equal(await mem.fetchTx('22'.repeat(32)), undefined)
  assert.equal(await mem.fetchTx('22'.repeat(32)), undefined)
  assert.equal(txHits, 2, 'a miss is not retried at once')
  assert.ok(Date.now() - t0 < 1000, 'a miss never sleeps')
})

test('wire: BEEF when the ancestry is proven — the receiver verifies it against its own headers', async (t) => {
  const _lm = await liveMem(t)
  if (_lm === undefined) return
  const { mem, chain } = _lm
  const { child } = await pair(chain, true)
  mem.rememberTx(child)
  const wire = mem.txWire(child.id('hex'))!
  assert.equal(wire.slice(0, 8), '01010101', 'atomic BEEF')
  const other = await liveMem(t)
  if (other === undefined) return
  const got = await TxStore.read(wire, chain)
  assert.equal(got.tx.id('hex'), child.id('hex'))
  assert.equal(got.spv, true)
  // a receiver on a different chain cannot verify the proof
  assert.equal((await TxStore.read(wire, other.chain)).spv, false)
  // a tampered child fails SPV
  const bad = Transaction.fromAtomicBEEF(Array.from(Buffer.from(wire, 'hex')))
  bad.outputs[0].satoshis = 9_999
  assert.equal((await TxStore.read(Buffer.from(bad.toAtomicBEEF()).toString('hex'), chain)).spv, false)
})

test('wire: Extended Format when the parent is unproven; raw when it is unknown', async (t) => {
  const _lm = await liveMem(t)
  if (_lm === undefined) return
  const { mem, chain } = _lm
  const { child } = await pair(chain, false)
  mem.rememberTx(child)
  const wire = mem.txWire(child.id('hex'))!
  assert.match(wire, /^[0-9a-f]{8}0000000000ef/)
  const got = await mem.readTxWire(wire)
  assert.equal(got.tx.id('hex'), child.id('hex'))
  assert.equal(got.spv, false)
  const lone = new TxStore()
  lone.put(Transaction.fromHex(child.toHex()))
  assert.equal(lone.wire(child.id('hex')), child.toHex())
})

test('TxStore: a proof ends the stored ancestry and survives a reload', async (t) => {
  const chain = (await stackChain(t))!
  const { parent, child } = await pair(chain, true)
  const rows = new Map<string, { txid: string; raw: string; proof?: string }>()
  const persist = {
    load: async () => [...rows.values()],
    put: async (r: { txid: string; raw: string; proof?: string }) => {
      rows.set(r.txid, r)
    }
  }
  const a = new TxStore(persist)
  a.put(child)
  await a.flushed()
  assert.deepEqual([...rows.keys()].sort(), [parent.id('hex'), child.id('hex')].sort(), 'nothing above the proven parent is kept')
  const b = new TxStore(persist)
  await b.load()
  const back = b.get(child.id('hex'))!
  assert.ok(back.inputs[0].sourceTransaction?.merklePath !== undefined)
  assert.equal(await back.verify(chain), true)
  assert.deepEqual(b.unproven(), [child.id('hex')])
})

test('spendValid: a real spend passes; a spend of another output or a broken signature does not', async (t) => {
  const chain = (await stackChain(t))!
  const { parent, child } = await pair(chain, false)
  const src = { txid: parent.id('hex'), vout: 0, satoshis: 10_000, script: parent.outputs[0].lockingScript.toHex() }
  assert.equal(spendValid(child, 0, src), true)
  assert.equal(spendValid(child, 0, { ...src, vout: 1 }), false)
  assert.equal(spendValid(child, 0, { ...src, satoshis: 10_001 }), false, 'the signature commits to the amount')
})
