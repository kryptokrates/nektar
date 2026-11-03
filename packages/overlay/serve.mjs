#!/usr/bin/env node
// nektar rendezvous — a public overlay node for agent discovery.
//
//   GET  /health                       { ok, agents, uptime }
//   GET  /directory?all=1              every live ad
//   GET  /directory?shard=<id>         agents serving that shard
//   GET  /directory?pub=<66hex>        one agent's ad
//   POST /announce     {ad,sig}        off-chain signed ad — free, no UTXO
//   POST /overlay/submit {beef,topics} on-chain path → tm_nektar / tm_presence
//
// An ad = { pub, endpoint, shards[], ts, exp? } signed by the agent's key over
// sha256('endpoint|shards|ts|exp'). Latest-by-ts wins; expired ads are hidden.
// The directory only finds peers — trust still comes from receipts, not us.
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
import { createNektarOverlay, verifyPresence } from './dist/src/index.js'

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
      const query = url.searchParams.get('all') !== null ? { all: true }
        : url.searchParams.get('shard') ? { shard: url.searchParams.get('shard') }
        : url.searchParams.get('pub') ? { pub: url.searchParams.get('pub') }
        : {}
      const out = await overlay.lsPresence.lookup({ query })
      const ads = out.map((o) => ({ ...decode(o), txid: o.txid })).filter((a) => a.pub)
      return json(res, 200, { ads })
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
        directory: 'GET /directory?all=1 | ?shard=<id> | ?pub=<66hex>',
        onchain: 'POST /overlay/submit {beef,topics:["tm_presence"]}'
      })
    }
    json(res, 404, { error: 'not found' })
  } catch (e) {
    json(res, 500, { error: String(e?.message ?? e) })
  }
})
server.listen(PORT, HOST)
