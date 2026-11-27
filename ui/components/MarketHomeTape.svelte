<script lang="ts">
  import type { DealFeedEntry, NavTab, Receipt } from '../lib/types'
  import { agoMs, fmtSats } from '../lib/format'
  import type { HomeAgentPill } from './MarketHomeHelpers'

  interface Props {
    homeAgentPills: HomeAgentPill[]
    deals: DealFeedEntry[]
    paidReceipts: Receipt[]
    onNavigate: (tab: NavTab) => void
    onInspectDeal: (d: DealFeedEntry) => void
    onInspectReceipt: (r: Receipt) => void
  }

  let { homeAgentPills, deals, paidReceipts, onNavigate, onInspectDeal, onInspectReceipt }: Props = $props()
</script>

<!-- 3. MINIMAL ACTIVE AGENTS STRIP (Only visible when agents/listings are live) -->
{#if homeAgentPills.length > 0}
  <section class="agents-strip" aria-label="Active Agents">
    <div class="agents-pills">
      {#each homeAgentPills as ag (ag.id)}
        <button class="apill" onclick={() => onNavigate('agents')}>
          <span class="adot" class:on={ag.live}></span>
          <b>{ag.name}</b>
          <span class="aoffer">{ag.offer}</span>
          {#if ag.earned > 0}
            <span class="aearn">+{fmtSats(ag.earned)}s</span>
          {/if}
        </button>
      {/each}
    </div>
    <button class="fleet-link" onclick={() => onNavigate('agents')}>Agents →</button>
  </section>
{/if}

<!-- 4. CLEARED TRADES (Only renders when trades exist!) -->
{#if deals.length > 0 || paidReceipts.length > 0}
  <section class="tape-section">
    <div class="tape-list">
      {#each deals as d}
        {@const isSell = d.role === 'seller' || d.cause.startsWith('seller')}
        <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_noninteractive_element_interactions -->
        <article class="tape-row" onclick={() => onInspectDeal(d)}>
          <span class="side-pill" class:sell={isSell} class:buy={!isSell}>{isSell ? 'SELL' : 'BUY'}</span>
          <span class="tape-topic">{d.shard ? `#${d.shard}` : d.op.slice(0, 14)}</span>
          <span class="tape-desc">{d.deliveredText || d.cause}</span>
          <span class="tape-status" class:ok={d.to === 'settled'} class:bad={d.to === 'failed'}>
            {d.to === 'settled' ? '✓' : d.to}
          </span>
          {#if d.priceSats}
            <span class="tape-sats" class:sell={isSell}>{isSell ? '+' : '-'}{fmtSats(d.priceSats)}s</span>
          {/if}
          <span class="time">{agoMs(d.at)}</span>
        </article>
      {/each}

      {#each paidReceipts as r}
        <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_noninteractive_element_interactions -->
        <article class="tape-row" onclick={() => onInspectReceipt(r)}>
          <span class="side-pill sell">SELL</span>
          <span class="tape-topic">{r.shards.length ? `#${r.shards[0]}` : 'verified'}</span>
          <span class="tape-desc">{r.answer || 'Verified answer'}</span>
          <span class="tape-status ok">✓</span>
          {#if r.sats}
            <span class="tape-sats sell">+{fmtSats(r.sats)}s</span>
          {/if}
          <span class="time">{agoMs(r.at)}</span>
        </article>
      {/each}
    </div>
  </section>
{/if}

<style>
  /* Minimal Active Agents Strip */
  .agents-strip {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.75rem;
    padding: 0.7rem 1.25rem;
    border-top: 1px solid var(--line);
    background: #0c0b09;
    flex-wrap: wrap;
  }
  .agents-pills {
    display: flex;
    align-items: center;
    gap: 0.45rem;
    flex-wrap: wrap;
  }
  .apill {
    display: inline-flex;
    align-items: center;
    gap: 0.42rem;
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 0.28rem 0.72rem;
    color: var(--ink);
    font: inherit;
    font-size: 0.76rem;
    cursor: pointer;
    transition: border-color 0.14s, background 0.14s;
  }
  .apill:hover {
    border-color: var(--line2);
    background: var(--hover);
  }
  .adot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--mut2);
  }
  .adot.on {
    background: var(--ok);
    box-shadow: 0 0 6px rgb(76 194 107 / 55%);
  }
  .aoffer {
    color: var(--amber);
    font-family: ui-monospace, monospace;
    font-size: 0.72rem;
    font-weight: 700;
  }
  .aearn {
    color: var(--ok);
    font-family: ui-monospace, monospace;
    font-size: 0.72rem;
    font-weight: 800;
  }
  .fleet-link {
    background: none;
    border: 0;
    color: var(--mut);
    font: inherit;
    font-size: 0.76rem;
    font-weight: 700;
    cursor: pointer;
    padding: 0.2rem 0.35rem;
  }
  .fleet-link:hover {
    color: var(--amber);
  }

  /* Cleared Trades Tape */
  .tape-section {
    border-top: 1px solid var(--line);
    padding: 0.9rem 1.25rem 2rem;
  }
  .tape-list { display: flex; flex-direction: column; gap: 0.4rem; }
  .tape-row {
    display: flex; align-items: center; gap: 0.65rem;
    padding: 0.58rem 0.85rem; background: var(--surf); border: 1px solid var(--line);
    border-radius: 10px; cursor: pointer; font-size: 0.82rem;
    transition: background 0.12s, border-color 0.12s;
  }
  .tape-row:hover { background: var(--hover); border-color: var(--line2); }
  .side-pill {
    font-family: ui-monospace, monospace; font-size: 0.65rem; font-weight: 800;
    padding: 0.12rem 0.48rem; border-radius: 999px; flex-shrink: 0;
  }
  .side-pill.buy { background: rgb(76 194 107 / 14%); color: var(--ok); }
  .side-pill.sell { background: rgb(247 153 27 / 14%); color: var(--amber); }
  .tape-topic { font-weight: 700; color: var(--ink); white-space: nowrap; }
  .tape-desc { flex: 1; min-width: 0; color: var(--mut); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .tape-status { font-family: ui-monospace, monospace; font-size: 0.76rem; color: var(--mut); }
  .tape-status.ok { color: var(--ok); font-weight: 800; }
  .tape-status.bad { color: var(--bad); }
  .tape-sats { font-family: ui-monospace, monospace; font-weight: 800; color: var(--ink); white-space: nowrap; }
  .tape-sats.sell { color: var(--amber); }
  .time { color: var(--mut2); font-family: ui-monospace, monospace; font-size: 0.72rem; white-space: nowrap; }

  @media (max-width: 820px) {
    .tape-desc { display: none; }
  }
</style>
