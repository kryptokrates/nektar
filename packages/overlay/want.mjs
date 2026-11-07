#!/usr/bin/env node
// Post a signed WANT ad to the rendezvous — the demand side of the market:
// "I need domain X, will pay up to N sats". Sellers watching the feed see it.
//
//   NEKTAR_AGENT_WIF=<wif>                        your agent identity key
//   NEKTAR_WANT_DOMAINS=soccer,ligue-1            what you need
//   NEKTAR_WANT_QUESTION="league results"         optional free-text (leaks intent — keep generic)
//   NEKTAR_WANT_MAX_SATS=1000                     optional price ceiling
//   NEKTAR_AGENT_ENDPOINT=http://you:8080         optional callback endpoint
//   NEKTAR_WANT_TTL_MS=86400000                   optional lifetime
//   NEKTAR_OVERLAY_URL=http://<rendezvous>:8090
import { PrivateKey } from '@bsv/sdk'
import { signWant } from './dist/src/index.js'

const need = (k) => { const v = process.env[k]; if (!v) { console.error(`missing env ${k}`); process.exit(1) } return v }
const url = (process.env.NEKTAR_OVERLAY_URL ?? 'http://localhost:8090').replace(/\/$/, '')
const key = PrivateKey.fromWif(need('NEKTAR_AGENT_WIF'))
const ttl = process.env.NEKTAR_WANT_TTL_MS ? Number(process.env.NEKTAR_WANT_TTL_MS) : undefined
const signed = signWant(key, {
  domains: need('NEKTAR_WANT_DOMAINS').split(',').map(s => s.trim().toLowerCase()).filter(Boolean),
  ...(process.env.NEKTAR_WANT_QUESTION ? { question: process.env.NEKTAR_WANT_QUESTION } : {}),
  ...(process.env.NEKTAR_WANT_MAX_SATS ? { maxSats: Number(process.env.NEKTAR_WANT_MAX_SATS) } : {}),
  ...(process.env.NEKTAR_AGENT_ENDPOINT ? { endpoint: process.env.NEKTAR_AGENT_ENDPOINT } : {}),
  ts: Date.now(),
  ...(ttl ? { exp: Date.now() + ttl } : {})
})

const r = await fetch(`${url}/want`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(signed) })
const out = await r.json()
if (!r.ok || !out.ok) { console.error('want failed:', JSON.stringify(out)); process.exit(1) }
console.log(`want posted: pub=${out.pub} domains=${signed.want.domains.join(',')} on ${url}`)
