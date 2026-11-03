#!/usr/bin/env node
// Announce this agent on the nektar rendezvous directory.
//
//   NEKTAR_AGENT_WIF=<wif>                      key = your agent identity
//   NEKTAR_AGENT_ENDPOINT=http://you:8080/mcp   how peers reach you
//   NEKTAR_AGENT_SHARDS=manual,tnet             shards you serve
//   NEKTAR_AGENT_TTL_MS=86400000                optional ad lifetime (default: none)
//   NEKTAR_OVERLAY_URL=http://167.233.174.91:8090
import { PrivateKey } from '@bsv/sdk'
import { signPresence } from './dist/src/index.js'

const need = (k) => { const v = process.env[k]; if (!v) { console.error(`missing env ${k}`); process.exit(1) } return v }
const url = (process.env.NEKTAR_OVERLAY_URL ?? 'http://localhost:8090').replace(/\/$/, '')
const key = PrivateKey.fromWif(need('NEKTAR_AGENT_WIF'))
const ttl = process.env.NEKTAR_AGENT_TTL_MS ? Number(process.env.NEKTAR_AGENT_TTL_MS) : undefined
const signed = signPresence(key, {
  endpoint: need('NEKTAR_AGENT_ENDPOINT'),
  shards: need('NEKTAR_AGENT_SHARDS').split(',').map(s => s.trim()).filter(Boolean),
  ts: Date.now(),
  ...(ttl ? { exp: Date.now() + ttl } : {})
})

const r = await fetch(`${url}/announce`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(signed) })
const out = await r.json()
if (!r.ok || !out.ok) { console.error('announce failed:', JSON.stringify(out)); process.exit(1) }
console.log(`announced pub=${out.pub} endpoint=${signed.ad.endpoint} shards=${signed.ad.shards.join(',')} on ${url}`)
