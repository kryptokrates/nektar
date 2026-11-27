<script lang="ts">
  import type { DistillState, MicroEarnerState } from '../lib/types'
  import { operatorJSON } from '../lib/api'
  import { agoUnix, shortHash } from '../lib/format'

  interface Props {
    distill?: DistillState
    microEarner?: MicroEarnerState
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
  }

  let { distill, microEarner, onRefresh, onToast }: Props = $props()

  let distillBusy = $state(false)
  let earnerBusy = $state(false)

  const distilledCount = $derived(distill?.distilledCount ?? 0)
  const quarantinedCount = $derived(distill?.quarantinedCount ?? 0)
  const quarantined = $derived((distill?.quarantined ?? []).slice(0, 5))
  const marketShards = $derived(distill?.marketShards ?? [])
  const lastCycle = $derived(distill?.lastCycle)

  const earnerEnabled = $derived(microEarner?.enabled ?? false)
  const earnerStatus = $derived(microEarner?.status ?? 'disabled')

  async function runDistillNow() {
    distillBusy = true
    try {
      const res = await operatorJSON<{ totalDistilled?: number; totalQuarantined?: number; status?: string }>(
        'POST',
        '/v1/distill/run',
        { autoAnchor: true, autoList: true, force: true },
        'Run SleepCycle distillation and PII gate'
      )
      if (!res) return
      if (!res.ok) {
        onToast(res.data.error ?? `Distillation failed (${res.status})`, true)
      } else {
        onToast(`SleepCycle complete: ${res.data.totalDistilled ?? 0} distilled, ${res.data.totalQuarantined ?? 0} blocked ✓`)
        await onRefresh()
      }
    } finally {
      distillBusy = false
    }
  }

  async function toggleMicroEarner() {
    earnerBusy = true
    try {
      const next = !earnerEnabled
      const res = await operatorJSON(
        'POST',
        '/v1/microearner/toggle',
        { enabled: next, runNow: next },
        next ? 'Enable background zero-sat Micro-Earner' : 'Pause background Micro-Earner'
      )
      if (!res) return
      if (!res.ok) {
        onToast(res.data.error ?? `Micro-Earner toggle failed (${res.status})`, true)
      } else {
        onToast(next ? 'Auto-Float Micro-Earner enabled ✓' : 'Auto-Float Micro-Earner paused')
        await onRefresh()
      }
    } finally {
      earnerBusy = false
    }
  }
</script>

<section class="sleep-card" aria-label="SleepCycle Distillation and PII Gate">
  <header class="card-head">
    <div>
      <h3>SleepCycle &amp; PII Gate</h3>
      <p class="sub">
        Fail-closed secret scrubbing · JevGraph triples · Cedar sell guard
      </p>
    </div>
    <div class="head-acts">
      <button class="btn-amber" onclick={runDistillNow} disabled={distillBusy}>
        {distillBusy ? 'Distilling…' : 'Distill Now'}
      </button>
    </div>
  </header>

  <div class="stats-row">
    <div class="stat">
      <span class="slbl">Distilled Facts</span>
      <strong class="sval ok">{distilledCount}</strong>
    </div>
    <div class="stat">
      <span class="slbl">Secrets Blocked</span>
      <strong class="sval" class:amber={quarantinedCount > 0}>{quarantinedCount}</strong>
    </div>
    <div class="stat">
      <span class="slbl">Market Shards</span>
      <strong class="sval">{marketShards.length}</strong>
    </div>
    <div class="stat">
      <span class="slbl">Last Cycle</span>
      <strong class="sval sm">{lastCycle?.lastRunAt ? agoUnix(lastCycle.lastRunAt) + ' ago' : 'Idle'}</strong>
    </div>
  </div>

  {#if marketShards.length > 0}
    <div class="shard-pills">
      {#each marketShards as sh}
        <span class="sh-pill">{sh}</span>
      {/each}
    </div>
  {/if}

  <div class="earner-row">
    <div>
      <div class="er-title">Background Zero-Sat Micro-Earner</div>
      <div class="er-sub">
        Status: <strong>{earnerStatus}</strong>
        {#if (microEarner?.tasksCompleted ?? 0) > 0 || (microEarner?.bountiesSubmitted ?? 0) > 0}
          · {microEarner?.tasksCompleted ?? 0} audits · {microEarner?.bountiesSubmitted ?? 0} sponsored
        {/if}
      </div>
    </div>
    <button class="btn-toggle" class:on={earnerEnabled} onclick={toggleMicroEarner} disabled={earnerBusy}>
      {earnerBusy ? '…' : earnerEnabled ? 'Auto-Float ON' : 'Auto-Float OFF'}
    </button>
  </div>

  {#if quarantined.length > 0}
    <div class="quarantine-box">
      <div class="q-title">Quarantined Blocks (rule ID &amp; hash only — zero secret plaintext)</div>
      <ul class="q-list">
        {#each quarantined as q}
          <li>
            <span class="q-rule">{q.ruleId}</span>
            <span class="q-meta">{q.shard}#{q.seq}</span>
            <span class="q-hash">{shortHash(q.contentHash, 8, 6)}</span>
          </li>
        {/each}
      </ul>
    </div>
  {:else}
    <div class="clean-note">
      Private working shards are non-sellable by default. Only sanitized, ontology-pinned facts cross into <code>market-*</code> shards.
    </div>
  {/if}
</section>

<style>
  .sleep-card {
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 14px;
    padding: 0.9rem 1rem;
    display: flex;
    flex-direction: column;
    gap: 0.7rem;
  }

  .card-head {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 0.6rem;
    flex-wrap: wrap;
  }

  h3 {
    margin: 0;
    font-size: 0.9rem;
    font-weight: 800;
    color: var(--ink);
  }

  .sub {
    margin: 0.14rem 0 0;
    font-size: 0.72rem;
    color: var(--mut2);
  }

  .btn-amber {
    background: var(--amber);
    color: #120e0c;
    border: 0;
    border-radius: 999px;
    padding: 0.34rem 0.82rem;
    font: inherit;
    font-size: 0.74rem;
    font-weight: 800;
    cursor: pointer;
  }

  .btn-amber:disabled { opacity: 0.6; cursor: wait; }

  .stats-row {
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: 0.45rem;
  }

  .stat {
    background: var(--bg);
    border: 1px solid var(--line);
    border-radius: 9px;
    padding: 0.42rem 0.55rem;
    display: flex;
    flex-direction: column;
    gap: 0.1rem;
  }

  .slbl { font-size: 0.65rem; color: var(--mut2); }
  .sval { font-size: 1rem; font-weight: 800; color: var(--ink); font-variant-numeric: tabular-nums; }
  .sval.sm { font-size: 0.82rem; }
  .sval.ok { color: var(--ok); }
  .sval.amber { color: var(--amber); }

  .shard-pills {
    display: flex;
    gap: 0.35rem;
    flex-wrap: wrap;
  }

  .sh-pill {
    font-family: ui-monospace, monospace;
    font-size: 0.68rem;
    padding: 0.14rem 0.48rem;
    border-radius: 999px;
    background: rgb(247 153 27 / 10%);
    border: 1px solid rgb(247 153 27 / 25%);
    color: var(--amber);
  }

  .earner-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.6rem;
    padding: 0.55rem 0.7rem;
    background: var(--bg);
    border: 1px solid var(--line);
    border-radius: 10px;
  }

  .er-title {
    font-size: 0.76rem;
    font-weight: 700;
    color: var(--ink);
  }

  .er-sub {
    font-size: 0.69rem;
    color: var(--mut2);
    margin-top: 0.1rem;
  }

  .btn-toggle {
    background: transparent;
    color: var(--mut);
    border: 1px solid var(--line2);
    border-radius: 999px;
    padding: 0.28rem 0.72rem;
    font: inherit;
    font-size: 0.72rem;
    font-weight: 700;
    cursor: pointer;
    flex-shrink: 0;
  }

  .btn-toggle.on {
    background: rgb(76 194 107 / 14%);
    border-color: rgb(76 194 107 / 40%);
    color: var(--ok);
  }

  .quarantine-box {
    background: rgb(235 87 87 / 6%);
    border: 1px solid rgb(235 87 87 / 24%);
    border-radius: 10px;
    padding: 0.52rem 0.68rem;
  }

  .q-title {
    font-size: 0.68rem;
    font-weight: 700;
    color: var(--bad);
    margin-bottom: 0.35rem;
  }

  .q-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
  }

  .q-list li {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.5rem;
    font-size: 0.7rem;
  }

  .q-rule {
    font-family: ui-monospace, monospace;
    color: var(--ink);
    font-weight: 700;
  }

  .q-meta { color: var(--mut); }

  .q-hash {
    font-family: ui-monospace, monospace;
    color: var(--mut2);
  }

  .clean-note {
    font-size: 0.72rem;
    line-height: 1.45;
    color: var(--mut);
  }

  @media (max-width: 560px) {
    .stats-row { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  }
</style>
