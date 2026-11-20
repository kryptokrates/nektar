<script lang="ts">
  import { onMount } from 'svelte'

  type Step = { name: string; done: boolean }
  type Receipt = { seq: number; tool: string; sats: number | null; url: string }
  type Host = { url: string; state: string }
  type Pending = { id: string; reason: string; maxExtraSats: number; operationId: string; expiresAt: string }
  type Op = { role: string; kind: string; state: string; lastError?: string }
  type State = {
    network: string
    model: string
    pub: string
    uptimeSec: number
    loop: { steps: Step[]; done: number; frontier: string; next: string }
    treasury: { sats: number; funded: boolean; payments: number }
    receipts: { count: number; earned: number; amounts: number[]; list: Receipt[] }
    memory: { shards: number; records: number; anchored: number; unproven: number; coverage: number; prover: string }
    market: Array<{ shard: string; domains: string[]; offers: string }>
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
  const donut = (pct: number) => `conic-gradient(var(--amber) ${pct * 3.6}deg, var(--line) 0deg)`

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
    const url = s.share.url
    try {
      if (navigator.share) await navigator.share({ title: 'my nektar agent', url })
      else await navigator.clipboard.writeText(url)
    } catch {
      await navigator.clipboard.writeText(url).catch(() => {})
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
      <div>
        <div class="pre">my agent is</div>
        <h1>{s.loop.frontier}</h1>
        <div class="steps">
          {#each s.loop.steps as st, i}
            <div class="step" class:done={st.done} class:next={!st.done && i === s.loop.steps.findIndex((x) => !x.done)}>
              <i>{st.done ? '✓' : ''}</i><span>{st.name}</span>
            </div>
          {/each}
        </div>
        <p class="next">{s.loop.next}</p>
      </div>
      <div class="heroact">
        {#if s.share.enabled}
          <button class="share" onclick={share}>{copied ? 'copied ✓' : 'share your agent ↗'}</button>
        {/if}
        <div class="ring" style:background={donut(s.memory.coverage)}><b>{s.memory.coverage}%</b><span>provable</span></div>
      </div>
    </section>

    <section class="tiles">
      <div class="tile"><span>treasury</span><b>{fmt(s.treasury.sats)} <em>sats</em></b><small>{s.treasury.funded ? 'funded' : 'needs funding'} · {s.treasury.payments} payments</small></div>
      <div class="tile"><span>earned</span><b>{fmt(s.receipts.earned)} <em>sats</em></b><small>{s.receipts.count} receipts</small></div>
      <div class="tile"><span>for sale</span><b>{s.market.length}</b><small>{s.market.length ? s.market.map((m) => m.shard).join(', ') : 'nothing listed'}</small></div>
      <div class="tile"><span>memory</span><b>{s.memory.records}</b><small>{s.memory.shards} shards · {s.memory.anchored} anchored{s.memory.unproven ? ` · ${s.memory.unproven} unproven` : ''}</small></div>
    </section>

    <section class="grid">
      <div class="card">
        <h2>Receipts <small>{s.receipts.count} checkable</small></h2>
        {#each s.receipts.list as r}
          <div class="row"><span>{r.tool}</span><b>{r.sats === null ? '—' : `${fmt(r.sats)} sats`}</b><a href={r.url}>verify ↗</a></div>
        {:else}
          <p class="empty">no receipts yet — groundedAnswer issues the first checkable one</p>
        {/each}
      </div>

      <div class="card">
        <h2>Market <small>{s.market.length} listed</small></h2>
        {#each s.market as m}
          <div class="row"><span><b>{m.shard}</b> <small class="mut">{m.domains.join(', ')}</small></span><b>{m.offers}</b></div>
        {:else}
          <p class="empty">nothing for sale — describeShard lists memory on the market</p>
        {/each}
      </div>

      <div class="card">
        <h2>Network <small>{s.net.hosts.length ? `${s.net.hosts.filter((h) => h.state === 'up').length} of ${s.net.hosts.length} hosts up` : 'single-player'}</small></h2>
        {#each s.net.hosts as h}
          <div class="row"><span class="mono">{host(h.url)}</span><b class={h.state === 'down' ? 'bad' : ''}>{h.state}</b></div>
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

      {#if s.automation}
        <div class="card">
          <h2>Automation <small>{s.automation.pending.length} pending approval{s.automation.pending.length === 1 ? '' : 's'}</small></h2>
          {#each s.automation.pending as a}
            <div class="row">
              <span><b>{a.reason}</b> <small class="mut">≤ {a.maxExtraSats} sats · {a.operationId.slice(0, 8)}… · expires {new Date(a.expiresAt).toLocaleTimeString()}</small></span>
              <span class="acts"><button onclick={() => decide(a.id, true)}>approve</button><button class="ghost" onclick={() => decide(a.id, false)}>deny</button></span>
            </div>
          {:else}
            <p class="empty">no pending approvals — routine trades do not prompt</p>
          {/each}
          {#each s.automation.ops as o}
            <div class="row"><span class="mut">{o.role} {o.kind}</span><b class={o.lastError ? 'bad' : ''}>{o.state}{o.lastError ? ` — ${o.lastError}` : ''}</b></div>
          {/each}
        </div>
      {/if}
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
    --field: #171210;
    --card: #241e1a;
    --card2: #2e2620;
    --line: #3a2f28;
    --mut: #a89a8e;
    --ok: #4cc26b;
    --bad: #e5534b;
    --azure: #58a6ff;
    --violet: #b083f0;
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
    padding: 1.6rem 1.5rem;
  }
  .pre {
    color: var(--amber);
    font-size: 0.72rem;
    text-transform: uppercase;
    letter-spacing: 0.08em;
  }
  h1 {
    font-size: 2.6rem;
    margin: 0.15rem 0 1rem;
    color: var(--amber);
    font-weight: 800;
  }
  .steps {
    display: flex;
    gap: 0;
    margin: 0 0 0.8rem;
  }
  .step {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 0.3rem;
    flex: 1;
    position: relative;
  }
  .step::before {
    content: '';
    position: absolute;
    top: 0.55rem;
    left: -50%;
    width: 100%;
    height: 2px;
    background: var(--line);
  }
  .step:first-child::before {
    display: none;
  }
  .step.done::before {
    background: var(--amber);
  }
  .step i {
    width: 1.15rem;
    height: 1.15rem;
    border-radius: 50%;
    border: 2px solid var(--line);
    background: var(--dark);
    z-index: 1;
    font-style: normal;
    font-size: 0.6rem;
    display: flex;
    align-items: center;
    justify-content: center;
    color: transparent;
  }
  .step.done i {
    background: var(--amber);
    border-color: var(--amber);
    color: var(--dark);
  }
  .step.next i {
    border-color: var(--amber);
    box-shadow: 0 0 0 0.3rem rgb(247 153 27 / 15%);
  }
  .step span {
    font-size: 0.6rem;
    color: var(--mut);
    text-transform: uppercase;
    letter-spacing: 0.03em;
    white-space: nowrap;
  }
  .step.done span {
    color: var(--field-2, #ece7e4);
  }
  .step.next span {
    color: var(--amber);
  }
  .next {
    color: var(--mut);
    margin: 0;
    max-width: 34rem;
  }
  .heroact {
    margin-left: auto;
    display: flex;
    align-items: center;
    gap: 1.4rem;
    flex-shrink: 0;
  }
  .share {
    background: var(--amber);
    color: var(--dark);
    border: 0;
    font-weight: 700;
    font-size: 1rem;
    padding: 0.75rem 1.6rem;
    border-radius: 10px;
    cursor: pointer;
    box-shadow: 0 0 22px rgb(247 153 27 / 30%);
  }
  .share:hover {
    filter: brightness(1.08);
  }
  .ring {
    width: 92px;
    height: 92px;
    border-radius: 50%;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    position: relative;
  }
  .ring::before {
    content: '';
    position: absolute;
    inset: 8px;
    border-radius: 50%;
    background: var(--card);
  }
  .ring b,
  .ring span {
    position: relative;
  }
  .ring b {
    font-size: 1.15rem;
  }
  .ring span {
    font-size: 0.6rem;
    color: var(--mut);
    text-transform: uppercase;
  }
  .tiles {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
    gap: 1rem;
  }
  .tile {
    background: var(--card);
    border: 1px solid var(--line);
    border-radius: 12px;
    padding: 0.9rem 1.1rem;
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
  }
  .tile span {
    font-size: 0.7rem;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--mut);
  }
  .tile b {
    font-size: 1.5rem;
  }
  .tile em {
    font-style: normal;
    font-size: 0.8rem;
    color: var(--mut);
  }
  .tile small {
    color: var(--mut);
    font-size: 0.75rem;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 1rem;
  }
  @media (max-width: 820px) {
    .grid,
    .tiles {
      grid-template-columns: 1fr;
    }
    .hero {
      flex-direction: column;
      align-items: flex-start;
    }
    .heroact {
      margin-left: 0;
    }
  }
  h2 {
    font-size: 0.95rem;
    margin: 0 0 0.6rem;
    display: flex;
    justify-content: space-between;
    align-items: baseline;
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
    padding: 0.5rem 0.3rem;
    border-bottom: 1px solid rgb(247 153 27 / 8%);
    font-size: 0.85rem;
  }
  .row:last-child {
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
  .mono {
    font-family: ui-monospace, monospace;
    font-size: 0.8rem;
  }
  .bad {
    color: var(--bad);
  }
  .empty {
    color: var(--mut);
    font-size: 0.8rem;
    padding: 0.8rem 0;
    margin: 0;
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
