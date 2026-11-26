<script lang="ts">
  import { onDestroy, onMount } from 'svelte'
  import type { MarketTapeEvent, PulseSignal, State } from '../lib/types'
  import { fmtSats } from '../lib/format'

  interface Props {
    s: State
    live: boolean
    pulse: PulseSignal | null
    onPickBuy: (domain: string, priceSats: number, hint?: string) => void
    onPickSell: (domain: string, rewardSats: number, hint?: string) => void
  }

  let { s, live, pulse, onPickBuy, onPickSell }: Props = $props()

  interface WingBar {
    id: string
    topic: string
    normTopic: string
    sats: number
    kind: string
    owner?: string
    note: string
    isSelf?: boolean
  }

  interface Comet {
    id: number
    side: 'buy' | 'sell'
    x: number
    y: number
    tail: string
    opacity: number
    color: string
  }

  function cleanTopic(raw: string): string {
    const t = (raw || 'general').trim()
    if (t.startsWith('cluster:')) return t.slice('cluster:'.length)
    return t.replace(/^#/, '') || 'general'
  }

  function parseSats(offers: string): number {
    const m = String(offers || '').match(/(\d+)/)
    return m ? Number(m[1]) : 100
  }

  // Left Wing: Real Demand (Open Bounties + Unmatched Wants + Live Network Wants)
  const buyBars = $derived.by<WingBar[]>(() => {
    const out: WingBar[] = []
    const seen = new Set<string>()

    for (const b of s.net.bounties ?? []) {
      const nt = cleanTopic(b.domain)
      const key = `bounty:${b.id}`
      if (seen.has(key)) continue
      seen.add(key)
      out.push({
        id: key,
        topic: `#${nt}`,
        normTopic: nt.toLowerCase(),
        sats: b.rewardSats || 0,
        kind: 'BOUNTY',
        note: b.question || `Open bounty (${b.filled}/${b.maxFills} filled)`
      })
    }

    for (const uw of s.net.unmatchedWants ?? []) {
      const nt = cleanTopic(uw.domain)
      const key = `want:${uw.wantId}`
      if (seen.has(key)) continue
      seen.add(key)
      out.push({
        id: key,
        topic: `#${nt}`,
        normTopic: nt.toLowerCase(),
        sats: uw.maxSats || 0,
        kind: 'WANT',
        note: uw.query || 'Looking for verified facts in this topic'
      })
    }

    for (const ev of s.marketTape ?? []) {
      if (ev.side !== 'buy') continue
      const nt = cleanTopic(ev.domains?.[0] || ev.shard || 'general')
      const key = `tape-b:${ev.id || nt}`
      if (seen.has(key)) continue
      seen.add(key)
      out.push({
        id: key,
        topic: `#${nt}`,
        normTopic: nt.toLowerCase(),
        sats: ev.sats || 0,
        kind: ev.kind === 'deal' ? 'DEAL' : 'WANT',
        isSelf: ev.isSelf,
        note: ev.summary || (ev.isSelf ? 'Your live buy request' : 'Live network buyer want')
      })
    }

    return out.sort((a, b) => b.sats - a.sats).slice(0, 5)
  })

  // Right Wing: Real Supply (Your Listed Shards + Live Offers + Active Peer Sellers)
  const sellBars = $derived.by<WingBar[]>(() => {
    const out: WingBar[] = []
    const seen = new Set<string>()

    for (const m of s.market ?? []) {
      if (m.active === false) continue
      const nt = cleanTopic(m.domains?.[0] || m.shard)
      const key = `self:${m.shard}`
      seen.add(key)
      out.push({
        id: key,
        topic: `#${nt}`,
        normTopic: nt.toLowerCase(),
        sats: parseSats(m.offers),
        kind: 'SHARD',
        owner: 'YOU',
        isSelf: true,
        note: `${m.records} facts · ${m.works} sold${m.earned > 0 ? ` · +${fmtSats(m.earned)} sats` : ''}`
      })
    }

    for (const ev of s.marketTape ?? []) {
      if (ev.side !== 'sell') continue
      const nt = cleanTopic(ev.domains?.[0] || ev.shard || 'general')
      const key = `tape-s:${ev.id || nt}`
      if (seen.has(key)) continue
      seen.add(key)
      out.push({
        id: key,
        topic: `#${nt}`,
        normTopic: nt.toLowerCase(),
        sats: ev.sats || 0,
        kind: ev.kind === 'deal' ? 'SOLD' : 'OFFER',
        owner: ev.isSelf ? 'YOU' : ev.pub ? `${ev.pub.slice(0, 8)}…` : 'peer',
        isSelf: ev.isSelf,
        note: ev.summary || 'Live verified knowledge offer'
      })
    }

    for (const p of s.net.peers ?? []) {
      for (const dm of p.sellDomains ?? []) {
        const nt = cleanTopic(dm)
        const key = `peer:${p.pub}:${nt}`
        if (seen.has(key) || out.some((x) => x.normTopic === nt.toLowerCase() && x.owner === (p.handle || p.pub.slice(0, 8)))) continue
        seen.add(key)
        out.push({
          id: key,
          topic: `#${nt}`,
          normTopic: nt.toLowerCase(),
          sats: 0,
          kind: p.isHub ? 'HUB' : 'PEER',
          owner: p.handle || `${p.pub.slice(0, 8)}…`,
          note: `${p.isHub ? 'Overlay Hub' : 'Verified Agent'} selling #${nt}`
        })
        if (out.length >= 8) break
      }
      if (out.length >= 8) break
    }

    return out.slice(0, 5)
  })

  const maxBuySats = $derived(Math.max(...buyBars.map((b) => b.sats), 100))
  const maxSellSats = $derived(Math.max(...sellBars.map((b) => b.sats), 100))
  const totalBuySats = $derived(buyBars.reduce((acc, b) => acc + b.sats, 0))
  const totalSellSats = $derived(sellBars.reduce((acc, b) => acc + b.sats, 0))

  // Settled volume & deal counts (100% real data)
  const settledDeals = $derived((s.automation?.deals ?? []).filter((d) => d.to === 'settled'))
  const clearedSats = $derived.by(() => {
    const dealSum = settledDeals.reduce((acc, d) => acc + (d.priceSats ?? 0), 0)
    return Math.max(s.receipts.earned, dealSum)
  })
  const clearedCount = $derived(settledDeals.length + (s.receipts.earned > 0 && settledDeals.length === 0 ? s.receipts.count : 0))

  // Dual-arc ring split around Hex Core (buy vs sell pressure)
  const buyShare = $derived.by(() => {
    const b = totalBuySats || buyBars.length * 100
    const sl = totalSellSats || sellBars.length * 100
    if (b + sl === 0) return 0.5
    return Math.min(0.85, Math.max(0.15, b / (b + sl)))
  })

  const ringCirc = 2 * Math.PI * 76
  const buyArcLen = $derived(ringCirc * buyShare)
  const sellArcLen = $derived(ringCirc * (1 - buyShare))

  // Layout geometry helpers (viewBox 920 x 344)
  const coreX = 460
  const coreY = 152
  const rowY = (i: number) => 72 + i * 38
  const calcBuyW = (b: WingBar) => 122 + Math.round((Math.max(b.sats, maxBuySats * 0.25) / maxBuySats) * 144)
  const calcSellW = (b: WingBar) =>
    b.sats > 0 ? 122 + Math.round((Math.max(b.sats, maxSellSats * 0.25) / maxSellSats) * 144) : 148

  // Semantic topic bridges connecting matching left & right bars through the center core
  const bridges = $derived.by(() => {
    const list: Array<{ topic: string; d: string; x1: number; y1: number; x2: number; y2: number }> = []
    buyBars.forEach((b, bi) => {
      const si = sellBars.findIndex((sl) => sl.normTopic === b.normTopic)
      if (si === -1) return
      const x1 = 20 + calcBuyW(b)
      const y1 = rowY(bi) + 14
      const x2 = 900 - calcSellW(sellBars[si])
      const y2 = rowY(si) + 14
      list.push({
        topic: b.normTopic,
        x1,
        y1,
        x2,
        y2,
        d: `M ${x1} ${y1} C ${x1 + 92} ${y1}, 392 ${coreY}, ${coreX} ${coreY} C 528 ${coreY}, ${x2 - 92} ${y2}, ${x2} ${y2}`
      })
    })
    return list
  })

  // Dual-polarity bottom heartbeat tape (last 44 real events + live pulses)
  interface HeartbeatSlot {
    buy: number
    sell: number
    settled: boolean
  }
  let liveBeats = $state<HeartbeatSlot[]>([])

  const heartbeatSlots = $derived.by<HeartbeatSlot[]>(() => {
    const base: HeartbeatSlot[] = []
    const tape = (s.marketTape ?? []).slice(0, 44).reverse()
    for (const ev of tape) {
      const mag = Math.min(22, Math.max(5, Math.round(Math.log10(Math.max(10, ev.sats || 25)) * 7)))
      const isSettled = ev.kind === 'deal' && ev.status === 'settled'
      base.push({
        buy: ev.side === 'buy' || isSettled ? mag : 0,
        sell: ev.side === 'sell' || isSettled ? mag : 0,
        settled: isSettled
      })
    }
    const merged = [...base, ...liveBeats].slice(-44)
    while (merged.length < 44) {
      merged.unshift({ buy: 0, sell: 0, settled: false })
    }
    return merged
  })

  // Interactive hover HUD
  let activeTopic = $state<string | null>(null)
  let hud = $state<{ x: number; y: number; title: string; detail: string } | null>(null)

  // Live kinetic comets, SPV shockwave & settlement flash
  let comets = $state<Comet[]>([])
  let shockwaveR = $state(64)
  let shockwaveOpacity = $state(0)
  let flashText = $state('')
  let flashVisible = $state(false)
  let flashTimer: ReturnType<typeof setTimeout> | null = null

  let nextCometId = 1
  let rafId: number | null = null
  interface ActiveComet {
    id: number
    side: 'buy' | 'sell'
    x0: number
    y0: number
    cx1: number
    cx2: number
    start: number
    dur: number
    color: string
    pts: Array<[number, number]>
    onArrive?: () => void
  }
  let activeComets: ActiveComet[] = []
  let activeShockStart: number | null = null
  let reducedMotion = false

  function stepAnim(now: number) {
    let keepRunning = false

    if (activeComets.length > 0) {
      const nextRendered: Comet[] = []
      const surviving: ActiveComet[] = []
      for (const c of activeComets) {
        const t = Math.min(1, (now - c.start) / c.dur)
        const ease = 1 - Math.pow(1 - t, 2.2)
        const u = 1 - ease
        const x = u * u * u * c.x0 + 3 * u * u * ease * c.cx1 + 3 * u * ease * ease * c.cx2 + ease * ease * ease * coreX
        const y = u * u * u * c.y0 + 3 * u * u * ease * c.y0 + 3 * u * ease * ease * coreY + ease * ease * ease * coreY
        c.pts.push([x, y])
        if (c.pts.length > 10) c.pts.shift()
        if (t < 1) {
          surviving.push(c)
          nextRendered.push({
            id: c.id,
            side: c.side,
            x,
            y,
            tail: c.pts.length > 1 ? c.pts.map((p, k) => `${k ? 'L' : 'M'}${p[0].toFixed(1)},${p[1].toFixed(1)}`).join(' ') : '',
            opacity: 1 - t * 0.3,
            color: c.color
          })
          keepRunning = true
        } else {
          c.onArrive?.()
        }
      }
      activeComets = surviving
      comets = nextRendered
    } else if (comets.length > 0) {
      comets = []
    }

    if (activeShockStart !== null) {
      const t = Math.min(1, (now - activeShockStart) / 720)
      shockwaveR = 64 + t * 54
      shockwaveOpacity = 0.92 * (1 - t)
      if (t < 1) {
        keepRunning = true
      } else {
        activeShockStart = null
        shockwaveOpacity = 0
      }
    }

    if (keepRunning) {
      rafId = requestAnimationFrame(stepAnim)
    } else {
      rafId = null
    }
  }

  function ensureAnimLoop() {
    if (reducedMotion || rafId !== null) return
    rafId = requestAnimationFrame(stepAnim)
  }

  function launchComet(side: 'buy' | 'sell', rowIdx?: number, onArrive?: () => void) {
    if (reducedMotion) {
      onArrive?.()
      return
    }
    const isBuy = side === 'buy'
    const bars = isBuy ? buyBars : sellBars
    const idx = rowIdx !== undefined ? Math.min(Math.max(0, rowIdx), Math.max(0, bars.length - 1)) : Math.floor(Math.random() * Math.max(1, bars.length))
    const bar = bars[idx]
    const x0 = isBuy ? (bar ? 20 + calcBuyW(bar) : 140) : (bar ? 900 - calcSellW(bar) : 780)
    const y0 = rowY(bar ? idx : 1) + 14
    activeComets.push({
      id: nextCometId++,
      side,
      x0,
      y0,
      cx1: isBuy ? x0 + 92 : x0 - 92,
      cx2: isBuy ? 392 : 528,
      start: performance.now(),
      dur: 620,
      color: isBuy ? '#4cc26b' : '#f7991b',
      pts: [],
      onArrive
    })
    ensureAnimLoop()
  }

  function triggerShockwave(label: string) {
    if (!reducedMotion) {
      activeShockStart = performance.now()
      ensureAnimLoop()
    }
    flashText = label
    flashVisible = true
    if (flashTimer) clearTimeout(flashTimer)
    flashTimer = setTimeout(() => (flashVisible = false), 2600)
  }

  function pushLiveBeat(side: 'buy' | 'sell' | 'clear', sats: number) {
    const mag = Math.min(22, Math.max(6, Math.round(Math.log10(Math.max(10, sats || 50)) * 7)))
    liveBeats = [
      ...liveBeats.slice(-24),
      {
        buy: side === 'buy' || side === 'clear' ? mag : 0,
        sell: side === 'sell' || side === 'clear' ? mag : 0,
        settled: side === 'clear'
      }
    ]
  }

  // React to live SSE market pulses in real time!
  let lastPulseSeq = 0
  $effect(() => {
    if (!pulse || pulse.seq === lastPulseSeq) return
    lastPulseSeq = pulse.seq
    const ev = pulse.event
    const tape: MarketTapeEvent | undefined = pulse.tape
    const topic = tape?.domains?.[0] ? `#${cleanTopic(tape.domains[0])}` : tape?.shard ? `#${cleanTopic(tape.shard)}` : ''

    if (ev === 'deal_settled' || ev === 'receipt' || (tape?.kind === 'deal' && tape.status === 'settled')) {
      let hit = 0
      const onHit = () => {
        hit++
        if (hit === 2) {
          const amt = tape?.sats ? `+${fmtSats(tape.sats)} sats` : 'Settled'
          triggerShockwave(`⚡ ${amt}${topic ? ` · ${topic}` : ''} · SPV ✓`)
          pushLiveBeat('clear', tape?.sats || 100)
        }
      }
      launchComet('buy', undefined, onHit)
      launchComet('sell', undefined, onHit)
    } else if (ev === 'market_want' || ev === 'tool:want' || tape?.side === 'buy') {
      launchComet('buy', 0)
      pushLiveBeat('buy', tape?.sats || 100)
    } else if (ev === 'market_offer' || ev === 'tool:sell' || ev === 'listing_updated' || ev === 'remember' || ev === 'anchor' || tape?.side === 'sell') {
      launchComet('sell', 0)
      pushLiveBeat('sell', tape?.sats || 100)
    }
  })

  onMount(() => {
    reducedMotion = typeof window !== 'undefined' && window.matchMedia?.('(prefers-reduced-motion: reduce)').matches
  })

  onDestroy(() => {
    if (rafId !== null) cancelAnimationFrame(rafId)
    if (flashTimer) clearTimeout(flashTimer)
  })

  const liq = $derived(s.net.liquidity)
  const peerCount = $derived(liq?.activePeers ?? s.net.peers?.length ?? 0)
  const fillPct = $derived(liq ? `${Math.round(liq.fillRate * 100)}%` : '—')
  const medianMs = $derived(liq && liq.medianTimeToOfferMs > 0 ? `${Math.round(liq.medianTimeToOfferMs)}ms` : '—')
</script>

<section class="pulse-stage" aria-label="Live Knowledge Market Pulse">
  <svg viewBox="0 0 920 344" class="svg" xmlns="http://www.w3.org/2000/svg">
    <defs>
      <radialGradient id="np-glow-buy" cx="14%" cy="44%" r="42%">
        <stop offset="0%" stop-color="#4cc26b" stop-opacity="0.14" />
        <stop offset="100%" stop-color="#4cc26b" stop-opacity="0" />
      </radialGradient>
      <radialGradient id="np-glow-sell" cx="86%" cy="44%" r="42%">
        <stop offset="0%" stop-color="#f7991b" stop-opacity="0.15" />
        <stop offset="100%" stop-color="#f7991b" stop-opacity="0" />
      </radialGradient>
      <radialGradient id="np-glow-core" cx="50%" cy="44%" r="22%">
        <stop offset="0%" stop-color="#f7991b" stop-opacity="0.2" />
        <stop offset="55%" stop-color="#4cc26b" stop-opacity="0.06" />
        <stop offset="100%" stop-color="#090807" stop-opacity="0" />
      </radialGradient>
      <linearGradient id="np-bar-buy" x1="0%" y1="0%" x2="100%" y2="0%">
        <stop offset="0%" stop-color="#4cc26b" stop-opacity="0.08" />
        <stop offset="100%" stop-color="#4cc26b" stop-opacity="0.44" />
      </linearGradient>
      <linearGradient id="np-bar-sell" x1="100%" y1="0%" x2="0%" y2="0%">
        <stop offset="0%" stop-color="#f7991b" stop-opacity="0.08" />
        <stop offset="100%" stop-color="#f7991b" stop-opacity="0.46" />
      </linearGradient>
      <linearGradient id="np-bridge" x1="0%" y1="0%" x2="100%" y2="0%">
        <stop offset="0%" stop-color="#4cc26b" />
        <stop offset="50%" stop-color="#ffd479" />
        <stop offset="100%" stop-color="#f7991b" />
      </linearGradient>
      <pattern id="np-grid" width="28" height="28" patternUnits="userSpaceOnUse">
        <path d="M 28 0 L 0 0 0 28" fill="none" stroke="#1a1714" stroke-width="0.7" />
      </pattern>
      <filter id="np-neon" x="-40%" y="-40%" width="180%" height="180%">
        <feGaussianBlur stdDeviation="3" result="blur" />
        <feMerge>
          <feMergeNode in="blur" />
          <feMergeNode in="SourceGraphic" />
        </feMerge>
      </filter>
    </defs>

    <!-- Obsidian Stage -->
    <rect width="920" height="344" fill="#090807" />
    <rect width="920" height="270" fill="url(#np-grid)" opacity="0.65" />
    <rect width="920" height="270" fill="url(#np-glow-buy)" />
    <rect width="920" height="270" fill="url(#np-glow-sell)" />
    <rect width="920" height="270" fill="url(#np-glow-core)" />

    <!-- Telemetry Strip -->
    <line x1="0" y1="32" x2="920" y2="32" stroke="#221f1c" stroke-width="1" />
    <circle cx="20" cy="16" r="3.8" fill={live ? '#4cc26b' : '#e5534b'} class:live-dot={live} />
    <text x="32" y="20" fill="#f1ede9" font-size="11" font-weight="800" letter-spacing="0.08em" font-family="system-ui, sans-serif">
      NEKTAR CLEARING PULSE
    </text>
    <rect x="206" y="7" width="66" height="17" rx="8.5" fill="#171412" stroke="#2e2925" />
    <text x="239" y="19" text-anchor="middle" fill="#f7991b" font-size="9.5" font-weight="700" font-family="ui-monospace, monospace">
      {s.network.toUpperCase()}
    </text>

    <g font-family="ui-monospace, monospace" font-size="10.5" fill="#9a9087">
      <text x="568" y="20">PEERS <tspan fill="#f1ede9" font-weight="700">{peerCount}</tspan></text>
      <text x="652" y="20">FILL <tspan fill="#4cc26b" font-weight="700">{fillPct}</tspan></text>
      <text x="734" y="20">QUOTE <tspan fill="#f1ede9" font-weight="700">{medianMs}</tspan></text>
      <text x="900" y="20" text-anchor="end" fill="#f7991b" font-weight="700">BSV SPV</text>
    </g>

    <!-- Wing Headers -->
    <text x="20" y="54" fill="#4cc26b" font-size="10.5" font-weight="800" letter-spacing="0.09em" font-family="system-ui, sans-serif">
      ▲ BUY DEMAND
    </text>
    <text x="286" y="54" text-anchor="end" fill="#9a9087" font-size="10.5" font-family="ui-monospace, monospace">
      {totalBuySats > 0 ? `${fmtSats(totalBuySats)} sats open` : `${buyBars.length} open`}
    </text>

    <text x="900" y="54" text-anchor="end" fill="#f7991b" font-size="10.5" font-weight="800" letter-spacing="0.09em" font-family="system-ui, sans-serif">
      SELL SUPPLY ▼
    </text>
    <text x="634" y="54" fill="#9a9087" font-size="10.5" font-family="ui-monospace, monospace">
      {s.market.length} listed · {peerCount} peers
    </text>

    <!-- Matching Topic Bridges -->
    {#each bridges as br}
      <path
        d={br.d}
        fill="none"
        stroke="url(#np-bridge)"
        stroke-width={activeTopic === br.topic ? 2.4 : 1.15}
        stroke-opacity={activeTopic ? (activeTopic === br.topic ? 0.95 : 0.08) : 0.28}
        class="bridge"
      />
    {/each}

    <!-- Left Wing Bars (Demand / Buys) -->
    {#if buyBars.length > 0}
      {#each buyBars as b, i}
        {@const y = rowY(i)}
        {@const w = calcBuyW(b)}
        {@const dim = activeTopic !== null && activeTopic !== b.normTopic}
        <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
        <g
          class="bar"
          opacity={dim ? 0.3 : 1}
          onmouseenter={() => {
            activeTopic = b.normTopic
            hud = { x: 24, y: Math.max(38, y - 34), title: `${b.kind} · ${b.topic} (${b.sats ? `${fmtSats(b.sats)} sats` : 'open'})`, detail: `${b.note.slice(0, 44)} · click to fill` }
          }}
          onmouseleave={() => {
            activeTopic = null
            hud = null
          }}
          onclick={() => {
            launchComet('buy', i)
            onPickSell(b.normTopic, b.sats || 100, b.note)
          }}
        >
          <rect
            x="20"
            {y}
            width={w}
            height="28"
            rx="6"
            fill="url(#np-bar-buy)"
            stroke={activeTopic === b.normTopic ? '#4cc26b' : '#213627'}
            stroke-width={activeTopic === b.normTopic ? 1.3 : 1}
          />
          <circle cx={20 + w} cy={y + 14} r="3.4" fill="#4cc26b" />
          <text x="30" y={y + 18} fill="#f1ede9" font-size="11.5" font-weight="700" font-family="system-ui, sans-serif">
            {b.topic.slice(0, 16)}
          </text>
          <text x={20 + w - 10} y={y + 18} text-anchor="end" fill="#4cc26b" font-size="11" font-weight="800" font-family="ui-monospace, monospace">
            {b.sats > 0 ? `${fmtSats(b.sats)}s` : b.kind}
          </text>
        </g>
      {/each}
    {:else}
      <g opacity="0.72">
        <rect x="20" y="96" width="236" height="44" rx="8" fill="#0f1210" stroke="#203025" stroke-dasharray="4 4" />
        <text x="34" y="115" fill="#9a9087" font-size="11" font-weight="600" font-family="system-ui, sans-serif">Listening for live buyer wants…</text>
        <text x="34" y="130" fill="#4cc26b" font-size="10" font-family="ui-monospace, monospace">Post a Buy below to launch live demand</text>
      </g>
    {/if}

    <!-- Right Wing Bars (Supply / Sells) -->
    {#if sellBars.length > 0}
      {#each sellBars as b, i}
        {@const y = rowY(i)}
        {@const w = calcSellW(b)}
        {@const x = 900 - w}
        {@const dim = activeTopic !== null && activeTopic !== b.normTopic}
        <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
        <g
          class="bar"
          opacity={dim ? 0.3 : 1}
          onmouseenter={() => {
            activeTopic = b.normTopic
            hud = { x: 646, y: Math.max(38, y - 34), title: `${b.kind} · ${b.topic}${b.sats ? ` (${fmtSats(b.sats)} sats)` : ''}`, detail: `${b.note.slice(0, 44)} · click to buy` }
          }}
          onmouseleave={() => {
            activeTopic = null
            hud = null
          }}
          onclick={() => {
            launchComet('sell', i)
            onPickBuy(b.normTopic, b.sats || 100, b.note)
          }}
        >
          <rect
            {x}
            {y}
            width={w}
            height="28"
            rx="6"
            fill="url(#np-bar-sell)"
            stroke={activeTopic === b.normTopic ? '#f7991b' : '#3a2b18'}
            stroke-width={activeTopic === b.normTopic ? 1.3 : 1}
          />
          <circle cx={x} cy={y + 14} r="3.4" fill="#f7991b" />
          <text x={x + 10} y={y + 18} fill="#f7991b" font-size="11" font-weight="800" font-family="ui-monospace, monospace">
            {b.sats > 0 ? `${fmtSats(b.sats)}s` : b.kind}
          </text>
          <text x="890" y={y + 18} text-anchor="end" fill="#f1ede9" font-size="11.5" font-weight="700" font-family="system-ui, sans-serif">
            {b.isSelf ? `${b.topic.slice(0, 14)} ★` : b.topic.slice(0, 16)}
          </text>
        </g>
      {/each}
    {:else}
      <g opacity="0.72">
        <rect x="664" y="96" width="236" height="44" rx="8" fill="#13100d" stroke="#36291a" stroke-dasharray="4 4" />
        <text x="886" y="115" text-anchor="end" fill="#9a9087" font-size="11" font-weight="600" font-family="system-ui, sans-serif">No active listings yet…</text>
        <text x="886" y="130" text-anchor="end" fill="#f7991b" font-size="10" font-family="ui-monospace, monospace">Anchor &amp; Sell below to supply the hive</text>
      </g>
    {/if}

    <!-- Kinetic Comets -->
    {#each comets as c (c.id)}
      {#if c.tail}
        <path d={c.tail} fill="none" stroke={c.color} stroke-width="2.6" stroke-linecap="round" opacity={c.opacity * 0.75} />
      {/if}
      <circle cx={c.x} cy={c.y} r="4.8" fill={c.color} opacity={c.opacity} filter="url(#np-neon)" />
    {/each}

    <!-- Center Hexagonal Clearing Core -->
    <g transform={`translate(${coreX}, ${coreY})`}>
      <circle r="76" fill="none" stroke="#1e1b18" stroke-width="3.5" />
      <!-- Dual-Arc Buy/Sell Liquidity Ring -->
      <circle
        r="76"
        fill="none"
        stroke="#4cc26b"
        stroke-width="3.5"
        stroke-linecap="round"
        stroke-dasharray={`${buyArcLen.toFixed(1)} ${ringCirc.toFixed(1)}`}
        transform="rotate(90)"
        opacity="0.86"
      />
      <circle
        r="76"
        fill="none"
        stroke="#f7991b"
        stroke-width="3.5"
        stroke-linecap="round"
        stroke-dasharray={`${sellArcLen.toFixed(1)} ${ringCirc.toFixed(1)}`}
        stroke-dashoffset={`-${buyArcLen.toFixed(1)}`}
        transform="rotate(90)"
        opacity="0.9"
      />

      <!-- SPV Shockwave Ring -->
      {#if shockwaveOpacity > 0.01}
        <circle r={shockwaveR} fill="none" stroke="#ffd479" stroke-width="2.5" opacity={shockwaveOpacity} />
      {/if}

      <polygon points="0,-64 55.4,-32 55.4,32 0,64 -55.4,32 -55.4,-32" fill="#11100e" stroke="#36302a" stroke-width="1.5" />
      <polygon points="0,-56 48.5,-28 48.5,28 0,56 -48.5,28 -48.5,-28" fill="#151311" stroke="#f7991b" stroke-opacity="0.3" stroke-width="1" />

      <!-- Nektar Drop Emblem -->
      <path
        fill="#f7991b"
        transform="translate(-8,-44) scale(0.042)"
        d="M 195 55 C 178 55 164 63 157 80 C 153 100 149 112 143 124 C 136 139 115 158 93 186 C 80 206 73 226 72 250 C 71 277 79 306 98 329 C 117 352 150 372 190 377 C 232 380 255 356 259 322 C 261 296 274 273 296 251 C 310 234 318 210 318 186 C 318 158 312 137 302 120 C 292 102 276 84 260 74 C 245 64 218 55 195 55 Z"
      />

      <text x="0" y="6" text-anchor="middle" fill="#f1ede9" font-size="21" font-weight="800" font-family="ui-monospace, monospace">
        {fmtSats(clearedSats)}
      </text>
      <text x="0" y="22" text-anchor="middle" fill="#9a9087" font-size="9.5" font-weight="700" letter-spacing="0.08em" font-family="system-ui, sans-serif">
        SATS CLEARED
      </text>
      <text x="0" y="38" text-anchor="middle" fill={clearedCount > 0 ? '#4cc26b' : '#9a9087'} font-size="9.5" font-weight="700" font-family="ui-monospace, monospace">
        {clearedCount > 0 ? `${clearedCount} SPV DEALS ✓` : live ? 'READY · LIVE SSE' : 'RECONNECTING'}
      </text>
    </g>

    <!-- Ephemeral Settlement Flash Pill -->
    {#if flashVisible && flashText}
      <g transform="translate(460, 56)">
        <rect x="-132" y="-13" width="264" height="24" rx="12" fill="#1b1610" stroke="#f7991b" stroke-width="1.2" />
        <text x="0" y="3" text-anchor="middle" fill="#ffd479" font-size="10.5" font-weight="700" font-family="ui-monospace, monospace">
          {flashText}
        </text>
      </g>
    {/if}

    <!-- Hover HUD Tooltip -->
    {#if hud}
      <g transform={`translate(${hud.x}, ${hud.y})`} pointer-events="none">
        <rect x="0" y="0" width="250" height="36" rx="8" fill="#171412" stroke="#3c352e" />
        <text x="10" y="15" fill="#f1ede9" font-size="10.5" font-weight="700" font-family="system-ui, sans-serif">{hud.title}</text>
        <text x="10" y="29" fill="#9a9087" font-size="9.5" font-family="ui-monospace, monospace">{hud.detail}</text>
      </g>
    {/if}

    <!-- Bottom Dual-Polarity Live Heartbeat Strip -->
    <line x1="0" y1="270" x2="920" y2="270" stroke="#221f1c" stroke-width="1" />
    <rect x="0" y="271" width="920" height="73" fill="#0d0c0a" />
    <line x1="20" y1="308" x2="900" y2="308" stroke="#26221e" stroke-width="1" stroke-dasharray="3 3" />

    {#each heartbeatSlots as slot, i}
      {@const x = 24 + i * 20}
      {@const fade = 0.32 + (i / heartbeatSlots.length) * 0.62}
      {#if slot.buy > 0}
        <rect {x} y={307 - slot.buy} width="10" height={slot.buy} rx="2" fill="#4cc26b" opacity={fade} />
      {/if}
      {#if slot.sell > 0}
        <rect {x} y="309" width="10" height={slot.sell} rx="2" fill="#f7991b" opacity={fade} />
      {/if}
      {#if slot.settled}
        <polygon points={`${x + 5},302 ${x + 10},308 ${x + 5},314 ${x},308`} fill="#ffd479" filter="url(#np-neon)" />
      {/if}
    {/each}

    <text x="20" y="286" fill="#4cc26b" font-size="9.5" font-weight="700" font-family="ui-monospace, monospace">▲ BUY FLOW</text>
    <text x="460" y="286" text-anchor="middle" fill="#6d655e" font-size="9.5" font-weight="700" letter-spacing="0.1em" font-family="system-ui, sans-serif">
      LIVE ORDER FLOW &amp; ATOMIC BSV CLEARING HEARTBEAT
    </text>
    <text x="900" y="336" text-anchor="end" fill="#f7991b" font-size="9.5" font-weight="700" font-family="ui-monospace, monospace">SELL FLOW ▼</text>
    <text x="20" y="336" fill="#ffd479" font-size="9.5" font-weight="700" font-family="ui-monospace, monospace">◆ SPV SETTLED</text>
  </svg>
</section>

<style>
  .pulse-stage {
    border-bottom: 1px solid var(--line);
    background: #090807;
  }
  .svg {
    display: block;
    width: 100%;
    height: auto;
    user-select: none;
  }
  .bar {
    cursor: pointer;
    transition: opacity 0.16s ease;
  }
  .bridge {
    transition: stroke-opacity 0.18s ease, stroke-width 0.18s ease;
  }
  @keyframes np-pulse {
    0%, 100% { opacity: 1; }
    50% { opacity: 0.35; }
  }
  .live-dot {
    animation: np-pulse 1.9s ease-in-out infinite;
  }
  @media (prefers-reduced-motion: reduce) {
    .live-dot { animation: none; }
  }
</style>
