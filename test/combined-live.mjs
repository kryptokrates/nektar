import { readFileSync } from 'node:fs'
import { execSync } from 'node:child_process'
const env = Object.fromEntries(readFileSync('/etc/nektar-http.env','utf8').split('\n').filter(l=>l.includes('=')).map(l=>l.split('=',2).map(s=>s.trim())))
const H={'content-type':'application/json', authorization:`Bearer ${env.NEKTAR_HTTP_TOKEN}`}
const http=async(url,o={})=>{const r=await fetch(url,{headers:H,...o});const j=await r.json().catch(()=>({}));return{status:r.status,body:j}}
const call=async(t,a={})=>http(`http://127.0.0.1:8080/v1/tools/${encodeURIComponent(t)}`,{method:'POST',body:JSON.stringify(a)})
const sale=(op,a={})=>call('sale',{op,...a})
let pass=0,fail=0;const ok=(c,n,x)=>{if(c){pass++;console.log('  ok —',n)}else{fail++;console.log('  FAIL —',n,JSON.stringify(x??'').slice(0,200))}}
const myPub='02223b8f0f3fd299a9b2eec9b592e8c0b81539ea7ebc2f55b266ea1b70d67df5b8'

const rec=await call('remember',{shard:'video-demo',data:'failover sale record',level:0})
ok(rec.status===200,'remember',rec.body)
const offer=await sale('offerMemory',{shard:'video-demo',seq:rec.body.seq,price:100,refundAfter:Math.floor(Date.now()/1000)+3600,maxFee:500,buyerPub:myPub})
ok(offer.status===200,'offerMemory',offer.body)

const se=await call('sendEnvelope',{keeperUrl:'http://127.0.0.1:8091',recipient:myPub,kind:'offer',payload:{kind:'offer',offer:offer.body}})
ok(se.status===200&&se.body.ok!==false,'envelope sent via rv :8091',se.body)

console.log('  waiting for :8092 to pull the tail …')
await new Promise(r=>setTimeout(r,12_000))
console.log('  killing rv :8091 …')
execSync("kill $(ss -tlnp | grep ':8091' | awk -F'pid=' '{print $2}' | cut -d, -f1 | head -1) || true")
const kill=await http('http://127.0.0.1:8091/peers').catch(()=>({status:0}))
ok(kill.status===0,'rv :8091 down — copy survives on :8092',kill.status)

const col=await call('collect',{keeperUrl:'http://127.0.0.1:8092'})
ok(col.status===200,'collect from rv :8092',col.body)
const items=Array.isArray(col.body)?col.body:(col.body.envelopes??col.body.items??[])
const got=items.find(e=>e.kind==='offer'||JSON.stringify(e.payload??{}).includes('offer'))
ok(!!got,`offer envelope recovered via :8092 (${items.length} envelopes)`,'')

const e2=await sale('escrow',{cfg:offer.body.cfg,amount:100,offer:offer.body})
ok(e2.status===200&&e2.body.accepted,'escrow funded on testnet',e2.body)
console.log('  escrow txid:',e2.body.txid)
const comp=await sale('complete',{cfg:e2.body.escrow??offer.body.cfg,utxo:e2.body.utxo,wrapped:offer.body.wrapped,changeSats:0})
ok(comp.status===200&&comp.body.accepted,'sale complete — disclosure tx',comp.body)
const claim=await sale('claim',{txHex:comp.body.txHex,hWrapped:offer.body.cfg.hWrapped,sellerPub:offer.body.sellerPub,commitment:offer.body.commitment,offerMode:offer.body.mode})
ok(claim.status===200&&claim.body.verified===true,'claim verified + decrypted',claim.body)
console.log(`\n${pass} passed, ${fail} failed`)
process.exit(fail===0?0:1)
