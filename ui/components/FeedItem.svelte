<script lang="ts">
  import type { DealFeedEntry, FeedFragment, PendingApproval, Receipt } from '../lib/types'
  import { agoMs, agoUnix, fmtSats } from '../lib/format'

  export type TimelineEntry =
    | { kind: 'approval'; sortAt: number; data: PendingApproval }
    | { kind: 'memory'; sortAt: number; data: FeedFragment }
    | { kind: 'receipt'; sortAt: number; data: Receipt }
    | { kind: 'deal'; sortAt: number; data: DealFeedEntry }

  interface Props {
    entry: TimelineEntry
    onInspectMemory: (m: FeedFragment) => void
    onInspectReceipt: (r: Receipt) => void
    onInspectDeal: (d: DealFeedEntry) => void
    onForgetMemory: (shard: string, seq: number) => void
    onDecideApproval: (id: string, approve: boolean) => void
    onShareUrl: (url: string) => void
  }

  let {
    entry,
    onInspectMemory,
    onInspectReceipt,
    onInspectDeal,
    onForgetMemory,
    onDecideApproval,
    onShareUrl
  }: Props = $props()

  let menuOpen = $state(false)
</script>

{#if entry.kind === 'approval'}
  {@const a = entry.data}
  <article class="post approval">
    <div class="av av-warn">!</div>
    <div class="body">
      <div class="head">
        <b>Needs approval</b>
        <span class="sats">≤ {fmtSats(a.maxExtraSats)} sats</span>
      </div>
      <p class="txt">{a.reason}</p>
      <div class="appr-acts">
        <button class="btn-ok" onclick={() => onDecideApproval(a.id, true)}>Approve</button>
        <button class="btn-ghost" onclick={() => onDecideApproval(a.id, false)}>Deny</button>
      </div>
    </div>
  </article>
{:else if entry.kind === 'memory'}
  {@const m = entry.data}
  <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_noninteractive_element_interactions -->
  <article class="post clickable" onclick={() => onInspectMemory(m)}>
    <div class="av">
      {#if m.batchN >= 0}
        <span class="seal ok" title={m.settled ? 'Verified on BSV blockchain' : 'Anchored on BSV'}>✓</span>
      {:else}
        <span class="seal" title="Saved locally (auto-anchors when funded)">●</span>
      {/if}
    </div>
    <div class="body">
      <div class="head">
        <b>Saved</b>
        <span class="tag">#{m.shard}</span>
        {#if m.batchN >= 0}
          <span class="badge ok" title={m.settled ? 'Verified on BSV blockchain' : 'Anchored on BSV'}>
            {m.settled ? 'Verified ✓' : 'Anchored'}
          </span>
        {:else}
          <span class="badge muted" title="Saved locally; auto-anchors when funded">Local</span>
        {/if}
        <span class="time">{agoUnix(m.createdAt)}</span>
        <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
        <div class="more-wrap" onclick={(e) => e.stopPropagation()}>
          <button class="more" onclick={() => (menuOpen = !menuOpen)} aria-label="More">···</button>
          {#if menuOpen}
            <div class="menu">
              <button onclick={() => { menuOpen = false; onInspectMemory(m) }}>View proof</button>
              <button class="danger" onclick={() => { menuOpen = false; onForgetMemory(m.shard, m.seq) }}>Forget &amp; shred</button>
            </div>
          {/if}
        </div>
      </div>
      <p class="txt" class:muted={m.encrypted}>{m.text}</p>
    </div>
  </article>
{:else if entry.kind === 'receipt'}
  {@const r = entry.data}
  {@const verdict = r.verdict ?? (r.anchored ? 'green' : 'amber')}
  <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_noninteractive_element_interactions -->
  <article class="post clickable" onclick={() => onInspectReceipt(r)}>
    <div class="av">
      <span
        class="seal"
        class:ok={verdict === 'green'}
        class:warn={verdict === 'amber'}
        class:bad={verdict === 'red'}
        title={verdict === 'green' ? 'Verified against saved memories on BSV' : verdict === 'red' ? 'Verification failed' : 'Verified locally, awaiting confirmation'}
      >
        {verdict === 'red' ? '✕' : '✓'}
      </span>
    </div>
    <div class="body">
      <div class="head">
        <b>Answered</b>
        {#each r.shards as sh}<span class="tag">#{sh}</span>{/each}
        <span
          class="badge"
          class:ok={verdict === 'green'}
          class:warn={verdict === 'amber'}
          class:bad={verdict === 'red'}
        >
          {verdict === 'green' ? 'Verified ✓' : verdict === 'red' ? 'Check failed' : 'Verified locally'}
        </span>
        {#if r.sats !== null && r.sats > 0}
          <span class="sats">+{fmtSats(r.sats)} sats</span>
        {/if}
        <span class="time">{agoMs(r.at)}</span>
        <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
        <button
          class="share-ic"
          onclick={(e) => { e.stopPropagation(); onShareUrl(window.location.origin + r.url) }}
          title="Copy verification link"
        >↗</button>
      </div>
      <p class="txt">{r.answer || 'Verified answer'}</p>
    </div>
  </article>
{:else}
  {@const d = entry.data}
  <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_noninteractive_element_interactions -->
  <article class="post clickable" onclick={() => onInspectDeal(d)}>
    <div class="av">
      <span class="seal" class:ok={d.to === 'settled'} class:bad={d.to === 'failed'}>⚡</span>
    </div>
    <div class="body">
      <div class="head">
        <b>Trade</b>
        <span class="badge" class:ok={d.to === 'settled'} class:bad={d.to === 'failed'} class:muted={d.to !== 'settled' && d.to !== 'failed'}>
          {d.to === 'settled' ? 'Completed ✓' : d.to === 'failed' ? 'Failed' : d.to}
        </span>
        <span class="time">{agoMs(d.at)}</span>
      </div>
      <p class="txt">{d.cause}</p>
    </div>
  </article>
{/if}

<style>
  .post {
    display: flex; gap: 0.9rem; padding: 1.05rem 1.25rem;
    border-bottom: 1px solid var(--line); transition: background 0.12s;
  }
  .post.clickable { cursor: pointer; }
  .post.clickable:hover { background: var(--hover); }
  .post.approval { background: rgb(247 153 27 / 5%); border-left: 2px solid var(--amber); }
  .av {
    width: 34px; height: 34px; border-radius: 50%; flex-shrink: 0;
    display: grid; place-items: center; background: var(--surf); border: 1px solid var(--line);
  }
  .av-warn { background: var(--amber); border-color: var(--amber); color: #120e0c; font-weight: 800; }
  .seal { font-size: 0.78rem; font-weight: 800; color: var(--mut2); }
  .seal.ok { color: var(--ok); }
  .seal.warn { color: var(--amber); }
  .seal.bad { color: var(--bad); }
  .body { flex: 1; min-width: 0; }
  .head {
    display: flex; align-items: center; gap: 0.45rem; flex-wrap: wrap;
    font-size: 0.86rem; margin-bottom: 0.3rem;
  }
  .tag { font-weight: 700; color: var(--amber); font-size: 0.84rem; }
  .badge {
    font-size: 0.72rem; font-weight: 700; padding: 0.1rem 0.45rem;
    border-radius: 999px; background: var(--surf); border: 1px solid var(--line);
    color: var(--mut);
  }
  .badge.ok { color: var(--ok); background: rgb(34 197 94 / 10%); border-color: rgb(34 197 94 / 28%); }
  .badge.warn { color: var(--amber); background: rgb(247 153 27 / 10%); border-color: rgb(247 153 27 / 30%); }
  .badge.bad { color: var(--bad); background: rgb(239 68 68 / 10%); border-color: rgb(239 68 68 / 30%); }
  .badge.muted { color: var(--mut2); }
  .sats { color: var(--ok); font-weight: 800; font-size: 0.82rem; font-variant-numeric: tabular-nums; }
  .time { margin-left: auto; color: var(--mut2); font-size: 0.76rem; font-variant-numeric: tabular-nums; }
  .txt {
    margin: 0; font-size: 0.95rem; line-height: 1.55; color: var(--ink);
    white-space: pre-wrap; word-break: break-word;
  }
  .txt.muted { color: var(--mut); font-style: italic; }
  .more-wrap { position: relative; }
  .more, .share-ic {
    background: none; border: 0; color: var(--mut2); font: inherit; font-weight: 700;
    padding: 0.1rem 0.35rem; border-radius: 6px; cursor: pointer;
    opacity: 0; transition: opacity 0.12s, color 0.12s;
  }
  .post:hover .more, .post:hover .share-ic, .more:focus-visible, .share-ic:focus-visible { opacity: 1; }
  .more:hover, .share-ic:hover { color: var(--amber); }
  .menu {
    position: absolute; right: 0; top: 100%; z-index: 20; min-width: 185px;
    background: #171412; border: 1px solid var(--line2); border-radius: 10px; padding: 0.3rem;
    box-shadow: 0 12px 30px rgb(0 0 0 / 60%); display: grid; gap: 0.15rem;
  }
  .menu button {
    background: none; border: 0; color: var(--ink); font: inherit; font-size: 0.8rem;
    text-align: left; padding: 0.45rem 0.65rem; border-radius: 7px; cursor: pointer;
  }
  .menu button:hover { background: var(--hover); }
  .menu button.danger { color: var(--bad); }
  .appr-acts { display: flex; gap: 0.5rem; margin-top: 0.7rem; }
  .btn-ok {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.38rem 0.95rem; font: inherit; font-size: 0.8rem; font-weight: 800; cursor: pointer;
  }
  .btn-ghost {
    background: transparent; color: var(--mut); border: 1px solid var(--line2);
    border-radius: 999px; padding: 0.38rem 0.9rem; font: inherit; font-size: 0.8rem; font-weight: 600; cursor: pointer;
  }
</style>
