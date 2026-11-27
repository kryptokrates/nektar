<script lang="ts">
  import type { State } from '../lib/types'
  import { operatorFetch } from '../lib/api'
  import { fmtSats } from '../lib/format'

  interface Props {
    s: State
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
    onConfirm: (title: string, detail: string) => Promise<boolean>
  }

  let { s, onRefresh, onToast, onConfirm }: Props = $props()

  // Editable Policy Profiles (Easy / Standard / Secure) — clicking a profile prefills limits & backend match confidence
  type Tier = 'easy' | 'standard' | 'secure'
  const tierDefaults: Record<
    Tier,
    {
      label: string
      tagline: string
      role: 'buyer' | 'seller' | 'both'
      maxPerDeal: number
      maxPerDay: number
      zeroConfMax: number
    }
  > = {
    easy: {
      label: 'Easy',
      tagline: 'Fast autonomous trading',
      role: 'both',
      maxPerDeal: 5000,
      maxPerDay: 50000,
      zeroConfMax: 2500
    },
    standard: {
      label: 'Standard',
      tagline: 'Balanced everyday safety',
      role: 'both',
      maxPerDeal: 2000,
      maxPerDay: 20000,
      zeroConfMax: 1000
    },
    secure: {
      label: 'Secure',
      tagline: 'Tight caps, strictest checks',
      role: 'buyer',
      maxPerDeal: 250,
      maxPerDay: 2000,
      zeroConfMax: 0
    }
  }

  let selectedTier = $state<Tier>('standard')
  let polRole = $state<'buyer' | 'seller' | 'both'>('both')
  let maxPerDeal = $state('2000')
  let maxPerDay = $state('20000')
  let zeroConfMax = $state('1000')
  let domainsInput = $state('')

  let showCustomize = $state(false)
  let showRules = $state(false)
  let busy = $state(false)

  const activePolicies = $derived(s.settings.policies.filter((p) => !p.revokedAt))

  function applyTier(t: Tier) {
    selectedTier = t
    const d = tierDefaults[t]
    polRole = d.role
    maxPerDeal = String(d.maxPerDeal)
    maxPerDay = String(d.maxPerDay)
    zeroConfMax = String(d.zeroConfMax)
  }

  async function savePolicy() {
    const deal = Math.max(1, Math.floor(Number(maxPerDeal) || 0))
    const day = Math.max(deal, Math.floor(Number(maxPerDay) || deal))
    const zc = Math.max(0, Math.floor(Number(zeroConfMax) || 0))
    const domains = domainsInput
      .split(',')
      .map((x) => x.trim())
      .filter(Boolean)

    const body: Record<string, unknown> = {
      profile: selectedTier,
      preset: polRole,
      maxPerDeal: deal,
      maxPerDay: day,
      zeroConfMax: zc,
      ...(domains.length ? { domains } : {})
    }
    if (s.network === 'main') {
      const ok = await onConfirm(
        'Approve Mainnet Policy',
        `Save "${tierDefaults[selectedTier].label}" policy (${fmtSats(deal)}/deal · ${fmtSats(day)}/day) on Mainnet?`
      )
      if (!ok) return
      body.confirm = 'I_UNDERSTAND'
    }
    busy = true
    try {
      const r = await operatorFetch('POST', '/v1/onboard/policy-preset', body, 'Save policy')
      if (!r) return
      const j = (await r.json().catch(() => ({}))) as { error?: string }
      if (!r.ok) onToast(j.error ?? `Save failed (${r.status})`, true)
      else {
        onToast(`${tierDefaults[selectedTier].label} policy saved ✓`)
        await onRefresh()
      }
    } finally {
      busy = false
    }
  }

  async function revokePolicy(id: string) {
    const ok = await onConfirm('Revoke Rule', 'Pause autonomous trades under this rule?')
    if (!ok) return
    const r = await operatorFetch('DELETE', `/v1/policies/${encodeURIComponent(id)}`, undefined, 'Revoke rule')
    if (r && r.ok) {
      onToast('Rule revoked')
      await onRefresh()
    }
  }
</script>

<div class="pad">
  <div class="tier-grid">
    {#each (['easy', 'standard', 'secure'] as const) as t}
      {@const d = tierDefaults[t]}
      <button class="tcard" class:on={selectedTier === t} onclick={() => applyTier(t)}>
        <div class="thead">
          <b>{d.label}</b>
          <span class="radio" class:on={selectedTier === t}></span>
        </div>
        <small>{d.tagline}</small>
      </button>
    {/each}
  </div>

  <button class="save-bar" onclick={savePolicy} disabled={busy}>
    {busy ? 'Saving…' : `Save ${tierDefaults[selectedTier].label} Policy`}
  </button>

  <div class="group">
    <button class="grow clickable" onclick={() => (showCustomize = !showCustomize)}>
      <b>Customize limits</b>
      <span class="arr">{showCustomize ? '▾' : '→'}</span>
    </button>

    {#if showCustomize}
      <div class="custom-body">
        <div class="role-row">
          <span class="flbl">Mode</span>
          <div class="rpills">
            <button class="rp" class:on={polRole === 'buyer'} onclick={() => (polRole = 'buyer')}>Buy only</button>
            <button class="rp" class:on={polRole === 'seller'} onclick={() => (polRole = 'seller')}>Sell only</button>
            <button class="rp" class:on={polRole === 'both'} onclick={() => (polRole = 'both')}>Buy &amp; Sell</button>
          </div>
        </div>

        <div class="num-grid">
          <label class="field">
            <span>Max per deal</span>
            <input class="in" type="number" min="1" bind:value={maxPerDeal} />
          </label>
          <label class="field">
            <span>Max per day</span>
            <input class="in" type="number" min="1" bind:value={maxPerDay} />
          </label>
          <label class="field">
            <span>Instant max</span>
            <input class="in" type="number" min="0" bind:value={zeroConfMax} />
          </label>
        </div>

        <label class="field">
          <span>Topics (optional)</span>
          <input class="in" bind:value={domainsInput} placeholder="Any topic" />
        </label>
      </div>
    {/if}

    {#if activePolicies.length}
      <button class="grow clickable" onclick={() => (showRules = !showRules)}>
        <div class="gcol">
          <b>Active rules</b>
        </div>
        <span class="val">{activePolicies.length} {showRules ? '▾' : '→'}</span>
      </button>

      {#if showRules}
        {#each activePolicies as row}
          <div class="grow subrow">
            <div class="gcol">
              <b>{row.policy?.roles?.join(' & ') || 'Policy'}</b>
              {#if row.policy}
                <small>≤ {fmtSats(row.policy.limits?.maxPerDeal ?? 0)} / deal · ≤ {fmtSats(row.policy.limits?.maxPerDay ?? 0)} / day</small>
              {/if}
            </div>
            <button class="rev" onclick={() => revokePolicy(row.policyId)}>Revoke</button>
          </div>
        {/each}
      {/if}
    {/if}
  </div>
</div>

<style>
  .pad { padding: 1.35rem 1.5rem 2.4rem; display: grid; gap: 1rem; width: 100%; }
  .tier-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 0.65rem; }
  @media (max-width: 560px) {
    .tier-grid { grid-template-columns: 1fr; }
  }
  .tcard {
    background: var(--surf); border: 1px solid var(--line); border-radius: 14px;
    padding: 1rem; text-align: left; color: var(--ink); font: inherit;
    display: grid; gap: 0.35rem; align-content: start; cursor: pointer;
    transition: border-color 0.14s, background 0.14s;
  }
  .tcard:hover { border-color: var(--line2); background: var(--hover); }
  .tcard.on { border-color: var(--amber); background: rgb(247 153 27 / 7%); }
  .thead { display: flex; align-items: center; justify-content: space-between; gap: 0.4rem; }
  .tcard b { font-size: 0.94rem; }
  .tcard small { color: var(--mut); font-size: 0.78rem; line-height: 1.4; }
  .radio {
    width: 15px; height: 15px; border-radius: 50%; border: 1.5px solid var(--line2); flex-shrink: 0;
  }
  .radio.on { border-color: var(--amber); background: var(--amber); box-shadow: inset 0 0 0 3px var(--surf); }
  .save-bar {
    width: 100%; background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.72rem 1.2rem; font: inherit; font-size: 0.88rem; font-weight: 800; cursor: pointer;
  }
  .save-bar:hover:not(:disabled) { filter: brightness(1.07); }
  .save-bar:disabled { opacity: 0.45; }
  .custom-body {
    padding: 0.95rem 1rem 1.05rem; border-bottom: 1px solid var(--line);
    display: grid; gap: 0.85rem; background: rgb(0 0 0 / 18%);
  }
  .role-row { display: flex; align-items: center; justify-content: space-between; gap: 0.75rem; flex-wrap: wrap; }
  .flbl { font-size: 0.8rem; font-weight: 600; color: var(--mut); }
  .rpills { display: flex; gap: 0.35rem; }
  .rp {
    background: var(--bg); border: 1px solid var(--line); color: var(--mut);
    border-radius: 999px; padding: 0.26rem 0.7rem; font: inherit; font-size: 0.76rem; font-weight: 700; cursor: pointer;
  }
  .rp.on { border-color: var(--amber); color: var(--amber); background: rgb(247 153 27 / 10%); }
  .num-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 0.6rem; }
  @media (max-width: 560px) {
    .num-grid { grid-template-columns: 1fr; }
  }
  .field { display: grid; gap: 0.3rem; }
  .field span { font-size: 0.74rem; color: var(--mut); font-weight: 600; }
  .in {
    width: 100%; background: var(--bg); border: 1px solid var(--line); border-radius: 10px;
    color: var(--ink); padding: 0.48rem 0.7rem; font: inherit; font-size: 0.84rem;
    font-variant-numeric: tabular-nums; outline: none;
  }
  .in:focus { border-color: var(--amber); }
  .group { background: var(--surf); border: 1px solid var(--line); border-radius: 14px; overflow: hidden; }
  .grow {
    width: 100%; display: flex; align-items: center; justify-content: space-between; gap: 0.8rem;
    padding: 0.88rem 1rem; border-bottom: 1px solid var(--line);
    background: none; border-left: 0; border-right: 0; border-top: 0;
    color: var(--ink); font: inherit; font-size: 0.88rem; text-align: left;
  }
  .grow:last-child { border-bottom: 0; }
  .grow.clickable { cursor: pointer; }
  .grow.clickable:hover { background: var(--hover); }
  .subrow { padding-left: 1.25rem; background: rgb(0 0 0 / 16%); }
  .gcol { flex: 1; min-width: 0; display: grid; gap: 0.12rem; }
  .gcol small { color: var(--mut); font-size: 0.78rem; }
  .val { color: var(--mut); font-size: 0.84rem; }
  .rev { background: none; border: 0; color: var(--mut); font: inherit; font-size: 0.78rem; font-weight: 600; cursor: pointer; }
  .rev:hover { color: var(--bad); }
  .arr { color: var(--mut2); }
</style>
