import { RunarContract, MockProvider, LocalSigner } from 'runar-sdk'
import { readFileSync } from 'node:fs'
const { Spend, Transaction, Hash } = await import('@bsv/sdk')
const art = JSON.parse(readFileSync('MamemZkClaim.json', 'utf8'))
const prover = new LocalSigner('11'.repeat(32)); const challenger = new LocalSigner('22'.repeat(32))
const prov = new MockProvider('testnet')
const pk = await prover.getPublicKey(); const cpk = await challenger.getPublicKey()
const pkh = x => Buffer.from(Hash.hash160(Buffer.from(x,'hex'))).toString('hex')
const Z='00'.repeat(32)
prov.addUtxo('1Q1pE5vPGEEMqRcVRMbtBK842Y6Pzo6nK9',{txid:'ab'.repeat(32),outputIndex:0,satoshis:1e6,script:''})
const c = new RunarContract(art,[0n,'aa'.repeat(32),'bb'.repeat(32),10n,'cc'.repeat(32),5000n,1000n,0n,Z,0n,29n,Z,pkh(pk),1000n,100n,29n])
c.connect(prov,prover); await c.deploy({satoshis:5000})
await c.call('challenge',[null,cpk],prov,challenger,{newState:{status:1n,challengerPkh:pkh(cpk),bisectHi:29n,responseDeadline:100n}})
const hexes=prov.getBroadcastedTxs()
const spendTx=Transaction.fromHex(hexes.at(-1)); const deployTx=Transaction.fromHex(hexes.at(-2))
const spend=new Spend({sourceTXID:spendTx.inputs[0].sourceTXID,sourceOutputIndex:0,sourceSatoshis:5000,
  lockingScript:deployTx.outputs[0].lockingScript,transactionVersion:spendTx.version,
  otherInputs:spendTx.inputs.slice(1),outputs:spendTx.outputs,inputIndex:0,
  unlockingScript:spendTx.inputs[0].unlockingScript,inputSequence:spendTx.inputs[0].sequence??0xffffffff,
  lockTime:spendTx.lockTime??0,isRelaxed:true})
let steps=0
try { while(spend.step()){ steps++ } } catch(e){
  console.log('failed at step',steps,'ctx:',spend.context,'pc:',spend.programCounter,'stack:',spend.stack.length,'items')
  console.log('top 3 stack lens:',spend.stack.slice(-3).map(x=>x.length))
}
