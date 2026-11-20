import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Beef, MerklePath, PrivateKey, P2PKH, Transaction, UnlockingScript } from '@bsv/sdk'
import { LiveWalletServices } from '../src/toolbox-services.js'

// Mock BRC-100 wallet: listOutputs serves a canned spendable set + the BEEF
// the class rebuilds Transaction objects from. internalize/relinquish are
// recorded so custody transitions are assertable.
interface Out {
  outpoint: string
  satoshis: number
  spendable: boolean
}
const fundingTx = (sats: number): Transaction => {
  const tx = new Transaction()
  tx.addOutput({ lockingScript: new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toHash()), satoshis: sats })
  return tx
}
const mergedBeef = (...txs: Transaction[]): number[] => {
  const beef = new Beef()
  for (const tx of txs) beef.mergeTransaction(tx)
  return beef.toBinary()
}
const mkWallet = (outs: Out[], beef: number[]) => {
  const relinquished: string[] = []
  const internalized: number[] = []
  return {
    relinquished,
    internalized,
    wallet: {
      listOutputs: async () => ({ outputs: outs, BEEF: beef }),
      relinquishOutput: async (a: { output: string }) => {
        relinquished.push(a.output)
        return { relinquished: true }
      },
      internalizeAction: async (a: { tx: number[] }) => {
        internalized.push(a.tx.length)
        return { accepted: true }
      }
    }
  }
}
// spv/proofless-funding: a BEEF whose entry carries the tx BUMP hands the
// utxo out WITH its merkle path — the bare-hex rebuild used to strip it, and
// every tree minted on that utxo lost its zero-conf anchor
test('LiveWalletServices.postBeef: the relayed tx keeps its input sources — the guard can price it', async () => {
  // the wallet re-relays its own unconfirmed txs; rebuilt from bare hex the
  // inputs carried no source outputs and SpendGuard refused every one
  const key = PrivateKey.fromRandom()
  const parent = fundingTx(1000)
  parent.outputs[0].lockingScript = new P2PKH().lock(key.toPublicKey().toHash())
  const child = new Transaction()
  child.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(key) })
  child.addOutput({ lockingScript: new P2PKH().lock(key.toPublicKey().toHash()), satoshis: 900 })
  await child.sign()
  const beef = new Beef()
  beef.mergeTransaction(child)
  const seen: Transaction[] = []
  const svc = new LiveWalletServices({
    chain: 'test',
    mc: {},
    tracker: { currentHeight: async () => 1, isValidRootForHeight: async () => true },
    broadcast: async (tx) => {
      seen.push(tx)
      return { status: 'success', txid: tx.id('hex') }
    }
  })
  const r = await svc.postBeef(beef as never, [child.id('hex')])
  assert.equal(r[0].status, 'success')
  assert.equal(seen.length, 1)
  assert.equal(seen[0].id('hex'), child.id('hex'))
  assert.equal(seen[0].inputs[0].sourceTransaction?.outputs[0].satoshis, 1000)
})

// take() reserves the picked outpoint — a second take can't receive it
// every endpoint 404s — index + ARC both ride mc.fetchImpl, so 'the index
// hasn't caught up' is a stub, not a live call
const offlineMc = {
  indexerUrl: 'http://index.test',
  broadcasterUrl: 'http://arc.test',
  fetchImpl: (async () => new Response('nf', { status: 404 })) as never
}
const mkServices = () =>
  new LiveWalletServices({
    chain: 'test',
    mc: offlineMc,
    tracker: { currentHeight: async () => 1, isValidRootForHeight: async () => true },
    broadcast: async (tx) => ({ status: 'success', txid: tx.id('hex') })
  })

// index-lag class: a tx we broadcast/adopted sits in txStore for minutes
// before the index knows it — the wallet's own service calls used to
// answer 'unknown'/404 against our own reality
test('LiveWalletServices: txStore-first reads — own txs answer before the index knows them', async () => {
  const tx = fundingTx(1000)
  const txid = tx.id('hex')
  const svc = mkServices()
  svc.txLookup = async (id) => (id === txid ? tx : undefined)
  const raw = await svc.getRawTx(txid)
  assert.equal(raw.name, 'txstore')
  assert.deepEqual(raw.rawTx, Array.from(tx.toBinary()))
  const st = await svc.getStatusForTxids([txid])
  assert.equal(st.results[0].status, 'known')
  assert.equal(await svc.isUtxo({ txid, vout: 0 }), true)
  assert.equal(await svc.isUtxo({ txid, vout: 3 }), false)
})

// wallet beef consumers apply the same closure rule as internalize — a
// bare unconfirmed tx is a rejection in a bag. Whole ancestry to a BUMP
// is what the call owes.
test('LiveWalletServices.getBeefForTxid: unconfirmed subject carries whole ancestry to a BUMP', async () => {
  const gp = fundingTx(1000) // mined — txStore keeps it path-attached
  gp.merklePath = new MerklePath(100, [
    [
      { offset: 0, hash: gp.id('hex'), txid: true },
      { offset: 1, hash: '00'.repeat(32) }
    ]
  ])
  const parent = new Transaction()
  parent.addInput({ sourceTXID: gp.id('hex'), sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff })
  parent.addOutput({ lockingScript: new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toHash()), satoshis: 800 })
  const subject = new Transaction()
  subject.addInput({ sourceTXID: parent.id('hex'), sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff })
  subject.addOutput({ lockingScript: new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toHash()), satoshis: 600 })
  const store = new Map([
    [gp.id('hex'), gp],
    [parent.id('hex'), parent],
    [subject.id('hex'), subject]
  ])
  const svc = mkServices()
  svc.txLookup = async (id) => store.get(id)
  const beef = await svc.getBeefForTxid(subject.id('hex'))
  const ids = (beef as unknown as Beef).txs.map((t) => t.txid)
  for (const id of [subject.id('hex'), parent.id('hex'), gp.id('hex')]) assert.ok(ids.includes(id), `beef carries ${id.slice(0, 8)}`)
  assert.equal((beef as unknown as Beef).findTxid(gp.id('hex'))?.hasProof, true)
})
