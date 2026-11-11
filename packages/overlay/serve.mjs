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
import { fileURLToPath } from 'node:url'
const __dirname = path.dirname(fileURLToPath(import.meta.url))
import knexPkg from 'knex'
import { BsvBroadcaster, ChaintracksTracker } from '@nektar/chain'
import { PrivateKey } from '@bsv/sdk'
import { createNektarOverlay, verifyPresence, verifyCatalog, verifyWant, verifyOffer, verifySettlement, verifyReputationCard, verifyDelegation, verifyAttestation, signBeacon, signInterval, deliveryRecord, eventEnvelope, KIND_PATTERN } from './dist/src/index.js'
import { sha256 } from '@nektar/core'

const HOST = process.env.NEKTAR_OVERLAY_HOST ?? '0.0.0.0'
const PORT = Number(process.env.NEKTAR_OVERLAY_PORT ?? 8090)
const DIR = process.env.NEKTAR_OVERLAY_DIR ?? path.join(process.cwd(), '.state', 'overlay')
const TOKEN = process.env.NEKTAR_OVERLAY_TOKEN
const NET = process.env.NEKTAR_NETWORK ?? 'testnet'
// No WhatsOnChain anywhere: tracker = chaintracks only (no WocTracker
// fallback); the WoC broadcast fallback is pointed back at the same
// broadcaster (self-referential retry — a 404 there just ends in 'broadcast failed').
const URLS = {
  testnet: { broadcaster: 'https://arcade-v2-testnet-us-1.bsvblockchain.tech', ct: 'https://arcade-v2-testnet-us-1.bsvblockchain.tech/chaintracks/v2' },
  ttn:     { broadcaster: 'https://arcade-v2-ttn-us-1.bsvblockchain.tech',    ct: 'https://arcade-v2-ttn-us-1.bsvblockchain.tech/chaintracks/v2' },
  main:    { broadcaster: 'https://arcade-us-1.bsvblockchain.tech',           ct: 'https://arcade-us-1.bsvblockchain.tech/chaintracks/v2' }
}
const u = URLS[NET] ?? URLS.testnet
fs.mkdirSync(DIR, { recursive: true })

const overlay = await createNektarOverlay(
  new ChaintracksTracker({ broadcasterUrl: u.broadcaster, wocUrl: u.broadcaster }, u.ct),
  new BsvBroadcaster({ broadcasterUrl: u.broadcaster, wocUrl: u.broadcaster }),
  knexPkg({ client: 'sqlite3', connection: { filename: path.join(DIR, 'overlay.sqlite') }, useNullAsDefault: true }),
  // strict by default — only SPV-verified (net+tip-bound) artifacts land on the
  // feed; NEKTAR_OVERLAY_STRICT_NET=0 relaxes for dev/compat
  { net: NET, strictNet: process.env.NEKTAR_OVERLAY_STRICT_NET !== '0',
    maxAgeMs: Number(process.env.NEKTAR_OVERLAY_MAX_AGE_MS ?? 86_400_000),
    // bstore-style: exp beyond the free window costs rateSatsPerHour per hour
    listingFee: process.env.NEKTAR_LISTING_ADDR ? {
      addr: process.env.NEKTAR_LISTING_ADDR,
      rateSatsPerHour: Number(process.env.NEKTAR_LISTING_RATE_SATS_PER_HOUR ?? 10),
      freeWindowMs: Number(process.env.NEKTAR_LISTING_FREE_WINDOW_MS ?? 3_600_000)
    } : undefined }
)
const SELF_KEY = process.env.NEKTAR_OVERLAY_WIF ? PrivateKey.fromWif(process.env.NEKTAR_OVERLAY_WIF) : PrivateKey.fromRandom()
const SELF_PUB = SELF_KEY.toPublicKey().toString()
const SELF_URL = process.env.NEKTAR_RV_URL ?? `http://${HOST}:${PORT}`
const PEERS = (process.env.NEKTAR_PEERS ?? '').split(',').map(s => s.trim().replace(/\/$/, '')).filter(Boolean)
const beaconNow = async () => {
  const sb = signBeacon(SELF_KEY, { endpoint: SELF_URL, net: NET, topics: ['tm_presence'], lookups: ['ls_presence'], ts: Date.now() })
  await overlay.lsPresence.announceBeacon(sb).catch(() => {})
  for (const peer of PEERS) fetch(`${peer}/beacon`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(sb) }).catch(() => {})
}
if (process.env.NEKTAR_BRIDGE_INGRESS) console.log(`[rendezvous] bridge ingress=${process.env.NEKTAR_BRIDGE_INGRESS} � plane delivery expected via overlay-bridge feed`)
// interval closer — every 10min commit the rv's own feed as a signed
// IntervalCommit: [firstSeq..lastSeq] + merkleRoot + prevIntervalHash, so a
// reader detects omission/reordering inside the range and repairs it from a peer
let intervalStart = 0
let prevIntervalHash = undefined
const closeInterval = async () => {
  try {
    const events = await overlay.lsPresence.feed({ since: intervalStart, limit: 10_000 })
    if (events.length === 0) return
    const leaves = events.map(e => Buffer.from(sha256(new TextEncoder().encode(JSON.stringify(e.payload)))).toString('hex'))
    const root = Buffer.from(sha256(Buffer.from(leaves.join('')))).toString('hex')
    const last = events[events.length - 1].seq
    const si = signInterval(SELF_KEY, { streamId: 'presence', firstSeq: events[0].seq, lastSeq: last, eventCount: events.length, merkleRoot: root, prevIntervalHash, ts: Date.now() })
    if (await overlay.lsPresence.announceInterval(si)) {
      prevIntervalHash = Buffer.from(sha256(new TextEncoder().encode(JSON.stringify(si.commit)))).toString('hex')
      intervalStart = last + 0.5
      for (const peer of PEERS) fetch(`${peer}/interval`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(si) }).catch(() => {})
    }
  } catch {}
}
setInterval(closeInterval, 10 * 60_000); setTimeout(closeInterval, 60_000)

setInterval(beaconNow, 30 * 60_000); setTimeout(beaconNow, 3000)
setInterval(() => { const n = overlay.lsPresence.sweep?.(); if (n) console.log(`[rendezvous] sweep: ${n} expired entries evicted`) }, 300_000)

// peer-rv federation — tail each peer's event feed and re-announce the signed
// payloads locally (intake still verifies sigs, so relayed artifacts are safe).
// announce dedups by ts, so re-ingestion is idempotent.
const ingest = {
  ad: p => overlay.lsPresence.announce(p),
  catalog: p => overlay.lsPresence.announceCatalog(p),
  want: p => overlay.lsPresence.announceWant(p),
  offer: p => overlay.lsPresence.announceOffer(p),
  settlement: p => overlay.lsPresence.announceSettlement(p),
  reputation: p => overlay.lsPresence.announceReputation(p),
  delegation: p => overlay.lsPresence.announceDelegation(p),
  attestation: p => overlay.lsPresence.announceAttestation(p),
  beacon: p => overlay.lsPresence.announceBeacon(p),
  revocation: p => overlay.lsPresence.revokeDelegation(p.revocation ?? p),
  interval: p => overlay.lsPresence.announceInterval(p),
  envelope: p => overlay.lsPresence.announceEnvelope(p.envelope ? p : { envelope: p, sig: p.sig })
}

// hash-addressed object store — any signed object that flowed through is
// retrievable by its content hash, so a surviving host serves the evidence
// after the original publisher goes away. Bounded LRU (max 10k objects).
const objectStore = new Map()
const sha256hex = (s) => Buffer.from(sha256(new TextEncoder().encode(s))).toString('hex')
const storeObject = (obj) => { if (objectStore.size > 10_000) objectStore.delete(objectStore.keys().next().value); objectStore.set(sha256hex(JSON.stringify(obj)), obj) }
for (const kind of ['ad','catalog','want','offer','settlement','reputation','delegation','attestation','beacon','interval','envelope']) {
  const orig = ingest[kind]
  ingest[kind] = async (p) => { storeObject(p); return orig?.(p) }
}
for (const peer of PEERS) {
  const CURSOR_FILE = path.join(DIR, 'peer-cursors.json')
  const savedCursors = (() => { try { return JSON.parse(fs.readFileSync(CURSOR_FILE, 'utf8')) } catch { return {} } })()
  const saveCursors = () => { try { fs.writeFileSync(CURSOR_FILE, JSON.stringify(savedCursors)) } catch {} }
  setInterval(saveCursors, 30_000)

  let cur = savedCursors[peer] ?? -1
  ;(async function tail () {
    for (;;) {
      try {
        const j = await (await fetch(`${peer}/events?since=${cur}&wait=30000`)).json()
        for (const e of j.events ?? []) {
          // cursor advances only after successful ingest — a failed apply is retried next poll, not skipped
          try { await ingest[e.kind]?.(e.payload); if (e.payload) objectStore.set(sha256hex(JSON.stringify(e.payload)), e.payload); cur = Math.max(cur, e.seq); savedCursors[peer] = cur } catch {}
        }
      } catch { await new Promise(r => setTimeout(r, 10_000)) }
    }
  })()
}
console.log(`[rendezvous] network=${NET} db=${path.join(DIR, 'overlay.sqlite')} listening ${HOST}:${PORT}`)

const json = (res, code, obj) => { res.writeHead(code, { 'content-type': 'application/json', 'access-control-allow-origin': '*' }); res.end(JSON.stringify(obj)) }
const body = (req) => new Promise((ok, no) => { let d = ''; req.on('data', c => { d += c; if (d.length > 1e6) no(new Error('too large')) }); req.on('end', () => ok(d)); req.on('error', no) })
const gated = (req) => !TOKEN || req.headers.authorization === `Bearer ${TOKEN}`
const decode = (o) => { try { return JSON.parse(Buffer.from(o.context ?? []).toString()) } catch { return null } }

const t0 = Date.now()
const server = http.createServer(async (req, res) => {
  try {
    const url = new URL(req.url ?? '/', `http://${req.headers.host ?? 'x'}`)
    if (req.method === 'POST' && url.pathname === '/beacon') {
      const { beacon, sig } = JSON.parse(await body(req) || '{}')
      const ok = await overlay.lsPresence.announceBeacon({ beacon, sig })
      return json(res, ok ? 200 : 400, ok ? { ok: true } : { error: 'rejected' })
    }
    if (req.method === 'GET' && url.pathname === '/events/delivery') {
      const seq = Number(url.searchParams.get('seq') ?? -1)
      const ev = (await overlay.lsPresence.feed({ since: seq - 0.5, limit: 1 })).find(e => e.seq === seq)
      if (!ev) return json(res, 404, { error: 'no event at seq' })
      const rec = deliveryRecord('tm_presence', new TextEncoder().encode(JSON.stringify(ev.payload)))
      res.writeHead(200, { 'content-type': 'application/octet-stream' }); return res.end(Buffer.from(rec))
    }
    // BRC-180 domain manifest — declares this host's overlay services so a
    // client resolving the domain gets the real tm_/ls_ names, no guessing
    if (req.method === 'GET' && (url.pathname === '/manifest.json' || url.pathname === '/.well-known/metanet')) {
      return json(res, 200, { metanet: { overlays: { tm_nektar: SELF_URL, ls_nektar: SELF_URL, tm_presence: SELF_URL, ls_presence: SELF_URL } } })
    }
    if (req.method === 'GET' && url.pathname === '/ship') {
      const sb = overlay.lsPresence.peersFor().find(x => x.beacon.pub === SELF_PUB)
      return json(res, 200, sb ? { SHIP: sb.beacon.topics, SLAP: sb.beacon.lookups ?? [], endpoint: sb.beacon.endpoint, net: sb.beacon.net, pub: SELF_PUB } : { note: 'no beacon yet — announced ~3s after boot' })
    }
    if (req.method === 'GET' && url.pathname === '/patterns') {
      return json(res, 200, { kindToPattern: KIND_PATTERN, note: 'replyTo-offers read as messagebox; everything else maps by kind' })
    }
    if (req.method === 'POST' && url.pathname === '/mailbox') {
      const { envelope, sig } = JSON.parse(await body(req) || '{}')
      const ok = await overlay.lsPresence.announceEnvelope({ envelope, sig })
      return json(res, ok ? 200 : 400, ok ? { ok: true, messageId: envelope?.messageId } : { error: 'rejected' })
    }
    if (req.method === 'GET' && url.pathname.startsWith('/v1/object/')) {
      const sha = url.pathname.split('/v1/object/')[1]
      const obj = objectStore.get(sha)
      return obj ? json(res, 200, obj) : json(res, 404, { error: 'object not found' })
    }
    if (req.method === 'GET' && url.pathname === '/mailbox') {
      const r = url.searchParams.get('recipient') ?? ''
      const t = url.searchParams.get('topic') ?? undefined
      if (!r && !t) return json(res, 400, { error: 'recipient or topic required' })
      return json(res, 200, { envelopes: overlay.lsPresence.mailboxFor(r, t) })
    }
    if (req.method === 'POST' && url.pathname === '/interval') {
      const { commit, sig } = JSON.parse(await body(req) || '{}')
      const ok = await overlay.lsPresence.announceInterval({ commit, sig })
      return json(res, ok ? 200 : 400, ok ? { ok: true } : { error: 'rejected' })
    }
    if (req.method === 'GET' && url.pathname === '/intervals') {
      const pub = url.searchParams.get('pub')
      return json(res, 200, { intervals: overlay.lsPresence.intervalsFor(pub ?? undefined) })
    }
    if (req.method === 'GET' && url.pathname === '/peers') {
      return json(res, 200, { peers: overlay.lsPresence.peersFor().map(sb => sb.beacon), self: SELF_PUB })
    }
    if (req.method === 'GET' && url.pathname === '/listing-fee') {
      const lf = process.env.NEKTAR_LISTING_ADDR ? {
        addr: process.env.NEKTAR_LISTING_ADDR,
        rateSatsPerHour: Number(process.env.NEKTAR_LISTING_RATE_SATS_PER_HOUR ?? 10),
        freeWindowMs: Number(process.env.NEKTAR_LISTING_FREE_WINDOW_MS ?? 3_600_000),
        note: 'exp beyond the free window requires pay:{beef} — a tx paying rate/hour to addr'
      } : { enabled: false, note: 'no listing fee — set NEKTAR_LISTING_ADDR to charge for exp beyond the free window' }
      return json(res, 200, lf)
    }
    if (req.method === 'GET' && url.pathname === '/logo.png') {
      const f = path.join(__dirname, '..', '..', 'logo.png')
      try { const png = fs.readFileSync(f); res.writeHead(200, { 'content-type': 'image/png', 'cache-control': 'public, max-age=86400' }); return res.end(png) } catch { return res.end() }
    }
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
    // open wants — optionally narrowed to one publisher and/or one want id
    if (req.method === 'GET' && url.pathname === '/wants') {
      const pub = url.searchParams.get('pub')
      const wantId = url.searchParams.get('wantId')
      if (pub && wantId) return json(res, 200, { want: overlay.lsPresence.wantFor(pub, wantId) ?? null })
      return json(res, 200, { wants: pub ? overlay.lsPresence.wantsFor(pub) : overlay.lsPresence.wantsFor() })
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
      const { grantId, pub, ts, sig } = JSON.parse(await body(req) || '{}')
      if (!grantId || !pub || !sig) return json(res, 400, { error: 'grantId, pub and sig required' })
      const ok = await overlay.lsPresence.revokeDelegation({ grantId, pub, ts: Number(ts ?? Date.now()), sig })
      return json(res, ok ? 200 : 400, ok ? { ok: true, grantId } : { error: 'not the owner, bad signature or unknown grant' })
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
      const pub = url.searchParams.get('pub') ?? undefined
      const limit = url.searchParams.get('limit') !== null ? Math.min(Number(url.searchParams.get('limit')), 1000) : 200
      const wait = Math.min(Number(url.searchParams.get('wait') ?? 0), 30_000)
      const deadline = Date.now() + wait
      for (;;) {
        const events = (await overlay.lsPresence.feed({ since, domains, kind: kind ?? undefined, pub, limit })).map(eventEnvelope)
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
#wrap{background:#161616;border:1px solid #262626;border-radius:10px;overflow:hidden;max-height:420px;overflow-y:auto}
#mt{width:100%;border-collapse:collapse;font-size:13px}
#mt thead th{position:sticky;top:0;background:#1a1a1a;color:#888;font-size:11px;letter-spacing:.1em;text-transform:uppercase;text-align:left;padding:10px 14px;cursor:pointer;user-select:none}
#mt thead th:hover{color:#f5b301}
#mt tbody td{padding:9px 14px;border-bottom:1px solid #1f1f1f}
#mt tbody tr:hover{background:#1c1c1c}
.num{text-align:right;font-variant-numeric:tabular-nums}
.dom{display:inline-block;background:#1a1a1a;border:1px solid #2a2a2a;border-radius:12px;padding:0 8px;font-size:11px;color:#999;margin-right:4px}
.ev{display:flex;gap:12px;padding:5px 0;border-bottom:1px solid #1a1a1a;font-size:12px;opacity:.75}
.ev .k{width:88px;flex-shrink:0}.ev .seq{color:#555;width:40px;flex-shrink:0}
.k-ad{color:#9ecbff}.k-catalog{color:#b78aff}.k-want{color:#fb923c}.k-offer{color:#4cc26b}
.k-settlement{color:#f5b301}.k-fraud{color:#e5534b}.k-delegation{color:#56d4dd}.k-attestation{color:#e6b3e6}
.dim{color:#777}.agents div{padding:4px 0;font-size:12px;color:#999}
.netbadge{font-size:11px;font-weight:600;letter-spacing:.1em;padding:3px 10px;border-radius:20px;margin-left:12px;vertical-align:middle;text-transform:uppercase}
.netbadge.testnet{background:#3a2a00;color:#f5b301;border:1px solid #f5b301}
.netbadge.mainnet{background:#062e0c;color:#4cc26b;border:1px solid #4cc26b}
.toggle{margin-left:14px}#netbtn{font:inherit;font-size:11px;padding:2px 12px;border-radius:20px;cursor:pointer}
header{position:relative;display:flex;align-items:center;justify-content:center;margin-bottom:18px}
#logo{display:flex;align-items:center;gap:8px}
#logo img{height:44px;width:auto}
header .toggle{position:absolute;right:0;top:50%;transform:translateY(-50%)}
#netbtn.testnet{background:#3a2a00;color:#f5b301;border:1px solid #f5b301}
#netbtn.mainnet{background:#062e0c;color:#4cc26b;border:1px solid #4cc26b}
footer{margin-top:24px;color:#555;font-size:12px}
</style></head><body>
<header>
<div id="logo"><img src="/logo.png" alt="nektar"><span class="dot" id="live"></span></div>
<span class="toggle"><button id="netbtn" class="testnet" onclick="setNet(NET==='ttn'?'main':'ttn')">mainnet</button></span>
</header>
<span id="netbadge" class="netbadge testnet" hidden>testnet</span>
<span id="rv-host" hidden></span>
<div id="wrap"><table id="mt"><thead><tr>
<th onclick="sortBy('rank')">#</th><th onclick="sortBy('pub')">agent</th><th onclick="sortBy('domain')">domain</th>
<th class="num" onclick="sortBy('price')">floor ⓢ</th><th class="num" onclick="sortBy('count')">listings</th><th onclick="sortBy('last')">last seen</th>
</tr></thead><tbody id="mb"><tr><td colspan="6" class="dim" style="padding:12px">no listings yet</td></tr></tbody></table></div>
<h2>Ticker</h2><div id="feed"><div class="dim">listening…</div></div>
<footer>signed artifacts only — pub + sig verifiable against the payload</footer>
<script>
const $ = id => document.getElementById(id)
// network sources — same-origin by default; override via ?ttn= & ?main=
const q=new URLSearchParams(location.search)
const SERVER_NET='${process.env.NEKTAR_NETWORK ?? 'ttn'}'
// NETS.main defaults to null — one rv serves one network; the toggle only
// appears when a real second source is given (?main=<other rv url>)
const NETS={ttn:q.get('ttn')||location.origin,main:q.get('main')||null}
if(!NETS.main||NETS.main===NETS.ttn)document.querySelector('.toggle').style.display='none'
let BASE=NETS.ttn, NET='ttn', es=null
const counts = {}
const seen = new Set()
let cursor = -1
function setNet(n){
  NET=n; BASE=NETS[n]||location.origin
  const btn=$('netbtn'); btn.textContent=n==='ttn'?'switch to mainnet':'switch to testnet'; btn.className=n==='ttn'?'testnet':'mainnet'
  $('rv-host').textContent=' · '+BASE
  const nb=$('netbadge'); nb.textContent=n==='main'?'mainnet':'testnet'; nb.className='netbadge '+(n==='main'?'mainnet':'testnet')
  $('feed').innerHTML='<div class="dim">listening…</div>'; seen.clear(); cursor=-1
  for(const k in rows)delete rows[k]; for(const k in lastSeen)delete lastSeen[k]; renderTable()
  if(es){es.close();es=null}
  for(const k in counts) delete counts[k]
  boot()
}
const bump = k => { counts[k] = (counts[k]||0)+1; const el=$('n-'+k); if(el) el.textContent=counts[k] }
const esc = s => String(s??'').replace(/[&<>]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;'}[c]))
// market rows — (pub|domain) → {price, count, last}
const rows={}
const lastSeen={}
function upOffer(o){
  const p=o?.offer??o; if(!p?.pub||p.priceSats==null)return
  const r=rows[p.pub]??(rows[p.pub]={pub:p.pub,domains:new Set(),price:Infinity,count:0,last:0})
  for(const d of (p.domains&&p.domains.length?p.domains:['—']))r.domains.add(d)
  r.price=Math.min(r.price,Number(p.priceSats)); r.count++
}
function touchPub(pub,ts){ if(pub)lastSeen[pub]=Math.max(lastSeen[pub]||0,ts||0) }
function renderTable(){
  const list=Object.values(rows)
  for(const r of list)r.last=lastSeen[r.pub]||0
  const k=sort.k,d=sort.d
  list.sort((a,b)=>{ const va=k==='rank'?a.count*-1:k==='price'?(a.price===Infinity?1e18:a.price):k==='last'?a.last:k==='pub'?a.pub:[...a.domains].join(',')
    const vb=k==='rank'?b.count*-1:k==='price'?(b.price===Infinity?1e18:b.price):k==='last'?b.last:k==='pub'?b.pub:[...b.domains].join(',')
    return (va<vb?-1:va>vb?1:0)*d })
  $('mb').innerHTML=list.length?list.slice(0,500).map((r,i)=>
    '<tr><td>'+(i+1)+'</td><td>'+esc(r.pub.slice(0,12))+'…</td><td>'+[...r.domains].map(d=>'<span class="dom">'+esc(d)+'</span>').join('')+'</td>'+
    '<td class="num">'+(r.price===Infinity?'—':r.price.toLocaleString())+'</td><td class="num">'+r.count+'</td>'+
    '<td class="dim">'+(r.last?new Date(r.last).toLocaleTimeString():'—')+'</td></tr>').join('')
    :'<tr><td colspan="6" class="dim" style="padding:12px">no listings yet</td></tr>'
}
const sort={k:'price',d:1}
function sortBy(k){ sort.d=sort.k===k?-sort.d:1; sort.k=k; renderTable() }
function row(e){
  const pub=(e.pub||'').slice(0,10), dom=(e.domains||[]).join(','), p=e.payload||{}
  let extra=''
  if(e.kind==='want'&&p.want?.maxSats!=null) extra='≤'+p.want.maxSats+' sats'
  if(e.kind==='offer'&&p.offer?.priceSats!=null) extra=p.offer.priceSats+' sats'
  return '<div class="ev"><span class="seq">#'+e.seq+'</span><span class="k k-'+e.kind+'">'+esc(e.kind)+'</span>'+
    '<span class="dim">'+esc(dom)+'</span><span class="dim">'+esc(pub)+'…</span><span class="dim">'+esc(extra)+'</span>'+
    '<span class="dim" style="margin-left:auto">'+new Date(e.ts||Date.now()).toLocaleTimeString()+'</span></div>'
}
let raf=false
function feed(evs){
  const box=$('feed'); if(box.querySelector('.dim')) box.innerHTML=''
  let dirty=false
  for(const e of evs){ if(seen.has(e.seq)) continue; seen.add(e.seq); cursor=Math.max(cursor,e.seq)
    bump(e.kind); box.insertAdjacentHTML('afterbegin',row(e)); touchPub(e.pub,e.ts)
    if(e.kind==='offer'){upOffer(e.payload?.offer?{offer:e.payload.offer}:e.payload);dirty=true}
    if(dirty&&!raf){raf=true;setTimeout(()=>{raf=false;renderTable()},300)} }
  while(box.children.length>5) box.lastChild.remove()
}
async function boot(){
  const j=await (await fetch(BASE+'/events?since=-30&limit=50')).json().catch(()=>({}))
  feed(j.events||[])
  const o=await (await fetch(BASE+'/offers')).json().catch(()=>({}))
  for(const x of o.offers||[])upOffer(x)
  renderTable()
  es=new EventSource(BASE+'/events/stream')
  es.onmessage=m=>{ try{ feed([JSON.parse(m.data)]) }catch{} }
  es.onopen=()=>{ $('live').style.background='#4cc26b' }
  es.onerror=()=>{ $('live').style.background='#e5534b' }
}
$('rv-host').textContent=' · '+BASE
setNet(SERVER_NET==='main'||SERVER_NET==='mainnet'?'main':'ttn')
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
