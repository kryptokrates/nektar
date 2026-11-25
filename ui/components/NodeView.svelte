<script lang="ts">
  import type { State } from '../lib/types'
  import { operatorFetch } from '../lib/api'
  import { fmtSats, shortHost } from '../lib/format'
  import HubTab from './HubTab.svelte'

  interface Props {
    s: State
    live: boolean
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
    onConfirm: (title: string, detail: string) => Promise<boolean>
    onOpenDock: () => void
  }

  let { s, live, onRefresh, onToast, onConfirm, onOpenDock }: Props = $props()

  type Sub = 'policy' | 'network' | 'system'
  let sub = $state<Sub>('policy')

  // Editable Policy Profiles (Easy / Standard / Secure) — clicking a profile prefills limits & backend match confidence
  type Tier = 'easy' | 'standard' | 'secure'
  const tierDefaults: Record<
    Tier,
    {
      label: string
      desc: string
      role: 'buyer' | 'seller' | 'both'
      maxPerDeal: number
      maxPerDay: number
      zeroConfMax: number
      confidence: string
    }
  > = {
    easy: {
      label: 'Easy',
      desc: 'Higher caps · 0.80 match confidence',
      role: 'both',
      maxPerDeal: 5000,
      maxPerDay: 50000,
      zeroConfMax: 2500,
      confidence: '0.80'
    },
    standard: {
      label: 'Standard',
      desc: 'Balanced caps · 0.85 match confidence',
      role: 'both',
      maxPerDeal: 2000,
      maxPerDay: 20000,
      zeroConfMax: 1000,
      confidence: '0.85'
    },
    secure: {
      label: 'Secure',
      desc: 'Tight caps, mined only · 0.92 match',
      role: 'buyer',
      maxPerDeal: 250,
      maxPerDay: 2000,
      zeroConfMax: 0,
      confidence: '0.92'
    }
  }

  let selectedTier = $state<Tier>('standard')
  let polRole = $state<'buyer' | 'seller' | 'both'>('both')
  let maxPerDeal = $state('2000')
  let maxPerDay = $state('20000')
  let zeroConfMax = $state('1000')
  let domainsInput = $state('')

  let joinInput = $state('')
  let busy = $state(false)
  let showHubDeploy = $state(false)
  let showRawState = $state(false)

  const activePolicies = $derived(s.settings.policies.filter((p) => !p.revokedAt))

  function applyTier(t: Tier) {
    selectedTier = t
    const d = tierDefaults[t]
    polRole = d.role
    maxPerDeal = String(d.maxPerDeal)
    maxPerDay = String(d.maxPerDay)
    zeroConfMax = String(d.zeroConfMax)
  }

  function fmtMatchConf(ai?: Record<string, unknown>): string {
    const bp = typeof ai?.minConfidenceBp === 'number' ? ai.minConfidenceBp : 0
    if (bp <= 0) return ''
    return `≥ ${(bp / 10000).toFixed(2)} match`
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
        `Allow up to ${fmtSats(deal)} sats/deal and ${fmtSats(day)} sats/day (${tierDefaults[selectedTier].label}) on Mainnet?`
      )
      if (!ok) return
      body.confirm = 'I_UNDERSTAND'
    }
    busy = true
    try {
      const r = await operatorFetch('POST', '/v1/onboard/policy-preset', body, 'Save spending policy')
      if (!r) return
      const j = (await r.json().catch(() => ({}))) as { error?: string }
      if (!r.ok) onToast(j.error ?? `Save failed (${r.status})`, true)
      else {
        onToast(`Policy saved (${tierDefaults[selectedTier].label} · ≥ ${tierDefaults[selectedTier].confidence} match) ✓`)
        await onRefresh()
      }
    } finally {
      busy = false
    }
  }

  async function revokePolicy(id: string) {
    const ok = await onConfirm('Revoke Policy', `Revoke "${id}"? Autonomous trades under this rule will pause.`)
    if (!ok) return
    const r = await operatorFetch('DELETE', `/v1/policies/${encodeURIComponent(id)}`, undefined, 'Revoke policy')
    if (r && r.ok) {
      onToast(`Revoked ${id}`)
      await onRefresh()
    }
  }

  async function joinHost(u: string) {
    if (!u.trim()) return
    const r = await operatorFetch('POST', '/v1/hosts', { url: u.trim() }, 'Join Hub')
    if (!r) return
    const j = (await r.json().catch(() => ({}))) as { url?: string; error?: string }
    if (!r.ok) onToast(j.error ?? `Join failed (${r.status})`, true)
    else {
      joinInput = ''
      onToast(`Connected to ${shortHost(j.url ?? u)} ✓`)
      await onRefresh()
    }
  }

  async function leaveHost(u: string) {
    const r = await operatorFetch('DELETE', `/v1/hosts?url=${encodeURIComponent(u)}`, undefined, `Disconnect ${shortHost(u)}`)
    if (r && r.ok) {
      onToast(`Disconnected ${shortHost(u)}`)
      await onRefresh()
    }
  }
</script>

<div class="view">
  <header class="sticky-head">
    <div class="toprow">
      <h1>Node</h1>
      <button class="dock-btn" onclick={onOpenDock}>Dock Agent</button>
    </div>
    <div class="seg">
      <button class="s" class:on={sub === 'policy'} onclick={() => (sub = 'policy')}>Policy</button>
      <button class="s" class:on={sub === 'network'} onclick={() => (sub = 'network')}>Network</button>
      <button class="s" class:on={sub === 'system'} onclick={() => (sub = 'system')}>System</button>
    </div>
  </header>

  {#if sub === 'policy'}
    <div class="pad">
      <div class="glbl">Choose A Profile</div>
      <div class="tier-grid">
        {#each (['easy', 'standard', 'secure'] as const) as t}
          {@const d = tierDefaults[t]}
          <button class="tcard" class:on={selectedTier === t} onclick={() => applyTier(t)}>
            <div class="thead">
              <b>{d.label}</b>
              <span class="tcap">{fmtSats(d.maxPerDeal)}s</span>
            </div>
            <small>{d.desc}</small>
          </button>
        {/each}
      </div>

      <div class="glbl">Fine-Tune Limits</div>
      <div class="form-card">
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
            <span>Max per deal (sats)</span>
            <input class="in" type="number" min="1" bind:value={maxPerDeal} />
          </label>
          <label class="field">
            <span>Max per day (sats)</span>
            <input class="in" type="number" min="1" bind:value={maxPerDay} />
          </label>
          <label class="field">
            <span>Instant 0-conf max</span>
            <input class="in" type="number" min="0" bind:value={zeroConfMax} />
          </label>
        </div>

        <label class="field">
          <span>Allowed topics (optional, comma-separated)</span>
          <input class="in" bind:value={domainsInput} placeholder="Leave blank for any topic (e.g. ops, bsv, research)" />
        </label>

        <div class="factions">
          <span class="conf-note">Automatic match confidence: <b>{tierDefaults[selectedTier].confidence}</b></span>
          <button class="pri" onclick={savePolicy} disabled={busy}>{busy ? 'Saving…' : 'Save Policy'}</button>
        </div>
      </div>

      {#if activePolicies.length}
        <div class="glbl">Active Policies</div>
        <div class="group">
          {#each activePolicies as row}
            {@const conf = fmtMatchConf(row.policy?.aiMatching)}
            <div class="grow">
              <div class="gcol">
                <b>{row.policy?.roles?.join(' + ') || row.policyId}</b>
                {#if row.policy}
                  <small>
                    ≤ {fmtSats(row.policy.limits?.maxPerDeal ?? 0)} sats/deal · ≤ {fmtSats(row.policy.limits?.maxPerDay ?? 0)} sats/day
                    {#if (row.policy.limits?.zeroConfMax ?? 0) === 0}· mined only{/if}
                    {#if conf}· {conf}{/if}
                  </small>
                {/if}
              </div>
              <button class="rev" onclick={() => revokePolicy(row.policyId)}>Revoke</button>
            </div>
          {/each}
        </div>
      {/if}
    </div>
  {:else if sub === 'network'}
    <div class="pad">
      <div class="glbl">Connected Hubs</div>
      <div class="group">
        {#each s.net.hosts as h}
          <div class="grow">
            <span class="dot" class:ok={h.state === 'up'} class:bad={h.state === 'down'}></span>
            <div class="gcol">
              <b>{shortHost(h.url)}</b>
              <small>{h.state === 'up' ? 'Connected' : h.state}</small>
            </div>
            <button class="rev" onclick={() => leaveHost(h.url)}>Leave</button>
          </div>
        {:else}
          <div class="grow"><small>Not connected to any hub yet.</small></div>
        {/each}
      </div>

      <div class="jrow">
        <input class="in" bind:value={joinInput} placeholder="Paste a hub URL (https://…)" onkeydown={(e) => e.key === 'Enter' && joinHost(joinInput)} />
        <button class="pri" onclick={() => joinHost(joinInput)}>Join</button>
      </div>

      {#if s.net.seeds?.length}
        <div class="seeds">
          {#each s.net.seeds as seed}
            <button class="chip" onclick={() => joinHost(seed)}>+ {shortHost(seed)}</button>
          {/each}
        </div>
      {/if}

      <div class="glbl">Run Your Own Hub</div>
      <div class="group">
        <button class="grow clickable" onclick={() => (showHubDeploy = !showHubDeploy)}>
          <div class="gcol">
            <b>Deploy a Nektar Hub Node</b>
            <small>Single-binary relay with bounties &amp; NAT inbox</small>
          </div>
          <span class="arr">{showHubDeploy ? '▾' : '→'}</span>
        </button>
      </div>
      {#if showHubDeploy}
        <HubTab network={s.network} onJoinHub={joinHost} />
      {/if}
    </div>
  {:else}
    <div class="pad">
      <div class="glbl">Node Health</div>
      <div class="group">
        <div class="grow"><b>Status</b><span class="val" class:ok={live}>{live ? 'Live' : 'Offline'}</span></div>
        <div class="grow"><b>Network</b><span class="val">{s.network}</span></div>
        <div class="grow"><b>Durable Deals</b><span class="val" class:ok={s.automation?.rt.executor === 'restate'}>{s.automation?.rt.executor === 'restate' ? 'Restate Active' : 'Local Mode'}</span></div>
        <div class="grow"><b>Memories</b><span class="val">{s.memory.anchored} / {s.memory.records} anchored</span></div>
        <div class="grow"><b>Identity</b><span class="val mono">{s.pub}…</span></div>
      </div>

      <div class="glbl">Developer</div>
      <div class="group">
        <button class="grow clickable" onclick={() => (showRawState = !showRawState)}>
          <b>Inspect Raw State JSON</b>
          <span class="arr">{showRawState ? '▾' : '→'}</span>
        </button>
      </div>
      {#if showRawState}
        <pre class="raw">{JSON.stringify(s, null, 2)}</pre>
      {/if}
    </div>
  {/if}
</div>

<style>
  .view { display: flex; flex-direction: column; }
  .sticky-head {
    position: sticky; top: 0; z-index: 15;
    background: rgb(9 8 7 / 92%); backdrop-filter: blur(12px);
    border-bottom: 1px solid var(--line);
    padding: 0.9rem 1.25rem 0.75rem; display: grid; gap: 0.75rem;
  }
  .toprow { display: flex; align-items: center; justify-content: space-between; }
  h1 { margin: 0; font-size: 1.15rem; font-weight: 800; letter-spacing: -0.02em; }
  .dock-btn {
    background: transparent; border: 1px solid var(--line2); color: var(--ink);
    border-radius: 999px; padding: 0.34rem 0.85rem; font: inherit; font-size: 0.78rem; font-weight: 700; cursor: pointer;
  }
  .dock-btn:hover { border-color: var(--amber); color: var(--amber); }
  .seg {
    display: grid; grid-template-columns: repeat(3, 1fr); gap: 0.22rem;
    background: var(--surf); border: 1px solid var(--line); border-radius: 10px; padding: 0.22rem;
  }
  .s {
    background: none; border: 0; color: var(--mut); font: inherit; font-size: 0.8rem; font-weight: 700;
    padding: 0.42rem 0.25rem; border-radius: 8px; cursor: pointer;
  }
  .s.on { background: #1e1b18; color: var(--ink); }
  .pad { padding: 1rem 1.25rem 2.2rem; display: grid; gap: 0.9rem; }
  .glbl {
    font-size: 0.7rem; font-weight: 700; letter-spacing: 0.1em; text-transform: uppercase;
    color: var(--mut2); margin-top: 0.25rem;
  }
  .tier-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 0.6rem; }
  @media (max-width: 560px) {
    .tier-grid { grid-template-columns: 1fr; }
  }
  .tcard {
    background: var(--surf); border: 1px solid var(--line); border-radius: 14px;
    padding: 0.9rem 0.95rem; text-align: left; color: var(--ink); font: inherit;
    display: grid; gap: 0.28rem; align-content: start; cursor: pointer;
    transition: border-color 0.14s, background 0.14s;
  }
  .tcard:hover { border-color: var(--line2); background: var(--hover); }
  .tcard.on { border-color: var(--amber); background: rgb(247 153 27 / 7%); }
  .thead { display: flex; align-items: baseline; justify-content: space-between; gap: 0.3rem; }
  .tcard b { font-size: 0.9rem; }
  .tcap { font-size: 0.74rem; color: var(--amber); font-weight: 700; font-variant-numeric: tabular-nums; }
  .tcard small { color: var(--mut); font-size: 0.76rem; line-height: 1.38; }
  .form-card {
    background: var(--surf); border: 1px solid var(--line); border-radius: 14px;
    padding: 1rem; display: grid; gap: 0.85rem;
  }
  .role-row { display: flex; align-items: center; justify-content: space-between; gap: 0.75rem; flex-wrap: wrap; }
  .flbl { font-size: 0.82rem; font-weight: 600; color: var(--mut); }
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
  .field { display: grid; gap: 0.32rem; }
  .field span { font-size: 0.74rem; color: var(--mut); font-weight: 600; }
  .in {
    width: 100%; background: var(--bg); border: 1px solid var(--line); border-radius: 10px;
    color: var(--ink); padding: 0.5rem 0.72rem; font: inherit; font-size: 0.84rem;
    font-variant-numeric: tabular-nums; outline: none;
  }
  .in:focus { border-color: var(--amber); }
  .factions { display: flex; align-items: center; justify-content: space-between; gap: 0.75rem; flex-wrap: wrap; }
  .conf-note { font-size: 0.76rem; color: var(--mut); }
  .conf-note b { color: var(--ink); font-variant-numeric: tabular-nums; }
  .pri {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.5rem 1.2rem; font: inherit; font-size: 0.82rem; font-weight: 800; cursor: pointer;
  }
  .pri:disabled { opacity: 0.45; }
  .group { background: var(--surf); border: 1px solid var(--line); border-radius: 14px; overflow: hidden; }
  .grow {
    width: 100%; display: flex; align-items: center; justify-content: space-between; gap: 0.8rem;
    padding: 0.85rem 1rem; border-bottom: 1px solid var(--line);
    background: none; border-left: 0; border-right: 0; border-top: 0;
    color: var(--ink); font: inherit; font-size: 0.88rem; text-align: left;
  }
  .grow:last-child { border-bottom: 0; }
  .grow.clickable { cursor: pointer; }
  .grow.clickable:hover { background: var(--hover); }
  .gcol { flex: 1; min-width: 0; display: grid; gap: 0.12rem; }
  .gcol small { color: var(--mut); font-size: 0.78rem; }
  .val { color: var(--mut); font-size: 0.84rem; }
  .val.ok { color: var(--ok); font-weight: 600; }
  .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--mut2); flex-shrink: 0; }
  .dot.ok { background: var(--ok); }
  .dot.bad { background: var(--bad); }
  .rev { background: none; border: 0; color: var(--mut); font: inherit; font-size: 0.78rem; font-weight: 600; cursor: pointer; }
  .rev:hover { color: var(--bad); }
  .arr { color: var(--mut2); }
  .jrow { display: flex; gap: 0.5rem; }
  .seeds { display: flex; gap: 0.45rem; flex-wrap: wrap; }
  .chip {
    background: var(--surf); border: 1px solid var(--line); color: var(--mut);
    border-radius: 999px; padding: 0.28rem 0.7rem; font: inherit; font-size: 0.76rem; cursor: pointer;
  }
  .chip:hover { border-color: var(--amber); color: var(--amber); }
  .raw {
    background: var(--surf); border: 1px solid var(--line); border-radius: 12px;
    padding: 0.85rem; font-size: 0.72rem; max-height: 360px; overflow: auto; margin: 0;
  }
</style>
