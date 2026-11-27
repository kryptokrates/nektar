<script lang="ts">
  import type { AssetBasketSummary } from '../lib/types'
  import { fmtSats, shortHash } from '../lib/format'

  interface Props {
    baskets: AssetBasketSummary[]
  }

  let { baskets }: Props = $props()

  let expandedBasket = $state<string>('')
</script>

{#if baskets.length > 0}
  <section class="baskets-sec">
    <div class="section-lbl">BRC-100 Asset Baskets</div>
    <div class="basket-list">
      {#each baskets as b}
        <button
          class="basket-card"
          class:open={expandedBasket === b.basket}
          onclick={() => (expandedBasket = expandedBasket === b.basket ? '' : b.basket)}
        >
          <div class="brow">
            <div class="bmeta">
              <b>{b.title}</b>
              <code class="bcode">{b.basket}</code>
            </div>
            <div class="bstat">
              <span class="bcnt">{b.count}</span>
              <span class="bsats">{fmtSats(b.totalSats)} sats</span>
            </div>
          </div>
          <div class="bdesc">{b.description}</div>
        </button>
        {#if expandedBasket === b.basket && b.items.length > 0}
          <div class="bitems">
            {#each b.items as item}
              <div class="bitem">
                <div class="bitem-l">
                  {#if item.meta?.shard}
                    <b>#{String(item.meta.shard)}</b>
                    <span class="mut">batch #{String(item.meta.n ?? 0)} · {String(item.meta.size ?? 0)} frags</span>
                  {:else if item.meta?.kind}
                    <b>{String(item.meta.kind)}</b>
                    <span class="mut">{shortHash(item.outpoint, 10, 6)}</span>
                  {:else}
                    <code class="bcode">{shortHash(item.outpoint, 10, 6)}</code>
                  {/if}
                </div>
                <div class="bitem-r">{fmtSats(item.satoshis)} sats</div>
              </div>
            {/each}
          </div>
        {/if}
      {/each}
    </div>
  </section>
{/if}

<style>
  .section-lbl {
    padding: 0.95rem 1.25rem 0.45rem; font-size: 0.7rem; font-weight: 700;
    letter-spacing: 0.1em; text-transform: uppercase; color: var(--mut2);
  }
  .mut { color: var(--mut); font-size: 0.76rem; }
  .baskets-sec { border-bottom: 1px solid var(--line); padding-bottom: 0.85rem; }
  .basket-list { display: grid; gap: 0.45rem; padding: 0 1.1rem; }
  .basket-card {
    text-align: left; background: var(--surf); border: 1px solid var(--line);
    border-radius: 12px; padding: 0.65rem 0.8rem; cursor: pointer; color: inherit; font: inherit;
    transition: border-color 0.14s;
  }
  .basket-card:hover, .basket-card.open { border-color: var(--amber); }
  .brow { display: flex; align-items: center; justify-content: space-between; gap: 0.5rem; }
  .bmeta { display: flex; flex-direction: column; gap: 0.12rem; min-width: 0; }
  .bmeta b { font-size: 0.84rem; color: var(--ink); }
  .bcode { font-size: 0.7rem; color: var(--amber); font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
  .bstat { text-align: right; display: flex; flex-direction: column; align-items: flex-end; }
  .bcnt { font-size: 0.92rem; font-weight: 800; color: var(--ink); font-variant-numeric: tabular-nums; }
  .bsats { font-size: 0.7rem; color: var(--mut); }
  .bdesc { margin-top: 0.3rem; font-size: 0.73rem; color: var(--mut); }
  .bitems {
    margin: -0.2rem 0 0.25rem; padding: 0.45rem 0.75rem; border-radius: 10px;
    background: var(--bg); border: 1px solid var(--line); display: grid; gap: 0.4rem;
  }
  .bitem { display: flex; align-items: center; justify-content: space-between; gap: 0.5rem; font-size: 0.76rem; }
  .bitem-l { display: flex; flex-direction: column; min-width: 0; }
  .bitem-r { font-weight: 700; color: var(--ink); white-space: nowrap; }
</style>
