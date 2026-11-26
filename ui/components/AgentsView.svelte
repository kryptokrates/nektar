<script lang="ts">
  import type { DealFeedEntry, DockClient, FeedFragment, PeerInfo, PulseSignal, Receipt, State } from '../lib/types'
  import { operatorFetch, postJSON } from '../lib/api'
  import { agoMs, agoUnix, fmtCompactSats, fmtSats } from '../lib/format'
  import HoneycombStage, { type StageBar, type StageStatItem } from './HoneycombStage.svelte'

  interface HiveAgent {
    id: string
    name: string
    status: string
    online: boolean
    balanceSats: number
    earned: number
    spent: number
    capNote?: string
  }

  interface HiveTopic {
    shard: string
    label: string
    count: number
    earned: number
    listed: boolean
    active: boolean
    price: string
  }

  type VaultRow =
    | { id: string; kind: 'memory'; sortAt: number; dot: 'ok' | 'muted'; text: string; topic: string; sats?: string; satsTone?: 'ok' | 'amber'; time: string; raw: FeedFragment }
    | { id: string; kind: 'receipt'; sortAt: number; dot: 'ok' | 'warn' | 'bad'; text: string; topic: string; sats?: string; satsTone?: 'ok' | 'amber'; time: string; raw: Receipt }
    | { id: string; kind: 'deal'; sortAt: number; dot: 'ok' | 'warn' | 'bad'; text: string; topic: string; sats?: string; satsTone?: 'ok' | 'amber'; time: string; raw: DealFeedEntry }

  interface Props {
    s: State
    live: boolean
    pulse: PulseSignal | null
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
    pulse,
    dockedApps,
    searchQuery,
    onOpenDock,
    onRefresh,
    onToast,
    onInspectMemory,
    onInspectReceipt,
    onInspectDeal,
    onDecideApproval
  }: Props = $props()

  let stageRef: {
    launchComet: (side: 'left' | 'right', rowIdx?: number, onArrive?: () => void) => void
    triggerShockwave: (label: string) => void
    shareSnapshot: (filename?: string) => Promise<void>
  } | null = null

  let selectedAgentId = $state<string>('node')
  let selectedTopic = $state<string>('__all__')
  let addOpen = $state(false)
  let noteText = $state('')
  let busy = $state(false)
  let addInputEl = $state<HTMLInputElement | null>(null)

  // Real financial summary
  const balanceSats = $derived(s.treasury.sats ?? 0)
  const settledDeals = $derived((s.automation?.deals ?? []).filter((d) => d.to === 'settled'))
  const totalEarned = $derived.by(() => {
    const dealSum = settledDeals.filter((d) => d.role === 'seller').reduce((acc, d) => acc + (d.priceSats ?? 0), 0)
    return Math.max(s.receipts.earned ?? 0, dealSum)
  })
  const totalSpent = $derived.by(() => {
    const dealSum = settledDeals.filter((d) => d.role === 'buyer').reduce((acc, d) => acc + (d.priceSats ?? 0), 0)
    return Math.max(s.treasury.spent ?? 0, dealSum)
  })
  const netSats = $derived(totalEarned - totalSpent)
  const vault = $derived(s.treasury.vault ?? null)
  const pendingApprovals = $derived(s.automation?.pending ?? [])

  // Only YOUR agents
  const dockedList = $derived(dockedApps.filter((c) => c.docked))
  const detectedUndockedCount = $derived(dockedApps.filter((c) => c.detected && !c.docked).length)
  const liveListings = $derived((s.market ?? []).filter((m) => m.active !== false))

  function prettyTopic(raw: string): string {
    const clean = raw.replace(/^cluster:/, '').replace(/^#/, '').trim()
    if (!clean) return 'Notes'
    return clean
      .split(/[-_]+/)
      .map((w) => (w ? w[0].toUpperCase() + w.slice(1) : ''))
      .join(' ')
  }

  const topics = $derived.by<HiveTopic[]>(() => {
    const map = new Map<string, HiveTopic>()
    for (const m of s.market ?? []) {
      map.set(m.shard, {
        shard: m.shard,
        label: prettyTopic(m.shard),
        count: m.records,
        listed: true,
        active: m.active !== false,
        price: m.offers,
        earned: m.earned
      })
    }
    const counts = new Map<string, number>()
    for (const f of s.feed ?? []) {
      counts.set(f.shard, (counts.get(f.shard) ?? 0) + 1)
      if (!map.has(f.shard)) {
        map.set(f.shard, {
          shard: f.shard,
          label: prettyTopic(f.shard),
          count: 1,
          listed: false,
          active: false,
          price: '',
          earned: 0
        })
      }
    }
    for (const [sh, cnt] of counts) {
      const g = map.get(sh)
      if (g && !g.listed) g.count = cnt
    }
    return Array.from(map.values()).sort((a, b) => {
      if (a.listed !== b.listed) return a.listed ? -1 : 1
      if (a.earned !== b.earned) return b.earned - a.earned
      return b.count - a.count
    })
  })

  const activeTopicMeta = $derived(topics.find((t) => t.shard === selectedTopic) ?? null)

  const myAgents = $derived.by<HiveAgent[]>(() => {
    const list: HiveAgent[] = [
      {
        id: 'node',
        name: 'Nektar Node',
        status: liveListings.length > 0 ? 'Trading' : 'Ready',
        online: live,
        balanceSats,
        earned: totalEarned,
        spent: totalSpent,
        capNote: s.treasury.dailyCapSats ? `Cap ${fmtCompactSats(s.treasury.dailyCapSats)}s/d` : undefined
      }
    ]
    if (vault) {
      list.push({
        id: 'vault',
        name: 'Agent Vault',
        status: vault.paused ? 'Paused' : 'Active',
        online: !vault.paused,
        balanceSats: vault.sats,
        earned: 0,
        spent: totalSpent,
        capNote: vault.maxPerSpendSats ? `Max ${fmtCompactSats(vault.maxPerSpendSats)}s/tx` : undefined
      })
    }
    for (const c of dockedList) {
      list.push({
        id: c.id,
        name: c.label,
        status: 'Live',
        online: true,
        balanceSats: vault ? vault.sats : balanceSats,
        earned: totalEarned,
        spent: totalSpent
      })
    }
    return list
  })

  // Shared HoneycombStage data mapping
  const hasFlow = $derived(totalEarned > 0 || totalSpent > 0)
  const earnedShare = $derived(hasFlow ? totalEarned / (totalEarned + totalSpent) : 0.5)
  const maxTopicScore = $derived(
    Math.max(...topics.slice(0, 5).map((t) => Math.max(t.count, t.earned > 0 ? t.count + 2 : t.count)), 1)
  )

  const agentStageBars = $derived<StageBar[]>(
    myAgents.slice(0, 5).map((a) => ({
      id: a.id,
      label: a.name,
      badge: `${fmtCompactSats(a.balanceSats)}s`,
      badgeColor: '#4cc26b',
      width: 224,
      dotColor: a.online ? '#4cc26b' : '#7d746b',
      hoverText: `${a.name} · ${fmtSats(a.balanceSats)} sats · +${fmtCompactSats(a.earned)}s / -${fmtCompactSats(a.spent)}s`,
      highlighted: selectedAgentId === a.id,
      onClick: () => (selectedAgentId = a.id)
    }))
  )

  const topicStageBars = $derived<StageBar[]>(
    topics.slice(0, 5).map((t) => {
      const score = Math.max(t.count, t.earned > 0 ? t.count + 2 : t.count)
      return {
        id: t.shard,
        label: t.listed ? `${t.label.slice(0, 14)} ★` : t.label,
        badge: t.earned > 0 ? `+${fmtCompactSats(t.earned)}s` : `${t.count}`,
        badgeColor: t.earned > 0 ? '#4cc26b' : '#f7991b',
        width: 132 + Math.round((score / maxTopicScore) * 110),
        hoverText: `${t.label} · ${t.count} ${t.count === 1 ? 'item' : 'items'}${t.earned > 0 ? ` · +${fmtSats(t.earned)}s earned` : t.listed ? ` · ${t.price}` : ''} · click to filter`,
        highlighted: selectedTopic === t.shard,
        dimmed: selectedTopic !== '__all__' && selectedTopic !== t.shard,
        onClick: () => (selectedTopic = selectedTopic === t.shard ? '__all__' : t.shard)
      }
    })
  )

  const stageFooterStats = $derived<StageStatItem[]>([
    { label: 'EARNED', value: `+${fmtCompactSats(totalEarned)}s`, color: '#4cc26b' },
    { label: 'SPENT', value: `-${fmtCompactSats(totalSpent)}s`, color: '#f7991b' },
    {
      label: 'NET',
      value: `${netSats >= 0 ? '+' : ''}${fmtCompactSats(netSats)}s`,
      color: netSats > 0 ? '#4cc26b' : netSats < 0 ? '#e5534b' : '#f1ede9'
    }
  ])

  let lastPulseSeq = 0
  $effect(() => {
    if (!pulse || pulse.seq === lastPulseSeq) return
    lastPulseSeq = pulse.seq
    const ev = pulse.event
    const sats = pulse.tape?.sats ?? 0
    if (ev === 'deal_settled' || ev === 'receipt') {
      stageRef?.launchComet('left', 0, () => {
        stageRef?.triggerShockwave(sats > 0 ? `+${fmtCompactSats(sats)} sats` : 'Verified ✓')
        stageRef?.launchComet('right', 0)
      })
    } else if (ev === 'remember' || ev === 'anchor' || ev === 'tool:sell') {
      stageRef?.launchComet('left', 0, () => stageRef?.launchComet('right', 0))
    } else if (ev === 'market_want' || ev === 'tool:want') {
      stageRef?.launchComet('left', 0)
    }
  })

  // Ultra-clean unified list of what your agents hold & traded
  const rows = $derived.by<VaultRow[]>(() => {
    const out: VaultRow[] = []
    for (const m of s.feed ?? []) {
      if (selectedTopic !== '__all__' && m.shard !== selectedTopic) continue
      out.push({
        id: `m:${m.shard}:${m.seq}`,
        kind: 'memory',
        sortAt: (m.createdAt || 0) * 1000,
        dot: m.batchN >= 0 ? 'ok' : 'muted',
        text: m.text,
        topic: prettyTopic(m.shard),
        time: agoUnix(m.createdAt),
        raw: m
      })
    }
    for (const r of s.receipts.list ?? []) {
      if (selectedTopic !== '__all__' && !r.shards.includes(selectedTopic)) continue
      const verdict = r.verdict ?? (r.anchored ? 'green' : 'amber')
      out.push({
        id: `r:${r.seq}:${r.at ?? 0}`,
        kind: 'receipt',
        sortAt: r.at ?? 0,
        dot: verdict === 'green' ? 'ok' : verdict === 'red' ? 'bad' : 'warn',
        text: r.answer || 'Verified answer',
        topic: r.shards[0] ? prettyTopic(r.shards[0]) : 'Answer',
        sats: r.sats && r.sats > 0 ? `+${fmtCompactSats(r.sats)}s` : undefined,
        satsTone: 'ok',
        time: agoMs(r.at),
        raw: r
      })
    }
    for (const d of s.automation?.deals ?? []) {
      if (selectedTopic !== '__all__' && d.shard !== selectedTopic) continue
      const isSell = d.role === 'seller'
      out.push({
        id: `d:${d.op}:${d.at}`,
        kind: 'deal',
        sortAt: d.at ?? 0,
        dot: d.to === 'settled' ? 'ok' : d.to === 'failed' ? 'bad' : 'warn',
        text: d.deliveredText || d.cause,
        topic: d.shard ? prettyTopic(d.shard) : isSell ? 'Sale' : 'Purchase',
        sats: d.priceSats ? `${isSell ? '+' : '-'}${fmtCompactSats(d.priceSats)}s` : undefined,
        satsTone: isSell ? 'ok' : 'amber',
        time: agoMs(d.at),
        raw: d
      })
    }
    out.sort((a, b) => b.sortAt - a.sortAt)

    const q = searchQuery.trim().toLowerCase()
    if (!q) return out
    return out.filter((r) => `${r.text} ${r.topic}`.toLowerCase().includes(q))
  })

  function openRow(r: VaultRow) {
    if (r.kind === 'memory') onInspectMemory(r.raw)
    else if (r.kind === 'receipt') onInspectReceipt(r.raw)
    else onInspectDeal(r.raw)
  }

  function toggleAdd() {
    addOpen = !addOpen
    if (addOpen) setTimeout(() => addInputEl?.focus(), 20)
  }

  async function saveMemory() {
    const body = noteText.trim()
    if (!body || busy) return
    busy = true
    try {
      let targetShard = selectedTopic !== '__all__' ? selectedTopic : 'notes'
      if (selectedTopic === '__all__') {
        try {
          const cl = await fetch(`/v1/topics/cluster?hint=${encodeURIComponent(body.slice(0, 180))}`)
          if (cl.ok) {
            const j = (await cl.json()) as { cluster?: string }
            if (j.cluster && j.cluster !== 'general') targetShard = j.cluster
          }
        } catch {}
      }
      const res = await postJSON<{ anchorTxid?: string }>('/v1/remember', {
        shard: targetShard,
        text: body,
        autoAnchor: s.treasury.funded
      })
      if (!res.ok) {
        onToast(res.data.error ?? `Save failed (${res.status})`, true)
        return
      }
      noteText = ''
      addOpen = false
      onToast(res.data.anchorTxid ? `Saved to ${prettyTopic(targetShard)} ✓` : 'Saved ✓')
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
      onToast(active ? `Resumed ${prettyTopic(shard)}` : `Paused ${prettyTopic(shard)}`)
      await onRefresh()
    }
  }
</script>

<div class="agents-page">
  <!-- Pixel-Locked 54px Topbar (Zero Shift vs Market) -->
  <header class="topbar">
    <div class="tleft">
      <h1>Agents</h1>
      {#if s.treasury.dailyCapSats}
        <span class="cap-pill">Cap {fmtCompactSats(s.treasury.dailyCapSats)}s/d</span>
      {/if}
    </div>
    <div class="tright">
      <button class="share-btn" onclick={() => stageRef?.shareSnapshot('nektar-agents.png')} title="Copy crisp PNG card for X / Twitter">
        Share Card ↗
      </button>
      <button class="connect-btn" onclick={onOpenDock}>
        + Connect AI
        {#if detectedUndockedCount > 0}
          <span class="found-cnt">{detectedUndockedCount}</span>
        {/if}
      </button>
    </div>
  </header>

  <!-- ZONE 1: SHARED HONEYCOMB STAGE (Single Source of Truth for Agents, Money & Owned Topics) -->
  <HoneycombStage
    bind:this={stageRef}
    ariaLabel="Your AI Agents Economy"
    leftTitle={`YOUR AGENTS · ${myAgents.length}`}
    rightTitle="WHAT THEY OWN"
    leftBars={agentStageBars}
    rightBars={topicStageBars}
    rightEmptyText="No knowledge saved yet"
    flowMode="through"
    coreValue={fmtCompactSats(balanceSats)}
    coreLabel="SATS BALANCE"
    ringLeftShare={earnedShare}
    ringMuted={!hasFlow}
    {live}
    footerStats={stageFooterStats}
    {onToast}
  />

  <!-- Urgent Pending Approvals (only rendered if an action genuinely waits on you) -->
  {#if pendingApprovals.length > 0}
    <section class="approvals" aria-label="Needs approval">
      {#each pendingApprovals as a (a.id)}
        <div class="appr-row">
          <span class="appr-dot">!</span>
          <span class="appr-txt">{a.reason} · ≤ {fmtCompactSats(a.maxExtraSats)}s</span>
          <div class="appr-acts">
            <button class="btn-ok" onclick={() => onDecideApproval(a.id, true)}>Approve</button>
            <button class="btn-ghost" onclick={() => onDecideApproval(a.id, false)}>Deny</button>
          </div>
        </div>
      {/each}
    </section>
  {/if}

  <!-- ZONE 2: ULTRA-CLEAN KNOWLEDGE & TRADES LIST (Zero Redundant Cards or Chrome) -->
  <section class="vault">
    <div class="vault-head">
      <div class="vh-left">
        <span class="vh-title">Knowledge</span>
        <span class="vh-cnt">{rows.length}</span>
        {#if activeTopicMeta}
          <button class="filter-chip" onclick={() => (selectedTopic = '__all__')} title="Clear topic filter">
            {activeTopicMeta.label} <span>×</span>
          </button>
          {#if activeTopicMeta.listed}
            <button
              class="list-toggle"
              onclick={() => toggleListing(activeTopicMeta.shard, !activeTopicMeta.active)}
            >
              {activeTopicMeta.active ? 'Pause selling' : 'Resume selling'}
            </button>
          {/if}
        {/if}
      </div>

      <button class="add-btn" class:open={addOpen} onclick={toggleAdd}>
        {addOpen ? 'Cancel' : '+ Add'}
      </button>
    </div>

    {#if addOpen}
      <div class="add-row">
        <input
          bind:this={addInputEl}
          bind:value={noteText}
          placeholder="Add a fact or note for your agents…"
          disabled={busy}
          onkeydown={(e) => {
            if (e.key === 'Enter' && !e.shiftKey) {
              e.preventDefault()
              void saveMemory()
            } else if (e.key === 'Escape') {
              addOpen = false
            }
          }}
        />
        <button class="save-btn" onclick={saveMemory} disabled={busy || !noteText.trim()}>
          {busy ? 'Saving…' : 'Save'}
        </button>
      </div>
    {/if}

    {#if rows.length === 0}
      <div class="empty">Nothing here yet.</div>
    {:else}
      <div class="list">
        {#each rows as row (row.id)}
          <button class="row" onclick={() => openRow(row)}>
            <span class="rdot" class:ok={row.dot === 'ok'} class:warn={row.dot === 'warn'} class:bad={row.dot === 'bad'}></span>
            <span class="rtxt">{row.text}</span>
            <span class="rmeta">
              {#if row.sats}
                <b class="rsats" class:ok={row.satsTone === 'ok'} class:amber={row.satsTone === 'amber'}>{row.sats}</b>
              {/if}
              <span class="rtopic">{row.topic}</span>
              <span class="rtime">{row.time}</span>
            </span>
          </button>
        {/each}
      </div>
    {/if}
  </section>
</div>

<style>
  .agents-page {
    display: flex;
    flex-direction: column;
    min-height: 100vh;
  }

  /* Pixel-locked 54px Sticky Header matching MarketHome */
  .topbar {
    position: sticky;
    top: 0;
    z-index: 20;
    height: 54px;
    box-sizing: border-box;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    padding: 0 1.25rem;
    background: rgb(9 8 7 / 92%);
    backdrop-filter: blur(12px);
    border-bottom: 1px solid var(--line);
  }
  .tleft { display: flex; align-items: center; gap: 0.65rem; }
  h1 { margin: 0; font-size: 1.06rem; font-weight: 800; letter-spacing: -0.02em; }
  .cap-pill {
    font-size: 0.72rem; font-weight: 700; color: var(--amber);
    background: rgb(247 153 27 / 10%); border: 1px solid rgb(247 153 27 / 26%);
    border-radius: 999px; padding: 0.12rem 0.55rem;
  }
  .tright { display: flex; align-items: center; gap: 0.5rem; }
  .share-btn {
    background: var(--surf); color: var(--ink); border: 1px solid var(--line2);
    border-radius: 999px; padding: 0.36rem 0.85rem; font: inherit;
    font-size: 0.78rem; font-weight: 700; cursor: pointer;
    transition: border-color 0.14s, color 0.14s;
  }
  .share-btn:hover { border-color: var(--amber); color: var(--amber); }
  .connect-btn {
    display: inline-flex; align-items: center; gap: 0.42rem;
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.36rem 0.9rem; font: inherit; font-size: 0.78rem; font-weight: 800;
    cursor: pointer; transition: filter 0.14s;
  }
  .connect-btn:hover { filter: brightness(1.08); }
  .found-cnt {
    background: rgb(18 14 12 / 20%); color: #120e0c;
    font-size: 0.68rem; font-weight: 800; border-radius: 999px;
    padding: 0.04rem 0.42rem; font-variant-numeric: tabular-nums;
  }

  /* Urgent Pending Approvals */
  .approvals {
    background: rgb(247 153 27 / 7%);
    border-bottom: 1px solid rgb(247 153 27 / 26%);
    padding: 0.55rem 1.25rem;
    display: flex; flex-direction: column; gap: 0.45rem;
  }
  .appr-row { display: flex; align-items: center; gap: 0.65rem; font-size: 0.82rem; }
  .appr-dot {
    width: 18px; height: 18px; border-radius: 50%; background: var(--amber); color: #120e0c;
    display: grid; place-items: center; font-size: 0.72rem; font-weight: 900; flex-shrink: 0;
  }
  .appr-txt { flex: 1; color: var(--ink); min-width: 0; }
  .appr-acts { display: flex; gap: 0.4rem; }
  .btn-ok {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.28rem 0.78rem; font: inherit; font-size: 0.75rem; font-weight: 800; cursor: pointer;
  }
  .btn-ghost {
    background: transparent; color: var(--mut); border: 1px solid var(--line2);
    border-radius: 999px; padding: 0.28rem 0.72rem; font: inherit; font-size: 0.75rem; cursor: pointer;
  }

  /* ZONE 2: Whisper-Quiet Knowledge & Trades List */
  .vault {
    display: flex;
    flex-direction: column;
    flex: 1;
  }
  .vault-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.75rem;
    padding: 0.75rem 1.25rem;
    border-bottom: 1px solid var(--line);
  }
  .vh-left { display: flex; align-items: center; gap: 0.55rem; flex-wrap: wrap; }
  .vh-title {
    font-size: 0.72rem; font-weight: 800; letter-spacing: 0.09em;
    text-transform: uppercase; color: var(--mut2);
  }
  .vh-cnt {
    font-family: ui-monospace, monospace; font-size: 0.74rem; color: var(--mut);
    font-variant-numeric: tabular-nums;
  }
  .filter-chip {
    display: inline-flex; align-items: center; gap: 0.32rem;
    background: rgb(247 153 27 / 12%); color: var(--amber);
    border: 1px solid rgb(247 153 27 / 32%); border-radius: 999px;
    padding: 0.14rem 0.58rem; font: inherit; font-size: 0.74rem; font-weight: 700; cursor: pointer;
  }
  .filter-chip span { opacity: 0.75; }
  .list-toggle {
    background: none; border: 1px solid var(--line); border-radius: 999px;
    color: var(--mut); padding: 0.14rem 0.58rem; font: inherit; font-size: 0.72rem;
    font-weight: 600; cursor: pointer;
  }
  .list-toggle:hover { border-color: var(--amber); color: var(--amber); }

  .add-btn {
    background: none; border: 1px solid var(--line); border-radius: 999px;
    color: var(--mut); padding: 0.24rem 0.72rem; font: inherit; font-size: 0.75rem;
    font-weight: 700; cursor: pointer; transition: border-color 0.14s, color 0.14s;
  }
  .add-btn:hover, .add-btn.open { border-color: var(--line2); color: var(--ink); }

  .add-row {
    display: flex; align-items: center; gap: 0.65rem;
    padding: 0.65rem 1.25rem;
    background: var(--surf);
    border-bottom: 1px solid var(--line);
  }
  .add-row input {
    flex: 1; min-width: 0; background: none; border: 0; color: var(--ink);
    font: inherit; font-size: 0.88rem; outline: none;
  }
  .add-row input::placeholder { color: var(--mut2); }
  .save-btn {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.3rem 0.85rem; font: inherit; font-size: 0.76rem; font-weight: 800; cursor: pointer;
  }
  .save-btn:disabled { opacity: 0.38; cursor: default; }

  .list { display: flex; flex-direction: column; }
  .row {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 0.75rem;
    padding: 0.78rem 1.25rem;
    background: none;
    border: 0;
    border-bottom: 1px solid var(--line);
    color: var(--ink);
    font: inherit;
    text-align: left;
    cursor: pointer;
    transition: background 0.12s;
  }
  .row:hover { background: var(--hover); }
  .rdot {
    width: 6.5px; height: 6.5px; border-radius: 50%;
    background: var(--mut2); flex-shrink: 0;
  }
  .rdot.ok { background: var(--ok); }
  .rdot.warn { background: var(--amber); }
  .rdot.bad { background: var(--bad); }
  .rtxt {
    flex: 1; min-width: 0;
    font-size: 0.88rem; color: var(--ink);
    white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
  }
  .rmeta {
    display: flex; align-items: center; gap: 0.65rem; flex-shrink: 0;
  }
  .rsats {
    font-family: ui-monospace, monospace; font-size: 0.78rem; font-weight: 800;
    font-variant-numeric: tabular-nums;
  }
  .rsats.ok { color: var(--ok); }
  .rsats.amber { color: var(--amber); }
  .rtopic {
    font-size: 0.74rem; font-weight: 600; color: var(--mut);
  }
  .rtime {
    font-family: ui-monospace, monospace; font-size: 0.72rem; color: var(--mut2);
    font-variant-numeric: tabular-nums; min-width: 26px; text-align: right;
  }
  .empty {
    padding: 3rem 1.25rem;
    text-align: center;
    color: var(--mut2);
    font-size: 0.86rem;
  }
</style>
