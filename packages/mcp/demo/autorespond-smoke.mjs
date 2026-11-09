#!/usr/bin/env node
// autorespond smoke — the seller's always-on loop on mock:
//   rendezvous + seller (soccer, answer@250) → foreign want posts →
//   tools/autorespond.mjs --once → a signed offer replyTo the want lands.
import { spawn } from 'node:child_process'
import { mkdtempSync, existsSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { PrivateKey } from '@bsv/sdk'
import { signWant } from '../../overlay/dist/src/index.js'

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..')
const BIN = join(ROOT, 'packages/mcp/dist/src/bin.js')
const SERVE = join(ROOT, 'packages/overlay/serve.mjs')
const AUTO = join(ROOT, 'tools/autorespond.mjs')
const RV = 18199, SELLER = 18198
const tmp = mkdtempSync(join(tmpdir(), 'nektar-auto-'))
const kids = []
const env = extra => ({ ...process.env, NEKTAR_NETWORK: 'mock', NEKTAR_BATCH: '1', NEKTAR_DEMO_UTXOS: '20', NEKTAR_AUTOMINE: '1', ...extra })
const spawnKid = (file, e) => { const k = spawn(process.execPath, [file], { env: e, stdio: ['ignore', 'ignore', 'pipe'] }); k.stderr.on('data', d => process.stderr.write(`[${file.split(/[\\/]/).pop()}] ${d}`)); kids.push(k); return k }
const healthy = async (port, path = '/v1/health') => { for (let i = 0; i < 60; i++) { try { const r = await fetch(`http://localhost:${port}${path}`); if (r.ok) return true } catch {} await new Promise(r => setTimeout(r, 400)) } throw new Error(`:${port} never came up`) }
const tool = async (port, name, args = {}) => { const r = await fetch(`http://localhost:${port}/v1/tools/${name}`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(args) }); const j = await r.json(); if (!r.ok) throw new Error(`${name} → ${r.status}: ${j.error}`); return j }
const die = m => { console.error(`FAIL: ${m}`); process.exitCode = 1 }

try {
  const buyerKey = PrivateKey.fromRandom()
  spawnKid(SERVE, env({ NEKTAR_OVERLAY_PORT: String(RV), NEKTAR_OVERLAY_DIR: join(tmp, 'ov') }))
  spawnKid(BIN, env({ NEKTAR_HTTP_PORT: String(SELLER), NEKTAR_DATA_DIR: join(tmp, 's'), NEKTAR_PAYMENTS: '1', NEKTAR_OPERATOR_WIF: PrivateKey.fromRandom().toWif([0xef]) }))
  await Promise.all([healthy(RV, '/health'), healthy(SELLER)])
  console.log('up: rendezvous + seller')

  await tool(SELLER, 'remember', { shard: 'soccer', data: 'Metz beat Lyon 2-1' })
  await tool(SELLER, 'describeShard', { shard: 'soccer', domains: ['soccer'], desc: 'results', offers: [{ kind: 'answer', priceSats: 250 }] })

  // a want from a DIFFERENT key — soccer, max 400
  const want = signWant(buyerKey, { domains: ['soccer'], question: 'who won?', maxSats: 400, ts: Date.now() })
  const wr = await fetch(`http://localhost:${RV}/want`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(want) })
  if (!wr.ok) die(`want post → ${wr.status}`)
  console.log('want posted (soccer ≤400) — running autorespond --once')

  const r = spawn(process.execPath, [AUTO, '--once', '--rv', `http://localhost:${RV}`, '--port', String(SELLER)], { env: env({ NEKTAR_DATA_DIR: join(tmp, 's') }), stdio: 'inherit' })
  await new Promise(res => r.on('exit', res))
  await new Promise(res => setTimeout(res, 500))

  // the offer must land on the rv, replyTo the want's pub
  const offers = await (await fetch(`http://localhost:${RV}/offers`)).json().catch(() => ({}))
  const mine = (offers.offers ?? []).find(o => o.offer?.replyTo === buyerKey.toPublicKey().toString())
  mine ? console.log(`PASS: offer replyTo posted — ${mine.offer?.priceSats} sats on ${mine.offer?.shard}`) : die(`no replyTo offer on rv (${JSON.stringify(offers).slice(0, 200)})`)

  // dedup: second pass must not respond again
  const r2 = spawn(process.execPath, [AUTO, '--once', '--rv', `http://localhost:${RV}`, '--port', String(SELLER)], { env: env({ NEKTAR_DATA_DIR: join(tmp, 's') }), stdio: 'inherit' })
  await new Promise(res => r2.on('exit', res))
  const offers2 = await (await fetch(`http://localhost:${RV}/offers`)).json().catch(() => ({}))
  const count = (offers2.offers ?? []).filter(o => o.offer?.replyTo === buyerKey.toPublicKey().toString()).length
  count === 1 ? console.log('PASS: dedup — second pass posted nothing new') : die(`dedup failed — ${count} replyTo offers`)
} finally {
  for (const k of kids) k.kill()
}
console.log('\ndone')
