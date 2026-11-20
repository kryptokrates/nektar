// pages.ts — the HTML pages of the HTTP surface. /dashboard is the operator
// audit surface, skinned with the installed @tabler/core kit (served from
// node_modules at /dashboard/tabler.css) and themed on the logo.svg palette:
// amber #f7991b on dark #221e1d. Cards are ordered by the operator's reason
// for running nektar: earn → prove → sell → identity → automation.
// /v1/verify's check page + /v1/badge stay self-contained — a verifier must
// work with zero external resources.
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { nektarEnv } from '@nektar/core'
import type { NektarMemory } from './memory.js'
import { receiptToken } from './receipt.js'

// logo.svg — the amber drop on its dark field is the whole brand palette
const NEKTAR_AMBER = '#f7991b'
const NEKTAR_DARK = '#221e1d'

let tablerBuf: Buffer | null | undefined
/** tabler.min.css via the declared dependency — resolved once, then cached */
export function tablerCss(): Buffer | null {
  if (tablerBuf === undefined) {
    try {
      tablerBuf = readFileSync(createRequire(import.meta.url).resolve('@tabler/core/dist/css/tabler.min.css'))
    } catch {
      tablerBuf = null
    }
  }
  return tablerBuf
}

const esc = (s: unknown) =>
  (s === null || s === undefined ? '' : typeof s === 'object' ? JSON.stringify(s) : String(s as string | number | boolean)).replace(
    /[&<>"]/g,
    (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]!
  )

// --tblr-* overrides replay logo.svg onto the kit — the only theme work done;
// every class used below is the kit's own. .kv/.stream are the two bespoke
// bits (record rows + the SSE event tail).
const THEME = `:root,[data-bs-theme=dark]{--tblr-primary:${NEKTAR_AMBER};--tblr-primary-fg:${NEKTAR_DARK};--tblr-body-bg:${NEKTAR_DARK};
--tblr-bg-surface:#2c2624;--tblr-bg-surface-secondary:#262120;--tblr-bg-surface-tertiary:#332c29;
--tblr-body-color:#eae6e3;--tblr-secondary:#a2958a;--tblr-muted:#a2958a;
--tblr-border-color:#3a332f;--tblr-border-color-translucent:rgba(247,153,27,.14)}
a{color:var(--tblr-primary)}
.kv{display:flex;align-items:center;gap:.75rem;padding:.5rem 0;border-bottom:1px solid var(--tblr-border-color-translucent);font-size:.875rem}
.kv:last-child{border-bottom:none}
.stream{margin-top:.5rem;padding-top:.5rem;border-top:1px solid var(--tblr-border-color-translucent);font-size:11px;color:var(--tblr-secondary);min-height:1em;max-height:4.2em;overflow:hidden;font-family:var(--tblr-font-monospace)}
.stream div{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.stream .t{color:var(--tblr-primary)}
code{background:var(--tblr-bg-surface-tertiary);padding:1px 6px;border-radius:4px}`

// emitted only when tabler.min.css can't be resolved — keeps the page readable
const FALLBACK = `body{background:${NEKTAR_DARK};color:#eae6e3;font-family:system-ui,sans-serif;margin:0}
.page{display:flex}.navbar-vertical{width:15rem;min-height:100vh;padding:1rem;border-right:1px solid #3a332f;flex:none}
.navbar-brand a{color:${NEKTAR_AMBER};font-weight:700;text-decoration:none;display:flex;gap:.5rem;align-items:center}
.nav-link{display:flex;gap:.5rem;align-items:center;padding:.4rem .5rem;color:#eae6e3;text-decoration:none;border-radius:.4rem}
.nav-link:hover{background:rgba(247,153,27,.08)}.nav-link-icon svg{width:20px;height:20px;vertical-align:middle}
.page-wrapper{flex:1}.page-header{padding:1.5rem 2rem 0}.page-pretitle{font-size:.75rem;text-transform:uppercase;letter-spacing:.04em;color:#a2958a}
.page-title{font-size:1.5rem;font-weight:700;margin:0}.page-body{padding:1.5rem 2rem}
.row-cards{display:grid;grid-template-columns:1fr 1fr;gap:1rem}.col-12{grid-column:1/-1}
@media (max-width:768px){.page{flex-direction:column}.navbar-vertical{width:auto;min-height:0}.row-cards{grid-template-columns:1fr}}
.card{background:#2c2624;border:1px solid rgba(247,153,27,.14);border-radius:.5rem}
.card-header{display:flex;justify-content:space-between;align-items:center;padding:.8rem 1.25rem;border-bottom:1px solid rgba(247,153,27,.14)}
.card-title{font-size:1rem;font-weight:600;margin:0}.card-body{padding:1rem 1.25rem}
.status{display:inline-flex;align-items:center;gap:.4rem;padding:.2rem .6rem;border-radius:999px;font-size:.75rem;background:rgba(255,255,255,.06)}
.status-green{color:#4cc26b}.status-red{color:#e5534b}.status-dot{display:inline-block;width:.5rem;height:.5rem;border-radius:50%;background:currentColor}
.btn{display:inline-block;padding:.2rem .7rem;border-radius:.4rem;border:1px solid #3a332f;color:${NEKTAR_AMBER};background:transparent;font-size:.8rem;cursor:pointer;text-decoration:none}
.btn-primary{background:${NEKTAR_AMBER};color:${NEKTAR_DARK};border-color:transparent}.btn-sm{font-size:.75rem}
.h1{font-size:2rem;font-weight:700;margin:0}.h4{font-size:1rem}.mb-1{margin-bottom:.25rem}.mb-0{margin-bottom:0}
.subheader{font-size:.75rem;text-transform:uppercase;letter-spacing:.04em;color:#a2958a}
.text-secondary{color:#a2958a}.small{font-size:.8rem}.py-2{padding:.5rem 0}.mt-4{margin-top:1.5rem}
.d-flex{display:flex}.align-items-center{align-items:center}.gap-2{gap:.5rem}.gap-3{gap:.75rem}.ms-auto{margin-left:auto}
.container-xl{max-width:100%}.empty{padding:1.5rem;text-align:center;color:#a2958a}`

// stroke icons in @tabler/icons style — hand-drawn so the icon pack stays out
// of the dependency set
const ICONS = {
  overview: '<rect x="4" y="4" width="7" height="7" rx="1"/><rect x="13" y="4" width="7" height="7" rx="1"/><rect x="4" y="13" width="7" height="7" rx="1"/><rect x="13" y="13" width="7" height="7" rx="1"/>',
  log: '<path d="M4 6a8 3 0 0 0 16 0a8 3 0 0 0-16 0"/><path d="M4 6v6a8 3 0 0 0 16 0V6"/><path d="M4 12v6a8 3 0 0 0 16 0v-6"/>',
  earn: '<circle cx="12" cy="12" r="9"/><path d="M14.8 9a2 2 0 0 0-1.8-1h-2a2 2 0 1 0 0 4h2a2 2 0 1 1 0 4h-2a2 2 0 0 1-1.8-1M12 7v10"/>',
  receipts: '<path d="M5 21V5a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2v16l-3-2-2 2-2-2-2 2-2-2-3 2"/><path d="M9 7h6M9 11h6M9 15h4"/>',
  market: '<path d="M3 6v5.2a2 2 0 0 0 .6 1.4l7.7 7.7a2.4 2.4 0 0 0 3.4 0l5.6-5.6a2.4 2.4 0 0 0 0-3.4l-7.7-7.7A2 2 0 0 0 11.2 3H5a2 2 0 0 0-2 3z"/><circle cx="7.5" cy="7.5" r="1"/>',
  agents: '<circle cx="12" cy="8" r="4"/><path d="M5 21c0-3.9 3.1-7 7-7s7 3.1 7 7"/>',
  automation: '<path d="M13 2 3 14h7l-1 8 10-12h-7l1-8z"/>',
  api: '<path d="M4 6h16M4 12h16M4 18h10"/>',
  health: '<path d="M12 21c-4-3.5-8-6.5-8-10a4.5 4.5 0 0 1 8-3 4.5 4.5 0 0 1 8 3c0 3.5-4 6.5-8 10z"/>',
  agent: '<rect x="5" y="8" width="14" height="10" rx="2"/><path d="M12 8V4M8 4h8M9 13h.01M15 13h.01"/>'
}
const icon = (p: string) =>
  `<svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">${p}</svg>`

const LOGO_SVG = `<svg viewBox="0 0 400 400" width="38" height="38" style="border-radius:8px;flex:none"><rect width="400" height="400" fill="${NEKTAR_DARK}"/><path fill="${NEKTAR_AMBER}" d="M 195 55 C 178 55 164 63 157 80 C 153 100 149 112 143 124 C 136 139 115 158 93 186 C 80 206 73 226 72 250 C 71 277 79 306 98 329 C 117 352 150 372 190 377 C 232 380 255 356 259 322 C 261 296 274 273 296 251 C 310 234 318 210 318 186 C 318 158 312 137 302 120 C 292 102 276 84 260 74 C 245 64 218 55 195 55 Z"/><circle cx="307" cy="78" r="38" fill="${NEKTAR_AMBER}"/><ellipse cx="163" cy="190" rx="20" ry="21" fill="${NEKTAR_DARK}"/><ellipse cx="231" cy="190" rx="20" ry="21" fill="${NEKTAR_DARK}"/><path d="M 150 265.5 C 161 279 176 287 196 288 C 216 287 230 278 243 265" fill="none" stroke="${NEKTAR_DARK}" stroke-width="24" stroke-linecap="round"/></svg>`

export interface HttpOps {
  wallet: () => Promise<unknown>
  costs: () => unknown
}

/** /dashboard — the operator audit surface. One page, live via SSE; every
 *  block degrades to an honest empty state. */
export async function dashboardPage(mem: NektarMemory, ops: HttpOps | undefined, host: string | undefined): Promise<string> {
  const d = await mem.doctor()
  if (ops?.wallet) {
    const w = (await ops.wallet()) as Record<string, unknown>
    d.funding = { ...w, funded: Number(w.balanceSats ?? 0) > 0 }
  }
  const base = nektarEnv('NEKTAR_VERIFY_BASE') ?? `http://${host ?? 'localhost'}`
  // receipts — newest last N
  const rrecs = (await mem.records('__receipts__').catch(() => [] as Array<{ seq: number; data?: string }>)) as Array<{ seq: number; data?: string }>
  const receiptRows = rrecs
    .slice(-10)
    .reverse()
    .map((r) => {
      let amt = '',
        tool = ''
      const m = r.data?.match(/^receipt:[0-9a-f]+:([A-Za-z0-9_-]+)$/)
      if (m) {
        try {
          const j = JSON.parse(Buffer.from(m[1], 'base64url').toString())
          amt = String(j.payment?.amountSats ?? j.payment?.amount ?? '')
          tool = String(j.tool ?? j.toolName ?? '')
        } catch {}
      }
      return `<div class="kv"><span class="text-secondary">#${r.seq}</span><span>${esc(tool || 'answer')}</span><span>${amt ? esc(amt) + ' sats' : ''}</span><a class="btn btn-sm ms-auto" href="${base}/v1/verify?s=${r.seq}&format=html">verify ↗</a></div>`
    })
    .join('')
  // market — offerable shards
  const cat = await mem.shardCatalog().catch(() => [] as Array<{ shard: string; domains: string[]; offers?: Array<{ kind: string; priceSats: number }> }>)
  const marketRows = cat
    .map(
      (m) =>
        `<div class="kv"><b>${esc(m.shard)}</b><span class="text-secondary">${esc(m.domains.join(', '))}</span><span class="ms-auto">${esc(m.offers?.map((o) => `${o.kind} ${o.priceSats}ⓢ`).join(' · ') ?? '')}</span></div>`
    )
    .join('')
  const agentsRows = `<div class="kv"><b>${esc(nektarEnv('NEKTAR_MODEL_ID') ?? 'this agent')}</b><span class="text-secondary">${esc(mem.operatorKey().toPublicKey().toString().slice(0, 16))}…</span></div>`
  const f = d.funding
  const bal = Number(f.balanceSats ?? f.satoshis ?? 0)
  const up = process.uptime()
  const upStr = up > 86400 ? `${Math.floor(up / 86400)}d` : up > 3600 ? `${Math.floor(up / 3600)}h` : `${Math.floor(up / 60)}m`
  const badge = (on: boolean, yes: string, no: string) => `<span class="status ${on ? 'status-green' : ''}">${on ? yes : no}</span>`
  const au = mem.automation
  const pending = au ? await au.store.approvals('pending') : []
  const autoOps = au
    ? (await au.store.operations())
        .filter((o) => o.kind !== 'enable')
        .slice(-8)
        .reverse()
    : []
  const automationCard = au
    ? `<div class="col-12" id="automation"><div class="card">
<div class="card-header"><h3 class="card-title">Automation</h3><div class="card-actions text-secondary small">${pending.length} pending approval${pending.length === 1 ? '' : 's'}</div></div>
<div class="card-body">
${pending.length ? pending.map((a) => `<div class="kv"><div><b>${esc(a.reason)}</b><div class="text-secondary small">≤ ${a.maxExtraSats} sats · ${esc(a.operationId.slice(0, 8))}… · expires ${esc(new Date(a.expiresAt).toISOString())}</div></div><button class="btn btn-sm btn-primary ms-auto" onclick="decide('${esc(a.approvalId)}',true)">approve</button><button class="btn btn-sm" onclick="decide('${esc(a.approvalId)}',false)">deny</button></div>`).join('') : '<div class="text-secondary small py-2">no pending approvals — routine trades do not prompt</div>'}
${autoOps.map((o) => `<div class="kv"><span class="text-secondary">${esc(o.role)} ${esc(o.kind)}</span><b class="ms-auto">${esc(o.state)}</b>${o.lastError ? `<span class="status status-red">${esc(o.lastError)}</span>` : ''}</div>`).join('')}
<div class="stream" data-ev="automation"></div>
</div></div></div>`
    : ''
  const kit = tablerCss() !== null
  const cssLink = kit ? `<link rel="stylesheet" href="/dashboard/tabler.css">` : ''
  const navItem = (href: string, label: string, svg: string, active = false) =>
    `<li class="nav-item${active ? ' active' : ''}"><a class="nav-link${active ? ' active' : ''}" href="${href}"><span class="nav-link-icon">${icon(svg)}</span><span class="nav-link-title">${label}</span></a></li>`
  // the stat row — the four motivations as numbers before any detail
  const stat = (id: string, label: string, value: string, unit: string, sub: string, svg: string, extra = '') => `<div class="col-sm-6 col-xl-3" id="${id}"><div class="card">
<div class="card-body">
<div class="d-flex align-items-center"><div class="subheader">${label}</div><div class="ms-auto"><span style="display:inline-flex;padding:.55rem;border-radius:.5rem;background:rgba(247,153,27,.12);color:var(--tblr-primary)">${icon(svg)}</span></div></div>
<div class="d-flex align-items-baseline"><div class="h1 mb-0 me-2">${value}</div><div class="text-secondary">${unit}</div></div>
<div class="d-flex align-items-center gap-2 mt-2 flex-wrap">${sub}</div>
${extra}
</div></div></div>`
  return `<!doctype html><html data-bs-theme="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="refresh" content="30"><title>nektar — operator</title>
${cssLink}<style>${THEME}${kit ? '' : FALLBACK}</style></head><body><div class="page">
<aside class="navbar navbar-vertical navbar-expand-md" data-bs-theme="dark"><div class="container-fluid">
<h1 class="navbar-brand"><a class="d-flex align-items-center gap-2" style="text-decoration:none" href="/dashboard">${LOGO_SVG}<span>nektar</span></a></h1>
<ul class="navbar-nav pt-3">
${navItem('/dashboard', 'Overview', ICONS.overview, true)}
${navItem('#earn', 'Treasury', ICONS.earn)}
${navItem('#receipts', 'Receipts', ICONS.receipts)}
${navItem('#market', 'Market', ICONS.market)}
${navItem('#agents', 'Agents', ICONS.agents)}
${au ? navItem('#automation', 'Automation', ICONS.automation) : ''}
</ul>
<ul class="navbar-nav mt-auto pb-3">
${navItem('/v1/openapi.json', 'API doc', ICONS.api)}
${navItem('/v1/health', 'Health', ICONS.health)}
${navItem('/.well-known/agent.json', 'Agent card', ICONS.agent)}
</ul>
</div></aside>
<div class="page-wrapper">
<div class="page-header d-print-none"><div class="container-xl"><div class="d-flex align-items-center justify-content-between">
<div><div class="page-pretitle">operator console</div><h2 class="page-title d-flex align-items-center gap-2">Dashboard<span class="status status-green" id="live"><span class="status-dot status-dot-animated"></span>live</span></h2></div>
<div class="d-flex align-items-center gap-3"><div class="text-secondary small">${esc(d.network)} · pub <code>${esc(mem.operatorKey().toPublicKey().toString().slice(0, 24))}…</code> · up ${upStr}</div><a class="btn btn-sm" href="/v1/openapi.json">API</a></div>
</div></div></div>
<div id="app" class="page-body"><div class="container-xl"><div class="row row-cards">
${stat('earn', 'Treasury', bal.toLocaleString(), 'sats', `${badge(!!f.funded, 'funded', 'unfunded')}${badge(d.payments === 'on', 'pay on', 'pay off')}<span class="text-secondary small">${esc(f.utxoCount ?? f.utxos ?? '—')} utxos</span>`, ICONS.earn, '<div class="stream" data-ev="money"></div>')}
${stat('receipts-stat', 'Receipts', String(rrecs.length), 'total', '<span class="text-secondary small">verified answers — each carries a checkable link</span>', ICONS.receipts)}
${stat('market-stat', 'For sale', String(cat.length), 'offers', '<span class="text-secondary small">public shards listed by domain</span>', ICONS.market)}
${stat('log-stat', 'Anchored', String(d.anchoredRecords), `of ${d.records}`, `<span class="text-secondary small">prover: ${esc(d.prover)}</span>${d.unprovenAnchors ? `<span class="status status-red">${d.unprovenAnchors} unproven</span>` : ''}`, ICONS.log)}
<div class="col-md-6" id="receipts"><div class="card">
<div class="card-header"><h3 class="card-title">Receipts</h3><div class="card-actions text-secondary small">${rrecs.length} total</div></div>
<div class="card-body">
${receiptRows || '<div class="empty"><div class="empty-title">no receipts yet</div><div class="empty-subtitle text-secondary">every verified answer lands here with a checkable link</div></div>'}
<div class="stream" data-ev="receipts"></div>
</div></div></div>
<div class="col-md-6" id="market"><div class="card">
<div class="card-header"><h3 class="card-title">Market</h3><div class="card-actions text-secondary small">${cat.length} for sale</div></div>
<div class="card-body">
${marketRows || '<div class="empty"><div class="empty-title">nothing for sale yet</div><div class="empty-subtitle text-secondary">describeShard a public shard to list it</div></div>'}
<div class="stream" data-ev="market"></div>
</div></div></div>
<div class="col-md-6" id="agents"><div class="card">
<div class="card-header"><h3 class="card-title">Agents</h3><div class="card-actions text-secondary small">${esc(nektarEnv('NEKTAR_MODEL_ID') ?? 'unattested')}</div></div>
<div class="card-body">
${agentsRows}
<div class="stream" data-ev="agents"></div>
</div></div></div>
<div class="col-md-6" id="log"><div class="card">
<div class="card-header"><h3 class="card-title">Log</h3><div class="card-actions text-secondary small">${esc(d.network)}</div></div>
<div class="card-body">
<div class="kv"><span>shards</span><b class="ms-auto">${d.shards}</b></div>
<div class="kv"><span>records</span><b class="ms-auto">${d.records}</b></div>
<div class="kv"><span>anchored</span><b class="ms-auto">${d.anchoredRecords}</b></div>
<div class="kv"><span>unproven</span><b class="ms-auto">${d.unprovenAnchors}</b></div>
<div class="kv"><span>prover</span><span class="ms-auto text-secondary">${esc(d.prover)}</span></div>
</div></div></div>
${automationCard}
</div></div></div>
<script>
const decide = async (id, approve) => {
  const tok = sessionStorage.getItem('nektar-operator-token') ?? prompt('operator token (NEKTAR_OPERATOR_TOKEN — not the agent bearer)')
  if (!tok) return
  sessionStorage.setItem('nektar-operator-token', tok)
  const r = await fetch('/v1/automation/approvals/' + id, { method: 'POST', headers: { 'content-type': 'application/json', authorization: 'Bearer ' + tok }, body: JSON.stringify({ approve }) })
  if (!r.ok) alert((await r.json()).error ?? r.status)
  reload()
}
const reload = async () => {
  try {
    const t = await (await fetch(location.pathname, { cache: 'no-store' })).text()
    const m = t.match(/<div id="app"[^>]*>([\\s\\S]*)<\\/div>\\s*<script>/)
    if (m) document.getElementById('app').innerHTML = m[1]
  } catch {}
}
const PANEL = { remember: 'market', describeShard: 'market', groundedAnswer: 'receipts', commitReceipt: 'receipts', attestInference: 'receipts', recall: 'receipts', payInvoice: 'money', payQuote: 'money', sale: 'money', claim: 'money', fundingImport: 'money', importRecord: 'agents', importShard: 'agents' }
const stamp = e => '<div><span class="t">' + new Date().toLocaleTimeString() + '</span> ' + e + '</div>'
const es = new EventSource('/dashboard/events')
es.onmessage = m => {
  const name = m.data || 'change', panel = PANEL[name.split('#')[0]] ?? 'market'
  const s = document.querySelector('.stream[data-ev="' + panel + '"]')
  if (s) { s.insertAdjacentHTML('afterbegin', stamp(name)); while (s.children.length > 4) s.lastChild.remove() }
  reload()
}
es.onerror = () => { document.getElementById('live').className = 'status status-red' }
es.onopen = () => { document.getElementById('live').className = 'status status-green' }
</script>
</div></div></body></html>`
}

/** shields-style badge — the always-visible verification label */
export function badgeSvg(value: string, color: string): string {
  const esc = (s: string) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  const lw = 52,
    vw = Math.max(60, value.length * 6.5 + 14)
  return `<svg xmlns="http://www.w3.org/2000/svg" width="${lw + vw}" height="20" role="img" aria-label="nektar: ${esc(value)}"><title>nektar: ${esc(value)}</title><rect width="${lw}" height="20" fill="${NEKTAR_DARK}" rx="3"/><rect x="${lw}" width="${vw}" height="20" fill="${color}" rx="3"/><rect x="${lw}" width="4" height="20" fill="${color}"/><g text-anchor="middle" font-family="Verdana,DejaVu Sans,sans-serif" font-size="11"><text x="${lw / 2}" y="14" fill="${NEKTAR_AMBER}">nektar</text><text x="${lw + vw / 2}" y="14" fill="#fff">${esc(value)}</text></g></svg>`
}

/** Minimal self-contained check page (no external resources — offline-safe).
 *  The QR encodes this same page URL so a phone scan re-opens the result.
 *  Written for humans: the answer leads, each check is a plain-English
 *  claim, and the raw internals stay visible as fine print for auditors. */
export async function verifyPage(
  r: {
    ok: boolean
    receiptHash: string
    checks: Array<{ name: string; ok: boolean | 'info' | 'skip'; detail: string }>
    receipt?: unknown
    anchorTxid?: string
  },
  pageUrl: string,
  ctx?: { base: string; importShards: string[]; totals: { views: number; anchors: number }; operator?: string }
): Promise<string> {
  const rec = r.receipt as
    | {
        answer?: string
        fragmentRefs?: Array<{ shard: string; seq: number }>
        checkResult?: string
        modelId?: { provider?: string; release?: string }
        modelAttestation?: { verifierTxid?: string; publicStatement?: string }
        payment?: { txid: string; amountSats: number; payTo: string }
      }
    | undefined
  const QR = await import('qrcode')
  const qr = await QR.toDataURL(pageUrl, { margin: 1, width: 180 })
  const esc = (s: string) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  const net = nektarEnv('NEKTAR_NETWORK') ?? 'regtest'
  const netLabel =
    net === 'main'
      ? 'BSV mainnet — real coins'
      : net === 'testnet'
        ? 'BSV public testnet — real transactions, test coins'
        : net === 'regtest'
          ? 'local Teranode regtest'
          : 'simulated chain — demo only'
  const icon = (o: boolean | 'info' | 'skip') => (o === true ? '✓' : o === false ? '✗' : '·')
  const cls = (o: boolean | 'info' | 'skip') => (o === true ? 'ok' : o === false ? 'bad' : 'info')
  // plain-English title per check — the raw detail stays as the sub-line
  const title = (name: string): string =>
    name === 'found'
      ? 'This receipt exists in the log'
      : name === 'hash'
        ? 'The record is unaltered'
        : name === 'provenance'
          ? 'Signed by the service operator'
          : name === 'anchored'
            ? 'Anchored on the blockchain'
            : name === 'fragment (private)'
              ? 'Cited record — private shard (ZK-verified)'
              : name.startsWith('fragment ')
                ? `Cited record ${name.slice(9)} exists on-chain`
                : name === 'grounding'
                  ? 'The answer traces to cited records'
                  : name === 'checkResult'
                    ? 'Grounding verdict'
                    : name === 'paid'
                      ? 'Paid for on-chain'
                      : name === 'model'
                        ? 'Model execution'
                        : name
  const cites = (rec?.fragmentRefs ?? []).map((f) => `${f.shard}:${f.seq}`)
  const meta = rec
    ? [
        cites.length ? `built from ${cites.length} cited record${cites.length === 1 ? '' : 's'} (${esc(cites.join(', '))})` : 'no cited records',
        rec.modelAttestation?.verifierTxid
          ? `model execution: consensus-verified on-chain (Groth16, ${esc(rec.modelAttestation.verifierTxid.slice(0, 12))}…)`
          : rec.modelAttestation
            ? `model execution: statement-attested (${esc((rec.modelAttestation.publicStatement ?? '').slice(0, 14))}… — off-chain)`
            : `declared model: ${esc(rec.modelId?.provider ?? 'undeclared')}/${esc(rec.modelId?.release ?? 'undeclared')} (declared, not proven)`,
        rec.payment ? `paid ${rec.payment.amountSats} sats` : null
      ]
        .filter(Boolean)
        .join(' · ')
    : ''
  const tok = r.receiptHash ? receiptToken(r.receiptHash) : null
  const badgeUrl = tok ? `${new URL(pageUrl).origin}/v1/badge?r=${tok}` : null
  const badgeMd = badgeUrl ? `[![verified answer](${badgeUrl})](${pageUrl.replace('&format=html', '')})` : null
  const ogDesc = r.ok
    ? `${r.checks.filter((c) => c.ok === true).length}/${r.checks.length} checks passed — committed, unaltered, anchored, derivable. Never truth.`
    : `receipt failed verification — ${r.checks.filter((c) => c.ok === false).length} check(s) broken`
  const explorer =
    net === 'main'
      ? 'https://bananablocks.com'
      : net === 'stn'
        ? null // no bananablocks equivalent on STN
        : net === 'testnet' || net === 'test'
          ? 'https://test.bananablocks.com'
          : null
  const anchorUrl = explorer && r.anchorTxid ? `${explorer}/tx/${r.anchorTxid}` : null
  return `<!doctype html><html><head><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1"><title>${r.ok ? '✓ verified answer' : '✗ receipt failed'}${rec?.answer ? ` — ${esc(rec.answer.slice(0, 60))}` : ''}</title>
<meta property="og:title" content="${r.ok ? '✓ verified answer receipt' : '✗ receipt failed verification'} — nektar">
<meta property="og:description" content="${esc(ogDesc)}">
<meta property="og:type" content="website"><meta property="og:url" content="${esc(pageUrl)}">
<meta name="twitter:card" content="summary">
<style>body{font:15px/1.55 system-ui,sans-serif;background:${NEKTAR_DARK};color:#eae6e3;max-width:600px;margin:24px auto;padding:0 16px}
h1{font-size:19px;margin-bottom:2px}.net{color:#a2958a;font-size:12px;margin-bottom:14px}.mono{font-family:ui-monospace,monospace;font-size:11px;word-break:break-all}
.badge{display:inline-block;padding:3px 12px;border-radius:5px;font-weight:700}
.ok{background:#12331c;color:#4ade80}.bad{background:#3d1215;color:#f87171}.info{color:#a2958a}
.lede{color:#a2958a;font-size:14px;margin:0 0 16px}
.ans{background:#2c2624;border:1px solid #3a332f;border-radius:8px;padding:16px 18px;margin:0 0 6px;font-size:17px;line-height:1.5}
.meta{color:#a2958a;font-size:12.5px;margin:10px 0 0}
ul{list-style:none;padding:0}li{margin:10px 0;font-weight:600}
.d{color:#a2958a;font-size:12.5px;font-weight:400;margin-left:22px}
img{display:block;margin:22px auto 6px;border:6px solid #fff;border-radius:6px}
.qrcap{text-align:center;color:#6e7681;font-size:11.5px}.qrcap a{color:${NEKTAR_AMBER};text-decoration:none}
.sharebtn{display:block;width:100%;margin:18px 0 4px;padding:15px;background:${NEKTAR_AMBER};color:${NEKTAR_DARK};border:0;border-radius:8px;font-size:16px;font-weight:700;cursor:pointer}
.sharebtn:hover{background:#e08a0f}.sharebtn.done{background:#12331c;color:#4ade80}
.md{display:block;background:#2c2624;border:1px solid #3a332f;border-radius:6px;padding:8px 10px;margin-top:8px;font:11.5px ui-monospace,monospace;color:#a2958a;word-break:break-all}
details{margin:16px 0;color:#a2958a}summary{cursor:pointer;font-size:13px}details>div{margin-top:10px}</style></head><body>
<h1>Answer receipt <span class="badge ${cls(r.ok)}">${r.ok ? 'VERIFIED' : 'FAILED'}</span></h1>
<div class=net>${esc(netLabel)}${ctx?.operator ? ` · operator <span class=mono>${esc(ctx.operator)}</span>` : ''}</div>
<div class=lede>${
    r.ok
      ? 'This answer was recorded in a signed, append-only log on the blockchain — unchanged and anchored. <b>Recorded ≠ true.</b>'
      : 'Something in this receipt did not check out — see the ✗ lines below.'
  }</div>
${rec?.answer !== undefined ? `<div class=ans>${esc(rec.answer)}</div>` : ''}
<button class=sharebtn id=sharebtn>Share this verification</button>
<script>document.getElementById('sharebtn').onclick=async function(){var u=${JSON.stringify(pageUrl.replace('&format=html', ''))};this.className='sharebtn';if(navigator.share){try{await navigator.share({title:'verified answer — nektar',url:u});this.textContent='\\u2713 shared';this.className='sharebtn done';return}catch(e){if(e&&e.name==='AbortError')return}}try{await navigator.clipboard.writeText(u);this.textContent='\\u2713 link copied';this.className='sharebtn done'}catch(e){this.textContent=u}}</script>
<img src="${qr}" alt="scan to re-verify"><div class=qrcap>scan to re-check</div>
${anchorUrl ? `<div class=qrcap><a href="${anchorUrl}" target=_blank rel=noopener>view the anchor transaction ↗</a></div>` : ''}
<details${r.ok ? '' : ' open'}><summary>details · ${r.checks.filter((c) => c.ok === true).length}/${r.checks.filter((c) => c.ok !== 'info' && c.ok !== 'skip').length} checks</summary><div>
<ul>${r.checks.map((c) => `<li><span class="${cls(c.ok)}">${icon(c.ok)}</span> ${esc(title(c.name))}<div class=d>${esc(c.detail)}</div></li>`).join('')}</ul>
${meta ? `<div class=meta>${meta}</div>` : ''}
<code class=md>${esc('git clone https://github.com/kryptokrates/nektar && pnpm mcp:install')}</code>
${badgeMd ? `<code class=md>${esc(badgeMd)}</code>` : ''}
</div></details>
</body></html>`
}
