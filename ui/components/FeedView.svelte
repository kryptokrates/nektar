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

  type Filter = 'all' | 'memories' | 'receipts' | 'trades' | 'approvals'
  let filter = $state<Filter>('all')

  const pendingApprovals = $derived(s.automation?.pending ?? [])

  const entries = $derived.by(() => {
    const out: TimelineEntry[] = []
    if (filter === 'all' || filter === 'approvals') {
      for (const a of pendingApprovals) {
        out.push({ kind: 'approval', sortAt: Number.MAX_SAFE_INTEGER, data: a })
      }
    }
    if (filter === 'all' || filter === 'memories') {
      for (const m of s.feed ?? []) {
        out.push({ kind: 'memory', sortAt: (m.createdAt || 0) * 1000, data: m })
      }
    }
    if (filter === 'all' || filter === 'receipts') {
      for (const r of s.receipts.list ?? []) {
        out.push({ kind: 'receipt', sortAt: r.at ?? 0, data: r })
      }
    }
    if (filter === 'all' || filter === 'trades') {
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
    <div class="tabs">
      <button class="t" class:on={filter === 'all'} onclick={() => (filter = 'all')}>All</button>
      <button class="t" class:on={filter === 'memories'} onclick={() => (filter = 'memories')}>Memories</button>
      <button class="t" class:on={filter === 'receipts'} onclick={() => (filter = 'receipts')}>Receipts</button>
      <button class="t" class:on={filter === 'trades'} onclick={() => (filter = 'trades')}>Trades</button>
      {#if pendingApprovals.length > 0}
        <button class="t warn" class:on={filter === 'approvals'} onclick={() => (filter = 'approvals')}>
          Approvals <span class="cnt">{pendingApprovals.length}</span>
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
        <p>Your timeline is quiet.</p>
        <small>Post a memory above — Nektar anchors and verifies it automatically.</small>
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
  .tabs { display: flex; padding: 0 0.5rem; }
  .t {
    flex: 1; position: relative; background: none; border: 0; color: var(--mut);
    font: inherit; font-size: 0.86rem; font-weight: 600; padding: 0.9rem 0.5rem;
    cursor: pointer; white-space: nowrap; transition: color 0.14s, background 0.14s;
  }
  .t:hover { color: var(--ink); background: var(--hover); }
  .t.on { color: var(--ink); font-weight: 700; }
  .t.on::after {
    content: ''; position: absolute; left: 26%; right: 26%; bottom: 0;
    height: 2.5px; border-radius: 99px; background: var(--amber);
  }
  .t.warn { color: var(--amber); }
  .cnt {
    background: var(--amber); color: #120e0c; font-size: 0.68rem; font-weight: 800;
    border-radius: 999px; padding: 0.05rem 0.4rem; margin-left: 0.25rem;
  }
  .empty-feed { padding: 3.5rem 1.5rem; text-align: center; color: var(--mut); }
  .empty-feed p { margin: 0 0 0.35rem; font-size: 0.95rem; color: var(--ink); }
  .empty-feed small { font-size: 0.82rem; color: var(--mut); }
</style>
