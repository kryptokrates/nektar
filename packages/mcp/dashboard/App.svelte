<script lang="ts">
  import { onMount } from 'svelte'

  type Receipt = { seq: number; tool: string; sats: number | null; shards: string[]; at: number | null; url: string }
  type Host = { url: string; state: string; saved?: boolean }
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
  type Rt = {
    executor: string
    adminUrl: string | null
    pending: WaitRow[]
    nextAt: number | null
    journal: Array<{ op: string; at: number; to: string; cause: string }>
  }
  type Shard = { shard: string; domains: string[]; records: number; works: number; earned: number; offers: string }
  type State = {
    network: string
    model: string
    pub: string
    uptimeSec: number
    loop: { steps: { name: string; done: boolean }[]; done: number; frontier: string; next: string }
    treasury: { sats: number; utxos: number; funded: boolean; payments: string }
    receipts: { count: number; earned: number; earned24h: number; amounts: number[]; list: Receipt[] }
    memory: { shards: number; records: number; anchored: number; unproven: number; coverage: number; prover: string }
    market: Shard[]
    net: {
      hosts: Host[]
      seeds: string[]
      slots: Record<string, number> | null
      boot: string | null
      push: string | null
    }
    automation: { pending: Pending[]; ops: Op[]; rt: Rt } | null
    share: { enabled: boolean; url: string }
    settings: {
      policy: {
        policyId?: string
        roles: string[]
        domains: string[]
        limits: Record<string, number>
        seller?: Record<string, unknown>
      } | null
      hosts: { env: string[]; added: string[]; blocked: string[] }
    }
  }

  let s = $state<State | null>(null)
  let err = $state('')
  let live = $state(false)
  let copied = $state(false)
  let tab = $state<'overview' | 'earnings' | 'activity' | 'network' | 'agent' | 'settings' | 'debug'>('overview')
  let sub = $state<'policy' | 'identity' | 'hosts' | 'runtime'>('policy')
  let joinInput = $state('')
  let joinMsg = $state('')
  let joinFound = $state<string[]>([])

  const fmt = (n: number) => n.toLocaleString('en').replace(/,/g, "'")
  const host = (u: string) => u.replace(/^https?:\/\//, '').replace(/\/.*$/, '')
  const ago = (sec: number) => {
    const h = Math.floor(sec / 3600)
    return h > 0 ? `${h}h ${Math.floor((sec % 3600) / 60)}m` : `${Math.floor(sec / 60)}m`
  }
  const agoSince = (at: number | null) => (at === null ? '' : ago(Math.max(0, Math.floor((Date.now() - at) / 1000))))

  // cumulative earnings curve — one point per work, oldest first
  const cum = (amounts: number[]) => amounts.slice().reverse().reduce<number[]>((out, v, i) => [...out, (out[i - 1] ?? 0) + v], [])
  const AW = 720,
    AH = 140
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
      document.title = s.receipts.earned > 0 ? `${fmt(s.receipts.earned)} sats · nektar` : 'nektar'
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

  // join flow — paste-anything → the server probes /.well-known/nektar,
  // persists + hot-adds, returns the peer's advertised hosts (the crawl)
  const hostPost = (u: string, tok?: string) =>
    fetch('/v1/hosts', {
      method: 'POST',
      headers: { 'content-type': 'application/json', ...(tok ? { authorization: `Bearer ${tok}` } : {}) },
      body: JSON.stringify({ url: u })
    })
  async function joinHost(u: string) {
    if (!u.trim()) return
    joinMsg = ''
    let r = await hostPost(u.trim(), sessionStorage.getItem('nektar-operator-token') ?? undefined)
    if (r.status === 403) {
      const tok = prompt('operator token (NEKTAR_OPERATOR_TOKEN)')
      if (!tok) return
      sessionStorage.setItem('nektar-operator-token', tok)
      r = await hostPost(u.trim(), tok)
    }
    const j = (await r.json().catch(() => ({}))) as { error?: string; url?: string; discovered?: string[] }
    if (!r.ok) {
      joinMsg = j.error ?? `join failed (${r.status})`
      return
    }
    joinMsg = `✓ ${j.url} joined`
    joinFound = j.discovered ?? []
    joinInput = ''
    await load()
  }
  async function joinAll() {
    for (const h of joinFound) await joinHost(h)
    joinFound = []
  }

  // leave a host — saved joins drop; env/seed hosts block so the leave persists
  async function disconnect(u: string) {
    const del = (tok?: string) =>
      fetch(`/v1/hosts?url=${encodeURIComponent(u)}`, { method: 'DELETE', headers: tok ? { authorization: `Bearer ${tok}` } : {} })
    let r = await del(sessionStorage.getItem('nektar-operator-token') ?? undefined)
    if (r.status === 403) {
      const tok = prompt('operator token (NEKTAR_OPERATOR_TOKEN)')
      if (!tok) return
      sessionStorage.setItem('nektar-operator-token', tok)
      r = await del(tok)
    }
    const j = (await r.json().catch(() => ({}))) as { error?: string }
    joinMsg = r.ok ? `left ${host(u)}` : (j.error ?? `disconnect failed (${r.status})`)
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
  {@const amts = s.receipts.amounts}
  {@const half = Math.floor(amts.length / 2)}
  {@const recent = half > 0 ? amts.slice(-half).reduce((x, y) => x + y, 0) / half : 0}
  {@const prior = amts.length - half > 0 ? amts.slice(0, amts.length - half).reduce((x, y) => x + y, 0) / (amts.length - half) : 0}
  {@const trend = amts.length >= 4 ? (recent > prior * 1.1 ? '↑' : recent < prior * 0.9 ? '↓' : '→') : null}
  <div class="app">
    <aside class="sidenav">
      <div class="brand">
        <svg viewBox="0 0 400 400" width="30" height="30" class="drop"
          ><rect width="400" height="400" fill="#221e1d" rx="60" /><path
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
        ><b>nektar</b><span class="live" class:down={!live}><i></i>{live ? 'live' : 'offline'}</span>
      </div>
      <nav class="menu">
        <button class="mi" class:active={tab === 'overview'} onclick={() => (tab = 'overview')}><svg viewBox="0 0 20 20" class="mico"><path d="M3 10l7-7 7 7v8a1 1 0 01-1 1h-4v-5h-4v5H4a1 1 0 01-1-1z" fill="currentColor" /></svg>Overview</button>
        <button class="mi" class:active={tab === 'earnings'} onclick={() => (tab = 'earnings')}><svg viewBox="0 0 20 20" class="mico"><path d="M10 3v14M14.5 6H8.2a2.6 2.6 0 000 5.2h3.6a2.6 2.6 0 010 5.2H5.5" stroke="currentColor" fill="none" stroke-width="1.7" stroke-linecap="round" /></svg>Earnings</button>
        <button class="mi" class:active={tab === 'activity'} onclick={() => (tab = 'activity')}><svg viewBox="0 0 20 20" class="mico"><path d="M2.5 10h3l2-5.5 3 11 2-5.5h4.5" stroke="currentColor" fill="none" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" /></svg>Activity</button>
        <button class="mi" class:active={tab === 'network'} onclick={() => (tab = 'network')}><svg viewBox="0 0 20 20" class="mico"><circle cx="4.5" cy="10" r="2" fill="currentColor" /><circle cx="15" cy="5" r="2" fill="currentColor" /><circle cx="15" cy="15" r="2" fill="currentColor" /><path d="M6.5 9.3 13 5.9M6.5 10.7l6.5 3.4" stroke="currentColor" stroke-width="1.4" /></svg>Network</button>
        <button class="mi" class:active={tab === 'agent'} onclick={() => (tab = 'agent')}><svg viewBox="0 0 20 20" class="mico"><rect x="4.5" y="7" width="11" height="9" rx="2.2" stroke="currentColor" fill="none" stroke-width="1.6" /><circle cx="8" cy="11" r="1.1" fill="currentColor" /><circle cx="12" cy="11" r="1.1" fill="currentColor" /><path d="M10 4v3" stroke="currentColor" stroke-width="1.6" /></svg>Agent</button>
        <button class="mi" class:active={tab === 'settings'} onclick={() => (tab = 'settings')}><svg viewBox="0 0 20 20" class="mico"><path d="M3 6h9M16 6h1M3 14h1M8 14h9" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" /><circle cx="14" cy="6" r="2" stroke="currentColor" fill="none" stroke-width="1.6" /><circle cx="6" cy="14" r="2" stroke="currentColor" fill="none" stroke-width="1.6" /></svg>Settings</button>
        <div class="msep"></div>
        <button class="mi dbg-mi" class:active={tab === 'debug'} onclick={() => (tab = 'debug')}><svg viewBox="0 0 20 20" class="mico"><rect x="3" y="4" width="14" height="12" rx="2" stroke="currentColor" fill="none" stroke-width="1.6" /><path d="M6 8l2.5 2L6 12M10 12h4" stroke="currentColor" fill="none" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" /></svg>Debug</button>
      </nav>
      <div class="sfoot">
        <code class="pub">{s.pub}…</code>
        <small class="mut">{s.model} · {s.network} · {ago(s.uptimeSec)}</small>
      </div>
    </aside>

    <main class="feed">
      {#if tab === 'overview'}
      <section class="hero card" id="overview">
      <div class="herorow">
        <div class="earn">
          <div class="pre verdict" class:verdict-sell={s.receipts.count > 0}>{s.receipts.count > 0 ? 'selling' : s.market.length > 0 ? 'listed' : 'idle'}</div>
          <h1>{fmt(s.receipts.earned)} <em>sats</em></h1>
          {#if s.receipts.earned24h > 0}<div class="chg24">+{fmt(s.receipts.earned24h)} sats <small>· 24h</small></div>{/if}
          <p class="sub">
            {s.receipts.count} checkable work{s.receipts.count === 1 ? '' : 's'} · {s.market.length} domain{s.market.length === 1 ? '' : 's'} for sale{#if trend !== null} · earning {trend}{/if}
          </p>
        </div>
        <div class="heroright">
          {#if s.share.enabled}
            <button class="share" onclick={share}>{copied ? 'copied ✓' : "share your agent's record ↗"}</button>
          {/if}
        </div>
      </div>
      {#if s.receipts.amounts.length}
        <svg viewBox={`0 0 ${AW} ${AH}`} class="area" preserveAspectRatio="none">
          <defs>
            <linearGradient id="eg" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stop-color="rgba(76,194,107,.35)" />
              <stop offset="100%" stop-color="rgba(76,194,107,0)" />
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
      {/if}

      {#if tab === 'overview' || tab === 'earnings'}
      <div class="card" id="earnings">
          <h2>Earnings <small>every answer checkable by a stranger</small></h2>
          {#if tab === 'earnings'}<p class="tabhint">the feed a buyer reads — every row a verifiable earning · next: per-answer proof pages · payout history</p>{/if}
          {#each s.receipts.list as r}
            <div class="lrow">
              <svg viewBox="0 0 400 400" class="drop-sm"><rect width="400" height="400" fill="#221e1d" rx="60" /><path
                  fill="#f7991b"
                  d="M 195 55 C 178 55 164 63 157 80 C 153 100 149 112 143 124 C 136 139 115 158 93 186 C 80 206 73 226 72 250 C 71 277 79 306 98 329 C 117 352 150 372 190 377 C 232 380 255 356 259 322 C 261 296 274 273 296 251 C 310 234 318 210 318 186 C 318 158 312 137 302 120 C 292 102 276 84 260 74 C 245 64 218 55 195 55 Z"
                /></svg>
              <div class="lbody"><b>{r.tool}</b>{#each r.shards as sh}<i class="dom">{sh}</i>{/each}</div>
              <b class="lnum">{r.sats === null ? '—' : `+${fmt(r.sats)}`}<small> sats</small></b>
              {#if r.at}<span class="rago">{agoSince(r.at)}</span>{/if}
              <a href={r.url}>verify ↗</a>
            </div>
          {:else}
            <p class="empty">no works yet — the first groundedAnswer issues the first checkable earning</p>
          {/each}
        </div>
      {/if}

      {#if tab === 'overview' || tab === 'activity'}
        <div class="card" id="activity">
          <h2>
            Activity <small>
              {#if s.automation}
                {s.automation.ops.filter((o) => o.state === 'running').length} running · {s.automation.pending.length} approval{s.automation.pending.length === 1 ? '' : 's'} · {s.automation.rt.pending.length} waiting
              {:else}
                not running — start automation to trade unattended
              {/if}
            </small>
          </h2>
          {#if tab === 'activity'}<p class="tabhint">pending first, then the journaled steps — next: per-op invocation timelines</p>{/if}
          {#if s.automation}
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
            {#if s.automation.rt.journal.length}
              <div class="journal">
                {#each s.automation.rt.journal as e}
                  <div><span class="mono">{e.op}</span> → {e.to} <span class="mut">· {e.cause}</span></div>
                {/each}
              </div>
            {/if}
          {:else}
            <p class="empty">the agent earns unattended once automation is attached</p>
          {/if}
        </div>
      {/if}

      {#if tab === 'network'}
        <div class="card">
          <h2>Network <small>{s.net.hosts.length ? `${s.net.hosts.filter((h) => h.state === 'up').length} of ${s.net.hosts.length} hosts up` : 'single-player'}</small></h2>
          <p class="tabhint">who this instance trades through — next: peer cards · per-host lane stock · quorum depth</p>
          {#each s.net.hosts as h}
            <div class="row">
              <span class="mono">{h.url}{#if h.saved} <small class="mut">· saved</small>{/if}</span>
              <span class="rowend"><b class={h.state === 'down' ? 'bad' : h.state === 'up' ? 'ok' : ''}>{h.state}</b><button class="x" title="disconnect" onclick={() => disconnect(h.url)}>✕</button></span>
            </div>
          {:else}
            <p class="empty">no market hosts yet — join one below to start trading</p>
          {/each}
          {#if s.net.slots}
            <div class="rline">
              {#each Object.entries(s.net.slots) as [lane, n]}<span class="chip">{lane} {n}</span>{/each}
            </div>
          {/if}
          {#if s.net.seeds?.length}
            <div class="jline"><span class="mut">public hosts:</span>{#each s.net.seeds as seed}<button class="jbtn" onclick={() => joinHost(seed)}>join {host(seed)}</button>{/each}</div>
          {/if}
          <div class="jline">
            <input class="jin" bind:value={joinInput} placeholder="add a host — https://… or a .well-known URL" onkeydown={(e) => e.key === 'Enter' && joinHost(joinInput)} />
            <button class="jbtn" onclick={() => joinHost(joinInput)}>join</button>
          </div>
          {#if joinMsg}<p class="jmsg">{joinMsg}</p>{/if}
          {#if joinFound.length}
            <div class="jline"><span class="mut">it knows {joinFound.length} more:</span>{#each joinFound as h}<button class="jbtn" onclick={() => joinHost(h)}>+ {host(h)}</button>{/each}<button class="jbtn jok" onclick={joinAll}>add all</button></div>
          {/if}
        </div>
      {/if}

      {#if tab === 'agent'}
        <div class="card">
          <h2>Agent <small>{s.automation ? `${s.automation.ops.filter((o) => o.state === 'running').length} running` : 'idle'}</small></h2>
          <p class="tabhint">the runtime you leave running — next: workflow history per op · awakeable stats</p>
          {#if s.automation}
            {#each s.automation.ops as o}
              <div class="row"><span class="mut">{o.role} {o.kind}</span><b class={o.lastError ? 'bad' : o.state === 'running' ? 'ok' : ''}>{o.state}{o.lastError ? ` — ${o.lastError}` : ''}</b></div>
            {/each}
            <div class="rline">
              <span class="chip">executor {s.automation.rt.executor}</span>
              <span class="chip">{s.automation.rt.pending.length} in outbox</span>
              <span class="chip">drain {s.automation.rt.nextAt ? new Date(s.automation.rt.nextAt).toLocaleTimeString() : 'idle'}</span>
            </div>
          {:else}
            <p class="empty">leave the agent running — it answers, prices and collects while you're away</p>
          {/if}
        </div>
      {/if}

      {#if tab === 'settings'}
        <div class="card">
          <h2>Settings <small>the knobs this instance runs under</small></h2>
          <div class="subpills">
            {#each ['policy', 'identity', 'hosts', 'runtime'] as p}
              <button class="pill" class:on={sub === p} onclick={() => (sub = p as typeof sub)}>{p}</button>
            {/each}
          </div>

          {#if sub === 'policy'}
            {#if s.settings.policy}
              {@const p = s.settings.policy}
              <div class="rline">
                <span class="chip">{p.roles.join(' + ')}</span>
                {#each p.domains ?? [] as dm}<span class="chip">{dm}</span>{/each}
                <span class="chip">policy {String(p.policyId).slice(0, 12)}…</span>
              </div>
              <table class="jt"><thead><tr><th>limit</th><th>value</th></tr></thead><tbody>
                {#each Object.entries(p.limits ?? {}) as [k, v]}<tr><td class="mut">{k}</td><td>{String(v)}</td></tr>{/each}
              </tbody></table>
              {#if p.seller}
                <table class="jt"><thead><tr><th>seller</th><th>value</th></tr></thead><tbody>
                  {#each Object.entries(p.seller) as [k, v]}<tr><td class="mut">{k}</td><td>{Array.isArray(v) ? v.join(', ') : String(v)}</td></tr>{/each}
                </tbody></table>
              {/if}
              <pre class="dbg">{JSON.stringify(p, null, 2)}</pre>
            {:else}
              <p class="empty">no seller policy approved yet — the agent can't trade until one is set (setPolicy via MCP)</p>
            {/if}
          {/if}

          {#if sub === 'identity'}
            <table class="jt"><tbody>
              <tr><td class="mut">pubkey</td><td class="mono">{s.pub}…</td></tr>
              <tr><td class="mut">model</td><td>{s.model}</td></tr>
              <tr><td class="mut">network</td><td>{s.network}</td></tr>
              <tr><td class="mut">uptime</td><td>{ago(s.uptimeSec)}</td></tr>
              <tr><td class="mut">card</td><td><a href={s.share.url}>{s.share.url}</a></td></tr>
            </tbody></table>
          {/if}

          {#if sub === 'hosts'}
            <table class="jt"><thead><tr><th>source</th><th>hosts</th></tr></thead><tbody>
              <tr><td class="mut">env / seeds</td><td class="mono">{s.settings.hosts.env.join(', ') || '—'}</td></tr>
              <tr><td class="mut">joined (saved)</td><td class="mono">{s.settings.hosts.added.join(', ') || '—'}</td></tr>
              <tr><td class="mut">left (blocked)</td><td class="mono">{s.settings.hosts.blocked.join(', ') || '—'}</td></tr>
            </tbody></table>
          {/if}

          {#if sub === 'runtime'}
            <table class="jt"><tbody>
              <tr><td class="mut">stream</td><td><b class={live ? 'ok' : 'bad'}>{live ? 'live' : 'offline'}</b></td></tr>
              <tr><td class="mut">share page</td><td>{s.share.enabled ? 'on' : 'off (NEKTAR_PACK_SHARE=0)'}</td></tr>
              <tr><td class="mut">push url</td><td class="mono">{s.net.push ?? '—'}</td></tr>
              {#if s.automation}
                <tr><td class="mut">executor</td><td>{s.automation.rt.executor}</td></tr>
                <tr><td class="mut">outbox</td><td>{s.automation.rt.pending.length} pending · drain {s.automation.rt.nextAt ? new Date(s.automation.rt.nextAt).toLocaleTimeString() : 'idle'}</td></tr>
              {:else}
                <tr><td class="mut">automation</td><td>not attached</td></tr>
              {/if}
            </tbody></table>
          {/if}
        </div>
      {/if}

      {#if tab === 'debug'}
        <div class="card">
          <h2>Debug <small>operator diagnostics — local only</small></h2>
          <p class="tabhint">the Restate introspection UI mounts here once the executor port lands — invocations, objects, awakeables</p>
          {#if s.automation}
            <div class="rline">
              <span class="chip">executor {s.automation.rt.executor}</span>
              <span class="chip">{s.automation.rt.pending.length} in outbox</span>
              <span class="chip">drain {s.automation.rt.nextAt ? new Date(s.automation.rt.nextAt).toLocaleTimeString() : 'idle'}</span>
            </div>
            {#if s.automation.rt.adminUrl}
              <iframe class="restate" src={s.automation.rt.adminUrl} title="restate ui"></iframe>
            {:else}
              <div class="rph"><b>restate ui</b><p>embeds at the executor's admin endpoint once the port lands — the journaled steps below are what it will introspect</p></div>
            {/if}
            {#if s.automation.rt.pending.length}
              <table class="jt"><thead><tr><th>kind</th><th>op</th><th>status</th><th>attempts</th><th>next</th></tr></thead><tbody>
                {#each s.automation.rt.pending as p}
                  <tr><td>{p.kind}</td><td class="mono">{p.operationId.slice(0, 8)}…</td><td>{p.status}{p.deferClass ? ` · ${p.deferClass}` : ''}{p.lastError ? ` · ${p.lastError}` : ''}</td><td>{p.attempts}</td><td class="mut">{new Date(p.nextAt).toLocaleTimeString()}</td></tr>
                {/each}
              </tbody></table>
            {/if}
            {#if s.automation.rt.journal.length}
              <table class="jt"><thead><tr><th>at</th><th>op</th><th>to</th><th>cause</th></tr></thead><tbody>
                {#each s.automation.rt.journal as e}
                  <tr><td class="mut">{new Date(e.at).toLocaleTimeString()}</td><td class="mono">{e.op}</td><td>{e.to}</td><td class="mut">{e.cause}</td></tr>
                {/each}
              </tbody></table>
            {/if}
          {:else}
            <p class="empty">automation not attached — nothing to introspect yet</p>
          {/if}
        </div>
        <div class="card">
          <h2>State <small>GET /state verbatim</small></h2>
          <pre class="dbg">{JSON.stringify(s, null, 2)}</pre>
        </div>
      {/if}
    </main>

    <aside class="rail">
        <div class="card">
          <h2>Wallet</h2>
          <div class="wbal">{fmt(s.treasury.sats)} <em>sats</em></div>
          <p class="wmeta">{s.treasury.utxos} reserve output{s.treasury.utxos === 1 ? '' : 's'} · payments {s.treasury.payments} · {s.treasury.funded ? 'funded' : 'needs funding'}</p>
        </div>

        <div class="card" id="network">
          <h2>Network <small>{s.net.hosts.length ? `${s.net.hosts.filter((h) => h.state === 'up').length} of ${s.net.hosts.length} hosts up` : 'single-player'}</small></h2>
          {#each s.net.hosts as h}
            <div class="row"><span class="mono">{host(h.url)}</span><b class={h.state === 'down' ? 'bad' : h.state === 'up' ? 'ok' : ''}>{h.state}</b></div>
          {:else}
            <p class="empty">no market hosts configured — set NEKTAR_MARKET_HOSTS to join a network</p>
          {/each}
          {#if s.net.slots}
            <div class="rline">
              {#each Object.entries(s.net.slots) as [lane, n]}<span class="chip">{lane} {n}</span>{/each}
            </div>
          {/if}
        </div>

        <div class="card" id="agent">
          <h2>Agent</h2>
          {#if s.automation}
            {#if s.automation.ops.length}
              <div class="ostrip">
                {#each s.automation.ops as o}<i class={o.lastError ? 's-err' : o.state === 'running' ? 's-run' : 's-idle'} title="{o.role} {o.kind} — {o.state}{o.lastError ? ` (${o.lastError})` : ''}"></i>{/each}
              </div>
            {/if}
            <div class="rline">
              <span class="chip">executor {s.automation.rt.executor}</span>
              <span class="chip">{s.automation.rt.pending.length} in outbox</span>
              <span class="chip">drain {s.automation.rt.nextAt ? new Date(s.automation.rt.nextAt).toLocaleTimeString() : 'idle'}</span>
            </div>
          {:else}
            <p class="empty">leave the agent running — it answers, prices and collects while you're away</p>
          {/if}
        </div>
    </aside>
  </div>
{/if}

<style>
  :global(*) {
    box-sizing: border-box;
  }
  :global(html) {
    scroll-behavior: smooth;
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
  .app {
    display: grid;
    grid-template-columns: 210px minmax(0, 1fr) 360px;
    gap: 1.4rem;
    width: min(1440px, 100%);
    margin: 0 auto;
    padding: 1.4rem 1.6rem;
    align-items: start;
    position: relative;
  }
  .app::before {
    content: '';
    position: absolute;
    inset: -200px -100px auto;
    height: 480px;
    background: radial-gradient(ellipse 60% 55% at 55% 0%, rgb(247 153 27 / 11%), transparent 70%);
    pointer-events: none;
    z-index: -1;
  }
  .sidenav {
    position: sticky;
    top: 0;
    display: flex;
    flex-direction: column;
    gap: 1.4rem;
    min-height: calc(100vh - 3rem);
    padding: 0.5rem 0.4rem;
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 0.55rem;
    font-size: 1.15rem;
    font-weight: 800;
  }
  .brand .live {
    margin-left: auto;
  }
  .menu {
    display: grid;
    gap: 0.15rem;
  }
  .mi {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    padding: 0.6rem 0.7rem;
    border-radius: 10px;
    color: var(--mut);
    text-decoration: none;
    font-weight: 600;
    font-size: 0.92rem;
    background: none;
    border: 0;
    font-family: inherit;
    text-align: left;
    cursor: pointer;
    transition:
      background 0.15s,
      color 0.15s;
  }
  .mi:hover {
    background: rgb(247 153 27 / 8%);
    color: #ece7e4;
  }
  .mi.active {
    color: var(--amber);
    background: rgb(247 153 27 / 10%);
  }
  .mico {
    width: 18px;
    height: 18px;
    flex-shrink: 0;
  }
  .sfoot {
    margin-top: auto;
    display: grid;
    gap: 0.3rem;
    font-size: 0.75rem;
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
  .mut {
    color: var(--mut);
  }
  .pub {
    background: var(--card2);
    padding: 2px 8px;
    border-radius: 5px;
    font-size: 0.75rem;
    color: var(--mut);
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .feed {
    display: grid;
    gap: 1.2rem;
    min-width: 0;
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
  .area {
    display: block;
    width: 100%;
    height: 130px;
    margin-top: 1rem;
  }
  .agrid {
    stroke: rgb(76 194 107 / 9%);
    stroke-width: 1;
  }
  .aline {
    fill: none;
    stroke: var(--ok);
    stroke-width: 2.5;
    stroke-linejoin: round;
    filter: drop-shadow(0 0 7px rgb(76 194 107 / 45%));
  }
  .adot {
    fill: var(--ok);
    filter: drop-shadow(0 0 6px rgb(76 194 107 / 80%));
  }
  .chg24 {
    color: var(--ok);
    font-weight: 800;
    font-size: 1.05rem;
    font-variant-numeric: tabular-nums;
    margin-top: 0.15rem;
  }
  .chg24 small {
    color: var(--mut);
    font-weight: 600;
    font-size: 0.75rem;
  }
  .rago {
    color: var(--mut);
    font-size: 0.72rem;
    white-space: nowrap;
  }
  .rail {
    display: grid;
    gap: 1.2rem;
    position: sticky;
    top: 1.2rem;
  }
  .lrow {
    display: flex;
    align-items: center;
    gap: 1rem;
    padding: 0.85rem 0.4rem;
    border-bottom: 1px solid rgb(247 153 27 / 8%);
    font-size: 0.9rem;
  }
  .lrow:last-of-type {
    border-bottom: none;
  }
  .lbody {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .lnum {
    color: var(--amber);
    font-size: 1.05rem;
    font-weight: 800;
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  .lnum small {
    color: var(--mut);
    font-size: 0.7rem;
    font-weight: 600;
  }
  .msep {
    border-top: 1px solid var(--line);
    margin: 0.35rem 0.5rem;
  }
  .dbg-mi {
    font-size: 0.82rem;
    color: var(--mut);
    opacity: 0.7;
  }
  .dbg-mi.active {
    opacity: 1;
  }
  .tabhint {
    color: var(--mut);
    font-size: 0.78rem;
    margin: -0.5rem 0 1rem;
  }
  .jt {
    width: 100%;
    border-collapse: collapse;
    font-size: 0.78rem;
    margin-top: 0.8rem;
  }
  .jt th {
    text-align: left;
    color: var(--mut);
    font-weight: 600;
    font-size: 0.66rem;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    padding: 0.35rem 0.5rem;
    border-bottom: 1px solid var(--line);
  }
  .jt td {
    padding: 0.45rem 0.5rem;
    border-bottom: 1px solid #1d1816;
  }
  .dbg {
    background: #0d0a09;
    border: 1px solid var(--line);
    border-radius: 10px;
    padding: 0.85rem;
    font-size: 0.72rem;
    line-height: 1.5;
    max-height: 420px;
    overflow: auto;
    margin: 0;
    white-space: pre-wrap;
    word-break: break-word;
  }
  .restate {
    width: 100%;
    height: 440px;
    border: 1px solid var(--line);
    border-radius: 10px;
    background: #fff;
    margin-top: 0.8rem;
  }
  .rph {
    border: 1px dashed var(--line);
    border-radius: 10px;
    padding: 1rem;
    margin-top: 0.8rem;
  }
  .rph b {
    color: var(--amber);
    font-size: 0.85rem;
  }
  .rph p {
    color: var(--mut);
    font-size: 0.78rem;
    margin: 0.3rem 0 0;
  }
  .jline {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    flex-wrap: wrap;
    margin-top: 0.7rem;
  }
  .jline .mut {
    font-size: 0.78rem;
  }
  .jin {
    flex: 1;
    min-width: 220px;
    background: #0d0a09;
    border: 1px solid var(--line);
    border-radius: 10px;
    color: #ece7e4;
    padding: 0.5rem 0.7rem;
    font: inherit;
    font-size: 0.85rem;
  }
  .jin:focus {
    outline: none;
    border-color: var(--amber);
  }
  .jbtn {
    background: #241c14;
    border: 1px solid var(--line);
    border-radius: 10px;
    color: var(--amber);
    padding: 0.45rem 0.75rem;
    font: inherit;
    font-size: 0.8rem;
    font-weight: 700;
    cursor: pointer;
    white-space: nowrap;
  }
  .jbtn:hover {
    border-color: var(--amber);
  }
  .jok {
    color: var(--ok);
  }
  .jmsg {
    color: var(--ok);
    font-size: 0.8rem;
    margin: 0.6rem 0 0;
  }
  .rowend {
    margin-left: auto;
    display: flex;
    align-items: center;
    gap: 0.6rem;
  }
  .x {
    background: none;
    border: 1px solid transparent;
    border-radius: 8px;
    color: var(--mut);
    font-size: 0.8rem;
    padding: 0.15rem 0.45rem;
    cursor: pointer;
    opacity: 0.6;
  }
  .x:hover {
    color: var(--bad);
    border-color: var(--bad);
    opacity: 1;
  }
  .subpills {
    display: flex;
    gap: 0.4rem;
    margin: -0.4rem 0 1rem;
  }
  .pill {
    background: #241c14;
    border: 1px solid var(--line);
    border-radius: 999px;
    color: var(--mut);
    padding: 0.3rem 0.8rem;
    font: inherit;
    font-size: 0.78rem;
    font-weight: 700;
    cursor: pointer;
    text-transform: capitalize;
  }
  .pill.on {
    color: var(--amber);
    border-color: var(--amber);
  }
  .drop-sm {
    width: 30px;
    height: 30px;
    border-radius: 50%;
    flex-shrink: 0;
  }
  .wbal {
    font-size: 1.9rem;
    font-weight: 800;
    color: var(--amber);
    font-variant-numeric: tabular-nums;
  }
  .wbal em {
    font-style: normal;
    font-size: 0.9rem;
    color: var(--mut);
    font-weight: 600;
  }
  .wmeta {
    color: var(--mut);
    font-size: 0.72rem;
    margin: 0.3rem 0 0;
  }
  @media (max-width: 920px) {
    .app {
      grid-template-columns: 1fr;
    }
    .sidenav {
      position: static;
      min-height: 0;
      flex-direction: row;
      align-items: center;
      gap: 1rem;
    }
    .menu {
      display: flex;
      flex-wrap: wrap;
      gap: 0.15rem;
    }
    .sfoot {
      display: none;
    }
    .rail {
      position: static;
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
  .lrow a {
    color: var(--amber);
    text-decoration: none;
    font-size: 0.78rem;
    white-space: nowrap;
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
  .verdict {
    font-weight: 800;
    letter-spacing: 0.22em;
    color: var(--mut);
  }
  .verdict-sell {
    color: var(--amber);
    text-shadow: 0 0 24px rgba(247, 153, 27, 0.5);
  }
</style>
