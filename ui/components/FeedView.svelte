<script lang="ts">
  import type { DealFeedEntry, FeedFragment, Receipt, State } from '../lib/types'
  import Composer from './Composer.svelte'
  import FeedItem, { type TimelineEntry } from './FeedItem.svelte'

  interface Props {
    s: State
    searchQuery: string
    composerEl?: HTMLTextAreaElement | null
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
    onInspectMemory: (m: FeedFragment) => void
    onInspectReceipt: (r: Receipt) => void
    onInspectDeal: (d: DealFeedEntry) => void
    onForgetMemory: (shard: string, seq: number) => void
    onDecideApproval: (id: string, approve: boolean) => void
    onShareUrl: (url: string) => void
  }

  let {
    s,
    searchQuery,
    composerEl = $bindable(null),
    onRefresh,
    onToast,
    onInspectMemory,
    onInspectReceipt,
    onInspectDeal,
    onForgetMemory,
    onDecideApproval,
    onShareUrl
  }: Props = $props()

  let approvalsOnly = $state(false)

  const pendingApprovals = $derived(s.automation?.pending ?? [])
  const showingApprovalsOnly = $derived(approvalsOnly && pendingApprovals.length > 0)

  const entries = $derived.by(() => {
    const out: TimelineEntry[] = []
    for (const a of pendingApprovals) {
      out.push({ kind: 'approval', sortAt: Number.MAX_SAFE_INTEGER, data: a })
    }
    if (!showingApprovalsOnly) {
      for (const m of s.feed ?? []) {
        out.push({ kind: 'memory', sortAt: (m.createdAt || 0) * 1000, data: m })
      }
      for (const r of s.receipts.list ?? []) {
        out.push({ kind: 'receipt', sortAt: r.at ?? 0, data: r })
      }
      for (const d of s.automation?.deals ?? []) {
        out.push({ kind: 'deal', sortAt: d.at ?? 0, data: d })
      }
    }
    out.sort((a, b) => b.sortAt - a.sortAt)

    const q = searchQuery.trim().toLowerCase()
    if (!q) return out
    return out.filter((e) => {
      if (e.kind === 'memory') return `${e.data.shard} ${e.data.text}`.toLowerCase().includes(q)
      if (e.kind === 'receipt') return `${e.data.answer ?? ''} ${e.data.shards.join(' ')}`.toLowerCase().includes(q)
      if (e.kind === 'deal') return `${e.data.cause} ${e.data.to}`.toLowerCase().includes(q)
      return e.data.reason.toLowerCase().includes(q)
    })
  })
</script>

<div class="stream">
  <header class="sticky-head">
    <div class="bar">
      <h2 class="title">Activity</h2>
      {#if pendingApprovals.length > 0}
        <button
          class="appr-pill"
          class:on={showingApprovalsOnly}
          onclick={() => (approvalsOnly = !showingApprovalsOnly)}
        >
          {showingApprovalsOnly ? 'Show all activity' : 'Needs approval'}
          {#if !showingApprovalsOnly}
            <span class="cnt">{pendingApprovals.length}</span>
          {/if}
        </button>
      {/if}
    </div>
  </header>

  <Composer
    bind:inputEl={composerEl}
    feeRateSatPerKB={s.treasury.feeRateSatPerKB ?? 100}
    funded={s.treasury.funded}
    onDone={onRefresh}
    {onToast}
  />

  {#if entries.length === 0}
    <div class="empty-feed">
      {#if searchQuery.trim()}
        <p>Nothing matching “{searchQuery}”.</p>
      {:else}
        <p>No activity yet.</p>
        <small>Save a note above or ask your AI — everything it saves, answers, and trades appears here.</small>
      {/if}
    </div>
  {:else}
    {#each entries as entry}
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

<style>
  .stream { display: flex; flex-direction: column; }
  .sticky-head {
    position: sticky; top: 0; z-index: 15;
    background: rgb(9 8 7 / 90%); backdrop-filter: blur(12px);
    border-bottom: 1px solid var(--line);
  }
  .bar {
    display: flex; align-items: center; justify-content: space-between; gap: 0.75rem;
    padding: 0.85rem 1.25rem;
  }
  .title { margin: 0; font-size: 1rem; font-weight: 800; color: var(--ink); letter-spacing: -0.01em; }
  .appr-pill {
    display: inline-flex; align-items: center; gap: 0.35rem;
    background: rgb(247 153 27 / 12%); color: var(--amber);
    border: 1px solid rgb(247 153 27 / 35%); border-radius: 999px;
    font: inherit; font-size: 0.78rem; font-weight: 700;
    padding: 0.32rem 0.75rem; cursor: pointer; transition: background 0.14s, color 0.14s;
  }
  .appr-pill:hover { background: rgb(247 153 27 / 20%); }
  .appr-pill.on { background: var(--amber); color: #120e0c; border-color: var(--amber); }
  .cnt {
    background: var(--amber); color: #120e0c; font-size: 0.68rem; font-weight: 800;
    border-radius: 999px; padding: 0.05rem 0.4rem;
  }
  .empty-feed { padding: 3.5rem 1.5rem; text-align: center; color: var(--mut); }
  .empty-feed p { margin: 0 0 0.35rem; font-size: 0.95rem; color: var(--ink); }
  .empty-feed small { font-size: 0.82rem; color: var(--mut); }
</style>
