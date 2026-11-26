<script lang="ts">
  import type { DealFeedEntry, DockClient, FeedFragment, PeerInfo, Receipt, State } from '../lib/types'
  import { operatorFetch, postJSON } from '../lib/api'
  import { fmtSats } from '../lib/format'
  import FeedItem, { type TimelineEntry } from './FeedItem.svelte'

  interface Props {
    s: State
    live: boolean
    dockedApps: DockClient[]
    searchQuery: string
    onSearch: (q: string) => void
    onOpenDock: () => void
    onRefresh: () => Promise<void>
    onRefreshDock: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
    onQuickBuyTopic: (domain: string, sats: number) => void
    onInspectMemory: (m: FeedFragment) => void
    onInspectReceipt: (r: Receipt) => void
    onInspectDeal: (d: DealFeedEntry) => void
    onInspectPeer: (p: PeerInfo) => void
    onForgetMemory: (shard: string, seq: number) => void
    onDecideApproval: (id: string, approve: boolean) => void
    onShareUrl: (url: string) => void
  }

  let {
    s,
    live,
    dockedApps,
    searchQuery,
    onSearch,
    onOpenDock,
    onRefresh,
    onRefreshDock,
    onToast,
    onQuickBuyTopic,
    onInspectMemory,
    onInspectReceipt,
    onInspectDeal,
    onInspectPeer,
    onForgetMemory,
    onDecideApproval,
    onShareUrl
  }: Props = $props()

  let selectedShard = $state<string>('__all__')
  let noteText = $state('')
  let noteShard = $state('notes')
  let busy = $state(false)
  let dockBusyId = $state('')

  // Keep private remember shard synced with selected shard when picking one
  $effect(() => {
    if (selectedShard !== '__all__') {
      noteShard = selectedShard
    }
  })

  const settledDeals = $derived((s.automation?.deals ?? []).filter((d) => d.to === 'settled'))
  const totalEarned = $derived.by(() => {
    const dealSum = settledDeals.filter((d) => d.role === 'seller').reduce((acc, d) => acc + (d.priceSats ?? 0), 0)
    return Math.max(s.receipts.earned, dealSum)
  })
  const liveListings = $derived((s.market ?? []).filter((m) => m.active !== false))
  const dockedList = $derived(dockedApps.filter((c) => c.docked))
  const detectedUndocked = $derived(dockedApps.filter((c) => c.detected && !c.docked))
  const peers = $derived(s.net.peers ?? [])

  // Build unified shard knowledge directory (merging market offerings + local fragments)
  interface ShardSummary {
    shard: string
    records: number
    anchored: number
    listed: boolean
    active: boolean
    price: string
    sold: number
    earned: number
  }

  const shardSummaries = $derived.by<ShardSummary[]>(() => {
    const map = new Map<string, ShardSummary>()
    for (const m of s.market ?? []) {
      map.set(m.shard, {
        shard: m.shard,
        records: m.records,
        anchored: 0,
        listed: true,
        active: m.active !== false,
        price: m.offers,
        sold: m.works,
        earned: m.earned
      })
    }
    for (const f of s.feed ?? []) {
      const existing = map.get(f.shard)
      if (existing) {
        existing.records = Math.max(existing.records, 1)
        if (f.batchN >= 0) existing.anchored += 1
      } else {
        map.set(f.shard, {
          shard: f.shard,
          records: 1,
          anchored: f.batchN >= 0 ? 1 : 0,
          listed: false,
          active: false,
          price: '',
          sold: 0,
          earned: 0
        })
      }
    }
    // Count exact visible fragments for unlisted shards
    const fragCounts = new Map<string, number>()
    for (const f of s.feed ?? []) {
      fragCounts.set(f.shard, (fragCounts.get(f.shard) ?? 0) + 1)
    }
    for (const [sh, cnt] of fragCounts) {
      const row = map.get(sh)
      if (row && !row.listed) row.records = cnt
    }

    return Array.from(map.values()).sort((a, b) => {
      if (a.listed !== b.listed) return a.listed ? -1 : 1
      if (a.earned !== b.earned) return b.earned - a.earned
      return b.records - a.records
    })
  })

  // Filtered knowledge entries for the selected shard (or all)
  const knowledgeEntries = $derived.by(() => {
    const out: TimelineEntry[] = []
    for (const a of s.automation?.pending ?? []) {
      out.push({ kind: 'approval', sortAt: Number.MAX_SAFE_INTEGER, data: a })
    }
    for (const m of s.feed ?? []) {
      if (selectedShard !== '__all__' && m.shard !== selectedShard) continue
      out.push({ kind: 'memory', sortAt: (m.createdAt || 0) * 1000, data: m })
    }
    for (const r of s.receipts.list ?? []) {
      if (selectedShard !== '__all__' && !r.shards.includes(selectedShard)) continue
      out.push({ kind: 'receipt', sortAt: r.at ?? 0, data: r })
    }
    for (const d of s.automation?.deals ?? []) {
      if (selectedShard !== '__all__' && d.shard !== selectedShard) continue
      out.push({ kind: 'deal', sortAt: d.at ?? 0, data: d })
    }
    out.sort((a, b) => b.sortAt - a.sortAt)

    const q = searchQuery.trim().toLowerCase()
    if (!q) return out
    return out.filter((e) => {
      if (e.kind === 'memory') return `${e.data.shard} ${e.data.text}`.toLowerCase().includes(q)
      if (e.kind === 'receipt') return `${e.data.answer ?? ''} ${e.data.shards.join(' ')}`.toLowerCase().includes(q)
      if (e.kind === 'deal') return `${e.data.cause} ${e.data.to} ${e.data.shard ?? ''}`.toLowerCase().includes(q)
      return e.data.reason.toLowerCase().includes(q)
    })
  })

  async function saveMemory() {
    const body = noteText.trim()
    if (!body || busy) return
    busy = true
    try {
      const shard = noteShard.trim().replace(/^#/, '') || 'notes'
      const res = await postJSON<{ anchorTxid?: string }>('/v1/remember', {
        shard,
        text: body,
        autoAnchor: s.treasury.funded
      })
      if (!res.ok) {
        onToast(res.data.error ?? `Save failed (${res.status})`, true)
        return
      }
      noteText = ''
      onToast(res.data.anchorTxid ? `Saved & anchored in #${shard} ✓` : `Saved to #${shard} ✓`)
      await onRefresh()
    } catch (e) {
      onToast(String(e), true)
    } finally {
      busy = false
    }
  }

  async function toggleListing(shard: string, active: boolean) {
    const r = await operatorFetch('PATCH', `/v1/market/listings/${encodeURIComponent(shard)}`, { active }, 'Update offering')
    if (r && r.ok) {
      onToast(active ? `Resumed #${shard}` : `Paused #${shard}`)
      await onRefresh()
    }
  }

  async function quickDock(clientId: string) {
    dockBusyId = clientId
    try {
      const r = await operatorFetch('POST', '/v1/dock/apply', { client: clientId, uninstall: false }, `Dock ${clientId}`)
      if (!r) return
      if (!r.ok) {
        onToast(`Could not dock ${clientId}`, true)
        return
      }
      onToast(`Docked ${clientId} ✓`)
      await onRefreshDock()
    } finally {
      dockBusyId = ''
    }
  }
</script>

<div class="agents-page">
  <!-- Full-Width Sticky Header -->
  <header class="topbar">
    <div class="tleft">
      <h1>Agents</h1>
      <div class="search-box">
        <svg viewBox="0 0 20 20" width="14" height="14" class="sico">
          <circle cx="9" cy="9" r="5.5" fill="none" stroke="currentColor" stroke-width="1.7" />
          <path d="M13.2 13.2L17 17" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" />
        </svg>
        <input
          value={searchQuery}
          oninput={(e) => onSearch((e.currentTarget as HTMLInputElement).value)}
          placeholder="Filter agents, shards, facts…"
          aria-label="Filter agents and knowledge"
        />
        {#if searchQuery}
          <button class="clr" onclick={() => onSearch('')} aria-label="Clear">✕</button>
        {/if}
      </div>
    </div>

    <div class="tright">
      <div class="kpi-pill">
        <span class="klbl">Earned</span>
        <b class="kval ok">+{fmtSats(totalEarned)}s</b>
      </div>
      <div class="kpi-pill">
        <span class="klbl">Knowledge</span>
        <b class="kval">{s.memory.anchored}/{s.memory.records}</b>
      </div>
      <button class="dock-cta" onclick={onOpenDock}>+ Dock Agent</button>
    </div>
  </header>

  <!-- 1. RUNNING AGENT FLEET -->
  <section class="fleet-section">
    <div class="sec-head">
      <span>RUNNING FLEET</span>
      <span class="sec-cnt">{1 + dockedList.length + (s.ai?.enabled ? 1 : 0) + peers.length} active</span>
    </div>

    <div class="fleet-grid">
      <!-- Primary Local Autonomous Trading Node -->
      <article class="acard primary">
        <div class="acard-top">
          <span class="status-dot" class:on={live}></span>
          <div class="atitle">
            <b>Nektar Node</b>
            <small>{liveListings.length > 0 ? `Offering ${liveListings.length} shard${liveListings.length === 1 ? '' : 's'}` : 'Autonomous market broker'}</small>
          </div>
          <span class="earn-badge" class:pos={totalEarned > 0}>+{fmtSats(totalEarned)}s</span>
        </div>

        {#if liveListings.length > 0}
          <div class="offer-chips">
            {#each liveListings.slice(0, 4) as l}
              <button class="ochip" onclick={() => (selectedShard = l.shard)}>
                <span>#{l.shard}</span>
                <em>{l.offers}</em>
              </button>
            {/each}
          </div>
        {:else}
          <p class="anote">Ready to auto-quote incoming wants and anchor verified facts.</p>
        {/if}

        <div class="acard-foot">
          <span>{settledDeals.length} settled</span>
          <span>·</span>
          <span>{s.receipts.count} receipts</span>
          <span>·</span>
          <span>{s.memory.records} facts</span>
        </div>
      </article>

      <!-- Docked Local AI Clients (Claude Code, Cursor, Codex, etc.) -->
      {#each dockedList as c}
        <article class="acard">
          <div class="acard-top">
            <span class="status-dot on"></span>
            <div class="atitle">
              <b>{c.label}</b>
              <small>{c.tierLabel} · Docked</small>
            </div>
            <span class="tag-pill ok">LIVE</span>
          </div>
          <p class="anote">{c.summary}</p>
          <div class="acard-foot">
            <span>Shared memory vault</span>
            <span>·</span>
            <span>{s.memory.shards} shards</span>
          </div>
        </article>
      {/each}

      <!-- Detected but Undocked Local AI Clients (1-click dock) -->
      {#each detectedUndocked.slice(0, 3) as c}
        <article class="acard subtle">
          <div class="acard-top">
            <span class="status-dot"></span>
            <div class="atitle">
              <b>{c.label}</b>
              <small>Detected locally</small>
            </div>
            {#if c.oneClick}
              <button class="mini-dock" onclick={() => quickDock(c.id)} disabled={dockBusyId === c.id}>
                {dockBusyId === c.id ? '…' : 'Dock'}
              </button>
            {/if}
          </div>
          <p class="anote">{c.summary}</p>
        </article>
      {/each}

      <!-- Jev Semantic Matcher (when enabled) -->
      {#if s.ai?.enabled}
        <article class="acard">
          <div class="acard-top">
            <span class="status-dot" class:on={!s.ai.circuitOpen}></span>
            <div class="atitle">
              <b>Jev Matcher</b>
              <small>{s.ai.model || s.ai.mode}</small>
            </div>
            <span class="tag-pill">{s.ai.avgLatencyMs ? `${Math.round(s.ai.avgLatencyMs)}ms` : s.ai.mode}</span>
          </div>
          <p class="anote">Scores semantic relevance between incoming wants and local knowledge.</p>
          <div class="acard-foot">
            <span>{s.ai.requests ?? 0} scored</span>
            {#if s.ai.escalations}<span>· {s.ai.escalations} escalated</span>{/if}
          </div>
        </article>
      {/if}

      <!-- Active Network Peer Agents -->
      {#each peers as p}
        {@const firstDom = p.sellDomains?.[0]?.replace(/^cluster:/, '') ?? ''}
        <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_noninteractive_element_interactions -->
        <article class="acard clickable" onclick={() => onInspectPeer(p)}>
          <div class="acard-top">
            <span class="status-dot on"></span>
            <div class="atitle">
              <b>{p.handle || `${p.pub.slice(0, 10)}…`}</b>
              <small>{p.isHub ? 'Overlay Hub' : 'Network Agent'}</small>
            </div>
            {#if firstDom}
              <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
              <button
                class="mini-buy"
                onclick={(e) => {
                  e.stopPropagation()
                  onQuickBuyTopic(firstDom, 100)
                }}
              >
                Buy →
              </button>
            {/if}
          </div>
          {#if p.sellDomains?.length}
            <div class="offer-chips">
              {#each p.sellDomains.slice(0, 3) as dm}
                <span class="ochip static">#{dm.replace(/^cluster:/, '')}</span>
              {/each}
            </div>
          {/if}
        </article>
      {/each}
    </div>
  </section>

  <!-- 2. WHAT THEY KNOW (Unified Knowledge Shards + Verified Facts Brain) -->
  <section class="brain-section">
    <aside class="shards-col">
      <div class="sec-head">
        <span>WHAT THEY KNOW</span>
        <span class="sec-cnt">{shardSummaries.length}</span>
      </div>

      <button class="srow" class:on={selectedShard === '__all__'} onclick={() => (selectedShard = '__all__')}>
        <div class="srow-top">
          <b>All Knowledge</b>
          <span class="scnt">{s.memory.records}</span>
        </div>
        <div class="srow-sub">{s.memory.anchored} anchored on BSV</div>
      </button>

      {#each shardSummaries as sh (sh.shard)}
        <div class="srow-wrap" class:on={selectedShard === sh.shard}>
          <button class="srow-btn" onclick={() => (selectedShard = sh.shard)}>
            <div class="srow-top">
              <b class:amber={sh.listed}>#{sh.shard}</b>
              {#if sh.earned > 0}
                <span class="searn">+{fmtSats(sh.earned)}s</span>
              {:else if sh.listed}
                <span class="sprice">{sh.price}</span>
              {:else}
                <span class="scnt">{sh.records}</span>
              {/if}
            </div>
            <div class="srow-sub">
              <span>{sh.records} fact{sh.records === 1 ? '' : 's'}</span>
              {#if sh.listed}
                <span>·</span>
                <span class:ok={sh.active}>{sh.active ? 'Offering' : 'Paused'}</span>
                {#if sh.sold > 0}<span>· {sh.sold} sold</span>{/if}
              {:else}
                <span>· Private</span>
              {/if}
            </div>
          </button>
          {#if sh.listed}
            <button
              class="spause"
              onclick={() => toggleListing(sh.shard, !sh.active)}
              title={sh.active ? 'Pause market offering' : 'Resume market offering'}
            >
              {sh.active ? 'Pause' : 'Live'}
            </button>
          {/if}
        </div>
      {/each}
    </aside>

    <div class="facts-col">
      <!-- Single-purpose Private Knowledge Input -->
      <div class="remember-box">
        <textarea
          bind:value={noteText}
          rows="2"
          placeholder="Teach your agents a private fact to remember &amp; anchor on BSV…"
          disabled={busy}
          onkeydown={(e) => {
            if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
              e.preventDefault()
              void saveMemory()
            }
          }}
        ></textarea>
        <div class="rbar">
          <label class="shard-in">
            <span>#</span>
            <input bind:value={noteShard} placeholder="notes" aria-label="Shard name" />
          </label>
          <button class="rbtn" onclick={saveMemory} disabled={busy || !noteText.trim()}>
            {busy ? 'Saving…' : 'Remember'}
          </button>
        </div>
      </div>

      {#if knowledgeEntries.length === 0}
        <div class="empty-facts">
          <p>{searchQuery.trim() ? `Nothing matching “${searchQuery}”.` : 'No verified facts in this shard yet.'}</p>
        </div>
      {:else}
        {#each knowledgeEntries as entry}
          <FeedItem
            {entry}
            {onInspectMemory}
            {onInspectReceipt}
            {onInspectDeal}
            {onForgetMemory}
            {onDecideApproval}
            {onShareUrl}
          />
        {/each}
      {/if}
    </div>
  </section>
</div>

<style>
  .agents-page {
    display: flex;
    flex-direction: column;
    min-height: 100vh;
  }

  /* Header */
  .topbar {
    position: sticky;
    top: 0;
    z-index: 20;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    padding: 0.65rem 1.25rem;
    background: rgb(9 8 7 / 92%);
    backdrop-filter: blur(12px);
    border-bottom: 1px solid var(--line);
    flex-wrap: wrap;
  }
  .tleft {
    display: flex;
    align-items: center;
    gap: 0.9rem;
    flex: 1;
    min-width: 240px;
  }
  h1 {
    margin: 0;
    font-size: 1.08rem;
    font-weight: 800;
    letter-spacing: -0.02em;
  }
  .search-box {
    flex: 1;
    max-width: 320px;
    display: flex;
    align-items: center;
    gap: 0.45rem;
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 0.34rem 0.75rem;
  }
  .search-box:focus-within { border-color: var(--line2); }
  .sico { color: var(--mut2); flex-shrink: 0; }
  .search-box input {
    flex: 1; min-width: 0; background: none; border: 0;
    color: var(--ink); font: inherit; font-size: 0.8rem; outline: none;
  }
  .search-box input::placeholder { color: var(--mut2); }
  .clr { background: none; border: 0; color: var(--mut2); cursor: pointer; font-size: 0.72rem; }
  .tright {
    display: flex;
    align-items: center;
    gap: 0.55rem;
  }
  .kpi-pill {
    display: inline-flex;
    align-items: center;
    gap: 0.4rem;
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 0.28rem 0.72rem;
    font-size: 0.76rem;
  }
  .klbl { color: var(--mut); }
  .kval { font-family: ui-monospace, monospace; font-weight: 800; font-variant-numeric: tabular-nums; }
  .kval.ok { color: var(--ok); }
  .dock-cta {
    background: var(--amber);
    color: #120e0c;
    border: 0;
    border-radius: 999px;
    padding: 0.36rem 0.92rem;
    font: inherit;
    font-size: 0.78rem;
    font-weight: 800;
    cursor: pointer;
    transition: filter 0.14s;
  }
  .dock-cta:hover { filter: brightness(1.08); }

  /* Fleet Section */
  .fleet-section {
    padding: 1.05rem 1.25rem 1.25rem;
    border-bottom: 1px solid var(--line);
    display: flex;
    flex-direction: column;
    gap: 0.7rem;
  }
  .sec-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    font-size: 0.68rem;
    font-weight: 800;
    letter-spacing: 0.1em;
    color: var(--mut2);
  }
  .sec-cnt { font-family: ui-monospace, monospace; color: var(--mut); }
  .fleet-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(265px, 1fr));
    gap: 0.75rem;
  }
  .acard {
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 14px;
    padding: 0.9rem 1rem;
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
    transition: border-color 0.14s, background 0.14s;
  }
  .acard.primary {
    border-color: rgb(247 153 27 / 30%);
    background: radial-gradient(ellipse 90% 80% at 100% 0%, rgb(247 153 27 / 7%), transparent 70%), var(--surf);
  }
  .acard.subtle { opacity: 0.85; }
  .acard.clickable { cursor: pointer; }
  .acard.clickable:hover { border-color: var(--line2); background: var(--hover); }
  .acard-top {
    display: flex;
    align-items: center;
    gap: 0.6rem;
  }
  .status-dot {
    width: 8px; height: 8px; border-radius: 50%; background: var(--mut2); flex-shrink: 0;
  }
  .status-dot.on { background: var(--ok); box-shadow: 0 0 8px rgb(76 194 107 / 55%); }
  .atitle { flex: 1; min-width: 0; display: grid; gap: 0.08rem; }
  .atitle b { font-size: 0.9rem; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .atitle small { font-size: 0.74rem; color: var(--mut); }
  .earn-badge {
    font-family: ui-monospace, monospace;
    font-size: 0.78rem;
    font-weight: 800;
    color: var(--mut);
    background: var(--bg);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 0.16rem 0.55rem;
  }
  .earn-badge.pos {
    color: var(--ok);
    border-color: rgb(76 194 107 / 35%);
    background: rgb(76 194 107 / 10%);
  }
  .tag-pill {
    font-family: ui-monospace, monospace;
    font-size: 0.66rem;
    font-weight: 800;
    color: var(--mut);
    background: var(--bg);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 0.14rem 0.48rem;
  }
  .tag-pill.ok { color: var(--ok); border-color: rgb(76 194 107 / 30%); }
  .offer-chips {
    display: flex;
    gap: 0.35rem;
    flex-wrap: wrap;
  }
  .ochip {
    display: inline-flex;
    align-items: center;
    gap: 0.3rem;
    background: var(--bg);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 0.18rem 0.55rem;
    color: var(--ink);
    font: inherit;
    font-size: 0.74rem;
    font-weight: 600;
    cursor: pointer;
  }
  .ochip:hover:not(.static) { border-color: var(--amber); }
  .ochip.static { cursor: default; color: var(--amber); }
  .ochip em {
    font-style: normal;
    font-family: ui-monospace, monospace;
    color: var(--amber);
    font-size: 0.7rem;
    font-weight: 700;
  }
  .anote {
    margin: 0;
    font-size: 0.78rem;
    line-height: 1.45;
    color: var(--mut);
  }
  .acard-foot {
    margin-top: auto;
    display: flex;
    gap: 0.4rem;
    font-size: 0.72rem;
    color: var(--mut2);
    font-family: ui-monospace, monospace;
  }
  .mini-dock {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.2rem 0.65rem; font: inherit; font-size: 0.72rem; font-weight: 800; cursor: pointer;
  }
  .mini-buy {
    background: transparent; color: var(--ok); border: 1px solid rgb(76 194 107 / 38%);
    border-radius: 999px; padding: 0.18rem 0.6rem; font: inherit; font-size: 0.72rem; font-weight: 700; cursor: pointer;
  }
  .mini-buy:hover { background: var(--ok); color: #08120a; }

  /* 2-Column Knowledge Brain */
  .brain-section {
    display: grid;
    grid-template-columns: 290px minmax(0, 1fr);
    flex: 1;
  }
  .shards-col {
    border-right: 1px solid var(--line);
    padding: 1rem 0.9rem 2rem;
    display: flex;
    flex-direction: column;
    gap: 0.42rem;
  }
  .srow, .srow-wrap {
    border: 1px solid transparent;
    border-radius: 11px;
    background: transparent;
    transition: background 0.12s, border-color 0.12s;
  }
  .srow {
    width: 100%;
    padding: 0.62rem 0.75rem;
    color: var(--ink);
    font: inherit;
    text-align: left;
    cursor: pointer;
    display: flex;
    flex-direction: column;
    gap: 0.18rem;
  }
  .srow:hover, .srow-wrap:hover { background: var(--hover); }
  .srow.on, .srow-wrap.on {
    background: var(--surf);
    border-color: var(--line2);
  }
  .srow-wrap {
    display: flex;
    align-items: center;
    gap: 0.35rem;
    padding-right: 0.45rem;
  }
  .srow-btn {
    flex: 1;
    min-width: 0;
    background: none;
    border: 0;
    padding: 0.62rem 0.75rem;
    color: var(--ink);
    font: inherit;
    text-align: left;
    cursor: pointer;
    display: flex;
    flex-direction: column;
    gap: 0.18rem;
  }
  .srow-top {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.5rem;
    font-size: 0.85rem;
  }
  .srow-top b.amber { color: var(--amber); }
  .srow-sub {
    display: flex;
    gap: 0.35rem;
    font-size: 0.72rem;
    color: var(--mut);
  }
  .srow-sub .ok { color: var(--ok); font-weight: 600; }
  .scnt { font-family: ui-monospace, monospace; font-size: 0.74rem; color: var(--mut); }
  .sprice { font-family: ui-monospace, monospace; font-size: 0.74rem; font-weight: 700; color: var(--amber); }
  .searn { font-family: ui-monospace, monospace; font-size: 0.74rem; font-weight: 800; color: var(--ok); }
  .spause {
    background: var(--bg);
    border: 1px solid var(--line);
    color: var(--mut);
    border-radius: 999px;
    padding: 0.16rem 0.52rem;
    font: inherit;
    font-size: 0.68rem;
    font-weight: 700;
    cursor: pointer;
  }
  .spause:hover { border-color: var(--amber); color: var(--amber); }

  /* Facts Pane */
  .facts-col {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }
  .remember-box {
    padding: 0.9rem 1.25rem;
    border-bottom: 1px solid var(--line);
    background: var(--surf);
    display: flex;
    flex-direction: column;
    gap: 0.55rem;
  }
  .remember-box textarea {
    width: 100%; background: transparent; border: 0; color: var(--ink);
    font: inherit; font-size: 0.92rem; line-height: 1.45; resize: none; outline: none; padding: 0;
  }
  .remember-box textarea::placeholder { color: var(--mut2); }
  .rbar { display: flex; align-items: center; justify-content: space-between; gap: 0.6rem; }
  .shard-in {
    display: inline-flex; align-items: center; gap: 0.25rem;
    background: var(--bg); border: 1px solid var(--line); border-radius: 999px;
    padding: 0.2rem 0.62rem; font-size: 0.76rem; color: var(--amber); font-weight: 700;
  }
  .shard-in input {
    width: 90px; background: none; border: 0; color: var(--ink);
    font: inherit; font-size: 0.76rem; font-weight: 700; outline: none;
  }
  .rbtn {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.4rem 1rem; font: inherit; font-size: 0.8rem; font-weight: 800; cursor: pointer;
  }
  .rbtn:disabled { opacity: 0.36; cursor: default; }
  .empty-facts { padding: 3rem 1.5rem; text-align: center; color: var(--mut); font-size: 0.88rem; }

  @media (max-width: 860px) {
    .brain-section { grid-template-columns: 1fr; }
    .shards-col { border-right: 0; border-bottom: 1px solid var(--line); }
  }
</style>
