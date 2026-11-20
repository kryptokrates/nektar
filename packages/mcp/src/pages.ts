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
export const NEKTAR_AMBER = '#f7991b'
export const NEKTAR_DARK = '#221e1d'

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
  (s === null || s === undefined ? '' : typeof s === 'object' ? JSON.stringify(s) : String(s as string | number | boolean | bigint)).replace(
    /[&<>"]/g,
    (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]!
  )

// --tblr-* overrides replay logo.svg onto the kit. The fallback block keeps a
// usable page when the kit's assets can't be resolved (bare installs).
const THEME = `:root,[data-bs-theme=dark]{--tblr-primary:${NEKTAR_AMBER};--tblr-primary-fg:${NEKTAR_DARK};--tblr-body-bg:${NEKTAR_DARK};
--tblr-bg-surface:#2c2624;--tblr-bg-surface-secondary:#262120;--tblr-bg-surface-tertiary:#332c29;
--tblr-body-color:#eae6e3;--tblr-secondary:#a2958a;--tblr-muted:#a2958a;
--tblr-border-color:#3a332f;--tblr-border-color-translucent:rgba(247,153,27,.14)}
body{font-family:var(--tblr-font-monospace)}a{color:var(--tblr-primary)}
.kv{display:flex;align-items:center;gap:.75rem;padding:.45rem 0;border-bottom:1px solid var(--tblr-border-color-translucent);font-size:.8125rem}
.kv:last-child{border-bottom:none}.kv .ms-auto{margin-left:auto}
.stream{margin-top:.75rem;padding-top:.5rem;border-top:1px solid var(--tblr-border-color-translucent);font-size:11px;color:var(--tblr-secondary);min-height:1em;max-height:4.2em;overflow:hidden}
.stream div{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.stream .t{color:var(--tblr-primary)}
code{background:var(--tblr-bg-surface-tertiary);padding:1px 6px;border-radius:4px}
.page{max-width:880px;margin:0 auto}.row-cards{display:grid;grid-template-columns:1fr 1fr;gap:1rem}
@media (max-width:640px){.row-cards{grid-template-columns:1fr}}
.card{background:var(--tblr-bg-surface);border:1px solid var(--tblr-border-color-translucent);border-radius:.5rem;padding:1rem 1.25rem}
.card-header{display:flex;justify-content:space-between;align-items:baseline;margin-bottom:.75rem}
.subheader{font-size:.75rem;text-transform:uppercase;letter-spacing:.04em;color:var(--tblr-secondary)}
.status{display:inline-flex;align-items:center;gap:.4rem;padding:.2rem .6rem;border-radius:999px;font-size:.75rem;background:rgba(255,255,255,.06)}
.status-green{color:#4cc26b}.status-red{color:#e5534b}.status-dot{display:inline-block;width:.5rem;height:.5rem;border-radius:50%;background:currentColor}
.btn{display:inline-block;padding:.2rem .7rem;border-radius:.4rem;border:1px solid var(--tblr-border-color);color:${NEKTAR_AMBER};background:transparent;font-size:.8rem;cursor:pointer}
.btn-primary{background:${NEKTAR_AMBER};color:${NEKTAR_DARK};border-color:transparent}`

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
    ? `<div class="col-12"><div class="card">
<div class="card-header"><span class="subheader">Automation</span><span class="text-secondary small">${pending.length} pending approval${pending.length === 1 ? '' : 's'}</span></div>
${pending.length ? pending.map((a) => `<div class="kv"><div><b>${esc(a.reason)}</b><div class="text-secondary small">≤ ${a.maxExtraSats} sats · ${esc(a.operationId.slice(0, 8))}… · expires ${esc(new Date(a.expiresAt).toISOString())}</div></div><button class="btn btn-sm btn-primary ms-auto" onclick="decide('${esc(a.approvalId)}',true)">approve</button><button class="btn btn-sm" onclick="decide('${esc(a.approvalId)}',false)">deny</button></div>`).join('') : '<div class="text-secondary small py-2">no pending approvals — routine trades do not prompt</div>'}
${autoOps.map((o) => `<div class="kv"><span class="text-secondary">${esc(o.role)} ${esc(o.kind)}</span><b class="ms-auto">${esc(o.state)}</b>${o.lastError ? `<span class="status status-red">${esc(o.lastError)}</span>` : ''}</div>`).join('')}
<div class="stream" data-ev="automation"></div>
</div></div>`
    : ''
  const cssLink = tablerCss() !== null ? `<link rel="stylesheet" href="/dashboard/tabler.css">` : ''
  return `<!doctype html><html data-bs-theme="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="refresh" content="30"><title>nektar — operator</title>
${cssLink}<style>${THEME}</style></head><body><div class="page">
<header class="container-xl d-flex align-items-center gap-3 py-4">${LOGO_SVG}<div>
<div class="d-flex align-items-center gap-2"><span class="h2 mb-0">nektar</span><span class="status status-green" id="live"><span class="status-dot status-dot-animated"></span>live</span></div>
<div class="text-secondary small">${esc(d.network)} · pub <code>${esc(mem.operatorKey().toPublicKey().toString().slice(0, 24))}…</code> · up ${upStr}</div>
</div></header>
<div id="app" class="page-body"><div class="container-xl"><div class="row row-cards">
<div class="col-md-6"><div class="card">
<div class="card-header"><span class="subheader">Earn</span><span class="d-flex gap-2">${badge(!!f.funded, 'funded', 'unfunded')}${badge(d.payments === 'on', 'pay on', 'pay off')}</span></div>
<div class="h1 mb-1" style="color:var(--tblr-primary)">${bal.toLocaleString()} <small class="h4 text-secondary">sats</small></div>
<div class="text-secondary small">earn sats for verifiable answers · ${esc(f.utxoCount ?? f.utxos ?? '—')} utxos</div>
<div class="stream" data-ev="money"></div>
</div></div>
<div class="col-md-6"><div class="card">
<div class="card-header"><span class="subheader">Receipts</span><span class="text-secondary small">${rrecs.length} total</span></div>
${receiptRows || '<div class="text-secondary small py-2">no receipts yet — every verified answer lands here with a checkable link</div>'}
<div class="stream" data-ev="receipts"></div>
</div></div>
<div class="col-md-6"><div class="card">
<div class="card-header"><span class="subheader">Market</span><span class="text-secondary small">${cat.length} for sale</span></div>
${marketRows || '<div class="text-secondary small py-2">nothing for sale yet — describeShard a public shard to list it</div>'}
<div class="stream" data-ev="market"></div>
</div></div>
<div class="col-md-6"><div class="card">
<div class="card-header"><span class="subheader">Agents</span><span class="text-secondary small">${esc(nektarEnv('NEKTAR_MODEL_ID') ?? 'unattested')}</span></div>
${agentsRows}
<div class="stream" data-ev="agents"></div>
</div></div>
${automationCard}
</div><footer class="text-secondary small mt-4">${d.shards} shards · ${d.records} records · ${d.anchoredRecords} anchored · ${d.unprovenAnchors} unproven · prover: ${esc(d.prover)}</footer></div></div>
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
</div></body></html>`
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
