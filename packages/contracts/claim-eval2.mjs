import { RunarContract, MockProvider, LocalSigner } from 'runar-sdk'
import { readFileSync } from 'node:fs'
const { Spend, Transaction, Hash } = await import('@bsv/sdk')
const art = JSON.parse(readFileSync('MamemZkClaim.json', 'utf8'))
const prover = new LocalSigner('11'.repeat(32))
const challenger = new LocalSigner('22'.repeat(32))
const prov = new MockProvider('testnet')
const pk = await prover.getPublicKey(); const cpk = await challenger.getPublicKey()
const pkh = x => Buffer.from(Hash.hash160(Buffer.from(x,'hex'))).toString('hex')
const ADDR='1Q1pE5vPGEEMqRcVRMbtBK842Y6Pzo6nK9'; let uid=0
const Z='00'.repeat(32)
const base = () => ({status:0n,receiptHash:'aa'.repeat(32),journalRoot:'bb'.repeat(32),journalSize:10n,queryId:'cc'.repeat(32),bondSats:5000n,deadline:1000n,responseDeadline:0n,challengerPkh:Z,bisectLo:0n,bisectHi:29n,midCommit:Z})
function mk(st){ const s=base(); Object.assign(s,st); return new RunarContract(art,[s.status,s.receiptHash,s.journalRoot,s.journalSize,s.queryId,s.bondSats,s.deadline,s.responseDeadline,s.challengerPkh,s.bisectLo,s.bisectHi,s.midCommit,pkh(pk),1000n,100n,29n]) }
async function deploy(d,w){ prov.addUtxo(ADDR,{txid:(uid++).toString(16).padStart(64,'0'),outputIndex:0,satoshis:1e6,script:''}); const c=mk({deadline:BigInt(d),responseDeadline:0n}); c.connect(prov,prover); await c.deploy({satoshis:5000}); return c }
function evalLast(){
  const hexes=prov.getBroadcastedTxs()
  const spendTx=Transaction.fromHex(hexes.at(-1)); const deployTx=Transaction.fromHex(hexes.at(-2))
  const spend=new Spend({sourceTXID:spendTx.inputs[0].sourceTXID,sourceOutputIndex:0,sourceSatoshis:deployTx.outputs[0].satoshis??5000,
    lockingScript:deployTx.outputs[0].lockingScript,transactionVersion:spendTx.version,
    otherInputs:spendTx.inputs.slice(1),outputs:spendTx.outputs,inputIndex:0,
    unlockingScript:spendTx.inputs[0].unlockingScript,inputSequence:spendTx.inputs[0].sequence??0xffffffff,
    lockTime:spendTx.lockTime??0,isRelaxed:true})
  try{spend.validate();return `VALID ops:${spend.executedOpCount}`}catch(e){return 'FAIL '+e.message.slice(0,70)}
}
const c = await deploy(1000,100)
try { await c.call('challenge',[null,cpk],prov,challenger,{newState:{status:1n,challengerPkh:pkh(cpk),bisectHi:29n,responseDeadline:100n}})
      console.log('challenge:',evalLast()) } catch(e){ console.log('challenge threw:',e.message.slice(0,90)) }
try { await c.call('reveal',[null,pk,'dd'.repeat(32)],prov,prover,{newState:{status:2n,midCommit:'dd'.repeat(32)}})
      console.log('reveal:',evalLast()) } catch(e){ console.log('reveal threw:',e.message.slice(0,90)) }
try { await c.call('bisect',[null,cpk,1n],prov,challenger,{newState:{status:1n,bisectLo:14n,midCommit:Z,responseDeadline:100n}})
      console.log('bisect:',evalLast()) } catch(e){ console.log('bisect threw:',e.message.slice(0,90)) }
