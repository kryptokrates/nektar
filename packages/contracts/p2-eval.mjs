import { RunarContract, MockProvider, LocalSigner } from 'runar-sdk'
import { readFileSync } from 'node:fs'
const { Spend, Transaction } = await import('@bsv/sdk')
const { rc, di } = JSON.parse(readFileSync('poseidon2-consts.json', 'utf8'))
const art = JSON.parse(readFileSync('Poseidon2Blocks.json', 'utf8'))
const signer = new LocalSigner('11'.repeat(32))
const prov = new MockProvider('testnet')
await signer.getPublicKey()
const P = 2013265921n, C = 24
const sbox = x => { const x2 = x*x%P, x4 = x2*x2%P, x6 = x4*x2%P; return x6*x%P }
function mExt (s) {
  const t=[0n,0n,0n,0n], out=[]
  for (let b=0;b<6;b++){
    const x=s.slice(b*4,b*4+4)
    const t0=(x[0]+x[1])%P, t1=(x[2]+x[3])%P, t2=(2n*x[1]+t1)%P, t3=(2n*x[3]+t0)%P
    const t4=(4n*t1+t3)%P, t5=(4n*t0+t2)%P, t6=(t3+t5)%P, t7=(t2+t4)%P
    const o=[t6,t5,t7,t4]
    for(let j=0;j<4;j++){t[j]=(t[j]+o[j])%P;out[b*4+j]=o[j]}
  }
  for(let i=0;i<C;i++)out[i]=(out[i]+t[i%4])%P
  return out
}
const mInt = s => { const sum=s.reduce((a,x)=>(a+x)%P,0n); return s.map((v,i)=>(sum+BigInt(di[i])*v)%P) }
const input = Array.from({length:C},(_,i)=>BigInt(i))
const r0 = s => { const o=s.map((v,i)=>(v+BigInt(rc[i]))%P).map(sbox); return mExt(o) }
const rp = s => { const o=[...s]; o[0]=(o[0]+BigInt(rc[4*C]))%P; o[0]=sbox(o[0]); return mInt(o) }
let uid = 0
async function run (method, fn) {
  prov.addUtxo('1Q1pE5vPGEEMqRcVRMbtBK842Y6Pzo6nK9', { txid: (uid++).toString(16).padStart(64,'0'), outputIndex: 0, satoshis: 1e6, script: '' })
  const c = new RunarContract(art, []); c.connect(prov, signer); await c.deploy({ satoshis: 1 })
  const exp = fn(input).slice(0,8)
  await c.call(method, [...input, ...exp])
  const hexes = prov.getBroadcastedTxs()
  const spendTx = Transaction.fromHex(hexes.at(-1)); const deployTx = Transaction.fromHex(hexes.at(-2))
  const spend = new Spend({ sourceTXID: deployTx.id('hex'), sourceOutputIndex: 0, sourceSatoshis: 1,
    lockingScript: deployTx.outputs[0].lockingScript, transactionVersion: spendTx.version,
    otherInputs: spendTx.inputs.slice(1), outputs: spendTx.outputs, inputIndex: 0,
    unlockingScript: spendTx.inputs[0].unlockingScript, inputSequence: spendTx.inputs[0].sequence ?? 0xffffffff, lockTime: spendTx.lockTime ?? 0, isRelaxed: true })
  try { spend.validate(); return `VALID ops:${spend.executedOpCount} peakStack:${spend.stackMem}B` } catch(e){ return 'FAIL '+e.message.slice(0,70) }
}
console.log('mExt        :', await run('mExt', mExt))
console.log('mInt        :', await run('mInt', mInt))
console.log('sboxesFull  :', await run('sboxesFull', s=>s.map(sbox)))
console.log('fullRound   :', await run('fullRound', r0))
console.log('partialRound:', await run('partialRound', rp))
