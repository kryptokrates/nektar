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
import { createNektarOverlay, verifyPresence, verifyCatalog, verifyWant, verifyOffer } from './dist/src/index.js'

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

const json = (res, code, obj) => { res.writeHead(code, { 'content-type': 'application/json' }); res.end(JSON.stringify(obj)) }
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
