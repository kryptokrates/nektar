// pages.ts — the HTML pages of the HTTP surface. /dashboard is the operator
// audit surface, skinned with the installed @tabler/core kit (served from
// node_modules at /dashboard/tabler.css) and themed on the logo.svg palette:
// amber #f7991b on dark #221e1d. The hero is the operator's cold-start loop —
// identity → funded → memory → anchored → verified → earning — and every
// public artifact (agent card, receipts, shard packs) carries a share button.
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
const THEME = `:root,[data-bs-theme=dark]{--tblr-primary:${NEKTAR_AMBER};--tblr-primary-fg:${NEKTAR_DARK};--tblr-body-bg:#1b1715;
--tblr-bg-surface:#2b2421;--tblr-bg-surface-secondary:#241f1c;--tblr-bg-surface-tertiary:#382f2a;
--tblr-body-color:#ece7e4;--tblr-secondary:#a89a8e;--tblr-muted:#a89a8e;
--tblr-border-color:#413630;--tblr-border-color-translucent:rgba(247,153,27,.18)}
a{color:var(--tblr-primary)}
.navbar{background:var(--tblr-bg-surface)}
.card{box-shadow:0 .5rem 1.4rem rgba(0,0,0,.4)}
.card-hero{background:linear-gradient(115deg,#2b2421 0%,#3a2c19 55%,#2b2421 100%)}
.page-pretitle{color:var(--tblr-primary)}
.kv{display:flex;align-items:center;gap:.75rem;padding:.55rem .4rem;border-bottom:1px solid var(--tblr-border-color-translucent);font-size:.875rem}
.kv:last-child{border-bottom:none}.kv:hover{background:rgba(247,153,27,.05)}
.stream{margin-top:.5rem;padding-top:.5rem;border-top:1px solid var(--tblr-border-color-translucent);font-size:11px;color:var(--tblr-secondary);min-height:1em;max-height:4.2em;overflow:hidden;font-family:var(--tblr-font-monospace)}
.stream div{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.stream .t{color:var(--tblr-primary)}
code{background:var(--tblr-bg-surface-tertiary);padding:1px 6px;border-radius:4px}
.glow{position:relative}
.glow::before{content:'';position:absolute;inset:-46px;background:radial-gradient(circle,rgba(247,153,27,.28),transparent 68%);pointer-events:none}
.glow>*{position:relative}
.step{display:flex;flex-direction:column;align-items:center;gap:.35rem;flex:1;position:relative}
.step::before{content:'';position:absolute;top:.55rem;left:-50%;width:100%;height:2px;background:var(--tblr-border-color)}
.step:first-child::before{display:none}
.step.done::before{background:var(--tblr-primary)}
.step i{width:1.1rem;height:1.1rem;border-radius:50%;border:2px solid var(--tblr-border-color);background:var(--tblr-bg-surface);z-index:1;font-style:normal;font-size:.6rem;display:flex;align-items:center;justify-content:center;color:transparent}
.step.done i{background:var(--tblr-primary);border-color:var(--tblr-primary);color:var(--tblr-primary-fg)}
.step.next i{border-color:var(--tblr-primary);box-shadow:0 0 0 .3rem rgba(247,153,27,.15)}
.step span{font-size:.62rem;color:var(--tblr-secondary);text-transform:uppercase;letter-spacing:.03em;white-space:nowrap}
.step.done span{color:var(--tblr-body-color)}
.step.next span{color:var(--tblr-primary)}`

// emitted only when tabler.min.css can't be resolved — keeps the page readable
const FALLBACK = `body{background:${NEKTAR_DARK};color:#eae6e3;font-family:system-ui,sans-serif;margin:0}
.navbar{display:flex;align-items:center;padding:.6rem 1.5rem;border-bottom:1px solid #3a332f}
.navbar-brand a{color:${NEKTAR_AMBER};font-weight:700;text-decoration:none;display:flex;gap:.5rem;align-items:center}
.navbar-nav{display:flex;list-style:none;gap:.25rem;padding:0;margin:0}
.nav-link{display:flex;gap:.4rem;align-items:center;padding:.4rem .6rem;color:#eae6e3;text-decoration:none;border-radius:.4rem}
.nav-link:hover,.nav-item.active .nav-link{background:rgba(247,153,27,.1);color:${NEKTAR_AMBER}}
.page-wrapper{max-width:1200px;margin:0 auto}.page-header{padding:1.5rem 1.5rem 0}.page-body{padding:1rem 1.5rem 2rem}
.page-pretitle{font-size:.75rem;text-transform:uppercase;letter-spacing:.04em;color:#a2958a}
.page-title{font-size:1.5rem;font-weight:700;margin:0}
.row-cards{display:grid;grid-template-columns:1fr 1fr;gap:1rem}
.col-xl-3,.col-sm-6{grid-column:span 1}.col-xl-6{grid-column:1/-1}.col-12{grid-column:1/-1}.col-md-6{grid-column:span 1}
@media (max-width:768px){.row-cards{grid-template-columns:1fr}.col-xl-3{grid-column:1/-1}}
.card{background:#2c2624;border:1px solid rgba(247,153,27,.14);border-radius:.5rem}
.card-header{display:flex;justify-content:space-between;align-items:center;padding:.8rem 1.25rem;border-bottom:1px solid rgba(247,153,27,.14)}
.card-title{font-size:1rem;font-weight:600;margin:0}.card-body{padding:1rem 1.25rem}.h-100{height:100%}
.status{display:inline-flex;align-items:center;gap:.4rem;padding:.2rem .6rem;border-radius:999px;font-size:.75rem;background:rgba(255,255,255,.06)}
.status-green{color:#4cc26b}.status-red{color:#e5534b}.status-dot{display:inline-block;width:.5rem;height:.5rem;border-radius:50%;background:currentColor}
.btn{display:inline-block;padding:.35rem .8rem;border-radius:.4rem;border:1px solid #3a332f;color:#eae6e3;background:transparent;font-size:.8rem;cursor:pointer;text-decoration:none}
.btn-primary{background:${NEKTAR_AMBER};color:${NEKTAR_DARK};border-color:transparent}.btn-sm{font-size:.75rem}
.h1{font-size:2rem;font-weight:700;margin:0}.h2{font-size:1.5rem;font-weight:700}.h3{font-size:1.25rem;font-weight:600}.h4{font-size:1rem}
.subheader{font-size:.75rem;text-transform:uppercase;letter-spacing:.04em;color:#a2958a}
.text-secondary{color:#a2958a}.small{font-size:.8rem}.empty{padding:1.5rem;text-align:center;color:#a2958a}
.d-flex{display:flex}.flex-row{flex-direction:row}.flex-wrap{flex-wrap:wrap}.flex-grow-1{flex-grow:1}.d-none{display:none}
.align-items-center{align-items:center}.align-items-baseline{align-items:baseline}.justify-content-center{justify-content:center}.justify-content-between{justify-content:space-between}
.ms-auto{margin-left:auto}.me-2{margin-right:.5rem}.me-4{margin-right:1.5rem}.mt-auto{margin-top:auto}
.mt-1{margin-top:.25rem}.mt-2{margin-top:.5rem}.mt-3{margin-top:1rem}.mb-0{margin-bottom:0}.mb-1{margin-bottom:.25rem}.mb-2{margin-bottom:.5rem}.mb-4{margin-bottom:1.5rem}
.gap-1{gap:.25rem}.gap-2{gap:.5rem}.gap-3{gap:.75rem}.gap-4{gap:1rem}.gap-5{gap:2rem}.w-100{width:100%}.py-2{padding:.5rem 0}
.container-xl{width:100%}.pt-3{padding-top:1rem}.pb-3{padding-bottom:1rem}
.progress{height:.5rem;background:#3a332f;border-radius:.25rem;overflow:hidden}.progress-bar{height:100%;background:${NEKTAR_AMBER}}
.step{display:flex;flex-direction:column;align-items:center;gap:.35rem;flex:1}
.step i{width:1.1rem;height:1.1rem;border-radius:50%;border:2px solid #3a332f;font-style:normal;font-size:.6rem;display:flex;align-items:center;justify-content:center;color:transparent}
.step.done i{background:${NEKTAR_AMBER};border-color:${NEKTAR_AMBER};color:${NEKTAR_DARK}}
.step.next i{border-color:${NEKTAR_AMBER}}
.step span{font-size:.62rem;color:#a2958a;text-transform:uppercase}
.list-unstyled{list-style:none;padding:0}`

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
  agent: '<rect x="5" y="8" width="14" height="10" rx="2"/><path d="M12 8V4M8 4h8M9 13h.01M15 13h.01"/>',
  copy: '<rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h2"/>'
}
const icon = (p: string) =>
  `<svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">${p}</svg>`

const logoSvg = (px: number) => `<svg viewBox="0 0 400 400" width="${px}" height="${px}" style="border-radius:${Math.round(px / 5)}px;flex:none"><rect width="400" height="400" fill="${NEKTAR_DARK}"/><path fill="${NEKTAR_AMBER}" d="M 195 55 C 178 55 164 63 157 80 C 153 100 149 112 143 124 C 136 139 115 158 93 186 C 80 206 73 226 72 250 C 71 277 79 306 98 329 C 117 352 150 372 190 377 C 232 380 255 356 259 322 C 261 296 274 273 296 251 C 310 234 318 210 318 186 C 318 158 312 137 302 120 C 292 102 276 84 260 74 C 245 64 218 55 195 55 Z"/><circle cx="307" cy="78" r="38" fill="${NEKTAR_AMBER}"/><ellipse cx="163" cy="190" rx="20" ry="21" fill="${NEKTAR_DARK}"/><ellipse cx="231" cy="190" rx="20" ry="21" fill="${NEKTAR_DARK}"/><path d="M 150 265.5 C 161 279 176 287 196 288 C 216 287 230 278 243 265" fill="none" stroke="${NEKTAR_DARK}" stroke-width="24" stroke-linecap="round"/></svg>`

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
  const amts: number[] = []
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
      amts.push(Number(amt) || 0)
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
  const cardUrl = `${base}/card`
  const shareEnabled = !/^(0|false|no)$/i.test(nektarEnv('NEKTAR_PACK_SHARE') ?? '1')
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
  const coverage = d.records > 0 ? Math.round((d.anchoredRecords / d.records) * 100) : 100
  const earned = amts.reduce((a, b) => a + b, 0)
  // cold-start loop — the smallest self-sustaining earning network for one
  // operator; every step is derived from live state, the frontier is named
  const steps = [
    { name: 'identity', done: true },
    { name: 'funded', done: bal > 0 },
    { name: 'memory', done: d.shards > 0 },
    { name: 'anchored', done: d.anchoredRecords > 0 },
    { name: 'verified', done: rrecs.length > 0 },
    { name: 'earning', done: earned > 0 || cat.length > 0 }
  ]
  const firstOpen = steps.findIndex((s) => !s.done)
  const doneCount = steps.filter((s) => s.done).length
  const nextText: Record<string, string> = {
    funded: 'next: fund the treasury — anchors and payouts draw from this book',
    memory: 'next: remember something — the first shard starts sellable memory',
    anchored: 'next: wait for an anchor — a record proves once it lands on-chain',
    verified: 'next: answer verifiably — groundedAnswer issues the first checkable receipt',
    earning: 'next: list a shard for sale — describeShard puts memory on the market'
  }
  const nextLine =
    firstOpen === -1 ? 'the earning loop is live — every link below is checkable by a stranger' : (nextText[steps[firstOpen].name] ?? '')
  const stepper = steps
    .map(
      (s, i) =>
        `<div class="step${s.done ? ' done' : i === firstOpen ? ' next' : ''}"><i>${s.done ? '✓' : ''}</i><span>${s.name}</span></div>`
    )
    .join('')
  // honest visuals only — the sparkline is real receipt amounts, the ring is
  // the true anchored share; nothing decorative masquerading as data
  const donut = (p: number) =>
    `<svg width="76" height="76" viewBox="0 0 36 36"><circle cx="18" cy="18" r="15.5" fill="none" stroke="var(--tblr-bg-surface-tertiary)" stroke-width="3"/><circle cx="18" cy="18" r="15.5" fill="none" stroke="var(--tblr-primary)" stroke-width="3" stroke-linecap="round" stroke-dasharray="${(p * 0.9739 * 100).toFixed(1)} 200" transform="rotate(-90 18 18)"/><text x="18" y="21" text-anchor="middle" fill="var(--tblr-body-color)" font-size="8" font-weight="700">${Math.round(p * 100)}%</text></svg>`
  const spark = (vals: number[]) => {
    const v = [...vals].reverse().slice(-10)
    const max = Math.max(1, ...v)
    const bars = v.map((n, i) => `<rect x="${i * 10 + 2}" y="${28 - Math.round((n / max) * 24)}" width="6" height="${Math.max(2, Math.round((n / max) * 24))}" rx="1" fill="var(--tblr-primary)" opacity="${n > 0 ? 0.9 : 0.25}"/>`).join('')
    return `<svg viewBox="0 0 ${v.length * 10} 28" class="w-100" height="28" preserveAspectRatio="none">${bars || '<rect x="0" y="26" width="100%" height="2" fill="var(--tblr-bg-surface-tertiary)"/>'}</svg>`
  }
  const navItem = (href: string, label: string, svg: string, active = false) =>
    `<li class="nav-item${active ? ' active' : ''}"><a class="nav-link" href="${href}"><span class="nav-link-icon">${icon(svg)}</span><span class="nav-link-title">${label}</span></a></li>`
  const miniStat = (label: string, value: string, right: string, sub: string, svg: string, color: string) => `<div class="col-sm-6 col-xl-3"><div class="card"><div class="card-body">
<div class="d-flex align-items-center"><div class="subheader">${label}</div><div class="ms-auto"><span style="display:inline-flex;align-items:center;justify-content:center;width:2.4rem;height:2.4rem;border-radius:.55rem;background:${color}22;color:${color}">${icon(svg)}</span></div></div>
<div class="h3 mb-1">${value}</div><div class="text-secondary small">${sub}</div>
</div></div></div>`
  return `<!doctype html><html data-bs-theme="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="refresh" content="30"><title>nektar — operator</title>
${cssLink}<style>${THEME}${kit ? '' : FALLBACK}</style></head><body><div class="page">
<header class="navbar navbar-expand-md" data-bs-theme="dark"><div class="container-xl">
<h1 class="navbar-brand me-4"><a class="d-flex align-items-center gap-2" style="text-decoration:none" href="/dashboard">${logoSvg(30)}<span>nektar</span></a></h1>
<div class="navbar-nav flex-row ms-auto align-items-center gap-3">
<span class="status status-green" id="live"><span class="status-dot status-dot-animated"></span>live</span>
<span class="text-secondary small">${esc(d.network)}</span>
<span class="text-secondary small">pub <code>${esc(mem.operatorKey().toPublicKey().toString().slice(0, 24))}…</code></span>
<span class="text-secondary small">up ${upStr}</span>
</div></div></header>
<div class="navbar navbar-expand-md" data-bs-theme="dark"><div class="container-xl"><ul class="navbar-nav">
${navItem('/dashboard', 'Overview', ICONS.overview, true)}
${navItem('#receipts', 'Receipts', ICONS.receipts)}
${navItem('#market', 'Market', ICONS.market)}
${navItem('#agents', 'Agents', ICONS.agents)}
${navItem('#log', 'Log', ICONS.log)}
${au ? navItem('#automation', 'Automation', ICONS.automation) : ''}
<li class="nav-item ms-auto">${''}<a class="nav-link" href="/v1/openapi.json"><span class="nav-link-icon">${icon(ICONS.api)}</span><span class="nav-link-title">API</span></a></li>
</ul></div></div>
<div class="page-wrapper">
<div class="page-header d-print-none"><div class="container-xl"><div class="d-flex align-items-center justify-content-between">
<div><div class="page-pretitle">operator console</div><h2 class="page-title">Dashboard</h2></div>
<div class="d-flex gap-2"><a class="btn" href="/v1/health">Health</a><a class="btn btn-primary" href="/.well-known/nektar">Discovery doc</a></div>
</div></div></div>
<div id="app" class="page-body"><div class="container-xl"><div class="row row-cards">
<div class="col-xl-6"><div class="card h-100 card-hero"><div class="card-body d-flex py-4">
<div class="flex-grow-1">
<div class="d-flex align-items-baseline gap-3 mb-1"><div class="h2 mb-0">${esc(nektarEnv('NEKTAR_MODEL_ID') ?? 'operator')}</div>
<div class="h4 mb-0" style="color:var(--tblr-primary)">${firstOpen === -1 ? 'loop live' : steps[firstOpen].name}</div>
<div class="text-secondary small">${doneCount}/${steps.length}</div></div>
<div class="d-flex mt-3 mb-3">${stepper}</div>
<div class="small mb-4">${esc(nextLine)}</div>
${shareEnabled ? `<div class="mb-4"><button class="btn btn-primary" onclick="shareAgent(this)">share your agent ↗</button></div>` : ''}
<div class="d-flex gap-5 mt-auto">
<div><div class="subheader">Treasury</div><div class="h2 mb-1" style="color:var(--tblr-primary)">${bal.toLocaleString()} <span class="h4 text-secondary">sats</span></div>
<div class="d-flex gap-2">${badge(!!f.funded, 'funded', 'unfunded')}${badge(d.payments === 'on', 'pay on', 'pay off')}</div>
<div class="stream" data-ev="money"></div></div>
<div><div class="subheader">Verifiability</div><div class="h2 mb-1">${coverage}%</div>
<div class="progress progress-sm" style="width:8rem"><div class="progress-bar" style="width:${coverage}%"></div></div>
<div class="text-secondary small mt-1">${d.anchoredRecords} of ${d.records} provable</div></div>
</div></div>
<div class="ms-auto d-none d-lg-flex align-items-center glow">${logoSvg(96)}</div>
</div></div></div>
<div class="col-sm-6 col-xl-3"><div class="card h-100"><div class="card-body">
<div class="subheader">Receipts</div>
<div class="d-flex align-items-baseline"><div class="h1 mb-0 me-2">${rrecs.length}</div><div class="text-secondary">verified answers</div></div>
<div class="text-secondary small mt-1">${earned > 0 ? `${earned.toLocaleString()} sats earned — last ${Math.min(10, amts.length)}` : 'each carries a checkable link'}</div>
<div class="mt-2">${spark(amts)}</div>
</div></div></div>
<div class="col-sm-6 col-xl-3"><div class="card h-100"><div class="card-body">
<div class="subheader">Anchored</div>
<div class="d-flex align-items-baseline"><div class="h1 mb-0 me-2">${d.anchoredRecords}</div><div class="text-secondary">of ${d.records} records</div></div>
<div class="d-flex justify-content-center mt-3">${donut(coverage / 100)}</div>
</div></div></div>
</div>
<div class="row row-cards mt-3">
${miniStat('Earned', `${earned.toLocaleString()} sats`, `${rrecs.length} receipts`, 'from verified answers', ICONS.earn, NEKTAR_AMBER)}
${miniStat('For sale', `${cat.length} offers`, 'public shards', 'listed by domain', ICONS.market, '#4cc26b')}
${miniStat('Utxos', `${esc(f.utxoCount ?? f.utxos ?? '—')}`, 'funding outputs', 'the coin book', ICONS.log, '#58a6ff')}
${miniStat('Shards', `${d.shards}`, 'memory held', `prover: ${esc(d.prover)}`, ICONS.agent, '#b083f0')}
</div>
<div class="row row-cards mt-3">
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
const copyLink = async (btn, u) => {
  try { await navigator.clipboard.writeText(u) } catch {
    const t = document.createElement('textarea'); t.value = u; document.body.appendChild(t); t.select(); document.execCommand('copy'); t.remove()
  }
  const s = btn.textContent; btn.textContent = '\\u2713 copied'; setTimeout(() => { btn.textContent = s }, 1600)
}
const shareAgent = async (btn) => {
  const u = ${JSON.stringify(cardUrl)}
  if (navigator.share) { try { await navigator.share({ title: 'nektar agent — verifiable memory', url: u }); return } catch (e) { if (e && e.name === 'AbortError') return } }
  copyLink(btn, u)
}
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

/** /card — the human twin of /.well-known/nektar: the agent's public
 *  storefront and the single artifact the dashboard's share button
 *  produces. Self-contained like the verify page so it travels alone.
 *  Renders only data already public via well-known + packs — pubkeys,
 *  public shards, offers, proof counts. Never treasury, automation
 *  internals, or private shards. */
export async function cardPage(mem: NektarMemory, host: string | undefined): Promise<string> {
  const base = nektarEnv('NEKTAR_VERIFY_BASE') ?? `http://${host ?? 'localhost'}`
  const pageUrl = `${base}/card`
  const d = await mem.doctor().catch(() => undefined)
  const pub = (await mem.publicShards().catch(() => [] as string[])).filter((s) => !s.startsWith('__'))
  const catalog = await mem.shardCatalog().catch(() => [] as Awaited<ReturnType<NektarMemory['shardCatalog']>>)
  const receipts = await mem
    .records('__receipts__')
    .then((r) => r.length)
    .catch(() => 0)
  const rows = (
    await Promise.all(
      pub.map(async (s) => {
        const m = catalog.find((x) => x.shard === s)
        const n = await mem
          .records(s)
          .then((r) => r.length)
          .catch(() => 0)
        const offers = m?.offers?.map((o) => `${o.kind} ${o.priceSats}ⓢ`).join(' · ') ?? ''
        return `<div class=row><b>${esc(s)}</b><span class=dim>${esc(m?.domains?.join(', ') ?? '')}${m?.desc ? ` — ${esc(m.desc)}` : ''}</span><span class=dim>${n} records${offers ? ` · ${esc(offers)}` : ''}</span><a class=lnk href="${base}/v1/pack/${encodeURIComponent(s)}">pack ↗</a></div>`
      })
    )
  ).join('')
  const QR = await import('qrcode')
  const qr = await QR.toDataURL(pageUrl, { margin: 1, width: 160 })
  const net = nektarEnv('NEKTAR_NETWORK') ?? 'regtest'
  const netLabel =
    net === 'main'
      ? 'BSV mainnet — real coins'
      : net === 'testnet'
        ? 'BSV public testnet — real transactions, test coins'
        : net === 'regtest'
          ? 'local Teranode regtest'
          : 'simulated chain — demo only'
  const coverage = d && d.records > 0 ? Math.round((d.anchoredRecords / d.records) * 100) : 0
  const operator = mem.operatorKey()?.toPublicKey().toString() ?? ''
  const ogDesc = `${d?.records ?? 0} records · ${coverage}% anchored on-chain · ${receipts} receipts issued · ${pub.length} shards for sale`
  return `<!doctype html><html><head><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1"><title>nektar agent — verifiable memory</title>
<meta property="og:title" content="nektar agent card — verifiable memory anchored on BSV">
<meta property="og:description" content="${esc(ogDesc)}">
<meta property="og:type" content="website"><meta property="og:url" content="${esc(pageUrl)}">
<meta name="twitter:card" content="summary">
<style>body{font:15px/1.55 system-ui,sans-serif;background:${NEKTAR_DARK};color:#eae6e3;max-width:600px;margin:24px auto;padding:0 16px}
h1{font-size:19px;margin-bottom:2px;display:flex;align-items:center;gap:10px}.net{color:#a2958a;font-size:12px;margin-bottom:14px;word-break:break-all}
.lede{color:#a2958a;font-size:14px;margin:0 0 16px}
.stat{background:#2c2624;border:1px solid #3a332f;border-radius:8px;padding:16px 18px;margin:0 0 18px}
.stat b{color:${NEKTAR_AMBER};font-size:22px}.stat .dim{font-size:13px}
.row{display:flex;align-items:baseline;gap:8px;padding:9px 0;border-bottom:1px solid #3a332f;font-size:14px;flex-wrap:wrap}
.row:last-child{border-bottom:none}
.dim{color:#a2958a;font-size:12.5px}.lnk{color:${NEKTAR_AMBER};margin-left:auto;text-decoration:none;font-size:12.5px}
h2{font-size:11px;text-transform:uppercase;letter-spacing:.06em;color:#a2958a;margin:20px 0 4px;font-weight:600}
a{color:${NEKTAR_AMBER}}
img{display:block;margin:22px auto 6px;border:6px solid #fff;border-radius:6px}
.qrcap{text-align:center;color:#6e7681;font-size:11.5px}
.sharebtn{display:block;width:100%;margin:18px 0 4px;padding:15px;background:${NEKTAR_AMBER};color:${NEKTAR_DARK};border:0;border-radius:8px;font-size:16px;font-weight:700;cursor:pointer}
.sharebtn:hover{background:#e08a0f}.sharebtn.done{background:#12331c;color:#4ade80}
.md{display:block;background:#2c2624;border:1px solid #3a332f;border-radius:6px;padding:8px 10px;margin-top:8px;font:11.5px ui-monospace,monospace;color:#a2958a;word-break:break-all}</style></head><body>
<h1>${logoSvg(30)} nektar agent</h1>
<div class=net>${esc(netLabel)} · operator <span class=mono>${esc(operator.slice(0, 24))}…</span> · model: ${esc(nektarEnv('NEKTAR_MODEL_ID') ?? 'undeclared')} (declared, not proven)</div>
<div class=lede>Verifiable, encrypted agent memory anchored on BSV. Every answer this agent gives can carry a receipt anyone can check — <b>recorded ≠ true</b>.</div>
<div class=stat><b>${d?.records ?? 0}</b> records · <b>${d?.anchoredRecords ?? 0}</b> anchored on-chain · <b>${coverage}%</b> provable · <b>${receipts}</b> receipts issued</div>
<h2>memory for sale</h2>
${rows || '<div class=dim>nothing public for sale yet — memory warming up</div>'}
<h2>check an answer</h2>
<div class=row><span class=dim>every receipt resolves here — paste a token, get the traffic-light check</span><a class=lnk href="${base}/v1/verify">${esc(base)}/v1/verify ↗</a></div>
<h2>join the network</h2>
<code class=md>${esc(`${base}/.well-known/agent.json`)}</code>
<code class=md>${esc('git clone https://github.com/kryptokrates/nektar && pnpm mcp:install')}</code>
<button class=sharebtn id=sharebtn>Share this agent</button>
<script>document.getElementById('sharebtn').onclick=async function(){var u=${JSON.stringify(pageUrl)};this.className='sharebtn';if(navigator.share){try{await navigator.share({title:'nektar agent — verifiable memory',url:u});this.textContent='\\u2713 shared';this.className='sharebtn done';return}catch(e){if(e&&e.name==='AbortError')return}}try{await navigator.clipboard.writeText(u);this.textContent='\\u2713 link copied';this.className='sharebtn done'}catch(e){this.textContent=u}}</script>
<img src="${qr}" alt="scan to open this card"><div class=qrcap>scan to open this card</div>
</body></html>`
}
