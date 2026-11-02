import { RunarContract, MockProvider, LocalSigner } from 'runar-sdk'
import { readFileSync } from 'node:fs'
const { Spend, Transaction, Hash } = await import('@bsv/sdk')
const art = JSON.parse(readFileSync('MamemZkClaim.json', 'utf8'))
const prover = new LocalSigner('11'.repeat(32))
const challenger = new LocalSigner('22'.repeat(32))
const prov = new MockProvider('testnet')
const pk = await prover.getPublicKey()
const cpk = await challenger.getPublicKey()
const pkh = x => Buffer.from(Hash.hash160(Buffer.from(x,'hex'))).toString('hex')
const ADDR = '1Q1pE5vPGEEMqRcVRMbtBK842Y6Pzo6nK9'
let uid = 0
const fund = (a=ADDR) => prov.addUtxo(a, { txid: (uid++).toString(16).padStart(64,'0'), outputIndex: 0, satoshis: 1e6, script: '' })
const Z = '00'.repeat(32)
const mk = (deadline, window) => new RunarContract(art, [0n, 'aa'.repeat(32), 'bb'.repeat(32), 10n, 'cc'.repeat(32), 5000n, deadline, 0n, Z, 0n, 29n, Z, pkh(pk), 1000n, window, 29n])
async function deploy (deadline, window) { fund(); const c = mk(BigInt(deadline), BigInt(window)); c.connect(prov, prover); await c.deploy({ satoshis: 5000 }); return c }
function evalLast () {
  const hexes = prov.getBroadcastedTxs()
  const spendTx = Transaction.fromHex(hexes.at(-1)); const deployTx = Transaction.fromHex(hexes.at(-2))
  const spend = new Spend({ sourceTXID: spendTx.inputs[0].sourceTXID, sourceOutputIndex: 0, sourceSatoshis: deployTx.outputs[0].satoshis ?? 5000,
    lockingScript: deployTx.outputs[0].lockingScript, transactionVersion: spendTx.version,
    otherInputs: spendTx.inputs.slice(1), outputs: spendTx.outputs, inputIndex: 0,
    unlockingScript: spendTx.inputs[0].unlockingScript, inputSequence: spendTx.inputs[0].sequence ?? 0xffffffff,
    lockTime: spendTx.lockTime ?? 0, isRelaxed: true })
  try { spend.validate(); return `VALID ops:${spend.executedOpCount}` } catch(e){ return 'FAIL '+e.message.slice(0,70) }
}
// A) settle happy path (deadline=0)
{ const c = await deploy(0, 100)
  await c.call('settle', [null, pk, 4000n])
  console.log('settle (happy path)     :', evalLast()) }
// B) dispute: challenge → reveal → bisect
{ const c = await deploy(1000, 100)
  await c.call('challenge', [null, cpk], prov, challenger)
  console.log('challenge               :', evalLast())
  await c.call('reveal', [null, pk, 'dd'.repeat(32)])
  console.log('reveal                  :', evalLast())
  await c.call('bisect', [null, cpk, 1n], prov, challenger)
  console.log('bisect (take upper)     :', evalLast()) }
