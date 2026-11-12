// Bisect: which call pattern fails on NektarAnchorRot?
// T1 append; T2 append→append; T3 rotate→rotate; T4 rotate→append.
// Each second call rehydrates c2 solely from the previous tx's output 0 and
// asserts byte-identical locking script before calling.
import { RunarContract, MockProvider, LocalSigner } from '../lib/runar.mjs'
import { PrivateKey } from '../lib/runar.mjs'
import { Transaction, Spend, LockingScript } from '../lib/sdk.mjs'
import { chainRp, utf8, toHex, hash160, fromHex } from '../../packages/core/dist/src/index.js'
import { readFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const art = JSON.parse(readFileSync(path.join(fileURLToPath(import.meta.url), '..', 'NektarAnchorRot.json'), 'utf8'))
const TAG = Buffer.from(utf8('nektar:rprime:v1')).toString('hex')
const pkh = (hex) => Buffer.from(hash160(fromHex(hex))).toString('hex')
const signer = new LocalSigner('11'.repeat(32))
const key = PrivateKey.fromHex('11'.repeat(32))
const pk = await signer.getPublicKey()

const le8 = (h) => {
  let v = 0n
  for (let i = 7; i >= 0; i--) v = (v << 8n) | BigInt(parseInt(h.slice(i * 2, i * 2 + 2), 16))
  return v
}
const parseState = (lock) => {
  for (let p = lock.length - 2; p >= 0; p -= 2) {
    if (lock.slice(p, p + 2) !== '6a') continue
    const cand = lock.slice(p + 2)
    if (cand.length >= 200 && cand.slice(160, 162) === '00') {
      const tl = parseInt(cand.slice(162, 164), 16) * 2
      const tail = 164 + tl
      return {
        n: le8(cand.slice(0, 16)),
        size: le8(cand.slice(16, 32)),
        R: cand.slice(32, 96),
        Rp: cand.slice(96, 160),
        tag: cand.slice(164, tail),
        pkh: cand.slice(tail + 2, tail + 42)
      }
    }
  }
  return null
}

const deploy = async () => {
  const prov = new MockProvider('testnet')
  prov.addUtxo(await signer.getAddress(), { txid: 'ab'.repeat(32), outputIndex: 0, satoshis: 1e6, script: '' })
  const R0 = Uint8Array.from({ length: 32 }, () => 0x11)
  const Rp0 = chainRp(new Uint8Array(32), R0, 1)
  const c = new RunarContract(art, [pk, pkh(pk), 0n, 1n, toHex(R0), toHex(Rp0), '', TAG])
  c.connect(prov, signer)
  await c.deploy({ satoshis: 1 })
  return { prov, c }
}

const signCall = async (c, method, args) => {
  const prep = await c.prepareCall(method, args)
  const der = Buffer.from(key.sign(Array.from(Buffer.from(prep.sighash, 'hex'))).toDER()).toString('hex') + '41'
  const sigs = {}
  for (const i of prep.sigIndices) sigs[i] = der
  return c.finalizeCall(prep, sigs)
}

const evalSpend = (txHex, lockingHex, sats = 1) => {
  const tx = Transaction.fromHex(txHex)
  const i = tx.inputs.findIndex((x) => x.unlockingScript.toHex().length > 100)
  const spend = new Spend({
    sourceTXID: tx.inputs[i].sourceTXID,
    sourceOutputIndex: tx.inputs[i].sourceOutputIndex,
    sourceSatoshis: sats,
    lockingScript: LockingScript.fromHex(lockingHex),
    transactionVersion: tx.version,
    otherInputs: tx.inputs.filter((_, j) => j !== i),
    allInputs: tx.inputs,
    outputs: tx.outputs,
    inputIndex: i,
    unlockingScript: tx.inputs[i].unlockingScript,
    inputSequence: tx.inputs[i].sequence ?? 0xffffffff,
    lockTime: tx.lockTime
  })
  return spend.validate()
}

const signCallKey = async (c, method, args, k) => {
  const prep = await c.prepareCall(method, args)
  const der = Buffer.from(k.sign(Array.from(Buffer.from(prep.sighash, 'hex'))).toDER()).toString('hex') + '41'
  const sigs = {}
  for (const i of prep.sigIndices) sigs[i] = der
  return c.finalizeCall(prep, sigs)
}

const mkC2 = (prov, prevTx, signr = signer) => {
  const lock = prevTx.outputs[0].lockingScript.toHex()
  const st = parseState(lock)
  const cc = new RunarContract(art, [pk, st.pkh, st.n, st.size, st.R, st.Rp, '', st.tag])
  cc.connect(prov, signr)
  cc['currentUtxo'] = { txid: prevTx.id('hex'), outputIndex: 0, satoshis: prevTx.outputs[0].satoshis ?? 1, script: lock }
  return { cc, lock }
}

for (const [name, steps] of [
  ['T1 append', [['append', [null, pk, 'aa'.repeat(32), 2n]]]],
  [
    'T2 append→append',
    [
      ['append', [null, pk, 'aa'.repeat(32), 2n]],
      ['append', [null, pk, 'bb'.repeat(32), 3n]]
    ]
  ],
  [
    'T3 rotate→rotate',
    [
      ['rotate', [null, pk, pkh(await new LocalSigner('33'.repeat(32)).getPublicKey())]],
      ['rotate', [null, pk, pkh(await new LocalSigner('44'.repeat(32)).getPublicKey())]]
    ]
  ],
  [
    'T4 rotate→append (old key — must fail)',
    [
      ['rotate', [null, pk, pkh(await new LocalSigner('33'.repeat(32)).getPublicKey())]],
      ['append', [null, pk, 'bb'.repeat(32), 3n]]
    ]
  ],
  ['T5 rotate→append (new key — must pass)', 'T5'],
  ['T6 rotate via freshContract (new key — must pass)', 'T6']
]) {
  if (steps === 'T6') {
    const { prov, c } = await deploy()
    const newSigner = new LocalSigner('33'.repeat(32))
    const newPk = await newSigner.getPublicKey()
    const newKeyP = PrivateKey.fromHex('33'.repeat(32))
    let label = 'PASS'
    try {
      const gTx = prov.broadcastedTxObjects.at(-1) // deploy tx is last broadcast
      const { cc, lock } = mkC2(prov, gTx)
      if (cc.getLockingScript() !== lock) {
        label = 'rebuilt differs'
        throw new Error(label)
      }
      await signCall(cc, 'rotate', [null, pk, pkh(newPk)])
      try {
        evalSpend(prov.broadcastedTxs.at(-1), lock)
      } catch (e) {
        label = `rotate eval threw: ${e.message.split('\n')[0]} PC:${e.programCounter ?? '?'}`
        throw new Error(label, { cause: e })
      }
      console.log('T6 rotate via freshContract: PASS')
    } catch {
      console.log(`T6 rotate via freshContract: ${label}`)
    }
    continue
  }
  if (steps === 'T5') {
    const { prov, c } = await deploy()
    const newSigner = new LocalSigner('33'.repeat(32))
    const newPk = await newSigner.getPublicKey()
    const newKeyP = PrivateKey.fromHex('33'.repeat(32))
    let label = 'PASS'
    try {
      const lock0 = c.getLockingScript()
      await signCall(c, 'rotate', [null, pk, pkh(newPk)])
      try {
        evalSpend(prov.broadcastedTxs.at(-1), lock0)
      } catch (e) {
        label = `rotate eval threw: ${e.message.split('\n')[0]}`
        throw new Error(label, { cause: e })
      }
      const prevTx = prov.broadcastedTxObjects.at(-1)
      const { cc, lock } = mkC2(prov, prevTx) // owner signer = funding; contract sig uses new key
      if (cc.getLockingScript() !== lock) {
        label = 'rebuilt differs'
        throw new Error(label)
      }
      await signCallKey(cc, 'append', [null, newPk, 'bb'.repeat(32), 3n], newKeyP)
      try {
        evalSpend(prov.broadcastedTxs.at(-1), lock)
      } catch (e) {
        label = `append(new key) eval threw: ${e.message.split('\n')[0]} PC:${e.programCounter ?? '?'}`
        throw new Error(label, { cause: e })
      }
      console.log('T5 rotate→append (new key): PASS')
    } catch {
      console.log(`T5 rotate→append (new key): ${label}`)
    }
    continue
  }
  const { prov, c } = await deploy()
  let label = 'PASS'
  try {
    // step 1 on deploy contract
    const [m1, a1] = steps[0]
    const lock0 = c.getLockingScript()
    await signCall(c, m1, a1)
    let txHex = prov.broadcastedTxs.at(-1)
    let ok = true
    try {
      evalSpend(txHex, lock0)
    } catch (e) {
      ok = false
      label = `STEP1 eval threw: ${e.message.split('\n')[0]} PC:${e.programCounter ?? '?'}`
    }
    if (!ok) throw new Error(label)
    if (steps[1]) {
      const prevTx = prov.broadcastedTxObjects.at(-1)
      const { cc, lock } = mkC2(prov, prevTx)
      const rebuilt = cc.getLockingScript()
      if (rebuilt !== lock) {
        label = `REBUILT script differs (len ${rebuilt.length / 2} vs ${lock.length / 2})`
        throw new Error(label)
      }
      const [m2, a2] = steps[1]
      await signCall(cc, m2, a2)
      txHex = prov.broadcastedTxs.at(-1)
      try {
        evalSpend(txHex, lock)
      } catch (e) {
        label = `STEP2 eval threw: ${e.message.split('\n')[0]} PC:${e.programCounter ?? '?'}`
        throw new Error(label, { cause: e })
      }
    }
    label = 'PASS'
  } catch (e) {
    if (label === 'PASS') label = `prepareCall/finalizeCall threw: ${e.message.split('\n')[0]}`
  }
  console.log(`${name}: ${label}`)
}
