<script lang="ts">
  import type { NavTab, State } from '../lib/types'
  import { operatorJSON } from '../lib/api'
  import { fmtCompactSats, fmtSats, fmtSatsUsd, fmtTokens, fmtUsdCents } from '../lib/format'

  interface Props {
    s: State
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
    onNavigate: (tab: NavTab) => void
  }

  let { s, onRefresh, onToast, onNavigate }: Props = $props()

  let unitMode = $state<'usd' | 'sats'>('usd')
  let earningBusy = $state(false)

  const econToday = $derived(
    s.economics?.today ?? {
      tokensIn: 0,
      tokensOut: 0,
      tokensSaved: 0,
      earnedSats: 0,
      spentSats: 0,
      netSats: 0,
      factsBought: 0,
      factsSold: 0,
      workBounties: 0,
      estSavedUsdCents: 0
    }
  )

  const econAll = $derived(s.economics?.allTime ?? econToday)

  const earnedSatsToday = $derived(econToday.earnedSats > 0 ? econToday.earnedSats : s.receipts.earned24h)
  const spentSatsToday = $derived(econToday.spentSats > 0 ? econToday.spentSats : (s.treasury.spent24h ?? 0))
  const netSatsToday = $derived(earnedSatsToday - spentSatsToday)

  const me = $derived(
    s.microEarner ?? {
      enabled: false,
      status: s.treasury.sats >= 2000 ? 'funded_idle' : 'disabled',
      targetFloatSats: 2000,
      balanceSats: s.treasury.sats,
      earnedSatsTotal: 0,
      tasksCompleted: 0,
      bountiesSubmitted: 0,
      lastRunAt: 0
    }
  )

  const targetFloat = $derived(Math.max(1, me.targetFloatSats || 2000))
  const currentBalance = $derived(s.treasury.sats)
  const floatPct = $derived(Math.min(100, Math.round((currentBalance / targetFloat) * 100)))
  const floatReady = $derived(me.status === 'funded_idle' || currentBalance >= targetFloat)

  const distilledCount = $derived(s.distill?.distilledCount ?? 0)
  const quarantinedCount = $derived(s.distill?.quarantinedCount ?? 0)

  async function triggerAutoFloat() {
    earningBusy = true
    try {
      const res = await operatorJSON('POST', '/v1/microearner/toggle', {
        enabled: true,
        targetFloatSats: targetFloat,
        runNow: true
      }, 'Enable zero-sat Auto-Float earning')
      if (!res) return
      if (!res.ok) {
        onToast(res.data.error ?? `Auto-Float failed (${res.status})`, true)
      } else {
        onToast('Auto-Float cycle completed ✓')
        await onRefresh()
      }
    } finally {
      earningBusy = false
    }
  }
</script>

<section class="op-hero" aria-label="Operational economics overview">
  <div class="op-head">
    <div class="op-title-row">
      <span class="op-kicker">AUTONOMOUS SAVINGS &amp; EARNINGS</span>
      <span class="pii-pill" title="Fail-closed gitleaks + regex PII scrubber protects private shards from leaking onto the market">
        <span class="pii-dot"></span>
        PII Gate Active · {distilledCount} distilled · {quarantinedCount} blocked
      </span>
    </div>

    <div class="op-controls">
      <div class="unit-toggle" role="group" aria-label="Display unit">
        <button class="ut-btn" class:on={unitMode === 'usd'} onclick={() => (unitMode = 'usd')}>USD</button>
        <button class="ut-btn" class:on={unitMode === 'sats'} onclick={() => (unitMode = 'sats')}>sats</button>
      </div>
      <button class="vault-link" onclick={() => onNavigate('wallet')} title="Open Chain &amp; Vault details (UTXOs, BRC-29, Covenants)">
        Chain &amp; Vault →
      </button>
    </div>
  </div>

  <div class="kpi-grid">
    <div class="kpi-card">
      <div class="kpi-lbl">Tokens Saved Today</div>
      <div class="kpi-val amber">{fmtTokens(econToday.tokensSaved)}</div>
      <div class="kpi-sub">
        {#if unitMode === 'usd'}
          Est. {fmtUsdCents(econToday.estSavedUsdCents)} LLM savings
        {:else}
          All-time {fmtTokens(econAll.tokensSaved)} tokens
        {/if}
      </div>
    </div>

    <div class="kpi-card">
      <div class="kpi-lbl">Net Earned (24h)</div>
      <div class="kpi-val" class:ok={netSatsToday >= 0} class:bad={netSatsToday < 0}>
        {#if unitMode === 'usd'}
          {netSatsToday >= 0 ? '+' : ''}{fmtSatsUsd(netSatsToday)}
        {:else}
          {netSatsToday >= 0 ? '+' : ''}{fmtCompactSats(netSatsToday)} <span class="kpi-unit">sats</span>
        {/if}
      </div>
      <div class="kpi-sub">
        {#if unitMode === 'usd'}
          {fmtUsdCents(econToday.estSavedUsdCents)} saved · {fmtCompactSats(earnedSatsToday)} sats earned
        {:else}
          +{fmtCompactSats(earnedSatsToday)} earned · -{fmtCompactSats(spentSatsToday)} spent
        {/if}
      </div>
    </div>

    <div class="kpi-card">
      <div class="kpi-lbl">Verified Knowledge</div>
      <div class="kpi-val">
        {econToday.factsBought} <span class="kpi-sep">bought</span> · {econToday.factsSold} <span class="kpi-sep">sold</span>
      </div>
      <div class="kpi-sub">
        All-time: {econAll.factsBought} bought · {econAll.factsSold} sold · {econAll.workBounties} work tasks
      </div>
    </div>

    <div class="kpi-card float-card">
      <div class="kpi-lbl-row">
        <span class="kpi-lbl">Zero-Sat Auto-Float</span>
        <span class="float-badge" class:ready={floatReady}>
          {floatReady ? 'Ready' : me.enabled ? 'Earning' : 'Paused'}
        </span>
      </div>
      <div class="float-bar-wrap" aria-label="Auto-float funding progress">
        <div class="float-bar" style="width: {floatPct}%"></div>
      </div>
      <div class="float-foot">
        <span class="kpi-sub">
          {#if unitMode === 'usd'}
            {fmtSatsUsd(currentBalance)} / {fmtSatsUsd(targetFloat)} float
          {:else}
            {fmtSats(currentBalance)} / {fmtSats(targetFloat)} sats
          {/if}
        </span>
        {#if !floatReady}
          <button class="earn-btn" onclick={triggerAutoFloat} disabled={earningBusy}>
            {earningBusy ? 'Earning…' : 'Earn Float'}
          </button>
        {/if}
      </div>
    </div>
  </div>
</section>

<style>
  .op-hero {
    padding: 0.9rem 1.25rem 1rem;
    border-bottom: 1px solid var(--line);
    background: linear-gradient(180deg, rgb(247 153 27 / 4%) 0%, transparent 100%);
  }

  .op-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.75rem;
    flex-wrap: wrap;
    margin-bottom: 0.72rem;
  }

  .op-title-row {
    display: flex;
    align-items: center;
    gap: 0.65rem;
    flex-wrap: wrap;
  }

  .op-kicker {
    font-size: 0.68rem;
    font-weight: 800;
    letter-spacing: 0.08em;
    color: var(--mut2);
  }

  .pii-pill {
    display: inline-flex;
    align-items: center;
    gap: 0.38rem;
    padding: 0.16rem 0.58rem;
    border-radius: 999px;
    background: rgb(76 194 107 / 10%);
    border: 1px solid rgb(76 194 107 / 28%);
    color: var(--ok);
    font-size: 0.7rem;
    font-weight: 700;
  }

  .pii-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--ok);
  }

  .op-controls {
    display: flex;
    align-items: center;
    gap: 0.55rem;
  }

  .unit-toggle {
    display: inline-flex;
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 2px;
  }

  .ut-btn {
    border: 0;
    background: none;
    color: var(--mut);
    font: inherit;
    font-size: 0.69rem;
    font-weight: 700;
    padding: 0.16rem 0.55rem;
    border-radius: 999px;
    cursor: pointer;
    transition: background 0.12s, color 0.12s;
  }

  .ut-btn.on {
    background: var(--amber);
    color: #120e0c;
  }

  .vault-link {
    background: none;
    border: 1px solid var(--line);
    border-radius: 999px;
    color: var(--mut);
    font: inherit;
    font-size: 0.7rem;
    font-weight: 600;
    padding: 0.2rem 0.62rem;
    cursor: pointer;
    transition: border-color 0.14s, color 0.14s;
  }

  .vault-link:hover {
    border-color: var(--line2);
    color: var(--ink);
  }

  .kpi-grid {
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: 0.7rem;
  }

  .kpi-card {
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 12px;
    padding: 0.75rem 0.85rem;
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
    min-width: 0;
  }

  .kpi-lbl-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.4rem;
  }

  .kpi-lbl {
    font-size: 0.72rem;
    font-weight: 600;
    color: var(--mut);
  }

  .kpi-val {
    font-size: 1.32rem;
    font-weight: 800;
    letter-spacing: -0.03em;
    color: var(--ink);
    font-variant-numeric: tabular-nums;
    line-height: 1.15;
  }

  .kpi-val.amber { color: var(--amber); }
  .kpi-val.ok { color: var(--ok); }
  .kpi-val.bad { color: var(--bad); }

  .kpi-unit, .kpi-sep {
    font-size: 0.76rem;
    font-weight: 600;
    color: var(--mut);
  }

  .kpi-sub {
    font-size: 0.7rem;
    color: var(--mut2);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .float-card {
    justify-content: space-between;
  }

  .float-badge {
    font-size: 0.64rem;
    font-weight: 800;
    padding: 0.08rem 0.45rem;
    border-radius: 999px;
    background: rgb(247 153 27 / 14%);
    color: var(--amber);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }

  .float-badge.ready {
    background: rgb(76 194 107 / 14%);
    color: var(--ok);
  }

  .float-bar-wrap {
    height: 7px;
    background: var(--bg);
    border: 1px solid var(--line);
    border-radius: 999px;
    overflow: hidden;
    margin: 0.28rem 0 0.2rem;
  }

  .float-bar {
    height: 100%;
    background: linear-gradient(90deg, var(--amber), #4cc26b);
    border-radius: 999px;
    transition: width 0.25s ease;
  }

  .float-foot {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.4rem;
  }

  .earn-btn {
    background: var(--amber);
    color: #120e0c;
    border: 0;
    border-radius: 999px;
    padding: 0.18rem 0.58rem;
    font: inherit;
    font-size: 0.68rem;
    font-weight: 800;
    cursor: pointer;
    flex-shrink: 0;
  }

  .earn-btn:disabled {
    opacity: 0.6;
    cursor: wait;
  }

  @media (max-width: 1050px) {
    .kpi-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  }

  @media (max-width: 560px) {
    .kpi-grid { grid-template-columns: 1fr; }
  }
</style>
