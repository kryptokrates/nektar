#!/usr/bin/env node
// Announce this agent on the nektar rendezvous directory.
//
//   NEKTAR_AGENT_WIF=<wif>                      key = your agent identity
//   NEKTAR_AGENT_ENDPOINT=http://you:8080/mcp   how peers reach you
//   NEKTAR_AGENT_SHARDS=manual,tnet             shards you serve
//   NEKTAR_AGENT_TTL_MS=86400000                optional ad lifetime (default: none)
//   NEKTAR_OVERLAY_URL=https://rv.nektar.run
import { PrivateKey } from '@bsv/sdk'
import { signPresence, signCatalog } from './dist/src/index.js'

const need = (k) => { const v = process.env[k]; if (!v) { console.error(`missing env ${k}`); process.exit(1) } return v }
const url = (process.env.NEKTAR_OVERLAY_URL ?? 'http://localhost:8090').replace(/\/$/, '')
const key = PrivateKey.fromWif(need('NEKTAR_AGENT_WIF'))
const endpoint = need('NEKTAR_AGENT_ENDPOINT')
const ttl = process.env.NEKTAR_AGENT_TTL_MS ? Number(process.env.NEKTAR_AGENT_TTL_MS) : undefined
const signed = signPresence(key, {
  endpoint,
  shards: need('NEKTAR_AGENT_SHARDS').split(',').map(s => s.trim()).filter(Boolean),
  ts: Date.now(),
  ...(ttl ? { exp: Date.now() + ttl } : {})
})

const r = await fetch(`${url}/announce`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(signed) })
const out = await r.json()
if (!r.ok || !out.ok) { console.error('announce failed:', JSON.stringify(out)); process.exit(1) }
console.log(`announced pub=${out.pub} endpoint=${signed.ad.endpoint} shards=${signed.ad.shards.join(',')} on ${url}`)

// content index: pull this instance's shard manifests off its own discovery
// doc and post the signed catalog — /directory?domain=<tag> then finds us.
// Skipped when the doc is unreachable or NEKTAR_AGENT_CATALOG=0.
if (!/^(0|false|no)$/i.test(process.env.NEKTAR_AGENT_CATALOG ?? '1')) {
  try {
    const wk = await fetch(`${new URL(endpoint).origin}/.well-known/nektar`, { signal: AbortSignal.timeout(10_000) })
    const doc = wk.ok ? await wk.json() : null
    const entries = (doc?.publicShards ?? []).filter(s => Array.isArray(s.domains) && s.domains.length)
      .map(s => ({ shard: s.shard, ...(s.shardId ? { shardId: s.shardId } : {}), domains: s.domains, ...(s.desc ? { desc: s.desc } : {}), ...(s.offers ? { offers: s.offers } : {}) }))
    if (entries.length) {
      const sc = signCatalog(key, { endpoint, shards: entries, ts: Date.now() })
      const cr = await fetch(`${url}/catalog`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(sc) })
      const co = await cr.json()
      if (!cr.ok || !co.ok) console.error('catalog post failed:', JSON.stringify(co))
      else console.log(`catalog posted: ${entries.length} described shard(s), domains ${[...new Set(entries.flatMap(e => e.domains))].join(',')}`)
    } else console.log('no described shards (describeShard) — catalog skipped')
  } catch (e) { console.error(`catalog skipped: ${e.message}`) }
}
