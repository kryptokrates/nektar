<script lang="ts">
  import type { ListedShard, MarketTapeEvent, PeerInfo } from '../lib/types'
  import { operatorFetch, postJSON } from '../lib/api'
  import { fmtSats } from '../lib/format'
  import { fetchTopicPills, type TopicPill } from './MarketHomeHelpers'

  interface Props {
    funded: boolean
    sellerPolicyReady?: boolean
    sellText?: string
    sellDomain?: string
    sellPrice?: string
    sellInputEl?: HTMLTextAreaElement | null
    listings: ListedShard[]
    peers: PeerInfo[]
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
    onLocalPulse: (event: string, tape?: MarketTapeEvent) => void
    onPickBuy: (domain: string, priceSats: number, hint?: string) => void
    onInspectPeer: (p: PeerInfo) => void
  }

  let {
    funded,
    sellerPolicyReady = true,
    sellText = $bindable(''),
    sellDomain = $bindable(''),
    sellPrice = $bindable('50'),
    sellInputEl = $bindable(null),
    listings,
    peers,
    onRefresh,
    onToast,
    onLocalPulse,
    onPickBuy,
    onInspectPeer
  }: Props = $props()

  let sellBusy = $state(false)
  let sellStage = $state('')
  let sellPills = $state<TopicPill[]>([])
  let openMenu = $state('')

  $effect(() => {
    const h = sellText.trim()
    if (h.length < 3) {
      sellPills = []
      return
    }
    let cancelled = false
    const t = setTimeout(async () => {
      try {
        const res = await fetchTopicPills(h)
        if (!cancelled) sellPills = res
      } catch {}
    }, 280)
    return () => {
      cancelled = true
      clearTimeout(t)
    }
  })

  async function submitSell() {
    const body = sellText.trim()
    if (!body || sellBusy) return
    sellBusy = true
    sellStage = ''
    try {
      const inferred = sellPills.map((p) => p.domainTag).filter(Boolean)
      const cleanDom = sellDomain.trim().replace(/^#/, '')
      const topic = cleanDom || sellPills[0]?.shardSlug || 'notes'
      const domains = cleanDom ? [cleanDom, ...inferred.filter((d) => d !== cleanDom)] : inferred.length > 0 ? inferred : [topic]
      const price = Math.max(1, Math.floor(Number(sellPrice) || 50))

      sellStage = funded ? 'Anchoring…' : 'Saving…'
      const res = await postJSON<{ anchorTxid?: string }>('/v1/remember', {
        shard: topic,
        text: body,
        autoAnchor: funded
      })
      if (!res.ok) {
        onToast(res.data.error ?? `Save failed (${res.status})`, true)
        return
      }

      sellStage = 'Listing…'
      const sellRes = await postJSON<{ quotingReady?: boolean; blockedReasons?: string[] }>('/v1/tools/sell', {
        shard: topic,
        domains,
        priceSats: price,
        description: body.slice(0, 140)
      })
      if (!sellRes.ok) {
        onToast(sellRes.data.error ?? `Listing failed (${sellRes.status})`, true)
        await onRefresh()
        return
      }

      onLocalPulse('tool:sell', {
        id: `local-sell-${Date.now()}`,
        kind: 'offer',
        side: 'sell',
        domains,
        summary: body.slice(0, 120),
        sats: price,
        shard: topic,
        status: sellRes.data.quotingReady === false ? 'pending' : 'listed',
        isSelf: true,
        at: Date.now()
      })
      sellText = ''
      sellDomain = ''
      const blockedReasons = (sellRes.data.blockedReasons ?? []).filter(Boolean)
      if (sellRes.data.quotingReady === false || blockedReasons.length > 0) {
        onToast(
          `Listed #${topic}, but auto-quoting is blocked: ${blockedReasons.join('; ') || 'approve a Seller policy in Settings → Policy'}`,
          true
        )
      } else if (!sellerPolicyReady) {
        onToast(`Listed #${topic} · approve a Seller policy in Settings → Policy to auto-quote buyers`, true)
      } else {
        onToast(`Listed #${topic} · ${fmtSats(price)} sats ✓`)
      }
      await onRefresh()
    } catch (e) {
      onToast(String(e), true)
    } finally {
      sellBusy = false
      sellStage = ''
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

<!-- RIGHT: SELL -->
<div class="col col-sell">
  <div class="col-head">
    <span class="side-dot sell"></span>
    <h2>Sell</h2>
  </div>

  <div class="order-box sell-box">
    <textarea
      bind:this={sellInputEl}
      bind:value={sellText}
      rows="2"
      placeholder="Paste verified data to anchor &amp; sell…"
      disabled={sellBusy}
      onkeydown={(e) => {
        if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
          e.preventDefault()
          void submitSell()
        }
      }}
    ></textarea>

    <div class="order-bar">
      <div class="order-opts">
        <label class="chip-in">
          <input class="num-in" type="number" min="1" bind:value={sellPrice} aria-label="Ask satoshis" />
          <span>sats</span>
        </label>
        <label class="chip-in topic-in">
          <span>#</span>
          <input bind:value={sellDomain} placeholder="topic" aria-label="Topic" />
        </label>
        {#each sellPills as p}
          <span class="auto-pill sell">{p.text}</span>
        {/each}
      </div>
      <button class="act-btn sell" onclick={submitSell} disabled={sellBusy || !sellText.trim()}>
        {sellBusy ? sellStage || '…' : 'Sell'}
      </button>
    </div>
  </div>

  {#if listings.length || peers.length}
    <div class="book-list">
      {#each listings as m}
        {@const statusLabel =
          m.active === false ? 'Paused' : !sellerPolicyReady ? 'Pending seller policy' : !funded ? 'Unfunded' : 'Live'}
        {@const statusWarn = m.active !== false && (!sellerPolicyReady || !funded)}
        <article class="book-row self-row">
          <div class="row-top">
            <span class="tag sell">#{m.shard}</span>
            <span class="stat-inline" class:warn={statusWarn}>{statusLabel} · {m.works} sold</span>
            <span class="price sell">{m.offers}</span>
            <div class="mwrap">
              <button class="more" onclick={() => (openMenu = openMenu === m.shard ? '' : m.shard)} aria-label="Options">···</button>
              {#if openMenu === m.shard}
                <div class="menu">
                  <button onclick={() => toggleListing(m.shard, m.active === false)}>
                    {m.active !== false ? 'Pause' : 'Resume'}
                  </button>
                  <button class="danger" onclick={() => removeListing(m.shard)}>Remove</button>
                </div>
              {/if}
            </div>
          </div>
        </article>
      {/each}

      {#each peers as p}
        {@const firstDom = p.sellDomains?.[0] ?? ''}
        <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_noninteractive_element_interactions -->
        <article class="book-row peer-row" onclick={() => onInspectPeer(p)}>
          <div class="row-top">
            <b class="pname">{p.handle || `${p.pub.slice(0, 10)}…`}</b>
            {#if firstDom}
              <span class="stat-inline">#{firstDom.replace(/^cluster:/, '')}</span>
              <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
              <button
                class="cross-btn to-buy"
                onclick={(e) => {
                  e.stopPropagation()
                  onPickBuy(firstDom.replace(/^cluster:/, ''), 100)
                }}
              >
                ← Buy
              </button>
            {/if}
          </div>
        </article>
      {/each}
    </div>
  {/if}
</div>

<style>
  .col {
    padding: 1.15rem 1.25rem 1.4rem;
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
    min-width: 0;
  }
  .col-head { display: flex; align-items: center; gap: 0.5rem; }
  .col-head h2 { margin: 0; font-size: 0.94rem; font-weight: 800; letter-spacing: -0.01em; }
  .side-dot { width: 8px; height: 8px; border-radius: 50%; flex-shrink: 0; }
  .side-dot.sell { background: var(--amber); }

  /* Clean Dark Order Boxes */
  .order-box {
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 13px;
    padding: 0.8rem 0.95rem;
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
    transition: border-color 0.15s;
  }
  .sell-box:focus-within { border-color: rgb(247 153 27 / 60%); }
  .order-box textarea {
    width: 100%; background: transparent; border: 0; color: var(--ink);
    font: inherit; font-size: 0.94rem; line-height: 1.45; resize: none; outline: none; padding: 0;
  }
  .order-box textarea::placeholder { color: var(--mut2); }
  .order-bar {
    display: flex; align-items: center; justify-content: space-between; gap: 0.5rem; flex-wrap: wrap;
    padding-top: 0.4rem; border-top: 1px solid rgb(255 255 255 / 4%);
  }
  .order-opts { display: flex; align-items: center; gap: 0.4rem; flex-wrap: wrap; }
  .chip-in {
    display: inline-flex; align-items: center; gap: 0.25rem;
    background: var(--bg); border: 1px solid var(--line); border-radius: 999px;
    padding: 0.22rem 0.62rem; font-size: 0.76rem; color: var(--mut);
  }
  .chip-in input {
    background: transparent; border: 0; color: var(--ink);
    font: inherit; font-size: 0.78rem; font-weight: 700; outline: none;
  }
  .chip-in .num-in {
    width: 46px;
    font-family: ui-monospace, monospace;
    font-variant-numeric: tabular-nums;
    -moz-appearance: textfield;
    appearance: textfield;
  }
  .chip-in .num-in::-webkit-outer-spin-button,
  .chip-in .num-in::-webkit-inner-spin-button {
    -webkit-appearance: none;
    margin: 0;
  }
  .chip-in.topic-in input { width: 72px; }
  .auto-pill {
    font-size: 0.7rem; font-weight: 700; border-radius: 999px; padding: 0.14rem 0.5rem; white-space: nowrap;
  }
  .auto-pill.sell { color: var(--amber); background: rgb(247 153 27 / 10%); }
  .act-btn {
    border: 0; border-radius: 999px; padding: 0.42rem 1.05rem;
    font: inherit; font-size: 0.82rem; font-weight: 800; cursor: pointer;
    transition: filter 0.14s, transform 0.1s;
  }
  .act-btn.sell { background: var(--amber); color: #120e0c; }
  .act-btn:hover:not(:disabled) { filter: brightness(1.08); }
  .act-btn:disabled { opacity: 0.35; cursor: default; }

  /* Compact Order Lists */
  .book-list { display: flex; flex-direction: column; gap: 0.45rem; }
  .book-row {
    background: var(--surf); border: 1px solid var(--line); border-radius: 10px;
    padding: 0.65rem 0.85rem; display: flex; flex-direction: column; gap: 0.32rem;
    transition: background 0.12s, border-color 0.12s;
  }
  .book-row:hover { background: var(--hover); border-color: var(--line2); }
  .self-row { border-color: rgb(247 153 27 / 22%); }
  .peer-row { cursor: pointer; }
  .row-top { display: flex; align-items: center; gap: 0.5rem; flex-wrap: wrap; }
  .tag { font-size: 0.84rem; font-weight: 800; }
  .tag.sell { color: var(--amber); }
  .stat-inline { font-size: 0.74rem; color: var(--mut); }
  .stat-inline.warn { color: var(--amber); font-weight: 600; }
  .price {
    margin-left: auto; font-family: ui-monospace, monospace;
    font-size: 0.82rem; font-weight: 800; font-variant-numeric: tabular-nums;
  }
  .price.sell { color: var(--amber); }
  .pname { font-size: 0.84rem; }
  .cross-btn {
    margin-left: auto; border-radius: 999px; padding: 0.18rem 0.62rem;
    font: inherit; font-size: 0.72rem; font-weight: 700; cursor: pointer;
    background: transparent;
  }
  .cross-btn.to-buy { color: var(--ok); border: 1px solid rgb(76 194 107 / 35%); }
  .cross-btn.to-buy:hover { background: var(--ok); color: #08120a; }
  .mwrap { position: relative; }
  .more { background: none; border: 0; color: var(--mut); font-weight: 700; cursor: pointer; padding: 0.05rem 0.3rem; }
  .menu {
    position: absolute; right: 0; top: 100%; z-index: 25; min-width: 118px;
    background: #171412; border: 1px solid var(--line2); border-radius: 10px; padding: 0.28rem;
    box-shadow: 0 12px 28px rgb(0 0 0 / 60%); display: grid; gap: 0.12rem;
  }
  .menu button {
    background: none; border: 0; color: var(--ink); font: inherit; font-size: 0.78rem;
    text-align: left; padding: 0.4rem 0.58rem; border-radius: 7px; cursor: pointer;
  }
  .menu button:hover { background: var(--hover); }
  .menu button.danger { color: var(--bad); }
</style>
