import { readFileSync } from 'node:fs'
const env = Object.fromEntries(readFileSync('/etc/nektar-http.env','utf8').split('\n').filter(l=>l.includes('=')).map(l=>l.split('=',2).map(s=>s.trim())))
const BASE='http://127.0.0.1:8080', TOK=env.NEKTAR_HTTP_TOKEN
const H={'content-type':'application/json', authorization:`Bearer ${TOK}`}
const call=async(t,a={})=>{const r=await fetch(`${BASE}/v1/tools/${encodeURIComponent(t)}`,{method:'POST',headers:H,body:JSON.stringify(a)});const j=await r.json().catch(()=>({}));return{status:r.status,body:j}}
const sale=(op,a={})=>call('sale',{op,...a})
let pass=0,fail=0;const ok=(c,n,x)=>{if(c){pass++;console.log('  ok —',n)}else{fail++;console.log('  FAIL —',n,JSON.stringify(x??'').slice(0,220))}}
const myPub='02223b8f0f3fd299a9b2eec9b592e8c0b81539ea7ebc2f55b266ea1b70d67df5b8'

console.log('== S2: escrow → early refund refused → refund after locktime ==')
const rec=await call('remember',{shard:'video-demo',data:'the answer is 42',level:0})
ok(rec.status===200,'remember',rec.body)
const offer=await sale('offerMemory',{shard:'video-demo',seq:rec.body.seq,price:100,refundAfter:Math.floor(Date.now()/1000)+30,maxFee:500,buyerPub:myPub})
ok(offer.status===200,'offerMemory',offer.body)
const esc=await sale('escrow',{cfg:offer.body.cfg,amount:100,offer:offer.body})
ok(esc.status===200&&esc.body.accepted,'escrow funded on testnet',esc.body)
console.log('  escrow txid:',esc.body.txid)
const rfArgs={cfg:esc.body.escrow??offer.body.cfg,utxo:esc.body.utxo,lockTime:offer.body.cfg.refundAfter,changeSats:0}
const r1=await sale('refund',rfArgs)
ok(r1.status!==200||r1.body.accepted!==true,'early refund refused',r1.body)
console.log('  waiting 200s for locktime')
await new Promise(r=>setTimeout(r,200_000))
const r2=await sale('refund',rfArgs)
ok(r2.status===200&&r2.body.accepted===true,'refund after locktime accepted',r2.body)
console.log('  refund txid:',r2.body.txid)

console.log('== S1: paid sale completes ==')
const o2=await sale('offerMemory',{shard:'video-demo',seq:rec.body.seq,price:100,refundAfter:Math.floor(Date.now()/1000)+3600,maxFee:500,buyerPub:myPub})
const e2=await sale('escrow',{cfg:o2.body.cfg,amount:100,offer:o2.body})
ok(e2.status===200&&e2.body.accepted,'escrow 2 funded',e2.body)
const comp=await sale('complete',{cfg:e2.body.escrow??o2.body.cfg,utxo:e2.body.utxo,wrapped:o2.body.wrapped,changeSats:0})
ok(comp.status===200&&comp.body.accepted,'sale complete — disclosure tx',comp.body)
const claim=await sale('claim',{txHex:comp.body.txHex,hWrapped:o2.body.cfg.hWrapped,sellerPub:o2.body.sellerPub,commitment:o2.body.commitment,offerMode:o2.body.mode})
ok(claim.status===200,'claim verified',claim.body)
console.log('  claimed value:',claim.body.verified?'verified':'?', 'decrypted:',claim.body.decrypted)

console.log(`\n${pass} passed, ${fail} failed`)
process.exit(fail===0?0:1)
