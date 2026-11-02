import { RunarContract, MockProvider, LocalSigner } from 'runar-sdk'
import { readFileSync } from 'node:fs'
const { Spend, Transaction } = await import('@bsv/sdk')
const art = JSON.parse(readFileSync('BabyBearProbe.json', 'utf8'))
const signer = new LocalSigner('11'.repeat(32))
const prov = new MockProvider('testnet')
await signer.getPublicKey()
const P = 2013265921n
let uid = 0
async function run (method, args) {
  prov.addUtxo('1Q1pE5vPGEEMqRcVRMbtBK842Y6Pzo6nK9', { txid: (uid++).toString(16).padStart(64,'0'), outputIndex: 0, satoshis: 1e6, script: '' })
  const c = new RunarContract(art, [])
  c.connect(prov, signer)
  await c.deploy({ satoshis: 1 })
  await c.call(method, args)
  const hexes = prov.getBroadcastedTxs()
  const spendTx = Transaction.fromHex(hexes.at(-1))
  const deployTx = Transaction.fromHex(hexes.at(-2))
  const spend = new Spend({
    sourceTXID: deployTx.id('hex'), sourceOutputIndex: 0,
    sourceSatoshis: 1, lockingScript: deployTx.outputs[0].lockingScript,
    transactionVersion: spendTx.version,
    otherInputs: spendTx.inputs.slice(1),
    outputs: spendTx.outputs,
    inputIndex: 0,
    unlockingScript: spendTx.inputs[0].unlockingScript,
    inputSequence: spendTx.inputs[0].sequence ?? 0xffffffff,
    lockTime: spendTx.lockTime ?? 0
  })
  try { spend.validate(); return `VALID (ops:${spend.executedOpCount} stack:${spend.stackMem}B)` } catch (e) { return 'FAIL: ' + e.message.slice(0, 90) }
}
const x7 = (x) => { const x2 = x*x%P, x4 = x2*x2%P, x6 = x4*x2%P; return x6*x%P }
console.log('mulmod 3*5=15      :', await run('mulmod', [3n, 5n, 15n]))
console.log('mulmod (p-1)^2=1   :', await run('mulmod', [P-1n, P-1n, 1n]))
console.log('mulmod WRONG 16    :', await run('mulmod', [3n, 5n, 16n]))
console.log('sbox7 2^7=128      :', await run('sbox7', [2n, 128n]))
console.log('sbox7 x=123456789  :', await run('sbox7', [123456789n, x7(123456789n)]))
console.log('mdsRow 16x1 = 20   :', await run('mdsRow', [...Array(16).fill(1n), 20n]))
console.log('mdsRow WRONG 21    :', await run('mdsRow', [...Array(16).fill(1n), 21n]))
