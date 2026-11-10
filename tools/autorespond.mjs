#!/usr/bin/env node
// tools/autorespond.mjs — the seller's always-on half: long-poll the
// rendezvous /events feed for `want` events in our domains and answer with a
// priced replyTo offer via the local instance's HTTP API.
//
//   node tools/autorespond.mjs              run forever (service mode)
//   node tools/autorespond.mjs --once       one poll pass, exit (smoke tests)
//   node tools/autorespond.mjs --price 250  flat fallback price in sats
//   node tools/autorespond.mjs --domains soccer,stats   override domain set
//
// Config: NEKTAR_OVERLAY_URL (required), NEKTAR_HTTP_PORT (default 8080),
// NEKTAR_AUTORESPOND_PRICE_SATS (fallback when a shard's manifest has no
// 'answer' offer), NEKTAR_DATA_DIR (dedup state). Answers OFFERS only — the
// buyer still peerCalls and pays; this never spends.
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const arg = (k, d) => { const i = process.argv.indexOf(k); return i >= 0 ? process.argv[i + 1] : d }
const ONCE = process.argv.includes('--once')
const env = existsSync(join(root, '.env'))
  ? Object.fromEntries(readFileSync(join(root, '.env'), 'utf8').split(/\r?\n/).map(l => l.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)).filter(Boolean).map(m => [m[1], m[2].trim()]))
  : {}
const rv = arg('--rv', process.env.NEKTAR_OVERLAY_URL ?? env.NEKTAR_OVERLAY_URL)?.replace(/\/+$/, '')
if (!rv) { console.error('autorespond: NEKTAR_OVERLAY_URL unset — pnpm config:nektar set NEKTAR_OVERLAY_URL https://rv.nektar.run'); process.exit(2) }
const port = Number(arg('--port', process.env.NEKTAR_HTTP_PORT ?? env.NEKTAR_HTTP_PORT ?? 8080))
const base = `http://127.0.0.1:${port}`
const flatPrice = arg('--price', process.env.NEKTAR_AUTORESPOND_PRICE_SATS ?? env.NEKTAR_AUTORESPOND_PRICE_SATS)
const domainOverride = arg('--domains', process.env.NEKTAR_AUTORESPOND_DOMAINS ?? env.NEKTAR_AUTORESPOND_DOMAINS)?.split(',').map(s => s.trim()).filter(Boolean)
const dataDir = resolve(process.env.NEKTAR_DATA_DIR ?? env.NEKTAR_DATA_DIR ?? join(root, '.nektar'))
const statePath = join(dataDir, 'autorespond.state.json')
const state = existsSync(statePath) ? JSON.parse(readFileSync(statePath, 'utf8')) : { cursor: -1, responded: [], seqs: {} }
const save = () => { mkdirSync(dataDir, { recursive: true }); writeFileSync(statePath, JSON.stringify(state)) }
const responded = new Set(state.responded)
// local instance auth — the HTTP API gates mutating tools when NEKTAR_HTTP_TOKEN
// (or its _FILE variant) is set; the token is never sent to the rendezvous
const tokenFile = process.env.NEKTAR_HTTP_TOKEN_FILE ?? env.NEKTAR_HTTP_TOKEN_FILE
const token = process.env.NEKTAR_HTTP_TOKEN ?? env.NEKTAR_HTTP_TOKEN ?? (tokenFile && existsSync(tokenFile) ? readFileSync(tokenFile, 'utf8').trim() : undefined)
const authHeaders = token ? { authorization: `Bearer ${token}` } : {}

const post = async (tool, args) => {
  const r = await fetch(`${base}/v1/tools/${tool}`, { method: 'POST', headers: { 'content-type': 'application/json', ...authHeaders }, body: JSON.stringify(args), signal: AbortSignal.timeout(20_000) })
  const j = await r.json().catch(() => ({}))
  if (!r.ok) throw new Error(`${tool}: ${j.error ?? r.status}`)
  return j
}

let own = { shards: [], pub: undefined, at: 0 }
const refreshCatalog = async () => {
  const wk = await (await fetch(`${base}/.well-known/nektar`, { headers: authHeaders, signal: AbortSignal.timeout(10_000) })).json()
  const rep = await post('reputation', {}).catch(() => ({}))
  own = { shards: wk.publicShards ?? [], pub: rep.card?.pub ?? rep.pub, at: Date.now() }
}

const domainsOf = s => (s.domains ?? []).map(d => String(d).toLowerCase())
const priceOf = s => s.offers?.find(o => o.kind === 'answer')?.priceSats ?? (flatPrice !== undefined ? Number(flatPrice) : undefined)

console.log(`autorespond — rv=${rv} instance=${base}${ONCE ? ' (single pass)' : ''}`)
for (;;) {
  try {
    if (Date.now() - own.at > 5 * 60_000 || own.shards.length === 0) await refreshCatalog()
    const domains = domainOverride ?? [...new Set(own.shards.flatMap(domainsOf))]
    if (!domains.length) { console.log('no offerable domains — describeShard a public shard first'); if (ONCE) process.exit(1); await new Promise(r => setTimeout(r, 30_000)); continue }
    const qs = new URLSearchParams({ kind: 'want', domains: domains.join(','), since: String(state.cursor), wait: '25000' })
    const r = await fetch(`${rv}/events?${qs}`, { signal: AbortSignal.timeout(45_000) })
    if (!r.ok) { console.error(`events → ${r.status}`); await new Promise(r => setTimeout(r, 2_000)); continue }
    const out = await r.json()
    // fail-closed cursor: only advance past events fully processed — a failed
    // respond leaves the cursor behind so the want is retried next poll
    let done = state.cursor
    for (const e of out.events ?? []) {
      if (e.kind !== 'want') continue
      const want = e.payload?.want ?? {}
      const key = `${e.pub}:${want.wantId ?? `seq${e.seq}`}:${want.revision ?? 0}`
      if (responded.has(key) || (own.pub && e.pub === own.pub)) { done = e.seq; continue }
      const wanted = (e.domains ?? []).map(d => String(d).toLowerCase())
      const shard = own.shards.find(s => wanted.every(w => domainsOf(s).includes(w)))
      if (!shard) { console.log(`#${e.seq} want [${wanted.join(',')}] — no shard covers it`); done = e.seq; continue }
      const price = priceOf(shard)
      if (price === undefined || price <= 0) { console.log(`#${e.seq} want — no price for ${shard.shard} (add an 'answer' offer or --price)`); done = e.seq; continue }
      if (want.maxSats !== undefined && price > Number(want.maxSats)) { console.log(`#${e.seq} want ≤${want.maxSats} — our price ${price} too high`); responded.add(key); done = e.seq; continue }
      if (typeof want.wantId !== 'string' || typeof want.taskHash !== 'string' || !Number.isSafeInteger(want.revision)) { console.log(`#${e.seq} want — no signed want identity, skipping`); done = e.seq; continue }
      try {
        const seq = (state.seqs[shard.shard] = (state.seqs[shard.shard] ?? 0) + 1)
        const res = await post('respond', { wantPub: e.pub, wantId: want.wantId, taskHash: want.taskHash, wantRevision: want.revision, shard: shard.shard, seq, priceSats: price, rendezvousUrl: rv })
        console.log(`#${e.seq} want [${wanted.join(',')}] → offer ${price} sats on ${shard.shard} (${res.pub?.slice(0, 16)}…)`)
        responded.add(key)
        done = e.seq
      } catch (err) { console.error(`respond #${e.seq}: ${err.message}`); break }
    }
    state.cursor = done
    state.responded = [...responded].slice(-10_000)
    save()
    // deferred exit — AbortSignal/fetch handles still closing; an immediate
    // process.exit hits a libuv UV_HANDLE_CLOSING assert on Windows
    if (ONCE) { setTimeout(() => process.exit(0), 250); break }
  } catch (err) {
    console.error(`poll error: ${err.message}`)
    if (ONCE) process.exit(1)
    await new Promise(r => setTimeout(r, 2_000))
  }
}
