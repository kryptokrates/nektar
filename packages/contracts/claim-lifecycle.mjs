import { RunarContract, MockProvider, LocalSigner } from 'runar-sdk'
import { readFileSync } from 'node:fs'
const { Spend, Transaction, Hash, Transaction: BsvTransaction, LockingScript, UnlockingScript } = await import('@bsv/sdk')
const art = JSON.parse(readFileSync('MamemZkClaim.json', 'utf8'))
const prover = new LocalSigner('11'.repeat(32)); const challenger = new LocalSigner('22'.repeat(32))
const pk = await prover.getPublicKey(); const cpk = await challenger.getPublicKey()
const pkh = x => Buffer.from(Hash.hash160(Buffer.from(x,'hex'))).toString('hex')
const p2pkhScript = p => '76a914'+p+'88ac'
const Z='00'.repeat(32)
const RH='aa'.repeat(32), JR='bb'.repeat(32), QID='cc'.repeat(32)
let PASS=0, FAILS=[]

// --- local arg encoders (SDK internals aren't exported) ---
const encPush = h => { const l=h.length/2
  if(l===0) return '00'
  if(l<=75) return l.toString(16).padStart(2,'0')+h
  if(l<=0xff) return '4c'+l.toString(16).padStart(2,'0')+h
  return '4d'+(l&0xff).toString(16).padStart(2,'0')+((l>>8)&0xff).toString(16).padStart(2,'0')+h }
const encNum = n => { if(n===0n)return '00'; if(n>=1n&&n<=16n)return (0x50+Number(n)).toString(16)
  if(n===-1n)return '4f'
  const neg=n<0n; let a=neg?-n:n; const b=[]
  while(a>0n){b.push(Number(a&0xffn));a>>=8n}
  if(b[b.length-1]&0x80) b.push(neg?0x80:0); else if(neg) b[b.length-1]|=0x80
  return encPush(b.map(x=>x.toString(16).padStart(2,'0')).join('')) }
const encArg = v => typeof v==='bigint'?encNum(v):encPush(v)

const evalSpend = (prov,tag,idx=0,srcIdx=1)=>{
  const hx=prov.getBroadcastedTxs()
  const st=Transaction.fromHex(hx.at(-1)); const dp=Transaction.fromHex(hx.at(-1-srcIdx))
  const srcOut = dp.outputs[st.inputs[idx].sourceOutputIndex]
  const sp=new Spend({sourceTXID:st.inputs[idx].sourceTXID,sourceOutputIndex:st.inputs[idx].sourceOutputIndex,sourceSatoshis:srcOut.satoshis??5000,
    lockingScript:srcOut.lockingScript,transactionVersion:st.version,
    otherInputs:st.inputs.filter((_,i)=>i!==idx),outputs:st.outputs,inputIndex:idx,
    unlockingScript:st.inputs[idx].unlockingScript,inputSequence:st.inputs[idx].sequence??0xffffffff,
    lockTime:st.lockTime??0,isRelaxed:true})
  try{sp.validate();console.log(tag,'VALID ops:',sp.executedOpCount);PASS++}catch(e){console.log(tag,'FAIL:',e.message.slice(0,80));FAILS.push(tag)}
}
const newProv = ()=>{const p=new MockProvider('testnet');p.addUtxo('1Q1pE5vPGEEMqRcVRMbtBK842Y6Pzo6nK9',{txid:'ab'.repeat(32),outputIndex:0,satoshis:1e6,script:''});return p}
const deploy = async (prov,opts={})=>{
  const {deadline=1000n,window=100n,stages=4n}=opts
  const ctor=[0n,RH,JR,10n,QID,5000n,deadline,0n,Z,0n,stages,Z,pkh(pk),1000n,window,stages]
  const c=new RunarContract(art,ctor);c.connect(prov,prover);await c.deploy({satoshis:5000})
  return c
}
const st0 = (over={})=>({status:0n,receiptHash:RH,journalRoot:JR,journalSize:10n,queryId:QID,bondSats:5000n,deadline:1000n,responseDeadline:0n,challengerPkh:Z,bisectLo:0n,bisectHi:4n,midCommit:Z,proverPkh:pkh(pk),maxFee:1000n,window:100n,stages:4n,...over})
const pkh_addr = await prover.getAddress(); const cpk_addr = await challenger.getAddress()
const fund = (prov,who,amt=6000)=>prov.addUtxo(who===prover?pkh_addr:cpk_addr,{txid:Math.random().toString(16).slice(2).padEnd(64,'0').slice(0,64),outputIndex:0,satoshis:amt,script:''})

// Terminal methods: emitted script expects [codePart][opSig][args][changePKH][changeAmount][preimage][selector]
// and a trailing 0-sat change output — patch the prepared call flags.
const callTerminal = async (c, method, args, prov, signer, outputs, changePkh) => {
  const op=c._provider, os=c._signer; c._provider=prov; c._signer=signer
  try {
    const pubMethods = c.artifact.abi.methods.filter(m=>m.isPublic)
    const mIdx = pubMethods.findIndex(m=>m.name===method)
    const prepared = await c.prepareCall(method, args, {terminalOutputs:[...outputs,{scriptHex:p2pkhScript(changePkh),satoshis:0}]})
    prepared._methodNeedsChange = true
    prepared._changePKHHex = changePkh
    prepared._changeAmount = 0n
    const sigSubscript = c.getSubscriptForSigning(prepared._contractUtxo.script, mIdx)
    const signatures = {}
    const txHex = prepared.tx.toHex()
    for (const idx of prepared.sigIndices) signatures[idx] = await signer.sign(txHex, 0, sigSubscript, prepared._contractUtxo.satoshis)
    return await c.finalizeCall(prepared, signatures)
  } finally { c._provider=op; c._signer=os }
}

// ── 1. settle (happy path): deadline=0 so locktime 0 passes ──
{
  const prov=newProv()
  const ctor=[0n,RH,JR,10n,QID,5000n,0n,0n,Z,0n,4n,Z,pkh(pk),1000n,100n,4n]
  const c=new RunarContract(art,ctor);c.connect(prov,prover);await c.deploy({satoshis:5000})
  const verdict='6a'+RH+JR+QID
  await callTerminal(c,'settle',[null,pk,4990n],prov,prover,[{scriptHex:verdict,satoshis:1},{scriptHex:p2pkhScript(pkh(pk)),satoshis:4990}],pkh(pk))
  evalSpend(prov,'settle')
}
// ── 2. challenge + reveal + bisect + proveStage ──
{
  const prov=newProv()
  const c=await deploy(prov)
  fund(prov,challenger)
  await c.call('challenge',[null,cpk],prov,challenger,{newState:st0({status:1n,challengerPkh:pkh(cpk),responseDeadline:100n})})
  evalSpend(prov,'challenge')
  fund(prov,prover)
  const MID='dd'.repeat(32)
  await c.call('reveal',[null,pk,MID],prov,prover,{newState:st0({status:2n,challengerPkh:pkh(cpk),responseDeadline:100n,midCommit:MID})})
  evalSpend(prov,'reveal',0,1)
  fund(prov,challenger)
  await c.call('bisect',[null,cpk,1n],prov,challenger,{newState:st0({status:1n,challengerPkh:pkh(cpk),responseDeadline:100n,bisectLo:2n,bisectHi:4n})})
  evalSpend(prov,'bisect1',0,1)
  fund(prov,prover)
  const MID2='ee'.repeat(32)
  await c.call('reveal',[null,pk,MID2],prov,prover,{newState:st0({status:2n,challengerPkh:pkh(cpk),responseDeadline:100n,bisectLo:2n,bisectHi:4n,midCommit:MID2})})
  evalSpend(prov,'reveal2',0,1)
  fund(prov,challenger)
  await c.call('bisect',[null,cpk,1n],prov,challenger,{newState:st0({status:2n,challengerPkh:pkh(cpk),responseDeadline:100n,bisectLo:3n,bisectHi:4n})})
  evalSpend(prov,'bisect2',0,1)
  // proveStage: manual 2-input tx — [claim UTXO, stage UTXO]; terminal unlock layout
  {
    const claimUtxo = c.currentUtxo
    const stageUtxo={txid:'ef'.repeat(32),outputIndex:0,satoshis:2000,script:p2pkhScript(pkh(pk))}
    const ourOp = Buffer.concat([Buffer.from(claimUtxo.txid,'hex').reverse(),Buffer.from([0,0,0,0])]).toString('hex')
    const stageOp = Buffer.concat([Buffer.from(stageUtxo.txid,'hex').reverse(),Buffer.from([0,0,0,0])]).toString('hex')
    const prevouts = ourOp+stageOp
    const tx = new BsvTransaction()
    tx.addInput({sourceTXID:claimUtxo.txid,sourceOutputIndex:claimUtxo.outputIndex,unlockingScript:UnlockingScript.fromHex('00'),sequence:0xffffffff})
    tx.addInput({sourceTXID:stageUtxo.txid,sourceOutputIndex:0,unlockingScript:UnlockingScript.fromHex('00'),sequence:0xffffffff})
    tx.addOutput({satoshis:4990,lockingScript:LockingScript.fromHex(p2pkhScript(pkh(pk)))})
    tx.addOutput({satoshis:0,lockingScript:LockingScript.fromHex(p2pkhScript(pkh(pk)))})
    const pubMethods = c.artifact.abi.methods.filter(m=>m.isPublic)
    const mIdx = pubMethods.findIndex(m=>m.name==='proveStage')
    const {sigHex:opSig, preimageHex:preimage} = c.computeOpPushTxWithCodeSep(tx, 0, claimUtxo.script, claimUtxo.satoshis, mIdx)
    const sigSub = c.getSubscriptForSigning(claimUtxo.script, mIdx)
    const userSig = await prover.sign(tx.toHex(), 0, sigSub, claimUtxo.satoshis)
    const unlock = c.buildStatefulPrefix(opSig, true)
      + encArg(userSig)+encArg(pk)+encArg(prevouts)+encArg(stageOp)+encArg(4990n)
      + encPush(pkh(pk))+encNum(0n)
      + encPush(preimage)+encNum(BigInt(mIdx))
    tx.inputs[0].unlockingScript = UnlockingScript.fromHex(unlock)
    const stageSig = await prover.sign(tx.toHex(), 1, stageUtxo.script, stageUtxo.satoshis)
    tx.inputs[1].unlockingScript = UnlockingScript.fromHex(encPush(stageSig)+encPush(pk))
    await prov.broadcast(tx)
    c.currentUtxo = null
    evalSpend(prov,'proveStage',0,1)
  }
}
// ── 3. claimTimeout (challenger silent; window=0 → responseDeadline=0) ──
{
  const prov=newProv()
  const c=await deploy(prov,{window:0n})
  fund(prov,challenger)
  await c.call('challenge',[null,cpk],prov,challenger,{newState:st0({status:1n,challengerPkh:pkh(cpk),responseDeadline:0n,window:0n})})
  evalSpend(prov,'challenge(w0)',0,1)
  await callTerminal(c,'claimTimeout',[null,pk,4990n],prov,prover,[{scriptHex:p2pkhScript(pkh(pk)),satoshis:4990}],pkh(pk))
  evalSpend(prov,'claimTimeout',0,1)
}
// ── 4. slashTimeout (prover silent after reveal; window=0) ──
{
  const prov=newProv()
  const c=await deploy(prov,{window:0n})
  fund(prov,challenger)
  await c.call('challenge',[null,cpk],prov,challenger,{newState:st0({status:1n,challengerPkh:pkh(cpk),responseDeadline:0n,window:0n})})
  fund(prov,prover)
  const MID='dd'.repeat(32)
  await c.call('reveal',[null,pk,MID],prov,prover,{newState:st0({status:2n,challengerPkh:pkh(cpk),responseDeadline:0n,midCommit:MID,window:0n})})
  await callTerminal(c,'slashTimeout',[null,cpk,4990n],prov,challenger,[{scriptHex:p2pkhScript(pkh(cpk)),satoshis:4990}],pkh(cpk))
  evalSpend(prov,'slashTimeout',0,1)
}
console.log(`\n=== ${PASS} passed, ${FAILS.length} failed: ${FAILS.join(',')}`)
