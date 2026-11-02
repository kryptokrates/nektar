import { RunarContract, MockProvider, LocalSigner } from 'runar-sdk'
import { readFileSync } from 'node:fs'
const { Spend, Transaction } = await import('@bsv/sdk')
const { rc, di } = JSON.parse(readFileSync('poseidon2-consts.json', 'utf8'))
const art = JSON.parse(readFileSync('Poseidon2Stage.json', 'utf8'))
const signer = new LocalSigner('11'.repeat(32))
const prov = new MockProvider('testnet')
await signer.getPublicKey()
const P = 2013265921n, C = 24
let uid = 0
async function run (method, round) {
  prov.addUtxo('1Q1pE5vPGEEMqRcVRMbtBK842Y6Pzo6nK9', { txid: (uid++).toString(16).padStart(64,'0'), outputIndex: 0, satoshis: 1e6, script: '' })
  const init = Array.from({length:C},(_,i)=>BigInt(i))
  const rcArgs = []
  const c = new RunarContract(art, [...init, BigInt(round)])
  c.connect(prov, signer); await c.deploy({ satoshis: 1 })
  await c.call(method, [])
  const hexes = prov.getBroadcastedTxs()
  const spendTx = Transaction.fromHex(hexes.at(-1)); const deployTx = Transaction.fromHex(hexes.at(-2))
  const spend = new Spend({ sourceTXID: spendTx.inputs[0].sourceTXID, sourceOutputIndex: 0, sourceSatoshis: 1,
    lockingScript: deployTx.outputs[0].lockingScript, transactionVersion: spendTx.version,
    otherInputs: spendTx.inputs.slice(1), outputs: spendTx.outputs, inputIndex: 0,
    unlockingScript: spendTx.inputs[0].unlockingScript, inputSequence: spendTx.inputs[0].sequence ?? 0xffffffff,
    lockTime: spendTx.lockTime ?? 0, isRelaxed: true })
  try { spend.validate(); return `VALID ops:${spend.executedOpCount} peakStack:${spend.stackMem}B spendTx:${hexes.at(-1).length/2}B` } catch(e){ return 'FAIL '+e.message.slice(0,80) }
}
console.log('fullStep (round 0)   :', await run('fullStep', 0))
console.log('partialStep (round 4):', await run('partialStep', 4))
