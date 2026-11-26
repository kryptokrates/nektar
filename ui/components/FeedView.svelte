<script lang="ts">
  import type { DealFeedEntry, FeedFragment, Receipt, State } from '../lib/types'
  import { postJSON } from '../lib/api'
  import FeedItem, { type TimelineEntry } from './FeedItem.svelte'

  interface Props {
    s: State
    searchQuery: string
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
    onRefresh,
    onToast,
    onInspectMemory,
    onInspectReceipt,
    onInspectDeal,
    onForgetMemory,
    onDecideApproval,
    onShareUrl
  }: Props = $props()

  let noteText = $state('')
  let noteShard = $state('notes')
  let busy = $state(false)

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

  const entries = $derived.by(() => {
    const out: TimelineEntry[] = []
    for (const a of s.automation?.pending ?? []) {
      out.push({ kind: 'approval', sortAt: Number.MAX_SAFE_INTEGER, data: a })
    }
    for (const m of s.feed ?? []) {
      out.push({ kind: 'memory', sortAt: (m.createdAt || 0) * 1000, data: m })
    }
    for (const r of s.receipts.list ?? []) {
      out.push({ kind: 'receipt', sortAt: r.at ?? 0, data: r })
    }
    for (const d of s.automation?.deals ?? []) {
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
</script>

<div class="stream">
  <header class="sticky-head">
    <div class="bar">
      <h2 class="title">Memory Vault</h2>
      <span class="meta">{s.memory.anchored} / {s.memory.records} anchored on BSV</span>
    </div>
  </header>

  <!-- Single-purpose private memory input (Buy & Sell live tablessly on Market Home) -->
  <section class="remember-box">
    <textarea
      bind:value={noteText}
      rows="2"
      placeholder="Remember a private fact or note to anchor on BSV (not listed for sale)…"
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
        {busy ? 'Saving…' : s.treasury.funded ? 'Remember & Anchor' : 'Remember'}
      </button>
    </div>
  </section>

  {#if entries.length === 0}
    <div class="empty-feed">
      {#if searchQuery.trim()}
        <p>Nothing matching “{searchQuery}”.</p>
      {:else}
        <p>No memories or receipts yet.</p>
        <small>Save a private fact above, or buy &amp; sell knowledge on the Market Home.</small>
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
  .title { margin: 0; font-size: 1.05rem; font-weight: 800; color: var(--ink); letter-spacing: -0.015em; }
  .meta { font-family: ui-monospace, monospace; font-size: 0.74rem; color: var(--mut); }
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
    font: inherit; font-size: 0.94rem; line-height: 1.48; resize: none; outline: none; padding: 0;
  }
  .remember-box textarea::placeholder { color: var(--mut2); }
  .rbar { display: flex; align-items: center; justify-content: space-between; gap: 0.6rem; }
  .shard-in {
    display: inline-flex; align-items: center; gap: 0.25rem;
    background: var(--bg); border: 1px solid var(--line); border-radius: 999px;
    padding: 0.22rem 0.65rem; font-size: 0.76rem; color: var(--amber); font-weight: 700;
  }
  .shard-in input {
    width: 88px; background: none; border: 0; color: var(--ink);
    font: inherit; font-size: 0.76rem; font-weight: 700; outline: none;
  }
  .rbtn {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.42rem 1rem; font: inherit; font-size: 0.8rem; font-weight: 800; cursor: pointer;
  }
  .rbtn:disabled { opacity: 0.38; cursor: default; }
  .empty-feed { padding: 3.5rem 1.5rem; text-align: center; color: var(--mut); }
  .empty-feed p { margin: 0 0 0.35rem; font-size: 0.95rem; color: var(--ink); }
  .empty-feed small { font-size: 0.82rem; color: var(--mut); }
</style>
