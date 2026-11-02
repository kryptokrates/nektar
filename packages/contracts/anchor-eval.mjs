import { RunarContract, MockProvider, LocalSigner } from 'runar-sdk'
import { readFileSync } from 'node:fs'
const { Spend, Transaction, Hash, PublicKey } = await import('@bsv/sdk')
const art = JSON.parse(readFileSync('MamemAnchor.json', 'utf8'))
const signer = new LocalSigner('11'.repeat(32))
const prov = new MockProvider('testnet')
const pk = await signer.getPublicKey()
const pkh = (x) => Buffer.from(Hash.hash160(Buffer.from(x,'hex'))).toString('hex')
prov.addUtxo('1Q1pE5vPGEEMqRcVRMbtBK842Y6Pzo6nK9', { txid: 'ab'.repeat(32), outputIndex: 0, satoshis: 1e6, script: '' })
const c = new RunarContract(art, [pk, pkh(pk), '4d455441', 0n, 10n, '11'.repeat(32), '22'.repeat(32), Buffer.from('RPRIME').toString('hex')])
c.connect(prov, signer)
await c.deploy({ satoshis: 1 })
await c.call('append', [null, pk, '33'.repeat(32), 20n])
const hexes = prov.getBroadcastedTxs()
const spendTx = Transaction.fromHex(hexes.at(-1)); const deployTx = Transaction.fromHex(hexes.at(-2))
const spend = new Spend({ sourceTXID: spendTx.inputs[0].sourceTXID, sourceOutputIndex: 0, sourceSatoshis: 1,
  lockingScript: deployTx.outputs[0].lockingScript, transactionVersion: spendTx.version,
  otherInputs: spendTx.inputs.slice(1), outputs: spendTx.outputs, inputIndex: 0,
  unlockingScript: spendTx.inputs[0].unlockingScript, inputSequence: spendTx.inputs[0].sequence ?? 0xffffffff,
  lockTime: spendTx.lockTime ?? 0, isRelaxed: true })
try { spend.validate(); console.log('anchor append: VALID ops:', spend.executedOpCount) } catch(e){ console.log('anchor append: FAIL', e.message.slice(0,90)) }
