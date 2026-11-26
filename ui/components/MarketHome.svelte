<script lang="ts">
  import type { DealFeedEntry, DockClient, MarketTapeEvent, NavTab, PeerInfo, PulseSignal, Receipt, State } from '../lib/types'
  import { operatorFetch, postJSON } from '../lib/api'
  import { agoMs, fmtSats } from '../lib/format'
  import MarketPulse from './MarketPulse.svelte'

  interface Props {
    s: State
    live: boolean
    pulse: PulseSignal | null
    dockedApps: DockClient[]
    searchQuery: string
    faucetBusy: boolean
    sellInputEl?: HTMLTextAreaElement | null
    buyInputEl?: HTMLTextAreaElement | null
    prefillBuy?: { domain: string; sats: number; seq: number } | null
    onSearch: (q: string) => void
    onOpenPalette: () => void
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
    onLocalPulse: (event: string, tape?: MarketTapeEvent) => void
    onNavigate: (tab: NavTab) => void
    onRequestFaucet: () => void
    onDecideApproval: (id: string, approve: boolean) => void
    onInspectDeal: (d: DealFeedEntry) => void
    onInspectPeer: (p: PeerInfo) => void
    onInspectReceipt: (r: Receipt) => void
  }

  let {
    s,
    live,
    pulse,
    dockedApps,
    searchQuery,
    faucetBusy,
    sellInputEl = $bindable(null),
    buyInputEl = $bindable(null),
    prefillBuy = null,
    onSearch,
    onOpenPalette,
    onRefresh,
    onToast,
    onLocalPulse,
    onNavigate,
    onRequestFaucet,
    onDecideApproval,
    onInspectDeal,
    onInspectPeer,
    onInspectReceipt
  }: Props = $props()

  interface TopicPill {
    text: string
    domainTag: string
    shardSlug: string
  }

  let buyText = $state('')
  let buyDomain = $state('')
  let buyMaxSats = $state('100')
  let buyBusy = $state(false)
  let buyPills = $state<TopicPill[]>([])

  let sellText = $state('')
  let sellDomain = $state('')
  let sellPrice = $state('50')
  let sellBusy = $state(false)
  let sellStage = $state('')
  let sellPills = $state<TopicPill[]>([])
  let openMenu = $state('')

  async function fetchTopicPills(hint: string): Promise<TopicPill[]> {
    const params = new URLSearchParams()
    params.set('hint', hint.slice(0, 180))
    const res = await fetch(`/v1/topics/cluster?${params.toString()}`)
    if (!res.ok) return []
    const data = (await res.json()) as {
      clusters?: Array<{ cluster: string; clusterTag: string; label: string }>
      related?: Array<{ topic: string }>
    }
    const pills: TopicPill[] = []
    for (const r of data.related ?? []) {
      pills.push({ text: `#${r.topic}`, domainTag: r.topic, shardSlug: r.topic })
    }
    for (const c of data.clusters ?? []) {
      if (c.cluster && c.cluster !== 'general' && c.label) {
        pills.push({ text: c.label, domainTag: c.clusterTag || `cluster:${c.cluster}`, shardSlug: c.cluster })
      }
    }
    return pills.slice(0, 2)
  }

  $effect(() => {
    const h = buyText.trim()
    if (h.length < 3) {
      buyPills = []
      return
    }
    let cancelled = false
    const t = setTimeout(async () => {
      try {
        const res = await fetchTopicPills(h)
        if (!cancelled) buyPills = res
      } catch {}
    }, 280)
    return () => {
      cancelled = true
      clearTimeout(t)
    }
  })

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

  let lastPrefillSeq = 0
  $effect(() => {
    if (prefillBuy && prefillBuy.seq !== lastPrefillSeq) {
      lastPrefillSeq = prefillBuy.seq
      pickBuy(prefillBuy.domain, prefillBuy.sats)
    }
  })

  // Minimal active running/earning agent pills for the home strip (real data only)
  const homeAgentPills = $derived.by(() => {
    const out: Array<{ id: string; name: string; offer: string; earned: number; live: boolean }> = []
    for (const m of s.market ?? []) {
      if (m.active === false) continue
      out.push({
        id: `shard:${m.shard}`,
        name: `#${m.shard} Seller`,
        offer: m.offers,
        earned: m.earned || 0,
        live: true
      })
    }
    for (const d of dockedApps) {
      if (!d.docked) continue
      out.push({
        id: `dock:${d.id}`,
        name: d.label,
        offer: 'Docked',
        earned: 0,
        live: true
      })
    }
    for (const p of (s.net.peers ?? []).slice(0, 4)) {
      const dm = p.sellDomains?.[0]?.replace(/^cluster:/, '')
      out.push({
        id: `peer:${p.pub}`,
        name: p.handle || `${p.pub.slice(0, 8)}…`,
        offer: dm ? `#${dm}` : p.isHub ? 'Hub' : 'Peer',
        earned: 0,
        live: true
      })
    }
    return out.slice(0, 6)
  })

  function pickBuy(domain: string, priceSats: number, hint?: string) {
    buyDomain = domain.replace(/^#/, '')
    if (priceSats > 0) buyMaxSats = String(priceSats)
    if (!buyText.trim() && hint) buyText = `Latest verified facts on #${buyDomain}`
    setTimeout(() => buyInputEl?.focus(), 20)
  }

  function pickSell(domain: string, rewardSats: number) {
    sellDomain = domain.replace(/^#/, '')
    if (rewardSats > 0) sellPrice = String(rewardSats)
    setTimeout(() => sellInputEl?.focus(), 20)
  }

  async function submitBuy() {
    const body = buyText.trim()
    if (!body || buyBusy) return
    buyBusy = true
    try {
      const cap = Math.max(1, Math.floor(Number(buyMaxSats) || 100))
      const inferred = buyPills.map((p) => p.domainTag).filter(Boolean)
      const cleanDom = buyDomain.trim().replace(/^#/, '')
      const domains = cleanDom ? [cleanDom, ...inferred.filter((d) => d !== cleanDom)] : inferred.length > 0 ? inferred : ['general']

      onLocalPulse('tool:want', {
        id: `local-buy-${Date.now()}`,
        kind: 'want',
        side: 'buy',
        domains,
        summary: body.slice(0, 120),
        sats: cap,
        status: 'open',
        isSelf: true,
        at: Date.now()
      })

      const res = await postJSON<{ status?: string; priceSats?: number }>('/v1/tools/want', {
        domains,
        question: body,
        maxSats: cap
      })
      if (!res.ok) {
        onToast(res.data.error ?? `Buy failed (${res.status})`, true)
        return
      }
      buyText = ''
      buyDomain = ''
      if (res.data.status === 'settled') {
        onLocalPulse('deal_settled', {
          id: `local-clear-${Date.now()}`,
          kind: 'deal',
          side: 'clear',
          domains,
          summary: body.slice(0, 120),
          sats: res.data.priceSats ?? cap,
          status: 'settled',
          isSelf: true,
          at: Date.now()
        })
        onToast('Bought & verified ✓')
      } else {
        onToast(`Buy request posted ✓`)
      }
      await onRefresh()
    } catch (e) {
      onToast(String(e), true)
    } finally {
      buyBusy = false
    }
  }

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

      sellStage = s.treasury.funded ? 'Anchoring…' : 'Saving…'
      const res = await postJSON<{ anchorTxid?: string }>('/v1/remember', {
        shard: topic,
        text: body,
        autoAnchor: s.treasury.funded
      })
      if (!res.ok) {
        onToast(res.data.error ?? `Save failed (${res.status})`, true)
        return
      }

      sellStage = 'Listing…'
      const sellRes = await postJSON('/v1/tools/sell', {
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
        status: 'listed',
        isSelf: true,
        at: Date.now()
      })
      sellText = ''
      sellDomain = ''
      onToast(`Listed #${topic} · ${fmtSats(price)} sats ✓`)
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

  const q = $derived(searchQuery.trim().toLowerCase())
  const pendingApprovals = $derived(s.automation?.pending ?? [])

  const bounties = $derived(
    (s.net.bounties ?? []).filter((b) => !q || `${b.domain} ${b.question}`.toLowerCase().includes(q))
  )
  const demand = $derived(
    (s.net.unmatchedWants ?? []).filter((u) => !q || `${u.domain} ${u.query}`.toLowerCase().includes(q))
  )
  const liveWants = $derived(
    (s.marketTape ?? [])
      .filter((e) => e.kind === 'want' && !demand.some((u) => u.wantId === e.id))
      .filter((e) => !q || `${(e.domains ?? []).join(' ')} ${e.summary ?? ''}`.toLowerCase().includes(q))
      .slice(0, 8)
  )
  const listings = $derived(
    (s.market ?? []).filter((m) => !q || `${m.shard} ${m.domains.join(' ')}`.toLowerCase().includes(q))
  )
  const peers = $derived(
    (s.net.peers ?? []).filter((p) => !q || `${p.handle ?? ''} ${p.pub} ${(p.sellDomains ?? []).join(' ')}`.toLowerCase().includes(q))
  )
  const deals = $derived(
    (s.automation?.deals ?? []).filter((d) => !q || `${d.shard ?? ''} ${d.cause} ${d.deliveredText ?? ''} ${d.to}`.toLowerCase().includes(q))
  )
  const paidReceipts = $derived(
    (s.receipts.list ?? [])
      .filter((r) => (r.sats ?? 0) > 0)
      .filter((r) => !q || `${r.shards.join(' ')} ${r.answer ?? ''}`.toLowerCase().includes(q))
      .slice(0, 6)
  )
</script>

<div class="mhome">
  <!-- Single Minimal Top Bar -->
  <header class="topbar">
    <div class="search-box">
      <svg viewBox="0 0 20 20" width="14" height="14" class="sico">
        <circle cx="9" cy="9" r="5.5" fill="none" stroke="currentColor" stroke-width="1.7" />
        <path d="M13.2 13.2L17 17" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" />
      </svg>
      <input
        value={searchQuery}
        oninput={(e) => onSearch((e.currentTarget as HTMLInputElement).value)}
        placeholder="Search market…"
        aria-label="Search market"
      />
      {#if searchQuery}
        <button class="clr" onclick={() => onSearch('')} aria-label="Clear search">✕</button>
      {:else}
        <button class="kbd" onclick={onOpenPalette} title="Command Palette">⌘K</button>
      {/if}
    </div>

    <div class="tright">
      {#if s.treasury.sats === 0 && s.network === 'testnet'}
        <button class="faucet-btn" onclick={onRequestFaucet} disabled={faucetBusy}>
          {faucetBusy ? 'Loading…' : '+ Free sats'}
        </button>
      {/if}
      <button class="bal-pill" onclick={() => onNavigate('wallet')} title="Wallet">
        <span class="bval">{fmtSats(s.treasury.sats)}</span>
        <span class="bunit">sats</span>
      </button>
    </div>
  </header>

  <!-- Only surfaces if an operator approval is actively pending -->
  {#if pendingApprovals.length > 0}
    <section class="approvals-strip" aria-label="Pending approvals">
      {#each pendingApprovals as a}
        <div class="appr-row">
          <span class="appr-badge">APPROVE</span>
          <span class="appr-txt">{a.reason} · ≤ {fmtSats(a.maxExtraSats)} sats</span>
          <div class="appr-acts">
            <button class="btn-ok" onclick={() => onDecideApproval(a.id, true)}>Approve</button>
            <button class="btn-ghost" onclick={() => onDecideApproval(a.id, false)}>Deny</button>
          </div>
        </div>
      {/each}
    </section>
  {/if}

  <!-- 1. VIRAL HERO: CLEAN NEKTAR CLEARING PULSE -->
  <MarketPulse {s} {live} {pulse} onPickBuy={pickBuy} onPickSell={pickSell} />

  <!-- 2. TABLESS SIDE-BY-SIDE BUY & SELL -->
  <section class="dual-deck">
    <!-- LEFT: BUY -->
    <div class="col col-buy">
      <div class="col-head">
        <span class="side-dot buy"></span>
        <h2>Buy</h2>
      </div>

      <div class="order-box buy-box">
        <textarea
          bind:this={buyInputEl}
          bind:value={buyText}
          rows="2"
          placeholder="What verified data do you want to buy?"
          disabled={buyBusy}
          onkeydown={(e) => {
            if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
              e.preventDefault()
              void submitBuy()
            }
          }}
        ></textarea>

        <div class="order-bar">
          <div class="order-opts">
            <label class="chip-in">
              <input class="num-in" type="number" min="1" bind:value={buyMaxSats} aria-label="Max satoshis" />
              <span>sats</span>
            </label>
            <label class="chip-in topic-in">
              <span>#</span>
              <input bind:value={buyDomain} placeholder="topic" aria-label="Topic" />
            </label>
            {#each buyPills as p}
              <span class="auto-pill buy">{p.text}</span>
            {/each}
          </div>
          <button class="act-btn buy" onclick={submitBuy} disabled={buyBusy || !buyText.trim()}>
            {buyBusy ? '…' : 'Buy'}
          </button>
        </div>
      </div>

      {#if bounties.length || demand.length || liveWants.length}
        <div class="book-list">
          {#each bounties as b}
            <article class="book-row">
              <div class="row-top">
                <span class="tag buy">#{b.domain}</span>
                <span class="price buy">+{fmtSats(b.rewardSats)}s</span>
              </div>
              <p class="row-txt">{b.question}</p>
              <div class="row-foot">
                <button class="cross-btn to-sell" onclick={() => pickSell(b.domain, b.rewardSats)}>Sell →</button>
              </div>
            </article>
          {/each}

          {#each demand as uw}
            <article class="book-row">
              <div class="row-top">
                <span class="tag buy">#{uw.domain}</span>
                <span class="price buy">{fmtSats(uw.maxSats)}s</span>
              </div>
              <p class="row-txt">{uw.query || 'Verified facts requested'}</p>
              <div class="row-foot">
                <span class="time">{agoMs(uw.seenAt * 1000)}</span>
                <button class="cross-btn to-sell" onclick={() => pickSell(uw.domain, uw.maxSats)}>Sell →</button>
              </div>
            </article>
          {/each}

          {#each liveWants as w}
            {@const dom = (w.domains?.[0] || 'general').replace(/^#/, '')}
            <article class="book-row">
              <div class="row-top">
                <span class="tag buy">#{dom}</span>
                <span class="price buy">{fmtSats(w.sats)}s</span>
              </div>
              <p class="row-txt">{w.summary || 'Live buy request'}</p>
              <div class="row-foot">
                <span class="time">{agoMs(w.at)}</span>
                <button class="cross-btn to-sell" onclick={() => pickSell(dom, w.sats)}>Sell →</button>
              </div>
            </article>
          {/each}
        </div>
      {/if}
    </div>

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
            <article class="book-row self-row">
              <div class="row-top">
                <span class="tag sell">#{m.shard}</span>
                <span class="stat-inline">{m.active !== false ? 'Live' : 'Paused'} · {m.works} sold</span>
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
            <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_noninteractive_element_interactions -->
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
                      pickBuy(firstDom.replace(/^cluster:/, ''), 100)
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
  </section>

  <!-- 3. MINIMAL ACTIVE AGENTS STRIP (Only visible when agents/listings are live) -->
  {#if homeAgentPills.length > 0}
    <section class="agents-strip" aria-label="Active Agents">
      <div class="agents-pills">
        {#each homeAgentPills as ag (ag.id)}
          <button class="apill" onclick={() => onNavigate('agents')}>
            <span class="adot" class:on={ag.live}></span>
            <b>{ag.name}</b>
            <span class="aoffer">{ag.offer}</span>
            {#if ag.earned > 0}
              <span class="aearn">+{fmtSats(ag.earned)}s</span>
            {/if}
          </button>
        {/each}
      </div>
      <button class="fleet-link" onclick={() => onNavigate('agents')}>Agents →</button>
    </section>
  {/if}

  <!-- 4. CLEARED TRADES (Only renders when trades exist!) -->
  {#if deals.length > 0 || paidReceipts.length > 0}
    <section class="tape-section">
      <div class="tape-list">
        {#each deals as d}
          {@const isSell = d.role === 'seller' || d.cause.startsWith('seller')}
          <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_noninteractive_element_interactions -->
          <article class="tape-row" onclick={() => onInspectDeal(d)}>
            <span class="side-pill" class:sell={isSell} class:buy={!isSell}>{isSell ? 'SELL' : 'BUY'}</span>
            <span class="tape-topic">{d.shard ? `#${d.shard}` : d.op.slice(0, 14)}</span>
            <span class="tape-desc">{d.deliveredText || d.cause}</span>
            <span class="tape-status" class:ok={d.to === 'settled'} class:bad={d.to === 'failed'}>
              {d.to === 'settled' ? '✓' : d.to}
            </span>
            {#if d.priceSats}
              <span class="tape-sats" class:sell={isSell}>{isSell ? '+' : '-'}{fmtSats(d.priceSats)}s</span>
            {/if}
            <span class="time">{agoMs(d.at)}</span>
          </article>
        {/each}

        {#each paidReceipts as r}
          <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_noninteractive_element_interactions -->
          <article class="tape-row" onclick={() => onInspectReceipt(r)}>
            <span class="side-pill sell">SELL</span>
            <span class="tape-topic">{r.shards.length ? `#${r.shards[0]}` : 'verified'}</span>
            <span class="tape-desc">{r.answer || 'Verified answer'}</span>
            <span class="tape-status ok">✓</span>
            {#if r.sats}
              <span class="tape-sats sell">+{fmtSats(r.sats)}s</span>
            {/if}
            <span class="time">{agoMs(r.at)}</span>
          </article>
        {/each}
      </div>
    </section>
  {/if}
</div>

<style>
  .mhome {
    display: flex;
    flex-direction: column;
    min-height: 100vh;
  }

  /* Minimal Top Bar */
  .topbar {
    position: sticky;
    top: 0;
    z-index: 20;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    padding: 0.65rem 1.25rem;
    background: rgb(9 8 7 / 92%);
    backdrop-filter: blur(12px);
    border-bottom: 1px solid var(--line);
  }
  .search-box {
    flex: 1;
    max-width: 340px;
    display: flex;
    align-items: center;
    gap: 0.5rem;
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 0.36rem 0.78rem;
  }
  .search-box:focus-within { border-color: var(--line2); }
  .sico { color: var(--mut2); flex-shrink: 0; }
  .search-box input {
    flex: 1; min-width: 0; background: none; border: 0;
    color: var(--ink); font: inherit; font-size: 0.82rem; outline: none;
  }
  .search-box input::placeholder { color: var(--mut2); }
  .clr, .kbd {
    background: none; border: 0; color: var(--mut2);
    font-family: ui-monospace, monospace; font-size: 0.7rem; cursor: pointer; padding: 0.08rem 0.2rem;
  }
  .clr:hover, .kbd:hover { color: var(--ink); }
  .tright {
    display: flex; align-items: center; gap: 0.55rem; flex-shrink: 0;
  }
  .faucet-btn {
    background: rgb(247 153 27 / 12%); color: var(--amber);
    border: 1px solid rgb(247 153 27 / 35%); border-radius: 999px;
    padding: 0.3rem 0.75rem; font: inherit; font-size: 0.76rem; font-weight: 700; cursor: pointer;
  }
  .bal-pill {
    display: inline-flex; align-items: baseline; gap: 0.3rem;
    background: var(--surf); border: 1px solid var(--line); border-radius: 999px;
    padding: 0.32rem 0.8rem; color: var(--ink); font: inherit; cursor: pointer;
    transition: border-color 0.14s;
  }
  .bal-pill:hover { border-color: var(--amber); }
  .bval { font-family: ui-monospace, monospace; font-size: 0.86rem; font-weight: 800; font-variant-numeric: tabular-nums; }
  .bunit { font-size: 0.72rem; font-weight: 700; color: var(--amber); }

  /* Pending Approvals */
  .approvals-strip {
    background: rgb(247 153 27 / 8%);
    border-bottom: 1px solid rgb(247 153 27 / 30%);
    padding: 0.6rem 1.25rem;
    display: grid; gap: 0.45rem;
  }
  .appr-row { display: flex; align-items: center; gap: 0.7rem; flex-wrap: wrap; font-size: 0.82rem; }
  .appr-badge {
    background: var(--amber); color: #120e0c; font-size: 0.65rem; font-weight: 800;
    padding: 0.12rem 0.48rem; border-radius: 999px;
  }
  .appr-txt { flex: 1; color: var(--ink); }
  .appr-acts { display: flex; gap: 0.4rem; }
  .btn-ok {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.3rem 0.8rem; font: inherit; font-size: 0.76rem; font-weight: 800; cursor: pointer;
  }
  .btn-ghost {
    background: transparent; color: var(--mut); border: 1px solid var(--line2);
    border-radius: 999px; padding: 0.3rem 0.75rem; font: inherit; font-size: 0.76rem; cursor: pointer;
  }

  /* Tabless Side-by-Side Dual Deck */
  .dual-deck {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
    flex: 1;
  }
  .col {
    padding: 1.15rem 1.25rem 1.4rem;
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
    min-width: 0;
  }
  .col-buy { border-right: 1px solid var(--line); }
  .col-head { display: flex; align-items: center; gap: 0.5rem; }
  .col-head h2 { margin: 0; font-size: 0.94rem; font-weight: 800; letter-spacing: -0.01em; }
  .side-dot { width: 8px; height: 8px; border-radius: 50%; flex-shrink: 0; }
  .side-dot.buy { background: var(--ok); }
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
  .buy-box:focus-within { border-color: rgb(76 194 107 / 55%); }
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
  .auto-pill.buy { color: var(--ok); background: rgb(76 194 107 / 10%); }
  .auto-pill.sell { color: var(--amber); background: rgb(247 153 27 / 10%); }
  .act-btn {
    border: 0; border-radius: 999px; padding: 0.42rem 1.05rem;
    font: inherit; font-size: 0.82rem; font-weight: 800; cursor: pointer;
    transition: filter 0.14s, transform 0.1s;
  }
  .act-btn.buy { background: var(--ok); color: #08120a; }
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
  .tag.buy { color: var(--ok); }
  .tag.sell { color: var(--amber); }
  .stat-inline { font-size: 0.74rem; color: var(--mut); }
  .price {
    margin-left: auto; font-family: ui-monospace, monospace;
    font-size: 0.82rem; font-weight: 800; font-variant-numeric: tabular-nums;
  }
  .price.buy { color: var(--ok); }
  .price.sell { color: var(--amber); }
  .row-txt { margin: 0; font-size: 0.84rem; line-height: 1.4; color: var(--ink); }
  .row-foot { display: flex; align-items: center; justify-content: space-between; gap: 0.5rem; }
  .pname { font-size: 0.84rem; }
  .cross-btn {
    margin-left: auto; border-radius: 999px; padding: 0.18rem 0.62rem;
    font: inherit; font-size: 0.72rem; font-weight: 700; cursor: pointer;
    background: transparent;
  }
  .cross-btn.to-sell { color: var(--amber); border: 1px solid rgb(247 153 27 / 35%); }
  .cross-btn.to-sell:hover { background: var(--amber); color: #120e0c; }
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

  /* Minimal Active Agents Strip */
  .agents-strip {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.75rem;
    padding: 0.7rem 1.25rem;
    border-top: 1px solid var(--line);
    background: #0c0b09;
    flex-wrap: wrap;
  }
  .agents-pills {
    display: flex;
    align-items: center;
    gap: 0.45rem;
    flex-wrap: wrap;
  }
  .apill {
    display: inline-flex;
    align-items: center;
    gap: 0.42rem;
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 0.28rem 0.72rem;
    color: var(--ink);
    font: inherit;
    font-size: 0.76rem;
    cursor: pointer;
    transition: border-color 0.14s, background 0.14s;
  }
  .apill:hover {
    border-color: var(--line2);
    background: var(--hover);
  }
  .adot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--mut2);
  }
  .adot.on {
    background: var(--ok);
    box-shadow: 0 0 6px rgb(76 194 107 / 55%);
  }
  .aoffer {
    color: var(--amber);
    font-family: ui-monospace, monospace;
    font-size: 0.72rem;
    font-weight: 700;
  }
  .aearn {
    color: var(--ok);
    font-family: ui-monospace, monospace;
    font-size: 0.72rem;
    font-weight: 800;
  }
  .fleet-link {
    background: none;
    border: 0;
    color: var(--mut);
    font: inherit;
    font-size: 0.76rem;
    font-weight: 700;
    cursor: pointer;
    padding: 0.2rem 0.35rem;
  }
  .fleet-link:hover {
    color: var(--amber);
  }

  /* Cleared Trades Tape */
  .tape-section {
    border-top: 1px solid var(--line);
    padding: 0.9rem 1.25rem 2rem;
  }
  .tape-list { display: flex; flex-direction: column; gap: 0.4rem; }
  .tape-row {
    display: flex; align-items: center; gap: 0.65rem;
    padding: 0.58rem 0.85rem; background: var(--surf); border: 1px solid var(--line);
    border-radius: 10px; cursor: pointer; font-size: 0.82rem;
    transition: background 0.12s, border-color 0.12s;
  }
  .tape-row:hover { background: var(--hover); border-color: var(--line2); }
  .side-pill {
    font-family: ui-monospace, monospace; font-size: 0.65rem; font-weight: 800;
    padding: 0.12rem 0.48rem; border-radius: 999px; flex-shrink: 0;
  }
  .side-pill.buy { background: rgb(76 194 107 / 14%); color: var(--ok); }
  .side-pill.sell { background: rgb(247 153 27 / 14%); color: var(--amber); }
  .tape-topic { font-weight: 700; color: var(--ink); white-space: nowrap; }
  .tape-desc { flex: 1; min-width: 0; color: var(--mut); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .tape-status { font-family: ui-monospace, monospace; font-size: 0.76rem; color: var(--mut); }
  .tape-status.ok { color: var(--ok); font-weight: 800; }
  .tape-status.bad { color: var(--bad); }
  .tape-sats { font-family: ui-monospace, monospace; font-weight: 800; color: var(--ink); white-space: nowrap; }
  .tape-sats.sell { color: var(--amber); }
  .time { color: var(--mut2); font-family: ui-monospace, monospace; font-size: 0.72rem; white-space: nowrap; }

  @media (max-width: 820px) {
    .dual-deck { grid-template-columns: 1fr; }
    .col-buy { border-right: 0; border-bottom: 1px solid var(--line); }
    .tape-desc { display: none; }
  }
</style>
