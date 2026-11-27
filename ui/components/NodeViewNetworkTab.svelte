<script lang="ts">
  import type { State } from '../lib/types'
  import { operatorFetch } from '../lib/api'
  import { shortHost } from '../lib/format'
  import HubTab from './HubTab.svelte'

  interface Props {
    s: State
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
  }

  let { s, onRefresh, onToast }: Props = $props()

  let joinInput = $state('')
  let showHubDeploy = $state(false)

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

<style>
  .pad { padding: 1.35rem 1.5rem 2.4rem; display: grid; gap: 1rem; width: 100%; }
  .glbl {
    font-size: 0.7rem; font-weight: 700; letter-spacing: 0.1em; text-transform: uppercase;
    color: var(--mut2); margin-top: 0.15rem;
  }
  .in {
    width: 100%; background: var(--bg); border: 1px solid var(--line); border-radius: 10px;
    color: var(--ink); padding: 0.48rem 0.7rem; font: inherit; font-size: 0.84rem;
    font-variant-numeric: tabular-nums; outline: none;
  }
  .in:focus { border-color: var(--amber); }
  .pri {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.5rem 1.2rem; font: inherit; font-size: 0.82rem; font-weight: 800; cursor: pointer;
  }
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
  .gcol { flex: 1; min-width: 0; display: grid; gap: 0.12rem; }
  .gcol small { color: var(--mut); font-size: 0.78rem; }
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
</style>
