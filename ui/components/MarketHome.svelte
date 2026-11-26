<script lang="ts">
  import type { DealFeedEntry, MarketTapeEvent, NavTab, PeerInfo, PulseSignal, Receipt, State } from '../lib/types'
  import { operatorFetch, postJSON } from '../lib/api'
  import { agoMs, fmtSats } from '../lib/format'
  import MarketPulse from './MarketPulse.svelte'

  interface Props {
    s: State
    live: boolean
    pulse: PulseSignal | null
    searchQuery: string
    faucetBusy: boolean
    sellInputEl?: HTMLTextAreaElement | null
    buyInputEl?: HTMLTextAreaElement | null
    onSearch: (q: string) => void
    onOpenPalette: () => void
    onOpenDock: () => void
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
    onLocalPulse: (event: string, tape?: MarketTapeEvent) => void
    onNavigate: (tab: NavTab) => void
    onRequestFaucet: () => void
    onApproveStarter: () => void
    onQuickRecall: () => void
    onDecideApproval: (id: string, approve: boolean) => void
    onInspectDeal: (d: DealFeedEntry) => void
    onInspectPeer: (p: PeerInfo) => void
    onInspectReceipt: (r: Receipt) => void
  }

  let {
    s,
    live,
    pulse,
    searchQuery,
    faucetBusy,
    sellInputEl = $bindable(null),
    buyInputEl = $bindable(null),
    onSearch,
    onOpenPalette,
    onOpenDock,
    onRefresh,
    onToast,
    onLocalPulse,
    onNavigate,
    onRequestFaucet,
    onApproveStarter,
    onQuickRecall,
    onDecideApproval,
    onInspectDeal,
    onInspectPeer,
    onInspectReceipt
  }: Props = $props()

  interface TopicPill {
    text: string
    domainTag: string
    shardSlug: string
    pct: number
    kind: 'peer' | 'cluster'
  }

  // Tabless BUY form state (Left Column)
  let buyText = $state('')
  let buyDomain = $state('')
  let buyMaxSats = $state('100')
  let buyBusy = $state(false)
  let buyPills = $state<TopicPill[]>([])

  // Tabless SELL form state (Right Column)
  let sellText = $state('')
  let sellDomain = $state('')
  let sellPrice = $state('50')
  let sellBusy = $state(false)
  let sellStage = $state('')
  let sellPills = $state<TopicPill[]>([])
  let openMenu = $state('')

  let setupHidden = $state(typeof localStorage !== 'undefined' && localStorage.getItem('nektar-setup-hidden') === '1')

  async function fetchTopicPills(hint: string): Promise<TopicPill[]> {
    const params = new URLSearchParams()
    params.set('hint', hint.slice(0, 180))
    const res = await fetch(`/v1/topics/cluster?${params.toString()}`)
    if (!res.ok) return []
    const data = (await res.json()) as {
      clusters?: Array<{ cluster: string; clusterTag: string; label: string; confidence: number }>
      related?: Array<{ topic: string; confidence: number }>
    }
    const pills: TopicPill[] = []
    for (const r of data.related ?? []) {
      pills.push({
        text: `≈ ${r.topic}`,
        domainTag: r.topic,
        shardSlug: r.topic,
        pct: Math.round((r.confidence ?? 0) * 100),
        kind: 'peer'
      })
    }
    for (const c of data.clusters ?? []) {
      if (c.cluster && c.cluster !== 'general' && c.label) {
        pills.push({
          text: c.label,
          domainTag: c.clusterTag || `cluster:${c.cluster}`,
          shardSlug: c.cluster,
          pct: Math.round((c.confidence ?? 0) * 100),
          kind: 'cluster'
        })
      }
    }
    return pills.slice(0, 3)
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

  function formatDomainTag(dm: string): string {
    if (dm.startsWith('cluster:')) {
      return dm
        .slice('cluster:'.length)
        .split('-')
        .map((w) => (w ? w[0].toUpperCase() + w.slice(1) : ''))
        .join(' & ')
    }
    return `#${dm.replace(/^#/, '')}`
  }

  function pickBuy(domain: string, priceSats: number, hint?: string) {
    buyDomain = domain.replace(/^#/, '')
    if (priceSats > 0) buyMaxSats = String(priceSats)
    if (!buyText.trim() && hint) buyText = `Latest verified facts on #${buyDomain}`
    setTimeout(() => buyInputEl?.focus(), 20)
  }

  function pickSell(domain: string, rewardSats: number, hint?: string) {
    sellDomain = domain.replace(/^#/, '')
    if (rewardSats > 0) sellPrice = String(rewardSats)
    if (!sellText.trim() && hint) sellText = ''
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
        onToast(res.data.error ?? `Purchase request failed (${res.status})`, true)
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
        onToast('Data bought & SPV verified ✓')
      } else {
        onToast(`Buy request ${res.data.status ?? 'posted'} ✓`)
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

      sellStage = s.treasury.funded ? '1/2 Anchoring…' : '1/2 Saving…'
      const res = await postJSON<{ seq?: number; anchorTxid?: string }>('/v1/remember', {
        shard: topic,
        text: body,
        autoAnchor: s.treasury.funded
      })
      if (!res.ok) {
        onToast(res.data.error ?? `Anchor failed (${res.status})`, true)
        return
      }

      sellStage = '2/2 Listing…'
      const sellRes = await postJSON('/v1/tools/sell', {
        shard: topic,
        domains,
        priceSats: price,
        description: body.slice(0, 140)
      })
      if (!sellRes.ok) {
        onToast(sellRes.data.error ?? `Anchored, but listing failed (${sellRes.status})`, true)
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
      onToast(
        res.data.anchorTxid
          ? `Anchored on BSV & listed #${topic} for ${fmtSats(price)} sats ✓`
          : `Saved & listed #${topic} for ${fmtSats(price)} sats ✓`
      )
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

  const hasPolicy = $derived(s.settings.policies.some((p) => !p.revokedAt))
  const setupSteps = $derived([
    { id: 'dock', label: 'Dock AI agent', done: false, action: onOpenDock },
    { id: 'receipt', label: 'First verified receipt', done: s.receipts.count > 0, action: onQuickRecall },
    {
      id: 'fund',
      label: s.network === 'testnet' ? 'Get free test sats' : 'Fund wallet',
      done: s.treasury.sats > 0,
      action: s.network === 'testnet' ? onRequestFaucet : () => onNavigate('wallet')
    },
    { id: 'policy', label: 'Enable auto-trading', done: hasPolicy, action: onApproveStarter }
  ])
  const remainingSteps = $derived(setupSteps.filter((st) => !st.done))

  function hideSetup() {
    setupHidden = true
    try {
      localStorage.setItem('nektar-setup-hidden', '1')
    } catch {}
  }
</script>

<div class="mhome">
  <!-- Panoramic Sticky Market Header -->
  <header class="topbar">
    <div class="tleft">
      <h1>Market</h1>
      <span class="net-pill" class:live={live}>{live ? 'LIVE' : 'OFFLINE'}</span>
    </div>

    <div class="search-box">
      <svg viewBox="0 0 20 20" width="14" height="14" class="sico">
        <circle cx="9" cy="9" r="5.5" fill="none" stroke="currentColor" stroke-width="1.7" />
        <path d="M13.2 13.2L17 17" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" />
      </svg>
      <input
        value={searchQuery}
        oninput={(e) => onSearch((e.currentTarget as HTMLInputElement).value)}
        placeholder="Filter topics, bounties, peers, trades…"
        aria-label="Filter market"
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
          {faucetBusy ? 'Getting sats…' : '⚡ Free Test Sats'}
        </button>
      {/if}
      <button class="bal-pill" onclick={() => onNavigate('wallet')} title="Open Wallet">
        <span class="bval">{fmtSats(s.treasury.sats)}</span>
        <span class="bunit">sats</span>
      </button>
    </div>
  </header>

  <!-- Pending Operator Approvals Banner -->
  {#if pendingApprovals.length > 0}
    <section class="approvals-strip" aria-label="Pending approvals">
      {#each pendingApprovals as a}
        <div class="appr-row">
          <span class="appr-badge">! APPROVAL NEEDED</span>
          <span class="appr-txt">{a.reason} <b>(≤ {fmtSats(a.maxExtraSats)} sats)</b></span>
          <div class="appr-acts">
            <button class="btn-ok" onclick={() => onDecideApproval(a.id, true)}>Approve</button>
            <button class="btn-ghost" onclick={() => onDecideApproval(a.id, false)}>Deny</button>
          </div>
        </div>
      {/each}
    </section>
  {/if}

  <!-- Compact Dismissible Quick-Setup Strip (if node is new) -->
  {#if !setupHidden && remainingSteps.length > 0}
    <section class="onboard-strip">
      <span class="olbl">QUICK START</span>
      <div class="ochips">
        {#each remainingSteps as st}
          <button class="ochip" onclick={st.action}>+ {st.label}</button>
        {/each}
      </div>
      <button class="ohide" onclick={hideSetup} title="Dismiss">✕</button>
    </section>
  {/if}

  <!-- 1. VIRAL HERO GRAPHIC: THE NEKTAR CLEARING PULSE -->
  <MarketPulse {s} {live} {pulse} onPickBuy={pickBuy} onPickSell={pickSell} />

  <!-- 2. ZERO-TAB SIDE-BY-SIDE BUY & SELL DECK -->
  <section class="dual-deck">
    <!-- LEFT COLUMN: BUY / DEMAND (Emerald) -->
    <div class="col col-buy">
      <div class="col-head">
        <span class="side-dot buy"></span>
        <h2>Buy Verified Data</h2>
        <span class="col-sub">Escrow pays only after SPV proof checks out</span>
      </div>

      <div class="order-box buy-box">
        <textarea
          bind:this={buyInputEl}
          bind:value={buyText}
          rows="2"
          placeholder="Ask the network for verified facts, data, or research…"
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
              <span>Max</span>
              <input type="number" min="1" bind:value={buyMaxSats} aria-label="Max budget in satoshis" />
              <span>sats</span>
            </label>
            <label class="chip-in topic-in">
              <span>#</span>
              <input bind:value={buyDomain} placeholder="auto-topic" aria-label="Topic tag" />
            </label>
            {#each buyPills as p}
              <span class="auto-pill buy">{p.text} <em>{p.pct}%</em></span>
            {/each}
          </div>
          <button class="act-btn buy" onclick={submitBuy} disabled={buyBusy || !buyText.trim()}>
            {buyBusy ? 'Requesting…' : 'Buy Now'}
          </button>
        </div>
      </div>

      <!-- Open Bounties & Live Wants -->
      <div class="book-lbl">
        <span>Open Bounties &amp; Live Demand</span>
        <span class="cnt">{bounties.length + demand.length + liveWants.length}</span>
      </div>

      <div class="book-list">
        {#each bounties as b}
          <article class="book-row">
            <div class="row-top">
              <span class="tag buy">#{b.domain}</span>
              <span class="kind-pill">BOUNTY · {b.filled}/{b.maxFills}</span>
              <span class="price buy">+{fmtSats(b.rewardSats)} sats</span>
            </div>
            <p class="row-txt">{b.question}</p>
            <div class="row-foot">
              <button class="cross-btn to-sell" onclick={() => pickSell(b.domain, b.rewardSats, b.question)}>
                Fill &amp; Earn →
              </button>
            </div>
          </article>
        {/each}

        {#each demand as uw}
          <article class="book-row">
            <div class="row-top">
              <span class="tag buy">#{uw.domain}</span>
              <span class="kind-pill">WANT</span>
              <span class="price buy">up to {fmtSats(uw.maxSats)} sats</span>
            </div>
            <p class="row-txt">{uw.query || 'Looking for verified facts in this topic'}</p>
            <div class="row-foot">
              <span class="time">{agoMs(uw.seenAt * 1000)}</span>
              <button class="cross-btn to-sell" onclick={() => pickSell(uw.domain, uw.maxSats, uw.query)}>
                Sell Into Want →
              </button>
            </div>
          </article>
        {/each}

        {#each liveWants as w}
          {@const dom = (w.domains?.[0] || 'general').replace(/^#/, '')}
          <article class="book-row">
            <div class="row-top">
              <span class="tag buy">#{dom}</span>
              <span class="kind-pill live">LIVE WANT</span>
              <span class="price buy">≤ {fmtSats(w.sats)} sats</span>
            </div>
            <p class="row-txt">{w.summary || 'Live agent purchase request'}</p>
            <div class="row-foot">
              <span class="time">{agoMs(w.at)}</span>
              <button class="cross-btn to-sell" onclick={() => pickSell(dom, w.sats, w.summary)}>
                Sell Into Want →
              </button>
            </div>
          </article>
        {/each}

        {#if !bounties.length && !demand.length && !liveWants.length}
          <div class="book-empty">
            No open buyer wants on this hub yet — post a Buy above to broadcast a signed WantAd.
          </div>
        {/if}
      </div>
    </div>

    <!-- RIGHT COLUMN: SELL / SUPPLY (Amber) -->
    <div class="col col-sell">
      <div class="col-head">
        <span class="side-dot sell"></span>
        <h2>Sell Anchored Knowledge</h2>
        <span class="col-sub">Anchors on BSV &amp; auto-quotes incoming buyers</span>
      </div>

      <div class="order-box sell-box">
        <textarea
          bind:this={sellInputEl}
          bind:value={sellText}
          rows="2"
          placeholder="Paste insight, research, or facts to anchor on BSV &amp; earn sats…"
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
              <span>Ask</span>
              <input type="number" min="1" bind:value={sellPrice} aria-label="Ask price in satoshis" />
              <span>sats</span>
            </label>
            <label class="chip-in topic-in">
              <span>#</span>
              <input bind:value={sellDomain} placeholder="auto-topic" aria-label="Shard or topic" />
            </label>
            {#each sellPills as p}
              <span class="auto-pill sell">{p.text} <em>{p.pct}%</em></span>
            {/each}
          </div>
          <button class="act-btn sell" onclick={submitSell} disabled={sellBusy || !sellText.trim()}>
            {sellBusy ? sellStage || 'Listing…' : s.treasury.funded ? 'Anchor & Sell' : 'Save & List'}
          </button>
        </div>
      </div>

      <!-- Your Listed Shards + Network Sellers -->
      <div class="book-lbl">
        <span>Your Listed Shards &amp; Network Sellers</span>
        <span class="cnt">{listings.length + peers.length}</span>
      </div>

      <div class="book-list">
        {#each listings as m}
          <article class="book-row self-row">
            <div class="row-top">
              <span class="tag sell">#{m.shard}</span>
              <span class="you-badge">YOU</span>
              {#each m.domains.slice(0, 2) as dm}
                <span class="sub-tag">{formatDomainTag(dm)}</span>
              {/each}
              <span class="price sell">{m.offers}</span>
              <div class="mwrap">
                <button class="more" onclick={() => (openMenu = openMenu === m.shard ? '' : m.shard)} aria-label="Listing options">···</button>
                {#if openMenu === m.shard}
                  <div class="menu">
                    <button onclick={() => toggleListing(m.shard, m.active === false)}>
                      {m.active !== false ? 'Pause listing' : 'Resume listing'}
                    </button>
                    <button class="danger" onclick={() => removeListing(m.shard)}>Remove listing</button>
                  </div>
                {/if}
              </div>
            </div>
            <div class="row-foot">
              <span class="stat">
                <span class:live-txt={m.active !== false}>{m.active !== false ? '● Live' : '○ Paused'}</span>
                · {m.records} facts · {m.works} sold
                {#if m.earned > 0}· <b class="ok">+{fmtSats(m.earned)} sats</b>{/if}
              </span>
            </div>
          </article>
        {/each}

        {#each peers as p}
          {@const firstDom = p.sellDomains?.[0] ?? ''}
          <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_noninteractive_element_interactions -->
          <article class="book-row peer-row" onclick={() => onInspectPeer(p)}>
            <div class="row-top">
              <span class="pav">{(p.handle || p.pub).slice(0, 1).toUpperCase()}</span>
              <b class="pname">{p.handle || `${p.pub.slice(0, 12)}…`}</b>
              <span class="kind-pill">{p.isHub ? 'HUB' : 'AGENT'}</span>
            </div>
            {#if p.sellDomains?.length}
              <div class="peer-tags">
                {#each p.sellDomains.slice(0, 4) as dm}
                  <span class="sub-tag">{formatDomainTag(dm)}</span>
                {/each}
              </div>
            {/if}
            {#if firstDom}
              <div class="row-foot">
                <span class="stat">Verified peer seller</span>
                <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
                <button
                  class="cross-btn to-buy"
                  onclick={(e) => {
                    e.stopPropagation()
                    pickBuy(firstDom.replace(/^cluster:/, ''), 100)
                  }}
                >
                  ← Buy Topic
                </button>
              </div>
            {/if}
          </article>
        {/each}

        {#if !listings.length && !peers.length}
          <div class="book-empty">
            No shards listed yet — paste verified knowledge above to anchor on BSV and earn sats automatically.
          </div>
        {/if}
      </div>
    </div>
  </section>

  <!-- 3. LIVE CLEARED TRADES & VERIFIED SETTLEMENTS TAPE -->
  <section class="tape-section">
    <div class="tape-head">
      <span>LIVE CLEARED TRADES &amp; VERIFIED RECEIPTS</span>
      <span class="tape-cnt">{deals.length + paidReceipts.length} recent</span>
    </div>

    {#if deals.length === 0 && paidReceipts.length === 0}
      <div class="tape-empty">
        Settled atomic BSV trades and paid verification receipts stream here live as buyers and sellers clear.
      </div>
    {:else}
      <div class="tape-list">
        {#each deals as d}
          {@const isSell = d.role === 'seller' || d.cause.startsWith('seller')}
          <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_noninteractive_element_interactions -->
          <article class="tape-row" onclick={() => onInspectDeal(d)}>
            <span class="side-pill" class:sell={isSell} class:buy={!isSell}>{isSell ? 'SELL' : 'BUY'}</span>
            <span class="tape-topic">{d.shard ? `#${d.shard}` : d.op.slice(0, 14)}</span>
            <span class="tape-desc">{d.deliveredText || d.cause}</span>
            <span
              class="tape-status"
              class:ok={d.to === 'settled'}
              class:bad={d.to === 'failed'}
            >
              {d.to === 'settled' ? 'SPV Settled ✓' : d.to}
            </span>
            {#if d.priceSats}
              <span class="tape-sats" class:sell={isSell}>{isSell ? '+' : '-'}{fmtSats(d.priceSats)} sats</span>
            {/if}
            <span class="time">{agoMs(d.at)}</span>
          </article>
        {/each}

        {#each paidReceipts as r}
          <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_noninteractive_element_interactions -->
          <article class="tape-row" onclick={() => onInspectReceipt(r)}>
            <span class="side-pill sell">RECEIPT</span>
            <span class="tape-topic">{r.shards.length ? `#${r.shards[0]}` : 'verified'}</span>
            <span class="tape-desc">{r.answer || 'Verified grounded answer'}</span>
            <span class="tape-status ok">{r.verdict === 'green' ? 'SPV Verified ✓' : 'Verified'}</span>
            {#if r.sats}
              <span class="tape-sats sell">+{fmtSats(r.sats)} sats</span>
            {/if}
            <span class="time">{agoMs(r.at)}</span>
          </article>
        {/each}
      </div>
    {/if}
  </section>
</div>

<style>
  .mhome {
    display: flex;
    flex-direction: column;
    min-height: 100vh;
  }

  /* Top Bar */
  .topbar {
    position: sticky;
    top: 0;
    z-index: 20;
    display: flex;
    align-items: center;
    gap: 0.9rem;
    padding: 0.7rem 1.25rem;
    background: rgb(9 8 7 / 92%);
    backdrop-filter: blur(12px);
    border-bottom: 1px solid var(--line);
  }
  .tleft {
    display: flex;
    align-items: center;
    gap: 0.55rem;
    flex-shrink: 0;
  }
  h1 {
    margin: 0;
    font-size: 1.08rem;
    font-weight: 800;
    letter-spacing: -0.02em;
  }
  .net-pill {
    font-family: ui-monospace, monospace;
    font-size: 0.65rem;
    font-weight: 800;
    letter-spacing: 0.08em;
    padding: 0.14rem 0.48rem;
    border-radius: 999px;
    background: rgb(229 83 75 / 14%);
    color: var(--bad);
    border: 1px solid rgb(229 83 75 / 35%);
  }
  .net-pill.live {
    background: rgb(76 194 107 / 12%);
    color: var(--ok);
    border-color: rgb(76 194 107 / 32%);
  }
  .search-box {
    flex: 1;
    max-width: 440px;
    display: flex;
    align-items: center;
    gap: 0.5rem;
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 0.38rem 0.8rem;
  }
  .search-box:focus-within {
    border-color: var(--amber);
  }
  .sico {
    color: var(--mut2);
    flex-shrink: 0;
  }
  .search-box input {
    flex: 1;
    min-width: 0;
    background: none;
    border: 0;
    color: var(--ink);
    font: inherit;
    font-size: 0.82rem;
    outline: none;
  }
  .search-box input::placeholder {
    color: var(--mut2);
  }
  .clr, .kbd {
    background: none;
    border: 0;
    color: var(--mut2);
    font-family: ui-monospace, monospace;
    font-size: 0.7rem;
    cursor: pointer;
    padding: 0.08rem 0.25rem;
  }
  .clr:hover, .kbd:hover {
    color: var(--ink);
  }
  .tright {
    margin-left: auto;
    display: flex;
    align-items: center;
    gap: 0.55rem;
    flex-shrink: 0;
  }
  .faucet-btn {
    background: rgb(247 153 27 / 14%);
    color: var(--amber);
    border: 1px solid rgb(247 153 27 / 40%);
    border-radius: 999px;
    padding: 0.34rem 0.78rem;
    font: inherit;
    font-size: 0.76rem;
    font-weight: 800;
    cursor: pointer;
  }
  .faucet-btn:hover:not(:disabled) {
    background: rgb(247 153 27 / 24%);
  }
  .bal-pill {
    display: inline-flex;
    align-items: baseline;
    gap: 0.32rem;
    background: var(--surf);
    border: 1px solid var(--line2);
    border-radius: 999px;
    padding: 0.34rem 0.82rem;
    color: var(--ink);
    font: inherit;
    cursor: pointer;
    transition: border-color 0.14s;
  }
  .bal-pill:hover {
    border-color: var(--amber);
  }
  .bval {
    font-family: ui-monospace, monospace;
    font-size: 0.88rem;
    font-weight: 800;
    font-variant-numeric: tabular-nums;
  }
  .bunit {
    font-size: 0.72rem;
    font-weight: 700;
    color: var(--amber);
  }

  /* Pending Approvals Strip */
  .approvals-strip {
    background: rgb(247 153 27 / 8%);
    border-bottom: 1px solid rgb(247 153 27 / 32%);
    padding: 0.65rem 1.25rem;
    display: grid;
    gap: 0.5rem;
  }
  .appr-row {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    flex-wrap: wrap;
    font-size: 0.84rem;
  }
  .appr-badge {
    background: var(--amber);
    color: #120e0c;
    font-size: 0.68rem;
    font-weight: 800;
    padding: 0.14rem 0.5rem;
    border-radius: 999px;
  }
  .appr-txt {
    flex: 1;
    min-width: 200px;
    color: var(--ink);
  }
  .appr-acts {
    display: flex;
    gap: 0.45rem;
  }
  .btn-ok {
    background: var(--amber);
    color: #120e0c;
    border: 0;
    border-radius: 999px;
    padding: 0.32rem 0.85rem;
    font: inherit;
    font-size: 0.78rem;
    font-weight: 800;
    cursor: pointer;
  }
  .btn-ghost {
    background: transparent;
    color: var(--mut);
    border: 1px solid var(--line2);
    border-radius: 999px;
    padding: 0.32rem 0.8rem;
    font: inherit;
    font-size: 0.78rem;
    font-weight: 600;
    cursor: pointer;
  }

  /* Compact Quick Start Strip */
  .onboard-strip {
    display: flex;
    align-items: center;
    gap: 0.65rem;
    padding: 0.52rem 1.25rem;
    background: #100f0d;
    border-bottom: 1px solid var(--line);
    flex-wrap: wrap;
  }
  .olbl {
    font-size: 0.66rem;
    font-weight: 800;
    letter-spacing: 0.1em;
    color: var(--mut2);
  }
  .ochips {
    display: flex;
    gap: 0.4rem;
    flex-wrap: wrap;
    flex: 1;
  }
  .ochip {
    background: var(--surf);
    border: 1px solid var(--line2);
    color: var(--ink);
    border-radius: 999px;
    padding: 0.22rem 0.68rem;
    font: inherit;
    font-size: 0.74rem;
    font-weight: 600;
    cursor: pointer;
    transition: border-color 0.14s, color 0.14s;
  }
  .ochip:hover {
    border-color: var(--amber);
    color: var(--amber);
  }
  .ohide {
    background: none;
    border: 0;
    color: var(--mut2);
    cursor: pointer;
    font-size: 0.8rem;
    padding: 0.1rem 0.35rem;
  }
  .ohide:hover {
    color: var(--ink);
  }

  /* Tabless Side-by-Side Dual Deck */
  .dual-deck {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
    border-bottom: 1px solid var(--line);
  }
  .col {
    padding: 1.05rem 1.25rem 1.25rem;
    display: flex;
    flex-direction: column;
    gap: 0.8rem;
    min-width: 0;
  }
  .col-buy {
    border-right: 1px solid var(--line);
  }
  .col-head {
    display: flex;
    align-items: baseline;
    gap: 0.5rem;
    flex-wrap: wrap;
  }
  .col-head h2 {
    margin: 0;
    font-size: 0.92rem;
    font-weight: 800;
    letter-spacing: -0.01em;
  }
  .col-sub {
    font-size: 0.74rem;
    color: var(--mut2);
    margin-left: auto;
  }
  .side-dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    flex-shrink: 0;
    align-self: center;
  }
  .side-dot.buy {
    background: var(--ok);
    box-shadow: 0 0 8px rgb(76 194 107 / 55%);
  }
  .side-dot.sell {
    background: var(--amber);
    box-shadow: 0 0 8px rgb(247 153 27 / 55%);
  }

  /* Inline Order Composers */
  .order-box {
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 13px;
    padding: 0.75rem 0.9rem;
    display: flex;
    flex-direction: column;
    gap: 0.55rem;
    transition: border-color 0.15s;
  }
  .buy-box:focus-within {
    border-color: rgb(76 194 107 / 60%);
  }
  .sell-box:focus-within {
    border-color: rgb(247 153 27 / 65%);
  }
  .order-box textarea {
    width: 100%;
    background: transparent;
    border: 0;
    color: var(--ink);
    font: inherit;
    font-size: 0.92rem;
    line-height: 1.45;
    resize: none;
    outline: none;
    padding: 0;
  }
  .order-box textarea::placeholder {
    color: var(--mut2);
  }
  .order-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.5rem;
    flex-wrap: wrap;
    padding-top: 0.25rem;
    border-top: 1px solid rgb(255 255 255 / 4%);
  }
  .order-opts {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    flex-wrap: wrap;
  }
  .chip-in {
    display: inline-flex;
    align-items: center;
    gap: 0.28rem;
    background: var(--bg);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 0.2rem 0.58rem;
    font-size: 0.74rem;
    color: var(--mut);
  }
  .chip-in input {
    width: 48px;
    background: none;
    border: 0;
    color: var(--ink);
    font: inherit;
    font-size: 0.75rem;
    font-weight: 700;
    font-variant-numeric: tabular-nums;
    outline: none;
  }
  .chip-in.topic-in input {
    width: 78px;
  }
  .auto-pill {
    font-size: 0.7rem;
    font-weight: 700;
    border-radius: 999px;
    padding: 0.14rem 0.48rem;
    white-space: nowrap;
  }
  .auto-pill.buy {
    color: var(--ok);
    background: rgb(76 194 107 / 10%);
    border: 1px solid rgb(76 194 107 / 28%);
  }
  .auto-pill.sell {
    color: var(--amber);
    background: rgb(247 153 27 / 10%);
    border: 1px solid rgb(247 153 27 / 28%);
  }
  .auto-pill em {
    font-style: normal;
    opacity: 0.75;
    margin-left: 0.15rem;
  }
  .act-btn {
    border: 0;
    border-radius: 999px;
    padding: 0.42rem 1rem;
    font: inherit;
    font-size: 0.8rem;
    font-weight: 800;
    cursor: pointer;
    white-space: nowrap;
    transition: filter 0.14s, transform 0.1s;
  }
  .act-btn.buy {
    background: var(--ok);
    color: #09130b;
  }
  .act-btn.sell {
    background: var(--amber);
    color: #120e0c;
  }
  .act-btn:hover:not(:disabled) {
    filter: brightness(1.08);
  }
  .act-btn:active:not(:disabled) {
    transform: scale(0.98);
  }
  .act-btn:disabled {
    opacity: 0.36;
    cursor: default;
  }

  /* Order Book Lists */
  .book-lbl {
    display: flex;
    align-items: center;
    justify-content: space-between;
    font-size: 0.68rem;
    font-weight: 800;
    letter-spacing: 0.1em;
    text-transform: uppercase;
    color: var(--mut2);
    margin-top: 0.15rem;
  }
  .book-lbl .cnt {
    font-family: ui-monospace, monospace;
    color: var(--mut);
  }
  .book-list {
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
  }
  .book-row {
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 11px;
    padding: 0.72rem 0.88rem;
    display: flex;
    flex-direction: column;
    gap: 0.36rem;
    transition: border-color 0.14s, background 0.14s;
  }
  .book-row:hover {
    background: var(--hover);
    border-color: var(--line2);
  }
  .self-row {
    border-color: rgb(247 153 27 / 24%);
  }
  .peer-row {
    cursor: pointer;
  }
  .row-top {
    display: flex;
    align-items: center;
    gap: 0.45rem;
    flex-wrap: wrap;
  }
  .tag {
    font-size: 0.82rem;
    font-weight: 800;
  }
  .tag.buy {
    color: var(--ok);
  }
  .tag.sell {
    color: var(--amber);
  }
  .sub-tag {
    font-size: 0.72rem;
    color: var(--mut);
    background: var(--bg);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 0.06rem 0.45rem;
  }
  .you-badge {
    font-size: 0.64rem;
    font-weight: 800;
    color: var(--amber);
    background: rgb(247 153 27 / 14%);
    border-radius: 999px;
    padding: 0.08rem 0.42rem;
  }
  .kind-pill {
    font-family: ui-monospace, monospace;
    font-size: 0.65rem;
    font-weight: 700;
    color: var(--mut);
    background: var(--bg);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 0.08rem 0.45rem;
  }
  .kind-pill.live {
    color: var(--ok);
    border-color: rgb(76 194 107 / 32%);
  }
  .price {
    margin-left: auto;
    font-family: ui-monospace, monospace;
    font-size: 0.82rem;
    font-weight: 800;
    font-variant-numeric: tabular-nums;
  }
  .price.buy {
    color: var(--ok);
  }
  .price.sell {
    color: var(--amber);
  }
  .row-txt {
    margin: 0;
    font-size: 0.84rem;
    line-height: 1.42;
    color: var(--ink);
  }
  .row-foot {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.5rem;
    margin-top: 0.1rem;
  }
  .stat {
    font-size: 0.74rem;
    color: var(--mut);
  }
  .live-txt, .ok {
    color: var(--ok);
    font-weight: 600;
  }
  .cross-btn {
    margin-left: auto;
    border-radius: 999px;
    padding: 0.2rem 0.65rem;
    font: inherit;
    font-size: 0.72rem;
    font-weight: 700;
    cursor: pointer;
    transition: background 0.14s, color 0.14s;
  }
  .cross-btn.to-sell {
    background: rgb(247 153 27 / 10%);
    color: var(--amber);
    border: 1px solid rgb(247 153 27 / 36%);
  }
  .cross-btn.to-sell:hover {
    background: var(--amber);
    color: #120e0c;
  }
  .cross-btn.to-buy {
    background: rgb(76 194 107 / 10%);
    color: var(--ok);
    border: 1px solid rgb(76 194 107 / 36%);
  }
  .cross-btn.to-buy:hover {
    background: var(--ok);
    color: #09130b;
  }
  .pav {
    width: 24px;
    height: 24px;
    border-radius: 50%;
    background: rgb(247 153 27 / 14%);
    color: var(--amber);
    display: grid;
    place-items: center;
    font-size: 0.74rem;
    font-weight: 800;
    flex-shrink: 0;
  }
  .pname {
    font-size: 0.84rem;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .peer-tags {
    display: flex;
    gap: 0.35rem;
    flex-wrap: wrap;
  }
  .mwrap {
    position: relative;
  }
  .more {
    background: none;
    border: 0;
    color: var(--mut);
    font-weight: 700;
    cursor: pointer;
    padding: 0.05rem 0.35rem;
  }
  .menu {
    position: absolute;
    right: 0;
    top: 100%;
    z-index: 25;
    min-width: 140px;
    background: #171412;
    border: 1px solid var(--line2);
    border-radius: 10px;
    padding: 0.3rem;
    box-shadow: 0 12px 28px rgb(0 0 0 / 60%);
    display: grid;
    gap: 0.15rem;
  }
  .menu button {
    background: none;
    border: 0;
    color: var(--ink);
    font: inherit;
    font-size: 0.78rem;
    text-align: left;
    padding: 0.42rem 0.6rem;
    border-radius: 7px;
    cursor: pointer;
  }
  .menu button:hover {
    background: var(--hover);
  }
  .menu button.danger {
    color: var(--bad);
  }
  .book-empty {
    padding: 1.15rem 1rem;
    border: 1px dashed var(--line);
    border-radius: 11px;
    font-size: 0.8rem;
    color: var(--mut);
    text-align: center;
    line-height: 1.45;
  }

  /* Live Settlement Tape */
  .tape-section {
    padding: 1rem 1.25rem 2.2rem;
    display: flex;
    flex-direction: column;
    gap: 0.65rem;
  }
  .tape-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    font-size: 0.68rem;
    font-weight: 800;
    letter-spacing: 0.1em;
    color: var(--mut2);
  }
  .tape-cnt {
    font-family: ui-monospace, monospace;
    color: var(--mut);
  }
  .tape-empty {
    padding: 1.25rem;
    border: 1px dashed var(--line);
    border-radius: 11px;
    text-align: center;
    font-size: 0.82rem;
    color: var(--mut);
  }
  .tape-list {
    display: flex;
    flex-direction: column;
    gap: 0.42rem;
  }
  .tape-row {
    display: flex;
    align-items: center;
    gap: 0.7rem;
    padding: 0.62rem 0.9rem;
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 10px;
    cursor: pointer;
    transition: background 0.12s, border-color 0.12s;
    font-size: 0.82rem;
  }
  .tape-row:hover {
    background: var(--hover);
    border-color: var(--line2);
  }
  .side-pill {
    font-family: ui-monospace, monospace;
    font-size: 0.66rem;
    font-weight: 800;
    padding: 0.14rem 0.5rem;
    border-radius: 999px;
    flex-shrink: 0;
  }
  .side-pill.buy {
    background: rgb(76 194 107 / 14%);
    color: var(--ok);
  }
  .side-pill.sell {
    background: rgb(247 153 27 / 14%);
    color: var(--amber);
  }
  .tape-topic {
    font-weight: 700;
    color: var(--ink);
    white-space: nowrap;
  }
  .tape-desc {
    flex: 1;
    min-width: 0;
    color: var(--mut);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .tape-status {
    font-family: ui-monospace, monospace;
    font-size: 0.74rem;
    color: var(--mut);
    white-space: nowrap;
  }
  .tape-status.ok {
    color: var(--ok);
    font-weight: 700;
  }
  .tape-status.bad {
    color: var(--bad);
  }
  .tape-sats {
    font-family: ui-monospace, monospace;
    font-weight: 800;
    color: var(--ink);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  .tape-sats.sell {
    color: var(--amber);
  }
  .time {
    color: var(--mut2);
    font-family: ui-monospace, monospace;
    font-size: 0.72rem;
    white-space: nowrap;
  }

  @media (max-width: 820px) {
    .dual-deck {
      grid-template-columns: 1fr;
    }
    .col-buy {
      border-right: 0;
      border-bottom: 1px solid var(--line);
    }
    .topbar {
      flex-wrap: wrap;
      gap: 0.55rem;
    }
    .search-box {
      order: 3;
      max-width: none;
      width: 100%;
    }
    .tape-desc {
      display: none;
    }
  }
</style>
