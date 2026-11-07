#!/usr/bin/env node
// follow.mjs — the reference JetStream consumer: long-poll a rendezvous
// /events feed by cursor and print every signed ad/catalog/want matching
// your domain interests. Ctrl-C to stop.
//
//   node packages/mcp/demo/follow.mjs --domains soccer,ligue-1 \
//     [--rendezvous http://localhost:8090] [--kind catalog] [--since 0] [--wait 25000]
const arg = (k, d) => { const i = process.argv.indexOf('--' + k); return i >= 0 ? process.argv[i + 1] : d }
const base = arg('rendezvous', process.env.NEKTAR_OVERLAY_URL ?? 'http://localhost:8090').replace(/\/+$/, '')
const domains = arg('domains', '')?.split(',').map(s => s.trim()).filter(Boolean)
const kind = arg('kind', undefined)
const wait = Number(arg('wait', 25000))
let cursor = Number(arg('since', -1))

console.log(`following ${base}/events domains=[${(domains ?? []).join(',') || '*'}] kind=${kind ?? '*'} from cursor ${cursor} — Ctrl-C to stop`)
for (;;) {
  const qs = new URLSearchParams({ since: String(cursor), wait: String(wait) })
  if (domains?.length) qs.set('domains', domains.join(','))
  if (kind) qs.set('kind', kind)
  try {
    const r = await fetch(`${base}/events?${qs}`, { signal: AbortSignal.timeout(wait + 20_000) })
    if (!r.ok) { console.error(`events → ${r.status}`); await new Promise(r => setTimeout(r, 2000)); continue }
    const out = await r.json()
    cursor = out.cursor
    for (const e of out.events ?? []) {
      const who = e.pub?.slice(0, 16)
      if (e.kind === 'ad') console.log(`#${e.seq} ad      ${who}… ${e.payload?.ad?.endpoint} shards=[${e.domains.join(',')}]`)
      else if (e.kind === 'catalog') console.log(`#${e.seq} catalog ${who}… claims [${e.domains.join(',')}] (${e.payload?.catalog?.shards?.length ?? 0} shards)`)
      else if (e.kind === 'want') console.log(`#${e.seq} want    ${who}… needs [${e.domains.join(',')}]${e.payload?.want?.maxSats ? ` ≤${e.payload.want.maxSats} sats` : ''}${e.payload?.want?.question ? ` "${e.payload.want.question}"` : ''}`)
    }
  } catch (err) { console.error(`poll error: ${err.message}`); await new Promise(r => setTimeout(r, 2000)) }
}
