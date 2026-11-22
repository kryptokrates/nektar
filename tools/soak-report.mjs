#!/usr/bin/env node
// soak-report.mjs — read-only soak snapshot, appended to .state/soak-report.jsonl
//
// Answers, once per invocation:
//   - what does {announces:{net}} return right now (verified, client-side)?
//   - how many DISTINCT publishers are on the dir lane?
//   - which hosts withhold or are unreachable?
//   - what did each watched agent spend today vs its SpendGuard caps?
//
// It is read-only: the announce read goes through the same SPV + signature
// path a client uses; agent stats come from GET /v1/wallet and /v1/costs.
// No keys, no writes to any service — the only write is one JSONL line.
//
// Env:
//   NEKTAR_SOAK_NET     network tag the announces are read for (default 'testnet')
//   NEKTAR_SOAK_HOSTS   CSV of seed hosts to query — default: NEKTAR_MARKET_HOSTS,
//                       else the built-in DEFAULT_MARKET_HOSTS for the net
//   NEKTAR_SOAK_AGENTS  CSV of name=url[@token] agent HTTP endpoints to poll
//                       (/v1/wallet + /v1/costs); token sent as Bearer
//   NEKTAR_SOAK_OUT     jsonl path (default <repo>/.state/soak-report.jsonl)
//   NEKTAR_SOAK_LOCAL_HOSTS=1  allow http://localhost/127.0.0.1 seed hosts
//
// Usage:  node tools/soak-report.mjs
//         NEKTAR_SOAK_AGENTS='a=http://127.0.0.1:8182@tok' node tools/soak-report.mjs

import { appendFileSync, mkdirSync } from 'node:fs'
import { join, dirname, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const dist = (p) => pathToFileURL(join(root, p)).href

const { readAnnounces } = await import(dist('packages/bstack/dist/src/hosts.js'))
const { fingerHostAllowed } = await import(dist('packages/bstack/dist/src/finger.js'))
const { DEFAULT_MARKET_HOSTS } = await import(dist('packages/core/dist/src/env.js'))
const { ChaintracksTracker, IndexTracker } = await import(dist('packages/chain/dist/src/mainnet.js'))
const { WitnessedTracker } = await import(dist('packages/chain/dist/src/headers.js'))

const net = process.env.NEKTAR_SOAK_NET ?? 'testnet'
const localHosts = process.env.NEKTAR_SOAK_LOCAL_HOSTS === '1'

const envHosts = (process.env.NEKTAR_SOAK_HOSTS ?? process.env.NEKTAR_MARKET_HOSTS ?? '')
  .split(',')
  .map((s) => s.trim().replace(/\/+$/, ''))
  .filter(Boolean)
const hosts = envHosts.length > 0 ? envHosts : [...(DEFAULT_MARKET_HOSTS[net] ?? [])]

const rec = {
  ts: Date.now(),
  net,
  hosts,
  announces: { count: 0, distinctPublishers: 0, publishers: [], txids: [], items: [] },
  withheld: [],
  unreachable: [],
  hostsUp: 0,
  agents: {},
  notes: []
}

// ── tracker: same dual-source witnessed construction the runtime uses ──────
const NET_ENDPOINTS = {
  testnet: {
    broadcasterUrl: 'https://arcade-v2-testnet-us-1.bsvblockchain.tech',
    indexerUrl: 'https://test.bananablocks.com/api/v1',
    chaintracksUrl: 'https://arcade-v2-testnet-us-1.bsvblockchain.tech/chaintracks/v2'
  },
  main: {
    broadcasterUrl: process.env.NEKTAR_BROADCASTER_URL ?? 'https://arcade.gorillapool.io',
    indexerUrl: process.env.NEKTAR_INDEX_URL ?? 'https://bananablocks.com/api/v1',
    chaintracksUrl: process.env.NEKTAR_CHAINTRACKS_URL ?? 'https://arcade-us-1.bsvblockchain.tech/chaintracks/v2'
  }
}
const ep = NET_ENDPOINTS[net]
if (ep === undefined) {
  rec.notes.push(`no public header endpoints for net '${net}' — announce read skipped`)
}

if (hosts.length > 0 && ep !== undefined) {
  const mc = { broadcasterUrl: ep.broadcasterUrl, indexerUrl: ep.indexerUrl, chaintracksUrl: ep.chaintracksUrl }
  const tracker = new WitnessedTracker(
    new ChaintracksTracker(mc, ep.chaintracksUrl, undefined, { requirePow: true }),
    new IndexTracker(mc, { requirePow: true }),
    {}
  )
  const allow = hosts.filter((h) => fingerHostAllowed(h, { allowHttp: localHosts, allowLocal: localHosts }))
  for (const h of hosts) if (!allow.includes(h)) rec.notes.push(`host skipped by egress screen: ${h}`)
  try {
    const r = await readAnnounces(allow, { net }, { tracker, net, timeoutMs: 10_000 })
    rec.unreachable = r.unreachable
    rec.withheld = r.withheld
    rec.hostsUp = allow.length - r.unreachable.length
    const pubs = new Set()
    for (const it of r.items) {
      const a = it.payload?.body?.announce
      if (a === undefined) continue
      pubs.add(a.pub)
      rec.announces.txids.push(it.txid)
      rec.announces.items.push({
        txid: it.txid,
        pub: a.pub,
        ts: a.ts,
        exp: a.exp,
        streamSeq: a.streamSeq,
        hosts: a.hosts,
        packs: (a.packs ?? []).map((p) => ({ shard: p.shard, domains: p.domains, store: p.store !== undefined, packUrl: p.packUrl !== undefined }))
      })
    }
    rec.announces.publishers = [...pubs]
    rec.announces.count = rec.announces.items.length
    rec.announces.distinctPublishers = pubs.size
  } catch (e) {
    rec.notes.push(`announce read failed: ${e instanceof Error ? e.message : String(e)}`)
  }
} else if (hosts.length === 0) {
  rec.notes.push('no seed hosts resolved — set NEKTAR_SOAK_HOSTS or NEKTAR_MARKET_HOSTS')
}

// ── agent spend/cap snapshot — GET /v1/wallet + /v1/costs (read-only) ──────
for (const spec of (process.env.NEKTAR_SOAK_AGENTS ?? '')
  .split(',')
  .map((s) => s.trim())
  .filter(Boolean)) {
  const eq = spec.indexOf('=')
  const name = eq > 0 ? spec.slice(0, eq) : `agent${Object.keys(rec.agents).length}`
  const parts = (eq > 0 ? spec.slice(eq + 1) : spec).split('@')
  const url = parts.at(0) ?? ''
  const token = parts.at(1)
  const base = url.replace(/\/+$/, '')
  const h = token !== undefined ? { authorization: `Bearer ${token}` } : {}
  const slot = {}
  for (const [key, path] of [
    ['wallet', '/v1/wallet'],
    ['costs', '/v1/costs']
  ]) {
    try {
      const res = await fetch(`${base}${path}`, { headers: h, signal: AbortSignal.timeout(8_000) })
      slot[key] = res.ok ? await res.json() : { error: `HTTP ${res.status}` }
    } catch (e) {
      slot[key] = { error: e instanceof Error ? e.message : String(e) }
    }
  }
  rec.agents[name] = slot
}

const out = process.env.NEKTAR_SOAK_OUT ?? join(root, '.state', 'soak-report.jsonl')
mkdirSync(dirname(out), { recursive: true })
appendFileSync(out, JSON.stringify(rec) + '\n')

console.log(
  `soak ${net}: ${rec.announces.count} announce(s) from ${rec.announces.distinctPublishers} publisher(s); ` +
    `hosts up ${rec.hostsUp}/${hosts.length}; withheld ${rec.withheld.length}; unreachable ${rec.unreachable.length}` +
    (Object.keys(rec.agents).length > 0
      ? '; agents: ' +
        Object.entries(rec.agents)
          .map(([n, s]) => {
            const w = s.wallet ?? {}
            return `${n} spentToday=${w.spentTodaySats ?? '?'} bal=${w.balanceSats ?? '?'}`
          })
          .join(', ')
      : '')
)
for (const n of rec.notes) console.log(`  note: ${JSON.stringify(n)}`)
console.log(`  → ${out}`)
