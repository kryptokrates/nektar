<script lang="ts">
  import type { State } from '../lib/types'
  import { operatorJSON } from '../lib/api'
  import { fmtCompactSats, fmtSatsUsd, fmtTokens, fmtUsdCents } from '../lib/format'

  interface Props {
    s: State
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
  }

  let { s, onRefresh, onToast }: Props = $props()

  let unitMode = $state<'usd' | 'sats'>('usd')
  let earningBusy = $state(false)

  const today = $derived(s.economics?.today)
  const tokensSaved = $derived(today?.tokensSaved ?? 0)
  const savedCents = $derived(today?.estSavedUsdCents ?? 0)
  const earnedSats = $derived((today?.earnedSats ?? 0) > 0 ? (today?.earnedSats ?? 0) : s.receipts.earned24h)
  const spentSats = $derived((today?.spentSats ?? 0) > 0 ? (today?.spentSats ?? 0) : (s.treasury.spent24h ?? 0))
  const netSats = $derived(earnedSats - spentSats)
  const factsBought = $derived(today?.factsBought ?? 0)
  const targetFloat = $derived(Math.max(1, s.microEarner?.targetFloatSats || 2000))
  const needsFloat = $derived(s.treasury.sats < targetFloat)

  async function triggerAutoFloat() {
    earningBusy = true
    try {
      const res = await operatorJSON(
        'POST',
        '/v1/microearner/toggle',
        { enabled: true, targetFloatSats: targetFloat, runNow: true },
        'Enable zero-sat Auto-Float earning'
      )
      if (!res) return
      if (!res.ok) onToast(res.data.error ?? `Auto-Float failed (${res.status})`, true)
      else {
        onToast('Auto-Float cycle completed ✓')
        await onRefresh()
      }
    } finally {
      earningBusy = false
    }
  }
</script>

<section class="op-strip" aria-label="Operational summary">
  <div class="metrics">
    <span class="m">
      <strong class="amber">{fmtTokens(tokensSaved)}</strong> tokens saved
      {#if savedCents > 0}<span class="dim">({fmtUsdCents(savedCents)})</span>{/if}
    </span>
    <span class="sep">·</span>
    <span class="m">
      <strong class:ok={netSats >= 0} class:bad={netSats < 0}>
        {netSats >= 0 ? '+' : ''}{unitMode === 'usd' ? fmtSatsUsd(netSats) : `${fmtCompactSats(netSats)} sats`}
      </strong>
      earned
    </span>
    <span class="sep">·</span>
    <span class="m"><strong>{factsBought}</strong> facts bought</span>
  </div>

  <div class="acts">
    {#if needsFloat}
      <button class="earn-btn" onclick={triggerAutoFloat} disabled={earningBusy}>
        {earningBusy ? 'Earning…' : 'Earn float'}
      </button>
    {/if}
    <div class="unit-toggle" role="group" aria-label="Display unit">
      <button class="ut" class:on={unitMode === 'usd'} onclick={() => (unitMode = 'usd')}>USD</button>
      <button class="ut" class:on={unitMode === 'sats'} onclick={() => (unitMode = 'sats')}>sats</button>
    </div>
  </div>
</section>

<style>
  .op-strip {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.75rem;
    flex-wrap: wrap;
    padding: 0.55rem 1.25rem;
    border-bottom: 1px solid var(--line);
    background: rgb(247 153 27 / 3%);
    font-size: 0.8rem;
  }

  .metrics {
    display: flex;
    align-items: center;
    gap: 0.55rem;
    flex-wrap: wrap;
    color: var(--mut);
  }

  .m {
    display: inline-flex;
    align-items: center;
    gap: 0.28rem;
  }

  .m strong {
    color: var(--ink);
    font-weight: 800;
    font-variant-numeric: tabular-nums;
  }

  .m strong.amber { color: var(--amber); }
  .m strong.ok { color: var(--ok); }
  .m strong.bad { color: var(--bad); }
  .dim { color: var(--mut2); font-size: 0.74rem; }
  .sep { color: var(--line2); }

  .pii { color: var(--ok); font-weight: 600; font-size: 0.75rem; }
  .dot { width: 6px; height: 6px; border-radius: 50%; background: var(--ok); }

  .acts {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }

  .earn-btn {
    background: var(--amber);
    color: #120e0c;
    border: 0;
    border-radius: 999px;
    padding: 0.2rem 0.65rem;
    font: inherit;
    font-size: 0.72rem;
    font-weight: 800;
    cursor: pointer;
  }
  .earn-btn:disabled { opacity: 0.6; cursor: wait; }

  .unit-toggle {
    display: inline-flex;
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 2px;
  }

  .ut {
    border: 0;
    background: none;
    color: var(--mut);
    font: inherit;
    font-size: 0.68rem;
    font-weight: 700;
    padding: 0.12rem 0.48rem;
    border-radius: 999px;
    cursor: pointer;
  }

  .ut.on { background: var(--amber); color: #120e0c; }
</style>
