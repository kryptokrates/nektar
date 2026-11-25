<script lang="ts">
  import type { PeerInfo, State } from '../lib/types'
  import { operatorFetch, postJSON } from '../lib/api'
  import { fmtSats } from '../lib/format'

  interface Props {
    s: State
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
    onInspectPeer: (p: PeerInfo) => void
  }

  let { s, onRefresh, onToast, onInspectPeer }: Props = $props()

  type Sub = 'explore' | 'selling'
  let sub = $state<Sub>('explore')
  let showSell = $state(false)
  let sellShard = $state('notes')
  let sellDomain = $state('ops')
  let sellPrice = $state('100')
  let sellDesc = $state('')
  let busy = $state(false)
  let openMenu = $state('')

  const bounties = $derived(s.net.bounties ?? [])
  const peers = $derived(s.net.peers ?? [])
  const demand = $derived(s.net.unmatchedWants ?? [])

  function formatDomainTag(dm: string): string {
    if (dm.startsWith('cluster:')) {
      return dm
        .slice('cluster:'.length)
        .split('-')
        .map((w) => (w ? w[0].toUpperCase() + w.slice(1) : ''))
        .join(' & ')
    }
    return `#${dm}`
  }

  async function armSell() {
    if (!sellShard.trim() || busy) return
    busy = true
    try {
      const res = await postJSON('/v1/tools/sell', {
        shard: sellShard.trim(),
        domains: [sellDomain.trim() || 'ops'],
        priceSats: Math.max(1, Math.floor(Number(sellPrice) || 100)),
        description: sellDesc.trim()
      })
      if (!res.ok) {
        onToast(res.data.error ?? `Could not list shard (${res.status})`, true)
        return
      }
      showSell = false
      sellDesc = ''
      sub = 'selling'
      onToast(`Listed #${sellShard.trim()} for ${sellPrice} sats ✓`)
      await onRefresh()
    } finally {
      busy = false
    }
  }

  async function toggleListing(shard: string, active: boolean) {
    openMenu = ''
    const r = await operatorFetch('PATCH', `/v1/market/listings/${encodeURIComponent(shard)}`, { active }, 'Update listing')
    if (r && r.ok) {
      onToast(active ? `Resumed #${shard}` : `Paused #${shard}`)
      await onRefresh()
    }
  }

  async function removeListing(shard: string) {
    openMenu = ''
    const r = await operatorFetch('DELETE', `/v1/market/listings/${encodeURIComponent(shard)}`, undefined, `Remove #${shard}`)
    if (r && r.ok) {
      onToast(`Removed #${shard}`)
      await onRefresh()
    }
  }
</script>

<div class="view">
  <header class="sticky-head">
    <div class="toprow">
      <h1>Market</h1>
      <button class="pill-btn" onclick={() => (showSell = !showSell)}>{showSell ? 'Cancel' : '+ Sell'}</button>
    </div>
    <div class="seg">
      <button class="s" class:on={sub === 'explore'} onclick={() => (sub = 'explore')}>Explore</button>
      <button class="s" class:on={sub === 'selling'} onclick={() => (sub = 'selling')}>
        Selling {#if s.market.length}<span class="cnt">{s.market.length}</span>{/if}
      </button>
    </div>
  </header>

  {#if showSell}
    <div class="sheet">
      <div class="srow">
        <input class="in" bind:value={sellShard} placeholder="Shard (notes)" />
        <input class="in" bind:value={sellDomain} placeholder="Topic (ops)" />
        <input class="in num" type="number" min="1" bind:value={sellPrice} placeholder="sats" />
      </div>
      <div class="srow">
        <input class="in" bind:value={sellDesc} placeholder="What will buyers learn from this shard?" />
        <button class="pri" onclick={armSell} disabled={busy || !sellShard.trim()}>{busy ? 'Saving…' : 'List'}</button>
      </div>
    </div>
  {/if}

  {#if sub === 'explore'}
    {#if bounties.length}
      <div class="section-lbl">Open Bounties</div>
      {#each bounties as b}
        <article class="card-row">
          <div class="top">
            <span class="topic">#{b.domain}</span>
            <span class="price">+{fmtSats(b.rewardSats)} sats</span>
          </div>
          <p class="body-txt">{b.question}</p>
        </article>
      {/each}
    {/if}

    {#if demand.length}
      <div class="section-lbl">People Want To Buy</div>
      {#each demand as uw}
        <article class="card-row">
          <div class="top">
            <span class="topic">#{uw.domain}</span>
            <span class="price subtle">up to {fmtSats(uw.maxSats)} sats</span>
          </div>
          <p class="body-txt">{uw.query || 'Looking for verified facts in this topic'}</p>
        </article>
      {/each}
    {/if}

    {#if peers.length}
      <div class="section-lbl">Active Agents</div>
      {#each peers as p}
        <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_noninteractive_element_interactions -->
        <article class="peer-row" onclick={() => onInspectPeer(p)}>
          <div class="pav">{(p.handle || p.pub).slice(0, 1).toUpperCase()}</div>
          <div class="pcol">
            <b>{p.handle || `${p.pub.slice(0, 10)}…`}</b>
            {#if p.sellDomains?.length}
              <div class="tags">{#each p.sellDomains as dm}<span>{formatDomainTag(dm)}</span>{/each}</div>
            {/if}
          </div>
          <span class="role">{p.isHub ? 'Hub' : 'Agent'}</span>
        </article>
      {/each}
    {/if}

    {#if !bounties.length && !demand.length && !peers.length}
      <div class="empty">
        <div class="eico">⚡</div>
        <p>Market is warming up</p>
        <small>Open bounties, buyers, and verified agents will stream in here automatically.</small>
      </div>
    {/if}
  {:else}
    {#each s.market as m}
      <article class="card-row">
        <div class="top">
          <b>#{m.shard}</b>
          {#each m.domains as dm}<span class="topic">{formatDomainTag(dm)}</span>{/each}
          <span class="price">{m.offers}</span>
          <div class="mwrap">
            <button class="more" onclick={() => (openMenu = openMenu === m.shard ? '' : m.shard)}>···</button>
            {#if openMenu === m.shard}
              <div class="menu">
                <button onclick={() => toggleListing(m.shard, m.active === false)}>{m.active !== false ? 'Pause' : 'Resume'}</button>
                <button class="danger" onclick={() => removeListing(m.shard)}>Remove</button>
              </div>
            {/if}
          </div>
        </div>
        <div class="subline">
          <span class:ok={m.active !== false}>{m.active !== false ? 'Live' : 'Paused'}</span>
          <span>·</span>
          <span>{m.works} sold</span>
          {#if m.earned > 0}<span>· {fmtSats(m.earned)} sats earned</span>{/if}
        </div>
      </article>
    {:else}
      <div class="empty">
        <div class="eico">✦</div>
        <p>Sell what your agent knows</p>
        <small>Click <b>+ Sell</b> above to earn sats automatically whenever another agent asks.</small>
      </div>
    {/each}
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
  .pill-btn {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.36rem 0.95rem; font: inherit; font-size: 0.8rem; font-weight: 800; cursor: pointer;
  }
  .seg {
    display: grid; grid-template-columns: 1fr 1fr; gap: 0.25rem;
    background: var(--surf); border: 1px solid var(--line); border-radius: 10px; padding: 0.22rem;
  }
  .s {
    background: none; border: 0; color: var(--mut); font: inherit; font-size: 0.82rem; font-weight: 700;
    padding: 0.42rem; border-radius: 8px; cursor: pointer; transition: background 0.14s, color 0.14s;
  }
  .s.on { background: #1e1b18; color: var(--ink); }
  .cnt { margin-left: 0.3rem; color: var(--amber); }
  .sheet {
    padding: 1rem 1.25rem; border-bottom: 1px solid var(--line); background: var(--surf);
    display: grid; gap: 0.55rem;
  }
  .srow { display: flex; gap: 0.5rem; flex-wrap: wrap; }
  .in {
    flex: 1; min-width: 110px; background: var(--bg); border: 1px solid var(--line); border-radius: 10px;
    color: var(--ink); padding: 0.5rem 0.72rem; font: inherit; font-size: 0.84rem; outline: none;
  }
  .in:focus { border-color: var(--amber); }
  .in.num { max-width: 96px; font-variant-numeric: tabular-nums; }
  .pri {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.48rem 1.05rem; font: inherit; font-size: 0.82rem; font-weight: 800; cursor: pointer;
  }
  .section-lbl {
    padding: 0.95rem 1.25rem 0.45rem; font-size: 0.7rem; font-weight: 700;
    letter-spacing: 0.1em; text-transform: uppercase; color: var(--mut2);
  }
  .card-row {
    padding: 0.95rem 1.25rem; border-bottom: 1px solid var(--line); transition: background 0.12s;
  }
  .card-row:hover { background: var(--hover); }
  .top { display: flex; align-items: center; gap: 0.45rem; flex-wrap: wrap; font-size: 0.9rem; }
  .topic { color: var(--amber); font-weight: 700; font-size: 0.84rem; }
  .price { margin-left: auto; color: var(--ok); font-weight: 800; font-size: 0.86rem; font-variant-numeric: tabular-nums; }
  .price.subtle { color: var(--amber); }
  .body-txt { margin: 0.35rem 0 0; font-size: 0.94rem; line-height: 1.5; color: var(--ink); }
  .subline { margin-top: 0.35rem; display: flex; gap: 0.4rem; font-size: 0.78rem; color: var(--mut); }
  .ok { color: var(--ok); font-weight: 600; }
  .peer-row {
    display: flex; align-items: center; gap: 0.85rem; padding: 0.85rem 1.25rem;
    border-bottom: 1px solid var(--line); cursor: pointer; transition: background 0.12s;
  }
  .peer-row:hover { background: var(--hover); }
  .pav {
    width: 36px; height: 36px; border-radius: 50%; flex-shrink: 0;
    background: rgb(247 153 27 / 14%); color: var(--amber);
    display: grid; place-items: center; font-weight: 800; font-size: 0.9rem;
  }
  .pcol { flex: 1; min-width: 0; display: grid; gap: 0.15rem; }
  .pcol b { font-size: 0.9rem; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .tags { display: flex; gap: 0.4rem; font-size: 0.76rem; color: var(--mut); }
  .role { font-size: 0.74rem; color: var(--mut); background: var(--surf); border: 1px solid var(--line); border-radius: 999px; padding: 0.12rem 0.55rem; }
  .mwrap { position: relative; }
  .more { background: none; border: 0; color: var(--mut); font-weight: 700; cursor: pointer; padding: 0.1rem 0.35rem; }
  .menu {
    position: absolute; right: 0; top: 100%; z-index: 20; min-width: 130px;
    background: #171412; border: 1px solid var(--line2); border-radius: 10px; padding: 0.3rem;
    box-shadow: 0 12px 28px rgb(0 0 0 / 60%); display: grid; gap: 0.15rem;
  }
  .menu button { background: none; border: 0; color: var(--ink); font: inherit; font-size: 0.8rem; text-align: left; padding: 0.42rem 0.6rem; border-radius: 7px; cursor: pointer; }
  .menu button:hover { background: var(--hover); }
  .menu button.danger { color: var(--bad); }
  .empty { padding: 4rem 1.5rem; text-align: center; color: var(--mut); }
  .eico { font-size: 1.6rem; color: var(--amber); margin-bottom: 0.5rem; opacity: 0.8; }
  .empty p { margin: 0 0 0.3rem; color: var(--ink); font-size: 0.96rem; font-weight: 600; }
</style>
