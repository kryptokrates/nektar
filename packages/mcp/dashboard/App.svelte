<script lang="ts">
  import { onMount } from 'svelte'

  type Receipt = { seq: number; tool: string; sats: number | null; url: string }
  type Host = { url: string; state: string }
  type Pending = { id: string; reason: string; maxExtraSats: number; operationId: string; expiresAt: string }
  type Op = { role: string; kind: string; state: string; lastError?: string }
  type Shard = { shard: string; domains: string[]; records: number; offers: string }
  type State = {
    network: string
    model: string
    pub: string
    uptimeSec: number
    loop: { steps: { name: string; done: boolean }[]; done: number; frontier: string; next: string }
    treasury: { sats: number; funded: boolean; payments: string }
    receipts: { count: number; earned: number; amounts: number[]; list: Receipt[] }
    memory: { shards: number; records: number; anchored: number; unproven: number; coverage: number; prover: string }
    market: Shard[]
    net: { hosts: Host[]; slots: Record<string, number> | null; boot: string | null; push: string | null }
    automation: { pending: Pending[]; ops: Op[] } | null
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
      <div class="earn">
        <div class="pre">earned</div>
        <h1>{fmt(s.receipts.earned)} <em>sats</em></h1>
        <p class="sub">
          {s.receipts.count} checkable work{s.receipts.count === 1 ? '' : 's'} · {fmt(s.treasury.sats)} sats in treasury ·
          {s.market.length} domain{s.market.length === 1 ? '' : 's'} for sale
        </p>
      </div>
      {#if s.share.enabled}
        <button class="share" onclick={share}>{copied ? 'copied ✓' : "share your agent's record ↗"}</button>
      {/if}
    </section>

    <section class="grid">
      <div class="card wide">
        <h2>Track record <small>domain depth a buyer sees — every record anchored on-chain</small></h2>
        {#each s.market as m}
          <div class="row">
            <span><b>{m.shard}</b> {#each m.domains as d}<i class="dom">{d}</i>{/each}</span>
            <b>{m.records} records</b><b class="offer">{m.offers}</b>
          </div>
        {:else}
          <p class="empty">nothing on the market yet — describeShard puts a domain's memory up for sale</p>
        {/each}
        {#if s.memory.unproven > 0}
          <p class="warn">{s.memory.unproven} anchor{s.memory.unproven === 1 ? '' : 's'} still unproven — not yet sellable as verifiable depth</p>
        {/if}
      </div>

      <div class="card">
        <h2>Works <small>verifiable answers, checkable by anyone</small></h2>
        {#each s.receipts.list as r}
          <div class="row"><span>{r.tool}</span><b>{r.sats === null ? '—' : `${fmt(r.sats)} sats`}</b><a href={r.url}>verify ↗</a></div>
        {:else}
          <p class="empty">no works yet — groundedAnswer issues the first checkable receipt</p>
        {/each}
      </div>

      <div class="card">
        <h2>Treasury <small>{s.treasury.funded ? 'funded' : 'needs funding'}</small></h2>
        <div class="big">{fmt(s.treasury.sats)} <em>sats</em></div>
        <p class="foot">payments {s.treasury.payments} · anchors and payouts draw from this book</p>
      </div>

      <div class="card">
        <h2>Network <small>{s.net.hosts.length ? `${s.net.hosts.filter((h) => h.state === 'up').length} of ${s.net.hosts.length} hosts up` : 'single-player'}</small></h2>
        {#each s.net.hosts as h}
          <div class="row"><span class="mono">{host(h.url)}</span><b class={h.state === 'down' ? 'bad' : h.state === 'up' ? 'ok' : ''}>{h.state}</b></div>
        {:else}
          <p class="empty">no market hosts configured — set NEKTAR_MARKET_HOSTS to join a network</p>
        {/each}
        {#if s.net.slots}
          <div class="lanes">
            {#each Object.entries(s.net.slots) as [lane, n]}
              <div class="lane"><span>{lane}</span><b>{n}</b></div>
            {/each}
          </div>
        {/if}
        {#if s.net.boot}<p class="foot">bootstrap {s.net.boot}{s.net.push ? ` · push ${host(s.net.push)}` : ''}</p>{/if}
      </div>

      <div class="card wide">
        <h2>
          Agent <small>
            {#if s.automation}
              {s.automation.ops.filter((o) => o.state === 'running').length} running · {s.automation.pending.length} pending approval{s.automation.pending.length === 1 ? '' : 's'}
            {:else}
              not running — start automation to trade unattended
            {/if}
          </small>
        </h2>
        {#if s.automation}
          {#each s.automation.pending as a}
            <div class="row">
              <span><b>{a.reason}</b> <small class="mut">≤ {a.maxExtraSats} sats · {a.operationId.slice(0, 8)}… · expires {new Date(a.expiresAt).toLocaleTimeString()}</small></span>
              <span class="acts"><button onclick={() => decide(a.id, true)}>approve</button><button class="ghost" onclick={() => decide(a.id, false)}>deny</button></span>
            </div>
          {/each}
          {#each s.automation.ops as o}
            <div class="row"><span class="mut">{o.role} {o.kind}</span><b class={o.lastError ? 'bad' : o.state === 'running' ? 'ok' : ''}>{o.state}{o.lastError ? ` — ${o.lastError}` : ''}</b></div>
          {:else}
            {#if !s.automation.pending.length}<p class="empty">idle — routine trades do not prompt</p>{/if}
          {/each}
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
    background: #171210;
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
    --card: #241e1a;
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
    max-width: 1080px;
    margin: 0 auto;
    padding: 1.4rem;
    display: grid;
    gap: 1rem;
  }
  .card {
    background: var(--card);
    border: 1px solid var(--line);
    border-radius: 12px;
    padding: 1.1rem 1.3rem;
    box-shadow: 0 8px 24px rgb(0 0 0 / 35%);
  }
  .hero {
    display: flex;
    align-items: center;
    gap: 2rem;
    background: linear-gradient(115deg, var(--card) 0%, #3a2c19 60%, var(--card) 100%);
    padding: 2rem 1.6rem;
  }
  .pre {
    color: var(--amber);
    font-size: 0.75rem;
    text-transform: uppercase;
    letter-spacing: 0.1em;
  }
  .earn h1 {
    font-size: 3.4rem;
    margin: 0.1rem 0 0.4rem;
    color: var(--amber);
    font-weight: 800;
    line-height: 1;
  }
  .earn h1 em {
    font-style: normal;
    font-size: 1.4rem;
    color: var(--mut);
    font-weight: 600;
  }
  .sub {
    color: var(--mut);
    margin: 0;
    font-size: 0.9rem;
  }
  .share {
    margin-left: auto;
    background: var(--amber);
    color: var(--dark);
    border: 0;
    font-weight: 700;
    font-size: 1rem;
    padding: 0.8rem 1.6rem;
    border-radius: 10px;
    cursor: pointer;
    box-shadow: 0 0 22px rgb(247 153 27 / 30%);
    white-space: nowrap;
    flex-shrink: 0;
  }
  .share:hover {
    filter: brightness(1.08);
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
    .hero {
      flex-direction: column;
      align-items: flex-start;
    }
    .share {
      margin-left: 0;
    }
    .earn h1 {
      font-size: 2.6rem;
    }
  }
  h2 {
    font-size: 0.95rem;
    margin: 0 0 0.6rem;
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: 1rem;
  }
  h2 small {
    color: var(--mut);
    font-weight: 400;
    font-size: 0.75rem;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 0.8rem;
    padding: 0.55rem 0.3rem;
    border-bottom: 1px solid rgb(247 153 27 / 8%);
    font-size: 0.85rem;
  }
  .row:last-of-type {
    border-bottom: none;
  }
  .row > :first-child {
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
  .dom {
    font-style: normal;
    font-size: 0.68rem;
    color: var(--amber);
    background: rgb(247 153 27 / 12%);
    border: 1px solid rgb(247 153 27 / 25%);
    padding: 0.1rem 0.45rem;
    border-radius: 99px;
    margin-left: 0.4rem;
  }
  .offer {
    color: var(--ok);
    font-weight: 600;
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
  }
  .big em {
    font-style: normal;
    font-size: 1rem;
    color: var(--mut);
    font-weight: 600;
  }
  .lanes {
    display: grid;
    grid-template-columns: repeat(4, 1fr);
    gap: 0.5rem;
    margin-top: 0.7rem;
    padding-top: 0.7rem;
    border-top: 1px solid var(--line);
  }
  .lane {
    background: var(--card2);
    border-radius: 8px;
    padding: 0.45rem 0.6rem;
    display: flex;
    flex-direction: column;
  }
  .lane span {
    font-size: 0.65rem;
    color: var(--mut);
    text-transform: uppercase;
  }
  .lane b {
    font-size: 1.1rem;
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
</style>
