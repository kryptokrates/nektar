#!/usr/bin/env node
// discovery smoke — the whole market loop on mock, two real processes:
//   seller remembers + describes (soccer) → announces ad + signed catalog on a
//   rendezvous → buyer discovers by domain tag → peerCall recall (free) →
//   peerCall groundedAnswer (402 → signed quote → pay → receipt).
// Everything offline: separate mock chains per instance; the payment tx
// travels as BEEF so the seller's mock validates against carried parents.
import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { PrivateKey } from '@bsv/sdk'
import { signPresence, signCatalog, signWant } from '../../overlay/dist/src/index.js'

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..')
const BIN = join(ROOT, 'packages/mcp/dist/src/bin.js')
const SERVE = join(ROOT, 'packages/overlay/serve.mjs')
const RV = 18099, SELLER = 18098, BUYER = 18097
const tmp = mkdtempSync(join(tmpdir(), 'nektar-disco-'))
const kids = []
const NET = process.env.SMOKE_NETWORK ?? 'mock'
const env = (extra) => ({ ...process.env, NEKTAR_NETWORK: NET, NEKTAR_BATCH: '1', NEKTAR_DEMO_UTXOS: '20', NEKTAR_AUTOMINE: '1', ...extra })
const spawnKid = (file, args, e) => {
  const k = spawn(process.execPath, [file, ...args], { env: e, stdio: ['ignore', 'ignore', 'pipe'] })
  k.stderr.on('data', d => process.stderr.write(`[${args.join(' ') || file}] ${d}`))
  kids.push(k)
  return k
}
const healthy = async (port, path = '/v1/health') => {
  for (let i = 0; i < 60; i++) {
    try { const r = await fetch(`http://localhost:${port}${path}`); if (r.ok) return true } catch {}
    await new Promise(r => setTimeout(r, 400))
  }
  throw new Error(`:${port} never came up`)
}
const tool = async (port, name, args = {}) => {
  const r = await fetch(`http://localhost:${port}/v1/tools/${name}`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(args) })
  const j = await r.json()
  if (!r.ok) throw new Error(`${name} → ${r.status}: ${j.error}`)
  return j
}
const die = (msg) => { console.error(`FAIL: ${msg}`); process.exitCode = 1 }

try {
  const sellerWif = PrivateKey.fromRandom().toWif([0xef])
  const buyerWif = PrivateKey.fromRandom().toWif([0xef])
  const sellerKey = PrivateKey.fromWif(sellerWif)

  spawnKid(SERVE, [], env({ NEKTAR_OVERLAY_PORT: String(RV), NEKTAR_OVERLAY_DIR: join(tmp, 'ov') }))
  spawnKid(BIN, [], env({ NEKTAR_HTTP_PORT: String(SELLER), NEKTAR_DATA_DIR: process.env.SMOKE_SELLER_DIR ?? join(tmp, 's'), NEKTAR_PAYMENTS: '1', NEKTAR_OPERATOR_WIF: sellerWif }))
  spawnKid(BIN, [], env({ NEKTAR_HTTP_PORT: String(BUYER), NEKTAR_DATA_DIR: process.env.SMOKE_BUYER_DIR ?? join(tmp, 'b'), NEKTAR_PAYMENTS: '1', NEKTAR_OPERATOR_WIF: buyerWif }))
  await Promise.all([healthy(RV, '/health'), healthy(SELLER), healthy(BUYER)])
  console.log('up: rendezvous + seller + buyer')

  // seller: one soccer record (anchored immediately — NEKTAR_BATCH=1), then
  // the committed manifest that makes it findable by domain
  const w = await tool(SELLER, 'remember', { shard: 'soccer', data: 'Metz beat Lyon 2-1 on 2026-11-01' })
  if (!w.anchored) die('seller record did not anchor')
  const d = await tool(SELLER, 'describeShard', { shard: 'soccer', domains: ['soccer', 'ligue-1'], desc: 'French football results', offers: [{ kind: 'answer', priceSats: 250 }] })
  console.log(`seller: remembered + described soccer (shardId ${d.manifest.shardId.slice(0, 12)}…)`)

  // live networks: anchors confirm on real block time — wait until the
  // record's fragment actually proves before selling answers over it
  if (NET !== 'mock') {
    for (let i = 0; i < 40; i++) {
      const p = await tool(SELLER, 'prove', { shard: 'soccer', seq: 0 }).catch(() => null)
      if (p?.bundle || p?.verified || p?.anchored) break
      await new Promise(r => setTimeout(r, 5000))
      if (i === 39) die('record anchor never confirmed')
    }
    console.log('seller: soccer record anchor confirmed on ' + NET)
  }

  // announce: signed presence ad + signed catalog (what announce.mjs does,
  // built here from the seller's own discovery doc)
  const wk = await (await fetch(`http://localhost:${SELLER}/.well-known/nektar`)).json()
  const entry = wk.publicShards.find(s => s.shard === 'soccer')
  if (!entry?.domains?.includes('soccer')) die('well-known doc lacks domains')
  const ad = signPresence(sellerKey, { endpoint: `http://localhost:${SELLER}`, shards: ['soccer'], ts: Date.now() })
  const ar = await fetch(`http://localhost:${RV}/announce`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(ad) })
  if (!(await ar.json()).ok) die('ad announce rejected')
  const cat = signCatalog(sellerKey, {
    endpoint: `http://localhost:${SELLER}`,
    shards: wk.publicShards.filter(s => s.domains?.length).map(s => ({ shard: s.shard, shardId: s.shardId, domains: s.domains, ...(s.desc ? { desc: s.desc } : {}), ...(s.offers ? { offers: s.offers } : {}) })),
    ts: Date.now()
  })
  const cr = await fetch(`http://localhost:${RV}/catalog`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(cat) })
  if (!(await cr.json()).ok) die('catalog announce rejected')
  console.log('rendezvous: ad + catalog announced')

  // buyer: who claims soccer?
  const found = await tool(BUYER, 'discover', { domain: 'soccer', rendezvousUrl: `http://localhost:${RV}` })
  const cand = (found.ads ?? []).find(a => a.endpoint === `http://localhost:${SELLER}`)
  if (!cand) die(`discover did not find the seller: ${JSON.stringify(found)}`)
  if (cand.catalog?.shards?.[0]?.shardId !== d.manifest.shardId) die('catalog shardId mismatch')
  console.log(`buyer: discover{domain:soccer} → ${cand.endpoint} (shardId checkable: ${cand.catalog.shards[0].shardId.slice(0, 12)}…)`)

  // free leg: read the record the candidate offers
  const rec = await tool(BUYER, 'peerCall', { endpoint: cand.endpoint, tool: 'recall', args: { shard: 'soccer' } })
  const seq = rec.items?.[0]?.seq ?? 0
  console.log(`buyer: peerCall recall → "${rec.items?.[0]?.text}"`)

  // paid leg: 402 → signed quote verified → paid → grounded answer + receipt
  const ask = await tool(BUYER, 'peerCall', {
    endpoint: cand.endpoint, tool: 'groundedAnswer', maxSats: 1000,
    args: { claims: [{ text: 'Metz beat Lyon 2-1', cites: [{ shard: 'soccer', seq }] }] }
  })
  if (!ask.paidTxid) die(`peerCall did not pay: ${JSON.stringify(ask).slice(0, 300)}`)
  const receipt = ask.result?.receipt ?? ask.result
  if (!receipt?.answer && !ask.result?.answer) die(`no grounded answer: ${JSON.stringify(ask).slice(0, 300)}`)
  console.log(`buyer: peerCall groundedAnswer → paid ${ask.amount} sats (txid ${ask.paidTxid.slice(0, 12)}…), answer + receipt delivered`)

  // event feed: the buyer's cursor replays the seller's ad + catalog events
  const feed = await (await fetch(`http://localhost:${RV}/events?domains=soccer`)).json()
  const kinds = (feed.events ?? []).map(e => e.kind).sort()
  if (!kinds.includes('catalog')) die(`event feed missing the catalog event: ${JSON.stringify(feed).slice(0, 200)}`)
  console.log(`buyer: /events?domains=soccer → ${kinds.join('+')} (cursor ${feed.cursor})`)

  // demand side: buyer posts a signed want; a fresh watch sees it land
  const w1 = await tool(BUYER, 'watch', { domains: ['soccer'], since: feed.cursor, rendezvousUrl: `http://localhost:${RV}` })
  const buyerKey = PrivateKey.fromWif(buyerWif)
  const want = signWant(buyerKey, { domains: ['soccer'], question: 'need weekly results', maxSats: 1000, ts: Date.now() })
  const wr = await fetch(`http://localhost:${RV}/want`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(want) })
  if (!(await wr.json()).ok) die('want rejected')
  const w2 = await tool(BUYER, 'watch', { domains: ['soccer'], since: w1.cursor ?? feed.cursor, rendezvousUrl: `http://localhost:${RV}` })
  const wantEv = (w2.events ?? []).find(e => e.kind === 'want' && e.pub === buyerKey.toPublicKey().toString())
  if (!wantEv) die(`watch did not deliver the want event: ${JSON.stringify(w2).slice(0, 200)}`)
  console.log(`buyer: watch{domains:soccer} delivered want event #${wantEv.seq} ("${wantEv.payload.want.question}")`)

  // the shelf: seller publishes a signed offer; buyer sees it via /offers + feed
  const pubOffer = await tool(SELLER, 'publishOffer', { shard: 'soccer', seq, priceSats: 400, leaf: 'f'.repeat(64), rendezvousUrl: 'http://localhost:' + RV })
  if (!pubOffer.ok) die('publishOffer failed')
  const shelf = await (await fetch(`http://localhost:${RV}/offers?domain=soccer`)).json()
  if (!shelf.offers?.some(o => o.offer.shard === 'soccer' && o.offer.priceSats === 400)) die('shelf missing the offer')
  console.log(`seller: publishOffer → shelf shows soccer:0 @ 400 sats`)

  // realized prices + reputation: seller announces the settled sale + its card
  await tool(SELLER, 'publishSettlement', { invoiceId: 'inv-smoke-1', tool: 'groundedAnswer', sats: ask.amount, txid: ask.paidTxid, domains: ['soccer'], rendezvousUrl: 'http://localhost:' + RV })
  const prices = await (await fetch(`http://localhost:${RV}/prices?domain=soccer`)).json()
  if (prices.count !== 1 || prices.medianSats !== ask.amount) die(`prices index wrong: ${JSON.stringify(prices).slice(0, 200)}`)
  console.log(`prices: soccer clears at ${prices.medianSats} sats (n=${prices.count})`)
  const rep = await tool(SELLER, 'reputation', { publish: true, rendezvousUrl: `http://localhost:${RV}` })
  if (!rep.ok) die('reputation publish failed')
  const rp = await (await fetch(`http://localhost:${RV}/reputation?domain=soccer`)).json()
  const sellerRep = rp.reputations?.find(r => r.card.pub === rep.pub)
  if (!sellerRep) die('reputation not indexed')
  console.log(`reputation: seller card → soccer:{receipts:${sellerRep.card.entries[0]?.receipts ?? 0}} — vectors, not one score`)

  // want→respond: the seller answers the buyer's want with a replyTo offer
  const sellerPub = rep.pub
  const resp = await tool(SELLER, 'respond', { wantPub: wantEv.pub, wantId: wantEv.payload.want.wantId, taskHash: wantEv.payload.want.taskHash, wantRevision: wantEv.payload.want.revision, shard: 'soccer', seq: 0, priceSats: 400, rendezvousUrl: `http://localhost:${RV}` })
  if (!resp.ok) die('respond failed')
  const buyerFeed = (await (await fetch(`http://localhost:${RV}/events?domains=soccer&kind=offer&since=${w2.cursor ?? feed.cursor}`)).json()).events
  if (!buyerFeed?.some(e => e.payload?.offer?.replyTo === wantEv.pub)) die('replyTo offer not on the feed')
  console.log(`respond: seller answered the want — offer replyTo ${wantEv.pub.slice(0, 12)}…`)

  // verifyReputation: buyer recomputes the seller's card from exported packs
  const receiptsPack = await tool(SELLER, 'exportShard', { shard: '__receipts__' })
  const catalogPack = await tool(SELLER, 'exportShard', { shard: '__catalog__' })
  const vr = await tool(BUYER, 'verifyReputation', { card: sellerRep.card, receiptsPack, catalogPack })
  if (!vr.ok) die(`verifyReputation mismatch: ${JSON.stringify(vr.diffs).slice(0, 200)}`)
  console.log(`verifyReputation: seller card recomputed — ${vr.checked} entries match`)

  console.log('\nOK — discover → quote → pay → receipt → shelf → prices → reputation, all on ' + NET + '')
} catch (e) {
  die(e.message)
} finally {
  for (const k of kids) k.kill('SIGTERM')
  setTimeout(() => rmSync(tmp, { recursive: true, force: true }), 500).unref()
}
process.exit(process.exitCode ?? 0)
