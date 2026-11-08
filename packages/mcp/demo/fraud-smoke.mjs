#!/usr/bin/env node
// fraud smoke — the ZKCP recourse leg: run a real disclosure escrow on ttn,
// then claim with a corrupted commitment.leaf → verification fails →
// publishFraud signs + posts an attributable fraud attestation to the rv.
// Reuses the funded ttn dirs (SMOKE_SELLER_DIR/SMOKE_BUYER_DIR, NEKTAR_FUND_WIF).
import { spawn } from 'node:child_process'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..')
const BIN = join(ROOT, 'packages/mcp/dist/src/bin.js')
const SERVE = join(ROOT, 'packages/overlay/serve.mjs')
const RV = 18096, SELLER = 18097, BUYER = 18098
const kids = []
const env = (extra) => ({ ...process.env, NEKTAR_NETWORK: 'ttn', NEKTAR_BATCH: '1', NEKTAR_PAID_DISCLOSURE: '1', ...extra })
const spawnKid = (file, e) => { const k = spawn(process.execPath, [file], { env: e, stdio: ['ignore', 'ignore', 'pipe'] }); k.stderr.on('data', d => process.stderr.write(`[${file.split(/[\\/]/).pop()}] ${d}`)); kids.push(k); return k }
const healthy = async (port, path = '/v1/health') => {
  for (let i = 0; i < 60; i++) { try { const r = await fetch(`http://localhost:${port}${path}`); if (r.ok) return true } catch {} await new Promise(r => setTimeout(r, 400)) }
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
  spawnKid(SERVE, env({ NEKTAR_OVERLAY_PORT: String(RV) }))
  await healthy(RV, '/directory')
  spawnKid(BIN, env({ NEKTAR_HTTP_PORT: String(SELLER), NEKTAR_DATA_DIR: process.env.SMOKE_SELLER_DIR }))
  spawnKid(BIN, env({ NEKTAR_HTTP_PORT: String(BUYER), NEKTAR_DATA_DIR: process.env.SMOKE_BUYER_DIR }))
  await Promise.all([healthy(SELLER), healthy(BUYER)])
  console.log('up: rv + seller + buyer on ttn')

  const buyerPub = (await tool(BUYER, 'reputation', {})).card.pub
  await tool(SELLER, 'remember', { shard: 'soccer', data: 'Metz beat Lyon 2-1 in Ligue 1' })
  const refundAfter = Math.floor(Date.now() / 1000) + 86_400
  const om = await tool(SELLER, 'sale', { op: 'offerMemory', shard: 'soccer', seq: 0, price: 400, refundAfter, maxFee: 1000, buyerPub })
  const offer = { cfg: om.cfg, wrapped: om.wrapped, sellerPub: om.sellerPub, expiresAt: om.expiresAt, offerSig: om.offerSig, commitment: om.commitment }
  const esc = await tool(BUYER, 'sale', { op: 'escrow', cfg: offer.cfg, amount: 2000, offer })
  const comp = await tool(BUYER, 'sale', { op: 'complete', cfg: offer.cfg, utxo: esc.utxo, wrapped: offer.wrapped, changeSats: 900 })
  console.log(`escrow ran → buy tx ${comp.txid?.slice(0, 14)}…`)

  // corrupt the leaf — the claim must fail and publishFraud must fire
  const badLeaf = 'ff'.repeat(32)
  const claim = await tool(BUYER, 'sale', {
    op: 'claim', txHex: comp.txHex, hWrapped: offer.cfg.hWrapped, sellerPub: offer.sellerPub,
    commitment: { leaf: badLeaf, hdp: offer.commitment?.hdp, authorPub: offer.commitment?.authorPub },
    publishFraud: true, rendezvousUrl: `http://localhost:${RV}`
  })
  console.log(`claim verified=${claim.verified} fraudPublished=${claim.fraudPublished} reason=${claim.reason?.slice(0, 60)}`)
  if (claim.verified !== false) die('corrupted leaf should fail verification')
  if (claim.fraudPublished !== true) die('publishFraud did not post')

  const att = await (await fetch(`http://localhost:${RV}/attestation`)).json()
  const fraud = (att.attestations ?? []).find(a => a.attestation.claims?.some(c => c.startsWith('fraud:sale:')))
  if (!fraud) die('fraud attestation not on the rendezvous')
  console.log(`rv: fraud attestation live — ${fraud.attestation.claims[0].slice(0, 40)}`)
  console.log('\nOK — ZKCP recourse: bad leaf → claim fails → signed fraud attestation on the rv')
} catch (e) {
  die(e.message)
} finally {
  for (const k of kids) k.kill('SIGTERM')
}
