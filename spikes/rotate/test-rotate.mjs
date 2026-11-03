// Rotation spike v2 (ownerPkh pattern): append(sig,pubKey,newR,newSize) and
// rotate(sig,pubKey,newOwnerPkh). Proven with SDK Spend eval (sdk@4.0.1):
//  (a) valid rotate passes; (b) non-owner sig fails; (c) after rotate the old
//      key cannot append and the new key can; (d) append with the right sig
//      but a pubKey whose hash160 != ownerPkh fails.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { RunarContract, MockProvider, LocalSigner } from '../lib/runar.mjs'
import { PrivateKey } from '../lib/runar.mjs'
import { Transaction, Spend, LockingScript } from '../lib/sdk.mjs'
import { chainRp, utf8, toHex, hash160, fromHex } from '../../packages/core/dist/src/index.js'

const art = JSON.parse(readFileSync(path.join(fileURLToPath(import.meta.url), '..', 'NektarAnchorRot.json'), 'utf8'))
const TAG = Buffer.from(utf8('nektar:rprime:v1')).toString('hex')
const pkh = (hex) => Buffer.from(hash160(fromHex(hex))).toString('hex')

const deploy = async (signer) => {
  const prov = new MockProvider('testnet')
  const pk = await signer.getPublicKey()
  prov.addUtxo(await signer.getAddress(), { txid: 'ab'.repeat(32), outputIndex: 0, satoshis: 1e6, script: '' })
  const R0 = Uint8Array.from({ length: 32 }, () => 0x11)
  const Rp0 = chainRp(new Uint8Array(32), R0, 1)
  // ctor: genesisOwner, ownerPkh, n, size, R, Rp, poseidonRoot, tagRprime
  const c = new RunarContract(art, [pk, pkh(pk), 0n, 1n, toHex(R0), toHex(Rp0), '', TAG])
  c.connect(prov, signer)
  await c.deploy({ satoshis: 1 })
  return { prov, c }
}

const evalSpend = (txHex, lockingHex, sats = 1) => {
  const tx = Transaction.fromHex(txHex)
  const i = tx.inputs.findIndex(x => x.unlockingScript.toHex().length > 100)
  const spend = new Spend({
    sourceTXID: tx.inputs[i].sourceTXID, sourceOutputIndex: tx.inputs[i].sourceOutputIndex,
    sourceSatoshis: sats, lockingScript: LockingScript.fromHex(lockingHex),
    transactionVersion: tx.version, otherInputs: tx.inputs.filter((_, j) => j !== i),
    allInputs: tx.inputs, outputs: tx.outputs, inputIndex: i,
    unlockingScript: tx.inputs[i].unlockingScript, inputSequence: tx.inputs[i].sequence ?? 0xffffffff,
    lockTime: tx.lockTime
  })
  return spend.validate()
}

const signCall = async (c, method, args, key) => {
  const prep = await c.prepareCall(method, args)
  const der = Buffer.from(key.sign(Array.from(Buffer.from(prep.sighash, 'hex'))).toDER()).toString('hex') + '41'
  const sigs = {}; for (const i of prep.sigIndices) sigs[i] = der
  return c.finalizeCall(prep, sigs)
}

const owner = new LocalSigner('11'.repeat(32))
const ownerKey = PrivateKey.fromHex('11'.repeat(32))
const otherKey = PrivateKey.fromHex('22'.repeat(32))
const newSigner = new LocalSigner('33'.repeat(32))
const newKey = PrivateKey.fromHex('33'.repeat(32))

const le8 = (h) => { let v = 0n; for (let i = 7; i >= 0; i--) v = (v << 8n) | BigInt(parseInt(h.slice(i * 2, i * 2 + 2), 16)); return v }
const parseState = (lock) => {
  // find the OP_RETURN whose suffix = n|size|R|Rp|poseidon(00)|len|tag|len|20B pkh
  for (let p = lock.length - 2; p >= 0; p -= 2) {
    if (lock.slice(p, p + 2) !== '6a') continue
    const cand = lock.slice(p + 2)
    if (cand.length >= 200 && cand.slice(160, 162) === '00') {
      const tl = parseInt(cand.slice(162, 164), 16) * 2
      const tail = 164 + tl
      return { n: le8(cand.slice(0, 16)), size: le8(cand.slice(16, 32)), R: cand.slice(32, 96), Rp: cand.slice(96, 160), tag: cand.slice(164, tail), pkh: cand.slice(tail + 2, tail + 42) }
    }
  }
  return null
}

test('rotate: valid rotation passes Spend eval', async () => {
  const { prov, c } = await deploy(owner)
  const lock = c.getLockingScript()
  const oPub = await owner.getPublicKey()
  const nPkh = pkh(await newSigner.getPublicKey())
  await signCall(c, 'rotate', [null, oPub, nPkh], ownerKey)
  assert.equal(evalSpend(prov.broadcastedTxs.at(-1), lock), true, 'valid rotate')
})

test('rotate: signed by non-owner fails', async () => {
  const { prov, c } = await deploy(owner)
  const lock = c.getLockingScript()
  const oPub = await owner.getPublicKey()
  const nPkh = pkh(await newSigner.getPublicKey())
  await signCall(c, 'rotate', [null, oPub, nPkh], otherKey)
  assert.throws(() => evalSpend(prov.broadcastedTxs.at(-1), lock), /OP_CHECKSIGVERIFY|top stack|signature|OP_EQUALVERIFY/, 'non-owner rotate')
})

test('rotate: after rotation old key cannot append, new key can', async () => {
  const { prov, c } = await deploy(owner)
  const oPub = await owner.getPublicKey()
  const nPub = await newSigner.getPublicKey()
  const nPkh = pkh(nPub)
  await signCall(c, 'rotate', [null, oPub, nPkh], ownerKey)
  const rotTx = prov.broadcastedTxObjects.at(-1)
  const rotLock = rotTx.outputs[0].lockingScript.toHex()
  const sats = rotTx.outputs[0].satoshis ?? 1
  const st = parseState(rotLock)
  const gPub = oPub
  const mkC2 = () => {
    const cc = new RunarContract(art, [gPub, st.pkh, st.n, st.size, st.R, st.Rp, '', st.tag])
    cc.connect(prov, owner) // funding uses owner's utxos; contract sig below uses the caller's key
    cc['currentUtxo'] = { txid: rotTx.id('hex'), outputIndex: 0, satoshis: sats, script: rotLock }
    return cc
  }
  // old key append → FAIL
  await signCall(mkC2(), 'append', [null, oPub, 'aa'.repeat(32), 2n], ownerKey)
  assert.throws(() => evalSpend(prov.broadcastedTxs.at(-1), rotLock), /OP_CHECKSIGVERIFY|top stack|signature|OP_EQUALVERIFY/, 'old key append rejected')
  // new key append → PASS
  await signCall(mkC2(), 'append', [null, nPub, 'aa'.repeat(32), 2n], newKey)
  assert.equal(evalSpend(prov.broadcastedTxs.at(-1), rotLock), true, 'new key append accepted')
})

test('append: right sig but wrong pubKey (hash160 mismatch) fails', async () => {
  const { prov, c } = await deploy(owner)
  const lock = c.getLockingScript()
  const wrongPub = await new LocalSigner('44'.repeat(32)).getPublicKey()
  // sig by owner, but pubKey arg is a DIFFERENT key → hash160(pubKey) != ownerPkh
  await signCall(c, 'append', [null, wrongPub, 'bb'.repeat(32), 2n], ownerKey)
  assert.throws(() => evalSpend(prov.broadcastedTxs.at(-1), lock), /OP_CHECKSIGVERIFY|top stack|signature|assert|OP_EQUALVERIFY/, 'pubkey-pkh mismatch rejected')
})
