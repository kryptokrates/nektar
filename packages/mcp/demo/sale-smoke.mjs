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
const disclosure = NET !== 'mock' ? { NEKTAR_PAID_DISCLOSURE: '1' } : {}
kids.push(spawn('node', ['packages/mcp/dist/src/bin.js'], { env: env({ ...disclosure, NEKTAR_HTTP_PORT: String(SELLER), NEKTAR_DATA_DIR: process.env.SMOKE_SELLER_DIR ?? join(tmp, 's') }), stdio: ['ignore', 'ignore', 'inherit'] }))
kids.push(spawn('node', ['packages/mcp/dist/src/bin.js'], { env: env({ ...disclosure, NEKTAR_HTTP_PORT: String(BUYER), NEKTAR_DATA_DIR: process.env.SMOKE_BUYER_DIR ?? join(tmp, 'b') }), stdio: ['ignore', 'ignore', 'inherit'] }))
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
    // live: the real DisclosureSale rail — offerMemory artifact → escrow →
    // buy tx (reveals wrapped on-chain, pays seller) → claim → import
    const tip = (await (await fetch('http://localhost:8098/v1/tip')).json()).height
    // escrow's §16/17 sig check binds cfg+seller+buyerPub+leaf — the offer
    // must be minted FOR the buyer's operator pub; `reputation` returns it
    const buyerPub = (await tool(BUYER, 'reputation', {})).card.pub
    const om = await tool(SELLER, 'sale', { op: 'offerMemory', shard: 'soccer', seq: 0, price: 400, refundAfter: tip + 2016, maxFee: 1000, buyerPub })
    // publish the raw artifact so the buyer can verify the offerSig + leaf path pre-funding
    await tool(SELLER, 'publishOffer', { shard: 'soccer', seq: 0, priceSats: 400, offer: om, rendezvousUrl: 'http://localhost:' + RV })
    const esc = await tool(BUYER, 'sale', { op: 'escrow', cfg: om.cfg, amount: 2_000, offer: { sellerPub: om.sellerPub, offerSig: om.offerSig, expiresAt: om.expiresAt, commitment: om.commitment } })
    if (!esc.accepted) die(`escrow refused: ${esc.reason}`)
    console.log(`buyer: escrow locked (txid ${String(esc.txid ?? '').slice(0, 12)}…)`)
    const comp = await tool(BUYER, 'sale', { op: 'complete', cfg: esc.escrow, utxo: esc.utxo, wrapped: om.wrapped, changeSats: 1_000 })
    if (!comp.accepted) die(`complete refused: ${comp.reason}`)
    console.log(`buyer: buy tx revealed wrapped — seller paid ${comp.sellerSats} sats (txid ${String(comp.txid ?? '').slice(0, 12)}…)`)
    // claim needs the raw buy tx — pull it off the node
    const qsEnv = {}
    try { for (const l of (await import('node:fs')).readFileSync('infra/teranode-quickstart/.env', 'utf8').split(/\r?\n/)) { const i = l.indexOf('='); if (i > 0) qsEnv[l.slice(0, i).trim()] = l.slice(i + 1).trim() } } catch {}
    const auth = 'Basic ' + Buffer.from((process.env.NEKTAR_RPC_USER || qsEnv.rpc_user || '') + ':' + (process.env.NEKTAR_RPC_PASS || qsEnv.rpc_pass || '')).toString('base64')
    const rpc = await fetch('http://localhost:9292/', { method: 'POST', headers: { 'content-type': 'application/json', authorization: auth }, body: JSON.stringify({ jsonrpc: '1.0', id: 's', method: 'getrawtransaction', params: [comp.txid] }) })
    const txHex = (await rpc.json()).result
    if (typeof txHex !== 'string') die(`getrawtransaction failed: ${JSON.stringify(txHex)}`)
    const claim = await tool(BUYER, 'sale', { op: 'claim', txHex, hWrapped: om.cfg.hWrapped, sellerPub: om.sellerPub, commitment: om.commitment, offerMode: om.mode })
    if (!claim.verified) die(`claim payload did not verify: ${JSON.stringify(claim).slice(0, 200)}`)
    console.log(`buyer: claim verified${claim.authorVerified ? ' + author provenance' : ''}`)
    const imp = await tool(BUYER, 'importRecord', { data: claim.data ?? Buffer.from(claim.valueHex ?? '', 'hex').toString(), provenance: claim.provenance, sourceShard: 'soccer', sourceSeq: 0, leaf: om.commitment.leaf, saleTxid: esc.txid })
    console.log(`buyer: importRecord → seq ${imp.seq} — purchased memory landed`)
  }

  console.log(`\nOK — offer shelf → ${NET === 'mock' ? 'verify → import' : 'escrow → complete → claim → import'}, on ${NET}`)
} finally { kill() }
