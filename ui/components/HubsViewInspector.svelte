<script lang="ts">
  import type { HubCatalogEntry } from '../lib/types'
  import { shortHost } from '../lib/format'
  import { isFreeHub, prettyHubTopic } from './HubsViewModel'

  interface Props {
    hub: HubCatalogEntry
    busyURL: string
    activeTopic: string
    onJoinHub: (url: string) => void
    onLeaveHub: (url: string) => void
    onToggleTopic: (slug: string) => void
    onSelectHub: (url: string) => void
  }

  let { hub: h, busyURL, activeTopic, onJoinHub, onLeaveHub, onToggleTopic, onSelectHub }: Props = $props()

  const isBusy = $derived(busyURL === h.url)
</script>

<section class="inspector" aria-label="Selected Hub Details">
  <div class="insp-top">
    <div class="insp-id">
      <span class="dot" class:live={h.connected && h.reachable} class:ready={!h.connected && h.reachable && !h.networkMismatch}></span>
      <b class="hname">{h.name}</b>
      {#if h.role}
        <span class="role" class:spine={h.role === 'spine'}>{h.role === 'spine' ? 'Spine' : 'Leaf'}</span>
      {/if}
      {#if h.reachable && h.latencyMs > 0}
        <span class="ping">{h.latencyMs}ms</span>
      {:else}
        <span class="ping off">Offline</span>
      {/if}
    </div>

    <div class="insp-act">
      {#if h.connected}
        <button class="act connected" onclick={() => onLeaveHub(h.url)} disabled={isBusy} title="Click to disconnect">
          {isBusy ? '…' : 'Connected ✓'}
        </button>
      {:else if h.networkMismatch}
        <span class="net-note">{h.network} only</span>
      {:else}
        <button class="act" onclick={() => onJoinHub(h.url)} disabled={isBusy}>
          {isBusy ? '…' : 'Connect'}
        </button>
      {/if}
    </div>
  </div>

  <div class="insp-grid">
    <!-- Exact Satoshi Relay & Egress Fees -->
    <div class="icell">
      <span class="ilbl">RELAY FEES</span>
      {#if isFreeHub(h)}
        <div class="fee-val ok">Free Relay · 0 sats</div>
      {:else if h.fees}
        <div class="fee-strip">
          <span><b>{h.fees.wantSats}s</b> want</span>
          <span><b>{h.fees.offerSats}s</b> offer</span>
          <span><b>{h.fees.bboxSats}s</b> NAT</span>
          <span><b>{h.fees.egressSatsPer100}s</b>/100 egress</span>
        </div>
      {:else}
        <div class="fee-val">{h.feePreset || 'Standard'}</div>
      {/if}
    </div>

    <!-- Hosted Topics -->
    <div class="icell">
      <span class="ilbl">TOPICS HOSTED</span>
      <div class="itopics">
        {#if h.topics?.length}
          {#each h.topics as t}
            {@const slug = t.replace(/^cluster:/, '').toLowerCase()}
            <button
              class="tpill"
              class:on={activeTopic === slug}
              onclick={() => onToggleTopic(slug)}
            >
              {prettyHubTopic(t)}
            </button>
          {/each}
        {:else}
          <span class="mut-txt">All general lanes</span>
        {/if}
      </div>
    </div>

    <!-- Federation Links -->
    <div class="icell">
      <span class="ilbl">CONNECTIONS</span>
      <div class="iconn">
        {#if h.peersCount && h.peersCount > 0}
          <span class="cval ok">{h.peersCount} live peers</span>
        {/if}
        {#if h.linkedHubs?.length}
          {#each h.linkedHubs.slice(0, 2) as lh}
            <button class="link-chip" onclick={() => onSelectHub(lh)}>↔ {shortHost(lh)}</button>
          {/each}
        {:else}
          <span class="mut-txt">Direct overlay relay</span>
        {/if}
      </div>
    </div>
  </div>
</section>

<style>
  .inspector {
    padding: 1.05rem 1.25rem 1.25rem;
    display: flex; flex-direction: column; gap: 0.9rem;
    flex: 1;
  }
  .insp-top {
    display: flex; align-items: center; justify-content: space-between; gap: 1rem;
  }
  .insp-id { display: flex; align-items: center; gap: 0.55rem; flex-wrap: wrap; }
  .dot {
    width: 8px; height: 8px; border-radius: 50%; background: var(--mut2); flex-shrink: 0;
  }
  .dot.live { background: var(--ok); box-shadow: 0 0 8px rgb(76 194 107 / 55%); }
  .dot.ready { background: transparent; border: 2px solid var(--ok); box-sizing: border-box; }
  .hname { font-size: 1.02rem; font-weight: 800; color: var(--ink); letter-spacing: -0.015em; }
  .role {
    font-size: 0.68rem; font-weight: 700; color: var(--mut);
    background: var(--surf); border: 1px solid var(--line);
    border-radius: 999px; padding: 0.08rem 0.5rem;
  }
  .role.spine { color: var(--amber); border-color: rgb(247 153 27 / 32%); background: rgb(247 153 27 / 10%); }
  .ping { font-family: ui-monospace, monospace; font-size: 0.78rem; font-weight: 800; color: var(--ok); }
  .ping.off { color: var(--mut2); }

  .act {
    background: transparent; color: var(--ink); border: 1px solid var(--line2);
    border-radius: 999px; padding: 0.32rem 0.88rem; font: inherit;
    font-size: 0.76rem; font-weight: 700; cursor: pointer;
    transition: border-color 0.14s, color 0.14s, background 0.14s;
  }
  .act:hover:not(:disabled) { border-color: var(--amber); color: var(--amber); }
  .act.connected { color: var(--ok); border-color: rgb(76 194 107 / 32%); background: rgb(76 194 107 / 8%); }
  .act.connected:hover:not(:disabled) { color: var(--bad); border-color: rgb(229 83 75 / 40%); background: rgb(229 83 75 / 8%); }
  .act:disabled { opacity: 0.4; cursor: default; }
  .net-note { font-size: 0.74rem; color: var(--mut2); font-family: ui-monospace, monospace; }

  .insp-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
    gap: 0.75rem;
  }
  .icell {
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 14px;
    padding: 0.85rem 1rem;
    display: flex; flex-direction: column; gap: 0.45rem;
  }
  .ilbl {
    font-size: 0.66rem; font-weight: 800; letter-spacing: 0.1em; color: var(--mut2);
  }
  .fee-val { font-size: 0.88rem; font-weight: 700; color: var(--ink); }
  .fee-val.ok { color: var(--ok); }
  .fee-strip {
    display: flex; align-items: center; gap: 0.65rem; flex-wrap: wrap;
    font-size: 0.8rem; color: var(--mut);
  }
  .fee-strip b { font-family: ui-monospace, monospace; color: var(--amber); font-weight: 800; }
  .itopics, .iconn { display: flex; align-items: center; gap: 0.4rem; flex-wrap: wrap; }
  .tpill {
    background: var(--bg); border: 1px solid var(--line); border-radius: 999px;
    padding: 0.16rem 0.58rem; color: var(--ink); font: inherit;
    font-size: 0.75rem; font-weight: 600; cursor: pointer;
  }
  .tpill:hover, .tpill.on { border-color: var(--amber); color: var(--amber); }
  .link-chip {
    background: rgb(247 153 27 / 8%); border: 1px solid rgb(247 153 27 / 26%);
    border-radius: 999px; padding: 0.16rem 0.58rem; color: var(--amber);
    font: inherit; font-size: 0.74rem; font-weight: 700; cursor: pointer;
  }
  .link-chip:hover { background: rgb(247 153 27 / 16%); }
  .cval.ok { font-size: 0.8rem; font-weight: 700; color: var(--ok); }
  .mut-txt { font-size: 0.78rem; color: var(--mut2); }
</style>
