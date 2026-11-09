#!/usr/bin/env node
// nektar rendezvous — a public overlay node for agent discovery.
//
//   GET  /health                       { ok, agents, uptime }
//   GET  /directory?all=1              every live ad (+ catalog when posted)
//   GET  /directory?shard=<id>         agents serving that shard
//   GET  /directory?pub=<66hex>        one agent's ad
//   GET  /directory?domain=<tag>       signed catalogs claiming that domain
//   GET  /directory?q=<text>           free-text over catalog domains+descs
//   POST /announce     {ad,sig}        off-chain signed ad — free, no UTXO
//   POST /catalog      {catalog,sig}   off-chain signed "what I know" index
//   POST /overlay/submit {beef,topics} on-chain path → tm_nektar / tm_presence
//
// An ad = { pub, endpoint, shards[], ts, exp? } signed by the agent's key over
// sha256('endpoint|shards|ts|exp'). Latest-by-ts wins; expired ads are hidden.
// A catalog = { pub, endpoint?, shards:[{shard,shardId?,domains[],desc?,offers?}], ts }
// signed over sha256('pub|ts|stableJson(shards)') — the agent's signed "what I
// know" claim; /directory?domain=<tag> and ?q=<text> search it. The directory
// only finds peers — trust still comes from receipts, not us.
//
// env: NEKTAR_OVERLAY_HOST (default 0.0.0.0)  NEKTAR_OVERLAY_PORT (default 8090)
//      NEKTAR_OVERLAY_DIR  (sqlite dir, default ./.state/overlay)
//      NEKTAR_NETWORK      (testnet|ttn|main — for on-chain submits; default testnet)
//      NEKTAR_OVERLAY_TOKEN (optional bearer — gates writes if set)
import http from 'node:http'
import path from 'node:path'
import fs from 'node:fs'
import knexPkg from 'knex'
import { ArcBroadcaster, ChaintracksTracker } from '@nektar/chain'
import { createNektarOverlay, verifyPresence, verifyCatalog, verifyWant, verifyOffer, verifySettlement, verifyReputationCard, verifyDelegation, verifyAttestation } from './dist/src/index.js'

const HOST = process.env.NEKTAR_OVERLAY_HOST ?? '0.0.0.0'
const PORT = Number(process.env.NEKTAR_OVERLAY_PORT ?? 8090)
const DIR = process.env.NEKTAR_OVERLAY_DIR ?? path.join(process.cwd(), '.state', 'overlay')
const TOKEN = process.env.NEKTAR_OVERLAY_TOKEN
const NET = process.env.NEKTAR_NETWORK ?? 'testnet'
// No WhatsOnChain anywhere: tracker = chaintracks only (no WocTracker
// fallback); broadcaster's post-ARC fallback is pointed back at the same
// arcade (self-referential retry — a 404 there just ends in 'broadcast failed').
const URLS = {
  testnet: { arc: 'https://arcade-v2-testnet-us-1.bsvblockchain.tech', ct: 'https://arcade-v2-testnet-us-1.bsvblockchain.tech/chaintracks/v1' },
  ttn:     { arc: 'https://arcade-v2-ttn-us-1.bsvblockchain.tech',    ct: 'https://arcade-v2-ttn-us-1.bsvblockchain.tech/chaintracks/v1' },
  main:    { arc: 'https://arcade-us-1.bsvblockchain.tech',           ct: 'https://arcade-us-1.bsvblockchain.tech/chaintracks/v2' }
}
const u = URLS[NET] ?? URLS.testnet
fs.mkdirSync(DIR, { recursive: true })

const overlay = await createNektarOverlay(
  new ChaintracksTracker({ arcUrl: u.arc, wocUrl: u.arc }, u.ct),
  new ArcBroadcaster({ arcUrl: u.arc, wocUrl: u.arc }),
  knexPkg({ client: 'sqlite3', connection: { filename: path.join(DIR, 'overlay.sqlite') }, useNullAsDefault: true })
)
console.log(`[rendezvous] network=${NET} db=${path.join(DIR, 'overlay.sqlite')} listening ${HOST}:${PORT}`)

const json = (res, code, obj) => { res.writeHead(code, { 'content-type': 'application/json', 'access-control-allow-origin': '*' }); res.end(JSON.stringify(obj)) }
const body = (req) => new Promise((ok, no) => { let d = ''; req.on('data', c => { d += c; if (d.length > 1e6) no(new Error('too large')) }); req.on('end', () => ok(d)); req.on('error', no) })
const gated = (req) => !TOKEN || req.headers.authorization === `Bearer ${TOKEN}`
const decode = (o) => { try { return JSON.parse(Buffer.from(o.context ?? []).toString()) } catch { return null } }

const t0 = Date.now()
const server = http.createServer(async (req, res) => {
  try {
    const url = new URL(req.url ?? '/', `http://${req.headers.host ?? 'x'}`)
    if (req.method === 'GET' && url.pathname === '/health') {
      const dir = await overlay.lsPresence.lookup({ query: { all: true } })
      return json(res, 200, { ok: true, agents: dir.length, uptime: Math.floor((Date.now() - t0) / 1000) })
    }
    // NOTE: engine.lookup would hydrate each result into a BEEF from storage —
    // off-chain ads have no UTXO, so the directory queries ls_presence directly.
    if (req.method === 'GET' && url.pathname === '/directory') {
      // content queries return signed catalogs; join with each claimant's ad
      const dom = url.searchParams.get('domain')
      const freetext = url.searchParams.get('q')
      if (dom || freetext) {
        const out = await overlay.lsPresence.lookup({ query: dom ? { domain: dom } : { q: freetext } })
        const results = out.map((o) => {
          const sc = decode(o)
          const adQ = overlay.lsPresence.lookup({ query: { pub: sc?.catalog?.pub } })
          return adQ.then(([a]) => ({ sc, ad: a ? decode(a) : undefined }))
        })
        const ads = (await Promise.all(results)).map(({ sc, ad }) => ({
          ...(ad ?? {}), pub: sc?.catalog?.pub, endpoint: ad?.endpoint ?? sc?.catalog?.endpoint,
          catalog: sc?.catalog, catalogSig: sc?.sig
        })).filter((a) => a.pub)
        return json(res, 200, { ads })
      }
      const query = url.searchParams.get('all') !== null ? { all: true }
        : url.searchParams.get('shard') ? { shard: url.searchParams.get('shard') }
        : url.searchParams.get('pub') ? { pub: url.searchParams.get('pub') }
        : {}
      const out = await overlay.lsPresence.lookup({ query })
      const ads = out.map((o) => ({ ...decode(o), txid: o.txid })).filter((a) => a.pub)
        .map((a) => { const sc = overlay.lsPresence.catalogFor(a.pub); return sc ? { ...a, catalog: sc.catalog, catalogSig: sc.sig } : a })
      return json(res, 200, { ads })
    }
    if (req.method === 'POST' && url.pathname === '/catalog') {
      if (!gated(req)) return json(res, 401, { error: 'unauthorized' })
      const { catalog, sig } = JSON.parse(await body(req) || '{}')
      if (!catalog || !sig || !verifyCatalog(catalog, sig)) return json(res, 400, { error: 'invalid catalog or signature' })
      const ok = await overlay.lsPresence.announceCatalog({ catalog, sig })
      return json(res, ok ? 200 : 400, ok ? { ok: true, pub: catalog.pub } : { error: 'rejected' })
    }
    if (req.method === 'POST' && url.pathname === '/want') {
      if (!gated(req)) return json(res, 401, { error: 'unauthorized' })
      const { want, sig } = JSON.parse(await body(req) || '{}')
      if (!want || !sig || !verifyWant(want, sig)) return json(res, 400, { error: 'invalid want or signature' })
      const ok = await overlay.lsPresence.announceWant({ want, sig })
      return json(res, ok ? 200 : 400, ok ? { ok: true, pub: want.pub } : { error: 'rejected' })
    }
    if (req.method === 'POST' && url.pathname === '/offer') {
      if (!gated(req)) return json(res, 401, { error: 'unauthorized' })
      const { offer, sig } = JSON.parse(await body(req) || '{}')
      if (!offer || !sig || !verifyOffer(offer, sig)) return json(res, 400, { error: 'invalid offer or signature' })
      const ok = await overlay.lsPresence.announceOffer({ offer, sig })
      return json(res, ok ? 200 : 400, ok ? { ok: true, pub: offer.pub } : { error: 'rejected' })
    }
    // the live shelf: signed offers, optionally narrowed by domain tag
    if (req.method === 'GET' && url.pathname === '/offers') {
      const domain = url.searchParams.get('domain') ?? undefined
      return json(res, 200, { offers: overlay.lsPresence.offersFor(domain) })
    }
    if (req.method === 'POST' && url.pathname === '/settlement') {
      if (!gated(req)) return json(res, 401, { error: 'unauthorized' })
      const { ad, sig } = JSON.parse(await body(req) || '{}')
      if (!ad || !sig || !verifySettlement(ad, sig)) return json(res, 400, { error: 'invalid settlement or signature' })
      const ok = await overlay.lsPresence.announceSettlement({ ad, sig })
      return json(res, ok ? 200 : 400, ok ? { ok: true, pub: ad.pub } : { error: 'rejected' })
    }
    if (req.method === 'POST' && url.pathname === '/reputation') {
      if (!gated(req)) return json(res, 401, { error: 'unauthorized' })
      const { card, sig } = JSON.parse(await body(req) || '{}')
      if (!card || !sig || !verifyReputationCard(card, sig)) return json(res, 400, { error: 'invalid card or signature' })
      const ok = await overlay.lsPresence.announceReputation({ card, sig })
      return json(res, ok ? 200 : 400, ok ? { ok: true, pub: card.pub } : { error: 'rejected' })
    }
    // realized clearing prices — signed settlement announcements indexed
    // per (domain, tool); the txids are independently checkable on-chain
    if (req.method === 'GET' && url.pathname === '/prices') {
      return json(res, 200, overlay.lsPresence.prices({ domain: url.searchParams.get('domain') ?? undefined, tool: url.searchParams.get('tool') ?? undefined }))
    }
    // multidimensional reputation — per-domain vectors, never one score
    if (req.method === 'GET' && url.pathname === '/reputation') {
      const pub = url.searchParams.get('pub'), domain = url.searchParams.get('domain') ?? undefined
      if (pub) return json(res, 200, { reputation: overlay.lsPresence.reputationFor(pub) ?? null })
      return json(res, 200, { reputations: overlay.lsPresence.reputationsFor(domain) })
    }
    if (req.method === 'POST' && url.pathname === '/delegation') {
      if (!gated(req)) return json(res, 401, { error: 'unauthorized' })
      const { grant, sig } = JSON.parse(await body(req) || '{}')
      if (!grant || !sig || !verifyDelegation(grant, sig)) return json(res, 400, { error: 'invalid delegation or signature' })
      const ok = await overlay.lsPresence.announceDelegation({ grant, sig })
      return json(res, ok ? 200 : 400, ok ? { ok: true, grantId: grant.grantId } : { error: 'rejected' })
    }
    if (req.method === 'GET' && url.pathname === '/delegation') {
      return json(res, 200, { delegations: overlay.lsPresence.delegationsFor(url.searchParams.get('grantee') ?? undefined) })
    }
    if (req.method === 'POST' && url.pathname === '/delegation/revoke') {
      if (!gated(req)) return json(res, 401, { error: 'unauthorized' })
      const { grantId, pub, ts } = JSON.parse(await body(req) || '{}')
      if (!grantId || !pub) return json(res, 400, { error: 'grantId and pub required' })
      const ok = await overlay.lsPresence.revokeDelegation({ grantId, pub, ts: Number(ts ?? Date.now()) })
      return json(res, ok ? 200 : 400, ok ? { ok: true, grantId } : { error: 'not the owner or unknown grant' })
    }
    if (req.method === 'POST' && url.pathname === '/attestation') {
      if (!gated(req)) return json(res, 401, { error: 'unauthorized' })
      const { attestation, sig } = JSON.parse(await body(req) || '{}')
      if (!attestation || !sig || !verifyAttestation(attestation, sig)) return json(res, 400, { error: 'invalid attestation or signature' })
      const ok = await overlay.lsPresence.announceAttestation({ attestation, sig })
      return json(res, ok ? 200 : 400, ok ? { ok: true, pub: attestation.pub } : { error: 'rejected' })
    }
    if (req.method === 'GET' && url.pathname === '/attestation') {
      return json(res, 200, { attestations: overlay.lsPresence.attestationsFor(url.searchParams.get('pub') ?? undefined) })
    }
    // JetStream-pull feed: consumers hold a cursor (last seen seq) and pull
    // new events, filtered by kind and domain tags. ?wait=<ms> long-polls
    // for near-real-time delivery without websockets or webhooks.
    if (req.method === 'GET' && url.pathname === '/events') {
      const since = url.searchParams.get('since') !== null ? Number(url.searchParams.get('since')) : undefined
      const domains = url.searchParams.get('domains')?.split(',').map(s => s.trim()).filter(Boolean)
      const kind = url.searchParams.get('kind') ?? undefined
      const limit = url.searchParams.get('limit') !== null ? Math.min(Number(url.searchParams.get('limit')), 1000) : 200
      const wait = Math.min(Number(url.searchParams.get('wait') ?? 0), 30_000)
      const deadline = Date.now() + wait
      for (;;) {
        const events = await overlay.lsPresence.feed({ since, domains, kind: kind ?? undefined, limit })
        if (events.length || Date.now() >= deadline) {
          return json(res, 200, { events, cursor: events.length ? events.at(-1).seq : (since ?? -1) })
        }
        await new Promise(r => setTimeout(r, 250))
      }
    }
    // GASP sync (BRC-88 propagation path): lets a peer engine pull topical
    // outputs directly — the same routes overlay-express exposes. The Hetzner
    // node is a real overlay peer: agents' own overlays replicate tm_nektar
    // anchors (and any on-chain tm_presence objects) from it.
    if (req.method === 'POST' && url.pathname === '/requestSyncResponse') {
      const { request, topic } = JSON.parse(await body(req) || '{}')
      return json(res, 200, await overlay.engine.provideForeignSyncResponse(request, topic))
    }
    if (req.method === 'POST' && url.pathname === '/requestForeignGASPNode') {
      const { graphID, txid, outputIndex, topic } = JSON.parse(await body(req) || '{}')
      return json(res, 200, await overlay.engine.provideForeignGASPNode(graphID, txid, outputIndex, topic))
    }
    if (req.method === 'POST' && url.pathname === '/announce') {
      if (!gated(req)) return json(res, 401, { error: 'unauthorized' })
      const { ad, sig } = JSON.parse(await body(req) || '{}')
      if (!ad || !sig || !verifyPresence(ad, sig)) return json(res, 400, { error: 'invalid ad or signature' })
      const ok = await overlay.lsPresence.announce({ ad, sig })
      return json(res, ok ? 200 : 400, ok ? { ok: true, pub: ad.pub } : { error: 'rejected' })
    }
    if (req.method === 'POST' && url.pathname === '/overlay/submit') {
      if (!gated(req)) return json(res, 401, { error: 'unauthorized' })
      const { beef, topics } = JSON.parse(await body(req) || '{}')
      if (!Array.isArray(beef) || !Array.isArray(topics)) return json(res, 400, { error: 'need {beef:number[],topics:string[]}' })
      const st = await overlay.engine.submit({ beef, topics })
      return json(res, 200, st)
    }
    if (req.method === 'GET' && url.pathname === '/events/stream') {
      res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache', connection: 'keep-alive', 'access-control-allow-origin': '*' })
      res.write(': connected\n\n')
      const push = (e) => { try { res.write(`data: ${JSON.stringify(e)}\n\n`) } catch {} }
      overlay.lsPresence.bus?.on('event', push)
      const beat = setInterval(() => { try { res.write(': beat\n\n') } catch {} }, 15000)
      req.on('close', () => { clearInterval(beat); overlay.lsPresence.bus?.off('event', push) })
      return
    }
    if (req.method === 'GET' && url.pathname === '/dashboard') {
      res.writeHead(200, { 'content-type': 'text/html; charset=utf-8' })
      return res.end(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>nektar market — live</title>
<style>
body{background:#0d0d0d;color:#eaeaea;font:14px/1.6 ui-monospace,monospace;max-width:920px;margin:0 auto;padding:40px 20px}
h1{font-size:22px;margin:0 0 4px;font-weight:600}.dot{display:inline-block;width:8px;height:8px;border-radius:50%;background:#4cc26b;margin-left:8px}
.sub{color:#888;font-size:13px;margin-bottom:32px}
.grid{display:grid;grid-template-columns:repeat(4,1fr);gap:14px;margin-bottom:24px}
.card{background:#161616;border:1px solid #262626;border-radius:10px;padding:16px 18px}
.card .n{font-size:24px;color:#f5b301;font-weight:600}.card .l{font-size:11px;letter-spacing:.12em;color:#888;text-transform:uppercase}
h2{font-size:11px;letter-spacing:.14em;color:#888;text-transform:uppercase;margin:20px 0 10px}
.ev{display:flex;gap:12px;padding:8px 0;border-bottom:1px solid #1f1f1f;font-size:13px}
.ev .k{width:88px;flex-shrink:0}.ev .seq{color:#555;width:40px;flex-shrink:0}
.k-ad{color:#9ecbff}.k-catalog{color:#b78aff}.k-want{color:#fb923c}.k-offer{color:#4cc26b}
.k-settlement{color:#f5b301}.k-fraud{color:#e5534b}.k-delegation{color:#56d4dd}.k-attestation{color:#e6b3e6}
.dim{color:#777}.agents div{padding:4px 0;font-size:12px;color:#999}
.netbadge{font-size:11px;font-weight:600;letter-spacing:.1em;padding:3px 10px;border-radius:20px;margin-left:12px;vertical-align:middle;text-transform:uppercase}
.netbadge.testnet{background:#3a2a00;color:#f5b301;border:1px solid #f5b301}
.netbadge.mainnet{background:#062e0c;color:#4cc26b;border:1px solid #4cc26b}
.toggle{margin-left:14px}.toggle button{background:#1a1a1a;border:1px solid #2a2a2a;color:#888;font:inherit;font-size:11px;padding:2px 10px;border-radius:20px;cursor:pointer;margin-right:4px}
.toggle button.on{background:#f5b301;color:#0d0d0d;border-color:#f5b301}
footer{margin-top:24px;color:#555;font-size:12px}
</style></head><body>
<h1>nektar market<span id="netbadge" class="netbadge testnet">testnet</span><span class="dot" id="live"></span></h1>
<div class="sub" id="net">live event feed · the agent memory market
<span class="toggle"><button id="t-ttn" class="on" onclick="setNet('ttn')">testnet</button><button id="t-main" onclick="setNet('main')">mainnet</button></span>
<span class="dim" id="rv-host"></span></div>
<div class="grid" id="stats">
<div class="card"><div class="n" id="n-want">0</div><div class="l">wants</div></div>
<div class="card"><div class="n" id="n-offer">0</div><div class="l">offers</div></div>
<div class="card"><div class="n" id="n-agent">0</div><div class="l">agents</div></div>
<div class="card"><div class="n" id="n-settle">0</div><div class="l">settled</div></div>
</div>
<h2>Live feed</h2><div id="feed"><div class="dim">listening…</div></div>
<h2>Agents on this index</h2><div class="agents" id="agents"><div class="dim">none yet</div></div>
<footer>every event is a signed artifact — pub + sig verifiable against the payload</footer>
<script>
const $ = id => document.getElementById(id)
// network sources — same-origin by default; override via ?ttn= & ?main=
const q=new URLSearchParams(location.search)
const SERVER_NET='${process.env.NEKTAR_NETWORK ?? 'ttn'}'
const NETS={ttn:q.get('ttn')||location.origin,main:q.get('main')||'https://rv.nektar.run'}
let BASE=NETS.ttn, NET='ttn', es=null
const counts = {}
const seen = new Set()
let cursor = -1
function setNet(n){
  NET=n; BASE=NETS[n]||location.origin
  document.querySelectorAll('.toggle button').forEach(b=>b.classList.remove('on'))
  $('t-'+n).classList.add('on'); $('rv-host').textContent=' · '+BASE
  const nb=$('netbadge'); nb.textContent=n==='main'?'mainnet':'testnet'; nb.className='netbadge '+(n==='main'?'mainnet':'testnet')
  $('feed').innerHTML='<div class="dim">listening…</div>'; seen.clear(); cursor=-1
  if(es){es.close();es=null}
  for(const k of ['want','offer','agent','settle']) $('n-'+k).textContent='0'
  for(const k in counts) delete counts[k]
  boot()
}
const bump = k => { counts[k] = (counts[k]||0)+1; const el=$('n-'+k); if(el) el.textContent=counts[k] }
const esc = s => String(s??'').replace(/[&<>]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;'}[c]))
function row(e){
  const pub=(e.pub||'').slice(0,10), dom=(e.domains||[]).join(','), p=e.payload||{}
  let extra=''
  if(e.kind==='want'&&p.want?.maxSats!=null) extra='≤'+p.want.maxSats+' sats'
  if(e.kind==='offer'&&p.offer?.priceSats!=null) extra=p.offer.priceSats+' sats'
  return '<div class="ev"><span class="seq">#'+e.seq+'</span><span class="k k-'+e.kind+'">'+esc(e.kind)+'</span>'+
    '<span class="dim">'+esc(dom)+'</span><span class="dim">'+esc(pub)+'…</span><span class="dim">'+esc(extra)+'</span>'+
    '<span class="dim" style="margin-left:auto">'+new Date(e.ts||Date.now()).toLocaleTimeString()+'</span></div>'
}
function feed(evs){
  const box=$('feed'); if(box.querySelector('.dim')) box.innerHTML=''
  for(const e of evs){ if(seen.has(e.seq)) continue; seen.add(e.seq); cursor=Math.max(cursor,e.seq)
    bump(e.kind); box.insertAdjacentHTML('afterbegin',row(e)) }
  while(box.children.length>40) box.lastChild.remove()
}
async function boot(){
  const j=await (await fetch(BASE+'/events?since=-30&limit=50')).json().catch(()=>({}))
  feed(j.events||[])
  es=new EventSource(BASE+'/events/stream')
  es.onmessage=m=>{ try{ feed([JSON.parse(m.data)]) }catch{} }
  es.onopen=()=>{ $('live').style.background='#4cc26b' }
  es.onerror=()=>{ $('live').style.background='#e5534b' }
  agents()
}
async function agents(){
  try{ const j=await (await fetch(BASE+'/directory?all=1')).json()
    const ads=j.ads??j.entries??[]
    $('n-agent').textContent=ads.length||0
    $('agents').innerHTML=ads.length?ads.map(a=>'<div>'+esc((a.pub||'').slice(0,14))+'… · '+esc((a.domains||a.shards||[]).join(', '))+'</div>').join(''):'<div class="dim">none yet</div>'
  }catch{}
}
$('rv-host').textContent=' · '+BASE
setNet(SERVER_NET==='main'||SERVER_NET==='mainnet'?'main':'ttn')
setInterval(agents,30000)
</script></body></html>`)
    }
    if (req.method === 'GET' && url.pathname === '/') {
      return json(res, 200, {
        service: 'nektar rendezvous (tm_presence/ls_presence)',
        announce: 'POST /announce {ad:{pub,endpoint,shards,ts,exp?},sig} — sig = BSV sig over sha256("endpoint|shards|ts|exp")',
        catalog: 'POST /catalog {catalog:{pub,endpoint?,shards:[{shard,shardId?,domains,desc?,offers?}],ts},sig} — the signed "what I know" index',
        want: 'POST /want {want:{pub,domains,question?,maxSats?,endpoint?,ts,exp?},sig} — signed demand ad',
        events: 'GET /events?since=<cursor>&domains=a,b&kind=ad|catalog|want&limit=<n>&wait=<ms> — durable pull feed',
        directory: 'GET /directory?all=1 | ?shard=<id> | ?pub=<66hex> | ?domain=<tag> | ?q=<text>',
        gasp: 'POST /requestSyncResponse | /requestForeignGASPNode — engine-level replication for peer overlays',
        onchain: 'POST /overlay/submit {beef,topics:["tm_presence"]}'
      })
    }
    json(res, 404, { error: 'not found' })
  } catch (e) {
    json(res, 500, { error: String(e?.message ?? e) })
  }
})
server.listen(PORT, HOST)
