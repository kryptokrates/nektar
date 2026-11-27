<script lang="ts">
  import type { DealFeedEntry, DockClient, FeedFragment, PeerInfo, PulseSignal, Receipt, RepoSummary, State } from '../lib/types'
  import { operatorFetch } from '../lib/api'
  import { fmtCompactSats } from '../lib/format'
  import { selectCoreRepVector } from '../lib/reputationHex'
  import HoneycombStage from './HoneycombStage.svelte'
  import RepoCommitDrawer from './RepoCommitDrawer.svelte'
  import AgentsApprovalsBanner from './AgentsApprovalsBanner.svelte'
  import AgentsVaultSection from './AgentsVaultSection.svelte'
  import {
    buildAgentStageBars,
    buildStageFooterStats,
    buildTopicStageBars,
    computeAgentsFinancials,
    deriveHiveAgents,
    deriveHiveTopics,
    deriveVaultRows,
    prettyTopic,
    type VaultRow
  } from './AgentsModel'

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
    onInspectCapsuleFile: (file: File) => void
    openRepoRequest?: { repo: string; seq: number } | null
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
    onDecideApproval,
    onInspectCapsuleFile,
    openRepoRequest
  }: Props = $props()

  let stageRef = $state<{
    launchComet: (side: 'left' | 'right', rowIdx?: number, onArrive?: () => void) => void
    triggerShockwave: (label: string) => void
    shareSnapshot: (filename?: string) => Promise<void>
  } | null>(null)

  let selectedAgentId = $state<string>('node')
  let selectedTopic = $state<string>('__all__')
  let inspectRepo = $state<string | null>(null)
  let lastOpenRepoSeq = 0

  $effect(() => {
    if (openRepoRequest && openRepoRequest.seq !== lastOpenRepoSeq) {
      lastOpenRepoSeq = openRepoRequest.seq
      inspectRepo = openRepoRequest.repo
    }
  })

  const financials = $derived(computeAgentsFinancials(s))
  const pendingApprovals = $derived(s.automation?.pending ?? [])
  const dockedList = $derived(dockedApps.filter((c) => c.docked))
  const detectedUndockedCount = $derived(dockedApps.filter((c) => c.detected && !c.docked).length)

  const topics = $derived(deriveHiveTopics(s))
  const activeTopicMeta = $derived(topics.find((t) => t.shard === selectedTopic) ?? null)
  const myAgents = $derived(deriveHiveAgents(s, live, dockedList, financials))

  const repoMap = $derived.by(() => {
    const m = new Map<string, RepoSummary>()
    for (const r of s.repos ?? []) m.set(r.repo, r)
    return m
  })
  const activeRepoSummary = $derived<RepoSummary | null>(
    selectedTopic !== '__all__' ? (repoMap.get(selectedTopic) ?? null) : null
  )
  const firstRepoName = $derived((s.repos ?? [])[0]?.repo ?? topics[0]?.shard ?? null)

  const hasFlow = $derived(financials.totalEarned > 0 || financials.totalSpent > 0)
  const earnedShare = $derived(
    hasFlow ? financials.totalEarned / (financials.totalEarned + financials.totalSpent) : 0.5
  )

  const agentStageBars = $derived(
    buildAgentStageBars(myAgents, selectedAgentId, (id) => (selectedAgentId = id))
  )
  const topicStageBars = $derived(
    buildTopicStageBars(topics, selectedTopic, (shard) => {
      selectedTopic = selectedTopic === shard ? '__all__' : shard
    })
  )
  const stageFooterStats = $derived(buildStageFooterStats(financials))
  const coreRep = $derived(selectCoreRepVector(s.reputation, s.pub))
  const rows = $derived(deriveVaultRows(s, selectedTopic, searchQuery))

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

  function openRow(r: VaultRow) {
    if (r.kind === 'memory') onInspectMemory(r.raw)
    else if (r.kind === 'receipt') onInspectReceipt(r.raw)
    else onInspectDeal(r.raw)
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

  <!-- ZONE 1: SHARED HONEYCOMB STAGE (Single Source of Truth for Agents, Money & Memory Repos) -->
  <HoneycombStage
    bind:this={stageRef}
    ariaLabel="Your AI Agents Economy"
    leftTitle={`YOUR AGENTS · ${myAgents.length}`}
    rightTitle="MEMORY REPOS"
    leftBars={agentStageBars}
    rightBars={topicStageBars}
    rightEmptyText="No memory repos yet"
    flowMode="through"
    coreValue={fmtCompactSats(financials.balanceSats)}
    coreLabel="SATS BALANCE"
    ringLeftShare={earnedShare}
    ringMuted={!hasFlow}
    {live}
    footerStats={stageFooterStats}
    repVector={coreRep.vector}
    repTrusted={coreRep.trusted}
    repUntrusted={coreRep.untrusted}
    {onToast}
  />

  <!-- Urgent Pending Approvals (only rendered if an action genuinely waits on you) -->
  <AgentsApprovalsBanner approvals={pendingApprovals} {onDecideApproval} />

  <!-- ZONE 2: ULTRA-CLEAN MEMORY REPOS & COMMITS LIST -->
  <AgentsVaultSection
    {rows}
    {selectedTopic}
    {activeTopicMeta}
    {activeRepoSummary}
    {firstRepoName}
    treasuryFunded={s.treasury.funded}
    onClearTopic={() => (selectedTopic = '__all__')}
    onToggleListing={toggleListing}
    onInspectRepo={(repo) => (inspectRepo = repo)}
    {onInspectCapsuleFile}
    onOpenRow={openRow}
    {onRefresh}
    {onToast}
  />

  <RepoCommitDrawer
    repo={inspectRepo}
    onClose={() => (inspectRepo = null)}
    onSelectRepo={(r) => (inspectRepo = r)}
    {onToast}
    {onRefresh}
  />
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
</style>
