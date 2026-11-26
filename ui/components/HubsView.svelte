<script lang="ts">
  import { onMount } from 'svelte'
  import type { HubCatalogEntry, State } from '../lib/types'
  import { operatorFetch } from '../lib/api'
  import { shortHost } from '../lib/format'
  import HubTab from './HubTab.svelte'

  interface Props {
    s: State
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
  }

  let { s, onRefresh, onToast }: Props = $props()

  let hubs = $state<HubCatalogEntry[]>([])
  let loading = $state(true)
  let refreshing = $state(false)
  let connectingAll = $state(false)
  let busyURL = $state('')
  let addOpen = $state(false)
  let addInput = $state('')
  let addInputEl = $state<HTMLInputElement | null>(null)
  let showDeploy = $state(false)

  const connectedCount = $derived(hubs.filter((h) => h.connected).length)
  const reachableCount = $derived(hubs.filter((h) => h.reachable && !h.networkMismatch).length)
  const quorumActive = $derived(hubs.filter((h) => h.connected && h.reachable).length >= 2)

  function prettyFee(preset?: string): string {
    if (!preset) return ''
    if (preset === 'free') return 'Free'
    if (preset === 'lightbsv_egress') return 'Egress'
    if (preset === 'hybrid') return 'Hybrid'
    return preset
  }

  function prettyTopic(raw: string): string {
    const clean = raw.replace(/^cluster:/, '').replace(/^#/, '').trim()
    if (!clean) return ''
    return clean
      .split(/[-_]+/)
      .map((w) => (w ? w[0].toUpperCase() + w.slice(1) : ''))
      .join(' ')
  }

  async function loadDirectory(force = false) {
    if (force) refreshing = true
    else if (hubs.length === 0) loading = true
    try {
      const r = await fetch(`/v1/hubs/directory${force ? '?refresh=1' : ''}`)
      if (r.ok) {
        const j = (await r.json()) as { hubs?: HubCatalogEntry[] }
        hubs = j.hubs ?? []
      }
    } catch {
      // Keep last known directory on transient error
    } finally {
      loading = false
      refreshing = false
    }
  }

  async function connectAll() {
    if (connectingAll) return
    connectingAll = true
    try {
      const r = await operatorFetch('POST', '/v1/hubs/connect-all', {}, 'Connect all reachable hubs')
      if (!r) return
      const j = (await r.json().catch(() => ({}))) as { connected?: string[]; hubs?: HubCatalogEntry[]; error?: string }
      if (!r.ok) {
        onToast(j.error ?? `Auto-connect failed (${r.status})`, true)
        return
      }
      if (j.hubs) hubs = j.hubs
      const n = j.connected?.length ?? 0
      onToast(n > 0 ? `Connected to ${n} live hub${n === 1 ? '' : 's'} ✓` : 'No matching reachable hubs found', n === 0)
      await onRefresh()
    } finally {
      connectingAll = false
    }
  }

  async function joinHub(rawURL: string) {
    const u = rawURL.trim()
    if (!u) return
    busyURL = u
    try {
      const r = await operatorFetch('POST', '/v1/hosts', { url: u }, `Connect to ${shortHost(u)}`)
      if (!r) return
      const j = (await r.json().catch(() => ({}))) as { url?: string; error?: string }
      if (!r.ok) {
        onToast(j.error ?? `Connect failed (${r.status})`, true)
        return
      }
      addInput = ''
      addOpen = false
      onToast(`Connected to ${shortHost(j.url ?? u)} ✓`)
      await Promise.all([onRefresh(), loadDirectory(true)])
    } finally {
      busyURL = ''
    }
  }

  async function leaveHub(u: string) {
    busyURL = u
    try {
      const r = await operatorFetch('DELETE', `/v1/hosts?url=${encodeURIComponent(u)}`, undefined, `Disconnect ${shortHost(u)}`)
      if (!r) return
      if (!r.ok) {
        onToast(`Disconnect failed (${r.status})`, true)
        return
      }
      onToast(`Disconnected ${shortHost(u)}`)
      await Promise.all([onRefresh(), loadDirectory(true)])
    } finally {
      busyURL = ''
    }
  }

  function toggleAdd() {
    addOpen = !addOpen
    if (addOpen) setTimeout(() => addInputEl?.focus(), 20)
  }

  onMount(() => {
    void loadDirectory(false)
  })
</script>

<div class="hubs-page">
  <!-- Pixel-Locked 54px Sticky Header -->
  <header class="topbar">
    <div class="tleft">
      <h1>Hubs</h1>
      <span class="net-pill">{s.network}</span>
      {#if !loading}
        <span class="mesh-stat" class:ok={quorumActive}>
          {connectedCount}/{hubs.length} connected{quorumActive ? ' · Quorum ✓' : ''}
        </span>
      {/if}
    </div>
    <div class="tright">
      <button class="ghost-btn" onclick={() => loadDirectory(true)} disabled={refreshing}>
        {refreshing ? 'Probing…' : 'Refresh'}
      </button>
      <button class="ghost-btn" class:open={addOpen} onclick={toggleAdd}>
        {addOpen ? 'Cancel' : '+ Add Hub'}
      </button>
      <button class="pri-btn" onclick={connectAll} disabled={connectingAll}>
        {connectingAll ? 'Connecting…' : 'Auto-Connect All'}
      </button>
    </div>
  </header>

  <!-- Inline + Add Hub bar (hidden unless clicked) -->
  {#if addOpen}
    <div class="add-row">
      <input
        bind:this={addInputEl}
        bind:value={addInput}
        placeholder="Paste any Hub domain or URL (e.g. hub.alice.com or https://hub1.nektar.run)"
        disabled={busyURL !== ''}
        onkeydown={(e) => {
          if (e.key === 'Enter') void joinHub(addInput)
          else if (e.key === 'Escape') addOpen = false
        }}
      />
      <button class="pri-btn" onclick={() => joinHub(addInput)} disabled={busyURL !== '' || !addInput.trim()}>
        {busyURL ? 'Joining…' : 'Connect'}
      </button>
    </div>
  {/if}

  <!-- Clean LightBSV Hub Directory -->
  <section class="dir">
    <div class="dir-head">
      <span>OVERLAY HUB DIRECTORY</span>
      <span class="dir-sub">
        {#if loading}
          Probing live hubs…
        {:else if reachableCount > 0}
          {reachableCount} reachable on {s.network}
        {:else}
          {hubs.length} listed · untrusted relays (every proof SPV-verified)
        {/if}
      </span>
    </div>

    {#if loading && hubs.length === 0}
      <div class="empty">Probing nektar.run fleet &amp; discovered hubs…</div>
    {:else if hubs.length === 0}
      <div class="empty">No hubs found yet. Click <b>+ Add Hub</b> to join any Nektar Hub.</div>
    {:else}
      <div class="list">
        {#each hubs as h (h.url)}
          {@const isBusy = busyURL === h.url}
          <div class="row" class:muted={h.networkMismatch}>
            <span
              class="dot"
              class:live={h.connected && h.reachable}
              class:ready={!h.connected && h.reachable && !h.networkMismatch}
              class:down={h.connected && !h.reachable}
              title={h.connected ? (h.reachable ? 'Connected & live' : 'Connected (unreachable)') : h.reachable ? 'Reachable' : 'Offline'}
            ></span>

            <div class="main">
              <div class="top-line">
                <b class="hname">{h.name}</b>
                {#if h.role}
                  <span class="role" class:spine={h.role === 'spine'}>{h.role === 'spine' ? 'Spine' : 'Leaf'}</span>
                {/if}
                {#if h.source === 'discovered'}
                  <span class="src-tag">Community</span>
                {/if}
                {#if h.networkMismatch}
                  <span class="warn-tag">{h.network || 'other net'}</span>
                {/if}
              </div>

              {#if h.topics?.length}
                <div class="topics">
                  {#each h.topics.slice(0, 4) as t}
                    {@const lbl = prettyTopic(t)}
                    {#if lbl}<span>{lbl}</span>{/if}
                  {/each}
                </div>
              {/if}
            </div>

            <div class="meta">
              {#if h.reachable && h.latencyMs > 0}
                <span class="lat">{h.latencyMs}ms</span>
              {:else if !h.networkMismatch}
                <span class="lat off">Offline</span>
              {/if}

              {#if prettyFee(h.feePreset)}
                <span class="fee">{prettyFee(h.feePreset)}</span>
              {/if}

              {#if h.connected}
                <button class="act connected" onclick={() => leaveHub(h.url)} disabled={isBusy} title="Click to disconnect">
                  {isBusy ? '…' : 'Connected ✓'}
                </button>
              {:else if h.networkMismatch}
                <span class="net-note">{h.network} only</span>
              {:else}
                <button class="act" onclick={() => joinHub(h.url)} disabled={isBusy}>
                  {isBusy ? '…' : 'Connect'}
                </button>
              {/if}
            </div>
          </div>
        {/each}
      </div>
    {/if}
  </section>

  <!-- Collapsible Run Your Own Hub Footer -->
  <section class="deploy-foot">
    <button class="deploy-toggle" onclick={() => (showDeploy = !showDeploy)}>
      <span>Run your own Nektar Hub</span>
      <span class="arr">{showDeploy ? '▾' : '→'}</span>
    </button>
    {#if showDeploy}
      <div class="deploy-body">
        <HubTab network={s.network} onJoinHub={joinHub} />
      </div>
    {/if}
  </section>
</div>

<style>
  .hubs-page {
    display: flex;
    flex-direction: column;
    min-height: 100vh;
  }

  /* Pixel-locked 54px Sticky Header */
  .topbar {
    position: sticky;
    top: 0;
    z-index: 20;
    height: 54px;
    box-sizing: border-box;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    padding: 0 1.25rem;
    background: rgb(9 8 7 / 92%);
    backdrop-filter: blur(12px);
    border-bottom: 1px solid var(--line);
  }
  .tleft { display: flex; align-items: center; gap: 0.65rem; min-width: 0; }
  h1 { margin: 0; font-size: 1.06rem; font-weight: 800; letter-spacing: -0.02em; }
  .net-pill {
    font-family: ui-monospace, monospace; font-size: 0.68rem; font-weight: 700;
    color: var(--mut); background: var(--surf); border: 1px solid var(--line);
    border-radius: 999px; padding: 0.12rem 0.5rem; text-transform: uppercase;
  }
  .mesh-stat { font-size: 0.76rem; color: var(--mut); font-weight: 600; white-space: nowrap; }
  .mesh-stat.ok { color: var(--ok); }
  .tright { display: flex; align-items: center; gap: 0.45rem; flex-shrink: 0; }

  .ghost-btn {
    background: none; color: var(--mut); border: 1px solid var(--line);
    border-radius: 999px; padding: 0.34rem 0.78rem; font: inherit;
    font-size: 0.76rem; font-weight: 700; cursor: pointer;
    transition: border-color 0.14s, color 0.14s;
  }
  .ghost-btn:hover, .ghost-btn.open { border-color: var(--line2); color: var(--ink); }
  .ghost-btn:disabled { opacity: 0.45; cursor: default; }

  .pri-btn {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.36rem 0.92rem; font: inherit; font-size: 0.76rem; font-weight: 800;
    cursor: pointer; transition: filter 0.14s;
  }
  .pri-btn:hover:not(:disabled) { filter: brightness(1.08); }
  .pri-btn:disabled { opacity: 0.4; cursor: default; }

  .add-row {
    display: flex; align-items: center; gap: 0.65rem;
    padding: 0.7rem 1.25rem;
    background: var(--surf);
    border-bottom: 1px solid var(--line);
  }
  .add-row input {
    flex: 1; min-width: 0; background: none; border: 0; color: var(--ink);
    font: inherit; font-size: 0.88rem; outline: none;
  }
  .add-row input::placeholder { color: var(--mut2); }

  /* Directory */
  .dir { display: flex; flex-direction: column; flex: 1; }
  .dir-head {
    display: flex; align-items: center; justify-content: space-between; gap: 1rem;
    padding: 0.72rem 1.25rem;
    border-bottom: 1px solid var(--line);
    font-size: 0.7rem; font-weight: 800; letter-spacing: 0.09em; color: var(--mut2);
  }
  .dir-sub { letter-spacing: 0; font-weight: 600; color: var(--mut); font-size: 0.74rem; }

  .list { display: flex; flex-direction: column; }
  .row {
    display: flex; align-items: center; gap: 0.85rem;
    padding: 0.88rem 1.25rem;
    border-bottom: 1px solid var(--line);
    transition: background 0.12s;
  }
  .row:hover { background: var(--hover); }
  .row.muted { opacity: 0.72; }

  .dot {
    width: 8px; height: 8px; border-radius: 50%;
    background: var(--mut2); flex-shrink: 0;
  }
  .dot.live { background: var(--ok); box-shadow: 0 0 8px rgb(76 194 107 / 55%); }
  .dot.ready { background: transparent; border: 2px solid var(--ok); box-sizing: border-box; }
  .dot.down { background: var(--bad); }

  .main { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 0.22rem; }
  .top-line { display: flex; align-items: center; gap: 0.45rem; flex-wrap: wrap; }
  .hname { font-size: 0.92rem; font-weight: 800; color: var(--ink); letter-spacing: -0.01em; }
  .role {
    font-size: 0.66rem; font-weight: 700; color: var(--mut);
    background: var(--surf); border: 1px solid var(--line);
    border-radius: 999px; padding: 0.06rem 0.46rem;
  }
  .role.spine { color: var(--amber); border-color: rgb(247 153 27 / 32%); background: rgb(247 153 27 / 10%); }
  .src-tag {
    font-size: 0.66rem; font-weight: 700; color: var(--ok);
    background: rgb(76 194 107 / 10%); border: 1px solid rgb(76 194 107 / 28%);
    border-radius: 999px; padding: 0.06rem 0.46rem;
  }
  .warn-tag {
    font-family: ui-monospace, monospace; font-size: 0.66rem; font-weight: 700;
    color: var(--mut2); background: var(--bg); border: 1px solid var(--line);
    border-radius: 999px; padding: 0.06rem 0.46rem;
  }

  .topics { display: flex; align-items: center; gap: 0.4rem; flex-wrap: wrap; }
  .topics span { font-size: 0.74rem; color: var(--mut); }
  .topics span:not(:last-child)::after { content: '·'; margin-left: 0.4rem; color: var(--line2); }

  .meta { display: flex; align-items: center; gap: 0.7rem; flex-shrink: 0; }
  .lat {
    font-family: ui-monospace, monospace; font-size: 0.74rem; font-weight: 700;
    color: var(--ok); font-variant-numeric: tabular-nums;
  }
  .lat.off { color: var(--mut2); font-weight: 600; }
  .fee {
    font-size: 0.72rem; font-weight: 600; color: var(--mut);
    background: var(--surf); border: 1px solid var(--line);
    border-radius: 999px; padding: 0.12rem 0.5rem;
  }
  .act {
    background: transparent; color: var(--ink); border: 1px solid var(--line2);
    border-radius: 999px; padding: 0.3rem 0.82rem; font: inherit;
    font-size: 0.75rem; font-weight: 700; cursor: pointer;
    transition: border-color 0.14s, color 0.14s, background 0.14s;
  }
  .act:hover:not(:disabled) { border-color: var(--amber); color: var(--amber); }
  .act.connected {
    color: var(--ok); border-color: rgb(76 194 107 / 32%); background: rgb(76 194 107 / 8%);
  }
  .act.connected:hover:not(:disabled) {
    color: var(--bad); border-color: rgb(229 83 75 / 40%); background: rgb(229 83 75 / 8%);
  }
  .act:disabled { opacity: 0.4; cursor: default; }
  .net-note { font-size: 0.72rem; color: var(--mut2); font-family: ui-monospace, monospace; }

  .empty {
    padding: 3rem 1.25rem;
    text-align: center;
    color: var(--mut);
    font-size: 0.86rem;
  }

  /* Quiet Collapsible Deploy Footer */
  .deploy-foot { border-top: 1px solid var(--line); }
  .deploy-toggle {
    width: 100%; display: flex; align-items: center; justify-content: space-between;
    background: none; border: 0; padding: 0.85rem 1.25rem;
    color: var(--mut); font: inherit; font-size: 0.82rem; font-weight: 700;
    cursor: pointer; transition: background 0.12s, color 0.12s;
  }
  .deploy-toggle:hover { background: var(--hover); color: var(--ink); }
  .deploy-body { padding: 0 1.25rem 2rem; }
</style>
