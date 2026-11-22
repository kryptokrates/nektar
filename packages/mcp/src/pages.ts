// pages.ts — the HTML pages of the HTTP surface plus the dashboard's JSON
// state. /dashboard itself is the compiled Svelte bundle (dashboard/ →
// dist/dashboard); dashboardState() is the data it renders. The verify check
// page, /v1/badge and /card stay self-contained — they must work with zero
// external resources. Palette comes from logo.svg: amber #f7991b on dark.
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { nektarEnv, DEFAULT_MARKET_HOSTS, marketHostsFromEnv } from '@nektar/core'
import { marketHosts, savedHosts, blockedHosts } from './hosts.js'
import type { NektarMemory } from './memory.js'
import { receiptToken } from './receipt.js'

// the compiled Svelte dashboard — vite emits dist/dashboard/; a missing
// bundle degrades to a one-line notice, never a stack trace
const DASH_DIR = fileURLToPath(new URL('../dashboard/', import.meta.url))
const DASH_MIME: Record<string, string> = {
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.map': 'application/json',
  '.png': 'image/png',
  '.woff2': 'font/woff2'
}
export const dashboardFile = (name: string): { body: Buffer; type: string } | null => {
  try {
    return { body: readFileSync(join(DASH_DIR, name)), type: DASH_MIME[name.slice(name.lastIndexOf('.'))] ?? 'application/octet-stream' }
  } catch {
    return null
  }
}

// logo.svg — the amber drop on its dark field is the whole brand palette
const NEKTAR_AMBER = '#f7991b'
const NEKTAR_DARK = '#221e1d'

const esc = (s: unknown) =>
  (s === null || s === undefined ? '' : typeof s === 'object' ? JSON.stringify(s) : String(s as string | number | boolean | symbol | bigint)).replace(
    // eslint-disable-line @typescript-eslint/no-unnecessary-type-assertion -- narrows unknown past the object guard; required by no-base-to-string
    /[&<>"]/g,
    (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]!
  )

const logoSvg = (px: number) =>
  `<svg viewBox="0 0 400 400" width="${px}" height="${px}" style="border-radius:${Math.round(px / 5)}px;flex:none"><rect width="400" height="400" fill="${NEKTAR_DARK}"/><path fill="${NEKTAR_AMBER}" d="M 195 55 C 178 55 164 63 157 80 C 153 100 149 112 143 124 C 136 139 115 158 93 186 C 80 206 73 226 72 250 C 71 277 79 306 98 329 C 117 352 150 372 190 377 C 232 380 255 356 259 322 C 261 296 274 273 296 251 C 310 234 318 210 318 186 C 318 158 312 137 302 120 C 292 102 276 84 260 74 C 245 64 218 55 195 55 Z"/><circle cx="307" cy="78" r="38" fill="${NEKTAR_AMBER}"/><ellipse cx="163" cy="190" rx="20" ry="21" fill="${NEKTAR_DARK}"/><ellipse cx="231" cy="190" rx="20" ry="21" fill="${NEKTAR_DARK}"/><path d="M 150 265.5 C 161 279 176 287 196 288 C 216 287 230 278 243 265" fill="none" stroke="${NEKTAR_DARK}" stroke-width="24" stroke-linecap="round"/></svg>`

export interface HttpOps {
  wallet: () => Promise<unknown>
  costs: () => unknown
}

/** /dashboard/state — the JSON the compiled Svelte dashboard renders. Same
 *  data the old server-rendered page carried: honest counts, the cold-start
 *  loop frontier, network position, the automation queue. */
/** the Host header is attacker-controlled — it lands in page HTML and
 *  inline <script> JSON (which escapes quotes but NOT angle brackets), so
 *  anything but RFC host[:port] characters is a reflected-XSS vector.
 *  Deviations fall back to localhost rather than being escaped piecemeal. */
const safeHost = (host: string | undefined): string =>
  host !== undefined && /^[A-Za-z0-9.-]+(:\d{1,5})?$|^\[[0-9a-fA-F:]+\](:\d{1,5})?$/.test(host) ? host : 'localhost'
export { safeHost }

/** JSON inside inline <script>: JSON.stringify escapes quotes but leaves
 *  '<'/'>' literal — '</script>' inside the payload breaks out of the
 *  block. Encode the angle brackets so any string is safe to embed. */
const jsStr = (v: string): string => JSON.stringify(v).replace(/</g, '\\u003c').replace(/>/g, '\\u003e')

export async function dashboardState(mem: NektarMemory, ops: HttpOps | undefined, host: string | undefined): Promise<Record<string, unknown>> {
  const d = await mem.doctor()
  if (ops?.wallet) {
    const w = (await ops.wallet()) as Record<string, unknown>
    d.funding = { ...w, funded: Number(w.balanceSats ?? 0) > 0 }
  }
  const base = nektarEnv('NEKTAR_VERIFY_BASE') ?? `http://${safeHost(host)}`
  // records + fragment.d — the writer-side plaintext: recall() withholds
  // text until the anchor verifies, but the operator's own ledger is readable
  const rrecs = (await mem.records('__receipts__').catch(() => [] as Array<{ seq: number; fragment: { d: Uint8Array } }>)) as Array<{
    seq: number
    fragment: { d: Uint8Array }
  }>
  const decoded = rrecs.map((r) => {
    let sats: number | null = null,
      tool = '',
      shards: string[] = [],
      at: number | null = null
    const m = Buffer.from(r.fragment.d)
      .toString('utf8')
      .match(/^receipt:[0-9a-f]+:([A-Za-z0-9_-]+)$/)
    if (m) {
      try {
        const j = JSON.parse(Buffer.from(m[1], 'base64url').toString()) as {
          tool?: string
          toolName?: string
          timestamp?: number
          payment?: { amountSats?: number; amount?: number }
          fragmentRefs?: Array<{ shard: string }>
        }
        sats = Number(j.payment?.amountSats ?? j.payment?.amount) || null
        tool = String(j.tool ?? j.toolName ?? '')
        // the receipt cites the fragments it answered from — works-per-domain
        shards = (j.fragmentRefs ?? []).map((f) => f.shard)
        at = typeof j.timestamp === 'number' ? j.timestamp : null
      } catch {}
    }
    return { seq: r.seq, tool: tool || 'answer', sats, shards, at }
  })
  const receipts = decoded
    .slice(-10)
    .reverse()
    .map((r) => ({ ...r, url: `${base}/v1/verify?s=${r.seq}&format=html` }))
  const amounts = decoded.map((r) => r.sats ?? 0)
  // the 24h delta — only receipts whose own timestamp lands inside the window;
  // untimestamped receipts don't count rather than guess
  const earned24h = decoded.reduce((a, r) => a + (r.at !== null && r.at >= Date.now() - 864e5 ? (r.sats ?? 0) : 0), 0)
  const cat = await mem.shardCatalog().catch(() => [] as Array<{ shard: string; domains: string[]; offers?: Array<{ kind: string; priceSats: number }> }>)
  const f = d.funding
  const bal = Number(f.balanceSats ?? f.satoshis ?? 0)
  const au = mem.automation
  // network position — which market hosts this instance trades through and
  // whether the last probe could reach them (up / down / configured)
  const hosts = marketHosts()
  const saved = new Set(savedHosts())
  const blocked = new Set(blockedHosts())
  const downSet = new Set(typeof au?.health === 'function' ? (au.health().report?.downHosts ?? []) : [])
  const slots = typeof au?.marketSlots === 'function' ? await au.marketSlots().catch(() => undefined) : undefined
  const boot = typeof au?.bootstrap === 'function' ? au.bootstrap() : undefined
  const pending = au ? await au.store.approvals('pending') : []
  const rawOps = au ? (await au.store.operations()).filter((o) => o.kind !== 'enable') : []
  const autoOps = rawOps.slice(-8).reverse()
  // the execution tail — Temporal's "pending first" view: non-acked outbox
  // rows (what it's waiting on, attempt count, next retry), the armed drain
  // time, and the newest journal events across the most recent ops
  const waiting = typeof au?.store.waitingOn === 'function' ? await au.store.waitingOn().catch(() => []) : []
  const nextAt = typeof au?.store.nextOutboxAt === 'function' ? await au.store.nextOutboxAt().catch(() => undefined) : undefined
  const journal = (
    await Promise.all(
      rawOps.slice(-3).map(async (o) =>
        (typeof au?.store.events === 'function' ? await au.store.events(o.operationId).catch(() => []) : []).map((e) => ({
          op: o.operationId.slice(0, 8),
          at: e.at,
          to: e.to,
          cause: e.cause
        }))
      )
    )
  )
    .flat()
    .sort((a, b) => b.at - a.at)
    .slice(0, 8)
  const coverage = d.records > 0 ? Math.round((d.anchoredRecords / d.records) * 100) : 100
  const earned = amounts.reduce((a, b) => a + b, 0)
  const worksFor = (shard: string) => decoded.filter((r) => r.shards.includes(shard)).length
  const earnedFor = (shard: string) => decoded.filter((r) => r.shards.includes(shard)).reduce((a, r) => a + (r.sats ?? 0), 0)
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
  const nextText: Record<string, string> = {
    funded: 'next: fund the treasury — anchors and payouts draw from this book',
    memory: 'next: remember something — the first shard starts sellable memory',
    anchored: 'next: wait for an anchor — a record proves once it lands on-chain',
    verified: 'next: answer verifiably — groundedAnswer issues the first checkable receipt',
    earning: 'next: list a shard for sale — describeShard puts memory on the market'
  }
  return {
    network: d.network,
    model: nektarEnv('NEKTAR_MODEL_ID') ?? 'operator',
    pub: mem.operatorKey().toPublicKey().toString().slice(0, 24),
    uptimeSec: Math.floor(process.uptime()),
    loop: {
      steps,
      done: steps.filter((s) => s.done).length,
      frontier: firstOpen === -1 ? 'loop live' : steps[firstOpen].name,
      next: firstOpen === -1 ? 'the earning loop is live — every link below is checkable by a stranger' : (nextText[steps[firstOpen].name] ?? '')
    },
    treasury: { sats: bal, utxos: Number(f.utxos ?? 0), funded: !!f.funded, payments: d.payments },
    receipts: { count: rrecs.length, earned, earned24h, amounts, list: receipts },
    memory: { shards: d.shards, records: d.records, anchored: d.anchoredRecords, unproven: d.unprovenAnchors, coverage, prover: d.prover },
    // the domain track record — records per shard is the agent's provable
    // depth in each domain it sells
    market: (
      await Promise.all(
        cat.map(async (m) => ({
          shard: m.shard,
          domains: m.domains,
          records: (await mem.records(m.shard).catch(() => [])).length,
          works: worksFor(m.shard),
          earned: earnedFor(m.shard),
          offers: m.offers?.map((o) => `${o.kind} ${o.priceSats}ⓢ`).join(' · ') ?? ''
        }))
      )
    ).sort((a, b) => b.earned - a.earned || b.records - a.records),
    net: {
      hosts: hosts.map((h) => ({ url: h, state: au === undefined ? 'configured' : downSet.has(h) ? 'down' : 'up', saved: saved.has(h) })),
      // the network's baked-in seeds not already joined — the zero-config
      // "join the public hosts" suggestion (regtest/main carry none)
      seeds: (DEFAULT_MARKET_HOSTS[d.network] ?? []).filter((h) => !hosts.includes(h) && !blocked.has(h)),
      slots: slots ?? null,
      boot: boot ?? null,
      push: nektarEnv('NEKTAR_PUSH_PUBLIC') ?? null
    },
    automation: au
      ? {
          pending: pending.map((a) => ({
            id: a.approvalId,
            reason: a.reason,
            maxExtraSats: a.maxExtraSats,
            operationId: a.operationId,
            expiresAt: a.expiresAt
          })),
          ops: autoOps.map((o) => ({ role: o.role, kind: o.kind, state: o.state, lastError: o.lastError })),
          rt: {
            executor: (au as { rt?: unknown }).rt === undefined ? 'local' : 'restate',
            // the Restate introspection endpoint — the Debug tab embeds it
            // once the executor port lands and reports one
            adminUrl: (au as { rt?: { adminUrl?: string } }).rt?.adminUrl ?? null,
            // paused heads wedge their object's FIFO invisibly — the number
            // an operator watches for 'the plane is frozen' (resume/kill via
            // `restate invocations resume --yes <id>` on the admin port)
            paused: (await (au as { rt?: { pausedCount?: () => Promise<number | undefined> } }).rt?.pausedCount?.()) ?? null,
            pending: waiting.slice(0, 6).map((w) => ({
              actionId: w.actionId,
              operationId: w.operationId,
              kind: w.kind,
              status: w.status,
              deferClass: w.deferClass ?? null,
              lastError: w.lastError ?? null,
              attempts: w.attempts,
              nextAt: w.nextAt
            })),
            nextAt: nextAt ?? null,
            journal
          }
        }
      : null,
    share: { enabled: !/^(0|false|no)$/i.test(nektarEnv('NEKTAR_PACK_SHARE') ?? '1'), url: `${base}/card` },
    // the operator's own knobs — the Settings tab: every approved policy the
    // agent trades under (seller-enabled marks the live seller row), and
    // where every market host came from (env / joined / left)
    settings: {
      policies: await (async () => {
        if (!au) return []
        const rows = await au.store.policies().catch(() => [] as Awaited<ReturnType<typeof au.store.policies>>)
        const en = await au.store.operationByKey('seller-enabled').catch(() => undefined)
        const live = typeof en?.data.policyHash === 'string' ? en.data.policyHash : undefined
        return Promise.all(
          rows.map(async (r) => ({
            ...r,
            active: r.policyHash === live,
            policy: r.revokedAt === undefined ? ((await au.store.activePolicy(r.policyHash).catch(() => undefined)) ?? null) : null
          }))
        )
      })(),
      hosts: { env: marketHostsFromEnv(), added: [...saved], blocked: [...blocked] }
    }
  }
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
<script>document.getElementById('sharebtn').onclick=async function(){var u=${jsStr(pageUrl.replace('&format=html', ''))};this.className='sharebtn';if(navigator.share){try{await navigator.share({title:'verified answer — nektar',url:u});this.textContent='\\u2713 shared';this.className='sharebtn done';return}catch(e){if(e&&e.name==='AbortError')return}}try{await navigator.clipboard.writeText(u);this.textContent='\\u2713 link copied';this.className='sharebtn done'}catch(e){this.textContent=u}}</script>
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
  const base = nektarEnv('NEKTAR_VERIFY_BASE') ?? `http://${safeHost(host)}`
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
  const operator = mem.operatorKey().toPublicKey().toString()
  const ogDesc = `${d?.records ?? 0} records · ${coverage}% anchored on-chain · ${receipts} receipts issued · ${pub.length} shards for sale`
  return `<!doctype html><html><head><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1"><title>nektar agent — verifiable memory</title>
<meta property="og:title" content="nektar agent card — verifiable memory anchored on BSV">
<meta property="og:description" content="${esc(ogDesc)}">
<meta property="og:type" content="website"><meta property="og:url" content="${esc(pageUrl)}">
<meta name="twitter:card" content="summary">
<style>body{font:15px/1.55 system-ui,sans-serif;background:${NEKTAR_DARK};color:#eae6e3;max-width:600px;margin:24px auto;padding:0 16px}
h1{font-size:19px;margin-bottom:2px;display:flex;align-items:center;gap:10px}.net{color:#a2958a;font-size:12px;margin-bottom:14px;word-break:break-all}
.mono{font-family:ui-monospace,monospace;font-size:11px}
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
<script>document.getElementById('sharebtn').onclick=async function(){var u=${jsStr(pageUrl)};this.className='sharebtn';if(navigator.share){try{await navigator.share({title:'nektar agent — verifiable memory',url:u});this.textContent='\\u2713 shared';this.className='sharebtn done';return}catch(e){if(e&&e.name==='AbortError')return}}try{await navigator.clipboard.writeText(u);this.textContent='\\u2713 link copied';this.className='sharebtn done'}catch(e){this.textContent=u}}</script>
<img src="${qr}" alt="scan to open this card"><div class=qrcap>scan to open this card</div>
</body></html>`
}
