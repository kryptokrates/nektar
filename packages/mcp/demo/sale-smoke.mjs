// record-sale e2e — the escrow path the offer shelf advertises, end to end.
// mock: offer → verify → importRecord landing (the escrow itself is regtest/
// ttn only — DisclosureSale spends a real covenant UTXO). SMOKE_NETWORK=regtest
// |ttn runs the full offer → escrow → complete → claim → import loop on-chain.
//   node packages/mcp/demo/sale-smoke.mjs                 # mock
//   SMOKE_NETWORK=regtest node packages/mcp/demo/sale-smoke.mjs
import { spawn } from 'node:child_process'
import { mkdtempSync } from 'node:fs'
import { join } from 'node:path'
import { tmpdir } from 'node:os'

const NET = process.env.SMOKE_NETWORK ?? 'mock'
const RV = 18099, SELLER = 18098, BUYER = 18097
const tmp = mkdtempSync(join(tmpdir(), 'nektar-sale-smoke-'))
const env = (extra) => ({ ...process.env, NEKTAR_NETWORK: NET, NEKTAR_BATCH: '1', NEKTAR_DEMO_UTXOS: '20', NEKTAR_AUTOMINE: '1', ...extra })
const die = (m) => { console.error(`FAIL: ${m}`); process.exit(1) }
const kids = []
const kill = () => { for (const k of kids) k.kill('SIGTERM') }
process.on('exit', kill)

const wait = async (url, tries = 40) => { for (let i = 0; i < tries; i++) { try { const r = await fetch(url); if (r.ok) return } catch {} await new Promise(r => setTimeout(r, 500)) } die(`${url} never came up`) }
const tool = async (port, name, args = {}) => { const r = await fetch(`http://localhost:${port}/v1/tools/${name}`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(args) }); const j = await r.json().catch(() => ({})); if (!r.ok) die(`${name} → ${r.status}: ${JSON.stringify(j).slice(0, 200)}`); return j }

console.log('up: rendezvous + seller + buyer')
kids.push(spawn('node', ['packages/overlay/serve.mjs'], { env: env({ NEKTAR_OVERLAY_PORT: String(RV), NEKTAR_OVERLAY_DIR: join(tmp, 'rv') }), stdio: ['ignore', 'ignore', 'inherit'] }))
kids.push(spawn('node', ['packages/mcp/dist/src/bin.js'], { env: env({ NEKTAR_HTTP_PORT: String(SELLER), NEKTAR_DATA_DIR: process.env.SMOKE_SELLER_DIR ?? join(tmp, 's') }), stdio: ['ignore', 'ignore', 'inherit'] }))
kids.push(spawn('node', ['packages/mcp/dist/src/bin.js'], { env: env({ NEKTAR_HTTP_PORT: String(BUYER), NEKTAR_DATA_DIR: process.env.SMOKE_BUYER_DIR ?? join(tmp, 'b') }), stdio: ['ignore', 'ignore', 'inherit'] }))
await wait(`http://localhost:${RV}/directory`); await wait(`http://localhost:${SELLER}/.well-known/nektar`); await wait(`http://localhost:${BUYER}/.well-known/nektar`)

try {
  await tool(SELLER, 'remember', { shard: 'soccer', data: 'Metz beat Lyon 2-1' })
  const d = await tool(SELLER, 'describeShard', { shard: 'soccer', domains: ['soccer'], desc: 'results', offers: [{ kind: 'record', priceSats: 400, seq: 0 }] })
  console.log(`seller: described soccer (shardId ${d.manifest.shardId.slice(0, 12)}…)`)

  // the sale rail (escrow/offerMemory/claim) is hidden unless paid+regtest —
  // on mock we exercise the visible half: the shelf + the import landing
  const leaf = (NET === 'mock') ? 'a'.repeat(64) : undefined
  await tool(SELLER, 'publishOffer', { shard: 'soccer', seq: 0, priceSats: 400, ...(leaf ? { leaf } : {}), rendezvousUrl: 'http://localhost:' + RV })
  const shelf = await (await fetch(`http://localhost:${RV}/offers?domain=soccer`)).json()
  const listing = shelf.offers?.find(o => o.offer.shard === 'soccer' && o.offer.priceSats === 400)
  if (!listing) die('offer not on the shelf')
  console.log(`shelf: soccer:0 @ 400 sats — leaf ${String(listing.offer.leaf ?? '').slice(0, 12)}…`)

  if (NET === 'mock') {
    const imp = await tool(BUYER, 'importRecord', { data: 'Metz beat Lyon 2-1', sourceShard: 'soccer', sourceSeq: 0, leaf: listing.offer.leaf })
    const recs = await tool(BUYER, 'recall', { shard: 'bought', text: 'Metz' })
    if (!recs.items?.length) die('imported record not recallable')
    console.log(`buyer: importRecord → seq ${imp.seq}, recallable on 'bought'`)
  } else {
    // live: real escrow — buyer locks sats, seller reveals, buyer claims + imports
    const esc = await tool(BUYER, 'sale', { op: 'escrow', offer: listing.offer.offer, cfg: listing.offer.offer.cfg, amount: 400 })
    if (!esc.accepted) die(`escrow refused: ${esc.reason}`)
    console.log(`buyer: escrow locked 400 sats (txid ${String(esc.txid ?? '').slice(0, 12)}…)`)
    const comp = await tool(SELLER, 'sale', { op: 'complete', offer: listing.offer.offer, escrowTxid: esc.txid })
    const claim = await tool(BUYER, 'sale', { op: 'claim', txHex: comp.txHex, commitment: listing.offer.offer.commitment, sellerPub: listing.offer.pub })
    if (!claim.verified) die(`claim payload did not verify: ${JSON.stringify(claim).slice(0, 200)}`)
    console.log(`buyer: claim verified${claim.authorVerified ? ' + author provenance' : ''}`)
    const imp = await tool(BUYER, 'importRecord', { data: claim.data, provenance: claim.provenance, sourceShard: 'soccer', sourceSeq: 0, leaf: listing.offer.leaf, saleTxid: esc.txid })
    console.log(`buyer: importRecord → seq ${imp.seq} — purchased memory landed`)
  }

  console.log(`\nOK — offer shelf → ${NET === 'mock' ? 'verify → import' : 'escrow → complete → claim → import'}, on ${NET}`)
} finally { kill() }
