<script lang="ts">
  import { onMount } from 'svelte'

  type Receipt = { seq: number; tool: string; sats: number | null; shards: string[]; url: string }
  type Host = { url: string; state: string }
  type Pending = { id: string; reason: string; maxExtraSats: number; operationId: string; expiresAt: string }
  type Op = { role: string; kind: string; state: string; lastError?: string }
  type WaitRow = {
    actionId: string
    operationId: string
    kind: string
    status: string
    deferClass: string | null
    lastError: string | null
    attempts: number
    nextAt: number
  }
  type Rt = { executor: string; pending: WaitRow[]; nextAt: number | null; journal: Array<{ op: string; at: number; to: string; cause: string }> }
  type Shard = { shard: string; domains: string[]; records: number; works: number; offers: string }
  type State = {
    network: string
    model: string
    pub: string
    uptimeSec: number
    loop: { steps: { name: string; done: boolean }[]; done: number; frontier: string; next: string }
    treasury: { sats: number; utxos: number; funded: boolean; payments: string }
    receipts: { count: number; earned: number; amounts: number[]; list: Receipt[] }
    memory: { shards: number; records: number; anchored: number; unproven: number; coverage: number; prover: string }
    market: Shard[]
    net: { hosts: Host[]; slots: Record<string, number> | null; boot: string | null; push: string | null }
    automation: { pending: Pending[]; ops: Op[]; rt: Rt } | null
    share: { enabled: boolean; url: string }
  }

  let s = $state<State | null>(null)
  let err = $state('')
  let live = $state(false)
  let copied = $state(false)

  const fmt = (n: number) => n.toLocaleString('en').replace(/,/g, "'")
  const host = (u: string) => u.replace(/^https?:\/\//, '').replace(/\/.*$/, '')
  const ago = (sec: number) => {
    const h = Math.floor(sec / 3600)
    return h > 0 ? `${h}h ${Math.floor((sec % 3600) / 60)}m` : `${Math.floor(sec / 60)}m`
  }

  // cumulative earnings curve — one point per work, oldest first
  const cum = (amounts: number[]) => amounts.slice().reverse().reduce<number[]>((out, v, i) => [...out, (out[i - 1] ?? 0) + v], [])
  const AW = 560,
    AH = 110
  function area(vals: number[]) {
    const max = Math.max(...vals, 1)
    const pts = vals.map(
      (v, i) => [vals.length === 1 ? AW / 2 : (i / (vals.length - 1)) * AW, AH - 6 - (v / max) * (AH - 16)] as const
    )
    const line = pts.map((p, i) => `${i ? 'L' : 'M'}${p[0].toFixed(1)},${p[1].toFixed(1)}`).join(' ')
    return { line, fill: pts.length ? `${line} L${AW},${AH} L0,${AH} Z` : '', last: pts.at(-1) }
  }

  async function load() {
    try {
      const r = await fetch('/state')
      if (!r.ok) throw new Error(`state ${r.status}`)
      s = await r.json()
      err = ''
    } catch (e) {
      err = String(e)
    }
  }

  async function share() {
    if (!s?.share.url) return
    try {
      if (navigator.share) await navigator.share({ title: "my agent's track record", url: s.share.url })
      else await navigator.clipboard.writeText(s.share.url)
    } catch {
      await navigator.clipboard.writeText(s.share.url).catch(() => {})
    }
    copied = true
    setTimeout(() => (copied = false), 1600)
  }

  async function decide(id: string, approve: boolean) {
    const tok = sessionStorage.getItem('nektar-operator-token') ?? prompt('operator token (NEKTAR_OPERATOR_TOKEN — not the agent bearer)')
    if (!tok) return
    sessionStorage.setItem('nektar-operator-token', tok)
    const r = await fetch(`/v1/automation/approvals/${id}`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', authorization: `Bearer ${tok}` },
      body: JSON.stringify({ approve })
    })
    if (!r.ok) err = `decision rejected (${r.status})`
    await load()
  }

  onMount(() => {
    load()
    const es = new EventSource('/events')
    es.onopen = () => (live = true)
    es.onmessage = () => load()
    es.onerror = () => (live = false)
    return () => es.close()
  })
</script>

{#if err}
  <div class="err">{err}</div>
{/if}
{#if !s}
  <div class="boot">loading…</div>
{:else}
  {@const curve = area(cum(s.receipts.amounts))}
  {@const maxRec = Math.max(...s.market.map((m) => m.records), 1)}
  {@const maxSats = Math.max(...s.receipts.amounts, 1)}
  {@const maxLane = Math.max(...Object.values(s.net.slots ?? {}), 1)}
  <nav>
    <svg viewBox="0 0 400 400" width="28" height="28" class="drop"
      ><rect width="400" height="400" fill="#221e1d" /><path
        fill="#f7991b"
        d="M 195 55 C 178 55 164 63 157 80 C 153 100 149 112 143 124 C 136 139 115 158 93 186 C 80 206 73 226 72 250 C 71 277 79 306 98 329 C 117 352 150 372 190 377 C 232 380 255 356 259 322 C 261 296 274 273 296 251 C 310 234 318 210 318 186 C 318 158 312 137 302 120 C 292 102 276 84 260 74 C 245 64 218 55 195 55 Z"
      /><circle cx="307" cy="78" r="38" fill="#f7991b" /><ellipse cx="163" cy="190" rx="20" ry="21" fill="#221e1d" /><ellipse
        cx="231"
        cy="190"
        rx="20"
        ry="21"
        fill="#221e1d"
      /><path
        d="M 150 265.5 C 161 279 176 287 196 288 C 216 287 230 278 243 265"
        fill="none"
        stroke="#221e1d"
        stroke-width="24"
        stroke-linecap="round"
      /></svg
    >
    <b>nektar</b>
    <span class="live" class:down={!live}><i></i>{live ? 'live' : 'offline'}</span>
    <span class="sp"></span>
    <span class="mut">{s.model} · {s.network} · {ago(s.uptimeSec)}</span>
    <code class="pub">{s.pub}…</code>
  </nav>

  <main>
    <section class="hero card">
      <div class="herorow">
        <div class="earn">
          <div class="pre">earned</div>
          <h1>{fmt(s.receipts.earned)} <em>sats</em></h1>
          <p class="sub">
            {s.receipts.count} checkable work{s.receipts.count === 1 ? '' : 's'} · {fmt(s.treasury.sats)} in treasury ·
            {s.market.length} domain{s.market.length === 1 ? '' : 's'} for sale
          </p>
        </div>
        <div class="heroright">
          <svg viewBox="0 0 100 100" class="dial">
            <circle cx="50" cy="50" r="42" class="dialbg" />
            <circle
              cx="50"
              cy="50"
              r="42"
              class="dialfg"
              class:full={s.memory.coverage === 100}
              style="stroke-dashoffset:{263.9 * (1 - s.memory.coverage / 100)}"
            />
            <text x="50" y="47" class="dialnum">{s.memory.coverage}%</text>
            <text x="50" y="64" class="diallbl">provable</text>
          </svg>
          <span class="dialline">{s.memory.anchored} of {s.memory.records} anchored</span>
          {#if s.share.enabled}
            <button class="share" onclick={share}>{copied ? 'copied ✓' : "share your agent's record ↗"}</button>
          {/if}
        </div>
      </div>
      {#if s.receipts.amounts.length}
        <svg viewBox={`0 0 ${AW} ${AH}`} class="area" preserveAspectRatio="none">
          <defs>
            <linearGradient id="eg" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stop-color="rgba(247,153,27,.4)" />
              <stop offset="100%" stop-color="rgba(247,153,27,0)" />
            </linearGradient>
          </defs>
          {#each [0.25, 0.5, 0.75] as g}<line x1="0" y1={AH * g} x2={AW} y2={AH * g} class="agrid" />{/each}
          <path d={curve.fill} fill="url(#eg)" />
          <path d={curve.line} class="aline" />
          {#if curve.last}<circle cx={curve.last[0]} cy={curve.last[1]} r="4" class="adot" />{/if}
        </svg>
      {:else}
        <p class="empty">the earnings curve draws itself — first groundedAnswer issues the first checkable work</p>
      {/if}
    </section>

    <section class="grid">
      <div class="card wide">
        <h2>Track record <small>provable depth per domain — the résumé a buyer sees</small></h2>
        {#each s.market as m}
          <div class="trow">
            <div class="thead"><b>{m.shard}</b>{#each m.domains as d}<i class="dom">{d}</i>{/each}</div>
            <div class="dbar"><i style="width:{(m.records / maxRec) * 100}%"></i></div>
            <div class="tnum">{m.records}<span> records · </span>{m.works}<span> works</span></div>
            <div class="offer">{m.offers}</div>
          </div>
        {:else}
          <p class="empty">nothing on the market yet — describeShard puts a domain's memory up for sale</p>
        {/each}
        {#if s.memory.unproven > 0}
          <p class="warn">{s.memory.unproven} anchor{s.memory.unproven === 1 ? '' : 's'} still unproven — not yet sellable as verifiable depth</p>
        {/if}
      </div>

      <div class="card">
        <h2>Works <small>sats per verifiable answer</small></h2>
        {#if s.receipts.amounts.length}
          <svg viewBox="0 0 320 72" class="wbars" preserveAspectRatio="none">
            <defs>
              <linearGradient id="wg" x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stop-color="#ffae3d" />
                <stop offset="100%" stop-color="#8a5a10" />
              </linearGradient>
            </defs>
            {#each s.receipts.amounts.slice().reverse() as a, i}
              {@const bw = 320 / s.receipts.amounts.length}
              <rect x={i * bw + 2.5} width={Math.max(bw - 5, 3)} height={8 + (a / maxSats) * 64} rx="4" transform="translate(0,{72 - (8 + (a / maxSats) * 64)})" class="wbar" />
            {/each}
          </svg>
        {/if}
        {#each s.receipts.list.slice(0, 5) as r}
          <div class="row">
            <span>{r.tool}{#each r.shards as sh}<i class="dom">{sh}</i>{/each}</span><b>{r.sats === null ? '—' : `${fmt(r.sats)} sats`}</b><a href={r.url}>verify ↗</a>
          </div>
        {:else}
          <p class="empty">no works yet</p>
        {/each}
      </div>

      <div class="card">
        <h2>Treasury <small>{s.treasury.funded ? 'funded' : 'needs funding'}</small></h2>
        <div class="big">{fmt(s.treasury.sats)} <em>sats</em></div>
        {#if s.treasury.utxos > 0}
          <div class="ustrip" title="{s.treasury.utxos} reserve outputs">
            {#each Array(Math.min(s.treasury.utxos, 24)) as _}<i></i>{/each}
            {#if s.treasury.utxos > 24}<span>+{s.treasury.utxos - 24}</span>{/if}
          </div>
        {/if}
        <p class="foot">{s.treasury.utxos} reserve output{s.treasury.utxos === 1 ? '' : 's'} · payments {s.treasury.payments}</p>
      </div>

      <div class="card">
        <h2>Network <small>{s.net.hosts.length ? `${s.net.hosts.filter((h) => h.state === 'up').length} of ${s.net.hosts.length} hosts up` : 'single-player'}</small></h2>
        {#each s.net.hosts as h}
          <div class="row"><span class="hdot" class:up={h.state === 'up'} class:downd={h.state === 'down'}></span><span class="mono">{host(h.url)}</span><b class={h.state === 'down' ? 'bad' : h.state === 'up' ? 'ok' : ''}>{h.state}</b></div>
        {:else}
          <p class="empty">no market hosts configured — set NEKTAR_MARKET_HOSTS to join a network</p>
        {/each}
        {#if s.net.slots}
          <div class="lgauges">
            {#each Object.entries(s.net.slots) as [lane, n]}
              <div class="lg"><div class="lhead"><span>{lane}</span><b>{n}</b></div><div class="lbar"><i style="width:{(n / maxLane) * 100}%"></i></div></div>
            {/each}
          </div>
        {/if}
        {#if s.net.boot}<p class="foot">bootstrap {s.net.boot}{s.net.push ? ` · push ${host(s.net.push)}` : ''}</p>{/if}
      </div>

      <div class="card wide">
        <h2>
          Agent <small>
            {#if s.automation}
              {s.automation.ops.filter((o) => o.state === 'running').length} running · {s.automation.pending.length} approval{s.automation.pending.length === 1 ? '' : 's'} · {s.automation.rt.pending.length} waiting
            {:else}
              not running — start automation to trade unattended
            {/if}
          </small>
        </h2>
        {#if s.automation}
          {#if s.automation.ops.length}
            <div class="ostrip">
              {#each s.automation.ops as o}<i class={o.lastError ? 's-err' : o.state === 'running' ? 's-run' : 's-idle'} title="{o.role} {o.kind} — {o.state}{o.lastError ? ` (${o.lastError})` : ''}"></i>{/each}
            </div>
          {/if}
          {#each s.automation.rt.pending as p}
            <div class="row">
              <span><i class="pulse"></i>{p.kind} <small class="mut">→ {p.operationId.slice(0, 8)}…{p.deferClass ? ` · ${p.deferClass}` : ''}{p.lastError ? ` · ${p.lastError}` : ''}</small></span>
              <b>attempt {p.attempts} · {new Date(p.nextAt).toLocaleTimeString()}</b>
            </div>
          {/each}
          {#each s.automation.pending as a}
            <div class="row">
              <span><b>{a.reason}</b> <small class="mut">≤ {a.maxExtraSats} sats · {a.operationId.slice(0, 8)}… · expires {new Date(a.expiresAt).toLocaleTimeString()}</small></span>
              <span class="acts"><button onclick={() => decide(a.id, true)}>approve</button><button class="ghost" onclick={() => decide(a.id, false)}>deny</button></span>
            </div>
          {/each}
          {#each s.automation.ops as o}
            <div class="row"><span class="mut">{o.role} {o.kind}</span><b class={o.lastError ? 'bad' : o.state === 'running' ? 'ok' : ''}>{o.state}{o.lastError ? ` — ${o.lastError}` : ''}</b></div>
          {:else}
            {#if !s.automation.pending.length && !s.automation.rt.pending.length}<p class="empty">idle — routine trades do not prompt</p>{/if}
          {/each}
          <div class="rline">
            <span class="chip">executor {s.automation.rt.executor}</span>
            <span class="chip">{s.automation.rt.pending.length} in outbox</span>
            <span class="chip">drain {s.automation.rt.nextAt ? new Date(s.automation.rt.nextAt).toLocaleTimeString() : 'idle'}</span>
          </div>
          {#if s.automation.rt.journal.length}
            <div class="journal">
              {#each s.automation.rt.journal as e}
                <div><span class="mono">{e.op}</span> → {e.to} <span class="mut">· {e.cause}</span></div>
              {/each}
            </div>
          {/if}
        {/if}
      </div>
    </section>
  </main>
{/if}

<style>
  :global(*) {
    box-sizing: border-box;
  }
  :global(body) {
    margin: 0;
    background: #120e0c;
    color: #ece7e4;
    font-family:
      system-ui,
      -apple-system,
      'Segoe UI',
      sans-serif;
    font-size: 14px;
  }
  :global(.err) {
    background: #e5534b;
    color: #fff;
    padding: 0.4rem 1rem;
    font-size: 0.8rem;
  }
  :root {
    --amber: #f7991b;
    --dark: #221e1d;
    --card: #211b17;
    --card2: #2e2620;
    --line: #3a2f28;
    --mut: #a89a8e;
    --ok: #4cc26b;
    --bad: #e5534b;
  }
  .boot {
    display: flex;
    min-height: 100vh;
    align-items: center;
    justify-content: center;
    color: var(--mut);
  }
  nav {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    padding: 0.7rem 1.4rem;
    background: var(--card);
    border-bottom: 1px solid var(--line);
    position: sticky;
    top: 0;
    z-index: 5;
  }
  .drop {
    border-radius: 6px;
  }
  .live {
    display: inline-flex;
    align-items: center;
    gap: 0.35rem;
    font-size: 0.72rem;
    color: var(--ok);
    background: rgb(76 194 107 / 10%);
    padding: 0.15rem 0.6rem;
    border-radius: 99px;
  }
  .live.down {
    color: var(--bad);
    background: rgb(229 83 75 / 10%);
  }
  .live i {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: currentColor;
    animation: pulse 2.4s infinite;
  }
  @keyframes pulse {
    50% {
      opacity: 0.4;
    }
  }
  .sp {
    flex: 1;
  }
  .mut {
    color: var(--mut);
  }
  .pub {
    background: var(--card2);
    padding: 2px 8px;
    border-radius: 5px;
    font-size: 0.75rem;
    color: var(--mut);
  }
  main {
    max-width: 1200px;
    margin: 0 auto;
    padding: 1.4rem;
    display: grid;
    gap: 1.1rem;
    position: relative;
  }
  main::before {
    content: '';
    position: absolute;
    inset: -200px -100px auto;
    height: 480px;
    background: radial-gradient(ellipse 60% 55% at 50% 0%, rgb(247 153 27 / 11%), transparent 70%);
    pointer-events: none;
    z-index: -1;
  }
  .card {
    background: var(--card);
    border: 1px solid var(--line);
    border-radius: 14px;
    padding: 1.2rem 1.4rem;
    box-shadow:
      inset 0 1px 0 rgb(255 255 255 / 4%),
      0 10px 28px rgb(0 0 0 / 45%);
  }
  .hero {
    background: linear-gradient(115deg, #2a221c 0%, #3a2a12 55%, #241e1a 100%);
    padding: 1.8rem 1.6rem 0.9rem;
  }
  .herorow {
    display: flex;
    align-items: center;
    gap: 1.6rem;
  }
  .pre {
    color: var(--amber);
    font-size: 0.78rem;
    text-transform: uppercase;
    letter-spacing: 0.14em;
  }
  .earn h1 {
    font-size: 4.2rem;
    margin: 0.15rem 0 0.5rem;
    color: var(--amber);
    font-weight: 800;
    line-height: 1;
    font-variant-numeric: tabular-nums;
    text-shadow: 0 0 30px rgb(247 153 27 / 35%);
  }
  .earn h1 em {
    font-style: normal;
    font-size: 1.5rem;
    color: var(--mut);
    font-weight: 600;
  }
  .sub {
    color: var(--mut);
    margin: 0;
    font-size: 0.92rem;
  }
  .heroright {
    margin-left: auto;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 0.9rem;
    flex-shrink: 0;
  }
  .share {
    background: var(--amber);
    color: var(--dark);
    border: 0;
    font-weight: 700;
    font-size: 0.95rem;
    padding: 0.7rem 1.4rem;
    border-radius: 10px;
    cursor: pointer;
    box-shadow: 0 0 22px rgb(247 153 27 / 35%);
    white-space: nowrap;
    transition: filter 0.15s;
  }
  .share:hover {
    filter: brightness(1.1);
  }
  .dial {
    width: 110px;
    height: 110px;
    transform: rotate(-90deg);
    filter: drop-shadow(0 0 10px rgb(247 153 27 / 30%));
  }
  .dialbg {
    fill: none;
    stroke: var(--line);
    stroke-width: 9;
  }
  .dialfg {
    fill: none;
    stroke: var(--amber);
    stroke-width: 9;
    stroke-linecap: round;
    stroke-dasharray: 263.9;
    transition:
      stroke-dashoffset 0.7s ease,
      stroke 0.4s;
  }
  .dialline {
    font-size: 0.7rem;
    color: var(--mut);
    margin-top: -0.5rem;
    font-variant-numeric: tabular-nums;
  }
  .dialfg.full {
    stroke: var(--ok);
    filter: drop-shadow(0 0 6px rgb(76 194 107 / 60%));
  }
  .dial text {
    transform: rotate(90deg);
    transform-origin: 50px 50px;
    text-anchor: middle;
    fill: #ece7e4;
  }
  .dialnum {
    font-size: 24px;
    font-weight: 800;
  }
  .diallbl {
    font-size: 10px;
    fill: var(--mut);
    text-transform: uppercase;
    letter-spacing: 0.08em;
  }
  .area {
    display: block;
    width: 100%;
    height: 130px;
    margin-top: 1rem;
  }
  .agrid {
    stroke: rgb(247 153 27 / 9%);
    stroke-width: 1;
  }
  .aline {
    fill: none;
    stroke: var(--amber);
    stroke-width: 2.5;
    stroke-linejoin: round;
    filter: drop-shadow(0 0 7px rgb(247 153 27 / 45%));
  }
  .adot {
    fill: var(--amber);
    filter: drop-shadow(0 0 6px rgb(247 153 27 / 80%));
  }
  .grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 1rem;
  }
  .wide {
    grid-column: 1 / -1;
  }
  @media (max-width: 820px) {
    .grid {
      grid-template-columns: 1fr;
    }
    .herorow {
      flex-wrap: wrap;
    }
    .heroright {
      margin-left: 0;
      flex-direction: row;
    }
    .earn h1 {
      font-size: 3rem;
    }
  }
  h2 {
    font-size: 0.95rem;
    margin: 0 0 0.7rem;
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: 1rem;
    text-transform: uppercase;
    letter-spacing: 0.06em;
  }
  h2 small {
    color: var(--mut);
    font-weight: 400;
    font-size: 0.75rem;
    text-transform: none;
    letter-spacing: 0;
  }
  .trow {
    display: grid;
    grid-template-columns: minmax(140px, 1.2fr) 2fr auto auto;
    align-items: center;
    gap: 0.9rem;
    padding: 0.55rem 0;
    border-bottom: 1px solid rgb(247 153 27 / 8%);
    font-size: 0.85rem;
  }
  .trow:last-of-type {
    border-bottom: none;
  }
  .thead {
    min-width: 0;
    overflow: hidden;
    white-space: nowrap;
  }
  .dom {
    font-style: normal;
    font-size: 0.66rem;
    color: var(--amber);
    background: rgb(247 153 27 / 12%);
    border: 1px solid rgb(247 153 27 / 25%);
    padding: 0.08rem 0.45rem;
    border-radius: 99px;
    margin-left: 0.4rem;
  }
  .dbar {
    height: 14px;
    background: var(--card2);
    border-radius: 7px;
    overflow: hidden;
  }
  .dbar i {
    display: block;
    height: 100%;
    border-radius: 7px;
    background: linear-gradient(90deg, #8a5a10, var(--amber) 70%, #ffbe55);
    box-shadow: 0 0 10px rgb(247 153 27 / 40%);
    transition: width 0.6s ease;
    min-width: 10px;
  }
  .tnum {
    font-weight: 800;
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  .tnum span {
    font-weight: 400;
    color: var(--mut);
    font-size: 0.75rem;
  }
  .offer {
    color: var(--ok);
    font-weight: 600;
    font-size: 0.8rem;
    white-space: nowrap;
  }
  .wbars {
    display: block;
    width: 100%;
    height: 72px;
    margin-bottom: 0.6rem;
  }
  .wbar {
    fill: url(#wg);
    transition: transform 0.5s ease;
    filter: drop-shadow(0 -1px 4px rgb(247 153 27 / 25%));
  }
  .wbar:hover {
    filter: drop-shadow(0 0 8px rgb(247 153 27 / 60%));
  }
  .row {
    display: flex;
    align-items: center;
    gap: 0.8rem;
    padding: 0.5rem 0.3rem;
    border-bottom: 1px solid rgb(247 153 27 / 8%);
    font-size: 0.85rem;
  }
  .row:last-of-type {
    border-bottom: none;
  }
  .row > :nth-child(2) {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .row a {
    color: var(--amber);
    text-decoration: none;
  }
  .mono {
    font-family: ui-monospace, monospace;
    font-size: 0.8rem;
  }
  .bad {
    color: var(--bad);
  }
  .ok {
    color: var(--ok);
  }
  .empty {
    color: var(--mut);
    font-size: 0.8rem;
    padding: 0.8rem 0;
    margin: 0;
  }
  .warn {
    color: var(--bad);
    font-size: 0.75rem;
    margin: 0.6rem 0 0;
    padding-top: 0.6rem;
    border-top: 1px solid var(--line);
  }
  .big {
    font-size: 2.2rem;
    font-weight: 800;
    color: var(--amber);
    font-variant-numeric: tabular-nums;
  }
  .big em {
    font-style: normal;
    font-size: 1rem;
    color: var(--mut);
    font-weight: 600;
  }
  .ustrip {
    display: flex;
    gap: 4px;
    flex-wrap: wrap;
    margin-top: 0.7rem;
    align-items: center;
  }
  .ustrip i {
    width: 22px;
    height: 8px;
    border-radius: 4px;
    background: linear-gradient(180deg, var(--amber), #b06f12);
    box-shadow: 0 0 6px rgb(247 153 27 / 30%);
  }
  .ustrip span {
    font-size: 0.7rem;
    color: var(--mut);
  }
  .hdot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--mut);
    flex-shrink: 0;
  }
  .hdot.up {
    background: var(--ok);
    box-shadow: 0 0 6px rgb(76 194 107 / 60%);
  }
  .hdot.downd {
    background: var(--bad);
    box-shadow: 0 0 6px rgb(229 83 75 / 60%);
  }
  .lgauges {
    display: grid;
    grid-template-columns: repeat(2, 1fr);
    gap: 0.6rem;
    margin-top: 0.7rem;
    padding-top: 0.7rem;
    border-top: 1px solid var(--line);
  }
  .lhead {
    display: flex;
    justify-content: space-between;
    font-size: 0.7rem;
    color: var(--mut);
    text-transform: uppercase;
    margin-bottom: 0.25rem;
  }
  .lhead b {
    color: #ece7e4;
    font-variant-numeric: tabular-nums;
  }
  .lbar {
    height: 10px;
    background: var(--card2);
    border-radius: 6px;
    overflow: hidden;
  }
  .lbar i {
    display: block;
    height: 100%;
    border-radius: 6px;
    background: linear-gradient(90deg, #2a7a42, var(--ok));
    box-shadow: 0 0 8px rgb(76 194 107 / 35%);
    transition: width 0.6s ease;
    min-width: 8px;
  }
  .ostrip {
    display: flex;
    gap: 5px;
    margin-bottom: 0.7rem;
  }
  .ostrip i {
    flex: 1;
    height: 8px;
    border-radius: 4px;
    background: var(--line);
  }
  .ostrip i.s-run {
    background: var(--ok);
    box-shadow: 0 0 6px rgb(76 194 107 / 40%);
  }
  .ostrip i.s-err {
    background: var(--bad);
    box-shadow: 0 0 6px rgb(229 83 75 / 40%);
  }
  .foot {
    color: var(--mut);
    font-size: 0.75rem;
    margin: 0.7rem 0 0;
  }
  .acts {
    display: flex;
    gap: 0.4rem;
    flex-shrink: 0;
  }
  .acts button {
    background: var(--amber);
    color: var(--dark);
    border: 0;
    border-radius: 6px;
    font-weight: 600;
    padding: 0.3rem 0.7rem;
    cursor: pointer;
    font-size: 0.75rem;
  }
  .acts .ghost {
    background: transparent;
    color: var(--mut);
    border: 1px solid var(--line);
  }
  i.pulse {
    display: inline-block;
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--amber);
    margin-right: 0.55rem;
    vertical-align: 1px;
    animation: pulse 1.6s ease-out infinite;
  }
  .rline {
    display: flex;
    gap: 0.5rem;
    flex-wrap: wrap;
    margin-top: 0.8rem;
    padding-top: 0.8rem;
    border-top: 1px solid var(--line);
  }
  .chip {
    font-size: 0.68rem;
    background: var(--card2);
    border: 1px solid var(--line);
    border-radius: 99px;
    padding: 0.18rem 0.6rem;
    color: var(--mut);
  }
  .journal {
    margin-top: 0.5rem;
    font-size: 0.75rem;
    display: grid;
    gap: 0.15rem;
    color: var(--mut);
  }
  .journal .mono {
    font-family: ui-monospace, 'Cascadia Mono', Consolas, monospace;
    color: var(--amber);
  }
</style>
